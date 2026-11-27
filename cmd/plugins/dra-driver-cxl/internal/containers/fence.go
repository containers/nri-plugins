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

	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// The fence (defaults.container.fencing allowed-mems): the NUMA nodes F on
// which the driver onlines CXL memory, numa slices of local memdevs and pool
// devices in mode numa. A fenced container has a fenced node in its
// cpuset.mems only when one of its allocation results has numa memory on it or
// it has a lease of it (wantMems). F is seeded at the start from the grants of
// this boot (Seed) and only grows; a node is fenced before its memory goes
// online: Fence returns after the containers have been updated through NRI, or
// at the end of its context, the pass finishing in the background. With
// fencing none F stays empty.
//
// A fence pass applies wantMems to the fenced containers of the table under
// the update token, like a lend, so that lends and passes never compute from
// stale sets. A pass runs when a node is fenced, when fencing of running
// containers becomes ready (NRI synchronized and the lends restored), for
// one container at its PostStartContainer and periodically (the repair
// passes), in the background but for Fence. A failed update is
// tried again by the next pass.

// Seed adds the nodes to the fenced nodes without a pass.
func (p *Plugin) Seed(nodes []int) {
	set := cpuset.New(nonNegative(nodes)...)
	if !p.fencing || set.IsEmpty() {
		return
	}
	klog.Infof("NRI fence: fencing CXL NUMA nodes %s of prepared claims", set)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fenced = p.fenced.Union(set)
}

// Fenced returns the fenced nodes.
func (p *Plugin) Fenced() cpuset.CPUSet {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fenced
}

// Fence adds a node to the fenced nodes and returns once the containers are
// updated through NRI, or at the end of ctx: the node is recorded and the
// containers updated in the background then. One that is fenced already
// returns after the pass that fenced it. Before fencing of running
// containers is ready, and without NRI, the node is only recorded. With
// fencing none it does nothing.
func (p *Plugin) Fence(ctx context.Context, node int) {
	if !p.fencing || node < 0 {
		return
	}
	done := make(chan struct{})
	p.passes.Add(1)
	go func() {
		defer p.passes.Done()
		defer close(done)
		p.fence(node)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		klog.V(3).Infof("NRI fence: fencing CXL NUMA node %d goes on in the background: %v", node, ctx.Err())
	}
}

// fence adds a node to the fenced nodes and runs the pass that updates the
// containers, after the pass of a Fence that runs.
func (p *Plugin) fence(node int) {
	p.fenceMu.Lock()
	defer p.fenceMu.Unlock()
	p.mu.Lock()
	if p.fenced.Contains(node) {
		p.mu.Unlock()
		return
	}
	p.fenced = p.fenced.Union(cpuset.New(node))
	nodes := p.fenced
	p.mu.Unlock()

	klog.Infof("NRI fence: fencing CXL NUMA node %d (fenced NUMA nodes %s)", node, nodes)
	if p.startFailed() {
		klog.Warningf("NRI fence: NRI not connected, containers are not fenced from CXL NUMA node %d", node)
		return
	}
	p.pass(context.Background(), "")
}

// LendsRestored tells that the lends of a restarted plugin are applied
// again. Fencing of running containers starts when NRI has synchronized
// too.
func (p *Plugin) LendsRestored() {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.readiness {
	case ReadinessLendsRestored, ReadinessReady:
		return
	case ReadinessSynchronized:
		p.ready(ReadinessReady, TriggerLendsRestored)
	default:
		p.ready(ReadinessLendsRestored, TriggerLendsRestored)
	}
}

// synchronized records the first Synchronize, or a later one, in the
// readiness of fencing. Called with mu held.
func (p *Plugin) synchronized() {
	to := ReadinessSynchronized
	if p.readiness == ReadinessLendsRestored || p.readiness == ReadinessReady {
		to = ReadinessReady
	}
	p.ready(to, TriggerSynchronize)
}

// ready applies a transition of the readiness of fencing; at READY a fence
// pass starts in the background. With fencing none there is no pass, but
// the first READY is logged all the same: it tells when the lends of a
// restarted plugin are back. Called with mu held.
func (p *Plugin) ready(to Readiness, trigger Trigger) {
	from := p.readiness
	if from == "" {
		from = ReadinessWaiting
	}
	p.readiness = from
	if err := fsm.Apply(&ReadinessTable, &p.readiness, to, trigger); err != nil {
		klog.Warningf("NRI fence: %v", err)
		return
	}
	if to != ReadinessReady {
		return
	}
	if !p.fencing {
		if from != ReadinessReady {
			klog.V(3).Info("NRI fence: lends restored (fencing none)")
		}
		return
	}
	if from != ReadinessReady {
		klog.V(3).Info("NRI fence: lends restored, fencing running containers")
	}
	p.background("")
}

// fenceAtStart tells whether a started container gets the repair pass of
// its own: it is fenced, there are fenced nodes and fencing of running
// containers is ready. Updates of the container before its start may have
// reached its spec only, also that of a pass while the plugin synchronized
// as the runtime was creating its task. Called with mu held.
func (p *Plugin) fenceAtStart(c *Container) bool {
	return p.readiness == ReadinessReady && c.Management == ManagementFenced && !p.fenced.IsEmpty()
}

// repair starts the repair pass of one started container in the
// background: its cgroup gets its wantMems if it has not got it.
func (p *Plugin) repair(id string) {
	p.background(id)
}

// Repair runs a fence pass over every fenced container (the periodic
// repair pass): an update of its cpuset.mems that failed is tried again.
func (p *Plugin) Repair(ctx context.Context) {
	p.pass(ctx, "")
}

// background runs a fence pass of the container id, of all if id is "",
// in the background.
func (p *Plugin) background(id string) {
	p.passes.Add(1)
	go func() {
		defer p.passes.Done()
		p.pass(context.Background(), id)
	}()
}

// pass sets cpuset.mems of the fenced containers of the table to wantMems
// where that changes it (curMems: the cgroup of a container whose task
// exists), with one NRI UpdateContainers, once fencing of running
// containers is ready; of the container with the id only if it is not "".
// A pass of all containers logs the violations of I11 of the containers
// it found started ("containers invariant: ...", V(4)), but not of those
// whose update failed: they violate it until the next pass.
func (p *Plugin) pass(ctx context.Context, only string) {
	if ctx.Err() != nil {
		return
	}
	// not waiting for a lend when not ready: readiness never ends
	p.mu.Lock()
	ready := p.readiness == ReadinessReady
	p.mu.Unlock()
	if !ready {
		return
	}
	p.updateMu.Lock()
	defer p.updateMu.Unlock()

	p.mu.Lock()
	fenced, s := p.fenced, p.stub
	if s == nil || fenced.IsEmpty() {
		p.mu.Unlock()
		return
	}
	mem := p.memNodes()
	changes := p.passChanges(fenced, mem, only)
	var checked map[string]bool
	if only == "" {
		checked = p.started()
	}
	if len(changes) == 0 {
		p.logViolations(fenced, mem, checked)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	mems := make(map[string]string, len(changes))
	order := make([]string, 0, len(changes))
	for _, ch := range changes {
		mems[ch.c.ID] = ch.mems
		order = append(order, ch.c.ID)
	}
	failed := applyMems(s, mems, order)

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ch := range changes {
		if err := failed[ch.c.ID]; err != nil {
			klog.Warningf("NRI fence: ctr=%s id=%s: %v", ch.c.name(), ch.c.ID, err)
			continue
		}
		if p.ctrs[ch.c.ID] == ch.c {
			ch.c.SpecMems = ch.mems
		}
		klog.V(3).Infof("NRI fence: ctr=%s id=%s cpuset.mems=%s (was %q, fenced NUMA nodes %s)", ch.c.name(), ch.c.ID, ch.mems, ch.was, fenced)
	}
	klog.Infof("NRI fence: %d containers updated, %d failed (fenced NUMA nodes %s)", len(changes)-len(failed), len(failed), fenced)
	for id := range failed {
		delete(checked, id)
	}
	p.logViolations(fenced, mem, checked)
}

// logViolations logs the violations of I11 of the containers in checked
// with the fenced nodes and the memory nodes of a pass, nothing if checked
// is nil. Called with mu held.
func (p *Plugin) logViolations(fenced cpuset.CPUSet, mem *memNodes, checked map[string]bool) {
	if checked == nil {
		return
	}
	for _, v := range p.violations(fenced, mem, checked) {
		klog.V(4).Info("containers invariant: " + v)
	}
}

// passChange is an update of the cpuset.mems of a container by a fence
// pass.
type passChange struct {
	c         *Container
	was, mems string
}

// passChanges returns the updates of a fence pass of the fenced containers
// in the order of their creation, of the container only if it is not "".
// Called with mu held.
func (p *Plugin) passChanges(fenced cpuset.CPUSet, mem *memNodes, only string) []passChange {
	var changes []passChange
	for _, c := range p.sorted() {
		if (only != "" && c.ID != only) || c.Management != ManagementFenced {
			continue
		}
		want, err := passMems(c, fenced, mem)
		if err != nil {
			klog.Warningf("NRI fence: ctr=%s id=%s: %v", c.name(), c.ID, err)
			continue
		}
		if was, differ := memsDiffer(c, want, "NRI fence"); differ {
			changes = append(changes, passChange{c: c, was: was, mems: want.String()})
		}
	}
	return changes
}
