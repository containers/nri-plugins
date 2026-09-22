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

// TestArtifactsOfLinksPackedLogs checks that the logs of a test are linked even
// though they are not there to stat: the artifacts of a test are packed up
// before a run is reported on, so linking only what is lying about would leave
// the log of the plugin and the log of the runtime out of every report.
func TestArtifactsOfLinksPackedLogs(t *testing.T) {
	dir := t.TempDir()
	rel := "n4c16-fedora-43-containerd/" + suiteDir + "/balloons/test01"

	// Everything bulky packed away, which is what a published test case looks
	// like. The archive is never read for this, so it can be empty.
	for _, name := range []string{artifactsTar, testLog, summaryTxt} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	links, err := artifactsOf(dir, rel, ref("containerd"))
	if err != nil {
		t.Fatal(err)
	}

	// Where the log is, and nothing about how to read it: that is decided when
	// a report is rendered, so that an old run's report gains it too.
	for label, want := range map[string]string{
		"plugin log":  rel + "/" + artifactsTar + "/nri-resource-policy.output.txt",
		"runtime log": rel + "/" + artifactsTar + "/runtime.containerd.log.txt",
		"test log":    rel + "/" + testLog,
	} {
		if links[label] != want {
			t.Errorf("%s links %q, expected %q", label, links[label], want)
		}
	}
}

// TestArtifactsOfPrefersTheFiles checks that a log which is there is linked
// where it is, rather than inside an archive it is not in.
func TestArtifactsOfPrefersTheFiles(t *testing.T) {
	dir := t.TempDir()
	rel := "n4c16-fedora-43-containerd/" + suiteDir + "/balloons/test01"
	plugin := "nri-resource-policy.output.txt"

	for _, name := range []string{artifactsTar, plugin, "runtime.crio.log.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	links, err := artifactsOf(dir, rel, ref("crio"))
	if err != nil {
		t.Fatal(err)
	}

	if want := rel + "/" + plugin; links["plugin log"] != want {
		t.Errorf("plugin log links %q, expected %q", links["plugin log"], want)
	}
	if want := rel + "/runtime.crio.log.txt"; links["runtime log"] != want {
		t.Errorf("runtime log links %q, expected %q", links["runtime log"], want)
	}
}

// TestArtifactsOfLinksNothingUnpacked checks that a test case with nothing
// packed up and nothing lying about is linked to nothing at all, rather than to
// an archive which is not there.
func TestArtifactsOfLinksNothingUnpacked(t *testing.T) {
	dir := t.TempDir()

	links, err := artifactsOf(dir, "vm/"+suiteDir+"/balloons/test01", ref("containerd"))
	if err != nil {
		t.Fatal(err)
	}

	if len(links) != 0 {
		t.Errorf("links for a test case which collected nothing: %v", links)
	}
}

// prompt is a line of a runner log as the framework writes it: the context in a
// coloured prompt, then the command it is about to run in the VM.
func prompt(test, command string) string {
	return "\x1b[38;5;11mroot@vm " + test + ">\x1b[0m " + command + "\n"
}

// TestCurrentTest checks which test a run is taken to be in, which is what the
// last prompt of its log says.
func TestCurrentTest(t *testing.T) {
	for what, tc := range map[string]struct{ log, want string }{
		"one test": {
			prompt("balloons/test01-basic-placement", "kubectl get pods"),
			"balloons/test01-basic-placement",
		},
		"the last of several": {
			prompt("balloons/test01-basic-placement", "kubectl get pods") +
				"some output of it\n" +
				prompt("topology-aware/test22-isolcpus", "mkdir -p /etc/default") +
				"more output\n",
			"topology-aware/test22-isolcpus",
		},
		"nothing we recognise": {"just some output\nand more\n", ""},
		"an empty log":         {"", ""},
		// A prompt of somebody's own, with no context in it.
		"no context": {"\x1b[38;5;11mroot@vm>\x1b[0m uname -a\n", ""},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, runnerLog), []byte(tc.log), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := currentTest(dir); got != tc.want {
			t.Errorf("%s: %q, expected %q", what, got, tc.want)
		}
	}

	// A run with no log at all, which is a run which has not started.
	if got := currentTest(t.TempDir()); got != "" {
		t.Errorf("a run with no log is in %q", got)
	}
}

// TestCurrentTestBeyondTheTail checks that a test which printed more than the
// tail we read is still found: the whole log is read rather than reporting
// nothing.
func TestCurrentTestBeyondTheTail(t *testing.T) {
	dir := t.TempDir()
	log := prompt("balloons/test07-maxballoons", "kubectl logs pod0") +
		strings.Repeat("a line of output which says nothing\n", 4000)

	if int64(len(log)) <= promptTail {
		t.Fatalf("the log is %d bytes, which does not exceed the %d byte tail",
			len(log), promptTail)
	}
	if err := os.WriteFile(filepath.Join(dir, runnerLog), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := currentTest(dir); got != "balloons/test07-maxballoons" {
		t.Errorf("a prompt past the tail was not found: %q", got)
	}
}
