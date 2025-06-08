// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package policy

import (
	"bytes"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	"github.com/prometheus/client_golang/prometheus"

	logger "github.com/containers/nri-plugins/pkg/log"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	// nrt "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
)

// Domain represents a hardware resource domain that can be policied by a backend.
type Domain string

const (
	// DomainCPU is the CPU resource domain.
	DomainCPU Domain = "CPU"
	// DomainMemory is the memory resource domain.
	DomainMemory Domain = "Memory"
	// DomainHugePage is the hugepages resource domain.
	DomainHugePage Domain = "HugePages"
	// DomainCache is the CPU cache resource domain.
	DomainCache Domain = "Cache"
	// DomainMemoryBW is the memory resource bandwidth.
	DomainMemoryBW Domain = "MBW"
)

// Constraint describes constraint of one hardware domain
type Constraint interface{}

// ConstraintSet describes, per hardware domain, the resources available for a policy.
type ConstraintSet map[Domain]Constraint

// Options describes policy options
type Options struct {
	// SendEvent is the function for delivering events back to the resource manager.
	SendEvent SendEventFn
}

// BackendOptions describes the options for a policy backend instance
type BackendOptions struct {
	// System provides system/HW/topology information
	System system.System
	// System state/cache
	Cache cache.Cache
	// SendEvent is the function for delivering events up to the resource manager.
	SendEvent SendEventFn
	// Config is the policy-specific configuration.
	Config interface{}
}

// CreateFn is the type for functions used to create a policy instance.
type CreateFn func(*BackendOptions) Backend

// SendEventFn is the type for a function to send events back to the resource manager.
type SendEventFn func(interface{}) error

const (
	// ExportedResources is the basename of the file container resources are exported to.
	ExportedResources = "resources.sh"
	// ExportSharedCPUs is the shell variable used to export shared container CPUs.
	ExportSharedCPUs = "SHARED_CPUS"
	// ExportIsolatedCPUs is the shell variable used to export isolated container CPUs.
	ExportIsolatedCPUs = "ISOLATED_CPUS"
	// ExportExclusiveCPUs is the shell variable used to export exclusive container CPUs.
	ExportExclusiveCPUs = "EXCLUSIVE_CPUS"
)

// Backend is the policy (decision making logic) interface exposed by implementations.
//
// A backends operates in a set of policy domains. Currently each policy domain
// corresponds to some particular hardware resource (CPU, memory, cache, etc).
type Backend interface {
	// Name gets the well-known name of this policy.
	Name() string
	// Description gives a verbose description about the policy implementation.
	Description() string
	// Setup initializes the policy backend with the given options.
	Setup(*BackendOptions) error
	// Reconfigure the policy backend.
	Reconfigure(interface{}) error
	// Start up and sycnhronizes the policy, using the given cache and resource constraints.
	Start() error
	// Sync synchronizes the policy, allocating/releasing the given containers.
	Sync([]cache.Container, []cache.Container) error
	// AllocateResources allocates resources to/for a container.
	AllocateResources(cache.Container) error
	// ReleaseResources release resources of a container.
	ReleaseResources(cache.Container) error
	// UpdateResources updates resource allocations of a container.
	UpdateResources(cache.Container) error
	// HandleEvent processes the given event. The returned boolean indicates whether
	// changes have been made to any of the containers while handling the event.
	HandleEvent(*events.Policy) (bool, error)
	// ExportResourceData provides resource data to export for the container.
	ExportResourceData(cache.Container) map[string]string
	// GetMetrics returns the policy-specific metrics collector.
	GetMetrics() Metrics
	// GetTopologyZones returns the policy/pool data for 'topology zone' CRDs.
	GetTopologyZones() []*TopologyZone
}

// Policy is the exposed interface for container resource allocations decision making.
type Policy interface {
	// ActivePolicy returns the name of the policy backend in use.
	ActivePolicy() string
	// Start starts up policy, prepare for serving resource management requests.
	Start(interface{}) error
	// Reconfigure the policy.
	Reconfigure(interface{}) error
	// Sync synchronizes the state of the active policy.
	Sync([]cache.Container, []cache.Container) error
	// AllocateResources allocates resources to a container.
	AllocateResources(cache.Container) error
	// ReleaseResources releases resources of a container.
	ReleaseResources(cache.Container) error
	// UpdateResources updates resource allocations of a container.
	UpdateResources(cache.Container) error
	// HandleEvent passes on the given event to the active policy. The returned boolean
	// indicates whether changes have been made to any of the containers while handling
	// the event.
	HandleEvent(*events.Policy) (bool, error)
	// ExportResourceData exports/updates resource data for the container.
	ExportResourceData(cache.Container)
	// GetTopologyZones returns the policy/pool data for 'topology zone' CRDs.
	GetTopologyZones() []*TopologyZone
}

// Metrics is the interface we expect policy-specific metrics to implement.
type Metrics interface {
	Describe(chan<- *prometheus.Desc)
	Collect(chan<- prometheus.Metric)
}

// Node resource topology resource and attribute names.
// XXX TODO(klihub): We'll probably need to add similar unified consts
//
//	for resource types (socket, die, NUMA node, etc.) and use them
//	in policies (for instance for TA pool 'kind's)
const (
	// MemoryResource is the resource name for memory
	MemoryResource = "memory"
	// CPUResource is the resource name for CPU
	CPUResource = "cpu"
	// MemsetAttribute is the attribute name for assignable memory set
	MemsetAttribute = "memory set"
	// CPUsAttribute is the attribute name for the assignable CPU set
	CPUsAttribute = "cpuset"
	// SharedCPUsAttribute is the attribute name for the assignable shared CPU set
	SharedCPUsAttribute = "shared cpuset"
	// ReservedCPUsAttribute is the attribute name for assignable the reserved CPU set
	ReservedCPUsAttribute = "reserved cpuset"
	// IsolatedCPUsAttribute is the attribute name for the assignable isolated CPU set
	IsolatedCPUsAttribute = "isolated cpuset"
	// ExcessCPUsAttribute is the attribute name for CPUs that
	// have been allocated yet not requested. For instance,
	// containers in a balloon request 1300 mCPU in total, so at
	// least 2 CPUs must be allocated to the balloon. This results
	// in excess 700 mCPUs available for bursting, for instance.
	ExcessCPUsAttribute = "excess cpus"
	// ComponentCPUsAttribute lists CPUs of components of a composite balloon
	ComponentCPUsAttribute = "component cpusets"
	// Exporting containers as topology subzones
	ContainerAllocationZoneType = "allocation for container"
)

// TopologyZone provides policy-/pool-specific data for 'node resource topology' CRs.
type TopologyZone struct {
	Name       string
	Parent     string
	Type       string
	Resources  []*ZoneResource
	Attributes []*ZoneAttribute
}

// ZoneResource is a resource available in some TopologyZone.
type ZoneResource struct {
	Name        string
	Capacity    resource.Quantity
	Allocatable resource.Quantity
	Available   resource.Quantity
}

// ZoneAttribute represents additional, policy-specific information about a zone.
type ZoneAttribute struct {
	Name  string
	Value string
}

// Policy instance/state.
type policy struct {
	options  Options          // policy options
	cache    cache.Cache      // system state cache
	active   Backend          // our active backend
	system   system.System    // system/HW/topology info
	pcollect *PolicyCollector // policy metrics collector
	scollect *SystemCollector // system metrics collector
}

// Out logger instance.
var log logger.Logger = logger.NewLogger("policy")

// NewPolicy creates a policy instance using the given backend.
func NewPolicy(backend Backend, cache cache.Cache, o *Options) (Policy, error) {
	log.Info("creating '%s' policy...", backend.Name())

	p := &policy{
		cache:   cache,
		options: *o,
		active:  backend,
	}

	sys, err := system.DiscoverSystem()
	if err != nil {
		return nil, policyError("failed to discover system topology: %v", err)
	}
	p.system = sys

	pcollect := p.newPolicyCollector()
	if err := pcollect.register(); err != nil {
		return nil, policyError("failed to register policy collector: %v", err)
	}
	p.pcollect = pcollect

	scollect := p.newSystemCollector()
	if err = scollect.register(); err != nil {
		return nil, policyError("failed to register system collector: %v", err)
	}
	p.scollect = scollect

	return p, nil
}

func (p *policy) ActivePolicy() string {
	if p.active != nil {
		return p.active.Name()
	}
	return ""
}

// Start starts up policy, preparing it for serving requests.
func (p *policy) Start(cfg interface{}) error {
	log.Info("activating '%s' policy...", p.active.Name())

	if err := p.active.Setup(&BackendOptions{
		Cache:     p.cache,
		System:    p.system,
		SendEvent: p.options.SendEvent,
		Config:    cfg,
	}); err != nil {
		return err
	}

	return p.active.Start()
}

// Reconfigure the policy.
func (p *policy) Reconfigure(cfg interface{}) error {
	return p.active.Reconfigure(cfg)
}

// Sync synchronizes the active policy state.
func (p *policy) Sync(add []cache.Container, del []cache.Container) error {
	return p.active.Sync(add, del)
}

// AllocateResources allocates resources for a container.
func (p *policy) AllocateResources(c cache.Container) error {
	return p.active.AllocateResources(c)
}

// ReleaseResources release resources of a container.
func (p *policy) ReleaseResources(c cache.Container) error {
	return p.active.ReleaseResources(c)
}

// UpdateResources updates resource allocations of a container.
func (p *policy) UpdateResources(c cache.Container) error {
	return p.active.UpdateResources(c)
}

// HandleEvent passes on the given event to the active policy.
func (p *policy) HandleEvent(e *events.Policy) (bool, error) {
	return p.active.HandleEvent(e)
}

// ExportResourceData exports/updates resource data for the container.
func (p *policy) ExportResourceData(c cache.Container) {
	var buf bytes.Buffer

	data := p.active.ExportResourceData(c)
	keys := []string{}
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := data[key]
		if _, err := buf.WriteString(fmt.Sprintf("%s=%q\n", key, value)); err != nil {
			log.Error("container %s: failed to export resource data (%s=%q)",
				c.PrettyName(), key, value)
			buf.Reset()
			break
		}
	}

	err := p.cache.WriteFile(c.GetID(), ExportedResources, 0644, buf.Bytes())
	if err != nil {
		log.Warnf("container %s: failed to export resource data: %v", c.PrettyName(), err)
	}
}

// GetTopologyZones returns the policy/pool data for 'topology zone' CRDs.
func (p *policy) GetTopologyZones() []*TopologyZone {
	return p.active.GetTopologyZones()
}
