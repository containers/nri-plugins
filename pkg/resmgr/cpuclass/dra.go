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
	"sort"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
)

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
			"DRA: sharedCounters is not yet supported (Model C / KEP-5941 is not " +
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
