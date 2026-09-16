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
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// run is a result directory with a bit of everything a run publishes in it.
var files = map[string]string{
	indexHTML:   "<html>the report</html>",
	resultsJSON: `{"name":"test-run"}`,
	statusTxt:   "PASS 1/1 tests passed\n",
	summaryTxt:  "vm:\n  + PASS balloons/test01\n",
	"git.sha1":  "0123456789abcdef\n",
	runnerLog:   "the log of the run\n",
	"vm/" + suiteDir + "/balloons/test01/" + testLog:       "test output\n",
	"vm/" + suiteDir + "/balloons/test01/commands/0001-vm": "a command\n",
	"vm/" + suiteDir + "/balloons/test01/" + summaryTxt:    "Test verdict: PASS\n",
	coverageDir + "/coverprofile":                          "mode: atomic\n",
}

func newRun(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestPackRun(t *testing.T) {
	dir := newRun(t)

	if err := packRun(dir); err != nil {
		t.Fatalf("failed to pack: %v", err)
	}

	// What it takes to tell how the run went stays where it is.
	for _, name := range unpacked {
		if _, ok := files[name]; !ok {
			continue
		}
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s was packed away", name)
		}
	}

	archive, err := openArchive(dir)
	if err != nil {
		t.Fatalf("failed to open the archive: %v", err)
	}

	for name, data := range files {
		packed := !slices.Contains(unpacked, name)
		if _, ok := archive.Size(name); ok != packed {
			t.Errorf("%s in the archive: %v, expected %v", name, ok, packed)
		}
		if !packed {
			continue
		}
		if exists(filepath.Join(dir, name)) {
			t.Errorf("%s was packed but left behind", name)
		}
		if got := read(t, archive, name); got != data {
			t.Errorf("%s reads %q, expected %q", name, got, data)
		}
	}

	// A second pass has nothing to pack and says so instead of packing the
	// archive into itself.
	if err := packRun(dir); err == nil {
		t.Errorf("packing an already packed run did not fail")
	}
}

func TestPackRunLeavesNoEmptyDirs(t *testing.T) {
	dir := newRun(t)

	if err := packRun(dir); err != nil {
		t.Fatalf("failed to pack: %v", err)
	}

	names, err := readDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range names {
		if isDir(filepath.Join(dir, name)) {
			t.Errorf("%s was left behind, empty", name)
		}
	}
}

func TestArchiveList(t *testing.T) {
	dir := newRun(t)
	if err := packRun(dir); err != nil {
		t.Fatalf("failed to pack: %v", err)
	}

	archive, err := openArchive(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		dir   string
		names []string
	}{
		{"", []string{coverageDir + "/", runnerLog, "vm/"}},
		{"vm", []string{suiteDir + "/"}},
		{"vm/" + suiteDir + "/balloons/test01", []string{"commands/", testLog, summaryTxt}},
		{"vm/" + suiteDir + "/balloons/test01/commands", []string{"0001-vm"}},
		{"nosuchdir", nil},
	} {
		got := archive.List(tc.dir)
		if !slices.Equal(got, tc.names) {
			t.Errorf("list of %q is %v, expected %v", tc.dir, got, tc.names)
		}
	}
}

// TestArchiveMember checks that we serve nothing but the regular files of an
// archive, whatever an archive we are pointed at holds.
func TestArchiveMember(t *testing.T) {
	for _, tc := range []struct {
		header tar.Header
		name   string
		ok     bool
	}{
		{tar.Header{Name: "a/b", Typeflag: tar.TypeReg}, "a/b", true},
		{tar.Header{Name: "./a/b", Typeflag: tar.TypeReg}, "a/b", true},
		{tar.Header{Name: "/a/b", Typeflag: tar.TypeReg}, "a/b", true},
		{tar.Header{Name: "../../etc/passwd", Typeflag: tar.TypeReg}, "etc/passwd", true},
		{tar.Header{Name: "a/../../b", Typeflag: tar.TypeReg}, "b", true},
		{tar.Header{Name: "a", Typeflag: tar.TypeSymlink}, "", false},
		{tar.Header{Name: "a", Typeflag: tar.TypeLink}, "", false},
		{tar.Header{Name: "a", Typeflag: tar.TypeDir}, "", false},
		{tar.Header{Name: "a", Typeflag: tar.TypeChar}, "", false},
		{tar.Header{Name: "/", Typeflag: tar.TypeReg}, "", false},
	} {
		name, ok := member(&tc.header)
		if name != tc.name || ok != tc.ok {
			t.Errorf("member %q (type %c) is (%q, %v), expected (%q, %v)",
				tc.header.Name, tc.header.Typeflag, name, ok, tc.name, tc.ok)
		}
	}
}

func TestArchiveOpenMissing(t *testing.T) {
	dir := newRun(t)
	if err := packRun(dir); err != nil {
		t.Fatalf("failed to pack: %v", err)
	}

	archive, err := openArchive(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := archive.Open("no/such/member"); err == nil {
		t.Errorf("opening a member which is not there did not fail")
	}
}

func TestOpenArchiveWithout(t *testing.T) {
	if _, err := openArchive(t.TempDir()); err == nil {
		t.Errorf("opening the archive of an unpacked run did not fail")
	}
}

// TestArchiveIgnoresOddMembers checks that a doctored archive gets us no
// further than the regular files in it.
func TestArchiveIgnoresOddMembers(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, filepath.Join(dir, runTar), []tar.Header{
		{Name: "sane", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644},
		{Name: "../escape", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777},
	})

	archive, err := openArchive(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got, expected := archive.names, []string{"escape", "sane"}; !slices.Equal(got, expected) {
		t.Errorf("members are %v, expected %v", got, expected)
	}
	if _, ok := archive.Size("../escape"); ok {
		t.Errorf("a member reaching out of the archive is served by that name")
	}
	if _, ok := archive.Size("link"); ok {
		t.Errorf("a symlink member is served")
	}
}

func writeArchive(t *testing.T, path string, headers []tar.Header) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	zw, err := zstd.NewWriter(file)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)

	for _, header := range headers {
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(make([]byte, header.Size)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, archive *Tarball, name string) string {
	t.Helper()

	file, err := archive.Open(name)
	if err != nil {
		t.Fatalf("failed to open %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("failed to read %s: %v", name, err)
	}

	return string(data)
}
