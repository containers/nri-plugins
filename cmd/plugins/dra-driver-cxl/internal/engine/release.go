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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// ReleaseGrants releases the grants refs as release says (Policy) and
// returns the errors of those that it could not release. First the stale
// lease binds of every memdev are unmounted again (retryAllStale). The
// grants of one memdev are released together in its queue (what the
// other grants of the call use is not kept), in the order of the first
// of them; every holder wait of the call ends at one deadline, which
// starts at its first holder wait. A grant that is released is gone from
// the store; one whose release failed stays RELEASING with what it still
// has, and a retry finds out from the kernel what exists. A ref without a
// grant is not an error. A grant of a PREPARED_STALE preparation is
// released by its serial: a pool memdev that no grant of this boot uses is
// disabled.
func (e *Engine) ReleaseGrants(ctx context.Context, refs []state.GrantRef, release Release) error {
	ctx, end := e.Request(ctx)
	defer end()
	r := e.records()
	for _, ref := range refs {
		if g := r.Grant(ref); g != nil && !e.owns(g.Asked.Source) {
			return fmt.Errorf("engine: grant %s: the records of source %q are not the engine's", ref, g.Asked.Source)
		}
	}
	e.retryAllStale(ctx, triggerOf(release))
	refs = localOrder(r, refs)
	call := newReleaseCall(e, release, refs)
	var errs []error
	for _, group := range e.byMemdev(refs) {
		if err := e.releaseGroup(ctx, group.serial, group.refs, call); err != nil {
			errs = append(errs, err)
		}
	}
	e.verify(ctx, nil, triggerOf(release))
	return errors.Join(errs...)
}

// localOrder returns refs with the grants of local memdevs in the order of
// their release: by the serial of their memdev, the last result of a claim
// on a memdev first; the others where they are.
func localOrder(r *state.Records, refs []state.GrantRef) []state.GrantRef {
	local := func(ref state.GrantRef) *state.Grant {
		if g := r.Grant(ref); g != nil && g.Asked.Source == state.SourceLocal {
			return g
		}
		return nil
	}
	var idx []int
	var grants []*state.Grant
	for i, ref := range refs {
		if g := local(ref); g != nil {
			idx = append(idx, i)
			grants = append(grants, g)
		}
	}
	slices.SortStableFunc(grants, func(a, b *state.Grant) int {
		if a.Serial != b.Serial {
			return cmp.Compare(a.Serial, b.Serial)
		}
		return cmp.Compare(b.Ref.Result, a.Ref.Result)
	})
	out := slices.Clone(refs)
	for i, j := range idx {
		out[j] = grants[i].Ref
	}
	return out
}

// releaseCall is what the release of the grants of one call shares: its
// policy and trigger, its grants, the deadlines of its holder waits, and
// the results of the release of the block devices and node mounts of its
// grants per memdev.
type releaseCall struct {
	release Release
	trigger state.Trigger
	refs    map[state.GrantRef]bool
	// holder is the holder wait; perClaim makes a deadline per claim (the
	// recovery's), else the call has one.
	holder    time.Duration
	perClaim  bool
	deadlines map[string]time.Time
	released  map[state.Serial]error
	// unrestored are the node mounts of the grants of the call that the
	// start did not mount again (UNRESTORED, FOREIGN), by grant.
	unrestored map[state.GrantRef]string
	// reasons are the Reasons of the grants of the call that are released
	// before they were realised, by grant.
	reasons map[state.GrantRef]string
}

// newReleaseCall returns the release call of the grants refs.
func newReleaseCall(e *Engine, release Release, refs []state.GrantRef) *releaseCall {
	c := &releaseCall{
		release:    release,
		trigger:    triggerOf(release),
		refs:       map[state.GrantRef]bool{},
		holder:     e.opts.Timeouts.Holder,
		deadlines:  map[string]time.Time{},
		released:   map[state.Serial]error{},
		unrestored: map[state.GrantRef]string{},
		reasons:    map[state.GrantRef]string{},
	}
	for _, ref := range refs {
		c.refs[ref] = true
	}
	return c
}

// holderDeadline returns the deadline of the holder waits of the call for a
// grant of the claim: the holder wait from the first holder wait of the
// call, or of the claim when the call has a deadline per claim.
func (c *releaseCall) holderDeadline(claim string) time.Time {
	if !c.perClaim {
		claim = ""
	}
	d, ok := c.deadlines[claim]
	if !ok {
		d = time.Now().Add(c.holder)
		c.deadlines[claim] = d
	}
	return d
}

// releaseError is the error of the release of a memdev: held tells whether
// its block device stayed held (errHolder).
type releaseError struct {
	serial state.Serial
	held   bool
	err    error
}

// Error returns the error in words.
func (e *releaseError) Error() string {
	return fmt.Sprintf("releasing serial %s: %v", e.serial.String(), e.err)
}

// Unwrap returns the cause.
func (e *releaseError) Unwrap() error {
	return e.err
}

// Is tells whether target is errHolder and the block device stayed held.
func (e *releaseError) Is(target error) bool {
	return target == errHolder && e.held
}

// triggerOf returns the trigger of the transitions of a release.
func triggerOf(release Release) state.Trigger {
	switch release {
	case ReleaseUnprepare:
		return state.TriggerUnprepare
	case ReleaseRollback:
		return state.TriggerPrepare
	}
	return state.TriggerRecover
}

// memdevGroup is the refs of grants of the memdev with the serial.
type memdevGroup struct {
	serial state.Serial
	refs   []state.GrantRef
}

// byMemdev groups the refs that have a grant by the memdev of their grants,
// in the order of the first ref of each memdev.
func (e *Engine) byMemdev(refs []state.GrantRef) []memdevGroup {
	r := e.records()
	var order []state.Serial
	groups := map[state.Serial][]state.GrantRef{}
	for _, ref := range refs {
		g := r.Grant(ref)
		if g == nil || slices.Contains(groups[g.Serial], ref) {
			continue
		}
		if _, ok := groups[g.Serial]; !ok {
			order = append(order, g.Serial)
		}
		groups[g.Serial] = append(groups[g.Serial], ref)
	}
	out := make([]memdevGroup, 0, len(order))
	for _, serial := range order {
		out = append(out, memdevGroup{serial: serial, refs: groups[serial]})
	}
	return out
}

// releaseGroup releases grants of the memdev with the serial, its queue
// held once: they are RELEASING together before the first of them is
// released. A ref whose grant another release took meanwhile is skipped.
func (e *Engine) releaseGroup(ctx context.Context, serial state.Serial, refs []state.GrantRef, call *releaseCall) error {
	leave, err := e.acquire(ctx, serial)
	if err != nil {
		return err
	}
	defer leave()
	if err := e.startReleases(ctx, refs, call); err != nil {
		return err
	}
	var errs []error
	for _, ref := range refs {
		if err := e.releaseGrant(ctx, ref, call); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseGrant releases a grant, its memdev's queue held: RELEASING (unless
// it is), its lease bind unbound, its node mount and block device released
// next to the other grants of the memdev, its own dax device deleted, the
// region per Policy when it was the last grant of the memdev, then
// NONE. A grant of a cut Prepare (JOURNALED, REALISING) that the
// recovery releases goes to NONE directly. A region that the policy
// keeps for the claim (a rollback below a newer region of another memdev)
// is kept with the grant JOURNALED; the recovery decides what it keeps
// before (keepForRecovery).
func (e *Engine) releaseGrant(ctx context.Context, ref state.GrantRef, call *releaseCall) error {
	r := e.records()
	g := r.Grant(ref)
	if g == nil {
		return nil
	}
	if p := r.PreparationOf(ref); p != nil && p.State == state.PreparationStale {
		return e.releaseStale(ctx, ref, call.trigger)
	}
	if g.State != state.GrantReleasing {
		if err := e.startReleases(ctx, []state.GrantRef{ref}, call); err != nil {
			return err
		}
	}
	cur := e.records().Grant(ref).Clone()
	call.noteUnrestored(e.records(), &cur)
	if err := e.undoGrant(ctx, &cur, call); err != nil {
		return err
	}
	return e.releaseLast(ctx, &cur, call)
}

// noteUnrestored remembers the node mount of a grant that the start did not
// mount again (UNRESTORED, FOREIGN).
func (c *releaseCall) noteUnrestored(r *state.Records, g *state.Grant) {
	if m := r.MountOf(g); m != nil && (m.State == state.MountUnrestored || m.State == state.MountForeign) {
		c.unrestored[g.Ref] = m.Path
	}
}

// releaseLast releases what the memdev of a grant whose own objects are
// released has when the grant is its last one of no other call, and
// records the grant NONE, or JOURNALED with the region that the policy
// keeps for its claim.
func (e *Engine) releaseLast(ctx context.Context, cur *state.Grant, call *releaseCall) error {
	ref, trigger := cur.Ref, call.trigger
	others, ofCall := e.memdevUsers(cur.Serial, ref, call)
	e.logReleased(cur, call.release)
	if others && !ofCall && cur.Asked.Source == state.SourcePool {
		klog.Infof("engine: %s is still used by another claim", memdevText(cur.Asked.Source, cur.Serial))
	}
	if !others && !ofCall {
		kept, err := e.applyPolicy(ctx, cur, call)
		if err != nil {
			return err
		}
		if kept {
			return e.keptForClaim(ctx, ref, trigger)
		}
	}
	return e.commit(ctx, func(r *state.Records) error { return releasedGrant(r, ref, trigger) })
}

// startReleases commits the grants refs RELEASING in one commit, but those
// that are released otherwise: of PREPARED_STALE preparations (by their
// serial), and of cut Prepares (JOURNALED, REALISING) that the recovery
// releases (NONE directly).
func (e *Engine) startReleases(ctx context.Context, refs []state.GrantRef, call *releaseCall) error {
	r := e.records()
	var start []state.GrantRef
	for _, ref := range refs {
		g := r.Grant(ref)
		p := r.PreparationOf(ref)
		switch {
		case g == nil, p != nil && p.State == state.PreparationStale:
		case call.release == ReleaseRecovery && (g.State == state.GrantJournaled || g.State == state.GrantRealising):
		default:
			start = append(start, ref)
		}
	}
	if len(start) == 0 {
		return nil
	}
	return e.commit(ctx, func(r *state.Records) error {
		for _, ref := range start {
			if g := r.Grant(ref); g != nil {
				realised := g.State != state.GrantJournaled && g.State != state.GrantRealising
				if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantReleasing, call.trigger); err != nil {
					return err
				}
				if !realised {
					g.Reason = call.reasons[ref]
				}
			}
		}
		return nil
	})
}

// memdevUsers tells whether the memdev with the serial has grants but ref
// of other calls (others) and of the release call (ofCall), but those of
// unfinished Unprepares that hold none of its regions.
func (e *Engine) memdevUsers(serial state.Serial, ref state.GrantRef, call *releaseCall) (others, ofCall bool) {
	r := e.records()
	if m := r.Memdev(serial); m != nil {
		for _, o := range m.Grants {
			if o == ref || !usesRegion(r, m, o) {
				continue
			}
			others = others || !call.refs[o]
			ofCall = ofCall || call.refs[o]
		}
	}
	return others, ofCall
}

// usesRegion tells whether the grant ref of the memdev record m may hold a
// region of it: every grant but the one of a Prepare that did not finish
// (of a PREPARING or KEPT claim) before it named a region, and the one of
// an unfinished Unprepare (RELEASING, of an UNPREPARING claim) that no dax
// device or node mount of its regions lists as a user, of mode block whose
// Got names no region (its region is gone; a new one may have its name).
func usesRegion(r *state.Records, m *state.Memdev, ref state.GrantRef) bool {
	g := r.Grant(ref)
	switch {
	case g != nil && g.Got.Region == "" && g.Intent.Region == "" && r.PrepareUnfinished(g):
		return false
	case g == nil || !unpreparing(r, g):
		return true
	case g.Asked.Mode == state.ModeBlock:
		return g.Got.Region != ""
	}
	for _, rr := range m.Regions {
		reg := r.Region(rr)
		if reg == nil {
			continue
		}
		for _, d := range reg.DaxDevices {
			if slices.Contains(d.Users, ref) {
				return true
			}
		}
		for _, mt := range reg.Mounts {
			if slices.Contains(mt.Users, ref) {
				return true
			}
		}
	}
	return false
}

// keepForRecovery keeps the cut grants refs of one memdev whose region lies
// below a newer region of another memdev untouched (Policy ReleaseRecovery
// keeps), the memdev's queue held: the grants JOURNALED and the region KEPT
// to be set up again, but a realised grant and the region that it uses
// stay as they are.
func (e *Engine) keepForRecovery(ctx context.Context, serial state.Serial, refs []state.GrantRef) error {
	leave, err := e.acquire(ctx, serial)
	if err != nil {
		return err
	}
	defer leave()
	r := e.records()
	realised := slices.ContainsFunc(refs, func(ref state.GrantRef) bool {
		g := r.Grant(ref)
		return g != nil && g.State == state.GrantRealised
	})
	if !realised {
		if err := e.keepMemdev(ctx, serial, state.TriggerRecover, true); err != nil {
			return err
		}
	}
	for _, ref := range refs {
		if err := e.keptForClaim(ctx, ref, state.TriggerRecover); err != nil {
			return err
		}
	}
	return nil
}

// undoGrant releases what a grant has of its own or shares: its lease
// bind, its node mount and block device next to the other grants of the
// memdev, its own dax device.
func (e *Engine) undoGrant(ctx context.Context, g *state.Grant, call *releaseCall) error {
	if err := e.unbindGrant(ctx, g, call.trigger); err != nil {
		return err
	}
	if err := e.leaveNodeMount(ctx, g, call); err != nil {
		return err
	}
	return e.deleteSlice(ctx, g, call.trigger)
}

// recoveryAbove returns the regions of other memdevs above the regions of
// the memdev of cut grants that the recovery keeps for their claim, none
// when it releases them: those are the last grants of the memdev, and the
// Policy (ReleaseRecovery) of their kind keeps a region below others.
func (e *Engine) recoveryAbove(serial state.Serial, refs []state.GrantRef) []string {
	r := e.records()
	m := r.Memdev(serial)
	if m == nil || slices.ContainsFunc(m.Grants, func(o state.GrantRef) bool { return !slices.Contains(refs, o) }) {
		return nil
	}
	g := r.Grant(refs[0])
	memdev, err := e.k.FindMemdev(uint64(serial))
	if err != nil || g == nil {
		return nil
	}
	if d, above := e.decision(g, memdev, ReleaseRecovery); d == Keep {
		return above
	}
	return nil
}

// hasOwnRegion tells whether the memdev record m names a region of ours.
func hasOwnRegion(r *state.Records, m *state.Memdev) bool {
	return m != nil && slices.ContainsFunc(m.Regions, func(ref state.RegionRef) bool {
		reg := r.Region(ref)
		return reg != nil && reg.Ours()
	})
}

// keptForClaim records a grant whose region is kept for its claim:
// JOURNALED again, holding the region by its intent; a realised grant of a
// cut Prepare that the recovery keeps stays as it is.
func (e *Engine) keptForClaim(ctx context.Context, ref state.GrantRef, trigger state.Trigger) error {
	return e.commit(ctx, func(r *state.Records) error {
		g := r.Grant(ref)
		if g == nil || g.State == state.GrantRealised {
			return nil
		}
		if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantJournaled, trigger); err != nil {
			return err
		}
		g.Reason = ""
		if g.Got.Region != "" {
			g.Intent.Region = g.Got.Region
		}
		if reg := r.Region(g.Got.Region); reg != nil && g.Got.Dax != "" {
			if d := reg.DaxDevice(g.Got.Dax); d != nil {
				d.Users = removeRef(d.Users, ref)
			}
		}
		g.Got = state.Got{Bind: g.Got.Bind, Lend: g.Got.Lend}
		return nil
	})
}

// releasedGrant removes a released grant (RELEASING -> NONE) from the
// records and from its memdev, and the memdev record when nothing of it is
// left (NONE).
func releasedGrant(r *state.Records, ref state.GrantRef, trigger state.Trigger) error {
	g := r.Grant(ref)
	if g == nil {
		return nil
	}
	if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantNone, trigger); err != nil {
		return err
	}
	serial := g.Serial
	if err := leaveUsers(r, ref, trigger); err != nil {
		return err
	}
	removeRecord(&r.Grants, func(x *state.Grant) bool { return x.Ref == ref })
	m := r.Memdev(serial)
	if m == nil {
		return nil
	}
	m.Grants = removeRef(m.Grants, ref)
	switch {
	case len(m.Grants) > 0:
		if m.State == state.MemdevActive {
			return fsm.Apply(&state.MemdevTable, &m.State, state.MemdevActive, trigger)
		}
		return nil
	case unused(m):
		return dropMemdev(r, serial, trigger)
	case m.State == state.MemdevActive && len(m.Regions) > 0:
		return fsm.Apply(&state.MemdevTable, &m.State, state.MemdevIdleKept, trigger)
	}
	return nil
}

// unused tells whether nothing is left of a memdev record: no grant, no
// region of ours, no hollow region and no stale lease bind or node mount.
func unused(m *state.Memdev) bool {
	return len(m.Grants) == 0 && len(m.Regions) == 0 && len(m.Hollow) == 0 && len(m.StaleBinds) == 0
}

// dropMemdev removes the record of the memdev with the serial (NONE).
func dropMemdev(r *state.Records, serial state.Serial, trigger state.Trigger) error {
	m := r.Memdev(serial)
	if m == nil {
		return nil
	}
	if err := fsm.Apply(&state.MemdevTable, &m.State, state.MemdevNone, trigger); err != nil {
		return err
	}
	removeRecord(&r.Memdevs, func(x *state.Memdev) bool { return x.Serial == serial })
	return nil
}

// leaveUsers removes a released grant from the users of the dax devices of
// the regions.
func leaveUsers(r *state.Records, ref state.GrantRef, trigger state.Trigger) error {
	for i := range r.Regions {
		for j := range r.Regions[i].DaxDevices {
			d := &r.Regions[i].DaxDevices[j]
			if !slices.Contains(d.Users, ref) {
				continue
			}
			if d.State != state.DaxUnbound && d.State != state.DaxCreating {
				if err := fsm.Apply(&state.DaxDeviceTable, &d.State, d.State, trigger); err != nil {
					return err
				}
			}
			d.Users = removeRef(d.Users, ref)
		}
	}
	return nil
}

// errHolder is the kind of the error of a holder wait that did not end.
var errHolder = errors.New("its block device is held")

// applyPolicy releases what the memdev of a grant has when the grant is
// its last one: the holder wait of a persistent block device, the regions
// of ours per Policy, and a pool memdev disabled so that the pool can
// detach it. A memdev that is gone is not an error: its regions of ours
// that the kernel left hollow are deleted. It tells whether the region was
// kept for the claim of the grant (a rollback or a recovery below a newer
// region of another memdev).
func (e *Engine) applyPolicy(ctx context.Context, g *state.Grant, call *releaseCall) (bool, error) {
	release, trigger := call.release, call.trigger
	source, serial := g.Asked.Source, g.Serial
	memdev, err := e.k.FindMemdev(uint64(serial))
	if errors.Is(err, kernel.ErrNotFound) {
		return false, e.deleteGoneHollow(ctx, g, trigger)
	}
	if err != nil {
		return false, &releaseError{serial: serial, err: err}
	}
	decision, _ := e.decision(g, memdev, release)
	forClaim := decision.forClaim(release)
	if decision.keeps() {
		e.keptIdle(g, memdev, decision, release)
		return forClaim, e.keepMemdev(ctx, serial, trigger, forClaim)
	}
	if dev := e.firstBlockDevice(g, call); dev != "" {
		if err := e.waitNoHolder(ctx, source, serial, dev, call.holderDeadline(g.Ref.Claim)); err != nil {
			return false, &releaseError{serial: serial, held: true, err: err}
		}
	}
	claim := ""
	if forClaim {
		claim = g.Ref.Claim
	}
	kept, err := e.destroyRegions(ctx, serial, memdev, decision, claim, trigger)
	if err == nil && !kept && source == state.SourcePool {
		err = e.disableMemdev(ctx, source, serial, memdev)
	}
	if err != nil {
		return false, &releaseError{serial: serial, err: err}
	}
	if m := e.records().Memdev(serial); m != nil && len(m.Hollow) > 0 {
		why := "left by a Prepare of the claim that did not finish"
		if release == ReleaseRollback {
			why = "left by a cxl create-region of the claim"
		}
		if err := e.deleteHollow(ctx, serial, trigger, why); err != nil {
			return false, err
		}
	}
	return kept, nil
}

// releaseIdle releases what the memdev m without grants has, its queue
// held, as the release policy decides for an idle memdev of the kind: the
// holder wait of its block device until holderDeadline, its regions of
// ours, a pool memdev disabled; its record goes when nothing is left of
// it. A memdev that is gone has nothing to release.
func (e *Engine) releaseIdle(ctx context.Context, m *state.Memdev, kind Kind, holderDeadline time.Time, trigger state.Trigger) error {
	source, serial := m.Source, m.Serial
	decision := Policy(ReleaseIdle, kind, nil)
	if decision.keeps() {
		return nil
	}
	memdev, err := e.k.FindMemdev(uint64(serial))
	if errors.Is(err, kernel.ErrNotFound) {
		return nil
	}
	if err != nil {
		return &releaseError{serial: serial, err: err}
	}
	if dev := e.currentBlockDevice(serial); dev != "" {
		if err := e.waitNoHolder(ctx, source, serial, dev, holderDeadline); err != nil {
			return &releaseError{serial: serial, held: true, err: err}
		}
	}
	_, err = e.destroyRegions(ctx, serial, memdev, decision, "", trigger)
	if err == nil && source == state.SourcePool {
		err = e.disableMemdev(ctx, source, serial, memdev)
	}
	if err == nil {
		err = e.commit(ctx, func(r *state.Records) error {
			if m := r.Memdev(serial); m != nil && unused(m) {
				return dropMemdev(r, serial, trigger)
			}
			return nil
		})
	}
	if err != nil {
		return &releaseError{serial: serial, err: err}
	}
	return nil
}

// deleteGoneHollow releases what is left of a memdev that is gone: its regions
// of ours that the kernel left hollow.
func (e *Engine) deleteGoneHollow(ctx context.Context, g *state.Grant, trigger state.Trigger) error {
	klog.Infof("engine: %s is gone, nothing to release", memdevText(g.Asked.Source, g.Serial))
	return e.deleteHollow(ctx, g.Serial, trigger, "its memdev was removed before the region was destroyed")
}

// releaseStale releases a grant of a PREPARED_STALE preparation: nothing
// of it exists in this boot, so its memdev is released by its serial (a
// pool memdev that no grant of this boot uses is disabled when it has no
// region), the directory of its lease bind is removed when nothing is
// mounted there, and the grant is dropped.
func (e *Engine) releaseStale(ctx context.Context, ref state.GrantRef, trigger state.Trigger) error {
	r := e.records()
	g := r.Grant(ref)
	serial := g.Serial
	if target := g.Intent.BindTarget; target != "" {
		// the directory of the lease bind of another boot
		if mi, err := e.k.Mounted(target); err == nil && mi == nil {
			e.removeMountDir(target)
		}
	}
	if g.Asked.Source == state.SourcePool && r.Memdev(serial) == nil {
		memdev, err := e.k.FindMemdev(uint64(serial))
		switch {
		case errors.Is(err, kernel.ErrNotFound):
			klog.Infof("engine: %s is gone, nothing to release", memdevText(g.Asked.Source, serial))
		case err != nil:
			return err
		default:
			if err := e.disableMemdev(ctx, g.Asked.Source, serial, memdev); err != nil {
				return err
			}
		}
	}
	return e.commit(ctx, func(r *state.Records) error {
		g := r.Grant(ref)
		if g == nil {
			return nil
		}
		if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantNone, trigger); err != nil {
			return err
		}
		removeRecord(&r.Grants, func(x *state.Grant) bool { return x.Ref == ref })
		if m := r.Memdev(serial); m != nil {
			m.Grants = removeRef(m.Grants, ref)
		}
		return nil
	})
}
