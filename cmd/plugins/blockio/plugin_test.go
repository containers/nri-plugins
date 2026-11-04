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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/intel/goresctrl/pkg/blockio"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

func TestAnnotationSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pod      map[string]string
		ctr      map[string]string
		disabled bool
		want     string
	}{
		{name: "unannotated"},
		{name: "pod default", pod: map[string]string{blockio.BlockioPodAnnotation: "pod"}, want: "100"},
		{
			name: "per-container override",
			pod: map[string]string{
				blockio.BlockioPodAnnotation: "pod", blockio.BlockioPodAnnotationContainerPrefix + "app": "specific",
			}, want: "200",
		},
		{
			name: "CRI override",
			pod: map[string]string{
				blockio.BlockioPodAnnotation: "pod", blockio.BlockioPodAnnotationContainerPrefix + "app": "specific",
			},
			ctr: map[string]string{blockio.BlockioContainerAnnotation: "cri"}, want: "300",
		},
		{
			name: "other container is unaffected",
			pod: map[string]string{
				blockio.BlockioPodAnnotation: "pod", blockio.BlockioPodAnnotationContainerPrefix + "sidecar": "specific",
			}, want: "100",
		},
		{
			name: "empty per-container override",
			pod: map[string]string{
				blockio.BlockioPodAnnotation: "pod", blockio.BlockioPodAnnotationContainerPrefix + "app": "",
			},
		},
		{
			name: "empty CRI override", pod: map[string]string{blockio.BlockioPodAnnotation: "pod"},
			ctr: map[string]string{blockio.BlockioContainerAnnotation: ""},
		},
		{
			name: "unknown override does not fall back", pod: map[string]string{blockio.BlockioPodAnnotation: "pod"},
			ctr: map[string]string{blockio.BlockioContainerAnnotation: "unknown"},
		},
		{name: "no configuration", pod: map[string]string{blockio.BlockioPodAnnotation: "pod"}, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &plugin{
				configFile: "already-loaded.yaml",
				classes: map[string]map[string]string{
					"pod": {"io.bfq.weight": "100"}, "specific": {"io.bfq.weight": "200"}, "cri": {"io.bfq.weight": "300"},
				},
			}
			if tc.disabled {
				p.configFile = ""
			}
			pod := &api.PodSandbox{Name: "pod", Namespace: "test", Annotations: tc.pod}
			ctr := &api.Container{Name: "app", Annotations: tc.ctr, Linux: &api.LinuxContainer{
				Resources: &api.LinuxResources{Unified: map[string]string{"memory.high": "1000000", "io.max": "8:0 rbps=100"}},
			}}
			before := proto.Clone(ctr)
			adjustment, updates, err := p.CreateContainer(context.Background(), pod, ctr)
			if err != nil || len(updates) != 0 {
				t.Fatalf("unexpected updates %v or error %v", updates, err)
			}
			if !proto.Equal(ctr, before) {
				t.Fatal("input container resources were mutated")
			}
			if tc.want == "" {
				if adjustment != nil {
					t.Fatalf("expected no adjustment, got %v", adjustment)
				}
				return
			}
			want := &api.ContainerAdjustment{Linux: &api.LinuxContainerAdjustment{
				Resources: &api.LinuxResources{Unified: map[string]string{"io.bfq.weight": tc.want}},
			}}
			if !proto.Equal(adjustment, want) {
				t.Fatalf("adjustment = %v, want %v", adjustment, want)
			}
		})
	}
}

func TestReadClasses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  string
		want    map[string]*oci.LinuxBlockIO
		wantErr string
	}{
		{
			name: "empty",
			want: map[string]*oci.LinuxBlockIO{},
		},
		{
			name:   "null",
			config: "null\n",
			want:   map[string]*oci.LinuxBlockIO{},
		},
		{
			name: "valid class",
			config: `Classes:
  gold:
    - Weight: 400
`,
			want: map[string]*oci.LinuxBlockIO{
				"gold": {Weight: uint16ptr(400)},
			},
		},
		{
			name: "unknown top-level field",
			config: `UnknownKey: ignored
Classes: {}
`,
			wantErr: "unknown field",
		},
		{
			name: "unknown class field",
			config: `Classes:
  gold:
    - Weight: 400
      UnknownField: ignored
`,
			wantErr: "unknown field",
		},
		{
			name: "partial class",
			config: `Classes:
  partial:
    - Weight: 400
    - Weight: invalid
`,
			wantErr: "Weight",
		},
		{
			name: "out of range default",
			config: `Classes:
  invalid:
    - Weight: 1001
`,
			wantErr: "Weight",
		},
		{
			name: "invalid throttle",
			config: `Classes:
  partial:
    - Weight: 400
    - ThrottleReadBps: invalid
`,
			wantErr: "ThrottleReadBps",
		},
		{
			name: "throttling without devices",
			config: `Classes:
  partial:
    - Weight: 100
      ThrottleReadBps: 50M
`,
			wantErr: "Devices not listed",
		},
		{
			name: "unmatched devices",
			config: `Classes:
  missing:
    - Devices: [/nri-blockio-does-not-exist/*]
      ThrottleReadBps: 50M
`,
			wantErr: "matches no devices",
		},
		{
			name: "empty device list",
			config: `Classes:
  empty:
    - Devices: []
      Weight: 100
`,
			wantErr: "Devices must not be empty",
		},
		{
			name: "invalid device glob",
			config: `Classes:
  invalid:
    - Devices: ['[']
      ThrottleReadBps: 50M
`,
			wantErr: "invalid device pattern",
		},
		{
			name:    "malformed YAML",
			config:  "Classes: [",
			wantErr: "parsing block I/O configuration",
		},
		{
			name: "wrong class shape",
			config: `Classes:
  gold: wrong
`,
			wantErr: "parsing block I/O configuration",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "classes.yaml")
			writeConfig(t, path, tc.config)
			got, err := readClasses(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != nil {
					t.Fatalf("classes = %v, error = %v; want no classes and error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("classes = %+v, want %+v", got, tc.want)
			}
		})
	}
	if _, err := readClasses(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected file-read error")
	}
}

func TestReadClassesRejectsInvalidDevices(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	writeConfig(t, file, "not a device")
	brokenLink := filepath.Join(root, "broken-link")
	if err := os.Symlink(filepath.Join(root, "missing"), brokenLink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, root, "/dev/null", brokenLink} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "classes.yaml")
			writeConfig(t, config, fmt.Sprintf("Classes:\n  partial:\n    - Weight: 400\n    - Devices: [%q]\n      ThrottleReadBps: 50M\n", path))
			classes, err := readClasses(config)
			if err == nil || classes != nil || !strings.Contains(err.Error(), `class "partial" entry 2`) {
				t.Fatalf("classes = %v, error = %v; want invalid-device error and no partial classes", classes, err)
			}
		})
	}
}

func TestStartupConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		remove bool
	}{
		{name: "changed", config: "Classes:\n  gold:\n    - Weight: 400\n"},
		{name: "malformed", config: "Classes: ["},
		{name: "missing", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "classes.yaml")
			writeConfig(t, path, "Classes:\n  gold:\n    - Weight: 100\n")
			p := &plugin{configFile: path}
			if err := p.loadConfig(); err != nil {
				t.Fatal(err)
			}
			if tc.remove {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeConfig(t, path, tc.config)
			}
			pod := &api.PodSandbox{Annotations: map[string]string{blockio.BlockioPodAnnotation: "gold"}}
			ctr := &api.Container{Name: "app"}
			adjustment, _, err := p.CreateContainer(context.Background(), pod, ctr)
			if err != nil {
				t.Fatal(err)
			}
			if got := adjustment.GetLinux().GetResources().GetUnified()["io.bfq.weight"]; got != "100" {
				t.Fatalf("weight = %q, want startup weight 100", got)
			}
			if _, _, err := p.CreateContainer(context.Background(), nil, ctr); err != nil {
				t.Fatalf("unannotated container creation failed: %v", err)
			}
		})
	}
}

func TestConcurrentClassSnapshots(t *testing.T) {
	// goresctrl's global class map must not leak between independent startup snapshots.
	pod := &api.PodSandbox{Annotations: map[string]string{blockio.BlockioPodAnnotation: "gold"}}
	ctr := &api.Container{Name: "app"}
	var workers sync.WaitGroup
	for i, weight := range []string{"100", "400"} {
		path := filepath.Join(t.TempDir(), "classes.yaml")
		writeConfig(t, path, "Classes:\n  gold:\n    - Weight: "+weight+"\n")
		for range 10 {
			workers.Go(func() {
				p := &plugin{configFile: path}
				if err := p.loadConfig(); err != nil {
					t.Errorf("plugin %d configuration failed: %v", i, err)
					return
				}
				adjustment, _, err := p.CreateContainer(context.Background(), pod, ctr)
				if err != nil {
					t.Errorf("plugin %d creation failed: %v", i, err)
					return
				}
				if got := adjustment.GetLinux().GetResources().GetUnified()["io.bfq.weight"]; got != weight {
					t.Errorf("plugin %d weight = %q, want %q", i, got, weight)
				}
			})
		}
	}
	workers.Wait()
}

// This exercises goresctrl's real discovery and rule ordering without opening
// a block device. Creating the fixture nodes requires CAP_MKNOD.
func TestDeviceDiscoveryAndOverrides(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "disk0")
	second := filepath.Join(root, "disk1")
	for i, path := range []string{first, second} {
		err := unix.Mknod(path, unix.S_IFBLK|0600, int(unix.Mkdev(240, uint32(i*16))))
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skip("block-device discovery fixtures require CAP_MKNOD")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "by-id")
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "classes.yaml")
	writeConfig(t, path, fmt.Sprintf(`Classes:
  bronze:
    - Devices: [%q]
      Weight: 100
      ThrottleReadBps: 50M
      ThrottleWriteBps: 10Mi
      ThrottleReadIOPS: 10k
      ThrottleWriteIOPS: 5k
    - Devices: [%q]
      Weight: 200
      ThrottleReadBps: 0
`, filepath.Join(root, "disk*"), link))
	p := &plugin{configFile: path}
	if err := p.loadConfig(); err != nil {
		t.Fatal(err)
	}
	got := p.classes["bronze"]
	weightLines := strings.Split(got["io.bfq.weight"], "\n")
	sort.Strings(weightLines)
	got["io.bfq.weight"] = strings.Join(weightLines, "\n")
	want := map[string]string{
		"io.bfq.weight": "240:0 100\n240:16 200",
		"io.max": "240:0 rbps=50000000 wbps=10485760 riops=10000 wiops=5000\n" +
			"240:16 rbps=max wbps=10485760 riops=10000 wiops=5000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unified = %v, want %v", got, want)
	}
	writeConfig(t, filepath.Join(root, "disk-invalid"), "not a device")
	if classes, err := readClasses(path); err == nil || classes != nil {
		t.Fatalf("classes = %v, error = %v; want rejection when a device glob also matches a regular file", classes, err)
	}
}

func writeConfig(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
