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
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/pkg/cxl/config"
)

// The usable size and the coherence of a local memdev from its
// memdevSettings, which match a memdev by name or serial in every notation;
// without a configuration nothing is coherent; invalid settings are refused.
func TestMemdevSettings(t *testing.T) {
	cfg, err := newDriverConfigFromString(`
memdevSettings:
- memdev: "0x0badc0dedeadbeef"
  usableSize: 128Gi
- memdev: mem1
  usableSize: 1000Mi
  coherent: true
- memdev: "3238065634"
  usableSize: 1Gi
- memdev: "0x00c100f5e1"
  coherent: true
`)
	noErr(t, err)
	// the publisher rounds the usable sizes down (internal/dra
	// TestMemdevUsableSize)
	for _, tc := range []struct {
		name     string
		serial   uint64
		usable   uint64
		coherent bool
	}{
		{"mem0", 0x0badc0dedeadbeef, 128 << 30, false},
		{"mem1", 0x1, 1000 << 20, true},
		{"mem2", 0x2, 0, false},
		{"mem4", 0xc100f5e2, 1 << 30, false},
		{"mem5", 0xc100f5e1, 0, true},
	} {
		if got := cfg.configuredUsableSize(tc.name, tc.serial); got != tc.usable {
			t.Errorf("%s: configured usable size %d, expected %d", tc.name, got, tc.usable)
		}
		if got := cfg.memdevCoherent(tc.name, tc.serial); got != tc.coherent {
			t.Errorf("%s: coherent %v, expected %v", tc.name, got, tc.coherent)
		}
	}
	var none *DriverConfig
	if none.memdevCoherent("mem0", 1) {
		t.Fatal("coherent without a configuration")
	}
	for spec, want := range map[string]bool{"mem0": true, "cxl/mem0": true, "0xc100f5e1": true, "0XC100F5E1": true, "3238065633": true, "0x1": false, "mem1": false} {
		cfg, err := newDriverConfigFromString(fmt.Sprintf("memdevSettings: [{memdev: %q, coherent: true}]", spec))
		noErr(t, err)
		if got := cfg.memdevCoherent("mem0", 0xc100f5e1); got != want {
			t.Errorf("device %q: coherent %v, expected %v", spec, got, want)
		}
	}
	for _, bad := range []string{
		"memdevSettings: [{usableSize: 1Gi}]",
		"memdevSettings: [{memdev: mem0, usableSize: 1x}]",
		"memdevSettings: [{memdev: mem0, usableSize: 100Mi}]",
		`memdevSettings: [{memdev: "c100f5e1"}]`,
		`memdevSettings: [{memdev: "251:0"}]`,
		`memdevSettings: [{memdev: "0x"}]`,
	} {
		if _, err := newDriverConfigFromString(bad); err == nil {
			t.Errorf("no error for %q", bad)
		}
	}
}

// TestChartSchemaKeys checks that the values schema of the Helm chart
// has the keys of the configuration file and of the MemoryConfig bodies
// of the classes: the json tags of the Go types that decode them.
func TestChartSchemaKeys(t *testing.T) {
	data, err := os.ReadFile("../../../deployment/helm/dra-driver-cxl/values.schema.json")
	noErr(t, err)
	var schema map[string]any
	noErr(t, json.Unmarshal(data, &schema))
	// keys returns the property names of the schema object at path, the
	// object of #/definitions/<name> for a "$ref"
	keys := func(path ...string) []string {
		var node any = schema
		for _, p := range path {
			node = node.(map[string]any)[p]
			if ref, ok := node.(map[string]any)["$ref"].(string); ok {
				node = schema["definitions"].(map[string]any)[strings.TrimPrefix(ref, "#/definitions/")]
			}
		}
		var out []string
		for k := range node.(map[string]any)["properties"].(map[string]any) {
			out = append(out, k)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	tags := func(v any) []string {
		var out []string
		for f := range reflect.TypeOf(v).Fields() {
			if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); f.IsExported() && name != "" && name != "-" {
				out = append(out, name)
			}
		}
		slices.Sort(out)
		return out
	}
	var device []string
	for _, kind := range []string{"volatileDevice", "sharedDevice", "persistentDevice", "numaDevice", "leaseDevice"} {
		device = append(device, keys("definitions", kind)...)
	}
	slices.Sort(device)
	body := tags(config.MemoryConfig{})
	body = slices.DeleteFunc(body, func(k string) bool { return k == "apiVersion" || k == "kind" })
	for _, tc := range []struct {
		what      string
		got, want []string
	}{
		{"config", keys("properties", "config"), tags(DriverConfig{})},
		{"config.memdevSettings[]", keys("properties", "config", "properties", "memdevSettings", "items"), tags(memdevSetting{})},
		{"config.defaults", keys("properties", "config", "properties", "defaults"), tags(config.Defaults{})},
		{"config.defaults.container", keys("properties", "config", "properties", "defaults", "properties", "container"), tags(config.Container{})},
		{"container", keys("definitions", "container"), []string{"steering"}},
		{"steering", keys("definitions", "steering"), tags(config.Steering{})},
		{"device", slices.Compact(device), tags(config.Device{})},
		{"the body of cxl-pool", keys("properties", "deviceClassConfig", "properties", "cxl-pool"), body},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s: the schema has %v, the configuration %v", tc.what, tc.got, tc.want)
		}
	}
}
