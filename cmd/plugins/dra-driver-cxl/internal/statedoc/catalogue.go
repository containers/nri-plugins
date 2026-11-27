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
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/containers"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// object is a record type or a kind of kernel object, by the name of its
// header comment, with the transition tables of its states.
type object struct {
	Name   string
	Tables []fsm.TableText
}

// stateObjects are the record types of the store, in the order of the
// documentation.
func stateObjects() []object {
	return []object{
		{Name: "Preparation", Tables: []fsm.TableText{state.PreparationTable.Text()}},
		{Name: "Grant", Tables: []fsm.TableText{state.GrantTable.Text()}},
		{Name: "Bind", Tables: []fsm.TableText{state.BindTable.Text()}},
		{Name: "Memdev", Tables: []fsm.TableText{state.MemdevTable.Text()}},
		{Name: "Region", Tables: []fsm.TableText{state.RegionTable.Text(), state.RegionSetUpTable.Text()}},
		{Name: "DaxDevice", Tables: []fsm.TableText{state.DaxDeviceTable.Text()}},
		{Name: "Mount", Tables: []fsm.TableText{state.MountTable.Text()}},
		{Name: "Provenance", Tables: []fsm.TableText{state.ProvenanceTable.Text()}},
		{Name: "Lease", Tables: []fsm.TableText{state.LeaseTable.Text()}},
		{Name: "LeaseClaim", Tables: []fsm.TableText{state.LeaseClaimTable.Text()}},
		{Name: "Steered"},
		{Name: "ListedPool"},
		{Name: "Probe"},
	}
}

// containerObjects are the records of the NRI plugin, held in memory.
func containerObjects() []object {
	return []object{
		{Name: "Container", Tables: []fsm.TableText{containers.LifecycleTable.Text(), containers.ManagementTable.Text()}},
		{Name: "Plugin", Tables: []fsm.TableText{containers.ReadinessTable.Text(), containers.ConnTable.Text()}},
	}
}

// kernelObjects are the kinds of kernel objects, in the order of the
// documentation.
func kernelObjects() []object {
	return []object{
		{Name: "Memdev", Tables: []fsm.TableText{kernel.MemdevTransitions.Text()}},
		{Name: "Port"},
		{Name: "Region", Tables: []fsm.TableText{kernel.RegionTransitions.Text()}},
		{Name: "DaxRegion and DaxDevice", Tables: []fsm.TableText{kernel.DaxTransitions.Text()}},
		{Name: "MemoryBlock", Tables: []fsm.TableText{kernel.BlockTransitions.Text()}},
		{Name: "NUMANode"},
		{Name: "NdRegion, Namespace and BlockDevice",
			Tables: []fsm.TableText{kernel.NdRegionTransitions.Text(), kernel.NamespaceTransitions.Text()}},
		{Name: "Superblock and Mount"},
		{Name: "LoopDevice"},
	}
}
