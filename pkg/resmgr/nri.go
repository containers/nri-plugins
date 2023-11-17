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

	"github.com/containers/nri-plugins/pkg/instrumentation/tracing"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
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
		return fmt.Errorf("failed to create NRI plugin stub: %w", err)
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
		return fmt.Errorf("failed to start NRI plugin: %w", err)
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

func (p *nriPlugin) Configure(ctx context.Context, cfg, runtime, version string) (stub.EventMask, error) {
	event := Configure

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(
			tracing.Attribute(SpanTagRuntimeName, runtime),
			tracing.Attribute(SpanTagRuntimeVersion, version),
		),
	)
	defer func() {
		span.End()
	}()

	p.dump(in, event, runtime, version)

	return api.MustParseEventMask(
		"RunPodSandbox,StopPodSandbox,RemovePodSandbox",
		"CreateContainer,StartContainer,UpdateContainer,StopContainer,RemoveContainer",
	), nil
}

func (p *nriPlugin) syncWithNRI(pods []*api.PodSandbox, containers []*api.Container) ([]cache.Container, []cache.Container, error) {
	m := p.resmgr

	allocated := []cache.Container{}
	released := []cache.Container{}

	m.Info("synchronizing cache state with NRI runtime...")

	_, _, deleted := m.cache.RefreshPods(pods)
	for _, c := range deleted {
		m.Info("discovered stale container %s...", c.GetID())
		released = append(released, c)
	}

	_, deleted = m.cache.RefreshContainers(containers)
	for _, c := range deleted {
		m.Info("discovered stale container %s...", c.GetID())
		released = append(released, c)
	}

	/* Go through all containers in the cache and check if we need to keep
	 * or remove their resource allocations.
	 */
	ctrs := m.cache.GetContainers()
	for _, c := range ctrs {
		switch c.GetState() {
		case cache.ContainerStateRunning, cache.ContainerStateCreated:
			m.Info("discovered out-of-sync running container %s...", c.GetID())
			allocated = append(allocated, c)

			/* By adding out-of-sync container to released list, we force re-allocation of
			 * the resources when calling policy.Start()
			 */
			released = append(released, c)

		case cache.ContainerStateExited:
			/* Treat stopped containers as deleted */
			m.Info("discovered stale stopped container %s...", c.GetID())
			released = append(released, c)

		default:
			m.Info("ignoring discovered container %s (in state %v)...", c.GetID(), c.GetState())
		}
	}

	return allocated, released, nil
}

func (p *nriPlugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) (updates []*api.ContainerUpdate, retErr error) {
	event := Synchronize

	_, span := tracing.StartSpan(
		ctx,
		event,
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

	p.dump(in, event, pods, containers)
	defer func() {
		p.dump(out, event, updates, retErr)
	}()

	m := p.resmgr

	allocated, released, err := p.syncWithNRI(pods, containers)
	if err != nil {
		p.resmgr.Error("failed to synchronize with NRI: %v", err)
		return nil, err
	}

	if err := m.policy.Sync(allocated, released); err != nil {
		return nil, fmt.Errorf("failed to sync policy %s: %w", m.policy.ActivePolicy(), err)
	}

	m.updateTopologyZones()

	return p.getPendingUpdates(nil), nil
}

func (p *nriPlugin) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) (retErr error) {
	event := RunPodSandbox

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(podSpanTags(pod)...),
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

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

func (p *nriPlugin) StopPodSandbox(ctx context.Context, podSandbox *api.PodSandbox) (retErr error) {
	event := StopPodSandbox

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(podSpanTags(podSandbox)...),
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

	m := p.resmgr

	released := []cache.Container{}
	pod, _ := m.cache.LookupPod(podSandbox.GetId())

	for _, c := range pod.GetContainers() {
		released = append(released, c)
	}

	if err := p.runPostReleaseHooks(event, released...); err != nil {
		m.Error("%s: failed to run post-release hooks for pod %s: %v",
			event, pod.GetName(), err)
	}

	p.dump(in, event, podSandbox)
	defer func() {
		p.dump(out, event, retErr)
	}()

	return nil
}

func (p *nriPlugin) RemovePodSandbox(ctx context.Context, podSandbox *api.PodSandbox) (retErr error) {
	event := RemovePodSandbox

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(podSpanTags(podSandbox)...),
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

	p.dump(in, event, podSandbox)
	defer func() {
		p.dump(out, event, retErr)
	}()

	m := p.resmgr

	released := []cache.Container{}
	pod, _ := m.cache.LookupPod(podSandbox.GetId())

	for _, c := range pod.GetContainers() {
		released = append(released, c)
	}

	if err := p.runPostReleaseHooks(event, released...); err != nil {
		m.Error("%s: failed to run post-release hooks for pod %s: %v",
			event, pod.GetName(), err)
	}

	m.Lock()
	defer m.Unlock()

	m.cache.DeletePod(podSandbox.GetId())
	return nil
}

func (p *nriPlugin) CreateContainer(ctx context.Context, podSandbox *api.PodSandbox, container *api.Container) (adjust *api.ContainerAdjustment, updates []*api.ContainerUpdate, retErr error) {
	event := CreateContainer

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(containerSpanTags(podSandbox, container)...),
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

	p.dump(in, event, podSandbox, container)
	defer func() {
		p.dump(out, event, adjust, updates, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	c, err := m.cache.InsertContainer(container)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to cache container: %w", err)
	}
	c.UpdateState(cache.ContainerStateCreating)

	if err := m.policy.AllocateResources(c); err != nil {
		c.UpdateState(cache.ContainerStateStale)
		return nil, nil, fmt.Errorf("failed to allocate resources: %w", err)
	}
	c.UpdateState(cache.ContainerStateCreated)

	c.InsertMount(&cache.Mount{
		Destination: "/.nri-resource-policy",
		Source:      m.cache.ContainerDirectory(c.GetID()),
		Type:        "bind",
		Options:     []string{"bind", "ro", "rslave"},
	})

	if err := p.runPostAllocateHooks(event, c); err != nil {
		m.Error("%s: failed to run post-allocate hooks for %s: %v",
			event, container.GetName(), err)
		p.runPostReleaseHooks(event, c)
		return nil, nil, fmt.Errorf("failed to allocate container resources: %w", err)
	}

	m.policy.ExportResourceData(c)
	m.updateTopologyZones()

	adjust = p.getPendingAdjustment(container)
	updates = p.getPendingUpdates(container)

	return adjust, updates, nil
}

func (p *nriPlugin) StartContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) (retErr error) {
	event := StartContainer

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(containerSpanTags(pod, container)...),
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

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

	if err := p.runPostStartHooks(event, c); err != nil {
		m.Error("%s: failed to run post-start hooks for %s: %v",
			event, c.PrettyName(), err)
	}

	return nil
}

func (p *nriPlugin) UpdateContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container, res *api.LinuxResources) (updates []*api.ContainerUpdate, retErr error) {
	event := UpdateContainer

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(containerSpanTags(pod, container)...),
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

	p.dump(in, event, pod, container, res)
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

	if realUpdates := c.SetResourceUpdates(res); !realUpdates {
		p.Warn("UpdateContainer with identical resources, ignoring it...")
		return nil, nil
	}
	//r := cache.EstimateResourceRequirements(res, c.GetQOSClass())

	if err := m.policy.UpdateResources(c); err != nil {
		return nil, fmt.Errorf("failed to update resources: %w", err)
	}

	return p.getPendingUpdates(nil), nil
}

func (p *nriPlugin) StopContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) (updates []*api.ContainerUpdate, retErr error) {
	event := StopContainer

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(containerSpanTags(pod, container)...),
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

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
		return nil, fmt.Errorf("failed to release resources: %w", err)
	}

	c.UpdateState(cache.ContainerStateExited)
	m.updateTopologyZones()

	return p.getPendingUpdates(container), nil
}

func (p *nriPlugin) RemoveContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) (retErr error) {
	event := RemoveContainer

	_, span := tracing.StartSpan(
		ctx,
		event,
		tracing.WithAttributes(containerSpanTags(pod, container)...),
	)
	defer func() {
		span.End(tracing.WithStatus(retErr))
	}()

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

	case CreateContainer, StartContainer, StopContainer, RemoveContainer:
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

	case UpdateContainer:
		if dir == in {
			if len(args) != 3 {
				p.Error("%s %s <dump error, %d args, expected (pod, container, resources)>",
					dir, event, len(args))
				return
			}

			pod, ok := args[0].(*api.PodSandbox)
			if !ok {
				p.Error("%s %s <dump error, args %T, %T, %T, expected (pod, container, resources)>",
					dir, event, args[0], args[1], args[2])
				return
			}
			ctr, ok := args[1].(*api.Container)
			if !ok {
				p.Error("%s %s <dump error, args %T, %T, %T, expected (pod, container, resources)>",
					dir, event, args[0], args[1], args[2])
				return
			}
			res, ok := args[2].(*api.LinuxResources)
			if !ok {
				p.Error("%s %s <dump error, args %T, %T, %T, expected (pod, container, resources)>",
					dir, event, args[0], args[1], args[2])
				return
			}

			p.Info("%s %s %s/%s:%s", dir, event, pod.GetNamespace(), pod.GetName(), ctr.GetName())
			p.dumpDetails(dir, event, ctr)
			p.dumpDetails(dir, event, res)
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
		case RunPodSandbox, CreateContainer, UpdateContainer, Synchronize:
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

	case *api.LinuxResources:
		data := marshal("updated resources", obj)
		p.DebugBlock(dir+"   <update> ", "%s", data)

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

const (
	SpanTagRuntimeName    = "runtime.name"
	SpanTagRuntimeVersion = "runtime.version"
	SpanTagNamespace      = "pod.namespace"
	SpanTagPodID          = "pod.id"
	SpanTagPodUID         = "pod.uid"
	SpanTagPodName        = "pod.name"
	SpanTagCtrID          = "container.id"
	SpanTagCtrName        = "container.name"
)

func podSpanTags(pod *api.PodSandbox) []tracing.KeyValue {
	namespace, podName := pod.GetNamespace(), pod.GetName()
	return []tracing.KeyValue{
		tracing.Attribute(SpanTagNamespace, namespace),
		tracing.Attribute(SpanTagPodID, pod.GetId()),
		tracing.Attribute(SpanTagPodUID, pod.GetUid()),
		tracing.Attribute(SpanTagPodName, podName),
	}
}

func containerSpanTags(pod *api.PodSandbox, ctr *api.Container) []tracing.KeyValue {
	return append(podSpanTags(pod),
		tracing.Attribute(SpanTagCtrID, ctr.GetId()),
		tracing.Attribute(SpanTagCtrName, ctr.GetName()),
	)
}

// runPostAllocateHooks runs the necessary hooks after allocating resources for some containers.
func (p *nriPlugin) runPostAllocateHooks(method string, created cache.Container) error {
	m := p.resmgr
	for _, c := range m.cache.GetPendingContainers() {
		if c == created {
			if err := m.control.RunPreCreateHooks(c); err != nil {
				m.Warn("%s pre-create hook failed for %s: %v",
					method, c.PrettyName(), err)
			}
			continue
		}

		switch c.GetState() {
		case cache.ContainerStateRunning, cache.ContainerStateCreated:
			if err := m.control.RunPostUpdateHooks(c); err != nil {
				m.Warn("%s post-update hook failed for %s: %v",
					method, c.PrettyName(), err)
			}
		default:
			m.Warn("%s: skipping container %s (in state %v)", method,
				c.PrettyName(), c.GetState())
		}
	}
	return nil
}

// runPostStartHooks runs the necessary hooks after having started a container.
func (p *nriPlugin) runPostStartHooks(method string, c cache.Container) error {
	m := p.resmgr
	if err := m.control.RunPostStartHooks(c); err != nil {
		m.Error("%s: post-start hook failed for %s: %v", method, c.PrettyName(), err)
	}
	return nil
}

// runPostReleaseHooks runs the necessary hooks after releaseing resources of some containers
func (p *nriPlugin) runPostReleaseHooks(method string, released ...cache.Container) error {
	m := p.resmgr
	for _, c := range released {
		if err := m.control.RunPostStopHooks(c); err != nil {
			m.Warn("post-stop hook failed for %s: %v", c.PrettyName(), err)
		}
	}
	for _, c := range m.cache.GetPendingContainers() {
		switch state := c.GetState(); state {
		case cache.ContainerStateStale, cache.ContainerStateExited:
			if err := m.control.RunPostStopHooks(c); err != nil {
				m.Warn("post-stop hook failed for %s: %v", c.PrettyName(), err)
			}
		case cache.ContainerStateRunning, cache.ContainerStateCreated:
			if err := m.control.RunPostUpdateHooks(c); err != nil {
				m.Warn("post-update hook failed for %s: %v", c.PrettyName(), err)
			}
		default:
			m.Warn("%s: skipping pending container %s (in state %v)",
				method, c.PrettyName(), c.GetState())
		}
	}
	return nil
}

// runPostUpdateHooks runs the necessary hooks after reconcilation.
func (p *nriPlugin) runPostUpdateHooks(method string) error {
	m := p.resmgr
	for _, c := range m.cache.GetPendingContainers() {
		switch c.GetState() {
		case cache.ContainerStateRunning, cache.ContainerStateCreated:
			if err := m.control.RunPostUpdateHooks(c); err != nil {
				return err
			}
		default:
			m.Warn("%s: skipping container %s (in state %v)", method,
				c.PrettyName(), c.GetState())
		}
	}
	return nil
}
