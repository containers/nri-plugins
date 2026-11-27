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

	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"
)

// Lends of NUMA nodes of CXL memory to containers: a lend adds the node to
// the cpuset.mems of the container, a revoke takes it off when its last
// lease revokes it, through NRI UpdateContainers (mems.go). The
// lends and revokes must not be called synchronously from an NRI event
// handler: the runtime serializes the events and the updates.

// ErrSteered is the error of a lend to a container whose memory a
// steering manager steers.
var ErrSteered = errors.New("container is memory-steered (container.steering.policy of its claims or the node defaults); lending is not supported together with steering yet")

// LendNUMANode lends the NUMA node to the newest container of the pod with
// the name for the lease: the NUMA node is in its cpuset.mems. Idempotent
// per NUMA node and lease.
func (p *Plugin) LendNUMANode(ctx context.Context, podUID, name string, numaNode int, lease string) error {
	return p.changeLend(ctx, lendChange{podUID: podUID, name: name, numaNode: numaNode, lease: lease, lend: true})
}

// RevokeNUMANode drops the lease of the NUMA node lent to the newest
// container of the pod with the name; the NUMA node leaves its cpuset.mems
// (its pages migrate back) when it has no lease left. Idempotent per NUMA
// node and lease; a container that is gone is not an error.
func (p *Plugin) RevokeNUMANode(ctx context.Context, podUID, name string, numaNode int, lease string) error {
	return p.changeLend(ctx, lendChange{podUID: podUID, name: name, numaNode: numaNode, lease: lease})
}

// lendChange is a lend (lend) or a revoke of a node to a container for a
// lease.
type lendChange struct {
	podUID, name string
	numaNode     int
	lease        string
	lend         bool
}

// op returns the change in words: lend or revoke.
func (l *lendChange) op() string {
	if l.lend {
		return "lend"
	}
	return "revoke"
}

// lendPlan is what a lend change makes of the cpuset.mems of a container.
type lendPlan struct {
	c *Container
	// leases are the leases of the node after the change, lent the
	// lent nodes.
	leases map[string]bool
	lent   cpuset.CPUSet
	// base are its own nodes after the change, mems its cpuset.mems.
	base cpuset.CPUSet
	mems string
	// needed: mems is sent; ownBase: base is recorded also without an
	// update (fencing none).
	needed, ownBase bool
	// inherits: it has the nodes of its parent (fencing none, an empty
	// cpuset.mems); unusable are the lent nodes that its cgroup lacks.
	inherits bool
	unusable cpuset.CPUSet
}

// changeLend applies a lend change to the lent nodes of the container and
// its cpuset.mems through NRI.
func (p *Plugin) changeLend(ctx context.Context, l lendChange) error {
	if l.numaNode < 0 {
		return fmt.Errorf("invalid NUMA node %d", l.numaNode)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.updateMu.Lock()
	defer p.updateMu.Unlock()

	p.mu.Lock()
	plan, err := p.planLend(l)
	if err != nil || plan == nil {
		p.mu.Unlock()
		return err
	}
	c := plan.c
	if !plan.needed {
		changed := c.Lends[l.numaNode][l.lease] != l.lend
		p.recordLend(plan, l, false)
		p.mu.Unlock()
		plan.logUnchanged(l, changed)
		return nil
	}
	if plan.base.IsEmpty() {
		p.mu.Unlock()
		return fmt.Errorf("%s NUMA node %d: ctr=%s: base memory nodes unknown, refusing to set cpuset.mems without them",
			l.op(), l.numaNode, c.name())
	}
	s := p.stub
	p.mu.Unlock()

	klog.V(3).Infof("NRI lend: %s NUMA node %d for %s: ctr=%s id=%s cpuset.mems=%s (base=%s lent=%s)", l.op(), l.numaNode, l.lease,
		c.name(), c.ID, plan.mems, plan.base, plan.lent)
	if err := applyMems(s, map[string]string{c.ID: plan.mems}, []string{c.ID})[c.ID]; err != nil {
		return fmt.Errorf("%s NUMA node %d: ctr=%s: %w", l.op(), l.numaNode, c.name(), err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctrs[c.ID] == c {
		p.recordLend(plan, l, true)
	}
	return nil
}

// planLend returns the plan of a lend change of the container, nil when the
// change is done without one, or an error. Called with mu held.
func (p *Plugin) planLend(l lendChange) (*lendPlan, error) {
	if p.stub == nil {
		return nil, fmt.Errorf("%s NUMA node %d: lending unavailable: NRI not connected", l.op(), l.numaNode)
	}
	c := p.lookup(l.podUID, l.name)
	switch {
	case c == nil && l.lend:
		return nil, fmt.Errorf("lend NUMA node %d: container %q of pod %s not found", l.numaNode, l.name, l.podUID)
	case c == nil:
		klog.V(3).Infof("NRI lend: revoke NUMA node %d from pod %s container %s: container gone", l.numaNode, l.podUID, l.name)
		return nil, nil
	case l.lend && c.steered():
		return nil, fmt.Errorf("lend NUMA node %d to %s: %w", l.numaNode, c.name(), ErrSteered)
	}
	if !c.started() {
		if done, err := lendNotStarted(c, l); done {
			return nil, err
		}
	}
	leases := c.Lends.with(l.numaNode, l.lease, l.lend)
	plan := &lendPlan{c: c, leases: leases, lent: c.Lends.after(l.numaNode, leases)}
	want, base, ok, err := wantMems(c, p.fenced, l.numaNode, plan.lent, p.memNodes())
	if err != nil {
		return nil, fmt.Errorf("%s NUMA node %d: ctr=%s: base memory nodes unknown: %w", l.op(), l.numaNode, c.name(), err)
	}
	plan.base = base
	switch {
	case c.Management == ManagementFenced:
		// sent when it differs from its cpuset.mems
		plan.mems = want.String()
		_, plan.needed = memsDiffer(c, want, "NRI lend")
	case c.Management == ManagementUnfenced && !ok:
		plan.inherits, plan.ownBase = true, true
		if l.lend {
			// unless a parent cgroup restricts its nodes
			plan.unusable = cgroupLacks(c.CgroupDir, plan.lent)
		}
	case ok:
		plan.mems, plan.ownBase = want.String(), true
		plan.needed = pinnedNeeded(plan, l, want)
	default:
		// steered: its cpuset.mems never changes
		plan.mems = c.SpecMems
	}
	return plan, nil
}

// lendNotStarted applies a lend change of a container whose task does not
// exist yet, and tells whether it is done: a container that has not
// started has only the lends of its creation (seeded); a lend of one of
// them is done, a revoke drops it, but from the spec of a fenced container
// by an update.
func lendNotStarted(c *Container, l lendChange) (bool, error) {
	seeded := c.Lends[l.numaNode][l.lease]
	switch {
	case l.lend && seeded:
		klog.V(4).Infof("NRI lend: lend NUMA node %d for %s: ctr=%s: lent at its creation, not started yet", l.numaNode, l.lease, c.name())
		return true, nil
	case l.lend:
		return true, fmt.Errorf("lend NUMA node %d to %s: container not started yet", l.numaNode, c.name())
	case !seeded:
		return true, nil
	case c.Management != ManagementFenced:
		// its spec has the nodes of the runtime: lent by updates only
		c.Lends = c.Lends.set(l.numaNode, c.Lends.with(l.numaNode, l.lease, false))
		klog.V(3).Infof("NRI lend: revoke NUMA node %d for %s: ctr=%s: not started yet", l.numaNode, l.lease, c.name())
		return true, nil
	}
	return false, nil
}

// pinnedNeeded tells whether a lend change of a container with a
// cpuset.mems of its own (fencing none) sends want: when the lent nodes or
// its base change, also when its cgroup lacks a lent node or has a revoked
// one, but not when its cgroup and its spec have it already. The claimed
// nodes that it had before any lend are its own (a container fenced before
// a switch to none has them); not those that a lend added.
func pinnedNeeded(plan *lendPlan, l lendChange, want cpuset.CPUSet) bool {
	c, base := plan.c, plan.base
	needed := !sameSet(plan.lent, c.Lends.nodes()) || !sameSet(base, c.Base.Union(c.OwnClaimed))
	switch {
	case !l.lend && !needed && len(plan.leases) == 0:
		// Not lent as far as we know, but a lend from before a plugin
		// restart that is not re-applied still shows in cpuset.mems.
		needed = !base.Contains(l.numaNode) && cgroupMemsContain(c.CgroupDir, l.numaNode)
	case l.lend && !needed:
		if missing := cgroupLacks(c.CgroupDir, plan.lent); !missing.IsEmpty() {
			// lost only if an update had them (not a seeded lend: lent at
			// its creation without an update)
			if memsContain(c.SpecMems, missing) {
				klog.Infof("NRI lend: ctr=%s id=%s: the cpuset.mems of its cgroup lacks the lent NUMA nodes %s, updating again", c.name(), c.ID, missing)
			}
			needed = true
		}
	}
	// Its cgroup and its spec have it already: a lend of a node of its
	// claims, or the revoke of one. The spec counts too: a later update of
	// its resources writes the cpuset.mems of the spec to the cgroup.
	return needed && (base.IsEmpty() || !sameMems(want, curMems(c)) || !sameMems(want, c.SpecMems))
}

// recordLend records the leases of the node of a lend change, its base
// when it is recorded without an update, and with an update (updated) its
// base and cpuset.mems. Called with mu held.
func (p *Plugin) recordLend(plan *lendPlan, l lendChange, updated bool) {
	c := plan.c
	c.Lends = c.Lends.set(l.numaNode, plan.leases)
	if updated || plan.ownBase {
		c.Base = plan.base
	}
	if updated {
		c.SpecMems = plan.mems
	}
}

// logUnchanged logs a lend change that sends no update.
//   - changed: the change added or dropped the lease
func (plan *lendPlan) logUnchanged(l lendChange, changed bool) {
	c := plan.c
	if plan.inherits && changed {
		klog.V(3).Infof("NRI lend: %s NUMA node %d for %s: ctr=%s id=%s: cpuset.mems left empty, it has the nodes of its parent (fencing none, lent=%s)",
			l.op(), l.numaNode, l.lease, c.name(), c.ID, plan.lent)
		if !plan.unusable.IsEmpty() {
			klog.Warningf("NRI lend: lend NUMA node %d for %s: ctr=%s id=%s: its cpuset.mems.effective lacks the lent NUMA nodes %s (a parent cgroup restricts it, or they have no memory): it cannot use them",
				l.numaNode, l.lease, c.name(), c.ID, plan.unusable)
		}
		return
	}
	klog.V(4).Infof("NRI lend: %s NUMA node %d for %s: ctr=%s: cpuset.mems unchanged (base=%s lent=%s, node leases %d)",
		l.op(), l.numaNode, l.lease, c.name(), plan.base, plan.lent, len(plan.leases))
}
