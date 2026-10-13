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
	"testing"

	"github.com/stretchr/testify/require"
)

// n4c128 is test/e2e/policies.test-suite/topology-aware/n4c128/topology.var.json.
var n4c128 = e2eTopology{{Mem: "2G", Cores: 16, Nodes: 2, Packages: 2}}

// TestE2eishN4c128Test19CacheClusters is n4c128/test19-cacheclusters:
// unlimited burstable containers confined to an L3 cache.
func TestE2eishN4c128Test19CacheClusters(t *testing.T) {
	// The extraEnv of the helm config of the e2e test: 16 L3 caches of 8
	// CPUs.
	e := newE2eishWithCaches(t, n4c128, `[
  {"cpusets": ["0-7","64-71" ], "level": 3, "size": "16M"},
  {"cpusets": ["8-15","72-79" ], "level": 3, "size": "16M"},
  {"cpusets": ["48-55","112-119" ], "level": 3, "size": "16M"},
  {"cpusets": ["56-63","120-127" ], "level": 3, "size": "16M"},
  {"cpusets": ["16-23","80-87" ], "level": 3, "size": "16M"},
  {"cpusets": ["24-31","88-95" ], "level": 3, "size": "16M"},
  {"cpusets": ["32-39","96-103" ], "level": 3, "size": "16M"},
  {"cpusets": ["40-47","104-111" ], "level": 3, "size": "16M"},
  {"cpusets": ["0-63"], "level": 2, "size": "4M"},
  {"cpusets": ["64-127"], "level": 2, "size": "4M"},
  {"cpusets": ["0-63"], "level": 1, "size": "1M", "kind": "d"},
  {"cpusets": ["0-63"], "level": 1, "size": "1M", "kind": "i"},
  {"cpusets": ["64-127"], "level": 1, "size": "128k", "kind": "d"},
  {"cpusets": ["64-127"], "level": 1, "size": "128k", "kind": "i"}
  ]
`)

	e.launch(e2eCustomConfig("750m"))

	l3cache := func(ctrs ...string) map[string]string {
		ann := map[string]string{}
		for _, c := range ctrs {
			ann["unlimited-burstable.resource-policy.nri.io/container."+c] = "l3cache"
		}
		return ann
	}

	// fillL3Caches creates pod<first> to pod<last>, one per L3 cache, each
	// bursting within its own L3 cache.
	fillL3Caches := func(first, last int, cpuReq string) {
		t.Helper()
		for i := first; i <= last; i++ {
			e.createBurstable(podSpec{CPUReq: cpuReq, CPULim: "0", MemReq: "50M",
				Annotations: l3cache(fmt.Sprintf("pod%dc0", i))})
		}
	}

	// requireL3CachesFilled checks that pod<first> to pod<last> take all
	// 127 usable CPUs in disjoint sets, one L3 cache each.
	requireL3CachesFilled := func(first, last int) {
		t.Helper()
		sets := []idSet{}
		for i := first; i <= last; i++ {
			sets = append(sets, e.cpus(fmt.Sprintf("pod%dc0", i)))
		}
		require.Len(t, union(sets...), 127)
		requireDisjoint(t, sets...)
	}

	// A container is confined to its L3 cache, the system or a package.
	ann := l3cache("pod0c0")
	ann["unlimited-burstable.resource-policy.nri.io/container.pod0c1"] = "system"
	ann["unlimited-burstable.resource-policy.nri.io/container.pod0c2"] = "package"
	e.createBurstable(podSpec{ContCount: 3, CPUReq: "1500m", CPULim: "0", MemReq: "100M",
		Annotations: ann})
	require.Len(t, e.cpus("pod0c0"), 8)
	require.Len(t, e.cpus("pod0c1"), 127)
	require.Len(t, e.cpus("pod0c2"), 64)
	e.deleteAllPods()

	// A request larger than an L3 cache promotes the container to its NUMA
	// node, which has 31 CPUs for cpu0 is reserved.
	e.createBurstable(podSpec{CPUReq: "12", CPULim: "0", MemReq: "100M",
		Annotations: l3cache("pod1c0")})
	require.Len(t, e.cpus("pod1c0"), 31)
	e.deleteAllPods()

	// So does a limit larger than an L3 cache.
	e.createBurstable(podSpec{CPUReq: "4", CPULim: "20", MemReq: "100M",
		Annotations: l3cache("pod2c0")})
	require.Len(t, e.cpus("pod2c0"), 32)
	e.deleteAllPods()

	// A limit which fits keeps the container in the L3 cache.
	e.createBurstable(podSpec{CPUReq: "4", CPULim: "8", MemReq: "100M",
		Annotations: l3cache("pod3c0")})
	require.Len(t, e.cpus("pod3c0"), 8)
	e.deleteAllPods()

	// The containers of a pod go to separate L3 caches.
	e.createBurstable(podSpec{ContCount: 2, CPUReq: "2", CPULim: "0", MemReq: "100M",
		Annotations: l3cache("pod4c0", "pod4c1")})
	require.Len(t, e.cpus("pod4c0"), 8)
	require.Len(t, e.cpus("pod4c1"), 8)
	requireDisjoint(t, e.cpus("pod4c0"), e.cpus("pod4c1"))
	e.deleteAllPods()

	// Affinity puts them in the same L3 cache.
	ann = l3cache("pod5c0", "pod5c1")
	ann["resource-policy.nri.io/affinity"] = "pod5c0: [ pod5c1 ]\n"
	e.createBurstable(podSpec{ContCount: 2, CPUReq: "2", CPULim: "0", MemReq: "100M",
		Annotations: ann})
	require.Len(t, e.cpus("pod5c0"), 8)
	require.Len(t, e.cpus("pod5c1"), 8)
	require.Equal(t, e.cpus("pod5c0"), e.cpus("pod5c1"))
	e.deleteAllPods()

	// Containers of separate pods spread over L3 caches.
	fillL3Caches(6, 9, "2")
	for _, c := range []string{"pod6c0", "pod7c0", "pod8c0", "pod9c0"} {
		require.Len(t, e.cpus(c), 8, c)
	}
	requireDisjoint(t, e.cpus("pod6c0"), e.cpus("pod7c0"), e.cpus("pod8c0"), e.cpus("pod9c0"))
	e.deleteAllPods()

	// With every L3 cache taken, the next container goes to the least
	// occupied one.
	fillL3Caches(10, 24, "4")
	fillL3Caches(25, 25, "1")
	requireL3CachesFilled(10, 25)
	fillL3Caches(26, 26, "2")
	require.Equal(t, e.cpus("pod25c0"), e.cpus("pod26c0"))
	e.deleteAllPods()

	// With no L3 cache left with room, the container goes to a NUMA node,
	// the one with the lowest ID first.
	fillL3Caches(27, 42, "7")
	requireL3CachesFilled(27, 42)
	fillL3Caches(43, 43, "3")
	require.Len(t, e.cpus("pod43c0"), 31)
	fillL3Caches(44, 44, "3")
	require.Len(t, e.cpus("pod44c0"), 32)
	e.deleteAllPods()

	// Exclusive CPUs of guaranteed containers shrink the L3 caches of
	// burstable ones, and grow them back once released.
	fillL3Caches(45, 60, "2")
	requireL3CachesFilled(45, 60)
	require.Len(t, e.cpus("pod45c0"), 8)

	e.createGuaranteed(podSpec{CPU: "4", Mem: "100M"})
	require.Len(t, e.cpus("pod61c0"), 4)
	require.Len(t, e.cpus("pod45c0"), 4)
	requireDisjoint(t, e.cpus("pod45c0"), e.cpus("pod61c0"))

	// pod62c0 spreads over two L3 caches, those of pod50c0 and pod51c0.
	e.createGuaranteed(podSpec{CPU: "8", Mem: "100M"})
	require.Len(t, e.cpus("pod62c0"), 8)
	require.Len(t, e.cpus("pod50c0"), 2)
	require.Len(t, e.cpus("pod51c0"), 6)

	e.deletePods("pod61", "pod62")
	require.Len(t, e.cpus("pod45c0"), 8)
	require.Len(t, e.cpus("pod50c0"), 8)
	require.Len(t, e.cpus("pod51c0"), 8)
}
