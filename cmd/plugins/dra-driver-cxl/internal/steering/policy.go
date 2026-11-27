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
	"maps"
	"strconv"
)

// Policy is the order in which a plan consumes the DRAM and CXL quotas.
type Policy int

const (
	// PolicyFirstDRAM consumes the DRAM quota first, then CXL.
	PolicyFirstDRAM Policy = iota
	// PolicyFirstCXL consumes the CXL quota first, then DRAM.
	PolicyFirstCXL
	// PolicyStartInterleaved interleaves DRAM and CXL until the
	// smaller quota is used, then consumes the rest of the other.
	PolicyStartInterleaved
	// PolicyEndInterleaved consumes the excess of the larger quota
	// first, then interleaves DRAM and CXL.
	PolicyEndInterleaved
	// PolicyWaypoints follows the waypoints of the user.
	PolicyWaypoints
)

// String returns the name of the policy in the container settings.
func (p Policy) String() string {
	switch p {
	case PolicyFirstDRAM:
		return "first-dram"
	case PolicyFirstCXL:
		return "first-cxl"
	case PolicyStartInterleaved:
		return "start-interleaved"
	case PolicyEndInterleaved:
		return "end-interleaved"
	case PolicyWaypoints:
		return "waypoints"
	}
	return "Policy(" + strconv.Itoa(int(p)) + ")"
}

// MemoryTypes lists the NUMA nodes and the byte quota of each memory
// type. A type with no nodes or a zero quota is absent.
type MemoryTypes struct {
	DRAMNUMANodes []int
	CXLNUMANodes  []int
	DRAMQuota     int64
	CXLQuota      int64
}

// TypeUsage is a target usage in bytes per memory type.
type TypeUsage struct {
	DRAM int64
	CXL  int64
}

// NewPlan returns the plan that consumes the quotas of the memory types
// in the order of the policy.
//   - waypoints: the target usages of PolicyWaypoints, none for the
//     other policies
//   - minStep, maxStep: the step bounds in bytes, 0 for the default
func NewPlan(policy Policy, mem MemoryTypes, waypoints []TypeUsage, minStep, maxStep int64) (*Plan, error) {
	if err := mem.validate(); err != nil {
		return nil, err
	}
	if (policy == PolicyWaypoints) != (len(waypoints) > 0) {
		return nil, fmt.Errorf("policy %s with %d waypoints", policy, len(waypoints))
	}
	var nodeWaypoints []NodeMem
	var err error
	if policy == PolicyWaypoints {
		nodeWaypoints, err = userWaypoints(waypoints, mem)
	} else {
		nodeWaypoints, err = generateWaypoints(policy, mem)
	}
	if err != nil {
		return nil, err
	}

	if maxStep == 0 {
		maxStep = mem.quota()
	}
	if minStep == 0 {
		// A step spreads evenly over its nodes: a minStep larger than
		// what a node gains towards a waypoint would make the waypoint
		// unreachable.
		minStep = maxStep
		if inc := smallestIncrement(nodeWaypoints); inc > 0 {
			minStep = min(minStep, inc)
		}
	}
	maxStep = max(maxStep, minStep)

	plan := &Plan{
		Waypoints: nodeWaypoints,
		MinStep:   minStep,
		MaxStep:   maxStep,
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// hasDRAM reports whether DRAM has nodes and a quota.
func (mem MemoryTypes) hasDRAM() bool {
	return len(mem.DRAMNUMANodes) > 0 && mem.DRAMQuota > 0
}

// hasCXL reports whether CXL has nodes and a quota.
func (mem MemoryTypes) hasCXL() bool {
	return len(mem.CXLNUMANodes) > 0 && mem.CXLQuota > 0
}

// quota returns the total quota of the present memory types.
func (mem MemoryTypes) quota() int64 {
	var total int64
	if mem.hasDRAM() {
		total += mem.DRAMQuota
	}
	if mem.hasCXL() {
		total += mem.CXLQuota
	}
	return total
}

// validate returns an error if the memory types are not usable.
func (mem MemoryTypes) validate() error {
	if mem.DRAMQuota < 0 || mem.CXLQuota < 0 {
		return fmt.Errorf("memory quotas must not be negative")
	}
	if !mem.hasDRAM() && !mem.hasCXL() {
		return fmt.Errorf("no memory type has both nodes and a quota")
	}
	seen := make(map[int]bool)
	for _, node := range append(append([]int{}, mem.DRAMNUMANodes...), mem.CXLNUMANodes...) {
		if node < 0 {
			return fmt.Errorf("invalid NUMA node %d", node)
		}
		if seen[node] {
			return fmt.Errorf("NUMA node %d listed more than once", node)
		}
		seen[node] = true
	}
	return nil
}

// spread returns a NodeMem with total divided evenly over the nodes.
func spread(nodes []int, total int64) NodeMem {
	nm := NodeMem{}
	if len(nodes) == 0 {
		return nm
	}
	perNode := total / int64(len(nodes))
	for _, node := range nodes {
		nm[node] = perNode
	}
	return nm
}

// typeWaypoint returns a waypoint that spreads the type usages over
// the nodes of the present memory types.
func typeWaypoint(mem MemoryTypes, usage TypeUsage) NodeMem {
	nm := NodeMem{}
	if mem.hasDRAM() {
		maps.Copy(nm, spread(mem.DRAMNUMANodes, usage.DRAM))
	}
	if mem.hasCXL() {
		maps.Copy(nm, spread(mem.CXLNUMANodes, usage.CXL))
	}
	return nm
}

// generateWaypoints returns the waypoints of a built-in policy for the
// memory types.
func generateWaypoints(policy Policy, mem MemoryTypes) ([]NodeMem, error) {
	all := TypeUsage{DRAM: mem.DRAMQuota, CXL: mem.CXLQuota}
	if !mem.hasDRAM() {
		all.DRAM = 0
	}
	if !mem.hasCXL() {
		all.CXL = 0
	}

	// With a single memory type every policy consumes it alone.
	if !mem.hasDRAM() || !mem.hasCXL() {
		return []NodeMem{typeWaypoint(mem, all)}, nil
	}

	var first TypeUsage
	switch policy {
	case PolicyFirstDRAM:
		first = TypeUsage{DRAM: all.DRAM}
	case PolicyFirstCXL:
		first = TypeUsage{CXL: all.CXL}
	case PolicyStartInterleaved:
		smaller := min(all.DRAM, all.CXL)
		first = TypeUsage{DRAM: smaller, CXL: smaller}
	case PolicyEndInterleaved:
		if all.DRAM > all.CXL {
			first = TypeUsage{DRAM: all.DRAM - all.CXL}
		} else {
			first = TypeUsage{CXL: all.CXL - all.DRAM}
		}
	default:
		return nil, fmt.Errorf("cannot generate waypoints for policy %s", policy)
	}
	return []NodeMem{typeWaypoint(mem, first), typeWaypoint(mem, all)}, nil
}

// userWaypoints returns the waypoints of user target usages spread over
// the nodes of the memory types, and an error for a usage above the quota
// of its type.
func userWaypoints(usages []TypeUsage, mem MemoryTypes) ([]NodeMem, error) {
	waypoints := make([]NodeMem, 0, len(usages))
	for i, usage := range usages {
		if usage.DRAM > 0 && !mem.hasDRAM() {
			return nil, fmt.Errorf("waypoint %d: DRAM usage specified but no DRAM memory", i)
		}
		if usage.CXL > 0 && !mem.hasCXL() {
			return nil, fmt.Errorf("waypoint %d: CXL usage specified but no CXL memory", i)
		}
		if usage.DRAM > mem.DRAMQuota {
			return nil, fmt.Errorf("waypoint %d: DRAM usage %d exceeds the DRAM quota %d", i, usage.DRAM, mem.DRAMQuota)
		}
		if usage.CXL > mem.CXLQuota {
			return nil, fmt.Errorf("waypoint %d: CXL usage %d exceeds the CXL quota %d", i, usage.CXL, mem.CXLQuota)
		}
		waypoints = append(waypoints, typeWaypoint(mem, usage))
	}
	return waypoints, nil
}

// smallestIncrement returns the smallest increase of the usage of a node
// from one waypoint to the next, starting from no usage, 0 if no usage
// increases.
func smallestIncrement(waypoints []NodeMem) int64 {
	var smallest int64
	prev := NodeMem{}
	for _, wp := range waypoints {
		for n, usage := range wp {
			if inc := usage - prev[n]; inc > 0 && (smallest == 0 || inc < smallest) {
				smallest = inc
			}
		}
		prev = wp
	}
	return smallest
}
