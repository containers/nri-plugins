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
	"sort"

	cpucfg "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/cpu"
	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	logger "github.com/containers/nri-plugins/pkg/log"
)

var (
	_     ResmgrConfig = &BalloonsPolicy{}
	bplog              = logger.NewLogger("policy")
)

func (c *BalloonsPolicy) AgentConfig() *AgentConfig {
	if c == nil {
		return nil
	}

	a := c.Spec.Agent

	return &a
}

func (c *BalloonsPolicy) CommonConfig() *CommonConfig {
	if c == nil {
		return nil
	}
	return &CommonConfig{
		Control:         c.Spec.Control,
		Log:             c.Spec.Log,
		Instrumentation: c.Spec.Instrumentation,
	}
}

// PolicyConfig returns the balloons-specific configuration handed to
// the policy. Before returning, any legacy control.cpu.classes
// entries are folded into Spec.Config.CPUClasses (without overriding
// entries with matching names). The legacy CPU controller is no
// longer used by the balloons policy; this reverse merge preserves
// backwards compatibility so existing configurations keep working
// while users migrate to the cpuClasses syntax.
func (c *BalloonsPolicy) PolicyConfig() interface{} {
	if c == nil {
		return nil
	}
	mergeLegacyCpuClasses(&c.Spec)
	return &c.Spec.Config
}

// mergeLegacyCpuClasses appends synthetic CPUClass entries derived
// from spec.Control.CPU.Classes for names that do not already exist
// in spec.Config.CPUClasses. Conflicting names log a single warning
// per name. Idempotent: repeated calls do not add duplicate entries
// and do not warn again for the same conflict.
func mergeLegacyCpuClasses(spec *BalloonsPolicySpec) {
	legacy := spec.Control.CPU.Classes
	if len(legacy) == 0 {
		return
	}
	existing := map[string]*policyapi.CPUClass{}
	for _, cc := range spec.CPUClasses {
		existing[cc.Name] = cc
	}
	// Sort the legacy class names so warning order is deterministic.
	names := make([]string, 0, len(legacy))
	for name := range legacy {
		names = append(names, name)
	}
	sort.Strings(names)
	added := []string{}
	for _, name := range names {
		cc := legacy[name]
		if prev, ok := existing[name]; ok {
			// Skip silently when the explicit entry already
			// has the exact values converted from the legacy
			// entry. That happens when a prior PolicyConfig()
			// call already merged this spec.
			if cpuClassMatchesLegacy(prev, cc) {
				continue
			}
			bplog.Warnf("control.cpu.classes entry %q overridden by cpuClasses entry; remove the legacy entry to silence this warning", name)
			continue
		}
		synth := &policyapi.CPUClass{
			Name:                        name,
			MinFreq:                     policyapi.Frequency(cc.MinFreq),
			MaxFreq:                     policyapi.Frequency(cc.MaxFreq),
			EnergyPerformancePreference: cc.EnergyPerformancePreference,
			UncoreMinFreq:               policyapi.Frequency(cc.UncoreMinFreq),
			UncoreMaxFreq:               policyapi.Frequency(cc.UncoreMaxFreq),
			FreqGovernor:                cc.FreqGovernor,
			DisabledCstates:             append([]string(nil), cc.DisabledCstates...),
		}
		spec.CPUClasses = append(spec.CPUClasses, synth)
		existing[name] = synth
		added = append(added, name)
	}
	if len(added) > 0 {
		bplog.Warnf("control.cpu.classes is deprecated; converted to cpuClasses: %v", added)
	}
}

// cpuClassMatchesLegacy reports whether cc has the exact field
// values that the reverse converter would produce for legacy. Used
// to suppress spurious "override" warnings when the same spec is
// processed more than once.
func cpuClassMatchesLegacy(cc *policyapi.CPUClass, legacy cpucfg.Class) bool {
	if cc.MinFreq != policyapi.Frequency(legacy.MinFreq) ||
		cc.MaxFreq != policyapi.Frequency(legacy.MaxFreq) ||
		cc.EnergyPerformancePreference != legacy.EnergyPerformancePreference ||
		cc.UncoreMinFreq != policyapi.Frequency(legacy.UncoreMinFreq) ||
		cc.UncoreMaxFreq != policyapi.Frequency(legacy.UncoreMaxFreq) ||
		cc.FreqGovernor != legacy.FreqGovernor {
		return false
	}
	if len(cc.DisabledCstates) != len(legacy.DisabledCstates) {
		return false
	}
	for i := range cc.DisabledCstates {
		if cc.DisabledCstates[i] != legacy.DisabledCstates[i] {
			return false
		}
	}
	return true
}

func (c *BalloonsPolicy) Validate() error {
	if c == nil {
		return nil
	}

	if err := c.CommonConfig().Validate(); err != nil {
		return err
	}

	if err := c.Spec.Validate(); err != nil {
		return err
	}

	return nil
}
