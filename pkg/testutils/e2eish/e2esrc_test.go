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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const e2eSrcTest = `package x

// TestX checks things.
func TestX(t *testing.T) {
	s := harness.NewE2EScript(t, plugin, "n4c16/test50-x")
	s.Launch(topo, base)
	s.CreateGuaranteed(harness.PodSpec{})
	require.Len(t, s.CPUs("pod0c0"), 1)
	s.VerifyPy("len(cpus) == 1")
	s.Terminate()
}

func TestLoop(t *testing.T) {
	s := harness.NewE2EScript(t, plugin, "n4c16/test51-x")
	for range 2 {
		s.Terminate()
	}
}

func TestLead(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	s := harness.NewE2EScript(t, plugin, "n4c16/test53-x")
	s.Terminate()
}

func TestHarness(t *testing.T) {
	s := harness.NewE2EScript(t, plugin, "n4c16/test52-x")
	s.Harness()
}
`

func writeSrc(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "x_test.go")
	require.NoError(t, os.WriteFile(path, []byte(e2eSrcTest), 0o644))
	return path
}

func TestE2ESourceMerge(t *testing.T) {
	w := pyWidths{cpu: 2, node: 1, pkg: 1}
	doc, stmts, err := e2eSource(writeSrc(t), "TestX", w)
	require.NoError(t, err)
	require.Equal(t, "TestX checks things.\n", doc)
	lines, err := mergeE2E(stmts, []e2eRec{
		{line: 6, cmds: []string{"launch"}},
		{line: 7, cmds: []string{"create"}},
		{line: 9, verify: "len(cpus) == 1"},
		{line: 10, cmds: []string{"helm-terminate"}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{
		"launch",
		"create",
		"verify \\",
		`    'len(cpus["pod0c0"]) == 1' \`,
		`    'len(cpus) == 1'`,
		"helm-terminate",
	}, lines)

	require.Equal(t, "3 commands, 2 assertions in 1 verify", e2eSummary(lines))
	require.Equal(t, "1 command, 0 assertions in 0 verifies", e2eSummary([]string{"helm-terminate"}))

	_, err = mergeE2E(stmts, []e2eRec{{line: 3, cmds: []string{"x"}}})
	require.ErrorContains(t, err, "outside")
}

func TestE2ESourceRefuses(t *testing.T) {
	w := pyWidths{cpu: 2, node: 1, pkg: 1}
	path := writeSrc(t)
	_, _, err := e2eSource(path, "TestLoop", w)
	require.ErrorContains(t, err, "x_test.go:15")
	_, stmts, err := e2eSource(path, "TestLead", w)
	require.NoError(t, err, "statements before NewE2EScript")
	require.Len(t, stmts, 1)
	_, _, err = e2eSource(path, "TestHarness", w)
	require.ErrorContains(t, err, "Harness")
	_, _, err = e2eSource(path, "TestNone", w)
	require.Error(t, err)
}

func TestWriteE2E(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "test50-x")
	files := map[string]string{"code.var.sh": "a\n"}
	writeE2E(t, dir, files, "")
	data, err := os.ReadFile(filepath.Join(dir, "code.var.sh"))
	require.NoError(t, err)
	require.Equal(t, "a\n", string(data))

	info, err := os.Stat(filepath.Join(dir, "code.var.sh"))
	require.NoError(t, err)
	writeE2E(t, dir, files, "") // up to date, left alone
	again, err := os.Stat(filepath.Join(dir, "code.var.sh"))
	require.NoError(t, err)
	require.Equal(t, info.ModTime(), again.ModTime())

	ft := &errT{}
	writeE2E(ft, dir, map[string]string{"code.var.sh": "b\n"}, "")
	require.True(t, ft.failed)
	data, _ = os.ReadFile(filepath.Join(dir, "code.var.sh"))
	require.Equal(t, "a\n", string(data))

	t.Setenv("E2EISH_EMIT_FORCE", "1")
	writeE2E(t, dir, map[string]string{"code.var.sh": "b\n"}, "")
	data, _ = os.ReadFile(filepath.Join(dir, "code.var.sh"))
	require.Equal(t, "b\n", string(data))
}

type errT struct{ failed bool }

func (*errT) Helper()                 {}
func (*errT) Logf(string, ...any)     {}
func (f *errT) Errorf(string, ...any) { f.failed = true }

func TestMergeE2ERefuses(t *testing.T) {
	w := pyWidths{cpu: 2, node: 1, pkg: 1}
	_, stmts, err := e2eSource(writeSrc(t), "TestX", w)
	require.NoError(t, err)
	_, err = mergeE2E(stmts, []e2eRec{{line: 9, verify: `len(cpus["a\\n"]) == 1`}})
	require.ErrorContains(t, err, "backslash")
	_, err = mergeE2E(stmts, []e2eRec{{line: 7, cmds: []string{"create"}}, {line: 9, consts: true}})
	require.ErrorContains(t, err, "py_consts")
}
