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

// Package cpuclass is the resource-manager-wide CPU class handler.
// It owns the per-CPU frequency, c-state, uncore-frequency and
// Intel Priority Core Turbo state implied by a list of user-facing
// CPU class definitions.
//
// Policies talk to a single *Handler, constructed with New(sys).
// Configure(spec) installs (or replaces) the class set; UseClass
// pins given CPUs to a named class; Commit() flushes deferred
// per-CPU sysfs writes; Hints() returns placement preferences a
// policy can use when picking new CPUs for an allocation.
package cpuclass

import (
	"fmt"
	"sort"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/cpufreq"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/cpuidle"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/pct"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/types"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/uncorefreq"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

var log = logger.NewLogger("cpuclass")

// AllocationIntent describes an upcoming CPU allocation for which
// the caller wants placement preferences.
type AllocationIntent = types.AllocationIntent

// AllocationHints carries technology-agnostic placement preferences
// returned by Handler.Hints.
type AllocationHints = types.AllocationHints

// CpuPreference is a named CPU set carrying a single placement
// preference (prefer or avoid).
type CpuPreference = types.CpuPreference

// ConfigSpec carries cpuclass configuration applied via
// Handler.Configure. Idleness is intentionally absent: the caller
// decides which class name (if any) means "idle" and applies it via
// UseClass.
type ConfigSpec struct {
	// Classes is the user-facing list of CPU classes.
	Classes []*policyapi.CPUClass
	// TurboDomain selects the per-domain turbo arbitration scope.
	// Empty resolves to "package".
	TurboDomain string
	// Allowed bounds every cpuclass operation. CPUs outside this
	// set are silently dropped by Configure, UseClass and Hints.
	Allowed cpuset.CPUSet
}

// Handler is the sole cpuclass entry point for policy code. It owns
// construction and configuration of the per-technology allocators
// (cpufreq, pct) and writers (cpufreq, cpuidle, uncorefreq).
type Handler struct {
	sys     sysfs.System
	allowed cpuset.CPUSet

	cpufreq *cpufreq.Allocator
	pct     *pct.Allocator

	// classes is the last-applied cpuClass list. Updated on every
	// Configure() call. Used by DRADevices to enumerate published classes.
	// Caller-owned slice; not deep-copied (consistent with pct.Configure behavior).
	classes []*policyapi.CPUClass

	// defs maps synthetic class name -> resolved class definition.
	// Populated by SetClassDef calls from the cpufreq allocator.
	defs map[string]types.ClassDef
	// classNames list all known/configured class names
	classNames map[string]struct{}
	// cpuClass maps cpu id -> synthetic class name. Value "" means
	// "explicitly assigned to no class". Absent CPUs are unmanaged.
	cpuClass map[int]string
	// dirtyCPUs tracks CPUs whose class assignment or whose class
	// definition changed since the last Commit().
	dirtyCPUs map[int]bool

	freqWriter   *cpufreq.Writer
	idleWriter   *cpuidle.Writer
	uncoreWriter *uncorefreq.Writer
}

// New constructs a Handler with both internal allocators (cpufreq
// and pct) ready in a "no configuration applied" state. Configure
// must be called before the handler is usable.
func New(sys sysfs.System) (*Handler, error) {
	h := &Handler{
		sys:          sys,
		defs:         map[string]types.ClassDef{},
		cpuClass:     map[int]string{},
		dirtyCPUs:    map[int]bool{},
		freqWriter:   cpufreq.NewWriter(cpufreq.Hooks{}),
		idleWriter:   cpuidle.NewWriter(cpuidle.Hooks{}),
		uncoreWriter: uncorefreq.NewWriter(uncorefreq.Hooks{}),
	}
	freq, err := cpufreq.New(sys, h)
	if err != nil {
		return nil, fmt.Errorf("cpuclass: failed to create cpufreq allocator: %w", err)
	}
	pctA, err := pct.NewAllocator(sys)
	if err != nil {
		return nil, fmt.Errorf("cpuclass: failed to create pct allocator: %w", err)
	}
	h.cpufreq = freq
	h.pct = pctA
	return h, nil
}

// PctFreeClassCapacity returns the number of logical CPUs that the
// PCT allocator can still route into the named cpuClass on this
// node, given that 'held' lists CPUs already consumed by some
// balloon belonging to any other cpuClass. Returns 0 if PCT is
// inactive or the class has no PCT plan.
func (h *Handler) PctFreeClassCapacity(className string, held cpuset.CPUSet) int {
	if h == nil || h.pct == nil {
		return 0
	}
	return h.pct.FreeClassCapacity(className, held)
}

// PctActive reports whether PCT is in effect on this node.
func (h *Handler) PctActive() bool {
	return h != nil && h.pct != nil && h.pct.Active()
}

// PickHpCpus selects n HP-eligible CPUs from the punit identified by
// (pkgID, punitID), excluding CPUs in held and those already tracked
// in hpUsed or hpDRAUsed. Delegates to the PCT allocator. Returns an
// error when the handler or its PCT allocator is nil, or when the
// underlying pick fails (inactive allocator, punit not found, or
// insufficient HP capacity).
func (h *Handler) PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error) {
	if h == nil || h.pct == nil {
		return cpuset.New(), fmt.Errorf("cpuclass: PickHpCpus: pct allocator not initialized")
	}
	return h.pct.PickHpCpus(pkgID, punitID, n, held)
}

// ReleaseHpCpus removes cpus from DRA HP accounting on the punit
// identified by (pkgID, punitID). Delegates to the PCT allocator.
// No-op when the handler or its PCT allocator is nil, or when the
// punit is unknown (idempotent).
func (h *Handler) ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet) {
	if h == nil || h.pct == nil {
		return
	}
	h.pct.ReleaseHpCpus(pkgID, punitID, cpus)
}

// AccountHpCpus records cpus as DRA HP-held on the punit identified
// by (pkgID, punitID). Used during restart reconciliation to rebuild
// HP accounting from persisted claim state without re-allocating CPUs.
// Delegates to the PCT allocator. Returns an error when the handler
// or its PCT allocator is nil, or when accounting fails (inactive
// allocator, punit not found, or HP-ineligible punit).
func (h *Handler) AccountHpCpus(pkgID, punitID int, cpus cpuset.CPUSet) error {
	if h == nil || h.pct == nil {
		return fmt.Errorf("cpuclass: AccountHpCpus: pct allocator not initialized")
	}
	return h.pct.AccountHpCpus(pkgID, punitID, cpus)
}

// IsHPClass reports whether className is currently classified as PCT
// high priority. Delegates to the PCT allocator. Returns false when
// the handler or its PCT allocator is nil, or when the allocator is
// inactive.
func (h *Handler) IsHPClass(className string) bool {
	if h == nil || h.pct == nil {
		return false
	}
	return h.pct.IsHPClass(className)
}

// ClassForCPU returns the synthetic class name currently assigned to cpu by
// the most recent UseClass/AssignCPUs call (as tracked in h.cpuClass), or ""
// if cpu is unmanaged or explicitly assigned to no class. Primarily useful
// for tests that need to verify a UseClass call actually changed (or
// restored) a CPU's class, since Handler otherwise exposes no per-CPU class
// query. Nil-safe.
//
// This is a test-only accessor kept on the exported Handler API rather than
// behind an export_test.go shim: its only callers are cmd/plugins/topology-
// aware/policy's tests, a different package, and Go test files are never
// compiled into a package's importable surface across package boundaries —
// an export_test.go in this package would be invisible there. Duplicating
// equivalent instrumentation on the caller side (e.g. inspecting SST mock
// state directly) would require exposing Handler's internal CLOS/class
// mapping some other way, which is a larger change than this narrow,
// clearly-documented read accessor.
func (h *Handler) ClassForCPU(cpu int) string {
	if h == nil {
		return ""
	}
	return h.cpuClass[cpu]
}

// Configure (re)applies a configuration spec. Idempotent: may be
// called repeatedly with changed classes, turbo-domain mode, or
// allowed set.
func (h *Handler) Configure(spec ConfigSpec) error {
	h.allowed = spec.Allowed
	h.defs = map[string]types.ClassDef{}
	h.cpuClass = map[int]string{}
	h.dirtyCPUs = map[int]bool{}
	h.freqWriter.Reset()
	h.uncoreWriter.Reset()
	if err := h.cpufreq.Configure(spec.Classes, spec.TurboDomain, spec.Allowed); err != nil {
		return fmt.Errorf("cpuclass: cpufreq configure: %w", err)
	}
	if name, needs := uncorefreq.RequiresAvailable(h.defs); needs && !h.uncoreWriter.Available() {
		return uncorefreq.UnavailableError(name)
	}
	if err := h.pct.Configure(spec.Classes, spec.Allowed); err != nil {
		return fmt.Errorf("cpuclass: pct configure: %w", err)
	}

	// h.classes is set after all fallible operations so that a partial
	// Configure failure leaves h.classes consistent with the previously
	// committed state (not a half-applied new config).
	h.classes = spec.Classes
	h.classNames = map[string]struct{}{}
	for _, cls := range spec.Classes {
		h.classNames[cls.Name] = struct{}{}
	}

	return nil
}

// IsKnownClass returns true if the named CPU class exists. Always returns
// false before Configure() has been called at least once.
func (h *Handler) IsKnownClass(name string) bool {
	_, ok := h.classNames[name]
	return ok
}

// SetClassDef records a class definition keyed by its synthetic
// name. If the definition materially changes, every CPU currently
// assigned to that synthetic class is marked dirty. Implements the
// cpufreq.Sink interface.
func (h *Handler) SetClassDef(name string, def types.ClassDef) {
	if name == "" {
		return
	}
	prev, had := h.defs[name]
	h.defs[name] = def
	if had && prev.Equal(def) {
		return
	}
	for cpu, cls := range h.cpuClass {
		if cls == name {
			h.dirtyCPUs[cpu] = true
		}
	}
}

// AssignCPUs updates the (cpu -> synthetic class) map for the given
// CPUs. CPUs whose class changes are added to the dirty set. An
// empty class name means "no class". Implements the cpufreq.Sink
// interface.
func (h *Handler) AssignCPUs(name string, cpus []int) {
	for _, cpu := range cpus {
		prev, had := h.cpuClass[cpu]
		if had && prev == name {
			continue
		}
		h.cpuClass[cpu] = name
		h.dirtyCPUs[cpu] = true
	}
}

// Commit flushes pending cpufreq, cpuidle and uncore changes to
// sysfs. Per-property writes are deduplicated against the writers'
// lastWritten caches.
func (h *Handler) Commit() error {
	if h == nil || len(h.dirtyCPUs) == 0 {
		return nil
	}
	perClass := map[string][]int{}
	for cpu := range h.dirtyCPUs {
		name, ok := h.cpuClass[cpu]
		if !ok || name == "" {
			continue
		}
		perClass[name] = append(perClass[name], cpu)
	}
	var firstErr error
	for name, cpus := range perClass {
		sort.Ints(cpus)
		def, ok := h.defs[name]
		if !ok {
			log.Debugf("cpuclass: Commit: no definition for class %q; skipping cpus %v", name, cpus)
			continue
		}
		if err := h.freqWriter.Enforce(name, def, cpus); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := h.idleWriter.Enforce(name, def.DisabledCstates, cpus); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	dirtyDies := uncorefreq.DiesForCpus(h.sys, h.dirtyCPUs)
	if err := h.uncoreWriter.Enforce(h.sys, h.defs, h.cpuClass, dirtyDies); err != nil && firstErr == nil {
		firstErr = err
	}
	h.dirtyCPUs = map[int]bool{}
	return firstErr
}

// UseClass applies className to the given CPUs across every internal
// allocator. An empty className means "no class". CPUs outside the
// configured Allowed set are silently dropped.
func (h *Handler) UseClass(className string, cpus cpuset.CPUSet) error {
	if err := h.cpufreq.UseClass(className, cpus); err != nil {
		log.Warnf("cpuclass: cpufreq failed to apply class %q on CPUs %s: %v", className, cpus, err)
	}
	if err := h.pct.UseClass(className, cpus); err != nil {
		log.Warnf("cpuclass: pct failed to apply class %q on CPUs %s: %v", className, cpus, err)
	}
	return nil
}

// Hints returns technology-agnostic placement preferences for an
// upcoming CPU allocation. The returned CpuPreference sets are
// always subsets of the configured Allowed set.
func (h *Handler) Hints(intent AllocationIntent) AllocationHints {
	hints := h.pct.Hints(intent)
	if h.allowed.Size() > 0 {
		hints = intersectHints(hints, h.allowed)
	}
	return hints
}

// Shutdown releases any platform-level resources owned by the
// handler. Safe to call multiple times.
func (h *Handler) Shutdown() error {
	if h == nil || h.pct == nil {
		return nil
	}
	return h.pct.Shutdown()
}

// intersectHints returns a copy of hints with every CpuPreference
// candidate set constrained to the given bound. Empty candidate sets
// are dropped; a preference is dropped only when all of its candidate
// sets become empty. Candidate order is preserved.
func intersectHints(hints AllocationHints, bound cpuset.CPUSet) AllocationHints {
	out := AllocationHints{}
	clip := func(prefs []CpuPreference) []CpuPreference {
		var res []CpuPreference
		for _, p := range prefs {
			sets := make([]cpuset.CPUSet, 0, len(p.Cpus))
			for _, c := range p.Cpus {
				s := c.Intersection(bound)
				if s.IsEmpty() {
					continue
				}
				sets = append(sets, s)
			}
			if len(sets) == 0 {
				continue
			}
			res = append(res, CpuPreference{Name: p.Name, Cpus: sets})
		}
		return res
	}
	out.Prefer = clip(hints.Prefer)
	out.Avoid = clip(hints.Avoid)
	return out
}
