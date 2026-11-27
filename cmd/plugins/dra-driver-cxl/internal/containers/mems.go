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
	"fmt"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"
)

// The cpuset.mems of the containers that the plugin does not steer: one
// computation (wantMems) and one writer (applyMems), under the update token.
//
// A fenced container (FENCED) has cpuset.mems = wantMems: its own nodes
// without the fenced nodes, the CXL numa nodes of its allocation results
// and the nodes lent to it. CreateContainer gives a new container wantMems
// as an adjustment when that differs from its spec; a fence pass and a
// lend update it when wantMems differs from its cgroup (from its spec
// before its task exists).
//
// A container that is neither fenced nor steered (UNFENCED, fencing none)
// keeps the cpuset.mems that the runtime gave it. If that is empty (the
// usual case) the container has the memory nodes of its parent, every node
// with memory, also those whose memory goes online later, and a lend or a
// revoke only records the leases of the node: an update with an empty
// cpuset.mems changes nothing, so the container would not inherit any
// more, and a revoke would take a node that still has memory off it. A
// container with a cpuset.mems of its own (set by the runtime, such as by
// kubelet's memory manager, or fenced by a plugin with fencing allowed-mems
// before a switch to none: it keeps that until it is created again) gets
// its base + the lent nodes, never below the claimed nodes that it had
// before any lend; an update that its cgroup and its spec both have
// already is not sent (a later update of its resources writes the spec's
// cpuset.mems to the cgroup).
//
// Updates before the start: containerd applies an NRI update to the cgroup
// only of a container that it has marked started, which it does after task
// start, right before PostStartContainer. Of a container that it has
// created but not marked started it changes only the spec, which reaches
// the cgroup only if the task is created after it, and the task is created
// before the StartContainer event; one that is not in its store yet
// (during CreateContainer) it skips. Both are reported as applied. So the
// OnContainerStarted callbacks and the repair pass of the started container
// run from PostStartContainer, and lends and passes compare with the
// cpuset.mems of the cgroup of a container whose task exists (curMems), and
// update it again when it differs. Not covered: a revoke of a container
// that is not fenced between its StartContainer and its PostStartContainer
// reaches its spec only, and its cgroup keeps the node (with fencing none
// the plugin keeps no container off a node, and offlining migrates the
// pages of the node whatever cpuset.mems says).

// wantMems returns the cpuset.mems that the plugin gives a container, its
// base, and false when the plugin gives it none. Fenced: its own nodes
// without the fenced nodes and the changed node (the base), with the nodes
// of its claims and the lent nodes; its own nodes are the cpuset.mems of its
// spec, the nodes with memory (mem) when the spec has none, an error when
// they are unknown. Fencing none with a cpuset.mems of its own: its own
// nodes without the changed node but with the claimed nodes that it had
// before any lend (the base), with the lent nodes. Fencing none with an
// empty cpuset.mems (it inherits the nodes of its parent: an update would
// pin it) and steered: none.
//   - fenced: the fenced nodes
//   - changed: the node of a lend change, -1 for none
//   - lent: the nodes lent to it
func wantMems(c *Container, fenced cpuset.CPUSet, changed int, lent cpuset.CPUSet, mem *memNodes) (cpuset.CPUSet, cpuset.CPUSet, bool, error) {
	without := cpuset.New()
	if changed >= 0 {
		without = cpuset.New(changed)
	}
	switch {
	case c.Management == ManagementFenced:
		own, err := ownMems(c, mem)
		if err != nil {
			return cpuset.New(), cpuset.New(), false, err
		}
		base := own.Difference(fenced).Difference(without)
		return base.Union(c.Claimed, lent), base, true, nil
	case c.Management == ManagementUnfenced && curMems(c) != "":
		base := c.Base.Difference(without).Union(c.OwnClaimed)
		return base.Union(lent), base, true, nil
	case c.Management == ManagementUnfenced:
		return cpuset.New(), c.Base.Difference(without), false, nil
	}
	return cpuset.New(), c.Base, false, nil
}

// ownMems returns the memory nodes a container has of its own: the
// cpuset.mems of its spec, or the nodes with memory (mem) when the spec
// has none.
func ownMems(c *Container, mem *memNodes) (cpuset.CPUSet, error) {
	if c.SpecMems == "" {
		return mem.get()
	}
	own, err := cpuset.Parse(c.SpecMems)
	if err != nil {
		return cpuset.New(), fmt.Errorf("invalid cpuset.mems %q: %w", c.SpecMems, err)
	}
	return own, nil
}

// passMems returns wantMems of a fenced container for a fence pass and its
// creation, and an error if its own nodes are unknown or nothing is left.
func passMems(c *Container, fenced cpuset.CPUSet, mem *memNodes) (cpuset.CPUSet, error) {
	want, _, _, err := wantMems(c, fenced, -1, c.Lends.nodes(), mem)
	if err == nil && want.IsEmpty() {
		own, _ := ownMems(c, mem)
		err = fmt.Errorf("no memory nodes left of %s without the fenced NUMA nodes %s", own, fenced)
	}
	return want, err
}

// fenceCreated returns the cpuset.mems that a new container gets ("" for
// no change: the spec's is kept) and the fenced nodes, and records it as
// its spec's. Called with mu held.
func (p *Plugin) fenceCreated(c *Container) (string, cpuset.CPUSet) {
	fenced := p.fenced
	if c.Management != ManagementFenced || fenced.IsEmpty() {
		return "", fenced
	}
	want, err := passMems(c, fenced, p.memNodes())
	if err != nil {
		klog.Warningf("NRI CreateContainer: ctr=%s: cpuset.mems left as it is: %v", c.name(), err)
		return "", fenced
	}
	if sameMems(want, c.SpecMems) {
		return "", fenced
	}
	c.SpecMems = want.String()
	return c.SpecMems, fenced
}

// memsDiffer returns the cpuset.mems of a container (curMems) and whether
// it or the cpuset.mems of its spec is not want (a later update of its
// resources writes the spec's to the cgroup), and logs it when its cgroup
// has not got an update that the plugin has recorded.
func memsDiffer(c *Container, want cpuset.CPUSet, prefix string) (string, bool) {
	cur := curMems(c)
	if sameMems(want, cur) {
		return cur, !sameMems(want, c.SpecMems)
	}
	if sameMems(want, c.SpecMems) {
		klog.Infof("%s: ctr=%s id=%s: the cpuset.mems of its cgroup is %q, not %s as updated, updating again",
			prefix, c.name(), c.ID, cur, c.SpecMems)
	}
	return cur, true
}

// applyMems sets the cpuset.mems of containers with one NRI
// UpdateContainers and returns the error of each container whose update
// failed. Called with the update token held and mu not held.
func applyMems(s Stub, mems map[string]string, order []string) map[string]error {
	updates := make([]*api.ContainerUpdate, 0, len(order))
	for _, id := range order {
		u := &api.ContainerUpdate{ContainerId: id}
		u.SetLinuxCPUSetMems(mems[id])
		updates = append(updates, u)
	}
	failed := map[string]error{}
	notUpdated, err := s.UpdateContainers(updates)
	for _, id := range order {
		if err != nil {
			failed[id] = fmt.Errorf("NRI UpdateContainers(cpuset.mems=%s): %w", mems[id], err)
		}
	}
	for _, u := range notUpdated {
		if id := u.GetContainerId(); err == nil && mems[id] != "" {
			failed[id] = fmt.Errorf("NRI UpdateContainers(cpuset.mems=%s): update failed in the runtime", mems[id])
		}
	}
	return failed
}

// memsContain tells whether the cpuset.mems mems has the nodes.
func memsContain(mems string, nodes cpuset.CPUSet) bool {
	set, err := cpuset.Parse(mems)
	return err == nil && nodes.IsSubsetOf(set)
}

// sameMems tells whether cpuset.mems has the nodes of want.
func sameMems(want cpuset.CPUSet, mems string) bool {
	set, err := cpuset.Parse(mems)
	return err == nil && sameSet(want, set)
}

// sameSet compares sets by content (CPUSet.Equals tells a zero set from an
// empty one).
func sameSet(a, b cpuset.CPUSet) bool {
	return a.IsSubsetOf(b) && b.IsSubsetOf(a)
}
