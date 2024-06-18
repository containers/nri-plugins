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

package libmem_test

import (
	. "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestHumanReadableSize(t *testing.T) {
	type testCase struct {
		name   string
		size   int64
		result string
	}

	for _, tc := range []*testCase{
		{
			name:   "zero",
			size:   0,
			result: "0",
		},
		{
			name:   "no units",
			size:   345,
			result: "345",
		},
		{
			name:   "1k",
			size:   1024,
			result: "1k",
		},
		{
			name:   "2k",
			size:   2048,
			result: "2k",
		},
		{
			name:   "8k",
			size:   8192,
			result: "8k",
		},
		{
			name:   "2.5k",
			size:   2048 + 512,
			result: "2.5k",
		},
		{
			name:   "1M",
			size:   1024 * 1024,
			result: "1M",
		},
		{
			name:   "2.5M",
			size:   2*1024*1024 + 512*1024,
			result: "2.5M",
		},
		{
			name:   "1G",
			size:   1024 * 1024 * 1024,
			result: "1G",
		},
		{
			name:   "4.25G",
			size:   4*1024*1024*1024 + 256*1024*1024,
			result: "4.25G",
		},
		{
			name:   "2.75T",
			size:   2*1024*1024*1024*1024 + 768*1024*1024*1024,
			result: "2.75T",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.result, HumanReadableSize(tc.size))
		})
	}
}

func TestRequestString(t *testing.T) {
	type testCase struct {
		name   string
		aff    NodeMask
		req    *Request
		result string
	}

	for _, tc := range []*testCase{
		{
			name:   "best effort request #0 of 1M",
			req:    NewRequest("0", 0, NewNodeMask(1)),
			result: "besteffort workload<ID:#0 affine to nodes{1}>",
		},
		{
			name:   "burstable request #1 of 1M",
			req:    NewRequest("1", 1024*1024, NewNodeMask(2)),
			result: "burstable workload<ID:#1, size 1M, affine to nodes{2}>",
		},
		{
			name: "guaranteed request #2 of 2.75M",
			req: NewRequest("2", 2*1024*1024+768*1024, NewNodeMask(1),
				WithName("default/busybox/ctr0"),
				WithQosClass("Guaranteed"),
			),
			result: "guaranteed workload<default/busybox/ctr0, size 2.75M, affine to nodes{1}>",
		},
		{
			name: "preserved workload #3 of 1G",
			req: NewRequest("2", 1024*1024*1024, NewNodeMask(2),
				WithName("preserved container"),
				WithPriority(Preserved),
			),
			result: "preserved workload<preserved container, size 1G, affine to nodes{2}>",
		},
		{
			name:   "memory reservation #3 of 1.25G",
			req:    ReservedMemory(1024*1024*1024+256*1024*1024, NewNodeMask(0, 1), WithName("test")),
			result: "memory reservation<test, size 1.25G, affine to nodes{0-1}>",
		},
		{
			name: "priority 1234 workload #5 of 512M",
			req: NewRequest("5", 512*1024*1024, NewNodeMask(2, 5),
				WithName("default/pod/container"),
				WithPriority(1234),
			),
			result: "priority 1234 workload<default/pod/container, size 512M, affine to nodes{2,5}>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.result, tc.req.String())
		})
	}
}
