// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package cpuallocator

import (
	"fmt"
	"slices"
	"sort"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils"
	"github.com/intel/goresctrl/pkg/sst"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// AllocFlag represents CPU allocation preferences.
type AllocFlag uint

const (
	// AllocIdlePackages requests allocation of full idle packages.
	AllocIdlePackages AllocFlag = 1 << iota
	// AllocIdleClusters requests allocation of full idle CPU clusters.
	AllocIdleClusters
	// AllocCacheGroups requests allocation and splitting of idle and used cache groups
	AllocCacheGroups
	// AllocIdleCores requests allocation of full idle cores (all threads in core).
	AllocIdleCores

	// AllocDefault is the default allocation preferences.
	AllocDefault = AllocIdlePackages | AllocIdleClusters | AllocCacheGroups | AllocIdleCores

	logSource = "cpuallocator"
)

// allocatorHelper encapsulates state for allocating CPUs.
type allocatorHelper struct {
	logger.Logger               // allocatorHelper logger instance
	sys           sysfs.System  // sysfs CPU and topology information
	topology      topologyCache // cached topology information
	flags         AllocFlag     // allocation preferences
	from          cpuset.CPUSet // set of CPUs to allocate from
	prefer        CPUPriority   // CPU priority to prefer
	cnt           int           // number of CPUs to allocate
	result        cpuset.CPUSet // set of CPUs allocated
}

// CPUAllocator is an interface for a generic CPU allocator
type CPUAllocator interface {
	AllocateCpus(from *cpuset.CPUSet, cnt int, options ...Option) (cpuset.CPUSet, error)
	ReleaseCpus(from *cpuset.CPUSet, cnt int, options ...Option) (cpuset.CPUSet, error)
	GetCPUPriorities() map[CPUPriority]cpuset.CPUSet
}

type CPUPriority int

const (
	PriorityHigh CPUPriority = iota
	PriorityNormal
	PriorityLow
	NumCPUPriorities
	PriorityNone = NumCPUPriorities
)

// Option is an option for a CPU allocation or release.
type Option func(*allocatorHelper) error

func (p CPUPriority) Option() Option {
	return WithPriority(p)
}

// WithPriority sets the preferred CPU priority for the allocation.
func WithPriority(p CPUPriority) Option {
	return func(a *allocatorHelper) error {
		a.prefer = p
		return nil
	}
}

// WithAllocFlags sets the allocation flags for the allocation.
func WithAllocFlags(flags AllocFlag) Option {
	return func(a *allocatorHelper) error {
		a.flags = flags
		return nil
	}
}

type cpuAllocator struct {
	logger.Logger
	sys           sysfs.System  // wrapped sysfs.System instance
	topologyCache topologyCache // topology lookups
}

// topologyCache caches topology lookups
type topologyCache struct {
	pkg  map[idset.ID]cpuset.CPUSet
	node map[idset.ID]cpuset.CPUSet
	core map[idset.ID]cpuset.CPUSet
	kind map[sysfs.CoreKind]cpuset.CPUSet

	cpuPriorities cpuPriorities // CPU priority mapping
	clusters      []*cpuCluster // CPU clusters
	cacheGroups   []*cacheGroup // CPU cache groups
}

type cpuPriorities [NumCPUPriorities]cpuset.CPUSet

type cpuCluster struct {
	pkg     idset.ID
	die     idset.ID
	cluster idset.ID
	cpus    cpuset.CPUSet
	kind    sysfs.CoreKind
}

type cacheGroup struct {
	id   int
	pkg  idset.ID
	die  idset.ID
	node idset.ID
	cpus cpuset.CPUSet
	kind sysfs.CoreKind
}

// IDFilter helps filtering Ids.
type IDFilter func(idset.ID) bool

// IDSorter helps sorting Ids.
type IDSorter func(int, int) bool

// our logger instance
var log = logger.NewLogger(logSource)

// NewCPUAllocator return a new cpuAllocator instance
func NewCPUAllocator(sys sysfs.System) CPUAllocator {
	ca := cpuAllocator{
		Logger:        log,
		sys:           sys,
		topologyCache: newTopologyCache(sys),
	}

	return &ca
}

// Pick packages, nodes or CPUs by filtering according to a function.
func pickIds(idSlice []idset.ID, f IDFilter) []idset.ID {
	ids := make([]idset.ID, len(idSlice))

	idx := 0
	for _, id := range idSlice {
		if f == nil || f(id) {
			ids[idx] = id
			idx++
		}
	}

	return ids[0:idx]
}

// newAllocatorHelper creates a new CPU allocatorHelper.
func newAllocatorHelper(sys sysfs.System, topo topologyCache) *allocatorHelper {
	a := &allocatorHelper{
		Logger:   log,
		sys:      sys,
		topology: topo,
		flags:    AllocDefault,
	}

	return a
}

// Allocate full idle CPU packages.
func (a *allocatorHelper) takeIdlePackages() {
	a.Debug("* takeIdlePackages()...")

	offline := a.sys.Offlined()

	// pick idle packages
	pkgs := pickIds(a.sys.PackageIDs(),
		func(id idset.ID) bool {
			// Consider a package idle if all online preferred CPUs are idle.
			// In particular, on hybrid core architectures exclude
			//   - exclude E-cores from allocations with <= PriorityNormal preference
			//   - exclude P-cores from allocations with  > PriorityLow preferences
			cset := a.topology.pkg[id].Difference(offline)
			if a.prefer < NumCPUPriorities {
				cset = cset.Intersection(a.topology.cpuPriorities[a.prefer])
			}
			return cset.Intersection(a.from).Equals(cset)
		})

	// sorted by number of preferred cpus and then by cpu id
	sort.Slice(pkgs,
		func(i, j int) bool {
			if res := a.topology.cpuPriorities.cmpCPUSet(a.topology.pkg[pkgs[i]], a.topology.pkg[pkgs[j]], a.prefer, -1); res != 0 {
				return res > 0
			}
			return pkgs[i] < pkgs[j]
		})

	a.Debug(" => idle packages sorted by preference: %v", pkgs)

	// take as many idle packages as we need/can
	for _, id := range pkgs {
		cset := a.topology.pkg[id].Difference(offline)
		if a.prefer < NumCPUPriorities {
			cset = cset.Intersection(a.topology.cpuPriorities[a.prefer])
		}
		a.Debug(" => considering package %v (#%s)...", id, cset)
		if a.cnt >= cset.Size() {
			a.Debug(" => taking package %v...", id)
			a.result = a.result.Union(cset)
			a.from = a.from.Difference(cset)
			a.cnt -= cset.Size()

			if a.cnt == 0 {
				break
			}
		}
	}
}

var (
	emptyCPUSet = cpuset.New()
)

// Allocate full idle CPU clusters.
func (a *allocatorHelper) takeIdleClusters() {
	var (
		offline  = a.sys.OfflineCPUs()
		pickIdle = func(c *cpuCluster) (bool, cpuset.CPUSet) {
			if len(a.topology.kind) > 1 {
				// we only take E-clusters for low-prio requests
				if a.prefer != PriorityLow && c.kind == sysfs.EfficientCore {
					a.Debug("  - omit %s, CPU preference is %s", c, a.prefer)
					return false, emptyCPUSet
				}
				// we only take P-clusters for other than low-prio requests
				if a.prefer == PriorityLow && c.kind == sysfs.PerformanceCore {
					a.Debug("  - omit %s, CPU preference is %s", c, a.prefer)
					return false, emptyCPUSet
				}
			}

			// we only take fully idle clusters
			cset := c.cpus.Difference(offline)
			free := cset.Intersection(a.from)
			if free.IsEmpty() || !free.Equals(cset) {
				a.Debug("  - omit %s, %d usable CPUs (%s)", c, free.Size(), free)
				return false, emptyCPUSet
			}

			a.Debug("  + pick %s, %d usable CPUs (%s)", c, free.Size(), free)
			return true, free
		}
		preferTightestFit = func(cA, cB *cpuCluster, pkgA, pkgB, dieA, dieB int, csetA, csetB cpuset.CPUSet) (r int) {
			defer func() {
				if r < 0 {
					a.Debug("  + prefer %s", cA)
					a.Debug("      over %s", cB)
				}
				if r > 0 {
					a.Debug("  + prefer %s", cB)
					a.Debug("      over %s", cA)
				}
				a.Debug("  - misfit %s", cA)
				a.Debug("       and %s", cB)
			}()

			// prefer cluster which alone can satisfy the request, preferring tighter
			cntA, cntB := csetA.Size(), csetB.Size()
			if cntA >= a.cnt && cntB < a.cnt {
				return -1
			}
			if cntA < a.cnt && cntB >= a.cnt {
				return 1
			}
			if cntA >= a.cnt && cntB >= a.cnt {
				if diff := cntA - cntB; diff != 0 {
					return diff
				}
				// do stable sort: prefer smaller package, die, and cluster IDs
				if cA.pkg != cB.pkg {
					return cA.pkg - cB.pkg
				}
				if cA.die != cB.die {
					return cA.die - cB.die
				}
				return cA.cluster - cB.cluster
			}

			// prefer die which alone can satisfy the request, preferring tighter
			if dieA >= a.cnt && dieB < a.cnt {
				return -1
			}
			if dieA < a.cnt && dieB >= a.cnt {
				return 1
			}
			if dieA >= a.cnt && dieB >= a.cnt {
				if diff := dieA - dieB; diff != 0 {
					return diff
				}
				// do stable sort: prefer smaller package, die, and cluster IDs
				if cA.pkg != cB.pkg {
					return cA.pkg - cB.pkg
				}
				if cA.die != cB.die {
					return cA.die - cB.die
				}
				return cA.cluster - cB.cluster
			}

			// prefer package which alone can satisfy the request, preferring tighter
			if pkgA >= a.cnt && pkgB < a.cnt {
				return -1
			}
			if pkgA < a.cnt && pkgB >= a.cnt {
				return 1
			}
			if pkgA >= a.cnt && pkgB >= a.cnt {
				if diff := pkgA - pkgB; diff != 0 {
					return diff
				}
				// do stable sort: prefer smaller package, die, and cluster IDs
				if cA.pkg != cB.pkg {
					return cA.pkg - cB.pkg
				}
				if cA.die != cB.die {
					return cA.die - cB.die
				}
				return cA.cluster - cB.cluster
			}

			// both unusable (don't need stable sort, we won't use them anyway)
			return 0
		}

		sorter = &clusterSorter{
			pick: pickIdle,
			sort: preferTightestFit,
		}
	)

	a.Debug("* takeIdleClusters()...")

	if len(a.topology.clusters) <= 1 {
		return
	}

	a.Debug("looking for %d %s CPUs from %s", a.cnt, a.prefer, a.from)

	a.sortCPUClusters(sorter)

	var (
		clusters  = sorter.clusters
		pkgCPUCnt = sorter.pkgCPUCnt
		cpus      = sorter.cpus
	)

	if len(clusters) < 1 {
		return
	}

	// tightest-fit cluster is a perfect fit, use it
	c := clusters[0]
	cset := cpus[c]
	if cset.Size() == a.cnt {
		log.Debug("=> picking single %s", c)
		a.result = a.result.Union(cset)
		a.from = a.from.Difference(cset)
		a.cnt -= cset.Size()
		return
	}

	// tightest-fit cluster is too big, so allocation can't consume any cluster fully
	if cset.Size() > a.cnt {
		log.Debug(" => tightest-fit cluster too big, can't consume a full cluster")
		return
	}

	// bail out if no package can satisfy the allocation
	if cnt := pkgCPUCnt[c.pkg]; cnt < a.cnt {
		log.Debug(" => no package can satisfy the allocation, bail out")
	}

	// start consuming clusters, until we're done
	for i, c := range clusters {
		cset := cpus[c]

		if a.cnt < cset.Size() {
			log.Debug("=> %d more CPUs needed after allocation of %d clusters", a.cnt, i)
			// XXX TODO: should restrict a.from to the same package, if that has enough
			// CPUs to satisfy the request
			return
		}

		log.Debug("=> picking %d. %s", i, c)

		if a.cnt >= cset.Size() {
			a.result = a.result.Union(cset)
			a.from = a.from.Difference(cset)
			a.cnt -= cset.Size()
		}

		if a.cnt == 0 {
			return
		}
	}
}

// Allocate idle or partial CPU last-level cache groups.
func (a *allocatorHelper) takeCacheGroups() {
	log.Debug("* takeCacheGroups()...")

	if len(a.topology.cacheGroups) <= 1 {
		return
	}

	if a.cnt < 2 {
		// TODO(klihub): we could also decide based on some criteria, perhaps some
		// caller preference, if it was better to handle such containers here and,
		// for instance, allocate them using already fragmented groups
		return
	}

	//
	// The allocation strategy here is roughly the following:
	//
	// 1. collect cache group candidates:
	//    a. ignore cache groups with conflicting allocation prio
	//    b. pick idle cache groups as 'preferred'
	//    c. pick other cache groups with some free CPUs left as 'usable'
	// 2. sort preferred cache groups: prefer tightest fitting package and die
	// 3. sort usable cache groups:
	//    a. prefer same package and die as the best preferred group, if we have any
	//    b. otherwise prefer looser groups from tightest fitting package and die
	// 4. bail out if no single package can satisfy the request
	// 5. allocate preferred groups
	//    a. take as many full groups as we can
	//    b. split up a preferred group if we have to
	// 6. allocate usable groups
	//    a. try allocating a single group with exactly matching size (IOW free CPUs)
	//    b. try allocating the smallest number of groups of a single size
	//    c. allocate using the smallest number of groups (largest to smallest)
	//
	// Notes:
	//   We might consider letting the requester control some aspects of allocation,
	//   for instance:
	//     - use only full idle groups (ideal maximum isolation)
	//     - use only full idle groups, and 1 fragmented (maximum isolation)
	//     - try using only fragmented groups (for lesser workloads, to preserve idle groups)
	//       o take fewest groups possible (take from large to small, minimize interference)
	//       o fragment fewest groups possible (take from small to large, preserve large groups)

	var (
		offline    = a.sys.OfflineCPUs()
		pickGroups = func(g *cacheGroup) (pickVerdict, cpuset.CPUSet) {
			if len(a.topology.kind) > 1 {
				// only take E-groups for low-prio requests, or if we have none other
				if a.prefer != PriorityLow && g.kind == sysfs.EfficientCore {
					log.Debug("  - ignore %s (CPU preference is %s)", g, a.prefer)
					return pickIgnore, emptyCPUSet
				}
				// only take P-groups for other than low-prio requests, or if we have none other
				if a.prefer == PriorityLow && g.kind == sysfs.PerformanceCore {
					log.Debug("  - ignore %s (CPU preference is %s)", g, a.prefer)
					return pickIgnore, emptyCPUSet
				}
			}

			cset := g.cpus.Difference(offline)
			free := cset.Intersection(a.from)

			// ignore groups without usable CPUs
			if free.IsEmpty() {
				log.Debug("  - ignore %s (no usable CPUs)", g)
				return pickIgnore, emptyCPUSet
			}

			// prefer fully usable idle groups
			if free.Equals(cset) {
				log.Debug("  + prefer %s (%d CPUs: %s)", g, free.Size(), free)
				return pickPrefer, free
			}

			// take also groups with some usable CPUs left
			log.Debug("  o usable %s (%d free CPUs: %s)", g, free.Size(), free)
			return pickUsable, free
		}

		sortIdle = func(gA, gB *cacheGroup, s *cacheGroupSorter) (r int) {
			defer func() {
				switch {
				case r < 0:
					log.Debug("  + prefer %s", gA)
					log.Debug("      over %s", gB)
				case r > 0:
					log.Debug("  + prefer %s", gB)
					log.Debug("      over %s", gA)
				default: // currently should not happen
					log.Debug("  - either %s", gA)
					log.Debug("        or %s", gB)
				}
			}()

			dieFullA := s.preferDieCPUCount(gA.pkg, gA.die)
			dieFullB := s.preferDieCPUCount(gB.pkg, gB.die)
			pkgFullA := s.preferPkgCPUCount(gA.pkg)
			pkgFullB := s.preferPkgCPUCount(gB.pkg)

			diePartA := s.usableDieCPUCount(gA.pkg, gA.die)
			diePartB := s.usableDieCPUCount(gB.pkg, gB.die)
			pkgPartA := s.usablePkgCPUCount(gA.pkg)
			pkgPartB := s.usablePkgCPUCount(gB.pkg)

			full, part := s.full, s.part

			// if only one die can satisfy the request, prefer that one
			if dieFullA >= full && dieFullB < full {
				if diePartA >= part {
					return -1
				}
			}
			if dieFullA < full && dieFullB >= full {
				if diePartB >= part {
					return 1
				}
			}
			if dieFullA >= full && dieFullB >= full {
				if diePartA >= part && diePartB < part {
					return -1
				}
				if diePartA < part && diePartB >= part {
					return 1
				}
			}

			// if both dies can satisfy the request, prefer tighter one
			if dieFullA >= full && dieFullB >= full {
				if diePartA >= part && diePartB >= part {
					if diff := dieFullA - dieFullB; diff != 0 {
						return diff
					}
					if diff := diePartA - diePartB; diff != 0 {
						return diff
					}
					// for a tie prefer smaller package, die, and group IDs
					if gA.pkg != gB.pkg {
						return gA.pkg - gB.pkg
					}
					if gA.die != gB.die {
						return gA.die - gB.die
					}
					return gA.id - gB.id
				}
			}

			// if only one package can satisfy the request, prefer that one
			if pkgFullA >= full && pkgFullB < full {
				if pkgPartA >= part {
					return -1
				}
			}
			if pkgFullA < full && pkgFullB >= full {
				if pkgPartB >= part {
					return 1
				}
			}
			if pkgFullA >= full && pkgFullB >= full {
				if pkgPartA >= part && pkgPartB < part {
					return -1
				}
				if pkgPartA < part && pkgPartB >= part {
					return 1
				}
			}

			// if both packages can satisfy the request, prefer tighter one
			if pkgFullA >= full && pkgFullB >= full {
				if pkgPartA >= part && pkgPartB >= part {
					if diff := pkgFullA - pkgFullB; diff != 0 {
						return diff
					}
					if diff := pkgPartA - pkgPartB; diff != 0 {
						return diff
					}
					// for a tie prefer smaller package, die, and group IDs
					if gA.pkg != gB.pkg {
						return gA.pkg - gB.pkg
					}
					if gA.die != gB.die {
						return gA.die - gB.die
					}
					return gA.id - gB.id
				}
			}

			// equality: sort by group ID.
			return gA.id - gB.id
		}

		sortUsed = func(gA, gB *cacheGroup, s *cacheGroupSorter) (r int) {
			defer func() {
				switch {
				case r < 0:
					log.Debug("  + prefer %s", gA)
					log.Debug("      over %s", gB)
				case r > 0:
					log.Debug("  + prefer %s", gB)
					log.Debug("      over %s", gA)
				default:
					log.Debug("  - either %s", gA)
					log.Debug("        or %s", gB)
				}
			}()

			var (
				diePartA = s.usableDieCPUCount(gA.pkg, gA.die)
				diePartB = s.usableDieCPUCount(gB.pkg, gB.die)
				csetA    = s.cpus[gA]
				csetB    = s.cpus[gB]
				full     = s.full
				part     = s.part
				idle     *cacheGroup
			)

			if len(s.prefer) > 0 {
				idle = s.prefer[0]
			}

			// if we are going to use idle groups prefer other groups from the same die and pkg
			if full > 0 && idle != nil {
				dieIdle := s.preferDieCPUCount(idle.pkg, idle.die)
				pkgIdle := s.preferPkgCPUCount(idle.pkg)

				if gA.pkg == pkgIdle && gB.pkg != pkgIdle {
					return -1
				}
				if gA.pkg != pkgIdle && gB.pkg == pkgIdle {
					return 1
				}
				if gA.pkg == pkgIdle && gB.pkg == pkgIdle {
					if gA.die == dieIdle && gB.die != dieIdle {
						return -1
					}
					if gA.die != dieIdle && gB.die == dieIdle {
						return 1
					}
					// for a tie prefer looser (bigger) group, smaller group ID
					if gA.die == dieIdle && gB.die == dieIdle {
						if diff := csetA.Size() - csetB.Size(); diff != 0 {
							return -diff
						}
						return gA.id - gB.id
					}
				}
				// equality: both are unusable, don't need to sort them
				return 0
			}

			// if we only have used groups, prefer tighter satisfying package and die
			total := full + part

			if diePartA >= total && diePartB < total {
				return -1
			}
			if diePartA < total && diePartB >= total {
				return 1
			}
			if diePartA >= total && diePartB >= total {
				if diff := diePartA - diePartB; diff != 0 {
					return diff
				}
				// for a tie prefer looser (bigger) group, smaller package, die, and group IDs
				if diff := csetA.Size() - csetB.Size(); diff != 0 {
					return -diff
				}
				if gA.pkg != gB.pkg {
					return gA.pkg - gB.pkg
				}
				if gA.die != gB.die {
					return gA.die - gB.die
				}
				return gA.id - gB.id
			}

			// equality: both are unusable, don't need to sort them
			return 0
		}

		sorter = &cacheGroupSorter{
			pick:       pickGroups,
			sortPrefer: sortIdle,
			sortUsable: sortUsed,
		}
	)

	log.Debug("looking for %d CPUs (prio %s) from %s", a.cnt, a.prefer, a.from)

	sorter.sortCacheGroups(a)

	var (
		preferPkgCPUs int
		usablePkgCPUs int
		chosenPkg     int

		result = a.result
		from   = a.from
		cnt    = a.cnt
	)

	switch {
	case len(sorter.prefer) > 0:
		chosenPkg = sorter.prefer[0].pkg
		preferPkgCPUs = sorter.preferPkgCPUCount(chosenPkg)
		usablePkgCPUs = sorter.usablePkgCPUCount(chosenPkg)
	case len(sorter.usable) > 0:
		chosenPkg = sorter.usable[0].pkg
		usablePkgCPUs = sorter.usablePkgCPUCount(chosenPkg)
	}

	if preferPkgCPUs+usablePkgCPUs < a.cnt {
		log.Debug("=> no package can satisfy the allocation")
		return
	}

	//
	// take full idle cache groups, splitting up the last one if necessary
	//

	log.Debug("trying to take idle cache groups...")
	for i, g := range sorter.prefer {
		if cnt <= 0 {
			break
		}

		cset := sorter.cpus[g]

		if cnt >= cset.Size() {
			log.Debug("=> took full idle cache group %d. %s", i, g)

			result = result.Union(cset)
			from = from.Difference(cset)
			cnt -= cset.Size()
			continue
		}

		// partially allocate the rest from this group
		ta := newAllocatorHelper(a.sys, a.topology)
		ta.prefer = a.prefer
		ta.flags = AllocIdleCores
		ta.from = cset
		ta.cnt = cnt
		use := ta.allocate()

		log.Debug("=> took %d CPUs (%s) from idle cache group %d. %s", use.Size(), use, i, g)

		result = result.Union(use)
		from = from.Difference(use)
		cnt -= use.Size()
	}

	if cnt <= 0 {
		a.result = result
		a.from = from
		a.cnt = cnt
		return
	}

	//
	// allocate non-idle usable cache groups
	//
	// We try a few strategies to fulfill the allocation in this order:
	//   1. try to find a single group with the exact number of CPUs
	//   2. try to find the minimal number of same-sized groups
	//   3. fulfill request by taking groups in decreasing size order
	//

	log.Debug("%d more CPUs needed", cnt)

	var (
		groupsBySize = map[int][]*cacheGroup{}
		totalByIndex = make([]int, 0, len(sorter.usable))
		totalCPUs    = 0
	)

	for i := 0; i <= len(sorter.usable)-1; i++ {
		g := sorter.usable[i]
		cset := sorter.cpus[g]

		// don't ever cross package boundary, ignore the rest of the groups
		if g.pkg != chosenPkg {
			break
		}

		groupsBySize[cset.Size()] = append(groupsBySize[cset.Size()], g)
		totalCPUs += cset.Size()
		totalByIndex = append(totalByIndex, totalCPUs)
	}

	if totalCPUs < cnt {
		log.Debug("=> internal error: total cache group CPUs %d <= expected %d", totalCPUs, cnt)
	}

	// try to pick a single exact sized group if possible
	log.Debug("trying to find a single cache group with %d CPUs...", cnt)

	if groups, ok := groupsBySize[cnt]; ok {
		g := groups[0]
		cset := sorter.cpus[g]

		log.Debug("=> took remaining %d CPUs (%s) of usable cache group %s", cnt, cset, g)

		result = result.Union(cset)
		from = from.Difference(cset)

		if cset.Size() != cnt {
			log.Error("=> internal error: group size by cnt %d != expected %d", cset.Size(), cnt)
			return
		}

		a.result = result
		a.from = from
		a.cnt = 0
		return
	}

	// try picking the smallest number of groups of a single size
	log.Debug("trying to find cache groups of a single size for %d more CPUs...", cnt)

	size := 0
	take := 0
	for grpSize, groups := range groupsBySize {
		if grpSize > 0 && grpSize < cnt && cnt%grpSize == 0 {
			if n := cnt / grpSize; n < len(groups) && n < take {
				size = grpSize
				take = n
			}
		}
	}

	if take != 0 && size > 1 { // don't take (so easily) more than one single CPU groups
		for i, g := range groupsBySize[size] {
			cset := sorter.cpus[g]

			log.Debug("=> took %d./%d %d remaining CPUs (%s) of usable cache group of size %d %s",
				i+1, take, cset.Size(), cset, size, g)

			result = result.Union(cset)
			from = from.Difference(cset)
			cnt -= cset.Size()
		}

		if cnt != 0 {
			log.Error("internal error: remaining cnt %d, expected 0", cnt)
			return
		}

		a.result = result
		a.from = from
		a.cnt = 0
	}

	// use up smallest number of groups possible (start with the largest group)
	log.Debug("=> taking cache groups in decreasing size order for %d more CPUs...", cnt)

	var (
		grpCnt = 0
		cpuCnt = 0
	)

	for i, total := range totalByIndex {
		grpCnt, cpuCnt = i+1, total

		if cnt <= total {
			break
		}
	}

	if cpuCnt < cnt {
		log.Debug("=> internal error: %d CPUs in usable cache groups < needed %d", cpuCnt, cnt)
		return
	}

	for i := 0; i < grpCnt; i++ {
		g := sorter.usable[i]
		cset := sorter.cpus[g]

		if cnt < cset.Size() {
			break
		}

		log.Debug("=> took %d./%d remaining CPUs (%s) of usable cache group %s", i, grpCnt, cset, g)

		result = result.Union(cset)
		from = from.Difference(cset)
		cnt -= cset.Size()
	}

	if cnt > 0 {
		// need to take only part of the last group we use
		g := sorter.usable[grpCnt-1]
		cset := sorter.cpus[g]

		ta := newAllocatorHelper(a.sys, a.topology)
		ta.prefer = a.prefer
		ta.flags = AllocIdleCores
		ta.from = cset
		ta.cnt = cnt
		use := ta.allocate()

		log.Debug("=> took %d./%d %d CPUs (%s) from cache group %s",
			grpCnt, grpCnt, use.Size(), use, g)

		result = result.Union(use)
		from = from.Difference(use)
		cnt -= use.Size()
	}

	if cnt != 0 {
		log.Error("=> internal error: %d unallocated cache group CPUs remain", cnt)
		return
	}

	a.result = result
	a.from = from
	a.cnt = 0
}

// Allocate full idle CPU cores.
func (a *allocatorHelper) takeIdleCores() {
	a.Debug("* takeIdleCores()...")

	offline := a.sys.Offlined()

	// pick (first id for all) idle cores
	cores := pickIds(a.sys.CPUIDs(),
		func(id idset.ID) bool {
			cset := a.topology.core[id].Difference(offline)
			if cset.IsEmpty() {
				return false
			}
			return cset.Intersection(a.from).Equals(cset) && cset.List()[0] == int(id)
		})

	// sorted by id
	sort.Slice(cores,
		func(i, j int) bool {
			if res := a.topology.cpuPriorities.cmpCPUSet(a.topology.core[cores[i]], a.topology.core[cores[j]], a.prefer, -1); res != 0 {
				return res > 0
			}
			return cores[i] < cores[j]
		})

	a.Debug(" => idle cores sorted by preference: %v", cores)

	// take as many idle cores as we can
	for _, id := range cores {
		cset := a.topology.core[id].Difference(offline)
		a.Debug(" => considering core %v (#%s)...", id, cset)
		if a.cnt >= cset.Size() {
			a.Debug(" => taking core %v...", id)
			a.result = a.result.Union(cset)
			a.from = a.from.Difference(cset)
			a.cnt -= cset.Size()

			if a.cnt == 0 {
				break
			}
		}
	}
}

// Allocate idle CPU hyperthreads.
func (a *allocatorHelper) takeIdleThreads() {
	offline := a.sys.Offlined()

	// pick all threads with free capacity
	cores := pickIds(a.sys.CPUIDs(),
		func(id idset.ID) bool {
			return a.from.Difference(offline).Contains(int(id))
		})

	a.Debug(" => idle threads unsorted: %v", cores)

	// sorted for preference by id, mimicking cpus_assignment.go for now:
	//   IOW, prefer CPUs
	//     - from packages with higher number of CPUs/cores already in a.result
	//     - from packages having larger number of available cpus with preferred priority
	//     - from a single package
	//     - from the list of cpus with preferred priority
	//     - from packages with fewer remaining free CPUs/cores in a.from
	//     - from cores with fewer remaining free CPUs/cores in a.from
	//     - from packages with lower id
	//     - with lower id
	sort.Slice(cores,
		func(i, j int) bool {
			iCore := cores[i]
			jCore := cores[j]
			iPkg := a.sys.CPU(iCore).PackageID()
			jPkg := a.sys.CPU(jCore).PackageID()

			iCoreSet := a.topology.core[iCore]
			jCoreSet := a.topology.core[jCore]
			iPkgSet := a.topology.pkg[iPkg]
			jPkgSet := a.topology.pkg[jPkg]

			iPkgColo := iPkgSet.Intersection(a.result).Size()
			jPkgColo := jPkgSet.Intersection(a.result).Size()
			if iPkgColo != jPkgColo {
				return iPkgColo > jPkgColo
			}

			// Always sort cores in package order
			if res := a.topology.cpuPriorities.cmpCPUSet(iPkgSet.Intersection(a.from), jPkgSet.Intersection(a.from), a.prefer, a.cnt); res != 0 {
				return res > 0
			}
			if iPkg != jPkg {
				return iPkg < jPkg
			}

			iCset := cpuset.New(int(cores[i]))
			jCset := cpuset.New(int(cores[j]))
			if res := a.topology.cpuPriorities.cmpCPUSet(iCset, jCset, a.prefer, 0); res != 0 {
				return res > 0
			}

			iPkgFree := iPkgSet.Intersection(a.from).Size()
			jPkgFree := jPkgSet.Intersection(a.from).Size()
			if iPkgFree != jPkgFree {
				return iPkgFree < jPkgFree
			}

			iCoreFree := iCoreSet.Intersection(a.from).Size()
			jCoreFree := jCoreSet.Intersection(a.from).Size()
			if iCoreFree != jCoreFree {
				return iCoreFree < jCoreFree
			}

			return iCore < jCore
		})

	a.Debug(" => idle threads sorted: %v", cores)

	// take as many idle cores as we can
	for _, id := range cores {
		cset := a.topology.core[id].Difference(offline)
		a.Debug(" => considering thread %v (#%s)...", id, cset)
		cset = cpuset.New(int(id))
		a.result = a.result.Union(cset)
		a.from = a.from.Difference(cset)
		a.cnt -= cset.Size()

		if a.cnt == 0 {
			break
		}
	}
}

// takeAny is a dummy allocator not dependent on sysfs topology information
func (a *allocatorHelper) takeAny() {
	a.Debug("* takeAnyCores()...")

	cpus := a.from.List()

	if len(cpus) >= a.cnt {
		cset := cpuset.New(cpus[0:a.cnt]...)
		a.result = a.result.Union(cset)
		a.from = a.from.Difference(cset)
		a.cnt = 0
	}
}

// Perform CPU allocation.
func (a *allocatorHelper) allocate() cpuset.CPUSet {
	if a.sys != nil {
		if (a.flags & AllocIdlePackages) != 0 {
			a.takeIdlePackages()
		}
		if len(a.topology.kind) > 1 {
			if a.cnt > 0 && (a.flags&AllocIdleClusters) != 0 {
				a.takeIdleClusters()
			}
			if a.cnt > 0 && (a.flags&AllocCacheGroups) != 0 {
				a.takeCacheGroups()
			}
		} else {
			if a.cnt > 0 && (a.flags&AllocCacheGroups) != 0 {
				a.takeCacheGroups()
			}
		}
		if a.cnt > 0 && (a.flags&AllocIdleCores) != 0 {
			a.takeIdleCores()
		}
		if a.cnt > 0 {
			a.takeIdleThreads()
		}
	} else {
		a.takeAny()
	}
	if a.cnt == 0 {
		return a.result
	}

	return cpuset.New()
}

type clusterSorter struct {
	// function to pick or ignore a cluster
	pick func(*cpuCluster) (bool, cpuset.CPUSet)
	// function to sort slice of picked clusters
	sort func(a, b *cpuCluster, pkgCntA, pkgCntB, dieCntA, dieCntB int, cpusA, cpusB cpuset.CPUSet) int

	// resulting cluster, available CPU count per package and die, available CPUs per cluster
	clusters  []*cpuCluster
	pkgCPUCnt map[idset.ID]int
	dieCPUCnt map[idset.ID]map[idset.ID]int
	cpus      map[*cpuCluster]cpuset.CPUSet
}

func (a *allocatorHelper) sortCPUClusters(s *clusterSorter) {
	var (
		clusters  = []*cpuCluster{}
		pkgCPUCnt = map[idset.ID]int{}
		dieCPUCnt = map[idset.ID]map[idset.ID]int{}
		cpus      = map[*cpuCluster]cpuset.CPUSet{}
	)

	a.Debug("picking suitable clusters")

	for _, c := range a.topology.clusters {
		var cset cpuset.CPUSet

		// pick or ignore cluster, determine usable cluster CPUs
		if s.pick == nil {
			cset = c.cpus
		} else {
			pick, usable := s.pick(c)
			if !pick || usable.Size() == 0 {
				continue
			}

			cset = usable
		}

		// collect cluster and usable CPUs
		clusters = append(clusters, c)
		cpus[c] = cset

		// count usable CPUs per package and die
		if _, ok := dieCPUCnt[c.pkg]; !ok {
			dieCPUCnt[c.pkg] = map[idset.ID]int{}
		}
		dieCPUCnt[c.pkg][c.die] += cset.Size()
		pkgCPUCnt[c.pkg] += cset.Size()
	}

	if a.DebugEnabled() {
		log.Debug("number of collected usable CPUs:")
		for pkg, cnt := range pkgCPUCnt {
			log.Debug("  - package #%d: %d", pkg, cnt)
		}
		for pkg, dies := range dieCPUCnt {
			for die, cnt := range dies {
				log.Debug("  - die #%d/%d %d", pkg, die, cnt)
			}
		}
	}

	// sort collected clusters
	if s.sort != nil {
		a.Debug("sorting picked clusters")
		slices.SortFunc(clusters, func(cA, cB *cpuCluster) int {
			pkgCPUsA, pkgCPUsB := pkgCPUCnt[cA.pkg], pkgCPUCnt[cB.pkg]
			dieCPUsA, dieCPUsB := dieCPUCnt[cA.pkg][cA.die], dieCPUCnt[cB.pkg][cB.die]
			cpusA, cpusB := cpus[cA], cpus[cB]
			return s.sort(cA, cB, pkgCPUsA, pkgCPUsB, dieCPUsA, dieCPUsB, cpusA, cpusB)
		})
	}

	s.clusters = clusters
	s.pkgCPUCnt = pkgCPUCnt
	s.dieCPUCnt = dieCPUCnt
	s.cpus = cpus
}

type pickVerdict int

const (
	pickPrefer pickVerdict = iota
	pickUsable
	pickIgnore
)

type cacheGroupSorter struct {
	// function to pick preferred and usable cache groups
	pick func(*cacheGroup) (pickVerdict, cpuset.CPUSet)
	// functions for sorting picked cache groups
	sortPrefer func(a, b *cacheGroup, s *cacheGroupSorter) int
	sortUsable func(a, b *cacheGroup, s *cacheGroupSorter) int

	// preferred groups, available CPU count per package and die
	prefer    []*cacheGroup
	preferPkg map[idset.ID]int
	preferDie map[idset.ID]map[idset.ID]int

	// other usable groups, available CPU count per package and die
	usable    []*cacheGroup
	usablePkg map[idset.ID]int
	usableDie map[idset.ID]map[idset.ID]int

	// available CPUs per group
	cpus map[*cacheGroup]cpuset.CPUSet

	// full and partial groups worth of requested CPUs
	full int
	part int
}

func (s *cacheGroupSorter) preferPkgCPUCount(pkg idset.ID) int {
	return s.preferPkg[pkg]
}

func (s *cacheGroupSorter) preferDieCPUCount(pkg, die idset.ID) int {
	return s.preferDie[pkg][die]
}

func (s *cacheGroupSorter) usablePkgCPUCount(pkg idset.ID) int {
	return s.usablePkg[pkg]
}

func (s *cacheGroupSorter) usableDieCPUCount(pkg, die idset.ID) int {
	return s.usableDie[pkg][die]
}

func (s *cacheGroupSorter) CPUSet(g *cacheGroup) cpuset.CPUSet {
	return s.cpus[g]
}

func (s *cacheGroupSorter) sortCacheGroups(a *allocatorHelper) {
	s.prefer = []*cacheGroup{}
	s.preferPkg = map[idset.ID]int{}
	s.preferDie = map[idset.ID]map[idset.ID]int{}
	s.usable = []*cacheGroup{}
	s.usablePkg = map[idset.ID]int{}
	s.usableDie = map[idset.ID]map[idset.ID]int{}
	s.cpus = map[*cacheGroup]cpuset.CPUSet{}

	log.Debug("picking suitable cache groups")

	// Notes:
	//   We blindly assume here that all cache groups of interest are of
	//   the same size and use this assumption to split the request into
	//   full cache size multiples and the remaining partial allocation.

	s.part = a.cnt % a.topology.cacheGroups[0].cpus.Size()
	s.full = a.cnt - s.part

	// collect preferred and usable groups, count their CPUs per package and die
	for _, g := range a.topology.cacheGroups {
		verdict, cset := s.pick(g)
		switch verdict {
		case pickPrefer:
			// collect preferred group and usable CPUs
			s.prefer = append(s.prefer, g)
			s.cpus[g] = cset

			// count preferred usable CPUs per package and die
			if _, ok := s.preferDie[g.pkg]; !ok {
				s.preferDie[g.pkg] = map[idset.ID]int{}
			}
			s.preferDie[g.pkg][g.die] += cset.Size()
			s.preferPkg[g.pkg] += cset.Size()

		case pickUsable:
			// collect non-preferred but usable group and CPUs
			s.usable = append(s.usable, g)
			s.cpus[g] = cset

			// count usable CPUs per package and die
			if _, ok := s.usableDie[g.pkg]; !ok {
				s.usableDie[g.pkg] = map[idset.ID]int{}
			}
			s.usableDie[g.pkg][g.die] += cset.Size()
			s.usablePkg[g.pkg] += cset.Size()

		case pickIgnore:
			continue
		}
	}

	if log.DebugEnabled() {
		if len(s.preferPkg) > 0 {
			log.Debug("number of preferred cache group CPUs per package/die:")
			for pkg, cnt := range s.preferPkg {
				log.Debug("  - package #%d: %d", pkg, cnt)
			}
			for pkg, dies := range s.preferDie {
				for die, cnt := range dies {
					log.Debug("  - die #%d/%d %d", pkg, die, cnt)
				}
			}
		} else {
			log.Debug("no preferred cache groups found")
		}

		if len(s.usablePkg) > 0 {
			log.Debug("number of non-preferred but usable cache group CPUs per package/die:")
			for pkg, cnt := range s.usablePkg {
				log.Debug("  - package #%d: %d", pkg, cnt)
			}
			for pkg, dies := range s.usableDie {
				for die, cnt := range dies {
					log.Debug("  - die #%d/%d %d", pkg, die, cnt)
				}
			}
		} else {
			log.Debug("no non-preferred but usable cache groups found")
		}
	}

	// sort preferred groups
	if len(s.prefer) > 0 {
		log.Debug("sorting preferred cache groups")
		slices.SortFunc(s.prefer, func(gA, gB *cacheGroup) int {
			return s.sortPrefer(gA, gB, s)
		})
	}

	// sort other usable groups
	if len(s.usable) > 0 {
		log.Debug("sorting non-preferred but usable cache groups")
		slices.SortFunc(s.usable, func(gA, gB *cacheGroup) int {
			return s.sortUsable(gA, gB, s)
		})
	}
}

func (ca *cpuAllocator) allocateCpus(from *cpuset.CPUSet, cnt int, options ...Option) (cpuset.CPUSet, error) {
	var result cpuset.CPUSet
	var err error

	switch {
	case from.Size() < cnt:
		result, err = cpuset.New(), fmt.Errorf("cpuset %s does not have %d CPUs", from, cnt)
	case from.Size() == cnt:
		result, err, *from = from.Clone(), nil, cpuset.New()
	default:
		a := newAllocatorHelper(ca.sys, ca.topologyCache)
		for _, o := range options {
			if err := o(a); err != nil {
				return cpuset.New(), err
			}
		}
		a.from = from.Clone()
		a.cnt = cnt

		result, err, *from = a.allocate(), nil, a.from.Clone()

		a.Debug("%d cpus from #%v (preferring #%v) => #%v", cnt, from.Union(result), a.prefer, result)
	}

	return result, err
}

// AllocateCpus allocates a number of CPUs from the given set.
func (ca *cpuAllocator) AllocateCpus(from *cpuset.CPUSet, cnt int, options ...Option) (cpuset.CPUSet, error) {
	result, err := ca.allocateCpus(from, cnt, options...)
	return result, err
}

// ReleaseCpus releases a number of CPUs from the given set.
func (ca *cpuAllocator) ReleaseCpus(from *cpuset.CPUSet, cnt int, options ...Option) (cpuset.CPUSet, error) {
	oset := from.Clone()

	result, err := ca.allocateCpus(from, from.Size()-cnt, options...)

	ca.Debug("ReleaseCpus(#%s, %d) => kept: #%s, released: #%s", oset, cnt, from, result)

	return result, err
}

// GetCPUPriorities returns the CPUSets for the discovered priorities.
func (ca *cpuAllocator) GetCPUPriorities() map[CPUPriority]cpuset.CPUSet {
	prios := make(map[CPUPriority]cpuset.CPUSet)
	for prio := CPUPriority(0); prio < NumCPUPriorities; prio++ {
		cset := ca.topologyCache.cpuPriorities[prio]
		prios[prio] = cset.Clone()
	}
	return prios
}

func newTopologyCache(sys sysfs.System) topologyCache {
	c := topologyCache{
		pkg:  make(map[idset.ID]cpuset.CPUSet),
		node: make(map[idset.ID]cpuset.CPUSet),
		core: make(map[idset.ID]cpuset.CPUSet),
	}
	if sys != nil {
		for _, id := range sys.PackageIDs() {
			c.pkg[id] = sys.Package(id).CPUSet()
		}
		for _, id := range sys.NodeIDs() {
			c.node[id] = sys.Node(id).CPUSet()
		}
		for _, id := range sys.CPUIDs() {
			c.core[id] = sys.CPU(id).ThreadCPUSet()
		}
	}

	c.discoverCPUClusters(sys)
	c.discoverCacheGroups(sys)
	c.discoverCPUPriorities(sys)

	return c
}

func (c *topologyCache) discoverCPUPriorities(sys sysfs.System) {
	if sys == nil {
		return
	}
	var prio cpuPriorities

	// Discover on per-package basis
	for id := range c.pkg {
		cpuPriorities, sstActive := c.discoverSstCPUPriority(sys, id)

		if !sstActive {
			cpuPriorities = c.discoverCpufreqPriority(sys, id)
		}

		ecores := c.kind[sysfs.EfficientCore]
		ocores := sys.OnlineCPUs().Difference(ecores)

		for p, cpus := range cpuPriorities {
			source := map[bool]string{true: "sst", false: "cpufreq"}[sstActive]
			cset := sysfs.CPUSetFromIDSet(idset.NewIDSet(cpus...))

			if p != int(PriorityLow) && ocores.Size() > 0 {
				cset = cset.Difference(ecores)
			}

			log.Debug("package #%d (%s): %d %s priority cpus (%v)", id, source, len(cpus), CPUPriority(p), cset)
			prio[p] = prio[p].Union(cset)
		}
	}
	c.cpuPriorities = prio
}

func (c *topologyCache) discoverSstCPUPriority(sys sysfs.System, pkgID idset.ID) ([NumCPUPriorities][]idset.ID, bool) {
	active := false

	pkg := sys.Package(pkgID)
	sst := pkg.SstInfo()
	cpuIDs := c.pkg[pkgID].List()
	prios := make(map[idset.ID]CPUPriority, len(cpuIDs))

	// Determine SST-based priority. Based on experimentation there is some
	// hierarchy between the SST features. Without trying to be too smart
	// we follow the principles below:
	// 1. SST-TF has highest preference, mastering over SST-BF and making most
	//    of SST-CP settings ineffective
	// 2. SST-CP dictates over SST-BF
	// 3. SST-BF is meaningful if neither SST-TF nor SST-CP is enabled
	switch {
	case sst == nil:
	case sst.TFEnabled:
		log.Debug("package #%d: using SST-TF based CPU prioritization", pkgID)
		// We only look at the CLOS id as SST-TF (seems to) follows ordered CLOS priority
		for _, i := range cpuIDs {
			id := idset.ID(i)
			p := PriorityLow
			// First two CLOSes are prioritized by SST
			if sys.CPU(id).SstClos() < 2 {
				p = PriorityHigh
			}
			prios[id] = p
		}
		active = true
	case sst.CPEnabled:
		closPrio := c.sstClosPriority(sys, pkgID)
		log.Debug("package #%d: using SST-CP based CPU prioritization with CLOS mapping %v", pkgID, closPrio)

		active = false
		for _, i := range cpuIDs {
			id := idset.ID(i)
			clos := sys.CPU(id).SstClos()
			p := closPrio[clos]
			if p != PriorityNormal {
				active = true
			}
			prios[id] = p
		}
	}

	if !active && sst != nil && sst.BFEnabled {
		log.Debug("package #%d: using SST-BF based CPU prioritization", pkgID)
		for _, i := range cpuIDs {
			id := idset.ID(i)
			p := PriorityLow
			if sst.BFCores.Has(id) {
				p = PriorityHigh
			}
			prios[id] = p
		}
		active = true
	}

	var ret [NumCPUPriorities][]idset.ID

	for cpu, prio := range prios {
		ret[prio] = append(ret[prio], cpu)
	}
	return ret, active
}

func (c *topologyCache) sstClosPriority(sys sysfs.System, pkgID idset.ID) map[int]CPUPriority {
	sortedKeys := func(m map[int]int) []int {
		keys := make([]int, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		return keys
	}

	pkg := sys.Package(pkgID)
	sstinfo := pkg.SstInfo()

	// Get a list of unique CLOS proportional priority values
	closPps := make(map[int]int)
	closIds := make(map[int]int)
	for _, cpuID := range c.pkg[pkgID].List() {
		clos := sys.CPU(idset.ID(cpuID)).SstClos()
		pp := sstinfo.ClosInfo[clos].ProportionalPriority
		closPps[pp] = clos
		closIds[clos] = 0 // 0 is a dummy value here
	}

	// Form a list of (active) CLOS ids in sorted order
	var closSorted []int
	if sstinfo.CPPriority == sst.Ordered {
		// In ordered mode the priority is simply the CLOS id
		closSorted = sortedKeys(closIds)
		log.Debug("package #%d, ordered SST-CP priority with CLOS ids %v", pkgID, closSorted)
	} else {
		// In proportional mode we sort by the proportional priority parameter
		closPpSorted := sortedKeys(closPps)

		for _, pp := range closPpSorted {
			closSorted = append(closSorted, closPps[pp])
		}
		log.Debug("package #%d, proportional SST-CP priority with PP-to-CLOS parity %v", pkgID, closPps)
	}

	// Map from CLOS id to cpuallocator CPU priority
	closPriority := make(map[int]CPUPriority, len(closSorted))
	for _, id := range closSorted {
		// Default to normal priority
		closPriority[id] = PriorityNormal
	}
	if len(closSorted) > 1 {
		// Highest CLOS id maps to high CPU priority
		closPriority[closSorted[0]] = PriorityHigh
		closPriority[closSorted[len(closSorted)-1]] = PriorityLow
	}

	return closPriority
}

func (c *topologyCache) discoverCpufreqPriority(sys sysfs.System, pkgID idset.ID) [NumCPUPriorities][]idset.ID {
	var prios [NumCPUPriorities][]idset.ID

	// Group cpus by base frequency, core kind and energy performance profile
	freqs := map[uint64][]idset.ID{}
	epps := map[sysfs.EPP][]idset.ID{}
	cpuIDs := c.pkg[pkgID].List()
	for _, num := range cpuIDs {
		id := idset.ID(num)
		cpu := sys.CPU(id)
		bf := cpu.BaseFrequency()
		freqs[bf] = append(freqs[bf], id)

		epp := cpu.EPP()
		epps[epp] = append(epps[epp], id)
	}

	// Construct a sorted lists of detected frequencies and epp values
	freqList := []uint64{}
	for freq := range freqs {
		if freq > 0 {
			freqList = append(freqList, freq)
		}
	}
	utils.SortUint64s(freqList)

	eppList := []int{}
	for e := range epps {
		if e != sysfs.EPPUnknown {
			eppList = append(eppList, int(e))
		}
	}
	sort.Ints(eppList)

	// Finally, determine priority of each CPU
	for _, num := range cpuIDs {
		id := idset.ID(num)
		cpu := sys.CPU(id)
		p := PriorityNormal

		if len(freqList) > 1 {
			bf := cpu.BaseFrequency()

			// All cpus NOT in the lowest base frequency bin are considered high prio
			if bf > freqList[0] {
				p = PriorityHigh
			} else {
				p = PriorityLow
			}
		}

		// All E-cores are unconditionally considered low prio.
		// All cpus NOT in the lowest performance epp are considered high prio.
		// NOTE: higher EPP value denotes lower performance preference
		if cpu.CoreKind() == sysfs.EfficientCore {
			p = PriorityLow
		} else {
			if len(eppList) > 1 {
				epp := cpu.EPP()
				if int(epp) < eppList[len(eppList)-1] {
					p = PriorityHigh
				} else {
					p = PriorityLow
				}
			}
		}

		prios[p] = append(prios[p], id)
	}

	return prios
}

func (c *topologyCache) discoverCPUClusters(sys sysfs.System) {
	if sys == nil {
		return
	}

	for _, id := range sys.PackageIDs() {
		pkg := sys.Package(id)
		clusters := []*cpuCluster{}
		for _, die := range pkg.DieIDs() {
			for _, cl := range pkg.LogicalDieClusterIDs(die) {
				if cpus := pkg.LogicalDieClusterCPUSet(die, cl); cpus.Size() > 0 {
					clusters = append(clusters, &cpuCluster{
						pkg:     id,
						die:     die,
						cluster: cl,
						cpus:    cpus,
						kind:    sys.CPU(cpus.List()[0]).CoreKind(),
					})
				}
			}
		}
		if len(clusters) > 1 {
			log.Debug("package #%d has %d clusters:", id, len(clusters))
			for _, cl := range clusters {
				log.Debug("  die #%d, cluster #%d: %s cpus %s",
					cl.die, cl.cluster, cl.kind, cl.cpus)
			}
			c.clusters = append(c.clusters, clusters...)
		}
	}

	c.kind = map[sysfs.CoreKind]cpuset.CPUSet{}
	for _, kind := range sys.CoreKinds() {
		c.kind[kind] = sys.CoreKindCPUs(kind)
	}
}

func (c *topologyCache) pickCacheLevelForGrouping(sys sysfs.System) int {
	if sys == nil {
		return -1
	}

	online := sys.OnlineCPUs()
	for _, id := range online.List() {
		cpu := sys.CPU(id)
		pkg := sys.Package(cpu.PackageID())
		for n := cpu.CacheCount() - 1; n > 0; n-- {
			cpus := cpu.GetNthLevelCacheCPUSet(n)

			switch {
			case cpus.Size() == 0 || cpus.Size() == 1:
				continue
			case cpus.Equals(cpu.ThreadCPUSet().Intersection(online)):
				continue
			case cpus.Equals(pkg.DieCPUSet(cpu.DieID()).Intersection(online)):
				continue
			case cpus.Equals(pkg.CPUSet().Intersection(online)):
				continue
			}

			return n
		}
	}

	return -1
}

func (c *topologyCache) discoverCacheGroups(sys sysfs.System) {
	if sys == nil {
		return
	}

	n := c.pickCacheLevelForGrouping(sys)
	if n < 0 {
		log.Info("no cache level provides useful CPU grouping")
		return
	}

	log.Info("picked cache level %d for CPU grouping", n)

	online := sys.OnlineCPUs()
	for _, id := range sys.PackageIDs() {
		pkg := sys.Package(id)
		groups := []*cacheGroup{}
		assigned := idset.NewIDSet()

		for _, cpuID := range pkg.CPUSet().Intersection(online).List() {
			if assigned.Has(cpuID) {
				continue
			}

			cpu := sys.CPU(cpuID)
			cpus := cpu.GetNthLevelCacheCPUSet(n).Intersection(online)

			switch {
			case cpus.Size() == 0 || cpus.Size() == 1:
				continue
			case cpus.Equals(cpu.ThreadCPUSet().Intersection(online)):
				continue
			case cpus.Equals(pkg.DieCPUSet(cpu.DieID()).Intersection(online)):
				continue
			case cpus.Equals(pkg.CPUSet().Intersection(online)):
				continue
			}

			groups = append(groups, &cacheGroup{
				pkg:  cpu.PackageID(),
				die:  cpu.DieID(),
				node: cpu.NodeID(),
				cpus: cpus.Clone(),
				kind: cpu.CoreKind(), // TODO(klihub): maybe verify all CPUs are the same kind
			})
			assigned.Add(cpus.UnsortedList()...)
		}

		if len(groups) > 1 {
			c.cacheGroups = append(c.cacheGroups, groups...)
		}
	}

	// sort groups by package, die, NUMA node, and lowest CPU ID.
	slices.SortFunc(c.cacheGroups, func(a, b *cacheGroup) int {
		if diff := a.pkg - b.pkg; diff != 0 {
			return diff
		}
		if diff := a.die - b.die; diff != 0 {
			return diff
		}
		if diff := a.node - b.node; diff != 0 {
			return diff
		}
		return a.cpus.List()[0] - b.cpus.List()[0]
	})

	for idx, g := range c.cacheGroups {
		g.id = idx

		for _, cpuID := range g.cpus.UnsortedList() {
			cpu := sys.CPU(cpuID)
			if cpu.PackageID() != g.pkg {
				log.Panic("CPU #%d in cache group #%d has package #%d != #%d",
					cpuID, g.id, cpu.PackageID(), g.pkg)
			}
			if cpu.DieID() != g.die {
				log.Panic("CPU #%d in cache group #%d has die #%d != #%d",
					cpuID, g.id, cpu.DieID(), g.die)
			}
			if cpu.NodeID() != g.node {
				log.Panic("CPU #%d in cache group #%d has node #%d != #%d",
					cpuID, g.id, cpu.NodeID(), g.die)
			}
		}

		cpu := sys.CPU(g.cpus.List()[0])
		log.Debug("cache group #%d: pkg #%d/die #%d/node #%d %s cpus %s",
			g.id, cpu.PackageID(), cpu.DieID(), cpu.NodeID(), g.kind, g.cpus)
	}
}

func (p CPUPriority) String() string {
	switch p {
	case PriorityHigh:
		return "high"
	case PriorityNormal:
		return "normal"
	case PriorityLow:
		return "low"
	}
	return "none"
}

// cmpCPUSet compares two cpusets in terms of preferred cpu priority. Returns:
//
//	> 0 if cpuset A is preferred
//	< 0 if cpuset B is preferred
//	0 if cpusets A and B are equal in terms of cpu priority
func (c *cpuPriorities) cmpCPUSet(csetA, csetB cpuset.CPUSet, prefer CPUPriority, cpuCnt int) int {
	if prefer == PriorityNone {
		return 0
	}

	// For low prio request, favor cpuset with the tightest fit.
	if cpuCnt > 0 && prefer == PriorityLow {
		prefA := csetA.Intersection(c[prefer]).Size()
		prefB := csetB.Intersection(c[prefer]).Size()
		// both sets have enough preferred CPUs, return the smaller one (tighter fit)
		if prefA >= cpuCnt && prefB >= cpuCnt {
			return prefB - prefA
		}
		// only one set has enough preferred CPUs, return the bigger/only one
		if prefA >= cpuCnt || prefB >= cpuCnt {
			return prefA - prefB
		}
	}

	// For high prio request, favor the tightest fit falling back to normal prio
	if cpuCnt > 0 && prefer == PriorityHigh {
		prefA := csetA.Intersection(c[prefer]).Size()
		prefB := csetB.Intersection(c[prefer]).Size()
		if prefA == 0 && prefB == 0 {
			prefA = csetA.Intersection(c[PriorityNormal]).Size()
			prefB = csetB.Intersection(c[PriorityNormal]).Size()
		}
		// both sets have enough preferred CPUs, return the smaller one (tighter fit)
		if prefA >= cpuCnt && prefB >= cpuCnt {
			return prefB - prefA
		}
		// only one set has enough preferred CPUs, return the bigger/only one
		if prefA >= cpuCnt || prefB >= cpuCnt {
			return prefA - prefB
		}
	}

	// Favor cpuset having CPUs with priorities equal to or lower than what was requested
	for prio := prefer; prio < NumCPUPriorities; prio++ {
		prefA := csetA.Intersection(c[prio]).Size()
		prefB := csetB.Intersection(c[prio]).Size()
		if cpuCnt > 0 && prio == prefer && prefA >= cpuCnt && prefB >= cpuCnt {
			// Prefer the tightest fitting if both cpusets satisfy the
			// requested amount of CPUs with the preferred priority
			return prefB - prefA
		}
		if prefA != prefB {
			return prefA - prefB
		}
	}
	// Repel cpuset having CPUs with higher priority than what was requested
	for prio := PriorityHigh; prio < prefer; prio++ {
		nonprefA := csetA.Intersection(c[prio]).Size()
		nonprefB := csetB.Intersection(c[prio]).Size()
		if nonprefA != nonprefB {
			return nonprefB - nonprefA
		}
	}
	return 0
}

func (c *cpuCluster) HasSmallerIDsThan(o *cpuCluster) bool {
	if c.pkg < o.pkg {
		return true
	}
	if c.pkg > o.pkg {
		return false
	}
	if c.die < o.die {
		return true
	}
	if c.die > o.die {
		return false
	}
	return c.cluster < o.cluster
}

func (c *cpuCluster) String() string {
	return fmt.Sprintf("cluster #%d/%d/%d, %d %s CPUs (%s)", c.pkg, c.die, c.cluster,
		c.cpus.Size(), c.kind, c.cpus)
}

func (c *cacheGroup) PackageID() int {
	return c.pkg
}

func (c *cacheGroup) DieID(sys sysfs.System) int {
	cpu := sys.CPU(c.cpus.List()[0])
	return cpu.DieID()
}

func (c *cacheGroup) SmallestCoreID(sys sysfs.System) int {
	return c.cpus.List()[0]
}

func (c *cacheGroup) String() string {
	return fmt.Sprintf("group #%d/%d, %d %s CPUs (%s)", c.pkg, c.id,
		c.cpus.Size(), c.kind, c.cpus)
}
