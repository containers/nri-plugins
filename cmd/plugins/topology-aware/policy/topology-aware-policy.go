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

package topologyaware

import (
	"errors"
	"fmt"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/prometheus/client_golang/prometheus"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/cpuallocator"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	idset "github.com/intel/goresctrl/pkg/utils"
)

const (
	// PolicyName is the name of this policy.
	PolicyName = "topology-aware"
	// PolicyDescription is a short description of this policy.
	PolicyDescription = "A policy for prototyping memory tiering."
	// PolicyPath is the path of this policy in the configuration hierarchy.
	PolicyPath = "policy." + PolicyName

	// ColdStartDone is the event generated for the end of a container cold start period.
	ColdStartDone = "cold-start-done"
)

// allocations is our cache.Cachable for saving resource allocations in the cache.
type allocations struct {
	policy *policy
	grants map[string]Grant
}

// policy is our runtime state for this policy.
type policy struct {
	options      *policyapi.BackendOptions // options we were created or reconfigured with
	cfg          *cfgapi.Config
	cache        cache.Cache               // pod/container cache
	sys          system.System             // system/HW topology info
	allowed      cpuset.CPUSet             // bounding set of CPUs we're allowed to use
	reserved     cpuset.CPUSet             // system-/kube-reserved CPUs
	reserveCnt   int                       // number of CPUs to reserve if given as resource.Quantity
	isolated     cpuset.CPUSet             // (our allowed set of) isolated CPUs
	nodes        map[string]Node           // pool nodes by name
	pools        []Node                    // pre-populated node slice for scoring, etc...
	root         Node                      // root of our pool/partition tree
	nodeCnt      int                       // number of pools
	depth        int                       // tree depth
	allocations  allocations               // container pool assignments
	cpuAllocator cpuallocator.CPUAllocator // CPU allocator used by the policy
	coldstartOff bool                      // coldstart forced off (have movable PMEM zones)
}

var opt = &cfgapi.Config{}

// Make sure policy implements the policy.Backend interface.
var _ policyapi.Backend = &policy{}

// Whether we have coldstart forced off due to PMEM in movable memory zones.
var coldStartOff bool

// New creates a new uninitialized topology-aware policy instance.
func New() policyapi.Backend {
	return &policy{}
}

// Setup initializes the topology-aware policy instance.
func (p *policy) Setup(opts *policyapi.BackendOptions) error {
	cfg, ok := opts.Config.(*cfgapi.Config)
	if !ok {
		return policyError("failed initialize %s policy: config of wrong type %T",
			PolicyName, opts.Config)
	}
	log.Infof("initial configuration: %+v", cfg)

	p.cfg = cfg
	p.cache = opts.Cache
	p.sys = opts.System
	p.options = opts
	p.cpuAllocator = cpuallocator.NewCPUAllocator(opts.System)

	opt = cfg

	if err := p.initialize(); err != nil {
		return policyError("failed to initialize %s policy: %w", PolicyName, err)
	}

	p.registerImplicitAffinities()

	return nil
}

// Name returns the name of this policy.
func (p *policy) Name() string {
	return PolicyName
}

// Description returns the description for this policy.
func (p *policy) Description() string {
	return PolicyDescription
}

// Start prepares this policy for accepting allocation/release requests.
func (p *policy) Start() error {
	if err := p.restoreCache(); err != nil {
		return policyError("failed to start: %v", err)
	}

	// Turn coldstart forcibly off if we have movable non-DRAM memory.
	// Note that although this can change dynamically we only check it
	// during startup and trust users to either not fiddle with memory
	// or restart us if they do.
	p.checkColdstartOff()

	p.root.Dump("<post-start>")

	return nil
}

// Sync synchronizes the state of this policy.
func (p *policy) Sync(add []cache.Container, del []cache.Container) error {
	log.Debug("synchronizing state...")
	for _, c := range del {
		p.ReleaseResources(c)
	}
	for _, c := range add {
		p.AllocateResources(c)
	}

	return nil
}

// AllocateResources is a resource allocation request for this policy.
func (p *policy) AllocateResources(container cache.Container) error {
	log.Debug("allocating resources for %s...", container.PrettyName())

	err := p.allocateResources(container, "")
	if err != nil {
		return err
	}

	p.root.Dump("<post-alloc>")

	return nil
}

func (p *policy) allocateResources(container cache.Container, poolHint string) error {
	grant, err := p.allocatePool(container, poolHint)
	if err != nil {
		return policyError("failed to allocate resources for %s: %v",
			container.PrettyName(), err)
	}
	p.applyGrant(grant)
	p.updateSharedAllocations(&grant)

	return nil
}

// ReleaseResources is a resource release request for this policy.
func (p *policy) ReleaseResources(container cache.Container) error {
	log.Debug("releasing resources of %s...", container.PrettyName())

	if grant, found := p.releasePool(container); found {
		p.updateSharedAllocations(&grant)
	}

	p.root.Dump("<post-release>")

	return nil
}

// UpdateResources is a resource allocation update request for this policy.
func (p *policy) UpdateResources(container cache.Container) error {
	log.Debug("updating (reallocating) container %s...", container.PrettyName())

	grant, found := p.releasePool(container)
	if !found {
		log.Warnf("can't find allocation to update for %s", container.PrettyName())
		return p.AllocateResources(container)
	}
	p.updateSharedAllocations(&grant)

	poolHint := grant.GetCPUNode().Name()
	err := p.allocateResources(container, poolHint)
	if err != nil {
		return err
	}

	p.root.Dump("<post-update>")

	return nil
}

// HandleEvent handles policy-specific events.
func (p *policy) HandleEvent(e *events.Policy) (bool, error) {
	log.Debug("received policy event %s.%s with data %v...", e.Source, e.Type, e.Data)

	switch e.Type {
	case events.ContainerStarted:
		c, ok := e.Data.(cache.Container)
		if !ok {
			return false, policyError("%s event: expecting cache.Container Data, got %T",
				e.Type, e.Data)
		}
		log.Info("triggering coldstart period (if necessary) for %s", c.PrettyName())
		return false, p.triggerColdStart(c)
	case ColdStartDone:
		id, ok := e.Data.(string)
		if !ok {
			return false, policyError("%s event: expecting container ID Data, got %T",
				e.Type, e.Data)
		}
		c, ok := p.cache.LookupContainer(id)
		if !ok {
			// TODO: This is probably a race condition. Should we return nil error here?
			return false, policyError("%s event: failed to lookup container %s", id)
		}
		log.Info("finishing coldstart period for %s", c.PrettyName())
		return p.finishColdStart(c)
	}
	return false, nil
}

// DescribeMetrics generates policy-specific prometheus metrics data descriptors.
func (p *policy) DescribeMetrics() []*prometheus.Desc {
	return nil
}

// PollMetrics provides policy metrics for monitoring.
func (p *policy) PollMetrics() policyapi.Metrics {
	return nil
}

// CollectMetrics generates prometheus metrics from cached/polled policy-specific metrics data.
func (p *policy) CollectMetrics(policyapi.Metrics) ([]prometheus.Metric, error) {
	return nil, nil
}

// GetTopologyZones returns the policy/pool data for 'topology zone' CRDs.
func (p *policy) GetTopologyZones() []*policyapi.TopologyZone {
	zones := []*policyapi.TopologyZone{}

	for _, pool := range p.pools {
		zone := &policyapi.TopologyZone{
			Name:      pool.Name(),
			Type:      string(pool.Kind()),
			Resources: []*policyapi.ZoneResource{},
		}
		if !pool.IsRootNode() {
			zone.Parent = pool.Parent().Name()
		}

		total := pool.GetSupply().(*supply)
		free := pool.FreeSupply().(*supply)
		capacity := int64(total.mem[memoryAll])
		available := int64(free.mem[memoryAll] - free.ExtraMemoryReservation(memoryAll))

		memory := &policyapi.ZoneResource{
			Name:        policyapi.MemoryResource,
			Capacity:    *resource.NewQuantity(capacity, resource.DecimalSI),
			Allocatable: *resource.NewQuantity(capacity, resource.DecimalSI),
			Available:   *resource.NewQuantity(available, resource.DecimalSI),
		}
		zone.Resources = append(zone.Resources, memory)

		attributes := []*policyapi.ZoneAttribute{
			{
				Name:  policyapi.MemsetAttribute,
				Value: pool.GetMemset(memoryAll).String(),
			},
		}

		cpu := &policyapi.ZoneResource{
			Name: policyapi.CPUResource,
			Capacity: *resource.NewMilliQuantity(
				1000*int64(total.SharableCPUs().Union(total.ReservedCPUs()).Size()),
				resource.DecimalSI),
			Allocatable: *resource.NewMilliQuantity(
				1000*int64(total.SharableCPUs().Union(total.ReservedCPUs()).Size()),
				resource.DecimalSI),
			Available: *resource.NewMilliQuantity(int64(free.AllocatableSharedCPU()),
				resource.DecimalSI),
		}
		zone.Resources = append(zone.Resources, cpu)

		attributes = append(attributes, &policyapi.ZoneAttribute{
			Name:  policyapi.SharedCPUsAttribute,
			Value: free.SharableCPUs().String(),
		})
		if !total.ReservedCPUs().IsEmpty() {
			attributes = append(attributes, &policyapi.ZoneAttribute{
				Name:  policyapi.ReservedCPUsAttribute,
				Value: total.ReservedCPUs().String(),
			})
		}
		if !free.IsolatedCPUs().IsEmpty() {
			attributes = append(attributes, &policyapi.ZoneAttribute{
				Name:  policyapi.IsolatedCPUsAttribute,
				Value: total.IsolatedCPUs().String(),
			})
		}

		zone.Attributes = attributes

		zones = append(zones, zone)
	}

	return zones
}

// ExportResourceData provides resource data to export for the container.
func (p *policy) ExportResourceData(c cache.Container) map[string]string {
	grant, ok := p.allocations.grants[c.GetID()]
	if !ok {
		return nil
	}

	data := map[string]string{}
	shared := grant.SharedCPUs().String()
	isolated := grant.ExclusiveCPUs().Intersection(grant.GetCPUNode().GetSupply().IsolatedCPUs())
	exclusive := grant.ExclusiveCPUs().Difference(isolated).String()

	if grant.SharedPortion() > 0 && shared != "" {
		data[policyapi.ExportSharedCPUs] = shared
	}
	if isolated.String() != "" {
		data[policyapi.ExportIsolatedCPUs] = isolated.String()
	}
	if exclusive != "" {
		data[policyapi.ExportExclusiveCPUs] = exclusive
	}

	mems := grant.Memset()
	dram := idset.NewIDSet()
	pmem := idset.NewIDSet()
	hbm := idset.NewIDSet()
	for _, id := range mems.SortedMembers() {
		node := p.sys.Node(id)
		switch node.GetMemoryType() {
		case system.MemoryTypeDRAM:
			dram.Add(id)
		case system.MemoryTypePMEM:
			pmem.Add(id)
			/*
				case system.MemoryTypeHBM:
					hbm.Add(id)
			*/
		}
	}
	data["ALL_MEMS"] = mems.String()
	if dram.Size() > 0 {
		data["DRAM_MEMS"] = dram.String()
	}
	if pmem.Size() > 0 {
		data["PMEM_MEMS"] = pmem.String()
	}
	if hbm.Size() > 0 {
		data["HBM_MEMS"] = hbm.String()
	}

	return data
}

// reallocateResources reallocates the given containers using the given pool hints
func (p *policy) reallocateResources(containers []cache.Container, pools map[string]string) error {
	errs := []error{}

	log.Info("reallocating resources...")

	cache.SortContainers(containers)

	for _, c := range containers {
		p.releasePool(c)
	}
	for _, c := range containers {
		log.Debug("reallocating resources for %s...", c.PrettyName())

		grant, err := p.allocatePool(c, pools[c.GetID()])
		if err != nil {
			errs = append(errs, err)
		} else {
			p.applyGrant(grant)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return err
	}

	p.updateSharedAllocations(nil)

	p.root.Dump("<post-realloc>")

	return nil
}

func (p *policy) Reconfigure(newCfg interface{}) error {
	cfg, ok := newCfg.(*cfgapi.Config)
	if !ok {
		return policyError("got config of wrong type %T", newCfg)
	}

	log.Infof("updated configuration: %+v", cfg)

	savedPolicy := *p
	allocations := savedPolicy.allocations.clone()

	opt = cfg
	p.cfg = cfg

	if err := p.initialize(); err != nil {
		*p = savedPolicy
		return policyError("failed to reconfigure: %v", err)
	}

	for _, grant := range allocations.grants {
		if err := grant.RefetchNodes(); err != nil {
			*p = savedPolicy
			opt = p.cfg
			return policyError("failed to reconfigure: %v", err)
		}
	}

	log.Warn("updating existing allocations...")
	if err := p.restoreAllocations(&allocations); err != nil {
		*p = savedPolicy
		opt = p.cfg
		return policyError("failed to reconfigure: %v", err)
	}

	p.root.Dump("<post-config>")

	return nil
}

// Initialize or reinitialize the policy.
func (p *policy) initialize() error {
	p.nodes = nil
	p.pools = nil
	p.root = nil
	p.nodeCnt = 0
	p.depth = 0
	p.allocations = p.newAllocations()

	if err := p.checkConstraints(); err != nil {
		return err
	}

	if err := p.buildPoolsByTopology(); err != nil {
		return err
	}

	return nil
}

// Check the constraints passed to us.
func (p *policy) checkConstraints() error {
	amount, kind := p.cfg.AvailableResources.Get(cfgapi.CPU)
	switch kind {
	case cfgapi.AmountCPUSet:
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return fmt.Errorf("failed to parse available CPU cpuset '%s': %w", amount, err)
		}
		p.allowed = cset
	case cfgapi.AmountQuantity:
		return fmt.Errorf("can't handle CPU resources given as resource.Quantity (%v)", amount)
	case cfgapi.AmountAbsent:
		// default to all online cpus
		p.allowed = p.sys.CPUSet().Difference(p.sys.Offlined())
	}

	p.isolated = p.sys.Isolated().Intersection(p.allowed)

	amount, kind = p.cfg.ReservedResources.Get(cfgapi.CPU)
	switch kind {
	case cfgapi.AmountAbsent:
		return policyError("cannot start without CPU reservation")

	case cfgapi.AmountCPUSet:
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return fmt.Errorf("failed to parse reserved CPU cpuset '%s': %w", amount, err)
		}
		p.reserved = cset
		// check that all reserved CPUs are in the allowed set
		if !p.reserved.Difference(p.allowed).IsEmpty() {
			return policyError("invalid reserved cpuset %s, some CPUs (%s) are not "+
				"part of the online allowed cpuset (%s)", p.reserved,
				p.reserved.Difference(p.allowed), p.allowed)
		}
		// check that none of the reserved CPUs are isolated
		if !p.reserved.Intersection(p.isolated).IsEmpty() {
			return policyError("invalid reserved cpuset %s, some CPUs (%s) are also isolated",
				p.reserved.Intersection(p.isolated))
		}

	case cfgapi.AmountQuantity:
		qty, err := amount.ParseQuantity()
		if err != nil {
			return fmt.Errorf("failed to parse reserved CPU quantity '%s': %w", amount, err)
		}

		p.reserveCnt = (int(qty.MilliValue()) + 999) / 1000
		// Use CpuAllocator to pick reserved CPUs among
		// allowed ones. Because using those CPUs is allowed,
		// they remain (they are put back) in the allowed set.
		cset, err := p.cpuAllocator.AllocateCpus(&p.allowed, p.reserveCnt, cpuallocator.PriorityNormal)
		p.allowed = p.allowed.Union(cset)
		if err != nil {
			log.Fatal("cannot reserve %dm CPUs for ReservedResources from AvailableResources: %s", qty.MilliValue(), err)
		}
		p.reserved = cset
	}

	if p.reserved.IsEmpty() {
		return policyError("cannot start without CPU reservation")
	}

	return nil
}

func (p *policy) restoreCache() error {
	allocations := p.newAllocations()
	if p.cache.GetPolicyEntry(keyAllocations, &allocations) {
		if err := p.restoreAllocations(&allocations); err != nil {
			return policyError("failed to restore allocations from cache: %v", err)
		}
		p.allocations.Dump(log.Info, "restored ")
	}
	p.saveAllocations()

	return nil
}

func (p *policy) checkColdstartOff() {
	for _, id := range p.sys.NodeIDs() {
		node := p.sys.Node(id)
		if node.GetMemoryType() == system.MemoryTypePMEM {
			if !node.HasNormalMemory() {
				coldStartOff = true
				log.Error("coldstart forced off: NUMA node #%d does not have normal memory", id)
				return
			}
		}
	}
}

// newAllocations returns a new initialized empty set of allocations.
func (p *policy) newAllocations() allocations {
	return allocations{policy: p, grants: make(map[string]Grant)}
}

// clone creates a copy of the allocation.
func (a *allocations) clone() allocations {
	o := allocations{policy: a.policy, grants: make(map[string]Grant)}
	for id, grant := range a.grants {
		o.grants[id] = grant.Clone()
	}
	return o
}

// getContainerPoolHints creates container pool hints for the current grants.
func (a *allocations) getContainerPoolHints() ([]cache.Container, map[string]string) {
	containers := make([]cache.Container, 0, len(a.grants))
	hints := make(map[string]string)
	for _, grant := range a.grants {
		c := grant.GetContainer()
		containers = append(containers, c)
		hints[c.GetID()] = grant.GetCPUNode().Name()
	}
	return containers, hints
}
