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
	"path/filepath"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/balloons"
	"github.com/containers/nri-plugins/pkg/cpuallocator"
	"github.com/containers/nri-plugins/pkg/kubernetes"
	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	cpucontrol "github.com/containers/nri-plugins/pkg/resmgr/control/cpu"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	policy "github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/utils"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// PolicyName is the name of this policy.
	PolicyName = "balloons"
	// PolicyDescription is a short description of this policy.
	PolicyDescription = "Flexible pools with per-pool CPU parameters"

	// balloonKey is a pod annotation key, the value is a pod balloon name.
	balloonKey = "balloon." + PolicyName + "." + kubernetes.ResmgrKeyNamespace
	// reservedBalloonDefName is the name in the reserved balloon definition.
	reservedBalloonDefName = "reserved"
	// defaultBalloonDefName is the name in the default balloon definition.
	defaultBalloonDefName = "default"
	// NoLimit value denotes no limit being set.
	NoLimit = 0
)

// balloons contains configuration and runtime attributes of the balloons policy
type balloons struct {
	options          *policy.BackendOptions // configuration common to all policies
	bpoptions        *BalloonsOptions       // balloons-specific configuration
	cch              cache.Cache            // nri-resource-policy cache
	allowed          cpuset.CPUSet          // bounding set of CPUs we're allowed to use
	reserved         cpuset.CPUSet          // system-/kube-reserved CPUs
	freeCpus         cpuset.CPUSet          // CPUs to be included in growing or new ballons
	cpuTree          *cpuTreeNode           // system CPU topology
	cpuTreeAllocator *cpuTreeAllocator      // CPU allocator from system CPU topology

	reservedBalloonDef *BalloonDef // built-in definition of the reserved balloon
	defaultBalloonDef  *BalloonDef // built-in definition of the default balloon
	balloons           []*Balloon  // balloon instances: reserved, default and user-defined

	cpuAllocator cpuallocator.CPUAllocator // CPU allocator used by the policy
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
	PodIDs           map[string][]string
	cpuTreeAllocator *cpuTreeAllocator
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
	if bln.Def.MaxCpus == NoLimit {
		return (bln.Cpus.Size() + freeCpus.Size()) * 1000
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

	log.Info("setting up %s policy...", PolicyName)
	if p.cpuTree, err = NewCpuTreeFromSystem(); err != nil {
		log.Errorf("creating CPU topology tree failed: %s", err)
	}
	log.Debug("CPU topology: %s", p.cpuTree)

	// Handle common policy options: AvailableResources and ReservedResources.
	// p.allowed: CPUs available for the policy
	amount, kind := bpoptions.AvailableResources.Get(cfgapi.CPU)
	switch kind {
	case cfgapi.AmountCPUSet:
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return balloonsError("failed to parse available CPU cpuset '%s': %w", amount, err)
		}
		p.allowed = cset
	case cfgapi.AmountQuantity:
		return balloonsError("can't handle CPU resources given as resource.Quantity (%v)", amount)
	case cfgapi.AmountAbsent:
		// Available CPUs not specified, default to all on-line CPUs.
		p.allowed = policyOptions.System.CPUSet().Difference(policyOptions.System.Offlined())
	}

	// p.reserved: CPUs reserved for kube-system pods, subset of p.allowed.
	amount, kind = bpoptions.ReservedResources.Get(cfgapi.CPU)
	switch kind {
	case cfgapi.AmountCPUSet:
		cset, err := amount.ParseCPUSet()
		if err != nil {
			return balloonsError("failed to parse reserved CPU cpuset '%s': %v", amount, err)
		}
		p.reserved = p.allowed.Intersection(cset)
	case cfgapi.AmountQuantity:
		qty, err := amount.ParseQuantity()
		if err != nil {
			return balloonsError("failed to parse reserved CPU quantity '%s': %v", amount, err)
		}
		reserveCnt := (int(qty.MilliValue()) + 999) / 1000
		cpus, err := p.cpuAllocator.AllocateCpus(&p.allowed, reserveCnt, cpuallocator.PriorityNone)
		if err != nil {
			return balloonsError("failed to allocate reserved CPUs: %s", err)
		}
		p.reserved = cpus
		p.allowed = p.allowed.Union(cpus)
	}
	if p.reserved.IsEmpty() {
		return balloonsError("%s cannot run without reserved CPUs that are also AvailableResources", PolicyName)
	}
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
		p.resizeBalloon(bln, max(1, reqMilliCpus))
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
			p.resizeBalloon(bln, 0)
			log.Debug("all containers removed, free balloon allocation %s", bln.PrettyName())
			p.freeBalloon(bln)
		} else {
			// Make sure that the balloon will have at
			// least 1 CPU to run remaining containers.
			p.resizeBalloon(bln, max(1, p.requestedMilliCpus(bln)))
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
func (b *balloons) GetTopologyZones() []*policy.TopologyZone {
	return nil
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

// balloonsByNamespace returns balloons that contain containers in a
// namespace.
func (p *balloons) balloonsByNamespace(namespace string) []*Balloon {
	blns := []*Balloon{}
	for _, bln := range p.balloons {
		for podID, ctrIDs := range bln.PodIDs {
			if len(ctrIDs) == 0 {
				continue
			}
			pod, ok := p.cch.LookupPod(podID)
			if !ok {
				continue
			}
			if pod.GetNamespace() == namespace {
				blns = append(blns, bln)
				break
			}
		}
	}
	return blns
}

// balloonsByPod returns balloons that contain any container of a pod.
func (p *balloons) balloonsByPod(pod cache.Pod) []*Balloon {
	podID := pod.GetID()
	blns := []*Balloon{}
	for _, bln := range p.balloons {
		if _, ok := bln.PodIDs[podID]; ok {
			blns = append(blns, bln)
		}
	}
	return blns
}

// balloonsByDef returns list of balloons instantiated from a balloon
// definition.
func (p *balloons) balloonsByDef(blnDef *BalloonDef) []*Balloon {
	balloons := []*Balloon{}
	for _, balloon := range p.balloons {
		if balloon.Def == blnDef {
			balloons = append(balloons, balloon)
		}
	}
	return balloons
}

// balloonDefByName returns a balloon definition with a name.
func (p *balloons) balloonDefByName(defName string) *BalloonDef {
	if defName == "reserved" {
		return p.reservedBalloonDef
	}
	if defName == "default" {
		return p.defaultBalloonDef
	}
	for _, blnDef := range p.bpoptions.BalloonDefs {
		if blnDef.Name == defName {
			return blnDef
		}
	}
	return nil
}

func (p *balloons) chooseBalloonDef(c cache.Container) (*BalloonDef, error) {
	var blnDef *BalloonDef
	// BalloonDef is defined by annotation?
	if blnDefName, ok := c.GetEffectiveAnnotation(balloonKey); ok {
		blnDef = p.balloonDefByName(blnDefName)
		if blnDef == nil {
			return nil, balloonsError("no balloon for annotation %q", blnDefName)
		}
		return blnDef, nil
	}

	// BalloonDef is defined by a special namespace (kube-system +
	// ReservedPoolNamespaces)?
	if namespaceMatches(c.GetNamespace(), append(p.bpoptions.ReservedPoolNamespaces, metav1.NamespaceSystem)) {
		return p.balloons[0].Def, nil
	}

	// BalloonDef is defined by the namespace.
	for _, blnDef := range append([]*BalloonDef{p.reservedBalloonDef, p.defaultBalloonDef}, p.bpoptions.BalloonDefs...) {
		if namespaceMatches(c.GetNamespace(), blnDef.Namespaces) {
			return blnDef, nil
		}
	}

	// Fallback to the default balloon.
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

// largest helps finding the largest element and value in a slice.
// Input the length of a slice and a function that returns the
// magnitude of given element in the slice as int.
func largest(sliceLen int, valueOf func(i int) int) (int, int) {
	largestIndex := -1
	largestValue := 0
	for index := 0; index < sliceLen; index++ {
		value := valueOf(index)
		if largestIndex == -1 || value > largestValue {
			largestIndex = index
			largestValue = value
		}
	}
	return largestIndex, largestValue
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
	cpucontrol.Assign(p.cch, p.bpoptions.IdleCpuClass, p.allowed.UnsortedList()...)
	log.Debugf("reset class of available cpus: %q (reserved: %q)", p.allowed, p.reserved)
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
	cpucontrol.Assign(p.cch, bln.Def.CpuClass, bln.Cpus.UnsortedList()...)
	log.Debugf("apply class %q on CPUs %q", bln.Def.CpuClass, bln.Cpus)
	return nil
}

// forgetCpuClass is called when CPUs of a balloon are released from duty.
func (p *balloons) forgetCpuClass(bln *Balloon) {
	// Use p.IdleCpuClass for bln.Cpus.
	// Usual inputs: see useCpuClass
	cpucontrol.Assign(p.cch, p.bpoptions.IdleCpuClass, bln.Cpus.UnsortedList()...)
	log.Debugf("forget class %q of cpus %q", bln.Def.CpuClass, bln.Cpus)
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
	// Configure new cpuTreeAllocator for this balloon if there
	// are type specific allocator options, otherwise use policy
	// default allocator.
	cpuTreeAllocator := p.cpuTreeAllocator
	if blnDef.AllocatorTopologyBalancing != nil || blnDef.PreferSpreadOnPhysicalCores != nil || len(blnDef.PreferCloseToDevices) > 0 || len(blnDef.PreferFarFromDevices) > 0 {
		allocatorOptions := cpuTreeAllocatorOptions{
			topologyBalancing:           p.bpoptions.AllocatorTopologyBalancing,
			preferSpreadOnPhysicalCores: p.bpoptions.PreferSpreadOnPhysicalCores,
			preferCloseToDevices:        blnDef.PreferCloseToDevices,
			preferFarFromDevices:        blnDef.PreferFarFromDevices,
		}
		if blnDef.AllocatorTopologyBalancing != nil {
			allocatorOptions.topologyBalancing = *blnDef.AllocatorTopologyBalancing
		}
		if blnDef.PreferSpreadOnPhysicalCores != nil {
			allocatorOptions.preferSpreadOnPhysicalCores = *blnDef.PreferSpreadOnPhysicalCores
		}
		cpuTreeAllocator = p.cpuTree.NewAllocator(allocatorOptions)
	}

	// Allocate CPUs
	if blnDef == p.reservedBalloonDef ||
		(blnDef == p.defaultBalloonDef && blnDef.MinCpus == 0 && blnDef.MaxCpus == 0) {
		// The reserved balloon uses ReservedResources CPUs.
		// So does the default balloon unless its CPU counts are tweaked.
		cpus = p.reserved
	} else {
		addFromCpus, _, err := cpuTreeAllocator.ResizeCpus(cpuset.New(), p.freeCpus, blnDef.MinCpus)
		if err != nil {
			return nil, balloonsError("failed to choose a cpuset for allocating MinCpus: %d from free cpus %q", blnDef.MinCpus, p.freeCpus)
		}
		cpus, err = p.cpuAllocator.AllocateCpus(&addFromCpus, blnDef.MinCpus, blnDef.AllocatorPriority.Value())
		if err != nil {
			return nil, balloonsError("could not allocate %d MinCpus for balloon %s[%d]: %w", blnDef.MinCpus, blnDef.Name, freeInstance, err)
		}
		p.freeCpus = p.freeCpus.Difference(cpus)
	}
	bln := &Balloon{
		Def:              blnDef,
		Instance:         freeInstance,
		PodIDs:           make(map[string][]string),
		Cpus:             cpus,
		SharedIdleCpus:   cpuset.New(),
		Mems:             p.closestMems(cpus),
		cpuTreeAllocator: cpuTreeAllocator,
	}
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
	p.cpuAllocator.ReleaseCpus(&bln.Cpus, bln.Cpus.Size(), bln.Def.AllocatorPriority.Value())
}

// freeBalloon clears a balloon and deletes it if allowed.
func (p *balloons) freeBalloon(bln *Balloon) {
	bln.PodIDs = make(map[string][]string)
	blnsSameDef := p.balloonsByDef(bln.Def)
	if len(blnsSameDef) > bln.Def.MinBalloons {
		p.deleteBalloon(bln)
	}
}

func (p *balloons) chooseBalloonInstance(blnDef *BalloonDef, fm FillMethod, c cache.Container) (*Balloon, error) {
	// If assigning to the reserved or the default balloon, fill
	// method is ignored: always fill the chosen balloon.
	if blnDef == p.balloons[0].Def {
		return p.balloons[0], nil
	}
	if blnDef == p.balloons[1].Def {
		return p.balloons[1], nil
	}
	reqMilliCpus := p.containerRequestedMilliCpus(c.GetID())
	// Handle fill methods that do not use existing instances of
	// balloonDef.
	switch fm {
	case FillReservedBalloon:
		return p.balloons[0], nil
	case FillDefaultBalloon:
		return p.balloons[1], nil
	case FillNewBalloon, FillNewBalloonMust:
		// Choosing an existing balloon without containers is
		// preferred over instantiating a new balloon.
		for _, bln := range p.balloonsByDef(blnDef) {
			if len(bln.PodIDs) == 0 {
				return bln, nil
			}
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
		return newBln, nil
	case FillSameNamespace:
		for _, bln := range p.balloonsByNamespace(c.GetNamespace()) {
			if bln.Def == blnDef && p.maxFreeMilliCpus(bln) >= reqMilliCpus {
				return bln, nil
			}
		}
		return nil, nil
	case FillSamePod:
		if pod, ok := c.GetPod(); ok {
			for _, bln := range p.balloonsByPod(pod) {
				if p.maxFreeMilliCpus(bln) >= reqMilliCpus {
					return bln, nil
				}
			}
			return nil, nil
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
		blnIdx, freeMilliCpus := largest(len(balloons), func(i int) int {
			return p.freeMilliCpus(balloons[i])
		})
		if freeMilliCpus >= reqMilliCpus {
			return balloons[blnIdx], nil
		}
	case FillBalancedInflate:
		// Are there balloons where the container would fit
		// after inflating the balloon?
		blnIdx, maxFreeMilliCpus := largest(len(balloons), func(i int) int {
			return p.maxFreeMilliCpus(balloons[i])
		})
		if maxFreeMilliCpus >= reqMilliCpus {
			return balloons[blnIdx], nil
		}
	default:
		return nil, balloonsError("balloon type fill method not implemented: %s", fm)
	}
	// No error, but balloon type remains undecided in this assign method.
	return nil, nil
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
	if blnDef == p.reservedBalloonDef {
		return p.balloons[0], nil
	}
	if blnDef == p.defaultBalloonDef {
		return p.balloons[1], nil
	}

	fillChain := []FillMethod{}
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
		bln, err := p.chooseBalloonInstance(blnDef, fillMethod, c)
		if err != nil {
			log.Debugf("fill method %q prevents allocation: %w", fillMethod, err)
			return nil, err
		}
		if bln == nil {
			log.Debugf("fill method %q not applicable", fillMethod)
			continue
		}
		log.Debugf("fill method %q suggests balloon instance %v", fillMethod, bln)
		return bln, nil
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

// getPodMilliCPU returns mCPUs requested by podID.
func (p *balloons) getPodMilliCPU(podID string) int64 {
	cpuRequested := int64(0)
	for _, c := range p.cch.GetContainers() {
		if c.GetPodID() == podID {
			if reqCpu, ok := c.GetResourceRequirements().Requests[corev1.ResourceCPU]; ok {
				cpuRequested += reqCpu.MilliValue()
			}
		}
	}
	return cpuRequested
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
	defer log.Debug("effective configuration:\n%s\n", utils.DumpJSON(p.bpoptions))
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
			p.resetCpuClass()
			for _, bln := range p.balloons {
				p.useCpuClass(bln)
			}
		}
		return nil
	}
	if err := p.setConfig(newBalloonsOptions); err != nil {
		log.Error("config update failed: %v", err)
		return err
	}
	log.Info("config updated successfully")
	p.Sync(p.cch.GetContainers(), p.cch.GetContainers())
	return nil
}

// applyBalloonDef creates user-defined balloons or reconfigures built-in
// balloons according to the blnDef. Does not initialize balloon CPUs.
func (p *balloons) applyBalloonDef(balloons *[]*Balloon, blnDef *BalloonDef, freeCpus *cpuset.CPUSet) error {
	if len(*balloons) < 2 {
		return balloonsError("internal error: reserved and default balloons missing, cannot apply balloon definitions")
	}
	reservedBalloon := (*balloons)[0]
	defaultBalloon := (*balloons)[1]
	// Every BalloonDef does one of the following:
	// 1. reconfigures the "reserved" balloon (most restricted)
	// 2. reconfigures the "default" balloon (somewhat restricted)
	// 3. defines new user-defined balloons.
	switch blnDef.Name {
	case "":
		// Case 0: bad name
		return balloonsError("undefined or empty balloon name")
	case reservedBalloon.Def.Name:
		// Case 1: reconfigure the "reserved" balloon.
		if blnDef.MinCpus != 0 {
			return balloonsError("cannot reconfigure the reserved balloon MinCpus, specified in ReservedResources CPUs")
		}
		if blnDef.MaxCpus != 0 {
			return balloonsError("cannot reconfigure the reserved balloon MaxCpus, specified in ReservedResources CPUs")
		}
		if blnDef.MinBalloons != 0 {
			return balloonsError("cannot reconfigure the reserved balloon MinBalloons")
		}
		p.reservedBalloonDef.AllocatorPriority = blnDef.AllocatorPriority
		p.reservedBalloonDef.CpuClass = blnDef.CpuClass
		p.reservedBalloonDef.Namespaces = blnDef.Namespaces
	case defaultBalloon.Def.Name:
		// Case 2: reconfigure the "default" balloon.
		defaultUsesReservedCpus := true
		if blnDef.MinCpus != 0 || blnDef.MaxCpus != 0 {
			defaultUsesReservedCpus = false
		}
		if blnDef.MinBalloons != 0 {
			return balloonsError("cannot reconfigure the default balloon MinBalloons")
		}
		p.defaultBalloonDef.MinCpus = blnDef.MinCpus
		p.defaultBalloonDef.MaxCpus = blnDef.MaxCpus
		p.defaultBalloonDef.AllocatorPriority = blnDef.AllocatorPriority
		p.defaultBalloonDef.CpuClass = blnDef.CpuClass
		p.defaultBalloonDef.Namespaces = blnDef.Namespaces
		if !defaultUsesReservedCpus {
			// Overwrite existing default balloon instance
			// that uses reserved CPUs with a balloon that
			// uses its own CPUs.
			newDefaultBln, err := p.newBalloon(p.defaultBalloonDef, false)
			if err != nil {
				return balloonsError("cannot create new default balloon: %w", err)
			}
			newDefaultBln.Instance = 0
			(*balloons)[1] = newDefaultBln
		}
	default:
		// Case 3: create minimum amount (MinBalloons) of each user-defined balloons.
		for allocPrio := cpuallocator.CPUPriority(0); allocPrio < cpuallocator.NumCPUPriorities; allocPrio++ {
			if blnDef.AllocatorPriority.Value() != allocPrio {
				continue
			}
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
		}
	}
	return nil
}

func (p *balloons) validateConfig(bpoptions *BalloonsOptions) error {
	for _, blnDef := range bpoptions.BalloonDefs {
		if blnDef.MaxCpus != NoLimit && blnDef.MinCpus > blnDef.MaxCpus {
			return balloonsError("MinCpus (%d) > MaxCpus (%d) in balloon type %q",
				blnDef.MinCpus, blnDef.MaxCpus, blnDef.Name)
		}
		if blnDef.MaxBalloons != NoLimit && blnDef.MinBalloons > blnDef.MaxBalloons {
			return balloonsError("MinBalloons (%d) > MaxBalloons (%d) in balloon type %q",
				blnDef.MinCpus, blnDef.MaxCpus, blnDef.Name)
		}
	}
	return nil
}

// setConfig takes new balloon configuration into use.
func (p *balloons) setConfig(bpoptions *BalloonsOptions) error {
	bpoptions = bpoptions.DeepCopy()
	setOmittedDefaults(bpoptions)

	// TODO: revert allocations (p.freeCpus) to old ones if the
	// configuration is invalid. Currently bad configuration
	// leaves a mess in bookkeeping.
	if err := p.validateConfig(bpoptions); err != nil {
		return balloonsError("invalid configuration: %w", err)
	}

	// Create the default reserved and default balloon
	// definitions. Some properties of these definitions may be
	// altered by user configuration.
	p.reservedBalloonDef = &BalloonDef{
		Name:              reservedBalloonDefName,
		MinBalloons:       1,
		AllocatorPriority: cfgapi.PriorityNone,
	}
	p.defaultBalloonDef = &BalloonDef{
		Name:              defaultBalloonDefName,
		MinBalloons:       1,
		AllocatorPriority: cfgapi.PriorityNone,
	}
	p.balloons = []*Balloon{}
	p.freeCpus = p.allowed.Clone()
	p.freeCpus = p.freeCpus.Difference(p.reserved)
	p.fillFarFromDevices(bpoptions.BalloonDefs)

	p.cpuTreeAllocator = p.cpuTree.NewAllocator(cpuTreeAllocatorOptions{
		topologyBalancing:           bpoptions.AllocatorTopologyBalancing,
		preferSpreadOnPhysicalCores: bpoptions.PreferSpreadOnPhysicalCores,
	})

	// We can't delay taking new configuration into use beyond this point,
	// because p.newBalloon() dereferences our options via p.bpoptions, so
	// it would end up using the old configuration.
	p.bpoptions = bpoptions

	// Instantiate built-in reserved and default balloons.
	reservedBalloon, err := p.newBalloon(p.reservedBalloonDef, false)
	if err != nil {
		return err
	}
	p.balloons = append(p.balloons, reservedBalloon)
	defaultBalloon, err := p.newBalloon(p.defaultBalloonDef, false)
	if err != nil {
		return err
	}
	p.balloons = append(p.balloons, defaultBalloon)
	// First apply customizations to built-in balloons: "reserved"
	// and "default".
	for _, blnDef := range bpoptions.BalloonDefs {
		if blnDef.Name != reservedBalloonDefName && blnDef.Name != defaultBalloonDefName {
			continue
		}
		if err := p.applyBalloonDef(&p.balloons, blnDef, &p.freeCpus); err != nil {
			return err
		}
	}
	// Apply all user balloon definitions, skip already customized
	// "reserved" and "default" balloons.
	for _, blnDef := range bpoptions.BalloonDefs {
		if blnDef.Name == reservedBalloonDefName || blnDef.Name == defaultBalloonDefName {
			continue
		}
		if err := p.applyBalloonDef(&p.balloons, blnDef, &p.freeCpus); err != nil {
			return err
		}
	}
	// Finish balloon instance initialization.
	log.Info("%s policy balloons:", PolicyName)
	for blnIdx, bln := range p.balloons {
		log.Info("- balloon %d: %s", blnIdx, bln)
	}
	p.updatePinning(p.shareIdleCpus(p.freeCpus, cpuset.New())...)
	// (Re)configures all CPUs in balloons.
	p.resetCpuClass()
	for _, bln := range p.balloons {
		p.useCpuClass(bln)
	}
	return nil
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
}

// closestMems returns memory node IDs good for pinning containers
// that run on given CPUs
func (p *balloons) closestMems(cpus cpuset.CPUSet) idset.IDSet {
	mems := idset.NewIDSet()
	sys := p.options.System
	for _, nodeID := range sys.NodeIDs() {
		if !cpus.Intersection(sys.Node(nodeID).CPUSet()).IsEmpty() {
			mems.Add(nodeID)
		}
	}
	return mems
}

// filterBalloons returns balloons for which the test function returns true
func filterBalloons(balloons []*Balloon, test func(*Balloon) bool) (ret []*Balloon) {
	for _, bln := range balloons {
		if test(bln) {
			ret = append(ret, bln)
		}
	}
	return
}

// availableMilliCPU returns mCPUs available in a balloon.
func (p *balloons) availableMilliCpus(balloon *Balloon) int64 {
	cpuAvail := int64(balloon.Cpus.Size() * 1000)
	cpuRequested := int64(0)
	for podID := range balloon.PodIDs {
		cpuRequested += p.getPodMilliCPU(podID)
	}
	return cpuAvail - cpuRequested
}

// resizeBalloon changes the CPUs allocated for a balloon, if allowed.
func (p *balloons) resizeBalloon(bln *Balloon, newMilliCpus int) error {
	if bln.Cpus.Equals(p.reserved) {
		log.Debugf("not resizing %s to %d mCPU, using fixed CPUs", bln, newMilliCpus)
		return nil
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
	defer p.useCpuClass(bln)
	if cpuCountDelta > 0 {
		// Inflate the balloon.
		addFromCpus, _, err := bln.cpuTreeAllocator.ResizeCpus(bln.Cpus, p.freeCpus, cpuCountDelta)
		if err != nil {
			return balloonsError("resize/inflate: failed to choose a cpuset for allocating additional %d CPUs: %w", cpuCountDelta, err)
		}
		log.Debugf("- allocating %d CPUs from %q", cpuCountDelta, addFromCpus)
		newCpus, err := p.cpuAllocator.AllocateCpus(&addFromCpus, newCpuCount-oldCpuCount, bln.Def.AllocatorPriority.Value())
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
		_, removeFromCpus, err := bln.cpuTreeAllocator.ResizeCpus(bln.Cpus, p.freeCpus, cpuCountDelta)
		if err != nil {
			return balloonsError("resize/deflate: failed to choose a cpuset for releasing %d CPUs: %w", -cpuCountDelta, err)
		}
		log.Debugf("- releasing %d CPUs from cpuset %q", -cpuCountDelta, removeFromCpus)
		_, err = p.cpuAllocator.ReleaseCpus(&removeFromCpus, -cpuCountDelta, bln.Def.AllocatorPriority.Value())
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
		cpus := bln.Cpus.Union(bln.SharedIdleCpus)
		bln.Mems = p.closestMems(cpus)
		for _, cID := range bln.ContainerIDs() {
			if c, ok := p.cch.LookupContainer(cID); ok {
				p.pinCpuMem(c, cpus, bln.Mems)
			}
		}
	}
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
	if addCpus.Size() > 0 {
		for blnIdx, bln := range p.balloons {
			topoLevel := bln.Def.ShareIdleCpusInSame
			if topoLevel == cfgapi.CPUTopologyLevelUndefined {
				continue
			}
			idleCpusInTopoLevel := cpuset.New()
			p.cpuTree.DepthFirstWalk(func(t *cpuTreeNode) error {
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
			})
			if idleCpusInTopoLevel.Size() == 0 {
				continue
			}
			sharedBefore := bln.SharedIdleCpus.Size()
			bln.SharedIdleCpus = bln.SharedIdleCpus.Union(idleCpusInTopoLevel)
			sharedNow := bln.SharedIdleCpus.Size()
			if sharedBefore != sharedNow {
				log.Debugf("balloon %d shares %d new idle CPU(s) in %s(s), %d in total (%s)",
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

// assignContainer adds a container to a balloon
func (p *balloons) assignContainer(c cache.Container, bln *Balloon) {
	log.Info("assigning container %s to balloon %s", c.PrettyName(), bln)
	// TODO: inflate the balloon (add CPUs / reconfigure balloons)
	// if necessary
	podID := c.GetPodID()
	bln.PodIDs[podID] = append(bln.PodIDs[podID], c.GetID())
	p.updatePinning(bln)
}

// dismissContainer removes a container from a balloon
func (p *balloons) dismissContainer(c cache.Container, bln *Balloon) {
	podID := c.GetPodID()
	bln.PodIDs[podID] = removeString(bln.PodIDs[podID], c.GetID())
	if len(bln.PodIDs[podID]) == 0 {
		delete(bln.PodIDs, podID)
	}
}

// pinCpuMem pins container to CPUs and memory nodes if flagged
func (p *balloons) pinCpuMem(c cache.Container, cpus cpuset.CPUSet, mems idset.IDSet) {
	if p.bpoptions.PinCPU == nil || *p.bpoptions.PinCPU {
		log.Debug("  - pinning %s to cpuset: %s", c.PrettyName(), cpus)
		c.SetCpusetCpus(cpus.String())
		if reqCpu, ok := c.GetResourceRequirements().Requests[corev1.ResourceCPU]; ok {
			mCpu := int(reqCpu.MilliValue())
			c.SetCPUShares(int64(cache.MilliCPUToShares(int64(mCpu))))
		}
	}
	if p.bpoptions.PinMemory == nil || *p.bpoptions.PinMemory {
		if c.PreserveMemoryResources() {
			log.Debug("  - preserving %s pinning to memory %q", c.PrettyName, c.GetCpusetMems())
		} else {
			log.Debug("  - pinning %s to memory %s", c.PrettyName(), mems)
			c.SetCpusetMems(mems.String())
		}
	}
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
