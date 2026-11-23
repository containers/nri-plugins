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

import "github.com/containers/nri-plugins/pkg/cxl/fsm"

// Grant is the memory given to one allocation result of a claim on one
// memdev, local or pool; the grants of a lease are those of its companion
// claim. Asked is what the result and its MemoryConfig ask for, Intent what
// the realisation may create, Got what it made. A grant references the
// kernel objects it uses (a region, a dax device, a node mount) instead of
// owning them: the Region record lists the grant among the users of each.
//
// Identity: Ref, the claim UID and the index of the allocation result;
// Serial is the memdev.
//
// States: GrantState. REQUESTED: decided from the claim and the config,
// nothing touched, not in the store. JOURNALED: the intent is in the store;
// a cut after this point is recoverable. REALISING: kernel work in progress.
// REALISED: usable by a container. REALISED_UNRESTORED: its node mount, dax
// or block device did not verify at a start: no CDI device. RELEASING:
// lends revoked, binds and mounts undone, memory offlined, dax device
// deleted, the region released per Policy when the grant was its last user.
// NONE: gone from the store. A refusal is a Reason on the way to
// RELEASING, not a state; a decoder hole is a typed error of the Prepare.
//
// Transitions: GrantTable below, and BindTable for Got.Bind; a transition
// not in them is refused.
//
// Invariants: every object named by Got is named by Intent first; Got.Region
// names a Region record that targets Serial; Got.Dax and Got.Mount name
// objects of that region that list the grant among their users.
//
// Persistence: intent (JOURNALED, committed durably before the first kernel
// action) and fact (Got, committed durably after every step that created
// something, before the next command that depends on it). A load in
// another boot keeps Serial, Source and Asked and clears Got and Intent.
//
// Locks and waits: the engine serializes the kernel work of a memdev; the
// record is changed only by store commits.
type Grant struct {
	Ref    GrantRef   `json:"ref"`
	Serial Serial     `json:"serial"`
	Asked  Asked      `json:"asked"`
	Intent Intent     `json:"intent,omitzero"`
	Got    Got        `json:"got,omitzero"`
	State  GrantState `json:"state"`
	// Reason tells why a grant is RELEASING before it was REALISED: the
	// error of the step of its Prepare that failed or refused it.
	Reason string `json:"reason,omitempty"`
}

// GrantState is the state of a Grant.
type GrantState string

const (
	GrantRequested  GrantState = "REQUESTED"
	GrantJournaled  GrantState = "JOURNALED"
	GrantRealising  GrantState = "REALISING"
	GrantRealised   GrantState = "REALISED"
	GrantUnrestored GrantState = "REALISED_UNRESTORED"
	GrantReleasing  GrantState = "RELEASING"
	GrantNone       GrantState = "NONE"
)

// Asked is what an allocation result and the device section of its
// MemoryConfig ask of a grant: what the engine acts on.
type Asked struct {
	Source Source `json:"source"`
	// Kind is how the memdev is used; with Source it is the row of the
	// release policy. It stays with a grant whose memdev record is gone.
	Kind     GrantKind `json:"kind"`
	Mode     Mode      `json:"mode"`
	ReadOnly bool      `json:"readOnly,omitempty"`
	// Access of mode fs, "" for the default of the filesystem type.
	Access Access `json:"access,omitempty"`
	// FSType of mode fs, "" to detect it.
	FSType string `json:"fsType,omitempty"`
	// Path of mode fs is the directory in the containers.
	Path string `json:"path,omitempty"`
	// Size is the size in bytes that the allocation result asks for.
	Size uint64 `json:"size,omitempty"`
	// Shared: one share of a shared device; false: the device or the slice
	// of it exclusively.
	Shared bool `json:"shared,omitempty"`
}

// GrantKind is the kind of device that a grant uses: its DeviceClass
// without "cxl-", the source and whether the device is shared or of
// persistent memory (Asked.Shared tells whether a persistent one is
// shared).
type GrantKind string

const (
	// KindLocal is a slice of a local memdev for one grant.
	KindLocal GrantKind = "local"
	// KindLocalShared is a local memdev that several grants map at a time.
	KindLocalShared GrantKind = "local-shared"
	// KindLocalPersistent is a local memdev of persistent memory in mode
	// block or fs.
	KindLocalPersistent GrantKind = "local-persistent"
	// KindPool is a whole pool device for one grant.
	KindPool GrantKind = "pool"
	// KindPoolShared is a pool device that several grants map at a time.
	KindPoolShared GrantKind = "pool-shared"
	// KindPoolPersistent is a pool device of persistent memory in mode
	// block or fs.
	KindPoolPersistent GrantKind = "pool-persistent"
)

// KindOf returns the kind of a device of the source that is shared or of
// persistent memory.
func KindOf(source Source, shared, persistent bool) GrantKind {
	kind := KindLocal
	if source == SourcePool {
		kind = KindPool
	}
	switch {
	case persistent:
		return kind + "-persistent"
	case shared:
		return kind + "-shared"
	}
	return kind
}

// Source returns the source of the kind's devices.
func (k GrantKind) Source() Source {
	if k == KindPool || k == KindPoolShared || k == KindPoolPersistent {
		return SourcePool
	}
	return SourceLocal
}

// Shared tells whether the kind is a shared device of volatile memory.
func (k GrantKind) Shared() bool {
	return k == KindLocalShared || k == KindPoolShared
}

// Persistent tells whether the kind is a device of persistent memory.
func (k GrantKind) Persistent() bool {
	return k == KindLocalPersistent || k == KindPoolPersistent
}

// Intent names what the realisation of a grant may create, committed
// before the first kernel action that creates it.
type Intent struct {
	// Region is the region the grant uses or creates: its noted name, set
	// to the created name with Got.Region when the kernel gave another.
	Region RegionRef `json:"region,omitempty"`
	// Noted are the regions that earlier Prepares of the grant noted and
	// did not create, before Region.
	Noted []RegionRef `json:"noted,omitempty"`
	// NdRegion is the nd region whose fsdax namespace the grant may
	// create.
	NdRegion string `json:"ndRegion,omitempty"`
	// MountPath is the node mount the grant may mount.
	MountPath string `json:"mountPath,omitempty"`
	// BindTarget is the lease bind the grant may bind.
	BindTarget string `json:"bindTarget,omitempty"`
	// Lend is the lend of a companion grant, before the node is lent.
	Lend *GrantLend `json:"lend,omitempty"`
}

// Got is what the realisation of a grant made.
type Got struct {
	Region RegionRef `json:"region,omitempty"`
	// Dax is the name of the dax device of the grant in the region's
	// DaxDevices.
	Dax string `json:"dax,omitempty"`
	// NUMANode is the NUMA node of the memory of a numa grant (-1 when it
	// was not known).
	NUMANode *int `json:"numaNode,omitempty"`
	// Size is the bytes that the grant got.
	Size        uint64       `json:"size,omitempty"`
	Namespace   string       `json:"namespace,omitempty"`
	BlockDevice *BlockDevice `json:"blockDevice,omitempty"`
	// Mount is the access of the region's node mount that the grant
	// uses.
	Mount Access     `json:"mount,omitempty"`
	Bind  *Bind      `json:"bind,omitempty"`
	Lend  *GrantLend `json:"lend,omitempty"`
}

// BlockDevice is a block device of an fsdax namespace (/dev/pmemK).
type BlockDevice struct {
	Name string     `json:"name"`
	Dev  DevNumbers `json:"dev"`
}

// GrantLend is the lend of a grant of a companion claim: the lease, its
// requester and what was lent, kept with the grant so that an Unprepare
// revokes it also after the lease record is gone.
type GrantLend struct {
	Lease     string `json:"lease"`
	Pod       PodRef `json:"pod"`
	Container string `json:"container"`
	// NUMANode is the lent NUMA node of a numa lease.
	NUMANode *int `json:"numaNode,omitempty"`
	// FSDir and Name are the directory of the fs leases of the lease
	// claim and the mount point of the lease in it.
	FSDir string `json:"fsDir,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Bind is the bind of a node mount into the lease-fs directory of a lease
// claim, the lend of an fs lease.
//
// Identity: Target, lease-fs/<lease claim uid>/<name>.
//
// States: BindState. ABSENT: not bound. BOUND: bound and verified. STALE:
// the unmount failed with EBUSY (a file of the bind is mapped): also in
// the memdev's StaleBinds, retried. A start leaves what else is mounted at
// a target, or a target that another prepared claim lists, as it is,
// without a record.
//
// Transitions: BindTable below; a transition not in it is refused.
//
// Invariants: a BOUND bind's node mount is mounted.
//
// Persistence: intent (the target in Intent before the bind) and fact.
//
// Locks and waits: under the engine lock.
type Bind struct {
	Target string    `json:"target"`
	State  BindState `json:"state"`
}

// BindState is the state of a Bind.
type BindState string

const (
	BindAbsent BindState = "ABSENT"
	BindBound  BindState = "BOUND"
	BindStale  BindState = "STALE"
)

// releasers are the actions that release the objects of a grant.
var releasers = fsm.List(TriggerUnprepare, TriggerPrepare, TriggerRecover)

// grantStored are the states of a grant record in the store.
var grantStored = fsm.List(GrantJournaled, GrantRealising, GrantRealised, GrantUnrestored, GrantReleasing)

// GrantTable is the transition table of Grant.State.
var GrantTable = fsm.Table[GrantState]{
	Object: "Grant",
	Field:  "State",
	States: fsm.List(GrantRequested, GrantJournaled, GrantRealising, GrantRealised, GrantUnrestored, GrantReleasing, GrantNone),
	Rows: []fsm.Transition[GrantState]{
		{
			From: fsm.List(GrantRequested), To: GrantJournaled, Triggers: fsm.List(TriggerPrepare),
			When: "the grant request is built from the result and the config",
			Guard: "no FOREIGN region on the memdev; local: provenance not POOL or HELD, a fresh list of the " +
				"pool ResourceSlices does not name the serial, no pool grant on it",
			Effects: "commit (d) of the intent: serial, mode, what may be created (noted region name, namespace, " +
				"mount path, bind target, lend)",
		},
		{
			From: fsm.List(GrantJournaled), To: GrantRealising, Triggers: fsm.List(TriggerPrepare),
			When:  "the first kernel action of the grant",
			Guard: "memdev ready (pool: waited for; without a driver: enabled after the probe grace)",
			Effects: "kernel commands; every created object committed (d) into Got before the next command that " +
				"depends on it",
		},
		{
			From: fsm.List(GrantRealising), To: GrantRealised, Triggers: fsm.List(TriggerPrepare),
			When: "the last step done (numa: fenced and online; dax: bound and zeroed; block, fs: mounted and " +
				"verified; companion: bound or lent)",
			Effects: "commit (d) jointly with the Preparation PREPARED (or per grant for claims of several " +
				"results, then PREPARED last)",
		},
		{
			From: fsm.List(GrantRealising), To: GrantReleasing, Triggers: fsm.List(TriggerPrepare, TriggerUnprepare),
			When: "a persistent refusal (no fsdax namespace for a reader; no allowed filesystem type; a block " +
				"writer that is not exclusive) or a config refusal found after kernel work; or any other " +
				"failure (decoder hole, no room, EBUSY online, timeout)",
			Effects: "Reason the error of the step, the Prepare's error (a typed error to the kubelet plugin); " +
				"rollback: the objects this grant made are released per Policy (trigger rollback); " +
				"an Unprepare of a grant that a failed release at the start left REALISING releases it the same way",
		},
		{
			From: fsm.List(GrantJournaled), To: GrantReleasing, Triggers: fsm.List(TriggerPrepare, TriggerUnprepare),
			When: "the rollback of a Prepare that failed before the grant's first kernel action; an Unprepare of " +
				"a PREPARING or KEPT preparation",
			Effects: "Reason the error of the step when the grant's own Prepare failed; release what Got and " +
				"Intent name (a kept region per Policy); commit (d)",
		},
		{
			From: fsm.List(GrantRealised), To: GrantReleasing, Triggers: releasers,
			When: "an Unprepare; the rollback of a sibling grant of the same claim; the recovery of a PREPARING " +
				"preparation (unfinished claim)",
			Guard:   "companion: lends revoked and binds unbound first",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(GrantReleasing), To: GrantNone, Triggers: releasers,
			When: "mounts unmounted or remounted read-only, blocks offline, dax device deleted, the region per Policy " +
				"when the grant was its last user",
			Effects: "commit (d) removing the grant; the Memdev's grants and the users of the region's objects " +
				"updated",
		},
		{
			From: fsm.List(GrantReleasing), Stays: true, Triggers: releasers,
			When:    "a release step failed (EBUSY offline, busy mount, holder)",
			Effects: "error up (kubelet retries); records unchanged",
		},
		{
			From: fsm.List(GrantReleasing), To: GrantJournaled, Triggers: fsm.List(TriggerPrepare, TriggerRecover),
			When: "the rollback of a failed Prepare, or the recovery of a cut Prepare's REALISED grant, released what " +
				"the grant made but kept its region below a newer region of another memdev (Policy in order refused, " +
				"or recovery keep)",
			Effects: "the grant holds the KEPT region for its claim (Intent.Region; Got cleared but Bind and Lend; " +
				"Reason cleared; the preparation KEPT): its Prepare again reuses it, its Unprepare releases it; " +
				"commit (d)",
		},
		{
			From: fsm.List(GrantRealised), To: GrantUnrestored, Triggers: fsm.List(TriggerRecover),
			When: "verification failed (mount: not remountable; dax: name, dev, size or region mismatch; block: " +
				"namespace, name or dev mismatch)",
			Effects: "commit (l); no CDI device",
		},
		{
			From: fsm.List(GrantUnrestored), To: GrantRealised, Triggers: fsm.List(TriggerPrepare, TriggerRecover),
			When:    "Prepare again, or the recovery at a restart, verifies or mounts again",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(GrantUnrestored), To: GrantReleasing, Triggers: releasers,
			When:    "an Unprepare or a rollback",
			Effects: "commit (d); nothing of Got to undo beyond what verifies",
		},
		{
			From: grantStored, Stays: true, Triggers: fsm.List(TriggerLoad),
			When:    "the preparation became PREPARED_STALE at the load",
			Effects: "Got and Intent cleared, nothing released at start; commit (l)",
		},
		{
			From: fsm.List(GrantJournaled, GrantRealising), To: GrantJournaled, Triggers: fsm.List(TriggerRecover),
			When:    "the recovery of a cut Prepare kept the region (region KEPT)",
			Guard:   "Policy (trigger recovery) says keep",
			Effects: "commit (d); a numa companion's lend is left to kubelet",
		},
		{
			From: fsm.List(GrantJournaled, GrantRealising), To: GrantNone, Triggers: fsm.List(TriggerRecover),
			When:    "the recovery of a cut Prepare released the objects named by Got and by Intent",
			Guard:   "Policy (trigger recovery) says destroy",
			Effects: "commit (d)",
		},
		{
			From:     fsm.List(GrantJournaled, GrantRealising, GrantRealised, GrantUnrestored),
			To:       GrantNone,
			Triggers: fsm.List(TriggerPrepare, TriggerUnprepare),
			When: "a Prepare again or an Unprepare of a PREPARED_STALE preparation (a RELEASING grant goes by the " +
				"release row)",
			Guard:   "the preparation is PREPARED_STALE",
			Effects: "dropped in the commit (d) of the preparation; an Unprepare releases by serial first",
		},
		{
			From: grantStored, To: GrantNone, Triggers: fsm.List(TriggerLoad),
			When:    "its preparation of another boot is dropped at the load (the driver drops such records)",
			Effects: "dropped with the preparation, commit (l)",
		},
	},
	Refused: []fsm.Refusal[GrantState]{
		{From: fsm.List(GrantRequested), To: fsm.List(GrantRealising, GrantRealised), Why: "nothing is created without the intent"},
		{From: fsm.List(GrantNone), Why: "a released grant is gone"},
	},
}

// BindTable is the transition table of Bind.State.
var BindTable = fsm.Table[BindState]{
	Object: "Bind",
	Field:  "State",
	States: fsm.List(BindAbsent, BindBound, BindStale),
	Rows: []fsm.Transition[BindState]{
		{
			From: fsm.List(BindAbsent), To: BindBound, Triggers: fsm.List(TriggerPrepare),
			When:  "companion Prepare of an fs lease, after the node mount exists",
			Guard: "the name is unused; the node mount is mounted",
			Effects: "plain bind of a read-only node mount, or a read-only bind of a writable type; verified; " +
				"commit (d) of the target in Got",
		},
		{
			From: fsm.List(BindBound), To: BindAbsent, Triggers: fsm.List(TriggerUnprepare, TriggerPrepare, TriggerRecover),
			When: "companion Unprepare (after the revoke); the rollback of the companion Prepare; the recovery " +
				"sweep of a bind that no REALISED grant of a PREPARED preparation lists",
			Effects: "unmount (never detached); commit (d)",
		},
		{
			From: fsm.List(BindBound), To: BindStale, Triggers: fsm.List(TriggerUnprepare, TriggerPrepare, TriggerRecover),
			When: "the unmount failed with EBUSY (a file of the bind is mapped)",
			Effects: "the memdev's StaleBinds gets the target; the lease RELEASING_BUSY; retried at every release " +
				"and by the periodic pass",
		},
		{
			From:     fsm.List(BindStale),
			To:       BindAbsent,
			Triggers: fsm.List(TriggerUnprepare, TriggerPrepare, TriggerRecover, TriggerReconcile),
			When:     "a retry unmounts it (the holder ended)",
			Effects:  "commit (d)",
		},
		{
			From: fsm.List(BindAbsent, BindBound), To: BindBound, Triggers: fsm.List(TriggerRecover),
			When: "the recovery binds a REALISED fs companion again whose node mount is mounted, unless its claim " +
				"is UNPREPARING; an existing mount at a listed target is kept",
		},
		{
			From: fsm.List(BindBound), To: BindBound, Triggers: fsm.List(TriggerPrepare),
			When: "a Prepare again mounts the node mount of an unrestored fs companion again and binds it again " +
				"as the recovery does",
		},
	},
}
