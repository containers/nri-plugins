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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"testing"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/testutils"
)

func findNodeWithName(name string, nodes []Node) Node {
	for _, node := range nodes {
		if node.Name() == name {
			return node
		}
	}
	panic("No node found with name " + name)
}

var (
	sysfsOnce sync.Once
	sysfsDir  string
	sysfsErr  error
)

// testSysfs returns a read-only directory with the test sysfs data, shared by
// all tests.
func testSysfs(t *testing.T) string {
	t.Helper()
	sysfsOnce.Do(func() {
		// Uncompressing the data takes a while, so it is done only once.
		sysfsDir, sysfsErr = os.MkdirTemp("", "topology-aware-sysfs-")
		if sysfsErr == nil {
			sysfsErr = testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), sysfsDir)
		}
		// A test which writes to the shared data fails, so it cannot change
		// what the tests after it see.
		if sysfsErr == nil {
			sysfsErr = chmodTree(sysfsDir, 0o555, 0o444)
		}
	})
	if sysfsErr != nil {
		t.Fatalf("failed to prepare test sysfs data: %v", sysfsErr)
	}
	return sysfsDir
}

// chmodTree sets the mode of every directory under root to dirMode, and of
// every regular file to fileMode.
func chmodTree(root string, dirMode, fileMode fs.FileMode) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return os.Chmod(p, dirMode)
		case d.Type().IsRegular():
			return os.Chmod(p, fileMode)
		default:
			return nil
		}
	})
}

// TestMain runs the tests and removes the shared test sysfs data after them.
func TestMain(m *testing.M) {
	code := m.Run()
	if sysfsDir != "" {
		// Removing the files needs write access to their directories.
		if err := chmodTree(sysfsDir, 0o755, 0o444); err != nil {
			fmt.Fprintf(os.Stderr, "failed to make test sysfs data writable: %v\n", err)
		}
		if err := os.RemoveAll(sysfsDir); err != nil {
			fmt.Fprintf(os.Stderr, "failed to remove test sysfs data: %v\n", err)
		}
	}
	os.Exit(code)
}

// TestSysfsIsReadOnly verifies that a test cannot change the shared test sysfs
// data.
func TestSysfsIsReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}

	node := path.Join(testSysfs(t), "sysfs", "server", "sys", "devices", "system", "node", "node3")
	if _, err := os.Stat(path.Join(node, "meminfo")); err != nil {
		t.Fatalf("test sysfs data has no meminfo to change: %v", err)
	}

	for _, tc := range []struct {
		name   string
		change func() error
	}{
		{"modify a file", func() error { return os.WriteFile(path.Join(node, "meminfo"), nil, 0o644) }},
		{"create a file", func() error { return os.WriteFile(path.Join(node, "new-file"), nil, 0o644) }},
		{"remove a file", func() error { return os.Remove(path.Join(node, "meminfo")) }},
		{"create a directory", func() error { return os.Mkdir(path.Join(node, "new-dir"), 0o755) }},
	} {
		if err := tc.change(); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%s: got %v, expected a permission error", tc.name, err)
		}
	}
}

// testServerSystem discovers the "server" test sysfs.
func testServerSystem(t *testing.T) system.System {
	t.Helper()
	sys, err := system.DiscoverSystemAt(path.Join(testSysfs(t), "sysfs", "server", "sys"))
	if err != nil {
		t.Fatalf("failed to discover system: %v", err)
	}
	return sys
}

func TestPoolCreation(t *testing.T) {

	// Test pool creation with "real" sysfs data.

	dir := testSysfs(t)

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
			if err := policy.Setup(policyOptions); err != nil {
				log.Warnf("failed to setup test policy: %v", err)
			}
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

	dir := testSysfs(t)

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
			if err := policy.Setup(policyOptions); err != nil {
				log.Warnf("failed to setup test policy: %v", err)
			}
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

	dir := testSysfs(t)

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
			if err := policy.Setup(policyOptions); err != nil {
				log.Warnf("failed to setup test policy: %v", err)
			}
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
