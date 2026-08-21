// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package topology

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"testing"
)

type logger int

func (logger) Debugf(format string, v ...interface{}) {
	fmt.Printf("D: [topology] "+format+"\n", v...)
}

var pkgDir string

func setupTestEnv(t *testing.T) func() {
	SetLogger(logger(0))

	pwd, err := os.Getwd()
	if err != nil {
		t.Fatal("unable to get current directory")
	}
	if pkgDir == "" {
		pkgDir = pwd
	}
	if path, err := filepath.EvalSymlinks(pwd); err == nil {
		pwd = path
	}
	SetSysRoot(pwd + "/testdata")
	teardown := func() {
		SetSysRoot("")
		os.Chdir(pkgDir)
	}

	return teardown
}

func TestMapKeys(t *testing.T) {
	cases := []struct {
		name   string
		input  map[string]bool
		output []string
	}{
		{
			name:   "empty",
			input:  map[string]bool{},
			output: []string{},
		},
		{
			name:   "one",
			input:  map[string]bool{"a": false},
			output: []string{"a"},
		},
		{
			name:   "multiple",
			input:  map[string]bool{"a": false, "b": true, "c": false},
			output: []string{"a", "b", "c"},
		},
	}
	for _, tc := range cases {
		test := tc
		t.Run(test.name, func(t *testing.T) {
			output := mapKeys(test.input)
			sort.Strings(output)
			if !reflect.DeepEqual(output, test.output) {
				t.Fatalf("expected output: %+v got: %+v", test.output, output)
			}
		})
	}
}

func TestSetSysRoot(t *testing.T) {
	teardown := setupTestEnv(t)
	defer teardown()

	type testCase struct {
		name   string
		cwd    string
		input  string
		result string
	}
	for _, tc := range []*testCase{
		{
			name:   "empty sysroot",
			input:  "",
			result: "",
		},
		{
			name:   "/ sysroot",
			input:  "/",
			result: "",
		},
		{
			name:   "less obvious / sysroot",
			input:  "/../../..////../",
			result: "",
		},
		{
			name:   "non-clean absolute path",
			input:  "/a/b/../c",
			result: "/a/c",
		},
		{
			name:   "clean absolute path",
			input:  "/mnt/host",
			result: "/mnt/host",
		},
		{
			name:   "non-clean relative path",
			cwd:    "/tmp",
			input:  "../foo/bar",
			result: "/foo/bar",
		},
		{
			name:   "clean relative path",
			cwd:    "/tmp",
			input:  "foo/bar",
			result: "/tmp/foo/bar",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cwd != "" {
				if err := os.Chdir(tc.cwd); err != nil {
					t.Skip(fmt.Sprintf("%s: failed to change directory to %s: %v",
						tc.name, tc.cwd, err))
				}
			}
			SetSysRoot(tc.input)
			if sysRoot != tc.result {
				t.Fatalf("expected %s, got %s", tc.result, sysRoot)
			}
		})
	}
}

func TestFindSysFsDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	teardown := setupTestEnv(t)
	defer teardown()
	cases := []struct {
		name        string
		input       string
		output      string
		expectedErr bool
	}{
		{
			name:        "empty",
			input:       "",
			output:      "",
			expectedErr: false,
		},
		{
			name:        "null",
			input:       "/dev/null",
			output:      "/sys/devices/virtual/mem/null",
			expectedErr: false,
		},
		{
			name:        "proc",
			input:       "/proc/self",
			output:      "",
			expectedErr: true,
		},
	}
	for _, tc := range cases {
		test := tc
		t.Run(test.name, func(t *testing.T) {
			output, err := FindSysFsDevice(test.input)
			switch {
			case err != nil && !test.expectedErr:
				t.Fatalf("unexpected error returned: %+v", err)
			case err == nil && test.expectedErr:
				t.Fatalf("unexpected success: %+v", output)
			case output != test.output:
				t.Fatalf("expected: %q got: %q", test.output, output)
			}
		})
	}
}

func TestReadFilesInDirectory(t *testing.T) {
	var file, empty string
	fname := "test-a"
	content := []byte(" something\n")
	expectedContent := "something"

	fileMap := map[string]*string{
		fname:          &file,
		"non_existing": &empty,
	}

	dir, err := os.MkdirTemp("", "readFilesInDirectory")
	if err != nil {
		t.Fatalf("unable to create test directory: %+v", err)
	}
	defer os.RemoveAll(dir)
	os.WriteFile(filepath.Join(dir, fname), content, 0644)

	if err = readFilesInDirectory(fileMap, dir); err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if empty != "" {
		t.Fatalf("unexpected content: %q", empty)
	}
	if file != expectedContent {
		t.Fatalf("unexpected content: %q expected: %q", file, expectedContent)
	}
}

func TestGetDevicesFromVirtual(t *testing.T) {
	teardown := setupTestEnv(t)
	defer teardown()

	cases := []struct {
		name        string
		input       string
		output      []string
		expectedErr bool
	}{
		{
			name:        "vfio",
			input:       "/sys/devices/virtual/vfio/42",
			output:      []string{sysRoot + "/sys/devices/pci0000:00/0000:00:02.0"},
			expectedErr: false,
		},
		{
			name:        "misc",
			input:       "/sys/devices/virtual/misc/vfio",
			output:      nil,
			expectedErr: false,
		},
		{
			name:        "missing-iommu-group",
			input:       "/sys/devices/virtual/vfio/84",
			output:      nil,
			expectedErr: true,
		},
		{
			name:        "non-virtual",
			input:       "/sys/devices/pci0000:00/0000:00:02.0",
			output:      nil,
			expectedErr: true,
		},
		{
			name:        "garbage",
			input:       "./sys/devices/virtual/vfio/42",
			output:      nil,
			expectedErr: true,
		},
	}

	for _, tc := range cases {
		test := tc
		t.Run(test.name, func(t *testing.T) {
			output, err := getDevicesFromVirtual(test.input)
			switch {
			case err != nil && !test.expectedErr:
				t.Fatalf("unexpected error returned: %+v", err)
			case err == nil && test.expectedErr:
				t.Fatalf("unexpected success: %+v", output)
			case len(output) != len(test.output):
				t.Fatalf("expected: %q got: %q", len(test.output), len(output))
			}
			for i, p := range test.output {
				if test.output[i] != p {
					t.Fatalf("expected: %q got: %q", test.output[i], p)
				}
			}
		})
	}
}

func TestMergeTopologyHints(t *testing.T) {
	cases := []struct {
		name           string
		inputA         Hints
		inputB         Hints
		expectedOutput Hints
		expectedErr    bool
	}{
		{
			name:           "empty",
			inputA:         nil,
			inputB:         nil,
			expectedOutput: Hints{},
		},
		{
			name:           "one,nil",
			inputA:         Hints{"test": Hint{Provider: "test", CPUs: "0"}},
			inputB:         nil,
			expectedOutput: Hints{"test": Hint{Provider: "test", CPUs: "0"}},
		},
		{
			name:           "nil, one",
			inputA:         nil,
			inputB:         Hints{"test": Hint{Provider: "test", CPUs: "0"}},
			expectedOutput: Hints{"test": Hint{Provider: "test", CPUs: "0"}},
		},
		{
			name:           "duplicate",
			inputA:         Hints{"test": Hint{Provider: "test", CPUs: "0"}},
			inputB:         Hints{"test": Hint{Provider: "test", CPUs: "0"}},
			expectedOutput: Hints{"test": Hint{Provider: "test", CPUs: "0"}},
		},
		{
			name:   "two",
			inputA: Hints{"test1": Hint{Provider: "test1", CPUs: "0"}},
			inputB: Hints{"test2": Hint{Provider: "test2", CPUs: "1"}},
			expectedOutput: Hints{
				"test1": Hint{Provider: "test1", CPUs: "0"},
				"test2": Hint{Provider: "test2", CPUs: "1"},
			},
		},
	}
	for _, tc := range cases {
		test := tc
		t.Run(test.name, func(t *testing.T) {
			output := MergeTopologyHints(test.inputA, test.inputB)
			if !reflect.DeepEqual(output, test.expectedOutput) {
				t.Fatalf("expected output: %+v got: %+v", test.expectedOutput, output)
			}
		})
	}
}

func TestNewTopologyHints(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	teardown := setupTestEnv(t)
	defer teardown()
	cases := []struct {
		name        string
		input       string
		output      Hints
		expectedErr bool
	}{
		{
			name:        "empty",
			input:       "non-existing",
			output:      nil,
			expectedErr: true,
		},
		{
			name:  "pci card1",
			input: "/sys/devices/pci0000:00/0000:00:02.0/drm/card1",
			output: Hints{
				"/sys/devices/pci0000:00/0000:00:02.0": Hint{
					Provider:  "/sys/devices/pci0000:00/0000:00:02.0",
					CPUs:      "0-7",
					NUMAs:     "",
					Sockets:   "",
					PCIeChain: []PCIeHop{{Address: "pci0000:00", Type: PCIeHopRoot}},
					IRQs:      []int{16, 24, 25},
				},
			},
			expectedErr: false,
		},
		{
			name:  "pci endpoint behind a bridge",
			input: "/sys/devices/pci0000:00/0000:00:1c.0/0000:01:00.0",
			output: Hints{
				"/sys/devices/pci0000:00/0000:00:1c.0/0000:01:00.0": Hint{
					Provider: "/sys/devices/pci0000:00/0000:00:1c.0/0000:01:00.0",
					CPUs:     "8-15",
					NUMAs:    "1",
					Sockets:  "",
					PCIeChain: []PCIeHop{
						{Address: "0000:00:1c.0", Type: PCIeHopBridge},
						{Address: "pci0000:00", Type: PCIeHopRoot},
					},
					// irq is 0 (no legacy interrupt), only the
					// MSI vector should show up here.
					IRQs: []int{33},
				},
			},
			expectedErr: false,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output, err := NewTopologyHints(test.input)
			switch {
			case err != nil && !test.expectedErr:
				t.Fatalf("unexpected error returned: %+v", err)
			case err == nil && test.expectedErr:
				t.Fatalf("unexpected success: %+v", output)
			case !reflect.DeepEqual(output, test.output):
				t.Fatalf("expected: %v got: %v", test.output, output)
			}
		})
	}
}

func TestPcieChain(t *testing.T) {
	teardown := setupTestEnv(t)
	defer teardown()
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatal("unable to get current directory")
	}
	root := pwd + "/testdata"

	cases := []struct {
		name   string
		devDir string
		want   []PCIeHop
	}{
		{
			name:   "endpoint directly on the root complex",
			devDir: root + "/sys/devices/pci0000:00/0000:00:02.0",
			want:   []PCIeHop{{Address: "pci0000:00", Type: PCIeHopRoot}},
		},
		{
			name:   "endpoint behind one bridge",
			devDir: root + "/sys/devices/pci0000:00/0000:00:1c.0/0000:01:00.0",
			want: []PCIeHop{
				{Address: "0000:00:1c.0", Type: PCIeHopBridge},
				{Address: "pci0000:00", Type: PCIeHopRoot},
			},
		},
		{
			name:   "non-PCI device has no chain",
			devDir: root + "/sys/devices/virtual/mem/null",
			want:   []PCIeHop{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pcieChain(tc.devDir); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("pcieChain(%s) = %+v, want %+v", tc.devDir, got, tc.want)
			}
		})
	}
}

func TestDeviceIRQs(t *testing.T) {
	teardown := setupTestEnv(t)
	defer teardown()
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatal("unable to get current directory")
	}
	root := pwd + "/testdata"

	cases := []struct {
		name   string
		devDir string
		want   []int
	}{
		{
			name:   "legacy irq plus non-overlapping MSI vectors",
			devDir: root + "/sys/devices/pci0000:00/0000:00:02.0",
			want:   []int{16, 24, 25},
		},
		{
			name:   "legacy irq overlapping an MSI vector is deduplicated",
			devDir: root + "/sys/devices/pci0000:00/0000:00:03.0",
			want:   []int{24, 26},
		},
		{
			name:   "irq==0 means MSI-only",
			devDir: root + "/sys/devices/pci0000:00/0000:00:1c.0/0000:01:00.0",
			want:   []int{33},
		},
		{
			name:   "no irq file, no msi_irqs dir",
			devDir: root + "/sys/devices/virtual/mem/null",
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceIRQs(tc.devDir); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deviceIRQs(%s) = %v, want %v", tc.devDir, got, tc.want)
			}
		})
	}
}
