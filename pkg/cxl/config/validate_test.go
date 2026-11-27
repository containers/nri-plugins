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

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

func TestValidateSteering(t *testing.T) {
	const p = "MemoryConfig: container.steering."
	tu := func(memoryType, usage string) TargetUsage { return TargetUsage{memoryType, usage} }
	for _, tc := range []struct {
		name string
		s    Steering
		// err of a resolved result and of an effective steering
		err, effErr string
	}{
		{name: "none", s: Steering{Policy: PolicyNone}},
		{name: "auto", s: Steering{Policy: PolicyAuto, MinStep: "64Mi", MaxStep: "1Gi"}},
		{name: "first-dram", s: Steering{Policy: PolicyFirstDRAM, MaxStep: "1G"}},
		{name: "first-cxl", s: Steering{Policy: PolicyFirstCXL}},
		{name: "start-interleaved", s: Steering{Policy: PolicyStartInterleaved, MinStep: "1Gi", MaxStep: "1Gi"}},
		{name: "end-interleaved", s: Steering{Policy: PolicyEndInterleaved}},
		{name: "waypoints", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{
			{TargetUsages: []TargetUsage{tu("DRAM", "4Gi")}},
			{TargetUsages: []TargetUsage{tu("CXL", "40Gi"), tu("DRAM", "8Gi")}},
			{TargetUsages: []TargetUsage{tu("CXL", "40Gi")}},
		}}},
		{name: "zero usage", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("CXL", "0")}}}}},
		{name: "no policy", s: Steering{MaxStep: "1Gi"},
			effErr: p + "maxStep needs a policy other than none"},
		{name: "no policy, waypoints", s: Steering{Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("DRAM", "1Gi")}}}},
			effErr: p + "waypoints needs a policy other than none"},
		{name: "policy waypoints without waypoints", s: Steering{Policy: PolicyWaypoints},
			effErr: p + "policy waypoints needs waypoints"},
		{name: "invalid policy", s: Steering{Policy: "off"},
			err: p + `policy: invalid policy "off" (none, auto, first-dram, first-cxl, start-interleaved, end-interleaved or waypoints)`},
		{name: "none with waypoints", s: Steering{Policy: PolicyNone, Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("DRAM", "1Gi")}}}},
			err: p + "waypoints needs a policy other than none"},
		{name: "none with minStep", s: Steering{Policy: PolicyNone, MinStep: "1Gi"},
			err: p + "minStep needs a policy other than none"},
		{name: "none with maxStep", s: Steering{Policy: PolicyNone, MaxStep: "1Gi"},
			err: p + "maxStep needs a policy other than none"},
		{name: "waypoints of another policy", s: Steering{Policy: PolicyAuto, Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("DRAM", "1Gi")}}}},
			err: p + "waypoints needs policy waypoints"},
		{name: "minStep exceeds maxStep", s: Steering{Policy: PolicyAuto, MinStep: "2Gi", MaxStep: "1Gi"},
			err: p + "minStep 2Gi exceeds maxStep 1Gi"},
		{name: "invalid minStep", s: Steering{Policy: PolicyAuto, MinStep: "1 Gi"},
			err: p + `minStep: invalid quantity "1 Gi"`},
		{name: "negative maxStep", s: Steering{Policy: PolicyAuto, MaxStep: "-1Gi"},
			err: p + `maxStep: invalid quantity "-1Gi" (must be positive)`},
		{name: "empty waypoint", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{{}}},
			err: p + "waypoints[0].targetUsages: empty"},
		{name: "memory type is case sensitive", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("dram", "1Gi")}}}},
			err: p + `waypoints[0].targetUsages[0].memoryType: invalid memory type "dram" (DRAM or CXL)`},
		{name: "memory type twice", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("DRAM", "1Gi"), tu("DRAM", "2Gi")}}}},
			err: p + "waypoints[0].targetUsages[1].memoryType: DRAM twice in the waypoint"},
		{name: "missing usage", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("CXL", "")}}}},
			err: p + "waypoints[0].targetUsages[0].usage: missing"},
		{name: "invalid usage", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("CXL", "much")}}}},
			err: p + `waypoints[0].targetUsages[0].usage: invalid quantity "much"`},
		{name: "negative usage", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{{TargetUsages: []TargetUsage{tu("CXL", "-1")}}}},
			err: p + `waypoints[0].targetUsages[0].usage: invalid quantity "-1" (must not be negative)`},
		{name: "decreasing usage", s: Steering{Policy: PolicyWaypoints, Waypoints: []Waypoint{
			{TargetUsages: []TargetUsage{tu("DRAM", "8Gi")}},
			{TargetUsages: []TargetUsage{tu("CXL", "8Gi")}},
			{TargetUsages: []TargetUsage{tu("DRAM", "4Gi")}},
		}}, err: p + "waypoints[2]: DRAM usage 4Gi is less than 8Gi of waypoint 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			if err := validateSteering(errPrefix, &s, false, nil); !checkErr(err, tc.err) {
				t.Errorf("resolved: got  %v\nwant %s", err, tc.err)
			}
			effErr := tc.effErr
			if effErr == "" {
				effErr = tc.err
			}
			if err := validateSteering(errPrefix, &s, true, nil); !checkErr(err, effErr) {
				t.Errorf("effective: got  %v\nwant %s", err, effErr)
			}
		})
	}
}

func TestJoinList(t *testing.T) {
	for want, values := range map[string][]string{
		"":             nil,
		"a":            {"a"},
		"a or b":       {"a", "b"},
		"a, b or c":    {"a", "b", "c"},
		"a, b, c or d": {"a", "b", "c", "d"},
	} {
		if got := joinList(values, "or"); got != want {
			t.Errorf("%v: %q", values, got)
		}
	}
}

func TestCompact(t *testing.T) {
	var nilConfig *MemoryConfig
	for _, tc := range []struct {
		c    *MemoryConfig
		want string
	}{
		{nilConfig, `{}`},
		{&MemoryConfig{APIVersion: APIVersion, Kind: Kind}, `{}`},
		{&MemoryConfig{APIVersion: APIVersion, Kind: Kind, Device: &Device{Mode: api.ModeDax}}, `{"device":{"mode":"dax"}}`},
		{&MemoryConfig{Pool: &Pool{Opaque: map[string]json.RawMessage{"k": json.RawMessage(`{ "a": 1 }`)}}}, `{"pool":{"opaque":{"k":{"a":1}}}}`},
	} {
		if got := tc.c.Compact(); got != tc.want {
			t.Errorf("%+v: %s, expected %s", tc.c, got, tc.want)
		}
	}
}
