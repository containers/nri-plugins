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
	"fmt"
	"slices"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// GrantError is the error of a grant that could not be realised.
type GrantError struct {
	Ref state.GrantRef
	Err error
}

// Error returns the error of the grant.
func (e *GrantError) Error() string {
	return e.Err.Error()
}

// Unwrap returns the error of the grant.
func (e *GrantError) Unwrap() error {
	return e.Err
}

// PrepareGrants realises the grants of reqs in their order and returns them
// as recorded, REALISED. A grant that is REALISED already and verifies is
// reused; one that a cut or failed Prepare left is realised further. On an
// error the grants of the claims of reqs that this or an earlier Prepare
// made are released as a rollback (Policy ReleaseRollback), on a context
// that the end of ctx does not end; what the rollback keeps stays recorded
// and is returned with the error.
func (e *Engine) PrepareGrants(ctx context.Context, reqs []GrantRequest) ([]state.Grant, error) {
	ctx, end := e.Request(ctx)
	defer end()
	refs := make([]state.GrantRef, 0, len(reqs))
	for i := range reqs {
		if !e.owns(reqs[i].Asked.Source) {
			return nil, fmt.Errorf("engine: grant %s: the records of source %q are not the engine's", reqs[i].Ref, reqs[i].Asked.Source)
		}
	}
	for i := range reqs {
		refs = append(refs, reqs[i].Ref)
		if err := e.prepareGrant(ctx, &reqs[i]); err != nil {
			rctx, cancel := e.rollbackContext(ctx)
			kept := e.rollback(rctx, reqs, reqs[i].Ref, err)
			e.verify(rctx, nil, state.TriggerPrepare)
			cancel()
			return kept, &GrantError{Ref: reqs[i].Ref, Err: err}
		}
	}
	e.verify(ctx, refs, state.TriggerPrepare)
	return e.grantsOf(refs), nil
}

// grantsOf returns the records of the grants refs that the store has.
func (e *Engine) grantsOf(refs []state.GrantRef) []state.Grant {
	r := e.records()
	var out []state.Grant
	for _, ref := range refs {
		if g := r.Grant(ref); g != nil {
			out = append(out, g.Clone())
		}
	}
	return out
}

// prepareGrant realises one grant in the queue of its memdev.
func (e *Engine) prepareGrant(ctx context.Context, req *GrantRequest) error {
	leave, err := e.acquire(ctx, req.Serial)
	if err != nil {
		return err
	}
	defer leave()
	if g := e.records().Grant(req.Ref); g != nil {
		switch g.State {
		case state.GrantRealised:
			if err := e.verifyGrant(g); err == nil {
				klog.V(3).Infof("engine: grant %s is realised and verifies: reused", req.Ref)
				return nil
			}
		case state.GrantUnrestored:
			return e.restoreGrant(ctx, req)
		case state.GrantReleasing:
			// a leftover of a failed release, on the context of a rollback
			rctx, cancel := e.rollbackContext(ctx)
			err := e.releaseGrant(rctx, req.Ref, newReleaseCall(e, ReleaseRollback, []state.GrantRef{req.Ref}))
			cancel()
			if err != nil {
				return err
			}
		}
	}
	if err := e.checkLocalKind(ctx, req); err != nil {
		return err
	}
	if err := e.destroyIdleLeftover(ctx, req); err != nil {
		return err
	}
	if err := e.journal(ctx, req); err != nil {
		return err
	}
	return e.realise(ctx, req)
}

// destroyIdleLeftover releases the region of a local persistent memdev that
// was kept idle when the next Prepare of the memdev finds no region above it
// any more (the release policy's idle leftover), the memdev's queue held:
// the Prepare makes it anew.
func (e *Engine) destroyIdleLeftover(ctx context.Context, req *GrantRequest) error {
	m := e.records().Memdev(req.Serial)
	if req.Asked.Kind != state.KindLocalPersistent || m == nil || m.State != state.MemdevIdleKept {
		return nil
	}
	idle := *m
	return e.releaseIdleQueued(ctx, &idle, state.TriggerPrepare)
}

// journal commits the intent of the grant (REQUESTED -> JOURNALED), with
// its memdev record, before any kernel object of it is touched. A grant
// that is JOURNALED or REALISING already keeps its intent. The local guard
// refuses a local grant on a serial of the pool.
func (e *Engine) journal(ctx context.Context, req *GrantRequest) error {
	if req.Asked.Source == state.SourceLocal {
		if err := e.localGuard(ctx, req.Serial); err != nil {
			klog.Errorf("engine: refusing %s: %v", memdevText(state.SourceLocal, req.Serial), err)
			return err
		}
	}
	return e.commit(ctx, func(r *state.Records) error {
		g := r.Grant(req.Ref)
		if g == nil {
			r.Grants = append(r.Grants, state.Grant{Ref: req.Ref, Serial: req.Serial, Asked: req.Asked, State: state.GrantRequested})
			g = &r.Grants[len(r.Grants)-1]
		}
		switch g.State {
		case state.GrantRequested:
			if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantJournaled, state.TriggerPrepare); err != nil {
				return err
			}
		case state.GrantJournaled, state.GrantRealising, state.GrantRealised:
		default:
			return fmt.Errorf("grant %s is %s", req.Ref, g.State)
		}
		g.Asked = req.Asked
		g.Intent.BindTarget = req.BindTarget
		if req.Lend != nil {
			lend := *req.Lend
			g.Intent.Lend = &lend
		}
		return journalMemdev(r, req)
	})
}

// journalMemdev adds the grant to the record of its memdev, made ACTIVE.
func journalMemdev(r *state.Records, req *GrantRequest) error {
	m := r.Memdev(req.Serial)
	if m == nil {
		r.Memdevs = append(r.Memdevs, state.Memdev{Serial: req.Serial, Source: req.Asked.Source, State: state.MemdevNone})
		m = &r.Memdevs[len(r.Memdevs)-1]
	}
	if m.Source != req.Asked.Source {
		return fmt.Errorf("memdev %s is a %s memdev, not %s", req.Serial, m.Source, req.Asked.Source)
	}
	switch m.State {
	case state.MemdevNone, state.MemdevIdleKept:
		if err := fsm.Apply(&state.MemdevTable, &m.State, state.MemdevActive, state.TriggerPrepare); err != nil {
			return err
		}
	case state.MemdevActive:
		if !m.HasGrant(req.Ref) {
			if err := fsm.Apply(&state.MemdevTable, &m.State, state.MemdevActive, state.TriggerPrepare); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("memdev %s is %s", req.Serial, m.State)
	}
	m.Facts = mergeFacts(m.Facts, req.Facts)
	m.Grants = addRef(m.Grants, req.Ref)
	return nil
}

// mergeFacts returns the facts of a memdev record with those that a
// request knows.
func mergeFacts(old, req state.MemdevFacts) state.MemdevFacts {
	out := req
	if out.NUMAAffinity == nil {
		out.NUMAAffinity = old.NUMAAffinity
	}
	return out
}

// localGuard returns an error if a local grant on the serial must not be
// made: the provenance of the serial is POOL or HELD, a pool grant uses
// it, or the kubelet plugin's guard refuses it.
func (e *Engine) localGuard(ctx context.Context, serial state.Serial) error {
	r := e.records()
	if p := r.ProvenanceOf(serial); p != nil && (p.State == state.ProvenancePool || p.State == state.ProvenanceHeld) {
		return fmt.Errorf("memdev %s is a pool device (%s): not used as a local memdev", serial.String(), p.State)
	}
	if m := r.Memdev(serial); m != nil && m.Source == state.SourcePool {
		return fmt.Errorf("memdev %s is a pool device: not used as a local memdev", serial.String())
	}
	if e.opts.LocalGuard != nil {
		return e.opts.LocalGuard(ctx, serial)
	}
	return nil
}

// realise makes the kernel objects of a JOURNALED or REALISING grant and
// records them, the grant REALISED at the end.
func (e *Engine) realise(ctx context.Context, req *GrantRequest) error {
	memdev, err := e.memdevOf(ctx, req)
	if err != nil {
		return err
	}
	if err := e.commit(ctx, func(r *state.Records) error {
		g := r.Grant(req.Ref)
		if g.State == state.GrantJournaled {
			if err := fsm.Apply(&state.GrantTable, &g.State, state.GrantRealising, state.TriggerPrepare); err != nil {
				return err
			}
		}
		if m := r.Memdev(req.Serial); m != nil {
			m.Name = memdev
		}
		return nil
	}); err != nil {
		return err
	}
	switch {
	case req.Asked.Kind.Persistent():
		return e.realisePersistent(ctx, req, memdev)
	case req.Asked.Source == state.SourcePool:
		return e.realisePoolRam(ctx, req, memdev)
	case req.Asked.Kind.Shared():
		return e.realiseLocalShared(ctx, req, memdev)
	}
	return e.realiseLocal(ctx, req, memdev)
}

// memdevOf returns the name of the memdev of a grant. A pool grant waits
// for its memdev to appear (Timeouts.Memdev), or for the driver of a memdev
// that is present (Timeouts.ProbeGrace: a disabled memdev is enabled when
// its region is made); a local memdev must be present.
func (e *Engine) memdevOf(ctx context.Context, req *GrantRequest) (string, error) {
	serial := uint64(req.Serial)
	if req.Asked.Source != state.SourcePool {
		return e.k.FindMemdev(serial)
	}
	if found, err := e.k.FindMemdev(serial); err == nil {
		wctx, cancel := context.WithTimeout(ctx, e.opts.Timeouts.ProbeGrace)
		defer cancel()
		if n, err := e.k.WaitMemdev(wctx, serial, kernel.MemdevBound); err == nil {
			return n, nil
		}
		return found, nil
	}
	wctx, cancel := context.WithTimeout(ctx, e.opts.Timeouts.Memdev)
	defer cancel()
	return e.k.WaitMemdev(wctx, serial, kernel.MemdevBound)
}

// realised records what a grant got and the grant REALISED, the grant a
// user of its dax device and node mount.
func (e *Engine) realised(ctx context.Context, req *GrantRequest, got state.Got, dev state.DevNumbers) error {
	return e.commit(ctx, func(r *state.Records) error {
		g := r.Grant(req.Ref)
		if g == nil {
			return fmt.Errorf("grant %s has no record", req.Ref)
		}
		bind := g.Got.Bind
		g.Got = got
		if got.Bind == nil {
			g.Got.Bind = bind
		}
		if got.Region != "" {
			if err := setIntentRegion(r, req.Ref, got.Region); err != nil {
				return err
			}
			reg := r.Region(got.Region)
			if reg == nil {
				return fmt.Errorf("region %s has no record", got.Region)
			}
			if got.Dax != "" {
				if err := addDaxUser(reg, got.Dax, req.Ref); err != nil {
					return err
				}
				if !dev.IsZero() {
					reg.DaxDevice(got.Dax).Dev = dev
				}
			}
		}
		if g.State == state.GrantRealised {
			return nil
		}
		return fsm.Apply(&state.GrantTable, &g.State, state.GrantRealised, state.TriggerPrepare)
	})
}

// addDaxUser adds a grant to the users of a dax device of a region.
func addDaxUser(reg *state.Region, dax string, ref state.GrantRef) error {
	d := reg.DaxDevice(dax)
	if d == nil {
		return fmt.Errorf("%s has no record of %s", reg.Ref(), dax)
	}
	if slices.Contains(d.Users, ref) {
		return nil
	}
	if len(d.Users) > 0 && d.State != state.DaxUnbound {
		if err := fsm.Apply(&state.DaxDeviceTable, &d.State, d.State, state.TriggerPrepare); err != nil {
			return err
		}
	}
	d.Users = append(d.Users, ref)
	return nil
}

// rollback releases the grants of the claims of reqs that the store has,
// those of this Prepare and of earlier ones that did not finish (Policy
// ReleaseRollback), and returns those that it did not release (the
// kubelet plugin logs that it keeps their claim's record). The grants of a
// claim that is prepared (a Prepare again that failed) stay. Errors are
// logged.
func (e *Engine) rollback(ctx context.Context, reqs []GrantRequest, failed state.GrantRef, why error) []state.Grant {
	r := e.records()
	var refs []state.GrantRef
	seen := map[state.GrantRef]bool{}
	add := func(ref state.GrantRef) {
		if !seen[ref] && r.Grant(ref) != nil {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	for i := range reqs {
		p := r.PreparationOf(reqs[i].Ref)
		if p != nil && p.Prepared() {
			continue
		}
		add(reqs[i].Ref)
		if p != nil {
			for _, res := range p.Results {
				if res.Grant != nil {
					add(*res.Grant)
				}
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	refs = localOrder(r, refs)
	e.revokeLends(ctx, refs)
	e.retryAllStale(ctx, state.TriggerPrepare)
	call := newReleaseCall(e, ReleaseRollback, refs)
	call.reasons[failed] = why.Error()
	for _, group := range e.byMemdev(refs) {
		if err := e.releaseGroup(ctx, group.serial, group.refs, call); err != nil {
			klog.Errorf("engine: rollback: %v", err)
		}
	}
	return e.grantsOf(refs)
}

// revokeLends revokes the NUMA nodes that the grants refs lend (Got.Lend,
// recorded before the lend) through Options.Revoke before their memory goes
// offline. Errors are logged: the release goes on.
func (e *Engine) revokeLends(ctx context.Context, refs []state.GrantRef) {
	if e.opts.Revoke == nil {
		return
	}
	r := e.records()
	for _, ref := range refs {
		g := r.Grant(ref)
		if g == nil || g.Got.Lend == nil || g.Got.Lend.NUMANode == nil {
			continue
		}
		if err := e.opts.Revoke(ctx, ref, *g.Got.Lend); err != nil {
			klog.Errorf("engine: rollback: claim %s: %v", e.claimNameOf(ref), err)
		}
	}
}

// RecordLend records the NUMA node that the realised grant ref of a
// companion claim lends, before it is lent: the node in the lend of its
// intent, and that lend as the grant's (Got.Lend), in one durable commit.
func (e *Engine) RecordLend(ctx context.Context, ref state.GrantRef, node int) error {
	return e.commit(ctx, func(r *state.Records) error {
		g := r.Grant(ref)
		if g == nil || g.Intent.Lend == nil {
			return fmt.Errorf("grant %s has no lend", ref)
		}
		g.Intent.Lend.NUMANode = state.NUMANodePtr(node)
		lend := *g.Intent.Lend
		g.Got.Lend = &lend
		return nil
	})
}

// verifyGrant returns an error if what a REALISED grant got is not what
// the kernel has now: its region on its memdev, the numbers of its dax
// device and its node mount mounted. The block device of a persistent grant
// is checked by the restore at start (blockError).
func (e *Engine) verifyGrant(g *state.Grant) error {
	if g.Got.Region == "" {
		return fmt.Errorf("grant %s has no region", g.Ref)
	}
	ri, err := e.k.Region(string(g.Got.Region))
	if err != nil || !slices.Contains(ri.Serials, uint64(g.Serial)) {
		return fmt.Errorf("grant %s: %s is gone", g.Ref, g.Got.Region)
	}
	r := e.records()
	if dev := r.DaxDevOf(g); g.Got.Dax != "" && !dev.IsZero() {
		major, minor, err := e.k.DaxDevNumbers(g.Got.Dax)
		if err != nil || major != dev.Major || minor != dev.Minor {
			return fmt.Errorf("grant %s: %s is not the dax device %d:%d as prepared", g.Ref, g.Got.Dax, dev.Major, dev.Minor)
		}
	}
	if path := mountPathOf(r, g); path != "" {
		mi, err := e.k.Mounted(path)
		if err != nil || mi == nil {
			return fmt.Errorf("grant %s: %s is not mounted", g.Ref, path)
		}
	}
	return nil
}
