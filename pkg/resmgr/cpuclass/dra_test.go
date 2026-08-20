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
	"strings"
	"testing"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
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

