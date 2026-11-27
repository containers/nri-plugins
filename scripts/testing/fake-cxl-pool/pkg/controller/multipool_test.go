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

package controller

// Tests of a controller in a cluster with several pools: the pool name
// from the server, the node selector of the nodes the server reaches.
//
// Two controllers of one driver keep off each other's pools by
// resourceslice Options.ReconcilePoolWithName. The fake clientset ignores
// field selectors, so a test with two controllers on one fake clientset
// cannot show that; TestRunPoolName checks that the slice informer of the
// library asks for the pool of the controller only.

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/client"
)

// selectorString returns a node selector as "key op [values]" per
// requirement, "<nil>" for nil.
func selectorString(ns *corev1.NodeSelector) string {
	if ns == nil {
		return "<nil>"
	}
	var terms []string
	for _, t := range ns.NodeSelectorTerms {
		var rs []string
		for _, r := range t.MatchExpressions {
			rs = append(rs, fmt.Sprintf("%s %s %v", r.Key, r.Operator, r.Values))
		}
		for _, r := range t.MatchFields {
			rs = append(rs, fmt.Sprintf("field %s %s %v", r.Key, r.Operator, r.Values))
		}
		terms = append(terms, strings.Join(rs, ", "))
	}
	return strings.Join(terms, " | ")
}

// waitSelector waits until the one pool slice has the node selector want.
func (e *env) waitSelector(want string) {
	e.t.Helper()
	e.eventually("node selector "+want, func() bool {
		l := e.slices()
		return len(l) == 1 && selectorString(l[0].Spec.NodeSelector) == want && l[0].Spec.AllNodes == nil
	})
}

func (e *env) setHosts(hosts []api.Host) {
	e.pool.mu.Lock()
	defer e.pool.mu.Unlock()
	e.pool.hosts = hosts
}

func TestPublishNodeSelector(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sync()
	e.waitSelector("kubernetes.io/hostname In [h-node1 h-node2]")

	// a host of node3 appears: the same devices for one node more
	hosts := append(newFakePool().hosts, api.Host{Name: "node3", State: api.HostRunning})
	e.setHosts(hosts)
	e.sync()
	e.waitSelector("kubernetes.io/hostname In [h-node1 h-node2 h-node3]")

	// a node without a hostname label, with a host of its name, is not
	// a pool host: the node selector cannot select it
	n := node("vm5", "")
	n.Labels = nil
	if _, err := e.kube.CoreV1().Nodes().Create(e.ctx, n, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.eventually("lister: node vm5", func() bool { _, err := e.c.nodes.Get("vm5"); return err == nil })
	e.setHosts(append(hosts, api.Host{Name: "vm5", State: api.HostRunning}))
	e.sync()
	e.sync()
	e.waitSelector("kubernetes.io/hostname In [h-node1 h-node2 h-node3]")
	if c := strings.Count(e.logs.String(), "node vm5 has no kubernetes.io/hostname label: not a pool host"); c != 1 {
		t.Fatalf("label warning logged %d times", c)
	}

	// no node of the cluster is a host: no slice
	e.setHosts(nil)
	e.sync()
	e.sync()
	e.eventually("no ResourceSlice", func() bool { return len(e.slices()) == 0 })
	if c := strings.Count(e.logs.String(), "pool fake-cxl-pool: no node of the cluster is a host of the server, publishing nothing"); c != 1 {
		t.Fatalf("no-node message logged %d times", c)
	}

	// hosts again: published again
	e.setHosts(newFakePool().hosts)
	e.sync()
	e.waitSelector("kubernetes.io/hostname In [h-node1 h-node2]")
	if !strings.Contains(e.logs.String(), "publishing pool fake-cxl-pool for nodes [h-node1 h-node2]: 3 devices [pooled0 shared0 weird-name]") {
		t.Fatalf("no publishing message in the log")
	}
}

func TestPoolNameFromServer(t *testing.T) {
	t.Parallel()
	e := newEnvWith(t, func(p *fakePool, _ *Config) { p.name = "pool-x" })
	if e.c.cfg.DRAPoolName != "pool-x" {
		t.Fatalf("pool name %q", e.c.cfg.DRAPoolName)
	}
	e.sync()
	e.eventually("slice of pool-x", func() bool {
		l := e.slices()
		return len(l) == 1 && l[0].Spec.Pool.Name == "pool-x"
	})
	// the configured name wins, the server is not asked
	e = newEnvWith(t, func(p *fakePool, c *Config) { p.name = "pool-x"; c.DRAPoolName = "flag-pool" })
	if e.c.cfg.DRAPoolName != "flag-pool" || e.pool.statuses != 0 {
		t.Fatalf("pool name %q, %d status requests", e.c.cfg.DRAPoolName, e.pool.statuses)
	}
}

// Run stops without an error when it is cancelled while it starts.
func TestRunCancelledAtStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New(Config{DRAPoolName: "pool-x", Logger: log.New(testWriter{t}, "", 0)}, fake.NewClientset(), client.New("http://127.0.0.1:1"), nil)
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run cancelled at start: %v", err)
	}
}

func TestRunPoolName(t *testing.T) {
	t.Parallel()
	e := newEnvWith(t, func(p *fakePool, _ *Config) { p.name = "pool-x" })
	// a controller of its own: Run starts its informers
	logs := &lockedBuf{}
	c := New(Config{Logger: log.New(io.MultiWriter(testWriter{t}, logs), "run: ", 0)}, e.kube, client.New(e.c.pool.BaseURL), nil)
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	e.eventually("slice of pool-x", func() bool {
		l := e.slices()
		return len(l) == 1 && l[0].Spec.Pool.Name == "pool-x"
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if want := "driver cxl-pool.nri.io, pool pool-x (from the server), server " + e.c.pool.BaseURL + ", sync interval 10s"; !strings.Contains(logs.String(), want) {
		t.Fatalf("no %q in the log", want)
	}
	if l := e.slices(); len(l) != 0 {
		t.Fatalf("slices left after Run: %+v", l)
	}
	// the slice informer of the library lists the slices of pool-x only
	selectors := []string{}
	for _, a := range e.kube.Actions() {
		if l, ok := a.(k8stesting.ListAction); ok && a.GetResource().Resource == "resourceslices" {
			selectors = append(selectors, l.GetListRestrictions().Fields.String())
		}
	}
	found := false
	for _, s := range selectors {
		found = found || strings.Contains(s, "spec.pool.name=pool-x")
	}
	if !found {
		t.Fatalf("no ResourceSlice list of pool pool-x: %q", selectors)
	}
}
