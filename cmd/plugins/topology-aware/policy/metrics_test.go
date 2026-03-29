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
	"testing"
)

// TestMetricsUpdateNilReceiver verifies that Update() on a nil
// *TopologyAwareMetrics is a safe no-op.
func TestMetricsUpdateNilReceiver(t *testing.T) {
	var m *TopologyAwareMetrics
	m.Update() // must not panic
}

// TestMetricsUpdateEmptyPools verifies that Update() with a policy that
// has no pools completes without panicking.
func TestMetricsUpdateEmptyPools(t *testing.T) {
	p := &policy{
		pools: nil,
		allocations: allocations{
			grants: make(map[string]Grant),
		},
	}
	m := &TopologyAwareMetrics{
		p:     p,
		Zones: make(map[string]*Zone),
	}
	m.Update() // must not panic
}
