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

package balloons

import (
	"testing"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	harness "github.com/containers/nri-plugins/pkg/testutils/e2eish"
)

// e2eishPlugin is the policy for the harness of pkg/testutils/e2eish.
var e2eishPlugin = &harness.Plugin{
	Name:   "balloons",
	CRD:    "config.nri_balloonspolicies.yaml",
	New:    func() policyapi.Backend { return New() },
	Config: func() any { return &BalloonsOptions{} },
	OffHost: func(cfg any) any {
		c := cfg.(*BalloonsOptions).DeepCopy()
		c.ControllableInterrupts = []string{harness.NoInterrupts}
		return c
	},
	UnsupportedConfig: func(cfg any) string {
		for _, def := range cfg.(*BalloonsOptions).BalloonDefs {
			if len(def.IrqClaim) > 0 || def.IrqMode != "" {
				return "balloon type " + def.Name + " with IRQ options, for the IRQs of the host running the replay"
			}
		}
		return ""
	},
}

// TestE2eishReplay replays e2e test logs against the policy alone. See
// "Replaying test logs as unit tests" in test/e2e/README.md.
func TestE2eishReplay(t *testing.T) {
	harness.Replay(t, e2eishPlugin)
}
