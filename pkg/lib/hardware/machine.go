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
	"io/fs"
	"slices"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// ID identifies a hardware element within its kind: a CPU, a package, a die, a
// NUMA node, a cache. It is an alias for int, so a []ID and a []int are the
// same type.
type ID = int

// Machine is the CPU and memory topology of a machine, as discovered once by
// [Discover]. It is immutable and safe for concurrent use.
//
// discover.go builds one; doc.go says what the handles it hands out promise.
type Machine struct {
	fsys fs.FS

	cpus   map[ID]*CPU
	cpuIDs []ID

	nodes   map[ID]*MemoryNode
	nodeIDs []ID

	caches map[CacheID]*Cache

	zones  map[Level][]*Zone
	levels []Level

	possible *libcpu.CpuMask
	present  *libcpu.CpuMask
	online   *libcpu.CpuMask
	isolated *libcpu.CpuMask
	kinds    map[CoreKind]*libcpu.CpuMask

	index *TopologyIndex
}

// Zones returns every zone at the given level, ordered by id, or nothing if the
// machine has no zones at that level.
//
// A level with a single zone is still a level: a machine with one package has
// one package zone. Nothing is left out for being uninformative, which is a
// judgement callers make for themselves; [Machine.SameZones] and the length of
// this are what they make it from.
func (m *Machine) Zones(level Level) []*Zone {
	return m.zones[level]
}

// There is deliberately no Zone(level, id) lookup: the kernel numbers dies,
// clusters and cores within their package, so an id alone does not name one.
// [TopologyIndex] addresses those by their full coordinates.

// SameZones reports whether two levels cut the machine the same way, i.e.
// whether their zones hold exactly the same sets of CPUs.
//
// It is how to ask the questions the policies ask today by hand: whether a
// machine's clusters are just its cores, or whether its dies are just its
// packages. A level with no zones is the same as no other level.
func (m *Machine) SameZones(a, b Level) bool {
	za, zb := m.zones[a], m.zones[b]
	if len(za) == 0 || len(zb) == 0 || len(za) != len(zb) {
		return false
	}

	for _, x := range za {
		found := false
		for _, y := range zb {
			if x.cpus.Equals(y.cpus) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

// Levels returns the levels which the machine actually has zones at, outermost
// first.
func (m *Machine) Levels() []Level {
	return m.levels
}

// CPU returns a handle for the CPU with the given id. The handle is never nil;
// if the CPU is not present it reports Valid() == false.
func (m *Machine) CPU(id ID) *CPU {
	if c, ok := m.cpus[id]; ok {
		return c
	}
	return invalidCPU
}

// CPUIDs returns the ids of all present CPUs, in increasing order.
func (m *Machine) CPUIDs() []ID {
	return m.cpuIDs
}

// PresentCPUs returns the CPUs the machine has, whether they are online or not.
func (m *Machine) PresentCPUs() *libcpu.CpuMask {
	return m.present
}

// PossibleCPUs returns the CPUs the kernel has reserved room for, a superset of
// [Machine.PresentCPUs] on a machine which supports CPU hotplug.
func (m *Machine) PossibleCPUs() *libcpu.CpuMask {
	return m.possible
}

// OnlineCPUs returns the CPUs which are online.
func (m *Machine) OnlineCPUs() *libcpu.CpuMask {
	return m.online
}

// OfflineCPUs returns the CPUs which are present but not online.
func (m *Machine) OfflineCPUs() *libcpu.CpuMask {
	return sealed(m.present.Difference(m.online))
}

// IsolatedCPUs returns the CPUs the kernel was told to isolate.
func (m *Machine) IsolatedCPUs() *libcpu.CpuMask {
	return m.isolated
}

// CoreKinds returns the core kinds the machine has, or a single
// [PerformanceCore] on a machine whose cores are all alike.
func (m *Machine) CoreKinds() []CoreKind {
	kinds := make([]CoreKind, 0, len(m.kinds))
	for kind := range m.kinds {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// CoreKindCPUs returns the CPUs of the given core kind.
func (m *Machine) CoreKindCPUs(kind CoreKind) *libcpu.CpuMask {
	if cpus, ok := m.kinds[kind]; ok {
		return cpus
	}
	return emptyCPUs
}

// MemoryNode returns a handle for the NUMA node with the given id. The handle
// is never nil; if there is no such node it reports Valid() == false.
func (m *Machine) MemoryNode(id ID) *MemoryNode {
	if n, ok := m.nodes[id]; ok {
		return n
	}
	return invalidMemoryNode
}

// MemoryNodes returns all NUMA nodes, ordered by id. A machine built without
// NUMA support has a single node, holding every CPU and all of the memory.
func (m *Machine) MemoryNodes() []*MemoryNode {
	nodes := make([]*MemoryNode, 0, len(m.nodeIDs))
	for _, id := range m.nodeIDs {
		nodes = append(nodes, m.nodes[id])
	}
	return nodes
}

// MemoryNodeIDs returns the ids of all NUMA nodes, in increasing order.
func (m *Machine) MemoryNodeIDs() []ID {
	return m.nodeIDs
}

// Caches returns every cache at the given level, ordered by id, or nothing if
// the machine has no caches at that level. Level 0 returns all caches.
func (m *Machine) Caches(level int) []*Cache {
	caches := make([]*Cache, 0, len(m.caches))
	for _, c := range m.caches {
		if level == 0 || c.level == level {
			caches = append(caches, c)
		}
	}
	slices.SortFunc(caches, compareCaches)
	return caches
}

// CacheLevels returns the cache levels the machine has, lowest first.
func (m *Machine) CacheLevels() []int {
	var levels []int
	for _, c := range m.caches {
		if !slices.Contains(levels, c.level) {
			levels = append(levels, c.level)
		}
	}
	slices.Sort(levels)
	return levels
}

// FS returns the filesystem this machine was discovered from, rooted at the host
// root. A caller which wants to read something discovery does not, or write to
// an attribute of hardware it found here, should go through this rather than the
// real filesystem: it is the only thing which knows where the tree actually is.
// Writing needs it to be a [WriterFS], which the default filesystem is.
func (m *Machine) FS() fs.FS {
	return m.fsys
}

//
// Shared zero values
//

// emptyCPUs is the set returned for a coordinate the machine does not have. It
// is sealed, so handing the same one out everywhere is safe.
var emptyCPUs = func() *libcpu.CpuMask {
	cpus := libcpu.NewCpuMask()
	cpus.Seal()
	return cpus
}()

// sealed seals a set before it is handed out, so that nothing can modify a set
// this package has published.
func sealed(cpus *libcpu.CpuMask) *libcpu.CpuMask {
	cpus.Seal()
	return cpus
}
