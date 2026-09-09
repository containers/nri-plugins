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

package cpuallocator

import (
	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"github.com/containers/nri-plugins/pkg/lib/hardware"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// toCpuSet converts a set the hardware package returns to the one this package
// keeps its topology in. It goes away if this package ever switches to libcpu
// sets throughout.
func toCpuSet(cpus libcpu.CPUSet) cpuset.CPUSet {
	return cpuset.New(cpus.List()...)
}

// cacheCPUsAtLevel returns the CPUs sharing any of a CPU's caches at one level,
// or its thread siblings when it has no caches at all.
//
// Note the union: a level with a separate data and instruction cache contributes
// both, which is why this does not just take hardware.CPU.Cache.
func cacheCPUsAtLevel(m *hardware.Machine, id idset.ID, level int) cpuset.CPUSet {
	c := m.CPU(id)

	caches := c.Caches()
	if len(caches) == 0 {
		return toCpuSet(c.Threads())
	}

	cpus := cpuset.New()
	for _, cache := range caches {
		if cache.Level() == level {
			cpus = cpus.Union(toCpuSet(cache.CPUs()))
		} else if cache.Level() > level {
			break
		}
	}

	return cpus
}
