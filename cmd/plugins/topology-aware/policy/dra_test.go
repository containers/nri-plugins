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
	"os"
	"path"
	"reflect"
	"testing"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/testutils"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

// testOwner keeps the DRA devices published last, and counts the publishes.
type testOwner struct {
	devices   []resourceapi.Device
	publishes int
}

func (o *testOwner) PublishDRADevices(devices []resourceapi.Device) error {
	o.devices = devices
	o.publishes++
	return nil
}

// draTestSystem discovers the "server" test sysfs: two packages, four NUMA
// nodes of 28 CPUs each with SMT on, two nodes with memory but no CPUs, and
// CPUs 4-7,60-63 kernel-isolated, two of them in each of the four nodes.
func draTestSystem(t *testing.T) system.System {
	t.Helper()

	dir, err := os.MkdirTemp("", "nri-topology-aware-dra-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { removeAll(t, dir) })

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		t.Fatalf("failed to uncompress test sysfs data: %v", err)
	}

	sys, err := system.DiscoverSystemAt(path.Join(dir, "sysfs", "server", "sys"))
	if err != nil {
		t.Fatalf("failed to discover system: %v", err)
	}

	return sys
}

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

// draTestPolicy returns a started policy on the given system, and the owner it
// published its devices to.
func draTestPolicy(t *testing.T, sys system.System, cfg *cfgapi.Config) (*policy, *testOwner) {
	t.Helper()

	owner := &testOwner{}
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

// draDevice is what a test expects of one published device.
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
	sys := draTestSystem(t)
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
			// CPU 4 is kernel-isolated and belongs to node 0. A sole
			// reserved CPU is allowed to also be isolated, which would
			// otherwise let it show up in both IsolatedCPUs() and
			// ReservedCPUs() of the same supply.
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

// TestDRADeviceShape checks the whole of one published device. Its names and
// fields are what a claim selects and consumes it by, so none of them may
// change by accident.
func TestDRADeviceShape(t *testing.T) {
	_, owner := draTestPolicy(t, draTestSystem(t), draTestConfig("cpuset:0,8,12", "cpuset:0"))

	one := resource.MustParse("1")
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
						Default: &one,
						ValidRange: &resourceapi.CapacityRequestPolicyRange{
							Min:  &one,
							Step: &one,
						},
					},
				},
			},
			NodeAllocatableResources: map[v1.ResourceName]resourceapi.NodeAllocatableResource{
				v1.ResourceCPU: {
					Mapping: &resourceapi.NodeAllocatableMapping{
						CapacityKey:        ptr.To(resourceapi.QualifiedName("dra.cpu/cpu")),
						CapacityMultiplier: &one,
					},
				},
			},
		},
	}

	if !apiequality.Semantic.DeepEqual(owner.devices, expected) {
		t.Errorf("published devices\n%+v\nexpected\n%+v", owner.devices, expected)
	}
}

// TestDRAPublish checks that we publish our devices when we start, and again
// only when a reconfiguration changes them.
func TestDRAPublish(t *testing.T) {
	sys := draTestSystem(t)
	p, owner := draTestPolicy(t, sys, draTestConfig("", "cpuset:0,1"))

	if owner.publishes != 1 {
		t.Fatalf("published %d times during start, expected once", owner.publishes)
	}

	// A change which leaves our devices alone must not republish them.
	cfg := draTestConfig("", "cpuset:0,1")
	cfg.ColocatePods = true
	if err := p.Reconfigure(cfg); err != nil {
		t.Fatalf("failed to reconfigure policy: %v", err)
	}
	if owner.publishes != 1 {
		t.Errorf("published %d times, expected no republish of unchanged devices",
			owner.publishes)
	}

	// Reserving one more CPU takes it out of the device it belongs to.
	if err := p.Reconfigure(draTestConfig("", "cpuset:0,1,2")); err != nil {
		t.Fatalf("failed to reconfigure policy: %v", err)
	}
	if owner.publishes != 2 {
		t.Errorf("published %d times, expected a republish of changed devices",
			owner.publishes)
	}

	summary := draDeviceSummary(owner.devices)
	if got := summary["cpudevnuma002"].cpus; got != 27 {
		t.Errorf("node 2 publishes %d CPUs, expected 27", got)
	}
}
