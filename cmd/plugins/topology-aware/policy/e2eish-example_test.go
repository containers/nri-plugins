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

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	harness "github.com/containers/nri-plugins/pkg/testutils/e2eish"
)

// Test that
//   - guaranteed containers of one and two CPUs get CPUs of their own,
//     the latter a full core,
//   - the burstable containers of a pod spread over nodes,
//   - a pod of its own template gets the CPUs it asks for.
func TestE2eishExample(t *testing.T) {
	// This is an example of an e2eish test to generate an e2e test from:
	// n4c16/test99-e2eish-example, with
	//
	//     E2EISH_EXAMPLES=1 test/e2e/emit_tests.sh topology-aware -- -run TestE2eishExample
	//
	// It is skipped unless E2EISH_EXAMPLES=1. See "Generating e2e tests
	// from unit tests" in test/e2e/README.md.
	if os.Getenv("E2EISH_EXAMPLES") != "1" {
		t.Skip("an example, run with E2EISH_EXAMPLES=1")
	}

	s := harness.NewE2EScript(t, e2eishPlugin, "n4c16/test99-e2eish-example")
	s.Launch(genN4c16, genN4c16BaseConfig)
	s.SyncWithRuntimePods(genN4c16Pods...)

	s.CreateGuaranteed(harness.PodSpec{CPU: "1"})
	require.Len(t, s.CPUs("pod0c0"), 1)
	s.CreateGuaranteed(harness.PodSpec{CPU: "2"})
	require.Len(t, s.CPUs("pod1c0"), 2)
	require.Len(t, s.Cores("pod1c0"), 1)
	harness.RequireDisjoint(t, s.CPUs("pod0c0"), s.CPUs("pod1c0"))

	s.CreateBurstable(harness.PodSpec{ContCount: 2, CPUReq: "500m", CPULim: "1"})
	harness.RequireDisjoint(t, s.Nodes("pod2c0"), s.Nodes("pod2c1"))
	harness.RequireDisjoint(t, s.CPUs("pod1c0"), s.CPUs("pod2c0"), s.CPUs("pod2c1"))

	s.CreatePod("pod3",
		harness.WithContainers(
			harness.NewContainer("pod3c0", harness.WithRequests("1", "100M"), harness.WithLimits("1", "100M")),
		),
	)
	require.Len(t, s.CPUs("pod3c0"), 1)

	s.DeleteAllPodsIn("default")
	s.Terminate()
}
