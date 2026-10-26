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

// We publish each group of CPUs of the topology as a DRA device: the machine,
// each socket and each NUMA node. Its capacity is the number of CPUs we can
// give out from that group. A claim requests a number of CPUs from one device.
//
// We allocate the CPUs of a claim as a grant, the same way as for a
// container. The grant has only exclusive CPUs: no shared part and no
// memory. Like other grants, it is accounted, kept across a reconfiguration
// and saved to the cache. The claimed CPUs leave the free supply, so the
// policy does not hand them out again. A claim is not bound to its CPUs:
// when a claim on a smaller device inside its device needs them, it moves to
// other CPUs of its device, and the containers holding it move with it.
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
	"maps"
	"math"
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
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

const (
	// draMachineDevice is the name of the device which publishes the machine.
	draMachineDevice = "cpudevmachine"
	// draSocketDeviceFormat names a device after the socket it publishes.
	draSocketDeviceFormat = "cpudevsocket%03d"
	// draNUMADeviceFormat names a device after the NUMA node it publishes.
	draNUMADeviceFormat = "cpudevnuma%03d"
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
	// The levels of our devices.
	draLevelMachine  = "machine"
	draLevelSocket   = "socket"
	draLevelNUMANode = "numanode"
	// draConfigAPIVersion and draConfigKind identify our claim configs.
	draConfigAPIVersion = "config.nri/v1alpha1"
	draConfigKind       = "TopologyAwareClaimConfig"
)

// draDriverName is the name of our DRA driver.
var draDriverName = dra.DriverName(PolicyName)

// claim is a DRA claim we allocated. It has a grant of all of its CPUs, and
// the CPUs, the device and the memory policy of each of its allocation
// results, in result order. mpols is nil if no result has a memory policy.
// moves are the claims allocating it moved to other CPUs, by UID. They are
// not saved.
type claim struct {
	Grant
	results []cpuset.CPUSet
	devices []string
	mpols   []*cfgapi.MemoryPolicy
	moves   map[string]claimMove
}

// claimMove is a claim moved to other CPUs, as it was before and after the
// move.
type claimMove struct {
	from, to *claim
}

// draCPUGroup is a group of CPUs of the topology which we publish as a DRA
// device: its device name, level and CPUs, and the ID attributes of its level.
type draCPUGroup struct {
	name  string
	level string
	cpus  cpuset.CPUSet
	ids   map[resourceapi.QualifiedName]int64
}

// draCPUGroups returns the NUMA nodes, the sockets and the machine.
func draCPUGroups(sys system.System) []draCPUGroup {
	var groups []draCPUGroup
	for _, id := range sys.NodeIDs() {
		groups = append(groups, draCPUGroup{
			name:  fmt.Sprintf(draNUMADeviceFormat, id),
			level: draLevelNUMANode,
			cpus:  sys.Node(id).CPUSet(),
			ids: map[resourceapi.QualifiedName]int64{
				deviceattribute.StandardDeviceAttributeNUMANode: int64(id),
				draAttrSocketID: int64(sys.Node(id).PackageID()),
			},
		})
	}
	for _, id := range sys.PackageIDs() {
		groups = append(groups, draCPUGroup{
			name:  fmt.Sprintf(draSocketDeviceFormat, id),
			level: draLevelSocket,
			cpus:  sys.Package(id).CPUSet(),
			ids:   map[resourceapi.QualifiedName]int64{draAttrSocketID: int64(id)},
		})
	}
	return append(groups, draCPUGroup{name: draMachineDevice, level: draLevelMachine, cpus: sys.CPUSet()})
}

// draDevices returns a device for each CPU group which has CPUs for claims.
func (p *policy) draDevices() []resourceapi.Device {
	var devices []resourceapi.Device
	for _, group := range p.groups {
		if d, ok := p.draDevice(group); ok {
			devices = append(devices, d)
		}
	}
	return devices
}

// draDevice returns the device of the CPU group, or false if it has no CPUs
// for claims.
func (p *policy) draDevice(group draCPUGroup) (resourceapi.Device, bool) {
	cpus, _ := p.draDeviceCPUs(group.name)
	if cpus.IsEmpty() {
		return resourceapi.Device{}, false
	}
	capacity := p.draCapacity(group.name, cpus)
	if capacity == 0 {
		return resourceapi.Device{}, false
	}

	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		draAttrLevel:      {StringValue: ptr.To(group.level)},
		draAttrSMTEnabled: {BoolValue: ptr.To(p.sys.MaxThreadCount() > 1)},
		draAttrNumCPUs:    {IntValue: ptr.To(int64(cpus.Size()))},
	}
	for attr, id := range group.ids {
		attrs[attr] = resourceapi.DeviceAttribute{IntValue: ptr.To(id)}
	}
	var roots []string
	for _, id := range p.sys.PackageIDs() {
		if pkg := p.sys.Package(id); !pkg.CPUSet().Intersection(cpus).IsEmpty() {
			roots = append(roots, pkg.PCIeRoots()...)
		}
	}
	if len(roots) > 0 && p.options.Owner.DRAListAttributesSupported() {
		// A root with CPUs of several sockets is a root of each of them.
		slices.Sort(roots)
		attrs[deviceattribute.StandardDeviceAttributePCIeRoot] = resourceapi.DeviceAttribute{
			StringValues: slices.Compact(roots),
		}
	}

	return resourceapi.Device{
		Name:                     group.name,
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
	// The scheduler subtracts what claims consume of the device from its
	// capacity, so the CPUs of its own claims are part of it. So are the CPUs
	// of claims on larger devices, as many as can move to other CPUs of their
	// device: those claims are not bound to the CPUs of this device.
	own, movable := 0, cpuset.New()
	larger := map[string]int{}
	for _, c := range p.allocations.claims {
		for i, held := range c.results {
			if i >= len(c.devices) {
				continue
			}
			if c.devices[i] == device {
				own += held.Intersection(cpus).Size()
			} else if p.largerDevice(c.devices[i], cpus) {
				larger[c.devices[i]] += held.Size()
				movable = movable.Union(held)
			}
		}
	}

	// A new claim can get the free CPUs of the device and the CPUs movable
	// claims hold in it. Each larger device must then hold the new claim, its
	// own claims and the claims of the larger devices inside it, in its free
	// CPUs and the CPUs of movable claims. Counting the free CPUs of the whole
	// larger device at once leaves the shared containers of its pools the
	// CPUs they need.
	n := p.availableCPUs(cpus, cpuset.New()).Size() + movable.Intersection(cpus).Size()
	needed := 0
	for _, name := range slices.SortedFunc(maps.Keys(larger), func(a, b string) int {
		aCPUs, _ := p.draDeviceCPUs(a)
		bCPUs, _ := p.draDeviceCPUs(b)
		return aCPUs.Size() - bCPUs.Size()
	}) {
		deviceCPUs, _ := p.draDeviceCPUs(name)
		needed += larger[name]
		free := p.availableCPUs(deviceCPUs, cpuset.New()).Size()
		n = min(n, free+movable.Intersection(deviceCPUs).Size()-needed)
	}
	return own + max(n, 0)
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
		c, err = p.allocateClaim(reqs, true)
		if err != nil {
			return nil, fmt.Errorf("claim %s: %w", uid, err)
		}
		c.mpols = mpols
		p.allocations.claims[uid] = c
		p.repinHolders(slices.Sorted(maps.Keys(c.moves)))
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

// ReleaseClaim releases the claim's CPUs. If undo is true, it also moves the
// claims allocating it moved back to the CPUs they had.
func (p *policy) ReleaseClaim(uid types.UID, undo bool) error {
	c, ok := p.allocations.claims[string(uid)]
	if !ok {
		return nil
	}

	// The kubelet unprepares a claim only after all of its containers have
	// stopped, and their grants went with them, so no grant holds the
	// claim's CPUs by now.
	c.Release()
	delete(p.allocations.claims, string(uid))
	if undo {
		p.moveClaimsBack(c)
	}
	p.saveAllocations()
	p.updateSharedAllocations(nil)

	return nil
}

// moveClaimsBack moves the claims allocating c moved back to the CPUs they
// had, unless they moved again since, and pins their holders to those CPUs.
func (p *policy) moveClaimsBack(c *claim) {
	var back []string
	for _, uid := range slices.Sorted(maps.Keys(c.moves)) {
		m := c.moves[uid]
		if p.allocations.claims[uid] != m.to {
			log.Warnf("DRA claim %s moved again or was released, not moving it back", uid)
			continue
		}
		if p.swapClaim(uid, m.to, m.from) {
			back = append(back, uid)
		}
	}
	p.repinHolders(back)
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
	for _, group := range p.groups {
		if group.name == device {
			return group.cpus.Intersection(grantableCPUs(p.root.GetSupply())), true
		}
	}
	return cpuset.New(), false
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

// allocateClaim returns a claim with the requested CPUs reserved for it, each
// result with CPUs of its own. If move is true, it moves claims on larger
// devices off the CPUs a request needs, and records the moves in the returned
// claim. On failure, every claim keeps its CPUs.
func (p *policy) allocateClaim(reqs []draRequest, move bool) (*claim, error) {
	// Requests on smaller devices pick first, so a request on a larger
	// device does not take the CPUs they need.
	order := make([]int, len(reqs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return reqs[a].deviceCPUs.Size() - reqs[b].deviceCPUs.Size()
	})

	// Each request picks from what the earlier ones left.
	cpus := cpuset.New()
	c := &claim{results: make([]cpuset.CPUSet, len(reqs)), devices: make([]string, len(reqs))}
	moved := map[string]*claim{}
	for _, i := range order {
		req := reqs[i]
		picked, err := p.pickCPUs(req, cpus)
		if err != nil && move {
			p.moveClaimsOff(req, cpus, moved)
			picked, err = p.pickCPUs(req, cpus)
		}
		if err != nil {
			p.undoMoves(moved)
			return nil, err
		}
		cpus = cpus.Union(picked)
		c.results[i] = picked
		c.devices[i] = req.device
	}

	if err := p.reserveClaim(c); err != nil {
		p.undoMoves(moved)
		return nil, err
	}
	for uid, from := range moved {
		if c.moves == nil {
			c.moves = map[string]claimMove{}
		}
		c.moves[uid] = claimMove{from: from, to: p.allocations.claims[uid]}
	}
	return c, nil
}

// reserveClaim reserves a grant of all CPUs of the claim's results for it,
// from the deepest pool that has all of them.
func (p *policy) reserveClaim(c *claim) error {
	cpus := cpuset.New()
	for _, held := range c.results {
		cpus = cpus.Union(held)
	}
	pool := p.poolForCPUs(cpus)
	c.Grant = newGrant(pool, nil, cpuNormal, "", cpus, 0, 0, nil, 0)
	_, err := pool.FreeSupply().Reserve(c.Grant, nil)
	return err
}

// moveClaimsOff moves claims on devices larger than the device of req to
// other CPUs of their devices, none of them in taken, until req has the CPUs
// it asks for or no claim can move. moved gets the claims as they were before
// their first move.
func (p *policy) moveClaimsOff(req draRequest, taken cpuset.CPUSet, moved map[string]*claim) {
	short := req.cpus - p.availableCPUs(req.deviceCPUs, taken).Size()
	for _, uid := range slices.Sorted(maps.Keys(p.allocations.claims)) {
		if short <= 0 {
			return
		}
		c := p.allocations.claims[uid]
		next := &claim{results: slices.Clone(c.results), devices: c.devices, mpols: c.mpols}
		gave, picked := 0, cpuset.New()
		for i, held := range c.results {
			if i >= len(c.devices) || !p.largerDevice(c.devices[i], req.deviceCPUs) {
				continue
			}
			// A result gives up only as many CPUs as req still needs.
			give, replace := p.moveResult(c.devices[i], held, req.deviceCPUs, short-gave, taken.Union(picked))
			if give.IsEmpty() {
				continue
			}
			next.results[i] = held.Difference(give).Union(replace)
			gave += give.Size()
			picked = picked.Union(replace)
		}
		if gave > 0 && p.swapClaim(uid, c, next) {
			if _, ok := moved[uid]; !ok {
				moved[uid] = c
			}
			short -= gave
		}
	}
}

// moveResult returns up to n CPUs of a result held on device which are in
// cpus, and as many other CPUs of the device to replace them with, none of
// them in taken. The replacing CPUs leave the free CPUs of cpus free. It
// returns no CPUs if the device has no others free.
func (p *policy) moveResult(device string, held, cpus cpuset.CPUSet, n int, taken cpuset.CPUSet) (cpuset.CPUSet, cpuset.CPUSet) {
	give := held.Intersection(cpus)
	deviceCPUs, _ := p.draDeviceCPUs(device)
	other := draRequest{device: device, deviceCPUs: deviceCPUs.Difference(cpus)}
	// Taking the free CPUs of cpus here keeps the CPUs of other from using
	// up the spare CPUs of shared containers which cpus needs.
	taken = taken.Union(p.availableCPUs(cpus, taken))
	other.cpus = min(n, give.Size(), p.availableCPUs(other.deviceCPUs, taken).Size())
	if other.cpus == 0 {
		return cpuset.New(), cpuset.New()
	}

	if other.cpus < give.Size() {
		// The CPUs the result keeps stay together.
		if _, err := p.cpuAllocator.ReleaseCpus(&give, other.cpus, defaultPrio.Option()); err != nil {
			log.Errorf("device %s: failed to pick CPUs to move off %s: %v", device, cpus, err)
			return cpuset.New(), cpuset.New()
		}
	}
	replace, err := p.pickCPUs(other, taken)
	if err != nil {
		log.Errorf("failed to pick CPUs to move to: %v", err)
		return cpuset.New(), cpuset.New()
	}
	return give, replace
}

// swapClaim replaces claim c by next, with the CPUs next holds, and tells
// whether it did. It keeps c if next holds the same CPUs or cannot have
// them.
func (p *policy) swapClaim(uid string, c, next *claim) bool {
	from, to := c.ExclusiveCPUs(), cpuset.New()
	for _, held := range next.results {
		to = to.Union(held)
	}
	if to.Equals(from) {
		return false
	}

	c.Release()
	if err := p.reserveClaim(next); err != nil {
		log.Errorf("claim %s: failed to move from CPUs %s to %s: %v", uid, from, to, err)
		if _, err := c.GetCPUNode().FreeSupply().Reserve(c.Grant, nil); err != nil {
			log.Errorf("claim %s: failed to get CPUs %s back: %v", uid, from, err)
		}
		return false
	}
	p.allocations.claims[uid] = next
	log.Infof("moved DRA claim %s from CPUs %s to %s", uid, from, to)
	return true
}

// undoMoves gives the moved claims back the CPUs they had.
func (p *policy) undoMoves(moved map[string]*claim) {
	for uid := range moved {
		p.allocations.claims[uid].Release()
	}
	for uid, c := range moved {
		if _, err := c.GetCPUNode().FreeSupply().Reserve(c.Grant, nil); err != nil {
			log.Errorf("claim %s: failed to get CPUs %s back: %v", uid, c.ExclusiveCPUs(), err)
		}
		p.allocations.claims[uid] = c
		log.Infof("moved DRA claim %s back to CPUs %s", uid, c.ExclusiveCPUs())
	}
}

// repinHolders pins the containers holding the given claims to the CPUs the
// claims hold now.
func (p *policy) repinHolders(uids []string) {
	for _, g := range p.allocations.grants {
		// A container keeping its own CPUs is not pinned to its claims.
		if g.ClaimedCPUs().IsEmpty() {
			continue
		}
		ctr := g.GetContainer()
		for uid := range dra.ContainerClaims(ctr, draDriverName) {
			if !slices.Contains(uids, string(uid)) {
				continue
			}
			claimed, _, err := p.containerClaims(ctr)
			if err != nil {
				log.Errorf("%s: failed to follow its moved DRA claims: %v", ctr.PrettyName(), err)
				break
			}
			g.SetClaimedCPUs(claimed)
			p.applyGrant(g)
			break
		}
	}
}

// moveClaim gives a saved claim other CPUs of the devices of its results, as
// many as each result had, moving claims on larger devices off them if
// needed. The returned error starts with why, the reason the claim cannot
// keep its CPUs.
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

	// No container runs on the claims yet, so moved claims need no repinning.
	moved, err := p.allocateClaim(reqs, true)
	if err != nil {
		return fmt.Errorf("%w, and failed to move it: %w", why, err)
	}
	moved.mpols = c.mpols
	p.allocations.claims[uid] = moved
	log.Warnf("%v, moved it from CPUs %s to %s", why, old, moved.ExclusiveCPUs())
	return nil
}

// pickCPUs returns as many free CPUs of the device as the request asks for,
// none of them in taken, close together: of one pool or NUMA node of the
// device when one has enough, and two CPUs are the two threads of one core.
func (p *policy) pickCPUs(req draRequest, taken cpuset.CPUSet) (cpuset.CPUSet, error) {
	top := p.poolForCPUs(req.deviceCPUs)
	from, err := p.claimableCPUs(top, req.deviceCPUs, taken)
	if err != nil {
		return cpuset.New(), fmt.Errorf("device %s: %w", req.device, err)
	}
	if from.Size() < req.cpus {
		return cpuset.New(), fmt.Errorf("device %s: %d CPUs requested, only %d free",
			req.device, req.cpus, from.Size())
	}

	// Of the deepest pools with enough free CPUs, the one with the fewest gets
	// the request, which leaves the larger ones to requests which need them.
	fit, fitCPUs := top, from
	visit := func(pool Node) bool {
		cpus, poolErr := p.claimableCPUs(pool, req.deviceCPUs, taken)
		if poolErr != nil {
			err = poolErr
			return true
		}
		deeper := pool.RootDistance() - fit.RootDistance()
		if cpus.Size() >= req.cpus && (deeper > 0 || deeper == 0 && cpus.Size() < fitCPUs.Size()) {
			fit, fitCPUs = pool, cpus
		}
		return false
	}
	for _, child := range top.Children() {
		if child.DepthFirst(visit) {
			break
		}
	}
	if err != nil {
		return cpuset.New(), fmt.Errorf("device %s: %w", req.device, err)
	}

	// A memoryless NUMA node has no pool, so its CPUs are in no child. If the
	// pool spans several nodes, the node with the fewest free CPUs which are
	// enough gets the request.
	fitDeviceCPUs := grantableCPUs(fit.GetSupply()).Intersection(req.deviceCPUs)
	for _, id := range p.sys.NodeIDs() {
		nodeCPUs := p.sys.Node(id).CPUSet().Intersection(fitDeviceCPUs)
		if nodeCPUs.IsEmpty() || nodeCPUs.Equals(fitDeviceCPUs) {
			continue
		}
		cpus, err := p.claimableCPUs(fit, nodeCPUs, taken)
		if err != nil {
			return cpuset.New(), fmt.Errorf("device %s: %w", req.device, err)
		}
		if cpus.Size() >= req.cpus && cpus.Size() < fitCPUs.Size() {
			fitCPUs = cpus
		}
	}

	cpus, err := p.cpuAllocator.AllocateCpus(&fitCPUs, req.cpus, defaultPrio.Option())
	if err != nil {
		return cpuset.New(), fmt.Errorf("device %s: %w", req.device, err)
	}
	return cpus, nil
}

// claimableCPUs returns the CPUs among cpus which a claim can get now from
// the pool, none of them in taken: the free isolated and sharable ones, minus
// the reserved ones, minus the ones the shared containers of the pool and of
// the pools above and below it need.
func (p *policy) claimableCPUs(pool Node, cpus, taken cpuset.CPUSet) (cpuset.CPUSet, error) {
	limit := math.MaxInt
	for above := pool.Parent(); !above.IsNil(); above = above.Parent() {
		limit = min(limit, spareSharedCPUs(above, taken))
	}
	sliceable, err := p.sliceableCPUs(pool, cpus, taken, limit)
	if err != nil {
		return cpuset.New(), err
	}
	free := pool.FreeSupply()
	isolated := free.IsolatedCPUs().Difference(free.ReservedCPUs()).Intersection(cpus).Difference(taken)
	return isolated.Union(sliceable), nil
}

// spareSharedCPUs returns how many more CPUs the shared containers of the
// pool can give up, after those in taken.
func spareSharedCPUs(pool Node, taken cpuset.CPUSet) int {
	free := pool.FreeSupply()
	return free.AllocatableSharedCPU(true)/1000 - taken.Intersection(free.SharableCPUs()).Size()
}

// sliceableCPUs returns at most limit sharable CPUs among cpus which a claim
// can take from the pool, none of them reserved or in taken, leaving the pool
// and each pool below it the CPUs its shared containers need.
func (p *policy) sliceableCPUs(pool Node, cpus, taken cpuset.CPUSet, limit int) (cpuset.CPUSet, error) {
	free := pool.FreeSupply()
	n := min(limit, spareSharedCPUs(pool, taken))
	var below []cpuset.CPUSet
	if pool.IsLeafNode() {
		leaf, err := free.SliceableCPUs()
		if err != nil {
			return cpuset.New(), err
		}
		below = append(below, leaf.Difference(free.ReservedCPUs()).Intersection(cpus).Difference(taken))
	} else {
		// A memoryless NUMA node has no pool, so its CPUs are in no child.
		local := free.SharableCPUs().Difference(free.ReservedCPUs()).Intersection(cpus).Difference(taken)
		for _, child := range pool.Children() {
			local = local.Difference(child.GetSupply().SharableCPUs())
		}
		below = append(below, local)
	}
	for _, child := range pool.Children() {
		group, err := p.sliceableCPUs(child, cpus, taken, n)
		if err != nil {
			return cpuset.New(), err
		}
		below = append(below, group)
	}

	// The CPUs of the child pools leave their own shared containers enough,
	// but maybe not those of this pool. The CPUs of whole child pools are
	// kept first, the largest first, so that a claim can still fit in one.
	slices.SortStableFunc(below, func(a, b cpuset.CPUSet) int { return b.Size() - a.Size() })
	sliceable := cpuset.New()
	for _, group := range below {
		left := n - sliceable.Size()
		if left <= 0 {
			break
		}
		if group.Size() > left {
			var err error
			if group, err = p.cpuAllocator.AllocateCpus(&group, left, defaultPrio.Option()); err != nil {
				return cpuset.New(), err
			}
		}
		sliceable = sliceable.Union(group)
	}
	return sliceable, nil
}

// availableCPUs returns the CPUs among cpus which a claim can get now, none of
// them in taken, or none if it fails to find them.
func (p *policy) availableCPUs(cpus, taken cpuset.CPUSet) cpuset.CPUSet {
	free, err := p.claimableCPUs(p.poolForCPUs(cpus), cpus, taken)
	if err != nil {
		log.Errorf("failed to find the free CPUs of %s: %v", cpus, err)
	}
	return free
}

// largerDevice tells whether the device has all of cpus and more.
func (p *policy) largerDevice(device string, cpus cpuset.CPUSet) bool {
	larger, ok := p.draDeviceCPUs(device)
	return ok && cpus.IsSubsetOf(larger) && !cpus.Equals(larger)
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
