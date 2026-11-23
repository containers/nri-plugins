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

package containers

import (
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// The states of the Plugin (plugin.go has its header): the readiness of
// fencing and the NRI connection, with their tables.

// Readiness is the readiness of fencing of the running containers of the
// plugin, in memory.
type Readiness string

// The readiness of fencing.
const (
	// ReadinessWaiting: neither the first Synchronize nor the lends
	// restored yet.
	ReadinessWaiting Readiness = "WAITING"
	// ReadinessSynchronized: the container table is built, the lends of
	// the leases are not restored yet.
	ReadinessSynchronized Readiness = "SYNCHRONIZED"
	// ReadinessLendsRestored: the lends are restored, the first
	// Synchronize has not come yet.
	ReadinessLendsRestored Readiness = "LENDS_RESTORED"
	// ReadinessReady: fence passes run.
	ReadinessReady Readiness = "READY"
)

// ConnState is the state of the NRI connection of the plugin, in memory.
type ConnState string

// The states of the NRI connection.
const (
	// ConnNew: Start has not returned.
	ConnNew ConnState = "NEW"
	// ConnStarted: the stub started.
	ConnStarted ConnState = "STARTED"
	// ConnFailed: the stub could not be made or started: no container is
	// fenced, lends fail.
	ConnFailed ConnState = "FAILED"
	// ConnClosed: the runtime closed the connection before Start returned.
	ConnClosed ConnState = "CLOSED"
	// ConnStopping: the driver stops, a close is no loss.
	ConnStopping ConnState = "STOPPING"
	// ConnStopped: closed while stopping, or after the context of Start
	// ended.
	ConnStopped ConnState = "STOPPED"
	// ConnLost: closed by the runtime: the process exits with status 1.
	ConnLost ConnState = "LOST"
)

const (
	// TriggerLendsRestored is the lease broker's word that the lends of
	// the leases are applied again (LendsRestored).
	TriggerLendsRestored Trigger = "lends restored"
	// TriggerNRIStart is the return of Start of the NRI stub.
	TriggerNRIStart Trigger = "NRI Start"
	// TriggerNRIClose is the close of the NRI connection that the stub
	// reports.
	TriggerNRIClose Trigger = "NRI close"
	// TriggerStopping is the stop of the driver (MarkStopping, Stop).
	TriggerStopping Trigger = "Stop"
)

// ReadinessTable is the transition table of the readiness of fencing.
var ReadinessTable = fsm.Table[Readiness]{
	Object: "Plugin",
	Field:  "Readiness",
	States: fsm.List(ReadinessWaiting, ReadinessSynchronized, ReadinessLendsRestored, ReadinessReady),
	Rows: []fsm.Transition[Readiness]{
		{
			From: fsm.List(ReadinessWaiting), To: ReadinessSynchronized, Triggers: fsm.List(TriggerSynchronize),
			When:    "the first Synchronize built the container table",
			Effects: "WaitSynchronized returns: the lease peers are identified, Prepare goes on",
		},
		{
			From: fsm.List(ReadinessWaiting), To: ReadinessLendsRestored, Triggers: fsm.List(TriggerLendsRestored),
			When: "the broker's first reconcile restored the lends before the first Synchronize",
		},
		{
			From: fsm.List(ReadinessSynchronized), To: ReadinessReady, Triggers: fsm.List(TriggerLendsRestored),
			When:    "the lends restored after the first Synchronize",
			Effects: "a fence pass in the background (fencing allowed-mems); logged once",
		},
		{
			From: fsm.List(ReadinessLendsRestored), To: ReadinessReady, Triggers: fsm.List(TriggerSynchronize),
			When:    "the first Synchronize after the lends were restored",
			Effects: "WaitSynchronized returns; a fence pass in the background (fencing allowed-mems); logged once",
		},
	},
	Refused: []fsm.Refusal[Readiness]{
		{From: fsm.List(ReadinessReady), To: fsm.List(ReadinessWaiting, ReadinessSynchronized, ReadinessLendsRestored),
			Why: "fencing stays ready for the life of the process"},
	},
}

// ConnTable is the transition table of the NRI connection.
var ConnTable = fsm.Table[ConnState]{
	Object: "Plugin",
	Field:  "Conn",
	States: fsm.List(ConnNew, ConnStarted, ConnFailed, ConnClosed, ConnStopping, ConnStopped, ConnLost),
	Rows: []fsm.Transition[ConnState]{
		{
			From: fsm.List(ConnNew), To: ConnStarted, Triggers: fsm.List(TriggerNRIStart),
			When:    "Start of the stub succeeded",
			Effects: "the plugin gets the container events",
		},
		{
			From: fsm.List(ConnNew, ConnClosed), To: ConnFailed, Triggers: fsm.List(TriggerNRIStart),
			When:    "the stub could not be made or started",
			Effects: "Fence records a node only (warned), lends fail with \"lending unavailable: NRI not connected\"",
		},
		{
			From: fsm.List(ConnNew), To: ConnClosed, Triggers: fsm.List(TriggerNRIClose),
			When: "the runtime closed the connection before Start returned",
		},
		{
			From: fsm.List(ConnStarted, ConnClosed), To: ConnLost, Triggers: fsm.List(TriggerNRIStart, TriggerNRIClose),
			When:    "the connection is closed after it started, not stopping, while the context of Start runs",
			Effects: "exit status 1: the restarted process connects again and gets its state from the store",
		},
		{
			From: fsm.List(ConnStarted, ConnClosed), To: ConnStopped, Triggers: fsm.List(TriggerNRIStart, TriggerNRIClose),
			When: "the connection is closed after it started, after the context of Start ended",
		},
		{
			From: fsm.List(ConnNew, ConnStarted, ConnFailed, ConnClosed), To: ConnStopping, Triggers: fsm.List(TriggerStopping),
			When:    "the driver stops",
			Effects: "a close of the connection from now on is no loss",
		},
		{
			From: fsm.List(ConnStopping), To: ConnStopped, Triggers: fsm.List(TriggerNRIClose),
			When: "the connection closed while stopping",
		},
		{
			From: fsm.List(ConnStopping, ConnStopped, ConnFailed, ConnLost), Stays: true,
			Triggers: fsm.List(TriggerNRIStart, TriggerNRIClose, TriggerStopping),
			When:     "a Start, close or stop of a connection that is stopping, gone or absent",
		},
	},
	Refused: []fsm.Refusal[ConnState]{
		{From: fsm.List(ConnLost, ConnStopped), To: fsm.List(ConnStarted), Why: "the stub never connects again: the process exits"},
	},
}
