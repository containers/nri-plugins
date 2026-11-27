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

import (
	"strings"
	"testing"
	"time"
)

func TestStackPrimitives(t *testing.T) {
	var st Stack
	for _, k := range []RKey{rkLocal, devKey(2), devKey(3)} {
		st.push(k)
	}
	st.destroy(devKey(2))
	if st.String() != "[local,hole,b-shared0]" || !st.hasHole() {
		t.Fatalf("%s", st)
	}
	st.destroy(devKey(3))
	if st.String() != "[local]" || st.hasHole() {
		t.Fatalf("the top pops the hole below it: %s", st)
	}
	var other Stack
	other.push(rkLocal)
	if !st.equal(&other) || st != other {
		t.Fatalf("%v != %v", st, other)
	}
}

// TestBlockedUntilHoleClears: the sequence of A9 on vm0 hb0. A pod that
// needs a new region through a stack with a hole is blocked; it starts
// only after the region above the hole is gone.
func TestBlockedUntilHoleClears(t *testing.T) {
	cfg := testConfig(t, "full", false)
	s := steps(t, newState(cfg), "create ldax@vm0#0", "settle", "create xdax-b@vm0#0", "settle", "create sdax-b@vm0#0", "settle")
	si := stackIndex(VM0, HB0)
	wantStack(t, s, si, "[local,b-pooled0,b-shared0]")
	s = steps(t, s, "delete xdax-b@vm0#0", "settle")
	wantStack(t, s, si, "[local,hole,b-shared0]")
	s = steps(t, s, "create xnuma-b@vm0#0", "settle")
	wantPhase(t, s, "xnuma-b@vm0#0", Blocked)
	// a lease would need no new region: the local one exists
	s = steps(t, s, "create lease@vm0#0", "settle", "lease-get lease@vm0#0 dax", "settle")

	// the oracle: blocked as predicted
	o := observation(t, s)
	v, _ := checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	p := podOf(t, o, "xnuma-b@vm0#0")
	ev, evs := p.Event, p.Events
	// the text is in an older event (A12): found
	p.Event = &ObsEvent{Reason: "FailedPrepareDynamicResources", Message: "rpc error: code = Unavailable"}
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	p.Event, p.Events = nil, nil
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VWait, "no Warning event with \"CXL decoders released out of order\" (newest \"none\"")
	p.Event, p.Events = ev, evs
	node := "vm0"
	p.Phase, p.Ready, p.Node = "Running", true, &node
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VFail, "Running, but its decoder stack vm0/hb0 [local,hole,b-shared0] has a hole")
	// the ports show the hole
	o = observation(t, s)
	o.VMs["vm0"].Ports[0].Decoders[1].Region = "region9"
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VWait, "stack vm0/hb0: committed decoders [local,local,b-shared0], want [local,hole,b-shared0]")
	o.VMs["vm0"].Ports[0].Decoders[1].Region = "region77"
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VWait, "stack vm0/hb0: decoder of region77, a region of no known serial")

	// the region above the hole goes: the hole clears, the blocked pod
	// starts at kubelet's next retry
	s = steps(t, s, "delete sdax-b@vm0#0", "settle")
	wantStack(t, s, si, "[local]")
	wantPhase(t, s, "xnuma-b@vm0#0", Unblocking)
	// nothing else on the stack while it waits (the order would be
	// unknown), but its deletion
	s = steps(t, s, "!create sdax-b@vm0#0", "!lease-put lease@vm0#0", "!delete ldax@vm0#0")

	// still ContainerCreating: accepted, until the timeout
	o = observation(t, s)
	p = podOf(t, o, "xnuma-b@vm0#0")
	w := "ContainerCreating"
	p.Phase, p.Ready, p.Waiting = "Pending", false, &w
	now := time.Unix(time.Now().Unix(), 0)
	v, adopted := checkStateAt(s, o, "", now, time.Minute)
	wantVerdict(t, v, VOK, "ok")
	if ss := slotOf(t, adopted, "xnuma-b@vm0#0"); ss.Phase != Unblocking || adopted.unblockAt(cfg.slotIndex("xnuma-b@vm0#0")) != now.Unix() {
		t.Fatalf("adopted %+v", ss)
	}
	v, _ = checkStateAt(adopted, o, "", now.Add(2*time.Minute), time.Minute)
	wantVerdict(t, v, VFail, "not Running 2m0s after its decoder hole cleared (ContainerCreating)")
	// Running: adopted with its region on top
	sm := newSim(cfg, 1)
	sm.truth = adopted.clone()
	sm.truth.Slots[cfg.slotIndex("xnuma-b@vm0#0")].Phase = Running
	sm.truth.Stacks[si].push(devKey(pooledDev(PoolB)))
	v, adopted2 := checkStateAt(adopted, sm.observe(int(s.Steps), 1), "", now, time.Minute)
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted2, "xnuma-b@vm0#0", Running)
	wantStack(t, adopted2, si, "[local,b-pooled0]")
	// and the stack is free again
	steps(t, adopted2, "create sdax-b@vm0#0")
	// deleting the waiting pod is allowed, whatever its retry did
	s = steps(t, s, "delete xnuma-b@vm0#0", "settle")
	wantStack(t, s, si, "[local]")
}

// An "any" pod is not created while a stack of its pool's hosts has an
// unblocking pod.
func TestUnblockingStackNoAnyPod(t *testing.T) {
	s := stateAfter(t, "full", true,
		"create xnuma-c@vm1#0", "settle", "create sdax-c@vm1#0", "settle", // vm1/hb1 [c-pooled0,c-shared0]
		"delete xnuma-c@vm1#0", "settle", // [hole,c-shared0]
		"create xnuma-c@vm1#0", "settle", // blocked
		"delete sdax-c@vm1#0", "settle") // [] unblocking
	wantPhase(t, s, "xnuma-c@vm1#0", Unblocking)
	// (xdax-c@any#0 waits for c-pooled0: no region event, allowed)
	n := steps(t, s, "!create sdax-c@vm1#0", "!create sdax-c@any#0", "create xdax-c@any#0", "create sdax-a@vm1#0")
	wantPhase(t, n, "xdax-c@any#0", Pending)
}

// kubelet retries blocked pods independently: their order is adopted from
// the decoders.
func TestUnblockingOrderAdopted(t *testing.T) {
	s := stateAfter(t, "full", true,
		"create xnuma-b@vm0#0", "settle", "create lnuma@vm0#0", "settle", "create sdax-b@vm0#0", "settle", // vm0/hb0 [b-pooled0,local,b-shared0]
		"delete xnuma-b@vm0#0", "settle", // [hole,local,b-shared0]
		"create xdax-b@vm0#0", "settle", // blocked
		"delete lnuma@vm0#0", "settle", // [hole,hole,b-shared0]
		"create ldax@vm0#0", "settle", // blocked
		"delete sdax-b@vm0#0", "settle") // [] both unblocking
	for _, sl := range []string{"xdax-b@vm0#0", "ldax@vm0#0"} {
		wantPhase(t, s, sl, Unblocking)
	}
	n := steps(t, s, "create plain@vm0#0", "settle")
	si := stackIndex(VM0, HB0)
	for _, order := range [][]RKey{{rkLocal, devKey(pooledDev(PoolB))}, {devKey(pooledDev(PoolB)), rkLocal}} {
		truth := n.clone()
		for _, sl := range []string{"xdax-b@vm0#0", "ldax@vm0#0"} {
			truth.Slots[truth.cfg.slotIndex(sl)].Phase = Running
		}
		truth.Stacks[si] = Stack{}
		for _, k := range order {
			truth.Stacks[si].push(k)
		}
		truth.growFence()
		v, adopted := checkStateAt(n, observation(t, truth), "", time.Now(), defaultUnblock)
		wantVerdict(t, v, VOK, "ok")
		if !adopted.Stacks[si].equal(&truth.Stacks[si]) {
			t.Fatalf("adopted %s, the SUT has %s", adopted.Stacks[si], truth.Stacks[si])
		}
	}
}

// No region is made in the window that clears a hole with a pod blocked
// behind it: its retry could come first.
func TestNoRegionInClearingWindow(t *testing.T) {
	s := stateAfter(t, "full", true,
		"create xnuma-c@vm1#0", "settle", "create sdax-c@vm1#0", "settle", // [c-pooled0,c-shared0]
		"delete xnuma-c@vm1#0", "settle", // [hole,c-shared0]
		"create xnuma-c@vm1#0", "settle") // blocked
	wantPhase(t, s, "xnuma-c@vm1#0", Blocked)
	n := steps(t, s, "delete sdax-c@vm1#0", "!create sexcl-c@vm1#0", "!create sdax-c@vm1#0", "settle")
	wantPhase(t, n, "xnuma-c@vm1#0", Unblocking)
}

// Two unblocking users of one region (the local memdev of a VM): one
// region when they run, whatever the order of their retries.
func TestUnblockingUsersOfOneRegion(t *testing.T) {
	s := stateAfter(t, "full", true,
		"create xnuma-a@vm2#0", "settle", "create sdax-a@vm2#0", "settle", // vm2/hb0 [a-pooled0,a-shared0]
		"delete xnuma-a@vm2#0", "settle", // [hole,a-shared0]
		"create lnuma@vm2#0", "settle", "create lsteer@vm2#0", "settle", // both blocked
		"delete sdax-a@vm2#0", "settle") // [] both unblocking
	wantPhase(t, s, "lnuma@vm2#0", Unblocking)
	wantPhase(t, s, "lsteer@vm2#0", Unblocking)
	truth := s.clone()
	for _, sl := range []string{"lnuma@vm2#0", "lsteer@vm2#0"} {
		truth.Slots[truth.cfg.slotIndex(sl)].Phase = Running
	}
	truth.Slots[truth.cfg.slotIndex("lsteer@vm2#0")].Steer = SteerOn
	truth.Stacks[stackIndex(VM2, HB0)].push(rkLocal)
	truth.growFence()
	v, adopted := checkState(s, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	wantStack(t, adopted, stackIndex(VM2, HB0), "[local]")
}

// A pool pod is bound only at PreBind's next poll after its claim is
// Attached (up to 5 s later): an unblocking pod seen without a node is not
// settled (its delete would be that of an unbound claim).
func TestUnblockingPodNotBound(t *testing.T) {
	s := stateAfter(t, "full", true,
		"create xnuma-a@vm2#0", "settle", "create sdax-a@vm2#0", "settle", // vm2/hb0 [a-pooled0,a-shared0]
		"delete xnuma-a@vm2#0", "settle", // [hole,a-shared0]
		"create xdax-a@vm2#0", "settle", // blocked
		"delete sdax-a@vm2#0", "settle") // unblocking
	wantPhase(t, s, "xdax-a@vm2#0", Unblocking)
	o := observation(t, s)
	podOf(t, o, "xdax-a@vm2#0").Node = nil
	v, _ := checkState(s, o, "")
	wantVerdict(t, v, VWait, "xdax-a@vm2#0: not bound yet")
}

// TestAdoptedOrder: two regions made in one window on one stack race for
// the order; on-line the observed order is adopted, and what a later
// destroy does depends on it.
func TestAdoptedOrder(t *testing.T) {
	cfg := testConfig(t, "full", true)
	si := stackIndex(VM2, HB0)
	s := steps(t, newState(cfg), "create lnuma@vm2#0", "create xdax-a@vm2#0", "settle")
	var orders []string
	for _, a := range alts(s) {
		orders = append(orders, a.Stacks[si].String())
	}
	if strings.Join(orders, " ") != "[local,a-pooled0] [a-pooled0,local]" {
		t.Fatalf("outcomes %v", orders)
	}
	seen := map[string]bool{}
	for seed := int64(1); seed <= 20 && len(seen) < 2; seed++ {
		sm := newSim(cfg, seed)
		sm.truth = sm.resolve(s)
		v, adopted := checkState(s, sm.observe(int(s.Steps), 1), "")
		wantVerdict(t, v, VOK, "ok")
		got := adopted.Stacks[si].String()
		if got != sm.truth.Stacks[si].String() {
			t.Fatalf("adopted %s, the SUT has %s", got, sm.truth.Stacks[si])
		}
		seen[got] = true
		// deleting lnuma: a hole only if local is not the top
		after := steps(t, adopted, "delete lnuma@vm2#0", "settle")
		want := "[a-pooled0]"
		if got == "[local,a-pooled0]" {
			want = "[hole,a-pooled0]"
		}
		wantStack(t, after, si, want)
		// without the ports the oracle cannot tell: it takes the issue
		// order
		o := sm.observe(int(s.Steps), 1)
		for _, ov := range o.VMs {
			ov.Ports = nil
		}
		v, adopted = checkState(s, o, "")
		wantVerdict(t, v, VOK, "ok")
		wantStack(t, adopted, si, "[local,a-pooled0]")
	}
	if len(seen) != 2 {
		t.Fatalf("the simulated SUT took one order only: %v", seen)
	}
	// off-line the race is not generated
	off := testConfig(t, "full", false)
	steps(t, newState(off), "create lnuma@vm2#0", "!create xdax-a@vm2#0", "create xdax-c@vm2#0")
}

func TestAvoidHoles(t *testing.T) {
	cfg := testConfig(t, "full", false)
	cfg.AvoidHoles = true
	s := steps(t, newState(cfg), "create lnuma@vm2#0", "settle", "create xdax-a@vm2#0", "settle")
	// lnuma's region is below a-pooled0's: no; with another user of the
	// local region, deleting one destroys nothing
	s = steps(t, s, "!delete lnuma@vm2#0", "create lnuma@vm2#1", "settle", "delete lnuma@vm2#1", "settle", "!delete lnuma@vm2#0",
		"delete xdax-a@vm2#0", "settle", "delete lnuma@vm2#0", "settle")
	wantStack(t, s, stackIndex(VM2, HB0), "[]")
}

func TestLeaseNeedsRegion(t *testing.T) {
	cfg := testConfig(t, "full", false)
	s := steps(t, newState(cfg), "create xdax-b@vm0#0", "settle", "create sdax-b@vm0#0", "settle", "delete xdax-b@vm0#0", "settle",
		"create lease@vm0#0", "settle")
	wantStack(t, s, stackIndex(VM0, HB0), "[hole,b-shared0]")
	// the companion's slice would need the local region: refused, the
	// lease-get would fail after its --wait
	s = steps(t, s, "!lease-get lease@vm0#0 numa", "!lease-get lease@vm0#0 dax", "create ldax@vm0#0", "settle")
	wantPhase(t, s, "ldax@vm0#0", Blocked)
}

// TestStackUsersOfOneRegion: shares of one device on one node use one
// region; the last one destroys it.
func TestStackUsersOfOneRegion(t *testing.T) {
	cfg := testConfig(t, "full", false)
	si := stackIndex(VM2, HB0)
	s := steps(t, newState(cfg), "create sdax-a@vm2#0", "settle", "create lnuma@vm2#0", "settle", "create sdax-a@vm2#1", "create sro-a@vm2#0", "settle")
	wantStack(t, s, si, "[a-shared0,local]")
	s = steps(t, s, "delete sdax-a@vm2#0", "settle", "delete sro-a@vm2#0", "settle")
	wantStack(t, s, si, "[a-shared0,local]")
	s = steps(t, s, "delete sdax-a@vm2#1", "settle")
	wantStack(t, s, si, "[hole,local]")
	// a lease slice on vm2 reuses the local region: fine with the hole
	s = steps(t, s, "create lease@vm2#0", "settle", "lease-get lease@vm2#0 numa", "settle", "delete lnuma@vm2#0", "settle")
	wantStack(t, s, si, "[hole,local]")
	s = steps(t, s, "lease-put lease@vm2#0", "settle")
	wantStack(t, s, si, "[]")
}

// A claim that gets a device only once the old users' claims are gone
// prepares after their Unprepare: the old region is destroyed (here: a
// hole), never reused. Also when the waiter is promoted while its pool's
// controller is down and prepares at ctrl-start; a share that fits at once
// waits for no release and may come first.
func TestHandover(t *testing.T) {
	for _, c := range []struct {
		name, slot string
		si         int
		lines      []string
		outcomes   string // phase and stack of each outcome
	}{
		{"exclusive", "xdax-a@vm1#0", stackIndex(VM1, HB0), []string{
			"create xnuma-a@vm1#0", "settle", "create lnuma@vm1#0", "settle", // vm1/hb0 [a-pooled0,local]
			"delete xnuma-a@vm1#0", "create xdax-a@vm1#0", "settle"},
			"blocked [hole,local]"},
		{"shared taken exclusively", "sexcl-c@vm1#0", stackIndex(VM1, HB1), []string{
			"create sdax-c@vm1#0", "settle", "create xnuma-c@vm1#0", "settle", // vm1/hb1 [c-shared0,c-pooled0]
			"delete sdax-c@vm1#0", "create sexcl-c@vm1#0", "settle"},
			"blocked [hole,c-pooled0]"},
		{"after every share", "sexcl-a@vm2#0", stackIndex(VM2, HB0), []string{
			"create sdax-a@vm2#0", "create sdax-a@vm2#1", "settle", "create lnuma@vm2#0", "settle", // vm2/hb0 [a-shared0,local]
			"create sexcl-a@vm2#0", "settle", // pending
			"delete sdax-a@vm2#0", "delete sdax-a@vm2#1", "settle"},
			"blocked [hole,local]"},
		{"at ctrl-start", "sexcl-a@vm2#0", stackIndex(VM2, HB0), []string{
			"create sdax-a@vm2#0", "settle", "create lnuma@vm2#0", "settle", // vm2/hb0 [a-shared0,local]
			"create sexcl-a@vm2#0", "settle", // waits for the share
			"ctrl-kill a", "delete sdax-a@vm2#0", "ctrl-start a", "settle"},
			"blocked [hole,local]"},
		{"share at ctrl-start", "sro-a@vm2#0", stackIndex(VM2, HB0), []string{
			"create sdax-a@vm2#0", "settle", "create lnuma@vm2#0", "settle",
			"ctrl-kill a", "delete sdax-a@vm2#0", "create sro-a@vm2#0", "ctrl-start a", "settle"},
			"blocked [hole,local]; running [a-shared0,local]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := stateAfter(t, "full", true, c.lines...)
			var got []string
			for _, a := range alts(s) {
				got = append(got, slotOf(t, a, c.slot).Phase.String()+" "+a.Stacks[c.si].String())
			}
			if strings.Join(got, "; ") != c.outcomes {
				t.Fatalf("outcomes %q, want %q:\n%s", strings.Join(got, "; "), c.outcomes, dumpAlts(s))
			}
		})
	}
	// a SUT that kept the region and reused it fails at once
	s := stateAfter(t, "full", true,
		"create xnuma-a@vm1#0", "settle", "create lnuma@vm1#0", "settle",
		"delete xnuma-a@vm1#0", "create xdax-a@vm1#0", "settle")
	leaked := s.clone()
	leaked.Slots[leaked.cfg.slotIndex("xdax-a@vm1#0")].Phase = Running
	leaked.Stacks[stackIndex(VM1, HB0)] = Stack{}
	leaked.Stacks[stackIndex(VM1, HB0)].push(devKey(pooledDev(PoolA)))
	leaked.Stacks[stackIndex(VM1, HB0)].push(rkLocal)
	v, _ := checkState(s, observation(t, leaked), "")
	wantVerdict(t, v, VFail, "Running, but its decoder stack vm1/hb0 [hole,local] has a hole")
}

// simStep applies one action line to the model and to the simulated SUT:
// a settle at the oracle's clock now (also the sim's), checked by the
// oracle; the adopted state.
func simStep(t *testing.T, sm *sim, model *State, n int, line string, now time.Time) *State {
	t.Helper()
	a := mustAct(t, model.cfg, line)
	if !model.enabled(a) {
		t.Fatalf("step %d %q: not enabled in\n%s", n, line, model.dump())
	}
	next := model.apply(a)
	if a.Verb != VSettle {
		d := sm.do(n, a)
		if v := checkDone(expectDone(model, next, a, n), d); v.Status != VOK {
			t.Fatalf("step %d %q: %s", n, line, v.Msg)
		}
		return adoptDone(next, a, d)
	}
	sm.now, sm.since = now, now
	sm.do(n, a)
	v, adopted := checkStateAt(next, sm.observe(n, 1), "", now, defaultUnblock)
	if v.Status != VOK {
		t.Fatalf("step %d settle at +%s: the oracle disagrees with the simulated SUT: %s %s\nmodel:\n%s\ntruth:\n%s", n, now.Sub(refEpoch), v.Status, v.Msg, next.dump(), sm.truth.dump())
	}
	return adopted
}

// The simulated SUT's retry of an unblocking pod and its promotion of a
// stalled contender are a coin at every settle, but never later than the
// oracle allows by its clock (A9: kubelet's back-off is capped at 5 min;
// A19: --stall-window), also when the settles are minutes apart: here
// they come 80 s apart, and a pod kept Unblocking through 5 of them
// would be later than the back-off allows.
func TestSimRetryDeadlines(t *testing.T) {
	const gap = 80 * time.Second
	cfg := testConfig(t, "full", true)
	run := func(seed int64, slot string, want Phase, lines ...string) (settles int) {
		sm := newSim(cfg, seed)
		model := newState(cfg)
		now := refEpoch
		for n, line := range lines {
			if line == "settle" {
				now = now.Add(gap)
			}
			model = simStep(t, sm, model, n+1, line, now)
		}
		// (the coin of the settle that first saw it so may have been
		// heads: settles 0)
		i := cfg.slotIndex(slot)
		first := now
		for k := 1; model.Slots[i].Phase == want; k++ {
			if now.Sub(first) > 6*time.Minute+gap {
				t.Fatalf("seed %d: %s still %s %s after the first settle that saw it so", seed, slot, want, now.Sub(first))
			}
			// (a window of a pod elsewhere, then its settle)
			other := "create plain@vm2#0"
			if k%2 == 0 {
				other = "delete plain@vm2#0"
			}
			model = simStep(t, sm, model, len(lines)+2*k-1, other, now)
			now = now.Add(gap)
			model = simStep(t, sm, model, len(lines)+2*k, "settle", now)
			settles = k
		}
		return settles
	}
	hole := []string{"create ldax@vm0#0", "settle", "create xdax-b@vm0#0", "settle", "create sdax-b@vm0#0", "settle",
		"delete xdax-b@vm0#0", "settle", "create xnuma-b@vm0#0", "settle", "delete sdax-b@vm0#0", "settle"}
	stall := []string{"ctrl-kill a", "create xnuma-a@vm1#0", "settle", "create xdax-a@vm1#0", "settle", "delete xnuma-a@vm1#0", "settle"}
	var forced, early [2]int
	for seed := int64(1); seed <= 64; seed++ {
		// kubelet: by the settle 300 s after the one that saw the hole
		// clear (the 4th one after it, at 320 s)
		switch k := run(seed, "xnuma-b@vm0#0", Unblocking, hole...); {
		case k > 4:
			t.Fatalf("seed %d: unblocked at the %dth settle", seed, k)
		case k == 4:
			forced[0]++
		default:
			early[0]++
		}
		// the scheduler: within --stall-window (6m) of the settle that
		// first saw it stalled (the 5th one after it, at 400 s)
		switch k := run(seed, "xdax-a@vm1#0", Stalled, stall...); {
		case k > 5:
			t.Fatalf("seed %d: promoted at the %dth settle", seed, k)
		case k == 5:
			forced[1]++
		default:
			early[1]++
		}
	}
	if forced[0] == 0 || early[0] == 0 || forced[1] == 0 || early[1] == 0 {
		t.Fatalf("forced %v, by the coin %v: want both", forced, early)
	}
}
