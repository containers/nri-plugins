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
	"context"
	"errors"
	"fmt"

	"github.com/containers/nri-plugins/pkg/irq"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	resapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/cpuallocator"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
)

const (
	// PolicyName is the name of this policy.
	PolicyName = "topology-aware"
	// PolicyDescription is a short description of this policy.
	PolicyDescription = "A policy for prototyping memory tiering."

	// ColdStartDone is the event generated for the end of a container cold start period.
	ColdStartDone = "cold-start-done"

	// CPU class extended resource domain.
	CpuClassResourceDomain = "cpuclass.resource-policy.nri.io"
)

// allocations is our cache.Cachable for saving resource allocations in the cache.
type allocations struct {
	policy *policy          // policy back pointer
	grants map[string]Grant // container grants by container ID
	irqCnt int              // number of grant additions/deletions with IRQ affinity
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
	memAllocator *libmem.Allocator         // memory allocator user by the policy
	cpuClasses   *cpuclass.Handler         // CPU class handler (cpufreq, SST/PCT, etc.)
	metrics      *TopologyAwareMetrics     // metrics provided by this policy
	irqCnt       int                       // last applied [allocations.]irqCnt

	// claimContainerRefs counts, per DRA ResourceClaim UID, how many live
	// containers currently reference that claim's CPUs (allocateClaim
	// increments, releaseClaim decrements). The pool supply is marked via
	// Supply.ClaimCPUs on the first container and unmarked via
	// Supply.UnclaimCPUs only once the last referencing container is
	// released — this is what makes a multi-container ResourceClaim
	// (AllowMultipleAllocations) safe.
	claimContainerRefs map[types.UID]int

	// claimedCPUsByContainer is, per container ID, the union of every live
	// DRA claim's CPUs that container consumes (populated/cleared in
	// AllocateResources/ReleaseResources alongside allocateClaim/
	// releaseClaim, and repopulated by reapplyDRAClaims after a restart/
	// Reconfigure). applyGrant/updateSharedAllocations union this in before
	// pinning a container's cpuset, so a claim consumer's normal grant
	// (computed independently, without regard to its claimed CPUs) doesn't
	// end up excluding the CPUs its CDI-injected NRI_CPU<N> env vars claim
	// it has.
	claimedCPUsByContainer map[string]cpuset.CPUSet

	// draPlugin is the DRA kubelet plugin instance for this driver, or nil
	// when DRA is disabled (cfg.DRAEnabled() == false) or Setup() could not
	// build one (see buildDRAPlugin in dra.go: missing kube client or node
	// name at Setup() time — an empty cpuClasses configuration no longer
	// prevents construction; the plugin is built with an empty device set
	// instead). AllocateResources
	// and ReleaseResources nil-check this field before passing it anywhere a
	// claimLister is expected: a nil *dra.Plugin handed to an interface
	// parameter would produce a non-nil interface wrapping a nil pointer
	// (the typed-nil trap), so callers must guard on p.draPlugin != nil
	// themselves rather than relying on claimCPUsFromContainer's internal
	// nil check.
	draPlugin *dra.Plugin
	// draCtx is the context passed to draPlugin.Start()/PublishResources();
	// stored so PostReconfigure can re-call PublishResources with the same
	// context. nil when draPlugin is nil.
	draCtx context.Context
	// draCtxCancel cancels draCtx. Called from Stop() to shut the DRA
	// plugin's background goroutines down. nil when draPlugin is nil.
	draCtxCancel context.CancelFunc
	// cdiDir is the directory DRA CDI spec files are written to. Empty
	// means the dra package's default (/var/run/cdi). Overridable so tests
	// can inject a temporary directory.
	cdiDir string
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
	var err error

	irq.BlockWrites()
	defer irq.UnblockWrites()

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
	p.memAllocator, err = libmem.NewAllocator(libmem.WithSystemNodes(opts.System))
	if err != nil {
		return policyError("failed to initialize %s policy: %w", err)
	}

	opt = cfg
	defaultPrio = cfg.DefaultCPUPriority.Value()

	defer p.commitCpuClasses("setup")
	defer p.applyIrqAffinity("setup")

	if err := p.initialize(); err != nil {
		return policyError("failed to initialize %s policy: %w", PolicyName, err)
	}

	if err := p.registerImplicitAffinities(); err != nil {
		return policyError("failed to initialize %s policy: %w", PolicyName, err)
	}

	// Build the DRA plugin, if enabled, once at initial Setup() time. There
	// is no DRAEnabled-flip check here — see buildDRAPlugin's doc comment
	// (dra.go) for why that check belongs in Reconfigure() instead.
	if p.cfg.DRAEnabled() {
		if err := p.buildDRAPlugin(opts); err != nil {
			return policyError("failed to initialize %s policy: %w", PolicyName, err)
		}
	}

	log.Infof("***** default CPU priority is %s", defaultPrio)

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

	// Start the DRA plugin (if built by Setup()) before reapplyDRAClaims:
	// draPlugin.Start(ctx) loads the persisted ClaimStore, which
	// reapplyDRAClaims reads via LiveClaimsLocked() to re-mark pool
	// supplies. Start/PublishResources are not made while holding the
	// resmgr lock — they take their own internal WithLock only for the
	// specific sections that touch shared Handler/claim state. However,
	// once draPlugin.Start(ctx) returns, the kubelet plugin is registered
	// and may immediately start serving PrepareResourceClaims/
	// UnprepareResourceClaims RPCs in background goroutines; those mutate
	// p.claims under deps.WithLock (= the resmgr write lock). reapplyDRAClaims
	// (via LiveClaimsLocked) reads p.claims and therefore must also run under
	// that same lock to avoid a concurrent unsynchronized map access.
	if p.draPlugin != nil {
		ctx, cancel := context.WithCancel(context.Background())
		p.draCtx = ctx
		p.draCtxCancel = cancel
		if err := p.draPlugin.Start(ctx); err != nil {
			cancel()
			return policyError("failed to start DRA plugin: %v", err)
		}
		if err := p.draPlugin.PublishResources(ctx); err != nil {
			cancel()
			p.draPlugin.Stop()
			return policyError("failed to publish DRA resources: %v", err)
		}

		// Re-mark any DRA-claimed CPUs in the freshly rebuilt pool
		// supplies. Must run under the resmgr write lock (see above);
		// p.options.WithLock is the same closure the DRA plugin itself
		// uses (dra.Deps.WithLock), and reapplyDRAClaims/its helpers
		// (evictOverlappingGrants, remarkClaimInSupply, reallocateEvicted)
		// do not call WithLock themselves, so this cannot deadlock.
		p.options.WithLock(func() {
			p.reapplyDRAClaims()
		})
	}

	// Turn coldstart forcibly off if we have movable non-DRAM memory.
	// Note that although this can change dynamically we only check it
	// during startup and trust users to either not fiddle with memory
	// or restart us if they do.
	p.checkColdstartOff()

	p.root.Dump("<post-start>")
	p.checkAllocations("  <post-start>")

	m, err := p.NewTopologyAwareMetrics()
	if err != nil {
		log.Errorf("****** failed to create topology-aware metrics during Start(): %v", err)
		return err
	}
	p.metrics = m

	return nil
}

// Stop shuts down this policy: it cancels the DRA plugin's context and
// stops the DRA plugin (kubeletplugin registration, background helper
// goroutines), if one was built. A no-op when DRA is disabled
// (draPlugin == nil) — safe to call regardless of whether Start() ran.
func (p *policy) Stop() error {
	if p.draCtxCancel != nil {
		p.draCtxCancel()
	}
	if p.draPlugin != nil {
		p.draPlugin.Stop()
	}
	return nil
}

// Sync synchronizes the state of this policy.
func (p *policy) Sync(add []cache.Container, del []cache.Container) error {
	irq.BlockWrites()
	defer irq.UnblockWrites()

	log.Debugf("synchronizing state...")
	for _, c := range del {
		if err := p.ReleaseResources(c); err != nil {
			log.Warnf("failed to release resources for %s: %v", c.PrettyName(), err)
		}
	}
	for _, c := range add {
		if err := p.AllocateResources(c); err != nil {
			log.Warnf("failed to allocate resources for %s: %v", c.PrettyName(), err)
		}
	}

	p.checkAllocations("  <post-sync>")

	return nil
}

func (p *policy) checkAllocations(format string, args ...any) {
	var (
		prefix  = fmt.Sprintf(format, args...)
		cpuExcl = 0
		cpuPart = 0
		mem     = int64(0)
		ctr     = map[string]Grant{}
		dup     = map[string][]Grant{}
	)

	for _, g := range p.allocations.grants {
		log.Debugf("%s %s (%s)", prefix, g, g.GetContainer().GetID())
		full := g.ExclusiveCPUs().Size()
		part := g.CPUPortion()
		cpuExcl += full
		cpuPart += part

		mem += g.GetMemorySize()

		_, ok := p.cache.LookupContainer(g.GetContainer().GetID())
		if !ok {
			log.Errorf("%s   %s STALE container among allocations, not found in cache", prefix, g)
		}

		key := g.GetContainer().PrettyName()
		old, ok := ctr[key]
		if ok {
			if len(dup[key]) == 0 {
				dup[key] = []Grant{old, g}
			} else {
				dup[key] = append(dup[key], g)
			}
		} else {
			ctr[key] = g
		}
	}

	for key, grants := range dup {
		log.Errorf("%s DUPLICATE allocation entries for container %s", prefix, key)
		for _, g := range grants {
			log.Errorf("%s   %s (%s)", prefix, g, g.GetContainer().GetID())
		}
	}

	log.Infof("%s total CPU granted: %dm (%d exclusive + %dm shared), total memory granted: %s",
		prefix, 1000*cpuExcl+cpuPart, cpuExcl, cpuPart, prettyMem(mem))

}

// AllocateResources is a resource allocation request for this policy.
func (p *policy) AllocateResources(container cache.Container) error {
	irq.BlockWrites()
	defer irq.UnblockWrites()

	log.Debugf("allocating resources for %s (%s)...", container.PrettyName(), container.GetID())

	defer p.commitCpuClasses(container.PrettyName())
	defer p.applyIrqAffinity(container.PrettyName())
	defer p.triggerDRARepublish()

	var markedClaims []containerClaim
	if p.draPlugin != nil {
		for _, cl := range claimCPUsFromContainer(container, p.draPlugin) {
			if err := p.allocateClaim(cl.UID, cl.CPUs, cl.ClassCPUs); err != nil {
				p.rollbackClaimMarks(container, markedClaims)
				return policyError("failed to allocate resources for %s: %v",
					container.PrettyName(), err)
			}
			markedClaims = append(markedClaims, cl)
		}
		if len(markedClaims) > 0 {
			p.setClaimedCPUs(container, markedClaims)
		}
	}

	err := p.allocateResources(container, "")
	if err != nil {
		p.rollbackClaimMarks(container, markedClaims)
		return err
	}

	p.root.Dump("<post-alloc>")
	p.checkAllocations("  <post-alloc %s>", container.PrettyName())

	p.metrics.Update()

	return nil
}

// setClaimedCPUs unions every claim's CPUs in marked and records the result
// in p.claimedCPUsByContainer, keyed by container's ID. Used by
// AllocateResources after successfully marking every live claim a container
// carries, and by reapplyDRAClaims to repopulate the map after a restart/
// Reconfigure (see its own comment for why that repopulation alone isn't
// sufficient there).
func (p *policy) setClaimedCPUs(container cache.Container, marked []containerClaim) {
	union := cpuset.New()
	for _, cl := range marked {
		union = union.Union(cl.CPUs)
	}
	if p.claimedCPUsByContainer == nil {
		p.claimedCPUsByContainer = map[string]cpuset.CPUSet{}
	}
	p.claimedCPUsByContainer[container.GetID()] = union
}

// rollbackClaimMarks releases every claim mark accumulated so far in an
// AllocateResources call that failed partway through (whether from a later
// claim's own allocateClaim call or from the subsequent normal pool
// allocation), so a partial failure never leaks a claimContainerRefs entry
// (or a stale claimedCPUsByContainer union) for a container that ultimately
// never got its resources allocated.
func (p *policy) rollbackClaimMarks(container cache.Container, marked []containerClaim) {
	for _, cl := range marked {
		if err := p.releaseClaim(cl.UID, cl.CPUs); err != nil {
			log.Errorf("dra: rollback: failed to release claim %s: %v", cl.UID, err)
		}
	}
	if len(marked) > 0 {
		delete(p.claimedCPUsByContainer, container.GetID())
	}
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
	irq.BlockWrites()
	defer irq.UnblockWrites()

	log.Debugf("releasing resources for %s (%s)...", container.PrettyName(), container.GetID())

	defer p.commitCpuClasses(container.PrettyName())
	defer p.applyIrqAffinity(container.PrettyName())
	defer p.triggerDRARepublish()

	if p.draPlugin != nil {
		for _, cl := range claimCPUsFromContainer(container, p.draPlugin) {
			if err := p.releaseClaim(cl.UID, cl.CPUs); err != nil {
				log.Errorf("failed to release DRA claim for %s: %v", container.PrettyName(), err)
			}
		}
		delete(p.claimedCPUsByContainer, container.GetID())
	}

	if grant, found := p.releasePool(container); found {
		p.updateSharedAllocations(&grant)
	}

	p.root.Dump("<post-release>")
	p.checkAllocations("  <post-release %s>", container.PrettyName())

	p.metrics.Update()

	return nil
}

// UpdateResources is a resource allocation update request for this policy.
func (p *policy) UpdateResources(container cache.Container) error {
	irq.BlockWrites()
	defer irq.UnblockWrites()

	log.Debugf("updating (reallocating) container %s...", container.PrettyName())

	defer p.commitCpuClasses(container.PrettyName())
	defer p.applyIrqAffinity(container.PrettyName())

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
	p.checkAllocations("  <post-update %s>", container.PrettyName())

	p.metrics.Update()

	return nil
}

// HandleEvent handles policy-specific events.
func (p *policy) HandleEvent(e *events.Policy) (bool, error) {
	log.Debugf("received policy event %s.%s with data %v...", e.Source, e.Type, e.Data)

	switch e.Type {
	case events.ContainerStarted:
		c, ok := e.Data.(cache.Container)
		if !ok {
			return false, policyError("%s event: expecting cache.Container Data, got %T",
				e.Type, e.Data)
		}
		log.Infof("triggering coldstart period (if necessary) for %s", c.PrettyName())
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
		log.Infof("finishing coldstart period for %s", c.PrettyName())
		return p.finishColdStart(c)
	}
	return false, nil
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

		memZone := libmem.NewNodeMask(pool.GetMemset(memoryAll).Members()...)
		capacity := p.memAllocator.ZoneCapacity(memZone)
		available := p.memAllocator.ZoneFree(memZone)

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

// GetExtendedResources returns the node-level extended resources
// to publish for this policy.
func (p *policy) GetExtendedResources() map[string]*resource.Quantity {
	// Own the entire domain unconditionally: any key under it
	// that we do not explicitly publish below must be removed.
	out := map[string]*resource.Quantity{
		CpuClassResourceDomain + "/*": nil,
	}
	// Experimental cpuClass.PublishExtendedResource publishes only PCT capacity.
	if p.cpuClasses == nil || !p.cpuClasses.PctActive() || p.cfg == nil {
		return out
	}
	for _, cc := range p.cfg.CPUClasses {
		if cc == nil || !cc.PublishExtendedResource {
			continue
		}
		if cc.PctPriority == "" && cc.SstClosID == nil {
			log.Warnf("ignoring publishExtendedResource on non-PCT cpuClass %q", cc.Name)
			continue
		}
		free := max(p.cpuClasses.PctFreeClassCapacity(cc.Name, cpuset.New()), 0)
		out[CpuClassResourceDomain+"/"+cc.Name] = resource.NewQuantity(int64(free), resource.DecimalSI)
	}
	return out
}

// ExportResourceData provides resource data to export for the container.
func (p *policy) ExportResourceData(c cache.Container) map[string]string {
	grant, ok := p.allocations.getGrant(c.GetID())
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

	mems := grant.GetMemoryZone()
	dram := mems.And(p.memAllocator.Masks().NodesByTypes(libmem.TypeMaskDRAM))
	pmem := mems.And(p.memAllocator.Masks().NodesByTypes(libmem.TypeMaskPMEM))
	hbm := mems.And(p.memAllocator.Masks().NodesByTypes(libmem.TypeMaskHBM))
	data["ALL_MEMS"] = mems.MemsetString()
	if dram.Size() > 0 {
		data["DRAM_MEMS"] = dram.MemsetString()
	}
	if pmem.Size() > 0 {
		data["PMEM_MEMS"] = pmem.MemsetString()
	}
	if hbm.Size() > 0 {
		data["HBM_MEMS"] = hbm.MemsetString()
	}

	return data
}

// reallocateResources reallocates the given containers using the given pool hints
func (p *policy) reallocateResources(containers []cache.Container, pools map[string]string) error {
	errs := []error{}

	log.Infof("reallocating resources...")

	cache.SortContainers(containers)

	for _, c := range containers {
		p.releasePool(c)
	}
	for _, c := range containers {
		log.Debugf("reallocating resources for %s (%s)...", c.PrettyName(), c.GetID())

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

	return nil
}

func (p *policy) Reconfigure(newCfg any) error {
	irq.BlockWrites()
	defer irq.UnblockWrites()

	cfg, ok := newCfg.(*cfgapi.Config)
	if !ok {
		return policyError("got config of wrong type %T", newCfg)
	}

	log.Infof("updated configuration: %+v", cfg)

	savedPolicy := *p
	allocations := savedPolicy.allocations.clone()

	// DRA enable/disable flip: buildDRAPlugin only ever runs once, from the
	// initial Setup() (see its doc comment in dra.go for why this check
	// cannot live there) — Reconfigure() never tears down or (re)builds
	// p.draPlugin. A config change that flips cfg.DRAEnabled() would
	// desync p.draPlugin from the new config's intent (e.g. "disabled" in
	// the new config but the plugin keeps running, or "enabled" but no
	// plugin ever gets built), so it is refused outright, independent of
	// any live claims.
	if cfg.DRAEnabled() != p.cfg.DRAEnabled() {
		return policyError("failed to reconfigure: cannot change dra.enabled (%v -> %v) without a restart",
			p.cfg.DRAEnabled(), cfg.DRAEnabled())
	}

	// Validate the new config's DRA-published cpuClasses *before* committing
	// anything below. Without this, a tier-conflicting reconfigure would
	// commit p.cfg here and only fail later, inside PublishResources
	// (called from PostReconfigure, after the caller's lock is released) —
	// by which point the bad config is already live and the old
	// ResourceSlices are left stale/advertised instead of updated.
	if cfg.DRAEnabled() {
		if err := cpuclass.ValidateCPUClassesForDRA(cfg.CPUClasses, cfg.DRASharedCounters()); err != nil {
			return policyError("failed to reconfigure: %v", err)
		}
	}

	// Snapshot the DRA-visible device attributes of every currently
	// configured cpuClass *before* initialize() discards p.cpuClasses (it
	// sets p.cpuClasses = nil, then builds a fresh Handler from the new
	// config). Compared below (once the new Handler is built) against an
	// equivalent post-initialize() snapshot to detect a cpuClass attribute
	// change that would invalidate any DRA claim already backed by that
	// class (resolved decision 8 / Option B: refuse, don't silently
	// reshape live claims).
	var oldDRADevices []resapi.Device
	if p.draPlugin != nil && p.cpuClasses != nil {
		oldDRADevices, _ = p.cpuClasses.DRADevicesAtMaxCapacity(DRADriverName)
	}

	opt = cfg
	p.cfg = cfg
	defaultPrio = cfg.DefaultCPUPriority.Value()

	defer p.commitCpuClasses("reconfigure")
	defer p.applyIrqAffinity("reconfigure")

	if err := p.initialize(); err != nil {
		*p = savedPolicy
		opt = p.cfg
		defaultPrio = p.cfg.DefaultCPUPriority.Value()
		return policyError("failed to reconfigure: %v", err)
	}

	if p.draPlugin != nil {
		var newDRADevices []resapi.Device
		if p.cpuClasses != nil {
			newDRADevices, _ = p.cpuClasses.DRADevicesAtMaxCapacity(DRADriverName)
		}
		liveClasses := p.draPlugin.LiveClaimClasses()
		for _, class := range changedDRAClasses(oldDRADevices, newDRADevices) {
			if live := liveClasses[class]; live > 0 {
				*p = savedPolicy
				opt = p.cfg
				defaultPrio = p.cfg.DefaultCPUPriority.Value()
				p.resyncCpuClassesAfterRefusedReconfigure()
				return policyError("failed to reconfigure: cpuClass %q has %d live DRA claim(s); "+
					"reconfiguring its DRA-visible attributes would invalidate them", class, live)
			}
		}
	}

	if err := p.registerImplicitAffinities(); err != nil {
		return policyError("failed to reconfigure: %v", err)
	}

	for _, grant := range allocations.grants {
		if err := grant.RefetchNodes(); err != nil {
			*p = savedPolicy
			opt = p.cfg
			defaultPrio = p.cfg.DefaultCPUPriority.Value()
			return policyError("failed to reconfigure: %v", err)
		}
	}

	log.Warnf("updating existing allocations...")
	if err := p.restoreAllocations(&allocations); err != nil {
		*p = savedPolicy
		opt = p.cfg
		defaultPrio = p.cfg.DefaultCPUPriority.Value()
		return policyError("failed to reconfigure: %v", err)
	}

	// Commit path (the refusal checks above did not fire): rebuild the DRA
	// plugin's own in-memory HP-CPU accounting (RestoreClaimsLocked) and
	// re-mark claimed CPUs in the freshly rebuilt pool supplies
	// (reapplyDRAClaims). Both calls are still inside the resmgr write lock
	// held by the caller — neither does I/O or calls WithLock, so both are
	// safe to run here. draPlugin.PublishResources (I/O) is deferred to
	// PostReconfigure(), which runs after the caller releases the lock.
	if p.draPlugin != nil {
		if err := p.draPlugin.RestoreClaimsLocked(); err != nil {
			log.Errorf("dra: Reconfigure: RestoreClaimsLocked: %v", err)
		}
	}
	p.reapplyDRAClaims()

	p.root.Dump("<post-config>")
	p.checkAllocations("  <post-config>")

	m, err := p.NewTopologyAwareMetrics()
	if err != nil {
		log.Errorf("****** failed to create topology-aware metrics during reconfigure: %v", err)
		return err
	}
	p.metrics = m

	return nil
}

// resyncCpuClassesAfterRefusedReconfigure repairs the physical side effect
// of an aborted Reconfigure() attempt that reaches the DRA live-claim
// refusal check: initialize() already wrote a physical CLOS association
// (p.resetCpuClass -> cpuClasses.UseClass -> pct.UseClass -> associate ->
// sst.AssociateCPUs, synchronous, not queued for the deferred Commit()) via
// a separate, now-discarded *cpuclass.Handler before *p = savedPolicy ran.
// *p = savedPolicy only restores Go-level bookkeeping on the *old*,
// restored Handler — that handler's own cpuClass/dirtyCPUs maps were never
// touched by the aborted attempt, so nothing below looks "changed" to it
// without first resetting its diff baseline.
//
// Must be called right after *p = savedPolicy (and the opt/p.cfg/
// defaultPrio restoration) — mirrors the normal successful continuation
// path's own order (initialize() -> restoreAllocations() ->
// RestoreClaimsLocked() -> reapplyDRAClaims()).
func (p *policy) resyncCpuClassesAfterRefusedReconfigure() {
	if p.cpuClasses != nil {
		// Re-run Configure on the old, already-configured handler with its
		// own (now-current-again) spec. This resets h.cpuClass/h.dirtyCPUs
		// to empty maps (letting the deferred Commit() below correctly
		// re-flush governor/idle/uncore state that would otherwise look
		// unchanged) and forces a fresh pct.Configure -> PrepareManagedMode
		// pass, which also resets hpUsed/hpDRAUsed HP-CPU accounting --
		// repaired below by RestoreClaimsLocked.
		if err := p.cpuClasses.Configure(cpuclass.ConfigSpec{
			Classes:     p.cfg.CPUClasses,
			TurboDomain: "package",
			Allowed:     p.allowed,
		}); err != nil {
			// Hardware may be left mid-CPReset; log and still proceed
			// best-effort below rather than let a secondary failure here
			// mask the primary refusal error.
			log.Errorf("dra: reconfigure refusal: failed to re-synchronize CPU class handler: %v", err)
		}

		p.resetCpuClass("reconfigure-refused", p.allowed)
		p.setReservedPoolCpuClass()

		if opt.PinCPU {
			for _, grant := range p.allocations.grants {
				if class := grant.CPUClass(); class != "" {
					if err := p.cpuClasses.UseClass(class, grant.ExclusiveCPUs()); err != nil {
						log.Errorf("dra: reconfigure refusal: failed to reapply class %q to grant %s: %v",
							class, grant, err)
					}
				}
			}
		}
	}

	if err := p.draPlugin.RestoreClaimsLocked(); err != nil {
		log.Errorf("dra: reconfigure refusal: RestoreClaimsLocked: %v", err)
	}
	p.reapplyDRAClaims()
}

// Initialize or reinitialize the policy.
func (p *policy) initialize() error {
	p.nodes = nil
	p.pools = nil
	p.root = nil
	p.nodeCnt = 0
	p.depth = 0
	p.allocations = p.newAllocations()
	p.cpuClasses = nil

	if err := p.checkConstraints(); err != nil {
		return err
	}

	if err := p.buildPoolsByTopology(); err != nil {
		return err
	}

	opt.UnlimitedBurstable = p.findExistingTopologyLevel(opt.UnlimitedBurstable)

	if len(opt.CPUClasses) > 0 {
		cc, err := cpuclass.New(p.options.System)
		if err != nil {
			return policyError("failed to create CPU class handler: %w", err)
		}
		p.cpuClasses = cc

		if err := p.cpuClasses.Configure(cpuclass.ConfigSpec{
			Classes:     opt.CPUClasses,
			TurboDomain: "package",
			Allowed:     p.allowed,
		}); err != nil {
			return policyError("failed to configure CPU class handler: %w", err)
		}

		if err := p.validateCpuClasses(); err != nil {
			return policyError("failed to validate CPU classes: %w", err)
		}

		p.resetCpuClass("initialize", p.allowed)
		p.setReservedPoolCpuClass()
	}

	if _, err := irq.SetAllowedInterrupts(opt.ControllableInterrupts); err != nil {
		return policyError("failed to set controllable interrupts: %w", err)
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

	case cfgapi.AmountExcludeCPUSet:
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return fmt.Errorf("failed to parse available CPU cpuset '%s': %w", amount, err)
		}
		p.allowed = p.sys.CPUSet().Difference(cset)

	case cfgapi.AmountQuantity:
		return fmt.Errorf("can't handle CPU resources given as resource.Quantity (%v)", amount)
	case cfgapi.AmountAbsent:
		// Available CPUs not specified, default to system CPUs.
		p.allowed = p.sys.CPUSet()
	}
	// Allocation of only online CPUs is allowed.
	p.allowed = p.allowed.Intersection(p.sys.OnlineCPUs())

	p.isolated = p.sys.Isolated().Intersection(p.allowed)

	amount, kind = p.cfg.ReservedResources.Get(cfgapi.CPU)
	switch kind {
	case cfgapi.AmountAbsent:
		return policyError("cannot start without CPU reservation")

	case cfgapi.AmountCPUSet, cfgapi.AmountExcludeCPUSet:
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return fmt.Errorf("failed to parse reserved CPU cpuset '%s': %w", amount, err)
		}
		if kind == cfgapi.AmountExcludeCPUSet {
			p.reserved = p.allowed.Difference(cset)
		} else {
			p.reserved = cset
		}

		// check that all reserved CPUs are in the allowed set
		if !p.reserved.Difference(p.allowed).IsEmpty() {
			return policyError("invalid reserved cpuset %s, some CPUs (%s) are not "+
				"part of the online allowed cpuset (%s)", p.reserved,
				p.reserved.Difference(p.allowed), p.allowed)
		}
		// check that if any reserved CPUs are isolated, it is the sole reserved CPU
		if isolated := p.reserved.Intersection(p.isolated); !isolated.IsEmpty() {
			if !p.reserved.Equals(isolated) {
				return policyError("invalid reserved cpuset %s, mixes isolated (%s) and normal (%s)",
					p.reserved, isolated, p.reserved.Difference(isolated))
			}
			if isolated.Size() > 1 {
				return policyError("invalid reserved cpuset %s, multiple isolated CPUs (%s)",
					p.reserved, isolated)
			}
			log.Warnf("reserved CPU %s is isolated", p.reserved)
		}

	case cfgapi.AmountQuantity:
		qty, err := amount.ParseQuantity()
		if err != nil {
			return fmt.Errorf("failed to parse reserved CPU quantity '%s': %w", amount, err)
		}

		p.reserveCnt = (int(qty.MilliValue()) + 999) / 1000
		// Use CpuAllocator to pick reserved CPUs from the allowed ones but
		// avoiding isolated CPUs. The picked CPUs are not removed from the
		// allowed set.
		from := p.allowed.Difference(p.isolated)
		cset, err := p.cpuAllocator.AllocateCpus(&from, p.reserveCnt, normalPrio.Option())
		if err != nil {
			return policyError("cannot reserve %dm CPUs for ReservedResources from AvailableResources: %s", qty.MilliValue(), err)
		}
		p.reserved = cset
	}

	if p.reserved.IsEmpty() {
		return policyError("cannot start without CPU reservation")
	}

	log.Infof("using reserved cpuset %s", p.reserved)

	return nil
}

func (p *policy) findExistingTopologyLevel(level cfgapi.CPUTopologyLevel) cfgapi.CPUTopologyLevel {
	for _, pool := range p.pools {
		l := pool.Kind().TopologyLevel()
		if l.Value() == level.Value() {
			return l
		}
		if l.Value() < level.Value() {
			log.Warnf("no pool of kind %q (%q), using %q instead",
				level, NodeKindForTopologyLevel(level), l)
			return l
		}
	}

	return cfgapi.CPUTopologyLevelPackage
}

func (p *policy) restoreCache() error {
	allocations := p.newAllocations()
	if p.cache.GetPolicyEntry(keyAllocations, &allocations) {
		if err := p.restoreAllocations(&allocations); err != nil {
			return policyError("failed to restore allocations from cache: %v", err)
		}
		p.allocations.Dump(log.Infof, "restored ")
	}
	p.saveAllocations()

	return nil
}

// remarkClaimInSupply marks cpus as claimed by DRA ResourceClaim uid in the
// tightest pool that fully contains them (tree-wide, via
// Supply.ClaimCPUs — see resources.go), without touching
// claimContainerRefs. This is the marking-only counterpart to allocateClaim
// (pools.go): reapplyDRAClaims uses it to restore Supply.claimRefs after
// Start()/Reconfigure() rebuild pool/supply state in initialize(), which
// discards any marks a prior allocateClaim call applied.
//
// Using allocateClaim here instead of this marking-only path would
// double-count in the Reconfigure() case: p.claimContainerRefs is an
// in-process map that Reconfigure() never resets, so containers backing a
// live claim are already reflected in it from the AllocateResources call
// that admitted them.
//
// That reasoning does NOT hold across a process restart: claimContainerRefs
// is a plain in-memory map with no persistence, so it is zero-valued right
// after Start(), even though containers backed by live claims are already
// running. The correct refcount is rebuilt indirectly: pkg/resmgr/nri.go's
// syncWithNRI/Synchronize forces every already-running container through
// ReleaseResources (a no-op here, since the refcount is already 0) followed
// by AllocateResources (which calls allocateClaim and increments the
// refcount) as part of the NRI resync that always follows agent Start().
// reapplyDRAClaims only has to fix up Supply.claimRefs (pool CPU exclusion)
// for the window between Start() and that resync; claimContainerRefs catches
// up once the resync runs. If that syncWithNRI invariant ever changes (e.g.
// running containers stop being included in both the "allocated" and
// "released" lists), releaseClaim will silently no-op on the eventual real
// ReleaseResources (refcount already 0) and the claimed CPUs will leak out
// of pool capacity permanently, until the next restart.
//
// Also re-applies the physical cpuClass (className) to cpus: initialize()
// (called by both Start() and Reconfigure() before this runs) resets every
// allowed CPU's class back to the shared-pool default
// (resetCpuClass("initialize", p.allowed)), which would otherwise silently
// strip the SST-CP/EPP/governor settings a live DRA claim depends on.
func (p *policy) remarkClaimInSupply(uid types.UID, cpus cpuset.CPUSet, classCPUs map[string]cpuset.CPUSet) error {
	if cpus.IsEmpty() {
		return policyError("cannot remark DRA claim %s: empty CPU set", uid)
	}

	pool, err := p.poolForCPUs(cpus)
	if err != nil {
		return policyError("cannot remark DRA claim %s (CPUs %s): %v", uid, cpus, err)
	}

	pool.FreeSupply().ClaimCPUs(uid, cpus)

	// classCPUs groups cpus by cpuClass (see classifyClaimCPUs): applied per
	// subset so a claim spanning more than one class re-applies each class
	// only to the CPUs that actually belong to it. Best-effort on the
	// restart/reconfigure reapply path: log but do not fail the remark.
	if err := p.applyClassCPUs("re-apply", uid, classCPUs); err != nil {
		log.Errorf("dra: %v", err)
	}

	return nil
}

// reapplyDRAClaims re-marks pool supplies for every currently live DRA
// claim, restoring the Supply.claimRefs bookkeeping that Start()'s
// restoreCache() and Reconfigure()'s restoreAllocations() lose whenever
// initialize() rebuilds the pool/supply tree from scratch. It is a no-op if
// DRA is disabled (draPlugin == nil).
//
// reapplyDRAClaims runs *after* restoreCache()/restoreAllocations() have
// already reinstated grants (see Start()/Reconfigure()), at a point where
// Supply.claimRefs has just been wiped by initialize() and not yet re-marked.
// reinstateGrants/reallocateResources are therefore unaware of live DRA
// claims while they run: if grant restoration (verbatim reinstatement, or
// its allocatePool-based fallback) happens to hand a regular container CPUs
// that a live claim already owns, that overlap is a real double-booking —
// two workloads pinned to the same physical CPUs. Before marking each
// claim's CPUs here, evict and requeue for reallocation any restored grant
// that overlaps them, exactly like allocateClaim's first-time eviction path
// (evictOverlappingGrants/reallocateEvicted, pools.go) — this does not touch
// claimContainerRefs, so it stays consistent with remarkClaimInSupply's
// marking-only contract.
//
// Caller must hold the resmgr write lock (LiveClaimsLocked's contract).
// Reconfigure() already runs under the caller's lock, so it calls this
// directly. Start() runs unlocked (see its comment), so it must establish
// the lock itself via p.options.WithLock(func() { p.reapplyDRAClaims() }) —
// do not call reapplyDRAClaims from inside a callback that is nested inside
// an *already held* WithLock/resmgr-lock scope, since the lock is not
// reentrant and a second acquisition would deadlock.
func (p *policy) reapplyDRAClaims() {
	if p.draPlugin == nil {
		return
	}

	remarked := false
	for uid, allocs := range p.draPlugin.LiveClaimsLocked() {
		cpus, classCPUs := classifyClaimCPUs(uid, allocs)

		if cpus.IsEmpty() {
			continue
		}

		evicted, evictedCpusets := p.evictOverlappingGrants(cpus, fmt.Sprintf("reapplyDRAClaims: claim %s", uid))

		if err := p.remarkClaimInSupply(uid, cpus, classCPUs); err != nil {
			log.Errorf("dra: reapplyDRAClaims: %v", err)
			if reallocErr := p.reallocateEvicted(evicted, evictedCpusets, cpuset.New(), uid); reallocErr != nil {
				log.Errorf("dra: reapplyDRAClaims: failed to restore grants after claim %s could not be remarked: %v", uid, reallocErr)
			}
			continue
		}
		remarked = true

		if err := p.reallocateEvicted(evicted, evictedCpusets, cpus, uid); err != nil {
			log.Errorf("dra: reapplyDRAClaims: claim %s: evicted %d restored grant(s) overlapping "+
				"claimed CPUs %s but failed to fully reallocate them: %v", uid, len(evicted), cpus, err)
		}
	}

	// claimedCPUsByContainer is a plain in-memory map with no persistence,
	// so it is empty here even though pool accounting above is now
	// correct -- same restart-window gap remarkClaimInSupply's own comment
	// documents for claimContainerRefs. Unlike that refcount, though,
	// waiting for the next syncWithNRI resync (Release+AllocateResources)
	// isn't good enough on its own: restoreCache()'s/Reconfigure()'s own
	// restoreAllocations() call already ran applyGrant for these
	// containers *before* reapplyDRAClaims was ever reached, without the
	// union -- their cpusets need an explicit re-pin here, now, not just
	// the map. Re-resolve every live claim's consuming container(s) and
	// repopulate the map, then re-pin unconditionally (not gated on
	// remarked/updateSharedAllocations's shared-portion check below, which
	// skips exactly the common plain-exclusive-CPU claim consumer case).
	for _, c := range p.cache.GetContainers() {
		claims := claimCPUsFromContainer(c, p.draPlugin)
		if len(claims) == 0 {
			continue
		}
		p.setClaimedCPUs(c, claims)

		if grant, ok := p.allocations.getGrant(c.GetID()); ok {
			p.applyGrant(grant)
		} else if opt.PinCPU {
			union := p.claimedCPUsByContainer[c.GetID()]
			p.setPreferredCpusetCpus(c, cpuset.New(), union,
				fmt.Sprintf("  => re-pinning %s to claimed cpuset %s (no regular grant)", c.PrettyName(), union))
		}
	}

	// Re-marking may have subtracted CPUs from one or more pools' sharable
	// capacity; any container already pinned (via applyGrant, from before
	// the rebuild) to the previous, wider sharable cpuset must be re-pinned
	// to the now-reduced set so it cannot keep running on CPUs a live DRA
	// claim exclusively owns.
	if remarked {
		p.updateSharedAllocations(nil)
	}
}

func (p *policy) checkColdstartOff() {
	for _, id := range p.sys.NodeIDs() {
		node := p.sys.Node(id)
		if node.GetMemoryType() == system.MemoryTypePMEM {
			if !node.HasNormalMemory() {
				coldStartOff = true
				log.Errorf("coldstart forced off: NUMA node #%d does not have normal memory", id)
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
	o := allocations{policy: a.policy, grants: make(map[string]Grant), irqCnt: a.irqCnt}
	for id, grant := range a.grants {
		o.grants[id] = grant.Clone()
	}
	return o
}

func (a *allocations) addGrant(g Grant) {
	a.grants[g.GetContainer().GetID()] = g
	if g.IrqAffinity() != nil {
		a.irqCnt++
	}
}

func (a *allocations) getGrant(ctrID string) (Grant, bool) {
	g, ok := a.grants[ctrID]
	return g, ok
}

func (a *allocations) delGrant(ctrID string) (Grant, bool) {
	g, ok := a.grants[ctrID]
	if ok {
		delete(a.grants, ctrID)
		if g.IrqAffinity() != nil {
			a.irqCnt++
		}
	}
	return g, ok
}

func (a *allocations) irqState() int {
	return a.irqCnt
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
