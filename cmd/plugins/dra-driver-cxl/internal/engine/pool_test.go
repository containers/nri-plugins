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
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// A pool device in mode numa: its region with daxN.0 bound to kmem and its
// memory online movable on the node of its window, fenced before any of it
// is online; its Unprepare destroys the region and disables the memdev.
func TestPoolNuma(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	at := v.onlineAtFence()
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeNUMA, state.KindPool))
	g := v.grant("c1", 0)
	if g.State != state.GrantRealised || g.Got.Region != "region0" || g.Got.Dax != "dax0.0" || state.NUMANodeOf(g.Got.NUMANode) != 2 {
		t.Fatalf("grant %+v", g)
	}
	reg := v.records().Region("region0")
	if reg == nil || reg.State != state.RegionOwned || !reg.SetUp || reg.DaxDevices[0].State != state.DaxKmemOnline ||
		len(reg.DaxDevices[0].Users) != 1 {
		t.Fatalf("region %+v", reg)
	}
	if len(v.fenced) != 1 || v.fenced[0] != 2 || !slices.Equal(*at, []string{"2: []"}) {
		t.Fatalf("fenced %v, online at the fences %v", v.fenced, *at)
	}
	snap, _ := v.f.Kernel().Observe(context.Background())
	if online := snap.OnlineBlocks(snap.Region("region0").Range.Blocks(snap.BlockSize)); len(online) == 0 {
		t.Fatal("no memory online")
	}
	// Prepare again: the realised grant verifies and is reused
	before := v.f.Commands()
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{poolReq("c1", 0, serialA, state.ModeNUMA, state.KindPool)}); err != nil {
		t.Fatal(err)
	}
	if cmds := v.f.CommandsSince(before); cmds != "" {
		t.Fatalf("a Prepare again ran %q", cmds)
	}
	v.mustUnprepare(poolDriver, "c1")
	v.clean("unprepared")
	if !strings.Contains(v.f.Commands(), "disable-memdev mem0") {
		t.Fatalf("memdev not disabled: %s", v.f.Commands())
	}
}

// A pool device in mode dax: device_dax over the whole region, never
// online; a shared device is the region of every claim of it until the
// last one goes.
func TestPoolDax(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeDax, state.KindPool))
	g := v.grant("c1", 0)
	if g.Got.Dax != "dax0.0" || v.records().DaxDevOf(g).IsZero() || v.f.DaxDriver("dax0.0") != kernel.DriverDeviceDax {
		t.Fatalf("grant %+v, driver %s", g, v.f.DaxDriver("dax0.0"))
	}
	if len(v.fenced) != 0 {
		t.Fatalf("fenced %v", v.fenced)
	}
	v.mustUnprepare(poolDriver, "c1")
	v.clean("unprepared")
}

func TestPoolSharedDax(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeDax, state.KindPoolShared))
	v.mustPrepare(poolDriver, "c2", poolReq("c2", 1, serialA, state.ModeDax, state.KindPoolShared))
	d := v.records().Region("region0").DaxDevice("dax0.0")
	if len(d.Users) != 2 {
		t.Fatalf("users %v", d.Users)
	}
	v.mustUnprepare(poolDriver, "c1")
	if v.records().Region("region0") == nil || strings.Contains(v.f.Commands(), "destroy-region") {
		t.Fatal("the region went with its first claim")
	}
	v.mustUnprepare(poolDriver, "c2")
	v.clean("unprepared")
}

// A pool Prepare waits for its memdev to appear; one that does not appear
// fails with the deadline and leaves nothing.
func TestPoolWaitsForMemdev(t *testing.T) {
	v := newEnv(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		v.f.Plug(fake.MemdevSpec{Serial: serialA})
	}()
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeDax, state.KindPool))
	v.mustUnprepare(poolDriver, "c1")

	v.e.opts.Timeouts.Memdev = 100 * time.Millisecond
	before := v.f.Commands()
	start := time.Now()
	err := v.prepare(poolDriver, "c2", poolReq("c2", 0, serialB, state.ModeDax, state.KindPool))
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "0xb1") || took < 100*time.Millisecond || took > 2*time.Second {
		t.Fatalf("error %v after %v", err, took)
	}
	if cmds := v.f.CommandsSince(before); cmds != "" {
		t.Fatalf("commands %q", cmds)
	}
	v.check("timed out")
	v.clean("timed out")
	if v.records().Preparation(poolDriver, "c2") != nil {
		t.Fatal("the preparation of the failed Prepare stays")
	}
}

// A region on a pool memdev that no record names is never reused: the
// memdev is refused naming it, the region recorded FOREIGN and the failed
// grant RELEASING with the refusal as its Reason; the record goes once the
// region is gone, at a start or at the next Prepare.
func TestPoolForeignRegion(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	k := v.f.Kernel()
	foreign := func() {
		t.Helper()
		d, _ := k.RootDecoder("mem0", kernel.RegionRAM)
		if _, err := k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: "mem0", Decoder: d, Type: kernel.RegionRAM}); err != nil {
			t.Fatal(err)
		}
	}
	destroy := func(name string) {
		t.Helper()
		if err := k.DestroyRegion(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	wantForeign := func(what string, n int) {
		t.Helper()
		r := v.records()
		if len(r.Regions) != n {
			t.Fatalf("%s: regions %+v", what, r.Regions)
		}
		if n == 1 {
			if reg := r.Regions[0]; reg.Name != "region0" || reg.State != state.RegionForeign || reg.Owner != state.OwnerForeign ||
				!slices.Equal(reg.Targets, []state.Serial{serialA}) {
				t.Fatalf("%s: region %+v", what, reg)
			}
		}
	}
	foreign()
	var reason string
	prev := v.e.afterCommit
	v.e.afterCommit = func() {
		prev()
		if g := v.records().Grant(state.GrantRef{Claim: "c1"}); g != nil && g.State == state.GrantReleasing {
			reason = g.Reason
		}
	}
	before := v.f.Commands()
	err := v.prepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeDax, state.KindPool))
	const refusal = "memdev mem0 has region region0 that the driver did not create"
	if !errors.Is(err, errForeignRegion) || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("error %v", err)
	}
	if reason != refusal {
		t.Fatalf("reason of the refused grant %q", reason)
	}
	v.e.afterCommit = prev
	if cmds := v.f.CommandsSince(before); strings.Contains(cmds, "region0") {
		t.Fatalf("the foreign region was touched: %s", cmds)
	}
	if r := v.records(); len(r.Grants) > 0 || len(r.Memdevs) > 0 {
		t.Fatalf("records %+v", r)
	}
	wantForeign("refused", 1)
	v.restart()
	wantForeign("a start with the region", 1)
	destroy("region0")
	v.restart()
	wantForeign("a start without the region", 0)

	// gone at the next Prepare, which creates a region of the name
	foreign()
	if err := v.prepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeDax, state.KindPool)); !errors.Is(err, errForeignRegion) {
		t.Fatalf("error %v", err)
	}
	wantForeign("refused again", 1)
	destroy("region0")
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeDax, state.KindPool))
	if reg := v.records().Region("region0"); reg == nil || !reg.Ours() || reg.State != state.RegionOwned {
		t.Fatalf("region %+v", reg)
	}
	v.mustUnprepare(poolDriver, "c1")
	v.clean("unprepared")
}

// A memdev whose endpoint port is not there yet: the region creation is
// tried once more.
func TestPoolEndpointRetry(t *testing.T) {
	v := newEnv(t)
	v.f.EndpointDelay = 60 * time.Millisecond
	v.f.Plug(fake.MemdevSpec{Serial: serialA})
	v.e.opts.Timeouts.ProbeGrace = time.Millisecond
	v.e.opts.Timeouts.RegionRetry = 200 * time.Millisecond
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeDax, state.KindPool))
	v.mustUnprepare(poolDriver, "c1")
	v.clean("unprepared")
}

// A release whose memory cannot be offlined (EBUSY) fails and keeps the
// records; the retry releases.
func TestPoolReleaseBusy(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeNUMA, state.KindPool))
	snap, _ := v.f.Kernel().Observe(context.Background())
	blocks := snap.Region("region0").Range.Blocks(snap.BlockSize)
	v.f.FailWrite(blockState(blocks[0]), syscall.EBUSY)
	if err := v.unprepare(poolDriver, "c1"); err == nil {
		t.Fatal("a busy release succeeded")
	}
	if g := v.grant("c1", 0); g.State != state.GrantReleasing {
		t.Fatalf("grant %s", g.State)
	}
	v.mustUnprepare(poolDriver, "c1")
	v.clean("released at the retry")
}

// A new region whose daxN.0 device_dax takes by an id that another tool
// left: recorded as found, used in mode dax.
func TestPoolLeftDeviceDaxID(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	v.f.SetDaxID(kernel.DriverDeviceDax, "dax0.0")
	v.mustPrepare(poolDriver, "c", poolReq("c", 0, serialA, state.ModeDax, state.KindPool))
	if d := v.records().Region("region0").DaxDevice("dax0.0"); d.State != state.DaxDeviceDax {
		t.Fatalf("dax device %+v", d)
	}
	v.mustUnprepare(poolDriver, "c")
	v.clean("unprepared")
}

// The numa twin: daxN.0 found bound to device_dax is switched to kmem
// (DEVICE_DAX -> UNBOUND -> KMEM_OFFLINE) and onlined.
func TestPoolLeftDeviceDaxIDNuma(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	v.f.SetDaxID(kernel.DriverDeviceDax, "dax0.0")
	v.mustPrepare(poolDriver, "c", poolReq("c", 0, serialA, state.ModeNUMA, state.KindPool))
	if d := v.records().Region("region0").DaxDevice("dax0.0"); d.State != state.DaxKmemOnline {
		t.Fatalf("dax device %+v", d)
	}
	v.mustUnprepare(poolDriver, "c")
	v.clean("unprepared")
}

// A timeout of 0 or less is the default.
func TestTimeoutDefaults(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		got := Timeouts{Memdev: d, Holder: d}.withDefaults()
		if got.Memdev != 30*time.Second || got.Holder != 10*time.Second || got.Rollback != time.Minute {
			t.Errorf("%v: %+v", d, got)
		}
	}
}

// A fence that does not finish in time: the memory goes online anyway.
func TestFenceDeadline(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	v.e.opts.Timeouts.Fence = 20 * time.Millisecond
	// the fence holds the node, its pass does not finish
	v.e.opts.Fence = func(ctx context.Context, node int) {
		v.fenceMu.Lock()
		v.fenced = append(v.fenced, node)
		v.fenceMu.Unlock()
		<-ctx.Done()
	}
	logs := klogtest.Capture(t, 0)
	v.mustPrepare(poolDriver, "c", poolReq("c", 0, serialA, state.ModeNUMA, state.KindPool))
	if !strings.Contains(logs(), "fencing NUMA node 2 did not finish in 20ms: its memory goes online anyway") {
		t.Fatalf("logs %s", logs())
	}
	v.mustUnprepare(poolDriver, "c")
}

// A cxl create-region that fails with add_dev is tried once more, logged
// with the claim and the DRA device of its preparation.
func TestPoolAddDevRetry(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	v.f.FailCommand("cxl create-region", fake.ErrAddDev)
	res := grantResult("c1", 0)
	res.Device = "pooled0"
	if err := v.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Preparations = append(doc.Preparations, state.Preparation{UID: "c1", Driver: poolDriver, ClaimName: "ns/c1",
			State: state.PreparationPreparing, Results: []state.Result{res}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logs := klogtest.Capture(t, 3)
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialA, state.ModeDax, state.KindPool))
	if out := logs(); !strings.Contains(out, "engine: claim ns/c1: device pooled0: ") || !strings.Contains(out, "retrying once in") {
		t.Fatalf("no retry line in\n%s", out)
	}
	v.mustUnprepare(poolDriver, "c1")
	v.clean("unprepared")
}

// A failed Prepare returns the grants that its rollback kept with the
// error: the second grant of the claim finds a foreign region, the
// rollback cannot disable the memdev of the first one.
func TestPoolRollbackReturnsKept(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA}, fake.MemdevSpec{Serial: serialB})
	k := v.f.Kernel()
	d, _ := k.RootDecoder("mem1", kernel.RegionRAM)
	if _, err := k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: "mem1", Decoder: d, Type: kernel.RegionRAM}); err != nil {
		t.Fatal(err)
	}
	v.f.FailCommand("cxl disable-memdev", syscall.EBUSY)
	b := poolReq("c", 1, serialB, state.ModeDax, state.KindPool)
	kept, err := v.e.PrepareGrants(context.Background(), []GrantRequest{poolReq("c", 0, serialA, state.ModeDax, state.KindPool), b})
	if !errors.Is(err, errForeignRegion) {
		t.Fatalf("error %v", err)
	}
	if len(kept) != 1 || kept[0].Ref != (state.GrantRef{Claim: "c"}) || kept[0].State != state.GrantReleasing {
		t.Fatalf("kept %+v", kept)
	}
}

// The rollback of a Prepare of a companion claim revokes the NUMA node that
// a grant of an earlier Prepare lends (recorded before the lend) before its
// memory goes offline.
func TestPoolRollbackRevokesLend(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA}, fake.MemdevSpec{Serial: serialB})
	companion := poolReq("c", 0, serialA, state.ModeNUMA, state.KindPool)
	companion.Lend = &state.GrantLend{Lease: "0123abcd", Pod: state.PodRef{UID: "p1", Namespace: "ns", Name: "pod"}, Container: "ctr"}
	if err := v.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Preparations = append(doc.Preparations, state.Preparation{UID: "c", Driver: poolDriver, ClaimName: "ns/c", Lease: "0123abcd",
			State: state.PreparationPreparing, Results: []state.Result{grantResult("c", 0), grantResult("c", 1)}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{companion}); err != nil {
		t.Fatal(err)
	}
	node := state.NUMANodeOf(v.grant("c", 0).Got.NUMANode)
	if err := v.e.RecordLend(context.Background(), state.GrantRef{Claim: "c"}, node); err != nil {
		t.Fatal(err)
	}
	if l := v.grant("c", 0).Got.Lend; l == nil || state.NUMANodeOf(l.NUMANode) != node {
		t.Fatalf("lend %+v", l)
	}
	k := v.f.Kernel()
	region := string(v.grant("c", 0).Got.Region)
	var revoked []string
	v.e.opts.Revoke = func(_ context.Context, ref state.GrantRef, lend state.GrantLend) error {
		blocks, _ := k.RegionBlocks(region)
		online := len(blocks) > 0 && slices.ContainsFunc(blocks, func(b int) bool { return k.MemoryBlock(b).State == kernel.BlockOnline })
		revoked = append(revoked, fmt.Sprintf("%s %s node %d online %v", ref, lend.Lease, state.NUMANodeOf(lend.NUMANode), online))
		return nil
	}
	v.f.FailCommand("cxl create-region", errors.New("cxl region: cmd_create_region: failed"))
	_, err := v.e.PrepareGrants(context.Background(), []GrantRequest{companion, poolReq("c", 1, serialB, state.ModeDax, state.KindPool)})
	if err == nil {
		t.Fatal("the Prepare did not fail")
	}
	if want := []string{fmt.Sprintf("c/0 0123abcd node %d online true", node)}; !slices.Equal(revoked, want) {
		t.Fatalf("revoked %q, want %q", revoked, want)
	}
	if g := v.records().Grant(state.GrantRef{Claim: "c"}); g != nil {
		t.Fatalf("the lent grant is not released: %+v", g)
	}
}

// A region that a release left disabled when its cxl destroy-region
// failed is enabled and set up again for the next grant of its memdev:
// the Prepare of another claim of the shared device succeeds, the retry of
// the release leaves the region to it.
func TestPoolRegionLeftDisabledIsSetUpAgain(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialC})
	v.configure(func(o *Options) { o.Timeouts.Settle = 300 * time.Millisecond })
	v.mustPrepare(poolDriver, "c1", poolReq("c1", 0, serialC, state.ModeDax, state.KindPoolShared))
	v.f.FailCommand("cxl destroy-region", errors.New("cxl region: cmd_destroy_region: failed"))
	if err := v.unprepare(poolDriver, "c1"); err == nil {
		t.Fatal("the release succeeded")
	}
	v.check("release failed")
	v.mustPrepare(poolDriver, "c2", poolReq("c2", 0, serialC, state.ModeDax, state.KindPoolShared))
	v.mustUnprepare(poolDriver, "c1")
	if g := v.grant("c2", 0); v.f.DaxDriver(g.Got.Dax) != "device_dax" {
		t.Fatalf("dax device %s of c2: driver %q", g.Got.Dax, v.f.DaxDriver(g.Got.Dax))
	}
	v.mustUnprepare(poolDriver, "c2")
	v.clean("unprepared")
}
