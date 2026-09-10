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

package topologyaware

import (
	"slices"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"github.com/containers/nri-plugins/pkg/lib/hardware"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// toCpuSet and toCpuMask convert between the set the hardware package speaks and
// the one this policy is written in. They are the seam left by moving the policy
// onto hardware without rewriting its pool arithmetic.
func toCpuSet(cpus libcpu.CPUSet) cpuset.CPUSet {
	return cpuset.New(cpus.List()...)
}

func toCpuMask(cpus cpuset.CPUSet) *libcpu.CpuMask {
	return libcpu.NewCpuMask(cpus.List()...)
}

//
// Packages, dies, clusters and caches
//
// The hardware package addresses dies, clusters and caches by their full
// coordinates, since the kernel numbers them within their package. These turn the
// questions this policy asks -- which are all "of this package" or "of this die"
// -- into those coordinates.
//

// packageZone returns the zone of one CPU package, or nil if the machine has no
// such package.
func packageZone(m *hardware.Machine, pkg idset.ID) *hardware.Zone {
	for _, z := range m.Zones(hardware.LevelPackage) {
		if z.ID() == pkg {
			return z
		}
	}
	return nil
}

// packageCPUs returns the CPUs of one package.
func packageCPUs(m *hardware.Machine, pkg idset.ID) cpuset.CPUSet {
	return toCpuSet(m.TopologyIndex().PackageCPUs(pkg))
}

// packageNodeIDs returns the NUMA nodes whose CPUs are in one package.
func packageNodeIDs(m *hardware.Machine, pkg idset.ID) []idset.ID {
	return sortedIDs(hardware.MemoryNodesFor(m, m.TopologyIndex().PackageCPUs(pkg)))
}

// dieIDs returns the die numbers of one package, in increasing order.
func dieIDs(m *hardware.Machine, pkg idset.ID) []idset.ID {
	var ids []idset.ID
	for _, die := range m.TopologyIndex().DieIDs(pkg) {
		ids = append(ids, die.Die)
	}
	return ids
}

// dieCPUs returns the CPUs of one die of one package.
func dieCPUs(m *hardware.Machine, pkg, die idset.ID) cpuset.CPUSet {
	return toCpuSet(m.TopologyIndex().DieCPUs(hardware.DieID{
		Package: pkg,
		Die:     die,
	}))
}

// dieNodeIDs returns the NUMA nodes whose CPUs are on one die of one package.
func dieNodeIDs(m *hardware.Machine, pkg, die idset.ID) []idset.ID {
	cpus := m.TopologyIndex().DieCPUs(hardware.DieID{Package: pkg, Die: die})
	return sortedIDs(hardware.MemoryNodesFor(m, cpus))
}

// clusterIDs returns the cluster numbers of one die of one package, in increasing
// order. These are the clusters the kernel reports, not the logical ones: this
// policy uses them to decide whether the cluster level says anything, and merging
// would hide a die whose every cluster is a single core.
func clusterIDs(m *hardware.Machine, pkg, die idset.ID) []idset.ID {
	var ids []idset.ID
	for _, cl := range m.TopologyIndex().ClusterIDs(hardware.DieID{
		Package: pkg,
		Die:     die,
	}) {
		ids = append(ids, cl.Cluster)
	}
	return ids
}

// l3CacheIDs returns the ids of the level 3 caches this package's CPUs use.
func l3CacheIDs(m *hardware.Machine, pkg idset.ID) []idset.ID {
	var ids []idset.ID
	for _, z := range l3CacheZones(m, pkg) {
		ids = append(ids, z.ID())
	}
	slices.Sort(ids)
	return ids
}

// l3CacheCPUs returns every CPU sharing one level 3 cache of this package,
// including any outside the package: a cache shared across packages belongs to
// both, and the whole of its CPU set is what it groups.
func l3CacheCPUs(m *hardware.Machine, pkg, cache idset.ID) cpuset.CPUSet {
	for _, z := range l3CacheZones(m, pkg) {
		if z.ID() == cache {
			return toCpuSet(z.CPUs())
		}
	}
	return cpuset.New()
}

// l3CacheZones returns the level 3 cache zones this package's CPUs use.
func l3CacheZones(m *hardware.Machine, pkg idset.ID) []*hardware.Zone {
	return hardware.ZonesOverlapping(m, hardware.LevelL3Cache,
		m.TopologyIndex().PackageCPUs(pkg))
}

//
// NUMA nodes
//

// nodeFilter is a predicate on a memory node, replacing the filters the pkg/sysfs
// interface offered.
type nodeFilter func(*hardware.MemoryNode) bool

var (
	// nodeHasMemory passes a node with some memory of its own.
	nodeHasMemory = func(n *hardware.MemoryNode) bool { return n.HasMemory() }
	// nodeHasLocalCPUs passes a node with CPUs of its own.
	nodeHasLocalCPUs = func(n *hardware.MemoryNode) bool { return !n.CPUs().IsEmpty() }
	// nodeHasNoLocalCPUs passes a node with none.
	nodeHasNoLocalCPUs = func(n *hardware.MemoryNode) bool { return n.CPUs().IsEmpty() }
	// nodeOfPMEMKind and nodeOfHBMKind pass a node of that kind.
	nodeOfPMEMKind = nodeOfKind(hardware.MemoryKindPMEM)
	nodeOfHBMKind  = nodeOfKind(hardware.MemoryKindHBM)
	// nodeOfDRAMKind passes a node of ordinary memory. A node the hardware
	// package could not classify counts as one: every node with CPUs is
	// classified, so an unknown one has none, and this is only ever asked
	// together with nodeHasLocalCPUs.
	nodeOfDRAMKind = func(n *hardware.MemoryNode) bool {
		return n.Kind() == hardware.MemoryKindDRAM ||
			n.Kind() == hardware.MemoryKindUnknown
	}
)

// nodeOfKind returns a filter passing nodes of the given memory kind.
func nodeOfKind(kind hardware.MemoryKind) nodeFilter {
	return func(n *hardware.MemoryNode) bool { return n.Kind() == kind }
}

// filterNodes returns those of the given nodes which pass every filter. A node
// the machine does not have passes nothing.
func filterNodes(m *hardware.Machine, ids []idset.ID, filters ...nodeFilter) idset.IDSet {
	out := idset.NewIDSet()

	for _, id := range ids {
		node := m.MemoryNode(id)
		if !node.Valid() {
			continue
		}
		if !nodePasses(node, filters...) {
			continue
		}
		out.Add(id)
	}

	return out
}

// nodePasses reports whether a node passes every filter.
func nodePasses(node *hardware.MemoryNode, filters ...nodeFilter) bool {
	for _, pass := range filters {
		if !pass(node) {
			return false
		}
	}
	return true
}

// closestNodes returns the nodes passing every filter grouped by how far they are
// from the given one, nearest first.
func closestNodes(
	m *hardware.Machine, from idset.ID, filters ...nodeFilter,
) ([]idset.IDSet, []int) {
	groups := hardware.ClosestMemoryNodes(m, from, func(n *hardware.MemoryNode) bool {
		return nodePasses(n, filters...)
	})

	nodes := make([]idset.IDSet, 0, len(groups))
	distances := make([]int, 0, len(groups))
	for _, g := range groups {
		nodes = append(nodes, idset.NewIDSet(g.Nodes...))
		distances = append(distances, g.Distance)
	}

	return nodes, distances
}

// sortedIDs returns ids in increasing order, never nil.
func sortedIDs(ids []idset.ID) []idset.ID {
	out := slices.Clone(ids)
	if out == nil {
		out = []idset.ID{}
	}
	slices.Sort(out)
	return out
}

// nodeHintToCPUs turns a topology hint's list of NUMA nodes into the online CPUs
// of those nodes, as a cpuset string. An unparsable list yields nothing.
func nodeHintToCPUs(m *hardware.Machine) func(string) string {
	return func(nodes string) string {
		mems, err := cpuset.Parse(nodes)
		if err != nil {
			return ""
		}

		cpus := cpuset.New()
		for _, id := range mems.List() {
			if node := m.MemoryNode(id); node.Valid() {
				cpus = cpus.Union(toCpuSet(node.CPUs()))
			}
		}

		return cpus.Intersection(toCpuSet(m.OnlineCPUs())).String()
	}
}
