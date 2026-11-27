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
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Options configure a Plugin.
type Options struct {
	// Defaults are the container settings of the driver configuration
	// (defaults.container); their fencing is the node's.
	Defaults config.Container
	// Store keeps the Steered records and the leases that seed the lends.
	Store *store.Store
	// Leases: the lease broker lends nodes, and the READY numa leases of
	// the store seed the lends of a container of their requester.
	Leases bool
	// SysRoot is the root of the sysfs mount: the NUMA nodes with memory
	// are in <SysRoot>/sys/devices/system/node.
	SysRoot string
	// Results are the prepared claims of the DRA drivers.
	Results []Results
}

// Connection are the NRI settings of the plugin: its name and index in
// the runtime's chain of plugins and the runtime's socket ("" for the
// defaults).
type Connection struct {
	Name, Idx, Socket string
	// Stub, if not nil, is the connection to the runtime: the plugin uses
	// it instead of an NRI stub of the settings.
	Stub Stub
}

// Plugin is the NRI plugin of the node: the table of the containers, the
// fenced NUMA nodes and the lends of nodes to running containers. It is the
// only writer of the cpuset.mems of the containers that it does not steer.
//
// Identity: one per driver process.
//
// States: Readiness, the readiness of fencing of the running containers:
// WAITING (Fence records a node only, no pass), SYNCHRONIZED,
// LENDS_RESTORED, READY once NRI has synchronized and the lends of the
// leases are restored, never back. Conn, the NRI connection: NEW,
// STARTED, FAILED (Start failed: the containers are not fenced, lends
// fail), CLOSED (before Start returned), STOPPING, STOPPED, LOST (the
// process exits).
//
// Transitions: ReadinessTable and ConnTable, and the tables of Container; a
// transition not in them is refused.
//
// Invariants: the fenced nodes only grow within a process; a fence pass and
// a lend run under the update token.
//
// Persistence: the Steered records in the store; the rest is in memory and
// rebuilt at a start: the fenced nodes seeded from the grants of this boot
// (Seed), the table by Synchronize, the lends from the READY leases of the
// store and the broker's first reconcile.
//
// Locks and waits: the mutex fenceMu serializes Fence across its pass; the update
// token (token) is held from computing wantMems to recording the result of
// UpdateContainers; the table mutex (mu) is never held across
// UpdateContainers, a store commit or a callback, as the runtime serializes
// NRI events and unsolicited updates with one lock; connMu guards Conn.
// Order: fenceMu, token, mu, connMu.
type Plugin struct {
	opts Options
	// fencing: defaults.container.fencing is allowed-mems.
	fencing bool
	// hasMemory is the sysfs file of the NUMA nodes with memory.
	hasMemory string

	fenceMu  sync.Mutex
	updateMu sync.Mutex

	mu sync.Mutex
	// stub is the NRI stub, nil while NRI is not connected.
	stub Stub
	ctrs map[string]*Container
	seq  uint64
	// fenced are the fenced NUMA nodes, F.
	fenced cpuset.CPUSet
	// synced is closed at the first Synchronize.
	synced     chan struct{}
	goneFns    []func(pod state.PodRef, container string)
	startedFns []func(pod state.PodRef, container string)
	// readiness is the readiness of fencing (ReadinessTable).
	readiness Readiness
	// passes are the fence passes in the background.
	passes sync.WaitGroup

	// connMu guards conn: a leaf lock.
	connMu sync.Mutex
	// conn is the state of the NRI connection (ConnTable).
	conn ConnState
}

// Stub is the part of stub.Stub that the plugin uses.
type Stub interface {
	Start(context.Context) error
	Stop()
	UpdateContainers([]*api.ContainerUpdate) ([]*api.ContainerUpdate, error)
}

// hasMemoryFile lists the NUMA nodes with memory, under the sysfs root.
const hasMemoryFile = "sys/devices/system/node/has_memory"

// exitOnNRILost ends the process when the runtime has closed the NRI
// connection (a variable for tests).
var exitOnNRILost = os.Exit

// New returns the plugin of the options, not connected to NRI: Fence
// records the nodes until Start. It returns an error without a store.
func New(opts Options) (*Plugin, error) {
	if opts.Store == nil {
		return nil, errors.New("containers: no store")
	}
	p := &Plugin{
		opts:      opts,
		fencing:   opts.Defaults.Fencing == "" || opts.Defaults.Fencing == config.FencingAllowedMems,
		hasMemory: filepath.Join(opts.SysRoot, "/", hasMemoryFile),
		ctrs:      map[string]*Container{},
		fenced:    cpuset.New(),
		synced:    make(chan struct{}),
		readiness: ReadinessWaiting,
		conn:      ConnNew,
	}
	return p, nil
}

// Start connects the plugin to the runtime's NRI socket, or through
// c.Stub. The plugin receives the container events from now on; a loss of
// the connection that Stop does not cause ends the process. Without a
// connection the plugin fences no container and lends fail.
func (p *Plugin) Start(ctx context.Context, c Connection) error {
	s := c.Stub
	if s == nil {
		var err error
		if s, err = p.newStub(ctx, c); err != nil {
			p.connect(nil)
			return fmt.Errorf("failed to create NRI plugin stub: %w", err)
		}
	}
	p.mu.Lock()
	p.stub = s
	p.mu.Unlock()
	if err := s.Start(ctx); err != nil {
		p.connect(nil)
		return fmt.Errorf("failed to start NRI plugin: %w", err)
	}
	p.connStarted(ctx)
	p.connect(s)
	klog.V(3).Info("NRI plugin started")
	return nil
}

// newStub returns the NRI stub of the settings of c.
func (p *Plugin) newStub(ctx context.Context, c Connection) (Stub, error) {
	opts := []stub.Option{stub.WithOnClose(func() { p.onClose(ctx) }), stub.WithLogger(stubLogger{})}
	if c.Name != "" {
		opts = append(opts, stub.WithPluginName(c.Name))
	}
	if c.Idx != "" {
		opts = append(opts, stub.WithPluginIdx(c.Idx))
	}
	if c.Socket != "" {
		opts = append(opts, stub.WithSocketPath(c.Socket))
	}
	return stub.New(p, opts...)
}

// stubLogger writes the messages of the NRI stub with klog, its debug
// messages at verbosity 4.
type stubLogger struct{}

func (stubLogger) Debugf(_ context.Context, format string, args ...any) {
	klog.V(4).Infof("NRI stub: "+format, args...)
}

func (stubLogger) Infof(_ context.Context, format string, args ...any) {
	klog.Infof("NRI stub: "+format, args...)
}

func (stubLogger) Warnf(_ context.Context, format string, args ...any) {
	klog.Warningf("NRI stub: "+format, args...)
}

func (stubLogger) Errorf(_ context.Context, format string, args ...any) {
	klog.Errorf("NRI stub: "+format, args...)
}

// SetResults sets the prepared claims of the DRA drivers that the
// containers use. Not guarded: it comes before Start, and the NRI events
// read them without the table mutex.
func (p *Plugin) SetResults(results ...Results) {
	p.opts.Results = results
}

// Fencing tells whether the plugin fences the CXL NUMA nodes of the driver:
// defaults.container.fencing is allowed-mems.
func (p *Plugin) Fencing() bool {
	return p.fencing
}

// connect sets the stub of the connection, nil when NRI did not start
// (the connection FAILED).
func (p *Plugin) connect(s Stub) {
	p.mu.Lock()
	p.stub = s
	p.mu.Unlock()
	if s == nil {
		p.connMu.Lock()
		defer p.connMu.Unlock()
		p.connTo(ConnFailed, TriggerNRIStart)
	}
}

// connTo applies a transition of the NRI connection; one that its table
// refuses is logged and leaves it as it is. Called with connMu held.
func (p *Plugin) connTo(to ConnState, trigger Trigger) {
	if p.conn == "" {
		p.conn = ConnNew
	}
	if err := fsm.Apply(&ConnTable, &p.conn, to, trigger); err != nil {
		klog.Warningf("NRI: %v", err)
	}
}

// startFailed tells whether the stub could not be made or started.
func (p *Plugin) startFailed() bool {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	return p.conn == ConnFailed
}

// Stop shuts the plugin down: the steering managers stop, then the
// connection closes, which is not a loss.
func (p *Plugin) Stop() {
	p.MarkStopping()
	p.mu.Lock()
	var steered []*Container
	var managers []func()
	for _, c := range p.ctrs {
		c.Plan = nil
		if c.manager != nil {
			steered, managers = append(steered, c), append(managers, c.manager.Stop)
			c.manager = nil
		}
	}
	s := p.stub
	p.mu.Unlock()
	ids := make([]string, 0, len(steered))
	for i, c := range steered {
		managers[i]()
		ids = append(ids, c.ID)
	}
	if len(ids) > 0 {
		p.removeSteered(ids...)
	}
	if s != nil {
		s.Stop()
		klog.V(3).Info("NRI plugin stopped")
	}
}

// MarkStopping makes a close of the connection from now on not a loss:
// the driver is stopping, and the runtime may close the connection while
// the driver stops (a node shutdown stops both).
func (p *Plugin) MarkStopping() {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	switch p.conn {
	case ConnStopping, ConnStopped, ConnLost:
		p.connTo(p.conn, TriggerStopping)
	default:
		p.connTo(ConnStopping, TriggerStopping)
	}
}

// Stopping tells whether a close of the connection is no loss any more.
func (p *Plugin) Stopping() bool {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	return p.conn == ConnStopping || p.conn == ConnStopped
}

// onClose records that the connection to the runtime has closed and exits
// if that is a loss. The stub never connects again: the process exits so
// that it is restarted (by the DaemonSet, or by Restart= of a systemd unit)
// and connects again; the restarted driver gets its state back from the
// store and Synchronize, as after a SIGKILL. A close while stopping or
// after ctx is done is not a loss, nor is one before Start has succeeded.
func (p *Plugin) onClose(ctx context.Context) {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	switch p.conn {
	case "", ConnNew:
		p.connTo(ConnClosed, TriggerNRIClose)
	case ConnStarted:
		p.closedLocked(ctx, TriggerNRIClose)
	case ConnStopping:
		p.connTo(ConnStopped, TriggerNRIClose)
		klog.V(3).Info("NRI connection to the runtime closed (stopping)")
	default:
		p.connTo(p.conn, TriggerNRIClose)
	}
}

// connStarted records that Start of the stub has succeeded, and exits if
// the runtime has closed the connection already.
func (p *Plugin) connStarted(ctx context.Context) {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	switch p.conn {
	case "", ConnNew:
		p.connTo(ConnStarted, TriggerNRIStart)
	case ConnClosed:
		p.closedLocked(ctx, TriggerNRIStart)
	default:
		p.connTo(p.conn, TriggerNRIStart)
	}
}

// closedLocked records a close of a started connection: STOPPED once ctx
// is done, else LOST, and the process ends with exit status 1. Called
// with connMu held: Stop cannot come between the decision and the exit.
func (p *Plugin) closedLocked(ctx context.Context, trigger Trigger) {
	if ctx.Err() != nil {
		p.connTo(ConnStopped, trigger)
		klog.V(3).Info("NRI connection to the runtime closed (stopping)")
		return
	}
	p.connTo(ConnLost, trigger)
	klog.Error("NRI connection to the runtime lost, exiting to be restarted and connect again")
	klog.Flush()
	exitOnNRILost(1)
}

// WaitSynchronized waits until the first Synchronize has filled the
// container table, and returns an error when ctx is done first.
func (p *Plugin) WaitSynchronized(ctx context.Context) error {
	select {
	case <-p.synced:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("NRI not synchronized yet: %w", ctx.Err())
	}
}

// OnContainerGone registers fn, called in its own goroutine for each
// container that stops or is removed.
func (p *Plugin) OnContainerGone(fn func(pod state.PodRef, container string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.goneFns = append(p.goneFns, fn)
}

// OnContainerStarted registers fn, called in its own goroutine for each
// container that the runtime has marked started: it applies updates to its
// cgroup from now on.
func (p *Plugin) OnContainerStarted(fn func(pod state.PodRef, container string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startedFns = append(p.startedFns, fn)
}

// fireGone runs the OnContainerGone callbacks of a container in the
// background. Called with mu held.
func (p *Plugin) fireGone(c *Container) {
	if lent := c.Lends.nodes(); !lent.IsEmpty() {
		klog.V(3).Infof("NRI lend: ctr=%s id=%s gone with lent nodes %s", c.name(), c.ID, lent)
	}
	pod, name := c.pod(), c.Name
	for _, fn := range p.goneFns {
		go fn(pod, name)
	}
}

// fireStarted runs the OnContainerStarted callbacks of a container in the
// background. Called with mu held.
func (p *Plugin) fireStarted(c *Container) {
	pod, name := c.pod(), c.Name
	for _, fn := range p.startedFns {
		go fn(pod, name)
	}
}

// ContainerByPid returns the pod and the name of the container whose cgroup
// has the process pid: the record with the longest cgroup directory that
// has its cgroup, or one whose cgroup is not resolved whose ID the cgroup
// names; false for none.
func (p *Plugin) ContainerByPid(pid int) (state.PodRef, string, bool) {
	c, ok := p.byPid(pid)
	if !ok {
		return state.PodRef{}, "", false
	}
	return c.pod(), c.Name, true
}

// byPid returns a copy of the record of the container whose cgroup has the
// process pid.
func (p *Plugin) byPid(pid int) (Container, bool) {
	dir, err := pidCgroupDir(pid)
	if err != nil {
		klog.V(4).Infof("NRI lend: pid %d: %v", pid, err)
		return Container{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *Container
	for _, c := range p.ctrs {
		if inCgroup(c, dir) && (best == nil || len(c.CgroupDir) > len(best.CgroupDir)) {
			best = c
		}
	}
	if best == nil {
		return Container{}, false
	}
	return *best, true
}

// Running tells whether the table has a record of the container name of
// the pod, created or started.
func (p *Plugin) Running(podUID, container string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lookup(podUID, container) != nil
}

// Record returns a copy of the record of the container id, and false for
// none.
func (p *Plugin) Record(id string) (Container, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.ctrs[id]
	if !ok {
		return Container{}, false
	}
	out := *c
	out.Lends = nil
	for n, leases := range c.Lends {
		out.Lends = out.Lends.set(n, maps.Clone(leases))
	}
	out.manager, out.starting, out.startEnd = nil, nil, nil
	return out, true
}

// LentNodes returns the nodes lent to the newest container of the pod with
// the name.
func (p *Plugin) LentNodes(podUID, name string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.lookup(podUID, name)
	if c == nil {
		return nil
	}
	return c.Lends.nodes().List()
}

// lookup returns the newest record of the container name of the pod, nil
// for none. Called with mu held.
func (p *Plugin) lookup(podUID, name string) *Container {
	var best *Container
	for _, c := range p.ctrs {
		if c.PodUID == podUID && c.Name == name && (best == nil || c.seq > best.seq) {
			best = c
		}
	}
	return best
}

// sorted returns the records in the order of their creation. Called with mu
// held.
func (p *Plugin) sorted() []*Container {
	out := make([]*Container, 0, len(p.ctrs))
	for _, c := range p.ctrs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// ctrName returns the name of a container of a pod: namespace/pod/name.
func ctrName(pod *api.PodSandbox, ctr *api.Container) string {
	return pod.GetNamespace() + "/" + pod.GetName() + "/" + ctr.GetName()
}
