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
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/containers/nri-plugins/pkg/resmgr/lib/memory"
)

func TestMemsetString(t *testing.T) {
	type testCase struct {
		name   string
		mask   NodeMask
		result string
	}
	for _, tc := range []*testCase{
		{
			name:   "empty mask",
			mask:   0,
			result: "",
		},
		{
			name:   "singe node mask",
			mask:   NewNodeMask(0),
			result: "0",
		},
		{
			name:   "mask without ranges",
			mask:   NewNodeMask(0, 2, 4, 6, 8, 11, 17),
			result: "0,2,4,6,8,11,17",
		},
		{
			name:   "single range of 2 mask",
			mask:   NewNodeMask(0, 1),
			result: "0-1",
		},
		{
			name:   "multiple ranges of 2 masks",
			mask:   NewNodeMask(0, 1, 3, 4, 6, 7, 11, 12, 16, 17),
			result: "0-1,3-4,6-7,11-12,16-17",
		},
		{
			name:   "single range mask",
			mask:   NewNodeMask(0, 1, 2),
			result: "0-2",
		},
		{
			name:   "single range mask",
			mask:   NewNodeMask(0, 1, 2, 3, 4, 5, 6),
			result: "0-6",
		},
		{
			name:   "multiple ranges mask",
			mask:   NewNodeMask(0, 1, 2, 5, 6, 7, 9, 10, 11, 17, 18, 19),
			result: "0-2,5-7,9-11,17-19",
		},
		{
			name:   "multiple ranges of two and others mask",
			mask:   NewNodeMask(0, 1, 2, 5, 6, 9, 10, 12, 15, 16, 17, 18, 20, 21, 22, 23, 24, 25, 26, 28, 30, 31, 32, 40, 41, 42),
			result: "0-2,5-6,9-10,12,15-18,20-26,28,30-32,40-42",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.mask.MemsetString()
			require.Equal(t, tc.result, result)
		})
	}
}
