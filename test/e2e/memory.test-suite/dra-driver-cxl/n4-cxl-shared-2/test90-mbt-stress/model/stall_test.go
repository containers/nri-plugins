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

// Stalled contenders: the scheduler's hold of a device whose claim was
// deleted before its pod was bound, the promotion, the stall window and
// the binding timeout.

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// A claim deleted while it waited for the binding conditions holds its
// device in the scheduler: the contender may stall (Pending, claim not
// allocated) or be promoted; both are accepted within --stall-window (6m
// by default), then the promotion is required.
func TestStalledContender(t *testing.T) {
	s := stateAfter(t, "full", true, "ctrl-kill a", "create xnuma-a@vm1#0", "settle", "create xdax-a@vm1#0", "settle")
	wantPhase(t, s, "xnuma-a@vm1#0", Attaching)
	wantPhase(t, s, "xdax-a@vm1#0", Pending)
	// off-line it is not generated
	off := stateAfter(t, "full", false, "ctrl-kill a", "create xnuma-a@vm1#0", "settle", "create xdax-a@vm1#0", "settle")
	steps(t, off, "!delete xnuma-a@vm1#0")
	s = steps(t, s, "delete xnuma-a@vm1#0", "settle")
	wantPhase(t, s, "xdax-a@vm1#0", Stalled)
	// the device stays its: nothing else asks for it
	steps(t, s, "!create xnuma-a@vm2#0")
	now := time.Unix(time.Now().Unix(), 0)
	// stalled: ok, and the time is kept
	o := observation(t, s)
	v, adopted := checkStateAt(s, o, "", now, defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "xdax-a@vm1#0", Stalled)
	v, _ = checkStateAt(adopted, o, "", now.Add(400*time.Second), defaultUnblock)
	wantVerdict(t, v, VFail, "still not allocated 6m40s after the delete")
	// promoted: allocated, waiting for the attach (controller down)
	truth := s.clone()
	truth.Slots[truth.cfg.slotIndex("xdax-a@vm1#0")].Phase = Attaching
	v, adopted = checkStateAt(s, observation(t, truth), "", now, defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "xdax-a@vm1#0", Attaching)
	// stalled, then the controller comes back: promoted later, Running
	st := steps(t, s, "ctrl-start a", "settle")
	wantPhase(t, st, "xdax-a@vm1#0", Stalled)
	truth = st.clone()
	truth.Slots[truth.cfg.slotIndex("xdax-a@vm1#0")].Phase = Running
	truth.Ctrl[PoolA].Att[0][VM1] = TriYes
	truth.Stacks[stackIndex(VM1, HB0)].push(devKey(pooledDev(PoolA)))
	v, adopted = checkStateAt(st, observation(t, truth), "", now, defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "xdax-a@vm1#0", Running)
	wantStack(t, adopted, stackIndex(VM1, HB0), "[a-pooled0]")
	// a holder that was bound hands over at once
	b := stateAfter(t, "full", true, "create xnuma-a@vm1#0", "settle", "create xdax-a@vm1#0", "settle", "delete xnuma-a@vm1#0", "settle")
	wantPhase(t, b, "xdax-a@vm1#0", Running)
}

// A claim deleted before its pod was bound holds its device in the
// scheduler up to a PreBind poll period (5 s) after the delete, also past
// a short settle: a contender created in the next window may stall. The
// oracle ends the hold heldSeconds after the settle that ended the
// delete's window (adoption record held=DEV:off). Off-line nothing would
// end it: no such delete.
func TestSchedulerHoldAfterSettle(t *testing.T) {
	s := stateAfter(t, "full", true, "ctrl-kill a", "create xnuma-a@vm1#0", "settle", "delete xnuma-a@vm1#0", "settle")
	d := pooledDev(PoolA)
	if !s.held(Res(d)) {
		t.Fatalf("a-pooled0 not held:\n%s", s.dump())
	}
	now := time.Unix(time.Now().Unix(), 0)
	v, adopted := checkStateAt(s, observation(t, s), "", now, defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	if adopted.HeldAt[d] != now.Unix() {
		t.Fatalf("held since %d, want %d", adopted.HeldAt[d], now.Unix())
	}
	n := steps(t, adopted, "create xdax-a@vm1#0", "!create xnuma-a@vm2#0")
	wantPhase(t, n, "xdax-a@vm1#0", Stalled)
	// a settle 6 s later ends it: a contender gets the device at once
	pred := steps(t, adopted, "create plain@vm0#0", "settle")
	v, later := checkStateAt(pred, observation(t, pred), "", now.Add(7*time.Second), defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	if later.held(Res(d)) {
		t.Fatalf("still held after 7 s")
	}
	if rec := adoptionRecord(pred, 0, later); !strings.Contains(rec, "held=a-pooled0:off") {
		t.Fatalf("adoption record %q", rec)
	}
	if back, err := applyAdoption(pred, adoptionRecord(pred, 0, later)); err != nil || back.held(Res(d)) {
		t.Fatalf("record not applied: %v", err)
	}
	wantPhase(t, steps(t, later, "create xdax-a@vm1#0"), "xdax-a@vm1#0", Starting)
	// a holder deleted while it was starting holds it too (a create
	// and a delete in one window)
	if h := stateAfter(t, "full", true, "create xdax-b@vm0#0", "delete xdax-b@vm0#0", "settle"); !h.held(Res(pooledDev(PoolB))) {
		t.Fatalf("b-pooled0 not held:\n%s", h.dump())
	}
	// a bound holder does not
	if b := stateAfter(t, "full", true, "create xdax-b@vm0#0", "settle", "delete xdax-b@vm0#0", "settle"); b.held(Res(pooledDev(PoolB))) {
		t.Fatalf("b-pooled0 held after a bound holder")
	}
	steps(t, stateAfter(t, "full", false, "ctrl-kill a", "create xnuma-a@vm1#0", "settle"), "!delete xnuma-a@vm1#0")
	steps(t, stateAfter(t, "full", false, "create xdax-b@vm0#0"), "!delete xdax-b@vm0#0")
}

// A stalled contender seen allocated: an "any" one on the node it was
// bound to; through a decoder hole Blocked; bound before a controller kill
// of the same window: Running.
func TestStalledContenderPromoted(t *testing.T) {
	s := stateAfter(t, "full", true, "ctrl-kill c", "create xnuma-c@vm1#0", "settle", "create xdax-c@any#0", "settle",
		"delete xnuma-c@vm1#0", "ctrl-start c", "settle")
	wantPhase(t, s, "xdax-c@any#0", Stalled)
	truth := s.clone()
	i := truth.cfg.slotIndex("xdax-c@any#0")
	truth.Slots[i].Phase, truth.Slots[i].Node = Running, VM2
	truth.Stacks[stackIndex(VM2, HB1)].push(devKey(pooledDev(PoolC)))
	v, adopted := checkState(s, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	if ss := slotOf(t, adopted, "xdax-c@any#0"); ss.Phase != Running || ss.Node != VM2 {
		t.Fatalf("adopted %s on %s", ss.Phase, ss.Node)
	}
	o := observation(t, truth)
	bogus := "vm9"
	podOf(t, o, "xdax-c@any#0").Node = &bogus
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VFail, `unknown node "vm9"`)
	// through vm1/hb0 [hole,local]: its Prepare needs a new region
	h := stateAfter(t, "full", true, "create xnuma-a@vm1#0", "settle", "create lnuma@vm1#0", "settle", "delete xnuma-a@vm1#0", "settle",
		"ctrl-kill a", "create xnuma-a@vm2#0", "settle", "create xdax-a@vm1#0", "settle", "delete xnuma-a@vm2#0", "ctrl-start a", "settle")
	wantPhase(t, h, "xdax-a@vm1#0", Stalled)
	wantStack(t, h, stackIndex(VM1, HB0), "[hole,local]")
	truth = h.clone()
	truth.Slots[truth.cfg.slotIndex("xdax-a@vm1#0")].Phase = Blocked
	v, adopted = checkState(h, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "xdax-a@vm1#0", Blocked)
	// promoted and bound, then the controller killed
	k := steps(t, stateAfter(t, "full", true, "ctrl-kill a", "create xnuma-a@vm1#0", "settle", "create xdax-a@vm1#0", "settle",
		"delete xnuma-a@vm1#0", "ctrl-start a", "settle"), "ctrl-kill a", "settle")
	wantPhase(t, k, "xdax-a@vm1#0", Stalled)
	if k.Ctrl[PoolA].Att[0][VM1] != TriMaybe {
		t.Fatalf("a-pooled0@vm1 %v after the kill, want maybe", k.Ctrl[PoolA].Att[0][VM1])
	}
	truth = k.clone()
	truth.Slots[truth.cfg.slotIndex("xdax-a@vm1#0")].Phase = Running
	truth.Ctrl[PoolA].Att[0][VM1] = TriYes
	truth.Stacks[stackIndex(VM1, HB0)].push(devKey(pooledDev(PoolA)))
	v, adopted = checkState(k, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "xdax-a@vm1#0", Running)
}

// A contender promoted when a bound holder goes, in a window in which (or
// before whose last settle) an unbound claim of the device was deleted,
// may stall.
func TestStalledAfterUnboundDelete(t *testing.T) {
	s := stateAfter(t, "full", true, "create sdax-a@vm2#1", "settle", "ctrl-kill a", "create sro-a@vm2#0", "settle",
		"create sexcl-a@vm2#0", "settle")
	wantPhase(t, s, "sro-a@vm2#0", Attaching)
	wantPhase(t, s, "sexcl-a@vm2#0", Pending)
	n := steps(t, s, "delete sro-a@vm2#0", "delete sdax-a@vm2#1")
	wantPhase(t, n, "sexcl-a@vm2#0", Stalled)
	// held since the last settle, a later fit
	h := stateAfter(t, "full", true, "create sdax-c@vm2#0", "settle", "create sro-c@vm0#0", "delete sro-c@vm0#0", "settle")
	h = steps(t, h, "delete sdax-c@vm2#0", "create sexcl-c@vm1#0")
	wantPhase(t, h, "sexcl-c@vm1#0", Stalled)
}

// A stalled contender gets its region whenever the scheduler requeues it:
// no other region event on the stacks it may get it on (also when
// ctrl-start makes it for an "any" pod, and for the hosts of a stalled
// "any" pod), and no contender stalls into a window that has one.
func TestStalledContenderStacksQuiet(t *testing.T) {
	held := stateAfter(t, "full", true, "create xdax-c@vm2#0", "delete xdax-c@vm2#0", "settle")
	steps(t, held, "create sro-c@vm2#0", "!create xdax-c@vm2#0")
	steps(t, held, "create xdax-c@vm2#0", "!create sro-c@vm2#0")
	a := steps(t, held, "create xdax-c@any#0", "settle")
	wantPhase(t, a, "xdax-c@any#0", Stalled)
	steps(t, a, "!create sdax-c@vm0#0", "create sdax-a@vm1#0")
	k := steps(t, held, "create xnuma-c@vm1#0", "settle", "ctrl-kill c", "create sdax-c@any#0")
	wantPhase(t, k, "xnuma-c@vm1#0", Stalled)
	steps(t, k, "!ctrl-start c")
}

// A claim that waits for the attach with its controller down times out in
// PreBind (600 s): it is deallocated, and the waiter of its device may get
// it. Accepted from bindingExpiry after the first settle that saw it
// waiting.
func TestBindingTimeout(t *testing.T) {
	s := stateAfter(t, "full", true, "ctrl-kill a", "create xnuma-a@vm1#0", "settle", "create xdax-a@vm1#0", "settle")
	now := time.Unix(time.Now().Unix(), 0)
	v, adopted := checkStateAt(s, observation(t, s), "", now, defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	truth := adopted.clone()
	truth.Slots[truth.cfg.slotIndex("xnuma-a@vm1#0")].Phase = Pending
	truth.Slots[truth.cfg.slotIndex("xdax-a@vm1#0")].Phase = Attaching
	v, _ = checkStateAt(adopted, observation(t, truth), "", now.Add(100*time.Second), defaultUnblock)
	if v.Status == VOK {
		t.Fatalf("the swap accepted 100 s after")
	}
	v, swapped := checkStateAt(adopted, observation(t, truth), "", now.Add(bindingExpiry*time.Second), defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, swapped, "xnuma-a@vm1#0", Pending)
	wantPhase(t, swapped, "xdax-a@vm1#0", Attaching)
	// ctrl-start: no binding timeout any more
	n := steps(t, swapped, "ctrl-start a")
	if n.unblockAt(n.cfg.slotIndex("xdax-a@vm1#0")) != 0 {
		t.Fatalf("binding time kept after ctrl-start")
	}
}

// simSteps drives the simulated SUT of seed through action lines (the
// model's state adopted from every check), each line enabled in the
// model's state, with a clock that advances dt per action. It returns the
// first step whose check is not ok ("" if none), the model's and the
// simulator's states.
func simSteps(t *testing.T, cfg *Config, seed int64, dt time.Duration, lines ...string) (string, *State, *sim) {
	t.Helper()
	s := newSim(cfg, seed)
	st := newState(cfg)
	now := time.Unix(1800000000, 0)
	for j, line := range lines {
		n := j + 1
		a := mustAct(t, cfg, line)
		if !st.enabled(a) {
			t.Fatalf("seed %d step %d %q: not enabled in\n%s", seed, n, line, st.dump())
		}
		now = now.Add(dt)
		next := st.apply(a)
		if a.Verb == VSettle {
			s.do(n, a)
			v, adopted := checkStateAt(next, s.observe(n, 1), "", now, defaultUnblock)
			if v.Status != VOK {
				return fmt.Sprintf("step %d %s: %s", n, line, v.Msg), st, s
			}
			st = adopted
			continue
		}
		d := s.do(n, a)
		if v := checkDone(expectDone(st, next, a, n), d); v.Status != VOK {
			return fmt.Sprintf("step %d %s: %s", n, line, v.Msg), st, s
		}
		st = adoptDone(next, a, d)
	}
	return "", st, s
}

// The simulated SUT ends the scheduler's hold of a device (a claim deleted
// before its pod was bound) within a PreBind poll period (5 s) of the
// delete, and the oracle 6 s after the settle; a promoted contender's
// Prepare through a decoder hole is refused like any other. A contender
// created after the hold is Blocked by the hole in the model and in the
// simulator, never Running above it.
func TestSimHoldEndsAndHoleBlocks(t *testing.T) {
	cfg := testConfig(t, "full", true)
	hole := []string{
		"create xnuma-b@vm0#0", "settle", "create lnuma@vm0#0", "settle", // vm0/hb0 [b-pooled0,local]
		"delete xnuma-b@vm0#0", "settle", // [hole,local]
	}
	held := []string{
		"ctrl-kill b", "create sro-b@vm0#0", "settle", // waits for the attach
		"delete sro-b@vm0#0", "ctrl-start b", "settle", // b-shared0 held
	}
	// the hold ends, sro-b blocked by the hole, in the model and in
	// every simulated SUT
	after := append(append(append([]string{}, hole...), held...), "create plain@vm0#0", "settle", "create sro-b@vm0#0", "settle")
	for seed := int64(1); seed <= 16; seed++ {
		msg, st, s := simSteps(t, cfg, seed, 4*time.Second, after...)
		if msg != "" {
			t.Fatalf("seed %d: %s\nsimulated:\n%s", seed, msg, s.truth.dump())
		}
		wantPhase(t, st, "sro-b@vm0#0", Blocked)
		wantPhase(t, s.truth, "sro-b@vm0#0", Blocked)
		wantStack(t, s.truth, stackIndex(VM0, HB0), "[hole,local]")
		if s.truth.held(Res(sharedDev(PoolB))) {
			t.Fatalf("seed %d: the simulator still holds b-shared0", seed)
		}
	}
	// still held in the window after the settle: a contender created
	// then stalls in the model; the simulator may hold it as long (it
	// promotes it or not), and never beyond the oracle
	pre := append(append([]string{}, hole...), held...)
	for seed := int64(1); seed <= 16; seed++ {
		msg, st, s := simSteps(t, cfg, seed, time.Second, append(append([]string{}, pre...), "create sro-b@vm0#0", "settle", "create plain@vm0#0", "settle")...)
		if msg != "" {
			t.Fatalf("seed %d: %s\nsimulated:\n%s", seed, msg, s.truth.dump())
		}
		if ph := slotOf(t, st, "sro-b@vm0#0").Phase; ph != Stalled && ph != Blocked {
			t.Fatalf("seed %d: sro-b %s, want stalled or blocked (promoted through the hole)", seed, ph)
		}
	}
	// a contender stalled behind an unbound holder in its window,
	// promoted by the simulator: blocked by the hole, never Running
	promoted := 0
	for seed := int64(1); seed <= 16; seed++ {
		msg, st, s := simSteps(t, cfg, seed, 4*time.Second, append(append([]string{}, hole...),
			"ctrl-kill b", "create xdax-b@vm0#0", "settle", "create xnuma-b@vm0#0", "settle", // a waiter of b-pooled0
			"delete xdax-b@vm0#0", "ctrl-start b", "settle")...) // stalled
		if msg != "" {
			t.Fatalf("seed %d: %s\nsimulated:\n%s", seed, msg, s.truth.dump())
		}
		switch ph := slotOf(t, s.truth, "xnuma-b@vm0#0").Phase; ph {
		case Blocked:
			promoted++
			wantPhase(t, st, "xnuma-b@vm0#0", Blocked)
		case Stalled:
		default:
			t.Fatalf("seed %d: simulated xnuma-b %s through vm0/hb0 %s", seed, ph, s.truth.Stacks[stackIndex(VM0, HB0)])
		}
	}
	if promoted == 0 {
		t.Fatalf("no seed promoted the stalled contender")
	}
	// an "any" contender promoted by the simulator: its region on the
	// stack of the host it was bound to
	promoted = 0
	for seed := int64(1); seed <= 16; seed++ {
		msg, st, s := simSteps(t, cfg, seed, 4*time.Second, "ctrl-kill c", "create xnuma-c@vm1#0", "settle", "create xdax-c@any#0", "settle",
			"delete xnuma-c@vm1#0", "ctrl-start c", "settle")
		if msg != "" {
			t.Fatalf("seed %d: %s\nsimulated:\n%s", seed, msg, s.truth.dump())
		}
		if ss := slotOf(t, s.truth, "xdax-c@any#0"); ss.Phase == Running {
			promoted++
			wantStack(t, s.truth, stackIndex(ss.Node, HB1), "[c-pooled0]")
			if got := slotOf(t, st, "xdax-c@any#0"); got.Phase != Running || got.Node != ss.Node {
				t.Fatalf("seed %d: adopted %s on %s", seed, got.Phase, got.Node)
			}
		}
	}
	if promoted == 0 {
		t.Fatalf("no seed promoted the stalled any contender")
	}
}

// A numa contender stalled by the scheduler's hold of its device may be
// promoted at any time, and its Prepare onlines the device as system RAM:
// the bytes are gone, unknown as soon as it stalls. A simulated SUT whose
// hold ends a window earlier than the model's starts the contender there;
// the later read of the device is accepted.
func TestStalledNumaContenderLosesData(t *testing.T) {
	cfg := testConfig(t, "full", true)
	held := []string{
		"create xdax-c@vm2#0", "settle", "write xdax-c@vm2#0", "delete xdax-c@vm2#0", "settle", // c-pooled0 "mbt-3"
		"ctrl-kill c", "create xdax-c@vm2#0", "settle", "delete xdax-c@vm2#0", "ctrl-start c", "settle", // held
	}
	s := stateAfter(t, "full", true, held...)
	if !s.held(Res(pooledDev(PoolC))) || s.Data[pooledDev(PoolC)] != (Token{Known: true, Text: "mbt-3"}) {
		t.Fatalf("held %v, data %s", s.held(Res(pooledDev(PoolC))), s.Data[pooledDev(PoolC)])
	}
	s = steps(t, s, "create xnuma-c@vm1#0")
	wantPhase(t, s, "xnuma-c@vm1#0", Stalled)
	if s.Data[pooledDev(PoolC)].Known {
		t.Fatalf("data of c-pooled0 %s with a stalled numa contender, want unknown", s.Data[pooledDev(PoolC)])
	}
	// the run: the simulator's hold ends a window earlier than the
	// model's, the contender starts there; the read is accepted
	for seed := int64(1); seed <= 16; seed++ {
		msg, _, sm := simSteps(t, cfg, seed, time.Second, append(append([]string{}, held...),
			"create plain@vm0#0", "settle", "create xnuma-c@vm1#0", "settle",
			"delete xnuma-c@vm1#0", "settle", "create xdax-c@vm2#0", "settle", "read xdax-c@vm2#0")...)
		if msg != "" {
			t.Fatalf("seed %d: %s\nsimulated:\n%s", seed, msg, sm.truth.dump())
		}
	}
}

// A contender stalled only by the scheduler's hold (it fits now) may be
// allocated at once (the hold is 0-5 s after the delete), so its Prepare
// races with the release of another share of its device on its node:
// before it the region stays (above a hole), after it the region is
// destroyed and made again on top, or blocked by a hole. The oracle adopts
// a promotion after every event of the window only. Such a window is not
// generated; a contender that waits for the releases of its device still
// may have them in its window.
func TestStalledShareRace(t *testing.T) {
	s := stateAfter(t, "full", true,
		"create xnuma-b@vm0#0", "settle", "create sro-b@vm0#0", "settle", // vm0/hb0 [b-pooled0,b-shared0]
		"delete xnuma-b@vm0#0", "settle", // [hole,b-shared0]
		"ctrl-kill b", "create sdax-b@vm0#1", "settle", "delete sdax-b@vm0#1", "ctrl-start b", "settle") // b-shared0 held
	wantStack(t, s, stackIndex(VM0, HB0), "[hole,b-shared0]")
	if !s.held(Res(sharedDev(PoolB))) {
		t.Fatalf("b-shared0 not held")
	}
	// the release first, then the contender: refused (it was enabled)
	steps(t, s, "delete sro-b@vm0#0", "!create sdax-b@vm0#0", "create plain@vm0#0")
	// the contender first, then the release: refused as before (its
	// stack has a pod that waits for the scheduler)
	n := steps(t, s, "create sdax-b@vm0#0", "!delete sro-b@vm0#0")
	wantPhase(t, n, "sdax-b@vm0#0", Stalled)
	// a waiter of an exclusive device behind a holder deleted before its
	// pod was bound: the holder's events are what it waits for
	w := stateAfter(t, "full", true, "ctrl-kill b", "create xnuma-b@vm0#0", "settle", "create xdax-b@vm0#0", "settle")
	w = steps(t, w, "ctrl-start b", "delete xnuma-b@vm0#0")
	wantPhase(t, w, "xdax-b@vm0#0", Stalled)
}

// --stall-window, for a scheduler that requeues a stalled contender: with
// a window not longer than the settle timeout a stalled contender is not
// settled; the settle waits for its promotion and fails when the window
// from the start of the settle that first saw it stalled is over. With the
// default (6m) it is settled, with the controller down or started right
// after the delete.
func TestStallWindow(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, window := range [][]string{
			{"delete xnuma-a@vm1#0", "settle"},                 // the controller stays down
			{"delete xnuma-a@vm1#0", "ctrl-start a", "settle"}, // ctrl-start right after the delete
		} {
			s := stateAfter(t, "full", true, "ctrl-kill a", "create xnuma-a@vm1#0", "settle", "create xdax-a@vm1#0", "settle")
			if strict {
				s.cfg.StallWindow, s.cfg.StallStrict = 30*time.Second, true
			}
			s = steps(t, s, window...)
			wantPhase(t, s, "xdax-a@vm1#0", Stalled)
			since := time.Unix(time.Now().Unix(), 0)
			o := observation(t, s) // Pending, not allocated
			v, adopted, _ := checkAltsSince(s, o, "", since.Add(10*time.Second), since, defaultUnblock)
			if strict {
				wantVerdict(t, v, VWait, "xdax-a@vm1#0: not allocated yet, want its promotion (a stalled contender, --stall-window 30s)")
				v, _, _ = checkAltsSince(s, o, "", since.Add(31*time.Second), since, defaultUnblock)
				wantVerdict(t, v, VFail, "still not allocated 31s after the delete of the claim it waits for (A19, --stall-window 30s)")
			} else {
				wantVerdict(t, v, VOK, "ok")
				if adopted.unblockAt(adopted.cfg.slotIndex("xdax-a@vm1#0")) != since.Unix() {
					t.Fatalf("stalled since %d, want the settle's start %d", adopted.unblockAt(adopted.cfg.slotIndex("xdax-a@vm1#0")), since.Unix())
				}
				v, _ = checkStateAt(adopted, o, "", since.Add(361*time.Second), defaultUnblock)
				wantVerdict(t, v, VFail, "still not allocated 6m1s after the delete of the claim it waits for (A19, --stall-window 6m0s)")
			}
			// promoted: ok in both modes
			truth := s.clone()
			i := truth.cfg.slotIndex("xdax-a@vm1#0")
			if truth.Ctrl[PoolA].Up {
				truth.Slots[i].Phase = Running
				truth.Ctrl[PoolA].Att[0][VM1] = TriYes
				truth.Stacks[stackIndex(VM1, HB0)].push(devKey(pooledDev(PoolA)))
			} else {
				truth.Slots[i].Phase = Attaching
			}
			v, adopted, _ = checkAltsSince(s, observation(t, truth), "", since.Add(12*time.Second), since, defaultUnblock)
			wantVerdict(t, v, VOK, "ok")
			wantPhase(t, adopted, "xdax-a@vm1#0", truth.Slots[i].Phase)
			// the generator rules stay: the device is the contender's
			steps(t, s, "!create xnuma-a@vm2#0")
		}
	}
	// the flag: in the header, taken by a replay, in the expectations
	fs := flag.NewFlagSet("online", flag.ContinueOnError)
	var c commonFlags
	c.register(fs, true)
	if err := fs.Parse([]string{"--stall-window", "30s", "--seed", "1"}); err != nil {
		t.Fatal(err)
	}
	o, err := c.options(true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o.header, "--stall-window 30s") || o.cfg.StallWindow != 30*time.Second || !o.cfg.StallStrict {
		t.Fatalf("header %q, window %s strict %v", o.header, o.cfg.StallWindow, o.cfg.StallStrict)
	}
	fs2 := flag.NewFlagSet("online", flag.ContinueOnError)
	var c2 commonFlags
	c2.register(fs2, true)
	_ = fs2.Parse(nil)
	_, header := readSteps("# mbt-cxl online seed=1\n# " + o.header + "\nsettle\n")
	if err := applyHeader(fs2, header, io.Discard); err != nil || c2.stallWindow != 30*time.Second {
		t.Fatalf("replay: %v, window %s", err, c2.stallWindow)
	}
	if o2, _ := c2.options(true, io.Discard); o2.cfg.StallStrict != true {
		t.Fatalf("replay not strict")
	}
	// the default: not strict, 6m
	if d := defaultConfig(); d.StallWindow != 6*time.Minute || d.StallStrict {
		t.Fatalf("default %s %v", d.StallWindow, d.StallStrict)
	}
	st := newState(o.cfg).expect(1, "settle")
	cfg, err := st.config()
	if err != nil || cfg.StallWindow != 30*time.Second || !cfg.StallStrict {
		t.Fatalf("expectation: %v %s %v", err, cfg.StallWindow, cfg.StallStrict)
	}
}
