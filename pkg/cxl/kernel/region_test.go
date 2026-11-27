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
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The name of the next region of the root decoder is noted before cxl
// create-region runs; an error of the note creates nothing.
func TestCreateRegionNoted(t *testing.T) {
	f, k := newNode(t)
	var notes []string
	note := func(next string) error {
		if strings.Contains(f.Commands(), "create-region") {
			t.Error("the note after cxl create-region")
		}
		notes = append(notes, next)
		return nil
	}
	ctx := context.Background()
	req := kernel.CreateRegionRequest{Memdev: "mem0", Decoder: "decoder0.0", Type: kernel.RegionRAM, Note: note}
	r, err := k.CreateRegion(ctx, req)
	if err != nil || r != "region0" || !reflect.DeepEqual(notes, []string{"region0"}) {
		t.Fatalf("created %q %v, notes %q", r, err, notes)
	}
	if !strings.Contains(f.Commands(), "cxl create-region -t ram -d decoder0.0 -m mem0") {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	if next, err := k.NextRegionName("decoder0.0", kernel.RegionRAM); err != nil || next != "region1" {
		t.Fatalf("next region %q %v", next, err)
	}
	// a note that fails: no create-region
	noErr(t, k.DestroyRegion(ctx, r))
	before := f.Commands()
	req.Note = func(string) error { return errors.New("no room") }
	if _, err := k.CreateRegion(ctx, req); err == nil || err.Error() != "no room" || f.Commands() != before {
		t.Fatalf("a failed note: %v\n%s", err, f.CommandsSince(before))
	}
	// an invalid type
	req.Type, req.Note = "volatile", nil
	if _, err := k.CreateRegion(ctx, req); err == nil {
		t.Fatal("invalid region type accepted")
	}
}

// A region of one memdev takes a root decoder of its host bridge alone: a
// root decoder that interleaves host bridges, or that lists two of them,
// is passed over (cxl create-region of one memdev fails there with EINVAL).
func TestRootDecoderNotInterleaved(t *testing.T) {
	f, k := newNode(t)
	root := "devices/platform/ACPI0017:00/root0/"
	f.Write(root+"decoder0.0/target_list", "12,24")
	f.Write(root+"decoder0.0/interleave_ways", "2")
	if d, err := k.RootDecoder("mem0", kernel.RegionRAM); !errors.Is(err, kernel.ErrNotFound) {
		t.Fatalf("root decoder %q %v, expected none", d, err)
	}
	for attr, v := range map[string]string{"devtype": "cxl_decoder_root", "target_list": "12", "interleave_ways": "1", "cap_ram": "1", "cap_pmem": "0"} {
		f.Write(root+"decoder0.4/"+attr, v)
	}
	f.LinkTo("bus/cxl/devices/decoder0.4", root+"decoder0.4")
	if d, err := k.RootDecoder("mem0", kernel.RegionRAM); err != nil || d != "decoder0.4" {
		t.Fatalf("root decoder %q %v, expected decoder0.4", d, err)
	}
	f.Write(root+"decoder0.0/interleave_ways", "1")
	if d, err := k.RootDecoder("mem0", kernel.RegionRAM); err != nil || d != "decoder0.4" {
		t.Fatalf("root decoder %q %v of one way and two targets, expected decoder0.4", d, err)
	}
}

// Regions get the lowest free name and the lowest free addresses of their
// window; a size is a multiple of 256 MiB.
func TestCreateRegionSizes(t *testing.T) {
	f := fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: serial0, Ram: gib})
	k := f.Kernel()
	r0 := createRegion(t, k, "mem0", kernel.RegionRAM, 256*mib)
	r1 := createRegion(t, k, "mem0", kernel.RegionRAM, 512*mib)
	if !strings.HasSuffix(f.Commands(), "cxl create-region -t ram -d decoder0.0 -m mem0 -s 536870912") {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	for _, tc := range []struct {
		region string
		want   kernel.Range
	}{
		{r0, kernel.Range{Start: window0, End: window0 + 256*mib - 1}},
		{r1, kernel.Range{Start: window0 + 256*mib, End: window0 + 768*mib - 1}},
	} {
		if got, err := k.RegionRange(tc.region); err != nil || got != tc.want {
			t.Errorf("%s: range %v %v, want %v", tc.region, got, err, tc.want)
		}
	}
	_, err := k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: "mem0", Decoder: "decoder0.0", Type: kernel.RegionRAM, Size: 100 * mib})
	wantErr(t, err, nil, "failed to set size")
	if !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("error %v is not EINVAL", err)
	}
	if _, err := k.RegionRange("region7"); err == nil {
		t.Fatal("range of a missing region")
	}
	f.Write("bus/cxl/devices/"+r0+"/size", "bogus")
	if _, err := k.RegionRange(r0); err == nil {
		t.Fatal("a bad region size accepted")
	}
}

// A region is ENABLED with a dax region and daxN.0 bound to kmem after its
// creation, DISABLED after cxl disable-region (its dax devices gone, its
// decoders committed), ENABLED again by EnableRegion.
func TestRegionEnableDisable(t *testing.T) {
	f, k := newNode(t)
	ctx := context.Background()
	wantErr(t, k.EnableRegion(ctx, "region0"), kernel.ErrNotFound, "region0")
	r := createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	info, err := k.Region(r)
	noErr(t, err)
	want := kernel.RegionInfo{Name: "region0", State: kernel.RegionEnabled, Mode: kernel.RegionRAM, Committed: true,
		Range: kernel.Range{Start: window0, End: window0 + 256*mib - 1}, Targets: []string{"decoder4.0"}, Memdevs: []string{"mem0"},
		Serials: []uint64{serial0}, RootDecoder: "decoder0.0", NUMANode: 2, DaxRegion: "dax_region0"}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("region %+v\nwant %+v", info, want)
	}
	if d := k.DaxDriver("dax0.0"); d != kernel.DriverKmem {
		t.Fatalf("dax0.0 bound to %q", d)
	}
	noErr(t, k.DisableRegion(ctx, r))
	if info, _ := k.Region(r); info.State != kernel.RegionDisabled || info.DaxRegion != "" || !info.Committed || info.NUMANode != -1 {
		t.Fatalf("disabled region %+v", info)
	}
	if _, err := k.DaxRegion(r); err == nil {
		t.Fatal("a disabled region has a dax region")
	}
	noErr(t, k.DestroyRegion(ctx, "region9"))
	noErr(t, k.EnableRegion(ctx, r))
	if !strings.HasSuffix(f.Commands(), "cxl enable-region region0") {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	before := f.Commands()
	noErr(t, k.EnableRegion(ctx, r))
	if f.Commands() != before {
		t.Fatalf("an enabled region was enabled again:\n%s", f.CommandsSince(before))
	}
	if info, _ := k.Region(r); info.State != kernel.RegionEnabled || info.DaxRegion != "dax_region0" {
		t.Fatalf("enabled region %+v", info)
	}
	noErr(t, k.DisableRegion(ctx, r))
	noErr(t, k.DestroyRegion(ctx, r))
	if info, err := k.Region(r); !errors.Is(err, kernel.ErrNotFound) || info.State != kernel.RegionAbsent {
		t.Fatalf("destroyed region %+v %v", info, err)
	}
	noErr(t, k.DisableRegion(ctx, r))
	noErr(t, k.DestroyRegion(ctx, r))
}

// Releasing a region offlines its memory, disables and destroys it; a
// region whose memory is online after it is disabled (the kernel could not
// remove it) is never destroyed, its memory would read garbage. A block
// whose state cannot be read counts as online.
func TestDestroyRegion(t *testing.T) {
	f, k := newNode(t)
	ctx := context.Background()
	r := createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	blocks, err := k.RegionBlocks(r)
	if err != nil || !reflect.DeepEqual(blocks, []int{74, 75}) {
		t.Fatalf("blocks %v %v", blocks, err)
	}
	noErr(t, k.OnlineBlocks(blocks, true))
	if n := k.BlocksNUMANode(blocks); n != 2 {
		t.Fatalf("node %d of the blocks", n)
	}
	before := f.Commands()
	noErr(t, k.DestroyRegion(ctx, r))
	if got := f.CommandsSince(before); got != "cxl disable-region region0\ncxl destroy-region region0" {
		t.Fatalf("commands:\n%s", got)
	}
	for _, b := range blocks {
		if st := f.BlockState(b); st != "" {
			t.Fatalf("memory%d %q after the release", b, st)
		}
	}
	noErr(t, k.DestroyRegion(ctx, r))

	// online without kmem: a failed driver switch left the memory online
	r = createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	noErr(t, k.OnlineBlocks(blocks, true))
	f.Remove("bus/dax/devices/dax0.0/driver")
	noErr(t, k.DestroyRegion(ctx, r))
	if !strings.HasSuffix(f.Commands(), "cxl destroy-region region0") {
		t.Fatalf("region not destroyed:\n%s", f.Commands())
	}

	// a block whose state cannot be read after disable-region counts as
	// online
	r = createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	state := f.P("devices/system/memory/memory75/state")
	f.After("cxl disable-region "+r, func() {
		f.SetBlockState(75, kernel.BlockStateOnline)
		noErr(t, os.Remove(state))
		noErr(t, os.Mkdir(state, 0o755))
	})
	before = f.Commands()
	err = k.DestroyRegion(ctx, r)
	wantErr(t, err, kernel.ErrBusyOnline, "memory blocks [75] are online after disabling it")
	if cmds := f.CommandsSince(before); strings.Contains(cmds, "destroy-region") {
		t.Fatalf("destroyed with a block of unknown state:\n%s", cmds)
	}
	noErr(t, os.Remove(state))
	f.SetBlockState(75, kernel.BlockStateOffline)
	noErr(t, k.DestroyRegion(ctx, r))

	// kmem could not remove memory75, and it is online again
	r = createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	f.After("cxl disable-region "+r, func() { f.SetBlockState(75, kernel.BlockStateOnline) })
	err = k.DestroyRegion(ctx, r)
	wantErr(t, err, kernel.ErrBusyOnline, "memory blocks [75] are online after disabling it")
	if info, _ := k.Region(r); info.State != kernel.RegionDisabled {
		t.Fatalf("region %v after the refusal", info.State)
	}

	// memory that cannot be offlined
	f.SetBlockState(75, kernel.BlockStateOffline)
	noErr(t, k.EnableRegion(ctx, r))
	noErr(t, k.OnlineBlocks([]int{74, 75}, true))
	f.FailWrite("devices/system/memory/memory74/state", syscall.EBUSY)
	err = k.DestroyRegion(ctx, r)
	wantErr(t, err, unix.EBUSY, "region0: cannot offline memory (in use?): memory74: offline")
}

// A locked region is never destroyed (ErrForeign).
func TestLockedRegion(t *testing.T) {
	f, k := newNode(t)
	ctx := context.Background()
	r := createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	f.Write("bus/cxl/devices/"+r+"/locked", "1")
	if info, _ := k.Region(r); !info.Locked {
		t.Fatal("region not locked")
	}
	before := f.Commands()
	wantErr(t, k.DestroyRegion(ctx, r), nil, "region0 is locked")
	_, err := k.DestroyRegionInOrder(ctx, "mem0", r)
	wantErr(t, err, nil, "region0 is locked")
	if f.Commands() != before {
		t.Fatalf("commands on a locked region:\n%s", f.CommandsSince(before))
	}
}

// A region whose memdev was removed is hollow; cxl destroy-region refuses
// it, the delete_region of its root decoder deletes it unless memory of its
// range is online.
func TestDeleteHollowRegion(t *testing.T) {
	f, k := newNode(t)
	r := createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	noErr(t, k.OnlineBlocks([]int{75}, true))
	wantErr(t, k.DeleteHollowRegion(r), nil, "region0 is not hollow: it is committed or has a target decoder")
	f.Unplug(serial0)
	if !k.HollowRegion(r) || k.HollowRegion("region9") {
		t.Fatalf("hollow: %s %v, region9 %v", r, k.HollowRegion(r), k.HollowRegion("region9"))
	}
	// a hollow region has the node of its memory that is online
	if info, _ := k.Region(r); info.State != kernel.RegionHollow || info.NUMANode != 2 || info.Targets != nil || info.DaxRegion != "" {
		t.Fatalf("hollow region %+v", info)
	}
	if d, err := k.RegionRootDecoder(r); err != nil || d != "decoder0.0" {
		t.Fatalf("root decoder %q %v", d, err)
	}
	wantErr(t, k.DeleteHollowRegion(r), kernel.ErrBusyOnline, "memory blocks [75] of its address range are not offline")
	f.SetBlockState(75, kernel.BlockStateOffline)
	noErr(t, k.DeleteHollowRegion(r))
	if !strings.HasSuffix(f.Commands(), "delete_region decoder0.0 region0") || k.HollowRegion(r) {
		t.Fatalf("not deleted:\n%s", f.Commands())
	}
	// gone: not an error
	noErr(t, k.DeleteHollowRegion(r))

	// a region without an address range yet (resource all ones, size 0)
	h := f.HollowRegion("decoder0.1", 256*mib)
	f.Write("bus/cxl/devices/"+h+"/resource", "0xffffffffffffffff")
	f.Write("bus/cxl/devices/"+h+"/size", "0x0")
	noErr(t, k.DeleteHollowRegion(h))
	if f.Exists("bus/cxl/devices/"+h) || !strings.HasSuffix(f.Commands(), "delete_region decoder0.1 "+h) {
		t.Fatalf("%s not deleted:\n%s", h, f.Commands())
	}
	// a hollow region that is not under a root decoder is not deleted, and
	// no delete_region is written
	f.Write("bus/cxl/devices/region9/commit", "0")
	f.Write("bus/cxl/devices/region9/target0", "")
	before := f.Commands()
	wantErr(t, k.DeleteHollowRegion("region9"), nil, "region9: its parent device devices is not a root decoder")
	if f.Commands() != before || !f.Exists("bus/cxl/devices/region9") {
		t.Fatalf("delete_region written:\n%s", f.CommandsSince(before))
	}
	// committed: not hollow
	h = f.HollowRegion("decoder0.1", 256*mib)
	f.Write("bus/cxl/devices/"+h+"/commit", "1")
	if k.HollowRegion(h) {
		t.Fatal("a committed region is hollow")
	}
}

// A region that does not start at a block boundary has only the blocks
// that lie wholly in it.
func TestRegionBlocksMisaligned(t *testing.T) {
	f, k := newNode(t)
	r := createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	f.Write("bus/cxl/devices/"+r+"/resource", "0x24c000000")
	f.Remove("devices/system/memory/memory75")
	if blocks, err := k.RegionBlocks(r); err != nil || !reflect.DeepEqual(blocks, []int{74}) {
		t.Fatalf("blocks %v %v", blocks, err)
	}
	ctx := ctxTimeout(t, time.Second)
	noErr(t, k.WaitRegionBlocks(ctx, r))
	// no whole block: nothing to wait for
	f.Write("bus/cxl/devices/"+r+"/size", "0x8000000")
	noErr(t, k.WaitRegionBlocks(ctx, r))
	// a block that does not come: the wait ends with the context
	f.Write("bus/cxl/devices/"+r+"/resource", "0x250000000")
	f.Write("bus/cxl/devices/"+r+"/size", "0x10000000")
	err := k.WaitRegionBlocks(ctxTimeout(t, 30*time.Millisecond), r)
	wantErr(t, err, context.DeadlineExceeded, "region0: 1 of 2 memory blocks appeared")
}
