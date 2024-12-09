// Copyright 2020 Intel Corporation. All Rights Reserved.
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

package cpuallocator

import (
	"os"
	"path"
	"testing"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"

	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils"

	logger "github.com/containers/nri-plugins/pkg/log"
)

func TestAllocatorHelper(t *testing.T) {
	// Create tmpdir and decompress testdata there
	tmpdir, err := os.MkdirTemp("", "nri-resource-policy-test-")
	if err != nil {
		t.Fatalf("failed to create tmpdir: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	if err := utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), tmpdir); err != nil {
		t.Fatalf("failed to decompress testdata: %v", err)
	}

	// Discover mock system from the testdata
	sys, err := sysfs.DiscoverSystemAt(
		path.Join(tmpdir, "sysfs", "2-socket-4-node-40-core", "sys"),
		sysfs.DiscoverCPUTopology, sysfs.DiscoverMemTopology)
	if err != nil {
		t.Fatalf("failed to discover mock system: %v", err)
	}
	topoCache := newTopologyCache(sys)

	// Fake cpu priorities: 5 cores from pkg #0 as high prio
	// Package CPUs: #0: [0-19,40-59], #1: [20-39,60-79]
	topoCache.cpuPriorities = [NumCPUPriorities]cpuset.CPUSet{
		cpuset.MustParse("2,5,8,15,17,42,45,48,55,57"),
		cpuset.MustParse("20-39,60-79"),
		cpuset.MustParse("0,1,3,4,6,7,9-14,16,18,19,40,41,43,44,46,47,49-54,56,58,59"),
	}

	tcs := []struct {
		description string
		from        cpuset.CPUSet
		prefer      CPUPriority
		cnt         int
		expected    cpuset.CPUSet
	}{
		{
			description: "too few available CPUs",
			from:        cpuset.MustParse("2,3,10-14,20"),
			prefer:      PriorityNormal,
			cnt:         9,
			expected:    cpuset.New(),
		},
		{
			description: "request all available CPUs",
			from:        cpuset.MustParse("2,3,10-14,20"),
			prefer:      PriorityNormal,
			cnt:         8,
			expected:    cpuset.MustParse("2,3,10-14,20"),
		},
		{
			description: "prefer high priority cpus",
			from:        cpuset.MustParse("2,3,10-25"),
			prefer:      PriorityHigh,
			cnt:         4,
			expected:    cpuset.New(2, 3, 15, 17),
		},
	}

	// Run tests
	for _, tc := range tcs {
		t.Run(tc.description, func(t *testing.T) {
			a := newAllocatorHelper(sys, topoCache)
			a.from = tc.from
			a.prefer = tc.prefer
			a.cnt = tc.cnt
			result := a.allocate()
			if !result.Equals(tc.expected) {
				t.Errorf("expected %q, result was %q", tc.expected, result)
			}
		})
	}
}

func TestClusteredAllocation(t *testing.T) {
	if v := os.Getenv("ENABLE_DEBUG"); v != "" {
		logger.EnableDebug(logSource)
	}

	// Create tmpdir and decompress testdata there
	tmpdir, err := os.MkdirTemp("", "nri-resource-policy-test-")
	if err != nil {
		t.Fatalf("failed to create tmpdir: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	if err := utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), tmpdir); err != nil {
		t.Fatalf("failed to decompress testdata: %v", err)
	}

	// Discover mock system from the testdata
	sys, err := sysfs.DiscoverSystemAt(
		path.Join(tmpdir, "sysfs", "2-socket-4-node-40-core", "sys"),
		sysfs.DiscoverCPUTopology, sysfs.DiscoverMemTopology)
	if err != nil {
		t.Fatalf("failed to discover mock system: %v", err)
	}
	topoCache := newTopologyCache(sys)

	// Fake cpu priorities: 5 cores from pkg #0 as high prio
	// Package CPUs: #0: [0-19,40-59], #1: [20-39,60-79]
	topoCache.cpuPriorities = [NumCPUPriorities]cpuset.CPUSet{
		cpuset.MustParse("0-79"),
	}

	topoCache.clusters = []*cpuCluster{
		{
			pkg:     0,
			die:     0,
			cluster: 0,
			cpus:    cpuset.MustParse("0-3"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 1,
			cpus:    cpuset.MustParse("4-7"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 2,
			cpus:    cpuset.MustParse("8-11"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 3,
			cpus:    cpuset.MustParse("12-15"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 4,
			cpus:    cpuset.MustParse("16-19"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 5,
			cpus:    cpuset.MustParse("40-43"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 6,
			cpus:    cpuset.MustParse("44-47"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 7,
			cpus:    cpuset.MustParse("48-51"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 8,
			cpus:    cpuset.MustParse("52-55"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 9,
			cpus:    cpuset.MustParse("56-59"),
		},

		{
			pkg:     1,
			die:     0,
			cluster: 0,
			cpus:    cpuset.MustParse("20,22,24,26"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 1,
			cpus:    cpuset.MustParse("21,23,25,27"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 2,
			cpus:    cpuset.MustParse("28-31"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 3,
			cpus:    cpuset.MustParse("32-35"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 4,
			cpus:    cpuset.MustParse("36-39"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 5,
			cpus:    cpuset.MustParse("60-63"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 6,
			cpus:    cpuset.MustParse("64-67"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 7,
			cpus:    cpuset.MustParse("68-71"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 8,
			cpus:    cpuset.MustParse("72-75"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 9,
			cpus:    cpuset.MustParse("76-79"),
		},
	}

	pkg0 := cpuset.MustParse("0-19,40-59")
	pkg1 := cpuset.MustParse("20-39,60-79")

	tcs := []struct {
		description string
		from        cpuset.CPUSet
		cnt         int
		expected    cpuset.CPUSet
	}{
		{
			description: "CPU cores worth one cluster",
			from:        pkg0,
			cnt:         4,
			expected:    cpuset.MustParse("0-3"),
		},
		{
			description: "CPU cores worth 2 clusters",
			from:        pkg0,
			cnt:         8,
			expected:    cpuset.MustParse("0-7"),
		},
		{
			description: "CPU cores worth 4 clusters in a package",
			from:        pkg0,
			cnt:         16,
			expected:    cpuset.MustParse("0-15"),
		},
		{
			description: "CPU cores worth all clusters in a package",
			from:        pkg0,
			cnt:         40,
			expected:    cpuset.MustParse("0-19,40-59"),
		},
		{
			description: "CPU cores 1 cluster more than available in the 1st package",
			from:        pkg0.Union(pkg1),
			cnt:         44,
			expected:    cpuset.MustParse("0-19,20,22,24,26,40-59"),
		},
		{
			description: "CPU cores 2 clusters more than available in the 1st package",
			from:        pkg0.Union(pkg1),
			cnt:         48,
			expected:    cpuset.MustParse("0-27,40-59"),
		},
	}

	// Run tests
	for _, tc := range tcs {
		t.Run(tc.description, func(t *testing.T) {
			a := newAllocatorHelper(sys, topoCache)
			a.from = tc.from
			a.cnt = tc.cnt
			result := a.allocate()
			if !result.Equals(tc.expected) {
				t.Errorf("expected %q, result was %q", tc.expected, result)
			}
		})
	}
}

func TestClusteredCoreKindAllocation(t *testing.T) {
	if v := os.Getenv("ENABLE_DEBUG"); v != "" {
		logger.EnableDebug(logSource)
	}

	// Create tmpdir and decompress testdata there
	tmpdir, err := os.MkdirTemp("", "nri-resource-policy-test-")
	if err != nil {
		t.Fatalf("failed to create tmpdir: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	if err := utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), tmpdir); err != nil {
		t.Fatalf("failed to decompress testdata: %v", err)
	}

	// Discover mock system from the testdata
	sys, err := sysfs.DiscoverSystemAt(
		path.Join(tmpdir, "sysfs", "2-socket-4-node-40-core", "sys"),
		sysfs.DiscoverCPUTopology, sysfs.DiscoverMemTopology)
	if err != nil {
		t.Fatalf("failed to discover mock system: %v", err)
	}

	cluster1 := []*cpuCluster{
		{
			pkg:     0,
			die:     0,
			cluster: 0,
			cpus:    cpuset.MustParse("0-3"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 1,
			cpus:    cpuset.MustParse("4-7"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 2,
			cpus:    cpuset.MustParse("8-11"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 3,
			cpus:    cpuset.MustParse("12-15"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 4,
			cpus:    cpuset.MustParse("16-19"),
			kind:    sysfs.EfficientCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 5,
			cpus:    cpuset.MustParse("40-43"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 6,
			cpus:    cpuset.MustParse("44-47"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 7,
			cpus:    cpuset.MustParse("48-51"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 8,
			cpus:    cpuset.MustParse("52-55"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 9,
			cpus:    cpuset.MustParse("56-59"),
			kind:    sysfs.EfficientCore,
		},

		{
			pkg:     1,
			die:     0,
			cluster: 0,
			cpus:    cpuset.MustParse("20,22,24,26"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 1,
			cpus:    cpuset.MustParse("21,23,25,27"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 2,
			cpus:    cpuset.MustParse("28-31"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 3,
			cpus:    cpuset.MustParse("32-35"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 4,
			cpus:    cpuset.MustParse("36-39"),
			kind:    sysfs.EfficientCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 5,
			cpus:    cpuset.MustParse("60-63"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 6,
			cpus:    cpuset.MustParse("64-67"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 7,
			cpus:    cpuset.MustParse("68-71"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 8,
			cpus:    cpuset.MustParse("72-75"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 9,
			cpus:    cpuset.MustParse("76-79"),
			kind:    sysfs.EfficientCore,
		},
	}

	cluster2 := []*cpuCluster{
		{
			pkg:     0,
			die:     0,
			cluster: 0,
			cpus:    cpuset.MustParse("0-3"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 1,
			cpus:    cpuset.MustParse("4-7"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 2,
			cpus:    cpuset.MustParse("8-11"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 3,
			cpus:    cpuset.MustParse("12-15"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 4,
			cpus:    cpuset.MustParse("16-19"),
			kind:    sysfs.EfficientCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 5,
			cpus:    cpuset.MustParse("40-43"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 6,
			cpus:    cpuset.MustParse("44-47"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 7,
			cpus:    cpuset.MustParse("48-51"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 8,
			cpus:    cpuset.MustParse("52-55"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 9,
			cpus:    cpuset.MustParse("56-59"),
			kind:    sysfs.EfficientCore,
		},

		{
			pkg:     1,
			die:     0,
			cluster: 0,
			cpus:    cpuset.MustParse("20,22,24,26"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 1,
			cpus:    cpuset.MustParse("21,23,25,27"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 2,
			cpus:    cpuset.MustParse("28-31"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 3,
			cpus:    cpuset.MustParse("32-35"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 4,
			cpus:    cpuset.MustParse("36-37"),
			kind:    sysfs.EfficientCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 5,
			cpus:    cpuset.MustParse("38-39"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 6,
			cpus:    cpuset.MustParse("60-63"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 7,
			cpus:    cpuset.MustParse("64-67"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 8,
			cpus:    cpuset.MustParse("68-71"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 9,
			cpus:    cpuset.MustParse("72-75"),
			kind:    sysfs.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 10,
			cpus:    cpuset.MustParse("76-79"),
			kind:    sysfs.EfficientCore,
		},
	}

	pkg0 := cpuset.MustParse("0-19,40-59")
	pkg1 := cpuset.MustParse("20-39,60-79")
	all := pkg0.Union(pkg1)

	tcs := []struct {
		description string
		clusters    []*cpuCluster
		from        cpuset.CPUSet
		prefer      CPUPriority
		cnt         int
		expected    cpuset.CPUSet
	}{
		{
			description: "P-cores worth one cluster",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         4,
			expected:    cpuset.MustParse("0-3"),
		},
		{
			description: "P-cores worth 2 clusters",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         8,
			expected:    cpuset.MustParse("0-7"),
		},
		{
			description: "P-cores worth all clusters in a package",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         32,
			expected:    cpuset.MustParse("0-15,40-55"),
		},
		{
			description: "E-cores worth 1 cluster",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityLow,
			cnt:         4,
			expected:    cpuset.MustParse("16-19"),
		},
		{
			description: "E-cores worth 2 clusters",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityLow,
			cnt:         8,
			expected:    cpuset.MustParse("16-19,56-59"),
		},
		{
			description: "P-cores worth 1 cluster more than in the 1st package",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         36,
			expected:    cpuset.MustParse("0-15,40-55,20,22,24,26"),
		},
		{
			description: "P-cores worth 2 clusters more than in the 1st package",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         40,
			expected:    cpuset.MustParse("0-15,20-27,40-55"),
		},
		{
			description: "E-cores worth 1 clusters, should take tighter fit",
			clusters:    cluster2,
			from:        all,
			prefer:      PriorityLow,
			cnt:         2,
			expected:    cpuset.MustParse("36-37"),
		},
		{
			description: "E-cores worth 2 clusters, should take tighter fit",
			clusters:    cluster2,
			from:        all,
			prefer:      PriorityLow,
			cnt:         6,
			expected:    cpuset.MustParse("36-37,76-79"),
		},
		{
			description: "E-cores worth 2 clusters, should take single die",
			clusters:    cluster2,
			from:        all,
			prefer:      PriorityLow,
			cnt:         8,
			expected:    cpuset.MustParse("16-19,56-59"),
		},
	}

	// Run tests
	for _, tc := range tcs {
		t.Run(tc.description, func(t *testing.T) {
			topoCache := newTopologyCache(sys)
			topoCache.clusters = tc.clusters
			a := newAllocatorHelper(sys, topoCache)
			a.from = tc.from
			a.prefer = tc.prefer
			a.cnt = tc.cnt
			result := a.allocate()
			if !result.Equals(tc.expected) {
				t.Errorf("expected %q, result was %q", tc.expected, result)
			}
		})
	}
}
