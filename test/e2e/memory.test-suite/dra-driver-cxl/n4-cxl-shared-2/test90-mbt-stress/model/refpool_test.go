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

// The fuzz of the model's pool rules against the independent reference
// (refpool.go): many short random sequences heavy in controller and
// server faults mixed with creates, deletes and crashes of the pool
// kinds of all three pools, every settle polled like bash does (0.5 s,
// then every 3 s of the reference's clock) and checked by the oracle,
// with the settle timeout of 180 s on that clock.

import (
	"flag"
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// The knobs of a longer fuzz by hand, for instance
// go test -run Fuzz -refpool-seqs 4000 -refpool-len 120.
var (
	refSeqs = flag.Int("refpool-seqs", 400, "random sequences of TestRefPoolFuzz (a quarter of them for TestFencingFuzz, 1/16 for TestRebootFuzz)")
	refSeed = flag.Int64("refpool-seed", 1, "first seed of the fuzz tests")
	refMax  = flag.Int("refpool-len", 60, "longest sequence of the fuzz tests")
)

// fuzzMode is the configuration of the model in a fuzz.
type fuzzMode struct {
	profile string
	fencing FencingRun
	reboot  RebootRun
}

type fuzzStats struct {
	ref                                  map[string]int
	seqs, steps, settles, polls          int
	stalled, unsched, absent, eitherSeen int
	verbs                                map[Verb]int
}

type fuzzFailure struct {
	seed  int64
	steps []string
	msg   string
	diffs []string
	model string
	ref   string
	hist  []string
}

func (f *fuzzFailure) String() string {
	return fmt.Sprintf("seed %d: %s\nsteps:\n  %s\ndifferences:\n  %s\nmodel before the step:\n%s\n%s\nreference log:\n  %s",
		f.seed, f.msg, strings.Join(f.steps, "\n  "), strings.Join(f.diffs, "\n  "), f.model, f.ref, strings.Join(f.hist, "\n  "))
}

// fuzzWeight is the weight of an action in the fuzz.
func fuzzWeight(cfg *Config, a Act) float64 {
	switch a.Verb {
	case VSettle:
		return 4
	case VCtrlStart, VCtrlKill, VCtrlRestart:
		return 2.5
	case VServerRestart:
		return 1
	case VKubeletRestart:
		return 0.4
	case VDriverKill, VDriverRestart, VContainerdRestart:
		return 0.15
	case VFencingSwitch:
		return 0.3
	case VNodeReboot:
		// (enabled only after a settle with every controller up)
		return 2
	case VPWrite, VLeaseGet, VLeasePut:
		return 1.5
	case VPRead, VPWipe, VDriverKillAt:
		return 0.5
	case VHold, VUnhold:
		return 1
	case VCreate, VDelete, VCrash:
		sl := &cfg.Slots[a.Slot]
		w := 1.0
		if a.Verb == VDelete {
			w = 2.5
		}
		if a.Verb == VCrash {
			w = 0.3
		}
		if !sl.Kind.pooled() {
			w *= 0.25
		}
		return w
	}
	return 0
}

// refSequence runs a sequence against the simulated SUT with the
// reference: random (script nil) or the actions of a script (each must
// be enabled in the model; then a final settle).
func refSequence(cfg *Config, acts []Act, seed int64, maxLen int, script []string, st *fuzzStats) *fuzzFailure {
	rng := rand.New(rand.NewSource(seed))
	sm := newSim(cfg, seed).withRef(seed * 7919)
	sm.ref.RequeueOnUnreserve = cfg.StallStrict && !refUnfixedScheduler
	model := newState(cfg)
	length := len(script)
	if script == nil {
		length = 4 + rng.Intn(maxLen-3)
	}
	var lines []string
	settle := Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool}
	defer func() {
		for k, v := range sm.ref.Count {
			st.ref[k] += v
		}
	}()
	fail := func(msg string, diffs []string, before *State) *fuzzFailure {
		return &fuzzFailure{seed: seed, steps: lines, msg: msg, diffs: diffs, model: before.dump(), ref: sm.ref.String(), hist: sm.ref.History()}
	}
	for n := 1; ; n++ {
		var a Act
		var pred *State
		switch {
		case len(sm.ref.Anomalies) > 0:
			return fail("reference anomaly: "+sm.ref.Anomalies[0], sm.ref.Anomalies, model)
		case n > length && model.Unsettled == 0:
			st.seqs++
			return nil
		case n > length:
			a, pred = settle, model.next(settle)
		case script != nil:
			x, err := parseAct(cfg, script[n-1])
			if err != nil {
				return fail(err.Error(), nil, model)
			}
			a, pred = x, model.next(x)
			if pred == nil {
				lines = append(lines, a.name(cfg))
				return fail("not enabled: "+script[n-1], nil, model)
			}
		default:
			var cand []Act
			var preds []*State
			var ws []float64
			total := 0.0
			for _, x := range acts {
				w := fuzzWeight(cfg, x)
				if w == 0 {
					continue
				}
				if p := model.next(x); p != nil {
					cand, preds, ws = append(cand, x), append(preds, p), append(ws, w)
					total += w
				}
			}
			if len(cand) == 0 {
				a, pred = settle, model.next(settle)
				break
			}
			x := rng.Float64() * total
			j := 0
			for ; j < len(ws)-1 && x >= ws[j]; j++ {
				x -= ws[j]
			}
			a, pred = cand[j], preds[j]
		}
		if pred == nil {
			return fail("no enabled action and no settle", nil, model)
		}
		lines = append(lines, a.name(cfg))
		st.steps++
		st.verbs[a.Verb]++
		if a.Verb != VSettle {
			d := sm.do(n, a)
			if v := checkDone(expectDone(model, pred, a, n), d); v.Status != VOK {
				return fail(v.Msg, v.Diffs, model)
			}
			model = adoptDone(pred, a, d)
			continue
		}
		st.settles++
		if dbgPred != nil {
			dbgPred(pred, sm)
		}
		sm.do(n, a)
		start := sm.ref.Now()
		since := sm.refWall()
		for poll := 1; ; poll++ {
			st.polls++
			sm.refPoll(poll)
			o := sm.observe(n, poll)
			v, adopted, _ := checkAltsSince(pred, o, "", sm.refWall(), since, defaultUnblock)
			elapsed := time.Duration(sm.ref.Now()-start) * time.Millisecond
			if v.Status == VWait && elapsed >= 180*time.Second {
				return fail("settle timeout after "+elapsed.Round(time.Second).String()+": "+v.Msg, v.Diffs, model)
			}
			if v.Status == VFail {
				return fail(v.Msg, v.Diffs, model)
			}
			if v.Status == VOK {
				for i := range adopted.Slots {
					switch adopted.Slots[i].Phase {
					case Stalled:
						st.stalled++
					case Unsched:
						st.unsched++
					}
				}
				for p := range adopted.Ctrl {
					if adopted.Ctrl[p].ResourceSlices == SlicesAbsent {
						st.absent++
					}
				}
				if len(pred.Alts) > 0 {
					st.eitherSeen++
				}
				model = adopted
				break
			}
		}
		sm.pend = nil
	}
}

var dbgPred func(*State, *sim)

// refUnfixedScheduler: the reference's scheduler that does not requeue at once
// also in strict stall mode (a test of the mode's failure).
var refUnfixedScheduler bool

var reNums = regexp.MustCompile(`[0-9]+(\.[0-9]+)?`)

// runRefFuzz runs sequences and returns the failures by message kind.
func runRefFuzz(t *testing.T, m fuzzMode, seqs int, seed0 int64, maxLen int) (map[string][]*fuzzFailure, *fuzzStats) {
	t.Helper()
	cfg := testConfig(t, m.profile, true)
	cfg.Unsettled = 3 // MBT_UNSETTLED of the runs
	cfg.Fencing, cfg.Reboot = m.fencing, m.reboot
	acts := allActs(cfg)
	st := &fuzzStats{verbs: map[Verb]int{}, ref: map[string]int{}}
	kinds := map[string][]*fuzzFailure{}
	for k := 0; k < seqs; k++ {
		if f := refSequence(cfg, acts, seed0+int64(k), maxLen, nil, st); f != nil {
			key := reNums.ReplaceAllString(f.msg, "N")
			kinds[key] = append(kinds[key], f)
		}
	}
	return kinds, st
}

// TestRefPoolFuzz: the model agrees with the reference of the pool side
// in random sequences of pool faults (-refpool-seqs N for more).
func TestRefPoolFuzz(t *testing.T) {
	seqs := *refSeqs
	if testing.Short() {
		seqs = 50
	}
	kinds, st := runRefFuzz(t, fuzzMode{profile: "full"}, seqs, *refSeed, *refMax)
	t.Logf("%d sequences (%d failed), %d steps, %d settles, %d polls; adopted: stalled %d, unsched %d, slices absent %d; settles with alternatives %d; verbs %v; reference %v",
		seqs, seqs-st.seqs, st.steps, st.settles, st.polls, st.stalled, st.unsched, st.absent, st.eitherSeen, st.verbs, st.ref)
	var keys []string
	for k := range kinds {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(kinds[keys[i]]) > len(kinds[keys[j]]) })
	for _, k := range keys {
		fs := kinds[k]
		sort.Slice(fs, func(i, j int) bool { return len(fs[i].steps) < len(fs[j].steps) })
		t.Errorf("%d x %s\nshortest:\n%s", len(fs), k, fs[0])
	}
}

// TestFencingFuzz is TestRefPoolFuzz with the fencing modes none and
// switch (A36, A37), the two-claim kind on both CXL NUMA nodes (A39) in every
// sequence's profile.
func TestFencingFuzz(t *testing.T) {
	seqs := *refSeqs / 4
	if testing.Short() {
		seqs = 15
	}
	for _, mode := range []FencingRun{FencingNone, FencingSwitch} {
		kinds, st := runRefFuzz(t, fuzzMode{profile: "full", fencing: mode}, seqs, *refSeed+1000, *refMax)
		t.Logf("fencing %s: %d sequences (%d failed), %d steps, %d settles; verbs %v", mode, seqs, seqs-st.seqs, st.steps, st.settles, st.verbs)
		for k, fs := range kinds {
			sort.Slice(fs, func(i, j int) bool { return len(fs[i].steps) < len(fs[j].steps) })
			t.Errorf("fencing %s: %d x %s\nshortest:\n%s", mode, len(fs), k, fs[0])
		}
	}
}

// refScript runs a script of a profile against the reference with a
// --stall-window (0: the default; not above 180 s: strict, and the
// reference's scheduler requeues a stalled contender); the failure if any.
func refScript(t *testing.T, profile string, seed int64, window time.Duration, lines ...string) *fuzzFailure {
	t.Helper()
	cfg := testConfig(t, profile, true)
	cfg.Unsettled = 3
	if window > 0 {
		cfg.StallWindow, cfg.StallStrict = window, window <= 180*time.Second
	}
	return refSequence(cfg, allActs(cfg), seed, 0, lines, &fuzzStats{verbs: map[Verb]int{}, ref: map[string]int{}})
}

func repeat(n int, lines ...string) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, lines...)
	}
	return out
}

// The controller down for longer than the binding timeout (600 s) with
// a contender of the waiting claim's device.
func TestRefPoolBindingTimeout(t *testing.T) {
	for _, start := range [][]string{
		{"ctrl-kill a", "create xnuma-a@vm1#0", "settle", "create xdax-a@vm1#0", "settle"},
		{"ctrl-kill a", "create xnuma-a@vm1#0", "settle"}, // no waiter: allocated again
		{"ctrl-kill c", "create sexcl-c@vm1#0", "settle", "create sdax-c@vm2#0", "settle"},
		{"ctrl-kill c", "create sdax-c@vm0#0", "settle", "create sexcl-c@vm1#0", "settle"},
	} {
		for seed := int64(1); seed <= 10; seed++ {
			lines := append(append([]string{"create plain@vm0#0"}, start...), repeat(120, "server-restart b", "settle")...)
			lines = append(lines, "ctrl-start "+strings.Fields(start[0])[1], "settle")
			if f := refScript(t, "full", seed, 0, lines...); f != nil {
				t.Errorf("%s", f)
				return
			}
		}
	}
}

// refSequences are sequences of the fuzz against the reference that
// disagreed once, with the profile and modes of their run: each runs
// without a disagreement now, or the model refuses its step refused and
// the steps before it (then a settle) run without one.
var refSequences = []struct {
	why     string
	profile string
	reboot  RebootRun
	fencing FencingRun
	window  time.Duration // --stall-window; 0: the default
	seed    int64
	refused int // the step that the model refuses, 1-based; 0: none
	steps   string
}{
	{why: "F2: hold after the settle: an \"any\" contender created in the next window stalls", profile: "full", seed: 40,
		steps: "create xnuma-c@vm1#0 | create sdax-b@vm0#1 | ctrl-kill c | settle | create xnuma-a@vm1#0 | " +
			"create sro-b@vm0#0 | settle | create sdax-a@vm2#1 | ctrl-restart b | delete xnuma-a@vm1#0 " +
			"| settle | ctrl-kill a | delete xnuma-c@vm1#0 | create xdax-a@vm1#0 | settle | create " +
			"xdax-c@any#0 | create sexcl-a@vm2#0 | delete sdax-a@vm2#1 | settle"},
	{why: "F2: hold after the settle (an attaching holder deleted)", profile: "full", seed: 106,
		steps: "create xdax-c@any#0 | create lnuma@vm2#0 | create sdax-b@vm0#1 | settle | create " +
			"lnuma@vm2#1 | delete sdax-b@vm0#1 | create plain@vm1#0 | settle | create sdax-b@vm0#0 | " +
			"create xnuma-a@vm1#0 | ctrl-kill a | settle | create sexcl-a@vm2#0 | delete xnuma-a@vm1#0 " +
			"| create lnuma@vm0#0 | settle | create xdax-a@vm1#0 | create xdax-b@vm0#0 | driver-kill " +
			"vm0 | settle"},
	{why: "F2: hold after the settle (a holder deleted while starting)", profile: "full", seed: 782,
		steps: "create xdax-b@vm0#0 | delete xdax-b@vm0#0 | ctrl-restart c | settle | create sdax-c@vm1#0 " +
			"| driver-kill vm1 | create xnuma-b@vm0#0 | settle"},
	{why: "F3: a stalled contender promoted through a hole is blocked", profile: "full", seed: 660,
		steps: "create sdax-a@vm2#1 | create lsteer@vm2#0 | create plain@vm2#0 | settle | kubelet-restart " +
			"vm0 | create xdax-a@vm1#0 | ctrl-kill a | settle | delete sdax-a@vm2#1 | server-restart c " +
			"| create xnuma-c@vm1#0 | settle | create lease@vm1#0 | create sexcl-a@vm2#0 | ctrl-kill c " +
			"| settle | create sexcl-c@vm1#0 | settle | delete xnuma-c@vm1#0 | server-restart b | " +
			"settle | create sdax-a@vm2#0 | create sdax-b@vm0#0 | create xdax-a@vm2#0 | settle | delete " +
			"sexcl-c@vm1#0 | ctrl-restart b | create sdax-b@vm0#1 | settle | delete sexcl-a@vm2#0 | " +
			"server-restart a | delete lease@vm1#0 | settle | ctrl-kill b | create xnuma-c@vm1#0 | " +
			"delete xnuma-c@vm1#0 | settle | create sro-a@vm2#0 | delete xdax-a@vm2#0 | settle | create " +
			"sexcl-c@vm1#0 | ctrl-start a | create sdax-a@vm2#1 | settle"},
	{why: "F4: two claims wait for the slices published again (refused)", profile: "full", seed: 234, refused: 19,
		steps: "ctrl-restart c | create xdax-a@vm1#0 | create lnuma@vm0#0 | settle | ctrl-restart a | " +
			"ctrl-kill a | ctrl-restart c | settle | create sro-a@vm1#0 | ctrl-kill b | driver-kill vm2 " +
			"| settle | delete sro-a@vm1#0 | ctrl-restart c | ctrl-start b | settle | create " +
			"sdax-a@vm1#0 | ctrl-start a | create sexcl-a@vm2#0 | settle"},
	{why: "F4: two creates while the slices are unknown (refused)", profile: "full", seed: 216, refused: 3,
		steps: "ctrl-restart a | create xdax-a@vm2#0 | create xnuma-a@vm1#0 | settle"},
	{why: "F5: ctrl-kill: the attach of a stalled contender in flight", profile: "full", seed: 352,
		steps: "create lnuma@vm0#0 | create sexcl-a@vm2#0 | create ldax@vm1#0 | settle | ctrl-kill a | " +
			"create sdax-c@vm2#0 | ctrl-kill b | settle | create sdax-c@vm1#0 | create sdax-c@vm0#0 | " +
			"delete sdax-c@vm1#0 | settle | create sdax-c@vm1#0 | ctrl-start a | ctrl-kill c | settle"},
	{why: "F5: ctrl-kill: the attach of a claim deleted before it was bound", profile: "full", seed: 477,
		steps: "ctrl-restart b | create lsteer@vm2#0 | ctrl-restart a | settle | create sdax-b@vm0#1 | " +
			"delete sdax-b@vm0#1 | ctrl-kill b | settle"},
	{why: "F5: ctrl-kill: an orphan detached by the start's reconcile", profile: "full", seed: 4208,
		steps: "server-restart b | create xnuma-a@vm1#0 | create sdax-c@any#0 | settle | ctrl-kill a | " +
			"create sro-c@vm2#0 | create lease@vm2#0 | settle | create sro-c@vm0#0 | ctrl-kill b | " +
			"settle | delete sro-c@vm2#0 | ctrl-restart c | ctrl-kill c | settle | create xdax-a@vm1#0 " +
			"| create ldax@vm2#0 | crash sdax-c@any#0 | settle | create sdax-b@vm0#1 | create " +
			"sdax-a@vm1#0 | create sdax-c@vm2#0 | settle | create sro-a@vm1#0 | delete sro-a@vm1#0 | " +
			"settle | delete sdax-b@vm0#1 | ctrl-start c | ctrl-kill c | settle"},
	{why: "F7: ctrl-start prepares an \"any\" pod on a stack of a stalled pod (refused)", profile: "full", seed: 1371, refused: 26,
		steps: "ctrl-kill c | create sro-b@vm0#0 | create sro-c@vm2#0 | settle | ctrl-kill b | create " +
			"lnuma@vm2#0 | ctrl-kill a | settle | create lsteer@vm2#0 | create plain@vm2#0 | create " +
			"xnuma-a@vm1#0 | settle | create ldax@vm2#0 | ctrl-start b | create xdax-c@any#0 | settle | " +
			"create xnuma-c@vm1#0 | delete sro-c@vm2#0 | delete xdax-c@any#0 | settle | ctrl-kill b | " +
			"server-restart a | create ldax@vm0#0 | settle | create sdax-c@any#0 | ctrl-start c | " +
			"delete ldax@vm2#0 | settle"},
	{why: "F8: the claim of a pod gone at the last settle not deallocated yet (refused)", profile: "full", seed: 6047, refused: 42,
		steps: "create xdax-a@vm2#0 | create ldax@vm2#0 | create sdax-c@vm1#0 | settle | create " +
			"sdax-c@any#0 | create plain@vm0#0 | create xnuma-c@vm1#0 | settle | create sdax-c@vm0#0 | " +
			"create sro-c@vm2#0 | ctrl-kill c | settle | ctrl-kill b | delete ldax@vm2#0 | ctrl-kill a " +
			"| settle | create sdax-a@vm1#0 | create sdax-b@vm0#0 | kubelet-restart vm1 | settle | " +
			"create ldax@vm2#0 | delete sdax-c@any#0 | driver-kill vm2 | settle | crash xdax-a@vm2#0 | " +
			"create lnuma@vm2#0 | create lnuma@vm2#1 | settle | create sdax-c@any#0 | create " +
			"sro-a@vm2#0 | delete sro-a@vm2#0 | settle | delete sdax-c@vm1#0 | delete sdax-c@vm0#0 | " +
			"create xdax-a@vm1#0 | settle | create sdax-c@vm0#0 | delete sdax-c@any#0 | delete " +
			"xnuma-c@vm1#0 | settle | create xnuma-c@vm1#0 | create xdax-c@vm2#0 | delete xdax-a@vm2#0 " +
			"| settle"},
	{why: "F9: promotion after an unbound delete of the same device in the window", profile: "full", seed: 18363,
		steps: "ctrl-kill b | ctrl-kill c | create sdax-a@vm2#1 | settle | delete sdax-a@vm2#1 | create " +
			"sdax-c@vm0#0 | delete sdax-c@vm0#0 | settle | kubelet-restart vm0 | create xnuma-c@vm1#0 | " +
			"create sdax-a@vm2#1 | settle | delete xnuma-c@vm1#0 | create xdax-c@vm2#0 | " +
			"kubelet-restart vm0 | settle | create sdax-b@vm0#0 | kubelet-restart vm0 | create " +
			"xnuma-a@vm1#0 | settle | create xnuma-b@vm0#0 | create sdax-c@vm0#0 | ctrl-kill a | settle " +
			"| server-restart c | create sro-a@vm1#0 | delete xnuma-a@vm1#0 | settle | delete " +
			"sdax-c@vm0#0 | kubelet-restart vm2 | delete xnuma-b@vm0#0 | settle | ctrl-start c | create " +
			"sro-a@vm2#0 | delete sro-a@vm1#0 | settle | create sexcl-a@vm2#0 | ctrl-restart c | delete " +
			"xdax-c@vm2#0 | settle | ctrl-start b | ctrl-restart c | create sro-c@vm0#0 | settle | " +
			"delete sro-a@vm2#0 | delete sdax-a@vm2#1 | create sexcl-c@vm1#0 | settle"},
	{why: "F9: promotion after an unbound delete (sexcl after shares)", profile: "full", seed: 10164,
		steps: "create plain@vm0#0 | create xdax-a@vm2#0 | delete xdax-a@vm2#0 | settle | create " +
			"sdax-a@vm2#0 | create xdax-b@vm0#0 | create sro-b@vm0#0 | settle | create xnuma-b@vm0#0 | " +
			"create sdax-c@vm2#0 | settle | server-restart c | ctrl-kill c | ctrl-kill b | settle | " +
			"delete sdax-a@vm2#0 | server-restart b | settle | delete xdax-b@vm0#0 | server-restart b | " +
			"server-restart b | settle | create sro-a@vm2#0 | delete sro-b@vm0#0 | create ldax@vm2#0 | " +
			"settle | ctrl-kill a | create lsteer@vm2#0 | create sexcl-c@vm1#0 | settle | create " +
			"sdax-a@vm2#0 | server-restart c | delete sdax-a@vm2#0 | settle | delete sexcl-c@vm1#0 | " +
			"create sdax-b@vm0#0 | server-restart a | settle | kubelet-restart vm0 | ctrl-start a | " +
			"delete sdax-b@vm0#0 | settle | create sdax-c@vm0#0 | settle | delete sdax-c@vm0#0 | create " +
			"sexcl-c@vm1#0 | delete sdax-c@vm2#0 | settle"},
	{why: "F9: a later fit of a device held since the last settle", profile: "full", seed: 5870,
		steps: "create sdax-a@vm2#1 | create xnuma-b@vm0#0 | create ldax@vm1#0 | settle | ctrl-kill b | " +
			"create lease@vm2#0 | create xdax-b@vm0#0 | settle | ctrl-restart a | delete xdax-b@vm0#0 | " +
			"create xnuma-a@vm1#0 | settle | kubelet-restart vm2 | server-restart c | ctrl-kill a | " +
			"settle | delete xnuma-a@vm1#0 | create sdax-c@any#0 | create sdax-a@vm2#0 | settle | " +
			"ctrl-start b | delete xnuma-b@vm0#0 | ctrl-kill c | settle | create sro-c@vm0#0 | delete " +
			"ldax@vm1#0 | delete sro-c@vm0#0 | settle | delete sdax-c@any#0 | ctrl-start a | create " +
			"sexcl-c@vm1#0 | settle"},
	{why: "F10: a contender stalled with other region events on its stack (refused)", profile: "full", seed: 35043, refused: 41,
		steps: "create sexcl-c@vm1#0 | create sdax-a@vm2#0 | delete sexcl-c@vm1#0 | settle | ctrl-kill b | " +
			"create lease@vm0#0 | ctrl-restart a | settle | kubelet-restart vm2 | create lnuma@vm0#1 | " +
			"driver-kill vm2 | settle | create xnuma-a@vm1#0 | create xdax-a@vm2#0 | create " +
			"sexcl-b@vm0#0 | settle | crash xnuma-a@vm1#0 | create sdax-c@any#0 | delete sdax-a@vm2#0 | " +
			"settle | delete xnuma-a@vm1#0 | ctrl-restart c | delete lnuma@vm0#1 | settle | create " +
			"sro-a@vm2#0 | create sdax-a@vm2#0 | settle | create xdax-a@vm1#0 | kubelet-restart vm1 | " +
			"delete sdax-c@any#0 | settle | ctrl-start b | create sro-b@vm0#0 | settle | create " +
			"xdax-c@vm2#0 | delete xdax-c@vm2#0 | create ldax@vm1#0 | settle | create sro-c@vm2#0 | " +
			"driver-restart vm1 | create xdax-c@vm2#0 | settle"},
	{why: "F11: a stalled \"any\" pod and a prepare on a host stack (refused)", profile: "full", seed: 90818, refused: 32,
		steps: "create sdax-a@vm2#1 | ctrl-restart c | ctrl-restart b | settle | ctrl-kill b | create " +
			"xnuma-a@vm1#0 | settle | create lnuma@vm1#0 | create plain@vm0#0 | delete xnuma-a@vm1#0 | " +
			"settle | ctrl-start b | ctrl-kill c | create sdax-c@vm1#0 | settle | delete sdax-c@vm1#0 | " +
			"create sdax-c@vm0#0 | create xdax-c@vm2#0 | settle | create lnuma@vm0#1 | create " +
			"xdax-c@any#0 | delete sdax-a@vm2#1 | settle | delete sdax-c@vm0#0 | delete xdax-c@vm2#0 | " +
			"create xdax-a@vm2#0 | settle | server-restart c | ctrl-start c | create xnuma-a@vm2#0 | " +
			"settle | create sdax-c@vm0#0 | driver-kill vm1 | create lease@vm1#0 | settle"},
	{why: "ctrl-start after the slices were absent: published again (A20)", profile: "full", seed: 860,
		steps: "create xnuma-a@vm1#0 | ctrl-restart c | ctrl-kill c | settle | ctrl-start c | create " +
			"sdax-c@vm0#0 | server-restart c | settle"},
	{why: "reference: an attach that conflicts with a wanted attachment on another host (AttachFailed, scheduled again)", profile: "full", seed: 9745,
		steps: "create sdax-a@vm1#0 | ctrl-kill c | create lnuma@vm2#0 | settle | delete sdax-a@vm1#0 | " +
			"create sexcl-b@vm0#0 | kubelet-restart vm1 | settle | create lsteer@vm1#0 | ctrl-kill b | " +
			"create sexcl-a@vm2#0 | settle | delete sexcl-b@vm0#0 | create plain@vm0#0 | create " +
			"xnuma-a@vm2#0 | settle | create sro-a@vm1#0 | ctrl-start c | containerd-restart vm2 | " +
			"settle | create xnuma-c@vm1#0 | settle | create sdax-c@vm1#0 | create lsteer@vm2#0 | " +
			"ctrl-restart a | settle | ctrl-kill a | create ldax@vm0#0 | ctrl-kill c | settle | delete " +
			"xnuma-a@vm2#0 | ctrl-start c | delete sexcl-a@vm2#0 | settle | ctrl-start a | ctrl-kill c " +
			"| create sro-a@vm2#0"},
	{why: "F13: an unblocking pool pod not bound yet (Attached, PreBind not polled again) taken as bound", profile: "full", seed: 700750,
		steps: "kubelet-restart vm2 | create sexcl-b@vm0#0 | server-restart a | settle | create " +
			"sro-a@vm2#0 | ctrl-kill a | create sdax-b@vm0#1 | settle | ctrl-restart c | ctrl-start a | " +
			"create xnuma-a@vm2#0 | settle | delete sexcl-b@vm0#0 | delete sro-a@vm2#0 | delete " +
			"xnuma-a@vm2#0 | settle | kubelet-restart vm2 | ctrl-kill c | ctrl-restart b | settle | " +
			"server-restart c | ctrl-restart a | create sro-b@vm0#0 | settle | create xdax-c@vm2#0 | " +
			"create sdax-a@vm2#0 | create sexcl-c@vm1#0 | settle | delete sro-b@vm0#0 | create " +
			"xnuma-a@vm1#0 | create sro-c@vm2#0 | settle | delete xdax-c@vm2#0 | create sro-a@vm2#0 | " +
			"delete sdax-b@vm0#1 | settle | delete sro-a@vm2#0 | delete sro-c@vm2#0 | create " +
			"xdax-b@vm0#0 | settle | ctrl-kill a | ctrl-kill b | ctrl-start c | settle | create " +
			"xdax-a@vm1#0 | delete sexcl-c@vm1#0 | create sexcl-b@vm0#0 | settle | create lnuma@vm1#0 | " +
			"create ldax@vm1#1 | kubelet-restart vm0 | settle | delete sexcl-b@vm0#0 | create " +
			"lnuma@vm0#0 | driver-restart vm1 | settle | ctrl-restart c | create xnuma-c@vm1#0 | create " +
			"sro-b@vm0#0 | settle | delete xnuma-c@vm1#0 | delete xnuma-a@vm1#0 | server-restart a | " +
			"settle | ctrl-start a | ctrl-start b | create sexcl-b@vm0#0 | settle | ctrl-restart b | " +
			"ctrl-restart a | ctrl-restart c | settle | create xnuma-a@vm1#0 | ctrl-kill c | delete " +
			"xdax-a@vm1#0 | settle | create sdax-c@vm2#0 | create xdax-a@vm2#0 | ctrl-start c | settle " +
			"| ctrl-restart b | create ldax@vm2#0 | delete sdax-a@vm2#0 | settle | create plain@vm2#0 | " +
			"kubelet-restart vm1 | containerd-restart vm0 | settle | ctrl-kill c | delete xdax-b@vm0#0 " +
			"| create sro-c@vm2#0 | settle | delete xdax-a@vm2#0 | ctrl-kill b | ctrl-start c | settle " +
			"| ctrl-start b | create xdax-c@vm2#0 | delete sro-c@vm2#0 | settle | create sro-a@vm2#0 | " +
			"delete ldax@vm2#0 | ctrl-kill c | settle | create sexcl-a@vm2#0 | settle | delete " +
			"sro-a@vm2#0 | create plain@vm1#0 | create lnuma@vm2#0 | settle"},
	{why: "a stalled writer promoted through a hole made a namespace in the truth (c-pmem1)", profile: "fullp", seed: 2264,
		steps: "create lnuma@vm2#0 | create sdax-c@vm1#0 | create xpf-c@vm0#0 | pwipe c c-pmem0 | " +
			"pwipe c c-pmem0 | pwipe c c-pmem0 | settle | ctrl-restart a | create xdax-a@vm2#0 | " +
			"ctrl-kill b | settle | create sro-a@vm1#0 | ctrl-kill a | ctrl-kill c | settle | " +
			"ctrl-start a | create pfsc-c@vm2#0 | create pwr-c@vm2#0 | settle | delete " +
			"sdax-c@vm1#0 | delete pfsc-c@vm2#0 | create sdax-b@vm0#0 | settle | create " +
			"sro-b@vm0#0 | create sdax-a@vm2#1 | delete sdax-a@vm2#1 | settle | server-restart c | " +
			"delete xpf-c@vm0#0 | driver-kill-at vm1 mount | settle | create sro-a@vm2#0 | create " +
			"sdax-b@vm0#1 | delete xdax-a@vm2#0 | settle | delete lnuma@vm2#0 | delete " +
			"sdax-b@vm0#1 | create pfsd-c@vm2#1 | settle | create xdax-a@vm1#0 | create " +
			"xnuma-a@vm1#0 | create xnuma-c@vm1#0 | settle | crash xdax-a@vm1#0 | ctrl-restart a | " +
			"settle | create xpf-c@vm0#0 | delete pfsd-c@vm2#1 | create pbad-c@vm0#0 | settle | " +
			"driver-kill-at vm2 region | ctrl-start b | server-restart c | settle | delete " +
			"xpf-c@vm0#0 | delete sdax-b@vm0#0 | driver-kill-at vm0 bind | settle | create " +
			"ldax@vm1#0 | create xdax-c@vm2#0 | create sdax-a@vm1#0 | settle | create " +
			"sexcl-b@vm0#0 | delete sro-a@vm1#0 | ctrl-kill a | settle | delete sro-b@vm0#0 | " +
			"delete pwr-c@vm2#0 | ctrl-start c | settle | pwipe c c-pmem1 | delete sexcl-b@vm0#0 | " +
			"create pwr-c@any#0 | create lsteer@vm2#0 | settle | ctrl-restart c | delete " +
			"sro-a@vm2#0 | settle | create sdax-c@vm1#0 | delete xnuma-a@vm1#0 | delete " +
			"xnuma-c@vm1#0 | settle | create xpb-c@vm1#0 | driver-kill-at vm1 region | create " +
			"xnuma-a@vm1#0 | settle"},
	{why: "a stalled writer promoted through a hole made a namespace in the truth (c-pmem0)", profile: "fullp", seed: 1452,
		steps: "ctrl-kill b | ctrl-kill c | server-restart c | settle | ctrl-restart a | create " +
			"sdax-c@vm2#0 | ctrl-start c | settle | create xdax-a@vm2#0 | crash sdax-c@vm2#0 | " +
			"create pfsd-c@any#0 | settle | create xnuma-b@vm0#0 | delete pfsd-c@any#0 | create " +
			"pwr-c@vm0#0 | settle | create xdax-c@any#0 | settle | create sro-c@vm0#0 | ctrl-kill " +
			"c | ctrl-restart a | settle | create xpb-c@vm1#0 | create sdax-a@vm1#0 | create " +
			"xnuma-a@vm2#0 | settle | delete xnuma-b@vm0#0 | settle | create sro-a@vm1#0 | pwrite " +
			"pwr-c@vm0#0 img | create sdax-c@vm0#0 | settle | delete xdax-a@vm2#0 | pwrite " +
			"pwr-c@vm0#0 img | create sro-c@vm2#0 | create pbro-c@vm1#0 | settle | ctrl-kill a | " +
			"delete xnuma-a@vm2#0 | delete sdax-c@vm2#0 | pwrite pwr-c@vm0#0 junk | settle | " +
			"create sexcl-a@vm2#0 | create lease@vm2#0 | create lsteer@vm1#0 | settle | " +
			"server-restart a | pwrite pwr-c@vm0#0 img | create sdax-c@vm2#0 | server-restart c | " +
			"pwrite pwr-c@vm0#0 img | pwrite pwr-c@vm0#0 junk | pwrite pwr-c@vm0#0 img | pwrite " +
			"pwr-c@vm0#0 img | pwrite pwr-c@vm0#0 junk | pwrite pwr-c@vm0#0 img | settle | delete " +
			"xpb-c@vm1#0 | delete pwr-c@vm0#0 | create xnuma-c@vm1#0 | settle | delete " +
			"sexcl-a@vm2#0 | create sro-b@vm0#0 | create ldax@vm1#1 | settle | delete sro-c@vm2#0 " +
			"| create sro-a@vm2#0 | delete pbro-c@vm1#0 | settle | driver-kill-at vm0 mount | " +
			"ctrl-start c | create xnuma-a@vm2#0 | settle | create sdax-b@vm0#0 | create " +
			"sexcl-a@vm2#0 | create sexcl-b@vm0#0 | settle | pwipe c c-pmem0 | create " +
			"sexcl-c@vm1#0 | create lnuma@vm0#1 | driver-kill-at vm2 bind | settle | create " +
			"xnuma-a@vm1#0 | delete sdax-a@vm1#0 | create pwr-c@vm2#0 | settle"},
	{why: "three \"any\" pods prepared at a ctrl-start: truncated, narrowed by the observed nodes", profile: "fullp", seed: 1484,
		steps: "create ldax@vm2#0 | create xdax-c@vm2#0 | ctrl-kill c | settle | create pwr-c@any#0 | " +
			"ctrl-kill b | create pwr-c@vm0#0 | settle | create ldax@vm0#0 | create sro-b@vm0#0 | " +
			"delete sro-b@vm0#0 | settle | driver-kill vm2 | create xnuma-b@vm0#0 | delete " +
			"xdax-c@vm2#0 | settle | create sdax-b@vm0#1 | create sdax-b@vm0#0 | create " +
			"sro-a@vm1#0 | settle | create lsteer@vm2#0 | create xdax-c@any#0 | settle | " +
			"driver-restart vm1 | delete sdax-b@vm0#0 | ctrl-kill a | settle | delete sro-a@vm1#0 " +
			"| create sdax-c@vm2#0 | create sdax-c@any#0 | settle | create xdax-b@vm0#0 | delete " +
			"sdax-b@vm0#1 | create plain@vm0#0 | settle | delete xnuma-b@vm0#0 | ctrl-start c | " +
			"create sdax-c@vm0#0 | settle"},
	{why: "lease-get, lease-put, ctrl-kill: the companion's detach may not have come", profile: "pmem", seed: 30671,
		steps: "create xnuma-c@vm1#0 | driver-kill-at vm2 bind | create pwr-c@vm0#0 | settle | " +
			"driver-kill-at vm1 ns | settle | create pfsd-c@vm1#0 | delete pwr-c@vm0#0 | settle | " +
			"driver-kill vm2 | settle | delete xnuma-c@vm1#0 | create pfsc-c@vm2#0 | " +
			"kubelet-restart vm2 | settle | ctrl-restart c | settle | create pfsd-c@vm2#0 | delete " +
			"pfsd-c@vm1#0 | driver-kill-at vm0 bind | settle | create pfsd-c@vm1#0 | settle | " +
			"driver-kill vm2 | create sdax-c@vm0#0 | delete pfsd-c@vm2#0 | settle | create " +
			"pfsd-c@vm2#0 | settle | ctrl-kill c | ctrl-start c | server-restart c | settle | " +
			"delete pfsc-c@vm2#0 | kubelet-restart vm2 | delete pfsd-c@vm1#0 | settle | create " +
			"pfsc-c@vm1#0 | settle | delete sdax-c@vm0#0 | ctrl-kill c | settle | delete " +
			"pfsd-c@vm2#0 | driver-restart vm2 | server-restart c | settle | delete pfsc-c@vm1#0 | " +
			"create pfsc-c@vm2#0 | server-restart c | settle | create pfsd-c@vm2#0 | settle | " +
			"driver-kill-at vm2 mount | create plse@vm2#0 | ctrl-start c | settle | driver-kill " +
			"vm1 | driver-kill vm2 | ctrl-kill c | settle | ctrl-start c | create pwr-c@vm2#0 | " +
			"server-restart c | settle | ctrl-restart c | kubelet-restart vm1 | create " +
			"sdax-c@vm0#0 | settle | driver-kill-at vm0 ns | containerd-restart vm2 | settle | " +
			"delete pfsc-c@vm2#0 | delete pfsd-c@vm2#0 | kubelet-restart vm0 | settle | create " +
			"pfsd-c@vm2#0 | delete pwr-c@vm2#0 | delete pfsd-c@vm2#0 | settle | server-restart c | " +
			"server-restart c | ctrl-restart c | settle | lease-get plse@vm2#0 pbw | lease-put " +
			"plse@vm2#0 | ctrl-kill c | settle"},
	{why: "ctrl-start: the waiter of an unbound claim deleted while the slices were gone stalls", profile: "pmem", seed: 4588,
		steps: "ctrl-restart c | kubelet-restart vm1 | kubelet-restart vm2 | settle | driver-kill-at " +
			"vm0 mount | ctrl-kill c | create pfsc-c@vm2#0 | settle | delete pfsc-c@vm2#0 | " +
			"driver-kill-at vm2 region | driver-kill-at vm1 region | settle | containerd-restart " +
			"vm1 | settle | create pwr-c@vm2#0 | kubelet-restart vm1 | driver-restart vm1 | settle " +
			"| create pfsc-c@vm1#0 | delete pfsc-c@vm1#0 | create pfsd-c@vm2#0 | settle | delete " +
			"pfsd-c@vm2#0 | settle | create pbro-c@vm0#0 | settle | driver-kill-at vm1 bind | " +
			"create sdax-c@vm0#0 | delete sdax-c@vm0#0 | settle | delete pwr-c@vm2#0 | ctrl-start " +
			"c | settle | create xnuma-c@vm1#0 | driver-restart vm1 | delete pbro-c@vm0#0 | settle " +
			"| create pbro-c@vm0#0 | ctrl-restart c | ctrl-kill c | settle | create pwr-c@vm0#0 | " +
			"delete pbro-c@vm0#0 | ctrl-start c | settle"},
	{why: "a refused \"any\" pod on the reference's node in the truth (its probe and a watcher)", profile: "fullp", seed: 50748,
		steps: "create sdax-c@any#0 | create pfsc-c@vm1#0 | ctrl-kill b | settle | kubelet-restart " +
			"vm1 | create xdax-c@any#0 | driver-kill-at vm0 region | settle | create pbro-c@vm1#0 " +
			"| create pfsd-c@any#0 | create pbad-c@vm0#0 | settle | create sdax-a@vm2#0 | delete " +
			"sdax-a@vm2#0 | create xnuma-c@vm1#0 | settle"},
	{why: "the truth's hold ends as the model's (a stalled \"any\" pod fired a watcher late)", profile: "fullp", window: 30 * time.Second, seed: 72622,
		steps: "ctrl-kill b | create lsteer@vm2#0 | create pfsd-c@any#0 | settle | create " +
			"pfsc-c@vm1#0 | server-restart c | driver-kill-at vm0 region | settle | ctrl-restart c " +
			"| settle | create lease@vm1#0 | create pfsd-c@vm2#0 | create sdax-a@vm2#0 | settle | " +
			"create xpb-c@vm1#0 | delete sdax-a@vm2#0 | driver-kill-at vm2 ns | settle | create " +
			"pwr-c@vm2#0 | delete pwr-c@vm2#0 | create sro-a@vm1#0 | settle | create lnuma@vm0#0 | " +
			"create sro-a@vm2#0 | delete pfsd-c@any#0 | settle | driver-kill-at vm1 mount | " +
			"ctrl-kill c | create sro-c@vm2#0 | settle | create sdax-b@vm0#0 | server-restart a | " +
			"create ldax@vm0#0 | settle | create pfsd-c@vm1#0 | ctrl-restart a | create " +
			"pbro-c@vm1#0 | settle | pwrite xpb-c@vm1#0 img | delete xpb-c@vm1#0 | delete " +
			"pfsd-c@vm1#0 | delete sdax-b@vm0#0 | settle | driver-restart vm2 | create " +
			"sdax-a@vm2#0 | create plain@vm0#0 | settle | create pbad-c@vm0#0 | ctrl-kill a | " +
			"create sexcl-c@vm1#0 | settle | create sdax-b@vm0#1 | kubelet-restart vm2 | " +
			"ctrl-start b | settle | ctrl-kill b | create xpb-c@vm1#0 | ctrl-start c | settle | " +
			"create xnuma-b@vm0#0 | create pwr-c@any#0 | settle | create xdax-c@any#0 | crash " +
			"sro-a@vm1#0 | create ldax@vm1#1 | settle | crash lsteer@vm2#0 | create sdax-b@vm0#0 | " +
			"delete sro-a@vm2#0 | pwrite xpb-c@vm1#0 img | settle | delete xpb-c@vm1#0 | create " +
			"plse@vm0#0 | create lease@vm2#0 | settle | create sexcl-b@vm0#0 | delete " +
			"xnuma-b@vm0#0 | delete xdax-c@any#0 | settle | delete sro-a@vm1#0 | delete " +
			"pbro-c@vm1#0 | delete sdax-b@vm0#1 | settle | create xnuma-b@vm0#0 | kubelet-restart " +
			"vm2 | driver-kill-at vm0 region | settle | create xnuma-c@vm1#0 | delete sdax-a@vm2#0 " +
			"| delete xnuma-c@vm1#0 | settle | delete pfsc-c@vm1#0 | delete pwr-c@any#0 | create " +
			"pfsd-c@any#0 | pwipe c c-pmem1 | settle"},
	// a dnuma pod waits for the block of vm0's local memdev that a
	// dax lease holds; the boot releases the lease, and the
	// reference's requeue after it (inside the step) must see the
	// block free: the simulated SUT answered from the truth before
	// the step, the dnuma pod was never allocated (sim.next)
	{why: "full qemu seed 270849: dnuma after the boot", profile: "full", reboot: RebootQemu, seed: 270849,
		steps: "ctrl-restart b | create xdax-b@vm0#0 | create sdax-c@vm2#0 | settle | create lease@vm0#0 | " +
			"create sro-c@vm2#0 | create sdax-b@vm0#1 | settle | create sdax-c@vm0#0 | create sdax-a@vm2#0 | create sdax-a@vm1#0 | " +
			"settle | create sro-b@vm0#0 | create ldax@vm0#0 | kubelet-restart vm2 | settle | lease-get lease@vm0#0 dax | " +
			"create dnuma@vm0#0 | create plain@vm0#0 | settle | ctrl-restart c | ctrl-restart a | ctrl-restart b | settle | " +
			"driver-restart vm0 | delete sdax-a@vm1#0 | create xnuma-a@vm2#0 | settle | node-reboot vm0 | settle"},
	// an "any" pod bound on vm2 while the detach of its device from
	// vm0 (a deleted claim's) was not done: the truth's node side
	// took vm0 (a tie of the node and the attachment), its env named
	// a dax device of vm0 (refFactWeight)
	{why: "full guest seed 652722: the node of an any pod", profile: "full", reboot: RebootGuest, fencing: FencingNone, seed: 652722,
		steps: "ctrl-restart a | create xnuma-a@vm1#0 | create xdax-c@vm2#0 | settle | create xnuma-b@vm0#0 | create xdax-a@vm2#0 | " +
			"ctrl-restart c | settle | node-reboot vm0 | settle | server-restart c | server-restart c | driver-kill vm1 | settle | " +
			"node-reboot vm1 | settle | create xdax-c@any#0 | server-restart a | delete xnuma-a@vm1#0 | settle | server-restart a | " +
			"settle | ctrl-restart c | create sdax-c@vm2#0 | create xnuma-a@vm1#0 | settle | create sro-c@vm2#0 | create sdax-c@vm0#0 | " +
			"create lsteer@vm1#0 | settle | delete xdax-c@any#0 | create xdax-b@vm0#0 | delete sro-c@vm2#0 | settle | " +
			"delete sdax-c@vm0#0 | delete xnuma-a@vm1#0 | delete xnuma-b@vm0#0 | settle | create sdax-c@vm0#0 | create sro-b@vm0#0 | " +
			"delete sro-b@vm0#0 | settle | delete xdax-b@vm0#0 | delete sdax-c@vm2#0 | ctrl-kill a | settle | create sro-c@vm2#0 | " +
			"create sdax-b@vm0#1 | create ldax@vm1#0 | settle | ctrl-start a | create sexcl-b@vm0#0 | delete xdax-a@vm2#0 | settle | " +
			"create sdax-c@any#0 | delete sdax-c@vm0#0 | server-restart c | settle"},
	// lease-get plse@vm0#0 pfd cut by the bind watcher of vm0 (A29
	// d): the reference released the lease, the truth kept it (the
	// detach of c-pmem0@vm0 not done at the settle's first poll
	// tied with the lease); "attachment c-pmem0@vm0: missing" at
	// the next settle (refFactWeight). Fixed, the lease is released
	// at that settle: the fuzz's hold of it is not enabled.
	{why: "fullp qemu switch 30s seed 1380903: a lease cut at its bind", profile: "fullp", reboot: RebootQemu, fencing: FencingSwitch, window: 30 * time.Second, seed: 1380903, refused: 105,
		steps: "create pwr-c@any#0 | server-restart c | create sdax-b@vm0#1 | settle | server-restart a | " +
			"ctrl-kill c | driver-kill-at vm2 mount | pwrite pwr-c@any#0 junk | pwrite pwr-c@any#0 img | " +
			"pwrite pwr-c@any#0 img | settle | create sexcl-c@vm1#0 | settle | delete sdax-b@vm0#1 | " +
			"pwrite pwr-c@any#0 junk | create sdax-c@any#0 | create pwr-c@vm2#0 | settle | create sexcl-b@vm0#0 | " +
			"create xnuma-b@vm0#0 | delete sexcl-c@vm1#0 | pwrite pwr-c@any#0 img | settle | create plse@vm2#0 | " +
			"driver-kill-at vm1 mount | driver-kill-at vm0 bind | settle | ctrl-kill b | create sdax-a@vm1#0 | " +
			"create plse@vm0#0 | pwrite pwr-c@any#0 junk | pwrite pwr-c@any#0 img | settle | delete sexcl-b@vm0#0 | " +
			"settle | create xdax-b@vm0#0 | delete sdax-c@any#0 | create sexcl-b@vm0#0 | pwrite pwr-c@any#0 img | " +
			"settle | create xpb-c@vm1#0 | ctrl-kill a | create xdax-c@vm2#0 | pwrite pwr-c@any#0 img | settle | " +
			"delete xnuma-b@vm0#0 | create lnuma@vm0#0 | delete pwr-c@any#0 | settle | ctrl-start b | " +
			"delete xpb-c@vm1#0 | create pbro-c@vm1#0 | settle | delete sdax-a@vm1#0 | create xpf-c@vm0#0 | " +
			"create xnuma-c@vm2#0 | settle | create sdax-a@vm1#0 | delete xdax-c@vm2#0 | delete xdax-b@vm0#0 | " +
			"settle | create sdax-a@vm2#0 | delete sdax-a@vm1#0 | create sro-b@vm0#0 | settle | " +
			"create xnuma-c@vm0#0 | delete xnuma-c@vm0#0 | create xdax-c@any#0 | settle | create sro-a@vm1#0 | " +
			"delete pwr-c@vm2#0 | create sdax-c@vm0#0 | settle | create sro-a@vm2#0 | create pbad-c@vm0#0 | " +
			"delete xpf-c@vm0#0 | settle | delete sro-a@vm2#0 | ctrl-start a | create pfsc-c@vm2#0 | settle | " +
			"create xpb-c@vm1#0 | delete pfsc-c@vm2#0 | delete pbad-c@vm0#0 | settle | ctrl-kill a | " +
			"delete sdax-a@vm2#0 | create sexcl-c@vm1#0 | settle | create sexcl-a@vm2#0 | create pfsc-c@vm2#0 | " +
			"fencing-switch | settle | kubelet-restart vm2 | ctrl-start c | ctrl-kill b | settle | " +
			"delete pfsc-c@vm2#0 | driver-kill-at vm0 bind | lease-get plse@vm0#0 pfd | settle | " +
			"delete xpb-c@vm1#0 | delete sro-a@vm1#0 | driver-kill-at vm2 region | hold plse@vm0#0 | " +
			"pwrite pbro-c@vm1#0 img | settle"},
	// the simulator's truth makes no namespace that the reference never let a
	// writer prepare: pwr-c@vm2#0 created while pool c has no slices, deleted
	// unallocated, in a window with ctrl-start c (seed 9762 alike)
	{why: "no namespace of a writer the reference never let prepare", profile: "pmem", seed: 9442,
		steps: "driver-kill-at vm0 ns | kubelet-restart vm2 | server-restart c | settle | driver-kill vm0 | create pbro-c@vm0#0 | delete pbro-c@vm0#0 | settle | ctrl-restart c | ctrl-kill c | create pwr-c@vm2#0 | settle | driver-kill-at vm0 ns | driver-kill-at vm2 ns | kubelet-restart vm0 | settle | delete pwr-c@vm2#0 | create pfsd-c@vm2#0 | ctrl-start c | settle"},
}

// refSequenceSteps returns the steps of the row of refSequences of a profile and seed.
func refSequenceSteps(t *testing.T, profile string, seed int64) []string {
	t.Helper()
	for _, r := range refSequences {
		if r.profile == profile && r.seed == seed {
			return strings.Split(r.steps, " | ")
		}
	}
	t.Fatalf("no sequence of %s seed %d", profile, seed)
	return nil
}

// TestRefPoolSequences runs the rows of refSequences against the reference.
func TestRefPoolSequences(t *testing.T) {
	for _, r := range refSequences {
		cfg := testConfig(t, r.profile, true)
		cfg.Unsettled = 3
		cfg.Reboot, cfg.Fencing = r.reboot, r.fencing
		if r.window > 0 {
			cfg.StallWindow, cfg.StallStrict = r.window, r.window <= 180*time.Second
		}
		lines := strings.Split(r.steps, " | ")
		if r.refused > 0 {
			f := refSequence(cfg, allActs(cfg), r.seed, 0, lines, &fuzzStats{verbs: map[Verb]int{}, ref: map[string]int{}})
			if f == nil || f.msg != "not enabled: "+lines[r.refused-1] || len(f.steps) != r.refused {
				t.Errorf("%s: want step %d %q refused, got %v", r.why, r.refused, lines[r.refused-1], f)
				continue
			}
			lines = lines[:r.refused-1]
		}
		if f := refSequence(cfg, allActs(cfg), r.seed, 0, lines, &fuzzStats{verbs: map[Verb]int{}, ref: map[string]int{}}); f != nil {
			t.Errorf("%s:\n%s", r.why, f)
		}
	}
}

// d8Steps are the actions of a claim that waits for the attach while the
// controller is down, its pod deleted and another pod pending on the
// same device: first the controller stays down for the settle, then it
// starts right after the delete (without the initial settle).
func d8Steps() []string {
	return []string{
		"ctrl-kill a", "settle", "create xnuma-a@vm1#0", "settle",
		"create xdax-a@vm1#0", "settle", "delete xnuma-a@vm1#0", "settle",
		"ctrl-start a", "settle", "delete xdax-a@vm1#0", "settle",
		"ctrl-kill a", "settle", "create xnuma-a@vm1#0", "settle",
		"create xdax-a@vm1#0", "settle", "delete xnuma-a@vm1#0", "ctrl-start a",
		"settle", "delete xdax-a@vm1#0", "settle",
	}
}

// --stall-window against the reference on the D8 sequence: the default
// (6m: stalled contenders settled in both states) with the scheduler of
// a cluster that does not requeue at once, a short window (30 s: settles wait
// for the promotion) with the fixed one; the short window with the old
// scheduler fails (the settle that waits for the promotion times out).
func TestRefPoolStallWindow(t *testing.T) {
	steps := d8Steps()
	for seed := int64(1); seed <= 20; seed++ {
		if f := refScript(t, "full", seed, 0, steps...); f != nil {
			t.Fatalf("default window:\n%s", f)
		}
		if f := refScript(t, "full", seed, 30*time.Second, steps...); f != nil {
			t.Fatalf("30 s, fixed scheduler:\n%s", f)
		}
	}
	refUnfixedScheduler = true
	defer func() { refUnfixedScheduler = false }()
	failed := 0
	for seed := int64(1); seed <= 20; seed++ {
		if f := refScript(t, "full", seed, 30*time.Second, steps...); f != nil {
			if !strings.Contains(f.msg, "still not allocated") {
				t.Fatalf("30 s, old scheduler: %s", f.msg)
			}
			failed++
		}
	}
	if failed == 0 {
		t.Fatalf("30 s with the old scheduler never failed")
	}
}

// A stalled contender (the scheduler holds the device of a claim deleted
// before it was bound) seen not allocated and unschedulable when its
// pool's slices are gone (ctrl-restart, then ctrl-kill) is Unsched, like
// the claims that wait for the slices: a short --stall-window does not
// fail it (fuzz, fullp seed 20031, pmem seeds 9442, 9445, 9472 with
// --stall-window 30s); allocated before the slices went it waits for
// the attach (seed 328).
func TestRefPoolStalledWithoutSlices(t *testing.T) {
	script := []string{"create sdax-c@any#0", "create pfsc-c@vm2#0", "pwipe c c-pmem1", "delete pfsc-c@vm2#0", "settle",
		"ctrl-restart c", "create pfsc-c@vm1#0", "ctrl-kill c", "settle"}
	if f := refScript(t, "fullp", 20031, 30*time.Second, script...); f != nil {
		t.Fatalf("%s", f)
	}
	s := steps(t, stateAfter(t, "fullp", true, script[:len(script)-1]...), "settle")
	wantPhase(t, s, "pfsc-c@vm1#0", Stalled)
	var absent *State
	for _, a := range alts(s) {
		if a.Ctrl[PoolC].ResourceSlices == SlicesAbsent {
			absent = a
		}
	}
	if absent == nil {
		t.Fatalf("no world without the slices:\n%s", dumpAlts(s))
	}
	truth := absent.clone()
	truth.Slots[truth.cfg.slotIndex("pfsc-c@vm1#0")].Phase = Unsched
	v, adopted := checkState(s, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "pfsc-c@vm1#0", Unsched)
	if f := refScript(t, "full", 328, 0, "create xnuma-a@vm2#0", "create sdax-c@vm1#0", "delete xnuma-a@vm2#0", "settle",
		"create xdax-a@vm1#0", "ctrl-restart a", "ctrl-kill a", "settle"); f != nil {
		t.Fatalf("%s", f)
	}
}

// A18 (fuzz pmem seed 10091): an allocated claim keeps its allocation
// when its pool's slices go (the scheduler deallocates only on its own
// failures and timeouts). A stalled pfsc-c@vm1#0 promoted before
// ctrl-restart deleted the slices, then ctrl-kill: allocated, waiting
// for the attach (adopted); after ctrl-start c and ctrl-kill c, the
// world without the slices has it as with them (attached before the
// kill or not: either), never Unsched.
func TestRefPoolAllocatedWithoutSlices(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pfsc-c@vm2#0", "delete pfsc-c@vm2#0", "settle", "create pfsc-c@vm1#0")
	wantPhase(t, s, "pfsc-c@vm1#0", Stalled)
	s = steps(t, s, "ctrl-restart c", "ctrl-kill c", "settle")
	var absent *State
	for _, a := range alts(s) {
		if a.Ctrl[PoolC].ResourceSlices == SlicesAbsent {
			absent = a
		}
	}
	if absent == nil {
		t.Fatalf("no world without the slices:\n%s", dumpAlts(s))
	}
	truth := absent.clone()
	truth.Slots[truth.cfg.slotIndex("pfsc-c@vm1#0")].Phase = Attaching
	v, adopted := checkState(s, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "pfsc-c@vm1#0", Attaching)
	if adopted.Ctrl[PoolC].ResourceSlices != SlicesAbsent {
		t.Fatalf("adopted slices %v", adopted.Ctrl[PoolC].ResourceSlices)
	}
	r := steps(t, adopted, "ctrl-start c", "ctrl-kill c", "settle")
	found := false
	for _, a := range alts(r) {
		if a.Ctrl[PoolC].ResourceSlices != SlicesAbsent {
			continue
		}
		found = true
		if ph := slotOf(t, a, "pfsc-c@vm1#0").Phase; ph != Either {
			t.Fatalf("without the slices: %s, want either (attached before the kill or not)", ph)
		}
	}
	if !found {
		t.Fatalf("no world without the slices:\n%s", dumpAlts(r))
	}
	// the fuzz's sequence against the reference
	script := []string{"driver-kill-at vm2 ns", "create sdax-c@vm0#0", "settle",
		"ctrl-restart c", "ctrl-restart c", "create xnuma-c@vm1#0", "settle",
		"create pfsd-c@vm1#0", "kubelet-restart vm1", "settle",
		"delete pfsd-c@vm1#0", "delete sdax-c@vm0#0", "settle",
		"driver-kill-at vm1 mount", "create pfsd-c@vm1#0", "settle",
		"ctrl-kill c", "settle",
		"delete xnuma-c@vm1#0", "ctrl-start c", "delete pfsd-c@vm1#0", "settle",
		"create pfsc-c@vm2#0", "delete pfsc-c@vm2#0", "ctrl-restart c", "settle",
		"create pfsc-c@vm1#0", "ctrl-restart c", "ctrl-kill c", "settle",
		"ctrl-start c", "ctrl-kill c", "create pfsd-c@vm1#0", "settle"}
	for _, w := range []time.Duration{0, 30 * time.Second} {
		if f := refScript(t, "pmem", 10091, w, script...); f != nil {
			t.Fatalf("--stall-window %s:\n%s", w, f)
		}
	}
}

// The outcome bound, narrowed (fuzz, fullp --refpool-len 120 seed 1484): ctrl-start c
// prepares three "any" pods that waited for the attach: 27 worlds of
// their hosts, each with up to 6 orders of the regions on their stacks;
// the settle's first 64 outcomes (truncated) all had xdax-c@any#0 on vm0
// or vm1, and it was seen bound on vm2. The oracle settles it again with
// the nodes it observes (adopted with any=SLOT:VM,... before alt=J), the
// simulator's truth with those that the reference allocated.
func TestRefPoolNarrowedAnyNodes(t *testing.T) {
	script := []string{"ctrl-kill c", "create xdax-c@any#0", "create sdax-c@any#0", "settle",
		"create pwr-c@any#0", "settle", "ctrl-start c", "create sdax-c@vm0#0", "settle"}
	s := stateAfter(t, "fullp", true, script...)
	if !s.Truncated || s.Wide == nil {
		t.Fatalf("%d outcomes, truncated %v, wide %v: not the case of seed 1484", len(s.Alts)+1, s.Truncated, s.Wide != nil)
	}
	cfg := s.cfg
	x, sd, pw := cfg.slotIndex("xdax-c@any#0"), cfg.slotIndex("sdax-c@any#0"), cfg.slotIndex("pwr-c@any#0")
	n := s.narrowed(map[int]VM{x: VM2, sd: VM1, pw: VM1}, nil)
	if n == nil || n.Truncated || n.Narrow != "xdax-c@any#0:vm2,sdax-c@any#0:vm1,pwr-c@any#0:vm1" {
		t.Fatalf("narrowed %v", n)
	}
	enumerated := map[string]bool{}
	for _, a := range alts(s) {
		enumerated[a.altKey()] = true
	}
	var truth *State
	for _, a := range alts(n) {
		if !enumerated[a.altKey()] {
			truth = a
		}
	}
	if truth == nil {
		t.Fatalf("every narrowed outcome is enumerated: the settle is no case of the outcome bound any more")
	}
	o := observation(t, truth.clone())
	v, adopted, alt := checkAltsSince(s, o, "", time.Now(), time.Now(), defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	if adopted.Narrow != n.Narrow || adopted.Slots[x].Node != VM2 || adopted.Stacks != truth.Stacks {
		t.Fatalf("adopted (narrow %q):\n%s\nwant\n%s", adopted.Narrow, adopted.dump(), truth.dump())
	}
	rec := adoptionRecord(s, alt, adopted)
	if !strings.HasPrefix(rec, "any="+n.Narrow+" ") {
		t.Fatalf("adoption record %q", rec)
	}
	back, err := applyAdoption(s, rec)
	if err != nil || back.altKey() != adopted.altKey() {
		t.Fatalf("applyAdoption %q: %v\n%v", rec, err, back)
	}
	// not narrowed: "not all enumerated"
	w := s.clone()
	w.Alts, w.Truncated = s.Alts, true
	v, _, _ = checkAltsSince(w, o, "", time.Now(), time.Now(), defaultUnblock)
	wantVerdict(t, v, VFail, "not all enumerated")
	// against the reference: this window (the fuzz's sequence, seed 1484,
	// is a row of refSequences)
	for seed := int64(1); seed <= 10; seed++ {
		if f := refScript(t, "fullp", seed, 0, script...); f != nil {
			t.Fatalf("%s", f)
		}
	}
}

// F5 for persistent leases (fuzz, pmem --refpool-len 120 seed 30671): a
// lease of c-pmem0 got and put in one window (its companion attached
// synchronously, after the last settle), then ctrl-kill c: the detach of
// its released claim may not have come before the kill, so the
// attachment is maybe (adopted), not "no".
func TestRefPoolKillAfterLeasePut(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create plse@vm2#0", "settle", "lease-get plse@vm2#0 pbw", "lease-put plse@vm2#0", "ctrl-kill c")
	if a := s.Ctrl[PoolC].Att[devIndex(cPmem0)][VM2]; a != TriMaybe {
		t.Fatalf("c-pmem0@vm2 %v after the kill, want maybe", a)
	}
	// (and the new controller detaches what is left)
	for seed := int64(1); seed <= 20; seed++ {
		if f := refScript(t, "pmem", seed, 0, "create plse@vm2#0", "settle", "lease-get plse@vm2#0 pbw",
			"lease-put plse@vm2#0", "ctrl-kill c", "settle", "ctrl-start c", "settle"); f != nil {
			t.Fatalf("%s", f)
		}
	}
}

// A19 at ctrl-start (fuzz, pmem seed 4588): pwr-c@vm0#0 waits for
// c-pmem0 of an allocated claim that waits for the attach while pool c
// has no slices (ctrl-restart, ctrl-kill); that claim is deleted before
// its pod was bound, so its device stays in flight in the scheduler, and
// ctrl-start publishes the slices: the waiter's attempt (requeued by the
// publish, before the hold ended) fails, and nothing requeues it: it is
// stalled, not starting.
func TestRefPoolStallAtCtrlStart(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pfsc-c@vm2#0", "delete pfsc-c@vm2#0", "settle", "create pfsc-c@vm1#0")
	wantPhase(t, s, "pfsc-c@vm1#0", Stalled)
	s = steps(t, s, "ctrl-restart c", "ctrl-kill c", "settle")
	truth := adoptSlices(t, s, PoolC, SlicesAbsent).clone()
	truth.Slots[truth.cfg.slotIndex("pfsc-c@vm1#0")].Phase = Attaching
	v, adopted := checkState(s, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	adopted.Held = 0 // (the hold of the first delete is over)
	w := steps(t, adopted, "create pwr-c@vm0#0", "delete pfsc-c@vm1#0")
	wantPhase(t, w, "pwr-c@vm0#0", Unsched)
	wantPhase(t, steps(t, w, "ctrl-start c"), "pwr-c@vm0#0", Stalled)
	// (the reference runs the fuzz's sequence as a row of refSequences)
	refSequenceSteps(t, "pmem", 4588)
}

// A truncated settle (the outcome bound, A35) narrowed by the bindings of its race-kill pods
// (review fuzz, fullp fencing none, seed 710078): six pods whose Prepares
// were optional (a ctrl-kill c), a dnuma one with two of them, on four
// stacks; the 64 outcomes enumerated all had the dnuma pod's Prepare
// (Running), the reference had it allocated, not bound (its Attached not
// written before the kill). Narrowed by "unbound" it is Either (the
// oracle: Attaching), and the record's any= replays it.
func TestRefPoolNarrowedRaceKill(t *testing.T) {
	cfg := testConfig(t, "fullp", true)
	cfg.Unsettled = 3
	cfg.Fencing = FencingNone
	lines := strings.Split("create pbro-c@vm1#0 | create plain@vm0#0 | driver-kill-at vm1 mount | settle | "+
		"create sro-a@vm1#0 | create pfsc-c@vm1#0 | ctrl-kill b | settle | create sdax-a@vm2#1 | "+
		"create plse@vm1#0 | create xdax-a@vm2#0 | settle | driver-kill-at vm2 mount | "+
		"create xnuma-c@vm1#0 | create sexcl-c@vm1#0 | settle | driver-restart vm2 | "+
		"create sro-c@vm2#0 | ctrl-kill c | settle | ctrl-start b | create pwr-c@any#0 | "+
		"create xpf-c@vm0#0 | settle | server-restart c | delete sro-c@vm2#0 | ctrl-restart b | "+
		"settle | server-restart b | delete sro-a@vm1#0 | create sro-c@vm2#0 | settle | "+
		"delete pfsc-c@vm1#0 | ctrl-start c | delete xnuma-c@vm1#0 | settle | create xnuma-c@vm0#0 | "+
		"ctrl-kill a | delete pbro-c@vm1#0 | settle | delete sdax-a@vm2#1 | create ldax@vm2#0 | "+
		"server-restart b | settle | ctrl-restart c | pread pwr-c@any#0 | create sdax-a@vm1#0 | "+
		"create xdax-b@vm0#0 | pwrite pwr-c@any#0 img | pwrite pwr-c@any#0 img | "+
		"pwrite pwr-c@any#0 img | settle | delete pwr-c@any#0 | create plain@vm2#0 | "+
		"delete xnuma-c@vm0#0 | settle | ctrl-kill c | ctrl-kill b | create xdax-a@vm1#0 | settle | "+
		"delete sexcl-c@vm1#0 | kubelet-restart vm2 | delete xdax-b@vm0#0 | settle | ctrl-start b | "+
		"delete xdax-a@vm2#0 | server-restart c | settle | driver-kill-at vm2 bind | "+
		"create sdax-c@vm1#0 | delete sdax-c@vm1#0 | settle | create xdax-a@vm2#0 | "+
		"create dnuma@vm0#0 | create sro-b@vm0#0 | settle | create xnuma-c@vm2#0 | "+
		"create pfsc-c@vm2#0 | delete xdax-a@vm1#0 | settle | driver-kill-at vm0 ns | "+
		"create xdax-b@vm0#0 | settle | ctrl-restart b | create sdax-c@vm2#0 | create xdax-a@vm1#0 | "+
		"settle | create sdax-c@vm1#0 | create pfsc-c@vm0#0 | driver-kill-at vm1 region | settle | "+
		"delete ldax@vm2#0 | ctrl-start c | ctrl-kill c | settle", " | ")
	var last *State
	dbgPred = func(p *State, _ *sim) { last = p }
	defer func() { dbgPred = nil }()
	if f := refSequence(cfg, allActs(cfg), 710078, 0, lines, &fuzzStats{verbs: map[Verb]int{}, ref: map[string]int{}}); f != nil {
		t.Fatalf("%s", f)
	}
	d := cfg.slotIndex("dnuma@vm0#0")
	if last == nil || !last.Truncated || last.Wide == nil || !slices.Contains(last.Wide.optPrepared(), d) {
		t.Fatalf("not the case of seed 710078: the last settle is not truncated with dnuma@vm0#0's Prepare optional")
	}
	for _, a := range alts(last) {
		if a.Slots[d].Phase != Running {
			t.Fatalf("an enumerated outcome has dnuma@vm0#0 %s: no case of the outcome bound any more", a.Slots[d].Phase)
		}
	}
	n := last.narrowed(nil, map[int]bool{d: false})
	if n == nil || n.Narrow != "dnuma@vm0#0:unbound" {
		t.Fatalf("narrowed %v", n)
	}
	for _, a := range alts(n) {
		if a.Slots[d].Phase != Either {
			t.Fatalf("narrowed: dnuma@vm0#0 %s, want either", a.Slots[d].Phase)
		}
	}
	back, err := last.narrowTo(n.Narrow)
	if err != nil || back.Narrow != n.Narrow || len(alts(back)) != len(alts(n)) || back.altKey() != n.altKey() {
		t.Fatalf("narrowTo %q: %v", n.Narrow, err)
	}
	if b, err := last.narrowTo("dnuma@vm0#0:bound"); err != nil || b.Slots[d].Phase != Running {
		t.Fatalf("narrowTo bound: %v", err)
	}
	if _, err := last.narrowTo("dnuma@vm0#0:maybe"); err == nil {
		t.Fatalf("narrowTo maybe accepted")
	}
}
