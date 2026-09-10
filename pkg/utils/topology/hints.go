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

package topology

import (
	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"github.com/containers/nri-plugins/pkg/lib/hardware"
	libmem "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
	"github.com/containers/nri-plugins/pkg/topology"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

type TopologyHint = topology.Hint

type Hint struct {
	machine *hardware.Machine
	hint    *TopologyHint
}

func NewHint(m *hardware.Machine, h TopologyHint) *Hint {
	return &Hint{
		machine: m,
		hint:    &h,
	}
}

func (h *Hint) CPUSetForCPUs() cpuset.CPUSet {
	cset, _ := cpuset.Parse(h.hint.CPUs)
	return cset
}

func (h *Hint) MemsForCPUs() libmem.NodeMask {
	mems := libmem.NewNodeMask()
	cset, _ := cpuset.Parse(h.hint.CPUs)
	for _, id := range h.machine.MemoryNodeIDs() {
		if h.machine.MemoryNode(id).CPUs().Intersects(toCpuMask(cset)) {
			mems.Set(id)
		}
	}
	return mems
}

func (h *Hint) CPUSetForNUMAs() cpuset.CPUSet {
	cset := cpuset.New()
	mems, _ := cpuset.Parse(h.hint.NUMAs)
	for _, id := range mems.UnsortedList() {
		cset = cset.Union(toCpuSet(h.machine.MemoryNode(id).CPUs()))
	}
	return cset
}

func (h *Hint) MemsForNUMAs() libmem.NodeMask {
	mems, _ := libmem.ParseNodeMask(h.hint.NUMAs)
	return mems
}

func (h *Hint) MisalignedCPUSet(cpus cpuset.CPUSet) cpuset.CPUSet {
	misaligned := cpuset.New()
	if aligned := h.CPUSetForCPUs(); !aligned.IsEmpty() {
		misaligned = misaligned.Union(cpus.Difference(aligned))
	}
	if aligned := h.CPUSetForNUMAs(); !aligned.IsEmpty() {
		misaligned = misaligned.Union(cpus.Difference(aligned))
	}
	return misaligned
}

func (h *Hint) MisalignedMems(mems libmem.NodeMask) libmem.NodeMask {
	misaligned := libmem.NewNodeMask()
	if aligned := h.MemsForCPUs(); aligned.Size() > 0 {
		misaligned = misaligned.Or(mems.AndNot(aligned))
	}
	if aligned := h.MemsForNUMAs(); aligned.Size() > 0 {
		misaligned = misaligned.Or(mems.AndNot(aligned))
	}
	return misaligned
}

// toCpuSet and toCpuMask convert between the set the hardware package speaks and
// the one these hints are expressed in.
func toCpuSet(cpus libcpu.CPUSet) cpuset.CPUSet {
	return cpuset.New(cpus.List()...)
}

func toCpuMask(cpus cpuset.CPUSet) *libcpu.CpuMask {
	return libcpu.NewCpuMask(cpus.List()...)
}
