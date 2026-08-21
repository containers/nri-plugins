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

package cpuclass

import (
	"regexp"
	"strings"
	"testing"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/pct"
	resapi "k8s.io/api/resource/v1"
	corev1 "k8s.io/api/core/v1"
)

func ptr[T any](v T) *T { return &v }

func TestValidateCPUClassesForDRA(t *testing.T) {
	tests := []struct {
		name           string
		classes        []*policyapi.CPUClass
		sharedCounters bool
		wantErr        bool
		// errContains is a substring the error must contain (if wantErr).
		errContains string
	}{
		{
			name:    "empty class list",
			classes: nil,
			wantErr: false,
		},
		{
			name: "non-PCT classes only — all exempt",
			classes: []*policyapi.CPUClass{
				{Name: "default"},
				{Name: "idle"},
				{Name: "turbo"},
			},
			wantErr: false,
		},
		{
			name: "single managed HP class published",
			classes: []*policyapi.CPUClass{
				{Name: "hp", PctPriority: "high"},
			},
			wantErr: false,
		},
		{
			name: "single managed LP class published",
			classes: []*policyapi.CPUClass{
				{Name: "lp", PctPriority: "low"},
			},
			wantErr: false,
		},
		{
			name: "one HP + one LP managed — different tiers, ok",
			classes: []*policyapi.CPUClass{
				{Name: "hp", PctPriority: "high"},
				{Name: "lp", PctPriority: "low"},
			},
			wantErr: false,
		},
		{
			name: "two assoc-only classes with different SstClosIDs — ok",
			classes: []*policyapi.CPUClass{
				{Name: "hp-assoc", SstClosID: ptr(0)},
				{Name: "lp-assoc", SstClosID: ptr(3)},
			},
			wantErr: false,
		},
		{
			name: "two assoc-only classes with same SstClosID, sharedCounters=false — error",
			classes: []*policyapi.CPUClass{
				{Name: "class-a", SstClosID: ptr(0)},
				{Name: "class-b", SstClosID: ptr(0)},
			},
			sharedCounters: false,
			wantErr:        true,
			errContains:    "class-a",
		},
		{
			name: "two assoc-only classes with same SstClosID, sharedCounters=true — ok",
			classes: []*policyapi.CPUClass{
				{Name: "class-a", SstClosID: ptr(0)},
				{Name: "class-b", SstClosID: ptr(0)},
			},
			sharedCounters: true,
			wantErr:        false,
		},
		{
			name: "two managed HP classes, both published, sharedCounters=false — error",
			classes: []*policyapi.CPUClass{
				{Name: "hp-perf", PctPriority: "high"},
				{Name: "hp-turbo", PctPriority: "high"},
			},
			sharedCounters: false,
			wantErr:        true,
			errContains:    "hp-perf",
		},
		{
			name: "two managed HP classes, both published, sharedCounters=true — ok",
			classes: []*policyapi.CPUClass{
				{Name: "hp-perf", PctPriority: "high"},
				{Name: "hp-turbo", PctPriority: "high"},
			},
			sharedCounters: true,
			wantErr:        false,
		},
		{
			name: "two managed HP classes, one opted out — ok",
			classes: []*policyapi.CPUClass{
				{Name: "hp-perf", PctPriority: "high"},
				{Name: "hp-turbo", PctPriority: "high", DRA: &policyapi.CPUClassDRA{Publish: ptr(false)}},
			},
			sharedCounters: false,
			wantErr:        false,
		},
		{
			name: "three managed HP classes, all published — error names all",
			classes: []*policyapi.CPUClass{
				{Name: "hp-a", PctPriority: "high"},
				{Name: "hp-b", PctPriority: "high"},
				{Name: "hp-c", PctPriority: "high"},
			},
			sharedCounters: false,
			wantErr:        true,
			errContains:    "hp-a",
		},
		{
			name: "mixed PCT and non-PCT — only PCT classes checked",
			classes: []*policyapi.CPUClass{
				{Name: "default"},
				{Name: "hp", PctPriority: "high"},
				{Name: "another-default"},
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCPUClassesForDRA(tc.classes, tc.sharedCounters)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateCPUClassesForDRA() = nil, want error")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
			} else {
				if err != nil {
					t.Fatalf("ValidateCPUClassesForDRA() = %v, want nil", err)
				}
			}
		})
	}
}

// dnsLabelRe matches valid Kubernetes DNS label names (RFC 1123 subset).
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// isDNSLabel reports whether s is a valid DNS label (≤63 chars, lowercase alphanumeric + hyphens).
func isDNSLabel(s string) bool {
	return len(s) <= 63 && dnsLabelRe.MatchString(s)
}

// attrInt retrieves the integer value of a device attribute by key.
// Returns (0, false) if absent or not an int.
func attrInt(dev resapi.Device, key resapi.QualifiedName) (int64, bool) {
	a, ok := dev.Attributes[key]
	if !ok || a.IntValue == nil {
		return 0, false
	}
	return *a.IntValue, true
}

// attrStr retrieves the string value of a device attribute by key.
// Returns ("", false) if absent or not a string.
func attrStr(dev resapi.Device, key resapi.QualifiedName) (string, bool) {
	a, ok := dev.Attributes[key]
	if !ok || a.StringValue == nil {
		return "", false
	}
	return *a.StringValue, true
}

// cpusMax returns the RequestPolicy.ValidRange.Max quantity for the "nri/cpus"
// capacity entry of a device. Returns (0, false) if missing or malformed.
func cpusMax(dev resapi.Device) (int64, bool) {
	cap, ok := dev.Capacity["nri/cpus"]
	if !ok || cap.RequestPolicy == nil || cap.RequestPolicy.ValidRange == nil || cap.RequestPolicy.ValidRange.Max == nil {
		return 0, false
	}
	return cap.RequestPolicy.ValidRange.Max.Value(), true
}

func TestBuildDRADevices(t *testing.T) {
	// Shorthand helpers used only in test table.
	hpClass := func(name string) *policyapi.CPUClass {
		return &policyapi.CPUClass{Name: name, PctPriority: "high"}
	}
	lpClass := func(name string) *policyapi.CPUClass {
		return &policyapi.CPUClass{Name: name, PctPriority: "low"}
	}
	nonPCTClass := func(name string) *policyapi.CPUClass {
		return &policyapi.CPUClass{Name: name}
	}
	unpublished := func(name string) *policyapi.CPUClass {
		return &policyapi.CPUClass{
			Name:        name,
			PctPriority: "high",
			DRA:         &policyapi.CPUClassDRA{Publish: ptr(false)},
		}
	}
	punit := func(pkg, id, hpCap, nonHPCap int) pct.PunitInfo {
		return pct.PunitInfo{PkgID: pkg, PunitID: id, HPCapacity: hpCap, NonHPCapacity: nonHPCap}
	}

	// isHP returns true only for classes whose name starts with "hp".
	isHP := func(name string) bool { return strings.HasPrefix(name, "hp") }

	tests := []struct {
		name       string
		driverName string
		classes    []*policyapi.CPUClass
		punits     []pct.PunitInfo
		isHP       func(string) bool
		// wantCount is the expected number of returned devices.
		wantCount int
		// verify is an optional per-result checker.
		verify func(t *testing.T, devices []resapi.Device)
	}{
		{
			name:       "one HP class + one punit (pkg=0 punit=0)",
			driverName: "test.driver",
			classes:    []*policyapi.CPUClass{hpClass("hp")},
			punits:     []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:       isHP,
			wantCount:  1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				// AllowMultipleAllocations must be true.
				if dev.AllowMultipleAllocations == nil || !*dev.AllowMultipleAllocations {
					t.Errorf("AllowMultipleAllocations: got %v, want true", dev.AllowMultipleAllocations)
				}
				// nri/cpus RequestPolicy.ValidRange.Max must equal HPCapacity=4.
				max, ok := cpusMax(dev)
				if !ok {
					t.Errorf("nri/cpus capacity/requestPolicy not set")
				} else if max != 4 {
					t.Errorf("nri/cpus max = %d, want 4 (HPCapacity)", max)
				}
				// NodeAllocatableResourceMappings must map corev1.ResourceCPU.
				if dev.NodeAllocatableResourceMappings == nil {
					t.Errorf("NodeAllocatableResourceMappings is nil")
				} else if _, ok := dev.NodeAllocatableResourceMappings[corev1.ResourceCPU]; !ok {
					t.Errorf("NodeAllocatableResourceMappings: missing %q", corev1.ResourceCPU)
				}
				// Topology attributes present even when 0.
				if v, ok := attrInt(dev, "nri/packageID"); !ok {
					t.Errorf("nri/packageID attribute missing")
				} else if v != 0 {
					t.Errorf("nri/packageID = %d, want 0", v)
				}
				if v, ok := attrInt(dev, "nri/punitID"); !ok {
					t.Errorf("nri/punitID attribute missing")
				} else if v != 0 {
					t.Errorf("nri/punitID = %d, want 0", v)
				}
				// nri/cpuClass present.
				if v, ok := attrStr(dev, "nri/cpuClass"); !ok {
					t.Errorf("nri/cpuClass attribute missing")
				} else if v != "hp" {
					t.Errorf("nri/cpuClass = %q, want \"hp\"", v)
				}
				// Device name must be DNS-valid.
				if !isDNSLabel(dev.Name) {
					t.Errorf("device name %q is not a valid DNS label", dev.Name)
				}
			},
		},
		{
			name:      "one non-HP class + one punit → max=NonHPCapacity",
			classes:   []*policyapi.CPUClass{lpClass("lp")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				max, ok := cpusMax(devices[0])
				if !ok {
					t.Errorf("nri/cpus capacity/requestPolicy not set")
				} else if max != 8 {
					t.Errorf("nri/cpus max = %d, want 8 (NonHPCapacity)", max)
				}
			},
		},
		{
			name:      "HP class + HPCapacity==0 → device skipped for that punit",
			classes:   []*policyapi.CPUClass{hpClass("hp")},
			punits:    []pct.PunitInfo{punit(0, 0, 0, 8)},
			isHP:      isHP,
			wantCount: 0,
		},
		{
			name:      "NonHPCapacity==0 → device skipped for that punit",
			classes:   []*policyapi.CPUClass{lpClass("lp")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 0)},
			isHP:      isHP,
			wantCount: 0,
		},
		{
			name:      "class with dra.publish: false → excluded",
			classes:   []*policyapi.CPUClass{unpublished("hp-hidden")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			wantCount: 0,
		},
		{
			name: "two classes × two punits → four devices with correct names",
			classes: []*policyapi.CPUClass{
				hpClass("hp"),
				lpClass("lp"),
			},
			punits: []pct.PunitInfo{
				punit(0, 0, 4, 8),
				punit(0, 1, 4, 8),
			},
			isHP:      isHP,
			wantCount: 4,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				names := make(map[string]bool, 4)
				for _, d := range devices {
					names[d.Name] = true
					if !isDNSLabel(d.Name) {
						t.Errorf("device name %q is not a valid DNS label", d.Name)
					}
				}
				// Same class across punits must use the same sanitized base,
				// not trigger the dedup counter (e.g. "hp-2-pkg0-punit0").
				wantNames := []string{
					"hp-pkg0-punit0",
					"hp-pkg0-punit1",
					"lp-pkg0-punit0",
					"lp-pkg0-punit1",
				}
				for _, want := range wantNames {
					if !names[want] {
						t.Errorf("expected device name %q not found in %v", want, devices)
					}
				}
			},
		},
		{
			name:      "empty classes → empty result",
			classes:   nil,
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			wantCount: 0,
		},
		{
			name:      "empty punits → empty result",
			classes:   []*policyapi.CPUClass{hpClass("hp")},
			punits:    nil,
			isHP:      isHP,
			wantCount: 0,
		},
		{
			// Class name > 60 chars must produce a device name ≤ 63 chars.
			name:      "long class name → device name ≤ 63 chars",
			classes:   []*policyapi.CPUClass{hpClass("hp-this-is-a-very-long-cpuclass-name-that-exceeds-sixty-chars-total-yes")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				if len(dev.Name) > 63 {
					t.Errorf("device name %q has length %d, want ≤ 63", dev.Name, len(dev.Name))
				}
				if !isDNSLabel(dev.Name) {
					t.Errorf("device name %q is not a valid DNS label", dev.Name)
				}
			},
		},
		{
			// Two classes whose names sanitize to the same base must get distinct device names.
			name: "two classes sanitize to same base → distinct device names",
			classes: []*policyapi.CPUClass{
				// Both "hp-class" and "hp_class" sanitize to "hp-class".
				hpClass("hp-class"),
				hpClass("hp_class"),
			},
			punits: []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:   isHP,
			// Both classes have HPCapacity>0 so both emit one device.
			wantCount: 2,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				seen := map[string]bool{}
				for _, d := range devices {
					if seen[d.Name] {
						t.Errorf("duplicate device name %q", d.Name)
					}
					seen[d.Name] = true
					if !isDNSLabel(d.Name) {
						t.Errorf("device name %q is not a valid DNS label", d.Name)
					}
				}
			},
		},
		{
			// Non-PCT class must NOT emit nri/pctPriority attribute.
			name:      "non-PCT class → nri/pctPriority attribute absent",
			classes:   []*policyapi.CPUClass{nonPCTClass("default")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      func(string) bool { return false }, // non-PCT: never HP
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				if _, ok := dev.Attributes["nri/pctPriority"]; ok {
					t.Errorf("nri/pctPriority attribute must be absent for non-PCT class, got %v", dev.Attributes["nri/pctPriority"])
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isHPFn := tc.isHP
			if isHPFn == nil {
				isHPFn = func(string) bool { return false }
			}
			driverName := tc.driverName
			if driverName == "" {
				driverName = "test.driver"
			}
			devices := buildDRADevices(driverName, tc.classes, tc.punits, isHPFn)
			if len(devices) != tc.wantCount {
				t.Fatalf("buildDRADevices() returned %d devices, want %d; devices=%v",
					len(devices), tc.wantCount, deviceNames(devices))
			}
			if tc.verify != nil {
				tc.verify(t, devices)
			}
		})
	}
}

// deviceNames returns a slice of device names for use in test error messages.
func deviceNames(devices []resapi.Device) []string {
	names := make([]string, len(devices))
	for i, d := range devices {
		names[i] = d.Name
	}
	return names
}

