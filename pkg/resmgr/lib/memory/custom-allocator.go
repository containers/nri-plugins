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

import "fmt"

const (
	AnyZone NodeMask = 0
)

// CustomFunctions can be used to provide custom implementations which
// override default algorithms in an Allocator. Use the WithCustomFunctions
// option to pass custom functions to an Allocator.
type CustomFunctions struct {
	// ExpandZone returns the extra nodes to expand the given zone for the
	// given memory types. Zone expansion happens either when resolving
	// overcommit of the given zone to figure out where to move allocations,
	// or to satisfy the requested memory types for an allocation request,
	// if the preferred affinity is not enough to do so. DefaultExpandZone
	// in CustomAllocator provides a default implementation.
	ExpandZone func(zone NodeMask, types TypeMask, a CustomAllocator) NodeMask
	// HandleOvercommit takes care of reducing memory usage in overcommitted
	// zones. Such zones have less capacity than the total consumption by all
	// active allocations. The given CustomAllocator provides functions for
	// overcommit handling, for instance MoveRequest() for moving allocations
	// to other zones and CheckOvercommit() for re-checking overcommit after
	// moves. DefaultHandleOvercommit in CustomAllocator provides a default
	// implementation.
	HandleOvercommit func(overcommit map[NodeMask]int64, a CustomAllocator) error
}

// CustomAllocator is the interface custom functions use to interact with
// the Allocator they customize.
type CustomAllocator interface {
	// CheckOvercommit checks any zone overcommit, returning the amount of spill for each.
	CheckOvercommit() map[NodeMask]int64
	// GetRequests returns all allocations.
	GetRequests() []*Request
	// GetRequestsForZone returns all allocations for the given zone.
	GetRequestsForZone(NodeMask) []*Request
	// MoveRequest moves the given allocation to the given zone.
	MoveRequest(id string, zone NodeMask) error
	// GetZones returns all zones which have active allocations.
	GetZones() []NodeMask
	// GetNodes returns all nodes known to the allocator.
	GetNodes() []*Node

	// DefaultExpandZone is the default implementation for zone expansion.
	DefaultExpandZone(NodeMask, TypeMask) NodeMask
	// DefaultHandleOvercommit is the default implementation for overcommit handling.
	DefaultHandleOvercommit(overcommit map[NodeMask]int64) error

	// GetAllocator returns the underlying customized allocator.
	GetAllocator() *Allocator
}

type customAllocator struct {
	a *Allocator
}

// WithCustomFunctions returns an option for overriding default algorithms in
// an Allocator.
func WithCustomFunctions(c *CustomFunctions) AllocatorOption {
	return func(a *Allocator) error {
		if c != nil {
			a.custom = *c
		}
		return nil
	}
}

func (c *customAllocator) CheckOvercommit() map[NodeMask]int64 {
	_, overcommits := c.a.checkOvercommit(0)
	return overcommits
}

func (c *customAllocator) GetRequests() []*Request {
	return SortRequests(c.a.requests, nil, RequestsByAge)
}

func (c *customAllocator) GetRequestsForZone(zone NodeMask) []*Request {
	z, ok := c.a.zones[zone]
	if !ok {
		return nil
	}

	return SortRequests(z.users, nil,
		RequestsByPriority,
		RequestsBySize,
		RequestsByAge,
	)
}

func (c *customAllocator) MoveRequest(id string, zone NodeMask) error {
	req, ok := c.a.requests[id]
	if !ok {
		return fmt.Errorf("%w: no request with ID %s", ErrUnknownRequest, id)
	}

	c.a.zoneMove(zone, req)
	return nil
}

func (c *customAllocator) GetZones() []NodeMask {
	var zones []NodeMask
	for nodes, z := range c.a.zones {
		if len(z.users) > 0 {
			zones = append(zones, nodes)
		}
	}
	return zones
}

func (c *customAllocator) GetNodes() []*Node {
	var nodes []*Node
	for _, n := range c.a.nodes {
		nodes = append(nodes, n)
	}
	return nodes
}

func (c *customAllocator) DefaultExpandZone(zone NodeMask, types TypeMask) NodeMask {
	extraNodes, _ := c.a.defaultExpand(zone, types)
	return extraNodes
}

func (c *customAllocator) DefaultHandleOvercommit(overcommit map[NodeMask]int64) error {
	var nodes NodeMask

	for n := range overcommit {
		nodes |= n
	}

	zones, spill := c.a.checkOvercommit(nodes)
	return c.a.defaultHandleOvercommit(nodes, zones, spill)
}

func (c *customAllocator) GetAllocator() *Allocator {
	return c.a
}
