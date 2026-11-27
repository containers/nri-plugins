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

// Tests of the persistent devices (A22-A27): the model's rules, the
// oracle's invariants P1-P11, the generation limits, determinism.

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pdevOf(s *State, d DevID) *PDev { return &s.PDev[pmemIndex(d)] }

// doneOf runs a non-settle action of a state against the simulated SUT
// of its truth and checks the done observation.
func doneOf(t *testing.T, s *State, line string) (*State, *DoneObs) {
	t.Helper()
	a := mustAct(t, s.cfg, line)
	if !s.enabled(a) {
		t.Fatalf("%q not enabled in\n%s", line, s.dump())
	}
	n := s.apply(a)
	sm := newSim(s.cfg, 1)
	sm.truth = s
	d := sm.do(int(n.Steps), a)
	if v := checkDone(expectDone(s, n, a, int(n.Steps)), d); v.Status != VOK {
		t.Fatalf("%s: %s", line, v.Msg)
	}
	return adoptDone(n, a, d), d
}

// The writer comes first: its Prepare makes the namespace of a fresh
// device, pwrite img puts the erofs and the token there; a reader after
// it mounts it and reads the token, on another VM too (one pair of
// backing files).
func TestPmemWriterFirst(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pwr-c@vm0#0", "settle")
	wantPhase(t, s, "pwr-c@vm0#0", Running)
	if pd := pdevOf(s, cPmem0); pd.NS != TriYes || pd.FS != TriNo || pd.Raw != tokZero {
		t.Fatalf("after the writer's Prepare: %+v", *pd)
	}
	wantStack(t, s, stackIndex(VM0, HB1), "[c-pmem0]")
	s, d := doneOf(t, s, "pwrite pwr-c@vm0#0 img")
	tok := "mbt-" + strconv.Itoa(int(s.Steps))
	if d.Status != 0 || d.Text == nil || *d.Text != tok {
		t.Fatalf("pwrite: %+v", d)
	}
	if pd := pdevOf(s, cPmem0); pd.FS != TriYes || pd.Tok.Text != tok || pd.Raw.Text != tok {
		t.Fatalf("after pwrite: %+v", *pd)
	}
	// a reader cannot have the device while the writer holds it
	s = steps(t, s, "create pfsd-c@vm2#0", "settle")
	wantPhase(t, s, "pfsd-c@vm2#0", Pending)
	s = steps(t, s, "delete pwr-c@vm0#0", "settle")
	wantPhase(t, s, "pfsd-c@vm2#0", Running)
	wantStack(t, s, stackIndex(VM2, HB1), "[c-pmem0]")
	wantStack(t, s, stackIndex(VM0, HB1), "[]")
	s, d = doneOf(t, s, "pread pfsd-c@vm2#0")
	if d.Text == nil || *d.Text != tok {
		t.Fatalf("pread: %+v", d)
	}
	// a read-only claim refuses writes and changes nothing (P11)
	n, d := doneOf(t, s, "pwrite pfsd-c@vm2#0 img")
	if d.Status == 0 || n.PDev != s.PDev {
		t.Fatalf("pwrite on a reader: status %d, %+v", d.Status, n.PDev)
	}
	// pfsc on the same VM: a second node mount (cached), the same region
	s = steps(t, s, "create pfsc-c@vm2#0", "settle")
	wantPhase(t, s, "pfsc-c@vm2#0", Running)
	wantStack(t, s, stackIndex(VM2, HB1), "[c-pmem0]")
}

// A reader before any writer is refused (no namespace), keeps its share
// and with it every writer out: Refused until it is deleted, then the
// writer gets the device. pbad is refused by its config, without a
// region.
func TestPmemReaderBeforeWriter(t *testing.T) {
	s := stateAfter(t, "fullp", true, "create pfsd-c@vm1#0", "create pbad-c@vm0#0", "settle")
	wantPhase(t, s, "pfsd-c@vm1#0", Refused)
	wantPhase(t, s, "pbad-c@vm0#0", Refused)
	wantStack(t, s, stackIndex(VM1, HB1), "[]")
	s = steps(t, s, "create pwr-c@vm2#0", "settle", "kubelet-restart vm1", "driver-kill vm1", "settle")
	wantPhase(t, s, "pwr-c@vm2#0", Pending)
	wantPhase(t, s, "pfsd-c@vm1#0", Refused)
	if pdevOf(s, cPmem0).NS != TriNo {
		t.Fatalf("the namespace came without a writer")
	}
	// the oracle: refused ok; Running fails; no event yet waits
	o := observation(t, s)
	v, _ := checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	o = observation(t, s)
	podOf(t, o, "pfsd-c@vm1#0").Events, podOf(t, o, "pfsd-c@vm1#0").Event = nil, nil
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VWait, `no Warning event with "no fsdax namespace"`)
	o = observation(t, s)
	podOf(t, o, "pbad-c@vm0#0").Phase = "Running"
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VFail, "Running, but its Prepare must be refused (writable only exclusively)")
	s = steps(t, s, "delete pfsd-c@vm1#0", "delete pbad-c@vm0#0", "settle")
	wantPhase(t, s, "pwr-c@vm2#0", Running)
	if pdevOf(s, cPmem0).NS != TriYes {
		t.Fatalf("no namespace after the writer")
	}
	// a pbro reader needs only the namespace; an fs reader the erofs
	// (a device released by the last window: one candidate per window,
	// F8)
	s = steps(t, s, "delete pwr-c@vm2#0", "create pbro-c@vm1#0", "settle", "create pfsd-c@vm2#0", "settle")
	wantPhase(t, s, "pbro-c@vm1#0", Running)
	wantPhase(t, s, "pfsd-c@vm2#0", Refused)
}

// An fs lease whose file is held stays Releasing after lease-put
// (LeaseBusy: its bind, node mount and region stay, pread works) until
// unhold; a crash of the requester ends the holder too.
func TestPmemLeaseBusy(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pwr-c@vm2#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm2#0 img")
	s = steps(t, s, "delete pwr-c@vm2#0", "create plse@vm2#0", "settle", "kubelet-restart vm0", "settle")
	s, d := doneOf(t, s, "lease-get plse@vm2#0 pfd")
	if d.Lease == nil {
		t.Fatalf("no lease")
	}
	steps(t, s, "!lease-get plse@vm2#0 pbr") // one persistent lease per slot
	s = steps(t, s, "settle")
	wantStack(t, s, stackIndex(VM2, HB1), "[c-pmem0]")
	s, _ = doneOf(t, s, "hold plse@vm2#0")
	s, d = doneOf(t, s, "lease-put plse@vm2#0")
	if d.Status == 0 || s.Slots[s.cfg.slotIndex("plse@vm2#0")].Lease != LeaseBusy {
		t.Fatalf("lease-put of a held lease: status %d", d.Status)
	}
	s = steps(t, s, "settle", "kubelet-restart vm2", "driver-kill vm2", "settle")
	if ss := slotOf(t, s, "plse@vm2#0"); ss.Lease != LeaseBusy || !ss.Hold {
		t.Fatalf("busy lease after faults: %+v", ss)
	}
	wantStack(t, s, stackIndex(VM2, HB1), "[c-pmem0]")
	v, _ := checkState(s, observation(t, s), "")
	wantVerdict(t, v, VOK, "ok")
	s, d = doneOf(t, s, "pread plse@vm2#0")
	if d.Text == nil || *d.Text == "" {
		t.Fatalf("pread of a busy lease: %+v", d)
	}
	// no other share of the device meanwhile: a writer waits
	s = steps(t, s, "create pwr-c@vm0#0", "settle")
	wantPhase(t, s, "pwr-c@vm0#0", Pending)
	n := steps(t, s, "unhold plse@vm2#0", "settle")
	if ss := slotOf(t, n, "plse@vm2#0"); ss.Lease != LeaseNone {
		t.Fatalf("lease after unhold: %+v", ss)
	}
	wantPhase(t, n, "pwr-c@vm0#0", Running)
	wantStack(t, n, stackIndex(VM2, HB1), "[]")
	// a crash of the requester ends the holder: released
	c := steps(t, s, "crash plse@vm2#0", "settle")
	if ss := slotOf(t, c, "plse@vm2#0"); ss.Lease != LeaseNone || ss.Hold {
		t.Fatalf("lease after the crash: %+v", ss)
	}
}

// The token stays across VMs, crashes and every fault; pwipe makes the
// device fresh again (only without attachments).
func TestPmemTokensAcrossFaults(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pwr-c@vm0#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm0#0 img")
	want := *pdevOf(s, cPmem0)
	steps(t, s, "!pwipe c c-pmem0")
	s = steps(t, s, "delete pwr-c@vm0#0", "settle", "kubelet-restart vm0", "settle", "create pfsd-c@vm1#0", "create pfsc-c@vm2#0", "create pbro-c@vm0#0", "settle",
		"crash pfsd-c@vm1#0", "driver-kill vm2", "containerd-restart vm0", "settle", "driver-restart vm1", "ctrl-kill c", "server-restart c", "settle",
		"kubelet-restart vm1", "ctrl-start c", "ctrl-restart c", "settle")
	for _, sl := range []string{"pfsd-c@vm1#0", "pfsc-c@vm2#0", "pbro-c@vm0#0"} {
		wantPhase(t, s, sl, Running)
		_, d := doneOf(t, s, "pread "+sl)
		if d.Text == nil || *d.Text != want.Tok.Text {
			t.Fatalf("pread %s: %v, want %s", sl, d.Text, want.Tok.Text)
		}
	}
	if pd := pdevOf(s, cPmem0); pd.NS != want.NS || pd.FS != want.FS || pd.Tok != want.Tok || pd.Raw != want.Raw {
		t.Fatalf("content after faults %+v, want %+v", *pd, want)
	}
	// (the claims' deallocation may lag the settle that saw their pods
	// gone, and with it the detach: one more window, F8)
	s = steps(t, s, "delete pfsd-c@vm1#0", "delete pfsc-c@vm2#0", "delete pbro-c@vm0#0", "!pwipe c c-pmem0", "settle", "!pwipe c c-pmem0",
		"kubelet-restart vm0", "settle", "pwipe c c-pmem0")
	if pd := pdevOf(s, cPmem0); *pd != freshPDev {
		t.Fatalf("after pwipe: %+v", *pd)
	}
	// junk: no erofs any more, readers refused
	s = steps(t, s, "create pwr-c@vm2#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm2#0 junk")
	s = steps(t, s, "delete pwr-c@vm2#0", "create pfsd-c@vm2#0", "settle")
	wantPhase(t, s, "pfsd-c@vm2#0", Refused)
}

// Persistent regions are regions of the hb1 stack (A9): a hole blocks a
// persistent Prepare, also a refused one's (blocked first, then refused
// when the hole clears).
func TestPmemDecoderHoles(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create xnuma-c@vm1#0", "settle", "create pfsd-c@vm1#0", "settle")
	wantStack(t, s, stackIndex(VM1, HB1), "[c-pooled0]")
	wantPhase(t, s, "pfsd-c@vm1#0", Refused)
	// a writer first: a region on top
	w := stateAfter(t, "pmem", true, "create xnuma-c@vm1#0", "create sdax-c@vm0#0", "settle", "create pwr-c@vm2#0", "settle")
	wantStack(t, w, stackIndex(VM2, HB1), "[c-pmem0]")
	// a hole below: c-pooled0 under a reader's c-pmem0 on vm1
	h := stateAfter(t, "pmem", true, "create pwr-c@vm0#0", "settle")
	h, _ = doneOf(t, h, "pwrite pwr-c@vm0#0 img")
	h = steps(t, h, "delete pwr-c@vm0#0", "create xnuma-c@vm1#0", "settle", "create pfsd-c@vm1#0", "settle")
	wantStack(t, h, stackIndex(VM1, HB1), "[c-pooled0,c-pmem0]")
	h = steps(t, h, "delete xnuma-c@vm1#0", "settle")
	wantStack(t, h, stackIndex(VM1, HB1), "[hole,c-pmem0]")
	// another reader of the device on vm1 shares the region: no block
	h = steps(t, h, "create pfsc-c@vm1#0", "settle")
	wantPhase(t, h, "pfsc-c@vm1#0", Running)
	// a refused-to-be reader through a hole is blocked first
	r := stateAfter(t, "fullp", true, "create xnuma-c@vm1#0", "settle", "create sdax-c@vm1#0", "settle", "delete xnuma-c@vm1#0", "settle")
	wantStack(t, r, stackIndex(VM1, HB1), "[hole,c-shared0]")
	r = steps(t, r, "create pfsd-c@vm1#0", "settle")
	wantPhase(t, r, "pfsd-c@vm1#0", Blocked)
	r = steps(t, r, "delete sdax-c@vm1#0", "settle")
	wantPhase(t, r, "pfsd-c@vm1#0", Refused)
	wantStack(t, r, stackIndex(VM1, HB1), "[]")
	// the transient region of a refused claim's retry on top: accepted
	o := observation(t, r)
	for j := range o.VMs["vm1"].Ports {
		p := &o.VMs["vm1"].Ports[j]
		if p.HB != nil && *p.HB == "hb1" {
			p.Decoders = append(p.Decoders, ObsDecoder{Name: "decoder3.0", Region: "region16"})
			p.Committed++
		}
	}
	o.VMs["vm1"].Regions = append(o.VMs["vm1"].Regions, ObsRegion{Name: "region16", Serials: []string{formatSerial(devices[cPmem0].Serial)}, Mode: "pmem"})
	v, _ := checkState(r, o, "")
	wantVerdict(t, v, VOK, "ok")
}

// The oracle's invariants of A26 against the simulated observation of a
// state with every persistent part: ok, and one deviation per check.
func TestPmemOracleInvariants(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pwr-c@vm2#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm2#0 img")
	s = steps(t, s, "delete pwr-c@vm2#0", "create plse@vm1#0", "settle", "kubelet-restart vm0", "settle", "create pfsd-c@vm1#0", "create pfsc-c@vm1#0", "create pbro-c@vm0#0", "settle")
	s, _ = doneOf(t, s, "lease-get plse@vm1#0 pfc")
	s = steps(t, s, "settle")
	base := observation(t, s)
	v, adopted := checkState(s, base, "")
	wantVerdict(t, v, VOK, "ok")
	if pd := pdevOf(adopted, cPmem0); pd.UUID == "" || pd.LSAHash == "" || pd.HeadHash == "" {
		t.Fatalf("not adopted: %+v", *pd)
	}
	s = adopted
	vmPart := func(o *Observation, vm string, field *json.RawMessage, f func(x any) any, into any) {
		if err := json.Unmarshal(*field, into); err != nil {
			t.Fatal(err)
		}
		*field = rawJSON(f(into))
	}
	cases := []struct {
		name   string
		change func(o *Observation)
		status VStatus
		msg    string
	}{
		{"P1 stale node mount", func(o *Observation) {
			var m []ObsMount
			vmPart(o, "vm0", &o.VMs["vm0"].FSMounts, func(any) any {
				return append(m, ObsMount{Target: simPoolMounts + "/c1ae2002-direct", Source: "/dev/pmem0", Fstype: "erofs", Superopts: "ro,dax=always", Vfsopts: "ro,nosuid,nodev"})
			}, &m)
		}, VWait, "stale node mount"},
		{"P1 node mount gone", func(o *Observation) { o.VMs["vm1"].FSMounts = json.RawMessage("[]") }, VWait, "no node mount c1ae2002-direct"},
		{"P1 direct without dax", func(o *Observation) {
			var m []ObsMount
			vmPart(o, "vm1", &o.VMs["vm1"].FSMounts, func(any) any {
				for j := range m {
					m[j].Superopts = "ro"
				}
				return m
			}, &m)
		}, VWait, "want /dev/pmem* with dax=always"},
		{"P2 bind left", func(o *Observation) {
			var m []ObsMount
			vmPart(o, "vm1", &o.VMs["vm1"].LeaseBinds, func(any) any {
				return append(m, ObsMount{Target: simLeaseFS + "/x/old", Source: "/dev/loop0", Fstype: "erofs", Vfsopts: "ro,nosuid,nodev"})
			}, &m)
		}, VWait, "bind left"},
		{"P3 loop left", func(o *Observation) {
			back := "/dev/pmem0"
			o.VMs["vm0"].Loops = rawJSON([]ObsLoop{{Name: "loop7", Backing: &back}})
		}, VWait, "loop devices over pmem"},
		{"P4 leaked region", func(o *Observation) {
			sz := uint64(256 * mi)
			o.VMs["vm2"].Pmem = rawJSON([]ObsPmemRegion{{Region: "region9", Serials: []string{formatSerial(devices[cPmem0].Serial)}, Size: &sz}})
		}, VWait, "no claim of it is prepared here (leaked region?)"},
		{"P5 UUID changed", func(o *Observation) {
			var r []ObsPmemRegion
			vmPart(o, "vm1", &o.VMs["vm1"].Pmem, func(any) any {
				u := "other"
				r[0].Namespaces[0].UUID = &u
				return r
			}, &r)
		}, VFail, "namespace lost"},
		{"P5 labels changed", func(o *Observation) {
			var m map[string]*ObsPDev
			_ = json.Unmarshal(o.PDevs, &m)
			m["c-pmem0"].LSAHash = "beef"
			o.PDevs = rawJSON(m)
		}, VFail, "the LSA changed"},
		{"P6 token changed", func(o *Observation) {
			var m map[string]*ObsPDev
			_ = json.Unmarshal(o.PDevs, &m)
			x := "mbt-0"
			m["c-pmem0"].Token = &x
			o.PDevs = rawJSON(m)
		}, VFail, "data changed"},
		{"P6 erofs gone", func(o *Observation) {
			var m map[string]*ObsPDev
			_ = json.Unmarshal(o.PDevs, &m)
			m["c-pmem0"].Erofs = false
			o.PDevs = rawJSON(m)
		}, VWait, "erofs false, want true"},
		{"P7 kernel", func(o *Observation) {
			o.VMs["vm1"].Kernel = rawJSON(ObsKernel{Lines: []string{"BUG: unable to handle page fault"}, Bad: 1})
		}, VFail, "kernel: 1 new BUG/Oops/WARNING"},
		{"P8 orphan lease dir", func(o *Observation) {
			o.VMs["vm0"].LeaseDirs = rawJSON(ObsLeaseDirs{Leases: []string{"gone-uid"}})
		}, VWait, "leases/gone-uid of no prepared lease claim"},
		{"P10 CDI of a block reader rw", func(o *Observation) {
			var m map[string]ObsCDIDevice
			vmPart(o, "vm0", &o.VMs["vm0"].CDIPool, func(any) any {
				for k, d := range m {
					rw := "rw"
					d.DeviceNodes[0].Permissions = &rw
					m[k] = d
				}
				return m
			}, &m)
		}, VWait, "permissions rw"},
		{"null part", func(o *Observation) { o.VMs["vm1"].Pmem = json.RawMessage("null") }, VWait, "pmem not observed (null)"},
		{"absent part: not checked", func(o *Observation) { o.VMs["vm1"].FSMounts = nil }, VOK, "ok"},
	}
	for _, c := range cases {
		o := observation(t, s)
		c.change(o)
		js, _ := json.Marshal(o)
		o2, _ := parseObservation(js)
		v, _ := checkState(s, o2, "")
		if v.Status != c.status || !strings.Contains(v.Msg+strings.Join(v.Diffs, "|"), c.msg) {
			t.Errorf("%s: verdict %s %q, want %s with %q; diffs %v", c.name, v.Status, v.Msg, c.status, c.msg, v.Diffs)
		}
	}
	// P10 inspect: an fs container with /dev/pmem*, a wrong env
	for _, slot := range []string{"pfsd-c@vm1#0", "pbro-c@vm0#0"} {
		sm := newSim(s.cfg, 1)
		sm.truth = s
		o := sm.observe(int(s.Steps), 1)
		in := o.Inspect[slot]
		if in == nil {
			t.Fatalf("no inspection of %s", slot)
		}
		if slot == "pfsd-c@vm1#0" {
			in.PmemNodes = []string{"/dev/pmem0"}
		} else {
			in.Env["CXL_C1AE2002_READ_ONLY"] = "0"
		}
		v, _ := checkState(s, o, "")
		wantVerdict(t, v, VFail, slot)
	}
}

// The generation limits of A27 and the model's own.
func TestPmemGenerationLimits(t *testing.T) {
	off := stateAfter(t, "pmem", false)
	steps(t, off, "!driver-kill-at vm0 region", "!hold pfsd-c@vm1#0", "!pwrite pwr-c@vm0#0 img")
	on := stateAfter(t, "pmem", true)
	steps(t, on, "driver-kill-at vm0 region", "!driver-kill-at vm0 ns", "pwipe c c-pmem0")
	// pwipe not while a claim may allocate or an attachment is left
	steps(t, on, "create pfsd-c@vm1#0", "!pwipe c c-pmem0")
	k := steps(t, on, "ctrl-kill c", "create pwr-c@vm0#0", "settle", "ctrl-start c", "settle", "delete pwr-c@vm0#0", "settle")
	steps(t, k, "!pwipe c c-pmem0", "kubelet-restart vm0", "settle", "pwipe c c-pmem0")
	// hold only on a Running fs kind (and not twice)
	w := stateAfter(t, "pmem", true, "create pwr-c@vm0#0", "settle")
	steps(t, w, "!hold pwr-c@vm0#0", "pwrite pwr-c@vm0#0 junk", "!pwrite pbro-c@vm0#0 img")
	// a pbro whose Prepare's outcome the window leaves open is not made
	n := stateAfter(t, "pmem", true, "create pwr-c@vm0#0", "create pbro-c@vm0#0")
	wantPhase(t, n, "pbro-c@vm0#0", Pending)
	steps(t, n, "!delete pwr-c@vm0#0")
	// no lease-get under an armed watcher of the VM, nor with a reader
	// refused by the content
	l := stateAfter(t, "pmem", true, "create plse@vm1#0", "settle")
	steps(t, l, "lease-get plse@vm1#0 pbw", "!lease-get plse@vm1#0 pfd", "!lease-get plse@vm1#0 pbr")
	steps(t, l, "driver-kill-at vm1 region", "!lease-get plse@vm1#0 pbw")
	// off-line: no Refused next to a pool-c region change on its VM
	r := stateAfter(t, "pmem", false, "create pfsd-c@vm1#0", "settle")
	wantPhase(t, r, "pfsd-c@vm1#0", Refused)
	steps(t, r, "!create xnuma-c@vm1#0", "create sdax-c@vm0#0")
	// MBT_AVOID names the new verbs
	cfg := testConfig(t, "pmem", true)
	cfg.Avoid = []string{"pwipe", "driver-kill-at", "hold"}
	for _, a := range allActs(cfg) {
		if a.Verb == VPWipe || a.Verb == VDriverKillAt || a.Verb == VHold {
			t.Fatalf("%s not avoided", a.name(cfg))
		}
	}
	if err := checkAvoid(cfg.Avoid); err != nil {
		t.Fatal(err)
	}
}

// driver-kill-at: the settle of a window with a Prepare at the marker
// has the outcome with the kill (restarts +1) and the one without; the
// observation decides; an armed watcher that did not fire is disarmed
// 70 s after the settle that first saw it.
func TestPmemDriverKillAt(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pwr-c@vm0#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm0#0 img")
	s = steps(t, s, "delete pwr-c@vm0#0", "settle", "driver-kill-at vm1 mount", "create pfsd-c@vm1#0", "settle")
	if len(s.Alts) != 1 || s.Drv[VM1].KillAt == 0 || s.Alts[0].Drv[VM1].Restarts != 1 || s.Alts[0].Drv[VM1].KillAt != 0 {
		t.Fatalf("outcomes:\n%s", dumpAlts(s))
	}
	now := time.Unix(1800000000, 0)
	v, adopted, alt := checkAltsSince(s, observation(t, s.Alts[0]), "", now, now, defaultUnblock)
	if v.Status != VOK || alt != 1 {
		t.Fatalf("the kill: %s %s alt %d", v.Status, v.Msg, alt)
	}
	wantPhase(t, adopted, "pfsd-c@vm1#0", Running)
	if rec := adoptionRecord(s, alt, adopted); !strings.Contains(rec, "alt=1") {
		t.Fatalf("record %q", rec)
	}
	// not fired: still armed, disarmed later
	notFired := s.clone()
	v, armed, _ := checkAltsSince(s, observation(t, notFired), "", now, now, defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	if armed.Drv[VM1].KillAt == 0 || armed.Drv[VM1].KillAtAt != now.Unix() {
		t.Fatalf("not armed: %+v", armed.Drv[VM1])
	}
	later := steps(t, armed, "crash pfsd-c@vm1#0", "settle")
	v, disarmed := checkStateAt(later, observation(t, later), "", now.Add(80*time.Second), defaultUnblock)
	wantVerdict(t, v, VOK, "ok")
	if disarmed.Drv[VM1].KillAt != 0 || !strings.Contains(adoptionRecord(later, 0, disarmed), "killat=vm1:off") {
		t.Fatalf("not disarmed: %+v", disarmed.Drv[VM1])
	}
	// no Prepare at the marker in the window: one outcome
	n := steps(t, stateAfter(t, "pmem", true), "driver-kill-at vm2 ns", "create pfsd-c@vm1#0", "settle")
	if len(n.Alts) != 0 {
		t.Fatalf("a kill without its marker:\n%s", dumpAlts(n))
	}
}

// A writer deleted (or its controller killed) while it may have made the
// namespace leaves it open; the oracle adopts it from pdevs.
func TestPmemNamespaceAdopted(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pwr-c@vm0#0", "delete pwr-c@vm0#0", "settle")
	if pd := pdevOf(s, cPmem0); pd.NS != TriMaybe {
		t.Fatalf("ns %v", pd.NS)
	}
	steps(t, s, "!create pbro-c@vm0#0", "create pfsd-c@vm1#0")
	for _, ns := range []Tri{TriYes, TriNo} {
		truth := s.clone()
		pdevOf(truth, cPmem0).NS = ns
		o := observation(t, truth)
		v, adopted := checkState(s, o, "")
		wantVerdict(t, v, VOK, "ok")
		if pdevOf(adopted, cPmem0).NS != ns || !strings.Contains(adoptionRecord(s, 0, adopted), "pns=c-pmem0:"+triName(ns)) {
			t.Fatalf("adopted %v", pdevOf(adopted, cPmem0).NS)
		}
	}
}

// Expectations round-trip the persistent state (generate, check).
func TestPmemExpectationRoundTrip(t *testing.T) {
	s := stateAfter(t, "fullp", true, "create pwr-c@vm0#0", "create plse@vm1#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm0#0 img")
	s = steps(t, s, "delete pwr-c@vm0#0", "settle", "create pfsd-c@vm2#0", "settle")
	s, _ = doneOf(t, s, "lease-get plse@vm1#0 pfd")
	s, _ = doneOf(t, s, "hold plse@vm1#0")
	s, _ = doneOf(t, s, "lease-put plse@vm1#0")
	s = steps(t, s, "driver-kill-at vm2 region", "create pbad-c@vm0#0", "settle")
	e := s.expect(int(s.Steps), "settle")
	back, err := e.state()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e, back.expect(int(s.Steps), "settle")) {
		a, _ := json.Marshal(e)
		b, _ := json.Marshal(back.expect(int(s.Steps), "settle"))
		t.Fatalf("round trip:\n%s\n%s", a, b)
	}
}

// Random walks of every enabled action, the simulated SUT answering from
// the model: every settle and done checked.
func TestPmemWalk(t *testing.T) {
	n := 20
	if testing.Short() {
		n = 4
	}
	for _, profile := range []string{"pmem", "fullp"} {
		for seed := int64(1); seed <= int64(n); seed++ {
			if msg, lines := pmemWalk(t, profile, true, seed, 200); msg != "" {
				t.Fatalf("%s seed %d: %s\nsteps:\n  %s", profile, seed, msg, strings.Join(lines, "\n  "))
			}
		}
	}
}

// pmemWalk runs a random walk of enabled actions of a profile against
// the simulated SUT (answering from the model), every settle and done
// checked by the oracle; it returns the first failure.
func pmemWalk(t *testing.T, profile string, online bool, seed int64, steps int) (string, []string) {
	t.Helper()
	cfg := testConfig(t, profile, online)
	cfg.Unsettled = 3
	acts := allActs(cfg)
	rng := rand.New(rand.NewSource(seed))
	sm := newSim(cfg, seed)
	st := newState(cfg)
	now := time.Unix(1800000000, 0)
	var lines []string
	settle := Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool}
	for n := 1; n <= steps || st.Unsettled > 0; n++ {
		var a Act
		var next *State
		if n > steps {
			a, next = settle, st.next(settle)
		} else {
			var cand []Act
			var nexts []*State
			for _, x := range acts {
				if p := st.next(x); p != nil {
					cand, nexts = append(cand, x), append(nexts, p)
				}
			}
			j := rng.Intn(len(cand))
			a, next = cand[j], nexts[j]
		}
		lines = append(lines, a.name(cfg))
		now = now.Add(5 * time.Second)
		if a.Verb == VSettle {
			sm.do(n, a)
			o := sm.observe(n, 1)
			v, adopted := checkStateAt(next, o, "", now, defaultUnblock)
			if v.Status != VOK {
				return v.Msg + "\n  " + strings.Join(v.Diffs, "\n  ") + "\nmodel:\n" + st.dump() + "\ntruth:\n" + sm.truth.dump(), lines
			}
			st = adopted
			continue
		}
		d := sm.do(n, a)
		if v := checkDone(expectDone(st, next, a, n), d); v.Status != VOK {
			return v.Msg, lines
		}
		st = adoptDone(next, a, d)
	}
	return "", lines
}

// The on-line loop over the protocol with the persistent profiles, the
// teardown included, against the simulated SUT and against the
// reference of the pool side.
func TestPmemOnline(t *testing.T) {
	for _, c := range []struct {
		profile string
		ref     bool
	}{{"pmem", false}, {"pmem", true}, {"fullp", true}} {
		o := testOpts(t, c.profile, size(80, 12), 2, true)
		o.seed = 21
		sm := newSim(o.cfg, 21)
		if c.ref {
			sm = sm.withRef(121)
			o.now = sm.refWall
		}
		logs, err := runSim(t, o, sm)
		if err != nil || sm.sawEnd == "" {
			t.Fatalf("%s (ref %v): %v, sim end %q fail %q\n%s", c.profile, c.ref, err, sm.sawEnd, sm.sawFail, logs)
		}
	}
}

// C2 sees faults in the persistent phases and states: refused claims,
// busy and held fs leases, held mounts, the device's content, the
// markers of driver-kill-at; C4 the device's content and its users.
func TestPmemCoverageStrings(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pwr-c@vm2#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm2#0 img")
	s = steps(t, s, "delete pwr-c@vm2#0", "create plse@vm1#0", "create pbro-c@vm0#0", "settle", "kubelet-restart vm0", "settle")
	s, _ = doneOf(t, s, "lease-get plse@vm1#0 pfd")
	s = steps(t, s, "settle", "create pfsd-c@vm1#0", "settle")
	s, _ = doneOf(t, s, "hold plse@vm1#0")
	s, _ = doneOf(t, s, "hold pfsd-c@vm1#0")
	s, _ = doneOf(t, s, "lease-put plse@vm1#0")
	got := strings.Join(faultContext(s, mustAct(t, s.cfg, "driver-kill-at vm1 mount")), "|")
	for _, want := range []string{"fault driver-kill-at:mount plse-pfd-busy-held running", "fault driver-kill-at:mount pfsd-held running",
		"fault driver-kill-at:mount c-pmem0 ns-yes fs-yes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("no %q in %s", want, got)
		}
	}
	r := stateAfter(t, "fullp", true, "create pbad-c@vm0#0", "settle")
	if got := strings.Join(faultContext(r, mustAct(t, r.cfg, "ctrl-kill c")), "|"); !strings.Contains(got, "fault ctrl-kill pbad refused") {
		t.Fatalf("no refused context in %s", got)
	}
	if got := strings.Join(c1Strings(s.cfg, mustAct(t, s.cfg, "driver-kill-at vm0 ns"), 3), "|"); got != "C1.fault driver-kill-at:ns" {
		t.Fatalf("C1 %s", got)
	}
	if a := strings.Join(s.abstract(), "; "); !strings.Contains(a, "dev c-pmem0 lease-pfd.busy.hold,pbro.run,pfsd.run.hold ns-yes fs-yes") {
		t.Fatalf("C4 %s", a)
	}
}

// A29 d, A30 (A29 d): a lease-get of an fs lease while driver-kill-at bind
// is armed on its VM returns 0 with the lease Ready, or nonzero (EOF)
// with the lease released or still Lending until kubelet's retry makes
// it Ready; all are accepted and adopted, with and without the kill.
// "bind" is generated.
func TestPmemKillAtBind(t *testing.T) {
	found := false
	for _, a := range allActs(testConfig(t, "pmem", true)) {
		found = found || (a.Verb == VDriverKillAt && a.argName() == "bind")
	}
	if !found {
		t.Fatalf("driver-kill-at bind not generated")
	}
	s := stateAfter(t, "pmem", true, "create pwr-c@vm2#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm2#0 img")
	s = steps(t, s, "delete pwr-c@vm2#0", "create plse@vm1#0", "settle", "kubelet-restart vm0", "settle", "driver-kill-at vm1 bind")
	steps(t, s, "lease-get plse@vm1#0 pfd")
	steps(t, steps(t, stateAfter(t, "pmem", true, "create plse@vm1#0", "settle"), "driver-kill-at vm1 region"), "!lease-get plse@vm1#0 pbw")
	a := mustAct(t, s.cfg, "lease-get plse@vm1#0 pfd")
	n := s.apply(a)
	e := expectDone(s, n, a, int(n.Steps))
	if e.Done.Status != "either" {
		t.Fatalf("expectation %+v", e.Done)
	}
	id := "L1"
	for _, c := range []struct {
		d    DoneObs
		open bool
		rec  string
		alts int // kept/released x not fired/fired
	}{{DoneObs{Status: 0, Lease: &id}, false, "lease=plse@vm1#0:L1", 2}, {DoneObs{Status: 1}, true, "lopen=plse@vm1#0:failed", 4}} {
		if v := checkDone(e, &c.d); v.Status != VOK {
			t.Fatalf("done %+v: %s", c.d, v.Msg)
		}
		got := adoptDone(n, a, &c.d)
		ss := got.Slots[got.cfg.slotIndex("plse@vm1#0")]
		if ss.Lease != LeaseHeld || ss.LeaseOpen != c.open || !strings.Contains(adoptionRecord(n, 0, got), c.rec) {
			t.Fatalf("adopted %+v, record %q", ss, adoptionRecord(n, 0, got))
		}
		back, err := applyAdoption(n, adoptionRecord(n, 0, got))
		if err != nil || back.Slots[back.cfg.slotIndex("plse@vm1#0")].LeaseOpen != c.open {
			t.Fatalf("record not applied: %v", err)
		}
		if c.open {
			// nothing on a lease still Lending (A30)
			steps(t, got, "!pread plse@vm1#0", "!hold plse@vm1#0", "!lease-put plse@vm1#0")
		}
		st := steps(t, got, "settle")
		if len(alts(st)) != c.alts {
			t.Fatalf("outcomes:\n%s", dumpAlts(st))
		}
		for _, w := range alts(st) {
			truth := w.clone()
			v, adopted := checkState(st, observation(t, truth), "")
			wantVerdict(t, v, VOK, "ok")
			lease := slotOf(t, adopted, "plse@vm1#0").Lease
			if lease != slotOf(t, w, "plse@vm1#0").Lease || adopted.Drv[VM1].Restarts != w.Drv[VM1].Restarts {
				t.Fatalf("adopted lease %v restarts %d, want those of %s", lease, adopted.Drv[VM1].Restarts, w.dump())
			}
			if lease == LeaseNone {
				wantStack(t, adopted, stackIndex(VM1, HB1), "[]")
				// a bind left behind (A29 d) is seen
				o := observation(t, truth)
				o.VMs["vm1"].LeaseBinds = rawJSON([]ObsMount{{Target: simLeaseFS + "/u/mbt", Source: "/dev/pmem0", Fstype: "erofs", Vfsopts: "ro,nosuid,nodev"}})
				v, _ = checkState(st, o, "")
				wantVerdict(t, v, VWait, "bind left")
			}
			if lease == LeaseHeld && c.open {
				// kept, before kubelet's retry (A33): Lending, no
				// mount in the requester's container yet
				o := observation(t, truth)
				in := o.Inspect["plse@vm1#0"]
				in.Mounts = []ObsMount{}
				in.Leases = rawJSON([]ObsLease{{Lease: "L1", State: "Lending", Mode: "fs"}})
				v, _ = checkState(st, o, "")
				wantVerdict(t, v, VWait, "its lease L1 is Lending")
				in.Leases = rawJSON([]ObsLease{{Lease: "L1", State: "Ready", Mode: "fs"}})
				v, _ = checkState(st, o, "")
				wantVerdict(t, v, VFail, "an fs lease it cannot see")
				// kubelet's retry made it Ready and the requester sees
				// its mount, but the companion is not Running yet: a
				// wait, not a deviation of the inspection (FP5V)
				o = observation(t, truth)
				for j := range o.Companions {
					if o.Companions[j].Node == "vm1" {
						o.Companions[j].Phase = "Pending"
						o.Companions[j].Containers = []ObsPodContainer{{Name: "pause", State: "waiting"}}
					}
				}
				v, _ = checkState(st, o, "")
				wantVerdict(t, v, VWait, "of plse@vm1#0: phase Pending")
				for _, d := range v.Diffs {
					if strings.HasPrefix(d, "inspect ") {
						t.Fatalf("a deviation of the inspection: %s", d)
					}
				}
			}
		}
	}
}

// A stalled reader whose Prepare is refused, promoted through a hole on
// its stack, is blocked first (fuzz, fullp seed 181).
func TestPmemStalledRefusedThroughHole(t *testing.T) {
	s := stateAfter(t, "fullp", true, "create xnuma-c@vm1#0", "settle", "create sdax-c@vm1#0", "settle", "delete xnuma-c@vm1#0", "settle",
		"create pfsc-c@vm2#0", "delete pfsc-c@vm2#0", "settle", "create pfsd-c@vm1#0", "settle")
	wantStack(t, s, stackIndex(VM1, HB1), "[hole,c-shared0]")
	wantPhase(t, s, "pfsd-c@vm1#0", Stalled)
	truth := s.clone()
	truth.Slots[truth.cfg.slotIndex("pfsd-c@vm1#0")].Phase = Blocked
	v, adopted := checkState(s, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "pfsd-c@vm1#0", Blocked)
	truth.Slots[truth.cfg.slotIndex("pfsd-c@vm1#0")].Phase = Refused
	v, _ = checkState(s, observation(t, truth), "")
	if v.Status == VOK {
		t.Fatalf("refused through a hole accepted")
	}
}

// A30: the watcher's state on the observe line: done without firing
// disarms it at once; fired wants the outcome with the kill.
func TestPmemKillAtState(t *testing.T) {
	s := steps(t, stateAfter(t, "pmem", true), "driver-kill-at vm2 ns", "create pfsd-c@vm1#0", "settle")
	o := observation(t, s)
	o.VMs["vm2"].KillAt = json.RawMessage(`{"armed":true,"fired":false,"done":true,"expired":true}`)
	v, adopted := checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	if adopted.Drv[VM2].KillAt != 0 || !strings.Contains(adoptionRecord(s, 0, adopted), "killat=vm2:off") {
		t.Fatalf("not disarmed: %+v", adopted.Drv[VM2])
	}
	o = observation(t, s)
	o.VMs["vm2"].KillAt = json.RawMessage(`{"armed":true,"fired":false,"done":false}`)
	if _, adopted = checkState(s, o, ""); adopted.Drv[VM2].KillAt == 0 {
		t.Fatalf("disarmed while not done")
	}
	o = observation(t, s)
	o.VMs["vm2"].KillAt = json.RawMessage(`{"armed":true,"fired":true,"done":true}`)
	v, _ = checkState(s, o, "")
	wantVerdict(t, v, VWait, "driver-kill-at fired, want the outcome with the kill")
}

// A31 (driver-kill-at vm2 region, driver-restart vm2, ...): a
// driver-kill, driver-restart or containerd-restart disarms the watcher
// of its VM (done, fired false, "disarmed by VERB"); kubelet-restart
// does not. A fired report must account for its restart: a refused
// claim's retries make the region at any time, so with one on the VM the
// settle has the outcome with the kill, and a driver fault that would
// disarm a watcher that may have fired is not generated.
func TestPmemKillAtDisarm(t *testing.T) {
	for _, verb := range []string{"driver-restart vm2", "driver-kill vm2", "containerd-restart vm2"} {
		s := steps(t, stateAfter(t, "pmem", true), "driver-kill-at vm2 region", verb)
		if s.Drv[VM2].KillAt != 0 {
			t.Fatalf("%s: still armed", verb)
		}
		s = steps(t, s, "settle", "kubelet-restart vm2", "create pfsd-c@vm2#0", "settle")
		if len(s.Alts) != 0 {
			t.Fatalf("%s: a kill after the disarm:\n%s", verb, dumpAlts(s))
		}
		o := observation(t, s)
		o.VMs["vm2"].KillAt = json.RawMessage(`{"armed":true,"fired":false,"done":true,"reason":"disarmed by ` + strings.Fields(verb)[0] + `"}`)
		v, _ := checkState(s, o, "")
		wantVerdict(t, v, VOK, "ok")
	}
	k := steps(t, stateAfter(t, "pmem", true), "driver-kill-at vm2 region", "kubelet-restart vm2")
	if k.Drv[VM2].KillAt == 0 {
		t.Fatalf("kubelet-restart disarmed")
	}
	// a refused reader on vm2, the watcher armed at region
	r := steps(t, stateAfter(t, "pmem", true, "create pfsd-c@vm2#0", "settle"), "driver-kill-at vm2 region")
	wantPhase(t, r, "pfsd-c@vm2#0", Refused)
	steps(t, r, "!driver-restart vm2", "!driver-kill vm2", "!containerd-restart vm2", "kubelet-restart vm2")
	r = steps(t, r, "create pfsc-c@vm1#0", "settle")
	if len(r.Alts) != 1 || r.Alts[0].Drv[VM2].Restarts != 1 {
		t.Fatalf("no outcome with the kill of a refused claim's retry:\n%s", dumpAlts(r))
	}
	o := observation(t, r.Alts[0])
	o.VMs["vm2"].KillAt = json.RawMessage(`{"armed":true,"fired":true,"done":true,"reason":"fired"}`)
	v, adopted := checkState(r, o, "")
	wantVerdict(t, v, VOK, "ok")
	if adopted.Drv[VM2].Restarts != 1 || adopted.Drv[VM2].KillAt != 0 {
		t.Fatalf("adopted %+v", adopted.Drv[VM2])
	}
}

// A busy fs lease's companion is Terminating, its pause container ended
// (the pod status says so): no running container of it need be observed
// (A11 goes by the containers' states); a running one that is not
// observed waits.
func TestPmemBusyLeaseCompanionTerminating(t *testing.T) {
	s := stateAfter(t, "pmem", true, "create pwr-c@vm2#0", "settle")
	s, _ = doneOf(t, s, "pwrite pwr-c@vm2#0 img")
	// the writer's claim may be deallocated a window after its pod (F8)
	s = steps(t, s, "delete pwr-c@vm2#0", "create plse@vm2#0", "settle", "kubelet-restart vm0", "settle")
	s, _ = doneOf(t, s, "lease-get plse@vm2#0 pfd")
	s = steps(t, s, "settle")
	s, _ = doneOf(t, s, "hold plse@vm2#0")
	s, _ = doneOf(t, s, "lease-put plse@vm2#0")
	pred := steps(t, s, "settle")
	if ss := slotOf(t, pred, "plse@vm2#0"); ss.Lease != LeaseBusy {
		t.Fatalf("plse@vm2#0 lease %v, want busy", ss.Lease)
	}
	o := observation(t, pred)
	companion := ""
	for j := range o.Companions {
		c := &o.Companions[j]
		if c.Node != "vm2" {
			continue
		}
		companion = c.Name
		c.Deleting = true
		c.Containers = []ObsPodContainer{{Name: "pause", State: "terminated"}}
	}
	if companion == "" {
		t.Fatalf("no companion of plse@vm2#0 in the observation")
	}
	ov := o.VMs["vm2"]
	kept := ov.Containers[:0]
	for _, c := range ov.Containers {
		if c.Pod != companion {
			kept = append(kept, c)
		}
	}
	ov.Containers = kept
	v, _ := checkState(pred, o, "")
	wantVerdict(t, v, VOK, "ok")
	for j := range o.Companions {
		for k := range o.Companions[j].Containers {
			o.Companions[j].Containers[k].State = "running"
		}
	}
	v, _ = checkState(pred, o, "")
	wantVerdict(t, v, VWait, "no container of companion "+companion+" observed")
}

// A Prepare that comes at any time may fire the watcher after the
// observation of the settle that saw it armed: the next window keeps
// that when its claim goes (A32: a refused claim; also a blocked one); a
// settle that sees it armed again starts afresh, and a volatile claim
// never marks it.
func TestPmemKillAtAfterSettle(t *testing.T) {
	// (c-pmem0 with a namespace, no erofs: pfsc-c is refused)
	r := stateAfter(t, "fullp", true, "create pwr-c@vm0#0", "settle", "delete pwr-c@vm0#0", "settle", "create pfsc-c@vm0#0", "settle")
	wantPhase(t, r, "pfsc-c@vm0#0", Refused)
	a := steps(t, r, "driver-kill-at vm0 ns", "settle")
	if a.Drv[VM0].KillAt != markerNS+1 || a.Drv[VM0].KillMay || !killAlt(a, VM0) {
		t.Fatalf("settled vm0 driver %+v:\n%s", a.Drv[VM0], dumpAlts(a))
	}
	s := steps(t, a, "create plain@vm0#0")
	if !s.Drv[VM0].KillMay {
		t.Fatalf("a step after the settle: vm0 driver %+v", s.Drv[VM0])
	}
	s = steps(t, s, "delete pfsc-c@vm0#0", "!containerd-restart vm0", "!driver-kill vm0", "!driver-restart vm0")
	if s.Drv[VM0].KillAt == 0 || !s.Drv[VM0].KillMay {
		t.Fatalf("after the delete: vm0 driver %+v", s.Drv[VM0])
	}
	// the delete is the window's first step: the retry may have come
	// before it
	d := steps(t, a, "delete pfsc-c@vm0#0", "!containerd-restart vm0")
	for _, x := range []*State{s, d} {
		k := steps(t, x, "settle")
		var kill *State
		for _, y := range alts(k) {
			if y.Drv[VM0].KillMay {
				t.Fatalf("settled with KillMay:\n%s", dumpAlts(k))
			}
			if y.Drv[VM0].KillAt == 0 && y.Drv[VM0].Restarts > k.Drv[VM0].Restarts {
				kill = y
			}
		}
		if kill == nil {
			t.Fatalf("no outcome with the kill:\n%s", dumpAlts(k))
		}
		o := observation(t, kill)
		o.VMs["vm0"].KillAt = json.RawMessage(`{"armed":true,"fired":true,"done":true,"reason":"fired"}`)
		v, adopted := checkState(k, o, "")
		wantVerdict(t, v, VOK, "ok")
		if dr := adopted.Drv[VM0]; dr.Restarts != 1 || dr.KillAt != 0 {
			t.Fatalf("adopted %+v", dr)
		}
		// not fired: armed, nothing left to fire it
		n := steps(t, k, "containerd-restart vm0")
		if n.Drv[VM0].KillAt != 0 || n.Drv[VM0].KillMay {
			t.Fatalf("containerd-restart: vm0 driver %+v", n.Drv[VM0])
		}
	}
	// a reader blocked through a hole (its retry makes the region),
	// deleted
	w := stateAfter(t, "fullp", true, "create pwr-c@vm0#0", "settle", "pwrite pwr-c@vm0#0 img", "delete pwr-c@vm0#0", "settle")
	h := steps(t, w, "create xnuma-c@vm1#0", "settle", "create sdax-c@vm1#0", "settle", "delete xnuma-c@vm1#0", "settle", "create pfsd-c@vm1#0", "settle")
	wantPhase(t, h, "pfsd-c@vm1#0", Blocked)
	h = steps(t, h, "driver-kill-at vm1 region", "settle")
	wantPhase(t, h, "pfsd-c@vm1#0", Blocked)
	b := steps(t, h, "delete pfsd-c@vm1#0", "!driver-kill vm1", "settle")
	if !killAlt(b, VM1) {
		t.Fatalf("blocked, deleted: no outcome with the kill:\n%s", dumpAlts(b))
	}
	// volatile claims never mark it
	v := steps(t, stateAfter(t, "fullp", true), "driver-kill-at vm1 region", "settle", "create xnuma-c@vm1#0", "delete xnuma-c@vm1#0")
	if v.Drv[VM1].KillMay {
		t.Fatalf("volatile: vm1 driver %+v", v.Drv[VM1])
	}
	steps(t, v, "driver-kill vm1")
}

// The driver pod's slack of a kubelet restart (A34): only while its
// container may be being created again, after a driver-kill of the
// window that no step waited out (kubelet-restart waits for the driver),
// or in the world in which an armed watcher that may have fired before
// the kubelet restart did; once per kill, on the VM's own driver; carried
// by the expectation, adopted and recorded at the settle.
func TestPmemDriverSlack(t *testing.T) {
	for _, c := range []struct {
		lines []string
		slack [nVMs]uint8
	}{
		{[]string{"driver-kill vm2", "kubelet-restart vm2"}, [nVMs]uint8{VM2: 1}},
		{[]string{"driver-kill vm2", "kubelet-restart vm2", "kubelet-restart vm2"}, [nVMs]uint8{VM2: 1}},
		// (the back-off of a second kill: created again later)
		{[]string{"driver-kill vm2", "create plain@vm2#0", "kubelet-restart vm2"}, [nVMs]uint8{VM2: 1}},
		{[]string{"driver-kill vm2", "driver-kill vm1", "kubelet-restart vm1"}, [nVMs]uint8{VM1: 1}},
		{[]string{"driver-kill vm2", "kubelet-restart vm1"}, [nVMs]uint8{}},
		{[]string{"kubelet-restart vm2", "driver-kill vm2"}, [nVMs]uint8{}},
		{[]string{"containerd-restart vm2", "kubelet-restart vm2"}, [nVMs]uint8{}},
		{[]string{"driver-restart vm2", "kubelet-restart vm2"}, [nVMs]uint8{}},
	} {
		s := stateAfter(t, "fullp", true, c.lines...)
		for v := VM(0); v < nVMs; v++ {
			if s.Drv[v].Slack != c.slack[v] {
				t.Fatalf("%v: %s driver %+v, want slack %d", c.lines, v, s.Drv[v], c.slack[v])
			}
		}
		k := steps(t, s, "settle")
		for v := VM(0); v < nVMs; v++ {
			if d := k.Drv[v]; d.Slack != c.slack[v] || d.Creating || d.KillSlack {
				t.Fatalf("%v: settled %s driver %+v", c.lines, v, d)
			}
		}
	}
	s := stateAfter(t, "fullp", true, "driver-kill vm2", "kubelet-restart vm2", "settle")
	for _, c := range []struct {
		restarts int
		status   VStatus
		msg      string
	}{{1, VOK, "ok"}, {2, VOK, "ok"}, {3, VWait, "has 3 restarts, want 1 (up to +1"}, {0, VWait, "has 0 restarts, want 1 (up to +1"}} {
		o := observation(t, s)
		o.Drivers["vm2"].Restarts = c.restarts
		v, adopted := checkState(s, o, "")
		wantVerdict(t, v, c.status, c.msg)
		if c.status == VOK {
			if d := adopted.Drv[VM2]; int(d.Restarts) != c.restarts || d.Slack != 0 {
				t.Fatalf("adopted %+v", d)
			}
			rec := adoptionRecord(s, 0, adopted)
			if !strings.Contains(" "+rec+" ", " restarts=vm2:"+strconv.Itoa(c.restarts)+"+0 ") {
				t.Fatalf("adoption record %q", rec)
			}
			// a replay applies the record: the same restarts, no slack
			re, err := applyAdoption(s, rec)
			if err != nil {
				t.Fatal(err)
			}
			if d := re.Drv[VM2]; int(d.Restarts) != c.restarts || d.Slack != 0 {
				t.Fatalf("%d restarts: replayed vm2 driver %+v", c.restarts, d)
			}
		}
	}
	// through the JSON expectation, as `check` does
	js, _ := json.Marshal(s.expect(int(s.Steps), "settle"))
	e := &Expectation{}
	if err := json.Unmarshal(js, e); err != nil {
		t.Fatal(err)
	}
	o := observation(t, s)
	o.Drivers["vm2"].Restarts = 2
	wantVerdict(t, checkSettle(e, o), VOK, "ok")
	// without the slack an extra restart still waits (A34 before)
	n := stateAfter(t, "fullp", true, "kubelet-restart vm2", "driver-kill vm2", "settle")
	o = observation(t, n)
	o.Drivers["vm2"].Restarts = 2
	v, _ := checkState(n, o, "")
	wantVerdict(t, v, VWait, "driver vm2: pod nri-dra-driver-cxl-vm2-0 has 2 restarts, want 1")
	if strings.Contains(v.Msg, "up to") {
		t.Fatalf("a slack note without slack: %q", v.Msg)
	}
	// a watcher that may have fired before the kubelet restart (A31: a
	// refused reader's retry makes its region): one more in the world
	// in which it fired, none in the other
	r := stateAfter(t, "fullp", true, "create pfsd-c@vm2#0", "settle", "driver-kill-at vm2 region", "kubelet-restart vm2")
	wantPhase(t, r, "pfsd-c@vm2#0", Refused)
	if d := r.Drv[VM2]; !d.KillSlack || d.Slack != 0 {
		t.Fatalf("vm2 driver %+v, want kubelet restarted after a possible kill", d)
	}
	r = steps(t, r, "settle")
	if len(r.Alts) != 1 || r.Drv[VM2].Restarts != 0 || r.Drv[VM2].Slack != 0 || r.Alts[0].Drv[VM2].Restarts != 1 || r.Alts[0].Drv[VM2].Slack != 1 {
		t.Fatalf("outcomes:\n%s", dumpAlts(r))
	}
	for _, c := range []struct {
		restarts int
		fired    bool
		status   VStatus
	}{{2, true, VOK}, {1, true, VOK}, {3, true, VWait}, {0, false, VOK}} {
		o := observation(t, r)
		o.Drivers["vm2"].Restarts = c.restarts
		o.VMs["vm2"].KillAt = json.RawMessage(`{"armed":true,"fired":` + strconv.FormatBool(c.fired) + `,"done":` + strconv.FormatBool(c.fired) + `}`)
		v, adopted := checkState(r, o, "")
		if v.Status != c.status {
			t.Fatalf("%d restarts, fired %v: verdict %s %q, want %s", c.restarts, c.fired, v.Status, v.Msg, c.status)
		}
		if c.status == VOK && int(adopted.Drv[VM2].Restarts) != c.restarts {
			t.Fatalf("%d restarts, fired %v: adopted %+v", c.restarts, c.fired, adopted.Drv[VM2])
		}
	}
	// a kubelet restart before any Prepare that could fire it: none
	q := stateAfter(t, "fullp", true, "driver-kill-at vm2 region", "kubelet-restart vm2", "create pfsd-c@vm2#0", "settle")
	for _, a := range alts(q) {
		if a.Drv[VM2].Slack != 0 {
			t.Fatalf("slack of a kill after the kubelet restart:\n%s", dumpAlts(q))
		}
	}
	if !killAlt(q, VM2) {
		t.Fatalf("no outcome with the kill:\n%s", dumpAlts(q))
	}
}

// killAlt: an outcome of the settle s has the kill of the watcher armed
// on v (fired: disarmed, the driver restarted).
func killAlt(s *State, v VM) bool {
	for _, a := range alts(s) {
		if a.Drv[v].KillAt == 0 && a.Drv[v].Restarts > s.Drv[v].Restarts {
			return true
		}
	}
	return false
}

// A32 and what else prepares where the window does not tell: a
// persistent Prepare may show the watcher's marker on its VM also when
// it is an "any" pod's (on the host it lands on), a stalled contender's
// promotion, an unblocking pod's retry, a blocked pod's retry (region:
// its create-region makes it before the hole refuses it) or a refused
// claim's retry (region, ns from the device's labels). ns is any
// namespace with a size in a new region (a reader's from the labels
// too); a region gone and back in the window is new to the watcher;
// volatile kinds show no marker (ram regions).
func TestPmemKillAtUnplacedPrepares(t *testing.T) {
	written := []string{"create pwr-c@vm0#0", "settle", "pwrite pwr-c@vm0#0 img", "delete pwr-c@vm0#0", "settle"}
	w := stateAfter(t, "fullp", true, written...)
	if pd := w.PDev[pmemIndex(cPmem0)]; pd.NS != TriYes || pd.FS != TriYes {
		t.Fatalf("c-pmem0 ns %v fs %v", pd.NS, pd.FS)
	}
	// (c-pmem0 with a namespace, no erofs)
	r := stateAfter(t, "fullp", true, "create pwr-c@vm0#0", "settle", "delete pwr-c@vm0#0", "settle")
	if pd := r.PDev[pmemIndex(cPmem0)]; pd.NS != TriYes || pd.FS != TriNo {
		t.Fatalf("c-pmem0 ns %v fs %v", pd.NS, pd.FS)
	}
	// (vm1/hb1 [hole,c-shared0], a reader blocked through the hole)
	h := steps(t, w, "create xnuma-c@vm1#0", "settle", "create sdax-c@vm1#0", "settle", "delete xnuma-c@vm1#0", "settle", "create pfsd-c@vm1#0", "settle")
	wantStack(t, h, stackIndex(VM1, HB1), "[hole,c-shared0]")
	wantPhase(t, h, "pfsd-c@vm1#0", Blocked)
	t.Run("any", func(t *testing.T) {
		// the kill only where the reader lands
		s := steps(t, w, "driver-kill-at vm1 mount", "create pfsd-c@any#0", "!driver-kill vm1", "!containerd-restart vm1", "settle")
		if !killAlt(s, VM1) {
			t.Fatalf("no outcome with the kill:\n%s", dumpAlts(s))
		}
		for _, a := range alts(s) {
			if ss := slotOf(t, a, "pfsd-c@any#0"); a.Drv[VM1].KillAt == 0 && ss.Node != VM1 {
				t.Fatalf("the kill of vm1 with the reader on %s", ss.Node)
			}
		}
	})
	t.Run("reader-ns", func(t *testing.T) {
		// a reader's new region gets the namespace from the labels
		if s := steps(t, w, "driver-kill-at vm2 ns", "create pfsd-c@vm2#0", "settle"); !killAlt(s, VM2) {
			t.Fatalf("no outcome with the kill:\n%s", dumpAlts(s))
		}
	})
	t.Run("refused", func(t *testing.T) {
		// its probe shows ns, not mount, and its retries do so at any
		// time
		if s := steps(t, r, "driver-kill-at vm2 mount", "create pfsd-c@vm2#0", "driver-kill vm2", "settle"); killAlt(s, VM2) {
			t.Fatalf("mount of a refused claim:\n%s", dumpAlts(s))
		}
		s := steps(t, r, "driver-kill-at vm2 ns", "create pfsd-c@vm2#0", "settle")
		wantPhase(t, s, "pfsd-c@vm2#0", Refused)
		if !killAlt(s, VM2) {
			t.Fatalf("no outcome with the kill:\n%s", dumpAlts(s))
		}
		steps(t, s, "!driver-kill vm2", "!driver-restart vm2", "kubelet-restart vm2")
	})
	t.Run("blocked", func(t *testing.T) {
		// region, not ns: no nd region through the hole
		steps(t, h, "driver-kill-at vm1 region", "!driver-kill vm1")
		steps(t, h, "driver-kill-at vm1 ns", "driver-kill vm1")
	})
	t.Run("unblocking", func(t *testing.T) {
		// its retry is a whole Prepare
		u := steps(t, h, "delete sdax-c@vm1#0", "settle")
		wantPhase(t, u, "pfsd-c@vm1#0", Unblocking)
		if s := steps(t, u, "driver-kill-at vm1 mount", "!driver-kill vm1", "settle"); !killAlt(s, VM1) {
			t.Fatalf("no outcome with the kill:\n%s", dumpAlts(s))
		}
	})
	t.Run("stalled", func(t *testing.T) {
		// a contender held in the scheduler: its promotion, on any host
		st := steps(t, w, "create pfsc-c@vm2#0", "delete pfsc-c@vm2#0", "settle", "create pfsd-c@any#0", "settle")
		wantPhase(t, st, "pfsd-c@any#0", Stalled)
		for _, v := range []VM{VM0, VM1, VM2} {
			if s := steps(t, st, "driver-kill-at "+v.String()+" mount", "!driver-kill "+v.String(), "settle"); !killAlt(s, v) {
				t.Fatalf("on %s: no outcome with the kill:\n%s", v, dumpAlts(s))
			}
		}
	})
	t.Run("gone-and-back", func(t *testing.T) {
		g := steps(t, w, "create pfsd-c@vm1#0", "settle")
		if s := steps(t, g, "driver-kill-at vm1 region", "delete pfsd-c@vm1#0", "create pfsc-c@vm1#0", "!driver-kill vm1", "settle"); !killAlt(s, VM1) {
			t.Fatalf("no outcome with the kill:\n%s", dumpAlts(s))
		}
	})
	t.Run("volatile", func(t *testing.T) {
		if s := steps(t, w, "driver-kill-at vm1 region", "create xnuma-c@vm1#0", "create sdax-c@vm1#0", "driver-kill vm1", "settle"); killAlt(s, VM1) {
			t.Fatalf("a kill at a volatile region:\n%s", dumpAlts(s))
		}
	})
}

// A writer blocked through a hole that a window clears is unblocking:
// its retry, any time, makes the namespace of a device without one (NS
// maybe, adopted from pdevs), and seen Running it has made it (fuzz,
// fullp seed 7985).
func TestPmemUnblockedWriterNamespace(t *testing.T) {
	h := stateAfter(t, "fullp", true, "create xnuma-c@vm1#0", "settle", "create sdax-c@vm1#0", "settle", "delete xnuma-c@vm1#0", "settle", "create xpb-c@vm1#0", "settle")
	wantStack(t, h, stackIndex(VM1, HB1), "[hole,c-shared0]")
	wantPhase(t, h, "xpb-c@vm1#0", Blocked)
	if pd := h.PDev[pmemIndex(cPmem1)]; pd.NS != TriNo {
		t.Fatalf("blocked: c-pmem1 ns %v", pd.NS)
	}
	u := steps(t, h, "delete sdax-c@vm1#0", "settle")
	wantPhase(t, u, "xpb-c@vm1#0", Unblocking)
	if pd := u.PDev[pmemIndex(cPmem1)]; pd.NS != TriMaybe {
		t.Fatalf("unblocking: c-pmem1 ns %v, want maybe", pd.NS)
	}
	truth := u.clone()
	truth.Slots[truth.cfg.slotIndex("xpb-c@vm1#0")].Phase = Running
	truth.Stacks[stackIndex(VM1, HB1)] = Stack{}
	truth.Stacks[stackIndex(VM1, HB1)].push(devKey(cPmem1))
	truth.madeNamespace(cPmem1)
	v, adopted := checkState(u, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "xpb-c@vm1#0", Running)
	if pd := adopted.PDev[pmemIndex(cPmem1)]; pd.NS != TriYes {
		t.Fatalf("adopted c-pmem1 ns %v, want yes", pd.NS)
	}
}

// A writer stalled by the scheduler's hold (A19) and promoted through a
// hole on its stack is Blocked: its Prepare fails at the region, before
// any namespace (pool_persistent.go preparePersistent). The oracle took
// it as Running first and made the namespace (a settle that waits for
// the promotion, --stall-window 30s, timed out on "want its namespace");
// the simulator's truth made one at random when the reference bound such
// a writer, and kept its holds for ever, so that it had writers stalled
// that the model had Starting (fuzz, fullp --refpool-len 120 seeds 1452,
// 2264, 3780: "the LSA changed ... without a writer's Prepare").
func TestPmemStalledWriterThroughHole(t *testing.T) {
	hole := []string{"create xnuma-c@vm1#0", "settle", "create sdax-c@vm1#0", "settle", "delete xnuma-c@vm1#0", "settle"}
	held := []string{"create xpf-c@vm0#0", "delete xpf-c@vm0#0", "settle"}
	stalled := append(append(append([]string(nil), hole...), held...), "create xpb-c@vm1#0", "settle")
	s := stateAfter(t, "fullp", true, stalled...)
	wantStack(t, s, stackIndex(VM1, HB1), "[hole,c-shared0]")
	wantPhase(t, s, "xpb-c@vm1#0", Stalled)
	if pd := pdevOf(s, cPmem1); pd.NS != TriMaybe {
		t.Fatalf("stalled writer: c-pmem1 ns %v, want maybe", pd.NS)
	}
	truth := s.clone()
	truth.Slots[truth.cfg.slotIndex("xpb-c@vm1#0")].Phase = Blocked
	pdevOf(truth, cPmem1).NS = TriNo
	v, adopted := checkState(s, observation(t, truth), "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "xpb-c@vm1#0", Blocked)
	if pd := pdevOf(adopted, cPmem1); pd.NS != TriNo {
		t.Fatalf("adopted c-pmem1 ns %v, want no", pd.NS)
	}
	// against the reference: the writer stalled (30 s: the settle
	// waits for its promotion, and the truth has it Blocked without a
	// namespace); the writer created after the hold ended (Starting in
	// the model and in the truth)
	var sm *sim
	dbgPred = func(_ *State, s *sim) { sm = s }
	defer func() { dbgPred = nil }()
	starting := append(append(append([]string(nil), held...), hole...), "create xpb-c@vm1#0", "settle")
	for _, w := range []time.Duration{0, 30 * time.Second} {
		for seed := int64(1); seed <= 10; seed++ {
			for _, script := range [][]string{stalled, starting} {
				if f := refScript(t, "fullp", seed, w, script...); f != nil {
					t.Fatalf("--stall-window %s:\n%s", w, f)
				}
				x := sm.truth.cfg.slotIndex("xpb-c@vm1#0")
				if ph, ns := sm.truth.Slots[x].Phase, pdevOf(sm.truth, cPmem1).NS; ph == Blocked && ns != TriNo {
					t.Fatalf("--stall-window %s, seed %d: the truth's blocked writer made a namespace (%v)", w, seed, ns)
				}
			}
		}
	}
}
