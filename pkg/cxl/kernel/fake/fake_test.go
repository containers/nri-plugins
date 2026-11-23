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

package fake_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"fmt"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// cut runs fn and tells whether it stopped with ErrCut.
func cut(t *testing.T, fn func()) (stopped bool) {
	t.Helper()
	defer func() {
		switch r := recover(); r {
		case nil:
		case fake.ErrCut:
			stopped = true
		default:
			panic(r)
		}
	}()
	fn()
	return false
}

// The hooks of the tests: a stop before or after a command or a write, a
// command or a write that fails, functions before and after one; each is
// used once.
func TestHooks(t *testing.T) {
	f := fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: 0xc1}) // mem0
	k := f.Kernel()
	ctx := context.Background()
	req := kernel.CreateRegionRequest{Memdev: "mem0", Decoder: "decoder0.0", Type: kernel.RegionRAM}

	f.CutBefore("cxl create-region")
	if !cut(t, func() { _, _ = k.CreateRegion(ctx, req) }) || strings.Contains(f.Commands(), "create-region") {
		t.Fatalf("not stopped before:\n%s", f.Commands())
	}
	f.CutAfter("cxl create-region")
	if !cut(t, func() { _, _ = k.CreateRegion(ctx, req) }) || !f.Exists("bus/cxl/devices/region0") {
		t.Fatalf("not stopped after:\n%s", f.Commands())
	}
	f.FailCommand("cxl disable-region", fake.ErrAddDev, nil)
	if err := k.DisableRegion(ctx, "region0"); !errors.Is(err, fake.ErrAddDev) {
		t.Fatalf("expected the failure, got %v", err)
	}
	if err := k.DisableRegion(ctx, "region0"); err != nil {
		t.Fatalf("the second disable-region failed: %v", err)
	}
	noErr(t, k.EnableRegion(ctx, "region0"))
	f.FailWrite("bus/dax/devices/dax0.0/size", syscall.EIO)
	noErr(t, k.UnbindDax(ctx, "dax0.0"))
	if err := k.ResizeDaxDevice(ctx, "dax0.0", 2<<20); !errors.Is(err, syscall.EIO) {
		t.Fatalf("expected the write failure, got %v", err)
	}
	var order []string
	f.Before("write bus/dax/devices/dax0.0/size", func() { order = append(order, "before") })
	f.After("write bus/dax/devices/dax0.0/size", func() { order = append(order, "after") })
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 2<<20))
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 4<<20))
	if strings.Join(order, ",") != "before,after" {
		t.Fatalf("hooks %v", order)
	}
	f.CutAfter("write bus/dax/devices/dax0.0/size 0")
	if !cut(t, func() { _ = k.ResizeDaxDevice(ctx, "dax0.0", 0) }) || f.DaxSize("dax0.0") != 0 {
		t.Fatalf("not stopped after the write: size %d", f.DaxSize("dax0.0"))
	}
	// a stop after a mount at a target
	dir := filepath.Join(t.TempDir(), "m")
	noErr(t, os.MkdirAll(dir, 0o755))
	f.CutMount(dir)
	if !cut(t, func() { _ = k.Mount("tmpfs", dir, "tmpfs", 0, "") }) || len(f.MountTable()) != 1 {
		t.Fatalf("not stopped after the mount: %+v", f.MountTable())
	}
}

// A reboot: a new boot id, nothing of the regions, dax devices, memory
// blocks and mounts of the old boot, the memdevs enabled again.
func TestReboot(t *testing.T) {
	f := fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: 0xc1})
	k := f.Kernel()
	ctx := context.Background()
	d, err := k.RootDecoder("mem0", kernel.RegionRAM)
	noErr(t, err)
	_, err = k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: "mem0", Decoder: d, Type: kernel.RegionRAM})
	noErr(t, err)
	noErr(t, k.OnlineBlocks([]int{74}, true))
	boot := f.BootID()
	f.Reboot()
	s, err := k.Observe(ctx)
	noErr(t, err)
	if b, _ := k.BootID(); b == boot || len(s.Regions) != 0 || len(s.DaxDevices) != 0 || f.BlockState(74) != "" || len(s.Memdevs) != 1 || s.Memdevs[0].State != kernel.MemdevReady {
		t.Fatalf("after the reboot: %+v", s)
	}
	if st := f.Stack("port1"); len(st) != 0 {
		t.Fatalf("port1 after the reboot %v", st)
	}
	if n, _ := s.NUMANode(2); n.HasMemory {
		t.Fatalf("node 2 has memory after the reboot")
	}
}

// The rules of the kernel that the package guards against: an offline of
// an offline memory block is EINVAL; disable-region offlines the memory of
// kmem and fails if it cannot; a bind of device_dax without its id is
// ENODEV; the mapping of a bound dax device is written; delete_region
// deletes a bound region, a violation when it is not hollow; a mount that
// would change the read-only state of a superblock is EBUSY.
func TestKernelRules(t *testing.T) {
	f := fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: 0xc1}) // mem0
	k := f.Kernel()
	ctx := context.Background()
	write := func(rel, val string) error { return f.WriteAttr(f.P(rel), val) }
	region, err := k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: "mem0", Decoder: "decoder0.0", Type: kernel.RegionRAM})
	noErr(t, err)
	if err := write("devices/system/memory/memory74/state", kernel.BlockStateOffline); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("offline of an offline block: %v", err)
	}
	noErr(t, k.OnlineBlocks([]int{74}, true))
	f.FailWrite("devices/system/memory/memory74/state", syscall.EBUSY)
	if _, err := f.Exec(ctx, "cxl", "disable-region", region); !errors.Is(err, syscall.EBUSY) || f.BlockState(74) != kernel.BlockStateOnline {
		t.Fatalf("disable-region with memory that cannot be offlined: %v, memory74 %q", err, f.BlockState(74))
	}
	if _, err := f.Exec(ctx, "cxl", "disable-region", region); err != nil || f.BlockState(74) != "" || f.DaxExists("dax0.0") {
		t.Fatalf("disable-region: %v, memory74 %q", err, f.BlockState(74))
	}

	_, err = f.Exec(ctx, "cxl", "enable-region", region)
	noErr(t, err)
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 2<<20))
	if err := write("bus/dax/drivers/"+kernel.DriverDeviceDax+"/bind", "dax0.0"); !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("bind of device_dax without the id: %v", err)
	}
	noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverDeviceDax))
	g := f.Gaps(region)[0]
	noErr(t, write("bus/dax/devices/dax0.0/mapping", fmt.Sprintf("%#x-%#x", g.Start, g.Start+2<<20-1)))
	if size := f.DaxSize("dax0.0"); size != 4<<20 {
		t.Fatalf("size %d after the mapping of a bound device", size)
	}
	noErr(t, write("bus/cxl/devices/decoder0.0/delete_region", region))
	if v := f.Violations(); f.Exists("bus/cxl/devices/"+region) || f.DaxExists("dax0.0") || len(v) != 1 {
		t.Fatalf("delete_region of a bound region: violations %q", v)
	}

	source := filepath.Join(f.DevRoot(), "pmem9")
	targets := []string{filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")}
	for _, d := range targets {
		noErr(t, os.MkdirAll(d, 0o755))
	}
	noErr(t, k.Mount(source, targets[0], "erofs", syscall.MS_RDONLY, ""))
	if err := k.Mount(source, targets[1], "erofs", 0, ""); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("read-write mount of a read-only superblock: %v", err)
	}
	if err := k.FSMount(source, targets[1], "erofs", true, nil); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("read-write fsmount of a read-only superblock: %v", err)
	}
	noErr(t, k.FSMount(source, targets[1], "erofs", false, nil))
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
