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
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"k8s.io/utils/cpuset"
)

// NodeMem is memory in bytes per NUMA node.
type NodeMem map[int]int64

// String returns "[node:bytes ...]" with nodes in ascending order.
func (nm NodeMem) String() string {
	nodes := make([]int, 0, len(nm))
	for node := range nm {
		nodes = append(nodes, node)
	}
	sort.Ints(nodes)
	parts := make([]string, len(nodes))
	for i, node := range nodes {
		parts[i] = strconv.Itoa(node) + ":" + strconv.FormatInt(nm[node], 10)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// Plan is a route for memory usage: a sequence of waypoints, and the
// step size bounds used when steering towards them.
//
// Each waypoint gives the target usage per node at the moment when
// the total usage equals the total of the waypoint. Steering
// directs only new allocations, it never frees or moves memory, so
// the usage of every node must be non-decreasing from one waypoint
// to the next. Past the last waypoint there are no more steps: new
// allocations go on to the nodes of the last segment.
type Plan struct {
	Waypoints []NodeMem
	// MinStep is the smallest amount of new allocations in bytes
	// after which the route is re-evaluated.
	MinStep int64
	// MaxStep is the largest amount of new allocations in bytes
	// after which the route must be re-evaluated.
	MaxStep int64
}

// Step tells where the next allocations should be placed.
type Step struct {
	// Nodes are the nodes for new allocations, spread evenly. Empty
	// when the plan has nothing left to steer.
	Nodes []int
	// Bytes is the amount of new allocations after which the next
	// step is due, 0 past the last waypoint.
	Bytes int64
	// Past tells that the usage is past the last waypoint: Nodes are
	// the nodes that the last segment adds usage to.
	Past bool
}

// Validate returns an error if the plan is not usable.
func (p *Plan) Validate() error {
	if p == nil {
		return fmt.Errorf("plan is nil")
	}
	if len(p.Waypoints) == 0 {
		return fmt.Errorf("plan has no waypoints")
	}
	if p.MinStep <= 0 {
		return fmt.Errorf("minStep must be positive, got %d", p.MinStep)
	}
	if p.MaxStep < p.MinStep {
		return fmt.Errorf("maxStep (%d) must not be less than minStep (%d)", p.MaxStep, p.MinStep)
	}
	prev := NodeMem{}
	for i, wp := range p.Waypoints {
		for node, usage := range wp {
			if node < 0 {
				return fmt.Errorf("waypoint %d: invalid NUMA node %d", i, node)
			}
			if usage < 0 {
				return fmt.Errorf("waypoint %d: negative usage %d on NUMA node %d", i, usage, node)
			}
		}
		for node, usage := range prev {
			if wp[node] < usage {
				return fmt.Errorf("waypoint %d: usage %d on NUMA node %d is less than %d in the previous waypoint",
					i, wp[node], node, usage)
			}
		}
		prev = wp
	}
	return nil
}

// Nodes returns the nodes of the waypoints of the plan.
func (p *Plan) Nodes() cpuset.CPUSet {
	nodes := cpuset.New()
	for _, wp := range p.Waypoints {
		for node := range wp {
			nodes = nodes.Union(cpuset.New(node))
		}
	}
	return nodes
}

// String returns the waypoints and step bounds of the plan.
func (p *Plan) String() string {
	wps := make([]string, len(p.Waypoints))
	for i, wp := range p.Waypoints {
		wps[i] = wp.String()
	}
	return fmt.Sprintf("waypoints=[%s] minStep=%d maxStep=%d", strings.Join(wps, " "), p.MinStep, p.MaxStep)
}

// NextStep returns the nodes and the amount of new allocations that
// steer the usage from its current state towards the plan. A nil
// usage means no memory on any node.
func (p *Plan) NextStep(usage NodeMem) (Step, error) {
	if err := p.Validate(); err != nil {
		return Step{}, err
	}
	if usage == nil {
		usage = NodeMem{}
	}

	// Waypoints are non-decreasing, so the reached ones form a
	// prefix. Find the first waypoint that has not been reached.
	next := 0
	for next < len(p.Waypoints) && waypointReached(usage, p.Waypoints[next], p.MinStep) {
		next++
	}

	var prev, target NodeMem
	switch {
	case next >= len(p.Waypoints):
		return Step{Nodes: p.lastNodes(), Past: true}, nil
	case next == 0:
		prev = NodeMem{}
		target = p.Waypoints[0]
	default:
		prev = p.Waypoints[next-1]
		target = p.Waypoints[next]
	}

	nodes, bytes, err := nextStep(usage, prev, target, p.MinStep, p.MaxStep)
	if err != nil {
		return Step{}, err
	}
	return Step{Nodes: nodes, Bytes: bytes}, nil
}

// maxLaggingNodes bounds the number of nodes considered as allocation
// targets in one step. Candidate node subsets are enumerated
// exhaustively, so the cost grows as 2^n.
const maxLaggingNodes = 16

// nextStep returns the nodes and the amount of new allocations that
// bring the current usage closest to the line from the previous
// waypoint to the next waypoint.
//
// Parameters:
//   - usage: current usage per node.
//   - from: previous waypoint, the last one that has been reached.
//   - to: next waypoint. At least from on every node.
//   - minStep: smallest allowed amount of new allocations.
//   - maxStep: largest allowed amount of new allocations.
//
// Returns the nodes for new allocations, the amount of new
// allocations in bytes clamped to [minStep, maxStep], and an error
// for invalid parameters. The nodes are nil when no node is behind
// the next waypoint by at least its share of minStep. New allocations
// are assumed to spread evenly over the returned nodes. If no memory
// is freed while the amount is allocated, no returned node exceeds
// its usage in to (the next waypoint).
func nextStep(usage, from, to NodeMem, minStep, maxStep int64) ([]int, int64, error) {
	if usage == nil || from == nil || to == nil {
		return nil, 0, fmt.Errorf("nextStep: nil input")
	}

	nodeSet := make(map[int]bool)
	for n := range usage {
		nodeSet[n] = true
	}
	for n := range from {
		nodeSet[n] = true
	}
	for n := range to {
		nodeSet[n] = true
	}
	for n := range nodeSet {
		if to[n] < from[n] {
			return nil, 0, fmt.Errorf("nextStep: to[%d]=%d < from[%d]=%d: waypoints must be non-decreasing",
				n, to[n], n, from[n])
		}
	}

	// v = to - from is the direction of the segment, c = usage - from
	// is the position relative to its start.
	v := make(map[int]float64, len(nodeSet))
	c := make(map[int]float64, len(nodeSet))
	var vDotV, cDotV, cDotC float64
	for n := range nodeSet {
		v[n] = float64(to[n] - from[n])
		c[n] = float64(usage[n] - from[n])
		vDotV += v[n] * v[n]
		cDotV += c[n] * v[n]
		cDotC += c[n] * c[n]
	}

	// Lagging nodes have usage below the next waypoint. They are the
	// allocation candidates, largest gap first.
	type nodeGap struct {
		node int
		gap  int64
	}
	var lagging []nodeGap
	for n := range nodeSet {
		if gap := to[n] - usage[n]; gap > 0 {
			lagging = append(lagging, nodeGap{n, gap})
		}
	}
	sort.Slice(lagging, func(i, j int) bool {
		if lagging[i].gap != lagging[j].gap {
			return lagging[i].gap > lagging[j].gap
		}
		return lagging[i].node < lagging[j].node
	})
	if len(lagging) > maxLaggingNodes {
		lagging = lagging[:maxLaggingNodes]
	}

	// Drop nodes that cannot fit minStep even when it is shared by
	// all remaining lagging nodes. With k lagging nodes and even
	// spreading, each gets at least minStep/k bytes. If the smallest
	// gap is below that, the node would exceed to in any subset of
	// size k, and also in any smaller subset where its share grows.
	for len(lagging) > 0 {
		k := int64(len(lagging))
		if k*lagging[len(lagging)-1].gap >= minStep {
			break
		}
		lagging = lagging[:len(lagging)-1]
	}
	if len(lagging) == 0 {
		return nil, 0, nil
	}

	// Enumerate all non-empty subsets of lagging nodes. For each
	// subset find the amount that brings the usage closest to the
	// segment, clamp it to [minStep, min(maxStep, k*minGap)] and
	// keep the subset with the smallest squared distance.
	k := len(lagging)
	bestSE := math.MaxFloat64
	bestCount := 0
	var bestMask uint64
	var bestStep int64

	for mask := uint64(1); mask < (1 << k); mask++ {
		minGap := int64(math.MaxInt64)
		numNodes := int64(0)
		var sumC, sumV float64
		for i := 0; i < k; i++ {
			if mask&(1<<i) != 0 {
				n := lagging[i].node
				numNodes++
				minGap = min(minGap, lagging[i].gap)
				sumC += c[n]
				sumV += v[n]
			}
		}

		lower := minStep
		upper := min(maxStep, numNodes*minGap)
		if lower > upper {
			continue
		}

		// The amount L that minimizes the squared distance from
		// (usage + L/k on the subset) to the segment line.
		fnumNodes := float64(numNodes)
		denom := fnumNodes*vDotV - sumV*sumV
		var optStep float64
		if math.Abs(denom) > 1e-6 {
			optStep = fnumNodes * (cDotV*sumV - sumC*vDotV) / denom
		} else {
			// The allocation direction is parallel to the
			// segment: every amount gives the same distance, so
			// take the largest one to minimize the number of
			// steps.
			optStep = float64(upper)
		}
		step := int64(math.Round(math.Max(float64(lower), math.Min(optStep, float64(upper)))))

		// Squared distance from q = usage + step/k on the subset to
		// the segment line: |q|^2 - (q.v)^2/|v|^2.
		perNode := float64(step) / fnumNodes
		qDotQ := cDotC + 2*perNode*sumC + fnumNodes*perNode*perNode
		qDotV := cDotV + perNode*sumV
		se := qDotQ
		if vDotV > 0 {
			se -= qDotV * qDotV / vDotV
		}

		count := int(numNodes)
		if se < bestSE || (se == bestSE && count > bestCount) {
			bestSE = se
			bestCount = count
			bestMask = mask
			bestStep = step
		}
	}

	if bestMask == 0 {
		return nil, 0, fmt.Errorf("nextStep: no feasible allocation within [%d, %d]", minStep, maxStep)
	}

	var nodes []int
	for i := 0; i < k; i++ {
		if bestMask&(1<<i) != 0 {
			nodes = append(nodes, lagging[i].node)
		}
	}
	sort.Ints(nodes)
	return nodes, bestStep, nil
}

// waypointReached reports whether the usage has reached the waypoint:
// some node exceeds its target, or the total gap of the lagging nodes
// is smaller than minStep so that no step fits before the waypoint.
func waypointReached(usage, wpUsage NodeMem, minStep int64) bool {
	var remainingGap int64
	for n, target := range wpUsage {
		if usage[n] > target {
			return true
		}
		remainingGap += target - usage[n]
	}
	return remainingGap < minStep
}

// lastNodes returns the nodes that the last segment of the plan adds
// usage to, of the latest segment that adds any.
func (p *Plan) lastNodes() []int {
	for i := len(p.Waypoints) - 1; i >= 0; i-- {
		prev := NodeMem{}
		if i > 0 {
			prev = p.Waypoints[i-1]
		}
		var nodes []int
		for n, usage := range p.Waypoints[i] {
			if usage > prev[n] {
				nodes = append(nodes, n)
			}
		}
		if len(nodes) > 0 {
			sort.Ints(nodes)
			return nodes
		}
	}
	return nil
}
