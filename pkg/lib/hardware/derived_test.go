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
	"slices"
	"testing"
	"testing/fstest"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// TestTopologyIndexAgreesWithZones checks that the flattened index answers the
// same thing the zones do. It is a lookup table over them, so any disagreement
// is a bug in the flattening.
func TestTopologyIndexAgreesWithZones(t *testing.T) {
	for _, name := range recordedTrees {
		t.Run(name, func(t *testing.T) {
			m := openRecorded(t, name)
			x := m.TopologyIndex()
			if x == nil {
				t.Fatal("TopologyIndex() is nil")
			}

			// the whole-machine sets are the machine's own
			if !x.AllCPUs().Equals(m.PresentCPUs()) {
				t.Errorf("AllCPUs %s != PresentCPUs %s", x.AllCPUs(), m.PresentCPUs())
			}
			if !x.OnlineCPUs().Equals(m.OnlineCPUs()) {
				t.Error("OnlineCPUs disagrees with the machine")
			}
			if !x.OfflineCPUs().Equals(m.OfflineCPUs()) {
				t.Error("OfflineCPUs disagrees with the machine")
			}
			if !x.IsolatedCPUs().Equals(m.IsolatedCPUs()) {
				t.Error("IsolatedCPUs disagrees with the machine")
			}

			// every coordinate the index enumerates has the zone's CPUs
			for _, z := range m.Zones(LevelPackage) {
				if got := x.PackageCPUs(z.ID()); !got.Equals(z.CPUs()) {
					t.Errorf("PackageCPUs(%d) = %s, want %s", z.ID(), got, z.CPUs())
				}
			}
			for _, z := range m.Zones(LevelDie) {
				if got := x.DieCPUs(z.DieID()); !got.Equals(z.CPUs()) {
					t.Errorf("DieCPUs(%s) = %s, want %s", z.DieID(), got, z.CPUs())
				}
			}
			for _, z := range m.Zones(LevelCluster) {
				if got := x.ClusterCPUs(z.ClusterID()); !got.Equals(z.CPUs()) {
					t.Errorf("ClusterCPUs(%s) = %s, want %s",
						z.ClusterID(), got, z.CPUs())
				}
			}
			for _, n := range m.MemoryNodes() {
				if got := x.MemoryNodeCPUs(n.ID()); !got.Equals(n.CPUs()) {
					t.Errorf("MemoryNodeCPUs(%d) = %s, want %s", n.ID(), got, n.CPUs())
				}
			}
			for _, c := range m.Caches(0) {
				// c.CacheID(), not a hand-built one: the kind is part of the
				// coordinate and leaving it out names a different cache
				if got := x.CacheCPUs(c.CacheID()); !got.Equals(c.CPUs()) {
					t.Errorf("CacheCPUs(%s) = %s, want %s",
						c.CacheID(), got, c.CPUs())
				}
			}

			// the enumerations cover exactly what the machine has
			if got, want := len(x.PackageIDs()), len(m.Zones(LevelPackage)); got != want {
				t.Errorf("PackageIDs has %d entries, want %d", got, want)
			}
			if got, want := len(x.DieIDs()), len(m.Zones(LevelDie)); got != want {
				t.Errorf("DieIDs has %d entries, want %d", got, want)
			}
			if got, want := len(x.MemoryNodeIDs()), len(m.MemoryNodes()); got != want {
				t.Errorf("MemoryNodeIDs has %d entries, want %d", got, want)
			}
			if got, want := len(x.CacheIDs()), len(m.Caches(0)); got != want {
				t.Errorf("CacheIDs has %d entries, want %d", got, want)
			}

			// filtering by package keeps only that package's dies and cores
			for _, pkg := range x.PackageIDs() {
				for _, die := range x.DieIDs(pkg) {
					if die.Package != pkg {
						t.Errorf("DieIDs(%d) returned %s", pkg, die)
					}
				}
				for _, core := range x.CoreIDs(pkg) {
					if core.Package != pkg {
						t.Errorf("CoreIDs(%d) returned %s", pkg, core)
					}
				}
			}

			// thread siblings agree with the CPUs' own view
			for _, id := range m.CPUIDs() {
				if !m.CPU(id).Online() {
					continue
				}
				if got := x.ThreadsOf(id); !got.Equals(m.CPU(id).Threads()) {
					t.Errorf("ThreadsOf(%d) = %s, want %s",
						id, got, m.CPU(id).Threads())
				}
			}

			// coordinates round-trip back to the right zones
			for _, id := range m.CPUIDs() {
				c := m.CPU(id)
				if !c.Online() {
					continue
				}
				at := x.CoordinatesOf(id)
				if at.CPU != id {
					t.Errorf("CoordinatesOf(%d).CPU = %d", id, at.CPU)
				}
				if at.Package != c.PackageID() || at.Core != c.CoreID() ||
					at.MemoryNode != c.NodeID() || at.Kind != c.Kind() {
					t.Errorf("CoordinatesOf(%d) = %s disagrees with the CPU", id, at)
				}
				if !x.PackageCPUs(at.Package).Contains(id) {
					t.Errorf("cpu%d is not in its own package's CPUs", id)
				}
			}

			// an absent CPU answers with unknown coordinates, not a panic
			at := x.CoordinatesOf(1 << 20)
			if at.CPU != unknownID || at.Package != unknownID {
				t.Errorf("CoordinatesOf(absent) = %s, want all unknown", at)
			}
			// so do absent coordinates
			if !x.PackageCPUs(1 << 20).IsEmpty() {
				t.Error("PackageCPUs of an absent package is not empty")
			}
			if !x.DieCPUs(DieID{Package: 1 << 20}).IsEmpty() {
				t.Error("DieCPUs of an absent die is not empty")
			}

			// asking twice gives the same table
			if m.TopologyIndex() != x {
				t.Error("TopologyIndex() built a second table")
			}

			t.Logf("\n%s", x)
		})
	}
}

// TestDerivedSets checks the set-to-set helpers against the topology they are
// derived from.
func TestDerivedSets(t *testing.T) {
	for _, name := range recordedTrees {
		t.Run(name, func(t *testing.T) {
			m := openRecorded(t, name)
			online := m.OnlineCPUs()

			// AllThreads only ever grows a set, and always to whole cores
			for _, cpus := range sampleSets(m) {
				all := AllThreads(m, cpus)
				if !cpus.IsSubsetOf(all) {
					t.Errorf("AllThreads(%s) = %s dropped CPUs", cpus, all)
				}
				assertSealedNamed(t, "AllThreads result", all)
				if again := AllThreads(m, all); !again.Equals(all) {
					t.Errorf("AllThreads is not idempotent on %s", cpus)
				}
				all.ForEachCpu(func(id int) bool {
					if c := m.CPU(id); c.Online() && !c.Threads().IsSubsetOf(all) {
						t.Errorf("AllThreads(%s) = %s is missing a sibling of cpu%d",
							cpus, all, id)
					}
					return true
				})
			}

			// SingleThreadPerCore only ever shrinks, and keeps one CPU per core
			for _, cpus := range sampleSets(m) {
				one := SingleThreadPerCore(m, cpus)
				if !one.IsSubsetOf(cpus) {
					t.Errorf("SingleThreadPerCore(%s) = %s added CPUs", cpus, one)
				}
				assertSealedNamed(t, "SingleThreadPerCore result", one)
				if again := SingleThreadPerCore(m, one); !again.Equals(one) {
					t.Errorf("SingleThreadPerCore is not idempotent on %s", cpus)
				}
				seen := libcpu.NewCpuMask()
				one.ForEachCpu(func(id int) bool {
					if seen.Contains(id) {
						t.Errorf("SingleThreadPerCore(%s) kept two threads of a core",
							cpus)
					}
					seen.Set(m.CPU(id).Threads().UnsortedList()...)
					return true
				})
			}

			// CPUsSharingCache grows to whole cache groups
			for _, level := range m.CacheLevels() {
				for _, cpus := range sampleSets(m) {
					all := CPUsSharingCache(m, level, cpus)
					if !cpus.IsSubsetOf(all) {
						t.Errorf("CPUsSharingCache(%d, %s) dropped CPUs", level, cpus)
					}
					assertSealedNamed(t, "CPUsSharingCache result", all)
				}
			}

			// grouping cache levels come coarsest first, and the groups at each
			// are non-trivial, disjoint, and within the machine
			coarser := 0
			for i, grouping := range GroupingCacheLevels(m) {
				t.Logf("grouping cache level %d, %d groups",
					grouping.Level, len(grouping.Groups))

				if i > 0 && grouping.Level >= coarser {
					t.Errorf("cache level %d follows %d, not coarsest first",
						grouping.Level, coarser)
				}
				coarser = grouping.Level

				if len(grouping.Groups) == 0 {
					t.Errorf("cache level %d reported with no groups", grouping.Level)
				}

				// groups of one level partition, groups of different levels nest
				seen := emptyCPUs.Union()
				for _, g := range grouping.Groups {
					if g.Level() != grouping.Level {
						t.Errorf("cache group %s is not at level %d",
							g.Key(), grouping.Level)
					}
					if g.CPUs().Size() <= 1 {
						t.Errorf("cache group %s has %d CPUs", g.Key(), g.CPUs().Size())
					}
					if g.CPUs().Intersects(seen) {
						t.Errorf("cache group %s overlaps another", g.Key())
					}
					seen = seen.Union(g.CPUs())
					if sameAsSomeZone(m, LevelCore, g.CPUs()) {
						t.Errorf("cache group %s is just a core", g.Key())
					}
					if sameAsSomeZone(m, LevelPackage, g.CPUs()) {
						t.Errorf("cache group %s is just a package", g.Key())
					}
				}
			}

			// logical clusters, for every die the machine has
			for _, die := range m.TopologyIndex().DieIDs() {
				dieCPUs := m.TopologyIndex().DieCPUs(die)

				// omitted: nothing reported is just a core, and every cluster
				// reported is one of the die's own
				for _, cpus := range LogicalClusters(m, die.Package, die.Die,
					OmitSingleCoreClusters) {
					if !cpus.IsSubsetOf(dieCPUs) {
						t.Errorf("LogicalClusters(%s) returned %s, not on the die",
							die, cpus)
					}
					if sameAsSomeZone(m, LevelCore, cpus) {
						t.Errorf("logical cluster %s is just a core", cpus)
					}
				}

				// merged: the same clusters plus at most one more, and together
				// they cover no more than the die
				var (
					omit  = LogicalClusters(m, die.Package, die.Die, OmitSingleCoreClusters)
					merge = LogicalClusters(m, die.Package, die.Die, MergeSingleCoreClusters)
					seen  = emptyCPUs.Union()
				)
				if len(merge) != len(omit) && len(merge) != len(omit)+1 {
					t.Errorf("%s: %d merged clusters, want %d or %d",
						die, len(merge), len(omit), len(omit)+1)
				}
				for _, cpus := range merge {
					if cpus.Intersects(seen) {
						t.Errorf("%s: merged cluster %s overlaps another", die, cpus)
					}
					seen = seen.Union(cpus)
					if !cpus.IsSubsetOf(dieCPUs) {
						t.Errorf("%s: merged cluster %s is not on the die", die, cpus)
					}
				}

				// the ids the ordering promises are increasing
				last := -1
				for _, cpus := range merge {
					id := m.CPU(cpus.List()[0]).ClusterID()
					if id <= last {
						t.Errorf("%s: cluster ids are not increasing: %d after %d",
							die, id, last)
					}
					last = id
				}
			}

			// closest nodes: nearest first, never the node itself
			for _, node := range m.MemoryNodes() {
				groups := ClosestMemoryNodes(m, node.ID(), nil)
				last := -1
				for _, g := range groups {
					if g.Distance <= last {
						t.Errorf("node#%d: distances are not increasing: %d after %d",
							node.ID(), g.Distance, last)
					}
					last = g.Distance
					if slices.Contains(g.Nodes, node.ID()) {
						t.Errorf("node#%d is among its own closest nodes", node.ID())
					}
				}

				// a match which accepts nothing yields nothing
				if got := ClosestMemoryNodes(m, node.ID(), func(*MemoryNode) bool {
					return false
				}); len(got) != 0 {
					t.Errorf("node#%d: a false match returned %d groups", node.ID(), len(got))
				}
			}
			if got := ClosestMemoryNodes(m, 1<<20, nil); got != nil {
				t.Error("ClosestMemoryNodes of an absent node returned groups")
			}

			// nodes local to a set of CPUs
			if got := MemoryNodesFor(m, online); len(got) == 0 {
				t.Error("no NUMA nodes are local to the online CPUs")
			}
			if got := MemoryNodesFor(m, emptyCPUs); len(got) != 0 {
				t.Errorf("MemoryNodesFor(empty) returned %v", got)
			}

			// every node is of exactly one kind
			total := 0
			for _, kind := range []MemoryKind{
				MemoryKindDRAM, MemoryKindPMEM, MemoryKindHBM, MemoryKindUnknown,
			} {
				total += len(MemoryNodesOfKind(m, kind))
			}
			if total != len(m.MemoryNodes()) {
				t.Errorf("kinds cover %d nodes, want %d", total, len(m.MemoryNodes()))
			}
		})
	}
}

// sampleSets returns a spread of CPU sets to exercise a set-to-set helper with:
// nothing, one CPU, one core, one node, one package, everything, and a set which
// deliberately cuts across cores.
func sampleSets(m *Machine) []*libcpu.CpuMask {
	sets := []*libcpu.CpuMask{emptyCPUs, m.OnlineCPUs()}

	ids := m.OnlineCPUs().List()
	if len(ids) == 0 {
		return sets
	}

	sets = append(sets, sealed(libcpu.NewCpuMask(ids[0])))
	sets = append(sets, m.CPU(ids[0]).Threads())

	if nodes := m.MemoryNodes(); len(nodes) > 0 {
		sets = append(sets, nodes[0].CPUs())
	}
	if pkgs := m.Zones(LevelPackage); len(pkgs) > 0 {
		sets = append(sets, pkgs[0].CPUs())
	}

	// every other online CPU, which splits cores wherever they are paired
	ragged := libcpu.NewCpuMask()
	for i := 0; i < len(ids); i += 2 {
		ragged.Set(ids[i])
	}
	sets = append(sets, sealed(ragged))

	return sets
}

// hybridCacheFS is a machine whose two kinds of core are grouped at two
// different cache levels: CPUs 0-3 share an L3 the other four have no access to,
// and CPUs 4-7 share an L2 the first four do not. Each level therefore holds
// exactly one group.
//
// No recorded tree looks like this, and it is the shape which says why
// [GroupingCacheLevels] answers with every level rather than with one.
func hybridCacheFS() fstest.MapFS {
	fsys := fstest.MapFS{
		"proc/meminfo": file("MemTotal:       1048576 kB\n"),
	}

	fsys["sys/devices/system/cpu/possible"] = file("0-7\n")
	fsys["sys/devices/system/cpu/present"] = file("0-7\n")
	fsys["sys/devices/system/cpu/online"] = file("0-7\n")

	for i := 0; i < 8; i++ {
		dir := "sys/devices/system/cpu/cpu" + itoa(i)
		fsys[dir+"/topology/physical_package_id"] = file("0\n")
		fsys[dir+"/topology/core_id"] = file(itoa(i) + "\n")
		fsys[dir+"/topology/core_cpus_list"] = file(itoa(i) + "\n")

		if i < 4 {
			// a private L2, and an L3 shared by these four only
			fsys[dir+"/cache/index0/level"] = file("2\n")
			fsys[dir+"/cache/index0/type"] = file("Unified\n")
			fsys[dir+"/cache/index0/id"] = file(itoa(i) + "\n")
			fsys[dir+"/cache/index0/shared_cpu_list"] = file(itoa(i) + "\n")
			fsys[dir+"/cache/index0/size"] = file("2048K\n")
			fsys[dir+"/cache/index1/level"] = file("3\n")
			fsys[dir+"/cache/index1/type"] = file("Unified\n")
			fsys[dir+"/cache/index1/id"] = file("0\n")
			fsys[dir+"/cache/index1/shared_cpu_list"] = file("0-3\n")
			fsys[dir+"/cache/index1/size"] = file("16384K\n")
		} else {
			// one L2 for the four of them, and no L3 at all
			fsys[dir+"/cache/index0/level"] = file("2\n")
			fsys[dir+"/cache/index0/type"] = file("Unified\n")
			fsys[dir+"/cache/index0/id"] = file("4\n")
			fsys[dir+"/cache/index0/shared_cpu_list"] = file("4-7\n")
			fsys[dir+"/cache/index0/size"] = file("4096K\n")
		}
	}

	return fsys
}

func TestGroupingCacheLevels(t *testing.T) {
	t.Run("two levels, one group each", func(t *testing.T) {
		m, err := Discover(WithFS(hybridCacheFS()))
		if err != nil {
			t.Fatalf("failed to discover the test machine: %v", err)
		}

		groupings := GroupingCacheLevels(m)
		if len(groupings) != 2 {
			t.Fatalf("expected two grouping levels, got %v", groupings)
		}

		// coarsest first, so the L3 of CPUs 0-3 comes before the L2 of 4-7
		for i, want := range []struct {
			level int
			cpus  string
		}{
			{level: 3, cpus: "0-3"},
			{level: 2, cpus: "4-7"},
		} {
			got := groupings[i]
			if got.Level != want.level {
				t.Errorf("grouping %d: expected level %d, got %d",
					i, want.level, got.Level)
			}
			if len(got.Groups) != 1 {
				t.Fatalf("grouping %d: expected one group, got %v", i, got.Groups)
			}
			if cpus := got.Groups[0].CPUs().String(); cpus != want.cpus {
				t.Errorf("grouping %d: expected CPUs %s, got %s", i, want.cpus, cpus)
			}
		}
	})

	t.Run("sample1", func(t *testing.T) {
		m := openRecorded(t, "sample1")

		// The one recorded machine with a cache grouping of its own: its two
		// E-core modules share an L2 each, and nothing else at any level says
		// anything a coarser or finer level does not.
		groupings := GroupingCacheLevels(m)
		if len(groupings) != 1 {
			t.Fatalf("expected one grouping level, got %v", groupings)
		}
		if groupings[0].Level != 2 {
			t.Errorf("expected level 2, got %d", groupings[0].Level)
		}
		if len(groupings[0].Groups) != 2 {
			t.Fatalf("expected two groups, got %v", groupings[0].Groups)
		}
		for _, g := range groupings[0].Groups {
			if g.CPUs().Size() != 4 {
				t.Errorf("expected a group of 4 CPUs, got %s", g.CPUs())
			}
		}
	})
}
