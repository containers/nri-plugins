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

// The tests use the variables of the generated tests, which the generation
// build leaves out.
//go:build !e2eish_generate

package topologyaware

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	harness "github.com/containers/nri-plugins/pkg/testutils/e2eish"
)

// e2eishDRAConfig enables DRA and reserves the one CPU of NUMA node 3 which
// the claims below leave unclaimed, as n4c16/test42-dra-devices does.
const e2eishDRAConfig = `config: {dra: {enabled: true}, reservedResources: {cpu: "cpuset:15"}}`

// e2eishClaimTemplate is a ResourceClaimTemplate of one request for cpus CPUs
// of a NUMA node, as test42-dra-devices writes them.
func e2eishClaimTemplate(name string, cpus, node int) string {
	return fmt.Sprintf(`
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: %s
spec:
  spec:
    devices:
      requests:
      - exactly:
          capacity:
            requests:
              dra.cpu/cpu: "%d"
          deviceClassName: %s
          selectors:
          - cel:
              expression: device.attributes["resource.kubernetes.io"].numaNode == %d
        name: cpus
`, name, cpus, draDriverName, node)
}

// e2eishClaimPod is a pod of one container holding the whole claim of its
// own template.
func e2eishClaimPod(s *harness.Script, name string) {
	s.CreatePod(name,
		harness.FromClaimTemplate("cpus", name),
		harness.WithContainers(
			harness.NewContainer(name+"c0", harness.WithClaim("cpus", "")),
		),
	)
}

// A pod with resource claims does not start when the configuration of the
// launch does not enable DRA. The policy publishes its devices whatever the
// configuration says, and the resource manager is what keeps them from a
// cluster with DRA disabled, where a claim of them cannot be allocated at
// all, so the harness has to keep them too.
func TestE2eishDRADisabledAllocatesNothing(t *testing.T) {
	s := harness.NewScript(t, e2eishPlugin)
	s.Launch(genN4c16, genN4c16BaseConfig, `config: {reservedResources: {cpu: "cpuset:15"}}`)
	s.SyncWithRuntimePods(genN4c16Pods...)

	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod0", 3, 3))
	e2eishClaimPod(s, "pod0")

	require.NotContains(t, s.Harness().Running(), "pod0c0",
		"a claim of devices which were never published does not fit")

	// The pod is left waiting, as the scheduler leaves it, and the script
	// reports the pods still waiting at the end of a test. A test which
	// forgot to enable DRA therefore fails with the claims of its pods
	// never fitting, rather than passing on an allocation no cluster of
	// that configuration could make.
	s.DeletePods("default", "pod0")
	s.Terminate()
}

// A pod which was waiting for its claims and is not running after a skipped
// block is forgotten, not started. The resume releases the claims of the pod
// which held the devices, and starting the waiting one on the strength of
// that would give the replay a pod the e2e test had deleted.
func TestE2eishDRAResumeForgetsPendingPodWhichIsGone(t *testing.T) {
	s := harness.NewScript(t, e2eishPlugin)
	s.Launch(genN4c16, genN4c16BaseConfig, e2eishDRAConfig)
	s.SyncWithRuntimePods(genN4c16Pods...)

	// pod0 takes all three unreserved CPUs of NUMA node 3, so pod1 waits.
	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod0", 3, 3))
	e2eishClaimPod(s, "pod0")
	require.Len(t, s.CPUs("pod0c0"), 3, "the CPUs pod0 claimed")
	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod1", 3, 3))
	e2eishClaimPod(s, "pod1")
	require.NotContains(t, s.Harness().Running(), "pod1c0", "pod1 waits for pod0's CPUs")

	// The block deletes both, so neither is running at the resume.
	s.Skip("a block the replay cannot follow, in which both pods are deleted")
	s.ResumeWithRuntimePods(genN4c16Pods...)

	require.NotContains(t, s.Harness().Running(), "pod1c0",
		"a pod which is not running after the block is not started by the resume")
	s.Terminate()
}

// A pod which was waiting for its claims and is running after a skipped
// block is started, with the claims the pod which held the devices gave up.
// The snapshot of the resume is what tells the two cases apart.
func TestE2eishDRAResumeStartsPendingPodWhichRuns(t *testing.T) {
	s := harness.NewScript(t, e2eishPlugin)
	s.Launch(genN4c16, genN4c16BaseConfig, e2eishDRAConfig)
	s.SyncWithRuntimePods(genN4c16Pods...)

	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod0", 3, 3))
	e2eishClaimPod(s, "pod0")
	claimed := s.CPUs("pod0c0")
	require.Len(t, claimed, 3, "the CPUs pod0 claimed")
	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod1", 3, 3))
	e2eishClaimPod(s, "pod1")
	require.NotContains(t, s.Harness().Running(), "pod1c0", "pod1 waits for pod0's CPUs")

	// The block deletes pod0, which the scheduler of a cluster answers by
	// starting pod1, so the snapshot of the resume has pod1 running.
	s.Skip("a block the replay cannot follow, in which pod0 is deleted")
	s.ResumeWithRuntimePods(append(genN4c16Pods,
		harness.NewPod("pod1", harness.WithContainers(harness.NewContainer("pod1c0")),
			harness.WithRunning("pod1c0")))...)

	require.Equal(t, claimed, s.CPUs("pod1c0"), "pod1 holds the devices pod0 gave up")
	s.DeleteAllPodsIn("default")
	s.Terminate()
}

// The claims of a pod which waits are the ones it was created with. A
// cluster creates a ResourceClaim from the template when it admits the pod
// and keeps it, so redefining the template afterwards cannot change what the
// pod is given when its claims finally fit.
func TestE2eishDRAPendingPodKeepsItsClaims(t *testing.T) {
	s := harness.NewScript(t, e2eishPlugin)
	s.Launch(genN4c16, genN4c16BaseConfig, e2eishDRAConfig)
	s.SyncWithRuntimePods(genN4c16Pods...)

	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod0", 3, 3))
	e2eishClaimPod(s, "pod0")
	claimed := s.CPUs("pod0c0")
	require.Len(t, claimed, 3, "the CPUs pod0 claimed")

	// pod1 asks for the same three CPUs of NUMA node 3, so it waits.
	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod1", 3, 3))
	e2eishClaimPod(s, "pod1")
	require.NotContains(t, s.Harness().Running(), "pod1c0", "pod1 waits for pod0's CPUs")

	// The template is redefined for one CPU of node 0. The claim of pod1
	// was created from the first one and is not the template's any more.
	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod1", 1, 0))
	s.DeletePods("default", "pod0")

	require.Equal(t, claimed, s.CPUs("pod1c0"),
		"pod1 holds the request of the template it was created with")
	s.DeleteAllPodsIn("default")
	s.Terminate()
}

// A pod which a resume starts runs only the containers the snapshot reports.
// The pods and containers gone during a skipped block are reconciled before
// such a pod exists, so nothing else would stop the ones which are not
// running.
func TestE2eishDRAResumeStartsOnlyReportedContainers(t *testing.T) {
	s := harness.NewScript(t, e2eishPlugin)
	s.Launch(genN4c16, genN4c16BaseConfig, e2eishDRAConfig)
	s.SyncWithRuntimePods(genN4c16Pods...)

	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod0", 3, 3))
	e2eishClaimPod(s, "pod0")
	require.Len(t, s.CPUs("pod0c0"), 3, "the CPUs pod0 claimed")

	// pod1 holds its claim in two containers, and waits for pod0's CPUs.
	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod1", 3, 3))
	s.CreatePod("pod1",
		harness.FromClaimTemplate("cpus", "pod1"),
		harness.WithContainers(
			harness.NewContainer("pod1c0", harness.WithClaim("cpus", "")),
			harness.NewContainer("pod1c1", harness.WithClaim("cpus", "")),
		),
	)
	require.NotContains(t, s.Harness().Running(), "pod1c0", "pod1 waits for pod0's CPUs")

	// The block deletes pod0, which starts pod1, and one of its containers
	// has stopped by the time of the resume.
	s.Skip("a block the replay cannot follow, in which pod0 is deleted")
	s.ResumeWithRuntimePods(append(genN4c16Pods,
		harness.NewPod("pod1",
			harness.WithContainers(
				harness.NewContainer("pod1c0"),
				harness.NewContainer("pod1c1"),
			),
			harness.WithRunning("pod1c0")))...)

	running := s.Harness().Running()
	require.Contains(t, running, "pod1c0", "the container the snapshot reports")
	require.NotContains(t, running, "pod1c1", "the container the snapshot does not report")
	s.DeleteAllPodsIn("default")
	s.Terminate()
}

// A pod deleted inside a skipped block releases the claims it held, so that
// a pod created after the resume can be allocated the devices of them. The
// pods and containers gone during a skip are deleted by the resume itself,
// which has to unprepare their claims the way deleting a pod does.
//
// The test creates no DeviceClass: the replay has the one the chart creates,
// as a log recorded since the chart took that over has none to create.
func TestE2eishDRAResumeReleasesClaims(t *testing.T) {
	s := harness.NewScript(t, e2eishPlugin)
	s.Launch(genN4c16, genN4c16BaseConfig, e2eishDRAConfig)
	s.SyncWithRuntimePods(genN4c16Pods...)

	// All three unreserved CPUs of NUMA node 3, so that the second pod
	// fits only once the first has given them up.
	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod0", 3, 3))
	e2eishClaimPod(s, "pod0")
	claimed := s.CPUs("pod0c0")
	require.Len(t, claimed, 3, "the CPUs pod0 claimed")

	s.Skip("a block the replay cannot follow, in which pod0 is deleted")
	s.ResumeWithRuntimePods(genN4c16Pods...)

	s.CreateClaimTemplate("default", e2eishClaimTemplate("pod1", 3, 3))
	e2eishClaimPod(s, "pod1")
	require.Len(t, s.CPUs("pod1c0"), 3, "the CPUs pod0 released")
	require.Equal(t, claimed, s.CPUs("pod1c0"),
		"pod1 holds the devices of pod0, which nothing else took")

	s.DeleteAllPodsIn("default")
	s.Terminate()
}
