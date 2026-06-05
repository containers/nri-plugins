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

// Package types defines the internal class-definition struct used by
// the cpuclass writers (cpufreq, cpuidle, uncorefreq). It exists as
// a separate package so each writer can depend on the same struct
// without depending on the public cpuclass API or on the
// soon-to-be-deprecated control/cpu config package.
package types

import (
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// ClassDef is the resolved, platform-aware definition of a CPU class
// as consumed by the writers. All frequency fields are in kHz; zero
// means "no enforcement". Symbolic frequencies in the user-facing
// configuration are resolved before being placed into a ClassDef.
type ClassDef struct {
	MinFreq                     uint
	MaxFreq                     uint
	EnergyPerformancePreference uint
	UncoreMinFreq               uint
	UncoreMaxFreq               uint
	FreqGovernor                string
	DisabledCstates             []string
}

// Equal reports whether two ClassDef values describe identical
// per-CPU enforcement. Used by the handler to decide whether a
// class-table change actually requires re-programming CPUs.
func (c ClassDef) Equal(other ClassDef) bool {
	if c.MinFreq != other.MinFreq ||
		c.MaxFreq != other.MaxFreq ||
		c.EnergyPerformancePreference != other.EnergyPerformancePreference ||
		c.UncoreMinFreq != other.UncoreMinFreq ||
		c.UncoreMaxFreq != other.UncoreMaxFreq ||
		c.FreqGovernor != other.FreqGovernor {
		return false
	}
	if len(c.DisabledCstates) != len(other.DisabledCstates) {
		return false
	}
	for i := range c.DisabledCstates {
		if c.DisabledCstates[i] != other.DisabledCstates[i] {
			return false
		}
	}
	return true
}

// AllocationIntent describes an upcoming CPU allocation for which
// the caller wants placement preferences. Lives here so internal
// helpers (e.g. pct) can implement Hints without depending on the
// public cpuclass package.
type AllocationIntent struct {
	ClassName      string
	CurrentCpus    cpuset.CPUSet
	FreeCpus       cpuset.CPUSet
	RequestedCount int
}

// CpuPreference is a named CPU set carrying a single placement
// preference (prefer or avoid depending on the slice it appears in).
type CpuPreference struct {
	Name string
	Cpus cpuset.CPUSet
}

// AllocationHints carries technology-agnostic placement preferences
// for an upcoming allocation. Both slices are ordered by descending
// priority.
type AllocationHints struct {
	Prefer []CpuPreference
	Avoid  []CpuPreference
}

// CPUSet aliases cpuset.CPUSet for callers that want to refer to it
// via this package without re-importing pkg/utils/cpuset.
type CPUSet = cpuset.CPUSet
