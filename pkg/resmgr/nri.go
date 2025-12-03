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
	"os"
	"slices"
	"time"

	"github.com/containers/nri-plugins/pkg/instrumentation/tracing"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	"sigs.k8s.io/yaml"

	"github.com/containerd/nri/pkg/api"
	stub "github.com/containerd/nri/pkg/stub"
	"github.com/containerd/otelttrpc"
	"github.com/containerd/ttrpc"
)

type nriPlugin struct {
	stub   stub.Stub
	resmgr *resmgr
	byname map[string]cache.Container
}

var (
	nri = logger.NewLogger("nri-plugin")
)

const (
	podResListTimeout = 2 * time.Second
	podResGetTimeout  = 1 * time.Second
)

func newNRIPlugin(resmgr *resmgr) (*nriPlugin, error) {
	p := &nriPlugin{
		resmgr: resmgr,
		byname: make(map[string]cache.Container),
	}

	nri.Info("creating plugin...")

	return p, nil
}

func (p *nriPlugin) createStub() error {
	var (
		opts = []stub.Option{
			stub.WithPluginName(opt.NriPluginName),
			stub.WithPluginIdx(opt.NriPluginIdx),
			stub.WithSocketPath(opt.NriSocket),
			stub.WithOnClose(p.onClose),
			stub.WithTTRPCOptions(
				[]ttrpc.ClientOpts{
					ttrpc.WithUnaryClientInterceptor(
						otelttrpc.UnaryClientInterceptor(),
					),
				},
				[]ttrpc.ServerOpt{
					ttrpc.WithUnaryServerInterceptor(
						otelttrpc.UnaryServerInterceptor(),
					),
				},
			),
		}
		err error
	)

	nri.Info("creating plugin stub...")

	if p.stub, err = stub.New(p, opts...); err != nil {
		return fmt.Errorf("failed to create NRI plugin stub: %w", err)
	}

	return nil
}

func (p *nriPlugin) start() error {
	if p == nil {
		return nil
	}

	nri.Info("starting plugin...")

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

	nri.Info("stopping plugin...")
	p.stub.Stop()
}

func (p *nriPlugin) onClose() {
	nri.Error("connection to NRI/runtime lost, exiting...")
	os.Exit(1)
}

func (p *nriPlugin) syncNamesToContainers(containers []cache.Container) []cache.Container {
	unmapped := make([]cache.Container, 0, len(p.byname))

	for _, ctr := range containers {
		old := p.mapNameToContainer(ctr)
		if old != nil && old.GetID() != ctr.GetID() {
			unmapped = append(unmapped, old)
		}
	}

	return unmapped
}

func (p *nriPlugin) mapNameToContainer(ctr cache.Container) cache.Container {
	name := ctr.PrettyName()
	old, ok := p.byname[name]

	p.byname[name] = ctr
	if ok {
		nri.Info("%s: remapped container from %s to %s", name, old.GetID(), ctr.GetID())
		return old
	}

	nri.Info("%s: mapped container to %s", name, ctr.GetID())
	return nil
}

func (p *nriPlugin) unmapName(name string) (cache.Container, bool) {
	old, ok := p.byname[name]
	if ok {
		delete(p.byname, name)
		nri.Info("%s: unmapped container from %s", name, old.GetID())
	}
	return old, ok
}

func (p *nriPlugin) unmapContainer(ctr cache.Container) {
	name := ctr.PrettyName()
	old, ok := p.byname[name]
	if ok {
		if old == ctr {
			delete(p.byname, name)
			nri.Info("%s: unmapped container (%s)", name, ctr.GetID())
		} else {
			nri.Warn("%s: leaving container mapped, ID mismatch (%s != %s)", name,
				old.GetID(), ctr.GetID())
		}
	}
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

	nri.Info("synchronizing cache state with NRI runtime...")

	_, _, deleted := m.cache.RefreshPods(pods, m.agent.GoListPodResources(podResListTimeout))
	for _, c := range deleted {
		nri.Info("discovered stale container %s (%s)...", c.PrettyName(), c.GetID())
		released = append(released, c)
	}

	_, deleted = m.cache.RefreshContainers(containers)
	for _, c := range deleted {
		nri.Info("discovered stale container %s (%s)...", c.PrettyName(), c.GetID())
		released = append(released, c)
	}

	/* Go through all containers in the cache and check if we need to keep
	 * or remove their resource allocations.
	 */
	ctrs := m.cache.GetContainers()
	for _, c := range ctrs {
		switch c.GetState() {
		case cache.ContainerStateRunning, cache.ContainerStateCreated:
			nri.Info("discovered created/running container %s (%s)...",
				c.PrettyName(), c.GetID())
			allocated = append(allocated, c)

			/* By adding out-of-sync container to released list, we force re-allocation of
			 * the resources when calling policy.Start()
			 */
			released = append(released, c)

		case cache.ContainerStateExited:
			/* Treat stopped containers as deleted */
			nri.Info("discovered stopped container %s (%s)...",
				c.PrettyName(), c.GetID())
			released = append(released, c)

		default:
			nri.Info("discovered container %s (%s), in state %v, ignoring it...",
				c.PrettyName(), c.GetID(), c.GetState())
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
		nri.Error("failed to synchronize with NRI: %v", err)
		return nil, err
	}

	unmapped := p.syncNamesToContainers(allocated)
	if err := m.policy.Sync(allocated, append(released, unmapped...)); err != nil {
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

	m := p.resmgr
	podResCh := m.agent.GoGetPodResources(pod.GetNamespace(), pod.GetName(), podResGetTimeout)

	p.dump(in, event, pod)
	defer func() {
		p.dump(out, event, retErr)
	}()

	m.Lock()
	defer m.Unlock()

	m.cache.InsertPod(pod, podResCh)

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

	// TODO(klihub): shouldn't we m.Lock()/defer m.Unlock() here?

	pod, _ := m.cache.LookupPod(podSandbox.GetId())
	released := slices.Clone(pod.GetContainers())
	m.agent.PurgePodResources(pod.GetNamespace(), pod.GetName())

	if err := p.runPostReleaseHooks(event, released...); err != nil {
		nri.Error("%s: failed to run post-release hooks for pod %s: %v",
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

	pod, _ := m.cache.LookupPod(podSandbox.GetId())
	released := slices.Clone(pod.GetContainers())
	m.agent.PurgePodResources(pod.GetNamespace(), pod.GetName())

	if err := p.runPostReleaseHooks(event, released...); err != nil {
		nri.Error("%s: failed to run post-release hooks for pod %s: %v",
			event, pod.GetName(), err)
	}

	m.Lock()
	defer m.Unlock()

	m.cache.DeletePod(podSandbox.GetId())
	return nil
}

func (p *nriPlugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) (adjust *api.ContainerAdjustment, updates []*api.ContainerUpdate, retErr error) {
	event := CreateContainer

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
		p.dump(out, event, adjust, updates, retErr)
	}()

	m := p.resmgr
	m.Lock()
	defer m.Unlock()

	c, err := m.cache.InsertContainer(container, cache.WithContainerState(cache.ContainerStateCreating))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to cache container: %w", err)
	}

	if old, ok := p.unmapName(c.PrettyName()); ok {
		nri.Info("%s: releasing stale instance %s", c.PrettyName(), old.GetID())
		if err := m.policy.ReleaseResources(old); err != nil {
			nri.Error("%s: failed to release stale instance %s", c.PrettyName(), old.GetID())
		}
		old.UpdateState(cache.ContainerStateExited)
	}

	if err := m.policy.AllocateResources(c); err != nil {
		c.UpdateState(cache.ContainerStateStale)
		return nil, nil, fmt.Errorf("failed to allocate resources: %w", err)
	}

	c.InsertMount(&cache.Mount{
		Destination: "/.nri-resource-policy",
		Source:      m.cache.ContainerDirectory(c.GetID()),
		Type:        "bind",
		Options:     []string{"bind", "ro", "rslave"},
	})

	c.UpdateState(cache.ContainerStateCreated)

	if err := p.runPostAllocateHooks(event, c); err != nil {
		nri.Error("%s: failed to run post-allocate hooks for %s: %v",
			event, container.GetName(), err)
		relErr := p.runPostReleaseHooks(event, c)
		if relErr != nil {
			nri.Warnf("%s: failed to run post-release hooks on error for %s: %v",
				event, container.GetName(), relErr)
		}
		return nil, nil, fmt.Errorf("failed to allocate container resources: %w", err)
	}

	m.policy.ExportResourceData(c)
	m.updateTopologyZones()

	adjust = p.getPendingAdjustment(container)
	updates = p.getPendingUpdates(container)

	p.mapNameToContainer(c)

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
		nri.Error("%s: policy failed to handle event %s: %v", event, e.Type, err)
	}

	if err := p.runPostStartHooks(event, c); err != nil {
		nri.Error("%s: failed to run post-start hooks for %s: %v",
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
		nri.Warn("UpdateContainer with identical resources, short-circuiting it...")
		if v := c.GetCPUShares(); v != 0 {
			c.SetCPUShares(v)
		}
		if v := c.GetCPUQuota(); v != 0 {
			c.SetCPUQuota(v)
		}
		if v := c.GetCPUPeriod(); v != 0 {
			c.SetCPUPeriod(v)
		}
		if v := c.GetCpusetCpus(); v != "" {
			c.SetCpusetCpus(v)
		}
		if v := c.GetCpusetMems(); v != "" {
			c.SetCpusetMems(v)
		}
		if v := c.GetMemoryLimit(); v != 0 {
			c.SetMemoryLimit(v)
		}
		if v := c.GetMemorySwap(); v != 0 {
			c.SetMemorySwap(v)
		}
	} else {
		old := c.GetResourceRequirements()
		upd, _ := c.GetResourceUpdates()
		nri.Warn("UpdateContainer with real resource changes: %s -> %s",
			old.String(), upd.String())
		if err := m.policy.UpdateResources(c); err != nil {
			return nil, fmt.Errorf("failed to update resources: %w", err)
		}
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

	p.unmapContainer(c)

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
		p.setDefaultClasses(c, adjust)

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
			p.setDefaultClasses(c, u)
			updates = append(updates, u)

			for _, ctrl := range c.GetPending() {
				c.ClearPending(ctrl)
			}
			m.policy.ExportResourceData(c)
		}
	}

	return updates
}

func (p *nriPlugin) setDefaultClasses(c cache.Container, request interface{}) {
	var (
		rdtClass     string
		blockIOClass string
	)

	if p.resmgr.cfg.CommonConfig().Control.RDT.Enable {
		if p.resmgr.cfg.CommonConfig().Control.RDT.UsePodQoSAsDefaultClass {
			if class := c.GetRDTClass(); class == "" {
				rdtClass = string(c.GetQOSClass())
			}
		}
	}

	if p.resmgr.cfg.CommonConfig().Control.BlockIO.Enable {
		if p.resmgr.cfg.CommonConfig().Control.BlockIO.UsePodQoSAsDefaultClass {
			if class := c.GetBlockIOClass(); class == "" {
				blockIOClass = string(c.GetQOSClass())
			}
		}
	}

	switch req := request.(type) {
	case *api.ContainerAdjustment:
		if rdtClass != "" {
			req.SetLinuxRDTClass(rdtClass)
		}
		if blockIOClass != "" {
			req.SetLinuxBlockIOClass(blockIOClass)
		}
	case *api.ContainerUpdate:
		if rdtClass != "" {
			req.SetLinuxRDTClass(rdtClass)
		}
		if blockIOClass != "" {
			req.SetLinuxBlockIOClass(blockIOClass)
		}
	}
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
				nri.Error("%s %s <dump error, %d args, expected (pod)>", dir, event, len(args))
				return
			}

			pod, ok := args[0].(*api.PodSandbox)
			if !ok {
				nri.Error("%s %s <dump error, arg %T, expected (pod)>", dir, event, args[0])
				return
			}

			nri.Info("%s %s %s/%s", dir, event, pod.GetNamespace(), pod.GetName())
			p.dumpDetails(dir, event, pod)
		} else {
			if len(args) != 1 {
				nri.Error("%s %s <dump error, %d args, expected (err/nil)>", dir, event, len(args))
				return
			}

			err := args[0]
			if err != nil {
				nri.Error("%s %s FAILED: %v", dir, event, err.(error))
				return
			}

			nri.Info("%s %s", dir, event)
		}

	case CreateContainer, StartContainer, StopContainer, RemoveContainer:
		if dir == in {
			if len(args) != 2 {
				nri.Error("%s %s <dump error, %d args, expected (pod, container)>",
					dir, event, len(args))
				return
			}

			pod, ok := args[0].(*api.PodSandbox)
			if !ok {
				nri.Error("%s %s <dump error, args %T, %T, expected (pod, container)>",
					dir, event, args[0], args[1])
				return
			}
			ctr, ok := args[1].(*api.Container)
			if !ok {
				nri.Error("%s %s <dump error, args %T, %T, expected (pod, container)>",
					dir, event, args[0], args[1])
				return
			}

			nri.Info("%s %s %s/%s/%s (%s)", dir, event,
				pod.GetNamespace(), pod.GetName(), ctr.GetName(), ctr.GetId())
			p.dumpDetails(dir, event, ctr)
		} else {
			if len(args) < 1 {
				nri.Error("%s %s <dump error, missing args>", dir, event)
				return
			}

			err := args[len(args)-1]
			if err != nil {
				nri.Error("%s %s FAILED: %v", dir, event, err.(error))
				return
			}

			nri.Info("%s %s", dir, event)

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
				nri.Error("%s %s <dump error, %d args, expected (pod, container, resources)>",
					dir, event, len(args))
				return
			}

			pod, ok := args[0].(*api.PodSandbox)
			if !ok {
				nri.Error("%s %s <dump error, args %T, %T, %T, expected (pod, container, resources)>",
					dir, event, args[0], args[1], args[2])
				return
			}
			ctr, ok := args[1].(*api.Container)
			if !ok {
				nri.Error("%s %s <dump error, args %T, %T, %T, expected (pod, container, resources)>",
					dir, event, args[0], args[1], args[2])
				return
			}
			res, ok := args[2].(*api.LinuxResources)
			if !ok {
				nri.Error("%s %s <dump error, args %T, %T, %T, expected (pod, container, resources)>",
					dir, event, args[0], args[1], args[2])
				return
			}

			nri.Info("%s %s %s/%s/%s (%s)", dir, event,
				pod.GetNamespace(), pod.GetName(), ctr.GetName(), ctr.GetId())
			p.dumpDetails(dir, event, ctr)
			p.dumpDetails(dir, event, res)
		} else {
			if len(args) < 1 {
				nri.Error("%s %s <dump error, missing args>", dir, event)
				return
			}

			err := args[len(args)-1]
			if err != nil {
				nri.Error("%s %s FAILED: %v", dir, event, err.(error))
				return
			}

			nri.Info("%s %s", dir, event)

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
				nri.Error("%s %s <dump error, %d args, expected (update)>", dir, event, len(args))
				return
			}

			nri.Info("%s %s", dir, event)
			p.dumpDetails(dir, event, args[0])
		} else {
			if len(args) != 1 {
				nri.Error("%s %s <dump error, %d args, expected (err/nil)>", dir, event, len(args))
				return
			}

			err := args[0]
			if err == nil {
				nri.Info("%s %s", dir, event)
				return
			}

			nri.Error("%s %s FAILED: %v", dir, event, err.(error))
		}

	case Configure:
		if dir == in {
			if len(args) != 2 {
				nri.Error("%s %s <dump error, %d args, expected (runtime, version)>",
					dir, event, len(args))
				return
			}

			runtime, ok := args[0].(string)
			if !ok {
				nri.Error("%s %s <dump error, args %T, %T, expected (runtime, version)>",
					dir, event, args[0], args[1])
				return
			}
			version, ok := args[1].(string)
			if !ok {
				nri.Error("%s %s <dump error, args %T, %T, expected (runtime, version)>",
					dir, event, args[0], args[1])
				return
			}

			nri.Info("%s %s, runtime %s %s", dir, event, runtime, version)
		} else {
			nri.Info("%s %s", dir, event)
		}

	case Synchronize:
		if dir == in {
			if len(args) != 2 {
				nri.Error("%s %s <dump error, %d args, expected (pods, containers)",
					dir, event, len(args))
			}

			nri.Info("%s %s", dir, event)
			p.dumpDetails(dir, event, args[0])
			p.dumpDetails(dir, event, args[1])
		} else {
			if len(args) != 2 {
				nri.Error("%s %s <dump error, %d args, expected (updates, err/nil)",
					dir, event, len(args))
				return
			}

			err := args[1]
			if err != nil {
				nri.Error("%s %s FAILED: %v", dir, event, err.(error))
				return
			}

			nri.Info("%s %s", dir, event)
			p.dumpDetails(dir, event, args[0])
		}

	default:
		nri.Info("%s %s", dir, event)
	}
}

func (p *nriPlugin) dumpDetails(dir, event string, arg interface{}) {
	// if debug is off for our debug source, we don't dump any details
	if !nri.DebugEnabled() {
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
		nri.DebugBlock(dir+"   <pod> ", "%s", data)

	case *api.Container:
		data := marshal("container", obj)
		nri.DebugBlock(dir+"   <ctr> ", "%s", data)

	case *api.LinuxResources:
		data := marshal("updated resources", obj)
		nri.DebugBlock(dir+"   <update> ", "%s", data)

	case *api.ContainerAdjustment:
		data := marshal("adjustment", obj)
		nri.DebugBlock(dir+"   <adjustment> ", "%s", data)

	case []*api.ContainerUpdate:
		for idx, update := range obj {
			data := marshal("update", update)
			nri.DebugBlock(dir+fmt.Sprintf("   <update #%d> ", idx), "%s", data)
		}

	case []*api.PodSandbox:
		for idx, pod := range obj {
			data := marshal("pod", pod)
			nri.DebugBlock(dir+fmt.Sprintf("   <pod #%d> ", idx), "%s", data)
		}

	case []*api.Container:
		for idx, ctr := range obj {
			data := marshal("container", ctr)
			nri.DebugBlock(dir+fmt.Sprintf("   <ctr #%d> ", idx), "%s", data)
		}
	default:
		nri.DebugBlock(dir+"   <unknown data of type> ", "%s", []byte(fmt.Sprintf("%T", arg)))
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
				nri.Warn("%s pre-create hook failed for %s: %v",
					method, c.PrettyName(), err)
			}
			continue
		}

		switch c.GetState() {
		case cache.ContainerStateRunning, cache.ContainerStateCreated:
			if err := m.control.RunPostUpdateHooks(c); err != nil {
				nri.Warn("%s post-update hook failed for %s: %v",
					method, c.PrettyName(), err)
			}
		default:
			nri.Warn("%s: skipping container %s (in state %v)", method,
				c.PrettyName(), c.GetState())
		}
	}
	return nil
}

// runPostStartHooks runs the necessary hooks after having started a container.
func (p *nriPlugin) runPostStartHooks(method string, c cache.Container) error {
	m := p.resmgr
	if err := m.control.RunPostStartHooks(c); err != nil {
		nri.Error("%s: post-start hook failed for %s: %v", method, c.PrettyName(), err)
	}
	return nil
}

// runPostReleaseHooks runs the necessary hooks after releasing resources of some containers
func (p *nriPlugin) runPostReleaseHooks(method string, released ...cache.Container) error {
	m := p.resmgr
	for _, c := range released {
		if err := m.control.RunPostStopHooks(c); err != nil {
			nri.Warn("post-stop hook failed for %s: %v", c.PrettyName(), err)
		}
	}
	for _, c := range m.cache.GetPendingContainers() {
		switch state := c.GetState(); state {
		case cache.ContainerStateStale, cache.ContainerStateExited:
			if err := m.control.RunPostStopHooks(c); err != nil {
				nri.Warn("post-stop hook failed for %s: %v", c.PrettyName(), err)
			}
		case cache.ContainerStateRunning, cache.ContainerStateCreated:
			if err := m.control.RunPostUpdateHooks(c); err != nil {
				nri.Warn("post-update hook failed for %s: %v", c.PrettyName(), err)
			}
		default:
			nri.Warn("%s: skipping pending container %s (in state %v)",
				method, c.PrettyName(), c.GetState())
		}
	}
	return nil
}
