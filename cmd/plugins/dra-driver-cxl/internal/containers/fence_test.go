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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

func TestFenceNodes(t *testing.T) {
	ctx := context.Background()
	// fencing none fences nothing
	none := newEnv(t)
	none.p.Seed([]int{2})
	none.p.Fence(ctx, 2)
	if !none.p.Fenced().IsEmpty() {
		t.Fatalf("none fences %s", none.p.Fenced())
	}

	e := newFenceEnv(t, "0-2")
	pod := testPod("u1")
	c1 := testCtr(pod, "c1", "app", 0)
	c1.State = api.ContainerState_CONTAINER_CREATED
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{pod}, []*api.Container{c1})
	noErr(t, err)
	e.p.LendsRestored()
	e.passes()
	e.p.Seed([]int{3, -1})
	e.p.Fence(ctx, -1)
	e.p.Fence(ctx, 2)
	e.expectUpdates("c1 mems=0-1")
	e.p.Fence(ctx, 2) // fenced already
	e.p.Fence(ctx, 3) // seeded
	e.expectUpdates()
	if got := e.p.Fenced().String(); got != "2-3" {
		t.Fatalf("fenced %s", got)
	}
	// without NRI the node is recorded only
	e.p.connect(nil)
	e.p.Fence(ctx, 5)
	e.expectUpdates()
	if got := e.p.Fenced().String(); got != "2-3,5" {
		t.Fatalf("fenced %s", got)
	}
	// before Start: recorded only
	n := newFenceEnv(t, "0-2")
	n.p.mu.Lock()
	n.p.stub = nil
	n.p.mu.Unlock()
	n.p.Fence(ctx, 2)
	if got := n.p.Fenced().String(); got != "2" {
		t.Fatalf("fenced %s before Start", got)
	}
}

func TestFenceWantMems(t *testing.T) {
	dir := t.TempDir()
	hasMem := filepath.Join(dir, "has_memory")
	noErr(t, os.WriteFile(hasMem, []byte("0-2\n"), 0o644))
	for _, tc := range []struct {
		mems    string // of the spec
		claimed cpuset.CPUSet
		lent    cpuset.CPUSet
		path    string
		want    string // "" for an error
	}{
		{"", cpuset.New(), cpuset.New(), hasMem, "0-1"},
		{"0", cpuset.New(), cpuset.New(), hasMem, "0"},
		{"0-2", cpuset.New(), cpuset.New(), hasMem, "0-1"},
		{"", cpuset.New(), cpuset.New(2), hasMem, "0-2"},
		{"0-1,2", cpuset.New(), cpuset.New(2), hasMem, "0-2"},
		{"", cpuset.New(3), cpuset.New(), hasMem, "0-1,3"},
		{"1", cpuset.New(3), cpuset.New(2), hasMem, "1-3"},
		{"0", cpuset.New(), cpuset.New(), filepath.Join(dir, "none"), "0"},
		{"", cpuset.New(), cpuset.New(), filepath.Join(dir, "none"), ""},
		{"2", cpuset.New(), cpuset.New(), hasMem, ""},
		{"x", cpuset.New(), cpuset.New(), hasMem, ""},
	} {
		c := &Container{SpecMems: tc.mems, Claimed: tc.claimed, Management: ManagementFenced}
		for _, n := range tc.lent.List() {
			c.Lends = c.Lends.set(n, map[string]bool{"l": true})
		}
		got, err := passMems(c, cpuset.New(2, 3), &memNodes{path: tc.path})
		if (err != nil) != (tc.want == "") || got.String() != tc.want {
			t.Errorf("spec %q claimed %s lent %s: %s %v, expected %q", tc.mems, tc.claimed, tc.lent, got, err, tc.want)
		}
	}
	mem := &memNodes{path: hasMem}
	for _, tc := range []struct {
		name       string
		c          *Container
		changed    int
		lent       cpuset.CPUSet
		want, base string // want "-" for none
	}{
		// a lend change takes the node off the base of a fenced container
		{"fenced", &Container{Management: ManagementFenced, SpecMems: "0-2"}, 2, cpuset.New(), "0-1", "0-1"},
		{"fenced, lent", &Container{Management: ManagementFenced, SpecMems: "0-2", Claimed: cpuset.New(4)}, 2, cpuset.New(2), "0-2,4", "0-1"},
		// fencing none with a cpuset.mems of its own: never below the
		// claimed nodes that it had before any lend
		{"pinned", &Container{Management: ManagementUnfenced, SpecMems: "0-2", Base: cpuset.New(0, 1, 2), OwnClaimed: cpuset.New(2)}, 2,
			cpuset.New(3), "0-3", "0-2"},
		{"pinned, revoke", &Container{Management: ManagementUnfenced, SpecMems: "0-3", Base: cpuset.New(0, 1, 3)}, 3, cpuset.New(),
			"0-1", "0-1"},
		// fencing none, inheriting: no value, the base without the node
		{"inherits", &Container{Management: ManagementUnfenced, Base: cpuset.New(0, 1, 2)}, 2, cpuset.New(2), "-", "0-1"},
		{"steered", &Container{Management: ManagementSteered, SpecMems: "0", Base: cpuset.New(0, 2)}, 2, cpuset.New(2), "-", "0,2"},
	} {
		want, base, ok, err := wantMems(tc.c, cpuset.New(3), tc.changed, tc.lent, mem)
		got := want.String()
		if !ok {
			got = "-"
		}
		if err != nil || got != tc.want || base.String() != tc.base {
			t.Errorf("%s: %s base %s %v, expected %s base %s", tc.name, got, base, err, tc.want, tc.base)
		}
	}
}

func TestFenceCreateContainer(t *testing.T) {
	e := newFenceEnv(t, "0-2")
	e.local.set(fenceClaims())
	e.pool.set(claimTable{"pool": {ClaimName: "ns/pool", Devices: []state.DeviceInfo{
		{DeviceType: device.DeviceTypeMemdev, Mode: cxlapi.ModeNUMA, NUMANodes: []int{3}, ConsumedBytes: 1 << 30},
	}}})
	pod := testPod("u1")

	// nothing fenced: no adjustment
	if got := e.createMems(pod, testCtr(pod, "c0", "a", 0)); got != "-" || e.specOf("c0") != "" {
		t.Fatalf("adjustment %s, mems %q without fenced nodes", got, e.specOf("c0"))
	}

	e.p.Seed([]int{2, 3})
	for _, tc := range []struct {
		ctr  *api.Container
		want string // adjustment, "-" for none
		mems string // recorded
	}{
		{testCtr(pod, "c1", "a", 0), "0-1", "0-1"},
		{withSpecMems(testCtr(pod, "c2", "a", 0), "0-1"), "-", "0-1"},
		{withSpecMems(testCtr(pod, "c3", "a", 0), "0-2"), "0-1", "0-1"},
		{withSpecMems(testCtr(pod, "c4", "a", 0), "1"), "-", "1"},
		// steered: left to its steering manager
		{withClaims(testCtr(pod, "c5", "a", 0), "dram-auto"), "-", ""},
		{withClaims(testCtr(pod, "c6", "a", 0), "numa-auto", "dax"), "-", ""},
		// not steered: DRAM is capacity only, numa slices and regions
		// give their nodes
		{withClaims(testCtr(pod, "c5n", "a", 0), "dram"), "0-1", "0-1"},
		{withClaims(testCtr(pod, "c6n", "a", 0), "numa", "dax"), "0-2", "0-2"},
		{withClaims(testCtr(pod, "c6r", "a", 0), "region"), "0-1,4", "0-1,4"},
		// dax claims are no memory nodes
		{withClaims(testCtr(pod, "c7", "a", 0), "dax"), "0-1", "0-1"},
		// pool numa claims get their node
		{withClaims(testCtr(pod, "c8", "a", 0), "pool"), "0-1,3", "0-1,3"},
		{withClaims(withSpecMems(testCtr(pod, "c9", "a", 0), "0-1,3"), "pool"), "-", "0-1,3"},
		// an unknown claim
		{withClaims(testCtr(pod, "c10", "a", 0), "gone"), "0-1", "0-1"},
	} {
		id := tc.ctr.GetId()
		if got := e.createMems(pod, tc.ctr); got != tc.want {
			t.Errorf("%s: adjustment %s, expected %s", id, got, tc.want)
		}
		if got := e.specOf(id); got != tc.mems {
			t.Errorf("%s: recorded mems %q, expected %q", id, got, tc.mems)
		}
	}

	// own nodes unknown: no adjustment
	e.hasMemory("")
	if got := e.createMems(pod, testCtr(pod, "c11", "a", 0)); got != "-" {
		t.Fatalf("adjustment %s without nodes with memory", got)
	}
	e.passes()
	e.expectUpdates()

	// fencing none
	n := newEnv(t)
	if got := n.createMems(pod, withSpecMems(testCtr(pod, "c1", "a", 0), "0-2")); got != "-" {
		t.Fatalf("adjustment %s in mode none", got)
	}
}

func TestFencePass(t *testing.T) {
	e := newFenceEnv(t, "0-2", 2)
	e.local.set(fenceClaims())
	ctx := context.Background()
	p1, p2 := testPod("u1"), testPod("u2")
	e.proc(101, "kubepods/u1/c1", "0-2", "")
	e.proc(102, "kubepods/u1/c3", "0-2", "")
	c1 := testCtr(p1, "c1", "app", 101)
	c1.State = api.ContainerState_CONTAINER_RUNNING
	c2 := testCtr(p1, "c2", "init", 0)
	c2.State = api.ContainerState_CONTAINER_CREATED
	c3 := withClaims(testCtr(p1, "c3", "steered", 102), "dram-auto")
	c3.State = api.ContainerState_CONTAINER_RUNNING
	c4 := withSpecMems(testCtr(p2, "c4", "pinned", 0), "0")
	c4.State = api.ContainerState_CONTAINER_CREATED
	c5 := testCtr(p2, "c5", "flaky", 0)
	c5.State = api.ContainerState_CONTAINER_CREATED
	ctrs := []*api.Container{c1, c2, c3, c4, c5}
	// not ready before the lends are restored: Fence only records
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{p1, p2}, ctrs)
	noErr(t, err)
	e.passes()
	e.p.Fence(ctx, 3)
	e.expectUpdates()
	if got := e.p.Fenced().String(); got != "2-3" {
		t.Fatalf("fenced %s", got)
	}

	// running and created containers, also c3 that its settings steer (no
	// manager of this process steers it), not the one whose nodes do not
	// change; a failed update is tried again
	e.stub.setFailIDs("c5")
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates("c1 mems=0-1", "c2 mems=0-1", "c3 mems=0-1")
	if got := e.specOf("c5"); got != "" {
		t.Fatalf("failed update recorded: %q", got)
	}
	e.stub.setFailIDs()
	e.p.LendsRestored() // once only
	e.passes()
	e.expectUpdates()
	// the periodic repair pass tries the failed update again
	e.p.Repair(ctx)
	e.expectUpdates("c5 mems=0-1")
	e.p.Repair(ctx)
	e.expectUpdates()
	e.p.Fence(ctx, 4)
	e.expectUpdates()

	// an error of UpdateContainers fails all, the next pass repeats
	e.setCgroupMems("kubepods/u1/c1", "0-2")
	e.stub.setErr(errors.New("ttrpc closed"), false)
	e.p.Repair(ctx)
	e.expectUpdates()
	e.stub.setErr(nil, false)
	e.p.Fence(ctx, 5)
	e.expectUpdates("c1 mems=0-1")

	// a running container whose cgroup has want but whose spec does not
	// is updated (want differs from its spec): a later update of its
	// resources would write the spec's cpuset.mems to the cgroup
	e.setSpecMems("c1", "0-2")
	e.p.Repair(ctx)
	e.expectUpdates("c1 mems=0-1")
	e.p.Repair(ctx)
	e.expectUpdates()

	// a new container is fenced at creation, the pass leaves it be
	if got := e.createMems(p2, testCtr(p2, "c6", "new", 0)); got != "0-1" {
		t.Fatalf("adjustment %s", got)
	}
	e.p.Fence(ctx, 6)
	e.expectUpdates()
}

func TestFenceLend(t *testing.T) {
	e := newFenceEnv(t, "0-2", 2)
	ctx := context.Background()
	e.readyFence()
	// fenced at creation
	e.run(testPod("u1"), "c1", "app", 101, "0-2")
	if got := e.specOf("c1"); got != "0-1" {
		t.Fatalf("mems %q after creation", got)
	}
	e.expectUpdates()

	// the DRAM nodes and the lent node, back to the DRAM nodes
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 2, "l2"))
	e.expectUpdates()
	e.check()
	if got := e.baseOf("u1", "app"); got != "0-1" || e.specOf("c1") != "0-2" {
		t.Fatalf("BaseMems %q, mems %q", got, e.specOf("c1"))
	}
	// a pass keeps the lent node
	e.p.Fence(ctx, 3)
	e.expectUpdates()
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "app", 2, "l1"))
	e.expectUpdates()
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "app", 2, "l2"))
	e.expectUpdates("c1 mems=0-1")
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "app", 2, "l2"))
	e.expectUpdates()

	// own nodes unknown
	e.hasMemory("")
	e.run(testPod("u2"), "c2", "app", 201, "0-2")
	if err := e.p.LendNUMANode(ctx, "u2", "app", 2, "l1"); err == nil || !strings.Contains(err.Error(), "base memory nodes unknown") {
		t.Fatalf("lend without own nodes: %v", err)
	}
	e.expectUpdates()

}

// TestFenceRestart checks that a restarted plugin keeps the lent node of a
// requester: the lend is re-applied before the first pass.
func TestFenceRestart(t *testing.T) {
	e := newFenceEnv(t, "0-2", 2)
	ctx := context.Background()
	p1, p2 := testPod("u1"), testPod("u2")
	e.proc(101, "kubepods/u1/c1", "0-2", "0-2")
	e.proc(201, "kubepods/u2/c2", "0-2", "")
	c1 := withSpecMems(testCtr(p1, "c1", "app", 101), "0-2")
	c1.State = api.ContainerState_CONTAINER_RUNNING
	c2 := testCtr(p2, "c2", "app", 201)
	c2.State = api.ContainerState_CONTAINER_RUNNING
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{p1, p2}, []*api.Container{c1, c2})
	noErr(t, err)
	e.passes()
	e.expectUpdates()
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 2, "l1"))
	e.expectUpdates()
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates("c2 mems=0-1")
}

// TestFenceConcurrent checks that a Fence of a node that another Fence is
// fencing returns only after the containers have been updated.
func TestFenceConcurrent(t *testing.T) {
	e := newFenceEnv(t, "0-2")
	ctx := context.Background()
	p := testPod("u1")
	c1 := testCtr(p, "c1", "app", 0)
	c1.State = api.ContainerState_CONTAINER_CREATED
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{p}, []*api.Container{c1})
	noErr(t, err)
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates()

	e.stub.closeGate(t)
	first, second, other := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { e.p.Fence(ctx, 2); close(first) }()
	<-e.stub.entered
	go func() { e.p.Fence(ctx, 2); close(second) }()
	go func() { e.p.Fence(ctx, 3); close(other) }()
	select {
	case <-second:
		t.Fatal("the second Fence returned before the update of the first")
	case <-other:
		t.Fatal("a Fence of another node returned before the update of the first")
	case <-time.After(50 * time.Millisecond):
	}
	e.stub.openGate()
	<-first
	<-second
	<-other
	e.expectUpdates("c1 mems=0-1")
}

// TestFenceDeadline checks that a Fence returns at the end of its context,
// while the pass that it waits for still runs: the engine onlines the
// memory anyway after its deadline; the node is recorded and the
// containers updated in the background.
func TestFenceDeadline(t *testing.T) {
	e := newFenceEnv(t, "0-2")
	e.readyFence()
	pod := testPod("u1")
	c1 := testCtr(pod, "c1", "app", 0)
	if got := e.createMems(pod, c1); got != "-" {
		t.Fatalf("adjustment %s", got)
	}
	// a Fence waits for the one running, until its deadline
	e.stub.closeGate(t)
	first := make(chan struct{})
	go func() { e.p.Fence(context.Background(), 2); close(first) }()
	<-e.stub.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second := make(chan struct{})
	go func() { e.p.Fence(ctx, 3); close(second) }()
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("a Fence did not return at its deadline while a pass runs")
	}
	select {
	case <-first:
		t.Fatal("the first Fence returned before its update")
	default:
	}
	e.stub.openGate()
	<-first
	e.passes()
	e.expectUpdates("c1 mems=0-1")
	if got := e.p.Fenced().String(); got != "2-3" {
		t.Fatalf("fenced %s", got)
	}
	// a Fence whose context is done before it starts: the node recorded
	// and the containers updated in the background
	if got := e.createMems(pod, withSpecMems(testCtr(pod, "c2", "app2", 0), "0-5")); got != "0-1,4-5" {
		t.Fatalf("adjustment %s", got)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	e.p.Fence(done, 5)
	e.passes()
	e.expectUpdates("c2 mems=0-1,4")
	if got := e.p.Fenced().String(); got != "2-3,5" {
		t.Fatalf("fenced %s", got)
	}
}

// TestFenceMissedAtStart checks that a container whose update failed
// before the runtime knew it gets the update at its start (the repair
// pass).
func TestFenceMissedAtStart(t *testing.T) {
	e := newFenceEnv(t, "0-2")
	ctx := context.Background()
	e.readyFence()
	pod := testPod("u1")
	e.proc(101, "kubepods/u1/c1", "0-2", "")
	c1 := testCtr(pod, "c1", "app", 101)
	if got := e.createMems(pod, c1); got != "-" {
		t.Fatalf("adjustment %s without fenced nodes", got)
	}
	// created, not in the runtime's store yet
	e.stub.setFailIDs("c1")
	e.p.Fence(ctx, 2)
	e.expectUpdates()
	e.stub.setFailIDs()
	e.startCtr(pod, c1)
	e.passes()
	e.expectUpdates("c1 mems=0-1")

	// fenced at creation: nothing at the start
	e.proc(102, "kubepods/u1/c2", "0-1", "0-1")
	c2 := testCtr(pod, "c2", "app2", 102)
	if got := e.createMems(pod, c2); got != "0-1" {
		t.Fatalf("adjustment %s", got)
	}
	e.startCtr(pod, c2)
	e.passes()
	e.expectUpdates()
	e.check()
}

// TestLendRestartedRequester checks that a requester container
// that restarts while the broker keeps its numa lease is created with the
// node of the lease: an update at its start would reach its spec only.
func TestLendRestartedRequester(t *testing.T) {
	e := newFenceEnv(t, "0-2", 2)
	ctx := context.Background()
	e.readyFence()
	pod := testPod("u1")
	c1 := e.run(pod, "c1", "main", 101, "0-1")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")
	e.setLeases("u1", "main", 2, "l1")

	// the workload crashes, the broker keeps the lease for the restart
	_, err := e.p.StopContainer(ctx, pod, c1)
	noErr(t, err)
	e.wait(e.gone, "u1", "main")
	c2 := testCtr(pod, "c2", "main", 102)
	if got := e.createMems(pod, c2); got != "0-2" {
		t.Fatalf("adjustment %s of the restarted requester, expected the lent node", got)
	}
	if got := e.p.LentNodes("u1", "main"); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("lent %v at the creation", got)
	}
	// the reconcile of the broker before its start: done
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	// its task is created from the spec; then the broker lends again
	e.proc(102, "kubepods/u1/c2", "0-2", "0-2")
	e.startCtr(pod, c2)
	e.passes()
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates()
	e.check()
	if got := e.baseOf("u1", "main"); got != "0-1" {
		t.Fatalf("BaseMems %q has the lent node", got)
	}
	// a pass keeps the node; other containers, as the companion pod's,
	// are not lent to
	e.p.Fence(ctx, 3)
	e.expectUpdates()
	cp := testPod("u9")
	if got := e.createMems(cp, testCtr(cp, "c9", "pause", 0)); got != "0-1" {
		t.Fatalf("adjustment %s of the companion", got)
	}
	// the lease is released
	e.setLeases("u1", "main", 2)
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c2 mems=0-1")
	if got := e.cgroupMems("kubepods/u1/c2"); got != "0-1" {
		t.Fatalf("cgroup mems %s after the release", got)
	}

	// released while the restarted requester is created: off its spec
	e.setLeases("u1", "main", 2, "l2")
	c3 := testCtr(pod, "c3", "main", 0)
	if got := e.createMems(pod, c3); got != "0-2" {
		t.Fatalf("adjustment %s", got)
	}
	e.setLeases("u1", "main", 2)
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "main", 2, "l2"))
	e.expectUpdates("c3 mems=0-1")
	if got := e.p.LentNodes("u1", "main"); len(got) != 0 || e.specOf("c3") != "0-1" {
		t.Fatalf("lent %v, mems %q after the revoke", got, e.specOf("c3"))
	}
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "main", 2, "l2"))
	if err := e.p.LendNUMANode(ctx, "u1", "main", 2, "l2"); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("lend before the start: %v", err)
	}
	e.expectUpdates()

	// steered: not lent to at its creation
	e.local.set(fenceClaims())
	sp := testPod("u2")
	e.setLeases("u2", "main", 2, "l3")
	if got := e.createMems(sp, withClaims(testCtr(sp, "c4", "main", 0), "numa-auto")); got != "-" {
		t.Fatalf("adjustment %s of a steered container", got)
	}
	if got := e.p.LentNodes("u2", "main"); len(got) != 0 {
		t.Fatalf("lent %v to a steered container", got)
	}

	// without the lease broker (--leases=false) the leases of the store
	// seed nothing
	off := newEnvWith(t, envOptions{noLeases: true, leases: []state.Lease{readyLease("l5", "u4", "main", 2)}})
	off.p.Seed([]int{2})
	off.hasMemory("0-2")
	op := testPod("u4")
	if got := off.createMems(op, testCtr(op, "c6", "main", 0)); got != "0-1" || len(off.p.LentNodes("u4", "main")) != 0 {
		t.Fatalf("adjustment %s without the broker", got)
	}

	// not fenced: lent by updates after its start only
	n := newEnvWith(t, envOptions{fencing: config.FencingNone, leases: []state.Lease{readyLease("l4", "u3", "main", 2)}})
	np := testPod("u3")
	if got := n.createMems(np, testCtr(np, "c5", "main", 0)); got != "-" {
		t.Fatalf("adjustment %s in mode none", got)
	}
	noErr(t, n.p.RevokeNUMANode(ctx, "u3", "main", 2, "l4"))
	if got := n.p.LentNodes("u3", "main"); len(got) != 0 {
		t.Fatalf("lent %v after the revoke", got)
	}
	n.expectUpdates()
}

// TestLendUpdateBeforeStart checks that a lend whose update reached only
// the spec, as an update before containerd has marked the container
// started, is applied again: by the pass at the start of a fenced
// container, by the next lend (the reconcile of the broker) of one that is
// not fenced.
func TestLendUpdateBeforeStart(t *testing.T) {
	e := newFenceEnv(t, "0-2", 2)
	ctx := context.Background()
	e.readyFence()
	pod := testPod("u1")
	c1 := testCtr(pod, "c1", "main", 101)
	if got := e.createMems(pod, c1); got != "0-1" {
		t.Fatalf("adjustment %s", got)
	}
	e.proc(101, "kubepods/u1/c1", "0-1", "0-1")
	noErr(t, e.p.StartContainer(ctx, pod, c1))
	e.noCallback(e.start)
	if c := e.record("c1"); c.Lifecycle != LifecycleTaskCreated {
		t.Fatalf("lifecycle %s", c.Lifecycle)
	}
	e.stub.setSpecOnly("c1", true)
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")
	if got := e.cgroupMems("kubepods/u1/c1"); got != "0-1" {
		t.Fatalf("cgroup mems %s", got)
	}
	e.startCtr(pod, c1)
	e.passes()
	e.expectUpdates("c1 mems=0-2")
	if got := e.cgroupMems("kubepods/u1/c1"); got != "0-2" {
		t.Fatalf("cgroup mems %s after the start", got)
	}
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates()
	// lost after the start (no such runtime): the next lend repeats it
	e.setCgroupMems("kubepods/u1/c1", "0-1")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")

	n := newEnv(t)
	np := testPod("u2")
	c2 := withSpecMems(testCtr(np, "c2", "main", 201), "0-1")
	if got := n.createMems(np, c2); got != "-" {
		t.Fatalf("adjustment %s in mode none", got)
	}
	n.proc(201, "kubepods/u2/c2", "0-1", "0-1")
	noErr(t, n.p.StartContainer(ctx, np, c2))
	n.stub.setSpecOnly("c2", true)
	noErr(t, n.p.LendNUMANode(ctx, "u2", "main", 2, "l1"))
	n.expectUpdates("c2 mems=0-2")
	n.startCtr(np, c2)
	n.expectUpdates()
	noErr(t, n.p.LendNUMANode(ctx, "u2", "main", 2, "l1"))
	n.expectUpdates("c2 mems=0-2")
	if got := n.cgroupMems("kubepods/u2/c2"); got != "0-2" {
		t.Fatalf("cgroup mems %s", got)
	}
	noErr(t, n.p.LendNUMANode(ctx, "u2", "main", 2, "l1"))
	n.expectUpdates()
}

// TestFenceSynchronizeWhileCreating checks that the first
// pass of a plugin that synchronized while the runtime was creating the
// task of a container is applied at the start of the container: the
// update reached its spec only, after the task had read it.
func TestFenceSynchronizeWhileCreating(t *testing.T) {
	e := newFenceEnv(t, "0-2", 2)
	ctx := context.Background()
	pod := testPod("u1")
	c1 := testCtr(pod, "c1", "main", 0)
	c1.State = api.ContainerState_CONTAINER_CREATED
	e.stub.setSpecOnly("c1", true)
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{pod}, []*api.Container{c1})
	noErr(t, err)
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates("c1 mems=0-1")
	// the task has every node
	e.proc(101, "kubepods/u1/c1", "0-2", "")
	c1.Pid = 101
	noErr(t, e.p.StartContainer(ctx, pod, c1))
	e.passes()
	e.expectUpdates()
	e.stub.setSpecOnly("c1", false)
	noErr(t, e.p.PostStartContainer(ctx, pod, c1))
	e.wait(e.start, "u1", "main")
	e.passes()
	e.expectUpdates("c1 mems=0-1")
	if got := e.cgroupMems("kubepods/u1/c1"); got != "0-1" {
		t.Fatalf("cgroup mems %s", got)
	}
	e.p.Fence(ctx, 3)
	e.expectUpdates()
	// a PostStartContainer of a container that is gone
	_, err = e.p.StopContainer(ctx, pod, c1)
	noErr(t, err)
	e.wait(e.gone, "u1", "main")
	noErr(t, e.p.PostStartContainer(ctx, pod, c1))
	e.noCallback(e.start)
}

// TestLendSynchronizeKept checks that a restarted plugin gives a container
// that is new to it the lends of its kept leases: a requester created with
// the node of its lease by the previous plugin keeps it through the first
// pass, before its start.
func TestLendSynchronizeKept(t *testing.T) {
	e := newEnvWith(t, envOptions{leases: []state.Lease{readyLease("l1", "u1", "main", 2), readyLease("l2", "u2", "main", 2)}})
	e.p.Seed([]int{2})
	e.hasMemory("0-2")
	ctx := context.Background()
	pod := testPod("u1")
	c2 := withSpecMems(testCtr(pod, "c2", "main", 0), "0-2")
	c2.State = api.ContainerState_CONTAINER_CREATED
	// a running requester whose cgroup lacks the node
	other := testPod("u2")
	e.proc(201, "kubepods/u2/c3", "0-1", "0-1")
	c3 := withSpecMems(testCtr(other, "c3", "main", 201), "0-2")
	c3.State = api.ContainerState_CONTAINER_RUNNING
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{pod, other}, []*api.Container{c2, c3})
	noErr(t, err)
	// the reconcile of the broker, then the first pass
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	noErr(t, e.p.LendNUMANode(ctx, "u2", "main", 2, "l2"))
	e.expectUpdates("c3 mems=0-2")
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates()
	if got := e.p.LentNodes("u1", "main"); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("lent %v", got)
	}
	if got := e.baseOf("u2", "main"); got != "0-1" {
		t.Fatalf("BaseMems %q", got)
	}
	e.proc(102, "kubepods/u1/c2", "0-2", "0-2")
	c2.Pid = 102
	e.startCtr(pod, c2)
	e.passes()
	e.expectUpdates()
}

// The init process of a running container is in a child cgroup of the
// container's (systemd in the container moves itself to init.scope) when
// a restarted plugin synchronizes. The plugin takes the container's own
// cgroup, which the runtime updates, so an update is sent once, not again
// at every pass and every lend for the lifetime of the container with an
// "updating again" line each time. A lease requester in
// another child cgroup of the container is found in it.
func TestFenceContainerCgroupAboveInit(t *testing.T) {
	e := newFenceEnv(t, "0-2", 2)
	logs := klogtest.Capture(t, 3)
	ctx := context.Background()
	pod := testPod("u1")
	parent := filepath.Join(cgroupRoot, "kubepods/u1/c1")
	child := e.proc(101, "kubepods/u1/c1/init.scope", "0-2", "")
	noErr(t, os.WriteFile(filepath.Join(parent, "cpuset.mems"), []byte("\n"), 0o644))
	noErr(t, os.WriteFile(filepath.Join(parent, "cpuset.mems.effective"), []byte("0-2\n"), 0o644))
	// runc writes the container's cgroup; the kernel recomputes the
	// child's effective set, its cpuset.mems stays ""
	e.stub.applied = func(id, mems string) {
		_ = os.WriteFile(filepath.Join(parent, "cpuset.mems"), []byte(mems+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(parent, "cpuset.mems.effective"), []byte(mems+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(child, "cpuset.mems.effective"), []byte(mems+"\n"), 0o644)
	}
	c1 := testCtr(pod, "c1", "main", 101)
	c1.State = api.ContainerState_CONTAINER_RUNNING
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{pod}, []*api.Container{c1})
	noErr(t, err)
	if dir := e.record("c1").CgroupDir; dir != parent {
		t.Fatalf("cgroup %s, expected the container's %s", dir, parent)
	}
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates("c1 mems=0-1")
	// a later pass (any Fence of a new node) finds it fenced
	e.p.pass(ctx, "")
	e.expectUpdates()
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c1 mems=0-2")
	for i := 0; i < 3; i++ { // three reconciles
		noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
		e.expectUpdates()
	}
	if n := strings.Count(logs(), "updating again"); n != 0 {
		t.Errorf("%d \"updating again\" lines; parent cpuset.mems %q", n, e.cgroupMems("kubepods/u1/c1"))
	}
	e.proc(201, "kubepods/u1/c1/app.scope", "0-2", "")
	if ref, ok := e.byPid(201); !ok || ref.ID != "c1" {
		t.Errorf("a process in another child cgroup: %+v %v", ref, ok)
	}
}

func TestContainerCgroupDir(t *testing.T) {
	old := cgroupRoot
	cgroupRoot = "/sys/fs/cgroup"
	t.Cleanup(func() { cgroupRoot = old })
	const id = "4bb166f8e2"
	scope := "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podu1.slice/cri-containerd-" + id + ".scope"
	for _, tc := range []struct{ dir, id, want string }{
		{scope, id, scope},
		{scope + "/init.scope", id, scope},
		{"/sys/fs/cgroup/kubepods/burstable/podu1/" + id + "/a/b", id, "/sys/fs/cgroup/kubepods/burstable/podu1/" + id},
		{"/sys/fs/cgroup/system.slice/crio-" + id + ".scope/container", id, "/sys/fs/cgroup/system.slice/crio-" + id + ".scope"},
		{"/sys/fs/cgroup/kubepods/podu1/other/init.scope", id, "/sys/fs/cgroup/kubepods/podu1/other/init.scope"},
		{scope + "/init.scope", "", scope + "/init.scope"},
		{"/elsewhere/" + id + "/init.scope", id, "/elsewhere/" + id + "/init.scope"},
	} {
		if got := containerCgroupDir(tc.dir, tc.id); got != tc.want {
			t.Errorf("containerCgroupDir(%s, %q) = %s, expected %s", tc.dir, tc.id, got, tc.want)
		}
	}
	// the cgroup of a pid
	e := newEnv(t)
	e.proc(7, "kubepods/u1/c7", "0", "")
	if dir, err := pidCgroupDir(7); err != nil || dir != filepath.Join(cgroupRoot, "kubepods/u1/c7") {
		t.Errorf("pidCgroupDir: %s %v", dir, err)
	}
	noErr(t, os.WriteFile(filepath.Join(procDir, "7", "cgroup"), []byte("1:cpu:/x\n"), 0o644))
	if _, err := pidCgroupDir(7); err == nil {
		t.Error("pidCgroupDir of a cgroup v1 line")
	}
	if _, err := resolveContainerCgroupDir(&api.Container{Id: "c7", Pid: 7}); err == nil || !strings.Contains(err.Error(), "no cgroup v2 entry found") {
		t.Errorf("resolveContainerCgroupDir of a cgroup v1 line: %v", err)
	}
	if _, err := resolveContainerCgroupDir(&api.Container{Id: "c7"}); err == nil {
		t.Error("resolveContainerCgroupDir of pid 0")
	}
}

// fencing none, a requester restarts with its kept lease and a spec pinned
// to its DRAM nodes (kubelet memory manager): the first lend after its
// start is not logged as a lost update ("updating again"): nothing was sent
// before.
func TestFenceNoneFirstLendNotUpdatingAgain(t *testing.T) {
	e := newEnvWith(t, envOptions{fencing: config.FencingNone, leases: []state.Lease{readyLease("l1", "u1", "main", 2)}})
	logs := klogtest.Capture(t, 3)
	ctx := context.Background()
	pod := testPod("u1")
	c2 := withSpecMems(testCtr(pod, "c2", "main", 102), "0-1")
	if got := e.createMems(pod, c2); got != "-" {
		t.Fatalf("adjustment %s", got)
	}
	e.proc(102, "kubepods/u1/c2", "0-1", "0-1")
	e.startCtr(pod, c2)
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1")) // the broker at PostStart
	e.expectUpdates("c2 mems=0-2")
	// a lost update is: the cgroup loses the node again
	e.setCgroupMems("kubepods/u1/c2", "0-1")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	e.expectUpdates("c2 mems=0-2")
	if n := strings.Count(logs(), "updating again"); n != 1 {
		t.Errorf("%d \"updating again\" lines, expected 1 (the lost update):\n%s", n, logs())
	}
}

// TestCharFenceLogLines pins the lines of a fence that adds a node, of one
// without NRI and of the seed of the prepared claims.
func TestCharFenceLogLines(t *testing.T) {
	e := newFenceEnv(t, "0-2")
	logs := klogtest.Capture(t, 3)
	ctx := context.Background()
	e.readyFence()
	e.p.Fence(ctx, 3)
	const fenced = "NRI fence: fencing CXL NUMA node 3 (fenced NUMA nodes 3)"
	if n := strings.Count(logs(), fenced); n != 1 {
		t.Fatalf("%d lines %q, expected 1:\n%s", n, fenced, logs())
	}
	e.p.Fence(ctx, 3)
	if n := strings.Count(logs(), "NRI fence: fencing CXL NUMA node"); n != 1 {
		t.Fatalf("a fenced node logged again:\n%s", logs())
	}

	n := newFenceEnv(t, "0-2")
	n.p.connect(nil)
	n.p.Fence(ctx, 3)
	const noNRI = "NRI fence: NRI not connected, containers are not fenced from CXL NUMA node 3"
	if !strings.Contains(logs(), noNRI) {
		t.Fatalf("no line %q:\n%s", noNRI, logs())
	}
	n.p.Seed([]int{2})
	line := lineWith(logs(), "of prepared claims")
	if !strings.HasSuffix(line, "NRI fence: fencing CXL NUMA nodes 2 of prepared claims") {
		t.Fatalf("seed line %q:\n%s", line, logs())
	}
}
