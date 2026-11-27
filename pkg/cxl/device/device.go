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

// Package device has the Kubernetes-facing names of the CXL DRA driver:
// driver, device, attribute, capacity, DeviceClass, label and CDI names,
// and the contract between the pool driver and a pool controller.
package device

import (
	"encoding/json"
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

const (
	// CDIClass is the CDI class of the devices of both drivers (CDIKind).
	CDIClass = "device"
	// ClaimsCDISpecName is the CDI spec file of the claims of cxl.nri.io
	// and of its lease claims.
	ClaimsCDISpecName = "cxl-claims.json"
	// PoolCDISpecName is the CDI spec file of the claims of the pool
	// driver.
	PoolCDISpecName = "cxl-pool-claims.json"

	// DriverName is the DRA driver of the devices attached to a node.
	DriverName = "cxl.nri.io"
	// PoolDriverName is the default DRA driver name of the pool driver,
	// which prepares the memdevs of pool devices.
	// See docs/memory/cxl/pool.md.
	PoolDriverName = "cxl-pool.nri.io"

	// DeviceTypeMemdev is a CXL memory device: a slice of a local memdev
	// or a whole pool device per claim.
	DeviceTypeMemdev = "memdev"
	// DeviceTypeRegion is a region that existed before the driver, online
	// as a NUMA node.
	DeviceTypeRegion = "region"
	// DeviceTypeDRAM is the system DRAM of the node.
	DeviceTypeDRAM = "dram"
	// DeviceTypeLease is the device whose claims give containers the
	// lease socket.
	DeviceTypeLease = "lease"

	// Device attributes.
	AttrType            resourcev1.QualifiedName = "type"
	AttrSource          resourcev1.QualifiedName = "source"
	AttrSerial          resourcev1.QualifiedName = "serial"
	AttrShared          resourcev1.QualifiedName = "shared"
	AttrPersistent      resourcev1.QualifiedName = "persistent"
	AttrSize            resourcev1.QualifiedName = "size"
	AttrMemoryBlockSize resourcev1.QualifiedName = "memoryBlockSize"
	// AttrMemoryNUMANode of a region is the NUMA node of its online
	// memory; AttrMemoryNUMANodes of the DRAM device are the NUMA nodes
	// of the DRAM (a cpuset list).
	AttrMemoryNUMANode  resourcev1.QualifiedName = "memoryNUMANode"
	AttrMemoryNUMANodes resourcev1.QualifiedName = "memoryNUMANodes"
	// AttrNUMANode of a memdev, or of a region whose memdevs agree on it,
	// is the CPU NUMA node of the device; absent if unknown. It is the
	// standard attribute: a claim can match it with other drivers' devices.
	AttrNUMANode = deviceattribute.StandardDeviceAttributeNUMANode
	// AttrCoherent of a pool device tells whether the hardware keeps its
	// memory coherent between the hosts that share it; absent is false.
	AttrCoherent resourcev1.QualifiedName = "coherent"

	// CapacityMemory is the memory capacity of a device in bytes.
	// Matches v1.ResourceMemory.
	CapacityMemory resourcev1.QualifiedName = "memory"
	// CapacityShares is the capacity of a shared device, local or pool,
	// published without a requestPolicy: a claim that requests none
	// consumes the whole value (exclusive use), a claim that requests 1
	// is one share.
	CapacityShares resourcev1.QualifiedName = "shares"
	// SharesUnlimited is the shares value of local shared devices and of
	// the fake pool: the most claims that may hold a device at a
	// time, a bound of the model rather than a limit.
	SharesUnlimited = 1000000

	// DeviceClasses of the driver: the names that its Helm chart installs.
	ClassLocal = "cxl-local"
	// ClassLocalShared selects local memdevs that the driver
	// configuration declares shared.
	ClassLocalShared = "cxl-local-shared"
	// ClassLocalPersistent selects local memdevs of persistent memory.
	ClassLocalPersistent = "cxl-local-persistent"
	ClassPool            = "cxl-pool"
	ClassPoolShared      = "cxl-pool-shared"
	// ClassPoolPersistent selects pool devices with persistent memory.
	ClassPoolPersistent = "cxl-pool-persistent"
	ClassRegion         = "cxl-region"
	ClassDRAM           = "cxl-dram"
	ClassLease          = "cxl-lease"

	// LeaseDeviceName is the name of the lease device.
	LeaseDeviceName = "lease"
	// MemdevDevicePrefix is the prefix of the device names of local
	// memdevs ("memdev-" + lower-case hex serial).
	MemdevDevicePrefix = "memdev-"
	// DRAMDeviceName is the name of the DRAM device.
	DRAMDeviceName = "dram"

	// Labels and annotation of the companion claim and pod of a lease.
	LeaseLabel          = "cxl.nri.io/lease"
	RequesterUIDLabel   = "cxl.nri.io/requester-uid"
	NodeLabel           = "cxl.nri.io/node"
	RequesterAnnotation = "cxl.nri.io/requester" // <namespace>/<pod>/<container>
)

// The contract between the pool driver and a pool controller
// (docs/memory/cxl/pool.md): the controller attaches the device of an
// allocation result of the driver to the host of the node of the
// allocation, and writes the condition AttachedCondition and the status
// data into the entry of the result in claim.status.devices, or the
// condition AttachFailedCondition.

// CDIKind returns the CDI kind of the devices of the DRA driver: the
// driver name as the vendor and the class CDIClass, as the resource
// manager names the kinds of its DRA drivers (pkg/resmgr/dra).
func CDIKind(driver string) string {
	return driver + "/" + CDIClass
}

// AttachedCondition returns the condition of a device that the pool
// controller of the driver attached for the claim.
func AttachedCondition(driver string) string {
	return driver + "/Attached"
}

// AttachFailedCondition returns the condition of a device that the pool
// controller of the driver could not attach for the claim.
func AttachFailedCondition(driver string) string {
	return driver + "/AttachFailed"
}

// PoolStatusData is the JSON of the status data of a pool device. Serial,
// shared and size are required: Shared and Size are pointers, so that a
// reader tells a missing key from false or 0. The flags are false when
// missing:
//   - Exclusive: the claim has all shares of a shared device
//   - Persistent: the device has persistent memory
//   - SingleHost: no other host has the device while the claim is
//     allocated
//   - Coherent: the writes of other hosts are visible without cache
//     maintenance
type PoolStatusData struct {
	Serial     string  `json:"serial"`
	Shared     *bool   `json:"shared"`
	Size       *uint64 `json:"size"`
	Exclusive  bool    `json:"exclusive,omitempty"`
	Persistent bool    `json:"persistent,omitempty"`
	SingleHost bool    `json:"singleHost,omitempty"`
	Coherent   bool    `json:"coherent,omitempty"`
}

// PoolDevice is the parsed status data of a pool device.
type PoolDevice struct {
	Serial     uint64
	Shared     bool
	Size       uint64
	Exclusive  bool
	Persistent bool
	SingleHost bool
	Coherent   bool
}

// ParsePoolStatusData returns the pool device of the JSON of its status
// data, an error if the JSON does not parse or a required key is missing.
func ParsePoolStatusData(raw []byte) (*PoolDevice, error) {
	var d PoolStatusData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("invalid device data %s: %w", string(raw), err)
	}
	if d.Serial == "" || d.Shared == nil || d.Size == nil {
		return nil, fmt.Errorf("device data %s: serial, shared and size are required", string(raw))
	}
	serial, err := api.ParseSerial(d.Serial)
	if err != nil {
		return nil, fmt.Errorf("device data: %w", err)
	}
	return &PoolDevice{Serial: serial, Shared: *d.Shared, Size: *d.Size, Exclusive: d.Exclusive, Persistent: d.Persistent,
		SingleHost: d.SingleHost, Coherent: d.Coherent}, nil
}

// PoolStatus returns the pool device of the status data of an allocation
// result of a claim (its entry of StatusOf), an error if the result has no
// entry, or one without data or with data that does not parse.
func PoolStatus(claim *resourcev1.ResourceClaim, r *resourcev1.DeviceRequestAllocationResult) (*PoolDevice, error) {
	st := StatusOf(claim, r)
	if st == nil {
		return nil, fmt.Errorf("claim status has no entry for device %s/%s (not attached by the pool controller yet?)", r.Pool, r.Device)
	}
	if st.Data == nil || len(st.Data.Raw) == 0 {
		return nil, fmt.Errorf("status of device %s/%s has no data (written by the pool controller with the Attached condition)", r.Pool, r.Device)
	}
	return ParsePoolStatusData(st.Data.Raw)
}

// StatusOf returns the entry of claim.status.devices of an allocation
// result, nil if the claim has none. The entry has the driver, the pool and
// the device of the result, and its share id if the result has one.
func StatusOf(claim *resourcev1.ResourceClaim, r *resourcev1.DeviceRequestAllocationResult) *resourcev1.AllocatedDeviceStatus {
	for i := range claim.Status.Devices {
		st := &claim.Status.Devices[i]
		if st.Driver != r.Driver || st.Pool != r.Pool || st.Device != r.Device {
			continue
		}
		if r.ShareID != nil && (st.ShareID == nil || *st.ShareID != string(*r.ShareID)) {
			continue
		}
		return st
	}
	return nil
}

// AllocationNode returns the node of an allocation that binds to one node:
// a node selector of one term with one matchFields value of metadata.name,
// as the scheduler writes for a device of a ResourceSlice of the node. It
// returns "" for another allocation.
func AllocationNode(a *resourcev1.AllocationResult) string {
	ns := a.NodeSelector
	if ns == nil || len(ns.NodeSelectorTerms) != 1 {
		return ""
	}
	for _, f := range ns.NodeSelectorTerms[0].MatchFields {
		if f.Key == "metadata.name" && len(f.Values) == 1 {
			return f.Values[0]
		}
	}
	return ""
}
