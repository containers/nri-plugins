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
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/containerd/nri/pkg/api"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/steering"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
)

// managements returns the management and the claimed nodes of the
// records of the table.
func (e *env) managements() map[string]string {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	got := map[string]string{}
	for id, c := range e.p.ctrs {
		got[id] = c.Management.text() + " " + c.Claimed.String()
	}
	return got
}

// managers returns the ids of the containers that a manager steers.
func (e *env) managers() []string {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	var ids []string
	for id, c := range e.p.ctrs {
		if c.manager != nil || c.Plan != nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// TestLendToUnsteeredNUMAClaim lends a node to a container whose static
// numa claim is not steered, and refuses a lend to one that is.
func TestLendToUnsteeredNUMAClaim(t *testing.T) {
	e := newFenceEnv(t, "0-1", 2)
	e.local.set(nriClaims())
	ctx := context.Background()
	e.readyFence()
	pod := testPod("u1")
	e.proc(101, "kubepods/u1/c1", "0-2", "0-2") // as runc creates it
	c1 := withClaims(testCtr(pod, "c1", "app", 101), "split")
	if got := e.createMems(pod, c1); got != "0-2" {
		t.Fatalf("adjustment %s", got)
	}
	e.startCtr(pod, c1)
	e.passes()
	e.expectUpdates()
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 3, "l1"))
	e.expectUpdates("c1 mems=0-3")
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "app", 3, "l1"))
	e.expectUpdates("c1 mems=0-2")

	e.proc(102, "kubepods/u1/c2", "0-2", "")
	c2 := withClaims(testCtr(pod, "c2", "steered", 102), "auto")
	if got := e.createMems(pod, c2); got != "-" {
		t.Fatalf("adjustment %s of a steered container", got)
	}
	err := e.p.LendNUMANode(ctx, "u1", "steered", 3, "l1")
	if !errors.Is(err, ErrSteered) || !strings.Contains(err.Error(), "lend NUMA node 3 to ns/pod-u1/steered: container is memory-steered") {
		t.Fatalf("lend to a steered container: %v", err)
	}
	e.expectUpdates()
}

// TestSynchronizeManagement checks the management of the containers that
// a restarted plugin learns from Synchronize.
func TestSynchronizeManagement(t *testing.T) {
	e := newFenceEnv(t, "0-1", 2)
	e.local.set(nriClaims())
	e.pool.set(nriPoolResults())
	ctx := context.Background()
	p1 := testPod("u1")
	var ctrs []*api.Container
	for _, c := range []struct {
		id    string
		pid   uint32
		claim string
	}{{"c1", 101, "auto"}, {"c2", 102, "split"}, {"c3", 103, "pool"}, {"c4", 104, ""}} {
		e.proc(c.pid, "kubepods/u1/"+c.id, "0-2", "")
		ctr := testCtr(p1, c.id, c.id, c.pid)
		if c.claim != "" {
			withClaims(ctr, c.claim)
		}
		ctr.State = api.ContainerState_CONTAINER_RUNNING
		ctrs = append(ctrs, ctr)
	}
	// settings that conflict (the claims changed): taken as steered
	conflict := withClaims(testCtr(p1, "c5", "c5", 0), "auto", "none")
	conflict.State = api.ContainerState_CONTAINER_RUNNING
	ctrs = append(ctrs, conflict)
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{p1}, ctrs)
	noErr(t, err)
	// c1, that its settings steer, is managed like a container that is not
	// steered
	want := map[string]string{"c1": "fenced 2", "c2": "fenced 2", "c3": "fenced 3", "c4": "fenced ", "c5": "steered "}
	if got := e.managements(); !reflect.DeepEqual(got, want) {
		t.Fatalf("management %v, expected %v", got, want)
	}
	// managers are not restarted
	if ids := e.managers(); len(ids) != 0 {
		t.Fatalf("managers %v", ids)
	}
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates("c1 mems=0-2", "c2 mems=0-2", "c3 mems=0-1,3", "c4 mems=0-1")
	noErr(t, e.p.LendNUMANode(ctx, "u1", "c1", 3, "l1"))
	e.expectUpdates("c1 mems=0-3")
	if err := e.p.LendNUMANode(ctx, "u1", "c5", 3, "l1"); !errors.Is(err, ErrSteered) {
		t.Fatalf("lend to a container with conflicting settings: %v", err)
	}
	noErr(t, e.p.LendNUMANode(ctx, "u1", "c2", 3, "l1"))
	e.expectUpdates("c2 mems=0-3")
}

// TestSynchronizeReleasesSteering checks the running containers of a
// restarted plugin: a container that its settings steer is not
// steered by the new process, and it is fenced like a container that is
// not steered, also one created while the plugin was down that has every
// node (X2). A container that a Steered record of the killed plugin names
// loses the memory.high that its manager left, also one taken as steered;
// the memory.high of the others is left alone (MemoryQoS). The records
// keep only the managers of the new process. A restart of the container is
// steered by a new manager, recorded until it stops.
func TestSynchronizeReleasesSteering(t *testing.T) {
	e := newEnvWith(t, envOptions{steered: map[string]string{"c1": "", "c8": "", "gone": ""}})
	// the records name the cgroups of the env
	noErr(t, e.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		for i := range doc.Steered {
			doc.Steered[i].CgroupDir = filepath.Join(cgroupRoot, "kubepods/u1", doc.Steered[i].Container)
		}
		return nil
	}))
	e.p.Seed([]int{2, 3})
	e.hasMemory("0-3")
	e.local.set(nriClaims())
	e.pool.set(nriPoolResults())
	t.Cleanup(e.p.Stop)
	ctx := context.Background()
	p1 := testPod("u1")

	// c1 was steered when the plugin was killed: the cpuset.mems of its
	// manager and the memory.high of its last step
	cg1 := e.proc(101, "kubepods/u1/c1", "0,2", "0,2")
	steerable(t, cg1, "140668928")
	// c2 was created while the plugin was down: every node, memory.high max
	cg2 := e.proc(102, "kubepods/u1/c2", "0-3", "")
	steerable(t, cg2, "max")
	// c3 is not steered, someone else (MemoryQoS) set its memory.high
	cg3 := e.proc(103, "kubepods/u1/c3", "0-3", "")
	steerable(t, cg3, "1073741824")
	// c5's settings conflict: never created by the plugin
	cg5 := e.proc(105, "kubepods/u1/c5", "0-3", "")
	steerable(t, cg5, "268435456")
	// c7 was created while the plugin was down, its settings steer it,
	// MemoryQoS set its memory.high
	cg7 := e.proc(107, "kubepods/u1/c7", "0-3", "")
	steerable(t, cg7, "536870912")
	// c8 was steered by the killed plugin under settings that combined
	// then, they conflict now
	cg8 := e.proc(108, "kubepods/u1/c8", "0,2", "0,2")
	steerable(t, cg8, "201326592")
	c1 := withClaims(testCtr(p1, "c1", "app", 101), "auto")
	c2 := withClaims(testCtr(p1, "c2", "down", 102), "poolauto")
	c3 := withClaims(testCtr(p1, "c3", "plain", 103), "split")
	c5 := withClaims(testCtr(p1, "c5", "conflict", 105), "auto", "none")
	c7 := withClaims(testCtr(p1, "c7", "qos", 107), "auto")
	c8 := withClaims(testCtr(p1, "c8", "changed", 108), "auto", "none")
	for _, c := range []*api.Container{c1, c2, c3, c5, c7, c8} {
		c.State = api.ContainerState_CONTAINER_RUNNING
	}
	// c4 was created by the killed plugin (with a plan), not started
	c4 := withClaims(testCtr(p1, "c4", "late", 0), "auto")
	c4.State = api.ContainerState_CONTAINER_CREATED
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{p1}, []*api.Container{c1, c2, c3, c4, c5, c7, c8})
	noErr(t, err)

	want := map[string]string{"c1": "fenced 2", "c2": "fenced 3", "c3": "fenced 2", "c4": "fenced 2", "c5": "steered ",
		"c7": "fenced 2", "c8": "steered "}
	if got := e.managements(); !reflect.DeepEqual(got, want) {
		t.Fatalf("management %v, expected %v", got, want)
	}
	if ids := e.managers(); len(ids) != 0 {
		t.Fatalf("managers %v", ids)
	}
	for _, c := range []struct{ dir, high string }{{cg1, "max"}, {cg2, "max"}, {cg3, "1073741824"}, {cg5, "268435456"},
		{cg7, "536870912"}, {cg8, "max"}} {
		if got := memoryHigh(t, c.dir); got != c.high {
			t.Errorf("%s: memory.high %s, expected %s", c.dir, got, c.high)
		}
	}
	if ids := e.steered(); len(ids) != 0 {
		t.Errorf("steering records %v after Synchronize, expected none", ids)
	}

	// the first pass fences them: c2 loses node 2 of another claim
	e.expectUpdates()
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates("c1 mems=0-2", "c2 mems=0-1,3", "c3 mems=0-2", "c4 mems=0-2", "c7 mems=0-2")
	e.check()
	// lent to, as containers that are not steered
	noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 3, "l1"))
	e.expectUpdates("c1 mems=0-3")
	noErr(t, e.p.RevokeNUMANode(ctx, "u1", "app", 3, "l1"))
	e.expectUpdates("c1 mems=0-2")
	if err := e.p.LendNUMANode(ctx, "u1", "conflict", 3, "l1"); !errors.Is(err, ErrSteered) {
		t.Fatalf("lend to a container taken as steered: %v", err)
	}

	// c4 starts without a plan in this process: not steered, fenced (its
	// task is created after the update of its spec)
	e.proc(104, "kubepods/u1/c4", "0-3", "0-2")
	c4.Pid = 104
	e.startCtr(p1, c4)
	e.passes()
	e.expectUpdates()
	if ids := e.managers(); len(ids) != 0 {
		t.Fatalf("managers %v", ids)
	}

	// c1 restarts: the new container gets a plan and a manager
	_, err = e.p.StopContainer(ctx, p1, c1)
	noErr(t, err)
	e.wait(e.gone, "u1", "app")
	cg6 := e.proc(106, "kubepods/u1/c6", "0-3", "")
	steerable(t, cg6, "max")
	c6 := withClaims(testCtr(p1, "c6", "app", 106), "auto")
	if got := e.createMems(p1, c6); got != "-" || e.record("c6").Plan == nil {
		t.Fatalf("adjustment %s of the restarted steered container", got)
	}
	e.startCtr(p1, c6)
	if ids := e.managers(); !reflect.DeepEqual(ids, []string{"c6"}) {
		t.Fatalf("managers %v", ids)
	}
	if got := memoryHigh(t, cg6); got == "max" {
		t.Fatal("restarted container not throttled by its manager")
	}
	if ids := e.steered(); !reflect.DeepEqual(ids, map[string]string{"c6": cg6}) {
		t.Fatalf("steering records %v, expected c6", ids)
	}
	if err := e.p.LendNUMANode(ctx, "u1", "app", 3, "l1"); !errors.Is(err, ErrSteered) {
		t.Fatalf("lend to the restarted steered container: %v", err)
	}
	e.p.pass(ctx, "")
	e.expectUpdates()
	// its stop releases it
	_, err = e.p.StopContainer(ctx, p1, c6)
	noErr(t, err)
	e.wait(e.gone, "u1", "app")
	if got := memoryHigh(t, cg6); got != "max" {
		t.Fatalf("memory.high %s after the stop of the manager", got)
	}
	if ids := e.steered(); len(ids) != 0 {
		t.Fatalf("steering records %v after the stop of the manager", ids)
	}
}

// A container steered by a killed plugin under the old defaults (steering
// policy auto) is released by the plugin restarted with new defaults (no
// steering) that do not steer it: the Steered record names it.
func TestSteeringReleaseAfterDefaultsChange(t *testing.T) {
	old := newDecider(t, config.Container{Steering: &config.Steering{Policy: config.PolicyAuto}})
	d, err := old.decide("x", old.results(testPod("p"), "x", []resultRef{{uid: "split", n: 0}, {uid: "split", n: 2}}), false)
	noErr(t, err)
	if d.management != ManagementSteered {
		t.Fatalf("precondition: old defaults do not steer: %s", d.management)
	}
	e := newEnvWith(t, envOptions{steered: map[string]string{"c1": ""}})
	e.p.Seed([]int{2})
	e.hasMemory("0-3")
	e.local.set(nriClaims())
	pod := testPod("u1")
	cg := e.proc(101, "kubepods/u1/c1", "0,2", "0,2")
	steerable(t, cg, "140668928")
	c1 := testCtr(pod, "c1", "app", 101)
	c1.Env = append(c1.Env, cxlapi.ResultMarker("split", 0)+"=1", cxlapi.ResultMarker("split", 2)+"=1")
	c1.State = api.ContainerState_CONTAINER_RUNNING
	_, err = e.p.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{c1})
	noErr(t, err)
	if got := memoryHigh(t, cg); got != "max" {
		t.Errorf("memory.high %s of the dead manager kept after a defaults change", got)
	}
	if m := e.record("c1").Management; m != ManagementFenced {
		t.Errorf("management %s, expected fenced", m)
	}
}

// A steered container whose manager does not start is managed like a
// container that is not steered: fenced by the repair pass
// of its PostStartContainer, lent to, and not in the Steered records.
// One warning names the reason.
func TestSteeringStartFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env) // the cgroup of c1 (pid 101)
		why   string
	}{
		{"no cgroup", func(e *env) {}, "failed to resolve cgroup directory"},
		{"manager start fails", func(e *env) {
			// no memory files: the manager cannot read memory.high
			e.proc(101, "kubepods/u1/c1", "0-3", "")
		}, "memory.high: no such file or directory"},
		{"memory.high of others", func(e *env) {
			steerable(t, e.proc(101, "kubepods/u1/c1", "0-3", ""), "536870912")
		}, "memory.high is 536870912, not max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFenceEnv(t, "0-3", 2, 3)
			e.local.set(nriClaims())
			ctx := context.Background()
			e.readyFence()
			logs := klogtest.Capture(t, 3)
			pod := testPod("u1")
			c1 := withClaims(testCtr(pod, "c1", "app", 101), "auto")
			if got := e.createMems(pod, c1); got != "-" || e.record("c1").Plan == nil {
				t.Fatalf("adjustment %s of a steered container", got)
			}
			tc.setup(e)
			e.startCtr(pod, c1)
			if c := e.record("c1"); c.Management != ManagementFenced || len(e.managers()) != 0 {
				t.Fatalf("management %s managers %v, expected fenced without a manager", c.Management, e.managers())
			}
			if ids := e.steered(); len(ids) != 0 {
				t.Fatalf("steering records %v", ids)
			}
			const warning = "NRI StartContainer: ctr=ns/pod-u1/app id=c1: failed to start memory steering: "
			var warnings []string
			for _, line := range strings.Split(logs(), "\n") {
				if strings.Contains(line, warning) {
					warnings = append(warnings, line)
				}
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.why) || !strings.HasSuffix(warnings[0], "; memory fenced") {
				t.Errorf("warnings %q, expected one with %q", warnings, tc.why)
			}
			e.expectUpdates("c1 mems=0-2")
			if tc.name == "no cgroup" {
				// its cgroup is unknown: the pass updates its spec
				return
			}
			if got := e.cgroupMems("kubepods/u1/c1"); got != "0-2" {
				t.Fatalf("cgroup cpuset.mems %s", got)
			}
			if tc.name == "memory.high of others" {
				if got := memoryHigh(t, filepath.Join(cgroupRoot, "kubepods/u1/c1")); got != "536870912" {
					t.Fatalf("memory.high %s of others replaced", got)
				}
			}
			noErr(t, e.p.LendNUMANode(ctx, "u1", "app", 3, "l1"))
			e.expectUpdates("c1 mems=0-3")
		})
	}
	// fencing none: not steered, its cpuset.mems left alone
	e := newEnv(t)
	e.hasMemory("0-3")
	e.local.set(nriClaims())
	pod := testPod("u1")
	c1 := withClaims(testCtr(pod, "c1", "app", 101), "auto")
	if got := e.createMems(pod, c1); got != "-" || e.record("c1").Management != ManagementSteered {
		t.Fatalf("adjustment %s of a steered container", got)
	}
	e.proc(101, "kubepods/u1/c1", "0-3", "")
	e.startCtr(pod, c1)
	if c := e.record("c1"); c.Management != ManagementUnfenced || len(e.managers()) != 0 || len(e.steered()) != 0 {
		t.Fatalf("record %+v, managers %v", c, e.managers())
	}
	noErr(t, e.p.LendNUMANode(context.Background(), "u1", "app", 3, "l1"))
	e.expectUpdates()
}

// TestCharSynchronizeSteeringLog pins the lines of the containers whose
// steering a restarted plugin does not restart: one that a Steered record
// of the killed plugin names (memory.high released) and one that its
// settings steer without a record (memory.high left alone).
func TestCharSynchronizeSteeringLog(t *testing.T) {
	e := newEnvWith(t, envOptions{steered: map[string]string{"c1": ""}})
	e.p.Seed([]int{2})
	e.hasMemory("0-3")
	e.local.set(nriClaims())
	logs := klogtest.Capture(t, 3)
	ctx := context.Background()
	p1 := testPod("u1")
	cg1 := e.proc(101, "kubepods/u1/c1", "0,2", "0,2")
	steerable(t, cg1, "140668928")
	cg7 := e.proc(107, "kubepods/u1/c7", "0-3", "")
	steerable(t, cg7, "536870912")
	c1 := withClaims(testCtr(p1, "c1", "app", 101), "auto")
	c7 := withClaims(testCtr(p1, "c7", "qos", 107), "auto")
	c1.State, c7.State = api.ContainerState_CONTAINER_RUNNING, api.ContainerState_CONTAINER_RUNNING
	_, err := e.p.Synchronize(ctx, []*api.PodSandbox{p1}, []*api.Container{c1, c7})
	noErr(t, err)
	for _, line := range []string{
		"NRI Synchronize: ctr=ns/pod-u1/app id=c1: steering not restarted, memory fenced; memory.high 140668928 removed (max)",
		"NRI Synchronize: ctr=ns/pod-u1/qos id=c7: steering not restarted, memory fenced; memory.high left alone: no manager steered it",
	} {
		if !strings.Contains(logs(), line) {
			t.Errorf("no line %q:\n%s", line, logs())
		}
	}
	if got := memoryHigh(t, cg1); got != "max" {
		t.Errorf("memory.high %s of the recorded container", got)
	}
	if got := memoryHigh(t, cg7); got != "536870912" {
		t.Errorf("memory.high %s of the container without a record", got)
	}
}

// TestSteeringEnded manages a running container whose steering ended on a
// refused memory policy like a container that is not steered: fenced by
// the repair pass, or with fencing none its cpuset.mems of before the
// steering, not in the Steered records, memory.high max, with one warning.
func TestSteeringEnded(t *testing.T) {
	for _, tc := range []struct {
		fencing    string
		management Management
		updates    []string
	}{
		{"", ManagementFenced, []string{"c1 mems=0-2"}},
		{config.FencingNone, ManagementUnfenced, nil},
	} {
		t.Run(string(tc.management), func(t *testing.T) {
			e := newEnvWith(t, envOptions{fencing: tc.fencing})
			e.p.Seed([]int{2, 3})
			e.hasMemory("0-3")
			e.local.set(nriClaims())
			e.readyFence()
			logs := klogtest.Capture(t, 3)
			pod := testPod("u1")
			cg1 := e.proc(101, "kubepods/u1/c1", "0-3", "")
			steerable(t, cg1, "max")
			c1 := withClaims(testCtr(pod, "c1", "app", 101), "auto")
			e.createMems(pod, c1)
			e.startCtr(pod, c1)
			e.p.mu.Lock()
			c := e.p.ctrs["c1"]
			mgr := c.manager
			e.p.mu.Unlock()
			if mgr == nil {
				t.Fatalf("management %s without a manager, expected steered", e.record("c1").Management)
			}
			e.expectUpdates()
			if got := e.cgroupMems("kubepods/u1/c1"); got != "0,2" {
				t.Fatalf("cpuset.mems %q of the steered container, expected 0,2", got)
			}
			e.p.steeringEnded(c, mgr, errors.New("memory policy refused: pid 101: operation not permitted"))
			e.passes()
			if c := e.record("c1"); c.Management != tc.management || len(e.managers()) != 0 {
				t.Fatalf("management %s managers %v, expected %s without a manager", c.Management, e.managers(), tc.management)
			}
			if ids := e.steered(); len(ids) != 0 {
				t.Fatalf("steering records %v", ids)
			}
			if got := memoryHigh(t, cg1); got != "max" {
				t.Errorf("memory.high %s", got)
			}
			e.expectUpdates(tc.updates...)
			if got := e.cgroupMems("kubepods/u1/c1"); tc.management == ManagementUnfenced && got != "" {
				t.Errorf("cpuset.mems %q with fencing none, expected the empty one of before the steering", got)
			}
			want := "NRI: ctr=ns/pod-u1/app id=c1: memory steering ended: memory policy refused: pid 101: " +
				"operation not permitted; memory " + tc.management.text()
			if got := logs(); strings.Count(got, want) != 1 {
				t.Errorf("no warning %q:\n%s", want, got)
			}
		})
	}
}

// TestSteeringEndedAtStart ends the steering of a container while its
// manager starts, before startSteering records the manager: the end is
// handled once the manager is recorded, as TestSteeringEnded's.
func TestSteeringEndedAtStart(t *testing.T) {
	e := newEnvWith(t, envOptions{})
	e.p.Seed([]int{2, 3})
	e.hasMemory("0-3")
	e.local.set(nriClaims())
	e.readyFence()
	logs := klogtest.Capture(t, 3)
	pod := testPod("u1")
	old := startManager
	t.Cleanup(func() { startManager = old })
	startManager = func(m *steering.Manager) error {
		err := old(m)
		e.p.mu.Lock()
		c := e.p.ctrs["c1"]
		e.p.mu.Unlock()
		e.p.steeringEnded(c, m, errors.New("memory policy refused: pid 101: operation not permitted"))
		return err
	}
	cg1 := e.proc(101, "kubepods/u1/c1", "0-3", "")
	steerable(t, cg1, "max")
	c1 := withClaims(testCtr(pod, "c1", "app", 101), "auto")
	e.createMems(pod, c1)
	e.startCtr(pod, c1)
	e.passes()
	if c := e.record("c1"); c.Management != ManagementFenced || len(e.managers()) != 0 {
		t.Fatalf("management %s managers %v, expected fenced without a manager", c.Management, e.managers())
	}
	if ids := e.steered(); len(ids) != 0 {
		t.Fatalf("steering records %v", ids)
	}
	if got := memoryHigh(t, cg1); got != "max" {
		t.Errorf("memory.high %s", got)
	}
	want := "NRI: ctr=ns/pod-u1/app id=c1: memory steering ended: memory policy refused: pid 101: " +
		"operation not permitted; memory " + ManagementFenced.text()
	if got := logs(); strings.Count(got, want) != 1 {
		t.Errorf("no warning %q:\n%s", want, got)
	}
}

// TestSteeredRecordBeforeStart checks that the Steered record of a
// container is in the store when its manager starts to write its cgroup,
// that a Start that fails leaves no record, and that no manager starts
// when the commit of the record fails.
func TestSteeredRecordBeforeStart(t *testing.T) {
	var failWrites atomic.Bool
	e := newEnvWith(t, envOptions{writeFault: func() error {
		if failWrites.Load() {
			return errors.New("disk failure")
		}
		return nil
	}})
	e.p.Seed([]int{2})
	e.hasMemory("0-3")
	e.local.set(nriClaims())
	e.readyFence()
	t.Cleanup(e.p.Stop)
	pod := testPod("u1")
	var at map[string]string
	old := startManager
	t.Cleanup(func() { startManager = old })
	startManager = func(m *steering.Manager) error {
		at = e.steered()
		return old(m)
	}
	cg1 := e.proc(101, "kubepods/u1/c1", "0-3", "")
	steerable(t, cg1, "max")
	c1 := withClaims(testCtr(pod, "c1", "app", 101), "auto")
	if got := e.createMems(pod, c1); got != "-" {
		t.Fatalf("adjustment %s", got)
	}
	e.startCtr(pod, c1)
	if !reflect.DeepEqual(at, map[string]string{"c1": cg1}) {
		t.Fatalf("steering records %v at the start of the manager, expected c1", at)
	}
	if got := memoryHigh(t, cg1); got == "max" {
		t.Fatal("not steered")
	}

	startManager = func(*steering.Manager) error {
		at = e.steered()
		return errors.New("no cgroup any more")
	}
	cg2 := e.proc(102, "kubepods/u1/c2", "0-3", "")
	steerable(t, cg2, "max")
	c2 := withClaims(testCtr(pod, "c2", "app2", 102), "auto")
	e.createMems(pod, c2)
	e.startCtr(pod, c2)
	if at["c2"] != cg2 {
		t.Fatalf("steering records %v at the start of the manager, expected c2", at)
	}
	if got := e.steered(); !reflect.DeepEqual(got, map[string]string{"c1": cg1}) {
		t.Fatalf("steering records %v after a failed start, expected c1", got)
	}
	if c := e.record("c2"); c.Management != ManagementFenced {
		t.Fatalf("management %s after a failed start", c.Management)
	}

	started := false
	startManager = func(*steering.Manager) error {
		started = true
		return nil
	}
	cg3 := e.proc(103, "kubepods/u1/c3", "0-3", "")
	steerable(t, cg3, "max")
	c3 := withClaims(testCtr(pod, "c3", "app3", 103), "auto")
	e.createMems(pod, c3)
	failWrites.Store(true)
	e.startCtr(pod, c3)
	failWrites.Store(false)
	if started || len(e.managers()) != 1 {
		t.Fatalf("a manager started without its Steered record: managers %v", e.managers())
	}
	if c := e.record("c3"); c.Management != ManagementFenced {
		t.Fatalf("management %s after a failed commit of the record", c.Management)
	}
	if got := e.steered(); !reflect.DeepEqual(got, map[string]string{"c1": cg1}) {
		t.Fatalf("steering records %v after a failed commit, expected c1", got)
	}
}

func TestSteeringPolicy(t *testing.T) {
	dram := steering.MemoryTypes{DRAMNUMANodes: []int{0}, DRAMQuota: 1 << 30}
	cxl := steering.MemoryTypes{CXLNUMANodes: []int{2}, CXLQuota: 1 << 30}
	both := steering.MemoryTypes{DRAMNUMANodes: []int{0}, CXLNUMANodes: []int{2}, DRAMQuota: 1 << 30, CXLQuota: 1 << 30}
	for _, tc := range []struct {
		name    string
		s       *config.Steering
		mem     steering.MemoryTypes
		want    steering.Policy
		steer   bool
		minStep int64
		maxStep int64
		// waypoints of the plan, unchecked if nil
		waypoints []steering.NodeMem
	}{
		{name: "nil", s: nil, mem: both},
		{name: "no policy", s: &config.Steering{}, mem: both},
		{name: "none", s: &config.Steering{Policy: config.PolicyNone}, mem: both},
		{name: "no memory", s: &config.Steering{Policy: config.PolicyAuto}},
		{name: "auto CXL only", s: &config.Steering{Policy: config.PolicyAuto}, mem: cxl, want: steering.PolicyFirstCXL, steer: true,
			minStep: 1 << 30, maxStep: 1 << 30},
		{name: "auto DRAM only", s: &config.Steering{Policy: config.PolicyAuto}, mem: dram, want: steering.PolicyFirstDRAM, steer: true,
			minStep: 1 << 30, maxStep: 1 << 30},
		{name: "auto both", s: &config.Steering{Policy: config.PolicyAuto, MaxStep: "1Gi"}, mem: both,
			want: steering.PolicyStartInterleaved, steer: true, minStep: 1 << 30, maxStep: 1 << 30},
		{name: "first-dram", s: &config.Steering{Policy: config.PolicyFirstDRAM, MinStep: "64Mi"}, mem: cxl,
			want: steering.PolicyFirstDRAM, steer: true, minStep: 64 << 20, maxStep: 1 << 30},
		{name: "first-cxl", s: &config.Steering{Policy: config.PolicyFirstCXL}, mem: both, want: steering.PolicyFirstCXL, steer: true,
			minStep: 1 << 30, maxStep: 2 << 30},
		{name: "start-interleaved", s: &config.Steering{Policy: config.PolicyStartInterleaved}, mem: both,
			want: steering.PolicyStartInterleaved, steer: true, minStep: 1 << 30, maxStep: 2 << 30},
		{name: "end-interleaved", s: &config.Steering{Policy: config.PolicyEndInterleaved}, mem: both,
			want: steering.PolicyEndInterleaved, steer: true, minStep: 1 << 30, maxStep: 2 << 30},
		{
			name: "waypoints",
			s: &config.Steering{Policy: config.PolicyWaypoints, MinStep: "64Mi", MaxStep: "1Gi", Waypoints: []config.Waypoint{
				{TargetUsages: []config.TargetUsage{{MemoryType: config.MemoryTypeDRAM, Usage: "512Mi"}, {MemoryType: config.MemoryTypeCXL, Usage: "256Mi"}}},
				{TargetUsages: []config.TargetUsage{{MemoryType: config.MemoryTypeDRAM, Usage: "1Gi"}}},
				{TargetUsages: []config.TargetUsage{{MemoryType: config.MemoryTypeCXL, Usage: "1Gi"}}},
			}},
			mem:  both,
			want: steering.PolicyWaypoints, steer: true, minStep: 64 << 20, maxStep: 1 << 30,
			// a memory type that a waypoint does not list keeps its usage
			waypoints: []steering.NodeMem{{0: 512 << 20, 2: 256 << 20}, {0: 1 << 30, 2: 256 << 20}, {0: 1 << 30, 2: 1 << 30}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, steer := steeringPolicy(tc.s, tc.mem)
			if steer != tc.steer || got != tc.want {
				t.Fatalf("got %s %v, expected %s %v", got, steer, tc.want, tc.steer)
			}
			if !steer {
				return
			}
			plan, err := steeringPlan(tc.s, got, tc.mem)
			if err != nil {
				t.Fatalf("no plan: %v", err)
			}
			if plan.MinStep != tc.minStep || plan.MaxStep != tc.maxStep {
				t.Errorf("steps %d, %d, expected %d, %d", plan.MinStep, plan.MaxStep, tc.minStep, tc.maxStep)
			}
			if tc.waypoints != nil && !reflect.DeepEqual(plan.Waypoints, tc.waypoints) {
				t.Errorf("waypoints %v, expected %v", plan.Waypoints, tc.waypoints)
			}
		})
	}
}

func TestPolicyNames(t *testing.T) {
	// every policy but none and auto is a policy of the steering package
	// with the same name
	for _, p := range []string{config.PolicyNone, config.PolicyAuto, config.PolicyFirstDRAM, config.PolicyFirstCXL,
		config.PolicyStartInterleaved, config.PolicyEndInterleaved, config.PolicyWaypoints} {
		policy, ok := policies[p]
		if want := p != config.PolicyNone && p != config.PolicyAuto; ok != want || (ok && policy.String() != p) {
			t.Errorf("%s: %s %v", p, policy, ok)
		}
	}
}
