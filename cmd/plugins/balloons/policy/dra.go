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

package balloons

// This file publishes instances of balloon types with the dra option as
// DRA devices. A device has a consumable "cpu" capacity of maxCPUs. A
// prepared claim reserves the CPUs it consumed in the balloon of the
// device, and containers using the claim are placed in that balloon.

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/irq"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	specs "tags.cncf.io/container-device-interface/specs-go"
)

const (
	// draCapacityCPU is the consumable capacity of a balloon device: whole CPUs.
	draCapacityCPU resourceapi.QualifiedName = "cpu"
	// draAttrBalloonType is the device attribute naming the balloon type.
	draAttrBalloonType resourceapi.QualifiedName = "balloonType"
	// draAttrInstance is the device attribute holding the balloon instance index.
	draAttrInstance resourceapi.QualifiedName = "instance"
	// draEnvBalloon is the variable telling a container the device of its balloon.
	draEnvBalloon = "DRA_BALLOON"
	// draDefaultDeviceName is the device name template used when the configuration gives none.
	draDefaultDeviceName = "${balloonType}-${instance}"
	// keyDRAClaims is the cache policy entry holding the claim records.
	keyDRAClaims = "dra-claims"
)

// draClaim records what a claim was given, one entry per allocation result.
type draClaim struct {
	Devices []draClaimDevice `json:"devices"`
}

// draClaimDevice records one allocation result of a claim.
type draClaimDevice struct {
	Device      string `json:"device"`      // DRA device name
	BalloonType string `json:"balloonType"` // balloon definition name
	Instance    int    `json:"instance"`    // balloon instance index
	MilliCPUs   int    `json:"milliCPUs"`   // CPUs consumed from the balloon
}

// draClaimRecords maps claim UIDs to the records of prepared claims.
type draClaimRecords map[string]*draClaim

// Set replaces the records with the given cached value.
func (r *draClaimRecords) Set(value any) {
	*r = value.(draClaimRecords)
}

// Get returns the records as the value to cache.
func (r *draClaimRecords) Get() any {
	return *r
}

// milliCpusIn returns the CPUs the claim consumed from a balloon instance.
func (rec *draClaim) milliCpusIn(defName string, instance int) int {
	mcpus := 0
	for _, dev := range rec.Devices {
		if dev.BalloonType == defName && dev.Instance == instance {
			mcpus += dev.MilliCPUs
		}
	}
	return mcpus
}

// draDeviceName returns the DRA device name of a balloon instance.
func draDeviceName(blnDef *BalloonDef, instance int) string {
	template := draDefaultDeviceName
	if blnDef.DRA != nil && blnDef.DRA.DeviceName != "" {
		template = blnDef.DRA.DeviceName
	}
	return strings.NewReplacer(
		"${balloonType}", blnDef.Name,
		"${instance}", strconv.Itoa(instance),
	).Replace(template)
}

// draNodeAllocatable returns true if the devices of a balloon type map their cpu capacity to node allocatable CPU.
func draNodeAllocatable(blnDef *BalloonDef) bool {
	return blnDef.DRA != nil && (blnDef.DRA.NodeAllocatable == nil || *blnDef.DRA.NodeAllocatable)
}

// draBalloonByDevice returns the published balloon with a device name, or nil.
func (p *balloons) draBalloonByDevice(name string) *Balloon {
	for _, bln := range p.balloons {
		if bln.Def.DRA != nil && draDeviceName(bln.Def, bln.Instance) == name {
			return bln
		}
	}
	return nil
}

// draBalloonByInstance returns the published balloon of a type and instance, or nil.
func (p *balloons) draBalloonByInstance(defName string, instance int) *Balloon {
	for _, bln := range p.balloons {
		if bln.Def.DRA != nil && bln.Def.Name == defName && bln.Instance == instance {
			return bln
		}
	}
	return nil
}

// draDevices returns the devices of the DRA balloons, sorted by name.
func (p *balloons) draDevices() []resourceapi.Device {
	devices := []resourceapi.Device{}
	for _, bln := range p.balloons {
		if bln.Def.DRA == nil {
			continue
		}
		devices = append(devices, resourceapi.Device{
			Name:                     draDeviceName(bln.Def, bln.Instance),
			AllowMultipleAllocations: ptr.To(true),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				draAttrBalloonType: {StringValue: ptr.To(bln.Def.Name)},
				draAttrInstance:    {IntValue: ptr.To(int64(bln.Instance))},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				draCapacityCPU: {
					Value: *resource.NewQuantity(int64(bln.Def.MaxCpus), resource.DecimalSI),
				},
			},
		})
		if draNodeAllocatable(bln.Def) {
			devices[len(devices)-1].NodeAllocatableResources = map[corev1.ResourceName]resourceapi.NodeAllocatableResource{
				corev1.ResourceCPU: {
					Mapping: &resourceapi.NodeAllocatableMapping{
						CapacityKey:        ptr.To(draCapacityCPU),
						CapacityMultiplier: ptr.To(resource.MustParse("1")),
					},
				},
			}
		}
	}
	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Name < devices[j].Name
	})
	return devices
}

// publishDRADevices publishes every DRA balloon as a device.
func (p *balloons) publishDRADevices() error {
	if p.options == nil || p.options.Owner == nil {
		return nil
	}
	devices := p.draDevices()
	names := make([]string, 0, len(devices))
	for _, dev := range devices {
		names = append(names, dev.Name)
	}
	if err := p.options.Owner.PublishDRADevices(devices); err != nil {
		return balloonsError("failed to publish DRA devices %v: %w", names, err)
	}
	log.Infof("published DRA devices: %v", names)
	return nil
}

// loadDRAClaims returns the claim records stored in the cache.
func loadDRAClaims(cch cache.Cache) draClaimRecords {
	claims := draClaimRecords{}
	if !cch.GetPolicyEntry(keyDRAClaims, &claims) || claims == nil {
		return draClaimRecords{}
	}
	return claims
}

// saveDRAClaims stores the claim records in the cache.
func (p *balloons) saveDRAClaims() {
	p.cch.SetPolicyEntry(keyDRAClaims, p.draClaims)
}

// draClaimedMilliCpus returns the CPUs that claims hold in a balloon.
func (p *balloons) draClaimedMilliCpus(bln *Balloon) int {
	if bln.Def.DRA == nil {
		return 0
	}
	mcpus := 0
	for _, rec := range p.draClaims {
		mcpus += rec.milliCpusIn(bln.Def.Name, bln.Instance)
	}
	return mcpus
}

// draTargetMilliCpus returns the size a balloon needs for its containers and claims.
func (p *balloons) draTargetMilliCpus(bln *Balloon) int {
	if bln.ContainerCount() > 0 {
		return max(1, p.requestedMilliCpus(bln))
	}
	return p.requestedMilliCpus(bln)
}

// draGrowBalloon inflates a balloon to hold its containers and claims, and
// returns an error if the balloon cannot hold the CPUs of its claims.
func (p *balloons) draGrowBalloon(bln *Balloon) error {
	target := p.draTargetMilliCpus(bln)
	if bln.AvailMilliCpus() < target {
		if err := p.resizeBalloon(bln, target); err != nil {
			return err
		}
	}
	if claimed := p.draClaimedMilliCpus(bln); bln.AvailMilliCpus() < claimed {
		return balloonsError("balloon %s has %d mCPU but its claims hold %d mCPU",
			bln.PrettyName(), bln.AvailMilliCpus(), claimed)
	}
	return nil
}

// AllocateClaim reserves the CPUs that a claim consumed in the balloons of
// its devices, inflating the balloons when needed, and returns one container
// edit per allocation result.
func (p *balloons) AllocateClaim(
	claim *resourceapi.ResourceClaim,
	results []resourceapi.DeviceRequestAllocationResult,
) ([]specs.ContainerEdits, error) {
	irq.BlockWrites()
	defer irq.UnblockWrites()
	p.BlockMeters()
	defer p.UnblockMeters()
	defer p.commitCpuClasses()
	defer p.applyIrqAffinities()

	uid := string(claim.UID)
	if rec, ok := p.draClaims[uid]; ok {
		// A claim's allocation never changes, so a re-prepare gets
		// what the claim was given the first time.
		if len(rec.Devices) != len(results) {
			return nil, balloonsError("claim %s has %d recorded device(s) but %d allocation result(s)",
				uid, len(rec.Devices), len(results))
		}
		return draClaimEdits(rec), nil
	}

	if len(results) == 0 {
		return nil, balloonsError("claim %s has no allocation results", uid)
	}

	rec := &draClaim{}
	blns := []*Balloon{}
	for _, r := range results {
		bln := p.draBalloonByDevice(r.Device)
		if bln == nil {
			return nil, balloonsError("claim %s: device %q is not a balloon published by this node",
				uid, r.Device)
		}
		// A result without consumed capacity comes from a device that
		// was not shareable, so the claim got the whole device.
		mcpus := bln.Def.MaxCpus * 1000
		if q, ok := r.ConsumedCapacity[draCapacityCPU]; ok {
			mcpus = int(q.MilliValue())
			if mcpus <= 0 {
				return nil, balloonsError("claim %s: device %q consumed %s %s, expected a positive amount",
					uid, r.Device, q.String(), draCapacityCPU)
			}
		}
		rec.Devices = append(rec.Devices, draClaimDevice{
			Device:      r.Device,
			BalloonType: bln.Def.Name,
			Instance:    bln.Instance,
			MilliCPUs:   mcpus,
		})
		if !slices.Contains(blns, bln) {
			blns = append(blns, bln)
		}
	}

	p.draClaims[uid] = rec
	grown := []*Balloon{}
	for _, bln := range blns {
		oldCpus := bln.Cpus.Size()
		err := p.draGrowBalloon(bln)
		if bln.Cpus.Size() != oldCpus {
			grown = append(grown, bln)
		}
		if err == nil {
			continue
		}
		// Undo the reservation: the kubelet retries the claim later.
		delete(p.draClaims, uid)
		for _, g := range grown {
			if rerr := p.resizeBalloon(g, p.draTargetMilliCpus(g)); rerr != nil {
				log.Warnf("failed to deflate balloon %s after failed claim %s: %v",
					g.PrettyName(), uid, rerr)
			}
		}
		return nil, balloonsError("claim %s: not enough CPUs for %d mCPU in balloon %s: %w",
			uid, rec.milliCpusIn(bln.Def.Name, bln.Instance), bln.PrettyName(), err)
	}

	p.saveDRAClaims()
	for _, bln := range blns {
		log.Infof("DRA claim %s allocated %d mCPU in balloon %s",
			uid, rec.milliCpusIn(bln.Def.Name, bln.Instance), bln)
	}
	return draClaimEdits(rec), nil
}

// draClaimEdits returns one container edit per recorded device.
func draClaimEdits(rec *draClaim) []specs.ContainerEdits {
	edits := make([]specs.ContainerEdits, len(rec.Devices))
	for i, dev := range rec.Devices {
		edits[i].Env = []string{draEnvBalloon + "=" + dev.Device}
	}
	return edits
}

// ReleaseClaim releases the CPUs a claim holds and deflates its balloons.
func (p *balloons) ReleaseClaim(uid types.UID) error {
	irq.BlockWrites()
	defer irq.UnblockWrites()
	p.BlockMeters()
	defer p.UnblockMeters()
	defer p.commitCpuClasses()
	defer p.applyIrqAffinities()

	rec, ok := p.draClaims[string(uid)]
	if !ok {
		return nil
	}
	delete(p.draClaims, string(uid))
	p.saveDRAClaims()

	blns := []*Balloon{}
	for _, dev := range rec.Devices {
		if bln := p.draBalloonByInstance(dev.BalloonType, dev.Instance); bln != nil && !slices.Contains(blns, bln) {
			blns = append(blns, bln)
		}
	}
	for _, bln := range blns {
		if err := p.resizeBalloon(bln, p.draTargetMilliCpus(bln)); err != nil {
			log.Warnf("failed to deflate balloon %s after releasing claim %s: %v",
				bln.PrettyName(), uid, err)
		}
		log.Infof("DRA claim %s released %d mCPU in balloon %s",
			uid, rec.milliCpusIn(bln.Def.Name, bln.Instance), bln)
	}
	return nil
}

// applyDRAClaims resizes DRA balloons to hold the CPUs of the recorded claims.
func (p *balloons) applyDRAClaims() error {
	blns := []*Balloon{}
	for _, uid := range slices.Sorted(maps.Keys(p.draClaims)) {
		for _, dev := range p.draClaims[uid].Devices {
			bln := p.draBalloonByInstance(dev.BalloonType, dev.Instance)
			if bln == nil {
				return balloonsError("claim %s holds balloon %s[%d] which does not exist",
					uid, dev.BalloonType, dev.Instance)
			}
			if !slices.Contains(blns, bln) {
				blns = append(blns, bln)
			}
		}
	}
	for _, bln := range blns {
		if err := p.draGrowBalloon(bln); err != nil {
			return balloonsError("failed to restore DRA claims in balloon %s: %w", bln.PrettyName(), err)
		}
		log.Infof("DRA claims hold %d mCPU in balloon %s", p.draClaimedMilliCpus(bln), bln)
	}
	return nil
}

// draContainerClaims returns the sorted UIDs of the claims of this policy
// that a container uses, found from its CDI devices.
func (p *balloons) draContainerClaims(c cache.Container) []string {
	uids := []string{}
	for _, name := range c.GetCDIDevices() {
		driver, uid, ok := dra.ClaimOfCDIDevice(name)
		if !ok || driver != p.draDriver || slices.Contains(uids, string(uid)) {
			continue
		}
		uids = append(uids, string(uid))
	}
	sort.Strings(uids)
	return uids
}

// draContainerMilliCpus returns the share of a container in a balloon of the
// CPUs of its claims. A claim used by several containers of the same pod in
// the balloon is divided equally between them.
func (p *balloons) draContainerMilliCpus(c cache.Container, bln *Balloon) int {
	mcpus := 0
	for _, uid := range p.draContainerClaims(c) {
		rec, ok := p.draClaims[uid]
		if !ok {
			continue
		}
		users := 0
		for _, cID := range bln.PodIDs[c.GetPodID()] {
			if ctr, ok := p.cch.LookupContainer(cID); ok && slices.Contains(p.draContainerClaims(ctr), uid) {
				users++
			}
		}
		mcpus += rec.milliCpusIn(bln.Def.Name, bln.Instance) / max(1, users)
	}
	return mcpus
}

// draBalloonForContainer returns the balloon a DRA container must be placed
// in, or an error describing the contradiction in its requests.
func (p *balloons) draBalloonForContainer(c cache.Container, claimUIDs []string) (*Balloon, error) {
	type balloonID struct {
		defName  string
		instance int
	}
	ids := []balloonID{}
	devices := []string{}
	var firstDev draClaimDevice
	for _, uid := range claimUIDs {
		rec, ok := p.draClaims[uid]
		if !ok {
			return nil, fmt.Errorf("container %s uses claim %s that this policy has not prepared",
				c.PrettyName(), uid)
		}
		for _, dev := range rec.Devices {
			id := balloonID{dev.BalloonType, dev.Instance}
			if len(ids) == 0 {
				firstDev = dev
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
			if !slices.Contains(devices, dev.Device) {
				devices = append(devices, dev.Device)
			}
		}
	}
	switch {
	case len(ids) == 0:
		return nil, fmt.Errorf("container %s uses claims %v that hold no DRA devices",
			c.PrettyName(), claimUIDs)
	case len(ids) > 1:
		return nil, fmt.Errorf("container %s requests DRA devices of more than one balloon: %s",
			c.PrettyName(), strings.Join(devices, ", "))
	}

	bln := p.draBalloonByInstance(ids[0].defName, ids[0].instance)
	if bln == nil {
		return nil, fmt.Errorf("container %s: claim of DRA device %q holds balloon %s[%d] which does not exist",
			c.PrettyName(), firstDev.Device, ids[0].defName, ids[0].instance)
	}

	if blnDefName, ok := c.GetEffectiveAnnotation(balloonKey); ok && blnDefName != bln.Def.Name {
		return nil, fmt.Errorf("container %s: pod annotation %s requests balloon type %q but its DRA device %q is of type %q",
			c.PrettyName(), balloonKey, blnDefName, firstDev.Device, bln.Def.Name)
	}

	return bln, nil
}

// validateDRAConfig returns an error if the DRA options of balloon types
// are invalid or if the configuration does not provide the balloons that
// recorded claims hold.
func (p *balloons) validateDRAConfig(bpoptions *BalloonsOptions) error {
	defByName := func(name string) *BalloonDef {
		for _, blnDef := range bpoptions.BalloonDefs {
			if blnDef.Name == name {
				return blnDef
			}
		}
		return nil
	}

	deviceNames := map[string]string{} // device name -> balloon type
	for _, blnDef := range bpoptions.BalloonDefs {
		for _, comp := range blnDef.Components {
			if compDef := defByName(comp.DefName); compDef != nil && compDef.DRA != nil {
				return balloonsError("balloon type %q: dra type cannot be a component of composite balloon type %q",
					compDef.Name, blnDef.Name)
			}
		}
		if blnDef.DRA == nil {
			continue
		}
		switch {
		case blnDef.Name == reservedBalloonDefName || blnDef.Name == defaultBalloonDefName:
			return balloonsError("balloon type %q: dra is not allowed in built-in balloon types", blnDef.Name)
		case blnDef.MaxCpus <= 0:
			return balloonsError("balloon type %q: dra requires maxCPUs > 0", blnDef.Name)
		case blnDef.MinBalloons < 1:
			return balloonsError("balloon type %q: dra requires minBalloons >= 1", blnDef.Name)
		case len(blnDef.Namespaces) > 0:
			return balloonsError("balloon type %q: dra does not allow namespaces", blnDef.Name)
		case len(blnDef.MatchExpressions) > 0:
			return balloonsError("balloon type %q: dra does not allow matchExpressions", blnDef.Name)
		}
		if blnDef.MaxBalloons > blnDef.MinBalloons {
			log.Warnf("balloon type %q: dra publishes only minBalloons (%d) instances, maxBalloons (%d) has no effect",
				blnDef.Name, blnDef.MinBalloons, blnDef.MaxBalloons)
		}
		for instance := 0; instance < blnDef.MinBalloons; instance++ {
			name := draDeviceName(blnDef, instance)
			if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
				return balloonsError("balloon type %q: dra device name %q of instance %d is not a DNS label: %s",
					blnDef.Name, name, instance, strings.Join(errs, "; "))
			}
			if other, ok := deviceNames[name]; ok {
				return balloonsError("balloon type %q: dra device name %q is already used by balloon type %q",
					blnDef.Name, name, other)
			}
			deviceNames[name] = blnDef.Name
		}
	}

	type balloonID struct {
		defName  string
		instance int
	}
	claimed := map[balloonID]int{}
	for _, uid := range slices.Sorted(maps.Keys(p.draClaims)) {
		for _, dev := range p.draClaims[uid].Devices {
			blnDef := defByName(dev.BalloonType)
			if blnDef == nil || blnDef.DRA == nil || dev.Instance >= blnDef.MinBalloons {
				return balloonsError("cannot apply configuration: claim %s holds balloon %s[%d] which the new configuration does not provide",
					uid, dev.BalloonType, dev.Instance)
			}
			claimed[balloonID{dev.BalloonType, dev.Instance}] += dev.MilliCPUs
		}
	}
	for id, mcpus := range claimed {
		if maxCpus := defByName(id.defName).MaxCpus; mcpus > maxCpus*1000 {
			return balloonsError("cannot apply configuration: claims hold %d mCPU in balloon %s[%d] but its maxCPUs is %d",
				mcpus, id.defName, id.instance, maxCpus)
		}
	}
	return nil
}
