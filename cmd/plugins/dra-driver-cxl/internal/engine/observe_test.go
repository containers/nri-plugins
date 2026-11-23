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

package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The changes of the kernel objects between two observations that the
// kernel's tables have give no line; one that they do not have does.
func TestUnknownTransitions(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	k := v.f.Kernel()
	before, err := k.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	v.mustPrepare(poolDriver, "c", poolReq("c", 0, serialA, state.ModeNUMA, state.KindPool))
	after, err := k.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lines := unknownTransitions(before, after); len(lines) > 0 {
		t.Fatalf("documented changes reported: %v", lines)
	}
	// a memdev that loses its endpoint port without being disabled
	bad := *after
	bad.Memdevs = append([]kernel.MemdevInfo(nil), after.Memdevs...)
	bad.Memdevs[0].State = kernel.MemdevBound
	bad.DaxDevices = append([]kernel.DaxDeviceInfo(nil), after.DaxDevices...)
	bad.DaxDevices[0].State = kernel.DaxOther
	lines := unknownTransitions(after, &bad)
	want := []string{
		"memdev 0xa1: MemdevInfo.State: READY -> BOUND: transition not in the table",
		"dax device dax0.0: DaxDeviceInfo.State: KMEM -> OTHER: transition not in the table",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	v.mustUnprepare(poolDriver, "c")
}

// Observe logs the unknown changes at debug level, with the start of the
// line that the gates grep for.
func TestObserveLogsUnknownTransitions(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	logs := klogtest.Capture(t, 4)
	if _, err := v.e.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the removal of a memdev is a row: nothing is logged
	v.f.Unplug(serialA)
	if _, err := v.e.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs(), UnknownTransition) {
		t.Fatalf("a documented change logged: %s", logs())
	}
	v.e.qmu.Lock()
	last := *v.e.last
	v.e.last = &last
	last.Memdevs = []kernel.MemdevInfo{{Serial: serialB, State: kernel.MemdevReady}}
	v.e.qmu.Unlock()
	v.f.EndpointDelay = time.Second
	v.f.Plug(fake.MemdevSpec{Serial: serialB})
	if _, err := v.e.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs(), UnknownTransition+"memdev 0xb1: MemdevInfo.State: READY -> BOUND") {
		t.Fatalf("logs %s", logs())
	}
}

// A hollow region of ours is warned of once while it stays hollow, and
// again when the kernel reuses its name for another hollow region.
func TestObserveHollowOncePerRegion(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	logs := klogtest.Capture(t, 3)
	for range 2 {
		v.mustPrepare(poolDriver, "c", poolReq("c", 0, serialA, state.ModeDax, state.KindPool))
		v.f.Unplug(serialA)
		for range 2 {
			if _, err := v.e.Observe(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		v.mustUnprepare(poolDriver, "c")
		if _, err := v.e.Observe(context.Background()); err != nil {
			t.Fatal(err)
		}
		v.f.Plug(fake.MemdevSpec{Serial: serialA})
	}
	// klog writes a warning to the output of each lower severity too
	warned := map[string]bool{}
	for _, line := range strings.Split(logs(), "\n") {
		if strings.Contains(line, "region0 is hollow") {
			warned[line] = true
		}
	}
	if len(warned) != 2 {
		t.Fatalf("warnings %v", warned)
	}
}

// A request that waits for its memdev does not hold up the requests of
// another memdev: one queue per memdev.
func TestQueuesPerMemdev(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialB})
	done := make(chan error, 1)
	go func() {
		_, err := v.e.PrepareGrants(context.Background(), []GrantRequest{poolReq("wait", 0, serialA, state.ModeDax, state.KindPool)})
		done <- err
	}()
	// the grant is JOURNALED before the request waits for its memdev
	for deadline := time.Now().Add(5 * time.Second); v.records().Grant(state.GrantRef{Claim: "wait"}) == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the request of the missing memdev journaled no grant")
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{poolReq("other", 0, serialB, state.ModeDax, state.KindPool)}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the other memdev waited %v", time.Since(start))
	}
	v.f.Plug(fake.MemdevSpec{Serial: serialA})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := v.e.ReleaseGrants(context.Background(), []state.GrantRef{{Claim: "wait"}, {Claim: "other"}}, ReleaseUnprepare); err != nil {
		t.Fatal(err)
	}
	if r := v.records(); len(r.Grants)+len(r.Memdevs)+len(r.Regions) > 0 {
		t.Fatalf("records %+v", r)
	}
}

// The verify step of a Prepare compares the new observation with the last
// one: an unknown change is logged at V(4), and so is every violation of an
// invariant.
func TestVerifyStepLogs(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	logs := klogtest.Capture(t, 4)
	if _, err := v.e.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a memdev whose endpoint port is still missing (BOUND) that the last
	// observation had READY: READY -> BOUND is no row
	v.f.EndpointDelay = time.Second
	v.f.Plug(fake.MemdevSpec{Serial: serialB})
	v.e.qmu.Lock()
	last := *v.e.last
	last.Memdevs = append(append([]kernel.MemdevInfo(nil), last.Memdevs...), kernel.MemdevInfo{Serial: serialB, State: kernel.MemdevReady})
	v.e.last = &last
	v.e.qmu.Unlock()
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{poolReq("c", 0, serialA, state.ModeDax, state.KindPool)}); err != nil {
		t.Fatal(err)
	}
	if err := v.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Provenance = append(doc.Provenance, state.Provenance{Serial: serialL, State: state.ProvenanceHeld})
		doc.Grants = append(doc.Grants, state.Grant{Ref: state.GrantRef{Claim: "l"}, Serial: serialL,
			Asked: state.Asked{Source: state.SourceLocal}, State: state.GrantJournaled})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := v.e.ReleaseGrants(context.Background(), []state.GrantRef{{Claim: "c"}}, ReleaseUnprepare); err != nil {
		t.Fatal(err)
	}
	out := logs()
	for _, want := range []string{
		UnknownTransition + "memdev 0xb1: MemdevInfo.State: READY -> BOUND",
		"engine invariant: I10: grant l/0: a new local grant on serial 0x11 of provenance HELD",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no line %q in\n%s", want, out)
		}
	}
}

// The verify step checks the invariants only between requests: a request
// whose verify step runs while a local part of another request is half made
// logs no violation of it; between requests, a part whose only user is the
// grant of a Prepare that did not finish (not a kept one) is a violation.
func TestVerifyStepBetweenRequests(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA}, fake.MemdevSpec{Serial: serialL, Ram: gib})
	logs := klogtest.Capture(t, 4)
	ran := false
	v.onZero = func() {
		// the dax part of l is sized and bound, its grant REALISING
		ran = true
		if err := v.prepare(poolDriver, "p", poolReq("p", 0, serialA, state.ModeDax, state.KindPool)); err != nil {
			t.Error(err)
		}
	}
	v.mustPrepare(localDriver, "l", sliceReq("l", 0, state.ModeDax, 256*mib))
	if !ran {
		t.Fatal("no zeroing of the dax part")
	}
	if out := logs(); strings.Contains(out, "engine invariant:") {
		t.Fatalf("violations of a request in progress logged:\n%s", out)
	}
	region, part := v.grant("l", 0).Got.Region, v.grant("l", 0).Got.Dax
	if err := v.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Grant(state.GrantRef{Claim: "l"}).State = state.GrantJournaled
		doc.Preparation(localDriver, "l").State = state.PreparationPreparing
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := v.e.ReleaseGrants(context.Background(), []state.GrantRef{{Claim: "p"}}, ReleaseUnprepare); err != nil {
		t.Fatal(err)
	}
	out := logs()
	for _, want := range []string{
		"engine invariant: users: region " + string(region) + ": dax device " + part + " has no realised or releasing user",
		"engine invariant: I2: region " + string(region) + ": " + part + " has no REALISED or RELEASING user",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no line %q in\n%s", want, out)
		}
	}
}

// The end of a request checks the invariants by its last observation when
// no other request is in progress: a Prepare of mode fs and its Unprepare
// log no violation of their node mount, nor does a request that ends while
// the Prepare of mode fs is in progress (its grant realised and its node
// mount mounted, its preparation PREPARING).
func TestRequestChecksAtItsEnd(t *testing.T) {
	v := pmemEnv(t, "ext4")
	v.f.Plug(fake.MemdevSpec{Serial: serialA})
	logs := klogtest.Capture(t, 4)
	v.mustPrepare(poolDriver, "w", fsGrant("w", false, ""))
	v.mustUnprepare(poolDriver, "w")
	ran := false
	v.beforePrepared = func() {
		ran = true
		if err := v.prepare(poolDriver, "p", poolReq("p", 0, serialA, state.ModeDax, state.KindPool)); err != nil {
			t.Error(err)
		}
	}
	v.mustPrepare(poolDriver, "r", fsGrant("r", true, state.AccessDirect))
	if !ran {
		t.Fatal("no request in the Prepare of mode fs")
	}
	v.mustUnprepare(poolDriver, "r")
	v.mustUnprepare(poolDriver, "p")
	if out := logs(); strings.Contains(out, "engine invariant:") {
		t.Fatalf("violations logged:\n%s", out)
	}
}
