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

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/intel/goresctrl/pkg/monitor"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	log = logrus.StandardLogger()
	log.SetLevel(logrus.TraceLevel)
}

func newTestPlugin(resctrlPath string) *plugin {
	cfg := &pluginConfig{
		ResctrlPath: resctrlPath,
	}
	mgr, err := monitor.New(monitor.Options{
		ResctrlRoot:      resctrlPath,
		KeyValidator:     monitor.PodUIDValidator,
		KeyCanonicalizer: monitor.CanonicalizePodUID,
	})
	if err != nil {
		panic(err)
	}
	return &plugin{
		config: cfg,
		mgr:    mgr,
	}
}

func makePod(uid, namespace, name string) *api.PodSandbox {
	return &api.PodSandbox{
		Id:        "sandbox-" + uid, // CRI sandbox ID != K8s pod UID
		Uid:       uid,
		Namespace: namespace,
		Name:      name,
		Labels:    map[string]string{},
	}
}

func makeContainer(id, name, podSandboxID string, pid uint32, rdtClass string) *api.Container {
	ctr := &api.Container{
		Id:           id,
		PodSandboxId: podSandboxID,
		Name:         name,
		Pid:          pid,
		Linux: &api.LinuxContainer{
			Resources: &api.LinuxResources{},
		},
	}
	if rdtClass != "" {
		ctr.Linux.Resources.RdtClass = &api.OptionalString{Value: rdtClass}
	}
	return ctr
}

func TestShouldMonitorPod_NoFilters(t *testing.T) {
	p := newTestPlugin("/tmp/resctrl-test")
	pod := makePod("uid-1", "default", "test-pod")
	assert.True(t, p.shouldMonitorPod(pod))
}

func TestShouldMonitorPod_NamespaceFilter(t *testing.T) {
	p := newTestPlugin("/tmp/resctrl-test")
	p.config.Namespaces = []string{"production", "staging"}

	pod1 := makePod("uid-1", "production", "pod1")
	assert.True(t, p.shouldMonitorPod(pod1))

	pod2 := makePod("uid-2", "kube-system", "pod2")
	assert.False(t, p.shouldMonitorPod(pod2))
}

func TestShouldMonitorPod_LabelFilter(t *testing.T) {
	p := newTestPlugin("/tmp/resctrl-test")
	p.config.LabelSelector = map[string]string{"monitor": "true"}

	pod1 := makePod("uid-1", "default", "pod1")
	pod1.Labels = map[string]string{"monitor": "true", "app": "web"}
	assert.True(t, p.shouldMonitorPod(pod1))

	pod2 := makePod("uid-2", "default", "pod2")
	pod2.Labels = map[string]string{"app": "web"}
	assert.False(t, p.shouldMonitorPod(pod2))
}

func TestGetRDTClass(t *testing.T) {
	ctr1 := makeContainer("c1", "container1", "uid-1", 1234, "BestEffort")
	assert.Equal(t, "BestEffort", getRDTClass(ctr1))

	ctr2 := makeContainer("c2", "container2", "uid-1", 1235, "")
	assert.Equal(t, "", getRDTClass(ctr2))

	ctr3 := &api.Container{
		Id:   "c3",
		Name: "container3",
	}
	assert.Equal(t, "", getRDTClass(ctr3))
}

func TestPprintCtr(t *testing.T) {
	pod := makePod("uid-1", "default", "my-pod")
	ctr := makeContainer("c1", "my-container", "uid-1", 1234, "")
	assert.Equal(t, "default/my-pod:my-container", pprintCtr(pod, ctr))
}

func TestPostCreateContainer_FilteredPod(t *testing.T) {
	p := newTestPlugin(t.TempDir())
	p.config.Namespaces = []string{"production"}

	pod := makePod("uid-1", "default", "test-pod")
	ctr := makeContainer("c1", "container1", "uid-1", 1234, "")

	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	// Pod should not be tracked since it's not in the production namespace.
	assert.Equal(t, 0, len(p.mgr.List()))
}

func TestPostCreateContainer_CreatesMonGroup(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)

	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	pod := makePod(podUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", podUID, 0, "")

	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	// Pod should be tracked.
	assert.Equal(t, 1, len(p.mgr.List()))

	// Mon_group directory should exist, keyed by bare pod UID.
	monDir := filepath.Join(tmpDir, "mon_groups", podUID)
	_, err = os.Stat(monDir)
	assert.NoError(t, err)
}

func TestPostCreateContainer_WithRDTClass(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "BestEffort"), 0755))

	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	pod := makePod(podUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", podUID, 0, "BestEffort")

	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	// Mon_group should be under the ctrl_group.
	monDir := filepath.Join(tmpDir, "BestEffort", "mon_groups", podUID)
	_, err = os.Stat(monDir)
	assert.NoError(t, err)
}

func TestMultiContainerPod(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	pod := makePod(podUID, "default", "multi-pod")
	ctr1 := makeContainer("c1", "container1", podUID, 0, "")
	ctr2 := makeContainer("c2", "container2", podUID, 0, "")

	// First container creates the mon_group.
	err := p.PostCreateContainer(context.Background(), pod, ctr1)
	require.NoError(t, err)
	assert.Equal(t, 1, len(p.mgr.List()))

	// Second container reuses the same mon_group.
	err = p.PostCreateContainer(context.Background(), pod, ctr2)
	require.NoError(t, err)
	assert.Equal(t, 1, len(p.mgr.List())) // still one pod

	// RemovePodSandbox is what actually removes the mon_group; container
	// stops do not affect it (the plugin no longer handles StopContainer).
	err = p.RemovePodSandbox(context.Background(), pod)
	require.NoError(t, err)
	assert.Equal(t, 0, len(p.mgr.List()))
}

func TestSetConfig(t *testing.T) {
	p := newTestPlugin("/tmp/resctrl-test")

	configYAML := []byte(`
resctrlPath: /tmp/test-resctrl
namespaces:
  - production
  - staging
labelSelector:
  monitor: "true"
`)

	err := p.setConfig(configYAML)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/test-resctrl", p.config.ResctrlPath)
	assert.Equal(t, []string{"production", "staging"}, p.config.Namespaces)
	assert.Equal(t, map[string]string{"monitor": "true"}, p.config.LabelSelector)
}

func TestSetConfig_InvalidYAML(t *testing.T) {
	p := newTestPlugin("/tmp/resctrl-test")

	err := p.setConfig([]byte(":::invalid yaml"))
	assert.Error(t, err)
}

func TestSetConfig_RelativePath(t *testing.T) {
	p := newTestPlugin("/tmp/resctrl-test")

	err := p.setConfig([]byte("resctrlPath: relative/path"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "absolute path")
}

func TestSynchronize_UsesUIDNotSandboxID(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	pod := makePod(podUID, "default", "sync-pod")
	// Container references the pod by sandbox ID, not by UID.
	ctr := makeContainer("c1", "container1", pod.GetId(), 0, "")

	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr})
	require.NoError(t, err)

	// The mon_group should be keyed by the K8s pod UID, not the sandbox ID.
	tracked := p.mgr.List()
	assert.Equal(t, 1, len(tracked))
	assert.Contains(t, tracked, podUID)

	// Mon_group directory should exist.
	monDir := filepath.Join(tmpDir, "mon_groups", podUID)
	_, err = os.Stat(monDir)
	assert.NoError(t, err)
}

func TestSynchronize_RemovesOrphanMonGroup(t *testing.T) {
	tmpDir := t.TempDir()

	// An orphaned mon_group left behind by a previous run, keyed by a
	// UUID-shaped pod UID that is no longer live.
	orphanUID := "deadbeef-0000-4000-8000-000000000000"
	orphanDir := filepath.Join(tmpDir, "mon_groups", orphanUID)
	require.NoError(t, os.MkdirAll(orphanDir, 0755))

	p := newTestPlugin(tmpDir)

	// Synchronize with a single live pod that is not the orphan.
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	pod := makePod(podUID, "default", "live-pod")
	ctr := makeContainer("c1", "container1", pod.GetId(), 0, "")

	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr})
	require.NoError(t, err)

	// The live pod's mon_group exists...
	_, err = os.Stat(filepath.Join(tmpDir, "mon_groups", podUID))
	assert.NoError(t, err)

	// ...and the orphan was reaped via Reconcile.
	_, err = os.Stat(orphanDir)
	assert.True(t, os.IsNotExist(err), "orphan mon_group should have been removed by Reconcile")
}

func TestReconcile_RetriesPendingRemoval(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	// A tracked group whose earlier Remove is assumed to have failed.
	_, err := p.mgr.EnsureGroup(podUID, "")
	require.NoError(t, err)
	require.Contains(t, p.mgr.List(), podUID)
	p.markPendingRemoval(podUID)

	// The reconciler must retry Remove (not merely Reconcile, which preserves
	// tracked keys) and clear the pending entry on success.
	p.reconcile(p.mgr)

	assert.NotContains(t, p.mgr.List(), podUID)
	assert.Empty(t, p.pendingRemovalKeys())
	_, err = os.Stat(filepath.Join(tmpDir, "mon_groups", podUID))
	assert.True(t, os.IsNotExist(err), "pending mon_group should have been removed on retry")
}

func TestPostCreateContainer_InvalidUID(t *testing.T) {
	p := newTestPlugin(t.TempDir())

	// Invalid UID (not a UUID) — EnsureGroup fails due to PodUIDValidator.
	pod := makePod("not-a-uuid", "default", "bad-pod")
	ctr := makeContainer("c1", "container1", "not-a-uuid", 0, "")

	err := p.PostCreateContainer(context.Background(), pod, ctr)
	// Non-fatal: returns nil but does not track.
	require.NoError(t, err)
	assert.Equal(t, 0, len(p.mgr.List()))
}

func TestStartContainer_AssignsPID(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	pod := makePod(podUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", podUID, 0, "")

	// Create the mon_group via PostCreateContainer.
	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	monDir := filepath.Join(tmpDir, "mon_groups", podUID)
	require.DirExists(t, monDir)

	// Simulate the kernel creating the tasks file.
	require.NoError(t, os.WriteFile(filepath.Join(monDir, "tasks"), nil, 0644))

	// StartContainer with a valid PID should write it to tasks.
	ctrWithPid := makeContainer("c1", "container1", podUID, 42, "")
	err = p.StartContainer(context.Background(), pod, ctrWithPid)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(monDir, "tasks"))
	require.NoError(t, err)
	assert.Equal(t, "42\n", string(data))
}

func TestStartContainer_PIDZero_FallbackToPostStart(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	pod := makePod(podUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", podUID, 0, "")

	// Create the mon_group.
	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	monDir := filepath.Join(tmpDir, "mon_groups", podUID)
	require.NoError(t, os.WriteFile(filepath.Join(monDir, "tasks"), nil, 0644))

	// StartContainer with PID 0 should not fail (just warns).
	err = p.StartContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	// PostStartContainer with a valid PID should write it.
	ctrWithPid := makeContainer("c1", "container1", podUID, 99, "")
	err = p.PostStartContainer(context.Background(), pod, ctrWithPid)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(monDir, "tasks"))
	require.NoError(t, err)
	assert.Equal(t, "99\n", string(data))
}

func TestStartContainer_FilteredPod(t *testing.T) {
	p := newTestPlugin(t.TempDir())
	p.config.Namespaces = []string{"production"}

	pod := makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "default", "test-pod")
	ctr := makeContainer("c1", "container1", "a1b2c3d4-e5f6-7890-abcd-ef1234567890", 42, "")

	// Should not error even though pod is filtered.
	err := p.StartContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
}

func TestRemovePodSandbox_RetainsGroupOnRmdirFailure(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	pod := makePod(podUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", podUID, 0, "")

	// Create the mon_group.
	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Equal(t, 1, len(p.mgr.List()))

	monDir := filepath.Join(tmpDir, "mon_groups", podUID)
	require.DirExists(t, monDir)

	// Put a file inside the mon_group dir so os.Remove (rmdir) would fail.
	require.NoError(t, os.WriteFile(filepath.Join(monDir, "tasks"), nil, 0644))

	// RemovePodSandbox attempts removal; with a non-empty dir rmdir fails,
	// so the entry remains in the manager (reconciler will retry later).
	err = p.RemovePodSandbox(context.Background(), pod)
	require.NoError(t, err) // handler does not propagate the rmdir error
	assert.Equal(t, 1, len(p.mgr.List()), "entry retained when rmdir fails; reconciler will clean")
}

func TestCheckRuntimeVersion(t *testing.T) {
	tests := []struct {
		name    string
		runtime string
		version string
		wantErr bool
	}{
		{"containerd any version", "containerd", "2.0.0", false},
		{"cri-o 1.36.0", "cri-o", "1.36.0", false},
		{"cri-o 1.37.0", "cri-o", "1.37.0", false},
		{"cri-o 2.0.0", "cri-o", "2.0.0", false},
		{"cri-o 1.35.0 rejected", "cri-o", "1.35.0", true},
		{"cri-o 1.35.2 rejected", "cri-o", "1.35.2", true},
		{"cri-o 1.31.5 rejected", "cri-o", "1.31.5", true},
		{"cri-o 0.99.0 rejected", "cri-o", "0.99.0", true},
		{"CRI-O case insensitive", "CRI-O", "1.35.0", true},
		{"cri-o no patch", "cri-o", "1.36", false},
		{"cri-o unparsable", "cri-o", "latest", true},
		{"cri-o v prefix accepted", "cri-o", "v1.36.0", false},
		{"cri-o v prefix rejected", "cri-o", "v1.35.0", true},
		{"cri-o strips pre-release suffix", "cri-o", "1.36.0-rc1", false},
		{"cri-o strips pre-release old version", "cri-o", "1.35.0-beta.1", true},
		{"cri-o strips build metadata", "cri-o", "1.36.0+build123", false},
		{"cri-o strips v prefix and pre-release", "cri-o", "v1.35.0-alpha.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRuntimeVersion(tt.runtime, tt.version)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestSetConfig_ReloadTearsDownTelemetry verifies that a dynamic setConfig
// reload, after telemetry has started, unregisters the OTel instruments bound
// to the old manager and starts fresh telemetry against the new manager,
// rather than leaking the old registration.
func TestSetConfig_ReloadTearsDownTelemetry(t *testing.T) {
	groups := map[string]map[string]map[string]string{
		"11111111-1111-1111-1111-111111111111": {
			"mon_L3_00": {"llc_occupancy": "4096"},
		},
	}
	root1 := setupTestResctrl(t, groups)
	root2 := setupTestResctrl(t, groups)

	p := newTestPlugin(root1)
	// Disable Prometheus so telemetry starts without binding a port.
	p.config.Telemetry = defaultTelemetryConfig()
	p.config.Telemetry.Prometheus.Enabled = false

	require.NoError(t, p.startTelemetry(context.Background()))
	oldReg := p.metrics
	oldTelem := p.telemetry
	require.NotNil(t, oldReg)
	require.NotNil(t, oldTelem)

	// Dynamic reconfiguration to a new resctrl root, telemetry still port-less.
	data := []byte("resctrlPath: " + root2 + "\ntelemetry:\n  prometheus:\n    enabled: false\n")
	require.NoError(t, p.setConfig(data))
	t.Cleanup(func() {
		if p.telemetry != nil {
			p.telemetry.shutdown(context.Background())
		}
	})

	// Telemetry and its registration were replaced, not leaked.
	require.NotNil(t, p.telemetry)
	require.NotNil(t, p.metrics)
	assert.NotSame(t, oldTelem, p.telemetry)
	assert.NotSame(t, oldReg, p.metrics)

	// The old registration was already unregistered; a second call is a no-op.
	assert.NoError(t, oldReg.Unregister())
}
