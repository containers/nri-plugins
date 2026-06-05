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
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Frequency represents a CPU frequency value that can be specified
// with human-readable units in YAML/JSON configuration. Supported
// formats:
//   - "3.2G" or "3.2GHz" = 3200000 (kHz)
//   - "2900M" or "2900MHz" = 2900000 (kHz)
//   - "2900000k" or "2900000kHz" = 2900000 (kHz)
//   - "2900000" (bare number) = 2900000 (kHz, backwards compatible)
//   - 2900000 (JSON number) = 2900000 (kHz, backwards compatible)
//   - "min" = platform minimum frequency (resolved at runtime)
//   - "base" = CPU base frequency (resolved at runtime)
//   - "turbo" = maximum turbo frequency (resolved at runtime)
//
// The internal representation is always in kHz (the unit used by Linux
// kernel sysfs cpufreq interface). Symbolic values ("min", "base",
// "turbo") are stored as sentinel constants and must be resolved with
// Resolve() before being passed to the CPU controller.
// +kubebuilder:validation:Type=string
type Frequency uint

const (
	// FrequencyMin is a sentinel indicating the platform minimum frequency.
	FrequencyMin Frequency = math.MaxUint - 2
	// FrequencyBase is a sentinel indicating the CPU base frequency.
	FrequencyBase Frequency = math.MaxUint - 1
	// FrequencyTurbo is a sentinel indicating the maximum turbo frequency.
	FrequencyTurbo Frequency = math.MaxUint
)

var frequencyRegexp = regexp.MustCompile(`(?i)^\s*([0-9]*\.?[0-9]+)\s*(GHz|G|MHz|M|kHz|k)?\s*$`)

// parseFrequency parses a frequency string into kHz.
func parseFrequency(s string) (Frequency, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Check for symbolic frequency names.
	switch strings.ToLower(s) {
	case "min":
		return FrequencyMin, nil
	case "base":
		return FrequencyBase, nil
	case "turbo":
		return FrequencyTurbo, nil
	}

	matches := frequencyRegexp.FindStringSubmatch(s)
	if matches == nil {
		return 0, fmt.Errorf("invalid frequency %q: expected number with optional unit (GHz, MHz, kHz) or symbolic name (min, base, turbo)", s)
	}

	numStr := matches[1]
	unit := strings.ToLower(matches[2])

	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid frequency %q: %w", s, err)
	}
	if val < 0 {
		return 0, fmt.Errorf("invalid frequency %q: negative value", s)
	}

	var kHz float64
	switch unit {
	case "ghz", "g":
		kHz = val * 1_000_000
	case "mhz", "m":
		kHz = val * 1_000
	case "khz", "k":
		kHz = val
	case "":
		// Bare number: interpret as kHz for backwards compatibility
		// with the existing uint config fields.
		kHz = val
	}

	result := uint(math.Round(kHz))
	if result == 0 && val > 0 {
		return 0, fmt.Errorf("invalid frequency %q: value too small to represent in kHz", s)
	}

	return Frequency(result), nil
}

// UnmarshalJSON implements json.Unmarshaler. Accepts both JSON strings
// with units (e.g., "3.2GHz") and plain JSON numbers (interpreted as kHz).
func (f *Frequency) UnmarshalJSON(data []byte) error {
	// Try string first (quoted value with optional unit).
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, err := parseFrequency(s)
		if err != nil {
			return err
		}
		*f = parsed
		return nil
	}

	// Try plain number (backwards compatible with uint kHz).
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		if n < 0 {
			return fmt.Errorf("invalid frequency: negative value %v", n)
		}
		*f = Frequency(uint(math.Round(n)))
		return nil
	}

	return fmt.Errorf("invalid frequency: expected string or number, got %s", string(data))
}

// MarshalJSON implements json.Marshaler. Symbolic frequencies are
// marshaled as their string name; numeric values as plain numbers (kHz)
// for backwards compatibility.
func (f Frequency) MarshalJSON() ([]byte, error) {
	switch f {
	case FrequencyMin:
		return json.Marshal("min")
	case FrequencyBase:
		return json.Marshal("base")
	case FrequencyTurbo:
		return json.Marshal("turbo")
	}
	return json.Marshal(uint(f))
}

// KHz returns the frequency value in kHz. For symbolic frequencies
// (min, base, turbo) this returns the sentinel value; use Resolve()
// first to obtain the actual platform frequency.
func (f Frequency) KHz() uint {
	return uint(f)
}

// IsSymbolic returns true if this frequency is a symbolic name
// (min, base, or turbo) that requires runtime resolution.
func (f Frequency) IsSymbolic() bool {
	return f == FrequencyMin || f == FrequencyBase || f == FrequencyTurbo
}

// Resolve converts a symbolic frequency to its concrete kHz value
// using platform frequency information. For non-symbolic frequencies,
// the value is returned unchanged. The parameters are:
//   - minKHz: platform minimum frequency (cpufreq/cpuinfo_min_freq)
//   - baseKHz: CPU base frequency (cpufreq/base_frequency)
//   - turboKHz: maximum turbo frequency (cpufreq/cpuinfo_max_freq)
func (f Frequency) Resolve(minKHz, baseKHz, turboKHz uint) uint {
	switch f {
	case FrequencyMin:
		return minKHz
	case FrequencyBase:
		return baseKHz
	case FrequencyTurbo:
		return turboKHz
	}
	return uint(f)
}

// String returns a human-readable representation.
func (f Frequency) String() string {
	switch f {
	case FrequencyMin:
		return "min"
	case FrequencyBase:
		return "base"
	case FrequencyTurbo:
		return "turbo"
	}
	kHz := uint(f)
	if kHz == 0 {
		return "0"
	}
	if kHz >= 1_000_000 && kHz%1_000_000 == 0 {
		return fmt.Sprintf("%dGHz", kHz/1_000_000)
	}
	if kHz >= 1_000 && kHz%1_000 == 0 {
		return fmt.Sprintf("%dMHz", kHz/1_000)
	}
	return fmt.Sprintf("%dkHz", kHz)
}
