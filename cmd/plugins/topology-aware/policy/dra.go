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

package topologyaware

import (
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	specs "tags.cncf.io/container-device-interface/specs-go"

	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
)

// AllocateClaim allocates resources for a claim being prepared.
func (p *policy) AllocateClaim(
	*resourceapi.ResourceClaim,
	[]resourceapi.DeviceRequestAllocationResult,
) ([]specs.ContainerEdits, error) {
	return nil, policyapi.ErrNoDRAClaims
}

// ReleaseClaim releases the resources allocated for the given claim.
func (p *policy) ReleaseClaim(types.UID) error {
	return policyapi.ErrNoDRAClaims
}
