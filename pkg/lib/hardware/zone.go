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

package hardware

import (
	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// Level names a kind of hardware which CPUs can share.
//
// A machine has zones at only some of these levels, and which ones is a property
// of the hardware. Ask [Machine.Levels] rather than assuming.
//
// The constants are a tag, not an order. Which level contains which is a
// property of the machine, not of this list: on one machine a cluster is a few
// cores within a NUMA node, on another a single cluster covers a whole die. Ask
// with [ZonesWithin] or [ZoneOf] when it matters.
type Level int

const (
	// LevelPackage is a physical package, i.e. a socket.
	LevelPackage Level = iota
	// LevelDie is a die within a package.
	LevelDie
	// LevelCluster is a group of CPUs the kernel reports as a cluster, from
	// topology/cluster_id and topology/cluster_cpus_list. What a cluster shares
	// is up to the hardware, and on some machines the grouping is real and
	// worth allocating along. On others it says nothing: every cluster is one
	// core, or a single cluster covers everything. There is no cluster level in
	// those cases. See also [LogicalClusters], for machines which report one
	// cluster per core only because of how their threads are counted.
	LevelCluster
	// LevelNUMANode is a NUMA node: CPUs with the same locality to a piece of
	// memory. See [MemoryNode] for the memory itself.
	LevelNUMANode
	// LevelL3Cache is a group of CPUs sharing a level 3 cache.
	LevelL3Cache
	// LevelL2Cache is a group of CPUs sharing a level 2 cache.
	LevelL2Cache
	// LevelCore is a physical core, i.e. a group of hardware threads.
	LevelCore
	// LevelThread is a single CPU as the kernel counts them.
	LevelThread

	// numLevels is how many levels there are, for sizing an array by Level.
	numLevels = int(LevelThread) + 1
)

// String returns the name of the level.
func (l Level) String() string {
	if l < 0 || int(l) >= numLevels {
		return "unknown level"
	}
	return levelNames[l]
}

// levelNames name the levels, indexed by [Level]. They double as the prefix
// [Zone.Name] is built from.
var levelNames = [...]string{
	LevelPackage:  "package",
	LevelDie:      "die",
	LevelCluster:  "cluster",
	LevelNUMANode: "node",
	LevelL3Cache:  "L3",
	LevelL2Cache:  "L2",
	LevelCore:     "core",
	LevelThread:   "thread",
}

// allLevels are the levels which may have zones, in the order
// [Machine.Levels] reports them. It is a reporting order and nothing more.
var allLevels = []Level{
	LevelPackage, LevelDie, LevelCluster, LevelNUMANode,
	LevelL3Cache, LevelL2Cache, LevelCore, LevelThread,
}

// Zone is a set of CPUs which share a piece of hardware at one [Level]: a
// package, a die, a cluster, a NUMA node, a cache, a core, a thread.
//
// Zones do not form a tree. The hardware does not describe one: whether a
// cluster falls inside a NUMA node or spans several is a property of the
// machine, and which levels are worth nesting is a decision each caller makes
// differently. [Machine.Zones] lists the zones of a level, [ZonesWithin] and
// [ZonesOverlapping] answer containment questions, and a caller which wants a
// hierarchy builds the one it wants out of those.
//
// A Zone is a handle into the [Machine] which produced it: never nil, and safe
// to use even when it refers to nothing, in which case Valid() is false.
type Zone struct {
	m     *Machine
	valid bool

	level Level
	id    ID
	name  string
	cpus  *libcpu.CpuMask

	// where it sits, as the kernel numbers things. unknownID for a coordinate
	// which does not apply or which the machine does not report.
	pkg     ID
	die     ID
	cluster ID
	node    ID

	cache *Cache // set for the cache levels
}

// invalidZone is what a lookup for a zone the machine does not have returns. Its
// methods answer with zero values.
var invalidZone = &Zone{
	id: unknownID, pkg: unknownID, die: unknownID,
	cluster: unknownID, node: unknownID,
}

// Valid reports whether the zone refers to real hardware.
func (z *Zone) Valid() bool {
	return z.valid
}

// Level returns the level of hardware this zone represents.
func (z *Zone) Level() Level {
	return z.level
}

// ID returns the id of the hardware this zone represents, as the kernel
// numbers it. Ids are unique within a level.
func (z *Zone) ID() ID {
	return z.id
}

// Name returns a stable name for the zone, spelling out where it sits, for
// instance "package#1/die#0/node#2". Names are unique within a [Machine]
// and are meant for logs and for keying by position; use [Zone.ID] to identify
// the hardware itself.
func (z *Zone) Name() string {
	return z.name
}

// CPUs returns the CPUs in this zone. The set is sealed.
func (z *Zone) CPUs() *libcpu.CpuMask {
	if z.cpus == nil {
		return emptyCPUs
	}
	return z.cpus
}

// MemoryNode returns the NUMA node this zone's CPUs are local to, or an invalid
// node if they span more than one.
func (z *Zone) MemoryNode() *MemoryNode {
	if z.m == nil || z.node == unknownID {
		return invalidMemoryNode
	}
	return z.m.MemoryNode(z.node)
}

// Cache returns the cache this zone represents, or an invalid cache if the zone
// is not a cache level.
func (z *Zone) Cache() *Cache {
	if z.cache == nil {
		return invalidCache
	}
	return z.cache
}

// DieID returns the coordinates of the die this zone is in, or is, with both ids
// -1 if it is not within a single die. Unlike [Zone.ID] this says which package
// the die belongs to, which matters because the kernel numbers dies per package
// rather than per machine.
func (z *Zone) DieID() DieID {
	if z.pkg == unknownID || z.die == unknownID {
		return DieID{Package: unknownID, Die: unknownID}
	}
	return DieID{Package: z.pkg, Die: z.die}
}

// ClusterID returns the coordinates of the cluster this zone is in, or is, with
// all ids -1 if it is not within a single cluster or the machine reports no
// meaningful clustering.
func (z *Zone) ClusterID() ClusterID {
	if z.pkg == unknownID || z.die == unknownID || z.cluster == unknownID {
		return ClusterID{Package: unknownID, Die: unknownID, Cluster: unknownID}
	}
	return ClusterID{Package: z.pkg, Die: z.die, Cluster: z.cluster}
}

// String returns [Zone.Name] and the zone's CPUs.
func (z *Zone) String() string {
	if !z.valid {
		return "<invalid zone>"
	}
	return z.name + " (" + z.CPUs().String() + ")"
}
