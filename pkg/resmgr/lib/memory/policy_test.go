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

package libmem

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

func TestMemoryPolicyNodes(t *testing.T) {
	// Nodes 0-3 have CPUs, 0 and 2 in package 0, 1 and 3 in package 1.
	// Nodes 4-7 have memory only. From node 1, node 3 is at distance 11
	// and node 6 at 17.
	sys, err := sysfs.DiscoverSystemAt("./testdata/sample2/sys")
	require.NoError(t, err)

	var (
		node1CPUs = cpuset.New(1, 5)
		mems      = NewNodeMask(0, 1)
	)

	for _, tc := range []struct {
		mode   string
		nodes  string
		cpus   cpuset.CPUSet
		expect NodeMask
		fail   bool
	}{
		{mode: "MPOL_INTERLEAVE", nodes: "all", cpus: node1CPUs, expect: NewNodeMask(0, 1, 2, 3, 4, 5, 6, 7)},
		{mode: "MPOL_INTERLEAVE", nodes: "allowed-mems", cpus: node1CPUs, expect: mems},
		{mode: "MPOL_INTERLEAVE", nodes: "", cpus: node1CPUs, expect: mems},
		{mode: "MPOL_INTERLEAVE", nodes: "cpu-nodes", cpus: node1CPUs, expect: NewNodeMask(1)},
		{mode: "MPOL_INTERLEAVE", nodes: "cpu-nodes", cpus: cpuset.New(0, 1), expect: NewNodeMask(0, 1)},
		{mode: "MPOL_INTERLEAVE", nodes: "cpu-packages", cpus: node1CPUs, expect: NewNodeMask(1, 3)},
		{mode: "MPOL_INTERLEAVE", nodes: "max-dist:11", cpus: node1CPUs, expect: NewNodeMask(1, 3)},
		{mode: "MPOL_INTERLEAVE", nodes: "max-dist:17", cpus: node1CPUs, expect: NewNodeMask(1, 3, 6)},
		{mode: "MPOL_INTERLEAVE", nodes: "0,2-3", cpus: node1CPUs, expect: NewNodeMask(0, 2, 3)},
		{mode: "MPOL_INTERLEAVE", nodes: "max-dist:x", cpus: node1CPUs, fail: true},
		{mode: "MPOL_INTERLEAVE", nodes: "max-dist:10", cpus: node1CPUs, expect: NewNodeMask(1)},
		{mode: "MPOL_INTERLEAVE", nodes: "max-dist:9", cpus: node1CPUs, fail: true},
		{mode: "MPOL_INTERLEAVE", nodes: "max-dist:0", cpus: node1CPUs, fail: true},
		{mode: "MPOL_INTERLEAVE", nodes: "max-dist:-1", cpus: node1CPUs, fail: true},
		{mode: "MPOL_INTERLEAVE", nodes: "1-x", cpus: node1CPUs, fail: true},
		{mode: "MPOL_INTERLEAVE", nodes: "bogus", cpus: node1CPUs, fail: true},
		{mode: "MPOL_LOCAL", nodes: "", cpus: node1CPUs, expect: NewNodeMask()},
		{mode: "MPOL_DEFAULT", nodes: "", cpus: node1CPUs, expect: NewNodeMask()},
		{mode: "MPOL_LOCAL", nodes: "0", cpus: node1CPUs, fail: true},
		{mode: "MPOL_DEFAULT", nodes: "all", cpus: node1CPUs, fail: true},
	} {
		t.Run(tc.mode+" "+tc.nodes, func(t *testing.T) {
			mask, err := MemoryPolicyNodes(sys, tc.mode, tc.nodes, tc.cpus, mems)
			if tc.fail {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expect.MemsetString(), mask.MemsetString())
		})
	}
}
