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
	"testing"
)

// ptr returns a pointer to v; helper for bool/int literals in tests.
func ptr[T any](v T) *T { return &v }

func TestCPUClassDRAPublish(t *testing.T) {
	tests := []struct {
		name string
		cc   *CPUClass
		want bool
	}{
		{
			name: "nil DRA field defaults to true",
			cc:   &CPUClass{},
			want: true,
		},
		{
			name: "DRA set but Publish nil defaults to true",
			cc:   &CPUClass{DRA: &CPUClassDRA{}},
			want: true,
		},
		{
			name: "explicit false",
			cc:   &CPUClass{DRA: &CPUClassDRA{Publish: ptr(false)}},
			want: false,
		},
		{
			name: "explicit true",
			cc:   &CPUClass{DRA: &CPUClassDRA{Publish: ptr(true)}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cc.DRAPublish()
			if got != tc.want {
				t.Errorf("DRAPublish() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCPUClassDRADeepCopy(t *testing.T) {
	orig := &CPUClass{
		Name: "hp",
		DRA:  &CPUClassDRA{Publish: ptr(false)},
	}
	copy := orig.DeepCopy()
	if copy == orig {
		t.Fatal("DeepCopy returned same pointer")
	}
	if copy.DRA == orig.DRA {
		t.Fatal("DRA pointer not deep-copied")
	}
	if copy.DRA.Publish == orig.DRA.Publish {
		t.Fatal("DRA.Publish pointer not deep-copied (shared storage)")
	}

	// Mutating the copy must not affect the original.
	*copy.DRA.Publish = true
	if *orig.DRA.Publish != false {
		t.Error("mutating copy's DRA.Publish affected the original")
	}
}
