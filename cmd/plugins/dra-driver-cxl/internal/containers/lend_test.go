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

package containers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/config"
)

func TestLendContainerTable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pod := testPod("u1")
	ctr := testCtr(pod, "c1", "app", 101)
	cg := e.proc(101, "kubepods/u1/c1", "0-1", "")

	_, _, err := e.p.CreateContainer(ctx, pod, ctr)
	noErr(t, err)
	ref, ok := e.newest("u1", "app")
	if !ok || ref.ID != "c1" || ref.PodNamespace != "ns" || ref.PodName != "pod-u1" || ref.Pid != 0 || !e.p.Running("u1", "app") {
		t.Fatalf("created: %+v %v", ref, ok)
	}
	if err := e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("lend before start: %v", err)
	}

	noErr(t, e.p.StartContainer(ctx, pod, ctr))
	// started when the runtime applies updates to its cgroup
	e.noCallback(e.start)
	noErr(t, e.p.PostStartContainer(ctx, pod, ctr))
	started := e.wait(e.start, "u1", "app")
	if want := (state.PodRef{Namespace: "ns", Name: "pod-u1", UID: "u1"}); started.pod != want {
		t.Fatalf("started %+v, expected %+v", started.pod, want)
	}
	want := "u1 ns/pod-u1/app id=c1 pid=101 cgroup=" + cg + " base=0-1"
	if ref, _ := e.newest("u1", "app"); ctrText(ref) != want {
		t.Fatalf("record %s, expected %s", ctrText(ref), want)
	}

	// The init pid, and a pid in a sub-cgroup of the container.
	if pod, name, ok := e.p.ContainerByPid(101); !ok || pod.UID != "u1" || pod.Namespace != "ns" || pod.Name != "pod-u1" || name != "app" {
		t.Fatalf("ContainerByPid(101): %+v %s %v", pod, name, ok)
	}
	e.proc(102, "kubepods/u1/c1/sub", "0-1", "")
	if ref, ok := e.byPid(102); !ok || ref.ID != "c1" {
		t.Fatalf("byPid(102): %+v %v", ref, ok)
	}
	// Prefix match is by path element, not by string.
	e.proc(103, "kubepods/u1/c10", "0-1", "")
	if ref, ok := e.byPid(103); ok {
		t.Fatalf("byPid(103) matched %+v", ref)
	}
	if _, _, ok := e.p.ContainerByPid(999); ok {
		t.Fatal("ContainerByPid(999) matched")
	}

	// Stop fires gone once; Remove after Stop does not fire again.
	_, err = e.p.StopContainer(ctx, pod, ctr)
	noErr(t, err)
	e.wait(e.gone, "u1", "app")
	noErr(t, e.p.RemoveContainer(ctx, pod, ctr))
	e.noCallback(e.gone)
	if _, ok := e.newest("u1", "app"); ok || e.p.Running("u1", "app") {
		t.Fatal("container still known after stop")
	}

	// Remove without Stop fires gone too.
	e.run(pod, "c2", "app", 201, "0")
	noErr(t, e.p.RemoveContainer(ctx, pod, testCtr(pod, "c2", "app", 201)))
	e.wait(e.gone, "u1", "app")
	e.expectUpdates()
}

func TestLendMemsArithmetic(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.runPinned(testPod("u1"), "c1", "app", 101, "0-1")

	steps := []struct {
		lend bool
		node int
		want string // update, "" for none
		lent []int
	}{
		{true, 2, "c1 mems=0-2", []int{2}},
		{true, 2, "", []int{2}}, // idempotent
		{true, 4, "c1 mems=0-2,4", []int{2, 4}},
		{false, 2, "c1 mems=0-1,4", []int{4}},
		{false, 2, "", []int{4}}, // idempotent
		{false, 4, "c1 mems=0-1", []int{}},
		{false, 4, "", []int{}},
	}
	for i, s := range steps {
		var err error
		if s.lend {
			err = e.p.LendNUMANode(ctx, "u1", "app", s.node, "l1")
		} else {
			err = e.p.RevokeNUMANode(ctx, "u1", "app", s.node, "l1")
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if s.want == "" {
			e.expectUpdates()
		} else {
			e.expectUpdates(s.want)
		}
		if got := e.p.LentNodes("u1", "app"); !reflect.DeepEqual(got, s.lent) {
			t.Fatalf("step %d: lent %v, expected %v", i, got, s.lent)
		}
	}
	if got := e.baseOf("u1", "app"); got != "0-1" {
		t.Fatalf("BaseMems changed: %q", got)
	}
	if err := e.p.LendNUMANode(ctx, "u1", "app", -1, "l1"); err == nil {
		t.Fatal("negative node accepted")
	}
	if err := e.p.RevokeNUMANode(ctx, "u1", "app", -1, "l1"); err == nil {
		t.Fatal("negative node accepted")
	}
}

// TestLendTwoLeases lends one node for two leases: the node stays in
// cpuset.mems until both have revoked it.
func TestLendTwoLeases(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.runPinned(testPod("u1"), "c1", "app", 101, "0-1")
	steps := []struct {
		lend  bool
		lease string
		want  string // update, "" for none
		lent  []int
		mems  string // cpuset.mems of the cgroup after the step, "" unchanged
	}{
		{true, "a", "c1 mems=0-2", []int{2}, "0-2"},
		{true, "b", "", []int{2}, ""},
		{true, "a", "", []int{2}, ""}, // idempotent
		// b holds the node: no shrink, although cpuset.mems has it
		{false, "a", "", []int{2}, ""},
		{false, "a", "", []int{2}, ""}, // idempotent
		{false, "x", "", []int{2}, ""}, // not a lease
		{false, "b", "c1 mems=0-1", []int{}, "0-1"},
		{false, "b", "", []int{}, ""}, // idempotent
	}
	for i, s := range steps {
		var err error
		if s.lend {
			err = e.p.LendNUMANode(ctx, "u1", "app", 2, s.lease)
		} else {
			err = e.p.RevokeNUMANode(ctx, "u1", "app", 2, s.lease)
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if s.want == "" {
			e.expectUpdates()
		} else {
			e.expectUpdates(s.want)
		}
		if got := e.p.LentNodes("u1", "app"); !reflect.DeepEqual(got, s.lent) {
			t.Fatalf("step %d: lent %v, expected %v", i, got, s.lent)
		}
		if s.mems != "" {
			e.setCgroupMems("kubepods/u1/c1", s.mems)
		}
	}

	// two nodes, each with its own leases
	for _, l := range []struct {
		node  int
		lease string
		want  string
	}{{2, "a", "c1 mems=0-2"}, {3, "a", "c1 mems=0-3"}, {3, "b", ""}} {
		noErr(t, e.p.LendNUMANode(ctx, "u1", "app", l.node, l.lease))
		if l.want == "" {
			e.expectUpdates()
		} else {
			e.expectUpdates(l.want)
		}
	}
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "app", 3, "a"))
	e.expectUpdates()
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "app", 2, "a"))
	e.expectUpdates("c1 mems=0-1,3")
	if got := e.p.LentNodes("u1", "app"); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("lent %v", got)
	}
}

func TestLendGoneContainer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pod := testPod("u1")
	ctr := e.runPinned(pod, "c1", "app", 101, "0")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 3, "l1"))
	e.expectUpdates("c1 mems=0,3")

	_, err := e.p.StopContainer(ctx, pod, ctr)
	noErr(t, err)
	e.wait(e.gone, "u1", "app")
	if err := e.p.RevokeNUMANode(ctx, "u1", "app", 3, "l1"); err != nil {
		t.Fatalf("revoke of a gone container: %v", err)
	}
	e.expectUpdates()
	if err := e.p.LendNUMANode(ctx, "u1", "app", 3, "l1"); err == nil {
		t.Fatal("lend to a gone container succeeded")
	}
	if got := e.p.LentNodes("u1", "app"); len(got) != 0 {
		t.Fatalf("lent nodes of a gone container: %v", got)
	}

	// The restarted container (new id) starts without lends.
	e.runPinned(pod, "c1b", "app", 111, "0")
	if got := e.p.LentNodes("u1", "app"); len(got) != 0 {
		t.Fatalf("restarted container has lends: %v", got)
	}
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 3, "l1"))
	e.expectUpdates("c1b mems=0,3")
}

// TestLendRefusedWhenSteered refuses a lend to a steered container, in any
// lifecycle state, and revokes nothing of it.
func TestLendRefusedWhenSteered(t *testing.T) {
	e := newEnv(t)
	e.hasMemory("0-2")
	e.local.set(fenceClaims())
	ctx := context.Background()
	steerable(t, e.proc(101, "kubepods/u1/c1", "0-2", ""), "max")
	pod := testPod("u1")
	// created: a plan; started: a manager
	c1 := withClaims(testCtr(pod, "c1", "app", 101), "numa-auto")
	if got := e.createMems(pod, c1); got != "-" {
		t.Fatalf("adjustment %s", got)
	}
	pod2 := testPod("u2")
	if got := e.createMems(pod2, withClaims(testCtr(pod2, "c2", "app", 0), "numa-auto")); got != "-" {
		t.Fatalf("adjustment %s", got)
	}
	check := func() {
		t.Helper()
		for _, uid := range []string{"u1", "u2"} {
			err := e.p.LendNUMANode(ctx, uid, "app", 2, "l1")
			if !errors.Is(err, ErrSteered) || !strings.Contains(err.Error(), "lending is not supported together with steering yet") {
				t.Fatalf("pod %s: expected steering refusal, got %v", uid, err)
			}
			noErr(t, e.p.RevokeNUMANode(ctx, uid, "app", 2, "l1"))
		}
		e.expectUpdates()
	}
	check()
	e.startCtr(pod, c1)
	if c := e.record("c1"); c.Management != ManagementSteered || c.Lifecycle != LifecycleStarted {
		t.Fatalf("record %+v", c)
	}
	check()
	e.p.Stop()
}

func TestLendUpdateFailures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.runPinned(testPod("u1"), "c1", "app", 101, "0")

	e.stub.setErr(nil, true)
	if err := e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("failed update not reported: %v", err)
	}
	e.stub.setErr(errors.New("ttrpc closed"), false)
	if err := e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"); err == nil || !strings.Contains(err.Error(), "ttrpc closed") {
		t.Fatalf("update error not reported: %v", err)
	}
	if got := e.p.LentNodes("u1", "app"); len(got) != 0 {
		t.Fatalf("failed lends recorded: %v", got)
	}
	e.stub.setErr(nil, false)
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"))
	e.expectUpdates("c1 mems=0,2")

	// No stub: NRI not connected.
	e.p.connect(nil)
	if err := e.p.LendNUMANode(ctx, "u1", "app", 3, "l1"); err == nil || !strings.Contains(err.Error(), "lend NUMA node 3: lending unavailable: NRI not connected") {
		t.Fatalf("expected NRI not connected, got %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.p.LendNUMANode(cctx, "u1", "app", 3, "l1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}

func TestLendNeverWithoutBaseMems(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pod := testPod("u1")
	// No cgroup and no spec mems: base unknown, but with fencing none it
	// inherits every node, nothing is set.
	ctr := testCtr(pod, "c1", "app", 0)
	_, _, err := e.p.CreateContainer(ctx, pod, ctr)
	noErr(t, err)
	e.startCtr(pod, ctr)
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"))
	if got := e.p.LentNodes("u1", "app"); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("lent %v", got)
	}
	// The only own node is the node being lent.
	e.runPinned(testPod("u2"), "c2", "app", 201, "2")
	if err := e.p.LendNUMANode(ctx, "u2", "app", 2, "l1"); err == nil || !strings.Contains(err.Error(), "base memory nodes unknown") {
		t.Fatalf("lend of the only node: %v", err)
	}
	e.expectUpdates()

	// Spec mems is the fallback when cgroupfs is unreadable.
	pod3 := testPod("u3")
	ctr3 := withSpecMems(testCtr(pod3, "c3", "app", 0), "1")
	_, _, err = e.p.CreateContainer(ctx, pod3, ctr3)
	noErr(t, err)
	e.startCtr(pod3, ctr3)
	noErr(t, e.p.LendNUMANode(ctx, "u3", "app", 2, "l1"))
	e.expectUpdates("c3 mems=1-2")
}

func TestLendBaseExcludesNodesLentToOthers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.runPinned(testPod("u1"), "c1", "app", 101, "0-1")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")
	// An unconstrained container started now sees node 2 as effective.
	e.run(testPod("u2"), "c2", "app", 201, "0-2")
	if got := e.baseOf("u2", "app"); got != "0-1" {
		t.Fatalf("BaseMems %q includes a node lent to another container", got)
	}
}

func TestLendSynchronize(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p1, p2 := testPod("u1"), testPod("u2")
	// Before a restart node 2 was lent to c1, node 3 to c2 (and the
	// lease of c2 is not re-applied), c3 is only created.
	cg1 := e.proc(101, "kubepods/u1/c1", "0-2", "0-2")
	e.proc(201, "kubepods/u2/c2", "0,3", "0,3")
	c1 := testCtr(p1, "c1", "app", 101)
	c1.State = api.ContainerState_CONTAINER_RUNNING
	c2 := testCtr(p2, "c2", "app", 201)
	c2.State = api.ContainerState_CONTAINER_RUNNING
	c3 := testCtr(p2, "c3", "init", 0)
	c3.State = api.ContainerState_CONTAINER_CREATED
	c4 := testCtr(p2, "c4", "old", 0)
	c4.State = api.ContainerState_CONTAINER_STOPPED

	syncCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	if err := e.p.WaitSynchronized(syncCtx); err == nil || !strings.Contains(err.Error(), "NRI not synchronized yet") {
		t.Fatalf("synchronized before Synchronize: %v", err)
	}
	cancel()
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{p1, p2}, []*api.Container{c1, c2, c3, c4})
	noErr(t, err)
	noErr(t, e.p.WaitSynchronized(ctx))
	e.noCallback(e.start)

	ref, ok := e.newest("u1", "app")
	want := "u1 ns/pod-u1/app id=c1 pid=101 cgroup=" + cg1 + " base=0-2"
	if !ok || ctrText(ref) != want {
		t.Fatalf("synced %s, expected %s", ctrText(ref), want)
	}
	if e.record("c1").Lifecycle != LifecycleStarted || e.record("c3").Lifecycle != LifecycleUnseen {
		t.Fatalf("lifecycles %s %s", e.record("c1").Lifecycle, e.record("c3").Lifecycle)
	}
	if _, ok := e.newest("u2", "old"); ok {
		t.Fatal("stopped container tracked")
	}
	if ref, ok := e.byPid(201); !ok || ref.ID != "c2" {
		t.Fatalf("byPid after sync: %+v %v", ref, ok)
	}

	// Re-applying the lend drops node 2 from BaseMems.
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")
	if got := e.baseOf("u1", "app"); got != "0-1" {
		t.Fatalf("BaseMems after re-lend %q", got)
	}
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "app", 2, "l1"))
	e.expectUpdates("c1 mems=0-1")

	// A revoke without a re-lend still shrinks cpuset.mems.
	noErr(t, e.p.RevokeNUMANode(ctx, "u2", "app", 3, "l1"))
	e.expectUpdates("c2 mems=0")
}

func TestLendConcurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.runPinned(testPod("u1"), "c1", "app", 101, "0")
	var wg sync.WaitGroup
	for n := 1; n <= 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := e.p.LendNUMANode(ctx, "u1", "app", n, "l1"); err != nil {
				t.Error(err)
			}
			_ = e.p.LentNodes("u1", "app")
			_, _, _ = e.p.ContainerByPid(101)
		}(n)
	}
	wg.Wait()
	if lent := e.p.LentNodes("u1", "app"); !reflect.DeepEqual(lent, []int{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("lent %v", lent)
	}
	ups := e.stub.taken()
	if len(ups) != 8 || ups[7] != "c1 mems=0-8" {
		t.Fatalf("updates %q", ups)
	}
}

// TestLendNoneInherits checks that with fencing none a lend and a
// revoke leave the empty cpuset.mems of a container alone: it has every
// node with memory, also one whose memory goes online later, and a revoke
// must not take a node that has memory of others off it. Also after a
// plugin restart, with the lease kept or released while it was down.
func TestLendNoneInherits(t *testing.T) {
	e := newEnv(t)
	logs := klogtest.Capture(t, 3)
	ctx := context.Background()
	pod := testPod("u1")
	// node 2 has memory, a numa slice of another pod
	e.run(pod, "c1", "main", 101, "0-2")
	for i, s := range []struct {
		lend  bool
		node  int
		lease string
		lent  []int
	}{
		{true, 2, "l1", []int{2}},
		{true, 2, "l1", []int{2}}, // the reconcile of the broker
		{true, 2, "l2", []int{2}},
		{false, 2, "l1", []int{2}},
		{true, 3, "l3", []int{2, 3}}, // no memory yet
		{false, 2, "l2", []int{3}},
		{false, 2, "l2", []int{3}},
		{false, 3, "l3", []int{}},
	} {
		if s.lend {
			noErr(t, e.p.LendNUMANode(ctx, "u1", "main", s.node, s.lease))
		} else {
			noErr(t, e.p.RevokeNUMANode(ctx, "u1", "main", s.node, s.lease))
		}
		e.expectUpdates()
		if got := e.p.LentNodes("u1", "main"); !reflect.DeepEqual(got, s.lent) {
			t.Fatalf("step %d: lent %v, expected %v", i, got, s.lent)
		}
	}
	if got := e.cgroupMems("kubepods/u1/c1"); got != "" {
		t.Fatalf("cgroup mems %q, expected empty", got)
	}
	out := logs()
	if n := strings.Count(out, "cpuset.mems left empty, it has the nodes of its parent (fencing none"); n != 6 {
		t.Errorf("%d \"left empty\" lines, expected 6 (one per change of the leases):\n%s", n, out)
	}
	if strings.Contains(out, "cpuset.mems=") {
		t.Errorf("cpuset.mems set:\n%s", out)
	}
	// node 3 has no memory yet: not in its cpuset.mems.effective
	if !strings.Contains(out, "lend NUMA node 3 for l3: ctr=ns/pod-u1/main id=c1: its cpuset.mems.effective lacks the lent NUMA nodes 3 (") {
		t.Errorf("no warning of node 3:\n%s", out)
	}
	if n := strings.Count(out, "cpuset.mems.effective lacks"); n != strings.Count(out, "lacks the lent NUMA nodes 3 (") {
		t.Errorf("warning of another node:\n%s", out)
	}

	// a restarted plugin: the broker keeps the lease l1 of node 2, the
	// lease l3 of node 3 was released while the plugin was down
	n := newEnvWith(t, envOptions{fencing: config.FencingNone, leases: []state.Lease{readyLease("l1", "u1", "main", 2)}})
	n.proc(101, "kubepods/u1/c1", "0-3", "")
	c1 := testCtr(pod, "c1", "main", 101)
	c1.State = api.ContainerState_CONTAINER_RUNNING
	_, err := n.p.Synchronize(ctx, []*api.PodSandbox{pod}, []*api.Container{c1})
	noErr(t, err)
	noErr(t, n.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	n.p.LendsRestored()
	noErr(t, n.p.RevokeNUMANode(ctx, "u1", "main", 3, "l3"))
	n.setLeases("u1", "main", 2)
	noErr(t, n.p.RevokeNUMANode(ctx, "u1", "main", 2, "l1"))
	n.expectUpdates()
	if got := n.p.LentNodes("u1", "main"); len(got) != 0 {
		t.Fatalf("lent %v after the revokes", got)
	}
	if got := n.cgroupMems("kubepods/u1/c1"); got != "" {
		t.Fatalf("cgroup mems %q after the restart, expected empty", got)
	}
	// the lent nodes are not its own, as with a cpuset.mems of its own
	if got := n.baseOf("u1", "main"); got != "0-1" {
		t.Fatalf("BaseMems %q", got)
	}
}

// TestLendNoneFencedBefore checks lends with fencing none to the
// containers that a plugin with fencing allowed-mems fenced before the
// switch to none: they keep their cpuset.mems, a lend adds the node and a
// revoke removes it again, but not a node of their claims.
func TestLendNoneFencedBefore(t *testing.T) {
	// a requester with node 2 of its lease l1, and a container with a numa
	// slice of node 2 and node 3 of its lease l2, both kept by the broker
	e := newEnvWith(t, envOptions{fencing: config.FencingNone,
		leases: []state.Lease{readyLease("l1", "u1", "main", 2), readyLease("l2", "u2", "main", 3)}})
	e.local.set(fenceClaims())
	ctx := context.Background()
	p1, p2 := testPod("u1"), testPod("u2")
	e.proc(101, "kubepods/u1/c1", "0-2", "0-2")
	c1 := withSpecMems(testCtr(p1, "c1", "main", 101), "0-2")
	c1.State = api.ContainerState_CONTAINER_RUNNING
	e.proc(201, "kubepods/u2/c2", "0-3", "0-3")
	c2 := withClaims(withSpecMems(testCtr(p2, "c2", "main", 201), "0-3"), "numa")
	c2.State = api.ContainerState_CONTAINER_RUNNING
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{p1, p2}, []*api.Container{c1, c2})
	noErr(t, err)
	// the reconcile of the broker: lent already
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	noErr(t, e.p.LendNUMANode(ctx, "u2", "main", 3, "l2"))
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates()

	// the leases are released
	e.setLeases("u1", "main", 2)
	e.setLeases("u2", "main", 3)
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-1")
	noErr(t, e.p.RevokeNUMANode(ctx, "u2", "main", 3, "l2"))
	e.expectUpdates("c2 mems=0-2")

	// a lease of the node of its claim: the node stays at the revoke, and
	// as its cgroup and spec have it, nothing is sent
	noErr(t, e.p.LendNUMANode(ctx, "u2", "main", 2, "l3"))
	e.expectUpdates()
	if got := e.p.LentNodes("u2", "main"); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("lent %v, expected [2]", got)
	}
	noErr(t, e.p.RevokeNUMANode(ctx, "u2", "main", 2, "l3"))
	e.expectUpdates()
	noErr(t, e.p.RevokeNUMANode(ctx, "u2", "main", 2, "l3"))
	e.expectUpdates()
	if got := e.p.LentNodes("u2", "main"); len(got) != 0 {
		t.Fatalf("lent %v after the revoke", got)
	}
	// a new lease of the requester
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 3, "l4"))
	e.expectUpdates("c1 mems=0-1,3")
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "main", 3, "l4"))
	e.expectUpdates("c1 mems=0-1")
	for _, c := range []struct{ rel, mems string }{{"kubepods/u1/c1", "0-1"}, {"kubepods/u2/c2", "0-2"}} {
		if got := e.cgroupMems(c.rel); got != c.mems {
			t.Errorf("%s: cgroup mems %q, expected %q", c.rel, got, c.mems)
		}
	}
}

// TestLendNonePinnedClaimed checks that with fencing none
// a revoke keeps only the claimed nodes that a pinned container had before
// any lend: a claimed node that only a lend added leaves at the revoke.
// After a plugin restart a claimed node in its cpuset.mems is its own.
func TestLendNonePinnedClaimed(t *testing.T) {
	e := newEnv(t)
	e.local.set(fenceClaims())
	ctx := context.Background()
	runClaimed := func(pod *api.PodSandbox, id string, pid uint32, mems string) {
		t.Helper()
		ctr := withClaims(withSpecMems(testCtr(pod, id, "main", pid), mems), "numa")
		_, _, err := e.p.CreateContainer(ctx, pod, ctr)
		noErr(t, err)
		e.stub.setSpecOnly(ctr.GetId(), true)
		e.proc(pid, "kubepods/"+pod.GetUid()+"/"+id, mems, mems)
		e.startCtr(pod, ctr)
	}
	// pinned to 0-1, its numa claim of node 2 not in it (fencing none
	// adds nothing)
	runClaimed(testPod("u1"), "c1", 101, "0-1")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-1")
	if got := e.cgroupMems("kubepods/u1/c1"); got != "0-1" {
		t.Fatalf("c1: cgroup mems %q, expected 0-1", got)
	}

	// pinned to 0-2 with its claimed node 2: a lease of node 2 sends
	// nothing, and the node stays at the revoke; a lease of node 3 comes
	// and goes
	runClaimed(testPod("u2"), "c2", 201, "0-2")
	noErr(t, e.p.LendNUMANode(ctx, "u2", "main", 2, "l2"))
	e.expectUpdates()
	noErr(t, e.p.RevokeNUMANode(ctx, "u2", "main", 2, "l2"))
	e.expectUpdates()
	noErr(t, e.p.LendNUMANode(ctx, "u2", "main", 3, "l3"))
	e.expectUpdates("c2 mems=0-3")
	noErr(t, e.p.RevokeNUMANode(ctx, "u2", "main", 3, "l3"))
	e.expectUpdates("c2 mems=0-2")
	if got := e.baseOf("u2", "main"); got != "0-2" {
		t.Fatalf("c2: BaseMems %q, expected 0-2", got)
	}

	// A restarted plugin: c1 has node 2 of the kept lease l1. It cannot
	// tell a lent node from an own one: the claimed node is kept.
	n := newEnvWith(t, envOptions{fencing: config.FencingNone, leases: []state.Lease{readyLease("l1", "u1", "main", 2)}})
	n.local.set(fenceClaims())
	pod := testPod("u1")
	n.proc(101, "kubepods/u1/c1", "0-2", "0-2")
	c1 := withClaims(withSpecMems(testCtr(pod, "c1", "main", 101), "0-2"), "numa")
	c1.State = api.ContainerState_CONTAINER_RUNNING
	_, err := n.p.Synchronize(ctx, []*api.PodSandbox{pod}, []*api.Container{c1})
	noErr(t, err)
	noErr(t, n.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	n.setLeases("u1", "main", 2)
	noErr(t, n.p.RevokeNUMANode(ctx, "u1", "main", 2, "l1"))
	n.expectUpdates()
	if got := n.cgroupMems("kubepods/u1/c1"); got != "0-2" {
		t.Fatalf("c1 after the restart: cgroup mems %q, expected 0-2", got)
	}
}

// TestLendNoneSkipNeedsSpec checks that an update that the
// cgroup of a pinned container has already is skipped only when its spec
// has it too: a later update of its resources would write the spec's
// cpuset.mems to the cgroup.
func TestLendNoneSkipNeedsSpec(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pod := testPod("u1")
	// a lend of the previous plugin reached the cgroup, the spec as the
	// plugin learns it has the nodes of the runtime
	e.proc(101, "kubepods/u1/c1", "0-2", "0-2")
	c1 := withSpecMems(testCtr(pod, "c1", "main", 101), "0-1")
	c1.State = api.ContainerState_CONTAINER_RUNNING
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{pod}, []*api.Container{c1})
	noErr(t, err)
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-1")
}

// TestLendNoneParentRestricts checks that a lend to an
// inheriting container whose cpuset.mems.effective lacks the node (a
// parent cgroup restricts it) is recorded, without an update, and warned
// about once per new lease.
func TestLendNoneParentRestricts(t *testing.T) {
	e := newEnv(t)
	logs := klogtest.Capture(t, 3)
	ctx := context.Background()
	const warning = "NRI lend: lend NUMA node 2 for l1: ctr=ns/pod-u1/main id=c1: its cpuset.mems.effective lacks the lent NUMA nodes 2 (a parent cgroup restricts it, or they have no memory): it cannot use them"
	// node 2 has memory, but a parent cgroup gives it 0-1 only
	e.run(testPod("u1"), "c1", "main", 101, "0-1")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates()
	if got := e.p.LentNodes("u1", "main"); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("lent %v, expected [2]", got)
	}
	out := logs()
	n := strings.Count(out, warning)
	if n == 0 {
		t.Fatalf("no warning %q:\n%s", warning, out)
	}
	// the reconcile of the broker: no warning again
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates()
	if out := logs(); strings.Count(out, warning) != n {
		t.Fatalf("warned again at a re-lend:\n%s", out)
	}
	if got := e.cgroupMems("kubepods/u1/c1"); got != "" {
		t.Fatalf("cgroup mems %q, expected empty", got)
	}
}

// TestLendsRestoredLogged checks that the plugin logs once when
// NRI has synchronized and the lends are restored, in either order, with
// fencing none too, where no fence pass runs; the readiness line comes
// after the line of the containers that Synchronize tracks, which a test
// that reads the log waits for first.
func TestLendsRestoredLogged(t *testing.T) {
	const (
		none   = "NRI fence: lends restored (fencing none)"
		fenced = "NRI fence: lends restored, fencing running containers"
		synced = "NRI Synchronize: 0 pods, 0 containers tracked"
	)
	for _, tc := range []struct {
		fencing, line, other string
		restoredFirst        bool
	}{
		{config.FencingNone, none, fenced, false},
		{config.FencingNone, none, fenced, true},
		{config.FencingAllowedMems, fenced, none, false},
		{config.FencingAllowedMems, fenced, none, true},
	} {
		t.Run(fmt.Sprintf("%s/restoredFirst=%v", tc.fencing, tc.restoredFirst), func(t *testing.T) {
			var e *env
			if tc.fencing == config.FencingAllowedMems {
				e = newFenceEnv(t, "0-2", 2)
			} else {
				e = newEnv(t)
			}
			logs := klogtest.Capture(t, 3)
			ctx := context.Background()
			if tc.restoredFirst {
				e.p.LendsRestored()
			} else {
				_, err := e.p.Synchronize(ctx, nil, nil)
				noErr(t, err)
			}
			if out := logs(); strings.Contains(out, "lends restored") {
				t.Fatalf("logged before both:\n%s", out)
			}
			if tc.restoredFirst {
				_, err := e.p.Synchronize(ctx, nil, nil)
				noErr(t, err)
			} else {
				e.p.LendsRestored()
			}
			_, err := e.p.Synchronize(ctx, nil, nil)
			noErr(t, err)
			e.p.LendsRestored()
			e.passes()
			out := logs()
			if n := strings.Count(out, tc.line); n != 1 {
				t.Errorf("%d lines %q, expected 1:\n%s", n, tc.line, out)
			}
			if strings.Contains(out, tc.other) {
				t.Errorf("line %q logged:\n%s", tc.other, out)
			}
			if i, j := strings.Index(out, synced), strings.Index(out, tc.line); i < 0 || j < i {
				t.Errorf("readiness line %q not after %q:\n%s", tc.line, synced, out)
			}
		})
	}
}

// steerable gives the cgroup dir the files that a steering manager reads
// and writes, with memory.high high.
func steerable(t *testing.T, dir, high string) {
	t.Helper()
	for name, data := range map[string]string{
		"memory.high":      high,
		"memory.current":   "4096",
		"memory.events":    "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0",
		"memory.numa_stat": "anon N0=4096 N1=0 N2=0 N3=0\nshmem N0=0 N1=0 N2=0 N3=0",
		"cgroup.procs":     "",
	} {
		noErr(t, os.WriteFile(filepath.Join(dir, name), []byte(data+"\n"), 0o644))
	}
}

// memoryHigh returns memory.high of the cgroup dir.
func memoryHigh(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "memory.high"))
	noErr(t, err)
	return strings.TrimSpace(string(data))
}
