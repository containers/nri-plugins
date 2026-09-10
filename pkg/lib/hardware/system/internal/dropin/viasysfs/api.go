// Package viasysfs references every exported pkg/sysfs symbol, against
// pkg/sysfs.
//
// It exists so that the compiler enforces the drop-in property. This file and
// its sibling in ../viasystem/api.go differ only in which package they
// import; TestSourcesAreIdentical in the parent directory checks that, and
// building both checks that the two packages really are interchangeable.
//
// Nothing imports this. It goes when pkg/sysfs goes.
package viasysfs

import (
	sysfs "github.com/containers/nri-plugins/pkg/sysfs" //nolint:staticcheck // deprecated on purpose: this is what it is compared against

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"github.com/intel/goresctrl/pkg/sst"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// interfaces, in the shapes consumers actually use them
var (
	_ sysfs.System      = nil
	_ sysfs.CPU         = nil
	_ sysfs.CPUPackage  = nil
	_ sysfs.Node        = nil
	_ *sysfs.Cache      = nil
	_ sysfs.NodeFilter  = nil
	_ sysfs.PickEntryFn = nil
)

// enums and structs
var (
	_ sysfs.DiscoveryFlag = sysfs.DiscoverCPUTopology | sysfs.DiscoverMemTopology |
		sysfs.DiscoverCache | sysfs.DiscoverSst | sysfs.DiscoverNone |
		sysfs.DiscoverAll | sysfs.DiscoverDefault
	_ sysfs.MemoryType = sysfs.MemoryTypeDRAM
	_ sysfs.MemoryType = sysfs.MemoryTypePMEM
	_ sysfs.MemoryType = sysfs.MemoryTypeHBM
	_ sysfs.CacheType  = sysfs.DataCache
	_ sysfs.CacheType  = sysfs.InstructionCache
	_ sysfs.CacheType  = sysfs.UnifiedCache
	_ int              = sysfs.NumCacheTypes
	_ sysfs.EPP        = sysfs.EPPPerformance
	_ sysfs.EPP        = sysfs.EPPBalancePerformance
	_ sysfs.EPP        = sysfs.EPPBalancePower
	_ sysfs.EPP        = sysfs.EPPPower
	_ sysfs.EPP        = sysfs.EPPUnknown
	_ sysfs.CoreKind   = sysfs.PerformanceCore
	_ sysfs.CoreKind   = sysfs.EfficientCore
	_ sysfs.CPUFreq    = sysfs.CPUFreq{Base: 1, Min: 2, Max: 3}
	_ sysfs.MemInfo    = sysfs.MemInfo{MemTotal: 1, MemFree: 2, MemUsed: 3}
)

// predefined filters, used both bare and in a []NodeFilter as pools.go does
var _ = []sysfs.NodeFilter{
	sysfs.NodeOfDRAMType, sysfs.NodeOfPMEMType, sysfs.NodeOfHBMType,
	sysfs.NodeHasMemory, sysfs.NodeHasNoMemory,
	sysfs.NodeHasLocalCPUs, sysfs.NodeHasNoLocalCPUs,
}

// stringers and parsers
var (
	_ = sysfs.MemoryTypeDRAM.String()
	_ = sysfs.DataCache.String()
	_ = sysfs.EPPPerformance.String()
	_ = sysfs.PerformanceCore.String()
	_ = sysfs.EPPFromString("performance")
)

// functions
func useFuncs(sys sysfs.System, c sysfs.CPU, cset cpuset.CPUSet, ids idset.IDSet) { //nolint:unused // compiled, not called
	sysfs.SetSysRoot("/host")
	_, _ = sysfs.DiscoverSystem()
	_, _ = sysfs.DiscoverSystem(sysfs.DiscoverCPUTopology | sysfs.DiscoverCache)
	_, _ = sysfs.DiscoverSystemAt("/sys")
	_, _ = sysfs.DiscoverSystemAt("/sys", sysfs.DiscoverAll)
	_ = sysfs.IDSetFromCPUSet(cset)
	_ = sysfs.CPUSetFromIDSet(ids)
	_ = sysfs.GetMemoryCapacity()
	_ = sysfs.ParseFileEntries("/proc/meminfo", map[string]any{}, nil)
	_ = sysfs.NodeOfType(sysfs.MemoryTypeDRAM)
	_ = sysfs.NodeFilterAnd(sysfs.NodeHasMemory, sysfs.NodeHasLocalCPUs)
	_ = sysfs.NodeFilterOr(sysfs.NodeHasMemory)
	_ = sysfs.NodeFilterNot(sysfs.NodeHasMemory)

	// every System method, called the way consumers call them
	_ = sys.Discover(sysfs.DiscoverAll)
	_, _ = sys.SetCpusOnline(true, ids)
	_ = sys.SetCPUFrequencyLimits(1, 2, ids)
	_ = sys.PackageIDs()
	_ = sys.NodeIDs()
	_ = sys.FilterNodes(sys.NodeIDs(), sysfs.NodeHasMemory)
	_ = sys.FilterNode(0)
	_, _ = sys.ClosestNodes(0, sysfs.NodeOfDRAMType, sysfs.NodeHasLocalCPUs)
	_ = sys.CPUIDs()
	_, _, _, _ = sys.PackageCount(), sys.SocketCount(), sys.CPUCount(), sys.NUMANodeCount()
	_, _ = sys.MinThreadCount(), sys.MaxThreadCount()
	_ = sys.CPUSet()
	_ = sys.NodeDistance(0, 1)
	_, _, _ = sys.PossibleCPUs(), sys.PresentCPUs(), sys.OnlineCPUs()
	_, _ = sys.IsolatedCPUs(), sys.OfflineCPUs()
	_ = sys.CoreKindCPUs(sysfs.PerformanceCore)
	_ = sys.CoreKinds()
	_ = sys.IDSetForCPUs(cset, func(c sysfs.CPU) idset.ID { return c.PackageID() })
	_ = sys.AllThreadsForCPUs(cset)
	_ = sys.SingleThreadForCPUs(cset)
	_ = sys.AllCPUsSharingNthLevelCacheWithCPUs(2, cset)
	_, _ = sys.Offlined(), sys.Isolated()
	_ = sys.NodeHintToCPUs("0-1")
	var _ *sst.Platform = sys.Sst() //nolint:staticcheck // the type is the check

	usePackage(sys.Package(0))
	useNode(sys.Node(0))
	useCPU(sys.CPU(0))
	useCPU(c)
}

func usePackage(p sysfs.CPUPackage) { //nolint:unused // compiled, not called
	_ = p.ID()
	_ = p.CPUSet()
	_ = p.DieIDs()
	_ = p.NodeIDs()
	_ = p.DieNodeIDs(0)
	_ = p.DieCPUSet(0)
	_ = p.DieClusterIDs(0)
	_ = p.DieClusterCPUSet(0, 0)
	_ = p.LogicalDieClusterIDs(0)
	_ = p.LogicalDieClusterCPUSet(0, 0)
	_ = p.L3CacheIDs()
	_ = p.L3CacheCPUSet(0)
	var _ *sst.PackageStatus = p.SstInfo() //nolint:staticcheck // the type is the check
}

func useNode(n sysfs.Node) { //nolint:unused // compiled, not called
	_ = n.ID()
	_ = n.PackageID()
	_ = n.DieID()
	_ = n.CPUSet()
	_ = n.Distance()
	_ = n.DistanceFrom(0)
	_, _ = n.ClosestNodes()
	_, _ = n.MemoryInfo()
	_ = n.GetMemoryType()
	_ = n.HasNormalMemory()
}

func useCPU(c sysfs.CPU) { //nolint:unused // compiled, not called
	_ = c.ID()
	_ = c.PackageID()
	_ = c.DieID()
	_ = c.ClusterID()
	_ = c.NodeID()
	_ = c.CoreID()
	_ = c.ThreadCPUSet()
	_ = c.BaseFrequency()
	_ = c.FrequencyRange()
	_ = c.EPP()
	_ = c.Online()
	_ = c.Isolated()
	_ = c.SetFrequencyLimits(1, 2)
	_ = c.SstClos()
	_ = c.CacheCount()
	_ = c.GetCaches()
	_ = c.GetCachesByLevel(2)
	_ = c.GetCacheByIndex(0)
	_ = c.GetNthLevelCacheCPUSet(2)
	_ = c.GetLastLevelCaches()
	_ = c.GetLastLevelCacheCPUSet()
	_ = c.CoreKind()

	// balloons keys a map by *Cache, so this must compile and be comparable
	seen := map[*sysfs.Cache]struct{}{}
	for _, cc := range c.GetCaches() {
		seen[cc] = struct{}{}
		_, _, _, _ = cc.ID(), cc.Level(), cc.Type(), cc.Size()
		_ = cc.SharedCPUSet()
	}
	_ = seen
}
