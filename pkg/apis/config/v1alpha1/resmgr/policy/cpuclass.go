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

package policy

import (
	"errors"
	"fmt"
)

// CPUClassDRA holds per-cpuClass DRA publication options.
// +k8s:deepcopy-gen=true
type CPUClassDRA struct {
	// Publish controls whether this cpuClass is exposed as a DRA device.
	// Defaults to true when unset. Set to false to hide this class from
	// DRA while keeping it usable via the normal nri-plugins config path.
	// +optional
	Publish *bool `json:"publish,omitempty"`
}

// CPUClass specifies CPU frequency, C-state, and turbo attributes
// for a CPU class.
// +k8s:deepcopy-gen=true
type CPUClass struct {
	// Name of the CPU class.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// MinFreq is the minimum CPU frequency for this class.
	// Accepts values with units: "3.2GHz", "2900MHz", "2900000kHz",
	// or a plain number in kHz. Also accepts symbolic names: "min"
	// (platform minimum), "base" (CPU base frequency), "turbo"
	// (maximum turbo frequency), resolved at runtime from sysfs.
	// When turboPriority is set, "turbo" resolves to actual turbo
	// only for the highest-priority active class; others get base.
	MinFreq Frequency `json:"minFreq,omitempty"`
	// MaxFreq is the maximum CPU frequency for this class.
	// Same format and symbolic names as MinFreq.
	MaxFreq Frequency `json:"maxFreq,omitempty"`
	// EnergyPerformancePreference for CPUs in this class.
	// +kubebuilder:validation:Minimum=0
	EnergyPerformancePreference uint `json:"energyPerformancePreference,omitempty"`
	// UncoreMinFreq is the minimum uncore frequency for this class.
	// Accepts values with units like MinFreq.
	UncoreMinFreq Frequency `json:"uncoreMinFreq,omitempty"`
	// UncoreMaxFreq is the maximum uncore frequency for this class.
	// Accepts values with units like MinFreq.
	UncoreMaxFreq Frequency `json:"uncoreMaxFreq,omitempty"`
	// FreqGovernor is the CPUFreq governor for this class
	// (e.g., "performance", "powersave", "schedutil").
	FreqGovernor string `json:"freqGovernor,omitempty"`
	// DisabledCstates lists C-states disabled for CPUs in this class.
	// Example: ["C4", "C6", "C8", "C10"]
	DisabledCstates []string `json:"disabledCstates,omitempty"`
	// TurboPriority controls exclusive turbo frequency access.
	// Among CPU classes with active CPU allocations, only the class
	// with the highest turboPriority gets the symbolic frequency "turbo"
	// resolved to the actual turbo frequency. All other classes get
	// "turbo" resolved to the base frequency instead.
	// If all classes have turboPriority 0 (default), every class
	// gets actual turbo frequencies -- no competition occurs.
	// +kubebuilder:validation:Minimum=0
	TurboPriority int `json:"turboPriority,omitempty"`
	// PctPriority requests Intel Priority Core Turbo (PCT)
	// hardware support, via SST-CP CLOSes, for CPUs in this
	// class. "high" associates the CPUs to the high-priority
	// CLOS (HP cores, typically running at Pmax). "low"
	// associates them to the low-priority CLOS (LP cores,
	// typically capped at P1). Unset = PCT is not requested
	// for this class. Mutually exclusive with SstClosID.
	// +kubebuilder:validation:Enum=high;low
	PctPriority string `json:"pctPriority,omitempty"`
	// SstClosID pins this class to a specific SST-CP CLOS ID
	// (0..ClosCount-1, typically 0..3) and signals "assoc-only"
	// mode: nri-plugin will only associate this class's CPUs to
	// the given CLOS, without touching the SoC-wide SST state
	// (no CPReset, no TFEnable, no CLOS reconfiguration). Use
	// this when an operator or the BIOS has pre-configured the
	// CLOSes. Mutually exclusive with PctPriority.
	// +kubebuilder:validation:Minimum=0
	SstClosID *int `json:"sstClosID,omitempty"`
	// PctMinFreq overrides the CLOS minimum frequency that
	// nri-plugin programs in managed mode. Defaults to MinFreq.
	// Uses the same format as MinFreq but resolves "turbo"
	// directly to the hardware maximum turbo frequency,
	// without participating in the soft turboPriority
	// arbitration. Ignored in assoc-only mode.
	PctMinFreq Frequency `json:"pctMinFreq,omitempty"`
	// PctMaxFreq overrides the CLOS maximum frequency that
	// nri-plugin programs in managed mode. Defaults to MaxFreq.
	// Same caveat as PctMinFreq.
	PctMaxFreq Frequency `json:"pctMaxFreq,omitempty"`
	// PublishExtendedResource opts this CPU class into publishing
	// a node-level extended resource, using a policy chosen name,
	// whose value reflects the number of logical CPUs that the policy
	// is currently able to route into this class on the node. The
	// scheduler can then bin-pack/spread containers by adding the
	// same resource to pod requests, avoiding HP-CPU over-subscription
	// on a single node. Has effect only when the class also carries
	// PctPriority or SstClosID. Experimental.
	PublishExtendedResource bool `json:"publishExtendedResource,omitempty"`
	// DRA holds DRA publication options for this class. Optional; defaults
	// to publish=true (the class is visible to the DRA driver).
	// +optional
	DRA *CPUClassDRA `json:"dra,omitempty"`
}

// DRAPublish reports whether this cpuClass should be published as a DRA device.
// Returns true if DRA is nil or DRA.Publish is nil.
func (cc *CPUClass) DRAPublish() bool {
	if cc.DRA == nil || cc.DRA.Publish == nil {
		return true
	}
	return *cc.DRA.Publish
}

func (cc *CPUClass) Validate() error {
	if cc.Name == "" {
		return errors.New("missing or empty name in a cpuClasses entry")
	}
	// Validate PCT fields.
	if cc.PctPriority != "" && cc.SstClosID != nil {
		return fmt.Errorf("cpuClass %q: pctPriority and sstClosID are mutually exclusive", cc.Name)
	}
	switch cc.PctPriority {
	case "", "high", "low":
	default:
		return fmt.Errorf("cpuClass %q: invalid pctPriority %q (allowed: \"high\", \"low\")", cc.Name, cc.PctPriority)
	}
	if cc.SstClosID != nil {
		if *cc.SstClosID < 0 {
			return fmt.Errorf("cpuClass %q: sstClosID must be >= 0, got %d", cc.Name, *cc.SstClosID)
		}
	}
	// pctMinFreq/pctMaxFreq only take effect in managed
	// mode (pctPriority); they program the SST CLOS that
	// policies own. With sstClosID the CLOS is
	// pre-programmed by intel-speed-select/BIOS, and
	// without any PCT field the cpuClass is not a PCT
	// class at all. In both cases these fields are silent
	// no-ops; reject them so users don't tweak values that
	// have no effect.
	if cc.PctMinFreq != 0 || cc.PctMaxFreq != 0 {
		switch {
		case cc.SstClosID != nil:
			return fmt.Errorf("cpuClass %q: pctMinFreq/pctMaxFreq require pctPriority (managed mode); they are incompatible with sstClosID, where the SST CLOS is pre-programmed by intel-speed-select/BIOS", cc.Name)
		case cc.PctPriority == "":
			return fmt.Errorf("cpuClass %q: pctMinFreq/pctMaxFreq require pctPriority (managed mode); the cpuClass is currently not a PCT class", cc.Name)
		}
	}
	// publishExtendedResource only makes sense for PCT
	// classes -- the agent computes capacity from a PCT
	// plan. Reject it on non-PCT classes so users don't
	// expect a node-level resource that will never be
	// published.
	if cc.PublishExtendedResource && cc.PctPriority == "" && cc.SstClosID == nil {
		return fmt.Errorf("cpuClass %q: publishExtendedResource requires the cpuClass to be a PCT class (set pctPriority or sstClosID)", cc.Name)
	}

	return nil
}

func ValidateCPUClasses(cpuClasses []*CPUClass) (cpuClassNames map[string]struct{}, pctManaged map[string]string, pctAssocOnly map[string]int, err error) {
	cpuClassNames = map[string]struct{}{}
	pctManaged = map[string]string{}
	pctAssocOnly = map[string]int{}

	for _, cc := range cpuClasses {
		if err = cc.Validate(); err != nil {
			return nil, nil, nil, err
		}

		if _, dup := cpuClassNames[cc.Name]; dup {
			return nil, nil, nil, fmt.Errorf("duplicate cpuClasses name: %q", cc.Name)
		}

		switch {
		case cc.PctPriority != "":
			pctManaged[cc.Name] = cc.PctPriority
		case cc.SstClosID != nil:
			pctAssocOnly[cc.Name] = *cc.SstClosID
		}
		cpuClassNames[cc.Name] = struct{}{}
	}

	if len(pctManaged) > 0 && len(pctAssocOnly) > 0 {
		return nil, nil, nil, fmt.Errorf("mixing managed (pctPriority) and assoc-only (sstClosID) PCT cpuClasses is not allowed: managed=%v, assocOnly=%v", pctManaged, pctAssocOnly)
	}
	if len(pctManaged) > 0 {
		hpClasses, lpClasses := []string{}, []string{}
		for name, prio := range pctManaged {
			if prio == "high" {
				hpClasses = append(hpClasses, name)
			} else {
				lpClasses = append(lpClasses, name)
			}
		}
		if len(hpClasses) > 1 {
			return nil, nil, nil, fmt.Errorf("at most one managed PCT cpuClass with pctPriority=high allowed, got %d: %v", len(hpClasses), hpClasses)
		}
		if len(lpClasses) > 1 {
			return nil, nil, nil, fmt.Errorf("at most one managed PCT cpuClass with pctPriority=low allowed, got %d: %v", len(lpClasses), lpClasses)
		}
	}

	return cpuClassNames, pctManaged, pctAssocOnly, nil
}
