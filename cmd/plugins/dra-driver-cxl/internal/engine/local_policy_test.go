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
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The release policy of a local memdev: its Unprepare destroys the
// region also below a newer region of another memdev (a hole); a rollback
// and a recovery keep it below one (KEPT, the grant JOURNALED, the
// preparation KEPT).
func TestPolicyLocal(t *testing.T) {
	t.Run("unprepare destroys below a newer region", func(t *testing.T) {
		v := newEnv(t, stackSpecs...)
		v.mustPrepare(localDriver, "l", sliceReq("l", 0, state.ModeDax, 256*mib))
		v.mustPrepare(poolDriver, "b", poolReq("b", 0, serialB, state.ModeDax, state.KindPool))
		wantStack(t, v, serialL, serialB)
		v.mustUnprepare(localDriver, "l")
		wantStack(t, v, 0, serialB)
		if m := v.records().Memdev(serialL); m != nil {
			t.Fatalf("memdev %+v", m)
		}
		v.mustUnprepare(poolDriver, "b")
		v.clean("unprepared")
	})
	t.Run("rollback keeps below a newer region", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
		regionAboveNext(t, v, "mem1")
		v.f.FailWrite("bus/dax/devices/dax0.0/size", syscall.EBUSY)
		logs := klogtest.Capture(t, 3)
		if err := v.prepare(localDriver, "k", sliceReq("k", 0, state.ModeDax, 256*mib)); err == nil {
			t.Fatal("the Prepare succeeded")
		}
		v.check("rolled back")
		reg := wantRegion(t, v, "region0", state.RegionKept)
		if reg.SetUp || reg.Zeroed {
			t.Fatalf("kept region %+v", reg)
		}
		if g := v.grant("k", 0); g.State != state.GrantJournaled || g.Intent.Region != "region0" || g.Got.Region != "" {
			t.Fatalf("grant %+v", g)
		}
		wantPreparation(t, v, localDriver, "k", state.PreparationKept)
		wantLog(t, logs(), "engine: local memdev 0x11 (mem0): keeping region0 for claims [k], whose Prepare did not finish: region1 lie above it")
		dropRaw(t, v, "region1")
		v.mustPrepare(localDriver, "k", sliceReq("k", 0, state.ModeDax, 256*mib))
		if g := v.grant("k", 0); g.Got.Region != "region0" {
			t.Fatalf("grant %+v", g.Got)
		}
		v.mustUnprepare(localDriver, "k")
		v.clean("unprepared")
	})
	t.Run("recovery keeps below a newer region", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
		// cut after the commit of the part, before the claim is prepared
		cutWhenRealised(v, "n")
		if !v.cut(func() { _ = v.prepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib)) }) {
			t.Fatal("not cut")
		}
		above := rawRegion(t, v, "mem1")
		out := v.restart()
		if !slices.Equal(out.Kept, []state.GrantRef{{Claim: "n"}}) || len(out.Released) > 0 {
			t.Fatalf("recovered %+v", out)
		}
		wantRegion(t, v, "region0", state.RegionKept)
		if g := v.grant("n", 0); g.State != state.GrantJournaled || g.Intent.Region != "region0" || g.Got.Dax != "" {
			t.Fatalf("grant %+v", g)
		}
		if size := v.f.DaxSize("dax0.0"); size != 0 {
			t.Fatalf("the part of the claim is left: dax0.0 of %d bytes", size)
		}
		wantPreparation(t, v, localDriver, "n", state.PreparationKept)
		dropRaw(t, v, above)
		v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
		v.mustUnprepare(localDriver, "n")
		v.clean("unprepared")
	})
}

// The release policy of a local memdev configured shared whose region is
// established: its Unprepare, a rollback and a recovery keep the region for
// the idle memdev (IDLE_KEPT), the Whole device stays, and the next grant
// does not zero it again. A Prepare that cannot read the dax device of the
// kept region keeps it too; a kept region that another tool released is
// forgotten, and a new one is made and zeroed.
func TestLocalPolicySharedConfigured(t *testing.T) {
	shared := func(v *env, o *Options) { o.Shared = func(state.Serial) bool { return true } }
	newShared := func(t *testing.T) *env {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialA, HostBridge: 24})
		v.configure(func(o *Options) { shared(v, o) })
		return v
	}
	idleKept := func(t *testing.T, v *env) {
		t.Helper()
		if m := v.records().Memdev(serialL); m == nil || m.State != state.MemdevIdleKept {
			t.Fatalf("memdev %+v", m)
		}
		reg := wantRegion(t, v, "region0", state.RegionOwned)
		if d := reg.WholeDevice(); d == nil || d.Name != "dax0.0" || d.State != state.DaxDeviceDax || len(d.Users) > 0 || !reg.Zeroed {
			t.Fatalf("region %+v", reg)
		}
		if v.f.DaxDriver("dax0.0") != kernel.DriverDeviceDax {
			t.Fatalf("dax0.0 is bound to %q", v.f.DaxDriver("dax0.0"))
		}
	}
	notZeroedAgain := func(t *testing.T, v *env) {
		t.Helper()
		v.mustPrepare(localDriver, "s9", sharedReq("s9"))
		if z := v.f.Zeroed(); len(z) != 1 {
			t.Fatalf("zeroed %v", z)
		}
		v.mustUnprepare(localDriver, "s9")
		releaseIdleShared(v)
		v.clean("released")
	}
	t.Run("unprepare", func(t *testing.T) {
		v := newShared(t)
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		logs := klogtest.Capture(t, 3)
		v.mustUnprepare(localDriver, "s1")
		idleKept(t, v)
		wantLog(t, logs(), "engine: local memdev 0x11 (mem0): claim s1: released shared dax0.0, claims left []",
			"engine: local memdev 0x11 (mem0): keeping region0 and dax0.0 of the idle shared device")
		notZeroedAgain(t, v)
	})
	t.Run("rollback", func(t *testing.T) {
		v := newShared(t)
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		v.mustUnprepare(localDriver, "s1")
		v.f.FailCommand("cxl create-region", errors.New("cxl region: cmd_create_region: failed"))
		if err := v.prepare(localDriver, "s2", sharedReq("s2"), poolReq("s2", 1, serialA, state.ModeDax, state.KindPool)); err == nil {
			t.Fatal("the Prepare succeeded")
		}
		wantPreparation(t, v, localDriver, "s2", "")
		v.check("rolled back")
		idleKept(t, v)
		notZeroedAgain(t, v)
	})
	t.Run("recovery", func(t *testing.T) {
		v := newShared(t)
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		cutWhenRealised(v, "s2")
		if !v.cut(func() { _ = v.prepare(localDriver, "s2", sharedReq("s2")) }) {
			t.Fatal("not cut")
		}
		logs := klogtest.Capture(t, 3)
		out := v.restart()
		if !slices.Equal(out.Released, []state.GrantRef{{Claim: "s2"}}) || len(out.Kept) > 0 {
			t.Fatalf("recovered %+v", out)
		}
		wantLog(t, logs(), "engine: local memdev 0x11 (mem0): claim s2 of the shared device is not prepared, dropping it",
			"engine: local memdev 0x11 (mem0): keeping shared region0 and dax0.0, claims [s1]")
		v.mustUnprepare(localDriver, "s1")
		idleKept(t, v)
		// the recovery of the idle memdev keeps it too
		v.restart()
		idleKept(t, v)
		notZeroedAgain(t, v)
	})
	t.Run("dax device unreadable", func(t *testing.T) {
		v := newShared(t)
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		v.mustUnprepare(localDriver, "s1")
		dev := v.f.Read("bus/dax/devices/dax0.0/dev")
		v.f.Write("bus/dax/devices/dax0.0/dev", "bogus")
		err := v.prepare(localDriver, "s2", sharedReq("s2"))
		if want := `memdev 0x11 (mem0): dax0.0: dax0.0: cannot parse device numbers "bogus"`; err == nil || err.Error() != want {
			t.Fatalf("error %v, want %q", err, want)
		}
		wantPreparation(t, v, localDriver, "s2", "")
		v.f.Write("bus/dax/devices/dax0.0/dev", dev)
		idleKept(t, v)
		notZeroedAgain(t, v)
	})
	t.Run("region gone", func(t *testing.T) {
		v := newShared(t)
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		v.mustUnprepare(localDriver, "s1")
		if err := v.f.Kernel().DestroyRegion(context.Background(), "region0"); err != nil {
			t.Fatal(err)
		}
		logs := klogtest.Capture(t, 3)
		v.mustPrepare(localDriver, "s2", sharedReq("s2"))
		wantLog(t, logs(), "engine: local memdev 0x11 (mem0): region0 of the shared device is gone, forgetting it and its claims []")
		if z := v.f.Zeroed(); len(z) != 2 {
			t.Fatalf("zeroed %v", z)
		}
		v.mustUnprepare(localDriver, "s2")
		releaseIdleShared(v)
		v.clean("released")
	})
}

// A new region of a local shared memdev whose set-up or zeroing failed is
// released by the rollback in order (KEPT for the claim below a newer
// region of another memdev); the next Prepare zeroes it again.
func TestLocalPolicySharedNew(t *testing.T) {
	zeroErr := errors.New("zeroing failed")
	t.Run("released", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
		v.f.ZeroErr = zeroErr
		err := v.prepare(localDriver, "s1", sharedReq("s1"))
		if err == nil || !strings.Contains(err.Error(), "memdev 0x11 (mem0): zeroing new shared dax0.0: ") {
			t.Fatalf("error %v", err)
		}
		v.check("rolled back")
		v.clean("rolled back")
		v.f.ZeroErr = nil
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		if z := v.f.Zeroed(); len(z) != 1 {
			t.Fatalf("zeroed %v", z)
		}
		v.mustUnprepare(localDriver, "s1")
		releaseIdleShared(v)
		v.clean("released")
	})
	t.Run("kept below a newer region", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
		v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
		regionAboveNext(t, v, "mem1")
		v.f.ZeroErr = zeroErr
		if err := v.prepare(localDriver, "s1", sharedReq("s1")); err == nil {
			t.Fatal("the Prepare succeeded")
		}
		v.check("rolled back")
		reg := wantRegion(t, v, "region0", state.RegionKept)
		if reg.Zeroed || reg.SetUp {
			t.Fatalf("region %+v", reg)
		}
		if g := v.grant("s1", 0); g.State != state.GrantJournaled || g.Intent.Region != "region0" {
			t.Fatalf("grant %+v", g)
		}
		wantPreparation(t, v, localDriver, "s1", state.PreparationKept)
		v.f.ZeroErr = nil
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		if z := v.f.Zeroed(); len(z) != 1 {
			t.Fatalf("zeroed %v", z)
		}
		dropRaw(t, v, "region1")
		v.mustUnprepare(localDriver, "s1")
		releaseIdleShared(v)
		v.clean("released")
	})
	t.Run("set-up failed", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
		v.f.FailWrite("bus/dax/drivers/kmem/unbind", syscall.EBUSY)
		if err := v.prepare(localDriver, "s1", sharedReq("s1")); err == nil {
			t.Fatal("the Prepare succeeded")
		}
		v.check("rolled back")
		v.clean("rolled back")
	})
	t.Run("cut after its zeroing", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
		v.cutZeroing(true)
		if !v.cut(func() { _ = v.prepare(localDriver, "s1", sharedReq("s1")) }) {
			t.Fatal("not cut")
		}
		out := v.restart()
		if !slices.Equal(out.Released, []state.GrantRef{{Claim: "s1"}}) {
			t.Fatalf("recovered %+v", out)
		}
		wantRegion(t, v, "region0", "")
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		if z := v.f.Zeroed(); len(z) != 2 {
			t.Fatalf("zeroed %v", z)
		}
		v.mustUnprepare(localDriver, "s1")
		releaseIdleShared(v)
		v.clean("released")
	})
}

// A local memdev used shared that is not configured shared any more: the
// Unprepare of its last claim and the recovery destroy its region, also
// below a newer region of another memdev.
func TestPolicyLocalSharedUnlisted(t *testing.T) {
	t.Run("unprepare", func(t *testing.T) {
		v := newEnv(t, stackSpecs...)
		shared := true
		v.e.opts.Shared = func(state.Serial) bool { return shared }
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		v.mustPrepare(poolDriver, "b", poolReq("b", 0, serialB, state.ModeDax, state.KindPool))
		shared = false
		v.mustUnprepare(localDriver, "s1")
		wantStack(t, v, 0, serialB)
		v.mustUnprepare(poolDriver, "b")
		v.clean("unprepared")
	})
	t.Run("recovery", func(t *testing.T) {
		v := newEnv(t, stackSpecs...)
		v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		v.mustUnprepare(localDriver, "s1")
		v.mustPrepare(poolDriver, "b", poolReq("b", 0, serialB, state.ModeDax, state.KindPool))
		v.configure(nil)
		cutWhenRealised(v, "s2")
		if !v.cut(func() { _ = v.prepare(localDriver, "s2", sharedReq("s2")) }) {
			t.Fatal("not cut")
		}
		out := v.restart()
		if !slices.Equal(out.Released, []state.GrantRef{{Claim: "s2"}}) {
			t.Fatalf("recovered %+v", out)
		}
		wantStack(t, v, 0, serialB)
		if m := v.records().Memdev(serialL); m != nil {
			t.Fatalf("memdev %+v", m)
		}
		v.mustUnprepare(poolDriver, "b")
		finish(t, v, "finished")
	})
}

// The release policy of a local persistent memdev: its Unprepare releases
// the region in order, kept for the idle memdev below a newer region of
// another memdev; a rollback keeps it there for the claim (KEPT); a
// recovery releases it in order, kept for the idle memdev.
func TestLocalPolicyPersistent(t *testing.T) {
	newPmem := func(t *testing.T) *env {
		v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib}, fake.MemdevSpec{Serial: serialB})
		v.formatPmem("mem0", "ext4")
		return v
	}
	idle := func(t *testing.T, v *env) {
		t.Helper()
		if m := v.records().Memdev(serialP); m == nil || m.State != state.MemdevIdleKept || len(m.Grants) > 0 {
			t.Fatalf("memdev %+v", m)
		}
		wantRegion(t, v, "region0", state.RegionOwned)
	}
	t.Run("unprepare", func(t *testing.T) {
		v := newPmem(t)
		v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
		above := rawRegion(t, v, "mem1")
		logs := klogtest.Capture(t, 3)
		v.mustUnprepare(localDriver, "w")
		idle(t, v)
		wantLog(t, logs(), "engine: local memdev 0x91 (mem0): keeping region0 of the idle persistent device: "+above+" lie above it")
		dropRaw(t, v, above)
		// the start releases the idle region in order
		v.restart()
		wantRegion(t, v, "region0", "")
		v.clean("released")
	})
	t.Run("rollback", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib}, fake.MemdevSpec{Serial: serialB})
		regionAboveNext(t, v, "mem1")
		// a reader of a device that no writer formatted fails
		if err := v.prepare(localDriver, "r", localPmemReq("r", 0, state.ModeFS, true, "")); err == nil {
			t.Fatal("the Prepare succeeded")
		}
		v.check("rolled back")
		wantRegion(t, v, "region0", state.RegionKept)
		if g := v.grant("r", 0); g.State != state.GrantJournaled || g.Intent.Region != "region0" {
			t.Fatalf("grant %+v", g)
		}
		wantPreparation(t, v, localDriver, "r", state.PreparationKept)
		dropRaw(t, v, "region1")
		v.mustUnprepare(localDriver, "r")
		v.clean("unprepared")
	})
	t.Run("recovery", func(t *testing.T) {
		v := newPmem(t)
		cutWhenRealised(v, "w")
		if !v.cut(func() { _ = v.prepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, "")) }) {
			t.Fatal("not cut")
		}
		above := rawRegion(t, v, "mem1")
		out := v.restart()
		if !slices.Equal(out.Released, []state.GrantRef{{Claim: "w"}}) || len(out.Kept) > 0 {
			t.Fatalf("recovered %+v", out)
		}
		idle(t, v)
		wantPreparation(t, v, localDriver, "w", state.PreparationPreparing)
		dropRaw(t, v, above)
		v.mustUnprepare(localDriver, "w")
		idle(t, v)
		// the next start releases the idle region
		v.restart()
		v.clean("released")
	})
}

// The release policy of the pool devices is not changed by the local
// cells: a rollback destroys the region also below a newer region of
// another memdev.
func TestPoolPolicyRollbackDestroysBelow(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA}, fake.MemdevSpec{Serial: serialB})
	port := v.f.Kernel().Memdev(serialA).Ports[0]
	regionAboveNext(t, v, "mem1")
	v.f.FailWrite("bus/dax/drivers/kmem/unbind", syscall.EBUSY)
	if err := v.prepare(poolDriver, "a", poolReq("a", 0, serialA, state.ModeDax, state.KindPool)); err == nil {
		t.Fatal("the Prepare succeeded")
	}
	v.check("rolled back")
	if rs := v.f.Kernel().Regions("mem0"); len(rs) > 0 {
		t.Fatalf("regions %v left", rs)
	}
	wantPreparation(t, v, poolDriver, "a", "")
	if got := v.f.Stack(port); !slices.Equal(got, []string{"", "region1"}) {
		t.Fatalf("stack %q", got)
	}
	dropRaw(t, v, "region1")
	v.clean("rolled back")
}

// The retry of the release of a local persistent memdev whose cxl
// destroy-region failed keeps the region idle when a region of another
// memdev came above it meanwhile: the region is KEPT (disabled), set up
// again by the next Prepare; the start destroys it once nothing is above.
func TestLocalPersistentRetryKeepsDisabledRegion(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib}, fake.MemdevSpec{Serial: serialC})
	v.formatPmem("mem0", "ext4")
	v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
	region := v.grant("w", 0).Got.Region
	v.f.FailCommand("cxl destroy-region", errors.New("cxl region: cmd_destroy_region: failed"))
	if err := v.unprepare(localDriver, "w"); err == nil {
		t.Fatal("the release succeeded")
	}
	v.check("release failed")
	v.mustPrepare(poolDriver, "c", poolReq("c", 0, serialC, state.ModeDax, state.KindPool))
	v.mustUnprepare(localDriver, "w")
	wantRegion(t, v, string(region), state.RegionKept)
	v.mustUnprepare(poolDriver, "c")
	v.restart()
	v.clean("restarted")
}

// The next Prepare of a local persistent memdev whose region was kept idle
// below a newer region that is gone now destroys the idle region first and
// makes it anew: the hole below it is reaped.
func TestLocalPersistentIdleLeftoverAtPrepare(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialB}, fake.MemdevSpec{Serial: serialP, Pmem: gib}, fake.MemdevSpec{Serial: serialC})
	v.formatPmem("mem1", "ext4")
	v.mustPrepare(poolDriver, "b", poolReq("b", 0, serialB, state.ModeDax, state.KindPool))
	v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
	v.mustPrepare(poolDriver, "c", poolReq("c", 0, serialC, state.ModeDax, state.KindPool))
	v.mustUnprepare(localDriver, "w")
	v.mustUnprepare(poolDriver, "b")
	v.mustUnprepare(poolDriver, "c")
	port := v.f.Kernel().Memdev(serialP).Ports[0]
	if st := v.f.Stack(port); len(st) != 2 || st[0] != "" {
		t.Fatalf("stack %q, want a hole below the idle region", st)
	}
	v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
	if st := v.f.Stack(port); slices.Contains(st, "") {
		t.Fatalf("stack %q: the hole below the idle region stayed", st)
	}
	v.mustUnprepare(localDriver, "w")
	v.clean("unprepared")
}
