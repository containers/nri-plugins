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
	// ExpandZone should return extra nodes to expand the given zone with
	// the given memory types. Zone expansion happens either when resolving
	// overflow of the given zone to figure out where to move allocations,
	// or to satisfy the requested memory types for an allocation request
	// if the preferred affinity is not enough to do so. DefaultExpandZone
	// in CustomAllocator provides a default implementation.
	ExpandZone func(zone NodeMask, types TypeMask, a CustomAllocator) NodeMask
	// HandleOverflow should take care of oversubscribed zones. These zones
	// have less capacity than overall consumption by active allocations.
	// The given CustomAllocator provides functions for this, among others
	// MoveRequest() for moving allocations to other zones and CheckOverflow()
	// for checking oversubscription after moves. A default implementation is
	// provided by DefaultHandleOverflow in CustomAllocator.
	HandleOverflow func(overflow map[NodeMask]int64, a CustomAllocator) error
}

// CustomAllocator is the interface custom functions use to interact with
// the Allocator they customize.
type CustomAllocator interface {
	// CheckOverflow checks any zone overflows, returning the amount of spill for each.
	CheckOverflow() map[NodeMask]int64
	// GetRequests returns the allocations for a zone, or all allocations if zone is 0.
	GetRequests(zone NodeMask) []*Request
	// MoveRequest moves the given allocation to the given zone.
	MoveRequest(id string, zone NodeMask) error
	// GetZones returns all zones which have active allocations.
	GetZones() []NodeMask
	// GetNodes returns all nodes known to the allocator.
	GetNodes() []*Node

	// DefaultExpandZone is the default implementation for CustomFunctions.ExpandZone().
	DefaultExpandZone(NodeMask, TypeMask) NodeMask
	// DefaultHandleOverflow is the default implementation CustomFunctions.HandleOverflow().
	DefaultHandleOverflow(overflow map[NodeMask]int64) error

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

func (c *customAllocator) CheckOverflow() map[NodeMask]int64 {
	_, overflows := c.a.checkOverflow(0)
	return overflows
}

func (c *customAllocator) GetRequests(zone NodeMask) []*Request {
	var requests []*Request
	for _, req := range c.a.requests {
		if zone == AnyZone || req.zone == zone {
			requests = append(requests, req)
		}
	}
	return requests
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

func (c *customAllocator) DefaultHandleOverflow(overflow map[NodeMask]int64) error {
	var nodes NodeMask

	for n := range overflow {
		nodes |= n
	}

	zones, spill := c.a.checkOverflow(nodes)
	return c.a.defaultHandleOverflow(nodes, zones, spill)
}

func (c *customAllocator) GetAllocator() *Allocator {
	return c.a
}
