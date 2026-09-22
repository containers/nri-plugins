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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishRun puts a run under root with results of its own, as the runner which
// published it recorded them.
func publishRun(t *testing.T, root, name string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.Rename(newRun(t), dir); err != nil {
		t.Fatal(err)
	}

	// What an older runner recorded, naming an artifact which was never there,
	// so that reporting on the run again is something a test can see.
	reported := `{"name":"` + name + `","verdict":"PASS",` +
		`"counts":{"PASS":1,"total":1},"tests":[{"name":"test01",` +
		`"path":"vm/` + suiteDir + `/balloons/test01","verdict":"PASS",` +
		`"links":{"artifacts":"once-upon-a-time.tar.xz"}}]}`
	if err := os.WriteFile(filepath.Join(dir, resultsJSON), []byte(reported), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func mustRead(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// TestRefreshReportsOnRunsAgain checks that refresh reports on every unpacked
// run again, which is how a run published by an older runner comes to record what
// one published today records, and that it leaves a packed run alone.
func TestRefreshReportsOnRunsAgain(t *testing.T) {
	root := t.TempDir()
	loose := publishRun(t, root, "test-2026-09-15-2113")
	packed := publishRun(t, root, "test-2026-09-16-0910")

	wasPacked := mustRead(t, filepath.Join(packed, resultsJSON))
	if err := packRun(packed); err != nil {
		t.Fatal(err)
	}

	if err := refreshRuns(root); err != nil {
		t.Fatal(err)
	}

	// The stale results named an artifact which was never there; results recorded
	// again name what the run actually collected.
	if now := mustRead(t, filepath.Join(loose, resultsJSON)); strings.Contains(now, "once-upon-a-time") {
		t.Errorf("an unpacked run was not reported on again")
	}

	// A packed run keeps what it was packed with: its tests are inside the
	// archive, so reporting on it again could only throw them away.
	if now := mustRead(t, filepath.Join(packed, resultsJSON)); now != wasPacked {
		t.Errorf("what a packed run was packed with was replaced")
	}
}

// TestReportRunWritesNoPage checks that reporting on a run records what the run
// collected and nothing else: what that is shown as is decided when it is served,
// so a report on disk is a copy which can only go stale.
func TestReportRunWritesNoPage(t *testing.T) {
	dir := newRun(t)
	if err := os.Remove(filepath.Join(dir, indexHTML)); err != nil {
		t.Fatal(err)
	}

	if err := reportRun(dir); err != nil {
		t.Fatal(err)
	}

	if exists(filepath.Join(dir, indexHTML)) {
		t.Errorf("%s was written", indexHTML)
	}
	for _, name := range []string{resultsJSON, statusTxt} {
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s was not written", name)
		}
	}
}
