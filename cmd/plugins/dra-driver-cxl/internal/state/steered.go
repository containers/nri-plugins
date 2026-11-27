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

// Steered is the record of a container whose cgroup a steering manager of
// the driver writes. So the records of a process that was killed, or that
// exited on the loss of NRI, name the containers whose memory.high its
// managers left, and the next process releases exactly those, whatever
// their settings say now.
//
// Identity: Container, the container ID.
//
// States: none; the record exists from before the manager writes the
// cgroup until the manager has stopped.
//
// Transitions: none.
//
// Invariants: a container has at most one record.
//
// Persistence: intent; committed durably before the manager starts, on a
// small bounded commit path, and removed after it stopped.
//
// Locks and waits: the NRI plugin's table mutex.
type Steered struct {
	Container string `json:"container"`
	// CgroupDir is the cgroup directory of the container that the
	// manager writes.
	CgroupDir string `json:"cgroupDir"`
}
