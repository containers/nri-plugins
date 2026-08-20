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
	"testing"

	"github.com/containers/nri-plugins/pkg/kubernetes/client"
)

// The Agent public getters are consumed by future DRA-driver wiring (see
// docs/plans/20260820-dra-step1-kubernetes-client-watch-lift.md Task 7).
// These tests cover both the "before setupClients" (zero-value) path and
// the "after successful setupClients" path using a real client.Client
// constructed from the fixture kubeconfig — the fixture points at
// example.com and no request is actually made, so no cluster is needed.

const fixtureKubeconfig = "../kubernetes/client/testdata/kubeconfig-example.yaml"

// TestAgent_Getters_ZeroValues verifies that the four getters return
// their zero values on a freshly-constructed Agent (before setupClients).
// NodeName follows a.nodeName which we set directly to exercise it.
func TestAgent_Getters_ZeroValues(t *testing.T) {
	a := &Agent{
		nodeName:   "test-node",
		kubeConfig: "/path/to/kubeconfig",
	}

	if got := a.NodeName(); got != "test-node" {
		t.Errorf("NodeName() = %q, want %q", got, "test-node")
	}
	if got := a.KubeConfig(); got != "/path/to/kubeconfig" {
		t.Errorf("KubeConfig() = %q, want %q", got, "/path/to/kubeconfig")
	}
	if got := a.KubeClient(); got != nil {
		t.Errorf("KubeClient() before setupClients = %+v, want nil", got)
	}
	if got := a.RestConfig(); got != nil {
		t.Errorf("RestConfig() before setupClients = %+v, want nil", got)
	}
}

// TestAgent_Getters_AfterClientSet verifies that KubeClient() and
// RestConfig() return non-nil values once the client has been
// constructed. Uses a client built directly from the fixture; this
// avoids depending on the full setupClients machinery (which is
// deferred to Task 10's e2e test).
func TestAgent_Getters_AfterClientSet(t *testing.T) {
	c, err := client.New(client.WithKubeConfig(fixtureKubeconfig))
	if err != nil {
		t.Fatalf("client.New failed: %v", err)
	}
	a := &Agent{
		nodeName:   "test-node",
		kubeConfig: fixtureKubeconfig,
		k8sCli:     c,
	}

	if got := a.KubeClient(); got == nil {
		t.Fatal("KubeClient() after client set = nil, want non-nil")
	}
	if got := a.KubeClient(); got != c {
		t.Errorf("KubeClient() = %p, want %p", got, c)
	}
	if got := a.RestConfig(); got == nil {
		t.Error("RestConfig() after client set = nil, want non-nil")
	} else if got.Host == "" {
		t.Error("RestConfig().Host is empty, want fixture value")
	}
}
