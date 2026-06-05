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

func TestFrequencyResolve(t *testing.T) {
	const (
		minHz   uint = 800000
		baseHz  uint = 2400000
		turboHz uint = 3800000
	)
	cases := []struct {
		name string
		f    Frequency
		want uint
	}{
		{"min sentinel", FrequencyMin, minHz},
		{"base sentinel", FrequencyBase, baseHz},
		{"turbo sentinel", FrequencyTurbo, turboHz},
		{"concrete value passed through", Frequency(1500000), 1500000},
		{"zero stays zero", Frequency(0), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.Resolve(minHz, baseHz, turboHz)
			if got != tc.want {
				t.Errorf("Resolve = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFrequencyIsSymbolic(t *testing.T) {
	if !FrequencyMin.IsSymbolic() {
		t.Errorf("FrequencyMin.IsSymbolic = false, want true")
	}
	if !FrequencyBase.IsSymbolic() {
		t.Errorf("FrequencyBase.IsSymbolic = false, want true")
	}
	if !FrequencyTurbo.IsSymbolic() {
		t.Errorf("FrequencyTurbo.IsSymbolic = false, want true")
	}
	if Frequency(3000000).IsSymbolic() {
		t.Errorf("concrete frequency must not be IsSymbolic")
	}
	if Frequency(0).IsSymbolic() {
		t.Errorf("zero must not be IsSymbolic")
	}
}

func TestFrequencyUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    Frequency
		wantErr bool
	}{
		{"symbolic min", `"min"`, FrequencyMin, false},
		{"symbolic base", `"base"`, FrequencyBase, false},
		{"symbolic turbo", `"turbo"`, FrequencyTurbo, false},
		{"symbolic uppercase", `"TURBO"`, FrequencyTurbo, false},
		{"GHz fractional", `"3.2GHz"`, Frequency(3200000), false},
		{"GHz short", `"2G"`, Frequency(2000000), false},
		{"MHz", `"2900MHz"`, Frequency(2900000), false},
		{"MHz short", `"2900M"`, Frequency(2900000), false},
		{"kHz explicit", `"2900000kHz"`, Frequency(2900000), false},
		{"kHz short", `"2900000k"`, Frequency(2900000), false},
		{"bare number as kHz", `"2900000"`, Frequency(2900000), false},
		{"json number as kHz", `2900000`, Frequency(2900000), false},
		{"empty string", `""`, Frequency(0), false},
		{"invalid unit", `"3GBz"`, 0, true},
		{"negative number", `-1000`, 0, true},
		{"garbage", `"abc"`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f Frequency
			err := json.Unmarshal([]byte(tc.input), &f)
			if tc.wantErr {
				if err == nil {
					t.Errorf("Unmarshal(%s) = nil err, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) unexpected err: %v", tc.input, err)
			}
			if f != tc.want {
				t.Errorf("Unmarshal(%s) = %d, want %d", tc.input, uint(f), uint(tc.want))
			}
		})
	}
}

func TestFrequencyMarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		f    Frequency
		want string
	}{
		{"min", FrequencyMin, `"min"`},
		{"base", FrequencyBase, `"base"`},
		{"turbo", FrequencyTurbo, `"turbo"`},
		{"concrete", Frequency(2900000), `2900000`},
		{"zero", Frequency(0), `0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.f)
			if err != nil {
				t.Fatalf("Marshal err: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("Marshal = %s, want %s", string(b), tc.want)
			}
		})
	}
}

func TestFrequencyRoundTrip(t *testing.T) {
	cases := []Frequency{
		FrequencyMin, FrequencyBase, FrequencyTurbo,
		Frequency(0), Frequency(2900000), Frequency(3800000),
	}
	for _, f := range cases {
		t.Run(f.String(), func(t *testing.T) {
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatalf("Marshal err: %v", err)
			}
			var got Frequency
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("Unmarshal err: %v", err)
			}
			if got != f {
				t.Errorf("round-trip: got %d, want %d", uint(got), uint(f))
			}
		})
	}
}
