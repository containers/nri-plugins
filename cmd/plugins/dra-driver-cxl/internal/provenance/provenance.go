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

package provenance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// Provenance of a serial (state.Provenance): which memdevs are devices of
// a memory pool.
//
// Purpose: a pool controller deletes its ResourceSlices when it stops
// gracefully, and a pool device stays attached to the node meanwhile, so
// the serials of the current ResourceSlices do not tell all pool devices
// on the node. The watcher remembers every serial that it has seen in a
// pool ResourceSlice, prepared by the pool driver or in the status of a
// pool claim of the node (POOL), and every memdev that it has taken as a
// local one (LOCAL):
//   - a POOL serial is never a local memdev, whatever the ResourceSlices
//     say; it is forgotten only after it has been observed absent (in no
//     pool slice, not prepared, not on the node) for Retention;
//   - a memdev that is neither POOL nor LOCAL (new) is HELD, not published
//     at all, while a pool claim of the node is not attached yet (no serial
//     in its status): it may be its device; and while the pool slices
//     cannot be listed yet;
//   - otherwise a new memdev is LOCAL at once, unless a pool that the
//     watcher remembers has been observed without ResourceSlices for less
//     than MissingWindow, the list fails, or the records were lost: then it
//     is HELD for MaxHold, or until the pool is back; if the pool's slices
//     list it, it is POOL.
//
// Absence is observed: the watch scans at least every Poll (plus a tick),
// also without udev events. An absence counts from the first observation
// that missed it, over observations no more than observationGap apart: a
// longer gap (a stopped driver, a starved process, a failing list, a step
// of the clock) starts it again. Absence is not committed: after a restart
// it starts again; a hold goes on from its HeldSince.
//
// Identity: the serial.
//
// States: state.ProvenanceState; UNKNOWN is no record.
//
// Transitions: see the table state.ProvenanceTable (internal/state and
// docs/memory/cxl/states.md); a transition not in it is refused.
//
// Invariants: POOL never turns LOCAL; a record changes only by a committed
// step.
//
// Persistence: the Provenance records and the ListedPools of the store
// are the only copy. A change is a step, a pure function of the records
// that the store applies to its document: durable when a serial turns
// POOL, a hold starts, ends or changes its reason, or the operator removes
// a record; lazy otherwise. Nothing acts on a change before its commit; a
// failed commit changes nothing and the step is taken again at the next
// scan. A lazy step's lines and effects follow its application by the
// store: its write may still fail, and then the next scan takes it again.
// A step refreshes LastSeen of the records that it sees, and Listed of the
// pools, when it changes the records or one of the times is older than
// refresh. Without a store, or with a LOST one, which refuses every
// commit, the records are process memory.
//
// Locks and waits: the steps take no lock; Watcher.mu guards the records
// in memory.

var (
	// Retention is how long a serial is remembered after it was first
	// observed absent.
	Retention = 24 * time.Hour
	// MissingWindow is how long a pool observed without ResourceSlices (or
	// lost records) holds new memdevs.
	MissingWindow = time.Hour
	// MaxHold is how long a new memdev is held at most while a pool is
	// missing.
	MaxHold = 5 * time.Minute
	// refresh is how often the times of last sight are committed at most
	// when nothing else changes.
	refresh = time.Hour
	// observationGap is the longest time between two observations over
	// which an absence continues.
	observationGap = 5 * time.Minute
	// commitTimeout bounds a commit of the records.
	commitTimeout = time.Minute
)

// The triggers of the transitions that the watcher applies.
const (
	triggerList    = state.TriggerList
	triggerPrepare = state.TriggerPrepare
	triggerObserve = state.TriggerObserve
	triggerExpire  = state.TriggerExpire
	triggerLoad    = state.TriggerLoad
)

// errDurable is the error of a step applied in a lazy commit that makes a
// change which must be committed durably.
var errDurable = errors.New("the step must be committed durably")

// recorded tells whether the record is POOL or LOCAL.
func recorded(p *state.Provenance) bool {
	return p.State == state.ProvenancePool || p.State == state.ProvenanceLocal
}

// expires tells whether a hold for the reason expires after MaxHold.
func expires(reason state.HeldReason) bool {
	return reason != state.HeldNeverListed && reason != state.HeldAttaching
}

// recordOf returns the record of the serial in r, nil for none.
func recordOf(r *state.Records, serial uint64) *state.Provenance {
	return r.ProvenanceOf(state.Serial(serial))
}

// poolSerialsOf returns the serials of the POOL records of r.
func poolSerialsOf(r *state.Records) map[uint64]bool {
	out := map[uint64]bool{}
	for i := range r.Provenance {
		if r.Provenance[i].State == state.ProvenancePool {
			out[uint64(r.Provenance[i].Serial)] = true
		}
	}
	return out
}

// sections returns a copy of the provenance sections of r: the records
// that the steps change.
func sections(r *state.Records) *state.Records {
	return &state.Records{Provenance: slices.Clone(r.Provenance), ListedPools: slices.Clone(r.ListedPools)}
}

// step is one change of the records at now: the transitions of a list, of
// the serials that the pool driver prepares, of an observation of the
// memdevs or of the operator's order. Its methods apply the transitions to
// records and collect what they did.
type step struct {
	now time.Time
	// changed: the step changed a record or a pool beyond the times of
	// last sight; durable: a change that is committed durably.
	changed, durable bool
	// seen and seenPools are the serials and the pools that the step saw:
	// their times of last sight are refreshed with a change.
	seen      []uint64
	seenPools []string
	// lines are the log lines of the transitions.
	lines []func()
	// pool and held are the memdevs of an observation that are pool
	// devices and HELD; conflicts the pool devices that local claims use.
	pool, held map[uint64]bool
	conflicts  []conflict
	// forgotten are the serials and poolsGone the pools whose records the
	// step removed.
	forgotten []uint64
	poolsGone []string
}

// conflict is a pool device that local claims use.
type conflict struct {
	serial uint64
	name   string
	claims []string
}

// stepFunc applies the transitions of a step to the records.
type stepFunc func(s *step, r *state.Records)

// log adds a line of the step.
func (s *step) log(line func()) {
	s.lines = append(s.lines, line)
}

// transit moves the record p to the state on the trigger, as the table
// allows, and tells whether it did; a refused transition is a line of the
// step and leaves p as it is.
func (s *step) transit(p *state.Provenance, to state.ProvenanceState, trigger state.Trigger) bool {
	if err := fsm.Apply(&state.ProvenanceTable, &p.State, to, trigger); err != nil {
		serial := p.Serial
		s.log(func() { klog.Errorf("provenance: serial %s: %v", serial, err) })
		return false
	}
	return true
}

// add adds the record of a serial without one (UNKNOWN) moved to the state
// on the trigger, and returns it; nil if the table refuses the transition.
func (s *step) add(r *state.Records, serial uint64, to state.ProvenanceState, trigger state.Trigger) *state.Provenance {
	p := state.Provenance{Serial: state.Serial(serial), State: state.ProvenanceUnknown}
	if !s.transit(&p, to, trigger) {
		return nil
	}
	r.Provenance = append(r.Provenance, p)
	s.changed = true
	return &r.Provenance[len(r.Provenance)-1]
}

// remove moves the record of the serial to UNKNOWN on the trigger and
// removes it, and tells whether it did.
func (s *step) remove(r *state.Records, serial uint64, trigger state.Trigger) bool {
	i := slices.IndexFunc(r.Provenance, func(p state.Provenance) bool { return uint64(p.Serial) == serial })
	if i < 0 || !s.transit(&r.Provenance[i], state.ProvenanceUnknown, trigger) {
		return false
	}
	r.Provenance = slices.Delete(r.Provenance, i, i+1)
	s.changed = true
	return true
}

// notePool makes the serial seen now POOL on the trigger, listed in pool
// ("" when prepared).
func (s *step) notePool(r *state.Records, serial uint64, pool string, trigger state.Trigger) {
	p := recordOf(r, serial)
	switch {
	case p == nil:
		if p = s.add(r, serial, state.ProvenancePool, trigger); p == nil {
			return
		}
		s.durable = true
	case p.State == state.ProvenancePool:
		s.transit(p, state.ProvenancePool, trigger)
	default:
		local := p.State == state.ProvenanceLocal
		if !s.transit(p, state.ProvenancePool, trigger) {
			return
		}
		if local {
			s.log(func() {
				klog.Infof("provenance: memdev serial 0x%x is a pool device now (%s)", serial, orPrepared(pool))
			})
		}
		p.HeldSince, p.HeldReason = time.Time{}, ""
		s.changed, s.durable = true, true
	}
	if pool != "" && p.Pool != pool {
		p.Pool, s.changed = pool, true
	}
	s.seen = append(s.seen, serial)
}

// noteLocal makes the memdev m seen now LOCAL on the trigger: a new or a
// HELD one.
func (s *step) noteLocal(r *state.Records, m *kernel.MemdevInfo, trigger state.Trigger) {
	p, changed := recordOf(r, m.Serial), false
	switch {
	case p == nil:
		p = s.add(r, m.Serial, state.ProvenanceLocal, triggerObserve)
		changed = p != nil
	case p.State == state.ProvenanceHeld:
		if changed = s.transit(p, state.ProvenanceLocal, trigger); changed {
			p.HeldSince, p.HeldReason = time.Time{}, ""
			s.changed, s.durable = true, true
		}
	}
	if changed {
		name, serial := m.Name, m.Serial
		s.log(func() { klog.Infof("provenance: memdev 0x%x (%s) is a local memdev", serial, name) })
	}
	if p != nil {
		s.seen = append(s.seen, m.Serial)
	}
}

// hold holds the new memdev m (its record p, nil for none, else HELD) for
// the reason why (reason), and tells whether it stays HELD: a reason that
// does not expire holds it; one that expires holds it for MaxHold from the
// start of the hold, or from the change of its reason from one that does
// not expire.
func (s *step) hold(r *state.Records, m *kernel.MemdevInfo, p *state.Provenance, why string, reason state.HeldReason) bool {
	if p == nil {
		name, serial := m.Name, m.Serial
		s.log(func() {
			klog.Warningf("provenance: holding new memdev 0x%x (%s): it may be a pool device: %s", serial, name, why)
		})
		if p = s.add(r, m.Serial, state.ProvenanceHeld, triggerObserve); p == nil {
			return true
		}
		p.HeldSince, p.HeldReason = s.now, reason
		s.durable = true
	} else {
		s.transit(p, state.ProvenanceHeld, triggerObserve)
		if p.HeldReason != reason {
			if expires(reason) && !expires(p.HeldReason) {
				p.HeldSince = s.now
			}
			p.HeldReason = reason
			s.changed, s.durable = true, true
		}
	}
	return !expires(reason) || s.now.Sub(p.HeldSince) < MaxHold
}

// forget removes the POOL or LOCAL record of the serial, as the operator
// orders with --forget-provenance.
func (s *step) forget(r *state.Records, serial uint64) {
	p := recordOf(r, serial)
	if p == nil || !recorded(p) {
		s.log(func() {
			klog.Warningf("provenance: --forget-provenance %s: no record of the serial", api.FormatSerial(serial))
		})
		return
	}
	old := p.State
	if s.remove(r, serial, triggerLoad) {
		s.durable = true
		s.forgotten = append(s.forgotten, serial)
		s.log(func() { klog.Infof("provenance: forgetting serial 0x%x (%s, --forget-provenance)", serial, old) })
	}
}

// expireAbsent removes the POOL or LOCAL record of the serial that has been
// observed absent since then for Retention.
func (s *step) expireAbsent(r *state.Records, serial uint64, since time.Time) {
	p := recordOf(r, serial)
	if p == nil || !recorded(p) {
		return
	}
	pool := p.State == state.ProvenancePool
	if s.remove(r, serial, triggerExpire) {
		s.forgotten = append(s.forgotten, serial)
		s.log(func() {
			klog.Infof("provenance: forgetting serial 0x%x (pool %v, observed absent since %s)", serial, pool, since.Format(time.RFC3339))
		})
	}
}

// poolGone is a pool observed without ResourceSlices since then for
// Retention.
type poolGone struct {
	pool  string
	since time.Time
}

// notePools adds the pools of a list that are new and removes the pools
// gone.
func (s *step) notePools(r *state.Records, listed map[string]bool, gone []poolGone) {
	for _, pool := range slices.Sorted(maps.Keys(listed)) {
		if !slices.ContainsFunc(r.ListedPools, func(lp state.ListedPool) bool { return lp.Pool == pool }) {
			r.ListedPools = append(r.ListedPools, state.ListedPool{Pool: pool, Listed: s.now})
			s.changed = true
		}
		s.seenPools = append(s.seenPools, pool)
	}
	for _, g := range gone {
		i := slices.IndexFunc(r.ListedPools, func(lp state.ListedPool) bool { return lp.Pool == g.pool })
		if i < 0 {
			continue
		}
		r.ListedPools = slices.Delete(r.ListedPools, i, i+1)
		s.changed = true
		s.poolsGone = append(s.poolsGone, g.pool)
		s.log(func() {
			klog.Infof("provenance: forgetting pool %s (observed without ResourceSlices since %s)", g.pool, g.since.Format(time.RFC3339))
		})
	}
}

// finish refreshes the times of last sight of the records and the pools
// that the step saw, when the step changed the records or one of the
// times is older than refresh.
func (s *step) finish(r *state.Records) {
	var times []*time.Time
	for _, serial := range s.seen {
		if p := recordOf(r, serial); p != nil && recorded(p) {
			times = append(times, &p.LastSeen)
		}
	}
	for _, pool := range s.seenPools {
		if i := slices.IndexFunc(r.ListedPools, func(lp state.ListedPool) bool { return lp.Pool == pool }); i >= 0 {
			times = append(times, &r.ListedPools[i].Listed)
		}
	}
	stale := slices.ContainsFunc(times, func(t *time.Time) bool { return s.now.Sub(*t) >= refresh })
	if !s.changed && !stale {
		return
	}
	for _, t := range times {
		*t = s.now
	}
}

// run applies fn and the refresh of the times of the step s to r.
func (s *step) run(r *state.Records, fn stepFunc) {
	fn(s, r)
	s.finish(r)
}

// records returns the provenance records: of the store's snapshot, or in
// memory.
func (w *Watcher) records() *state.Records {
	if w.st != nil {
		return &w.st.Snapshot().Records
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mem
}

// commit applies fn to the records in a commit of durability d: through the
// store, or to a copy of the records in memory that replaces them.
func (w *Watcher) commit(d store.Durability, fn func(r *state.Records) error) error {
	if w.st == nil {
		w.mu.Lock()
		defer w.mu.Unlock()
		next := sections(w.mem)
		if err := fn(next); err != nil {
			return err
		}
		next.Sort()
		if len(next.Provenance) == 0 {
			next.Provenance = nil
		}
		if len(next.ListedPools) == 0 {
			next.ListedPools = nil
		}
		w.mem = next
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), commitTimeout)
	defer cancel()
	return w.storeCommit(ctx, d, func(doc *store.Document) error {
		return fn(&doc.Records)
	})
}

// commitStep applies the step fn of the kind at now to the records in one
// commit, and returns what it did and whether it is committed. A dry run on
// r0, the records read before, decides: a step that changes nothing is not
// committed; the commit is durable if a change of the dry run must be, and
// applied again durably if a lazy one finds such a change. The lines of the
// step are logged once the store applied it, a lazy step's before its
// write. A failed commit changes nothing: the dry run is returned without
// its lines and effects, and the watch scans again at each tick until a
// step of the kind is committed.
func (w *Watcher) commitStep(kind string, now time.Time, r0 *state.Records, fn stepFunc) (*step, bool) {
	dry := &step{now: now}
	work := sections(r0)
	dry.run(work, fn)
	if reflect.DeepEqual(work.Provenance, r0.Provenance) && reflect.DeepEqual(work.ListedPools, r0.ListedPools) {
		w.committed(kind, dry)
		return dry, true
	}
	var s *step
	apply := func(d store.Durability) func(r *state.Records) error {
		return func(r *state.Records) error {
			s = &step{now: now}
			s.run(r, fn)
			if s.durable && d == store.Lazy {
				return errDurable
			}
			return nil
		}
	}
	d := store.Lazy
	if dry.durable {
		d = store.Durable
	}
	err := w.commit(d, apply(d))
	if errors.Is(err, errDurable) {
		err = w.commit(store.Durable, apply(store.Durable))
	}
	if err != nil {
		klog.Errorf("provenance: writing %s: %v", w.opts.Name, err)
		w.mu.Lock()
		w.uncommitted[kind] = true
		w.mu.Unlock()
		return dry, false
	}
	w.committed(kind, s)
	return s, true
}

// committed takes the effects of the committed step s of the kind on the
// memory of the watcher and logs its lines.
func (w *Watcher) committed(kind string, s *step) {
	lines := s.lines
	w.mu.Lock()
	delete(w.uncommitted, kind)
	for _, serial := range s.forgotten {
		delete(w.absent, serial)
	}
	for _, pool := range s.poolsGone {
		delete(w.poolAbsent, pool)
	}
	for _, c := range s.conflicts {
		if w.conflicts[c.serial] {
			continue
		}
		w.conflicts[c.serial] = true
		lines = append(lines, func() {
			klog.Errorf("provenance: memdev 0x%x (%s) is a pool device, but local claims use it: it is no longer published, and no local claim is prepared on it; release the local claims (%v) and check its memory",
				c.serial, c.name, c.claims)
		})
	}
	w.mu.Unlock()
	for _, line := range lines {
		line()
	}
}

// open opens the records of the store: the line with their counts, or the
// error of a LOST store, whose records are lost: lost is set, which holds
// new memdevs for MissingWindow, and the records are kept in memory.
func (w *Watcher) open() {
	st := w.opts.Store
	if st == nil {
		return
	}
	if err := st.Lost(); err != nil {
		w.lost = err.Error()
		klog.Errorf("provenance: %s: %v: the remembered provenance is lost; new memdevs are held for up to %v each in the next %v",
			w.opts.Name, err, MaxHold, MissingWindow)
		return
	}
	w.st, w.storeCommit = st, st.Commit
	r := w.records()
	held := 0
	for i := range r.Provenance {
		if r.Provenance[i].State == state.ProvenanceHeld {
			held++
		}
	}
	klog.Infof("provenance: loaded %d serials (%d of pool devices) and %d pools from %s",
		len(r.Provenance)-held, len(poolSerialsOf(r)), len(r.ListedPools), w.opts.Name)
	if held > 0 {
		klog.Infof("provenance: %d memdevs held before the restart stay held", held)
	}
}

// forget removes the records of the serials, as the operator orders with
// --forget-provenance.
func (w *Watcher) forget(serials []uint64) {
	if len(serials) == 0 {
		return
	}
	w.commitStep("forget", w.opts.Now(), w.records(), func(s *step, r *state.Records) {
		for _, serial := range serials {
			s.forget(r, serial)
		}
	})
}

// observePoolsLocked observes the absence of the pools of r that the list
// at now does not list, and returns those observed absent for Retention.
// Called with w.mu held.
func (w *Watcher) observePoolsLocked(r *state.Records, listed map[string]bool, now time.Time) []poolGone {
	continuous := observedSince(w.lastList, now)
	for pool := range listed {
		delete(w.poolAbsent, pool)
	}
	var gone []poolGone
	for _, lp := range r.ListedPools {
		if listed[lp.Pool] {
			continue
		}
		since, ok := w.poolAbsent[lp.Pool]
		switch {
		case !ok || !continuous:
			w.poolAbsent[lp.Pool] = now
		case now.Sub(since) > Retention:
			gone = append(gone, poolGone{pool: lp.Pool, since: since})
		}
	}
	w.lastList = now
	return gone
}

// observedSince tells whether an observation at now continues the one at
// last: there was one, and no more than observationGap ago.
func observedSince(last, now time.Time) bool {
	return !last.IsZero() && !now.Before(last) && now.Sub(last) <= observationGap
}

// orPrepared returns where a pool serial was seen for the log lines.
func orPrepared(pool string) string {
	if pool == "" {
		return "prepared by the pool driver"
	}
	return "in a ResourceSlice of " + pool
}

// uncertainLocked returns why a new memdev cannot be taken as local now but
// may be after MaxHold, and its reason: the list fails, pools of r have
// been observed without ResourceSlices for less than MissingWindow, or the
// records were lost less than MissingWindow ago. "" if it can. Called with
// w.mu held.
func (w *Watcher) uncertainLocked(r *state.Records, now time.Time) (string, state.HeldReason) {
	if w.failing || !w.known {
		return "the ResourceSlices of pool controllers cannot be listed", state.HeldListFailing
	}
	var why []string
	var reason state.HeldReason
	if w.lost != "" {
		if w.lostAt.IsZero() {
			w.lostAt = now
		}
		if now.Sub(w.lostAt) < MissingWindow {
			why = append(why, fmt.Sprintf("the remembered provenance is lost (%s)", w.lost))
			reason = state.HeldLost
		}
	}
	var missing []string
	for _, lp := range r.ListedPools {
		if since, ok := w.poolAbsent[lp.Pool]; ok && !w.listed[lp.Pool] && now.Sub(since) < MissingWindow {
			missing = append(missing, lp.Pool)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		why = append(why, "pools without ResourceSlices: "+strings.Join(missing, ", "))
		if reason == "" {
			reason = state.HeldPoolMissing
		}
	}
	return strings.Join(why, "; "), reason
}

// Gate observes the memdevs of a snapshot. It returns the serials of pool
// devices (those of poolSerials, the POOL records and those in the status of
// pool claims of the node) and the new memdevs that are HELD, which the scan
// does not publish, and makes LOCAL the memdevs that it takes as local. local
// has the claims of the local memdevs that the device engine has records of,
// by serial: they are local unless they are serials of pool devices.
func (w *Watcher) Gate(ctx context.Context, snap *kernel.Snapshot, poolSerials map[uint64]bool, local map[uint64][]string) (map[uint64]bool, map[uint64]bool) {
	if w == nil || snap == nil {
		return poolSerials, nil
	}
	r0 := w.records()
	ev, evErr := w.evidence(ctx, r0, snap, poolSerials, local)
	g := &gateScan{memdevs: snap.Memdevs, ev: ev, poolSerials: poolSerials, local: local}
	present := map[uint64]bool{}
	for i := range snap.Memdevs {
		present[snap.Memdevs[i].Serial] = true
	}
	now := w.opts.Now()
	w.mu.Lock()
	g.known = w.known
	g.uncertain, g.reason = w.uncertainLocked(r0, now)
	if evErr != nil && g.uncertain == "" {
		g.uncertain, g.reason = "the pool claims of the node cannot be listed", state.HeldClaimsNotListable
	}
	for serial := range present {
		delete(w.absent, serial)
	}
	g.expired = w.observeAbsenceLocked(r0, present, now)
	w.mu.Unlock()
	s, ok := w.commitStep("observation", now, r0, g.apply)
	if !ok {
		g.withhold(s, r0)
	}
	return s.pool, s.held
}

// gateScan is one observation of the memdevs of a scan: the memdevs, the
// serials of pool devices and the local uses that the scan has, the evidence
// of the pool claims of the node (nil when they were not listed), whether a
// list succeeded, the reason for doubt of a new memdev ("" for none) and the
// recorded serials observed absent for Retention, with the start of their
// absence.
type gateScan struct {
	memdevs     []kernel.MemdevInfo
	ev          *claimEvidence
	poolSerials map[uint64]bool
	local       map[uint64][]string
	known       bool
	uncertain   string
	reason      state.HeldReason
	expired     map[uint64]time.Time
}

// apply applies the observation g to the records r in the step s: each
// memdev observed, the records of the serials absent for Retention and
// the HELD ones of memdevs that are gone removed.
func (g *gateScan) apply(s *step, r *state.Records) {
	s.pool, s.held = maps.Clone(g.poolSerials), map[uint64]bool{}
	if s.pool == nil {
		s.pool = map[uint64]bool{}
	}
	present := map[uint64]bool{}
	for i := range g.memdevs {
		m := &g.memdevs[i]
		present[m.Serial] = true
		pool, held := g.observe(s, r, m)
		if pool {
			s.pool[m.Serial] = true
		}
		if held {
			s.held[m.Serial] = true
		}
	}
	for _, serial := range slices.Sorted(maps.Keys(g.expired)) {
		s.expireAbsent(r, serial, g.expired[serial])
	}
	var gone []uint64
	for i := range r.Provenance {
		if p := &r.Provenance[i]; p.State == state.ProvenanceHeld && !present[uint64(p.Serial)] {
			gone = append(gone, uint64(p.Serial))
		}
	}
	for _, serial := range gone {
		if s.remove(r, serial, triggerObserve) {
			s.durable = true
		}
	}
}

// withhold adds to the held memdevs of the step s, whose commit failed,
// those without a POOL or LOCAL record in r that s does not take as pool
// devices: a new memdev or the end of a hold is not published before its
// commit.
func (g *gateScan) withhold(s *step, r *state.Records) {
	for i := range g.memdevs {
		serial := g.memdevs[i].Serial
		if p := recordOf(r, serial); !s.pool[serial] && (p == nil || !recorded(p)) {
			s.held[serial] = true
		}
	}
}

// observe observes the memdev m on the node in the step s: a pool device
// (pool), a new memdev that stays HELD (held), else a local memdev.
func (g *gateScan) observe(s *step, r *state.Records, m *kernel.MemdevInfo) (pool, held bool) {
	serial := m.Serial
	p := recordOf(r, serial)
	claimPool, inClaim := "", false
	if g.ev != nil {
		claimPool, inClaim = g.ev.serials[serial]
	}
	claims, uses := g.local[serial]
	switch {
	case g.poolSerials[serial] || (p != nil && p.State == state.ProvenancePool) || inClaim:
		s.notePool(r, serial, claimPool, triggerList)
		if uses {
			s.conflicts = append(s.conflicts, conflict{serial: serial, name: m.Name, claims: claims})
		}
		return true, false
	case (p != nil && recorded(p)) || uses:
		s.noteLocal(r, m, triggerObserve)
		return false, false
	}
	return false, g.observeNew(s, r, m, p)
}

// observeNew observes a new memdev m (its record p, nil or HELD) in the
// step s, and tells whether it stays HELD: while there is a reason for
// doubt; else it is LOCAL.
func (g *gateScan) observeNew(s *step, r *state.Records, m *kernel.MemdevInfo, p *state.Provenance) bool {
	why, reason := g.uncertain, g.reason
	switch {
	case !g.known:
		why, reason = "the ResourceSlices of pool controllers were never listed", state.HeldNeverListed
	case g.ev != nil && len(g.ev.attaching) > 0:
		why, reason = "pool claims of the node are not attached yet: "+strings.Join(g.ev.attaching, ", "), state.HeldAttaching
	}
	name, serial := m.Name, m.Serial
	if why == "" {
		if p != nil && p.State == state.ProvenanceHeld {
			s.log(func() {
				klog.Infof("provenance: publishing memdev 0x%x (%s) as a local memdev: no pool is missing now", serial, name)
			})
		}
		s.noteLocal(r, m, triggerObserve)
		return false
	}
	if s.hold(r, m, p, why, reason) {
		return true
	}
	s.log(func() {
		klog.Warningf("provenance: publishing memdev 0x%x (%s) as a local memdev after holding it for %v (%s)",
			serial, name, MaxHold, why)
	})
	s.noteLocal(r, m, triggerExpire)
	return false
}

// evidence lists the pool claims of the node when a memdev of snap is new
// (no pool serial, no record in r, no local use).
func (w *Watcher) evidence(ctx context.Context, r *state.Records, snap *kernel.Snapshot, poolSerials map[uint64]bool, local map[uint64][]string) (*claimEvidence, error) {
	isNew := func(m kernel.MemdevInfo) bool {
		p := recordOf(r, m.Serial)
		_, uses := local[m.Serial]
		return !poolSerials[m.Serial] && (p == nil || !recorded(p)) && !uses
	}
	if !slices.ContainsFunc(snap.Memdevs, isNew) || w.opts.NodeName == "" {
		return nil, nil
	}
	ev, err := w.claimEvidence(ctx)
	if err != nil {
		klog.Warningf("provenance: listing the pool claims of the node: %v", err)
	}
	return ev, err
}

// observeAbsenceLocked observes the absence of the recorded serials of r
// that are not on the node (present), not in the pool slices of the last
// list and not prepared, when that list is current, and returns those
// observed absent for Retention with the start of their absence. Called
// with w.mu held.
func (w *Watcher) observeAbsenceLocked(r *state.Records, present map[uint64]bool, now time.Time) map[uint64]time.Time {
	if !w.known || w.failing || !observedSince(w.lastList, now) {
		return nil // the slices are not observed now
	}
	continuous := observedSince(w.lastObs, now)
	w.lastObs = now
	expired := map[uint64]time.Time{}
	for i := range r.Provenance {
		p := &r.Provenance[i]
		s := uint64(p.Serial)
		if !recorded(p) {
			continue
		}
		if _, listed := w.serials[s]; present[s] || listed || w.prepared[s] {
			delete(w.absent, s)
			continue
		}
		since, ok := w.absent[s]
		switch {
		case !ok || !continuous:
			w.absent[s] = now
		case now.Sub(since) > Retention:
			expired[s] = since
		}
	}
	return expired
}
