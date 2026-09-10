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

package cpuclass

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/containers/nri-plugins/pkg/lib/hardware"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/cpufreq"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/cpuidle"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/types"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/uncorefreq"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// dieFakeCpu specifies the (pkg, die) location of a single CPU when building a
// machine for these tests.
type dieFakeCpu struct {
	pkg int
	die int
}

// newDieMachine returns a machine with the given cpu -> (pkg, die) layout.
//
// Handler takes a *hardware.Machine, which is a concrete type and cannot be
// faked, so the layout is written out as the kernel would present it in sysfs and
// read back through real discovery. One NUMA node holds everything: the uncore
// writer cares about packages and dies only.
func newDieMachine(t *testing.T, cpus map[int]dieFakeCpu) *hardware.Machine {
	t.Helper()

	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	ids := make([]int, 0, len(cpus))
	for cpu := range cpus {
		ids = append(ids, cpu)
	}
	sort.Ints(ids)

	all := cpuset.New(ids...).String()
	fsys := fstest.MapFS{
		"proc/meminfo":                           file("MemTotal: 1048576 kB\n"),
		"sys/devices/system/cpu/online":          file(all + "\n"),
		"sys/devices/system/cpu/present":         file(all + "\n"),
		"sys/devices/system/cpu/possible":        file(all + "\n"),
		"sys/devices/system/node/node0/cpulist":  file(all + "\n"),
		"sys/devices/system/node/node0/distance": file("10\n"),
		"sys/devices/system/node/node0/meminfo":  file("Node 0 MemTotal: 1048576 kB\n"),
	}
	for cpu, loc := range cpus {
		dir := fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology", cpu)
		fsys[dir+"/physical_package_id"] = file(fmt.Sprintf("%d\n", loc.pkg))
		fsys[dir+"/die_id"] = file(fmt.Sprintf("%d\n", loc.die))
		fsys[dir+"/core_id"] = file(fmt.Sprintf("%d\n", cpu))
		fsys[dir+"/core_cpus_list"] = file(fmt.Sprintf("%d\n", cpu))
	}

	m, err := hardware.Discover(hardware.WithFS(fsys))
	if err != nil {
		t.Fatalf("failed to discover the test machine: %v", err)
	}
	return m
}

// recordingWriters captures the per-CPU and per-die writes issued by
// Commit() so tests can assert exactly what was programmed.
type recordingWriters struct {
	mu      sync.Mutex
	minF    map[int]int
	maxF    map[int]int
	gov     map[int]string
	minU    map[uncorefreq.DieKey]int
	maxU    map[uncorefreq.DieKey]int
	minCnt  int
	maxCnt  int
	govCnt  int
	uMinCnt int
	uMaxCnt int
}

func newRecordingWriters() *recordingWriters {
	return &recordingWriters{
		minF: map[int]int{},
		maxF: map[int]int{},
		gov:  map[int]string{},
		minU: map[uncorefreq.DieKey]int{},
		maxU: map[uncorefreq.DieKey]int{},
	}
}

// installOn replaces the cpufreq and uncore writers of h with
// in-memory recorders. The cpuidle writer is replaced by a no-op so
// tests do not need a real cstates handle.
func (r *recordingWriters) installOn(h *Handler) {
	h.freqWriter = cpufreq.NewWriter(cpufreq.Hooks{
		SetMin: func(cpu, freq int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.minF[cpu] = freq
			r.minCnt++
			return nil
		},
		SetMax: func(cpu, freq int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.maxF[cpu] = freq
			r.maxCnt++
			return nil
		},
		SetGov: func(cpu int, g string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.gov[cpu] = g
			r.govCnt++
			return nil
		},
	})
	h.uncoreWriter = uncorefreq.NewWriter(uncorefreq.Hooks{
		SetMin: func(pkg, die, freq int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.minU[uncorefreq.DieKey{Pkg: pkg, Die: die}] = freq
			r.uMinCnt++
			return nil
		},
		SetMax: func(pkg, die, freq int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.maxU[uncorefreq.DieKey{Pkg: pkg, Die: die}] = freq
			r.uMaxCnt++
			return nil
		},
	})
	h.idleWriter = cpuidle.NewWriter(cpuidle.Hooks{})
}

// newBareHandler returns a Handler with empty state, no sysfs
// topology (callers may set h.machine), and the recording writers
// installed. The cpuidle writer is left in a state where Enforce
// will return early because no class has DisabledCstates.
func newBareHandler() (*Handler, *recordingWriters) {
	h := &Handler{
		defs:      map[string]types.ClassDef{},
		cpuClass:  map[int]string{},
		dirtyCPUs: map[int]bool{},
	}
	r := newRecordingWriters()
	r.installOn(h)
	return h, r
}

// TestCommitIdempotentCpufreq verifies that a second Commit() with
// no state change re-issues zero sysfs writes.
func TestCommitIdempotentCpufreq(t *testing.T) {
	h, r := newBareHandler()
	h.SetClassDef("hp@d0", types.ClassDef{MinFreq: 800_000, MaxFreq: 4_600_000, FreqGovernor: "performance"})
	h.AssignCPUs("hp@d0", []int{0, 1})
	if err := h.Commit(); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if r.minCnt != 2 || r.maxCnt != 2 || r.govCnt != 2 {
		t.Fatalf("expected 2 of each write, got min=%d max=%d gov=%d", r.minCnt, r.maxCnt, r.govCnt)
	}
	if err := h.Commit(); err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if r.minCnt != 2 || r.maxCnt != 2 || r.govCnt != 2 {
		t.Fatalf("second Commit should be no-op, got min=%d max=%d gov=%d", r.minCnt, r.maxCnt, r.govCnt)
	}
}

// TestClassDefChangeDirtiesAssignedCpus verifies that updating a
// class definition reprograms the CPUs already assigned to that
// class on the next Commit, without requiring a re-assign.
func TestClassDefChangeDirtiesAssignedCpus(t *testing.T) {
	h, r := newBareHandler()
	h.SetClassDef("hp@d0", types.ClassDef{MinFreq: 800_000, MaxFreq: 4_000_000})
	h.AssignCPUs("hp@d0", []int{0, 1})
	if err := h.Commit(); err != nil {
		t.Fatalf("Commit#1: %v", err)
	}
	h.SetClassDef("hp@d0", types.ClassDef{MinFreq: 800_000, MaxFreq: 4_600_000})
	if err := h.Commit(); err != nil {
		t.Fatalf("Commit#2: %v", err)
	}
	for _, cpu := range []int{0, 1} {
		if r.maxF[cpu] != 4_600_000 {
			t.Errorf("cpu%d max=%d, want 4_600_000", cpu, r.maxF[cpu])
		}
	}
}

// TestAssignToEmptyClassDoesNotWriteCpufreq verifies that moving a
// CPU to the empty class leaves the writers untouched.
func TestAssignToEmptyClassDoesNotWriteCpufreq(t *testing.T) {
	h, r := newBareHandler()
	h.SetClassDef("hp@d0", types.ClassDef{MinFreq: 800_000, MaxFreq: 4_000_000, FreqGovernor: "performance"})
	h.AssignCPUs("hp@d0", []int{0})
	if err := h.Commit(); err != nil {
		t.Fatalf("Commit#1: %v", err)
	}
	r.maxCnt, r.minCnt, r.govCnt = 0, 0, 0
	h.AssignCPUs("", []int{0})
	if err := h.Commit(); err != nil {
		t.Fatalf("Commit#2: %v", err)
	}
	if r.minCnt+r.maxCnt+r.govCnt != 0 {
		t.Errorf("empty class should not write to cpufreq, got min=%d max=%d gov=%d", r.minCnt, r.maxCnt, r.govCnt)
	}
}

// TestUncoreSkipBothZero verifies that a die with effective min=0
// and max=0 produces no uncore writes.
func TestUncoreSkipBothZero(t *testing.T) {
	m := newDieMachine(t, map[int]dieFakeCpu{
		0: {pkg: 0, die: 0},
		1: {pkg: 0, die: 0},
	})
	h, r := newBareHandler()
	h.machine = m
	h.SetClassDef("idle@d0", types.ClassDef{MinFreq: 800_000})
	h.AssignCPUs("idle@d0", []int{0, 1})
	if err := h.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if r.uMinCnt != 0 || r.uMaxCnt != 0 {
		t.Errorf("uncore should not be written when both limits are 0, got min=%d max=%d", r.uMinCnt, r.uMaxCnt)
	}
}

// TestUncoreMaxWinsAcrossClasses verifies the per-die max-wins
// reduction when multiple classes are active on the same die.
func TestUncoreMaxWinsAcrossClasses(t *testing.T) {
	m := newDieMachine(t, map[int]dieFakeCpu{
		0: {pkg: 0, die: 0},
		1: {pkg: 0, die: 0},
	})
	h, r := newBareHandler()
	h.machine = m
	h.SetClassDef("lo@d0", types.ClassDef{UncoreMinFreq: 800_000, UncoreMaxFreq: 1_500_000})
	h.SetClassDef("hi@d0", types.ClassDef{UncoreMinFreq: 1_200_000, UncoreMaxFreq: 2_400_000})
	h.AssignCPUs("lo@d0", []int{0})
	h.AssignCPUs("hi@d0", []int{1})
	if err := h.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	key := uncorefreq.DieKey{Pkg: 0, Die: 0}
	if got := r.maxU[key]; got != 2_400_000 {
		t.Errorf("uncore max = %d, want 2_400_000 (hi class wins)", got)
	}
	if got := r.minU[key]; got != 1_200_000 {
		t.Errorf("uncore min = %d, want 1_200_000 (hi class wins)", got)
	}
}

// TestUncoreRecomputesOnAssignmentChange verifies that removing the
// winner class from a die triggers a fresh write with the loser's
// (lower) values.
func TestUncoreRecomputesOnAssignmentChange(t *testing.T) {
	m := newDieMachine(t, map[int]dieFakeCpu{
		0: {pkg: 0, die: 0},
		1: {pkg: 0, die: 0},
	})
	h, r := newBareHandler()
	h.machine = m
	h.SetClassDef("lo@d0", types.ClassDef{UncoreMaxFreq: 1_500_000})
	h.SetClassDef("hi@d0", types.ClassDef{UncoreMaxFreq: 2_400_000})
	h.AssignCPUs("lo@d0", []int{0})
	h.AssignCPUs("hi@d0", []int{1})
	if err := h.Commit(); err != nil {
		t.Fatalf("Commit#1: %v", err)
	}
	h.AssignCPUs("lo@d0", []int{1})
	if err := h.Commit(); err != nil {
		t.Fatalf("Commit#2: %v", err)
	}
	if got := r.maxU[uncorefreq.DieKey{Pkg: 0, Die: 0}]; got != 1_500_000 {
		t.Errorf("uncore max after hi removed = %d, want 1_500_000", got)
	}
}

// TestDiesForCpus covers the (pkg, die) lookup across more than one of each,
// which the tests above do not: they all describe a single die. A CPU knows its
// own die, so this is also what says newDieMachine lays the topology out the way
// its callers describe it.
func TestDiesForCpus(t *testing.T) {
	m := newDieMachine(t, map[int]dieFakeCpu{
		0: {pkg: 0, die: 0},
		1: {pkg: 0, die: 0},
		2: {pkg: 0, die: 1},
		3: {pkg: 1, die: 0},
		4: {pkg: 1, die: 1},
	})

	for _, tc := range []struct {
		name string
		cpus []int
		want []uncorefreq.DieKey
	}{
		{"one die", []int{0, 1}, []uncorefreq.DieKey{{Pkg: 0, Die: 0}}},
		{"two dies of one package", []int{1, 2},
			[]uncorefreq.DieKey{{Pkg: 0, Die: 0}, {Pkg: 0, Die: 1}}},
		{"across packages", []int{0, 3},
			[]uncorefreq.DieKey{{Pkg: 0, Die: 0}, {Pkg: 1, Die: 0}}},
		{"every die", []int{0, 1, 2, 3, 4}, []uncorefreq.DieKey{
			{Pkg: 0, Die: 0}, {Pkg: 0, Die: 1}, {Pkg: 1, Die: 0}, {Pkg: 1, Die: 1},
		}},
		{"a CPU the machine does not have", []int{1 << 20}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpus := map[int]bool{}
			for _, cpu := range tc.cpus {
				cpus[cpu] = true
			}

			got := uncorefreq.DiesForCpus(m, cpus)
			if len(got) != len(tc.want) {
				t.Fatalf("DiesForCpus(%v) = %v, want %v", tc.cpus, got, tc.want)
			}
			for _, key := range tc.want {
				if !got[key] {
					t.Errorf("DiesForCpus(%v) = %v, missing %v", tc.cpus, got, key)
				}
			}
		})
	}
}
