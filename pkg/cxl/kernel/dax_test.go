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
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// daxNode returns a fake with a 1 GiB ram region0 on mem0 whose dax0.0
// is unbound with all of the memory, its blocks gone.
func daxNode(t *testing.T) (*fake.Fake, *kernel.Kernel) {
	t.Helper()
	f := fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: serial0, Ram: gib})
	k := f.Kernel()
	createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	noErr(t, k.UnbindDax(context.Background(), "dax0.0"))
	return f, k
}

// A region is sliced into dax devices, which are bound, deleted, and
// recognized by their state.
func TestDaxDevices(t *testing.T) {
	f, k := daxNode(t)
	ctx := context.Background()
	d, err := k.DaxRegion("region0")
	if err != nil || d.Name != "dax_region0" || d.Available != 0 || d.Align != fake.DaxAlign || !reflect.DeepEqual(d.Devices, []string{"dax0.0"}) {
		t.Fatalf("dax region %+v %v", d, err)
	}
	if _, err := k.DaxRegion("region7"); err == nil {
		t.Fatal("dax region of a missing region")
	}
	// the first slice: dax0.0 shrinks
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 128*mib))
	if f.DaxDriver("dax0.0") != "" || f.DaxSize("dax0.0") != 128*mib {
		t.Fatalf("dax0.0 bound %q size %d", f.DaxDriver("dax0.0"), f.DaxSize("dax0.0"))
	}
	if err := k.ResizeDaxDevice(ctx, "dax0.0", mib); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("a misaligned size: %v", err)
	}
	noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverDeviceDax))
	// more slices, first-fit
	dax, err := k.CreateDaxDevice(ctx, "region0", 256*mib)
	if err != nil || dax != "dax0.1" || f.DaxDriver(dax) != "" || f.DaxSize(dax) != 256*mib {
		t.Fatalf("created %q %v", dax, err)
	}
	noErr(t, k.BindDax(ctx, dax, kernel.DriverDeviceDax))
	info, err := k.DaxDevice(dax)
	noErr(t, err)
	want := kernel.DaxDeviceInfo{Name: "dax0.1", Region: "region0", State: kernel.DaxDeviceDax, Driver: kernel.DriverDeviceDax, Size: 256 * mib,
		Ranges: []kernel.Range{{Start: window0 + 128*mib, End: window0 + 384*mib - 1}}, Major: 252, Minor: 1, TargetNode: 2, WholeBlocks: 2}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("dax device %+v\nwant %+v", info, want)
	}
	if d, _ := k.DaxRegion("region0"); d.Available != gib-384*mib || len(d.Devices) != 2 {
		t.Fatalf("dax region %+v", d)
	}
	// no room: the new device is deleted again
	_, err = k.CreateDaxDevice(ctx, "region0", gib)
	wantErr(t, err, kernel.ErrNoRoom, "set size")
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("no room: %v is not ENOSPC", err)
	}
	if devs := k.DaxDevices("region0"); len(devs) != 2 {
		t.Fatalf("a failed creation left devices %v", devs)
	}
	// a bound device is not resized while bound: it is unbound first
	noErr(t, k.ResizeDaxDevice(ctx, dax, 128*mib))
	if f.DaxDriver(dax) != "" {
		t.Fatal("resized device still bound")
	}
	// deletion, of a missing device too
	noErr(t, k.DeleteDaxDevice(ctx, "region0", dax))
	if f.DaxExists(dax) {
		t.Fatalf("%s not deleted", dax)
	}
	noErr(t, k.DeleteDaxDevice(ctx, "region0", dax))
	// dax0.0 is kept with size 0
	noErr(t, k.DeleteDaxDevice(ctx, "region0", "dax0.0"))
	if info, _ := k.DaxDevice("dax0.0"); info.State != kernel.DaxSeed || f.DaxDriver("dax0.0") != "" {
		t.Fatalf("dax0.0 %+v", info)
	}
	if err := k.BindDax(ctx, "dax0.0", kernel.DriverDeviceDax); err == nil {
		t.Fatal("a device of size 0 was bound")
	}
	if d, _ := k.DaxRegion("region0"); d.Available != gib {
		t.Fatalf("available %d after deleting all slices", d.Available)
	}
	// a full region has no room for another device
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", gib))
	_, err = k.CreateDaxDevice(ctx, "region0", fake.DaxAlign)
	wantErr(t, err, kernel.ErrNoRoom, "create a dax device")
	if strings.Contains(f.Commands(), "daxctl") {
		t.Fatalf("daxctl ran:\n%s", f.Commands())
	}
}

// First-fit placement of a slice that grows across gaps: a device of two
// ranges, its whole memory blocks, and the gaps of the region.
func TestDaxDeviceRanges(t *testing.T) {
	f, k := daxNode(t)
	ctx := context.Background()
	if r, err := k.DaxDeviceRanges("dax0.0"); err != nil || !reflect.DeepEqual(r, []kernel.Range{{Start: window0, End: window0 + gib - 1}}) {
		t.Fatalf("ranges %v %v", r, err)
	}
	if blocks, err := k.DaxDeviceBlocks("dax0.0", bs); err != nil || len(blocks) != 0 {
		t.Fatalf("unbound kmem: blocks %v %v", blocks, err)
	}
	if _, err := k.DaxDeviceBlocks("dax0.0", 0); err == nil {
		t.Fatal("block size 0 accepted")
	}
	if gaps, err := k.RegionFreeGaps("region0"); err != nil || len(gaps) != 0 {
		t.Fatalf("gaps of a full region %v %v", gaps, err)
	}
	// dax0.0 keeps the first 130 MiB, dax0.1 the next 128 MiB; dax0.0
	// shrinks to 2 MiB, and dax0.2 of 384 MiB fills the gap after it and
	// takes 256 MiB after dax0.1
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 130*mib))
	d1, err := k.CreateDaxDevice(ctx, "region0", 128*mib)
	noErr(t, err)
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 2*mib))
	d2, err := k.CreateDaxDevice(ctx, "region0", 384*mib)
	noErr(t, err)
	want := []kernel.Range{{Start: window0 + 2*mib, End: window0 + 130*mib - 1}, {Start: window0 + 258*mib, End: window0 + 514*mib - 1}}
	if r, _ := k.DaxDeviceRanges(d2); !reflect.DeepEqual(r, want) {
		t.Fatalf("%s: ranges %v, want %v", d2, r, want)
	}
	if want[0].Aligned(bs) || want[1].Aligned(bs) || !want[0].Aligned(fake.DaxAlign) {
		t.Fatalf("alignment of %v", want)
	}
	// kmem adds only the blocks wholly in its memory
	noErr(t, k.BindDax(ctx, d2, kernel.DriverKmem))
	noErr(t, k.WaitDaxBlocks(ctxTimeout(t, time.Second), d2))
	if blocks, _ := k.DaxDeviceBlocks(d2, bs); !reflect.DeepEqual(blocks, []int{(window0 + 384*mib) / bs}) {
		t.Fatalf("blocks of %s %v", d2, blocks)
	}
	if blocks, _ := k.DaxDeviceBlocks(d1, bs); len(blocks) != 0 {
		t.Fatalf("blocks of unbound %s %v", d1, blocks)
	}
	gaps, err := k.RegionFreeGaps("region0")
	if err != nil || !reflect.DeepEqual(gaps, []kernel.Range{{Start: window0 + 514*mib, End: window0 + gib - 1}}) {
		t.Fatalf("gaps %v %v", gaps, err)
	}
	// a device of size 0 has no memory, and no whole block to wait for
	d3, err := k.CreateDaxDevice(ctx, "region0", 2*mib)
	noErr(t, err)
	noErr(t, k.ResizeDaxDevice(ctx, d3, 0))
	if r, err := k.DaxDeviceRanges(d3); err != nil || len(r) != 0 {
		t.Fatalf("ranges of an empty device %v %v", r, err)
	}
	wantErr(t, k.WaitDaxBlocks(ctx, d3), nil, d3+" has no whole memory block")
	// a block that does not come
	f.Remove("devices/system/memory/memory" + itoa((window0+384*mib)/bs))
	wantErr(t, k.WaitDaxBlocks(ctxTimeout(t, 30*time.Millisecond), d2), context.DeadlineExceeded, "0 of 1 memory blocks appeared")
	// a bad mapping
	f.Write("bus/dax/devices/"+d2+"/mapping0/end", "bogus")
	if _, err := k.DaxDeviceRanges(d2); err == nil {
		t.Fatal("a bad mapping accepted")
	}
	if _, err := k.RegionFreeGaps("region0"); err == nil {
		t.Fatal("gaps with a bad mapping")
	}
}

// A dax device is placed explicitly, and the placement checked: memory that
// is taken fails (ErrNoRoom), a length that the kernel ignores deletes the
// device.
func TestCreateDaxDeviceRanges(t *testing.T) {
	f, k := daxNode(t)
	ctx := context.Background()
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 64*mib))
	// the block-aligned second block
	second := kernel.Range{Start: window0 + bs, End: window0 + 2*bs - 1}
	dax, err := k.CreateDaxDeviceRanges(ctx, "region0", []kernel.Range{second})
	if err != nil || dax != "dax0.1" || f.DaxDriver(dax) != "" || f.DaxSize(dax) != bs {
		t.Fatalf("created %q %v", dax, err)
	}
	if r, _ := k.DaxDeviceRanges(dax); !reflect.DeepEqual(r, []kernel.Range{second}) {
		t.Fatalf("ranges %v", r)
	}
	for _, tc := range []struct {
		r    kernel.Range
		kind error
		err  string
	}{
		{kernel.Range{Start: window0 + bs, End: window0 + bs + 64*mib - 1}, kernel.ErrNoRoom, "cannot allocate memory"},
		{kernel.Range{Start: window0 + 64*mib, End: window0 + 65*mib - 1}, nil, "memory [] after mapping [0x254000000-0x2540fffff]"},
	} {
		_, err := k.CreateDaxDeviceRanges(ctx, "region0", []kernel.Range{tc.r})
		wantErr(t, err, tc.kind, tc.err)
	}
	if devs := k.DaxDevices("region0"); len(devs) != 2 {
		t.Fatalf("failed placements left devices %v", devs)
	}
	// two ranges, listed in any order
	noErr(t, k.DeleteDaxDevice(ctx, "region0", dax))
	two := []kernel.Range{{Start: window0 + 3*bs, End: window0 + 4*bs - 1}, {Start: window0 + 64*mib, End: window0 + 96*mib - 1}}
	dax, err = k.CreateDaxDeviceRanges(ctx, "region0", two)
	if err != nil || f.DaxSize(dax) != bs+32*mib {
		t.Fatalf("created %q %v", dax, err)
	}
	if r, _ := k.DaxDeviceRanges(dax); !reflect.DeepEqual(r, []kernel.Range{two[1], two[0]}) {
		t.Fatalf("ranges %v", r)
	}
	noErr(t, k.DeleteDaxDevice(ctx, "region0", dax))
	_, err = k.CreateDaxDeviceRanges(ctx, "region0", []kernel.Range{two[0], {Start: window0 + 64*mib, End: window0 + 65*mib - 1}})
	wantErr(t, err, nil, "after mapping")
	_, err = k.CreateDaxDeviceRanges(ctx, "region0", nil)
	wantErr(t, err, nil, "a dax device without memory")
	if devs := k.DaxDevices("region0"); len(devs) != 1 {
		t.Fatalf("failed placements left devices %v", devs)
	}
}

// kmem is never unbound from memory that is online: the kernel could not
// remove it.
func TestUnbindKmemOnline(t *testing.T) {
	f, k := newNode(t)
	ctx := context.Background()
	createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	noErr(t, k.OnlineBlocks([]int{74, 75}, true))
	noErr(t, k.OfflineBlocks([]int{74}))
	err := k.UnbindDax(ctx, "dax0.0")
	wantErr(t, err, kernel.ErrBusyOnline, "dax0.0: memory blocks [75] are not offline, kmem keeps it")
	if f.DaxDriver("dax0.0") != kernel.DriverKmem {
		t.Fatal("dax0.0 unbound")
	}
	if err := k.BindDax(ctx, "dax0.0", kernel.DriverDeviceDax); !errors.Is(err, kernel.ErrBusyOnline) || f.DaxDriver("dax0.0") != kernel.DriverKmem {
		t.Fatalf("switch from online kmem memory: %v", err)
	}
	if err := k.ResizeDaxDevice(ctx, "dax0.0", 0); !errors.Is(err, kernel.ErrBusyOnline) || f.DaxSize("dax0.0") != 256*mib {
		t.Fatalf("resize of online kmem memory: %v", err)
	}
	noErr(t, k.OfflineBlocks([]int{75}))
	noErr(t, k.UnbindDax(ctx, "dax0.0"))
	if f.DaxDriver("dax0.0") != "" {
		t.Fatal("dax0.0 still bound")
	}
	// unbinding an unbound device is fine
	noErr(t, k.UnbindDax(ctx, "dax0.0"))
	// without a block size nothing can be checked: kmem stays
	noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverKmem))
	f.Remove("devices/system/memory/block_size_bytes")
	if err := k.UnbindDax(ctx, "dax0.0"); err == nil || f.DaxDriver("dax0.0") != kernel.DriverKmem {
		t.Fatalf("unbind without a block size: %v", err)
	}
}

// BindDax binds by sysfs only: kmem with bind (a new_id would make it take
// every unbound dax device of the CXL regions), device_dax with new_id and
// bind, the id of the driver removed after every bind.
func TestBindDaxWrites(t *testing.T) {
	f, k := newNode(t)
	ctx := context.Background()
	createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	noErr(t, k.UnbindDax(ctx, "dax0.0"))
	f.Writes()
	noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverDeviceDax))
	if w := strings.Join(f.Writes(), ","); w != "bus/dax/drivers/device_dax/new_id,bus/dax/drivers/device_dax/remove_id" {
		t.Fatalf("device_dax bind writes %s", w)
	}
	noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverKmem))
	if w := strings.Join(f.Writes(), ","); w != "bus/dax/drivers/device_dax/remove_id,bus/dax/drivers/device_dax/unbind,bus/dax/drivers/kmem/bind,bus/dax/drivers/kmem/remove_id" {
		t.Fatalf("kmem bind writes %s", w)
	}
	noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverKmem))
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("a bound device bound again: %v", w)
	}
	if f.HasDaxID(kernel.DriverDeviceDax, "dax0.0") || f.HasDaxID(kernel.DriverKmem, "dax0.0") {
		t.Fatal("an id left")
	}
	if strings.Contains(f.Commands(), "daxctl") {
		t.Fatalf("daxctl ran:\n%s", f.Commands())
	}
}

// Whoever writes a new_id of a dax driver makes the kernel attach the
// driver to every unbound dax device whose name it has as an id. A bind of
// another device between UnbindDax and the size write does not take the
// device back: no id is left after a bind, and UnbindDax removes the id
// first, also one that another tool left.
func TestDaxIDsNotLeft(t *testing.T) {
	for _, left := range []bool{false, true} {
		f, k := daxNode(t)
		ctx := context.Background()
		noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 128*mib))
		other, err := k.CreateDaxDevice(ctx, "region0", 64*mib)
		noErr(t, err)
		noErr(t, k.BindDax(ctx, "dax0.0", kernel.DriverDeviceDax))
		if f.HasDaxID(kernel.DriverDeviceDax, "dax0.0") {
			t.Fatal("device_dax has the id dax0.0 after binding it")
		}
		if left {
			f.SetDaxID(kernel.DriverDeviceDax, "dax0.0")
		}
		// the other Prepare binds its device between the unbind of dax0.0
		// and the write of its size
		f.Before("write bus/dax/devices/dax0.0/size", func() {
			if err := k.BindDax(ctx, other, kernel.DriverDeviceDax); err != nil {
				t.Errorf("the other bind: %v", err)
			}
		})
		noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 64*mib))
		if f.DaxDriver("dax0.0") != "" || f.DaxSize("dax0.0") != 64*mib || f.DaxDriver(other) != kernel.DriverDeviceDax {
			t.Fatalf("left %v: dax0.0 %q %d, %s %q", left, f.DaxDriver("dax0.0"), f.DaxSize("dax0.0"), other, f.DaxDriver(other))
		}
		// a failed bind leaves no id either: dax0.0 has size 0 now
		noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 0))
		if err := k.BindDax(ctx, "dax0.0", kernel.DriverDeviceDax); err == nil {
			t.Fatal("a device of size 0 was bound")
		}
		if f.HasDaxID(kernel.DriverDeviceDax, "dax0.0") || f.HasDaxID(kernel.DriverDeviceDax, other) {
			t.Fatal("an id left")
		}
	}
}

// The dax devices of a region of the kernel: daxN.0 of a new ram region is
// bound to kmem by the kernel with its blocks added offline, which appear
// after the driver link.
func TestDaxDefaultKmem(t *testing.T) {
	f, k := newNode(t)
	f.NoKmemBlocks = true
	createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	dax, err := k.WaitDaxRegion(ctxTimeout(t, time.Second), "region0")
	if err != nil || dax != "dax0.0" || f.DaxDriver(dax) != kernel.DriverKmem {
		t.Fatalf("first dax device %q %v", dax, err)
	}
	wantErr(t, k.WaitRegionBlocks(ctxTimeout(t, 30*time.Millisecond), "region0"), context.DeadlineExceeded, "0 of 2 memory blocks appeared")
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.AddBlocks(74, 75)
	}()
	noErr(t, k.WaitRegionBlocks(ctxTimeout(t, 5*time.Second), "region0"))
	_, err = k.WaitDaxRegion(ctxTimeout(t, 20*time.Millisecond), "region9")
	wantErr(t, err, context.DeadlineExceeded, "no dax device bound to a driver appeared in region9")
	noErr(t, k.UnbindDax(context.Background(), dax))
	_, err = k.WaitDaxRegion(ctxTimeout(t, 20*time.Millisecond), "region0")
	wantErr(t, err, context.DeadlineExceeded, "no dax device bound to a driver appeared in region0")
	if dax, err = k.WaitFirstDax(ctxTimeout(t, time.Second), "region0"); err != nil || dax != "dax0.0" {
		t.Fatalf("first unbound dax device %q %v", dax, err)
	}
	_, err = k.WaitFirstDax(ctxTimeout(t, 20*time.Millisecond), "region9")
	wantErr(t, err, context.DeadlineExceeded, "no dax device appeared in region9")
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

// A read of the mappings of a dax device waits for a write of the size or
// of a mapping of a dax device, which may remove a mapping: the kernel
// would wait for the reader inside the files of the mapping while the
// reader waits for the writer (drivers/dax/bus.c). The read then has the
// memory after the write.
func TestResizeDaxDeviceExcludesMappingReads(t *testing.T) {
	f, k := daxNode(t)
	ctx := context.Background()
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 128*mib))
	first := kernel.Range{Start: window0, End: window0 + 128*mib - 1}
	var got []kernel.Range
	var returned bool
	var done <-chan struct{}
	f.Before("write bus/dax/devices/dax0.0/size 0", func() {
		returned, done = fake.ReadWithin(func() { got, _ = k.DaxDeviceRanges("dax0.0") })
	})
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 0))
	<-done
	if returned || got != nil {
		t.Fatalf("a read of the mappings during the shrink: returned inside it %v, memory %v", returned, got)
	}
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 128*mib))
	f.Before("write bus/dax/devices/dax0.1/mapping", func() {
		returned, done = fake.ReadWithin(func() { got, _ = k.DaxDeviceRanges("dax0.0") })
	})
	second := kernel.Range{Start: window0 + 256*mib, End: window0 + 384*mib - 1}
	dax, err := k.CreateDaxDeviceRanges(ctx, "region0", []kernel.Range{second})
	noErr(t, err)
	<-done
	if dax != "dax0.1" || returned || !reflect.DeepEqual(got, []kernel.Range{first}) {
		t.Fatalf("a read of the mappings during the mapping of %s: returned inside it %v, memory %v", dax, returned, got)
	}
}

// A size write to a dax device waits for the delete of a dax device and
// for cxl disable-region and destroy-region: the kernel would hold
// dax_region_rwsem across the removal of the device while the size write
// to it waits for the rwsem inside the attribute (drivers/dax/bus.c).
func TestDaxRemovalsExcludeSizeWrites(t *testing.T) {
	f, k := daxNode(t)
	ctx := context.Background()
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 128*mib))
	other, err := k.CreateDaxDevice(ctx, "region0", 128*mib)
	noErr(t, err)
	returned := map[string]bool{}
	var done []<-chan struct{}
	resizeDuring := func(what string) func() {
		return func() {
			r, d := fake.ReadWithin(func() { _ = k.ResizeDaxDevice(ctx, "dax0.0", 64*mib) })
			returned[what], done = r, append(done, d)
		}
	}
	f.Before("write bus/cxl/devices/region0/dax_region0/dax_region/delete "+other, resizeDuring("delete"))
	noErr(t, k.DeleteDaxDevice(ctx, "region0", other))
	f.Before("cxl disable-region region0", resizeDuring("disable-region"))
	f.Before("cxl destroy-region region0", resizeDuring("destroy-region"))
	noErr(t, k.DestroyRegion(ctx, "region0"))
	for _, d := range done {
		<-d
	}
	if len(done) != 3 || returned["delete"] || returned["disable-region"] || returned["destroy-region"] {
		t.Fatalf("size writes during the removals: %d, returned %v", len(done), returned)
	}
}

// The topology reads no mapping of a dax device: a scan during a write of
// the size of a dax device does not wait for it, while a full observation,
// which reads the memory of the dax devices, waits and has the memory
// after the write.
func TestObserveTopologyDuringDaxWrite(t *testing.T) {
	f, k := daxNode(t)
	ctx := context.Background()
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 128*mib))
	var top, full *kernel.Snapshot
	var topErr, fullErr error
	var topReturned, fullReturned bool
	var topDone, fullDone <-chan struct{}
	f.Before("write bus/dax/devices/dax0.0/size 0", func() {
		fullReturned, fullDone = fake.ReadWithin(func() { full, fullErr = k.Observe(ctx) })
		topReturned, topDone = fake.ReadWithin(func() { top, topErr = k.ObserveTopology(ctx) })
	})
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 0))
	<-topDone
	<-fullDone
	noErr(t, topErr)
	noErr(t, fullErr)
	if !topReturned || fullReturned {
		t.Fatalf("during the shrink of dax0.0: the topology returned %v, the full observation %v", topReturned, fullReturned)
	}
	if d := top.DaxDevice("dax0.0"); d.Size != 128*mib || d.Ranges != nil || d.WholeBlocks != 0 {
		t.Fatalf("dax0.0 of the topology %+v, expected its size before the shrink and no memory", d)
	}
	if d := full.DaxDevice("dax0.0"); d.Ranges != nil || d.WholeBlocks != 0 {
		t.Fatalf("dax0.0 of the full observation %+v, expected no memory", d)
	}
}

// The mappings of a dax device that the size writes of another goroutine
// change are read before or after each write, never in between: its
// memory is none or all of the first slice; the observations of the node
// run beside them without an error. The readers run until the writer has
// made its changes a number of times.
func TestDaxMappingsRace(t *testing.T) {
	f, k := daxNode(t)
	ctx := context.Background()
	noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 0))
	first := []kernel.Range{{Start: window0, End: window0 + 128*mib - 1}}
	second := []kernel.Range{{Start: window0 + 256*mib, End: window0 + 384*mib - 1}}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	loop := func(fn func() error) {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := fn(); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	loop(func() error {
		got, err := k.DaxDeviceRanges("dax0.0")
		if err != nil || (got != nil && !reflect.DeepEqual(got, first)) {
			return fmt.Errorf("memory of dax0.0 %v, %v", got, err)
		}
		return nil
	})
	loop(func() error { _, err := k.Observe(ctx); return err })
	loop(func() error { _, err := k.ObserveTopology(ctx); return err })
	for range 100 {
		noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 128*mib))
		dax, err := k.CreateDaxDeviceRanges(ctx, "region0", second)
		noErr(t, err)
		noErr(t, k.DeleteDaxDevice(ctx, "region0", dax))
		noErr(t, k.ResizeDaxDevice(ctx, "dax0.0", 0))
	}
	close(stop)
	wg.Wait()
	if s := f.DaxSize("dax0.0"); s != 0 && s != 128*mib {
		t.Fatalf("dax0.0 has %d bytes", s)
	}
}
