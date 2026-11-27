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

// Local memdevs: an unshared local memdev gives each grant a slice of its
// region (a dax device on whole memory blocks: kmem with its memory online
// movable in mode numa, device_dax zeroed in mode dax); a shared memdev gives
// every grant the Whole device of its region (device_dax, zeroed once when the
// region is new or set up again after it was kept for a claim). A new region
// of either is made with the usable size of the memdev and probed for aliasing
// (probe.go).

// realiseLocalShared realises a grant of a local memdev configured shared:
// the Whole device of its region, zeroed when the region was not zeroed
// since it was made or set up again.
func (e *Engine) realiseLocalShared(ctx context.Context, req *GrantRequest, memdev string) error {
	bs, err := e.k.BlockSize()
	if err != nil {
		// the block size only checks the alignment of the region
		bs = kernel.RegionAlign
	}
	region, dax, err := e.localRegion(ctx, req, memdev, bs)
	if err != nil {
		return err
	}
	reg := e.records().Region(state.RegionRef(region))
	if reg != nil && !reg.Zeroed {
		start := time.Now()
		size, err := e.k.DaxDeviceSize(dax)
		if err == nil {
			err = e.k.Zero(ctx, dax)
		}
		if ze := (*kernel.ZeroError)(nil); errors.As(err, &ze) {
			// the zeroing is named once
			err = ze.Err
		}
		if err != nil {
			return fmt.Errorf("memdev %s (%s): zeroing new shared %s: %w", req.Serial.String(), memdev, dax, err)
		}
		klog.Infof("engine: local memdev %s (%s): zeroed new shared %s size %d in %s", req.Serial.String(), memdev, dax, size, since(start))
		if err := e.commit(ctx, func(r *state.Records) error {
			r.Region(state.RegionRef(region)).Zeroed = true
			return nil
		}); err != nil {
			return err
		}
	}
	major, minor, err := e.k.DaxDevNumbers(dax)
	if err != nil {
		return fmt.Errorf("memdev %s (%s): %s: %w", req.Serial.String(), memdev, dax, err)
	}
	size, _ := e.k.DaxDeviceSize(dax)
	return e.realised(ctx, req, state.Got{Region: state.RegionRef(region), Dax: dax, Size: size},
		state.DevNumbers{Major: major, Minor: minor})
}

// realiseLocal realises a grant of a slice of a local memdev: the
// region of the memdev (its Whole device bound to device_dax), a dax device
// of the asked size rounded up to whole memory blocks, placed on whole
// blocks, then kmem and online movable (mode numa, the node fenced first) or
// device_dax and zeroed (mode dax).
func (e *Engine) realiseLocal(ctx context.Context, req *GrantRequest, memdev string) error {
	bs, err := e.k.BlockSize()
	if err != nil {
		return err
	}
	if got, ok := e.sliceMade(req); ok {
		// a numa slice that a Prepare cut after it went online
		return e.realised(ctx, req, got, state.DevNumbers{})
	}
	if err := e.deleteLeftSlice(ctx, req); err != nil {
		return err
	}
	region, _, err := e.localRegion(ctx, req, memdev, bs)
	if err != nil {
		return err
	}
	got, err := e.realiseSlice(ctx, req, memdev, region, bs)
	if err != nil {
		return fmt.Errorf("memdev %s (%s): %w", req.Serial.String(), memdev, err)
	}
	return e.realised(ctx, req, got, state.DevNumbers{})
}

// sliceMade returns what a grant of a Prepare that did not finish got of
// its slice of mode numa when the slice is online (KMEM_ONLINE), and whether
// it is: the Prepare again reuses it (a numa companion kept at a start).
func (e *Engine) sliceMade(req *GrantRequest) (state.Got, bool) {
	r := e.records()
	g := r.Grant(req.Ref)
	if g == nil || g.Got.Dax == "" || req.Asked.Mode != state.ModeNUMA {
		return state.Got{}, false
	}
	reg := r.Region(g.Got.Region)
	if reg == nil {
		return state.Got{}, false
	}
	d := reg.DaxDevice(g.Got.Dax)
	if d == nil || d.State != state.DaxKmemOnline || !slices.Equal(d.Users, []state.GrantRef{req.Ref}) || reg.NUMANode == nil {
		return state.Got{}, false
	}
	return state.Got{Region: g.Got.Region, Dax: d.Name, Size: d.Size, NUMANode: state.NUMANodePtr(*reg.NUMANode)}, true
}

// deleteLeftSlice deletes the slice that a grant of a Prepare that did not
// finish left (its dax device offlined and deleted), before the grant gets
// a slice anew.
func (e *Engine) deleteLeftSlice(ctx context.Context, req *GrantRequest) error {
	g := e.records().Grant(req.Ref)
	if g == nil || g.Got.Dax == "" {
		return nil
	}
	cur := g.Clone()
	return e.deleteSlice(ctx, &cur, state.TriggerPrepare)
}

// realiseSlice makes the slice of a grant of an unshared local memdev in its
// region (addSlice) and binds it for its mode, and returns what the grant
// got.
func (e *Engine) realiseSlice(ctx context.Context, req *GrantRequest, memdev, region string, bs uint64) (state.Got, error) {
	dax, size, err := e.addSlice(ctx, req, memdev, region, bs)
	if err != nil {
		return state.Got{}, err
	}
	got := state.Got{Region: state.RegionRef(region), Dax: dax, Size: size}
	if req.Asked.Mode == state.ModeNUMA {
		node, err := e.onlineSlice(ctx, req, memdev, region, dax, size, bs)
		got.NUMANode = state.NUMANodePtr(node)
		return got, err
	}
	return got, e.bindSlice(ctx, req, memdev, region, dax, size)
}

// localRegion returns the ram region of ours of the memdev of a local grant
// and its Whole device, bound to device_dax: an established region as it
// is; a new one made with the usable size of the memdev; a kept one set up
// again. A new or set up region is probed for aliasing: one whose memory
// aliases is released and made again with the distinct bytes only (once),
// unless a region of another memdev lies above it: then it stays, not set
// up, and the grant fails until nothing lies above it.
func (e *Engine) localRegion(ctx context.Context, req *GrantRequest, memdev string, bs uint64) (string, string, error) {
	for try := 0; ; try++ {
		region, whole, fresh, err := e.setUpLocal(ctx, req, memdev, bs)
		if err != nil || !fresh {
			return region, whole, err
		}
		aliased, err := e.probeRegion(ctx, req, memdev, region, whole)
		if err != nil || aliased == 0 {
			return region, whole, err
		}
		if err := e.remakeAliased(ctx, req, memdev, region, aliased, try); err != nil {
			return "", "", err
		}
	}
}

// setUpLocal returns the ram region of ours of the memdev of a local grant
// and its Whole device bound to device_dax, made (with the usable size of
// the memdev) or set up again, and tells whether it is either, to be
// probed.
func (e *Engine) setUpLocal(ctx context.Context, req *GrantRequest, memdev string, bs uint64) (string, string, bool, error) {
	s := req.Serial.String()
	reg := ownRegion(e.records(), req.Serial, state.RegionRAM)
	created, again := reg == nil, reg != nil && !reg.SetUp
	usable := e.usableSize(req)
	if ram, err := e.k.RamSize(memdev); err == nil && usable >= ram {
		usable = 0
	}
	if created && usable > 0 {
		klog.Infof("engine: local memdev %s (%s): creating a region of its first %d bytes only (usable size)", s, memdev, usable)
	}
	start := time.Now()
	region, err := e.ensureRegion(ctx, req, memdev, state.RegionRAM, usable)
	if err != nil && !errors.Is(err, errForeignRegion) {
		err = fmt.Errorf("memdev %s (%s): %w", s, memdev, err)
	}
	if err != nil {
		return "", "", false, err
	}
	whole, err := e.wholeDevice(ctx, req, region, kernel.DriverDeviceDax)
	switch {
	case err != nil && again:
		return "", "", false, fmt.Errorf("memdev %s (%s): setting up kept %s: %w", s, memdev, region, err)
	case err != nil:
		return "", "", false, err
	case created:
		e.logCreated(memdev, req.Serial, region, whole, bs, start)
	case again:
		klog.Infof("engine: local memdev %s (%s): set up kept %s (devdax, dax %s, NUMA node %d) in %s", s, memdev, region, whole,
			e.k.TargetNode(whole), since(start))
	}
	return region, whole, created || again, nil
}

// remakeAliased releases the region of a local memdev whose memory aliases
// (aliased distinct bytes) for one of the usable size, unless it is made
// again already (try) or a region of another memdev lies above it.
func (e *Engine) remakeAliased(ctx context.Context, req *GrantRequest, memdev, region string, aliased uint64, try int) error {
	s := req.Serial.String()
	if try > 0 {
		return fmt.Errorf("memdev %s (%s): %s of %d bytes still aliases", s, memdev, region, aliased)
	}
	above, err := e.destroyAliased(ctx, req, memdev, region)
	if err != nil {
		return fmt.Errorf("memdev %s (%s): destroying aliased %s: %w", s, memdev, region, err)
	}
	if len(above) > 0 {
		return &aliasedBelowError{memdev: memdev, serial: req.Serial, region: region, usable: aliased}
	}
	klog.Infof("engine: local memdev %s (%s): destroyed aliased %s, creating a region of %d bytes", s, memdev, region, e.usableSize(req))
	return nil
}

// logCreated logs the new region of a local memdev, and warns when it does
// not start at a boundary of the memory blocks of bs bytes.
func (e *Engine) logCreated(memdev string, serial state.Serial, region, whole string, bs uint64, start time.Time) {
	s := serial.String()
	klog.Infof("engine: local memdev %s (%s): created %s (devdax, dax %s, NUMA node %d) in %s", s, memdev, region, whole,
		e.k.TargetNode(whole), since(start))
	if r, err := e.k.RegionRange(region); err == nil && bs > 0 && r.Start%bs != 0 {
		usable, end := uint64(0), (r.End+1)/bs*bs
		if start := roundUp(r.Start, bs); end > start {
			usable = end - start
		}
		klog.Warningf("engine: local memdev %s (%s): %s at %v does not start at a memory block boundary (block size %d): slices use only its %d bytes of whole blocks",
			s, memdev, region, r, bs, usable)
	}
}

// destroyAliased releases the region of a local memdev whose memory aliases
// in order, for a smaller one, and returns the regions of other memdevs
// above it that refused the release: the region is kept for the claim then
// (KEPT, not set up).
func (e *Engine) destroyAliased(ctx context.Context, req *GrantRequest, memdev, region string) ([]string, error) {
	var above []string
	if err := e.locked(func() error {
		var err error
		above, err = e.k.DestroyRegionInOrder(ctx, memdev, region)
		return err
	}); err != nil {
		return nil, err
	}
	if len(above) > 0 {
		return above, e.keepMemdev(ctx, req.Serial, state.TriggerPrepare, true)
	}
	return nil, e.commit(ctx, func(r *state.Records) error {
		if g := r.Grant(req.Ref); g != nil && g.Intent.Region == state.RegionRef(region) {
			g.Intent.Region = ""
		}
		return dropRegion(r, state.RegionRef(region), state.TriggerPrepare)
	})
}

// localKindError is the error of a local grant on a memdev that grants of
// claims use in another way.
type localKindError struct {
	memdev       string
	serial       state.Serial
	claims       []string
	how, refusal string
}

// Error returns the refusal in words.
func (e *localKindError) Error() string {
	return fmt.Sprintf("memdev %s (%s): used %s by claims %v; %s", e.serial.String(), e.memdev, e.how, e.claims, e.refusal)
}

// checkLocalKind returns an error when grants of other claims use the local
// memdev of a request in another way than it asks (local, local-shared,
// local-persistent; a persistent memdev shared or not); a memdev that no grant
// uses but whose region is of another way is released first as an idle one
// (a persistent one when nothing holds its block device).
func (e *Engine) checkLocalKind(ctx context.Context, req *GrantRequest) error {
	if req.Asked.Source != state.SourceLocal {
		return nil
	}
	r := e.records()
	m := r.Memdev(req.Serial)
	if m == nil || m.Source != state.SourceLocal {
		return nil
	}
	persistent, shared := req.Asked.Kind.Persistent(), req.Facts.Shared
	if req.Asked.Kind.Shared() {
		shared = true
	}
	if m.Facts.Persistent == persistent && m.Facts.Shared == shared {
		return nil
	}
	memdev := e.memdevName(req.Serial)
	if claims := otherClaims(m, req.Ref.Claim); len(claims) > 0 {
		how, refusal := kindConflict(m.Facts, persistent, shared)
		return &localKindError{memdev: memdev, serial: req.Serial, claims: claims, how: how, refusal: refusal}
	}
	if len(m.Grants) > 0 || len(m.Regions) == 0 || m.Facts.Persistent == persistent && persistent {
		// the claim's own grants hold it, or a persistent memdev that is
		// shared now or not: its region stays
		return nil
	}
	// the request tells whether the memdev is configured shared now
	return e.releaseIdle(ctx, m, e.idleKind(m, shared), time.Now(), state.TriggerPrepare)
}

// kindConflict returns how the grants of a local memdev with the facts
// use it and why a request (persistent, shared) cannot use it so, in the
// words of the refusal: the kinds of their DeviceClasses.
func kindConflict(f state.MemdevFacts, persistent, shared bool) (string, string) {
	used := kindOfFacts(f)
	if f.Persistent && persistent {
		return fmt.Sprintf("as a %s %s device", sharedWord(f.Shared), used), fmt.Sprintf("it is %s now (sharedMemdevs)", sharedWord(shared))
	}
	return fmt.Sprintf("as a %s device", used), fmt.Sprintf("a %s request cannot use it", state.KindOf(state.SourceLocal, shared, persistent))
}

// sharedWord returns "shared" or "not shared".
func sharedWord(shared bool) string {
	if shared {
		return "shared"
	}
	return "not shared"
}

// kindOfFacts returns the grant kind of a local memdev with the facts.
func kindOfFacts(f state.MemdevFacts) state.GrantKind {
	return state.KindOf(state.SourceLocal, f.Shared, f.Persistent)
}

// otherClaims returns the claims but uid of the grants of a memdev, in
// ascending order.
func otherClaims(m *state.Memdev, uid string) []string {
	var out []string
	for _, ref := range m.Grants {
		if ref.Claim != uid && !slices.Contains(out, ref.Claim) {
			out = append(out, ref.Claim)
		}
	}
	slices.Sort(out)
	return out
}

// roundUp returns v rounded up to a multiple of step (v if step is 0).
func roundUp(v, step uint64) uint64 {
	if step == 0 || v%step == 0 {
		return v
	}
	return (v/step + 1) * step
}

// addSlice makes the dax device of a slice of the memdev for a grant and
// returns it with its size: the unused Whole device daxN.0 or the seed
// resized, else a new device placed first-fit by the kernel, on the first
// whole free blocks instead when first-fit crossed a block boundary. The
// device is recorded UNBOUND with the grant its user.
func (e *Engine) addSlice(ctx context.Context, req *GrantRequest, memdev, region string, bs uint64) (string, uint64, error) {
	first, size, err := e.sliceRoom(req, memdev, region, bs)
	if err != nil {
		return "", 0, err
	}
	start := time.Now()
	dax := first
	if first != "" {
		err = e.locked(func() error { return e.k.ResizeDaxDevice(ctx, first, size) })
	} else {
		err = e.locked(func() error {
			var err error
			dax, err = e.k.CreateDaxDevice(ctx, region, size)
			return err
		})
	}
	if err != nil {
		return "", 0, err
	}
	if err := e.commit(ctx, func(r *state.Records) error { return sliceRecord(r, req.Ref, region, dax, first != "") }); err != nil {
		return "", 0, err
	}
	aligned, err := e.alignSlice(ctx, req, memdev, region, dax, size, bs)
	if err != nil {
		return "", 0, err
	}
	ranges, _ := e.k.DaxDeviceRanges(aligned)
	info, _ := e.k.DaxDevice(aligned)
	dev := state.DevNumbers{Major: info.Major, Minor: info.Minor}
	if err := e.commit(ctx, func(r *state.Records) error { return sizedSlice(r, req.Ref, region, aligned, size, dev) }); err != nil {
		return "", 0, err
	}
	klog.Infof("engine: local memdev %s (%s): claim %s: created slice %s mode %s size %d at %v in %s",
		req.Serial.String(), memdev, req.Ref.Claim, aligned, req.Asked.Mode, size, ranges, since(start))
	return aligned, size, nil
}

// sliceRoom returns the device that a slice of the memdev for a grant
// reuses (the unused daxN.0 that kmem does not have, "" for a new one) and
// the size of the slice: the asked size rounded up to the memory block size
// bs and the align of the dax region; an error when the region has no room.
func (e *Engine) sliceRoom(req *GrantRequest, memdev, region string, bs uint64) (string, uint64, error) {
	dr, err := e.k.DaxRegion(region)
	if err != nil {
		return "", 0, err
	}
	size := req.Asked.Size
	if s := roundUp(roundUp(size, bs), dr.Align); s != size {
		klog.Warningf("engine: local memdev %s (%s): claim %s: slice of %d bytes rounded up to %d (memory block size %d, align %d)",
			req.Serial.String(), memdev, req.Ref.Claim, size, s, bs, dr.Align)
		size = s
	}
	reg := e.records().Region(state.RegionRef(region))
	first := ""
	avail := dr.Available
	for _, dax := range dr.Devices {
		d := reg.DaxDevice(dax)
		if strings.HasSuffix(dax, ".0") && d != nil && len(d.Users) == 0 && e.k.DaxDriver(dax) != kernel.DriverKmem {
			first = dax
			cur, _ := e.k.DaxDeviceSize(dax)
			avail += cur
		}
	}
	if avail < size {
		klog.Errorf("engine: local memdev %s (%s): %d bytes free in %s, a slice needs %d: claims allocated more than the capacity?",
			req.Serial.String(), memdev, avail, region, size)
		return "", 0, fmt.Errorf("%w for the slice: %d bytes free in %s, the slice needs %d", kernel.ErrNoRoom, avail, region, size)
	}
	return first, size, nil
}

// sizedSlice records the dax device of a slice placed: UNBOUND, with its
// size, memory and numbers, the grant its user.
func sizedSlice(r *state.Records, ref state.GrantRef, region, dax string, size uint64, dev state.DevNumbers) error {
	d := r.Region(state.RegionRef(region)).DaxDevice(dax)
	if d.State == state.DaxCreating {
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxUnbound, state.TriggerPrepare); err != nil {
			return err
		}
	}
	d.Size, d.Users, d.Dev = size, []state.GrantRef{ref}, dev
	return nil
}

// sliceRecord records the dax device of a new slice: the Whole device
// daxN.0 resized (DEVICE_DAX -> UNBOUND, no longer the Whole device), the
// seed sized again, or a new device (NONE -> CREATING).
func sliceRecord(r *state.Records, ref state.GrantRef, region, dax string, reused bool) error {
	reg := r.Region(state.RegionRef(region))
	d := reg.DaxDevice(dax)
	if d == nil {
		reg.DaxDevices = append(reg.DaxDevices, state.DaxDevice{Name: dax, State: state.DaxNone})
		d = &reg.DaxDevices[len(reg.DaxDevices)-1]
	}
	switch {
	case reused && (d.State == state.DaxDeviceDax || d.State == state.DaxSeed):
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxUnbound, state.TriggerPrepare); err != nil {
			return err
		}
		d.Whole, d.Driver = false, ""
	case d.State == state.DaxNone:
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxCreating, state.TriggerPrepare); err != nil {
			return err
		}
	}
	d.Users = []state.GrantRef{ref}
	if g := r.Grant(ref); g != nil {
		g.Got.Region, g.Got.Dax = state.RegionRef(region), dax
	}
	return nil
}

// alignSlice returns the dax device of a slice of size bytes with memory
// on whole memory blocks of bs bytes: dax, or a new device on the first
// whole free blocks of the region, in address order, that replaces dax.
func (e *Engine) alignSlice(ctx context.Context, req *GrantRequest, memdev, region, dax string, size, bs uint64) (string, error) {
	ranges, err := e.k.DaxDeviceRanges(dax)
	if err != nil {
		return dax, err
	}
	aligned := len(ranges) > 0
	for _, r := range ranges {
		aligned = aligned && r.Aligned(bs)
	}
	if aligned {
		return dax, nil
	}
	klog.Warningf("engine: local memdev %s (%s): claim %s: slice %s at %v is not aligned to the memory block size %d, placing it explicitly",
		req.Serial.String(), memdev, req.Ref.Claim, dax, ranges, bs)
	if err := e.locked(func() error { return e.k.DeleteDaxDevice(ctx, region, dax) }); err != nil {
		return dax, err
	}
	if err := e.commit(ctx, func(r *state.Records) error { return deletedSlice(r, req.Ref, region, dax) }); err != nil {
		return "", err
	}
	gaps, err := e.k.RegionFreeGaps(region)
	if err != nil {
		return "", err
	}
	whole := blockRanges(gaps, size, bs)
	if whole == nil {
		return "", fmt.Errorf("%w for the slice: no %d bytes of whole free memory blocks in %s (free %v, memory block size %d)", kernel.ErrNoRoom, size, region, gaps, bs)
	}
	var placed string
	if err := e.locked(func() error {
		var err error
		placed, err = e.k.CreateDaxDeviceRanges(ctx, region, whole)
		return err
	}); err != nil {
		return "", err
	}
	return placed, e.commit(ctx, func(r *state.Records) error { return sliceRecord(r, req.Ref, region, placed, false) })
}

// deletedSlice records the dax device of a slice whose placement failed as
// deleted: the seed for daxN.0, else gone.
func deletedSlice(r *state.Records, ref state.GrantRef, region, dax string) error {
	reg := r.Region(state.RegionRef(region))
	d := reg.DaxDevice(dax)
	if d == nil {
		return nil
	}
	d.Users = nil
	if strings.HasSuffix(dax, ".0") {
		d.Size = 0
		return fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxSeed, state.TriggerPrepare)
	}
	if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxNone, state.TriggerPrepare); err != nil {
		return err
	}
	removeRecord(&reg.DaxDevices, func(x *state.DaxDevice) bool { return x.Name == dax })
	if g := r.Grant(ref); g != nil && g.Got.Dax == dax {
		g.Got.Dax = ""
	}
	return nil
}

// blockRanges returns the parts of the ranges that are whole memory blocks
// of bs bytes, in address order, up to size bytes; nil if they have less.
func blockRanges(ranges []kernel.Range, size, bs uint64) []kernel.Range {
	var out []kernel.Range
	for _, r := range ranges {
		start, end := roundUp(r.Start, bs), (r.End+1)/bs*bs
		if start >= end || size == 0 {
			continue
		}
		n := min(end-start, size)
		out = append(out, kernel.Range{Start: start, End: start + n - 1})
		size -= n
	}
	if size > 0 {
		return nil
	}
	return out
}

// onlineSlice binds the dax device of a slice of mode numa to kmem, onlines
// its memory blocks movable and returns the NUMA node of the memory. The
// node is fenced before the memory goes online when the dax device tells
// it, else right after.
func (e *Engine) onlineSlice(ctx context.Context, req *GrantRequest, memdev, region, dax string, size, bs uint64) (int, error) {
	start := time.Now()
	fenced := e.k.TargetNode(dax)
	e.fence(ctx, fenced)
	if err := e.locked(func() error { return e.k.BindDax(ctx, dax, kernel.DriverKmem) }); err != nil {
		return -1, err
	}
	if err := e.commit(ctx, func(r *state.Records) error {
		return bindRecord(r.Region(state.RegionRef(region)).DaxDevice(dax), kernel.DriverKmem)
	}); err != nil {
		return -1, err
	}
	if err := e.k.WaitDaxBlocks(ctx, dax); err != nil {
		return -1, err
	}
	blocks, err := e.k.DaxDeviceBlocks(dax, bs)
	if err != nil {
		return -1, err
	}
	node, err := e.onlineMemory(ctx, region, dax, blocks, fenced, func(node int) {
		klog.V(3).Infof("engine: local memdev %s (%s): claim %s: NUMA node %d of %s known only after onlining, fencing it late",
			req.Serial.String(), memdev, req.Ref.Claim, node, dax)
	})
	switch {
	case err != nil:
		return -1, err
	case node < 0:
		return -1, fmt.Errorf("cannot find the NUMA node of %s", dax)
	}
	klog.Infof("engine: local memdev %s (%s): claim %s: onlined slice %s mode %s size %d blocks %v NUMA node %d in %s",
		req.Serial.String(), memdev, req.Ref.Claim, dax, req.Asked.Mode, size, blocks, node, since(start))
	return node, nil
}

// bindSlice binds the dax device of a slice of mode dax to device_dax,
// and zeroes it.
func (e *Engine) bindSlice(ctx context.Context, req *GrantRequest, memdev, region, dax string, size uint64) error {
	if err := e.locked(func() error { return e.k.BindDax(ctx, dax, kernel.DriverDeviceDax) }); err != nil {
		return err
	}
	major, minor, err := e.k.DaxDevNumbers(dax)
	if err != nil {
		return err
	}
	dev := state.DevNumbers{Major: major, Minor: minor}
	if err := e.commit(ctx, func(r *state.Records) error {
		d := r.Region(state.RegionRef(region)).DaxDevice(dax)
		d.Dev = dev
		return bindRecord(d, kernel.DriverDeviceDax)
	}); err != nil {
		return err
	}
	start := time.Now()
	if err := e.k.Zero(ctx, dax); err != nil {
		// "zeroing daxN.M: ..."
		return err
	}
	klog.Infof("engine: local memdev %s (%s): claim %s: zeroed slice %s mode %s size %d in %s",
		req.Serial.String(), memdev, req.Ref.Claim, dax, req.Asked.Mode, size, since(start))
	return nil
}

// logReleased logs the release of a grant of a local memdev with the claims
// that are left: of a shared memdev at every release but the recovery's
// (the start logs what it keeps), of a persistent one when claims are
// left; at the recovery, that the claim is not prepared.
func (e *Engine) logReleased(g *state.Grant, release Release) {
	m := e.records().Memdev(g.Serial)
	if g.Asked.Source != state.SourceLocal || m == nil {
		return
	}
	memdev, s, claim := e.memdevName(g.Serial), g.Serial.String(), g.Ref.Claim
	left := otherClaims(m, claim)
	if left == nil {
		left = []string{}
	}
	switch {
	case release == ReleaseRecovery && g.Asked.Kind.Shared():
		klog.Infof("engine: local memdev %s (%s): claim %s of the shared device is not prepared, dropping it", s, memdev, claim)
	case release == ReleaseRecovery && g.Asked.Kind.Persistent():
		klog.Infof("engine: local memdev %s (%s): claim %s of the persistent device is not prepared, dropping it", s, memdev, claim)
	case release == ReleaseRecovery:
		klog.Infof("engine: local memdev %s (%s): claim %s is not prepared, releasing it", s, memdev, claim)
	case g.Asked.Kind.Shared():
		dax := ""
		if reg := ownRegion(e.records(), g.Serial, state.RegionRAM); reg != nil {
			dax = reg.FirstDax()
		}
		klog.Infof("engine: local memdev %s (%s): claim %s: released shared %s, claims left %v", s, memdev, claim, dax, left)
	case g.Asked.Kind.Persistent() && len(left) > 0:
		dev := ""
		if g.Got.BlockDevice != nil {
			dev = g.Got.BlockDevice.Name
		}
		klog.Infof("engine: local memdev %s (%s): claim %s: released persistent %s, claims left %v", s, memdev, claim, dev, left)
	}
}

// keptIdle logs the region of a local shared memdev that its release keeps
// for the idle memdev, but at the recovery (the start logs what it keeps).
func (e *Engine) keptIdle(g *state.Grant, memdev string, decision Decision, release Release) {
	if decision != KeepIdle || g.Asked.Source != state.SourceLocal || release == ReleaseRecovery {
		return
	}
	if reg := ownRegion(e.records(), g.Serial, state.RegionRAM); reg != nil {
		klog.Infof("engine: local memdev %s (%s): keeping %s and %s of the idle shared device", g.Serial.String(), memdev, reg.Ref(), reg.FirstDax())
	}
}

// deleteSlice deletes the dax device of a grant: its own one (a slice of an
// unshared memdev) offlined and deleted, or the grant removed from the users
// of a shared one.
func (e *Engine) deleteSlice(ctx context.Context, g *state.Grant, trigger state.Trigger) error {
	if g.Got.Region == "" || g.Got.Dax == "" {
		return nil
	}
	reg := e.records().Region(g.Got.Region)
	if reg == nil {
		return nil
	}
	d := reg.DaxDevice(g.Got.Dax)
	if d == nil {
		return nil
	}
	if own := !d.Whole && len(d.Users) == 1 && d.Users[0] == g.Ref; !own {
		// a shared device: the grant leaves its users when it is released
		return nil
	}
	region, dax := string(g.Got.Region), g.Got.Dax
	memdev := e.memdevName(g.Serial)
	logStep := func(step string, start time.Time, details string) {
		klog.Infof("engine: local memdev %s (%s): claim %s: %s slice %s mode %s size %d%s in %s",
			g.Serial.String(), memdev, g.Ref.Claim, step, dax, g.Asked.Mode, d.Size, details, since(start))
	}
	start := time.Now()
	blocks, err := e.offlineDax(ctx, dax)
	if err != nil {
		return fmt.Errorf("memdev %s (%s): deleting slice %s: %w", g.Serial.String(), memdev, dax, err)
	}
	if len(blocks) > 0 {
		logStep("offlined", start, fmt.Sprintf(" blocks %v", blocks))
	}
	start = time.Now()
	if err := e.locked(func() error { return e.k.DeleteDaxDevice(ctx, region, dax) }); err != nil {
		return fmt.Errorf("memdev %s (%s): deleting slice %s: %w", g.Serial.String(), memdev, dax, err)
	}
	logStep("deleted", start, "")
	return e.commit(ctx, func(r *state.Records) error { return deletedOwnSlice(r, g.Ref, g.Got.Region, dax, trigger) })
}

// offlineDax offlines the memory blocks of the dax device dax when it is
// bound to kmem, under the engine lock; it returns the blocks offlined.
func (e *Engine) offlineDax(ctx context.Context, dax string) ([]int, error) {
	if e.k.DaxDriver(dax) != kernel.DriverKmem {
		return nil, nil
	}
	bs, err := e.k.BlockSize()
	var blocks []int
	if err == nil {
		blocks, err = e.k.DaxDeviceBlocks(dax, bs)
	}
	if err == nil {
		err = e.locked(func() error { return e.k.OfflineBlocks(blocks) })
	}
	if err != nil {
		return nil, fmt.Errorf("cannot offline its memory (in use?): %w", err)
	}
	return blocks, nil
}

// deletedOwnSlice records the own dax device of a grant deleted: a device that
// a cut Prepare made goes (CREATING -> NONE), another one becomes the
// seed (daxN.0) or goes.
func deletedOwnSlice(r *state.Records, ref state.GrantRef, region state.RegionRef, dax string, trigger state.Trigger) error {
	reg := r.Region(region)
	d := reg.DaxDevice(dax)
	if rg := r.Grant(ref); rg != nil {
		rg.Got.Dax = ""
	}
	if d.State == state.DaxCreating {
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxNone, trigger); err != nil {
			return err
		}
		removeRecord(&reg.DaxDevices, func(x *state.DaxDevice) bool { return x.Name == dax })
		return nil
	}
	d.Users = nil
	if strings.HasSuffix(dax, ".0") {
		d.Driver, d.Size, d.Dev = "", 0, state.DevNumbers{}
		return fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxSeed, trigger)
	}
	if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxNone, trigger); err != nil {
		return err
	}
	removeRecord(&reg.DaxDevices, func(x *state.DaxDevice) bool { return x.Name == dax })
	return nil
}
