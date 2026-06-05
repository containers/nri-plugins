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

package v1alpha1

import (
	"testing"

	control "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control"
	cpucfg "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/cpu"
	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	balloonscfg "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"
)

// mkSpec builds a BalloonsPolicySpec carrying the given cpuClasses
// list and legacy control.cpu.classes map. Other fields are left at
// zero values.
func mkSpec(cpuClasses []*policyapi.CPUClass, legacy map[string]cpucfg.Class) *BalloonsPolicySpec {
	return &BalloonsPolicySpec{
		Config: balloonscfg.Config{
			CPUClasses: cpuClasses,
		},
		Control: control.Config{
			CPU: cpucfg.Config{
				Classes: legacy,
			},
		},
	}
}

// TestMergeLegacy_AddsMissingNames verifies that legacy entries
// whose names do not appear in cpuClasses are appended.
func TestMergeLegacy_AddsMissingNames(t *testing.T) {
	spec := mkSpec(nil, map[string]cpucfg.Class{
		"old": {MinFreq: 1_000_000, MaxFreq: 2_000_000, FreqGovernor: "performance"},
	})
	mergeLegacyCpuClasses(spec)
	if len(spec.CPUClasses) != 1 {
		t.Fatalf("want 1 cpuClass after merge, got %d", len(spec.CPUClasses))
	}
	cc := spec.CPUClasses[0]
	if cc.Name != "old" || cc.MinFreq.KHz() != 1_000_000 || cc.MaxFreq.KHz() != 2_000_000 || cc.FreqGovernor != "performance" {
		t.Errorf("merged class wrong: %+v", cc)
	}
}

// TestMergeLegacy_ExplicitWins verifies that explicit cpuClasses
// entries take precedence over legacy entries with the same name.
func TestMergeLegacy_ExplicitWins(t *testing.T) {
	explicit := &policyapi.CPUClass{
		Name:    "hp",
		MinFreq: policyapi.FrequencyBase,
		MaxFreq: policyapi.FrequencyTurbo,
	}
	spec := mkSpec(
		[]*policyapi.CPUClass{explicit},
		map[string]cpucfg.Class{
			"hp": {MinFreq: 800_000, MaxFreq: 1_500_000},
		},
	)
	mergeLegacyCpuClasses(spec)
	if len(spec.CPUClasses) != 1 {
		t.Fatalf("want 1 cpuClass (explicit unchanged), got %d", len(spec.CPUClasses))
	}
	cc := spec.CPUClasses[0]
	if cc != explicit {
		t.Errorf("explicit entry was replaced")
	}
	if cc.MinFreq != policyapi.FrequencyBase {
		t.Errorf("explicit symbolic MinFreq overwritten, got %v", cc.MinFreq)
	}
}

// TestMergeLegacy_Idempotent verifies that running the merge twice
// does not duplicate appended entries.
func TestMergeLegacy_Idempotent(t *testing.T) {
	spec := mkSpec(nil, map[string]cpucfg.Class{
		"a": {MinFreq: 1_000_000},
		"b": {MaxFreq: 2_000_000},
	})
	mergeLegacyCpuClasses(spec)
	first := len(spec.CPUClasses)
	mergeLegacyCpuClasses(spec)
	if len(spec.CPUClasses) != first {
		t.Errorf("second merge added entries: first=%d second=%d", first, len(spec.CPUClasses))
	}
}

// TestMergeLegacy_NoLegacy_NoChange verifies that an empty legacy
// map leaves cpuClasses untouched.
func TestMergeLegacy_NoLegacy_NoChange(t *testing.T) {
	keep := &policyapi.CPUClass{Name: "x"}
	spec := mkSpec([]*policyapi.CPUClass{keep}, nil)
	mergeLegacyCpuClasses(spec)
	if len(spec.CPUClasses) != 1 || spec.CPUClasses[0] != keep {
		t.Errorf("cpuClasses unexpectedly modified: %+v", spec.CPUClasses)
	}
}
