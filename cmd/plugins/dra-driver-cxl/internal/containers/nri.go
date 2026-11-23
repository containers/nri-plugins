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

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// The NRI events of the plugin: the transitions of the records of the
// containers (LifecycleTable, ManagementTable).

var _ stub.PostStartContainerInterface = (*Plugin)(nil)

// Synchronize builds the container table from the runtime's pods and
// containers, and starts a fence pass in the background when fencing of
// running containers is ready. It comes once per process: the runtime
// synchronizes a plugin once per connection, and a lost connection ends
// the process. A container is managed as its results say, but never
// steered: steering is planned at CreateContainer and runs in the process
// that started the container; a manager of a previous process is released
// (releaseSteering). It gets the lends of its READY numa leases. The
// Steered records keep the managers of this process only from now on.
func (p *Plugin) Synchronize(_ context.Context, pods []*api.PodSandbox, ctrs []*api.Container) ([]*api.ContainerUpdate, error) {
	podByID := make(map[string]*api.PodSandbox, len(pods))
	for _, pod := range pods {
		podByID[pod.GetId()] = pod
	}
	p.mu.Lock()
	for _, ctr := range ctrs {
		if !listed(ctr.GetState()) {
			continue
		}
		pod, ok := podByID[ctr.GetPodSandboxId()]
		if !ok {
			pod = &api.PodSandbox{Id: ctr.GetPodSandboxId()}
		}
		p.syncContainer(pod, ctr)
	}
	var keep []string
	for id, c := range p.ctrs {
		if c.manager != nil {
			keep = append(keep, id)
		}
	}
	p.mu.Unlock()
	p.syncSteered(keep)

	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.synced:
	default:
		close(p.synced)
	}
	klog.V(3).Infof("NRI Synchronize: %d pods, %d containers tracked", len(pods), len(p.ctrs))
	p.synchronized()
	return nil, nil
}

// listed tells whether Synchronize takes a container of the state into the
// table: created, running or paused.
func listed(s api.ContainerState) bool {
	return s == api.ContainerState_CONTAINER_CREATED || s == api.ContainerState_CONTAINER_RUNNING ||
		s == api.ContainerState_CONTAINER_PAUSED
}

// syncContainer adds a container that Synchronize lists to the table; one
// that the table has is left as it is. Called with mu held.
func (p *Plugin) syncContainer(pod *api.PodSandbox, ctr *api.Container) {
	if c := p.ctrs[ctr.GetId()]; c != nil {
		klog.Warningf("NRI Synchronize: ctr=%s id=%s has a record already, left as it is", c.name(), c.ID)
		return
	}
	c := p.syncNew(pod, ctr, ctr.GetState() != api.ContainerState_CONTAINER_CREATED)
	c.SpecMems = specMems(ctr)
	klog.V(4).Infof("NRI Synchronize: ctr=%s id=%s pid=%d cgroup=%s baseMems=%s memory=%s",
		c.name(), c.ID, c.Pid, c.CgroupDir, c.Base, c.Management.text())
}

// syncNew returns the record of a container that Synchronize finds and the
// plugin has none of: its management as CreateContainer would decide it,
// but not steered by this process; a manager of a previous process
// released; its kept lends. A container whose settings do not combine any
// more is taken as steered: neither fenced nor lent to. Called with mu
// held.
func (p *Plugin) syncNew(pod *api.PodSandbox, ctr *api.Container, running bool) *Container {
	name := ctrName(pod, ctr)
	d, err := p.decide(name, p.results(pod, name, extractResults(ctr.GetEnv())), false)
	management, claimed, steers := ManagementSteered, cpuset.New(), false
	switch {
	case err != nil:
		klog.Warningf("NRI Synchronize: ctr=%s: %v; taken as steered", name, err)
	case d.management == ManagementSteered:
		management, claimed, steers = unsteered(d.eff), d.claimed, true
	default:
		management, claimed = d.management, d.claimed
	}
	c := p.track(pod, ctr, LifecycleUnseen, management, TriggerSynchronize)
	c.Claimed = claimed
	if err == nil {
		c.Steering = d.eff.PolicyText()
	}
	if running {
		p.startTask(c, ctr, LifecycleStarted, TriggerSynchronize)
	}
	p.releaseSteering(c, steers)
	c.OwnClaimed = ownClaimed(c)
	if lent := p.seedLends(c); !lent.IsEmpty() {
		// not its own nodes, as in baseMems
		c.Base = c.Base.Difference(lent)
		klog.V(3).Infof("NRI Synchronize: ctr=%s id=%s: nodes %s lent by its leases", c.name(), c.ID, lent)
	}
	return c
}

// CreateContainer decides the management of the memory of a container
// from the container settings of the allocation results that it uses and
// the defaults: a steered container gets the steering plan of the memory of
// its results, kept until StartContainer; a fenced container gets
// cpuset.mems without the fenced nodes that its results have no numa
// memory on, if that changes its cpuset.mems. A container that is not
// steered is lent the nodes of its READY numa leases (kept by the broker
// when it stopped to restart): a fenced one gets them in the adjustment, as
// an update at its start would reach only its spec. Returns an error if
// the container settings of the results conflict or are invalid, or no
// plan can be built, which blocks the creation.
func (p *Plugin) CreateContainer(_ context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	name, id := ctrName(pod, ctr), ctr.GetId()
	klog.V(3).Infof("NRI CreateContainer: ctr=%s id=%s", name, id)

	// the results are read before the table mutex is taken
	results := p.results(pod, name, extractResults(ctr.GetEnv()))
	d, err := p.decide(name, results, true)
	if err != nil {
		klog.Errorf("NRI CreateContainer: %v", err)
		p.mu.Lock()
		p.track(pod, ctr, LifecycleCreated, p.defaultManagement(), TriggerCreate)
		p.mu.Unlock()
		return nil, nil, err
	}
	logDecision(name, d, len(results) > 0)

	p.mu.Lock()
	c := p.track(pod, ctr, LifecycleCreated, d.management, TriggerCreate)
	c.Claimed, c.Steering = d.claimed, d.eff.PolicyText()
	c.OwnClaimed = ownClaimed(c)
	if d.plan != nil {
		c.Plan, c.unsteered = d.plan, unsteered(d.eff)
	}
	if lent := p.seedLends(c); !lent.IsEmpty() {
		klog.V(3).Infof("NRI CreateContainer: ctr=%s: nodes %s lent by its leases (kept for its restart)", name, lent)
	}
	mems, fenced := p.fenceCreated(c)
	p.mu.Unlock()

	if mems == "" {
		return nil, nil, nil
	}
	klog.V(3).Infof("NRI CreateContainer: ctr=%s: cpuset.mems=%s (fenced NUMA nodes %s)", name, mems, fenced)
	adjust := &api.ContainerAdjustment{}
	adjust.SetLinuxCPUSetMems(mems)
	return adjust, nil, nil
}

// StartContainer records the pid, the cgroup and the own nodes of a
// container whose task the runtime has created, and starts steering its
// memory along the plan of its CreateContainer: its Steered record is
// committed before the manager writes its cgroup. A container whose
// steering does not start is managed as one that is not steered. The
// OnContainerStarted callbacks and the repair pass of the container wait
// for PostStartContainer: an update sent now would reach only its spec.
func (p *Plugin) StartContainer(_ context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	name, id := ctrName(pod, ctr), ctr.GetId()
	p.mu.Lock()
	c := p.ctrs[id]
	if c == nil {
		c = p.track(pod, ctr, LifecycleNone, p.defaultManagement(), TriggerStart)
	}
	plan := c.Plan
	c.Plan = nil
	p.startTask(c, ctr, LifecycleTaskCreated, TriggerStart)
	klog.V(4).Infof("NRI StartContainer: ctr=%s id=%s pid=%d cgroup=%s baseMems=%s", name, id, c.Pid, c.CgroupDir, c.Base)
	p.mu.Unlock()

	if plan == nil {
		klog.V(3).Infof("NRI StartContainer: ctr=%s id=%s: no memory claims to be managed", name, id)
		return nil
	}
	p.startSteering(c, ctr, plan)
	return nil
}

// PostStartContainer runs the OnContainerStarted callbacks of a container
// that the runtime has started and marked started (from now on it applies
// updates of the container to its cgroup): the broker lends the nodes of
// the leases of a restarted requester again, or finds them lent at its
// creation. For a fenced container it starts the repair pass of the one
// container in the background, which updates its cgroup if an update
// before its start reached only its spec.
func (p *Plugin) PostStartContainer(_ context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.ctrs[ctr.GetId()]
	if c == nil {
		// stopped already
		return nil
	}
	if c.started() {
		p.lifecycle(c, LifecycleStarted, TriggerPostStart)
	} else {
		p.startTask(c, ctr, LifecycleStarted, TriggerPostStart)
	}
	klog.V(4).Infof("NRI PostStartContainer: ctr=%s id=%s", c.name(), c.ID)
	p.fireStarted(c)
	if p.fenceAtStart(c) {
		p.repair(c.ID)
	}
	return nil
}

// StopContainer removes a container that the runtime stops from the table
// and stops the steering of its memory.
func (p *Plugin) StopContainer(_ context.Context, pod *api.PodSandbox, ctr *api.Container) ([]*api.ContainerUpdate, error) {
	klog.V(3).Infof("NRI StopContainer: ctr=%s", ctrName(pod, ctr))
	p.gone(ctr.GetId())
	return nil, nil
}

// RemoveContainer removes a container that the runtime removes from the
// table and stops the steering of its memory.
func (p *Plugin) RemoveContainer(_ context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	name := ctrName(pod, ctr)
	p.gone(ctr.GetId())
	if refs := extractResults(ctr.GetEnv()); len(refs) == 0 {
		klog.V(5).Infof("NRI RemoveContainer: ctr=%s: no CXL claims", name)
	} else {
		klog.V(5).Infof("NRI RemoveContainer: ctr=%s releasing results: %v", name, refs)
	}
	return nil
}

// gone removes a container from the table, fires OnContainerGone once and
// stops its manager.
func (p *Plugin) gone(id string) {
	p.mu.Lock()
	c := p.ctrs[id]
	if c != nil {
		c = p.untrack(c, TriggerStop)
	}
	p.mu.Unlock()
	if c != nil {
		p.stopManagers([]*Container{c})
	}
}

// track returns the record of a container, a new one in the lifecycle
// state with the management if the table has none. Called with mu held.
func (p *Plugin) track(pod *api.PodSandbox, ctr *api.Container, lifecycle Lifecycle, management Management, trigger Trigger) *Container {
	if c, ok := p.ctrs[ctr.GetId()]; ok {
		return c
	}
	p.seq++
	c := &Container{
		ID:           ctr.GetId(),
		PodUID:       pod.GetUid(),
		PodNamespace: pod.GetNamespace(),
		PodName:      pod.GetName(),
		Name:         ctr.GetName(),
		Lifecycle:    LifecycleNone,
		Management:   ManagementUndecided,
		SpecMems:     specMems(ctr),
		Base:         cpuset.New(),
		Claimed:      cpuset.New(),
		OwnClaimed:   cpuset.New(),
		seq:          p.seq,
	}
	p.manage(c, management, trigger)
	if lifecycle != LifecycleNone {
		p.lifecycle(c, lifecycle, trigger)
	}
	p.ctrs[c.ID] = c
	return c
}

// startTask records the pid and the cgroup of a container whose task
// exists, takes it to the lifecycle state on trigger, reads its own nodes,
// and at its first start records the claimed nodes that its cgroup has as
// its own. Called with mu held.
func (p *Plugin) startTask(c *Container, ctr *api.Container, to Lifecycle, trigger Trigger) {
	first := !c.started()
	c.Pid = ctr.GetPid()
	if c.CgroupDir == "" {
		if dir, err := resolveContainerCgroupDir(ctr); err == nil {
			c.CgroupDir = dir
		} else {
			klog.V(3).Infof("NRI lend: ctr=%s id=%s: no cgroup directory: %v", c.name(), c.ID, err)
		}
	}
	p.lifecycle(c, to, trigger)
	c.Base = p.baseMems(c, ctr)
	if first {
		c.OwnClaimed = ownClaimed(c)
	}
}

// untrack removes a record from the table, fires OnContainerGone and
// returns the record. Called with mu held.
func (p *Plugin) untrack(c *Container, trigger Trigger) *Container {
	p.lifecycle(c, LifecycleGone, trigger)
	delete(p.ctrs, c.ID)
	p.fireGone(c)
	return c
}

// lifecycle applies a transition of the lifecycle of a container; one that
// its table refuses is logged and leaves it as it is.
func (p *Plugin) lifecycle(c *Container, to Lifecycle, trigger Trigger) {
	if err := fsm.Apply(&LifecycleTable, &c.Lifecycle, to, trigger); err != nil {
		klog.Warningf("NRI: ctr=%s id=%s: %v", c.name(), c.ID, err)
	}
}

// manage applies a transition of the management of a container; one that
// its table refuses is logged and leaves it as it is.
func (p *Plugin) manage(c *Container, to Management, trigger Trigger) {
	if err := fsm.Apply(&ManagementTable, &c.Management, to, trigger); err != nil {
		klog.Warningf("NRI: ctr=%s id=%s: %v", c.name(), c.ID, err)
	}
}

// seedLends gives a container that has no lends the nodes of the READY numa
// leases of its pod and name in the store's snapshot, unless it is steered
// (lending to it is refused), and returns the lent nodes. Called with mu
// held.
func (p *Plugin) seedLends(c *Container) cpuset.CPUSet {
	if !p.opts.Leases || c.PodUID == "" || len(c.Lends) != 0 || c.steered() {
		return cpuset.New()
	}
	var kept lentNUMANodes
	for node, ids := range state.LeaseLends(&p.opts.Store.Snapshot().Records, c.PodUID, c.Name) {
		for _, id := range ids {
			if node >= 0 {
				kept = kept.set(node, kept.with(node, id, true))
			}
		}
	}
	c.Lends = kept
	return kept.nodes()
}

// specMems returns the cpuset.mems of the spec of a container.
func specMems(ctr *api.Container) string {
	return ctr.GetLinux().GetResources().GetCpu().GetMems()
}
