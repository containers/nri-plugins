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

// Fencing modes, the exact O5, the two-claim kind dnuma and the cpuset.mems
// coverage strings (A36-A40).

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	m "github.com/askervin/gofmbt/gofmbt"
)

// fencingConfig is the full profile (off-line: no "any" slots) in a
// fencing mode.
func fencingConfig(t *testing.T, mode FencingRun) *Config {
	t.Helper()
	cfg := testConfig(t, "full", false)
	cfg.Fencing = mode
	return cfg
}

// memsCase changes cpuset.mems.effective of a container of an observation
// and wants a verdict.
type memsCase struct {
	vm, pod, mems string
	status        VStatus
	want          string
}

func checkMemsCases(t *testing.T, s *State, cases []memsCase) {
	t.Helper()
	o := observation(t, s)
	v, _ := checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	for _, c := range cases {
		o := observation(t, s)
		ctrOf(t, o, c.vm, c.pod).MemsEffective = c.mems
		v, _ := checkState(s, o, "")
		if v.Status != c.status || !strings.Contains(v.Msg, c.want) {
			t.Errorf("%s %s %q: verdict %s %q, want %s with %q; diffs:\n  %s", c.vm, c.pod, c.mems, v.Status, v.Msg, c.status, c.want, strings.Join(v.Diffs, "\n  "))
		}
	}
}

// allowed-mems: a node-3-only claimer while node 2 is fenced with memory,
// a node-2 claimer, a plain container, a dnuma container: exactly D + E.
func TestFencingExactAllowedMems(t *testing.T) {
	cfg := fencingConfig(t, FencingAM)
	s := steps(t, newState(cfg), "create lnuma@vm0#0", "create xnuma-c@vm0#0", "create plain@vm0#0", "settle")
	if got := s.fenceNUMANodes(s.Drv[VM0].F); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("vm0 fenced %v, want [2 3]", got)
	}
	checkMemsCases(t, s, []memsCase{
		{"vm0", "mbt-xnuma-c", "0,1,3", VOK, "ok"},
		{"vm0", "mbt-xnuma-c", "0-3", VWait, "has fenced CXL node 2"},
		{"vm0", "mbt-xnuma-c", "0-1", VWait, "lacks its NUMA node 3"},
		{"vm0", "mbt-xnuma-c", "1,3", VWait, "lacks DRAM NUMA node 0"},
		{"vm0", "mbt-xnuma-c", "0,1,3,4", VWait, "has node 4"},
		{"vm0", "mbt-lnuma", "0-2", VOK, "ok"},
		{"vm0", "mbt-lnuma", "0-3", VWait, "has fenced CXL node 3"},
		{"vm0", "mbt-plain", "0-1", VOK, "ok"},
		{"vm0", "mbt-plain", "0,1,3", VWait, "has fenced CXL node 3"},
	})
	// both nodes in one container (A39)
	s = steps(t, newState(cfg), "create dnuma@vm1#0", "create plain@vm1#0", "settle")
	wantPhase(t, s, "dnuma@vm1#0", Running)
	if e, m := s.claimNUMANodes(cfg.slotIndex("dnuma@vm1#0")), s.memNUMANodes(VM1); e.String() != "2,3" || m.String() != "2,3" {
		t.Fatalf("E %s M %s, want 2,3 and 2,3", e, m)
	}
	checkMemsCases(t, s, []memsCase{
		{"vm1", "mbt-dnuma", "0-3", VOK, "ok"},
		{"vm1", "mbt-dnuma", "0-2", VWait, "lacks its NUMA node 3"},
		{"vm1", "mbt-dnuma", "0,1,3", VWait, "lacks its NUMA node 2"},
		{"vm1", "mbt-plain", "0-1", VOK, "ok"},
		{"vm1", "mbt-plain", "0-2", VWait, "has fenced CXL node 2"},
	})
	// a steered container: its node, no CXL node without memory there
	s = steps(t, newState(cfg), "create lsteer@vm2#0", "create xnuma-c@vm2#0", "settle")
	checkMemsCases(t, s, []memsCase{
		{"vm2", "mbt-lsteer", "0,2", VOK, "ok"},
		{"vm2", "mbt-lsteer", "0-2", VOK, "ok"},
		{"vm2", "mbt-lsteer", "0", VWait, "lacks its NUMA node 2"},
	})
	s = steps(t, newState(cfg), "create lsteer@vm2#0", "settle")
	checkMemsCases(t, s, []memsCase{
		{"vm2", "mbt-lsteer", "0,2,3", VWait, "has CXL node 3"},
	})
	// a lent node (a held numa lease) and the companion
	s = steps(t, newState(cfg), "create lease@vm1#0", "settle", "lease-get lease@vm1#0 numa", "settle")
	checkMemsCases(t, s, []memsCase{
		{"vm1", "mbt-lease", "0-2", VOK, "ok"},
		{"vm1", "mbt-lease", "0-1", VWait, "lacks its NUMA node 2"},
		{"vm1", "cxl-lease", "0-2", VWait, "has fenced CXL node 2"},
	})
}

// The rule itself: FMaybe either way, nodes without memory never.
func TestFencingRule(t *testing.T) {
	d := numaSetOf([]int{0, 1})
	n2, n3 := numaBit(2), numaBit(3)
	for _, c := range []struct {
		mode           FenceMode
		e, f, fm, m    numaSet
		steered, loose bool
		must, allowed  string
	}{
		{FenceAM, 0, n2, 0, n2, false, false, "0,1", "0,1"},
		{FenceAM, n3, n2 | n3, 0, n2 | n3, false, false, "0,1,3", "0,1,3"},
		{FenceAM, 0, 0, n2, n2, false, false, "0,1", "0,1,2"},
		{FenceAM, 0, 0, 0, n3, false, false, "0,1,3", "0,1,3"}, // (M \ F)
		{FenceNone, 0, 0, 0, n2 | n3, false, false, "0,1,2,3", "0,1,2,3"},
		{FenceNone, n2, 0, 0, n2, false, false, "0,1,2", "0,1,2"},
		{FenceNone, n2, 0, 0, n2 | n3, true, false, "2", "0,1,2,3"},
		{FenceNone, 0, 0, 0, n2, false, true, "0,1", "0,1,2"},
		{FenceAM, n2, n2, 0, n2, true, false, "2", "0,1,2"},
	} {
		r := memsRule(c.mode, d, c.e, c.f, c.fm, c.m, c.steered, c.loose)
		if r.must.String() != c.must || r.allowed.String() != c.allowed {
			t.Errorf("%+v: %s, want %s and at most %s", c, r, c.must, c.allowed)
		}
	}
}

// none: every container has D + M, whatever it claims.
func TestFencingExactNone(t *testing.T) {
	cfg := fencingConfig(t, FencingNone)
	s := steps(t, newState(cfg), "create lnuma@vm0#0", "create xnuma-c@vm0#0", "create plain@vm0#0", "settle")
	for v := VM(0); v < nVMs; v++ {
		if s.Drv[v].F != 0 || s.Drv[v].FMaybe != 0 {
			t.Fatalf("driver %s fences %v %v with fencing none", v, s.fenceNUMANodes(s.Drv[v].F), s.fenceNUMANodes(s.Drv[v].FMaybe))
		}
	}
	checkMemsCases(t, s, []memsCase{
		{"vm0", "mbt-plain", "0-3", VOK, "ok"},
		{"vm0", "mbt-plain", "0-1", VWait, "lacks CXL NUMA node 2 with memory"},
		{"vm0", "mbt-xnuma-c", "0,1,3", VWait, "lacks CXL NUMA node 2 with memory"},
		{"vm0", "mbt-lnuma", "0-3", VOK, "ok"},
	})
	s = steps(t, newState(cfg), "create lease@vm1#0", "create dnuma@vm1#0", "settle", "lease-get lease@vm1#0 numa", "settle")
	checkMemsCases(t, s, []memsCase{
		{"vm1", "mbt-dnuma", "0-3", VOK, "ok"},
		{"vm1", "mbt-dnuma", "0-2", VWait, "lacks its NUMA node 3"},
		{"vm1", "mbt-lease", "0-3", VOK, "ok"},
		{"vm1", "mbt-lease", "0-2", VWait, "lacks CXL NUMA node 3 with memory"},
		{"vm1", "cxl-lease", "0-3", VOK, "ok"},
	})
	// a node without memory is never there
	s = steps(t, newState(cfg), "create plain@vm2#0", "settle")
	checkMemsCases(t, s, []memsCase{
		{"vm2", "mbt-plain", "0-1", VOK, "ok"},
		{"vm2", "mbt-plain", "0-2", VWait, "has CXL node 2"},
	})
}

// fencing-switch: the mode flips, every driver restarts (A37); the
// containers that existed at the switch to none are loose until they are
// created again; after the switch back every container is exact again.
func TestFencingSwitch(t *testing.T) {
	sw := mustAct(t, fencingConfig(t, FencingSwitch), "fencing-switch")
	if s := newState(fencingConfig(t, FencingAM)); s.allowed(sw) || slices.Contains(allActs(s.cfg), sw) {
		t.Fatalf("fencing-switch without --fencing switch")
	}
	cfg := fencingConfig(t, FencingSwitch)
	if !slices.Contains(allActs(cfg), sw) {
		t.Fatalf("no fencing-switch with --fencing switch")
	}
	// one fencing-switch per window, also after other steps of it
	steps(t, newState(cfg), "fencing-switch", "!fencing-switch", "create plain@vm0#0", "!fencing-switch", "settle", "fencing-switch")
	s := steps(t, newState(cfg), "create lnuma@vm0#0", "create plain@vm0#0", "create lsteer@vm2#0", "create lease@vm1#0", "settle", "lease-get lease@vm1#0 numa", "settle")
	if s.Fence != FenceAM || s.Drv[VM0].F == 0 {
		t.Fatalf("switch starts in allowed-mems:\n%s", s.dump())
	}
	n := steps(t, s, "fencing-switch")
	for v := VM(0); v < nVMs; v++ {
		d := &n.Drv[v]
		if !d.Restarted || d.PodGen != s.Drv[v].PodGen+1 || d.F != 0 || d.FMaybe != 0 || d.PodBefore != s.Drv[v].Pod {
			t.Fatalf("driver %s after the switch: %+v", v, *d)
		}
	}
	if n.Fence != FenceNone || !slotOf(t, n, "plain@vm0#0").Loose || !slotOf(t, n, "lease@vm1#0").CompLoose || slotOf(t, n, "lsteer@vm2#0").Steer != SteerOff {
		t.Fatalf("after the switch to none:\n%s", n.dump())
	}
	// a driver killed in the window, an armed watcher: no switch
	steps(t, n, "driver-kill vm1", "!fencing-switch")
	n = steps(t, n, "settle")
	checkMemsCases(t, n, []memsCase{
		{"vm0", "mbt-plain", "0-1", VOK, "ok"}, // as before
		{"vm0", "mbt-plain", "0-2", VOK, "ok"}, // as without fencing
		{"vm0", "mbt-plain", "0,2", VWait, "lacks DRAM NUMA node 1"},
		{"vm0", "mbt-plain", "0,1,3", VWait, "has CXL node 3"},
		{"vm1", "cxl-lease", "0-1", VOK, "ok"},
		{"vm1", "cxl-lease", "0-2", VOK, "ok"},
		{"vm2", "mbt-lsteer", "0,2", VOK, "ok"}, // its manager's
	})
	// created again: exact
	n = steps(t, n, "crash plain@vm0#0", "settle")
	if slotOf(t, n, "plain@vm0#0").Loose || !slotOf(t, n, "lnuma@vm0#0").Loose {
		t.Fatalf("after the crash:\n%s", n.dump())
	}
	checkMemsCases(t, n, []memsCase{
		{"vm0", "mbt-plain", "0-2", VOK, "ok"},
		{"vm0", "mbt-plain", "0-1", VWait, "lacks CXL NUMA node 2 with memory"},
	})
	// and back: the first fence pass makes every container exact
	n = steps(t, n, "fencing-switch", "settle")
	if n.Fence != FenceAM || slotOf(t, n, "lnuma@vm0#0").Loose || slotOf(t, n, "lease@vm1#0").CompLoose {
		t.Fatalf("after the switch to allowed-mems:\n%s", n.dump())
	}
	if got := n.fenceNUMANodes(n.Drv[VM0].F); !slices.Equal(got, []int{2}) {
		t.Fatalf("vm0 fenced %v after the switch, want [2] (reseeded)", got)
	}
	checkMemsCases(t, n, []memsCase{
		{"vm0", "mbt-plain", "0-1", VOK, "ok"},
		{"vm0", "mbt-plain", "0-2", VWait, "has fenced CXL node 2"},
		{"vm1", "cxl-lease", "0-2", VWait, "has fenced CXL node 2"},
	})
	// C2: the switch is a fault of every pod, with its direction
	fc := faultContext(s, sw)
	if !slices.Contains(fc, "fault fencing-switch>none plain") || !slices.Contains(fc, "fault fencing-switch>none lease-numa") {
		t.Fatalf("C2 of the switch: %q", fc)
	}
	if fc := faultContext(n, sw); !slices.Contains(fc, "fault fencing-switch>none lnuma") {
		t.Fatalf("C2 of the switch back: %q", fc)
	}
}

// The executor's side of A36-A39:
// the done of fencing-switch names the new mode, the driver log the mode
// of the driver, and in mode none a lease-put narrows nothing (A38: no
// tolerance for a driver that narrows).
func TestFencingExecutorEvidence(t *testing.T) {
	cfg := fencingConfig(t, FencingSwitch)
	s := steps(t, newState(cfg), "create lnuma@vm1#0", "settle")
	sw := mustAct(t, cfg, "fencing-switch")
	n := s.apply(sw)
	e := expectDone(s, n, sw, int(n.Steps))
	for _, c := range []struct {
		js     string
		status VStatus
	}{{`{"fencing":"none"}`, VOK}, {`{}`, VOK}, {`{"fencing":"allowed-mems"}`, VFail}} {
		d, err := parseDone(0, []byte(c.js))
		if err != nil {
			t.Fatal(err)
		}
		if v := checkDone(e, d); v.Status != c.status {
			t.Fatalf("done %s: %s %q, want %s", c.js, v.Status, v.Msg, c.status)
		}
	}
	// a requester created in none: not loose
	n = steps(t, n, "settle", "create lease@vm1#0", "settle", "lease-get lease@vm1#0 numa", "settle", "lease-put lease@vm1#0", "settle")
	o := observation(t, n)
	v, _ := checkState(n, o, "")
	wantVerdict(t, v, VOK, "ok")
	ctrOf(t, o, "vm1", "mbt-lease").MemsEffective = "0-1" // a revoke that narrows (A38)
	v, _ = checkState(n, o, "")
	wantVerdict(t, v, VWait, "lacks CXL NUMA node 2 with memory")
	o = observation(t, n)
	am := "allowed-mems"
	o.VMs["vm2"].DriverLog.Fencing = &am
	v, _ = checkState(n, o, "")
	wantVerdict(t, v, VWait, "driver vm2: fencing allowed-mems, want none")
	// the executor's claims of a dnuma pod: c0 local (attached null),
	// c1 c-pooled0
	s = steps(t, newState(fencingConfig(t, FencingAM)), "create dnuma@vm0#0", "settle")
	o = observation(t, s)
	p := podOf(t, o, "dnuma@vm0#0")
	p.Claims[0].Attached = nil
	p.Claims[0].Devices[0].Pool = "vm0-cxl"
	p.Claim = &p.Claims[0]
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
}

// dnuma: one local block and c-pooled0; Pending until both are free, the
// waiter of both; Running with both regions; its env.
func TestDualOccupancy(t *testing.T) {
	cfg := fencingConfig(t, FencingAM)
	i := cfg.slotIndex("dnuma@vm1#0")
	if i < 0 || cfg.slotIndex("dnuma@vm0#0") < 0 || cfg.slotIndex("xnuma-c@vm0#0") < 0 || cfg.slotIndex("xnuma-c@vm2#0") < 0 {
		t.Fatalf("profile full without the slots of A39")
	}
	if sl := &cfg.Slots[i]; sl.KindName() != "dnuma" || sl.Label() != "dnuma_vm1_0" || sl.Dev != pooledDev(PoolC) || !sl.holdsRes(localRes(VM1)) {
		t.Fatalf("slot %+v", *sl)
	}
	// vm1 has two blocks: both taken, dnuma waits for one
	s := steps(t, newState(cfg), "create lnuma@vm1#0", "create ldax@vm1#0", "settle", "create dnuma@vm1#0")
	wantPhase(t, s, "dnuma@vm1#0", Pending)
	// the waiter of both resources: no other claim of them
	s = steps(t, s, "!create xnuma-c@vm0#0", "!create ldax@vm1#1", "settle")
	// a block goes: it gets both
	s = steps(t, s, "delete lnuma@vm1#0")
	wantPhase(t, s, "dnuma@vm1#0", Starting)
	s = steps(t, s, "settle")
	wantPhase(t, s, "dnuma@vm1#0", Running)
	wantStack(t, s, stackIndex(VM1, HB0), "[local]")
	wantStack(t, s, stackIndex(VM1, HB1), "[c-pooled0]")
	if held, _ := s.localUse(VM1); held != 2 {
		t.Fatalf("vm1 local blocks %d, want 2", held)
	}
	// c-pooled0 is taken: xnuma-c waits
	s = steps(t, s, "create xnuma-c@vm2#0")
	wantPhase(t, s, "xnuma-c@vm2#0", Pending)
	s = steps(t, s, "settle")
	o := observation(t, s)
	v, _ := checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	// the env of both claims (O6)
	in := o.Inspect["dnuma@vm1#0"]
	if in == nil || in.Env["CXL_C1EE2001_NUMA_NODE"] != "3" || in.Env["CXL_C100E2E1_NUMA_NODE"] != "2" || in.Env["CXL_C1EE2001_SIZE"] != "268435456" {
		t.Fatalf("env %v", in)
	}
	o.Inspect["dnuma@vm1#0"].Env["CXL_C100E2E1_NUMA_NODE"] = "3"
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VFail, "CXL_C100E2E1_NUMA_NODE=3, want 2")
	// its claims: both are needed, in either order
	o = observation(t, s)
	p := podOf(t, o, "dnuma@vm1#0")
	p.Claims[0], p.Claims[1] = p.Claims[1], p.Claims[0]
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	p.Claims = p.Claims[:1]
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VFail, "want its two (A39)")
	// its local claim in state.json, its pool claim in the pool's
	o = observation(t, s)
	st := o.VMs["vm1"].State
	st.Prepared[localDriver] = slices.DeleteFunc(st.Prepared[localDriver], func(u string) bool { return strings.HasSuffix(u, "-local") })
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VWait, "of cxl.nri.io is not prepared")
	// MemTotal of both nodes
	o = observation(t, s)
	o.VMs["vm1"].Nodes["3"] = ObsNUMANode{MemTotal: 0}
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VWait, "vm1: NUMA node 3 MemTotal 0")
	// deleted: both regions go, the waiter of c-pooled0 gets it
	s = steps(t, s, "delete dnuma@vm1#0")
	wantPhase(t, s, "xnuma-c@vm2#0", Starting)
	s = steps(t, s, "settle")
	wantStack(t, s, stackIndex(VM1, HB1), "[]")
	if held, _ := s.localUse(VM1); held != 1 {
		t.Fatalf("vm1 local blocks %d, want 1", held)
	}
	// crash: restarted like lnuma; no write or read
	s = steps(t, s, "create dnuma@vm0#0", "settle")
	wantPhase(t, s, "dnuma@vm0#0", Pending) // (c-pooled0 is xnuma-c's)
	s = steps(t, s, "delete xnuma-c@vm2#0", "settle", "crash dnuma@vm0#0", "settle")
	if ss := slotOf(t, s, "dnuma@vm0#0"); ss.Phase != Running || ss.Restarts != 1 {
		t.Fatalf("dnuma after its crash: %+v", *ss)
	}
	if slices.Contains(allActs(cfg), mustAct(t, cfg, "write dnuma@vm0#0")) {
		t.Fatalf("write of dnuma generated")
	}
}

// A dnuma pod is never Blocked: no step after which one of its Prepares
// could be refused by a decoder hole, no delete before it is bound.
func TestDualNeverBlocked(t *testing.T) {
	cfg := fencingConfig(t, FencingAM)
	s := steps(t, newState(cfg), "create xnuma-c@vm0#0", "settle", "create sdax-c@vm0#0", "settle", "delete xnuma-c@vm0#0", "settle")
	wantStack(t, s, stackIndex(VM0, HB1), "[hole,c-shared0]")
	if a := mustAct(t, cfg, "create dnuma@vm0#0"); !s.allowed(a) || s.enabled(a) {
		t.Fatalf("create dnuma over a hole: allowed %v enabled %v", s.allowed(a), s.enabled(a))
	}
	// the hole clears: fine
	s = steps(t, s, "delete sdax-c@vm0#0", "settle", "create dnuma@vm0#0")
	// not deleted before it is bound
	s = steps(t, s, "!delete dnuma@vm0#0", "settle", "delete dnuma@vm0#0", "settle")
	// a hole made in the window after its Prepare could come first
	steps(t, s, "create xnuma-c@vm0#0", "settle", "create sdax-c@vm0#0", "settle", "delete xnuma-c@vm0#0", "!create dnuma@vm0#0")
	// the controller killed while it starts: both claims or none
	cfg2 := testConfig(t, "full", true)
	s = steps(t, newState(cfg2), "create dnuma@vm1#0", "ctrl-kill c", "settle")
	for _, a := range alts(s) {
		ss := slotOf(t, a, "dnuma@vm1#0")
		pool, local := a.Stacks[stackIndex(VM1, HB1)].contains(devKey(pooledDev(PoolC))), a.Stacks[stackIndex(VM1, HB0)].contains(rkLocal)
		if pool != local || (ss.Phase == Running) != pool {
			t.Fatalf("an outcome with one claim prepared:\n%s", a.dump())
		}
	}
	if len(alts(s)) < 2 {
		t.Fatalf("want both outcomes of the kill, got %d", len(alts(s)))
	}
}

// A40: the strings of a settle, and the planner gets them as C2.
func TestMemsStrings(t *testing.T) {
	cfg := fencingConfig(t, FencingAM)
	s := steps(t, newState(cfg), "create lnuma@vm0#0", "create xnuma-c@vm0#0", "create plain@vm0#0", "settle")
	got := memsStrings(s)
	want := []string{"mems|am|e=-|f=2,3|m=2,3", "mems|am|e=2|f=2,3|m=2,3", "mems|am|e=3|f=2,3|m=2,3"}
	if !slices.Equal(got, want) {
		t.Fatalf("mems strings %q, want %q", got, want)
	}
	s = steps(t, newState(cfg), "create dnuma@vm1#0", "settle")
	if got, want := memsStrings(s), []string{"mems|am|e=2,3|f=2,3|m=2,3"}; !slices.Equal(got, want) {
		t.Fatalf("mems strings %q, want %q", got, want)
	}
	cfg = fencingConfig(t, FencingNone)
	s = steps(t, newState(cfg), "create xnuma-c@vm0#0", "create plain@vm2#0", "settle")
	if got, want := memsStrings(s), []string{"mems|none|e=-|f=-|m=-", "mems|none|e=3|f=-|m=3"}; !slices.Equal(got, want) {
		t.Fatalf("mems strings %q, want %q", got, want)
	}
	mm := newMbtModel(cfg)
	cs := newCovStrings(mm, Coverage{W: [4]int{1, 2, 1, 0}})
	n := steps(t, newState(cfg), "create xnuma-c@vm0#0")
	settle := Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool}
	strs := cs.computeStep(m.NewStep(n, mm.actions[mm.byAct[settle]], n.apply(settle)))
	if !slices.Contains(strs, "C2.0|mems|none|e=3|f=-|m=3") || !slices.Contains(strs, "C2.1|mems|none|e=3|f=-|m=3") {
		t.Fatalf("settle step strings %q", strs)
	}
}

// The mode is in the steps file's header and in expectations (A36).
func TestFencingHeaderAndExpectation(t *testing.T) {
	fs := flag.NewFlagSet("online", flag.ContinueOnError)
	var c commonFlags
	c.register(fs, true)
	if err := fs.Parse([]string{"--fencing", "switch"}); err != nil {
		t.Fatal(err)
	}
	o, err := c.options(true, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if o.cfg.Fencing != FencingSwitch || !strings.Contains(o.header, "--fencing switch") {
		t.Fatalf("fencing %s, header %q", o.cfg.Fencing, o.header)
	}
	// a replay takes it from the header, a file without it is allowed-mems
	fs2 := flag.NewFlagSet("online", flag.ContinueOnError)
	var c2 commonFlags
	c2.register(fs2, true)
	_, header := readSteps("# mbt-cxl online\n# " + o.header + "\nsettle\n")
	if err := applyHeader(fs2, header, &bytes.Buffer{}); err != nil || c2.fencing != "switch" {
		t.Fatalf("fencing %q from the header (%v)", c2.fencing, err)
	}
	if err := fs.Parse([]string{"--fencing", "both"}); err == nil {
		if _, err := c.options(true, &bytes.Buffer{}); err == nil {
			t.Fatalf("--fencing both accepted")
		}
	}
	// the expectation of a state after a switch to none
	cfg := fencingConfig(t, FencingSwitch)
	s := steps(t, newState(cfg), "create plain@vm0#0", "create lease@vm1#0", "settle", "lease-get lease@vm1#0 dax", "fencing-switch", "settle")
	e := s.expect(int(s.Steps), "settle")
	js, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back Expectation
	if err := json.Unmarshal(js, &back); err != nil {
		t.Fatal(err)
	}
	r, err := back.state()
	if err != nil {
		t.Fatal(err)
	}
	if r.Fence != FenceNone || !slotOf(t, r, "plain@vm0#0").Loose || !slotOf(t, r, "lease@vm1#0").CompLoose {
		t.Fatalf("round trip:\n%s\nfrom %s", r.dump(), js)
	}
	if r.key() != s.key() {
		t.Fatalf("keys differ after the round trip:\n%s\n%s", r.dump(), s.dump())
	}
}

// A replay and generate --from take the model of the steps file's header.
// Its --fencing and --reboot also configure the cluster (code.var.sh): a
// command line that contradicts them is an error, not replaced, and a
// header without them is not of this mbt-cxl; a file without a header
// takes the command line's.
func TestStepsFileHeader(t *testing.T) {
	parse := func(header string, args ...string) (*commonFlags, error) {
		t.Helper()
		fs := flag.NewFlagSet("online", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		c := &commonFlags{}
		c.register(fs, true)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		_, h := readSteps(header + "settle\n")
		return c, applyHeader(fs, h, io.Discard)
	}
	const hdr = "# mbt-cxl online seed=1\n# --profile full --seed 1 --avoid \"\" --fencing switch --reboot=guest --steps 9\n"
	const old = "# mbt-cxl online seed=1\n# --profile full --seed 1 --avoid \"\" --steps 9\n"
	for _, tc := range []struct {
		header  string
		args    []string
		err     string
		fencing string
		reboot  string
	}{
		{hdr, nil, "", "switch", "guest"},
		{hdr, []string{"--fencing=switch", "--reboot=guest"}, "", "switch", "guest"},
		{hdr, []string{"--fencing", "none"}, "--fencing=none of the command line, but the steps file is of --fencing=switch", "", ""},
		{hdr, []string{"--reboot=qemu"}, "--reboot=qemu of the command line, but the steps file is of --reboot=guest", "", ""},
		// MBT_REBOOT= (set, empty) cannot run a file with reboots
		{hdr, []string{"--reboot="}, "--reboot= of the command line, but the steps file is of --reboot=guest", "", ""},
		// a header without them is not of this mbt-cxl
		{old, nil, "the header of the steps file has no --", "", ""},
		{old, []string{"--fencing", "allowed-mems", "--reboot="}, "the header of the steps file has no --", "", ""},
		{"", []string{"--fencing", "none", "--reboot=qemu"}, "", "none", "qemu"},
	} {
		c, err := parse(tc.header, tc.args...)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%q %v: error %v, want %q", tc.header, tc.args, err, tc.err)
		case tc.err == "" && err != nil:
			t.Errorf("%q %v: %v", tc.header, tc.args, err)
		case tc.err == "" && (c.fencing != tc.fencing || c.reboot != tc.reboot):
			t.Errorf("%q %v: --fencing %q --reboot %q, want %q %q", tc.header, tc.args, c.fencing, c.reboot, tc.fencing, tc.reboot)
		}
	}
	// the other model flags of the header replace the command line's,
	// with a message; --steps, --seed and the facts of the cluster
	// (--local-blocks) stay the command line's
	fs := flag.NewFlagSet("online", flag.ContinueOnError)
	var c commonFlags
	c.register(fs, true)
	if err := fs.Parse([]string{"--profile", "full", "--steps", "7", "--avoid", "crash"}); err != nil {
		t.Fatal(err)
	}
	_, h := readSteps("# mbt-cxl online seed=1\n# --profile small --seed 9 --avoid \"\" --avoid-holes=true --local-blocks vm2=8,vm1=1,vm0=2 --steps 40 --plan-ahead=true --fencing allowed-mems --reboot=\nsettle\n")
	var msgs bytes.Buffer
	if err := applyHeader(fs, h, &msgs); err != nil {
		t.Fatal(err)
	}
	if c.profile != "small" || c.avoid != "" || !c.avoidHoles || c.localBlocks != "vm2=8,vm1=2,vm0=2" || c.steps != 7 || c.seed != 0 {
		t.Fatalf("flags %+v", c)
	}
	if !strings.Contains(msgs.String(), `--profile "full" of the command line replaced by "small"`) {
		t.Fatalf("no message: %q", msgs.String())
	}
	// online --replay and generate --from refuse to start
	file := filepath.Join(t.TempDir(), "mbt-steps.txt")
	if err := os.WriteFile(file, []byte(hdr+"settle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"online", "--replay", file, "--fencing", "none"}, {"generate", "--from", file, "--reboot=qemu"}} {
		var out, errb bytes.Buffer
		if rc := run(args, strings.NewReader(""), &out, &errb); rc != 2 || !strings.Contains(errb.String(), "configures the cluster too") {
			t.Fatalf("%v: exit %d, %q", args, rc, errb.String())
		}
	}
}
