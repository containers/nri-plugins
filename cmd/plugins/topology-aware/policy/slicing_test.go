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
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/containers/nri-plugins/pkg/agent/podresapi"
	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	harness "github.com/containers/nri-plugins/pkg/testutils/e2eish"
	"github.com/containers/nri-plugins/pkg/topology"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// newSlicingPolicy sets up 2 sockets of 16 CPUs, each with 2 L3 caches of
// 8 CPUs. CPU 0 is reserved:
//
//	root
//	|-- socket #0: L3 cache #0 (1-7), L3 cache #1 (8-15)
//	`-- socket #1: L3 cache #2 (16-23), L3 cache #3 (24-31)
func newSlicingPolicy(t *testing.T) *policy {
	t.Helper()
	return newSlicingPolicyOn(t, harness.Topology{{Mem: "2G", Cores: 8, Packages: 2}},
		harness.Host{Caches: `[{"cpusets": ["0-7", "8-15", "16-23", "24-31"], "level": 3, "size": "32M"}]`})
}

// newSlicingPolicyOn sets up a policy on a machine of the given topology and
// host, with CPU 0 reserved.
func newSlicingPolicyOn(t *testing.T, topo harness.Topology, host harness.Host) *policy {
	t.Helper()
	sys := harness.NewMachine(t, topo, host)
	p := New().(*policy)
	if err := p.Setup(&policyapi.BackendOptions{
		Cache:  &mockCache{returnValue2ForLookupContainer: true},
		System: sys,
		Config: &cfgapi.Config{ReservedResources: cfgapi.Constraints{cfgapi.CPU: "cpuset:0"}},
	}); err != nil {
		t.Fatalf("failed to set up policy: %v", err)
	}
	return p
}

// hintedCtr is a container with pod resource topology hints.
type hintedCtr struct {
	*mockContainer
	hints topology.Hints
}

func (c *hintedCtr) GetTopologyHints() topology.Hints {
	return c.hints
}

// newSlicingCtr returns a container which requests cpu. With hints, it is a
// container which picks its CPUs by those pod resource topology hints.
func newSlicingCtr(name string, qos v1.PodQOSClass, cpu string, hints ...string) cache.Container {
	rl := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse(cpu),
		v1.ResourceMemory: resource.MustParse("100Mi"),
	}
	c := &mockContainer{
		name:                                  name,
		returnValueForGetID:                   name,
		returnValueForQOSClass:                qos,
		returnValueForGetResourceRequirements: v1.ResourceRequirements{Requests: rl, Limits: rl},
		pod:                                   &mockPod{name: name, returnValueFotGetQOSClass: qos, annotations: map[string]string{}},
	}
	if len(hints) == 0 {
		return c
	}
	c.pod.(*mockPod).annotations[pickResourcesByHints+"/pod"] = "true"
	h := &hintedCtr{mockContainer: c, hints: topology.Hints{}}
	for i, cpus := range hints {
		provider := fmt.Sprintf("%sdevice-%d", podresapi.HintProvider, i)
		h.hints[provider] = topology.Hint{Provider: provider, CPUs: cpus}
	}
	return h
}

// requireNoOvercommit reports an error for each pool with negative free shared CPU.
func requireNoOvercommit(t *testing.T, p *policy) {
	t.Helper()
	for _, n := range p.pools {
		if free := n.FreeSupply().AllocatableSharedCPU(true); free < 0 {
			t.Errorf("%s is overcommitted: %dm", n.Name(), free)
		}
	}
}

// Slicing CPUs off the root must leave every pool below it enough whole
// CPUs for its shared grants, whether those are in L3 cache or socket pools.
func TestExclusiveSlicingKeepsSharedCPUs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pools     []string
		shared    string
		sliceable int // CPUs the root can slice: 5+6+6+6 and 11+12
	}{
		{
			name:      "L3 cache pools",
			pools:     []string{"socket #0/L3 cache #0", "socket #0/L3 cache #1", "socket #1/L3 cache #2", "socket #1/L3 cache #3"},
			shared:    "1500m",
			sliceable: 23,
		},
		{
			name:      "socket pools",
			pools:     []string{"socket #0", "socket #1"},
			shared:    "4000m",
			sliceable: 23,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSlicingPolicy(t)
			for i, pool := range tc.pools {
				if _, err := p.allocatePool(newSlicingCtr(fmt.Sprintf("shared-%d", i), v1.PodQOSBurstable, tc.shared), pool); err != nil {
					t.Fatalf("failed to allocate shared CPU in %s: %v", pool, err)
				}
			}
			if cpus, err := p.root.FreeSupply().SliceableCPUs(); err != nil || cpus.Size() != tc.sliceable {
				t.Errorf("root can slice %s (%v), expected %d CPUs", cpus, err, tc.sliceable)
			}
			g, err := p.allocatePool(newSlicingCtr("excl", v1.PodQOSGuaranteed, "14"), "")
			if err != nil {
				t.Fatalf("failed to allocate 14 exclusive CPUs: %v", err)
			}
			if g.GetCPUNode().Name() != "root" {
				t.Fatalf("allocated from %s, expected root", g.GetCPUNode().Name())
			}
			requireNoOvercommit(t, p)
		})
	}
}

// Slicing CPUs off the root by topology hints must keep 2 CPUs of each hint,
// from a socket which has room for all the CPUs of the request, and from a
// socket which has room for fewer.
func TestExclusiveSlicingKeepsHintedCPUs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shared string // in socket #0
		cpu    string
		hints  []string
	}{
		{name: "socket with room for 4 of 4 CPUs", shared: "11000m", cpu: "4", hints: []string{"1-7", "8-15"}},
		{name: "socket with room for 3 of 4 CPUs", shared: "12000m", cpu: "4", hints: []string{"12-13", "24-25"}},
		{name: "socket with room for 4 of 6 CPUs", shared: "11000m", cpu: "6", hints: []string{"1-7", "8-15", "24-25"}},
		{name: "socket with room for 7 of 8 CPUs", shared: "8000m", cpu: "8", hints: []string{"1-7", "8-11", "12-15", "24-27"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSlicingPolicy(t)
			if _, err := p.allocatePool(newSlicingCtr("shared", v1.PodQOSBurstable, tc.shared), "socket #0"); err != nil {
				t.Fatalf("failed to allocate shared CPU in socket #0: %v", err)
			}
			g, err := p.allocatePool(newSlicingCtr("hinted", v1.PodQOSGuaranteed, tc.cpu, tc.hints...), "root")
			if err != nil {
				t.Fatalf("failed to allocate %s exclusive CPUs: %v", tc.cpu, err)
			}
			for _, h := range tc.hints {
				if got := g.ExclusiveCPUs().Intersection(cpuset.MustParse(h)); got.Size() != 2 {
					t.Errorf("granted %s, expected 2 CPUs of hint %s", g.ExclusiveCPUs(), h)
				}
			}
			requireNoOvercommit(t, p)
		})
	}
}

// SliceableCPUs() of a pool must offer at most the free whole CPUs of the pool
// itself, and the CPUs of a pool below it which are in none of its children.
// Slicing CPUs for a request must offer all CPUs of a non-leaf pool with room
// for the request.
func TestSliceableCPUs(t *testing.T) {
	t.Run("pool with room for the request", func(t *testing.T) {
		p := newSlicingPolicy(t)
		if _, err := p.allocatePool(newSlicingCtr("shared", v1.PodQOSBurstable, "17000m"), "root"); err != nil {
			t.Fatalf("failed to allocate shared CPU in root: %v", err)
		}
		root := p.root.FreeSupply().(*supply)
		for cnt, want := range map[int]int{14: 31, 15: 14, math.MaxInt: 14} {
			if cpus, err := root.sliceableCPUs(p.root, cnt, nil); err != nil || cpus.Size() != want {
				t.Errorf("root can slice %s (%v) for %d CPUs, expected %d CPUs", cpus, err, cnt, want)
			}
		}
	})
	t.Run("pool with shared grants", func(t *testing.T) {
		p := newSlicingPolicy(t)
		if _, err := p.allocatePool(newSlicingCtr("shared", v1.PodQOSBurstable, "14000m"), "socket #0"); err != nil {
			t.Fatalf("failed to allocate shared CPU in socket #0: %v", err)
		}
		if cpus, err := p.nodes["socket #0"].FreeSupply().SliceableCPUs(); err != nil || cpus.Size() != 1 {
			t.Errorf("socket #0 can slice %s (%v), expected 1 CPU", cpus, err)
		}
	})
	t.Run("L3 caches straddling NUMA nodes", func(t *testing.T) {
		// NUMA node #0 has CPUs 0-7, but only its L3 caches 0-2 and 3-5.
		p := newSlicingPolicyOn(t, harness.Topology{{Mem: "2G", Cores: 4, Nodes: 2, Packages: 2}}, harness.Host{
			Caches: `[{"cpusets": ["0-2", "3-5", "6-9", "10-12", "13-15", "16-18", "19-21", "22-25", "26-28", "29-31"], "level": 3, "size": "32M"}]`,
		})
		if cpus, err := p.nodes["socket #0"].FreeSupply().SliceableCPUs(); err != nil || !cpus.Equals(cpuset.MustParse("1-15")) {
			t.Errorf("socket #0 can slice %s (%v), expected 1-15", cpus, err)
		}
	})
}
