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
	"testing"

	"github.com/stretchr/testify/require"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
)

// n6HbmCxl is test/e2e/policies.test-suite/topology-aware/n6-hbm-cxl/topology.var.json:
// a DRAM node with CPUs per package, and per package an HBM and a PMEM node
// without CPUs.
var n6HbmCxl = e2eTopology{
	{Mem: "2G", Threads: 2, Cores: 2, Nodes: 1, Packages: 2},
	{Mem: "1G", NodeDist: map[int]int{0: 15, 1: 30, 2: 10, 3: 35}},
	{Mem: "1G", NodeDist: map[int]int{0: 30, 1: 15, 2: 35, 3: 10}},
	{Mem: "4G", NodeDist: map[int]int{0: 60, 1: 70, 2: 62, 3: 72, 4: 10, 5: 75}},
	{Mem: "4G", NodeDist: map[int]int{0: 70, 1: 60, 2: 72, 3: 62, 4: 75, 5: 10}},
}

// n6MemoryNodes is the node of each memory type in each package, as in
// n6-hbm-cxl/py_consts.var.py.
var n6MemoryNodes = map[string]map[string]int{
	"package0": {"dram": 0, "hbm": 2, "pmem": 4},
	"package1": {"dram": 1, "hbm": 3, "pmem": 5},
}

// requireLocalMems is local_mems(): the container uses exactly the given
// memory types of its own package.
func requireLocalMems(t *testing.T, e *e2eish, ctr string, types ...string) {
	t.Helper()

	pkgs := e.packages(ctr)
	require.Len(t, pkgs, 1, "%s runs in packages %s", ctr, pkgs)
	ids := []int{}
	for pkg := range pkgs {
		for _, kind := range types {
			ids = append(ids, n6MemoryNodes[pkg][kind])
		}
	}
	require.Equal(t, nodeIds(ids...), e.mems(ctr), "%s memory of %s in %s", types, ctr, pkgs)
}

// TestE2eishN6HbmCxlTest01MemoryTypes is n6-hbm-cxl/test01-memory-types.
func TestE2eishN6HbmCxlTest01MemoryTypes(t *testing.T) {
	e := newE2eish(t, n6HbmCxl)
	e.launch(e2eConfig())

	// pod0c0 has the default types, DRAM and PMEM.
	e.createGuaranteed(podSpec{ContCount: 4, CPU: "100m", Mem: "512M", Annotations: map[string]string{
		"memory-type.resource-policy.nri.io/container.pod0c1": "dram",
		"memory-type.resource-policy.nri.io/container.pod0c2": "hbm",
		"memory-type.resource-policy.nri.io/container.pod0c3": "pmem",
	}})
	requireLocalMems(t, e, "pod0c0", "dram", "pmem")
	requireLocalMems(t, e, "pod0c1", "dram")
	requireLocalMems(t, e, "pod0c2", "hbm")
	requireLocalMems(t, e, "pod0c3", "pmem")

	// Memory left behind in HBM or DRAM would break the next step.
	e.deletePods("pod0")

	e.createGuaranteed(podSpec{ContCount: 4, CPU: "100m", Mem: "2816M", Annotations: map[string]string{
		"memory-type.resource-policy.nri.io/container.pod1c0": "hbm,dram",
		"memory-type.resource-policy.nri.io/container.pod1c1": "hbm,dram",
		"memory-type.resource-policy.nri.io/container.pod1c2": "pmem",
		"memory-type.resource-policy.nri.io/container.pod1c3": "pmem",
	}})
	requireLocalMems(t, e, "pod1c0", "hbm", "dram")
	requireLocalMems(t, e, "pod1c1", "hbm", "dram")
	requireLocalMems(t, e, "pod1c2", "pmem")
	requireLocalMems(t, e, "pod1c3", "pmem")

	// 2.0G more PMEM fits in no single PMEM node. libmem resolves the
	// overflow by spreading the lowest priority request, the smallest one
	// of pod2c0, over both PMEM nodes.
	e.createGuaranteed(podSpec{CPU: "100m", Mem: "2G", Annotations: map[string]string{
		"memory-type.resource-policy.nri.io/container.pod2c0": "pmem",
	}})
	requireLocalMems(t, e, "pod1c0", "hbm", "dram")
	requireLocalMems(t, e, "pod1c1", "hbm", "dram")
	requireLocalMems(t, e, "pod1c2", "pmem")
	requireLocalMems(t, e, "pod1c3", "pmem")
	require.Equal(t, nodeIds(4, 5), e.mems("pod2c0"))
}

// TestE2eishN6HbmCxlTest04Nrt is n6-hbm-cxl/test04-nrt: the topology zones
// the policy exports for NodeResourceTopology.
func TestE2eishN6HbmCxlTest04Nrt(t *testing.T) {
	e := newE2eish(t, n6HbmCxl)
	e.launch(e2eCustomConfig("cpuset:3"))

	zones := map[string]*policyapi.TopologyZone{}
	for _, z := range e.policy.GetTopologyZones() {
		zones[z.Name] = z
	}
	attr := func(zone, name string) string {
		t.Helper()
		z, ok := zones[zone]
		require.True(t, ok, "no zone %q", zone)
		for _, a := range z.Attributes {
			if a.Name == name {
				return a.Value
			}
		}
		require.Failf(t, "missing attribute", "zone %q has no %q", zone, name)
		return ""
	}

	require.Equal(t, "0,2,4", attr("socket #0", policyapi.MemsetAttribute))
	require.Equal(t, "0-2", attr("socket #0", policyapi.SharedCPUsAttribute))
	require.Equal(t, "3", attr("socket #0", policyapi.ReservedCPUsAttribute))

	require.Equal(t, "1,3,5", attr("socket #1", policyapi.MemsetAttribute))
	require.Equal(t, "4-7", attr("socket #1", policyapi.SharedCPUsAttribute))

	require.Equal(t, "0-5", attr("root", policyapi.MemsetAttribute))
	require.Equal(t, "0-2,4-7", attr("root", policyapi.SharedCPUsAttribute))
	require.Equal(t, "3", attr("root", policyapi.ReservedCPUsAttribute))
}
