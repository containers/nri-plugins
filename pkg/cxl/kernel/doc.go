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

// Package kernel observes and changes the CXL kernel objects of a node:
// memdevs, the decoder stacks of their ports, regions, dax regions and dax
// devices, memory blocks and NUMA nodes, the persistent memory chain (nd
// region, namespaces, block devices), and the superblocks, mounts, binds and
// loop devices on those block devices.
//
// Observe returns a Snapshot of all of them with a typed state per object.
// The package has one definition of each derived fact:
//
//   - a region is hollow when it is not committed and has no target decoder
//     set (RegionHollow);
//   - a region is enabled when the cxl_region driver is bound to it
//     (RegionEnabled), disabled otherwise;
//   - a memory block is online when its state is not "offline"
//     (BlockOnline); a region or a dax device has online memory when one of
//     the blocks wholly in its memory is online;
//   - the NUMA node of a region is the target_node of a dax device of the
//     region with memory, else the node of a memory block of the region
//     (RegionInfo.NUMANode).
//
// The operations of Kernel (EnableMemdev, CreateRegion, DestroyRegionInOrder,
// BindDax, OnlineBlocks, CreateFsdaxNamespace, Mount, ...) return typed errors
// (errors.go). The package never runs daxctl and never writes the new_id of
// kmem: dax devices are bound through the sysfs attributes of the dax bus,
// and the id of a device is removed from its driver after every bind.
//
// Waits are polls with a deadline, woken early by kernel events (Events).
//
// A Kernel holds configuration only, and every call reads sysfs afresh. The
// package has one lock, daxBus (dax.go): it orders the operations of the
// dax bus that the kernel could deadlock on. The caller serializes the
// other kernel operations of a process.
//
// Names: the observation of an object that the driver also keeps a record of
// has the suffix Info (MemdevInfo, RegionInfo, DaxDeviceInfo, MountInfo);
// the other objects keep the kernel's words (Port, DaxRegion, MemoryBlock,
// NUMANode, NdRegion, Namespace, BlockDevice, Superblock, LoopDevice).
//
// Each object file starts with the object's purpose, identity, states, the
// transitions of its states (a Table next to the type), its invariants, and
// its waits. The fake of the kernel's side for tests is in the package
// kernel/fake.
package kernel
