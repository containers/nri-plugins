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

package main

import (
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/sirupsen/logrus"

	policyapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// testSystem is a system with CPUs 0-3 and memory nodes 0-1.
type testSystem struct {
	system.System
}

func (testSystem) OnlineCPUs() cpuset.CPUSet { return cpuset.New(0, 1, 2, 3) }
func (testSystem) NodeIDs() []idset.ID       { return []idset.ID{0, 1} }

func TestToLinuxMemoryPolicyNodes(t *testing.T) {
	sys = testSystem{}
	log = logrus.StandardLogger()

	container := func(mems string) *api.Container {
		return &api.Container{
			Linux: &api.LinuxContainer{
				Resources: &api.LinuxResources{
					Cpu: &api.LinuxCPU{Mems: mems},
				},
			},
		}
	}

	cases := []struct {
		name  string
		spec  policyapi.MemoryPolicy
		mems  string
		nodes string
		fail  bool
	}{
		{"no nodes means allowed-mems", policyapi.MemoryPolicy{Mode: "MPOL_INTERLEAVE"}, "1", "1", false},
		{"no nodes, no cpuset.mems", policyapi.MemoryPolicy{Mode: "MPOL_INTERLEAVE"}, "", "0-1", false},
		{"allowed-mems", policyapi.MemoryPolicy{Mode: "MPOL_BIND", Nodes: "allowed-mems"}, "0", "0", false},
		{"MPOL_LOCAL takes no nodes", policyapi.MemoryPolicy{Mode: "MPOL_LOCAL"}, "1", "", false},
		{"MPOL_DEFAULT takes no nodes", policyapi.MemoryPolicy{Mode: "MPOL_DEFAULT"}, "1", "", false},
		{"MPOL_LOCAL with nodes", policyapi.MemoryPolicy{Mode: "MPOL_LOCAL", Nodes: "0"}, "", "", true},
		{"MPOL_DEFAULT with nodes", policyapi.MemoryPolicy{Mode: "MPOL_DEFAULT", Nodes: "all"}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := toLinuxMemoryPolicy(&tc.spec, container(tc.mems))
			if tc.fail {
				if err == nil {
					t.Errorf("toLinuxMemoryPolicy succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("toLinuxMemoryPolicy failed: %v", err)
			}
			if policy.Nodes != tc.nodes {
				t.Errorf("nodes = %q, want %q", policy.Nodes, tc.nodes)
			}
		})
	}
}
