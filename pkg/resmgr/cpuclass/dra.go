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

// nonAlphaRe matches runs of characters that are not lowercase letters or digits.
// Used by sanitizeBase to replace them with hyphens.
var nonAlphaRe = regexp.MustCompile(`[^a-z0-9]+`)

// maxDNSLabel is the Kubernetes DNS label name length limit.
const maxDNSLabel = 63

// maxDeviceBase returns the maximum length for the sanitized class-name
// portion of a device name before any dedup suffix is appended.
func maxDeviceBase(punits []pct.PunitInfo, classCount int) int {
	maxSuffixLen := len("-pkg0-punit0")
	for _, pu := range punits {
		if suffixLen := len("-pkg" + strconv.Itoa(pu.PkgID) + "-punit" + strconv.Itoa(pu.PunitID)); suffixLen > maxSuffixLen {
			maxSuffixLen = suffixLen
		}
	}
	maxDedupSuffixLen := len("-" + strconv.Itoa(classCount+1))
	return maxDNSLabel - maxSuffixLen - maxDedupSuffixLen
}

// ValidateCPUClassesForDRA checks that DRA-published PCT classes do not
// overcommit any priority tier. Classes are grouped by tier — the
// pctPriority value for managed PCT classes, or the SstClosID for
// assoc-only classes — and more than one DRA-published class in the same
// tier is an error. Non-PCT classes and managed LP classes (pctPriority
// != "high") are exempt, since buildDRADevices with hpOnly=true never
// publishes them.
//
// sharedCounters is rejected: buildDRADevices always publishes
// independent full-capacity devices per class, so accepting it here would
// silently disable this overcommit guard without implementing the
// KEP-5941 shared-counter model.
//
// Called at driver Configure time, not at config load time.
func ValidateCPUClassesForDRA(classes []*policyapi.CPUClass, sharedCounters bool) error {
	if sharedCounters {
		return fmt.Errorf(
			"DRA: sharedCounters is not yet supported (KEP-5941 is not " +
				"implemented); leave spec.dra.sharedCounters unset or false",
		)
	}

	// Group DRA-published PCT classes by tier label.
	byTier := map[string][]string{} // tier label → sorted class names
	for _, cc := range classes {
		if !isPCTClass(cc) {
			continue
		}
		if !cc.DRAPublish() {
			continue
		}
		if cc.PctPriority != "" && cc.PctPriority != "high" {
			continue // LP classes are exempt, see doc comment
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
				"at most one is allowed. "+
				"Resolution: set cpuClass.dra.publish: false on all but one "+
				"(sharedCounters is not yet supported — see KEP-5941)",
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

// buildDRADevices constructs the []resapi.Device slice (one device per
// published cpuClass × SST-TF punit) to be passed to kubeletplugin.PublishResources.
//
// For each published class, for each punit: emits one device if capacity > 0.
// HP classes use HPCapacity; non-HP classes use NonHPCapacity. hpOnly only
// affects the device-name length budget for now.
func buildDRADevices(
	driverName string,
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
	publishedClassCount := 0
	for _, cc := range classes {
		if cc.DRAPublish() && (!hpOnly || isHP(cc.Name)) {
			publishedClassCount++
		}
	}
	baseMaxLen := maxDeviceBase(punits, publishedClassCount)

	for _, cc := range classes {
		if !cc.DRAPublish() {
			continue
		}
		if _, done := baseForClass[cc.Name]; done {
			continue // same class name seen twice — skip (defensive)
		}
		candidate := sanitizeBase(cc.Name, baseMaxLen)
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
		base := baseForClass[cc.Name]
		// Always set: duplicate class names are skipped above, and
		// validation guarantees no duplicates reach this point.
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
				"nri/packageID": intAttr(int64(pu.PkgID)),
				"nri/punitID":   intAttr(int64(pu.PunitID)),
				"nri/cpuClass":  strAttr(cc.Name),
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
					"nri/cpus": {
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
							CapacityKey:        kptr.To(resapi.QualifiedName("nri/cpus")),
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
//
// Must be called on the resmgr goroutine or under the resmgr lock — same as all
// other Handler methods.
func (h *Handler) DRADevices(driverName string) ([]resapi.Device, error) {
	if h == nil || h.pct == nil {
		return []resapi.Device{}, nil
	}
	// Punits() returns nil when inactive, so len()==0 covers both
	// the inactive and the "no punits" cases.
	punits := h.pct.Punits()
	if len(punits) == 0 {
		return []resapi.Device{}, nil
	}
	return buildDRADevices(driverName, h.classes, punits, h.pct.IsHPClass, true), nil
}
