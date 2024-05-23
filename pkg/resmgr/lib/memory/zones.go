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

import "slices"

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

// ZoneNumUsers returns the number of requests assigned to the zone.
func (a *Allocator) ZoneNumUsers(zone NodeMask) int {
	if z, ok := a.zones[zone]; ok {
		return len(z.users)
	}
	return 0
}

// ZoneFilter is a function to filter zones.
type ZoneFilter func(NodeMask) bool

// ZoneSorter is a function to compare zones for sorting.
type ZoneSorter func(z1, z2 NodeMask) int

// SortZones filters zones by a filter function into a slice, then
// sorts the slice by chaining the given sorting functions. A nil
// filter function picks all zones.
func (a *Allocator) SortZones(f ZoneFilter, s ...ZoneSorter) []NodeMask {
	slice := make([]NodeMask, 0, len(a.zones))
	for z := range a.zones {
		if f == nil || f(z) {
			slice = append(slice, z)
		}
	}
	if len(s) > 0 {
		slices.SortFunc(slice, func(z1, z2 NodeMask) int {
			for _, fn := range s {
				if diff := fn(z1, z2); diff != 0 {
					return diff
				}
			}
			return 0
		})
	}
	return slice
}

// ZonesByUsersSubzonesFirst compares zone by number of users.
func (a *Allocator) ZonesByUsersSubzonesFirst(zone1, zone2 NodeMask) int {
	z1, z2 := a.zones[zone1], a.zones[zone2]
	if z1 != nil && z2 != nil {
		if diff := len(z2.users) - len(z1.users); diff != 0 {
			return diff
		}
	}

	if (zone1 & zone2) == zone1 {
		return -1
	}
	if (zone1 & zone2) == zone2 {
		return 1
	}

	if diff := zone2.Size() - zone1.Size(); diff != 0 {
		return diff
	}

	return int(zone2 - zone1)
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
	req.zone = zone
	a.journal.assign(zone, req.ID())

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
	a.journal.delete(zone, id)
	req.zone = 0

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

func (a *Allocator) zoneShrinkUsage(zone NodeMask, amount int64, limit Priority, extra TypeMask) int64 {
	z, ok := a.zones[zone]
	if !ok || len(z.users) == 0 {
		return 0
	}

	//
	// This is used by our default overcommit handler to shrink usage of a zone.
	//
	//   - find a new zone by expanding this one, optionally with extra types
	//   - pick requests up to an priority limit, sort them by decreasing size
	//   - move requests to new zone, stop if we've freed up enough capacity
	//
	// TODO(klihub): We compare our internally set creation time stamps and
	// return the younger of the two requests as a last resort to provide a
	// stable sorting order. We probably should allow the caller to set the
	// creation timestamp to that of the container. However currently there
	// is no way of figuring that out using NRI...
	//
	// TODO(klihub): add container creation and startup timestamps to NRI.
	//

	nodes, types := a.expand(zone, z.types|extra)

	if nodes == 0 {
		log.Debug("  - %s: couldn't expand by %s types", zoneName(zone), extra)
		return 0
	}

	moved := int64(0)
	for _, req := range SortRequests(z.users,
		RequestsWithMaxPriority(limit),
		RequestsByPriority,
		RequestsBySize,
		RequestsByAge,
	) {
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

func zoneName(zone NodeMask) string {
	if zone != 0 {
		return "zone<" + zone.MemsetString() + ">"
	} else {
		return "zone<without nodes>"
	}
}
