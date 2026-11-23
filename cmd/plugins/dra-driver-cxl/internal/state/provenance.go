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
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Provenance is the sticky knowledge of the source of a serial: whether a
// memdev with it is a device of a memory pool or a device of the node. The
// current API view does not tell: a pool device may be attached while the
// pool's ResourceSlices are gone, or before its claim has status data.
//
// Identity: Serial.
//
// States: ProvenanceState. UNKNOWN: no record. HELD: a new memdev not
// published because its source is uncertain (HeldReason). LOCAL: established
// as a device of the node. POOL: seen in a pool ResourceSlice, in a pool
// claim's status data or prepared by the pool driver; sticky.
//
// Transitions: ProvenanceTable below; a transition not in it is refused.
//
// Invariants: POOL never turns LOCAL; no new local grant on a serial that is
// POOL or HELD.
//
// Persistence: fact, the only copy of the provenance; the records of POOL
// and LOCAL serials with Pool and LastSeen (committed at most hourly when
// only LastSeen changed), and of HELD serials with HeldSince and HeldReason
// (committed durably when the hold starts, ends or changes its reason), kept
// across restarts and reboots: a hold goes on from its HeldSince. The
// absence of a serial is kept in memory only: a restart observes it anew.
//
// Locks and waits: the provenance watcher is the only writer.
type Provenance struct {
	Serial Serial          `json:"serial"`
	State  ProvenanceState `json:"state"`
	// Pool is <driver>/<pool> of the ResourceSlice or claim that listed
	// the serial last.
	Pool string `json:"pool,omitempty"`
	// LastSeen is when the serial was last seen in a pool ResourceSlice,
	// prepared or on the node.
	LastSeen time.Time `json:"lastSeen,omitzero"`
	// HeldSince is when the hold started, or when its reason last turned
	// from one without expiry to one with: the expiry counts from it.
	HeldSince  time.Time  `json:"heldSince,omitzero"`
	HeldReason HeldReason `json:"heldReason,omitempty"`
}

// HeldReason tells why a serial is HELD.
type HeldReason string

const (
	// HeldNeverListed: the pool ResourceSlices were never listed. No
	// expiry.
	HeldNeverListed HeldReason = "pool ResourceSlices never listed"
	// HeldAttaching: a pool claim of the node is attaching. No expiry.
	HeldAttaching HeldReason = "pool claim attaching"
	// HeldPoolMissing: a remembered pool's ResourceSlices are missing for
	// less than 1 h.
	HeldPoolMissing HeldReason = "pool ResourceSlices missing"
	// HeldListFailing: the list of the pool ResourceSlices fails.
	HeldListFailing HeldReason = "list failing"
	// HeldClaimsNotListable: the pool claims of the node cannot be listed.
	HeldClaimsNotListable HeldReason = "claims not listable"
	// HeldLost: the record of the serial was lost.
	HeldLost HeldReason = "store lost"
)

// ProvenanceState is the state of a Provenance.
type ProvenanceState string

const (
	ProvenanceUnknown ProvenanceState = "UNKNOWN"
	ProvenanceHeld    ProvenanceState = "HELD"
	ProvenanceLocal   ProvenanceState = "LOCAL"
	ProvenancePool    ProvenanceState = "POOL"
)

// ProvenanceTable is the transition table of Provenance.State.
var ProvenanceTable = fsm.Table[ProvenanceState]{
	Object: "Provenance",
	Field:  "State",
	States: fsm.List(ProvenanceUnknown, ProvenanceHeld, ProvenanceLocal, ProvenancePool),
	Rows: []fsm.Transition[ProvenanceState]{
		{
			From: fsm.List(ProvenanceUnknown), To: ProvenancePool, Triggers: fsm.List(TriggerList, TriggerPrepare),
			When: "a pool ResourceSlice lists the serial; or a pool claim's status data names it; or a pool grant " +
				"is JOURNALED",
			Effects: "commit (d) with Pool <driver>/<pool> and LastSeen",
		},
		{
			From: fsm.List(ProvenanceUnknown), To: ProvenanceHeld, Triggers: fsm.List(TriggerObserve),
			When: "a new memdev appears with a reason for doubt: pool ResourceSlices never listed; a pool claim of " +
				"the node attaching; a remembered pool's ResourceSlices missing for less than 1 h; the list fails; " +
				"the record was lost",
			Effects: "commit (d) with HeldSince and HeldReason; not published",
		},
		{
			From: fsm.List(ProvenanceUnknown), To: ProvenanceLocal, Triggers: fsm.List(TriggerObserve),
			When:    "a new memdev appears and there is no reason to hold it",
			Effects: "commit (l); published per its kind",
		},
		{
			From: fsm.List(ProvenanceHeld), Stays: true, Triggers: fsm.List(TriggerObserve, TriggerList),
			When:    "the reason refreshed by a later observation",
			Effects: "commit (d) when the reason changed; HeldSince restarts at a change from a reason without expiry",
		},
		{
			From: fsm.List(ProvenanceHeld), To: ProvenanceLocal, Triggers: fsm.List(TriggerExpire),
			When: "the hold expired (5 min) and the reason was: list failing, pool missing for less than 1 h, store " +
				"lost, claims not listable",
			Guard:   "not \"never listed\", not \"attaching\"",
			Effects: "commit (d); published",
		},
		{
			From: fsm.List(ProvenanceHeld), To: ProvenanceLocal, Triggers: fsm.List(TriggerObserve),
			When:    "a later observation finds no reason for doubt any more, or the engine has the memdev as local",
			Guard:   "not listed by a pool ResourceSlice or claim",
			Effects: "commit (d); published",
		},
		{
			From: fsm.List(ProvenanceHeld), To: ProvenanceUnknown, Triggers: fsm.List(TriggerObserve),
			When:    "the memdev is gone before its hold ended",
			Effects: "commit (d) removing the record",
		},
		{
			From: fsm.List(ProvenanceHeld), To: ProvenancePool, Triggers: fsm.List(TriggerList, TriggerPrepare),
			When:    "listed by a ResourceSlice or a claim, or a pool grant",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(ProvenanceLocal), To: ProvenancePool, Triggers: fsm.List(TriggerList, TriggerPrepare),
			When:    "a pool ResourceSlice or claim lists it later",
			Effects: "commit (d); withdrawn from publication; existing local grants logged once (conflict)",
		},
		{
			From: fsm.List(ProvenancePool), Stays: true, Triggers: fsm.List(TriggerList, TriggerObserve, TriggerPrepare),
			When:    "seen again",
			Effects: "LastSeen (l, at most hourly)",
		},
		{
			From: fsm.List(ProvenancePool), To: ProvenanceUnknown, Triggers: fsm.List(TriggerExpire),
			When: "24 h of observed absence (observations at most 5 min apart; the absence timer restarts after a " +
				"gap) and no grant",
			Effects: "commit (l) removing the record",
		},
		{
			From: fsm.List(ProvenanceLocal), To: ProvenanceUnknown, Triggers: fsm.List(TriggerExpire),
			When:    "the memdev absent for 24 h of observed absence",
			Effects: "commit (l) removing the record",
		},
		{
			From: fsm.List(ProvenancePool, ProvenanceLocal), To: ProvenanceUnknown, Triggers: fsm.List(TriggerLoad),
			When:    "the operator names the serial with --forget-provenance at the start",
			Effects: "commit (d) removing the record; a memdev of the serial is new again",
		},
	},
	Refused: []fsm.Refusal[ProvenanceState]{
		{From: fsm.List(ProvenancePool), To: fsm.List(ProvenanceLocal, ProvenanceHeld), Why: "POOL is sticky"},
	},
}
