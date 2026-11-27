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

const (
	MiB  int64 = 1024 * 1024
	GiB        = 1024 * MiB
	GiBf       = float64(GiB)
)

// simulateUsageGrowth spreads bytes of new memory usage evenly across
// nodes in cur. A remainder that does not divide evenly is dropped.
func simulateUsageGrowth(cur NodeMem, nodes []int, bytes int64) {
	perNode := bytes / int64(len(nodes))
	for _, n := range nodes {
		cur[n] += perNode
	}
}

// nm constructs a NodeMem from a usage list. The index in the list is
// the node number and the value is its usage.
func nm(usage ...int64) NodeMem {
	m := make(NodeMem, len(usage))
	for node, u := range usage {
		m[node] = u
	}
	return m
}

func TestNextStepTowardsSegment(t *testing.T) {
	// node1
	//   ^ ctpY                     nwp
	// 80x-                      +---x
	//   |        ctpB      3--4-+
	// 70+-        x   +-1-2+      x
	//   |  pwp   +----+ctpL      ctpC
	// 60+-  x----+
	//   |
	// 50+-
	//   |
	// 40+-  x
	//   |  ctpA
	// 30+-
	//   |
	// 20+-
	//   |
	// 10+-
	//   |                          ctpX
	//   +--+--+--+--+--+--+--+--+--x-> node0
	//      20 40 60 80 100  120   140 GiB
	pwp := nm(20*GiB, 60*GiB)
	nwp := nm(140*GiB, 80*GiB)

	tcases := []struct {
		name     string
		ctp      NodeMem
		minStep  int64
		maxStep  int64
		nodes    []int
		bytes    int64
		bytesMax int64
	}{
		{
			name:    "ctpA-1-2: small step towards pwp",
			ctp:     nm(20*GiB+1, 40*GiB),
			minStep: 1 * GiB,
			maxStep: 2 * GiB,
			nodes:   []int{1},
			bytes:   2 * GiB,
		},
		{
			name:    "ctpA-1-20: practically catch pwp",
			ctp:     nm(20*GiB+1, 40*GiB),
			minStep: 1 * GiB,
			maxStep: 20 * GiB,
			nodes:   []int{1},
			bytes:   20 * GiB,
		},
		{
			name:     "ctpA-30-100: catch line between pwp and nwp",
			ctp:      nm(20*GiB+1, 40*GiB),
			minStep:  30 * GiB,
			maxStep:  100 * GiB,
			nodes:    []int{0, 1},
			bytes:    35 * GiB,
			bytesMax: 55 * GiB,
		},
		{
			name:    "ctpB-1-1: small step towards line between pwp and nwp",
			ctp:     nm(50*GiB, 70*GiB),
			minStep: 1 * GiB,
			maxStep: 1 * GiB,
			nodes:   []int{0},
			bytes:   1 * GiB,
		},
		{
			name:     "ctpB-1-100: catch line between pwp and nwp",
			ctp:      nm(50*GiB, 70*GiB),
			minStep:  1 * GiB,
			maxStep:  100 * GiB,
			nodes:    []int{0},
			bytes:    28 * GiB,
			bytesMax: 32 * GiB,
		},
		{
			name:    "ctpC-1-1: small step towards line between pwp and nwp",
			ctp:     nm(130*GiB, 70*GiB),
			minStep: 1 * GiB,
			maxStep: 1 * GiB,
			nodes:   []int{1},
			bytes:   1 * GiB,
		},
		{
			name:     "ctpC-1-10: just reach the line between pwp and nwp",
			ctp:      nm(130*GiB, 70*GiB),
			minStep:  1 * GiB,
			maxStep:  10 * GiB,
			nodes:    []int{1},
			bytes:    8500 * MiB,
			bytesMax: 9500 * MiB,
		},
		{
			name:    "ctpC-1-20: hit exactly nwp on maxStep",
			ctp:     nm(130*GiB, 70*GiB),
			minStep: 1 * GiB,
			maxStep: 20 * GiB,
			nodes:   []int{0, 1},
			bytes:   20 * GiB,
		},
		{
			name:    "ctpC-20-20: hit exactly nwp on step",
			ctp:     nm(130*GiB, 70*GiB),
			minStep: 20 * GiB,
			maxStep: 20 * GiB,
			nodes:   []int{0, 1},
			bytes:   20 * GiB,
		},
		{
			name:    "ctpC-20-100: hit exactly nwp on minStep",
			ctp:     nm(130*GiB, 70*GiB),
			minStep: 20 * GiB,
			maxStep: 100 * GiB,
			nodes:   []int{0, 1},
			bytes:   20 * GiB,
		},
		{
			name:    "ctpL1-10-100: exactly on the line, grow x, go below the line",
			ctp:     nm(80*GiB, 70*GiB),
			minStep: 10 * GiB,
			maxStep: 100 * GiB,
			nodes:   []int{0},
			bytes:   10 * GiB,
		},
		{
			name:    "ctpL2-10-100: continuing right below the line, grow x,y, go above the line",
			ctp:     nm(90*GiB, 70*GiB),
			minStep: 10 * GiB,
			maxStep: 100 * GiB,
			nodes:   []int{0, 1},
			bytes:   10 * GiB,
		},
		{
			name:     "ctpL3-10-100: continuing right above the line, grow x again, find back exactly on line",
			ctp:      nm(95*GiB, 75*GiB),
			minStep:  10 * GiB,
			maxStep:  100 * GiB,
			nodes:    []int{0},
			bytes:    15 * GiB,
			bytesMax: 17 * GiB,
		},
		{
			name:    "ctpY-1-100: max step towards nwp",
			ctp:     nm(0*GiB, 80*GiB),
			minStep: 1 * GiB,
			maxStep: 100 * GiB,
			nodes:   []int{0},
			bytes:   100 * GiB,
		},
		{
			name:    "ctpY-1-200: max step to nwp and no longer",
			ctp:     nm(0*GiB, 80*GiB),
			minStep: 1 * GiB,
			maxStep: 200 * GiB,
			nodes:   []int{0},
			bytes:   140 * GiB,
		},
		{
			name:    "ctpX-1-42: max step towards nwp",
			ctp:     nm(140*GiB, 0*GiB),
			minStep: 1 * GiB,
			maxStep: 42 * GiB,
			nodes:   []int{1},
			bytes:   42 * GiB,
		},
		{
			name:    "ctpX-1-200: max step to nwp and no longer",
			ctp:     nm(140*GiB, 0*GiB),
			minStep: 1 * GiB,
			maxStep: 200 * GiB,
			nodes:   []int{1},
			bytes:   80 * GiB,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, bytes, err := nextStep(tc.ctp, pwp, nwp, tc.minStep, tc.maxStep)
			t.Logf("%s: nodes: %v, bytes: %.3f GiB", tc.name, nodes, float64(bytes)/GiBf)
			require.NoError(t, err)
			require.Equal(t, tc.nodes, nodes)
			if tc.bytesMax != 0 {
				require.GreaterOrEqual(t, bytes, tc.bytes)
				require.LessOrEqual(t, bytes, tc.bytesMax)
			} else {
				require.Equal(t, tc.bytes, bytes)
			}
		})
	}
}

func TestNextStepCases(t *testing.T) {
	t.Run("all-in one lagging node", func(t *testing.T) {
		// n0: pwp-nwp increase, ctp-nwp flat already there
		// n1: pwp-nwp flat, ctp-nwp a lot behind (n1 needs to grow)
		// n2: pwp-nwp flat, ctp-nwp flat
		// n3: pwp-nwp big increase, ctp-nwp tiny increase (n3 has no urgency to grow)
		pwp := nm(00*GiB, 10*GiB, 20*GiB, 0*GiB)
		nwp := nm(10*GiB, 10*GiB, 20*GiB, 30*GiB)
		ctp := nm(10*GiB, 7*GiB, 20*GiB, 29*GiB)
		nodes, bytes, err := nextStep(ctp, pwp, nwp, 1*GiB, 4*GiB)
		require.NoError(t, err)
		require.Equal(t, []int{1}, nodes, "usage should increase only on node1")
		require.Equal(t, 3*GiB, bytes)
	})

	t.Run("split on two lagging nodes", func(t *testing.T) {
		pwp := nm(00*GiB, 10*GiB, 20*GiB, 0*GiB)
		nwp := nm(10*GiB, 10*GiB, 20*GiB, 30*GiB)
		ctp := nm(10*GiB, 8*GiB, 20*GiB, 29*GiB)
		nodes, bytes, err := nextStep(ctp, pwp, nwp, 1*GiB, 4*GiB)
		require.NoError(t, err)
		// Node 1 alone gets closest to the pwp-nwp line.
		require.Equal(t, []int{1}, nodes, "usage should increase only on node1 to get closest to pwp-nwp line")
		require.Equal(t, 2*GiB, bytes)
	})

	t.Run("join to optimal pwp-nwp track rather than minimize square error to nwp", func(t *testing.T) {
		pwp := nm(0, 2, 6, 0)
		nwp := nm(0, 14, 8, 0)
		ctp := nm(0, 6, 4, 0)
		nodes, bytes, err := nextStep(ctp, pwp, nwp, 1, 2)
		require.NoError(t, err)
		require.Equal(t, []int{2}, nodes, "should prefer pwp-nwp path over directly reaching nwp")
		require.Equal(t, int64(2), bytes)
	})

	t.Run("node IDs of 64 and above", func(t *testing.T) {
		pwp := NodeMem{64: 0, 130: 0, 3: 5 * GiB}
		nwp := NodeMem{64: 10 * GiB, 130: 10 * GiB, 3: 5 * GiB}
		ctp := NodeMem{64: 0, 130: 0, 3: 5 * GiB}
		nodes, bytes, err := nextStep(ctp, pwp, nwp, 1*GiB, 4*GiB)
		require.NoError(t, err)
		require.Equal(t, []int{64, 130}, nodes)
		require.Equal(t, 4*GiB, bytes)
	})

	t.Run("more lagging nodes than the enumeration limit", func(t *testing.T) {
		pwp := NodeMem{}
		nwp := NodeMem{}
		ctp := NodeMem{}
		for n := 0; n < maxLaggingNodes+8; n++ {
			nwp[n] = int64(n+1) * GiB
		}
		nodes, bytes, err := nextStep(ctp, pwp, nwp, 1*GiB, 4*GiB)
		require.NoError(t, err)
		require.NotEmpty(t, nodes)
		require.LessOrEqual(t, len(nodes), maxLaggingNodes)
		require.GreaterOrEqual(t, bytes, 1*GiB)
		require.LessOrEqual(t, bytes, 4*GiB)
	})
}

func TestPlanValidate(t *testing.T) {
	valid := &Plan{
		Waypoints: []NodeMem{nm(10*GiB, 0), nm(10*GiB, 5*GiB)},
		MinStep:   1 * GiB,
		MaxStep:   4 * GiB,
	}
	require.NoError(t, valid.Validate())

	var nilPlan *Plan
	require.ErrorContains(t, nilPlan.Validate(), "nil")

	tcases := []struct {
		name string
		plan Plan
		err  string
	}{
		{
			name: "no waypoints",
			plan: Plan{MinStep: 1 * GiB, MaxStep: 1 * GiB},
			err:  "no waypoints",
		},
		{
			name: "zero minStep",
			plan: Plan{Waypoints: valid.Waypoints, MinStep: 0, MaxStep: 4 * GiB},
			err:  "minStep must be positive",
		},
		{
			name: "maxStep smaller than minStep",
			plan: Plan{Waypoints: valid.Waypoints, MinStep: 4 * GiB, MaxStep: 1 * GiB},
			err:  "maxStep",
		},
		{
			name: "decreasing waypoint",
			plan: Plan{
				Waypoints: []NodeMem{nm(10*GiB, 0), nm(5*GiB, 5*GiB)},
				MinStep:   1 * GiB,
				MaxStep:   4 * GiB,
			},
			err: "less than",
		},
		{
			name: "negative usage",
			plan: Plan{Waypoints: []NodeMem{nm(-1)}, MinStep: 1, MaxStep: 1},
			err:  "negative usage",
		},
		{
			name: "negative node",
			plan: Plan{Waypoints: []NodeMem{{-1: 1}}, MinStep: 1, MaxStep: 1},
			err:  "invalid NUMA node",
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, tc.plan.Validate(), tc.err)
			_, err := tc.plan.NextStep(nil)
			require.Error(t, err)
		})
	}
}

func TestPlanNextStepNothingToSteer(t *testing.T) {
	plan := &Plan{
		Waypoints: []NodeMem{nm(0, 0)},
		MinStep:   1 * GiB,
		MaxStep:   4 * GiB,
	}
	// The all-zeroes waypoint is reached immediately and the
	// extrapolated waypoint is all zeroes, too, so there is nowhere
	// to steer.
	step, err := plan.NextStep(nil)
	require.NoError(t, err)
	require.Empty(t, step.Nodes)
	require.Equal(t, int64(0), step.Bytes)
}

// TestPlanFollow simulates the usage that follows the steps of plans of
// several shapes and step settings: every step is within the step bounds,
// every waypoint is reached, and past the last one the step has the nodes
// of the last segment and no bound, with each node close to its target.
func TestPlanFollow(t *testing.T) {
	type shape struct {
		name      string
		waypoints []NodeMem
		last      []int // the nodes of the last segment
	}
	shapes := []shape{
		{"DRAM-then-CXL", []NodeMem{nm(20*GiB, 0), nm(20*GiB, 100*GiB)}, []int{1}},
		{"CXL-then-DRAM", []NodeMem{nm(0, 100*GiB), nm(20*GiB, 100*GiB)}, []int{0}},
		{"interleave-start", []NodeMem{nm(20*GiB, 20*GiB), nm(20*GiB, 100*GiB)}, []int{1}},
		{"interleave-end", []NodeMem{nm(0, 80*GiB), nm(20*GiB, 100*GiB)}, []int{0, 1}},
	}
	type tcase struct {
		name string
		plan Plan
		last []int
	}
	var tcases []tcase
	for _, s := range shapes {
		for _, steps := range []struct {
			name             string
			minStep, maxStep int64
		}{
			{"min1-max1", 1 * GiB, 1 * GiB},
			{"min20-max20", 20 * GiB, 20 * GiB},
			{"min5-max20", 5 * GiB, 20 * GiB},
		} {
			tcases = append(tcases, tcase{s.name + "/" + steps.name, Plan{Waypoints: s.waypoints, MinStep: steps.minStep, MaxStep: steps.maxStep}, s.last})
		}
	}
	tcases = append(tcases, tcase{"four nodes", Plan{
		Waypoints: []NodeMem{
			nm(0*GiB, 2*GiB, 0*GiB, 5*GiB),
			nm(1*GiB, 3*GiB, 1*GiB, 5*GiB),
			nm(2*GiB, 10*GiB, 2*GiB, 5*GiB),
			nm(2*GiB, 12*GiB, 10*GiB, 5*GiB),
		},
		MinStep: 1 * GiB,
		MaxStep: 4 * GiB,
	}, []int{1, 2}})

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			plan := &tc.plan
			cur := NodeMem{}
			reached := make([]bool, len(plan.Waypoints))
			past := false
			for i := 0; i < 1000 && !past; i++ {
				step, err := plan.NextStep(cur)
				require.NoError(t, err, "iteration %d", i)
				t.Logf("iteration %3d: usage %s step %v/%d", i, cur, step.Nodes, step.Bytes)
				if step.Past {
					require.Equal(t, Step{Nodes: tc.last, Past: true}, step)
					past = true
					break
				}
				require.NotEmpty(t, step.Nodes, "iteration %d", i)
				require.GreaterOrEqual(t, step.Bytes, plan.MinStep, "iteration %d", i)
				require.LessOrEqual(t, step.Bytes, plan.MaxStep, "iteration %d", i)
				simulateUsageGrowth(cur, step.Nodes, step.Bytes)
				// reached: less than MinStep short of the waypoint in total
				for wi, wp := range plan.Waypoints {
					var short int64
					for n, target := range wp {
						short += max(0, target-cur[n])
					}
					reached[wi] = reached[wi] || short < plan.MinStep
				}
			}
			require.True(t, past, "the usage does not get past the last waypoint")
			for wi, ok := range reached {
				require.True(t, ok, "waypoint %d %s not reached, usage %s", wi, plan.Waypoints[wi], cur)
			}
			// steering is approximate: each node within MaxStep of its target
			for n, target := range plan.Waypoints[len(plan.Waypoints)-1] {
				require.InDelta(t, target, cur[n], float64(plan.MaxStep), "node %d", n)
			}
		})
	}
}

// TestNextStepPastLastWaypoint checks the step of each policy at and past
// the last waypoint (the nodes of the last segment that adds usage, no
// bound) and below it again (a bounded step towards it).
func TestNextStepPastLastWaypoint(t *testing.T) {
	both := MemoryTypes{DRAMNUMANodes: []int{0}, CXLNUMANodes: []int{1, 2}, DRAMQuota: 4 * GiB, CXLQuota: 8 * GiB}
	for _, tc := range []struct {
		policy Policy
		mem    MemoryTypes
		nodes  []int
	}{
		{PolicyFirstDRAM, both, []int{1, 2}},
		{PolicyFirstCXL, both, []int{0}},
		{PolicyStartInterleaved, both, []int{1, 2}},
		{PolicyEndInterleaved, both, []int{0, 1, 2}},
		// equal quotas: the last segment adds nothing, the one before
		// interleaves
		{PolicyStartInterleaved, MemoryTypes{DRAMNUMANodes: []int{0}, CXLNUMANodes: []int{1}, DRAMQuota: 4 * GiB, CXLQuota: 4 * GiB}, []int{0, 1}},
		{PolicyFirstDRAM, MemoryTypes{CXLNUMANodes: []int{1, 2}, CXLQuota: 8 * GiB}, []int{1, 2}},
		{PolicyFirstCXL, MemoryTypes{DRAMNUMANodes: []int{0}, DRAMQuota: 4 * GiB}, []int{0}},
	} {
		t.Run(tc.policy.String(), func(t *testing.T) {
			plan, err := NewPlan(tc.policy, tc.mem, nil, 0, 0)
			require.NoError(t, err)
			last := plan.Waypoints[len(plan.Waypoints)-1]
			beyond := NodeMem{}
			for n, usage := range last {
				beyond[n] = usage + 3*GiB
			}
			for _, usage := range []NodeMem{last, beyond} {
				step, err := plan.NextStep(usage)
				require.NoError(t, err)
				require.Equal(t, Step{Nodes: tc.nodes, Past: true}, step, "usage %s", usage)
			}
			below := NodeMem{}
			for n, usage := range last {
				below[n] = usage
			}
			below[tc.nodes[0]] -= plan.MinStep
			step, err := plan.NextStep(below)
			require.NoError(t, err)
			require.False(t, step.Past, "usage %s", below)
			require.Equal(t, []int{tc.nodes[0]}, step.Nodes)
			require.GreaterOrEqual(t, step.Bytes, plan.MinStep)
		})
	}
}

func TestNodeMemString(t *testing.T) {
	require.Equal(t, "[]", NodeMem{}.String())
	require.Equal(t, "[0:4096 3:0 130:1]", NodeMem{130: 1, 0: 4096, 3: 0}.String())
	plan := &Plan{
		Waypoints: []NodeMem{nm(1, 0), nm(1, 2)},
		MinStep:   3,
		MaxStep:   4,
	}
	require.Equal(t, "waypoints=[[0:1 1:0] [0:1 1:2]] minStep=3 maxStep=4", plan.String())
}
