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

// This is the reason pkg/sysfs stays in the tree while this package exists: both
// implementations are in one build, so every method of both can be run against
// the same recorded topology and the answers compared. Nothing else states
// equivalence as strongly.
//
// It compares everything, including the methods no caller uses, because the ones
// nobody calls are exactly the ones a reimplementation gets wrong unnoticed.
//
// Once every consumer has moved off pkg/sysfs, this file and pkg/sysfs go
// together.

package system_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/containers/nri-plugins/pkg/lib/hardware/system"
	"github.com/containers/nri-plugins/pkg/sysfs" //nolint:staticcheck // deprecated on purpose: this is what it is compared against
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// trees are the recorded sysfs topologies to compare over, as test-setup.sh
// unpacks them. They come from pkg/sysfs, pkg/cpuallocator and the
// topology-aware policy, which each keep some for their own tests.
var trees = []string{
	"sample1",
	"sample2",
	"2-socket-4-node-40-core",
	"4-socket-server-nosnc",
	"desktop",
	"server",
}

// pair is one topology discovered through both implementations.
type pair struct {
	name string
	old  sysfs.System
	new  system.System
}

// discoverBoth reads one recorded tree through pkg/sysfs and through this
// package. Both take the sysfs mount point, so both get the same argument.
func discoverBoth(t *testing.T, tree string) pair {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("testdata", tree, "sys"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("recorded tree %s is not unpacked: run ./test-setup.sh", tree)
	}

	old, err := sysfs.DiscoverSystemAt(root)
	if err != nil {
		t.Fatalf("pkg/sysfs failed to discover %s: %v", tree, err)
	}

	new, err := system.DiscoverSystemAt(root)
	if err != nil {
		t.Fatalf("the drop-in failed to discover %s: %v", tree, err)
	}

	return pair{name: tree, old: old, new: new}
}

func TestEquivalentSystem(t *testing.T) {
	for _, tree := range trees {
		t.Run(tree, func(t *testing.T) {
			p := discoverBoth(t, tree)

			// whole-machine sets
			eqCPUSet(t, "CPUSet", p.old.CPUSet(), p.new.CPUSet())
			eqCPUSet(t, "PossibleCPUs", p.old.PossibleCPUs(), p.new.PossibleCPUs())
			eqCPUSet(t, "PresentCPUs", p.old.PresentCPUs(), p.new.PresentCPUs())
			eqCPUSet(t, "OnlineCPUs", p.old.OnlineCPUs(), p.new.OnlineCPUs())
			eqCPUSet(t, "OfflineCPUs", p.old.OfflineCPUs(), p.new.OfflineCPUs())
			eqCPUSet(t, "IsolatedCPUs", p.old.IsolatedCPUs(), p.new.IsolatedCPUs())
			eqCPUSet(t, "Offlined", p.old.Offlined(), p.new.Offlined())
			eqCPUSet(t, "Isolated", p.old.Isolated(), p.new.Isolated())

			// counts
			eqInt(t, "PackageCount", p.old.PackageCount(), p.new.PackageCount())
			eqInt(t, "SocketCount", p.old.SocketCount(), p.new.SocketCount())
			eqInt(t, "CPUCount", p.old.CPUCount(), p.new.CPUCount())
			eqInt(t, "NUMANodeCount", p.old.NUMANodeCount(), p.new.NUMANodeCount())
			eqInt(t, "MinThreadCount", p.old.MinThreadCount(), p.new.MinThreadCount())
			eqInt(t, "MaxThreadCount", p.old.MaxThreadCount(), p.new.MaxThreadCount())

			// id lists
			eqIDs(t, "PackageIDs", p.old.PackageIDs(), p.new.PackageIDs())
			eqIDs(t, "NodeIDs", p.old.NodeIDs(), p.new.NodeIDs())
			eqIDs(t, "CPUIDs", p.old.CPUIDs(), p.new.CPUIDs())

			eqCoreKinds(t, p)
			eqPackages(t, p)
			eqNodes(t, p)
			eqCPUs(t, p)
			eqDerivedSets(t, p)
			eqNodeFilters(t, p)
			eqNodeHints(t, p)
		})
	}
}

func eqCoreKinds(t *testing.T, p pair) {
	t.Helper()

	oldKinds, newKinds := p.old.CoreKinds(), p.new.CoreKinds()
	if len(oldKinds) != len(newKinds) {
		t.Errorf("CoreKinds: %d vs %d kinds", len(oldKinds), len(newKinds))
	}

	// compare by kind rather than by position: pkg/sysfs iterates a map
	for _, kind := range []int{int(sysfs.PerformanceCore), int(sysfs.EfficientCore)} {
		eqCPUSet(t, "CoreKindCPUs("+sysfs.CoreKind(kind).String()+")",
			p.old.CoreKindCPUs(sysfs.CoreKind(kind)),
			p.new.CoreKindCPUs(system.CoreKind(kind)))

		inOld := slices.Contains(oldKinds, sysfs.CoreKind(kind))
		inNew := slices.Contains(newKinds, system.CoreKind(kind))
		if inOld != inNew {
			t.Errorf("CoreKinds: %s present=%v vs %v",
				sysfs.CoreKind(kind), inOld, inNew)
		}
	}
}

func eqPackages(t *testing.T, p pair) {
	t.Helper()

	for _, id := range p.old.PackageIDs() {
		var (
			op = p.old.Package(id)
			np = p.new.Package(id)
			at = "package#" + itoa(id)
		)
		if np == nil {
			t.Errorf("%s: the drop-in has no such package", at)
			continue
		}

		eqInt(t, at+" ID", op.ID(), np.ID())
		eqCPUSet(t, at+" CPUSet", op.CPUSet(), np.CPUSet())
		eqIDs(t, at+" DieIDs", op.DieIDs(), np.DieIDs())
		eqIDs(t, at+" NodeIDs", op.NodeIDs(), np.NodeIDs())
		eqIDs(t, at+" L3CacheIDs", op.L3CacheIDs(), np.L3CacheIDs())

		for _, l3 := range op.L3CacheIDs() {
			eqCPUSet(t, at+" L3CacheCPUSet("+itoa(l3)+")",
				op.L3CacheCPUSet(l3), np.L3CacheCPUSet(l3))
		}

		for _, die := range op.DieIDs() {
			d := at + "/die#" + itoa(die)
			eqCPUSet(t, d+" DieCPUSet", op.DieCPUSet(die), np.DieCPUSet(die))
			eqIDs(t, d+" DieNodeIDs", op.DieNodeIDs(die), np.DieNodeIDs(die))
			eqIDs(t, d+" DieClusterIDs",
				op.DieClusterIDs(die), np.DieClusterIDs(die))
			eqIDs(t, d+" LogicalDieClusterIDs",
				op.LogicalDieClusterIDs(die), np.LogicalDieClusterIDs(die))

			for _, cl := range op.DieClusterIDs(die) {
				eqCPUSet(t, d+" DieClusterCPUSet("+itoa(cl)+")",
					op.DieClusterCPUSet(die, cl), np.DieClusterCPUSet(die, cl))
			}
			for _, cl := range op.LogicalDieClusterIDs(die) {
				eqCPUSet(t, d+" LogicalDieClusterCPUSet("+itoa(cl)+")",
					op.LogicalDieClusterCPUSet(die, cl),
					np.LogicalDieClusterCPUSet(die, cl))
			}
		}

		// a die which is not there answers the same way in both
		eqCPUSet(t, at+" DieCPUSet(absent)",
			op.DieCPUSet(1<<20), np.DieCPUSet(1<<20))
		eqIDs(t, at+" DieNodeIDs(absent)",
			op.DieNodeIDs(1<<20), np.DieNodeIDs(1<<20))
		eqIDs(t, at+" DieClusterIDs(absent)",
			op.DieClusterIDs(1<<20), np.DieClusterIDs(1<<20))
		eqCPUSet(t, at+" L3CacheCPUSet(absent)",
			op.L3CacheCPUSet(1<<20), np.L3CacheCPUSet(1<<20))
	}
}

func eqNodes(t *testing.T, p pair) {
	t.Helper()

	for _, id := range p.old.NodeIDs() {
		var (
			on = p.old.Node(id)
			nn = p.new.Node(id)
			at = "node#" + itoa(id)
		)
		if nn == nil {
			t.Errorf("%s: the drop-in has no such node", at)
			continue
		}

		eqInt(t, at+" ID", on.ID(), nn.ID())
		eqInt(t, at+" PackageID", on.PackageID(), nn.PackageID())
		eqInt(t, at+" DieID", on.DieID(), nn.DieID())
		eqCPUSet(t, at+" CPUSet", on.CPUSet(), nn.CPUSet())
		eqInts(t, at+" Distance", on.Distance(), nn.Distance())
		eqBool(t, at+" HasNormalMemory",
			on.HasNormalMemory(), nn.HasNormalMemory())
		eqStr(t, at+" GetMemoryType",
			on.GetMemoryType().String(), nn.GetMemoryType().String())

		for _, to := range p.old.NodeIDs() {
			eqInt(t, at+" DistanceFrom("+itoa(to)+")",
				on.DistanceFrom(to), nn.DistanceFrom(to))
		}
		eqInt(t, at+" DistanceFrom(absent)",
			on.DistanceFrom(1<<20), nn.DistanceFrom(1<<20))

		oNodes, oDist := on.ClosestNodes()
		nNodes, nDist := nn.ClosestNodes()
		eqInts(t, at+" ClosestNodes distances", oDist, nDist)
		eqIDSets(t, at+" ClosestNodes", oNodes, nNodes)

		// MemoryInfo reads the machine, so free and used can move between the two
		// calls. Only the total is stable enough to compare.
		oi, oerr := on.MemoryInfo()
		ni, nerr := nn.MemoryInfo()
		switch {
		case (oerr == nil) != (nerr == nil):
			t.Errorf("%s MemoryInfo: errors differ: %v vs %v", at, oerr, nerr)
		case oerr == nil && oi.MemTotal != ni.MemTotal:
			t.Errorf("%s MemoryInfo MemTotal: %d vs %d", at, oi.MemTotal, ni.MemTotal)
		}
	}
}

func eqCPUs(t *testing.T, p pair) {
	t.Helper()

	for _, id := range p.old.CPUIDs() {
		var (
			oc = p.old.CPU(id)
			nc = p.new.CPU(id)
			at = "cpu#" + itoa(id)
		)
		if nc == nil {
			t.Errorf("%s: the drop-in has no such CPU", at)
			continue
		}

		eqInt(t, at+" ID", oc.ID(), nc.ID())
		eqInt(t, at+" PackageID", oc.PackageID(), nc.PackageID())
		eqInt(t, at+" DieID", oc.DieID(), nc.DieID())
		eqInt(t, at+" ClusterID", oc.ClusterID(), nc.ClusterID())
		eqInt(t, at+" NodeID", oc.NodeID(), nc.NodeID())
		eqInt(t, at+" CoreID", oc.CoreID(), nc.CoreID())
		eqCPUSet(t, at+" ThreadCPUSet", oc.ThreadCPUSet(), nc.ThreadCPUSet())
		eqBool(t, at+" Online", oc.Online(), nc.Online())
		eqBool(t, at+" Isolated", oc.Isolated(), nc.Isolated())
		eqStr(t, at+" CoreKind", oc.CoreKind().String(), nc.CoreKind().String())
		eqStr(t, at+" EPP", oc.EPP().String(), nc.EPP().String())
		eqInt(t, at+" SstClos", oc.SstClos(), nc.SstClos())
		eqInt(t, at+" CacheCount", oc.CacheCount(), nc.CacheCount())

		if got, want := nc.BaseFrequency(), oc.BaseFrequency(); got != want {
			t.Errorf("%s BaseFrequency: %d vs %d", at, want, got)
		}
		of, nf := oc.FrequencyRange(), nc.FrequencyRange()
		if of.Base != nf.Base || of.Min != nf.Min || of.Max != nf.Max {
			t.Errorf("%s FrequencyRange: %+v vs %+v", at, of, nf)
		}

		eqCaches(t, at+" GetCaches", oc.GetCaches(), nc.GetCaches())
		eqCaches(t, at+" GetLastLevelCaches",
			oc.GetLastLevelCaches(), nc.GetLastLevelCaches())
		eqCPUSet(t, at+" GetLastLevelCacheCPUSet",
			oc.GetLastLevelCacheCPUSet(), nc.GetLastLevelCacheCPUSet())

		for level := 0; level <= 4; level++ {
			eqCaches(t, at+" GetCachesByLevel("+itoa(level)+")",
				oc.GetCachesByLevel(level), nc.GetCachesByLevel(level))
			eqCPUSet(t, at+" GetNthLevelCacheCPUSet("+itoa(level)+")",
				oc.GetNthLevelCacheCPUSet(level), nc.GetNthLevelCacheCPUSet(level))
		}
		for idx := -1; idx <= oc.CacheCount(); idx++ {
			eqCache(t, at+" GetCacheByIndex("+itoa(idx)+")",
				oc.GetCacheByIndex(idx), nc.GetCacheByIndex(idx))
		}
	}
}

// eqCacheIdentity checks that both implementations hand out one *Cache per cache,
// which the balloons policy relies on by keying a map with it.
func TestEquivalentCacheIdentity(t *testing.T) {
	for _, tree := range trees {
		t.Run(tree, func(t *testing.T) {
			p := discoverBoth(t, tree)

			oldSeen := map[*sysfs.Cache]cpuset.CPUSet{}
			newSeen := map[*system.Cache]cpuset.CPUSet{}

			for _, id := range p.old.CPUIDs() {
				for _, c := range p.old.CPU(id).GetCaches() {
					oldSeen[c] = c.SharedCPUSet()
				}
				for _, c := range p.new.CPU(id).GetCaches() {
					newSeen[c] = c.SharedCPUSet()
				}
			}

			if len(oldSeen) != len(newSeen) {
				t.Errorf("distinct caches by pointer: %d vs %d",
					len(oldSeen), len(newSeen))
			}

			// the same cache asked for twice is the same pointer
			for _, id := range p.old.CPUIDs() {
				a := p.new.CPU(id).GetCaches()
				b := p.new.CPU(id).GetCaches()
				for i := range a {
					if a[i] != b[i] {
						t.Fatalf("cpu#%d cache %d is a different pointer each time",
							id, i)
					}
				}
			}
		})
	}
}

func eqDerivedSets(t *testing.T, p pair) {
	t.Helper()

	for _, tc := range sampleSets(p) {
		eqCPUSet(t, "AllThreadsForCPUs("+tc.String()+")",
			p.old.AllThreadsForCPUs(tc), p.new.AllThreadsForCPUs(tc))
		eqCPUSet(t, "SingleThreadForCPUs("+tc.String()+")",
			p.old.SingleThreadForCPUs(tc), p.new.SingleThreadForCPUs(tc))

		for level := 0; level <= 4; level++ {
			eqCPUSet(t,
				"AllCPUsSharingNthLevelCacheWithCPUs("+itoa(level)+", "+tc.String()+")",
				p.old.AllCPUsSharingNthLevelCacheWithCPUs(level, tc),
				p.new.AllCPUsSharingNthLevelCacheWithCPUs(level, tc))
		}

		// IDSetForCPUs, with each of the ids a caller might ask for
		eqIDSet(t, "IDSetForCPUs(package)",
			p.old.IDSetForCPUs(tc, func(c sysfs.CPU) idset.ID { return c.PackageID() }),
			p.new.IDSetForCPUs(tc, func(c system.CPU) idset.ID { return c.PackageID() }))
		eqIDSet(t, "IDSetForCPUs(node)",
			p.old.IDSetForCPUs(tc, func(c sysfs.CPU) idset.ID { return c.NodeID() }),
			p.new.IDSetForCPUs(tc, func(c system.CPU) idset.ID { return c.NodeID() }))
		eqIDSet(t, "IDSetForCPUs(die)",
			p.old.IDSetForCPUs(tc, func(c sysfs.CPU) idset.ID { return c.DieID() }),
			p.new.IDSetForCPUs(tc, func(c system.CPU) idset.ID { return c.DieID() }))
	}
}

func eqNodeFilters(t *testing.T, p pair) {
	t.Helper()

	filters := []struct {
		name string
		old  sysfs.NodeFilter
		new  system.NodeFilter
	}{
		{"NodeOfDRAMType", sysfs.NodeOfDRAMType, system.NodeOfDRAMType},
		{"NodeOfPMEMType", sysfs.NodeOfPMEMType, system.NodeOfPMEMType},
		{"NodeOfHBMType", sysfs.NodeOfHBMType, system.NodeOfHBMType},
		{"NodeHasMemory", sysfs.NodeHasMemory, system.NodeHasMemory},
		{"NodeHasNoMemory", sysfs.NodeHasNoMemory, system.NodeHasNoMemory},
		{"NodeHasLocalCPUs", sysfs.NodeHasLocalCPUs, system.NodeHasLocalCPUs},
		{"NodeHasNoLocalCPUs", sysfs.NodeHasNoLocalCPUs, system.NodeHasNoLocalCPUs},
	}

	ids := p.old.NodeIDs()

	for _, f := range filters {
		eqIDSet(t, "FilterNodes("+f.name+")",
			p.old.FilterNodes(ids, f.old), p.new.FilterNodes(ids, f.new))

		for _, id := range ids {
			eqBool(t, "FilterNode("+itoa(id)+", "+f.name+")",
				p.old.FilterNode(id, f.old), p.new.FilterNode(id, f.new))

			oNodes, oDist := p.old.ClosestNodes(id, f.old)
			nNodes, nDist := p.new.ClosestNodes(id, f.new)
			eqInts(t, "ClosestNodes("+itoa(id)+", "+f.name+") distances", oDist, nDist)
			eqIDSets(t, "ClosestNodes("+itoa(id)+", "+f.name+")", oNodes, nNodes)
		}
	}

	// no filters at all, and a combination, as pools.go uses them
	eqIDSet(t, "FilterNodes()", p.old.FilterNodes(ids), p.new.FilterNodes(ids))
	eqIDSet(t, "FilterNodes(PMEM+HasMemory+NoLocalCPUs)",
		p.old.FilterNodes(ids, sysfs.NodeOfPMEMType, sysfs.NodeHasMemory,
			sysfs.NodeHasNoLocalCPUs),
		p.new.FilterNodes(ids, system.NodeOfPMEMType, system.NodeHasMemory,
			system.NodeHasNoLocalCPUs))

	// a node which is not there
	eqBool(t, "FilterNode(absent)",
		p.old.FilterNode(1<<20), p.new.FilterNode(1<<20))
	oNodes, oDist := p.old.ClosestNodes(1 << 20)
	nNodes, nDist := p.new.ClosestNodes(1 << 20)
	eqInts(t, "ClosestNodes(absent) distances", oDist, nDist)
	eqIDSets(t, "ClosestNodes(absent)", oNodes, nNodes)
}

func eqNodeHints(t *testing.T, p pair) {
	t.Helper()

	hints := []string{"", "0", "0-1", "1", itoa(1 << 20), "0," + itoa(1<<20), "bad"}
	for _, hint := range hints {
		eqStr(t, "NodeHintToCPUs("+hint+")",
			p.old.NodeHintToCPUs(hint), p.new.NodeHintToCPUs(hint))
	}
}

// TestEquivalentAbsentHardware checks that both implementations answer, rather
// than panicking, when asked about hardware the machine does not have.
//
// pkg/sysfs used to return a nil pointer inside a non-nil interface here, so its
// callers' nil checks were dead and the call after them panicked. That is fixed,
// so the drop-in has nothing to copy: both return an untyped nil and both
// tolerate an absent CPU in a set.
func TestEquivalentAbsentHardware(t *testing.T) {
	for _, tree := range trees {
		t.Run(tree, func(t *testing.T) {
			p := discoverBoth(t, tree)
			absent := cpuset.New(1<<20, 1<<20+1)

			// the lookups return a nil which compares equal to nil
			if p.old.CPU(1<<20) != nil || p.new.CPU(1<<20) != nil {
				t.Error("CPU(absent) is not nil in one of them")
			}
			if p.old.Node(1<<20) != nil || p.new.Node(1<<20) != nil {
				t.Error("Node(absent) is not nil in one of them")
			}
			if p.old.Package(1<<20) != nil || p.new.Package(1<<20) != nil {
				t.Error("Package(absent) is not nil in one of them")
			}

			// the set helpers tolerate a CPU which is not there
			eqCPUSet(t, "SingleThreadForCPUs(absent)",
				p.old.SingleThreadForCPUs(absent),
				p.new.SingleThreadForCPUs(absent))
			eqCPUSet(t, "AllThreadsForCPUs(absent)",
				p.old.AllThreadsForCPUs(absent), p.new.AllThreadsForCPUs(absent))
			eqCPUSet(t, "AllCPUsSharingNthLevelCacheWithCPUs(2, absent)",
				p.old.AllCPUsSharingNthLevelCacheWithCPUs(2, absent),
				p.new.AllCPUsSharingNthLevelCacheWithCPUs(2, absent))
			eqIDSet(t, "IDSetForCPUs(absent)",
				p.old.IDSetForCPUs(absent, func(c sysfs.CPU) idset.ID { return c.ID() }),
				p.new.IDSetForCPUs(absent, func(c system.CPU) idset.ID { return c.ID() }))

			// and so does a set mixing absent CPUs with real ones
			mixed := p.old.OnlineCPUs().Union(absent)
			eqCPUSet(t, "SingleThreadForCPUs(mixed)",
				p.old.SingleThreadForCPUs(mixed), p.new.SingleThreadForCPUs(mixed))

			// an unknown node has no distance rather than a panic
			eqInt(t, "NodeDistance(absent, 0)",
				p.old.NodeDistance(1<<20, 0), p.new.NodeDistance(1<<20, 0))
			if got := p.new.NodeDistance(1<<20, 0); got != -1 {
				t.Errorf("NodeDistance(absent, 0) = %d, want -1", got)
			}
		})
	}
}

// TestEquivalentSetSysRoot checks that the package-global sys root behaves as it
// does in pkg/sysfs, including the cases SetSysRoot special-cases.
func TestEquivalentSetSysRoot(t *testing.T) {
	// Both keep this in a package global, so discovering through it and through
	// DiscoverSystemAt has to agree.
	root, err := filepath.Abs(filepath.Join("testdata", trees[0]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sys")); err != nil {
		t.Skipf("recorded tree %s is not unpacked: run ./test-setup.sh", trees[0])
	}

	sysfs.SetSysRoot(root)
	system.SetSysRoot(root)
	t.Cleanup(func() {
		sysfs.SetSysRoot("")
		system.SetSysRoot("")
	})

	old, err := sysfs.DiscoverSystem()
	if err != nil {
		t.Fatalf("pkg/sysfs: %v", err)
	}
	new, err := system.DiscoverSystem()
	if err != nil {
		t.Fatalf("the drop-in: %v", err)
	}

	eqCPUSet(t, "CPUSet via SetSysRoot", old.CPUSet(), new.CPUSet())
	eqIDs(t, "NodeIDs via SetSysRoot", old.NodeIDs(), new.NodeIDs())
}

// TestDiscoverSystemAtRejectsNonSysfs checks the one place the drop-in is
// stricter than pkg/sysfs, deliberately: it needs the host root above the mount
// point, so it has to recognise the mount point to find it.
func TestDiscoverSystemAtRejectsNonSysfs(t *testing.T) {
	if _, err := system.DiscoverSystemAt("testdata/sample1"); err == nil {
		t.Error("a path which is not a sysfs mount point was accepted")
	}
}

//
// comparison helpers
//
// Each says which method disagreed and how, since a bare "not equal" in a matrix
// this size is not enough to act on.
//

func eqCPUSet(t *testing.T, what string, old, new cpuset.CPUSet) {
	t.Helper()
	if !old.Equals(new) {
		t.Errorf("%s: pkg/sysfs %s, drop-in %s", what, old, new)
	}
}

func eqInt(t *testing.T, what string, old, new int) {
	t.Helper()
	if old != new {
		t.Errorf("%s: pkg/sysfs %d, drop-in %d", what, old, new)
	}
}

func eqBool(t *testing.T, what string, old, new bool) {
	t.Helper()
	if old != new {
		t.Errorf("%s: pkg/sysfs %v, drop-in %v", what, old, new)
	}
}

func eqStr(t *testing.T, what string, old, new string) {
	t.Helper()
	if old != new {
		t.Errorf("%s: pkg/sysfs %q, drop-in %q", what, old, new)
	}
}

func eqIDs(t *testing.T, what string, old, new []idset.ID) {
	t.Helper()
	// both are documented as sorted, so compare them as they come
	if !slices.Equal(old, new) {
		t.Errorf("%s: pkg/sysfs %v, drop-in %v", what, old, new)
	}
}

func eqInts(t *testing.T, what string, old, new []int) {
	t.Helper()
	if !slices.Equal(old, new) {
		t.Errorf("%s: pkg/sysfs %v, drop-in %v", what, old, new)
	}
}

func eqIDSet(t *testing.T, what string, old, new idset.IDSet) {
	t.Helper()
	if !slices.Equal(old.SortedMembers(), new.SortedMembers()) {
		t.Errorf("%s: pkg/sysfs %v, drop-in %v",
			what, old.SortedMembers(), new.SortedMembers())
	}
}

func eqIDSets(t *testing.T, what string, old, new []idset.IDSet) {
	t.Helper()
	if len(old) != len(new) {
		t.Errorf("%s: pkg/sysfs %d groups, drop-in %d", what, len(old), len(new))
		return
	}
	for i := range old {
		eqIDSet(t, what+" group "+itoa(i), old[i], new[i])
	}
}

func eqCache(t *testing.T, what string, old *sysfs.Cache, new *system.Cache) {
	t.Helper()

	if (old == nil) != (new == nil) {
		t.Errorf("%s: pkg/sysfs nil=%v, drop-in nil=%v", what, old == nil, new == nil)
		return
	}
	if old == nil {
		return
	}

	eqInt(t, what+" ID", old.ID(), new.ID())
	eqInt(t, what+" Level", old.Level(), new.Level())
	eqStr(t, what+" Type", old.Type().String(), new.Type().String())
	if old.Size() != new.Size() {
		t.Errorf("%s Size: pkg/sysfs %d, drop-in %d", what, old.Size(), new.Size())
	}
	eqCPUSet(t, what+" SharedCPUSet", old.SharedCPUSet(), new.SharedCPUSet())
}

func eqCaches(t *testing.T, what string, old []*sysfs.Cache, new []*system.Cache) {
	t.Helper()

	if len(old) != len(new) {
		t.Errorf("%s: pkg/sysfs %d caches, drop-in %d", what, len(old), len(new))
		return
	}
	for i := range old {
		eqCache(t, what+"["+itoa(i)+"]", old[i], new[i])
	}
}

// sampleSets returns the CPU sets to exercise the set-to-set methods with:
// nothing, one CPU, one core, one node, one package, everything, and a set which
// deliberately cuts across cores and caches.
func sampleSets(p pair) []cpuset.CPUSet {
	sets := []cpuset.CPUSet{cpuset.New(), p.old.CPUSet(), p.old.OnlineCPUs()}

	ids := p.old.OnlineCPUs().List()
	if len(ids) == 0 {
		return sets
	}

	sets = append(sets, cpuset.New(ids[0]))
	sets = append(sets, p.old.CPU(ids[0]).ThreadCPUSet())

	if nodes := p.old.NodeIDs(); len(nodes) > 0 {
		sets = append(sets, p.old.Node(nodes[0]).CPUSet())
	}
	if pkgs := p.old.PackageIDs(); len(pkgs) > 0 {
		sets = append(sets, p.old.Package(pkgs[0]).CPUSet())
	}

	ragged := []int{}
	for i := 0; i < len(ids); i += 2 {
		ragged = append(ragged, ids[i])
	}
	sets = append(sets, cpuset.New(ragged...))

	return sets
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
