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
	"fmt"
	"sort"

	idset "github.com/intel/goresctrl/pkg/utils"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/types"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

var log = logger.NewLogger("cpuclass")

const (
	// pctDefaultHpClos / pctDefaultLpClos are the conventional
	// CLOS slots used in managed mode when the user does not pin
	// SstClosID explicitly. See the PCT Technical Article example.
	pctDefaultHpClos = 0
	pctDefaultLpClos = 3
)

// pctMode is the operating mode of the PCT allocator.
type pctMode int

const (
	pctModeDisabled  pctMode = iota
	pctModeManaged           // nri-plugin owns SoC-wide SST + CLOS configs
	pctModeAssocOnly         // operator/BIOS owns CLOSes; we only associate CPUs
)

// pctClassPlan records the CLOS that should be used for one PCT
// cpuClass and the freq bounds to program in managed mode.
type pctClassPlan struct {
	ClosID  int
	MinFreq uint // kHz, 0 = leave alone
	MaxFreq uint // kHz, 0 = leave alone
}

// Sys is the subset of sysfs.System that Allocator depends
// on. Defined here so tests can substitute a fake without
// implementing the full sysfs.System surface.
type Sys interface {
	PackageIDs() []idset.ID
	Package(id idset.ID) sysfs.CPUPackage
	CPU(id idset.ID) sysfs.CPU
	CPUIDs() []idset.ID
}

// Allocator manages Intel Priority Core Turbo CLOS associations
// driven by cpuClass definitions.
type Allocator struct {
	sys         Sys
	sst         sst
	mode        pctMode
	classByName map[string]*policyapi.CPUClass
	classPlan   map[string]*pctClassPlan // class name -> CLOS plan (PCT classes only)
	// fallbackClos is the hardware CLOS used for CPUs whose class
	// is not a PCT class. After SST reset CLOS 0 is the default,
	// so we use it here too. This is a hardware-level concept,
	// not a user-visible "idle".
	fallbackClos int
	allowed      cpuset.CPUSet
	// hpClasses holds the names of cpuClasses currently
	// classified as high priority. In managed mode this is every
	// class with pctPriority=high. In assoc-only mode it is
	// populated from GetClosConfig at Configure(): the CLOS with
	// the largest programmed MaxFreq is HP; classes targeting
	// that CLOS are HP. Tie-break (equal MaxFreq) goes to the
	// smaller CLOS id, matching SST-CP ordered-priority
	// convention. Empty when no HP class can be determined.
	hpClasses map[string]bool
	// punits is the per-punit topology cached from sst.Punits()
	// at Configure() time, with each punit's CPUs already
	// intersected with allowed.
	punits []pctPunit
	// punitByCpu maps each allowed CPU to its index in punits.
	// CPUs outside any known punit are absent from the map; the
	// allocator treats them as "no HP knowledge".
	punitByCpu map[int]int
	// hpUsed[i] is the set of CPUs currently held by HP-class
	// workloads on punits[i].
	hpUsed map[int]cpuset.CPUSet
	// hpEligiblePunit[i] reports whether punits[i] can actually
	// host HP-class CPUs at top turbo. Populated at Configure().
	// In managed mode every punit becomes eligible (the plugin
	// enables SST-TF itself). In assoc-only mode a punit is
	// eligible only when SST-TF is currently enabled on it
	// (operator's responsibility); otherwise its standard
	// turbo-ratio bucket caps HP frequency and the punit must
	// not contribute to scheduler-visible HP capacity. Missing
	// entries are treated as not eligible.
	hpEligiblePunit map[int]bool
}

// NewAllocator returns a new PCT allocator in the disabled mode.
func NewAllocator(sys Sys) (*Allocator, error) {
	s, err := newSst()
	if err != nil {
		return nil, err
	}
	return &Allocator{
		sys:  sys,
		sst:  s,
		mode: pctModeDisabled,
	}, nil
}

// configure selects the PCT operating mode from the given cpuClass
// definitions and, in managed mode, programs the corresponding SST
// CLOSes. Honors `allowed` as the boundary of CPUs the allocator may
// touch.
//
//   - classes: cpuClass definitions to inspect for PCT fields.
//   - allowed: CPUs the allocator may configure.
func (a *Allocator) Configure(classes []*policyapi.CPUClass, allowed cpuset.CPUSet) error {
	a.classByName = make(map[string]*policyapi.CPUClass, len(classes))
	for _, cc := range classes {
		a.classByName[cc.Name] = cc
	}
	a.fallbackClos = pctDefaultHpClos // CLOS 0 == default-after-reset
	a.allowed = allowed
	a.hpUsed = map[int]cpuset.CPUSet{}
	a.hpClasses = map[string]bool{}
	a.hpEligiblePunit = map[int]bool{}
	a.punits = nil
	a.punitByCpu = nil

	mode, plans, err := a.planClasses(classes)
	if err != nil {
		return err
	}
	a.mode = mode
	a.classPlan = plans
	if mode == pctModeDisabled {
		log.Debugf("pct: no cpuClasses request PCT; PCT allocator disabled")
		return nil
	}
	if !a.sst.Supported() {
		log.Warnf("pct: SST not supported on this host; ignoring PCT fields in cpuClasses")
		a.mode = pctModeDisabled
		a.classPlan = nil
		return nil
	}

	a.snapshotPunits()
	log.Infof("pct: mode=%s, %d PCT cpuClass(es), %d punit(s) across %d package(s)",
		a.modeString(), len(plans), len(a.punits), len(a.packageIDsFromPunits()))

	if mode == pctModeManaged {
		if err := a.sst.PrepareManagedMode(); err != nil {
			return fmt.Errorf("pct: failed to prepare managed mode: %w", err)
		}
		// Managed mode owns SST-TF and enables it on every punit
		// (PrepareManagedMode). All snapshotted punits are thus
		// HP-eligible.
		for idx := range a.punits {
			a.hpEligiblePunit[idx] = true
		}
		// Program every requested CLOS.
		closesProgrammed := map[int]bool{}
		closIDs := make([]int, 0, len(plans))
		for _, p := range plans {
			if closesProgrammed[p.ClosID] {
				continue
			}
			closIDs = append(closIDs, p.ClosID)
			closesProgrammed[p.ClosID] = true
		}
		sort.Ints(closIDs)
		for _, closID := range closIDs {
			var minF, maxF int
			for _, p := range plans {
				if p.ClosID == closID {
					minF = int(p.MinFreq)
					maxF = int(p.MaxFreq)
					break
				}
			}
			cfg := pctClosConfig{ClosID: closID, MinFreq: minF, MaxFreq: maxF}
			if err := a.sst.ConfigureClos(cfg); err != nil {
				return fmt.Errorf("pct: failed to configure CLOS %d: %w", closID, err)
			}
			log.Infof("pct: programmed CLOS %d min=%d max=%d kHz", closID, minF, maxF)
		}
		if err := a.sst.EnableCP(); err != nil {
			return fmt.Errorf("pct: failed to enable SST-CP: %w", err)
		}
		// Managed mode: HP classes are exactly those with pctPriority=high.
		// LP classes are those with pctPriority=low.
		var lpClos *int
		for _, cc := range classes {
			switch cc.PctPriority {
			case "high":
				a.hpClasses[cc.Name] = true
				log.Infof("pct: cpuClass %q classified HP (managed: pctPriority=high, CLOS %d)",
					cc.Name, plans[cc.Name].ClosID)
			case "low":
				id := plans[cc.Name].ClosID
				lpClos = &id
				log.Infof("pct: cpuClass %q classified LP (managed: pctPriority=low, CLOS %d)",
					cc.Name, plans[cc.Name].ClosID)
			}
		}
		// Idle / non-PCT CPUs must fall back to the LP CLOS (when
		// defined). Leaving them on CLOS 0 inflates the SST-TF
		// active-HP-core count on every punit and prevents bucket-0
		// turbo selection on punits hosting both an HP and an LP
		// balloon.
		if lpClos != nil {
			a.fallbackClos = *lpClos
			log.Infof("pct: fallback CLOS for non-PCT CPUs set to %d (LP)", a.fallbackClos)
		}
	} else {
		// Assoc-only: classify HP/LP from CLOS configs programmed
		// by the operator/BIOS. The CLOS with the largest MaxFreq
		// among the CLOSes our cpuClasses target is HP.
		a.classifyAssocOnlyHP(classes)
		a.evaluateAssocOnlyHpEligibility()
	}
	return nil
}

// evaluateAssocOnlyHpEligibility populates hpEligiblePunit and
// warns the operator about punits where SST-TF is disabled. In
// assoc-only mode the plugin must not toggle SST-TF (the operator
// owns global SST state). Without SST-TF the standard turbo-ratio
// table caps HP cores at the many-active-cores bucket frequency --
// a low-CLOS-ID association alone is not enough to exceed it.
// Capacity for HP cpuClasses on such punits must therefore be
// reported as zero, otherwise the scheduler bin-packs HP pods onto
// nodes that cannot actually deliver top turbo. The warning points
// the operator at the intel-speed-select command that enables it.
func (a *Allocator) evaluateAssocOnlyHpEligibility() {
	if len(a.punits) == 0 {
		return
	}
	status, err := a.sst.TFStatus()
	if err != nil {
		log.Warnf("pct: assoc-only: cannot read SST-TF status: %v", err)
		// Unknown TF state: leave every punit ineligible. Safer
		// to under-publish HP capacity than to over-publish it.
		return
	}
	for idx, pu := range a.punits {
		enabled, ok := status[pctPunitID{PkgID: pu.PkgID, PunitID: pu.PunitID}]
		if !ok {
			// No entry: TF state unknown for this punit. Treat
			// as ineligible.
			continue
		}
		if enabled {
			a.hpEligiblePunit[idx] = true
			continue
		}
		// Pick one representative CPU from the punit for the
		// operator hint -- intel-speed-select needs at least
		// one CPU on the target punit.
		repCPU := -1
		for _, c := range pu.CPUs.UnsortedList() {
			repCPU = c
			break
		}
		log.Warnf("pct: assoc-only: SST-TF disabled on pkg=%d punit=%d; "+
			"HP cores on this punit cannot exceed the standard "+
			"turbo-ratio bucket frequency. Enable with: "+
			"intel-speed-select -c %d turbo-freq enable -a",
			pu.PkgID, pu.PunitID, repCPU)
	}
}

// snapshotPunits caches the per-punit topology from the sst
// backend, intersecting each punit's CPUs with the allowed set.
// Punits whose intersection with allowed is empty are dropped --
// they cannot affect placement under this Configure(). The
// resulting punits and punitByCpu indices drive HP accounting and
// hpReserveCpus tier selection.
func (a *Allocator) snapshotPunits() {
	raw := a.sst.Punits()
	a.punits = make([]pctPunit, 0, len(raw))
	a.punitByCpu = map[int]int{}
	for _, pu := range raw {
		cpus := pu.CPUs
		if a.allowed.Size() > 0 {
			cpus = cpus.Intersection(a.allowed)
		}
		if cpus.IsEmpty() {
			continue
		}
		idx := len(a.punits)
		a.punits = append(a.punits, pctPunit{
			PkgID:            pu.PkgID,
			PunitID:          pu.PunitID,
			CPUs:             cpus,
			MaxHpCpus:        pu.MaxHpCpus,
			GuaranteedHpCpus: pu.GuaranteedHpCpus,
		})
		for _, c := range cpus.UnsortedList() {
			a.punitByCpu[c] = idx
		}
	}
}

// packageIDsFromPunits returns the set of package IDs present in
// the cached punits, in stable sorted order.
func (a *Allocator) packageIDsFromPunits() []int {
	seen := map[int]bool{}
	ids := []int{}
	for _, pu := range a.punits {
		if seen[pu.PkgID] {
			continue
		}
		seen[pu.PkgID] = true
		ids = append(ids, pu.PkgID)
	}
	sort.Ints(ids)
	return ids
}

// classifyAssocOnlyHP populates hpClasses by reading the
// programmed MaxFreq of each CLOS referenced by an assoc-only
// cpuClass. The CLOS with the largest MaxFreq is treated as HP;
// ties go to the smaller CLOS id (matching SST-CP ordered-priority
// convention where lower CLOS ids have higher priority). When no
// CLOS reports a programmed MaxFreq, no class is classified as HP
// (HP-specific hints stay quiet for that class set).
func (a *Allocator) classifyAssocOnlyHP(classes []*policyapi.CPUClass) {
	maxFreqs := map[int]int{}
	closIDs := []int{}
	for _, p := range a.classPlan {
		if _, seen := maxFreqs[p.ClosID]; seen {
			continue
		}
		cfg, ok, err := a.sst.GetClosConfig(p.ClosID)
		if err != nil {
			log.Warnf("pct: assoc-only: GetClosConfig(%d) failed: %v", p.ClosID, err)
			continue
		}
		if !ok {
			log.Infof("pct: assoc-only: CLOS %d not programmed; cannot classify HP/LP", p.ClosID)
			continue
		}
		maxFreqs[p.ClosID] = cfg.MaxFreq
		closIDs = append(closIDs, p.ClosID)
		log.Infof("pct: assoc-only: CLOS %d programmed min=%d max=%d kHz", p.ClosID, cfg.MinFreq, cfg.MaxFreq)
	}
	if len(closIDs) == 0 {
		return
	}
	sort.Ints(closIDs)
	bestClos := -1
	bestMax := -1
	for _, id := range closIDs {
		if maxFreqs[id] > bestMax {
			bestMax = maxFreqs[id]
			bestClos = id
		}
	}
	if bestClos < 0 || bestMax <= 0 {
		log.Infof("pct: assoc-only: no CLOS has a programmed MaxFreq; HP classification skipped")
		return
	}
	for _, cc := range classes {
		p, ok := a.classPlan[cc.Name]
		if !ok || p.ClosID != bestClos {
			continue
		}
		a.hpClasses[cc.Name] = true
		log.Infof("pct: cpuClass %q classified HP (assoc-only: CLOS %d MaxFreq=%d kHz)", cc.Name, bestClos, bestMax)
	}
}

// planClasses returns the PCT operating mode and the per-class
// CLOS plan derived from cpuClasses.
func (a *Allocator) planClasses(classes []*policyapi.CPUClass) (pctMode, map[string]*pctClassPlan, error) {
	plans := map[string]*pctClassPlan{}
	managed, assocOnly := false, false
	for _, cc := range classes {
		switch {
		case cc.PctPriority != "":
			managed = true
			plan := &pctClassPlan{}
			switch cc.PctPriority {
			case "high":
				plan.ClosID = pctDefaultHpClos
			case "low":
				plan.ClosID = pctDefaultLpClos
			default:
				return pctModeDisabled, nil, fmt.Errorf("cpuClass %q: invalid pctPriority %q", cc.Name, cc.PctPriority)
			}
			minSrc, maxSrc := cc.PctMinFreq, cc.PctMaxFreq
			if minSrc == 0 {
				minSrc = cc.MinFreq
			}
			if maxSrc == 0 {
				maxSrc = cc.MaxFreq
			}
			plan.MinFreq = a.resolveHWFreq(minSrc)
			plan.MaxFreq = a.resolveHWFreq(maxSrc)
			plans[cc.Name] = plan
		case cc.SstClosID != nil:
			assocOnly = true
			plans[cc.Name] = &pctClassPlan{ClosID: *cc.SstClosID}
		}
	}
	switch {
	case !managed && !assocOnly:
		return pctModeDisabled, nil, nil
	case managed && assocOnly:
		return pctModeDisabled, nil, fmt.Errorf("pct: cannot mix managed (pctPriority) and assoc-only (sstClosID) modes")
	case managed:
		return pctModeManaged, plans, nil
	default:
		return pctModeAssocOnly, plans, nil
	}
}

// resolveHWFreq returns the hardware frequency in kHz that the
// given symbolic policyapi.Frequency refers to. "turbo" resolves to the
// platform's maximum turbo frequency.
func (a *Allocator) resolveHWFreq(f policyapi.Frequency) uint {
	if f == 0 {
		return 0
	}
	info, err := discoverTurboInfo(a.sys)
	if err != nil || info == nil {
		log.Warnf("pct: cannot discover platform turbo info: %v", err)
		return uint(f)
	}
	return f.Resolve(info.minFreqKHz, info.baseFreqKHz, info.maxTurboFreqKHz)
}

// active reports whether PCT is in effect (mode != disabled).
func (a *Allocator) Active() bool {
	return a != nil && a.mode != pctModeDisabled
}

// freeClassCapacity returns the number of logical CPUs that can
// still be allocated to className, given that 'held' lists CPUs
// already consumed by some balloon on this node (any class).
//
// Same formula in managed and assoc-only modes:
//   - HP class: sum over HP-eligible punits of
//     min(GuaranteedHpCpus, |pu.CPUs intersect Allowed minus held|).
//     HP capacity is bounded by the punit's *guaranteed top-turbo*
//     HP count (smallest non-zero SST-TF bucket
//     HighPriorityCoreCount, or SST-BF HP CPU count when TF is
//     unsupported) -- not by the larger MaxHpCpus the allocator
//     uses for steering. The scheduler-visible capacity must
//     reflect how many CPUs can *actually* sustain the highest
//     turbo frequency this platform exposes; otherwise HP pods
//     get scheduled past the guaranteed-turbo headroom and fall
//     back to lower-bucket frequencies.
//   - non-HP class: |Allowed minus held|. The allocator can
//     re-associate any Allowed CPU to any CLOS on demand, so the
//     gating set is what the plugin owns, not what currently
//     lives on the target CLOS in hardware.
//
// The modes differ in how hpEligiblePunit is populated:
//   - Managed mode: every snapshotted punit is HP-eligible (the
//     plugin enables SST-TF itself via PrepareManagedMode).
//   - Assoc-only mode: a punit is HP-eligible only when SST-TF
//     is currently enabled on it (operator's responsibility).
//     Punits where TF is disabled cannot exceed the standard
//     turbo-ratio bucket and contribute 0 to HP capacity, so the
//     scheduler does not bin-pack HP pods onto nodes that cannot
//     deliver top turbo.
//
// Returns 0 for classes that have no PCT plan or when PCT is not
// active. Negative intermediate counts are clamped to 0.
func (a *Allocator) FreeClassCapacity(className string, held cpuset.CPUSet) int {
	if !a.Active() {
		return 0
	}
	if _, ok := a.classPlan[className]; !ok {
		return 0
	}
	allowed := a.allowed
	free := allowed
	if free.Size() > 0 {
		free = free.Difference(held)
	}
	if !a.classIsHighPriority(className) {
		return free.Size()
	}
	total := 0
	for idx, pu := range a.punits {
		if !a.hpEligiblePunit[idx] {
			continue
		}
		puCpus := pu.CPUs
		if allowed.Size() > 0 {
			puCpus = puCpus.Intersection(allowed)
		}
		puFree := puCpus.Difference(held).Size()
		gtdHp := pu.GuaranteedHpCpus
		if gtdHp <= 0 {
			continue
		}
		room := gtdHp
		if puFree < room {
			room = puFree
		}
		if room < 0 {
			room = 0
		}
		total += room
	}
	return total
}

// useClass associates the given CPUs to the CLOS chosen for className.
// In managed mode, CPUs whose className is not a PCT class are
// associated to the fallback CLOS. In assoc-only mode such CPUs are
// left unchanged. CPUs outside the configured Allowed set are silently
// dropped.
func (a *Allocator) UseClass(className string, cpus cpuset.CPUSet) error {
	if !a.Active() {
		return nil
	}
	if a.allowed.Size() > 0 {
		cpus = cpus.Intersection(a.allowed)
	}
	if cpus.IsEmpty() {
		return nil
	}
	a.trackHpUsage(className, cpus)
	plan, ok := a.classPlan[className]
	if !ok {
		if a.mode == pctModeAssocOnly {
			return nil
		}
		return a.associate(cpus, a.fallbackClos)
	}
	return a.associate(cpus, plan.ClosID)
}

// trackHpUsage updates per-punit HP CPU bookkeeping so cpus are
// recorded as held by an HP class if className is HP, and removed
// from HP bookkeeping otherwise. CPUs not mapped to any punit
// (e.g. outside Allowed at Configure time) are ignored: they
// cannot affect HP placement and tracking them would only confuse
// hpInUseCpus.
func (a *Allocator) trackHpUsage(className string, cpus cpuset.CPUSet) {
	if !a.hpHintsActive() {
		return
	}
	a.clearHpUsage(cpus)
	if !a.classIsHighPriority(className) {
		return
	}
	perPunit := map[int][]int{}
	for _, cpu := range cpus.UnsortedList() {
		idx, ok := a.punitByCpu[cpu]
		if !ok {
			continue
		}
		perPunit[idx] = append(perPunit[idx], cpu)
	}
	for idx, list := range perPunit {
		set := a.hpUsed[idx]
		a.hpUsed[idx] = set.Union(cpuset.New(list...))
	}
}

// clearHpUsage removes cpus from per-punit HP bookkeeping.
func (a *Allocator) clearHpUsage(cpus cpuset.CPUSet) {
	if !a.hpHintsActive() {
		return
	}
	for idx, set := range a.hpUsed {
		if remaining := set.Difference(cpus); remaining.Size() != set.Size() {
			a.hpUsed[idx] = remaining
		}
	}
}

func (a *Allocator) associate(cpus cpuset.CPUSet, clos int) error {
	list := cpus.UnsortedList()
	sort.Ints(list)
	assocs := make([]pctClosAssoc, 0, len(list))
	for _, c := range list {
		assocs = append(assocs, pctClosAssoc{CPU: c, ClosID: clos})
	}
	if err := a.sst.AssociateCPUs(assocs); err != nil {
		return fmt.Errorf("pct: associate cpus %s to CLOS %d: %w", cpus, clos, err)
	}
	log.Debugf("pct: associated cpus %s to CLOS %d", cpus, clos)
	return nil
}

// Shutdown restores the platform to its default state. Safe to
// call multiple times.
func (a *Allocator) Shutdown() error {
	if a == nil || !a.sst.Supported() {
		return nil
	}
	if a.mode != pctModeManaged {
		return nil
	}
	return a.sst.Shutdown()
}

func (a *Allocator) modeString() string {
	switch a.mode {
	case pctModeManaged:
		return "managed"
	case pctModeAssocOnly:
		return "assoc-only"
	default:
		return "disabled"
	}
}

// classIsHighPriority reports whether className is currently
// classified as PCT high priority. In managed mode this comes from
// pctPriority=high; in assoc-only mode it comes from the largest
// programmed CLOS MaxFreq (see classifyAssocOnlyHP). The two
// regimes share one map so that hints() can treat HP/non-HP
// classes uniformly.
func (a *Allocator) classIsHighPriority(className string) bool {
	if !a.Active() {
		return false
	}
	return a.hpClasses[className]
}

// hpHintsActive reports whether HP-room reasoning (hpReserveCpus,
// hpInUseCpus, trackHpUsage) is currently meaningful. It requires
// PCT to be active *and* at least one cpuClass to be classified as
// HP. In assoc-only mode without programmed CLOS frequencies this
// is false even though the allocator runs, because we cannot
// distinguish HP from LP CLOSes from the data we have.
func (a *Allocator) hpHintsActive() bool {
	return a.Active() && len(a.hpClasses) > 0
}

// closCpus returns the subset of Allowed CPUs that are currently
// associated to CLOS closID.
func (a *Allocator) closCpus(closID int) cpuset.CPUSet {
	if !a.Active() {
		return cpuset.New()
	}
	out := []int{}
	for _, cpu := range a.allowed.UnsortedList() {
		id, err := a.sst.GetCPUClosID(cpu)
		if err != nil {
			continue
		}
		if id == closID {
			out = append(out, cpu)
		}
	}
	return cpuset.New(out...)
}

// hpInUseCpus returns the union of CPUs of every punit currently
// hosting at least one HP CPU, constrained to Allowed. Expanding
// HP usage to whole-punit (rather than whole-package) granularity
// keeps the Avoid hint for non-HP classes from being unnecessarily
// broad on TPMI-class platforms with multiple punits per package.
func (a *Allocator) hpInUseCpus() cpuset.CPUSet {
	if !a.hpHintsActive() {
		return cpuset.New()
	}
	out := cpuset.New()
	for idx, used := range a.hpUsed {
		if used.IsEmpty() {
			continue
		}
		if idx < 0 || idx >= len(a.punits) {
			continue
		}
		out = out.Union(a.punits[idx].CPUs)
	}
	if a.allowed.Size() > 0 {
		out = out.Intersection(a.allowed)
	}
	return out
}

// hpReserveCpus returns the CPU set the upcoming HP allocation
// should prefer, computed with punit-granular HP-room accounting:
//
//	room(punit) = MaxHpCpus(punit) - len(hpUsed[punit] \ excludeBln)
//
// Selection follows a strict tier order:
//
//   - Tier A (single-punit win): the punit with the largest
//     non-zero room and at least requested free CPUs. Returns the
//     free CPUs of that punit.
//   - Tier B (same-package union): when no single punit can host
//     `requested` HP CPUs but some package's punits jointly can,
//     return the union of free CPUs across that package's punits.
//     The picked package is the one with the largest aggregate
//     room; ties broken by largest aggregate free-CPU count.
//   - Tier C (cross-package): never. Steering HP work across
//     sockets defeats the turbo gains it would obtain, because
//     cross-socket data traffic typically dominates per-core
//     frequency benefits.
//
// When `requested` is 0 the function falls back to Tier A only --
// pick the punit with the most HP room and at least one free CPU.
// Returns the empty set when no punit/package satisfies any tier
// or no free CPUs remain after Allowed-intersection; the caller
// then falls back to topology-only placement.
//
//   - free: free CPUs to consider for placement.
//   - excludeBln: CPUs to exclude from HP-room accounting (the
//     caller's current CPU set, e.g. when expanding an existing
//     allocation, so its current HP usage is not double-counted).
//   - requested: number of CPUs the upcoming allocation wants.
//     0 means "unknown" (initial priming before the count is
//     known); Tier A is used.
func (a *Allocator) hpReserveCpus(free cpuset.CPUSet, excludeBln cpuset.CPUSet, requested int) cpuset.CPUSet {
	if !a.hpHintsActive() {
		return cpuset.New()
	}
	if a.allowed.Size() > 0 {
		free = free.Intersection(a.allowed)
	}
	if free.IsEmpty() {
		return cpuset.New()
	}

	type punitState struct {
		free cpuset.CPUSet
		room int
	}
	states := make([]punitState, len(a.punits))
	anyKnown := false
	for i, pu := range a.punits {
		states[i].free = pu.CPUs.Intersection(free)
		if pu.MaxHpCpus <= 0 {
			// Unknown capacity for this punit: do not let it
			// influence HP steering. Leave room=0 so it never
			// wins Tier A; package-aggregate Tier B still
			// uses only known-capacity punits.
			continue
		}
		anyKnown = true
		used := a.hpUsed[i]
		if excludeBln.Size() > 0 {
			used = used.Difference(excludeBln)
		}
		room := pu.MaxHpCpus - used.Size()
		if room < 0 {
			room = 0
		}
		states[i].room = room
	}
	if !anyKnown {
		return cpuset.New()
	}

	// Tier A: best single punit that satisfies the request.
	need := requested
	if need < 1 {
		need = 1
	}
	bestIdx := -1
	bestRoom := 0
	bestFree := -1
	for i := range a.punits {
		s := states[i]
		if s.free.IsEmpty() || s.room <= 0 {
			continue
		}
		// Both the punit's free CPUs and its remaining HP
		// room must be able to host the entire request.
		if s.free.Size() < need || s.room < need {
			continue
		}
		if s.room > bestRoom || (s.room == bestRoom && s.free.Size() > bestFree) {
			bestIdx = i
			bestRoom = s.room
			bestFree = s.free.Size()
		}
	}
	if bestIdx >= 0 {
		log.Debugf("pct: hpReserveCpus tier=A punit=%d/%d room=%d free=%s",
			a.punits[bestIdx].PkgID, a.punits[bestIdx].PunitID, bestRoom, states[bestIdx].free)
		return states[bestIdx].free
	}

	// Tier B: aggregate per package; pick the package whose
	// punits together have the most room (and free CPUs).
	if requested > 0 {
		type pkgAgg struct {
			room  int
			free  cpuset.CPUSet
			freeN int
		}
		agg := map[int]*pkgAgg{}
		for i, pu := range a.punits {
			if states[i].room <= 0 || states[i].free.IsEmpty() {
				continue
			}
			e, ok := agg[pu.PkgID]
			if !ok {
				e = &pkgAgg{free: cpuset.New()}
				agg[pu.PkgID] = e
			}
			e.room += states[i].room
			e.free = e.free.Union(states[i].free)
		}
		pkgIDs := make([]int, 0, len(agg))
		for id, e := range agg {
			e.freeN = e.free.Size()
			pkgIDs = append(pkgIDs, id)
		}
		sort.Ints(pkgIDs) // deterministic tie-break order
		bestPkg := -1
		bestPkgRoom := 0
		bestPkgFree := -1
		for _, id := range pkgIDs {
			e := agg[id]
			if e.room < requested {
				continue
			}
			if e.freeN < requested {
				continue
			}
			if e.room > bestPkgRoom || (e.room == bestPkgRoom && e.freeN > bestPkgFree) {
				bestPkg = id
				bestPkgRoom = e.room
				bestPkgFree = e.freeN
			}
		}
		if bestPkg >= 0 {
			log.Debugf("pct: hpReserveCpus tier=B pkg=%d room=%d free=%s",
				bestPkg, bestPkgRoom, agg[bestPkg].free)
			return agg[bestPkg].free
		}
	}

	// Tier C is never taken: do not hint across packages.
	log.Debugf("pct: hpReserveCpus tier=none (no punit or package has %d HP room with %d free CPUs)",
		requested, free.Size())
	return cpuset.New()
}

// classClosID returns the CLOS ID that the named cpuClass maps to,
// or (-1, false) if the class has no PCT plan.
func (a *Allocator) classClosID(className string) (int, bool) {
	if !a.Active() {
		return -1, false
	}
	p, ok := a.classPlan[className]
	if !ok {
		return -1, false
	}
	return p.ClosID, true
}

// virtDevSstHpReserveHint and virtDevSstHpInUseHint are the
// human-readable hint names returned in types.CpuPreference.Name for the
// dynamic PCT placement preferences.
const (
	virtDevSstHpReserveHint = "sst-hp-reserve"
	virtDevSstHpInUseHint   = "sst-hp-in-use"
)

// virtDevSstClosHint returns the human-readable hint name for the
// CLOS-membership preference of the given CLOS ID.
func virtDevSstClosHint(closID int) string {
	return fmt.Sprintf("sst-clos-%d", closID)
}

// hints returns prefer/avoid CPU sets that PCT would like an upcoming
// allocation under intent.ClassName to honor. Returned types.CpuPreference
// sets are not yet intersected with Allowed; the handler does that.
//
// Behavior:
//   - Class has an explicit CLOS plan (assoc-only or managed): Prefer
//     CLOS-member CPUs.
//   - Class is currently classified HP: Prefer hpReserveCpus
//     (best-fit punit; same-package union as fallback), and also
//     CLOS-member CPUs. No cross-package hint is ever emitted.
//   - Class is not HP and at least one HP class exists: Avoid
//     hpInUseCpus (punits currently hosting HP work).
func (a *Allocator) Hints(intent types.AllocationIntent) types.AllocationHints {
	if a == nil || !a.Active() {
		return types.AllocationHints{}
	}
	out := types.AllocationHints{}

	if closID, ok := a.classClosID(intent.ClassName); ok {
		closCpus := a.closCpus(closID)
		if !closCpus.IsEmpty() {
			out.Prefer = append(out.Prefer, types.CpuPreference{
				Name: virtDevSstClosHint(closID),
				Cpus: closCpus,
			})
		}
	}

	if a.classIsHighPriority(intent.ClassName) {
		reserve := a.hpReserveCpus(intent.FreeCpus, intent.CurrentCpus, intent.RequestedCount)
		if !reserve.IsEmpty() {
			out.Prefer = append(out.Prefer, types.CpuPreference{
				Name: virtDevSstHpReserveHint,
				Cpus: reserve,
			})
		}
		return out
	}

	if a.hpHintsActive() {
		inUse := a.hpInUseCpus()
		if !inUse.IsEmpty() {
			out.Avoid = append(out.Avoid, types.CpuPreference{
				Name: virtDevSstHpInUseHint,
				Cpus: inUse,
			})
		}
	}
	return out
}

// turboInfo holds the platform frequency reference used by the PCT
// allocator to resolve symbolic min/base/turbo frequencies.
type turboInfo struct {
	baseFreqKHz     uint
	maxTurboFreqKHz uint
	minFreqKHz      uint
}

// discoverTurboInfo reads platform turbo capabilities from sysfs via
// the first online CPU. Returns nil if no online CPU exposes valid
// frequency data.
func discoverTurboInfo(sys Sys) (*turboInfo, error) {
	cpuIDs := sys.CPUIDs()
	if len(cpuIDs) == 0 {
		return nil, fmt.Errorf("no CPUs found in system topology")
	}
	for _, id := range cpuIDs {
		cpu := sys.CPU(id)
		if cpu == nil || !cpu.Online() {
			continue
		}
		freq := cpu.FrequencyRange()
		baseFreq := cpu.BaseFrequency()
		if freq.Min == 0 && freq.Max == 0 {
			continue
		}
		if baseFreq == 0 {
			baseFreq = freq.Max
		}
		return &turboInfo{
			baseFreqKHz:     uint(baseFreq),
			maxTurboFreqKHz: uint(freq.Max),
			minFreqKHz:      uint(freq.Min),
		}, nil
	}
	return nil, fmt.Errorf("no online CPU with valid frequency information found")
}
