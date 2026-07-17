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

// Package cpufreq owns the cpufreq-side CPU-class lifecycle:
// resolution of symbolic frequencies (min/base/turbo), turbo-priority
// winner selection per turbo domain, and the per-CPU sysfs writes
// that follow. The package is consumed by the cpuclass handler and
// exposes no behavior to user-facing code.
package cpufreq

import (
	"fmt"
	"sort"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/types"
	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

var log = logger.NewLogger("cpuclass")

// Sink is the back-channel through which the allocator publishes
// resolved class definitions and per-CPU class assignments to its
// owner (the cpuclass handler). The handler turns these into per-CPU
// dirty bits and sysfs writes performed by its Commit().
type Sink interface {
	SetClassDef(name string, def types.ClassDef)
	AssignCPUs(name string, cpus []int)
}

// Allocator owns the per-turbo-domain class state for cpufreq.
type Allocator struct {
	sys         sysfs.System
	sink        Sink
	classes     []*policyapi.CPUClass
	classByName map[string]*policyapi.CPUClass
	turboDomain string
	turboInfo   *platformTurboInfo
	allowed     cpuset.CPUSet

	cpuDomain map[int]domainID
	domains   []domainID

	// activeCpus[d][className] is the set of CPUs in turbo domain d
	// currently assigned to className.
	activeCpus map[domainID]map[string]cpuset.CPUSet

	// winnerPrio[d] is the highest TurboPriority among classes that
	// had any active CPUs in domain d the last time
	// recalculateTurbo(d) ran. -1 forces the first recalculation.
	winnerPrio map[domainID]int
}

// domainID identifies one turbo arbitration domain.
type domainID int

const systemDomainID domainID = 0

const (
	turboDomainPackage = "package"
	turboDomainSystem  = "system"
)

// New returns an Allocator that publishes class definitions and
// per-CPU assignments to sink. The constructor does not push any
// class definitions; the caller follows up with Configure().
func New(sys sysfs.System, sink Sink) (*Allocator, error) {
	if sys == nil {
		return nil, fmt.Errorf("cpufreq: missing required argument sys")
	}
	if sink == nil {
		return nil, fmt.Errorf("cpufreq: missing required argument sink")
	}
	a := &Allocator{
		sys:        sys,
		sink:       sink,
		activeCpus: map[domainID]map[string]cpuset.CPUSet{},
		winnerPrio: map[domainID]int{},
	}
	a.discoverPlatformInfo()
	return a, nil
}

// Configure replaces the CPU class set, turbo domain mode and the
// set of allowed CPUs. Resets per-domain turbo winners and
// re-publishes class definitions to the sink.
func (a *Allocator) Configure(classes []*policyapi.CPUClass, turboDomain string, allowed cpuset.CPUSet) error {
	a.classes = classes
	a.classByName = make(map[string]*policyapi.CPUClass, len(classes))
	for _, cc := range classes {
		a.classByName[cc.Name] = cc
	}
	switch turboDomain {
	case "", turboDomainPackage, turboDomainSystem:
		a.turboDomain = turboDomain
	default:
		return fmt.Errorf("cpufreq: unsupported turboDomain %q (expected %q or %q)",
			turboDomain, turboDomainPackage, turboDomainSystem)
	}
	a.allowed = allowed
	a.buildCpuDomains()
	a.activeCpus = map[domainID]map[string]cpuset.CPUSet{}
	a.winnerPrio = map[domainID]int{}
	a.pushInitialClassDefinitions()
	return nil
}

// IsKnownClass reports whether the given class name is known to the
// allocator's CPUClasses configuration.
func (a *Allocator) IsKnownClass(name string) bool {
	_, ok := a.classByName[name]
	return ok
}

// resolveClassName logs an error for unknown names and returns the
// name unchanged so the caller sees what was requested.
func (a *Allocator) resolveClassName(name string) string {
	if name == "" {
		return ""
	}
	if a.IsKnownClass(name) {
		return name
	}
	log.Errorf("unknown CPU class %q", name)
	return name
}

// UseClass marks the given CPUs as active under className,
// recalculates the turbo winner of every affected turbo domain, then
// publishes per-CPU assignments to the sink. CPUs outside the
// configured Allowed set are silently dropped.
func (a *Allocator) UseClass(className string, cpus cpuset.CPUSet) error {
	if a.allowed.Size() > 0 {
		cpus = cpus.Intersection(a.allowed)
	}
	if cpus.IsEmpty() {
		return nil
	}
	className = a.resolveClassName(className)
	a.removeCpusFromAllClasses(cpus)
	byDomain := a.cpusByDomain(cpus)
	if className != "" {
		for d, dc := range byDomain {
			if a.activeCpus[d] == nil {
				a.activeCpus[d] = map[string]cpuset.CPUSet{}
			}
			a.activeCpus[d][className] = a.activeCpus[d][className].Union(dc)
		}
	}
	for d := range byDomain {
		a.recalculateTurbo(d)
	}
	for d, dc := range byDomain {
		syn := a.syntheticName(className, d)
		a.sink.AssignCPUs(syn, dc.UnsortedList())
	}
	return nil
}

// removeCpusFromAllClasses removes the given CPUs from every active
// class set, in every turbo domain.
func (a *Allocator) removeCpusFromAllClasses(cpus cpuset.CPUSet) {
	for d, perClass := range a.activeCpus {
		for name, set := range perClass {
			newSet := set.Difference(cpus)
			if newSet.IsEmpty() {
				delete(perClass, name)
			} else {
				perClass[name] = newSet
			}
		}
		if len(perClass) == 0 {
			delete(a.activeCpus, d)
		}
	}
}

func (a *Allocator) cpusByDomain(cpus cpuset.CPUSet) map[domainID]cpuset.CPUSet {
	out := map[domainID]cpuset.CPUSet{}
	for _, cpu := range cpus.UnsortedList() {
		d, ok := a.cpuDomain[cpu]
		if !ok {
			d = systemDomainID
		}
		out[d] = out[d].Union(cpuset.New(cpu))
	}
	return out
}

func (a *Allocator) buildCpuDomains() {
	a.cpuDomain = map[int]domainID{}
	seen := map[domainID]bool{}
	mode := a.turboDomain
	if mode == "" {
		mode = turboDomainPackage
	}
	for _, cpuID := range a.sys.CPUIDs() {
		if a.allowed.Size() > 0 && !a.allowed.Contains(int(cpuID)) {
			continue
		}
		c := a.sys.CPU(cpuID)
		if c == nil {
			continue
		}
		var d domainID
		switch mode {
		case turboDomainSystem:
			d = systemDomainID
		default:
			d = domainID(c.PackageID())
		}
		a.cpuDomain[int(cpuID)] = d
		seen[d] = true
	}
	a.domains = a.domains[:0]
	for d := range seen {
		a.domains = append(a.domains, d)
	}
	sort.Slice(a.domains, func(i, j int) bool { return a.domains[i] < a.domains[j] })
	for _, d := range a.domains {
		a.winnerPrio[d] = -1
	}
	log.Debugf("turbo domains (mode=%s): %v (cpu->domain: %v)", mode, a.domains, a.cpuDomain)
}

// syntheticName returns the per-domain internal name used to track a
// user-facing class in a turbo domain. Empty class names pass
// through unchanged.
func (a *Allocator) syntheticName(name string, d domainID) string {
	if name == "" {
		return ""
	}
	if _, ok := a.classByName[name]; !ok {
		return name
	}
	return fmt.Sprintf("%s@d%d", name, d)
}

// pushInitialClassDefinitions resolves symbolic frequencies in every
// CPUClass and publishes the resulting types.ClassDef to the sink,
// once per (class, turbo domain) pair.
func (a *Allocator) pushInitialClassDefinitions() {
	if len(a.domains) == 0 {
		return
	}
	for _, cc := range a.classes {
		def := classDefFromCPUClass(cc, a.turboInfo, 0)
		for _, d := range a.domains {
			a.sink.SetClassDef(a.syntheticName(cc.Name, d), def)
		}
		log.Infof("cpuClass %q configured: minFreq=%s(%d) maxFreq=%s(%d) disabledCstates=%v",
			cc.Name, cc.MinFreq, def.MinFreq, cc.MaxFreq, def.MaxFreq, cc.DisabledCstates)
	}
}

// recalculateTurbo resolves exclusive turbo frequency access in the
// given turbo domain based on TurboPriority across all CPU classes
// that currently have active CPUs in that domain. See the in-tree
// design notes for the algorithm.
func (a *Allocator) recalculateTurbo(d domainID) {
	if len(a.classes) == 0 {
		return
	}
	newPrio := 0
	if perClass, ok := a.activeCpus[d]; ok {
		for _, cc := range a.classes {
			if cc.TurboPriority <= newPrio {
				continue
			}
			if set, ok := perClass[cc.Name]; ok && !set.IsEmpty() {
				newPrio = cc.TurboPriority
			}
		}
	}
	if prev, ok := a.winnerPrio[d]; ok && prev == newPrio {
		return
	}
	a.winnerPrio[d] = newPrio
	if a.turboInfo == nil {
		log.Warnf("turbo recalculation skipped (domain %d): no platform turbo info", d)
		return
	}
	for _, cc := range a.classes {
		effectiveTurboKHz := a.turboInfo.baseFreqKHz
		if newPrio == 0 || cc.TurboPriority >= newPrio {
			effectiveTurboKHz = a.turboInfo.maxTurboFreqKHz
		}
		def := classDefFromCPUClass(cc, a.turboInfo, effectiveTurboKHz)
		a.sink.SetClassDef(a.syntheticName(cc.Name, d), def)
		log.Infof("turbo: domain=%d class %q (prio=%d, winner=%v): minFreq=%d maxFreq=%d",
			d, cc.Name, cc.TurboPriority,
			newPrio == 0 || cc.TurboPriority >= newPrio,
			def.MinFreq, def.MaxFreq)
	}
}

// classDefFromCPUClass converts a user-facing CPUClass to a
// resolved ClassDef. When info is nil, symbolic frequencies resolve
// to 0; when info is non-nil they resolve to the corresponding
// platform value (with effectiveTurboKHz overriding the turbo
// sentinel if non-zero).
func classDefFromCPUClass(cc *policyapi.CPUClass, info *platformTurboInfo, effectiveTurboKHz uint) types.ClassDef {
	resolve := func(f policyapi.Frequency) uint {
		if info != nil {
			turboKHz := info.maxTurboFreqKHz
			if effectiveTurboKHz > 0 {
				turboKHz = effectiveTurboKHz
			}
			return f.Resolve(info.minFreqKHz, info.baseFreqKHz, turboKHz)
		}
		if f.IsSymbolic() {
			return 0
		}
		return f.KHz()
	}
	return types.ClassDef{
		MinFreq:                     resolve(cc.MinFreq),
		MaxFreq:                     resolve(cc.MaxFreq),
		EnergyPerformancePreference: cc.EnergyPerformancePreference,
		UncoreMinFreq:               resolve(cc.UncoreMinFreq),
		UncoreMaxFreq:               resolve(cc.UncoreMaxFreq),
		FreqGovernor:                cc.FreqGovernor,
		DisabledCstates:             cc.DisabledCstates,
	}
}
