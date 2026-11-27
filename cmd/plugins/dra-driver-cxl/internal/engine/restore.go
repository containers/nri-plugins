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
	"slices"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// errForeignMount is the kind of the error of a node mount path where
// something else than the node mount of the grants is mounted.
var errForeignMount = errors.New("something else is mounted at the node mount")

// restoreGrants mounts again the node mounts of the realised grants of the
// prepared claims, by the identity of their block device, with the state
// of the superblock and the read-only flags that the grants of the device
// derive. A grant whose node mount cannot be mounted again is
// REALISED_UNRESTORED (its node mount UNRESTORED); an unrestored one that
// verifies again is REALISED.
func (e *Engine) restoreGrants(ctx context.Context, why map[state.GrantRef]error) error {
	r := e.records()
	serials := map[state.Serial]bool{}
	for i := range r.Grants {
		g := &r.Grants[i]
		if !e.owns(g.Asked.Source) {
			continue
		}
		if p := r.PreparationOf(g.Ref); p != nil && p.Prepared() && p.State != state.PreparationUnpreparing && prepared(g) {
			serials[g.Serial] = true
		}
	}
	for _, serial := range slices.Sorted(maps.Keys(serials)) {
		if err := e.restoreGrantsOf(ctx, serial, why); err != nil {
			return err
		}
	}
	return nil
}

// restoreGrantsOf restores the node mounts of the realised grants of the
// memdev with the serial (restoreSerial) and records what it found, with
// the reasons in why.
func (e *Engine) restoreGrantsOf(ctx context.Context, serial state.Serial, why map[state.GrantRef]error) error {
	rs := e.restoreSerial(serial, why)
	if len(rs.failed) == 0 && len(rs.restored) == 0 && len(rs.loops) == 0 && len(rs.bad) == 0 {
		return nil
	}
	return e.commit(ctx, func(r *state.Records) error {
		restored(r, rs.loops)
		if err := unrestored(r, rs.failed, rs.restored); err != nil {
			return err
		}
		return badBlocks(r, rs.bad)
	})
}

// blockError returns why a grant of a persistent memdev does not verify:
// its block device is not the one of the fsdax namespace of its region now;
// nil for another grant.
func (e *Engine) blockError(g *state.Grant) error {
	if !blockChecked(g) {
		return nil
	}
	return e.checkBlockDevice(g)
}

// restoredSerial is what the restore of the realised grants of a memdev
// found: the grants whose node mount is not mounted again, with why; the
// unrestored ones that verify again; the loop devices of the cached node
// mounts now, by node mount; the grants of a memdev whose block device is
// not the one of its region now, with why.
type restoredSerial struct {
	failed   map[state.GrantRef]error
	restored []state.GrantRef
	loops    map[string]string
	bad      map[state.GrantRef]error
}

// restoreSerial restores the node mounts of the realised grants of the
// memdev with the serial and returns what it found; why gets the reasons of
// the grants that stay unrestored too.
func (e *Engine) restoreSerial(serial state.Serial, why map[state.GrantRef]error) restoredSerial {
	grants := e.preparedPmemGrants(serial)
	writer := firstWriter(grants) != nil || firstWriter(e.unpreparingPmemGrants(serial)) != nil
	badWhy := map[state.GrantRef]error{}
	var rs restoredSerial
	rs.bad = e.checkBlocks(serial, badWhy)
	byMount := e.restoreMounts(grants, writer, rs.bad)
	rs.failed, rs.restored = e.classifyRestored(serial, byMount, why)
	for ref, err := range badWhy {
		why[ref] = err
	}
	for ref := range rs.bad {
		rs.restored = slices.DeleteFunc(rs.restored, func(r state.GrantRef) bool { return r == ref })
		if _, ok := rs.failed[ref]; ok {
			// its node mount is not mounted again: UNRESTORED with it
			rs.failed[ref] = rs.bad[ref]
			delete(rs.bad, ref)
		}
	}
	if dev := firstBlockDevice(grants); dev != "" && e.checkRecordedBlockDevice(serial, dev) == nil {
		e.restoreFlags(serial, dev, grants, writer)
	}
	rs.loops = map[string]string{}
	for _, u := range grants {
		if rm, ok := byMount[u.nodeMount]; ok && rm.err == nil && rm.loop != u.loop && u.access == api.AccessCached {
			klog.Infof("engine: %s memdev %s: %s is on %s now, not %s", u.source, serial.String(), u.nodeMount, rm.loop, u.loop)
			rs.loops[u.nodeMount] = rm.loop
		}
	}
	return rs
}

// blockChecked tells whether the block device of a grant is verified
// against its region at a start: every grant of a local persistent memdev,
// a pool grant of mode block (a pool grant of mode fs is verified by its
// node mount).
func blockChecked(g *state.Grant) bool {
	if !g.Asked.Kind.Persistent() || g.Got.BlockDevice == nil {
		return false
	}
	return g.Asked.Source == state.SourceLocal || g.Asked.Mode == state.ModeBlock
}

// checkBlocks returns the realised and unrestored grants of prepared claims
// of the persistent memdev with the serial whose block device is verified
// (blockChecked) when it is not the one of the fsdax namespace of its
// region now: those grants have no CDI device then; why gets the reason.
func (e *Engine) checkBlocks(serial state.Serial, why map[state.GrantRef]error) map[state.GrantRef]error {
	r := e.records()
	var grants []*state.Grant
	for i := range r.Grants {
		g := &r.Grants[i]
		p := r.PreparationOf(g.Ref)
		if g.Serial == serial && blockChecked(g) &&
			prepared(g) && p != nil && (p.State == state.PreparationPrepared || p.State == state.PreparationUnrestored) {
			grants = append(grants, g)
		}
	}
	if len(grants) == 0 {
		return nil
	}
	err := e.checkBlockDevice(grants[0])
	if err == nil {
		return nil
	}
	klog.Errorf("engine: %s (%s): %v", memdevText(grants[0].Asked.Source, serial), e.memdevName(serial), err)
	bad := map[state.GrantRef]error{}
	for _, g := range grants {
		why[g.Ref], bad[g.Ref] = err, err
	}
	return bad
}

// badBlocks records the grants whose block device changed (bad)
// REALISED_UNRESTORED and their preparations PREPARED_UNRESTORED; their
// node mounts stay as they are, released with them.
func badBlocks(r *state.Records, bad map[state.GrantRef]error) error {
	for ref := range bad {
		g := r.Grant(ref)
		if g == nil {
			continue
		}
		if g.State == state.GrantRealised {
			if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantUnrestored, state.TriggerRecover); err != nil {
				return err
			}
		}
		if err := preparationUnrestored(r, ref); err != nil {
			return err
		}
	}
	return nil
}

// preparationUnrestored records the PREPARED or PREPARED_UNRESTORED
// preparation of a grant that the recovery did not restore
// PREPARED_UNRESTORED.
func preparationUnrestored(r *state.Records, ref state.GrantRef) error {
	if p := r.PreparationOf(ref); p != nil && (p.State == state.PreparationPrepared || p.State == state.PreparationUnrestored) {
		return fsm.Apply(&state.PreparationTable, &p.State, state.PreparationUnrestored, state.TriggerRecover)
	}
	return nil
}

// restored records the loop devices of the cached node mounts that are on
// another one now (loops by node mount path) in their mounts.
func restored(r *state.Records, loops map[string]string) {
	for i := range r.Regions {
		for j := range r.Regions[i].Mounts {
			if m := &r.Regions[i].Mounts[j]; loops[m.Path] != "" {
				m.Loop = loops[m.Path]
			}
		}
	}
}

// preparedPmemGrants returns the grants of the memdev with the serial of
// PREPARED and PREPARED_UNRESTORED preparations: those that a start
// restores (not the grants of cut Prepares, not those being unprepared).
func (e *Engine) preparedPmemGrants(serial state.Serial) []pmemGrant {
	r := e.records()
	var out []pmemGrant
	for _, u := range pmemGrants(r, serial) {
		if p := r.PreparationOf(u.ref); p != nil && (p.State == state.PreparationPrepared || p.State == state.PreparationUnrestored) {
			out = append(out, u)
		}
	}
	return out
}

// unpreparingPmemGrants returns the grants of the memdev with the serial
// of claims whose Unprepare did not finish (UNPREPARING) that still use
// their node mount: a start leaves them as they are, and a writer of
// them keeps the device read-write until kubelet's retry of the Unprepare.
func (e *Engine) unpreparingPmemGrants(serial state.Serial) []pmemGrant {
	r := e.records()
	if m := r.Memdev(serial); m == nil {
		return nil
	}
	var out []pmemGrant
	for _, u := range pmemGrants(r, serial) {
		if p := r.PreparationOf(u.ref); p != nil && p.State == state.PreparationUnpreparing {
			out = append(out, u)
		}
	}
	return out
}

// restoreMounts restores the node mount of each of the grants and returns
// why each that is not mounted afterwards is not, by node mount. The error
// of a node mount whose grants are all on a block device that changed (bad)
// is not logged: the block device's is.
func (e *Engine) restoreMounts(grants []pmemGrant, writer bool, bad map[state.GrantRef]error) map[string]restoredMount {
	byMount := map[string]restoredMount{}
	for _, mnt := range nodeMounts(grants) {
		i := slices.IndexFunc(grants, func(u pmemGrant) bool { return u.nodeMount == mnt })
		loop, err := e.restoreNodeMount(&grants[i], writer, nodeMounts(grants))
		quiet := !slices.ContainsFunc(grants, func(u pmemGrant) bool { return u.nodeMount == mnt && bad[u.ref] == nil })
		switch {
		case err != nil && errors.Is(err, errForeignMount):
			klog.Errorf("engine: %v: it is never used or unmounted by the driver", err)
		case err != nil && !quiet:
			klog.Errorf("engine: claim %s: %v", e.claimNameOf(grants[i].ref), err)
		}
		byMount[mnt] = restoredMount{loop: loop, err: err}
	}
	return byMount
}

// checkRestoredMount checks what is mounted at the node mount of the grant
// u at a start, mi, and returns the loop device of a cached one and whether
// it verifies: an error of kind errForeignMount when it is not the device
// of the grants; the device of the grants with other options or another
// type, or a mount whose device cannot be told, is logged and stays theirs
// as it is (released with them).
func (e *Engine) checkRestoredMount(u *pmemGrant, mi *kernel.MountInfo) (string, bool, error) {
	ok, err := e.isDeviceMount(mi, u.blockDevice, u.access)
	if err == nil && !ok {
		_, cerr := e.checkNodeMount(u.serial, u.blockDevice, u.access, u.fsType, mi)
		return "", false, fmt.Errorf("%w: %w", errForeignMount, cerr)
	}
	loop := ""
	if err == nil {
		loop, err = e.checkNodeMount(u.serial, u.blockDevice, u.access, u.fsType, mi)
	}
	if err != nil {
		klog.Errorf("engine: claim %s: %v", e.claimNameOf(u.ref), err)
		if u.access == api.AccessCached {
			loop = mi.Source
		}
		return loop, false, nil
	}
	return loop, true, nil
}

// restoredMount is the result of the restore of a node mount: its loop
// device now (cached), or the error that leaves its grants unrestored.
type restoredMount struct {
	loop string
	err  error
}

// classifyRestored returns the realised grants of prepared claims of the
// memdev with the serial whose node mount was not restored (byMount), with
// why (in why also those that stay unrestored), and the unrestored ones
// whose node mount is mounted again.
func (e *Engine) classifyRestored(serial state.Serial, byMount map[string]restoredMount, why map[state.GrantRef]error) (map[state.GrantRef]error, []state.GrantRef) {
	failed := map[state.GrantRef]error{}
	var restored []state.GrantRef
	r := e.records()
	for i := range r.Grants {
		g := &r.Grants[i]
		path := mountPathOf(r, g)
		if g.Serial != serial || !prepared(g) || path == "" {
			// only node mounts are verified at a start
			continue
		}
		rm, ok := byMount[path]
		err := rm.err
		if ok && err != nil {
			why[g.Ref] = err
		}
		switch {
		case !ok:
		case err != nil && g.State == state.GrantRealised:
			failed[g.Ref] = err
		case err == nil && g.State == state.GrantUnrestored:
			restored = append(restored, g.Ref)
		}
	}
	return failed, restored
}

// restoreFlags sets the read-only flags of the block device dev and of the
// loop devices of the node mounts of the grants as the grants derive.
// Errors are logged: the next Prepare or release of the device tries again.
func (e *Engine) restoreFlags(serial state.Serial, dev string, grants []pmemGrant, writer bool) {
	source := grants[0].source
	var loops []string
	for _, mnt := range nodeMounts(grants) {
		if mi, err := e.k.Mounted(mnt); err == nil && mi != nil && e.loopOf(mi) != "" && !config.FSTypeReadOnly(mi.FSType) {
			loops = append(loops, e.loopOf(mi))
		}
	}
	var errs []error
	if writer {
		errs = append(errs, e.setReadOnly(source, serial, dev, false))
		for _, l := range loops {
			errs = append(errs, e.setReadOnly(source, serial, l, false))
		}
	} else {
		for _, l := range loops {
			errs = append(errs, e.setReadOnly(source, serial, l, true))
		}
		errs = append(errs, e.setReadOnly(source, serial, dev, true))
	}
	if err := errors.Join(errs...); err != nil {
		klog.Errorf("engine: %v", err)
	}
}

// restoreNodeMount returns the loop device of the node mount of a grant
// after it mounted it again when it is missing and its block device is the
// one prepared, or checked the mount there; an existing superblock is made
// read-write for a writer of the device or read-only without one, a failure
// of that is logged. explained are the node mounts of the grants of the
// device.
func (e *Engine) restoreNodeMount(u *pmemGrant, writer bool, explained []string) (string, error) {
	mi, err := e.k.Mounted(u.nodeMount)
	if err != nil {
		return "", err
	}
	if mi == nil {
		klog.Infof("engine: %s memdev %s: %s is not mounted, mounting it again", u.source, u.serial.String(), u.nodeMount)
		if err := e.checkRecordedBlockDevice(u.serial, u.blockDevice); err != nil {
			e.removeMountDir(u.nodeMount)
			return "", fmt.Errorf("not restoring %s: %w", u.nodeMount, err)
		}
		return e.mountNode(u.source, u.serial, u.blockDevice, u.access, u.fsType, u.nodeMount, writer, explained)
	}
	loop, verified, err := e.checkRestoredMount(u, mi)
	if err != nil || !verified {
		return loop, err
	}
	switch {
	case writer && mi.SuperReadOnly() && config.FSTypeRemountable(mi.FSType):
		err = e.remountReadWrite(u.source, u.serial, u.blockDevice, loop, u.nodeMount, explained)
	case writer && mi.SuperReadOnly():
		err = fmt.Errorf("memdev %s: %s is mounted read-only and cannot be remounted read-write (%s)", u.serial.String(), u.nodeMount, mi.FSType)
	case !writer && !mi.SuperReadOnly():
		err = e.remountReadOnly(u.source, u.serial, u.blockDevice, loop, u.nodeMount)
	}
	if err != nil {
		klog.Errorf("engine: %v", err)
	}
	return loop, nil
}

// unrestored records the grants that did not verify REALISED_UNRESTORED
// (their node mounts UNRESTORED, or FOREIGN when something else is mounted
// there; their preparations PREPARED_UNRESTORED) and those that verify
// again REALISED (their preparations PREPARED when every grant is).
func unrestored(r *state.Records, failed map[state.GrantRef]error, restored []state.GrantRef) error {
	for ref, why := range failed {
		if err := grantUnrestored(r, ref, why); err != nil {
			return err
		}
	}
	for _, ref := range restored {
		if err := grantRestored(r, ref); err != nil {
			return err
		}
	}
	for i := range r.Preparations {
		p := &r.Preparations[i]
		if p.State != state.PreparationUnrestored || !r.AllRealised(p) {
			continue
		}
		if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationPrepared, state.TriggerRecover); err != nil {
			return err
		}
	}
	return nil
}

// grantUnrestored records a grant whose node mount was not restored, for
// why.
func grantUnrestored(r *state.Records, ref state.GrantRef, why error) error {
	g := r.Grant(ref)
	if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantUnrestored, state.TriggerRecover); err != nil {
		return err
	}
	if reg := r.Region(g.Got.Region); reg != nil && g.Got.Mount != "" {
		to := state.MountUnrestored
		if errors.Is(why, errForeignMount) {
			to = state.MountForeign
		}
		if m := reg.Mount(g.Got.Mount); m != nil && m.State != state.MountUnrestored && m.State != state.MountForeign {
			if err := fsm.Apply(&state.MountTable, &m.State, to, state.TriggerRecover); err != nil {
				return err
			}
		}
	}
	return preparationUnrestored(r, ref)
}

// grantRestored records an unrestored grant whose node mount is mounted
// again.
func grantRestored(r *state.Records, ref state.GrantRef) error {
	g := r.Grant(ref)
	if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantRealised, state.TriggerRecover); err != nil {
		return err
	}
	reg := r.Region(g.Got.Region)
	if reg == nil || g.Got.Mount == "" {
		return nil
	}
	m := reg.Mount(g.Got.Mount)
	if m == nil || m.State != state.MountUnrestored {
		return nil
	}
	want := state.MountedRO
	if mountWriter(r, m) {
		want = state.MountedRW
	}
	return fsm.Apply(&state.MountTable, &m.State, want, state.TriggerRecover)
}

// mountWriter tells whether a user of the node mount writes.
func mountWriter(r *state.Records, m *state.Mount) bool {
	for _, ref := range m.Users {
		if g := r.Grant(ref); g != nil && !g.Asked.ReadOnly {
			return true
		}
	}
	return false
}

// restoreGrant mounts again the node mount of a REALISED_UNRESTORED grant
// for a Prepare again of its claim, and makes it REALISED (its node mount
// mounted); an error names why it stays unrestored.
func (e *Engine) restoreGrant(ctx context.Context, req *GrantRequest) error {
	if err := e.blockError(e.records().Grant(req.Ref)); err != nil {
		// the grants of a device whose block device changed stay
		// unrestored
		return err
	}
	return e.remountGrant(ctx, req)
}

// remountGrant mounts again the node mount of a REALISED_UNRESTORED grant
// and makes it REALISED (restoreGrant), with the other grants of the node
// mount; their lease binds are bound again.
func (e *Engine) remountGrant(ctx context.Context, req *GrantRequest) error {
	g := e.records().Grant(req.Ref)
	grants := e.preparedPmemGrants(g.Serial)
	i := slices.IndexFunc(grants, func(u pmemGrant) bool { return u.ref == g.Ref })
	if i < 0 || grants[i].nodeMount == "" {
		if err := e.verifyGrant(g); err != nil {
			return err
		}
	}
	loop := ""
	if i >= 0 && grants[i].nodeMount != "" {
		l, err := e.restoreNodeMount(&grants[i], firstWriter(grants) != nil, nodeMounts(grants))
		if err != nil {
			return fmt.Errorf("memdev %s: its node mount %s is not mounted and cannot be restored: %w",
				g.Serial.String(), grants[i].nodeMount, err)
		}
		loop = l
	}
	var users []state.GrantRef
	err := e.commit(ctx, func(r *state.Records) error {
		g := r.Grant(req.Ref)
		if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantRealised, state.TriggerPrepare); err != nil {
			return err
		}
		reg := r.Region(g.Got.Region)
		if reg == nil || g.Got.Mount == "" {
			return nil
		}
		m := reg.Mount(g.Got.Mount)
		if m == nil {
			return nil
		}
		users = slices.Clone(m.Users)
		if loop != "" {
			m.Loop = loop
		}
		if m.State == state.MountUnrestored {
			want := state.MountedRO
			if mountWriter(r, m) {
				want = state.MountedRW
			}
			if err := fsm.Apply(&state.MountTable, &m.State, want, state.TriggerPrepare); err != nil {
				return err
			}
		}
		return remountedUsers(r, m, req.Ref)
	})
	if err != nil {
		return err
	}
	r := e.records()
	for _, ref := range users {
		if g := r.Grant(ref); g != nil {
			e.restoreBind(ctx, r, g, state.TriggerPrepare)
		}
	}
	return nil
}
