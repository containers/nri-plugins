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
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func revOf(t *testing.T, src string) (string, error) {
	t.Helper()
	e, err := parser.ParseExpr(src)
	require.NoError(t, err)
	return revTr{"s", "harness", pyWidths{cpu: 2, node: 1, pkg: 1}}.statement(e.(*ast.CallExpr))
}

func TestRevStatement(t *testing.T) {
	for _, tc := range []struct{ goSrc, py string }{
		{`require.Len(t, s.CPUs("pod0c0"), 1)`, `len(cpus["pod0c0"]) == 1`},
		{`require.GreaterOrEqual(t, len(s.Cores("c")), 2)`, `len(cores["c"]) >= 2`},
		{`require.Equal(t, s.CPUIDs(4, 5), s.CPUs("c"))`, `cpus["c"] == {"cpu04", "cpu05"}`},
		{`require.Equal(t, s.Nodes("a"), s.Nodes("b"))`, `nodes["a"] == nodes["b"]`},
		{`require.Equal(t, []int{1}, harness.IDs(s.Nodes("c")))`, `node_ids(nodes["c"]) == {1}`},
		{`require.NotEqual(t, 2, len(s.CPUs("c")))`, `len(cpus["c"]) != 2`},
		{`harness.RequireDisjoint(t, s.CPUs("a"), s.CPUs("b"))`, `disjoint_sets(cpus["a"], cpus["b"])`},
		{`require.Subset(t, s.Mems("a"), s.Mems("b"))`, `mems["b"].issubset(mems["a"])`},
		{`require.Contains(t, s.Nodes("a"), "node1")`, `"node1" in nodes["a"]`},
		{`require.Equal(t, harness.Union(s.CPUs("a"), s.CPUs("b")), s.CPUs("c"))`,
			`set.union(cpus["a"], cpus["b"]) == cpus["c"]`},
		{`require.Len(t, harness.Difference(s.CPUs("a"), s.CPUs("b")), 0)`, `len((cpus["a"] - cpus["b"])) == 0`},
		{`require.True(t, s.CPUs("a").Has("cpu01") || len(s.CPUs("b")) == 2, "x")`,
			`"cpu01" in cpus["a"] or len(cpus["b"]) == 2`},
		{`require.False(t, s.CPUs("a").Equal(s.CPUs("b")))`, `not (cpus["a"] == cpus["b"])`},
	} {
		py, err := revOf(t, tc.goSrc)
		require.NoError(t, err, tc.goSrc)
		require.Equal(t, tc.py, py, tc.goSrc)
	}
}

func TestRevRefuses(t *testing.T) {
	for _, src := range []string{
		`require.NoError(t, err)`,
		`require.Len(t, x, 1)`,
		`require.Len(t, s.CPUs(name), 1)`,
		`require.Equal(t, s.CPUIDs(), s.CPUs("c"))`,
		`fmt.Println("x")`,
	} {
		_, err := revOf(t, src)
		require.Error(t, err, src)
	}
}

// TestRevRoundTrip reverses every assertion of the generated tests, and
// translates it forward again.
func TestRevRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(srcDir(), "cmd/plugins/*/policy/e2eish-gen-*_test.go"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	name := regexp.MustCompile(`cmd/plugins/([^/]+)/policy/e2eish-gen-(.+)_test.go$`)
	count := 0
	for _, file := range files {
		m := name.FindStringSubmatch(file)
		topo, err := readTopology(filepath.Join(srcDir(), "test/e2e/policies.test-suite", m[1], m[2], "topology.var.json"))
		require.NoError(t, err)
		n := newTopoNames(NewMachine(t, topo, Host{}))
		tr := revTr{"s", "harness", pyWidths{cpu: n.cpuW, node: n.nodeW, pkg: n.pkgW}}

		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(node ast.Node) bool {
			stmt, ok := node.(*ast.ExprStmt)
			if !ok {
				return true
			}
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok || !isAssertion(call, tr.harness) {
				return true
			}
			goSrc := types.ExprString(call)
			py, err := tr.statement(call)
			require.NoError(t, err, goSrc)
			fwd, err := translatePy(py, tr.w)
			require.NoError(t, err, "%s: %s", goSrc, py)
			if len(fwd) > 1 {
				// A conjunction of require.True comes back as one
				// assertion for each term.
				var terms []string
				for _, f := range fwd {
					back, err := parser.ParseExpr(f)
					require.NoError(t, err, f)
					term, err := tr.statement(back.(*ast.CallExpr))
					require.NoError(t, err, f)
					terms = append(terms, term)
				}
				require.Equal(t, py, strings.Join(terms, " and "), goSrc)
				count++
				return true
			}
			require.Len(t, fwd, 1, "%s: %s", goSrc, py)
			back, err := parser.ParseExpr(fwd[0])
			require.NoError(t, err, fwd[0])
			require.Equal(t, normalizeAssert(call), normalizeAssert(back.(*ast.CallExpr)), py)
			count++
			return true
		})
	}
	t.Logf("%d assertions round-tripped", count)
}

// normalizeAssert drops the message of require.True and orders the values
// of Equal and NotEqual.
func normalizeAssert(c *ast.CallExpr) string {
	args := []string{}
	for _, a := range c.Args {
		args = append(args, types.ExprString(a))
	}
	switch fn := types.ExprString(c.Fun); fn {
	case "require.True":
		args = args[:2]
	case "require.Equal", "require.NotEqual":
		slices.Sort(args[1:])
	}
	return types.ExprString(c.Fun) + "(" + strings.Join(args, ", ") + ")"
}
