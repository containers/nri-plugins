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

// n4c16Widths are the id widths of n4c16: 16 CPUs, 4 nodes, 2 packages.
var n4c16Widths = pyWidths{cpu: 2, node: 1, pkg: 1}

func TestTranslatePy(t *testing.T) {
	for _, tc := range []struct{ py, golang string }{
		{`len(cpus["pod0c0"]) == 1`, `require.Len(t, s.CPUs("pod0c0"), 1)`},
		{`len(cpus["pod0c0"]) != 4`, `require.NotEqual(t, 4, len(s.CPUs("pod0c0")))`},
		{`len(nodes["pod0c0"]) > 1`, `require.Greater(t, len(s.Nodes("pod0c0")), 1)`},
		{`len(cpus["a"]) >= 2`, `require.GreaterOrEqual(t, len(s.CPUs("a")), 2)`},
		{`len(cpus["a"]) < 3`, `require.Less(t, len(s.CPUs("a")), 3)`},
		{`cpus["a"] == cpus["b"]`, `require.Equal(t, s.CPUs("a"), s.CPUs("b"))`},
		{`cpus["a"] == {"cpu04", "cpu05"}`, `require.Equal(t, s.CPUIDs(4, 5), s.CPUs("a"))`},
		{`{"cpu04"} == cpus["a"]`, `require.Equal(t, s.CPUIDs(4), s.CPUs("a"))`},
		// A set literal is a set: a repeated element is one element.
		{`cpus["a"] == {"cpu05", "cpu04", "cpu05"}`, `require.Equal(t, s.CPUIDs(4, 5), s.CPUs("a"))`},
		{`nodes["a"] == {"node1"}`, `require.Equal(t, s.NodeIDs(1), s.Nodes("a"))`},
		{`packages["a"] == {"package0"}`, `require.Equal(t, s.PackageIDs(0), s.Packages("a"))`},
		{`cores["a"] == {"node1/core0"}`, `require.Equal(t, harness.NewSet("node1/core0"), s.Cores("a"))`},
		{`mems["a"] == mems["b"]`, `require.Equal(t, s.Mems("a"), s.Mems("b"))`},
		{`packages["a"] != packages["b"]`, `require.NotEqual(t, s.Packages("a"), s.Packages("b"))`},
		{`disjoint_sets(cpus["a"], cpus["b"])`, `harness.RequireDisjoint(t, s.CPUs("a"), s.CPUs("b"))`},
		{"disjoint_sets(\n  cpus['a'],\n  cpus['b'])", `harness.RequireDisjoint(t, s.CPUs("a"), s.CPUs("b"))`},
		{`cpus["a"].isdisjoint({"cpu00", "cpu01"})`, `harness.RequireDisjoint(t, s.CPUs("a"), s.CPUIDs(0, 1))`},
		{`cpus["a"].issubset({"cpu00", "cpu01"})`, `require.Subset(t, s.CPUIDs(0, 1), s.CPUs("a"))`},
		{`nodes["a"].issubset({"node1",})`, `require.Subset(t, s.NodeIDs(1), s.Nodes("a"))`},
		{`cpus["a"].issubset(cpus["b"])`, `require.Subset(t, s.CPUs("b"), s.CPUs("a"))`},
		{`"node1" not in nodes["a"]`, `require.NotContains(t, s.Nodes("a"), "node1")`},
		{`"node1" in nodes["a"]`, `require.Contains(t, s.Nodes("a"), "node1")`},
		{`node_ids(nodes["a"]) == {2}`, `require.Equal(t, []int{2}, harness.IDs(s.Nodes("a")))`},
		{`cpu_ids(cpus["a"]) == {3, 1}`, `require.Equal(t, []int{1, 3}, harness.IDs(s.CPUs("a")))`},
		{`cpu_ids(cpus["a"]) == {1, 3, 1}`, `require.Equal(t, []int{1, 3}, harness.IDs(s.CPUs("a")))`},
		{`len(set.intersection(cpus["a"], {"cpu00", "cpu01"})) == 1`,
			`require.Len(t, harness.Intersection(s.CPUs("a"), s.CPUIDs(0, 1)), 1)`},
		{`len(cpus["a"].union(cpus["b"])) == 4`, `require.Len(t, harness.Union(s.CPUs("a"), s.CPUs("b")), 4)`},
		{`len(cpus["a"] - cpus["b"]) == 0`, `require.Len(t, harness.Difference(s.CPUs("a"), s.CPUs("b")), 0)`},
		{`cpus["a"] in [{"cpu00"}, {"cpu01"}]`,
			`require.Contains(t, []harness.Set{s.CPUIDs(0), s.CPUIDs(1)}, s.CPUs("a"))`},
		{`len(cpus["a"]) == 2 or len(cpus["a"]) == 3`,
			`require.True(t, len(s.CPUs("a")) == 2 || len(s.CPUs("a")) == 3, "len(cpus[\"a\"]) == 2 or len(cpus[\"a\"]) == 3")`},
		{`mems["a"] == mems["b"] or mems["a"] == mems["c"]`,
			`require.True(t, s.Mems("a").Equal(s.Mems("b")) || s.Mems("a").Equal(s.Mems("c")), "mems[\"a\"] == mems[\"b\"] or mems[\"a\"] == mems[\"c\"]")`},
		{`not disjoint_sets(cpus["a"], cpus["b"])`,
			`require.True(t, !harness.Disjoint(s.CPUs("a"), s.CPUs("b")), "not disjoint_sets(cpus[\"a\"], cpus[\"b\"])")`},
	} {
		got, err := translatePy(tc.py, n4c16Widths)
		require.NoError(t, err, tc.py)
		require.Equal(t, []string{tc.golang}, got, tc.py)
	}
}

func TestTranslatePyStatements(t *testing.T) {
	got, err := translatePy(`cpus["a"] == cpus["b"] == cpus["c"]`, n4c16Widths)
	require.NoError(t, err)
	require.Equal(t, []string{
		`require.Equal(t, s.CPUs("a"), s.CPUs("b"))`,
		`require.Equal(t, s.CPUs("a"), s.CPUs("c"))`,
	}, got)

	got, err = translatePy(`len(cpus["a"]) == 1 and nodes["a"] == {"node0"}`, n4c16Widths)
	require.NoError(t, err)
	require.Equal(t, []string{
		`require.Len(t, s.CPUs("a"), 1)`,
		`require.Equal(t, s.NodeIDs(0), s.Nodes("a"))`,
	}, got)

	// A chain of other operators than == holds only as a whole.
	for _, py := range []string{
		`len(nodes["a"]) == len(nodes["b"]) == 1`,
		`cpus["a"] == cpus["b"] != cpus["c"]`,
	} {
		got, err = translatePy(py, n4c16Widths)
		require.NoError(t, err, py)
		require.Len(t, got, 1, py)
		require.Contains(t, got[0], "require.True(t, ", py)
	}
}

func TestTranslatePyStaysPython(t *testing.T) {
	for _, py := range []string{
		`len(cpus["a"])`,                   // an int as a boolean
		`cpus["a"]`,                        // a set as a boolean
		`len(cpus["a"]) == 1 or cpus["b"]`, // a set as a boolean
		`local_mems("pod0c0", "dram")`,     // a py_consts helper
		`cpuset("0-3") == cpuset("4-7")`,   // a helper of a test
		`[len(mems[c]) for c in mems] == [1] * len(mems)`,
		`disjoint_sets(*[cpus[f"pod{i}c0"] for i in range(0, 3)])`,
		`allocations["pod0c0"] == 1`, // the policy cache
		`len(cpus[a]) == 1`,          // a name, not a string, as the key
		`cpus["a"] == 1`,             // a set and an int
		`len(cpus["a"]) == "1"`,      // an int and a string
		`cpus["a"] == {1}`,           // a set and ids
		`len(cpus["a"]) = 1`,         // a lone = is no comparison
		`len(cpus["a"]) ==`,          // a syntax error
		`len(cpus["a"]) == 1)`,       // trailing input
		`cpus["a"].pop()`,            // an unknown method
	} {
		_, err := translatePy(py, n4c16Widths)
		require.Error(t, err, py)
	}
}

func TestTranslatePyIDHelpers(t *testing.T) {
	for py, golang := range map[string]string{
		`node_ids(nodes["a"]) == {1}`:       `require.Equal(t, []int{1}, harness.IDs(s.Nodes("a")))`,
		`node_ids(mems["a"]) == {1}`:        `require.Equal(t, []int{1}, harness.IDs(s.Mems("a")))`,
		`package_ids(packages["a"]) == {0}`: `require.Equal(t, []int{0}, harness.IDs(s.Packages("a")))`,
	} {
		got, err := translatePy(py, n4c16Widths)
		require.NoError(t, err, py)
		require.Equal(t, []string{golang}, got, py)
	}
	// python gives tuples for composite names, and fails on other levels.
	for _, py := range []string{
		`core_ids(cores["a"]) == core_ids(cores["b"])`,
		`die_ids(dies["a"]) == {0}`,
		`thread_ids(threads["a"]) == {0}`,
		`node_ids(cores["a"]) == {1}`,
		`cpu_ids(nodes["a"]) == {1}`,
		`node_ids({"node1"}) == {1}`,
	} {
		_, err := translatePy(py, n4c16Widths)
		require.Error(t, err, py)
	}
}

func TestTranslatePyLiteralWidths(t *testing.T) {
	w := pyWidths{cpu: 3, node: 1, pkg: 1}
	for py, golang := range map[string]string{
		`cpus["a"] == {"cpu004"}`:         `require.Equal(t, s.CPUIDs(4), s.CPUs("a"))`,
		`cpus["a"] == {"cpu04"}`:          `require.Equal(t, harness.NewSet("cpu04"), s.CPUs("a"))`,
		`cpus["a"] == {"cpu004", "cpu4"}`: `require.Equal(t, harness.NewSet("cpu004", "cpu4"), s.CPUs("a"))`,
		`nodes["a"] == {"node1"}`:         `require.Equal(t, s.NodeIDs(1), s.Nodes("a"))`,
		`packages["a"] == {"package00"}`:  `require.Equal(t, harness.NewSet("package00"), s.Packages("a"))`,
	} {
		got, err := translatePy(py, w)
		require.NoError(t, err, py)
		require.Equal(t, []string{golang}, got, py)
	}
	// With no known widths, names stay verbatim.
	got, err := translatePy(`cpus["a"] == {"cpu04"}`, pyWidths{})
	require.NoError(t, err)
	require.Equal(t, []string{`require.Equal(t, harness.NewSet("cpu04"), s.CPUs("a"))`}, got)
}

func TestTranslatePyEscapes(t *testing.T) {
	for _, py := range []string{`cpus["a\nb"] == cpus["b"]`, `"node\x31" in nodes["a"]`} {
		_, err := translatePy(py, n4c16Widths)
		require.Error(t, err, py)
	}
}
