// Copyright 2020 Intel Corporation. All Rights Reserved.
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
	"io/ioutil"
	"os"
	"path"
	"testing"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils"
)

func findNodeWithID(id int, nodes []Node) Node {
	for _, node := range nodes {
		if node.NodeID() == id {
			return node
		}
	}
	panic("No node found with id " + fmt.Sprintf("%d", id))
}

func findNodeWithName(name string, nodes []Node) Node {
	for _, node := range nodes {
		if node.Name() == name {
			return node
		}
	}
	panic("No node found with name " + name)
}

func setLinks(nodes []Node, tree map[int][]int) {
	hasParent := map[int]struct{}{}
	for parent, children := range tree {
		parentNode := findNodeWithID(parent, nodes)
		for _, child := range children {
			childNode := findNodeWithID(child, nodes)
			childNode.LinkParent(parentNode)
			hasParent[child] = struct{}{}
		}
	}
	orphans := []int{}
	for id := range tree {
		if _, ok := hasParent[id]; !ok {
			node := findNodeWithID(id, nodes)
			node.LinkParent(nilnode)
			orphans = append(orphans, id)
		}
	}
	if len(orphans) != 1 {
		panic(fmt.Sprintf("expected one root node, got %d with IDs %v", len(orphans), orphans))
	}
}

func TestPoolCreation(t *testing.T) {

	// Test pool creation with "real" sysfs data.

	// Create a temporary directory for the test data.
	dir, err := ioutil.TempDir("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	// Uncompress the test data to the directory.
	err = utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir)
	if err != nil {
		panic(err)
	}

	tcases := []struct {
		path                    string
		name                    string
		req                     Request
		affinities              map[int]int32
		expectedRemainingNodes  []int
		expectedFirstNodeMemory memoryType
		expectedLeafNodeCPUs    int
		expectedRootNodeCPUs    int
		// TODO: expectedRootNodeMemory   int
	}{
		{
			path: path.Join(dir, "sysfs", "desktop", "sys"),
			name: "sysfs pool creation from a desktop system",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryAll,
				container: &mockContainer{},
			},
			expectedRemainingNodes:  []int{0},
			expectedFirstNodeMemory: memoryDRAM,
			expectedLeafNodeCPUs:    20,
			expectedRootNodeCPUs:    20,
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "sysfs pool creation from a server system",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryDRAM,
				container: &mockContainer{},
			},
			expectedRemainingNodes:  []int{0, 1, 2, 3, 4, 5, 6},
			expectedFirstNodeMemory: memoryDRAM | memoryPMEM,
			expectedLeafNodeCPUs:    28,
			expectedRootNodeCPUs:    112,
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "pmem request on a server system",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryDRAM | memoryPMEM,
				container: &mockContainer{},
			},
			expectedRemainingNodes:  []int{0, 1, 2, 3, 4, 5, 6},
			expectedFirstNodeMemory: memoryDRAM | memoryPMEM,
			expectedLeafNodeCPUs:    28,
			expectedRootNodeCPUs:    112,
		},
		{
			path: path.Join(dir, "sysfs", "4-socket-server-nosnc", "sys"),
			name: "sysfs pool creation from a 4 socket server with SNC disabled",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryAll,
				container: &mockContainer{},
			},
			expectedRemainingNodes:  []int{0, 1, 2, 3, 4},
			expectedFirstNodeMemory: memoryDRAM,
			expectedLeafNodeCPUs:    36,
			expectedRootNodeCPUs:    36 * 4,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			sys, err := system.DiscoverSystemAt(tc.path)
			if err != nil {
				panic(err)
			}

			policyOptions := &policyapi.BackendOptions{
				Cache:  &mockCache{},
				System: sys,
				Config: &cfgapi.Config{
					ReservedResources: cfgapi.Constraints{
						cfgapi.CPU: "750m",
					},
				},
			}

			log.EnableDebug(true)
			policy := New().(*policy)
			policy.Setup(policyOptions)
			log.EnableDebug(false)

			if policy.root.GetSupply().SharableCPUs().Size()+policy.root.GetSupply().IsolatedCPUs().Size()+policy.root.GetSupply().ReservedCPUs().Size() != tc.expectedRootNodeCPUs {
				t.Errorf("Expected %d CPUs, got %d", tc.expectedRootNodeCPUs,
					policy.root.GetSupply().SharableCPUs().Size()+policy.root.GetSupply().IsolatedCPUs().Size()+policy.root.GetSupply().ReservedCPUs().Size())
			}

			for _, p := range policy.pools {
				if p.IsLeafNode() {
					if len(p.Children()) != 0 {
						t.Errorf("Leaf node %v had %d children", p, len(p.Children()))
					}
					if p.GetSupply().SharableCPUs().Size()+p.GetSupply().IsolatedCPUs().Size()+p.GetSupply().ReservedCPUs().Size() != tc.expectedLeafNodeCPUs {
						t.Errorf("Expected %d CPUs, got %d (%s)", tc.expectedLeafNodeCPUs,
							p.GetSupply().SharableCPUs().Size()+p.GetSupply().IsolatedCPUs().Size()+p.GetSupply().ReservedCPUs().Size(),
							p.GetSupply().DumpCapacity())
					}
				}
			}

			scores, filteredPools := policy.sortPoolsByScore(tc.req, tc.affinities)
			fmt.Printf("scores: %v, remaining pools: %v\n", scores, filteredPools)

			if len(filteredPools) != len(tc.expectedRemainingNodes) {
				t.Errorf("Wrong number of nodes in the filtered pool: expected %d but got %d", len(tc.expectedRemainingNodes), len(filteredPools))
			}

			for _, id := range tc.expectedRemainingNodes {
				found := false
				for _, node := range filteredPools {
					if node.NodeID() == id {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Did not find id %d in filtered pools: %s", id, filteredPools)
				}
			}

			if len(filteredPools) > 0 && filteredPools[0].GetMemoryType() != tc.expectedFirstNodeMemory {
				t.Errorf("Expected first node memory type %v, got %v", tc.expectedFirstNodeMemory, filteredPools[0].GetMemoryType())
			}
		})
	}
}

func TestWorkloadPlacement(t *testing.T) {

	// Do some workloads (containers) and see how they are placed in the
	// server system.

	// Create a temporary directory for the test data.
	dir, err := ioutil.TempDir("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	// Uncompress the test data to the directory.
	err = utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir)
	if err != nil {
		panic(err)
	}

	tcases := []struct {
		path                   string
		name                   string
		req                    Request
		affinities             map[int]int32
		expectedRemainingNodes []int
		expectedLeafNode       bool
	}{
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "workload placement on a server system leaf node",
			req: &request{
				memReq:  10000,
				memLim:  10000,
				memType: memoryUnspec,
				isolate: false,
				full:    25, // 28 - 2 isolated = 26: but fully exhausting the shared CPU subpool is disallowed

				container: &mockContainer{},
			},
			expectedRemainingNodes: []int{0, 1, 2, 3, 4, 5, 6},
			expectedLeafNode:       true,
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "workload placement on a server system root node: CPUs don't fit to leaf",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      29,
				container: &mockContainer{},
			},
			expectedRemainingNodes: []int{0, 1, 2, 3, 4, 5, 6},
			expectedLeafNode:       false,
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "workload placement on a server system root node: memory doesn't fit to leaf",
			req: &request{
				memReq:    190000000000,
				memLim:    190000000000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      28,
				container: &mockContainer{},
			},
			expectedRemainingNodes: []int{2, 6},
			expectedLeafNode:       false,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			sys, err := system.DiscoverSystemAt(tc.path)
			if err != nil {
				panic(err)
			}

			policyOptions := &policyapi.BackendOptions{
				Cache:  &mockCache{},
				System: sys,
				Config: &cfgapi.Config{
					ReservedResources: cfgapi.Constraints{
						cfgapi.CPU: "750m",
					},
				},
			}

			log.EnableDebug(true)
			policy := New().(*policy)
			policy.Setup(policyOptions)
			log.EnableDebug(false)

			scores, filteredPools := policy.sortPoolsByScore(tc.req, tc.affinities)
			fmt.Printf("scores: %v, remaining pools: %v\n", scores, filteredPools)

			if len(filteredPools) != len(tc.expectedRemainingNodes) {
				t.Errorf("Wrong number of nodes in the filtered pool: expected %d but got %d", len(tc.expectedRemainingNodes), len(filteredPools))
			}

			for _, id := range tc.expectedRemainingNodes {
				found := false
				for _, node := range filteredPools {
					if node.NodeID() == id {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Did not find id %d in filtered pools: %s", id, filteredPools)
				}
			}
			if filteredPools[0].IsLeafNode() != tc.expectedLeafNode {
				t.Errorf("Workload should have been placed in a leaf node: %t", tc.expectedLeafNode)
			}
		})
	}
}

func TestAffinities(t *testing.T) {
	//
	// Test how (already pre-calculated) affinities affect workload placement.
	//

	// Create a temporary directory for the test data.
	dir, err := ioutil.TempDir("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	// Uncompress the test data to the directory.
	err = utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir)
	if err != nil {
		panic(err)
	}

	tcases := []struct {
		path       string
		name       string
		req        Request
		affinities map[string]int32
		expected   string
	}{
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "no affinities",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{},
			expected:   "NUMA node #2",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "reserved - no affinities",
			req: &request{
				cpuType:   cpuReserved,
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      0,
				container: &mockContainer{},
			},
			affinities: map[string]int32{},
			expected:   "NUMA node #0",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "affinity to NUMA node #1",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"NUMA node #1": 1,
			},
			expected: "NUMA node #1",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "affinity to socket #1",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"socket #1": 1,
			},
			expected: "socket #1",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "equal affinities to NUMA node #1, socket #1",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"socket #1":    1,
				"NUMA node #1": 1,
			},
			expected: "NUMA node #1",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "equal affinities to NUMA node #1, NUMA node #3",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"NUMA node #1": 1,
				"NUMA node #3": 1,
			},
			expected: "socket #1",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "double affinity to NUMA node #1 vs. #3",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"NUMA node #1": 2,
				"NUMA node #3": 1,
			},
			expected: "socket #1",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "triple affinity to NUMA node #1 vs. #3",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"NUMA node #1": 3,
				"NUMA node #3": 1,
			},
			expected: "NUMA node #1",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "double affinity to NUMA node #0,#3 vs. socket #1",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"NUMA node #0": 2,
				"NUMA node #3": 2,
				"socket #1":    1,
			},
			expected: "root",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "equal affinity to NUMA node #0,#3 vs. socket #1",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"NUMA node #0": 1,
				"NUMA node #3": 1,
				"socket #1":    1,
			},
			expected: "root",
		},
		{
			path: path.Join(dir, "sysfs", "server", "sys"),
			name: "half the affinity to NUMA node #0,#3 vs. socket #1",
			req: &request{
				memReq:    10000,
				memLim:    10000,
				memType:   memoryUnspec,
				isolate:   false,
				full:      3,
				container: &mockContainer{},
			},
			affinities: map[string]int32{
				"NUMA node #0": 1,
				"NUMA node #3": 1,
				"socket #1":    2,
			},
			expected: "socket #1",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			sys, err := system.DiscoverSystemAt(tc.path)
			if err != nil {
				panic(err)
			}

			policyOptions := &policyapi.BackendOptions{
				Cache:  &mockCache{},
				System: sys,
				Config: &cfgapi.Config{
					ReservedResources: cfgapi.Constraints{
						cfgapi.CPU: "750m",
					},
				},
			}

			log.EnableDebug(true)
			policy := New().(*policy)
			policy.Setup(policyOptions)
			log.EnableDebug(false)

			affinities := map[int]int32{}
			for name, weight := range tc.affinities {
				affinities[findNodeWithName(name, policy.pools).NodeID()] = weight
			}

			log.EnableDebug(true)
			scores, filteredPools := policy.sortPoolsByScore(tc.req, affinities)
			fmt.Printf("scores: %v, remaining pools: %v\n", scores, filteredPools)
			log.EnableDebug(false)

			if len(filteredPools) < 1 {
				t.Errorf("pool scoring failed to find any pools")
			}

			node := filteredPools[0]
			if node.Name() != tc.expected {
				t.Errorf("expected best pool %s, got %s", tc.expected, node.Name())
			}
		})
	}
}
