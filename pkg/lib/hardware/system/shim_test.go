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

// The parts of the drop-in the differential test cannot reach: the write paths,
// which nothing in the tree calls and which need a writable sysfs; the pieces
// which do not depend on a discovered machine; and FromMachine.

package system_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/pkg/lib/hardware"
	"github.com/containers/nri-plugins/pkg/lib/hardware/system"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// TestEquivalentEPPParsing checks the EPP names round-trip the same way in both.
func TestEquivalentEPPParsing(t *testing.T) {
	for _, name := range []string{
		"performance", "balance_performance", "balance_power", "power",
		"", "nonsense",
	} {
		old := sysfs.EPPFromString(name)
		new := system.EPPFromString(name)
		if int(old) != int(new) {
			t.Errorf("EPPFromString(%q): pkg/sysfs %d, drop-in %d", name, old, new)
		}
		if old.String() != new.String() {
			t.Errorf("EPPFromString(%q).String(): pkg/sysfs %q, drop-in %q",
				name, old.String(), new.String())
		}
	}

	// every value's name round-trips
	for e := 0; e <= int(system.EPPUnknown); e++ {
		name := system.EPP(e).String()
		if name == "" {
			continue
		}
		if got := system.EPPFromString(name); int(got) != e {
			t.Errorf("EPP(%d).String() = %q parses back as %d", e, name, got)
		}
	}
}

// TestEquivalentFilterCombinators checks the And/Or/Not combinators, which the
// differential test does not reach because no caller uses them.
func TestEquivalentFilterCombinators(t *testing.T) {
	p := discoverBoth(t, trees[0])
	ids := p.old.NodeIDs()

	cases := []struct {
		name string
		old  sysfs.NodeFilter
		new  system.NodeFilter
	}{
		{
			name: "And(HasMemory, HasLocalCPUs)",
			old:  sysfs.NodeFilterAnd(sysfs.NodeHasMemory, sysfs.NodeHasLocalCPUs),
			new:  system.NodeFilterAnd(system.NodeHasMemory, system.NodeHasLocalCPUs),
		},
		{
			name: "Or(PMEM, HBM)",
			old:  sysfs.NodeFilterOr(sysfs.NodeOfPMEMType, sysfs.NodeOfHBMType),
			new:  system.NodeFilterOr(system.NodeOfPMEMType, system.NodeOfHBMType),
		},
		{
			name: "Not(HasLocalCPUs)",
			old:  sysfs.NodeFilterNot(sysfs.NodeHasLocalCPUs),
			new:  system.NodeFilterNot(system.NodeHasLocalCPUs),
		},
		{
			name: "And()",
			old:  sysfs.NodeFilterAnd(),
			new:  system.NodeFilterAnd(),
		},
		{
			name: "Or()",
			old:  sysfs.NodeFilterOr(),
			new:  system.NodeFilterOr(),
		},
	}

	for _, tc := range cases {
		eqIDSet(t, "FilterNodes("+tc.name+")",
			p.old.FilterNodes(ids, tc.old), p.new.FilterNodes(ids, tc.new))
	}

	// NodeOfType, for each type
	for _, ty := range []int{
		int(system.MemoryTypeDRAM), int(system.MemoryTypePMEM), int(system.MemoryTypeHBM),
	} {
		eqIDSet(t, "FilterNodes(NodeOfType)",
			p.old.FilterNodes(ids, sysfs.NodeOfType(sysfs.MemoryType(ty))),
			p.new.FilterNodes(ids, system.NodeOfType(system.MemoryType(ty))))
	}
}

// TestEquivalentUtilities checks the helpers which only live in pkg/sysfs by
// accident and which the drop-in repeats so that an import swap is complete.
func TestEquivalentUtilities(t *testing.T) {
	if got, want := system.GetMemoryCapacity(), sysfs.GetMemoryCapacity(); got != want {
		t.Errorf("GetMemoryCapacity: pkg/sysfs %d, drop-in %d", want, got)
	}

	cpus := mustParse(t, "0-3,8")
	eqIDSet(t, "IDSetFromCPUSet",
		sysfs.IDSetFromCPUSet(cpus), system.IDSetFromCPUSet(cpus))
	eqCPUSet(t, "CPUSetFromIDSet",
		sysfs.CPUSetFromIDSet(idset.NewIDSet(0, 1, 2, 3, 8)),
		system.CPUSetFromIDSet(idset.NewIDSet(0, 1, 2, 3, 8)))

	// ParseFileEntries, over /proc/meminfo as the cgroup code uses it
	pick := func(line string) (string, string, error) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return "", "", nil
		}
		return fields[0], fields[1], nil
	}

	var oldTotal, newTotal uint64
	oerr := sysfs.ParseFileEntries("/proc/meminfo",
		map[string]any{"MemTotal:": &oldTotal}, pick)
	nerr := system.ParseFileEntries("/proc/meminfo",
		map[string]any{"MemTotal:": &newTotal}, pick)

	if (oerr == nil) != (nerr == nil) {
		t.Errorf("ParseFileEntries: errors differ: %v vs %v", oerr, nerr)
	}
	if oerr == nil && oldTotal != newTotal {
		t.Errorf("ParseFileEntries MemTotal: %d vs %d", oldTotal, newTotal)
	}

	// a file which is not there fails in both
	oerr = sysfs.ParseFileEntries("/nonexistent", map[string]any{}, pick)
	nerr = system.ParseFileEntries("/nonexistent", map[string]any{}, pick)
	if (oerr == nil) != (nerr == nil) {
		t.Errorf("ParseFileEntries of a missing file: %v vs %v", oerr, nerr)
	}
}

// TestFromMachine checks the seam a caller which already has a Machine uses.
func TestFromMachine(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", trees[0]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sys")); err != nil {
		t.Skipf("recorded tree %s is not unpacked: run ./test-setup.sh", trees[0])
	}

	m, err := hardware.Discover(hardware.WithRoot(root))
	if err != nil {
		t.Fatalf("hardware.Discover: %v", err)
	}

	wrapped := system.FromMachine(m)
	direct, err := system.DiscoverSystemAt(filepath.Join(root, "sys"))
	if err != nil {
		t.Fatalf("DiscoverSystemAt: %v", err)
	}

	eqCPUSet(t, "FromMachine CPUSet", direct.CPUSet(), wrapped.CPUSet())
	eqIDs(t, "FromMachine NodeIDs", direct.NodeIDs(), wrapped.NodeIDs())
	eqIDs(t, "FromMachine PackageIDs", direct.PackageIDs(), wrapped.PackageIDs())

	// Discover on an existing System is a no-op which reports success, as
	// nothing calls it with anything the constructor did not already read
	if err := wrapped.Discover(system.DiscoverAll); err != nil {
		t.Errorf("Discover on an existing System: %v", err)
	}

	// SST is probed for a wrapped Machine too, so that this seam yields a System
	// which is complete in the same way the discovering constructors do. Compare
	// the two rather than asserting absence, so this holds on a machine with SST
	// as well as on one without.
	if (wrapped.Sst() == nil) != (direct.Sst() == nil) {
		t.Errorf("SST platform presence differs: FromMachine %v, discovered %v",
			wrapped.Sst() != nil, direct.Sst() != nil)
	}
	cpu0 := m.CPUIDs()[0]
	if got, want := wrapped.CPU(cpu0).SstClos(), direct.CPU(cpu0).SstClos(); got != want {
		t.Errorf("SstClos = %d, want %d", got, want)
	}
	for _, id := range wrapped.PackageIDs() {
		w, d := wrapped.Package(id).SstInfo(), direct.Package(id).SstInfo()
		if (w == nil) != (d == nil) {
			t.Errorf("package#%d SST info presence differs: FromMachine %v, "+
				"discovered %v", id, w != nil, d != nil)
		}
	}
}

// TestWritePaths exercises SetCpusOnline, SetCPUFrequencyLimits and
// CPU.SetFrequencyLimits, which nothing in the tree calls and which the
// differential test cannot run against a read-only recorded tree.
//
// It copies a recorded tree somewhere writable, adds the attributes the writes
// touch, and checks what lands in them.
func TestWritePaths(t *testing.T) {
	src, err := filepath.Abs(filepath.Join("testdata", trees[0]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "sys")); err != nil {
		t.Skipf("recorded tree %s is not unpacked: run ./test-setup.sh", trees[0])
	}

	root := t.TempDir()
	if out, err := runCp(src+"/sys", root); err != nil {
		t.Skipf("cannot copy the recorded tree: %v: %s", err, out)
	}

	sys, err := system.DiscoverSystemAt(filepath.Join(root, "sys"))
	if err != nil {
		t.Fatalf("DiscoverSystemAt: %v", err)
	}

	cpus := sys.CPUIDs()
	if len(cpus) < 2 {
		t.Skip("need at least two CPUs")
	}

	// the attributes the writes touch, which a recorded tree does not have
	cpuDir := filepath.Join(root, "sys", "devices", "system", "cpu",
		"cpu"+itoa(cpus[1]))
	freqDir := filepath.Join(cpuDir, "cpufreq")
	if err := os.MkdirAll(freqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		filepath.Join(cpuDir, "online"):            "1\n",
		filepath.Join(freqDir, "scaling_min_freq"): "0000000\n",
		filepath.Join(freqDir, "scaling_max_freq"): "0000000\n",
	} {
		if err := os.WriteFile(name, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("SetCpusOnline", func(t *testing.T) {
		changed, err := sys.SetCpusOnline(false, idset.NewIDSet(cpus[1]))
		if err != nil {
			t.Fatalf("SetCpusOnline: %v", err)
		}
		if !changed.Has(cpus[1]) {
			t.Errorf("cpu%d is not among the changed CPUs %v",
				cpus[1], changed.SortedMembers())
		}
		if got := readFile(t, filepath.Join(cpuDir, "online")); got != "0\n" {
			t.Errorf("online holds %q, want %q", got, "0\n")
		}

		// cpu0 is never taken offline, as pkg/sysfs also refuses
		changed, err = sys.SetCpusOnline(false, idset.NewIDSet(0))
		if err != nil {
			t.Fatalf("SetCpusOnline(cpu0): %v", err)
		}
		if changed.Has(0) {
			t.Error("cpu0 was taken offline")
		}
	})

	t.Run("SetFrequencyLimits", func(t *testing.T) {
		c := sys.CPU(cpus[1])
		if c.FrequencyRange().Min == 0 {
			t.Skip("the recorded tree has no cpufreq range to clamp against")
		}

		if err := c.SetFrequencyLimits(1_000_000, 9_000_000_000); err != nil {
			t.Fatalf("SetFrequencyLimits: %v", err)
		}

		// the values are clamped to the range cpufreq reports
		freq := c.FrequencyRange()
		if got, want := readFile(t, filepath.Join(freqDir, "scaling_max_freq")),
			itoa(int(freq.Max))+"\n"; got != want {
			t.Errorf("scaling_max_freq holds %q, want %q (clamped)", got, want)
		}
	})

	t.Run("SetCPUFrequencyLimits", func(t *testing.T) {
		// the whole-machine form, which walks every CPU; the ones without the
		// attributes fail, so pass only the one which has them
		err := sys.SetCPUFrequencyLimits(1_000_000, 2_000_000,
			idset.NewIDSet(cpus[1]))
		if err != nil {
			t.Fatalf("SetCPUFrequencyLimits: %v", err)
		}
	})

	// A System from FromMachine writes to the machine's own tree. It has no root
	// of its own to write to, so if it ever built one from a remembered path
	// instead of asking the machine, a write here would land in the real /sys.
	t.Run("FromMachine writes to the machine's tree", func(t *testing.T) {
		m, err := hardware.Discover(hardware.WithRoot(root))
		if err != nil {
			t.Fatalf("hardware.Discover: %v", err)
		}

		// The machine reads who is online from the cpu-level "online" list, which
		// the earlier subtest did not touch, so it still sees cpu1 as online and
		// taking it offline is a change it will act on.
		online := filepath.Join(cpuDir, "online")
		if err := os.WriteFile(online, []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		changed, err := system.FromMachine(m).SetCpusOnline(false,
			idset.NewIDSet(cpus[1]))
		if err != nil {
			t.Fatalf("SetCpusOnline: %v", err)
		}
		if !changed.Has(cpus[1]) {
			t.Errorf("cpu%d is not among the changed CPUs %v",
				cpus[1], changed.SortedMembers())
		}
		if got := readFile(t, online); got != "0\n" {
			t.Errorf("online under the machine's root holds %q, want %q", got, "0\n")
		}
	})
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	blob, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}

// runCp copies a directory tree, since Go has no library call for it and the
// write tests need a writable copy of a recorded one.
func runCp(from, to string) (string, error) {
	out, err := exec.Command("cp", "-a", from, to).CombinedOutput()
	return string(out), err
}

// mustParse parses a cpuset or fails the test.
func mustParse(t *testing.T, s string) cpuset.CPUSet {
	t.Helper()
	cpus, err := cpuset.Parse(s)
	if err != nil {
		t.Fatalf("cpuset.Parse(%q): %v", s, err)
	}
	return cpus
}
