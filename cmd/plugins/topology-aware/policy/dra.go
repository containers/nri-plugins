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

package topologyaware

// We publish our NUMA nodes as DRA devices with consumable capacity. Once
// allocation is implemented, a claim will select a node by attribute and ask
// for a number of CPUs, and we will grant it CPUs of that node. For now,
// AllocateClaim and ReleaseClaim below are stubs: no claim can be granted yet.

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
	specs "tags.cncf.io/container-device-interface/specs-go"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
)

const (
	// draDeviceFormat names a device after the NUMA node it publishes.
	draDeviceFormat = "cpudevnuma%03d"
	// draCapacityCPU is the number of CPUs a claim consumes of a device.
	draCapacityCPU resourceapi.QualifiedName = "dra.cpu/cpu"
	// draAttrSocketID is the socket the NUMA node belongs to.
	draAttrSocketID resourceapi.QualifiedName = "dra.cpu/socketID"
	// draAttrSMTEnabled tells whether the CPUs have SMT enabled.
	draAttrSMTEnabled resourceapi.QualifiedName = "dra.cpu/smtEnabled"
)

// publishDRADevices publishes our NUMA nodes as DRA devices, unless they are
// the ones we published last. What we publish changes only when our allowed
// or reserved CPUs do, so a reconfiguration is usually a no-op here.
func (p *policy) publishDRADevices() error {
	devices := p.draDevices()

	// Semantic equality, because quantities compare by value, not by the
	// representation they were parsed from.
	if apiequality.Semantic.DeepEqual(devices, p.draPublished) {
		return nil
	}

	if err := p.options.Owner.PublishDRADevices(devices); err != nil {
		return err
	}
	p.draPublished = devices

	return nil
}

// draDevices returns one device per NUMA node we can grant CPUs from. A node's
// capacity is its share of the CPUs an exclusive grant can draw from, isolated
// plus sharable, which leaves out the reserved ones. It is a total and stays
// one: the scheduler subtracts what claims consume itself.
func (p *policy) draDevices() []resourceapi.Device {
	var (
		supply = p.root.GetSupply()
		// The sole reserved CPU is allowed to also be isolated, so it can
		// show up in IsolatedCPUs() too and must be excluded explicitly.
		grantable = supply.IsolatedCPUs().Union(supply.SharableCPUs()).
				Difference(supply.ReservedCPUs())
		smt     = p.sys.MaxThreadCount() > 1
		devices []resourceapi.Device
	)

	for _, id := range p.sys.NodeIDs() {
		node := p.sys.Node(id)
		cpus := grantable.Intersection(node.CPUSet())
		if cpus.IsEmpty() {
			continue
		}

		// Each device gets quantities of its own: a shared one caches the
		// string of its value when it is first printed.
		one := resource.MustParse("1")

		devices = append(devices, resourceapi.Device{
			Name:                     fmt.Sprintf(draDeviceFormat, id),
			AllowMultipleAllocations: ptr.To(true),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				deviceattribute.StandardDeviceAttributeNUMANode: {
					IntValue: ptr.To(int64(id)),
				},
				draAttrSocketID:   {IntValue: ptr.To(int64(node.PackageID()))},
				draAttrSMTEnabled: {BoolValue: ptr.To(smt)},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				draCapacityCPU: {
					Value: *resource.NewQuantity(int64(cpus.Size()), resource.DecimalSI),
					// Without a request policy a claim asking for no
					// amount would consume the whole node.
					RequestPolicy: &resourceapi.CapacityRequestPolicy{
						Default: &one,
						ValidRange: &resourceapi.CapacityRequestPolicyRange{
							Min:  &one,
							Step: &one,
						},
					},
				},
			},
			// Let the scheduler count claimed CPUs into the node's cpu
			// capacity. Pre-1.37 API servers drop this silently.
			NodeAllocatableResources: map[v1.ResourceName]resourceapi.NodeAllocatableResource{
				v1.ResourceCPU: {
					Mapping: &resourceapi.NodeAllocatableMapping{
						CapacityKey:        ptr.To(draCapacityCPU),
						CapacityMultiplier: &one,
					},
				},
			},
		})
	}

	return devices
}

// AllocateClaim allocates resources for a claim being prepared.
func (p *policy) AllocateClaim(
	*resourceapi.ResourceClaim,
	[]resourceapi.DeviceRequestAllocationResult,
) ([]specs.ContainerEdits, error) {
	return nil, policyapi.ErrNoDRAClaims
}

// ReleaseClaim releases the resources allocated for the given claim.
func (p *policy) ReleaseClaim(types.UID) error {
	return policyapi.ErrNoDRAClaims
}
