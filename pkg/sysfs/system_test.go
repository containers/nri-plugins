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
	"slices"
	"testing"

	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// Looking up hardware which is not there has to return a nil interface, so that
// a caller's `== nil` check works.
//
// It used to return a nil pointer inside a non-nil interface, which is never
// equal to nil, so every such check was dead code and the call which followed it
// dereferenced the nil pointer. Several callers had one, believed it worked, and
// crashed anyway; the topology-aware policy looks up node and socket ids taken
// straight out of a container's topology hints, so a hint naming hardware the
// machine does not have was enough to bring the plugin down.

// absentID is an id no machine has.
const absentID = 1 << 20

// discoverSample returns a System discovered from one of the recorded trees,
// skipping the test if they have not been unpacked.
func discoverSample(t *testing.T, name string) sysfs.System {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	root := path.Join(cwd, "testdata", name)
	if _, err := os.Stat(path.Join(root, "sys")); err != nil {
		t.Skipf("recorded tree %s is not unpacked: run ./test-setup.sh", name)
	}

	sys, err := sysfs.DiscoverSystemAt(path.Join(root, "sys"))
	if err != nil {
		t.Fatalf("failed to discover %s: %v", name, err)
	}

	return sys
}

func TestAbsentLookupsReturnNil(t *testing.T) {
	for _, name := range []string{"sample1", "sample2", "sample3"} {
		t.Run(name, func(t *testing.T) {
			sys := discoverSample(t, name)

			if cpu := sys.CPU(absentID); cpu != nil {
				t.Errorf("CPU(%d) = %v, want nil", absentID, cpu)
			}
			if node := sys.Node(absentID); node != nil {
				t.Errorf("Node(%d) = %v, want nil", absentID, node)
			}
			if pkg := sys.Package(absentID); pkg != nil {
				t.Errorf("Package(%d) = %v, want nil", absentID, pkg)
			}

			// negative ids are absent too, and used to panic just the same
			if cpu := sys.CPU(-1); cpu != nil {
				t.Errorf("CPU(-1) = %v, want nil", cpu)
			}
			if node := sys.Node(-1); node != nil {
				t.Errorf("Node(-1) = %v, want nil", node)
			}
			if pkg := sys.Package(-1); pkg != nil {
				t.Errorf("Package(-1) = %v, want nil", pkg)
			}

			// the ids the machine does have still resolve
			for _, id := range sys.CPUIDs() {
				if sys.CPU(id) == nil {
					t.Errorf("CPU(%d) = nil for a CPU the machine has", id)
				}
			}
			for _, id := range sys.NodeIDs() {
				if sys.Node(id) == nil {
					t.Errorf("Node(%d) = nil for a node the machine has", id)
				}
			}
			for _, id := range sys.PackageIDs() {
				if sys.Package(id) == nil {
					t.Errorf("Package(%d) = nil for a package the machine has", id)
				}
			}
		})
	}
}

// TestNodeDistanceOfAbsentNode checks that asking the distance from a node which
// is not there answers instead of panicking, using the -1 which
// Node.DistanceFrom already returns for an unknown destination.
func TestNodeDistanceOfAbsentNode(t *testing.T) {
	sys := discoverSample(t, "sample2")

	if got := sys.NodeDistance(absentID, 0); got != -1 {
		t.Errorf("NodeDistance(absent, 0) = %d, want -1", got)
	}
	if got := sys.NodeDistance(-1, 0); got != -1 {
		t.Errorf("NodeDistance(-1, 0) = %d, want -1", got)
	}

	// an absent destination was already handled, and still is
	nodes := sys.NodeIDs()
	if len(nodes) == 0 {
		t.Skip("no NUMA nodes")
	}
	if got := sys.NodeDistance(nodes[0], absentID); got != -1 {
		t.Errorf("NodeDistance(%d, absent) = %d, want -1", nodes[0], got)
	}

	// and the real distances are unchanged
	for _, from := range nodes {
		for _, to := range nodes {
			if got := sys.NodeDistance(from, to); got < 0 {
				t.Errorf("NodeDistance(%d, %d) = %d, want a real distance",
					from, to, got)
			}
		}
	}
}

// TestSetHelpersTolerateAbsentCPUs checks the set-to-set helpers when handed a
// CPU the machine does not have. SingleThreadForCPUs used to panic; the others
// already skipped.
func TestSetHelpersTolerateAbsentCPUs(t *testing.T) {
	sys := discoverSample(t, "sample1")

	online := sys.OnlineCPUs()
	if online.IsEmpty() {
		t.Skip("no online CPUs")
	}

	for _, tc := range []struct {
		name string
		cpus cpuset.CPUSet
		want cpuset.CPUSet
	}{
		{
			name: "only-absent",
			cpus: cpuset.New(absentID, absentID+1),
			// nothing is known about them, so they come back as themselves
			want: cpuset.New(absentID, absentID+1),
		},
		{
			name: "absent-mixed-with-real",
			cpus: online.Union(cpuset.New(absentID)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sys.SingleThreadForCPUs(tc.cpus)

			// whatever it returns has to be a subset of what it was given: the
			// helper picks CPUs out, it does not invent them
			if !got.IsSubsetOf(tc.cpus) {
				t.Errorf("SingleThreadForCPUs(%s) = %s, which is not a subset",
					tc.cpus, got)
			}
			if tc.want.Size() > 0 && !got.Equals(tc.want) {
				t.Errorf("SingleThreadForCPUs(%s) = %s, want %s",
					tc.cpus, got, tc.want)
			}

			// the absent CPU must not have displaced a real one: every core of
			// the real part is still represented
			real := sys.SingleThreadForCPUs(tc.cpus.Intersection(online))
			if !real.IsSubsetOf(got) {
				t.Errorf("SingleThreadForCPUs(%s) = %s dropped %s",
					tc.cpus, got, real.Difference(got))
			}
		})
	}

	// the ones which already checked, for completeness
	absent := cpuset.New(absentID)
	if got := sys.AllThreadsForCPUs(absent); !got.IsEmpty() {
		t.Errorf("AllThreadsForCPUs(absent) = %s, want empty", got)
	}
	if got := sys.AllCPUsSharingNthLevelCacheWithCPUs(2, absent); !got.IsEmpty() {
		t.Errorf("AllCPUsSharingNthLevelCacheWithCPUs(2, absent) = %s, want empty", got)
	}
	if got := sys.IDSetForCPUs(absent, func(c sysfs.CPU) int {
		return c.PackageID()
	}); len(got) != 0 {
		t.Errorf("IDSetForCPUs(absent) = %v, want empty", got.SortedMembers())
	}
}

// TestNodeHintToCPUsIgnoresAbsentNodes is the case which brought the plugin down:
// a container's topology hint naming a NUMA node the machine does not have.
func TestNodeHintToCPUsIgnoresAbsentNodes(t *testing.T) {
	sys := discoverSample(t, "sample1")

	for _, tc := range []struct {
		hint string
		want string
	}{
		{hint: "", want: ""},
		{hint: "0", want: sys.Node(0).CPUSet().Intersection(sys.OnlineCPUs()).String()},
		{hint: "1048576", want: ""},
		{hint: "0,1048576", want: sys.Node(0).CPUSet().Intersection(sys.OnlineCPUs()).String()},
		{hint: "not a cpuset", want: ""},
	} {
		t.Run(tc.hint, func(t *testing.T) {
			if got := sys.NodeHintToCPUs(tc.hint); got != tc.want {
				t.Errorf("NodeHintToCPUs(%q) = %q, want %q", tc.hint, got, tc.want)
			}
		})
	}
}

// TestRecordedPCIeRoots discovers the PCIe roots of a recorded two-socket
// machine with sub-NUMA clustering. Each root reports the CPUs of one NUMA
// node only, and must still be given to the whole package.
func TestRecordedPCIeRoots(t *testing.T) {
	sys := discoverSample(t, "sample3")

	for id, want := range map[idset.ID][]string{
		0: {"pci0000:00", "pci0000:17", "pci0000:3a", "pci0000:5d"},
		1: {"pci0000:80", "pci0000:85", "pci0000:ae", "pci0000:d7"},
	} {
		if got := sys.Package(id).PCIeRoots(); !slices.Equal(got, want) {
			t.Errorf("package %d: got PCIe roots %v, want %v", id, got, want)
		}
	}
}
