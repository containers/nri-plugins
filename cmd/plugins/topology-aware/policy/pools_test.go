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
	"os"
	"path"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/testutils"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

func findNodeWithName(name string, nodes []Node) Node {
	for _, node := range nodes {
		if node.Name() == name {
			return node
		}
	}
	panic("No node found with name " + name)
}

func removeAll(t *testing.T, path string) {
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("failed to remove %q: %v", path, err)
	}
}
func TestPoolCreation(t *testing.T) {

	// Test pool creation with "real" sysfs data.

	// Create a temporary directory for the test data.
	dir, err := os.MkdirTemp("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		panic(err)
	}
	defer removeAll(t, dir)

	// Uncompress the test data to the directory.
	err = testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir)
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

	// Create a temporary directory for the test data.
	dir, err := os.MkdirTemp("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		panic(err)
	}
	defer removeAll(t, dir)

	// Uncompress the test data to the directory.
	err = testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir)
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

	// Create a temporary directory for the test data.
	dir, err := os.MkdirTemp("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		panic(err)
	}
	defer removeAll(t, dir)

	// Uncompress the test data to the directory.
	err = testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir)
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

//
// DRA claim identification and pool accounting (Step 8, Task 6).
//

func TestParseCDIClaimUID(t *testing.T) {
	tcases := []struct {
		name       string
		deviceName string
		wantUID    string
		wantOK     bool
	}{
		{
			name:       "simple uid",
			deviceName: "nri.topology-aware.cpu/device=claim-abc123-req-dev-0",
			wantUID:    "abc123",
			wantOK:     true,
		},
		{
			// A uid containing '-' (the real Kubernetes UID shape) is still
			// recovered exactly, as long as <request> and <device> are each
			// single tokens: the split only has to strip a fixed 3 trailing
			// tokens, so it doesn't matter how many tokens the leading uid
			// part itself has.
			name:       "uid with embedded dashes, single-token request/device",
			deviceName: "nri.topology-aware.cpu/device=claim-13f7db45-eb2a-4dd1-b0cb-1234567890ab-myreq-dev0-0",
			wantUID:    "13f7db45-eb2a-4dd1-b0cb-1234567890ab",
			wantOK:     true,
		},
		{
			// Documents the known best-effort limitation: when <device> is
			// itself multi-token after sanitization (e.g. "punit-0-0"), the
			// fixed trailing-3-token strip lands in the wrong place and
			// "swallows" part of <request> into the returned uid.
			// claimCPUsFromContainer compensates for this by iterating over
			// its known-live UIDs and doing exact device-name construction
			// instead of relying on parseCDIClaimUID alone.
			name:       "documents limitation: multi-token device swallows the request",
			deviceName: "nri.topology-aware.cpu/device=claim-13f7db45-myreq-punit-0-0-0",
			wantUID:    "13f7db45-myreq-punit",
			wantOK:     true,
		},
		{
			name:       "wrong driver prefix",
			deviceName: "other.driver/device=claim-abc123-req-dev-0",
			wantOK:     false,
		},
		{
			name:       "not a CDI qualified name at all",
			deviceName: "not-a-cdi-device-name",
			wantOK:     false,
		},
		{
			name:       "right prefix, too few remaining tokens",
			deviceName: "nri.topology-aware.cpu/device=claim-dev-0",
			wantOK:     false,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			uid, ok := parseCDIClaimUID(tc.deviceName)
			if ok != tc.wantOK {
				t.Fatalf("parseCDIClaimUID(%q) ok = %v, want %v", tc.deviceName, ok, tc.wantOK)
			}
			if ok && uid != tc.wantUID {
				t.Errorf("parseCDIClaimUID(%q) = %q, want %q", tc.deviceName, uid, tc.wantUID)
			}
		})
	}
}

// fakeClaimLister is a minimal claimLister for tests, avoiding the need to
// stand up a full *dra.Plugin (kubelet registration, CDI writer, claim
// store, etc.) just to exercise claimCPUsFromContainer's lookup logic.
type fakeClaimLister struct {
	claims map[types.UID][]dra.ResultAlloc
}

func (f *fakeClaimLister) LiveClaimsLocked() map[types.UID][]dra.ResultAlloc {
	return f.claims
}

func (*fakeClaimLister) DriverName() string {
	return DRADriverName
}

func cdiClaimDeviceName(uid types.UID, request, device string, idx int) string {
	return DRADriverName + "/device=" + dra.CDIDeviceName(uid, request, device, idx)
}

func TestClaimCPUsFromContainer(t *testing.T) {
	uid := types.UID("claim-uid-1")
	name := cdiClaimDeviceName(uid, "myreq", "dev0", 0)

	c := &mockContainer{cdiDeviceNames: []string{name}}
	lister := &fakeClaimLister{
		claims: map[types.UID][]dra.ResultAlloc{
			uid: {{Request: "myreq", Device: "dev0", ClassName: "gold", CPUs: "0-1"}},
		},
	}

	claims := claimCPUsFromContainer(c, lister)
	if len(claims) != 1 {
		t.Fatalf("claimCPUsFromContainer() returned %d claim(s), want 1", len(claims))
	}
	got := claims[0]
	if got.UID != uid {
		t.Errorf("claimCPUsFromContainer() uid = %q, want %q", got.UID, uid)
	}
	if want := cpuset.MustParse("0-1"); !got.ClassCPUs["gold"].Equals(want) {
		t.Errorf("claimCPUsFromContainer() classCPUs[gold] = %s, want %s", got.ClassCPUs["gold"], want)
	}
	if len(got.ClassCPUs) != 1 {
		t.Errorf("claimCPUsFromContainer() classCPUs = %v, want exactly one class", got.ClassCPUs)
	}
	if want := cpuset.MustParse("0-1"); !got.CPUs.Equals(want) {
		t.Errorf("claimCPUsFromContainer() cpus = %s, want %s", got.CPUs, want)
	}
}

func TestClaimCPUsFromContainerDashedUIDAndDashedDeviceName(t *testing.T) {
	// A real k8s UID (embedded dashes) combined with a device name that
	// is itself multi-token after sanitization (e.g. "punit-0-0") is exactly
	// the case parseCDIClaimUID's fast path alone mis-splits (see
	// TestParseCDIClaimUID's "documents limitation" case). claimCPUsFromContainer
	// must still resolve it correctly via exact device-name construction.
	uid := types.UID("13f7db45-eb2a-4dd1-b0cb-1234567890ab")
	name := cdiClaimDeviceName(uid, "myreq", "punit-0-0", 0)

	c := &mockContainer{cdiDeviceNames: []string{name}}
	lister := &fakeClaimLister{
		claims: map[types.UID][]dra.ResultAlloc{
			uid: {{Request: "myreq", Device: "punit-0-0", ClassName: "gold", CPUs: "4-5"}},
		},
	}

	claims := claimCPUsFromContainer(c, lister)
	if len(claims) != 1 {
		t.Fatalf("claimCPUsFromContainer() returned %d claim(s), want 1", len(claims))
	}
	got := claims[0]
	if got.UID != uid {
		t.Errorf("claimCPUsFromContainer() uid = %q, want %q", got.UID, uid)
	}
	if want := cpuset.MustParse("4-5"); !got.ClassCPUs["gold"].Equals(want) {
		t.Errorf("claimCPUsFromContainer() classCPUs[gold] = %s, want %s", got.ClassCPUs["gold"], want)
	}
	if want := cpuset.MustParse("4-5"); !got.CPUs.Equals(want) {
		t.Errorf("claimCPUsFromContainer() cpus = %s, want %s", got.CPUs, want)
	}
}

func TestClaimCPUsFromContainerUsesOnlyConsumedAllocs(t *testing.T) {
	// A single claim can back more than one DeviceRequestAllocationResult
	// (e.g. multiple requests in one claim); the union of their CPUs must be
	// returned, and (since every alloc here shares the same class) a single
	// classCPUs entry covering that whole union.
	uid := types.UID("claim-uid-multi")
	name0 := cdiClaimDeviceName(uid, "req0", "dev0", 0)

	c := &mockContainer{cdiDeviceNames: []string{name0}}
	lister := &fakeClaimLister{
		claims: map[types.UID][]dra.ResultAlloc{
			uid: {
				{Request: "req0", Device: "dev0", ClassName: "gold", CPUs: "0-1"},
				{Request: "req1", Device: "dev1", ClassName: "gold", CPUs: "2-3"},
			},
		},
	}

	claims := claimCPUsFromContainer(c, lister)
	if len(claims) != 1 {
		t.Fatalf("claimCPUsFromContainer() returned %d claim(s), want 1", len(claims))
	}
	got := claims[0]
	if want := cpuset.MustParse("0-1"); !got.CPUs.Equals(want) {
		t.Errorf("claimCPUsFromContainer() cpus = %s, want %s (only the consumed allocation)", got.CPUs, want)
	}
	if want := cpuset.MustParse("0-1"); len(got.ClassCPUs) != 1 || !got.ClassCPUs["gold"].Equals(want) {
		t.Errorf("claimCPUsFromContainer() classCPUs = %v, want {gold: %s}", got.ClassCPUs, want)
	}
}

// TestClaimCPUsFromContainerMultipleClasses covers the MAJOR finding this
// fix addresses: a single ResourceClaim can legitimately contain
// DeviceRequestAllocationResults resolving to devices of *different*
// cpuClasses (the per-punit multi-class-overcommit validation only guards a
// single punit's classes against each other, it does not forbid a claim's
// requests from spanning more than one punit/class). claimCPUsFromContainer
// must group the claimed CPUs by class rather than collapsing to a single
// className, so callers can apply each class only to the CPUs that actually
// belong to it.
func TestClaimCPUsFromContainerMultipleClasses(t *testing.T) {
	uid := types.UID("claim-uid-multiclass")
	name0 := cdiClaimDeviceName(uid, "req0", "dev0", 0)
	name1 := cdiClaimDeviceName(uid, "req1", "dev1", 1)

	c := &mockContainer{cdiDeviceNames: []string{name0, name1}}
	lister := &fakeClaimLister{
		claims: map[types.UID][]dra.ResultAlloc{
			uid: {
				{Request: "req0", Device: "dev0", ClassName: "gold", CPUs: "0-1"},
				{Request: "req1", Device: "dev1", ClassName: "silver", CPUs: "2-3"},
			},
		},
	}

	claims := claimCPUsFromContainer(c, lister)
	if len(claims) != 1 {
		t.Fatalf("claimCPUsFromContainer() returned %d claim(s), want 1", len(claims))
	}
	got := claims[0]
	if got.UID != uid {
		t.Errorf("claimCPUsFromContainer() uid = %q, want %q", got.UID, uid)
	}
	if want := cpuset.MustParse("0-3"); !got.CPUs.Equals(want) {
		t.Errorf("claimCPUsFromContainer() cpus = %s, want %s (union across classes)", got.CPUs, want)
	}
	if len(got.ClassCPUs) != 2 {
		t.Fatalf("claimCPUsFromContainer() classCPUs = %v, want two distinct classes", got.ClassCPUs)
	}
	if want := cpuset.MustParse("0-1"); !got.ClassCPUs["gold"].Equals(want) {
		t.Errorf("claimCPUsFromContainer() classCPUs[gold] = %s, want %s", got.ClassCPUs["gold"], want)
	}
	if want := cpuset.MustParse("2-3"); !got.ClassCPUs["silver"].Equals(want) {
		t.Errorf("claimCPUsFromContainer() classCPUs[silver] = %s, want %s", got.ClassCPUs["silver"], want)
	}
}

func TestClaimCPUsFromContainerNoCDIDevices(t *testing.T) {
	c := &mockContainer{}
	lister := &fakeClaimLister{claims: map[types.UID][]dra.ResultAlloc{}}

	claims := claimCPUsFromContainer(c, lister)
	if len(claims) != 0 {
		t.Errorf("claimCPUsFromContainer() = %v, want empty for a container with no CDI devices", claims)
	}
}

func TestClaimCPUsFromContainerUnknownClaim(t *testing.T) {
	// The container carries a well-formed claim device name, but the parsed
	// UID has no entry in LiveClaimsLocked() (e.g. the claim was already
	// unprepared, or the device belongs to some other driver's namespace).
	uid := types.UID("claim-uid-gone")
	name := cdiClaimDeviceName(uid, "myreq", "dev0", 0)

	c := &mockContainer{cdiDeviceNames: []string{name}}
	lister := &fakeClaimLister{claims: map[types.UID][]dra.ResultAlloc{}}

	claims := claimCPUsFromContainer(c, lister)
	if len(claims) != 0 {
		t.Errorf("claimCPUsFromContainer() = %v, want empty for an unknown/stale claim UID", claims)
	}
}

// TestClaimCPUsFromContainerMultipleDistinctClaims verifies that a
// container whose CDI devices resolve to two distinct live claim UIDs must
// have both claims' CPUs accounted for, not just the first one encountered.
func TestClaimCPUsFromContainerMultipleDistinctClaims(t *testing.T) {
	uid1 := types.UID("claim-uid-first")
	uid2 := types.UID("claim-uid-second")
	name1 := cdiClaimDeviceName(uid1, "req0", "dev0", 0)
	name2 := cdiClaimDeviceName(uid2, "req0", "dev1", 0)

	c := &mockContainer{cdiDeviceNames: []string{name1, name2}}
	lister := &fakeClaimLister{
		claims: map[types.UID][]dra.ResultAlloc{
			uid1: {{Request: "req0", Device: "dev0", ClassName: "gold", CPUs: "0-1"}},
			uid2: {{Request: "req0", Device: "dev1", ClassName: "silver", CPUs: "2-3"}},
		},
	}

	claims := claimCPUsFromContainer(c, lister)
	if len(claims) != 2 {
		t.Fatalf("claimCPUsFromContainer() returned %d claim(s), want 2 (both distinct live claims must be accounted for)", len(claims))
	}

	byUID := map[types.UID]containerClaim{}
	for _, cl := range claims {
		byUID[cl.UID] = cl
	}
	if cl, ok := byUID[uid1]; !ok || !cl.CPUs.Equals(cpuset.MustParse("0-1")) {
		t.Errorf("claim %s CPUs = %v, want 0-1", uid1, cl.CPUs)
	}
	if cl, ok := byUID[uid2]; !ok || !cl.CPUs.Equals(cpuset.MustParse("2-3")) {
		t.Errorf("claim %s CPUs = %v, want 2-3", uid2, cl.CPUs)
	}
}

// goldClassCPUs wraps cpus as the single-class classCPUs allocateClaim/
// remarkClaimInSupply expect, for tests that don't care about the
// multi-class case (see TestAllocateClaimAppliesPerAllocClass for that).
func goldClassCPUs(cpus cpuset.CPUSet) map[string]cpuset.CPUSet {
	return map[string]cpuset.CPUSet{"gold": cpus}
}

// addTestGrant hands out an exclusive grant for container from pool's
// FreeSupply, mirroring (at the level of supply-state mutation) what
// supply.AllocateCPU does for the granting node itself, then records the
// grant in p.allocations so p.releasePool/p.reallocateResources can find it.
// This lets eviction tests exercise the real p.releasePool/grant.Release()
// code path without going through the full container-annotation-driven
// request/offer pipeline (which coldstart_test.go notes is impractical to
// mock with a bare container).
func addTestGrant(t *testing.T, p *policy, pool Node, container cache.Container, exclusive cpuset.CPUSet) Grant {
	t.Helper()

	g := newGrant(pool, container, cpuNormal, "", exclusive, 0, memoryDRAM, nil, 0)

	s, ok := pool.FreeSupply().(*supply)
	if !ok {
		t.Fatalf("pool %q FreeSupply() is not a *supply", pool.Name())
	}
	s.isolated = s.isolated.Difference(exclusive)
	s.sharable = s.sharable.Difference(exclusive)
	g.AccountAllocateCPU()

	p.allocations.addGrant(g)

	return g
}

func TestAllocateClaimMarksTightestPool(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	cpus := cpuset.New(sharable[0], sharable[1])

	uid := types.UID("claim-mark")
	if err := p.allocateClaim(uid, cpus, goldClassCPUs(cpus)); err != nil {
		t.Fatalf("allocateClaim() failed: %v", err)
	}

	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(cpus).Size() != 0 {
		t.Errorf("claimed CPUs %s still present in %q sharable set: %s", cpus, leaf.Name(), got)
	}

	// A regular exclusive-CPU request for another container must not be
	// able to pick the claimed CPUs.
	free := leaf.FreeSupply().AllocatableSharedCPU()
	full := free / 1000
	if full < 1 {
		t.Fatalf("expected at least 1 full CPU still allocatable on %q, got %dm", leaf.Name(), free)
	}
	req := &request{full: full, container: &mockContainer{}}
	offer, err := leaf.FreeSupply().GetCPUOffer(req)
	if err != nil {
		t.Fatalf("GetCPUOffer for %d full CPUs failed unexpectedly: %v", full, err)
	}
	if offer.Intersection(cpus).Size() != 0 {
		t.Errorf("CPU offer %s for another container includes claimed CPUs %s", offer, cpus)
	}
}

func TestAllocateClaimOutsideAllowedReturnsError(t *testing.T) {
	p := newDRATestPolicy(t)

	// CPU 99999 does not exist on the test system at all, so it can't be a
	// subset of any pool's (including root's) statically assigned range.
	cpus := cpuset.New(99999)

	if err := p.allocateClaim(types.UID("claim-outside"), cpus, goldClassCPUs(cpus)); err == nil {
		t.Fatalf("allocateClaim() with CPUs outside the allowed set: got nil error, want a descriptive error")
	}
}

// TestAllocateClaimSpanningNoPoolReturnsError covers the other poolForCPUs
// failure mode from TestAllocateClaimOutsideAllowedReturnsError: CPUs that
// are individually within p.allowed (each belongs to some pool), but
// straddle two sibling pools so that no single pool's static range is a
// superset of the whole set. A legitimate single-punit DRA CPU pick never
// does this; allocateClaim must still reject it with a descriptive error
// rather than, say, silently marking one of the two pools.
func TestAllocateClaimSpanningNoPoolReturnsError(t *testing.T) {
	p := newDRATestPolicy(t)

	// "NUMA node #0" and "NUMA node #2" are siblings under "socket #0" (see
	// TestSupplyClaimCPUsAncestorNotDoubleSubtracted): no pool below "root"
	// (or "socket #0") is a strict subset spanning both, so a cpuset with
	// one CPU from each cannot be contained by any single pool.
	leafA := findPoolNode(t, p, "NUMA node #0")
	leafB := findPoolNode(t, p, "NUMA node #2")

	cpuA := leafA.GetSupply().SharableCPUs().List()
	cpuB := leafB.GetSupply().SharableCPUs().List()
	if len(cpuA) < 1 || len(cpuB) < 1 {
		t.Fatalf("expected at least 1 CPU on both %q and %q", leafA.Name(), leafB.Name())
	}
	spanning := cpuset.New(cpuA[0], cpuB[0])

	err := p.allocateClaim(types.UID("claim-spanning"), spanning, goldClassCPUs(spanning))
	if err == nil {
		t.Fatalf("allocateClaim() with CPUs spanning two pools: got nil error, want a descriptive error")
	}
	if _, exists := p.claimContainerRefs[types.UID("claim-spanning")]; exists {
		t.Errorf("claimContainerRefs unexpectedly populated for a claim that failed to allocate")
	}
}

func TestAllocateClaimRefcountsMultipleContainers(t *testing.T) {
	// A ResourceClaim with AllowMultipleAllocations backs more than one
	// container; allocateClaim is called once per container sharing it.
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 1 {
		t.Fatalf("expected at least 1 sharable CPU on %q", leaf.Name())
	}
	cpus := cpuset.New(sharable[0])
	uid := types.UID("claim-shared")

	if err := p.allocateClaim(uid, cpus, goldClassCPUs(cpus)); err != nil {
		t.Fatalf("first allocateClaim() failed: %v", err)
	}
	afterFirst := leaf.FreeSupply().SharableCPUs()

	if err := p.allocateClaim(uid, cpus, goldClassCPUs(cpus)); err != nil {
		t.Fatalf("second allocateClaim() (second container, same claim) failed: %v", err)
	}
	if got := leaf.FreeSupply().SharableCPUs(); !got.Equals(afterFirst) {
		t.Errorf("second allocateClaim() for the same uid changed pool supply: got %s, want unchanged %s", got, afterFirst)
	}
	if got := p.claimContainerRefs[uid]; got != 2 {
		t.Errorf("claimContainerRefs[%s] = %d, want 2 after two containers", uid, got)
	}

	// Releasing once (one of the two containers) must not restore the CPUs yet.
	if err := p.releaseClaim(uid, cpus); err != nil {
		t.Fatalf("first releaseClaim() failed: %v", err)
	}
	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(cpus).Size() != 0 {
		t.Errorf("CPUs %s restored after releasing only one of two referencing containers: sharable=%s", cpus, got)
	}

	// Releasing the second (last) container must restore the CPUs.
	if err := p.releaseClaim(uid, cpus); err != nil {
		t.Fatalf("second releaseClaim() failed: %v", err)
	}
	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(cpus).Size() != cpus.Size() {
		t.Errorf("CPUs %s not restored after releasing the last referencing container: sharable=%s", cpus, got)
	}
	if _, exists := p.claimContainerRefs[uid]; exists {
		t.Errorf("claimContainerRefs[%s] still present after refcount reached zero", uid)
	}
}

func TestReleaseClaimUnknownUIDNoop(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	before := leaf.FreeSupply().SharableCPUs()

	if err := p.releaseClaim(types.UID("never-claimed"), cpuset.New(before.List()[0])); err != nil {
		t.Errorf("releaseClaim() for an unknown uid: got error %v, want nil (idempotent)", err)
	}
	if got := leaf.FreeSupply().SharableCPUs(); !got.Equals(before) {
		t.Errorf("releaseClaim() for an unknown uid changed pool supply: got %s, want unchanged %s", got, before)
	}
}

// TestReleaseClaimResetsCpuClass verifies that releaseClaim is symmetric
// with allocateClaim's cpuClasses.UseClass call: releasing a claim must
// reset the physical cpuClass on the unclaimed CPUs back to the shared-pool
// baseline (mirroring releasePool's resetCpuClass call), not leave them
// stuck in the claim's class for whatever unrelated container the pool
// hands them to next.
func TestReleaseClaimResetsCpuClass(t *testing.T) {
	p := newDRATestPolicyWithCPUClasses(t, "shared", "gold")

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	cpu := sharable[0]
	claimed := cpuset.New(cpu)

	// A sibling CPU never touched by the claim: its class reflects whatever
	// initialize() applied via resetCpuClass("initialize", p.allowed) — the
	// shared-pool baseline every allowed CPU starts in.
	baselineCPU := sharable[1]
	baseline := p.cpuClasses.ClassForCPU(baselineCPU)

	uid := types.UID("claim-cpuclass-reset")
	if err := p.allocateClaim(uid, claimed, goldClassCPUs(claimed)); err != nil {
		t.Fatalf("allocateClaim() failed: %v", err)
	}
	if got := p.cpuClasses.ClassForCPU(cpu); got == baseline {
		t.Fatalf("test setup error: claimed CPU %d class unchanged (%q) after allocateClaim with class %q",
			cpu, got, "gold")
	}

	if err := p.releaseClaim(uid, claimed); err != nil {
		t.Fatalf("releaseClaim() failed: %v", err)
	}
	if got := p.cpuClasses.ClassForCPU(cpu); got != baseline {
		t.Errorf("claimed CPU %d class = %q after releaseClaim(), want reset back to shared-pool baseline %q",
			cpu, got, baseline)
	}
}

// TestAllocateClaimAppliesPerAllocClass covers the MAJOR finding this fix
// addresses: a single DRA claim can resolve to more than one cpuClass
// across its ResultAllocs (see classifyClaimCPUs), and allocateClaim must
// apply each class only to the CPU subset that actually belongs to it,
// rather than applying whichever class happened to come first to the
// claim's entire (unioned) CPU set.
func TestAllocateClaimAppliesPerAllocClass(t *testing.T) {
	p := newDRATestPolicyWithCPUClasses(t, "shared", "gold", "silver")

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	goldCPU := cpuset.New(sharable[0])
	silverCPU := cpuset.New(sharable[1])
	claimed := goldCPU.Union(silverCPU)

	uid := types.UID("claim-multiclass")
	classCPUs := map[string]cpuset.CPUSet{
		"gold":   goldCPU,
		"silver": silverCPU,
	}
	if err := p.allocateClaim(uid, claimed, classCPUs); err != nil {
		t.Fatalf("allocateClaim() failed: %v", err)
	}

	// ClassForCPU may return a synthetic, decorated class name (e.g. with a
	// per-die suffix) rather than the literal config name — see
	// TestReleaseClaimResetsCpuClass, which compares against a baseline
	// instead of a literal string for the same reason. What matters here is
	// that the gold- and silver-alloc CPUs end up in *different*, class
	// specific buckets: proof that allocateClaim applied each alloc's own
	// class to its own CPU subset, instead of the pre-fix behavior of
	// picking one class (from the first alloc) and applying it to the whole
	// unioned CPU set.
	goldGot := p.cpuClasses.ClassForCPU(sharable[0])
	silverGot := p.cpuClasses.ClassForCPU(sharable[1])
	if !strings.HasPrefix(goldGot, "gold") {
		t.Errorf("gold-alloc CPU %d class = %q, want a class derived from %q", sharable[0], goldGot, "gold")
	}
	if !strings.HasPrefix(silverGot, "silver") {
		t.Errorf("silver-alloc CPU %d class = %q, want a class derived from %q", sharable[1], silverGot, "silver")
	}
	if goldGot == silverGot {
		t.Errorf("gold-alloc and silver-alloc CPUs ended up in the same class %q; "+
			"per-alloc class application did not take effect", goldGot)
	}
}

func TestAllocateClaimEvictsOverlappingExclusiveGrant(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 1 {
		t.Fatalf("expected at least 1 sharable CPU on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0])

	victim := &mockContainer{returnValueForGetID: "victim"}
	addTestGrant(t, p, leaf, victim, claimed)

	if _, ok := p.allocations.getGrant("victim"); !ok {
		t.Fatalf("test setup error: victim grant not present before allocateClaim")
	}

	if err := p.allocateClaim(types.UID("evict-claim"), claimed, goldClassCPUs(claimed)); err != nil {
		t.Fatalf("allocateClaim() failed: %v", err)
	}

	// No grant anywhere may still exclusively hold the claimed CPU: either
	// the victim's original grant was released outright, or it was
	// reallocated to CPUs that no longer overlap the claim.
	for _, g := range p.allocations.grants {
		if g.ExclusiveCPUs().Intersection(claimed).Size() != 0 {
			t.Errorf("claimed CPU %s still exclusively granted to %s after eviction",
				claimed, g.GetContainer().PrettyName())
		}
	}

	// And the claimed CPU must not be free for a new regular allocation.
	free := leaf.FreeSupply()
	if free.SharableCPUs().Union(free.IsolatedCPUs()).Intersection(claimed).Size() != 0 {
		t.Errorf("claimed CPU %s still free in %q supply after allocateClaim", claimed, leaf.Name())
	}

	// allocateClaim returned nil, so the evicted victim must actually have
	// been reallocated a new grant (not just released and forgotten) — there
	// is ample capacity left on this test system for reallocatePool to
	// succeed. The new grant's exact shape (exclusive vs. shared/fractional)
	// depends on the request reallocatePool derives from the container's own
	// declared resource requirements — zero for the bare mockContainer used
	// here as "victim" — so only its existence and non-overlap with the
	// claimed CPU are asserted, not its exact size/type.
	newGrant, ok := p.allocations.getGrant("victim")
	if !ok {
		t.Fatalf("victim has no grant after allocateClaim() succeeded; eviction must reallocate, not just release")
	}
	if newGrant.ExclusiveCPUs().Intersection(claimed).Size() != 0 {
		t.Errorf("victim's new grant %s still overlaps claimed CPU %s", newGrant.ExclusiveCPUs(), claimed)
	}
}
