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
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/intel/goresctrl/pkg/monitor"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
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
		config:         cfg,
		mgr:            mgr,
		pendingRemoval: make(map[string]bool),
		pendingCreate:  make(map[string][]retryCtr),
		errCount:       noop.Int64Counter{},
		loggedErrnos:   make(map[syscall.Errno]bool),
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

// tasksFileManager creates the tasks file of each new mon_group, as the kernel
// does, so that AssignPID works on a plain directory tree.
type tasksFileManager struct {
	*monitor.Manager
}

func (m tasksFileManager) EnsureGroup(key, rdtClass string) (*monitor.Group, error) {
	grp, err := m.Manager.EnsureGroup(key, rdtClass)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(grp.Path(), "tasks"), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return grp, f.Close()
}

// failingManager fails the first fails EnsureGroup calls with err, then
// behaves like tasksFileManager.
type failingManager struct {
	tasksFileManager
	err   error
	fails int
	calls int
}

func (m *failingManager) EnsureGroup(key, rdtClass string) (*monitor.Group, error) {
	m.calls++
	if m.fails > 0 {
		m.fails--
		return nil, m.err
	}
	return m.tasksFileManager.EnsureGroup(key, rdtClass)
}

// newFailingTestPlugin builds a test plugin whose first fails EnsureGroup calls
// return errno the way goresctrl reports a failed mkdir.
func newFailingTestPlugin(t *testing.T, errno syscall.Errno, fails int) (*plugin, *failingManager) {
	p := newTestPlugin(t.TempDir())
	err := error(&os.PathError{Op: "mkdir", Path: "mon_groups/x", Err: errno})
	if errno == syscall.ENOSPC {
		err = fmt.Errorf("%w (key x): %w", monitor.ErrNoRMIDs, err)
	}
	fm := &failingManager{tasksFileManager: tasksFileManager{p.mgr.(*monitor.Manager)}, err: err, fails: fails}
	p.mgr = fm
	return p, fm
}

// startContainer delivers the create and start hooks of one container.
func startContainer(t *testing.T, p *plugin, pod *api.PodSandbox, ctr *api.Container) {
	ctx := context.Background()
	created := makeContainer(ctr.GetId(), ctr.GetName(), pod.GetId(), 0, "")
	require.NoError(t, p.PostCreateContainer(ctx, pod, created))
	require.NoError(t, p.StartContainer(ctx, pod, ctr))
	require.NoError(t, p.PostStartContainer(ctx, pod, ctr))
}

// countErrorLogs counts the error-level log entries until the test ends.
func countErrorLogs(t *testing.T) func() int {
	hook := logtest.NewLocal(log)
	t.Cleanup(func() { log.ReplaceHooks(make(logrus.LevelHooks)) })
	return func() int {
		n := 0
		for _, e := range hook.AllEntries() {
			if e.Level <= logrus.ErrorLevel {
				n++
			}
		}
		return n
	}
}

func makePod(uid, namespace, name string) *api.PodSandbox {
	return &api.PodSandbox{
		Id:        "sandbox-" + uid, // CRI sandbox ID != K8s pod UID
		Uid:       uid,
		Namespace: namespace,
		Name:      name,
		Labels:    map[string]string{},
		Pid:       1, // live; a dead sandbox reports 0
	}
}

// makeContainer reports rdtClass in Linux.Rdt.ClosId, as containerd and CRI-O do.
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
		ctr.Linux.Rdt = &api.LinuxRdt{ClosId: &api.OptionalString{Value: rdtClass}}
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
	closID := func(v string) *api.LinuxRdt { return &api.LinuxRdt{ClosId: &api.OptionalString{Value: v}} }
	rdtClass := func(v string) *api.LinuxResources {
		return &api.LinuxResources{RdtClass: &api.OptionalString{Value: v}}
	}
	tests := []struct {
		name      string
		linux     *api.LinuxContainer
		wantClass string
		wantField string
	}{
		{"no linux", nil, "", "none"},
		{"no class", &api.LinuxContainer{Resources: &api.LinuxResources{}}, "", "none"},
		{"ClosId only", &api.LinuxContainer{Rdt: closID("gold")}, "gold", "Linux.Rdt.ClosId"},
		{"RdtClass only", &api.LinuxContainer{Resources: rdtClass("gold")}, "gold", "Linux.Resources.RdtClass"},
		{"ClosId wins", &api.LinuxContainer{Rdt: closID("gold"), Resources: rdtClass("bronze")}, "gold", "Linux.Rdt.ClosId"},
		{"root class name", &api.LinuxContainer{Rdt: closID("system/default")}, "", "Linux.Rdt.ClosId"},
		{"OCI root", &api.LinuxContainer{Rdt: closID("/")}, "", "Linux.Rdt.ClosId"},
		{"RdtClass root class name", &api.LinuxContainer{Resources: rdtClass("system/default")}, "", "Linux.Resources.RdtClass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, field := getRDTClass(&api.Container{Name: "c", Linux: tt.linux})
			assert.Equal(t, tt.wantClass, class)
			assert.Equal(t, tt.wantField, field)
		})
	}
}

func TestGroupClass(t *testing.T) {
	const (
		podAnn     = "rdt.resources.beta.kubernetes.io/pod"
		sidecarAnn = "rdt.resources.beta.kubernetes.io/container.sidecar"
	)
	tests := []struct {
		name      string
		podAnns   map[string]string
		ctrAnns   map[string]string
		ctrName   string
		ctrClass  string
		wantClass string
		wantOK    bool
	}{
		{"no pod class", nil, nil, "sidecar", "bronze", "bronze", true},
		{"no pod class, override", map[string]string{sidecarAnn: "bronze"}, nil, "sidecar", "bronze", "bronze", true},
		{"pod class", map[string]string{podAnn: "gold"}, nil, "app", "gold", "gold", true},
		{"override to another class", map[string]string{podAnn: "gold", sidecarAnn: "bronze"}, nil, "sidecar", "bronze", "bronze", false},
		{"override applies to its container only", map[string]string{podAnn: "gold", sidecarAnn: "bronze"}, nil, "app", "gold", "gold", true},
		{"CRI container override", map[string]string{podAnn: "gold"}, map[string]string{"io.kubernetes.cri.rdt-class": "bronze"}, "sidecar", "bronze", "bronze", false},
		{"override to root", map[string]string{podAnn: "gold", sidecarAnn: "system/default"}, nil, "sidecar", "", "", false},
		{"override to the pod class", map[string]string{podAnn: "gold", sidecarAnn: "gold"}, nil, "sidecar", "gold", "gold", true},
		{"pod class not applied", map[string]string{podAnn: "gold"}, nil, "app", "", "", true},
		{"override not applied but in the pod class", map[string]string{podAnn: "gold", sidecarAnn: "bronze"}, nil, "sidecar", "gold", "gold", true},
		{"pod class is root", map[string]string{podAnn: "system/default", sidecarAnn: "bronze"}, nil, "app", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "default", "p")
			pod.Annotations = tt.podAnns
			ctr := makeContainer("c1", tt.ctrName, pod.GetId(), 0, tt.ctrClass)
			ctr.Annotations = tt.ctrAnns
			class, ok := groupClass(pod, ctr)
			assert.Equal(t, tt.wantClass, class)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}

// setupPodClassTest builds a plugin over a resctrl tree with ctrl groups gold
// and bronze, and a pod whose pod-level class is gold and whose sidecar is in
// bronze. The runtime has already placed app (PID 42) in gold and sidecar
// (PID 77) in bronze.
func setupPodClassTest(t *testing.T) (p *plugin, root string, pod *api.PodSandbox, app, sidecar *api.Container) {
	root = t.TempDir()
	for class, pid := range map[string]string{"gold": "42\n", "bronze": "77\n"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, class), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, class, "tasks"), []byte(pid), 0o644))
	}
	p = newTestPlugin(root)
	p.mgr = tasksFileManager{Manager: p.mgr.(*monitor.Manager)}

	pod = makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "default", "app")
	pod.Annotations = map[string]string{
		"rdt.resources.beta.kubernetes.io/pod":               "gold",
		"rdt.resources.beta.kubernetes.io/container.sidecar": "bronze",
	}
	app = makeContainer("c1", "app", pod.GetId(), 42, "gold")
	sidecar = makeContainer("c2", "sidecar", pod.GetId(), 77, "bronze")
	return p, root, pod, app, sidecar
}

// assertPodClassGroup checks that the pod's mon_group is under gold only, holds
// app's PID and never sidecar's.
func assertPodClassGroup(t *testing.T, root, uid string) {
	assert.NoDirExists(t, filepath.Join(root, "mon_groups", uid))
	assert.NoDirExists(t, filepath.Join(root, "bronze", "mon_groups", uid))
	data, err := os.ReadFile(filepath.Join(root, "gold", "mon_groups", uid, "tasks"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "42")
	assert.NotContains(t, string(data), "77")
}

// TestPodClass_ContainerOrder verifies that the pod-level class gets the pod's
// mon_group whether the off-class sidecar is created before or after app.
func TestPodClass_ContainerOrder(t *testing.T) {
	for _, sidecarFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "sidecar first", false: "sidecar last"}[sidecarFirst], func(t *testing.T) {
			p, root, pod, app, sidecar := setupPodClassTest(t)
			ctrs := []*api.Container{app, sidecar}
			if sidecarFirst {
				ctrs = []*api.Container{sidecar, app}
			}
			ctx := context.Background()
			for _, ctr := range ctrs {
				created := makeContainer(ctr.GetId(), ctr.GetName(), pod.GetId(), 0, ctr.GetLinux().GetRdt().GetClosId().GetValue())
				require.NoError(t, p.PostCreateContainer(ctx, pod, created))
			}
			for _, ctr := range ctrs {
				require.NoError(t, p.StartContainer(ctx, pod, ctr))
				require.NoError(t, p.PostStartContainer(ctx, pod, ctr))
			}
			assertPodClassGroup(t, root, pod.GetUid())
		})
	}
}

// TestSynchronize_PodClass verifies the pod-level class rule when the plugin
// adopts running containers, listed with the off-class sidecar first.
func TestSynchronize_PodClass(t *testing.T) {
	p, root, pod, app, sidecar := setupPodClassTest(t)
	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{sidecar, app})
	require.NoError(t, err)
	assertPodClassGroup(t, root, pod.GetUid())
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

// TestSynchronize_DeadSandboxByRuntime covers pid 0 at Synchronize. On
// containerd it is a dead sandbox whose RemovePodSandbox may never arrive, so it
// must not keep the group; on CRI-O it is a live infra-less pod whose group must
// survive while its container restarts.
func TestSynchronize_DeadSandboxByRuntime(t *testing.T) {
	ctx := context.Background()
	const uid = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	t.Run("containerd", func(t *testing.T) {
		tmpDir := t.TempDir()
		p := newTestPlugin(tmpDir)
		p.runtime = "containerd"
		dead := makePod(uid, "default", "app")
		dead.Pid = 0
		live := makePod(uid, "default", "app")
		live.Id = "sandbox-attempt-1"
		ctr := makeContainer("c1", "app", live.GetId(), 0, "")
		_, err := p.Synchronize(ctx, []*api.PodSandbox{dead, live}, []*api.Container{ctr})
		require.NoError(t, err)

		require.NoError(t, p.RemovePodSandbox(ctx, live))
		assert.NotContains(t, p.mgr.List(), uid)
		assert.NoDirExists(t, filepath.Join(tmpDir, "mon_groups", uid))
	})

	t.Run("cri-o", func(t *testing.T) {
		tmpDir := t.TempDir()
		p := newTestPlugin(tmpDir)
		p.runtime = "cri-o"
		monDir := filepath.Join(tmpDir, "mon_groups", uid)
		require.NoError(t, os.MkdirAll(monDir, 0o755))
		noInfra := makePod(uid, "default", "app")
		noInfra.Pid = 0
		_, err := p.Synchronize(ctx, []*api.PodSandbox{noInfra}, nil)
		require.NoError(t, err)

		assert.Contains(t, p.mgr.List(), uid)
		assert.DirExists(t, monDir)
	})
}

func TestReconcile_RetriesPendingRemoval(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	// A tracked group whose earlier Remove is assumed to have failed.
	_, err := p.mgr.EnsureGroup(podUID, "")
	require.NoError(t, err)
	require.Contains(t, p.mgr.List(), podUID)
	p.pendingRemoval[podUID] = true

	// The reconciler must retry Remove (not merely Reconcile, which preserves
	// tracked keys) and clear the pending entry on success.
	p.reconcile(p.mgr)

	assert.NotContains(t, p.mgr.List(), podUID)
	assert.Empty(t, p.pendingRemoval)
	_, err = os.Stat(filepath.Join(tmpDir, "mon_groups", podUID))
	assert.True(t, os.IsNotExist(err), "pending mon_group should have been removed on retry")
}

// TestReconcile_KeepsPendingGroupOfLiveSandbox verifies that a failed removal is
// dropped, not retried, once a live sandbox of the same pod is recorded.
func TestReconcile_KeepsPendingGroupOfLiveSandbox(t *testing.T) {
	p := newTestPlugin(t.TempDir())
	const uid = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	_, err := p.mgr.EnsureGroup(uid, "")
	require.NoError(t, err)
	p.pendingRemoval[uid] = true
	p.addSandbox(makePod(uid, "default", "app"))

	p.reconcile(p.mgr)
	assert.Contains(t, p.mgr.List(), uid)
	assert.Empty(t, p.pendingRemoval)
}

// TestReconcile_RetriesWhenNoRMIDFree verifies that a started container whose
// pod found no free RMID is monitored by the reconciler once one is free,
// without an error-level log.
func TestReconcile_RetriesWhenNoRMIDFree(t *testing.T) {
	const uid = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	pid := os.Getpid() // a live process, so the retry is not dropped
	for _, tt := range []struct {
		name  string
		errno syscall.Errno
		fails int // failing EnsureGroup calls before the first reconcile
		sync  bool
	}{
		{"hooks, ENOSPC", syscall.ENOSPC, 3, false},
		{"Synchronize, EBUSY", syscall.EBUSY, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			errorLogs := countErrorLogs(t)
			p, fm := newFailingTestPlugin(t, tt.errno, tt.fails+1)
			pod := makePod(uid, "default", "app")
			ctr := makeContainer("c1", "app", pod.GetId(), uint32(pid), "")
			if tt.sync {
				_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr})
				require.NoError(t, err)
			} else {
				startContainer(t, p, pod, ctr)
			}
			require.Len(t, p.pendingCreate[uid], 1)
			require.Equal(t, 1, fm.fails)

			p.reconcile(p.mgr)
			assert.NotContains(t, p.mgr.List(), uid, "still no free RMID")
			require.Len(t, p.pendingCreate[uid], 1)

			p.reconcile(p.mgr)
			assert.Empty(t, p.pendingCreate)
			data, err := os.ReadFile(filepath.Join(p.config.ResctrlPath, "mon_groups", uid, "tasks"))
			require.NoError(t, err)
			assert.Equal(t, strconv.Itoa(pid)+"\n", string(data))
			assert.Zero(t, errorLogs())
		})
	}
}

// TestReconcile_DropsRetry verifies that a queued container is dropped, without
// creating a group, once its init has exited (its PID's start time changed) or
// its pod has been removed.
func TestReconcile_DropsRetry(t *testing.T) {
	const uid = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	for _, tt := range []struct {
		name string
		drop func(p *plugin, pod *api.PodSandbox)
	}{
		{"PID reused", func(p *plugin, _ *api.PodSandbox) { p.pendingCreate[uid][0].startTime++ }},
		{"pod removed", func(p *plugin, pod *api.PodSandbox) {
			require.NoError(t, p.RemovePodSandbox(context.Background(), pod))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, fm := newFailingTestPlugin(t, syscall.ENOSPC, 3)
			pod := makePod(uid, "default", "app")
			startContainer(t, p, pod, makeContainer("c1", "app", pod.GetId(), uint32(os.Getpid()), ""))
			require.Len(t, p.pendingCreate[uid], 1)

			tt.drop(p, pod)
			calls := fm.calls
			p.reconcile(p.mgr)
			assert.Equal(t, calls, fm.calls, "no EnsureGroup for a dropped container")
			assert.Empty(t, p.pendingCreate)
			assert.NoDirExists(t, filepath.Join(p.config.ResctrlPath, "mon_groups", uid))
		})
	}
}

// TestCountError verifies that failed creations are counted by errno in
// resctrl_mon_errors_total, that EACCES is logged at error level once and is
// not retried.
func TestCountError(t *testing.T) {
	errorLogs := countErrorLogs(t)
	p, _ := newFailingTestPlugin(t, syscall.EACCES, 100)
	p.config.Telemetry = defaultTelemetryConfig()
	p.config.Telemetry.Prometheus.ListenAddress = "127.0.0.1:0"
	require.NoError(t, p.startTelemetry(context.Background()))
	t.Cleanup(func() { p.telemetry.shutdown(context.Background()) })

	for i, uid := range []string{"a1b2c3d4-e5f6-7890-abcd-ef1234567890", "b1b2c3d4-e5f6-7890-abcd-ef1234567890"} {
		pod := makePod(uid, "default", "app")
		startContainer(t, p, pod, makeContainer("c", "app", pod.GetId(), uint32(os.Getpid()), ""))
		assert.Equal(t, 1, errorLogs(), "after pod %d", i)
	}
	assert.Empty(t, p.pendingCreate, "EACCES is not retried")

	resp, err := http.Get("http://" + p.telemetry.promListener.Addr().String() + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "# TYPE resctrl_mon_errors_total counter\n")
	// 2 pods x (PostCreateContainer, StartContainer, PostStartContainer)
	assert.Regexp(t, regexp.MustCompile(`(?m)^resctrl_mon_errors_total\{error_type="EACCES"[^}]*\} 6$`), string(body))
}

// TestSynchronize_AdoptsExistingGroupClass verifies that on restart a pod's
// existing group keeps its class even when an off-class container is listed
// first, instead of a duplicate being created under the other class.
func TestSynchronize_AdoptsExistingGroupClass(t *testing.T) {
	tmpDir := t.TempDir()
	p := newTestPlugin(tmpDir)
	const uid = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "BestEffort", "mon_groups", uid), 0o755))

	pod := makePod(uid, "default", "app")
	sidecar := makeContainer("c1", "sidecar", pod.GetId(), 0, "")
	app := makeContainer("c2", "app", pod.GetId(), 0, "BestEffort")
	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{sidecar, app})
	require.NoError(t, err)

	assert.DirExists(t, filepath.Join(tmpDir, "BestEffort", "mon_groups", uid))
	assert.NoDirExists(t, filepath.Join(tmpDir, "mon_groups", uid))
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
