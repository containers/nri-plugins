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

package main

import (
	"slices"
	"testing"
	"testing/fstest"

	"github.com/containers/nri-plugins/pkg/lib/hardware"
	idset "github.com/intel/goresctrl/pkg/utils"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// hbmCxlFS is the shape of the n6-hbm-cxl e2e topology, reduced: two packages
// with CPUs and DRAM, and two memory nodes with no CPUs of their own, one near
// each package.
//
//	node0  DRAM, cpus 0-1, package 0
//	node1  DRAM, cpus 2-3, package 1
//	node2  no cpus, 15 from node0, 30 from node1
//	node3  no cpus, 15 from node1, 30 from node0
func hbmCxlFS() fstest.MapFS {
	fsys := fstest.MapFS{
		"proc/meminfo":                    file("MemTotal: 8388608 kB\n"),
		"sys/devices/system/cpu/online":   file("0-3\n"),
		"sys/devices/system/cpu/present":  file("0-3\n"),
		"sys/devices/system/cpu/possible": file("0-3\n"),
	}
	for cpu, pkg := range map[int]int{0: 0, 1: 0, 2: 1, 3: 1} {
		dir := "sys/devices/system/cpu/cpu" + itoa(cpu) + "/topology"
		fsys[dir+"/physical_package_id"] = file(itoa(pkg) + "\n")
		fsys[dir+"/core_id"] = file(itoa(cpu) + "\n")
		fsys[dir+"/core_cpus_list"] = file(itoa(cpu) + "\n")
	}
	for node, spec := range map[int]struct {
		cpus     string
		distance string
	}{
		0: {"0-1", "10 20 15 30"},
		1: {"2-3", "20 10 30 15"},
		2: {"", "15 30 10 35"},
		3: {"", "30 15 35 10"},
	} {
		dir := "sys/devices/system/node/node" + itoa(node)
		fsys[dir+"/cpulist"] = file(spec.cpus + "\n")
		fsys[dir+"/distance"] = file(spec.distance + "\n")
		fsys[dir+"/meminfo"] = file("Node " + itoa(node) + " MemTotal: 2097152 kB\n")
	}
	return fsys
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A memory node with no CPUs of its own belongs to the package of the nearest
// node which has them. Reading the node's own package id instead yields -1, and
// "cpu-packages" then matches no such node at all -- which on a machine with HBM
// or CXL means the memory those policies exist to steer towards is never chosen.
func TestPackagesOfNode(t *testing.T) {
	m, err := hardware.Discover(hardware.WithFS(hbmCxlFS()))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	saved := machine
	machine = m
	defer func() { machine = saved }()

	for _, tc := range []struct {
		node idset.ID
		want []idset.ID
	}{
		{0, []idset.ID{0}},
		{1, []idset.ID{1}},
		{2, []idset.ID{0}}, // no CPUs, nearest is node0
		{3, []idset.ID{1}}, // no CPUs, nearest is node1
	} {
		got := packagesOfNode(tc.node)
		if !slices.Equal(got, tc.want) {
			t.Errorf("packagesOfNode(%d) = %v, want %v", tc.node, got, tc.want)
		}
	}

	if got := packagesOfNode(1 << 20); got != nil {
		t.Errorf("packagesOfNode(absent) = %v, want nil", got)
	}
}
