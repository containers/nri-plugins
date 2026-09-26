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
	"net"
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

// recordingManager wraps a real *monitor.Manager but replaces Reconcile with a
// recorder. goresctrl's Reconcile reaps orphan mon_groups with rmdir(2), which
// on tmpfs cannot delete a realistic (tasks-file-bearing) mon_group the way the
// resctrl kernel does. The plugin's own responsibility is the live set it hands
// to Reconcile, so these tests capture that set and leave physical reaping to
// goresctrl's own reconcile tests.
type recordingManager struct {
	*monitor.Manager
	reconciled    bool
	lastReconcile []string
}

func (m *recordingManager) Reconcile(live []string) error {
	m.reconciled = true
	m.lastReconcile = append([]string(nil), live...)
	return nil
}

// newRecordingTestPlugin builds a test plugin whose manager records the live set
// passed to Reconcile instead of performing filesystem reaping.
func newRecordingTestPlugin(resctrlPath string) (*plugin, *recordingManager) {
	p := newTestPlugin(resctrlPath)
	rec := &recordingManager{Manager: p.mgr.(*monitor.Manager)}
	p.mgr = rec
	return p, rec
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
resctrlPath: /tmp/resctrl-test
namespaces:
  - production
  - staging
labelSelector:
  monitor: "true"
`)

	err := p.setConfig(configYAML)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/resctrl-test", p.config.ResctrlPath)
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

	p, rec := newRecordingTestPlugin(tmpDir)

	// Synchronize with a single live pod that is not the orphan.
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	pod := makePod(podUID, "default", "live-pod")
	ctr := makeContainer("c1", "container1", pod.GetId(), 0, "")

	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr})
	require.NoError(t, err)

	// The live pod's mon_group was created.
	_, err = os.Stat(filepath.Join(tmpDir, "mon_groups", podUID))
	assert.NoError(t, err)

	// Synchronize reconciles with a live set that includes the live pod but not
	// the orphan, so goresctrl reaps the orphan (the physical rmdir is covered
	// by goresctrl's own reconcile tests).
	require.True(t, rec.reconciled, "Synchronize must reconcile")
	assert.Contains(t, rec.lastReconcile, monitor.CanonicalizePodUID(podUID))
	assert.NotContains(t, rec.lastReconcile, orphanUID,
		"orphan must be absent from the live set so Reconcile reaps it")
}

// TestSynchronize_ReapsStaleTrackedGroupAfterMissedTeardown verifies that a pod
// torn down while the plugin was disconnected (so RemovePodSandbox never fired)
// has its still-tracked mon_group removed on the next Synchronize, whose pod
// list is the runtime's authoritative liveness snapshot. Manager.Reconcile
// preserves tracked entries regardless of the live set, so the plugin must
// remove the stale key explicitly.
func TestSynchronize_ReapsStaleTrackedGroupAfterMissedTeardown(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)

	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	canon := monitor.CanonicalizePodUID(podUID)
	pod := makePod(podUID, "default", "live-pod")
	ctr := makeContainer("c1", "container1", pod.GetId(), 0, "")

	// First sync tracks the pod's mon_group.
	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr})
	require.NoError(t, err)
	require.Contains(t, p.mgr.List(), canon)

	// The pod vanishes without a RemovePodSandbox event. The next Synchronize's
	// authoritative (now empty) pod list must reap the stale tracked group.
	_, err = p.Synchronize(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.NotContains(t, p.mgr.List(), canon,
		"a tracked group whose pod vanished without RemovePodSandbox must be reaped on resync")
	_, statErr := os.Stat(filepath.Join(tmpDir, "mon_groups", podUID))
	assert.True(t, os.IsNotExist(statErr), "stale mon_group dir must be removed")
}

// TestReconcile_PreservesContainerlessLiveSandbox verifies that a monitored pod
// sandbox that is alive with no running container (its mon_group survives from
// a previous run but no EnsureGroup tracks it) stays in the reconcile live set,
// so the background reconciler does not reap it and a restarting container
// reuses the same RMID.
func TestReconcile_PreservesContainerlessLiveSandbox(t *testing.T) {
	tmpDir := t.TempDir()

	// A container-less sandbox: the plugin never calls EnsureGroup for it, so
	// mgr.List() omits it and only the live set protects it.
	sandboxUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	canon := monitor.CanonicalizePodUID(sandboxUID)

	p, rec := newRecordingTestPlugin(tmpDir)

	// Synchronize with the live sandbox but no containers.
	pod := makePod(sandboxUID, "default", "live-pod")
	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, nil)
	require.NoError(t, err)

	// The initial Reconcile's live set protects the sandbox even though it is
	// not tracked in the Manager.
	require.NotContains(t, p.mgr.List(), canon, "container-less sandbox is not tracked in the Manager")
	assert.Contains(t, rec.lastReconcile, canon)

	// A background reconcile tick must still protect it (regression: it used to
	// reconcile against mgr.List() only and reap the group).
	p.reconcile(p.mgr)
	assert.Contains(t, rec.lastReconcile, canon)

	// Once the sandbox is removed, it drops out of the live set and becomes
	// reap-able.
	require.NoError(t, p.RemovePodSandbox(context.Background(), pod))
	p.reconcile(p.mgr)
	assert.NotContains(t, rec.lastReconcile, canon, "sandbox should be reap-able after the pod is gone")
}

func TestReconcile_LiveKeyCanonicalizedAcrossUIDForms(t *testing.T) {
	tmpDir := t.TempDir()

	// The same pod UID in the two forms PodUIDValidator accepts.
	const compact = "a1b2c3d4e5f67890abcdef1234567890"
	const dashed = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	p, rec := newRecordingTestPlugin(tmpDir)

	// Synchronize reports the sandbox in compact form; addSandbox stores it
	// under the canonical dashed form, which reconcileLiveSet then emits.
	pod := makePod(compact, "default", "live-pod")
	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, nil)
	require.NoError(t, err)
	p.reconcile(p.mgr)
	assert.Contains(t, rec.lastReconcile, dashed)

	// Removal of the same sandbox reports the equivalent dashed UID.
	// RemovePodSandbox must canonicalize it so the recorded entry is dropped and
	// the group can be reaped; otherwise it would stay in the live set forever.
	removed := makePod(dashed, "default", "live-pod")
	removed.Id = pod.Id
	require.NoError(t, p.RemovePodSandbox(context.Background(), removed))
	p.reconcile(p.mgr)
	assert.NotContains(t, rec.lastReconcile, dashed,
		"live key must be dropped once the equivalent dashed UID is removed")
}

// TestReconcile_ProtectsFilteredLivePod verifies that a live pod excluded by the
// filters stays in the reconcile live set, so a UUID-named group that other
// tooling owns for it is not reaped.
func TestReconcile_ProtectsFilteredLivePod(t *testing.T) {
	p, rec := newRecordingTestPlugin(t.TempDir())
	p.config.Namespaces = []string{"production"}
	const uid = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{makePod(uid, "default", "other")}, nil)
	require.NoError(t, err)
	assert.Contains(t, rec.lastReconcile, uid)

	p.reconcile(p.mgr)
	assert.Contains(t, rec.lastReconcile, uid)
}

// TestRemovePodSandbox_OldSandboxKeepsGroup is the regression test for kubelet
// garbage-collecting an older sandbox of a still-live pod: only removing the
// pod's last sandbox may remove its mon_group.
func TestRemovePodSandbox_OldSandboxKeepsGroup(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	ctx := context.Background()
	const podUID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	monDir := filepath.Join(tmpDir, "mon_groups", podUID)

	oldPod := makePod(podUID, "default", "test-pod")
	newPod := makePod(podUID, "default", "test-pod")
	newPod.Id = "sandbox-attempt-1"

	require.NoError(t, p.PostCreateContainer(ctx, oldPod, makeContainer("c1", "app", oldPod.GetId(), 0, "")))
	require.NoError(t, p.RunPodSandbox(ctx, newPod))

	require.NoError(t, p.RemovePodSandbox(ctx, oldPod))
	assert.Contains(t, p.mgr.List(), podUID)
	assert.DirExists(t, monDir)

	require.NoError(t, p.RemovePodSandbox(ctx, newPod))
	assert.Empty(t, p.mgr.List())
	assert.NoDirExists(t, monDir)
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

// TestStartContainer_OffClassSidecarNotAssigned verifies the core safety
// invariant: assigning a PID to a pod's mon_group must never move a container
// into a different resctrl control group. When the pod's mon_group was created
// under one RDT class, a later container in a different class (e.g. an
// off-class sidecar) must not have its PID written to that group's tasks file.
func TestStartContainer_OffClassSidecarNotAssigned(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "BestEffort"), 0755))

	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	pod := makePod(podUID, "default", "test-pod")

	// First container establishes the pod's mon_group under BestEffort.
	app := makeContainer("c1", "app", podUID, 0, "BestEffort")
	require.NoError(t, p.PostCreateContainer(context.Background(), pod, app))

	monDir := filepath.Join(tmpDir, "BestEffort", "mon_groups", podUID)
	require.DirExists(t, monDir)
	require.NoError(t, os.WriteFile(filepath.Join(monDir, "tasks"), nil, 0644))

	// A root-class sidecar in the same pod must not be assigned: writing its
	// PID here would rewrite its CLOSID into the BestEffort ctrl_group.
	sidecar := makeContainer("c2", "sidecar", podUID, 77, "")
	require.NoError(t, p.StartContainer(context.Background(), pod, sidecar))
	require.NoError(t, p.PostStartContainer(context.Background(), pod, sidecar))

	data, err := os.ReadFile(filepath.Join(monDir, "tasks"))
	require.NoError(t, err)
	assert.Empty(t, string(data), "off-class sidecar PID must not be written to the pod mon_group")
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

// TestSetConfig_RejectedAfterStart verifies that configuration is fixed once the
// plugin has started: any later change is rejected and the original is kept.
func TestSetConfig_RejectedAfterStart(t *testing.T) {
	root := setupTestResctrl(t, map[string]map[string]map[string]string{})

	p := newTestPlugin(root)
	// Port-less telemetry marks the plugin as started.
	p.config.Telemetry = defaultTelemetryConfig()
	p.config.Telemetry.Prometheus.Enabled = false
	require.NoError(t, p.startTelemetry(context.Background()))
	t.Cleanup(func() { p.telemetry.shutdown(context.Background()) })

	err := p.setConfig([]byte("resctrlPath: " + root + "\nnamespaces:\n  - production\n"))
	require.ErrorContains(t, err, "cannot change after startup")
	assert.Empty(t, p.config.Namespaces)
}

// TestSetConfig_AllowsInitialRootSelection verifies that a non-default
// resctrlPath supplied before the plugin is running (no telemetry, no
// reconciler) is accepted and rebuilds the manager, rather than being rejected
// as a change to a running plugin.
func TestSetConfig_AllowsInitialRootSelection(t *testing.T) {
	empty := map[string]map[string]map[string]string{}
	root1 := setupTestResctrl(t, empty)
	root2 := setupTestResctrl(t, empty)

	// newPlugin-equivalent initial state: config points at root1, no telemetry
	// or reconciler running yet.
	p := newTestPlugin(root1)
	oldMgr := p.mgr

	require.NoError(t, p.setConfig([]byte("resctrlPath: "+root2+"\n")))
	assert.Equal(t, root2, p.config.ResctrlPath)
	assert.NotSame(t, oldMgr, p.mgr, "manager should be rebuilt for the new root")
}

// TestConfigure_TelemetryBindFailureIsNonFatal verifies that a telemetry bind
// failure does not fail Configure, so mon_group management keeps running.
func TestConfigure_TelemetryBindFailureIsNonFatal(t *testing.T) {
	root := setupTestResctrl(t, map[string]map[string]map[string]string{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	p := newTestPlugin(root)
	p.config.Telemetry = defaultTelemetryConfig()
	p.config.Telemetry.Prometheus.ListenAddress = ln.Addr().String()

	_, err = p.Configure(context.Background(), "", "containerd", "v2.0.0")
	require.NoError(t, err)
	assert.Nil(t, p.telemetry)
}
