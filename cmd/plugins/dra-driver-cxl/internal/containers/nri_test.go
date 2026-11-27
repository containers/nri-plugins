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
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	nriapi "github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/net/multiplex"
	"github.com/containerd/ttrpc"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/config"
)

// TestLifecycleOfContainers creates, starts and stops a fenced, a steered
// and an inheriting container (fencing none) with the lifecycle
// and the management of each record, and a container that the plugin
// never saw created.
func TestLifecycleOfContainers(t *testing.T) {
	ctx := context.Background()
	lifecycle := func(e *env, id string) Lifecycle { return e.record(id).Lifecycle }

	// fenced: the adjustment at its creation, the repair pass at its start
	e := newFenceEnv(t, "0-2", 2)
	e.local.set(fenceClaims())
	e.readyFence()
	pod := testPod("u1")
	c1 := testCtr(pod, "c1", "fenced", 101)
	if got := e.createMems(pod, c1); got != "0-1" || lifecycle(e, "c1") != LifecycleCreated ||
		e.record("c1").Management != ManagementFenced {
		t.Fatalf("created: adjustment %s, record %+v", got, e.record("c1"))
	}
	// the task has every node: its spec was read before the adjustment
	e.proc(101, "kubepods/u1/c1", "0-2", "")
	noErr(t, e.p.StartContainer(ctx, pod, c1))
	if lifecycle(e, "c1") != LifecycleTaskCreated {
		t.Fatalf("lifecycle %s", lifecycle(e, "c1"))
	}
	e.stub.setSpecOnly("c1", false)
	noErr(t, e.p.PostStartContainer(ctx, pod, c1))
	e.wait(e.start, "u1", "fenced")
	e.passes()
	e.expectUpdates("c1 mems=0-1")
	if lifecycle(e, "c1") != LifecycleStarted || e.cgroupMems("kubepods/u1/c1") != "0-1" {
		t.Fatalf("started: %s, cgroup %s", lifecycle(e, "c1"), e.cgroupMems("kubepods/u1/c1"))
	}
	e.check()
	// a cgroup that lost its update violates I11 until the next pass
	e.setCgroupMems("kubepods/u1/c1", "0-2")
	if v := e.p.Check(); len(v) != 1 || !strings.Contains(v[0], `I11: ctr=ns/pod-u1/fenced id=c1: cpuset.mems "0-2", not 0-1`) {
		t.Fatalf("violations %q", v)
	}
	// a full pass that cannot repair it (the runtime updates the spec
	// only) logs the violation
	e.stub.setSpecOnly("c1", true)
	passLogs := klogtest.Capture(t, 4)
	e.p.Repair(ctx)
	e.passes()
	if out := passLogs(); !strings.Contains(out, `containers invariant: I11: ctr=ns/pod-u1/fenced id=c1: cpuset.mems "0-2", not 0-1`) {
		t.Fatalf("no violation logged:\n%s", out)
	}
	e.stub.setSpecOnly("c1", false)
	e.expectUpdates("c1 mems=0-1")
	e.p.Fence(ctx, 3)
	e.expectUpdates("c1 mems=0-1")
	e.check()

	// steered: a plan at its creation, a manager and its record at its
	// start, released at its stop
	cg2 := e.proc(102, "kubepods/u1/c2", "0-2", "")
	steerable(t, cg2, "max")
	c2 := withClaims(testCtr(pod, "c2", "steered", 102), "numa-auto")
	if got := e.createMems(pod, c2); got != "-" || e.record("c2").Management != ManagementSteered || e.record("c2").Plan == nil {
		t.Fatalf("created: adjustment %s, record %+v", got, e.record("c2"))
	}
	e.startCtr(pod, c2)
	if c := e.record("c2"); c.Lifecycle != LifecycleStarted || c.Plan != nil || !reflect.DeepEqual(e.managers(), []string{"c2"}) {
		t.Fatalf("started: record %+v, managers %v", c, e.managers())
	}
	if got := e.steered(); !reflect.DeepEqual(got, map[string]string{"c2": cg2}) {
		t.Fatalf("steering records %v", got)
	}
	if got := memoryHigh(t, cg2); got == "max" {
		t.Fatal("no memory.high of the manager")
	}
	e.expectUpdates()
	_, err := e.p.StopContainer(ctx, pod, c2)
	noErr(t, err)
	e.wait(e.gone, "u1", "steered")
	if _, ok := e.p.Record("c2"); ok || len(e.steered()) != 0 || memoryHigh(t, cg2) != "max" {
		t.Fatalf("after the stop: steering records %v, memory.high %s", e.steered(), memoryHigh(t, cg2))
	}

	// a container that the plugin never saw created: the defaults
	c3 := testCtr(pod, "c3", "unseen", 103)
	e.proc(103, "kubepods/u1/c3", "0-1", "0-1")
	noErr(t, e.p.StartContainer(ctx, pod, c3))
	if c := e.record("c3"); c.Lifecycle != LifecycleTaskCreated || c.Management != ManagementFenced {
		t.Fatalf("record %+v", c)
	}
	noErr(t, e.p.RemoveContainer(ctx, pod, c3))
	e.wait(e.gone, "u1", "unseen")

	// a container that Synchronize found created gets started; one that
	// PostStart starts; one that Synchronize found running
	c4 := testCtr(pod, "c4", "synced", 104)
	c4.State = nriapi.ContainerState_CONTAINER_CREATED
	c5 := testCtr(pod, "c5", "post", 105)
	c5.State = nriapi.ContainerState_CONTAINER_CREATED
	c6 := testCtr(pod, "c6", "late", 106)
	c6.State = nriapi.ContainerState_CONTAINER_RUNNING
	n := newFenceEnv(t, "0-2", 2)
	n.proc(106, "kubepods/u1/c6", "0-2", "0-2")
	_, err = n.p.Synchronize(ctx, []*nriapi.PodSandbox{pod}, []*nriapi.Container{c4, c5, c6})
	noErr(t, err)
	if lifecycle(n, "c4") != LifecycleUnseen {
		t.Fatalf("lifecycle %s", lifecycle(n, "c4"))
	}
	n.proc(104, "kubepods/u1/c4", "0-2", "0-2")
	n.startCtr(pod, c4)
	if lifecycle(n, "c4") != LifecycleStarted {
		t.Fatalf("lifecycle %s", lifecycle(n, "c4"))
	}
	n.proc(105, "kubepods/u1/c5", "0-2", "0-2")
	noErr(t, n.p.PostStartContainer(ctx, pod, c5))
	n.wait(n.start, "u1", "post")
	if c := n.record("c5"); c.Lifecycle != LifecycleStarted || c.Pid != 105 {
		t.Fatalf("record %+v", c)
	}
	if c := n.record("c6"); c.Lifecycle != LifecycleStarted || c.Pid != 106 {
		t.Fatalf("record %+v", c)
	}
	// its PostStartContainer after that: the callbacks, no refused
	// transition
	logs := klogtest.Capture(t, 3)
	noErr(t, n.p.PostStartContainer(ctx, pod, c6))
	n.wait(n.start, "u1", "late")
	if c := n.record("c6"); c.Lifecycle != LifecycleStarted {
		t.Fatalf("record %+v", c)
	}
	if out := logs(); strings.Contains(out, "not in the table") {
		t.Fatalf("a refused transition:\n%s", out)
	}

	// inheriting (fencing none): never narrowed, also by a lend and a
	// revoke
	i := newEnv(t)
	c7 := i.run(pod, "c7", "inherits", 107, "0-2")
	if c := i.record("c7"); c.Lifecycle != LifecycleStarted || c.Management != ManagementUnfenced {
		t.Fatalf("record %+v", c)
	}
	noErr(t, i.p.LendNUMANode(ctx, "u1", "inherits", 2, "l1"))
	noErr(t, i.p.RevokeNUMANode(ctx, "u1", "inherits", 2, "l1"))
	i.expectUpdates()
	if got := i.cgroupMems("kubepods/u1/c7"); got != "" {
		t.Fatalf("cgroup mems %q", got)
	}
	_, err = i.p.StopContainer(ctx, pod, c7)
	noErr(t, err)
	i.wait(i.gone, "u1", "inherits")
}

// TestLendsAtCreationAndStart checks the lend rules of a container that
// has not started: a lend of a node that it was lent at its creation is
// done, another is an error; a started one is lent by an update.
func TestLendsAtCreationAndStart(t *testing.T) {
	e := newEnvWith(t, envOptions{leases: []state.Lease{readyLease("l1", "u1", "main", 2)}})
	e.p.Seed([]int{2, 3})
	e.hasMemory("0-3")
	ctx := context.Background()
	e.readyFence()
	pod := testPod("u1")
	c1 := testCtr(pod, "c1", "main", 101)
	if got := e.createMems(pod, c1); got != "0-2" {
		t.Fatalf("adjustment %s", got)
	}
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 2, "l1"))
	wantErr(t, e.p.LendNUMANode(ctx, "u1", "main", 3, "l2"), "lend NUMA node 3 to ns/pod-u1/main: container not started yet")
	e.expectUpdates()
	e.proc(101, "kubepods/u1/c1", "0-2", "0-2")
	e.startCtr(pod, c1)
	e.expectUpdates()
	noErr(t, e.p.LendNUMANode(ctx, "u1", "main", 3, "l2"))
	e.expectUpdates("c1 mems=0-3")
	if got := e.p.LentNodes("u1", "main"); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("lent %v", got)
	}
}

// recordExits makes exitOnNRILost send the exit statuses to the returned
// channel for the test.
func recordExits(t *testing.T) chan int {
	t.Helper()
	exits := make(chan int, 4)
	old := exitOnNRILost
	exitOnNRILost = func(code int) { exits <- code }
	t.Cleanup(func() { exitOnNRILost = old })
	return exits
}

// expectExits checks that the exit statuses sent to exits are want.
func expectExits(t *testing.T, exits chan int, want ...int) {
	t.Helper()
	close(exits)
	got := []int{}
	for code := range exits {
		got = append(got, code)
	}
	if !reflect.DeepEqual(got, append([]int{}, want...)) {
		t.Errorf("exit statuses %v, expected %v", got, want)
	}
}

// TestNRIConnectionLost: the plugin exits with status 1 when the runtime
// closes the connection after Start, before Start has returned too, and
// not when Stop or a done ctx closes it, after MarkStopping, or when Start
// failed.
func TestNRIConnectionLost(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		run  func(ctx context.Context, p *Plugin)
		want []int
	}{
		{"runtime closes", context.Background(), func(ctx context.Context, p *Plugin) {
			p.connStarted(ctx)
			p.onClose(ctx)
		}, []int{1}},
		{"closed before Start returned", context.Background(), func(ctx context.Context, p *Plugin) {
			p.onClose(ctx)
			p.connStarted(ctx)
		}, []int{1}},
		{"Start failed", context.Background(), func(ctx context.Context, p *Plugin) {
			p.onClose(ctx)
		}, nil},
		{"Stop", context.Background(), func(ctx context.Context, p *Plugin) {
			p.connStarted(ctx)
			p.Stop()
			p.onClose(ctx)
		}, nil},
		{"ctx done", done, func(ctx context.Context, p *Plugin) {
			p.connStarted(ctx)
			p.onClose(ctx)
		}, nil},
		// the runtime closes the connection while the driver stops
		{"MarkStopping", context.Background(), func(ctx context.Context, p *Plugin) {
			p.connStarted(ctx)
			p.MarkStopping()
			p.onClose(ctx)
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exits := recordExits(t)
			tc.run(tc.ctx, newEnv(t).p)
			expectExits(t, exits, tc.want...)
		})
	}
}

// TestCharNRILostLogLine pins the line of the loss of the NRI connection:
// logged with the exit, not after MarkStopping.
func TestCharNRILostLogLine(t *testing.T) {
	const lost = "NRI connection to the runtime lost, exiting to be restarted and connect again"
	ctx := context.Background()
	exits := recordExits(t)
	logs := klogtest.Capture(t, 3)
	p := newEnv(t).p
	p.connStarted(ctx)
	p.onClose(ctx)
	// klog writes an error line to the logs of every severity up to it
	n := strings.Count(logs(), lost)
	if n == 0 {
		t.Fatalf("no line %q:\n%s", lost, logs())
	}
	expectExits(t, exits, 1)

	exits = recordExits(t)
	p = newEnv(t).p
	p.connStarted(ctx)
	p.MarkStopping()
	p.onClose(ctx)
	if strings.Count(logs(), lost) != n {
		t.Fatalf("logged while stopping:\n%s", logs())
	}
	expectExits(t, exits)
}

// fakeRuntime is the runtime end of an NRI connection: it accepts a
// plugin on a socket, registers and configures it.
type fakeRuntime struct {
	sock       string
	registered chan struct{} // gets a value at RegisterPlugin
	conns      chan net.Conn // the connection of the configured plugin
}

func (rt *fakeRuntime) RegisterPlugin(context.Context, *nriapi.RegisterPluginRequest) (*nriapi.Empty, error) {
	rt.registered <- struct{}{}
	return &nriapi.Empty{}, nil
}

func (*fakeRuntime) UpdateContainers(context.Context, *nriapi.UpdateContainersRequest) (*nriapi.UpdateContainersResponse, error) {
	return &nriapi.UpdateContainersResponse{}, nil
}

// newFakeRuntime starts a fake runtime that serves one plugin.
func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	// a short path: the limit of a unix socket path is 108 bytes
	dir, err := os.MkdirTemp("", "nri")
	noErr(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	rt := &fakeRuntime{
		sock:       filepath.Join(dir, "nri.sock"),
		registered: make(chan struct{}, 1),
		conns:      make(chan net.Conn, 1),
	}
	l, err := net.Listen("unix", rt.sock)
	noErr(t, err)
	srv, err := ttrpc.NewServer()
	noErr(t, err)
	nriapi.RegisterRuntimeService(srv, rt)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted net.Conn
	)
	// cleanups run in reverse order: close everything, then wait
	t.Cleanup(wg.Wait)
	t.Cleanup(func() {
		_ = l.Close()
		_ = srv.Close()
		mu.Lock()
		defer mu.Unlock()
		if accepted != nil {
			_ = accepted.Close()
		}
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := l.Accept()
		if err != nil {
			t.Errorf("fake runtime: accept: %v", err)
			return
		}
		mu.Lock()
		accepted = conn
		mu.Unlock()
		mux := multiplex.Multiplex(conn)
		rl, err := mux.Listen(multiplex.RuntimeServiceConn)
		if err != nil {
			t.Errorf("fake runtime: listen: %v", err)
			return
		}
		go func() { _ = srv.Serve(context.Background(), rl) }()
		pc, err := mux.Open(multiplex.PluginServiceConn)
		if err != nil {
			t.Errorf("fake runtime: open plugin connection: %v", err)
			return
		}
		plugin := nriapi.NewPluginClient(ttrpc.NewClient(pc))
		// as the runtime does: configure after the registration
		select {
		case <-rt.registered:
		case <-time.After(10 * time.Second):
			t.Errorf("fake runtime: the plugin did not register")
			return
		}
		if _, err := plugin.Configure(context.Background(), &nriapi.ConfigureRequest{
			RuntimeName:         "fake",
			RuntimeVersion:      "v2.2.0",
			NRIVersion:          "v0.12.2",
			RegistrationTimeout: 5000,
			RequestTimeout:      2000,
		}); err != nil {
			t.Errorf("fake runtime: configure: %v", err)
			return
		}
		rt.conns <- conn
	}()
	return rt
}

// conn returns the connection of the configured plugin.
func (rt *fakeRuntime) conn(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-rt.conns:
		return conn
	case <-time.After(10 * time.Second):
		t.Fatal("fake runtime: the plugin was not configured")
		return nil
	}
}

// waitClosed waits until the stub has reported the connection closed.
func waitClosed(t *testing.T, p *Plugin) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		p.connMu.Lock()
		closed := p.conn == ConnClosed || p.conn == ConnStopped || p.conn == ConnLost
		p.connMu.Unlock()
		if closed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the stub did not report the connection closed")
		}
	}
}

// TestNRIConnectionLostStub: with the NRI stub connected to a runtime,
// the plugin exits when the runtime closes the connection, and not when
// Stop closes it (which the stub reports to onClose too); a plugin whose
// Start fails fences no container. The stub logs through klog.
func TestNRIConnectionLostStub(t *testing.T) {
	start := func(t *testing.T, rt *fakeRuntime) *Plugin {
		t.Helper()
		e := newEnvWith(t, envOptions{fencing: config.FencingNone})
		p, err := New(Options{Defaults: config.Container{Fencing: config.FencingNone}, Store: e.st, SysRoot: t.TempDir()})
		noErr(t, err)
		noErr(t, p.Start(context.Background(), Connection{Name: "test", Idx: "50", Socket: rt.sock}))
		return p
	}
	t.Run("runtime closes", func(t *testing.T) {
		exits := recordExits(t)
		rt := newFakeRuntime(t)
		p := start(t, rt)
		t.Cleanup(p.Stop)
		noErr(t, rt.conn(t).Close())
		select {
		case code := <-exits:
			if code != 1 {
				t.Errorf("exit status %d, expected 1", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no exit after the runtime closed the connection")
		}
		waitClosed(t, p)
		expectExits(t, exits)
	})
	t.Run("Stop", func(t *testing.T) {
		exits := recordExits(t)
		logs := klogtest.Capture(t, 0)
		rt := newFakeRuntime(t)
		p := start(t, rt)
		rt.conn(t)
		p.Stop()
		waitClosed(t, p)
		expectExits(t, exits)
		if !strings.Contains(logs(), "NRI stub: Created plugin 50-test") {
			t.Errorf("no line of the NRI stub in the klog output:\n%s", logs())
		}
	})
	t.Run("Start fails", func(t *testing.T) {
		e := newFenceEnv(t, "0-2")
		p, err := New(Options{Store: e.st, SysRoot: t.TempDir()})
		noErr(t, err)
		err = p.Start(context.Background(), Connection{Name: "test", Idx: "50", Socket: filepath.Join(t.TempDir(), "none.sock")})
		wantErr(t, err, "failed to start NRI plugin")
		logs := klogtest.Capture(t, 3)
		p.Fence(context.Background(), 2)
		if !strings.Contains(logs(), "NRI fence: NRI not connected, containers are not fenced from CXL NUMA node 2") {
			t.Fatalf("no warning:\n%s", logs())
		}
		wantErr(t, p.LendNUMANode(context.Background(), "u1", "app", 2, "l1"), "lending unavailable: NRI not connected")
	})
	if _, err := New(Options{}); err == nil {
		t.Fatal("a plugin without a store")
	}
}
