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
	"os"
	"path"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/testutils"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// newDRATestPolicy builds a real policy (with a real, multi-level node tree:
// root -> socket -> NUMA node) from the "server" sysfs test data. This gives
// us actual Parent()/Children() wiring, which is what ClaimCPUs/UnclaimCPUs
// tree-wide propagation depends on.
func newDRATestPolicy(t *testing.T) *policy {
	t.Helper()
	// WithLock must be non-nil: Start() runs reapplyDRAClaims() under
	// p.options.WithLock whenever p.draPlugin != nil (tests below set
	// p.draPlugin directly), mirroring the real resmgr write lock.
	return newDRATestPolicyWithLock(t, func(f func()) { f() })
}

// newDRATestPolicyWithLock is newDRATestPolicy with an injectable WithLock,
// letting lock-contract tests (see topology-aware-policy_test.go) observe
// exactly when Start()/Reconfigure() hold the resmgr write lock.
func newDRATestPolicyWithLock(t *testing.T, withLock func(func())) *policy {
	t.Helper()

	dir, err := os.MkdirTemp("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { removeAll(t, dir) })

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		t.Fatalf("failed to uncompress test sysfs data: %v", err)
	}

	sys, err := system.DiscoverSystemAt(path.Join(dir, "sysfs", "server", "sys"))
	if err != nil {
		t.Fatalf("failed to discover test system: %v", err)
	}

	policyOptions := &policyapi.BackendOptions{
		Cache:  &mockCache{},
		System: sys,
		Config: &cfgapi.Config{
			ReservedResources: cfgapi.Constraints{
				cfgapi.CPU: "750m",
			},
		},
		WithLock: withLock,
	}

	p := New().(*policy)
	if err := p.Setup(policyOptions); err != nil {
		t.Fatalf("failed to set up test policy: %v", err)
	}

	return p
}

// newDRATestPolicyWithCPUClasses is newDRATestPolicy plus a real
// *cpuclass.Handler (p.cpuClasses), configured with sharedClass (used as
// SharedPoolCpuClass, i.e. the class initialize()'s resetCpuClass and
// releaseClaim's resetCpuClass reapply to CPUs no longer exclusively held)
// plus one or more claimClasses (e.g. distinct classes different
// ResultAllocs within one DRA claim can resolve to). Lets tests observe
// cpuClass.UseClass side effects via Handler.ClassForCPU without needing a
// live SST/PCT backend.
func newDRATestPolicyWithCPUClasses(t *testing.T, sharedClass string, claimClasses ...string) *policy {
	t.Helper()

	dir, err := os.MkdirTemp("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { removeAll(t, dir) })

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		t.Fatalf("failed to uncompress test sysfs data: %v", err)
	}

	sys, err := system.DiscoverSystemAt(path.Join(dir, "sysfs", "server", "sys"))
	if err != nil {
		t.Fatalf("failed to discover test system: %v", err)
	}

	cpuClasses := []*cfgapi.CPUClass{{Name: sharedClass}}
	for _, c := range claimClasses {
		cpuClasses = append(cpuClasses, &cfgapi.CPUClass{Name: c})
	}

	policyOptions := &policyapi.BackendOptions{
		Cache:  &mockCache{},
		System: sys,
		Config: &cfgapi.Config{
			ReservedResources: cfgapi.Constraints{
				cfgapi.CPU: "750m",
			},
			CPUClasses:         cpuClasses,
			SharedPoolCpuClass: sharedClass,
		},
		// See newDRATestPolicy: Start() requires a non-nil WithLock whenever
		// p.draPlugin is set.
		WithLock: func(f func()) { f() },
	}

	p := New().(*policy)
	if err := p.Setup(policyOptions); err != nil {
		t.Fatalf("failed to set up test policy: %v", err)
	}
	if p.cpuClasses == nil {
		t.Fatalf("test setup error: p.cpuClasses is nil despite non-empty CPUClasses config")
	}

	return p
}

// findPoolNode returns the pool Node with the given name, failing the test if
// it isn't found.
func findPoolNode(t *testing.T, p *policy, name string) Node {
	t.Helper()
	for _, n := range p.pools {
		if n.Name() == name {
			return n
		}
	}
	t.Fatalf("no pool node named %q found", name)
	return nil
}

// ancestorsOf returns node's ancestor chain, closest first, by walking
// Parent().
func ancestorsOf(n Node) []Node {
	var ancestors []Node
	for parent := n.Parent(); !parent.IsNil(); parent = parent.Parent() {
		ancestors = append(ancestors, parent)
	}
	return ancestors
}

func TestSupplyClaimCPUsTreeWide(t *testing.T) {
	p := newDRATestPolicy(t)

	// "NUMA node #0" is a leaf under "socket #0", which is a child of "root".
	leaf := findPoolNode(t, p, "NUMA node #0")
	ancestors := ancestorsOf(leaf)
	if len(ancestors) == 0 {
		t.Fatalf("expected %q to have ancestors, got none", leaf.Name())
	}

	// Pick a couple of sharable CPUs known to belong to this leaf's supply.
	claimed := leaf.FreeSupply().SharableCPUs()
	if claimed.Size() < 2 {
		t.Fatalf("expected leaf %q to have at least 2 sharable CPUs, got %s", leaf.Name(), claimed)
	}
	// Take exactly two CPUs from the leaf's sharable set.
	claimedList := claimed.List()
	cpus := cpuset.New(claimedList[0], claimedList[1])

	uid := types.UID("claim-uid-1")

	leafSharableBefore := leaf.FreeSupply().SharableCPUs()
	leafAllocatableBefore := leaf.FreeSupply().AllocatableSharedCPU()
	ancestorSharableBefore := make(map[string]cpuset.CPUSet, len(ancestors))
	for _, a := range ancestors {
		ancestorSharableBefore[a.Name()] = a.FreeSupply().SharableCPUs()
	}

	leaf.FreeSupply().ClaimCPUs(uid, cpus)

	// The leaf's own sharable set must have shrunk by exactly `cpus`.
	if got := leaf.FreeSupply().SharableCPUs(); !got.Equals(leafSharableBefore.Difference(cpus)) {
		t.Errorf("leaf %q sharable CPUs after claim = %s, want %s",
			leaf.Name(), got, leafSharableBefore.Difference(cpus))
	}

	// Every ancestor's sharable set must also have shrunk by `cpus`.
	for _, a := range ancestors {
		want := ancestorSharableBefore[a.Name()].Difference(cpus)
		if got := a.FreeSupply().SharableCPUs(); !got.Equals(want) {
			t.Errorf("ancestor %q sharable CPUs after claim = %s, want %s", a.Name(), got, want)
		}
	}

	// The claimed CPUs must no longer be available for a regular allocation
	// from the leaf: AllocatableSharedCPU (milli-CPU) must have dropped by
	// exactly 2 full CPUs worth (2000m).
	freeAfter := leaf.FreeSupply().AllocatableSharedCPU()
	if want := leafAllocatableBefore - 2000; freeAfter != want {
		t.Errorf("claimed leaf allocatable shared CPU = %dm, want %dm (before claim: %dm)",
			freeAfter, want, leafAllocatableBefore)
	}

	// A regular exclusive-CPU allocation request for another container must
	// not be able to pick the claimed CPUs, no matter how many full CPUs it
	// asks for out of what's still nominally available.
	full := freeAfter / 1000
	if full < 1 {
		t.Fatalf("expected at least 1 full CPU still allocatable on %q after claim, got %dm", leaf.Name(), freeAfter)
	}
	req := &request{
		full:      full,
		container: &mockContainer{},
	}
	offer, err := leaf.FreeSupply().GetCPUOffer(req)
	if err != nil {
		t.Fatalf("GetCPUOffer for %d full CPUs failed unexpectedly: %v", full, err)
	}
	if offer.Intersection(cpus).Size() != 0 {
		t.Errorf("CPU offer %s for another container includes claimed CPUs %s", offer, cpus)
	}
}

// TestSupplyClaimCPUsReservedPartition covers the reserved-CPU case
// poolForCPUs (pools.go) allows for but that ClaimCPUs/UnclaimCPUs used to
// ignore: poolForCPUs matches a pool's static range as
// isolated+reserved+sharable, so a DRA claim can legitimately land on a CPU
// that is part of a pool's reserved partition (the DRA CPU-pick allocator's
// own "allowed" domain, p.allowed, is not required to exclude p.reserved).
// ClaimCPUs must subtract from the reserved cpuset too, or
// AllocatableReservedCPU would keep advertising that CPU's capacity to
// ordinary reserved-type grants even though DRA already owns it exclusively
// (a double-booking gap on the reserved partition).
func TestSupplyClaimCPUsReservedPartition(t *testing.T) {
	p := newDRATestPolicy(t)

	var reservedPool Node
	for _, n := range p.pools {
		if n.GetSupply().ReservedCPUs().Size() > 0 {
			reservedPool = n
			break
		}
	}
	if reservedPool == nil {
		t.Fatalf("test setup error: no pool in the test topology has a non-empty reserved partition")
	}

	reservedSupply, ok := reservedPool.FreeSupply().(*supply)
	if !ok {
		t.Fatalf("pool %q FreeSupply() is not a *supply", reservedPool.Name())
	}

	reserved := reservedSupply.ReservedCPUs()
	cpus := cpuset.New(reserved.List()[0])
	uid := types.UID("claim-uid-reserved")

	reservedAllocatableBefore := reservedSupply.AllocatableReservedCPU()

	reservedSupply.ClaimCPUs(uid, cpus)

	if got := reservedSupply.ReservedCPUs(); got.Intersection(cpus).Size() != 0 {
		t.Errorf("claimed reserved CPU %s still present in %q reserved set after ClaimCPUs: %s",
			cpus, reservedPool.Name(), got)
	}
	// AllocatableReservedCPU has a special sentinel: it returns -1 (not a
	// proportional milliCPU amount) once the reserved cpuset becomes
	// entirely empty, rather than when its granted capacity merely drops to
	// zero. Claiming the last reserved CPU on this pool hits that sentinel;
	// claiming one of several would not.
	wantAfterClaim := reservedAllocatableBefore - 1000
	if reserved.Difference(cpus).IsEmpty() {
		wantAfterClaim = -1
	}
	if got := reservedSupply.AllocatableReservedCPU(); got != wantAfterClaim {
		t.Errorf("%q allocatable reserved CPU after claiming 1 reserved CPU = %dm, want %dm",
			reservedPool.Name(), got, wantAfterClaim)
	}

	reservedSupply.UnclaimCPUs(uid)

	if got := reservedSupply.ReservedCPUs(); !got.Equals(reserved) {
		t.Errorf("%q reserved set after UnclaimCPUs = %s, want restored %s", reservedPool.Name(), got, reserved)
	}
	if got := reservedSupply.AllocatableReservedCPU(); got != reservedAllocatableBefore {
		t.Errorf("%q allocatable reserved CPU after UnclaimCPUs = %dm, want restored %dm",
			reservedPool.Name(), got, reservedAllocatableBefore)
	}
}

func TestSupplyClaimCPUsIdempotentReplace(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 2 {
		t.Fatalf("expected at least 2 sharable CPUs on %q", leaf.Name())
	}

	uid := types.UID("claim-uid-replace")
	cpusA := cpuset.New(sharable[0])
	cpusB := cpuset.New(sharable[1])

	before := leaf.FreeSupply().SharableCPUs()

	leaf.FreeSupply().ClaimCPUs(uid, cpusA)
	leaf.FreeSupply().ClaimCPUs(uid, cpusB)

	// Only cpusB should be subtracted; cpusA must have been restored when the
	// second ClaimCPUs call replaced the mark for the same uid.
	want := before.Difference(cpusB)
	if got := leaf.FreeSupply().SharableCPUs(); !got.Equals(want) {
		t.Errorf("sharable CPUs after replacing claim = %s, want %s (cpusA=%s should be restored, cpusB=%s subtracted)",
			got, want, cpusA, cpusB)
	}

	// And ancestors must reflect the same non-stacked (single) subtraction.
	for _, a := range ancestorsOf(leaf) {
		full := a.FreeSupply().SharableCPUs()
		if full.Intersection(cpusA).Size() != cpusA.Size() {
			t.Errorf("ancestor %q is missing cpusA=%s (claim replace should have restored it there too): sharable=%s",
				a.Name(), cpusA, full)
		}
		if full.Intersection(cpusB).Size() != 0 {
			t.Errorf("ancestor %q still contains cpusB=%s that should be claimed: sharable=%s",
				a.Name(), cpusB, full)
		}
	}
}

func TestSupplyUnclaimCPUsRestores(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	ancestors := ancestorsOf(leaf)
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 1 {
		t.Fatalf("expected at least 1 sharable CPU on %q", leaf.Name())
	}

	uid := types.UID("claim-uid-unclaim")
	cpus := cpuset.New(sharable[0])

	leafBefore := leaf.FreeSupply().SharableCPUs()
	ancestorBefore := make(map[string]cpuset.CPUSet, len(ancestors))
	for _, a := range ancestors {
		ancestorBefore[a.Name()] = a.FreeSupply().SharableCPUs()
	}

	leaf.FreeSupply().ClaimCPUs(uid, cpus)
	leaf.FreeSupply().UnclaimCPUs(uid)

	if got := leaf.FreeSupply().SharableCPUs(); !got.Equals(leafBefore) {
		t.Errorf("leaf %q sharable CPUs after unclaim = %s, want restored %s", leaf.Name(), got, leafBefore)
	}
	for _, a := range ancestors {
		if got := a.FreeSupply().SharableCPUs(); !got.Equals(ancestorBefore[a.Name()]) {
			t.Errorf("ancestor %q sharable CPUs after unclaim = %s, want restored %s",
				a.Name(), got, ancestorBefore[a.Name()])
		}
	}
}

func TestSupplyUnclaimCPUsUnknownUIDNoop(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	before := leaf.FreeSupply().SharableCPUs()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("UnclaimCPUs for an unknown UID must not panic, got: %v", r)
		}
	}()

	leaf.FreeSupply().UnclaimCPUs(types.UID("never-claimed"))

	if got := leaf.FreeSupply().SharableCPUs(); !got.Equals(before) {
		t.Errorf("sharable CPUs changed after unclaiming an unknown UID: got %s, want unchanged %s", got, before)
	}
}

func TestSupplyCloneCarriesClaimRefs(t *testing.T) {
	p := newDRATestPolicy(t)

	leaf := findPoolNode(t, p, "NUMA node #0")
	sharable := leaf.FreeSupply().SharableCPUs().List()
	if len(sharable) < 1 {
		t.Fatalf("expected at least 1 sharable CPU on %q", leaf.Name())
	}

	uid := types.UID("claim-uid-clone")
	cpus := cpuset.New(sharable[0])

	leaf.FreeSupply().ClaimCPUs(uid, cpus)

	clone := leaf.FreeSupply().Clone()

	// The clone must already reflect the subtraction (it was taken after the
	// claim), and — because claimRefs travels with Clone() — unclaiming on
	// the clone must be able to restore the CPU, proving the clone knows
	// which cpus belong to uid.
	beforeUnclaim := clone.SharableCPUs()
	if beforeUnclaim.Intersection(cpus).Size() != 0 {
		t.Fatalf("clone did not inherit the claimed subtraction: sharable=%s still contains %s", beforeUnclaim, cpus)
	}

	clone.UnclaimCPUs(uid)

	afterUnclaim := clone.SharableCPUs()
	if afterUnclaim.Intersection(cpus).Size() != cpus.Size() {
		t.Errorf("clone.UnclaimCPUs(%s) did not restore %s: sharable=%s -- Clone() must carry claimRefs",
			uid, cpus, afterUnclaim)
	}

	// The original leaf supply must be unaffected by unclaiming on the clone.
	if got := leaf.FreeSupply().SharableCPUs(); got.Intersection(cpus).Size() != 0 {
		t.Errorf("unclaiming on the clone leaked back into the original supply: sharable=%s still missing %s", got, cpus)
	}
}

func TestSupplyClaimCPUsAncestorNotDoubleSubtracted(t *testing.T) {
	p := newDRATestPolicy(t)

	// "NUMA node #0" and "NUMA node #2" are siblings under "socket #0".
	leafA := findPoolNode(t, p, "NUMA node #0")
	leafB := findPoolNode(t, p, "NUMA node #2")
	ancestor := findPoolNode(t, p, "socket #0")

	if leafA.Parent().Name() != ancestor.Name() || leafB.Parent().Name() != ancestor.Name() {
		t.Fatalf("expected %q and %q to share parent %q; got parents %q and %q",
			leafA.Name(), leafB.Name(), ancestor.Name(), leafA.Parent().Name(), leafB.Parent().Name())
	}

	cpusA := cpuset.New(leafA.FreeSupply().SharableCPUs().List()[0])
	cpusB := cpuset.New(leafB.FreeSupply().SharableCPUs().List()[0])
	if cpusA.Intersection(cpusB).Size() != 0 {
		t.Fatalf("test setup error: cpusA and cpusB must be disjoint, got %s and %s", cpusA, cpusB)
	}

	ancestorBefore := ancestor.FreeSupply().SharableCPUs()

	leafA.FreeSupply().ClaimCPUs(types.UID("claim-a"), cpusA)
	leafB.FreeSupply().ClaimCPUs(types.UID("claim-b"), cpusB)

	want := ancestorBefore.Difference(cpusA).Difference(cpusB)
	if got := ancestor.FreeSupply().SharableCPUs(); !got.Equals(want) {
		t.Errorf("ancestor %q sharable CPUs after two independent child claims = %s, want %s (exactly one subtraction per claim, no double-subtraction)",
			ancestor.Name(), got, want)
	}
}
