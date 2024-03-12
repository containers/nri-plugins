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
	"os"
	"path"

	"github.com/containers/nri-plugins/pkg/sysfs"
	idset "github.com/intel/goresctrl/pkg/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
)

var _ = BeforeSuite(func() {
	cwd, _ := os.Getwd()

	SetSysRoot(path.Join(cwd, "testdata/sample1"))
	sys, err := DiscoverSystem()
	Expect(err).To(BeNil())
	Expect(sys).ToNot(BeNil())
	sampleSysfs["sample1"] = sys

	SetSysRoot(path.Join(cwd, "testdata/sample2"))
	sys, err = DiscoverSystem()
	Expect(err).To(BeNil())
	Expect(sys).ToNot(BeNil())
	sampleSysfs["sample2"] = sys
})

var _ = DescribeTable("cache discovery",
	func(sample string, cpu ID, idx, level, id int, kind CacheType, size uint64, cpus string) {
		sys := sampleSysfs[sample]
		Expect(sys).ToNot(BeNil())
		c := sys.CPU(cpu)
		Expect(c).ToNot(BeNil())
		cch := c.GetCacheByIndex(idx)
		Expect(cch).ToNot(BeNil())
		Expect(cch.ID()).To(Equal(id))
		Expect(cch.Level()).To(Equal(level))
		Expect(cch.Type()).To(Equal(kind))
		Expect(cch.Size()).To(Equal(size))
		Expect(cch.SharedCPUSet().String()).To(Equal(cpus))
	},

	// sample sysfs 1, pick and test a few CPU cores
	Entry("CPU #0, cache #0", "sample1", 0, 0, 1, 0, Data, 48*K, "0-1"),
	Entry("CPU #0, cache #1", "sample1", 0, 1, 1, 0, Instruction, 32*K, "0-1"),
	Entry("CPU #0, cache #2", "sample1", 0, 2, 2, 0, Unified, 1280*K, "0-1"),
	Entry("CPU #0, cache #3", "sample1", 0, 3, 3, 0, Unified, 18432*K, "0-15"),
	Entry("CPU #1, cache #0", "sample1", 1, 0, 1, 0, Data, 48*K, "0-1"),
	Entry("CPU #1, cache #1", "sample1", 1, 1, 1, 0, Instruction, 32*K, "0-1"),
	Entry("CPU #1, cache #2", "sample1", 1, 2, 2, 0, Unified, 1280*K, "0-1"),
	Entry("CPU #1, cache #3", "sample1", 1, 3, 3, 0, Unified, 18432*K, "0-15"),

	Entry("CPU #8, cache #0", "sample1", 8, 0, 1, 16, Data, 32*K, "8"),
	Entry("CPU #8, cache #1", "sample1", 8, 1, 1, 16, Instruction, 64*K, "8"),
	Entry("CPU #8, cache #2", "sample1", 8, 2, 2, 4, Unified, 2048*K, "8-11"),
	Entry("CPU #8, cache #3", "sample1", 8, 3, 3, 0, Unified, 18432*K, "0-15"),
	Entry("CPU #11, cache #0", "sample1", 11, 0, 1, 19, Data, 32*K, "11"),
	Entry("CPU #11, cache #1", "sample1", 11, 1, 1, 19, Instruction, 64*K, "11"),
	Entry("CPU #11, cache #2", "sample1", 11, 2, 2, 4, Unified, 2048*K, "8-11"),
	Entry("CPU #11, cache #3", "sample1", 11, 3, 3, 0, Unified, 18432*K, "0-15"),

	Entry("CPU #12, cache #0", "sample1", 12, 0, 1, 20, Data, 32*K, "12"),
	Entry("CPU #12, cache #1", "sample1", 12, 1, 1, 20, Instruction, 64*K, "12"),
	Entry("CPU #12, cache #2", "sample1", 12, 2, 2, 5, Unified, 2048*K, "12-15"),
	Entry("CPU #12, cache #3", "sample1", 12, 3, 3, 0, Unified, 18432*K, "0-15"),
	Entry("CPU #15, cache #0", "sample1", 15, 0, 1, 23, Data, 32*K, "15"),
	Entry("CPU #15, cache #1", "sample1", 15, 1, 1, 23, Instruction, 64*K, "15"),
	Entry("CPU #15, cache #2", "sample1", 15, 2, 2, 5, Unified, 2048*K, "12-15"),
	Entry("CPU #15, cache #3", "sample1", 15, 3, 3, 0, Unified, 18432*K, "0-15"),

	// sample sysfs 2, pick and test a few CPU cores
	Entry("CPU #0, cache #0", "sample2", 0, 0, 1, 0, Data, 32*K, "0,56"),
	Entry("CPU #0, cache #1", "sample2", 0, 1, 1, 0, Instruction, 32*K, "0,56"),
	Entry("CPU #0, cache #2", "sample2", 0, 2, 2, 0, Unified, 1024*K, "0,56"),
	Entry("CPU #0, cache #3", "sample2", 0, 3, 3, 0, Unified, 39424*K, "0,2,4,6,8,10,12,14,16,18,20,22,24,26,28,30,32,34,36,38,40,42,44,46,48,50,52,54,56,58,60,62,64,66,68,70,72,74,76,78,80,82,84,86,88,90,92,94,96,98,100,102,104,106,108,110"),
	Entry("CPU #56, cache #0", "sample2", 56, 0, 1, 0, Data, 32*K, "0,56"),
	Entry("CPU #56, cache #1", "sample2", 56, 1, 1, 0, Instruction, 32*K, "0,56"),
	Entry("CPU #56, cache #2", "sample2", 56, 2, 2, 0, Unified, 1024*K, "0,56"),
	Entry("CPU #56, cache #3", "sample2", 56, 3, 3, 0, Unified, 39424*K, "0,2,4,6,8,10,12,14,16,18,20,22,24,26,28,30,32,34,36,38,40,42,44,46,48,50,52,54,56,58,60,62,64,66,68,70,72,74,76,78,80,82,84,86,88,90,92,94,96,98,100,102,104,106,108,110"),

	Entry("CPU #1, cache #0", "sample2", 1, 0, 1, 32, Data, 32*K, "1,57"),
	Entry("CPU #1, cache #1", "sample2", 1, 1, 1, 32, Instruction, 32*K, "1,57"),
	Entry("CPU #1, cache #2", "sample2", 1, 2, 2, 32, Unified, 1024*K, "1,57"),
	Entry("CPU #1, cache #3", "sample2", 1, 3, 3, 1, Unified, 39424*K, "1,3,5,7,9,11,13,15,17,19,21,23,25,27,29,31,33,35,37,39,41,43,45,47,49,51,53,55,57,59,61,63,65,67,69,71,73,75,77,79,81,83,85,87,89,91,93,95,97,99,101,103,105,107,109,111"),
	Entry("CPU #57, cache #0", "sample2", 57, 0, 1, 32, Data, 32*K, "1,57"),
	Entry("CPU #57, cache #1", "sample2", 57, 1, 1, 32, Instruction, 32*K, "1,57"),
	Entry("CPU #57, cache #2", "sample2", 57, 2, 2, 32, Unified, 1024*K, "1,57"),
	Entry("CPU #57, cache #3", "sample2", 57, 3, 3, 1, Unified, 39424*K, "1,3,5,7,9,11,13,15,17,19,21,23,25,27,29,31,33,35,37,39,41,43,45,47,49,51,53,55,57,59,61,63,65,67,69,71,73,75,77,79,81,83,85,87,89,91,93,95,97,99,101,103,105,107,109,111"),
)

var _ = DescribeTable("LLC CPUSet",
	func(sample string, cpu ID, cpus string) {
		sys := sampleSysfs[sample]
		Expect(sys).ToNot(BeNil())
		c := sys.CPU(cpu)
		Expect(c).ToNot(BeNil())
		cset := c.GetLastLevelCacheCPUSet()
		Expect(cset.String()).To(Equal(cpus))
	},

	// sample sysfs 1, pick and test a few CPU cores
	Entry("CPU #0", "sample1", 0, "0-15"),

	// sample sysfs 2, pick and test a few CPU cores
	Entry("CPU #0", "sample2", 0, "0,2,4,6,8,10,12,14,16,18,20,22,24,26,28,30,32,34,36,38,40,42,44,46,48,50,52,54,56,58,60,62,64,66,68,70,72,74,76,78,80,82,84,86,88,90,92,94,96,98,100,102,104,106,108,110"),
	Entry("CPU #1", "sample2", 1, "1,3,5,7,9,11,13,15,17,19,21,23,25,27,29,31,33,35,37,39,41,43,45,47,49,51,53,55,57,59,61,63,65,67,69,71,73,75,77,79,81,83,85,87,89,91,93,95,97,99,101,103,105,107,109,111"),
)

var _ = DescribeTable("Unfiltered cluster detection",
	func(sample string, pkg, die idset.ID, clusterIDs []idset.ID) {
		sys := sampleSysfs[sample]
		Expect(sys).ToNot(BeNil())

		result := sys.Package(pkg).DieClusterIDs(die)
		Expect(result).To(Equal(clusterIDs))
	},

	Entry("die #0 cluster IDs", "sample1", 0, 0, []idset.ID{0, 8, 16, 24, 32, 40}),
)

var _ = DescribeTable("Unfiltered cluster CPUSet",
	func(sample string, pkg, die, cluster idset.ID, cpus string) {
		sys := sampleSysfs[sample]
		Expect(sys).ToNot(BeNil())

		result := sys.Package(pkg).DieClusterCPUSet(die, cluster)
		Expect(result.String()).To(Equal(cpus))
	},

	Entry("die #0, cluster #0", "sample1", 0, 0, 0, "0-1"),
	Entry("die #0, cluster #32", "sample1", 0, 0, 8, "2-3"),
	Entry("die #0, cluster #40", "sample1", 0, 0, 16, "4-5"),
	Entry("die #0, cluster #40", "sample1", 0, 0, 24, "6-7"),
	Entry("die #0, cluster #40", "sample1", 0, 0, 32, "8-11"),
	Entry("die #0, cluster #40", "sample1", 0, 0, 40, "12-15"),
)

var _ = DescribeTable("Logical/hyperthread-filtered cluster detection",
	func(sample string, pkg, die idset.ID, clusterIDs []idset.ID) {
		sys := sampleSysfs[sample]
		Expect(sys).ToNot(BeNil())

		result := sys.Package(pkg).LogicalDieClusterIDs(die)
		Expect(result).To(Equal(clusterIDs))
	},

	Entry("die #0 cluster IDs", "sample1", 0, 0, []idset.ID{0, 32, 40}),
)

var _ = DescribeTable("Logical/hyperthread-filtered cluster CPUSet",
	func(sample string, pkg, die, cluster idset.ID, cpus string) {
		sys := sampleSysfs[sample]
		Expect(sys).ToNot(BeNil())

		result := sys.Package(pkg).LogicalDieClusterCPUSet(die, cluster)
		Expect(result.String()).To(Equal(cpus))
	},

	Entry("die #0, cluster #0", "sample1", 0, 0, 0, "0-7"),
	Entry("die #0, cluster #32", "sample1", 0, 0, 32, "8-11"),
	Entry("die #0, cluster #40", "sample1", 0, 0, 40, "12-15"),
)

var _ = DescribeTable("P-/E-Core CPU detection",
	func(sample string, kind CoreKind, cpus string) {
		sys := sampleSysfs[sample]
		Expect(sys).ToNot(BeNil())
		cset := sys.CoreKindCPUs(kind)
		Expect(cset.String()).To(Equal(cpus))
	},

	Entry("P-Cores", "sample1", PerformanceCore, "0-7"),
	Entry("E-Cores", "sample1", EfficientCore, "8-15"),
)
