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

package policy

// SchedulingClass specifies the default Linux scheduling and IO
// priority parameters for containers assigned into this class.
// +k8s:deepcopy-gen=true
type SchedulingClass struct {
	// Name of the scheduling class.
	// +kube:validation:Required
	Name string `json:"name"`
	// Policy is the Linux scheduling policy to use.
	// SCHED_<NAME> translates to <name> etc.
	// +kubebuilder:validation:Enum=none;other;fifo;rr;batch;iso;idle;deadline
	// +kubebuilder:validation:Format:string
	Policy SchedulingPolicy `json:"policy,omitempty"`
	// Priority is the Linux scheduling priority to use.
	// Valid range depends on the selected policy.
	// Refer to sched_setscheduler(2) for details.
	Priority *int `json:"priority,omitempty"`
	// Flags is a list of Linux scheduling flags to set.
	// SCHED_FLAG_<ORIG_NAME> translates to <orig-name> etc.
	// +kubebuilder:validation:Enum=reset-on-fork;reclaim;dl-overrun;keep-policy;keep-params;util-clamp-min;util-clamp-max
	Flags SchedulingFlags `json:"flags,omitempty"`
	// Nice is the Linux nice value to use.
	Nice *int `json:"nice,omitempty"`
	// Runtime is the Linux SCHED_DEADLINE runtime value to use (in microseconds).
	Runtime *uint64 `json:"runtime,omitempty"`
	// Deadline is the Linux SCHED_DEADLINE deadline value to use (in microseconds).
	Deadline *uint64 `json:"deadline,omitempty"`
	// Period is the Linux SCHED_DEADLINE period value to use (in microseconds).
	Period *uint64 `json:"period,omitempty"`
	// IOClass is the IO scheduling class to use.
	// IOPRIO_CLASS_<NAME> translates to <name>.
	// Refer to ioprio_set(2) and ionice(1) for details.
	// +kubebuilder:validation:Enum=none;rt;be;idle
	IOClass IOPriorityClass `json:"ioClass,omitempty"`
	// IOPriority is the IO priority within the selected IO class to use.
	// Valid range depends on the selected class.
	// Refer to ionice(1) for details.
	IOPriority *int `json:"ioPriority,omitempty"`
}
