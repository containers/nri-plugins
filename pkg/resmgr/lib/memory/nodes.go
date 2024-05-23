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
	"fmt"
	"maps"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// Node represents a memory node with some amount and type of attached memory.
type Node struct {
	id       ID
	memType  Type
	capacity int64
	normal   bool
	cpus     cpuset.CPUSet
	distance Distance
}

// Distance represents distance of a memory node from other nodes.
type Distance struct {
	vector []int
	sorted []int
	nodes  map[int]NodeMask
}

// NewNode creates a new node with the given parameters.
func NewNode(id ID, t Type, capa int64, normal bool, cpus cpuset.CPUSet, d []int) (*Node, error) {
	if !t.IsValid() {
		return nil, fmt.Errorf("%w: unknown type %d", ErrInvalidType, t)
	}

	if id > MaxNodeID {
		return nil, fmt.Errorf("%w: %d > %d", ErrInvalidNode, id, MaxNodeID)
	}

	dist, err := NewDistance(id, d)
	if err != nil {
		return nil, err
	}

	return &Node{
		id:       id,
		memType:  t,
		capacity: capa,
		normal:   normal,
		cpus:     cpus.Clone(),
		distance: dist,
	}, nil
}

// ID returns the node ID for the node.
func (n *Node) ID() ID {
	return n.id
}

// Mask returns the node mask bit for the node.
func (n *Node) Mask() NodeMask {
	return (1 << n.id)
}

// Type returns the type of memory attached to the node.
func (n *Node) Type() Type {
	return n.memType
}

// Capacity returns the amount of memory attached to the node.
func (n *Node) Capacity() int64 {
	return n.capacity
}

// IsNormal returns true if the memory attached to the node is not movable.
func (n *Node) IsNormal() bool {
	return n.normal
}

// IsMovable returns true if the memory attached to the node is movable.
func (n *Node) IsMovable() bool {
	return !n.IsNormal()
}

// HasMemory returns true if the node has some memory attached.
func (n *Node) HasMemory() bool {
	return n.capacity > 0
}

// CloseCPUs returns the set of CPUs closest to the node.
func (n *Node) CloseCPUs() cpuset.CPUSet {
	return n.cpus
}

// HasCPUs returns true if the node has a non-empty set of close CPUs.
func (n *Node) HasCPUs() bool {
	return n.cpus.Size() > 0
}

// Distance returns distance information for the node.
func (n *Node) Distance() *Distance {
	return n.distance.Clone()
}

// DistanceTo returns the distance of the node to the given node.
func (n *Node) DistanceTo(id ID) int {
	if id < len(n.distance.vector) {
		return n.distance.vector[id]
	}
	return math.MaxInt
}

// ForeachDistance calls the given function for each distance in the distance
// vector in increasing order, skipping the first distance to the node itself,
// until the function returns false, or ForeachDone. Iteration continues if
// the returned value is true, or ForeachMore. At each iteration the function
// is called with the current distance and the set of nodes at that distance.
func (n *Node) ForeachDistance(fn func(int, NodeMask) bool) {
	for _, d := range n.distance.sorted[1:] {
		if !fn(d, n.distance.nodes[d]) {
			return
		}
	}
}

// NewDistance creates a new Distance from the given distance vector.
func NewDistance(id ID, vector []int) (Distance, error) {
	if len(vector) > MaxNodeID {
		return Distance{}, fmt.Errorf("%w: too many distances (%d > %d)",
			ErrInvalidNode, len(vector), MaxNodeID)
	}

	var (
		sorted []int
		nodes  = make(map[int]NodeMask)
	)

	for id, d := range vector {
		if m, ok := nodes[d]; !ok {
			sorted = append(sorted, d)
			nodes[d] = (1 << id)
		} else {
			nodes[d] = m | (1 << id)
		}
	}

	slices.Sort(sorted)

	if nodes[sorted[0]] != NewNodeMask(id) {
		return Distance{}, fmt.Errorf("%w: shortest distance not to self #%d", ErrInvalidNode, id)
	}

	return Distance{
		vector: slices.Clone(vector),
		sorted: sorted,
		nodes:  nodes,
	}, nil
}

// Clone returns a copy of the distance info.
func (d *Distance) Clone() *Distance {
	return &Distance{
		vector: slices.Clone(d.vector),
		sorted: slices.Clone(d.sorted),
		nodes:  maps.Clone(d.nodes),
	}
}

// Vector returns the original distance vector for the distance info.
func (d *Distance) Vector() []int {
	return d.vector
}

// Sorted returns the original distance vector sorted.
func (d *Distance) Sorted() []int {
	return d.sorted
}

// NodesAt returns the set of nodes at the given distance.
func (d *Distance) NodesAt(dist int) NodeMask {
	return d.nodes[dist]
}

type (
	// NodeMask represents a set of node IDs as a bit mask.
	NodeMask uint64
)

const (
	// MaxNodeID is the maximum node ID that can be stored in a NodeMask.
	MaxNodeID = 63
)

// NewNodeMask returns a NodeMask with the given ids.
func NewNodeMask(ids ...ID) NodeMask {
	return NodeMask(0).Set(ids...)
}

// ParseNodeMaskparses the given string representation of a NodeMask.
func ParseNodeMask(str string) (NodeMask, error) {
	m := NodeMask(0)
	for _, s := range strings.Split(str, ",") {
		switch minmax := strings.SplitN(s, "-", 2); len(minmax) {
		case 2:
			beg, err := strconv.ParseInt(minmax[0], 10, 32)
			if err != nil {
				return 0, fmt.Errorf("%w: failed to parse node mask %q: %w",
					ErrInvalidNodeMask, str, err)
			}
			end, err := strconv.ParseInt(minmax[1], 10, 32)
			if err != nil {
				return 0, fmt.Errorf("%w: failed to parse node mask %q: %w",
					ErrInvalidNodeMask, str, err)
			}
			if end < beg {
				return 0, fmt.Errorf("%w: invalid range (%d - %d) in node mask %q",
					ErrInvalidNodeMask, beg, end, str)
			}
			for id := beg; id < end; id++ {
				if id > MaxNodeID {
					return 0, fmt.Errorf("%w: invalid node ID in mask %q (range %d-%d)",
						ErrInvalidNodeMask, str, beg, end)
				}
				m |= (1 << id)
			}
		case 1:
			id, err := strconv.ParseInt(minmax[1], 10, 32)
			if err != nil {
				return 0, fmt.Errorf("%w: failed to parse node mask %q: %w",
					ErrInvalidNodeMask, str, err)
			}
			if id > MaxNodeID {
				return 0, fmt.Errorf("%w: invalid node ID (%d) in mask %q",
					ErrInvalidNodeMask, id, str)
			}
			m |= (1 << id)
		default:
			return 0, fmt.Errorf("%w: failed to parse node mask %q", ErrInvalidNodeMask, str)
		}
	}
	return m, nil
}

// MustParseNodeMask parses the given string representation of a NodeMask.
// It panicks on failure.
func MustParseNodeMask(str string) NodeMask {
	m, err := ParseNodeMask(str)
	if err == nil {
		return m
	}

	panic(err)
}

// Slice returns the node IDs stored in the NodeMask as a slice in increasing order.
func (m NodeMask) Slice() []ID {
	var ids []ID
	m.Foreach(func(id ID) bool {
		ids = append(ids, id)
		return true
	})
	return ids
}

// Set returns a NodeMask with both the original and the given IDs added.
func (m NodeMask) Set(ids ...ID) NodeMask {
	for _, id := range ids {
		m |= (1 << id)
	}
	return m
}

// Clear returns a NodeMask with the given IDs removed.
func (m NodeMask) Clear(ids ...ID) NodeMask {
	for _, id := range ids {
		m &^= (1 << id)
	}
	return m
}

// Contains returns true if all the given IDs are present in the NodeMask.
func (m NodeMask) Contains(ids ...ID) bool {
	for _, id := range ids {
		if (m & (1 << id)) == 0 {
			return false
		}
	}
	return true
}

// ContainsAny returns true if any of the given IDs are present in the NodeMask.
func (m NodeMask) ContainsAny(ids ...ID) bool {
	for _, id := range ids {
		if (m & (1 << id)) != 0 {
			return true
		}
	}
	return false
}

// And returns a NodeMask with all IDs which are present in both NodeMasks.
func (m NodeMask) And(o NodeMask) NodeMask {
	return m & o
}

// Or returns a NodeMask with all IDs which are present at least in one of the NodeMasks.
func (m NodeMask) Or(o NodeMask) NodeMask {
	return m | o
}

// AndNot returns a NodeMask with all IDs which are present in m but not in o.
func (m NodeMask) AndNot(o NodeMask) NodeMask {
	return m &^ o
}

// Size returns the number of IDs present in the NodeMask.
func (m NodeMask) Size() int {
	return bits.OnesCount64(uint64(m))
}

// String returns a string representation of the NodeMask.
func (m NodeMask) String() string {
	b := strings.Builder{}
	b.WriteString("nodes{")
	b.WriteString(m.MemsetString())
	b.WriteString("}")

	return b.String()
}

// MemsetString returns a linux memory set-compatible string representation
// of the NodeMask. This string is suitable to be used with the cpuset cgroup
// controller for pinning processes to memory nodes.
func (m NodeMask) MemsetString() string {
	var (
		b         = strings.Builder{}
		sep       = ""
		beg       = -1
		end       = -1
		dumpRange = func() {
			switch {
			case beg < 0:
			case beg == end:
				b.WriteString(sep)
				b.WriteString(strconv.Itoa(beg))
				sep = ","
			case beg <= end-1:
				b.WriteString(sep)
				b.WriteString(strconv.Itoa(beg))
				b.WriteString("-")
				b.WriteString(strconv.Itoa(end))
				sep = ","
			}
		}
	)

	m.Foreach(func(id ID) bool {
		switch {
		case beg < 0:
			beg, end = id, id
		case beg >= 0 && id == end+1:
			end = id
		default:
			dumpRange()
			beg, end = id, id
		}
		return true
	})

	dumpRange()

	return b.String()
}

// Foreach calls the given function for each ID set in the NodeMask until
// the function returns false, or ForeachDone. Iteration continues if the
// returned value is true, or ForeachMore.
func (m NodeMask) Foreach(fn func(ID) bool) {
	for b := 0; m != 0; b, m = b+8, m>>8 {
		if m&0xff != 0 {
			if m&0xf != 0 {
				if m&0x1 != 0 {
					if !fn(b + 0) {
						return
					}
				}
				if m&0x2 != 0 {
					if !fn(b + 1) {
						return
					}
				}
				if m&0x4 != 0 {
					if !fn(b + 2) {
						return
					}
				}
				if m&0x8 != 0 {
					if !fn(b + 3) {
						return
					}
				}
			}
			if m&0xf0 != 0 {
				if m&0x10 != 0 {
					if !fn(b + 4) {
						return
					}
				}
				if m&0x20 != 0 {
					if !fn(b + 5) {
						return
					}
				}
				if m&0x40 != 0 {
					if !fn(b + 6) {
						return
					}
				}
				if m&0x80 != 0 {
					if !fn(b + 7) {
						return
					}
				}
			}
		}
	}
}
