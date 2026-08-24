// Copyright 2026 Intel Corporation. All Rights Reserved.
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
	"context"
	"fmt"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// ---- Task 7 test helpers: a minimal real *dra.Plugin seeded with a live
// claim via PrepareResourceClaims. Building a real Plugin (rather than
// re-deriving the claimLister interface with a fake) is what lets these
// tests exercise the exact typed-nil-trap-prone p.draPlugin field the
// production AllocateResources/ReleaseResources call sites use. ----

// fakeDRADeviceLister is a dra.DeviceLister that always returns a fixed list
// of devices, regardless of driverName.
type fakeDRADeviceLister struct {
	devices []resourceapi.Device
}

func (f *fakeDRADeviceLister) DRADevices(_ string) ([]resourceapi.Device, error) {
	return f.devices, nil
}

// fakeDRAClaimAllocator is a dra.ClaimAllocator that always "picks" a fixed
// CPUSet and reports every class as HP-eligible. Good enough for
// PrepareResourceClaims to succeed; the pool-accounting side effects this
// package cares about (Supply.ClaimCPUs/UnclaimCPUs) are exercised
// separately via allocateClaim/releaseClaim, not via this allocator.
type fakeDRAClaimAllocator struct {
	pick cpuset.CPUSet
}

func (f *fakeDRAClaimAllocator) PickHpCpus(_, _, _ int, _ cpuset.CPUSet) (cpuset.CPUSet, error) {
	return f.pick, nil
}
func (f *fakeDRAClaimAllocator) ReleaseHpCpus(_, _ int, _ cpuset.CPUSet)       {}
func (f *fakeDRAClaimAllocator) AccountHpCpus(_, _ int, _ cpuset.CPUSet) error { return nil }
func (f *fakeDRAClaimAllocator) IsHPClass(_ string) bool                       { return true }

// fakeDRACDIWriter is a dra.CDIWriter that succeeds without touching disk.
type fakeDRACDIWriter struct{}

func (*fakeDRACDIWriter) WriteClaim(types.UID, []dra.CDIDevice) error { return nil }
func (*fakeDRACDIWriter) RemoveClaim(types.UID) error                 { return nil }
func (*fakeDRACDIWriter) ClaimSpecExists(types.UID) bool              { return false }
func (*fakeDRACDIWriter) ListClaims() ([]types.UID, error)            { return nil, nil }

// fakeDRAClaimStore is a dra.ClaimStore that succeeds without persisting
// anything.
type fakeDRAClaimStore struct{}

func (*fakeDRAClaimStore) Save(map[types.UID]*dra.ClaimState) error     { return nil }
func (*fakeDRAClaimStore) Load() (map[types.UID]*dra.ClaimState, error) { return nil, nil }

// newTestDRAPlugin builds a real *dra.Plugin backed entirely by fakes, ready
// for PrepareResourceClaims calls. pick is the CPUSet the fake allocator
// hands out for every PickHpCpus call.
func newTestDRAPlugin(t *testing.T, pick cpuset.CPUSet, deviceName string) *dra.Plugin {
	t.Helper()

	className := "gold"
	device := resourceapi.Device{
		Name: deviceName,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"nri/cpuClass":  {StringValue: &className},
			"nri/packageID": {IntValue: new(int64)},
			"nri/punitID":   {IntValue: new(int64)},
		},
	}

	deps := dra.Deps{
		KubeClient:      fake.NewClientset(),
		NodeName:        "test-node",
		ValidateClasses: func() error { return nil },
		DeviceLister:    &fakeDRADeviceLister{devices: []resourceapi.Device{device}},
		ClaimAllocator:  &fakeDRAClaimAllocator{pick: pick},
		CDIWriter:       &fakeDRACDIWriter{},
		ClaimStore:      &fakeDRAClaimStore{},
		WithLock:        func(f func()) { f() },
		Logger:          log,
	}

	p, err := dra.New(DRADriverName, deps)
	if err != nil {
		t.Fatalf("dra.New() failed: %v", err)
	}
	return p
}

// seedLiveClaim runs PrepareResourceClaims for a single claim UID against
// plugin, returning the qualified CDI device name the runtime would see on
// the container's CDIDevices (the same string GetCDIDeviceNames() returns).
func seedLiveClaim(t *testing.T, plugin *dra.Plugin, uid types.UID, deviceName string, numCPUs int) string {
	t.Helper()

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Driver:  DRADriverName,
							Pool:    "pool0",
							Device:  deviceName,
							Request: "req0",
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
								"nri/cpus": resource.MustParse(fmt.Sprintf("%d", numCPUs)),
							},
						},
					},
				},
			},
		},
	}

	result, err := plugin.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("PrepareResourceClaims() unexpected error: %v", err)
	}
	r, ok := result[uid]
	if !ok {
		t.Fatalf("PrepareResourceClaims() result missing uid %s", uid)
	}
	if r.Err != nil {
		t.Fatalf("PrepareResourceClaims() PrepareResult.Err = %v, want nil", r.Err)
	}
	if len(r.Devices) != 1 || len(r.Devices[0].CDIDeviceIDs) != 1 {
		t.Fatalf("PrepareResourceClaims() result = %+v, want exactly one device with one CDI device ID", r)
	}

	return r.Devices[0].CDIDeviceIDs[0]
}

// TestAllocateResourcesWithTAClaimCallsAllocateClaim verifies that
// AllocateResources recognizes a container carrying a live TA DRA claim's
// CDI device, marks the claimed CPUs in the pool supply (so a subsequent
// regular allocation cannot pick them), and bumps claimContainerRefs.
func TestAllocateResourcesWithTAClaimCallsAllocateClaim(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0], sharable[1])

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-alloc-1")
	cdiName := seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())

	p.draPlugin = plugin

	container := &mockContainer{returnValueForGetID: "c1", cdiDeviceNames: []string{cdiName}}

	if err := p.AllocateResources(container); err != nil {
		t.Fatalf("AllocateResources() unexpected error: %v", err)
	}

	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(claimed).Size() != 0 {
		t.Errorf("claimed CPUs %s still present in %q sharable set after AllocateResources: %s", claimed, leaf.Name(), got)
	}
	if got := p.claimContainerRefs[uid]; got != 1 {
		t.Errorf("claimContainerRefs[%s] = %d after AllocateResources, want 1", uid, got)
	}
}

// TestReleaseResourcesWithTAClaimCallsReleaseClaim verifies that
// ReleaseResources restores CPUs claimed by allocateClaim once the last
// referencing container is released.
func TestReleaseResourcesWithTAClaimCallsReleaseClaim(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0], sharable[1])

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-release-1")
	cdiName := seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())

	p.draPlugin = plugin

	container := &mockContainer{returnValueForGetID: "c1", cdiDeviceNames: []string{cdiName}}

	if err := p.AllocateResources(container); err != nil {
		t.Fatalf("AllocateResources() unexpected error: %v", err)
	}
	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(claimed).Size() != 0 {
		t.Fatalf("test setup error: claimed CPUs %s not marked before ReleaseResources", claimed)
	}

	if err := p.ReleaseResources(container); err != nil {
		t.Fatalf("ReleaseResources() unexpected error: %v", err)
	}

	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(claimed).Size() != claimed.Size() {
		t.Errorf("claimed CPUs %s not restored after ReleaseResources: sharable=%s", claimed, got)
	}
	if _, exists := p.claimContainerRefs[uid]; exists {
		t.Errorf("claimContainerRefs[%s] still present after ReleaseResources released the last container", uid)
	}
}

// TestAllocateResourcesNoCDIDevicesUnaffected verifies that a container
// without any TA CDI device is unaffected by an active (non-nil) draPlugin:
// no claim bookkeeping happens, and regular allocation behaves exactly as it
// did before Step 8.
func TestAllocateResourcesNoCDIDevicesUnaffected(t *testing.T) {
	p := newDRATestPolicy(t)
	p.draPlugin = newTestDRAPlugin(t, cpuset.New(), "dev0") // no live claims seeded

	container := &mockContainer{returnValueForGetID: "c1"}

	if err := p.AllocateResources(container); err != nil {
		t.Fatalf("AllocateResources() unexpected error: %v", err)
	}
	if len(p.claimContainerRefs) != 0 {
		t.Errorf("claimContainerRefs = %v after AllocateResources on a container with no CDI devices, want empty", p.claimContainerRefs)
	}
}

// TestAllocateResourcesNilDRAPluginNoCrash verifies that AllocateResources
// and ReleaseResources on a policy with DRA disabled (draPlugin == nil, the
// default) neither panic nor attempt any claim lookup.
func TestAllocateResourcesNilDRAPluginNoCrash(t *testing.T) {
	p := newDRATestPolicy(t)
	if p.draPlugin != nil {
		t.Fatalf("test setup error: draPlugin unexpectedly non-nil")
	}

	container := &mockContainer{returnValueForGetID: "c1"}

	if err := p.AllocateResources(container); err != nil {
		t.Fatalf("AllocateResources() with nil draPlugin: unexpected error: %v", err)
	}
	if err := p.ReleaseResources(container); err != nil {
		t.Fatalf("ReleaseResources() with nil draPlugin: unexpected error: %v", err)
	}
}
