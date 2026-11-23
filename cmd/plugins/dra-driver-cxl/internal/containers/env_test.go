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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
)

// fakeStub is a fake NRI stub that records UpdateContainers.
type fakeStub struct {
	mu      sync.Mutex
	updates []string // "<id> mems=<mems>"
	fail    bool     // report the update as failed
	err     error    // return this error
	// failIDs are the containers whose updates are reported failed.
	failIDs map[string]bool
	// specOnly are the containers whose updates reach only their spec,
	// as in containerd before it has marked a container started.
	specOnly map[string]bool
	// applied is called for an update that reaches the cgroup.
	applied func(id, mems string)
	// entered gets a value when UpdateContainers is called, and gate
	// blocks it until closed (nil: neither).
	entered chan struct{}
	gate    chan struct{}
	opened  sync.Once
}

// closeGate makes UpdateContainers block until openGate, and returns the
// channel that gets a value at each call.
func (s *fakeStub) closeGate(t *testing.T) chan struct{} {
	s.entered, s.gate = make(chan struct{}, 4), make(chan struct{})
	// a failed test must not leave the gated calls blocked
	t.Cleanup(s.openGate)
	return s.entered
}

// openGate lets the gated UpdateContainers calls go on.
func (s *fakeStub) openGate() {
	s.opened.Do(func() { close(s.gate) })
}

func (s *fakeStub) Start(context.Context) error { return nil }
func (s *fakeStub) Stop()                       {}
func (s *fakeStub) UpdateContainers(u []*api.ContainerUpdate) ([]*api.ContainerUpdate, error) {
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.fail {
		return u, nil
	}
	var failed, applied []*api.ContainerUpdate
	for _, cu := range u {
		if s.failIDs[cu.GetContainerId()] {
			failed = append(failed, cu)
			continue
		}
		s.updates = append(s.updates, cu.GetContainerId()+" mems="+cu.GetLinux().GetResources().GetCpu().GetMems())
		if !s.specOnly[cu.GetContainerId()] {
			applied = append(applied, cu)
		}
	}
	if s.applied != nil {
		s.mu.Unlock()
		for _, cu := range applied {
			s.applied(cu.GetContainerId(), cu.GetLinux().GetResources().GetCpu().GetMems())
		}
		s.mu.Lock()
	}
	return failed, nil
}

// setSpecOnly makes the updates of the container reach only its spec
// (on) or its cgroup too.
func (s *fakeStub) setSpecOnly(id string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.specOnly == nil {
		s.specOnly = map[string]bool{}
	}
	s.specOnly[id] = on
}

// setFailIDs makes the updates of the containers ids fail in the runtime.
func (s *fakeStub) setFailIDs(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failIDs = map[string]bool{}
	for _, id := range ids {
		s.failIDs[id] = true
	}
}

// setErr makes UpdateContainers return err.
func (s *fakeStub) setErr(err error, fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err, s.fail = err, fail
}

func (s *fakeStub) taken() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.updates
	s.updates = nil
	return out
}

// event is a callback of the plugin: the pod and the name of a container.
type event struct {
	pod  state.PodRef
	name string
}

// env is a plugin with a fake stub, a store and fake /proc and cgroupfs.
type env struct {
	t     *testing.T
	p     *Plugin
	stub  *fakeStub
	st    *store.Store
	root  string
	gone  chan event
	start chan event
	// local and pool are the prepared claims of the drivers.
	local, pool claimTable
}

// envOptions are the settings of a test env.
type envOptions struct {
	// fencing is defaults.container.fencing, "" for allowed-mems.
	fencing string
	// defaults are the container settings (fencing from fencing).
	defaults config.Container
	// steered are the Steered records of the store at the start.
	steered map[string]string
	// leases are READY numa leases in the store at the start.
	leases []state.Lease
	// noLeases: no lease broker, the leases do not seed lends.
	noLeases bool
	// writeFault is the store's WriteFault after the start.
	writeFault func() error
}

// newEnv returns a plugin in mode fencing none, with the nodes with memory
// unreadable, connected to a fake stub.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, envOptions{fencing: config.FencingNone})
}

// newFenceEnv returns a plugin in mode allowed-mems with the nodes with
// memory hasMemory and the fenced nodes fenced (without a pass).
func newFenceEnv(t *testing.T, hasMemory string, fenced ...int) *env {
	t.Helper()
	e := newEnvWith(t, envOptions{})
	e.p.Seed(fenced)
	e.hasMemory(hasMemory)
	return e
}

// newEnvWith returns a plugin of the options connected to a fake stub.
func newEnvWith(t *testing.T, o envOptions) *env {
	t.Helper()
	root := t.TempDir()
	oldProc, oldCg := procDir, cgroupRoot
	procDir, cgroupRoot = filepath.Join(root, "proc"), filepath.Join(root, "cg")
	t.Cleanup(func() { procDir, cgroupRoot = oldProc, oldCg })

	e := &env{
		t:     t,
		stub:  &fakeStub{},
		root:  root,
		gone:  make(chan event, 16),
		start: make(chan event, 16),
		local: claimTable{},
		pool:  claimTable{},
	}
	e.st = openStore(t, filepath.Join(root, "state"), o)
	defaults := o.defaults
	defaults.Fencing = o.fencing
	p, err := New(Options{Defaults: defaults, Store: e.st, Leases: !o.noLeases, SysRoot: root, Results: []Results{e.local, e.pool}})
	noErr(t, err)
	e.p = p
	p.connect(e.stub)
	p.OnContainerGone(func(pod state.PodRef, name string) { e.gone <- event{pod, name} })
	p.OnContainerStarted(func(pod state.PodRef, name string) { e.start <- event{pod, name} })
	e.stub.applied = e.toCgroup
	t.Cleanup(e.passes)
	return e
}

// openStore returns a store in dir with the records of o.
func openStore(t *testing.T, dir string, o envOptions) *store.Store {
	t.Helper()
	ctx := context.Background()
	open := func(opts store.Options) *store.Store {
		t.Helper()
		opts.Dir, opts.BootID = dir, "boot-1"
		st, err := store.Open(ctx, opts)
		noErr(t, err)
		return st
	}
	st := open(store.Options{})
	noErr(t, st.Commit(ctx, store.Durable, func(doc *store.Document) error {
		for id, cg := range o.steered {
			doc.Steered = append(doc.Steered, state.Steered{Container: id, CgroupDir: cg})
		}
		doc.Leases = append(doc.Leases, o.leases...)
		return nil
	}))
	noErr(t, st.Close())
	st = open(store.Options{WriteFault: o.writeFault})
	if st.Origin() != store.Loaded {
		t.Fatalf("store origin %v, expected Loaded", st.Origin())
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// setLeases makes the leases ids READY numa leases of node of the
// container name of the pod podUID in the store, none of it without ids.
func (e *env) setLeases(podUID, name string, node int, ids ...string) {
	e.t.Helper()
	noErr(e.t, e.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		var kept []state.Lease
		for _, l := range doc.Leases {
			if l.Pod.UID != podUID || l.Container != name {
				kept = append(kept, l)
			}
		}
		for _, id := range ids {
			kept = append(kept, readyLease(id, podUID, name, node))
		}
		doc.Leases = kept
		return nil
	}))
}

// readyLease returns a READY numa lease of node of the container name of
// the pod podUID.
func readyLease(id, podUID, name string, node int) state.Lease {
	return state.Lease{ID: id, Claim: "lc-" + podUID, Pod: state.PodRef{Namespace: "ns", Name: "pod-" + podUID, UID: podUID},
		Container: name, State: state.LeaseReady, Asked: state.LeaseAsked{Mode: state.ModeNUMA, Size: "256Mi"},
		Lend: &state.LeaseLend{NUMANode: state.NUMANodePtr(node)}, Created: time.Now()}
}

// steered returns the Steered records of the store.
func (e *env) steered() map[string]string {
	out := map[string]string{}
	for _, s := range e.st.Snapshot().Steered {
		out[s.Container] = s.CgroupDir
	}
	return out
}

// toCgroup writes the cpuset.mems of an update to the cgroup of a container
// whose task exists, as runc does. An update sent with the table mutex held
// fails the test (the runtime would deadlock): the mutex is not free within
// a second.
func (e *env) toCgroup(id, mems string) {
	for deadline := time.Now().Add(time.Second); !e.p.mu.TryLock(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			e.t.Error("UpdateContainers with the table mutex held")
			return
		}
	}
	dir := ""
	if c := e.p.ctrs[id]; c != nil && c.started() {
		dir = c.CgroupDir
	}
	e.p.mu.Unlock()
	if dir == "" {
		return
	}
	if _, err := os.Stat(dir); err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "cpuset.mems"), []byte(mems+"\n"), 0o644); err != nil {
		e.t.Error(err)
	}
}

// hasMemory sets the nodes with memory, "" for none (unreadable).
func (e *env) hasMemory(nodes string) {
	e.t.Helper()
	if nodes == "" {
		_ = os.Remove(e.p.hasMemory)
		return
	}
	noErr(e.t, os.MkdirAll(filepath.Dir(e.p.hasMemory), 0o755))
	noErr(e.t, os.WriteFile(e.p.hasMemory, []byte(nodes+"\n"), 0o644))
}

// passes waits for the fence passes running in the background.
func (e *env) passes() {
	e.p.passes.Wait()
}

// setCgroupMems sets the cpuset.mems of the cgroup of a container.
func (e *env) setCgroupMems(rel, mems string) {
	e.t.Helper()
	noErr(e.t, os.WriteFile(filepath.Join(cgroupRoot, rel, "cpuset.mems"), []byte(mems+"\n"), 0o644))
}

// cgroupMems returns the cpuset.mems of the cgroup of a container.
func (e *env) cgroupMems(rel string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(cgroupRoot, rel, "cpuset.mems"))
	noErr(e.t, err)
	return strings.TrimSpace(string(data))
}

// proc makes /proc/<pid>/cgroup point to cgroup rel and returns the
// absolute cgroup directory, with cpuset.mems.effective = effMems and
// cpuset.mems = mems.
func (e *env) proc(pid uint32, rel, effMems, mems string) string {
	e.t.Helper()
	pdir := filepath.Join(procDir, fmt.Sprint(pid))
	cg := filepath.Join(cgroupRoot, rel)
	for _, d := range []string{pdir, cg} {
		noErr(e.t, os.MkdirAll(d, 0o755))
	}
	noErr(e.t, os.WriteFile(filepath.Join(pdir, "cgroup"), []byte("0::/"+rel+"\n"), 0o644))
	noErr(e.t, os.WriteFile(filepath.Join(cg, "cpuset.mems.effective"), []byte(effMems+"\n"), 0o644))
	noErr(e.t, os.WriteFile(filepath.Join(cg, "cpuset.mems"), []byte(mems+"\n"), 0o644))
	return cg
}

func testPod(uid string) *api.PodSandbox {
	return &api.PodSandbox{Id: "sb-" + uid, Uid: uid, Namespace: "ns", Name: "pod-" + uid}
}

func testCtr(pod *api.PodSandbox, id, name string, pid uint32) *api.Container {
	return &api.Container{Id: id, PodSandboxId: pod.GetId(), Name: name, Pid: pid}
}

// withSpecMems sets cpuset.mems of the spec of a container.
func withSpecMems(ctr *api.Container, mems string) *api.Container {
	ctr.Linux = &api.LinuxContainer{Resources: &api.LinuxResources{Cpu: &api.LinuxCPU{Mems: mems}}}
	return ctr
}

// withClaims adds the markers of the results 0 of the claims to the env of
// a container.
func withClaims(ctr *api.Container, uids ...string) *api.Container {
	for _, uid := range uids {
		ctr.Env = append(ctr.Env, cxlapi.ResultMarker(uid, 0)+"=1")
	}
	return ctr
}

// run creates and starts a container whose cgroup has effMems, and the
// cpuset.mems of its adjustment, as runc creates it. Its updates reach
// only its spec until it has started, as in containerd.
func (e *env) run(pod *api.PodSandbox, id, name string, pid uint32, effMems string) *api.Container {
	e.t.Helper()
	ctr := testCtr(pod, id, name, pid)
	adj, _, err := e.p.CreateContainer(context.Background(), pod, ctr)
	noErr(e.t, err)
	e.stub.setSpecOnly(ctr.GetId(), true)
	e.proc(pid, "kubepods/"+pod.GetUid()+"/"+id, effMems, adj.GetLinux().GetResources().GetCpu().GetMems())
	e.startCtr(pod, ctr)
	return ctr
}

// runPinned is run of a container whose spec has the cpuset.mems mems, as
// kubelet's memory manager pins it: its cgroup has it as cpuset.mems and
// cpuset.mems.effective.
func (e *env) runPinned(pod *api.PodSandbox, id, name string, pid uint32, mems string) *api.Container {
	e.t.Helper()
	ctr := withSpecMems(testCtr(pod, id, name, pid), mems)
	_, _, err := e.p.CreateContainer(context.Background(), pod, ctr)
	noErr(e.t, err)
	e.stub.setSpecOnly(ctr.GetId(), true)
	e.proc(pid, "kubepods/"+pod.GetUid()+"/"+id, mems, mems)
	e.startCtr(pod, ctr)
	return ctr
}

// startCtr starts a created container: StartContainer, then the runtime
// marks it started (its updates reach the cgroup) and PostStartContainer.
// Waits for the OnContainerStarted callback and the fence passes.
func (e *env) startCtr(pod *api.PodSandbox, ctr *api.Container) {
	e.t.Helper()
	ctx := context.Background()
	noErr(e.t, e.p.StartContainer(ctx, pod, ctr))
	e.stub.setSpecOnly(ctr.GetId(), false)
	noErr(e.t, e.p.PostStartContainer(ctx, pod, ctr))
	e.wait(e.start, pod.GetUid(), ctr.GetName())
	e.passes()
}

// wait returns the next callback of c, which must be of the container name
// of the pod podUID.
func (e *env) wait(c chan event, podUID, name string) event {
	e.t.Helper()
	select {
	case ev := <-c:
		if ev.pod.UID != podUID || ev.name != name {
			e.t.Fatalf("callback for %s/%s, expected %s/%s", ev.pod.UID, ev.name, podUID, name)
		}
		return ev
	case <-time.After(5 * time.Second):
		e.t.Fatalf("no callback for %s/%s", podUID, name)
	}
	return event{}
}

func (e *env) noCallback(c chan event) {
	e.t.Helper()
	select {
	case ev := <-c:
		e.t.Fatalf("unexpected callback for %s/%s", ev.pod.UID, ev.name)
	case <-time.After(50 * time.Millisecond):
	}
}

func (e *env) expectUpdates(want ...string) {
	e.t.Helper()
	got := e.stub.taken()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		e.t.Fatalf("updates %q, expected %q", got, want)
	}
}

// createMems runs CreateContainer and returns the cpuset.mems of the
// adjustment, "-" for no adjustment. The updates of the container reach
// only its spec until it has started (startCtr), as in containerd.
func (e *env) createMems(pod *api.PodSandbox, ctr *api.Container) string {
	e.t.Helper()
	adj, upd, err := e.p.CreateContainer(context.Background(), pod, ctr)
	noErr(e.t, err)
	e.stub.setSpecOnly(ctr.GetId(), true)
	if len(upd) != 0 {
		e.t.Fatalf("CreateContainer returned updates %v", upd)
	}
	if adj == nil {
		return "-"
	}
	return adj.GetLinux().GetResources().GetCpu().GetMems()
}

// record returns the record of a container.
func (e *env) record(id string) *Container {
	e.t.Helper()
	c, ok := e.p.Record(id)
	if !ok {
		e.t.Fatalf("no record of %s", id)
	}
	return &c
}

// specOf returns the recorded cpuset.mems of the spec of a container.
func (e *env) specOf(id string) string {
	e.t.Helper()
	return e.record(id).SpecMems
}

// setSpecMems sets the cpuset.mems of the spec of a container in its
// record, as an update of the runtime that the plugin did not see.
func (e *env) setSpecMems(id, mems string) {
	e.t.Helper()
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	c := e.p.ctrs[id]
	if c == nil {
		e.t.Fatalf("no record of %s", id)
	}
	c.SpecMems = mems
}

// newest returns a copy of the newest record of the container name of the
// pod podUID, and false for none.
func (e *env) newest(podUID, name string) (Container, bool) {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	c := e.p.lookup(podUID, name)
	if c == nil {
		return Container{}, false
	}
	return *c, true
}

// byPid returns a copy of the record of the container whose cgroup has the
// process pid, and false for none.
func (e *env) byPid(pid int) (Container, bool) {
	return e.p.byPid(pid)
}

// ctrText returns the identity, the task and the own nodes of a record.
func ctrText(c Container) string {
	return fmt.Sprintf("%s %s/%s/%s id=%s pid=%d cgroup=%s base=%s", c.PodUID, c.PodNamespace, c.PodName, c.Name, c.ID, c.Pid,
		c.CgroupDir, c.Base)
}

// baseOf returns the own nodes of the newest container of the pod with the
// name.
func (e *env) baseOf(podUID, name string) string {
	e.t.Helper()
	c, ok := e.newest(podUID, name)
	if !ok {
		e.t.Fatalf("no container %s/%s", podUID, name)
	}
	return c.Base.String()
}

// check fails the test if a fenced, started container does not have its
// wantMems (I11).
func (e *env) check() {
	e.t.Helper()
	e.passes()
	for _, v := range e.p.Check() {
		e.t.Error(v)
	}
}

// readyFence makes fencing of running containers ready.
func (e *env) readyFence() {
	e.t.Helper()
	_, err := e.p.Synchronize(context.Background(), nil, nil)
	noErr(e.t, err)
	e.p.LendsRestored()
	e.passes()
	e.expectUpdates()
}

// claimInfo is a prepared claim: its namespace/name and its devices.
type claimInfo struct {
	ClaimName string
	Devices   []state.DeviceInfo
}

// claimTable is the prepared claims of a driver by uid.
type claimTable map[string]*claimInfo

// PreparedResult implements Results.
func (c claimTable) PreparedResult(uid string, n int) (string, state.DeviceInfo, bool) {
	info := c[uid]
	if info == nil {
		return "", state.DeviceInfo{}, false
	}
	for _, d := range info.Devices {
		if d.Result == n {
			return info.ClaimName, d, true
		}
	}
	return "", state.DeviceInfo{}, false
}

// set replaces the claims of the table.
func (c claimTable) set(claims claimTable) {
	for uid := range c {
		delete(c, uid)
	}
	for uid, info := range claims {
		c[uid] = info
	}
}

// lineWith returns the last line of out that has text, "" for none.
func lineWith(out, text string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], text) {
			return lines[i]
		}
	}
	return ""
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("error %v, expected one with %q", err, text)
	}
}
