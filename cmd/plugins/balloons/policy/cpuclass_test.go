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

package balloons

import (
	"strings"
	"testing"

	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// fakeHintProvider returns a scripted sequence of cpuclass.AllocationHints,
// one per Hints() call. After the script is exhausted it keeps
// returning the last entry.
type fakeHintProvider struct {
	script []cpuclass.AllocationHints
	calls  int
}

func (f *fakeHintProvider) Hints(_ cpuclass.AllocationIntent) cpuclass.AllocationHints {
	i := f.calls
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	f.calls++
	return f.script[i]
}

func countHintDevs(devs []string) int {
	n := 0
	for _, d := range devs {
		if strings.HasPrefix(d, cpuClassHintDevPrefix) {
			n++
		}
	}
	return n
}

func countHintMapKeys(m map[string][]cpuset.CPUSet) int {
	n := 0
	for k := range m {
		if strings.HasPrefix(k, cpuClassHintDevPrefix) {
			n++
		}
	}
	return n
}

// TestMergeCpuClassHintsNoAccumulation verifies that repeated
// allocation rounds do not cause cpuClass hint entries to accumulate
// in cpuTreeAllocatorOptions. Each round must leave behind exactly
// the hint count reported by the provider on that round, regardless
// of how many earlier rounds added different hints.
func TestMergeCpuClassHintsNoAccumulation(t *testing.T) {
	cpusA := cpuset.MustParse("2-3")
	cpusB := cpuset.MustParse("4-5")
	cpusC := cpuset.MustParse("6-7")
	cpusAvoid := cpuset.MustParse("0-1")

	provider := &fakeHintProvider{
		script: []cpuclass.AllocationHints{
			// Round 1: one prefer (A), one avoid.
			{
				Prefer: []cpuclass.CpuPreference{{Name: "hp-reserve", Cpus: cpusA}},
				Avoid:  []cpuclass.CpuPreference{{Name: "lp-clos", Cpus: cpusAvoid}},
			},
			// Round 2: two prefers (A, B) - different name at index 1
			// so the slot-0 name stays stable, slot-1 is new.
			{
				Prefer: []cpuclass.CpuPreference{
					{Name: "hp-reserve", Cpus: cpusA},
					{Name: "extra", Cpus: cpusB},
				},
				Avoid: []cpuclass.CpuPreference{{Name: "lp-clos", Cpus: cpusAvoid}},
			},
			// Round 3: name at slot 0 CHANGES to C - without proper
			// cleanup the stale "__cls_pref_0_hp-reserve" map key from
			// rounds 1+2 would survive into round 3.
			{
				Prefer: []cpuclass.CpuPreference{{Name: "third", Cpus: cpusC}},
				Avoid:  nil,
			},
		},
	}

	opts := &cpuTreeAllocatorOptions{
		preferCloseToDevices: []string{"user-dev-A", "user-dev-B"},
		preferFarFromDevices: []string{"user-far"},
		virtDevCpusets:       map[string][]cpuset.CPUSet{},
	}

	for round := 1; round <= 3; round++ {
		mergeCpuClassHints(opts, provider, cpuclass.AllocationIntent{})

		gotPrefDevs := countHintDevs(opts.preferCloseToDevices)
		gotFarDevs := countHintDevs(opts.preferFarFromDevices)
		gotMapKeys := countHintMapKeys(opts.virtDevCpusets)

		var expPref, expFar int
		switch round {
		case 1:
			expPref, expFar = 1, 1
		case 2:
			expPref, expFar = 2, 1
		case 3:
			expPref, expFar = 1, 0
		}
		if gotPrefDevs != expPref {
			t.Errorf("round %d: preferCloseToDevices hint count = %d, want %d (slice=%v)",
				round, gotPrefDevs, expPref, opts.preferCloseToDevices)
		}
		if gotFarDevs != expFar {
			t.Errorf("round %d: preferFarFromDevices hint count = %d, want %d (slice=%v)",
				round, gotFarDevs, expFar, opts.preferFarFromDevices)
		}
		if gotMapKeys != expPref+expFar {
			t.Errorf("round %d: virtDevCpusets hint key count = %d, want %d (keys=%v)",
				round, gotMapKeys, expPref+expFar, mapKeys(opts.virtDevCpusets))
		}
	}

	// Sanity: user-supplied (non-hint) devices must survive untouched.
	if got := userDevs(opts.preferCloseToDevices); len(got) != 2 ||
		got[0] != "user-dev-A" || got[1] != "user-dev-B" {
		t.Errorf("user preferCloseToDevices were modified: got %v", got)
	}
	if got := userDevs(opts.preferFarFromDevices); len(got) != 1 || got[0] != "user-far" {
		t.Errorf("user preferFarFromDevices were modified: got %v", got)
	}
}

func TestFilterOutHintDevs(t *testing.T) {
	in := []string{"a", "__cls_pref_0_x", "b", "__cls_avoid_0_y", "c"}
	got := filterOutHintDevs(in)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("filterOutHintDevs len=%d, want %d: got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filterOutHintDevs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func userDevs(devs []string) []string {
	out := []string{}
	for _, d := range devs {
		if !strings.HasPrefix(d, cpuClassHintDevPrefix) {
			out = append(out, d)
		}
	}
	return out
}

func mapKeys(m map[string][]cpuset.CPUSet) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
