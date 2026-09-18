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

package v1alpha1

import (
	"testing"

	"sigs.k8s.io/yaml"

	cpucfg "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/cpu"
)

// The CPU controller which consumed control.cpu.classes is gone, but the
// balloons policy still translates those classes into its own cpuClasses, so a
// configuration which carries them has to keep working. Validate warns about
// them and does not reject them.
func TestCommonConfigValidateLegacyCPUClasses(t *testing.T) {
	t.Run("with-classes", func(t *testing.T) {
		c := &CommonConfig{}
		c.Control.CPU.Classes = map[string]cpucfg.Class{
			"legacy-fast": {MinFreq: 3800000, MaxFreq: 3800000},
		}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() rejected a legacy config: %v", err)
		}
	})

	t.Run("without-classes", func(t *testing.T) {
		c := &CommonConfig{}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	t.Run("nil", func(t *testing.T) {
		var c *CommonConfig
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() on nil = %v, want nil", err)
		}
	})
}

// TestCommonConfigDRA verifies that dra.enabled reaches CommonConfig from every
// policy's configuration, and that leaving it out is distinguishable from
// setting it to false.
func TestCommonConfigDRA(t *testing.T) {
	policies := []struct {
		name string
		new  func() ResmgrConfig
	}{
		{"topology-aware", func() ResmgrConfig { return &TopologyAwarePolicy{} }},
		{"balloons", func() ResmgrConfig { return &BalloonsPolicy{} }},
		{"template", func() ResmgrConfig { return &TemplatePolicy{} }},
	}

	enabled, disabled := true, false

	configs := []struct {
		name    string
		spec    string
		enabled *bool
	}{
		{"no dra section", "spec: {}", nil},
		{"empty dra section", "spec:\n  dra: {}\n", nil},
		{"dra disabled", "spec:\n  dra:\n    enabled: false\n", &disabled},
		{"dra enabled", "spec:\n  dra:\n    enabled: true\n", &enabled},
	}

	for _, p := range policies {
		for _, c := range configs {
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				cfg := p.new()
				if err := yaml.Unmarshal([]byte(c.spec), cfg); err != nil {
					t.Fatalf("failed to unmarshal %q: %v", c.spec, err)
				}
				switch got := cfg.CommonConfig().DRA.Enabled; {
				case (got == nil) != (c.enabled == nil):
					t.Errorf("dra.enabled is %v, expected %v", got, c.enabled)
				case got != nil && *got != *c.enabled:
					t.Errorf("dra.enabled is %v, expected %v", *got, *c.enabled)
				}
			})
		}
	}
}
