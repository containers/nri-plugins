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

// DRA-specific helpers for the cpuclass package:
// ValidateCPUClassesForDRA, buildDRADevices, and Handler.DRADevices.

package cpuclass

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kptr "k8s.io/utils/ptr"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass/internal/pct"
)

// DRA device attribute/capacity keys shared with pkg/resmgr/dra (which reads
// them back out of the published device list) and with the topology-aware
// policy's Reconfigure device-diff logic (cmd/plugins/topology-aware/policy/dra.go)
// — defined once here, referenced everywhere else, mirroring the DRADriverName
// convention.
const (
	// AttrCPUClass names the cpuClass a device belongs to.
	AttrCPUClass resapi.QualifiedName = "nri/cpuClass"
	// AttrPackageID names the CPU package a device's punit lives on.
	AttrPackageID resapi.QualifiedName = "nri/packageID"
	// AttrPunitID names the SST-TF punit a device represents.
	AttrPunitID resapi.QualifiedName = "nri/punitID"
	// CapacityCPUs is the capacity key for the number of CPUs a device grants.
	CapacityCPUs resapi.QualifiedName = "nri/cpus"
)

// nonAlphaRe matches runs of characters that are not lowercase letters or digits.
// Used by sanitizeBase to replace them with hyphens.
var nonAlphaRe = regexp.MustCompile(`[^a-z0-9]+`)

// maxDeviceBase is the maximum length for the sanitized class-name portion of a
// device name before any dedup suffix is appended.
//
// Budget: 63 (DNS label max) - 14 (worst-case punit suffix "-pkg99-punit99") -
// 3 (worst-case dedup suffix "-99") = 46.
const maxDeviceBase = 46

// ValidateCPUClassesForDRA checks that DRA-published PCT classes do not
// overcommit any priority tier.
//
// Tier classification is static and computed from config alone:
//   - Managed PCT (PctPriority != ""): tier = "pctPriority=<value>".
//   - Assoc-only PCT (SstClosID != nil): tier = "closID=<N>".
//   - Non-PCT classes: exempt — they have no turbo-frequency tier concept.
//
// If sharedCounters is false and any tier has more than one
// DRA-published class, an error is returned naming the tier, the
// conflicting class names (sorted), and the two resolutions.
//
// Called at driver Configure time, not at config load time. The []Punit
// parameter is absent in v1 — per-punit enforcement is deferred to the
// device-build step (Step 5) where runtime punit topology is available.
func ValidateCPUClassesForDRA(classes []*policyapi.CPUClass, sharedCounters bool) error {
	if sharedCounters {
		return nil
	}

	// Group published PCT classes by tier label.
	byTier := map[string][]string{} // tier label → sorted class names
	for _, cc := range classes {
		if !isPCTClass(cc) {
			continue
		}
		if !cc.DRAPublish() {
			continue
		}
		tier := tierLabel(cc)
		byTier[tier] = append(byTier[tier], cc.Name)
	}

	// Check for conflicts: any tier with more than one published class.
	tiers := make([]string, 0, len(byTier))
	for t := range byTier {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers) // deterministic outer ordering

	for _, tier := range tiers {
		names := byTier[tier]
		if len(names) <= 1 {
			continue
		}
		sort.Strings(names) // deterministic name listing in the error
		return fmt.Errorf(
			"DRA: tier %q has %d published cpuClasses (%v); "+
				"at most one is allowed without sharedCounters. "+
				"Resolutions: set cpuClass.dra.publish: false on all but one, "+
				"or enable spec.dra.sharedCounters: true (requires KEP-5941)",
			tier, len(names), names,
		)
	}

	return nil
}

// isPCTClass reports whether cc is a PCT class (managed or assoc-only).
func isPCTClass(cc *policyapi.CPUClass) bool {
	return cc.PctPriority != "" || cc.SstClosID != nil
}

// tierLabel returns the tier string used for grouping and error messages.
func tierLabel(cc *policyapi.CPUClass) string {
	if cc.PctPriority != "" {
		return "pctPriority=" + cc.PctPriority
	}
	return fmt.Sprintf("closID=%d", *cc.SstClosID)
}

// sanitizeBase lowercases s, replaces runs of non-alphanumeric characters with
// "-", trims leading/trailing hyphens, and truncates to maxLen (trimming any
// trailing hyphen created by truncation). Returns "class" if the result is empty.
func sanitizeBase(s string, maxLen int) string {
	b := strings.ToLower(s)
	b = nonAlphaRe.ReplaceAllString(b, "-")
	b = strings.Trim(b, "-")
	if b == "" {
		return "class"
	}
	if len(b) > maxLen {
		b = strings.TrimRight(b[:maxLen], "-")
	}
	if b == "" {
		return "class"
	}
	return b
}

// deviceName assembles a DRA device name from a pre-sanitized class base and
// punit topology identifiers. Format: <base>-pkg<pkgID>-punit<punitID>.
func deviceName(classBase string, pkgID, punitID int) string {
	return classBase + "-pkg" + strconv.Itoa(pkgID) + "-punit" + strconv.Itoa(punitID)
}

// intAttr returns a DeviceAttribute with an integer value.
func intAttr(v int64) resapi.DeviceAttribute {
	return resapi.DeviceAttribute{IntValue: kptr.To(v)}
}

// strAttr returns a DeviceAttribute with a string value.
func strAttr(v string) resapi.DeviceAttribute {
	return resapi.DeviceAttribute{StringValue: kptr.To(v)}
}

// buildDRADevices constructs the []resapi.Device slice (Model B: one device per
// published cpuClass × SST-TF punit) to be passed to kubeletplugin.PublishResources.
//
// For each published class, for each punit: emits one device if capacity > 0.
// HP classes use HPCapacity; non-HP classes use NonHPCapacity.
//
// When hpOnly is true, only HP classes (isHP returns true) are emitted.
// Non-HP DRA is deferred because PunitInfo carries no per-punit CPU list;
// see plan.md Step 7.
func buildDRADevices(
	classes []*policyapi.CPUClass,
	punits []pct.PunitInfo,
	isHP func(className string) bool,
	hpOnly bool,
) []resapi.Device {
	if len(classes) == 0 || len(punits) == 0 {
		return []resapi.Device{}
	}

	// Pre-compute a stable sanitized base for each published class name.
	// Dedup: if two different class names produce the same base, the second
	// gets a "-N" suffix (N starting at 2). The same class name across multiple
	// punits always reuses the same pre-computed base (no counter increment).
	takenBases := map[string]struct{}{} // bases already claimed by some class
	baseForClass := map[string]string{} // className -> final sanitized base

	for _, cc := range classes {
		if !cc.DRAPublish() {
			continue
		}
		// non-HP DRA deferred — PunitInfo has no per-punit CPU list; see plan.md Step 7
		if hpOnly && !isHP(cc.Name) {
			continue
		}
		if _, done := baseForClass[cc.Name]; done {
			continue // same class name seen twice — skip (defensive)
		}
		candidate := sanitizeBase(cc.Name, maxDeviceBase)
		if _, taken := takenBases[candidate]; !taken {
			takenBases[candidate] = struct{}{}
			baseForClass[cc.Name] = candidate
		} else {
			// Collision: find the next available suffixed base.
			for n := 2; ; n++ {
				suffixed := candidate + "-" + strconv.Itoa(n)
				if _, inUse := takenBases[suffixed]; !inUse {
					takenBases[suffixed] = struct{}{}
					baseForClass[cc.Name] = suffixed
					break
				}
			}
		}
	}

	var devices []resapi.Device

	for _, cc := range classes {
		if !cc.DRAPublish() {
			continue
		}
		if hpOnly && !isHP(cc.Name) {
			continue
		}
		base := baseForClass[cc.Name]
		// baseForClass always has an entry for published classes at this point:
		// the first loop processes every published class name exactly once, and
		// duplicate class names in the input are skipped defensively there.
		// If upstream validation passes, duplicate names cannot reach here.
		for _, pu := range punits {
			// Select capacity based on HP classification.
			var capacity int
			if isHP(cc.Name) {
				capacity = pu.HPCapacity
			} else {
				capacity = pu.NonHPCapacity
			}
			if capacity == 0 {
				continue // zero-capacity RequestPolicy is invalid; skip
			}

			name := deviceName(base, pu.PkgID, pu.PunitID)

			attrs := map[resapi.QualifiedName]resapi.DeviceAttribute{
				AttrPackageID: intAttr(int64(pu.PkgID)),
				AttrPunitID:   intAttr(int64(pu.PunitID)),
				AttrCPUClass:  strAttr(cc.Name),
			}
			// nri/pctPriority is only emitted for PCT classes (non-empty PctPriority).
			// Omitting it for non-PCT classes avoids CEL false-positives on "" values.
			if cc.PctPriority != "" {
				attrs["nri/pctPriority"] = strAttr(cc.PctPriority)
			}

			capStr := strconv.Itoa(capacity)
			dev := resapi.Device{
				Name:       name,
				Attributes: attrs,
				Capacity: map[resapi.QualifiedName]resapi.DeviceCapacity{
					CapacityCPUs: {
						Value: resource.MustParse(capStr),
						RequestPolicy: &resapi.CapacityRequestPolicy{
							Default: kptr.To(resource.MustParse("1")),
							ValidRange: &resapi.CapacityRequestPolicyRange{
								Min:  kptr.To(resource.MustParse("1")),
								Max:  kptr.To(resource.MustParse(capStr)),
								Step: kptr.To(resource.MustParse("1")),
							},
						},
					},
				},
				AllowMultipleAllocations: kptr.To(true),
				NodeAllocatableResources: map[corev1.ResourceName]resapi.NodeAllocatableResource{
					corev1.ResourceCPU: {
						Mapping: &resapi.NodeAllocatableMapping{
							CapacityKey:        kptr.To(CapacityCPUs),
							CapacityMultiplier: kptr.To(resource.MustParse("1")),
						},
					},
				},
			}
			devices = append(devices, dev)
		}
	}

	if devices == nil {
		return []resapi.Device{}
	}
	return devices
}

// DRADevices returns the DRA device slice for the current cpuClass configuration.
// Returns an empty (non-nil) slice when the handler is nil, PCT is inactive,
// or no punits are available.
// Always returns nil error in v1; error handling will be added in Step 6.
//
// Must be called on the resmgr goroutine or under the resmgr lock — same as all
// other Handler methods.
func (h *Handler) DRADevices(_ string) ([]resapi.Device, error) {
	if h == nil || h.pct == nil {
		return []resapi.Device{}, nil
	}
	// Punits() returns nil when inactive, so len()==0 covers both
	// the inactive and the "no punits" cases.
	punits := h.pct.Punits()
	if len(punits) == 0 {
		return []resapi.Device{}, nil
	}
	return buildDRADevices(h.classes, punits, h.pct.IsHPClass, true), nil
}
