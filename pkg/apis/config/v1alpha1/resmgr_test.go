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
