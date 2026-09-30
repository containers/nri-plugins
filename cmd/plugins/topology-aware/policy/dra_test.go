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

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
	specs "tags.cncf.io/container-device-interface/specs-go"
)

// draTestOwner keeps published DRA devices.
type draTestOwner struct {
	sync.Mutex
	devices []resourceapi.Device
}

func (*draTestOwner) UpdateContainers() error { return nil }

func (o *draTestOwner) PublishDRADevices(devices []resourceapi.Device) error {
	o.devices = devices
	return nil
}

// The DRA tests run on the "server" test sysfs: two packages, four NUMA
// nodes of 28 CPUs each with SMT on, two nodes with memory but no CPUs, and
// CPUs 4-7,60-63 kernel-isolated, two of them in each of the four nodes.

// draTestConfig is a configuration with the given available and reserved CPUs.
func draTestConfig(available, reserved string) *cfgapi.Config {
	cfg := &cfgapi.Config{
		AvailableResources: cfgapi.Constraints{},
		ReservedResources:  cfgapi.Constraints{cfgapi.CPU: cfgapi.Amount(reserved)},
	}
	if available != "" {
		cfg.AvailableResources[cfgapi.CPU] = cfgapi.Amount(available)
	}
	return cfg
}

func draTestPolicy(t *testing.T, sys system.System, cfg *cfgapi.Config) (*policy, *draTestOwner) {
	t.Helper()

	owner := &draTestOwner{}
	p := New().(*policy)
	if err := p.Setup(&policyapi.BackendOptions{
		Cache:  &mockCache{},
		System: sys,
		Owner:  owner,
		Config: cfg,
	}); err != nil {
		t.Fatalf("failed to set up policy: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("failed to start policy: %v", err)
	}

	return p, owner
}

type draDevice struct {
	numaNode int64
	socketID int64
	smt      bool
	cpus     int64
}

// draDeviceSummary summarizes the published devices by device name.
func draDeviceSummary(devices []resourceapi.Device) map[string]draDevice {
	summary := map[string]draDevice{}
	for _, d := range devices {
		capacity := d.Capacity[draCapacityCPU]
		summary[d.Name] = draDevice{
			numaNode: ptr.Deref(d.Attributes[deviceattribute.StandardDeviceAttributeNUMANode].IntValue, -1),
			socketID: ptr.Deref(d.Attributes[draAttrSocketID].IntValue, -1),
			smt:      ptr.Deref(d.Attributes[draAttrSMTEnabled].BoolValue, false),
			cpus:     capacity.Value.Value(),
		}
	}
	return summary
}

func TestDRADevices(t *testing.T) {
	sys := testServerSystem(t)
	node0 := sys.Node(0).CPUSet().String()

	for _, tc := range []struct {
		name      string
		available string
		reserved  string
		expected  map[string]draDevice
	}{
		{
			// The two nodes with no CPUs of their own publish nothing.
			name:     "one device per NUMA node with CPUs",
			reserved: "cpuset:0,1",
			expected: map[string]draDevice{
				"cpudevnuma000": {numaNode: 0, socketID: 0, smt: true, cpus: 27},
				"cpudevnuma001": {numaNode: 1, socketID: 1, smt: true, cpus: 27},
				"cpudevnuma002": {numaNode: 2, socketID: 0, smt: true, cpus: 28},
				"cpudevnuma003": {numaNode: 3, socketID: 1, smt: true, cpus: 28},
			},
		},
		{
			name:      "a node with no CPUs we may use is left out",
			available: "exclude-cpuset:" + node0,
			reserved:  "cpuset:1",
			expected: map[string]draDevice{
				"cpudevnuma001": {numaNode: 1, socketID: 1, smt: true, cpus: 27},
				"cpudevnuma002": {numaNode: 2, socketID: 0, smt: true, cpus: 28},
				"cpudevnuma003": {numaNode: 3, socketID: 1, smt: true, cpus: 28},
			},
		},
		{
			name:      "reserved CPUs are not published",
			available: "cpuset:0,8,12",
			reserved:  "cpuset:0,8",
			expected: map[string]draDevice{
				"cpudevnuma000": {numaNode: 0, socketID: 0, smt: true, cpus: 1},
			},
		},
		{
			// CPU 4 is kernel-isolated and belongs to node 0.
			name:     "a reserved CPU that is also isolated is still excluded",
			reserved: "cpuset:4",
			expected: map[string]draDevice{
				"cpudevnuma000": {numaNode: 0, socketID: 0, smt: true, cpus: 27},
				"cpudevnuma001": {numaNode: 1, socketID: 1, smt: true, cpus: 28},
				"cpudevnuma002": {numaNode: 2, socketID: 0, smt: true, cpus: 28},
				"cpudevnuma003": {numaNode: 3, socketID: 1, smt: true, cpus: 28},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, owner := draTestPolicy(t, sys, draTestConfig(tc.available, tc.reserved))

			summary := draDeviceSummary(owner.devices)
			if !reflect.DeepEqual(summary, tc.expected) {
				t.Errorf("published devices %v, expected %v", summary, tc.expected)
			}
		})
	}
}

// TestDRADeviceShape checks attributes of published device.
func TestDRADeviceShape(t *testing.T) {
	_, owner := draTestPolicy(t, testServerSystem(t), draTestConfig("cpuset:0,8,12", "cpuset:0"))

	one := resource.NewQuantity(1, resource.DecimalSI)
	expected := []resourceapi.Device{
		{
			Name:                     "cpudevnuma000",
			AllowMultipleAllocations: ptr.To(true),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				deviceattribute.StandardDeviceAttributeNUMANode: {IntValue: ptr.To(int64(0))},
				"dra.cpu/socketID":   {IntValue: ptr.To(int64(0))},
				"dra.cpu/smtEnabled": {BoolValue: ptr.To(true)},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				"dra.cpu/cpu": {
					Value: *resource.NewQuantity(2, resource.DecimalSI),
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
						CapacityKey:        ptr.To(resourceapi.QualifiedName("dra.cpu/cpu")),
						CapacityMultiplier: one,
					},
				},
			},
		},
	}

	if !apiequality.Semantic.DeepEqual(owner.devices, expected) {
		t.Errorf("published devices\n%+v\nexpected\n%+v", owner.devices, expected)
	}
}

// TestDRAPublish checks that policy publishes our devices on startup
// and after reconfiguration.
func TestDRAPublish(t *testing.T) {
	sys := testServerSystem(t)
	p, owner := draTestPolicy(t, sys, draTestConfig("", "cpuset:0,1"))

	if got := draDeviceSummary(owner.devices)["cpudevnuma002"].cpus; got != 28 {
		t.Fatalf("node 2 publishes %d CPUs after start, expected 28", got)
	}

	// Reserving one more CPU takes it out of the device it belongs to.
	if err := p.Reconfigure(draTestConfig("", "cpuset:0,1,2")); err != nil {
		t.Fatalf("failed to reconfigure policy: %v", err)
	}
	if got := draDeviceSummary(owner.devices)["cpudevnuma002"].cpus; got != 27 {
		t.Errorf("node 2 publishes %d CPUs after reserving CPU 2, expected 27", got)
	}
}

// draTestClaim returns a claim with the given UID.
func draTestClaim(uid string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: uid, Namespace: "test", UID: types.UID(uid)},
	}
}

// draTestResult is an allocation result of one device.
func draTestResult(node int, cpus string) resourceapi.DeviceRequestAllocationResult {
	r := resourceapi.DeviceRequestAllocationResult{
		Request: "cpus",
		Driver:  "topology-aware.nri.io",
		Pool:    "node",
		Device:  fmt.Sprintf(draDeviceFormat, node),
	}
	if cpus != "" {
		r.ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{
			draCapacityCPU: resource.MustParse(cpus),
		}
	}
	return r
}

// draTestReserved reserves two normal CPUs of NUMA node 3.
func draTestReserved(sys system.System) string {
	normal := sys.Node(3).CPUSet().Difference(sys.IsolatedCPUs())
	return "cpuset:" + cpuset.New(normal.List()[:2]...).String()
}

// freeCPUs returns the CPUs no grant or claim holds.
func freeCPUs(p *policy) cpuset.CPUSet {
	return grantableCPUs(p.root.FreeSupply())
}

// pinnedContainer keeps the cpuset the policy pinned it to last.
type pinnedContainer struct {
	mockContainer
	cpus string
}

func (c *pinnedContainer) SetCpusetCpus(cpus string) {
	c.cpus = cpus
}

// allocateShared gives a container a shared grant of cpu, in pool if one is
// given.
func allocateShared(t *testing.T, p *policy, id, cpu, pool string) *pinnedContainer {
	t.Helper()
	c := &pinnedContainer{mockContainer: mockContainer{
		name:                   id,
		returnValueForGetID:    id,
		returnValueForQOSClass: v1.PodQOSBurstable,
		returnValueForGetResourceRequirements: v1.ResourceRequirements{
			Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse(cpu)},
		},
	}}
	grant, err := p.allocatePool(c, pool)
	if err != nil {
		t.Fatalf("failed to allocate a shared grant: %v", err)
	}
	p.applyGrant(grant)
	return c
}

// allocateClaim allocates a claim, failing the test on error.
func allocateClaim(t *testing.T, p *policy, uid string, results ...resourceapi.DeviceRequestAllocationResult) cpuset.CPUSet {
	t.Helper()
	edits, err := p.AllocateClaim(draTestClaim(uid), results)
	if err != nil {
		t.Fatalf("failed to allocate claim %s: %v", uid, err)
	}
	return claimedCPUs(t, uid, edits, len(results))
}

// claimedCPUs gets the claimed cpuset out of the edits.
func claimedCPUs(t *testing.T, uid string, edits []specs.ContainerEdits, results int) cpuset.CPUSet {
	t.Helper()
	if len(edits) != results {
		t.Fatalf("claim %s: got %d edits for %d results", uid, len(edits), results)
	}

	prefix := draEnvPrefix + uid + "="
	value := ""
	for i, edit := range edits {
		if len(edit.Env) != 1 || !strings.HasPrefix(edit.Env[0], prefix) {
			t.Fatalf("claim %s: edit %d is %v, expected one %s<cpuset> variable", uid, i, edit.Env, prefix)
		}
		if v := edit.Env[0][len(prefix):]; value == "" {
			value = v
		} else if v != value {
			t.Fatalf("claim %s: edit %d sets %q, edit 0 set %q", uid, i, v, value)
		}
	}

	cpus, err := cpuset.Parse(value)
	if err != nil {
		t.Fatalf("claim %s: edits set cpuset %q: %v", uid, value, err)
	}
	return cpus
}

func TestAllocateClaim(t *testing.T) {
	sys := testServerSystem(t)

	tcs := []struct {
		name    string
		results []resourceapi.DeviceRequestAllocationResult
		wantErr string
	}{
		{
			name:    "one node",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(1, "2")},
		},
		{
			name:    "two results on one node",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "2"), draTestResult(0, "3")},
		},
		{
			name:    "two nodes",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "1"), draTestResult(2, "1")},
		},
		{
			name:    "a whole node",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(2, "28")},
		},
		{
			name:    "a whole node less the reserved CPUs",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(3, "26")},
		},
		{
			name:    "a reserved CPU",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(3, "27")},
			wantErr: "which offers 26",
		},
		{
			name:    "more than the node has",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(2, "29")},
			wantErr: "which offers 28",
		},
		{
			name:    "no results",
			wantErr: "no allocation results",
		},
		{
			name:    "unknown device",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(9, "1")},
			wantErr: "unknown device",
		},
		{
			name:    "no amount",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "")},
			wantErr: "consumed no dra.cpu/cpu",
		},
		{
			name:    "zero",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "0")},
			wantErr: "not a positive whole number",
		},
		{
			name:    "a fraction",
			results: []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "1500m")},
			wantErr: "not a positive whole number",
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))
			before := freeCPUs(p)

			edits, err := p.AllocateClaim(draTestClaim("a"), tc.results)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got error %v, expected one containing %q", err, tc.wantErr)
				}
				if got := freeCPUs(p); !got.Equals(before) {
					t.Errorf("a failed claim left CPUs %s claimed", before.Difference(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("failed to allocate claim: %v", err)
			}

			cpus := claimedCPUs(t, "a", edits, len(tc.results))
			for _, id := range sys.NodeIDs() {
				want := 0
				for _, r := range tc.results {
					if r.Device == fmt.Sprintf(draDeviceFormat, id) {
						q := r.ConsumedCapacity[draCapacityCPU]
						want += int(q.Value())
					}
				}
				if got := cpus.Intersection(sys.Node(id).CPUSet()).Size(); got != want {
					t.Errorf("claim got %d CPUs of node %d, expected %d", got, id, want)
				}
			}
			if got := before.Difference(freeCPUs(p)); !got.Equals(cpus) {
				t.Errorf("claim took CPUs %s out of the free supply, expected %s", got, cpus)
			}
		})
	}
}

// A claim takes sharable CPUs only as far as the shared containers can spare
// them, so a node with a little over one sharable CPU left still gives a
// claim its isolated CPUs and that one sharable CPU.
func TestAllocateClaimSparesSharedCPUs(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	// Shared containers of 990m each, one fewer than the pool has sharable
	// CPUs, leave a little over one CPU's worth of shared capacity.
	pool := p.poolForCPUs(sys.Node(0).CPUSet())
	sharable := pool.FreeSupply().SharableCPUs().Size()
	for i := range sharable - 1 {
		allocateShared(t, p, fmt.Sprintf("shared-%d", i), "990m", pool.Name())
	}

	isolated := sys.Node(0).CPUSet().Intersection(sys.IsolatedCPUs())
	cpus := allocateClaim(t, p, "a", draTestResult(0, fmt.Sprint(isolated.Size()+1)))
	if !isolated.IsSubsetOf(cpus) {
		t.Errorf("claim got CPUs %s, expected the isolated %s and one more", cpus, isolated)
	}
}

// TestReserveClaimSparesSharedCPUs checks that reserving a claim grant fails
// if the shared containers left on its sharable CPUs would be overcommitted.
func TestReserveClaimSparesSharedCPUs(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	// Shared containers of 990m each, one fewer than the pool has sharable
	// CPUs, leave a little over one CPU's worth of shared capacity.
	pool := p.poolForCPUs(sys.Node(0).CPUSet())
	sharable := pool.FreeSupply().SharableCPUs()
	for i := range sharable.Size() - 1 {
		allocateShared(t, p, fmt.Sprintf("shared-%d", i), "990m", pool.Name())
	}
	free := freeCPUs(p)

	two := newGrant(pool, nil, cpuNormal, "", cpuset.New(sharable.List()[:2]...), 0, 0, nil, 0)
	if _, err := pool.FreeSupply().Reserve(two, nil); err == nil || !strings.Contains(err.Error(), "can't reserve 0 shared CPUs") {
		t.Fatalf("got error %v reserving %s, expected an overcommit", err, two)
	}
	if got := freeCPUs(p); !got.Equals(free) {
		t.Errorf("a refused claim grant left CPUs %s reserved", free.Difference(got))
	}

	one := newGrant(pool, nil, cpuNormal, "", cpuset.New(sharable.List()[0]), 0, 0, nil, 0)
	if _, err := pool.FreeSupply().Reserve(one, nil); err != nil {
		t.Errorf("failed to reserve %s: %v", one, err)
	}
	if got := freeCPUs(p); !got.Equals(free.Difference(one.ExclusiveCPUs())) {
		t.Errorf("free CPUs are %s after reserving %s, expected %s", got, one, free.Difference(one.ExclusiveCPUs()))
	}
}

// TestAllocateClaimPicksByTopology checks that a claim for two CPUs
// gets both threads of one core, not one thread of each of two cores.
func TestAllocateClaimPicksByTopology(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	cpus := allocateClaim(t, p, "a", draTestResult(1, "2"))
	if core := sys.CPU(cpus.List()[0]).ThreadCPUSet(); !core.Equals(cpus) {
		t.Errorf("claim got CPUs %s, expected the threads of one core, such as %s", cpus, core)
	}
}

// TestAllocateClaimAgain checks that preparing a claim again gets the same CPUs.
func TestAllocateClaimAgain(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	first := allocateClaim(t, p, "a", draTestResult(0, "2"))
	free := freeCPUs(p)

	again := allocateClaim(t, p, "a", draTestResult(0, "2"))
	if !again.Equals(first) {
		t.Errorf("claim got CPUs %s the second time, %s the first", again, first)
	}
	if got := freeCPUs(p); !got.Equals(free) {
		t.Errorf("preparing a claim again changed the free CPUs from %s to %s", free, got)
	}

	// A claim cannot change what it consumed once it has CPUs.
	if _, err := p.AllocateClaim(draTestClaim("a"), []resourceapi.DeviceRequestAllocationResult{
		draTestResult(0, "3"),
	}); err == nil || !strings.Contains(err.Error(), "holds 2 CPUs but consumed 3") {
		t.Errorf("got error %v, expected a size mismatch", err)
	}
	// Nor move them to another node.
	if _, err := p.AllocateClaim(draTestClaim("a"), []resourceapi.DeviceRequestAllocationResult{
		draTestResult(1, "2"),
	}); err == nil || !strings.Contains(err.Error(), "holds 0 CPUs of node #1 but consumed 2") {
		t.Errorf("got error %v, expected a node mismatch", err)
	}
}

// TestAllocateClaimsExclusive checks that claims on one node get disjoint CPUs,
// a claim the node cannot fill fails, and a failed multi-result claim leaves nothing claimed.
func TestAllocateClaimsExclusive(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	a := allocateClaim(t, p, "a", draTestResult(0, "4"))
	b := allocateClaim(t, p, "b", draTestResult(0, "4"))
	if !a.Intersection(b).IsEmpty() {
		t.Errorf("claims a (%s) and b (%s) share CPUs", a, b)
	}

	free := freeCPUs(p)
	left := free.Intersection(sys.Node(0).CPUSet()).Size()
	tooMany := fmt.Sprintf("%d", left+1)

	if _, err := p.AllocateClaim(draTestClaim("c"), []resourceapi.DeviceRequestAllocationResult{
		draTestResult(0, tooMany),
	}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("only %d free", left)) {
		t.Errorf("got error %v, expected exhaustion of node 0", err)
	}

	if _, err := p.AllocateClaim(draTestClaim("d"), []resourceapi.DeviceRequestAllocationResult{
		draTestResult(1, "1"), draTestResult(0, tooMany),
	}); err == nil {
		t.Errorf("claim d succeeded, expected exhaustion of node 0")
	}
	if got := freeCPUs(p); !got.Equals(free) {
		t.Errorf("failed claims left CPUs %s claimed", free.Difference(got))
	}
}

// TestReleaseClaim checks that releasing a claim frees its CPUs,
// that releasing an unknown or already released claim has no effect,
// and that released CPUs can be claimed again.
func TestReleaseClaim(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))
	free := freeCPUs(p)

	a := allocateClaim(t, p, "a", draTestResult(0, "2"), draTestResult(1, "2"))
	b := allocateClaim(t, p, "b", draTestResult(0, "2"))

	if err := p.ReleaseClaim("a"); err != nil {
		t.Fatalf("failed to release claim a: %v", err)
	}
	if got := freeCPUs(p); !got.Equals(free.Difference(b)) {
		t.Errorf("free CPUs are %s after releasing a, expected all but b's %s", got, b)
	}

	// Releasing a claim we do not know, or one we have released, is nothing.
	for _, uid := range []types.UID{"a", "unknown"} {
		if err := p.ReleaseClaim(uid); err != nil {
			t.Errorf("failed to release claim %s: %v", uid, err)
		}
	}
	if got := freeCPUs(p); !got.Equals(free.Difference(b)) {
		t.Errorf("free CPUs are %s after releasing nothing, expected %s", got, free.Difference(b))
	}

	// The released CPUs can be claimed again.
	c := allocateClaim(t, p, "c", draTestResult(0, "2"), draTestResult(1, "2"))
	if !c.Equals(a) {
		t.Errorf("claim c got CPUs %s, expected the %s claim a released", c, a)
	}
}

// TestClaimAndContainerWithOneID verifies that a claim and a container
// with the same ID both survive a reconfiguration and a round trip through the cache.
func TestClaimAndContainerWithOneID(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	c := allocateShared(t, p, "a", "100m", "")
	allocateClaim(t, p, "a", draTestResult(0, "2"))
	free := freeCPUs(p)

	cfg := draTestConfig("", draTestReserved(sys))
	cfg.ColocatePods = true
	if err := p.Reconfigure(cfg); err != nil {
		t.Fatalf("failed to reconfigure policy: %v", err)
	}
	if g, ok := p.allocations.grants["a"]; !ok {
		t.Errorf("container grant a lost in a reconfiguration")
	} else if pool := g.GetCPUNode(); pool != p.nodes[pool.Name()] {
		t.Errorf("container grant a still refers to the pools from before the reconfiguration")
	}
	if _, ok := p.allocations.claims["a"]; !ok {
		t.Errorf("claim grant a lost in a reconfiguration")
	}
	if got := freeCPUs(p); !got.Equals(free) {
		t.Errorf("free CPUs are %s after reconfiguring, expected %s", got, free)
	}

	data, err := p.allocations.MarshalJSON()
	if err != nil {
		t.Fatalf("failed to marshal allocations: %v", err)
	}
	p.cache = &mockCache{returnValue1ForLookupContainer: c, returnValue2ForLookupContainer: true}
	restored := p.newAllocations()
	if err := restored.UnmarshalJSON(data); err != nil {
		t.Fatalf("failed to unmarshal allocations: %v", err)
	}
	if len(restored.grants) != 1 || len(restored.claims) != 1 {
		t.Errorf("restored %d container and %d claim grants from %s, expected one of each",
			len(restored.grants), len(restored.claims), data)
	}
}

// TestReconfigureKeepsClaims verifies that a reconfiguration keeps
// the claimed CPUs out of the rebuilt pools.
// It also verifies that a refused reconfiguration does not alter the
// state of the claimed CPUs.
func TestReconfigureKeepsClaims(t *testing.T) {
	sys := testServerSystem(t)
	cfg := draTestConfig("", draTestReserved(sys))
	cfg.PinCPU = true
	p, _ := draTestPolicy(t, sys, cfg)

	shared := allocateShared(t, p, "shared", "100m", "")
	a := allocateClaim(t, p, "a", draTestResult(0, "2"))
	free := freeCPUs(p)

	cfg = draTestConfig("", draTestReserved(sys))
	cfg.PinCPU = true
	cfg.ColocatePods = true
	if err := p.Reconfigure(cfg); err != nil {
		t.Fatalf("failed to reconfigure policy: %v", err)
	}
	if got := freeCPUs(p); !got.Equals(free) {
		t.Errorf("free CPUs are %s after reconfiguring, expected %s", got, free)
	}
	if pinned := cpuset.MustParse(shared.cpus); pinned.IsEmpty() || !pinned.Intersection(a).IsEmpty() {
		t.Errorf("shared container pinned to %s after reconfiguring, claim a has %s", pinned, a)
	}

	prio := defaultPrio
	refused := draTestConfig("", "cpuset:"+cpuset.New(a.List()[0]).String())
	refused.DefaultCPUPriority = cfgapi.PriorityLow
	err := p.Reconfigure(refused)
	if err == nil || !strings.Contains(err.Error(), "can't reserve") {
		t.Fatalf("got error %v reserving a claimed CPU, expected a refusal", err)
	}
	if defaultPrio != prio {
		t.Errorf("default CPU priority is %s after a refused reconfiguration, expected %s", defaultPrio, prio)
	}
	if got := freeCPUs(p); !got.Equals(free) {
		t.Errorf("free CPUs are %s after a refused reconfiguration, expected %s", got, free)
	}
	if again := allocateClaim(t, p, "a", draTestResult(0, "2")); !again.Equals(a) {
		t.Errorf("claim a has CPUs %s after a refused reconfiguration, expected %s", again, a)
	}
}
