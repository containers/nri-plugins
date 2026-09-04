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
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// test helpers: a minimal real *dra.Plugin seeded with a live
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

// fakeDRACDIWriter is a dra.CDIWriter that tracks per-UID "written" state
// in memory instead of touching disk. Stateful (rather than a fixed
// false/nil) so that ClaimSpecExists/ListClaims accurately reflect prior
// WriteClaim/RemoveClaim calls: dra.Plugin.Start()'s orphan-claim sweep
// calls ClaimSpecExists for every persisted claim and drops any claim it
// reports as missing.
type fakeDRACDIWriter struct {
	written map[types.UID]bool
}

func (w *fakeDRACDIWriter) WriteClaim(uid types.UID, _ []dra.CDIDevice) error {
	if w.written == nil {
		w.written = map[types.UID]bool{}
	}
	w.written[uid] = true
	return nil
}
func (w *fakeDRACDIWriter) RemoveClaim(uid types.UID) error {
	delete(w.written, uid)
	return nil
}
func (w *fakeDRACDIWriter) ClaimSpecExists(uid types.UID) bool { return w.written[uid] }
func (w *fakeDRACDIWriter) ListClaims() ([]types.UID, error) {
	uids := make([]types.UID, 0, len(w.written))
	for uid := range w.written {
		uids = append(uids, uid)
	}
	return uids, nil
}

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
	return newTestDRAPluginWithLock(t, pick, deviceName, func(f func()) { f() })
}

// newTestDRAPluginWithLock is newTestDRAPlugin with an injectable WithLock,
// letting lock-contract tests share a single non-reentrant stub between the
// DRA plugin's deps.WithLock and the policy's options.WithLock (both are
// backed by the same resmgr write lock in production).
func newTestDRAPluginWithLock(t *testing.T, pick cpuset.CPUSet, deviceName string, withLock func(func())) *dra.Plugin {
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
		RegistrarDir:    t.TempDir(),
		PluginDataDir:   t.TempDir(),
		ValidateClasses: func() error { return nil },
		DeviceLister:    &fakeDRADeviceLister{devices: []resourceapi.Device{device}},
		ClaimAllocator:  &fakeDRAClaimAllocator{pick: pick},
		CDIWriter:       &fakeDRACDIWriter{},
		ClaimStore:      &fakeDRAClaimStore{},
		WithLock:        withLock,
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

// TestAllocateResourcesRollsBackClaimOnPoolAllocationFailure verifies that
// AllocateResources rolls back a successful claim-ref mark if the subsequent
// normal pool allocation fails, instead of leaking a claimContainerRefs
// entry (and the corresponding pool supply mark) for a container that never
// actually got its resources allocated.
func TestAllocateResourcesRollsBackClaimOnPoolAllocationFailure(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 1 {
		t.Fatalf("expected at least 1 sharable CPU on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0])

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-rollback-1")
	cdiName := seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())

	p.draPlugin = plugin

	// An absurdly large exclusive CPU request guarantees the subsequent
	// normal pool allocation fails, regardless of topology.
	container := &mockContainer{
		returnValueForGetID: "c1",
		cdiDeviceNames:      []string{cdiName},
		returnValueForGetResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100000")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100000")},
		},
	}

	if err := p.AllocateResources(container); err == nil {
		t.Fatal("AllocateResources() expected error from an unsatisfiable resource request, got nil")
	}

	if _, exists := p.claimContainerRefs[uid]; exists {
		t.Errorf("claimContainerRefs[%s] still present after AllocateResources failed; claim mark was not rolled back", uid)
	}
	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(claimed).Size() != claimed.Size() {
		t.Errorf("claimed CPUs %s not restored to pool supply after AllocateResources rollback: sharable=%s", claimed, got)
	}
}

// TestApplyGrantUnionsClaimedCPUsIntoContainerCpuset is the regression test
// for DRA-claimed CPUs missing from a consumer's cpuset: allocateClaim only
// marks pool supply (and, separately, physically paints the claim's
// cpuClass) — it has no way to touch the consuming container's own cgroup
// cpuset. applyGrant is what actually
// calls container.SetCpusetCpus, but until this fix it only ever pinned the
// *normal* grant's own CPUs (Y), silently excluding whatever CPUs (X) the
// container also holds via a live DRA claim — even though the CDI-injected
// NRI_CPU<N> env vars tell the container it has X. This asserts the
// container's actual pinned cpuset is the union of X and Y, and that X was
// not counted towards Y's own (independently-computed) sizing.
func TestApplyGrantUnionsClaimedCPUsIntoContainerCpuset(t *testing.T) {
	p := newDRATestPolicy(t)
	// applyGrant only calls container.SetCpusetCpus at all when PinCPU is
	// enabled; opt and p.cfg alias the same *cfgapi.Config after Setup().
	p.cfg.PinCPU = true

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 1 {
		t.Fatalf("expected at least 1 sharable CPU on %q", leaf.Name())
	}
	claimedCPU := sharable[0]
	claimed := cpuset.New(claimedCPU)

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-cpuset-union-1")
	cdiName := seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	p.draPlugin = plugin

	container := &mockContainer{
		returnValueForGetID: "c1",
		cdiDeviceNames:      []string{cdiName},
		returnValueForGetResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		},
	}

	if err := p.AllocateResources(container); err != nil {
		t.Fatalf("AllocateResources() unexpected error: %v", err)
	}

	gotCpus, err := cpuset.Parse(container.GetCpusetCpus())
	if err != nil {
		t.Fatalf("failed to parse container cpuset %q: %v", container.GetCpusetCpus(), err)
	}
	if !gotCpus.Contains(claimedCPU) {
		t.Errorf("container cpuset %s does not include claimed CPU %d (X) — only the normal grant's own CPUs (Y) were pinned", gotCpus, claimedCPU)
	}
	// The normal 1-CPU exclusive request (Y) must have been sized on its
	// own, independent of the claimed CPU: exactly one CPU besides the
	// claimed one, not zero (which would mean Y's sizing was inflated by
	// counting the already-claimed CPU as if it were Y's own).
	exclusiveOnly := gotCpus.Difference(claimed)
	if exclusiveOnly.Size() != 1 {
		t.Errorf("container cpuset %s minus claimed CPU %s = %s, want exactly 1 CPU for the normal grant (Y), got double-counting or a missing grant",
			gotCpus, claimed, exclusiveOnly)
	}
}

// TestAllocateResourcesRollbackClearsClaimedCPUsByContainer verifies that
// rollbackClaimMarks also clears claimedCPUsByContainer's entry for a
// container whose AllocateResources call failed partway through, not just
// release the claims in pool/refcount terms -- otherwise a rolled-back call
// could leave a stale cpuset union in place for a container that no longer
// holds the claim.
func TestAllocateResourcesRollbackClearsClaimedCPUsByContainer(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 1 {
		t.Fatalf("expected at least 1 sharable CPU on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0])

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-rollback-clear-1")
	cdiName := seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	p.draPlugin = plugin

	container := &mockContainer{
		returnValueForGetID: "rollback-clear-c1",
		cdiDeviceNames:      []string{cdiName},
		returnValueForGetResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100000")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100000")},
		},
	}

	if err := p.AllocateResources(container); err == nil {
		t.Fatal("AllocateResources() expected error from an unsatisfiable resource request, got nil")
	}

	if _, exists := p.claimedCPUsByContainer[container.GetID()]; exists {
		t.Errorf("claimedCPUsByContainer[%s] still present after AllocateResources rollback", container.GetID())
	}
}

// TestApplyGrantEmptyCpusUsesClaimedCPUsInsteadOfBlanking covers applyGrant's
// cpus.Size() == 0 exit (which otherwise calls container.SetCpusetCpus("")):
// a claim consumer whose *normal* grant happens to compute an empty cpuset
// must not lose access to its claimed CPUs entirely. Uses a
// cpuReserved grant on a leaf pool with no reserved CPUs of its own to
// exercise an empty `cpus` without needing to fully drain a pool's sharable
// capacity.
func TestApplyGrantEmptyCpusUsesClaimedCPUsInsteadOfBlanking(t *testing.T) {
	p := newDRATestPolicy(t)
	p.cfg.PinCPU = true

	leaf := findPoolNode(t, p, "NUMA node #1")
	if !leaf.GetSupply().ReservedCPUs().IsEmpty() {
		t.Fatalf("test setup error: %q has reserved CPUs, want none", leaf.Name())
	}

	container := &mockContainer{returnValueForGetID: "empty-grant-claim-1"}
	claimed := cpuset.New(leaf.FreeSupply().SharableCPUs().List()[0])
	p.claimedCPUsByContainer = map[string]cpuset.CPUSet{
		container.GetID(): claimed,
	}

	g := newGrant(leaf, container, cpuReserved, "", cpuset.New(), 0, memoryDRAM, nil, 0)
	p.applyGrant(g)

	gotCpus, err := cpuset.Parse(container.GetCpusetCpus())
	if err != nil {
		t.Fatalf("failed to parse container cpuset %q: %v", container.GetCpusetCpus(), err)
	}
	if !gotCpus.Equals(claimed) {
		t.Errorf("container cpuset = %s after applyGrant with an empty (reserved) grant, want claimed CPUs %s (must not be blanked to \"\")", gotCpus, claimed)
	}
}

// TestUpdateSharedAllocationsPreservesClaimedCPUsOnRepin covers the
// updateSharedAllocations re-pin path triggered from allocateClaim:
// container A already holds a live claim (claimACPU) plus a plain
// shared-portion grant (no exclusive CPUs). When an unrelated claim for
// container B is allocated, allocateClaim's own updateSharedAllocations(nil)
// call re-pins every other grant -- including A's -- to the pool's now-
// smaller sharable set. That re-pin must still include A's own claimed CPU,
// not just the (shrunk) shared set.
func TestUpdateSharedAllocationsPreservesClaimedCPUsOnRepin(t *testing.T) {
	p := newDRATestPolicy(t)
	p.cfg.PinCPU = true

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}

	containerA := &mockContainer{returnValueForGetID: "shared-alloc-a"}
	claimACPU := sharable[0]
	p.claimedCPUsByContainer = map[string]cpuset.CPUSet{
		containerA.GetID(): cpuset.New(claimACPU),
	}
	grantA := newGrant(leaf, containerA, cpuNormal, "", cpuset.New(), 100, memoryDRAM, nil, 0)
	p.allocations.addGrant(grantA)

	claimBCPU := sharable[1]
	uidB := types.UID("claim-shared-repin-b")
	if err := p.allocateClaim(uidB, cpuset.New(claimBCPU), goldClassCPUs(cpuset.New(claimBCPU))); err != nil {
		t.Fatalf("allocateClaim() failed: %v", err)
	}

	gotCpus, err := cpuset.Parse(containerA.GetCpusetCpus())
	if err != nil {
		t.Fatalf("failed to parse container A's cpuset %q: %v", containerA.GetCpusetCpus(), err)
	}
	if !gotCpus.Contains(claimACPU) {
		t.Errorf("container A's cpuset %s lost its own claimed CPU %d after an unrelated claim's updateSharedAllocations re-pin", gotCpus, claimACPU)
	}
}

// TestReapplyDRAClaimsRepinsContainerCpusetWithGrant is the restart-window
// regression test for reapplyDRAClaims's cpuset re-pin: a container
// restored with a regular grant (grantCPU, e.g. by restoreCache() before
// reapplyDRAClaims is ever reached) that also carries a live DRA claim
// (claimedCPU) must end up with both in its cpuset once reapplyDRAClaims
// runs -- not just wait for the next NRI resync's Release+AllocateResources
// to add the claimed CPU in.
func TestReapplyDRAClaimsRepinsContainerCpusetWithGrant(t *testing.T) {
	p := newDRATestPolicy(t)
	p.cfg.PinCPU = true

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	claimedCPU := sharable[0]
	grantCPU := sharable[1]
	claimed := cpuset.New(claimedCPU)

	container := &mockContainer{returnValueForGetID: "reapply-repin-1"}
	addTestGrant(t, p, leaf, container, cpuset.New(grantCPU))

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-reapply-repin-1")
	cdiName := seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	container.cdiDeviceNames = []string{cdiName}
	p.draPlugin = plugin

	mc, ok := p.cache.(*mockCache)
	if !ok {
		t.Fatalf("test setup error: p.cache is not *mockCache")
	}
	mc.containers = []cache.Container{container}

	p.reapplyDRAClaims()

	gotCpus, err := cpuset.Parse(container.GetCpusetCpus())
	if err != nil {
		t.Fatalf("failed to parse container cpuset %q: %v", container.GetCpusetCpus(), err)
	}
	want := cpuset.New(claimedCPU, grantCPU)
	if !gotCpus.Equals(want) {
		t.Errorf("container cpuset after reapplyDRAClaims() = %s, want %s (claimed + grant, re-pinned without waiting for the next NRI resync)", gotCpus, want)
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

// reapplyDRAClaims()/remarkClaimInSupply() and their
// Start()/Reconfigure() wiring. ----

// TestStartMarksLiveDRAClaimsInPoolSupply verifies that Start() re-marks
// pool supplies for every claim the (already-loaded) DRA plugin reports as
// live, so a subsequent regular allocation cannot pick those CPUs. This
// stands in for a full ClaimStore-backed reload: p.draPlugin is seeded with
// a live claim via PrepareResourceClaims before Start() runs.
func TestStartMarksLiveDRAClaimsInPoolSupply(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0], sharable[1])

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-start-1")
	_ = seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	p.draPlugin = plugin

	if err := p.Start(); err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}

	free := leaf.FreeSupply()
	if got := free.SharableCPUs().Union(free.IsolatedCPUs()).Intersection(claimed); got.Size() != 0 {
		t.Errorf("claimed CPUs %s still free in %q supply after Start(): %s", claimed, leaf.Name(), got)
	}
}

// TestStartReappliesDRAClaimsAfterRestoreCache verifies the ordering that
// Start() must observe: restoreCache() alone (which only restores
// previously-cached container allocations, and knows nothing about DRA
// claims) must not mark any claimed CPUs; only the rest of Start() (which
// calls reapplyDRAClaims() after restoreCache() returns) does.
func TestStartReappliesDRAClaimsAfterRestoreCache(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0], sharable[1])

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-start-order-1")
	_ = seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	p.draPlugin = plugin

	if err := p.restoreCache(); err != nil {
		t.Fatalf("restoreCache() unexpected error: %v", err)
	}
	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(claimed).Size() != claimed.Size() {
		t.Fatalf("test setup error: claimed CPUs %s unexpectedly marked by restoreCache() alone (want unmarked at this point): sharable=%s",
			claimed, got)
	}

	if err := p.Start(); err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(claimed).Size() != 0 {
		t.Errorf("claimed CPUs %s not marked once Start() completed: sharable=%s", claimed, got)
	}
}

// lockContractStub is a WithLock stand-in that panics if invoked while
// already "held", i.e. re-entrantly. Mirrors pkg/resmgr/policy's
// lockContractStub: used here to assert that Start()'s draPlugin.Start(),
// draPlugin.PublishResources(), and reapplyDRAClaims() calls each acquire
// the (shared, non-reentrant) resmgr write lock in strict sequence, never
// nested — the exact bug class of the reapplyDRAClaims/LiveClaimsLocked
// unsynchronized-access race this test guards against.
type lockContractStub struct {
	mu   sync.Mutex
	held bool
}

func (s *lockContractStub) run(f func()) {
	s.mu.Lock()
	if s.held {
		s.mu.Unlock()
		panic("WithLock invoked re-entrantly")
	}
	s.held = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.held = false
		s.mu.Unlock()
	}()

	f()
}

// TestStartReapplyDRAClaimsHoldsWriteLockNotReentrant verifies that Start()
// runs reapplyDRAClaims() (and therefore LiveClaimsLocked(), which reads
// p.claims with no internal synchronization) under the same non-reentrant
// WithLock the DRA plugin's own Start()/PublishResources() use — and never
// nests a second acquisition inside an already-held one, which would
// deadlock. A single lockContractStub is shared between the policy's
// options.WithLock and the DRA plugin's deps.WithLock, exactly as production
// wiring shares one resmgr write lock (m.withWriteLock) between both.
func TestStartReapplyDRAClaimsHoldsWriteLockNotReentrant(t *testing.T) {
	stub := &lockContractStub{}
	p := newDRATestPolicyWithLock(t, stub.run)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0], sharable[1])

	plugin := newTestDRAPluginWithLock(t, claimed, "dev0", stub.run)
	uid := types.UID("claim-lock-contract-1")
	_ = seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	p.draPlugin = plugin

	var startErr error
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		startErr = p.Start()
	}()

	if panicked {
		t.Fatalf("Start() panicked: WithLock was invoked re-entrantly")
	}
	if startErr != nil {
		t.Fatalf("Start() unexpected error: %v", startErr)
	}
	if stub.held {
		t.Errorf("resmgr write lock still held after Start() returned")
	}

	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(claimed).Size() != 0 {
		t.Errorf("claimed CPUs %s not marked once Start() completed: sharable=%s", claimed, got)
	}
}

// TestReconfigureReappliesLiveDRAClaims verifies that Reconfigure() re-marks
// pool supplies for live DRA claims after restoreAllocations() rebuilds the
// pool tree from scratch (initialize() resets p.nodes/p.pools/p.root and
// wipes any earlier Supply.claimRefs marks), and that doing so does not
// disturb the (empty, in this test) set of regular container grants.
func TestReconfigureReappliesLiveDRAClaims(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0], sharable[1])

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-reconf-1")
	_ = seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	p.draPlugin = plugin

	newCfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{
			cfgapi.CPU: "750m",
		},
	}

	if err := p.Reconfigure(newCfg); err != nil {
		t.Fatalf("Reconfigure() unexpected error: %v", err)
	}

	// initialize() rebuilt the pool tree: re-fetch by name rather than
	// reusing the pre-Reconfigure leaf Node.
	leaf = findPoolNode(t, p, "NUMA node #0")
	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(claimed).Size() != 0 {
		t.Errorf("claimed CPUs %s not re-marked in %q sharable set after Reconfigure(): %s",
			claimed, leaf.Name(), got)
	}
	if len(p.allocations.grants) != 0 {
		t.Errorf("Reconfigure()'s DRA re-apply unexpectedly created/left grants: %v", p.allocations.grants)
	}
}

// TestReconfigureRejectsConflictingDRACPUClasses verifies that Reconfigure()
// validates the new config's DRA-published cpuClasses (via
// cpuclass.ValidateCPUClassesForDRA) *before* committing it to p.cfg. Without
// this, a tier-conflicting reconfigure would only fail later inside
// PublishResources (called from PostReconfigure, after the caller's lock is
// released) — by which point the bad config would already be live.
func TestReconfigureRejectsConflictingDRACPUClasses(t *testing.T) {
	p := newDRATestPolicy(t)
	p.cfg.DRA = &cfgapi.TopologyAwareDRA{Enabled: true}

	newCfg := &cfgapi.Config{
		DRA: &cfgapi.TopologyAwareDRA{Enabled: true},
		CPUClasses: []*cfgapi.CPUClass{
			{Name: "hp-a", PctPriority: "high"},
			{Name: "hp-b", PctPriority: "high"},
		},
		ReservedResources: cfgapi.Constraints{
			cfgapi.CPU: "750m",
		},
	}

	err := p.Reconfigure(newCfg)
	if err == nil {
		t.Fatal("Reconfigure() with conflicting DRA-published cpuClasses returned nil error, want error")
	}
	if !strings.Contains(err.Error(), "hp-a") {
		t.Errorf("Reconfigure() error = %q, want it to name the conflicting classes", err.Error())
	}

	// The rejected config must not have been committed.
	if len(p.cfg.CPUClasses) != 0 {
		t.Errorf("p.cfg.CPUClasses = %v after rejected Reconfigure(), want unchanged (empty)", p.cfg.CPUClasses)
	}
}

// TestClaimContainerRefsRebuiltAfterStartResync verifies the mechanism
// documented on remarkClaimInSupply/reapplyDRAClaims for the restart case:
// p.claimContainerRefs is a plain in-memory map, so it is empty right after
// Start(), even though a container backed by a live DRA claim is already
// running. It is only rebuilt once pkg/resmgr/nri.go's syncWithNRI/
// Synchronize forces that already-running container through
// ReleaseResources (a no-op, since the refcount is already 0) followed by
// AllocateResources (which increments it) — reproduced here directly via
// p.Sync(add, del) with the same container in both lists, exactly as
// syncWithNRI does for every container discovered in ContainerStateRunning/
// ContainerStateCreated.
func TestClaimContainerRefsRebuiltAfterStartResync(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0], sharable[1])

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-restart-resync-1")
	cdiName := seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	p.draPlugin = plugin

	if err := p.Start(); err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}

	// Right after Start(), the pool supply already excludes the claimed
	// CPUs (reapplyDRAClaims), but claimContainerRefs knows nothing about
	// the container yet — it hasn't gone through AllocateResources in this
	// process.
	if got := p.claimContainerRefs[uid]; got != 0 {
		t.Fatalf("test setup error: claimContainerRefs[%s] = %d right after Start(), want 0", uid, got)
	}

	container := &mockContainer{returnValueForGetID: "restart-c1", cdiDeviceNames: []string{cdiName}}

	// Mirror syncWithNRI: an already-running container is placed in both
	// the "allocated" and "released" lists so Sync() releases (no-op) then
	// re-allocates it.
	if err := p.Sync([]cache.Container{container}, []cache.Container{container}); err != nil {
		t.Fatalf("Sync() unexpected error: %v", err)
	}

	if got := p.claimContainerRefs[uid]; got != 1 {
		t.Errorf("claimContainerRefs[%s] = %d after Start()+resync Sync(), want 1", uid, got)
	}
}

// TestReapplyDRAClaimsNilDRAPluginNoop verifies that reapplyDRAClaims() is a
// no-op (no panic, no supply changes) when DRA is disabled (draPlugin ==
// nil, the default).
func TestReapplyDRAClaimsNilDRAPluginNoop(t *testing.T) {
	p := newDRATestPolicy(t)
	if p.draPlugin != nil {
		t.Fatalf("test setup error: draPlugin unexpectedly non-nil")
	}

	leaf := findPoolNode(t, p, "NUMA node #0")
	before := leaf.FreeSupply().SharableCPUs()

	p.reapplyDRAClaims()

	if got := leaf.FreeSupply().SharableCPUs(); !got.Equals(before) {
		t.Errorf("reapplyDRAClaims() with nil draPlugin changed pool supply: got %s, want unchanged %s", got, before)
	}
}

// TestReapplyDRAClaimsEvictsOverlappingRestoredGrant covers the double-
// booking gap identified in the DRA step 8 review: reapplyDRAClaims() runs
// *after* restoreCache()'s/restoreAllocations()'s grant restoration, at a
// point where Supply.claimRefs has just been wiped by initialize() and not
// yet re-marked. If grant restoration (verbatim reinstatement, or its
// allocatePool-based fallback) handed a regular container CPUs that a live
// DRA claim already owns, that overlap must be detected and evicted here —
// otherwise two workloads end up pinned to the same physical CPUs until the
// next restart. addTestGrant stands in for "restoreCache() already
// reinstated this grant" without needing the full cache/offer machinery.
func TestReapplyDRAClaimsEvictsOverlappingRestoredGrant(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 1 {
		t.Fatalf("expected at least 1 sharable CPU on %q", leaf.Name())
	}
	claimed := cpuset.New(sharable[0])

	// Simulate restoreCache()/restoreAllocations() having already reinstated
	// (or freshly reallocated) a regular grant that happens to overlap the
	// CPU a live DRA claim owns — the exact scenario reapplyDRAClaims must
	// still be able to correct despite running after grant restoration.
	victim := &mockContainer{returnValueForGetID: "reapply-victim"}
	addTestGrant(t, p, leaf, victim, claimed)
	if _, ok := p.allocations.getGrant("reapply-victim"); !ok {
		t.Fatalf("test setup error: victim grant not present before reapplyDRAClaims")
	}

	plugin := newTestDRAPlugin(t, claimed, "dev0")
	uid := types.UID("claim-reapply-evict-1")
	_ = seedLiveClaim(t, plugin, uid, "dev0", claimed.Size())
	p.draPlugin = plugin

	p.reapplyDRAClaims()

	// No grant anywhere may still exclusively hold the claimed CPU: either
	// the victim's original grant was released outright, or it was
	// reallocated to CPUs that no longer overlap the claim.
	for _, g := range p.allocations.grants {
		if g.ExclusiveCPUs().Intersection(claimed).Size() != 0 {
			t.Errorf("claimed CPU %s still exclusively granted to %s after reapplyDRAClaims",
				claimed, g.GetContainer().PrettyName())
		}
	}

	// And the claimed CPU must not be free for a new regular allocation.
	free := leaf.FreeSupply()
	if free.SharableCPUs().Union(free.IsolatedCPUs()).Intersection(claimed).Size() != 0 {
		t.Errorf("claimed CPU %s still free in %q supply after reapplyDRAClaims", claimed, leaf.Name())
	}

	// The evicted victim must actually have been reallocated a new grant
	// (not just released and forgotten) — ample capacity remains on this
	// test system for reallocatePool to succeed.
	newGrant, ok := p.allocations.getGrant("reapply-victim")
	if !ok {
		t.Fatalf("victim has no grant after reapplyDRAClaims(); eviction must reallocate, not just release")
	}
	if newGrant.ExclusiveCPUs().Intersection(claimed).Size() != 0 {
		t.Errorf("victim's new grant %s still overlaps claimed CPU %s", newGrant.ExclusiveCPUs(), claimed)
	}

	// reapplyDRAClaims() must not touch claimContainerRefs (marking-only
	// contract) — even though it evicted/reallocated a container here.
	if _, exists := p.claimContainerRefs[uid]; exists {
		t.Errorf("claimContainerRefs unexpectedly populated by reapplyDRAClaims (marking-only contract violated)")
	}
}
