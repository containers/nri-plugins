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
	"path/filepath"
	"slices"
	"time"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// The start of the local memdevs, after the release of the cut Prepares:
// the slices of prepared claims whose dax device is gone are forgotten, and
// every local memdev is kept or released as its use and the configuration
// say: a shared one is kept while configured shared (and while claims use
// it), a persistent one without claims released in order when nothing
// holds its block device, an unshared one without claims released.

// forgetGoneSlices forgets the dax devices of the realised grants of
// prepared claims of local memdevs that are gone or have another size than
// recorded (a reboot that the boot ids do not tell, another tool): the
// grant keeps its region without a device node, which no container gets;
// a dax device of another size there belongs to no grant then.
func (e *Engine) forgetGoneSlices(ctx context.Context) {
	r := e.records()
	var gone []state.GrantRef
	for i := range r.Grants {
		g := &r.Grants[i]
		d := e.ownSlice(r, g)
		if d == nil {
			continue
		}
		if size, err := e.k.DaxDeviceSize(g.Got.Dax); err == nil && size == d.Size {
			continue
		}
		klog.Warningf("engine: local memdev %s (%s): claim %s: slice %s of %d bytes is gone, dropping it", g.Serial.String(),
			e.memdevName(g.Serial), g.Ref.Claim, g.Got.Dax, d.Size)
		if g.Asked.Mode == state.ModeDax {
			klog.Warningf("engine: claim %s: no device node for the dax slice %s: the slice is gone", g.Ref.Claim, filepath.Join(e.devRoot(), g.Got.Dax))
		}
		gone = append(gone, g.Ref)
	}
	if len(gone) == 0 {
		return
	}
	present := map[string]bool{}
	for _, ref := range gone {
		g := r.Grant(ref)
		_, err := e.k.DaxDevice(g.Got.Dax)
		present[g.Got.Dax] = err == nil
	}
	if err := e.commit(ctx, func(r *state.Records) error { return forgotSlices(r, gone, present) }); err != nil {
		klog.Errorf("engine: %v", err)
	}
}

// ownSlice returns the dax device of its own of a realised grant of a
// prepared claim on an unshared local memdev, nil for another grant.
func (e *Engine) ownSlice(r *state.Records, g *state.Grant) *state.DaxDevice {
	p := r.PreparationOf(g.Ref)
	if g.Asked.Source != state.SourceLocal || !e.owns(g.Asked.Source) || g.Asked.Kind != state.KindLocal || g.Got.Dax == "" ||
		!prepared(g) || p == nil || !p.Prepared() {
		return nil
	}
	reg := r.Region(g.Got.Region)
	if reg == nil {
		return nil
	}
	if d := reg.DaxDevice(g.Got.Dax); d != nil && !d.Whole {
		return d
	}
	return nil
}

// forgotSlices records the dax devices of the grants gone forgotten: the
// grants without them, a device that the kernel has (present) without
// users, one that it has not dropped.
func forgotSlices(r *state.Records, gone []state.GrantRef, present map[string]bool) error {
	for _, ref := range gone {
		g := r.Grant(ref)
		reg := r.Region(g.Got.Region)
		dax := g.Got.Dax
		g.Got.Dax = ""
		d := reg.DaxDevice(dax)
		if d == nil {
			continue
		}
		d.Users = removeRef(d.Users, ref)
		if len(d.Users) > 0 || present[dax] {
			continue
		}
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxNone, state.TriggerRecover); err != nil {
			return err
		}
		removeRecord(&reg.DaxDevices, func(x *state.DaxDevice) bool { return x.Name == dax })
	}
	return nil
}

// recoverLocal keeps or releases every local memdev after the release of
// the cut Prepares (see the top of the file). Errors are logged.
func (e *Engine) recoverLocal(ctx context.Context) {
	if !e.owns(state.SourceLocal) {
		return
	}
	for _, m := range slices.Clone(e.records().Memdevs) {
		if m.Source != state.SourceLocal || m.State == state.MemdevGone {
			continue
		}
		var err error
		switch {
		case m.Facts.Persistent:
			err = e.recoverPersistent(ctx, &m)
		case m.Facts.Shared:
			err = e.recoverShared(ctx, &m)
		case len(m.Grants) == 0:
			err = e.releaseIdleLocal(ctx, &m)
		}
		if err != nil {
			klog.Errorf("engine: %v", err)
		}
	}
}

// localRegionNames returns the region of ours of a local memdev and its
// first dax device, "" for none.
func (e *Engine) localRegionNames(m *state.Memdev) (string, string) {
	r := e.records()
	for _, ref := range m.Regions {
		if reg := r.Region(ref); reg != nil && reg.Ours() {
			return string(reg.Ref()), reg.FirstDax()
		}
	}
	return "", ""
}

// recoverShared keeps the region of a shared local memdev while claims use
// it, else as the release policy decides for an idle memdev (kept while it
// is configured shared).
func (e *Engine) recoverShared(ctx context.Context, m *state.Memdev) error {
	region, dax := e.localRegionNames(m)
	if region == "" {
		return nil
	}
	memdev, s := e.memdevName(m.Serial), m.Serial.String()
	claims := otherClaims(m, "")
	shared := e.configuredShared(m.Serial)
	switch {
	case len(claims) == 0 && !Policy(ReleaseIdle, e.idleKind(m, shared), nil).keeps():
		klog.Infof("engine: local memdev %s (%s): not in sharedMemdevs any more, destroying its region", s, memdev)
		return e.releaseIdleLocal(ctx, m)
	case shared:
		klog.Infof("engine: local memdev %s (%s): keeping shared %s and %s, claims %v", s, memdev, region, dax, claims)
	default:
		klog.Warningf("engine: local memdev %s (%s): not in sharedMemdevs any more: keeping %s for claims %v, not publishing the memdev until they are gone",
			s, memdev, region, claims)
	}
	return nil
}

// recoverPersistent keeps the region of a persistent local memdev that
// claims use, and releases one without claims as an idle memdev (in order:
// it keeps its region while regions of other memdevs lie above it).
func (e *Engine) recoverPersistent(ctx context.Context, m *state.Memdev) error {
	if len(m.Grants) > 0 {
		region, _ := e.localRegionNames(m)
		klog.Infof("engine: local memdev %s (%s): keeping persistent %s, claims %v", m.Serial.String(), e.memdevName(m.Serial), region, otherClaims(m, ""))
		return nil
	}
	return e.releaseIdleLocal(ctx, m)
}

// releaseIdleLocal is releaseIdleQueued of the recovery at the start: it
// takes the queue of the memdev first.
func (e *Engine) releaseIdleLocal(ctx context.Context, m *state.Memdev) error {
	leave, err := e.acquire(ctx, m.Serial)
	if err != nil {
		return err
	}
	defer leave()
	return e.releaseIdleQueued(ctx, m, state.TriggerRecover)
}

// releaseIdleQueued releases a local memdev without grants (releaseIdle),
// its queue held, only when nothing holds its block device now: a held one
// keeps its region, with a warning.
func (e *Engine) releaseIdleQueued(ctx context.Context, m *state.Memdev, trigger state.Trigger) error {
	err := e.releaseIdle(ctx, m, e.idleKind(m, e.configuredShared(m.Serial)), time.Now(), trigger)
	var re *releaseError
	if errors.As(err, &re) && re.held {
		region, _ := e.localRegionNames(m)
		klog.Warningf("engine: local memdev %s (%s): keeping %s of the idle persistent device: %v", m.Serial.String(), e.memdevName(m.Serial), region, re.err)
		return nil
	}
	return err
}

// ForgetLocal forgets the records of the local memdevs with the serials
// whose region was being made, without touching their regions: they are
// pool devices now. A region is being made while it is NOTED,
// and while no grant of the memdev got anything of it (a slice or a block
// device), all of them of Prepares that the process stop cut; those grants
// go with the records.
func (e *Engine) ForgetLocal(ctx context.Context, serials []state.Serial) error {
	r := e.records()
	var forget []state.Serial
	for _, serial := range serials {
		if m := r.Memdev(serial); m == nil || m.Source != state.SourceLocal || !regionInMaking(r, m) {
			continue
		}
		memdev, _ := e.k.FindMemdev(uint64(serial))
		klog.Warningf("engine: local memdev %s (%s): its region was being created, and it is a pool device now: forgetting it without touching its regions %v",
			serial.String(), memdev, e.k.Regions(memdev))
		forget = append(forget, serial)
	}
	if len(forget) == 0 {
		return nil
	}
	return e.commit(ctx, func(r *state.Records) error {
		for _, serial := range forget {
			if err := forgetMemdev(r, serial); err != nil {
				return err
			}
		}
		return nil
	})
}

// regionInMaking tells whether a region of the memdev is being made
// (state.Records.RegionInMaking).
func regionInMaking(r *state.Records, m *state.Memdev) bool {
	return slices.ContainsFunc(m.Regions, func(ref state.RegionRef) bool {
		reg := r.Region(ref)
		return reg != nil && r.RegionInMaking(m, reg)
	})
}

// forgetMemdev removes the record of the memdev with the serial, its
// grants and its region records, at a start.
func forgetMemdev(r *state.Records, serial state.Serial) error {
	m := r.Memdev(serial)
	for _, ref := range m.Grants {
		if g := r.Grant(ref); g != nil {
			if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantNone, state.TriggerRecover); err != nil {
				return err
			}
			removeRecord(&r.Grants, func(x *state.Grant) bool { return x.Ref == ref })
		}
	}
	for _, ref := range append(slices.Clone(m.Regions), m.Hollow...) {
		reg := r.Region(ref)
		if reg == nil {
			continue
		}
		if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionNone, state.TriggerRecover); err != nil {
			return err
		}
		removeRecord(&r.Regions, func(x *state.Region) bool { return x.Ref() == ref })
	}
	m.Grants, m.Regions, m.Hollow = nil, nil, nil
	return dropMemdev(r, serial, state.TriggerRecover)
}
