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
	"github.com/intel/goresctrl/pkg/sst"
	idset "github.com/intel/goresctrl/pkg/utils"
)

// speedSelect is what Intel Speed Select Technology says about the machine.
//
// This lives here because this package is the only one which asks. Topology
// discovery deliberately knows nothing about SST -- it is a feature of the
// running platform rather than a property of its shape -- so the probing sits
// next to the CPU prioritization which is the only thing that wants it.
//
// A nil *speedSelect means SST told us nothing, either because the platform does
// not support it or because probing failed. Every method tolerates that, so a
// caller does not have to check first.
type speedSelect struct {
	platform *sst.Platform
	status   map[idset.ID]*sst.PackageStatus
	clos     map[idset.ID]int
}

// discoverSpeedSelect probes SST for the given packages. It returns nil when
// there is nothing to report; failing to probe is not an error, it just means
// prioritization has to be decided some other way.
func discoverSpeedSelect(pkgIDs []idset.ID) *speedSelect {
	if !sst.SstSupported() {
		return nil
	}

	platform, err := sst.Init()
	if err != nil || platform == nil {
		log.Debugf("SST is supported but could not be initialized: %v", err)
		return nil
	}

	s := &speedSelect{
		platform: platform,
		status:   map[idset.ID]*sst.PackageStatus{},
		clos:     map[idset.ID]int{},
	}

	for _, id := range pkgIDs {
		pkg, ok := platform.Package(id)
		if !ok {
			continue
		}
		status, err := pkg.GetStatus()
		if err != nil {
			log.Debugf("package #%d: no SST status: %v", id, err)
			continue
		}

		for _, punit := range status.Punits {
			if !punit.CP.Supported || !punit.CP.Enabled {
				continue
			}
			for _, cpu := range punit.CPUs.SortedMembers() {
				clos, err := platform.GetCPUClosID(cpu)
				if err != nil {
					continue
				}
				s.clos[cpu] = clos
			}
		}

		s.status[id] = status
	}

	return s
}

// PackageStatus returns what SST says about a package, or nil if it says nothing.
func (s *speedSelect) PackageStatus(pkgID idset.ID) *sst.PackageStatus {
	if s == nil {
		return nil
	}
	return s.status[pkgID]
}

// Clos returns the SST-CP class of service a CPU is in, or -1 when SST
// prioritization is not in effect for it.
func (s *speedSelect) Clos(cpu idset.ID) int {
	if s == nil {
		return -1
	}
	if clos, ok := s.clos[cpu]; ok {
		return clos
	}
	return -1
}

// Package returns the SST view of a package, for the perf level information
// which only it can answer.
func (s *speedSelect) Package(pkgID idset.ID) (*sst.Package, bool) {
	if s == nil || s.platform == nil {
		return nil, false
	}
	return s.platform.Package(pkgID)
}
