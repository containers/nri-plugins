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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

func newNamespaceTestPlugin(t *testing.T, root string) *plugin {
	t.Helper()
	cfg := &pluginConfig{ResctrlPath: root, GroupBy: groupByNamespace}
	mgr, err := newManager(cfg)
	require.NoError(t, err)
	return &plugin{
		config:         cfg,
		mgr:            tasksFileManager{mgr},
		pendingRemoval: make(map[string]bool),
	}
}

func monGroupDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "mon_groups"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func startPod(t *testing.T, p *plugin, pod *api.PodSandbox, pid uint32) {
	t.Helper()
	ctr := makeContainer("ctr-"+pod.GetUid(), "main", pod.GetId(), pid, "")
	require.NoError(t, p.RunPodSandbox(context.Background(), pod))
	require.NoError(t, p.PostCreateContainer(context.Background(), pod, ctr))
	require.NoError(t, p.StartContainer(context.Background(), pod, ctr))
}

func TestNamespaceKeyValidator(t *testing.T) {
	assert.True(t, namespaceKeyValidator("ns-production"))
	assert.False(t, namespaceKeyValidator("ns-"))
	assert.False(t, namespaceKeyValidator("production"))
	assert.False(t, namespaceKeyValidator("a1b2c3d4-e5f6-7890-abcd-ef1234567890"))
	assert.False(t, namespaceKeyValidator("ns-a/b"))
}

func TestNamespaceMode_PodsShareOneGroup(t *testing.T) {
	root := t.TempDir()
	p := newNamespaceTestPlugin(t, root)

	podA := makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "prod", "a")
	podB := makePod("b1b2c3d4-e5f6-7890-abcd-ef1234567890", "prod", "b")
	podC := makePod("c1b2c3d4-e5f6-7890-abcd-ef1234567890", "other", "c")

	startPod(t, p, podA, 100)
	startPod(t, p, podB, 200)
	startPod(t, p, podC, 300)

	assert.ElementsMatch(t, []string{"ns-prod", "ns-other"}, monGroupDirs(t, root),
		"one group per namespace, not per pod")
	assert.ElementsMatch(t, []string{"ns-prod", "ns-other"}, p.mgr.List())
}

func TestNamespaceMode_GroupRemovedWithLastPod(t *testing.T) {
	root := t.TempDir()
	p := newNamespaceTestPlugin(t, root)

	podA := makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "prod", "a")
	podB := makePod("b1b2c3d4-e5f6-7890-abcd-ef1234567890", "prod", "b")
	startPod(t, p, podA, 100)
	startPod(t, p, podB, 200)

	require.NoError(t, p.RemovePodSandbox(context.Background(), podA))
	assert.Equal(t, []string{"ns-prod"}, p.mgr.List(),
		"group must stay while another pod of the namespace is alive")
	assert.Equal(t, []string{"ns-prod"}, monGroupDirs(t, root))

	// The kernel lets rmdir succeed on a mon_group whose tasks file is virtual;
	// on a plain directory tree the stand-in tasks file has to go first.
	require.NoError(t, os.Remove(filepath.Join(root, "mon_groups", "ns-prod", "tasks")))
	require.NoError(t, p.RemovePodSandbox(context.Background(), podB))
	assert.Empty(t, monGroupDirs(t, root), "group must go with the namespace's last pod")
	assert.Empty(t, p.mgr.List())
}

func TestNamespaceMode_NamespaceFilterStillApplies(t *testing.T) {
	root := t.TempDir()
	p := newNamespaceTestPlugin(t, root)
	p.config.Namespaces = []string{"prod"}

	startPod(t, p, makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "prod", "a"), 100)
	startPod(t, p, makePod("c1b2c3d4-e5f6-7890-abcd-ef1234567890", "other", "c"), 300)

	assert.Equal(t, []string{"ns-prod"}, monGroupDirs(t, root))
}

func TestNamespaceMode_SynchronizeAdoptsExistingGroup(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "mon_groups", "ns-prod"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "mon_groups", "ns-prod", "tasks"), nil, 0o644))

	p := newNamespaceTestPlugin(t, root)
	pod := makePod("a1b2c3d4-e5f6-7890-abcd-ef1234567890", "prod", "a")
	ctr := makeContainer("c1", "main", pod.GetId(), 100, "")

	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{ctr})
	require.NoError(t, err)
	assert.Equal(t, []string{"ns-prod"}, p.mgr.List())
	assert.Equal(t, []string{"ns-prod"}, monGroupDirs(t, root))
}

func TestSetConfig_GroupBy(t *testing.T) {
	root := t.TempDir()
	p := newTestPlugin(root)
	require.NoError(t, p.setConfig([]byte("resctrlPath: "+root+"\ngroupBy: namespace\n")))
	assert.True(t, p.config.isNamespaceMode())

	_, err := p.mgr.(*monitor.Manager).EnsureGroup("ns-prod", "")
	require.NoError(t, err, "manager must have been rebuilt for namespace keys")

	p = newTestPlugin(t.TempDir())
	require.NoError(t, p.setConfig([]byte("namespaces: [a]\n")))
	assert.Equal(t, groupByPod, p.config.GroupBy, "default stays per pod")

	p = newTestPlugin(t.TempDir())
	assert.Error(t, p.setConfig([]byte("groupBy: node\n")))
}

func TestGroupAttributes_Namespace(t *testing.T) {
	fn := groupAttributesFor("/sys/fs/resctrl", groupByNamespace)
	attrs := fn("ns-prod", "/sys/fs/resctrl/mon_groups/ns-prod")
	got := map[attribute.Key]string{}
	for _, kv := range attrs {
		got[kv.Key] = kv.Value.AsString()
	}
	assert.Equal(t, "prod", got["k8s.namespace.name"])
	assert.Equal(t, "namespace", got["resctrl.group.source"])
	assert.NotContains(t, got, attribute.Key("k8s.pod.uid"))
}
