// Copyright 2021 Intel Corporation. All Rights Reserved.
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

package resmgr

import (
	"context"
	"fmt"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/pkg/errors"
	"sigs.k8s.io/yaml"

	"github.com/containerd/nri/pkg/api"
	stub "github.com/containerd/nri/pkg/stub"
)

type nriPlugin struct {
	logger.Logger
	stub   stub.Stub
	resmgr *resmgr
}

func newNRIPlugin(resmgr *resmgr) (*nriPlugin, error) {
	p := &nriPlugin{
		Logger: logger.NewLogger("nri-plugin"),
		resmgr: resmgr,
	}

	p.Info("creating plugin...")

	return p, nil
}

func (p *nriPlugin) createStub() error {
	var (
		opts = []stub.Option{
			stub.WithPluginName(opt.NriPluginName),
			stub.WithPluginIdx(opt.NriPluginIdx),
			stub.WithSocketPath(opt.NriSocket),
			stub.WithOnClose(p.onClose),
		}
		err error
	)

	p.Info("creating plugin stub...")

	if p.stub, err = stub.New(p, opts...); err != nil {
		return errors.Wrap(err, "failed to create NRI plugin stub")
	}

	return nil
}

func (p *nriPlugin) start() error {
	if p == nil {
		return nil
	}

	p.Info("starting plugin...")

	if err := p.createStub(); err != nil {
		return err
	}

	if err := p.stub.Start(context.Background()); err != nil {
		return errors.Wrap(err, "failed to start NRI plugin")
	}

	return nil
}

func (p *nriPlugin) stop() {
	if p == nil {
		return
	}

	p.Info("stopping plugin...")
	p.stub.Stop()
}

func (p *nriPlugin) restart() error {
	return p.start()
}

func (p *nriPlugin) onClose() {
	p.resmgr.Warn("connection to NRI/runtime lost, trying to reconnect...")
	p.restart()
}

func (p *nriPlugin) Configure(cfg, runtime, version string) (stub.EventMask, error) {
	event := Configure
	p.dump(in, event, runtime, version)

	return api.MustParseEventMask(
		"RunPodSandbox,StopPodSandbox,RemovePodSandbox",
		"CreateContainer,StartContainer,UpdateContainer,StopContainer,RemoveContainer",
	), nil
}

func (p *nriPlugin) syncWithNRI(pods []*api.PodSandbox, containers []*api.Container) ([]cache.Container, []cache.Container, error) {
	m := p.resmgr

	m.Info("synchronizing cache state with NRI/CRI runtime...")

	add, del := []cache.Container{}, []cache.Container{}

	_, _, deleted := m.cache.RefreshPods(pods)
	for _, c := range deleted {
		m.Info("discovered stale container %s...", c.GetID())
		del = append(del, c)
	}

	added, deleted := m.cache.RefreshContainers(containers)
	for _, c := range added {
		if c.GetState() != cache.ContainerStateRunning {
			m.Info("ignoring discovered container %s (in state %v)...",
				c.GetID(), c.GetState())
			continue
		}
		m.Info("discovered out-of-sync running container %s...", c.GetID())
		add = append(add, c)
	}
	for _, c := range deleted {
		m.Info("discovered stale container %s...", c.GetID())
		del = append(del, c)
	}

	return add, del, nil
}

func (p *nriPlugin) Synchronize(pods []*api.PodSandbox, containers []*api.Container) (updates []*api.ContainerUpdate, retErr error) {
	event := Synchronize
	p.dump(in, event, pods, containers)
	defer func() {
		p.dump(out, event, updates, retErr)
	}()

	m := p.resmgr

	add, del, err := p.syncWithNRI(pods, containers)
	if err != nil {
		p.resmgr.Error("failed to synchronize with NRI: %v", err)
		return nil, err
	}

	if err := m.policy.Start(add, del); err != nil {
		return nil, errors.Wrapf(err,
			"failed to start policy %s", policy.ActivePolicy())
	}

	m.updateTopologyZones()

	return p.getPendingUpdates(nil), nil
}

func (p *nriPlugin) RunPodSandbox(pod *api.PodSandbox) (retErr error) {
	event := RunPodSandbox
	p.dump(in, event, pod)
	defer func() {
		p.dump(out, event, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	m.cache.InsertPod(pod)
	return nil
}

func (p *nriPlugin) StopPodSandbox(pod *api.PodSandbox) (retErr error) {
	event := StopPodSandbox
	p.dump(in, event, pod)
	defer func() {
		p.dump(out, event, retErr)
	}()

	return nil
}

func (p *nriPlugin) RemovePodSandbox(pod *api.PodSandbox) (retErr error) {
	event := RemovePodSandbox
	p.dump(in, event, pod)
	defer func() {
		p.dump(out, event, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	m.cache.DeletePod(pod.Id)
	return nil
}

func (p *nriPlugin) CreateContainer(pod *api.PodSandbox, container *api.Container) (adjust *api.ContainerAdjustment, updates []*api.ContainerUpdate, retErr error) {
	event := CreateContainer
	p.dump(in, event, pod, container)
	defer func() {
		p.dump(out, event, adjust, updates, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	c, err := m.cache.InsertContainer(container)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to cache container")
	}
	c.UpdateState(cache.ContainerStateCreating)

	if err := m.policy.AllocateResources(c); err != nil {
		c.UpdateState(cache.ContainerStateStale)
		return nil, nil, errors.Wrap(err, "failed to allocate resources")
	}
	c.UpdateState(cache.ContainerStateCreated)

	c.InsertMount(&cache.Mount{
		Destination: "/.nri-resource-policy",
		Source:      m.cache.ContainerDirectory(c.GetID()),
		Type:        "bind",
		Options:     []string{"bind", "ro", "rslave"},
	})
	m.policy.ExportResourceData(c)
	m.updateTopologyZones()

	adjust = p.getPendingAdjustment(container)
	updates = p.getPendingUpdates(container)

	return adjust, updates, nil
}

func (p *nriPlugin) StartContainer(pod *api.PodSandbox, container *api.Container) (retErr error) {
	event := StartContainer
	p.dump(in, event, pod, container)
	defer func() {
		p.dump(out, event, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	c, ok := m.cache.LookupContainer(container.Id)
	if !ok {
		return nil
	}

	c.UpdateState(cache.ContainerStateRunning)

	e := &events.Policy{
		Type:   events.ContainerStarted,
		Source: "resource-manager",
		Data:   c,
	}

	if _, err := m.policy.HandleEvent(e); err != nil {
		m.Error("%s: policy failed to handle event %s: %v", event, e.Type, err)
	}

	return nil
}

func (p *nriPlugin) UpdateContainer(pod *api.PodSandbox, container *api.Container) (updates []*api.ContainerUpdate, retErr error) {
	event := UpdateContainer
	p.dump(in, event, pod, container)
	defer func() {
		p.dump(out, event, updates, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	// XXX TODO(klihub): hook in policy processing

	return nil, nil
}

func (p *nriPlugin) StopContainer(pod *api.PodSandbox, container *api.Container) (updates []*api.ContainerUpdate, retErr error) {
	event := StopContainer
	p.dump(in, event, pod, container)
	defer func() {
		p.dump(out, event, updates, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	c, ok := m.cache.LookupContainer(container.Id)
	if !ok {
		return nil, nil
	}

	if err := m.policy.ReleaseResources(c); err != nil {
		return nil, errors.Wrap(err, "failed to release resources")
	}

	c.UpdateState(cache.ContainerStateExited)
	m.updateTopologyZones()

	return p.getPendingUpdates(container), nil
}

func (p *nriPlugin) RemoveContainer(pod *api.PodSandbox, container *api.Container) (retErr error) {
	event := RemoveContainer
	p.dump(in, event, pod, container)
	defer func() {
		p.dump(out, event, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	m.cache.DeleteContainer(container.Id)
	return nil
}

func (p *nriPlugin) updateContainers() (retErr error) {
	// Notes: must be called with p.resmgr lock held.

	updates := p.getPendingUpdates(nil)

	event := UpdateContainers
	p.dump(out, event, updates)
	defer func() {
		p.dump(in, event, retErr)
	}()

	_, err := p.stub.UpdateContainers(updates)
	if err != nil {
		return fmt.Errorf("post-config container update failed: %w", err)
	}

	return nil
}

func (p *nriPlugin) getPendingAdjustment(container *api.Container) *api.ContainerAdjustment {
	if c, ok := p.resmgr.cache.LookupContainer(container.GetId()); ok {
		adjust := c.GetPendingAdjustment()
		for _, ctrl := range c.GetPending() {
			c.ClearPending(ctrl)
		}
		return adjust
	}

	return nil
}

func (p *nriPlugin) getPendingUpdates(skip *api.Container) []*api.ContainerUpdate {
	m := p.resmgr
	updates := []*api.ContainerUpdate{}
	for _, c := range m.cache.GetPendingContainers() {
		if skip != nil && skip.GetId() == c.GetID() {
			continue
		}

		if u := c.GetPendingUpdate(); u != nil {
			if bioc := c.GetBlockIOClass(); bioc != "" {
				u.SetLinuxBlockIOClass(bioc)
			}
			if rdtc := c.GetRDTClass(); rdtc != "" {
				u.SetLinuxRDTClass(rdtc)
			}
			updates = append(updates, u)

			for _, ctrl := range c.GetPending() {
				c.ClearPending(ctrl)
			}
		}
	}

	return updates
}

const (
	in  = "=>"
	out = "<="
)

const (
	Configure        = "Configure"
	Synchronize      = "Synchronize"
	RunPodSandbox    = "RunPodSandbox"
	StopPodSandbox   = "StopPodSandbox"
	RemovePodSandbox = "RemovePodSandbox"
	CreateContainer  = "CreateContainer"
	StartContainer   = "StartContainer"
	UpdateContainer  = "UpdateContainer"
	StopContainer    = "StopContainer"
	RemoveContainer  = "RemoveContainer"
	UpdateContainers = "UpdateContainers"
)

func (p *nriPlugin) dump(dir, event string, args ...interface{}) {
	switch event {
	case RunPodSandbox, StopPodSandbox, RemovePodSandbox:
		if dir == in {
			if len(args) != 1 {
				p.Error("%s %s <dump error, %d args, expected (pod)>", dir, event, len(args))
				return
			}

			pod, ok := args[0].(*api.PodSandbox)
			if !ok {
				p.Error("%s %s <dump error, arg %T, expected (pod)>", dir, event, args[0])
				return
			}

			p.Info("%s %s %s/%s", dir, event, pod.GetNamespace(), pod.GetName())
			p.dumpDetails(dir, event, pod)
		} else {
			if len(args) != 1 {
				p.Error("%s %s <dump error, %d args, expected (err/nil)>", dir, event, len(args))
				return
			}

			err := args[0]
			if err != nil {
				p.Error("%s %s FAILED: %v", dir, event, err.(error))
				return
			}

			p.Info("%s %s", dir, event)
		}

	case CreateContainer, StartContainer, UpdateContainer, StopContainer, RemoveContainer:
		if dir == in {
			if len(args) != 2 {
				p.Error("%s %s <dump error, %d args, expected (pod, container)>",
					dir, event, len(args))
				return
			}

			pod, ok := args[0].(*api.PodSandbox)
			if !ok {
				p.Error("%s %s <dump error, args %T, %T, expected (pod, container)>",
					dir, event, args[0], args[1])
				return
			}
			ctr, ok := args[1].(*api.Container)
			if !ok {
				p.Error("%s %s <dump error, args %T, %T, expected (pod, container)>",
					dir, event, args[0], args[1])
				return
			}

			p.Info("%s %s %s/%s:%s", dir, event, pod.GetNamespace(), pod.GetName(), ctr.GetName())
			p.dumpDetails(dir, event, ctr)
		} else {
			if len(args) < 1 {
				p.Error("%s %s <dump error, missing args>", dir, event)
				return
			}

			err := args[len(args)-1]
			if err != nil {
				p.Error("%s %s FAILED: %v", dir, event, err.(error))
				return
			}

			p.Info("%s %s", dir, event)

			switch event {
			case CreateContainer:
				p.dumpDetails(dir, event, args[0])
				p.dumpDetails(dir, event, args[1])
			case StopContainer, UpdateContainer:
				p.dumpDetails(dir, event, args[0])
			}
		}

	case UpdateContainers: // post-config outgoing UpdateContainers
		if dir == out {
			if len(args) != 1 {
				p.Error("%s %s <dump error, %d args, expected (update)>", dir, event, len(args))
				return
			}

			p.Info("%s %s", dir, event)
			p.dumpDetails(dir, event, args[0])
		} else {
			if len(args) != 1 {
				p.Error("%s %s <dump error, %d args, expected (err/nil)>", dir, event, len(args))
				return
			}

			err := args[0]
			if err == nil {
				p.Info("%s %s", dir, event)
				return
			}

			p.Error("%s %s FAILED: %v", dir, event, err.(error))
		}

	case Configure:
		if dir == in {
			if len(args) != 2 {
				p.Error("%s %s <dump error, %d args, expected (runtime, version)>",
					dir, event, len(args))
				return
			}

			runtime, ok := args[0].(string)
			if !ok {
				p.Error("%s %s <dump error, args %T, %T, expected (runtime, version)>",
					dir, event, args[0], args[1])
				return
			}
			version, ok := args[1].(string)
			if !ok {
				p.Error("%s %s <dump error, args %T, %T, expected (runtime, version)>",
					dir, event, args[0], args[1])
				return
			}

			p.Info("%s %s, runtime %s %s", dir, event, runtime, version)
		} else {
			p.Info("%s %s", dir, event)
		}

	case Synchronize:
		if dir == in {
			if len(args) != 2 {
				p.Error("%s %s <dump error, %d args, expected (pods, containers)",
					dir, event, len(args))
			}

			p.Info("%s %s", dir, event)
			p.dumpDetails(dir, event, args[0])
			p.dumpDetails(dir, event, args[1])
		} else {
			if len(args) != 2 {
				p.Error("%s %s <dump error, %d args, expected (updates, err/nil)",
					dir, event, len(args))
				return
			}

			err := args[1]
			if err != nil {
				p.Error("%s %s FAILED: %v", dir, event, err.(error))
				return
			}

			p.Info("%s %s", dir, event)
			p.dumpDetails(dir, event, args[0])
			p.dumpDetails(dir, event, args[0])
		}

	default:
		p.Info("%s %s", dir, event)
	}
}

func (p *nriPlugin) dumpDetails(dir, event string, arg interface{}) {
	// if debug is off for our debug source, we don't dump any details
	if !p.DebugEnabled() {
		return
	}

	if dir == in {
		switch event {
		case RunPodSandbox, CreateContainer, Synchronize:
		default:
			// we only dump details for the requests listed above
			return
		}
	} else {
		switch event {
		case CreateContainer, UpdateContainer, StopContainer, Synchronize, UpdateContainers:
		default:
			// we only dump details for the responses listed above
			return
		}
	}

	switch obj := arg.(type) {
	case *api.PodSandbox:
		data := marshal("pod", obj)
		p.DebugBlock(dir+"   <pod> ", "%s", data)

	case *api.Container:
		data := marshal("container", obj)
		p.DebugBlock(dir+"   <ctr> ", "%s", data)

	case *api.ContainerAdjustment:
		data := marshal("adjustment", obj)
		p.DebugBlock(dir+"   <adjustment> ", "%s", data)

	case []*api.ContainerUpdate:
		for idx, update := range obj {
			data := marshal("update", update)
			p.DebugBlock(dir+fmt.Sprintf("   <update #%d> ", idx), "%s", data)
		}

	case []*api.PodSandbox:
		for idx, pod := range obj {
			data := marshal("pod", pod)
			p.DebugBlock(dir+fmt.Sprintf("   <pod #%d> ", idx), "%s", data)
		}

	case []*api.Container:
		for idx, ctr := range obj {
			data := marshal("container", ctr)
			p.DebugBlock(dir+fmt.Sprintf("   <ctr #%d> ", idx), "%s", data)
		}
	default:
		p.DebugBlock(dir+"   <unknown data of type> ", "%s", []byte(fmt.Sprintf("%T", arg)))
	}
}

func marshal(kind string, obj interface{}) []byte {
	data, err := yaml.Marshal(obj)
	if err != nil {
		data = []byte(fmt.Sprintf("<failed to marshal details of %s (%T): %v>", kind, obj, err))
	}
	return data
}
