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
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Lease is one request of a running container for memory, served through
// the socket of its lease claim. Its memory comes from a companion claim
// cxl-lease-<id> whose grants the broker lends to the requester. The lease
// owns its Lend.
//
// Identity: ID, 8 hex digits; the requester (Pod, Container); Claim, the
// UID of the lease claim whose socket received the request; RequestID, the
// client's idempotency key.
//
// States: LeaseState (the protocol's words). NONE: no record. CREATING:
// stored before any API object exists. PENDING: the companion claim and
// pod exist. LENDING: the companion claim's Prepare passed the companion
// gate. READY: fd passable, bind mounted or NUMA node lent. RELEASING: the
// deletion of the companion is requested, the lend revoked first.
// RELEASING_BUSY: the companion's unbind failed busy (a mapped file); the
// companion stays Terminating until the holder ends. FAILED: Error says
// why; a failed lease is released in place and stays FAILED until it is
// gone, so that a list shows why.
//
// Transitions: LeaseTable below; a transition not in it is refused.
//
// Invariants: Lend is set only while the lease is READY, RELEASING,
// RELEASING_BUSY or FAILED; no lend on a NUMA node whose memory is
// offline.
//
// Persistence: intent (CREATING before the API objects) and fact, the only
// copy: a transition is committed before the act that follows it. A load in
// another boot drops every lease; the first reconcile deletes their
// companions.
//
// Locks and waits: the commits of the store.
type Lease struct {
	ID        string     `json:"id"`
	Claim     string     `json:"claim"`
	RequestID string     `json:"requestID,omitempty"`
	Pod       PodRef     `json:"pod"`
	Container string     `json:"container"`
	Asked     LeaseAsked `json:"asked"`
	// Got is what the companion claim's Prepare gave, set at READY.
	Got   *LeaseGot  `json:"got,omitempty"`
	State LeaseState `json:"state"`
	Error string     `json:"error,omitempty"`
	// ReleaseRequested: a release was started.
	ReleaseRequested bool `json:"releaseRequested,omitempty"`
	// DeletionRequested: the deletion of the companion pod and claim was
	// requested.
	DeletionRequested bool       `json:"deletionRequested,omitempty"`
	Lend              *LeaseLend `json:"lend,omitempty"`
	Created           time.Time  `json:"created"`
	// SpecHash is the hash of the companion claim's spec as the API server
	// returned it at its creation (lend.SpecHash).
	SpecHash string `json:"specHash,omitempty"`
}

// LeaseAsked is what a lease request asks for.
type LeaseAsked struct {
	// Source is the requested source, "" for local, then pool.
	Source Source `json:"source,omitempty"`
	// DeviceSource is the source of the device that Serial names when
	// no source was requested.
	DeviceSource Source `json:"deviceSource,omitempty"`
	// Mode is the effective mode.
	Mode  Mode `json:"mode"`
	Share bool `json:"shared,omitempty"`
	// Shared: the ResourceSlice has the device as a shared one.
	Shared bool `json:"sharedDevice,omitempty"`
	// ReadOnly: the fd of dax and block is read-only, the mount of fs.
	ReadOnly bool   `json:"readOnly,omitempty"`
	Access   Access `json:"access,omitempty"`
	FSType   string `json:"fsType,omitempty"`
	// Name is the mount point of an fs lease, unique per lease claim.
	Name string `json:"name,omitempty"`
	// Size is the requested quantity.
	Size string `json:"size,omitempty"`
	// Serial is the requested device; SerialZero tells that the request
	// named serial 0.
	Serial     Serial `json:"serial,omitzero"`
	SerialZero bool   `json:"serialZero,omitempty"`
}

// AsksSerial tells whether the request named a serial.
func (a *LeaseAsked) AsksSerial() bool {
	return a.Serial != 0 || a.SerialZero
}

// LeaseGot is what the companion claim's Prepare gave a lease.
type LeaseGot struct {
	Source Source `json:"source,omitempty"`
	Serial Serial `json:"serial,omitzero"`
	// Shared: the prepared device is a shared device.
	Shared bool   `json:"sharedDevice,omitempty"`
	Size   uint64 `json:"size,omitempty"`
	// Dax and Block are the host paths of the device of a dax or block
	// lease.
	Dax    string `json:"dax,omitempty"`
	Block  string `json:"block,omitempty"`
	Access Access `json:"access,omitempty"`
	FSType string `json:"fsType,omitempty"`
	// Path is the directory of the filesystem of an fs lease in the
	// container.
	Path string `json:"path,omitempty"`
}

// LeaseLend is what a lease lends its requester: a NUMA node added to its
// cpuset.mems (numa) or a bind of a node mount (fs).
type LeaseLend struct {
	NUMANode   *int   `json:"numaNode,omitempty"`
	BindTarget string `json:"bindTarget,omitempty"`
}

// LeaseLends returns the NUMA nodes that the READY leases of mode numa of
// the container name of the pod podUID lend it, each with the ids of the
// leases that hold it, sorted; nil for none. A requester container that
// restarts is created with them.
func LeaseLends(r *Records, podUID, container string) map[int][]string {
	var nodes map[int][]string
	for i := range r.Leases {
		l := &r.Leases[i]
		if l.Pod.UID != podUID || l.Container != container || l.Asked.Mode != ModeNUMA || l.State != LeaseReady ||
			l.Lend == nil || l.Lend.NUMANode == nil {
			continue
		}
		if nodes == nil {
			nodes = map[int][]string{}
		}
		n := *l.Lend.NUMANode
		nodes[n] = append(nodes[n], l.ID)
	}
	for _, ids := range nodes {
		slices.Sort(ids)
	}
	return nodes
}

// LeaseState is the state of a Lease.
type LeaseState string

const (
	LeaseNone          LeaseState = "NONE"
	LeaseCreating      LeaseState = "CREATING"
	LeasePending       LeaseState = "PENDING"
	LeaseLending       LeaseState = "LENDING"
	LeaseReady         LeaseState = "READY"
	LeaseReleasing     LeaseState = "RELEASING"
	LeaseReleasingBusy LeaseState = "RELEASING_BUSY"
	LeaseFailed        LeaseState = "FAILED"
)

// leaseStored are the states of a lease record in the store.
var leaseStored = fsm.List(LeaseCreating, LeasePending, LeaseLending, LeaseReady, LeaseReleasing, LeaseReleasingBusy, LeaseFailed)

// LeaseTable is the transition table of Lease.State.
var LeaseTable = fsm.Table[LeaseState]{
	Object: "Lease",
	Field:  "State",
	States: fsm.List(LeaseNone, LeaseCreating, LeasePending, LeaseLending,
		LeaseReady, LeaseReleasing, LeaseReleasingBusy, LeaseFailed),
	Rows: []fsm.Transition[LeaseState]{
		{
			From: fsm.List(LeaseNone), To: LeaseCreating, Triggers: fsm.List(TriggerRequest),
			When:    "a request on a lease claim's socket: authenticated, validated, the serial resolved, the id chosen",
			Guard:   "an fs lease: the name is unused",
			Effects: "commit (d) before any API object exists; a failed commit refuses the request",
		},
		{
			From: fsm.List(LeaseCreating), To: LeasePending, Triggers: fsm.List(TriggerRequest),
			When:    "the companion claim and pod created",
			Guard:   "still CREATING (OnLending may have won)",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(LeaseCreating, LeaseReleasing), To: LeaseFailed, Triggers: fsm.List(TriggerRequest),
			When:    "the companion creation failed (AlreadyExists, API error); RELEASING: released meanwhile",
			Effects: "the release starts, the lease stays FAILED; the client gets Failed with the error",
		},
		{
			From: fsm.List(LeaseCreating, LeasePending), To: LeaseLending, Triggers: fsm.List(TriggerOnLending),
			When:    "the companion gate passed: the name, the namespace and the owner of the claim",
			Effects: "commit (d); the lend of the requester to the driver (with FSDir and the name of an fs lease)",
		},
		{
			From: fsm.List(LeaseLending, LeaseReady), Stays: true, Triggers: fsm.List(TriggerOnLending),
			When:    "a retried or announced-again Prepare of the companion (also while kubelet retries a failed one)",
			Effects: "no commit",
		},
		{
			From: fsm.List(LeaseLending), To: LeaseReady, Triggers: fsm.List(TriggerOnPrepared),
			When:  "the companion's Prepare hands over the lent devices",
			Guard: "the mode of the device matches (fs and block)",
			Effects: "commit (d) of Got and of the Lend (the NUMA node of numa, the bind target of fs); the waiting " +
				"client gets the response (the fd of dax and block)",
		},
		{
			From: fsm.List(LeaseReady), Stays: true, Triggers: fsm.List(TriggerOnPrepared),
			When:    "the companion's Prepare announced again (kubelet prepares a prepared claim again)",
			Effects: "commit (d) of Got and the Lend again",
		},
		{
			From: fsm.List(LeaseLending, LeaseReady), To: LeaseFailed, Triggers: fsm.List(TriggerOnPrepared),
			When:    "the companion prepared a device of another mode (fs or block against another)",
			Effects: "commit (d); the release starts",
		},
		{
			From: fsm.List(LeaseCreating, LeasePending, LeaseLending, LeaseReady), To: LeaseFailed, Triggers: fsm.List(TriggerOnLendFailed),
			When:    "a bind or lend error of the companion's Prepare, or an error that its retries cannot heal",
			Effects: "commit (d); the release starts",
		},
		{
			From:     fsm.List(LeaseCreating, LeasePending, LeaseLending, LeaseReady, LeaseFailed),
			To:       LeaseFailed,
			Triggers: fsm.List(TriggerOnReleasing),
			When:     "the companion claim unprepared behind the broker (the companion pod deleted or evicted)",
			Effects:  "commit (d); the next reconcile releases it",
		},
		{
			From:     fsm.List(LeaseCreating, LeasePending, LeaseLending, LeaseReady),
			To:       LeaseReleasing,
			Triggers: fsm.List(TriggerRequest),
			When:     "the request's --wait ended (READY: the companion's Prepare made it READY meanwhile)",
			Effects:  "the client gets Failed \"timeout: not Ready after D\"; commit (d) of ReleaseRequested",
		},
		{
			From:     fsm.List(LeaseCreating, LeasePending, LeaseLending, LeaseReady),
			To:       LeaseReleasing,
			Triggers: fsm.List(TriggerRelease, TriggerUnprepare, TriggerReconcile),
			When: "a release by the client; the requester gone; CloseClaim of the lease claim; the reconcile " +
				"(companion gone, requester done, CREATING for longer than 2 min: left over by a crash)",
			Effects: "commit (d) of ReleaseRequested; the node revoked; pod and claim deleted (Foreground); " +
				"DeletionRequested (d)",
		},
		{
			From: fsm.List(LeaseFailed), Stays: true,
			Triggers: fsm.List(TriggerRequest, TriggerRelease, TriggerUnprepare, TriggerReconcile, TriggerOnPrepared,
				TriggerOnLendFailed, TriggerRecover),
			When: "the release of a failed lease",
			Effects: "released in place: commit (d) of ReleaseRequested; the node revoked; pod and claim deleted " +
				"(Foreground); DeletionRequested (d); the lease stays FAILED so that a list shows why",
		},
		{
			From: fsm.List(LeaseReleasing, LeaseReleasingBusy), Stays: true,
			Triggers: fsm.List(TriggerRelease, TriggerUnprepare, TriggerReconcile),
			When:     "the release started again: a record of a start (ReleaseRequested set), its release not running",
			Effects:  "the release runs again",
		},
		{
			From: fsm.List(LeaseReleasing), To: LeaseReleasingBusy, Triggers: fsm.List(TriggerRelease),
			When:    "the companion's bind is STALE: its unbind failed with EBUSY (a file of it is mapped)",
			Effects: "commit (d); the companion stays Terminating; reads work until the holder ends",
		},
		{
			From: fsm.List(LeaseReleasingBusy), To: LeaseReleasing, Triggers: fsm.List(TriggerRelease),
			When:    "the companion's bind is not STALE any more (the holder ended, the unbind succeeded), or it is gone",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(LeaseReleasing, LeaseFailed), To: LeaseNone, Triggers: fsm.List(TriggerRelease),
			When: "pod and claim both NotFound",
			Effects: "commit (d) removing the lease; the lease claim's directories removed when it is closed and " +
				"this was its last lease",
		},
		{
			From: fsm.List(LeaseLending, LeaseReady), To: LeaseFailed, Triggers: fsm.List(TriggerRecover),
			When:    "the start did not restore the device of the companion: its Preparation is PREPARED_UNRESTORED",
			Effects: "commit (d); the release starts; the requester's next list shows the lease Failed",
		},
		{
			From: leaseStored, To: LeaseNone, Triggers: fsm.List(TriggerLoad),
			When:    "the document is of another boot",
			Effects: "dropped; the companions deleted by the first reconcile",
		},
	},
	Refused: []fsm.Refusal[LeaseState]{
		{From: fsm.List(LeaseReady), To: fsm.List(LeasePending), Why: "a lent lease does not go back"},
		{From: fsm.List(LeaseReleasing), To: fsm.List(LeaseReady), Why: "a release is not undone"},
	},
}
