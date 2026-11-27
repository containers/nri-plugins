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

package steering

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/mpolinject"
)

// writeNodes makes a sysfs with the online nodes 0-3, the nodes of
// has_normal_memory and the distances of node 3 (a movable-only CXL node).
func writeNodes(t *testing.T, normal, distance3 string) string {
	return writeOnlineNodes(t, "0-3", normal, distance3)
}

// writeOnlineNodes is writeNodes with the online nodes.
func writeOnlineNodes(t *testing.T, online, normal, distance3 string) string {
	root := t.TempDir()
	dir := filepath.Join(root, "devices", "system", "node")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "node3"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "online"), []byte(online+"\n"), 0o644))
	if normal != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "has_normal_memory"), []byte(normal+"\n"), 0o644))
	}
	if distance3 != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "node3", "distance"), []byte(distance3+"\n"), 0o644))
	}
	return root
}

func TestNormalMemoryFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		online    string
		normal    string
		distance3 string
		allowed   []int
		permitted string
		node      int
		ok        bool
	}{
		{"only movable node allowed: nearest normal node", "0-3", "0-2", "17 14 15 10", []int{3}, "0-3", 1, true},
		{"equal distances: lowest normal node", "0-3", "0-2", "14 14 14 10", []int{3}, "0-3", 0, true},
		{"no distances: lowest normal node", "0-3", "1-2", "", []int{3}, "0-3", 1, true},
		{"a normal node allowed already", "0-3", "0-2", "17 14 15 10", []int{2, 3}, "0-3", 0, false},
		{"has_normal_memory unknown", "0-3", "", "17 14 15 10", []int{3}, "0-3", 0, false},
		{"nearest permitted normal node", "0-3", "0-2", "17 14 15 10", []int{3}, "0,2-3", 2, true},
		{"no permitted normal node", "0-3", "0-2", "17 14 15 10", []int{3}, "3", 0, false},
		// one distance per online node: node 1 is offline
		{"sparse online nodes", "0,2-3", "0,2", "17 15 10", []int{3}, "0-3", 2, true},
		{"distances not of the online nodes: lowest normal node", "0,2-3", "0,2", "17 14 15 10", []int{3}, "0-3", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeOnlineNodes(t, tc.online, tc.normal, tc.distance3)
			allowed := map[int]bool{}
			for _, n := range tc.allowed {
				allowed[n] = true
			}
			permitted, err := cpuset.Parse(tc.permitted)
			require.NoError(t, err)
			node, ok := normalMemoryFallback(root, allowed, permitted, 3)
			require.Equal(t, tc.ok, ok)
			if ok {
				require.Equal(t, tc.node, node)
			}
		})
	}
}

func TestAllowNodesFirstWrite(t *testing.T) {
	cg := t.TempDir()
	mems := filepath.Join(cg, "cpuset.mems")
	m := &Manager{
		cgroupPath: cg,
		sysRoot:    writeNodes(t, "0-1", ""),
		name:       "test",
		permitted:  cpuset.New(0, 1, 2),
		// the cgroup holds memory on its DRAM nodes already
		allowed: map[int]bool{0: true, 1: true},
	}
	// the first step adds no node and still confines the cgroup
	require.NoError(t, m.allowNodes([]int{0, 1}))
	data, err := os.ReadFile(mems)
	require.NoError(t, err)
	require.Equal(t, "0-1\n", string(data))
	// a later step that adds no node writes nothing
	require.NoError(t, os.WriteFile(mems, []byte("unchanged\n"), 0o644))
	require.NoError(t, m.allowNodes([]int{1}))
	data, err = os.ReadFile(mems)
	require.NoError(t, err)
	require.Equal(t, "unchanged\n", string(data))
	// a step that adds a node writes all of them
	require.NoError(t, m.allowNodes([]int{2}))
	data, err = os.ReadFile(mems)
	require.NoError(t, err)
	require.Equal(t, "0-2\n", string(data))
}

// fakeCgroup returns a cgroup directory with the files that a Manager
// reads: memory.high and cpuset.mems as given, usage on nodes 0 and 3,
// and one process.
func fakeCgroup(t *testing.T, high, mems string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{
		"memory.high":      high,
		"memory.current":   "8192",
		"memory.events":    "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0",
		"memory.numa_stat": "anon N0=4096 N1=0 N3=4096\nshmem N0=0 N1=0 N3=0",
		"cgroup.procs":     "4242",
		"cpuset.mems":      mems,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(data+"\n"), 0o644))
	}
	return dir
}

// readFile returns the content of the file of the cgroup without white
// space around it.
// replaceFile replaces the file at path with data in one rename, as the
// kernel shows a cgroup file that a manager reads when it steps: a
// running manager reads the old content or the new one, never a truncated
// file. memory.events, which the manager watches, is written in place.
func replaceFile(t *testing.T, path, data string) {
	t.Helper()
	tmp := path + ".new"
	require.NoError(t, os.WriteFile(tmp, []byte(data), 0o644))
	require.NoError(t, os.Rename(tmp, path))
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return strings.TrimSpace(string(data))
}

// TestStartFailsClosed leaves a cgroup as it was when Start fails: on a
// memory.high of others and on a memory policy that is not set. A started
// manager writes only permitted nodes to cpuset.mems.
func TestStartFailsClosed(t *testing.T) {
	if !mpolinject.Supported {
		t.Skip(mpolinject.ErrUnsupported)
	}
	plan, err := NewPlan(PolicyFirstCXL, MemoryTypes{CXLNUMANodes: []int{1}, CXLQuota: 64 * MiB}, nil, 0, 0)
	require.NoError(t, err)
	permitted := cpuset.New(0, 1)
	sys := writeNodes(t, "0", "")
	injected := 0
	injectErr := fmt.Errorf("pid 4242: %w", unix.EPERM)
	old := setMemoryPolicy
	t.Cleanup(func() { setMemoryPolicy = old })
	setMemoryPolicy = func([]int, int, []int) error {
		injected++
		return injectErr
	}

	// a memory.high of others: nothing written
	cg := fakeCgroup(t, "536870912", "")
	m, err := New(cg, plan, permitted, WithSysRoot(sys))
	require.NoError(t, err)
	require.ErrorContains(t, m.Start(), "memory.high is 536870912, not max")
	require.Equal(t, "536870912", readFile(t, cg, "memory.high"))
	require.Equal(t, "", readFile(t, cg, "cpuset.mems"))
	require.Zero(t, injected)

	// no memory policy set: the cgroup as it was
	cg = fakeCgroup(t, "max", "")
	m, err = New(cg, plan, permitted, WithSysRoot(sys))
	require.NoError(t, err)
	require.ErrorIs(t, m.Start(), unix.EPERM)
	require.Equal(t, 1, injected)
	require.Equal(t, "max", readFile(t, cg, "memory.high"))
	require.Equal(t, "", readFile(t, cg, "cpuset.mems"))

	// started: the usage on node 3 is not permitted, so node 3 is left out
	injectErr = nil
	m, err = New(cg, plan, permitted, WithSysRoot(sys))
	require.NoError(t, err)
	require.NoError(t, m.Start())
	require.Equal(t, 2, injected)
	require.Equal(t, "0-1", readFile(t, cg, "cpuset.mems"))
	require.NotEqual(t, "max", readFile(t, cg, "memory.high"))
	m.Stop()
	require.Equal(t, "max", readFile(t, cg, "memory.high"))

	// the nodes of the plan must be permitted
	_, err = New(cg, plan, cpuset.New(0), WithSysRoot(sys))
	require.ErrorContains(t, err, "not in the permitted nodes")
}

// TestPastLastWaypoint checks that a manager keeps the policy of the last
// segment past the last waypoint without a memory.high bound, and steers
// again when the usage is below the last waypoint.
func TestPastLastWaypoint(t *testing.T) {
	if !mpolinject.Supported {
		t.Skip(mpolinject.ErrUnsupported)
	}
	var mu sync.Mutex
	var calls []string
	oldSet, oldWait := setMemoryPolicy, waitTimeout
	t.Cleanup(func() { setMemoryPolicy, waitTimeout = oldSet, oldWait })
	setMemoryPolicy = func(pids []int, mode int, nodes []int) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, fmt.Sprintf("%v mode %d nodes %v", pids, mode, nodes))
		return nil
	}
	waitTimeout = 10 * time.Millisecond
	called := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}

	plan, err := NewPlan(PolicyFirstDRAM, MemoryTypes{DRAMNUMANodes: []int{0}, CXLNUMANodes: []int{1}, DRAMQuota: 8 * MiB, CXLQuota: 8 * MiB},
		nil, 0, 0)
	require.NoError(t, err)
	cg := fakeCgroup(t, "max", "")
	numaStat := func(n0, n1 int64) {
		data := fmt.Sprintf("anon N0=%d N1=%d\nshmem N0=0 N1=0\n", n0, n1)
		replaceFile(t, filepath.Join(cg, "memory.numa_stat"), data)
	}
	numaStat(8*MiB, 9*MiB)
	m, err := New(cg, plan, cpuset.New(0, 1), WithSysRoot(writeNodes(t, "0", "")))
	require.NoError(t, err)
	require.NoError(t, m.Start())
	defer m.Stop()
	preferCXL := fmt.Sprintf("[4242] mode %d nodes [1]", unix.MPOL_PREFERRED)
	require.Equal(t, []string{preferCXL}, called(), "past the last waypoint: the nodes of the last segment")
	require.Equal(t, "max", readFile(t, cg, "memory.high"))

	// still past: nothing changes
	time.Sleep(5 * waitTimeout)
	require.Equal(t, []string{preferCXL}, called())
	require.Equal(t, "max", readFile(t, cg, "memory.high"))

	// below the last waypoint again: a bounded step towards it, with the
	// policy that the process has already
	numaStat(8*MiB, 0)
	require.Eventually(t, func() bool { return readFile(t, cg, "memory.high") != "max" }, 5*time.Second, waitTimeout)
	require.Equal(t, []string{preferCXL}, called())

	// a new process in a cgroup below gets the policy at the next step,
	// when the kernel throttles the cgroup at the bound
	child := filepath.Join(cg, "init.scope")
	require.NoError(t, os.Mkdir(child, 0o755))
	replaceFile(t, filepath.Join(child, "cgroup.procs"), "4343\n")
	numaStat(8*MiB, 9*MiB)
	require.NoError(t, os.WriteFile(filepath.Join(cg, "memory.events"), []byte("low 0\nhigh 1\nmax 0\noom 0\noom_kill 0\n"), 0o644))
	require.Eventually(t, func() bool { return readFile(t, cg, "memory.high") == "max" }, 5*time.Second, waitTimeout)
	require.Equal(t, []string{preferCXL, fmt.Sprintf("[4343] mode %d nodes [1]", unix.MPOL_PREFERRED)}, called())
}

// TestPolicyRefused ends the steering when a process refuses the memory
// policy of a later step: memory.high max, cpuset.mems as the steering
// wrote it until RestoreMems, and onEnd with the reason.
func TestPolicyRefused(t *testing.T) {
	if !mpolinject.Supported {
		t.Skip(mpolinject.ErrUnsupported)
	}
	oldSet, oldWait := setMemoryPolicy, waitTimeout
	t.Cleanup(func() { setMemoryPolicy, waitTimeout = oldSet, oldWait })
	setMemoryPolicy = func(pids []int, _ int, _ []int) error {
		if slices.Contains(pids, 4343) {
			return fmt.Errorf("pid 4343: set_mempolicy: %w", unix.EPERM)
		}
		return nil
	}
	waitTimeout = 10 * time.Millisecond
	plan, err := NewPlan(PolicyFirstCXL, MemoryTypes{CXLNUMANodes: []int{1}, CXLQuota: 64 * MiB}, nil, 0, 0)
	require.NoError(t, err)
	cg := fakeCgroup(t, "max", "")
	ended := make(chan error, 1)
	m, err := New(cg, plan, cpuset.New(0, 1), WithSysRoot(writeNodes(t, "0", "")),
		WithOnEnd(func(err error) { ended <- err }))
	require.NoError(t, err)
	require.NoError(t, m.Start())
	require.Equal(t, "0-1", readFile(t, cg, "cpuset.mems"))

	// a new process refuses the policy at the next step
	replaceFile(t, filepath.Join(cg, "cgroup.procs"), "4242\n4343\n")
	require.NoError(t, os.WriteFile(filepath.Join(cg, "memory.events"), []byte("low 0\nhigh 1\nmax 0\noom 0\noom_kill 0\n"), 0o644))
	select {
	case err := <-ended:
		require.ErrorIs(t, err, unix.EPERM)
	case <-time.After(5 * time.Second):
		t.Fatal("the steering goes on after a refused memory policy")
	}
	require.Equal(t, "max", readFile(t, cg, "memory.high"))
	require.Equal(t, "0-1", readFile(t, cg, "cpuset.mems"))
	m.Stop()
	m.RestoreMems()
	require.Equal(t, "", readFile(t, cg, "cpuset.mems"))
}

// TestCgroupGone ends the steering of a cgroup that is removed, and Stop
// returns after it.
func TestCgroupGone(t *testing.T) {
	if !mpolinject.Supported {
		t.Skip(mpolinject.ErrUnsupported)
	}
	oldSet, oldWait := setMemoryPolicy, waitTimeout
	t.Cleanup(func() { setMemoryPolicy, waitTimeout = oldSet, oldWait })
	setMemoryPolicy = func([]int, int, []int) error { return nil }
	waitTimeout = 10 * time.Millisecond
	plan, err := NewPlan(PolicyFirstCXL, MemoryTypes{CXLNUMANodes: []int{1}, CXLQuota: 64 * MiB}, nil, 0, 0)
	require.NoError(t, err)
	cg := fakeCgroup(t, "max", "")
	m, err := New(cg, plan, cpuset.New(0, 1), WithSysRoot(writeNodes(t, "0", "")))
	require.NoError(t, err)
	require.NoError(t, m.Start())
	require.NoError(t, os.RemoveAll(cg))
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the steering of a removed cgroup goes on")
	}
	m.Stop()
}

// TestResetMemoryHigh removes the memory.high that a killed manager left,
// leaves max as it is and creates no file.
func TestResetMemoryHigh(t *testing.T) {
	cg := t.TempDir()
	high := filepath.Join(cg, "memory.high")
	readHigh := func() string {
		data, err := os.ReadFile(high)
		require.NoError(t, err)
		return string(data)
	}

	require.NoError(t, os.WriteFile(high, []byte("140668928\n"), 0o644))
	was, err := Release(cg)
	require.NoError(t, err)
	require.Equal(t, "140668928", was)
	require.Equal(t, "max\n", readHigh())

	// max already (a manager stopped gracefully): nothing to write
	require.NoError(t, os.WriteFile(high, []byte("max\n"), 0o444))
	was, err = Release(cg)
	require.NoError(t, err)
	require.Equal(t, "", was)
	require.Equal(t, "max\n", readHigh())

	// a cgroup without memory.high, or gone
	require.NoError(t, os.Remove(high))
	_, err = Release(cg)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(high)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = Release(filepath.Join(cg, "gone"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
