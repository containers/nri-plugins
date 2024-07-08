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
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
)

func TestCustomExpansion(t *testing.T) {
	var (
		setup = &testSetup{
			description: "4 DRAM nodes, 4 bytes per node, 2 close CPUs",
			types: []Type{
				TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM,
			},
			capacities: []int64{
				4, 4, 4, 4,
			},
			movability: []bool{
				normal, normal, normal, normal,
			},
			closeCPUs: [][]int{
				{0, 1}, {2, 3}, {4, 5}, {6, 7},
			},
			distances: [][]int{
				{10, 21, 11, 21},
				{21, 10, 21, 11},
				{11, 21, 10, 21},
				{21, 11, 21, 10},
				{17, 28, 28, 28},
			},
		}
	)

	a, err := NewAllocator(
		WithNodes(setup.nodes(t)),
		WithCustomFunctions(
			&CustomFunctions{
				ExpandZone: func(zone NodeMask, types TypeMask, a CustomAllocator) NodeMask {
					var (
						nodes02 = NewNodeMask(0, 2)
						nodes13 = NewNodeMask(1, 3)
					)
					switch zone {
					case nodes02:
						return NewNodeMask(0, 1, 2)
					case nodes13:
						return NewNodeMask(1, 2, 3)
					default:
						return a.DefaultExpandZone(zone, types)
					}
				},
			},
		),
	)
	require.Nil(t, err)
	require.NotNil(t, a)

	type testCase struct {
		name     string
		id       string
		limit    int64
		types    TypeMask
		affinity NodeMask
		qosClass string
		result   NodeMask
		updates  map[string]NodeMask
		fail     bool
		release  []string
	}

	for _, tc := range []*testCase{
		{
			name:     "6 bytes from zone #0",
			id:       "1",
			affinity: NewNodeMask(0),
			limit:    6,
			types:    TypeMaskDRAM,
			qosClass: "burstable",
			result:   NewNodeMask(0, 2),
			release:  []string{"1"},
		},
		{
			name:     "6 bytes from zone #1",
			id:       "2",
			affinity: NewNodeMask(1),
			limit:    6,
			types:    TypeMaskDRAM,
			qosClass: "burstable",
			result:   NewNodeMask(1, 3),
			release:  []string{"2"},
		},
		{
			name:     "9 bytes from zone #0, custom exception, should flow over to 0, 1, 2",
			id:       "3",
			affinity: NewNodeMask(0),
			limit:    9,
			types:    TypeMaskDRAM,
			qosClass: "burstable",
			result:   NewNodeMask(0, 1, 2),
			release:  []string{"3"},
		},
		{
			name:     "9 bytes from zone #1, custom exception, should flow over to 1, 2, 3",
			id:       "4",
			affinity: NewNodeMask(1),
			limit:    9,
			types:    TypeMaskDRAM,
			qosClass: "burstable",
			result:   NewNodeMask(1, 2, 3),
			release:  []string{"4"},
		},
		{
			name:     "15 bytes from zone #0",
			id:       "5",
			affinity: NewNodeMask(0),
			limit:    15,
			types:    TypeMaskDRAM,
			qosClass: "burstable",
			result:   NewNodeMask(0, 1, 2, 3),
			release:  []string{"5"},
		},
		{
			name:     "15 bytes from zone #1",
			id:       "6",
			affinity: NewNodeMask(1),
			limit:    15,
			types:    TypeMaskDRAM,
			qosClass: "burstable",
			result:   NewNodeMask(0, 1, 2, 3),
			release:  []string{"6"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes, updates, err := a.Allocate(
				ContainerWithTypes(tc.id, tc.name, tc.qosClass, tc.limit, tc.affinity, tc.types),
			)

			if tc.fail {
				require.NotNil(t, err, "unexpected allocation success")
				require.Equal(t, NodeMask(0), nodes, tc.name)
				require.Nil(t, updates, tc.name)
			} else {
				require.Nil(t, err, "unexpected allocation failure")
				require.Equal(t, tc.result, nodes, "allocated nodes")
				require.Equal(t, tc.updates, updates, "updated nodes")
			}

			if len(tc.release) > 0 {
				for _, id := range tc.release {
					err := a.Release(id)
					require.Nil(t, err, "release of ID #"+id)
				}
			}
		})
	}
}
