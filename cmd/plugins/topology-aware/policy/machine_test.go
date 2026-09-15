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

package topologyaware

import (
	"fmt"
	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/containers/nri-plugins/pkg/lib/hardware"
)

// synthNode describes one NUMA node of a machine to build for a test.
//
// A node with CPUs is ordinary memory. A node with memory and no CPUs is
// something special, and which special sort is inferred from its size against the
// DRAM nodes: larger is persistent memory, smaller is high-bandwidth memory. That
// is how the hardware package classifies a real machine, so describing the sizes
// is how a test asks for a PMEM or an HBM node.
type synthNode struct {
	cpus     string // cpulist, empty for a node with no CPUs of its own
	memKB    int    // MemTotal in kB
	distance []int  // distance to every node, this one included
}

// synthMachine builds a machine from the given nodes by writing the topology out
// as sysfs and discovering it.
//
// The policy reads topology from a *hardware.Machine, which is a concrete type
// and cannot be faked, so a test describes the machine it wants and reads it back
// through real discovery. All CPUs are single-threaded cores; the node a CPU is
// in also gives its package, one package per node with CPUs.
func synthMachine(t *testing.T, nodes []synthNode) *hardware.Machine {
	t.Helper()

	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	fsys := fstest.MapFS{}

	var (
		online = libcpu.NewCpuMask()
		normal = libcpu.NewCpuMask()
		pkg    = 0
	)
	for id, node := range nodes {
		dir := fmt.Sprintf("sys/devices/system/node/node%d", id)

		cpus := libcpu.NewCpuMask()
		if node.cpus != "" {
			var err error
			if cpus, err = libcpu.ParseCpuMask(node.cpus); err != nil {
				t.Fatalf("node%d: bad cpulist %q: %v", id, node.cpus, err)
			}
		}
		online = online.Union(cpus)

		fsys[dir+"/cpulist"] = file(node.cpus + "\n")
		fsys[dir+"/meminfo"] = file(
			fmt.Sprintf("Node %d MemTotal: %d kB\n", id, node.memKB))

		if node.memKB > 0 {
			normal = normal.Union(libcpu.NewCpuMask(id))
		}

		dist := make([]string, 0, len(node.distance))
		for _, d := range node.distance {
			dist = append(dist, fmt.Sprintf("%d", d))
		}
		fsys[dir+"/distance"] = file(strings.Join(dist, " ") + "\n")

		if cpus.IsEmpty() {
			continue
		}
		for _, cpu := range cpus.List() {
			topo := fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology", cpu)
			fsys[topo+"/physical_package_id"] = file(fmt.Sprintf("%d\n", pkg))
			fsys[topo+"/core_id"] = file(fmt.Sprintf("%d\n", cpu))
			fsys[topo+"/core_cpus_list"] = file(fmt.Sprintf("%d\n", cpu))
		}
		pkg++
	}

	all := online.String()

	// Every node with memory of its own has normal, i.e. non-movable, memory.
	// Without this a node reads as movable-only, which nothing here wants to
	// describe and which leaves an allocator with no memory to hand out.
	fsys["sys/devices/system/node/has_normal_memory"] = file(normal.String() + "\n")

	fsys["proc/meminfo"] = file("MemTotal: 1048576 kB\n")
	fsys["sys/devices/system/cpu/online"] = file(all + "\n")
	fsys["sys/devices/system/cpu/present"] = file(all + "\n")
	fsys["sys/devices/system/cpu/possible"] = file(all + "\n")

	m, err := hardware.Discover(hardware.WithFS(fsys))
	if err != nil {
		t.Fatalf("failed to discover the test machine: %v", err)
	}
	return m
}

// oneCpuMachine is the smallest machine there is: one CPU, one node, one
// package. Asking it about any other package or node finds nothing, which is
// what the cases for hints naming hardware the machine does not have need.
func oneCpuMachine(t *testing.T) *hardware.Machine {
	t.Helper()
	return synthMachine(t, []synthNode{
		{cpus: "0", memKB: 1048576, distance: []int{10}},
	})
}

// twoSocketMachine has two packages of two CPUs, with one NUMA node each: cpus
// 0-1 in package 0 and node 0, cpus 2-3 in package 1 and node 1.
func twoSocketMachine(t *testing.T) *hardware.Machine {
	t.Helper()
	return synthMachine(t, []synthNode{
		{cpus: "0-1", memKB: 1048576, distance: []int{10, 20}},
		{cpus: "2-3", memKB: 1048576, distance: []int{20, 10}},
	})
}
