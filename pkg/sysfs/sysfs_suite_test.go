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

package sysfs_test

import (
	"fmt"
	"os"
	"path"
	"testing"

	"github.com/containers/nri-plugins/pkg/sysfs"
	idset "github.com/intel/goresctrl/pkg/utils"
	"github.com/stretchr/testify/require"
)

type (
	ID        = idset.ID
	System    = sysfs.System
	CacheType = sysfs.CacheType
	CoreKind  = sysfs.CoreKind
)

var (
	SetSysRoot     = sysfs.SetSysRoot
	DiscoverSystem = sysfs.DiscoverSystem
	sampleSysfs    = map[string]sysfs.System{}
)

const (
	Data        = sysfs.DataCache
	Instruction = sysfs.InstructionCache
	Unified     = sysfs.UnifiedCache
	K           = uint64(1024)

	PerformanceCore = sysfs.PerformanceCore
	EfficientCore   = sysfs.EfficientCore

	evenCPUs = "0,2,4,6,8,10,12,14,16,18,20,22,24,26,28,30,32,34,36,38,40,42,44,46,48,50,52,54,56,58,60,62,64,66,68,70,72,74,76,78,80,82,84,86,88,90,92,94,96,98,100,102,104,106,108,110"
	oddCPUs  = "1,3,5,7,9,11,13,15,17,19,21,23,25,27,29,31,33,35,37,39,41,43,45,47,49,51,53,55,57,59,61,63,65,67,69,71,73,75,77,79,81,83,85,87,89,91,93,95,97,99,101,103,105,107,109,111"
)

func TestMain(m *testing.M) {
	cwd, _ := os.Getwd()

	for _, name := range []string{"sample1", "sample2"} {
		SetSysRoot(path.Join(cwd, "testdata", name))
		sys, err := DiscoverSystem()
		if err != nil || sys == nil {
			fmt.Fprintf(os.Stderr, "failed to discover %s: %v\n", name, err)
			os.Exit(1)
		}
		sampleSysfs[name] = sys
	}

	os.Exit(m.Run())
}

func TestCacheDiscovery(t *testing.T) {
	tcases := []struct {
		name   string
		sample string
		cpu    ID
		idx    int
		level  int
		id     int
		kind   CacheType
		size   uint64
		cpus   string
	}{
		// sample sysfs 1, pick and test a few CPU cores
		{"CPU #0, cache #0", "sample1", 0, 0, 1, 0, Data, 48 * K, "0-1"},
		{"CPU #0, cache #1", "sample1", 0, 1, 1, 0, Instruction, 32 * K, "0-1"},
		{"CPU #0, cache #2", "sample1", 0, 2, 2, 0, Unified, 1280 * K, "0-1"},
		{"CPU #0, cache #3", "sample1", 0, 3, 3, 0, Unified, 18432 * K, "0-15"},
		{"CPU #1, cache #0", "sample1", 1, 0, 1, 0, Data, 48 * K, "0-1"},
		{"CPU #1, cache #1", "sample1", 1, 1, 1, 0, Instruction, 32 * K, "0-1"},
		{"CPU #1, cache #2", "sample1", 1, 2, 2, 0, Unified, 1280 * K, "0-1"},
		{"CPU #1, cache #3", "sample1", 1, 3, 3, 0, Unified, 18432 * K, "0-15"},

		{"CPU #8, cache #0", "sample1", 8, 0, 1, 16, Data, 32 * K, "8"},
		{"CPU #8, cache #1", "sample1", 8, 1, 1, 16, Instruction, 64 * K, "8"},
		{"CPU #8, cache #2", "sample1", 8, 2, 2, 4, Unified, 2048 * K, "8-11"},
		{"CPU #8, cache #3", "sample1", 8, 3, 3, 0, Unified, 18432 * K, "0-15"},
		{"CPU #11, cache #0", "sample1", 11, 0, 1, 19, Data, 32 * K, "11"},
		{"CPU #11, cache #1", "sample1", 11, 1, 1, 19, Instruction, 64 * K, "11"},
		{"CPU #11, cache #2", "sample1", 11, 2, 2, 4, Unified, 2048 * K, "8-11"},
		{"CPU #11, cache #3", "sample1", 11, 3, 3, 0, Unified, 18432 * K, "0-15"},

		{"CPU #12, cache #0", "sample1", 12, 0, 1, 20, Data, 32 * K, "12"},
		{"CPU #12, cache #1", "sample1", 12, 1, 1, 20, Instruction, 64 * K, "12"},
		{"CPU #12, cache #2", "sample1", 12, 2, 2, 5, Unified, 2048 * K, "12-15"},
		{"CPU #12, cache #3", "sample1", 12, 3, 3, 0, Unified, 18432 * K, "0-15"},
		{"CPU #15, cache #0", "sample1", 15, 0, 1, 23, Data, 32 * K, "15"},
		{"CPU #15, cache #1", "sample1", 15, 1, 1, 23, Instruction, 64 * K, "15"},
		{"CPU #15, cache #2", "sample1", 15, 2, 2, 5, Unified, 2048 * K, "12-15"},
		{"CPU #15, cache #3", "sample1", 15, 3, 3, 0, Unified, 18432 * K, "0-15"},

		// sample sysfs 2, pick and test a few CPU cores
		{"CPU #0, cache #0", "sample2", 0, 0, 1, 0, Data, 32 * K, "0,56"},
		{"CPU #0, cache #1", "sample2", 0, 1, 1, 0, Instruction, 32 * K, "0,56"},
		{"CPU #0, cache #2", "sample2", 0, 2, 2, 0, Unified, 1024 * K, "0,56"},
		{"CPU #0, cache #3", "sample2", 0, 3, 3, 0, Unified, 39424 * K, evenCPUs},
		{"CPU #56, cache #0", "sample2", 56, 0, 1, 0, Data, 32 * K, "0,56"},
		{"CPU #56, cache #1", "sample2", 56, 1, 1, 0, Instruction, 32 * K, "0,56"},
		{"CPU #56, cache #2", "sample2", 56, 2, 2, 0, Unified, 1024 * K, "0,56"},
		{"CPU #56, cache #3", "sample2", 56, 3, 3, 0, Unified, 39424 * K, evenCPUs},

		{"CPU #1, cache #0", "sample2", 1, 0, 1, 32, Data, 32 * K, "1,57"},
		{"CPU #1, cache #1", "sample2", 1, 1, 1, 32, Instruction, 32 * K, "1,57"},
		{"CPU #1, cache #2", "sample2", 1, 2, 2, 32, Unified, 1024 * K, "1,57"},
		{"CPU #1, cache #3", "sample2", 1, 3, 3, 1, Unified, 39424 * K, oddCPUs},
		{"CPU #57, cache #0", "sample2", 57, 0, 1, 32, Data, 32 * K, "1,57"},
		{"CPU #57, cache #1", "sample2", 57, 1, 1, 32, Instruction, 32 * K, "1,57"},
		{"CPU #57, cache #2", "sample2", 57, 2, 2, 32, Unified, 1024 * K, "1,57"},
		{"CPU #57, cache #3", "sample2", 57, 3, 3, 1, Unified, 39424 * K, oddCPUs},
	}

	for _, tc := range tcases {
		t.Run(tc.sample+"/"+tc.name, func(t *testing.T) {
			sys := sampleSysfs[tc.sample]
			require.NotNil(t, sys)
			c := sys.CPU(tc.cpu)
			require.NotNil(t, c)
			cch := c.GetCacheByIndex(tc.idx)
			require.NotNil(t, cch)
			require.Equal(t, tc.id, cch.ID())
			require.Equal(t, tc.level, cch.Level())
			require.Equal(t, tc.kind, cch.Type())
			require.Equal(t, tc.size, cch.Size())
			require.Equal(t, tc.cpus, cch.SharedCPUSet().String())
		})
	}
}

func TestLLCCPUSet(t *testing.T) {
	tcases := []struct {
		name   string
		sample string
		cpu    ID
		cpus   string
	}{
		{"CPU #0", "sample1", 0, "0-15"},
		{"CPU #0", "sample2", 0, evenCPUs},
		{"CPU #1", "sample2", 1, oddCPUs},
	}

	for _, tc := range tcases {
		t.Run(tc.sample+"/"+tc.name, func(t *testing.T) {
			sys := sampleSysfs[tc.sample]
			require.NotNil(t, sys)
			c := sys.CPU(tc.cpu)
			require.NotNil(t, c)
			require.Equal(t, tc.cpus, c.GetLastLevelCacheCPUSet().String())
		})
	}
}

func TestUnfilteredClusterDetection(t *testing.T) {
	sys := sampleSysfs["sample1"]
	require.NotNil(t, sys)

	result := sys.Package(0).DieClusterIDs(0)
	require.Equal(t, []idset.ID{0, 8, 16, 24, 32, 40}, result)
}

func TestUnfilteredClusterCPUSet(t *testing.T) {
	tcases := []struct {
		cluster idset.ID
		cpus    string
	}{
		{0, "0-1"},
		{8, "2-3"},
		{16, "4-5"},
		{24, "6-7"},
		{32, "8-11"},
		{40, "12-15"},
	}

	sys := sampleSysfs["sample1"]
	require.NotNil(t, sys)

	for _, tc := range tcases {
		t.Run(fmt.Sprintf("die #0, cluster #%d", tc.cluster), func(t *testing.T) {
			result := sys.Package(0).DieClusterCPUSet(0, tc.cluster)
			require.Equal(t, tc.cpus, result.String())
		})
	}
}

func TestLogicalClusterDetection(t *testing.T) {
	sys := sampleSysfs["sample1"]
	require.NotNil(t, sys)

	result := sys.Package(0).LogicalDieClusterIDs(0)
	require.Equal(t, []idset.ID{0, 32, 40}, result)
}

func TestLogicalClusterCPUSet(t *testing.T) {
	tcases := []struct {
		cluster idset.ID
		cpus    string
	}{
		{0, "0-7"},
		{32, "8-11"},
		{40, "12-15"},
	}

	sys := sampleSysfs["sample1"]
	require.NotNil(t, sys)

	for _, tc := range tcases {
		t.Run(fmt.Sprintf("die #0, cluster #%d", tc.cluster), func(t *testing.T) {
			result := sys.Package(0).LogicalDieClusterCPUSet(0, tc.cluster)
			require.Equal(t, tc.cpus, result.String())
		})
	}
}

func TestCoreKindCPUs(t *testing.T) {
	tcases := []struct {
		name string
		kind CoreKind
		cpus string
	}{
		{"P-Cores", PerformanceCore, "0-7"},
		{"E-Cores", EfficientCore, "8-15"},
	}

	sys := sampleSysfs["sample1"]
	require.NotNil(t, sys)

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			cset := sys.CoreKindCPUs(tc.kind)
			require.Equal(t, tc.cpus, cset.String())
		})
	}
}

func TestClosestNodes(t *testing.T) {
	tcases := []struct {
		node      ID
		closest   []idset.IDSet
		distances []int
	}{
		{0, []idset.IDSet{
			idset.NewIDSet(2),
			idset.NewIDSet(4),
			idset.NewIDSet(1, 3),
			idset.NewIDSet(5, 6, 7),
		}, []int{11, 17, 21, 28}},
		{1, []idset.IDSet{
			idset.NewIDSet(3),
			idset.NewIDSet(6),
			idset.NewIDSet(0, 2),
			idset.NewIDSet(4, 5, 7),
		}, []int{11, 17, 21, 28}},
		{2, []idset.IDSet{
			idset.NewIDSet(0),
			idset.NewIDSet(5),
			idset.NewIDSet(1, 3),
			idset.NewIDSet(4, 6, 7),
		}, []int{11, 17, 21, 28}},
		{3, []idset.IDSet{
			idset.NewIDSet(1),
			idset.NewIDSet(7),
			idset.NewIDSet(0, 2),
			idset.NewIDSet(4, 5, 6),
		}, []int{11, 17, 21, 28}},
		{4, []idset.IDSet{
			idset.NewIDSet(0),
			idset.NewIDSet(1, 2, 3, 5, 6, 7),
		}, []int{17, 28}},
		{5, []idset.IDSet{
			idset.NewIDSet(2),
			idset.NewIDSet(0, 1, 3, 4, 6, 7),
		}, []int{17, 28}},
		{6, []idset.IDSet{
			idset.NewIDSet(1),
			idset.NewIDSet(0, 2, 3, 4, 5, 7),
		}, []int{17, 28}},
		{7, []idset.IDSet{
			idset.NewIDSet(3),
			idset.NewIDSet(0, 1, 2, 4, 5, 6),
		}, []int{17, 28}},
	}

	sys := sampleSysfs["sample2"]
	require.NotNil(t, sys)

	for _, tc := range tcases {
		t.Run(fmt.Sprintf("node #%d", tc.node), func(t *testing.T) {
			nodes, dists := sys.Node(tc.node).ClosestNodes()
			require.Equal(t, tc.closest, nodes)
			require.Equal(t, tc.distances, dists)
		})
	}
}

var nodeWithEvenID = func(n sysfs.Node) bool { return (n.ID() & 0x1) == 0 }

func TestFilterNodes(t *testing.T) {
	tcases := []struct {
		name    string
		filters []sysfs.NodeFilter
		result  idset.IDSet
	}{
		{
			"nodes with even IDs",
			[]sysfs.NodeFilter{nodeWithEvenID},
			idset.NewIDSet(0, 2, 4, 6),
		},
		{
			"nodes with odd IDs",
			[]sysfs.NodeFilter{sysfs.NodeFilterNot(nodeWithEvenID)},
			idset.NewIDSet(1, 3, 5, 7),
		},
		{
			"nodes with CPU locality",
			[]sysfs.NodeFilter{sysfs.NodeHasLocalCPUs},
			idset.NewIDSet(0, 1, 2, 3),
		},
		{
			"nodes without CPU locality",
			[]sysfs.NodeFilter{sysfs.NodeFilterNot(sysfs.NodeHasLocalCPUs)},
			idset.NewIDSet(4, 5, 6, 7),
		},
		{
			"DRAM nodes",
			[]sysfs.NodeFilter{sysfs.NodeOfDRAMType},
			idset.NewIDSet(0, 1, 2, 3),
		},
		{
			"PMEM nodes",
			[]sysfs.NodeFilter{sysfs.NodeOfPMEMType},
			idset.NewIDSet(4, 5, 6, 7),
		},
		{
			"PMEM or odd ID nodes",
			[]sysfs.NodeFilter{
				sysfs.NodeFilterOr(sysfs.NodeOfPMEMType, sysfs.NodeFilterNot(nodeWithEvenID)),
			},
			idset.NewIDSet(1, 3, 4, 5, 6, 7),
		},
	}

	sys := sampleSysfs["sample2"]
	require.NotNil(t, sys)

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			nodes := sys.FilterNodes(sys.NodeIDs(), tc.filters...)
			require.Equal(t, tc.result, nodes)
		})
	}
}

func TestFilteredClosestNodes(t *testing.T) {
	tcases := []struct {
		name      string
		node      ID
		filters   []sysfs.NodeFilter
		closest   []idset.IDSet
		distances []int
	}{
		{
			"even ID nodes close to node #0", 0,
			[]sysfs.NodeFilter{nodeWithEvenID},
			[]idset.IDSet{
				idset.NewIDSet(2),
				idset.NewIDSet(4),
				idset.NewIDSet(6),
			},
			[]int{11, 17, 28},
		},
		{
			"odd ID nodes close to node #0", 0,
			[]sysfs.NodeFilter{sysfs.NodeFilterNot(nodeWithEvenID)},
			[]idset.IDSet{
				idset.NewIDSet(1, 3),
				idset.NewIDSet(5, 7),
			},
			[]int{21, 28},
		},
		{
			"even ID PMEM nodes close to node #0", 0,
			[]sysfs.NodeFilter{nodeWithEvenID, sysfs.NodeOfPMEMType},
			[]idset.IDSet{
				idset.NewIDSet(4),
				idset.NewIDSet(6),
			},
			[]int{17, 28},
		},
		{
			"odd ID DRAM nodes close to node #1", 1,
			[]sysfs.NodeFilter{sysfs.NodeFilterNot(nodeWithEvenID), sysfs.NodeOfDRAMType},
			[]idset.IDSet{
				idset.NewIDSet(3),
			},
			[]int{11},
		},
	}

	sys := sampleSysfs["sample2"]
	require.NotNil(t, sys)

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, dists := sys.ClosestNodes(tc.node, tc.filters...)
			require.Equal(t, tc.closest, nodes)
			require.Equal(t, tc.distances, dists)
		})
	}
}
