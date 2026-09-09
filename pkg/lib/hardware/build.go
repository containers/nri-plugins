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
	"os"
	"slices"
	"strconv"
	"strings"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

//
// Memory kinds
//

// classifyMemory works out what sort of memory each NUMA node holds.
//
// The kernel does not say, so this is inference, and the same inference
// pkg/sysfs makes: a node with CPUs of its own holds ordinary DRAM, and a node
// with memory but no CPUs holds something special. Which special sort is
// guessed from size, since high-bandwidth memory is smaller than a DRAM node
// and persistent memory is larger.
//
// Unlike pkg/sysfs this does not fail when the guess cannot be made. A machine
// with only CPU-less nodes, or with no DRAM to compare against, gets
// [MemoryKindUnknown] and stays usable; refusing to describe the machine at all
// is worse than admitting to not knowing.
func (d *discovery) classifyMemory() error {
	m := d.m

	var (
		dramTotal int64
		dramNodes int64
		special   []*MemoryNode
	)

	for _, node := range m.nodes {
		switch {
		case !node.cpus.IsEmpty():
			// CPUs of its own: ordinary memory, whether it has any or not
			node.kind = MemoryKindDRAM
			if node.capacity > 0 {
				dramTotal += node.capacity
				dramNodes++
			}
		case node.capacity == 0:
			// no CPUs and no memory: nothing to classify, and nothing which
			// would want to allocate from it either
			node.kind = MemoryKindUnknown
		default:
			special = append(special, node)
		}
	}

	if len(special) == 0 {
		return nil
	}

	if dramNodes == 0 {
		// Nothing to compare against. Leave them unknown rather than guessing.
		return nil
	}

	average := dramTotal / dramNodes
	for _, node := range special {
		if node.capacity < average {
			node.kind = MemoryKindHBM
		} else {
			node.kind = MemoryKindPMEM
		}
	}

	return nil
}

//
// Zones
//

// buildZones collects the zones of every level the machine has.
//
// There is no tree. Zones at one level are a set of CPU sets, and nothing here
// decides which level contains which or which level is worth keeping: those are
// questions the hardware does not answer and different callers answer
// differently. [Machine.Zones] lists them, [Machine.SameZones] says when two
// levels cut the machine the same way, and containment is a query.
func (d *discovery) buildZones() error {
	m := d.m

	for _, level := range allLevels {
		zones := d.zonesAt(level)
		if len(zones) == 0 {
			continue
		}
		m.zones[level] = zones
		m.levels = append(m.levels, level)
	}

	d.indexZonesByCPU()

	return nil
}

// zonesAt collects the zones of one level.
func (d *discovery) zonesAt(level Level) []*Zone {
	m := d.m

	// group holds the CPUs of each zone, keyed by a value which is unique at
	// this level, and the coordinates that key stands for.
	type group struct {
		cpus  *libcpu.CpuMask
		id    ID
		at    Coordinates
		cache *Cache
	}
	groups := map[ID]*group{}

	add := func(key ID, id ID, at Coordinates, cache *Cache, cpus ...int) {
		g, ok := groups[key]
		if !ok {
			g = &group{cpus: libcpu.NewCpuMask(), id: id, at: at, cache: cache}
			groups[key] = g
		}
		g.cpus.Set(cpus...)
	}

	switch level {
	case LevelPackage, LevelDie, LevelCluster, LevelCore:
		for _, id := range m.cpuIDs {
			c := m.cpus[id]
			if !c.online {
				continue
			}
			key, zid, ok := zoneKeyOf(c, level)
			if !ok {
				continue
			}
			add(key, zid, c.coordinates(), nil, c.id)
		}

	case LevelNUMANode:
		for _, id := range m.nodeIDs {
			node := m.nodes[id]
			if node.cpus.IsEmpty() {
				// memory only: a MemoryNode, but no set of CPUs to be a zone of
				continue
			}
			at := Coordinates{
				CPU: unknownID, Package: unknownID, Die: unknownID,
				Cluster: unknownID, MemoryNode: id, Core: unknownID,
			}
			add(id, id, at, nil, node.cpus.UnsortedList()...)
		}

	case LevelL3Cache, LevelL2Cache:
		want := cacheLevelOf(level)
		for key, cache := range m.caches {
			// only unified caches make a zone: a level with separate data and
			// instruction caches has two per group of CPUs, which would be two
			// zones holding the same CPUs
			if key.Level != want || key.Kind != UnifiedCache || cache.cpus.IsEmpty() {
				continue
			}
			at := Coordinates{
				CPU: unknownID, Package: unknownID, Die: unknownID,
				Cluster: unknownID, MemoryNode: unknownID, Core: unknownID,
			}
			add(cache.id, cache.id, at, cache, cache.cpus.UnsortedList()...)
		}

	case LevelThread:
		for _, id := range m.cpuIDs {
			add(id, id, m.cpus[id].coordinates(), nil, id)
		}
	}

	keys := make([]ID, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	zones := make([]*Zone, 0, len(keys))
	for _, key := range keys {
		g := groups[key]
		g.cpus.Seal()

		z := &Zone{
			m: m, valid: true, level: level, id: g.id, cpus: g.cpus,
			pkg: g.at.Package, die: g.at.Die, cluster: g.at.Cluster,
			node: g.at.MemoryNode, cache: g.cache,
		}
		// A zone whose CPUs span several of something is in none of them.
		d.narrowCoordinates(z)
		z.name = z.zoneName()

		if z.cache != nil {
			z.cache.zone = z
		}
		zones = append(zones, z)
	}

	return zones
}

// narrowCoordinates drops any coordinate of a zone which is not the same for
// every CPU in it, since a zone spanning two packages is in neither.
func (d *discovery) narrowCoordinates(z *Zone) {
	first := true
	z.cpus.ForEachCpu(func(id int) bool {
		c, ok := d.m.cpus[id]
		if !ok {
			return true
		}
		at := c.coordinates()
		if first {
			z.pkg, z.die = at.Package, at.Die
			z.cluster, z.node = at.Cluster, at.MemoryNode
			first = false
			return true
		}
		if z.pkg != at.Package {
			z.pkg = unknownID
		}
		if z.die != at.Die {
			z.die = unknownID
		}
		if z.cluster != at.Cluster {
			z.cluster = unknownID
		}
		if z.node != at.MemoryNode {
			z.node = unknownID
		}
		return true
	})

	// a die or cluster id only means something together with its package
	if z.pkg == unknownID {
		z.die, z.cluster = unknownID, unknownID
	}
	if z.die == unknownID {
		z.cluster = unknownID
	}
}

// zoneName names a zone from its coordinates, so that the name says where it is
// without depending on a tree.
func (z *Zone) zoneName() string {
	name := levelNames[z.level] + "#" + strconv.Itoa(z.id)

	switch z.level {
	case LevelDie, LevelCore:
		if z.pkg != unknownID {
			name = "package#" + strconv.Itoa(z.pkg) + "/" + name
		}
	case LevelCluster:
		if z.pkg != unknownID && z.die != unknownID {
			name = "package#" + strconv.Itoa(z.pkg) +
				"/die#" + strconv.Itoa(z.die) + "/" + name
		}
	}

	return name
}

// zoneKeyOf returns a key which is unique for a CPU's zone at a level, the id
// that zone reports, and whether the machine reports one at all.
//
// Package, die, cluster and core ids are numbered within their parent, so a
// bare id would collide across packages. The key combines them; the id stays
// the kernel's.
func zoneKeyOf(c *CPU, level Level) (key ID, id ID, ok bool) {
	switch level {
	case LevelPackage:
		return c.pkg, c.pkg, c.pkg != unknownID
	case LevelDie:
		return c.pkg<<20 | c.die, c.die,
			c.pkg != unknownID && c.die != unknownID
	case LevelCluster:
		return c.pkg<<40 | c.die<<20 | c.cluster, c.cluster,
			c.pkg != unknownID && c.die != unknownID && c.cluster != unknownID
	case LevelCore:
		return c.pkg<<20 | c.core, c.core,
			c.pkg != unknownID && c.core != unknownID
	}
	return unknownID, unknownID, false
}

// indexZonesByCPU records which zone at each level holds each CPU and each NUMA
// node, so that CPU.Zone and MemoryNode.Zone are an array index, not a search.
func (d *discovery) indexZonesByCPU() {
	m := d.m

	for _, level := range m.levels {
		for _, z := range m.zones[level] {
			z.cpus.ForEachCpu(func(id int) bool {
				if c, ok := m.cpus[id]; ok {
					c.zones[level] = z
				}
				return true
			})
		}
	}

	for _, id := range m.nodeIDs {
		node := m.nodes[id]
		if node.cpus.IsEmpty() {
			continue
		}
		for _, level := range m.levels {
			for _, z := range m.zones[level] {
				if !node.cpus.IsSubsetOf(z.cpus) {
					continue
				}
				have := node.zones[level]
				if have == nil || z.cpus.Size() < have.cpus.Size() {
					node.zones[level] = z
				}
			}
		}
	}
}

// cacheLevelOf returns the cache level a zone level stands for, or 0.
func cacheLevelOf(level Level) int {
	switch level {
	case LevelL2Cache:
		return 2
	case LevelL3Cache:
		return 3
	}
	return 0
}

//
// Environment overrides
//

// The variables [WithEnvOverrides] reads. Their contents are what pkg/sysfs
// accepts, so that an end-to-end test which sets them keeps working across the
// move.
const (
	envCoreCPUs = "OVERRIDE_SYS_CORE_CPUS"
	envAtomCPUs = "OVERRIDE_SYS_ATOM_CPUS"
	envCaches   = "OVERRIDE_SYS_CACHES"
	envCPUFreq  = "OVERRIDE_SYS_CPUFREQ"
)

// WithEnvOverrides applies the OVERRIDE_SYS_* environment variables, which
// substitute core kinds, cache layout and CPU frequencies for the ones the
// machine reports.
//
// They exist for the end-to-end tests. Those install real plugin binaries into
// a freshly provisioned qemu virtual machine, with a real CRI runtime and a real
// Kubernetes cluster, and then check what the plugins actually do. Some of the
// hardware worth testing against is not something qemu emulates -- hybrid
// cores, a particular cache layout, cpufreq at all -- so a test sets these to
// make the plugin see hardware the VM does not have, and the feature gets
// exercised at least that far.
//
// That is what they are for. Inside this repository's own tests [WithFS] is the
// better tool: a recorded or synthetic topology needs no environment variable
// and no virtual machine.
func WithEnvOverrides() Option {
	return func(o *options) error {
		return o.applyEnvOverrides()
	}
}

// applyEnvOverrides reads the OVERRIDE_SYS_* variables into the options.
func (o *options) applyEnvOverrides() error {
	for kind, name := range map[CoreKind]string{
		PerformanceCore: envCoreCPUs,
		EfficientCore:   envAtomCPUs,
	} {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		cpus, err := libcpu.ParseCpuMask(value)
		if err != nil {
			return newOverrideError(name, value, err)
		}
		cpus.Seal()
		if o.kinds == nil {
			o.kinds = map[CoreKind]*libcpu.CpuMask{}
		}
		o.kinds[kind] = cpus
	}

	if value := os.Getenv(envCaches); value != "" {
		caches, err := parseCacheOverrides(value)
		if err != nil {
			return newOverrideError(envCaches, value, err)
		}
		o.caches = caches
	}

	if value := os.Getenv(envCPUFreq); value != "" {
		freq, err := parseFreqOverrides(value)
		if err != nil {
			return newOverrideError(envCPUFreq, value, err)
		}
		o.freq = freq
	}

	return nil
}

// newOverrideError says which variable was wrong, since a mistake in one is
// otherwise hard to place.
func newOverrideError(name, value string, err error) error {
	return &overrideError{name: name, value: value, err: err}
}

// overrideError is a malformed OVERRIDE_SYS_* variable.
type overrideError struct {
	name  string
	value string
	err   error
}

// Error implements error.
func (e *overrideError) Error() string {
	return "bad " + e.name + "=" + strconv.Quote(e.value) + ": " + e.err.Error()
}

// Unwrap returns the underlying parse failure.
func (e *overrideError) Unwrap() error {
	return e.err
}

// trimmed is strings.TrimSpace, named for what the overrides need it for.
func trimmed(s string) string {
	return strings.TrimSpace(s)
}
