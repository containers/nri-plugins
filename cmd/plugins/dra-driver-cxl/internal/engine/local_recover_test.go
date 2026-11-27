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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// logLine returns the first message of the klog output that contains
// text, without the klog header, "" for none.
func logLine(logs, text string) string {
	for _, l := range strings.Split(logs, "\n") {
		if _, msg, ok := strings.Cut(l, "] "); ok && strings.Contains(msg, text) {
			return msg
		}
	}
	return ""
}

// The start keeps the region of a local memdev configured shared, with
// claims or idle; one not configured shared any more stays for its claims
// (a warning), and is released when it is idle, its record dropped.
func TestLocalRecoverShared(t *testing.T) {
	for _, c := range []struct {
		name     string
		claims   bool
		shared   bool
		want     string
		released bool
	}{
		{"configured with claims", true, true, "engine: local memdev 0x11 (mem0): keeping shared region0 and dax0.0, claims [s1]", false},
		{"configured idle", false, true, "engine: local memdev 0x11 (mem0): keeping shared region0 and dax0.0, claims []", false},
		{"not configured with claims", true, false, "engine: local memdev 0x11 (mem0): not in sharedMemdevs any more: keeping region0 for " +
			"claims [s1], not publishing the memdev until they are gone", false},
		{"not configured idle", false, false, "engine: local memdev 0x11 (mem0): not in sharedMemdevs any more, destroying its region", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
			v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
			v.mustPrepare(localDriver, "s1", sharedReq("s1"))
			if !c.claims {
				v.mustUnprepare(localDriver, "s1")
			}
			if !c.shared {
				v.tune = nil
			}
			logs := klogtest.Capture(t, 3)
			v.restart()
			out := logs()
			line := logLine(out, c.want)
			if line == "" {
				t.Fatalf("no %q in\n%s", c.want, out)
			}
			if c.released {
				if released := logLine(out, "destroyed region0 in "); released == "" {
					t.Errorf("no line of the region release in\n%s", out)
				}
				v.clean("released")
				return
			}
			wantRegion(t, v, "region0", state.RegionOwned)
			v.mustUnprepare(localDriver, "s1")
			releaseIdleShared(v)
			v.clean("released")
		})
	}
}

// The start keeps the region of a persistent local memdev with claims, and
// releases an idle one in order when nothing holds its block device: kept
// while a region of another memdev lies above it or a holder holds it.
func TestLocalRecoverPersistent(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib}, fake.MemdevSpec{Serial: serialB})
	v.formatPmem("mem0", "ext4")
	v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
	logs := klogtest.Capture(t, 3)
	v.restart()
	wantLog(t, logs(), "engine: local memdev 0x91 (mem0): keeping persistent region0, claims [w]")
	above := rawRegion(t, v, "mem1")
	v.mustUnprepare(localDriver, "w")
	// idle below a region of another memdev
	logs = klogtest.Capture(t, 3)
	v.restart()
	wantLog(t, logs(), "engine: local memdev 0x91 (mem0): keeping region0 of the idle persistent device: "+above+" lie above it")
	if m := v.records().Memdev(serialP); m == nil || m.State != state.MemdevIdleKept {
		t.Fatalf("memdev %+v", m)
	}
	dropRaw(t, v, above)
	// idle and held
	pmem := filepath.Join(v.f.DevRoot(), "pmem0")
	v.f.SetHolder(pmem, "dm-0")
	logs = klogtest.Capture(t, 3)
	v.restart()
	wantLog(t, logs(), "engine: local memdev 0x91 (mem0): keeping region0 of the idle persistent device: memdev 0x91: "+pmem+" is still in use",
		"engine: local memdev 0x91: "+pmem+" is held: in use")
	wantRegion(t, v, "region0", state.RegionOwned)
	v.f.SetHolder(pmem, "")
	logs = klogtest.Capture(t, 3)
	v.restart()
	wantLog(t, logs(), "engine: local memdev 0x91 (mem0): destroyed region0 in ")
	v.clean("released")
}

// The start releases the region of an unshared local memdev without claims,
// and deletes the parts that belong to no claim first, the memory of a
// numa part offlined.
func TestLocalRecoverIdle(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.mustPrepare(localDriver, "d", sliceReq("d", 0, state.ModeDax, 256*mib))
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeNUMA, 128*mib))
	// the claims are forgotten (another front-end), their parts left
	if err := v.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Grants, doc.Preparations = nil, nil
		m := doc.Memdev(serialL)
		m.Grants, m.State = nil, state.MemdevIdleKept
		for _, dax := range []string{"dax0.0", "dax0.1"} {
			doc.Region("region0").DaxDevice(dax).Users = nil
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logs := klogtest.Capture(t, 3)
	v.restart()
	wantLog(t, logs(), "engine: local memdev 0x11 (mem0): dax0.0 (mode dax, 268435456 bytes) belongs to no claim, deleting it",
		"engine: local memdev 0x11 (mem0): dax0.1 (mode numa, 134217728 bytes) belongs to no claim, deleting it",
		"engine: local memdev 0x11 (mem0): destroyed region0 in ")
	v.clean("released")
}

// The start forgets the parts of prepared claims whose dax device is gone
// or has another size: the grant keeps its region without the device, and
// a device of another size is deleted as belonging to no claim.
func TestLocalRecoverGoneSlices(t *testing.T) {
	for _, c := range []struct {
		name string
		do   func(t *testing.T, v *env)
		want []string
	}{
		{"gone", func(t *testing.T, v *env) {
			if err := v.f.Kernel().DeleteDaxDevice(context.Background(), "region0", "dax0.1"); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"another size", func(t *testing.T, v *env) {
			if err := v.f.Kernel().ResizeDaxDevice(context.Background(), "dax0.1", 128*mib); err != nil {
				t.Fatal(err)
			}
		}, []string{"engine: local memdev 0x11 (mem0): dax0.1 (mode dax, 134217728 bytes) belongs to no claim, deleting it"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
			v.mustPrepare(localDriver, "d1", sliceReq("d1", 0, state.ModeDax, 256*mib))
			v.mustPrepare(localDriver, "d2", sliceReq("d2", 0, state.ModeDax, 256*mib))
			if g := v.grant("d2", 0); g.Got.Dax != "dax0.1" {
				t.Fatalf("grant %+v", g.Got)
			}
			c.do(t, v)
			logs := klogtest.Capture(t, 3)
			v.restart()
			wantLog(t, logs(), append([]string{
				"engine: local memdev 0x11 (mem0): claim d2: slice dax0.1 of 268435456 bytes is gone, dropping it",
				"claim d2: no device node for the dax slice " + filepath.Join(v.f.DevRoot(), "dax0.1") + ": the slice is gone",
			}, c.want...)...)
			if g := v.grant("d2", 0); g.Got.Dax != "" || g.Got.Region != "region0" || g.State != state.GrantRealised {
				t.Fatalf("grant %+v", g)
			}
			if d := v.records().Region("region0").DaxDevice("dax0.1"); d != nil {
				t.Fatalf("record of dax0.1 %+v", d)
			}
			if v.f.DaxExists("dax0.1") {
				t.Fatal("dax0.1 is left")
			}
			if g := v.grant("d1", 0); g.Got.Dax != "dax0.0" {
				t.Fatalf("grant %+v", g)
			}
			v.mustUnprepare(localDriver, "d2")
			v.mustUnprepare(localDriver, "d1")
			v.clean("unprepared")
		})
	}
}

// The uses of a prepared local persistent memdev whose block device is not
// the one of its region now (also of mode block) have no CDI device: the
// grants REALISED_UNRESTORED, the preparations PREPARED_UNRESTORED, the
// node mount left as it is and released with them; a Prepare again stays
// unrestored with the error.
func TestLocalRecoverBadBlocks(t *testing.T) {
	for _, c := range []struct {
		name   string
		req    GrantRequest
		format bool
	}{
		{"mode fs", localPmemReq("w", 0, state.ModeFS, false, ""), true},
		{"mode block", localPmemReq("w", 0, state.ModeBlock, false, ""), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
			if c.format {
				v.formatPmem("mem0", "ext4")
			}
			v.mustPrepare(localDriver, "w", c.req)
			mount := ""
			if m := v.records().MountOf(v.grant("w", 0)); m != nil {
				mount = m.Path
			}
			v.f.RenamePmem("region0", "pmem3", "259:9")
			logs := klogtest.Capture(t, 3)
			out := v.restart()
			if !slices.Equal(out.Unrestored, []state.GrantRef{{Claim: "w"}}) || out.Why[state.GrantRef{Claim: "w"}] == nil {
				t.Fatalf("recovered %+v", out)
			}
			if line := logLine(logs(), "engine: local memdev 0x91 (mem0): "); line == "" {
				t.Fatalf("no error line in\n%s", logs())
			}
			if g := v.grant("w", 0); g.State != state.GrantUnrestored {
				t.Fatalf("grant %s", g.State)
			}
			wantPreparation(t, v, localDriver, "w", state.PreparationUnrestored)
			if mount != "" {
				if mi, _ := v.f.Kernel().Mounted(mount); mi == nil {
					t.Fatalf("%s is not mounted", mount)
				}
			}
			why := out.Why[state.GrantRef{Claim: "w"}].Error()
			if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{c.req}); err == nil || err.Error() != why {
				t.Fatalf("Prepare again: %v, want %q", err, why)
			}
			if g := v.grant("w", 0); g.State != state.GrantUnrestored {
				t.Fatalf("the Prepare again changed the grant: %s", g.State)
			}
			v.mustUnprepare(localDriver, "w")
			if mount != "" {
				if mi, _ := v.f.Kernel().Mounted(mount); mi != nil {
					t.Fatalf("%s is left mounted", mount)
				}
			}
			v.clean("unprepared")
		})
	}
}

// ForgetLocal forgets the records of a local memdev whose region was being
// created (NOTED, or made and cut in its probe) and that is a pool device
// now, without touching the region; a local memdev whose region is made
// stays.
func TestLocalForgetLocal(t *testing.T) {
	for _, c := range []struct {
		name   string
		cut    func(v *env)
		region state.RegionState
	}{
		{"created", func(v *env) { v.f.CutAfter("cxl create-region") }, state.RegionNoted},
		{"probe", func(v *env) { v.f.CutProbe() }, state.RegionOwned},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
			v.configure(func(o *Options) { probeOptions(v, o) })
			v.mustPrepare(localDriver, "b", localReq("b", 0, serialB, state.ModeDax, state.KindLocal, 256*mib))
			c.cut(v)
			if !v.cut(func() { _ = v.prepare(localDriver, "l", sliceReq("l", 0, state.ModeDax, 256*mib)) }) {
				t.Fatal("not cut")
			}
			if reg := v.records().Region("region1"); reg == nil || reg.State != c.region {
				t.Fatalf("region %+v", reg)
			}
			_ = v.st.Close()
			v.open()
			before := v.f.Commands()
			logs := klogtest.Capture(t, 3)
			if err := v.e.ForgetLocal(context.Background(), []state.Serial{serialL, serialB, serialA}); err != nil {
				t.Fatal(err)
			}
			wantLog(t, logs(), "engine: local memdev 0x11 (mem0): its region was being created, and it is a pool device now: forgetting it without "+
				"touching its regions [region1]")
			r := v.records()
			if r.Memdev(serialL) != nil || r.Region("region1") != nil || r.Grant(state.GrantRef{Claim: "l"}) != nil {
				t.Fatalf("records %+v", r)
			}
			if r.Memdev(serialB) == nil || r.Grant(state.GrantRef{Claim: "b"}) == nil {
				t.Fatalf("records of serial 0xb1 %+v", r)
			}
			if cmds := v.f.CommandsSince(before); cmds != "" {
				t.Fatalf("commands %q", cmds)
			}
			if rs := v.f.Kernel().Regions("mem0"); !slices.Equal(rs, []string{"region1"}) {
				t.Fatalf("regions of mem0 %v", rs)
			}
			if _, err := v.e.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if rs := v.f.Kernel().Regions("mem0"); !slices.Equal(rs, []string{"region1"}) {
				t.Fatalf("regions of mem0 after the recovery %v", rs)
			}
			dropRaw(t, v, "region1")
			finish(t, v, "finished")
		})
	}
}

// The start releases the cut Prepares of local memdevs as the recovery
// cells say, with the line of each kind: a part released, also before its
// region was made, a new shared region released, an established one kept
// idle, a persistent region released.
func TestRecoverCutPrepares(t *testing.T) {
	shared := func(v *env) { v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } }) }
	realised := func(v *env) { cutWhenRealised(v, "c") }
	for _, c := range []struct {
		name    string
		specs   []fake.MemdevSpec
		setup   func(v *env)
		cut     func(v *env)
		req     GrantRequest
		line    string
		regions int
	}{
		{"local", []fake.MemdevSpec{{Serial: serialL, Ram: gib}}, nil, realised, sliceReq("c", 0, state.ModeNUMA, 256*mib),
			"engine: local memdev 0x11 (mem0): claim c is not prepared, releasing it", 0},
		{"local before its region", []fake.MemdevSpec{{Serial: serialL, Ram: gib}}, nil, func(v *env) { v.f.CutBefore("cxl create-region") },
			sliceReq("c", 0, state.ModeNUMA, 256*mib), "engine: local memdev 0x11 (mem0): claim c is not prepared, releasing it", 0},
		{"shared new", []fake.MemdevSpec{{Serial: serialL, Ram: gib}}, shared, func(v *env) { v.cutZeroing(true) }, sharedReq("c"),
			"engine: local memdev 0x11 (mem0): claim c of the shared device is not prepared, dropping it", 0},
		{"shared established", []fake.MemdevSpec{{Serial: serialL, Ram: gib}}, func(v *env) {
			shared(v)
			v.mustPrepare(localDriver, "s", sharedReq("s"))
			v.mustUnprepare(localDriver, "s")
		}, realised, sharedReq("c"), "engine: local memdev 0x11 (mem0): claim c of the shared device is not prepared, dropping it", 1},
		{"persistent", []fake.MemdevSpec{{Serial: serialP, Pmem: gib}}, func(v *env) { v.formatPmem("mem0", "ext4") }, realised,
			localPmemReq("c", 0, state.ModeFS, true, ""), "engine: local memdev 0x91 (mem0): claim c of the persistent device is not prepared, dropping it", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, c.specs...)
			if c.setup != nil {
				c.setup(v)
			}
			c.cut(v)
			if !v.cut(func() { _ = v.prepare(localDriver, "c", c.req) }) {
				t.Fatal("not cut")
			}
			logs := klogtest.Capture(t, 3)
			out := v.restart()
			if !slices.Equal(out.Released, []state.GrantRef{{Claim: "c"}}) || len(out.Kept) > 0 {
				t.Fatalf("recovered %+v", out)
			}
			wantLog(t, logs(), c.line, "engine: claim ns/c (c): released, its Prepare again prepares its devices anew")
			if rs := v.f.Kernel().Regions("mem0"); len(rs) != c.regions {
				t.Fatalf("regions %v", rs)
			}
			wantPreparation(t, v, localDriver, "c", state.PreparationPreparing)
			// kubelet's retry prepares anew
			v.mustPrepare(localDriver, "c", c.req)
			v.mustUnprepare(localDriver, "c")
			if strings.HasPrefix(c.name, "shared") {
				releaseIdleShared(v)
			}
			v.clean("unprepared")
		})
	}
}

// A cut Prepare of a local persistent memdev after its namespace was made,
// whose block device something holds at the start: the recovery waits for
// the holder until the holder deadline and keeps the claim (KEPT) with its
// region; once nothing holds the block device, the Unprepare releases them.
func TestLocalRecoverHeldBlockDevice(t *testing.T) {
	const holder = 200 * time.Millisecond
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.configure(func(o *Options) { o.Timeouts.Holder = holder })
	v.f.CutAfter("ndctl create-namespace")
	if !v.cut(func() { _ = v.prepare(localDriver, "b", localPmemReq("b", 0, state.ModeBlock, false, "")) }) {
		t.Fatal("not cut")
	}
	pmem := filepath.Join(v.f.DevRoot(), "pmem0")
	v.f.SetHolder(pmem, "dm-0")
	logs := klogtest.Capture(t, 3)
	start := time.Now()
	out := v.restart()
	took := time.Since(start)
	if !slices.Equal(out.Kept, []state.GrantRef{{Claim: "b"}}) || len(out.Released) > 0 {
		t.Fatalf("recovered %+v", out)
	}
	if took < holder || took > 10*holder {
		t.Fatalf("the start took %v, the holder deadline is %v", took, holder)
	}
	lines := logs()
	kept := logLine(lines, "claim ns/b (b): not everything was released at start (")
	if !strings.Contains(kept, "is still in use") {
		t.Fatalf("line %q does not say that the device is still in use in\n%s", kept, lines)
	}
	wantLog(t, lines, "engine: local memdev 0x91: "+pmem+" is held: in use")
	wantPreparation(t, v, localDriver, "b", state.PreparationKept)
	if g := v.grant("b", 0); g.Intent.Region != "region0" {
		t.Fatalf("grant %+v", g)
	}
	wantRegion(t, v, "region0", state.RegionOwned)
	if rs := v.f.Kernel().Regions("mem0"); !slices.Equal(rs, []string{"region0"}) {
		t.Fatalf("regions %v", rs)
	}
	// the holder is gone: the Unprepare releases the region
	v.f.SetHolder(pmem, "")
	logs = klogtest.Capture(t, 3)
	v.mustUnprepare(localDriver, "b")
	wantLog(t, logs(), "engine: local memdev 0x91 (mem0): destroyed region0 in ")
	v.clean("unprepared")
}

// localBusyCompanionNextToReader prepares the fs claim r and the fs lease
// companion c on the node mount of the shared persistent local memdev
// serialP, then fails the Unprepare of c on its busy lease bind. It returns
// the node mount and the bind.
func localBusyCompanionNextToReader(t *testing.T, v *env) (string, string) {
	t.Helper()
	target := companionTarget(v)
	r := localPmemReq("r", 0, state.ModeFS, true, "")
	r.Facts.Shared = true
	c := localPmemReq("c", 0, state.ModeFS, true, "")
	c.Facts.Shared = true
	c.BindTarget = target
	c.Lend = &state.GrantLend{Lease: "0123abcd", Name: "data"}
	v.mustPrepare(localDriver, "r", r)
	v.mustPrepare(localDriver, "c", c)
	mnt := mountPathOf(v.records(), v.grant("c", 0))
	if mnt == "" || mnt != mountPathOf(v.records(), v.grant("r", 0)) {
		t.Fatalf("node mounts %q and %q, want one", mnt, mountPathOf(v.records(), v.grant("r", 0)))
	}
	v.f.SetBusy(target, true)
	if err := v.unprepare(localDriver, "c"); err == nil {
		t.Fatal("a busy unbind succeeded")
	}
	return mnt, target
}

// localPmemEnvShared returns a node with the persistent local memdev
// serialP, configured shared, of erofs.
func localPmemEnvShared(t *testing.T) *env {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
	v.formatPmem("mem0", "erofs")
	return v
}

// A local companion whose Unprepare failed on its busy lease bind still uses
// its node mount: the Unprepare of another fs claim of the node mount keeps
// it; the Unprepare again of the companion unbinds and unmounts.
func TestLocalReleaseKeepsNodeMountOfBusyCompanion(t *testing.T) {
	v := localPmemEnvShared(t)
	mnt, target := localBusyCompanionNextToReader(t, v)
	v.mustUnprepare(localDriver, "r")
	v.mustBeMounted("after the Unprepare of r", mnt, target)
	v.f.SetBusy(target, false)
	v.mustUnprepare(localDriver, "c")
	v.mustNotBeMounted("after the Unprepare again of c", mnt, target)
}

// A start between the failed Unprepare of a local companion and its
// Unprepare again keeps the companion's node mount and bind.
func TestLocalRecoverKeepsNodeMountOfBusyCompanion(t *testing.T) {
	v := localPmemEnvShared(t)
	mnt, target := localBusyCompanionNextToReader(t, v)
	v.mustUnprepare(localDriver, "r")
	v.restart()
	v.mustBeMounted("after the start", mnt, target)
	v.f.SetBusy(target, false)
	v.mustUnprepare(localDriver, "c")
	v.mustNotBeMounted("after the Unprepare again of c", mnt, target)
}

// The grant of an Unprepare that a stop cut after its region was destroyed
// (UNPREPARING, RELEASING, its region forgotten at the start) holds no
// region: the region that a later claim of the memdev made goes with that
// claim's Unprepare.
func TestLocalReleasingGrantHoldsNoNewRegion(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.mustPrepare(localDriver, "d", sliceReq("d", 0, state.ModeDax, 256*mib))
	v.f.CutAfter("cxl destroy-region")
	if !v.cut(func() { _ = v.unprepare(localDriver, "d") }) {
		t.Fatal("not cut")
	}
	v.restart()
	if g := v.grant("d", 0); g.State != state.GrantReleasing {
		t.Fatalf("grant %+v", g)
	}
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeNUMA, 256*mib))
	v.mustUnprepare(localDriver, "n")
	if regions := v.f.Kernel().Regions("mem0"); len(regions) > 0 {
		t.Fatalf("regions left %v", regions)
	}
	v.mustUnprepare(localDriver, "d")
	v.clean("unprepared")
}

// A region that a start kept for the claim of a cut Prepare, below a newer
// region of another memdev, and that a later start does not keep for it: that
// start releases the claim, which then holds nothing (its Unprepare runs no
// command, its Prepare again prepares it anew). The region of an unshared
// memdev whose region above went goes at that start; a shared one stays with
// the idle memdev, and the Prepare again sets it up and zeroes it once.
func TestLocalKeptRegionReleasedLater(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "local-shared"}[shared], func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
			v.configure(func(o *Options) { probeOptions(v, o) })
			req := sliceReq("u1", 0, state.ModeNUMA, 256*mib)
			if shared {
				v.configure(func(o *Options) { sharedOptions(v, o) })
				req = sharedReq("u1")
			}
			v.f.CutProbe()
			if !v.cut(func() { _ = v.prepare(localDriver, "u1", req) }) {
				t.Fatal("not cut")
			}
			above := rawRegion(t, v, "mem1")
			if out := v.restart(); !slices.Equal(out.Kept, []state.GrantRef{{Claim: "u1"}}) {
				t.Fatalf("recovered %+v", out)
			}
			wantRegion(t, v, "region0", state.RegionKept)
			if !shared {
				dropRaw(t, v, above)
			}
			before := v.f.Commands()
			if out := v.restart(); !slices.Equal(out.Released, []state.GrantRef{{Claim: "u1"}}) || len(out.Kept) > 0 {
				t.Fatalf("recovered at the later start %+v", out)
			}
			want := "cxl disable-region region0\ncxl destroy-region region0"
			if shared {
				want = ""
				if m := v.records().Memdev(serialL); m == nil || m.State != state.MemdevIdleKept || len(m.Grants) > 0 {
					t.Fatalf("memdev %+v", m)
				}
			}
			if cmds := v.f.CommandsSince(before); cmds != want {
				t.Fatalf("commands at the later start %q, want %q", cmds, want)
			}
			before = v.f.Commands()
			v.mustUnprepare(localDriver, "u1")
			if cmds := v.f.CommandsSince(before); cmds != "" {
				t.Fatalf("the Unprepare of the released claim ran %q", cmds)
			}
			wantPreparation(t, v, localDriver, "u1", "")
			v.mustPrepare(localDriver, "u1", req)
			creates := map[bool]int{false: 2, true: 1}[shared]
			if n := strings.Count(v.f.Commands(), "cxl create-region -t ram -d decoder0.0 -m mem0"); n != creates {
				t.Fatalf("%d regions created on mem0, want %d", n, creates)
			}
			if z := v.f.Zeroed(); shared && len(z) != 1 {
				t.Fatalf("zeroed %v, want the region once", z)
			}
			v.mustUnprepare(localDriver, "u1")
			if shared {
				dropRaw(t, v, above)
				releaseIdleShared(v)
			}
			v.clean("unprepared")
		})
	}
}

// A kept region is not set up any more, and a cut release may have
// disabled it: the Unprepare of the claim of a kept region, cut after its
// cxl disable-region, leaves it disabled; the start keeps it again below
// the newer region (the claim's grant RELEASING), and the Prepares of
// another claim and of the kept claim set it up again. A shared device
// that is not configured shared any more, kept for the claim whose other
// result failed, gets all of its region again at the claim's Prepare
// again, zeroed.
func TestLocalKeptRegionSetUpAgain(t *testing.T) {
	for _, c := range []struct {
		name string
		cut  func(v *env)
	}{
		{"probe", func(v *env) { v.f.CutProbe() }},
		{"slice", func(v *env) { v.f.CutAfter("write " + blockState(74) + " online_movable") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
			v.configure(func(o *Options) { probeOptions(v, o) })
			c.cut(v)
			if !v.cut(func() { _ = v.prepare(localDriver, "u1", sliceReq("u1", 0, state.ModeNUMA, 128*mib)) }) {
				t.Fatal("not cut")
			}
			above := rawRegion(t, v, "mem1")
			v.restart()
			wantRegion(t, v, "region0", state.RegionKept)
			v.f.CutAfter("cxl disable-region")
			if !v.cut(func() { _ = v.unprepare(localDriver, "u1") }) {
				t.Fatal("the Unprepare was not cut")
			}
			before := v.f.Commands()
			if out := v.restart(); len(out.Released)+len(out.Kept) > 0 {
				t.Fatalf("recovered %+v", out)
			}
			if cmds := v.f.CommandsSince(before); cmds != "" {
				t.Fatalf("commands at the start %q", cmds)
			}
			wantRegion(t, v, "region0", state.RegionKept)
			if g := v.grant("u1", 0); g.State != state.GrantReleasing {
				t.Fatalf("grant of the kept claim %+v", g)
			}
			before = v.f.Commands()
			v.mustPrepare(localDriver, "u2", sliceReq("u2", 0, state.ModeNUMA, 128*mib))
			if cmds := v.f.CommandsSince(before); cmds != "cxl enable-region region0" {
				t.Fatalf("commands of the Prepare %q", cmds)
			}
			v.mustPrepare(localDriver, "u1", sliceReq("u1", 0, state.ModeDax, 128*mib))
			if n := strings.Count(v.f.Commands(), "cxl create-region -t ram -d decoder0.0 -m mem0"); n != 1 {
				t.Fatalf("%d regions created on mem0", n)
			}
			wantRegion(t, v, "region0", state.RegionOwned)
			if g1, g2 := v.grant("u1", 0), v.grant("u2", 0); g1.Got.Region != "region0" || g2.Got.Region != "region0" {
				t.Fatalf("grants %+v %+v", g1, g2)
			}
			snap, err := v.f.Kernel().Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if online := snap.OnlineBlocks(snap.Region("region0").Range.Blocks(snap.BlockSize)); !slices.Equal(online, []int{74}) {
				t.Fatalf("online blocks %v", online)
			}
			v.mustUnprepare(localDriver, "u1")
			v.mustUnprepare(localDriver, "u2")
			dropRaw(t, v, above)
			v.restart()
			v.clean("unprepared")
		})
	}

	t.Run("no longer shared", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB},
			fake.MemdevSpec{Serial: serialC, Ram: 512 * mib, HostBridge: 24})
		shared := true
		v.configure(func(o *Options) {
			probeOptions(v, o)
			o.Shared = func(s state.Serial) bool { return shared && s == serialL }
		})
		regionAboveNext(t, v, "mem1")
		v.f.Before("cxl create-region -t ram -d decoder0.1", func() { shared = false })
		v.f.FailCommand("cxl create-region -t ram -d decoder0.1", errors.New("cxl region: cmd_create_region: failed"))
		if err := v.prepare(localDriver, "u1", sharedReq("u1"), localReq("u1", 1, serialC, state.ModeDax, state.KindLocal, 256*mib)); err == nil {
			t.Fatal("the Prepare succeeded")
		}
		wantRegion(t, v, "region0", state.RegionKept)
		wantPreparation(t, v, localDriver, "u1", state.PreparationKept)
		v.mustPrepare(localDriver, "u1", sharedReq("u1"))
		if g := v.grant("u1", 0); g.Got.Region != "region0" || g.Got.Size != gib {
			t.Fatalf("grant %+v, want all of region0", g)
		}
		if z := v.f.Zeroed(); len(z) != 2 {
			t.Fatalf("zeroed %v, want the region twice", z)
		}
		v.mustUnprepare(localDriver, "u1")
		dropRaw(t, v, "region1")
		v.restart()
		v.clean("unprepared")
	})
}

// A kept region that a Prepare enabled again is kept again by the start
// after a stop before its set-up: its daxN.0 is the Whole device of a kept
// region, and the Prepare again of the claim sets the region up.
func TestLocalKeptRegionEnabledAgainCut(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
	v.mustPrepare(localDriver, "d", sliceReq("d", 0, state.ModeDax, 256*mib))
	v.f.FailCommand("cxl destroy-region", errors.New("cxl region: cmd_destroy_region: failed"))
	if err := v.unprepare(localDriver, "d"); err == nil {
		t.Fatal("the release succeeded")
	}
	v.mustPrepare(poolDriver, "b", poolReq("b", 0, serialB, state.ModeDax, state.KindPool))
	v.f.CutAfter("cxl enable-region")
	if !v.cut(func() { _ = v.prepare(localDriver, "n", sliceReq("n", 0, state.ModeNUMA, 256*mib)) }) {
		t.Fatal("not cut")
	}
	v.restart()
	wantPreparation(t, v, localDriver, "n", state.PreparationKept)
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeNUMA, 256*mib))
	v.mustUnprepare(localDriver, "n")
	v.mustUnprepare(localDriver, "d")
	v.mustUnprepare(poolDriver, "b")
	v.clean("unprepared")
}

// The grant of a reader of mode block of an Unprepare that a stop cut after
// its region was destroyed (its region forgotten at the start) holds no
// region: the region that a later claim of the memdev made goes with that
// claim's Unprepare.
func TestLocalReleasingBlockGrantHoldsNoNewRegion(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.formatPmem("mem0", "ext4")
	v.mustPrepare(localDriver, "b", localPmemReq("b", 0, state.ModeBlock, true, ""))
	v.f.CutAfter("cxl destroy-region")
	if !v.cut(func() { _ = v.unprepare(localDriver, "b") }) {
		t.Fatal("not cut")
	}
	v.restart()
	if g := v.grant("b", 0); g.State != state.GrantReleasing || g.Got.Region != "" {
		t.Fatalf("grant %+v", g)
	}
	v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeBlock, true, ""))
	v.mustUnprepare(localDriver, "r")
	if regions := v.f.Kernel().Regions("mem0"); len(regions) > 0 {
		t.Fatalf("regions left %v", regions)
	}
	v.mustUnprepare(localDriver, "b")
	v.clean("unprepared")
}

// A persistent local memdev whose Prepare a process stop cut once its
// region was made is not forgotten when it is a pool device now: only a ram
// region is in the making until its first part; the recovery releases the
// region of the cut Prepare.
func TestLocalForgetLocalPersistent(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.f.CutAfter("ndctl create-namespace")
	if !v.cut(func() {
		_ = v.prepare(localDriver, "p", localReq("p", 0, serialP, state.ModeBlock, state.KindLocalPersistent, 0))
	}) {
		t.Fatal("not cut")
	}
	reg := ownRegion(v.records(), serialP, state.RegionPmem)
	if reg == nil || reg.State != state.RegionOwned {
		t.Fatalf("region %+v", reg)
	}
	_ = v.st.Close()
	v.open()
	logs := klogtest.Capture(t, 3)
	if err := v.e.ForgetLocal(context.Background(), []state.Serial{serialP}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs(), "forgetting it without touching its regions") {
		t.Fatalf("the persistent memdev forgotten:\n%s", logs())
	}
	if v.records().Memdev(serialP) == nil {
		t.Fatal("the record of the persistent memdev is gone")
	}
	before := v.f.Commands()
	if _, err := v.e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cmds := v.f.CommandsSince(before); !strings.Contains(cmds, "cxl destroy-region "+string(reg.Ref())) {
		t.Fatalf("the region of the cut Prepare is not released:\n%s", cmds)
	}
	finish(t, v, "recovered")
}
