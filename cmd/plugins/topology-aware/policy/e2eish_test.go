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

package topologyaware

// The e2eish tests of the policy, which replay e2e tests against the policy
// alone, on the harness of pkg/testutils/e2eish. See "Replaying test logs as
// unit tests" in test/e2e/README.md.

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	harness "github.com/containers/nri-plugins/pkg/testutils/e2eish"
)

// e2eishPlugin is the policy for the harness.
var e2eishPlugin = &harness.Plugin{
	Name:   "topology-aware",
	CRD:    "config.nri_topologyawarepolicies.yaml",
	New:    func() policyapi.Backend { return New() },
	Config: func() any { return &cfgapi.Config{} },
	OffHost: func(cfg any) any {
		c := cfg.(*cfgapi.Config).DeepCopy()
		c.ControllableInterrupts = []string{harness.NoInterrupts}
		return c
	},
	UnsupportedPod: func(pod *corev1.Pod) string {
		for key := range pod.Annotations {
			if strings.HasPrefix(key, keyIrqAffinity) {
				return "a pod with IRQ affinity, of the IRQs of the host running the replay"
			}
		}
		return ""
	},
}

func TestE2eishReplay(t *testing.T) {
	harness.Replay(t, e2eishPlugin)
}
