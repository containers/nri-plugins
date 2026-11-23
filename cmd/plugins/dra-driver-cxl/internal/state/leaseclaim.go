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

// LeaseClaim is a claim on the lease device, prepared by cxl.nri.io for a
// pod: its lease socket, its directories and the pods it is reserved for.
// It owns <data dir>/leases/<uid>/cxl.sock, <data dir>/leases/<uid>/fs and
// <data dir>/lease-fs/<uid>; the CDI device lease-<uid> gives them to the
// claim's containers.
//
// Identity: UID, the claim UID (also the socket directory and the CDI
// device name); Namespace and Name of the claim.
//
// States: LeaseClaimState. NONE: no record. OPEN: listening.
// OPEN_NO_LISTENER: listening again at a start failed; the reconcile and
// the next OpenClaim listen again. CLOSING: CloseClaim releases every lease of the claim; a new
// lease request on the socket is refused; a start keeps it CLOSING.
// CLOSED: the listener and its connections closed, the record removed.
// REMOVING: its lease-fs directory is gone and its socket directory is
// being removed (a bind still mounted keeps lease-fs/<uid>, and the claim
// CLOSED, until the last lease is gone). CLOSED and REMOVING are in memory only.
//
// Transitions: LeaseClaimTable below; a transition not in it is refused.
//
// Invariants: a lease claim in the store is OPEN, OPEN_NO_LISTENER or
// CLOSING; lease-fs/<uid> and leases/<uid> exist only for lease claims of
// prepared claims.
//
// Persistence: fact, the only copy; committed by OpenClaim before its
// socket, its CDI device and the Preparation PREPARED of the claim. Kept
// across reboots: kubelet prepares the claim again.
//
// Locks and waits: the commits of the store; CloseClaim waits up to 5 s for
// the leases of the claim.
type LeaseClaim struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Pods are the pods of reservedFor at the Prepare, refreshed when a
	// peer's pod is unknown.
	Pods  []PodRef        `json:"pods"`
	State LeaseClaimState `json:"state"`
}

// LeaseClaimState is the state of a LeaseClaim.
type LeaseClaimState string

const (
	LeaseClaimNone           LeaseClaimState = "NONE"
	LeaseClaimOpen           LeaseClaimState = "OPEN"
	LeaseClaimOpenNoListener LeaseClaimState = "OPEN_NO_LISTENER"
	LeaseClaimClosing        LeaseClaimState = "CLOSING"
	LeaseClaimClosed         LeaseClaimState = "CLOSED"
	LeaseClaimRemoving       LeaseClaimState = "REMOVING"
)

// LeaseClaimTable is the transition table of LeaseClaim.State.
var LeaseClaimTable = fsm.Table[LeaseClaimState]{
	Object: "LeaseClaim",
	Field:  "State",
	States: fsm.List(LeaseClaimNone, LeaseClaimOpen, LeaseClaimOpenNoListener,
		LeaseClaimClosing, LeaseClaimClosed, LeaseClaimRemoving),
	Rows: []fsm.Transition[LeaseClaimState]{
		{
			From: fsm.List(LeaseClaimNone), To: LeaseClaimOpen, Triggers: fsm.List(TriggerPrepare),
			When: "a Prepare of a claim with a lease result (OpenClaim)",
			Effects: "commit (d), a failed one fails the Prepare; CDI device lease-<uid>; its lease-fs directory made; the socket " +
				"created in a private directory and renamed",
		},
		{
			From: fsm.List(LeaseClaimOpen), Stays: true, Triggers: fsm.List(TriggerPrepare, TriggerRequest),
			When:    "OpenClaim again; Pods refreshed from reservedFor when a peer's pod is unknown",
			Effects: "commit (d)",
		},
		{
			From: fsm.List(LeaseClaimOpenNoListener, LeaseClaimClosing), To: LeaseClaimOpen, Triggers: fsm.List(TriggerPrepare),
			When:    "OpenClaim of a claim whose socket did not start again, or whose close did not finish",
			Effects: "commit (d); the socket started; a later Unprepare closes the claim",
		},
		{
			From: fsm.List(LeaseClaimOpen, LeaseClaimOpenNoListener), To: LeaseClaimOpen, Triggers: fsm.List(TriggerRecover),
			When:  "the start listens on the socket again",
			Guard: "listen succeeded",
		},
		{
			From: fsm.List(LeaseClaimOpenNoListener), To: LeaseClaimOpen, Triggers: fsm.List(TriggerReconcile),
			When:    "the reconcile listens on the socket again",
			Guard:   "listen succeeded",
			Effects: "commit (d)",
		},
		{
			From:     fsm.List(LeaseClaimOpen, LeaseClaimOpenNoListener),
			To:       LeaseClaimOpenNoListener,
			Triggers: fsm.List(TriggerRecover),
			When:     "listening again at the start failed",
			Effects:  "logged; the socket starts at the next OpenClaim",
		},
		{
			From: fsm.List(LeaseClaimOpen, LeaseClaimOpenNoListener), To: LeaseClaimClosing, Triggers: fsm.List(TriggerUnprepare),
			When: "an Unprepare of the claim (CloseClaim)",
			Effects: "commit (d); every lease of the claim released; a new lease request on the socket is refused, " +
				"releases, lists and env are served",
		},
		{
			From: fsm.List(LeaseClaimClosing), Stays: true, Triggers: fsm.List(TriggerRecover),
			When: "the start of a claim whose close a stop cut",
			Effects: "the socket listened again (a failed listen logged); new lease requests refused; kubelet's " +
				"Unprepare retry closes the claim",
		},
		{
			From: fsm.List(LeaseClaimClosing), Stays: true, Triggers: fsm.List(TriggerUnprepare),
			When:    "the Unprepare retried: the leases of the claim released again",
			Effects: "\"still releasing N leases\" to kubelet while not every lease has DeletionRequested within 5 s",
		},
		{
			From: fsm.List(LeaseClaimClosing), To: LeaseClaimClosed, Triggers: fsm.List(TriggerUnprepare),
			When: "every lease has DeletionRequested (not necessarily gone)",
			Effects: "the listener and its open connections closed; commit (d) removing the record; CDI device " +
				"removed; the socket removed",
		},
		{
			From:     fsm.List(LeaseClaimClosed),
			To:       LeaseClaimRemoving,
			Triggers: fsm.List(TriggerUnprepare, TriggerRelease, TriggerReconcile, TriggerRecover),
			When: "the directories of a claim without a record are removed: by CloseClaim, by the release of its last " +
				"lease, by the sweep of the reconcile and of the start",
			Guard:   "no bind mounted in its lease-fs directory, which is removed (else it stays CLOSED)",
			Effects: "the entries of its socket directory removed by name, never following a symlink",
		},
		{
			From:     fsm.List(LeaseClaimRemoving),
			To:       LeaseClaimNone,
			Triggers: fsm.List(TriggerUnprepare, TriggerRelease, TriggerReconcile, TriggerRecover),
			When:     "the socket directory removed",
		},
	},
	Refused: []fsm.Refusal[LeaseClaimState]{
		{
			From: fsm.List(LeaseClaimClosing), To: fsm.List(LeaseClaimOpen), Except: fsm.List(TriggerPrepare),
			Why: "a closing claim is opened again only by a Prepare of the claim",
		},
	},
}
