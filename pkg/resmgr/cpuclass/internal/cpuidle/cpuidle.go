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

// Package cpuidle is the C-state writer used by the cpuclass
// handler. It wraps the goresctrl cstates library, exposing a
// uniform Hooks-injectable interface that matches the cpufreq and
// uncorefreq writers.
package cpuidle

import (
	"fmt"

	"github.com/intel/goresctrl/pkg/cstates"

	logger "github.com/containers/nri-plugins/pkg/log"
)

var log = logger.NewLogger("cpuclass")

// Hooks lets tests intercept the cstate apply operations without
// touching real sysfs. Production use leaves all hooks nil; the
// writer then talks to the platform via goresctrl. The two hooks
// mirror the two Apply calls performed per enforce(): enable and
// disable.
type Hooks struct {
	Apply func(cpus []int, enabled, disabled []string) error
}

// Writer enforces per-class enable/disable bits across the cstate
// names exposed by the platform. The cstates handle is created
// lazily on first enforce() call that has any disabled cstates;
// hosts and tests that never request a cstate change therefore
// never touch the cpuidle sysfs.
type Writer struct {
	hooks Hooks
	cs    *cstates.Cstates
}

// NewWriter returns a Writer wired to the given hooks. Pass a
// zero-valued Hooks to use real sysfs via goresctrl.
func NewWriter(hooks Hooks) *Writer {
	return &Writer{hooks: hooks}
}

// Enforce applies the class-specific C-state enable/disable mask on
// the given CPUs. An empty disabledCstates leaves the writer
// untouched as long as the cstates handle has never been
// initialized. Returns the first error encountered.
func (w *Writer) Enforce(class string, disabledCstates []string, cpus []int) error {
	if len(cpus) == 0 {
		return nil
	}
	if len(disabledCstates) == 0 && w.cs == nil && w.hooks.Apply == nil && cstatesEnvOverridesJson == "" {
		return nil
	}
	if w.hooks.Apply != nil {
		return w.hooks.Apply(cpus, nil, disabledCstates)
	}
	if err := w.ensureHandle(); err != nil {
		return err
	}
	enabledCstates := []string{}
	for _, name := range w.cs.Names() {
		enabled := true
		for _, d := range disabledCstates {
			if name == d {
				enabled = false
				break
			}
		}
		if enabled {
			enabledCstates = append(enabledCstates, name)
		}
	}
	cpuCstates := w.cs.Copy(cstates.NewBasicFilter().SetCPUs(cpus...))
	enCpuCstates := cpuCstates.Copy(cstates.NewBasicFilter().SetCstateNames(enabledCstates...))
	disCpuCstates := cpuCstates.Copy(cstates.NewBasicFilter().SetCstateNames(disabledCstates...))
	enCpuCstates.SetAttrs(cstates.AttrDisable, "0")
	disCpuCstates.SetAttrs(cstates.AttrDisable, "1")
	log.Debugf("cstates: class %q on cpus %v: enable=%v disable=%v", class, cpus, enabledCstates, disabledCstates)
	if err := enCpuCstates.Apply(); err != nil {
		return fmt.Errorf("cannot enable cstates %v on cpus %v: %w", enabledCstates, cpus, err)
	}
	if err := disCpuCstates.Apply(); err != nil {
		return fmt.Errorf("cannot disable cstates %v on cpus %v: %w", disabledCstates, cpus, err)
	}
	return nil
}

// ensureHandle lazily creates the cstates handle, picking the
// in-memory override fs when OVERRIDE_SYS_CSTATES is set.
func (w *Writer) ensureHandle() error {
	if w.cs != nil {
		return nil
	}
	filter := cstates.NewBasicFilter().SetAttributes(cstates.AttrDisable)
	var (
		cs  *cstates.Cstates
		err error
	)
	if cstatesEnvOverridesJson != "" {
		cs, err = newCstatesFromOverride(filter)
	} else {
		cs, err = cstates.NewCstatesFromSysfs(filter)
	}
	if err != nil {
		return fmt.Errorf("failed to read C-states: %w", err)
	}
	w.cs = cs
	return nil
}
