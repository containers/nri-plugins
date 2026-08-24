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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDRAEnabledZeroConfig(t *testing.T) {
	c := &Config{}
	require.False(t, c.DRAEnabled(), "zero Config (nil DRA) should report DRAEnabled() == false")
	require.False(t, c.DRASharedCounters(), "zero Config (nil DRA) should report DRASharedCounters() == false")
}

func TestDRAEnabledTrue(t *testing.T) {
	c := &Config{
		ReservedResources: Constraints{CPU: "750m"},
		DRA:               &TopologyAwareDRA{Enabled: true},
	}
	require.True(t, c.DRAEnabled())
	require.NoError(t, c.Validate())
}

func TestDRASharedCountersTrue(t *testing.T) {
	c := &Config{
		ReservedResources: Constraints{CPU: "750m"},
		DRA:               &TopologyAwareDRA{SharedCounters: true},
	}
	require.True(t, c.DRASharedCounters())
	require.NoError(t, c.Validate())
}
