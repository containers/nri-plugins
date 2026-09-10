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

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"github.com/containers/nri-plugins/pkg/lib/hardware"
	"github.com/containers/nri-plugins/pkg/testutils"

	logger "github.com/containers/nri-plugins/pkg/log"
)

func TestAllocatorHelper(t *testing.T) {
	// Create tmpdir and decompress testdata there
	tmpdir, err := os.MkdirTemp("", "nri-resource-policy-test-")
	if err != nil {
		t.Fatalf("failed to create tmpdir: %v", err)
	}
	defer removeAll(t, tmpdir)

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), tmpdir); err != nil {
		t.Fatalf("failed to decompress testdata: %v", err)
	}

	// Discover mock system from the testdata
	m, err := hardware.Discover(
		hardware.WithRoot(path.Join(tmpdir, "sysfs", "2-socket-4-node-40-core")))
	if err != nil {
		t.Fatalf("failed to discover mock system: %v", err)
	}
	topoCache := newTopologyCache(m)

	// Fake cpu priorities: 5 cores from pkg #0 as high prio
	// Package CPUs: #0: [0-19,40-59], #1: [20-39,60-79]
	topoCache.cpuPriorities = cpuPriorities{
		libcpu.MustParseCpuMask("2,5,8,15,17,42,45,48,55,57"),
		libcpu.MustParseCpuMask("20-39,60-79"),
		libcpu.MustParseCpuMask("0,1,3,4,6,7,9-14,16,18,19,40,41,43,44,46,47,49-54,56,58,59"),
	}

	tcs := []struct {
		description string
		from        *libcpu.CpuMask
		prefer      CPUPriority
		cnt         int
		expected    *libcpu.CpuMask
	}{
		{
			description: "too few available CPUs",
			from:        libcpu.MustParseCpuMask("2,3,10-14,20"),
			prefer:      PriorityNormal,
			cnt:         9,
			expected:    libcpu.NewCpuMask(),
		},
		{
			description: "request all available CPUs",
			from:        libcpu.MustParseCpuMask("2,3,10-14,20"),
			prefer:      PriorityNormal,
			cnt:         8,
			expected:    libcpu.MustParseCpuMask("2,3,10-14,20"),
		},
		{
			description: "prefer high priority cpus",
			from:        libcpu.MustParseCpuMask("2,3,10-25"),
			prefer:      PriorityHigh,
			cnt:         4,
			expected:    libcpu.NewCpuMask(2, 3, 15, 17),
		},
	}

	// Run tests
	for _, tc := range tcs {
		t.Run(tc.description, func(t *testing.T) {
			a := newAllocatorHelper(m, topoCache)
			a.from = tc.from.Clone()
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
	defer removeAll(t, tmpdir)

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), tmpdir); err != nil {
		t.Fatalf("failed to decompress testdata: %v", err)
	}

	// Discover mock system from the testdata
	m, err := hardware.Discover(
		hardware.WithRoot(path.Join(tmpdir, "sysfs", "2-socket-4-node-40-core")))
	if err != nil {
		t.Fatalf("failed to discover mock system: %v", err)
	}
	topoCache := newTopologyCache(m)

	// Fake cpu priorities: 5 cores from pkg #0 as high prio
	// Package CPUs: #0: [0-19,40-59], #1: [20-39,60-79]
	topoCache.cpuPriorities = cpuPriorities{
		libcpu.MustParseCpuMask("0-79"),
	}

	topoCache.clusters = []*cpuCluster{
		{
			pkg:     0,
			die:     0,
			cluster: 0,
			cpus:    libcpu.MustParseCpuMask("0-3"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 1,
			cpus:    libcpu.MustParseCpuMask("4-7"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 2,
			cpus:    libcpu.MustParseCpuMask("8-11"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 3,
			cpus:    libcpu.MustParseCpuMask("12-15"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 4,
			cpus:    libcpu.MustParseCpuMask("16-19"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 5,
			cpus:    libcpu.MustParseCpuMask("40-43"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 6,
			cpus:    libcpu.MustParseCpuMask("44-47"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 7,
			cpus:    libcpu.MustParseCpuMask("48-51"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 8,
			cpus:    libcpu.MustParseCpuMask("52-55"),
		},
		{
			pkg:     0,
			die:     0,
			cluster: 9,
			cpus:    libcpu.MustParseCpuMask("56-59"),
		},

		{
			pkg:     1,
			die:     0,
			cluster: 0,
			cpus:    libcpu.MustParseCpuMask("20,22,24,26"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 1,
			cpus:    libcpu.MustParseCpuMask("21,23,25,27"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 2,
			cpus:    libcpu.MustParseCpuMask("28-31"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 3,
			cpus:    libcpu.MustParseCpuMask("32-35"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 4,
			cpus:    libcpu.MustParseCpuMask("36-39"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 5,
			cpus:    libcpu.MustParseCpuMask("60-63"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 6,
			cpus:    libcpu.MustParseCpuMask("64-67"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 7,
			cpus:    libcpu.MustParseCpuMask("68-71"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 8,
			cpus:    libcpu.MustParseCpuMask("72-75"),
		},
		{
			pkg:     1,
			die:     0,
			cluster: 9,
			cpus:    libcpu.MustParseCpuMask("76-79"),
		},
	}

	pkg0 := libcpu.MustParseCpuMask("0-19,40-59")
	pkg1 := libcpu.MustParseCpuMask("20-39,60-79")

	tcs := []struct {
		description string
		from        *libcpu.CpuMask
		cnt         int
		expected    *libcpu.CpuMask
	}{
		{
			description: "CPU cores worth one cluster",
			from:        pkg0,
			cnt:         4,
			expected:    libcpu.MustParseCpuMask("0-3"),
		},
		{
			description: "CPU cores worth 2 clusters",
			from:        pkg0,
			cnt:         8,
			expected:    libcpu.MustParseCpuMask("0-7"),
		},
		{
			description: "CPU cores worth 4 clusters in a package",
			from:        pkg0,
			cnt:         16,
			expected:    libcpu.MustParseCpuMask("0-15"),
		},
		{
			description: "CPU cores worth all clusters in a package",
			from:        pkg0,
			cnt:         40,
			expected:    libcpu.MustParseCpuMask("0-19,40-59"),
		},
		{
			description: "CPU cores 1 cluster more than available in the 1st package",
			from:        pkg0.Union(pkg1),
			cnt:         44,
			expected:    libcpu.MustParseCpuMask("0-19,20,22,24,26,40-59"),
		},
		{
			description: "CPU cores 2 clusters more than available in the 1st package",
			from:        pkg0.Union(pkg1),
			cnt:         48,
			expected:    libcpu.MustParseCpuMask("0-27,40-59"),
		},
	}

	// Run tests
	for _, tc := range tcs {
		t.Run(tc.description, func(t *testing.T) {
			a := newAllocatorHelper(m, topoCache)
			a.from = tc.from.Clone()
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
	defer removeAll(t, tmpdir)

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), tmpdir); err != nil {
		t.Fatalf("failed to decompress testdata: %v", err)
	}

	// Discover mock system from the testdata
	m, err := hardware.Discover(
		hardware.WithRoot(path.Join(tmpdir, "sysfs", "2-socket-4-node-40-core")))
	if err != nil {
		t.Fatalf("failed to discover mock system: %v", err)
	}

	cluster1 := []*cpuCluster{
		{
			pkg:     0,
			die:     0,
			cluster: 0,
			cpus:    libcpu.MustParseCpuMask("0-3"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 1,
			cpus:    libcpu.MustParseCpuMask("4-7"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 2,
			cpus:    libcpu.MustParseCpuMask("8-11"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 3,
			cpus:    libcpu.MustParseCpuMask("12-15"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 4,
			cpus:    libcpu.MustParseCpuMask("16-19"),
			kind:    hardware.EfficientCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 5,
			cpus:    libcpu.MustParseCpuMask("40-43"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 6,
			cpus:    libcpu.MustParseCpuMask("44-47"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 7,
			cpus:    libcpu.MustParseCpuMask("48-51"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 8,
			cpus:    libcpu.MustParseCpuMask("52-55"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 9,
			cpus:    libcpu.MustParseCpuMask("56-59"),
			kind:    hardware.EfficientCore,
		},

		{
			pkg:     1,
			die:     0,
			cluster: 0,
			cpus:    libcpu.MustParseCpuMask("20,22,24,26"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 1,
			cpus:    libcpu.MustParseCpuMask("21,23,25,27"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 2,
			cpus:    libcpu.MustParseCpuMask("28-31"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 3,
			cpus:    libcpu.MustParseCpuMask("32-35"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 4,
			cpus:    libcpu.MustParseCpuMask("36-39"),
			kind:    hardware.EfficientCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 5,
			cpus:    libcpu.MustParseCpuMask("60-63"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 6,
			cpus:    libcpu.MustParseCpuMask("64-67"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 7,
			cpus:    libcpu.MustParseCpuMask("68-71"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 8,
			cpus:    libcpu.MustParseCpuMask("72-75"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 9,
			cpus:    libcpu.MustParseCpuMask("76-79"),
			kind:    hardware.EfficientCore,
		},
	}

	cluster2 := []*cpuCluster{
		{
			pkg:     0,
			die:     0,
			cluster: 0,
			cpus:    libcpu.MustParseCpuMask("0-3"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 1,
			cpus:    libcpu.MustParseCpuMask("4-7"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 2,
			cpus:    libcpu.MustParseCpuMask("8-11"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 3,
			cpus:    libcpu.MustParseCpuMask("12-15"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 4,
			cpus:    libcpu.MustParseCpuMask("16-19"),
			kind:    hardware.EfficientCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 5,
			cpus:    libcpu.MustParseCpuMask("40-43"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 6,
			cpus:    libcpu.MustParseCpuMask("44-47"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 7,
			cpus:    libcpu.MustParseCpuMask("48-51"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 8,
			cpus:    libcpu.MustParseCpuMask("52-55"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     0,
			die:     0,
			cluster: 9,
			cpus:    libcpu.MustParseCpuMask("56-59"),
			kind:    hardware.EfficientCore,
		},

		{
			pkg:     1,
			die:     0,
			cluster: 0,
			cpus:    libcpu.MustParseCpuMask("20,22,24,26"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 1,
			cpus:    libcpu.MustParseCpuMask("21,23,25,27"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 2,
			cpus:    libcpu.MustParseCpuMask("28-31"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 3,
			cpus:    libcpu.MustParseCpuMask("32-35"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 4,
			cpus:    libcpu.MustParseCpuMask("36-37"),
			kind:    hardware.EfficientCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 5,
			cpus:    libcpu.MustParseCpuMask("38-39"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 6,
			cpus:    libcpu.MustParseCpuMask("60-63"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 7,
			cpus:    libcpu.MustParseCpuMask("64-67"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 8,
			cpus:    libcpu.MustParseCpuMask("68-71"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 9,
			cpus:    libcpu.MustParseCpuMask("72-75"),
			kind:    hardware.PerformanceCore,
		},
		{
			pkg:     1,
			die:     0,
			cluster: 10,
			cpus:    libcpu.MustParseCpuMask("76-79"),
			kind:    hardware.EfficientCore,
		},
	}

	pkg0 := libcpu.MustParseCpuMask("0-19,40-59")
	pkg1 := libcpu.MustParseCpuMask("20-39,60-79")
	all := pkg0.Union(pkg1)

	tcs := []struct {
		description string
		clusters    []*cpuCluster
		from        *libcpu.CpuMask
		prefer      CPUPriority
		cnt         int
		expected    *libcpu.CpuMask
	}{
		{
			description: "P-cores worth one cluster",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         4,
			expected:    libcpu.MustParseCpuMask("0-3"),
		},
		{
			description: "P-cores worth 2 clusters",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         8,
			expected:    libcpu.MustParseCpuMask("0-7"),
		},
		{
			description: "P-cores worth all clusters in a package",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         32,
			expected:    libcpu.MustParseCpuMask("0-15,40-55"),
		},
		{
			description: "E-cores worth 1 cluster",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityLow,
			cnt:         4,
			expected:    libcpu.MustParseCpuMask("16-19"),
		},
		{
			description: "E-cores worth 2 clusters",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityLow,
			cnt:         8,
			expected:    libcpu.MustParseCpuMask("16-19,56-59"),
		},
		{
			description: "P-cores worth 1 cluster more than in the 1st package",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         36,
			expected:    libcpu.MustParseCpuMask("0-15,40-55,20,22,24,26"),
		},
		{
			description: "P-cores worth 2 clusters more than in the 1st package",
			clusters:    cluster1,
			from:        all,
			prefer:      PriorityNormal,
			cnt:         40,
			expected:    libcpu.MustParseCpuMask("0-15,20-27,40-55"),
		},
		{
			description: "E-cores worth 1 clusters, should take tighter fit",
			clusters:    cluster2,
			from:        all,
			prefer:      PriorityLow,
			cnt:         2,
			expected:    libcpu.MustParseCpuMask("36-37"),
		},
		{
			description: "E-cores worth 2 clusters, should take tighter fit",
			clusters:    cluster2,
			from:        all,
			prefer:      PriorityLow,
			cnt:         6,
			expected:    libcpu.MustParseCpuMask("36-37,76-79"),
		},
		{
			description: "E-cores worth 2 clusters, should take single die",
			clusters:    cluster2,
			from:        all,
			prefer:      PriorityLow,
			cnt:         8,
			expected:    libcpu.MustParseCpuMask("16-19,56-59"),
		},
	}

	// Run tests
	for _, tc := range tcs {
		t.Run(tc.description, func(t *testing.T) {
			topoCache := newTopologyCache(m)
			topoCache.clusters = tc.clusters
			a := newAllocatorHelper(m, topoCache)
			a.from = tc.from.Clone()
			a.prefer = tc.prefer
			a.cnt = tc.cnt
			result := a.allocate()
			if !result.Equals(tc.expected) {
				t.Errorf("expected %q, result was %q", tc.expected, result)
			}
		})
	}
}

func removeAll(t *testing.T, path string) {
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("failed to remove %q: %v", path, err)
	}
}

// TestAllocateAndReleaseCpus pins what the two public entry points do to the set
// they are given, which is how every caller learns what is left. Nothing else
// here covers it: the tests above drive the helper directly.
func TestAllocateAndReleaseCpus(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "nri-resource-policy-test-")
	if err != nil {
		t.Fatalf("failed to create tmpdir: %v", err)
	}
	defer removeAll(t, tmpdir)

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), tmpdir); err != nil {
		t.Fatalf("failed to decompress testdata: %v", err)
	}

	m, err := hardware.Discover(
		hardware.WithRoot(path.Join(tmpdir, "sysfs", "2-socket-4-node-40-core")))
	if err != nil {
		t.Fatalf("failed to discover mock system: %v", err)
	}
	ca := NewCPUAllocator(m)

	for _, tc := range []struct {
		description string
		from        string
		cnt         int
		release     bool
		expected    string // what comes back
		remaining   string // what the given set holds afterwards
		expectErr   bool
	}{
		{
			description: "allocating takes the CPUs out of the set",
			from:        "0-7",
			cnt:         2,
			expected:    "0-1",
			remaining:   "2-7",
		},
		{
			description: "allocating everything empties the set",
			from:        "0-7",
			cnt:         8,
			expected:    "0-7",
			remaining:   "",
		},
		{
			description: "asking for more than there is leaves the set alone",
			from:        "0-7",
			cnt:         9,
			expected:    "",
			remaining:   "0-7",
			expectErr:   true,
		},
		{
			// Note which way round this is: the set is left holding the CPUs to
			// release, and the ones to keep are returned.
			description: "releasing leaves the released CPUs in the set",
			from:        "0-7",
			cnt:         2,
			release:     true,
			expected:    "0-5",
			remaining:   "6-7",
		},
		{
			description: "releasing everything keeps nothing",
			from:        "0-7",
			cnt:         8,
			release:     true,
			expected:    "",
			remaining:   "0-7",
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			var (
				from = libcpu.MustParseCpuMask(tc.from)
				cpus *libcpu.CpuMask
				err  error
			)

			if tc.release {
				cpus, err = ca.ReleaseCpus(from, tc.cnt)
			} else {
				cpus, err = ca.AllocateCpus(from, tc.cnt)
			}

			switch {
			case tc.expectErr && err == nil:
				t.Error("expected an error, got none")
			case !tc.expectErr && err != nil:
				t.Errorf("unexpected error: %v", err)
			}
			if got := cpus.String(); got != tc.expected {
				t.Errorf("expected %q back, got %q", tc.expected, got)
			}
			if got := from.String(); got != tc.remaining {
				t.Errorf("expected %q left in the set, got %q", tc.remaining, got)
			}
		})
	}
}
