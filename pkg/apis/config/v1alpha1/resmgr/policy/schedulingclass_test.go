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

import (
	"testing"
)

// The scheduling class constants are user-facing strings which NRI must
// recognize: a constant which does not convert cannot be used at all, and
// neither can the CRD enum value generated from it.

func TestSchedulingPolicyToNRI(t *testing.T) {
	cases := []struct {
		policy SchedulingPolicy
		want   string
	}{
		{SchedulingPolicyNone, "SCHED_NONE"},
		{SchedulingPolicyOther, "SCHED_OTHER"},
		{SchedulingPolicyFifo, "SCHED_FIFO"},
		{SchedulingPolicyRr, "SCHED_RR"},
		{SchedulingPolicyBatch, "SCHED_BATCH"},
		{SchedulingPolicyIdle, "SCHED_IDLE"},
		{SchedulingPolicyDeadline, "SCHED_DEADLINE"},
	}
	for _, tc := range cases {
		t.Run(tc.policy.String(), func(t *testing.T) {
			got, err := tc.policy.ToNRI()
			if err != nil {
				t.Fatalf("failed to convert scheduling policy %q: %v", tc.policy, err)
			}
			if got.String() != tc.want {
				t.Errorf("scheduling policy %q: expected %s, got %s", tc.policy, tc.want, got)
			}
		})
	}
}

func TestSchedulingFlagToNRI(t *testing.T) {
	cases := []struct {
		flag SchedulingFlag
		want string
	}{
		{SchedulingFlagResetOnFork, "SCHED_FLAG_RESET_ON_FORK"},
		{SchedulingFlagReclaim, "SCHED_FLAG_RECLAIM"},
		{SchedulingFlagDlOverrun, "SCHED_FLAG_DL_OVERRUN"},
		{SchedulingFlagKeepPolicy, "SCHED_FLAG_KEEP_POLICY"},
		{SchedulingFlagKeepParams, "SCHED_FLAG_KEEP_PARAMS"},
		{SchedulingFlagUtilClampMin, "SCHED_FLAG_UTIL_CLAMP_MIN"},
		{SchedulingFlagUtilClampMax, "SCHED_FLAG_UTIL_CLAMP_MAX"},
	}
	for _, tc := range cases {
		t.Run(tc.flag.String(), func(t *testing.T) {
			got, err := tc.flag.ToNRI()
			if err != nil {
				t.Fatalf("failed to convert scheduling flag %q: %v", tc.flag, err)
			}
			if got.String() != tc.want {
				t.Errorf("scheduling flag %q: expected %s, got %s", tc.flag, tc.want, got)
			}
		})
	}
}

func TestIOPriorityClassToNRI(t *testing.T) {
	cases := []struct {
		ioClass IOPriorityClass
		want    string
	}{
		{IOPriorityClassNone, "IOPRIO_CLASS_NONE"},
		{IOPriorityClassRt, "IOPRIO_CLASS_RT"},
		{IOPriorityClassBe, "IOPRIO_CLASS_BE"},
		{IOPriorityClassIdle, "IOPRIO_CLASS_IDLE"},
	}
	for _, tc := range cases {
		t.Run(tc.ioClass.String(), func(t *testing.T) {
			got, err := tc.ioClass.ToNRI()
			if err != nil {
				t.Fatalf("failed to convert IO priority class %q: %v", tc.ioClass, err)
			}
			if got.String() != tc.want {
				t.Errorf("IO priority class %q: expected %s, got %s", tc.ioClass, tc.want, got)
			}
		})
	}
}
