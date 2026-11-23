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

package state

import (
	"github.com/containers/nri-plugins/pkg/cxl/config"
)

// DeviceInfo is one allocation result of a prepared claim of either
// driver with what it got, derived from the records: what the CDI spec,
// the lease broker and the NRI plugin need of it.
type DeviceInfo struct {
	RequestName string
	DeviceName  string // device name in the pool
	// Result is the index of the allocation result of the device in the
	// claim, the n of its CDI device claim-<uid>-<n>.
	Result int
	// DeviceType is device.DeviceTypeMemdev, DeviceTypeRegion,
	// DeviceTypeDRAM or DeviceTypeLease.
	DeviceType string
	// Mode is the api.Mode of a memory device, "" for the lease device.
	Mode           string
	SysfsPath      string // sysfs path of the CXL region
	NUMANodes      []int  // NUMA node(s) for this device
	NUMAAffinities []int  // CPU NUMA affinities of backing memdevs (local memdevs only)
	TotalBytes     uint64 // total device memory capacity (memdev: the size of the grant)
	ConsumedBytes  int64  // allocated capacity from ConsumedCapacity

	// Serial is the serial of a memdev.
	Serial Serial
	// Region is the name of the region of a region device (region0).
	Region string
	// DaxDevice is /dev/daxX.Y of mode dax, "" when the device is gone.
	DaxDevice string
	DaxMajor  int
	DaxMinor  int
	ReadOnly  bool // dax, block: the device node is read-only; fs: the filesystem
	// Shared is true for a shared memdev.
	Shared bool
	// Persistent is true for a memdev in mode block or fs: BlockDevice is
	// its block device (/dev/pmemK) with its numbers; fs: Access is the
	// effective access, Path the directory in the containers, NodeMount
	// the node mount of the filesystem of type FSType.
	Persistent  bool
	BlockDevice string
	BlockMajor  int
	BlockMinor  int
	Access      string
	Path        string
	NodeMount   string
	FSType      string
	// ResultSettings are the container settings of the resolved
	// MemoryConfig of the result and their origins.
	config.ResultSettings
}
