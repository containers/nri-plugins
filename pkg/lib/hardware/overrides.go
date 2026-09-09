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

package hardware

import (
	"encoding/json"
	"fmt"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// The shapes of the OVERRIDE_SYS_CACHES and OVERRIDE_SYS_CPUFREQ variables.
// They are what pkg/sysfs accepts, so that an end-to-end test which sets them
// keeps working across the move. See [WithEnvOverrides] for why they exist.

// cacheOverride describes caches to report instead of the ones the machine has.
// Every cpuset listed gets a cache, numbered from zero within its level and
// kind.
//
//	[{"cpusets": ["0-15,32-47", "16-31,48-63"], "level": 3, "size": "128M"}]
type cacheOverride struct {
	Cpusets []string `json:"cpusets"`
	Level   int      `json:"level"`
	Kind    string   `json:"kind"`
	Size    string   `json:"size"`
}

// freqOverride describes frequencies to report instead of the ones cpufreq
// gives, for a virtual machine which has no cpufreq at all.
type freqOverride struct {
	Cpus string `json:"cpus"`
	Base uint64 `json:"base"`
	Min  uint64 `json:"min"`
	Max  uint64 `json:"max"`
}

// parseCacheOverrides turns the JSON of OVERRIDE_SYS_CACHES into the caches of
// each CPU, ordered by level and kind as discovery orders the real ones.
func parseCacheOverrides(blob string) (map[ID][]*Cache, error) {
	var overrides []cacheOverride
	if err := json.Unmarshal([]byte(blob), &overrides); err != nil {
		return nil, err
	}

	// ids are handed out per level and kind, as the kernel numbers them
	next := map[CacheID]ID{}
	byCPU := map[ID][]*Cache{}

	for _, o := range overrides {
		kind, err := parseOverrideCacheKind(o.Kind)
		if err != nil {
			return nil, err
		}

		level := o.Level
		if level <= 0 {
			level = 1
		}

		size, err := parseSize(trimmed(o.Size))
		if err != nil {
			return nil, fmt.Errorf("bad size %q: %w", o.Size, err)
		}

		for _, str := range o.Cpusets {
			cpus, err := libcpu.ParseCpuMask(trimmed(str))
			if err != nil {
				return nil, fmt.Errorf("bad cpuset %q: %w", str, err)
			}
			cpus.Seal()

			key := CacheID{Level: level, ID: int(kind)}
			cache := &Cache{
				valid: true,
				id:    next[key],
				level: level,
				kind:  kind,
				size:  size,
				cpus:  cpus,
			}
			next[key]++

			cpus.ForEachCpu(func(id int) bool {
				byCPU[id] = append(byCPU[id], cache)
				return true
			})
		}
	}

	for _, caches := range byCPU {
		sortCaches(caches)
	}

	return byCPU, nil
}

// parseOverrideCacheKind accepts the short and long spellings an override may
// use, and treats an unspecified kind as unified.
func parseOverrideCacheKind(s string) (CacheKind, error) {
	switch trimmed(s) {
	case "d", "data", "Data":
		return DataCache, nil
	case "i", "instruction", "Instruction":
		return InstructionCache, nil
	case "u", "unified", "Unified", "":
		return UnifiedCache, nil
	}
	return UnifiedCache, fmt.Errorf("unknown cache kind %q", s)
}

// parseFreqOverrides turns the JSON of OVERRIDE_SYS_CPUFREQ into the
// frequencies of each CPU.
func parseFreqOverrides(blob string) (map[ID]Freq, error) {
	var overrides []freqOverride
	if err := json.Unmarshal([]byte(blob), &overrides); err != nil {
		return nil, err
	}

	byCPU := map[ID]Freq{}
	for _, o := range overrides {
		cpus, err := libcpu.ParseCpuMask(trimmed(o.Cpus))
		if err != nil {
			return nil, fmt.Errorf("bad CPU list %q: %w", o.Cpus, err)
		}
		freq := Freq{Base: o.Base, Min: o.Min, Max: o.Max, EPP: EPPUnknown}
		cpus.ForEachCpu(func(id int) bool {
			byCPU[id] = freq
			return true
		})
	}

	return byCPU, nil
}
