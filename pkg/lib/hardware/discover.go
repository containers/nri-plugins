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
	"io/fs"
	"path"
	"slices"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// Where things are, relative to the host root.
const (
	sysCPUDir  = "sys/devices/system/cpu"
	sysNodeDir = "sys/devices/system/node"
	procMemDir = "proc"
)

// Discover reads the topology of a machine and returns it.
func Discover(opts ...Option) (*Machine, error) {
	o := &options{}
	for _, apply := range opts {
		if err := apply(o); err != nil {
			return nil, err
		}
	}
	if o.fsys == nil {
		o.fsys = HostFS("/")
	}

	d := &discovery{
		m: &Machine{
			fsys:   o.fsys,
			cpus:   map[ID]*CPU{},
			nodes:  map[ID]*MemoryNode{},
			caches: map[CacheID]*Cache{},
			kinds:  map[CoreKind]*libcpu.CpuMask{},
			zones:  map[Level][]*Zone{},
		},
		opts: o,
	}

	for _, step := range []struct {
		what string
		run  func() error
	}{
		{"CPUs", d.discoverCPUs},
		{"core kinds", d.discoverCoreKinds},
		{"NUMA nodes", d.discoverMemoryNodes},
		{"memory kinds", d.classifyMemory},
		{"zones", d.buildZones},
	} {
		if err := step.run(); err != nil {
			return nil, fmt.Errorf("failed to discover %s: %w", step.what, err)
		}
	}

	d.m.index = d.m.buildIndex()

	return d.m, nil
}

// discovery is the state of one Discover call.
type discovery struct {
	m    *Machine
	opts *options
}

// fsys is the filesystem being read.
func (d *discovery) fsys() fs.FS {
	return d.m.fsys
}

//
// CPUs
//

// discoverCPUs reads the CPU sets the kernel publishes for the machine as a
// whole, then every cpuN directory.
func (d *discovery) discoverCPUs() error {
	m := d.m

	// The four machine-wide sets. Only "present" is essential; a kernel which
	// does not publish one of the others leaves it empty rather than failing,
	// except that "online" falls back to "present" because too much depends on
	// knowing which CPUs are usable.
	m.possible = d.readCPUsOrEmpty(path.Join(sysCPUDir, "possible"))
	m.present = d.readCPUsOrEmpty(path.Join(sysCPUDir, "present"))
	m.online = d.readCPUsOrEmpty(path.Join(sysCPUDir, "online"))
	m.isolated = d.readCPUsOrEmpty(path.Join(sysCPUDir, "isolated"))

	if m.online.IsEmpty() && !m.present.IsEmpty() {
		m.online = m.present
	}
	if m.possible.IsEmpty() {
		m.possible = m.present
	}

	names, ids, err := globIDs(d.fsys(), path.Join(sysCPUDir, "cpu[0-9]*"))
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no CPUs found under %s", sysCPUDir)
	}

	for i, name := range names {
		cpu, err := d.discoverCPU(name, ids[i])
		if err != nil {
			return fmt.Errorf("cpu%d: %w", ids[i], err)
		}
		m.cpus[cpu.id] = cpu
		m.cpuIDs = append(m.cpuIDs, cpu.id)
	}

	slices.Sort(m.cpuIDs)

	// A kernel which publishes no "present" is old enough that we should just
	// believe the directories instead.
	if m.present.IsEmpty() {
		present := libcpu.NewCpuMask(m.cpuIDs...)
		present.Seal()
		m.present = present
		if m.online.IsEmpty() {
			m.online = present
		}
	}

	return nil
}

// discoverCPU reads one cpuN directory.
//
// An offline CPU publishes no topology at all, so its ids stay unknown. That is
// not an error: the machine is expected to have offline CPUs.
func (d *discovery) discoverCPU(dir string, id ID) (*CPU, error) {
	c := &CPU{
		m:        d.m,
		id:       id,
		valid:    true,
		dir:      dir,
		online:   d.m.online.Contains(id),
		isolated: d.m.isolated.Contains(id),
		pkg:      unknownID,
		die:      unknownID,
		cluster:  unknownID,
		node:     unknownID,
		core:     unknownID,
	}

	if c.online {
		if err := d.readCPUTopology(c); err != nil {
			return nil, err
		}
	}

	c.freq = d.readCPUFreq(c)

	caches, err := d.discoverCPUCaches(c)
	if err != nil {
		return nil, err
	}
	c.caches = caches

	return c, nil
}

// readCPUTopology reads the topology/ ids of an online CPU. The package and core
// ids are required; the rest of the machine cannot be assembled without them.
// A die or cluster the kernel does not report stays unknown.
func (d *discovery) readCPUTopology(c *CPU) error {
	topo := path.Join(c.dir, "topology")

	pkg, err := readInt(d.fsys(), path.Join(topo, "physical_package_id"))
	if err != nil {
		return fmt.Errorf("no package id: %w", err)
	}
	c.pkg = pkg

	core, err := readInt(d.fsys(), path.Join(topo, "core_id"))
	if err != nil {
		return fmt.Errorf("no core id: %w", err)
	}
	c.core = core

	if die, err := readInt(d.fsys(), path.Join(topo, "die_id")); err == nil {
		c.die = die
	}
	if cluster, err := readInt(d.fsys(), path.Join(topo, "cluster_id")); err == nil {
		c.cluster = cluster
	}

	// core_cpus_list is the current name, thread_siblings_list the old one.
	threads, err := readCPUs(d.fsys(), path.Join(topo, "core_cpus_list"))
	if err != nil {
		threads, err = readCPUs(d.fsys(), path.Join(topo, "thread_siblings_list"))
		if err != nil {
			return fmt.Errorf("no thread siblings: %w", err)
		}
	}
	c.threads = threads

	// Which NUMA node a CPU is in shows up as a nodeN symlink in its directory.
	// A kernel built without NUMA has none, and everything is in node 0.
	if nodes, err := glob(d.fsys(), path.Join(c.dir, "node[0-9]*")); err == nil {
		if len(nodes) == 1 {
			if node, ok := trailingID(nodes[0]); ok {
				c.node = node
			}
		}
	}
	if c.node == unknownID {
		c.node = 0
	}

	// A die or a cluster the kernel does not name is still one of each: treat the
	// package as holding a single one, so that the level exists uniformly and is
	// simply uninformative. [Machine.SameZones] is then how a caller finds out
	// that it says nothing -- on such a machine the die and cluster levels hold
	// the same zones as each other and as the package.
	if c.die == unknownID {
		c.die = 0
	}
	if c.cluster == unknownID {
		c.cluster = 0
	}

	return nil
}

// readCPUFreq reads what cpufreq says about a CPU. Absent cpufreq support, or
// with the values overridden, the zero value or the override stands.
func (d *discovery) readCPUFreq(c *CPU) Freq {
	if freq, ok := d.opts.freq[c.id]; ok {
		return freq
	}

	dir := path.Join(c.dir, "cpufreq")
	freq := Freq{EPP: EPPUnknown}

	if base, err := readUint64(d.fsys(), path.Join(dir, "base_frequency")); err == nil {
		freq.Base = base
	}
	if min, err := readUint64(d.fsys(), path.Join(dir, "cpuinfo_min_freq")); err == nil {
		freq.Min = min
	}
	if max, err := readUint64(d.fsys(), path.Join(dir, "cpuinfo_max_freq")); err == nil {
		freq.Max = max
	}
	if epp, err := readFile(d.fsys(), path.Join(dir, "energy_performance_preference")); err == nil {
		freq.EPP = ParseEPP(epp)
	}

	return freq
}

//
// Caches
//

// discoverCPUCaches reads the caches of one CPU, lowest level first.
//
// A cache shared by several CPUs is read once and shared: the second CPU to
// mention it gets the same *Cache. Its identity is (level, kind, id), because
// the kernel numbers caches within a level and kind rather than across the
// machine.
func (d *discovery) discoverCPUCaches(c *CPU) ([]*Cache, error) {
	if caches, ok := d.opts.caches[c.id]; ok {
		return d.internCaches(caches), nil
	}

	names, _, err := globIDs(d.fsys(), path.Join(c.dir, "cache", "index[0-9]*"))
	if err != nil {
		return nil, err
	}

	caches := make([]*Cache, 0, len(names))
	for _, name := range names {
		cache, err := d.readCache(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		caches = append(caches, cache)
	}

	// Ordered by level, then kind, so that Cache(level) and the last-level
	// helpers can rely on it. globIDs already gives index order, which is
	// normally the same thing, but nothing promises that.
	slices.SortStableFunc(caches, func(a, b *Cache) int {
		if a.level != b.level {
			return a.level - b.level
		}
		return int(a.kind) - int(b.kind)
	})

	return caches, nil
}

// readCache reads one cache/indexN directory, returning the shared instance if
// this cache has been seen already.
func (d *discovery) readCache(dir string) (*Cache, error) {
	level, err := readInt(d.fsys(), path.Join(dir, "level"))
	if err != nil {
		return nil, fmt.Errorf("no level: %w", err)
	}

	kindStr, err := readFile(d.fsys(), path.Join(dir, "type"))
	if err != nil {
		return nil, fmt.Errorf("no type: %w", err)
	}
	kind, err := parseCacheKind(kindStr)
	if err != nil {
		return nil, err
	}

	// A cache without an id is one the kernel does not number. Fall back to the
	// lowest CPU sharing it, which is unique per cache within a level and kind.
	id, err := readInt(d.fsys(), path.Join(dir, "id"))
	haveID := err == nil

	cpus, err := readCPUs(d.fsys(), path.Join(dir, "shared_cpu_list"))
	if err != nil {
		return nil, fmt.Errorf("no shared CPUs: %w", err)
	}
	if !haveID {
		if cpus.IsEmpty() {
			return nil, fmt.Errorf("no id and no shared CPUs")
		}
		id = cpus.List()[0]
	}

	key := CacheID{Level: level, Kind: kind, ID: id}
	if have, ok := d.m.caches[key]; ok {
		return have, nil
	}

	size := int64(0)
	if str, err := readFile(d.fsys(), path.Join(dir, "size")); err == nil {
		if size, err = parseSize(str); err != nil {
			return nil, fmt.Errorf("bad size %q: %w", str, err)
		}
	}

	cache := &Cache{
		m:     d.m,
		valid: true,
		id:    id,
		level: level,
		kind:  kind,
		size:  size,
		cpus:  cpus,
	}
	d.m.caches[key] = cache

	return cache, nil
}

// internCaches turns overridden cache descriptions into shared instances.
func (d *discovery) internCaches(caches []*Cache) []*Cache {
	out := make([]*Cache, 0, len(caches))
	for _, c := range caches {
		key := CacheID{Level: c.level, Kind: c.kind, ID: c.id}
		have, ok := d.m.caches[key]
		if !ok {
			c.m, c.valid = d.m, true
			d.m.caches[key] = c
			have = c
		}
		out = append(out, have)
	}
	return out
}

//
// Core kinds
//

// discoverCoreKinds works out which CPUs are performance cores and which are
// efficiency cores.
//
// The kernel exposes this as two lists, one per kind. A machine whose cores are
// all alike has neither, and everything is a performance core. A machine which
// names only one kind has the rest inferred, since there are only two.
func (d *discovery) discoverCoreKinds() error {
	m := d.m

	if len(d.opts.kinds) > 0 {
		for kind, cpus := range d.opts.kinds {
			m.kinds[kind] = cpus
		}
	} else {
		for kind, dir := range coreKindDirs {
			cpus, err := readCPUs(d.fsys(), dir)
			if err != nil || cpus.IsEmpty() {
				continue
			}
			m.kinds[kind] = cpus
		}
	}

	switch len(m.kinds) {
	case 0:
		m.kinds[PerformanceCore] = m.online

	case 1:
		for kind, cpus := range m.kinds {
			// Round the named kind up to whole cores, and let the other kind be
			// whatever is left. A partial list is a list of cores, not threads.
			named := m.allThreads(cpus)
			if named.Equals(m.online) {
				break
			}
			rest := m.online.Difference(named)

			named.Seal()
			other := libcpu.NewCpuMask(rest.UnsortedList()...)
			other.Seal()

			m.kinds[kind] = named
			m.kinds[otherCoreKind(kind)] = other
			break
		}
	}

	return d.checkCoreKinds()
}

// checkCoreKinds rejects a core kind split which cannot be true: kinds have to
// be whole cores, a core cannot be of two kinds, and every online CPU has to
// have one.
func (d *discovery) checkCoreKinds() error {
	m := d.m

	seen := libcpu.NewCpuMask()
	for kind, cpus := range m.kinds {
		if missing := m.allThreads(cpus).Difference(cpus); !missing.IsEmpty() {
			return fmt.Errorf("%s CPUs (%s) are missing thread siblings (%s)",
				kind, cpus, missing)
		}
		if overlap := cpus.Intersection(seen); !overlap.IsEmpty() {
			return fmt.Errorf("%s CPUs (%s) overlap another kind (%s)",
				kind, cpus, overlap)
		}
		seen = libcpu.NewCpuMask(seen.Union(cpus).UnsortedList()...)
	}

	if missing := m.online.Difference(seen); !missing.IsEmpty() {
		return fmt.Errorf("CPUs %s are of no known core kind", missing)
	}

	for kind, cpus := range m.kinds {
		cpus.ForEachCpu(func(id int) bool {
			if c, ok := m.cpus[id]; ok {
				c.kind = kind
			}
			return true
		})
	}

	return nil
}

//
// Memory nodes
//

// discoverMemoryNodes reads the NUMA nodes.
//
// A kernel built without NUMA has no node directories at all, in which case
// there is one node holding every online CPU and all of the memory.
func (d *discovery) discoverMemoryNodes() error {
	m := d.m

	names, ids, err := globIDs(d.fsys(), path.Join(sysNodeDir, "node[0-9]*"))
	if err != nil {
		return err
	}

	if len(names) == 0 {
		node := &MemoryNode{
			m:        m,
			valid:    true,
			id:       0,
			cpus:     m.online,
			distance: []int{localDistance},
			normal:   true,
			kind:     MemoryKindDRAM,
			meminfo:  path.Join(procMemDir, "meminfo"),
		}

		// Its capacity is the machine's own, there being no node to ask. Read it
		// here as the per-node case below does: a node whose capacity is unknown
		// reads as a node with no memory, which is worse than not discovering.
		info, err := readMemInfo(d.fsys(), node.meminfo, node.id)
		if err != nil {
			return fmt.Errorf("no NUMA nodes and no %s: %w", node.meminfo, err)
		}
		node.capacity = info.Total

		m.nodes[0] = node
		m.nodeIDs = []ID{0}
		return nil
	}

	normal := d.readCPUsOrEmpty(path.Join(sysNodeDir, "has_normal_memory"))

	for i, name := range names {
		node := &MemoryNode{
			m:       m,
			valid:   true,
			id:      ids[i],
			dir:     name,
			meminfo: path.Join(name, "meminfo"),
			kind:    MemoryKindUnknown,
			normal:  normal.Contains(ids[i]),
		}

		if node.cpus, err = readCPUs(d.fsys(), path.Join(name, "cpulist")); err != nil {
			return fmt.Errorf("node%d: no CPU list: %w", ids[i], err)
		}
		if node.distance, err = readInts(d.fsys(), path.Join(name, "distance"), " "); err != nil {
			return fmt.Errorf("node%d: no distance vector: %w", ids[i], err)
		}

		info, err := readMemInfo(d.fsys(), node.meminfo, ids[i])
		if err != nil {
			return fmt.Errorf("node%d: %w", ids[i], err)
		}
		node.capacity = info.Total

		m.nodes[node.id] = node
		m.nodeIDs = append(m.nodeIDs, node.id)
	}

	slices.Sort(m.nodeIDs)

	d.symmetrizeDistances()

	return nil
}

// symmetrizeDistances averages a NUMA distance matrix which is not symmetric.
//
// Distance is a cost, and a cost which differs by direction breaks every caller
// which treats it as one. Rather than let that surface far away, average the two
// and say so.
func (d *discovery) symmetrizeDistances() {
	m := d.m

	for _, i := range m.nodeIDs {
		for _, j := range m.nodeIDs {
			if i >= j {
				continue
			}
			a, b := m.nodes[i], m.nodes[j]
			if j >= len(a.distance) || i >= len(b.distance) {
				continue
			}
			if a.distance[j] == b.distance[i] {
				continue
			}
			avg := (a.distance[j] + b.distance[i]) / 2
			a.distance[j], b.distance[i] = avg, avg
		}
	}
}

//
// Options
//

// Option configures [Discover].
type Option func(*options) error

// options is the accumulated configuration of a [Discover] call.
type options struct {
	fsys   fs.FS
	kinds  map[CoreKind]*libcpu.CpuMask
	caches map[ID][]*Cache
	freq   map[ID]Freq
}

// WithRoot reads the topology below root instead of "/", for a host filesystem
// mounted somewhere else. It is shorthand for WithFS(HostFS(root)).
func WithRoot(root string) Option {
	return func(o *options) error {
		o.fsys = HostFS(root)
		return nil
	}
}

// WithFS reads the topology through fsys instead of the real filesystem. Paths
// are relative to the host root, so a "sys" and a "proc" directory are both
// expected within it. Discovery only reads, so a read-only fs.FS is enough.
func WithFS(fsys fs.FS) Option {
	return func(o *options) error {
		if fsys == nil {
			return fmt.Errorf("WithFS: nil filesystem")
		}
		o.fsys = fsys
		return nil
	}
}

//
// helpers
//

// unknownID is the id of a coordinate the machine does not report.
const unknownID = -1

// localDistance is the NUMA distance from a node to itself.
const localDistance = 10

// coreKindDirs is where the kernel lists the CPUs of each core kind.
var coreKindDirs = map[CoreKind]string{
	PerformanceCore: "sys/devices/cpu_core/cpus",
	EfficientCore:   "sys/devices/cpu_atom/cpus",
}

// otherCoreKind returns the kind which is not this one. There are two.
func otherCoreKind(kind CoreKind) CoreKind {
	if kind == PerformanceCore {
		return EfficientCore
	}
	return PerformanceCore
}

// readCPUsOrEmpty reads a CPU list, treating anything unreadable as empty. It is
// for the attributes whose absence means "none", not "broken".
func (d *discovery) readCPUsOrEmpty(name string) *libcpu.CpuMask {
	if cpus, err := readCPUs(d.fsys(), name); err == nil {
		return cpus
	}
	return emptyCPUs
}

// allThreads rounds a set of CPUs up to whole cores. It is on Machine rather
// than in convenience.go because discovery needs it before a Machine is finished.
func (m *Machine) allThreads(cpus libcpu.CPUSet) *libcpu.CpuMask {
	all := libcpu.NewCpuMask()
	cpus.ForEachCpu(func(id int) bool {
		if c, ok := m.cpus[id]; ok && c.threads != nil {
			all.Set(c.threads.UnsortedList()...)
		} else {
			all.Set(id)
		}
		return true
	})
	return all
}
