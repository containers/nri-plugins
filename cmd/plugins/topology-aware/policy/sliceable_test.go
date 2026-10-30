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
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	v1 "k8s.io/api/core/v1"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	harness "github.com/containers/nri-plugins/pkg/testutils/e2eish"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// annotated returns c with the pod annotation key set to value.
func annotated(c cache.Container, key, value string) cache.Container {
	c.(*mockContainer).pod.(*mockPod).annotations[key+"/pod"] = value
	return c
}

// freeWholeCPUs returns how many whole CPUs a supply can give up.
func freeWholeCPUs(s Supply) int {
	return max(0, s.AllocatableSharedCPU(true)/1000)
}

// checkSliceable verifies for every pool that SliceableCPUCount() is the
// number of CPUs in SliceableCPUs(), with or without preferred CPUs, that
// no pool gets more of them than it can give up, and that slicing all of
// them, or the same number of CPUs picked for a request of that many,
// overcommits no pool.
func checkSliceable(t *testing.T, p *policy, when string, hinted []cpuset.CPUSet) {
	t.Helper()
	for _, pool := range p.pools {
		s := pool.FreeSupply().(*supply)
		cnt := s.SliceableCPUCount()
		for _, pref := range [][]cpuset.CPUSet{nil, hinted} {
			all, err := s.sliceableCPUs(pool, math.MaxInt, pref)
			if err != nil || all.Size() != cnt {
				t.Fatalf("%s: %s: sliceable CPUs preferring %v: %s (%v), SliceableCPUCount() is %d",
					when, pool.Name(), pref, all, err, cnt)
			}
			checkWithinLimits(t, pool, all, when)
			checkNoOvercommit(t, p, pool, all, when)

			some, err := s.sliceableCPUs(pool, cnt, pref)
			if err != nil || some.Size() < cnt {
				t.Fatalf("%s: %s: sliceable CPUs for %d preferring %v: %s (%v)",
					when, pool.Name(), cnt, pref, some, err)
			}
			sliced, err := s.takeCPUs(&some, nil, cnt, nonePrio)
			if err != nil {
				t.Fatalf("%s: %s: failed to take %d sliceable CPUs: %v", when, pool.Name(), cnt, err)
			}
			checkNoOvercommit(t, p, pool, sliced, when)
		}
	}
}

// checkWithinLimits verifies that neither pool nor any pool below it has
// more of cpus than it can give up.
func checkWithinLimits(t *testing.T, pool Node, cpus cpuset.CPUSet, when string) {
	t.Helper()
	ps := pool.FreeSupply()
	if part, limit := cpus.Intersection(ps.SharableCPUs()), freeWholeCPUs(ps); part.Size() > limit {
		t.Fatalf("%s: sliceable CPUs have %s of %s, above its %d free whole CPUs",
			when, part, pool.Name(), limit)
	}
	for _, c := range pool.Children() {
		checkWithinLimits(t, c, cpus, when)
	}
}

// checkNoOvercommit verifies that slicing cpus off pool overcommits no pool
// which is not overcommitted already.
func checkNoOvercommit(t *testing.T, p *policy, pool Node, cpus cpuset.CPUSet, when string) {
	t.Helper()
	for _, n := range p.pools {
		ns := n.FreeSupply()
		before := 1000*ns.SharableCPUs().Size() - n.GrantedSharedCPU()
		after := 1000*ns.SharableCPUs().Difference(cpus).Size() - n.GrantedSharedCPU()
		if before >= 0 && after < 0 {
			t.Fatalf("%s: slicing %s off %s overcommits %s (%dm)", when, cpus, pool.Name(), n.Name(), after)
		}
	}
}

// checkScoreMatchesAllocation verifies for every pool that GetScore() finds
// enough capacity for a request if and only if AllocateCPU() succeeds.
func checkScoreMatchesAllocation(t *testing.T, p *policy, when string, ctr cache.Container) {
	t.Helper()
	req, err := p.newRequest(ctr, p.memAllocator.Masks().AvailableTypes())
	if err != nil {
		t.Fatalf("%s: failed to create request: %v", when, err)
	}
	for _, pool := range p.pools {
		s := pool.FreeSupply().(*supply)
		sufficient := s.GetScore(req).SharedCapacity() >= 0
		g, err := s.AllocateCPU(req)
		if err == nil {
			s.ReleaseCPU(g)
		}
		if sufficient != (err == nil) {
			t.Fatalf("%s: %s: %s scores sufficient=%v, but allocation error is %v",
				when, pool.Name(), req, sufficient, err)
		}
	}
}

// Random grants and releases on two topologies, one with CPUs of NUMA nodes
// in none of their L3 cache pools, checking after every step that no pool is
// overcommitted, that the sliceable count and set agree, and that scoring
// agrees with allocation.
func TestSliceableRandomGrants(t *testing.T) {
	for _, tc := range []struct {
		name string
		topo harness.Topology
		host harness.Host
	}{
		{
			name: "2 sockets x 2 L3 caches",
			topo: harness.Topology{{Mem: "2G", Cores: 8, Packages: 2}},
			host: harness.Host{Caches: `[{"cpusets": ["0-7", "8-15", "16-23", "24-31"], "level": 3, "size": "32M"}]`},
		},
		{
			name: "isolated CPUs",
			topo: harness.Topology{{Mem: "2G", Cores: 8, Packages: 2}},
			host: harness.Host{
				Caches:   `[{"cpusets": ["0-7", "8-15", "16-23", "24-31"], "level": 3, "size": "32M"}]`,
				Isolated: "4-7",
			},
		},
		{
			name: "L3 caches straddling NUMA nodes",
			topo: harness.Topology{{Mem: "2G", Cores: 4, Nodes: 2, Packages: 2}},
			host: harness.Host{Caches: `[{"cpusets": ["0-2", "3-5", "6-9", "10-12", "13-15",
				"16-18", "19-21", "22-25", "26-28", "29-31"], "level": 3, "size": "32M"}]`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSlicingPolicyOn(t, tc.topo, tc.host)

			var (
				rnd  = rand.New(rand.NewPCG(1, 2))
				live []cache.Container
			)
			poolCPUs := func() string {
				return p.pools[rnd.IntN(len(p.pools))].GetSupply().SharableCPUs().String()
			}
			for i := range 300 {
				var (
					name = fmt.Sprintf("ctr-%d", i)
					hint = p.pools[rnd.IntN(len(p.pools))].Name()
					ctr  cache.Container
				)
				if rnd.IntN(5) == 0 {
					hint = ""
				}
				switch r := rnd.IntN(100); {
				case r < 25 && len(live) > 0:
					k := rnd.IntN(len(live))
					p.releasePool(live[k])
					live = append(live[:k], live[k+1:]...)
				case r < 60:
					ctr = newSlicingCtr(name, v1.PodQOSBurstable, fmt.Sprintf("%dm", 1+rnd.IntN(5000)))
				case r < 65:
					ctr = newSlicingCtr(name, v1.PodQOSGuaranteed, fmt.Sprint(1+rnd.IntN(12)))
				case r < 70:
					ctr = annotated(newSlicingCtr(name, v1.PodQOSGuaranteed, fmt.Sprint(1+rnd.IntN(3))),
						preferIsolatedCPUsKey, "true")
				case r < 80:
					ctr = newSlicingCtr(name, v1.PodQOSGuaranteed, fmt.Sprint(2+rnd.IntN(11)), poolCPUs(), poolCPUs())
				default:
					ctr = annotated(newSlicingCtr(name, v1.PodQOSGuaranteed, fmt.Sprintf("%d%03dm", 1+rnd.IntN(12), 1+rnd.IntN(999))),
						preferSharedCPUsKey, "false")
				}

				when := fmt.Sprintf("step #%d", i)
				if ctr != nil {
					if ctr.GetQOSClass() == v1.PodQOSGuaranteed {
						checkScoreMatchesAllocation(t, p, when, ctr)
					}
					if _, err := p.allocatePool(ctr, hint); err == nil {
						live = append(live, ctr)
					}
				}
				requireNoOvercommit(t, p)
				checkSliceable(t, p, when, []cpuset.CPUSet{cpuset.MustParse(poolCPUs()), cpuset.MustParse(poolCPUs())})
			}
			t.Logf("%d live containers after 300 steps", len(live))
		})
	}
}
