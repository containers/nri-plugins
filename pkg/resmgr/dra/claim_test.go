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

package dra

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	specs "tags.cncf.io/container-device-interface/specs-go"
)

const (
	otherDriverName = "other.example.com"
	testPoolName    = "test-pool"
)

// testResult is one allocated device of a claim. Its pool is deliberately not
// named after the node, so a pool we answer with is provably the claim's.
func testResult(driver, request, device string) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{
		Request: request,
		Driver:  driver,
		Pool:    testPoolName,
		Device:  device,
	}
}

// testClaim is an allocated claim with the given results.
func testClaim(uid types.UID, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "claim-" + string(uid),
			UID:       uid,
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
		},
	}
}

// specFiles are the spec files the plugin has written.
func specFiles(t *testing.T, p *Plugin) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(p.cdi.specDir(t), "*"))
	if err != nil {
		t.Fatalf("failed to read the spec directory: %v", err)
	}

	return files
}

// prepare prepares a single claim which must succeed, for the tests which start
// from a prepared one.
func prepare(t *testing.T, p *Plugin, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	t.Helper()

	result, err := p.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("PrepareResourceClaims() failed: %v", err)
	}

	prepared, ok := result[claim.UID]
	if !ok {
		t.Fatalf("claim %s has no result", claim.UID)
	}
	if prepared.Err != nil {
		t.Fatalf("claim %s failed: %v", claim.UID, prepared.Err)
	}

	return prepared
}

// unprepare unprepares the given claim UIDs, named the way testClaim names them.
func unprepare(t *testing.T, p *Plugin, uids ...types.UID) map[types.UID]error {
	t.Helper()

	claims := make([]kubeletplugin.NamespacedObject, 0, len(uids))
	for _, uid := range uids {
		claims = append(claims, kubeletplugin.NamespacedObject{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "claim-" + string(uid)},
			UID:            uid,
		})
	}

	result, err := p.UnprepareResourceClaims(t.Context(), claims)
	if err != nil {
		t.Fatalf("UnprepareResourceClaims() failed: %v", err)
	}

	return result
}

// TestPrepareResourceClaims verifies that a claim is allocated by the policy and
// answered with the CDI devices which carry the policy's edits, that only our own
// allocation results take part, and that all of it happens under the owner's lock.
//
// One result also carries a consumed capacity, which the driver must prepare
// without looking at: it reaches the policy untouched in the results below, and
// nothing the driver writes or answers is derived from it.
func TestPrepareResourceClaims(t *testing.T) {
	shareID := types.UID("2f1e0d9c-8b7a-4695-8213-0fedcba98765")

	ours := []resourceapi.DeviceRequestAllocationResult{
		testResult(testDriverName, "request-0", "cpu-0"),
		testResult(testDriverName, "request-1", "cpu-1"),
	}
	ours[0].ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{
		"cpu": resource.MustParse("2"),
	}
	ours[1].ShareID = &shareID

	claim := testClaim(testClaimUID,
		ours[0],
		testResult(otherDriverName, "request-gpu", "gpu-0"),
		ours[1],
	)
	policy := &testPolicy{}
	p, owner := newTestPlugin(t, fake.NewClientset(), policy)

	prepared := prepare(t, p, claim)

	devices := []kubeletplugin.Device{
		{
			Requests:     []string{"request-0"},
			PoolName:     testPoolName,
			DeviceName:   "cpu-0",
			CDIDeviceIDs: []string{testDriverName + "/device=claim-" + string(testClaimUID) + "-0"},
		},
		{
			Requests:     []string{"request-1"},
			PoolName:     testPoolName,
			DeviceName:   "cpu-1",
			CDIDeviceIDs: []string{testDriverName + "/device=claim-" + string(testClaimUID) + "-1"},
			ShareID:      &shareID,
		},
	}
	if !reflect.DeepEqual(prepared.Devices, devices) {
		t.Errorf("prepared devices are\n%+v\nexpected\n%+v", prepared.Devices, devices)
	}

	// The policy gets the claim itself and the results which are ours, nothing
	// more and nothing less.
	if len(policy.claims) != 1 || policy.claims[0] != claim {
		t.Errorf("policy was asked to allocate %v, expected claim %s", policy.claims, claim.UID)
	}
	if allocated := policy.allocated[claim.UID]; !reflect.DeepEqual(allocated, ours) {
		t.Errorf("policy allocated results\n%+v\nexpected\n%+v", allocated, ours)
	}

	// The spec grants what the policy said, one device per allocated device.
	spec := readSpec(t, p.cdi, claim.UID)
	specDevices := []specs.Device{
		{
			Name:           "claim-" + string(testClaimUID) + "-0",
			ContainerEdits: specs.ContainerEdits{Env: []string{"NRI_DEVICE=cpu-0"}},
		},
		{
			Name:           "claim-" + string(testClaimUID) + "-1",
			ContainerEdits: specs.ContainerEdits{Env: []string{"NRI_DEVICE=cpu-1"}},
		},
	}
	if !reflect.DeepEqual(spec.Devices, specDevices) {
		t.Errorf("spec devices are\n%+v\nexpected\n%+v", spec.Devices, specDevices)
	}

	if owner.locks != 1 || owner.unlocks != 1 {
		t.Errorf("locked %d times and unlocked %d times, expected once each",
			owner.locks, owner.unlocks)
	}
	if owner.updates != 1 {
		t.Errorf("committed %d claim update(s), expected one", owner.updates)
	}
}

// TestClaimsBeforeAllowed verifies that claims are refused until the owner has
// synchronized with the runtime. Both directions are refused as a whole request,
// which is what makes the kubelet ask again, and neither reaches the policy: an
// allocation made during the synchronization would race with it, and a commit
// made during it would be silently dropped.
func TestClaimsBeforeAllowed(t *testing.T) {
	claim := testClaim(testClaimUID, testResult(testDriverName, "request-0", "cpu-0"))

	policy := &testPolicy{}
	p, owner := newTestPlugin(t, fake.NewClientset(), policy)

	// As we are between registering the driver and the first synchronization.
	p.allowed.Store(false)

	if _, err := p.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim}); err == nil {
		t.Errorf("PrepareResourceClaims() succeeded, expected it to be refused")
	}

	unprepared := []kubeletplugin.NamespacedObject{{UID: claim.UID}}
	if _, err := p.UnprepareResourceClaims(t.Context(), unprepared); err == nil {
		t.Errorf("UnprepareResourceClaims() succeeded, expected it to be refused")
	}

	if len(policy.claims) != 0 || len(policy.releases) != 0 {
		t.Errorf("policy was asked to allocate %v and release %v, expected neither",
			policy.claims, policy.releases)
	}
	if owner.locks != 0 {
		t.Errorf("took the owner's lock %d time(s), expected none", owner.locks)
	}
	if files := specFiles(t, p); len(files) != 0 {
		t.Errorf("wrote spec file(s) %v, expected none", files)
	}
}

// TestPrepareResourceClaimsUnallocated verifies claims with nothing of ours in
// them are left alone: one with no allocation at all, one allocated from another
// driver only. The kubelet only asks the drivers a claim was allocated from, so
// there is nothing to fail about, but also nothing to prepare.
func TestPrepareResourceClaimsUnallocated(t *testing.T) {
	claims := []*resourceapi.ResourceClaim{
		{ObjectMeta: metav1.ObjectMeta{UID: "unallocated-uid"}},
		testClaim("other-driver-uid", testResult(otherDriverName, "request-gpu", "gpu-0")),
	}

	policy := &testPolicy{}
	p, owner := newTestPlugin(t, fake.NewClientset(), policy)

	result, err := p.PrepareResourceClaims(t.Context(), claims)
	if err != nil {
		t.Fatalf("PrepareResourceClaims() failed: %v", err)
	}

	for _, claim := range claims {
		prepared, ok := result[claim.UID]
		switch {
		case !ok:
			t.Errorf("claim %s has no result", claim.UID)
		case prepared.Err != nil:
			t.Errorf("claim %s failed: %v", claim.UID, prepared.Err)
		case len(prepared.Devices) != 0:
			t.Errorf("claim %s got %d devices, expected none", claim.UID, len(prepared.Devices))
		}
	}

	if len(policy.claims) != 0 {
		t.Errorf("policy was asked to allocate %v, expected nothing", policy.claims)
	}
	if owner.updates != 0 {
		t.Errorf("committed %d claim update(s), expected none", owner.updates)
	}
	if files := specFiles(t, p); len(files) != 0 {
		t.Errorf("wrote spec file(s) %v, expected none", files)
	}
}

// TestPrepareResourceClaimsIsolation verifies a claim which cannot be prepared
// fails on its own: the kubelet asks for all the claims of a pod at once and
// retries the ones which failed.
func TestPrepareResourceClaimsIsolation(t *testing.T) {
	var (
		good    = testClaim("good-uid", testResult(testDriverName, "request-0", "cpu-0"))
		bad     = testClaim("bad-uid", testResult(testDriverName, "request-0", "cpu-1"))
		failure = errors.New("no CPUs left")
	)

	policy := &testPolicy{allocateErr: failure, failUID: bad.UID}
	p, owner := newTestPlugin(t, fake.NewClientset(), policy)

	result, err := p.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{good, bad})
	if err != nil {
		t.Fatalf("PrepareResourceClaims() failed: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("got %d results, expected two", len(result))
	}
	if prepared := result[good.UID]; prepared.Err != nil {
		t.Errorf("claim %s failed: %v", good.UID, prepared.Err)
	} else if len(prepared.Devices) != 1 {
		t.Errorf("claim %s got %d devices, expected one", good.UID, len(prepared.Devices))
	}
	if prepared := result[bad.UID]; !errors.Is(prepared.Err, failure) {
		t.Errorf("claim %s failed with %v, expected it to wrap %v", bad.UID, prepared.Err, failure)
	}

	// The failed claim leaves nothing behind, the other one is prepared.
	if files := specFiles(t, p); len(files) != 1 {
		t.Errorf("wrote spec file(s) %v, expected one", files)
	}
	if len(policy.releases) != 0 {
		t.Errorf("policy was asked to release %v, expected nothing", policy.releases)
	}

	// Two claims, one lock: the lock belongs to the request, not to the claim.
	if owner.locks != 1 || owner.unlocks != 1 {
		t.Errorf("locked %d times and unlocked %d times for one request, expected once each",
			owner.locks, owner.unlocks)
	}
}

// TestPrepareResourceClaimsRollback verifies that a claim allocated by the policy
// but not prepared by us is given back: the policy must not be left accounting
// for resources no container ever got.
func TestPrepareResourceClaimsRollback(t *testing.T) {
	for _, tc := range []struct {
		name string
		// edits replaces what the policy returns, updateErr fails the commit.
		edits     func([]resourceapi.DeviceRequestAllocationResult) []specs.ContainerEdits
		updateErr error
	}{
		{
			// A policy bug: the edits no longer say which device is which.
			name:  "edits not matching the allocated devices",
			edits: func([]resourceapi.DeviceRequestAllocationResult) []specs.ContainerEdits { return nil },
		},
		{
			name:      "allocation which cannot be committed",
			updateErr: errors.New("failed to save cache"),
		},
		{
			// A spec with a device granting nothing is rejected by CDI itself.
			name: "edits which cannot be written",
			edits: func(results []resourceapi.DeviceRequestAllocationResult) []specs.ContainerEdits {
				return make([]specs.ContainerEdits, len(results))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := testClaim(testClaimUID, testResult(testDriverName, "request-0", "cpu-0"))

			policy := &testPolicy{edits: tc.edits}
			p, owner := newTestPlugin(t, fake.NewClientset(), policy)
			owner.updateErr = tc.updateErr

			result, err := p.PrepareResourceClaims(t.Context(),
				[]*resourceapi.ResourceClaim{claim})
			if err != nil {
				t.Fatalf("PrepareResourceClaims() failed: %v", err)
			}

			prepared := result[claim.UID]
			if prepared.Err == nil {
				t.Errorf("claim %s was prepared, expected it to fail", claim.UID)
			}
			if len(prepared.Devices) != 0 {
				t.Errorf("claim %s got %d devices, expected none", claim.UID, len(prepared.Devices))
			}
			if !reflect.DeepEqual(policy.releases, []types.UID{claim.UID}) {
				t.Errorf("policy was asked to release %v, expected claim %s",
					policy.releases, claim.UID)
			}
			if files := specFiles(t, p); len(files) != 0 {
				t.Errorf("left spec file(s) %v behind, expected none", files)
			}
		})
	}
}

// TestPrepareResourceClaimsAgain verifies that preparing a prepared claim reaches
// the policy again and answers the same. The kubelet repeats a request whose
// answer it lost, and only the policy knows what it has already allocated.
func TestPrepareResourceClaimsAgain(t *testing.T) {
	claim := testClaim(testClaimUID, testResult(testDriverName, "request-0", "cpu-0"))

	policy := &testPolicy{}
	p, _ := newTestPlugin(t, fake.NewClientset(), policy)

	first := prepare(t, p, claim)
	second := prepare(t, p, claim)

	if !reflect.DeepEqual(first.Devices, second.Devices) {
		t.Errorf("re-preparing gave devices\n%+v\nexpected\n%+v", second.Devices, first.Devices)
	}
	if len(policy.claims) != 2 {
		t.Errorf("policy was asked to allocate %d time(s), expected twice", len(policy.claims))
	}
	if files := specFiles(t, p); len(files) != 1 {
		t.Errorf("wrote spec file(s) %v, expected one", files)
	}
}

// TestPrepareResourceClaimsAgainRollback verifies that a failed *re*-prepare
// leaves the earlier allocation alone. AllocateClaim is idempotent and releasing
// is per claim, not per attempt, so rolling back here would take resources away
// from the containers of a pod which is already using the claim.
func TestPrepareResourceClaimsAgainRollback(t *testing.T) {
	claim := testClaim(testClaimUID, testResult(testDriverName, "request-0", "cpu-0"))

	policy := &testPolicy{}
	p, owner := newTestPlugin(t, fake.NewClientset(), policy)

	prepare(t, p, claim)

	owner.updateErr = errors.New("failed to save cache")

	result, err := p.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("PrepareResourceClaims() failed: %v", err)
	}

	if prepared := result[claim.UID]; prepared.Err == nil {
		t.Errorf("claim %s was prepared, expected it to fail", claim.UID)
	}
	if len(policy.releases) != 0 {
		t.Errorf("policy was asked to release %v, expected the allocation to stay",
			policy.releases)
	}
	if files := specFiles(t, p); len(files) != 1 {
		t.Errorf("spec file(s) %v, expected the claim to keep its spec", files)
	}
}

// TestUnprepareResourceClaims verifies that unpreparing a claim gives its
// resources back and removes its spec, under the owner's lock.
func TestUnprepareResourceClaims(t *testing.T) {
	claim := testClaim(testClaimUID, testResult(testDriverName, "request-0", "cpu-0"))

	policy := &testPolicy{}
	p, owner := newTestPlugin(t, fake.NewClientset(), policy)

	prepare(t, p, claim)

	// Unpreparing twice, and unpreparing a claim we never prepared, must all
	// succeed: the kubelet may ask more than once, and for a claim whose
	// preparation it never got an answer for.
	uids := []types.UID{claim.UID, claim.UID, "unknown-uid"}

	result := unprepare(t, p, uids...)

	if len(result) != 2 {
		t.Errorf("got %d results, expected two", len(result))
	}
	for _, uid := range uids {
		if err, ok := result[uid]; !ok {
			t.Errorf("claim %s has no result", uid)
		} else if err != nil {
			t.Errorf("claim %s failed: %v", uid, err)
		}
	}

	if len(policy.allocated) != 0 {
		t.Errorf("policy still accounts for %v", policy.allocated)
	}
	if files := specFiles(t, p); len(files) != 0 {
		t.Errorf("left spec file(s) %v behind, expected none", files)
	}

	// Once for the prepare and once for the unprepare: the lock is taken per
	// kubelet request, not per claim, and the second request carries three.
	if owner.locks != 2 || owner.unlocks != 2 {
		t.Errorf("locked %d times and unlocked %d times, expected twice each",
			owner.locks, owner.unlocks)
	}
	if owner.updates != 4 {
		t.Errorf("committed %d claim update(s), expected four", owner.updates)
	}
}

// TestUnprepareResourceClaimsCommitError verifies that a release which cannot be
// committed keeps the claim's spec. The policy has given the resources back, but
// nothing has recorded that yet, so the devices granting them must outlive the
// failure and go away with the retry that commits it.
func TestUnprepareResourceClaimsCommitError(t *testing.T) {
	claim := testClaim(testClaimUID, testResult(testDriverName, "request-0", "cpu-0"))

	policy := &testPolicy{}
	p, owner := newTestPlugin(t, fake.NewClientset(), policy)

	prepare(t, p, claim)

	failure := errors.New("failed to save cache")
	owner.updateErr = failure

	result := unprepare(t, p, claim.UID)

	if !errors.Is(result[claim.UID], failure) {
		t.Errorf("claim %s failed with %v, expected it to wrap %v",
			claim.UID, result[claim.UID], failure)
	}
	if files := specFiles(t, p); len(files) != 1 {
		t.Errorf("spec file(s) %v, expected the claim to keep its spec", files)
	}
}

// TestUnprepareResourceClaimsPolicyError verifies that a claim the policy cannot
// release keeps its spec: the resources are still accounted for, so the devices
// granting them must stay until a retry gets rid of both.
func TestUnprepareResourceClaimsPolicyError(t *testing.T) {
	claim := testClaim(testClaimUID, testResult(testDriverName, "request-0", "cpu-0"))

	policy := &testPolicy{}
	p, _ := newTestPlugin(t, fake.NewClientset(), policy)

	prepare(t, p, claim)

	failure := errors.New("no such claim")
	policy.releaseErr = failure

	result := unprepare(t, p, claim.UID)

	if !errors.Is(result[claim.UID], failure) {
		t.Errorf("claim %s failed with %v, expected it to wrap %v",
			claim.UID, result[claim.UID], failure)
	}
	if files := specFiles(t, p); len(files) != 1 {
		t.Errorf("spec file(s) %v, expected the claim to keep its spec", files)
	}
}
