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

package kernel_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

const mountInfoSample = `22 1 0:21 / / rw,relatime shared:1 - btrfs /dev/vda3 rw,seclabel,compress=zstd:1,space_cache=v2,subvolid=257,subvol=/root
75 22 0:5 / /dev rw,nosuid shared:2 - devtmpfs devtmpfs rw,seclabel,size=4096k,nr_inodes=1048576,mode=755
301 22 259:0 / /var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0001-direct ro,nosuid,nodev,relatime shared:150 - erofs /dev/pmem0 ro,user_xattr,acl,dax=always
302 22 7:0 / /var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0001-cached ro,nosuid,nodev,relatime shared:151 - erofs /dev/loop0 ro,user_xattr,acl,dax=never
310 22 0:40 / /mnt/with\040space rw,relatime - tmpfs tmpfs rw
313 22 259:0 /sub\040dir /mnt/sub ro,nosuid,nodev,relatime shared:150 - erofs /dev/pmem0 ro,user_xattr,acl,dax=always
311 22 0:41 / /mnt/stacked rw,relatime - tmpfs first rw
312 311 0:42 / /mnt/stacked rw,relatime master:3 - tmpfs second rw,size=1k
314 22 0:43 / /mnt/back\134slash\04 rw,relatime - tmpfs x\999 rw
`

// The mounts of a mountinfo: escaped fields, binds of subdirectories,
// stacked mounts.
func TestParseMountInfo(t *testing.T) {
	mounts, err := kernel.ParseMountInfo(strings.NewReader(mountInfoSample))
	noErr(t, err)
	if len(mounts) != 9 {
		t.Fatalf("%d mounts", len(mounts))
	}
	want := kernel.MountInfo{
		DevNum:     "259:0",
		Root:       "/",
		Target:     "/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0001-direct",
		FSType:     "erofs",
		Source:     "/dev/pmem0",
		SuperOpts:  []string{"ro", "user_xattr", "acl", "dax=always"},
		MountFlags: []string{"ro", "nosuid", "nodev", "relatime"},
	}
	mi := kernel.FindMount(mounts, want.Target+"/")
	if mi == nil || !reflect.DeepEqual(*mi, want) || !mi.HasSuperOpt("dax=always") || mi.HasSuperOpt("dax=never") || !mi.SuperReadOnly() {
		t.Fatalf("direct mount %+v", mi)
	}
	if mi := kernel.FindMount(mounts, "/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts/c1ae0001-cached"); mi == nil || mi.Source != "/dev/loop0" || !mi.HasSuperOpt("dax=never") {
		t.Fatalf("cached mount %+v", mi)
	}
	if mi := kernel.FindMount(mounts, "/mnt/sub"); mi == nil || mi.Root != "/sub dir" || mi.Source != "/dev/pmem0" {
		t.Fatalf("bind of a subdirectory: %+v", mi)
	}
	if mi := kernel.FindMount(mounts, "/mnt/with space"); mi == nil || mi.FSType != "tmpfs" || mi.SuperReadOnly() {
		t.Fatalf("escaped mount point: %+v", mi)
	}
	if mi := kernel.FindMount(mounts, "/mnt/stacked"); mi == nil || mi.Source != "second" {
		t.Fatalf("stacked mount: %+v", mi)
	}
	// a short escape and one that is no octal number stay as they are
	if mi := kernel.FindMount(mounts, `/mnt/back\slash\04`); mi == nil || mi.Source != `x\999` {
		t.Fatalf("escaped backslash: %+v", mi)
	}
	if kernel.FindMount(mounts, "/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts") != nil {
		t.Fatal("a parent directory is not a mount")
	}
	for _, bad := range []string{"1 2 3\n", "22 1 0:21 / / rw,relatime shared:1 btrfs /dev/vda3 rw\n", "22 1 0:21 / / rw - btrfs\n"} {
		if _, err := kernel.ParseMountInfo(strings.NewReader(bad)); err == nil {
			t.Errorf("invalid line accepted: %q", bad)
		}
	}
}

// fsdaxNode returns a fake with a persistent memdev whose region0 has an
// fsdax namespace with the block device /dev/pmem0 of the fake, and that
// device.
func fsdaxNode(t *testing.T) (*fake.Fake, *kernel.Kernel, string) {
	t.Helper()
	f, k := pmemNode(t)
	createRegion(t, k, "mem0", kernel.RegionPmem, 0)
	ns, err := k.CreateFsdaxNamespace(ctxTimeout(t, 5*time.Second), "region0", "")
	noErr(t, err)
	return f, k, ns.BlockDevice
}

// mountPoint returns a new directory to mount at.
func mountPoint(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	noErr(t, os.MkdirAll(dir, 0o755))
	return dir
}

// The superblocks of a block device and of the loop devices over it, in
// every mount namespace, one per device.
func TestSuperblocks(t *testing.T) {
	f, k, dev := fsdaxNode(t)
	direct := mountPoint(t, "direct")
	noErr(t, setRO(k, dev, false))
	noErr(t, k.FSMount(dev, direct, "ext4", true, []string{"dax=always"}))
	loop, err := k.ConfigureLoop(dev, false)
	noErr(t, err)
	cached := mountPoint(t, "cached")
	noErr(t, k.Mount(loop.Path(), cached, "erofs", syscall.MS_RDONLY, "dax=never"))
	noErr(t, loop.Close())
	f.AddForeign(300, dev, "/run/cxl/fs/y", "ext4", false)
	sbs, err := k.Superblocks(dev)
	noErr(t, err)
	want := []kernel.Superblock{
		{Device: dev, DevNum: "259:0", Source: dev, FSType: "ext4", Where: "pid 1 at " + direct},
		{Device: loop.Path(), DevNum: "7:0", Source: loop.Path(), FSType: "erofs", ReadOnly: true, Where: "pid 1 at " + cached},
	}
	if !reflect.DeepEqual(sbs, want) {
		t.Fatalf("superblocks %+v\nwant %+v", sbs, want)
	}
	if _, err := k.Superblocks(f.DevRoot() + "/pmem9"); err == nil {
		t.Fatal("no error for a device without a number")
	}
	// the mounts of the process
	if mi, err := k.Mounted(direct); err != nil || mi == nil || mi.Source != dev || mi.SuperReadOnly() {
		t.Fatalf("mounted %+v %v", mi, err)
	}
	if mi, err := k.Mounted(cached); err != nil || mi == nil || mi.Source != loop.Path() {
		t.Fatalf("mounted %+v %v", mi, err)
	}
	// the cached mount goes, and its autoclear loop device with it
	noErr(t, k.Unmount(cached))
	if l := k.LoopDevices(); len(l) != 0 {
		t.Fatalf("loop devices %+v", l)
	}
	f.SetBusy(direct, true)
	if err := k.Unmount(direct); !errors.Is(err, unix.EBUSY) {
		t.Fatalf("busy unmount: %v", err)
	}
}

// A block device is never made read-only under a read-write superblock in
// any mount namespace, nor when its superblocks cannot be read; a device
// that is gone is not an error.
func TestSetBlockDeviceReadOnly(t *testing.T) {
	f, k, dev := fsdaxNode(t)
	if changed, err := k.SetBlockDeviceReadOnly(dev, true); err != nil || !changed {
		t.Fatalf("changed %v %v", changed, err)
	}
	if !f.IsReadOnly(dev) {
		t.Fatal("not read-only")
	}
	if got := f.MountCalls("blkroset"); len(got) != 1 {
		t.Fatalf("calls %v", got)
	}
	// unchanged: no call
	if changed, err := k.SetBlockDeviceReadOnly(dev, true); err != nil || changed {
		t.Fatalf("unchanged: changed %v %v", changed, err)
	}
	noErr(t, setRO(k, dev, false))
	f.AddForeign(300, dev, "/run/cxl/fs/x", "ext4", false)
	err := setRO(k, dev, true)
	wantErr(t, err, unix.EBUSY, "not making "+dev+" read-only: "+dev+" is mounted read-write in pid 300 at /run/cxl/fs/x")
	if f.IsReadOnly(dev) {
		t.Fatal("made read-only under a read-write superblock")
	}
	f.RemoveForeign(300)
	f.HideProc(true)
	wantErr(t, setRO(k, dev, true), nil, "its superblocks")
	f.HideProc(false)
	// gone
	noErr(t, setRO(k, f.DevRoot()+"/pmem9", true))
	noErr(t, setRO(k, "", true))
	f.SetReadOnlyError(dev, syscall.EPERM)
	wantErr(t, setRO(k, dev, true), nil, "making "+dev+" read-only")
	if v := f.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

// A btrfs superblock has an anonymous device number: its mounts are
// matched by their source. A mounted btrfs that lists the device in sysfs
// without a mount of it found is a read-write superblock of the device.
// The guard of SetBlockDeviceReadOnly sees both.
func TestBtrfsSuperblocks(t *testing.T) {
	f, k, dev := fsdaxNode(t)
	noErr(t, setRO(k, dev, false))
	target := mountPoint(t, "btrfs")
	noErr(t, k.FSMount(dev, target, "btrfs", true, nil))
	sbs, err := k.Superblocks(dev)
	noErr(t, err)
	if len(sbs) != 1 || sbs[0].Device != dev || !strings.HasPrefix(sbs[0].DevNum, "0:") || sbs[0].ReadOnly {
		t.Fatalf("superblocks %+v", sbs)
	}
	wantErr(t, setRO(k, dev, true), unix.EBUSY, "not making "+dev+" read-only: "+dev+" is mounted read-write in pid 1 at "+target)
	noErr(t, k.Unmount(target))

	// a btrfs of another device is not one of dev
	f.AddForeign(300, "/dev/sdz", "/mnt/other", "btrfs", false)
	if sbs, err := k.Superblocks(dev); err != nil || len(sbs) != 0 {
		t.Fatalf("superblocks %+v %v", sbs, err)
	}
	// a mounted btrfs that lists dev, its mount not found by its source
	f.AddForeign(301, "/dev/disk/by-label/x", "/mnt/x", "btrfs", true)
	f.Write("fs/btrfs/e9a4c1d2/devices/"+filepath.Base(dev), "")
	sbs, err = k.Superblocks(dev)
	noErr(t, err)
	want := []kernel.Superblock{{Device: dev, Source: dev, FSType: "btrfs", Where: "btrfs e9a4c1d2, no mount of it found"}}
	if !reflect.DeepEqual(sbs, want) {
		t.Fatalf("superblocks %+v, want %+v", sbs, want)
	}
	wantErr(t, setRO(k, dev, true), unix.EBUSY, "is mounted read-write in btrfs e9a4c1d2")
}

// What holds a block device: a loop device over it (also one that a
// restarted container set up, whose backing sysfs shows as "/pmem0"), an
// exclusive open that fails; not a device that is gone.
func TestHolder(t *testing.T) {
	f, k, dev := fsdaxNode(t)
	if h, err := k.Holder(dev); err != nil || h != "" {
		t.Fatalf("holder %q %v", h, err)
	}
	loop, err := k.ConfigureLoop(dev, false)
	noErr(t, err)
	if h, err := k.Holder(dev); err != nil || h != filepath.Base(loop.Path()) {
		t.Fatalf("holder %q %v", h, err)
	}
	if b, err := k.LoopBackingFile(loop.Path()); err != nil || b != dev {
		t.Fatalf("backing %q %v", b, err)
	}
	// after a restart of the driver container sysfs shows the path
	// relative to the root of its /dev: the device number tells
	f.Write("block/loop0/loop/backing_file", "/pmem0")
	if b, err := k.LoopBackingFile(loop.Path()); err != nil || b != dev {
		t.Fatalf("backing after a restart %q %v", b, err)
	}
	if l := k.LoopDevices(); len(l) != 1 || l[0].Backing != dev || l[0].DevNum != "7:0" {
		t.Fatalf("loop devices %+v", l)
	}
	if b, err := k.LoopBackingFile("/dev/loop9"); err != nil || b != "" {
		t.Fatalf("backing of a missing loop device %q %v", b, err)
	}
	noErr(t, loop.Clear())
	if h, err := k.Holder(dev); err != nil || h != "" {
		t.Fatalf("holder after the clear %q %v", h, err)
	}
	f.SetHolder(dev, "a container's bind")
	if h, err := k.Holder(dev); err != nil || h != "in use" {
		t.Fatalf("holder %q %v", h, err)
	}
	if h, err := k.Holder(f.DevRoot() + "/pmem9"); err != nil || h != "" {
		t.Fatalf("holder of a missing device %q %v", h, err)
	}
	f.SetSyscallErrors(syscall.EACCES, nil, nil)
	wantErr(t, func() error { _, err := k.Holder(dev); return err }(), nil, "permission denied")
	for backing, want := range map[string]bool{"/dev/pmem0": true, "/dev/x/pmem0": false, "/tmp/pmem0": false, "pmem0": false, "/pmem0": true, "/pmem1": false, "/": false} {
		if got := kernel.SameDevice(backing, "/dev/pmem0"); got != want {
			t.Errorf("SameDevice(%q) = %v", backing, got)
		}
	}
	if p := kernel.DevPath("/pmem1"); p != "/dev/pmem1" {
		t.Errorf("DevPath %q", p)
	}
}

// Remount changes the superblock of every mount of it; a remount
// to read-only with a file open for writing is EBUSY; a read-only bind of a
// node mount reaches its target read-only.
func TestRemountAndBind(t *testing.T) {
	f, k, dev := fsdaxNode(t)
	node := mountPoint(t, "node")
	noErr(t, setRO(k, dev, false))
	noErr(t, k.FSMount(dev, node, "ext4", true, nil))
	bind := mountPoint(t, "bind")
	noErr(t, k.BindReadOnly(node, bind))
	if mi, _ := k.Mounted(bind); mi == nil || !slices.Contains(mi.MountFlags, "ro") || mi.SuperReadOnly() {
		t.Fatalf("bind %+v", mi)
	}
	f.SetWriting(node, true)
	if err := k.Remount(node, false); !errors.Is(err, unix.EBUSY) {
		t.Fatalf("remount while writing: %v", err)
	}
	f.SetWriting(node, false)
	noErr(t, k.Syncfs(node))
	noErr(t, k.Remount(node, false))
	if mi, _ := k.Mounted(bind); mi == nil || !mi.SuperReadOnly() {
		t.Fatalf("bind after the remount %+v", mi)
	}
	noErr(t, setRO(k, dev, true))
	if err := k.Remount(node, true); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("read-write on a read-only device: %v", err)
	}
	if v := f.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

// The signatures of the known filesystem types in the head of a block
// device.
func TestParseFsSignature(t *testing.T) {
	for _, tc := range []struct {
		image    []string
		types    []string
		proto    string
		recovery bool
	}{
		{nil, nil, "", false},
		{[]string{"erofs"}, []string{"erofs"}, "", false},
		{[]string{"squashfs"}, []string{"squashfs"}, "", false},
		{[]string{"ext4"}, []string{"ext4"}, "", false},
		{[]string{"ext4-dirty"}, []string{"ext4"}, "", true},
		{[]string{"xfs"}, []string{"xfs"}, "", false},
		{[]string{"btrfs"}, []string{"btrfs"}, "", false},
		{[]string{"gfs2"}, []string{"gfs2"}, "lock_dlm", false},
		{[]string{"gfs2-nolock"}, []string{"gfs2"}, "lock_nolock", false},
		{[]string{"xfs", "ext4"}, []string{"ext4", "xfs"}, "", false},
	} {
		s := kernel.ParseFSSignature(fake.FSImage(tc.image...))
		if !slices.Equal(s.Types, tc.types) || s.Gfs2LockProto != tc.proto || s.Ext4NeedsRecovery != tc.recovery {
			t.Errorf("%v: %+v", tc.image, s)
		}
	}
	// every filesystem type that the drivers know has its signature
	for _, name := range api.KnownFSTypes {
		if s := kernel.ParseFSSignature(fake.FSImage(name)); !slices.Equal(s.Types, []string{name}) {
			t.Errorf("%s: %+v", name, s)
		}
	}
	// a gfs2 magic without the superblock meta type is not gfs2
	img := fake.FSImage("gfs2")
	img[65543] = 2
	if s := kernel.ParseFSSignature(img); len(s.Types) != 0 {
		t.Errorf("gfs2 of meta type 2: %+v", s)
	}
	// a short head matches what it covers
	if s := kernel.ParseFSSignature(fake.FSImage("ext4", "btrfs")[:4096]); !slices.Equal(s.Types, []string{"ext4"}) {
		t.Errorf("short head: %+v", s)
	}
	if s := kernel.ParseFSSignature([]byte("XF")); len(s.Types) != 0 {
		t.Errorf("tiny head: %+v", s)
	}
}

// The head of a block device is read for its signatures; the data of a
// persistent memdev outlives its block device.
func TestReadFsSignature(t *testing.T) {
	f, k, dev := fsdaxNode(t)
	f.SetImage(dev, fake.FSImage("erofs"))
	s, err := k.ReadFSSignature(dev)
	if err != nil || !slices.Equal(s.Types, []string{api.FSTypeErofs}) {
		t.Fatalf("signature %+v %v", s, err)
	}
	if head, err := k.ReadHead(dev, 10); err != nil || len(head) != 10 {
		t.Fatalf("head %d bytes %v", len(head), err)
	}
	if _, err := k.ReadFSSignature(f.DevRoot() + "/pmem9"); err == nil {
		t.Fatal("signature of a missing device")
	}
	// a reboot: the data stay, the block device comes back
	f.Reboot()
	createRegion(t, k, "mem0", kernel.RegionPmem, 0)
	nss, err := k.Namespaces(ctxTimeout(t, 5*time.Second), "region0")
	noErr(t, err)
	ns := kernel.FsdaxNamespace(nss)
	if ns == nil {
		t.Fatalf("namespaces after a reboot %+v", nss)
	}
	if s, err := k.ReadFSSignature(ns.BlockDevice); err != nil || !slices.Equal(s.Types, []string{api.FSTypeErofs}) || ns.Minor == 0 {
		t.Fatalf("after a reboot: %s %d:%d signature %+v %v", ns.BlockDevice, ns.Major, ns.Minor, s, err)
	}
	// a new probe gives the block device another name and numbers
	renamed := f.RenamePmem("region0", "pmem3", "259:9")
	nss, err = k.Namespaces(ctxTimeout(t, 5*time.Second), "region0")
	noErr(t, err)
	if ns := kernel.FsdaxNamespace(nss); ns == nil || ns.BlockDevice != renamed || ns.Minor != 9 {
		t.Fatalf("renamed: %+v", nss)
	}
	if s, err := k.ReadFSSignature(renamed); err != nil || !slices.Equal(s.Types, []string{api.FSTypeErofs}) {
		t.Fatalf("renamed signature %+v %v", s, err)
	}
}

// setRO sets the read-only flag of a block device and returns the error.
func setRO(k *kernel.Kernel, dev string, readOnly bool) error {
	_, err := k.SetBlockDeviceReadOnly(dev, readOnly)
	return err
}
