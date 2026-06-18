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
	return &plugin{
		config: cfg,
		state:  newPodState(),
		rdt:    newResctrlOps(resctrlPath),
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
	assert.Equal(t, 0, p.state.podCount())
}

func TestPostCreateContainer_CreatesMonGroup(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)

	pod := makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "default", "test-pod")
	ctr := makeContainer("c1", "container1", "a1b2c3d4-e5f6-7890-abcd-ef1234567890", 0, "")

	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	// Pod should be tracked.
	assert.Equal(t, 1, p.state.podCount())
	monDir := p.state.getMonGroupDir("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	assert.Contains(t, monDir, "mon_groups/a1b2c3d4-e5f6-7890-abcd-ef1234567890")
}

func TestPostCreateContainer_WithRDTClass(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "BestEffort"), 0755))

	pod := makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "default", "test-pod")
	ctr := makeContainer("c1", "container1", "a1b2c3d4-e5f6-7890-abcd-ef1234567890", 0, "BestEffort")

	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	monDir := p.state.getMonGroupDir("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	assert.Contains(t, monDir, "BestEffort/mon_groups/a1b2c3d4-e5f6-7890-abcd-ef1234567890")
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
	assert.Equal(t, 1, p.state.podCount())

	// Second container reuses the same mon_group.
	err = p.PostCreateContainer(context.Background(), pod, ctr2)
	require.NoError(t, err)
	assert.Equal(t, 1, p.state.podCount()) // still one pod

	// Stopping first container should not remove the mon_group.
	_, err = p.StopContainer(context.Background(), pod, ctr1)
	require.NoError(t, err)
	assert.Equal(t, 1, p.state.podCount())
	assert.False(t, p.state.podHasNoContainers(podUID))

	// Stopping the last container retains the mon_group (it is released only
	// when the pod sandbox is removed, so the RMID stays stable across
	// container restarts).
	_, err = p.StopContainer(context.Background(), pod, ctr2)
	require.NoError(t, err)
	assert.Equal(t, 1, p.state.podCount())
	assert.True(t, p.state.podHasNoContainers(podUID))

	// Removing the pod sandbox releases the mon_group.
	err = p.RemovePodSandbox(context.Background(), pod)
	require.NoError(t, err)
	assert.Equal(t, 0, p.state.podCount())
}

func TestStopContainer_UnknownPod(t *testing.T) {
	p := newTestPlugin(t.TempDir())

	pod := makePod("unknown-uid", "default", "unknown-pod")
	ctr := makeContainer("c1", "container1", "unknown-uid", 1234, "")

	updates, err := p.StopContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Nil(t, updates)
}

// TestContainerRestart_RetainsMonGroup verifies that a container restart under
// the same pod UID (e.g. restartPolicy: Always after a fixed-duration workload
// exits) keeps the same mon_group directory, so the kernel does not release and
// reassign the RMID. RMID reassignment would carry residual hardware counter
// values and surface as a false counter spike.
func TestContainerRestart_RetainsMonGroup(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	pod := makePod(podUID, "default", "restart-pod")
	ctr := makeContainer("c1", "container1", podUID, 0, "")

	// Container starts: mon_group is created.
	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	monDir := p.state.getMonGroupDir(podUID)
	require.NotEmpty(t, monDir)
	require.DirExists(t, monDir)

	// Container exits (workload timed out). The mon_group must be retained.
	_, err = p.StopContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Equal(t, 1, p.state.podCount())
	assert.DirExists(t, monDir, "mon_group must survive a container restart")

	// kubelet restarts the container under the same pod UID. The same
	// mon_group directory (and thus RMID) must be reused.
	err = p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Equal(t, monDir, p.state.getMonGroupDir(podUID), "restart must reuse the same mon_group")

	// Pod is finally deleted: mon_group is released.
	err = p.RemovePodSandbox(context.Background(), pod)
	require.NoError(t, err)
	assert.Equal(t, 0, p.state.podCount())
	assert.NoDirExists(t, monDir)
}

// TestRemovePodSandbox_UnknownPod verifies that removing a pod we never tracked
// is a no-op and does not error.
func TestRemovePodSandbox_UnknownPod(t *testing.T) {
	p := newTestPlugin(t.TempDir())

	pod := makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "default", "unknown-pod")

	err := p.RemovePodSandbox(context.Background(), pod)
	require.NoError(t, err)
	assert.Equal(t, 0, p.state.podCount())
}

// TestMissingPodUID_NoMonGroup verifies that a sandbox without a valid pod UID
// is handled as a safe no-op across the full lifecycle: no mon_group is created
// (mon_groups are keyed by pod UID, not per container), and the stop/remove
// handlers neither panic nor leak state. This documents that the plugin does
// not currently fall back to per-container monitoring when the UID is absent.
func TestMissingPodUID_NoMonGroup(t *testing.T) {
	p := newTestPlugin(t.TempDir())

	pod := makePod("", "default", "no-uid-pod")
	ctr := makeContainer("c1", "container1", "", 1234, "")

	// Creation must not create a group and must be non-fatal.
	require.NoError(t, p.PostCreateContainer(context.Background(), pod, ctr))
	assert.Equal(t, 0, p.state.podCount())

	// The remaining handlers must be safe no-ops.
	require.NoError(t, p.StartContainer(context.Background(), pod, ctr))
	require.NoError(t, p.PostStartContainer(context.Background(), pod, ctr))

	_, err := p.StopContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Equal(t, 0, p.state.podCount())

	require.NoError(t, p.RemovePodSandbox(context.Background(), pod))
	assert.Equal(t, 0, p.state.podCount())
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
	assert.Equal(t, 1, p.state.podCount())
	assert.True(t, p.state.hasPod(podUID))
	assert.False(t, p.state.hasPod(pod.GetId()))

	monDir := p.state.getMonGroupDir(podUID)
	assert.Contains(t, monDir, podUID)
}

func TestEnsureMonGroup_InvalidUID(t *testing.T) {
	p := newTestPlugin(t.TempDir())

	err := p.ensureMonGroup("", "c1", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pod UID")

	err = p.ensureMonGroup("not-a-uuid", "c1", "")
	assert.Error(t, err)

	assert.Equal(t, 0, p.state.podCount())
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

	monDir := p.state.getMonGroupDir(podUID)
	require.NotEmpty(t, monDir)

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

	monDir := p.state.getMonGroupDir(podUID)
	require.NotEmpty(t, monDir)
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

func TestCompactUID_EnsureMonGroupStoresCanonical(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)

	compactUID := "a1b2c3d4e5f678901234abcdef567890"
	canonicalUID := "a1b2c3d4-e5f6-7890-1234-abcdef567890"

	pod := makePod(compactUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", compactUID, 0, "")

	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	// State must be keyed under the canonical dashed form.
	assert.True(t, p.state.hasPod(canonicalUID))
	assert.False(t, p.state.hasPod(compactUID))

	monDir := p.state.getMonGroupDir(canonicalUID)
	assert.Contains(t, monDir, canonicalUID)
}

func TestCompactUID_StartContainerFindsMonGroup(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)

	compactUID := "a1b2c3d4e5f678901234abcdef567890"
	canonicalUID := "a1b2c3d4-e5f6-7890-1234-abcdef567890"

	pod := makePod(compactUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", compactUID, 0, "")

	// Create mon_group via compact UID.
	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)

	monDir := p.state.getMonGroupDir(canonicalUID)
	require.NotEmpty(t, monDir)
	require.NoError(t, os.WriteFile(filepath.Join(monDir, "tasks"), nil, 0644))

	// StartContainer also using compact UID must find and write to the same mon_group.
	ctrWithPid := makeContainer("c1", "container1", compactUID, 77, "")
	err = p.StartContainer(context.Background(), pod, ctrWithPid)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(monDir, "tasks"))
	require.NoError(t, err)
	assert.Equal(t, "77\n", string(data))
}

func TestCompactUID_RemovePodSandboxCleansUp(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)

	compactUID := "a1b2c3d4e5f678901234abcdef567890"
	canonicalUID := "a1b2c3d4-e5f6-7890-1234-abcdef567890"

	pod := makePod(compactUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", compactUID, 0, "")

	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Equal(t, 1, p.state.podCount())

	// Stopping the last container retains the mon_group.
	_, err = p.StopContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Equal(t, 1, p.state.podCount())

	// Removing the pod sandbox (compact UID) must normalize and clean up.
	err = p.RemovePodSandbox(context.Background(), pod)
	require.NoError(t, err)
	assert.Equal(t, 0, p.state.podCount())
	assert.False(t, p.state.hasPod(canonicalUID))
}

func TestRemovePodSandbox_RemovesStateOnRmdirFailure(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	pod := makePod(podUID, "default", "test-pod")
	ctr := makeContainer("c1", "container1", podUID, 0, "")

	// Create the mon_group.
	err := p.PostCreateContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Equal(t, 1, p.state.podCount())

	monDir := p.state.getMonGroupDir(podUID)
	require.NotEmpty(t, monDir)

	// Put a file inside the mon_group dir so os.Remove fails (dir not empty).
	require.NoError(t, os.WriteFile(filepath.Join(monDir, "tasks"), nil, 0644))

	// Stopping the last container retains the mon_group.
	_, err = p.StopContainer(context.Background(), pod, ctr)
	require.NoError(t, err)
	assert.Equal(t, 1, p.state.podCount())

	// RemovePodSandbox should still drop the pod from state even if rmdir
	// fails (the orphaned directory is reaped later by the reconciler).
	err = p.RemovePodSandbox(context.Background(), pod)
	require.NoError(t, err)
	assert.Equal(t, 0, p.state.podCount())
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
