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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// Recovered is what the recovery tells the kubelet plugins; the records tell
// the rest.
type Recovered struct {
	// Why are the reasons of the grants of prepared claims whose node
	// mount is not mounted after the start: those that did not verify now
	// (no CDI device for their claims) and those that were unrestored
	// already.
	Why map[state.GrantRef]error
}

// Recover is the recovery at the start of the process, before NRI and
// kubelet: the records of the store (of this boot, as the store left them
// at its load) are matched with the kernel, in this order:
//   - memdevs that are absent are GONE (their grants kept for kubelet's
//     Unprepare), noted regions are those of their name that exist on
//     their memdev (OWNED, HOLLOW_OWNED) or none, owned regions that are
//     gone are dropped;
//   - the node mounts of the realised grants of PREPARED and
//     PREPARED_UNRESTORED claims are mounted again by the identity of their
//     block device (else the grant
//     is REALISED_UNRESTORED and its preparation PREPARED_UNRESTORED), the
//     lease binds of realised companion grants bound again;
//   - the grants of Prepares that a process stop cut (JOURNALED, REALISING,
//     and the REALISED and RELEASING grants of PREPARING preparations: a
//     rollback that a stop cut; not of PREPARED_STALE ones), but those of
//     companion claims of mode numa (kept for kubelet's Unprepare, which
//     revokes, then releases, and Prepare again, which reuses) and those of
//     absent memdevs, are released per Policy (ReleaseRecovery), the
//     top-most region first across memdevs, with a holder deadline per
//     claim; a kept one makes its preparation KEPT;
//   - the node mounts and lease binds under the driver's directories that
//     no realised grant of a prepared claim lists are unmounted, the dax
//     devices of our regions that no record names deleted, our hollow
//     regions deleted when their memory is offline.
func (e *Engine) Recover(ctx context.Context) (*Recovered, error) {
	out := &Recovered{Why: map[state.GrantRef]error{}}
	snap, err := e.Observe(ctx)
	if err != nil {
		return nil, err
	}
	e.removeDaxNodeDirs()
	if err := e.recoverMemdevs(ctx, snap); err != nil {
		return nil, err
	}
	if err := e.recoverRegions(ctx, snap); err != nil {
		return nil, err
	}
	if err := e.syncForeignRegions(ctx, e.memdevRegions(snap), snap.Region, state.TriggerRecover); err != nil {
		return nil, err
	}
	if err := e.restoreGrants(ctx, out.Why); err != nil {
		return nil, err
	}
	e.restoreBinds(ctx)
	released, kept := e.releaseCut(ctx)
	if err := e.recoverPreparations(ctx, released, kept); err != nil {
		return nil, err
	}
	e.sweepBinds(ctx)
	e.sweepNodeMounts()
	e.forgetGoneSlices(ctx)
	e.deleteUnowned(ctx)
	for _, m := range e.records().Memdevs {
		if len(m.Hollow) > 0 && e.owns(m.Source) {
			if err := e.deleteHollow(ctx, m.Serial, state.TriggerRecover, "its memdev was removed before the region was destroyed"); err != nil {
				klog.Errorf("engine: %v", err)
			}
		}
	}
	e.recoverLocal(ctx)
	if err := e.commit(ctx, func(r *state.Records) error { return dropUnused(r, e.owns) }); err != nil {
		return nil, err
	}
	if e.owns(state.SourceLocal) {
		n := 0
		for _, m := range e.records().Memdevs {
			if m.Source == state.SourceLocal {
				n++
			}
		}
		klog.Infof("engine: %d local memdevs in use after the recovery", n)
	}
	return out, nil
}

// removeDaxNodeDirs removes the temporary directories of the private dax
// device nodes of the zeroing and the probe that a stopped process left.
func (e *Engine) removeDaxNodeDirs() {
	removed, err := e.k.RemoveDaxNodeDirs()
	for _, dir := range removed {
		klog.Infof("engine: removed %s, left by a zeroing or an address line probe that did not finish", dir)
	}
	if err != nil {
		klog.Errorf("engine: %v", err)
	}
}

// dropUnused removes the memdev records of which nothing is left (NONE):
// no grant, no region of ours, no stale lease bind or node mount.
func dropUnused(r *state.Records, owns func(state.Source) bool) error {
	for i := range r.Memdevs {
		m := &r.Memdevs[i]
		if !owns(m.Source) || !unused(m) || m.State == state.MemdevIdleKept {
			continue
		}
		if err := fsm.Apply(&state.MemdevTable, &m.State, state.MemdevNone, state.TriggerRecover); err != nil {
			return err
		}
	}
	removeRecord(&r.Memdevs, func(m *state.Memdev) bool { return m.State == state.MemdevNone })
	return nil
}

// numaCompanion tells whether a grant is one of a companion claim of mode
// numa.
func numaCompanion(g *state.Grant) bool {
	return g.Intent.Lend != nil && g.Asked.Mode == state.ModeNUMA
}

// cutGrant tells whether a grant is one of a Prepare that a process stop
// cut: JOURNALED or REALISING; REALISED of a claim whose Prepare did not
// finish (PREPARING, or KEPT: an earlier start kept it as it was); or
// RELEASING of a PREPARING claim (a rollback that the stop cut). A
// RELEASING grant of a KEPT claim is one of its cut Unprepare, which
// kubelet's retry finishes.
func cutGrant(r *state.Records, g *state.Grant) bool {
	p := r.PreparationOf(g.Ref)
	if p != nil && p.State == state.PreparationStale {
		// nothing of another boot is left: released by its Unprepare
		return false
	}
	switch g.State {
	case state.GrantJournaled, state.GrantRealising:
		return true
	case state.GrantRealised:
		return r.PrepareUnfinished(g)
	case state.GrantReleasing:
		return p != nil && p.State == state.PreparationPreparing
	}
	return false
}

// recoverMemdevs makes the memdev records of memdevs that are absent GONE,
// and those of memdevs that are back ACTIVE.
func (e *Engine) recoverMemdevs(ctx context.Context, snap *kernel.Snapshot) error {
	var gone, back []state.Serial
	for _, m := range e.records().Memdevs {
		if !e.owns(m.Source) {
			continue
		}
		present := snap.Memdev(uint64(m.Serial)).State != kernel.MemdevAbsent
		switch {
		case !present && m.State != state.MemdevGone:
			gone = append(gone, m.Serial)
		case present && m.State == state.MemdevGone && len(m.Grants) > 0:
			back = append(back, m.Serial)
		}
	}
	if len(gone)+len(back) == 0 {
		return nil
	}
	if err := e.commit(ctx, func(r *state.Records) error { return memdevsSeen(r, gone, back) }); err != nil {
		return err
	}
	for _, serial := range gone {
		if m := e.records().Memdev(serial); m != nil {
			klog.Warningf("engine: %s is absent: its records are kept for kubelet's Unprepare of its claims",
				memdevText(m.Source, serial))
		}
	}
	return nil
}

// memdevsSeen records the memdevs of the serials gone GONE and those of back
// ACTIVE again.
func memdevsSeen(r *state.Records, gone, back []state.Serial) error {
	for _, serial := range gone {
		if m := r.Memdev(serial); m != nil {
			if err := fsm.Apply(&state.MemdevTable, &m.State, state.MemdevGone, state.TriggerRecover); err != nil {
				return err
			}
		}
	}
	for _, serial := range back {
		if m := r.Memdev(serial); m != nil {
			if err := fsm.Apply(&state.MemdevTable, &m.State, state.MemdevActive, state.TriggerRecover); err != nil {
				return err
			}
		}
	}
	return nil
}

// recoverRegions matches the region records with the kernel: a noted region
// of that name on its memdev is OWNED (HOLLOW_OWNED when hollow), else the
// note is cleared; an OWNED or KEPT region that is gone is dropped (its
// grants keep the rest for kubelet), one whose memdev is gone is
// HOLLOW_OWNED.
func (e *Engine) recoverRegions(ctx context.Context, snap *kernel.Snapshot) error {
	seen := map[state.RegionRef]regionSeen{}
	r := e.records()
	for _, reg := range r.Regions {
		if !reg.Ours() || !e.ownsRegion(r, &reg) {
			continue
		}
		ri := snap.Region(string(reg.Ref()))
		exists := ri.State != kernel.RegionAbsent
		seen[reg.Ref()] = regionSeen{
			exists: exists,
			mine:   exists && len(reg.Targets) > 0 && slices.Contains(ri.Serials, uint64(reg.Targets[0])),
			hollow: ri.State == kernel.RegionHollow,
			info:   ri,
		}
	}
	var dropped []state.RegionRef
	if err := e.commit(ctx, func(r *state.Records) error {
		var err error
		dropped, err = regionsSeen(r, seen)
		return err
	}); err != nil {
		return err
	}
	for _, ref := range dropped {
		klog.Warningf("engine: %s is gone: its record is dropped, its grants kept for kubelet", ref)
	}
	return nil
}

// regionSeen is what the kernel has of a region record at a start.
type regionSeen struct {
	exists, mine, hollow bool
	info                 kernel.RegionInfo
}

// regionsSeen matches the region records with what the kernel has of them
// (seen) and returns the refs of the OWNED or KEPT records that it dropped
// because their region is gone.
func regionsSeen(r *state.Records, seen map[state.RegionRef]regionSeen) ([]state.RegionRef, error) {
	var dropped []state.RegionRef
	for _, ref := range regionRefs(r) {
		reg := r.Region(ref)
		k, ok := seen[ref]
		if !ok {
			continue
		}
		switch reg.State {
		case state.RegionNoted:
			if err := recoverNoted(r, reg, k); err != nil {
				return nil, err
			}
		case state.RegionOwned, state.RegionKept:
			switch {
			case k.hollow:
				if err := hollowRecovered(r, ref); err != nil {
					return nil, err
				}
			case !k.exists || !k.mine:
				if err := dropRegion(r, ref, state.TriggerRecover); err != nil {
					return nil, err
				}
				dropped = append(dropped, ref)
			}
		}
	}
	return dropped, nil
}

// regionRefs returns the refs of the region records.
func regionRefs(r *state.Records) []state.RegionRef {
	var out []state.RegionRef
	for i := range r.Regions {
		out = append(out, r.Regions[i].Ref())
	}
	return out
}

// recoverNoted resolves a NOTED region at a start: the region
// of its name exists on its memdev (OWNED), hollow (HOLLOW_OWNED), or not
// (the note cleared).
func recoverNoted(r *state.Records, reg *state.Region, k regionSeen) error {
	ref := reg.Ref()
	switch {
	case k.hollow:
		if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionHollowOwned, state.TriggerRecover); err != nil {
			return err
		}
		reg.Name = string(ref)
		for _, serial := range reg.Targets {
			if m := r.Memdev(serial); m != nil {
				m.Regions = removeRef(m.Regions, ref)
				m.Hollow = addRef(m.Hollow, ref)
			}
		}
	case k.mine:
		if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionOwned, state.TriggerRecover); err != nil {
			return err
		}
		reg.Name, reg.NotedName = string(ref), ""
		reg.NUMANode = state.NUMANodePtr(k.info.NUMANode)
		reg.Size = k.info.Range.Size()
	default:
		if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionNone, state.TriggerRecover); err != nil {
			return err
		}
		removeRecord(&r.Regions, func(x *state.Region) bool { return x.Ref() == ref })
		for i := range r.Memdevs {
			r.Memdevs[i].Regions = removeRef(r.Memdevs[i].Regions, ref)
		}
		for i := range r.Grants {
			if r.Grants[i].Intent.Region == ref {
				r.Grants[i].Intent.Region = ""
			}
		}
	}
	return nil
}

// hollowRecovered records a region of ours that a start finds hollow.
func hollowRecovered(r *state.Records, ref state.RegionRef) error {
	reg := r.Region(ref)
	if err := fsm.Apply(&state.RegionTable, &reg.State, state.RegionHollowOwned, state.TriggerRecover); err != nil {
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

// restoreBinds binds again the lease binds of the realised companion
// grants (restoreBind).
func (e *Engine) restoreBinds(ctx context.Context) {
	r := e.records()
	for i := range r.Grants {
		e.restoreBind(ctx, r, &r.Grants[i], state.TriggerRecover)
	}
}

// restoreBind binds again the lease bind of a realised companion grant of
// a prepared claim whose node mount is mounted, but not of a claim being
// unprepared: kubelet unprepares it again. Errors are logged.
func (e *Engine) restoreBind(ctx context.Context, r *state.Records, g *state.Grant, trigger state.Trigger) {
	b := g.Got.Bind
	p := r.PreparationOf(g.Ref)
	if b == nil || b.State != state.BindBound || g.State != state.GrantRealised || p == nil || !p.Prepared() ||
		p.State == state.PreparationUnpreparing || !e.owns(g.Asked.Source) {
		return
	}
	u := pmemGrantOf(r, g)
	mi, err := e.k.Mounted(b.Target)
	if err != nil || mi != nil {
		return
	}
	claim, what := e.claimNameOf(g.Ref), bindWhat(g)
	if node, err := e.k.Mounted(u.nodeMount); err != nil || node == nil {
		klog.Errorf("engine: claim %s: %s: not binding %s again: %s is not mounted (%v)", claim, what, b.Target, u.nodeMount, err)
		return
	}
	ref := g.Ref
	if _, err := e.bindLease(&u, b.Target, func(t string) bool { return e.bindUsed(t, ref) }); err != nil {
		klog.Errorf("engine: claim %s: %s: not binding %s again: %v", claim, what, b.Target, err)
		return
	}
	klog.Infof("engine: claim %s: %s: bound %s at %s again", claim, what, u.nodeMount, b.Target)
	if err := e.commit(ctx, func(r *state.Records) error {
		rb := r.Grant(ref).Got.Bind
		return fsm.Apply(&state.BindTable, &rb.State, state.BindBound, trigger)
	}); err != nil {
		klog.Errorf("engine: %v", err)
	}
}

// releaseCut releases the objects of the grants of cut Prepares per Policy
// (ReleaseRecovery), but those of companion claims of mode numa and of
// memdevs that are gone, the memdev whose regions are top-most first. The
// log lines name each claim: when its first memdev is released or kept,
// and when its last one is. Errors are logged; a grant that is not
// released is kept. It returns the grants that it released and those that
// it kept.
func (e *Engine) releaseCut(ctx context.Context) (released, kept []state.GrantRef) {
	rctx, cancel := e.rollbackContext(ctx)
	defer cancel()
	cut := e.findCutClaims()
	if len(cut.bySerial) == 0 {
		return nil, nil
	}
	call := newReleaseCall(e, ReleaseRecovery, nil)
	call.perClaim = true
	for len(cut.bySerial) > 0 {
		serial := e.topMost(cut.bySerial)
		refs := cut.bySerial[serial]
		delete(cut.bySerial, serial)
		for _, ref := range refs {
			call.refs[ref] = true
		}
		if above := e.recoveryAbove(serial, refs); len(above) > 0 {
			for _, ref := range refs {
				cut.keep(e, ref, above)
			}
			if err := e.keepForRecovery(rctx, serial, refs); err != nil {
				klog.Errorf("engine: claim %s: %v", e.claimNameOf(refs[0]), err)
			}
			kept = append(kept, refs...)
			continue
		}
		cut.announce(e, serial, refs)
		err := e.releaseGroup(rctx, serial, refs, call)
		for _, ref := range refs {
			if e.records().Grant(ref) == nil {
				released = append(released, ref)
				continue
			}
			kept = append(kept, ref)
			cut.failed(ref, err)
		}
		for _, ref := range refs {
			cut.done(e, ref)
		}
	}
	return released, kept
}

// cutClaims are the claims of cut Prepares that the recovery releases:
// their grants by memdev, and per claim its grants that are left, the
// regions above those kept and the errors of their release.
type cutClaims struct {
	bySerial map[state.Serial][]state.GrantRef
	left     map[string][]state.GrantRef
	above    map[string][]string
	errs     map[string][]error
}

// findCutClaims returns the cut claims that the recovery releases; the grants
// of numa companions stay (logged), those of memdevs that are gone stay for
// kubelet's Unprepare.
func (e *Engine) findCutClaims() *cutClaims {
	c := &cutClaims{bySerial: map[state.Serial][]state.GrantRef{}, left: map[string][]state.GrantRef{},
		above: map[string][]string{}, errs: map[string][]error{}}
	r := e.records()
	companions := map[string]bool{}
	for i := range r.Grants {
		g := &r.Grants[i]
		if !cutGrant(r, g) || !e.owns(g.Asked.Source) {
			continue
		}
		switch m := r.Memdev(g.Serial); {
		case numaCompanion(g):
			if !companions[g.Ref.Claim] {
				companions[g.Ref.Claim] = true
				klog.Infof("engine: claim %s (%s) is a companion claim: its Unprepare releases its devices, its Prepare again reuses them",
					e.claimNameOf(g.Ref), g.Ref.Claim)
			}
		case m != nil && m.State == state.MemdevGone:
		default:
			c.bySerial[g.Serial] = append(c.bySerial[g.Serial], g.Ref)
			c.left[g.Ref.Claim] = append(c.left[g.Ref.Claim], g.Ref)
		}
	}
	return c
}

// keep records that the grant ref is kept below the regions above.
func (c *cutClaims) keep(e *Engine, ref state.GrantRef, above []string) {
	for _, a := range above {
		if !slices.Contains(c.above[ref.Claim], a) {
			c.above[ref.Claim] = append(c.above[ref.Claim], a)
		}
	}
	c.done(e, ref)
}

// failed records the error of the release of the grant ref.
func (c *cutClaims) failed(ref state.GrantRef, err error) {
	if err == nil {
		err = fmt.Errorf("grant %s is not released", ref)
	}
	if !slices.ContainsFunc(c.errs[ref.Claim], func(x error) bool { return x == err }) {
		c.errs[ref.Claim] = append(c.errs[ref.Claim], err)
	}
}

// announce logs the release of the devices of the memdev with the serial
// for each claim of the grants refs of it.
func (c *cutClaims) announce(e *Engine, serial state.Serial, refs []state.GrantRef) {
	var claims []state.GrantRef
	for _, ref := range refs {
		if !slices.ContainsFunc(claims, func(o state.GrantRef) bool { return o.Claim == ref.Claim }) {
			claims = append(claims, ref)
		}
	}
	for _, ref := range claims {
		klog.Infof("engine: claim %s (%s), whose Prepare did not finish: releasing devices %s", e.claimNameOf(ref), ref.Claim,
			serial.String())
	}
}

// done logs what the recovery did with the claim of ref once ref is its
// last grant that is left.
func (c *cutClaims) done(e *Engine, ref state.GrantRef) {
	claim := ref.Claim
	c.left[claim] = removeRef(c.left[claim], ref)
	if len(c.left[claim]) > 0 {
		return
	}
	delete(c.left, claim)
	name := e.claimNameOf(ref)
	if above := c.above[claim]; len(above) > 0 {
		klog.Warningf("engine: claim %s (%s): not releasing its devices at start: %s lie above their regions, a release would leave a hole in the decoders of their ports; its Unprepare releases them, its Prepare again reuses them",
			name, claim, strings.Join(above, ", "))
	}
	switch {
	case len(c.errs[claim]) > 0:
		klog.Warningf("engine: claim %s (%s): not everything was released at start (%v): keeping its record in the store: its Unprepare releases its devices, its Prepare again reuses them",
			name, claim, errors.Join(c.errs[claim]...))
	case len(c.above[claim]) == 0:
		klog.Infof("engine: claim %s (%s): released, its Prepare again prepares its devices anew", name, claim)
	}
}

// ownsRegion tells whether the engine owns the record of a region: the
// source of the record of one of its memdevs, else of a grant that got it.
func (e *Engine) ownsRegion(r *state.Records, reg *state.Region) bool {
	if e.opts.Sources == nil {
		return true
	}
	for _, serial := range reg.Targets {
		if m := r.Memdev(serial); m != nil {
			return e.owns(m.Source)
		}
	}
	for i := range r.Grants {
		if r.Grants[i].Got.Region == reg.Ref() {
			return e.owns(r.Grants[i].Asked.Source)
		}
	}
	return false
}

// bindOrigin returns the serial of the node mount in a mounts directory
// of an owned source that a lease bind of the device source binds, the
// serial by the name of the node mount.
func (e *Engine) bindOrigin(mounts []kernel.MountInfo, source string) (state.Serial, bool) {
	dirs := map[string]bool{}
	for s, d := range e.opts.MountsDirs {
		if e.owns(s) {
			dirs[d] = true
		}
	}
	for i := range mounts {
		m := &mounts[i]
		if m.Source != source || !dirs[filepath.Dir(m.Target)] {
			continue
		}
		hex, _, _ := strings.Cut(filepath.Base(m.Target), "-")
		if serial, err := strconv.ParseUint(hex, 16, 64); err == nil {
			return state.Serial(serial), true
		}
	}
	return 0, false
}

// topMost returns the serial of the grants whose memdev has no region of
// another memdev of the grants above its regions, the lowest serial of
// those (or of all, when every one has).
func (e *Engine) topMost(bySerial map[state.Serial][]state.GrantRef) state.Serial {
	serials := slices.Sorted(maps.Keys(bySerial))
	for _, serial := range serials {
		memdev, err := e.k.FindMemdev(uint64(serial))
		if err != nil {
			return serial
		}
		above, err := e.k.RegionsAbove(memdev)
		if err != nil {
			continue
		}
		blocked := false
		for _, name := range above {
			if ri, err := e.k.Region(name); err == nil {
				for _, s := range ri.Serials {
					_, cut := bySerial[state.Serial(s)]
					blocked = blocked || cut
				}
			}
		}
		if !blocked {
			return serial
		}
	}
	return serials[0]
}

// recoverPreparations makes a PREPARING preparation with a grant that the
// recovery kept KEPT, with the results of its grants that are left; the
// others stay PREPARING with their results for kubelet's retry. A KEPT
// preparation of an earlier start loses the results of the grants that the
// recovery released, and goes when none is left (keptReleased).
func (e *Engine) recoverPreparations(ctx context.Context, released, kept []state.GrantRef) error {
	if len(kept) == 0 && len(released) == 0 && !keptWithoutGrant(e.records()) {
		return nil
	}
	return e.commit(ctx, func(r *state.Records) error {
		if err := keptReleased(r, released); err != nil {
			return err
		}
		return preparingRecovered(r, released, kept)
	})
}

// preparingRecovered makes a PREPARING preparation with a grant that the
// recovery kept KEPT, with the results of its grants that are left, and
// one with a grant that it released PREPARING again.
func preparingRecovered(r *state.Records, released, kept []state.GrantRef) error {
	for i := range r.Preparations {
		p := &r.Preparations[i]
		if p.State != state.PreparationPreparing {
			continue
		}
		isKept, touched := false, false
		for _, res := range p.Results {
			if res.Grant == nil {
				continue
			}
			isKept = isKept || slices.Contains(kept, *res.Grant)
			touched = touched || isKept || slices.Contains(released, *res.Grant)
		}
		switch {
		case isKept:
			if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationKept, state.TriggerRecover); err != nil {
				return err
			}
			p.Results = slices.DeleteFunc(p.Results, func(res state.Result) bool {
				return res.Grant != nil && r.Grant(*res.Grant) == nil
			})
		case touched:
			if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationPreparing, state.TriggerRecover); err != nil {
				return err
			}
		}
	}
	return nil
}

// keptReleased drops from the KEPT preparations the results of the grants
// released (the recovery released them: nothing lies above their regions
// any more, or their idle shared memdev keeps its region) or gone (a cut
// Unprepare released them), and removes a KEPT preparation that has no
// grant left: its Prepare again prepares the claim anew, its Unprepare
// finds nothing.
func keptReleased(r *state.Records, released []state.GrantRef) error {
	type key struct{ uid, driver string }
	var gone []key
	for i := range r.Preparations {
		p := &r.Preparations[i]
		if p.State != state.PreparationKept || !slices.ContainsFunc(p.Results, func(res state.Result) bool {
			return res.Grant != nil && (slices.Contains(released, *res.Grant) || r.Grant(*res.Grant) == nil)
		}) {
			continue
		}
		p.Results = slices.DeleteFunc(p.Results, func(res state.Result) bool {
			return res.Grant != nil && r.Grant(*res.Grant) == nil
		})
		if slices.ContainsFunc(p.Results, func(res state.Result) bool { return res.Grant != nil }) {
			if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationKept, state.TriggerRecover); err != nil {
				return err
			}
			continue
		}
		if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationNone, state.TriggerRecover); err != nil {
			return err
		}
		gone = append(gone, key{p.UID, p.Driver})
	}
	for _, k := range gone {
		removeRecord(&r.Preparations, func(x *state.Preparation) bool { return x.UID == k.uid && x.Driver == k.driver })
	}
	return nil
}

// keptWithoutGrant tells whether a KEPT preparation has a result whose grant
// has no record: an Unprepare that a stop cut after it released the grant.
func keptWithoutGrant(r *state.Records) bool {
	for i := range r.Preparations {
		p := &r.Preparations[i]
		if p.State == state.PreparationKept && slices.ContainsFunc(p.Results, func(res state.Result) bool {
			return res.Grant != nil && r.Grant(*res.Grant) == nil
		}) {
			return true
		}
	}
	return false
}

// listedMounts returns the node mounts and lease binds that the realised
// grants of prepared claims list, unrestored ones included (a mount there
// is not ours, or none is there), and those that a grant whose release in
// an Unprepare failed still has.
func (e *Engine) listedMounts() (mounts, binds map[string]bool) {
	r := e.records()
	mounts, binds = map[string]bool{}, map[string]bool{}
	for i := range r.Grants {
		g := &r.Grants[i]
		p := r.PreparationOf(g.Ref)
		if (!prepared(g) || p == nil || !p.Prepared()) && !unpreparing(r, g) {
			continue
		}
		if path := mountPathOf(r, g); path != "" {
			mounts[path] = true
		}
		if g.Got.Bind != nil {
			binds[g.Got.Bind.Target] = true
		}
	}
	return mounts, binds
}

// sweepNodeMounts unmounts the mounts in the mounts directories that no
// realised grant of a prepared claim lists, and removes their directories.
// Errors are logged.
func (e *Engine) sweepNodeMounts() {
	listed, _ := e.listedMounts()
	for _, source := range slices.Sorted(maps.Keys(e.opts.MountsDirs)) {
		if !e.owns(source) {
			continue
		}
		dir := e.opts.MountsDirs[source]
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			klog.Errorf("engine: %v", err)
		}
		for _, ent := range entries {
			mnt := filepath.Join(dir, ent.Name())
			if listed[mnt] {
				continue
			}
			klog.Infof("engine: %s belongs to no prepared claim, unmounting it", mnt)
			if mi, err := e.k.Mounted(mnt); err != nil || mi != nil {
				if err == nil {
					err = e.locked(func() error { return e.k.Unmount(mnt) })
				}
				if err != nil {
					klog.Errorf("engine: unmounting %s: %v", mnt, err)
					continue
				}
			}
			e.removeMountDir(mnt)
		}
	}
}

// sweepBinds unmounts, topmost first, the mounts under the lease-fs root
// that no realised grant of a prepared claim lists and that bind a node
// mount of an owned source (their source is that of a mount in its mounts
// directory), or any such mount when the engine owns every source, and
// forgets those that a memdev had as stale. One that cannot be unmounted is
// logged and recorded stale with the memdev of the node mount that it
// binds, if that is known.
func (e *Engine) sweepBinds(ctx context.Context) {
	if e.opts.LeaseFsRoot == "" {
		return
	}
	_, listed := e.listedMounts()
	mounts, err := e.k.Mounts()
	if err != nil {
		klog.Errorf("engine: %v", err)
		return
	}
	for i := len(mounts) - 1; i >= 0; i-- {
		mi := &mounts[i]
		if !strings.HasPrefix(mi.Target, e.opts.LeaseFsRoot+"/") || listed[mi.Target] {
			continue
		}
		serial, ok := e.bindOrigin(mounts, mi.Source)
		if !ok && !e.ownsAll() {
			// its node mount is gone: the bind of no memdev, which
			// only the engine of all sources sweeps
			continue
		}
		klog.Infof("engine: %s belongs to no prepared claim, unmounting it", mi.Target)
		target := mi.Target
		if err := e.locked(func() error { return e.k.Unmount(target) }); err != nil {
			klog.Errorf("engine: unmounting %s: %v; trying again at the next release", target, err)
			if ok {
				e.addStale(serial, target)
			} else {
				e.addUnownedBind(target)
			}
			continue
		}
		e.removeMountDir(target)
		e.forgetSwept(ctx, serial, target)
	}
}

// forgetSwept forgets a lease bind that the sweep unmounted when the memdev
// with the serial had it as stale.
func (e *Engine) forgetSwept(ctx context.Context, serial state.Serial, target string) {
	if m := e.records().Memdev(serial); m == nil || !slices.Contains(m.StaleBinds, target) {
		return
	}
	if err := e.commit(ctx, func(r *state.Records) error {
		return forgetStale(r, serial, []string{target}, state.TriggerRecover)
	}); err != nil {
		klog.Errorf("engine: memdev %s: forgetting stale %s: %v", serial, target, err)
	}
}

// deleteUnowned deletes the dax devices of our regions that no record
// names or that have no user and are not the Whole device (their memory
// offlined first). Errors are logged.
func (e *Engine) deleteUnowned(ctx context.Context) {
	r := e.records()
	for i := range r.Regions {
		reg := &r.Regions[i]
		if !reg.Ours() || reg.State != state.RegionOwned && reg.State != state.RegionKept || !e.ownsRegion(r, reg) {
			continue
		}
		name := string(reg.Ref())
		for _, dax := range e.k.DaxDevices(name) {
			d := reg.DaxDevice(dax)
			if d != nil && (d.Whole || len(d.Users) > 0 || d.State == state.DaxSeed) {
				continue
			}
			if d == nil && strings.HasSuffix(dax, ".0") {
				continue
			}
			if err := e.deleteDax(ctx, reg, dax); err != nil {
				klog.Errorf("engine: %s: %v", name, err)
			}
		}
	}
}

// deleteDax offlines the memory of a dax device of a region of ours that
// no grant uses, deletes it and drops its record.
func (e *Engine) deleteDax(ctx context.Context, reg *state.Region, dax string) error {
	region := reg.Ref()
	var serial state.Serial
	if len(reg.Targets) > 0 {
		serial = reg.Targets[0]
	}
	m := e.records().Memdev(serial)
	local := m != nil && m.Source == state.SourceLocal
	if local {
		mode := state.ModeDax
		if e.k.DaxDriver(dax) == kernel.DriverKmem {
			mode = state.ModeNUMA
		}
		size, _ := e.k.DaxDeviceSize(dax)
		klog.Warningf("engine: local memdev %s (%s): %s (mode %s, %d bytes) belongs to no claim, deleting it",
			serial.String(), e.memdevName(serial), dax, mode, size)
	}
	if _, err := e.offlineDax(ctx, dax); err != nil {
		return fmt.Errorf("deleting %s: %w", dax, err)
	}
	if err := e.locked(func() error { return e.k.DeleteDaxDevice(ctx, string(region), dax) }); err != nil {
		return err
	}
	if !local {
		klog.Infof("engine: %s: deleted %s, which belongs to no grant", region, dax)
	}
	return e.commit(ctx, func(r *state.Records) error {
		reg := r.Region(region)
		if reg == nil {
			return nil
		}
		d := reg.DaxDevice(dax)
		if d == nil {
			return nil
		}
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, state.DaxNone, state.TriggerRecover); err != nil {
			return err
		}
		removeRecord(&reg.DaxDevices, func(x *state.DaxDevice) bool { return x.Name == dax })
		return nil
	})
}
