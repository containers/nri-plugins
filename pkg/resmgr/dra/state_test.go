/*
Copyright The NRI Plugins Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dra

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// newTestCache creates a real cache in a temporary directory. The cache rejects
// directories with group/other write bits (reject mask 0022); we create the
// sub-directory with explicit 0700 permissions to avoid a t.TempDir() umask
// clash on systems with umask 0002 (which would give 0775).
func newTestCache(t *testing.T) cache.Cache {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(cacheDir, 0700); err != nil {
		t.Fatalf("newTestCache: mkdir %q: %v", cacheDir, err)
	}
	c, err := cache.NewCache(cache.Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("newTestCache: cache.NewCache() error: %v", err)
	}
	return c
}

// TestMarshalUnmarshalClaims_RoundTrip verifies that a multi-alloc claim
// survives a marshal → unmarshal round-trip without data loss.
func TestMarshalUnmarshalClaims_RoundTrip(t *testing.T) {
	uid := types.UID("test-uid-1234")
	originalCPUs := cpuset.New(0, 1, 2, 3)

	claims := map[types.UID]*ClaimState{
		uid: {
			UID: string(uid),
			Allocs: []ResultAlloc{
				{
					Request:   "req-hp",
					Pool:      "node1",
					Device:    "sst-cp-0-0",
					ShareID:   "",
					ClassName: "hp",
					PkgID:     0,
					PunitID:   0,
					CPUs:      originalCPUs.String(),
				},
				{
					Request:   "req-hp-2",
					Pool:      "node1",
					Device:    "sst-cp-0-1",
					ShareID:   "share-abc",
					ClassName: "hp",
					PkgID:     0,
					PunitID:   1,
					CPUs:      cpuset.New(4, 5).String(),
				},
			},
		},
	}

	raw, err := marshalClaims(claims)
	if err != nil {
		t.Fatalf("marshalClaims() unexpected error: %v", err)
	}

	got, err := unmarshalClaims(raw)
	if err != nil {
		t.Fatalf("unmarshalClaims() unexpected error: %v", err)
	}

	cs, ok := got[uid]
	if !ok {
		t.Fatalf("unmarshalClaims(): uid %q not found in result", uid)
	}
	if cs.UID != string(uid) {
		t.Errorf("ClaimState.UID = %q, want %q", cs.UID, string(uid))
	}
	if len(cs.Allocs) != 2 {
		t.Fatalf("len(Allocs) = %d, want 2", len(cs.Allocs))
	}

	// Verify CPUs round-trip via cpuset.Parse.
	parsedCPUs, err := cpuset.Parse(cs.Allocs[0].CPUs)
	if err != nil {
		t.Fatalf("cpuset.Parse(%q) error: %v", cs.Allocs[0].CPUs, err)
	}
	if !parsedCPUs.Equals(originalCPUs) {
		t.Errorf("CPUs round-trip: got %v, want %v", parsedCPUs, originalCPUs)
	}

	// Verify other fields.
	if cs.Allocs[0].Request != "req-hp" {
		t.Errorf("Allocs[0].Request = %q, want %q", cs.Allocs[0].Request, "req-hp")
	}
	if cs.Allocs[1].ShareID != "share-abc" {
		t.Errorf("Allocs[1].ShareID = %q, want %q", cs.Allocs[1].ShareID, "share-abc")
	}
	if cs.Allocs[1].PunitID != 1 {
		t.Errorf("Allocs[1].PunitID = %d, want 1", cs.Allocs[1].PunitID)
	}
}

// TestUnmarshalClaims_Empty verifies that an empty map round-trips to an empty
// map (not nil).
func TestUnmarshalClaims_Empty(t *testing.T) {
	raw, err := marshalClaims(map[types.UID]*ClaimState{})
	if err != nil {
		t.Fatalf("marshalClaims({}) error: %v", err)
	}
	got, err := unmarshalClaims(raw)
	if err != nil {
		t.Fatalf("unmarshalClaims({}) error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("unmarshalClaims({}) = %v, want empty map", got)
	}
}

// TestCacheClaimStore_RoundTrip verifies that Save → Load preserves multi-alloc
// claim data using a real (file-backed) cache.
func TestCacheClaimStore_RoundTrip(t *testing.T) {
	c := newTestCache(t)

	uid := types.UID("claim-abc")
	originalCPUs := cpuset.New(10, 11, 12, 13)

	claims := map[types.UID]*ClaimState{
		uid: {
			UID: string(uid),
			Allocs: []ResultAlloc{
				{
					Request:   "req-0",
					Pool:      "node1",
					Device:    "dev-0",
					ShareID:   "",
					ClassName: "hp",
					PkgID:     1,
					PunitID:   2,
					CPUs:      originalCPUs.String(),
				},
			},
		},
	}

	store := NewCacheClaimStore(c)
	if err := store.Save(claims); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got == nil {
		t.Fatal("Load() returned nil, want non-nil map")
	}
	cs, ok := got[uid]
	if !ok {
		t.Fatalf("Load(): uid %q not found", uid)
	}
	if len(cs.Allocs) != 1 {
		t.Fatalf("Load(): len(Allocs) = %d, want 1", len(cs.Allocs))
	}

	// Verify CPUs survive round-trip.
	parsedCPUs, err := cpuset.Parse(cs.Allocs[0].CPUs)
	if err != nil {
		t.Fatalf("cpuset.Parse(%q) error: %v", cs.Allocs[0].CPUs, err)
	}
	if !parsedCPUs.Equals(originalCPUs) {
		t.Errorf("CPU round-trip: got %v, want %v", parsedCPUs, originalCPUs)
	}
}

// TestCacheClaimStore_EmptyCache verifies that Load returns nil, nil when
// no claim state has ever been saved.
func TestCacheClaimStore_EmptyCache(t *testing.T) {
	c := newTestCache(t)

	store := NewCacheClaimStore(c)
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() on empty cache error: %v", err)
	}
	if got != nil {
		t.Errorf("Load() on empty cache = %v, want nil", got)
	}
}

// TestCacheClaimStore_LoadReturnsSavedData verifies that Load returns the
// data previously saved (guarding against the composite-literal-address bug
// where the in-parameter to GetPolicyEntry would be unreachable).
func TestCacheClaimStore_LoadReturnsSavedData(t *testing.T) {
	c := newTestCache(t)

	uid := types.UID("claim-xyz")
	claims := map[types.UID]*ClaimState{
		uid: {
			UID: string(uid),
			Allocs: []ResultAlloc{
				{
					Request:   "req-x",
					Pool:      "pool-0",
					Device:    "dev-x",
					ClassName: "hp",
					PkgID:     0,
					PunitID:   0,
					CPUs:      cpuset.New(7, 8).String(),
				},
			},
		},
	}

	store := NewCacheClaimStore(c)
	if err := store.Save(claims); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Immediately call Load on the same store instance (tests the in-memory path).
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("Load() returned empty map — composite-literal-address bug?")
	}
	if _, ok := got[uid]; !ok {
		t.Errorf("Load() result missing uid %q", uid)
	}
}

// TestCacheClaimStore_MultiClaim verifies Save/Load with multiple claims in the
// same store entry.
func TestCacheClaimStore_MultiClaim(t *testing.T) {
	c := newTestCache(t)

	uid1 := types.UID("claim-001")
	uid2 := types.UID("claim-002")

	claims := map[types.UID]*ClaimState{
		uid1: {UID: string(uid1), Allocs: []ResultAlloc{
			{Request: "r1", Pool: "p", Device: "d1", ClassName: "hp", PkgID: 0, PunitID: 0, CPUs: cpuset.New(0).String()},
		}},
		uid2: {UID: string(uid2), Allocs: []ResultAlloc{
			{Request: "r2", Pool: "p", Device: "d2", ClassName: "hp", PkgID: 0, PunitID: 1, CPUs: cpuset.New(1).String()},
		}},
	}

	store := NewCacheClaimStore(c)
	if err := store.Save(claims); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Load() returned %d claims, want 2", len(got))
	}
	if _, ok := got[uid1]; !ok {
		t.Errorf("Load() missing uid %q", uid1)
	}
	if _, ok := got[uid2]; !ok {
		t.Errorf("Load() missing uid %q", uid2)
	}
}
