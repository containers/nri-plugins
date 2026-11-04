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
				configFile: "already-loaded.yaml", weights: weightSupport{bfq: true},
				classes: map[string]*oci.LinuxBlockIO{
					"pod": {Weight: uint16ptr(100)}, "specific": {Weight: uint16ptr(200)}, "cri": {Weight: uint16ptr(300)},
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
		wantErr bool
	}{
		{name: "empty", want: map[string]*oci.LinuxBlockIO{}},
		{name: "null", config: "null\n", want: map[string]*oci.LinuxBlockIO{}},
		{
			name: "CRI-O format with unknown keys", config: "UnknownKey: ignored\nClasses:\n  gold:\n    - Weight: 400\n      UnknownField: ignored\n",
			want: map[string]*oci.LinuxBlockIO{"gold": {Weight: uint16ptr(400)}},
		},
		{
			name: "forced partial class", config: "Classes:\n  partial:\n    - Weight: 400\n    - Weight: invalid\n",
			want: map[string]*oci.LinuxBlockIO{"partial": {Weight: uint16ptr(400)}},
		},
		{
			name: "out of range default", config: "Classes:\n  invalid:\n    - Weight: 1001\n",
			want: map[string]*oci.LinuxBlockIO{"invalid": {}},
		},
		{
			name: "throttling without devices", config: "Classes:\n  partial:\n    - Weight: 100\n      ThrottleReadBps: 50M\n",
			want: map[string]*oci.LinuxBlockIO{"partial": {Weight: uint16ptr(100)}},
		},
		{
			name: "unmatched devices", config: "Classes:\n  missing:\n    - Devices: [/nri-blockio-does-not-exist/*]\n      ThrottleReadBps: 50M\n",
			want: map[string]*oci.LinuxBlockIO{"missing": {}},
		},
		{name: "malformed YAML", config: "Classes: [", wantErr: true},
		{name: "wrong class shape", config: "Classes:\n  gold: wrong\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "classes.yaml")
			writeConfig(t, path, tc.config)
			got, err := readClasses(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected error %v", err)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("classes = %+v, want %+v", got, tc.want)
			}
		})
	}
	if _, err := readClasses(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected file-read error")
	}
}

func TestReload(t *testing.T) {
	for _, reload := range []bool{false, true} {
		name := "startup only"
		if reload {
			name = "reload before creation"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "classes.yaml")
			writeConfig(t, path, "Classes:\n  gold:\n    - Weight: 100\n")
			p := &plugin{configFile: path, reload: reload, weights: weightSupport{bfq: true}}
			if err := p.loadConfig(); err != nil {
				t.Fatal(err)
			}
			writeConfig(t, path, "Classes:\n  gold:\n    - Weight: 400\n")
			pod := &api.PodSandbox{Annotations: map[string]string{blockio.BlockioPodAnnotation: "gold"}}
			ctr := &api.Container{Name: "app"}
			adjustment, _, err := p.CreateContainer(context.Background(), pod, ctr)
			if err != nil {
				t.Fatal(err)
			}
			want := "100"
			if reload {
				want = "400"
			}
			if got := adjustment.GetLinux().GetResources().GetUnified()["io.bfq.weight"]; got != want {
				t.Fatalf("weight = %q, want %q", got, want)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			adjustment, _, err = p.CreateContainer(context.Background(), pod, ctr)
			if err != nil {
				t.Fatalf("reload=%v: creation failed after file removal: %v", reload, err)
			}
			if reload && adjustment != nil {
				t.Fatal("failed reload applied stale class settings")
			}
			// No selection must succeed even if a reload would fail.
			if _, _, err := p.CreateContainer(context.Background(), nil, ctr); err != nil {
				t.Fatalf("unannotated container triggered reload: %v", err)
			}
			writeConfig(t, path, "Classes: [")
			before := p.classes
			if err := p.loadConfig(); err == nil {
				t.Fatal("expected malformed reload to fail")
			}
			if !reflect.DeepEqual(before, p.classes) {
				t.Fatal("failed reload replaced the previous snapshot")
			}
		})
	}
}

func TestConcurrentClassSnapshots(t *testing.T) {
	// goresctrl's global class map must not leak between independent snapshots,
	// including while several creation callbacks reload the same plugin.
	pod := &api.PodSandbox{Annotations: map[string]string{blockio.BlockioPodAnnotation: "gold"}}
	ctr := &api.Container{Name: "app"}
	var workers sync.WaitGroup
	for i, weight := range []string{"100", "400"} {
		path := filepath.Join(t.TempDir(), "classes.yaml")
		writeConfig(t, path, "Classes:\n  gold:\n    - Weight: "+weight+"\n")
		p := &plugin{configFile: path, reload: true, weights: weightSupport{bfq: true}}
		if err := p.loadConfig(); err != nil {
			t.Fatal(err)
		}
		for range 10 {
			workers.Go(func() {
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
	classes, err := readClasses(path)
	if err != nil {
		t.Fatal(err)
	}
	got := linuxBlockIOToUnified(classes["bronze"], weightSupport{bfq: true, deviceWeights: true})
	want := map[string]string{
		"io.bfq.weight": "240:0 100\n240:16 200",
		"io.max": "240:0 rbps=50000000 wbps=10485760 riops=10000 wiops=5000\n" +
			"240:16 rbps=max wbps=10485760 riops=10000 wiops=5000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unified = %v, want %v", got, want)
	}
}

func writeConfig(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
