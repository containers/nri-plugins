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
	idset "github.com/intel/goresctrl/pkg/utils"
)

// cacheCPUsAtLevel returns the CPUs sharing any of a CPU's caches at one level,
// or its thread siblings when it has no caches at all. The result is for reading
// only: a CPU with no caches is answered with the machine's own sealed set.
//
// Note the union: a level with a separate data and instruction cache contributes
// both, which is why this does not just take hardware.CPU.Cache.
func cacheCPUsAtLevel(m *hardware.Machine, id idset.ID, level int) *libcpu.CpuMask {
	c := m.CPU(id)

	caches := c.Caches()
	if len(caches) == 0 {
		return c.Threads()
	}

	cpus := libcpu.NewCpuMask()
	for _, cache := range caches {
		if cache.Level() == level {
			cpus = cpus.Union(cache.CPUs())
		} else if cache.Level() > level {
			break
		}
	}

	return cpus
}
