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
	"reflect"
	"strings"
	"testing"

	policy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
)

func TestToMemoryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claim  ClaimMemoryPolicy
		want   *policy.MemoryPolicy
		errMsg string
	}{
		{
			name:  "default",
			claim: ClaimMemoryPolicy{Mode: "default"},
			want:  &policy.MemoryPolicy{Mode: "MPOL_DEFAULT"},
		},
		{
			name:  "preferred",
			claim: ClaimMemoryPolicy{Mode: "preferred", Nodes: "cpu-nodes"},
			want:  &policy.MemoryPolicy{Mode: "MPOL_PREFERRED", Nodes: "cpu-nodes"},
		},
		{
			name:  "bind",
			claim: ClaimMemoryPolicy{Mode: "bind", Nodes: "0,2-3", Flags: []string{"static-nodes"}},
			want:  &policy.MemoryPolicy{Mode: "MPOL_BIND", Nodes: "0,2-3", Flags: []string{"MPOL_F_STATIC_NODES"}},
		},
		{
			name:  "interleave with flags",
			claim: ClaimMemoryPolicy{Mode: "interleave", Nodes: "0-1", Flags: []string{"relative-nodes"}},
			want:  &policy.MemoryPolicy{Mode: "MPOL_INTERLEAVE", Nodes: "0-1", Flags: []string{"MPOL_F_RELATIVE_NODES"}},
		},
		{
			name:  "bind with numa-balancing",
			claim: ClaimMemoryPolicy{Mode: "bind", Nodes: "0", Flags: []string{"relative-nodes", "numa-balancing"}},
			want: &policy.MemoryPolicy{Mode: "MPOL_BIND", Nodes: "0",
				Flags: []string{"MPOL_F_RELATIVE_NODES", "MPOL_F_NUMA_BALANCING"}},
		},
		{
			name:  "preferred-many with numa-balancing",
			claim: ClaimMemoryPolicy{Mode: "preferred-many", Flags: []string{"numa-balancing"}},
			want:  &policy.MemoryPolicy{Mode: "MPOL_PREFERRED_MANY", Flags: []string{"MPOL_F_NUMA_BALANCING"}},
		},
		{
			name:  "local",
			claim: ClaimMemoryPolicy{Mode: "local"},
			want:  &policy.MemoryPolicy{Mode: "MPOL_LOCAL"},
		},
		{
			name:  "preferred-many",
			claim: ClaimMemoryPolicy{Mode: "preferred-many", Nodes: "max-dist:21"},
			want:  &policy.MemoryPolicy{Mode: "MPOL_PREFERRED_MANY", Nodes: "max-dist:21"},
		},
		{
			name:  "weighted-interleave",
			claim: ClaimMemoryPolicy{Mode: "weighted-interleave", Nodes: "allowed-mems"},
			want:  &policy.MemoryPolicy{Mode: "MPOL_WEIGHTED_INTERLEAVE", Nodes: "allowed-mems"},
		},
		{
			name:   "MPOL_ mode",
			claim:  ClaimMemoryPolicy{Mode: "MPOL_PREFERRED_MANY"},
			errMsg: `"MPOL_PREFERRED_MANY", use "preferred-many" in claims`,
		},
		{
			name:   "MPOL_ flag",
			claim:  ClaimMemoryPolicy{Mode: "bind", Flags: []string{"MPOL_F_STATIC_NODES"}},
			errMsg: `"MPOL_F_STATIC_NODES", use "static-nodes" in claims`,
		},
		{
			name:   "upper case mode",
			claim:  ClaimMemoryPolicy{Mode: "Interleave"},
			errMsg: `unknown "Interleave"`,
		},
		{
			name:   "underscore in mode",
			claim:  ClaimMemoryPolicy{Mode: "preferred_many"},
			errMsg: `unknown "preferred_many"`,
		},
		{
			name:   "unknown mode",
			claim:  ClaimMemoryPolicy{Mode: "spread"},
			errMsg: `invalid memory policy mode: unknown "spread"`,
		},
		{
			name:   "no mode",
			claim:  ClaimMemoryPolicy{Nodes: "all"},
			errMsg: `invalid memory policy mode: unknown ""`,
		},
		{
			name:   "unknown flag",
			claim:  ClaimMemoryPolicy{Mode: "bind", Flags: []string{"static"}},
			errMsg: `invalid memory policy flag: unknown "static"`,
		},
		{
			name:   "static and relative nodes",
			claim:  ClaimMemoryPolicy{Mode: "bind", Nodes: "0", Flags: []string{"static-nodes", "relative-nodes"}},
			errMsg: "static-nodes and relative-nodes exclude each other",
		},
		{
			name:   "interleave with numa-balancing",
			claim:  ClaimMemoryPolicy{Mode: "interleave", Flags: []string{"numa-balancing"}},
			errMsg: `numa-balancing needs mode bind or preferred-many, got "interleave"`,
		},
		{
			name:   "relative-nodes with named nodes",
			claim:  ClaimMemoryPolicy{Mode: "bind", Nodes: "cpu-nodes", Flags: []string{"relative-nodes"}},
			errMsg: `relative-nodes needs a list of node indices, got "cpu-nodes"`,
		},
		{
			name:   "relative-nodes with allowed-mems",
			claim:  ClaimMemoryPolicy{Mode: "bind", Flags: []string{"relative-nodes"}},
			errMsg: `relative-nodes needs a list of node indices, got ""`,
		},
		{
			name:   "local with static-nodes",
			claim:  ClaimMemoryPolicy{Mode: "local", Flags: []string{"static-nodes"}},
			errMsg: "mode local takes no static-nodes or relative-nodes flag",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.claim.ToMemoryPolicy()
			if tc.errMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("got policy %v and error %v, expected error %q", got, err, tc.errMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got policy %+v, expected %+v", got, tc.want)
			}
		})
	}
}
