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
	"fmt"
	"os"
	"path"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils"
)

// sharedSys is the System object discovered once and reused by all tests.
// The policy only reads from System, so sharing the same instance is safe.
var sharedSys system.System

// TestMain extracts testdata/sysfs.tar.bz2 and discovers the System topology
// once for the whole test binary, making each individual test cheap.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nri-resources-test-shared-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	if err := utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		os.RemoveAll(dir) //nolint:errcheck
		fmt.Fprintf(os.Stderr, "failed to uncompress testdata: %v\n", err)
		os.Exit(1)
	}
	sysPath := path.Join(dir, "sysfs", "server", "sys")
	sharedSys, err = system.DiscoverSystemAt(sysPath)
	if err != nil {
		os.RemoveAll(dir) //nolint:errcheck
		fmt.Fprintf(os.Stderr, "failed to discover system: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir) //nolint:errcheck
	os.Exit(code)
}

// newTestPolicy creates a fresh policy from the shared System topology,
// avoiding both tarball extraction and filesystem scanning per test.
func newTestPolicy(t *testing.T) *policy {
	t.Helper()
	p := New().(*policy)
	if err := p.Setup(&policyapi.BackendOptions{
		Cache:  &mockCache{},
		System: sharedSys,
		Config: &cfgapi.Config{
			ReservedResources: cfgapi.Constraints{cfgapi.CPU: "750m"},
		},
	}); err != nil {
		t.Fatalf("failed to setup policy: %v", err)
	}
	return p
}

// findLeafDRAMPool returns the first leaf node that has DRAM and sharable CPUs.
func findLeafDRAMPool(pools []Node) Node {
	for _, n := range pools {
		if n.IsLeafNode() && n.HasMemoryType(memoryDRAM) && n.FreeSupply().SharableCPUs().Size() > 0 {
			return n
		}
	}
	return nil
}

// burstableContainer returns a mockContainer with a 500m CPU Burstable request
// (i.e. limits are higher than requests for some resource)
func burstableContainer(id string) *mockContainer {
	return &mockContainer{
		returnValueForGetID: id,
		returnValueForGetResourceRequirements: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("500m"),
				v1.ResourceMemory: resource.MustParse("1Mi"),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("750m"),
				v1.ResourceMemory: resource.MustParse("1Mi"),
			},
		},
		returnValueForQOSClass: v1.PodQOSBurstable,
	}
}

// hasGrant reports whether the policy holds an active grant for the container,
// using the exported ExportResourceData as the observable (returns nil when no
// grant exists).
func hasGrant(p *policy, ctr *mockContainer) bool {
	return p.ExportResourceData(ctr) != nil
}

// TestSupplyAllocate verifies that AllocateResources() creates a grant for the container.
func TestSupplyAllocate(t *testing.T) {
	p := newTestPolicy(t)

	ctr := burstableContainer("test-allocate")
	if err := p.AllocateResources(ctr); err != nil {
		t.Fatalf("AllocateResources: %v", err)
	}
	if !hasGrant(p, ctr) {
		t.Error("expected grant to exist after AllocateResources, but ExportResourceData returned nil")
	}
}

// TestSupplyAllocateInvalidContainer verifies that AllocateResources() returns an error
// when the container requests more CPUs than are available in any pool.
func TestSupplyAllocateInvalidContainer(t *testing.T) {
	p := newTestPolicy(t)

	// Request more CPUs than any single pool can satisfy.
	ctr := &mockContainer{
		returnValueForGetID: "test-allocate-invalid",
		returnValueForGetResourceRequirements: v1.ResourceRequirements{
			Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("9999")},
			Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("9999")},
		},
		returnValueForQOSClass: v1.PodQOSGuaranteed,
	}

	if err := p.AllocateResources(ctr); err == nil {
		t.Error("expected error from AllocateResources with oversized CPU request, got nil")
	}
	if hasGrant(p, ctr) {
		t.Error("expected no grant after failed AllocateResources, but ExportResourceData returned non-nil")
	}
}

// TestSupplyReleaseCPU verifies that ReleaseResources() removes the grant for the container.
func TestSupplyReleaseCPU(t *testing.T) {
	p := newTestPolicy(t)

	ctr := burstableContainer("test-release")
	if err := p.AllocateResources(ctr); err != nil {
		t.Fatalf("AllocateResources: %v", err)
	}
	if err := p.ReleaseResources(ctr); err != nil {
		t.Fatalf("ReleaseResources: %v", err)
	}
	if hasGrant(p, ctr) {
		t.Error("expected no grant after ReleaseResources, but ExportResourceData returned non-nil")
	}
}

// TestSupplyReleaseCPUNotAllocated verifies that ReleaseResources() on a container that
// was never allocated does not return an error and leaves no grant behind.
func TestSupplyReleaseCPUNotAllocated(t *testing.T) {
	p := newTestPolicy(t)

	ctr := burstableContainer("test-release-not-allocated")
	if err := p.ReleaseResources(ctr); err != nil {
		t.Errorf("ReleaseResources on unallocated container returned unexpected error: %v", err)
	}
	if hasGrant(p, ctr) {
		t.Error("expected no grant for never-allocated container, but ExportResourceData returned non-nil")
	}
}

// TestSupplyCumulate verifies that Cumulate() merges the sharable CPUs of two
// distinct leaf nodes, making the result larger than either individual supply.
// Only Supply (and its Clone/SharableCPUs methods) and Node.FreeSupply() are used —
// all exported.
func TestSupplyCumulate(t *testing.T) {
	p := newTestPolicy(t)

	// Collect two leaf nodes with non-zero sharable CPUs.
	var leaves []Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.FreeSupply().SharableCPUs().Size() > 0 {
			leaves = append(leaves, n)
			if len(leaves) == 2 {
				break
			}
		}
	}
	if len(leaves) < 2 {
		t.Fatal("need at least 2 leaf nodes with sharable CPUs")
	}

	s0 := leaves[0].FreeSupply().Clone()
	s1 := leaves[1].FreeSupply()
	before := s0.SharableCPUs().Size()

	s0.Cumulate(s1)

	// NUMA leaf nodes have non-overlapping CPU sets, so the union must be larger.
	if got := s0.SharableCPUs().Size(); got <= before {
		t.Errorf("SharableCPUs after Cumulate: got %d, want > %d", got, before)
	}
}

// TestSupplyCumulateSelf verifies that cumulating a supply with itself (or an
// identical clone) does not change its SharableCPUs size, because the union of
// a set with itself equals the original set.
func TestSupplyCumulateSelf(t *testing.T) {
	p := newTestPolicy(t)

	var leaf Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.FreeSupply().SharableCPUs().Size() > 0 {
			leaf = n
			break
		}
	}
	if leaf == nil {
		t.Fatal("no leaf node with sharable CPUs found")
	}

	s := leaf.FreeSupply().Clone()
	before := s.SharableCPUs().Size()

	// Cumulate with an identical clone — union is a no-op.
	s.Cumulate(leaf.FreeSupply().Clone())

	if got := s.SharableCPUs().Size(); got != before {
		t.Errorf("SharableCPUs after self-Cumulate: got %d, want %d (unchanged)", got, before)
	}
}

// ---------------------------------------------------------------------------
// GetNode
// ---------------------------------------------------------------------------

// TestSupplyGetNode verifies that GetNode() returns the node the supply belongs
// to, identifiable by matching names.
func TestSupplyGetNode(t *testing.T) {
	p := newTestPolicy(t)

	for _, pool := range p.pools {
		if got, want := pool.FreeSupply().GetNode().Name(), pool.Name(); got != want {
			t.Errorf("pool %q: FreeSupply().GetNode().Name() = %q, want %q", pool.Name(), got, want)
		}
	}
}

// TestSupplyGetNodeDistinct verifies that two different pools have different
// nodes according to GetNode(), confirming the getter is not constant.
func TestSupplyGetNodeDistinct(t *testing.T) {
	p := newTestPolicy(t)

	if len(p.pools) < 2 {
		t.Fatal("need at least 2 pools")
	}
	n0 := p.pools[0].FreeSupply().GetNode().Name()
	n1 := p.pools[1].FreeSupply().GetNode().Name()
	if n0 == n1 {
		t.Errorf("different pools returned the same GetNode name %q", n0)
	}
}

// ---------------------------------------------------------------------------
// Clone
// ---------------------------------------------------------------------------

// TestSupplyClone verifies that a cloned supply has the same SharableCPUs as
// the original.
func TestSupplyClone(t *testing.T) {
	p := newTestPolicy(t)

	leaf := findLeafDRAMPool(p.pools)
	if leaf == nil {
		t.Fatal("no suitable leaf pool found")
	}
	s := leaf.FreeSupply()
	clone := s.Clone()
	if !clone.SharableCPUs().Equals(s.SharableCPUs()) {
		t.Errorf("Clone SharableCPUs %v != original %v", clone.SharableCPUs(), s.SharableCPUs())
	}
}

// TestSupplyCloneIndependence verifies that Cumulating into a clone does not
// affect the original supply's SharableCPUs.
func TestSupplyCloneIndependence(t *testing.T) {
	p := newTestPolicy(t)

	var leaves []Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.FreeSupply().SharableCPUs().Size() > 0 {
			leaves = append(leaves, n)
			if len(leaves) == 2 {
				break
			}
		}
	}
	if len(leaves) < 2 {
		t.Fatal("need at least 2 leaf pools with sharable CPUs")
	}
	original := leaves[0].FreeSupply()
	before := original.SharableCPUs().Size()

	clone := original.Clone()
	clone.Cumulate(leaves[1].FreeSupply())

	if got := original.SharableCPUs().Size(); got != before {
		t.Errorf("original SharableCPUs changed after Cumulate into clone: got %d, want %d", got, before)
	}
}

// ---------------------------------------------------------------------------
// IsolatedCPUs
// ---------------------------------------------------------------------------

// TestSupplyIsolatedCPUs verifies that at least one pool reports isolated CPUs
// (the server test topology has isolated CPUs on each NUMA node).
func TestSupplyIsolatedCPUs(t *testing.T) {
	p := newTestPolicy(t)

	for _, pool := range p.pools {
		if pool.GetSupply().IsolatedCPUs().Size() > 0 {
			return // found one — test passes
		}
	}
	t.Error("expected at least one pool to have isolated CPUs, found none")
}

// TestSupplyIsolatedCPUsRootAggregates verifies that the root node has at
// least as many isolated CPUs as any single leaf (root is the aggregate).
func TestSupplyIsolatedCPUsRootAggregates(t *testing.T) {
	p := newTestPolicy(t)

	rootIsolated := p.root.GetSupply().IsolatedCPUs().Size()
	for _, pool := range p.pools {
		if pool.IsLeafNode() {
			leafIsolated := pool.GetSupply().IsolatedCPUs().Size()
			if leafIsolated > rootIsolated {
				t.Errorf("leaf %q has %d isolated CPUs, more than root's %d",
					pool.Name(), leafIsolated, rootIsolated)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// ReservedCPUs
// ---------------------------------------------------------------------------

// TestSupplyReservedCPUs verifies that the root pool has non-empty reserved
// CPUs (the test policy is configured with 750m reserved).
func TestSupplyReservedCPUs(t *testing.T) {
	p := newTestPolicy(t)

	if p.root.GetSupply().ReservedCPUs().IsEmpty() {
		t.Error("expected root GetSupply().ReservedCPUs() to be non-empty with 750m reserved config")
	}
}

// TestSupplyReservedCPUsLeafEmpty verifies that not all leaf nodes carry
// reserved CPUs: at least one leaf has empty ReservedCPUs() (the reserved
// CPU lives on one NUMA node only) and at least one has non-empty
// ReservedCPUs() (confirming the 750m config is in effect).
func TestSupplyReservedCPUsLeafEmpty(t *testing.T) {
	p := newTestPolicy(t)

	var withReserved, withoutReserved int
	for _, pool := range p.pools {
		if !pool.IsLeafNode() {
			continue
		}
		if pool.GetSupply().ReservedCPUs().IsEmpty() {
			withoutReserved++
		} else {
			withReserved++
		}
	}
	if withReserved == 0 {
		t.Error("expected at least one leaf to have reserved CPUs (750m config)")
	}
	if withoutReserved == 0 {
		t.Error("expected at least one leaf to have empty reserved CPUs")
	}
}

// ---------------------------------------------------------------------------
// SharableCPUs
// ---------------------------------------------------------------------------

// TestSupplySharableCPUs verifies that each leaf pool has sharable CPUs.
func TestSupplySharableCPUs(t *testing.T) {
	p := newTestPolicy(t)

	for _, pool := range p.pools {
		if pool.IsLeafNode() && pool.GetSupply().SharableCPUs().IsEmpty() {
			t.Errorf("leaf %q has no sharable CPUs", pool.Name())
		}
	}
}

// TestSupplySharableCPUsRootAggregates verifies that the root supply has more
// sharable CPUs than any individual leaf (root accumulates all).
func TestSupplySharableCPUsRootAggregates(t *testing.T) {
	p := newTestPolicy(t)

	rootSharable := p.root.GetSupply().SharableCPUs().Size()
	for _, pool := range p.pools {
		if pool.IsLeafNode() {
			if leafSharable := pool.GetSupply().SharableCPUs().Size(); leafSharable >= rootSharable {
				t.Errorf("leaf %q has %d sharable CPUs >= root's %d",
					pool.Name(), leafSharable, rootSharable)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// GrantedReserved
// ---------------------------------------------------------------------------

// TestSupplyGrantedReservedInitiallyZero verifies that GrantedReserved() is 0
// on the root before any allocation.
func TestSupplyGrantedReservedInitiallyZero(t *testing.T) {
	p := newTestPolicy(t)

	if got := p.root.FreeSupply().GrantedReserved(); got != 0 {
		t.Errorf("GrantedReserved() before allocation: got %d, want 0", got)
	}
}

// TestSupplyGrantedReservedAfterKubeSystemAlloc verifies that GrantedReserved()
// increases on the root after allocating resources for a kube-system container,
// which is steered to the reserved CPU pool.
func TestSupplyGrantedReservedAfterKubeSystemAlloc(t *testing.T) {
	p := newTestPolicy(t)

	ctr := &mockContainer{
		returnValueForGetID:    "kube-system-ctr",
		namespace:              metav1.NamespaceSystem,
		returnValueForQOSClass: v1.PodQOSBurstable,
		returnValueForGetResourceRequirements: v1.ResourceRequirements{
			Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
			Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")},
		},
	}
	if err := p.AllocateResources(ctr); err != nil {
		t.Fatalf("AllocateResources: %v", err)
	}
	if got := p.root.FreeSupply().GrantedReserved(); got <= 0 {
		t.Errorf("GrantedReserved() after kube-system alloc: got %d, want > 0", got)
	}
}

// ---------------------------------------------------------------------------
// GrantedShared
// ---------------------------------------------------------------------------

// TestSupplyGrantedSharedInitiallyZero verifies that GrantedShared() is 0 on
// all pools before any allocation.
func TestSupplyGrantedSharedInitiallyZero(t *testing.T) {
	p := newTestPolicy(t)

	for _, pool := range p.pools {
		if got := pool.FreeSupply().GrantedShared(); got != 0 {
			t.Errorf("pool %q: GrantedShared() before allocation = %d, want 0", pool.Name(), got)
		}
	}
}

// TestSupplyGrantedSharedAfterAlloc verifies that at least one pool's
// GrantedShared() is non-zero after a Burstable container allocation.
func TestSupplyGrantedSharedAfterAlloc(t *testing.T) {
	p := newTestPolicy(t)

	ctr := burstableContainer("granted-shared-test")
	if err := p.AllocateResources(ctr); err != nil {
		t.Fatalf("AllocateResources: %v", err)
	}
	for _, pool := range p.pools {
		if pool.FreeSupply().GrantedShared() > 0 {
			return // found one — test passes
		}
	}
	t.Error("expected at least one pool to have GrantedShared() > 0 after allocation")
}

// ---------------------------------------------------------------------------
// AllocatableSharedCPU
// ---------------------------------------------------------------------------

// TestSupplyAllocatableSharedCPU verifies that each leaf pool has positive
// allocatable shared CPU before any allocation.
func TestSupplyAllocatableSharedCPU(t *testing.T) {
	p := newTestPolicy(t)

	for _, pool := range p.pools {
		if pool.IsLeafNode() {
			if got := pool.FreeSupply().AllocatableSharedCPU(); got <= 0 {
				t.Errorf("leaf %q: AllocatableSharedCPU() = %d, want > 0", pool.Name(), got)
			}
		}
	}
}

// TestSupplyAllocatableSharedCPUDecreasesAfterAlloc verifies that the pool
// assigned to a Burstable 500m container has 500m less allocatable shared CPU.
func TestSupplyAllocatableSharedCPUDecreasesAfterAlloc(t *testing.T) {
	p := newTestPolicy(t)

	before := make(map[string]int, len(p.pools))
	for _, pool := range p.pools {
		before[pool.Name()] = pool.FreeSupply().AllocatableSharedCPU()
	}

	ctr := burstableContainer("allocatable-shared-test")
	if err := p.AllocateResources(ctr); err != nil {
		t.Fatalf("AllocateResources: %v", err)
	}

	for _, pool := range p.pools {
		diff := before[pool.Name()] - pool.FreeSupply().AllocatableSharedCPU()
		if diff == 500 {
			return // exactly one pool decreased by the 500m fraction — test passes
		}
	}
	t.Error("expected exactly one pool to show a 500m decrease in AllocatableSharedCPU after allocation")
}

// ---------------------------------------------------------------------------
// SliceableCPUs
// ---------------------------------------------------------------------------

// TestSupplySliceableCPUs verifies that a fresh leaf pool returns a non-empty
// sliceable CPUSet with no error.
func TestSupplySliceableCPUs(t *testing.T) {
	p := newTestPolicy(t)

	leaf := findLeafDRAMPool(p.pools)
	if leaf == nil {
		t.Fatal("no suitable leaf pool found")
	}
	sliceable, err := leaf.FreeSupply().SliceableCPUs()
	if err != nil {
		t.Fatalf("SliceableCPUs() returned error: %v", err)
	}
	if sliceable.IsEmpty() {
		t.Error("SliceableCPUs() returned empty CPUSet for fresh leaf pool")
	}
}

// TestSupplySliceableCPUsSubsetOfSharable verifies that SliceableCPUs() always
// returns a subset of SharableCPUs() — sliceable CPUs can only come from the
// sharable pool.
func TestSupplySliceableCPUsSubsetOfSharable(t *testing.T) {
	p := newTestPolicy(t)

	for _, pool := range p.pools {
		s := pool.FreeSupply()
		sliceable, err := s.SliceableCPUs()
		if err != nil {
			continue // skip pools where slicing is not possible
		}
		sharable := s.SharableCPUs()
		if !sharable.Intersection(sliceable).Equals(sliceable) {
			t.Errorf("pool %q: sliceable %v is not a subset of sharable %v",
				pool.Name(), sliceable, sharable)
		}
	}
}
