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
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Trigger names the action of the driver or the event that causes a
// transition.
type Trigger = fsm.Trigger

const (
	// TriggerPrepare is NodePrepareResources of either driver, its
	// rollback included.
	TriggerPrepare Trigger = "prepare"
	// TriggerUnprepare is NodeUnprepareResources of either driver.
	TriggerUnprepare Trigger = "unprepare"
	// TriggerRecover is the recovery at the start of the process, both
	// phases.
	TriggerRecover Trigger = "recover"
	// TriggerLoad is the store loading the document at the start of the
	// process.
	TriggerLoad Trigger = "load"
	// TriggerObserve is an observation of the kernel: a scan, a uevent,
	// a memdev that appeared or went.
	TriggerObserve Trigger = "observe"
	// TriggerList is a list of the pool driver's ResourceSlices or of
	// the pool claims allocated to the node.
	TriggerList Trigger = "list"
	// TriggerExpire is a timer of a record that ran out.
	TriggerExpire Trigger = "expire"
	// TriggerRequest is a lease request on the socket of a lease claim,
	// until its response.
	TriggerRequest Trigger = "request"
	// TriggerRelease is the release of a lease: by its client, at the
	// end of its requester, at a failure.
	TriggerRelease Trigger = "release"
	// TriggerReconcile is the periodic lease reconcile and repair pass.
	TriggerReconcile Trigger = "reconcile"
	// TriggerOnLending is the companion claim's Prepare passing the
	// companion gate.
	TriggerOnLending Trigger = "lending"
	// TriggerOnPrepared is the companion claim's Prepare handing over
	// the lent devices.
	TriggerOnPrepared Trigger = "prepared"
	// TriggerOnLendFailed is a bind or lend error of the companion
	// claim's Prepare, or an error that its retries cannot heal.
	TriggerOnLendFailed Trigger = "lend-failed"
	// TriggerOnReleasing is the companion claim's Unprepare.
	TriggerOnReleasing Trigger = "releasing"
)

// Triggers are all triggers, in the order of the documentation.
var Triggers = []Trigger{
	TriggerPrepare, TriggerUnprepare, TriggerRecover, TriggerLoad, TriggerObserve, TriggerList,
	TriggerExpire, TriggerRequest, TriggerRelease, TriggerReconcile, TriggerOnLending,
	TriggerOnPrepared, TriggerOnLendFailed, TriggerOnReleasing,
}
