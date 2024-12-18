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

package cpu

// +k8s:deepcopy-gen=true
type Config struct {
	Classes map[string]Class `json:"classes"`
}

type Class struct {
	// MinFreq is the minimum frequency for this class.
	MinFreq uint `json:"minFreq,omitempty"`
	// MaxFreq is the maximum frequency for this class.
	MaxFreq uint `json:"maxFreq,omitempty"`
	// EnergyPerformancePreference for CPUs in this class.
	EnergyPerformancePreference uint `json:"energyPerformancePreference,omitempty"`
	// UncoreMinFreq is the minimum uncore frequency for this class.
	UncoreMinFreq uint `json:"uncoreMinFreq,omitempty"`
	// UncoreMaxFreq is the maximum uncore frequency for this class.
	UncoreMaxFreq uint `json:"uncoreMaxFreq,omitempty"`
	// CPUFreq Governor for this class.
	FreqGovernor string `json:"freqGovernor,omitempty"`
}
