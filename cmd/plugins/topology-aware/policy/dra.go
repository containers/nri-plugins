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
// A container holding claims runs on their CPUs, and also on its own CPUs
// if it has a CPU request. A container naming only some requests of a claim
// runs on the CPUs of those requests.
//
// A claim or its device class may set a memory policy for the containers
// holding the claim, in a TopologyAwareClaimConfig. A config applies to the
// results of the requests it names. A config naming no requests applies to
// all results.

import (
	"fmt"
	"slices"
	"strings"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
	specs "tags.cncf.io/container-device-interface/specs-go"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

const (
	// draNUMADeviceFormat names a device after the NUMA node it publishes.
	draNUMADeviceFormat = "cpudevnuma%03d"
	// draSocketDeviceFormat names a device after the socket it publishes.
	draSocketDeviceFormat = "cpudevsocket%03d"
	// draCapacityCPU is the number of CPUs a claim consumes of a device.
	draCapacityCPU resourceapi.QualifiedName = "dra.cpu/cpu"
	// draAttrSocketID is the socket the CPUs of a device belong to.
	draAttrSocketID resourceapi.QualifiedName = "dra.cpu/socketID"
	// draAttrSMTEnabled tells whether the CPUs have SMT enabled.
	draAttrSMTEnabled resourceapi.QualifiedName = "dra.cpu/smtEnabled"
	// draAttrLevel is the level of the topology a device publishes.
	draAttrLevel resourceapi.QualifiedName = "dra.cpu/level"
	// draAttrNumCPUs is the number of CPUs of a device, used or not.
	draAttrNumCPUs resourceapi.QualifiedName = "dra.cpu/numCPUs"
	// draLevelNUMANode and draLevelSocket are the levels of our devices.
	draLevelNUMANode = "numanode"
	draLevelSocket   = "socket"
	// draConfigAPIVersion and draConfigKind identify our claim configs.
	draConfigAPIVersion = "config.nri/v1alpha1"
	draConfigKind       = "TopologyAwareClaimConfig"
)

// draDriverName is the name of our DRA driver.
var draDriverName = dra.DriverName(PolicyName)

// claim is a DRA claim we allocated. It has a grant of all of its CPUs, and
// the CPUs, the device and the memory policy of each of its allocation
// results, in result order. mpols is nil if no result has a memory policy.
type claim struct {
	Grant
	results []cpuset.CPUSet
	devices []string
	mpols   []*cfgapi.MemoryPolicy
}

// draDevices returns one device per NUMA node and one per socket which has
// CPUs for claims.
func (p *policy) draDevices() []resourceapi.Device {
	var devices []resourceapi.Device

	for _, id := range p.sys.NodeIDs() {
		node := p.sys.Node(id)
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			deviceattribute.StandardDeviceAttributeNUMANode: {
				IntValue: ptr.To(int64(id)),
			},
		}
		name := fmt.Sprintf(draNUMADeviceFormat, id)
		if d, ok := p.draDevice(name, draLevelNUMANode, node.PackageID(), attrs); ok {
			devices = append(devices, d)
		}
	}
	for _, id := range p.sys.PackageIDs() {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
		name := fmt.Sprintf(draSocketDeviceFormat, id)
		if d, ok := p.draDevice(name, draLevelSocket, id, attrs); ok {
			devices = append(devices, d)
		}
	}

	return devices
}

// draDevice returns the named device with the attributes of its level and
// socket added to attrs, or false if it has no CPUs for claims.
func (p *policy) draDevice(name, level string, socket idset.ID, attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) (resourceapi.Device, bool) {
	cpus, _ := p.draDeviceCPUs(name)
	if cpus.IsEmpty() {
		return resourceapi.Device{}, false
	}
	capacity := p.draCapacity(name, cpus)
	if capacity == 0 {
		return resourceapi.Device{}, false
	}

	attrs[draAttrLevel] = resourceapi.DeviceAttribute{StringValue: ptr.To(level)}
	attrs[draAttrSocketID] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(socket))}
	attrs[draAttrSMTEnabled] = resourceapi.DeviceAttribute{BoolValue: ptr.To(p.sys.MaxThreadCount() > 1)}
	attrs[draAttrNumCPUs] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(cpus.Size()))}
	roots := p.sys.Package(socket).PCIeRoots()
	if len(roots) > 0 && p.options.Owner.DRAListAttributesSupported() {
		attrs[deviceattribute.StandardDeviceAttributePCIeRoot] = resourceapi.DeviceAttribute{
			StringValues: roots,
		}
	}

	return resourceapi.Device{
		Name:                     name,
		AllowMultipleAllocations: ptr.To(true),
		Attributes:               attrs,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			draCapacityCPU: draCPUCapacity(capacity),
		},
		NodeAllocatableResources: map[v1.ResourceName]resourceapi.NodeAllocatableResource{
			v1.ResourceCPU: {
				Mapping: &resourceapi.NodeAllocatableMapping{
					CapacityKey:        ptr.To(draCapacityCPU),
					CapacityMultiplier: resource.NewQuantity(1, resource.DecimalSI),
				},
			},
		},
	}, true
}

// draCapacity returns the number of CPUs of a device which claims hold or could
// get now: the isolated and sharable CPUs, minus the reserved ones, minus the
// ones containers hold exclusively, minus the ones shared containers need,
// minus the ones of claims on smaller devices inside it.
func (p *policy) draCapacity(device string, cpus cpuset.CPUSet) int {
	free, err := p.claimableCPUs(cpus)
	if err != nil {
		log.Errorf("failed to find the free CPUs of DRA device %s: %v", device, err)
	}

	// The scheduler subtracts what claims consume of the device from its
	// capacity, so the CPUs of its own claims are part of it. So are the CPUs
	// of claims on larger devices: those claims are not bound to the CPUs of
	// this device.
	for _, c := range p.allocations.claims {
		for i, held := range c.results {
			if i >= len(c.devices) {
				continue
			}
			if c.devices[i] != device {
				larger, ok := p.draDeviceCPUs(c.devices[i])
				if !ok || !cpus.IsSubsetOf(larger) || cpus.Equals(larger) {
					continue
				}
			}
			free = free.Union(held.Intersection(cpus))
		}
	}
	return free.Size()
}

// publishDRADevices publishes our DRA devices if DRA is enabled, logging a
// failure.
func (p *policy) publishDRADevices() {
	if !p.options.Owner.DRAEnabled() {
		return
	}
	if err := p.options.Owner.PublishDRADevices(p.draDevices()); err != nil {
		log.Errorf("failed to publish DRA devices: %v", err)
	}
}

// draCPUCapacity returns a capacity of n CPUs, which claims consume in whole
// CPUs, one CPU if they ask for no amount.
func draCPUCapacity(n int) resourceapi.DeviceCapacity {
	one := resource.NewQuantity(1, resource.DecimalSI)
	// Without a request policy a claim asking for no amount would consume
	// the whole device.
	policy := &resourceapi.CapacityRequestPolicy{
		Default:    one,
		ValidRange: &resourceapi.CapacityRequestPolicyRange{Min: one},
	}
	// The API server rejects a step which does not fit in the capacity. With
	// one CPU, the minimum alone leaves only one whole CPU to ask for.
	if n > 1 {
		policy.ValidRange.Step = one
	}
	return resourceapi.DeviceCapacity{
		Value:         *resource.NewQuantity(int64(n), resource.DecimalSI),
		RequestPolicy: policy,
	}
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
	mpols, err := p.claimMemoryPolicies(results, dra.ClaimConfigs(claim, draDriverName))
	if err != nil {
		return nil, fmt.Errorf("claim %s: %w", uid, err)
	}

	c, ok := p.allocations.claims[uid]
	if !ok {
		c, err = p.allocateClaim(reqs)
		if err != nil {
			return nil, fmt.Errorf("claim %s: %w", uid, err)
		}
		c.mpols = mpols
		p.allocations.claims[uid] = c
		p.saveAllocations()
		// Shared containers running on the claimed CPUs move off them.
		p.updateSharedAllocations(nil)
	}

	if err := checkClaimCPUs(c, reqs); err != nil {
		return nil, fmt.Errorf("claim %s: %w", uid, err)
	}
	// The claim's configs do not change after it is allocated.
	if !slices.EqualFunc(c.mpols, mpols, sameMemoryPolicy) {
		return nil, fmt.Errorf("claim %s holds memory policies %v, but its configs give %v", uid, c.mpols, mpols)
	}

	// Containers find their CPUs in the resources we export to them.
	return nil, nil
}

// checkClaimCPUs verifies that every result of the claim holds the requested
// number of CPUs of its device.
func checkClaimCPUs(c *claim, reqs []draRequest) error {
	if len(c.results) != len(reqs) || len(c.devices) != len(reqs) {
		return fmt.Errorf("holds %d results but has %d", len(c.results), len(reqs))
	}
	for i, req := range reqs {
		if c.devices[i] != req.device {
			return fmt.Errorf("result %d holds CPUs of device %s, not of %s", i, c.devices[i], req.device)
		}
		if held := c.results[i]; held.Size() != req.cpus || !held.IsSubsetOf(req.deviceCPUs) {
			return fmt.Errorf("result %d holds CPUs %s but consumed %d CPUs of device %s", i, held, req.cpus, req.device)
		}
	}
	return nil
}

// ReleaseClaim releases the claim's CPUs.
func (p *policy) ReleaseClaim(uid types.UID) error {
	c, ok := p.allocations.claims[string(uid)]
	if !ok {
		return nil
	}

	// The kubelet unprepares a claim only after all of its containers have
	// stopped, and their grants went with them, so no grant holds the
	// claim's CPUs by now.
	c.Release()
	delete(p.allocations.claims, string(uid))
	p.saveAllocations()
	p.updateSharedAllocations(nil)

	return nil
}

// containerClaims returns the CPUs and the memory policy of the claim results
// a container holds. It ignores results we do not know, with a warning for
// an unknown claim, and fails if the results have different memory
// policies.
func (p *policy) containerClaims(c cache.Container) (cpuset.CPUSet, *cfgapi.MemoryPolicy, error) {
	var (
		cpus = cpuset.New()
		mpol *cfgapi.MemoryPolicy
		from types.UID
	)
	for uid, indexes := range dra.ContainerClaims(c, draDriverName) {
		claim, ok := p.allocations.claims[string(uid)]
		if !ok {
			log.Warnf("%s holds unknown DRA claim %s", c.PrettyName(), uid)
			continue
		}
		for _, i := range indexes {
			if i >= len(claim.results) {
				continue
			}
			cpus = cpus.Union(claim.results[i])
			if i >= len(claim.mpols) || claim.mpols[i] == nil {
				continue
			}
			if mpol == nil {
				mpol, from = claim.mpols[i], uid
			} else if !mpol.DeepEqual(claim.mpols[i]) {
				return cpuset.New(), nil, fmt.Errorf("memory policy %s of claim %s conflicts with %s of claim %s",
					claim.mpols[i], uid, mpol, from)
			}
		}
	}
	return cpus, mpol, nil
}

// claimMemoryPolicies returns the memory policies of the claim results, or nil
// if there are none. A later config overrides an earlier one.
func (p *policy) claimMemoryPolicies(results []resourceapi.DeviceRequestAllocationResult,
	configs []resourceapi.DeviceAllocationConfiguration) ([]*cfgapi.MemoryPolicy, error) {
	var mpols []*cfgapi.MemoryPolicy
	for i, config := range configs {
		cfg, err := decodeClaimConfig(config.Opaque.Parameters.Raw)
		if err != nil {
			return nil, fmt.Errorf("config %d: %w", i, err)
		}
		if cfg.MemoryPolicy == nil {
			continue
		}
		mpol, err := cfg.MemoryPolicy.ToMemoryPolicy()
		if err != nil {
			return nil, fmt.Errorf("config %d: %w", i, err)
		}
		// The nodes are resolved later, for each container. Resolving them
		// here with no CPUs only checks the value.
		nodes, err := libmem.MemoryPolicyNodes(p.sys, mpol.Mode, mpol.Nodes, cpuset.New(), libmem.NewNodeMask())
		if err != nil {
			return nil, fmt.Errorf("config %d: %w", i, err)
		}
		// Unset nodes mean allowed-mems. We keep the unset form, write node
		// lists in one form, and sort the flags, so the policies of two
		// claims compare equal.
		switch {
		case mpol.Nodes == "allowed-mems":
			mpol.Nodes = ""
		case mpol.Nodes != "" && mpol.Nodes[0] >= '0' && mpol.Nodes[0] <= '9':
			// The kernel refuses a node list with no memory node of the host.
			// With relative-nodes the list holds indices, not node IDs.
			if !slices.Contains(mpol.Flags, "MPOL_F_RELATIVE_NODES") &&
				nodes.And(libmem.NewNodeMask(p.getAllMems().Members()...)).Size() == 0 {
				return nil, fmt.Errorf("config %d: memory policy nodes %q has no memory node of this host", i, mpol.Nodes)
			}
			mpol.Nodes = nodes.MemsetString()
		}
		slices.Sort(mpol.Flags)
		mpol.Flags = slices.Compact(mpol.Flags)
		for j, r := range results {
			if !configApplies(config.Requests, r.Request) {
				continue
			}
			if mpols == nil {
				mpols = make([]*cfgapi.MemoryPolicy, len(results))
			}
			mpols[j] = mpol
		}
	}
	return mpols, nil
}

// sameMemoryPolicy tells whether two memory policies are the same. Two nil
// policies are the same.
func sameMemoryPolicy(a, b *cfgapi.MemoryPolicy) bool {
	return a == b || a != nil && a.DeepEqual(b)
}

// decodeClaimConfig decodes the parameters of a claim config.
func decodeClaimConfig(data []byte) (*cfgapi.TopologyAwareClaimConfig, error) {
	cfg := &cfgapi.TopologyAwareClaimConfig{}
	if err := yaml.UnmarshalStrict(data, cfg); err != nil {
		return nil, err
	}
	if cfg.APIVersion != draConfigAPIVersion || cfg.Kind != draConfigKind {
		return nil, fmt.Errorf("got apiVersion %q and kind %q, expected %q and %q",
			cfg.APIVersion, cfg.Kind, draConfigAPIVersion, draConfigKind)
	}
	return cfg, nil
}

// configApplies tells whether a config applies to a result of request.
// requests are the requests the config names. A config naming no requests
// applies to all results. A config naming a request also applies to its
// subrequests.
func configApplies(requests []string, request string) bool {
	main, _, _ := strings.Cut(request, "/")
	return len(requests) == 0 || slices.Contains(requests, request) || slices.Contains(requests, main)
}

// applyMemoryPolicy sets the memory policy of a new container to the policy of
// its claims, with the nodes resolved for the container's CPUs and memory
// nodes.
func (p *policy) applyMemoryPolicy(c cache.Container, mpol *cfgapi.MemoryPolicy, cpus cpuset.CPUSet, mems libmem.NodeMask) {
	// A container with no cpuset runs on all CPUs, and all nodes are
	// allowed if its memory is not pinned.
	if cpus.IsEmpty() {
		cpus = p.sys.OnlineCPUs()
	}
	if mems == libmem.NodeMask(0) {
		mems = libmem.NewNodeMask(p.sys.NodeIDs()...)
	}
	nodes, err := libmem.MemoryPolicyNodes(p.sys, mpol.Mode, mpol.Nodes, cpus, mems)
	if err != nil {
		log.Errorf("%s: failed to set memory policy %s: %v", c.PrettyName(), mpol, err)
		return
	}
	mode, flags, err := mpol.ToNRI()
	if err != nil {
		log.Errorf("%s: failed to set memory policy %s: %v", c.PrettyName(), mpol, err)
		return
	}
	log.Infof("  => setting memory policy of %s to %s %v, nodes %s",
		c.PrettyName(), mpol.Mode, mpol.Flags, nodes.MemsetString())
	c.SetMemoryPolicy(mode, nodes.MemsetString(), flags...)
}

// draRequest is what one allocation result requests: CPUs of a device.
type draRequest struct {
	device     string
	deviceCPUs cpuset.CPUSet
	cpus       int
}

// draRequests translates claim allocation results into draRequests.
func (p *policy) draRequests(results []resourceapi.DeviceRequestAllocationResult) ([]draRequest, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("no allocation results")
	}

	reqs := make([]draRequest, 0, len(results))
	for i, r := range results {
		deviceCPUs, ok := p.draDeviceCPUs(r.Device)
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
		if offered := deviceCPUs.Size(); n > int64(offered) {
			return nil, fmt.Errorf("result %d consumed %d CPUs of device %s, which offers %d",
				i, n, r.Device, offered)
		}

		reqs = append(reqs, draRequest{device: r.Device, deviceCPUs: deviceCPUs, cpus: int(n)})
	}
	return reqs, nil
}

// draDeviceCPUs returns the grantable CPUs of the device, or false for an
// unknown device.
func (p *policy) draDeviceCPUs(device string) (cpuset.CPUSet, bool) {
	var cpus cpuset.CPUSet
	if id, ok := draDeviceID(device, draNUMADeviceFormat); ok {
		node := p.sys.Node(id)
		if node == nil {
			return cpuset.New(), false
		}
		cpus = node.CPUSet()
	} else if id, ok := draDeviceID(device, draSocketDeviceFormat); ok {
		pkg := p.sys.Package(id)
		if pkg == nil {
			return cpuset.New(), false
		}
		cpus = pkg.CPUSet()
	} else {
		return cpuset.New(), false
	}
	return cpus.Intersection(grantableCPUs(p.root.GetSupply())), true
}

// numaDevices returns the NUMA device of the first CPU of each result, or nil if
// a result has no CPU we know. A claim saved with no devices has NUMA devices
// only.
func (p *policy) numaDevices(results []cpuset.CPUSet) []string {
	var devices []string
	for _, cpus := range results {
		ids := cpus.List()
		if len(ids) == 0 {
			return nil
		}
		cpu := p.sys.CPU(idset.ID(ids[0]))
		if cpu == nil {
			return nil
		}
		devices = append(devices, fmt.Sprintf(draNUMADeviceFormat, cpu.NodeID()))
	}
	return devices
}

// draDeviceID returns the ID in the device name of the given format, or false
// if the name is not of the format.
func draDeviceID(device, format string) (idset.ID, bool) {
	var id int
	if _, err := fmt.Sscanf(device, format, &id); err != nil {
		return 0, false
	}
	return idset.ID(id), fmt.Sprintf(format, id) == device
}

// allocateClaim grants a claim the requested CPUs. Each request picks
// from what the earlier ones left, so several requests for one node get
// disjoint CPUs. The grant is reserved from the deepest pool that has
// all of its CPUs.
func (p *policy) allocateClaim(reqs []draRequest) (*claim, error) {
	cpus := cpuset.New()
	c := &claim{}
	for _, req := range reqs {
		picked, err := p.pickCPUs(req, cpus)
		if err != nil {
			return nil, err
		}
		cpus = cpus.Union(picked)
		c.results = append(c.results, picked)
		c.devices = append(c.devices, req.device)
	}

	pool := p.poolForCPUs(cpus)
	c.Grant = newGrant(pool, nil, cpuNormal, "", cpus, 0, 0, nil, 0)
	if _, err := pool.FreeSupply().Reserve(c.Grant, nil); err != nil {
		return nil, err
	}
	return c, nil
}

// moveClaim gives a saved claim other CPUs of the devices of its results, as
// many as each result had. The returned error starts with why, the reason
// the claim cannot keep its CPUs.
func (p *policy) moveClaim(uid string, c *claim, why error) error {
	if len(c.results) == 0 || len(c.devices) != len(c.results) {
		return fmt.Errorf("%w, and failed to move it: its results are lost", why)
	}

	old := cpuset.New()
	reqs := make([]draRequest, 0, len(c.results))
	for i, cpus := range c.results {
		deviceCPUs, ok := p.draDeviceCPUs(c.devices[i])
		if !ok {
			return fmt.Errorf("%w, and failed to move it: unknown device %q for result #%d, CPUs %s",
				why, c.devices[i], i, cpus)
		}
		old = old.Union(cpus)
		reqs = append(reqs, draRequest{device: c.devices[i], deviceCPUs: deviceCPUs, cpus: cpus.Size()})
	}

	moved, err := p.allocateClaim(reqs)
	if err != nil {
		return fmt.Errorf("%w, and failed to move it: %w", why, err)
	}
	moved.mpols = c.mpols
	p.allocations.claims[uid] = moved
	log.Warnf("%v, moved it from CPUs %s to %s", why, old, moved.ExclusiveCPUs())
	return nil
}

// pickCPUs returns as many free CPUs of the device as the request asks for,
// none of them in taken. It picks them from the deepest pool which has enough
// of them, and by topology within it, so two CPUs are the two threads of one
// core.
func (p *policy) pickCPUs(req draRequest, taken cpuset.CPUSet) (cpuset.CPUSet, error) {
	from, err := p.claimableCPUs(req.deviceCPUs)
	if err != nil {
		return cpuset.New(), fmt.Errorf("device %s: %w", req.device, err)
	}
	from = from.Difference(taken)
	if from.Size() < req.cpus {
		return cpuset.New(), fmt.Errorf("device %s: %d CPUs requested, only %d free",
			req.device, req.cpus, from.Size())
	}

	// Of the child pools with enough free CPUs, the one with the fewest gets
	// the request, which leaves the larger ones to requests which need them.
	for pool := p.poolForCPUs(req.deviceCPUs); ; {
		var (
			fit     Node
			fitCPUs cpuset.CPUSet
		)
		for _, child := range pool.Children() {
			cpus := from.Intersection(grantableCPUs(child.GetSupply()))
			if cpus.Size() >= req.cpus && (fit == nil || cpus.Size() < fitCPUs.Size()) {
				fit, fitCPUs = child, cpus
			}
		}
		if fit == nil {
			break
		}
		pool, from = fit, fitCPUs
	}

	cpus, err := p.cpuAllocator.AllocateCpus(&from, req.cpus, defaultPrio.Option())
	if err != nil {
		return cpuset.New(), fmt.Errorf("device %s: %w", req.device, err)
	}
	return cpus, nil
}

// claimableCPUs returns the CPUs among cpus which a claim can get now: the
// free isolated and sharable ones, minus the reserved ones, minus the ones
// shared containers need.
func (p *policy) claimableCPUs(cpus cpuset.CPUSet) (cpuset.CPUSet, error) {
	free := p.poolForCPUs(cpus).FreeSupply()
	sliceable, err := free.SliceableCPUs()
	if err != nil {
		return cpuset.New(), err
	}
	return free.IsolatedCPUs().Union(sliceable).Difference(free.ReservedCPUs()).Intersection(cpus), nil
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
