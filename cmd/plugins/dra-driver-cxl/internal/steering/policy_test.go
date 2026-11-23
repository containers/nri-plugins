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

package steering

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPolicyPlanOrders(t *testing.T) {
	mem := MemoryTypes{
		DRAMNUMANodes: []int{0},
		CXLNUMANodes:  []int{1},
		DRAMQuota:     20 * GiB,
		CXLQuota:      100 * GiB,
	}
	tcases := []struct {
		policy    Policy
		waypoints []NodeMem
	}{
		{PolicyFirstDRAM, []NodeMem{nm(20*GiB, 0), nm(20*GiB, 100*GiB)}},
		{PolicyFirstCXL, []NodeMem{nm(0, 100*GiB), nm(20*GiB, 100*GiB)}},
		{PolicyStartInterleaved, []NodeMem{nm(20*GiB, 20*GiB), nm(20*GiB, 100*GiB)}},
		{PolicyEndInterleaved, []NodeMem{nm(0, 80*GiB), nm(20*GiB, 100*GiB)}},
	}
	for _, tc := range tcases {
		t.Run(tc.policy.String(), func(t *testing.T) {
			plan, err := NewPlan(tc.policy, mem, nil, 0, 0)
			require.NoError(t, err)
			require.Equal(t, tc.waypoints, plan.Waypoints)
			// maxStep defaults to the total quota, minStep to the
			// smallest increment between waypoints
			require.Equal(t, 20*GiB, plan.MinStep)
			require.Equal(t, 120*GiB, plan.MaxStep)
			// and the plan steers from no usage on
			step, err := plan.NextStep(NodeMem{})
			require.NoError(t, err)
			require.NotEmpty(t, step.Nodes, "first step")
		})
	}

	t.Run("larger DRAM quota with end-interleaved", func(t *testing.T) {
		plan, err := NewPlan(PolicyEndInterleaved, MemoryTypes{
			DRAMNUMANodes: []int{0}, CXLNUMANodes: []int{1}, DRAMQuota: 100 * GiB, CXLQuota: 20 * GiB,
		}, nil, 0, 0)
		require.NoError(t, err)
		require.Equal(t, []NodeMem{nm(80*GiB, 0), nm(100*GiB, 20*GiB)}, plan.Waypoints)
	})

	t.Run("single type spreads over nodes", func(t *testing.T) {
		for _, policy := range []Policy{PolicyFirstDRAM, PolicyFirstCXL, PolicyStartInterleaved, PolicyEndInterleaved} {
			plan, err := NewPlan(policy, MemoryTypes{
				DRAMNUMANodes: []int{0, 2}, CXLNUMANodes: []int{1}, DRAMQuota: 20 * GiB,
			}, nil, 0, 0)
			require.NoError(t, err, policy)
			require.Equal(t, []NodeMem{{0: 10 * GiB, 2: 10 * GiB}}, plan.Waypoints, policy)
			require.Equal(t, 20*GiB, plan.MaxStep, policy)
		}
	})
}

func TestPolicyPlanSteps(t *testing.T) {
	mem := MemoryTypes{DRAMNUMANodes: []int{0}, CXLNUMANodes: []int{1}, DRAMQuota: 2 * GiB, CXLQuota: 2 * GiB}

	plan, err := NewPlan(PolicyFirstDRAM, mem, nil, 1*GiB, 0)
	require.NoError(t, err)
	require.Equal(t, 1*GiB, plan.MinStep)
	require.Equal(t, 4*GiB, plan.MaxStep, "maxStep defaults to the total quota")

	plan, err = NewPlan(PolicyFirstDRAM, mem, nil, 0, 512*MiB)
	require.NoError(t, err)
	require.Equal(t, 512*MiB, plan.MinStep, "minStep defaults to maxStep")
	require.Equal(t, 512*MiB, plan.MaxStep)

	plan, err = NewPlan(PolicyFirstDRAM, mem, nil, 8*GiB, 0)
	require.NoError(t, err)
	require.Equal(t, 8*GiB, plan.MinStep)
	require.Equal(t, 8*GiB, plan.MaxStep, "maxStep is raised to minStep")

	plan, err = NewPlan(PolicyFirstDRAM, mem, nil, 16*MiB, 64*MiB)
	require.NoError(t, err)
	require.Equal(t, 16*MiB, plan.MinStep)
	require.Equal(t, 64*MiB, plan.MaxStep)

	// one node of each type: the only increments are the quotas
	plan, err = NewPlan(PolicyFirstDRAM, mem, nil, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 2*GiB, plan.MinStep, "minStep defaults to the smallest increment between waypoints")
	require.Equal(t, 4*GiB, plan.MaxStep)
}

func TestPolicyPlanDefaultStepsInterleaved(t *testing.T) {
	// 128Mi of DRAM on two nodes and 128Mi of CXL: the first waypoint
	// is 64Mi, 64Mi and 128Mi, which no even step of the whole quota
	// reaches
	mem := MemoryTypes{DRAMNUMANodes: []int{0, 1}, CXLNUMANodes: []int{2}, DRAMQuota: 128 * MiB, CXLQuota: 128 * MiB}
	plan, err := NewPlan(PolicyStartInterleaved, mem, nil, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 64*MiB, plan.MinStep)
	require.Equal(t, 256*MiB, plan.MaxStep)

	cur := NodeMem{}
	for i := 0; i < 10; i++ {
		step, err := plan.NextStep(cur)
		require.NoError(t, err)
		if step.Past {
			break
		}
		require.GreaterOrEqual(t, step.Bytes, plan.MinStep)
		require.LessOrEqual(t, step.Bytes, plan.MaxStep)
		for _, n := range step.Nodes {
			cur[n] += step.Bytes / int64(len(step.Nodes))
		}
	}
	require.Equal(t, NodeMem{0: 64 * MiB, 1: 64 * MiB, 2: 128 * MiB}, cur, "the usage reaches the waypoint")
}

func TestPolicyPlanUserWaypoints(t *testing.T) {
	waypoints := []TypeUsage{
		{DRAM: 100 * MiB, CXL: 50 * MiB},
		{DRAM: 100 * MiB, CXL: 200 * MiB},
		{DRAM: 300 * MiB, CXL: 250 * MiB},
	}
	mem := MemoryTypes{DRAMNUMANodes: []int{2}, CXLNUMANodes: []int{3, 4}, DRAMQuota: 300 * MiB, CXLQuota: 300 * MiB}
	plan, err := NewPlan(PolicyWaypoints, mem, waypoints, 20*MiB, 60*MiB)
	require.NoError(t, err)
	require.Equal(t, []NodeMem{
		{2: 100 * MiB, 3: 25 * MiB, 4: 25 * MiB},
		{2: 100 * MiB, 3: 100 * MiB, 4: 100 * MiB},
		{2: 300 * MiB, 3: 125 * MiB, 4: 125 * MiB},
	}, plan.Waypoints)
	require.Equal(t, 20*MiB, plan.MinStep)
	require.Equal(t, 60*MiB, plan.MaxStep)

	_, err = NewPlan(PolicyWaypoints, MemoryTypes{DRAMNUMANodes: []int{2}, DRAMQuota: 300 * MiB}, waypoints, 0, 0)
	require.ErrorContains(t, err, "no CXL memory")
	_, err = NewPlan(PolicyWaypoints, MemoryTypes{DRAMNUMANodes: []int{2}, CXLNUMANodes: []int{3, 4}, DRAMQuota: 300 * MiB, CXLQuota: 200 * MiB},
		waypoints, 0, 0)
	require.ErrorContains(t, err, "waypoint 2: CXL usage 262144000 exceeds the CXL quota 209715200")
}

func TestPolicyPlanErrors(t *testing.T) {
	_, err := NewPlan(PolicyFirstDRAM, MemoryTypes{}, nil, 0, 0)
	require.ErrorContains(t, err, "no memory type")
	_, err = NewPlan(PolicyFirstDRAM, MemoryTypes{DRAMNUMANodes: []int{0}, DRAMQuota: -1}, nil, 0, 0)
	require.ErrorContains(t, err, "negative")
	_, err = NewPlan(PolicyFirstDRAM, MemoryTypes{DRAMNUMANodes: []int{0}, CXLNUMANodes: []int{0}, DRAMQuota: 1, CXLQuota: 1}, nil, 0, 0)
	require.ErrorContains(t, err, "more than once")
	_, err = NewPlan(PolicyFirstDRAM, MemoryTypes{DRAMNUMANodes: []int{-1}, DRAMQuota: 1}, nil, 0, 0)
	require.ErrorContains(t, err, "invalid NUMA node")
	both := MemoryTypes{DRAMNUMANodes: []int{0}, CXLNUMANodes: []int{1}, DRAMQuota: 1, CXLQuota: 1}
	_, err = NewPlan(Policy(42), both, nil, 0, 0)
	require.ErrorContains(t, err, "Policy(42)")
	_, err = NewPlan(PolicyWaypoints, both, nil, 0, 0)
	require.ErrorContains(t, err, "waypoints")
	_, err = NewPlan(PolicyFirstDRAM, both, []TypeUsage{{DRAM: 1}}, 0, 0)
	require.ErrorContains(t, err, "waypoints")
}
