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

// We publish each NUMA node as a DRA device. Its capacity is the number of
// CPUs we can give out from that node. A claim requests a number of CPUs
// from one device.
//
// We allocate the CPUs of a claim as a grant, the same way as for a
// container. The grant has only exclusive CPUs: no shared part and no
// memory. Like other grants, it is accounted, kept across a reconfiguration
// and saved to the cache. The claimed CPUs leave the free supply, so the
// policy does not hand them out again.

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
	specs "tags.cncf.io/container-device-interface/specs-go"

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
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
	// draEnvPrefix is a prefix for environment variables listing the CPUs
	// assigned to a container by a claim: DRA_CPUSET_<claim UID>=<cpuset>.
	draEnvPrefix = "DRA_CPUSET_"
)

// draDevices returns one device per NUMA node. A node's capacity is its share
// of the CPUs an exclusive grant can draw from, isolated plus sharable,
// which leaves out the reserved ones.
func (p *policy) draDevices() []resourceapi.Device {
	var (
		grantable = grantableCPUs(p.root.GetSupply())
		smt       = p.sys.MaxThreadCount() > 1
		devices   []resourceapi.Device
	)

	for _, id := range p.sys.NodeIDs() {
		node := p.sys.Node(id)
		cpus := grantable.Intersection(node.CPUSet())
		if cpus.IsEmpty() {
			continue
		}

		one := resource.NewQuantity(1, resource.DecimalSI)
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
						Default: one,
						ValidRange: &resourceapi.CapacityRequestPolicyRange{
							Min:  one,
							Step: one,
						},
					},
				},
			},
			NodeAllocatableResources: map[v1.ResourceName]resourceapi.NodeAllocatableResource{
				v1.ResourceCPU: {
					Mapping: &resourceapi.NodeAllocatableMapping{
						CapacityKey:        ptr.To(draCapacityCPU),
						CapacityMultiplier: one,
					},
				},
			},
		})
	}

	return devices
}

func grantableCPUs(s Supply) cpuset.CPUSet {
	return s.IsolatedCPUs().Union(s.SharableCPUs()).Difference(s.ReservedCPUs())
}

// AllocateClaim allocates resources for a claim.
func (p *policy) AllocateClaim(
	claim *resourceapi.ResourceClaim,
	results []resourceapi.DeviceRequestAllocationResult,
) ([]specs.ContainerEdits, error) {
	uid := string(claim.UID)

	reqs, err := p.draRequests(results)
	if err != nil {
		return nil, fmt.Errorf("claim %s: %w", uid, err)
	}

	grant, ok := p.allocations.claims[uid]
	if !ok {
		grant, err = p.allocateClaim(reqs)
		if err != nil {
			return nil, fmt.Errorf("claim %s: %w", uid, err)
		}
		p.allocations.claims[uid] = grant
		p.saveAllocations()
		// Shared containers running on the claimed CPUs move off them.
		p.updateSharedAllocations(nil)
	}

	if err := checkClaimCPUs(grant.ExclusiveCPUs(), reqs); err != nil {
		return nil, fmt.Errorf("claim %s: %w", uid, err)
	}

	return claimEdits(claim.UID, grant.ExclusiveCPUs(), len(results)), nil
}

// checkClaimCPUs verifies that the claim holds the requested number of CPUs
// on every node.
func checkClaimCPUs(cpus cpuset.CPUSet, reqs []draRequest) error {
	consumed := map[int]int{}
	total := 0
	for _, req := range reqs {
		consumed[req.node.ID()] += req.cpus
		total += req.cpus
	}

	if cpus.Size() != total {
		return fmt.Errorf("holds %d CPUs but consumed %d", cpus.Size(), total)
	}
	for _, req := range reqs {
		id := req.node.ID()
		if held := cpus.Intersection(req.node.CPUSet()).Size(); held != consumed[id] {
			return fmt.Errorf("holds %d CPUs of node #%d but consumed %d", held, id, consumed[id])
		}
	}
	return nil
}

// ReleaseClaim releases the claim's CPUs.
func (p *policy) ReleaseClaim(uid types.UID) error {
	grant, ok := p.allocations.claims[string(uid)]
	if !ok {
		return nil
	}

	grant.Release()
	delete(p.allocations.claims, string(uid))
	p.saveAllocations()
	p.updateSharedAllocations(nil)

	return nil
}

// draRequest is what one allocation result requests: CPUs of a NUMA node.
type draRequest struct {
	node system.Node
	cpus int
}

// draRequests translates claim allocation results into draRequests.
func (p *policy) draRequests(results []resourceapi.DeviceRequestAllocationResult) ([]draRequest, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("no allocation results")
	}

	grantable := grantableCPUs(p.root.GetSupply())
	reqs := make([]draRequest, 0, len(results))
	for i, r := range results {
		node, ok := p.draNode(r.Device)
		if !ok {
			return nil, fmt.Errorf("result %d: unknown device %q", i, r.Device)
		}

		q, ok := r.ConsumedCapacity[draCapacityCPU]
		if !ok {
			return nil, fmt.Errorf("result %d consumed no %s", i, draCapacityCPU)
		}
		n, ok := q.AsInt64()
		if !ok || n < 1 {
			return nil, fmt.Errorf("result %d consumed %s %s, not a positive whole number",
				i, q.String(), draCapacityCPU)
		}
		if offered := node.CPUSet().Intersection(grantable).Size(); n > int64(offered) {
			return nil, fmt.Errorf("result %d consumed %d CPUs of node #%d, which offers %d",
				i, n, node.ID(), offered)
		}

		reqs = append(reqs, draRequest{node: node, cpus: int(n)})
	}
	return reqs, nil
}

// draNode returns the NUMA node identified by the device.
func (p *policy) draNode(device string) (system.Node, bool) {
	var id int
	if _, err := fmt.Sscanf(device, draDeviceFormat, &id); err != nil {
		return nil, false
	}
	if fmt.Sprintf(draDeviceFormat, id) != device {
		return nil, false
	}
	node := p.sys.Node(idset.ID(id))
	return node, node != nil
}

// allocateClaim grants a claim the requested CPUs. Each request picks
// from what the earlier ones left, so several requests for one node get
// disjoint CPUs. The grant is reserved from the deepest pool that has
// all of its CPUs.
func (p *policy) allocateClaim(reqs []draRequest) (Grant, error) {
	cpus := cpuset.New()
	for _, req := range reqs {
		picked, err := p.pickNodeCPUs(req.node, req.cpus, cpus)
		if err != nil {
			return nil, err
		}
		cpus = cpus.Union(picked)
	}

	pool := p.poolForCPUs(cpus)
	grant := newGrant(pool, nil, cpuNormal, "", cpus, 0, 0, nil, 0)
	if _, err := pool.FreeSupply().Reserve(grant, nil); err != nil {
		return nil, err
	}
	return grant, nil
}

// pickNodeCPUs picks n free CPUs of a NUMA node. It picks by topology,
// so two CPUs are the two threads of one core.
func (p *policy) pickNodeCPUs(node system.Node, n int, taken cpuset.CPUSet) (cpuset.CPUSet, error) {
	nodeCPUs := node.CPUSet().Intersection(grantableCPUs(p.root.GetSupply()))
	pool := p.poolForCPUs(nodeCPUs)

	free := pool.FreeSupply()
	sliceable, err := free.SliceableCPUs()
	if err != nil {
		return cpuset.New(), fmt.Errorf("node #%d: %w", node.ID(), err)
	}
	from := free.IsolatedCPUs().Union(sliceable).Difference(free.ReservedCPUs()).Intersection(nodeCPUs).Difference(taken)
	if from.Size() < n {
		return cpuset.New(), fmt.Errorf("node #%d: %d CPUs requested, only %d free",
			node.ID(), n, from.Size())
	}

	cpus, err := p.cpuAllocator.AllocateCpus(&from, n, defaultPrio.Option())
	if err != nil {
		return cpuset.New(), fmt.Errorf("node #%d: %w", node.ID(), err)
	}
	return cpus, nil
}

// poolForCPUs returns the deepest pool whose supply has all of the CPUs. The
// tree does not always have a pool per NUMA node: a single node collapses to
// the root, a shallow tree stops at the socket, and L3 cache pools can sit
// below a node.
func (p *policy) poolForCPUs(cpus cpuset.CPUSet) Node {
	pool := p.root
	if cpus.IsEmpty() {
		return pool
	}

	for deeper := true; deeper; {
		deeper = false
		for _, child := range pool.Children() {
			if cpus.IsSubsetOf(grantableCPUs(child.GetSupply())) {
				pool, deeper = child, true
				break
			}
		}
	}
	return pool
}

// claimEdits returns one edit per result, each setting
// DRA_CPUSET_<claim UID>=<cpuset> to the claim's CPUs.
func claimEdits(uid types.UID, cpus cpuset.CPUSet, results int) []specs.ContainerEdits {
	env := fmt.Sprintf("%s%s=%s", draEnvPrefix, uid, cpus.String())
	edits := make([]specs.ContainerEdits, results)
	for i := range edits {
		edits[i].Env = []string{env}
	}
	return edits
}
