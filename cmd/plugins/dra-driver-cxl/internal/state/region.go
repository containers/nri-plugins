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
	"slices"
	"strings"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Region is the record of a CXL region on the node: one of ours, or a
// foreign one that the driver observes and never touches. A region of ours
// owns the dax devices and node mounts that grants use; each lists its
// users. A memdev may have several regions, and a region several target
// memdevs.
//
// Identity: Name (regionN), or NotedName while the region is being
// created (RegionRef).
//
// States: RegionState. NONE: no record. NOTED: the name that cxl
// create-region will give is committed before it runs. OWNED: ours,
// enabled. KEPT: ours, disabled, kept
// below newer regions of another memdev (its release would leave a decoder
// hole) or for a claim whose Prepare did not finish. HOLLOW_OWNED: ours,
// hollow (its memdev went or create-region was cut), deleted when its
// blocks are offline. FOREIGN: not ours, on a memdev under the driver's
// care, named by no record of ours; recorded when a Prepare refuses the
// memdev for it and by the recovery, removed once it is gone; never
// touched. SetUp: the first dax device
// (or the nd region) of the region is made; false again while KEPT.
//
// Transitions: RegionTable and RegionSetUpTable below; a transition not in
// them is refused.
//
// Invariants: a region of ours is exactly one record, in state OWNED, KEPT
// or HOLLOW_OWNED after a Prepare, referenced by every memdev of Targets.
//
// Persistence: intent (NotedName before create-region) and fact. A load in
// another boot drops the record: region names are given anew at every
// boot.
//
// Locks and waits: create, enable, disable, destroy and delete run under
// the engine lock, with the note committed before the command.
type Region struct {
	Name      string      `json:"name,omitempty"`
	NotedName string      `json:"notedName,omitempty"`
	Mode      RegionMode  `json:"mode"`
	Targets   []Serial    `json:"targets"`
	Owner     RegionOwner `json:"owner"`
	State     RegionState `json:"state"`
	SetUp     bool        `json:"setUp,omitempty"`
	// Zeroed is a region whose memory was zeroed once (a new shared
	// local region).
	Zeroed bool `json:"zeroed,omitempty"`
	// NUMANode is the NUMA node of the memory of the region.
	NUMANode   *int        `json:"numaNode,omitempty"`
	Size       uint64      `json:"size,omitempty"`
	DaxDevices []DaxDevice `json:"daxDevices,omitempty"`
	Pmem       *Pmem       `json:"pmem,omitempty"`
	Mounts     []Mount     `json:"mounts,omitempty"`
}

// RegionMode is the mode of a region.
type RegionMode string

const (
	RegionRAM  RegionMode = "ram"
	RegionPmem RegionMode = "pmem"
)

// RegionOwner tells whether a region is the driver's.
type RegionOwner string

const (
	OwnerOurs    RegionOwner = "ours"
	OwnerForeign RegionOwner = "foreign"
)

// RegionState is the state of a Region.
type RegionState string

const (
	RegionNone        RegionState = "NONE"
	RegionNoted       RegionState = "NOTED"
	RegionOwned       RegionState = "OWNED"
	RegionKept        RegionState = "KEPT"
	RegionHollowOwned RegionState = "HOLLOW_OWNED"
	RegionForeign     RegionState = "FOREIGN"
)

// Pmem is the nvdimm side of a pmem region.
type Pmem struct {
	NdRegion    string       `json:"ndRegion,omitempty"`
	Namespace   string       `json:"namespace,omitempty"`
	BlockDevice *BlockDevice `json:"blockDevice,omitempty"`
}

// DaxDevice is a dax device of a region of ours with the grants that use
// it: one user for a slice of an unshared memdev, every grant of the memdev
// for the Whole device of a shared memdev, more for a lease that joins a
// claim's device.
//
// Identity: Name (daxN.M) in its region.
//
// States: DaxDeviceState. NONE: no device. CREATING: dax_region/create
// written, the name read back. UNBOUND: sized, unbound. DEVICE_DAX: bound to
// device_dax. KMEM_OFFLINE, KMEM_ONLINE: bound to kmem, its memory blocks
// offline or online (movable). SEED: daxN.0 at size 0, which the kernel
// never deletes.
//
// Transitions: DaxDeviceTable below; a transition not in it is refused.
//
// Invariants: a device that is not the seed and not the Whole device of a
// kept region has a user that is REALISED or RELEASING after a Prepare; the
// recovery deletes the others.
//
// Persistence: fact; the kernel gives the name after the creation, so it
// is not part of an intent.
//
// Locks and waits: every dax bus write under the engine lock; the memory
// block waits outside it.
type DaxDevice struct {
	Name   string         `json:"name"`
	Dev    DevNumbers     `json:"dev,omitzero"`
	Size   uint64         `json:"size,omitempty"`
	Driver DaxDriver      `json:"driver,omitempty"`
	State  DaxDeviceState `json:"state"`
	Users  []GrantRef     `json:"users,omitempty"`
	// Whole is the device of the whole region (the seed of the region
	// of an unshared memdev, the shared device of a shared memdev).
	Whole bool `json:"whole,omitempty"`
}

// DaxDriver is the driver of a dax device, "" for none.
type DaxDriver string

const (
	DriverDeviceDax DaxDriver = "device_dax"
	DriverKmem      DaxDriver = "kmem"
)

// DaxDeviceState is the state of a DaxDevice.
type DaxDeviceState string

const (
	DaxNone        DaxDeviceState = "NONE"
	DaxCreating    DaxDeviceState = "CREATING"
	DaxUnbound     DaxDeviceState = "UNBOUND"
	DaxDeviceDax   DaxDeviceState = "DEVICE_DAX"
	DaxKmemOffline DaxDeviceState = "KMEM_OFFLINE"
	DaxKmemOnline  DaxDeviceState = "KMEM_ONLINE"
	DaxSeed        DaxDeviceState = "SEED"
)

// Mount is a node mount of the filesystem of a pmem region with one
// access, and the grants that use it.
//
// Identity: the region and Access; Path is <data dir of the grant's source
// driver>/mounts/<serial hex>-<access>.
//
// States: MountState. NONE: not mounted. MOUNTED_RO, MOUNTED_RW: mounted,
// read-write while a writer grant uses it. FOREIGN: something else is
// mounted at the path; never used or unmounted. UNRESTORED: at a start the
// mount of a REALISED grant could not be restored. An unmount that fails
// busy fails the release, the mount stays as it is: kubelet retries.
//
// Transitions: MountTable below; a transition not in it is refused.
//
// Invariants: the block device is read-only iff no writer grant uses the
// region.
//
// Persistence: intent (the path in a grant's Intent before the mount) and
// fact.
//
// Locks and waits: mounts under the engine lock; the holder wait of a
// release outside it.
type Mount struct {
	Access Access     `json:"access"`
	Path   string     `json:"path"`
	State  MountState `json:"state"`
	// Loop is the loop device of a cached mount.
	Loop   string     `json:"loop,omitempty"`
	FSType string     `json:"fsType,omitempty"`
	Users  []GrantRef `json:"users,omitempty"`
}

// MountState is the state of a Mount.
type MountState string

const (
	MountNone       MountState = "NONE"
	MountedRO       MountState = "MOUNTED_RO"
	MountedRW       MountState = "MOUNTED_RW"
	MountForeign    MountState = "FOREIGN"
	MountUnrestored MountState = "UNRESTORED"
)

// regionStored are the states of a region record in the store.
var regionStored = fsm.List(RegionNoted, RegionOwned, RegionKept, RegionHollowOwned, RegionForeign)

// RegionTable is the transition table of Region.State.
var RegionTable = fsm.Table[RegionState]{
	Object: "Region",
	Field:  "State",
	States: fsm.List(RegionNone, RegionNoted, RegionOwned, RegionKept, RegionHollowOwned, RegionForeign),
	Rows: []fsm.Transition[RegionState]{
		{
			From: fsm.List(RegionNone), To: RegionNoted, Triggers: fsm.List(TriggerPrepare),
			When:    "a Prepare needs a region on the memdev",
			Guard:   "engine lock held; the next name read from the root decoder",
			Effects: "commit (d) of NotedName before cxl create-region",
		},
		{
			From: fsm.List(RegionNoted), To: RegionOwned, Triggers: fsm.List(TriggerPrepare),
			When:    "cxl create-region returned (engine lock held for the command); the region's targets are this memdev",
			Guard:   "the name is NotedName, or the created name read again",
			Effects: "commit (d) of Name and Mode",
		},
		{
			From: fsm.List(RegionNoted), To: RegionNone, Triggers: fsm.List(TriggerPrepare, TriggerRecover),
			When: "create-region failed with nothing created (decoder hole; endpoint not ready: retried once after " +
				"3 s); or the recovery finds no region of the noted name",
			Guard:   "no region of the noted name exists",
			Effects: "commit (d) clearing the note",
		},
		{
			From: fsm.List(RegionNoted), To: RegionOwned, Triggers: fsm.List(TriggerRecover),
			When:    "the recovery finds a region of the noted name whose targets are this memdev",
			Effects: "commit (d) of Name",
		},
		{
			From: fsm.List(RegionNoted), To: RegionHollowOwned, Triggers: fsm.List(TriggerRecover),
			When:    "the recovery finds a hollow region of the noted name",
			Effects: "the memdev's Hollow gets the name; commit (l)",
		},
		{
			From: fsm.List(RegionOwned), To: RegionKept, Triggers: releasers,
			When: "a release with Policy \"in order\" found a newer region of another memdev above after cxl " +
				"disable-region; or the recovery keeps a cut Prepare's region untouched below a newer region (Policy keep) " +
				"and no grant of it was REALISED; or a Prepare or an idle keep finds an OWNED region that the kernel has disabled (a release failed " +
				"after cxl disable-region): recorded KEPT, enabled and set up again by the Prepare",
			Effects: "the region stays disabled (in order) or as it is (recovery); SetUp false; commit (d)",
		},
		{
			From: fsm.List(RegionKept), To: RegionOwned, Triggers: fsm.List(TriggerPrepare),
			When:    "a new grant needs the region",
			Guard:   "enable-region succeeded",
			Effects: "commit (d); set up again (SetUp true after the first device)",
		},
		{
			From: fsm.List(RegionKept), To: RegionNone, Triggers: releasers,
			When:    "a release with Policy destroy (top of the stack now; an Unprepare of local; pool)",
			Guard:   "blocks offline",
			Effects: "cxl destroy-region; commit (d)",
		},
		{
			From: fsm.List(RegionOwned), To: RegionNone, Triggers: releasers,
			When:  "the last user released and Policy says destroy; or the recovery finds the region missing",
			Guard: "blocks offline",
			Effects: "disable and destroy under the engine lock (the in-order check repeated after the disable); " +
				"commit (d); a missing region: the record cleared, the grants kept for kubelet",
		},
		{
			From: fsm.List(RegionOwned, RegionKept), To: RegionHollowOwned, Triggers: fsm.List(TriggerObserve, TriggerRecover),
			When:    "the memdev was removed under the region",
			Effects: "the memdev's Hollow gets the name; commit (l)",
		},
		{
			From: fsm.List(RegionHollowOwned), To: RegionNone, Triggers: fsm.List(TriggerUnprepare, TriggerPrepare, TriggerRecover),
			When:    "the hollow region deleted",
			Guard:   "every block of its range offline",
			Effects: "delete_region written under the engine lock; commit (d)",
		},
		{
			From: fsm.List(RegionNone), To: RegionForeign, Triggers: fsm.List(TriggerPrepare, TriggerRecover),
			When: "a Prepare finds a region that no record names on its memdev, or the recovery on a memdev of " +
				"a record",
			Effects: "commit (d) of the record (Owner foreign, Targets from the kernel); the Prepare refused, " +
				"its error naming the region; never touched",
		},
		{
			From: fsm.List(RegionForeign), To: RegionNone, Triggers: fsm.List(TriggerPrepare, TriggerUnprepare, TriggerRecover),
			When: "a Prepare, an Unprepare or the recovery finds the region gone or on other targets, or a Prepare " +
				"creates a region of its name",
			Effects: "commit (d) removing the record",
		},
		{
			From: regionStored, To: RegionNone, Triggers: fsm.List(TriggerLoad),
			When:    "the document is of another boot",
			Effects: "the record dropped: region names are given anew at every boot",
		},
	},
	Refused: []fsm.Refusal[RegionState]{
		{From: fsm.List(RegionNone), To: fsm.List(RegionOwned), Why: "no creation without the note"},
		{
			From: fsm.List(RegionForeign),
			To:   fsm.List(RegionNoted, RegionOwned, RegionKept, RegionHollowOwned),
			Why:  "a foreign region never becomes ours",
		},
	},
}

// RegionSetUpTable is the transition table of Region.SetUp.
var RegionSetUpTable = fsm.Table[bool]{
	Object: "Region",
	Field:  "SetUp",
	States: fsm.List(false, true),
	Rows: []fsm.Transition[bool]{
		{
			From: fsm.List(false), To: true, Triggers: fsm.List(TriggerPrepare),
			When: "the Whole or first dax device made (local shared: zeroed; pool numa: kmem; pool dax: " +
				"device_dax), or the nd region settled (pmem)",
			Guard:   "the region is OWNED",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(true), To: false, Triggers: releasers,
			When:    "the region becomes KEPT (disabled)",
			Effects: "commit (d) with the KEPT state",
		},
	},
}

// daxBound are the states of a dax device bound to a driver.
var daxBound = fsm.List(DaxDeviceDax, DaxKmemOffline, DaxKmemOnline)

// DaxDeviceTable is the transition table of DaxDevice.State.
var DaxDeviceTable = fsm.Table[DaxDeviceState]{
	Object: "DaxDevice",
	Field:  "State",
	States: fsm.List(DaxNone, DaxCreating, DaxUnbound, DaxDeviceDax, DaxKmemOffline, DaxKmemOnline, DaxSeed),
	Rows: []fsm.Transition[DaxDeviceState]{
		{
			From: fsm.List(DaxNone), To: DaxCreating, Triggers: fsm.List(TriggerPrepare),
			When:    "a grant needs a dax device (or the Whole device at set-up)",
			Guard:   "engine lock held",
			Effects: "dax_region/create written; the name read back",
		},
		{
			From: fsm.List(DaxCreating), To: DaxUnbound, Triggers: fsm.List(TriggerPrepare),
			When: "size (or explicit ranges) written and verified",
			Guard: "room in the dax region; whole-block placement verified (created again through the mapping " +
				"attribute if not)",
			Effects: "commit (d) of name, dev and ranges; Users the grant",
		},
		{
			From: fsm.List(DaxCreating), To: DaxNone, Triggers: fsm.List(TriggerPrepare, TriggerUnprepare),
			When: "size or ranges failed, or the verification failed; or the Unprepare of a claim whose Prepare did not " +
				"finish (PREPARING or KEPT, Preparation PREPARING -> NONE) deletes the device its cut Prepare was making",
			Effects: "deleted; error",
		},
		{
			From: fsm.List(DaxNone), To: DaxKmemOffline, Triggers: fsm.List(TriggerPrepare),
			When:    "the first device (daxN.0) of a new ram region appeared, bound to kmem by the kernel",
			Effects: "its blocks offlined and held offline; commit (d)",
		},
		{
			From: fsm.List(DaxNone), To: DaxDeviceDax, Triggers: fsm.List(TriggerPrepare),
			When: "the first device (daxN.0) of a new ram region appeared bound to device_dax (an id of its name " +
				"that another tool left in device_dax)",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(DaxKmemOffline), To: DaxUnbound, Triggers: fsm.List(TriggerPrepare),
			When:    "kmem unbound from the offline device (the set-up of a region for device_dax or for slices)",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(DaxDeviceDax), To: DaxUnbound, Triggers: fsm.List(TriggerPrepare),
			When: "the unused Whole device daxN.0 of the region of an unshared memdev resized for the first slice of a grant (unbound " +
				"by the resize), or unbound from device_dax for a switch to kmem",
			Guard:   "no user",
			Effects: "commit (d); a slice: Users the grant, no longer the Whole device",
		},
		{
			From: fsm.List(DaxUnbound, DaxDeviceDax, DaxKmemOffline, DaxKmemOnline), To: DaxSeed, Triggers: releasers,
			When: "daxN.0 resized to 0: the seed of the region of an unshared memdev; or daxN.0 released with Users empty and not " +
				"kept: the kernel keeps it at size 0",
			Effects: "kmem: blocks offlined (EBUSY: stays, error up); unbound; resized to 0; commit (d)",
		},
		{
			From: fsm.List(DaxSeed), To: DaxUnbound, Triggers: fsm.List(TriggerPrepare),
			When:    "the seed sized again (the Whole device of a region set up again)",
			Guard:   "room in the dax region",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(DaxUnbound), To: DaxDeviceDax, Triggers: fsm.List(TriggerPrepare),
			When:    "bound to device_dax by sysfs",
			Effects: "zeroed (a local dax grant, a new shared region); commit (d)",
		},
		{
			From: fsm.List(DaxUnbound), To: DaxKmemOffline, Triggers: fsm.List(TriggerPrepare),
			When:    "bound to kmem by sysfs bind (never new_id)",
			Guard:   "fenced before its blocks go online (KMEM_OFFLINE -> KMEM_ONLINE)",
			Effects: "wait for the blocks; commit (d)",
		},
		{
			From: fsm.List(DaxKmemOffline), To: DaxKmemOnline, Triggers: fsm.List(TriggerPrepare),
			When:    "the blocks onlined movable",
			Guard:   "all blocks exist; the udev online settled",
			Effects: "commit (d); the NUMA node's MemTotal grows",
		},
		{
			From: daxBound, Stays: true, Triggers: releasers,
			When: "a second grant uses the device (a shared device, a shared slice, a lease joining), or a user " +
				"grant RELEASING",
			Guard:   "a new user: the device is shareable for that kind",
			Effects: "Users gets the grant or loses it; commit (d)",
		},
		{
			From: fsm.List(DaxUnbound, DaxDeviceDax, DaxKmemOffline, DaxKmemOnline), To: DaxNone,
			Triggers: fsm.List(TriggerUnprepare, TriggerPrepare),
			When:     "Users empty and not kept (the Whole device of a configured shared memdev stays)",
			Effects:  "kmem: blocks offlined (EBUSY: stays, error up); unbound; resized to 0; delete written; commit (d)",
		},
		{
			From:     fsm.List(DaxCreating, DaxUnbound, DaxDeviceDax, DaxKmemOffline, DaxKmemOnline),
			To:       DaxNone,
			Triggers: fsm.List(TriggerRecover),
			When: "the recovery deletes a dax device of a region of ours without users and not the kept Whole " +
				"device, or releases the device of a grant that it releases",
			Guard:   "blocks offline first",
			Effects: "deleted",
		},
	},
	Refused: []fsm.Refusal[DaxDeviceState]{
		{From: fsm.List(DaxUnbound), To: fsm.List(DaxKmemOnline), Why: "never online without the fence step"},
	},
}

// mounted are the states of a mounted Mount.
var mounted = fsm.List(MountedRO, MountedRW)

// MountTable is the transition table of Mount.State.
var MountTable = fsm.Table[MountState]{
	Object: "Mount",
	Field:  "State",
	States: fsm.List(MountNone, MountedRO, MountedRW, MountForeign, MountUnrestored),
	Rows: []fsm.Transition[MountState]{
		{
			From: fsm.List(MountNone), To: MountedRO, Triggers: fsm.List(TriggerPrepare),
			When:  "the first fs grant of the access, no writer grant",
			Guard: "filesystem type allowed and detected; block device set read-only first; the access rules",
			Effects: "mount (direct: dax=always verified; cached: loop and dax=never); commit (d) with Users the " +
				"grant",
		},
		{
			From: fsm.List(MountNone), To: MountedRW, Triggers: fsm.List(TriggerPrepare),
			When: "the first fs grant is a writer (readOnly false)",
			Guard: "a single-host type, or a cluster type on a coherent device; no unexplained superblock (refused " +
				"otherwise: fail closed)",
			Effects: "block device read-write; mount rw; commit (d)",
		},
		{
			From: fsm.List(MountedRO), To: MountedRW, Triggers: fsm.List(TriggerPrepare),
			When:    "a writer grant arrives on a mounted access",
			Guard:   "a type that can be remounted read-write (ext4, xfs); no unexplained superblock; no other access mounted",
			Effects: "block device, loop device and superblock read-write; commit (d)",
		},
		{
			From: fsm.List(MountedRW), To: MountedRO, Triggers: releasers,
			When: "the last writer grant released, readers remain",
			Effects: "syncfs, superblock read-only, loop and block device read-only; EBUSY (a file open for " +
				"write): error up, state unchanged",
		},
		{
			From: mounted, Stays: true, Triggers: releasers,
			When: "an fs grant of the same access arrives, or one is released while others remain; or the " +
				"recovery verified the mount or mounted it again by block device identity",
			Effects: "Users gets the grant or loses it; commit (d)",
		},
		{
			From: mounted, To: MountNone, Triggers: releasers,
			When: "the last user released",
			Effects: "unmounted (never detached); directory removed; commit (d); an unmount that fails busy " +
				"fails the release, the mount unchanged",
		},
		{
			From: fsm.List(MountNone, MountedRO, MountedRW, MountUnrestored), To: MountUnrestored, Triggers: fsm.List(TriggerRecover),
			When: "the recovery cannot mount a REALISED grant's node mount again (region gone, namespace or " +
				"block device changed, mount failed), or fails again at a later start",
			Effects: "the grants of the mount REALISED_UNRESTORED; commit (l)",
		},
		{
			From: fsm.List(MountUnrestored), To: MountedRO, Triggers: fsm.List(TriggerPrepare, TriggerRecover),
			When:    "a Prepare again, or the recovery at a restart, mounts it, no writer",
			Guard:   "identity verified",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(MountUnrestored), To: MountedRW, Triggers: fsm.List(TriggerPrepare, TriggerRecover),
			When:    "a Prepare again, or the recovery at a restart, mounts it for a writer",
			Guard:   "identity verified",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(MountUnrestored), To: MountNone, Triggers: releasers,
			When:    "the last user released",
			Effects: "nothing to unmount; commit (d)",
		},
		{
			From: fsm.List(MountNone, MountedRO, MountedRW), To: MountForeign, Triggers: fsm.List(TriggerRecover),
			When:    "the recovery finds something else mounted at the path",
			Effects: "error logged; never used, never unmounted by the driver; the grants REALISED_UNRESTORED",
		},
	},
	Refused: []fsm.Refusal[MountState]{
		{
			From: fsm.List(MountForeign),
			To:   fsm.List(MountedRO, MountedRW, MountNone),
			Why:  "a foreign mount is never used or unmounted",
		},
	},
}

// Ref returns the reference of the region: its name, or its noted name
// while it has none.
func (r *Region) Ref() RegionRef {
	if r.Name != "" {
		return RegionRef(r.Name)
	}
	return RegionRef(r.NotedName)
}

// Ours tells whether the region is the driver's.
func (r *Region) Ours() bool {
	return r.Owner == OwnerOurs
}

// DaxDevice returns the dax device with the name, nil if there is none.
func (r *Region) DaxDevice(name string) *DaxDevice {
	for i := range r.DaxDevices {
		if r.DaxDevices[i].Name == name {
			return &r.DaxDevices[i]
		}
	}
	return nil
}

// WholeDevice returns the Whole dax device of the region, nil if there is
// none.
func (r *Region) WholeDevice() *DaxDevice {
	for i := range r.DaxDevices {
		if r.DaxDevices[i].Whole {
			return &r.DaxDevices[i]
		}
	}
	return nil
}

// FirstDax returns the name of the first dax device of the region: its
// Whole device, else its daxN.0 (also once a slice reuses it), "" for none.
func (r *Region) FirstDax() string {
	if w := r.WholeDevice(); w != nil {
		return w.Name
	}
	for _, d := range r.DaxDevices {
		if strings.HasSuffix(d.Name, ".0") {
			return d.Name
		}
	}
	return ""
}

// Mount returns the node mount with the access, nil if there is none.
func (r *Region) Mount(access Access) *Mount {
	for i := range r.Mounts {
		if r.Mounts[i].Access == access {
			return &r.Mounts[i]
		}
	}
	return nil
}

// TargetsSerial tells whether the region targets the memdev with the serial.
func (r *Region) TargetsSerial(serial Serial) bool {
	return slices.Contains(r.Targets, serial)
}
