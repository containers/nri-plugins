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
	resourceapi "k8s.io/api/resource/v1"

	"github.com/containers/nri-plugins/pkg/resmgr/dra"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// DRADriverName is the DRA driver name used to publish and identify CPU
// devices for the topology-aware policy. Defined once here; referenced by
// every other Step 8 source file (adapter, pools, dra.go, Reconfigure).
const DRADriverName = "nri.topology-aware.cpu"

// policyDRAAdapter implements dra.ClaimAllocator and dra.DeviceLister by
// forwarding every call to the *current* p.cpuClasses at call time.
//
// A field holding *cpuclass.Handler directly (captured once, at
// construction) would go stale: initialize() sets p.cpuClasses = nil and
// installs a brand new *cpuclass.Handler on every Reconfigure. Because this
// adapter only ever holds the *policy back-pointer and dereferences
// p.cpuClasses fresh on each call, it always observes the handler that is
// live at call time.
//
// All *cpuclass.Handler methods used here are nil-receiver-safe (they check
// h == nil internally and return zero-values/errors), so no additional nil
// guard is required in the adapter for a nil p.cpuClasses.
type policyDRAAdapter struct {
	p *policy
}

// Make sure policyDRAAdapter implements the interfaces the DRA plugin needs.
var (
	_ dra.ClaimAllocator = &policyDRAAdapter{}
	_ dra.DeviceLister   = &policyDRAAdapter{}
)

// PickHpCpus routes to the current p.cpuClasses handler's PickHpCpus.
func (a *policyDRAAdapter) PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error) {
	return a.p.cpuClasses.PickHpCpus(pkgID, punitID, n, held)
}

// ReleaseHpCpus routes to the current p.cpuClasses handler's ReleaseHpCpus.
func (a *policyDRAAdapter) ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet) {
	a.p.cpuClasses.ReleaseHpCpus(pkgID, punitID, cpus)
}

// AccountHpCpus routes to the current p.cpuClasses handler's AccountHpCpus.
func (a *policyDRAAdapter) AccountHpCpus(pkgID, punitID int, cpus cpuset.CPUSet) error {
	return a.p.cpuClasses.AccountHpCpus(pkgID, punitID, cpus)
}

// IsHPClass routes to the current p.cpuClasses handler's IsHPClass.
func (a *policyDRAAdapter) IsHPClass(className string) bool {
	return a.p.cpuClasses.IsHPClass(className)
}

// DRADevices routes to the current p.cpuClasses handler's DRADevices.
func (a *policyDRAAdapter) DRADevices(driverName string) ([]resourceapi.Device, error) {
	return a.p.cpuClasses.DRADevices(driverName)
}
