// Copyright 2022 Intel Corporation. All Rights Reserved.
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

package balloons

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"
	"github.com/containers/nri-plugins/pkg/cpuallocator"
	"github.com/containers/nri-plugins/pkg/kubernetes"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	cpucontrol "github.com/containers/nri-plugins/pkg/resmgr/control/cpu"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	policy "github.com/containers/nri-plugins/pkg/resmgr/policy"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/utils"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// PolicyName is the name of this policy.
	PolicyName = "balloons"
	// PolicyDescription is a short description of this policy.
	PolicyDescription = "Flexible pools with per-pool CPU parameters"
	// balloonKey is a pod annotation key, the value is a pod balloon name.
	balloonKey = "balloon." + PolicyName + "." + kubernetes.ResmgrKeyNamespace
	// hideHyperthreadsKey is a pod annotation key for pod/container-specific hyperthread allowance.
	hideHyperthreadsKey = "hide-hyperthreads." + kubernetes.ResmgrKeyNamespace
	// reservedBalloonDefName is the name in the reserved balloon definition.
	reservedBalloonDefName = "reserved"
	// defaultBalloonDefName is the name in the default balloon definition.
	defaultBalloonDefName = "default"
	// NoLimit value denotes no limit being set.
	NoLimit = 0
	// virtDevReservedCpus is the name of a virtual device close to
	// CPUs that are configured as ReservedResources.
	virtDevReservedCpus = "reserved CPUs"
	// virtDevIsolatedCpus is the name of a virtual device close to
	// host isolated CPUs.
	virtDevIsolatedCpus = "isolated CPUs"
	// virtDevECores is the name of a virtual device close to
	// power efficient cores.
	virtDevECores = "efficient cores"
	// virtDevPCores is the name of a virtual device close to
	// high performance cores.
	virtDevPCores = "performance cores"
)

// balloons contains configuration and runtime attributes of the balloons policy
type balloons struct {
	options   *policy.BackendOptions // configuration common to all policies
	bpoptions *BalloonsOptions       // balloons-specific configuration
	cch       cache.Cache            // nri-resource-policy cache
	allowed   cpuset.CPUSet          // bounding set of CPUs we're allowed to use
	reserved  cpuset.CPUSet          // system-/kube-reserved CPUs
	freeCpus  cpuset.CPUSet          // CPUs to be included in growing or new ballons
	ifreeCpus cpuset.CPUSet          // initially free CPUs before assigning any containers
	cpuTree   *cpuTreeNode           // system CPU topology

	reservedBalloonDef *BalloonDef // reserved balloon definition, pointer to bpoptions.BalloonDefs[x]
	defaultBalloonDef  *BalloonDef // default balloon definition, pointer to bpoptions.BalloonDefs[y]
	balloons           []*Balloon  // balloon instances: reserved, default and user-defined

	cpuAllocator cpuallocator.CPUAllocator    // CPU allocator used by the policy
	memAllocator *libmem.Allocator            // memory allocator used by the policy
	loadVirtDev  map[string]*loadClassVirtDev // map LoadClasses to virtual devices
}

// Balloon contains attributes of a balloon instance
type Balloon struct {
	// Def is the definition from which this balloon instance is created.
	Def *BalloonDef
	// Instance is the index of this balloon instance, starting from
	// zero for every balloon definition.
	Instance int
	// Cpus is the set of CPUs exclusive to this balloon instance only.
	Cpus cpuset.CPUSet
	// Mems is the set of memory nodes with minimal access delay
	// from CPUs.
	Mems idset.IDSet
	// SharedIdleCpus is the set of idle CPUs that workloads in a
	// balloon are allowed to use with workloads in other balloons
	// that shareIdleCpus.
	SharedIdleCpus cpuset.CPUSet
	// PodIDs maps pod ID to list of container IDs.
	// - len(PodIDs) is the number of pods in the balloon.
	// - len(PodIDs[podID]) is the number of containers of podID
	//   currently assigned to the balloon.
	PodIDs map[string][]string
	// Groups is a multiset (group-by-value -> appearance-count)
	// of evaluated GroupBy expressions on containers in the balloon.
	Groups map[string]int
	// LoadedVirtDevs is a set of virtual devices under load due
	// to this balloon.
	LoadedVirtDevs map[string]struct{}
	cpuTreeAlloc   *cpuTreeAllocator
	memTypeMask    libmem.TypeMask
	components     []*Balloon
}

// loadClassVirtDev is a virtual device under load due to a load class.
type loadClassVirtDev struct {
	// name is the name of the virtual device.
	name string
	// level specifies CPUs affected by load on the virtual device.
	level CPUTopologyLevel
	// updateOnEveryCpuAllocation specifies if affected CPUs must be
	// updated whenever a new CPU is allocated to a balloon with this
	// virtual device.
	updateOnEveryCpuAllocation bool
}

var log logger.Logger = logger.NewLogger("policy")

// String is a stringer for a balloon.
func (bln Balloon) String() string {
	return fmt.Sprintf("%s{cpus:%q, mems:%q}", bln.PrettyName(), bln.Cpus, bln.Mems)
}

// PrettyName returns a unique name for a balloon.
func (bln Balloon) PrettyName() string {
	return fmt.Sprintf("%s[%d]", bln.Def.Name, bln.Instance)
}

// ContainerIDs returns IDs of containers assigned in a balloon.
// (Using cache.Container.GetID()'s)
func (bln Balloon) ContainerIDs() []string {
	cIDs := []string{}
	for _, ctrIDs := range bln.PodIDs {
		cIDs = append(cIDs, ctrIDs...)
	}
	return cIDs
}

// ContainerCount returns the number of containers in a balloon.
func (bln Balloon) ContainerCount() int {
	count := 0
	for _, ctrIDs := range bln.PodIDs {
		count += len(ctrIDs)
	}
	return count
}

func (bln Balloon) AvailMilliCpus() int {
	return bln.Cpus.Size() * 1000
}

func (bln Balloon) MaxAvailMilliCpus(freeCpus cpuset.CPUSet) int {
	availableFreeCpus := freeCpus.Size()
	if len(bln.components) > 0 {
		// MaxCpus of component balloons can limit the size of
		// the composite balloon.
		compMinAvailmCPUs := -1
		for _, comp := range bln.components {
			mcpus := comp.MaxAvailMilliCpus(freeCpus)
			if mcpus < compMinAvailmCPUs || compMinAvailmCPUs == -1 {
				compMinAvailmCPUs = mcpus
			}
		}
		// Assume this composite balloon allocates equal
		// number of CPUs for every component balloon.
		sumMinAvailCPUs := (compMinAvailmCPUs * len(bln.components)) / 1000
		availableFreeCpus = min(sumMinAvailCPUs, availableFreeCpus)
	}
	if bln.Def.MaxCpus == NoLimit || bln.Def.MaxCpus > availableFreeCpus {
		return (bln.Cpus.Size() + availableFreeCpus) * 1000
	}
	return bln.Def.MaxCpus * 1000
}

// New creates a new uninitialized balloons policy instance.
func New() policy.Backend {
	return &balloons{}
}

// Setup initializes the balloons policy instance.
func (p *balloons) Setup(policyOptions *policy.BackendOptions) error {
	var err error

	bpoptions, ok := policyOptions.Config.(*BalloonsOptions)
	if !ok {
		return balloonsError("failed to initialize %s policy: config of wrong type %T",
			PolicyName, policyOptions.Config)
	}
	bpoptions = bpoptions.DeepCopy()

	p.options = policyOptions
	p.cch = policyOptions.Cache
	p.cpuAllocator = cpuallocator.NewCPUAllocator(policyOptions.System)

	malloc, err := libmem.NewAllocator(libmem.WithSystemNodes(policyOptions.System))
	if err != nil {
		return balloonsError("failed to create memory allocator: %w", err)
	}
	p.memAllocator = malloc

	log.Info("setting up %s policy...", PolicyName)
	if p.cpuTree, err = NewCpuTreeFromSystem(); err != nil {
		log.Errorf("creating CPU topology tree failed: %s", err)
	}
	log.Debug("CPU topology: %s", p.cpuTree)

	// Handle policy-specific options
	log.Debug("creating %s configuration", PolicyName)
	if err := p.setConfig(bpoptions); err != nil {
		return balloonsError("failed to create %s policy: %v", PolicyName, err)
	}
	log.Debug("first effective configuration:\n%s\n", utils.DumpJSON(p.bpoptions))

	return nil
}

// Name returns the name of this policy.
func (p *balloons) Name() string {
	return PolicyName
}

// Description returns the description for this policy.
func (p *balloons) Description() string {
	return PolicyDescription
}

// Start prepares this policy for accepting allocation/release requests.
func (p *balloons) Start() error {
	log.Info("%s policy started", PolicyName)
	return nil
}

// Sync synchronizes the active policy state.
func (p *balloons) Sync(add []cache.Container, del []cache.Container) error {
	log.Debug("synchronizing state...")
	for _, c := range del {
		if err := p.ReleaseResources(c); err != nil {
			log.Warnf("releasing resources for Sync produced an error: %v", err)
		}
	}

	cache.SortContainers(add, cache.ComparePodCtime, cache.CompareContainerCtime)

	for _, c := range add {
		if err := p.AllocateResources(c); err != nil {
			log.Warnf("allocating resources for Sync produced an error: %v", err)
		}
	}
	return nil
}

// AllocateResources is a resource allocation request for this policy.
func (p *balloons) AllocateResources(c cache.Container) error {
	if c.PreserveCpuResources() {
		log.Infof("not handling resources of container %s, preserving CPUs %q and memory %q", c.PrettyName(), c.GetCpusetCpus(), c.GetCpusetMems())
		return nil
	}

	if p.bpoptions.Preserve != nil {
		rule, err := p.bpoptions.Preserve.MatchContainer(c)
		if err != nil {
			log.Errorf("error in matching container %s to preserve conditions: %s", c, err)
		} else if rule != "" {
			log.Debugf("preserve container %s due to matching %s", c, rule)
			return nil
		}
	}

	log.Debug("allocating resources for container %s (request %d mCPU, limit %d mCPU)...",
		c.PrettyName(),
		p.containerRequestedMilliCpus(c.GetID()),
		p.containerLimitedMilliCpus(c.GetID()))
	bln, err := p.allocateBalloon(c)
	if err != nil {
		return balloonsError("balloon allocation for container %s failed: %w", c.PrettyName(), err)
	}
	if bln == nil {
		return balloonsError("no suitable balloons found for container %s", c.PrettyName())
	}
	// Resize selected balloon to fit the new container, unless it
	// uses the ReservedResources CPUs, which is a fixed set.
	reqMilliCpus := p.containerRequestedMilliCpus(c.GetID()) + p.requestedMilliCpus(bln)
	// Even if all containers in a balloon request is 0 mCPU in
	// total (all are BestEffort, for example), force the size of
	// the balloon to be enough for at least 1 mCPU
	// request. Otherwise balloon's cpuset becomes empty, which in
	// would mean no CPU pinning and balloon's containers would
	// run on any CPUs.
	if bln.AvailMilliCpus() < max(1, reqMilliCpus) {
		if err := p.resizeBalloon(bln, max(1, reqMilliCpus)); err != nil {
			return balloonsError("resizing balloon %s failed: %w", bln.PrettyName(), err)
		}
	}
	p.assignContainer(c, bln)
	if log.DebugEnabled() {
		log.Debug(p.dumpBalloon(bln))
	}
	return nil
}

// ReleaseResources is a resource release request for this policy.
func (p *balloons) ReleaseResources(c cache.Container) error {
	log.Debug("releasing container %s...", c.PrettyName())
	if bln := p.balloonByContainer(c); bln != nil {
		p.dismissContainer(c, bln)
		if log.DebugEnabled() {
			log.Debug(p.dumpBalloon(bln))
		}
		if bln.ContainerCount() == 0 {
			// Deflate the balloon completely before
			// freeing it.
			if err := p.resizeBalloon(bln, 0); err != nil {
				log.Warnf("failed to deflate balloon %s: %v", bln.PrettyName(), err)
			}
			log.Debug("all containers removed, free balloon allocation %s", bln.PrettyName())
			p.freeBalloon(bln)
		} else {
			// Make sure that the balloon will have at
			// least 1 CPU to run remaining containers.
			if err := p.resizeBalloon(bln, max(1, p.requestedMilliCpus(bln))); err != nil {
				return balloonsError("resizing balloon %s failed: %w", bln.PrettyName(), err)
			}
		}
	} else {
		log.Debug("ReleaseResources: balloon-less container %s, nothing to release", c.PrettyName())
	}
	return nil
}

// UpdateResources is a resource allocation update request for this policy.
func (p *balloons) UpdateResources(c cache.Container) error {
	log.Debug("(not) updating container %s...", c.PrettyName())
	return nil
}

// HandleEvent handles policy-specific events.
func (p *balloons) HandleEvent(*events.Policy) (bool, error) {
	log.Debug("(not) handling event...")
	return false, nil
}

// ExportResourceData provides resource data to export for the container.
func (p *balloons) ExportResourceData(c cache.Container) map[string]string {
	return nil
}

// GetTopologyZones returns the policy/pool data for 'topology zone' CRDs.
func (p *balloons) GetTopologyZones() []*policy.TopologyZone {
	showContainers := false
	if p.bpoptions.ShowContainersInNrt != nil {
		showContainers = *p.bpoptions.ShowContainersInNrt
	}

	zones := []*policyapi.TopologyZone{}
	sysmCpu := 1000 * p.cpuTree.cpus.Size()
	for _, bln := range p.balloons {
		// Expose every balloon as a separate zone.
		zone := &policyapi.TopologyZone{
			Name: bln.PrettyName(),
			Type: "balloon",
		}

		cpu := &policyapi.ZoneResource{
			Name: policyapi.CPUResource,
		}

		// "Capacity" is the total number of CPUs available in
		// the system, including CPUs not allowed to be used
		// by the policy.
		cpu.Capacity = *resource.NewMilliQuantity(
			int64(sysmCpu),
			resource.DecimalSI)

		// "Allocatable" is the largest CPU request of a
		// container that can be fit into the balloon, given
		// that this or other balloons do not include any
		// containers.
		maxBlnSize := p.ifreeCpus.Size()
		if bln.Def.MinBalloons > 0 {
			// If this is a pre-created balloon, then
			// ifreeCpus is missing CPUs pre-allocated for
			// it.
			maxBlnSize += bln.Def.MinCpus
		}
		if bln.Def.MaxCpus == NoLimit || bln.Def.MaxCpus > maxBlnSize {
			cpu.Allocatable = *resource.NewMilliQuantity(
				1000*int64(maxBlnSize),
				resource.DecimalSI)
		} else {
			cpu.Allocatable = *resource.NewMilliQuantity(
				1000*int64(bln.Def.MaxCpus),
				resource.DecimalSI)
		}

		// "Available" is the largest CPU request of a
		// container that currently fits into the
		// balloon. This takes into account containers already
		// in the balloon, balloon's CPU limit (maxCPUs),
		// policy's allowed CPUs and already allocated CPUs to
		// other balloons (freeCpus) as the balloon may be
		// inflated to fit the container.
		blnReqmCpu := p.requestedMilliCpus(bln)
		cpu.Available = *resource.NewMilliQuantity(
			int64(bln.MaxAvailMilliCpus(p.freeCpus)-blnReqmCpu),
			resource.DecimalSI)

		zone.Resources = append(zone.Resources, cpu)

		attributes := []*policyapi.ZoneAttribute{
			{
				// "cpuset" are CPUs allowed only to
				// containers in this balloon.
				Name:  policyapi.CPUsAttribute,
				Value: bln.Cpus.String(),
			},
			{
				// "shared cpuset" are CPUs allowed to
				// containers in this and other
				// balloons that shareIdleCPUsInSame
				// scope.
				Name:  policyapi.SharedCPUsAttribute,
				Value: bln.SharedIdleCpus.String(),
			},
			{
				// "excess cpus" is the largest CPU
				// request of a container that fits
				// into this balloon without inflating
				// it.
				Name:  policyapi.ExcessCPUsAttribute,
				Value: fmt.Sprintf("%dm", bln.AvailMilliCpus()-blnReqmCpu),
			},
		}
		if len(bln.components) > 0 {
			var compCpusString func(*Balloon) string
			compCpusString = func(b *Balloon) string {
				if len(b.components) == 0 {
					return fmt.Sprintf("{%s}", b.Cpus)
				}
				res := []string{}
				for _, comp := range b.components {
					res = append(res, compCpusString(comp))
				}
				return fmt.Sprintf("{%s}", strings.Join(res, ", "))
			}
			attributes = append(attributes, &policyapi.ZoneAttribute{
				Name:  policyapi.ComponentCPUsAttribute,
				Value: compCpusString(bln),
			})
		}
		zone.Attributes = append(zone.Attributes, attributes...)
		zones = append(zones, zone)

		// Add more zones only if showing containers as part
		// of node resource topologies is enabled.
		showContainersOfThisBalloon := showContainers
		if bln.Def.ShowContainersInNrt != nil {
			showContainersOfThisBalloon = *bln.Def.ShowContainersInNrt
		}
		if !showContainersOfThisBalloon {
			continue
		}

		// A container assigned into a balloon is exposed as a
		// "allocation for container" subzone whose parent is
		// the balloon zone. The subzone has following
		// resources:
		//
		// "Capacity": container's resource usage limit. CPU
		// usage of the container is limited by
		// resources.limits.cpu and the number of allowed CPUs
		// (balloon cpuset + shared). "Capacity" reflects the
		// the tighter of these two limits.
		//
		// "Allocatable": container resource request. This
		// reflects how container affects the balloon
		// size. When the balloon has no "excess cpus", the
		// sum of "Allocatable" CPUs of its containers equals
		// to the size of balloon's cpuset.
		//
		// "Available": always 0. This prevents any kube
		// scheduler extension from allocating resources from
		// this subzone.
		//
		// Attributes of the subzone include cpuset and memory
		// nodes allowed for the container. The cpuset consist
		// of balloon's own and shared CPUs. Memory nodes
		// depend on balloon-type parameters and pod
		// annotations that specify if memory should be pinned
		// at all, and which memory types should be used. Set
		// of allowed memory nodes may be expanded from the
		// lowest latency nodes due to memory requests that do
		// not fit on the limited number of node set.
		for _, ctrIDs := range bln.PodIDs {
			for _, ctrID := range ctrIDs {
				c, ok := p.cch.LookupContainer(ctrID)
				if !ok {
					continue
				}
				czone := &policyapi.TopologyZone{
					Name: c.PrettyName(),
					Type: policyapi.ContainerAllocationZoneType,
				}
				ctrLimitmCpu := p.containerLimitedMilliCpus(ctrID)
				ctrReqmCpu := p.containerRequestedMilliCpus(ctrID)
				ctrCapacitymCpu := ctrLimitmCpu
				ctrCpusetCpus := c.GetCpusetCpus()
				ctrAllowedmCpu := sysmCpu
				if ctrCpusetCpus != "" {
					ctrAllowedmCpu = 1000 * cpuset.MustParse(ctrCpusetCpus).Size()
				}
				if ctrLimitmCpu == 0 || ctrLimitmCpu > ctrAllowedmCpu {
					ctrCapacitymCpu = ctrAllowedmCpu
				}
				czone.Resources = []*policyapi.ZoneResource{
					{
						Name: policyapi.CPUResource,
						Capacity: *resource.NewMilliQuantity(
							int64(ctrCapacitymCpu),
							resource.DecimalSI),
						Allocatable: *resource.NewMilliQuantity(
							int64(ctrReqmCpu),
							resource.DecimalSI),
					},
				}
				czone.Parent = zone.Name
				czone.Attributes = []*policyapi.ZoneAttribute{
					{
						Name:  policyapi.CPUsAttribute,
						Value: ctrCpusetCpus,
					},
					{
						Name:  policyapi.MemsetAttribute,
						Value: c.GetCpusetMems(),
					},
				}
				zones = append(zones, czone)
			}
		}
	}
	return zones
}

// balloonByContainer returns a balloon that contains a container.
func (p *balloons) balloonByContainer(c cache.Container) *Balloon {
	podID := c.GetPodID()
	cID := c.GetID()
	for _, bln := range p.balloons {
		for _, ctrID := range bln.PodIDs[podID] {
			if ctrID == cID {
				return bln
			}
		}
	}
	return nil
}

// balloonsByFunc returns balloons for which the callback function
// returns true.
func balloonsByFunc(balloons []*Balloon, f func(*Balloon) bool) []*Balloon {
	blns := []*Balloon{}
	for _, bln := range balloons {
		if f(bln) {
			blns = append(blns, bln)
		}
	}
	return blns
}

// balloonsByNamespace returns balloons that contain containers in a
// namespace.
func (p *balloons) balloonsByNamespace(namespace string) []*Balloon {
	return balloonsByFunc(p.balloons, func(bln *Balloon) bool {
		for podID, ctrIDs := range bln.PodIDs {
			if len(ctrIDs) == 0 {
				continue
			}
			if pod, ok := p.cch.LookupPod(podID); ok && pod.GetNamespace() == namespace {
				return true
			}
		}
		return false
	})
}

// balloonsByPod returns balloons that contain any container of a pod.
func (p *balloons) balloonsByPod(pod cache.Pod) []*Balloon {
	podID := pod.GetID()
	return balloonsByFunc(p.balloons, func(bln *Balloon) bool {
		_, ok := bln.PodIDs[podID]
		return ok
	})
}

// balloonsByDef returns list of balloons instantiated from a balloon
// definition.
func (p *balloons) balloonsByDef(blnDef *BalloonDef) []*Balloon {
	return balloonsByFunc(p.balloons, func(bln *Balloon) bool {
		return bln.Def == blnDef
	})
}

// balloonDefByName returns a balloon definition with a name.
func (p *balloons) balloonDefByName(defName string) *BalloonDef {
	for _, blnDef := range p.bpoptions.BalloonDefs {
		if blnDef.Name == defName {
			return blnDef
		}
	}
	return nil
}

func (p *balloons) chooseBalloonDef(c cache.Container) (*BalloonDef, error) {
	log.Debugf("choosing balloon type for container %s...", c.PrettyName())
	// Case 1: BalloonDef is defined by annotation.
	if blnDefName, ok := c.GetEffectiveAnnotation(balloonKey); ok {
		blnDef := p.balloonDefByName(blnDefName)
		if blnDef == nil {
			return nil, balloonsError("no balloon for annotation %q", blnDefName)
		}
		log.Debugf("- annotation %q found, using balloon type %q", balloonKey, blnDefName)
		return blnDef, nil
	}

	for _, blnDef := range p.bpoptions.BalloonDefs {
		// Case 2: BalloonDef is defined by a match expression.
		for _, expr := range blnDef.MatchExpressions {
			log.Debugf("- checking expression %s of balloon type %q against container %s...",
				expr.String(), blnDef.Name, c.PrettyName())
			if expr.Evaluate(c) {
				log.Debugf("  => matches")
				return blnDef, nil
			}
		}

		// Case 3: BalloonDef is defined by the namespace.
		if namespaceMatches(c.GetNamespace(), blnDef.Namespaces) {
			log.Debugf("- namespace %q matches namespaces of balloon type %q", c.GetNamespace(), blnDef.Name)
			return blnDef, nil
		}
	}

	log.Debugf("- no match found, using default balloon type %q", defaultBalloonDefName)
	// Case 4: Fallback to the default balloon.
	return p.defaultBalloonDef, nil
}

func (p *balloons) containerRequestedMilliCpus(contID string) int {
	cont, ok := p.cch.LookupContainer(contID)
	if !ok {
		return 0
	}
	reqCpu, ok := cont.GetResourceRequirements().Requests[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	return int(reqCpu.MilliValue())
}

func (p *balloons) containerLimitedMilliCpus(contID string) int {
	cont, ok := p.cch.LookupContainer(contID)
	if !ok {
		return 0
	}
	reqCpu, ok := cont.GetResourceRequirements().Limits[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	return int(reqCpu.MilliValue())
}

// requestedMilliCpus sums up and returns CPU requests of all
// containers assigned to a balloon.
func (p *balloons) requestedMilliCpus(bln *Balloon) int {
	cpuRequested := 0
	for _, cID := range bln.ContainerIDs() {
		cpuRequested += p.containerRequestedMilliCpus(cID)
	}
	return cpuRequested
}

// freeMilliCpus returns free CPU resources in a balloon without
// inflating the balloon.
func (p *balloons) freeMilliCpus(bln *Balloon) int {
	return bln.AvailMilliCpus() - p.requestedMilliCpus(bln)
}

// maxFreeMilliCpus returns free CPU resources in a balloon when it is
// inflated as large as possible.
func (p *balloons) maxFreeMilliCpus(bln *Balloon) int {
	return bln.MaxAvailMilliCpus(p.freeCpus) - p.requestedMilliCpus(bln)
}

// largest helps finding largest elements and the largest value in a
// slice. Input the length of a slice and a function that returns the
// magnitude of given element in the slice as int.
func largest(sliceLen int, valueOf func(i int) int) ([]int, int) {
	largestIndices := []int{}
	// the largest value found so far is the smallest number that
	// can be presented with int:
	largestValue := math.MinInt
	for index := 0; index < sliceLen; index++ {
		value := valueOf(index)
		switch {
		case len(largestIndices) == 0:
			largestIndices = append(largestIndices, index)
			largestValue = value
		case value == largestValue:
			largestIndices = append(largestIndices, index)
		case value > largestValue:
			largestIndices = []int{index}
			largestValue = value
		}
	}
	return largestIndices, largestValue
}

// resetCpuClass resets CPU configurations globally. All balloons can
// be ignored, their CPU configurations will be applied later.
func (p *balloons) resetCpuClass() error {
	// Usual inputs:
	// - p.allowed (cpuset.CPUset): all CPUs available for this
	//   policy.
	// - p.IdleCpuClass (string): CPU class for allowed CPUs.
	//
	// Other inputs, if needed:
	// - p.reserved (cpuset.CPUset): CPUs of ReservedResources
	//   (typically for kube-system containers).
	//
	// Note: p.useCpuClass(balloon) will be called before assigning
	// containers on the balloon, including the reserved balloon.
	//
	// TODO: don't depend on cpu controller directly
	if err := cpucontrol.Assign(p.cch, p.bpoptions.IdleCpuClass, p.allowed.UnsortedList()...); err != nil {
		log.Warnf("failed to reset class of available cpus: %v", err)
	} else {
		log.Debugf("reset class of available cpus: %q (reserved: %q)", p.allowed, p.reserved)
	}
	return nil
}

// useCpuClass configures CPUs of a balloon.
func (p *balloons) useCpuClass(bln *Balloon) error {
	// Usual inputs:
	// - CPUs that cpuallocator has reserved for this balloon:
	//   bln.Cpus (cpuset.CPUSet).
	// - User-defined CPU configuration for CPUs of balloon of this type:
	//   bln.Def.CpuClass (string).
	// - Current configuration(?): feel free to add data
	//   structure for this. For instance policy-global p.cpuConfs,
	//   or balloon-local bln.cpuConfs.
	//
	// Other input examples, if needed:
	// - Requested CPU resources by all containers in the balloon:
	//   p.requestedMilliCpus(bln).
	// - Free CPU resources in the balloon: p.freeMilliCpus(bln).
	// - Number of assigned containers: bln.ContainerCount().
	// - Container details: access p.cch with bln.ContainerIDs().
	// - User-defined CPU AllocatorPriority: bln.Def.AllocatorPriority.
	// - All existing balloon instances: p.balloons.
	// - CPU configurations by user: bln.Def.CpuClass (for bln in p.balloons)
	if len(bln.components) > 0 {
		// If this is a composite balloon, CPU class is
		// defined in the component balloons.
		log.Debugf("apply CPU class %q on CPUs %s of composite balloon %q",
			bln.Def.CpuClass, bln.Cpus, bln.PrettyName())
		for _, compBln := range bln.components {
			if err := p.useCpuClass(compBln); err != nil {
				log.Warnf("failed to apply CPU class %q on CPUs %s of %q in composite balloon %q: %v",
					compBln.Def.CpuClass, compBln.Cpus, compBln.PrettyName(), bln.PrettyName(), err)
			}

		}
		return nil
	}
	if err := cpucontrol.Assign(p.cch, bln.Def.CpuClass, bln.Cpus.UnsortedList()...); err != nil {
		log.Warnf("failed to apply class %q on CPUs %q: %v", bln.Def.CpuClass, bln.Cpus, err)
	} else {
		log.Debugf("apply CPU class %q on CPUs %q of %q", bln.Def.CpuClass, bln.Cpus, bln.PrettyName())
	}
	return nil
}

// forgetCpuClass is called when CPUs of a balloon are released from duty.
func (p *balloons) forgetCpuClass(bln *Balloon) {
	// Use p.IdleCpuClass for bln.Cpus.
	// Usual inputs: see useCpuClass
	if err := cpucontrol.Assign(p.cch, p.bpoptions.IdleCpuClass, bln.Cpus.UnsortedList()...); err != nil {
		log.Warnf("failed to forget class %q of cpus %q: %v", bln.Def.CpuClass, bln.Cpus, err)
	} else {
		if len(bln.components) > 0 {
			log.Debugf("forget classes of composite balloon %q cpus %q", bln.Def.Name, bln.Cpus)
		} else {
			log.Debugf("forget class %q of cpus %q", bln.Def.CpuClass, bln.Cpus)
		}
	}
}

// updateLoadedVirtDevsInAllocatorOptions updates CPU allocator
// options with virtual devices under load due to the given load
// classes. Returns a set of virtual device names under load.
func (p *balloons) updateLoadedVirtDevsInAllocatorOptions(allocatorOptions *cpuTreeAllocatorOptions, loadClassNames []string) map[string]struct{} {
	loadedVirtDevs := make(map[string]struct{})
	for _, lc := range loadClassNames {
		vdName := p.loadVirtDev[lc].name
		if _, ok := loadedVirtDevs[vdName]; ok {
			// Already handled this virtual device in some
			// other load class.
			continue
		}
		// Go through all balloons that share the same loaded
		// virtual device and collect their CPUs into vdCpus
		// (virtual device CPUs)
		vdCpus := cpuset.New()
		for _, bln := range p.balloons {
			if _, ok := bln.LoadedVirtDevs[vdName]; !ok {
				continue
			}
			vdCpus = vdCpus.Union(bln.Cpus)
		}
		loadedVirtDevs[vdName] = struct{}{}
		p.updateLoadedVirtDev(allocatorOptions, p.loadVirtDev[lc], vdCpus, true)
	}
	return loadedVirtDevs
}

func (p *balloons) updateLoadedVirtDev(allocatorOptions *cpuTreeAllocatorOptions, virtDev *loadClassVirtDev, vdCpus cpuset.CPUSet, overwrite bool) {
	prevCpus := cpuset.New()
	virtDevName := virtDev.name
	if !overwrite && len(allocatorOptions.virtDevCpusets[virtDevName]) > 0 {
		prevCpus = allocatorOptions.virtDevCpusets[virtDevName][0]
	}
	// Calculate all CPUs affected by load on vdCpus.
	switch virtDev.level {
	case CPUTopologyLevelCore:
		// add all CPUs from same cores of virtual device CPUs
		allocatorOptions.virtDevCpusets[virtDevName] = []cpuset.CPUSet{prevCpus.Union(p.cpuTree.system().AllThreadsForCPUs(vdCpus))}
	case CPUTopologyLevelL2Cache:
		// add all CPUs from the same L2 cache of virtual
		// device CPUs
		allocatorOptions.virtDevCpusets[virtDevName] = []cpuset.CPUSet{prevCpus.Union(p.cpuTree.system().AllCPUsSharingNthLevelCacheWithCPUs(2, vdCpus))}
	default:
		log.Error("internal error: not implemented load level %q used in virtual device %q", virtDev.level, virtDevName)
	}
	log.Debugf("    loaded virtual device %q on CPUs %q affects CPUs %q", virtDev.name, vdCpus, allocatorOptions.virtDevCpusets[virtDevName])
}

// virtDevsChangeDuringCpuAllocation returns true if any of the
// virtual devices under load due to the given load classes change
// during CPU allocation.
func (p *balloons) virtDevsChangeDuringCpuAllocation(loadClassNames []string) bool {
	for _, lc := range loadClassNames {
		if virtDev, ok := p.loadVirtDev[lc]; ok && virtDev.updateOnEveryCpuAllocation {
			return true
		}
	}
	return false
}

func (p *balloons) newCompositeBalloon(blnDef *BalloonDef, confCpus bool, freeInstance int) (*Balloon, error) {
	componentBlns := make([]*Balloon, 0, len(blnDef.Components))
	deleteComponentBlns := func() {
		for _, compBln := range componentBlns {
			log.Debugf("removing component balloon %s of composite balloon %s",
				compBln.PrettyName(), blnDef.Name)
			p.deleteBalloon(compBln)
		}
	}
	for _, comp := range blnDef.Components {
		// Create a balloon for each component.
		compDef := p.balloonDefByName(comp.DefName)
		if compDef == nil {
			deleteComponentBlns()
			return nil, balloonsError("unknown balloon definition %q in composite balloon %q",
				comp.DefName, blnDef.Name)
		}
		compBln, err := p.newBalloon(compDef, confCpus)
		if err != nil || compBln == nil {
			deleteComponentBlns()
			return nil, balloonsError("failed to create component balloon %q for composite balloon %q: %v",
				comp.DefName, blnDef.Name, err)
		}
		componentBlns = append(componentBlns, compBln)
		log.Debugf("created component balloon %s of composite balloon %s",
			compBln.PrettyName(), blnDef.Name)
	}
	memTypeMask, _ := memTypeMaskFromStringList(blnDef.MemoryTypes)
	bln := &Balloon{
		Def:            blnDef,
		Instance:       freeInstance,
		Groups:         make(map[string]int),
		PodIDs:         make(map[string][]string),
		Cpus:           cpuset.New(),
		SharedIdleCpus: cpuset.New(),
		LoadedVirtDevs: make(map[string]struct{}),
		cpuTreeAlloc:   nil, // Allocator is not used for composite balloons.
		memTypeMask:    memTypeMask,
		components:     componentBlns,
	}
	log.Debugf("created composite balloon %s with %d components. Now resize it to %d mCPU",
		bln.PrettyName(), len(bln.components), blnDef.MinCpus*1000)
	if err := p.resizeBalloon(bln, blnDef.MinCpus*1000); err != nil {
		deleteComponentBlns()
		return nil, err
	}
	if confCpus {
		if err := p.useCpuClass(bln); err != nil {
			deleteComponentBlns()
			log.Errorf("failed to apply CPU configuration to new composite balloon %s[%d] (cpus: %s): %w",
				blnDef.Name, bln.Instance, bln.Cpus, err)
			return nil, err
		}
	}
	return bln, nil
}

func (p *balloons) newBalloon(blnDef *BalloonDef, confCpus bool) (*Balloon, error) {
	var cpus cpuset.CPUSet
	var err error
	blnsOfDef := p.balloonsByDef(blnDef)
	// Allowed to create new balloon instance from blnDef?
	if blnDef.MaxBalloons > NoLimit && blnDef.MaxBalloons <= len(blnsOfDef) {
		return nil, balloonsError("cannot create new %q balloon, MaxBalloons limit (%d) reached", blnDef.Name, blnDef.MaxBalloons)
	}
	// Find the first unused balloon instance index.
	freeInstance := 0
	for freeInstance = 0; freeInstance < len(blnsOfDef); freeInstance++ {
		isFree := true
		for _, bln := range blnsOfDef {
			if bln.Instance == freeInstance {
				isFree = false
				break
			}
		}
		if isFree {
			break
		}
	}
	if len(blnDef.Components) > 0 {
		return p.newCompositeBalloon(blnDef, confCpus, freeInstance)
	}
	// Configure cpuTreeAllocator for this balloon. The reserved
	// balloon always prefers to be close to the virtual device
	// that is close to ReservedResources CPUs. All other balloon
	// types prefer to be far from those CPUs.
	allocatorOptions := cpuTreeAllocatorOptions{
		topologyBalancing:           p.bpoptions.AllocatorTopologyBalancing,
		preferSpreadOnPhysicalCores: p.bpoptions.PreferSpreadOnPhysicalCores,
		preferCloseToDevices:        blnDef.PreferCloseToDevices,
		preferFarFromDevices:        blnDef.PreferFarFromDevices,
		virtDevCpusets: map[string][]cpuset.CPUSet{
			virtDevReservedCpus: {p.reserved},
			virtDevIsolatedCpus: {p.options.System.Isolated()},
			virtDevECores:       {p.cpuAllocator.GetCPUPriorities()[cpuallocator.PriorityLow]},
			virtDevPCores:       {p.cpuAllocator.GetCPUPriorities()[cpuallocator.PriorityHigh]},
		},
	}
	if blnDef.AllocatorTopologyBalancing != nil {
		allocatorOptions.topologyBalancing = *blnDef.AllocatorTopologyBalancing
	}
	if blnDef.PreferSpreadOnPhysicalCores != nil {
		allocatorOptions.preferSpreadOnPhysicalCores = *blnDef.PreferSpreadOnPhysicalCores
	}
	loadedVirtDevs := p.updateLoadedVirtDevsInAllocatorOptions(&allocatorOptions, blnDef.Loads)
	if len(loadedVirtDevs) > 0 {
		log.Debugf("balloon %s[%d] loaded virtual devices: %+v", blnDef.Name, freeInstance, allocatorOptions.virtDevCpusets)
	}
	cpuTreeAlloc := p.cpuTree.NewAllocator(allocatorOptions)
	memTypeMask, _ := memTypeMaskFromStringList(blnDef.MemoryTypes)
	bln := &Balloon{
		Def:            blnDef,
		Instance:       freeInstance,
		Groups:         make(map[string]int),
		PodIDs:         make(map[string][]string),
		Cpus:           cpuset.New(),
		SharedIdleCpus: cpuset.New(),
		LoadedVirtDevs: loadedVirtDevs,
		cpuTreeAlloc:   cpuTreeAlloc,
		memTypeMask:    memTypeMask,
	}
	if p.virtDevsChangeDuringCpuAllocation(blnDef.Loads) {
		bln.cpuTreeAlloc.options.deviceUpdateOnEveryCpu = func(currentCpus cpuset.CPUSet) {
			for _, load := range blnDef.Loads {
				p.updateLoadedVirtDev(&cpuTreeAlloc.options, p.loadVirtDev[load], currentCpus, false)
			}
		}
	}
	if err := p.resizeBalloon(bln, blnDef.MinCpus*1000); err != nil {
		return nil, err
	}
	bln.Mems = p.closestMems(bln.Cpus)
	if confCpus {
		if err = p.useCpuClass(bln); err != nil {
			log.Errorf("failed to apply CPU configuration to new balloon %s[%d] (cpus: %s): %w", blnDef.Name, freeInstance, cpus, err)
			return nil, err
		}
	}
	return bln, nil
}

// deleteBalloon removes an empty balloon.
func (p *balloons) deleteBalloon(bln *Balloon) {
	log.Debugf("deleting balloon %s", bln)
	remainingBalloons := []*Balloon{}
	for _, b := range p.balloons {
		if b != bln {
			remainingBalloons = append(remainingBalloons, b)
		}
	}
	p.balloons = remainingBalloons
	p.forgetCpuClass(bln)
	p.freeCpus = p.freeCpus.Union(bln.Cpus)
	if _, err := p.cpuAllocator.ReleaseCpus(&bln.Cpus, bln.Cpus.Size(), bln.Def.AllocatorPriority.Value().Option()); err != nil {
		log.Warnf("failed to release CPUs %q of balloon %s[%d]: %v", bln.Cpus, bln.Def.Name, bln.Instance, err)
	}
}

// freeBalloon clears a balloon and deletes it if allowed.
func (p *balloons) freeBalloon(bln *Balloon) {
	bln.PodIDs = make(map[string][]string)
	blnsSameDef := p.balloonsByDef(bln.Def)
	if len(blnsSameDef) > bln.Def.MinBalloons {
		p.deleteBalloon(bln)
	}
}

func (p *balloons) fillableBalloonInstances(blnDef *BalloonDef, fm FillMethod, c cache.Container) ([]*Balloon, error) {
	reqMilliCpus := p.containerRequestedMilliCpus(c.GetID())
	switch fm {
	case FillNewBalloon, FillNewBalloonMust:
		// Choosing an existing balloon without containers is
		// preferred over instantiating a new balloon.
		for _, bln := range p.balloonsByDef(blnDef) {
			if len(bln.PodIDs) == 0 {
				return []*Balloon{bln}, nil
			}
		}
		// Creating a new balloon and placing a container
		// (even a best effort one) to it always requires at
		// least one CPU. Make sure this is doable.
		if p.freeCpus.Size() == 0 || p.freeCpus.Size() < blnDef.MinCpus {
			if fm == FillNewBalloonMust {
				return nil, balloonsError("not enough CPUs to create new balloon for container %s requesting %s mCPU. free CPUs: %s",
					c.PrettyName(), reqMilliCpus, p.freeCpus.Size())
			}
			return nil, nil
		}
		newBln, err := p.newBalloon(blnDef, false)
		if err != nil {
			if fm == FillNewBalloonMust {
				return nil, err
			}
			return nil, nil
		}
		// newBln may already have CPUs allocated for it. If
		// we notice that the new balloon fill method cannot
		// be used after all, collect steps to undo() new
		// balloon creation.
		undoFuncs := []func(){}
		undo := func() {
			for _, undoFunc := range undoFuncs {
				undoFunc()
			}
		}
		undoFuncs = append(undoFuncs, func() {
			p.freeCpus = p.freeCpus.Union(newBln.Cpus)
		})
		if newBln.MaxAvailMilliCpus(p.freeCpus) < reqMilliCpus {
			// New balloon cannot be inflated to fit new
			// container. Release its CPUs if already
			// allocated (MinCPUs > 0), and never add it
			// to the list of balloons.
			undo()
			if fm == FillNewBalloonMust {
				return nil, balloonsError("not enough CPUs to run container %s requesting %s mCPU. %s.MaxCPUs: %d mCPU, free CPUs: %s",
					c.PrettyName(), reqMilliCpus, blnDef.Name, blnDef.MaxCpus*1000, p.freeCpus.Size()*1000)
			} else {
				return nil, nil
			}
		}
		// Make the existence of the new balloon official by
		// adding it to the balloons slice.
		p.balloons = append(p.balloons, newBln)
		undoFuncs = append(undoFuncs, func() {
			p.balloons = p.balloons[:len(p.balloons)-1]
		})
		// If the new balloon already has CPUs, there is some
		// housekeeping to do.
		if newBln.Cpus.Size() > 0 {
			// Make sure CPUs in the balloon use correct
			// CPU class.
			if err = p.useCpuClass(newBln); err != nil {
				log.Errorf("failed to apply CPU configuration to new balloon %s (cpus: %s): %s",
					newBln.PrettyName(), newBln.Cpus, err)
				undo()
				return nil, err
			}
			// Reshare idle CPUs because freeCpus have
			// changed and CPUs of the new balloon are no
			// more idle.
			p.updatePinning(p.shareIdleCpus(p.freeCpus, newBln.Cpus)...)
		}
		return []*Balloon{newBln}, nil
	case FillSameGroup:
		group, err := c.Expand(blnDef.GroupBy, true)
		if err != nil {
			log.Errorf("error choosing balloon for container %q based on groupBy: %s", c.PrettyName(), err)
			return nil, nil
		}
		return balloonsByFunc(p.balloons,
			func(bln *Balloon) bool {
				return bln.Groups[group] > 0 &&
					bln.Def == blnDef &&
					p.maxFreeMilliCpus(bln) >= reqMilliCpus
			}), nil
	case FillSameNamespace:
		return balloonsByFunc(p.balloonsByNamespace(c.GetNamespace()),
			func(bln *Balloon) bool {
				return bln.Def == blnDef && p.maxFreeMilliCpus(bln) >= reqMilliCpus
			}), nil
	case FillSamePod:
		if pod, ok := c.GetPod(); ok {
			return balloonsByFunc(p.balloonsByPod(pod),
				func(bln *Balloon) bool {
					return bln.Def == blnDef && p.maxFreeMilliCpus(bln) >= reqMilliCpus
				}), nil
		} else {
			return nil, balloonsError("fill method %s failed: cannot find pod for container %s", fm, c.PrettyName())
		}
	}
	// Handle fill methods that need existing instances of
	// balloonDef, and fail if there are no instances.
	balloons := p.balloonsByDef(blnDef)
	if len(balloons) == 0 {
		return nil, nil
	}
	switch fm {
	case FillBalanced:
		// Are there balloons where the container would fit
		// without inflating the balloon?
		return balloonsByFunc(balloons, func(bln *Balloon) bool {
			return p.freeMilliCpus(bln) >= reqMilliCpus
		}), nil
	case FillBalancedInflate:
		// Are there balloons where the container would fit
		// after inflating the balloon?
		return balloonsByFunc(balloons, func(bln *Balloon) bool {
			return p.maxFreeMilliCpus(bln) >= reqMilliCpus
		}), nil
	default:
		break
	}
	return nil, balloonsError("balloon type fill method not implemented: %s", fm)
}

func namespaceMatches(namespace string, patterns []string) bool {
	for _, pattern := range patterns {
		ret, err := filepath.Match(pattern, namespace)
		if err == nil && ret {
			return true
		}
	}
	return false
}

// allocateBalloon returns a balloon allocated for a container.
func (p *balloons) allocateBalloon(c cache.Container) (*Balloon, error) {
	blnDef, err := p.chooseBalloonDef(c)
	if err != nil {
		return nil, err
	}
	if blnDef == nil {
		return nil, balloonsError("no applicable balloon type found")
	}

	bln, err := p.allocateBalloonOfDef(blnDef, c)
	if err != nil {
		return nil, err
	}
	if bln == nil {
		return nil, balloonsError("no suitable balloon instance available")
	}
	return bln, nil
}

// allocateBalloonOfDef returns a balloon instantiated from a
// definition for a container.
func (p *balloons) allocateBalloonOfDef(blnDef *BalloonDef, c cache.Container) (*Balloon, error) {
	fillChain := []FillMethod{}
	if blnDef.GroupBy != "" {
		fillChain = append(fillChain, FillSameGroup)
	}
	if !blnDef.PreferSpreadingPods {
		fillChain = append(fillChain, FillSamePod)
	}
	if blnDef.PreferPerNamespaceBalloon {
		fillChain = append(fillChain, FillSameNamespace, FillNewBalloon)
	}
	if blnDef.PreferNewBalloons {
		fillChain = append(fillChain, FillNewBalloon, FillBalanced, FillBalancedInflate)
	} else {
		fillChain = append(fillChain, FillBalanced, FillBalancedInflate, FillNewBalloon)
	}
	for _, fillMethod := range fillChain {
		blns, err := p.fillableBalloonInstances(blnDef, fillMethod, c)
		if err != nil {
			log.Debugf("fill method %q prevents allocation: %w", fillMethod, err)
			return nil, err
		}
		if len(blns) == 0 {
			log.Debugf("fill method %q not applicable", fillMethod)
			continue
		}
		log.Debugf("fill method %q suggests any of balloon instances %v", fillMethod, blns)

		// TODO: Consider: in case of a best effort container,
		// choose the balloon with the least number of
		// containers assigned to it. This avoids piling up
		// all best efforts to a balloon that has least CPU
		// reservations on it.

		// Choose the balloon with the most free CPUs. If
		// there are equally good candidates, choose the one
		// with the lowest number of containers assigned.
		largestBy := p.freeMilliCpus
		if fillMethod == FillBalancedInflate {
			largestBy = p.maxFreeMilliCpus
		}
		mostRoom, _ := largest(len(blns), func(i int) int {
			return largestBy(blns[i])
		})
		leastContainers, _ := largest(len(mostRoom), func(i int) int {
			return -blns[mostRoom[i]].ContainerCount()
		})
		bestBln := blns[mostRoom[leastContainers[0]]]
		return bestBln, nil
	}
	return nil, nil
}

// dumpBalloon dumps balloon contents in detail.
func (p *balloons) dumpBalloon(bln *Balloon) string {
	conts := []string{}
	pods := []string{}
	for podID, contIDs := range bln.PodIDs {
		podName := podID
		if pod, ok := p.cch.LookupPod(podID); ok {
			podName = pod.GetName()
		}
		pods = append(pods, podName)
		for _, contID := range contIDs {
			if cont, ok := p.cch.LookupContainer(contID); ok {
				conts = append(conts, cont.PrettyName())
			} else {
				conts = append(conts, podName+"."+contID)
			}
		}
	}
	s := fmt.Sprintf("Balloon %s{Cpus: %s; Mems: %s; mCPU used: %d; capacity: %d; max. capacity: %d; pods: %s; conts: %s}",
		bln.PrettyName(),
		bln.Cpus,
		bln.Mems,
		p.requestedMilliCpus(bln),
		bln.AvailMilliCpus(),
		bln.MaxAvailMilliCpus(p.freeCpus),
		pods,
		conts)
	return s
}

// changesBalloons returns true if two balloons policy configurations
// may lead into different balloon instances or workload assignment.
func changesBalloons(opts0, opts1 *BalloonsOptions) bool {
	if opts0 == nil && opts1 == nil {
		return false
	}
	if opts0 == nil || opts1 == nil {
		return true
	}
	if len(opts0.BalloonDefs) != len(opts1.BalloonDefs) {
		return true
	}
	o0 := opts0.DeepCopy()
	o1 := opts1.DeepCopy()
	// Ignore differences in CPU class names. Every other change
	// potentially changes balloons or workloads.
	o0.IdleCpuClass = ""
	o1.IdleCpuClass = ""
	for i := range o0.BalloonDefs {
		o0.BalloonDefs[i].CpuClass = ""
		o1.BalloonDefs[i].CpuClass = ""
	}
	return utils.DumpJSON(o0) != utils.DumpJSON(o1)
}

// changesCpuClasses returns true if two balloons policy
// configurations can lead to using different CPU classes on
// corresponding balloon instances. Calling changesCpuClasses(o0, o1)
// makes sense only if changesBalloons(o0, o1) has returned false.
func changesCpuClasses(opts0, opts1 *BalloonsOptions) bool {
	if opts0 == nil && opts1 == nil {
		return false
	}
	if opts0 == nil || opts1 == nil {
		return true
	}
	if opts0.IdleCpuClass != opts1.IdleCpuClass {
		return true
	}
	if len(opts0.BalloonDefs) != len(opts1.BalloonDefs) {
		return true
	}
	for i := range opts0.BalloonDefs {
		if opts0.BalloonDefs[i].CpuClass != opts1.BalloonDefs[i].CpuClass {
			return true
		}
	}
	return false
}

func (p *balloons) Reconfigure(newCfg interface{}) error {
	balloonsOptions, ok := newCfg.(*BalloonsOptions)
	if !ok {
		return balloonsError("config data of unexpected type %T", newCfg)
	}

	log.Info("configuration update")
	defer func() {
		log.Debug("effective configuration:\n%s\n", utils.DumpJSON(p.bpoptions))
	}()
	newBalloonsOptions := balloonsOptions.DeepCopy()
	if !changesBalloons(p.bpoptions, newBalloonsOptions) {
		if !changesCpuClasses(p.bpoptions, newBalloonsOptions) {
			log.Info("no configuration changes")
		} else {
			log.Info("configuration changes only on CPU classes")
			// Update new CPU classes to existing balloon
			// definitions. The same BalloonDef instances
			// must be kept in use, because each Balloon
			// instance holds a direct reference to its
			// BalloonDef.
			for i := range p.bpoptions.BalloonDefs {
				p.bpoptions.BalloonDefs[i].CpuClass = newBalloonsOptions.BalloonDefs[i].CpuClass
			}
			// (Re)configures all CPUs in balloons.
			if err := p.resetCpuClass(); err != nil {
				log.Warnf("failed to reset CPU class: %v", err)
			}
			for _, bln := range p.balloons {
				if err := p.useCpuClass(bln); err != nil {
					log.Warnf("failed to apply CPU class to balloon %s: %v", bln.PrettyName(), err)
				}
			}
		}
		return nil
	}
	if err := p.setConfig(newBalloonsOptions); err != nil {
		log.Error("config update failed: %v", err)
		return err
	}
	log.Info("config updated successfully")
	if err := p.Sync(p.cch.GetContainers(), p.cch.GetContainers()); err != nil {
		log.Warnf("failed to sync containers: %v", err)
	}
	return nil
}

// applyBalloonDef creates user-defined balloons or reconfigures built-in
// balloons according to the blnDef. Does not initialize balloon CPUs.
func (p *balloons) applyBalloonDef(balloons *[]*Balloon, blnDef *BalloonDef, freeCpus *cpuset.CPUSet) error {
	for blnIdx := 0; blnIdx < blnDef.MinBalloons; blnIdx++ {
		newBln, err := p.newBalloon(blnDef, false)
		if err != nil {
			return err
		}
		if newBln == nil {
			return balloonsError("failed to create balloon '%s[%d]' as required by MinBalloons=%d", blnDef.Name, blnIdx, blnDef.MinBalloons)
		}
		*balloons = append(*balloons, newBln)
	}

	return nil
}

func (p *balloons) validateConfig(bpoptions *BalloonsOptions) error {
	seenNames := map[string]struct{}{}
	undefinedLoadClasses := map[string]struct{}{}
	compositeBlnDefs := map[string]*BalloonDef{}
	for _, blnDef := range bpoptions.BalloonDefs {
		if blnDef.Name == "" {
			return balloonsError("missing or empty name in a balloon type")
		}
		if _, ok := seenNames[blnDef.Name]; ok {
			return balloonsError("two balloon types with the same name: %q", blnDef.Name)
		}
		seenNames[blnDef.Name] = struct{}{}
		if blnDef.MaxCpus != NoLimit && blnDef.MinCpus > blnDef.MaxCpus {
			return balloonsError("MinCpus (%d) > MaxCpus (%d) in balloon type %q",
				blnDef.MinCpus, blnDef.MaxCpus, blnDef.Name)
		}
		if blnDef.MaxBalloons != NoLimit && blnDef.MinBalloons > blnDef.MaxBalloons {
			return balloonsError("MinBalloons (%d) > MaxBalloons (%d) in balloon type %q",
				blnDef.MinCpus, blnDef.MaxCpus, blnDef.Name)
		}
		if _, err := memTypeMaskFromStringList(blnDef.MemoryTypes); err != nil {
			return balloonsError("invalid memoryTypes: %w", err)
		}
		if blnDef.Name == reservedBalloonDefName {
			if blnDef.MinBalloons < 0 || blnDef.MinBalloons > 1 {
				return balloonsError("invalid configuration: exactly one %q balloon expected but MinBalloons=%d",
					blnDef.Name, blnDef.MinBalloons)
			}
			if blnDef.MaxBalloons < 0 || blnDef.MaxBalloons > 1 {
				return balloonsError("invalid configuration: exactly one %q balloon expected but MaxBalloons=%d",
					blnDef.Name, blnDef.MaxBalloons)
			}
		}
		if blnDef.PreferIsolCpus && blnDef.ShareIdleCpusInSame != "" {
			log.Warn("WARNING: using PreferIsolCpus with ShareIdleCpusInSame is highly discouraged")
		}
		if len(blnDef.Components) > 0 {
			compositeBlnDefs[blnDef.Name] = blnDef
			if blnDef.CpuClass != "" {
				return balloonsError("composite balloon %q cannot have CpuClasses", blnDef.Name)
			}
			forbiddenCpuAllocationOptions := []string{}
			if blnDef.PreferSpreadOnPhysicalCores != nil {
				forbiddenCpuAllocationOptions = append(forbiddenCpuAllocationOptions, "PreferSpreadOnPhysicalCores")
			}
			if blnDef.AllocatorTopologyBalancing != nil {
				forbiddenCpuAllocationOptions = append(forbiddenCpuAllocationOptions, "AllocatorTopologyBalancing")
			}
			if len(blnDef.PreferCloseToDevices) > 0 {
				forbiddenCpuAllocationOptions = append(forbiddenCpuAllocationOptions, "PreferCloseToDevices")
			}
			if blnDef.PreferIsolCpus {
				forbiddenCpuAllocationOptions = append(forbiddenCpuAllocationOptions, "PreferIsolCpus")
			}
			if blnDef.PreferCoreType != "" {
				forbiddenCpuAllocationOptions = append(forbiddenCpuAllocationOptions, "PreferCoreType")
			}
			if len(forbiddenCpuAllocationOptions) > 0 {
				return balloonsError("CPU allocation options not allowed in composite balloons, but %q has: %s",
					blnDef.Name, strings.Join(forbiddenCpuAllocationOptions, ", "))
			}
		}
		for _, load := range blnDef.Loads {
			undefinedLoadClasses[load] = struct{}{}
		}
	}
	for lcIndex, loadClass := range bpoptions.LoadClasses {
		delete(undefinedLoadClasses, loadClass.Name)
		if loadClass.Name == "" {
			return balloonsError("missing or empty name in a load classes list, index %d", lcIndex)
		}
		if loadClass.Level == CPUTopologyLevelUndefined {
			return balloonsError("missing or invalid level in load class %q", loadClass.Name)
		}
	}
	if len(undefinedLoadClasses) > 0 {
		return balloonsError("loads defined in balloonTypes but missing from loadClasses: %v", undefinedLoadClasses)
	}
	var circularCheck func(name string, seen map[string]int) error
	circularCheck = func(name string, seen map[string]int) error {
		if seen[name] > 0 {
			return balloonsError("circular composition detected in composite balloon %q", name)
		}
		seen[name] += 1
		if compBlnDef, ok := compositeBlnDefs[name]; ok {
			for _, comp := range compBlnDef.Components {
				if err := circularCheck(comp.DefName, seen); err != nil {
					return err
				}
			}
		}
		seen[name] -= 1
		return nil
	}
	for compBlnName, compBlnDef := range compositeBlnDefs {
		for compIdx, comp := range compBlnDef.Components {
			if comp.DefName == "" {
				return balloonsError("missing or empty component balloonType name in composite balloon %q component %d",
					compBlnName, compIdx+1)
			}
			// Make sure every component balloon type is
			// defined in BalloonDefs.
			if _, ok := seenNames[comp.DefName]; !ok {
				return balloonsError("balloon type %q in composite balloon %q is not defined in balloonTypes",
					comp.DefName, compBlnName)
			}
		}
		// Check for circular compositions.
		seen := map[string]int{}
		if err := circularCheck(compBlnName, seen); err != nil {
			return err
		}
	}
	return nil
}

// setConfig takes new balloon configuration into use.
func (p *balloons) setConfig(bpoptions *BalloonsOptions) error {
	bpoptions = bpoptions.DeepCopy()

	// Handle AvailableResources.cpus, if defined.
	// Set p.allowed: CPUs available for the policy.
	var availableCpus cpuset.CPUSet
	amount, kind := bpoptions.AvailableResources.Get(cfgapi.CPU)
	switch kind {
	case cfgapi.AmountCPUSet:
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return balloonsError("failed to parse available CPU cpuset '%s': %w", amount, err)
		}
		availableCpus = cset
	case cfgapi.AmountQuantity:
		return balloonsError("can't handle CPU resources given as resource.Quantity (%v)", amount)
	case cfgapi.AmountAbsent:
		// Available CPUs not specified, default to all on-line CPUs.
		availableCpus = p.options.System.CPUSet().Difference(p.options.System.Offlined())
	}
	p.allowed = availableCpus

	setOmittedDefaults(bpoptions)

	reservedBalloonDef, defaultBalloonDef, err := p.fillBuiltinBalloonDefs(bpoptions)
	if err != nil {
		return err
	}
	if err = p.validateConfig(bpoptions); err != nil {
		return balloonsError("invalid configuration: %w", err)
	}
	p.fillLoadVirtDevices(bpoptions.LoadClasses)
	p.fillCloseToDevices(bpoptions.BalloonDefs)
	p.fillFarFromDevices(bpoptions.BalloonDefs)

	// Preparation and configuration validation is now done
	// without touching the state of the policy.
	// Next apply the configuration.
	p.reservedBalloonDef = reservedBalloonDef
	p.defaultBalloonDef = defaultBalloonDef
	p.balloons = []*Balloon{}
	p.freeCpus = p.allowed.Clone()
	p.bpoptions = bpoptions

	// Create balloon instances in the order of AllocatorPriority.
	for allocPrio := cpuallocator.CPUPriority(0); allocPrio <= cpuallocator.NumCPUPriorities; allocPrio++ {
		for _, blnDef := range bpoptions.BalloonDefs {
			if blnDef.AllocatorPriority.Value() != allocPrio {
				continue
			}
			if err := p.applyBalloonDef(&p.balloons, blnDef, &p.freeCpus); err != nil {
				return err
			}
		}
	}
	p.ifreeCpus = p.freeCpus.Clone()

	// Finish balloon instance initialization.
	log.Info("%s policy balloons:", PolicyName)
	for blnIdx, bln := range p.balloons {
		log.Info("- balloon %d: %s", blnIdx, bln)
	}
	p.updatePinning(p.shareIdleCpus(p.freeCpus, cpuset.New())...)
	// (Re)configures all CPUs in balloons.
	if err := p.resetCpuClass(); err != nil {
		log.Warnf("failed to reset CPU class: %v", err)
	}
	for _, bln := range p.balloons {
		if err := p.useCpuClass(bln); err != nil {
			log.Warnf("failed to apply CPU class to balloon %s: %v", bln.PrettyName(), err)
		}
	}
	return nil
}

// fillBuiltinBalloonDefs ensures that reserved and default balloon
// definitions are included in bpoptions.BalloonDefs, they have valid
// parameters for balloon instantiation, and that reserved BalloonDef
// parameters are aligned with ReservedResources cpus definition.
func (p *balloons) fillBuiltinBalloonDefs(bpoptions *BalloonsOptions) (*BalloonDef, *BalloonDef, error) {
	// Add reserved and default balloon definitions to BalloonDefs
	// if they are not already there.
	var reservedBalloonDef, defaultBalloonDef *BalloonDef
	for _, blnDef := range bpoptions.BalloonDefs {
		switch blnDef.Name {
		case reservedBalloonDefName:
			reservedBalloonDef = blnDef
		case defaultBalloonDefName:
			defaultBalloonDef = blnDef
		}
	}
	if reservedBalloonDef == nil {
		// Add an implicit reserved balloon type as the first
		// item in the list of balloon types. As matching new
		// containers to balloon types happens in the order of
		// types in the list, implicit namespace match to
		// "kube-system" and optional ReservedPoolNamespaces
		// will pick up containers before any user-specified
		// types. As a consequence, namespace match "*" in a
		// user-defined balloon type will match any namespace
		// other than kube-system and ones listed as
		// ReservedPoolNamespaces. Users can change this order
		// by explicitly defining the "reserved" balloon type
		// in their list at the position that suits them.
		reservedBalloonDef = &BalloonDef{
			Name:              reservedBalloonDefName,
			MinBalloons:       1,
			AllocatorPriority: cfgapi.PriorityLow,
		}
		bpoptions.BalloonDefs = append([]*BalloonDef{reservedBalloonDef}, bpoptions.BalloonDefs...)
	}
	if defaultBalloonDef == nil {
		// Add an implicit default balloon type definition as
		// the last item of the list.
		defaultBalloonDef = &BalloonDef{
			Name:              defaultBalloonDefName,
			MinBalloons:       1,
			MaxBalloons:       1,
			AllocatorPriority: cfgapi.PriorityLow,
		}
		bpoptions.BalloonDefs = append(bpoptions.BalloonDefs, defaultBalloonDef)
	}

	// If configuration specifies ReservedResources.cpus, modify
	// reservedBalloonDef so that reserved CPU allocation will
	// happen as expected. Check possible conflicts between
	// ReservedResources.cpus and explicit "reserved" balloon type
	// definitions.
	amount, kind := bpoptions.ReservedResources.Get(cfgapi.CPU)
	switch kind {
	case cfgapi.AmountCPUSet:
		// Explicitly specified reserved cpuset. Raise
		// allocator priority to Normal to catch these CPUs
		// before other Normal-priority balloon types defined
		// by user. High-priority user-defined balloon types
		// can still allocate CPUs first. If reserved
		// balloon's MinCpus is undefined, set it to catch all
		// (or at most MaxCpu) CPUs in the reserved cpuset.
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return nil, nil, balloonsError("failed to parse reserved CPU cpuset '%s': %v", amount, err)
		}
		if cset.Difference(p.allowed).Size() > 0 {
			return nil, nil, balloonsError("ReservedResources cpus %s contains CPUs not in AllowedResources %s, namely %s",
				cset, p.allowed, cset.Difference(p.allowed))
		}
		p.reserved = p.allowed.Intersection(cset)
		if reservedBalloonDef.MinCpus == 0 {
			if p.reserved.Size() < reservedBalloonDef.MaxCpus {
				reservedBalloonDef.MinCpus = p.reserved.Size()
			} else {
				reservedBalloonDef.MinCpus = reservedBalloonDef.MaxCpus
			}
		}
		reservedBalloonDef.AllocatorPriority = cfgapi.PriorityNormal
		// The reserved balloon prefers CPUs close to a
		// virtual device associated with ReservedResources
		// cpuset. This will make it unlikely to get those
		// CPUs allocated into any other balloons even if they
		// would be free, that is, MinCpus <
		// p.reserved.Size(). Explicitly defined
		// ReservedResources cpuset overrides any other
		// PreferCloseToDevices definition in the reserved
		// balloon type.
		reservedBalloonDef.PreferCloseToDevices = append([]string{virtDevReservedCpus}, reservedBalloonDef.PreferCloseToDevices...)
	case cfgapi.AmountQuantity:
		// ReservedResources.cpus defines number of
		// CPUs. Treat the value as a minimum size for the
		// reserved balloon, but the balloon is allowed to
		// grow larger.
		qty, err := amount.ParseQuantity()
		if err != nil {
			return nil, nil, balloonsError("failed to parse reserved CPU quantity '%s': %v", amount, err)
		}
		reserveCnt := (int(qty.MilliValue()) + 999) / 1000
		if reservedBalloonDef.MinCpus == 0 {
			reservedBalloonDef.MinCpus = reserveCnt
			if reservedBalloonDef.MaxCpus != 0 && reservedBalloonDef.MaxCpus < reservedBalloonDef.MinCpus {
				return nil, nil, balloonsError("mismatching reserved balloon maxCpus: %d and ReservedResources cpus: %d mCPU (implies minCpu %d)",
					reservedBalloonDef.MaxCpus, qty.MilliValue(), reservedBalloonDef.MinCpus)
			}
		}
		if reservedBalloonDef.MinCpus != reserveCnt {
			return nil, nil, balloonsError("mismatching reserved balloon minCpus: %d and ReservedResources cpus: %d mCPU",
				reservedBalloonDef.MinCpus, qty.MilliValue())
		}
		p.reserved = cpuset.New()
	}

	reservedBalloonDef.MinBalloons = 1
	reservedBalloonDef.MaxBalloons = 1
	reservedBalloonDef.Namespaces = append(reservedBalloonDef.Namespaces, metav1.NamespaceSystem)
	reservedBalloonDef.Namespaces = append(reservedBalloonDef.Namespaces, bpoptions.ReservedPoolNamespaces...)

	return reservedBalloonDef, defaultBalloonDef, nil
}

func (p *balloons) fillCloseToDevices(blnDefs []*BalloonDef) {
	for _, blnDef := range blnDefs {
		if blnDef.PreferIsolCpus {
			blnDef.PreferCloseToDevices = append(blnDef.PreferCloseToDevices, virtDevIsolatedCpus)
		}
		if blnDef.PreferCoreType == "performance" {
			blnDef.PreferCloseToDevices = append(blnDef.PreferCloseToDevices, virtDevPCores)
		}
		if blnDef.PreferCoreType == "efficient" {
			blnDef.PreferCloseToDevices = append(blnDef.PreferCloseToDevices, virtDevECores)
		}
	}
}

// fillFarFromDevices adds BalloonDefs implicit device anti-affinities
// towards devices that other BalloonDefs prefer to be close to.
func (p *balloons) fillFarFromDevices(blnDefs []*BalloonDef) {
	// devDefClose[device][blnDef.Name] equals true if and
	// only if the blnDef prefers to be close to the device.
	devDefClose := map[string]map[string]bool{}
	// avoidDevs is a list of devices for which at least one
	// balloon type prefers to be close to. The order of devices
	// in the avoidDevs list is significant: devices in the
	// beginning of the list will be more effectively avoided than
	// devices later in the list.
	avoidDevs := []string{}
	if p.options.System.Isolated().Size() != 0 {
		avoidDevs = append(avoidDevs, virtDevIsolatedCpus)
	}
	for _, blnDef := range blnDefs {
		for _, closeDev := range blnDef.PreferCloseToDevices {
			if _, ok := devDefClose[closeDev]; !ok {
				avoidDevs = append(avoidDevs, closeDev)
				devDefClose[closeDev] = map[string]bool{}
			}
			devDefClose[closeDev][blnDef.Name] = true
		}
	}
	// Add every device in avoidDev to PreferFarFromDevices lists
	// of those balloon types that do not prefer to be close to
	// the device.
	for _, avoidDev := range avoidDevs {
		for _, blnDef := range blnDefs {
			if !devDefClose[avoidDev][blnDef.Name] {
				blnDef.PreferFarFromDevices = append(blnDef.PreferFarFromDevices, avoidDev)
			}
		}
	}
	// Add virtual devices related to load classes to be avoided.
	for _, blnDef := range blnDefs {
		addedVirtDevs := map[string]struct{}{}
		for _, load := range blnDef.Loads {
			vdName := p.loadVirtDev[load].name
			if _, ok := addedVirtDevs[vdName]; ok {
				continue
			}
			blnDef.PreferFarFromDevices = append(blnDef.PreferFarFromDevices, vdName)
		}
	}
}

func (p *balloons) fillLoadVirtDevices(loadClasses []LoadClass) {
	// Convert *what* is loaded and *what* should be avoided,
	// described by the user in loadClasses, into *how* to model
	// the load internally (by virtual devices) and *how* to
	// communicate it to the CPU allocator.
	p.loadVirtDev = map[string]*loadClassVirtDev{}
	for _, lc := range loadClasses {
		virtDev := &loadClassVirtDev{
			name:                       "loaded-" + lc.Level.String(),
			level:                      lc.Level,
			updateOnEveryCpuAllocation: lc.OverloadsLevelInBalloon,
		}
		p.loadVirtDev[lc.Name] = virtDev
	}
}

// memTypeMaskFromStringList returns memory type mask corresponding a
// list of strings.
func memTypeMaskFromStringList(memTypes []string) (libmem.TypeMask, error) {
	mask := libmem.TypeMask(0)
	for _, typeString := range memTypes {
		memType, err := libmem.ParseType(typeString)
		if err != nil {
			return 0, err
		}
		mask |= memType.Mask()
	}
	return mask, nil
}

// closestMems returns memory node IDs good for pinning containers
// that run on given CPUs
func (p *balloons) closestMems(cpus cpuset.CPUSet) idset.IDSet {
	return idset.NewIDSet(p.memAllocator.CPUSetAffinity(cpus).Slice()...)
}

// resizeCompositeBalloon changes the CPUs allocated for all sub-components
func (p *balloons) resizeCompositeBalloon(bln *Balloon, newMilliCpus int) error {
	origFreeCpus := p.freeCpus.Clone()
	origCompBlnsCpus := []cpuset.CPUSet{}
	newMilliCpusPerComponent := newMilliCpus / len(bln.components)
	blnCpus := cpuset.New()
	for _, compBln := range bln.components {
		origCompBlnsCpus = append(origCompBlnsCpus, compBln.Cpus.Clone())
		if err := p.resizeBalloon(compBln, newMilliCpusPerComponent); err != nil {
			p.freeCpus = origFreeCpus
			for i, origCompBlnCpus := range origCompBlnsCpus {
				bln.components[i].Cpus = origCompBlnCpus
			}
			return balloonsError("resize composite balloon %s: %w", bln.PrettyName(), err)
		}
		blnCpus = blnCpus.Union(compBln.Cpus)
	}
	p.forgetCpuClass(bln) // reset CPU classes in balloon's old CPUs
	bln.Cpus = blnCpus
	log.Debugf("- resize composite ballooon successful: %s, freecpus: %#s", bln, p.freeCpus)
	p.updatePinning(bln)
	if err := p.useCpuClass(bln); err != nil { // set CPU classes in balloon's new CPUs
		log.Warnf("failed to apply CPU class to balloon %s: %v", bln.PrettyName(), err)
	}
	return nil
}

// resizeBalloon changes the CPUs allocated for a balloon, if allowed.
func (p *balloons) resizeBalloon(bln *Balloon, newMilliCpus int) error {
	if len(bln.components) > 0 {
		return p.resizeCompositeBalloon(bln, newMilliCpus)
	}
	oldCpuCount := bln.Cpus.Size()
	newCpuCount := (newMilliCpus + 999) / 1000
	if bln.Def.MaxCpus > NoLimit && newCpuCount > bln.Def.MaxCpus {
		newCpuCount = bln.Def.MaxCpus
	}
	if bln.Def.MinCpus > 0 && newCpuCount < bln.Def.MinCpus {
		newCpuCount = bln.Def.MinCpus
	}
	log.Debugf("resize %s to fit %d mCPU", bln, newMilliCpus)
	log.Debugf("- change size from %d to %d full cpus", oldCpuCount, newCpuCount)
	log.Debugf("- free cpus: %q", p.freeCpus)
	if oldCpuCount == newCpuCount {
		return nil
	}
	cpuCountDelta := newCpuCount - oldCpuCount
	p.forgetCpuClass(bln)
	defer func() {
		if err := p.useCpuClass(bln); err != nil {
			log.Warnf("failed to apply CPU class to balloon %s: %v", bln.PrettyName(), err)
		}
	}()
	p.updateLoadedVirtDevsInAllocatorOptions(&bln.cpuTreeAlloc.options, bln.Def.Loads)
	if cpuCountDelta > 0 {
		// Inflate the balloon.
		addFromCpus, _, err := bln.cpuTreeAlloc.ResizeCpus(bln.Cpus, p.freeCpus, cpuCountDelta)
		if err != nil {
			return balloonsError("resize/inflate: failed to choose a cpuset for allocating additional %d CPUs: %w", cpuCountDelta, err)
		}
		log.Debugf("- allocating %d CPUs from %q", cpuCountDelta, addFromCpus)
		newCpus, err := p.cpuAllocator.AllocateCpus(&addFromCpus, newCpuCount-oldCpuCount, bln.Def.AllocatorPriority.Value().Option())
		if err != nil {
			return balloonsError("resize/inflate: allocating %d CPUs for %s failed: %w", cpuCountDelta, bln, err)
		}
		oldBlnCpus := bln.Cpus
		oldFreeCpus := p.freeCpus
		p.freeCpus = p.freeCpus.Difference(newCpus)
		bln.Cpus = bln.Cpus.Union(newCpus)
		log.Debugf("- allocated, changed cpus: balloon from %q to %q, free from %q to %q", oldBlnCpus, bln.Cpus, oldFreeCpus, p.freeCpus)
		p.updatePinning(p.shareIdleCpus(p.freeCpus, newCpus)...)
	} else {
		// Deflate the balloon.
		_, removeFromCpus, err := bln.cpuTreeAlloc.ResizeCpus(bln.Cpus, p.freeCpus, cpuCountDelta)
		if err != nil {
			return balloonsError("resize/deflate: failed to choose a cpuset for releasing %d CPUs: %w", -cpuCountDelta, err)
		}
		log.Debugf("- releasing %d CPUs from cpuset %q", -cpuCountDelta, removeFromCpus)
		_, err = p.cpuAllocator.ReleaseCpus(&removeFromCpus, -cpuCountDelta, bln.Def.AllocatorPriority.Value().Option())
		if err != nil {
			return balloonsError("resize/deflate: releasing %d CPUs from %s failed: %w", -cpuCountDelta, bln, err)
		}
		oldBlnCpus := bln.Cpus
		oldFreeCpus := p.freeCpus
		p.freeCpus = p.freeCpus.Union(removeFromCpus)
		bln.Cpus = bln.Cpus.Difference(removeFromCpus)
		log.Debugf("- released, changed cpus: balloon from %q to %q, free from %q to %q", oldBlnCpus, bln.Cpus, oldFreeCpus, p.freeCpus)
		p.updatePinning(p.shareIdleCpus(removeFromCpus, cpuset.New())...)
	}
	log.Debugf("- resize successful: %s, freecpus: %#s", bln, p.freeCpus)
	p.updatePinning(bln)
	return nil
}

func (p *balloons) updatePinning(blns ...*Balloon) {
	for _, bln := range blns {
		var cpusNoHt cpuset.CPUSet
		var allowedCpus cpuset.CPUSet
		pinnableCpus := bln.Cpus.Union(bln.SharedIdleCpus)
		bln.Mems = p.closestMems(pinnableCpus)
		for _, cID := range bln.ContainerIDs() {
			if c, ok := p.cch.LookupContainer(cID); ok {
				if runWithoutHyperthreads(c, bln) {
					if cpusNoHt.Size() == 0 {
						cpusNoHt = p.cpuTree.system().SingleThreadForCPUs(pinnableCpus)
					}
					allowedCpus = cpusNoHt
				} else {
					allowedCpus = pinnableCpus
				}
				p.pinCpuMem(c, allowedCpus, bln.Mems, bln.memTypeMask, bln.Def.PinMemory)
			}
		}
	}
}

// runWithoutHyperthreads returns true if a container should run using
// only single hyperthread from each physical core.
func runWithoutHyperthreads(c cache.Container, bln *Balloon) bool {
	// Is balloon type configuration overridden by annotation?
	if value, ok := c.GetEffectiveAnnotation(hideHyperthreadsKey); ok {
		if hide, err := strconv.ParseBool(value); err == nil {
			return hide
		}
	}
	return bln.Def.HideHyperthreads != nil && *bln.Def.HideHyperthreads
}

// shareIdleCpus adds addCpus and removes removeCpus to those balloons
// that whose containers are allowed to use shared idle CPUs. Returns
// balloons that will need re-pinning.
func (p *balloons) shareIdleCpus(addCpus, removeCpus cpuset.CPUSet) []*Balloon {
	updateBalloons := map[int]struct{}{}
	if removeCpus.Size() > 0 {
		for blnIdx, bln := range p.balloons {
			if bln.SharedIdleCpus.Intersection(removeCpus).Size() > 0 {
				bln.SharedIdleCpus = bln.SharedIdleCpus.Difference(removeCpus)
				updateBalloons[blnIdx] = struct{}{}
			}
		}
	}
	addCpus = addCpus.Difference(p.options.System.Isolated())
	if addCpus.Size() > 0 {
		for blnIdx, bln := range p.balloons {
			topoLevel := bln.Def.ShareIdleCpusInSame
			if topoLevel == cfgapi.CPUTopologyLevelUndefined {
				continue
			}
			idleCpusInTopoLevel := cpuset.New()
			if err := p.cpuTree.DepthFirstWalk(func(t *cpuTreeNode) error {
				// Dive in correct topology level.
				if t.level != topoLevel {
					return nil
				}
				// Does the balloon include CPUs in the correct topology level?
				if t.cpus.Intersection(bln.Cpus).Size() > 0 {
					// Share idle CPUs on this level to this balloon.
					idleCpusInTopoLevel = idleCpusInTopoLevel.Union(t.cpus.Intersection(addCpus))
				}
				// Do not walk deeper than the correct level.
				return WalkSkipChildren
			}); err != WalkSkipChildren && err != WalkStop {
				log.Warnf("failed to walk CPU tree: %v", err)
			}
			if idleCpusInTopoLevel.Size() == 0 {
				continue
			}
			sharedBefore := bln.SharedIdleCpus.Size()
			bln.SharedIdleCpus = bln.SharedIdleCpus.Union(idleCpusInTopoLevel)
			sharedNow := bln.SharedIdleCpus.Size()
			if sharedBefore != sharedNow {
				log.Debugf("balloon %s shares %d new idle CPU(s) in %s(s), %d in total (%s)",
					bln.PrettyName(), sharedNow-sharedBefore,
					topoLevel, bln.SharedIdleCpus.Size(), bln.SharedIdleCpus)
				updateBalloons[blnIdx] = struct{}{}
			}
		}
	}
	updatedBalloons := make([]*Balloon, 0, len(updateBalloons))
	for blnIdx := range updateBalloons {
		updatedBalloons = append(updatedBalloons, p.balloons[blnIdx])
	}
	return updatedBalloons
}

// updateGroups updates the number of groups present in the balloon.
func (bln *Balloon) updateGroups(c cache.Container, delta int) {
	if bln.Def.GroupBy != "" {
		group, _ := c.Expand(bln.Def.GroupBy, false)
		bln.Groups[group] += delta
	}
}

// assignContainer adds a container to a balloon
func (p *balloons) assignContainer(c cache.Container, bln *Balloon) {
	log.Info("assigning container %s to balloon %s", c.PrettyName(), bln)
	podID := c.GetPodID()
	bln.PodIDs[podID] = append(bln.PodIDs[podID], c.GetID())
	bln.updateGroups(c, 1)
	p.updatePinning(bln)
}

// dismissContainer removes a container from a balloon
func (p *balloons) dismissContainer(c cache.Container, bln *Balloon) {
	if err := p.memAllocator.Release(c.GetID()); err != nil {
		log.Error("dismissContainer: failed to release memory for %s: %v", c.PrettyName(), err)
	}
	podID := c.GetPodID()
	bln.PodIDs[podID] = removeString(bln.PodIDs[podID], c.GetID())
	if len(bln.PodIDs[podID]) == 0 {
		delete(bln.PodIDs, podID)
	}
	bln.updateGroups(c, -1)
}

// pinCpuMem pins container to CPUs and memory nodes if flagged
func (p *balloons) pinCpuMem(c cache.Container, cpus cpuset.CPUSet, mems idset.IDSet, memTypeMask libmem.TypeMask, blnDefPinMemory *bool) {
	if p.bpoptions.PinCPU == nil || *p.bpoptions.PinCPU {
		log.Debug("  - pinning %s to cpuset: %s", c.PrettyName(), cpus)
		c.SetCpusetCpus(cpus.String())
		if reqCpu, ok := c.GetResourceRequirements().Requests[corev1.ResourceCPU]; ok {
			mCpu := int(reqCpu.MilliValue())
			c.SetCPUShares(int64(cache.MilliCPUToShares(int64(mCpu))))
		}
	}
	// Start from policy-level PinMemory...
	pinMemory := p.bpoptions.PinMemory == nil || *p.bpoptions.PinMemory
	// ...and allow override in balloon-type-level PinMemory
	if blnDefPinMemory != nil {
		pinMemory = *blnDefPinMemory
	}
	if pinMemory {
		if c.PreserveMemoryResources() {
			log.Debug("  - preserving %s pinning to memory %q", c.PrettyName, c.GetCpusetMems())
			preserveMems, err := parseIDSet(c.GetCpusetMems())
			if err != nil {
				log.Error("failed to parse CpusetMems: %v", err)
			} else {
				zone := p.allocMem(c, preserveMems, 0, true)
				log.Debug("  - allocated preserved memory %s", c.PrettyName, zone)
				c.SetCpusetMems(zone.MemsetString())
			}
		} else {
			effMemTypeMask, err := c.MemoryTypes()
			if err != nil {
				log.Error("%v", err)
			}
			if effMemTypeMask != 0 {
				// memory-type pod/container-specific
				// annotation overrides balloon's
				// memory options that are the default
				// to all containers in the balloon.
				log.Debug("  - %s memory-type annotation mask %s overrides balloon mems %s and mask %s", c.PrettyName(), effMemTypeMask, mems, memTypeMask)
			} else {
				effMemTypeMask = memTypeMask
			}
			log.Debug("  - requested %s to memory %s (types %s)", c.PrettyName(), mems, effMemTypeMask)
			zone := p.allocMem(c, mems, effMemTypeMask, false)
			log.Debug("  - allocated %s to memory %s", c.PrettyName(), zone)
			c.SetCpusetMems(zone.MemsetString())
		}
	}
}

func (p *balloons) allocMem(c cache.Container, mems idset.IDSet, types libmem.TypeMask, preserve bool) libmem.NodeMask {
	var (
		amount  = getMemoryLimit(c)
		nodes   = libmem.NewNodeMask(mems.Members()...)
		req     *libmem.Request
		zone    libmem.NodeMask
		updates map[string]libmem.NodeMask
		err     error
	)

	if _, ok := p.memAllocator.AssignedZone(c.GetID()); !ok {
		if preserve {
			req = libmem.PreservedContainer(
				c.GetID(),
				c.PrettyName(),
				amount,
				nodes,
			)
		} else {
			req = libmem.ContainerWithTypes(
				c.GetID(),
				c.PrettyName(),
				string(c.GetQOSClass()),
				amount,
				nodes,
				types,
			)
		}
		zone, updates, err = p.memAllocator.Allocate(req)
	} else {

		zone, updates, err = p.memAllocator.Realloc(c.GetID(), nodes, types)
	}

	if err != nil {
		log.Error("allocMem: falling back to %s, failed to allocate memory for %s: %v",
			nodes, c.PrettyName(), err)
		return nodes
	}

	for oID, oz := range updates {
		if oc, ok := p.cch.LookupContainer(oID); ok {
			oc.SetCpusetMems(oz.MemsetString())
		}
	}

	return zone
}

func parseIDSet(mems string) (idset.IDSet, error) {
	cset, err := cpuset.Parse(mems)
	if err != nil {
		return idset.NewIDSet(), err
	}
	return idset.NewIDSet(cset.List()...), nil
}

func getMemoryLimit(c cache.Container) int64 {
	res, ok := c.GetResourceUpdates()
	if !ok {
		res = c.GetResourceRequirements()
	}

	if limit, ok := res.Limits[corev1.ResourceMemory]; ok {
		return limit.Value()
	}

	return 0
}

// balloonsError formats an error from this policy.
func balloonsError(format string, args ...interface{}) error {
	return fmt.Errorf(PolicyName+": "+format, args...)
}

// removeString returns the first occurrence of a string from string slice.
func removeString(strings []string, element string) []string {
	for index, s := range strings {
		if s == element {
			strings[index] = strings[len(strings)-1]
			return strings[:len(strings)-1]
		}
	}
	return strings
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
