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
	"reflect"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// block_size_bytes is hex without 0x: "8000000" is 128 MiB.
func TestBlockSize(t *testing.T) {
	f := fake.New(t)
	k := f.Kernel()
	for _, tc := range []struct {
		val  string
		want uint64
	}{
		{"8000000", 134217728},
		{"0x8000000", 134217728},
		{"80000000", 2 << 30},
	} {
		f.Write("devices/system/memory/block_size_bytes", tc.val)
		if got, err := k.BlockSize(); err != nil || got != tc.want {
			t.Fatalf("block size %q: %d %v, expected %d", tc.val, got, err, tc.want)
		}
	}
	for _, bad := range []string{"0", "bogus", ""} {
		f.Write("devices/system/memory/block_size_bytes", bad)
		if _, err := k.BlockSize(); err == nil {
			t.Fatalf("block size %q accepted", bad)
		}
	}
	f.Remove("devices/system/memory/block_size_bytes")
	if _, err := k.BlockSize(); err == nil {
		t.Fatal("a missing block size accepted")
	}
}

// The ranges of memory and the blocks that lie wholly in them.
func TestRange(t *testing.T) {
	r := kernel.Range{Start: 0x250000000, End: 0x257ffffff}
	if r.Size() != bs || !r.Aligned(bs) || r.String() != "0x250000000-0x257ffffff" {
		t.Fatalf("range %v size %d aligned %v", r, r.Size(), r.Aligned(bs))
	}
	if got := r.Blocks(bs); !reflect.DeepEqual(got, []int{74}) {
		t.Fatalf("blocks %v", got)
	}
	// a range that starts 2 MiB into a block holds no whole block
	r = kernel.Range{Start: 0x250200000, End: 0x2581fffff}
	if r.Aligned(bs) || r.Aligned(0) || len(r.Blocks(bs)) != 0 || !r.Aligned(fake.DaxAlign) {
		t.Fatalf("misaligned range %v: blocks %v", r, r.Blocks(bs))
	}
	r = kernel.Range{Start: 0x250200000, End: 0x25fffffff}
	if got := r.Blocks(bs); !reflect.DeepEqual(got, []int{75}) {
		t.Fatalf("blocks %v", got)
	}
	if got := r.Blocks(0); got != nil {
		t.Fatalf("blocks of size 0 %v", got)
	}
}

// DRAM nodes have memory from boot. The node of a window gets memory when
// its blocks go online. Onlining movable refuses a block online in another
// zone (auto-onlined into ZONE_NORMAL, never removable).
func TestOnlineBlocks(t *testing.T) {
	f, k := newNode(t)
	nodes := k.NUMANodes()
	if len(nodes) != 6 || !nodes[0].HasMemory || nodes[0].MemTotal != 8<<30 || nodes[2].HasMemory {
		t.Fatalf("nodes %+v", nodes)
	}
	// has_memory is a list of ranges and numbers: an item that is no
	// number is left out
	f.Write("devices/system/node/has_memory", "0-1, 3,x")
	if nodes := k.NUMANodes(); !nodes[1].HasMemory || nodes[2].HasMemory || !nodes[3].HasMemory {
		t.Fatalf("nodes of has_memory 0-1, 3,x: %+v", nodes)
	}
	f.Write("devices/system/node/has_memory", "0-1")
	createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	if n, ok := observe(t, k).NUMANode(2); !ok || n.HasMemory || n.MemTotal != 0 {
		t.Fatalf("node 2 before online %+v", n)
	}
	noErr(t, k.OnlineBlocks([]int{74, 75}, true))
	for _, b := range []int{74, 75} {
		if st := f.BlockState(b); st != kernel.BlockStateOnline {
			t.Fatalf("memory%d %q", b, st)
		}
		if mb := k.MemoryBlock(b); mb.State != kernel.BlockOnline || mb.Zones != "Movable" || mb.NUMANode != 2 {
			t.Fatalf("memory block %+v", mb)
		}
	}
	if n, _ := observe(t, k).NUMANode(2); !n.HasMemory || n.MemTotal != 256*mib {
		t.Fatalf("node 2 after online %+v", n)
	}
	// idempotent
	noErr(t, k.OnlineBlocks([]int{74, 75}, true))
	f.Write("devices/system/memory/memory75/valid_zones", "Normal")
	wantErr(t, k.OnlineBlocks([]int{74, 75}, true), nil, "memory75 is already online in zone Normal, not Movable (auto-onlined?): offline it first")
	noErr(t, k.OnlineBlocks([]int{75}, false))
	if mb := k.MemoryBlock(99); mb.State != kernel.BlockAbsent || mb.NUMANode != -1 {
		t.Fatalf("absent block %+v", mb)
	}
	if n := k.BlocksNUMANode([]int{99, 75}); n != 2 {
		t.Fatalf("node %d", n)
	}
	if n := k.BlocksNUMANode([]int{99}); n != -1 {
		t.Fatalf("node %d of no block", n)
	}
	// a block that cannot be offlined: EBUSY
	f.FailWrite("devices/system/memory/memory74/state", syscall.EBUSY)
	wantErr(t, k.OfflineBlocks([]int{74, 75}), unix.EBUSY, "memory74: offline")
}

// A udev rule onlines the blocks that kmem adds, also after they were
// offlined: StayedOffline returns the block that came online, -1 when the
// blocks stayed offline for the settle time, and the error of a context
// that ends.
func TestStayedOffline(t *testing.T) {
	f, k := newNode(t)
	f.AutoOnline = 2
	createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	ctx := context.Background()
	blocks := []int{74, 75}
	for range 2 {
		noErr(t, k.OfflineBlocks(blocks))
		if b, err := k.StayedOffline(ctx, blocks, time.Second); err != nil || b < 0 {
			t.Fatalf("block %d %v, expected one online again", b, err)
		}
	}
	noErr(t, k.OfflineBlocks(blocks))
	if b, err := k.StayedOffline(ctx, blocks, 50*time.Millisecond); err != nil || b != -1 {
		t.Fatalf("block %d %v, expected none", b, err)
	}
	_, err := k.StayedOffline(ctxTimeout(t, 10*time.Millisecond), blocks, time.Second)
	wantErr(t, err, context.DeadlineExceeded, "")
}
