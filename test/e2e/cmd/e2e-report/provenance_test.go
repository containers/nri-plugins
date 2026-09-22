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
	"strings"
	"testing"
)

// TestVersionTextSaysTheBranch checks that a run says which branch it tested,
// beside the revision it tested of it.
func TestVersionTextSaysTheBranch(t *testing.T) {
	for _, tc := range []struct{ describe, branch, want string }{
		{"v0.13.1-172-g6cf8fafd", "main", "v0.13.1-172-g6cf8fafd (main)"},
		{"v0.13.1-172-g6cf8fafd", "", "v0.13.1-172-g6cf8fafd"},
		// A run of a tree with no tags in it still says where it came from.
		{"", "release-0.13", "release-0.13"},
		{"", "", ""},
	} {
		run := &Run{Git: Git{Describe: tc.describe, Branch: tc.branch}}
		if got := versionText(run); got != tc.want {
			t.Errorf("describe %q, branch %q: %q, expected %q",
				tc.describe, tc.branch, got, tc.want)
		}
	}
}

// TestRunMetaSaysTheRunnerWhenItDiffers checks that the tree the runner came
// from is reported when it is not the tree under test, and passed over when it
// is: the runner re-execs itself out of the worktree it creates, so on a normal
// run the two are the same and saying so would be noise.
func TestRunMetaSaysTheRunnerWhenItDiffers(t *testing.T) {
	tested := "6cf8fafdfeedfacedeadbeef0123456789abcdef"
	elsewhere := "4f2a1c9bfeedfacedeadbeef0123456789abcdef"

	same := metaText(runMeta(&Run{Git: Git{SHA1: tested, Runner: tested}}))
	if strings.Contains(same, "runner 6cf8fafd") {
		t.Errorf("a run whose runner was the tree it tested says so: %q", same)
	}

	other := metaText(runMeta(&Run{
		Git: Git{
			SHA1:   tested,
			Runner: elsewhere,
			Remote: "https://github.com/containers/nri-plugins",
			Web:    "https://github.com/containers/nri-plugins",
		},
	}))
	if !strings.Contains(other, "runner 4f2a1c9b") {
		t.Errorf("a run driven from another tree does not say so: %q", other)
	}

	// And it links where the revision came from, so it can be looked at.
	for _, link := range runMeta(&Run{Git: Git{SHA1: tested, Runner: elsewhere,
		Web: "https://github.com/containers/nri-plugins"}}) {
		if strings.HasPrefix(link.Text, "runner ") {
			want := "https://github.com/containers/nri-plugins/commit/" + elsewhere
			if link.Href != want {
				t.Errorf("the runner links %q, expected %q", link.Href, want)
			}
		}
	}
}

// TestShortSHA checks that a revision is shortened the way a version does it,
// and that something which is already short is left alone.
func TestShortSHA(t *testing.T) {
	for sha1, want := range map[string]string{
		"6cf8fafdfeedfacedeadbeef0123456789abcdef": "6cf8fafd",
		"6cf8fafd": "6cf8fafd",
		"6cf8":     "6cf8",
		"":         "",
	} {
		if got := shortSHA(sha1); got != want {
			t.Errorf("shortSHA(%q) = %q, expected %q", sha1, got, want)
		}
	}
}

// metaText is what a meta line says, for looking things up in.
func metaText(links []htmlLink) string {
	texts := make([]string, 0, len(links))
	for _, link := range links {
		texts = append(texts, link.Text)
	}

	return strings.Join(texts, " | ")
}

// TestScanRunReadsProvenance checks that what the runner records of where a run
// came from is read back.
func TestScanRunReadsProvenance(t *testing.T) {
	run, err := scanRun(newRun(t))
	if err != nil {
		t.Fatal(err)
	}

	if run.Git.Branch != "main" {
		t.Errorf("branch %q, expected main", run.Git.Branch)
	}
	if run.Git.Runner != "0123456789abcdef" {
		t.Errorf("runner %q, expected the sha1 the run recorded", run.Git.Runner)
	}
}
