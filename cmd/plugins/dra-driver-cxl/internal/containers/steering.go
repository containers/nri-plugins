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
	"slices"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/steering"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/config"
)

// The steering of the memory of containers: a steering manager of the
// plugin writes the cpuset.mems, the thread memory policies and
// memory.high of a steered container directly, never through NRI. Its
// cpuset.mems has only nodes that fencing with allowed-mems would give the
// container (steerNodes). A container whose memory.high is not max at its
// start is not steered: the manager never replaces a bound of others. The
// Steered records of the store name the containers whose cgroup a manager
// writes: a record is committed before the manager writes the cgroup, and
// removed when the manager has stopped. So the records of a process that
// was killed, or that exited on the loss of NRI, name the containers whose
// memory.high its managers left, and the next process releases exactly
// those, whatever their settings say now (the configuration may have
// changed across the restart), and leaves the memory.high of the others
// alone.

// steeredCommitWait bounds the wait of a commit of the Steered records in
// the queue of the store. The commits run inside NRI handlers, so the
// bound is below the default timeout of an NRI request.
const steeredCommitWait = api.DefaultPluginRequestTimeout / 2

// startManager starts a steering manager: its first step writes the
// cgroup (a variable for tests).
var startManager = (*steering.Manager).Start

// policies are the steering policies of the settings, all but none and
// auto.
var policies = map[string]steering.Policy{
	config.PolicyFirstDRAM:        steering.PolicyFirstDRAM,
	config.PolicyFirstCXL:         steering.PolicyFirstCXL,
	config.PolicyStartInterleaved: steering.PolicyStartInterleaved,
	config.PolicyEndInterleaved:   steering.PolicyEndInterleaved,
	config.PolicyWaypoints:        steering.PolicyWaypoints,
}

// steeringPolicy returns the policy of the steering settings for the
// memory types, and false when the policy is none or there are neither
// DRAM nor CXL nodes. Policy auto is first-cxl for CXL nodes only,
// first-dram for DRAM nodes only, and start-interleaved for both.
func steeringPolicy(s *config.Steering, mem steering.MemoryTypes) (steering.Policy, bool) {
	if s == nil || (len(mem.DRAMNUMANodes) == 0 && len(mem.CXLNUMANodes) == 0) {
		return 0, false
	}
	if s.Policy == config.PolicyAuto {
		switch {
		case len(mem.CXLNUMANodes) == 0:
			return steering.PolicyFirstDRAM, true
		case len(mem.DRAMNUMANodes) == 0:
			return steering.PolicyFirstCXL, true
		}
		return steering.PolicyStartInterleaved, true
	}
	policy, ok := policies[s.Policy]
	return policy, ok
}

// steeringPlan returns the plan of the policy with the waypoints and steps
// of the validated steering settings for the memory types.
func steeringPlan(s *config.Steering, policy steering.Policy, mem steering.MemoryTypes) (*steering.Plan, error) {
	minStep, err := quantityBytes(s.MinStep)
	if err != nil {
		return nil, err
	}
	maxStep, err := quantityBytes(s.MaxStep)
	if err != nil {
		return nil, err
	}
	var waypoints []steering.TypeUsage
	var usage steering.TypeUsage
	for _, wp := range s.Waypoints {
		// a memory type that the waypoint does not list keeps its usage
		for _, tu := range wp.TargetUsages {
			bytes, err := quantityBytes(tu.Usage)
			if err != nil {
				return nil, err
			}
			switch tu.MemoryType {
			case config.MemoryTypeDRAM:
				usage.DRAM = bytes
			case config.MemoryTypeCXL:
				usage.CXL = bytes
			}
		}
		waypoints = append(waypoints, usage)
	}
	return steering.NewPlan(policy, mem, waypoints, minStep, maxStep)
}

// quantityBytes returns the bytes of a quantity, 0 for "".
func quantityBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	return q.Value(), nil
}

// startSteering starts steering the memory of a container whose task the
// runtime has created along its plan, recorded in the store before the
// manager writes its cgroup. A container whose steering does not start is
// managed as a container that is not steered (notSteered); one whose
// steering ends while its manager starts, as steeringEnded says.
func (p *Plugin) startSteering(c *Container, ctr *api.Container, plan *steering.Plan) {
	name := c.name()
	dir, err := resolveContainerCgroupDir(ctr)
	if err != nil {
		p.notSteered(c, false, fmt.Errorf("failed to resolve cgroup directory: %w", err))
		return
	}
	klog.V(3).Infof("NRI StartContainer: ctr=%s id=%s pid=%d cgroupsPath=%s", name, c.ID, ctr.GetPid(), dir)
	p.mu.Lock()
	permitted, err := p.steerNodes(c, plan)
	p.mu.Unlock()
	if err != nil {
		p.notSteered(c, false, err)
		return
	}
	var mgr *steering.Manager
	mgr, err = steering.New(dir, plan, permitted,
		steering.WithName(name),
		steering.WithOnEnd(func(why error) { p.steeringEnded(c, mgr, why) }),
		steering.WithSysRoot(filepath.Join(p.opts.SysRoot, "/sys")))
	if err != nil {
		p.notSteered(c, false, err)
		return
	}
	if err := p.addSteered(c.ID, dir); err != nil {
		p.notSteered(c, false, fmt.Errorf("failed to record memory steering: %w", err))
		return
	}
	p.mu.Lock()
	c.starting = mgr
	p.mu.Unlock()
	err = startManager(mgr)
	p.mu.Lock()
	ended := c.startEnd
	c.starting, c.startEnd = nil, nil
	if err != nil {
		p.mu.Unlock()
		p.notSteered(c, true, err)
		return
	}
	if p.ctrs[c.ID] != c {
		p.mu.Unlock()
		p.stopManager(c.ID, name, mgr)
		return
	}
	c.manager = mgr
	p.mu.Unlock()
	klog.V(3).Infof("NRI StartContainer: ctr=%s: started memory steering (cgroup=%s)", name, dir)
	if ended != nil {
		p.steeringEnded(c, mgr, ended)
	}
}

// steerNodes returns the nodes that the cpuset.mems of a steered
// container may have: the nodes that fencing with allowed-mems would give
// it (its own nodes without the fenced nodes, with its claimed nodes) and
// the nodes of its plan. Called with mu held.
func (p *Plugin) steerNodes(c *Container, plan *steering.Plan) (cpuset.CPUSet, error) {
	own, err := ownMems(c, p.memNodes())
	if err != nil {
		return cpuset.New(), err
	}
	return own.Difference(p.fenced).Union(c.Claimed, plan.Nodes()), nil
}

// notSteered manages a started container whose steering did not start
// like a container that is not steered (fenced with allowed-mems): no
// manager of this process steers it, so it is fenced and may be lent to.
// Its repair pass runs at its PostStartContainer. A failed Start of a
// manager (added: its Steered record was committed) has left the cgroup as
// it was, so the container leaves the Steered records.
//   - why: the reason, in the warning
func (p *Plugin) notSteered(c *Container, added bool, why error) {
	if added {
		p.removeSteered(c.ID)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctrs[c.ID] != c || c.manager != nil {
		klog.V(3).Infof("NRI StartContainer: ctr=%s id=%s: memory steering not started: %v", c.name(), c.ID, why)
		return
	}
	p.manage(c, c.unsteered, TriggerStart)
	klog.Warningf("NRI StartContainer: ctr=%s id=%s: failed to start memory steering: %v; memory %s",
		c.name(), c.ID, why, c.Management.text())
}

// steeringEnded manages a running container whose steering ended on a
// refused memory policy like a container that is not steered: its manager
// mgr has set memory.high to max. A fenced container gets wantMems by the
// repair pass; one with fencing none gets back its cpuset.mems of before
// the steering. A container that stopped meanwhile is left to its stop.
// An end that comes while the manager starts is left to startSteering.
func (p *Plugin) steeringEnded(c *Container, mgr *steering.Manager, why error) {
	p.mu.Lock()
	if c.starting == mgr {
		c.startEnd = why
		p.mu.Unlock()
		return
	}
	if p.ctrs[c.ID] != c || c.manager != mgr {
		p.mu.Unlock()
		return
	}
	c.manager = nil
	p.manage(c, c.unsteered, TriggerSteeringEnded)
	repair := c.Lifecycle == LifecycleStarted && p.fenceAtStart(c)
	memory := c.Management.text()
	p.mu.Unlock()
	p.stopManager(c.ID, c.name(), mgr)
	klog.Warningf("NRI: ctr=%s id=%s: memory steering ended: %v; memory %s", c.name(), c.ID, why, memory)
	switch {
	case repair:
		p.repair(c.ID)
	case memory == ManagementUnfenced.text():
		mgr.RestoreMems()
	}
}

// stopManagers stops the managers of containers that left the table and
// removes their Steered records.
func (p *Plugin) stopManagers(gone []*Container) {
	for _, c := range gone {
		p.mu.Lock()
		mgr := c.manager
		c.manager, c.Plan = nil, nil
		p.mu.Unlock()
		if mgr != nil {
			p.stopManager(c.ID, c.name(), mgr)
		}
	}
}

// stopManager stops the manager of the container id, which sets its
// memory.high to max, and then removes its Steered record.
func (p *Plugin) stopManager(id, name string, mgr *steering.Manager) {
	mgr.Stop()
	p.removeSteered(id)
	klog.V(3).Infof("NRI: stopped memory steering for ctr=%s", name)
}

// releaseSteering sets memory.high of a container to max, as the Stop of
// a manager does, if a Steered record says that a manager of a previous
// process steered it, whatever its settings say now (steers: they steer
// it): the manager of a killed plugin left the memory.high of its last
// step, which nothing raises any more. The memory.high of a container that
// no manager steered (created while the plugin was down, or one that is not
// steered) is left alone: it may be someone else's (MemoryQoS). Its
// cpuset.mems and memory policies are left as they are. Called with mu
// held.
func (p *Plugin) releaseSteering(c *Container, steers bool) {
	dir, recorded := p.steeredDir(c.ID)
	switch {
	case !recorded:
		if steers {
			klog.V(3).Infof("NRI Synchronize: ctr=%s id=%s: steering not restarted, memory %s; memory.high left alone: no manager steered it",
				c.name(), c.ID, c.Management.text())
		}
		return
	case dir == "":
		dir = c.CgroupDir
	}
	release(c, dir)
}

// release sets memory.high of the cgroup dir of a container to max.
func release(c *Container, dir string) {
	memory := c.Management.text()
	if dir == "" {
		klog.Warningf("NRI Synchronize: ctr=%s id=%s: steering not restarted, memory %s; memory.high not checked: no cgroup directory",
			c.name(), c.ID, memory)
		return
	}
	was, err := steering.Release(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		klog.V(3).Infof("NRI Synchronize: ctr=%s id=%s: steering not restarted, memory %s; no memory.high: %v",
			c.name(), c.ID, memory, err)
	case err != nil:
		klog.Warningf("NRI Synchronize: ctr=%s id=%s: steering not restarted, memory %s; removing memory.high failed: %v",
			c.name(), c.ID, memory, err)
	case was != "":
		klog.Infof("NRI Synchronize: ctr=%s id=%s: steering not restarted, memory %s; memory.high %s removed (max)",
			c.name(), c.ID, memory, was)
	default:
		klog.V(3).Infof("NRI Synchronize: ctr=%s id=%s: steering not restarted, memory %s; memory.high max",
			c.name(), c.ID, memory)
	}
}

// steeredDir returns the cgroup directory of the Steered record of the
// container id, and whether the store has one.
func (p *Plugin) steeredDir(id string) (string, bool) {
	for _, s := range p.opts.Store.Snapshot().Steered {
		if s.Container == id {
			return s.CgroupDir, true
		}
	}
	return "", false
}

// addSteered commits the Steered record of the container id whose cgroup
// dir a manager writes.
func (p *Plugin) addSteered(id, dir string) error {
	return p.commitSteered(func(records []state.Steered) []state.Steered {
		records = slices.DeleteFunc(records, func(s state.Steered) bool { return s.Container == id })
		return append(records, state.Steered{Container: id, CgroupDir: dir})
	})
}

// removeSteered commits the removal of the Steered records of the
// containers ids; the store writes nothing when it has none of them. A
// failed commit is logged.
func (p *Plugin) removeSteered(ids ...string) {
	err := p.commitSteered(func(records []state.Steered) []state.Steered {
		return slices.DeleteFunc(records, func(s state.Steered) bool { return slices.Contains(ids, s.Container) })
	})
	if err != nil {
		klog.Warningf("NRI: steering record: %v", err)
	}
}

// syncSteered commits the Steered records of the containers keep only (the
// managers of this process): from now on the records are the plugin's. A
// failed commit is logged.
func (p *Plugin) syncSteered(keep []string) {
	err := p.commitSteered(func(records []state.Steered) []state.Steered {
		return slices.DeleteFunc(records, func(s state.Steered) bool { return !slices.Contains(keep, s.Container) })
	})
	if err != nil {
		klog.Warningf("NRI: steering record: %v", err)
	}
}

// commitSteered commits the change of the Steered records durably; the
// commit waits at most steeredCommitWait in the queue of the store.
func (p *Plugin) commitSteered(change func([]state.Steered) []state.Steered) error {
	ctx, cancel := context.WithTimeout(context.Background(), steeredCommitWait)
	defer cancel()
	return p.opts.Store.Commit(ctx, store.Durable, func(doc *store.Document) error {
		doc.Steered = change(slices.Clone(doc.Steered))
		return nil
	})
}
