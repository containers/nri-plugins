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

package config

import (
	"encoding/json"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string // the container settings as JSON
		err  string
	}{
		{name: "empty", data: `{}`, want: `{}`},
		{name: "empty container", data: `{"container": {}}`, want: `{}`},
		{name: "empty steering", data: `{"container": {"steering": {}}}`, want: `{}`},
		{name: "opt in", data: `{"container": {"steering": {"policy": "auto"}}}`, want: `{"steering":{"policy":"auto"}}`},
		{
			name: "every field",
			data: `{"container": {"steering": {"policy": "waypoints", "minStep": "64Mi", "maxStep": "1Gi",` +
				` "waypoints": [{"targetUsages": [{"memoryType": "DRAM", "usage": "4Gi"}]}]}, "fencing": "none"}}`,
			want: `{"steering":{"policy":"waypoints","waypoints":[{"targetUsages":[{"memoryType":"DRAM","usage":"4Gi"}]}],"minStep":"64Mi","maxStep":"1Gi"},"fencing":"none"}`,
		},
		{name: "fencing", data: `{"container": {"fencing": "allowed-mems"}}`, want: `{"fencing":"allowed-mems"}`},
		{name: "steering none", data: `{"container": {"steering": {"policy": "none"}}}`, want: `{"steering":{"policy":"none"}}`},
		{name: "device is not a default", data: `{"device": {"mode": "dax"}}`, err: `defaults: json: unknown field "device"`},
		{name: "pool is not a default", data: `{"pool": {}}`, err: `defaults: json: unknown field "pool"`},
		{name: "unknown container field", data: `{"container": {"memoryLimit": {}}}`, err: `defaults: json: unknown field "container.memoryLimit"`},
		{name: "case sensitive", data: `{"container": {"Fencing": "none"}}`, err: `defaults: json: unknown field "container.Fencing"`},
		{name: "duplicate field", data: `{"container": {"fencing": "none", "fencing": "none"}}`, err: `defaults: json: duplicate field "container.fencing"`},
		{name: "invalid fencing", data: `{"container": {"fencing": "hard"}}`, err: `defaults.container.fencing: invalid value "hard" (allowed-mems or none)`},
		{
			name: "invalid policy",
			data: `{"container": {"steering": {"policy": "off"}}}`,
			err:  `defaults.container.steering.policy: invalid policy "off" (none, auto, first-dram, first-cxl, start-interleaved, end-interleaved or waypoints)`,
		},
		{name: "steps without a policy", data: `{"container": {"steering": {"maxStep": "1Gi"}}}`, err: `defaults.container.steering.maxStep needs a policy other than none`},
		{name: "steps with none", data: `{"container": {"steering": {"policy": "none", "minStep": "1Gi"}}}`, err: `defaults.container.steering.minStep needs a policy other than none`},
		{name: "waypoints without waypoints", data: `{"container": {"steering": {"policy": "waypoints"}}}`, err: `defaults.container.steering.policy waypoints needs waypoints`},
		{name: "invalid step", data: `{"container": {"steering": {"policy": "auto", "maxStep": "big"}}}`, err: `defaults.container.steering.maxStep: invalid quantity "big"`},
		{name: "not JSON", data: `container: [`, err: `defaults: invalid character 'c' looking for beginning of value`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDefaults([]byte(tc.data))
			if !checkErr(err, tc.err) {
				t.Fatalf("got  %v\nwant %s", err, tc.err)
			}
			if tc.err != "" {
				return
			}
			data, _ := json.Marshal(d.ContainerSettings())
			if string(data) != tc.want {
				t.Errorf("got\n%s\nexpected\n%s", data, tc.want)
			}
		})
	}
}

func TestDefaultsValidate(t *testing.T) {
	var nilDefaults *Defaults
	if err := nilDefaults.Validate(); err != nil {
		t.Errorf("nil: %v", err)
	}
	if c := nilDefaults.ContainerSettings(); c.Steering != nil || c.Fencing != "" {
		t.Errorf("nil: %+v", c)
	}
	d := &Defaults{Container: &Container{Fencing: "mempolicy"}}
	if err := d.Validate(); !checkErr(err, `defaults.container.fencing: invalid value "mempolicy" (allowed-mems or none)`) {
		t.Errorf("mempolicy: %v", err)
	}
	d = &Defaults{Container: &Container{Steering: &Steering{Policy: PolicyAuto}, Fencing: FencingNone}}
	if err := d.Validate(); err != nil {
		t.Errorf("valid: %v", err)
	}
	eff, err := Combine(nil, d.ContainerSettings())
	if err != nil {
		t.Fatal(err)
	}
	if eff.PolicyText() != "auto (node default)" || eff.FencingText() != "none (node default)" || !eff.Steers() {
		t.Errorf("effective %+v %v", eff.Container, eff.Origins)
	}
}
