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

// We publish our NUMA nodes as DRA devices with consumable capacity. A claim
// selects a node by attribute and asks for a number of CPUs, and we grant it
// CPUs of that node, picked by topology from the node's pool the way an
// exclusive grant is. Claimed CPUs leave the free supply of every pool, so
// the NRI path cannot hand them out while the claim lives. The claiming
// container is not pinned to them yet.

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

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
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
	// draEnvPrefix starts the variable telling a container which CPUs its
	// claim got, DRA_CPUSET_<claim UID>=<cpuset>.
	draEnvPrefix = "DRA_CPUSET_"
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

// grantableCPUs returns the CPUs of a supply an exclusive grant or a claim can
// draw from: isolated plus sharable, less reserved. The sole reserved CPU is
// allowed to also be isolated, so it can show up in IsolatedCPUs() too.
func grantableCPUs(s Supply) cpuset.CPUSet {
	return s.IsolatedCPUs().Union(s.SharableCPUs()).Difference(s.ReservedCPUs())
}

// AllocateClaim gives a claim CPUs of the NUMA nodes its results name, as
// many as each result consumed. A claim we already hold CPUs for gets the
// same edits again. This is the last line of defense against handing a CPU
// to two claims: we pick only CPUs no grant and no claim holds.
func (p *policy) AllocateClaim(
	claim *resourceapi.ResourceClaim,
	results []resourceapi.DeviceRequestAllocationResult,
) ([]specs.ContainerEdits, error) {
	uid := string(claim.UID)

	reqs, err := p.draRequests(results)
	if err != nil {
		return nil, fmt.Errorf("claim %s: %w", uid, err)
	}

	cpus, ok := p.draClaims[uid]
	if !ok {
		cpus, err = p.pickClaimCPUs(reqs)
		if err != nil {
			return nil, fmt.Errorf("claim %s: %w", uid, err)
		}
		p.draClaims[uid] = cpus
		// Shared containers running on the claimed CPUs move off them.
		p.updateSharedAllocations(nil)
	}

	if err := checkClaimCPUs(cpus, reqs); err != nil {
		return nil, fmt.Errorf("claim %s: %w", uid, err)
	}

	return claimEdits(claim.UID, cpus, len(results)), nil
}

// checkClaimCPUs checks that the CPUs a claim holds are what its results
// consumed, node by node, so that a re-prepare naming other nodes is refused
// rather than answered with CPUs of the nodes it named before.
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

// ReleaseClaim gives the CPUs of a claim back. A claim we do not know is
// nothing to release: the kubelet may unprepare a claim more than once.
func (p *policy) ReleaseClaim(uid types.UID) error {
	cpus, ok := p.draClaims[string(uid)]
	if !ok {
		return nil
	}

	p.unclaimCPUs(cpus)
	delete(p.draClaims, string(uid))
	p.updateSharedAllocations(nil)

	return nil
}

// draRequest is what one allocation result asks of us: CPUs of a NUMA node.
type draRequest struct {
	node system.Node
	cpus int
}

// draRequests decodes the results of a claim: the device names the NUMA node,
// and the consumed capacity is how many of its CPUs the result took.
func (p *policy) draRequests(results []resourceapi.DeviceRequestAllocationResult) ([]draRequest, error) {
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
		if n > int64(node.CPUSet().Size()) {
			return nil, fmt.Errorf("result %d consumed %d CPUs of node #%d, which has %d",
				i, n, node.ID(), node.CPUSet().Size())
		}

		reqs = append(reqs, draRequest{node: node, cpus: int(n)})
	}
	return reqs, nil
}

// draNode returns the NUMA node a device name stands for.
func (p *policy) draNode(device string) (system.Node, bool) {
	for _, id := range p.sys.NodeIDs() {
		if fmt.Sprintf(draDeviceFormat, id) == device {
			return p.sys.Node(id), true
		}
	}
	return nil, false
}

// pickClaimCPUs picks the CPUs the requests ask for, node by node, and takes
// them out of the free supply. Each request picks from what the earlier ones
// left, so several results on one node get disjoint CPUs. On failure nothing
// stays claimed.
func (p *policy) pickClaimCPUs(reqs []draRequest) (cpuset.CPUSet, error) {
	picked := cpuset.New()
	for _, req := range reqs {
		cpus, err := p.pickNodeCPUs(req.node, req.cpus)
		if err != nil {
			p.unclaimCPUs(picked)
			return cpuset.New(), err
		}
		picked = picked.Union(cpus)
	}
	return picked, nil
}

// pickNodeCPUs picks n free CPUs of a NUMA node and claims them. It picks by
// topology, as an exclusive grant does, so two CPUs are the two threads of
// one core rather than one thread of each of two cores.
func (p *policy) pickNodeCPUs(node system.Node, n int) (cpuset.CPUSet, error) {
	nodeCPUs := node.CPUSet().Intersection(grantableCPUs(p.root.GetSupply()))
	pool := p.poolForCPUs(nodeCPUs)

	// As for an exclusive grant, sharable CPUs are only ours to pick as far
	// as the shared containers of each pool below can spare them.
	free := pool.FreeSupply()
	sliceable, err := free.SliceableCPUs()
	if err != nil {
		return cpuset.New(), fmt.Errorf("node #%d: %w", node.ID(), err)
	}
	from := free.IsolatedCPUs().Union(sliceable).Difference(free.ReservedCPUs()).Intersection(nodeCPUs)
	if from.Size() < n {
		return cpuset.New(), fmt.Errorf("node #%d: %d CPUs requested, only %d free",
			node.ID(), n, from.Size())
	}

	cpus, err := p.cpuAllocator.AllocateCpus(&from, n, defaultPrio.Option())
	if err != nil {
		return cpuset.New(), fmt.Errorf("node #%d: %w", node.ID(), err)
	}

	if err := p.claimCPUs(cpus); err != nil {
		return cpuset.New(), err
	}
	return cpus, nil
}

// poolForCPUs returns the deepest pool whose supply has all of the CPUs. The
// tree does not always have a pool per NUMA node: a single node collapses to
// the root, a shallow tree stops at the socket, and L3 cache pools can sit
// below a node. So neither the node nor a leaf is the right pool to pick from.
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

// claimCPUs takes CPUs out of the free supply of every pool, so that no grant
// gets them until unclaimCPUs gives them back. Sharable CPUs taken this way
// shrink what shared containers run on, so, as for an exclusive grant, they
// must not overcommit the shared portions already granted.
func (p *policy) claimCPUs(cpus cpuset.CPUSet) error {
	pool := p.poolForCPUs(cpus)
	free := pool.FreeSupply()

	if !cpus.IsSubsetOf(grantableCPUs(free)) {
		return fmt.Errorf("CPUs %s are not free in %s",
			cpus.Difference(grantableCPUs(free)), pool.Name())
	}
	if shared := cpus.Intersection(free.SharableCPUs()); free.AllocatableSharedCPU(true) < 1000*shared.Size() {
		return fmt.Errorf("claiming CPUs %s would overcommit the shared CPUs of %s",
			shared, pool.Name())
	}

	p.root.DepthFirst(func(n Node) bool {
		n.FreeSupply().ClaimCPUs(cpus)
		return false
	})
	return nil
}

// unclaimCPUs puts CPUs a claim held back into the free supply of every pool.
func (p *policy) unclaimCPUs(cpus cpuset.CPUSet) {
	p.root.DepthFirst(func(n Node) bool {
		n.FreeSupply().UnclaimCPUs(cpus)
		return false
	})
}

// reclaimCPUs takes the CPUs of every live claim out of a rebuilt pool tree.
// A claim's CPUs cannot move, so a configuration under which they are no
// longer ours to give is refused.
func (p *policy) reclaimCPUs() error {
	for uid, cpus := range p.draClaims {
		if err := p.claimCPUs(cpus); err != nil {
			return fmt.Errorf("DRA claim %s: %w", uid, err)
		}
	}
	return nil
}

// claimEdits tells a container the CPUs of its claim, one edit per result.
// Every edit sets the same variable to the same value: a container given
// several results keeps the value of the last one, as CDI cannot merge the
// values of a variable, and here they are all the same.
func claimEdits(uid types.UID, cpus cpuset.CPUSet, results int) []specs.ContainerEdits {
	env := fmt.Sprintf("%s%s=%s", draEnvPrefix, uid, cpus.String())
	edits := make([]specs.ContainerEdits, results)
	for i := range edits {
		edits[i].Env = []string{env}
	}
	return edits
}
