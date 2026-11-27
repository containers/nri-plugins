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
)

// ListedPool is a CXL memory pool whose ResourceSlices the driver listed
// with serials in them: the memdevs of a remembered pool whose slices are
// missing for less than an hour are held.
//
// Identity: Pool, <driver>/<pool>.
//
// States: none; the record exists from the first list with serials of the
// pool.
//
// Transitions: none.
//
// Invariants: a pool has at most one record.
//
// Persistence: fact; Listed committed with the provenance records.
//
// Locks and waits: the provenance watcher is the only writer.
type ListedPool struct {
	Pool string `json:"pool"`
	// Listed is the time of the last list whose slices of the pool had
	// serials.
	Listed time.Time `json:"listed"`
}
