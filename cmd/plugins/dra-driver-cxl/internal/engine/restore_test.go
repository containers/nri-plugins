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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The start makes the superblock of a node mount and the read-only flag of its
// block device what the prepared uses derive: a node mount that is gone is
// mounted again read-write for a writer, the block device that was left
// read-only made writable first; a read-write superblock without a writer (the
// Prepare of the writer cut after its remount read-write) is made read-only,
// then the block device; a read-only one under a writer is made read-write,
// the block device first. A node mount that is gone and whose block device is
// another one now is not mounted again, with the line that the observer
// counts.
func TestRecoverRestoresFlags(t *testing.T) {
	for _, source := range sources {
		t.Run(string(source), func(t *testing.T) {
			driver := driverOf(source)
			v := pmemEnv(t, "ext4")
			k := v.f.Kernel()
			pmem := filepath.Join(v.f.DevRoot(), "pmem0")
			wantFlags := func(what, mnt string, readOnly bool) {
				t.Helper()
				mi, err := k.Mounted(mnt)
				if err != nil || mi == nil || mi.SuperReadOnly() != readOnly || v.f.IsReadOnly(pmem) != readOnly {
					t.Fatalf("%s: node mount %+v (%v), block device read-only %v, want read-only %v", what, mi, err, v.f.IsReadOnly(pmem), readOnly)
				}
			}
			v.mustPrepare(driver, "w", fsUse(source, "w", false, ""))
			v.mustPrepare(driver, "r", fsUse(source, "r", true, ""))
			direct := v.mount("w", 0).Path

			// gone, its block device read-only: mounted again read-write
			if err := k.Unmount(direct); err != nil {
				t.Fatal(err)
			}
			if _, err := k.SetBlockDeviceReadOnly(pmem, true); err != nil {
				t.Fatal(err)
			}
			n := mountCalls(v)
			v.restart()
			wantMountCalls(t, v, n, "blkroset "+pmem+" 0", "fsmount "+pmem+" "+direct+" ext4 rw dax=always")
			wantFlags("mounted again", direct, false)

			// the writer is gone, its Prepare again cut after the remount read-write:
			// made read-only
			v.mustUnprepare(driver, "w")
			v.f.CutMount(direct)
			if !v.cut(func() { _ = v.prepare(driver, "w", fsUse(source, "w", false, "")) }) {
				t.Fatal("not cut")
			}
			wantFlags("cut", direct, false)
			n = mountCalls(v)
			if out := v.restart(); !slices.Equal(out.Released, []state.GrantRef{{Claim: "w"}}) {
				t.Fatalf("recovered %+v", out)
			}
			wantMountCalls(t, v, n, "syncfs "+direct, "remount "+direct+" ro", "blkroset "+pmem+" 1")
			wantFlags("remounted read-only", direct, true)

			// a writer again, the superblock and the block device made
			// read-only behind it: made read-write
			v.mustPrepare(driver, "w", fsUse(source, "w", false, ""))
			if err := k.Remount(direct, false); err != nil {
				t.Fatal(err)
			}
			if _, err := k.SetBlockDeviceReadOnly(pmem, true); err != nil {
				t.Fatal(err)
			}
			n = mountCalls(v)
			v.restart()
			wantMountCalls(t, v, n, "blkroset "+pmem+" 0", "remount "+direct+" rw")
			wantFlags("remounted read-write", direct, false)
			if viol := v.f.Violations(); len(viol) > 0 {
				t.Fatalf("violations %v", viol)
			}

			// gone, and the block device is another one: not mounted again
			if err := k.Unmount(direct); err != nil {
				t.Fatal(err)
			}
			v.f.RenamePmem("region0", "pmem3", "259:9")
			logs := klogtest.Capture(t, 3)
			n = mountCalls(v)
			out := v.restart()
			wantMountCalls(t, v, n)
			if !slices.Equal(out.Unrestored, []state.GrantRef{{Claim: "r"}, {Claim: "w"}}) {
				t.Fatalf("recovered %+v", out)
			}
			changed := "memdev 0x91: its fsdax namespace is namespace0.0 with " + filepath.Join(v.f.DevRoot(), "pmem3") +
				" (259:9), not namespace0.0 with " + pmem + " (259:1) as prepared"
			// a pool claim is unrestored for its node mount, with the line
			// that the observer counts; a local claim for its block device,
			// with the line of its memdev only
			want := "not restoring " + direct + ": " + changed
			line, notRemounted := "engine: claim ns/r: "+want, true
			if source == state.SourceLocal {
				want, line, notRemounted = changed, "engine: local memdev 0x91 (mem0): "+changed, false
			}
			for _, ref := range out.Unrestored {
				if why := out.Why[ref]; why == nil || why.Error() != want {
					t.Errorf("%s unrestored for %v, want %q", ref, why, want)
				}
			}
			lines := logs()
			wantLog(t, lines, line)
			counted := ""
			for _, l := range strings.Split(lines, "\n") {
				if (strings.Contains(l, "not restoring") || strings.Contains(l, "not binding")) && counted == "" {
					counted = l
				}
			}
			switch {
			case notRemounted && counted == "":
				t.Errorf("no line that the observer counts in\n%s", lines)
			case !notRemounted && counted != "":
				t.Errorf("the start logged %q, which the observer counts", counted)
			}
			if mi, _ := k.Mounted(direct); mi != nil {
				t.Fatalf("mounted again: %+v", mi)
			}
			v.mustUnprepare(driver, "w")
			v.mustUnprepare(driver, "r")
			v.clean("unprepared")
		})
	}
}

// A PREPARED claim whose block device is another one now: its grant is
// REALISED_UNRESTORED and its preparation PREPARED_UNRESTORED; a Prepare
// again fails while it does not verify, its Unprepare releases it.
func TestRecoverUnrestored(t *testing.T) {
	v := pmemEnv(t, "ext4")
	v.mustPrepare(poolDriver, "w", fsGrant("w", false, ""))
	direct := v.mount("w", 0).Path
	if err := v.f.Kernel().Unmount(direct); err != nil {
		t.Fatal(err)
	}
	v.f.RenamePmem("region0", "pmem3", "259:9")
	out := v.restart()
	if !slices.Equal(out.Unrestored, []state.GrantRef{{Claim: "w"}}) {
		t.Fatalf("recovered %+v", out)
	}
	if g := v.grant("w", 0); g.State != state.GrantUnrestored {
		t.Fatalf("grant %s", g.State)
	}
	if p := v.records().Preparation(poolDriver, "w"); p.State != state.PreparationUnrestored {
		t.Fatalf("preparation %s", p.State)
	}
	if m := v.records().Region("region0").Mount(state.AccessDirect); m.State != state.MountUnrestored {
		t.Fatalf("mount %+v", m)
	}
	v.check("unrestored")
	// a later start that finds the other block device still leaves both so
	ref := state.GrantRef{Claim: "w"}
	if out := v.restart(); len(out.Unrestored) > 0 || out.Why[ref] == nil {
		t.Fatalf("recovered again %+v", out)
	}
	if p := v.records().Preparation(poolDriver, "w"); p.State != state.PreparationUnrestored {
		t.Fatalf("preparation again %s", p.State)
	}
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{fsGrant("w", false, "")}); err == nil ||
		!strings.Contains(err.Error(), "cannot be restored") {
		t.Fatalf("Prepare again: %v", err)
	}
	if g := v.grant("w", 0); g.State != state.GrantUnrestored {
		t.Fatalf("the Prepare again released the grant: %s", g.State)
	}
	// the release waits for the holders of the block device of the region
	// now, not of the one as prepared
	logs := klogtest.Capture(t, 3)
	v.mustUnprepare(poolDriver, "w")
	want := fmt.Sprintf("claim ns/w: its node mount %s was not restored at start; the release of pool memdev 0x91 waits for the holders of %s, the block device of its region now, not of %s",
		direct, filepath.Join(v.f.DevRoot(), "pmem3"), filepath.Join(v.f.DevRoot(), "pmem0"))
	if out := logs(); !strings.Contains(out, want) {
		t.Fatalf("no %q in\n%s", want, out)
	}
	v.clean("unprepared")
}

// An unrestored grant is mounted again by a Prepare again once its block
// device is the one prepared (REALISED, its mount MOUNTED_RW), or at the
// next start (a reader, MOUNTED_RO).
func TestRecoverRestored(t *testing.T) {
	for _, writer := range []bool{true, false} {
		v := pmemEnv(t, "ext4")
		v.mustPrepare(poolDriver, "c", fsGrant("c", !writer, ""))
		g, mnt := v.grant("c", 0), v.mount("c", 0).Path
		if err := v.f.Kernel().Unmount(mnt); err != nil {
			t.Fatal(err)
		}
		v.f.RenamePmem("region0", "pmem3", "259:9")
		if out := v.restart(); len(out.Unrestored) != 1 {
			t.Fatalf("recovered %+v", out)
		}
		v.f.RenamePmem("region0", "pmem0", "259:1")
		if writer {
			if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{fsGrant("c", false, "")}); err != nil {
				t.Fatal(err)
			}
		} else if out := v.restart(); len(out.Restored) != 1 {
			t.Fatalf("restored %+v", out)
		}
		want := state.MountedRO
		if writer {
			want = state.MountedRW
		}
		if g := v.grant("c", 0); g.State != state.GrantRealised {
			t.Fatalf("grant %s", g.State)
		}
		if m := v.records().Region(g.Got.Region).Mount(g.Got.Mount); m.State != want {
			t.Fatalf("mount %+v", m)
		}
		v.mustUnprepare(poolDriver, "c")
	}
}

// An fs companion grant whose node mount and lease bind a new mount
// namespace lost, and whose node mount a start cannot mount again: a
// Prepare again that mounts the node mount binds it again.
func TestRestoredBind(t *testing.T) {
	v := pmemEnv(t, "ext4")
	req := fsGrant("c", true, "")
	req.BindTarget = filepath.Join(v.dir, localDriver, "lease-fs", "lc1", "data")
	v.mustPrepare(poolDriver, "c", req)
	k := v.f.Kernel()
	for _, mnt := range []string{req.BindTarget, v.mount("c", 0).Path} {
		if err := k.Unmount(mnt); err != nil {
			t.Fatal(err)
		}
	}
	v.f.RenamePmem("region0", "pmem3", "259:9")
	if out := v.restart(); len(out.Unrestored) != 1 {
		t.Fatalf("recovered %+v", out)
	}
	v.f.RenamePmem("region0", "pmem0", "259:1")
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{req}); err != nil {
		t.Fatal(err)
	}
	if mi, _ := k.Mounted(req.BindTarget); mi == nil {
		t.Fatal("not bound again")
	}
	v.check("restored")
	v.mustUnprepare(poolDriver, "c")
	v.clean("unprepared")
}

// A pool block grant whose block device is not the one of its region at a
// start is REALISED_UNRESTORED (every grant kind is verified), its
// preparation PREPARED_UNRESTORED; a Prepare again once the block device is
// the prepared one again restores it.
func TestRecoverPoolBlockChanged(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.mustPrepare(poolDriver, "b", poolReq("b", 0, serialP, state.ModeBlock, state.KindPoolPersistent))
	v.f.RenamePmem("region0", "pmem3", "259:9")
	out := v.restart()
	if !slices.Equal(out.Unrestored, []state.GrantRef{{Claim: "b"}}) || out.Why[state.GrantRef{Claim: "b"}] == nil {
		t.Fatalf("recovered %+v", out)
	}
	if g := v.grant("b", 0); g.State != state.GrantUnrestored {
		t.Fatalf("grant %s", g.State)
	}
	wantPreparation(t, v, poolDriver, "b", state.PreparationUnrestored)
	req := poolReq("b", 0, serialP, state.ModeBlock, state.KindPoolPersistent)
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{req}); err == nil || !strings.Contains(err.Error(), "as prepared") {
		t.Fatalf("Prepare again with another block device: %v", err)
	}
	v.f.RenamePmem("region0", "pmem0", "259:0")
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{req}); err != nil {
		t.Fatalf("Prepare again: %v", err)
	}
	if g := v.grant("b", 0); g.State != state.GrantRealised {
		t.Fatalf("grant %s after the Prepare again", g.State)
	}
	v.mustUnprepare(poolDriver, "b")
	v.clean("unprepared")
}

// A cached node mount that the start mounts again on another loop device
// records that one; a Prepare again that mounts an unrestored node mount
// again makes its other grants REALISED too.
func TestRecoverLoopRecorded(t *testing.T) {
	v := pmemEnv(t, "ext4")
	v.mustPrepare(poolDriver, "r1", fsGrant("r1", true, state.AccessCached))
	v.mustPrepare(poolDriver, "r2", fsGrant("r2", true, state.AccessCached))
	mnt := v.mount("r1", 0).Path
	k := v.f.Kernel()
	if err := k.Unmount(mnt); err != nil {
		t.Fatal(err)
	}
	// another loop device takes the number of the old one
	other := filepath.Join(v.dir, "other")
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ConfigureLoop(other, false); err != nil {
		t.Fatal(err)
	}
	v.restart()
	g := v.grant("r1", 0)
	m := v.records().Region(g.Got.Region).Mount(g.Got.Mount)
	if mi, _ := k.Mounted(mnt); mi == nil || mi.Source != m.Loop || m.Path != mnt {
		t.Fatalf("mounted %+v, mount %+v, grant %+v", mi, m, g.Got)
	}
	// unrestored: a block device that is not the one prepared
	if err := k.Unmount(mnt); err != nil {
		t.Fatal(err)
	}
	v.f.RenamePmem("region0", "pmem3", "259:9")
	if out := v.restart(); len(out.Unrestored) != 2 {
		t.Fatalf("recovered %+v", out)
	}
	v.f.RenamePmem("region0", "pmem0", "259:1")
	if _, err := v.e.PrepareGrants(context.Background(), []GrantRequest{fsGrant("r1", true, state.AccessCached)}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"r1", "r2"} {
		if g := v.grant(uid, 0); g.State != state.GrantRealised {
			t.Fatalf("grant %s: %s", uid, g.State)
		}
	}
	// the preparation of r1 is the front-end's, after PrepareGrants
	if p := v.records().Preparation(poolDriver, "r2"); p.State != state.PreparationPrepared {
		t.Fatalf("preparation r2: %s", p.State)
	}
	v.mustUnprepare(poolDriver, "r1")
	v.mustUnprepare(poolDriver, "r2")
	v.clean("unprepared")
}
