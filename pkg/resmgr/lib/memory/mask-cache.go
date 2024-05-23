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

import "maps"

// MaskCache caches oft-used memory type and node masks for an Allocator.
type MaskCache struct {
	types TypeMask
	nodes struct {
		all       NodeMask
		normal    NodeMask
		movable   NodeMask
		hasMemory NodeMask
		noMemory  NodeMask
		hasCPU    NodeMask
		noCPU     NodeMask
		byTypes   map[TypeMask]NodeMask
	}
}

// NewMaskCache returns a new cache with the given nodes added.
func NewMaskCache(nodes ...*Node) *MaskCache {
	c := &MaskCache{}
	c.nodes.byTypes = make(map[TypeMask]NodeMask)
	for _, n := range nodes {
		c.addNode(n)
	}
	return c
}

// addNode adds the given node the cache.
func (c *MaskCache) addNode(n *Node) {
	typeMask := n.memType.Mask()
	nodeMask := n.Mask()

	c.nodes.all |= nodeMask
	if n.HasMemory() {
		c.types |= typeMask
		c.nodes.hasMemory |= nodeMask
		if n.IsNormal() {
			c.nodes.normal |= nodeMask
		} else {
			c.nodes.movable |= nodeMask
		}
		c.nodes.byTypes[typeMask] |= nodeMask
		for _, types := range []TypeMask{
			TypeMaskDRAM | TypeMaskPMEM,
			TypeMaskDRAM | TypeMaskHBM,
			TypeMaskPMEM | TypeMaskHBM,
			TypeMaskAll,
		} {
			for _, t := range types.Slice() {
				c.nodes.byTypes[types] |= c.nodes.byTypes[t.Mask()]
			}
		}
	} else {
		c.nodes.noMemory |= nodeMask
	}
	if n.HasCPUs() {
		c.nodes.hasCPU |= nodeMask
	} else {
		c.nodes.noCPU |= nodeMask
	}
}

// AvailableTypes returns all available types from the cache. A type
// considered available if at least one node with non-zero amount of
// attached memory of the type was added to the cache.
func (c *MaskCache) AvailableTypes() TypeMask {
	return c.types
}

// AvailableNodes returns all nodes added to the cache.
func (c *MaskCache) AvailableNodes() NodeMask {
	return c.nodes.all
}

// NodesWithNormalMem returns all nodes with normal, non-movable
// memory attached from the cache.
func (c *MaskCache) NodesWithNormalMem() NodeMask {
	return c.nodes.normal
}

// NodesWithMovableMem returns all nodes with movable memory attached
// from the cache.
func (c *MaskCache) NodesWithMovableMem() NodeMask {
	return c.nodes.movable
}

// NodesWithMem returns all nodes with any memory attached from the
// cache.
func (c *MaskCache) NodesWithMem() NodeMask {
	return c.nodes.hasMemory
}

// NodesWithMem returns all nodes with no memory attached from the
// cache.
func (c *MaskCache) NodesWithoutMem() NodeMask {
	return c.nodes.noMemory
}

// NodesByTypes the nodes with given type of memory attached from
// the cache. Note that if any of the given types is unavailable
// NodesByTypes returns an empty NodeMask even if other types of
// memory in the TypeMask is available.
func (c *MaskCache) NodesByTypes(types TypeMask) NodeMask {
	return c.nodes.byTypes[types]
}

// NodesWithCloseCPUs returns all nodes which have a non-empty set
// of close CPUs from the cache.
func (c *MaskCache) NodesWithCloseCPUs() NodeMask {
	return c.nodes.hasCPU
}

// NodesWithoutCloseCPUs returns all nodes which have no close CPUs
// from the cache.
func (c *MaskCache) NodesWithoutCloseCPUs() NodeMask {
	return c.nodes.noCPU
}

// Clone returns a copy of the cache.
func (c *MaskCache) Clone() *MaskCache {
	n := &MaskCache{
		types: c.types,
		nodes: c.nodes,
	}
	n.nodes.byTypes = maps.Clone(c.nodes.byTypes)
	return n
}

// Log debug-dumps the contents of the cache.
func (c *MaskCache) Dump(prefix, header string) {
	log.Info("%s%s", header, prefix)
	log.Info("%s  - available types: %s", prefix, c.types)
	log.Info("%s  - available nodes: %s", prefix, c.nodes.all)
	log.Info("%s      has memory: %s", prefix, c.nodes.hasMemory)
	for types, nodes := range c.nodes.byTypes {
		log.Info("%s        %s: %s", prefix, types, nodes)
	}
	log.Info("%s      no memory: %s", prefix, c.nodes.noMemory)
	log.Info("%s     has close CPUs: %s", prefix, c.nodes.hasCPU)
	log.Info("%s      no close CPUs: %s", prefix, c.nodes.noCPU)
}
