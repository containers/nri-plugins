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
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	nri "github.com/containerd/nri/pkg/api"
	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/testutils"
	"github.com/containers/nri-plugins/pkg/topology"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

// draTestOwner keeps published DRA devices, or fails to publish them with
// publishErr.
type draTestOwner struct {
	sync.Mutex
	devices    []resourceapi.Device
	publishErr error

	listAttributesSupported bool
}

func (*draTestOwner) UpdateContainers() error { return nil }

func (*draTestOwner) DRAEnabled() bool { return true }

func (o *draTestOwner) PublishDRADevices(devices []resourceapi.Device) error {
	if o.publishErr != nil {
		return o.publishErr
	}
	o.devices = devices
	return nil
}

func (o *draTestOwner) DRAListAttributesSupported() bool { return o.listAttributesSupported }

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
	level    string
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
			level:    ptr.Deref(d.Attributes[draAttrLevel].StringValue, ""),
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
			name:     "one device per NUMA node and socket with CPUs",
			reserved: "cpuset:0,1",
			expected: map[string]draDevice{
				"cpudevnuma000":   {level: "numanode", numaNode: 0, socketID: 0, smt: true, cpus: 27},
				"cpudevnuma001":   {level: "numanode", numaNode: 1, socketID: 1, smt: true, cpus: 27},
				"cpudevnuma002":   {level: "numanode", numaNode: 2, socketID: 0, smt: true, cpus: 28},
				"cpudevnuma003":   {level: "numanode", numaNode: 3, socketID: 1, smt: true, cpus: 28},
				"cpudevsocket000": {level: "socket", numaNode: -1, socketID: 0, smt: true, cpus: 55},
				"cpudevsocket001": {level: "socket", numaNode: -1, socketID: 1, smt: true, cpus: 55},
			},
		},
		{
			name:      "a node with no CPUs we may use is left out",
			available: "exclude-cpuset:" + node0,
			reserved:  "cpuset:1",
			expected: map[string]draDevice{
				"cpudevnuma001":   {level: "numanode", numaNode: 1, socketID: 1, smt: true, cpus: 27},
				"cpudevnuma002":   {level: "numanode", numaNode: 2, socketID: 0, smt: true, cpus: 28},
				"cpudevnuma003":   {level: "numanode", numaNode: 3, socketID: 1, smt: true, cpus: 28},
				"cpudevsocket000": {level: "socket", numaNode: -1, socketID: 0, smt: true, cpus: 28},
				"cpudevsocket001": {level: "socket", numaNode: -1, socketID: 1, smt: true, cpus: 55},
			},
		},
		{
			name:      "reserved CPUs are not published",
			available: "cpuset:0,8,12",
			reserved:  "cpuset:0,8",
			expected: map[string]draDevice{
				"cpudevnuma000":   {level: "numanode", numaNode: 0, socketID: 0, smt: true, cpus: 1},
				"cpudevsocket000": {level: "socket", numaNode: -1, socketID: 0, smt: true, cpus: 1},
			},
		},
		{
			// CPU 4 is kernel-isolated and belongs to node 0.
			name:     "a reserved CPU that is also isolated is still excluded",
			reserved: "cpuset:4",
			expected: map[string]draDevice{
				"cpudevnuma000":   {level: "numanode", numaNode: 0, socketID: 0, smt: true, cpus: 27},
				"cpudevnuma001":   {level: "numanode", numaNode: 1, socketID: 1, smt: true, cpus: 28},
				"cpudevnuma002":   {level: "numanode", numaNode: 2, socketID: 0, smt: true, cpus: 28},
				"cpudevnuma003":   {level: "numanode", numaNode: 3, socketID: 1, smt: true, cpus: 28},
				"cpudevsocket000": {level: "socket", numaNode: -1, socketID: 0, smt: true, cpus: 55},
				"cpudevsocket001": {level: "socket", numaNode: -1, socketID: 1, smt: true, cpus: 56},
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

// draTestContainer returns a container of the QoS class with the given CPU
// request, and the same limit if it is guaranteed.
func draTestContainer(id string, qos v1.PodQOSClass, cpu string) *pinnedContainer {
	c := &pinnedContainer{mockContainer: mockContainer{
		name:                   id,
		returnValueForGetID:    id,
		returnValueForQOSClass: qos,
		returnValueForGetResourceRequirements: v1.ResourceRequirements{
			Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse(cpu)},
		},
	}}
	if qos == v1.PodQOSGuaranteed {
		c.returnValueForGetResourceRequirements.Limits = c.returnValueForGetResourceRequirements.Requests
	}
	return c
}

// TestDRADevicesFollowFreeCPUs checks that the capacity of a device goes down
// by the CPUs containers hold exclusively, the CPUs shared containers need and
// the CPUs of claims on smaller devices inside it, but not by its own claims
// or those of larger devices.
func TestDRADevicesFollowFreeCPUs(t *testing.T) {
	sys := testServerSystem(t)
	p, owner := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	expected := map[string]int64{
		"cpudevnuma000": 28,
		"cpudevnuma001": 28,
		"cpudevnuma002": 28,
		"cpudevnuma003": 26,
		// Sockets 0 and 1 have nodes 0 and 2, and 1 and 3.
		"cpudevsocket000": 56,
		"cpudevsocket001": 54,
	}
	check := func(what string) {
		t.Helper()
		got := map[string]int64{}
		for name, d := range draDeviceSummary(owner.devices) {
			got[name] = d.cpus
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("%s: published capacities %v, expected %v", what, got, expected)
		}
	}
	check("at start")

	exclusive := draTestContainer("exclusive", v1.PodQOSGuaranteed, "2")
	if err := p.allocateResources(exclusive, "NUMA node #1"); err != nil {
		t.Fatalf("failed to allocate: %v", err)
	}
	expected["cpudevnuma001"] -= 2
	expected["cpudevsocket001"] -= 2
	check("after an exclusive allocation")

	// A claim on a node cannot leave it, so the socket device leaves its
	// CPUs out.
	allocateClaim(t, p, "a", draTestResult(0, "3"))
	expected["cpudevsocket000"] -= 3
	check("after a NUMA claim")

	// A claim on a socket lowers no node.
	allocateClaim(t, p, "b", draTestSocketResult(1, "4"))
	check("after a socket claim")

	shared := draTestContainer("shared", v1.PodQOSBurstable, "1500m")
	if err := p.allocateResources(shared, "NUMA node #2"); err != nil {
		t.Fatalf("failed to allocate: %v", err)
	}
	expected["cpudevnuma002"] -= 2
	expected["cpudevsocket000"] -= 2
	check("after a shared allocation")

	if err := p.ReleaseResources(exclusive); err != nil {
		t.Fatalf("failed to release: %v", err)
	}
	if err := p.ReleaseResources(shared); err != nil {
		t.Fatalf("failed to release: %v", err)
	}
	for _, uid := range []types.UID{"a", "b"} {
		if err := p.ReleaseClaim(uid); err != nil {
			t.Fatalf("failed to release claim: %v", err)
		}
	}
	expected["cpudevnuma001"] += 2
	expected["cpudevnuma002"] += 2
	expected["cpudevsocket000"] += 5
	expected["cpudevsocket001"] += 2
	check("after releasing everything")
}

// TestDRADevicesNearlyFull checks that a device with one free CPU has no step,
// which the API server would reject, and that a device with no free CPU is
// left out.
func TestDRADevicesNearlyFull(t *testing.T) {
	// Node 0 and socket 0 have CPUs 8 and 12 for claims, node 1 and socket 1
	// have CPU 1.
	p, owner := draTestPolicy(t, testServerSystem(t), draTestConfig("cpuset:0,1,8,12", "cpuset:0"))

	if err := p.allocateResources(draTestContainer("c0", v1.PodQOSGuaranteed, "1"), "NUMA node #0"); err != nil {
		t.Fatalf("failed to allocate: %v", err)
	}
	if err := p.allocateResources(draTestContainer("c1", v1.PodQOSGuaranteed, "1"), "NUMA node #1"); err != nil {
		t.Fatalf("failed to allocate: %v", err)
	}

	var names []string
	for _, d := range owner.devices {
		names = append(names, d.Name)
		capacity := d.Capacity[draCapacityCPU]
		r := capacity.RequestPolicy.ValidRange
		if capacity.Value.Value() != 1 || r.Min.Value() != 1 || r.Step != nil {
			t.Errorf("%s: published capacity %s with range min %s, step %v, expected 1 with min 1 and no step",
				d.Name, capacity.Value.String(), r.Min.String(), r.Step)
		}
	}
	if expected := []string{"cpudevnuma000", "cpudevsocket000"}; !slices.Equal(names, expected) {
		t.Errorf("published devices %v, expected %v", names, expected)
	}
}

// TestDRAPublishFailure checks that a failure to publish the devices does not
// fail an allocation.
func TestDRAPublishFailure(t *testing.T) {
	sys := testServerSystem(t)
	p, owner := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	owner.publishErr = fmt.Errorf("test failure")
	if err := p.allocateResources(draTestContainer("c", v1.PodQOSGuaranteed, "2"), ""); err != nil {
		t.Errorf("allocation failed with a failing publish: %v", err)
	}
	allocateClaim(t, p, "a", draTestResult(0, "2"))
}

// TestDRADeviceShape checks attributes of published devices.
func TestDRADeviceShape(t *testing.T) {
	_, owner := draTestPolicy(t, testServerSystem(t), draTestConfig("cpuset:0,8,12", "cpuset:0"))

	one := resource.NewQuantity(1, resource.DecimalSI)
	device := func(name, level string) resourceapi.Device {
		return resourceapi.Device{
			Name:                     name,
			AllowMultipleAllocations: ptr.To(true),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"dra.cpu/level":      {StringValue: ptr.To(level)},
				"dra.cpu/socketID":   {IntValue: ptr.To(int64(0))},
				"dra.cpu/smtEnabled": {BoolValue: ptr.To(true)},
				"dra.cpu/numCPUs":    {IntValue: ptr.To(int64(2))},
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
		}
	}
	numa := device("cpudevnuma000", "numanode")
	numa.Attributes[deviceattribute.StandardDeviceAttributeNUMANode] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(0))}
	expected := []resourceapi.Device{numa, device("cpudevsocket000", "socket")}

	if !apiequality.Semantic.DeepEqual(owner.devices, expected) {
		t.Errorf("published devices\n%+v\nexpected\n%+v", owner.devices, expected)
	}
}

// pcieSystem adds PCIe roots to the packages of a test system.
type pcieSystem struct {
	system.System
	roots map[idset.ID][]string
}

func (s *pcieSystem) Package(id idset.ID) system.CPUPackage {
	return &pciePackage{CPUPackage: s.System.Package(id), roots: s.roots[id]}
}

type pciePackage struct {
	system.CPUPackage
	roots []string
}

func (p *pciePackage) PCIeRoots() []string { return p.roots }

// TestDRAPCIeRoots checks that devices carry their package's PCIe roots, if
// any, and only if list attributes are supported.
func TestDRAPCIeRoots(t *testing.T) {
	sys := &pcieSystem{
		System: testServerSystem(t),
		roots:  map[idset.ID][]string{0: {"pci0000:00", "pci0000:5d"}},
	}
	p, owner := draTestPolicy(t, sys, draTestConfig("", "cpuset:0,1"))

	published := func() map[string][]string {
		roots := map[string][]string{}
		for _, d := range owner.devices {
			if attr, ok := d.Attributes[deviceattribute.StandardDeviceAttributePCIeRoot]; ok {
				roots[d.Name] = attr.StringValues
			}
		}
		return roots
	}

	if got := published(); len(got) != 0 {
		t.Errorf("published PCIe roots %v without list attribute support", got)
	}

	owner.listAttributesSupported = true
	if err := p.Reconfigure(draTestConfig("", "cpuset:0,1")); err != nil {
		t.Fatalf("failed to reconfigure policy: %v", err)
	}
	expected := map[string][]string{
		"cpudevnuma000":   {"pci0000:00", "pci0000:5d"},
		"cpudevnuma002":   {"pci0000:00", "pci0000:5d"},
		"cpudevsocket000": {"pci0000:00", "pci0000:5d"},
	}
	if got := published(); !reflect.DeepEqual(got, expected) {
		t.Errorf("published PCIe roots %v, expected %v", got, expected)
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
		Device:  fmt.Sprintf(draNUMADeviceFormat, node),
	}
	if cpus != "" {
		r.ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{
			draCapacityCPU: resource.MustParse(cpus),
		}
	}
	return r
}

// draTestSocketResult is an allocation result of one socket device.
func draTestSocketResult(socket int, cpus string) resourceapi.DeviceRequestAllocationResult {
	r := draTestResult(0, cpus)
	r.Device = fmt.Sprintf(draSocketDeviceFormat, socket)
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

// pinnedContainer records the cpuset, the CPU shares and the memory policy
// that our policy sets on it. A later call replaces an earlier one.
type pinnedContainer struct {
	mockContainer
	devices []string
	hints   topology.Hints
	strict  bool
	cpus    string
	mems    string
	shares  int64
	state   cache.ContainerState
	mpol    string
	// preserveCPUs and preserveMems keep cpus and mems as they are.
	preserveCPUs bool
	preserveMems bool
}

func (c *pinnedContainer) PreserveCpuResources() bool {
	return c.preserveCPUs
}

func (c *pinnedContainer) PreserveMemoryResources() bool {
	return c.preserveMems
}

func (c *pinnedContainer) GetCpusetCpus() string {
	return c.cpus
}

func (c *pinnedContainer) GetCpusetMems() string {
	return c.mems
}

func (c *pinnedContainer) SetCpusetMems(mems string) {
	c.mems = mems
}

func (c *pinnedContainer) GetState() cache.ContainerState {
	return c.state
}

func (c *pinnedContainer) SetMemoryPolicy(mode nri.MpolMode, nodes string, flags ...nri.MpolFlag) {
	c.mpol = fmt.Sprintf("%s:%s%v", mode, nodes, flags)
}

func (c *pinnedContainer) GetTopologyHints() topology.Hints {
	return c.hints
}

func (c *pinnedContainer) StrictTopologyHints() bool {
	return c.strict
}

func (c *pinnedContainer) SetCpusetCpus(cpus string) {
	c.cpus = cpus
}

func (c *pinnedContainer) SetCPUShares(shares int64) {
	c.shares = shares
}

func (c *pinnedContainer) GetCDIDevices() []string {
	return c.devices
}

// holds returns the names of the CDI devices our DRA driver writes for the
// given results of claim uid.
func holds(uid string, indexes ...int) []string {
	devices := []string{}
	for _, i := range indexes {
		devices = append(devices, fmt.Sprintf("%s/device=claim-%s-%d", draDriverName, uid, i))
	}
	return devices
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

// allocateClaim allocates a claim, failing the test on error, and returns its
// CPUs.
func allocateClaim(t *testing.T, p *policy, uid string, results ...resourceapi.DeviceRequestAllocationResult) cpuset.CPUSet {
	t.Helper()
	edits, err := p.AllocateClaim(draTestClaim(uid), results)
	if err != nil {
		t.Fatalf("failed to allocate claim %s: %v", uid, err)
	}
	if edits != nil {
		t.Fatalf("claim %s got edits %v, expected none", uid, edits)
	}
	return p.allocations.claims[uid].ExclusiveCPUs()
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

			if edits != nil {
				t.Errorf("claim got edits %v, expected none", edits)
			}
			claim := p.allocations.claims["a"]
			cpus := claim.ExclusiveCPUs()
			// Every result has CPUs of its own, as many as it consumed, all
			// on its device.
			if len(claim.results) != len(tc.results) {
				t.Fatalf("claim has %d results, expected %d", len(claim.results), len(tc.results))
			}
			all := cpuset.New()
			for i, r := range tc.results {
				deviceCPUs, _ := p.draDeviceCPUs(r.Device)
				q := r.ConsumedCapacity[draCapacityCPU]
				if got := claim.results[i]; got.Size() != int(q.Value()) || !got.IsSubsetOf(deviceCPUs) ||
					!got.Intersection(all).IsEmpty() {
					t.Errorf("result %d got CPUs %s, expected %d of device %s, none of %s",
						i, got, q.Value(), r.Device, all)
				}
				all = all.Union(claim.results[i])
			}
			if !all.Equals(cpus) {
				t.Errorf("results have CPUs %s, the claim %s", all, cpus)
			}
			for _, id := range sys.NodeIDs() {
				want := 0
				for _, r := range tc.results {
					if r.Device == fmt.Sprintf(draNUMADeviceFormat, id) {
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
	}); err == nil || !strings.Contains(err.Error(), "but consumed 3 CPUs of device cpudevnuma000") {
		t.Errorf("got error %v, expected a size mismatch", err)
	}
	// Nor move them to another node.
	if _, err := p.AllocateClaim(draTestClaim("a"), []resourceapi.DeviceRequestAllocationResult{
		draTestResult(1, "2"),
	}); err == nil || !strings.Contains(err.Error(), "holds CPUs of device cpudevnuma000, not of cpudevnuma001") {
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

// A container holding claims runs on their CPUs, and also on its own CPUs if
// it has a CPU request: exclusive, shared or reserved ones. It keeps its
// claimed CPUs as other claims come and go. A variable naming other CPUs or a
// claim we do not hold is ignored.
func TestPinClaimedCPUs(t *testing.T) {
	sys := testServerSystem(t)

	// Every case gets a policy of its own holding these claims, a on node 0
	// and b on node 1.
	setup := func(t *testing.T) (p *policy, a, b cpuset.CPUSet) {
		cfg := draTestConfig("", draTestReserved(sys))
		cfg.PinCPU = true
		p, _ = draTestPolicy(t, sys, cfg)
		a = allocateClaim(t, p, "a", draTestResult(0, "2"))
		b = allocateClaim(t, p, "b", draTestResult(1, "2"))
		return p, a, b
	}

	tcs := []struct {
		name      string
		namespace string
		qos       v1.PodQOSClass
		cpu       string
		devices   []string
		claims    []string
		exclusive int
		portion   int  // shared or reserved milli-CPU the container gets
		reserved  bool // the container gets reserved CPUs
	}{
		{
			name:    "besteffort",
			qos:     v1.PodQOSBestEffort,
			devices: holds("a", 0),
			claims:  []string{"a"},
		},
		{
			name:    "shared",
			qos:     v1.PodQOSBurstable,
			cpu:     "100m",
			devices: holds("a", 0),
			claims:  []string{"a"},
			portion: 100,
		},
		{
			name:      "reserved",
			namespace: "kube-system",
			qos:       v1.PodQOSBurstable,
			cpu:       "100m",
			devices:   holds("a", 0),
			claims:    []string{"a"},
			portion:   100,
			reserved:  true,
		},
		{
			// A reserved container with no CPU request runs on its claims
			// only, as a besteffort one does.
			name:      "reserved, no request",
			namespace: "kube-system",
			qos:       v1.PodQOSBestEffort,
			devices:   holds("a", 0),
			claims:    []string{"a"},
			reserved:  true,
		},
		{
			// 1500m gives one exclusive CPU and a 500m shared portion.
			name:      "exclusive and shared",
			qos:       v1.PodQOSGuaranteed,
			cpu:       "1500m",
			devices:   holds("a", 0),
			claims:    []string{"a"},
			exclusive: 1,
			portion:   500,
		},
		{
			name:      "exclusive",
			qos:       v1.PodQOSGuaranteed,
			cpu:       "2",
			devices:   holds("a", 0),
			claims:    []string{"a"},
			exclusive: 2,
		},
		{
			name:    "two claims",
			qos:     v1.PodQOSBurstable,
			cpu:     "100m",
			devices: append(holds("a", 0), holds("b", 0)...),
			claims:  []string{"a", "b"},
			portion: 100,
		},
		{
			// Claim a has a single result.
			name:    "unknown result",
			qos:     v1.PodQOSBurstable,
			cpu:     "100m",
			devices: holds("a", 1),
		},
		{
			name:    "unknown claim",
			qos:     v1.PodQOSBurstable,
			cpu:     "100m",
			devices: holds("x", 0),
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			p, a, b := setup(t)
			claimed := cpuset.New()
			for _, uid := range tc.claims {
				claimed = claimed.Union(map[string]cpuset.CPUSet{"a": a, "b": b}[uid])
			}

			c := &pinnedContainer{
				mockContainer: mockContainer{
					name:                   tc.name,
					namespace:              tc.namespace,
					returnValueForGetID:    tc.name,
					returnValueForQOSClass: tc.qos,
				},
				devices: tc.devices,
			}
			if tc.cpu != "" {
				c.returnValueForGetResourceRequirements = v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse(tc.cpu)},
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse(tc.cpu)},
				}
			}
			grant, err := p.allocatePool(c, "")
			if err != nil {
				t.Fatalf("failed to allocate: %v", err)
			}
			p.applyGrant(grant)

			exclusive := grant.ExclusiveCPUs()
			if exclusive.Size() != tc.exclusive {
				t.Fatalf("got exclusive CPUs %s, expected %d", exclusive, tc.exclusive)
			}
			pinned := cpuset.MustParse(c.cpus)
			if claimed.IsEmpty() {
				if pinned.IsEmpty() || !pinned.Intersection(a.Union(b)).IsEmpty() {
					t.Errorf("pinned to %s, expected shared CPUs, none of %s", pinned, a.Union(b))
				}
				return
			}
			own := exclusive
			switch {
			case tc.portion > 0 && tc.reserved:
				own = grant.ReservedCPUs()
			case tc.portion > 0:
				own = own.Union(grant.SharedCPUs())
			}
			if want := own.Union(claimed); !pinned.Equals(want) {
				t.Errorf("pinned to %s, expected %s", pinned, want)
			}

			// A container with normal CPUs goes to the pool of its claims,
			// so its memory comes from there. A container with reserved CPUs
			// stays in the root pool, where the reserved CPUs are.
			pool := p.poolForCPUs(claimed)
			if tc.reserved {
				pool = p.root
			}
			if got := grant.GetCPUNode(); !got.IsSameNode(pool) {
				t.Errorf("granted from %s, expected %s", got.Name(), pool.Name())
			}
			mems := libmem.NewNodeMask(pool.GetMemset(memoryAll).Members()...)
			if zone := grant.GetMemoryZone(); zone == 0 || zone.And(mems) != zone {
				t.Errorf("got memory zone %s, expected one within %s", zone, mems)
			}
			if portion := grant.CPUPortion(); portion != tc.portion {
				t.Errorf("granted %dm shared or reserved CPU, expected %dm", portion, tc.portion)
			}
			// As for mixed exclusive and shared CPUs, the CPU shares come from
			// the shared or reserved portion if there is one, otherwise from
			// all CPUs the container runs on.
			milliCPU := tc.portion
			if milliCPU == 0 {
				milliCPU = 1000 * pinned.Size()
			}
			if want := cache.MilliCPUToShares(int64(milliCPU)); c.shares != int64(want) {
				t.Errorf("got CPU shares %d, expected %d", c.shares, want)
			}

			// While another claim is held, the container keeps its claimed
			// CPUs and does not run on the other claim's CPUs. Once that
			// claim is released, the container is back where it started.
			// Node 0 has two isolated CPUs, so a claim for four takes some
			// shared CPUs too.
			cpus := allocateClaim(t, p, "c", draTestResult(0, "4"))
			if got := cpuset.MustParse(c.cpus); !claimed.IsSubsetOf(got) || !got.Intersection(cpus).IsEmpty() {
				t.Errorf("pinned to %s while claim c holds %s, expected all of %s and none of %s",
					got, cpus, claimed, cpus)
			}
			if err := p.ReleaseClaim("c"); err != nil {
				t.Fatalf("failed to release claim c: %v", err)
			}
			if got := cpuset.MustParse(c.cpus); !got.Equals(pinned) {
				t.Errorf("pinned to %s after claim c came and went, expected %s", got, pinned)
			}
		})
	}
}

// A container exports the CPUs of its claims apart from its own CPUs.
func TestExportClaimedCPUs(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))
	a := allocateClaim(t, p, "a", draTestResult(0, "2"))

	tcs := []struct {
		name    string
		qos     v1.PodQOSClass
		cpu     string
		devices []string
		claimed cpuset.CPUSet
	}{
		{"besteffort", v1.PodQOSBestEffort, "", holds("a", 0), a},
		{"exclusive", v1.PodQOSGuaranteed, "2", holds("a", 0), a},
		{"no claim", v1.PodQOSBurstable, "100m", nil, cpuset.New()},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			c := &pinnedContainer{
				mockContainer: mockContainer{
					name:                   tc.name,
					returnValueForGetID:    tc.name,
					returnValueForQOSClass: tc.qos,
				},
				devices: tc.devices,
			}
			if tc.cpu != "" {
				c.returnValueForGetResourceRequirements = v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse(tc.cpu)},
					Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse(tc.cpu)},
				}
			}
			grant, err := p.allocatePool(c, "")
			if err != nil {
				t.Fatalf("failed to allocate: %v", err)
			}
			p.allocations.addGrant(grant)

			data := p.ExportResourceData(c)
			got, ok := data[policyapi.ExportClaimedCPUs]
			switch {
			case tc.claimed.IsEmpty() && ok:
				t.Errorf("exported %s=%q, expected none", policyapi.ExportClaimedCPUs, got)
			case !tc.claimed.IsEmpty() && got != tc.claimed.String():
				t.Errorf("exported %s=%q, expected %q", policyapi.ExportClaimedCPUs, got, tc.claimed)
			}
			for _, key := range []string{policyapi.ExportSharedCPUs, policyapi.ExportIsolatedCPUs, policyapi.ExportExclusiveCPUs} {
				if own, ok := data[key]; ok && !cpuset.MustParse(own).Intersection(tc.claimed).IsEmpty() {
					t.Errorf("exported %s=%q, which has claimed CPUs %s", key, own, tc.claimed)
				}
			}
		})
	}
}

// A container naming some requests of a claim runs only on the CPUs of those
// requests, and the policy exports these CPUs to it as its claimed CPUs.
func TestPinClaimRequests(t *testing.T) {
	sys := testServerSystem(t)
	cfg := draTestConfig("", draTestReserved(sys))
	cfg.PinCPU = true
	p, _ := draTestPolicy(t, sys, cfg)

	allocateClaim(t, p, "a", draTestResult(0, "2"), draTestResult(1, "2"))
	results := p.allocations.claims["a"].results

	for _, tc := range []struct {
		name    string
		devices []string
		expect  cpuset.CPUSet
	}{
		{"first request", holds("a", 0), results[0]},
		{"second request", holds("a", 1), results[1]},
		{"both requests", holds("a", 0, 1), results[0].Union(results[1])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &pinnedContainer{
				mockContainer: mockContainer{
					name:                   tc.name,
					returnValueForGetID:    tc.name,
					returnValueForQOSClass: v1.PodQOSBestEffort,
				},
				devices: tc.devices,
			}
			grant, err := p.allocatePool(c, "")
			if err != nil {
				t.Fatalf("failed to allocate: %v", err)
			}
			p.applyGrant(grant)
			p.allocations.addGrant(grant)
			defer p.releasePool(c)

			if pinned := cpuset.MustParse(c.cpus); !pinned.Equals(tc.expect) {
				t.Errorf("pinned to %s, expected %s", pinned, tc.expect)
			}
			if got := p.ExportResourceData(c)[policyapi.ExportClaimedCPUs]; got != tc.expect.String() {
				t.Errorf("exported claimed CPUs %q, expected %q", got, tc.expect)
			}
		})
	}
}

// The CPUs of each result of a claim are saved and restored with the claim.
func TestClaimResultsSurviveRestore(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	allocateClaim(t, p, "a", draTestResult(0, "2"), draTestResult(1, "3"))

	data, err := p.allocations.MarshalJSON()
	if err != nil {
		t.Fatalf("failed to save allocations: %v", err)
	}
	restored := p.newAllocations()
	if err := restored.UnmarshalJSON(data); err != nil {
		t.Fatalf("failed to restore allocations: %v", err)
	}

	want, got := p.allocations.claims["a"].results, restored.claims["a"].results
	if len(got) != len(want) || !got[0].Equals(want[0]) || !got[1].Equals(want[1]) {
		t.Errorf("restored results %v, expected %v", got, want)
	}
}

// A besteffort container holding a claim does not run on shared CPUs, so it
// holds back no shared capacity of its pool: the rest of its node can still
// be claimed.
func TestClaimingContainerSparesSharedCPUs(t *testing.T) {
	sys := testServerSystem(t)
	cfg := draTestConfig("", draTestReserved(sys))
	cfg.PinCPU = true
	p, _ := draTestPolicy(t, sys, cfg)

	allocateClaim(t, p, "a", draTestResult(0, "2"))
	c := &pinnedContainer{
		mockContainer: mockContainer{
			name:                   "claimer",
			returnValueForGetID:    "claimer",
			returnValueForQOSClass: v1.PodQOSBestEffort,
		},
		devices: holds("a", 0),
	}
	grant, err := p.allocatePool(c, "")
	if err != nil {
		t.Fatalf("failed to allocate: %v", err)
	}
	p.applyGrant(grant)

	left := freeCPUs(p).Intersection(sys.Node(0).CPUSet()).Size()
	allocateClaim(t, p, "b", draTestResult(0, fmt.Sprint(left)))
}

// Claims are allocated before their container and its hints are known, so
// strict topology hints apply only to the container's own CPUs. A container
// whose claimed CPUs fail the hint is allocated anyway and keeps the claim.
func TestClaimStrictTopologyHint(t *testing.T) {
	sys := testServerSystem(t)

	for _, tc := range []struct {
		name    string
		aligned bool
	}{
		{name: "aligned claim", aligned: true},
		{name: "misaligned claim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := draTestConfig("", draTestReserved(sys))
			cfg.PinCPU = true
			p, _ := draTestPolicy(t, sys, cfg)

			a := allocateClaim(t, p, "a", draTestResult(0, "2"))
			hinted := sys.Node(0).CPUSet()
			if !tc.aligned {
				hinted = hinted.Difference(a)
			}
			c := &pinnedContainer{
				mockContainer: mockContainer{
					name:                   "claimer",
					returnValueForGetID:    "claimer",
					returnValueForQOSClass: v1.PodQOSBestEffort,
				},
				devices: holds("a", 0),
				hints:   topology.Hints{"test": {CPUs: hinted.String()}},
				strict:  true,
			}

			grant, err := p.allocatePool(c, "")
			if err != nil {
				t.Fatalf("failed to allocate: %v", err)
			}
			if !grant.ClaimedCPUs().Equals(a) {
				t.Errorf("holding %s, expected the claim's %s", grant.ClaimedCPUs(), a)
			}
		})
	}
}

// A container holding a claim stays on it across a reconfiguration.
func TestReconfigureKeepsClaimer(t *testing.T) {
	sys := testServerSystem(t)
	cfg := draTestConfig("", draTestReserved(sys))
	cfg.PinCPU = true
	p, _ := draTestPolicy(t, sys, cfg)

	a := allocateClaim(t, p, "a", draTestResult(0, "2"))
	c := &pinnedContainer{
		mockContainer: mockContainer{
			name:                   "claimer",
			returnValueForGetID:    "claimer",
			returnValueForQOSClass: v1.PodQOSBestEffort,
		},
		devices: holds("a", 0),
	}
	grant, err := p.allocatePool(c, "")
	if err != nil {
		t.Fatalf("failed to allocate: %v", err)
	}
	p.applyGrant(grant)
	if pinned := cpuset.MustParse(c.cpus); !pinned.Equals(a) {
		t.Fatalf("pinned to %s, expected the claim's %s", pinned, a)
	}

	cfg = draTestConfig("", draTestReserved(sys))
	cfg.PinCPU = true
	cfg.ColocatePods = true
	if err := p.Reconfigure(cfg); err != nil {
		t.Fatalf("failed to reconfigure policy: %v", err)
	}
	grant, _ = p.allocations.getGrant("claimer")
	if pinned := cpuset.MustParse(c.cpus); !pinned.Equals(a) || !grant.ClaimedCPUs().Equals(a) {
		t.Errorf("pinned to %s, holding %s after reconfiguring, expected the claim's %s",
			pinned, grant.ClaimedCPUs(), a)
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

// draRestart sets up and starts a policy with its cache under dir, as the
// plugin does after a restart, and returns the error of its setup.
func draRestart(t *testing.T, dir string, sys system.System, cfg *cfgapi.Config) (*policy, error) {
	t.Helper()
	cch, err := cache.NewCache(cache.Options{CacheDir: filepath.Join(dir, "cache")})
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	p := New().(*policy)
	if err := p.Setup(&policyapi.BackendOptions{
		Cache:  cch,
		System: sys,
		Owner:  &draTestOwner{},
		Config: cfg,
	}); err != nil {
		return nil, err
	}
	if err := p.Start(); err != nil {
		t.Fatalf("failed to start policy: %v", err)
	}
	return p, nil
}

// TestRestoreClaims verifies that claims keep their CPUs across a restart,
// including a claim with no container, and that a container created later
// is pinned to the CPUs of its claim.
func TestRestoreClaims(t *testing.T) {
	sys := testServerSystem(t)
	dir := t.TempDir()
	cfg := draTestConfig("", draTestReserved(sys))
	cfg.PinCPU = true
	p, err := draRestart(t, dir, sys, cfg)
	if err != nil {
		t.Fatalf("failed to set up policy: %v", err)
	}

	a := allocateClaim(t, p, "a", draTestResult(0, "2"))
	b := allocateClaim(t, p, "b", draTestResult(1, "2"), draTestResult(2, "1"))
	free := freeCPUs(p)
	results := p.allocations.claims["b"].results

	p, err = draRestart(t, dir, sys, cfg)
	if err != nil {
		t.Fatalf("failed to restart policy: %v", err)
	}
	if len(p.allocations.claims) != 2 {
		t.Fatalf("restored %d claims, expected 2", len(p.allocations.claims))
	}
	if got := p.allocations.claims["a"].ExclusiveCPUs(); !got.Equals(a) {
		t.Errorf("claim a has CPUs %s after a restart, expected %s", got, a)
	}
	if got := p.allocations.claims["b"].ExclusiveCPUs(); !got.Equals(b) {
		t.Errorf("claim b has CPUs %s after a restart, expected %s", got, b)
	}
	if got := p.allocations.claims["b"].results; !reflect.DeepEqual(got, results) {
		t.Errorf("claim b has results %v after a restart, expected %v", got, results)
	}
	if got := freeCPUs(p); !got.Equals(free) {
		t.Errorf("free CPUs are %s after a restart, expected %s", got, free)
	}

	c := &pinnedContainer{
		mockContainer: mockContainer{
			name:                   "claimer",
			returnValueForGetID:    "claimer",
			returnValueForQOSClass: v1.PodQOSBestEffort,
		},
		devices: holds("a", 0),
	}
	grant, err := p.allocatePool(c, "")
	if err != nil {
		t.Fatalf("failed to allocate: %v", err)
	}
	p.applyGrant(grant)
	if pinned := cpuset.MustParse(c.cpus); !pinned.Equals(a) {
		t.Errorf("pinned to %s after a restart, expected the claim's %s", pinned, a)
	}
}

// TestRestoreClaimsRefused verifies that a restart fails on a claim whose
// node has too few CPUs for it, naming the claim, and that the claim is
// restored once the cause is gone.
func TestRestoreClaimsRefused(t *testing.T) {
	sys := testServerSystem(t)

	for _, tc := range []struct {
		name string
		// restart restarts the policy so that it cannot hold claim a.
		restart func(t *testing.T, dir string, a cpuset.CPUSet) error
		err     string
	}{
		{
			name: "claimed CPUs reserved",
			restart: func(t *testing.T, dir string, a cpuset.CPUSet) error {
				cfg := draTestConfig("", "cpuset:"+cpuset.New(a.List()[0]).String())
				_, err := draRestart(t, dir, sys, cfg)
				return err
			},
			err: "because they are reserved",
		},
		{
			// An isolated CPU may be reserved if it is the only one.
			name: "claimed isolated CPU reserved",
			restart: func(t *testing.T, dir string, a cpuset.CPUSet) error {
				isolated := a.Intersection(sys.IsolatedCPUs())
				if isolated.IsEmpty() {
					t.Fatalf("claim a holds no isolated CPUs: %s", a)
				}
				cfg := draTestConfig("", "cpuset:"+cpuset.New(isolated.List()[0]).String())
				_, err := draRestart(t, dir, sys, cfg)
				return err
			},
			err: "because they are reserved",
		},
		{
			name: "claimed CPUs not available",
			restart: func(t *testing.T, dir string, a cpuset.CPUSet) error {
				available := sys.CPUSet().Difference(cpuset.New(a.List()[0]))
				cfg := draTestConfig("cpuset:"+available.String(), draTestReserved(sys))
				_, err := draRestart(t, dir, sys, cfg)
				return err
			},
			err: "can't reserve exclusive CPUs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := draTestConfig("", draTestReserved(sys))
			p, err := draRestart(t, dir, sys, cfg)
			if err != nil {
				t.Fatalf("failed to set up policy: %v", err)
			}
			// Claim all of node 0, its isolated CPUs too.
			a := allocateClaim(t, p, "a", draTestResult(0, fmt.Sprint(sys.Node(0).CPUSet().Size())))

			err = tc.restart(t, dir, a)
			if err == nil || !strings.Contains(err.Error(), "DRA claim a") || !strings.Contains(err.Error(), tc.err) ||
				!strings.Contains(err.Error(), "failed to move it") {
				t.Fatalf("got error %v restarting, expected one naming claim a and %q", err, tc.err)
			}

			p, err = draRestart(t, dir, sys, cfg)
			if err != nil {
				t.Fatalf("failed to restart policy with the old config: %v", err)
			}
			if _, ok := p.allocations.claims["a"]; !ok {
				t.Fatalf("claim a not restored with the old config")
			}
			if got := p.allocations.claims["a"].ExclusiveCPUs(); !got.Equals(a) {
				t.Errorf("claim a has CPUs %s after a restart, expected %s", got, a)
			}
		})
	}
}

// TestRestoreClaimsMoved verifies that a restart moves a claim which cannot
// keep its CPUs to other CPUs of the same node. Each result gets as many CPUs
// as before, and the other claims keep their CPUs.
func TestRestoreClaimsMoved(t *testing.T) {
	sys := testServerSystem(t)
	node0 := sys.Node(0).CPUSet()

	for _, tc := range []struct {
		name string
		// restart restarts the policy so that claim a cannot keep its CPUs.
		restart func(t *testing.T, dir string, a cpuset.CPUSet) (*policy, error)
		// keepsB tells whether claim b keeps its CPUs.
		keepsB bool
	}{
		{
			name: "claimed CPU reserved",
			restart: func(t *testing.T, dir string, a cpuset.CPUSet) (*policy, error) {
				cfg := draTestConfig("", "cpuset:"+cpuset.New(a.List()[0]).String())
				return draRestart(t, dir, sys, cfg)
			},
			keepsB: true,
		},
		{
			name: "pool gone",
			restart: func(t *testing.T, dir string, a cpuset.CPUSet) (*policy, error) {
				file := filepath.Join(dir, "cache", "cache")
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatalf("failed to read cache: %v", err)
				}
				gone := strings.ReplaceAll(string(data), "NUMA node #0", "NUMA node #9")
				if err := os.WriteFile(file, []byte(gone), 0o600); err != nil {
					t.Fatalf("failed to write cache: %v", err)
				}
				return draRestart(t, dir, sys, draTestConfig("", draTestReserved(sys)))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p, err := draRestart(t, dir, sys, draTestConfig("", draTestReserved(sys)))
			if err != nil {
				t.Fatalf("failed to set up policy: %v", err)
			}
			b := allocateClaim(t, p, "b", draTestResult(0, "2"))
			a := allocateClaim(t, p, "a", draTestResult(0, "1"), draTestResult(0, "2"))

			p, err = tc.restart(t, dir, a)
			if err != nil {
				t.Fatalf("failed to restart policy: %v", err)
			}
			if tc.keepsB {
				if got := p.allocations.claims["b"].ExclusiveCPUs(); !got.Equals(b) {
					t.Errorf("claim b has CPUs %s after a restart, expected %s", got, b)
				}
			}

			taken := p.reserved
			for _, uid := range []string{"a", "b"} {
				c, ok := p.allocations.claims[uid]
				if !ok {
					t.Fatalf("claim %s not restored", uid)
				}
				sizes := map[string][]int{"a": {1, 2}, "b": {2}}[uid]
				if len(c.results) != len(sizes) {
					t.Fatalf("claim %s has %d results after a restart, expected %d", uid, len(c.results), len(sizes))
				}
				all := cpuset.New()
				for i, cpus := range c.results {
					if cpus.Size() != sizes[i] || !cpus.IsSubsetOf(node0) || !cpus.Intersection(taken).IsEmpty() {
						t.Errorf("result #%d of claim %s has CPUs %s after a restart, expected %d CPUs of node 0 not in %s",
							i, uid, cpus, sizes[i], taken)
					}
					taken = taken.Union(cpus)
					all = all.Union(cpus)
				}
				if got := c.ExclusiveCPUs(); !got.Equals(all) {
					t.Errorf("claim %s has CPUs %s after a restart, expected those of its results %s", uid, got, all)
				}
			}
		})
	}
}

// TestRestoreClaimDevices verifies that claims keep their devices across a
// restart, that a socket claim which cannot keep its CPUs moves to other CPUs
// of its socket, and that a claim saved without its devices gets the NUMA
// devices of its CPUs.
func TestRestoreClaimDevices(t *testing.T) {
	sys := testServerSystem(t)
	dir := t.TempDir()
	p, err := draRestart(t, dir, sys, draTestConfig("", draTestReserved(sys)))
	if err != nil {
		t.Fatalf("failed to set up policy: %v", err)
	}
	allocateClaim(t, p, "a", draTestResult(0, "2"))
	b := allocateClaim(t, p, "b", draTestSocketResult(1, "3"))

	devices := func(p *policy, uid string) []string {
		t.Helper()
		c, ok := p.allocations.claims[uid]
		if !ok {
			t.Fatalf("claim %s not restored", uid)
		}
		return c.devices
	}

	// Reserving a CPU of claim b moves it. Reserved CPUs must not mix
	// isolated and normal ones.
	gone := b.Difference(sys.IsolatedCPUs()).List()[0]
	cfg := draTestConfig("", draTestReserved(sys)+","+cpuset.New(gone).String())
	if p, err = draRestart(t, dir, sys, cfg); err != nil {
		t.Fatalf("failed to restart policy: %v", err)
	}
	if got := devices(p, "a"); !slices.Equal(got, []string{"cpudevnuma000"}) {
		t.Errorf("claim a has devices %v after a restart, expected cpudevnuma000", got)
	}
	if got := devices(p, "b"); !slices.Equal(got, []string{"cpudevsocket001"}) {
		t.Errorf("claim b has devices %v after a restart, expected cpudevsocket001", got)
	}
	moved := p.allocations.claims["b"].ExclusiveCPUs()
	if moved.Size() != 3 || !moved.IsSubsetOf(sys.Package(1).CPUSet()) || moved.Contains(gone) {
		t.Errorf("claim b has CPUs %s after a restart, expected 3 CPUs of socket 1 without %d", moved, gone)
	}

	// A cache with no devices for claims, as older versions write it.
	file := filepath.Join(dir, "cache", "cache")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("failed to read cache: %v", err)
	}
	old := regexp.MustCompile(`,?\\?"Devices\\?":\[[^\]]*\]`).ReplaceAllString(string(data), "")
	if old == string(data) {
		t.Fatalf("no devices to remove in the cache")
	}
	if err := os.WriteFile(file, []byte(old), 0o600); err != nil {
		t.Fatalf("failed to write cache: %v", err)
	}
	if p, err = draRestart(t, dir, sys, cfg); err != nil {
		t.Fatalf("failed to restart policy: %v", err)
	}
	if got := devices(p, "a"); !slices.Equal(got, []string{"cpudevnuma000"}) {
		t.Errorf("claim a has devices %v after a restart from an old cache, expected cpudevnuma000", got)
	}
	allocateClaim(t, p, "a", draTestResult(0, "2"))
}

// TestSocketClaims verifies that a socket request gets CPUs of its socket, of
// one NUMA node when one has enough free CPUs, of the node with the fewest of
// them, and of several nodes only when it needs more than one has.
func TestSocketClaims(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	nodes := func(cpus cpuset.CPUSet) []idset.ID {
		var ids []idset.ID
		for _, id := range sys.NodeIDs() {
			if !cpus.Intersection(sys.Node(id).CPUSet()).IsEmpty() {
				ids = append(ids, id)
			}
		}
		return ids
	}

	// Node 0 has 28 CPUs, its sibling node 2 too. The CPU IDs of the two
	// nodes alternate, so picking from the whole socket would take both.
	if got := nodes(allocateClaim(t, p, "a", draTestSocketResult(0, "4"))); len(got) != 1 {
		t.Errorf("a claim of 4 CPUs got CPUs of nodes %v, expected one node", got)
	}
	// Now one node has 24 free CPUs: a claim which fits goes there.
	busy := nodes(p.allocations.claims["a"].ExclusiveCPUs())[0]
	if got := nodes(allocateClaim(t, p, "b", draTestSocketResult(0, "20"))); !slices.Equal(got, []idset.ID{busy}) {
		t.Errorf("a claim of 20 CPUs got CPUs of nodes %v, expected node %d with the fewest free CPUs", got, busy)
	}
	if got := nodes(allocateClaim(t, p, "c", draTestSocketResult(0, "26"))); len(got) != 1 || got[0] == busy {
		t.Errorf("a claim of 26 CPUs got CPUs of nodes %v, expected the other node", got)
	}

	// Socket 1 has 54 CPUs: a claim of 40 spans its nodes, and two requests
	// of a claim get disjoint CPUs.
	d := allocateClaim(t, p, "d", draTestSocketResult(1, "40"))
	if d.Size() != 40 || !d.IsSubsetOf(sys.Package(1).CPUSet()) || len(nodes(d)) != 2 {
		t.Errorf("a claim of 40 CPUs got CPUs %s, expected 40 CPUs of both nodes of socket 1", d)
	}
	allocateClaim(t, p, "e", draTestSocketResult(1, "3"), draTestSocketResult(1, "3"))
	e := p.allocations.claims["e"]
	if r := e.results; r[0].Size() != 3 || r[1].Size() != 3 || !r[0].Intersection(r[1]).IsEmpty() ||
		!e.ExclusiveCPUs().IsSubsetOf(sys.Package(1).CPUSet()) || !e.ExclusiveCPUs().Intersection(d).IsEmpty() {
		t.Errorf("two requests of 3 CPUs got CPUs %v, expected disjoint CPUs of socket 1 apart from %s", r, d)
	}
}

// TestDRADeviceCPUs verifies the CPUs of devices by name.
func TestDRADeviceCPUs(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", "cpuset:0"))

	for _, tc := range []struct {
		device string
		cpus   cpuset.CPUSet
		ok     bool
	}{
		{device: "cpudevnuma000", cpus: sys.Node(0).CPUSet().Difference(cpuset.New(0)), ok: true},
		{device: "cpudevnuma003", cpus: sys.Node(3).CPUSet(), ok: true},
		{device: "cpudevsocket000", cpus: sys.Package(0).CPUSet().Difference(cpuset.New(0)), ok: true},
		{device: "cpudevsocket001", cpus: sys.Package(1).CPUSet(), ok: true},
		{device: "cpudevsocket0"},
		{device: "cpudevsocket1000"},
		{device: "cpudevsocket002"},
		{device: "cpudevnuma0"},
		{device: "cpudevnuma009"},
		{device: "cpudev000"},
	} {
		cpus, ok := p.draDeviceCPUs(tc.device)
		if ok != tc.ok || ok && !cpus.Equals(tc.cpus) {
			t.Errorf("device %s has CPUs %s, %v, expected %s, %v", tc.device, cpus, ok, tc.cpus, tc.ok)
		}
	}
}

// draTestMemoryPolicy is a claim config with a memory policy of the given
// mode and nodes, for the given requests.
func draTestMemoryPolicy(source resourceapi.AllocationConfigSource, mode, nodes string, requests ...string) resourceapi.DeviceAllocationConfiguration {
	return draTestClaimConfig(source, fmt.Sprintf(
		`{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig","memoryPolicy":{"mode":%q,"nodes":%q}}`,
		mode, nodes), requests...)
}

// draTestClaimConfig is a claim config with the given parameters.
func draTestClaimConfig(source resourceapi.AllocationConfigSource, params string, requests ...string) resourceapi.DeviceAllocationConfiguration {
	return resourceapi.DeviceAllocationConfiguration{
		Source:   source,
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver:     draDriverName,
				Parameters: runtime.RawExtension{Raw: []byte(params)},
			},
		},
	}
}

// draTestClaimWithConfigs is a claim with the given configs.
func draTestClaimWithConfigs(uid string, configs ...resourceapi.DeviceAllocationConfiguration) *resourceapi.ResourceClaim {
	claim := draTestClaim(uid)
	claim.Status.Allocation = &resourceapi.AllocationResult{
		Devices: resourceapi.DeviceAllocationResult{Config: configs},
	}
	return claim
}

// draTestRequest is draTestResult for the named request.
func draTestRequest(request string, node int, cpus string) resourceapi.DeviceRequestAllocationResult {
	r := draTestResult(node, cpus)
	r.Request = request
	return r
}

// Test that each claim result gets the memory policy of the configs for its
// request, and that a later config overrides an earlier one.
func TestClaimMemoryPolicies(t *testing.T) {
	const (
		class = resourceapi.AllocationConfigSourceClass
		own   = resourceapi.AllocationConfigSourceClaim
	)
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))
	interleave := &cfgapi.MemoryPolicy{Mode: "MPOL_INTERLEAVE", Nodes: "cpu-nodes"}
	bind := &cfgapi.MemoryPolicy{Mode: "MPOL_BIND", Nodes: "0"}
	results := []resourceapi.DeviceRequestAllocationResult{
		draTestRequest("a", 0, "1"),
		draTestRequest("b/small", 1, "1"),
	}

	for _, tc := range []struct {
		name    string
		configs []resourceapi.DeviceAllocationConfiguration
		expect  []*cfgapi.MemoryPolicy
	}{
		{"no configs", nil, nil},
		{
			"no memory policy",
			[]resourceapi.DeviceAllocationConfiguration{
				draTestClaimConfig(own, `{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig"}`),
			},
			nil,
		},
		{
			"all requests",
			[]resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "cpu-nodes")},
			[]*cfgapi.MemoryPolicy{interleave, interleave},
		},
		{
			"claim replaces class",
			[]resourceapi.DeviceAllocationConfiguration{
				draTestMemoryPolicy(class, "interleave", "cpu-nodes"),
				draTestMemoryPolicy(own, "bind", "0"),
			},
			[]*cfgapi.MemoryPolicy{bind, bind},
		},
		{
			"one request",
			[]resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "bind", "0", "a")},
			[]*cfgapi.MemoryPolicy{bind, nil},
		},
		{
			"main request of a subrequest",
			[]resourceapi.DeviceAllocationConfiguration{
				draTestMemoryPolicy(class, "interleave", "cpu-nodes"),
				draTestMemoryPolicy(own, "bind", "0", "b"),
			},
			[]*cfgapi.MemoryPolicy{interleave, bind},
		},
		{
			"subrequest",
			[]resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "bind", "0", "b/small")},
			[]*cfgapi.MemoryPolicy{nil, bind},
		},
		{
			"another subrequest",
			[]resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "bind", "0", "b/large")},
			nil,
		},
		{
			"relative nodes are indices",
			[]resourceapi.DeviceAllocationConfiguration{
				draTestClaimConfig(own, `{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig",`+
					`"memoryPolicy":{"mode":"bind","nodes":"63","flags":["relative-nodes"]}}`),
			},
			[]*cfgapi.MemoryPolicy{
				{Mode: "MPOL_BIND", Nodes: "63", Flags: []string{"MPOL_F_RELATIVE_NODES"}},
				{Mode: "MPOL_BIND", Nodes: "63", Flags: []string{"MPOL_F_RELATIVE_NODES"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.AllocateClaim(draTestClaimWithConfigs(tc.name, tc.configs...), results); err != nil {
				t.Fatalf("failed to allocate claim: %v", err)
			}
			if got := p.allocations.claims[tc.name].mpols; !reflect.DeepEqual(got, tc.expect) {
				t.Errorf("got memory policies %v, expected %v", got, tc.expect)
			}
		})
	}
}

// Test that a claim with a bad config is refused and allocates nothing.
func TestClaimConfigRefused(t *testing.T) {
	const own = resourceapi.AllocationConfigSourceClaim
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))
	free := freeCPUs(p)

	for _, tc := range []struct {
		name   string
		config resourceapi.DeviceAllocationConfiguration
		errMsg string
	}{
		{
			"no kind",
			draTestClaimConfig(own, `{"memoryPolicy":{"mode":"interleave"}}`),
			`got apiVersion "" and kind ""`,
		},
		{
			"another kind",
			draTestClaimConfig(own, `{"apiVersion":"config.nri/v1alpha1","kind":"BalloonsClaimConfig"}`),
			`kind "BalloonsClaimConfig"`,
		},
		{
			"another version",
			draTestClaimConfig(own, `{"apiVersion":"config.nri/v1","kind":"TopologyAwareClaimConfig"}`),
			`apiVersion "config.nri/v1"`,
		},
		{
			"unknown field",
			draTestClaimConfig(own, `{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig","cpuClass":"hp"}`),
			`unknown field "cpuClass"`,
		},
		{
			"not JSON",
			draTestClaimConfig(own, `{`),
			"config 1:",
		},
		{
			"unknown mode",
			draTestMemoryPolicy(own, "spread", ""),
			`unknown "spread"`,
		},
		{
			"MPOL_ mode",
			draTestMemoryPolicy(own, "MPOL_BIND", "0"),
			`use "bind" in claims`,
		},
		{
			"bad nodes",
			draTestMemoryPolicy(own, "bind", "max-dist:far"),
			`invalid memory policy nodes "max-dist:far"`,
		},
		{
			"max-dist below 10",
			draTestMemoryPolicy(own, "interleave", "max-dist:9"),
			`invalid memory policy nodes "max-dist:9": distance is below 10`,
		},
		{
			"zero max-dist",
			draTestMemoryPolicy(own, "interleave", "max-dist:0"),
			`invalid memory policy nodes "max-dist:0": distance is below 10`,
		},
		{
			"negative max-dist",
			draTestMemoryPolicy(own, "interleave", "max-dist:-1"),
			`invalid memory policy nodes "max-dist:-1": distance is below 10`,
		},
		{
			"nodes not on the host",
			draTestMemoryPolicy(own, "bind", "63"),
			"has no memory node of this host",
		},
		{
			"nodes for local",
			draTestMemoryPolicy(own, "local", "0"),
			"takes no nodes",
		},
		{
			"static and relative nodes",
			draTestClaimConfig(own, `{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig",`+
				`"memoryPolicy":{"mode":"bind","nodes":"0","flags":["static-nodes","relative-nodes"]}}`),
			"static-nodes and relative-nodes exclude each other",
		},
		{
			"relative-nodes with named nodes",
			draTestClaimConfig(own, `{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig",`+
				`"memoryPolicy":{"mode":"bind","nodes":"cpu-nodes","flags":["relative-nodes"]}}`),
			`relative-nodes needs a list of node indices, got "cpu-nodes"`,
		},
		{
			"numa-balancing for interleave",
			draTestClaimConfig(own, `{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig",`+
				`"memoryPolicy":{"mode":"interleave","flags":["numa-balancing"]}}`),
			"numa-balancing needs mode bind or preferred-many",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := draTestClaimWithConfigs("c", draTestMemoryPolicy(own, "interleave", ""), tc.config)
			_, err := p.AllocateClaim(claim, []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "2")})
			if err == nil || !strings.Contains(err.Error(), "claim c: config 1:") || !strings.Contains(err.Error(), tc.errMsg) {
				t.Fatalf("got error %v, expected one for config 1 with %q", err, tc.errMsg)
			}
			if len(p.allocations.claims) != 0 || !freeCPUs(p).Equals(free) {
				t.Errorf("refused claim allocated CPUs")
			}
		})
	}
}

// Test that a memory policy bound to a node without memory is refused.
func TestClaimMemoryPolicyMemorylessNode(t *testing.T) {
	// Take the memory away from node 3 of a private copy of the "server"
	// test sysfs.
	dir := t.TempDir()
	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		t.Fatalf("failed to uncompress test sysfs data: %v", err)
	}
	root := path.Join(dir, "sysfs", "server", "sys")
	meminfo := path.Join(root, "devices", "system", "node", "node3", "meminfo")
	if err := os.WriteFile(meminfo, []byte("Node 3 MemTotal: 0 kB\nNode 3 MemFree: 0 kB\n"), 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", meminfo, err)
	}
	sys, err := system.DiscoverSystemAt(root)
	if err != nil {
		t.Fatalf("failed to discover system: %v", err)
	}
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))
	own := resourceapi.AllocationConfigSourceClaim
	results := []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "2")}

	claim := draTestClaimWithConfigs("a", draTestMemoryPolicy(own, "bind", "3"))
	if _, err := p.AllocateClaim(claim, results); err == nil || !strings.Contains(err.Error(), "has no memory node of this host") {
		t.Errorf("got error %v for a memoryless node, expected a refusal", err)
	}
	claim = draTestClaimWithConfigs("b", draTestMemoryPolicy(own, "bind", "2-3"))
	if _, err := p.AllocateClaim(claim, results); err != nil {
		t.Errorf("failed to allocate claim with a memory node: %v", err)
	}
}

// Test that preparing a claim again gives the same memory policies, and that
// the claim's configs cannot change after it is allocated.
func TestClaimMemoryPolicyAgain(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))
	results := []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "2")}
	own := resourceapi.AllocationConfigSourceClaim

	claim := draTestClaimWithConfigs("a", draTestMemoryPolicy(own, "interleave", ""))
	for range 2 {
		if _, err := p.AllocateClaim(claim, results); err != nil {
			t.Fatalf("failed to allocate claim: %v", err)
		}
	}

	for _, changed := range []*resourceapi.ResourceClaim{
		draTestClaimWithConfigs("a", draTestMemoryPolicy(own, "bind", "")),
		draTestClaim("a"),
	} {
		_, err := p.AllocateClaim(changed, results)
		if err == nil || !strings.Contains(err.Error(), "but its configs give") {
			t.Errorf("got error %v for changed configs, expected a refusal", err)
		}
	}
}

// Test that the memory policies of a claim are saved and restored with it.
func TestClaimMemoryPoliciesSurviveRestore(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	claim := draTestClaimWithConfigs("a",
		draTestMemoryPolicy(resourceapi.AllocationConfigSourceClaim, "interleave", "cpu-nodes", "b"))
	if _, err := p.AllocateClaim(claim, []resourceapi.DeviceRequestAllocationResult{
		draTestRequest("a", 0, "1"), draTestRequest("b", 1, "1"),
	}); err != nil {
		t.Fatalf("failed to allocate claim: %v", err)
	}
	allocateClaim(t, p, "none", draTestResult(0, "1"))

	data, err := p.allocations.MarshalJSON()
	if err != nil {
		t.Fatalf("failed to save allocations: %v", err)
	}
	restored := p.newAllocations()
	if err := restored.UnmarshalJSON(data); err != nil {
		t.Fatalf("failed to restore allocations: %v", err)
	}

	for _, uid := range []string{"a", "none"} {
		if want, got := p.allocations.claims[uid].mpols, restored.claims[uid].mpols; !reflect.DeepEqual(got, want) {
			t.Errorf("claim %s: restored memory policies %v, expected %v", uid, got, want)
		}
	}
}

// Test that a reconfiguration keeps the memory policies of the claims.
func TestReconfigureKeepsClaimMemoryPolicies(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))

	claim := draTestClaimWithConfigs("a",
		draTestMemoryPolicy(resourceapi.AllocationConfigSourceClaim, "interleave", "cpu-nodes"))
	results := []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "1")}
	if _, err := p.AllocateClaim(claim, results); err != nil {
		t.Fatalf("failed to allocate claim: %v", err)
	}
	mpols := p.allocations.claims["a"].mpols

	cfg := draTestConfig("", draTestReserved(sys))
	cfg.ColocatePods = true
	if err := p.Reconfigure(cfg); err != nil {
		t.Fatalf("failed to reconfigure policy: %v", err)
	}
	if got := p.allocations.claims["a"].mpols; !reflect.DeepEqual(got, mpols) {
		t.Errorf("claim a has memory policies %v after reconfiguring, expected %v", got, mpols)
	}
	if _, err := p.AllocateClaim(claim, results); err != nil {
		t.Errorf("failed to prepare claim a again: %v", err)
	}
}

// Test that a new container gets the memory policy of its claims, with the
// nodes resolved for its CPUs and memory.
func TestApplyClaimMemoryPolicy(t *testing.T) {
	own := resourceapi.AllocationConfigSourceClaim
	sys := testServerSystem(t)
	results := []resourceapi.DeviceRequestAllocationResult{
		draTestRequest("a", 0, "2"), draTestRequest("b", 1, "2"),
	}

	cases := []struct {
		name      string
		noPinCPU  bool
		pinMemory bool
		configs   []resourceapi.DeviceAllocationConfiguration
		holds     []int
		state     cache.ContainerState
		// cpus and mems are the cpuset of the container. They are preserved
		// unless CPU pinning is off.
		cpus   string
		mems   string
		expect string
		errMsg string
	}{
		{
			name:    "nodes of the claimed CPUs",
			configs: []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "cpu-nodes")},
			holds:   []int{0, 1},
			expect:  "MPOL_INTERLEAVE:0-1[]",
		},
		{
			name:    "one request",
			configs: []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "cpu-nodes")},
			holds:   []int{1},
			expect:  "MPOL_INTERLEAVE:1[]",
		},
		{
			name: "flags",
			configs: []resourceapi.DeviceAllocationConfiguration{draTestClaimConfig(own,
				`{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig",`+
					`"memoryPolicy":{"mode":"bind","nodes":"2-3","flags":["static-nodes"]}}`)},
			holds:  []int{0},
			expect: "MPOL_BIND:2-3[MPOL_F_STATIC_NODES]",
		},
		{
			name:    "local",
			configs: []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "local", "")},
			holds:   []int{0},
			expect:  "MPOL_LOCAL:[]",
		},
		{
			name:    "all memory without pinning",
			configs: []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "allowed-mems")},
			holds:   []int{0},
			expect:  "MPOL_INTERLEAVE:0-5[]",
		},
		{
			name:      "pinned memory",
			pinMemory: true,
			configs:   []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "")},
			holds:     []int{0},
			expect:    "pinned",
		},
		{
			name:    "preserved CPUs",
			configs: []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "cpu-nodes")},
			holds:   []int{0},
			cpus:    sys.Node(2).CPUSet().String(),
			expect:  "MPOL_INTERLEAVE:2[]",
		},
		{
			name:     "unpinned CPUs",
			noPinCPU: true,
			configs:  []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "cpu-nodes")},
			holds:    []int{0},
			cpus:     sys.Node(2).CPUSet().String(),
			expect:   "MPOL_INTERLEAVE:2[]",
		},
		{
			name:      "preserved memory",
			pinMemory: true,
			configs:   []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "")},
			holds:     []int{0},
			mems:      "3",
			expect:    "MPOL_INTERLEAVE:3[]",
		},
		{
			name:    "request without a policy",
			configs: []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "bind", "0", "a")},
			holds:   []int{0, 1},
			expect:  "MPOL_BIND:0[]",
		},
		{
			name: "conflicting requests",
			configs: []resourceapi.DeviceAllocationConfiguration{
				draTestMemoryPolicy(own, "bind", "0", "a"),
				draTestMemoryPolicy(own, "bind", "1", "b"),
			},
			holds:  []int{0, 1},
			errMsg: "conflicts with",
		},
		{
			name:  "no memory policy",
			holds: []int{0, 1},
		},
		{
			name:    "running container",
			configs: []resourceapi.DeviceAllocationConfiguration{draTestMemoryPolicy(own, "interleave", "")},
			holds:   []int{0},
			state:   cache.ContainerStateRunning,
		},
	}

	// Policy options are global, so we create one policy for each pinning
	// setup and run its cases on it.
	for _, pin := range []struct{ cpu, memory bool }{{true, false}, {true, true}, {false, false}} {
		cfg := draTestConfig("", draTestReserved(sys))
		cfg.PinCPU = pin.cpu
		cfg.PinMemory = pin.memory
		p, _ := draTestPolicy(t, sys, cfg)

		for _, tc := range cases {
			if tc.noPinCPU == pin.cpu || tc.pinMemory != pin.memory {
				continue
			}
			t.Run(tc.name, func(t *testing.T) {
				uid := strings.ReplaceAll(tc.name, " ", "-")
				if _, err := p.AllocateClaim(draTestClaimWithConfigs(uid, tc.configs...), results); err != nil {
					t.Fatalf("failed to allocate claim: %v", err)
				}

				state := tc.state
				if state == 0 {
					state = cache.ContainerStateCreating
				}
				c := &pinnedContainer{
					mockContainer: mockContainer{
						name:                   tc.name,
						returnValueForGetID:    tc.name,
						returnValueForQOSClass: v1.PodQOSBestEffort,
					},
					devices:      holds(uid, tc.holds...),
					state:        state,
					cpus:         tc.cpus,
					mems:         tc.mems,
					preserveCPUs: tc.cpus != "" && !tc.noPinCPU,
					preserveMems: tc.mems != "",
				}
				grant, err := p.allocatePool(c, "")
				if tc.errMsg != "" {
					if err == nil || !strings.Contains(err.Error(), tc.errMsg) {
						t.Fatalf("got error %v, expected %q", err, tc.errMsg)
					}
					return
				}
				if err != nil {
					t.Fatalf("failed to allocate: %v", err)
				}
				p.applyGrant(grant)

				expect := tc.expect
				if expect == "pinned" {
					expect = "MPOL_INTERLEAVE:" + grant.GetMemoryZone().MemsetString() + "[]"
				}
				if c.mpol != expect {
					t.Errorf("got memory policy %q, expected %q", c.mpol, expect)
				}
			})
		}
	}
}

// Test that a container holding claims with different memory policies is
// refused.
func TestConflictingClaimMemoryPolicies(t *testing.T) {
	sys := testServerSystem(t)
	p, _ := draTestPolicy(t, sys, draTestConfig("", draTestReserved(sys)))
	own := resourceapi.AllocationConfigSourceClaim
	results := []resourceapi.DeviceRequestAllocationResult{draTestResult(0, "1")}

	for uid, mpol := range map[string]string{
		"a": `{"mode":"interleave"}`,
		"b": `{"mode":"bind"}`,
		"c": `{"mode":"interleave","nodes":"allowed-mems"}`,
		"d": `{"mode":"bind","flags":["static-nodes","numa-balancing"]}`,
		"e": `{"mode":"bind","flags":["numa-balancing","static-nodes","numa-balancing"]}`,
		"f": `{"mode":"bind","nodes":"0-1"}`,
		"g": `{"mode":"bind","nodes":"1,0"}`,
	} {
		config := draTestClaimConfig(own,
			`{"apiVersion":"config.nri/v1alpha1","kind":"TopologyAwareClaimConfig","memoryPolicy":`+mpol+`}`)
		if _, err := p.AllocateClaim(draTestClaimWithConfigs(uid, config), results); err != nil {
			t.Fatalf("failed to allocate claim %s: %v", uid, err)
		}
	}

	for _, tc := range []struct {
		name    string
		devices []string
		errMsg  string
	}{
		{"same policies in different words", append(holds("a", 0), holds("c", 0)...), ""},
		{"same flags in a different order", append(holds("d", 0), holds("e", 0)...), ""},
		{"same nodes in a different form", append(holds("f", 0), holds("g", 0)...), ""},
		{"different policies", append(holds("a", 0), holds("b", 0)...), "conflicts with"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &pinnedContainer{
				mockContainer: mockContainer{
					name:                   tc.name,
					returnValueForGetID:    tc.name,
					returnValueForQOSClass: v1.PodQOSBestEffort,
				},
				devices: tc.devices,
				state:   cache.ContainerStateCreating,
			}
			_, err := p.allocatePool(c, "")
			if tc.errMsg == "" && err != nil {
				t.Errorf("failed to allocate: %v", err)
			}
			if tc.errMsg != "" && (err == nil || !strings.Contains(err.Error(), tc.errMsg)) {
				t.Errorf("got error %v, expected %q", err, tc.errMsg)
			}
		})
	}
}
