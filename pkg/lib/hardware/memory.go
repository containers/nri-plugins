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

package hardware

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// MemoryNode is a NUMA node: some memory, and the CPUs which are closest to
// it. A node may have no memory, and it may have no CPUs; a machine with
// high-bandwidth or persistent memory typically has nodes of the latter kind.
//
// It is a handle into the [Machine] which produced it: never nil, and safe to
// use even when it refers to no node, in which case it reports Valid() ==
// false.
type MemoryNode struct {
	m     *Machine
	dir   string // where it was read from, "" when the kernel has no NUMA
	valid bool

	id       ID
	kind     MemoryKind
	cpus     *libcpu.CpuMask
	capacity int64
	normal   bool
	distance []int

	meminfo string // where Usage() reads from
	zones   [numLevels]*Zone
}

// invalidMemoryNode is what a lookup for a node the machine does not have
// returns. Its methods answer with zero values rather than panicking.
var invalidMemoryNode = &MemoryNode{id: unknownID, kind: MemoryKindUnknown}

// Valid reports whether this handle refers to a node the machine has.
func (n *MemoryNode) Valid() bool {
	return n.valid
}

// ID returns the id of this node.
func (n *MemoryNode) ID() ID {
	return n.id
}

// Kind returns what sort of memory this node has.
func (n *MemoryNode) Kind() MemoryKind {
	return n.kind
}

// CPUs returns the CPUs closest to this node's memory, which is empty for a
// node holding only memory. The set is sealed.
func (n *MemoryNode) CPUs() *libcpu.CpuMask {
	if n.cpus == nil {
		return emptyCPUs
	}
	return n.cpus
}

// Capacity returns how much memory this node has, in bytes, as read once
// during discovery. It is 0 for a node with no memory of its own.
func (n *MemoryNode) Capacity() int64 {
	return n.capacity
}

// HasMemory reports whether this node has any memory, i.e. whether
// [MemoryNode.Capacity] is above zero.
func (n *MemoryNode) HasMemory() bool {
	return n.capacity > 0
}

// HasNormalMemory reports whether some of this node's memory is in a zone the
// kernel will satisfy ordinary allocations from.
func (n *MemoryNode) HasNormalMemory() bool {
	return n.normal
}

// Usage reads how much of this node's memory is in use now. Unlike everything
// else here it goes to the machine on every call, because the answer changes;
// [MemoryNode.Capacity] does not and is not re-read.
func (n *MemoryNode) Usage() (MemInfo, error) {
	if !n.valid || n.m == nil {
		return MemInfo{}, fmt.Errorf("no such NUMA node")
	}
	return readMemInfo(n.m.fsys, n.meminfo, n.id)
}

// Distances returns the cost of reaching every NUMA node from this one, indexed
// by node id, as the kernel reports it.
func (n *MemoryNode) Distances() []int {
	return n.distance
}

// Distance returns the cost of reaching node to from this one, or -1 if there
// is no such node.
func (n *MemoryNode) Distance(to ID) int {
	if to < 0 || to >= len(n.distance) {
		return unknownID
	}
	return n.distance[to]
}

// Zone returns the zone at the given level which contains this node's CPUs, or
// an invalid zone if it has none or spans more than one.
func (n *MemoryNode) Zone(level Level) *Zone {
	if level < 0 || int(level) >= numLevels || n.zones[level] == nil {
		return invalidZone
	}
	return n.zones[level]
}

// PackageID returns the id of the package this node belongs to, or -1 if it has
// no CPUs. It is shorthand for n.Zone(LevelPackage).ID().
func (n *MemoryNode) PackageID() ID {
	return n.Zone(LevelPackage).ID()
}

// DieID returns the id of the die this node belongs to, or -1 if it has no
// CPUs.
func (n *MemoryNode) DieID() ID {
	return n.Zone(LevelDie).ID()
}

// String returns "node#<id>".
func (n *MemoryNode) String() string {
	if !n.valid {
		return "node#?"
	}
	return "node#" + strconv.Itoa(n.id)
}

// MemoryKind is the sort of memory a [MemoryNode] holds.
//
// The kernel does not report this, so it is inferred: a node with CPUs of its
// own holds ordinary DRAM, and one without is told apart by how much memory it
// has relative to the DRAM nodes. That heuristic can be wrong, which is why
// [MemoryKindUnknown] exists rather than a guess being forced.
type MemoryKind int

const (
	// MemoryKindDRAM is ordinary system memory.
	MemoryKindDRAM MemoryKind = iota
	// MemoryKindPMEM is persistent memory: larger and slower than DRAM.
	MemoryKindPMEM
	// MemoryKindHBM is high-bandwidth memory: smaller and faster than DRAM.
	MemoryKindHBM
	// MemoryKindUnknown is reported when the kind could not be determined.
	MemoryKindUnknown
)

// String returns "DRAM", "PMEM", "HBM" or "unknown".
func (k MemoryKind) String() string {
	switch k {
	case MemoryKindDRAM:
		return "DRAM"
	case MemoryKindPMEM:
		return "PMEM"
	case MemoryKindHBM:
		return "HBM"
	}
	return "unknown"
}

// ParseMemoryKind returns the kind named by s, or an error.
func ParseMemoryKind(s string) (MemoryKind, error) {
	switch strings.ToUpper(s) {
	case "DRAM":
		return MemoryKindDRAM, nil
	case "PMEM":
		return MemoryKindPMEM, nil
	case "HBM":
		return MemoryKindHBM, nil
	case "UNKNOWN":
		return MemoryKindUnknown, nil
	}
	return MemoryKindUnknown, fmt.Errorf("unknown memory kind %q", s)
}

// readMemInfo reads a meminfo file, either a NUMA node's or /proc/meminfo.
//
// A node's is "Node 0 MemTotal:  32768 kB" and /proc's is "MemTotal: 32768 kB",
// so the two differ only in a two-field prefix. Everything is reported in kB and
// returned in bytes.
func readMemInfo(fsys fs.FS, name string, id ID) (MemInfo, error) {
	blob, err := readFile(fsys, name)
	if err != nil {
		return MemInfo{}, err
	}

	info := MemInfo{}
	for _, line := range strings.Split(blob, "\n") {
		fields := strings.Fields(line)
		// drop the "Node <id>" prefix a per-node meminfo has
		if len(fields) > 2 && fields[0] == "Node" {
			fields = fields[2:]
		}
		if len(fields) < 2 {
			continue
		}

		dst := (*int64)(nil)
		switch fields[0] {
		case "MemTotal:":
			dst = &info.Total
		case "MemFree:":
			dst = &info.Free
		default:
			continue
		}

		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return MemInfo{}, fmt.Errorf("%s: bad %s %q: %w",
				name, fields[0], fields[1], err)
		}
		if len(fields) > 2 && fields[2] == "kB" {
			value *= 1024
		}
		*dst = value
	}

	// Some kernel and hardware combinations have been seen reporting more free
	// than total memory. Callers compute usage from the difference and go badly
	// wrong on a negative, so refuse it here where it can still be explained.
	if info.Free > info.Total {
		return MemInfo{}, fmt.Errorf(
			"%s: node #%d reports more free (%d) than total (%d) memory; "+
				"this is a kernel bug, try a newer kernel",
			name, id, info.Free, info.Total)
	}

	info.Used = info.Total - info.Free

	return info, nil
}

// MemInfo is how much memory is present and how much of it is in use, in bytes.
type MemInfo struct {
	// Total is how much memory there is.
	Total int64
	// Free is how much of it is unused.
	Free int64
	// Used is Total less Free.
	Used int64
}
