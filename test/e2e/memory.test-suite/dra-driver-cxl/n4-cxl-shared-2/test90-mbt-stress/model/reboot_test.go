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

// node-reboot (A41-A44): the verb,
// the transition per kind, leases and dnuma at a reboot, fencing modes,
// the boot ids, the device nodes of the containers, generation.

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// rebootConfig is a profile with --reboot=guest.
func rebootConfig(t *testing.T, profile string, online bool) *Config {
	t.Helper()
	cfg := testConfig(t, profile, online)
	cfg.Reboot = RebootGuest
	return cfg
}

// The verb: vm0 and vm1 only, with --reboot only, MBT_AVOID removes it,
// the steps file's header carries --reboot (A41).
func TestNodeRebootVerb(t *testing.T) {
	cfg := rebootConfig(t, "full", true)
	if _, err := parseAct(cfg, "node-reboot vm2"); err == nil {
		t.Fatalf("node-reboot vm2 parsed (vm2 runs the control plane)")
	}
	a := mustAct(t, cfg, "node-reboot vm1")
	if a.name(cfg) != "node-reboot vm1" || !a.Verb.vmFault() {
		t.Fatalf("%+v: %q", a, a.name(cfg))
	}
	var reboots []string
	for _, x := range allActs(cfg) {
		if x.Verb == VNodeReboot {
			reboots = append(reboots, x.name(cfg))
		}
	}
	if !slices.Equal(reboots, []string{"node-reboot vm0", "node-reboot vm1"}) {
		t.Fatalf("node-reboot actions %q", reboots)
	}
	none := testConfig(t, "full", true)
	if slices.Contains(allActs(none), a) || newState(none).allowed(a) {
		t.Fatalf("node-reboot without --reboot")
	}
	cfg.Avoid = []string{"node-reboot"}
	if slices.Contains(allActs(cfg), a) {
		t.Fatalf("MBT_AVOID=node-reboot keeps it")
	}
	cfg.Avoid = []string{"node-reboot:vm0"}
	if acts := allActs(cfg); slices.Contains(acts, mustAct(t, cfg, "node-reboot vm0")) || !slices.Contains(acts, a) {
		t.Fatalf("MBT_AVOID=node-reboot:vm0")
	}
	// the header (A16)
	var c commonFlags
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c.register(fs, true)
	if err := applyHeader(fs, splitFlags(`--profile full --fencing allowed-mems --reboot=qemu --max-paths 0`), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	o, err := c.options(true, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if o.cfg.Reboot != RebootQemu || !strings.Contains(o.header, "--reboot=qemu") {
		t.Fatalf("reboot %q, header %q", o.cfg.Reboot, o.header)
	}
	if _, err := parseRebootRun("warm"); err == nil {
		t.Fatalf("--reboot=warm parsed")
	}
}

// When it is generated: after a settle, every controller up, nothing on
// the VM waiting for a retry or a promotion; then only a settle (and
// synchronous steps elsewhere) until the settle.
func TestNodeRebootPreconditions(t *testing.T) {
	cfg := rebootConfig(t, "full", true)
	s := steps(t, newState(cfg), "create plain@vm1#0", "create xdax-b@vm0#0", "!node-reboot vm1", "settle")
	steps(t, s, "node-reboot vm1",
		// forced: only synchronous steps, not on the VM's pods
		"!create plain@vm0#0", "!crash plain@vm1#0", "!node-reboot vm0", "write xdax-b@vm0#0", "settle", "node-reboot vm0")
	steps(t, s, "ctrl-kill a", "settle", "!node-reboot vm1", "!node-reboot vm0", "ctrl-start a", "settle", "node-reboot vm1")
	// an unblocking pod on the VM: its retry may come before the boot
	u := steps(t, newState(cfg), "create xnuma-a@vm1#0", "create sdax-a@vm1#0", "settle", "delete xnuma-a@vm1#0", "settle",
		"create xdax-a@vm1#0", "settle")
	wantPhase(t, u, "xdax-a@vm1#0", Blocked)
	u = steps(t, u, "node-reboot vm1", "settle")
	wantPhase(t, u, "xdax-a@vm1#0", Running)
	u = steps(t, newState(cfg), "create xnuma-a@vm1#0", "create sdax-a@vm1#0", "settle", "delete xnuma-a@vm1#0", "settle",
		"create xdax-a@vm1#0", "settle", "delete sdax-a@vm1#0", "settle")
	wantPhase(t, u, "xdax-a@vm1#0", Unblocking)
	steps(t, u, "!node-reboot vm1", "node-reboot vm0")
}

// The transition per kind (A43): restarts, phases, stacks, data, F,
// steering, the driver; and the settle that the oracle checks against the
// simulated SUT's observation with the stacks in another order.
func TestNodeRebootTransition(t *testing.T) {
	cfg := rebootConfig(t, "full", true)
	s := steps(t, newState(cfg), "create xnuma-a@vm1#0", "create sdax-c@vm2#0", "create sdax-a@vm1#0", "create lnuma@vm1#0",
		"create ldax@vm1#0", "create xnuma-c@vm1#0", "create plain@vm1#0", "create lsteer@vm1#0", "create plain@vm0#0",
		"create sdax-a@vm2#0", "settle", "crash plain@vm1#0", "settle")
	wantPhase(t, s, "lsteer@vm1#0", Pending) // (vm1 has 2 local blocks)
	s = steps(t, s, "write sdax-a@vm1#0", "write ldax@vm1#0")
	tok := s.Data[sharedDev(PoolA)]
	before := s
	n := steps(t, s, "node-reboot vm1")
	d := &n.Drv[VM1]
	if d.Restarts != before.Drv[VM1].Restarts+1 || d.Slack != 1 || !d.Rebooted || d.Reboots != 1 || n.Unsettled == 0 || !n.forced() {
		t.Fatalf("driver after the reboot: %+v (unsettled %d)", *d, n.Unsettled)
	}
	for _, name := range []string{"xnuma-a@vm1#0", "sdax-a@vm1#0", "lnuma@vm1#0", "ldax@vm1#0", "xnuma-c@vm1#0", "plain@vm1#0"} {
		wantPhase(t, n, name, Restarting)
		if ss, bs := slotOf(t, n, name), slotOf(t, before, name); ss.Restarts != bs.Restarts+1 {
			t.Fatalf("%s: %d restarts, want %d", name, ss.Restarts, bs.Restarts+1)
		}
	}
	if slotOf(t, n, "plain@vm1#0").Slack != 1 || slotOf(t, n, "lnuma@vm1#0").Slack != 0 {
		t.Fatalf("slack: plain %d (want 1: no claim to wait for), lnuma %d", slotOf(t, n, "plain@vm1#0").Slack, slotOf(t, n, "lnuma@vm1#0").Slack)
	}
	wantPhase(t, n, "lsteer@vm1#0", Pending)
	wantPhase(t, n, "sdax-c@vm2#0", Running)
	wantPhase(t, n, "plain@vm0#0", Running)
	if n.Stacks[stackIndex(VM1, HB0)].N != 0 || n.Stacks[stackIndex(VM1, HB1)].N != 0 {
		t.Fatalf("vm1's stacks before its settle:\n%s", n.dump())
	}
	if n.Reorder != 1<<uint(stackIndex(VM1, HB0)) {
		t.Fatalf("reorder %b, want vm1/hb0 only (vm1/hb1 has c-pooled0 only)", n.Reorder)
	}
	n = steps(t, n, "settle")
	for _, name := range []string{"xnuma-a@vm1#0", "sdax-a@vm1#0", "lnuma@vm1#0", "ldax@vm1#0", "xnuma-c@vm1#0", "plain@vm1#0"} {
		wantPhase(t, n, name, Running)
	}
	wantStack(t, n, stackIndex(VM1, HB0), "[a-pooled0,a-shared0,local]")
	wantStack(t, n, stackIndex(VM1, HB1), "[c-pooled0]")
	if n.Reorder == 0 || !n.Drv[VM1].Rebooted || n.String() != strings.ReplaceAll(n.String(), "rebooted", "") {
		t.Fatalf("settled: reorder %b, rebooted %v, abstract %q", n.Reorder, n.Drv[VM1].Rebooted, n.String())
	}
	if got := n.Data[sharedDev(PoolA)]; !got.OrZero || got.Text != tok.Text || n.SlotData[cfg.slotIndex("ldax@vm1#0")] != tokZero {
		t.Fatalf("data: a-shared0 %s (before %s), ldax %s", got, tok, n.SlotData[cfg.slotIndex("ldax@vm1#0")])
	}
	if got := n.fenceNUMANodes(n.Drv[VM1].F); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("vm1 fenced %v, want [2 3]", got)
	}
	// the observation: the stack in another order is adopted; any other
	// set of regions is not
	sm := newSim(cfg, 1)
	sm.truth = sm.resolve(n)
	st := &sm.truth.Stacks[stackIndex(VM1, HB0)]
	*st = Stack{}
	for _, k := range []RKey{rkLocal, devKey(sharedDev(PoolA)), devKey(pooledDev(PoolA))} {
		st.push(k)
	}
	o := sm.observe(int(n.Steps), 1)
	v, rs := checkState(n, o, "")
	wantVerdict(t, v, VOK, "ok")
	if rs.Stacks[stackIndex(VM1, HB0)].String() != "[local,a-shared0,a-pooled0]" || rs.Reorder != 0 {
		t.Fatalf("adopted stack %s, reorder %b", rs.Stacks[stackIndex(VM1, HB0)], rs.Reorder)
	}
	if rec := adoptionRecord(n, 0, rs); !strings.Contains(rec, "stack=vm1/hb0:local,a-shared0,a-pooled0") || !strings.Contains(rec, "restarts=vm1:") {
		t.Fatalf("adoption record %q", rec)
	}
	// generate --from replays the record
	if ad, err := applyAdoption(n, adoptionRecord(n, 0, rs)); err != nil || ad.Reorder != 0 || ad.Stacks[stackIndex(VM1, HB0)].String() != "[local,a-shared0,a-pooled0]" {
		t.Fatalf("applyAdoption: %v\n%s", err, ad.dump())
	}
	if n.openChoice() == "" {
		t.Fatalf("no open choice in a settle with a rebuilt stack")
	}
	st.N = 2
	st.E[2] = 0
	v, _ = checkState(n, sm.observe(int(n.Steps), 1), "")
	wantVerdict(t, v, VWait, "in any order (rebuilt by a node-reboot)")
	// the driver pod: +1 (or +2, adopted)
	o = observation(t, n)
	o.Drivers["vm1"].Restarts += 2
	v, _ = checkState(n, o, "")
	wantVerdict(t, v, VWait, "driver vm1")
	o = observation(t, n)
	o.Drivers["vm1"].Restarts++
	if v, rs = checkState(n, o, ""); v.Status != VOK || rs.Drv[VM1].Restarts != n.Drv[VM1].Restarts+1 || rs.Drv[VM1].Slack != 0 {
		t.Fatalf("driver restarts +2: %s %q, adopted %d+%d", v.Status, v.Msg, rs.Drv[VM1].Restarts, rs.Drv[VM1].Slack)
	}
	// a read of the shared device: the token or zeros; a read decides
	r := mustAct(t, cfg, "read sdax-a@vm2#0")
	e := expectDone(n, n.apply(r), r, int(n.Steps)+1)
	for _, c := range []struct {
		text   string
		status VStatus
	}{{tok.Text, VOK}, {"", VOK}, {"junk", VFail}} {
		txt := c.text
		if v := checkDone(e, &DoneObs{Text: &txt}); v.Status != c.status {
			t.Fatalf("read %q: %s %q, want %s", c.text, v.Status, v.Msg, c.status)
		}
	}
	zero := ""
	if ad := adoptDone(n.apply(r), r, &DoneObs{Text: &zero}); ad.Data[sharedDev(PoolA)] != tokZero {
		t.Fatalf("read zeros: data %s", ad.Data[sharedDev(PoolA)])
	}
	if w := steps(t, n, "write sdax-a@vm2#0"); w.Data[sharedDev(PoolA)].OrZero {
		t.Fatalf("a write leaves the token open")
	}
	// the expectation keeps it all (off-line check)
	js, _ := json.Marshal(n.expect(int(n.Steps), "settle"))
	ex := &Expectation{}
	if err := json.Unmarshal(js, ex); err != nil {
		t.Fatal(err)
	}
	es, err := ex.state()
	if err != nil {
		t.Fatal(err)
	}
	if es.Reorder != n.Reorder || es.Data[sharedDev(PoolA)] != n.Data[sharedDev(PoolA)] || !es.Drv[VM1].Rebooted || es.Drv[VM1].Reboots != 1 {
		t.Fatalf("expectation: reorder %b data %s driver %+v", es.Reorder, es.Data[sharedDev(PoolA)], es.Drv[VM1])
	}
	wantVerdict(t, checkSettle(ex, observation(t, n)), VOK, "ok")
	// C2: the pods of the VM with kind and phase
	fc := faultContext(before, mustAct(t, cfg, "node-reboot vm1"))
	for _, want := range []string{"fault node-reboot xnuma running", "fault node-reboot lsteer pending", "fault node-reboot plain running"} {
		if !slices.Contains(fc, want) {
			t.Fatalf("C2 of the reboot: %q, want %q", fc, want)
		}
	}
	if slices.Contains(fc, "fault node-reboot xdax running") {
		t.Fatalf("C2 of the reboot has a pod of vm2: %q", fc)
	}
}

// Leases at a reboot: released, the companion gone, the waiter of the
// block promoted; the companion's claim makes no region after the boot
// (its Prepare fails before it touches a device, A42).
func TestNodeRebootLeases(t *testing.T) {
	cfg := rebootConfig(t, "full", true)
	s := steps(t, newState(cfg), "create lease@vm1#0", "create lnuma@vm1#0", "settle", "lease-get lease@vm1#0 dax", "settle",
		"create ldax@vm1#0", "settle")
	wantPhase(t, s, "ldax@vm1#0", Pending)
	n := steps(t, s, "node-reboot vm1")
	if ss := slotOf(t, n, "lease@vm1#0"); ss.Lease != LeaseReleasing || ss.Phase != Restarting {
		t.Fatalf("lease slot after the reboot: %+v", *ss)
	}
	wantPhase(t, n, "ldax@vm1#0", Starting)
	n = steps(t, n, "settle")
	if ss := slotOf(t, n, "lease@vm1#0"); ss.Lease != LeaseNone || ss.Phase != Running || ss.Restarts != 1 {
		t.Fatalf("lease slot after the settle: %+v", *ss)
	}
	wantPhase(t, n, "ldax@vm1#0", Running)
	if slotOf(t, n, "ldax@vm1#0").Restarts != 0 {
		t.Fatalf("the promoted waiter restarted")
	}
	o := observation(t, n)
	if len(o.Companions) != 0 {
		t.Fatalf("companions %v", o.Companions)
	}
	v, _ := checkState(n, o, "")
	wantVerdict(t, v, VOK, "ok")
	// the old companion still there: a wait
	o.Companions = []ObsCompanion{{Name: "cxl-lease-l0001", Node: "vm1", Phase: "Running", Lease: "L0001", Deleting: true}}
	v, _ = checkState(n, o, "")
	wantVerdict(t, v, VWait, "no lease of the model")
	// a lease-get works again (when a block is free)
	steps(t, n, "!lease-get lease@vm1#0 numa", "delete ldax@vm1#0", "settle", "lease-get lease@vm1#0 numa")
	// the companion's slice was the only local region: none after the
	// boot
	s = steps(t, newState(cfg), "create lease@vm1#0", "create xnuma-a@vm1#0", "settle", "lease-get lease@vm1#0 numa", "settle")
	wantStack(t, s, stackIndex(VM1, HB0), "[a-pooled0,local]")
	n = steps(t, s, "node-reboot vm1", "settle")
	wantStack(t, n, stackIndex(VM1, HB0), "[a-pooled0]")
	if got := n.fenceNUMANodes(n.Drv[VM1].F); n.Reorder != 0 || !slices.Equal(got, []int{2}) {
		t.Fatalf("after the reboot:\n%s", n.dump())
	}
	v, _ = checkState(n, observation(t, n), "")
	wantVerdict(t, v, VOK, "ok")
}

// dnuma at a reboot: both claims prepared again, both nodes fenced; off-line
// a reboot that leaves two regions on a stack of the VM is not generated.
func TestNodeRebootDual(t *testing.T) {
	cfg := rebootConfig(t, "full", true)
	s := steps(t, newState(cfg), "create dnuma@vm1#0", "create sdax-a@vm1#0", "settle")
	n := steps(t, s, "node-reboot vm1", "settle")
	wantPhase(t, n, "dnuma@vm1#0", Running)
	if slotOf(t, n, "dnuma@vm1#0").Restarts != 1 || n.Reorder != 1<<uint(stackIndex(VM1, HB0)) {
		t.Fatalf("dnuma after the reboot:\n%s", n.dump())
	}
	wantStack(t, n, stackIndex(VM1, HB1), "[c-pooled0]")
	if got := n.fenceNUMANodes(n.Drv[VM1].F); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("vm1 fenced %v", got)
	}
	v, _ := checkState(n, observation(t, n), "")
	wantVerdict(t, v, VOK, "ok")
	off := rebootConfig(t, "full", false)
	steps(t, newState(off), "create dnuma@vm1#0", "settle", "create sdax-a@vm1#0", "settle", "!node-reboot vm1")
	steps(t, newState(off), "create dnuma@vm1#0", "settle", "node-reboot vm1", "settle")
}

// Fencing modes at a reboot: none fences nothing; the loose containers of
// a switch to none are created again: exact (D + M).
func TestNodeRebootFencing(t *testing.T) {
	cfg := rebootConfig(t, "full", false)
	cfg.Fencing = FencingSwitch
	s := steps(t, newState(cfg), "create plain@vm1#0", "create lnuma@vm1#0", "settle", "fencing-switch", "settle")
	if !slotOf(t, s, "plain@vm1#0").Loose || s.Fence != FenceNone {
		t.Fatalf("not loose after the switch:\n%s", s.dump())
	}
	checkMemsCases(t, s, []memsCase{
		{"vm1", "mbt-plain", "0-1", VOK, "ok"},
		{"vm1", "mbt-plain", "0-2", VOK, "ok"},
	})
	n := steps(t, s, "node-reboot vm1", "settle")
	if slotOf(t, n, "plain@vm1#0").Loose || slotOf(t, n, "lnuma@vm1#0").Loose || n.Drv[VM1].F != 0 || n.Drv[VM1].FMaybe != 0 {
		t.Fatalf("after the reboot in fencing none:\n%s", n.dump())
	}
	checkMemsCases(t, n, []memsCase{
		{"vm1", "mbt-plain", "0-2", VOK, "ok"},
		{"vm1", "mbt-plain", "0-1", VWait, "lacks CXL NUMA node 2 with memory"},
	})
	// and allowed-mems: fenced again from the claims of the new boot
	n = steps(t, n, "fencing-switch", "settle", "node-reboot vm1", "settle")
	if got := n.fenceNUMANodes(n.Drv[VM1].F); !slices.Equal(got, []int{2}) {
		t.Fatalf("vm1 fenced %v, want [2]", got)
	}
	checkMemsCases(t, n, []memsCase{
		{"vm1", "mbt-plain", "0-1", VOK, "ok"},
		{"vm1", "mbt-plain", "0-2", VWait, "has fenced CXL node 2"},
	})
}

// Persistent devices at a reboot: the content survives, a refused claim
// stays refused, the steps after read what was written before.
func TestNodeRebootPersistent(t *testing.T) {
	cfg := rebootConfig(t, "fullp", true)
	s := steps(t, newState(cfg), "create pwr-c@vm0#0", "create xpb-c@vm1#0", "settle", "pwrite pwr-c@vm0#0 img", "pwrite xpb-c@vm1#0 img",
		"delete pwr-c@vm0#0", "settle", "create plain@vm2#0", "settle", "create pbad-c@vm0#0", "create pfsd-c@vm1#0", "create pbro-c@vm1#0", "settle")
	wantPhase(t, s, "pbad-c@vm0#0", Refused)
	wantPhase(t, s, "pfsd-c@vm1#0", Running)
	pd := s.PDev
	n := steps(t, s, "node-reboot vm0", "settle", "node-reboot vm1", "settle")
	for _, name := range []string{"pfsd-c@vm1#0", "pbro-c@vm1#0", "xpb-c@vm1#0"} {
		wantPhase(t, n, name, Running)
	}
	wantPhase(t, n, "pbad-c@vm0#0", Refused)
	if n.PDev != pd || slotOf(t, n, "pbad-c@vm0#0").Restarts != 0 || slotOf(t, n, "pfsd-c@vm1#0").Restarts != 1 {
		t.Fatalf("persistent content or the refused claim changed:\n%s", n.dump())
	}
	for _, c := range []struct{ slot, want string }{{"pfsd-c@vm1#0", pd[0].Tok.Text}, {"pbro-c@vm1#0", pd[0].Raw.Text}, {"xpb-c@vm1#0", pd[1].Raw.Text}} {
		pr := mustAct(t, cfg, "pread "+c.slot)
		if e := expectDone(n, n.apply(pr), pr, int(n.Steps)+1); e.Done.Text == nil || *e.Done.Text != c.want || c.want == "" {
			t.Fatalf("pread %s after the reboot: %+v, want %q", c.slot, e.Done, c.want)
		}
	}
	if n.Reorder != 1<<uint(stackIndex(VM1, HB1)) {
		t.Fatalf("reorder %b, want vm1/hb1 (c-pmem0, c-pmem1)", n.Reorder)
	}
	v, _ := checkState(n, observation(t, n), "")
	wantVerdict(t, v, VOK, "ok")
}

// The boot ids (A44): a new one after a node-reboot of the VM, never
// otherwise; node-reboot's done names the new one.
func TestNodeRebootBootID(t *testing.T) {
	cfg := rebootConfig(t, "full", true)
	s := steps(t, newState(cfg), "create plain@vm1#0", "settle")
	v, rs := checkState(s, observation(t, s), "")
	wantVerdict(t, v, VOK, "ok")
	if rs.Drv[VM1].Boot != "boot-vm1-0" {
		t.Fatalf("boot id not learnt: %+v", rs.Drv[VM1])
	}
	s = rs
	// no reboot: the same boot
	o := observation(t, s)
	o.VMs["vm1"].BootID = "boot-vm1-9"
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VFail, "the VM booted without a node-reboot")
	// the done of node-reboot
	a := mustAct(t, cfg, "node-reboot vm1")
	n := s.apply(a)
	e := expectDone(s, n, a, int(n.Steps))
	if e.Done.BootNot != "boot-vm1-0" || n.Drv[VM1].BootBefore != "boot-vm1-0" || n.Drv[VM1].Boot != "" {
		t.Fatalf("done %+v, driver %+v", e.Done, n.Drv[VM1])
	}
	old, nw := "boot-vm1-0", "boot-vm1-1"
	wantVerdict(t, checkDone(e, &DoneObs{Boot: &old}), VFail, "as before")
	wantVerdict(t, checkDone(e, &DoneObs{Boot: &nw}), VOK, "ok")
	wantVerdict(t, checkDone(e, &DoneObs{Status: 1}), VFail, "exit status 1")
	// without the boot in the done: the settle wants another one
	p := n.apply(Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool})
	o = observation(t, p)
	o.VMs["vm1"].BootID = "boot-vm1-0"
	v, _ = checkState(p, o, "")
	wantVerdict(t, v, VFail, "as before its node-reboot")
	o.VMs["vm1"].BootID = "boot-vm1-7"
	o.VMs["vm1"].State.BootID = "boot-vm1-7"
	v, rs = checkState(p, o, "")
	wantVerdict(t, v, VOK, "ok")
	if rs.Drv[VM1].Boot != "boot-vm1-7" || rs.Drv[VM1].BootBefore != "" {
		t.Fatalf("new boot id not learnt: %+v", rs.Drv[VM1])
	}
	// with it: the settle wants exactly it
	n = adoptDone(n, a, &DoneObs{Boot: &nw})
	p = n.apply(Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool})
	o = observation(t, p)
	v, _ = checkState(p, o, "")
	wantVerdict(t, v, VOK, "ok")
	o.VMs["vm1"].BootID = "boot-vm1-8"
	v, _ = checkState(p, o, "")
	wantVerdict(t, v, VFail, "want boot-vm1-1")
	// not observed: a note
	o = observation(t, p)
	o.VMs["vm1"].BootID = ""
	v, _ = checkState(p, o, "")
	if v.Status != VOK || !slices.ContainsFunc(v.Notes, func(x string) bool { return strings.Contains(x, "bootID not observed") }) {
		t.Fatalf("no boot id: %s %q, notes %q", v.Status, v.Msg, v.Notes)
	}
}

// The device nodes of the containers (A44): the current device of each
// claim, nothing stale; also without a reboot.
func TestDevNodes(t *testing.T) {
	cfg := rebootConfig(t, "fullp", true)
	s := steps(t, newState(cfg), "create xdax-a@vm1#0", "create ldax@vm1#0", "create sro-a@vm1#0", "create pwr-c@vm0#0",
		"create plain@vm1#0", "create lnuma@vm1#0", "settle")
	o := observation(t, s)
	v, _ := checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	type nodes = []ObsDevNode
	nd := func(path, dev string) ObsDevNode { return simNode(path, "c", dev) }
	set := func(o *Observation, vm, pod string, ns nodes) { ctrOf(t, o, vm, pod).DevNodes = rawJSON(ns) }
	get := func(o *Observation, vm, pod string) nodes {
		var ns nodes
		if err := json.Unmarshal(ctrOf(t, o, vm, pod).DevNodes, &ns); err != nil {
			t.Fatal(err)
		}
		return ns
	}
	dax := get(o, "vm1", "mbt-xdax-a")
	ldax := get(o, "vm1", "mbt-ldax")
	if len(dax) != 1 || len(ldax) != 1 || len(get(o, "vm0", "mbt-pwr-c")) != 1 || len(get(o, "vm1", "mbt-plain")) != 0 {
		t.Fatalf("device nodes of the simulated SUT: %v %v", dax, ldax)
	}
	for _, c := range []struct {
		name   string
		change func(o *Observation)
		status VStatus
		want   string
	}{
		{"a node of another boot", func(o *Observation) { set(o, "vm1", "mbt-xdax-a", nodes{nd("/dev/dax5.0", "252:4")}) }, VWait, "is no CXL device of the VM (stale"},
		{"another device's numbers", func(o *Observation) { set(o, "vm1", "mbt-xdax-a", nodes{nd(dax[0].Path, "252:1")}) }, VWait, "the VM's"},
		{"another claim's device", func(o *Observation) { set(o, "vm1", "mbt-xdax-a", ldax) }, VWait, "not its claim's"},
		{"no node", func(o *Observation) { set(o, "vm1", "mbt-ldax", nodes{}) }, VWait, "0 device nodes of its claim"},
		{"a node in a plain container", func(o *Observation) { set(o, "vm1", "mbt-plain", dax) }, VWait, "not its claim's"},
		{"a block device of another boot", func(o *Observation) { set(o, "vm0", "mbt-pwr-c", nodes{nd("/dev/pmem0", "259:7")}) }, VWait, "the VM's pmem0 is"},
		{"not read", func(o *Observation) { ctrOf(t, o, "vm1", "mbt-ldax").DevNodes = json.RawMessage("null") }, VWait, "devices not read"},
		{"devices not read", func(o *Observation) { o.VMs["vm1"].CXLDevs = json.RawMessage("null") }, VWait, "cxlDevices not read"},
		{"not observed", func(o *Observation) {
			o.VMs["vm1"].CXLDevs = nil
			ctrOf(t, o, "vm0", "mbt-pwr-c").DevNodes = nil
		}, VOK, "ok"},
		// the env of an inspection (fatal: not repeated)
		{"env of an old boot", func(o *Observation) { o.Inspect["xdax-a@vm1#0"].Env["CXL_C1EE0001_DAX"] = "/dev/dax5.0" }, VFail, "is no dax device of 0xc1ee0001 on vm1 now (stale"},
		{"env with another claim's block device", func(o *Observation) { o.Inspect["pwr-c@vm0#0"].Env["CXL_C1AE2002_PATH"] = "/dev/pmem1" }, VFail, "is no pmem device"},
		{"a numa node without memory", func(o *Observation) {
			o.Inspect["lnuma@vm1#0"].Env["CXL_C100E2E1_NUMA_NODE"] = "3"
		}, VFail, "CXL_C100E2E1_NUMA_NODE=3, want 2"},
	} {
		o := observation(t, s)
		c.change(o)
		v, _ := checkState(s, o, "")
		if v.Status != c.status || !strings.Contains(v.Msg, c.want) {
			t.Errorf("%s: verdict %s %q, want %s with %q; diffs:\n  %s", c.name, v.Status, v.Msg, c.status, c.want, strings.Join(v.Diffs, "\n  "))
		}
	}
	// a NUMA node whose memory is gone: a wait (O3's MemTotal says it too)
	o = observation(t, s)
	o.VMs["vm1"].Nodes["2"] = ObsNUMANode{}
	v, _ = checkState(s, o, "")
	if v.Status != VWait || !slices.ContainsFunc(v.Diffs, func(x string) bool { return strings.Contains(x, "CXL_C100E2E1_NUMA_NODE=2 has no memory on vm1") }) {
		t.Fatalf("node without memory: %s %q; diffs:\n  %s", v.Status, v.Msg, strings.Join(v.Diffs, "\n  "))
	}
	// after a reboot the devices have new names: the old nodes are stale
	n := steps(t, s, "node-reboot vm1", "settle")
	o = observation(t, n)
	if nd := get(o, "vm1", "mbt-xdax-a"); nd[0] == dax[0] {
		t.Fatalf("the simulated SUT kept the device of the old boot")
	}
	v, _ = checkState(n, o, "")
	wantVerdict(t, v, VOK, "ok")
	set(o, "vm1", "mbt-xdax-a", dax)
	v, _ = checkState(n, o, "")
	wantVerdict(t, v, VWait, "stale")
}

// AttachFailed of a Running pod's claim: a wait in the settle of a
// node-reboot (the attach into the new qemu fails until the pool server
// discovers it, A44), a fail otherwise.
func TestNodeRebootAttachFailed(t *testing.T) {
	cfg := rebootConfig(t, "full", true)
	cfg.Reboot = RebootQemu
	s := steps(t, newState(cfg), "create xdax-a@vm1#0", "create dnuma@vm0#0", "create lnuma@vm1#0", "settle")
	yes, no := true, false
	o := observation(t, s)
	// the simulated SUT says it as the observer does: false for an
	// allocated pool claim (also of "claims"), null for a local one
	for _, x := range []struct {
		slot string
		c    *ObsClaim
		want string
	}{{"xdax-a@vm1#0", podOf(t, o, "xdax-a@vm1#0").Claim, "false"}, {"lnuma@vm1#0", podOf(t, o, "lnuma@vm1#0").Claim, "null"},
		{"dnuma@vm0#0 c0", &podOf(t, o, "dnuma@vm0#0").Claims[0], "null"}, {"dnuma@vm0#0 c1", &podOf(t, o, "dnuma@vm0#0").Claims[1], "false"}} {
		if js, _ := json.Marshal(x.c.AttachFailed); string(js) != x.want {
			t.Fatalf("%s: attachFailed %s, want %s", x.slot, js, x.want)
		}
	}
	wantVerdict(t, mustCheck(t, s, o), VOK, "ok")
	podOf(t, o, "dnuma@vm0#0").Claims[1].AttachFailed = &yes
	wantVerdict(t, mustCheck(t, s, o), VFail, "dnuma@vm0#0: claim AttachFailed, want Attached")
	o = observation(t, s)
	c := podOf(t, o, "xdax-a@vm1#0").Claim
	c.AttachFailed, c.Attached = &yes, &no
	v, _ := checkState(s, o, "")
	wantVerdict(t, v, VFail, "claim AttachFailed, want Attached")
	n := steps(t, s, "node-reboot vm1", "settle")
	o = observation(t, n)
	c = podOf(t, o, "xdax-a@vm1#0").Claim
	c.AttachFailed, c.Attached = &yes, &no
	v, _ = checkState(n, o, "")
	wantVerdict(t, v, VWait, "claim not Attached")
	if !slices.ContainsFunc(v.Diffs, func(x string) bool { return strings.Contains(x, "AttachFailed (after the node-reboot of vm1)") }) {
		t.Fatalf("diffs %q", v.Diffs)
	}
	// the next window: not any more
	n = steps(t, n, "create plain@vm0#0", "settle")
	o = observation(t, n)
	podOf(t, o, "xdax-a@vm1#0").Claim.AttachFailed = &yes
	v, _ = checkState(n, o, "")
	wantVerdict(t, v, VFail, "claim AttachFailed")
}

// mustCheck is checkState's verdict.
func mustCheck(t *testing.T, s *State, o *Observation) Verdict {
	t.Helper()
	v, _ := checkState(s, o, "")
	return v
}

// The independent pool reference with node-reboots in the fuzz
// in the fuzz: guest and qemu.
func TestRebootFuzz(t *testing.T) {
	seqs := *refSeqs / 16
	if testing.Short() {
		seqs = 15
	}
	for _, mode := range []RebootRun{RebootGuest, RebootQemu} {
		kinds, st := runRefFuzz(t, fuzzMode{profile: "full", reboot: mode}, seqs, *refSeed+2000, *refMax)
		t.Logf("reboot %s: %d sequences (%d failed), %d steps, %d settles; verbs %v", mode, seqs, seqs-st.seqs, st.steps, st.settles, st.verbs)
		if st.verbs[VNodeReboot] == 0 {
			t.Errorf("reboot %s: no node-reboot in the fuzz", mode)
		}
		for k, fs := range kinds {
			slices.SortFunc(fs, func(a, b *fuzzFailure) int { return len(a.steps) - len(b.steps) })
			t.Errorf("reboot %s: %d x %s\nshortest:\n%s", mode, len(fs), k, fs[0])
		}
	}
}

// The reference with a qemu restart: the attachments of the VM are
// dropped and attached again, the model's prediction is the same.
func TestRefPoolQemuRestart(t *testing.T) {
	for _, profile := range []string{"full", "fullp"} {
		cfg := testConfig(t, profile, true)
		cfg.Unsettled = 3
		cfg.Reboot = RebootQemu
		lines := []string{"create xnuma-a@vm1#0", "create sdax-a@vm1#0", "create sdax-a@vm2#0", "settle", "create xnuma-c@vm1#0", "settle",
			"create lease@vm1#0", "settle", "lease-get lease@vm1#0 dax", "settle", "node-reboot vm1", "settle", "node-reboot vm0", "settle"}
		if profile == "fullp" {
			lines = append(lines, "create plse@vm1#0", "settle", "lease-get plse@vm1#0 pbw", "settle", "create pfsd-c@vm2#0", "settle", "node-reboot vm1", "settle")
		}
		if f := refSequence(cfg, allActs(cfg), 3, 0, lines, &fuzzStats{verbs: map[Verb]int{}, ref: map[string]int{}}); f != nil {
			t.Fatalf("%s: %s", profile, f)
		}
	}
}

// On-line through the protocol: node-reboot's done carries the new boot
// id, the settle after it adopts the order of the rebuilt stacks, and
// generate --from follows the run with its adoption records.
func TestOnlineReboot(t *testing.T) {
	o := testOpts(t, "full", 0, 2, true)
	o.cfg.Reboot = RebootGuest
	o.header = "--profile full --reboot=guest"
	o.replay = []string{"settle", "create xnuma-a@vm1#0", "create sdax-a@vm1#0", "create lnuma@vm1#0", "settle",
		"create plain@vm1#0", "create sdax-a@vm2#0", "settle", "write sdax-a@vm2#0", "node-reboot vm1", "read sdax-a@vm2#0", "settle",
		"read sdax-a@vm1#0", "node-reboot vm0", "settle", "node-reboot vm1", "settle"}
	sm := newSim(o.cfg, 5)
	sm.transient = 2
	logs, err := runSim(t, o, sm)
	if err != nil || sm.sawEnd == "" {
		t.Fatalf("run: %v, sim fail %q\n%s", err, sm.sawFail, logs)
	}
	data, err := os.ReadFile(filepath.Join(o.outDir, "mbt-steps.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\nnode-reboot vm") || !strings.Contains(string(data), "stack=vm1/hb0:") || !strings.Contains(string(data), "data=a-shared0:") {
		t.Fatalf("no node-reboot or no --reboot in the steps file:\n%s", data)
	}
	lines, header := readSteps(string(data))
	if !slices.Contains(header, "--reboot=guest") {
		t.Fatalf("header %q", header)
	}
	g := genOpts(t, "full", 0, 1)
	g.cfg.Online = true
	g.cfg.Reboot = RebootGuest
	if err := g.cfg.loadProfile("full"); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := generate(g, lines, &b); err != nil {
		t.Fatalf("generate --from: %v", err)
	}
	if !strings.Contains(b.String(), " node-reboot vm") {
		t.Fatalf("generate --from lost the reboot")
	}
}
