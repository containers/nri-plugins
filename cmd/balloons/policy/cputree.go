// Copyright 2022 Intel Corporation. All Rights Reserved.
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

package balloons

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

type CPUTopologyLevel int

const (
	CPUTopologyLevelUndefined CPUTopologyLevel = iota
	CPUTopologyLevelSystem
	CPUTopologyLevelPackage
	CPUTopologyLevelDie
	CPUTopologyLevelNuma
	CPUTopologyLevelCore
	CPUTopologyLevelThread
	CPUTopologyLevelCount
)

// CpuTreeNode is a node in the CPU tree.
type CPUTreeNode struct {
	name     string
	level    CPUTopologyLevel
	parent   *CPUTreeNode
	children []*CPUTreeNode
	cpus     cpuset.CPUSet // union of CPUs of child nodes

}

// cpuTreeNodeAttributes contains various attributes of a CPU tree
// node. When allocating or releasing CPUs, all CPU tree nodes in
// which allocating/releasing could be possible are stored to the same
// slice with these attributes. The attributes contain all necessary
// information for comparing which nodes are the best choices for
// allocating/releasing, thus traversing the tree is not needed in the
// comparison phase.
type CPUTreeNodeAttributes struct {
	t                *CPUTreeNode
	depth            int
	currentCpus      cpuset.CPUSet
	freeCpus         cpuset.CPUSet
	currentCPUCount  int
	currentCPUCounts []int
	freeCPUCount     int
	freeCPUCounts    []int
}

// cpuTreeAllocator allocates CPUs from the branch of a CPU tree
// where the "root" node is the topmost CPU of the branch.
type CPUTreeAllocator struct {
	options cpuTreeAllocatorOptions
	root    *CPUTreeNode
}

// cpuTreeAllocatorOptions contains parameters for the CPU allocator
// that that selects CPUs from a CPU tree.
type cpuTreeAllocatorOptions struct {
	// topologyBalancing true prefers allocating from branches
	// with most free CPUs (spread allocations), while false is
	// the opposite (packed allocations).
	topologyBalancing bool
}

// Strings returns topology level as a string
func (ctl CPUTopologyLevel) String() string {
	s, ok := cpuTopologyLevelToString[ctl]
	if ok {
		return s
	}
	return fmt.Sprintf("CPUTopologyLevelUnknown(%d)", ctl)
}

// cpuTopologyLevelToString defines names for all CPU topology levels.
var cpuTopologyLevelToString = map[CPUTopologyLevel]string{
	CPUTopologyLevelUndefined: "",
	CPUTopologyLevelSystem:    "system",
	CPUTopologyLevelPackage:   "package",
	CPUTopologyLevelDie:       "die",
	CPUTopologyLevelNuma:      "numa",
	CPUTopologyLevelCore:      "core",
	CPUTopologyLevelThread:    "thread",
}

// MarshalJSON()
func (ctl CPUTopologyLevel) MarshalJSON() ([]byte, error) {
	return json.Marshal(ctl.String())
}

// UnmarshalJSON unmarshals a JSON string to CPUTopologyLevel
func (ctl *CPUTopologyLevel) UnmarshalJSON(data []byte) error {
	var dataString string
	if err := json.Unmarshal(data, &dataString); err != nil {
		return err
	}
	name := strings.ToLower(dataString)
	for ctlConst, ctlString := range cpuTopologyLevelToString {
		if ctlString == name {
			*ctl = ctlConst
			return nil
		}
	}
	return fmt.Errorf("invalid CPU topology level %q", name)
}

// String returns string representation of a CPU tree node.
func (t *CPUTreeNode) String() string {
	if len(t.children) == 0 {
		return t.name
	}
	return fmt.Sprintf("%s%v", t.name, t.children)
}

// String returns CPUTreeNodeAttributes as a string.
func (tna CPUTreeNodeAttributes) String() string {
	return fmt.Sprintf("%s{%d,%v,%d,%d}", tna.t.name, tna.depth,
		tna.currentCPUCounts,
		tna.freeCPUCount, tna.freeCPUCounts)
}

// NewCpuTree returns a named CPU tree node.
func NewCPUTree(name string) *CPUTreeNode {
	return &CPUTreeNode{
		name: name,
		cpus: cpuset.NewCPUSet(),
	}
}

// AddChild adds new child node to a CPU tree node.
func (t *CPUTreeNode) AddChild(child *CPUTreeNode) {
	child.parent = t
	t.children = append(t.children, child)
}

// AddCpus adds CPUs to a CPU tree node and all its parents.
func (t *CPUTreeNode) AddCpus(cpus cpuset.CPUSet) {
	t.cpus = t.cpus.Union(cpus)
	if t.parent != nil {
		t.parent.AddCpus(cpus)
	}
}

// Cpus returns CPUs of a CPU tree node.
func (t *CPUTreeNode) Cpus() cpuset.CPUSet {
	return t.cpus
}

// WalkSkipChildren error returned from a DepthFirstWalk handler
// prevents walking deeper in the tree. The caller of the
// DepthFirstWalk will get no error.
var ErrWalkSkipChildren = errors.New("skip children")

// WalkStop error returned from a DepthFirstWalk handler stops the
// walk altogether. The caller of the DepthFirstWalk will get the
// WalkStop error.
var ErrWalkStop = errors.New("stop")

// DepthFirstWalk walks through nodes in a CPU tree. Every node is
// passed to the handler callback that controls next step by
// returning:
// - nil: continue walking to the next node
// - WalkSkipChildren: continue to the next node but skip children of this node
// - WalkStop: stop walking.
func (t *CPUTreeNode) DepthFirstWalk(handler func(*CPUTreeNode) error) error {
	if err := handler(t); err != nil {
		if err == ErrWalkSkipChildren {
			return nil
		}
		return err
	}
	for _, child := range t.children {
		if err := child.DepthFirstWalk(handler); err != nil {
			return err
		}
	}
	return nil
}

// CPULocations returns a slice where each element contains names of
// topology elements over which a set of CPUs spans. Example:
// systemNode.CPULocations(cpuset:0,99) = [["system"],["p0", "p1"], ["p0d0", "p1d0"], ...]
func (t *CPUTreeNode) CPULocations(cpus cpuset.CPUSet) [][]string {
	names := make([][]string, int(CPUTopologyLevelCount)-int(t.level))
	t.DepthFirstWalk(func(tn *CPUTreeNode) error {
		if tn.cpus.Intersection(cpus).Size() == 0 {
			return ErrWalkSkipChildren
		}
		levelIndex := int(tn.level) - int(t.level)
		names[levelIndex] = append(names[levelIndex], tn.name)
		return nil
	})
	return names
}

// NewCpuTreeFromSystem returns the root node of the topology tree
// constructed from the underlying system.
func NewCPUTreeFromSystem() (*CPUTreeNode, error) {
	sys, err := system.DiscoverSystem(system.DiscoverCPUTopology)
	if err != nil {
		return nil, err
	}
	// TODO: split deep nested loops into functions
	sysTree := NewCPUTree("system")
	sysTree.level = CPUTopologyLevelSystem
	for _, packageID := range sys.PackageIDs() {
		packageTree := NewCPUTree(fmt.Sprintf("p%d", packageID))
		packageTree.level = CPUTopologyLevelPackage
		cpuPackage := sys.Package(packageID)
		sysTree.AddChild(packageTree)
		for _, dieID := range cpuPackage.DieIDs() {
			dieTree := NewCPUTree(fmt.Sprintf("p%dd%d", packageID, dieID))
			dieTree.level = CPUTopologyLevelDie
			packageTree.AddChild(dieTree)
			for _, nodeID := range cpuPackage.DieNodeIDs(dieID) {
				nodeTree := NewCPUTree(fmt.Sprintf("p%dd%dn%d", packageID, dieID, nodeID))
				nodeTree.level = CPUTopologyLevelNuma
				dieTree.AddChild(nodeTree)
				node := sys.Node(nodeID)
				for _, cpuID := range node.CPUSet().ToSlice() {
					cpuTree := NewCPUTree(fmt.Sprintf("p%dd%dn%dcpu%d", packageID, dieID, nodeID, cpuID))

					cpuTree.level = CPUTopologyLevelCore
					nodeTree.AddChild(cpuTree)
					cpu := sys.CPU(cpuID)
					for _, threadID := range cpu.ThreadCPUSet().ToSlice() {
						threadTree := NewCPUTree(fmt.Sprintf("p%dd%dn%dcpu%dt%d", packageID, dieID, nodeID, cpuID, threadID))
						threadTree.level = CPUTopologyLevelThread
						cpuTree.AddChild(threadTree)
						threadTree.AddCpus(cpuset.NewCPUSet(threadID))
					}
				}
			}
		}
	}
	return sysTree, nil
}

// ToAttributedSlice returns a CPU tree node and recursively all its
// child nodes in a slice that contains nodes with their attributes
// for allocation/releasing comparison.
// - currentCpus is the set of CPUs that can be freed in coming operation
// - freeCpus is the set of CPUs that can be allocated in coming operation
// - filter(tna) returns false if the node can be ignored
func (t *CPUTreeNode) ToAttributedSlice(
	currentCpus, freeCpus cpuset.CPUSet,
	filter func(*CPUTreeNodeAttributes) bool) []CPUTreeNodeAttributes {
	tnas := []CPUTreeNodeAttributes{}
	currentCPUCounts := []int{}
	freeCPUCounts := []int{}
	t.toAttributedSlice(currentCpus, freeCpus, filter, &tnas, 0, currentCPUCounts, freeCPUCounts)
	return tnas
}

func (t *CPUTreeNode) toAttributedSlice(
	currentCpus, freeCpus cpuset.CPUSet,
	filter func(*CPUTreeNodeAttributes) bool,
	tnas *[]CPUTreeNodeAttributes,
	depth int,
	currentCPUCounts []int,
	freeCPUCounts []int) {
	currentCpusHere := t.cpus.Intersection(currentCpus)
	freeCpusHere := t.cpus.Intersection(freeCpus)
	currentCPUCountHere := currentCpusHere.Size()
	currentCPUCountsHere := make([]int, len(currentCPUCounts)+1, len(currentCPUCounts)+1)
	copy(currentCPUCountsHere, currentCPUCounts)
	currentCPUCountsHere[depth] = currentCPUCountHere

	freeCPUCountHere := freeCpusHere.Size()
	freeCPUCountsHere := make([]int, len(freeCPUCounts)+1, len(freeCPUCounts)+1)
	copy(freeCPUCountsHere, freeCPUCounts)
	freeCPUCountsHere[depth] = freeCPUCountHere

	tna := CPUTreeNodeAttributes{
		t:                t,
		depth:            depth,
		currentCpus:      currentCpusHere,
		freeCpus:         freeCpusHere,
		currentCPUCount:  currentCPUCountHere,
		currentCPUCounts: currentCPUCountsHere,
		freeCPUCount:     freeCPUCountHere,
		freeCPUCounts:    freeCPUCountsHere,
	}

	if filter != nil && !filter(&tna) {
		return
	}

	*tnas = append(*tnas, tna)
	for _, child := range t.children {
		child.toAttributedSlice(currentCpus, freeCpus, filter,
			tnas, depth+1, currentCPUCountsHere, freeCPUCountsHere)
	}
}

// NewAllocator returns new CPU allocator for allocating CPUs from a
// CPU tree branch.
func (t *CPUTreeNode) NewAllocator(options cpuTreeAllocatorOptions) *CPUTreeAllocator {
	ta := &CPUTreeAllocator{
		root:    t,
		options: options,
	}
	return ta
}

// sorterAllocate implements an "is-less-than" callback that helps
// sorting a slice of CPUTreeNodeAttributes. The first item in the
// sorted list contains an optimal CPU tree node for allocating new
// CPUs.
func (ta *CPUTreeAllocator) sorterAllocate(tnas []CPUTreeNodeAttributes) func(int, int) bool {
	return func(i, j int) bool {
		if tnas[i].depth != tnas[j].depth {
			return tnas[i].depth > tnas[j].depth
		}
		for tdepth := 0; tdepth < len(tnas[i].currentCPUCounts); tdepth++ {
			// After this currentCpus will increase.
			// Maximize the maximal amount of currentCpus
			// as high level in the topology as possible.
			if tnas[i].currentCPUCounts[tdepth] != tnas[j].currentCPUCounts[tdepth] {
				return tnas[i].currentCPUCounts[tdepth] > tnas[j].currentCPUCounts[tdepth]
			}
		}
		for tdepth := 0; tdepth < len(tnas[i].freeCPUCounts); tdepth++ {
			// After this freeCpus will decrease.
			if tnas[i].freeCPUCounts[tdepth] != tnas[j].freeCPUCounts[tdepth] {
				if ta.options.topologyBalancing {
					// Goal: minimize maximal freeCpus in topology.
					return tnas[i].freeCPUCounts[tdepth] > tnas[j].freeCPUCounts[tdepth]
				}
				// Goal: maximize maximal freeCpus in topology.
				return tnas[i].freeCPUCounts[tdepth] < tnas[j].freeCPUCounts[tdepth]
			}
		}
		return tnas[i].t.name < tnas[j].t.name
	}
}

// sorterRelease implements an "is-less-than" callback that helps
// sorting a slice of cpuTreeNodeAttributes. The first item in the
// list contains an optimal CPU tree node for releasing new CPUs.
func (ta *CPUTreeAllocator) sorterRelease(tnas []CPUTreeNodeAttributes) func(int, int) bool {
	return func(i, j int) bool {
		if tnas[i].depth != tnas[j].depth {
			return tnas[i].depth > tnas[j].depth
		}
		for tdepth := 0; tdepth < len(tnas[i].currentCPUCounts); tdepth++ {
			// After this currentCpus will decrease. Aim
			// to minimize the minimal amount of
			// currentCpus in order to decrease
			// fragmentation as high level in the topology
			// as possible.
			if tnas[i].currentCPUCounts[tdepth] != tnas[j].currentCPUCounts[tdepth] {
				return tnas[i].currentCPUCounts[tdepth] < tnas[j].currentCPUCounts[tdepth]
			}
		}
		for tdepth := 0; tdepth < len(tnas[i].freeCPUCounts); tdepth++ {
			// After this freeCpus will increase. Try to
			// maximize minimal free CPUs for better
			// isolation as high level in the topology as
			// possible.
			if tnas[i].freeCPUCounts[tdepth] != tnas[j].freeCPUCounts[tdepth] {
				if ta.options.topologyBalancing {
					return tnas[i].freeCPUCounts[tdepth] < tnas[j].freeCPUCounts[tdepth]
				}
				return tnas[i].freeCPUCounts[tdepth] < tnas[j].freeCPUCounts[tdepth]
			}
		}
		return tnas[i].t.name > tnas[j].t.name
	}
}

// ResizeCpus implements topology awareness to both adding CPUs to and
// removing them from a set of CPUs. It returns CPUs from which actual
// allocation or releasing of CPUs can be done. ResizeCpus does not
// allocate or release CPUs.
//
// Parameters:
//   - currentCpus: a set of CPUs to/from which CPUs would be added/removed.
//   - freeCpus: a set of CPUs available CPUs.
//   - delta: number of CPUs to add (if positive) or remove (if negative).
//
// Return values:
//   - addFromCpus contains free CPUs from which delta CPUs can be
//     allocated. Note that the size of the set may be larger than
//     delta: there is room for other allocation logic to select from
//     these CPUs.
//   - removeFromCpus contains CPUs in currentCpus set from which
//     abs(delta) CPUs can be freed.
func (ta *CPUTreeAllocator) ResizeCpus(currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	if delta > 0 {
		return ta.resizeCpus(currentCpus, freeCpus, delta)
	}
	// In multi-CPU removal, remove CPUs one by one instead of
	// trying to find a single topology element from which all of
	// them could be removed.
	removeFrom := cpuset.NewCPUSet()
	addFrom := cpuset.NewCPUSet()
	for n := 0; n < -delta; n++ {
		_, removeSingleFrom, err := ta.resizeCpus(currentCpus, freeCpus, -1)
		if err != nil {
			return addFrom, removeFrom, err
		}
		// Make cheap internal error checks in order to capture
		// issues in alternative algorithms.
		if removeSingleFrom.Size() != 1 {
			return addFrom, removeFrom, fmt.Errorf("internal error: failed to find single cpu to free, "+
				"currentCpus=%s freeCpus=%s expectedSingle=%s",
				currentCpus, freeCpus, removeSingleFrom)
		}
		if removeFrom.Union(removeSingleFrom).Size() != n+1 {
			return addFrom, removeFrom, fmt.Errorf("internal error: double release of a cpu, "+
				"currentCpus=%s freeCpus=%s alreadyRemoved=%s removedNow=%s",
				currentCpus, freeCpus, removeFrom, removeSingleFrom)
		}
		removeFrom = removeFrom.Union(removeSingleFrom)
		currentCpus = currentCpus.Difference(removeSingleFrom)
		freeCpus = freeCpus.Union(removeSingleFrom)
	}
	return addFrom, removeFrom, nil
}

func (ta *CPUTreeAllocator) resizeCpus(currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	tnas := ta.root.ToAttributedSlice(currentCpus, freeCpus,
		func(tna *CPUTreeNodeAttributes) bool {
			// filter out branches with insufficient cpus
			if delta > 0 && tna.freeCPUCount-delta < 0 {
				// cannot allocate delta cpus
				return false
			}
			if delta < 0 && tna.currentCPUCount+delta < 0 {
				// cannot release delta cpus
				return false
			}
			return true
		})

	// Sort based on attributes
	if delta > 0 {
		sort.Slice(tnas, ta.sorterAllocate(tnas))
	} else {
		sort.Slice(tnas, ta.sorterRelease(tnas))
	}
	if len(tnas) == 0 {
		return freeCpus, currentCpus, fmt.Errorf("not enough free CPUs")
	}
	return tnas[0].freeCpus, tnas[0].currentCpus, nil
}
