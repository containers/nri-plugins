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
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/utils"
)

// n4c16 is test/e2e/policies.test-suite/topology-aware/n4c16/topology.var.json.
var n4c16 = e2eTopology{{Mem: "2G", Cores: 2, Nodes: 2, Packages: 2}}

// TestE2eishN4c16Test00BasicPlacement is n4c16/test00-basic-placement.
func TestE2eishN4c16Test00BasicPlacement(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig())

	// pod0: 4 guaranteed containers eligible for isolated CPUs spread
	// evenly over the NUMA nodes.
	e.createGuaranteed(podSpec{ContCount: 4, CPU: "1"})
	for _, c := range []string{"pod0c0", "pod0c1", "pod0c2", "pod0c3"} {
		require.Len(t, e.cpus(c), 1, c)
	}
	requireDisjoint(t, e.cpus("pod0c0"), e.cpus("pod0c1"), e.cpus("pod0c2"), e.cpus("pod0c3"))
	requireDisjoint(t, e.nodes("pod0c0"), e.nodes("pod0c1"), e.nodes("pod0c2"), e.nodes("pod0c3"))
	e.deleteAllPods()

	// pod1: 4 guaranteed containers not eligible for isolated CPUs spread
	// evenly over the NUMA nodes.
	e.createGuaranteed(podSpec{ContCount: 4, CPU: "3", Annotations: map[string]string{
		"hide-hyperthreads.resource-policy.nri.io/container.pod1c2": "true",
	}})
	require.Len(t, e.cpus("pod1c0"), 3)
	require.Len(t, e.cpus("pod1c1"), 3)
	require.Len(t, e.cpus("pod1c2"), 2)
	require.Len(t, e.cores("pod1c2"), 2)
	require.Len(t, e.cpus("pod1c3"), 3)
	requireDisjoint(t, e.cpus("pod1c0"), e.cpus("pod1c1"), e.cpus("pod1c2"), e.cpus("pod1c3"))
	requireDisjoint(t, e.nodes("pod1c0"), e.nodes("pod1c1"), e.nodes("pod1c2"), e.nodes("pod1c3"))
	e.deleteAllPods()

	// pod2: 4 burstable containers not eligible for exclusive CPUs spread
	// evenly over the NUMA nodes.
	e.createBurstable(podSpec{ContCount: 4, CPUReq: "2", CPULim: "2002m"})
	requireDisjoint(t, e.cpus("pod2c0"), e.cpus("pod2c1"), e.cpus("pod2c2"), e.cpus("pod2c3"))
	requireDisjoint(t, e.nodes("pod2c0"), e.nodes("pod2c1"), e.nodes("pod2c2"), e.nodes("pod2c3"))
	e.deleteAllPods()

	// pod3: the resources of init containers are freed before the next
	// one starts. Otherwise neither 5 init containers of 5 CPUs nor the
	// containers after them would fit.
	e.createGuaranteed(podSpec{IContCount: 5, ContCount: 2, CPU: "5", Mem: "100M"})
	requireDisjoint(t, e.cpus("pod3c0"), e.cpus("pod3c1"))
	requireDisjoint(t, e.nodes("pod3c0"), e.nodes("pod3c1"))
	requireDisjoint(t, e.packages("pod3c0"), e.packages("pod3c1"))
	e.deleteAllPods()

	// pod4: with pod colocation the containers of a pod are kept close
	// instead of spread out.
	e.launch(e2eConfig(func(c *cfgapi.Config) { c.ColocatePods = true }))
	e.createGuaranteed(podSpec{ContCount: 4, CPU: "100m"})
	require.Equal(t, e.cpus("pod4c0"), e.cpus("pod4c1"))
	require.Equal(t, e.cpus("pod4c0"), e.cpus("pod4c2"))
	require.Equal(t, e.cpus("pod4c0"), e.cpus("pod4c3"))
	e.deleteAllPods()

	// pod{5,6,7}: with namespace colocation the containers of pods in the
	// same namespace are kept close instead of spread out.
	e.launch(e2eConfig(func(c *cfgapi.Config) { c.ColocateNamespaces = true }))
	e.createGuaranteed(podSpec{Namespace: "test-ns", CPU: "100m"})
	e.createGuaranteed(podSpec{Namespace: "test-ns", CPU: "100m"})
	e.createGuaranteed(podSpec{Namespace: "test-ns", ContCount: 2, CPU: "100m"})
	require.Equal(t, e.cpus("pod5c0"), e.cpus("pod6c0"))
	require.Equal(t, e.cpus("pod5c0"), e.cpus("pod7c0"))
	require.Equal(t, e.cpus("pod5c0"), e.cpus("pod7c1"))
}

// TestE2eishN4c16Test01AlwaysFits is n4c16/test01-always-fits: guaranteed
// and burstable pods get the CPUs they need while enough are free.
func TestE2eishN4c16Test01AlwaysFits(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig(func(c *cfgapi.Config) { c.ColocatePods = true }))

	// pod0 fits in a core.
	e.createGuaranteed(podSpec{CPU: "1"})
	require.Equal(t, nodeIds(1), e.nodes("pod0c0"))
	require.Equal(t, cpuIds(4), e.cpus("pod0c0"))

	// pod1 takes a full core, from another node than pod0.
	e.createGuaranteed(podSpec{CPU: "2"})
	require.Equal(t, cpuIds(4), e.cpus("pod0c0"))
	require.Equal(t, nodeIds(2), e.nodes("pod1c0"))
	require.Equal(t, cpuIds(8, 9), e.cpus("pod1c0"))

	// pod2 does not fit in a core but fits in a node.
	e.createGuaranteed(podSpec{CPU: "3"})
	require.Len(t, e.cpus("pod0c0"), 1)
	require.Len(t, e.cpus("pod1c0"), 2)
	require.Len(t, e.cores("pod1c0"), 1)
	require.Len(t, e.cpus("pod2c0"), 3)
	require.Len(t, e.cores("pod2c0"), 2)
	require.Len(t, e.nodes("pod2c0"), 1)
	requireDisjoint(t, e.cpus("pod0c0"), e.cpus("pod1c0"), e.cpus("pod2c0"))

	// pod3 would exhaust the shared CPUs of a node. The policy refuses
	// that even for idle shared CPUs, so the container goes to another
	// node or, failing that, one level up.
	e.createGuaranteed(podSpec{CPU: "4"})
	require.Len(t, e.cpus("pod0c0"), 1)
	require.Len(t, e.cpus("pod1c0"), 2)
	require.Len(t, e.cores("pod1c0"), 1)
	require.Len(t, e.cpus("pod2c0"), 3)
	require.Len(t, e.cores("pod2c0"), 2)
	require.Len(t, e.nodes("pod2c0"), 1)
	require.Len(t, e.cpus("pod3c0"), 4)
	require.Len(t, e.cores("pod3c0"), 2)
	require.Len(t, e.nodes("pod3c0"), 2)
	requireDisjoint(t, e.cpus("pod0c0"), e.cpus("pod1c0"), e.cpus("pod2c0"), e.cpus("pod3c0"))
	e.deleteAllPods()

	// pod4 fits in a die.
	e.createGuaranteed(podSpec{CPU: "5"})
	require.Len(t, e.cpus("pod4c0"), 5)
	require.Len(t, e.cores("pod4c0"), 3)
	require.Len(t, e.nodes("pod4c0"), 2)
	require.Len(t, e.dies("pod4c0"), 1)

	// pod5 takes a full die. cpu0 is reserved, so 7 CPUs fill die 0.
	e.createGuaranteed(podSpec{CPU: "7"})
	require.Len(t, e.cpus("pod4c0"), 5)
	require.Len(t, e.cores("pod4c0"), 3)
	require.Len(t, e.nodes("pod4c0"), 2)
	require.Len(t, e.dies("pod4c0"), 1)
	require.Len(t, e.cpus("pod5c0"), 7)
	require.Len(t, e.cores("pod5c0"), 4)
	require.Len(t, e.dies("pod5c0"), 1)
	requireDisjoint(t, e.cpus("pod4c0"), e.cpus("pod5c0"))
	e.deleteAllPods()

	// pod6 does not fit in a die and needs the root.
	e.createGuaranteed(podSpec{CPU: "9"})
	require.Len(t, e.cpus("pod6c0"), 9)
	require.Len(t, e.packages("pod6c0"), 2)
	e.deleteAllPods()

	e.resetCounters()

	// Burstable containers get at least the CPUs they request.
	e.createBurstable(podSpec{CPUReq: "3", CPULim: "4"})
	require.GreaterOrEqual(t, len(e.cpus("pod0c0")), 2)

	e.createBurstable(podSpec{CPUReq: "4", CPULim: "5"})
	require.GreaterOrEqual(t, len(e.cpus("pod0c0")), 2)
	require.GreaterOrEqual(t, len(e.cpus("pod1c0")), 4)

	e.createBurstable(podSpec{CPUReq: "5", CPULim: "6"})
	require.GreaterOrEqual(t, len(e.cpus("pod0c0")), 2)
	require.GreaterOrEqual(t, len(e.cpus("pod1c0")), 4)
	require.GreaterOrEqual(t, len(e.cpus("pod2c0")), 5)

	e.deletePods("pod0", "pod1")

	e.createBurstable(podSpec{CPUReq: "8", CPULim: "9"})
	require.GreaterOrEqual(t, len(e.cpus("pod2c0")), 5)
	require.GreaterOrEqual(t, len(e.cpus("pod3c0")), 8)

	e.deletePods("pod3")

	// pod2 and pod4 take a die each, with 8 CPUs per die, so pod5
	// spreads over nodes.
	e.createBurstable(podSpec{CPUReq: "5", CPULim: "6"})
	e.createBurstable(podSpec{CPUReq: "4", CPULim: "5"})
	require.GreaterOrEqual(t, len(e.cpus("pod2c0")), 5)
	require.GreaterOrEqual(t, len(e.cpus("pod4c0")), 5)
	require.GreaterOrEqual(t, len(e.cpus("pod5c0")), 4)
}

// TestE2eishN4c16Test02ShrinkAndGrowShared is
// n4c16/test02-shrink-and-grow-shared: shared CPUs shrink as exclusive ones
// are taken and grow back as they are freed.
func TestE2eishN4c16Test02ShrinkAndGrowShared(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig(func(c *cfgapi.Config) { c.ColocatePods = true }))

	// pod0 takes 10 of 16 CPUs with two containers, each in its own die.
	e.createGuaranteed(podSpec{CPU: "5", ContCount: 2})
	require.Len(t, e.cpus("pod0c0"), 5)
	require.Len(t, e.cpus("pod0c1"), 5)
	require.Len(t, e.nodes("pod0c0"), 2)
	require.Len(t, e.nodes("pod0c1"), 2)
	require.Len(t, e.dies("pod0c0"), 1)
	require.Len(t, e.dies("pod0c1"), 1)
	requireDisjoint(t, e.cpus("pod0c0"), e.cpus("pod0c1"))

	// pod1 is two best-effort containers.
	e.createBestEffort(podSpec{ContCount: 2})
	require.Len(t, e.cpus("pod0c0"), 5)
	require.Len(t, e.cpus("pod0c1"), 5)
	require.NotEmpty(t, e.cpus("pod1c0"))
	require.NotEmpty(t, e.cpus("pod1c1"))
	requireDisjoint(t,
		union(e.cpus("pod0c0"), e.cpus("pod0c1")),
		union(e.cpus("pod1c0"), e.cpus("pod1c1")))

	e.deletePods("pod0")

	// pod2 squeezes the best-effort containers with 4 containers of 3
	// CPUs.
	e.createGuaranteed(podSpec{CPU: "3", ContCount: 4})
	for _, c := range []string{"pod2c0", "pod2c1", "pod2c2", "pod2c3"} {
		require.Len(t, e.cpus(c), 3, c)
	}
	requireDisjoint(t, e.cpus("pod2c0"), e.cpus("pod2c1"), e.cpus("pod2c2"), e.cpus("pod2c3"))

	// pod3 takes the last unreserved CPU the shared pools can give.
	e.createGuaranteed(podSpec{CPU: "1"})
	requireDisjoint(t,
		union(e.cpus("pod1c0"), e.cpus("pod1c1")),
		union(e.cpus("pod3c0"),
			e.cpus("pod2c0"), e.cpus("pod2c1"), e.cpus("pod2c2"), e.cpus("pod2c3")))
}

// TestE2eishN4c16Test03SimpleAffinity is n4c16/test03-simple-affinity.
func TestE2eishN4c16Test03SimpleAffinity(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig())

	// pod0: 4 containers without affinities spread evenly over the nodes.
	e.createGuaranteed(podSpec{ContCount: 4, CPU: "1"})
	require.Equal(t, nodeIds(1), e.nodes("pod0c0"))
	require.Equal(t, nodeIds(2), e.nodes("pod0c1"))
	require.Equal(t, nodeIds(3), e.nodes("pod0c2"))
	require.Equal(t, nodeIds(0), e.nodes("pod0c3"))
	e.deleteAllPods()

	// pod1: affinities [0,1] and [2,3] put c0 and c1 in node1, c2 and c3
	// in node2.
	e.createGuaranteed(podSpec{ContCount: 4, CPU: "1", Annotations: map[string]string{
		"resource-policy.nri.io/affinity": "pod1c0: [ pod1c1 ]\npod1c2: [ pod1c3 ]\n",
	}})
	require.Equal(t, nodeIds(1), e.nodes("pod1c0"))
	require.Equal(t, nodeIds(1), e.nodes("pod1c1"))
	require.Equal(t, nodeIds(2), e.nodes("pod1c2"))
	require.Equal(t, nodeIds(2), e.nodes("pod1c3"))
	e.deleteAllPods()

	// pod2: anti-affinities keep c4 away from c0, c1 and c2, and c5 away
	// from c0, c2 and c3.
	e.createGuaranteed(podSpec{ContCount: 6, CPU: "1", Annotations: map[string]string{
		"resource-policy.nri.io/anti-affinity": "pod2c4: [ pod2c0, pod2c1, pod2c2 ]\n" +
			"pod2c5: [ pod2c0, pod2c2, pod2c3 ]\n",
	}})
	requireDisjoint(t, e.nodes("pod2c4"), e.nodes("pod2c0"), e.nodes("pod2c1"), e.nodes("pod2c2"))
	requireDisjoint(t, e.nodes("pod2c5"), e.nodes("pod2c0"), e.nodes("pod2c2"), e.nodes("pod2c3"))
}

// TestE2eishN4c16Test04AvailableResources is
// n4c16/test04-available-resources: the policy uses only available CPUs.
func TestE2eishN4c16Test04AvailableResources(t *testing.T) {
	e := newE2eish(t, n4c16)

	// An explicit cpuset of available CPUs.
	e.launch(e2eConfig(func(c *cfgapi.Config) {
		c.ReservedResources = cfgapi.Constraints{cfgapi.CPU: "cpuset:11"}
		c.AvailableResources = cfgapi.Constraints{cfgapi.CPU: "cpuset:4-7,8-11"}
	}))

	// pod0 gets exclusive CPUs.
	e.createGuaranteed(podSpec{CPU: "3"})
	require.Equal(t, cpuIds(4, 5, 6), e.cpus("pod0c0"))
	require.Equal(t, nodeIds(1), e.mems("pod0c0"))

	// pod1 gets shared CPUs.
	e.createGuaranteed(podSpec{ContCount: 2, CPU: "980m"})
	require.Equal(t, cpuIds(8, 9, 10), e.cpus("pod1c0"))
	require.Equal(t, cpuIds(8, 9, 10), e.cpus("pod1c1"))
	require.Equal(t, nodeIds(2), e.mems("pod1c0"))
	require.Equal(t, nodeIds(2), e.mems("pod1c1"))
	e.deleteAllPods()

	// Excluded CPUs, with unlimited burstable containers on all of the
	// machine.
	e.launch(e2eConfig(func(c *cfgapi.Config) {
		c.ReservedResources = cfgapi.Constraints{cfgapi.CPU: "cpuset:11-15"}
		c.AvailableResources = cfgapi.Constraints{cfgapi.CPU: "exclude-cpuset:0-1"}
		c.UnlimitedBurstable = cfgapi.CPUTopologyLevelSystem
	}))

	// pod2 is burstable without a CPU limit.
	e.createBurstable(podSpec{CPUReq: "250m", CPULim: "0"})
	require.Equal(t, cpuIds(2, 3, 4, 5, 6, 7, 8, 9, 10), e.cpus("pod2c0"))
	require.Equal(t, nodeIds(0, 1, 2, 3), e.mems("pod2c0"))
}

// TestE2eishN4c16Test05ReservedResources is n4c16/test05-reserved-resources:
// kube-system containers run on reserved CPUs, reserved CPUs are allocated
// and released, and a container needing more than the reserved capacity
// goes to shared CPUs.
func TestE2eishN4c16Test05ReservedResources(t *testing.T) {
	e := newE2eish(t, n4c16)

	config := func(reserved cfgapi.Amount) *cfgapi.Config {
		return e2eConfig(func(c *cfgapi.Config) {
			c.ReservedResources = cfgapi.Constraints{cfgapi.CPU: reserved}
			c.AvailableResources = cfgapi.Constraints{cfgapi.CPU: "cpuset:4-7,8-13"}
		})
	}

	// Reserved CPUs are not a subset of the available ones.
	e.launchFails(config("cpuset:3,7,11,15"))

	// More CPUs are reserved than available.
	e.launchFails(config("11"))

	// Best-effort containers may use both reserved CPUs of a node.
	e.launch(config("cpuset:10-11"))
	e.createBestEffort(podSpec{Namespace: "kube-system", ContCount: 3})
	require.Equal(t, cpuIds(10, 11), e.cpus("pod0c0"))
	require.Equal(t, cpuIds(10, 11), e.cpus("pod0c1"))
	require.Equal(t, cpuIds(10, 11), e.cpus("pod0c2"))
	e.deletePods("pod0")

	// Best-effort containers are pinned to reserved CPUs.
	e.launch(config("cpuset:7,11"))
	e.createBestEffort(podSpec{Namespace: "kube-system", ContCount: 4})
	for _, c := range []string{"pod1c0", "pod1c1", "pod1c2", "pod1c3"} {
		require.Equal(t, cpuIds(7, 11), e.cpus(c), c)
	}

	// Guaranteed kube-system containers are pinned to reserved CPUs.
	e.createGuaranteed(podSpec{Namespace: "kube-system", CPU: "200m", ContCount: 4})
	for _, c := range []string{"pod2c0", "pod2c1", "pod2c2", "pod2c3"} {
		require.Equal(t, cpuIds(7, 11), e.cpus(c), c)
	}

	// pod2 took 4 * 200m of reserved CPUs, balanced over both nodes, so
	// each node has at most 800m free but the root has 1200m. Reserved
	// CPUs are not isolated, so 1000m fits. Twice, to check release.
	for _, pod := range []string{"pod3", "pod4"} {
		e.createGuaranteed(podSpec{Namespace: "kube-system", CPU: "100m"})
		require.Equal(t, cpuIds(7, 11), e.cpus(pod+"c0"), pod)
		e.deletePods(pod)
	}

	// pod5 needs more than all reserved CPUs, so it runs on shared ones.
	e.createGuaranteed(podSpec{Namespace: "kube-system", CPU: "2"})
	require.Equal(t, cpuIds(4, 5, 6, 8, 9, 10, 12, 13), e.cpus("pod5c0"))
	e.deleteAllPods()

	// Reserving milli-CPUs reserves the first available CPUs, rounded up.
	e.resetCounters()
	e.launch(config("2250m"))
	e.createBestEffort(podSpec{Namespace: "kube-system", ContCount: 1})
	require.Equal(t, cpuIds(4, 5, 6), e.cpus("pod0c0"))
}

// TestE2eishN4c16Test07MixedAllocations is n4c16/test07-mixed-allocations:
// containers with both exclusive and shared CPUs.
func TestE2eishN4c16Test07MixedAllocations(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig())

	// Every container is in a single node, not in a socket or the root.
	singleNodes := func() {
		t.Helper()
		for _, c := range e.running() {
			require.Len(t, e.mems(c), 1, c)
		}
	}

	// pod0c0 and pod0c1 go to the shared pools of separate nodes.
	e.createGuaranteed(podSpec{ContCount: 2, CPU: "500m"})
	require.Len(t, e.mems("pod0c0"), 1)
	require.Len(t, e.mems("pod0c1"), 1)
	requireDisjoint(t, e.mems("pod0c0"), e.mems("pod0c1"))
	require.Len(t, e.cpus("pod0c0"), 4)
	require.Len(t, e.cpus("pod0c1"), 4)
	requireDisjoint(t, e.cpus("pod0c0"), e.cpus("pod0c1"))

	// pod1c0 gets the last free 4-CPU node. node0 has only 3 CPUs, as
	// cpu0 is reserved, so pod1c1 goes next to pod0c0 or pod0c1. It gets
	// an exclusive CPU there, leaving that one 3 shared CPUs.
	e.createGuaranteed(podSpec{ContCount: 2, CPU: "1500m"})
	singleNodes()
	requireDisjoint(t, e.mems("pod1c0"), e.mems("pod1c1"))
	require.True(t, len(e.cpus("pod0c0")) == 3 || len(e.cpus("pod0c1")) == 3)
	require.True(t, len(e.cpus("pod0c0")) == 4 || len(e.cpus("pod0c1")) == 4)
	require.Len(t, e.cpus("pod1c0"), 4)
	require.Len(t, e.cpus("pod1c1"), 4)
	require.Len(t, e.unshared("pod1c1"), 1)

	// pod2c0 goes to node0, which has the largest free shared pool. pod2c1
	// goes next to whichever of pod0c0 and pod0c1 is alone in its node.
	e.createGuaranteed(podSpec{ContCount: 2, CPU: "2400m"})
	singleNodes()
	require.Len(t, e.unshared("pod1c1"), 1)
	require.Len(t, e.cpus("pod2c0"), 3)
	require.Len(t, e.unshared("pod2c0"), 3)
	require.Len(t, e.cpus("pod2c1"), 4)
	require.Len(t, e.unshared("pod2c1"), 0)
	require.True(t, maps.Equal(e.mems("pod2c1"), e.mems("pod0c0")) ||
		maps.Equal(e.mems("pod2c1"), e.mems("pod0c1")))

	// pod3c0 opts out of shared CPUs, to get 2 exclusive CPUs and 400m
	// shared. The isolated preference should not matter. It only fits
	// next to pod1c0.
	e.createGuaranteed(podSpec{CPU: "2400m", Annotations: map[string]string{
		"prefer-shared-cpus.resource-policy.nri.io/pod":   "false",
		"prefer-isolated-cpus.resource-policy.nri.io/pod": "true",
	}})
	singleNodes()
	require.Equal(t, e.mems("pod1c0"), e.mems("pod3c0"))
	require.Len(t, e.unshared("pod1c0"), 1)
	require.Len(t, e.unshared("pod3c0"), 2)

	// pod4 replaces pod3, opting out of isolated CPUs too. That does not
	// stop it from getting exclusive ones.
	e.deletePods("pod3")
	e.createGuaranteed(podSpec{CPU: "2400m", Annotations: map[string]string{
		"prefer-shared-cpus.resource-policy.nri.io/pod":   "false",
		"prefer-isolated-cpus.resource-policy.nri.io/pod": "false",
	}})
	singleNodes()
	require.Equal(t, e.mems("pod1c0"), e.mems("pod4c0"))
	require.Len(t, e.unshared("pod1c0"), 1)
	require.Len(t, e.unshared("pod4c0"), 2)

	// Deleting pod1 gives the shared pool of pod0c0 or pod0c1 back its
	// fourth CPU.
	e.deletePods("pod1")
	require.Len(t, e.cpus("pod0c0"), 4)
	require.Len(t, e.cpus("pod0c1"), 4)

	// pod5 replaces pod1, opting in to shared CPUs.
	e.createGuaranteed(podSpec{ContCount: 2, CPU: "1500m", Annotations: map[string]string{
		"prefer-shared-cpus.resource-policy.nri.io/pod": "true",
	}})
	singleNodes()
	require.True(t, maps.Equal(e.mems("pod5c0"), e.mems("pod0c0")) ||
		maps.Equal(e.mems("pod5c0"), e.mems("pod0c1")))
	require.Len(t, e.cpus("pod5c0"), 4)
	require.Len(t, e.cpus("pod0c0"), 4)
	require.Len(t, e.cpus("pod0c1"), 4)
	require.Equal(t, e.mems("pod4c0"), e.mems("pod5c1"))
	require.Len(t, e.cpus("pod5c1"), 2)
	require.Len(t, e.unshared("pod5c0"), 0)
	require.Len(t, e.unshared("pod5c1"), 0)
}

// TestE2eishN4c16Test09ContainerExit is n4c16/test09-container-exit:
// resources follow a container through a crash, a restart and an exit.
func TestE2eishN4c16Test09ContainerExit(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig())

	e.createGuaranteed(podSpec{CPU: "1000m", Mem: "64M"})
	require.Len(t, e.cpus("pod0c0"), 1)
	require.True(t, e.hasGrant("pod0c0"))

	// pod0c0 crashes and is restarted.
	e.restartContainer("pod0c0")
	require.Len(t, e.cpus("pod0c0"), 1)
	require.True(t, e.hasGrant("pod0c0"))
	require.Len(t, e.policy.allocations.grants, 1)

	// pod0c0 exits and completes.
	e.exitContainer("pod0c0")
	require.False(t, e.hasGrant("pod0c0"))
	require.Empty(t, e.policy.allocations.grants)
}

// TestE2eishN4c16Test10AdditionalReservedNamespaces is
// n4c16/test10-additional-reserved-namespaces: containers in the namespaces
// of ReservedPoolNamespaces run on reserved CPUs, unless annotated otherwise.
func TestE2eishN4c16Test10AdditionalReservedNamespaces(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig(func(c *cfgapi.Config) {
		c.ReservedPoolNamespaces = []string{"reserved-pool", "reserved-*", "foobar"}
		c.AvailableResources = cfgapi.Constraints{cfgapi.CPU: "cpuset:8-11"}
		c.ReservedResources = cfgapi.Constraints{cfgapi.CPU: "cpuset:10-11"}
	}))

	e.createBestEffort(podSpec{Namespace: "kube-system"})
	e.createBestEffort(podSpec{})
	require.Equal(t, cpuIds(10, 11), e.cpus("pod0c0"))
	require.Equal(t, cpuIds(8, 9), e.cpus("pod1c0"))
	e.deleteAllPods()

	// Containers in a reserved namespace go to reserved CPUs, unless
	// annotated out of them.
	e.createBestEffort(podSpec{Namespace: "foobar"})
	e.createBestEffort(podSpec{Namespace: "foobar", Annotations: map[string]string{
		"prefer-reserved-cpus.resource-policy.nri.io/pod": "false",
	}})
	e.createBestEffort(podSpec{Namespace: "foobar", Annotations: map[string]string{
		"prefer-reserved-cpus.resource-policy.nri.io/pod": "true",
	}})
	require.Equal(t, cpuIds(10, 11), e.cpus("pod2c0"))
	require.Equal(t, cpuIds(8, 9), e.cpus("pod3c0"))
	require.Equal(t, cpuIds(10, 11), e.cpus("pod4c0"))
}

// TestE2eishN4c16Test11ReservedCpuAnnotations is
// n4c16/test11-reserved-cpu-annotations: the prefer-reserved-cpus,
// cpu.preserve and memory.preserve annotations.
func TestE2eishN4c16Test11ReservedCpuAnnotations(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig(func(c *cfgapi.Config) {
		c.AvailableResources = cfgapi.Constraints{cfgapi.CPU: "cpuset:8-11"}
		c.ReservedResources = cfgapi.Constraints{cfgapi.CPU: "cpuset:10-11"}
	}))

	e.createGuaranteed(podSpec{Annotations: map[string]string{
		"prefer-reserved-cpus.resource-policy.nri.io/pod": "true",
	}})
	require.Equal(t, cpuIds(10, 11), e.cpus("pod0c0"))

	e.createGuaranteed(podSpec{Annotations: map[string]string{
		"prefer-reserved-cpus.resource-policy.nri.io/container.special": "false",
	}})
	require.Equal(t, cpuIds(8), e.cpus("pod1c0"))

	e.deletePods("pod0")
	e.createGuaranteed(podSpec{Annotations: map[string]string{
		"prefer-reserved-cpus.resource-policy.nri.io/pod": "true",
		"hide-hyperthreads.resource-policy.nri.io/pod":    "true",
	}})
	require.Equal(t, cpuIds(10), e.cpus("pod2c0"))

	// Preserved CPUs or memory are left unpinned.
	e.createGuaranteed(podSpec{ContCount: 4, CPU: "100m", Mem: "100M", Annotations: map[string]string{
		"cpu.preserve.resource-policy.nri.io":                     "true",
		"memory.preserve.resource-policy.nri.io/container.pod3c1": "true",
		"memory.preserve.resource-policy.nri.io/container.pod3c2": "true",
		"cpu.preserve.resource-policy.nri.io/container.pod3c2":    "false",
		"cpu.preserve.resource-policy.nri.io/container.pod3c3":    "false",
		"memory.preserve.resource-policy.nri.io/container.pod3c3": "false",
	}})
	require.Len(t, e.cpus("pod3c0"), 16)
	require.Len(t, e.mems("pod3c0"), 4)
	require.Len(t, e.cpus("pod3c1"), 16)
	require.Len(t, e.mems("pod3c1"), 4)
	require.Len(t, e.cpus("pod3c2"), 1)
	require.Len(t, e.mems("pod3c2"), 4)
	require.Len(t, e.cpus("pod3c3"), 1)
	require.Len(t, e.mems("pod3c3"), 1)
}

// TestE2eishN4c16Test14Burstable is n4c16/test14-burstable.
func TestE2eishN4c16Test14Burstable(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig(func(c *cfgapi.Config) { c.ColocatePods = true }))

	// The memory limits add up to more than the machine has. The pods fit
	// only if memory is allocated by request.
	e.createBurstable(podSpec{CPUReq: "1", CPULim: "2", MemReq: "100M", MemLim: "2.5G"})
	e.createBurstable(podSpec{CPUReq: "1", CPULim: "2", MemReq: "200M", MemLim: "5G"})
	e.createBurstable(podSpec{CPUReq: "1", CPULim: "2", MemReq: "100M", MemLim: "2.5G"})
	e.createBurstable(podSpec{CPUReq: "1", CPULim: "2", MemReq: "200M", MemLim: "0"})

	// The limits of pod0 and pod2 need 2 nodes, that of pod1 all 4. pod3
	// is unlimited, so it gets all nodes too.
	require.Len(t, e.nodes("pod0c0"), 2)
	require.Len(t, e.nodes("pod1c0"), 4)
	require.Len(t, e.nodes("pod2c0"), 2)
	require.Len(t, e.nodes("pod3c0"), 4)
	e.deleteAllPods()

	e.launch(e2eConfig())

	// Limited burstable containers go to the lowest pool with capacity for
	// their limit, here a socket.
	e.createBurstable(podSpec{ContCount: 2, CPUReq: "2", CPULim: "5", MemReq: "100M"})
	require.Equal(t, nodeIds(2, 3), e.nodes("pod4c0"))
	require.Equal(t, nodeIds(0, 1), e.nodes("pod4c1"))

	// Unlimited burstable containers go to a socket by default.
	e.createBurstable(podSpec{ContCount: 2, CPUReq: "2", CPULim: "0", MemReq: "100M"})
	for _, c := range []string{"pod5c0", "pod5c1"} {
		nodes := e.nodes(c)
		require.True(t, maps.Equal(nodes, nodeIds(0, 1)) || maps.Equal(nodes, nodeIds(2, 3)),
			"%s in nodes %s", c, nodes)
	}
	e.deleteAllPods()

	// Annotations override the default unlimited burstable level.
	e.createBurstable(podSpec{ContCount: 2, CPUReq: "2", CPULim: "0", MemReq: "100M",
		Annotations: map[string]string{
			"unlimited-burstable.resource-policy.nri.io/container.pod6c0": "numa",
			"unlimited-burstable.resource-policy.nri.io/container.pod6c1": "system",
		}})
	require.Len(t, e.nodes("pod6c0"), 1)
	require.Len(t, e.nodes("pod6c1"), 4)
	e.deleteAllPods()

	// A level the machine lacks gets the next level up.
	e.createBurstable(podSpec{ContCount: 2, CPUReq: "2", CPULim: "0", MemReq: "100M",
		Annotations: map[string]string{
			"unlimited-burstable.resource-policy.nri.io/container.pod7c0": "numa",
			"unlimited-burstable.resource-policy.nri.io/container.pod7c1": "die",
		}})
	require.Len(t, e.nodes("pod7c0"), 1)
	require.Len(t, e.nodes("pod7c1"), 2)
	e.deleteAllPods()

	// Unannotated containers follow the configured level.
	e.launch(e2eConfig(func(c *cfgapi.Config) {
		c.UnlimitedBurstable = cfgapi.CPUTopologyLevelNuma
	}))
	e.createBurstable(podSpec{ContCount: 2, CPUReq: "2", CPULim: "0", MemReq: "100M",
		Annotations: map[string]string{
			"unlimited-burstable.resource-policy.nri.io/container.pod8c1": "die",
		}})
	require.Len(t, e.nodes("pod8c0"), 1)
	require.Len(t, e.nodes("pod8c1"), 2)
}

// TestE2eishN4c16Test14GlobalSharedPreference is
// n4c16/test14-global-shared-preference.
func TestE2eishN4c16Test14GlobalSharedPreference(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig(func(c *cfgapi.Config) {
		prefer := true
		c.PreferShared = &prefer
	}))

	// Unannotated guaranteed containers get shared CPUs.
	e.createGuaranteed(podSpec{CPU: "1"})
	require.Greater(t, len(e.cpus("pod0c0")), 1)
	e.createGuaranteed(podSpec{CPU: "2"})
	require.Greater(t, len(e.cpus("pod1c0")), 1)
	e.deleteAllPods()

	// Annotated containers opt out of shared CPUs.
	optOut := map[string]string{"prefer-shared-cpus.resource-policy.nri.io/pod": "false"}
	e.createGuaranteed(podSpec{CPU: "1", Annotations: optOut})
	require.Len(t, e.cpus("pod2c0"), 1)
	e.createGuaranteed(podSpec{CPU: "2", Annotations: optOut})
	require.Len(t, e.cpus("pod3c0"), 2)
}

// TestE2eishN4c16Test15BusySharedPools is n4c16/test15-busy-shared-pools: a
// guaranteed container the size of a node must not exhaust the shared pool
// of a node with a container in it, so it is sliced from a socket.
func TestE2eishN4c16Test15BusySharedPools(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig())

	// Best-effort containers, one per node.
	e.createBestEffort(podSpec{ContCount: 4})
	e.createGuaranteed(podSpec{CPU: "4", Mem: "100M"})
	requireDisjoint(t, e.cpus("pod0c0"), e.cpus("pod0c1"), e.cpus("pod0c2"), e.cpus("pod0c3"),
		e.cpus("pod1c0"))
	e.deleteAllPods()

	// Burstable containers which look best-effort by their CPU request.
	e.createBurstable(podSpec{ContCount: 4, CPUReq: "1m", CPULim: "750m"})
	e.createGuaranteed(podSpec{CPU: "4", Mem: "100M"})
	requireDisjoint(t, e.cpus("pod2c0"), e.cpus("pod2c1"), e.cpus("pod2c2"), e.cpus("pod2c3"),
		e.cpus("pod3c0"))
	e.deleteAllPods()

	// Burstable containers with a non-zero CPU request.
	e.createBurstable(podSpec{ContCount: 4, CPUReq: "250m", CPULim: "750m"})
	e.createGuaranteed(podSpec{CPU: "4", Mem: "100M"})
	requireDisjoint(t, e.cpus("pod4c0"), e.cpus("pod4c1"), e.cpus("pod4c2"), e.cpus("pod4c3"),
		e.cpus("pod5c0"))
}

// TestE2eishN4c16Test16IdleSharedPools is n4c16/test16-idle-shared-pools:
// the shared pool of an idle node may be sliced empty.
func TestE2eishN4c16Test16IdleSharedPools(t *testing.T) {
	e := newE2eish(t, n4c16)
	e.launch(e2eConfig())

	e.createGuaranteed(podSpec{ContCount: 3, CPU: "4", Mem: "100M"})
	requireDisjoint(t, e.cpus("pod0c0"), e.cpus("pod0c1"), e.cpus("pod0c2"))
	requireDisjoint(t, e.nodes("pod0c0"), e.nodes("pod0c1"), e.nodes("pod0c2"))
	for _, c := range []string{"pod0c0", "pod0c1", "pod0c2"} {
		require.Len(t, e.cpus(c), 4, c)
		require.Len(t, e.nodes(c), 1, c)
	}

	// Best-effort and burstable containers go to the only pool with free
	// CPUs left.
	e.createBestEffort(podSpec{ContCount: 4})
	require.Equal(t, e.cpus("pod1c0"), e.cpus("pod1c1"))
	require.Equal(t, e.cpus("pod1c0"), e.cpus("pod1c2"))
	require.Equal(t, e.cpus("pod1c0"), e.cpus("pod1c3"))

	e.createBurstable(podSpec{ContCount: 4, CPUReq: "100m", CPULim: "150m"})
	require.Equal(t, e.cpus("pod2c0"), e.cpus("pod2c1"))
	require.Equal(t, e.cpus("pod2c0"), e.cpus("pod2c2"))
	require.Equal(t, e.cpus("pod2c0"), e.cpus("pod2c3"))
	require.Equal(t, e.cpus("pod2c0"), e.cpus("pod1c0"))
	requireDisjoint(t, e.nodes("pod0c0"), e.nodes("pod0c1"), e.nodes("pod0c2"), e.nodes("pod1c0"))
	e.deleteAllPods()

	// The other way around: best-effort containers first, then a
	// guaranteed one gets a full node.
	e.createBestEffort(podSpec{ContCount: 2})
	requireDisjoint(t, e.cpus("pod3c0"), e.cpus("pod3c1"))

	e.createGuaranteed(podSpec{CPU: "4", Mem: "100M"})
	requireDisjoint(t, e.cpus("pod4c0"), e.cpus("pod3c0"), e.cpus("pod3c1"))
	require.Len(t, e.cpus("pod4c0"), 4)
	require.Len(t, e.nodes("pod4c0"), 1)
	requireDisjoint(t, e.nodes("pod3c0"), e.nodes("pod3c1"), e.nodes("pod4c0"))
}

// TestE2eishN4c16Test18StrictAlignment is n4c16/test18-strict-alignment:
// strict topology hints are obeyed or allocation fails.
func TestE2eishN4c16Test18StrictAlignment(t *testing.T) {
	// The e2e deployment enables test hints.
	t.Setenv(utils.EnvVarEnableTestAPIs, "1")

	e := newE2eish(t, n4c16)
	e.launch(e2eConfig())

	// hints gives the annotations of a strict test hint for a node of the
	// container, followed by key-value pairs of extra annotations.
	hints := func(ctr, numa string, extra ...string) map[string]string {
		ann := map[string]string{
			"strict.topologyhints.resource-policy.nri.io/container." + ctr: "true",
			"test.topologyhints.resource-policy.nri.io/container." + ctr: `{ test: { NUMAs: "` +
				numa + `" } } }`,
		}
		for i := 0; i < len(extra); i += 2 {
			ann[extra[i]] = extra[i+1]
		}
		return ann
	}

	e.createGuaranteed(podSpec{CPU: "1", Mem: "100M", Annotations: hints("pod0c0", "2")})
	require.Subset(t, cpuIds(8, 9, 10, 11), e.cpus("pod0c0"))
	require.Equal(t, nodeIds(2), e.nodes("pod0c0"))

	e.createGuaranteed(podSpec{CPU: "1", Mem: "100M", Annotations: hints("pod1c0", "3")})
	require.Subset(t, cpuIds(12, 13, 14, 15), e.cpus("pod1c0"))
	require.Equal(t, nodeIds(3), e.nodes("pod1c0"))

	e.createGuaranteed(podSpec{CPU: "1", Mem: "100M", Annotations: hints("pod2c0", "2")})
	require.Subset(t, cpuIds(8, 9, 10, 11), e.cpus("pod2c0"))
	require.Equal(t, nodeIds(2), e.nodes("pod2c0"))

	// Not enough CPUs are left in node 2.
	e.createGuaranteed(podSpec{CPU: "3", Mem: "100M", Annotations: hints("pod3c0", "2"),
		WantError: "fail strict hint"})

	// This would fit, but there are no isolated CPUs.
	e.createGuaranteed(podSpec{CPU: "1", Mem: "100M", WantError: "isolated CPUs",
		Annotations: hints("pod4c0", "2",
			"require-isolated-cpus.resource-policy.nri.io/container.pod4c0", "true")})

	// A preference for isolated CPUs is not a requirement.
	e.createGuaranteed(podSpec{CPU: "1", Mem: "100M",
		Annotations: hints("pod5c0", "2",
			"prefer-isolated-cpus.resource-policy.nri.io/container.pod5c0", "true")})
	require.Subset(t, cpuIds(8, 9, 10, 11), e.cpus("pod5c0"))
	require.Equal(t, nodeIds(2), e.nodes("pod5c0"))
	e.deletePods("pod5")

	// pod4 again, with a pod-scoped preference under the requirement.
	e.createGuaranteed(podSpec{CPU: "1", Mem: "100M", WantError: "isolated CPUs",
		Annotations: hints("pod6c0", "2",
			"prefer-isolated-cpus.resource-policy.nri.io/pod", "false",
			"require-isolated-cpus.resource-policy.nri.io/container.pod6c0", "true")})

	// pod5 again, with a pod-scoped requirement under the preference.
	e.createGuaranteed(podSpec{CPU: "1", Mem: "100M",
		Annotations: hints("pod7c0", "2",
			"prefer-isolated-cpus.resource-policy.nri.io/container.pod7c0", "true",
			"require-isolated-cpus.resource-policy.nri.io/pod", "true")})
	require.Subset(t, cpuIds(8, 9, 10, 11), e.cpus("pod7c0"))
	require.Equal(t, nodeIds(2), e.nodes("pod7c0"))
}

// TestE2eishN4c16Test20Coldstart is n4c16/test20-coldstart: a container with
// a cold start period gets PMEM only at first, and DRAM too once the period
// is over. The memory type overrides of the e2e helm config make node 2 DRAM
// and node 3 PMEM.
func TestE2eishN4c16Test20Coldstart(t *testing.T) {
	if e2eReexec(t, `OVERRIDE_SYS_MEMORY_TYPE={"2": "DRAM", "3": "PMEM"}`) {
		return
	}

	e := newE2eish(t, n4c16)
	e.launch(e2eConfig())

	e.createGuaranteed(podSpec{ContCount: 2, CPU: "1", Annotations: map[string]string{
		"memory-type.resource-policy.nri.io/container.pod0c1": "DRAM,PMEM",
		"cold-start.resource-policy.nri.io/container.pod0c1":  "{ duration: 3s }",
	}})
	require.Equal(t, nodeIds(3), e.mems("pod0c1"))

	// The e2e test checks again after 5 seconds.
	require.Eventually(t, func() bool {
		return maps.Equal(nodeIds(2, 3), e.mems("pod0c1"))
	}, 5*time.Second, 100*time.Millisecond, "pod0c1 got no DRAM after its cold start")
}
