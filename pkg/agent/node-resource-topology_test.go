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

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	nrt "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	nrtfake "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned/fake"
)

const testNode = "node0"

// newNrtTestAgent returns an agent with a fake NRT client and no worker.
func newNrtTestAgent() (*Agent, *nrtfake.Clientset) {
	cs := nrtfake.NewSimpleClientset()
	a := &Agent{
		nodeName: testNode,
		nrtCli:   cs.TopologyV1alpha2(),
		nrtWake:  make(chan struct{}, 1),
		stopC:    make(chan struct{}),
	}
	return a, cs
}

// startNrtWorker runs the NRT reconciler until the agent is stopped. It
// returns a channel closed when the worker has exited.
func startNrtWorker(a *Agent) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.reconcileLoop("node resource topology CR", a.nrtWake, a.reconcileNrtCR)
	}()
	return done
}

func zones(names ...string) []*policyapi.TopologyZone {
	var out []*policyapi.TopologyZone
	for _, name := range names {
		out = append(out, &policyapi.TopologyZone{Name: name, Type: "Node"})
	}
	return out
}

func getCR(t *testing.T, cs *nrtfake.Clientset) *nrt.NodeResourceTopology {
	cr, err := cs.TopologyV1alpha2().NodeResourceTopologies().Get(context.Background(), testNode, metav1.GetOptions{})
	require.NoError(t, err)
	return cr
}

// waitForZones waits until the CR has the given zones.
func waitForZones(t *testing.T, cs *nrtfake.Clientset, want []*policyapi.TopologyZone) {
	require.Eventually(t, func() bool {
		cr, err := cs.TopologyV1alpha2().NodeResourceTopologies().Get(context.Background(), testNode, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return len(cr.Zones) == len(want) && cr.Zones[0].Name == want[0].Name
	}, 5*time.Second, 5*time.Millisecond)
}

func waitForStop(t *testing.T, a *Agent, done <-chan struct{}) {
	a.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestNrtCRLatestUpdateWins(t *testing.T) {
	a, cs := newNrtTestAgent()
	done := startNrtWorker(a)

	require.NoError(t, a.UpdateNrtCR("policy", zones("old")))
	require.NoError(t, a.UpdateNrtCR("policy", zones("new")))
	waitForZones(t, cs, zones("new"))
	waitForStop(t, a, done)

	cr := getCR(t, cs)
	require.Equal(t, "new", cr.Zones[0].Name)
	require.Equal(t, "policy", cr.Attributes[0].Value)
}

func TestNrtCRBurstCoalesces(t *testing.T) {
	a, cs := newNrtTestAgent()

	for i := 0; i < 5; i++ {
		require.NoError(t, a.UpdateNrtCR("policy", zones("zone")))
	}
	require.Len(t, a.nrtWake, 1, "a burst leaves one pending wakeup")

	require.NoError(t, a.reconcileNrtCR())
	writes := 0
	for _, action := range cs.Actions() {
		if action.GetVerb() == "create" || action.GetVerb() == "update" {
			writes++
		}
	}
	require.Equal(t, 1, writes, "one write for the whole burst")
}

func TestNrtCRUnchangedIsNotWritten(t *testing.T) {
	a, cs := newNrtTestAgent()

	zone := &policyapi.TopologyZone{
		Name:      "zone",
		Type:      "Node",
		Resources: []*policyapi.ZoneResource{{Name: "cpu", Capacity: resource.MustParse("4")}},
	}
	require.NoError(t, a.UpdateNrtCR("policy", []*policyapi.TopologyZone{zone}))
	require.NoError(t, a.reconcileNrtCR())
	require.NoError(t, a.reconcileNrtCR())

	writes := 0
	for _, action := range cs.Actions() {
		if action.GetVerb() == "create" || action.GetVerb() == "update" {
			writes++
		}
	}
	require.Equal(t, 1, writes, "a resync of an unchanged CR writes nothing")
}

func TestNrtCRResyncHealsDrift(t *testing.T) {
	resyncInterval = 10 * time.Millisecond
	t.Cleanup(func() { resyncInterval = 30 * time.Second })

	a, cs := newNrtTestAgent()
	done := startNrtWorker(a)

	require.NoError(t, a.UpdateNrtCR("policy", zones("ours")))
	waitForZones(t, cs, zones("ours"))

	cr := getCR(t, cs)
	cr.Zones[0].Name = "theirs"
	_, err := cs.TopologyV1alpha2().NodeResourceTopologies().Update(context.Background(), cr, metav1.UpdateOptions{})
	require.NoError(t, err)

	waitForZones(t, cs, zones("ours"))
	waitForStop(t, a, done)
}

func TestNrtCRWithoutClient(t *testing.T) {
	a, _ := newNrtTestAgent()
	a.nrtCli = nil

	require.NoError(t, a.UpdateNrtCR("policy", zones("zone")))
	require.NoError(t, a.reconcileNrtCR())
}
