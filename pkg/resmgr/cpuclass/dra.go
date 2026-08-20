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

// Package cpuclass — DRA-specific helpers.
// This file is created in Step 3 and extended in Step 5 with
// Manager.DRADevices() and related methods.

package cpuclass

import (
	"fmt"
	"sort"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
)

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
