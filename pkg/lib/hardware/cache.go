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
	"fmt"
	"slices"
	"strconv"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// Cache is one CPU cache, and the CPUs which share it. It is a handle into
// the [Machine] which produced it: never nil, and safe to use even when it
// refers to no cache, in which case it reports Valid() == false.
//
// A cache the machine reports once is one Cache here, so two CPUs sharing an L3
// get the same handle for it and comparing handles is a valid identity test.
// [Cache.Key] is for when a string is wanted instead, for a log line or a map
// keyed by something other than the handle.
type Cache struct {
	m     *Machine
	valid bool

	id    ID
	level int
	kind  CacheKind
	size  int64
	cpus  *libcpu.CpuMask
	zone  *Zone
}

// invalidCache is what a lookup for a cache the machine does not have returns.
// As in pkg/sysfs its methods answer with zero values rather than panicking.
var invalidCache = &Cache{}

// Valid reports whether this handle refers to a cache the machine has.
func (c *Cache) Valid() bool {
	return c.valid
}

// ID returns the id of this cache, as the kernel numbers it. Ids are unique
// within a level and kind, not across the machine; see [Cache.Key].
func (c *Cache) ID() ID {
	return c.id
}

// Level returns which level of cache this is: 1 for L1, 2 for L2, and so on.
func (c *Cache) Level() int {
	return c.level
}

// Kind returns whether this cache holds data, instructions, or both.
func (c *Cache) Kind() CacheKind {
	return c.kind
}

// Size returns the size of this cache in bytes, or 0 if the machine does not
// say.
func (c *Cache) Size() int64 {
	return c.size
}

// CPUs returns the CPUs sharing this cache. The set is sealed.
func (c *Cache) CPUs() *libcpu.CpuMask {
	if c.cpus == nil {
		return emptyCPUs
	}
	return c.cpus
}

// CacheID returns the full coordinates of this cache, for looking it up in a
// [TopologyIndex] or keying a map by it. Prefer this to assembling a [CacheID]
// by hand: all three fields are needed, and forgetting the kind silently names a
// different cache.
func (c *Cache) CacheID() CacheID {
	if !c.valid {
		return CacheID{Level: 0, Kind: UnifiedCache, ID: unknownID}
	}
	return CacheID{Level: c.level, Kind: c.kind, ID: c.id}
}

// Key returns a string which identifies this cache within the machine, unlike
// [Cache.ID] which is only unique within a level and kind. It is usable as a
// map key.
func (c *Cache) Key() string {
	if !c.valid {
		return ""
	}
	return "L" + strconv.Itoa(c.level) + c.kind.suffix() + "#" + strconv.Itoa(c.id)
}

// Zone returns this cache as a zone, or an invalid zone if the cache is at a
// level which does not group CPUs usefully.
func (c *Cache) Zone() *Zone {
	if c.zone == nil {
		return invalidZone
	}
	return c.zone
}

// String returns something like "L3#0 (unified, 32M)".
func (c *Cache) String() string {
	if !c.valid {
		return "L?#?"
	}
	return fmt.Sprintf("%s (%s, %s)", c.Key(), c.kind, sizeString(c.size))
}

// CacheKind is what a cache holds.
type CacheKind int

const (
	// DataCache holds data only.
	DataCache CacheKind = iota
	// InstructionCache holds instructions only.
	InstructionCache
	// UnifiedCache holds both.
	UnifiedCache
)

// String returns "data", "instruction" or "unified".
func (k CacheKind) String() string {
	switch k {
	case DataCache:
		return "data"
	case InstructionCache:
		return "instruction"
	case UnifiedCache:
		return "unified"
	}
	return "unknown cache kind"
}

// suffix distinguishes a data from an instruction cache in a [Cache.Key],
// since the two can share a level and an id. A unified cache needs none.
func (k CacheKind) suffix() string {
	switch k {
	case DataCache:
		return "d"
	case InstructionCache:
		return "i"
	}
	return ""
}

// parseCacheKind returns the kind the kernel names in a cache's type attribute.
func parseCacheKind(s string) (CacheKind, error) {
	switch s {
	case "Data":
		return DataCache, nil
	case "Instruction":
		return InstructionCache, nil
	case "Unified":
		return UnifiedCache, nil
	}
	return UnifiedCache, fmt.Errorf("unknown cache type %q", s)
}

// compareCaches orders caches by level, then kind, then id, which is the order
// everything here hands them out in.
func compareCaches(a, b *Cache) int {
	if a.level != b.level {
		return a.level - b.level
	}
	if a.kind != b.kind {
		return int(a.kind) - int(b.kind)
	}
	return a.id - b.id
}

// sortCaches orders a CPU's caches the way discovery hands them out: level
// first, then kind.
func sortCaches(caches []*Cache) {
	slices.SortStableFunc(caches, compareCaches)
}

// parseSize parses a cache size as the kernel writes it: a number, optionally
// followed by a unit of K, M or G.
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1<<30, s[:len(s)-1]
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}

	return n * mult, nil
}

// sizeString renders a size the way the kernel writes one.
func sizeString(size int64) string {
	switch {
	case size == 0:
		return "0"
	case size%(1<<30) == 0:
		return strconv.FormatInt(size/(1<<30), 10) + "G"
	case size%(1<<20) == 0:
		return strconv.FormatInt(size/(1<<20), 10) + "M"
	case size%(1<<10) == 0:
		return strconv.FormatInt(size/(1<<10), 10) + "K"
	}
	return strconv.FormatInt(size, 10)
}
