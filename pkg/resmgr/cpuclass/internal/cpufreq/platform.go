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
	"fmt"

	"github.com/containers/nri-plugins/pkg/sysfs"
)

// platformTurboInfo holds platform-level turbo frequency capabilities
// discovered from sysfs.
type platformTurboInfo struct {
	baseFreqKHz     uint
	maxTurboFreqKHz uint
	minFreqKHz      uint
}

// discoverPlatformInfo populates a.turboInfo from sysfs. Failure is
// non-fatal: symbolic frequencies then resolve to 0.
func (a *Allocator) discoverPlatformInfo() {
	info, err := discoverTurboInfo(a.sys)
	if err != nil {
		log.Warnf("cpufreq: cannot discover platform turbo info: %v", err)
		return
	}
	a.turboInfo = info
}

// discoverTurboInfo reads platform turbo capabilities from sysfs. It
// uses the first online CPU's frequency range as representative.
func discoverTurboInfo(sys sysfs.System) (*platformTurboInfo, error) {
	cpuIDs := sys.CPUIDs()
	if len(cpuIDs) == 0 {
		return nil, fmt.Errorf("no CPUs found in system topology")
	}
	for _, id := range cpuIDs {
		cpu := sys.CPU(id)
		if cpu == nil || !cpu.Online() {
			continue
		}
		freq := cpu.FrequencyRange()
		baseFreq := cpu.BaseFrequency()
		if freq.Min == 0 && freq.Max == 0 {
			log.Warnf("cannot detect cpu%d frequency range, skipping platform turbo info", id)
			continue
		}
		if baseFreq == 0 {
			log.Warnf("cannot detect cpu%d base frequency, default to max", id)
			baseFreq = freq.Max
		}
		return &platformTurboInfo{
			baseFreqKHz:     uint(baseFreq),
			maxTurboFreqKHz: uint(freq.Max),
			minFreqKHz:      uint(freq.Min),
		}, nil
	}
	return nil, fmt.Errorf("no online CPU with valid frequency information found")
}
