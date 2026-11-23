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

import "time"

// Probe is the result of the address line probe of a local memdev whose
// memory aliases: offsets X and X+P reach the same memory, so only the
// first UsableSize bytes are distinct. The regions of the memdev are made
// with that size from then on, and the publisher publishes it.
//
// Identity: the serial of the memdev, the key of the file in hex without
// 0x.
//
// States: none: a fact about the hardware, recorded when a probe finds
// aliasing.
//
// Transitions: none; the driver never removes a probe, an operator removes
// the file to probe every memdev again.
//
// Invariants: UsableSize is a positive multiple of 256 MiB.
//
// Persistence: <plugin data dir>/localProbes.json, not the document: it
// survives a reboot and a new document, so that no start probes a memdev
// again.
//
// Locks and waits: written by the engine in the queue of the memdev.
type Probe struct {
	// UsableSize is the number of distinct bytes the probe found.
	UsableSize uint64 `json:"usableSize"`
	// Result describes the aliasing.
	Result string `json:"result"`
	// Region is the region that was probed.
	Region string `json:"region"`
	// Time is when the probe ran.
	Time time.Time `json:"time"`
}
