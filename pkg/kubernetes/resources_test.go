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

package kubernetes_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/containers/nri-plugins/pkg/kubernetes"
)

func TestCalculateOomAdjToMemReqEstimates(t *testing.T) {
	const (
		K int64 = 1024
		M int64 = 1024 * K
		G int64 = 1024 * M
	)

	for capacity := int64(4 * G); capacity <= (1024+512)*G; capacity += 4 * G {
		SetMemoryCapacity(capacity)
		for adj := int64(MinBurstableOOMScoreAdj); adj <= MaxBurstableOOMScoreAdj; adj++ {
			req := OomAdjToMemReq(adj, capacity)
			require.NotNil(t, req,
				"capacity %d, adj %d: OomAdjToMemReq() returned nil", capacity, adj)

			chk := MemReqToOomAdj(*req)
			require.Equal(t, adj, chk,
				"capacity %d, adj %d: req=%d, chk=%d != adj\n", capacity, adj, *req, chk)
		}
	}
}
