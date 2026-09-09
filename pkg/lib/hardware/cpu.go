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
	"strconv"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// CPU is one CPU as the kernel counts them, i.e. a hardware thread. It is a
// handle into the [Machine] which produced it: never nil, and safe to use even
// when it refers to no CPU, in which case it reports Valid() == false.
type CPU struct {
	m     *Machine
	dir   string // where it was read from, for the write paths
	id    ID
	valid bool

	online   bool
	isolated bool
	kind     CoreKind

	pkg     ID
	die     ID
	cluster ID
	node    ID
	core    ID

	threads *libcpu.CpuMask
	freq    Freq
	caches  []*Cache

	// zone at each level, indexed by Level, filled in when the zones are built
	zones [numLevels]*Zone
}

// invalidCPU is what a lookup for a CPU the machine does not have returns. Its
// methods answer with zero values rather than panicking.
var invalidCPU = &CPU{
	pkg: unknownID, die: unknownID, cluster: unknownID,
	node: unknownID, core: unknownID,
}

// Valid reports whether this handle refers to a CPU the machine has.
func (c *CPU) Valid() bool {
	return c.valid
}

// ID returns the id of this CPU.
func (c *CPU) ID() ID {
	return c.id
}

// Online reports whether this CPU is online. Only an online CPU has a known
// place in the topology: the zones and ids of an offline CPU are not known and
// read as invalid.
func (c *CPU) Online() bool {
	return c.online
}

// Isolated reports whether the kernel was told to isolate this CPU.
func (c *CPU) Isolated() bool {
	return c.isolated
}

// Kind returns whether this is a performance or an efficiency core. On a
// machine whose cores are all alike it is [PerformanceCore].
func (c *CPU) Kind() CoreKind {
	return c.kind
}

// Zone returns the zone at the given level which contains this CPU, or an
// invalid zone if the machine has no zones at that level.
func (c *CPU) Zone(level Level) *Zone {
	if level < 0 || int(level) >= numLevels || c.zones[level] == nil {
		return invalidZone
	}
	return c.zones[level]
}

// PackageID returns the id of the package this CPU is in, or -1 if unknown. It
// is shorthand for c.Zone(LevelPackage).ID().
func (c *CPU) PackageID() ID {
	return c.pkg
}

// DieID returns the id of the die this CPU is in, or -1 if unknown.
func (c *CPU) DieID() ID {
	return c.die
}

// ClusterID returns the id of the cluster this CPU is in, or -1 if unknown.
func (c *CPU) ClusterID() ID {
	return c.cluster
}

// NodeID returns the id of the NUMA node this CPU is in, or -1 if unknown.
func (c *CPU) NodeID() ID {
	return c.node
}

// CoreID returns the id of the core this CPU is a thread of, or -1 if unknown.
func (c *CPU) CoreID() ID {
	return c.core
}

// Threads returns all the CPUs of this CPU's core, including this one. On a
// machine without simultaneous multithreading that is this CPU alone. The set
// is sealed.
func (c *CPU) Threads() *libcpu.CpuMask {
	if c.threads == nil {
		return emptyCPUs
	}
	return c.threads
}

// MemoryNode returns the NUMA node this CPU belongs to.
func (c *CPU) MemoryNode() *MemoryNode {
	if c.m == nil {
		return invalidMemoryNode
	}
	return c.m.MemoryNode(c.node)
}

// Caches returns the caches this CPU uses, lowest level first.
func (c *CPU) Caches() []*Cache {
	return c.caches
}

// Cache returns this CPU's cache at the given level, or an invalid cache if it
// has none. Where a CPU has both a data and an instruction cache at a level,
// this returns the data one; use [CPU.Caches] to see all of them.
func (c *CPU) Cache(level int) *Cache {
	for _, cache := range c.caches {
		if cache.level == level {
			return cache
		}
		if cache.level > level {
			break
		}
	}
	return invalidCache
}

// coordinates is where this CPU sits, as one value.
func (c *CPU) coordinates() Coordinates {
	return Coordinates{
		CPU: c.id, Package: c.pkg, Die: c.die, Cluster: c.cluster,
		MemoryNode: c.node, Core: c.core, Kind: c.kind,
	}
}

// Freq returns what is known about this CPU's clock frequency. Absent cpufreq
// support the zero value is returned.
func (c *CPU) Freq() Freq {
	return c.freq
}

// String returns "cpu#<id>".
func (c *CPU) String() string {
	if !c.valid {
		return "cpu#?"
	}
	return "cpu#" + strconv.Itoa(c.id)
}

// CoreKind classifies a core by the role the hardware intends it for.
type CoreKind int

const (
	// PerformanceCore is a P-core, and the kind of every core on a machine
	// which does not distinguish.
	PerformanceCore CoreKind = iota
	// EfficientCore is an E-core.
	EfficientCore
)

// String returns "P-core" or "E-core".
func (k CoreKind) String() string {
	switch k {
	case PerformanceCore:
		return "P-core"
	case EfficientCore:
		return "E-core"
	}
	return "unknown core kind"
}

// Freq is what is known about a CPU's clock frequency, in kHz. A zero field
// means the machine did not report that value.
type Freq struct {
	// Base is the frequency the CPU is nominally clocked at.
	Base uint64
	// Min is the lowest frequency it can be scaled to.
	Min uint64
	// Max is the highest, including any turbo range.
	Max uint64
	// EPP is the energy performance preference the governor is set to.
	EPP EPP
}

// EPP is an energy performance preference: how the governor is told to trade
// power against performance. Lower values prefer performance.
type EPP int

const (
	// EPPPerformance prefers performance unconditionally.
	EPPPerformance EPP = iota
	// EPPBalancePerformance prefers performance but will save power.
	EPPBalancePerformance
	// EPPBalancePower prefers saving power but will perform.
	EPPBalancePower
	// EPPPower prefers saving power unconditionally.
	EPPPower
	// EPPUnknown is reported when the machine does not say.
	EPPUnknown
)

// String returns the kernel's name for the preference, or "" for
// [EPPUnknown].
func (e EPP) String() string {
	if e < 0 || int(e) >= len(eppNames) {
		return ""
	}
	return eppNames[e]
}

// eppNames are the kernel's names for the preferences, indexed by [EPP].
// EPPUnknown is last and has no name.
var eppNames = [...]string{
	EPPPerformance:        "performance",
	EPPBalancePerformance: "balance_performance",
	EPPBalancePower:       "balance_power",
	EPPPower:              "power",
	EPPUnknown:            "",
}

// ParseEPP returns the preference the kernel calls s, or [EPPUnknown].
func ParseEPP(s string) EPP {
	for epp, name := range eppNames {
		if name != "" && name == s {
			return EPP(epp)
		}
	}
	return EPPUnknown
}
