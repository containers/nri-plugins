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
	"fmt"
	"strings"
	"testing"
)

func TestExclusivePoolDevice(t *testing.T) {
	s := stateAfter(t, "full", false, "create xnuma-a@vm1#0", "create xdax-a@vm2#0", "!create xdax-a@vm1#0")
	wantPhase(t, s, "xnuma-a@vm1#0", Starting)
	wantPhase(t, s, "xdax-a@vm2#0", Pending)
	s = steps(t, s, "settle")
	wantPhase(t, s, "xnuma-a@vm1#0", Running)
	wantPhase(t, s, "xdax-a@vm2#0", Pending)
	if s.Ctrl[PoolA].Att[0][VM1] != TriYes || s.Ctrl[PoolA].Att[0][VM2] != TriNo {
		t.Fatalf("attachments %v", s.Ctrl[PoolA].Att)
	}
	if s.Data[pooledDev(PoolA)].Known {
		t.Fatalf("a numa use must make the content unknown")
	}
	// the holder goes: the waiter gets the device, nobody else may ask
	s = steps(t, s, "delete xnuma-a@vm1#0", "!create xdax-a@vm1#0")
	wantPhase(t, s, "xdax-a@vm2#0", Starting)
	if !s.contested(Res(pooledDev(PoolA))) {
		t.Fatalf("not contested")
	}
	s = steps(t, s, "settle", "create xdax-a@vm1#0")
	wantPhase(t, s, "xdax-a@vm2#0", Running)
	wantPhase(t, s, "xnuma-a@vm1#0", Absent)
	wantPhase(t, s, "xdax-a@vm1#0", Pending)
	// moved from vm1 to vm2: detach then attach
	if s.Ctrl[PoolA].Att[0][VM1] != TriNo || s.Ctrl[PoolA].Att[0][VM2] != TriYes {
		t.Fatalf("attachments %v", s.Ctrl[PoolA].Att)
	}
}

func TestMoveInOneWindow(t *testing.T) {
	s := stateAfter(t, "full", false, "create xdax-a@vm1#0", "settle", "delete xdax-a@vm1#0", "create xdax-a@vm2#0")
	// the only candidate for the device being released
	wantPhase(t, s, "xdax-a@vm2#0", Starting)
	s = steps(t, s, "!create xnuma-a@vm2#0", "settle")
	wantPhase(t, s, "xdax-a@vm2#0", Running)
	// on-line the claim of a pod gone at the last settle may be
	// deallocated a moment later: one candidate per such device in the
	// window, and nothing released after a settle without deletes
	s = stateAfter(t, "full", true, "create xnuma-c@vm1#0", "settle", "delete xnuma-c@vm1#0", "settle")
	s = steps(t, s, "create xnuma-c@vm1#0", "!create xdax-c@vm2#0", "settle")
	if s.Deallocating != 0 {
		t.Fatalf("released %b after a settle without deletes", s.Deallocating)
	}
}

func TestSharedDevice(t *testing.T) {
	s := stateAfter(t, "full", false, "create sdax-a@vm1#0", "create sdax-a@vm2#0", "create sro-a@vm2#0", "create sexcl-a@vm2#0", "settle")
	for _, n := range []string{"sdax-a@vm1#0", "sdax-a@vm2#0", "sro-a@vm2#0"} {
		wantPhase(t, s, n, Running)
	}
	wantPhase(t, s, "sexcl-a@vm2#0", Pending)
	att, excl := s.desiredAtt(PoolA)
	if !att[1][VM1] || !att[1][VM2] || excl[1][VM2] {
		t.Fatalf("desired %v excl %v", att, excl)
	}
	// a waiter blocks new users of the device
	s = steps(t, s, "!create sdax-a@vm2#1", "delete sdax-a@vm1#0", "delete sdax-a@vm2#0")
	wantPhase(t, s, "sexcl-a@vm2#0", Pending)
	s = steps(t, s, "delete sro-a@vm2#0")
	wantPhase(t, s, "sexcl-a@vm2#0", Starting)
	s = steps(t, s, "settle")
	_, excl = s.desiredAtt(PoolA)
	if !excl[1][VM2] || s.Ctrl[PoolA].Att[1][VM1] != TriNo {
		t.Fatalf("excl %v att %v", excl, s.Ctrl[PoolA].Att)
	}
	// shares wait for the exclusive user
	s = steps(t, s, "create sdax-a@vm1#0", "settle")
	wantPhase(t, s, "sdax-a@vm1#0", Pending)
}

func TestLocalCapacity(t *testing.T) {
	s := stateAfter(t, "full", false, "create lnuma@vm0#0", "create ldax@vm0#0", "create lnuma@vm0#1", "!create lnuma@vm0#1", "settle")
	wantPhase(t, s, "lnuma@vm0#0", Running)
	wantPhase(t, s, "ldax@vm0#0", Running)
	wantPhase(t, s, "lnuma@vm0#1", Pending)
	if got := s.SlotData[s.cfg.slotIndex("ldax@vm0#0")]; got != tokZero {
		t.Fatalf("a new dax slice reads %v, want zero", got)
	}
	s = steps(t, s, "create lease@vm0#0", "settle", "!lease-get lease@vm0#0 numa")
	s = steps(t, s, "delete ldax@vm0#0")
	wantPhase(t, s, "lnuma@vm0#1", Starting)
	// the block of the deleted slice is promised: no lease
	s = steps(t, s, "!lease-get lease@vm0#0 dax", "settle")
	wantPhase(t, s, "lnuma@vm0#1", Running)
	held, all := s.localUse(VM0)
	if held != 2 || all != 2 {
		t.Fatalf("local use %d/%d", held, all)
	}
}

func TestLease(t *testing.T) {
	s := stateAfter(t, "full", false, "!lease-get lease@vm0#0 numa", "create lease@vm0#0", "!lease-get lease@vm0#0 numa", "settle",
		"lease-get lease@vm0#0 numa", "!lease-get lease@vm0#0 dax")
	ss := slotOf(t, s, "lease@vm0#0")
	if ss.Lease != LeaseHeld || ss.LeaseMode != ModeNUMA {
		t.Fatalf("lease %v %v", ss.Lease, ss.LeaseMode)
	}
	if s.Drv[VM0].F&s.fenceBit(2) == 0 {
		t.Fatalf("a numa lease fences its node")
	}
	// one block left on vm0
	s = steps(t, s, "settle", "create lnuma@vm0#0", "create lnuma@vm0#1", "settle")
	wantPhase(t, s, "lnuma@vm0#0", Running)
	wantPhase(t, s, "lnuma@vm0#1", Pending)
	// kept across a crash of the requester
	s = steps(t, s, "crash lease@vm0#0", "settle")
	if slotOf(t, s, "lease@vm0#0").Lease != LeaseHeld {
		t.Fatalf("lease not kept across a crash")
	}
	// released with the requester pod; its block goes to the waiter
	s = steps(t, s, "delete lease@vm0#0")
	wantPhase(t, s, "lnuma@vm0#1", Starting)
	s = steps(t, s, "settle")
	if ss := slotOf(t, s, "lease@vm0#0"); ss.Lease != LeaseNone || ss.Phase != Absent {
		t.Fatalf("lease %v phase %v", ss.Lease, ss.Phase)
	}
	// lease-put
	s = steps(t, s, "create lease@vm2#0", "settle", "lease-get lease@vm2#0 dax", "settle", "lease-put lease@vm2#0", "!lease-put lease@vm2#0", "settle")
	if slotOf(t, s, "lease@vm2#0").Lease != LeaseNone {
		t.Fatalf("lease-put")
	}
}

func TestCrash(t *testing.T) {
	cfg := testConfig(t, "full", false)
	s := newState(cfg)
	s = steps(t, s, "!crash plain@vm0#0", "create plain@vm0#0", "!crash plain@vm0#0", "settle", "crash plain@vm0#0", "!crash plain@vm0#0")
	wantPhase(t, s, "plain@vm0#0", Restarting)
	s = steps(t, s, "settle", "crash plain@vm0#0", "settle", "!crash plain@vm0#0")
	if r := slotOf(t, s, "plain@vm0#0").Restarts; r != 2 {
		t.Fatalf("restarts %d", r)
	}
	// crash keeps the content of a dax slice
	s = steps(t, s, "create ldax@vm1#0", "settle", "write ldax@vm1#0", "crash ldax@vm1#0", "settle")
	if got := s.SlotData[cfg.slotIndex("ldax@vm1#0")]; !got.Known || !strings.HasPrefix(got.Text, "mbt-") {
		t.Fatalf("data %v", got)
	}
}

func TestControllerKill(t *testing.T) {
	// off-line a kill may not leave a prepare open
	stateAfter(t, "full", false, "create sdax-c@vm0#0", "settle", "create xnuma-c@vm1#0", "!ctrl-kill c")
	s := stateAfter(t, "full", true, "create sdax-c@vm0#0", "settle", "create xnuma-c@vm1#0", "delete sdax-c@vm0#0", "ctrl-kill c", "!ctrl-kill c",
		"create sdax-c@vm2#0", "settle")
	// created before the kill: running (prepared before it) or waiting
	// for the attach; after it: waits for the attach
	var phases []Phase
	for _, a := range alts(s) {
		phases = append(phases, slotOf(t, a, "xnuma-c@vm1#0").Phase)
		wantPhase(t, a, "sdax-c@vm2#0", Attaching)
	}
	if len(phases) != 2 || phases[0] != Running || phases[1] != Attaching {
		t.Fatalf("outcomes of xnuma-c@vm1#0: %v", phases)
	}
	s = s.Alts[0]
	s.Alts = nil
	c := s.Ctrl[PoolC]
	if c.Up || c.Att[0][VM1] != TriMaybe || c.Att[1][VM0] != TriMaybe || c.Att[1][VM2] != TriNo {
		t.Fatalf("controller %+v", c)
	}
	// an Attaching pod holds its device
	s = steps(t, s, "create sexcl-c@vm1#0", "settle")
	wantPhase(t, s, "sexcl-c@vm1#0", Pending)
	s = steps(t, s, "ctrl-start c", "settle")
	wantPhase(t, s, "xnuma-c@vm1#0", Running)
	wantPhase(t, s, "sdax-c@vm2#0", Running)
	c = s.Ctrl[PoolC]
	if c.Att[0][VM1] != TriYes || c.Att[1][VM0] != TriNo || c.Att[1][VM2] != TriYes {
		t.Fatalf("controller %+v", c)
	}
	// deleted while the controller is down: the attachment stays
	s = steps(t, s, "ctrl-kill c", "settle", "delete sdax-c@vm2#0", "settle")
	if s.Ctrl[PoolC].Att[1][VM2] != TriYes {
		t.Fatalf("attachment of a deleted pod must stay while the controller is down: %+v", s.Ctrl[PoolC])
	}
	s = steps(t, s, "ctrl-start c", "settle")
	if s.Ctrl[PoolC].Att[1][VM2] != TriNo {
		t.Fatalf("attachment not detached after ctrl-start: %+v", s.Ctrl[PoolC])
	}
}

// At a controller kill the attachments that may have changed since the
// last settle are open: the attach of a claim deleted before it was bound
// (in flight at the server), and an attachment that only claims being
// allocated want (a reconcile may have detached it before them).
func TestCtrlKillOpensAttachments(t *testing.T) {
	s := stateAfter(t, "full", true, "create sdax-b@vm0#1", "delete sdax-b@vm0#1", "ctrl-kill b")
	if got := s.Ctrl[PoolB].Att[1][VM0]; got != TriMaybe {
		t.Fatalf("b-shared0@vm0 %v, want maybe", got)
	}
	o := stateAfter(t, "full", true, "create sro-c@vm2#0", "settle", "ctrl-kill c", "delete sro-c@vm2#0", "settle")
	if o.Ctrl[PoolC].Att[1][VM2] != TriYes {
		t.Fatalf("orphan c-shared0@vm2 %v", o.Ctrl[PoolC].Att[1][VM2])
	}
	o = steps(t, o, "create sdax-c@vm2#0", "ctrl-start c", "ctrl-kill c")
	if got := o.Ctrl[PoolC].Att[1][VM2]; got != TriMaybe {
		t.Fatalf("c-shared0@vm2 %v, want maybe", got)
	}
}

// ctrl-restart b, then ctrl-kill b before the new controller published
// the slices: a claim of the pool created then is unschedulable, not
// waiting for the attach, until the controller is back.
func TestSlicesAbsentAfterCtrlRestart(t *testing.T) {
	s := stateAfter(t, "full", true, "create xnuma-b@vm0#0", "create sdax-b@vm0#1", "settle", "create xdax-b@vm0#0", "settle")
	wantPhase(t, s, "xdax-b@vm0#0", Pending)
	// not while a claim of the pool is being allocated
	steps(t, s, "create sro-b@vm0#0", "!ctrl-restart b")
	s = steps(t, s, "ctrl-restart b", "ctrl-kill b", "create sro-b@vm0#0", "settle")
	var phases []string
	for _, a := range alts(s) {
		phases = append(phases, slotOf(t, a, "sro-b@vm0#0").Phase.String()+"/"+a.Ctrl[PoolB].ResourceSlices.String())
		wantPhase(t, a, "xdax-b@vm0#0", Pending)
	}
	if strings.Join(phases, " ") != "attaching/present unsched/absent" {
		t.Fatalf("outcomes %v", phases)
	}
	// no slices, the claim not allocated
	truth := s.Alts[0]
	o := observation(t, truth)
	v, adopted := checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "sro-b@vm0#0", Unsched)
	if adopted.Ctrl[PoolB].ResourceSlices != SlicesAbsent {
		t.Fatalf("slices %s", adopted.Ctrl[PoolB].ResourceSlices)
	}
	// without poolSlices the pod's state decides
	o.PoolSlices = nil
	v, adopted = checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	wantPhase(t, adopted, "sro-b@vm0#0", Unsched)
	// slices present while predicted absent: wait
	o = observation(t, truth)
	o.PoolSlices["b"] = []ObsPoolSlice{{Name: "x"}}
	v, _ = checkState(adopted, o, "")
	wantVerdict(t, v, VWait, "pool b: 1 ResourceSlices, want none")
	// a new claim of the pool is unschedulable at once; nothing else
	// may ask for a device that such a claim waits for
	steps(t, adopted, "!create sexcl-b@vm0#0", "!create sdax-b@vm0#0")
	n := steps(t, adopted, "delete xnuma-b@vm0#0")
	wantPhase(t, n, "xdax-b@vm0#0", Unsched)
	n = steps(t, n, "settle", "!create xnuma-b@vm0#0", "ctrl-start b", "settle")
	wantPhase(t, n, "sro-b@vm0#0", Running)
	// xnuma-b's region was below b-shared0's: a hole
	wantPhase(t, n, "xdax-b@vm0#0", Blocked)
	wantStack(t, n, stackIndex(VM0, HB0), "[hole,b-shared0]")
	if n.Ctrl[PoolB].ResourceSlices != SlicesPresent || len(n.Alts) != 0 {
		t.Fatalf("after ctrl-start: slices %s, %d alternatives", n.Ctrl[PoolB].ResourceSlices, len(n.Alts))
	}
	// off-line a kill does not follow a restart in one window
	steps(t, stateAfter(t, "full", false, "create plain@vm0#0", "settle", "ctrl-restart b"), "!ctrl-kill b", "settle", "ctrl-kill b")
	// claims that tried while the pool had no slices are requeued
	// together when they are published: the scheduler picks one. While
	// the slices are not known to be back, a claim of the pool is the
	// only candidate for its device.
	steps(t, stateAfter(t, "full", true, "ctrl-restart a", "create xdax-a@vm2#0"), "!create xnuma-a@vm1#0", "create sdax-a@vm1#0")
	a := adoptSlices(t, stateAfter(t, "full", true, "ctrl-restart a", "ctrl-kill a", "settle"), PoolA, SlicesAbsent)
	a = steps(t, a, "create sdax-a@vm1#0", "settle", "ctrl-start a", "!create sexcl-a@vm2#0", "settle", "create sexcl-a@vm2#0")
	wantPhase(t, a, "sexcl-a@vm2#0", Pending)
}

// ctrl-restart a, ctrl-kill a before the new controller published,
// server-restart a, settle with no pod of the pool starting: the slices
// may be absent (the outcomes differ only in them: they must not be
// merged); absent is adopted, and nothing but ctrl-start brings them
// back.
func TestSlicesBackOnlyAtCtrlStart(t *testing.T) {
	s := stateAfter(t, "full", true, "create sexcl-a@vm2#0", "settle", "ctrl-restart a", "write sexcl-a@vm2#0", "ctrl-kill a", "server-restart a", "settle")
	var worlds []string
	for _, a := range alts(s) {
		worlds = append(worlds, a.Ctrl[PoolA].ResourceSlices.String())
	}
	if strings.Join(worlds, " ") != "present absent" {
		t.Fatalf("outcomes %v", worlds)
	}
	o := observation(t, s.Alts[0])
	if len(o.PoolSlices["a"]) != 0 {
		t.Fatalf("simulated slices %v", o.PoolSlices["a"])
	}
	v, adopted := checkState(s, o, "")
	wantVerdict(t, v, VOK, "ok")
	if adopted.Ctrl[PoolA].ResourceSlices != SlicesAbsent {
		t.Fatalf("adopted slices %s", adopted.Ctrl[PoolA].ResourceSlices)
	}
	n := steps(t, adopted, "server-restart a", "kubelet-restart vm2", "driver-kill vm2", "containerd-restart vm1", "settle")
	if n.Ctrl[PoolA].ResourceSlices != SlicesAbsent {
		t.Fatalf("slices %s after other faults", n.Ctrl[PoolA].ResourceSlices)
	}
	n = steps(t, n, "create xnuma-a@vm1#0")
	wantPhase(t, n, "xnuma-a@vm1#0", Unsched)
	n = steps(t, n, "settle", "ctrl-start a", "settle")
	wantPhase(t, n, "xnuma-a@vm1#0", Running)
	if n.Ctrl[PoolA].ResourceSlices != SlicesPresent {
		t.Fatalf("slices %s after ctrl-start", n.Ctrl[PoolA].ResourceSlices)
	}
}

// Pool a's slices absent after ctrl-restart a and ctrl-kill a; ctrl-start
// a, then the holder of a-pooled0 deleted: the new controller publishes
// the slices within seconds, the waiter of a-pooled0 and the claim that
// waited for the slices are allocated, attached and Running at the
// settle.
func TestSlicesBackAfterCtrlStart(t *testing.T) {
	s := stateAfter(t, "full", true, "create xnuma-a@vm2#0", "settle", "create xdax-a@vm1#0", "settle",
		"ctrl-restart a", "ctrl-kill a", "settle")
	s = adoptSlices(t, s, PoolA, SlicesAbsent)
	s = steps(t, s, "create sro-a@vm1#0", "settle")
	wantPhase(t, s, "sro-a@vm1#0", Unsched)
	s = steps(t, s, "ctrl-start a", "delete xnuma-a@vm2#0", "settle")
	for _, a := range alts(s) { // (the order of their two regions races)
		wantPhase(t, a, "xdax-a@vm1#0", Running)
		wantPhase(t, a, "sro-a@vm1#0", Running)
		if a.Ctrl[PoolA].ResourceSlices != SlicesPresent {
			t.Fatalf("slices %s", a.Ctrl[PoolA].ResourceSlices)
		}
	}
	// killed again before the publish: present or absent
	k := steps(t, adoptSlices(t, stateAfter(t, "full", true, "ctrl-restart a", "ctrl-kill a", "settle"), PoolA, SlicesAbsent), "ctrl-start a", "ctrl-kill a", "settle")
	var worlds []string
	for _, a := range alts(k) {
		worlds = append(worlds, a.Ctrl[PoolA].ResourceSlices.String())
	}
	if strings.Join(worlds, " ") != "present absent" {
		t.Fatalf("ctrl-start, ctrl-kill: outcomes %v", worlds)
	}
}

func TestDriverRestart(t *testing.T) {
	s := stateAfter(t, "full", false, "create lsteer@vm2#0", "!create xnuma-a@vm2#0", "create xnuma-c@vm1#0", "settle", "create xnuma-a@vm2#0", "settle")
	if got := slotOf(t, s, "lsteer@vm2#0").Steer; got != SteerOn {
		t.Fatalf("steer %v", got)
	}
	if s.Drv[VM2].F != s.fenceBit(2) || s.Drv[VM1].F != s.fenceBit(3) {
		t.Fatalf("F %v %v", s.Drv[VM2].F, s.Drv[VM1].F)
	}
	// the local region exists: lnuma reuses it, no race with the
	// destroy of a-pooled0's region
	s = steps(t, s, "delete xnuma-a@vm2#0", "create lnuma@vm2#0", "driver-kill vm2", "!driver-kill vm2", "!driver-restart vm2")
	d := s.Drv[VM2]
	if d.Restarts != 1 || !d.Killed || d.F != s.fenceBit(2) {
		t.Fatalf("driver %+v", d)
	}
	if got := slotOf(t, s, "lsteer@vm2#0").Steer; got != SteerOff {
		t.Fatalf("a manager does not survive its driver: steer %v", got)
	}
	s = steps(t, s, "!lease-get lease@vm2#0 numa", "settle", "crash lsteer@vm2#0", "driver-kill vm2", "!driver-kill vm2", "settle")
	if got := slotOf(t, s, "lsteer@vm2#0").Steer; got != SteerAny {
		t.Fatalf("restarted in a driver restart window: steer %v", got)
	}
	s = steps(t, s, "driver-restart vm2", "settle")
	if d := s.Drv[VM2]; d.Restarts != 0 || d.PodGen != 1 {
		t.Fatalf("driver %+v", d)
	}
	// F is reseeded from what is prepared: xnuma-a was deleted
	s = steps(t, s, "delete lnuma@vm2#0", "delete lsteer@vm2#0", "settle", "driver-restart vm2", "settle")
	if s.Drv[VM2].F != 0 || s.Drv[VM2].FMaybe != 0 {
		t.Fatalf("F %v maybe %v", s.Drv[VM2].F, s.Drv[VM2].FMaybe)
	}
	// in flux at a restart: maybe
	s = steps(t, s, "create lnuma@vm2#0", "driver-restart vm2")
	if s.Drv[VM2].FMaybe != s.fenceBit(2) {
		t.Fatalf("maybe %v", s.Drv[VM2].FMaybe)
	}
	s = steps(t, s, "settle")
	if s.Drv[VM2].F != s.fenceBit(2) || s.Drv[VM2].FMaybe != 0 {
		t.Fatalf("F %v maybe %v", s.Drv[VM2].F, s.Drv[VM2].FMaybe)
	}
}

// After driver-kill, driver-restart and containerd-restart alike a running
// lsteer container is not steered (memory.high max); restarted, it is
// steered again. An unblocking one may be created while the driver is
// away (the new driver's Synchronize sees it CREATED and manages it
// fenced, memory.high max) or after the restart (steered): either is
// adopted, and off stays off until its container restarts under a running
// driver.
func TestSteeringAfterDriverFaults(t *testing.T) {
	unblocking := stateAfter(t, "full", true,
		"create xnuma-a@vm2#0", "settle", "create sdax-a@vm2#0", "settle", // vm2/hb0 [a-pooled0,a-shared0]
		"delete xnuma-a@vm2#0", "settle", // [hole,a-shared0]
		"create lsteer@vm2#0", "settle", // blocked
		"delete sdax-a@vm2#0", "settle") // unblocking
	wantPhase(t, unblocking, "lsteer@vm2#0", Unblocking)
	for _, fault := range []string{"driver-kill vm2", "containerd-restart vm2", "driver-restart vm2"} {
		s := stateAfter(t, "full", false, "create lsteer@vm2#0", "settle", fault, "settle")
		if got := slotOf(t, s, "lsteer@vm2#0").Steer; got != SteerOff {
			t.Fatalf("%s: steer %s, want off (max)", fault, got)
		}
		s = steps(t, s, "crash lsteer@vm2#0", "settle")
		if got := slotOf(t, s, "lsteer@vm2#0").Steer; got != SteerOn {
			t.Fatalf("%s, crash: steer %s, want on", fault, got)
		}
		n := steps(t, unblocking, fault, "settle")
		if st := slotOf(t, n, "lsteer@vm2#0").Steer; st != SteerAny {
			t.Fatalf("%s, unblocking: steering %s, want either", fault, st)
		}
		for _, steer := range []Steer{SteerOff, SteerOn} {
			truth := n.clone()
			ss := &truth.Slots[truth.cfg.slotIndex("lsteer@vm2#0")]
			ss.Phase, ss.Steer = Running, steer
			truth.setUnblockAt(truth.cfg.slotIndex("lsteer@vm2#0"), 0)
			truth.Stacks[stackIndex(VM2, HB0)].push(rkLocal)
			truth.growFence()
			v, adopted := checkState(n, observation(t, truth), "")
			wantVerdict(t, v, VOK, "ok")
			got := slotOf(t, adopted, "lsteer@vm2#0")
			if got.Phase != Running || got.Steer != steer {
				t.Fatalf("%s: adopted %s steer %s, want running %s", fault, got.Phase, got.Steer, steer)
			}
			if steer == SteerOff {
				if st := steps(t, adopted, "create plain@vm2#0", "settle"); slotOf(t, st, "lsteer@vm2#0").Steer != SteerOff {
					t.Fatalf("%s: off not kept", fault)
				}
				if st := steps(t, adopted, "crash lsteer@vm2#0", "settle"); slotOf(t, st, "lsteer@vm2#0").Steer != SteerOn {
					t.Fatalf("%s: not steered after a crash", fault)
				}
			}
		}
	}
}

func TestContainerdRestart(t *testing.T) {
	s := stateAfter(t, "full", false, "create lsteer@vm1#0", "create xnuma-c@vm1#0", "settle", "containerd-restart vm1", "!containerd-restart vm1",
		"!driver-kill vm1", "!lease-get lease@vm1#0 numa")
	d := s.Drv[VM1]
	if d.Restarts != 1 || !d.Killed || d.F != s.fenceBit(2)|s.fenceBit(3) {
		t.Fatalf("driver %+v", d)
	}
	// A17: steering ends as after driver-kill: memory.high max
	if got := slotOf(t, s, "lsteer@vm1#0").Steer; got != SteerOff {
		t.Fatalf("steer %v", got)
	}
	steps(t, s, "settle", "containerd-restart vm1", "settle", "!containerd-restart vm1", "!driver-kill vm1", "driver-restart vm1", "settle", "containerd-restart vm1")
}

// A kubelet or a containerd restart allows one more restart of every
// container of its VM that is being created or started (starting,
// restarting, unblocking pods): kubelet may create it again. The observed
// count is adopted. A containerd restart while kubelet re-created a
// crashed container gave it two restarts for one crash.
func TestRestartSlack(t *testing.T) {
	for _, c := range []struct {
		name    string
		lines   []string
		slack   map[string]uint8 // of the slots
		crashed int              // lease@vm2#0's crashes in the model
	}{
		{"kubelet", []string{"create lease@vm2#0", "create plain@vm1#0", "kubelet-restart vm2", "settle"},
			map[string]uint8{"lease@vm2#0": 1, "plain@vm1#0": 0}, 0},
		{"containerd", []string{"create lease@vm2#0", "create sdax-a@vm2#0", "settle", "lease-get lease@vm2#0 numa",
			"crash lease@vm2#0", "write sdax-a@vm2#0", "create lnuma@vm2#1", "create plain@vm1#0", "containerd-restart vm2", "settle"},
			map[string]uint8{"lease@vm2#0": 1, "lnuma@vm2#1": 1, "sdax-a@vm2#0": 0, "plain@vm1#0": 0}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := stateAfter(t, "full", true, c.lines...)
			for slot, want := range c.slack {
				if got := slotOf(t, s, slot).Slack; got != want {
					t.Fatalf("%s: slack %d, want %d", slot, got, want)
				}
			}
			if got := int(slotOf(t, s, "lease@vm2#0").Restarts); got != c.crashed {
				t.Fatalf("lease@vm2#0: %d restarts, want %d", got, c.crashed)
			}
			for restarts := c.crashed; restarts <= c.crashed+2; restarts++ {
				o := observation(t, s)
				podOf(t, o, "lease@vm2#0").Restarts = restarts
				clear(o.Inspect)
				v, adopted := checkState(s, o, "")
				if restarts == c.crashed+2 {
					wantVerdict(t, v, VFail, fmt.Sprintf("%d restarts, the model crashed it %d times (+1 for kubelet or containerd restarts", restarts, c.crashed))
					continue
				}
				wantVerdict(t, v, VOK, "ok")
				if ss := slotOf(t, adopted, "lease@vm2#0"); int(ss.Restarts) != restarts || ss.Slack != 0 {
					t.Fatalf("%d restarts: adopted %+v", restarts, ss)
				}
			}
		})
	}
}

func TestData(t *testing.T) {
	cfg := testConfig(t, "full", false)
	s := newState(cfg)
	s = steps(t, s, "create sdax-c@vm0#0", "create sro-c@vm2#0", "settle", "write sdax-c@vm0#0")
	tok := s.Data[sharedDev(PoolC)]
	if !tok.Known || tok.Text != "mbt-4" {
		t.Fatalf("token %v", tok)
	}
	s = steps(t, s, "write sro-c@vm2#0", "read sro-c@vm2#0")
	if s.Data[sharedDev(PoolC)] != tok {
		t.Fatalf("a read-only write changed the content")
	}
	e := expectDone(s, s.next(mustAct(t, cfg, "read sro-c@vm2#0")), mustAct(t, cfg, "read sro-c@vm2#0"), 7)
	if e.Done.Text == nil || *e.Done.Text != "mbt-4" {
		t.Fatalf("read expectation %+v", e.Done)
	}
	w := mustAct(t, cfg, "write sro-c@vm2#0")
	if e := expectDone(s, s.next(w), w, 7); e.Done.Status != "nonzero" {
		t.Fatalf("write on a read-only claim must be refused: %+v", e.Done)
	}
	// pool dax devices keep their bytes between claims and nodes
	s = steps(t, s, "create xdax-c@vm2#0", "settle", "write xdax-c@vm2#0", "delete xdax-c@vm2#0", "settle", "create xnuma-c@vm1#0", "settle")
	if s.Data[pooledDev(PoolC)].Known {
		t.Fatalf("numa use must make it unknown")
	}
	s = steps(t, s, "delete xnuma-c@vm1#0", "settle", "create xdax-c@vm2#0", "settle", "write xdax-c@vm2#0", "delete xdax-c@vm2#0", "settle")
	if tok := s.Data[pooledDev(PoolC)]; !tok.Known {
		t.Fatalf("token %v", tok)
	}
}

func TestForcedSettleAndAvoid(t *testing.T) {
	cfg := testConfig(t, "small", false)
	cfg.Unsettled = 2
	cfg.Avoid = []string{"containerd-restart", "driver-kill:vm1", "crash:lnuma"}
	s := newState(cfg)
	steps(t, s, "!settle", "create plain@vm0#0", "create sdax-c@vm0#0", "!create lnuma@vm0#0", "!driver-kill vm0", "settle",
		"write sdax-c@vm0#0", "read sdax-c@vm0#0")
	mm := newMbtModel(cfg)
	for _, a := range mm.acts {
		n := a.name(cfg)
		if strings.HasPrefix(n, "containerd-restart") || n == "driver-kill vm1" || strings.HasPrefix(n, "crash lnuma") {
			t.Fatalf("%s not avoided", n)
		}
	}
	if err := checkAvoid([]string{"no-such-verb"}); err == nil {
		t.Fatalf("unknown verb accepted")
	}
}

func TestAnySlots(t *testing.T) {
	cfg := testConfig(t, "full", true)
	s := newState(cfg)
	// unknown node: no controller kill until the settle
	s = steps(t, s, "create xdax-c@any#0", "!ctrl-kill c", "settle")
	// the settle tries every host: vm0 first, the others in Alts
	var nodes []VM
	for _, a := range alts(s) {
		ss := slotOf(t, a, "xdax-c@any#0")
		if ss.Phase != Running {
			t.Fatalf("any slot %+v", ss)
		}
		nodes = append(nodes, ss.Node)
	}
	if len(nodes) != 3 || nodes[0] != VM0 || nodes[1] != VM1 || nodes[2] != VM2 {
		t.Fatalf("nodes %v", nodes)
	}
	steps(t, s, "ctrl-kill c")
	off := testConfig(t, "full", false)
	if off.slotIndex("xdax-c@any#0") >= 0 {
		t.Fatalf("off-line profile has an any slot")
	}
}

func TestAbstractionMemo(t *testing.T) {
	s := stateAfter(t, "small", false, "create lnuma@vm0#0", "create xnuma-a@vm1#0")
	a := s.String()
	if a != s.String() || !strings.Contains(a, "dev a-pooled0 xnuma.new") || !strings.Contains(a, "local vm0 room lnuma.new") {
		t.Fatalf("abstraction %q", a)
	}
	if s.apply(mustAct(t, s.cfg, "settle")).String() == a {
		t.Fatalf("settle does not change the abstraction")
	}
}

func TestParseRoundTrip(t *testing.T) {
	cfg := testConfig(t, "full", true)
	for _, a := range allActs(cfg) {
		b, err := parseAct(cfg, a.name(cfg))
		if err != nil || b != a {
			t.Fatalf("%q: %+v %v, want %+v", a.name(cfg), b, err, a)
		}
	}
	for _, bad := range []string{"", "create", "create nosuch@vm0#0", "lease-get lease@vm0#0", "lease-get lease@vm0#0 fs",
		"driver-kill any", "ctrl-kill d", "settle now", "frobnicate"} {
		if _, err := parseAct(cfg, bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	for _, bad := range []string{"xnuma-b@vm1#0", "lnuma@any#0", "plain@any#0", "xnuma@vm1#0", "lnuma-a@vm0#0", "xnuma-a@vm1#01"} {
		if _, err := parseSlot(bad); err == nil {
			t.Fatalf("slot %q accepted", bad)
		}
	}
}
