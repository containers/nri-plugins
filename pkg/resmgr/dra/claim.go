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
	"context"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// PrepareResourceClaims prepares the given claims: the policy allocates the
// resources of each claim and we hand the kubelet the CDI devices which grant
// them to the containers using the claim.
//
// Every claim is answered on its own. The kubelet asks for all the claims of a
// pod in a single request, but it records the answer per claim and retries the
// ones which failed, so failing them all together would only throw away the
// claims we did prepare.
func (p *Plugin) PrepareResourceClaims(_ context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	if err := p.claimsAllowed(); err != nil {
		return nil, err
	}

	p.owner.Lock()
	defer p.owner.Unlock()

	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		devices, err := p.prepareClaim(claim)
		if err != nil {
			log.Errorf("failed to prepare claim %s/%s: %v", claim.Namespace, claim.Name, err)
			result[claim.UID] = kubeletplugin.PrepareResult{Err: err}
			continue
		}
		result[claim.UID] = kubeletplugin.PrepareResult{Devices: devices}
	}

	return result, nil
}

// UnprepareResourceClaims releases the resources prepared for the given claims.
func (p *Plugin) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	if err := p.claimsAllowed(); err != nil {
		return nil, err
	}

	p.owner.Lock()
	defer p.owner.Unlock()

	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		err := p.releaseClaim(claim.UID)
		if err != nil {
			log.Errorf("failed to unprepare claim %s: %v", claim, err)
		}
		result[claim.UID] = err
	}

	return result, nil
}

// claimsAllowed fails a kubelet request we are not ready to serve. The whole
// request is refused rather than each of its claims: there is nothing wrong with
// any of them, and the kubelet retries a request it could not get an answer to.
func (p *Plugin) claimsAllowed() error {
	if !p.allowed.Load() {
		return fmt.Errorf("dra: driver %s cannot serve claims yet, still synchronizing with the runtime",
			p.driverName)
	}

	return nil
}

// prepareClaim prepares a single claim and returns the devices its requests were
// prepared as.
func (p *Plugin) prepareClaim(claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {
	results := p.claimResults(claim)
	if len(results) == 0 {
		// The kubelet only asks the drivers a claim was allocated from, so this
		// is not supposed to happen. Nothing of ours is in the claim, so there is
		// nothing to allocate and nothing to inject either.
		log.Warnf("claim %s has no devices of ours, nothing to prepare", claim.UID)
		return nil, nil
	}

	// Whether a failure below may hand the allocation back. A spec is the last
	// thing a successful prepare writes, so a claim without one was never
	// prepared and nothing can be using its devices, which makes releasing it
	// safe. A claim which has one may have running containers on those devices,
	// and taking their resources away would be worse than leaving the allocation
	// in place until the kubelet unprepares the claim.
	prepared, err := p.cdi.HasClaim(claim.UID)
	if err != nil {
		return nil, err
	}
	rollback := !prepared

	// The policy is asked to allocate the claim even if we already have a spec
	// for it. Re-preparing a prepared claim is normal, the kubelet repeats a
	// request whose answer it lost, and the policy is the one which knows what it
	// has already allocated. Skipping it here, on the strength of a file we once
	// wrote, would leave a claim whose spec outlived its accounting unaccounted
	// for forever.
	edits, err := p.policy.AllocateClaim(claim, results)
	if err != nil {
		return nil, fmt.Errorf("dra: policy failed to allocate claim %s: %w", claim.UID, err)
	}
	if len(edits) != len(results) {
		p.undoClaim(claim.UID, rollback)
		return nil, fmt.Errorf("dra: policy returned %d container edit(s) for the %d device(s) of claim %s",
			len(edits), len(results), claim.UID)
	}

	// Commit the allocation before writing the spec which grants it: a crash
	// between the two then leaves a spec nothing claims, which we can find and
	// clean up, rather than resources granted to a container but accounted to
	// nobody.
	if err := p.owner.ClaimAllocated(); err != nil {
		p.undoClaim(claim.UID, rollback)
		return nil, fmt.Errorf("dra: failed to commit the allocation of claim %s: %w", claim.UID, err)
	}

	names, err := p.cdi.WriteClaim(claim.UID, edits)
	if err != nil {
		p.undoClaim(claim.UID, rollback)
		return nil, err
	}

	// One CDI device per allocated device, in the order the policy's edits came
	// in, which is the order of the allocation results.
	devices := make([]kubeletplugin.Device, 0, len(results))
	for i, r := range results {
		devices = append(devices, kubeletplugin.Device{
			Requests:     []string{r.Request},
			PoolName:     r.Pool,
			DeviceName:   r.Device,
			CDIDeviceIDs: []string{names[i]},
			ShareID:      r.ShareID,
		})
	}

	return devices, nil
}

// releaseClaim releases the resources of a claim and removes its CDI spec. A
// claim we know nothing about is not an error: the kubelet may ask to unprepare
// the same claim more than once, and one it never got a successful answer for.
func (p *Plugin) releaseClaim(uid types.UID) error {
	if err := p.policy.ReleaseClaim(uid); err != nil {
		return fmt.Errorf("dra: policy failed to release claim %s: %w", uid, err)
	}

	// Committing before removing the spec keeps the ordering of preparing a
	// claim: what we may leave behind is a spec, never an allocation.
	if err := p.owner.ClaimReleased(); err != nil {
		return fmt.Errorf("dra: failed to commit the release of claim %s: %w", uid, err)
	}

	return p.cdi.RemoveClaim(uid)
}

// undoClaim releases a claim we allocated but failed to prepare, unless an
// earlier prepare of the same claim had already succeeded: what the policy
// accounts for then is not only what this failed attempt allocated, and the
// kubelet releases it all when the last pod using the claim goes away.
//
// There is nothing to do about a failure of the release itself, the claim is
// failed already, but it must be visible: it leaves the policy accounting for
// resources no container got.
func (p *Plugin) undoClaim(uid types.UID, rollback bool) {
	if !rollback {
		log.Warnf("claim %s stays allocated: it was prepared before, so its devices may be in use", uid)
		return
	}

	if err := p.releaseClaim(uid); err != nil {
		log.Errorf("failed to release the allocation of claim %s: %v", uid, err)
	}
}

// claimResults returns the allocation results of a claim which are ours. A claim
// can be allocated from several drivers, and the other drivers' devices are
// theirs to prepare, not ours to look at.
func (p *Plugin) claimResults(claim *resourceapi.ResourceClaim) []resourceapi.DeviceRequestAllocationResult {
	if claim.Status.Allocation == nil {
		return nil
	}

	var results []resourceapi.DeviceRequestAllocationResult
	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver == p.driverName {
			results = append(results, r)
		}
	}

	return results
}
