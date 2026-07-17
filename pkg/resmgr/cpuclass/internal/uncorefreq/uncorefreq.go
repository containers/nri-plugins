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

// Package uncorefreq is the per-die uncore frequency writer used by
// the cpuclass handler. It exposes a Hooks-injectable interface
// matching the cpufreq and cpuidle writers and computes the
// effective per-die min/max as the max-wins reduction over all
// classes that have at least one CPU on the die.
package uncorefreq

import (
	"fmt"

	"github.com/intel/goresctrl/pkg/utils"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/types"
	"github.com/containers/nri-plugins/pkg/sysfs"
)

var log = logger.NewLogger("cpuclass")

// DieKey identifies one (package, die) uncore frequency domain.
type DieKey struct {
	Pkg int
	Die int
}

// Hooks lets tests intercept per-die uncore writes without touching
// real sysfs. Production use leaves all hooks nil; the writer then
// talks to the platform via goresctrl. Setting any hook also forces
// "available" to true so tests can exercise enforce paths on VMs
// without the intel_uncore_frequency driver.
type Hooks struct {
	SetMin func(pkg, die, kHz int) error
	SetMax func(pkg, die, kHz int) error
}

// uncoreWritten records the last successfully written min/max kHz
// on a single die. Used for write deduplication.
type uncoreWritten struct {
	min    uint
	max    uint
	hasMin bool
	hasMax bool
}

// Writer enforces per-die uncore frequency limits. A die with
// effective min=max=0 is left untouched. Failures on individual
// dies are logged; the first error is returned to the caller.
type Writer struct {
	hooks       Hooks
	available   bool
	lastWritten map[DieKey]uncoreWritten
}

// NewWriter returns a Writer wired to the given hooks. Pass a
// zero-valued Hooks to use real sysfs via goresctrl. The "available"
// bit is probed once; setting any hook overrides the probe.
func NewWriter(hooks Hooks) *Writer {
	available := utils.UncoreFreqAvailable()
	if hooks.SetMin != nil || hooks.SetMax != nil {
		available = true
	}
	return &Writer{
		hooks:       hooks,
		available:   available,
		lastWritten: make(map[DieKey]uncoreWritten),
	}
}

// Available reports whether the uncore frequency driver was found
// at construction time. Used by the handler to surface a helpful
// configuration error when classes request uncore limits but the
// driver is missing.
func (w *Writer) Available() bool { return w.available }

// Reset clears the per-die lastWritten cache. Called by the handler
// when class definitions or the allowed set change.
func (w *Writer) Reset() {
	w.lastWritten = make(map[DieKey]uncoreWritten)
}

// RequiresAvailable reports whether any class definition requests
// uncore limits. Used by the handler to fail Configure with a
// helpful error when classes ask for uncore but the driver is not
// loaded.
func RequiresAvailable(defs map[string]types.ClassDef) (string, bool) {
	for name, c := range defs {
		if c.UncoreMinFreq != 0 || c.UncoreMaxFreq != 0 {
			return name, true
		}
	}
	return "", false
}

// UnavailableError formats a configuration error when classes
// request uncore limits but the driver is missing.
func UnavailableError(className string) error {
	return fmt.Errorf("uncore limits set in cpu class %q but uncore driver not available; load the intel_uncore_frequency driver", className)
}

// Enforce recomputes and writes the effective uncore min/max for
// every dirty die. Parameters:
//   - sys: narrow topology surface used to enumerate CPUs per die.
//   - defs: class name -> definition.
//   - cpuClass: cpu id -> class name (current assignments).
//   - dirtyDies: set of (pkg, die) keys that need recomputation.
//
// Returns the first error encountered. Skips silently when the
// uncore driver is unavailable.
func (w *Writer) Enforce(sys sysfs.System, defs map[string]types.ClassDef, cpuClass map[int]string, dirtyDies map[DieKey]bool) error {
	if !w.available || len(dirtyDies) == 0 {
		return nil
	}
	var firstErr error
	for key := range dirtyDies {
		min, max, minCls, maxCls := effectiveUncoreFreqs(sys, key, defs, cpuClass)
		if min == 0 && max == 0 {
			log.Debugf("uncore: pkg/die %d/%d: no limits in effect", key.Pkg, key.Die)
			continue
		}
		log.Debugf("uncore: pkg/die %d/%d: min=%d (class %q) max=%d (class %q)",
			key.Pkg, key.Die, min, minCls, max, maxCls)
		state := w.lastWritten[key]
		if min > 0 && max > 0 && min > max {
			log.Warnf("uncore: pkg/die %d/%d: min %d > max %d", key.Pkg, key.Die, min, max)
		}
		if min > 0 && (!state.hasMin || state.min != min) {
			if err := w.callSetMin(key.Pkg, key.Die, int(min)); err != nil {
				log.Errorf("uncore: pkg/die %d/%d: cannot set min=%d: %v", key.Pkg, key.Die, min, err)
				if firstErr == nil {
					firstErr = err
				}
			}
			state.min = min
			state.hasMin = true
		}
		if max > 0 && (!state.hasMax || state.max != max) {
			if err := w.callSetMax(key.Pkg, key.Die, int(max)); err != nil {
				log.Errorf("uncore: pkg/die %d/%d: cannot set max=%d: %v", key.Pkg, key.Die, max, err)
				if firstErr == nil {
					firstErr = err
				}
			}
			state.max = max
			state.hasMax = true
		}
		w.lastWritten[key] = state
	}
	return firstErr
}

// effectiveUncoreFreqs computes the effective uncore min and max for
// a single die. Returns 0,0 when no class with uncore limits is
// active on the die.
func effectiveUncoreFreqs(sys sysfs.System, key DieKey, defs map[string]types.ClassDef, cpuClass map[int]string) (minFreq, maxFreq uint, minCls, maxCls string) {
	pkg := sys.Package(utils.ID(key.Pkg))
	if pkg == nil {
		return 0, 0, "", ""
	}
	dieCPUs := pkg.DieCPUSet(utils.ID(key.Die))
	seen := map[string]bool{}
	for _, cpu := range dieCPUs.UnsortedList() {
		name, ok := cpuClass[cpu]
		if !ok || name == "" {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		def, ok := defs[name]
		if !ok {
			continue
		}
		if def.UncoreMinFreq > minFreq {
			minFreq = def.UncoreMinFreq
			minCls = name
		}
		if def.UncoreMaxFreq > maxFreq {
			maxFreq = def.UncoreMaxFreq
			maxCls = name
		}
	}
	return minFreq, maxFreq, minCls, maxCls
}

// DiesForCpus returns the set of (pkg, die) keys that contain at
// least one cpu from cpus.
func DiesForCpus(sys sysfs.System, cpus map[int]bool) map[DieKey]bool {
	out := map[DieKey]bool{}
	if sys == nil {
		return out
	}
	for cpu := range cpus {
		c := sys.CPU(utils.ID(cpu))
		if c == nil {
			continue
		}
		pkgID := int(c.PackageID())
		pkg := sys.Package(utils.ID(pkgID))
		if pkg == nil {
			continue
		}
		for _, die := range pkg.DieIDs() {
			if pkg.DieCPUSet(die).Contains(cpu) {
				out[DieKey{Pkg: pkgID, Die: int(die)}] = true
				break
			}
		}
	}
	return out
}

func (w *Writer) callSetMin(pkg, die, freq int) error {
	if w.hooks.SetMin != nil {
		return w.hooks.SetMin(pkg, die, freq)
	}
	return utils.SetUncoreMinFreq(utils.ID(pkg), utils.ID(die), freq)
}

func (w *Writer) callSetMax(pkg, die, freq int) error {
	if w.hooks.SetMax != nil {
		return w.hooks.SetMax(pkg, die, freq)
	}
	return utils.SetUncoreMaxFreq(utils.ID(pkg), utils.ID(die), freq)
}
