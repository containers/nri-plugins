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

package resmgr

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	resourceapi "k8s.io/api/resource/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	specs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/pkg/agent"
	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
)

// what an explicitly set dra.enabled points to
var (
	draOn  = true
	draOff = false
)

// The methods Synchronize needs of a policy. AllocateClaim fails on purpose:
// a claim reaching it is what says claims are being served, and failing there
// keeps the test out of the NRI container updates a real allocation makes.
func (testPolicy) Sync([]cache.Container, []cache.Container) error { return nil }
func (testPolicy) ActivePolicy() string                            { return "test-policy" }
func (testPolicy) GetTopologyZones() []*policy.TopologyZone        { return nil }
func (testPolicy) GetExtendedResources() map[string]*apiresource.Quantity {
	return nil
}
func (testPolicy) AllocateClaim(*resourceapi.ResourceClaim, []resourceapi.DeviceRequestAllocationResult) ([]specs.ContainerEdits, error) {
	return nil, errAllocate
}

var errAllocate = errors.New("test policy allocates nothing")

// newTestCache returns a cache in a directory of its own. The cache refuses a
// directory others may write to, which is what a temporary directory is.
func newTestCache(t *testing.T) cache.Cache {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("failed to create cache directory: %v", err)
	}

	c, err := cache.NewCache(cache.Options{CacheDir: dir})
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	return c
}

// newTestAgent returns an agent with the given node name and no kubernetes
// client, which is what an agent looks like before it is started.
func newTestAgent(t *testing.T, nodeName string) *agent.Agent {
	t.Helper()

	t.Setenv("NODE_NAME", nodeName)

	a, err := agent.New(agent.TopologyAwareConfigInterface())
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	return a
}

// TestSetupDRA verifies that DRA is left off, without failing startup, when it
// is disabled or when we lack what the plugin needs.
func TestSetupDRA(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   cfgapi.DRAConfig
		agent func(*testing.T) *agent.Agent
	}{
		{
			name: "unset",
			cfg:  cfgapi.DRAConfig{},
		},
		{
			name: "disabled",
			cfg:  cfgapi.DRAConfig{Enabled: &draOff},
		},
		{
			name:  "enabled without a node name",
			cfg:   cfgapi.DRAConfig{Enabled: &draOn},
			agent: func(*testing.T) *agent.Agent { return &agent.Agent{} },
		},
		{
			name: "enabled without a kubernetes client",
			cfg:  cfgapi.DRAConfig{Enabled: &draOn},
			agent: func(t *testing.T) *agent.Agent {
				return newTestAgent(t, "test-node")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &resmgr{}
			if tc.agent != nil {
				m.agent = tc.agent(t)
			}

			if err := m.setupDRA(&tc.cfg); err != nil {
				t.Fatalf("setupDRA() failed: %v", err)
			}
			if m.dra != nil {
				t.Error("setupDRA() created a plugin, expected none")
			}

			// Starting, publishing and stopping must be no-ops without a
			// plugin: resmgr and the policy run through these paths whether
			// DRA came up or not.
			if err := m.startDRA(); err != nil {
				t.Errorf("startDRA() without a plugin failed: %v", err)
			}
			if err := m.PublishDRADevices([]resourceapi.Device{{Name: "cpu-0"}}); err != nil {
				t.Errorf("PublishDRADevices() without a plugin failed: %v", err)
			}
			m.dra.Stop()
		})
	}
}

// TestReconfigureDRA verifies that turning DRA on or off is refused in a
// running plugin, and that any other configuration change is let through. Unset
// and false are the same switch, so swapping one for the other is no change.
func TestReconfigureDRA(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running *bool
		updated *bool
		fail    bool
	}{
		{name: "off, still off", running: &draOff, updated: &draOff},
		{name: "on, still on", running: &draOn, updated: &draOn},
		{name: "unset, still unset"},
		{name: "unset, set to off", updated: &draOff},
		{name: "off, unset", running: &draOff},
		{name: "off, turned on", running: &draOff, updated: &draOn, fail: true},
		{name: "on, turned off", running: &draOn, updated: &draOff, fail: true},
		{name: "unset, turned on", updated: &draOn, fail: true},
		{name: "on, unset", running: &draOn, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &cfgapi.TopologyAwarePolicy{}
			cfg.Spec.DRA.Enabled = tc.running
			m := &resmgr{cfg: cfg}

			err := m.reconfigureDRA(&cfgapi.DRAConfig{Enabled: tc.updated})

			switch {
			case tc.fail && err == nil:
				t.Error("reconfigureDRA() accepted the change, expected an error")
			case !tc.fail && err != nil:
				t.Errorf("reconfigureDRA() failed: %v", err)
			}
		})
	}
}

// TestStopStopsDRABeforeTakingTheLock verifies the shutdown order. Stopping the
// plugin waits for the kubelet requests in flight, and every such request takes
// resmgr's lock, so the plugin has to be stopped before the lock is taken.
func TestStopStopsDRABeforeTakingTheLock(t *testing.T) {
	m := &resmgr{}

	pluginDir := t.TempDir()
	plugin, err := dra.New("test.nri.io", dra.Options{
		NodeName:      "test-node",
		KubeClient:    fake.NewClientset(),
		Owner:         m,
		Policy:        testPolicy{},
		RegistrarDir:  t.TempDir(),
		PluginDataDir: pluginDir,
	})
	if err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}
	if err := plugin.Start(t.Context()); err != nil {
		t.Fatalf("failed to start plugin: %v", err)
	}
	m.dra = plugin

	// The plugin's socket goes away when it stops, which is how we see that it
	// has stopped from here.
	socket := filepath.Join(pluginDir, "dra.sock")
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("failed to stat plugin socket: %v", err)
	}

	// Stand in for a kubelet request being served.
	m.Lock()

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()

	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(socket); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			m.Unlock()
			t.Fatal("the plugin was not stopped while the lock was held")
		}
		time.Sleep(10 * time.Millisecond)
	}

	m.Unlock()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return")
	}
}

// TestClaimCommitFailedUpdates verifies that a container update the runtime did
// not apply fails the allocation of a claim, but not its release: the container
// may still be using what the claim is being given, while released resources
// only sit idle until the update gets through.
func TestClaimCommitFailedUpdates(t *testing.T) {
	for _, tc := range []struct {
		name string
		stub func(ctr cache.Container) *testStub
	}{
		{
			name: "update rejected",
			stub: func(ctr cache.Container) *testStub {
				return &testStub{failed: []*api.ContainerUpdate{{ContainerId: ctr.GetID()}}}
			},
		},
		{
			name: "update request failed",
			stub: func(cache.Container) *testStub {
				return &testStub{err: errors.New("update failed")}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ctr := newTestUpdate(t, &testStub{})
			m.nri.stub = tc.stub(ctr)

			if err := m.ClaimAllocated(); err == nil {
				t.Error("ClaimAllocated() succeeded with a container left un-updated")
			}
			if err := m.ClaimReleased(); err != nil {
				t.Errorf("ClaimReleased() failed: %v", err)
			}
			if ctr.PeekPendingUpdate() == nil {
				t.Error("the update the runtime did not apply is no longer pending")
			}
		})
	}
}

// TestClaimCommitPushesOnlyPendingUpdates verifies that committing a claim
// change which left nothing to update does not push anything to the runtime.
// Unpreparing a claim the policy has no record of is such a change, and failing
// it on a push with nothing in it would hold up the pod it belongs to.
func TestClaimCommitPushesOnlyPendingUpdates(t *testing.T) {
	m := &resmgr{
		cache:  newTestCache(t),
		policy: testPolicy{},
		cfg:    &cfgapi.TopologyAwarePolicy{},
	}
	stub := &testStub{}
	m.nri = &nriPlugin{resmgr: m, stub: stub}

	if err := m.ClaimReleased(); err != nil {
		t.Fatalf("ClaimReleased() failed: %v", err)
	}
	if len(stub.sent) != 0 {
		t.Errorf("pushed %d container update(s) for a claim which changed nothing", len(stub.sent))
	}

	// A claim the policy did take resources from a container for is pushed.
	m.cache.InsertPod(&api.PodSandbox{Id: "pod-0", Name: "pod-0"}, nil)
	c, err := m.cache.InsertContainer(&api.Container{
		Id:           "ctr-0",
		Name:         "ctr-0",
		PodSandboxId: "pod-0",
	})
	if err != nil {
		t.Fatalf("failed to insert container: %v", err)
	}
	c.SetCpusetCpus("0")

	if err := m.ClaimAllocated(); err != nil {
		t.Fatalf("ClaimAllocated() failed: %v", err)
	}
	if len(stub.sent) != 1 {
		t.Errorf("pushed %d container update(s) for a claim which updated a container, expected 1",
			len(stub.sent))
	}
}

// TestSynchronizeAllowsDRAClaims verifies that Synchronize is what lets the DRA
// plugin serve claims. The driver is registered when we start, but the policy
// only learns about the containers already running from Synchronize: a claim
// served before that would be allocated against a node which looks empty.
func TestSynchronizeAllowsDRAClaims(t *testing.T) {
	const driverName = "test.nri.io"

	m := &resmgr{
		agent:  newTestAgent(t, "test-node"),
		cache:  newTestCache(t),
		policy: testPolicy{},
	}
	m.nri = &nriPlugin{resmgr: m}

	plugin, err := dra.New(driverName, dra.Options{
		NodeName:      "test-node",
		KubeClient:    fake.NewClientset(),
		Owner:         m,
		Policy:        testPolicy{},
		RegistrarDir:  t.TempDir(),
		PluginDataDir: t.TempDir(),
		CDIDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("failed to create plugin: %v", err)
	}

	claims := []*resourceapi.ResourceClaim{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "claim", UID: "test-uid"},
			Status: resourceapi.ResourceClaimStatus{
				Allocation: &resourceapi.AllocationResult{
					Devices: resourceapi.DeviceAllocationResult{
						Results: []resourceapi.DeviceRequestAllocationResult{
							{Request: "request-0", Driver: driverName, Pool: "test-node", Device: "cpu-0"},
						},
					},
				},
			},
		},
	}

	// Registered, not synchronized: the request is refused as a whole, which is
	// what makes the kubelet retry it.
	m.dra = plugin
	if _, err := plugin.PrepareResourceClaims(t.Context(), claims); err == nil {
		t.Errorf("claims were served before Synchronize")
	}

	// DRA disabled must survive the same call, with no plugin to allow.
	m.dra = nil
	if _, err := m.nri.Synchronize(t.Context(), nil, nil); err != nil {
		t.Fatalf("Synchronize() failed: %v", err)
	}

	m.dra = plugin
	if _, err := m.nri.Synchronize(t.Context(), nil, nil); err != nil {
		t.Fatalf("Synchronize() failed: %v", err)
	}

	// Served now: the claim reaches the policy, which fails it on purpose.
	result, err := plugin.PrepareResourceClaims(t.Context(), claims)
	if err != nil {
		t.Fatalf("claims were still refused after Synchronize: %v", err)
	}
	if prepared := result["test-uid"]; !errors.Is(prepared.Err, errAllocate) {
		t.Errorf("claim failed with %v, expected it to reach the policy and wrap %v",
			prepared.Err, errAllocate)
	}
}
