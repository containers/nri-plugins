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

package libmem_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

func TestNewAllocatorWithSystemNodes(t *testing.T) {
	var (
		sysRoot = "./testdata/sample2"
		sys     sysfs.System
		err     error
		a       *Allocator
	)

	sys, err = sysfs.DiscoverSystemAt(sysRoot + "/sys")
	require.Nil(t, err, "sysfs discovery error for "+sysRoot)
	require.NotNil(t, sys, "sysfs discovery for "+sysRoot)

	a, err = NewAllocator(WithSystemNodes(sys))
	require.Nil(t, err, "allocator creation error")
	require.NotNil(t, a, "created allocator")
}

func TestOffer(t *testing.T) {
	var (
		setup = &testSetup{
			description: "test setup",
			types: []Type{
				TypeDRAM, TypeDRAM,
			},
			capacities: []int64{
				4, 4,
			},
			movability: []bool{
				normal, normal,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3},
			},
			distances: [][]int{
				{10, 21},
				{21, 10},
			},
		}
	)

	a, err := NewAllocator(WithNodes(setup.nodes(t)))
	require.Nil(t, err, "unexpected NewAllocator() error")
	require.NotNil(t, a, "unexpected nil allocator")

	o1, err := a.GetOffer(Container("id1", "test", "burstable", 1, NewNodeMask(0)))
	require.Nil(t, err, "unexpected GetOffer() error")
	require.NotNil(t, o1, "unexpected nil offer")

	o2, err := a.GetOffer(Container("id2", "test", "burstable", 1, NewNodeMask(1)))
	require.Nil(t, err, "unexpected GetOffer() error")
	require.NotNil(t, o2, "unexpected nil offer")

	n, _, err := o1.Commit()
	require.Nil(t, err, "unexpected Offer.Commit() error")
	require.NotEqual(t, n, NodeMask(0), "unexpected Offer.Commit() failure")

	n, _, err = o2.Commit()
	t.Logf("got error %v", err)
	require.NotNil(t, err, "unexpected success, offer should have expired")
	require.Equal(t, n, NodeMask(0), "failed commit should return 0 NodeMask")
}

func TestCPUSetAffinity(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM+4 PMEM NUMA nodes, 4 bytes per node, 2 close CPUs",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
				TypePMEM, TypePMEM, TypePMEM, TypePMEM,
			},
			capacities: []int64{
				4, 4, 4, 4,
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
				normal, normal, normal, normal,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3}, {4, 5}, {6, 7},
				{8, 9}, {10, 11}, {12, 13}, {14, 15},
			},
			distances: [][]int{
				{10, 21, 11, 21, 17, 28, 28, 28},
				{21, 10, 21, 11, 28, 28, 17, 28},
				{11, 21, 10, 21, 28, 17, 28, 28},
				{21, 11, 21, 10, 28, 28, 28, 17},
				{17, 28, 28, 28, 10, 28, 28, 28},
				{28, 28, 17, 28, 28, 10, 28, 28},
				{28, 17, 28, 28, 28, 28, 10, 28},
				{28, 28, 28, 17, 28, 28, 28, 10},
			},
		}
	)

	a, err := NewAllocator(WithNodes(setup.nodes(t)))
	require.Nil(t, err)
	require.NotNil(t, a)

	type testCase struct {
		name     string
		cpus     []int
		affinity NodeMask
	}

	for _, tc := range []*testCase{
		{
			name:     "CPU #0",
			cpus:     []int{0},
			affinity: NewNodeMask(0),
		},
		{
			name:     "CPU #0,1",
			cpus:     []int{0, 1},
			affinity: NewNodeMask(0),
		},
		{
			name:     "CPU #2",
			cpus:     []int{2},
			affinity: NewNodeMask(1),
		},
		{
			name:     "CPU #2,3",
			cpus:     []int{2, 3},
			affinity: NewNodeMask(1),
		},
		{
			name:     "CPU #0,2",
			cpus:     []int{0, 2},
			affinity: NewNodeMask(0, 1),
		},
		{
			name:     "CPU #0,2,5",
			cpus:     []int{0, 2, 5},
			affinity: NewNodeMask(0, 1, 2),
		},
		{
			name:     "CPU #0,3,4,7",
			cpus:     []int{0, 3, 4, 7},
			affinity: NewNodeMask(0, 1, 2, 3),
		},
		{
			name:     "CPU #8,12,15",
			cpus:     []int{8, 12, 15},
			affinity: NewNodeMask(4, 6, 7),
		},
		{
			name:     "CPU #0,3,4,7,8,11,12,15",
			cpus:     []int{0, 3, 4, 7, 8, 11, 12, 15},
			affinity: NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.affinity, a.CPUSetAffinity(cpuset.New(tc.cpus...)))
		})
	}
}

func TestExpand(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM+4 PMEM+4 HBM NUMA nodes",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
				TypePMEM, TypePMEM, TypePMEM, TypePMEM,
				TypeHBM, TypeHBM, TypeHBM, TypeHBM,
			},
			capacities: []int64{
				4, 4, 4, 4,
				4, 4, 4, 4,
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
				normal, normal, normal, normal,
				normal, normal, normal, normal,
			},
			closeCPUs: [][]int{
				{}, {}, {}, {},
				{}, {}, {}, {},
				{}, {}, {}, {},
			},
			distances: [][]int{
				{10, 11, 21, 21, 17, 27, 28, 28, 14, 15, 23, 23},
				{11, 10, 21, 21, 27, 17, 28, 28, 15, 14, 23, 23},
				{21, 21, 10, 11, 28, 28, 17, 27, 23, 23, 14, 15},
				{21, 21, 11, 10, 28, 28, 27, 17, 23, 23, 15, 14},
				{17, 27, 28, 28, 10, 28, 28, 28, 16, 26, 26, 26},
				{27, 17, 28, 28, 28, 10, 28, 28, 29, 11, 29, 29},
				{28, 28, 17, 27, 28, 28, 10, 28, 29, 29, 11, 29},
				{28, 28, 27, 17, 28, 28, 28, 10, 29, 29, 29, 11},
				{14, 15, 23, 23, 16, 29, 29, 29, 10, 12, 12, 12},
				{15, 14, 23, 23, 26, 11, 29, 29, 12, 10, 12, 12},
				{23, 23, 14, 15, 26, 29, 11, 29, 12, 12, 10, 12},
				{23, 23, 15, 14, 26, 29, 29, 11, 12, 12, 12, 10},
			},
		}
	)

	a, err := NewAllocator(WithNodes(setup.nodes(t)))
	require.Nil(t, err)
	require.NotNil(t, a)

	type testCase struct {
		name   string
		nodes  NodeMask
		types  []TypeMask
		result []NodeMask
	}

	for _, tc := range []*testCase{
		{
			name:  "DRAM expansion, starting at node #0",
			nodes: NewNodeMask(0),
			types: []TypeMask{
				TypeMaskDRAM,
				TypeMaskDRAM,
			},
			result: []NodeMask{
				NewNodeMask(1),
				NewNodeMask(2, 3),
			},
		},
		{
			name:  "PMEM expansion, starting at node #0",
			nodes: NewNodeMask(0),
			types: []TypeMask{
				TypeMaskPMEM,
				TypeMaskPMEM,
				TypeMaskPMEM,
			},
			result: []NodeMask{
				NewNodeMask(4),
				NewNodeMask(5),
				NewNodeMask(6, 7),
			},
		},
		{
			name:  "HBM expansion, starting at node #0",
			nodes: NewNodeMask(0),
			types: []TypeMask{
				TypeMaskHBM,
				TypeMaskHBM,
			},
			result: []NodeMask{
				NewNodeMask(8),
				NewNodeMask(9, 10, 11),
			},
		},
		{
			name:  "DRAM+PMEM expansion, starting at node #0",
			nodes: NewNodeMask(0),
			types: []TypeMask{
				TypeMaskDRAM | TypeMaskPMEM,
				TypeMaskDRAM | TypeMaskPMEM,
				TypeMaskDRAM | TypeMaskPMEM,
			},
			result: []NodeMask{
				NewNodeMask(1, 4),
				NewNodeMask(2, 3, 5),
				NewNodeMask(6, 7),
			},
		},
		{
			name:  "DRAM+HBM expansion, starting at node #2",
			nodes: NewNodeMask(2),
			types: []TypeMask{
				TypeMaskDRAM | TypeMaskHBM,
				TypeMaskDRAM | TypeMaskHBM,
			},
			result: []NodeMask{
				NewNodeMask(3, 10),
				NewNodeMask(0, 1, 8, 9, 11),
			},
		},
		{
			name:  "DRAM+PMEM+HBM expansion, starting at node #1",
			nodes: NewNodeMask(1),
			types: []TypeMask{
				TypeMaskDRAM | TypeMaskPMEM | TypeMaskHBM,
				TypeMaskDRAM | TypeMaskPMEM | TypeMaskHBM,
				TypeMaskDRAM | TypeMaskPMEM | TypeMaskHBM,
			},
			result: []NodeMask{
				NewNodeMask(0, 5, 9),
				NewNodeMask(2, 3, 4, 8, 10, 11),
				NewNodeMask(6, 7),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := tc.nodes
			for i, types := range tc.types {
				newNodes, _ := a.Expand(nodes, types)
				require.Equal(t, tc.result[i], newNodes)
				nodes |= newNodes
			}
		})
	}
}

func TestAllocate(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM+4 PMEM NUMA nodes, 4 bytes per node, 2 close CPUs",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
				TypePMEM, TypePMEM, TypePMEM, TypePMEM,
			},
			capacities: []int64{
				4, 4, 4, 4,
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
				normal, normal, normal, normal,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3}, {4, 5}, {6, 7},
				{8, 9}, {10, 11}, {12, 13}, {14, 15},
			},
			distances: [][]int{
				{10, 21, 11, 21, 17, 28, 28, 28},
				{21, 10, 21, 11, 28, 28, 17, 28},
				{11, 21, 10, 21, 28, 17, 28, 28},
				{21, 11, 21, 10, 28, 28, 28, 17},
				{17, 28, 28, 28, 10, 28, 28, 28},
				{28, 28, 17, 28, 28, 10, 28, 28},
				{28, 17, 28, 28, 28, 28, 10, 28},
				{28, 28, 28, 17, 28, 28, 28, 10},
			},
		}

		customDefault = &CustomFunctions{
			ExpandZone: func(zone NodeMask, types TypeMask, a CustomAllocator) NodeMask {
				return a.DefaultExpandZone(zone, types)
			},
			HandleOvercommit: func(overcommit map[NodeMask]int64, a CustomAllocator) error {
				return a.DefaultHandleOvercommit(overcommit)
			},
		}
	)

	for _, c := range []*CustomFunctions{nil, customDefault} {
		a, err := NewAllocator(
			WithNodes(setup.nodes(t)),
			WithCustomFunctions(c),
		)
		require.Nil(t, err)
		require.NotNil(t, a)

		type testCase struct {
			name     string
			id       string
			limit    int64
			types    TypeMask
			preserve bool
			strict   bool
			affinity NodeMask
			qos      string
			result   NodeMask
			updates  map[string]NodeMask
			fail     bool
			reset    bool
			release  []string
		}

		for _, tc := range []*testCase{
			{
				name:     "too big allocation",
				id:       "1",
				affinity: NewNodeMask(0, 1, 2, 3),
				limit:    33,
				types:    TypeMaskDRAM,
				fail:     true,
			},
			{
				name:     "allocation with unavailable node",
				id:       "1",
				affinity: NewNodeMask(10),
				limit:    1,
				types:    TypeMaskDRAM,
				fail:     true,
			},
			{
				name:  "allocation without affinity",
				id:    "1",
				limit: 1,
				types: TypeMaskDRAM,
				fail:  true,
			},
			{
				name:  "allocation with unavailable node type",
				id:    "1",
				limit: 1,
				types: TypeMaskHBM,
				fail:  true,
			},
			{
				name:     "2 bytes of DRAM from node #0",
				id:       "1",
				affinity: NewNodeMask(0),
				limit:    2,
				types:    TypeMaskDRAM,
				result:   NewNodeMask(0),
			},
			{
				name:  "allocation attempt with existing ID",
				id:    "1",
				limit: 1,
				types: TypeMaskDRAM,
				fail:  true,
			},
			{
				name:     "2 bytes of DRAM from node #2",
				id:       "2",
				affinity: NewNodeMask(2),
				limit:    2,
				types:    TypeMaskDRAM,
				result:   NewNodeMask(2),
			},
			{
				name:     "2 bytes of DRAM from node #0",
				id:       "3",
				affinity: NewNodeMask(0),
				limit:    2,
				types:    TypeMaskDRAM,
				result:   NewNodeMask(0),
			},
			{
				name:     "2 bytes of DRAM from node #2",
				id:       "4",
				affinity: NewNodeMask(2),
				limit:    2,
				types:    TypeMaskDRAM,
				result:   NewNodeMask(2),
			},
			{
				name:     "2 bytes of DRAM, guaranteed from node #0",
				id:       "5",
				affinity: NewNodeMask(0),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "guaranteed",
				result:   NewNodeMask(0),
				updates:  map[string]NodeMask{"3": NewNodeMask(0, 1, 2, 3)},
			},
			{
				name:     "2 bytes of DRAM, guaranteed from node #2",
				id:       "6",
				affinity: NewNodeMask(2),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "guaranteed",
				result:   NewNodeMask(2),
				updates:  map[string]NodeMask{"4": NewNodeMask(0, 1, 2, 3)},
			},
			{
				name:     "2 bytes of DRAM, guaranteed from node #0",
				id:       "7",
				affinity: NewNodeMask(0),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "guaranteed",
				result:   NewNodeMask(0),
				updates:  map[string]NodeMask{"1": NewNodeMask(0, 1, 2, 3)},
			},
			{
				name:     "2 bytes of DRAM, guaranteed from node #2",
				id:       "8",
				affinity: NewNodeMask(2),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "guaranteed",
				result:   NewNodeMask(2),
				updates:  map[string]NodeMask{"2": NewNodeMask(0, 1, 2, 3)},
			},
			{
				name:     "2 bytes of DRAM from node #1",
				id:       "9",
				affinity: NewNodeMask(1),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(1),
				updates:  map[string]NodeMask{"4": NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7)},
			},
			{
				name:     "2 bytes of DRAM from node #3",
				id:       "10",
				affinity: NewNodeMask(3),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(3),
				updates:  map[string]NodeMask{"3": NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7)},
			},
			{
				name:     "2 bytes of DRAM from node #1",
				id:       "11",
				affinity: NewNodeMask(1),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(1),
				updates:  map[string]NodeMask{"2": NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7)},
			},
			{
				name:     "2 bytes of DRAM from node #3",
				id:       "12",
				affinity: NewNodeMask(3),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(3),
				updates:  map[string]NodeMask{"1": NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7)},
			},
			{
				name:     "2 bytes of DRAM from node #1, then release all",
				id:       "13",
				affinity: NewNodeMask(1),
				limit:    2,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7),
				release:  []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13"},
			},

			// ----- all allocations released -----

			{
				name:     "6 bytes of DRAM from nodes #0,2",
				id:       "14",
				affinity: NewNodeMask(0, 2),
				limit:    6,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0, 2),
			},
			{
				name:     "3 bytes of DRAM from node #0",
				id:       "15",
				affinity: NewNodeMask(0),
				limit:    3,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0),
				updates:  map[string]NodeMask{"14": NewNodeMask(0, 1, 2, 3)},
				release:  []string{"14", "15"},
			},

			// ----- all allocations released -----

			{
				name:     "4 bytes of DRAM from nodes #0,2, preserved",
				id:       "16",
				affinity: NewNodeMask(0, 2),
				limit:    4,
				types:    TypeMaskDRAM,
				preserve: true,
				qos:      "burstable",
				result:   NewNodeMask(0, 2),
			},
			{
				name:     "4 bytes of DRAM from node #0,2",
				id:       "17",
				affinity: NewNodeMask(0, 2),
				limit:    4,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0, 2),
			},
			{
				name:     "1 byte of DRAM from node #0",
				id:       "18",
				affinity: NewNodeMask(0),
				limit:    1,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0),
				updates:  map[string]NodeMask{"17": NewNodeMask(0, 1, 2, 3)},
				release:  []string{"16", "17", "18"},
			},

			// ----- all allocations released -----

			{
				name:     "8 bytes of DRAM from nodes #0,1,2,3, strict",
				id:       "19",
				affinity: NewNodeMask(0, 1, 2, 3),
				limit:    8,
				types:    TypeMaskDRAM,
				strict:   true,
				qos:      "burstable",
				result:   NewNodeMask(0, 1, 2, 3),
			},
			{
				name:     "8 bytes of DRAM from node #0,1,2,3",
				id:       "20",
				affinity: NewNodeMask(0, 1, 2, 3),
				limit:    8,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0, 1, 2, 3),
			},
			{
				name:     "1 byte of DRAM from node #0",
				id:       "21",
				affinity: NewNodeMask(0),
				limit:    1,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0),
				updates:  map[string]NodeMask{"20": NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7)},
				release:  []string{"19", "20", "21"},
			},

			// ----- all allocations released -----

			{
				name:     "4 bytes of DRAM from nodes #0, preserved",
				id:       "22",
				affinity: NewNodeMask(0),
				limit:    4,
				types:    TypeMaskDRAM,
				preserve: true,
				qos:      "burstable",
				result:   NewNodeMask(0),
			},
			{
				name:     "4 bytes of DRAM from nodes #0,2",
				id:       "23",
				affinity: NewNodeMask(0, 2),
				limit:    4,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0, 2),
			},
			{
				name:     "1 byte of DRAM from nodes #0,2",
				id:       "24",
				affinity: NewNodeMask(0, 2),
				limit:    1,
				types:    TypeMaskDRAM,
				qos:      "burstable",
				result:   NewNodeMask(0, 1, 2, 3),
			},
		} {
			name := tc.name
			if c != nil {
				name = name + ", custom defaults"
			}

			t.Run(name, func(t *testing.T) {
				var (
					req *Request
				)

				switch {
				case tc.preserve:
					req = PreservedContainer(tc.id, name, tc.limit, tc.affinity)
				case tc.strict:
					req = ContainerWithStrictTypes(tc.id, name, tc.qos, tc.limit, tc.affinity, tc.types)
				default:
					req = ContainerWithTypes(tc.id, name, tc.qos, tc.limit, tc.affinity, tc.types)
				}

				nodes, updates, err := a.Allocate(req)

				defer func() {
					if tc.reset {
						a.Reset()
					}
				}()
				defer func() {
					for _, id := range tc.release {
						err := a.Release(id)
						require.Nil(t, err)
					}
				}()

				if tc.fail {
					require.NotNil(t, err, "unexpected allocation success")
					require.Equal(t, NodeMask(0), nodes, name)
					require.Nil(t, updates, name)
					t.Logf("* got error %v", err)
				} else {
					require.Nil(t, err, "unexpected allocation failure")
					require.Equal(t, tc.result, nodes, "allocated nodes")
					require.Equal(t, tc.updates, updates, "updated nodes")
				}
			})
		}
	}
}

func TestStrictAllocation(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM+4 PMEM NUMA nodes, 4 bytes per node, 2 close CPUs",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
				TypePMEM, TypePMEM, TypePMEM, TypePMEM,
			},
			capacities: []int64{
				4, 4, 4, 4,
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
				normal, normal, normal, normal,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3}, {4, 5}, {6, 7},
				{8, 9}, {10, 11}, {12, 13}, {14, 15},
			},
			distances: [][]int{
				{10, 21, 11, 21, 17, 28, 28, 28},
				{21, 10, 21, 11, 28, 28, 17, 28},
				{11, 21, 10, 21, 28, 17, 28, 28},
				{21, 11, 21, 10, 28, 28, 28, 17},
				{17, 28, 28, 28, 10, 28, 28, 28},
				{28, 28, 17, 28, 28, 10, 28, 28},
				{28, 17, 28, 28, 28, 28, 10, 28},
				{28, 28, 28, 17, 28, 28, 28, 10},
			},
		}
	)

	a, err := NewAllocator(WithNodes(setup.nodes(t)))
	require.Nil(t, err)
	require.NotNil(t, a)

	type testCase struct {
		name     string
		id       string
		limit    int64
		types    TypeMask
		strict   bool
		affinity NodeMask
		qos      string
		result   NodeMask
		updates  map[string]NodeMask
		fail     bool
		release  []string
	}

	for _, tc := range []*testCase{
		{
			name:     "1 byte of PMEM, affinity to nodes #0, strict",
			id:       "1",
			affinity: NewNodeMask(0),
			limit:    1,
			types:    TypeMaskPMEM,
			strict:   true,
			qos:      "burstable",
			result:   NewNodeMask(4),
			release:  []string{"1"},
		},
		{
			name:     "8 bytes of HBMEM, affinity to nodes #0, strict",
			id:       "19",
			affinity: NewNodeMask(0),
			limit:    8,
			types:    TypeMaskHBM,
			strict:   true,
			qos:      "burstable",
			fail:     true,
		},
		{
			name:     "8 bytes of DRAM from nodes #0,1,2,3, strict",
			id:       "19",
			affinity: NewNodeMask(0, 1, 2, 3),
			limit:    8,
			types:    TypeMaskDRAM,
			strict:   true,
			qos:      "burstable",
			result:   NewNodeMask(0, 1, 2, 3),
		},
		{
			name:     "8 bytes of DRAM from node #0,1,2,3",
			id:       "20",
			affinity: NewNodeMask(0, 1, 2, 3),
			limit:    8,
			types:    TypeMaskDRAM,
			qos:      "burstable",
			result:   NewNodeMask(0, 1, 2, 3),
		},
		{
			name:     "1 byte of DRAM from node #0",
			id:       "21",
			affinity: NewNodeMask(0),
			limit:    1,
			types:    TypeMaskDRAM,
			qos:      "burstable",
			result:   NewNodeMask(0),
			updates:  map[string]NodeMask{"20": NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				req *Request
			)

			if tc.strict {
				req = ContainerWithStrictTypes(tc.id, tc.name, tc.qos, tc.limit, tc.affinity, tc.types)
			} else {
				req = ContainerWithTypes(tc.id, tc.name, tc.qos, tc.limit, tc.affinity, tc.types)
			}

			nodes, updates, err := a.Allocate(req)

			if tc.fail {
				require.NotNil(t, err, "unexpected allocation success")
				require.Equal(t, NodeMask(0), nodes, tc.name)
				require.Nil(t, updates, tc.name)
				t.Logf("* got error %v", err)
			} else {
				require.Nil(t, err, "unexpected allocation failure")
				require.Equal(t, tc.result, nodes, "allocated nodes")
				require.Equal(t, tc.updates, updates, "updated nodes")
			}

			for _, id := range tc.release {
				err := a.Release(id)
				require.Nil(t, err, "release of ID #"+id)
			}
		})
	}
}

func TestPreservedAllocation(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM+4 PMEM NUMA nodes, 4 bytes per node, 2 close CPUs",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
				TypePMEM, TypePMEM, TypePMEM, TypePMEM,
			},
			capacities: []int64{
				4, 4, 4, 4,
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
				normal, normal, normal, normal,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3}, {4, 5}, {6, 7},
				{8, 9}, {10, 11}, {12, 13}, {14, 15},
			},
			distances: [][]int{
				{10, 21, 11, 21, 17, 28, 28, 28},
				{21, 10, 21, 11, 28, 28, 17, 28},
				{11, 21, 10, 21, 28, 17, 28, 28},
				{21, 11, 21, 10, 28, 28, 28, 17},
				{17, 28, 28, 28, 10, 28, 28, 28},
				{28, 28, 17, 28, 28, 10, 28, 28},
				{28, 17, 28, 28, 28, 28, 10, 28},
				{28, 28, 28, 17, 28, 28, 28, 10},
			},
		}
	)

	a, err := NewAllocator(WithNodes(setup.nodes(t)))
	require.Nil(t, err)
	require.NotNil(t, a)

	type testCase struct {
		name     string
		id       string
		limit    int64
		types    TypeMask
		preserve bool
		affinity NodeMask
		qos      string
		result   NodeMask
		updates  map[string]NodeMask
		fail     bool
		release  []string
	}

	for _, tc := range []*testCase{
		{
			name:     "4 bytes of DRAM from nodes #0,2, preserved",
			id:       "16",
			affinity: NewNodeMask(0, 2),
			limit:    4,
			types:    TypeMaskDRAM,
			preserve: true,
			qos:      "burstable",
			result:   NewNodeMask(0, 2),
		},
		{
			name:     "4 bytes of DRAM from node #0,2",
			id:       "17",
			affinity: NewNodeMask(0, 2),
			limit:    4,
			types:    TypeMaskDRAM,
			qos:      "burstable",
			result:   NewNodeMask(0, 2),
		},
		{
			name:     "1 byte of DRAM from node #0",
			id:       "18",
			affinity: NewNodeMask(0),
			limit:    1,
			types:    TypeMaskDRAM,
			qos:      "burstable",
			result:   NewNodeMask(0),
			updates:  map[string]NodeMask{"17": NewNodeMask(0, 1, 2, 3)},
			release:  []string{"16", "17", "18"},
		},

		// ----- all allocations released -----

		{
			name:     "4 bytes of DRAM from nodes #0, preserved",
			id:       "22",
			affinity: NewNodeMask(0),
			limit:    4,
			types:    TypeMaskDRAM,
			preserve: true,
			qos:      "burstable",
			result:   NewNodeMask(0),
		},
		{
			name:     "4 bytes of DRAM from nodes #0,2",
			id:       "23",
			affinity: NewNodeMask(0, 2),
			limit:    4,
			types:    TypeMaskDRAM,
			qos:      "burstable",
			result:   NewNodeMask(0, 2),
		},
		{
			name:     "1 byte of DRAM from nodes #0,2",
			id:       "24",
			affinity: NewNodeMask(0, 2),
			limit:    1,
			types:    TypeMaskDRAM,
			qos:      "burstable",
			result:   NewNodeMask(0, 1, 2, 3),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				req *Request
			)
			if tc.preserve {
				req = PreservedContainer(tc.id, tc.name, tc.limit, tc.affinity)
			} else {
				req = ContainerWithTypes(tc.id, tc.name, tc.qos, tc.limit, tc.affinity, tc.types)
			}

			nodes, updates, err := a.Allocate(req)

			if tc.fail {
				require.NotNil(t, err, "unexpected allocation success")
				require.Equal(t, NodeMask(0), nodes, tc.name)
				require.Nil(t, updates, tc.name)
				t.Logf("* got error %v", err)
			} else {
				require.Nil(t, err, "unexpected allocation failure")
				require.Equal(t, tc.result, nodes, "allocated nodes")
				require.Equal(t, tc.updates, updates, "updated nodes")
			}

			for _, id := range tc.release {
				err := a.Release(id)
				require.Nil(t, err, "release of ID #"+id)
			}
		})
	}
}

func TestRealloc(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM+4 PMEM NUMA nodes, 4 bytes per node, 2 close CPUs",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
				TypePMEM, TypePMEM, TypePMEM, TypePMEM,
			},
			capacities: []int64{
				4, 4, 4, 4,
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
				normal, normal, normal, normal,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3}, {4, 5}, {6, 7},
				{8, 9}, {10, 11}, {12, 13}, {14, 15},
			},
			distances: [][]int{
				{10, 21, 11, 21, 17, 28, 28, 28},
				{21, 10, 21, 11, 28, 28, 17, 28},
				{11, 21, 10, 21, 28, 17, 28, 28},
				{21, 11, 21, 10, 28, 28, 28, 17},
				{17, 28, 28, 28, 10, 28, 28, 28},
				{28, 28, 17, 28, 28, 10, 28, 28},
				{28, 17, 28, 28, 28, 28, 10, 28},
				{28, 28, 28, 17, 28, 28, 28, 10},
			},
		}
	)

	a, err := NewAllocator(
		WithNodes(setup.nodes(t)),
	)
	require.Nil(t, err)
	require.NotNil(t, a)

	type testCase struct {
		name      string
		id        string
		limit     int64
		types     TypeMask
		affinity  NodeMask
		result    NodeMask
		updates   map[string]NodeMask
		newNodes  NodeMask
		newTypes  TypeMask
		realloced NodeMask
		fail      bool
		reset     bool
		release   []string
	}

	for _, tc := range []*testCase{
		{
			name:      "2 bytes of DRAM from node #0, realloced to use also node #1",
			id:        "1",
			affinity:  NewNodeMask(0),
			limit:     2,
			types:     TypeMaskDRAM,
			result:    NewNodeMask(0),
			newNodes:  NewNodeMask(1),
			realloced: NewNodeMask(0, 1, 2, 3),
		},
		{
			name:      "Then realloced to same subset node #0",
			id:        "1",
			newNodes:  NewNodeMask(0),
			realloced: NewNodeMask(0, 1, 2, 3),
		},
		{
			name:      "Then realloced to same subset node #0, 1, 3",
			id:        "1",
			newNodes:  NewNodeMask(0, 1, 3),
			realloced: NewNodeMask(0, 1, 2, 3),
		},
		{
			name:      "Then realloced to same node type DRAM",
			id:        "1",
			newTypes:  TypeMaskDRAM,
			realloced: NewNodeMask(0, 1, 2, 3),
			reset:     true,
		},
		{
			name:      "2 bytes of DRAM from node #0, realloced to DRAM+PMEM",
			id:        "2",
			affinity:  NewNodeMask(0),
			limit:     2,
			types:     TypeMaskDRAM,
			result:    NewNodeMask(0),
			newTypes:  TypeMaskPMEM,
			realloced: NewNodeMask(0, 4),
		},
		{
			name:      "Then realloced to same subset PMEM node types",
			id:        "2",
			newTypes:  TypeMaskDRAM,
			realloced: NewNodeMask(0, 4),
		},
		{
			name:      "Then realloced to same subset PMEM node types",
			id:        "2",
			newTypes:  TypeMaskPMEM,
			realloced: NewNodeMask(0, 4),
		},
		{
			name:      "Then realloced to same DRAM+PMEM node types",
			id:        "2",
			newTypes:  TypeMaskDRAM | TypeMaskPMEM,
			realloced: NewNodeMask(0, 4),
		},

		{
			name:      "2 bytes of DRAM from node #1, realloced to DRAM+PMEM",
			id:        "3",
			affinity:  NewNodeMask(1),
			limit:     2,
			types:     TypeMaskDRAM,
			result:    NewNodeMask(1),
			newTypes:  TypeMaskPMEM,
			realloced: NewNodeMask(1, 6),
		},
		{
			name:      "2 bytes of DRAM from node #0,2, realloced to DRAM+PMEM",
			id:        "4",
			affinity:  NewNodeMask(0, 2),
			limit:     2,
			types:     TypeMaskDRAM,
			result:    NewNodeMask(0, 2),
			newTypes:  TypeMaskPMEM,
			realloced: NewNodeMask(0, 2, 4, 5),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				nodes   NodeMask
				updates map[string]NodeMask
				qos     = "burstable"
				err     error
			)

			if tc.affinity != 0 || tc.types != 0 {
				nodes, updates, err = a.Allocate(
					ContainerWithTypes(tc.id, tc.name, qos, tc.limit, tc.affinity, tc.types),
				)

				require.Nil(t, err, "unexpected allocation failure")
				require.Equal(t, tc.result, nodes, "allocated nodes")
				require.Equal(t, tc.updates, updates, "updated nodes")
			}

			nodes, _, err = a.Realloc(tc.id, tc.newNodes, tc.newTypes)

			if !tc.fail {
				require.Nil(t, err, "unexpected realloc failure")
				require.Equal(t, tc.realloced, nodes, "realloced nodes")
			}

			if tc.reset {
				a.Reset()
			}

			for _, id := range tc.release {
				err := a.Release(id)
				require.Nil(t, err, "release of ID #"+id)
			}
		})
	}
}

func TestOfferInvalidation(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM+4 PMEM NUMA nodes, 4 bytes per node, 2 close CPUs",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
				TypePMEM, TypePMEM, TypePMEM, TypePMEM,
			},
			capacities: []int64{
				4, 4, 4, 4,
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
				normal, normal, normal, normal,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3}, {4, 5}, {6, 7},
				{8, 9}, {10, 11}, {12, 13}, {14, 15},
			},
			distances: [][]int{
				{10, 21, 11, 21, 17, 28, 28, 28},
				{21, 10, 21, 11, 28, 28, 17, 28},
				{11, 21, 10, 21, 28, 17, 28, 28},
				{21, 11, 21, 10, 28, 28, 28, 17},
				{17, 28, 28, 28, 10, 28, 28, 28},
				{28, 28, 17, 28, 28, 10, 28, 28},
				{28, 17, 28, 28, 28, 28, 10, 28},
				{28, 28, 28, 17, 28, 28, 28, 10},
			},
		}

		offers = map[string]*Offer{}
	)

	a, err := NewAllocator(
		WithNodes(setup.nodes(t)),
	)
	require.Nil(t, err)
	require.NotNil(t, a)

	type commit struct {
		id   string
		fail bool
	}

	type testCase struct {
		name     string
		id       string
		limit    int64
		types    TypeMask
		affinity NodeMask
		commits  []commit
		release  []string
	}

	for _, tc := range []*testCase{
		{
			name:     "2 bytes of DRAM from node #0",
			id:       "1",
			affinity: NewNodeMask(0),
			limit:    2,
			types:    TypeMaskDRAM,
		},
		{
			name:     "2 bytes of DRAM from node #1",
			id:       "2",
			affinity: NewNodeMask(1),
			limit:    2,
			types:    TypeMaskDRAM,
			commits: []commit{
				{id: "1"}, {id: "2", fail: true},
			},
		},
		{
			name:     "2 bytes of DRAM from node #3",
			id:       "3",
			affinity: NewNodeMask(3),
			limit:    2,
			types:    TypeMaskDRAM,
			release: []string{
				"1",
			},
			commits: []commit{
				{id: "3", fail: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := a.GetOffer(
				ContainerWithTypes(tc.id, tc.name, "burstable", tc.limit, tc.affinity, tc.types),
			)

			require.Nil(t, err, "unexpected allocation failure")
			require.NotNil(t, o, "unexpected nil offer")

			offers[tc.id] = o

			for _, id := range tc.release {
				err := a.Release(id)
				require.Nil(t, err, "unexpected release failure")
			}

			for _, c := range tc.commits {
				_, _, err := offers[c.id].Commit()
				if c.fail {
					require.NotNil(t, err, "unexpected offer commit success")
				} else {
					require.Nil(t, err, "unexpected offer commit failure")
				}
			}
		})
	}
}

func TestEnsureNormalMemory(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM+4 PMEM NUMA nodes, 4 bytes per node, 2 close CPUs",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
				TypePMEM, TypePMEM, TypePMEM, TypePMEM,
			},
			capacities: []int64{
				4, 4, 4, 4,
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
				movable, movable, movable, movable,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3}, {4, 5}, {6, 7},
				{8, 9}, {10, 11}, {12, 13}, {14, 15},
			},
			distances: [][]int{
				{10, 21, 11, 21, 17, 28, 28, 28},
				{21, 10, 21, 11, 28, 28, 17, 28},
				{11, 21, 10, 21, 28, 17, 28, 28},
				{21, 11, 21, 10, 28, 28, 28, 17},
				{17, 28, 28, 28, 10, 28, 28, 28},
				{28, 28, 17, 28, 28, 10, 28, 28},
				{28, 17, 28, 28, 28, 28, 10, 28},
				{28, 28, 28, 17, 28, 28, 28, 10},
			},
		}
	)

	a, err := NewAllocator(WithNodes(setup.nodes(t)))
	require.Nil(t, err)
	require.NotNil(t, a)

	type testCase struct {
		name     string
		id       string
		limit    int64
		types    TypeMask
		strict   bool
		affinity NodeMask
		qos      string
		result   NodeMask
		updates  map[string]NodeMask
		fail     bool
		release  []string
	}

	for _, tc := range []*testCase{
		{
			name:     "4 bytes of PMEM from node #4",
			id:       "1",
			affinity: NewNodeMask(4),
			limit:    4,
			types:    TypeMaskPMEM,
			qos:      "burstable",
			result:   NewNodeMask(0, 4),
			release:  []string{"1"},
		},
		{
			name:     "4 bytes of PMEM from node #6",
			id:       "2",
			affinity: NewNodeMask(6),
			limit:    4,
			types:    TypeMaskPMEM,
			qos:      "burstable",
			result:   NewNodeMask(1, 6),
			release:  []string{"2"},
		},
		{
			name:     "4 bytes of PMEM from node #5",
			id:       "3",
			affinity: NewNodeMask(5),
			limit:    4,
			types:    TypeMaskPMEM,
			qos:      "burstable",
			result:   NewNodeMask(2, 5),
			release:  []string{"3"},
		},
		{
			name:     "1 byte of PMEM from node #4, strict, should fail",
			id:       "18",
			affinity: NewNodeMask(4),
			limit:    1,
			types:    TypeMaskPMEM,
			qos:      "burstable",
			strict:   true,
			fail:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				req *Request
			)

			if tc.strict {
				req = ContainerWithStrictTypes(tc.id, tc.name, tc.qos, tc.limit, tc.affinity, tc.types)
			} else {
				req = ContainerWithTypes(tc.id, tc.name, tc.qos, tc.limit, tc.affinity, tc.types)
			}

			nodes, updates, err := a.Allocate(req)

			if tc.fail {
				require.NotNil(t, err, "unexpected allocation success")
				require.Equal(t, NodeMask(0), nodes, tc.name)
				require.Nil(t, updates, tc.name)
				t.Logf("* got error %v", err)
			} else {
				require.Nil(t, err, "unexpected allocation failure")
				require.Equal(t, tc.result, nodes, "allocated nodes")
				require.Equal(t, tc.updates, updates, "updated nodes")
			}

			for _, id := range tc.release {
				err := a.Release(id)
				require.Nil(t, err, "release of ID #"+id)
			}
		})
	}
}

// ==================================================================================

type testSetup struct {
	description string
	types       []Type
	capacities  []int64
	movability  []bool
	closeCPUs   [][]int
	distances   [][]int
}

const (
	movable = true
	normal  = false
)

func (s *testSetup) nodes(t *testing.T) []*Node {
	var nodes []*Node

	for id, memType := range s.types {
		var (
			capacity  = s.capacities[id]
			normal    = !s.movability[id]
			closeCPUs = cpuset.New(s.closeCPUs[id]...)
			distance  = s.distances[id]
		)

		phase := fmt.Sprintf("node #%d for test setup %s", id, s.description)
		n, err := NewNode(id, memType, capacity, normal, closeCPUs, distance)
		require.Nil(t, err, phase)
		require.NotNil(t, n, phase)

		nodes = append(nodes, n)
	}

	return nodes
}
