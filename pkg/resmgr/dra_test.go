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
	"os"
	"path/filepath"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/containers/nri-plugins/pkg/agent"
	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
)

// what an explicitly set dra.enabled points to
var (
	draOn  = true
	draOff = false
)

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
