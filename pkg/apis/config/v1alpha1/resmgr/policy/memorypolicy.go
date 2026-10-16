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

package policy

import (
	"fmt"
	"strings"

	nriapi "github.com/containerd/nri/pkg/api"
)

// MemoryPolicy is a Linux memory policy for a container. Refer to
// set_mempolicy(2) for details.
// +k8s:deepcopy-gen=true
// +deepequal-gen=true
type MemoryPolicy struct {
	// Mode is the memory policy mode: MPOL_DEFAULT, MPOL_PREFERRED,
	// MPOL_BIND, MPOL_INTERLEAVE, MPOL_LOCAL, MPOL_PREFERRED_MANY or
	// MPOL_WEIGHTED_INTERLEAVE.
	Mode string `json:"mode"`
	// Nodes are the memory nodes of the policy:
	//   - all: all nodes
	//   - allowed-mems: the memory nodes the container may use
	//   - cpu-nodes: the nodes of the container's CPUs
	//   - cpu-packages: all nodes in the packages of the container's CPUs
	//   - max-dist:<distance>: nodes within the distance of cpu-nodes
	//   - a list of node IDs, such as 0,2-3
	// Unset means allowed-mems.
	// +optional
	Nodes string `json:"nodes,omitempty"`
	// Flags are mode flags: MPOL_F_STATIC_NODES, MPOL_F_RELATIVE_NODES
	// or MPOL_F_NUMA_BALANCING.
	// +optional
	Flags []string `json:"flags,omitempty"`
}

func (mp *MemoryPolicy) String() string {
	if mp == nil {
		return "nil"
	}
	modeFlags := strings.Join(append([]string{mp.Mode}, mp.Flags...), "|")
	return fmt.Sprintf("%s:%s", modeFlags, mp.Nodes)
}

// ToNRI returns the mode and the flags of the policy as NRI values.
func (mp *MemoryPolicy) ToNRI() (nriapi.MpolMode, []nriapi.MpolFlag, error) {
	mode, ok := nriapi.MpolMode_value[mp.Mode]
	if !ok {
		return 0, nil, fmt.Errorf("invalid memory policy mode %q", mp.Mode)
	}

	var flags []nriapi.MpolFlag
	for _, f := range mp.Flags {
		flag, ok := nriapi.MpolFlag_value[f]
		if !ok {
			return 0, nil, fmt.Errorf("invalid memory policy flag %q", f)
		}
		flags = append(flags, nriapi.MpolFlag(flag))
	}

	return nriapi.MpolMode(mode), flags, nil
}
