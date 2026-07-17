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

package cpufreq

import (
	"github.com/intel/goresctrl/pkg/utils"

	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/types"
)

// Hooks lets tests intercept per-CPU writes without touching real
// sysfs. Production use leaves all hooks nil; the writer then talks
// to the platform via goresctrl.
type Hooks struct {
	SetMin func(cpu, kHz int) error
	SetMax func(cpu, kHz int) error
	SetGov func(cpu int, governor string) error
}

// cpufreqWritten records the last successfully written values on a
// single CPU. Used for write deduplication.
type cpufreqWritten struct {
	min      uint
	max      uint
	governor string
	hasMin   bool
	hasMax   bool
	hasGov   bool
}

// Writer is the direct per-CPU cpufreq sysfs writer. Properties are
// written only when the desired value differs from the last
// successfully written one. Failures on individual CPUs/properties
// are logged but do not stop processing of the remaining ones; the
// first error encountered is returned.
type Writer struct {
	hooks       Hooks
	lastWritten map[int]cpufreqWritten
}

// NewWriter returns a Writer wired to the given hooks. Pass a
// zero-valued Hooks to use real sysfs via goresctrl.
func NewWriter(hooks Hooks) *Writer {
	return &Writer{
		hooks:       hooks,
		lastWritten: make(map[int]cpufreqWritten),
	}
}

// Reset clears the per-CPU lastWritten cache so the next Enforce
// pass re-writes every desired value. Called by the handler when
// class definitions or the allowed set change.
func (w *Writer) Reset() {
	w.lastWritten = make(map[int]cpufreqWritten)
}

// Forget drops the lastWritten cache entries for the given CPUs.
func (w *Writer) Forget(cpus ...int) {
	for _, c := range cpus {
		delete(w.lastWritten, c)
	}
}

// Enforce writes min/max/governor to sysfs for every CPU in cpus,
// skipping properties whose desired value matches the last written
// one. A zero min or max means "don't enforce". An empty governor
// means "don't enforce". The first error encountered is returned.
func (w *Writer) Enforce(class string, def types.ClassDef, cpus []int) error {
	if len(cpus) == 0 {
		return nil
	}
	min := def.MinFreq
	max := def.MaxFreq
	governor := def.FreqGovernor

	var firstErr error
	for _, cpu := range cpus {
		state := w.lastWritten[cpu]

		if min > 0 && (!state.hasMin || state.min != min) {
			log.Debugf("enforcing cpu frequency min %d from class %q on cpu %d", min, class, cpu)
			if err := w.callSetMin(cpu, int(min)); err != nil {
				log.Errorf("cpufreq: cpu%d: cannot set min=%d: %v", cpu, min, err)
				if firstErr == nil {
					firstErr = err
				}
			}
			state.min = min
			state.hasMin = true
		}

		if max > 0 && (!state.hasMax || state.max != max) {
			log.Debugf("enforcing cpu frequency max %d from class %q on cpu %d", max, class, cpu)
			if err := w.callSetMax(cpu, int(max)); err != nil {
				log.Errorf("cpufreq: cpu%d: cannot set max=%d: %v", cpu, max, err)
				if firstErr == nil {
					firstErr = err
				}
			}
			state.max = max
			state.hasMax = true
		}

		if governor != "" && (!state.hasGov || state.governor != governor) {
			log.Debugf("enforcing cpu frequency governor %q from class %q on cpu %d", governor, class, cpu)
			if err := w.callSetGov(cpu, governor); err != nil {
				log.Errorf("cpufreq: cpu%d: cannot set governor=%q: %v", cpu, governor, err)
				if firstErr == nil {
					firstErr = err
				}
			}
			state.governor = governor
			state.hasGov = true
		}

		w.lastWritten[cpu] = state
	}

	return firstErr
}

func (w *Writer) callSetMin(cpu, freq int) error {
	if w.hooks.SetMin != nil {
		return w.hooks.SetMin(cpu, freq)
	}
	return utils.SetCPUScalingMinFreq(utils.ID(cpu), freq)
}

func (w *Writer) callSetMax(cpu, freq int) error {
	if w.hooks.SetMax != nil {
		return w.hooks.SetMax(cpu, freq)
	}
	return utils.SetCPUScalingMaxFreq(utils.ID(cpu), freq)
}

func (w *Writer) callSetGov(cpu int, governor string) error {
	if w.hooks.SetGov != nil {
		return w.hooks.SetGov(cpu, governor)
	}
	return utils.SetCPUScalingGovernor(utils.ID(cpu), governor)
}
