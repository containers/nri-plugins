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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateMonGroup_RootClass(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)

	dir, err := r.createMonGroup("", "pod-uid-1")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(tmpDir, "mon_groups", "pod-uid-1"), dir)

	// Directory should exist.
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestCreateMonGroup_WithRDTClass(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "BestEffort"), 0755))

	dir, err := r.createMonGroup("BestEffort", "pod-uid-2")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(tmpDir, "BestEffort", "mon_groups", "pod-uid-2"), dir)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestCreateMonGroup_MissingCtrlGroup(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)

	// Attempt to create a mon_group under a non-existent ctrl_group.
	_, err := r.createMonGroup("NoSuchClass", "pod-uid-3")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ctrl_group")

	// Verify the ctrl_group was NOT created.
	_, err = os.Stat(filepath.Join(tmpDir, "NoSuchClass"))
	assert.True(t, os.IsNotExist(err))
}

func TestCreateMonGroup_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)

	dir1, err := r.createMonGroup("", "pod-uid-1")
	require.NoError(t, err)

	dir2, err := r.createMonGroup("", "pod-uid-1")
	require.NoError(t, err)

	assert.Equal(t, dir1, dir2)
}

func TestRemoveMonGroup(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)

	dir, err := r.createMonGroup("", "pod-uid-1")
	require.NoError(t, err)

	err = r.removeMonGroup(dir)
	require.NoError(t, err)

	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

func TestRemoveMonGroup_NotExist(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)

	err := r.removeMonGroup(filepath.Join(tmpDir, "mon_groups", "nonexistent"))
	assert.NoError(t, err)
}

func TestWriteTaskPID(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)

	dir, err := r.createMonGroup("", "pod-uid-1")
	require.NoError(t, err)

	// In real resctrl, the kernel creates the tasks file when the
	// mon_group directory is created. Simulate that here.
	tasksFile := filepath.Join(dir, "tasks")
	require.NoError(t, os.WriteFile(tasksFile, nil, 0644))

	err = r.writeTaskPID(dir, 12345)
	require.NoError(t, err)

	data, err := os.ReadFile(tasksFile)
	require.NoError(t, err)
	assert.Equal(t, "12345\n", string(data))
}

func TestCleanOrphanedMonGroups(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)
	state := newPodState()

	// Create a mon_group that IS tracked.
	trackedUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	dir, err := r.createMonGroup("", trackedUID)
	require.NoError(t, err)
	state.addPod(trackedUID, dir)

	// Create a mon_group that is NOT tracked (orphan).
	orphanUID := "deadbeef-dead-beef-dead-beefdeadbeef"
	_, err = r.createMonGroup("", orphanUID)
	require.NoError(t, err)

	r.cleanOrphanedMonGroups(state)

	// Tracked should still exist.
	_, err = os.Stat(filepath.Join(tmpDir, "mon_groups", trackedUID))
	assert.NoError(t, err)

	// Orphan should be removed.
	_, err = os.Stat(filepath.Join(tmpDir, "mon_groups", orphanUID))
	assert.True(t, os.IsNotExist(err))
}

func TestCleanOrphanedMonGroups_CtrlGroup(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)
	state := newPodState()

	// Create orphan under a ctrl_group.
	orphanUID := "deadbeef-dead-beef-dead-beefdeadbeef"
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "BestEffort"), 0755))
	_, err := r.createMonGroup("BestEffort", orphanUID)
	require.NoError(t, err)

	r.cleanOrphanedMonGroups(state)

	_, err = os.Stat(filepath.Join(tmpDir, "BestEffort", "mon_groups", orphanUID))
	assert.True(t, os.IsNotExist(err))
}

func TestCleanOrphanedMonGroups_StaleLocation(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)
	state := newPodState()

	podUID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	// Create a mon_group under BestEffort (simulates previous run).
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "BestEffort"), 0755))
	_, err := r.createMonGroup("BestEffort", podUID)
	require.NoError(t, err)

	// Track the pod at the root class (simulates current run with different RDT class).
	rootDir, err := r.createMonGroup("", podUID)
	require.NoError(t, err)
	state.addPod(podUID, rootDir)

	r.cleanOrphanedMonGroups(state)

	// Root mon_group (tracked) should still exist.
	_, err = os.Stat(rootDir)
	assert.NoError(t, err)

	// BestEffort mon_group (stale) should be removed.
	_, err = os.Stat(filepath.Join(tmpDir, "BestEffort", "mon_groups", podUID))
	assert.True(t, os.IsNotExist(err))
}

func TestIsValidRDTClass(t *testing.T) {
	assert.True(t, isValidRDTClass("BestEffort"))
	assert.True(t, isValidRDTClass("Guaranteed"))
	assert.True(t, isValidRDTClass("COS1"))
	assert.True(t, isValidRDTClass("my-class_v2"))

	assert.False(t, isValidRDTClass(""))
	assert.False(t, isValidRDTClass("."))
	assert.False(t, isValidRDTClass(".."))
	assert.False(t, isValidRDTClass("../../etc"))
	assert.False(t, isValidRDTClass("foo/bar"))
	assert.False(t, isValidRDTClass("class\x00name"))
}

func TestCreateMonGroup_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	r := newResctrlOps(tmpDir)

	_, err := r.createMonGroup("../../etc", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid RDT class")

	_, err = r.createMonGroup("foo/bar", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid RDT class")

	_, err = r.createMonGroup("..", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid RDT class")
}
