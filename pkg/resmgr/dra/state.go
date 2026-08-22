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
	UID    string       `json:"UID"`
	Allocs []ResultAlloc `json:"Allocs"`
}
