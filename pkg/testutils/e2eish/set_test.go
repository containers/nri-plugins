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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSet(t *testing.T) {
	a, b := NewSet("cpu01", "cpu02"), NewSet("cpu02", "cpu03")
	require.Len(t, a, 2)
	require.Equal(t, "{cpu01, cpu02}", a.String())
	require.Equal(t, NewSet("cpu01", "cpu02", "cpu03"), Union(a, b))
	require.Equal(t, NewSet("cpu02"), Intersection(a, b))
	require.Equal(t, NewSet("cpu01"), Difference(a, b))
	require.True(t, a.Has("cpu01"))
	require.True(t, NewSet("cpu02").IsSubset(a))
	require.True(t, a.Equal(NewSet("cpu02", "cpu01")))
	require.Subset(t, a, NewSet("cpu02"))
	require.Contains(t, a, "cpu01")
	require.False(t, Disjoint(a, b))
	require.True(t, Disjoint(a, NewSet("cpu03"), NewSet("cpu04")))
	require.Equal(t, []int{1, 2}, IDs(a))
	require.Empty(t, Union())
}

func TestIDs(t *testing.T) {
	require.Equal(t, []int{0, 1, 3}, IDs(NewSet("cpu03", "cpu00", "cpu01")))
	require.Equal(t, []int{1}, IDs(NewSet("node1")))
	require.Equal(t, []int{0, 10}, IDs(NewSet("package10", "package0")))
	require.Empty(t, IDs(NewSet()))
	// The *_ids helpers are only ever applied to cpus, nodes, mems and
	// packages, whose names have an id of their own. The composite names of
	// cores, threads and dies do not, and neither does a name with no number.
	require.PanicsWithValue(t, "e2eish: IDs of node1/core3, not a name ending in a number",
		func() { IDs(NewSet("node1/core3")) })
	require.Panics(t, func() { IDs(NewSet("package0/die0")) })
	require.Panics(t, func() { IDs(NewSet("node")) })
	require.Panics(t, func() { IDs(NewSet("cpu0a")) })
}

func TestRequireDisjoint(t *testing.T) {
	RequireDisjoint(t, NewSet("a"), NewSet("b"), NewSet("c"))
	mock := &recordingT{}
	RequireDisjoint(mock, NewSet("a", "b"), NewSet("c"), NewSet("b"))
	require.True(t, mock.failed)
	require.Contains(t, mock.msg, "share b")
}

// recordingT records a failed assertion instead of failing the test.
type recordingT struct {
	failed bool
	msg    string
}

func (r *recordingT) Errorf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}

func (r *recordingT) FailNow() {}
