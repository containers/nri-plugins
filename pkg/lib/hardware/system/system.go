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

package system

import (
	"fmt"
	"path"
	"path/filepath"
	"strconv"

	"github.com/containers/nri-plugins/pkg/lib/hardware"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"github.com/intel/goresctrl/pkg/sst"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// System devices
//
// Declared exactly as pkg/sysfs declares it. Do not tidy it: the point is that
// it matches, so that a consumer can switch to this package without touching
// anything but the import.
type System interface {
	Discover(flags DiscoveryFlag) error
	SetCpusOnline(online bool, cpus idset.IDSet) (idset.IDSet, error)
	SetCPUFrequencyLimits(min, max uint64, cpus idset.IDSet) error
	PackageIDs() []idset.ID
	NodeIDs() []idset.ID
	FilterNodes(ids []idset.ID, filters ...NodeFilter) idset.IDSet
	FilterNode(id idset.ID, filters ...NodeFilter) bool
	ClosestNodes(id idset.ID, filters ...NodeFilter) ([]idset.IDSet, []int)
	CPUIDs() []idset.ID
	PackageCount() int
	SocketCount() int
	CPUCount() int
	NUMANodeCount() int
	MinThreadCount() int
	MaxThreadCount() int
	CPUSet() cpuset.CPUSet
	Package(id idset.ID) CPUPackage
	Node(id idset.ID) Node
	NodeDistance(from, to idset.ID) int
	CPU(id idset.ID) CPU
	PossibleCPUs() cpuset.CPUSet
	PresentCPUs() cpuset.CPUSet
	OnlineCPUs() cpuset.CPUSet
	IsolatedCPUs() cpuset.CPUSet
	OfflineCPUs() cpuset.CPUSet
	CoreKindCPUs(CoreKind) cpuset.CPUSet
	CoreKinds() []CoreKind
	IDSetForCPUs(cpuset.CPUSet, func(CPU) idset.ID) idset.IDSet
	AllThreadsForCPUs(cpuset.CPUSet) cpuset.CPUSet
	SingleThreadForCPUs(cpuset.CPUSet) cpuset.CPUSet
	AllCPUsSharingNthLevelCacheWithCPUs(int, cpuset.CPUSet) cpuset.CPUSet

	Offlined() cpuset.CPUSet
	Isolated() cpuset.CPUSet

	NodeHintToCPUs(string) string

	Sst() *sst.Platform
}

// CPUPackage is a physical package (a collection of CPUs).
type CPUPackage interface {
	ID() idset.ID
	CPUSet() cpuset.CPUSet
	DieIDs() []idset.ID
	NodeIDs() []idset.ID
	DieNodeIDs(idset.ID) []idset.ID
	DieCPUSet(idset.ID) cpuset.CPUSet
	DieClusterIDs(idset.ID) []idset.ID
	DieClusterCPUSet(idset.ID, idset.ID) cpuset.CPUSet
	LogicalDieClusterIDs(idset.ID) []idset.ID
	LogicalDieClusterCPUSet(idset.ID, idset.ID) cpuset.CPUSet
	L3CacheIDs() []idset.ID
	L3CacheCPUSet(idset.ID) cpuset.CPUSet
	SstInfo() *sst.PackageStatus
}

// Node represents a NUMA node.
type Node interface {
	ID() idset.ID
	PackageID() idset.ID
	DieID() idset.ID
	CPUSet() cpuset.CPUSet
	Distance() []int
	DistanceFrom(id idset.ID) int
	ClosestNodes() ([]idset.IDSet, []int)
	MemoryInfo() (*MemInfo, error)
	GetMemoryType() MemoryType
	HasNormalMemory() bool
}

// CPU is a CPU core.
type CPU interface {
	ID() idset.ID
	PackageID() idset.ID
	DieID() idset.ID
	ClusterID() idset.ID
	NodeID() idset.ID
	CoreID() idset.ID
	ThreadCPUSet() cpuset.CPUSet
	BaseFrequency() uint64
	FrequencyRange() CPUFreq
	EPP() EPP
	Online() bool
	Isolated() bool
	SetFrequencyLimits(min, max uint64) error
	SstClos() int
	CacheCount() int
	GetCaches() []*Cache
	GetCachesByLevel(int) []*Cache
	GetCacheByIndex(int) *Cache
	GetNthLevelCacheCPUSet(n int) cpuset.CPUSet
	GetLastLevelCaches() []*Cache
	GetLastLevelCacheCPUSet() cpuset.CPUSet
	CoreKind() CoreKind
}

//
// Construction
//

// sysRoot is the parent directory of the host's /sys, as [SetSysRoot] last set
// it. Package-global, as in pkg/sysfs, and read when a System is built.
var sysRoot string

// SetSysRoot sets the sys root directory.
func SetSysRoot(root string) {
	if root == "" {
		sysRoot = ""
		return
	}

	root = filepath.Clean(root)
	if root != "" && !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			panic(fmt.Errorf("failed to resolve %q to absolute path: %v", root, err))
		}
		root = abs
	}
	if root == "/" {
		root = ""
	}

	sysRoot = root
}

// DiscoverSystem performs discovery of the running systems details.
//
// The flags are accepted and ignored, as they are in pkg/sysfs, whose
// DiscoverSystem drops them before passing them on.
func DiscoverSystem(args ...DiscoveryFlag) (System, error) {
	return discover(filepath.Join("/", sysRoot))
}

// DiscoverSystemAt performs discovery of the running systems details from sysfs
// mounted at path.
//
// path names the sysfs mount point, so it ends in "/sys", whereas the hardware
// package takes the host root above it. The two are translated here.
func DiscoverSystemAt(path string, args ...DiscoveryFlag) (System, error) {
	root, base := filepath.Split(filepath.Clean(path))
	if base != "sys" {
		return nil, fmt.Errorf("%q does not look like a sysfs mount point", path)
	}
	if root == "" {
		root = "."
	}

	return discover(filepath.Clean(root))
}

// FromMachine wraps an already discovered [hardware.Machine] in the pkg/sysfs
// interface. It is the seam a caller which has moved on to hardware uses to keep
// feeding a caller which has not, so it has to yield a System which is complete
// in every way the ones below do: SST is probed here too, and the write paths go
// to the same tree the machine was read from.
func FromMachine(m *hardware.Machine) System {
	sys := newSystem(m)
	sys.discoverSst()
	return sys
}

// discover reads a machine below root and wraps it.
func discover(root string) (System, error) {
	m, err := hardware.Discover(hardware.WithRoot(root), hardware.WithEnvOverrides())
	if err != nil {
		return nil, err
	}

	return FromMachine(m), nil
}

// system implements [System] over a [hardware.Machine].
type system struct {
	hw *hardware.Machine
	x  *hardware.TopologyIndex

	cpus   map[idset.ID]*cpu
	nodes  map[idset.ID]*node
	pkgs   map[idset.ID]*cpuPackage
	caches map[*hardware.Cache]*Cache

	// SST, which the hardware package deliberately knows nothing about
	sst     *sst.Platform
	sstClos map[idset.ID]int
	sstPkg  map[idset.ID]*sst.PackageStatus
}

// newSystem wraps a machine, interning one wrapper per piece of hardware so that
// callers which compare or key by them keep working.
func newSystem(m *hardware.Machine) *system {
	sys := &system{
		hw:      m,
		x:       m.TopologyIndex(),
		cpus:    map[idset.ID]*cpu{},
		nodes:   map[idset.ID]*node{},
		pkgs:    map[idset.ID]*cpuPackage{},
		caches:  map[*hardware.Cache]*Cache{},
		sstClos: map[idset.ID]int{},
		sstPkg:  map[idset.ID]*sst.PackageStatus{},
	}

	for _, id := range m.CPUIDs() {
		sys.cpus[id] = &cpu{sys: sys, hw: m.CPU(id)}
	}
	for _, n := range m.MemoryNodes() {
		sys.nodes[n.ID()] = &node{sys: sys, hw: n}
	}
	for _, z := range m.Zones(hardware.LevelPackage) {
		sys.pkgs[z.ID()] = &cpuPackage{sys: sys, hw: z}
	}

	return sys
}

// wrapCache returns the one wrapper for a cache, so that a caller which keys a
// map by *Cache -- as the balloons policy does -- sees the same pointer for the
// same cache.
func (s *system) wrapCache(hw *hardware.Cache) *Cache {
	if c, ok := s.caches[hw]; ok {
		return c
	}
	c := &Cache{hw: hw}
	s.caches[hw] = c
	return c
}

// wrapCaches wraps a list of caches.
func (s *system) wrapCaches(hw []*hardware.Cache) []*Cache {
	out := make([]*Cache, 0, len(hw))
	for _, c := range hw {
		out = append(out, s.wrapCache(c))
	}
	return out
}

// write writes a number to a sysfs attribute of the machine.
//
// Through the machine's own filesystem, not a fresh one built from a root we
// remembered: that is the only way a write lands in the tree the topology was
// read from, whether that is the host root, a recorded tree or an injected fs.FS.
func (s *system) write(name string, value uint64) error {
	fsys, ok := s.hw.FS().(hardware.WriterFS)
	if !ok {
		return fmt.Errorf("cannot write %s: the filesystem is read-only", name)
	}
	// pkg/sysfs appends a newline; the kernel does not care but keep it identical
	return fsys.WriteFile(name, []byte(strconv.FormatUint(value, 10)+"\n"))
}

//
// SST
//
// The hardware package has nothing to do with Intel Speed Select, so the three
// interface methods which expose it are served from here. This is a port of
// pkg/sysfs.discoverSst. It is the last thing which should move out of this
// package, and it moves to whoever still wants it -- today only pkg/cpuallocator.
//

// discoverSst probes Speed Select and records what it says. Failure is not
// fatal: SST is simply reported as absent, as pkg/sysfs does.
func (s *system) discoverSst() {
	if !sst.SstSupported() {
		return
	}

	platform, err := sst.Init()
	if err != nil || platform == nil {
		return
	}
	s.sst = platform

	for id := range s.pkgs {
		pkg, ok := platform.Package(id)
		if !ok {
			continue
		}
		status, err := pkg.GetStatus()
		if err != nil {
			continue
		}

		for _, punit := range status.Punits {
			if !punit.CP.Supported || !punit.CP.Enabled {
				continue
			}
			for _, cpu := range punit.CPUs.SortedMembers() {
				clos, err := platform.GetCPUClosID(cpu)
				if err != nil {
					continue
				}
				s.sstClos[cpu] = clos
			}
		}

		s.sstPkg[id] = status
	}
}

func (s *system) Discover(flags DiscoveryFlag) error {
	// Everything the flags could ask for was discovered already. pkg/sysfs
	// re-reads and mutates in place; nothing calls it with anything new.
	return nil
}

func (s *system) SetCpusOnline(online bool, cpus idset.IDSet) (idset.IDSet, error) {
	if cpus == nil {
		cpus = idset.NewIDSet(s.CPUIDs()...)
	}

	desired := map[bool]uint64{false: 0, true: 1}[online]
	changed := idset.NewIDSet()

	for _, id := range cpus.SortedMembers() {
		if id <= 0 {
			// cpu0 cannot be taken offline, and pkg/sysfs skips it
			continue
		}
		c, ok := s.cpus[id]
		if !ok {
			continue
		}
		if c.hw.Online() == online {
			continue
		}

		name := path.Join(sysCPUDir, "cpu"+strconv.Itoa(id), "online")
		if err := s.write(name, desired); err != nil {
			return nil, err
		}
		changed.Add(id)
	}

	// The machine is immutable, so what it says about who is online is now out
	// of date. Nothing in the tree calls this; a caller which starts to would
	// have to rediscover.
	return changed, nil
}

func (s *system) SetCPUFrequencyLimits(min, max uint64, cpus idset.IDSet) error {
	if cpus == nil {
		cpus = idset.NewIDSet(s.CPUIDs()...)
	}

	for _, id := range cpus.SortedMembers() {
		c, ok := s.cpus[id]
		if !ok {
			continue
		}
		if err := c.SetFrequencyLimits(min, max); err != nil {
			return err
		}
	}

	return nil
}

func (s *system) PackageIDs() []idset.ID {
	ids := make([]idset.ID, 0, len(s.pkgs))
	for _, z := range s.hw.Zones(hardware.LevelPackage) {
		ids = append(ids, z.ID())
	}
	return ids
}

func (s *system) NodeIDs() []idset.ID {
	return s.hw.MemoryNodeIDs()
}

func (s *system) FilterNodes(ids []idset.ID, filters ...NodeFilter) idset.IDSet {
	out := idset.NewIDSet()
	for _, id := range ids {
		if s.FilterNode(id, filters...) {
			out.Add(id)
		}
	}
	return out
}

func (s *system) FilterNode(id idset.ID, filters ...NodeFilter) bool {
	n, ok := s.nodes[id]
	if !ok {
		return false
	}
	for _, filter := range filters {
		if !filter(n) {
			return false
		}
	}
	return true
}

func (s *system) ClosestNodes(id idset.ID, filters ...NodeFilter) ([]idset.IDSet, []int) {
	if _, ok := s.nodes[id]; !ok {
		return nil, nil
	}

	match := func(n *hardware.MemoryNode) bool {
		return s.FilterNode(n.ID(), filters...)
	}

	groups := hardware.ClosestMemoryNodes(s.hw, id, match)
	if len(groups) == 0 {
		return nil, nil
	}

	nodes := make([]idset.IDSet, 0, len(groups))
	distances := make([]int, 0, len(groups))
	for _, g := range groups {
		nodes = append(nodes, idset.NewIDSet(g.Nodes...))
		distances = append(distances, g.Distance)
	}

	return nodes, distances
}

func (s *system) CPUIDs() []idset.ID {
	return s.hw.CPUIDs()
}

func (s *system) PackageCount() int {
	return len(s.pkgs)
}

func (s *system) SocketCount() int {
	return len(s.pkgs)
}

func (s *system) CPUCount() int {
	return len(s.cpus)
}

func (s *system) NUMANodeCount() int {
	return max(len(s.nodes), 1)
}

func (s *system) MinThreadCount() int {
	min, _ := s.threadCounts()
	return min
}

func (s *system) MaxThreadCount() int {
	_, max := s.threadCounts()
	return max
}

func (s *system) CPUSet() cpuset.CPUSet {
	return cpuset.New(s.hw.CPUIDs()...)
}

// Package returns the package with the given id, or nil if there is none.
func (s *system) Package(id idset.ID) CPUPackage {
	if pkg, ok := s.pkgs[id]; ok {
		return pkg
	}
	return nil
}

// Node returns the NUMA node with the given id, or nil if there is none.
func (s *system) Node(id idset.ID) Node {
	if n, ok := s.nodes[id]; ok {
		return n
	}
	return nil
}

// NodeDistance returns the distance between two NUMA nodes, or -1 if either is
// unknown.
func (s *system) NodeDistance(from, to idset.ID) int {
	n, ok := s.nodes[from]
	if !ok {
		return -1
	}
	return n.DistanceFrom(to)
}

// CPU returns the CPU with the given id, or nil if there is none.
func (s *system) CPU(id idset.ID) CPU {
	if c, ok := s.cpus[id]; ok {
		return c
	}
	return nil
}

func (s *system) PossibleCPUs() cpuset.CPUSet {
	return toCPUSet(s.hw.PossibleCPUs())
}

func (s *system) PresentCPUs() cpuset.CPUSet {
	return toCPUSet(s.hw.PresentCPUs())
}

func (s *system) OnlineCPUs() cpuset.CPUSet {
	return toCPUSet(s.hw.OnlineCPUs())
}

func (s *system) IsolatedCPUs() cpuset.CPUSet {
	return toCPUSet(s.hw.IsolatedCPUs())
}

func (s *system) OfflineCPUs() cpuset.CPUSet {
	return toCPUSet(s.hw.OfflineCPUs())
}

func (s *system) CoreKindCPUs(kind CoreKind) cpuset.CPUSet {
	return toCPUSet(s.hw.CoreKindCPUs(hardware.CoreKind(kind)))
}

func (s *system) CoreKinds() []CoreKind {
	kinds := s.hw.CoreKinds()
	out := make([]CoreKind, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, CoreKind(kind))
	}
	return out
}

func (s *system) IDSetForCPUs(cpus cpuset.CPUSet, idForCPU func(CPU) idset.ID) idset.IDSet {
	ids := idset.NewIDSet()
	for _, id := range cpus.UnsortedList() {
		if c, ok := s.cpus[id]; ok {
			ids.Add(idForCPU(c))
		}
	}
	return ids
}

func (s *system) AllThreadsForCPUs(cpus cpuset.CPUSet) cpuset.CPUSet {
	all := cpuset.New()
	for _, id := range cpus.UnsortedList() {
		if c, ok := s.cpus[id]; ok {
			all = all.Union(c.ThreadCPUSet())
		}
	}
	return all
}

func (s *system) SingleThreadForCPUs(cpus cpuset.CPUSet) cpuset.CPUSet {
	var (
		result  = make([]int, 0, cpus.Size())
		handled = make(map[int]struct{}, cpus.Size())
	)

	for _, id := range cpus.List() {
		if _, ok := handled[id]; ok {
			continue
		}
		handled[id] = struct{}{}
		result = append(result, id)
		c, ok := s.cpus[id]
		if !ok {
			continue
		}
		for _, sibling := range c.ThreadCPUSet().UnsortedList() {
			handled[sibling] = struct{}{}
		}
	}

	return cpuset.New(result...)
}

func (s *system) AllCPUsSharingNthLevelCacheWithCPUs(n int, cpus cpuset.CPUSet) cpuset.CPUSet {
	all := cpuset.New()
	for _, id := range cpus.UnsortedList() {
		if all.Contains(id) {
			continue
		}
		if c, ok := s.cpus[id]; ok {
			all = all.Union(c.GetNthLevelCacheCPUSet(n))
		}
	}
	return all
}

func (s *system) Offlined() cpuset.CPUSet {
	return s.OfflineCPUs()
}

func (s *system) Isolated() cpuset.CPUSet {
	return s.IsolatedCPUs()
}

func (s *system) NodeHintToCPUs(nodes string) string {
	mset, err := cpuset.Parse(nodes)
	if err != nil {
		return ""
	}

	cset := cpuset.New()
	for _, id := range mset.List() {
		if n, ok := s.nodes[id]; ok {
			cset = cset.Union(n.CPUSet())
		}
	}

	return cset.Intersection(s.OnlineCPUs()).String()
}

func (s *system) Sst() *sst.Platform {
	return s.sst
}

// threadCounts returns the smallest and largest number of threads per core the
// machine has. As in pkg/sysfs a CPU with no thread siblings, i.e. an offline
// one, is not counted.
func (s *system) threadCounts() (int, int) {
	var min, max int

	for _, id := range s.hw.CPUIDs() {
		n := s.hw.CPU(id).Threads().Size()
		if n == 0 {
			continue
		}
		if min == 0 || n < min {
			min = n
		}
		if max == 0 || n > max {
			max = n
		}
	}

	return min, max
}

// sysCPUDir is where the per-CPU attributes the write paths touch live, relative
// to the host root.
const sysCPUDir = "sys/devices/system/cpu"

// system must satisfy System.
var _ System = (*system)(nil)
