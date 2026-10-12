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

package sysfs

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// addPCIeRoot adds the PCIe root name to the sysfs tree at root, with the
// given cpulistaffinity, or with none if affinity is empty.
func addPCIeRoot(t *testing.T, root, name, affinity string) {
	t.Helper()
	bus := filepath.Join(root, "devices", name, "pci_bus", name[len("pci"):])
	if err := os.MkdirAll(bus, 0o755); err != nil {
		t.Fatal(err)
	}
	if affinity == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(bus, "cpulistaffinity"), []byte(affinity+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// strideCPUs returns the CPUs first, first+stride, ... up to last.
func strideCPUs(first, stride, last int) cpuset.CPUSet {
	var cpus []int
	for cpu := first; cpu <= last; cpu += stride {
		cpus = append(cpus, cpu)
	}
	return cpuset.New(cpus...)
}

// TestDiscoverPCIeRoots uses the layout of a two-socket machine with
// sub-NUMA clustering: 88 CPUs, even ones on package 0 and odd ones on
// package 1, two NUMA nodes per package. Each root reports the CPUs of one
// NUMA node only, and must still be given to the whole package.
func TestDiscoverPCIeRoots(t *testing.T) {
	var (
		node0 = strideCPUs(0, 4, 84).String()
		node1 = strideCPUs(1, 4, 85).String()
		node2 = strideCPUs(2, 4, 86).String()
		node3 = strideCPUs(3, 4, 87).String()
	)

	root := t.TempDir()
	addPCIeRoot(t, root, "pci0000:00", node0)
	addPCIeRoot(t, root, "pci0000:17", node0)
	addPCIeRoot(t, root, "pci0000:3a", node0)
	addPCIeRoot(t, root, "pci0000:5d", node2)
	addPCIeRoot(t, root, "pci0000:80", node1)
	addPCIeRoot(t, root, "pci0000:85", node1)
	addPCIeRoot(t, root, "pci0000:ae", node1)
	addPCIeRoot(t, root, "pci0000:d7", node3)
	// Not on the machine: a root with no NUMA node, reporting all CPUs,
	// one reporting CPUs we do not know, and entries to skip.
	addPCIeRoot(t, root, "pci0000:f0", "0-87")
	addPCIeRoot(t, root, "pci0000:f8", "100-103")
	addPCIeRoot(t, root, "pci0000:40", "")    // no cpulistaffinity
	addPCIeRoot(t, root, "pci0000:c0", "x")   // unparsable
	addPCIeRoot(t, root, "pci-bogus0", "0-3") // not a root name

	sys := &system{
		Logger: log,
		path:   root,
		packages: map[idset.ID]*cpuPackage{
			0: {id: 0, cpus: IDSetFromCPUSet(strideCPUs(0, 2, 86))},
			1: {id: 1, cpus: IDSetFromCPUSet(strideCPUs(1, 2, 87))},
		},
	}
	// Twice, to check that discovering again does not add the roots again.
	sys.discoverPCIeRoots()
	sys.discoverPCIeRoots()

	for id, want := range map[idset.ID][]string{
		0: {"pci0000:00", "pci0000:17", "pci0000:3a", "pci0000:5d", "pci0000:f0"},
		1: {"pci0000:80", "pci0000:85", "pci0000:ae", "pci0000:d7", "pci0000:f0"},
	} {
		if got := sys.packages[id].PCIeRoots(); !slices.Equal(got, want) {
			t.Errorf("package %d: got PCIe roots %v, want %v", id, got, want)
		}
	}
}

func TestDiscoverPCIeRootsNone(t *testing.T) {
	sys := &system{
		Logger: log,
		path:   t.TempDir(),
		packages: map[idset.ID]*cpuPackage{
			0: {id: 0, cpus: IDSetFromCPUSet(cpuset.MustParse("0-15"))},
		},
	}
	sys.discoverPCIeRoots()
	if got := sys.packages[0].PCIeRoots(); len(got) != 0 {
		t.Errorf("got PCIe roots %v, want none", got)
	}
}
