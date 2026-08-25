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
	"sync"
	"testing"

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

// fakeDRACDIWriter is a dra.CDIWriter that tracks per-UID "written" state
// in memory instead of touching disk. Stateful (rather than a fixed
// false/nil) so that ClaimSpecExists/ListClaims accurately reflect prior
// WriteClaim/RemoveClaim calls: dra.Plugin.Start()'s orphan-claim sweep
// calls ClaimSpecExists for every persisted claim and drops any claim it
// reports as missing, so a fixed `false` would silently discard every
// claim seeded via seedLiveClaim before Start() runs (Task 9 exercises
// Start() for real via policy.Start()).
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
		KubeClient: fake.NewClientset(),
		NodeName:   "test-node",
		// RegistrarDir/PluginDataDir: real tempdirs, not the kubeletplugin
		// package defaults (/var/lib/kubelet/...) — Task 9 exercises
		// plugin.Start() for real via policy.Start(), which would
		// otherwise try (and likely fail, for lack of permissions) to
		// create real kubelet directories in the test environment.
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

// ---- Task 8 tests: reapplyDRAClaims()/remarkClaimInSupply() and their
// Start()/Reconfigure() wiring. ----

// TestStartMarksLiveDRAClaimsInPoolSupply verifies that Start() re-marks
// pool supplies for every claim the (already-loaded) DRA plugin reports as
// live, so a subsequent regular allocation cannot pick those CPUs. This
// stands in for a full ClaimStore-backed reload: p.draPlugin is seeded with
// a live claim via PrepareResourceClaims before Start() runs, exactly as it
// would be once Task 9's draPlugin.Start(ctx) has loaded persisted claims
// from the ClaimStore before Start() reaches reapplyDRAClaims().
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
//
// This is the closest ordering check obtainable at Task 8: draPlugin.Start(ctx)
// itself is not yet wired into policy.Start() (that lifecycle wiring is
// Task 9's job), so there is no real "draPlugin.Start(ctx) must precede
// reapplyDRAClaims()" call sequence to assert against with a call-order
// mock yet. What can be verified now is that reapplyDRAClaims() runs *after*
// restoreCache() within Start(), which this test does directly.
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
