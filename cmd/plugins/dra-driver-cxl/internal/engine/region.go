// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// errForeignRegion is the kind of the error of a grant on a memdev with a
// region that no record of the driver names: the driver never uses or
// touches it.
var errForeignRegion = errors.New("a region that the driver did not create")

// foreignRegionError is the error of a grant on the memdev with a region
// that no record names.
type foreignRegionError struct {
	memdev, region string
}

// Error returns the refusal in words.
func (e *foreignRegionError) Error() string {
	return fmt.Sprintf("memdev %s has region %s that the driver did not create", e.memdev, e.region)
}

// Is tells whether target is ErrForeignRegion.
func (e *foreignRegionError) Is(target error) bool {
	return target == errForeignRegion
}

// ownRegion returns the region of ours of the memdev of the grant that a
// grant of mode uses (pmem for persistent grants, ram for the others),
// nil if there is none.
func ownRegion(r *state.Records, serial state.Serial, mode state.RegionMode) *state.Region {
	m := r.Memdev(serial)
	if m == nil {
		return nil
	}
	for _, ref := range m.Regions {
		if reg := r.Region(ref); reg != nil && reg.Mode == mode {
			return reg
		}
	}
	return nil
}

// ensureRegion returns the region of ours of the memdev of a grant for the
// region mode, made ready for set-up: an OWNED one as it is, a KEPT one
// enabled again, else a new one (noted before cxl create-region, one retry
// after Timeouts.RegionRetry when the memdev had no endpoint port yet). A
// region of the memdev that no record names refuses the memdev, recorded
// FOREIGN. The grant's intent and the memdev record name the region.
func (e *Engine) ensureRegion(ctx context.Context, req *GrantRequest, memdev string, mode state.RegionMode, size uint64) (string, error) {
	if err := e.locked(func() error { return e.k.EnableMemdev(ctx, memdev) }); err != nil {
		return "", err
	}
	if reg := ownRegion(e.records(), req.Serial, mode); reg != nil {
		gone, err := e.forgetGoneRegion(ctx, req, memdev, reg)
		if err != nil {
			return "", err
		}
		if !gone {
			return e.useRegion(ctx, req, memdev, reg)
		}
	}
	names := e.k.Regions(memdev)
	if err := e.syncForeignRegions(ctx, names, e.kernelRegion, state.TriggerPrepare); err != nil {
		return "", err
	}
	if len(names) > 0 {
		return "", &foreignRegionError{memdev: memdev, region: names[0]}
	}
	name, err := e.createRegion(ctx, req, memdev, mode, size)
	if errors.Is(err, kernel.ErrEndpointNotReady) {
		klog.Warningf("engine: claim %s: device %s: %v; retrying once in %v", e.claimNameOf(req.Ref), e.deviceNameOf(req.Ref), err,
			e.opts.Timeouts.RegionRetry)
		select {
		case <-ctx.Done():
		case <-time.After(e.opts.Timeouts.RegionRetry):
			name, err = e.createRegion(ctx, req, memdev, mode, size)
		}
	}
	return name, err
}

// forgetGoneRegion forgets the recorded region of ours reg of a local
// shared or persistent memdev that is gone (another tool released it while
// the driver ran), with what its grants got of it, and tells whether it
// did: a new region is made then.
func (e *Engine) forgetGoneRegion(ctx context.Context, req *GrantRequest, memdev string, reg *state.Region) (bool, error) {
	if req.Asked.Kind != state.KindLocalShared && req.Asked.Kind != state.KindLocalPersistent {
		return false, nil
	}
	name := string(reg.Ref())
	if ri, err := e.k.Region(name); err == nil && slices.Contains(ri.Memdevs, memdev) {
		return false, nil
	}
	what := "shared"
	if req.Asked.Kind.Persistent() {
		what = "persistent"
	}
	claims := []string{}
	if m := e.records().Memdev(req.Serial); m != nil {
		claims = append(claims, otherClaims(m, req.Ref.Claim)...)
	}
	klog.Warningf("engine: local memdev %s (%s): %s of the %s device is gone, forgetting it and its claims %v", req.Serial.String(), memdev, name, what, claims)
	return true, e.commit(ctx, func(r *state.Records) error { return dropRegion(r, reg.Ref(), state.TriggerPrepare) })
}

// useRegion returns the name of the recorded region of ours reg for a
// grant: KEPT is enabled again (OWNED, not set up), OWNED as it is, an
// OWNED one that the kernel has disabled (a release failed after cxl
// disable-region) as KEPT. The region must exist and target the memdev.
func (e *Engine) useRegion(ctx context.Context, req *GrantRequest, memdev string, reg *state.Region) (string, error) {
	name := string(reg.Ref())
	ri, err := e.k.Region(name)
	if err != nil || !slices.Contains(ri.Memdevs, memdev) {
		return "", fmt.Errorf("memdev %s: its %s is gone (%v)", memdev, name, err)
	}
	// a region has one target memdev: its kernel state is the one to look
	// at (an interleaved region would need every target's)
	if reg.State == state.RegionOwned && ri.State == kernel.RegionDisabled {
		klog.Infof("engine: %s memdev %s (%s): %s is disabled: keeping it to enable and set it up again", req.Asked.Source, req.Serial.String(),
			memdev, name)
		if err := e.commit(ctx, func(r *state.Records) error {
			return keepRegion(r.Region(reg.Ref()), state.TriggerPrepare)
		}); err != nil {
			return "", err
		}
		reg = e.records().Region(reg.Ref())
	}
	switch reg.State {
	case state.RegionOwned:
		if req.Asked.Source == state.SourcePool {
			e.k.Logf("%s: reusing %s", memdev, name)
		}
	case state.RegionKept:
		if err := e.locked(func() error { return e.k.EnableRegion(ctx, name) }); err != nil {
			return "", err
		}
		if req.Asked.Source == state.SourcePool {
			e.k.Logf("%s: reusing %s", memdev, name)
		}
		klog.Infof("engine: %s memdev %s (%s): enabled kept %s", req.Asked.Source, req.Serial.String(), memdev, name)
		if err := e.commit(ctx, func(r *state.Records) error {
			rec := r.Region(state.RegionRef(name))
			if err := fsm.Apply(&state.RegionTable, &rec.State, state.RegionOwned, state.TriggerPrepare); err != nil {
				return err
			}
			return setIntentRegion(r, req.Ref, rec.Ref())
		}); err != nil {
			return "", err
		}
		return name, nil
	default:
		return "", fmt.Errorf("memdev %s: %s is %s", memdev, name, reg.State)
	}
	return name, e.commit(ctx, func(r *state.Records) error { return setIntentRegion(r, req.Ref, state.RegionRef(name)) })
}

// setIntentRegion sets the region of the intent of a grant.
func setIntentRegion(r *state.Records, ref state.GrantRef, region state.RegionRef) error {
	g := r.Grant(ref)
	if g == nil {
		return fmt.Errorf("grant %s has no record", ref)
	}
	if g.Intent.Region != "" && g.Intent.Region != region {
		g.Intent.Noted = addRef(g.Intent.Noted, g.Intent.Region)
	}
	g.Intent.Region = region
	return nil
}

// createRegion creates a region of the mode on the memdev of a grant under
// the engine lock: the name that it will get is committed (NONE -> NOTED)
// before cxl create-region runs, and the created region (OWNED) after it.
func (e *Engine) createRegion(ctx context.Context, req *GrantRequest, memdev string, mode state.RegionMode, size uint64) (string, error) {
	typ := kernel.RegionRAM
	if mode == state.RegionPmem {
		typ = kernel.RegionPmem
	}
	decoder, err := e.k.RootDecoder(memdev, typ)
	if err != nil {
		return "", err
	}
	noted, created := "", ""
	err = e.locked(func() error {
		var err error
		created, err = e.k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: memdev, Decoder: decoder, Type: typ, Size: size,
			Note: func(next string) error {
				if next == "" {
					return nil
				}
				if err := e.commit(ctx, func(r *state.Records) error { return noteRegion(r, req, next, mode) }); err != nil {
					return fmt.Errorf("recording region %s: %w", next, err)
				}
				noted = next
				return nil
			}})
		return err
	})
	if err != nil {
		if noted != "" {
			e.clearNote(ctx, req, noted)
		}
		return "", err
	}
	ri, _ := e.k.Region(created)
	if err := e.commit(ctx, func(r *state.Records) error { return createdRegion(r, req, noted, created, mode, ri) }); err != nil {
		return "", err
	}
	return created, nil
}

// noteRegion records the region that a creation will make (NONE ->
// NOTED), named by the memdev and the grant's intent.
func noteRegion(r *state.Records, req *GrantRequest, name string, mode state.RegionMode) error {
	ref := state.RegionRef(name)
	if err := forgetForeign(r, ref, state.TriggerPrepare); err != nil {
		return err
	}
	if r.Region(ref) != nil {
		return fmt.Errorf("region %s has a record already", name)
	}
	reg := state.Region{NotedName: name, Mode: mode, Targets: []state.Serial{req.Serial}, Owner: state.OwnerOurs,
		State: state.RegionNone}
	if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionNoted, state.TriggerPrepare); err != nil {
		return err
	}
	r.Regions = append(r.Regions, reg)
	if m := r.Memdev(req.Serial); m != nil {
		m.Regions = addRef(m.Regions, ref)
	}
	return setIntentRegion(r, req.Ref, ref)
}

// createdRegion records the region that a creation made (NOTED ->
// OWNED) under its name, which replaces the noted one.
func createdRegion(r *state.Records, req *GrantRequest, noted, created string, mode state.RegionMode, ri kernel.RegionInfo) error {
	if noted == "" {
		// the next name could not be read: recorded after the creation
		if err := forgetForeign(r, state.RegionRef(created), state.TriggerPrepare); err != nil {
			return err
		}
		reg := state.Region{NotedName: created, Mode: mode, Targets: []state.Serial{req.Serial}, Owner: state.OwnerOurs,
			State: state.RegionNone}
		if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionNoted, state.TriggerPrepare); err != nil {
			return err
		}
		r.Regions = append(r.Regions, reg)
		noted = created
	}
	reg := r.Region(state.RegionRef(noted))
	if reg == nil {
		return fmt.Errorf("region %s has no record", noted)
	}
	if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionOwned, state.TriggerPrepare); err != nil {
		return err
	}
	reg.Name, reg.NotedName = created, ""
	reg.NUMANode = state.NUMANodePtr(ri.NUMANode)
	reg.Size = ri.Range.Size()
	if m := r.Memdev(req.Serial); m != nil {
		m.Regions = addRef(removeRef(m.Regions, state.RegionRef(noted)), state.RegionRef(created))
	}
	return setIntentRegion(r, req.Ref, state.RegionRef(created))
}

// clearNote clears the note of a region that a failed creation did not
// make (NOTED -> NONE); a region of the noted name that exists is left to
// the recovery.
func (e *Engine) clearNote(ctx context.Context, req *GrantRequest, noted string) {
	if _, err := e.k.Region(noted); err == nil {
		klog.Errorf("engine: %s: a failed cxl create-region left %s: the rollback releases it",
			memdevText(req.Asked.Source, req.Serial), noted)
		return
	}
	if err := e.commit(ctx, func(r *state.Records) error {
		ref := state.RegionRef(noted)
		reg := r.Region(ref)
		if reg == nil {
			return nil
		}
		if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionNone, state.TriggerPrepare); err != nil {
			return err
		}
		removeRecord(&r.Regions, func(x *state.Region) bool { return x.Ref() == ref })
		if m := r.Memdev(req.Serial); m != nil {
			m.Regions = removeRef(m.Regions, ref)
		}
		if g := r.Grant(req.Ref); g != nil && g.Intent.Region == ref {
			g.Intent.Region = ""
		}
		return nil
	}); err != nil {
		klog.Errorf("engine: %s: clearing the note of %s: %v", memdevText(req.Asked.Source, req.Serial), noted, err)
	}
}

// wholeDevice returns the first dax device of a ram region set up for the
// driver: it waits for the dax region and for the memory blocks that kmem
// adds, records daxN.0 as the region's Whole device, and binds it to driver
// (kmem with its memory offline, or device_dax with the memory offline for
// the settle time first). The region is SetUp afterwards.
func (e *Engine) wholeDevice(ctx context.Context, req *GrantRequest, region, driver string) (string, error) {
	reg := e.records().Region(state.RegionRef(region))
	if reg != nil && reg.SetUp {
		if d := reg.WholeDevice(); d != nil {
			return d.Name, nil
		}
		return "", nil
	}
	// set up before (a kept region): its first device may be unbound or
	// smaller after a cut resize or driver switch, or the seed that a slice
	// on it left
	again := reg != nil && len(reg.DaxDevices) > 0
	ctx, cancel := context.WithTimeout(ctx, e.settle(req.Asked.Source))
	defer cancel()
	dax, err := e.waitWhole(ctx, req, region, again)
	if err != nil {
		return "", err
	}
	if err := e.recordWhole(ctx, req, region, dax); err != nil {
		return "", err
	}
	if e.k.DaxDriver(dax) != driver {
		if err := e.switchWhole(ctx, req, region, dax, driver); err != nil {
			return "", err
		}
	}
	return dax, e.commitWhole(ctx, req, region, dax, driver)
}

// waitWhole returns the first dax device of a region, outside the engine
// lock: of a new region once it is bound to a driver
// and kmem added its memory blocks, of a region set up again (again) once it
// exists.
func (e *Engine) waitWhole(ctx context.Context, req *GrantRequest, region string, again bool) (string, error) {
	var dax string
	var err error
	if again {
		dax, err = e.k.WaitFirstDax(ctx, region)
	} else {
		dax, err = e.k.WaitDaxRegion(ctx, region)
	}
	if err != nil || e.k.DaxDriver(dax) != kernel.DriverKmem {
		return dax, err
	}
	return dax, e.k.WaitRegionBlocks(ctx, region)
}

// switchWhole binds the first dax device of a region to driver: from kmem
// with its memory offline for the settle time first, an unbound one given
// all of the memory of the region first; kmem's memory blocks waited for.
func (e *Engine) switchWhole(ctx context.Context, req *GrantRequest, region, dax, driver string) error {
	switch e.k.DaxDriver(dax) {
	case kernel.DriverKmem:
		blocks, _ := e.k.RegionBlocks(region)
		if err := e.offlineSettled(ctx, req, blocks); err != nil {
			return err
		}
	case "":
		if err := e.spanRegion(ctx, region, dax); err != nil {
			return err
		}
	}
	if err := e.locked(func() error { return e.k.BindDax(ctx, dax, driver) }); err != nil {
		return err
	}
	if driver != kernel.DriverKmem {
		return nil
	}
	return e.k.WaitRegionBlocks(ctx, region)
}

// spanRegion gives the unbound first dax device of a region all of the
// memory of the region that no other device has.
func (e *Engine) spanRegion(ctx context.Context, region, dax string) error {
	dr, err := e.k.DaxRegion(region)
	if err != nil {
		return err
	}
	cur, err := e.k.DaxDeviceSize(dax)
	if err != nil || dr.Available == 0 {
		return err
	}
	return e.locked(func() error { return e.k.ResizeDaxDevice(ctx, dax, cur+dr.Available) })
}

// recordWhole records the first dax device of a new region as observed:
// kmem with its memory offline, or device_dax by an id that another tool
// left. A region set up before keeps its record.
func (e *Engine) recordWhole(ctx context.Context, req *GrantRequest, region, dax string) error {
	driver := e.k.DaxDriver(dax)
	return e.commit(ctx, func(r *state.Records) error {
		reg := r.Region(state.RegionRef(region))
		if reg == nil {
			return fmt.Errorf("region %s has no record", region)
		}
		if reg.DaxDevice(dax) != nil {
			return nil
		}
		d := state.DaxDevice{Name: dax, State: state.DaxNone, Whole: true}
		to := state.DaxKmemOffline
		if driver == kernel.DriverDeviceDax {
			to = state.DaxDeviceDax
		}
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, to, state.TriggerPrepare); err != nil {
			return err
		}
		d.Driver = state.DaxDriver(driver)
		reg.DaxDevices = append(reg.DaxDevices, d)
		return nil
	})
}

// commitWhole records the Whole device of a region bound to driver, and
// the region SetUp.
func (e *Engine) commitWhole(ctx context.Context, req *GrantRequest, region, dax, driver string) error {
	info, _ := e.k.DaxDevice(dax)
	return e.commit(ctx, func(r *state.Records) error {
		reg := r.Region(state.RegionRef(region))
		d := reg.DaxDevice(dax)
		if d == nil {
			return fmt.Errorf("%s has no record of %s", region, dax)
		}
		if err := bindRecord(d, driver); err != nil {
			return err
		}
		d.Whole = true
		d.Size, d.Dev = info.Size, state.DevNumbers{Major: info.Major, Minor: info.Minor}
		if !reg.SetUp {
			return fsm.Apply(&state.RegionSetUpTable, &reg.SetUp, true, state.TriggerPrepare)
		}
		return nil
	})
}

// bindRecord applies the transitions of a dax device bound to driver: from
// the other driver (kmem offline, device_dax) or the seed through unbound,
// from unbound.
func bindRecord(d *state.DaxDevice, driver string) error {
	want := state.DaxDeviceDax
	if driver == kernel.DriverKmem {
		want = state.DaxKmemOffline
	}
	if d.State == want || (want == state.DaxKmemOffline && d.State == state.DaxKmemOnline) {
		d.Driver = state.DaxDriver(driver)
		return nil
	}
	switch d.State {
	case state.DaxKmemOffline, state.DaxSeed, state.DaxDeviceDax:
		// unbound from the other driver first
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxUnbound, state.TriggerPrepare); err != nil {
			return err
		}
	}
	if err := fsm.Apply(&state.DaxDeviceTable, &d.State, want, state.TriggerPrepare); err != nil {
		return err
	}
	d.Driver = state.DaxDriver(driver)
	return nil
}

// keptOnlineError is the error of memory blocks that keep coming online
// after offlining them.
type keptOnlineError struct {
	blocks []int
}

// Error returns the error in words.
func (e *keptOnlineError) Error() string {
	return fmt.Sprintf("memory blocks %v keep coming online after offlining them: is a udev rule onlining hot-added memory?", e.blocks)
}

// Is tells whether target is kernel.ErrBusyOnline.
func (e *keptOnlineError) Is(target error) bool {
	return target == kernel.ErrBusyOnline
}

// offlineSettled offlines memory blocks under the engine lock and waits,
// outside it, until they stayed offline for Timeouts.OfflineSettle: a udev
// rule may online hot-added blocks after they were offlined. Blocks that
// come online are offlined again until Timeouts.Offline.
func (e *Engine) offlineSettled(ctx context.Context, req *GrantRequest, blocks []int) error {
	deadline := time.Now().Add(e.opts.Timeouts.Offline)
	for {
		if err := e.locked(func() error { return e.k.OfflineBlocks(blocks) }); err != nil {
			return err
		}
		onlined, err := e.k.StayedOffline(ctx, blocks, e.opts.Timeouts.OfflineSettle)
		if err != nil || onlined < 0 {
			return err
		}
		e.k.Logf("memory%d: online again after offlining it (auto-onlined by udev?)", onlined)
		if time.Now().After(deadline) {
			return &keptOnlineError{blocks: blocks}
		}
	}
}

// fence fences a NUMA node before memory of it goes online, waiting at
// most Timeouts.Fence; the memory goes online anyway after that.
func (e *Engine) fence(ctx context.Context, node int) {
	if e.opts.Fence == nil || node < 0 {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, e.opts.Timeouts.Fence)
	defer cancel()
	e.opts.Fence(fctx, node)
	if errors.Is(fctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		klog.Warningf("engine: fencing NUMA node %d did not finish in %v: its memory goes online anyway", node, e.opts.Timeouts.Fence)
	}
}

// onlineMemory onlines the memory blocks of the dax device dax of a region
// movable, kmem's, and fences its NUMA node again when it is known only
// after onlining (fenced is the node fenced before, the one that the dax
// device tells; late logs the late fence); it records the device
// KMEM_ONLINE and the node of the region, and returns the node, < 0 when
// it cannot be found.
func (e *Engine) onlineMemory(ctx context.Context, region, dax string, blocks []int, fenced int, late func(node int)) (int, error) {
	if err := e.locked(func() error { return e.k.OnlineBlocks(blocks, true) }); err != nil {
		return -1, err
	}
	node := e.k.TargetNode(dax)
	if node < 0 {
		node = e.k.BlocksNUMANode(blocks)
	}
	if node < 0 {
		return -1, nil
	}
	if e.opts.Fence != nil && node != fenced {
		late(node)
		e.fence(ctx, node)
	}
	return node, e.commit(ctx, func(r *state.Records) error {
		reg := r.Region(state.RegionRef(region))
		d := reg.DaxDevice(dax)
		reg.NUMANode = state.NUMANodePtr(node)
		if d.State == state.DaxKmemOffline {
			return fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxKmemOnline, state.TriggerPrepare)
		}
		return nil
	})
}

// destroyRegions releases the regions of ours of the memdev with the serial
// by decision (Destroy, or in order), and tells whether a region was kept
// for the claim forClaim; one kept below newer regions of another memdev is
// KEPT for forClaim, or the memdev IDLE_KEPT with it when forClaim is "".
func (e *Engine) destroyRegions(ctx context.Context, serial state.Serial, memdev string, decision Decision, forClaim string, trigger state.Trigger) (bool, error) {
	m := e.records().Memdev(serial)
	if m == nil {
		return false, nil
	}
	source, persistent, regions := m.Source, m.Facts.Persistent, slices.Clone(m.Regions)
	start := time.Now()
	for _, ref := range regions {
		name := string(ref)
		if _, err := e.k.Region(name); err != nil {
			if cerr := e.commit(ctx, func(r *state.Records) error { return dropRegion(r, ref, trigger) }); cerr != nil {
				return false, cerr
			}
			continue
		}
		var rs []string
		err := e.locked(func() error {
			if decision.inOrder() {
				var err error
				rs, err = e.k.DestroyRegionInOrder(ctx, memdev, name)
				return err
			}
			return e.k.DestroyRegion(ctx, name)
		})
		if err != nil {
			return false, err
		}
		if len(rs) > 0 {
			idle := "the idle device"
			if persistent {
				idle = "the idle persistent device"
			}
			if forClaim != "" {
				klog.Warningf("engine: %s memdev %s (%s): keeping %s for claims %v, whose Prepare did not finish: %s lie above it, its release would leave a hole in the decoders of their ports; their Unprepare releases it, their Prepare again reuses it",
					source, serial.String(), memdev, name, []string{forClaim}, strings.Join(rs, ", "))
			} else {
				klog.Warningf("engine: %s memdev %s (%s): keeping %s of %s: %s lie above it, its release would leave a hole in the decoders of their ports",
					source, serial.String(), memdev, name, idle, strings.Join(rs, ", "))
			}
			return forClaim != "", e.keepMemdev(ctx, serial, trigger, forClaim != "")
		}
		if err := e.commit(ctx, func(r *state.Records) error { return dropRegion(r, ref, trigger) }); err != nil {
			return false, err
		}
		if source != state.SourcePool {
			// the pool logs the disable of its memdev
			klog.Infof("engine: local memdev %s (%s): destroyed %s in %s", serial.String(), memdev, name, since(start))
		}
	}
	return false, nil
}

// disableMemdev disables a pool memdev so that the pool can detach it,
// unless it has a region that no record of the driver names (never
// touched; logged).
func (e *Engine) disableMemdev(ctx context.Context, source state.Source, serial state.Serial, memdev string) error {
	if rs := e.k.Regions(memdev); len(rs) > 0 {
		klog.Warningf("engine: %s memdev %s (%s): not disabling it: it has regions %v that no record of the driver names",
			source, serial.String(), memdev, rs)
		return nil
	}
	if err := e.locked(func() error { return e.k.DisableMemdev(ctx, memdev) }); err != nil {
		return err
	}
	klog.Infof("engine: %s (%s): disabled", memdevText(source, serial), memdev)
	return nil
}

// hasRegion tells whether the memdev with the serial has a region.
func (e *Engine) hasRegion(serial state.Serial) bool {
	memdev, err := e.k.FindMemdev(uint64(serial))
	return err == nil && len(e.k.Regions(memdev)) > 0
}

// dropRegion removes a region of ours that is destroyed or gone from the
// records (the region -> NONE, its dax devices with it) and from its
// memdevs.
func dropRegion(r *state.Records, ref state.RegionRef, trigger state.Trigger) error {
	reg := r.Region(ref)
	if reg == nil {
		return nil
	}
	if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionNone, trigger); err != nil {
		return err
	}
	removeRecord(&r.Regions, func(x *state.Region) bool { return x.Ref() == ref })
	for i := range r.Memdevs {
		r.Memdevs[i].Regions = removeRef(r.Memdevs[i].Regions, ref)
		r.Memdevs[i].Hollow = removeRef(r.Memdevs[i].Hollow, ref)
	}
	for i := range r.Grants {
		if r.Grants[i].Got.Region == ref {
			r.Grants[i].Got.Region, r.Grants[i].Got.Dax, r.Grants[i].Got.Mount = "", "", ""
		}
	}
	return nil
}

// keepMemdev records the regions of the memdev with the serial kept, the
// kernel objects untouched: KEPT (to be set up again) when kept for claims,
// as they are otherwise, but those that the kernel has disabled (a release
// that failed after cxl disable-region) KEPT too.
func (e *Engine) keepMemdev(ctx context.Context, serial state.Serial, trigger state.Trigger, forClaims bool) error {
	disabled := map[state.RegionRef]bool{}
	if m := e.records().Memdev(serial); m != nil && !forClaims {
		for _, ref := range m.Regions {
			if ri, err := e.k.Region(string(ref)); err == nil && ri.State == kernel.RegionDisabled {
				disabled[ref] = true
			}
		}
	}
	if !forClaims && len(disabled) == 0 {
		return nil
	}
	return e.commit(ctx, func(r *state.Records) error {
		m := r.Memdev(serial)
		if m == nil {
			return nil
		}
		for _, ref := range m.Regions {
			reg := r.Region(ref)
			if reg == nil || reg.State != state.RegionOwned || !forClaims && !disabled[ref] {
				continue
			}
			if err := keepRegion(reg, trigger); err != nil {
				return err
			}
		}
		return nil
	})
}

// keepRegion records an OWNED region KEPT: disabled, to be set up again.
func keepRegion(reg *state.Region, trigger state.Trigger) error {
	if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionKept, trigger); err != nil {
		return err
	}
	// set up again, a kept region is zeroed again
	reg.Zeroed = false
	if reg.SetUp {
		return fsm.Apply(&state.RegionSetUpTable, &reg.SetUp, false, trigger)
	}
	return nil
}

// deleteHollow deletes the regions of ours of the memdev with the serial
// that the kernel left hollow (its memdev removed under them) and whose
// memory blocks are offline; one that cannot be deleted stays in the
// memdev's Hollow (logged with the command that deletes it).
func (e *Engine) deleteHollow(ctx context.Context, serial state.Serial, trigger state.Trigger, why string) error {
	m := e.records().Memdev(serial)
	if m == nil {
		return nil
	}
	for _, ref := range append(slices.Clone(m.Regions), m.Hollow...) {
		name := string(ref)
		if _, err := e.k.Region(name); err != nil {
			if err := e.commit(ctx, func(r *state.Records) error { return dropRegion(r, ref, trigger) }); err != nil {
				return err
			}
			continue
		}
		if !e.k.HollowRegion(name) {
			continue
		}
		klog.Warningf("engine: %s: %s has no target decoder (%s): deleting it", memdevText(m.Source, serial), name, why)
		if err := e.commit(ctx, func(r *state.Records) error { return hollowRegion(r, ref) }); err != nil {
			return err
		}
		if err := e.locked(func() error { return e.k.DeleteHollowRegion(name) }); err != nil {
			decoder, derr := e.k.RegionRootDecoder(name)
			if derr != nil {
				decoder = "<its root decoder>"
			}
			klog.Errorf("engine: %s: %v; it holds its address range until it is deleted, when none of its memory blocks is online: echo %s > /sys/bus/cxl/devices/%s/delete_region",
				memdevText(m.Source, serial), err, name, decoder)
			continue
		}
		klog.Infof("engine: %s: deleted hollow %s", memdevText(m.Source, serial), name)
		if err := e.commit(ctx, func(r *state.Records) error { return dropRegion(r, ref, trigger) }); err != nil {
			return err
		}
	}
	return nil
}

// hollowRegion records a region of ours whose memdev went under it
// (HOLLOW_OWNED), in the memdev's Hollow.
func hollowRegion(r *state.Records, ref state.RegionRef) error {
	reg := r.Region(ref)
	if reg == nil || reg.State == state.RegionHollowOwned {
		return nil
	}
	if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionHollowOwned, state.TriggerObserve); err != nil {
		return err
	}
	for _, serial := range reg.Targets {
		if m := r.Memdev(serial); m != nil {
			m.Regions = removeRef(m.Regions, ref)
			m.Hollow = addRef(m.Hollow, ref)
		}
	}
	return nil
}
