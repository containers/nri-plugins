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

package main

// Decoder stacks (contract A9): Linux allocates the HDM decoders
// of a CXL port as a stack. Per (VM, host bridge) the model keeps the
// regions through it in creation order: a new region is pushed (only if
// there is no hole), destroying the top pops it and every hole below,
// destroying another one leaves a hole. A Prepare that needs a new
// region through a stack with a hole fails: the pod is blocked until
// the hole clears.
//
// Region lifetimes on a VM: an exclusive pool device = its claim; a
// shared pool device = its first to last claim on the VM; the local
// memdev = its first to last slice (pods and leases). Users prepare and
// unprepare regions; the transitions record these events, and the
// settle plays them in every order that can happen (async operations of
// one window race for the order) to get the outcomes.

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// RKey is a region of a stack: the local memdev or a pool device.
type RKey int8

const (
	rkHole  RKey = -1
	rkLocal RKey = 0
)

func devKey(d DevID) RKey { return RKey(1 + d) }

func (k RKey) String() string {
	switch k {
	case rkHole:
		return "hole"
	case rkLocal:
		return "local"
	}
	return DevID(k - 1).String()
}

// kind is the abstraction of a key in C4: local, pooled or shared.
func (k RKey) kind() string {
	switch {
	case k == rkHole:
		return "hole"
	case k == rkLocal:
		return "local"
	case devices[k-1].Shared:
		return "shared"
	}
	return "pooled"
}

const maxStack = 8

// Stack is the decoder stack of one host bridge of one VM.
type Stack struct {
	N int8
	E [maxStack]RKey
}

const nStacks = nVMs * nBridges

func stackIndex(v VM, b Bridge) int { return int(v)*nBridges + int(b) }

func stackVM(si int) VM         { return VM(si / nBridges) }
func stackBridge(si int) Bridge { return Bridge(si % nBridges) }

func stackName(si int) string {
	return stackVM(si).String() + "/hb" + string(rune('0'+stackBridge(si)))
}

func (s *Stack) hasHole() bool {
	for i := int8(0); i < s.N; i++ {
		if s.E[i] == rkHole {
			return true
		}
	}
	return false
}

func (s *Stack) holes() int {
	n := 0
	for i := int8(0); i < s.N; i++ {
		if s.E[i] == rkHole {
			n++
		}
	}
	return n
}

func (s *Stack) contains(k RKey) bool {
	for i := int8(0); i < s.N; i++ {
		if s.E[i] == k {
			return true
		}
	}
	return false
}

func (s *Stack) top() RKey {
	if s.N == 0 {
		return rkHole
	}
	return s.E[s.N-1]
}

func (s *Stack) push(k RKey) {
	if s.N < maxStack {
		s.E[s.N] = k
		s.N++
	}
}

// destroy destroys the region of a key: the top is popped with every
// hole below it, another one becomes a hole.
func (s *Stack) destroy(k RKey) {
	for i := s.N - 1; i >= 0; i-- {
		if s.E[i] != k {
			continue
		}
		if i == s.N-1 {
			s.N--
			s.E[s.N] = 0
			for s.N > 0 && s.E[s.N-1] == rkHole {
				s.N--
				s.E[s.N] = 0
			}
		} else {
			s.E[i] = rkHole
		}
		return
	}
}

// equal compares the entries of two stacks.
func (s *Stack) equal(o *Stack) bool {
	if s.N != o.N {
		return false
	}
	for i := int8(0); i < s.N; i++ {
		if s.E[i] != o.E[i] {
			return false
		}
	}
	return true
}

// keys returns the entries.
func (s *Stack) keys() []RKey { return append([]RKey(nil), s.E[:s.N]...) }

func (s Stack) String() string {
	parts := make([]string, 0, s.N)
	for i := int8(0); i < s.N; i++ {
		parts = append(parts, s.E[i].String())
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// --- users and events ---

// A user of a region is the claim of a slot's pod (user = slot index) or
// the companion slice of the lease of a lease slot (user = nSlots +
// slot index). A dnuma pod has two claims (A39): its pool claim is the
// slot's user, its local slice the second one (nSlots + slot index).
func (s *State) secondUser(i int) int16 { return int16(len(s.Slots) + i) }

// claimUsers are the users of the claims of the pod of slot i: the slot,
// and for a dnuma pod its local claim.
func (s *State) claimUsers(i int) []int16 {
	if s.slot(i).Kind.dual() {
		return []int16{int16(i), s.secondUser(i)}
	}
	return []int16{int16(i)}
}

// dualUser returns the user of the local claim of a dnuma slot whose pool
// claim is user u, -1 for other users.
func (s *State) dualUser(u int16) int16 {
	if int(u) < len(s.Slots) && s.slot(int(u)).Kind.dual() {
		return s.secondUser(int(u))
	}
	return -1
}

// userSlot returns the slot of a user and whether it is a lease.
func (s *State) userSlot(u int16) (int, bool) {
	if int(u) >= len(s.Slots) {
		return int(u) - len(s.Slots), true
	}
	return int(u), false
}

// userRegion returns the stack and region of a user on its node, false
// if the user has no region (plain, lease claims) or no known node.
func (s *State) userRegion(u int16) (int, RKey, bool) {
	i, lease := s.userSlot(u)
	sl := s.slot(i)
	if lease {
		switch {
		case sl.Kind == KLease:
			return stackIndex(sl.VM, HB0), rkLocal, true
		case sl.Kind == KPlse && s.Slots[i].LeaseMode.persistent():
			// the companion of a persistent lease: c-pmem0 on the
			// requester's node
			return stackIndex(sl.VM, HB1), devKey(cPmem0), true
		case sl.Kind.dual():
			// the local claim of a dnuma pod
			return stackIndex(sl.VM, HB0), rkLocal, true
		}
		return 0, 0, false
	}
	v := s.Slots[i].Node
	switch {
	case v == VMAny:
		return 0, 0, false
	case sl.Kind.pooled():
		return stackIndex(v, poolBridge[sl.Pool]), devKey(sl.Dev), true
	case sl.Kind.local():
		return stackIndex(v, HB0), rkLocal, true
	}
	return 0, 0, false
}

type evKind uint8

const (
	evPrepare evKind = iota
	evUnprepare
)

// stackEvent is a prepare or unprepare of a user in the current window.
type stackEvent struct {
	Stack int8
	Key   RKey
	User  int16
	Kind  evKind
	Opt   bool  // a prepare that may not happen (a controller kill)
	Sync  bool  // done before the next action was sent (lease-get)
	Seq   int16 // issue order
	After int16 // happens after the event of this Seq (a handover), -1
	// Handover: the claim got its device only when the earlier
	// holders of it were gone: its prepare comes after every earlier
	// unprepare of the same region (the scheduler
	// allocates it after their claims are deallocated, which is after
	// their Unprepare)
	Handover bool
	// Probe: the prepare of a claim whose Prepare is refused (A25): it
	// needs the region (Blocked by a hole like any other), its rollback
	// destroys it again at the top: no change of the stack
	Probe bool
}

// addEvent records an event of a user (copy on write). The stack of an
// "any" pod whose node is not known yet is -1 (the settle tries every
// host).
func (n *State) addEvent(u int16, kind evKind, sync bool, after int16) int16 {
	si, k, ok := n.userRegion(u)
	if !ok {
		i, lease := n.userSlot(u)
		sl := n.slot(i)
		if lease || !sl.Kind.pooled() || n.Slots[i].Node != VMAny {
			return -1
		}
		si, k = -1, devKey(sl.Dev)
	}
	seq := int16(len(n.Ev))
	n.Ev = append(n.Ev[:len(n.Ev):len(n.Ev)], stackEvent{Stack: int8(si), Key: k, User: u, Kind: kind, Sync: sync, Seq: seq, After: after})
	return seq
}

// setHandover marks the event of a seq as a handover.
func (n *State) setHandover(seq int16) {
	if seq < 0 || int(seq) >= len(n.Ev) {
		return
	}
	n.Ev = append([]stackEvent(nil), n.Ev...)
	n.Ev[seq].Handover = true
}

// hasPrepare tells if a user has a prepare event in this window.
func (s *State) hasPrepare(u int16) bool {
	for _, e := range s.Ev {
		if e.User == u && e.Kind == evPrepare {
			return true
		}
	}
	return false
}

// hasUnprepare tells if a user has an unprepare event in this window.
func (s *State) hasUnprepare(u int16) bool {
	for _, e := range s.Ev {
		if e.User == u && e.Kind == evUnprepare {
			return true
		}
	}
	return false
}

// setOptional makes the prepare events of a user optional (of both
// claims of a dnuma pod).
func (n *State) setOptional(u int16) {
	lu := n.dualUser(u)
	for j := range n.Ev {
		if (n.Ev[j].User == u || (lu >= 0 && n.Ev[j].User == lu)) && n.Ev[j].Kind == evPrepare && !n.Ev[j].Opt {
			n.Ev = append([]stackEvent(nil), n.Ev...)
			n.Ev[j].Opt = true
		}
	}
}

// eventsOn returns the events of a stack.
func (s *State) eventsOn(si int) []stackEvent {
	var out []stackEvent
	for _, e := range s.Ev {
		if int(e.Stack) == si {
			out = append(out, e)
		}
	}
	return out
}

// preparedAtStart tells if a user had its region at the start of the
// window (before its events).
func (s *State) preparedAtStart(u int16) bool {
	i, lease := s.userSlot(u)
	ss := &s.Slots[i]
	if lease && !s.slot(i).Kind.dual() {
		return (ss.Lease == LeaseHeld || ss.Lease == LeaseReleasing || ss.Lease == LeaseBusy) && !s.hasPrepare(u)
	}
	if s.hasPrepare(u) {
		return false
	}
	switch ss.Phase {
	case Running, Restarting:
		return true
	case Deleting:
		return ss.Prev == Running || ss.Prev == Restarting
	}
	return false
}

// blockedAtStart tells if a user was blocked at the start of the window
// and still holds its claim (it keeps retrying).
func (s *State) blockedAtStart(u int16) bool {
	i, lease := s.userSlot(u)
	return !lease && s.Slots[i].Phase == Blocked
}

// stackOutcome is what a window did to a stack.
type stackOutcome struct {
	S        Stack
	Prepared map[int16]bool // users with their region at the end
	Blocked  map[int16]bool // users whose prepare failed and that wait
	Cleared  bool           // the stack had a hole, it has none at the end
	Pushed   bool           // a region was made in the window
}

func sortedUsers(m map[int16]bool) []int16 {
	us := make([]int16, 0, len(m))
	for u, on := range m {
		if on {
			us = append(us, u)
		}
	}
	sort.Slice(us, func(i, j int) bool { return us[i] < us[j] })
	return us
}

// maxEvents is the most events of one stack whose orders are enumerated
// (with memoization); more are played in issue order only, and the
// settle says so (Truncated).
const maxEvents = 24

// userSet is a set of users (slots and leases of up to 96 slots).
type userSet [3]uint64

func (u *userSet) has(x int16) bool { return u[x/64]&(1<<uint(x%64)) != 0 }
func (u *userSet) set(x int16)      { u[x/64] |= 1 << uint(x%64) }
func (u *userSet) clear(x int16)    { u[x/64] &^= 1 << uint(x%64) }

func (u *userSet) toMap() map[int16]bool {
	m := map[int16]bool{}
	for x := int16(0); x < 192; x++ {
		if u.has(x) {
			m[x] = true
		}
	}
	return m
}

// playState is the state of a stack while the events of a window are
// played.
type playState struct {
	S        Stack
	ref      [1 + nDevs]int8
	prepared userSet
	blocked  userSet
	pushed   bool
}

func (p *playState) apply(e *stackEvent) {
	switch e.Kind {
	case evPrepare:
		switch {
		case e.Probe:
			if p.ref[e.Key] == 0 && p.S.hasHole() {
				p.blocked.set(e.User)
			}
		case p.ref[e.Key] > 0:
			p.ref[e.Key]++
			p.prepared.set(e.User)
		case p.S.hasHole():
			p.blocked.set(e.User)
		default:
			p.S.push(e.Key)
			p.ref[e.Key] = 1
			p.prepared.set(e.User)
			p.pushed = true
		}
	case evUnprepare:
		p.blocked.clear(e.User)
		if p.prepared.has(e.User) {
			p.prepared.clear(e.User)
			p.ref[e.Key]--
			if p.ref[e.Key] == 0 {
				p.S.destroy(e.Key)
			}
		}
	}
}

// outcomes returns the distinct outcomes of the window on a stack, the
// one of the issue order (every optional prepare done) first, and
// whether the enumeration was cut (too many events: issue order only).
func (s *State) outcomes(si int) []*stackOutcome {
	outs, _ := s.outcomesCut(si)
	return outs
}

func (s *State) outcomesCut(si int) ([]*stackOutcome, bool) {
	evs := s.eventsOn(si)
	var start playState
	start.S = s.Stacks[si]
	// every user of the stack, also those without events (none after a
	// node-reboot of its VM in this window: nothing is prepared at the
	// boot)
	booted := s.rebooted(si)
	for u := int16(0); int(u) < 2*len(s.Slots) && !booted; u++ {
		if ssi, k, ok := s.userRegion(u); ok && ssi == si {
			if s.preparedAtStart(u) {
				start.prepared.set(u)
				start.ref[k]++
			}
			if s.blockedAtStart(u) {
				start.blocked.set(u)
			}
		}
	}
	hadHole := start.S.hasHole()
	var outs []*stackOutcome
	// (the stack entries above N are always zero: equal stacks are
	// equal structs, like their strings in stackOutcome.key)
	type outKey struct {
		S                 Stack
		prepared, blocked userSet
	}
	seen := map[outKey]bool{}
	add := func(p *playState) {
		k := outKey{p.S, p.prepared, p.blocked}
		if seen[k] {
			return
		}
		seen[k] = true
		outs = append(outs, &stackOutcome{S: p.S, Prepared: p.prepared.toMap(), Blocked: p.blocked.toMap(),
			Cleared: hadHole && !p.S.hasHole(), Pushed: p.pushed})
	}
	// the issue order first
	issue := start
	for j := range evs {
		issue.apply(&evs[j])
	}
	add(&issue)
	if len(evs) == 0 || (booted && s.Reorder&(1<<uint(si)) != 0) {
		// (a stack rebuilt by a node-reboot: every order of its
		// prepares is an outcome, with the same regions and users; the
		// oracle adopts the observed order, Reorder)
		return outs, false
	}
	if len(evs) > maxEvents {
		return outs, true
	}
	bySeq := map[int16]int{}
	for j, e := range evs {
		bySeq[e.Seq] = j
	}
	full := uint32(1)<<uint(len(evs)) - 1
	type memoK struct {
		mask uint32
		p    playState
	}
	memo := map[memoK]bool{}
	var dfs func(mask uint32, p playState)
	dfs = func(mask uint32, p playState) {
		if mask == full {
			add(&p)
			return
		}
		k := memoK{mask, p}
		if memo[k] {
			return
		}
		memo[k] = true
		for j := range evs {
			bit := uint32(1) << uint(j)
			if mask&bit != 0 || !ready(evs, mask, bySeq, j) {
				continue
			}
			q := p
			q.apply(&evs[j])
			dfs(mask|bit, q)
			if evs[j].Opt {
				// the optional prepare does not happen
				dfs(mask|bit, p)
			}
		}
	}
	dfs(0, start)
	return outs, false
}

// ready tells if event j may come next (mask: the events placed): sync
// events issued before it, the event it comes after, the prepare of its
// user and, for a handover, the earlier unprepares of its region are
// placed.
func ready(evs []stackEvent, mask uint32, bySeq map[int16]int, j int) bool {
	e := &evs[j]
	for x := range evs {
		f := &evs[x]
		if x == j || mask&(1<<uint(x)) != 0 {
			continue
		}
		// a sync event (lease-get) was done before anything issued
		// after it; async events issued before it may come later
		if f.Sync && f.Seq < e.Seq {
			return false
		}
		if e.Kind == evUnprepare && f.User == e.User && f.Kind == evPrepare {
			return false
		}
		if e.Kind == evPrepare && e.Handover && f.Kind == evUnprepare && f.Key == e.Key && f.Seq < e.Seq {
			return false
		}
	}
	if e.After >= 0 {
		if x, ok := bySeq[e.After]; ok && mask&(1<<uint(x)) == 0 {
			return false
		}
	}
	return true
}

// unblockingOn tells if a stack has a user that waits for kubelet's retry
// or the scheduler's requeue (a stalled contender, also of an "any" slot)
// after its hole cleared.
func (s *State) unblockingOn(si int) bool {
	for i := range s.Slots {
		if !s.Slots[i].Phase.waitsRetry() {
			continue
		}
		if ssi, _, ok := s.userRegion(int16(i)); ok && ssi == si {
			return true
		}
		// (a stalled dnuma pod gets its local slice with its promotion)
		if lu := s.dualUser(int16(i)); lu >= 0 {
			if ssi, _, ok := s.userRegion(lu); ok && ssi == si {
				return true
			}
		}
		// an "any" pod whose node is not known yet: any host of its
		// pool
		if sl := s.slot(i); sl.Kind.pooled() && s.Slots[i].Node == VMAny && poolBridge[sl.Pool] == stackBridge(si) && poolHasHost(sl.Pool, stackVM(si)) {
			return true
		}
	}
	return false
}

// setRequired makes the prepare events of a user happen for sure (its
// controller is up again; both claims of a dnuma pod).
func (n *State) setRequired(u int16) {
	lu := n.dualUser(u)
	for j := range n.Ev {
		if (n.Ev[j].User == u || (lu >= 0 && n.Ev[j].User == lu)) && n.Ev[j].Kind == evPrepare && n.Ev[j].Opt {
			n.Ev = append([]stackEvent(nil), n.Ev...)
			n.Ev[j].Opt = false
		}
	}
}

// maxAlts bounds the outcomes of one settle.
const maxAlts = 64

// settleAll returns the settled state of a window, and in its Alts the
// other outcomes that its races for the decoder stacks (and the node of
// an "any" pod) can have. The first state is the issue order with the
// first host of every "any" pod.
func (n *State) settleAll() *State {
	var alts []*State
	seen := map[string]bool{}
	truncated := false
	var bases []*State
	// (the worlds of the watchers that may have fired earlier in the
	// window (A32) come last: the alternatives of a settle keep their
	// order, and the "#adopt N alt=J" of older steps files their meaning)
	for _, late := range []bool{false, true} {
		for _, k := range n.expandLeaseOpen() {
			// (a watcher fires in the worlds of the slices and of the
			// hosts of the "any" pods where a Prepare on its VM may
			// show its marker (A32); the order of the worlds is as
			// before)
			for _, fired := range k.killWorlds(late) {
				for _, w := range k.expandResourceSlices() {
					for _, h := range w.expandAnyNodes() {
						if f := h.fireKillAt(fired, late); f != nil {
							bases = append(bases, f)
						}
					}
				}
			}
		}
	}
	for _, b := range bases {
		var sis []int
		var outs [][]*stackOutcome
		for si := 0; si < nStacks; si++ {
			if len(b.eventsOn(si)) == 0 && !b.blockedOn(si) {
				continue
			}
			o, cut := b.outcomesCut(si)
			truncated = truncated || cut
			sis = append(sis, si)
			outs = append(outs, o)
		}
		idx := make([]int, len(sis))
		for {
			a := b.clone()
			for j, si := range sis {
				a.applyOutcome(si, outs[j][idx[j]])
			}
			if a.applyDual(b, sis, outs, idx) {
				a.settle()
				if k := a.altKey(); !seen[k] {
					seen[k] = true
					alts = append(alts, a)
				}
			}
			if len(alts) >= maxAlts {
				truncated = true
				break
			}
			// the next combination
			j := len(idx) - 1
			for ; j >= 0; j-- {
				idx[j]++
				if idx[j] < len(outs[j]) {
					break
				}
				idx[j] = 0
			}
			if j < 0 {
				break
			}
		}
		if len(alts) >= maxAlts {
			break
		}
	}
	if len(alts) == 0 {
		// (cannot happen: the issue order prepares both claims of a
		// dnuma pod, or one is blocked)
		a := bases[0].clone()
		a.settle()
		alts = append(alts, a)
	}
	p := alts[0]
	p.Alts = alts[1:]
	p.Truncated = truncated
	if truncated && (len(n.unplacedAny()) > 0 || len(n.optPrepared()) > 0) {
		// (the hosts of its "any" pods and the optional Prepares of
		// its race-kill pods multiply its worlds: the oracle settles it
		// again with the nodes and the bindings it observes)
		p.Wide = n
	}
	return p
}

// optPrepared returns the slots whose Prepares in this window are
// optional (Opt: a controller kill came while they were attaching; both
// claims of a dnuma pod), in slot order.
func (n *State) optPrepared() []int {
	var out []int
	for _, e := range n.Ev {
		if !e.Opt || e.Kind != evPrepare {
			continue
		}
		i, lease := n.userSlot(e.User)
		if lease && !n.slot(i).Kind.dual() {
			continue
		}
		if !slices.Contains(out, i) {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// dropPrepares removes the prepare events of user u (and of the local
// claim of a dnuma pod): the world in which its optional Prepare does
// not come (copy on write; an event After one removed is free).
func (n *State) dropPrepares(u int16) {
	lu := n.dualUser(u)
	ev := make([]stackEvent, 0, len(n.Ev))
	for _, e := range n.Ev {
		if (e.User == u || (lu >= 0 && e.User == lu)) && e.Kind == evPrepare {
			continue
		}
		ev = append(ev, e)
	}
	n.Ev = ev
}

// unplacedAny returns the "any" pods that prepare in this window on a
// node that is not known yet, in slot order.
func (n *State) unplacedAny() []int {
	var anys []int
	for _, e := range n.Ev {
		if e.Stack >= 0 {
			continue
		}
		if i, lease := n.userSlot(e.User); !lease && !slices.Contains(anys, i) {
			anys = append(anys, i)
		}
	}
	sort.Ints(anys)
	return anys
}

// narrowed settles a truncated settle (the outcome bound, A35) again with the nodes of some
// of its "any" pods known (observed bound), and whether some of its
// race-kill pods are bound (observed bound or Attached: their optional
// Prepares come; neither: they do not): settleAll's worlds are those of
// the full enumeration with these facts, fewer of them, so that the
// races on their stacks are enumerated (fuzz, fullp seed 1484: three
// "any" pods prepared at a ctrl-start, 27 host worlds of up to 6 stack
// orders, the 64 outcomes all with the first pod on vm0 or vm1, seen on
// vm2; review fuzz, fullp seed 710078: six race-kill pods, a dnuma one
// of them with two Prepares, on four stacks, the 64 outcomes all with
// the dnuma pod's Prepare, seen not bound). nil if there is no such
// settle or no such fact. The items of Narrow: SLOT:VM, SLOT:bound,
// SLOT:unbound.
func (p *State) narrowed(nodes map[int]VM, bound map[int]bool) *State {
	w := p.Wide
	if w == nil {
		return nil
	}
	c := w.clone()
	c.Ev = append([]stackEvent(nil), w.Ev...)
	var items []string
	for _, i := range w.unplacedAny() {
		v, ok := nodes[i]
		if !ok {
			continue
		}
		sl := c.slot(i)
		c.Slots[i].Node = v
		for j := range c.Ev {
			if c.Ev[j].Stack < 0 && int(c.Ev[j].User) == i {
				c.Ev[j].Stack = int8(stackIndex(v, poolBridge[sl.Pool]))
			}
		}
		items = append(items, sl.Name+":"+v.String())
	}
	for _, i := range w.optPrepared() {
		b, ok := bound[i]
		if !ok {
			continue
		}
		if b {
			c.setRequired(int16(i))
			items = append(items, c.slot(i).Name+":bound")
		} else {
			c.dropPrepares(int16(i))
			items = append(items, c.slot(i).Name+":unbound")
		}
	}
	if len(items) == 0 {
		return nil
	}
	n := c.settleAll()
	n.Narrow = strings.Join(items, ",")
	return n
}

// narrowTo is narrowed by the adoption record's any=SLOT:VM,...
func (p *State) narrowTo(list string) (*State, error) {
	nodes, bound := map[int]VM{}, map[int]bool{}
	for _, item := range strings.Split(list, ",") {
		name, vm, ok := strings.Cut(item, ":")
		i := p.cfg.slotIndex(name)
		if ok && i >= 0 && (vm == "bound" || vm == "unbound") {
			bound[i] = vm == "bound"
			continue
		}
		v, err := parseVM(vm)
		if !ok || i < 0 || err != nil || v == VMAny || !p.cfg.Slots[i].hostOK(v) {
			return nil, fmt.Errorf("invalid node of an \"any\" pod %q", item)
		}
		nodes[i] = v
	}
	n := p.narrowed(nodes, bound)
	if n == nil || n.Narrow != list {
		return nil, fmt.Errorf("no settle to narrow to %s", list)
	}
	return n, nil
}

// expandLeaseOpen returns a state per world of the leases whose
// lease-get failed under a driver-kill-at bind (A29 d, A30): kept (still
// Lending, Ready after kubelet's retry of its companion's Prepare) or
// released (what the cut Prepare made goes).
func (n *State) expandLeaseOpen() []*State {
	states := []*State{n}
	for i := range n.Slots {
		if !n.Slots[i].LeaseOpen {
			continue
		}
		var next []*State
		for _, b := range states {
			kept, released := b.clone(), b.clone()
			kept.Slots[i].LeaseOpen = false
			released.Slots[i].LeaseOpen = false
			released.releasePLease(i)
			next = append(next, kept, released)
		}
		states = next
	}
	return states
}

// killWorlds returns the worlds of the armed driver-kill-at watchers
// (A23), as sets of the VMs whose watcher fired: not fired (still armed:
// it may fire later, until the oracle disarms it 70 s after it saw it),
// and, when a Prepare on the VM may show the marker (killCandidate),
// fired. late: only the worlds in which a watcher that may have fired
// before one of the window's steps (KillMay) fired.
func (n *State) killWorlds(late bool) [][nVMs]bool {
	worlds := [][nVMs]bool{{}}
	for v := VM(0); v < nVMs; v++ {
		d := &n.Drv[v]
		if d.KillAt == 0 || (!late || !d.KillMay) && !n.killCandidate(v, int(d.KillAt-1)) {
			continue
		}
		var next [][nVMs]bool
		for _, w := range worlds {
			f := w
			f[v] = true
			next = append(next, w, f)
		}
		worlds = next
	}
	if !late {
		return worlds
	}
	var out [][nVMs]bool
	for _, w := range worlds {
		for v := VM(0); v < nVMs; v++ {
			if w[v] && n.Drv[v].KillMay {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

// fireKillAt returns the state of a world of the watchers (killWorlds)
// with the hosts of the "any" pods and the slices settled, nil if a
// watcher that fired in it has no Prepare there that may show its
// marker. late: a watcher that could fire before one of the window's
// steps (KillMay) fired also without such a Prepare, and one of them
// did (the other worlds are the first pass's). A watcher that fired
// killed the driver (a driver-kill: restarts +1, fencing seeded,
// steering as after a kill; one more if a kubelet restart may have come
// while its container was being created again, KillSlack); the
// interrupted Prepare is retried after the restart, so the pods end as
// without the kill.
func (n *State) fireKillAt(fired [nVMs]bool, late bool) *State {
	if fired == [nVMs]bool{} {
		return n
	}
	earlier := false
	for v := VM(0); v < nVMs; v++ {
		if !fired[v] || n.killCandidate(v, int(n.Drv[v].KillAt-1)) {
			continue
		}
		if !late || !n.Drv[v].KillMay {
			return nil
		}
		earlier = true
	}
	if late && !earlier {
		return nil
	}
	f := n.clone()
	for v := VM(0); v < nVMs; v++ {
		if !fired[v] {
			continue
		}
		d := &f.Drv[v]
		d.KillAt, d.KillAtAt, d.KillMay = 0, 0, false
		d.Restarts++
		d.Inc++
		d.Killed, d.Creating = true, true
		if d.KillSlack && d.Slack < 255 {
			d.Slack++
		}
		d.KillSlack = false
		f.driverRestarted(v, SteerOff)
	}
	return f
}

// killCandidate: a persistent Prepare on VM v may show the marker in
// the window (A32): one of the window's events there, also of an "any"
// pod not placed yet that may land on v, or a Prepare that comes at any
// time without an event: a stalled contender's promotion, an unblocking
// pod's retry (whole Prepares), a refused claim's retry (makes and rolls
// back its region, which is probed: A25, A31) and a blocked pod's retry
// (its cxl create-region makes the region before the hole refuses it).
// What the watcher sees (T/mbt-observe.py Marker): region, a new pmem
// region; ns, a namespace with a size in a new pmem region (from the
// labels of a device with one, or made by a writer); mount, a new node
// mount (fs claims and fs leases); bind, a new bind of an fs lease.
// Volatile kinds never show one (their regions are ram, ignored).
func (s *State) killCandidate(v VM, marker int) bool {
	if s.killAnyTime(v, marker) {
		return true
	}
	si := stackIndex(v, HB1)
	for _, e := range s.Ev {
		if e.Kind != evPrepare || e.Key < 1 || !devices[e.Key-1].Persistent {
			continue
		}
		i, lease := s.userSlot(e.User)
		if int(e.Stack) != si && (e.Stack >= 0 || lease || !s.mayLandOn(i, v)) {
			continue
		}
		kind := s.slot(i).Kind
		if lease {
			kind = s.Slots[i].LeaseMode.kind()
		}
		stage := prepWhole
		if e.Probe {
			stage = prepProbe
		}
		if s.prepareShows(v, marker, kind, lease, DevID(e.Key-1), stage) {
			return true
		}
	}
	return false
}

// killAnyTime: a Prepare on VM v that comes at any time, without an
// event of the window, may show the marker (killCandidate). It may come
// right after the observation of a settle that saw the watcher armed
// and its claim may be gone by the next settle: A32's refused reader,
// retried by kubelet right after the observation and deleted four steps
// later (apply keeps it in KillMay).
func (s *State) killAnyTime(v VM, marker int) bool {
	for i := range s.Slots {
		ss, sl := &s.Slots[i], s.slot(i)
		if !sl.Kind.persistent() || sl.Kind == KPBad || !s.mayLandOn(i, v) {
			continue
		}
		stage := prepRegion
		switch ss.Phase {
		case Refused:
			stage = prepProbe
		case Blocked:
		case Unblocking, Stalled:
			if ss.Phase == Stalled && !s.Ctrl[sl.Pool].Up {
				// (promoted, it waits for the attach)
				continue
			}
			stage = prepWhole
			if ss.Refuse {
				stage = prepProbe
			}
		default:
			continue
		}
		if s.prepareShows(v, marker, sl.Kind, false, sl.Dev, stage) {
			return true
		}
	}
	return false
}

// how far a Prepare gets (killCandidate): makes the region (blocked by a
// hole: no commit, no nd region), probes it too (refused: rolled back),
// or the whole Prepare
const (
	prepRegion = iota
	prepProbe
	prepWhole
)

// mayLandOn: slot i is on VM v, or an "any" slot that may land there.
func (s *State) mayLandOn(i int, v VM) bool {
	n := s.Slots[i].Node
	return n == v || (n == VMAny && s.slot(i).hostOK(v))
}

// prepareShows: a Prepare of the kind (a lease's: its mode's) on device
// d of VM v that gets as far as stage shows the marker.
func (s *State) prepareShows(v VM, marker int, kind Kind, lease bool, d DevID, stage int) bool {
	si := stackIndex(v, HB1)
	region := s.mayMakeRegion(si, devKey(d))
	switch marker {
	case markerRegion:
		return region
	case markerNS:
		return region && stage >= prepProbe && (s.PDev[pmemIndex(d)].NS != TriNo || (stage == prepWhole && kind.writer()))
	case markerMount:
		return stage == prepWhole && kind.fsKind() && !s.nodeMountKept(v, d, kind == KPFsc)
	case markerBind:
		return stage == prepWhole && lease && kind.fsKind()
	}
	return false
}

// mayMakeRegion: a Prepare of key on stack si may make a new region: it
// is not there at the start of the window, or an Unprepare of the window
// may take it first (gone and back is new to the watcher).
func (s *State) mayMakeRegion(si int, key RKey) bool {
	if !s.Stacks[si].contains(key) {
		return true
	}
	for _, e := range s.Ev {
		if e.Kind == evUnprepare && int(e.Stack) == si && e.Key == key {
			return true
		}
	}
	return false
}

// nodeMountKept: the node mount of the device with the access is on VM v
// for the whole window: a claim or lease prepared before it keeps it (no
// Unprepare of it in the window).
func (s *State) nodeMountKept(v VM, d DevID, cached bool) bool {
	for i := range s.Slots {
		ss, sl := &s.Slots[i], s.slot(i)
		u := int16(-1)
		switch {
		case sl.Kind.fsKind() && sl.Dev == d && ss.Node == v && (sl.Kind == KPFsc) == cached:
			u = int16(i)
		case sl.Kind == KPlse && d == cPmem0 && sl.VM == v && ss.LeaseMode.fs() && (ss.LeaseMode == ModePFC) == cached:
			u = s.secondUser(i)
		}
		if u >= 0 && s.preparedAtStart(u) && !s.hasUnprepare(u) {
			return true
		}
	}
	return false
}

// expandResourceSlices returns a state per world of the pools whose controller is
// down while it is not known whether it published its slices again
// after a graceful restart (A18): present (as is) and absent (the claims
// of the pool that are being allocated cannot be: Unsched, no prepare;
// one allocated before, Alloc, keeps its allocation: it waits for the
// attach, or prepares if attached before the kill).
func (n *State) expandResourceSlices() []*State {
	states := []*State{n}
	for p := Pool(0); p < nPools; p++ {
		if n.Ctrl[p].Up || n.Ctrl[p].ResourceSlices != SlicesUnknown {
			continue
		}
		var next []*State
		for _, b := range states {
			present := b.clone()
			present.Ctrl[p].ResourceSlices = SlicesPresent
			absent := b.clone()
			absent.Ctrl[p].ResourceSlices = SlicesAbsent
			var ev []stackEvent
			for i := range absent.Slots {
				sl := absent.slot(i)
				if ss := &absent.Slots[i]; sl.Kind.pooled() && sl.Pool == p && ss.Phase == Starting && !ss.Alloc {
					ss.Phase, ss.RaceKill = Unsched, false
				}
			}
			for _, e := range absent.Ev {
				// (both claims of a dnuma pod)
				if i, lease := absent.userSlot(e.User); (!lease || absent.slot(i).Kind.dual()) && absent.Slots[i].Phase == Unsched {
					continue
				}
				ev = append(ev, e)
			}
			absent.Ev = ev
			next = append(next, present, absent)
		}
		states = next
	}
	return states
}

// blockedOn tells if a stack has a blocked pod.
func (s *State) blockedOn(si int) bool {
	for i := range s.Slots {
		if s.Slots[i].Phase == Blocked {
			if ssi, _, ok := s.userRegion(int16(i)); ok && ssi == si {
				return true
			}
			if lu := s.dualUser(int16(i)); lu >= 0 {
				if ssi, _, ok := s.userRegion(lu); ok && ssi == si {
					return true
				}
			}
		}
	}
	return false
}

// expandAnyNodes returns a state per combination of hosts of the "any"
// pods that prepare in this window on a node that is not known yet.
func (n *State) expandAnyNodes() []*State {
	var anys []int
	for _, e := range n.Ev {
		if e.Stack >= 0 {
			continue
		}
		i, _ := n.userSlot(e.User)
		dup := false
		for _, x := range anys {
			dup = dup || x == i
		}
		if !dup {
			anys = append(anys, i)
		}
	}
	states := []*State{n}
	for _, i := range anys {
		var next []*State
		sl := n.slot(i)
		for _, b := range states {
			for _, v := range poolHosts[sl.Pool] {
				c := b.clone()
				c.Slots[i].Node = v
				c.Ev = append([]stackEvent(nil), b.Ev...)
				for j := range c.Ev {
					if c.Ev[j].Stack < 0 && int(c.Ev[j].User) == i {
						c.Ev[j].Stack = int8(stackIndex(v, poolBridge[sl.Pool]))
					}
				}
				next = append(next, c)
			}
		}
		states = next
	}
	return states
}

// applyOutcome sets the stack and the phases of its pods by an outcome:
// prepared -> Running, a failed prepare -> Blocked, or Unblocking when
// no hole is left; a prepare that did not happen (a controller kill) ->
// Attaching. Pods being deleted are left to the settle.
func (a *State) applyOutcome(si int, o *stackOutcome) {
	a.Stacks[si] = o.S
	hole := o.S.hasHole()
	users := map[int16]bool{}
	for _, e := range a.eventsOn(si) {
		users[e.User] = true
	}
	for u := range o.Blocked {
		users[u] = true
	}
	for _, u := range sortedUsers(users) {
		i, lease := a.userSlot(u)
		if lease {
			continue
		}
		ss := &a.Slots[i]
		sl := a.slot(i)
		if ss.Phase == Deleting || ss.Phase == Absent {
			continue
		}
		if ss.Phase == Starting && ss.Refuse && ss.RaceKill && !o.Blocked[u] {
			// a refused claim's Prepare makes no region: whether it was
			// attached before the controller kill is the settle's
			// Either (refused or attaching), not the stack's
			continue
		}
		switch {
		case o.Prepared[u]:
			ss.Phase = Running
		case o.Blocked[u] && hole:
			ss.Phase = Blocked
		case o.Blocked[u]:
			if ss.Phase != Unblocking {
				a.setUnblockAt(i, 0)
			}
			ss.Phase = Unblocking
		case ss.Phase == Starting && sl.Kind.pooled() && !a.Ctrl[sl.Pool].Up:
			ss.Phase = Attaching
		}
		ss.RaceKill = false
	}
}

// applyDual sets the phase of a dnuma pod by the outcomes of both of its
// claims' stacks (applyOutcome sets it by its pool claim's): its local
// slice blocked by a hole makes it Blocked (or Unblocking without one).
// It returns false for a combination of outcomes in which one of its two
// Prepares came and the other did not although none was refused: kubelet
// prepares both claims of a bound pod, a pod that is not bound neither
// (a controller kill while it was starting).
func (a *State) applyDual(b *State, sis []int, outs [][]*stackOutcome, idx []int) bool {
	outOf := func(si int) *stackOutcome {
		for j, x := range sis {
			if x == si {
				return outs[j][idx[j]]
			}
		}
		return nil
	}
	for i := range a.Slots {
		sl, ss := a.slot(i), &a.Slots[i]
		if !sl.Kind.dual() || ss.Phase == Deleting || ss.Phase == Absent {
			continue
		}
		u, lu := int16(i), a.secondUser(i)
		if !b.hasPrepare(u) && !b.hasPrepare(lu) && !b.blockedAtStart(u) {
			continue
		}
		pool, local := outOf(stackIndex(ss.Node, poolBridge[sl.Pool])), outOf(stackIndex(ss.Node, HB0))
		if local != nil && local.Blocked[lu] {
			if local.S.hasHole() {
				ss.Phase = Blocked
			} else if ss.Phase != Blocked {
				if ss.Phase != Unblocking {
					a.setUnblockAt(i, 0)
				}
				ss.Phase = Unblocking
			}
			continue
		}
		if pool != nil && pool.Blocked[u] {
			continue
		}
		if b.hasPrepare(u) && b.hasPrepare(lu) && (pool != nil && pool.Prepared[u]) != (local != nil && local.Prepared[lu]) {
			return false
		}
	}
	return true
}

// altKey identifies a settled outcome.
func (s *State) altKey() string {
	b := make([]byte, 0, 4*len(s.Slots)+nStacks*(maxStack+1))
	for i := range s.Slots {
		b = append(b, byte(s.Slots[i].Phase), byte(s.Slots[i].Node+1), byte(s.Slots[i].Lease))
	}
	for si := range s.Stacks {
		st := &s.Stacks[si]
		b = append(b, byte(st.N))
		for _, k := range st.E[:st.N] {
			b = append(b, byte(k+1))
		}
	}
	// the pools: worlds that differ only in the slices or the
	// attachments of a pool are different outcomes (A18, A20)
	for p := range s.Ctrl {
		c := &s.Ctrl[p]
		b = append(b, byte(c.ResourceSlices))
		for _, row := range c.Att {
			for _, t := range row {
				b = append(b, byte(t))
			}
		}
	}
	// the drivers (a driver-kill-at that fired or not) and the
	// persistent devices
	for v := range s.Drv {
		b = append(b, s.Drv[v].Restarts, s.Drv[v].KillAt)
	}
	for x := range s.PDev {
		b = append(b, byte(s.PDev[x].NS), byte(s.PDev[x].FS))
	}
	return string(b)
}
