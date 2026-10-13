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

package libmem

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// localDistance is the distance of a NUMA node from itself. The kernel
// refuses firmware tables which give another value, or which put another
// node at the same or a shorter distance.
const localDistance = 10

// MemoryPolicyNodes returns the nodes a memory policy uses for a container
// running on the given CPUs and memory nodes. The mode and the nodes use the
// format of the Mode and Nodes fields of MemoryPolicy in the policy
// configuration.
func MemoryPolicyNodes(sys sysfs.System, mode, nodes string, cpus cpuset.CPUSet, mems NodeMask) (NodeMask, error) {
	cpuNodes := func() idset.IDSet {
		return sys.IDSetForCPUs(cpus, func(cpu sysfs.CPU) idset.ID { return cpu.NodeID() })
	}

	switch {
	// MPOL_DEFAULT and MPOL_LOCAL take no nodes, the kernel refuses any.
	case mode == "MPOL_DEFAULT" || mode == "MPOL_LOCAL":
		if nodes != "" {
			return 0, fmt.Errorf("memory policy mode %s takes no nodes, got %q", mode, nodes)
		}
		return NewNodeMask(), nil

	case nodes == "all":
		return NewNodeMask(sys.NodeIDs()...), nil

	case nodes == "" || nodes == "allowed-mems":
		return mems, nil

	case nodes == "cpu-nodes":
		return NewNodeMask(cpuNodes().Members()...), nil

	case nodes == "cpu-packages":
		pkgs := sys.IDSetForCPUs(cpus, func(cpu sysfs.CPU) idset.ID { return cpu.PackageID() })
		mask := NewNodeMask()
		for _, id := range sys.NodeIDs() {
			if pkgs.Has(sys.Node(id).PackageID()) {
				mask = mask.Set(id)
			}
		}
		return mask, nil

	case strings.HasPrefix(nodes, "max-dist:"):
		maxDist, err := strconv.Atoi(strings.TrimPrefix(nodes, "max-dist:"))
		if err != nil {
			return 0, fmt.Errorf("invalid memory policy nodes %q: %w", nodes, err)
		}
		// A max-dist below localDistance gives no nodes at all. The kernel
		// refuses such a policy for most modes, and the container fails to
		// start.
		if maxDist < localDistance {
			return 0, fmt.Errorf("invalid memory policy nodes %q: distance is below %d", nodes, localDistance)
		}
		mask := NewNodeMask()
		for _, from := range cpuNodes().Members() {
			for _, to := range sys.NodeIDs() {
				if sys.NodeDistance(from, to) <= maxDist {
					mask = mask.Set(to)
				}
			}
		}
		return mask, nil

	case nodes[0] >= '0' && nodes[0] <= '9':
		mask, err := ParseNodeMask(nodes)
		if err != nil {
			return 0, fmt.Errorf("invalid memory policy nodes %q: %w", nodes, err)
		}
		return mask, nil
	}

	return 0, fmt.Errorf("invalid memory policy nodes %q", nodes)
}
