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

package pct

import (
	"errors"
	"sort"
	"testing"

	gosst "github.com/intel/goresctrl/pkg/sst"
	idset "github.com/intel/goresctrl/pkg/utils"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/types"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

var errFakeSstNoClos = errors.New("fakeSst: no CLOS for CPU")

// --- minimal sysfs.System / CPUPackage / CPU fakes ------------------

// fakePackage implements sysfs.CPUPackage via an embedded nil
// interface. Methods not overridden here panic if called, which is
// the desired guardrail in unit tests.
type fakePackage struct {
	sysfs.CPUPackage
	id   idset.ID
	cpus cpuset.CPUSet
}

func (p *fakePackage) ID() idset.ID          { return p.id }
func (p *fakePackage) CPUSet() cpuset.CPUSet { return p.cpus }

// fakeCPU implements sysfs.CPU likewise.
type fakeCPU struct {
	sysfs.CPU
	id  idset.ID
	pkg idset.ID
}

func (c *fakeCPU) ID() idset.ID        { return c.id }
func (c *fakeCPU) PackageID() idset.ID { return c.pkg }

// fakeSys is a minimal Sys implementation built from package
// CPU maps.
type fakeSys struct {
	packageCpus map[idset.ID]cpuset.CPUSet // pkgID -> cpus
	cpuPkg      map[int]idset.ID           // cpu -> pkgID
}

func (s *fakeSys) PackageIDs() []idset.ID {
	ids := make([]idset.ID, 0, len(s.packageCpus))
	for id := range s.packageCpus {
		ids = append(ids, id)
	}
	return ids
}

func (s *fakeSys) Package(id idset.ID) sysfs.CPUPackage {
	cpus, ok := s.packageCpus[id]
	if !ok {
		return nil
	}
	return &fakePackage{id: id, cpus: cpus}
}

func (s *fakeSys) CPU(id idset.ID) sysfs.CPU {
	pkg, ok := s.cpuPkg[int(id)]
	if !ok {
		return nil
	}
	return &fakeCPU{id: id, pkg: pkg}
}

func (s *fakeSys) CPUIDs() []idset.ID { return nil }

// newTwoPackageFakeSys returns a fakeSys with two packages of 4 CPUs
// each: pkg0=0..3, pkg1=4..7.
func newTwoPackageFakeSys() *fakeSys {
	return &fakeSys{
		packageCpus: map[idset.ID]cpuset.CPUSet{
			0: cpuset.MustParse("0-3"),
			1: cpuset.MustParse("4-7"),
		},
		cpuPkg: map[int]idset.ID{
			0: 0, 1: 0, 2: 0, 3: 0,
			4: 1, 5: 1, 6: 1, 7: 1,
		},
	}
}

// --- minimal sst fake ------------------------------------------------

// fakeSst implements just the methods that Allocator.hints (and
// closCpus) actually call.
type fakeSst struct {
	supported bool
	cpuClos   map[int]int // cpu -> CLOS id
	maxHp     map[int]int // pkgID -> max HP CPUs (missing = "unknown")
	pkgCpus   map[int]cpuset.CPUSet
	// punits, when non-nil, overrides the synthesized one-punit-per-package
	// Punits() output. Use to exercise multi-punit-per-package layouts.
	punits []pctPunit
	// closCfg, when non-nil, drives GetClosConfig() responses.
	closCfg map[int]pctClosCfg
}

func (s *fakeSst) Supported() bool                    { return s.supported }
func (s *fakeSst) ClosCount() int                     { return 4 }
func (s *fakeSst) PackageIDs() []int                  { return nil }
func (s *fakeSst) CPUsOfPackage(int) []int            { return nil }
func (s *fakeSst) PrepareManagedMode() error          { return nil }
func (s *fakeSst) ConfigureClos(pctClosConfig) error  { return nil }
func (s *fakeSst) EnableCP() error                    { return nil }
func (s *fakeSst) AssociateCPUs([]pctClosAssoc) error { return nil }
func (s *fakeSst) GetCPUClosID(cpu int) (int, error) {
	if clos, ok := s.cpuClos[cpu]; ok {
		return clos, nil
	}
	// Return an error so closCpus skips this CPU rather than
	// treating it as "associated to CLOS 0 by default".
	return -1, errFakeSstNoClos
}

// Punits synthesizes one punit per package whose CPUs come from
// pkgCpus (or maxHp keys if pkgCpus is nil) with MaxHpCpus set
// from the maxHp map. PunitID is always 0 (single punit per pkg
// preserves the legacy per-package test semantics).
func (s *fakeSst) Punits() []pctPunit {
	if s.punits != nil {
		out := make([]pctPunit, len(s.punits))
		copy(out, s.punits)
		return out
	}
	pkgIDs := map[int]struct{}{}
	for id := range s.pkgCpus {
		pkgIDs[id] = struct{}{}
	}
	for id := range s.maxHp {
		pkgIDs[id] = struct{}{}
	}
	out := make([]pctPunit, 0, len(pkgIDs))
	for id := range pkgIDs {
		cpus, ok := s.pkgCpus[id]
		if !ok {
			// Derive a default cpu range matching newTwoPackageFakeSys layout.
			switch id {
			case 0:
				cpus = cpuset.MustParse("0-3")
			case 1:
				cpus = cpuset.MustParse("4-7")
			}
		}
		out = append(out, pctPunit{
			PkgID:     id,
			PunitID:   0,
			CPUs:      cpus,
			MaxHpCpus: s.maxHp[id],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PkgID != out[j].PkgID {
			return out[i].PkgID < out[j].PkgID
		}
		return out[i].PunitID < out[j].PunitID
	})
	return out
}

func (s *fakeSst) GetClosConfig(closID int) (pctClosCfg, bool, error) {
	if c, ok := s.closCfg[closID]; ok {
		return c, true, nil
	}
	return pctClosCfg{}, false, nil
}

func (s *fakeSst) Shutdown() error { return nil }

func (s *fakeSst) TFStatus() (map[pctPunitID]bool, error) {
	// Tests do not care about SST-TF; report enabled everywhere.
	out := map[pctPunitID]bool{}
	for _, pu := range s.Punits() {
		out[pctPunitID{PkgID: pu.PkgID, PunitID: pu.PunitID}] = true
	}
	return out, nil
}

// --- helpers to construct a hand-wired Allocator -----------------

func newManagedPctForTest(t *testing.T, classes []*policyapi.CPUClass, plans map[string]*pctClassPlan,
	allowed cpuset.CPUSet, sys *fakeSys, sst *fakeSst) *Allocator {
	t.Helper()
	a := &Allocator{
		sys:         sys,
		sst:         sst,
		mode:        pctModeManaged,
		classByName: map[string]*policyapi.CPUClass{},
		classPlan:   plans,
		allowed:     allowed,
		hpUsed:      map[int]cpuset.CPUSet{},
		hpDRAUsed:   map[int]cpuset.CPUSet{},
		hpClasses:   map[string]bool{},
	}
	for _, cc := range classes {
		a.classByName[cc.Name] = cc
		if cc.PctPriority == "high" {
			a.hpClasses[cc.Name] = true
		}
	}
	pctTestWirePunits(a)
	return a
}

// pctTestWirePunits seeds a hand-built Allocator's punit caches
// from its sst's Punits(), intersected with allowed. It is the
// test-time equivalent of snapshotPunits() and lets struct-literal
// fixtures exercise the punit-keyed code paths.
func pctTestWirePunits(a *Allocator) {
	if a.punitByCpu == nil {
		a.punitByCpu = map[int]int{}
	}
	if a.hpClasses == nil {
		a.hpClasses = map[string]bool{}
	}
	if a.hpEligiblePunit == nil {
		a.hpEligiblePunit = map[int]bool{}
	}
	for name, cc := range a.classByName {
		if cc.PctPriority == "high" {
			a.hpClasses[name] = true
		}
	}
	pus := a.sst.Punits()
	a.punits = a.punits[:0]
	for _, pu := range pus {
		cpus := pu.CPUs
		if a.allowed.Size() > 0 {
			cpus = cpus.Intersection(a.allowed)
		}
		if cpus.IsEmpty() {
			continue
		}
		idx := len(a.punits)
		a.punits = append(a.punits, pctPunit{
			PkgID: pu.PkgID, PunitID: pu.PunitID,
			CPUs: cpus, MaxHpCpus: pu.MaxHpCpus,
			GuaranteedHpCpus: pu.GuaranteedHpCpus,
		})
		for _, c := range cpus.UnsortedList() {
			a.punitByCpu[c] = idx
		}
		// Default to HP-eligible so existing tests that don't
		// care about TF state keep working. Tests that exercise
		// HP-ineligibility set hpEligiblePunit explicitly after
		// calling this helper.
		a.hpEligiblePunit[idx] = true
	}
}

// --- hints() test suite ---------------------------------------------

// TestPctHintsNoClassNoOp covers the "no plan and not managed-with-HP"
// branch where hints() must return an empty types.AllocationHints.
func TestPctHintsNoClassNoOp(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{supported: true}

	// disabled allocator: hints must short-circuit to empty.
	a := &Allocator{sys: sys, sst: sst, mode: pctModeDisabled}
	got := a.Hints(types.AllocationIntent{ClassName: "anything", RequestedCount: 1})
	if len(got.Prefer) != 0 || len(got.Avoid) != 0 {
		t.Errorf("disabled mode: hints=%+v, want empty", got)
	}

	// managed mode with no HP class defined and an unknown
	// className: no prefer, no avoid.
	classes := []*policyapi.CPUClass{{Name: "lp", PctPriority: "low"}}
	// "lp" is configured but classIsHighPriority is false; still the
	// "anyHighPriorityClassDefined" gate must be false so no Avoid.
	a2 := newManagedPctForTest(t, classes,
		map[string]*pctClassPlan{"lp": {ClosID: 3}},
		cpuset.MustParse("0-7"), sys, sst)
	got = a2.Hints(types.AllocationIntent{ClassName: "unknown-class", RequestedCount: 1})
	if len(got.Avoid) != 0 {
		t.Errorf("no HP class: Avoid=%+v, want empty", got.Avoid)
	}
}

// TestPctHintsAssocOnlyPreferClosCpus covers the "explicit CLOS plan"
// branch in assoc-only mode: hints prefer free CPUs already
// associated to the class's CLOS, enabling bin packing.
func TestPctHintsAssocOnlyPreferClosCpus(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{
		supported: true,
		// cpus 1, 2 and 3 already on CLOS 1, others on default CLOS 0.
		cpuClos: map[int]int{1: 1, 2: 1, 3: 1},
	}
	a := &Allocator{
		sys:         sys,
		sst:         sst,
		mode:        pctModeAssocOnly,
		classByName: map[string]*policyapi.CPUClass{"c1": {Name: "c1"}},
		classPlan:   map[string]*pctClassPlan{"c1": {ClosID: 1}},
		allowed:     cpuset.MustParse("0-7"),
		hpUsed:      map[int]cpuset.CPUSet{},
	}
	pctTestWirePunits(a)
	got := a.Hints(types.AllocationIntent{
		ClassName: "c1",
		// cpu 1 is on CLOS 1 but already taken by someone
		// else, so it must not show up in the hint.
		FreeCpus:       cpuset.MustParse("2-7"),
		RequestedCount: 1,
	})
	if len(got.Prefer) != 1 {
		t.Fatalf("Prefer count = %d, want 1: got=%+v", len(got.Prefer), got)
	}
	if got.Prefer[0].Name != virtDevSstClosHint(1) {
		t.Errorf("Prefer[0].Name = %q, want %q", got.Prefer[0].Name, virtDevSstClosHint(1))
	}
	want := cpuset.MustParse("2-3")
	if !got.Prefer[0].Cpus[0].Equals(want) {
		t.Errorf("Prefer[0].Cpus = %v, want %s", got.Prefer[0].Cpus, want)
	}
	if len(got.Avoid) != 0 {
		t.Errorf("assoc-only mode must not emit Avoid hints: %+v", got.Avoid)
	}
}

// TestPctHintsHighPriorityReserveAndClosCpus covers the HP class
// branch: hints contain (a) free CPUs already on the HP CLOS for bin
// packing and (b) the HP-reserve preference (largest-room package).
func TestPctHintsHighPriorityReserveAndClosCpus(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{
		supported: true,
		// cpus 0 and 1 already on CLOS 0 (HP), cpu 0 in use.
		cpuClos: map[int]int{0: 0, 1: 0},
		// max_hp_cpus = 2 per package on both packages.
		maxHp: map[int]int{0: 2, 1: 2},
	}
	a := &Allocator{
		sys:  sys,
		sst:  sst,
		mode: pctModeManaged,
		classByName: map[string]*policyapi.CPUClass{
			"hp": {Name: "hp", PctPriority: "high"},
		},
		classPlan: map[string]*pctClassPlan{"hp": {ClosID: 0}},
		allowed:   cpuset.MustParse("0-7"),
		// pkg0 has 1 HP cpu already used (cpu 0).
		hpUsed: map[int]cpuset.CPUSet{0: cpuset.MustParse("0")},
	}
	pctTestWirePunits(a)

	// Free pool excludes the already-used cpu 0.
	free := cpuset.MustParse("1-7")
	got := a.Hints(types.AllocationIntent{
		ClassName:      "hp",
		CurrentCpus:    cpuset.New(),
		FreeCpus:       free,
		RequestedCount: 1,
	})

	// Expect two Prefer hints: free CLOS 0 members (cpu 1, as cpu 0
	// is already in use) and HP reserve (the package with more HP
	// room - pkg1, since pkg0 has 2-1=1 room left and pkg1 has
	// 2-0=2 room left).
	if len(got.Prefer) != 2 {
		t.Fatalf("Prefer count = %d, want 2: got=%+v", len(got.Prefer), got.Prefer)
	}
	if got.Prefer[0].Name != virtDevSstClosHint(0) {
		t.Errorf("Prefer[0].Name = %q, want %q", got.Prefer[0].Name, virtDevSstClosHint(0))
	}
	wantClos := cpuset.MustParse("1")
	if !got.Prefer[0].Cpus[0].Equals(wantClos) {
		t.Errorf("Prefer[0].Cpus = %v, want %s (cpu 0 on CLOS 0 but not free)",
			got.Prefer[0].Cpus, wantClos)
	}
	if got.Prefer[1].Name != virtDevSstHpReserveHint {
		t.Errorf("Prefer[1].Name = %q, want %q", got.Prefer[1].Name, virtDevSstHpReserveHint)
	}
	wantReserve := cpuset.MustParse("4-7")
	if !got.Prefer[1].Cpus[0].Equals(wantReserve) {
		t.Errorf("HP reserve = %v, want %s (largest-room package)", got.Prefer[1].Cpus, wantReserve)
	}
	// HP-class hints must NOT carry an Avoid (HP picks first).
	if len(got.Avoid) != 0 {
		t.Errorf("HP class: Avoid=%+v, want empty", got.Avoid)
	}
}

// TestPctHintsManagedNonHpAvoidsHpInUse covers the managed-mode
// non-HP-class branch: hints must Avoid CPUs on packages currently
// hosting HP-class CPUs, so non-HP classes do not steal HP turbo
// budget. THIS BRANCH IS NOT COVERED IN test19 e2e.
func TestPctHintsManagedNonHpAvoidsHpInUse(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{
		supported: true,
		cpuClos:   map[int]int{},
		maxHp:     map[int]int{0: 2, 1: 2},
	}
	a := &Allocator{
		sys:  sys,
		sst:  sst,
		mode: pctModeManaged,
		classByName: map[string]*policyapi.CPUClass{
			"hp": {Name: "hp", PctPriority: "high"},
			"lp": {Name: "lp", PctPriority: "low"},
		},
		classPlan: map[string]*pctClassPlan{
			"hp": {ClosID: 0},
			"lp": {ClosID: 3},
		},
		allowed: cpuset.MustParse("0-7"),
		// pkg0 hosts HP cpu 1.
		hpUsed: map[int]cpuset.CPUSet{0: cpuset.MustParse("1")},
	}
	pctTestWirePunits(a)
	got := a.Hints(types.AllocationIntent{
		ClassName:      "lp",
		FreeCpus:       cpuset.MustParse("2-7"),
		RequestedCount: 1,
	})

	// LP has a CLOS plan, so Prefer would include CLOS 3 - but only
	// if any free CPU is currently on CLOS 3. With none, classClosID
	// still matches but closCpus returns empty and the Prefer entry
	// is skipped rather than emitted as an empty CPU set. So
	// len(Prefer) == 0.
	if len(got.Prefer) != 0 {
		t.Errorf("Prefer = %+v, want empty (no LP CPUs currently on CLOS 3)", got.Prefer)
	}
	// Avoid must list pkg0's full CPU set (where HP is in use).
	if len(got.Avoid) != 1 {
		t.Fatalf("Avoid count = %d, want 1: got=%+v", len(got.Avoid), got.Avoid)
	}
	if got.Avoid[0].Name != virtDevSstHpInUseHint {
		t.Errorf("Avoid[0].Name = %q, want %q", got.Avoid[0].Name, virtDevSstHpInUseHint)
	}
	wantAvoid := cpuset.MustParse("0-3") // entire pkg0
	if !got.Avoid[0].Cpus[0].Equals(wantAvoid) {
		t.Errorf("Avoid[0].Cpus = %v, want %s (pkg0 == HP-in-use package)", got.Avoid[0].Cpus, wantAvoid)
	}
}

// TestPctHintsAllowedBoundsResults ensures that even with sst /
// hpUsed pointing at CPUs outside the allowed set, hints honor
// Allowed (via the handler-level intersectHints + pct-internal
// allowed intersections).
func TestPctHintsAllowedBoundsResults(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{
		supported: true,
		cpuClos:   map[int]int{1: 0, 4: 0}, // HP cpus on both packages
		maxHp:     map[int]int{0: 2, 1: 2},
	}
	a := &Allocator{
		sys:  sys,
		sst:  sst,
		mode: pctModeManaged,
		classByName: map[string]*policyapi.CPUClass{
			"hp": {Name: "hp", PctPriority: "high"},
		},
		classPlan: map[string]*pctClassPlan{"hp": {ClosID: 0}},
		// allowed restricts to pkg0 only.
		allowed: cpuset.MustParse("0-3"),
		hpUsed: map[int]cpuset.CPUSet{
			0: cpuset.MustParse("0"),
			1: cpuset.MustParse("4"), // outside allowed
		},
	}
	pctTestWirePunits(a)
	got := a.Hints(types.AllocationIntent{
		ClassName:      "hp",
		FreeCpus:       cpuset.MustParse("1-3"),
		RequestedCount: 1,
	})
	// closCpus walks a.allowed, so cpu 4 is excluded automatically.
	// Prefer[0] (free closCpus) must contain only cpu 1.
	if len(got.Prefer) == 0 {
		t.Fatalf("Prefer empty, want at least closCpus hint")
	}
	if !got.Prefer[0].Cpus[0].Equals(cpuset.MustParse("1")) {
		t.Errorf("Prefer[0].Cpus = %v, want {1} (cpu 4 outside allowed)", got.Prefer[0].Cpus)
	}
	// HP reserve must come from a package whose free CPUs are
	// inside allowed; only pkg0 qualifies.
	if len(got.Prefer) >= 2 {
		want := cpuset.MustParse("1-3")
		if !got.Prefer[1].Cpus[0].Equals(want) {
			t.Errorf("HP reserve = %v, want %s (pkg0 free cpus inside allowed)", got.Prefer[1].Cpus, want)
		}
	}
}

// --- Tier A/B/C reservation tests ----------------------------------

// newTwoPunitFakeSys returns a fakeSys whose package layout matches
// the standard two-punit-per-package fixture below: pkg0 = 0..7
// (punit-0 = 0..3, punit-1 = 4..7), pkg1 = 8..15 (punit-2 = 8..11,
// punit-3 = 12..15). The synthesis function does not know about
// punits, only packages.
func newTwoPunitFakeSys() *fakeSys {
	return &fakeSys{
		packageCpus: map[idset.ID]cpuset.CPUSet{
			0: cpuset.MustParse("0-7"),
			1: cpuset.MustParse("8-15"),
		},
		cpuPkg: map[int]idset.ID{
			0: 0, 1: 0, 2: 0, 3: 0, 4: 0, 5: 0, 6: 0, 7: 0,
			8: 1, 9: 1, 10: 1, 11: 1, 12: 1, 13: 1, 14: 1, 15: 1,
		},
	}
}

// makeTwoPunitsPerPkg returns four punits laid out as in
// newTwoPunitFakeSys, with the given MaxHpCpus per punit.
func makeTwoPunitsPerPkg(hp0, hp1, hp2, hp3 int) []pctPunit {
	return []pctPunit{
		{PkgID: 0, PunitID: 0, CPUs: cpuset.MustParse("0-3"), MaxHpCpus: hp0},
		{PkgID: 0, PunitID: 1, CPUs: cpuset.MustParse("4-7"), MaxHpCpus: hp1},
		{PkgID: 1, PunitID: 2, CPUs: cpuset.MustParse("8-11"), MaxHpCpus: hp2},
		{PkgID: 1, PunitID: 3, CPUs: cpuset.MustParse("12-15"), MaxHpCpus: hp3},
	}
}

// TestPctHints_HpRoomTierAPunitWins: punit-0 is fully occupied by
// HP work, punit-1 in the same package has full HP room. A request
// for 1 HP CPU must steer to punit-1 (Tier A), not to pkg1.
func TestPctHints_HpRoomTierAPunitWins(t *testing.T) {
	sys := newTwoPunitFakeSys()
	sst := &fakeSst{
		supported: true,
		punits:    makeTwoPunitsPerPkg(2, 2, 2, 2),
	}
	a := &Allocator{
		sys:         sys,
		sst:         sst,
		mode:        pctModeManaged,
		classByName: map[string]*policyapi.CPUClass{"hp": {Name: "hp", PctPriority: "high"}},
		classPlan:   map[string]*pctClassPlan{"hp": {ClosID: 0}},
		allowed:     cpuset.MustParse("0-15"),
		// Punit-0 fully booked with HP (cpus 0,1 take both HP slots).
		hpUsed: map[int]cpuset.CPUSet{0: cpuset.MustParse("0-1")},
	}
	pctTestWirePunits(a)

	got := a.Hints(types.AllocationIntent{
		ClassName:      "hp",
		FreeCpus:       cpuset.MustParse("2-15"),
		RequestedCount: 1,
	})

	// Find HP reserve hint.
	var reserve cpuset.CPUSet
	for _, p := range got.Prefer {
		if p.Name == virtDevSstHpReserveHint {
			reserve = p.Cpus[0]
		}
	}
	if reserve.IsEmpty() {
		t.Fatalf("expected HP reserve hint, got Prefer=%+v", got.Prefer)
	}
	// Tier A: punit-1 (room=2) beats punit-0 (room=0) and the
	// equal-room punits in pkg1 because punit-0/punit-1 both belong
	// to pkg0 -- here we pick by largest room.
	// Actually both punit-1 (room=2), punit-2 (room=2), punit-3
	// (room=2) tie; tie-break by free-CPU count (all 4) and then
	// by iteration order (slice index 1 first). So expect punit-1.
	want := cpuset.MustParse("4-7")
	if !reserve.Equals(want) {
		t.Errorf("Tier A HP reserve = %s, want %s (punit-1)", reserve, want)
	}
}

// TestPctHints_HpRoomTierBSamePackage: punit-0 and punit-1 in pkg0
// each have only 1 HP slot left, but together they offer 2 slots --
// enough for the request. Pkg1 has only 1 HP slot in total. The
// Tier-B aggregate must steer to pkg0 (free CPUs of both punits).
func TestPctHints_HpRoomTierBSamePackage(t *testing.T) {
	sys := newTwoPunitFakeSys()
	sst := &fakeSst{
		supported: true,
		punits:    makeTwoPunitsPerPkg(2, 2, 1, 0),
	}
	a := &Allocator{
		sys:         sys,
		sst:         sst,
		mode:        pctModeManaged,
		classByName: map[string]*policyapi.CPUClass{"hp": {Name: "hp", PctPriority: "high"}},
		classPlan:   map[string]*pctClassPlan{"hp": {ClosID: 0}},
		allowed:     cpuset.MustParse("0-15"),
		// Both pkg0 punits already host 1 HP CPU each, leaving room=1 in each.
		hpUsed: map[int]cpuset.CPUSet{
			0: cpuset.MustParse("0"), // punit-0 idx 0
			1: cpuset.MustParse("4"), // punit-1 idx 1
		},
	}
	pctTestWirePunits(a)

	got := a.Hints(types.AllocationIntent{
		ClassName:      "hp",
		FreeCpus:       cpuset.MustParse("1-3,5-15"),
		RequestedCount: 2,
	})
	var reserve cpuset.CPUSet
	for _, p := range got.Prefer {
		if p.Name == virtDevSstHpReserveHint {
			reserve = p.Cpus[0]
		}
	}
	if reserve.IsEmpty() {
		t.Fatalf("expected HP reserve hint, got Prefer=%+v", got.Prefer)
	}
	// Tier A is impossible (no single punit has room>=2 in pkg0,
	// and pkg1 punit-2 has 1 cpu only). Tier B: pkg0 sum-room=2
	// >= 2, pkg1 sum-room=1 < 2. Reserve = pkg0 free CPUs.
	want := cpuset.MustParse("1-3,5-7")
	if !reserve.Equals(want) {
		t.Errorf("Tier B HP reserve = %s, want %s (pkg0 union)", reserve, want)
	}
}

// TestPctHints_HpRoomTierCNoCrossPackage: request exceeds the HP
// room of every single package. Tier C is never taken - the
// allocator must return no HP-reserve hint so the caller falls back
// to topology-only placement on the same socket.
func TestPctHints_HpRoomTierCNoCrossPackage(t *testing.T) {
	sys := newTwoPunitFakeSys()
	sst := &fakeSst{
		supported: true,
		// pkg0 has 2 HP CPUs total, pkg1 has 2 HP CPUs total.
		punits: makeTwoPunitsPerPkg(1, 1, 1, 1),
	}
	a := &Allocator{
		sys:         sys,
		sst:         sst,
		mode:        pctModeManaged,
		classByName: map[string]*policyapi.CPUClass{"hp": {Name: "hp", PctPriority: "high"}},
		classPlan:   map[string]*pctClassPlan{"hp": {ClosID: 0}},
		allowed:     cpuset.MustParse("0-15"),
		hpUsed:      map[int]cpuset.CPUSet{},
	}
	pctTestWirePunits(a)

	got := a.Hints(types.AllocationIntent{
		ClassName:      "hp",
		FreeCpus:       cpuset.MustParse("0-15"),
		RequestedCount: 3, // > any single package's HP capacity (2)
	})
	for _, p := range got.Prefer {
		if p.Name == virtDevSstHpReserveHint {
			t.Errorf("Tier C must not emit HP reserve hint; got %+v", p)
		}
	}
}

// TestPctHints_HpInUseIsPunitGranular: managed-mode non-HP class
// must Avoid only the punits currently hosting HP work, not the
// entire package. This is a regression guard for the punit-keyed
// rewrite of hpInUseCpus.
func TestPctHints_HpInUseIsPunitGranular(t *testing.T) {
	sys := newTwoPunitFakeSys()
	sst := &fakeSst{
		supported: true,
		punits:    makeTwoPunitsPerPkg(2, 2, 2, 2),
	}
	a := &Allocator{
		sys:  sys,
		sst:  sst,
		mode: pctModeManaged,
		classByName: map[string]*policyapi.CPUClass{
			"hp": {Name: "hp", PctPriority: "high"},
			"lp": {Name: "lp", PctPriority: "low"},
		},
		classPlan: map[string]*pctClassPlan{
			"hp": {ClosID: 0},
			"lp": {ClosID: 3},
		},
		allowed: cpuset.MustParse("0-15"),
		// HP work on punit-0 only (pkg0).
		hpUsed: map[int]cpuset.CPUSet{0: cpuset.MustParse("0")},
	}
	pctTestWirePunits(a)

	got := a.Hints(types.AllocationIntent{
		ClassName:      "lp",
		FreeCpus:       cpuset.MustParse("1-15"),
		RequestedCount: 1,
	})
	if len(got.Avoid) != 1 {
		t.Fatalf("Avoid count = %d, want 1: got=%+v", len(got.Avoid), got.Avoid)
	}
	// Must be punit-0 (cpus 0-3) ONLY, not all of pkg0 (0-7).
	want := cpuset.MustParse("0-3")
	if !got.Avoid[0].Cpus[0].Equals(want) {
		t.Errorf("Avoid = %v, want %s (punit-0 only, not full pkg0)", got.Avoid[0].Cpus, want)
	}
}

// --- classifyAssocOnlyHP tests -------------------------------------

// TestPctClassifyAssocOnlyHP_MaxFreqWins: of two referenced CLOSes
// with programmed MaxFreq, the larger MaxFreq is the HP class.
func TestPctClassifyAssocOnlyHP_MaxFreqWins(t *testing.T) {
	a := &Allocator{
		sst: &fakeSst{
			supported: true,
			closCfg: map[int]pctClosCfg{
				1: {MinFreq: 1000000, MaxFreq: 3000000}, // base-ish
				2: {MinFreq: 2000000, MaxFreq: 3800000}, // turbo
			},
		},
		classPlan: map[string]*pctClassPlan{
			"c-base":  {ClosID: 1},
			"c-turbo": {ClosID: 2},
		},
		hpClasses: map[string]bool{},
	}
	classes := []*policyapi.CPUClass{
		{Name: "c-base"},
		{Name: "c-turbo"},
	}
	a.classifyAssocOnlyHP(classes)
	if a.hpClasses["c-base"] {
		t.Errorf("c-base must NOT be classified HP (lower MaxFreq)")
	}
	if !a.hpClasses["c-turbo"] {
		t.Errorf("c-turbo must be classified HP (higher MaxFreq)")
	}
}

// TestPctClassifyAssocOnlyHP_TieBreakSmallerClos: when two CLOSes
// share the highest MaxFreq, the smaller CLOS id wins (SST-CP
// ordered-priority convention).
func TestPctClassifyAssocOnlyHP_TieBreakSmallerClos(t *testing.T) {
	a := &Allocator{
		sst: &fakeSst{
			supported: true,
			closCfg: map[int]pctClosCfg{
				1: {MaxFreq: 3800000},
				2: {MaxFreq: 3800000}, // tie
			},
		},
		classPlan: map[string]*pctClassPlan{
			"c1": {ClosID: 1},
			"c2": {ClosID: 2},
		},
		hpClasses: map[string]bool{},
	}
	a.classifyAssocOnlyHP([]*policyapi.CPUClass{{Name: "c1"}, {Name: "c2"}})
	if !a.hpClasses["c1"] {
		t.Errorf("c1 must win tie (smaller CLOS id)")
	}
	if a.hpClasses["c2"] {
		t.Errorf("c2 must NOT be HP (lost tie)")
	}
}

// TestPctClassifyAssocOnlyHP_NoProgrammedFreq: when no CLOS has a
// programmed MaxFreq, no class is classified HP -- HP-specific
// hints stay quiet.
func TestPctClassifyAssocOnlyHP_NoProgrammedFreq(t *testing.T) {
	a := &Allocator{
		sst: &fakeSst{supported: true, closCfg: map[int]pctClosCfg{}},
		classPlan: map[string]*pctClassPlan{
			"c1": {ClosID: 1},
		},
		hpClasses: map[string]bool{},
	}
	a.classifyAssocOnlyHP([]*policyapi.CPUClass{{Name: "c1"}})
	if len(a.hpClasses) != 0 {
		t.Errorf("hpClasses=%v, want empty when no CLOS has programmed MaxFreq", a.hpClasses)
	}
}

// TestPctClassifyAssocOnlyHP_ZeroMaxFreqIgnored: a CLOS that
// returns (cfg, true, nil) but with MaxFreq==0 must not be
// classified HP (zero is "not specified").
func TestPctClassifyAssocOnlyHP_ZeroMaxFreqIgnored(t *testing.T) {
	a := &Allocator{
		sst: &fakeSst{
			supported: true,
			closCfg:   map[int]pctClosCfg{1: {MinFreq: 1000000}}, // MaxFreq=0
		},
		classPlan: map[string]*pctClassPlan{"c1": {ClosID: 1}},
		hpClasses: map[string]bool{},
	}
	a.classifyAssocOnlyHP([]*policyapi.CPUClass{{Name: "c1"}})
	if a.hpClasses["c1"] {
		t.Errorf("c1 must NOT be HP when MaxFreq=0")
	}
}

// --- BF fallback test ----------------------------------------------

// TestPctPunitMaxHpCpus_BfFallback: punit with TF unsupported but
// BF-supported high-priority CPU set must report MaxHpCpus equal
// to len(BF.HighPriorityCPUs).
func TestPctPunitMaxHpCpus_BfFallback(t *testing.T) {
	pi := &gosst.PerfLevelInfo{
		BF: gosst.BFInfo{
			Supported:        true,
			HighPriorityCPUs: idset.NewIDSet(0, 1, 2, 3),
		},
		TF: gosst.TFInfo{Supported: false},
	}
	if got := punitMaxHpCpus(pi); got != 4 {
		t.Errorf("punitMaxHpCpus = %d, want 4 (BF fallback)", got)
	}
}

// TestPctPunitMaxHpCpus_TfWins: when both TF and BF are present,
// TF takes precedence (largest bucket HighPriorityCoreCount sets
// the cap).
func TestPctPunitMaxHpCpus_TfWins(t *testing.T) {
	pi := &gosst.PerfLevelInfo{
		BF: gosst.BFInfo{
			Supported:        true,
			HighPriorityCPUs: idset.NewIDSet(0, 1), // 2
		},
		TF: gosst.TFInfo{
			Supported: true,
			Buckets: []gosst.TFBucketInfo{
				{ID: 0, HighPriorityCoreCount: 1},
				{ID: 1, HighPriorityCoreCount: 4}, // max
				{ID: 2, HighPriorityCoreCount: 2},
			},
		},
	}
	if got := punitMaxHpCpus(pi); got != 4 {
		t.Errorf("punitMaxHpCpus = %d, want 4 (largest TF bucket)", got)
	}
}

// TestPctPunitMaxHpCpus_NeitherSupported: with neither TF nor BF
// supported, MaxHpCpus is 0 (the allocator excludes such punits
// from HP steering).
func TestPctPunitMaxHpCpus_NeitherSupported(t *testing.T) {
	pi := &gosst.PerfLevelInfo{}
	if got := punitMaxHpCpus(pi); got != 0 {
		t.Errorf("punitMaxHpCpus = %d, want 0", got)
	}
}

// TestPctPunitGuaranteedHpCpus_TfSmallestBucket: with multiple
// non-zero TF buckets, the guaranteed top-turbo HP CPU count is
// the smallest HighPriorityCoreCount (smaller buckets unlock
// higher turbo frequencies).
func TestPctPunitGuaranteedHpCpus_TfSmallestBucket(t *testing.T) {
	pi := &gosst.PerfLevelInfo{
		TF: gosst.TFInfo{
			Supported: true,
			Buckets: []gosst.TFBucketInfo{
				{ID: 0, HighPriorityCoreCount: 24},
				{ID: 1, HighPriorityCoreCount: 8}, // smallest non-zero
				{ID: 2, HighPriorityCoreCount: 16},
			},
		},
	}
	if got := punitGuaranteedHpCpus(pi); got != 8 {
		t.Errorf("punitGuaranteedHpCpus = %d, want 8 (smallest TF bucket)", got)
	}
}

// TestPctPunitGuaranteedHpCpus_BfFallback: when TF is
// unsupported, fall back to len(BF.HighPriorityCPUs).
func TestPctPunitGuaranteedHpCpus_BfFallback(t *testing.T) {
	pi := &gosst.PerfLevelInfo{
		BF: gosst.BFInfo{
			Supported:        true,
			HighPriorityCPUs: idset.NewIDSet(0, 1, 2, 3),
		},
	}
	if got := punitGuaranteedHpCpus(pi); got != 4 {
		t.Errorf("punitGuaranteedHpCpus = %d, want 4 (BF fallback)", got)
	}
}

// TestPctPunitGuaranteedHpCpus_NeitherSupported: neither TF nor
// BF -> 0.
func TestPctPunitGuaranteedHpCpus_NeitherSupported(t *testing.T) {
	pi := &gosst.PerfLevelInfo{}
	if got := punitGuaranteedHpCpus(pi); got != 0 {
		t.Errorf("punitGuaranteedHpCpus = %d, want 0", got)
	}
}

// --- FreeClassCapacity test suite -----------------------------------

// newAssocOnlyPctForTest mirrors newManagedPctForTest but configures
// the allocator in assoc-only mode. hpClasses, classPlan and
// hpEligiblePunit must be set up by the caller after the helper
// returns to keep the test intent explicit.
func newAssocOnlyPctForTest(t *testing.T, classes []*policyapi.CPUClass, plans map[string]*pctClassPlan,
	allowed cpuset.CPUSet, sys *fakeSys, sst *fakeSst) *Allocator {
	t.Helper()
	a := &Allocator{
		sys:             sys,
		sst:             sst,
		mode:            pctModeAssocOnly,
		classByName:     map[string]*policyapi.CPUClass{},
		classPlan:       plans,
		allowed:         allowed,
		hpUsed:          map[int]cpuset.CPUSet{},
		hpClasses:       map[string]bool{},
		hpEligiblePunit: map[int]bool{},
	}
	for _, cc := range classes {
		a.classByName[cc.Name] = cc
	}
	pctTestWirePunits(a)
	return a
}

// TestFreeClassCapacity_AssocOnlyHpFromFallbackCLOS verifies the
// real-world assoc-only bug fix: every CPU starts on the fallback
// (LP) CLOS in hardware because the balloons policy associates the
// idle/default class on Configure, yet HP capacity for an HP class
// must still report sum_pu min(GuaranteedHpCpus, |pu.CPUs \ held|)
// -- not zero. (Pre-fix the result was 0 because closCpus(HP CLOS)
// was empty.)
func TestFreeClassCapacity_AssocOnlyHpFromFallbackCLOS(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{
		supported: true,
		// All CPUs are on CLOS 3 (the LP/fallback CLOS). The HP
		// CLOS 0 has no CPUs associated to it.
		cpuClos: map[int]int{
			0: 3, 1: 3, 2: 3, 3: 3,
			4: 3, 5: 3, 6: 3, 7: 3,
		},
		// Two punits (one per package); each guarantees 2 HP CPUs at top turbo.
		punits: []pctPunit{
			{PkgID: 0, PunitID: 0, CPUs: cpuset.MustParse("0-3"), GuaranteedHpCpus: 2},
			{PkgID: 1, PunitID: 0, CPUs: cpuset.MustParse("4-7"), GuaranteedHpCpus: 2},
		},
	}
	classes := []*policyapi.CPUClass{
		{Name: "hp"}, // pctPriority not set; HP is decided by classifyAssocOnlyHP at runtime
		{Name: "lp"},
	}
	a := newAssocOnlyPctForTest(t, classes,
		map[string]*pctClassPlan{"hp": {ClosID: 0}, "lp": {ClosID: 3}},
		cpuset.MustParse("0-7"), sys, sst)
	a.hpClasses["hp"] = true // simulate classifyAssocOnlyHP result

	// Held by some non-HP balloon: 2 CPUs (one per punit).
	held := cpuset.MustParse("3,7")

	gotHp := a.FreeClassCapacity("hp", held)
	wantHp := 2 + 2 // both punits: min(2, |{0,1,2}|=3)=2 and min(2, |{4,5,6}|=3)=2
	if gotHp != wantHp {
		t.Errorf("HP capacity (assoc-only, all cpus on fallback CLOS) = %d, want %d",
			gotHp, wantHp)
	}

	gotLp := a.FreeClassCapacity("lp", held)
	wantLp := 8 - 2 // allowed (8) minus held (2)
	if gotLp != wantLp {
		t.Errorf("LP capacity (assoc-only) = %d, want %d", gotLp, wantLp)
	}
}

// TestFreeClassCapacity_AssocOnlyHpTFDisabledPunitExcluded verifies
// the eligibility gate: punits where SST-TF is disabled in
// assoc-only mode contribute zero HP capacity even when their
// GuaranteedHpCpus is non-zero. Prevents over-publishing HP
// capacity on nodes that cannot actually deliver top turbo.
func TestFreeClassCapacity_AssocOnlyHpTFDisabledPunitExcluded(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{
		supported: true,
		punits: []pctPunit{
			{PkgID: 0, PunitID: 0, CPUs: cpuset.MustParse("0-3"), GuaranteedHpCpus: 2},
			{PkgID: 1, PunitID: 0, CPUs: cpuset.MustParse("4-7"), GuaranteedHpCpus: 2},
		},
	}
	a := newAssocOnlyPctForTest(t, []*policyapi.CPUClass{{Name: "hp"}},
		map[string]*pctClassPlan{"hp": {ClosID: 0}},
		cpuset.MustParse("0-7"), sys, sst)
	a.hpClasses["hp"] = true
	// pctTestWirePunits marked both eligible; flip pkg1 punit to
	// TF-disabled to model the assoc-only "operator did not enable
	// SST-TF on this punit" case.
	a.hpEligiblePunit[1] = false

	got := a.FreeClassCapacity("hp", cpuset.New())
	want := 2 // only pkg0 contributes
	if got != want {
		t.Errorf("HP capacity with one TF-disabled punit = %d, want %d", got, want)
	}
}

// TestFreeClassCapacity_AssocOnlyNoHpClassification: assoc-only
// where no class was classified HP (e.g. no CLOS has a programmed
// MaxFreq) falls through to the non-HP formula |Allowed \ held|.
func TestFreeClassCapacity_AssocOnlyNoHpClassification(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{
		supported: true,
		punits: []pctPunit{
			{PkgID: 0, PunitID: 0, CPUs: cpuset.MustParse("0-3"), GuaranteedHpCpus: 2},
			{PkgID: 1, PunitID: 0, CPUs: cpuset.MustParse("4-7"), GuaranteedHpCpus: 2},
		},
	}
	a := newAssocOnlyPctForTest(t, []*policyapi.CPUClass{{Name: "c1"}},
		map[string]*pctClassPlan{"c1": {ClosID: 1}},
		cpuset.MustParse("0-7"), sys, sst)
	// Intentionally no entries in a.hpClasses.

	got := a.FreeClassCapacity("c1", cpuset.MustParse("1,5"))
	want := 8 - 2
	if got != want {
		t.Errorf("non-HP assoc-only capacity = %d, want %d", got, want)
	}
}

// TestFreeClassCapacity_ManagedHpRespectsEligibility keeps the
// existing managed-mode formula intact: every punit is HP-eligible
// (PrepareManagedMode enables SST-TF) and the result is the
// guaranteed-top-turbo sum, capped by per-punit free CPUs.
func TestFreeClassCapacity_ManagedHpRespectsEligibility(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{
		supported: true,
		punits: []pctPunit{
			{PkgID: 0, PunitID: 0, CPUs: cpuset.MustParse("0-3"), GuaranteedHpCpus: 2},
			{PkgID: 1, PunitID: 0, CPUs: cpuset.MustParse("4-7"), GuaranteedHpCpus: 2},
		},
	}
	classes := []*policyapi.CPUClass{
		{Name: "hp", PctPriority: "high"},
		{Name: "lp", PctPriority: "low"},
	}
	a := newManagedPctForTest(t, classes,
		map[string]*pctClassPlan{"hp": {ClosID: 0}, "lp": {ClosID: 3}},
		cpuset.MustParse("0-7"), sys, sst)

	gotHp := a.FreeClassCapacity("hp", cpuset.MustParse("3"))
	wantHp := 2 + 2 // pkg0: min(2, 3)=2; pkg1: min(2, 4)=2
	if gotHp != wantHp {
		t.Errorf("managed HP capacity = %d, want %d", gotHp, wantHp)
	}
	gotLp := a.FreeClassCapacity("lp", cpuset.MustParse("3"))
	wantLp := 8 - 1
	if gotLp != wantLp {
		t.Errorf("managed LP capacity = %d, want %d", gotLp, wantLp)
	}

	// Squeeze pkg0: hold 3 of its 4 CPUs => pkg0 contributes min(2,1)=1.
	gotHp = a.FreeClassCapacity("hp", cpuset.MustParse("0-2"))
	wantHp = 1 + 2
	if gotHp != wantHp {
		t.Errorf("managed HP capacity with squeezed pkg0 = %d, want %d", gotHp, wantHp)
	}
}

// TestFreeClassCapacity_UnknownClassReturnsZero: unknown class
// (no PCT plan) yields 0 regardless of mode.
func TestFreeClassCapacity_UnknownClassReturnsZero(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{supported: true}
	a := newManagedPctForTest(t, []*policyapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		map[string]*pctClassPlan{"hp": {ClosID: 0}},
		cpuset.MustParse("0-7"), sys, sst)
	if got := a.FreeClassCapacity("nope", cpuset.New()); got != 0 {
		t.Errorf("unknown class capacity = %d, want 0", got)
	}
}

// makePunitsWithGtdHp returns two punits in the same package, each with the
// given MaxHpCpus and GuaranteedHpCpus values.
func makePunitsWithGtdHp(maxHp0, gtdHp0, maxHp1, gtdHp1 int) []pctPunit {
	return []pctPunit{
		{PkgID: 0, PunitID: 0, CPUs: cpuset.MustParse("0-3"), MaxHpCpus: maxHp0, GuaranteedHpCpus: gtdHp0},
		{PkgID: 0, PunitID: 1, CPUs: cpuset.MustParse("4-7"), MaxHpCpus: maxHp1, GuaranteedHpCpus: gtdHp1},
	}
}

// newPickAllocator returns an Allocator pre-wired for PickHpCpus / ReleaseHpCpus tests.
func newPickAllocator(t *testing.T, punits []pctPunit) *Allocator {
	t.Helper()
	sys := newTwoPunitFakeSys()
	sst := &fakeSst{supported: true, punits: punits}
	classes := []*policyapi.CPUClass{{Name: "hp", PctPriority: "high"}}
	plans := map[string]*pctClassPlan{"hp": {ClosID: 0}}
	a := newManagedPctForTest(t, classes, plans, cpuset.MustParse("0-7"), sys, sst)
	return a
}

func TestPunitHPCapacity(t *testing.T) {
	// Active() == false: Allocator with mode == disabled
	inactiveA := &Allocator{}
	if got := inactiveA.punitHPCapacity(0); got != 0 {
		t.Errorf("punitHPCapacity on inactive allocator = %d, want 0", got)
	}

	a := newPickAllocator(t, makePunitsWithGtdHp(4, 3, 4, 1))
	tests := []struct {
		name string
		idx  int
		want int
	}{
		{"eligible punit 0", 0, 3},
		{"eligible punit 1", 1, 1},
		{"out-of-range", 99, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a.punitHPCapacity(tc.idx)
			if got != tc.want {
				t.Errorf("punitHPCapacity(%d) = %d, want %d", tc.idx, got, tc.want)
			}
		})
	}
	// HP-ineligible punit
	a.hpEligiblePunit[0] = false
	if got := a.punitHPCapacity(0); got != 0 {
		t.Errorf("punitHPCapacity for ineligible punit = %d, want 0", got)
	}
}

// TestPunitHPCapacity_CappedByAllowedIntersection verifies that
// punitHPCapacity caps the raw hardware GuaranteedHpCpus by the punit's
// actual CPU count after intersecting with the allowed/online set —
// otherwise it would advertise more DRA capacity than PickHpCpus can
// supply once some of the punit's CPUs are excluded (e.g. offline or
// outside the reserved/shared pool).
func TestPunitHPCapacity_CappedByAllowedIntersection(t *testing.T) {
	sys := newTwoPunitFakeSys()
	// Raw punit 0 spans CPUs 0-3 (4 CPUs) with GuaranteedHpCpus=3.
	// Restricting "allowed" to CPUs 0-1,4-7 leaves punit 0 with only
	// CPUs 0-1 (2 CPUs) after intersection -- less than its raw
	// GuaranteedHpCpus of 3.
	sst := &fakeSst{supported: true, punits: makePunitsWithGtdHp(4, 3, 4, 1)}
	classes := []*policyapi.CPUClass{{Name: "hp", PctPriority: "high"}}
	plans := map[string]*pctClassPlan{"hp": {ClosID: 0}}
	a := newManagedPctForTest(t, classes, plans, cpuset.MustParse("0-1,4-7"), sys, sst)

	if got := a.punitHPCapacity(0); got != 2 {
		t.Errorf("punitHPCapacity(0) = %d, want 2 (capped by allowed intersection, not raw GuaranteedHpCpus=3)", got)
	}
	// Punit 1 is unaffected: its full range (4-7) is within allowed, and
	// its GuaranteedHpCpus=1 stays under the 4-CPU cap.
	if got := a.punitHPCapacity(1); got != 1 {
		t.Errorf("punitHPCapacity(1) = %d, want 1 (unaffected by allowed restriction)", got)
	}
}

func TestPunitNonHPCapacity(t *testing.T) {
	inactiveA := &Allocator{}
	if got := inactiveA.punitNonHPCapacity(0); got != 0 {
		t.Errorf("punitNonHPCapacity on inactive allocator = %d, want 0", got)
	}

	// punit 0: CPUs 0-3 (4 total), GuaranteedHpCpus=3 → 1 non-HP
	// punit 1: CPUs 4-7 (4 total), GuaranteedHpCpus=0 → 4 non-HP
	a := newPickAllocator(t, makePunitsWithGtdHp(4, 3, 4, 0))
	tests := []struct {
		name string
		idx  int
		want int
	}{
		{"partial HP", 0, 1},
		{"all non-HP", 1, 4},
		{"out-of-range", 99, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a.punitNonHPCapacity(tc.idx)
			if got != tc.want {
				t.Errorf("punitNonHPCapacity(%d) = %d, want %d", tc.idx, got, tc.want)
			}
		})
	}
	// all CPUs HP-guaranteed → 0 non-HP
	a2 := newPickAllocator(t, makePunitsWithGtdHp(4, 4, 4, 4))
	if got := a2.punitNonHPCapacity(0); got != 0 {
		t.Errorf("all-HP punit nonHPCapacity = %d, want 0", got)
	}

	// HP-ineligible punit with non-zero GuaranteedHpCpus: the guard in
	// punitHPCapacity must zero out the HP deduction so the full CPU count
	// is reported as non-HP capacity.
	// punit 0: HP-ineligible, GuaranteedHpCpus=2, CPUs 0-3 → 4 non-HP (not 2)
	// punit 1: HP-eligible,   GuaranteedHpCpus=2, CPUs 4-7 → 2 non-HP
	a3 := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))
	a3.hpEligiblePunit[0] = false
	if got := a3.punitNonHPCapacity(0); got != 4 {
		t.Errorf("HP-ineligible punit nonHPCapacity = %d, want 4", got)
	}
	if got := a3.punitNonHPCapacity(1); got != 2 {
		t.Errorf("HP-eligible punit nonHPCapacity = %d, want 2", got)
	}
}

func TestAllocatorPunits(t *testing.T) {
	// Inactive allocator returns nil.
	inactiveA := &Allocator{}
	if got := inactiveA.Punits(); got != nil {
		t.Errorf("Punits() on inactive allocator = %v, want nil", got)
	}

	a := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 1))
	pi := a.Punits()
	if len(pi) != 2 {
		t.Fatalf("Punits() len = %d, want 2", len(pi))
	}
	want := []PunitInfo{
		{PkgID: 0, PunitID: 0, HPCapacity: 2, NonHPCapacity: 2},
		{PkgID: 0, PunitID: 1, HPCapacity: 1, NonHPCapacity: 3},
	}
	for i, w := range want {
		if pi[i] != w {
			t.Errorf("Punits()[%d] = %+v, want %+v", i, pi[i], w)
		}
	}
}

// TestAllocatorPunits_NonDRAHpUsageReducesCapacity verifies that
// Punits() deducts non-DRA HP CPUs already tracked in hpUsed from the
// advertised HPCapacity, since Kubernetes only subtracts DRA
// allocations from the advertised capacity and would otherwise let a
// claim be placed that PickHpCpus must reject at Prepare time.
func TestAllocatorPunits_NonDRAHpUsageReducesCapacity(t *testing.T) {
	a := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 1))
	// Simulate the NRI path having already claimed 1 HP CPU on punit 0.
	a.hpUsed[0] = cpuset.MustParse("0")

	pi := a.Punits()
	want := []PunitInfo{
		{PkgID: 0, PunitID: 0, HPCapacity: 1, NonHPCapacity: 2},
		{PkgID: 0, PunitID: 1, HPCapacity: 1, NonHPCapacity: 3},
	}
	for i, w := range want {
		if pi[i] != w {
			t.Errorf("Punits()[%d] = %+v, want %+v", i, pi[i], w)
		}
	}
}

func TestPickHpCpus(t *testing.T) {
	// Active()==false
	inactiveA := &Allocator{}
	if _, err := inactiveA.PickHpCpus(0, 0, 1, cpuset.New()); err == nil {
		t.Error("PickHpCpus on inactive allocator: expected error, got nil")
	}

	a := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))

	// Success: pick 2 CPUs from punit 0 (PkgID=0, PunitID=0, CPUs 0-3).
	got, err := a.PickHpCpus(0, 0, 2, cpuset.New())
	if err != nil {
		t.Fatalf("PickHpCpus success case: %v", err)
	}
	if got.Size() != 2 {
		t.Errorf("PickHpCpus returned %d CPUs, want 2", got.Size())
	}
	// hpDRAUsed updated, hpUsed unchanged.
	if !a.hpDRAUsed[0].Equals(got) {
		t.Errorf("hpDRAUsed[0] = %v, want %v", a.hpDRAUsed[0], got)
	}
	if a.hpUsed[0].Size() != 0 {
		t.Errorf("hpUsed[0] should be untouched, got %v", a.hpUsed[0])
	}

	// Exhaustion: already 2 DRA-held + 0 available for another pick.
	if _, err := a.PickHpCpus(0, 0, 1, cpuset.New()); err == nil {
		t.Error("PickHpCpus exhaustion: expected error, got nil")
	}

	// HP-ineligible punit.
	a2 := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))
	a2.hpEligiblePunit[0] = false
	if _, err := a2.PickHpCpus(0, 0, 1, cpuset.New()); err == nil {
		t.Error("PickHpCpus ineligible punit: expected error, got nil")
	}

	// (PkgID, PunitID) not found.
	a3 := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))
	if _, err := a3.PickHpCpus(99, 99, 1, cpuset.New()); err == nil {
		t.Error("PickHpCpus not-found: expected error, got nil")
	}

	// held exclusion: hold CPUs 0,1 → pick of 2 from a 4-CPU punit
	// must return 2,3 (the remaining ones).
	a4 := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))
	held := cpuset.MustParse("0-1")
	got4, err := a4.PickHpCpus(0, 0, 2, held)
	if err != nil {
		t.Fatalf("PickHpCpus held-exclusion: %v", err)
	}
	if got4.Intersection(held).Size() != 0 {
		t.Errorf("PickHpCpus returned a held CPU: %v", got4)
	}
}

func TestReleaseHpCpus(t *testing.T) {
	a := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))

	// Pick 2 CPUs then release them.
	picked, _ := a.PickHpCpus(0, 0, 2, cpuset.New())
	a.ReleaseHpCpus(0, 0, picked)
	if a.hpDRAUsed[0].Size() != 0 {
		t.Errorf("hpDRAUsed[0] after full release = %v, want empty", a.hpDRAUsed[0])
	}
	// Map entry deleted.
	if _, ok := a.hpDRAUsed[0]; ok {
		t.Error("hpDRAUsed[0] entry should be deleted after full release")
	}

	// Release CPUs not held — no-op.
	a.ReleaseHpCpus(0, 0, cpuset.MustParse("0-1"))

	// Out-of-range (not found) — no-op, no panic.
	a.ReleaseHpCpus(99, 99, cpuset.MustParse("0"))

	// Partial release.
	picked2, _ := a.PickHpCpus(0, 0, 2, cpuset.New())
	first := cpuset.New(picked2.UnsortedList()[0])
	a.ReleaseHpCpus(0, 0, first)
	if a.hpDRAUsed[0].Size() != 1 {
		t.Errorf("hpDRAUsed[0] after partial release size = %d, want 1", a.hpDRAUsed[0].Size())
	}
}

func TestHpDRAUsedIsolation(t *testing.T) {
	// Build an allocator with both HP and LP classes to test that UseClass
	// on DRA-held CPUs does not corrupt the hpDRAUsed/hpUsed separation.
	sys := newTwoPunitFakeSys()
	sst := &fakeSst{supported: true, punits: makePunitsWithGtdHp(4, 2, 4, 2)}
	classes := []*policyapi.CPUClass{
		{Name: "hp", PctPriority: "high"},
		{Name: "lp", PctPriority: "low"},
	}
	plans := map[string]*pctClassPlan{
		"hp": {ClosID: 0},
		"lp": {ClosID: 3},
	}
	a := newManagedPctForTest(t, classes, plans, cpuset.MustParse("0-7"), sys, sst)

	// DRA holds 2 CPUs on punit 0; hpUsed[0] is empty.
	draHeld, err := a.PickHpCpus(0, 0, 2, cpuset.New())
	if err != nil {
		t.Fatalf("PickHpCpus: %v", err)
	}
	before := a.hpDRAUsed[0].Clone()

	// HP UseClass on DRA-held CPUs — must NOT add them to hpUsed (they are
	// already accounted in hpDRAUsed; double-counting corrupts Punits capacity).
	_ = a.UseClass("hp", draHeld)
	if !a.hpDRAUsed[0].Equals(before) {
		t.Errorf("hpDRAUsed[0] changed after HP UseClass: got %v, want %v", a.hpDRAUsed[0], before)
	}
	if !a.hpUsed[0].IsEmpty() {
		t.Errorf("hpUsed[0] = %v after HP UseClass on DRA-held CPUs, want empty", a.hpUsed[0])
	}

	// Non-HP UseClass on overlapping CPUs — must NOT remove them from hpDRAUsed.
	_ = a.UseClass("lp", draHeld)
	if !a.hpDRAUsed[0].Equals(before) {
		t.Errorf("hpDRAUsed[0] changed after non-HP UseClass: got %v, want %v", a.hpDRAUsed[0], before)
	}
	// hpInUseCpus must still report the DRA-held CPUs.
	inUse := a.hpInUseCpus()
	for _, cpu := range draHeld.UnsortedList() {
		if !inUse.Contains(cpu) {
			t.Errorf("hpInUseCpus missing DRA-held cpu %d", cpu)
		}
	}
}

// TestIsHPClass covers the exported IsHPClass wrapper: HP class returns true;
// non-HP class returns false; unknown class returns false; inactive allocator
// returns false.
func TestIsHPClass(t *testing.T) {
	sys := newTwoPackageFakeSys()
	sst := &fakeSst{supported: true, maxHp: map[int]int{0: 2, 1: 2}}
	classes := []*policyapi.CPUClass{
		{Name: "hp", PctPriority: "high"},
		{Name: "lp", PctPriority: "low"},
	}
	a := newManagedPctForTest(t, classes,
		map[string]*pctClassPlan{"hp": {ClosID: 0}, "lp": {ClosID: 3}},
		cpuset.MustParse("0-7"), sys, sst)

	// HP class must return true.
	if !a.IsHPClass("hp") {
		t.Error("IsHPClass(\"hp\") = false, want true")
	}
	// Non-HP class must return false.
	if a.IsHPClass("lp") {
		t.Error("IsHPClass(\"lp\") = true, want false")
	}
	// Unknown class must return false.
	if a.IsHPClass("unknown") {
		t.Error("IsHPClass(\"unknown\") = true, want false")
	}

	// Inactive allocator must return false.
	inactiveA := &Allocator{}
	if inactiveA.IsHPClass("hp") {
		t.Error("IsHPClass on inactive allocator = true, want false")
	}
}

// TestAccountHpCpus covers AccountHpCpus: used during restart reconciliation
// to rebuild hpDRAUsed from persisted claim state.
func TestAccountHpCpus(t *testing.T) {
	// Inactive allocator must return an error.
	inactiveA := &Allocator{}
	if err := inactiveA.AccountHpCpus(0, 0, cpuset.MustParse("0")); err == nil {
		t.Error("AccountHpCpus on inactive allocator: expected error, got nil")
	}

	// HP-ineligible punit must return an error.
	aInelig := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))
	aInelig.hpEligiblePunit[0] = false
	if err := aInelig.AccountHpCpus(0, 0, cpuset.MustParse("0")); err == nil {
		t.Error("AccountHpCpus on HP-ineligible punit: expected error, got nil")
	}

	// Unknown punit must return an error.
	aUnknown := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))
	if err := aUnknown.AccountHpCpus(99, 99, cpuset.MustParse("0")); err == nil {
		t.Error("AccountHpCpus unknown punit: expected error, got nil")
	}

	// Success: account CPUs on an HP-eligible punit.
	aOK := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2))
	cpus := cpuset.MustParse("0-1")
	if err := aOK.AccountHpCpus(0, 0, cpus); err != nil {
		t.Fatalf("AccountHpCpus success case: %v", err)
	}
	if !aOK.hpDRAUsed[0].Equals(cpus) {
		t.Errorf("hpDRAUsed[0] = %v, want %v", aOK.hpDRAUsed[0], cpus)
	}
	// hpUsed must remain untouched.
	if aOK.hpUsed[0].Size() != 0 {
		t.Errorf("hpUsed[0] should be untouched after AccountHpCpus, got %v", aOK.hpUsed[0])
	}

	// Double-account same CPUs is idempotent (union semantics).
	if err := aOK.AccountHpCpus(0, 0, cpus); err != nil {
		t.Fatalf("AccountHpCpus idempotent call: %v", err)
	}
	if !aOK.hpDRAUsed[0].Equals(cpus) {
		t.Errorf("hpDRAUsed[0] after double-account = %v, want %v (must be idempotent)", aOK.hpDRAUsed[0], cpus)
	}

	// Over-capacity: account more CPUs than GuaranteedHpCpus allows.
	// Must NOT return an error (container may already be running), and
	// hpDRAUsed must include all accounted CPUs.
	aOver := newPickAllocator(t, makePunitsWithGtdHp(4, 2, 4, 2)) // GuaranteedHpCpus=2
	overCommit := cpuset.MustParse("0-3")                         // 4 CPUs > GuaranteedHpCpus=2
	if err := aOver.AccountHpCpus(0, 0, overCommit); err != nil {
		t.Fatalf("AccountHpCpus over-capacity: expected no error, got %v", err)
	}
	if !aOver.hpDRAUsed[0].Equals(overCommit) {
		t.Errorf("hpDRAUsed[0] = %v, want %v (over-capacity still updates)", aOver.hpDRAUsed[0], overCommit)
	}
}

func TestHpReserveRoomWithDRAHolds(t *testing.T) {
	// Two punits, each with MaxHpCpus=2, GuaranteedHpCpus=2.
	a := newPickAllocator(t, makePunitsWithGtdHp(2, 2, 2, 2))

	// Before any holds, room on punit 0 should be 2.
	// hpReserveCpus returns Tier-A candidate sets; if room>=requested we
	// get a candidate set back. Requesting 2 CPUs from punit 0 should succeed.
	free := cpuset.MustParse("0-7")
	before := a.hpReserveCpus(free, cpuset.New(), 2)
	if len(before) == 0 {
		t.Fatal("hpReserveCpus before DRA holds: expected at least one candidate, got none")
	}

	// DRA picks 1 CPU on punit 0.
	_, err := a.PickHpCpus(0, 0, 1, cpuset.New())
	if err != nil {
		t.Fatalf("PickHpCpus: %v", err)
	}

	// Now request 2 CPUs from punit 0: room is 1 (2 - 1 DRA hold), so
	// hpReserveCpus should not return punit 0 as a single-punit Tier-A
	// candidate for a request of 2. It may return punit 1 (unaffected).
	after := a.hpReserveCpus(free, cpuset.New(), 2)
	for _, candidate := range after {
		// No candidate set should include the DRA-held CPUs as "free" HP room
		// for a 2-CPU request on punit 0 alone.
		if candidate.Intersection(cpuset.MustParse("0-3")).Size() > 1 {
			t.Errorf("hpReserveCpus candidate includes punit 0 CPUs despite DRA hold reducing room to 1")
		}
	}
}
