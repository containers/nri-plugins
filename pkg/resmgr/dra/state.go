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
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
)

// draClaimsKey is the cache policy-entry key under which claim state is stored.
const draClaimsKey = "dra/claims"

// ResultAlloc holds the allocated state for a single DeviceRequestAllocationResult.
// Allocs[i] corresponds to filtered allocation result i; CDI device-name index is
// positional — order must be preserved on rebuild.
type ResultAlloc struct {
	// Request is r.Request from DeviceRequestAllocationResult.
	Request string `json:"Request"`
	// Pool is r.Pool from DeviceRequestAllocationResult.
	Pool string `json:"Pool"`
	// Device is r.Device (the DRA device name) from DeviceRequestAllocationResult.
	Device string `json:"Device"`
	// ShareID is string(*r.ShareID) or "" when r.ShareID is nil.
	ShareID string `json:"ShareID"`
	// ClassName is the value of the nri/cpuClass attribute.
	ClassName string `json:"ClassName"`
	// PkgID is the value of the nri/packageID attribute.
	PkgID int `json:"PkgID"`
	// PunitID is the value of the nri/punitID attribute.
	PunitID int `json:"PunitID"`
	// CPUs is the cpuset.CPUSet.String() representation of the allocated CPUs.
	CPUs string `json:"CPUs"`
}

// ClaimState holds the persisted state for a prepared ResourceClaim.
type ClaimState struct {
	// UID is types.UID as string.
	UID    string        `json:"UID"`
	Allocs []ResultAlloc `json:"Allocs"`
}

// marshalClaims encodes the claims map to a map[string]string (uid → JSON of ClaimState)
// suitable for storage via cache.SetPolicyEntry.
func marshalClaims(claims map[types.UID]*ClaimState) (map[string]string, error) {
	out := make(map[string]string, len(claims))
	for uid, cs := range claims {
		data, err := json.Marshal(cs)
		if err != nil {
			return nil, fmt.Errorf("dra: marshal claim %s: %w", uid, err)
		}
		out[string(uid)] = string(data)
	}
	return out, nil
}

// unmarshalClaims decodes a map[string]string (as stored by marshalClaims) back
// into the claims map.
func unmarshalClaims(raw map[string]string) (map[types.UID]*ClaimState, error) {
	out := make(map[types.UID]*ClaimState, len(raw))
	for uid, data := range raw {
		var cs ClaimState
		if err := json.Unmarshal([]byte(data), &cs); err != nil {
			return nil, fmt.Errorf("dra: unmarshal claim %s: %w", uid, err)
		}
		out[types.UID(uid)] = &cs
	}
	return out, nil
}

// cacheClaimStore implements ClaimStore using the resmgr cache as its backing
// store. Save is a no-op when the cache is in a BlockSave window; the data is
// still in the in-memory policyData map and will persist on the next unblocked
// Save. Callers of Save must tolerate this.
type cacheClaimStore struct {
	c cache.Cache
}

// NewCacheClaimStore returns a ClaimStore backed by the given resmgr cache.
// The dra → cache import is cycle-free (verified).
func NewCacheClaimStore(c cache.Cache) ClaimStore {
	return &cacheClaimStore{c: c}
}

// Save persists the given claims map to the backing cache store.
func (s *cacheClaimStore) Save(claims map[types.UID]*ClaimState) error {
	previous := map[string]string{}
	s.c.GetPolicyEntry(draClaimsKey, &previous)
	m, err := marshalClaims(claims)
	if err != nil {
		return err
	}
	s.c.SetPolicyEntry(draClaimsKey, m)
	if err := s.c.Save(); err != nil {
		s.c.SetPolicyEntry(draClaimsKey, previous)
		return err
	}
	return nil
}

// Load reads the claims map from the backing cache store. Returns nil, nil
// when no claim state has been saved yet.
func (s *cacheClaimStore) Load() (map[types.UID]*ClaimState, error) {
	// Named variable is required — a composite-literal address passed directly
	// to GetPolicyEntry is not addressable and the unmarshal target would be
	// unreachable on a first-load path.
	m := map[string]string{}
	if !s.c.GetPolicyEntry(draClaimsKey, &m) {
		return nil, nil
	}
	return unmarshalClaims(m)
}
