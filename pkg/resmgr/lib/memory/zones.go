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
	"slices"
)

// Zone is a collection of Nodes which is collectively used to fulfill one
// or more allocation requests.
type Zone struct {
	nodes    NodeMask            // IDs of nodes that make up this zone
	types    TypeMask            // types of memory in this zone
	capacity int64               // total memory capacity in this zone
	users    map[string]*Request // requests allocated from this zone
}

// ZoneType returns the types of memory available in this zone.
func (a *Allocator) ZoneType(zone NodeMask) TypeMask {
	return a.zoneType(zone & a.masks.nodes.all)
}

// ZoneCapacity returns the amount of total memory in the zone.
func (a *Allocator) ZoneCapacity(zone NodeMask) int64 {
	return a.zoneCapacity(zone & a.masks.nodes.hasMemory)
}

// ZoneUsage returns the amount of allocated memory in the zone.
func (a *Allocator) ZoneUsage(zone NodeMask) int64 {
	return a.zoneUsage(zone & a.masks.nodes.hasMemory)
}

// ZoneFree returns the amount of free memory in the zone.
func (a *Allocator) ZoneFree(zone NodeMask) int64 {
	return a.zoneFree(zone & a.masks.nodes.hasMemory)
}

func (a *Allocator) zoneType(zone NodeMask) TypeMask {
	var types TypeMask

	if z, ok := a.zones[zone]; ok {
		types = z.types
	} else {
		for _, id := range (zone & a.masks.nodes.all).Slice() {
			types |= a.nodes[id].Type().Mask()
		}
	}

	return types
}

func (a *Allocator) zoneCapacity(zone NodeMask) int64 {
	var capacity int64

	if z, ok := a.zones[zone]; ok {
		capacity = z.capacity
	} else {
		for _, id := range (zone & a.masks.nodes.hasMemory).Slice() {
			capacity += a.nodes[id].capacity
		}
	}

	return capacity
}

func (a *Allocator) zoneUsage(zone NodeMask) int64 {
	var usage int64

	// An allocation is considered to belong to a zone if its nodes
	// fully fit into the zone.

	for nodes, z := range a.zones {
		if (zone & nodes) == nodes {
			for _, req := range z.users {
				usage += req.Size()
			}
		}
	}

	return usage
}

func (a *Allocator) zoneFree(zone NodeMask) int64 {
	return a.zoneCapacity(zone) - a.zoneUsage(zone)
}

func (a *Allocator) zoneAssign(zone NodeMask, req *Request) {
	z, ok := a.zones[zone]
	if !ok {
		z = &Zone{
			nodes:    zone,
			types:    a.zoneType(zone),
			capacity: a.zoneCapacity(zone),
			users:    map[string]*Request{},
		}
		a.zones[zone] = z
	}

	z.users[req.ID()] = req
	a.users[req.ID()] = zone
	a.record.assign(zone, req.ID())

	log.Debug("  + %s: assign %s", zoneName(zone), req)
}

func (a *Allocator) zoneRemove(zone NodeMask, id string) {
	z, ok := a.zones[zone]
	if !ok {
		return
	}

	req, ok := z.users[id]
	if !ok {
		return
	}

	delete(z.users, req.ID())
	delete(a.users, req.ID())
	a.record.delete(zone, id)

	log.Debug("  - %s: remove %s", zoneName(zone), req)
}

func (a *Allocator) zoneMove(zone NodeMask, req *Request) {
	if from, ok := a.users[req.ID()]; ok {
		if from == zone {
			log.Warn("  - %s: useless move of %s (same zone)...", zoneName(zone), req)
			return
		}
		a.zoneRemove(from, req.ID())
	}
	a.zoneAssign(zone, req)
}

func (a *Allocator) zoneShrinkUsage(zone NodeMask, amount int64, limit Inertia, extra TypeMask) int64 {
	z, ok := a.zones[zone]
	if !ok {
		return 0
	}

	//
	// This is used by our default overflow handler to shrinks usage of a zone.
	//
	//   - find a new zone by expanding this one, optionally with extra types
	//   - pick requests up to an inertia limit, sort them by decreasing size
	//   - move requests to new zone, stop if we've freed up enough capacity
	//

	nodes, types := a.expand(zone, z.types|extra)

	if nodes == 0 {
		log.Debug("  - %s: couldn't move any requests", zoneName(zone))
		return 0
	}

	moved := int64(0)
	for _, req := range z.pickAndSortRequests(limit) {
		if !req.IsStrict() || req.Types() == z.types|types {
			a.zoneMove(zone|nodes, req)
			moved += req.Size()
			if moved >= amount {
				break
			}
		}
	}

	log.Debug("  - %s: freed up %s bytes of memory", zoneName(zone), prettySize(moved))

	return moved
}

func (z *Zone) pickAndSortRequests(limit Inertia) []*Request {
	requests := make([]*Request, 0, len(z.users))
	for _, req := range z.users {
		if req.Inertia() <= limit {
			requests = append(requests, req)
		}
	}
	slices.SortFunc(requests, SortRequestsByInertiaAndSize)

	return requests
}

func zoneName(zone NodeMask) string {
	if zone != 0 {
		return "zone<" + zone.MemsetString() + ">"
	} else {
		return "zone<without nodes>"
	}
}
