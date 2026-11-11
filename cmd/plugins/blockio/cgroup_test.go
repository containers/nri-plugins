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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	oci "github.com/opencontainers/runtime-spec/specs-go"
)

func TestWeightTranslation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parameters oci.LinuxBlockIO
		support    weightSupport
		want       map[string]string
	}{
		{name: "empty", want: map[string]string{}},
		{
			name: "minimum without BFQ", parameters: oci.LinuxBlockIO{Weight: uint16ptr(10)},
			want: map[string]string{"io.weight": "1"},
		},
		{
			name: "default without BFQ", parameters: oci.LinuxBlockIO{Weight: uint16ptr(100)},
			want: map[string]string{"io.weight": "910"},
		},
		{
			name: "maximum without BFQ", parameters: oci.LinuxBlockIO{Weight: uint16ptr(1000)},
			want: map[string]string{"io.weight": "10000"},
		},
		{
			name: "zero default is unset", parameters: oci.LinuxBlockIO{Weight: uint16ptr(0)},
			want: map[string]string{},
		},
		{
			name: "BFQ default", parameters: oci.LinuxBlockIO{Weight: uint16ptr(400)},
			support: weightSupport{bfq: true}, want: map[string]string{"io.bfq.weight": "400"},
		},
		{
			name: "BFQ default and device weights",
			parameters: oci.LinuxBlockIO{Weight: uint16ptr(400), WeightDevice: []oci.LinuxWeightDevice{
				{LinuxBlockIODevice: device(259, 1), Weight: uint16ptr(200)},
				{LinuxBlockIODevice: device(8, 32), Weight: uint16ptr(100)},
				{LinuxBlockIODevice: device(8, 0), Weight: uint16ptr(50)},
			}},
			support: weightSupport{bfq: true, deviceWeights: true},
			want:    map[string]string{"io.bfq.weight": "400\n8:0 50\n8:32 100\n259:1 200"},
		},
		{
			name: "BFQ device weight without default",
			parameters: oci.LinuxBlockIO{WeightDevice: []oci.LinuxWeightDevice{
				{LinuxBlockIODevice: device(8, 0), Weight: uint16ptr(100)},
			}},
			support: weightSupport{bfq: true, deviceWeights: true},
			want:    map[string]string{"io.bfq.weight": "8:0 100"},
		},
		{
			name: "old BFQ omits device weights",
			parameters: oci.LinuxBlockIO{Weight: uint16ptr(400), WeightDevice: []oci.LinuxWeightDevice{
				{LinuxBlockIODevice: device(8, 0), Weight: uint16ptr(100)},
			}},
			support: weightSupport{bfq: true}, want: map[string]string{"io.bfq.weight": "400"},
		},
		{
			name: "no BFQ omits device weights",
			parameters: oci.LinuxBlockIO{Weight: uint16ptr(10), WeightDevice: []oci.LinuxWeightDevice{
				{LinuxBlockIODevice: device(8, 0), Weight: uint16ptr(100)},
			}},
			want: map[string]string{"io.weight": "1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := linuxBlockIOToUnified(&tc.parameters, tc.support)
			if !reflect.DeepEqual(got, tc.want) {
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
		"io.weight": "910",
		"io.max": "8:0 rbps=50000000 wbps=10485760 riops=10000 wiops=max\n" +
			"8:32 rbps=max wiops=5000\n259:1 rbps=100000000 riops=max",
	}
	if got := linuxBlockIOToUnified(parameters, weightSupport{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Unified = %v, want %v", got, want)
	}
}

func TestDetectWeightSupport(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		data string
		want weightSupport
	}{
		{name: "no BFQ"},
		{
			name: "delegated mount", path: "io.bfq.weight", data: "default 100\n",
			want: weightSupport{bfq: true, deviceWeights: true},
		},
		{
			name: "real root has no BFQ file", path: "system.slice/io.bfq.weight", data: "default 100\n8:0 200\n",
			want: weightSupport{bfq: true, deviceWeights: true},
		},
		{
			name: "old BFQ", path: "system.slice/io.bfq.weight", data: "100\n",
			want: weightSupport{bfq: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.path != "" {
				path := filepath.Join(root, tc.path)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				writeConfig(t, path, tc.data)
			}
			got, err := detectWeightSupport(root)
			if err != nil || got != tc.want {
				t.Fatalf("support = %+v, error = %v; want %+v", got, err, tc.want)
			}
		})
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "io.bfq.weight"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := detectWeightSupport(root); err == nil {
		t.Fatal("expected BFQ read error")
	}
	if _, err := detectWeightSupport(filepath.Join(root, "missing")); err == nil {
		t.Fatal("expected missing cgroup mount error")
	}
}

func TestRejectNonCgroupV2(t *testing.T) {
	if err := checkCgroupV2(t.TempDir()); err == nil {
		t.Fatal("expected regular filesystem to be rejected")
	}
}

func uint16ptr(value uint16) *uint16 { return &value }

func device(major, minor int64) oci.LinuxBlockIODevice {
	return oci.LinuxBlockIODevice{Major: major, Minor: minor}
}
