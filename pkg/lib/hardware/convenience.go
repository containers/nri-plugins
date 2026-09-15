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

// Everything below is derived from what the rest of the package already
// exposes: it adds no state and reads nothing the core did not read. It is here
// because several callers had each grown their own version of it.
//
// Keeping it in one file, written only against the exported API above, is what
// keeps the core small. Nothing here may reach into unexported fields.

import (
	"slices"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

//
// Containment
//

// ZoneOf returns the smallest zone at the given level which holds all of cpus,
// or an invalid zone if none does.
func ZoneOf(m *Machine, level Level, cpus libcpu.CPUSet) *Zone {
	best := invalidZone
	for _, z := range m.Zones(level) {
		if !cpus.IsSubsetOf(z.cpus) {
			continue
		}
		if !best.valid || z.cpus.Size() < best.cpus.Size() {
			best = z
		}
	}
	return best
}

// ZonesWithin returns the zones at the given level all of whose CPUs are in
// cpus. A zone only partly covered is left out, which is what a caller
// subdividing a set of CPUs it may allocate from wants: a half-covered cache is
// not a cache it can hand out whole.
func ZonesWithin(m *Machine, level Level, cpus libcpu.CPUSet) []*Zone {
	var out []*Zone
	for _, z := range m.Zones(level) {
		if z.cpus.IsSubsetOf(cpus) {
			out = append(out, z)
		}
	}
	return out
}

// ZonesOverlapping returns the zones at the given level which have any CPU in
// cpus. A zone only partly covered is included, which is what a caller asking
// what a set of CPUs touches wants: a die with one CPU in the set is still a
// die it has to account for.
//
// This and [ZonesWithin] differ only in that predicate, and choosing the wrong
// one is easy to do quietly. "The caches I can allocate" is Within; "the dies I
// have to reprogram" is Overlapping.
func ZonesOverlapping(m *Machine, level Level, cpus libcpu.CPUSet) []*Zone {
	var out []*Zone
	for _, z := range m.Zones(level) {
		if z.cpus.Intersects(cpus) {
			out = append(out, z)
		}
	}
	return out
}

//
// Threads and cores
//

// AllThreads returns cpus together with every other CPU sharing a core with
// one of them, i.e. cpus rounded up to whole cores.
func AllThreads(m *Machine, cpus libcpu.CPUSet) *libcpu.CpuMask {
	all := libcpu.NewCpuMask()
	cpus.ForEachCpu(func(id int) bool {
		if c := m.CPU(id); c.Valid() && !c.Threads().IsEmpty() {
			all.Set(c.Threads().UnsortedList()...)
		} else {
			all.Set(id)
		}
		return true
	})
	return sealed(all)
}

// SingleThreadPerCore returns the subset of cpus holding only the
// lowest-numbered CPU of each core it covers.
func SingleThreadPerCore(m *Machine, cpus libcpu.CPUSet) *libcpu.CpuMask {
	var (
		out  = libcpu.NewCpuMask()
		done = libcpu.NewCpuMask()
	)

	// List, not UnsortedList: which thread of a core is kept has to be the
	// lowest-numbered one, and that needs a defined order.
	for _, id := range cpus.List() {
		if done.Contains(id) {
			continue
		}
		out.Set(id)
		done.Set(id)
		if c := m.CPU(id); c.Valid() {
			done.Set(c.Threads().UnsortedList()...)
		}
	}

	return sealed(out)
}

//
// Caches
//

// CPUsSharingCache returns cpus together with every other CPU sharing a cache
// at the given level with one of them, i.e. cpus rounded up to whole cache
// groups.
func CPUsSharingCache(m *Machine, level int, cpus libcpu.CPUSet) *libcpu.CpuMask {
	all := libcpu.NewCpuMask()
	cpus.ForEachCpu(func(id int) bool {
		if all.Contains(id) {
			return true
		}
		if cache := m.CPU(id).Cache(level); cache.Valid() {
			all.Set(cache.CPUs().UnsortedList()...)
		} else {
			all.Set(id)
		}
		return true
	})
	return sealed(all)
}

// CacheGrouping is one cache level, and the caches at that level which group
// CPUs in a way worth allocating along. The Groups are what [CacheGroups]
// returns for the Level.
type CacheGrouping struct {
	Level  int
	Groups []*Cache
}

// GroupingCacheLevels returns every cache level whose caches group CPUs in a way
// no coarser or finer level of the topology already offers, coarsest level first.
// It is empty for a machine where no cache level says anything of its own.
//
// There can be more than one such level, which is why this does not answer with
// a single one. A machine whose core types differ can be grouped at a different
// level in each region: the cores of one kind sharing a cache the other kind has
// no access to, while the other kind is grouped by a cache further out. A caller
// which wants one grouping for the whole machine takes the first, the coarsest;
// one which handles such a machine properly walks them all and asks which groups
// hold the CPUs it is placing.
//
// A level with a single group is reported like any other. One group still says
// which CPUs belong together, which is the whole question here, and on a machine
// with two kinds of core there may be exactly one group per kind.
func GroupingCacheLevels(m *Machine) []CacheGrouping {
	var groupings []CacheGrouping

	// From the largest caches inwards, so that the coarsest grouping comes first:
	// a machine whose L3 groups usefully should be allocated along its L3 before
	// an L2 which is finer than anything a caller wants to keep intact.
	levels := m.CacheLevels()
	for i := len(levels) - 1; i >= 0; i-- {
		if groups := CacheGroups(m, levels[i]); len(groups) > 0 {
			groupings = append(groupings, CacheGrouping{
				Level:  levels[i],
				Groups: groups,
			})
		}
	}

	return groupings
}

// CacheGroups returns the caches at the given level which group CPUs
// non-trivially, ordered by package, die, NUMA node and lowest CPU. A cache
// shared by exactly one core, or by a whole die or package, is left out: it
// duplicates a grouping the topology already offers.
//
// A cache which covers the same CPUs as a cluster or a NUMA node is reported,
// even though that level names the same group. Those levels are not always
// there, and a caller which does not consult them would otherwise be told
// nothing about a grouping which is real.
//
// This is the set of groups a CPU allocator should prefer to keep intact, for
// the levels [GroupingCacheLevels] reports.
func CacheGroups(m *Machine, level int) []*Cache {
	var groups []*Cache

	for _, cache := range m.Caches(level) {
		cpus := cache.CPUs()

		// A cache shared by one CPU, or by exactly the threads of one core, says
		// no more than the core level does. One covering a whole die or package
		// says no more than those do.
		switch {
		case cpus.Size() <= 1:
			continue
		case sameAsSomeZone(m, LevelCore, cpus):
			continue
		case sameAsSomeZone(m, LevelDie, cpus):
			continue
		case sameAsSomeZone(m, LevelPackage, cpus):
			continue
		}

		groups = append(groups, cache)
	}

	// Ordered by where they sit, then by lowest CPU, so that a caller walking
	// them in order walks the machine in order.
	slices.SortFunc(groups, func(a, b *Cache) int {
		x, y := groupCoordinates(m, a), groupCoordinates(m, b)
		if x.Package != y.Package {
			return x.Package - y.Package
		}
		if x.Die != y.Die {
			return x.Die - y.Die
		}
		if x.MemoryNode != y.MemoryNode {
			return x.MemoryNode - y.MemoryNode
		}
		return a.CPUs().List()[0] - b.CPUs().List()[0]
	})

	return groups
}

//
// Clusters
//

// SingleCoreClusters says what [LogicalClusters] does with a cluster which holds
// nothing but the threads of one core.
//
// Some machines report every core as its own cluster, which makes the cluster
// level say no more than the core level does. Such a cluster is never reported as
// it stands; this is how a caller says whether it wants those CPUs grouped
// somewhere or not reported at all. Which is right depends on what the clusters
// are being used for, so there is no default.
type SingleCoreClusters bool

const (
	// OmitSingleCoreClusters leaves them out, so that what remains describes
	// real sharing. A die whose every cluster is one core yields nothing.
	OmitSingleCoreClusters SingleCoreClusters = false
	// MergeSingleCoreClusters gathers them into a single cluster. A die whose
	// every cluster is one core yields that one merged cluster.
	//
	// This is what pkg/sysfs reported. A caller which groups CPUs by cluster and
	// would rather group these somewhere than drop them wants this.
	MergeSingleCoreClusters SingleCoreClusters = true
)

// LogicalClusters returns the CPUs of each cluster of the given die, ordered by
// cluster id, with the single-core clusters treated as single says.
//
// A merged cluster spans several of the machine's real clusters, so there is no
// [Zone] to return for it and these are plain CPU sets. The cluster id of any of
// them, merged or not, is the cluster id of its lowest-numbered CPU:
//
//	id := m.CPU(cpus.List()[0]).ClusterID()
//
// which is what the ordering is by.
func LogicalClusters(m *Machine, pkg, die ID, single SingleCoreClusters) []*libcpu.CpuMask {
	var (
		want     = DieID{Package: pkg, Die: die}
		clusters []*libcpu.CpuMask
		merged   = libcpu.NewCpuMask()
	)

	for _, z := range m.Zones(LevelCluster) {
		if z.DieID() != want {
			continue
		}
		if sameAsSomeZone(m, LevelCore, z.CPUs()) {
			if single == MergeSingleCoreClusters {
				merged.Set(z.CPUs().UnsortedList()...)
			}
			continue
		}
		clusters = append(clusters, z.CPUs())
	}

	if !merged.IsEmpty() {
		clusters = append(clusters, sealed(merged))
	}

	slices.SortFunc(clusters, func(a, b *libcpu.CpuMask) int {
		return m.CPU(a.List()[0]).ClusterID() - m.CPU(b.List()[0]).ClusterID()
	})

	return clusters
}

//
// NUMA nodes
//

// MemoryNodeDistanceGroup is a set of NUMA nodes all equally far from some
// other node.
type MemoryNodeDistanceGroup struct {
	// Distance is how far the nodes are, as the kernel reports it.
	Distance int
	// Nodes are the nodes at that distance, in increasing order of id.
	Nodes []ID
}

// ClosestMemoryNodes returns the NUMA nodes other than from which satisfy
// match, grouped by how far they are and ordered nearest first. A nil match
// accepts every node.
//
// A caller looking for the nearest node with ordinary memory reads the first
// group; one willing to look further walks on.
func ClosestMemoryNodes(m *Machine, from ID, match func(*MemoryNode) bool) []MemoryNodeDistanceGroup {
	origin := m.MemoryNode(from)
	if !origin.Valid() {
		return nil
	}

	byDistance := map[int][]ID{}
	for _, node := range m.MemoryNodes() {
		if node.ID() == from {
			continue
		}
		if match != nil && !match(node) {
			continue
		}
		d := origin.Distance(node.ID())
		if d < 0 {
			continue
		}
		byDistance[d] = append(byDistance[d], node.ID())
	}

	distances := make([]int, 0, len(byDistance))
	for d := range byDistance {
		distances = append(distances, d)
	}
	slices.Sort(distances)

	groups := make([]MemoryNodeDistanceGroup, 0, len(distances))
	for _, d := range distances {
		nodes := byDistance[d]
		slices.Sort(nodes)
		groups = append(groups, MemoryNodeDistanceGroup{Distance: d, Nodes: nodes})
	}

	return groups
}

// MemoryNodesFor returns the ids of the NUMA nodes any of whose CPUs are in
// cpus, i.e. the memory local to those CPUs.
func MemoryNodesFor(m *Machine, cpus libcpu.CPUSet) []ID {
	var out []ID
	for _, node := range m.MemoryNodes() {
		if node.CPUs().Intersects(cpus) {
			out = append(out, node.ID())
		}
	}
	return out
}

// MemoryNodesOfKind returns the ids of the NUMA nodes holding the given kind of
// memory.
func MemoryNodesOfKind(m *Machine, kind MemoryKind) []ID {
	var out []ID
	for _, node := range m.MemoryNodes() {
		if node.Kind() == kind {
			out = append(out, node.ID())
		}
	}
	return out
}

//
// shared helpers
//

// sameAsSomeZone reports whether cpus is exactly the CPUs of one of the zones at
// a level. It is how a derived grouping tells whether it is saying anything the
// topology does not already say.
func sameAsSomeZone(m *Machine, level Level, cpus libcpu.CPUSet) bool {
	for _, z := range m.Zones(level) {
		if z.CPUs().Equals(cpus) {
			return true
		}
	}
	return false
}

// groupCoordinates returns where a cache group sits, taken from one of its CPUs.
// Every CPU of a group is in the same package and die; a group which straddles
// either would have been left out for matching no zone.
func groupCoordinates(m *Machine, cache *Cache) Coordinates {
	cpus := cache.CPUs().List()
	if len(cpus) == 0 {
		return Coordinates{
			CPU: unknownID, Package: unknownID, Die: unknownID,
			Cluster: unknownID, MemoryNode: unknownID, Core: unknownID,
		}
	}
	return m.TopologyIndex().CoordinatesOf(cpus[0])
}
