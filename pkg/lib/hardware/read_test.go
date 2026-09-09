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

package hardware

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"
)

// file is shorthand for one entry in a synthetic filesystem.
func file(contents string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(contents)}
}

func TestReadFile(t *testing.T) {
	fsys := fstest.MapFS{
		"plain":            file("value"),
		"newline":          file("value\n"),
		"many-newlines":    file("value\n\n\n"),
		"empty":            file(""),
		"only-newline":     file("\n"),
		"inner-whitespace": file("  spaced out  \n"),
		"multi-line":       file("first\nsecond\n"),
	}

	for _, tc := range []struct {
		name string
		want string
	}{
		{"plain", "value"},
		{"newline", "value"},
		{"many-newlines", "value"},
		{"empty", ""},
		{"only-newline", ""},
		// only newlines are trimmed, not other whitespace: a sysfs attribute
		// which contains spaces means them
		{"inner-whitespace", "  spaced out  "},
		{"multi-line", "first\nsecond"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readFile(fsys, tc.name)
			if err != nil {
				t.Fatalf("readFile(%q): %v", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("readFile(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		if _, err := readFile(fsys, "nope"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("readFile of a missing file: %v, want fs.ErrNotExist", err)
		}
	})
}

func TestReadInt(t *testing.T) {
	fsys := fstest.MapFS{
		"zero":     file("0\n"),
		"positive": file("42\n"),
		"negative": file("-1\n"),
		"hex":      file("0x10\n"),
		"padded":   file("007\n"),
		"empty":    file("\n"),
		"words":    file("not a number\n"),
		"float":    file("1.5\n"),
	}

	for _, tc := range []struct {
		name string
		want int
		fail bool
	}{
		{name: "zero", want: 0},
		{name: "positive", want: 42},
		// a die_id or cluster_id the kernel does not know reads as -1
		{name: "negative", want: -1},
		{name: "hex", want: 16},
		{name: "padded", want: 7},
		{name: "empty", fail: true},
		{name: "words", fail: true},
		{name: "float", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readInt(fsys, tc.name)
			switch {
			case tc.fail && err == nil:
				t.Errorf("readInt(%q) = %d, want an error", tc.name, got)
			case !tc.fail && err != nil:
				t.Errorf("readInt(%q): %v", tc.name, err)
			case !tc.fail && got != tc.want:
				t.Errorf("readInt(%q) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestReadUint64(t *testing.T) {
	fsys := fstest.MapFS{
		"zero":     file("0\n"),
		"freq":     file("2400000\n"),
		"big":      file("18446744073709551615\n"),
		"negative": file("-1\n"),
		"overflow": file("18446744073709551616\n"),
	}

	for _, tc := range []struct {
		name string
		want uint64
		fail bool
	}{
		{name: "zero", want: 0},
		{name: "freq", want: 2400000},
		{name: "big", want: 1<<64 - 1},
		{name: "negative", fail: true},
		{name: "overflow", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readUint64(fsys, tc.name)
			switch {
			case tc.fail && err == nil:
				t.Errorf("readUint64(%q) = %d, want an error", tc.name, got)
			case !tc.fail && err != nil:
				t.Errorf("readUint64(%q): %v", tc.name, err)
			case !tc.fail && got != tc.want:
				t.Errorf("readUint64(%q) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestReadCPUs(t *testing.T) {
	fsys := fstest.MapFS{
		"single":     file("0\n"),
		"range":      file("0-3\n"),
		"list":       file("0,2,4\n"),
		"mixed":      file("0-3,8,12-15\n"),
		"high":       file("960-1023\n"),
		"unordered":  file("3,1,0,2\n"),
		"empty":      file("\n"),
		"words":      file("nope\n"),
		"bad-range":  file("5-3\n"),
		"open-range": file("0-\n"),
	}

	for _, tc := range []struct {
		name string
		want []int
		fail bool
	}{
		{name: "single", want: []int{0}},
		{name: "range", want: []int{0, 1, 2, 3}},
		{name: "list", want: []int{0, 2, 4}},
		{name: "mixed", want: []int{0, 1, 2, 3, 8, 12, 13, 14, 15}},
		{name: "high", want: cpuRange(960, 1023)},
		{name: "unordered", want: []int{0, 1, 2, 3}},
		// an empty attribute is an empty set, not an error: isolated is empty on
		// most machines
		{name: "empty", want: []int{}},
		{name: "words", fail: true},
		{name: "bad-range", fail: true},
		{name: "open-range", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readCPUs(fsys, tc.name)
			if tc.fail {
				if err == nil {
					t.Errorf("readCPUs(%q) = %s, want an error", tc.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readCPUs(%q): %v", tc.name, err)
			}
			if !slices.Equal(got.List(), tc.want) {
				t.Errorf("readCPUs(%q) = %v, want %v", tc.name, got.List(), tc.want)
			}
			// every set discovery hands out has to be sealed
			assertSealed(t, got)
		})
	}
}

func TestReadInts(t *testing.T) {
	fsys := fstest.MapFS{
		"distance":   file("10 21\n"),
		"one":        file("10\n"),
		"four":       file("10 21 31 41\n"),
		"extra-gaps": file("10  21\n"),
		"words":      file("10 x\n"),
	}

	for _, tc := range []struct {
		name string
		want []int
		fail bool
	}{
		{name: "distance", want: []int{10, 21}},
		{name: "one", want: []int{10}},
		{name: "four", want: []int{10, 21, 31, 41}},
		// empty fields are skipped, so repeated separators are harmless
		{name: "extra-gaps", want: []int{10, 21}},
		{name: "words", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readInts(fsys, tc.name, " ")
			if tc.fail {
				if err == nil {
					t.Errorf("readInts(%q) = %v, want an error", tc.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readInts(%q): %v", tc.name, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("readInts(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestGlobIDs(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/devices/system/cpu/cpu0/x":      file(""),
		"sys/devices/system/cpu/cpu1/x":      file(""),
		"sys/devices/system/cpu/cpu2/x":      file(""),
		"sys/devices/system/cpu/cpu10/x":     file(""),
		"sys/devices/system/cpu/cpu11/x":     file(""),
		"sys/devices/system/cpu/cpu100/x":    file(""),
		"sys/devices/system/cpu/cpufreq/x":   file(""),
		"sys/devices/system/cpu/online":      file(""),
		"sys/devices/system/cpu/cpuidle/x":   file(""),
		"sys/devices/system/cpu/microcode/x": file(""),
	}

	names, ids, err := globIDs(fsys, "sys/devices/system/cpu/cpu[0-9]*")
	if err != nil {
		t.Fatalf("globIDs: %v", err)
	}

	// numeric order, not the lexical order fs.Glob returns: cpu2 before cpu10
	want := []ID{0, 1, 2, 10, 11, 100}
	if !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if len(names) != len(ids) {
		t.Fatalf("got %d names for %d ids", len(names), len(ids))
	}
	for i, name := range names {
		if got, _ := trailingID(name); got != ids[i] {
			t.Errorf("names[%d] = %q does not match ids[%d] = %d", i, name, i, ids[i])
		}
	}
}

func TestTrailingID(t *testing.T) {
	for _, tc := range []struct {
		name string
		want ID
		ok   bool
	}{
		{"cpu0", 0, true},
		{"cpu12", 12, true},
		{"node1", 1, true},
		{"index3", 3, true},
		{"sys/devices/system/cpu/cpu7", 7, true},
		{"cpu", 0, false},
		{"cpufreq", 0, false},
		{"", 0, false},
		{"sys/devices/system/cpu/online", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := trailingID(tc.name)
			if ok != tc.ok {
				t.Fatalf("trailingID(%q) ok = %v, want %v", tc.name, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("trailingID(%q) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestExists(t *testing.T) {
	fsys := fstest.MapFS{"a/b": file("")}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"a/b", true},
		{"a", true},
		{"a/c", false},
		{"b", false},
	} {
		if got := exists(fsys, tc.name); got != tc.want {
			t.Errorf("exists(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Machine.FS hands back the filesystem the machine was discovered from, so that
// a caller reading or writing more of the same tree cannot end up in a different
// one. Discovering from an injected fs.FS has to yield that fs.FS, not a
// filesystem rooted somewhere the machine knows nothing about.
func TestMachineFS(t *testing.T) {
	// a pointer, so that identity can be compared at all: fstest.MapFS is a map
	fsys := &wrappedFS{syntheticFS(1, true)}

	m, err := Discover(WithFS(fsys))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := m.FS(); got != fs.FS(fsys) {
		t.Errorf("FS() = %#v, want the injected one", got)
	}

	// the default is the real filesystem, which can be written through
	m, err = Discover(WithRoot(t.TempDir()))
	if err == nil {
		if _, ok := m.FS().(WriterFS); !ok {
			t.Error("the default filesystem is not a WriterFS")
		}
	}
}

// wrappedFS is an fs.FS which can be compared for identity.
type wrappedFS struct {
	fs.FS
}

// A read-only fs.FS is enough to discover with, so writing through one has to
// fail with something a caller can act on rather than panicking.
func TestWriteFileNeedsAWriterFS(t *testing.T) {
	err := writeFile(fstest.MapFS{"a": file("")}, "a", []byte("1"))
	if err == nil {
		t.Fatal("writing through a read-only fs.FS succeeded")
	}
	if got := err.Error(); !contains(got, "read-only") {
		t.Errorf("error %q does not say the filesystem is read-only", got)
	}
}

func TestHostFS(t *testing.T) {
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "sys", "devices"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "sys", "devices", "attr")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := HostFS(root)

	t.Run("read", func(t *testing.T) {
		got, err := readFile(h, "sys/devices/attr")
		if err != nil {
			t.Fatalf("readFile: %v", err)
		}
		if got != "original" {
			t.Errorf("readFile = %q, want %q", got, "original")
		}
	})

	t.Run("glob", func(t *testing.T) {
		names, err := glob(h, "sys/devices/*")
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		if !slices.Equal(names, []string{"sys/devices/attr"}) {
			t.Errorf("glob = %v", names)
		}
	})

	t.Run("write", func(t *testing.T) {
		if err := writeFile(h, "sys/devices/attr", []byte("newvalue1")); err != nil {
			t.Fatalf("writeFile: %v", err)
		}
		blob, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		// exactly the bytes given, with no newline appended: what a caller
		// writes is what the kernel is handed
		if got := string(blob); got != "newvalue1" {
			t.Errorf("file holds %q, want %q", got, "newvalue1")
		}
	})

	// The file is opened write-only and is neither created nor truncated, which
	// is what a sysfs attribute wants: each write is a whole command and the
	// file has no length worth preserving. On a regular file that shows up as a
	// short write leaving the tail of the previous contents behind. pkg/sysfs
	// does the same, so this is deliberate, but it is worth pinning: switching
	// to os.WriteFile would add O_CREATE|O_TRUNC and change it.
	t.Run("write-does-not-truncate", func(t *testing.T) {
		if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeFile(h, "sys/devices/attr", []byte("changed")); err != nil {
			t.Fatalf("writeFile: %v", err)
		}
		blob, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(blob), "changedl\n"; got != want {
			t.Errorf("file holds %q, want %q: the write should not truncate", got, want)
		}
	})

	t.Run("write-missing", func(t *testing.T) {
		// writing must not create: a sysfs attribute which is not there is a
		// kernel which does not support it, not a file to make
		err := writeFile(h, "sys/devices/absent", []byte("1"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("writing a missing file: %v, want fs.ErrNotExist", err)
		}
		if exists(h, "sys/devices/absent") {
			t.Error("writing a missing file created it")
		}
	})

	t.Run("invalid-path", func(t *testing.T) {
		// fs.FS names are unrooted; an absolute path is a programming error and
		// has to be rejected rather than silently escaping the root
		for _, name := range []string{"/sys/devices/attr", "../escape", "./here"} {
			if err := writeFile(h, name, []byte("1")); err == nil {
				t.Errorf("writeFile(%q) succeeded, want an error", name)
			}
		}
	})

	t.Run("empty-root-is-slash", func(t *testing.T) {
		if got, want := HostFS("").(*hostFS).root, "/"; got != want {
			t.Errorf("HostFS(\"\").root = %q, want %q", got, want)
		}
	})
}

//
// helpers
//

func cpuRange(lo, hi int) []int {
	out := make([]int, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// assertSealed checks that a set discovery produced cannot be modified.
func assertSealed(t *testing.T, cpus interface{ Set(...int) }) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("set is not sealed")
		}
	}()
	cpus.Set(0)
}
