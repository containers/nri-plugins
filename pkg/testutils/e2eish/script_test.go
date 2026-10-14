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

package e2eish

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPyReport(t *testing.T) {
	require.Empty(t, (&Script{}).pyReport())
	require.Equal(t, "3 python assertions verified, 1 skipped", (&Script{verified: 3, skipped: 1}).pyReport())
	require.Equal(t, "1 python assertion verified, 0 skipped", (&Script{verified: 1}).pyReport())
}

func TestE2eVisible(t *testing.T) {
	require.NoError(t, e2eVisible("pod0c0"))
	require.NoError(t, e2eVisible("pod12c3"))
	require.Error(t, e2eVisible("webc0"))
	require.Error(t, e2eVisible("pod0c0i"))
}
