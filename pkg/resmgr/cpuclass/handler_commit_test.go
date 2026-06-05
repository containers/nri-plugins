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
	"sync"
	"testing"

	idset "github.com/intel/goresctrl/pkg/utils"

	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/cpufreq"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/cpuidle"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/types"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/uncorefreq"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// dieFakePackage extends the package fake with die support so the
// uncore writer can enumerate (pkg, die) tuples.
type dieFakePackage struct {
	sysfs.CPUPackage
	id      idset.ID
	cpus    cpuset.CPUSet
	dies    []idset.ID
	dieCpus map[idset.ID]cpuset.CPUSet
}

func (p *dieFakePackage) ID() idset.ID                       { return p.id }
func (p *dieFakePackage) CPUSet() cpuset.CPUSet              { return p.cpus }
func (p *dieFakePackage) DieIDs() []idset.ID                 { return p.dies }
func (p *dieFakePackage) DieCPUSet(d idset.ID) cpuset.CPUSet { return p.dieCpus[d] }

// dieFakeCPU augments the cpu fake with package id.
type dieFakeCPU struct {
	sysfs.CPU
	id  idset.ID
	pkg idset.ID
}

func (c *dieFakeCPU) ID() idset.ID        { return c.id }
func (c *dieFakeCPU) PackageID() idset.ID { return c.pkg }

// dieFakeSys is the minimum sysfs.System surface used by the
// uncore writer (Package, CPU, PackageIDs, DieIDs, DieCPUSet).
// Unimplemented methods panic via the embedded nil interface.
type dieFakeSys struct {
	sysfs.System
	packages map[idset.ID]*dieFakePackage
	cpuPkg   map[int]idset.ID
}

func (s *dieFakeSys) PackageIDs() []idset.ID {
	ids := make([]idset.ID, 0, len(s.packages))
	for id := range s.packages {
		ids = append(ids, id)
	}
	return ids
}

func (s *dieFakeSys) Package(id idset.ID) sysfs.CPUPackage {
	if p, ok := s.packages[id]; ok {
		return p
	}
	return nil
}

func (s *dieFakeSys) CPU(id idset.ID) sysfs.CPU {
	pkg, ok := s.cpuPkg[int(id)]
	if !ok {
		return nil
	}
	return &dieFakeCPU{id: id, pkg: pkg}
}

// dieFakeCpu specifies the (pkg, die) location of a single CPU when
// building a dieFakeSys.
type dieFakeCpu struct {
	pkg int
	die int
}

// newDieFakeSys builds a dieFakeSys from a map cpu -> (pkg, die).
func newDieFakeSys(cpus map[int]dieFakeCpu) *dieFakeSys {
	pkgs := map[idset.ID]*dieFakePackage{}
	cpuPkg := map[int]idset.ID{}
	type pkgDieKey struct{ pkg, die int }
	dieCpus := map[pkgDieKey]cpuset.CPUSet{}
	pkgCpus := map[int]cpuset.CPUSet{}
	pkgDies := map[int]map[int]bool{}
	for cpu, loc := range cpus {
		cpuPkg[cpu] = idset.ID(loc.pkg)
		pkgCpus[loc.pkg] = pkgCpus[loc.pkg].Union(cpuset.New(cpu))
		k := pkgDieKey(loc)
		dieCpus[k] = dieCpus[k].Union(cpuset.New(cpu))
		if pkgDies[loc.pkg] == nil {
			pkgDies[loc.pkg] = map[int]bool{}
		}
		pkgDies[loc.pkg][loc.die] = true
	}
	for pkg, dies := range pkgDies {
		dList := make([]idset.ID, 0, len(dies))
		for d := range dies {
			dList = append(dList, idset.ID(d))
		}
		dc := map[idset.ID]cpuset.CPUSet{}
		for d := range dies {
			dc[idset.ID(d)] = dieCpus[pkgDieKey{pkg, d}]
		}
		pkgs[idset.ID(pkg)] = &dieFakePackage{
			id:      idset.ID(pkg),
			cpus:    pkgCpus[pkg],
			dies:    dList,
			dieCpus: dc,
		}
	}
	return &dieFakeSys{packages: pkgs, cpuPkg: cpuPkg}
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
// topology (callers may set h.sys), and the recording writers
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
	sys := newDieFakeSys(map[int]dieFakeCpu{
		0: {pkg: 0, die: 0},
		1: {pkg: 0, die: 0},
	})
	h, r := newBareHandler()
	h.sys = sys
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
	sys := newDieFakeSys(map[int]dieFakeCpu{
		0: {pkg: 0, die: 0},
		1: {pkg: 0, die: 0},
	})
	h, r := newBareHandler()
	h.sys = sys
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
	sys := newDieFakeSys(map[int]dieFakeCpu{
		0: {pkg: 0, die: 0},
		1: {pkg: 0, die: 0},
	})
	h, r := newBareHandler()
	h.sys = sys
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
