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

package main

import (
	"reflect"
	"testing"

	oci "github.com/opencontainers/runtime-spec/specs-go"
)

func TestWeightTranslation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parameters oci.LinuxBlockIO
		want       map[string]string
	}{
		{name: "empty", want: map[string]string{}},
		{
			name: "minimum", parameters: oci.LinuxBlockIO{Weight: uint16ptr(10)},
			want: map[string]string{"io.bfq.weight": "10"},
		},
		{
			name: "default", parameters: oci.LinuxBlockIO{Weight: uint16ptr(100)},
			want: map[string]string{"io.bfq.weight": "100"},
		},
		{
			name: "maximum", parameters: oci.LinuxBlockIO{Weight: uint16ptr(1000)},
			want: map[string]string{"io.bfq.weight": "1000"},
		},
		{
			name: "zero default is unset", parameters: oci.LinuxBlockIO{Weight: uint16ptr(0)},
			want: map[string]string{},
		},
		{
			name: "default and device weights",
			parameters: oci.LinuxBlockIO{Weight: uint16ptr(400), WeightDevice: []oci.LinuxWeightDevice{
				{LinuxBlockIODevice: device(259, 1), Weight: uint16ptr(200)},
				{LinuxBlockIODevice: device(8, 32), Weight: uint16ptr(100)},
				{LinuxBlockIODevice: device(8, 0), Weight: uint16ptr(50)},
			}},
			want: map[string]string{"io.bfq.weight": "400\n259:1 200\n8:32 100\n8:0 50"},
		},
		{
			name: "device weight without default",
			parameters: oci.LinuxBlockIO{WeightDevice: []oci.LinuxWeightDevice{
				{LinuxBlockIODevice: device(8, 0), Weight: uint16ptr(100)},
			}},
			want: map[string]string{"io.bfq.weight": "8:0 100"},
		},
		{
			name: "unset device weight is skipped",
			parameters: oci.LinuxBlockIO{WeightDevice: []oci.LinuxWeightDevice{
				{LinuxBlockIODevice: device(8, 0)},
			}},
			want: map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linuxBlockIOToUnified(&tc.parameters); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Unified = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestThrottleTranslation(t *testing.T) {
	parameters := &oci.LinuxBlockIO{
		Weight: uint16ptr(100),
		ThrottleReadBpsDevice: []oci.LinuxThrottleDevice{
			{LinuxBlockIODevice: device(259, 1), Rate: 100000000},
			{LinuxBlockIODevice: device(8, 32), Rate: 0},
			{LinuxBlockIODevice: device(8, 0), Rate: 50000000},
		},
		ThrottleWriteBpsDevice: []oci.LinuxThrottleDevice{
			{LinuxBlockIODevice: device(8, 0), Rate: 10485760},
		},
		ThrottleReadIOPSDevice: []oci.LinuxThrottleDevice{
			{LinuxBlockIODevice: device(8, 0), Rate: 10000},
			{LinuxBlockIODevice: device(259, 1), Rate: 0},
		},
		ThrottleWriteIOPSDevice: []oci.LinuxThrottleDevice{
			{LinuxBlockIODevice: device(8, 32), Rate: 5000},
			{LinuxBlockIODevice: device(8, 0), Rate: 0},
		},
	}
	want := map[string]string{
		"io.bfq.weight": "100",
		"io.max": "8:0 rbps=50000000 wbps=10485760 riops=10000 wiops=max\n" +
			"8:32 rbps=max wiops=5000\n259:1 rbps=100000000 riops=max",
	}
	if got := linuxBlockIOToUnified(parameters); !reflect.DeepEqual(got, want) {
		t.Fatalf("Unified = %v, want %v", got, want)
	}
}

func uint16ptr(value uint16) *uint16 { return &value }

func device(major, minor int64) oci.LinuxBlockIODevice {
	return oci.LinuxBlockIODevice{Major: major, Minor: minor}
}
