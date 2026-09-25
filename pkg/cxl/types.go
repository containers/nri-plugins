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

package cxl

// Devices represents CXL bus topology and memory nodes.
type Devices struct {
	SysfsPath       string            // Sysfs path to the CXL bus, e.g. /sys/bus/cxl
	MemoryDevices   []*MemoryDevice   // Memory devices on this CXL bus
	RegionDevices   []*RegionDevice   // Region devices on this CXL bus
	EndpointDevices []*EndpointDevice // Endpoint devices on this CXL bus
	MemoryNodes     []*MemoryNode     // System memory node, e.g. /sys/devices/system/node/node0
}

// MemoryDevice represents a CXL memory device.
// See https://www.kernel.org/doc/Documentation/ABI/testing/sysfs-bus-cxl
// for more details.
type MemoryDevice struct {
	SysfsPath       string // Sysfs path to the memory device, e.g. /sys/bus/cxl/devices/mem0
	Name            string // Memory device name, e.g. "mem0"
	DevName         string // uevent DEVNAME, e.g. "cxl/mem0"
	RamSize         uint64 // RAM (volatile memory) size in bytes
	PmemSize        uint64 // PMEM (persistent memory) size in bytes
	FirmwareVersion string // FirmwareVersion string
	Serial          uint64 // 64-bit PCIe device serial number
	NodeAffinity    int    // NUMA node affinity, CPU node on host, -1 if not set
	Driver          string // Driver name from uevent, e.g. "cxl_mem". Empty when memdev is disabled.
	Major           int    // Major device number from uevent
	Minor           int    // Minor device number from uevent
	Enabled         bool   // Whether the memory device is enabled (Driver != "")
}

// RegionDevice represents a memory region device in sysfs.
type RegionDevice struct {
	SysfsPath  string          // Sysfs path to the region device, e.g. /sys/bus/cxl/devices/region0
	Name       string          // Region name, e.g. "region0"
	Size       uint64          // Size in bytes
	Resource   uint64          // Resource start address
	Mode       string          // Mode string, e.g. "ram"
	Node       int             // NUMA node where region memory is located, -1 if region is disabled
	OnlineSize uint64          // Size of onlined memory in this region in bytes (may be 0 even if region is enabled)
	Targets    []string        // Target decoders, e.g. ["decoder4.0", "decoder7.0"]
	Memories   []*MemoryDevice // Memory devices associated with this region
	Enabled    bool            // Whether the region is enabled (Node != -1)
}

// EndpointDevice represents a CXL endpoint device in sysfs.
type EndpointDevice struct {
	SysfsPath    string                    // Sysfs path to the endpoint device, e.g. /sys/bus/cxl/devices/endpoint4
	UportMajor   int                       // Major device number of the uport
	UportMinor   int                       // Minor device number of the uport
	UportDevName string                    // Device name of the uport, e.g. "cxl/mem0"
	Decoders     map[string]*DecoderDevice // Decoders in this endpoint, key is decoder name
}

// MemoryNode represents a NUMA memory node, possibly backed by CXL memory.
type MemoryNode struct {
	SysfsPath string // Sysfs path to the memory node, e.g. /sys/devices/system/node/node0
	ID        int    // Node ID, e.g. 0
	Name      string // Node name, e.g. "node0"
	Size      uint64 // Size of online memory in the node in bytes (MemTotal)
}

// DecoderDevice represents a CXL decoder device in sysfs.
type DecoderDevice struct {
	SysfsPath string // Sysfs path to the decoder device, e.g. /sys/bus/cxl/devices/decoder4.0
	Name      string // Decoder name, e.g. "decoder4.0"
	Mode      string // Decoder mode, e.g. "ram"
	Region    string // Associated region name, e.g. "region0"
}

// ZoneInfo is read from /proc/zoneinfo.
type ZoneInfo struct {
	FilePath      string         // path to contents, e.g. /proc/zoneinfo
	PfnToNode     map[int64]int  // Maps page frame number to NUMA node ID
	NodeToPresent map[int]uint64 // number of pages present per node
}
