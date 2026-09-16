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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// artifacts are what a test case packs up for itself, the way the framework
// packs them.
var testArtifacts = map[string]string{
	"commands/0001-vm":               "a command\n",
	"commands/0002-host":             "another command\n",
	"nri-resource-policy.output.txt": "the log of the plugin\n",
}

// packArtifacts packs the artifacts of a test case the way the runner does,
// with the tool named by the suffix of the tarball.
func packArtifacts(t *testing.T, dir, name string) {
	t.Helper()

	flag := map[string]string{".tar.xz": "-cJf", ".tar.gz": "-czf", ".tar": "-cf"}
	for suffix, f := range flag {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		for file, data := range testArtifacts {
			path := filepath.Join(dir, "artifacts", file)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("tar", f, filepath.Join(dir, name),
			"-C", filepath.Join(dir, "artifacts"), ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to pack %s: %v: %s", name, err, out)
		}
		if err := os.RemoveAll(filepath.Join(dir, "artifacts")); err != nil {
			t.Fatal(err)
		}
		return
	}

	t.Fatalf("no idea how to pack %s", name)
}

func TestUnpacker(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"artifacts.tar.xz", true},
		{"results.tar.zst", true},
		{"artifacts.tar.gz", true},
		{"artifacts.tgz", true},
		{"artifacts.tar", true},
		{"run.sh.output.txt", false},
		{"commands", false},
		{"artifacts.zip", false},
		{"tar.xz.txt", false},
	} {
		if got := isTarball(tc.name); got != tc.ok {
			t.Errorf("isTarball(%q) is %v, expected %v", tc.name, got, tc.ok)
		}
	}
}

// TestServeIntoTarball checks that the artifacts a test packed up are browsable
// as well as downloadable, whether the run they belong to is packed or not.
func TestServeIntoTarball(t *testing.T) {
	for _, name := range []string{"artifacts.tar.xz", "artifacts.tar.gz", "artifacts.tar"} {
		for _, packed := range []bool{false, true} {
			t.Run(name+map[bool]string{true: "/packed", false: "/as-is"}[packed], func(t *testing.T) {
				root, run := newRoot(t, false)
				test := "vm/" + suiteDir + "/balloons/test01"
				packArtifacts(t, filepath.Join(root, run, test), name)
				if packed {
					if err := packRun(filepath.Join(root, run)); err != nil {
						t.Fatal(err)
					}
				}

				base := "/" + run + "/" + test + "/" + name

				// The tarball itself is downloaded, contents and all.
				got := get(t, root, base)
				if got.Code != http.StatusOK || got.Body.Len() == 0 {
					t.Errorf("GET %s: %d, %d bytes", base, got.Code, got.Body.Len())
				}
				if kind := got.Header().Get("Content-Type"); strings.HasPrefix(kind, "text/html") {
					t.Errorf("GET %s: content type %q", base, kind)
				}

				// With a trailing slash it is browsed into instead.
				got = get(t, root, base+"/")
				if got.Code != http.StatusOK {
					t.Fatalf("GET %s/: %d", base, got.Code)
				}
				for _, expected := range []string{"commands/", "nri-resource-policy.output.txt"} {
					if !strings.Contains(got.Body.String(), ">"+expected+"<") {
						t.Errorf("the listing of %s does not name %s", base, expected)
					}
				}

				// And every file in it is served from it.
				for file, data := range testArtifacts {
					got := get(t, root, base+"/"+file)
					if got.Code != http.StatusOK || got.Body.String() != data {
						t.Errorf("GET %s/%s: %d %q, expected 200 %q",
							base, file, got.Code, got.Body, data)
					}
				}

				// A directory in it lists, and redirects to itself with a slash.
				if got := get(t, root, base+"/commands"); got.Code != http.StatusMovedPermanently {
					t.Errorf("GET %s/commands: %d, expected 301", base, got.Code)
				}
				got = get(t, root, base+"/commands/")
				if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "0001-vm") {
					t.Errorf("GET %s/commands/: %d, %q", base, got.Code, got.Body)
				}

				// Nothing else is in it.
				if got := get(t, root, base+"/no/such/file"); got.Code != http.StatusNotFound {
					t.Errorf("GET %s/no/such/file: %d, expected 404", base, got.Code)
				}
			})
		}
	}
}

// TestServeIntoRunArchive checks that the archive of a run is browsable as
// well, and downloadable as it is.
func TestServeIntoRunArchive(t *testing.T) {
	root, run := newRoot(t, true)

	got := get(t, root, "/"+run+"/"+runTar)
	if got.Code != http.StatusOK || got.Body.Len() == 0 {
		t.Errorf("GET the archive of a run: %d, %d bytes", got.Code, got.Body.Len())
	}

	got = get(t, root, "/"+run+"/"+runTar+"/")
	if got.Code != http.StatusOK {
		t.Fatalf("GET into the archive of a run: %d", got.Code)
	}
	if !strings.Contains(got.Body.String(), ">vm/<") {
		t.Errorf("the listing of the archive does not name vm/: %q", got.Body)
	}

	member := runnerLog
	if got := get(t, root, "/"+run+"/"+runTar+"/"+member); got.Body.String() != files[member] {
		t.Errorf("GET %s from the archive of a run: %q", member, got.Body)
	}
}

// TestServeListsTarballBothWays checks that a listing offers a tarball to
// download and to browse into.
func TestServeListsTarballBothWays(t *testing.T) {
	root, run := newRoot(t, false)
	test := "vm/" + suiteDir + "/balloons/test01"
	packArtifacts(t, filepath.Join(root, run, test), "artifacts.tar.xz")

	got := get(t, root, "/"+run+"/"+test+"/")
	for _, expected := range []string{
		`href="artifacts.tar.xz"`,
		`href="artifacts.tar.xz/"`,
	} {
		if !strings.Contains(got.Body.String(), expected) {
			t.Errorf("the listing has no %s", expected)
		}
	}
}

// TestServeWithoutXz checks that a host without xz still serves the tarball of
// a test, it just cannot serve into it.
func TestServeWithoutXz(t *testing.T) {
	root, run := newRoot(t, false)
	test := "vm/" + suiteDir + "/balloons/test01"
	packArtifacts(t, filepath.Join(root, run, test), "artifacts.tar.xz")

	t.Setenv("PATH", "")

	base := "/" + run + "/" + test + "/artifacts.tar.xz"
	for _, path := range []string{base, base + "/"} {
		got := get(t, root, path)
		if got.Code != http.StatusOK || got.Body.Len() == 0 {
			t.Errorf("GET %s: %d, %d bytes", path, got.Code, got.Body.Len())
		}
	}
	if got := get(t, root, base+"/commands/0001-vm"); got.Code != http.StatusNotFound {
		t.Errorf("GET into the tarball without xz: %d, expected 404", got.Code)
	}
}

func TestBrowsable(t *testing.T) {
	for _, tc := range []struct {
		names  []string
		listed []string
	}{
		{
			[]string{"a.txt", "artifacts.tar.xz", "commands/"},
			[]string{"a.txt", "artifacts.tar.xz", "artifacts.tar.xz/", "commands/"},
		},
		{
			// The same name from where it is and from the archive of the run.
			[]string{"commands/", "a.txt", "commands/"},
			[]string{"a.txt", "commands/"},
		},
	} {
		if got := browsable(tc.names); !slices.Equal(got, tc.listed) {
			t.Errorf("browsable(%v) is %v, expected %v", tc.names, got, tc.listed)
		}
	}
}
