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

package v1alpha1

// DRAConfig provides configuration data for the DRA (Dynamic Resource
// Allocation) driver. The driver is owned by the resource manager, not by a
// policy, so this is common configuration for all policies.
type DRAConfig struct {
	// Enabled registers a DRA driver for this node and publishes the devices
	// the active policy provides. A policy which provides no devices leaves a
	// registered driver with no devices, which claims cannot be made against.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}
