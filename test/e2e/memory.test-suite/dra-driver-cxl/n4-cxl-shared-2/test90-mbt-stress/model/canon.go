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

// Canonical states and the per-plan transition cache. Two states that differ only in what
// the search never reads (the step number, the action that led to them,
// the oracle's clocks, tokens and names, the other outcomes of a settle)
// behave the same in next() and in the coverage functions: the planner
// keeps one object per key and computes its steps once per plan.
//
// The key is everything next() and the coverage functions read:
//
//	field                        bytes   encoding
//	Slots[i] (every slot)        4       Phase | Prev<<4;
//	                                     Node+1 | RaceKill<<2 | Steer<<3 |
//	                                     Lease<<5 | CompLoose<<7;
//	                                     min(Restarts,15) | min(Slack,15)<<4
//	                                     (15: the exact bytes are in the
//	                                     trailer);
//	                                     LeaseMode | Hold<<3 | Refuse<<4 |
//	                                     LeaseOpen<<5 | Alloc<<6 | Loose<<7
//	Ctrl[p] (every pool)         4       Up | Slices<<1 | Att[d][v]<<(3+2*(3d+v))
//	Drv[v] (every VM)            4       Restarts; F | FMaybe<<4;
//	                                     Killed | Restarted<<1 | KillAt<<2 |
//	                                     KillMay<<5 | Creating<<6 |
//	                                     KillSlack<<7; Slack
//	PDev[x] (every pmem device)  1       NS | FS<<2
//	Stacks[si] (every stack)     1+N/2   N, then the keys+1 as nibbles
//	Contested, Held, Released    2 each  little endian
//	Unsettled                    1
//	Fence, Switched              1       Fence | Switched<<1
//	Reorder, Drv[v].Rebooted     1 each  the stacks' bits; bit v of the VMs
//	Ev                           2+7/ev  the count (2 bytes), then per event
//	                                     Stack+1 | Kind<<3 | Opt<<4 |
//	                                     Sync<<5 | Handover<<6 | Probe<<7;
//	                                     Key+1; User;
//	                                     Seq (2); After+1 (2)
//	trailer                      2/esc   Restarts, Slack of every slot
//	                                     whose nibble is 15, in slot order
//
// Left out, with the reason (none of them is read by next(), allowed(),
// stackRulesOK() or the coverage functions C1-C4: canon_test.go checks it):
//
//	Steps       the step number: only the token text of a write (Data,
//	            SlotData) and the protocol read it
//	Last        the action that led here: the C2 function takes the
//	            action from the step (gofmbt's Action) instead
//	HeldAt      the oracle's clock of Held (the bit itself is in the key)
//	Drv.KillAtAt the oracle's clock of KillAt (KillAt is in the key)
//	PDev[x].LSAHash, .HeadHash  hashes adopted from observations
//	UnblockAt   the oracle's clock of Unblocking, Stalled, Attaching
//	Drv.Pod, Drv.PodBefore, Drv.PodGen, Drv.Inc
//	            driver pod names and incarnations: the oracle and the
//	            simulator only
//	Drv.Boot, Drv.BootBefore, Drv.Reboots
//	            boot ids and the count of node-reboots (A44): the oracle
//	            and the simulator only
//	LeaseIDs    lease IDs learnt from observations: the oracle only
//	SlotData, Data, PDev[x].Tok, .Raw, .UUID
//	            the tokens of dax and persistent devices and ldax
//	            slices, the namespace UUID: expectations and the oracle
//	Slots[i].Gen
//	            pod name generation: the oracle and the simulator only
//	Alts, Truncated, Wide, Narrow
//	            the other outcomes of a settle, and what narrows a
//	            truncated one: the oracle only
//	cfg         shared by every state of a planner
//
// The canonical object of a key carries the left-out fields of whichever
// path reached the key first. That is harmless because no state of the
// search leaves the planner: plan and planAfter return actions, and the
// on-line loop computes the states it executes itself (from.apply(a)).

import (
	m "github.com/askervin/gofmbt/gofmbt"
)

// appendKey appends the canonical key of the state to b.
func (s *State) appendKey(b []byte) []byte {
	esc := false
	for i := range s.Slots {
		ss := &s.Slots[i]
		b = append(b,
			byte(ss.Phase)|byte(ss.Prev)<<4,
			byte(ss.Node+1)|b2u(ss.RaceKill)<<2|byte(ss.Steer)<<3|byte(ss.Lease)<<5|b2u(ss.CompLoose)<<7,
			nib(ss.Restarts)|nib(ss.Slack)<<4,
			byte(ss.LeaseMode)|b2u(ss.Hold)<<3|b2u(ss.Refuse)<<4|b2u(ss.LeaseOpen)<<5|b2u(ss.Alloc)<<6|b2u(ss.Loose)<<7)
		esc = esc || ss.Restarts >= 15 || ss.Slack >= 15
	}
	for p := range s.Ctrl {
		c := &s.Ctrl[p]
		x := uint32(b2u(c.Up)) | uint32(c.ResourceSlices)<<1
		for d := range c.Att {
			for v := range c.Att[d] {
				x |= uint32(c.Att[d][v]) << (3 + 2*(nVMs*d+v))
			}
		}
		b = append(b, byte(x), byte(x>>8), byte(x>>16), byte(x>>24))
	}
	for v := range s.Drv {
		d := &s.Drv[v]
		b = append(b, d.Restarts, d.F|d.FMaybe<<4,
			b2u(d.Killed)|b2u(d.Restarted)<<1|d.KillAt<<2|b2u(d.KillMay)<<5|b2u(d.Creating)<<6|b2u(d.KillSlack)<<7, d.Slack)
	}
	for x := range s.PDev {
		b = append(b, byte(s.PDev[x].NS)|byte(s.PDev[x].FS)<<2)
	}
	for si := range s.Stacks {
		st := &s.Stacks[si]
		b = append(b, byte(st.N))
		for j := int8(0); j < st.N; j += 2 {
			x := byte(st.E[j] + 1)
			if j+1 < st.N {
				x |= byte(st.E[j+1]+1) << 4
			}
			b = append(b, x)
		}
	}
	b = append(b, byte(s.Contested), byte(s.Contested>>8), byte(s.Held), byte(s.Held>>8),
		byte(s.Deallocating), byte(s.Deallocating>>8), s.Unsettled, byte(s.Fence)|b2u(s.Switched)<<1)
	var rebooted byte
	for v := range s.Drv {
		rebooted |= b2u(s.Drv[v].Rebooted) << uint(v)
	}
	b = append(b, s.Reorder, rebooted)
	b = append(b, byte(len(s.Ev)), byte(len(s.Ev)>>8))
	for j := range s.Ev {
		e := &s.Ev[j]
		b = append(b,
			byte(e.Stack+1)|byte(e.Kind)<<3|b2u(e.Opt)<<4|b2u(e.Sync)<<5|b2u(e.Handover)<<6|b2u(e.Probe)<<7,
			byte(e.Key+1), byte(e.User),
			byte(e.Seq), byte(e.Seq>>8), byte(e.After+1), byte((e.After+1)>>8))
	}
	if esc {
		for i := range s.Slots {
			if ss := &s.Slots[i]; ss.Restarts >= 15 || ss.Slack >= 15 {
				b = append(b, ss.Restarts, ss.Slack)
			}
		}
	}
	return b
}

func b2u(x bool) byte {
	if x {
		return 1
	}
	return 0
}

// nib is a nibble of a counter, 15 for 15 and more (escaped).
func nib(x uint8) byte {
	if x >= 15 {
		return 15
	}
	return x
}

// key returns the canonical key of the state.
func (s *State) key() string { return string(s.appendKey(nil)) }

// planWalk is the Walkable of one plan: the model's steps with canonical
// end states, computed once per canonical state. It lives as long as one
// plan (plan or planAfter, F4: consecutive plans share ~3% of their
// trees) and is never stored in a State (F5: the Coverer keeps covered
// steps, and with them everything their states point to). Not safe for
// concurrent use: one plan at a time (online.go never runs plan,
// planAfter and mark concurrently).
type planWalk struct {
	mm     *mbtModel
	states map[string]*State    // key -> canonical state
	steps  map[*State][]*m.Step // canonical state -> its steps, end states canonical
	max    int                  // states kept at most: then new ones are not cached
	buf    []byte
}

// defaultPlanStates is the default of --plan-states (Config.PlanStates):
// no cache. The cache trades memory for plan time (it saves little at
// depth 4, where the trees exceed the bound), which the default of a test
// generator should not spend unasked.
const defaultPlanStates = 0

// maxPlanStates caches a whole depth-3 plan of the full profile: ~1.7-2 KB per state (the
// state, its slots, its key, its step, its coverage strings), ~300 MB
// live at the bound. A depth-3 plan of the full profile has at most
// about 135k distinct states, a depth-4 one about 430k. Beyond the bound new states are walked as without the
// cache: the same steps, only slower.
const maxPlanStates = 150000

// newPlanWalk returns the walkable of a plan from roots (the start state
// of the plan, and in planAfter the predicted end state of the step
// being executed): they are canonical for their keys, so that the tree
// below them reuses their memoized data.
func newPlanWalk(mm *mbtModel, roots ...*State) *planWalk {
	w := &planWalk{mm: mm, states: map[string]*State{}, steps: map[*State][]*m.Step{}, max: mm.cfg.PlanStates}
	for _, r := range roots {
		w.intern(r)
	}
	return w
}

// intern returns the canonical state of the key of n, n itself if it is
// the first of its key.
func (w *planWalk) intern(n *State) *State {
	if w.max <= 0 {
		return n
	}
	w.buf = n.appendKey(w.buf[:0])
	if c, ok := w.states[string(w.buf)]; ok {
		return c
	}
	if !w.full() {
		w.states[string(w.buf)] = n
	}
	return n
}

// StepsFrom implements gofmbt's Walkable. The search reorders the slice
// it gets: a copy of the cached one.
func (w *planWalk) StepsFrom(s m.State) []*m.Step {
	st := s.(*State)
	steps, ok := w.steps[st]
	if !ok {
		steps = w.mm.stepsOf(st, w.intern)
		if w.full() {
			// full: the steps of new states are not kept either
			return steps
		}
		w.steps[st] = steps
	}
	return append([]*m.Step(nil), steps...)
}

// full tells if the walk keeps no more states.
func (w *planWalk) full() bool { return len(w.states) >= w.max }

// size is the number of canonical states and of states with cached steps.
func (w *planWalk) size() (states, expanded int) { return len(w.states), len(w.steps) }
