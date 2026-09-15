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
	"errors"
	"slices"
	"testing"
)

// The e2e tests set these on a deployed plugin, so a change in what they accept
// is a change in behaviour for something outside this repository's tests.

func TestParseCacheOverrides(t *testing.T) {
	t.Run("two-l3-caches", func(t *testing.T) {
		// the shape pkg/sysfs documents: two 128M L3 caches, one per socket
		byCPU, err := parseCacheOverrides(
			`[{"cpusets": ["0-3,8-11", "4-7,12-15"], "level": 3, "size": "128M"}]`)
		if err != nil {
			t.Fatalf("parseCacheOverrides: %v", err)
		}

		// ids are handed out per level and kind, from zero
		first, second := byCPU[0], byCPU[4]
		if len(first) != 1 || len(second) != 1 {
			t.Fatalf("cpu0 has %d caches, cpu4 has %d, want 1 each",
				len(first), len(second))
		}
		if first[0].id != 0 || second[0].id != 1 {
			t.Errorf("ids are %d and %d, want 0 and 1", first[0].id, second[0].id)
		}
		if first[0] == second[0] {
			t.Error("the two cpusets share one cache")
		}

		// every CPU of a cpuset gets the same shared instance
		for _, id := range []int{0, 1, 2, 3, 8, 9, 10, 11} {
			if len(byCPU[id]) != 1 || byCPU[id][0] != first[0] {
				t.Errorf("cpu%d does not share cpu0's cache", id)
			}
		}

		if got, want := first[0].size, int64(128<<20); got != want {
			t.Errorf("size = %d, want %d", got, want)
		}
		if got := first[0].level; got != 3 {
			t.Errorf("level = %d, want 3", got)
		}
		if got := first[0].kind; got != UnifiedCache {
			t.Errorf("kind = %s, want unified: an unspecified kind is unified", got)
		}
		if got, want := first[0].cpus.List(), []int{0, 1, 2, 3, 8, 9, 10, 11}; !slices.Equal(got, want) {
			t.Errorf("CPUs = %v, want %v", got, want)
		}
	})

	t.Run("kinds-and-ordering", func(t *testing.T) {
		// a CPU's caches come out ordered by level then kind, as discovery
		// orders the real ones
		byCPU, err := parseCacheOverrides(`[
			{"cpusets": ["0"], "level": 3, "size": "8M"},
			{"cpusets": ["0"], "level": 1, "kind": "i", "size": "32k"},
			{"cpusets": ["0"], "level": 1, "kind": "data", "size": "48k"},
			{"cpusets": ["0"], "level": 2, "kind": "unified", "size": "2M"}
		]`)
		if err != nil {
			t.Fatalf("parseCacheOverrides: %v", err)
		}

		caches := byCPU[0]
		if len(caches) != 4 {
			t.Fatalf("cpu0 has %d caches, want 4", len(caches))
		}

		want := []struct {
			level int
			kind  CacheKind
			size  int64
		}{
			{1, DataCache, 48 << 10},
			{1, InstructionCache, 32 << 10},
			{2, UnifiedCache, 2 << 20},
			{3, UnifiedCache, 8 << 20},
		}
		for i, w := range want {
			got := caches[i]
			if got.level != w.level || got.kind != w.kind || got.size != w.size {
				t.Errorf("cache %d is L%d %s %d, want L%d %s %d",
					i, got.level, got.kind, got.size, w.level, w.kind, w.size)
			}
		}

		// a data and an instruction cache at one level are numbered separately
		if caches[0].id != 0 || caches[1].id != 0 {
			t.Errorf("L1d and L1i have ids %d and %d, want 0 each",
				caches[0].id, caches[1].id)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		// no level means level 1, as pkg/sysfs does
		byCPU, err := parseCacheOverrides(`[{"cpusets": ["0"], "size": "1M"}]`)
		if err != nil {
			t.Fatalf("parseCacheOverrides: %v", err)
		}
		if got := byCPU[0][0].level; got != 1 {
			t.Errorf("level = %d, want 1 when unspecified", got)
		}
	})

	t.Run("bad-input", func(t *testing.T) {
		for _, tc := range []struct{ name, json string }{
			{"not-json", `not json`},
			{"bad-cpuset", `[{"cpusets": ["nope"], "level": 3}]`},
			{"bad-kind", `[{"cpusets": ["0"], "kind": "sideways"}]`},
			{"bad-size", `[{"cpusets": ["0"], "size": "big"}]`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := parseCacheOverrides(tc.json); err == nil {
					t.Errorf("parseCacheOverrides(%s) succeeded", tc.json)
				}
			})
		}
	})
}

func TestParseFreqOverrides(t *testing.T) {
	byCPU, err := parseFreqOverrides(
		`[{"cpus": "0-3", "base": 2400000, "min": 800000, "max": 3600000},
		  {"cpus": "4-7", "base": 1800000, "min": 800000, "max": 2400000}]`)
	if err != nil {
		t.Fatalf("parseFreqOverrides: %v", err)
	}

	for _, id := range []int{0, 1, 2, 3} {
		if got := byCPU[id]; got.Base != 2400000 || got.Min != 800000 || got.Max != 3600000 {
			t.Errorf("cpu%d = %+v, want base 2400000 min 800000 max 3600000", id, got)
		}
	}
	if got := byCPU[4].Base; got != 1800000 {
		t.Errorf("cpu4 base = %d, want 1800000", got)
	}
	if _, ok := byCPU[8]; ok {
		t.Error("cpu8 got a frequency it was not given")
	}
	// nothing says anything about the governor, so it stays unknown
	if got := byCPU[0].EPP; got != EPPUnknown {
		t.Errorf("EPP = %s, want unknown", got)
	}

	for _, bad := range []string{`not json`, `[{"cpus": "nope"}]`} {
		if _, err := parseFreqOverrides(bad); err == nil {
			t.Errorf("parseFreqOverrides(%s) succeeded", bad)
		}
	}
}

func TestWithEnvOverrides(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		t.Setenv(envAtomCPUs, "2-3")
		t.Setenv(envCoreCPUs, "0-1")
		t.Setenv(envCPUFreq, `[{"cpus": "0-3", "base": 1000000}]`)

		m, err := Discover(WithFS(syntheticFS(4, true)), WithEnvOverrides())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}

		if got, want := m.CoreKindCPUs(PerformanceCore).String(), "0-1"; got != want {
			t.Errorf("P-cores = %s, want %s", got, want)
		}
		if got, want := m.CoreKindCPUs(EfficientCore).String(), "2-3"; got != want {
			t.Errorf("E-cores = %s, want %s", got, want)
		}
		if got := m.CPU(0).Freq().Base; got != 1000000 {
			t.Errorf("cpu0 base frequency = %d, want the override 1000000", got)
		}

		checkMachineInvariants(t, m)
	})

	t.Run("bad-value-names-the-variable", func(t *testing.T) {
		t.Setenv(envCaches, "not json")

		_, err := Discover(WithFS(syntheticFS(2, true)), WithEnvOverrides())
		if err == nil {
			t.Fatal("a malformed override was accepted")
		}

		var oe *overrideError
		if !errors.As(err, &oe) {
			t.Fatalf("error %v is not an overrideError", err)
		}
		if oe.name != envCaches {
			t.Errorf("the error names %s, want %s", oe.name, envCaches)
		}
		if errors.Unwrap(oe) == nil {
			t.Error("the error does not wrap the parse failure")
		}
	})

	t.Run("unset-changes-nothing", func(t *testing.T) {
		// t.Setenv with an empty value is still "set", so clear them explicitly
		for _, name := range []string{envCoreCPUs, envAtomCPUs, envCaches, envCPUFreq} {
			t.Setenv(name, "")
		}

		withOverrides, err := Discover(WithFS(syntheticFS(4, true)), WithEnvOverrides())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		plain, err := Discover(WithFS(syntheticFS(4, true)))
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}

		if !withOverrides.CoreKindCPUs(PerformanceCore).
			Equals(plain.CoreKindCPUs(PerformanceCore)) {
			t.Error("empty overrides changed the core kinds")
		}
	})
}

func TestParseSizeAndSizeString(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		fail bool
	}{
		{in: "", want: 0},
		{in: "0", want: 0},
		{in: "32K", want: 32 << 10},
		{in: "32k", want: 32 << 10},
		{in: "2M", want: 2 << 20},
		{in: "128M", want: 128 << 20},
		{in: "1G", want: 1 << 30},
		{in: "1024", want: 1024},
		{in: "big", fail: true},
		{in: "M", fail: true},
	} {
		got, err := parseSize(tc.in)
		switch {
		case tc.fail && err == nil:
			t.Errorf("parseSize(%q) = %d, want an error", tc.in, got)
		case !tc.fail && err != nil:
			t.Errorf("parseSize(%q): %v", tc.in, err)
		case !tc.fail && got != tc.want:
			t.Errorf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	// the two round-trip for the sizes the kernel actually writes
	for _, s := range []string{"32K", "2M", "128M", "1G"} {
		size, err := parseSize(s)
		if err != nil {
			t.Fatalf("parseSize(%q): %v", s, err)
		}
		if got := sizeString(size); got != s {
			t.Errorf("sizeString(parseSize(%q)) = %q", s, got)
		}
	}
	if got := sizeString(0); got != "0" {
		t.Errorf("sizeString(0) = %q, want %q", got, "0")
	}
	if got := sizeString(1500); got != "1500" {
		t.Errorf("sizeString(1500) = %q, want %q", got, "1500")
	}
}
