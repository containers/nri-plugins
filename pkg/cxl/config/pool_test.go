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

package config

import (
	"testing"
)

// refSettings is the object of the fake pool controller.
type refSettings struct {
	Slot     string `json:"slot,omitempty"`
	NUMANode *int   `json:"numaNode,omitempty"`
}

func TestPoolOpaque(t *testing.T) {
	const key = "fake-cxl-pool"
	for _, tc := range []struct {
		name    string
		entries []cfg
		found   bool
		slot    string
		node    int // -1: none
		err     string
	}{
		{name: "no entries", node: -1},
		{name: "no pool section", entries: []cfg{claimed(`"device": {"mode": "dax"}`).of(pool)}, node: -1},
		{
			name:    "other keys only",
			entries: []cfg{claimed(`"pool": {"opaque": {"vendor-x": {"port": 3, "qos": "gold"}}}`).of(pool)},
			node:    -1,
		},
		{
			name: "own key, other keys ignored",
			entries: []cfg{
				class(`"pool": {"opaque": {"fake-cxl-pool": {"slot": "0000:0d:00.0"}, "vendor-x": {"port": 3}}}`, "r").of(pool),
				claimed(`"pool": {"opaque": {"fake-cxl-pool": {"numaNode": 1}, "vendor-y": {"anything": ["is", "fine"]}}}`).of(pool),
			},
			found: true, slot: "0000:0d:00.0", node: 1,
		},
		{
			name:    "merged with null",
			entries: []cfg{class(`"pool": {"opaque": {"fake-cxl-pool": {"slot": "x", "numaNode": 0}}}`, "r").of(pool), claimed(`"pool": {"opaque": {"fake-cxl-pool": {"slot": null}}}`).of(pool)},
			found:   true, node: 0,
		},
		{
			name:    "empty object",
			entries: []cfg{claimed(`"pool": {"opaque": {"fake-cxl-pool": {}}}`).of(pool)},
			found:   true, node: -1,
		},
		{
			name:    "unknown field",
			entries: []cfg{claimed(`"pool": {"opaque": {"fake-cxl-pool": {"slots": ["x"]}}}`).of(pool)},
			found:   true, node: -1,
			err: `MemoryConfig: pool.opaque.fake-cxl-pool: json: unknown field "slots"`,
		},
		{
			name:    "case sensitive",
			entries: []cfg{claimed(`"pool": {"opaque": {"fake-cxl-pool": {"NumaNode": 1}}}`).of(pool)},
			found:   true, node: -1,
			err: `MemoryConfig: pool.opaque.fake-cxl-pool: json: unknown field "NumaNode"`,
		},
		{
			name:    "wrong type",
			entries: []cfg{claimed(`"pool": {"opaque": {"fake-cxl-pool": {"numaNode": "1"}}}`).of(pool)},
			found:   true, node: -1,
			err: `MemoryConfig: pool.opaque.fake-cxl-pool: json: cannot unmarshal string into Go struct field refSettings.numaNode of type int`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Resolve(testClaim(tc.entries...), pool, "r")
			if err != nil {
				t.Fatal(err)
			}
			var out refSettings
			found, err := PoolOpaque(res, key, &out)
			if found != tc.found || !checkErr(err, tc.err) {
				t.Fatalf("found %v %v, expected %v %q", found, err, tc.found, tc.err)
			}
			if tc.err != "" {
				return
			}
			node := -1
			if out.NUMANode != nil {
				node = *out.NUMANode
			}
			if out.Slot != tc.slot || node != tc.node {
				t.Errorf("got %q %d, expected %q %d", out.Slot, node, tc.slot, tc.node)
			}
		})
	}
	if found, err := PoolOpaque(nil, key, &refSettings{}); found || err != nil {
		t.Errorf("nil: %v %v", found, err)
	}
}
