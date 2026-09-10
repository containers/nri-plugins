// Copyright 2019 Intel Corporation. All Rights Reserved.
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
	"strconv"
	"strings"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"github.com/containers/nri-plugins/pkg/lib/hardware"
	"github.com/containers/nri-plugins/pkg/topology"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// Calculate the hint score of the given hint and CPUSet.
func cpuHintScore(hint topology.Hint, CPUs *libcpu.CpuMask) float64 {
	hCPUs, err := libcpu.ParseCpuMask(hint.CPUs)
	if err != nil {
		log.Warnf("invalid hint CPUs '%s' from %s", hint.CPUs, hint.Provider)
		return 0.0
	}
	common := hCPUs.Intersection(CPUs)
	return float64(common.Size()) / float64(hCPUs.Size())
}

// Calculate the NUMA node score of the given hint and NUMA node.
func numaHintScore(hint topology.Hint, sysIDs ...idset.ID) float64 {
	for idstr := range strings.SplitSeq(hint.NUMAs, ",") {
		hID, err := strconv.ParseInt(idstr, 0, 0)
		if err != nil {
			log.Warnf("invalid hint NUMA node %s from %s", idstr, hint.Provider)
			return 0.0
		}

		for _, id := range sysIDs {
			if hID == int64(id) {
				return 1.0
			}
		}
	}

	return 0.0
}

// Calculate the die node score of the given hint and die.
func dieHintScore(hint topology.Hint, m *hardware.Machine, pkg, die idset.ID) float64 {
	numaNodes := idset.NewIDSet(dieNodeIDs(m, pkg, die)...)

	for idstr := range strings.SplitSeq(hint.NUMAs, ",") {
		hID, err := strconv.ParseInt(idstr, 0, 0)
		if err != nil {
			log.Warnf("invalid hint NUMA node %s from %s", idstr, hint.Provider)
			return 0.0
		}

		if numaNodes.Has(idset.ID(hID)) {
			return 1.0
		}
	}

	return 0.0
}

// Calculate the socket node score of the given hint and NUMA node.
func socketHintScore(hint topology.Hint, sysID idset.ID) float64 {
	for idstr := range strings.SplitSeq(hint.Sockets, ",") {
		id, err := strconv.ParseInt(idstr, 0, 0)
		if err != nil {
			log.Warnf("invalid hint socket '%s' from %s", idstr, hint.Provider)
			return 0.0
		}
		if id == int64(sysID) {
			return 1.0
		}
	}

	return 0.0
}

// return the cpuset for the CPU, NUMA or socket hints, preferred in this particular order.
func (cs *supply) hintCpus(h topology.Hint) *libcpu.CpuMask {
	cpus := libcpu.NewCpuMask()

	switch {
	case h.CPUs != "":
		cpus = libcpu.MustParseCpuMask(h.CPUs)

	case h.NUMAs != "":
		for idstr := range strings.SplitSeq(h.NUMAs, ",") {
			if id, err := strconv.ParseInt(idstr, 0, 0); err == nil {
				if node := cs.node.Machine().MemoryNode(idset.ID(id)); node.Valid() {
					cpus = cpus.Union(node.CPUs())
				}
			}
		}

	case h.Sockets != "":
		for idstr := range strings.SplitSeq(h.Sockets, ",") {
			if id, err := strconv.ParseInt(idstr, 0, 0); err == nil {
				if pkg := packageZone(cs.node.Machine(), idset.ID(id)); pkg != nil {
					cpus = cpus.Union(pkg.CPUs())
				}
			}
		}
	}

	return cpus
}
