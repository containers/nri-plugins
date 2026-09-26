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
	"encoding/json"
	"testing"
)

func TestCPUClassDRAPublish(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		publish bool
		invalid bool
	}{
		{"unset", `{"pctPriority": "high"}`, true, false},
		{"empty dra", `{"pctPriority": "high", "dra": {}}`, true, false},
		{"false", `{"pctPriority": "high", "dra": {"publish": false}}`, false, false},
		{"true", `{"pctPriority": "high", "dra": {"publish": true}}`, true, false},
		{"true, assoc-only", `{"sstClosID": 1, "dra": {"publish": true}}`, true, false},
		{"true, low priority", `{"pctPriority": "low", "dra": {"publish": true}}`, true, true},
		{"true, not PCT", `{"dra": {"publish": true}}`, true, true},
		{"false, not PCT", `{"dra": {"publish": false}}`, false, false},
		{"unset, not PCT", `{}`, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc := &CPUClass{}
			if err := json.Unmarshal([]byte(tc.json), cc); err != nil {
				t.Fatalf("failed to unmarshal %s: %v", tc.json, err)
			}
			cc.Name = "class"

			if got := cc.DRAPublish(); got != tc.publish {
				t.Errorf("DRAPublish() = %v, want %v", got, tc.publish)
			}
			err := cc.Validate()
			if tc.invalid && err == nil {
				t.Errorf("Validate() accepted %s", tc.json)
			}
			if !tc.invalid && err != nil {
				t.Errorf("Validate() rejected %s: %v", tc.json, err)
			}
		})
	}
}
