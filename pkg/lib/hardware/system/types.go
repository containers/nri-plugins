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

	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// The enumerations below repeat pkg/sysfs, values included. Anything which
// persists one of these numbers, or compares it against a literal, keeps
// working.

// DiscoveryFlag controls what hardware details to discover.
//
// The values are accepted and ignored, as they are in pkg/sysfs.
type DiscoveryFlag uint

const (
	// DiscoverCPUTopology requests discovering CPU topology details.
	DiscoverCPUTopology DiscoveryFlag = 1 << iota
	// DiscoverMemTopology requests discovering memory topology details.
	DiscoverMemTopology
	// DiscoverCache requests discovering CPU cache details.
	DiscoverCache
	// DiscoverSst requests discovering details of Intel Speed Select Technology
	DiscoverSst
	// DiscoverNone is the zero value for discovery flags.
	DiscoverNone DiscoveryFlag = 0
	// DiscoverAll requests full supported discovery.
	DiscoverAll DiscoveryFlag = 0xffffffff
	// DiscoverDefault is the default set of discovery flags.
	DiscoverDefault DiscoveryFlag = DiscoverAll
)

// MemoryType is an enum for the Node memory
type MemoryType int

const (
	// MemoryTypeDRAM means that the node has regular DRAM-type memory
	MemoryTypeDRAM MemoryType = iota
	// MemoryTypePMEM means that the node has persistent memory
	MemoryTypePMEM
	// MemoryTypeHBM means that the node has high bandwidth memory
	MemoryTypeHBM
)

// String returns the name of the memory type, or a %!(BAD-MemoryType:n) marker
// for an unknown one, as pkg/sysfs does.
func (t MemoryType) String() string {
	switch t {
	case MemoryTypeDRAM:
		return "DRAM"
	case MemoryTypePMEM:
		return "PMEM"
	case MemoryTypeHBM:
		return "HBM"
	}
	return fmt.Sprintf("%%(BAD-MemoryType:%d)", t)
}

// CacheType specifies a cache type.
type CacheType int

const (
	// DataCache is a data only cache
	DataCache CacheType = iota
	// InstructionCache is an instruction only cache.
	InstructionCache
	// UnifiedCache is a unified data and instruction cache.
	UnifiedCache
	numCacheTypes
	// NumCacheTypes is the number of cache types.
	NumCacheTypes = int(numCacheTypes)
)

// String returns "Data", "Instruction", "Unified", or "" for an unknown type.
func (t CacheType) String() string {
	switch t {
	case DataCache:
		return "Data"
	case InstructionCache:
		return "Instruction"
	case UnifiedCache:
		return "Unified"
	}
	return ""
}

// EPP represents the value of a CPU energy performance profile
type EPP int

const (
	EPPPerformance EPP = iota
	EPPBalancePerformance
	EPPBalancePower
	EPPPower
	EPPUnknown
)

// String returns EPP value as string
func (e EPP) String() string {
	if int(e) < len(eppStrings) {
		return eppStrings[e]
	}
	return ""
}

// EPPFromString converts string to EPP value
func EPPFromString(s string) EPP {
	if v, ok := eppValues[s]; ok {
		return v
	}
	return EPPUnknown
}

// CoreKind represents high-level classification of CPU cores, currently P- and
// E-cores
type CoreKind int

const (
	PerformanceCore CoreKind = iota
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
	return ""
}

// CPUFreq is a CPU frequency scaling range
type CPUFreq struct {
	Base uint64 // base frequency
	Min  uint64 // minimum frequency (kHz)
	Max  uint64 // maximum frequency (kHz)
}

// MemInfo contains data read from a NUMA node meminfo file.
type MemInfo struct {
	MemTotal uint64
	MemFree  uint64
	MemUsed  uint64
}

//
// Node filters
//

// NodeFilter is a function for filtering nodes. A node passes a filter if the
// filter returns true for the node.
type NodeFilter func(n Node) bool

// NodeOfType filters nodes with the given memory type.
func NodeOfType(t MemoryType) NodeFilter {
	return func(n Node) bool {
		return n.GetMemoryType() == t
	}
}

var (
	// NodeOfDRAMType filters nodes with DRAM memory.
	NodeOfDRAMType = NodeOfType(MemoryTypeDRAM)
	// NodeOfPMEMType filters nodes with PMEM memory.
	NodeOfPMEMType = NodeOfType(MemoryTypePMEM)
	// NodeOfHBMType filters nodes with HBM memory.
	NodeOfHBMType = NodeOfType(MemoryTypeHBM)
	// NodeHasMemory filters nodes with some attached memory.
	//
	// As in pkg/sysfs a node whose meminfo cannot be read counts as having
	// memory, so that an unreadable node is not silently excluded from
	// everything.
	NodeHasMemory = func(n Node) bool {
		mi, _ := n.MemoryInfo()
		return mi == nil || mi.MemTotal > 0
	}
	// NodeHasNoMemory filters nodes with no any attached memory.
	NodeHasNoMemory = func(n Node) bool {
		mi, _ := n.MemoryInfo()
		return mi != nil && mi.MemTotal == 0
	}
	// NodeHasLocalCPUs filters nodes which has close CPUs.
	NodeHasLocalCPUs = func(n Node) bool {
		return !n.CPUSet().IsEmpty()
	}
	// NodeHasNoLocalCPUs filters nodes which don't have close CPUs.
	NodeHasNoLocalCPUs = func(n Node) bool {
		return n.CPUSet().IsEmpty()
	}
)

// eppStrings are the kernel's names for the energy performance preferences,
// indexed by EPP. Built this way to catch a change in the enum.
var eppStrings = func() [EPPUnknown]string {
	var e [EPPUnknown]string
	e[EPPPerformance] = "performance"
	e[EPPBalancePerformance] = "balance_performance"
	e[EPPBalancePower] = "balance_power"
	e[EPPPower] = "power"
	return e
}()

// eppValues is eppStrings the other way round.
var eppValues = func() map[string]EPP {
	m := make(map[string]EPP, len(eppStrings))
	for i, v := range eppStrings {
		m[v] = EPP(i)
	}
	return m
}()

// NodeFilterAnd returns a filter that is the logical AND of the given filters.
func NodeFilterAnd(filters ...NodeFilter) NodeFilter {
	return func(n Node) bool {
		for _, f := range filters {
			if !f(n) {
				return false
			}
		}
		return true
	}
}

// NodeFilterOr returns a filter that is the logical OR of the given filters.
func NodeFilterOr(filters ...NodeFilter) NodeFilter {
	return func(n Node) bool {
		for _, f := range filters {
			if f(n) {
				return true
			}
		}
		return false
	}
}

// NodeFilterNot returns a filter that is the logical NOT of the given filter.
func NodeFilterNot(f NodeFilter) NodeFilter {
	return func(n Node) bool {
		return !f(n)
	}
}

//
// Utilities
//
// These have nothing to do with topology, they just live in pkg/sysfs. They are
// repeated here so that a consumer's import swap is complete, and they should
// end up somewhere under pkg/utils rather than following the topology into
// hardware.
//

// PickEntryFn picks a given input line apart into an entry of key and value.
type PickEntryFn func(string) (string, string, error)

// ParseFileEntries parses a sysfs files for the given entries.
func ParseFileEntries(path string, values map[string]any, pickFn PickEntryFn) error {
	return sysfs.ParseFileEntries(path, values, sysfs.PickEntryFn(pickFn))
}

// IDSetFromCPUSet returns an id set corresponding to a cpuset.CPUSet.
func IDSetFromCPUSet(cset cpuset.CPUSet) idset.IDSet {
	return idset.NewIDSetFromIntSlice(cset.List()...)
}

// CPUSetFromIDSet returns a cpuset.CPUSet corresponding to an id set.
func CPUSetFromIDSet(s idset.IDSet) cpuset.CPUSet {
	return cpuset.New(s.Members()...)
}

// GetMemoryCapacity parses memory capacity from /proc/meminfo (mimicking
// cAdvisor).
func GetMemoryCapacity() int64 {
	return sysfs.GetMemoryCapacity()
}
