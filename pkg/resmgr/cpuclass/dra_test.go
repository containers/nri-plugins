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

	corev1 "k8s.io/api/core/v1"
	resapi "k8s.io/api/resource/v1"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/pct"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
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
			name: "two assoc-only classes with same SstClosID, sharedCounters=false — ok (assoc-only exempt, never HP-published)",
			classes: []*policyapi.CPUClass{
				{Name: "class-a", SstClosID: ptr(0)},
				{Name: "class-b", SstClosID: ptr(0)},
			},
			sharedCounters: false,
			wantErr:        false,
		},
		{
			name: "two assoc-only classes with same SstClosID, sharedCounters=true — rejected",
			classes: []*policyapi.CPUClass{
				{Name: "class-a", SstClosID: ptr(0)},
				{Name: "class-b", SstClosID: ptr(0)},
			},
			sharedCounters: true,
			wantErr:        true,
			errContains:    "sharedCounters",
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
			name: "two managed HP classes, both published, sharedCounters=true — rejected",
			classes: []*policyapi.CPUClass{
				{Name: "hp-perf", PctPriority: "high"},
				{Name: "hp-turbo", PctPriority: "high"},
			},
			sharedCounters: true,
			wantErr:        true,
			errContains:    "sharedCounters",
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

// cpusCapacity returns the DeviceCapacity Value for the "nri/cpus" entry.
func cpusCapacity(dev resapi.Device) (int64, bool) {
	cap, ok := dev.Capacity["nri/cpus"]
	if !ok {
		return 0, false
	}
	return cap.Value.Value(), true
}

// cpusDefault returns the RequestPolicy.Default value for "nri/cpus".
// Returns (0, false) if missing or malformed.
func cpusDefault(dev resapi.Device) (int64, bool) {
	cap, ok := dev.Capacity["nri/cpus"]
	if !ok || cap.RequestPolicy == nil || cap.RequestPolicy.Default == nil {
		return 0, false
	}
	return cap.RequestPolicy.Default.Value(), true
}

// cpusMin returns the RequestPolicy.ValidRange.Min value for "nri/cpus".
// Returns (0, false) if missing or malformed.
func cpusMin(dev resapi.Device) (int64, bool) {
	cap, ok := dev.Capacity["nri/cpus"]
	if !ok || cap.RequestPolicy == nil || cap.RequestPolicy.ValidRange == nil || cap.RequestPolicy.ValidRange.Min == nil {
		return 0, false
	}
	return cap.RequestPolicy.ValidRange.Min.Value(), true
}

// cpusStep returns the RequestPolicy.ValidRange.Step value for "nri/cpus".
// Returns (0, false) if missing or malformed.
func cpusStep(dev resapi.Device) (int64, bool) {
	cap, ok := dev.Capacity["nri/cpus"]
	if !ok || cap.RequestPolicy == nil || cap.RequestPolicy.ValidRange == nil || cap.RequestPolicy.ValidRange.Step == nil {
		return 0, false
	}
	return cap.RequestPolicy.ValidRange.Step.Value(), true
}

// checkDeviceShape asserts the shape invariants that every emitted device must
// satisfy: AllowMultipleAllocations, DeviceCapacity.Value, the full
// RequestPolicy (Default/Min/Max/Step), and NodeAllocatableResources content
// (Mapping.CapacityKey + Mapping.CapacityMultiplier). It also verifies
// topology attributes and the nri/cpuClass attribute.
//
// wantPctPriorityPresent controls whether nri/pctPriority is expected to exist
// (PCT classes) or must be absent (non-PCT classes).
func checkDeviceShape(t *testing.T, dev resapi.Device, wantCapacity int64,
	wantClass, wantPctPriority string, wantPctPriorityPresent bool) {
	t.Helper()

	// AllowMultipleAllocations must be true.
	if dev.AllowMultipleAllocations == nil || !*dev.AllowMultipleAllocations {
		t.Errorf("AllowMultipleAllocations: got %v, want true", dev.AllowMultipleAllocations)
	}

	// nri/cpuClass.
	if v, ok := attrStr(dev, "nri/cpuClass"); !ok {
		t.Error("nri/cpuClass attribute missing")
	} else if v != wantClass {
		t.Errorf("nri/cpuClass = %q, want %q", v, wantClass)
	}

	// nri/pctPriority.
	_, hasPct := dev.Attributes["nri/pctPriority"]
	if wantPctPriorityPresent {
		if v, ok := attrStr(dev, "nri/pctPriority"); !ok {
			t.Errorf("nri/pctPriority attribute missing (want %q)", wantPctPriority)
		} else if v != wantPctPriority {
			t.Errorf("nri/pctPriority = %q, want %q", v, wantPctPriority)
		}
	} else if hasPct {
		t.Errorf("nri/pctPriority must be absent, got %v", dev.Attributes["nri/pctPriority"])
	}

	// DeviceCapacity.Value (outer field, independent of ValidRange.Max).
	if cv, ok := cpusCapacity(dev); !ok {
		t.Error("nri/cpus capacity Value missing")
	} else if cv != wantCapacity {
		t.Errorf("nri/cpus capacity Value = %d, want %d", cv, wantCapacity)
	}

	// RequestPolicy: Default must be 1.
	if dv, ok := cpusDefault(dev); !ok {
		t.Error("nri/cpus RequestPolicy.Default missing")
	} else if dv != 1 {
		t.Errorf("nri/cpus RequestPolicy.Default = %d, want 1", dv)
	}

	// RequestPolicy.ValidRange: Min must be 1.
	if mv, ok := cpusMin(dev); !ok {
		t.Error("nri/cpus RequestPolicy.ValidRange.Min missing")
	} else if mv != 1 {
		t.Errorf("nri/cpus RequestPolicy.ValidRange.Min = %d, want 1", mv)
	}

	// RequestPolicy.ValidRange: Max must equal capacity.
	if xv, ok := cpusMax(dev); !ok {
		t.Error("nri/cpus RequestPolicy.ValidRange.Max missing")
	} else if xv != wantCapacity {
		t.Errorf("nri/cpus RequestPolicy.ValidRange.Max = %d, want %d", xv, wantCapacity)
	}

	// RequestPolicy.ValidRange: Step must be 1.
	if sv, ok := cpusStep(dev); !ok {
		t.Error("nri/cpus RequestPolicy.ValidRange.Step missing")
	} else if sv != 1 {
		t.Errorf("nri/cpus RequestPolicy.ValidRange.Step = %d, want 1", sv)
	}

	// NodeAllocatableResources content.
	if dev.NodeAllocatableResources == nil {
		t.Error("NodeAllocatableResources is nil")
	} else {
		r, ok := dev.NodeAllocatableResources[corev1.ResourceCPU]
		if !ok {
			t.Errorf("NodeAllocatableResources: missing %q key", corev1.ResourceCPU)
		} else if r.Mapping == nil {
			t.Errorf("NodeAllocatableResources[cpu].Mapping is nil")
		} else {
			if r.Mapping.CapacityKey == nil || *r.Mapping.CapacityKey != "nri/cpus" {
				t.Errorf("NodeAllocatableResources[cpu].Mapping.CapacityKey = %v, want \"nri/cpus\"", r.Mapping.CapacityKey)
			}
			if r.Mapping.CapacityMultiplier == nil || r.Mapping.CapacityMultiplier.Value() != 1 {
				t.Errorf("NodeAllocatableResources[cpu].Mapping.CapacityMultiplier = %v, want 1",
					r.Mapping.CapacityMultiplier)
			}
		}
	}
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
		name    string
		classes []*policyapi.CPUClass
		punits  []pct.PunitInfo
		isHP    func(string) bool
		hpOnly  bool
		// wantCount is the expected number of returned devices.
		wantCount int
		// verify is an optional per-result checker.
		verify func(t *testing.T, devices []resapi.Device)
	}{
		{
			name:      "one HP class + one punit (pkg=0 punit=0)",
			classes:   []*policyapi.CPUClass{hpClass("hp")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				// Full device-shape invariants: capacity=HPCapacity=4, PCT class "hp"/"high".
				checkDeviceShape(t, dev, 4, "hp", "high", true)
				// Topology attributes present and correct.
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
				// Device name must be DNS-valid.
				if !isDNSLabel(dev.Name) {
					t.Errorf("device name %q is not a valid DNS label", dev.Name)
				}
			},
		},
		{
			name:      "one non-HP PCT class + one punit → max=NonHPCapacity",
			classes:   []*policyapi.CPUClass{lpClass("lp")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				// Full device-shape invariants: capacity=NonHPCapacity=8, PCT class "lp"/"low".
				checkDeviceShape(t, dev, 8, "lp", "low", true)
				// Topology attributes.
				if _, ok := attrInt(dev, "nri/packageID"); !ok {
					t.Error("nri/packageID attribute missing")
				}
				if _, ok := attrInt(dev, "nri/punitID"); !ok {
					t.Error("nri/punitID attribute missing")
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
				// Every device must have AllowMultipleAllocations=true.
				for _, d := range devices {
					if d.AllowMultipleAllocations == nil || !*d.AllowMultipleAllocations {
						t.Errorf("device %q: AllowMultipleAllocations not true", d.Name)
					}
				}
				// Every device must carry nri/cpuClass pointing to the right class.
				for _, d := range devices {
					v, ok := attrStr(d, "nri/cpuClass")
					if !ok {
						t.Errorf("device %q: nri/cpuClass missing", d.Name)
						continue
					}
					// Name encodes the class base: "hp-pkg..." vs "lp-pkg...".
					wantClass := "hp"
					if strings.HasPrefix(d.Name, "lp-") {
						wantClass = "lp"
					}
					if v != wantClass {
						t.Errorf("device %q: nri/cpuClass = %q, want %q", d.Name, v, wantClass)
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
			// 2-digit pkg+punit IDs ("-pkg10-punit10" = 14 chars) combined with a
			// max-length base and a dedup suffix must still fit in 63 chars.
			name:      "2-digit pkg and punit IDs → device name ≤ 63 chars",
			classes:   []*policyapi.CPUClass{hpClass("hp-this-is-a-very-long-cpuclass-name-that-exceeds-sixty-chars-total-yes")},
			punits:    []pct.PunitInfo{punit(10, 10, 4, 8)},
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
			// Non-PCT class must NOT emit nri/pctPriority attribute; must still
			// carry topology and cpuClass attributes, and AllowMultipleAllocations.
			name:      "non-PCT class → nri/pctPriority absent, topology present",
			classes:   []*policyapi.CPUClass{nonPCTClass("default")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      func(string) bool { return false }, // non-PCT: never HP
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				// pctPriority must be absent.
				if _, ok := dev.Attributes["nri/pctPriority"]; ok {
					t.Errorf("nri/pctPriority must be absent for non-PCT class, got %v",
						dev.Attributes["nri/pctPriority"])
				}
				// AllowMultipleAllocations must still be true.
				if dev.AllowMultipleAllocations == nil || !*dev.AllowMultipleAllocations {
					t.Errorf("AllowMultipleAllocations: got %v, want true", dev.AllowMultipleAllocations)
				}
				// Topology attributes must be present.
				if _, ok := attrInt(dev, "nri/packageID"); !ok {
					t.Error("nri/packageID attribute missing")
				}
				if _, ok := attrInt(dev, "nri/punitID"); !ok {
					t.Error("nri/punitID attribute missing")
				}
				// nri/cpuClass must still be present.
				if v, ok := attrStr(dev, "nri/cpuClass"); !ok {
					t.Error("nri/cpuClass attribute missing")
				} else if v != "default" {
					t.Errorf("nri/cpuClass = %q, want \"default\"", v)
				}
			},
		},
		{
			// Non-zero PkgID and PunitID must appear correctly in attributes and name.
			name:      "non-zero PkgID and PunitID → correct in attrs and name",
			classes:   []*policyapi.CPUClass{hpClass("hp")},
			punits:    []pct.PunitInfo{punit(2, 3, 5, 6)}, // pkg=2, punit=3
			isHP:      isHP,
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				if v, ok := attrInt(dev, "nri/packageID"); !ok || v != 2 {
					t.Errorf("nri/packageID = %d (ok=%v), want 2", v, ok)
				}
				if v, ok := attrInt(dev, "nri/punitID"); !ok || v != 3 {
					t.Errorf("nri/punitID = %d (ok=%v), want 3", v, ok)
				}
				if dev.Name != "hp-pkg2-punit3" {
					t.Errorf("device name = %q, want hp-pkg2-punit3", dev.Name)
				}
				// Full shape check with HPCapacity=5.
				checkDeviceShape(t, dev, 5, "hp", "high", true)
			},
		},
		{
			// Class name that is all non-alphanumeric → sanitizeBase returns "class".
			name:      "all-special-char class name → 'class' fallback base",
			classes:   []*policyapi.CPUClass{hpClass("---")},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				if dev.Name != "class-pkg0-punit0" {
					t.Errorf("device name = %q, want class-pkg0-punit0", dev.Name)
				}
				if !isDNSLabel(dev.Name) {
					t.Errorf("device name %q is not a valid DNS label", dev.Name)
				}
			},
		},
		{
			// hpOnly=true: mixed HP/non-HP config → only HP devices emitted.
			// Non-HP DRA is deferred; non-HP classes must be silently filtered.
			name: "mixed HP/non-HP config with hpOnly=true → only HP devices emitted",
			classes: []*policyapi.CPUClass{
				hpClass("hp"),
				lpClass("lp"),
				nonPCTClass("default"),
			},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			hpOnly:    true,
			wantCount: 1,
			verify: func(t *testing.T, devices []resapi.Device) {
				t.Helper()
				dev := devices[0]
				if v, ok := attrStr(dev, "nri/cpuClass"); !ok {
					t.Error("nri/cpuClass attribute missing")
				} else if v != "hp" {
					t.Errorf("nri/cpuClass = %q, want \"hp\"", v)
				}
				if !isDNSLabel(dev.Name) {
					t.Errorf("device name %q is not a valid DNS label", dev.Name)
				}
			},
		},
		{
			// hpOnly=false: non-HP classes are included (base behaviour unchanged).
			name: "mixed HP/non-HP config with hpOnly=false → all published devices emitted",
			classes: []*policyapi.CPUClass{
				hpClass("hp"),
				lpClass("lp"),
			},
			punits:    []pct.PunitInfo{punit(0, 0, 4, 8)},
			isHP:      isHP,
			hpOnly:    false,
			wantCount: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isHPFn := tc.isHP
			if isHPFn == nil {
				isHPFn = func(string) bool { return false }
			}
			devices := buildDRADevices(tc.classes, tc.punits, isHPFn, tc.hpOnly)
			if len(devices) != tc.wantCount {
				t.Fatalf("buildDRADevices() returned %d devices, want %d; devices=%v",
					len(devices), tc.wantCount, deviceNames(devices))
			}
			// buildDRADevices must always return a non-nil slice (including the
			// empty case) so callers can safely range over it.
			if devices == nil {
				t.Errorf("buildDRADevices() returned nil, want non-nil []Device{}")
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

// sstWithPunit is a JSON seed for OVERRIDE_SST that provides one active
// SST-TF package (pkg=1) with one punit (punit=2), 8 CPUs, and
// GuaranteedHpCpus=3 (i.e. HPCapacity=3, NonHPCapacity=5).
// Using non-zero pkg and punit IDs exercises the full attribute path.
const sstWithPunit = `{"supported":true,"clos_count":4,"packages":[{"id":1,"cpus":"0-7","tf_supported":true,"tf_enabled":true,"cp_supported":true,"cp_enabled":false,"cp_priority":"ordered","punits":[{"id":2,"cpus":"0-7","max_hp_cpus":4,"guaranteed_hp_cpus":3}]}]}`

// TestDRADevices covers Handler.DRADevices: the nil-handler guard, the
// nil-pct guard, the inactive-pct early-return, and the delegation path
// to buildDRADevices when pct is active with classes set.
//
// Tests that require an active pct allocator use OVERRIDE_SST so that
// pct.Configure succeeds without real SST hardware.
func TestDRADevices(t *testing.T) {
	t.Run("nil handler returns empty non-nil", func(t *testing.T) {
		var h *Handler
		devs, err := h.DRADevices("test.driver")
		if err != nil {
			t.Fatalf("DRADevices on nil handler: %v", err)
		}
		if devs == nil {
			t.Error("nil handler: want non-nil empty slice, got nil")
		}
		if len(devs) != 0 {
			t.Errorf("nil handler: want 0 devices, got %d", len(devs))
		}
	})

	t.Run("nil pct returns empty non-nil", func(t *testing.T) {
		h := &Handler{} // pct is nil
		devs, err := h.DRADevices("test.driver")
		if err != nil {
			t.Fatalf("DRADevices with nil pct: %v", err)
		}
		if devs == nil {
			t.Error("nil pct: want non-nil empty slice, got nil")
		}
		if len(devs) != 0 {
			t.Errorf("nil pct: want 0 devices, got %d", len(devs))
		}
	})

	t.Run("inactive pct returns empty non-nil", func(t *testing.T) {
		// OVERRIDE_SST with supported=false → pct stays disabled after Configure.
		t.Setenv("OVERRIDE_SST", `{"supported":false,"packages":[]}`)
		pctA, err := pct.NewAllocator(nil)
		if err != nil {
			t.Fatalf("pct.NewAllocator: %v", err)
		}
		if err := pctA.Configure(nil, cpuset.New()); err != nil {
			t.Fatalf("pct.Configure: %v", err)
		}
		h := &Handler{pct: pctA}
		devs, err := h.DRADevices("test.driver")
		if err != nil {
			t.Fatalf("DRADevices inactive pct: %v", err)
		}
		if devs == nil {
			t.Error("inactive pct: want non-nil empty slice, got nil")
		}
		if len(devs) != 0 {
			t.Errorf("inactive pct: want 0 devices, got %d", len(devs))
		}
	})

	t.Run("active pct empty classes returns empty non-nil", func(t *testing.T) {
		// pct is active but h.classes is nil → buildDRADevices returns empty.
		t.Setenv("OVERRIDE_SST", sstWithPunit)
		t.Setenv("OVERRIDE_SST_STATE_DIR", t.TempDir())
		pctA, err := pct.NewAllocator(nil)
		if err != nil {
			t.Fatalf("pct.NewAllocator: %v", err)
		}
		classes := []*policyapi.CPUClass{{Name: "hp", PctPriority: "high"}}
		if err := pctA.Configure(classes, cpuset.New()); err != nil {
			t.Fatalf("pct.Configure: %v", err)
		}
		h := &Handler{pct: pctA, classes: nil} // classes not set
		devs, err := h.DRADevices("test.driver")
		if err != nil {
			t.Fatalf("DRADevices: %v", err)
		}
		if devs == nil {
			t.Error("want non-nil empty slice, got nil")
		}
		if len(devs) != 0 {
			t.Errorf("want 0 devices (no classes), got %d", len(devs))
		}
	})

	t.Run("active pct with classes delegates to buildDRADevices", func(t *testing.T) {
		// OVERRIDE_SST: pkg=1, punit=2, HPCapacity=3.
		// DRADevices must pass h.classes and h.pct.IsHPClass to buildDRADevices
		// and return the resulting device slice.
		t.Setenv("OVERRIDE_SST", sstWithPunit)
		t.Setenv("OVERRIDE_SST_STATE_DIR", t.TempDir())
		pctA, err := pct.NewAllocator(nil)
		if err != nil {
			t.Fatalf("pct.NewAllocator: %v", err)
		}
		classes := []*policyapi.CPUClass{{Name: "hp", PctPriority: "high"}}
		if err := pctA.Configure(classes, cpuset.New()); err != nil {
			t.Fatalf("pct.Configure: %v", err)
		}
		if !pctA.Active() {
			t.Fatal("pct not active after Configure with supported SST — check OVERRIDE_SST JSON")
		}
		// h.classes is set directly, mirroring what Handler.Configure does
		// (post fix: h.classes is assigned after all fallible ops).
		h := &Handler{pct: pctA, classes: classes}
		devs, err := h.DRADevices("test.driver")
		if err != nil {
			t.Fatalf("DRADevices: %v", err)
		}
		// pkg=1, punit=2, HPCapacity=3 → one device "hp-pkg1-punit2" with
		// capacity 3. NonHPCapacity=5 (8 CPUs − 3 HP) but "hp" is HP class.
		if len(devs) != 1 {
			t.Fatalf("DRADevices: got %d devices, want 1; names=%v", len(devs), deviceNames(devs))
		}
		dev := devs[0]
		if dev.Name != "hp-pkg1-punit2" {
			t.Errorf("device name = %q, want hp-pkg1-punit2", dev.Name)
		}
		// Verify topology attributes carry the non-zero IDs.
		if v, ok := attrInt(dev, "nri/packageID"); !ok || v != 1 {
			t.Errorf("nri/packageID = %d (ok=%v), want 1", v, ok)
		}
		if v, ok := attrInt(dev, "nri/punitID"); !ok || v != 2 {
			t.Errorf("nri/punitID = %d (ok=%v), want 2", v, ok)
		}
		// Full device-shape invariants (capacity=3, HP class).
		checkDeviceShape(t, dev, 3, "hp", "high", true)
	})
}
