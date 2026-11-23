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

package kernel

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// MemoryBlock: a memory block memoryN (/sys/devices/system/memory).
//
// Identity: the index N = physical address / block size (block_size_bytes,
// hex without 0x: "8000000" is 128 MiB, up to 2 GiB on servers).
// States (BlockState): ABSENT (kmem has not added it, or removed it),
// OFFLINE, ONLINE (any state word but "offline"; a state that cannot be read
// is ONLINE: the checks fail closed). Transitions: BlockTransitions.
// Facts: valid_zones (online_movable over a block online in another zone is
// refused: memory onlined into ZONE_NORMAL can never be removed), the NUMA
// node of its memory (memoryN/nodeM).
// Invariants: kmem adds only the blocks that lie wholly in the memory of a
// dax device; a region or a dax device has online memory iff one of those
// blocks is online.
// Waits: the blocks of a kmem dax device appear asynchronously after the
// driver link (WaitDaxBlocks); a udev rule may online hot-added blocks again
// after they were offlined (StayedOffline).

// Block state words of memoryN/state.
const (
	BlockStateOffline       = "offline"
	BlockStateOnline        = "online"
	BlockStateOnlineMovable = "online_movable"
	BlockStateOnlineKernel  = "online_kernel"
)

// BlockState is the state of a memory block.
type BlockState int

const (
	// BlockAbsent: the block does not exist.
	BlockAbsent BlockState = iota
	// BlockOffline: the block is offline.
	BlockOffline
	// BlockOnline: the block is online (or its state cannot be read).
	BlockOnline
)

var blockStateNames = []string{"ABSENT", "OFFLINE", "ONLINE"}

func (s BlockState) String() string { return stateText(blockStateNames, int(s)) }

// MarshalText returns the name of the state.
func (s BlockState) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// BlockTransitions are the changes of the state of a memory block.
var BlockTransitions = fsm.Table[BlockState]{
	Object:   "MemoryBlock",
	Field:    "State",
	Observed: true,
	States:   fsm.List(BlockAbsent, BlockOffline, BlockOnline),
	Rows: []fsm.Transition[BlockState]{
		{From: fsm.List(BlockAbsent), To: BlockOffline, When: "kmem bound to the dax device of its memory (BindDax, or the kernel's default binding of a new ram region)",
			Guard: "auto_online_blocks offline"},
		{From: fsm.List(BlockAbsent), To: BlockOnline, When: "kmem bound, with auto_online_blocks or a udev rule onlining hot-added memory"},
		{From: fsm.List(BlockOffline), To: BlockOnline, When: "OnlineBlocks, or a udev rule (StayedOffline tells it within its settle time)",
			Guard: "online_movable: not online in another zone"},
		{From: fsm.List(BlockOnline), To: BlockOffline, When: "OfflineBlocks",
			Guard: "its pages can be moved (EBUSY otherwise)"},
		{From: fsm.List(BlockOffline), To: BlockAbsent, When: "kmem unbound (UnbindDax, DeleteDaxDevice), DisableRegion, hot-remove of its memdev"},
		{From: fsm.List(BlockOnline), To: BlockAbsent, When: "DestroyRegion (offline, then disable), observed across it; a numa slice release (OfflineBlocks, then UnbindDax)",
			Guard: "its pages can be moved"},
	},
}

// MemoryBlock is the observation of a memory block.
type MemoryBlock struct {
	Index int        `json:"index"`
	State BlockState `json:"state"`
	// Zones is its valid_zones.
	Zones string `json:"zones,omitempty"`
	// NUMANode is the NUMA node of its memory, -1 if not known.
	NUMANode int `json:"numaNode"`
}

// Range is a range of physical addresses from Start to End, both included,
// as the kernel shows them.
type Range struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

// compareStart orders ranges by their start.
func compareStart(a, b Range) int {
	return cmp.Compare(a.Start, b.Start)
}

// Size returns the number of bytes in the range.
func (r Range) Size() uint64 {
	return r.End - r.Start + 1
}

// Aligned tells whether the range starts and ends at multiples of align.
func (r Range) Aligned(align uint64) bool {
	return align > 0 && r.Start%align == 0 && r.Size()%align == 0
}

// String returns the range as "0x<start>-0x<end>", the format of the
// mapping attribute of dax devices.
func (r Range) String() string {
	return fmt.Sprintf("0x%x-0x%x", r.Start, r.End)
}

// Blocks returns the numbers of the memory blocks of bs bytes that lie
// wholly in the range.
func (r Range) Blocks(bs uint64) []int {
	if bs == 0 {
		return nil
	}
	var out []int
	for b := (r.Start + bs - 1) / bs; (b+1)*bs-1 <= r.End; b++ {
		out = append(out, int(b))
	}
	return out
}

// BlockSize returns the size of the memory blocks of the system in bytes
// (devices/system/memory/block_size_bytes, hex without 0x).
func (k *Kernel) BlockSize() (uint64, error) {
	path := filepath.Join(k.sysRoot(), "devices", "system", "memory", "block_size_bytes")
	s, err := readTrim(path)
	if err != nil {
		return 0, err
	}
	bs, err := ParseHex(s)
	if err != nil || bs == 0 {
		return 0, fmt.Errorf("cannot parse the memory block size %q in %s", s, path)
	}
	return bs, nil
}

// blockExists tells whether memory block n exists.
func (k *Kernel) blockExists(n int) bool {
	_, err := os.Stat(k.block(n, "state"))
	return err == nil
}

// onlineBlocks returns the memory blocks of blocks that are online
// (BlockOnline: a state that cannot be read is online).
func (k *Kernel) onlineBlocks(blocks []int) []int {
	var online []int
	for _, b := range blocks {
		if k.blockState(b) == BlockOnline {
			online = append(online, b)
		}
	}
	return online
}

// blockState returns the state of memory block n.
func (k *Kernel) blockState(n int) BlockState {
	st, err := readTrim(k.block(n, "state"))
	switch {
	case os.IsNotExist(err):
		return BlockAbsent
	case err != nil:
		return BlockOnline
	case st == BlockStateOffline:
		return BlockOffline
	}
	return BlockOnline
}

// MemoryBlock returns the observation of memory block n.
func (k *Kernel) MemoryBlock(n int) MemoryBlock {
	b := MemoryBlock{Index: n, State: k.blockState(n), NUMANode: -1}
	if b.State == BlockAbsent {
		return b
	}
	b.Zones, _ = readTrim(k.block(n, "valid_zones"))
	b.NUMANode = k.BlocksNUMANode([]int{n})
	return b
}

// RegionBlocks returns the memory blocks of a region that exist: those that
// lie wholly in its range.
func (k *Kernel) RegionBlocks(region string) ([]int, error) {
	blocks, _, err := k.regionBlocks(region)
	return blocks, err
}

// regionBlocks returns the memory blocks of a region that exist, and how
// many blocks lie wholly in the region (kmem adds only those).
func (k *Kernel) regionBlocks(region string) ([]int, int, error) {
	r, err := k.RegionRange(region)
	if err != nil {
		return nil, 0, err
	}
	bs, err := k.BlockSize()
	if err != nil {
		return nil, 0, err
	}
	whole := r.Blocks(bs)
	var out []int
	for _, b := range whole {
		if k.blockExists(b) {
			out = append(out, b)
		}
	}
	return out, len(whole), nil
}

// WaitRegionBlocks waits until every memory block wholly in the range of a
// region exists: the kernel creates the driver link of a dax device before
// it probes the driver, so a dax device that the kernel binds to kmem by
// itself (the default for a volatile region) looks bound while
// dev_dax_kmem_probe is still adding its memory. A region without a whole
// block gets no memory blocks.
func (k *Kernel) WaitRegionBlocks(ctx context.Context, region string) error {
	var blocks []int
	var want int
	err := k.poll(ctx, func() (bool, error) {
		var err error
		blocks, want, err = k.regionBlocks(region)
		return len(blocks) == want, err
	})
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%s: %d of %d memory blocks appeared: %w", region, len(blocks), want, err)
	}
	return err
}

// BlocksNUMANode returns the NUMA node of memory blocks (memoryN/nodeM of the
// first block with one), -1 if not found.
func (k *Kernel) BlocksNUMANode(blocks []int) int {
	for _, b := range blocks {
		m, _ := filepath.Glob(k.block(b, "node*"))
		for _, path := range m {
			if n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "node")); err == nil {
				return n
			}
		}
	}
	return -1
}

// setBlocksState onlines (BlockStateOnline, BlockStateOnlineMovable,
// BlockStateOnlineKernel) or offlines (BlockStateOffline) memory blocks,
// skipping blocks that are already there, stopping at the first block that
// fails. A block that is online in another zone than Movable is an error
// with BlockStateOnlineMovable.
func (k *Kernel) setBlocksState(blocks []int, state string) error {
	want := strings.TrimSuffix(strings.TrimSuffix(state, "_movable"), "_kernel")
	for _, b := range blocks {
		cur, err := readTrim(k.block(b, "state"))
		if err != nil {
			return err
		}
		if cur == want {
			// online, but maybe in a zone that cannot be offlined reliably
			if state == BlockStateOnlineMovable {
				if z, err := readTrim(k.block(b, "valid_zones")); err == nil && z != "" && !strings.HasPrefix(z, "Movable") {
					return fmt.Errorf("memory%d is already online in zone %s, not Movable (auto-onlined?): offline it first", b, z)
				}
			}
			continue
		}
		if err := k.writeFile(k.block(b, "state"), state); err != nil {
			return fmt.Errorf("memory%d: %s: %w", b, state, err)
		}
		k.logf("memory%d: %s", b, state)
	}
	return nil
}

// OnlineBlocks onlines memory blocks, into ZONE_MOVABLE with movable,
// skipping blocks that are online already. A block that is online in
// another zone than Movable is an error with movable.
func (k *Kernel) OnlineBlocks(blocks []int, movable bool) error {
	state := BlockStateOnline
	if movable {
		state = BlockStateOnlineMovable
	}
	return k.setBlocksState(blocks, state)
}

// OfflineBlocks offlines memory blocks, skipping blocks that are offline
// already. A block whose memory the kernel cannot move fails with EBUSY.
func (k *Kernel) OfflineBlocks(blocks []int) error {
	return k.setBlocksState(blocks, BlockStateOffline)
}

// StayedOffline waits until memory blocks have stayed offline for settle
// and returns -1, or returns a block that came online before: a udev rule
// (for instance 40-redhat.rules of RHEL and its derivatives) may online
// hot-added blocks after they were offlined, whatever auto_online_blocks
// says.
func (k *Kernel) StayedOffline(ctx context.Context, blocks []int, settle time.Duration) (int, error) {
	stable := time.Now().Add(settle)
	for time.Now().Before(stable) {
		if err := sleep(ctx, k.pollInterval()); err != nil {
			return -1, err
		}
		if online := k.onlineBlocks(blocks); len(online) > 0 {
			return online[0], nil
		}
	}
	return -1, nil
}

// NUMANode: a NUMA node nodeN (/sys/devices/system/node). The NUMA node of
// a CFMWS window is fixed at boot; its nodeN directory exists while it has
// memory blocks. States: has memory or not (has_memory); MemTotal is the
// sum of its online memory.

// NUMANode is the observation of a NUMA node.
type NUMANode struct {
	ID int `json:"id"`
	// HasMemory: the node is in has_memory.
	HasMemory bool `json:"hasMemory"`
	// MemTotal is its online memory in bytes (meminfo MemTotal).
	MemTotal uint64 `json:"memTotal"`
}

// NUMANodes returns the NUMA nodes of the system in the order of their
// ids.
func (k *Kernel) NUMANodes() []NUMANode {
	dir := filepath.Join(k.sysRoot(), "devices", "system", "node")
	m, _ := filepath.Glob(filepath.Join(dir, "node[0-9]*"))
	sortNat(m)
	hasMemory := map[int]bool{}
	if s, err := readTrim(filepath.Join(dir, "has_memory")); err == nil {
		for _, id := range parseList(s) {
			hasMemory[id] = true
		}
	}
	var out []NUMANode
	for _, p := range m {
		id, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(p), "node"))
		if err != nil {
			continue
		}
		n := NUMANode{ID: id, HasMemory: hasMemory[id]}
		data, err := os.ReadFile(filepath.Join(p, "meminfo"))
		if err == nil {
			err = fmt.Errorf("%s: no MemTotal in its meminfo", filepath.Base(p))
			for _, line := range strings.Split(string(data), "\n") {
				var nid int
				var kb uint64
				if _, serr := fmt.Sscanf(line, "Node %d MemTotal: %d kB", &nid, &kb); serr == nil {
					n.MemTotal, err = kb<<10, nil
					break
				}
			}
		}
		k.factErr(err)
		out = append(out, n)
	}
	return out
}

// parseList returns the numbers of a kernel list ("0-2,5").
func parseList(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out
}
