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

package controller

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// PoolSettings is the object of the controller in pool.opaque.<config key>
// of a MemoryConfig: where the pool server attaches the device.
type PoolSettings struct {
	// Slot is an exact CXL slot (bus) of the host, "" for any.
	Slot string `json:"slot,omitempty"`
	// NUMANode prefers the slots of the host bridges on that NUMA node of
	// the host.
	NUMANode *int `json:"numaNode,omitempty"`
}

// String returns the settings for logs, "" when nothing is set.
func (s PoolSettings) String() string {
	switch {
	case s.Slot != "" && s.NUMANode != nil:
		return fmt.Sprintf("slot %s, NUMA node %d", s.Slot, *s.NUMANode)
	case s.Slot != "":
		return "slot " + s.Slot
	case s.NUMANode != nil:
		return fmt.Sprintf("NUMA node %d", *s.NUMANode)
	}
	return ""
}

// poolSettings returns the settings of the controller for an allocation
// result of the claim, pool.opaque.<config key> of the MemoryConfig of the
// result (zero when absent), and the resolved MemoryConfig of the result
// (nil when it cannot be resolved). It returns an error if a MemoryConfig
// entry of the driver in the claim or the object is invalid.
func (c *Controller) poolSettings(claim *resourceapi.ResourceClaim, r *resourceapi.DeviceRequestAllocationResult) (PoolSettings, *config.Resolved, error) {
	var s PoolSettings
	// every entry of the driver, also of other requests: the node driver
	// refuses the whole claim for any of them
	if err := config.CheckEntries(claim, device.PoolDriverName); err != nil {
		return s, nil, err
	}
	res, err := config.Resolve(claim, device.PoolDriverName, r.Request)
	if err != nil {
		return s, nil, err
	}
	if _, err := config.PoolOpaque(res, c.cfg.ControllerKey, &s); err != nil {
		return PoolSettings{}, res, err
	}
	if s.NUMANode != nil && *s.NUMANode < 0 {
		return PoolSettings{}, res, fmt.Errorf("MemoryConfig: pool.opaque.%s.numaNode: invalid NUMA node %d", c.cfg.ControllerKey, *s.NUMANode)
	}
	return s, res, nil
}
