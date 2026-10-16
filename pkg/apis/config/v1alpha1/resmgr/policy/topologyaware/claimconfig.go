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
	"fmt"
	"slices"
	"strings"

	nriapi "github.com/containerd/nri/pkg/api"
	policy "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TopologyAwareClaimConfig is the config of a DRA claim or device class for
// the topology-aware policy.
type TopologyAwareClaimConfig struct {
	metav1.TypeMeta `json:",inline"`
	// MemoryPolicy is the memory policy of the containers using the claim.
	// +optional
	MemoryPolicy *ClaimMemoryPolicy `json:"memoryPolicy,omitempty"`
}

// ClaimMemoryPolicy is the memory policy of a claim. It is the same as
// MemoryPolicy, with short names for the mode and the flags.
type ClaimMemoryPolicy struct {
	// Mode is the memory policy mode: default, preferred, bind, interleave,
	// local, preferred-many or weighted-interleave.
	Mode string `json:"mode"`
	// Nodes are the memory nodes of the policy, as in MemoryPolicy.
	// +optional
	Nodes string `json:"nodes,omitempty"`
	// Flags are mode flags: static-nodes, relative-nodes or numa-balancing.
	// +optional
	Flags []string `json:"flags,omitempty"`
}

// ToMemoryPolicy returns the policy with the MPOL_ names of its mode and
// flags.
func (mp *ClaimMemoryPolicy) ToMemoryPolicy() (*policy.MemoryPolicy, error) {
	mode, err := mpolName(mp.Mode, "MPOL_", nriapi.MpolMode_value)
	if err != nil {
		return nil, fmt.Errorf("invalid memory policy mode: %w", err)
	}

	var flags []string
	for _, f := range mp.Flags {
		flag, err := mpolName(f, "MPOL_F_", nriapi.MpolFlag_value)
		if err != nil {
			return nil, fmt.Errorf("invalid memory policy flag: %w", err)
		}
		flags = append(flags, flag)
	}
	// The kernel refuses these with EINVAL.
	nodeFlag := slices.Contains(flags, "MPOL_F_STATIC_NODES") || slices.Contains(flags, "MPOL_F_RELATIVE_NODES")
	switch {
	case slices.Contains(flags, "MPOL_F_STATIC_NODES") && slices.Contains(flags, "MPOL_F_RELATIVE_NODES"):
		return nil, fmt.Errorf("memory policy flags static-nodes and relative-nodes exclude each other")
	case slices.Contains(flags, "MPOL_F_NUMA_BALANCING") && mode != "MPOL_BIND" && mode != "MPOL_PREFERRED_MANY":
		return nil, fmt.Errorf("memory policy flag numa-balancing needs mode bind or preferred-many, got %q", mp.Mode)
	case nodeFlag && mode == "MPOL_LOCAL":
		return nil, fmt.Errorf("memory policy mode local takes no static-nodes or relative-nodes flag")
	// The kernel reads the nodes of a relative-nodes policy as indices into
	// the allowed memory nodes. The names, such as cpu-nodes, give node IDs.
	case slices.Contains(flags, "MPOL_F_RELATIVE_NODES") &&
		(mp.Nodes == "" || mp.Nodes[0] < '0' || mp.Nodes[0] > '9'):
		return nil, fmt.Errorf("memory policy flag relative-nodes needs a list of node indices, got %q", mp.Nodes)
	}

	return &policy.MemoryPolicy{Mode: mode, Nodes: mp.Nodes, Flags: flags}, nil
}

// mpolName returns the MPOL_ name of a short name: interleave is
// MPOL_INTERLEAVE, static-nodes is MPOL_F_STATIC_NODES.
func mpolName(name, prefix string, known map[string]int32) (string, error) {
	long := prefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	if _, ok := known[long]; ok && name == strings.ToLower(name) && !strings.Contains(name, "_") {
		return long, nil
	}

	if _, ok := known[name]; ok {
		short := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, prefix), "_", "-"))
		return "", fmt.Errorf("%q, use %q in claims", name, short)
	}

	return "", fmt.Errorf("unknown %q", name)
}
