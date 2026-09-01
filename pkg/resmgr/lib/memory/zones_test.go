// Copyright 2026 Intel Corporation. All Rights Reserved.
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

package libmem_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
)

// newZoneTestAllocator creates a simple 2-DRAM-node allocator for zone tests.
func newZoneTestAllocator(t *testing.T) *Allocator {
	t.Helper()
	setup := &testSetup{
		description: "2 DRAM nodes for zone tests",
		types:       []Type{TypeDRAM, TypeDRAM},
		capacities:  []int64{8, 8},
		movability:  []bool{normal, normal},
		closeCPUs:   [][]int{{0, 1}, {2, 3}},
		distances: [][]int{
			{10, 21},
			{21, 10},
		},
	}
	a, err := NewAllocator(WithNodes(setup.nodes(t)))
	require.NoError(t, err)
	require.NotNil(t, a)
	return a
}

// newFourNodeDRAMAllocator creates a 4-DRAM-node allocator (4 bytes/node) for zone tests.
// Node distances: 0<->2 and 1<->3 are close (11), cross pairs are far (21).
func newFourNodeDRAMAllocator(t *testing.T) *Allocator {
	t.Helper()
	setup := &testSetup{
		description: "4 DRAM nodes for zone tests",
		types:       []Type{TypeDRAM, TypeDRAM, TypeDRAM, TypeDRAM},
		capacities:  []int64{4, 4, 4, 4},
		movability:  []bool{normal, normal, normal, normal},
		closeCPUs:   [][]int{{0, 1}, {2, 3}, {4, 5}, {6, 7}},
		distances: [][]int{
			{10, 21, 11, 21},
			{21, 10, 21, 11},
			{11, 21, 10, 21},
			{21, 11, 21, 10},
		},
	}
	a, err := NewAllocator(WithNodes(setup.nodes(t)))
	require.NoError(t, err)
	require.NotNil(t, a)
	return a
}

// TestZoneCapacity verifies that ZoneCapacity returns the total memory of the
// nodes in the requested zone.
func TestZoneCapacity(t *testing.T) {
	a := newZoneTestAllocator(t)

	require.Equal(t, int64(8), a.ZoneCapacity(NewNodeMask(0)))
	require.Equal(t, int64(8), a.ZoneCapacity(NewNodeMask(1)))
	require.Equal(t, int64(16), a.ZoneCapacity(NewNodeMask(0, 1)))
}

// TestZoneUsageAndFree allocates memory into a zone and checks that ZoneUsage
// and ZoneFree reflect the allocation correctly.
func TestZoneUsageAndFree(t *testing.T) {
	a := newZoneTestAllocator(t)

	zone := NewNodeMask(0, 1)
	require.Equal(t, int64(0), a.ZoneUsage(zone), "usage before allocation")
	require.Equal(t, int64(16), a.ZoneFree(zone), "free before allocation")

	_, _, err := a.Allocate(Container("c1", "test", "burstable", 6, NewNodeMask(0)))
	require.NoError(t, err)

	require.Equal(t, int64(6), a.ZoneUsage(zone), "usage after 6-byte allocation")
	require.Equal(t, int64(10), a.ZoneFree(zone), "free after 6-byte allocation")
}

// TestZoneNumUsers verifies that ZoneNumUsers counts requests assigned to a zone.
func TestZoneNumUsers(t *testing.T) {
	a := newZoneTestAllocator(t)

	zone := NewNodeMask(0)
	require.Equal(t, 0, a.ZoneNumUsers(zone), "no users before allocation")

	_, _, err := a.Allocate(Container("c1", "test", "burstable", 2, NewNodeMask(0)))
	require.NoError(t, err)
	require.Equal(t, 1, a.ZoneNumUsers(zone), "one user after first allocation")

	_, _, err = a.Allocate(Container("c2", "test", "burstable", 2, NewNodeMask(0)))
	require.NoError(t, err)
	require.Equal(t, 2, a.ZoneNumUsers(zone), "two users after second allocation")
}

// TestZonesSort verifies SortZones with nil/non-nil filters and with a sorter.
func TestZonesSort(t *testing.T) {
	a := newFourNodeDRAMAllocator(t)

	zone0 := NewNodeMask(0)
	zone1 := NewNodeMask(1)

	// Populate two distinct zones: zone0 gets two requests, zone1 gets one.
	_, _, err := a.Allocate(Container("c1", "test", "burstable", 2, zone0))
	require.NoError(t, err)
	_, _, err = a.Allocate(Container("c2", "test", "burstable", 2, zone0))
	require.NoError(t, err)
	_, _, err = a.Allocate(Container("c3", "test", "burstable", 2, zone1))
	require.NoError(t, err)

	// Positive: nil filter returns all created zones.
	all := a.SortZones(nil)
	require.Equal(t, 2, len(all), "nil filter should return all zones")
	require.ElementsMatch(t, []NodeMask{zone0, zone1}, all, "nil filter should include zone0 and zone1")

	// Positive: filter restricts to a single zone.
	only0 := a.SortZones(func(z NodeMask) bool { return z == zone0 })
	require.Equal(t, []NodeMask{zone0}, only0, "filter should return only zone0")

	// Positive: sorter orders zone with more users first.
	sorted := a.SortZones(nil, a.ZonesByUsersSubzonesFirst)
	require.Equal(t, zone0, sorted[0], "zone0 (2 users) should sort before zone1 (1 user)")
	require.Equal(t, zone1, sorted[1], "zone1 (1 user) should sort after zone0 (2 users)")

	// Negative: filter that excludes everything returns an empty slice.
	none := a.SortZones(func(NodeMask) bool { return false })
	require.Empty(t, none, "filter rejecting all zones should return empty slice")
}

// TestZonesByUsersSubzonesFirst verifies the comparator used by SortZones.
func TestZonesByUsersSubzonesFirst(t *testing.T) {
	a := newFourNodeDRAMAllocator(t)

	zone0 := NewNodeMask(0)
	zone1 := NewNodeMask(1)

	// Populate zones: zone0 gets two requests, zone1 gets one.
	_, _, err := a.Allocate(Container("c1", "test", "burstable", 2, zone0))
	require.NoError(t, err)
	_, _, err = a.Allocate(Container("c2", "test", "burstable", 2, zone0))
	require.NoError(t, err)
	_, _, err = a.Allocate(Container("c3", "test", "burstable", 2, zone1))
	require.NoError(t, err)

	// Positive: zone with more users sorts before zone with fewer users.
	// diff = len(z2.users) - len(z1.users) = 1 - 2 = -1 -> zone0 < zone1
	require.Negative(t, a.ZonesByUsersSubzonesFirst(zone0, zone1),
		"zone0 (2 users) should sort before zone1 (1 user)")

	// Positive: symmetric -- reversed argument order gives a positive result.
	require.Positive(t, a.ZonesByUsersSubzonesFirst(zone1, zone0),
		"zone1 (1 user) should sort after zone0 (2 users)")

	// Positive: a subzone sorts before its superset when user counts are tied.
	// zone0 is a subset of zone{0,1}: (zone0 & zone{0,1}) == zone0 -> returns -1
	zone01 := NewNodeMask(0, 1)
	require.Negative(t, a.ZonesByUsersSubzonesFirst(zone0, zone01),
		"subzone zone0 should sort before superset zone{0,1}")

	// Positive: symmetric -- superset sorts after subzone -> positive result.
	require.Positive(t, a.ZonesByUsersSubzonesFirst(zone01, zone0),
		"superset zone{0,1} should sort after subzone zone0")

	// Negative: zones not present in the allocator (nil entries) fall back to
	// subset/size logic. Two disjoint phantom zones with the same size sort by
	// their numeric NodeMask value: higher value first (zone2 - zone1 > 0).
	phantom2 := NewNodeMask(2)
	phantom3 := NewNodeMask(3)
	require.Positive(t, a.ZonesByUsersSubzonesFirst(phantom2, phantom3),
		"for disjoint equal-size zones, higher NodeMask value should sort first")
}
