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
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// A writable block grant creates the fsdax namespace of the device, the
// block device read-write; its release destroys the region and disables
// the memdev, leaving the namespace in the labels.
func TestPersistentBlockWriter(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.mustPrepare(poolDriver, "b1", poolReq("b1", 0, serialP, state.ModeBlock, state.KindPoolPersistent))
	g := v.grant("b1", 0)
	if g.Got.BlockDevice == nil || g.Got.Namespace == "" || g.Intent.NdRegion == "" || v.f.IsReadOnly(g.Got.BlockDevice.Name) {
		t.Fatalf("grant %+v", g)
	}
	// a reader next to the block writer is refused
	err := v.prepare(poolDriver, "b2", fsGrant("b2", true, ""))
	if err == nil || !strings.Contains(err.Error(), "mode block read-write needs the device alone on this node") {
		t.Fatalf("error %v", err)
	}
	v.mustUnprepare(poolDriver, "b1")
	v.clean("unprepared")
	if !v.f.Labels(serialP) {
		t.Fatal("the labels went with the region")
	}
}

// A reader of a device that no writer formatted is refused: no fsdax
// namespace.
func TestPersistentReaderNeedsNamespace(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	err := v.prepare(poolDriver, "r1", fsGrant("r1", true, ""))
	if err == nil || !strings.Contains(err.Error(), "no fsdax namespace") {
		t.Fatalf("error %v", err)
	}
	v.check("refused")
	v.clean("refused")
}

// ext4 read-write: the writer mounts it rw with DAX, a reader joins the
// mount, the writer's release makes it read-only, the reader's unmounts it.
func TestPersistentFsReadWrite(t *testing.T) {
	v := pmemEnv(t, "ext4")
	v.mustPrepare(poolDriver, "w", fsGrant("w", false, ""))
	g := v.grant("w", 0)
	direct := filepath.Join(v.dir, poolDriver, "mounts", "91-direct")
	m := v.records().Region(g.Got.Region).Mount(state.AccessDirect)
	if m == nil || m.Path != direct || g.Got.Mount != state.AccessDirect || m.FSType != "ext4" || g.Intent.MountPath != direct {
		t.Fatalf("grant %+v, mount %+v", g.Got, m)
	}
	if m.State != state.MountedRW {
		t.Fatalf("mount %+v", m)
	}
	v.mustPrepare(poolDriver, "r", fsGrant("r", true, ""))
	if m := v.records().Region(g.Got.Region).Mount(state.AccessDirect); len(m.Users) != 2 || m.State != state.MountedRW {
		t.Fatalf("mount %+v", m)
	}
	v.mustUnprepare(poolDriver, "w")
	if m := v.records().Region(g.Got.Region).Mount(state.AccessDirect); m.State != state.MountedRO {
		t.Fatalf("mount after the writer %+v", m)
	}
	if !v.f.IsReadOnly(g.Got.BlockDevice.Name) {
		t.Fatal("block device writable without a writer")
	}
	v.mustUnprepare(poolDriver, "r")
	v.clean("unprepared")
	if len(v.f.Violations()) > 0 {
		t.Fatalf("violations %v", v.f.Violations())
	}
}

// erofs read-only with access cached: a read-only loop device over the
// block device, dax=never.
func TestPersistentFsReadOnly(t *testing.T) {
	v := pmemEnv(t, "erofs")
	v.mustPrepare(poolDriver, "r", fsGrant("r", true, state.AccessCached))
	g, m := v.grant("r", 0), v.mount("r", 0)
	if g.Got.Mount != state.AccessCached || m.Loop == "" || m.FSType != "erofs" {
		t.Fatalf("grant %+v, mount %+v", g.Got, m)
	}
	err := v.prepare(poolDriver, "w", fsGrant("w", false, state.AccessCached))
	want := "erofs is a read-only filesystem: set readOnly: true"
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("error %v", err)
	}
	v.mustUnprepare(poolDriver, "r")
	v.clean("unprepared")
}

// A reader without an access gets direct on a type with DAX, cached on the
// others; a read-only type is mounted with mount(2), the others with the
// new mount API; fsTypes lets in the types it lists. A block reader makes
// the block device read-only.
func TestPersistentDefaults(t *testing.T) {
	for _, tc := range []struct {
		typ     string
		access  state.Access
		syscall string
	}{
		{"erofs", state.AccessDirect, "mount "},
		{"squashfs", state.AccessCached, "mount "},
		{"ext4", state.AccessDirect, "fsmount "},
		{"xfs", state.AccessDirect, "fsmount "},
		{"btrfs", state.AccessCached, "fsmount "},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			v := pmemEnv(t, tc.typ)
			v.configure(func(o *Options) { o.FSTypes = []string{tc.typ} })
			n := mountCalls(v)
			v.mustPrepare(poolDriver, "r", fsGrant("r", true, ""))
			if g := v.grant("r", 0); g.Got.Mount != tc.access {
				t.Fatalf("access %q, want %q", g.Got.Mount, tc.access)
			}
			var mounts []string
			for _, c := range mountCallsSince(v, n) {
				if strings.HasPrefix(c, "mount ") || strings.HasPrefix(c, "fsmount ") {
					mounts = append(mounts, c)
				}
			}
			if len(mounts) != 1 || !strings.HasPrefix(mounts[0], tc.syscall) {
				t.Fatalf("mounts %q, want one %q", mounts, tc.syscall)
			}
			v.mustUnprepare(poolDriver, "r")
			v.clean("unprepared")
		})
	}
	t.Run("block reader", func(t *testing.T) {
		v := pmemEnv(t, "")
		v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeBlock, true, ""))
		if g := v.grant("r", 0); g.Got.BlockDevice == nil || !v.f.IsReadOnly(g.Got.BlockDevice.Name) {
			t.Fatalf("block reader on a writable device: %+v", g.Got)
		}
		v.mustUnprepare(localDriver, "r")
		v.clean("unprepared")
	})
}

// A read-write claim with access cached of a device that a reader mounts
// with access direct only is refused with the text of the e2e tests, and
// the refusal mounts and changes nothing.
func TestPersistentRefusesSecondAccess(t *testing.T) {
	v := pmemEnv(t, "ext4")
	v.mustPrepare(poolDriver, "r", fsGrant("r", true, state.AccessDirect))
	n := mountCalls(v)
	err := v.prepare(poolDriver, "w", fsGrant("w", false, state.AccessCached))
	want := "memdev " + api.FormatSerial(serialP) + ": it is mounted with access direct only on this node: " +
		"a read-write claim with access cached would mount it a second time"
	if err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
	// the refusal mounts and changes nothing: the reader's read-only node
	// mount stays the only one, on the read-only block device
	wantMountCalls(t, v, n)
	direct := v.mount("r", 0).Path
	if mounts := v.f.MountTable(); len(mounts) != 1 || mounts[0].Target != direct || !mounts[0].SuperReadOnly() {
		t.Fatalf("mounts %+v", mounts)
	}
	if !v.f.IsReadOnly(filepath.Join(v.f.DevRoot(), "pmem0")) {
		t.Fatal("block device writable after the refusal")
	}
	v.mustUnprepare(poolDriver, "r")
}

// The refusals of the rules of a persistent device before anything of it
// is mounted, with their texts, and the rules that let a claim through.
func TestPersistentRules(t *testing.T) {
	fs := func(uid string, readOnly bool, access state.Access, fsType string) GrantRequest {
		req := localPmemReq(uid, 0, state.ModeFS, readOnly, access)
		req.Asked.FSType = fsType
		return req
	}
	// shared makes req of a device that other hosts may use
	shared := func(req GrantRequest, coherent bool) GrantRequest {
		req.SingleHost, req.Facts.Coherent = false, coherent
		return req
	}
	reader := func(uid string, access state.Access) func(v *env, dev string) {
		return func(v *env, dev string) { v.mustPrepare(localDriver, uid, fs(uid, true, access, "")) }
	}
	known := []string{"erofs", "squashfs", "ext4", "xfs", "btrfs", "gfs2"}
	for _, tc := range []struct {
		name  string
		image string
		// fsTypes are the types that the node allows, nil for known
		fsTypes []string
		setup   func(v *env, dev string)
		req     GrantRequest
		// err is the refusal, $DEV for the block device; "" for none
		err string
	}{
		{name: "type not allowed", image: "ext4", fsTypes: []string{"erofs", "squashfs"}, req: fs("x", true, "", ""),
			err: "memdev 0x91: filesystem type ext4 is not allowed on this node (fsTypes: erofs, squashfs)"},
		{name: "no type allowed", image: "erofs", fsTypes: []string{}, req: fs("x", true, "", ""),
			err: "memdev 0x91: filesystem type erofs is not allowed on this node (fsTypes: none)"},
		{name: "type without a block device", fsTypes: append(slices.Clone(known), "tmpfs"), req: fs("x", true, "", "tmpfs"),
			err: "memdev 0x91: filesystem type tmpfs does not mount a block device"},
		{name: "read-only type written", image: "erofs", req: fs("x", false, "", ""),
			err: "memdev 0x91: erofs is a read-only filesystem: set readOnly: true"},
		{name: "direct without DAX", image: "btrfs", req: fs("x", true, state.AccessDirect, ""),
			err: "memdev 0x91: btrfs has no DAX: use access cached"},
		{name: "single-host type written by hosts", image: "gfs2-nolock", req: shared(fs("x", false, "", "gfs2"), true),
			err: "memdev 0x91: read-write on several hosts needs a cluster filesystem on coherent memory: gfs2 with lock_nolock is a single-host filesystem"},
		{name: "cluster type on memory that is not coherent", image: "gfs2", req: shared(fs("x", false, "", "gfs2"), false),
			err: "memdev 0x91: read-write on several hosts needs a cluster filesystem on coherent memory: the device is not coherent between hosts"},
		{name: "cluster type written by hosts", image: "gfs2", req: shared(fs("x", false, "", "gfs2"), true)},
		{name: "cached next to a direct writer", image: "ext4", req: fs("x", true, state.AccessCached, ""),
			setup: func(v *env, dev string) { v.mustPrepare(localDriver, "w", fs("w", false, state.AccessDirect, "")) },
			err:   "memdev 0x91: it is mounted read-write with access direct on this node: access cached would mount it a second time"},
		{name: "writer next to both accesses", image: "ext4", req: fs("x", false, state.AccessDirect, ""),
			setup: func(v *env, dev string) {
				reader("r1", state.AccessDirect)(v, dev)
				reader("r2", state.AccessCached)(v, dev)
			},
			err: "memdev 0x91: it is mounted with access direct and cached on this node: a read-write claim needs one access only"},
		{name: "direct writer next to a cached reader", image: "ext4", req: fs("x", false, state.AccessDirect, ""), setup: reader("r", state.AccessCached),
			err: "memdev 0x91: it is mounted with access cached only on this node: a read-write claim with access direct would mount it a second time"},
		{name: "block writer next to a reader", image: "ext4", req: localPmemReq("x", 0, state.ModeBlock, false, ""), setup: reader("r", ""),
			err: "memdev 0x91: mode block read-write needs the device alone on this node"},
		{name: "btrfs writer next to a reader", image: "btrfs", req: fs("x", false, "", ""), setup: reader("r", ""),
			err: "memdev 0x91: btrfs is mounted read-only on this node and cannot be remounted read-write: a read-write claim waits until the read-only claims of the node are gone"},
		{name: "reader of a dirty ext4", image: "ext4-dirty", req: fs("x", true, "", ""),
			err: "memdev 0x91: ext4 on $DEV needs recovery (it was not unmounted cleanly): a read-only mount cannot recover it, a read-write claim on one node does"},
		{name: "reader of an xfs that the mount finds dirty", image: "xfs", req: fs("x", true, "", ""),
			setup: func(v *env, dev string) {
				// the block device is there while a block reader holds the device
				v.mustPrepare(localDriver, "b", localPmemReq("b", 0, state.ModeBlock, true, ""))
				v.f.SetDirty(dev, true)
			},
			err: "memdev 0x91: xfs on $DEV needs recovery (it was not unmounted cleanly): a read-only mount cannot recover it, a read-write claim on one node does"},
		{name: "writer of a dirty ext4", image: "ext4-dirty", req: fs("x", false, "", "")},
		{name: "mount refused", image: "ext4", req: fs("x", true, "", ""),
			setup: func(v *env, dev string) {
				v.f.SetMountError(dev, syscall.EINVAL, "e ext4: unsupported blocksize for DAX")
			},
			err: "memdev 0x91: $DEV has no ext4 filesystem: invalid argument: e ext4: unsupported blocksize for DAX"},
		{name: "mount of the loop device refused", image: "ext4", req: fs("x", true, state.AccessCached, ""),
			setup: func(v *env, dev string) { v.f.SetMountError(filepath.Join(v.f.DevRoot(), "loop0"), syscall.EINVAL, "") },
			err:   "memdev 0x91: $DEV has no ext4 filesystem: invalid argument"},
		{name: "superblock without DAX", image: "ext4", req: fs("x", true, "", ""),
			setup: func(v *env, dev string) { v.f.SetSuperOpts(dev, "ro", "dax=never") },
			err:   "memdev 0x91: ext4 on $DEV was mounted without DAX"},
		{name: "no known filesystem", req: fs("x", true, "", ""),
			err: "memdev 0x91: $DEV has no filesystem of a type that the driver knows (erofs, squashfs, ext4, xfs, btrfs or gfs2): set device.fsType"},
		{name: "type other than the node mount's", image: "ext4", req: fs("x", true, "", "xfs"), setup: reader("r", ""),
			err: "memdev 0x91: $DEV has a filesystem of type ext4, not xfs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := pmemEnv(t, tc.image)
			fsTypes := tc.fsTypes
			if fsTypes == nil {
				fsTypes = known
			}
			v.configure(func(o *Options) { o.FSTypes = fsTypes })
			dev := filepath.Join(v.f.DevRoot(), "pmem0")
			if tc.setup != nil {
				tc.setup(v, dev)
			}
			mounts, loops := len(v.f.MountTable()), len(v.f.Kernel().LoopDevices())
			err := v.prepare(localDriver, "x", tc.req)
			switch {
			case tc.err == "" && err != nil:
				t.Fatal(err)
			case tc.err != "" && (err == nil || err.Error() != strings.ReplaceAll(tc.err, "$DEV", dev)):
				t.Fatalf("error %v, want %q", err, tc.err)
			case tc.err != "" && (len(v.f.MountTable()) != mounts || len(v.f.Kernel().LoopDevices()) != loops):
				t.Fatalf("mounts %+v, loop devices %v after a refusal", v.f.MountTable(), v.f.Kernel().LoopDevices())
			}
			finish(t, v, "finished")
			entries, _ := os.ReadDir(filepath.Join(v.dir, localDriver, "mounts"))
			if len(v.f.MountTable()) != 0 || len(v.f.Kernel().LoopDevices()) != 0 || len(entries) != 0 {
				t.Fatalf("left: mounts %+v loops %v dirs %v", v.f.MountTable(), v.f.Kernel().LoopDevices(), entries)
			}
		})
	}
}

// A persistent grant of a memdev without persistent memory is refused.
func TestPersistentWithoutPmem(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	err := v.prepare(poolDriver, "x", fsReq(poolReq("x", 0, serialA, state.ModeFS, state.KindPoolPersistent), true, ""))
	if want := "memdev 0xa1: mem0 has no persistent memory"; err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
	finish(t, v, "unprepared")
}

// A remount read-write that the filesystem refuses leaves both block devices
// of a cached xfs read-only again; a read-only xfs is mounted with nouuid.
func TestPersistentRemountReadWriteFails(t *testing.T) {
	v := pmemEnv(t, "xfs")
	v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeFS, true, state.AccessCached))
	m := v.mount("r", 0)
	if c := v.f.MountCalls("fsmount"); m.Loop == "" || !slices.Equal(c, []string{"fsmount " + m.Loop + " " + m.Path + " xfs ro dax=never,nouuid"}) {
		t.Fatalf("mounts %q, loop device %q", c, m.Loop)
	}
	v.f.SetRemountError(m.Path, syscall.EINVAL)
	err := v.prepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, state.AccessCached))
	if want := "memdev 0x91: remounting " + m.Path + " read-write: invalid argument"; err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
	if !v.f.IsReadOnly(filepath.Join(v.f.DevRoot(), "pmem0")) || !v.f.IsReadOnly(m.Loop) {
		t.Fatal("block devices writable after a failed remount read-write")
	}
	if mi, _ := v.f.Kernel().Mounted(m.Path); mi == nil || !mi.SuperReadOnly() {
		t.Fatalf("node mount %+v", mi)
	}
	v.mustUnprepare(localDriver, "r")
	v.clean("unprepared")
	if len(v.f.Violations()) > 0 {
		t.Fatalf("violations %v", v.f.Violations())
	}
}

// A superblock of the device that no grant of the node explains, a mount in
// another mount namespace of the block device or of a loop device over it,
// refuses a writer, a fresh mount of a writable type and a block writer,
// and a read-write one a reader that would make the block device
// read-only, before anything is mounted; the same claim is prepared once
// that mount is gone. A copy of a node mount in a container or a bind of
// it elsewhere is explained. Superblocks that cannot be read refuse a use.
func TestPersistentUnexplainedSuperblocks(t *testing.T) {
	fsClaim := func(uid string, readOnly bool) GrantRequest {
		return localPmemReq(uid, 0, state.ModeFS, readOnly, "")
	}
	blockClaim := func(uid string, readOnly bool) GrantRequest {
		return localPmemReq(uid, 0, state.ModeBlock, readOnly, "")
	}
	unexplained := func(src, ro, target string) string {
		return "memdev 0x91: $DEV has a superblock that no claim of this node explains (" + src + " mounted " + ro + " in pid 4242 at " +
			target + "): it is not used until that mount is gone"
	}
	notReadOnly := "memdev 0x91: not making $DEV read-only: $DEV is mounted read-write in pid 4242 at /mnt/x"
	for _, c := range []struct {
		name  string
		image string
		// held is a claim prepared before the mount elsewhere, if any
		held GrantRequest
		// the mount elsewhere, of a loop device over the block device
		// if loop; none without a target
		target   string
		loop, ro bool
		// hidden makes the superblocks unreadable
		hidden bool
		req    GrantRequest
		// err is the refusal, $DEV for the block device, $SRC for the
		// source of the mount elsewhere, $PROC for the procfs; "" for none
		err string
	}{
		{name: "writer", image: "ext4", target: "/mnt/x", req: fsClaim("x", false), err: unexplained("$DEV", "rw", "/mnt/x")},
		{name: "reader of a writable type", image: "ext4", target: "/mnt/x", req: fsClaim("x", true), err: unexplained("$DEV", "rw", "/mnt/x")},
		{name: "block writer", image: "ext4", target: "/mnt/x", req: blockClaim("x", false), err: unexplained("$DEV", "rw", "/mnt/x")},
		{name: "block writer, read-only mount", image: "ext4", target: "/mnt/x", ro: true, req: blockClaim("x", false),
			err: unexplained("$DEV", "ro", "/mnt/x")},
		{name: "block reader", image: "ext4", target: "/mnt/x", req: blockClaim("x", true), err: notReadOnly},
		{name: "block reader, read-only mount", image: "ext4", target: "/mnt/x", ro: true, req: blockClaim("x", true)},
		// a btrfs superblock has a device number of its own
		{name: "block writer, btrfs", image: "btrfs", target: "/mnt/x", req: blockClaim("x", false), err: unexplained("$DEV", "rw", "/mnt/x")},
		{name: "block reader, btrfs", image: "btrfs", target: "/mnt/x", req: blockClaim("x", true), err: notReadOnly},
		{name: "remount read-write, loop device", image: "ext4", held: fsClaim("r", true), target: "/mnt/old", loop: true, ro: true,
			req: fsClaim("x", false), err: unexplained("$SRC", "ro", "/mnt/old")},
		{name: "remount read-write, bind of the node mount", image: "ext4", held: fsClaim("r", true), target: "/run/cxl/fs/ro", ro: true,
			req: fsClaim("x", false)},
		{name: "writer, copy of the node mount in a container", image: "ext4", held: fsClaim("w", false), target: "/data",
			req: fsClaim("x", false)},
		{name: "writer, superblocks unreadable", image: "ext4", hidden: true, req: fsClaim("x", false),
			err: "open $PROC/self/mountinfo: no such file or directory"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib}, fake.MemdevSpec{Serial: serialB})
			v.formatPmem("mem0", c.image)
			v.configure(func(o *Options) { o.FSTypes = []string{"ext4", "btrfs"} })
			dev := filepath.Join(v.f.DevRoot(), "pmem0")
			// the region stays idle under a region above it, its block
			// device writable as a block writer leaves it
			v.mustPrepare(localDriver, "b", blockClaim("b", false))
			above := rawRegion(t, v, "mem1")
			v.mustUnprepare(localDriver, "b")
			if c.held.Ref.Claim != "" {
				v.mustPrepare(localDriver, c.held.Ref.Claim, c.held)
			}
			src := dev
			var loop kernel.Loop
			if c.loop {
				var err error
				if loop, err = v.f.Kernel().ConfigureLoop(dev, false); err != nil {
					t.Fatal(err)
				}
				src = loop.Path()
			}
			if c.target != "" {
				v.f.AddForeign(4242, src, c.target, c.image, c.ro)
			}
			v.f.HideProc(c.hidden)
			n := mountCalls(v)
			err := v.prepare(localDriver, "x", c.req)
			v.f.HideProc(false)
			want := strings.NewReplacer("$DEV", dev, "$SRC", src, "$PROC", v.f.ProcRoot()).Replace(c.err)
			switch {
			case c.err == "" && err != nil:
				t.Fatal(err)
			case c.err != "" && (err == nil || err.Error() != want):
				t.Fatalf("error %v, want %q", err, want)
			case c.err != "":
				if calls := mountCallsSince(v, n); len(calls) > 0 {
					t.Fatalf("calls %q of a refused Prepare", calls)
				}
				v.f.RemoveForeign(4242)
				if loop != nil {
					if err := loop.Clear(); err != nil {
						t.Fatal(err)
					}
				}
				v.mustPrepare(localDriver, "x", c.req)
			}
			v.f.RemoveForeign(4242)
			for _, p := range v.records().Preparations {
				v.mustUnprepare(localDriver, p.UID)
			}
			dropRaw(t, v, above)
			v.restart()
			v.check("finished")
			v.clean("finished")
			if viol := v.f.Violations(); len(viol) > 0 {
				t.Fatalf("violations %v", viol)
			}
		})
	}
}

// fsUse returns the request of a grant of mode fs of the claim uid on the
// persistent memdev serialP of the source.
func fsUse(source state.Source, uid string, readOnly bool, access state.Access) GrantRequest {
	if source == state.SourceLocal {
		return localPmemReq(uid, 0, state.ModeFS, readOnly, access)
	}
	return fsGrant(uid, readOnly, access)
}

// mountCalls returns the number of system calls of the mount table of the
// node so far.
func mountCalls(v *env) int {
	return len(v.f.MountCalls(""))
}

// mountCallsSince returns the system calls of the mount table of the node
// after the first n.
func mountCallsSince(v *env, n int) []string {
	return slices.Clone(v.f.MountCalls("")[n:])
}

// wantMountCalls fails the test unless the system calls of the mount table
// of the node after the first n are want.
func wantMountCalls(t *testing.T, v *env, n int, want ...string) {
	t.Helper()
	if got := mountCallsSince(v, n); !slices.Equal(got, want) {
		t.Fatalf("calls\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// A cached ext4 is on a writable loop device: the reader makes the block
// device read-only, attaches the loop device read-write and makes it
// read-only before its mount; a writer makes the block device, then the
// loop device writable before the superblock; the release of the writer
// writes the data out before the superblock turns read-only, then the loop
// device, then the block device. A read-only type is on a read-only loop
// device.
func TestPersistentCachedOrder(t *testing.T) {
	for _, source := range sources {
		t.Run(string(source), func(t *testing.T) {
			driver := driverOf(source)
			v := pmemEnv(t, "ext4")
			cached := filepath.Join(v.dir, driver, "mounts", "91-cached")
			pmem, loop := filepath.Join(v.f.DevRoot(), "pmem0"), filepath.Join(v.f.DevRoot(), "loop0")
			n := mountCalls(v)
			v.mustPrepare(driver, "r", fsUse(source, "r", true, state.AccessCached))
			if m := v.mount("r", 0); m.Loop != loop || m.Path != cached {
				t.Fatalf("mount %+v", m)
			}
			wantMountCalls(t, v, n, "blkroset "+pmem+" 1", "loop "+loop+" "+pmem+" rw", "blkroset "+loop+" 1",
				"fsmount "+loop+" "+cached+" ext4 ro dax=never", "loop-close "+loop)
			n = mountCalls(v)
			v.mustPrepare(driver, "w", fsUse(source, "w", false, state.AccessCached))
			wantMountCalls(t, v, n, "blkroset "+pmem+" 0", "blkroset "+loop+" 0", "remount "+cached+" rw")
			n = mountCalls(v)
			v.mustUnprepare(driver, "w")
			wantMountCalls(t, v, n, "syncfs "+cached, "remount "+cached+" ro", "blkroset "+loop+" 1", "blkroset "+pmem+" 1")
			v.mustUnprepare(driver, "r")
			if mounts, loops := v.f.MountTable(), v.f.Kernel().LoopDevices(); len(mounts) != 0 || len(loops) != 0 {
				t.Fatalf("left: mounts %+v loops %+v", mounts, loops)
			}
			v.clean("unprepared")
			if len(v.f.Violations()) > 0 {
				t.Fatalf("violations %v", v.f.Violations())
			}
		})
	}
	t.Run("read-only type", func(t *testing.T) {
		v := pmemEnv(t, "erofs")
		pmem, loop := filepath.Join(v.f.DevRoot(), "pmem0"), filepath.Join(v.f.DevRoot(), "loop0")
		n := mountCalls(v)
		v.mustPrepare(poolDriver, "r", fsGrant("r", true, state.AccessCached))
		if got := v.f.MountCalls("loop "); !slices.Equal(got, []string{"loop " + loop + " " + pmem}) {
			t.Fatalf("loop devices %q of the calls %q", got, mountCallsSince(v, n))
		}
		v.mustUnprepare(poolDriver, "r")
		v.clean("unprepared")
	})
}

// The log lines of the node mounts and of the read-only flags of the block
// devices, each once and in order.
func TestPersistentLogLines(t *testing.T) {
	prefix := "engine: pool memdev " + api.FormatSerial(serialP) + ": "
	wantLines := func(t *testing.T, logs string, want ...string) {
		t.Helper()
		if got := linesWithPrefix(logs, prefix); !slices.Equal(got, want) {
			t.Fatalf("log lines\n  %s\nexpected\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	}
	t.Run("writer alone", func(t *testing.T) {
		v := pmemEnv(t, "ext4")
		logs := klogtest.Capture(t, 3)
		direct := filepath.Join(v.dir, poolDriver, "mounts", "91-direct")
		v.mustPrepare(poolDriver, "w", fsGrant("w", false, ""))
		v.mustUnprepare(poolDriver, "w")
		pmem := v.f.DevRoot() + "/pmem0"
		wantLines(t, logs(),
			prefix+"mounted "+pmem+" at "+direct+" (ext4, rw, dax=always)",
			prefix+"unmounted "+direct)
	})
	t.Run("cached reader and writer", func(t *testing.T) {
		v := pmemEnv(t, "ext4")
		logs := klogtest.Capture(t, 3)
		cached := filepath.Join(v.dir, poolDriver, "mounts", "91-cached")
		pmem, loop := v.f.DevRoot()+"/pmem0", v.f.DevRoot()+"/loop0"
		v.mustPrepare(poolDriver, "r", fsGrant("r", true, state.AccessCached))
		v.mustPrepare(poolDriver, "w", fsGrant("w", false, state.AccessCached))
		v.mustUnprepare(poolDriver, "w")
		v.mustUnprepare(poolDriver, "r")
		wantLines(t, logs(),
			prefix+pmem+" read-only",
			prefix+loop+" read-only",
			prefix+"mounted "+loop+" at "+cached+" (ext4, ro, dax=never)",
			prefix+pmem+" read-write",
			prefix+loop+" read-write",
			prefix+"remounted "+cached+" read-write",
			prefix+"remounted "+cached+" read-only",
			prefix+loop+" read-only",
			prefix+pmem+" read-only",
			prefix+"unmounted "+cached)
	})
}

// A grant of a local persistent memdev whose release failed holds the device
// until its release is done: the node mount of a reader whose unmount is busy,
// the read-write superblock of a writer whose remount read-only is busy. The
// rules of one node refuse the Prepares of other claims next to it, which
// mount and change nothing; a reader of its node mount reuses it as it is.
func TestPersistentReleasingGrants(t *testing.T) {
	refused := func(t *testing.T, v *env, req GrantRequest, want string) {
		t.Helper()
		n := mountCalls(v)
		if err := v.prepare(localDriver, req.Ref.Claim, req); err == nil || err.Error() != want {
			t.Errorf("claim %s: error %v, want %q", req.Ref.Claim, err, want)
		}
		if calls := mountCallsSince(v, n); len(calls) > 0 {
			t.Errorf("claim %s: calls %q of a refused Prepare", req.Ref.Claim, calls)
		}
	}
	t.Run("node mount of a reader", func(t *testing.T) {
		v := pmemEnv(t, "ext4")
		v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeFS, true, state.AccessCached))
		cached := v.mount("r", 0).Path
		v.f.SetBusy(cached, true)
		if err := v.unprepare(localDriver, "r"); err == nil || !strings.Contains(err.Error(), "unmounting "+cached+": device or resource busy") {
			t.Fatalf("error %v", err)
		}
		refused(t, v, localPmemReq("w", 0, state.ModeFS, false, state.AccessDirect),
			"memdev 0x91: it is mounted with access cached only on this node: a read-write claim with access direct would mount it a second time")
		v.f.SetBusy(cached, false)
		finish(t, v, "finished")
		if viol := v.f.Violations(); len(viol) > 0 {
			t.Fatalf("violations %v", viol)
		}
	})
	t.Run("superblock of a writer", func(t *testing.T) {
		v := pmemEnv(t, "ext4")
		v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
		v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeFS, true, ""))
		direct := v.mount("w", 0).Path
		v.f.SetWriting(direct, true)
		if err := v.unprepare(localDriver, "w"); err == nil || !strings.Contains(err.Error(), "remounting "+direct+" read-only: device or resource busy") {
			t.Fatalf("error %v", err)
		}
		other := "memdev " + api.FormatSerial(serialP) + ": it is mounted read-write with access direct on this node: " +
			"access cached would mount it a second time"
		refused(t, v, localPmemReq("c", 0, state.ModeFS, true, state.AccessCached), other)
		refused(t, v, localPmemReq("w2", 0, state.ModeFS, false, state.AccessCached), other)
		// a reader of the node mount: as it is, read-write
		n := mountCalls(v)
		v.mustPrepare(localDriver, "r2", localPmemReq("r2", 0, state.ModeFS, true, state.AccessDirect))
		if calls := mountCallsSince(v, n); len(calls) > 0 {
			t.Errorf("calls %q of the reader", calls)
		}
		if mi, _ := v.f.Kernel().Mounted(direct); mi == nil || mi.SuperReadOnly() {
			t.Errorf("node mount %+v", mi)
		}
		v.f.SetWriting(direct, false)
		finish(t, v, "finished")
		if viol := v.f.Violations(); len(viol) > 0 {
			t.Fatalf("violations %v", viol)
		}
	})
}

// The Unprepare of a writer whose remount read-only fails does not finish: its
// claim stays UNPREPARING and its grant RELEASING through a start, which
// leaves the read-write node mount as it is for kubelet's retry; the retry
// makes the node mount and the block device read-only for the reader. A
// remount read-only fails closed when the superblocks cannot be read.
func TestPersistentRemountReadOnlyFails(t *testing.T) {
	v := pmemEnv(t, "ext4")
	dev := filepath.Join(v.f.DevRoot(), "pmem0")
	v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
	v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeFS, true, ""))
	direct := v.mount("w", 0).Path
	writable := func(what string) {
		t.Helper()
		if mi, _ := v.f.Kernel().Mounted(direct); mi == nil || mi.SuperReadOnly() || v.f.IsReadOnly(dev) {
			t.Fatalf("%s: node mount %+v, block device read-only %v", what, mi, v.f.IsReadOnly(dev))
		}
		wantPreparation(t, v, localDriver, "w", state.PreparationUnpreparing)
		if g := v.grant("w", 0); g.State != state.GrantReleasing {
			t.Fatalf("%s: grant %+v", what, g)
		}
	}
	v.f.SetWriting(direct, true)
	if err := v.unprepare(localDriver, "w"); err == nil || err.Error() != "releasing w/0 (memdev 0x91): memdev 0x91: remounting "+direct+" read-only: device or resource busy" {
		t.Fatalf("error %v", err)
	}
	writable("failed remount read-only")
	n := mountCalls(v)
	v.restart()
	writable("restarted")
	wantMountCalls(t, v, n)
	v.f.SetWriting(direct, false)
	v.mustUnprepare(localDriver, "w")
	if mi, _ := v.f.Kernel().Mounted(direct); mi == nil || !mi.SuperReadOnly() || !v.f.IsReadOnly(dev) {
		t.Fatalf("remounted read-only: node mount %+v, block device read-only %v", mi, v.f.IsReadOnly(dev))
	}
	v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
	v.f.HideProc(true)
	err := v.unprepare(localDriver, "w")
	v.f.HideProc(false)
	if want := "releasing w/0 (memdev 0x91): memdev 0x91: not making " + dev + " read-only: its superblocks: open " + v.f.ProcRoot() + ": no such file or directory"; err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
	v.check("superblocks unreadable")
	finish(t, v, "finished")
	if viol := v.f.Violations(); len(viol) > 0 {
		t.Fatalf("violations %v", viol)
	}
}

// A claim of mode fs of another boot (PREPARED_STALE) has nothing mounted:
// the one-node rules do not count it, a new reader next to a stale writer
// and a new writer next to a stale reader are prepared.
func TestPersistentStaleGrantHoldsNothing(t *testing.T) {
	for _, c := range []struct {
		name         string
		stale, newer bool
		want         state.MountState
	}{
		{"stale writer, new reader", false, true, state.MountedRO},
		{"stale reader, new writer", true, false, state.MountedRW},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
			v.formatPmem("mem0", "ext4")
			v.mustPrepare(localDriver, "old", localPmemReq("old", 0, state.ModeFS, c.stale, ""))
			v.f.Reboot()
			v.restart()
			wantPreparation(t, v, localDriver, "old", state.PreparationStale)
			v.mustPrepare(localDriver, "new", localPmemReq("new", 0, state.ModeFS, c.newer, ""))
			if m := v.mount("new", 0); m.State != c.want {
				t.Fatalf("node mount %+v, want %s", m, c.want)
			}
			v.mustUnprepare(localDriver, "old")
			v.mustUnprepare(localDriver, "new")
			v.clean("unprepared")
		})
	}
}

// A writer whose Unprepare failed after its node mount was unmounted has
// nothing mounted: the one-node rules do not count it, a reader is
// prepared next to it; the retry of the Unprepare leaves the reader's
// mount.
func TestPersistentUnmountedGrantHoldsNothing(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.formatPmem("mem0", "ext4")
	v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
	v.f.FailCommand("cxl disable-region", errors.New("cxl region: cmd_disable_region: failed"))
	if err := v.unprepare(localDriver, "w"); err == nil {
		t.Fatal("the release succeeded")
	}
	v.check("release failed")
	v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeFS, true, ""))
	if m := v.mount("r", 0); m.State != state.MountedRO {
		t.Fatalf("node mount %+v", m)
	}
	v.mustUnprepare(localDriver, "w")
	if m := v.mount("r", 0); m.State != state.MountedRO {
		t.Fatalf("node mount after the retry %+v", m)
	}
	v.mustUnprepare(localDriver, "r")
	v.clean("unprepared")
}

// A claim of mode fs whose Unprepare a process stop cut after its region
// was destroyed keeps the block device of that region in its record; the
// writer of a new region next to a reader goes, and the block device, which
// may have the same name now, is read-only again: the records of the uses
// say what it is, not that claim's.
func TestPersistentReleaseIgnoresGoneBlockDevice(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.formatPmem("mem0", "ext4")
	v.mustPrepare(localDriver, "a", localPmemReq("a", 0, state.ModeFS, true, ""))
	v.f.CutAfter("cxl destroy-region")
	if !v.cut(func() { _ = v.unprepare(localDriver, "a") }) {
		t.Fatal("not cut")
	}
	v.restart()
	v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeFS, true, ""))
	v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
	v.mustUnprepare(localDriver, "w")
	if g := v.grant("r", 0); !v.f.IsReadOnly(g.Got.BlockDevice.Name) {
		t.Fatalf("%s is read-write without a writer", g.Got.BlockDevice.Name)
	}
	v.mustUnprepare(localDriver, "r")
	v.mustUnprepare(localDriver, "a")
	v.clean("unprepared")
}

// The release of a persistent grant whose region a cut release destroyed
// waits for no holder of its block device, and a memdev without a region
// is no warning.
func TestPersistentReleaseWithoutRegion(t *testing.T) {
	v := pmemEnv(t, "ext4")
	v.mustPrepare(poolDriver, "w", fsGrant("w", false, ""))
	v.f.CutAfter("cxl destroy-region")
	if !v.cut(func() { _ = v.unprepare(poolDriver, "w") }) {
		t.Fatal("not cut")
	}
	logs := klogtest.Capture(t, 3)
	v.restart()
	v.mustUnprepare(poolDriver, "w")
	if !strings.Contains(logs(), "has no region now: its release waits for no holder") {
		t.Fatalf("no release without a region in the log:\n%s", logs())
	}
	for _, l := range strings.Split(logs(), "\n") {
		if strings.HasPrefix(l, "W") && strings.Contains(l, "has no region") {
			t.Errorf("warning %q", l)
		}
	}
	v.clean("unprepared")
}

// The type of the filesystem of a block device from its signatures and
// the declared type, with the texts of the refusals.
func TestFsSignatureType(t *testing.T) {
	const dev = "/dev/pmem0"
	for _, tc := range []struct {
		types    []string
		declared string
		fsType   string
		err      string
	}{
		{[]string{"ext4"}, "", "ext4", ""},
		{[]string{"ext4"}, "ext4", "ext4", ""},
		{[]string{"ext4"}, "xfs", "", "memdev 0xc1ae0001: /dev/pmem0 has a filesystem of type ext4, not xfs"},                                   // P3
		{[]string{"btrfs"}, "f2fs", "", "memdev 0xc1ae0001: /dev/pmem0 has a filesystem of type btrfs, not f2fs"},                               // P3 unknown
		{[]string{"ext4", "xfs"}, "xfs", "xfs", ""},                                                                                             // several, declared
		{[]string{"ext4", "xfs"}, "", "", "memdev 0xc1ae0001: /dev/pmem0 has signatures of several filesystems (ext4, xfs): set device.fsType"}, // P2
		{[]string{"ext4", "xfs"}, "btrfs", "", "has signatures of several filesystems (ext4, xfs)"},
		{nil, "", "", "memdev 0xc1ae0001: /dev/pmem0 has no filesystem of a type that the driver knows (erofs, squashfs, ext4, xfs, btrfs or gfs2): set device.fsType"}, // P1
		{nil, "f2fs", "f2fs", ""}, // mounted as declared
		{nil, "xfs", "", "memdev 0xc1ae0001: /dev/pmem0 has no filesystem of type xfs (no signature of a type that the driver knows)"}, // P3b
	} {
		got, err := fsType(kernel.FSSignature{Types: tc.types}, "0xc1ae0001", dev, tc.declared)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%v %q: error %v, expected %q", tc.types, tc.declared, err, tc.err)
		case tc.err == "" && err != nil:
			t.Errorf("%v %q: %v", tc.types, tc.declared, err)
		case tc.err == "" && got != tc.fsType:
			t.Errorf("%v %q: %s, expected %s", tc.types, tc.declared, got, tc.fsType)
		}
	}
}

// A cluster filesystem is gfs2 with lock_dlm; the others are single-host,
// and the reason says why.
func TestFsSignatureCluster(t *testing.T) {
	for _, tc := range []struct {
		sig    kernel.FSSignature
		fsType string
		ok     bool
		why    string
	}{
		{kernel.FSSignature{Types: []string{"gfs2"}, Gfs2LockProto: kernel.Gfs2LockProtoDLM}, "gfs2", true, ""},
		{kernel.FSSignature{Types: []string{"gfs2"}, Gfs2LockProto: "lock_nolock"}, "gfs2", false, "gfs2 with lock_nolock is a single-host filesystem"},
		{kernel.FSSignature{Types: []string{"gfs2"}}, "gfs2", false, "gfs2 without a lock protocol is a single-host filesystem"},
		{kernel.FSSignature{Types: []string{"ext4"}}, "ext4", false, "ext4 is a single-host filesystem"},
		{kernel.FSSignature{Types: []string{"erofs"}}, "f2fs", false, "f2fs is a single-host filesystem"},
	} {
		if ok, why := cluster(tc.sig, tc.fsType); ok != tc.ok || why != tc.why {
			t.Errorf("%+v %s: %v %q", tc.sig, tc.fsType, ok, why)
		}
	}
}
