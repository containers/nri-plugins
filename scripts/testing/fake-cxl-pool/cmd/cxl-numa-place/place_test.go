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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sparseFile returns the path of a new file of 4 MiB in a test directory
// whose second MiB is written, so in memory, and the rest a hole.
func sparseFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dev.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 << 20); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if _, err := f.WriteAt(data, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// residentPages returns the number of pages of path in memory.
func residentPages(t *testing.T, path string) int {
	t.Helper()
	m, err := openMap(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.close() }()
	pages, err := m.resident()
	if err != nil {
		t.Fatal(err)
	}
	return len(pages)
}

// whereOrSkip returns the placement of path, and skips the test if the
// host does not permit move_pages.
func whereOrSkip(t *testing.T, path string) *Placement {
	t.Helper()
	p, err := where(path)
	if errors.Is(err, errMovePagesDenied) {
		t.Skipf("cannot query the nodes of pages here: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWhereSparseFile(t *testing.T) {
	path := sparseFile(t)
	written := (1 << 20) / os.Getpagesize()
	if got := residentPages(t, path); got != written {
		t.Skipf("the file system of %s keeps %d pages of the file in memory, not the %d written", path, got, written)
	}
	p := whereOrSkip(t, path)
	if p.Size != 4<<20 || p.Pages != 4*written || p.Resident != written {
		t.Errorf("where = size %d, pages %d, resident %d, want %d, %d, %d", p.Size, p.Pages, p.Resident, 4<<20, 4*written, written)
	}
	if len(p.Unlocated) != 0 {
		t.Errorf("where: unlocated pages %v", p.Unlocated)
	}
	located := 0
	for _, count := range p.Nodes {
		located += count
	}
	if located != written {
		t.Errorf("where: %d pages on nodes %v, want %d", located, p.Nodes, written)
	}
	// The holes stay holes.
	if got := residentPages(t, path); got != written {
		t.Errorf("after where: %d pages in memory, want %d", got, written)
	}
}

func TestWhereEmptyAndNotRegular(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.raw")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := where(empty)
	if err != nil || p.Size != 0 || p.Pages != 0 || p.Resident != 0 || len(p.Nodes) != 0 {
		t.Errorf("where of an empty file = %+v, %v", p, err)
	}
	if _, err := where(dir); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("where of a directory: %v", err)
	}
	if _, err := where(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("where of a missing file: %v", err)
	}
}

func TestCheckResidencyVisible(t *testing.T) {
	if err := checkResidencyVisible(sparseFile(t)); err != nil {
		t.Errorf("checkResidencyVisible of an own file: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root sees the page cache of every file")
	}
	const other = "/etc/passwd"
	if st, err := os.Stat(other); err != nil || st.Mode().Perm()&0o022 != 0 {
		t.Skipf("no file of another owner without write permission: %s %v", other, err)
	}
	if err := checkResidencyVisible(other); err == nil || !strings.Contains(err.Error(), "run as root") {
		t.Errorf("checkResidencyVisible(%s): %v", other, err)
	}
}
