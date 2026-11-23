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

package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// testCLI returns a cli and its output buffers.
func testCLI() (*cli, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return &cli{stdout: &stdout, stderr: &stderr}, &stdout, &stderr
}

func TestUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		err  string
	}{
		{args: nil, code: exitUsage, err: "no command"},
		{args: []string{"here"}, code: exitUsage, err: `unknown command "here"`},
		{args: []string{"where"}, code: exitUsage, err: "where: no FILE"},
		{args: []string{"where", "-o", "yaml", "f"}, code: exitUsage, err: `-o "yaml": the output is table or json`},
		{args: []string{"where", "--nodes", "5", "f"}, code: exitUsage, err: "flag provided but not defined: -nodes"},
		{args: []string{"move", "f"}, code: exitUsage, err: `unknown command "move"`},
		{args: []string{"--help"}, code: exitOK},
		{args: []string{"where", "-h"}, code: exitOK},
	} {
		c, stdout, stderr := testCLI()
		code := c.run(tc.args)
		if code != tc.code || !strings.Contains(stderr.String(), tc.err) {
			t.Errorf("%v: exit %d, stderr %q, want exit %d and %q", tc.args, code, stderr.String(), tc.code, tc.err)
		}
		if tc.code == exitOK && !strings.HasPrefix(stdout.String(), "Usage: cxl-numa-place") {
			t.Errorf("%v: stdout %q, want the usage", tc.args, stdout.String())
		}
	}
}

func TestWhereCommand(t *testing.T) {
	path := sparseFile(t)
	whereOrSkip(t, path)
	c, stdout, stderr := testCLI()
	if code := c.run([]string{"where", path, "-o", "json"}); code != exitOK {
		t.Fatalf("where -o json: exit %d, stderr %q", code, stderr.String())
	}
	var ps []*Placement
	if err := json.Unmarshal(stdout.Bytes(), &ps); err != nil || len(ps) != 1 || ps[0].File != path {
		t.Fatalf("where -o json: %v, output %s", err, stdout.String())
	}
	stdout.Reset()
	if code := c.run([]string{"where", path, path}); code != exitOK {
		t.Fatalf("where: exit %d, stderr %q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "FILE") || !strings.HasPrefix(lines[1], path) {
		t.Errorf("where of a file twice: %q", stdout.String())
	}
	c, _, stderr = testCLI()
	if code := c.run([]string{"where", filepath.Join(t.TempDir(), "missing")}); code != exitError || !strings.Contains(stderr.String(), "no such file") {
		t.Errorf("where of a missing file: exit %d, stderr %q", code, stderr.String())
	}
}
