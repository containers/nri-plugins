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
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
)

// All reading goes through these. They take paths relative to the host root and
// slash-separated, as [fs.FS] requires: build them with [path.Join], never
// filepath.Join, or an absolute path will slip in and fs.ValidPath will reject
// it at runtime.

// readFile returns the contents of a file with any trailing newlines removed.
// sysfs attributes are newline-terminated and nothing here wants the newline.
func readFile(fsys fs.FS, name string) (string, error) {
	blob, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	return strings.Trim(string(blob), "\n"), nil
}

// readInt reads a file and parses its contents as a signed integer. A leading
// 0x or 0b is honoured, as strconv does with a base of 0.
func readInt(fsys fs.FS, name string) (int, error) {
	str, err := readFile(fsys, name)
	if err != nil {
		return 0, err
	}
	i, err := strconv.ParseInt(str, 0, strconv.IntSize)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", name, str, err)
	}
	return int(i), nil
}

// readUint64 reads a file and parses its contents as an unsigned integer.
func readUint64(fsys fs.FS, name string) (uint64, error) {
	str, err := readFile(fsys, name)
	if err != nil {
		return 0, err
	}
	u, err := strconv.ParseUint(str, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid unsigned integer %q: %w", name, str, err)
	}
	return u, nil
}

// readCPUs reads a file and parses its contents as a kernel CPU list, the
// "0-3,8,12-15" form sysfs uses for every set of CPUs. The result is sealed, so
// it is safe to hand out and to share.
func readCPUs(fsys fs.FS, name string) (*libcpu.CpuMask, error) {
	str, err := readFile(fsys, name)
	if err != nil {
		return nil, err
	}
	cpus, err := libcpu.ParseCpuMask(str)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid CPU list %q: %w", name, str, err)
	}
	cpus.Seal()
	return cpus, nil
}

// readInts reads a file and parses its contents as a list of integers separated
// by sep. The NUMA distance vector is the only thing which needs it, and it is
// space separated rather than a CPU list.
func readInts(fsys fs.FS, name, sep string) ([]int, error) {
	str, err := readFile(fsys, name)
	if err != nil {
		return nil, err
	}

	var out []int
	for _, field := range strings.Split(str, sep) {
		if field == "" {
			continue
		}
		i, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid integer %q: %w", name, field, err)
		}
		out = append(out, i)
	}

	return out, nil
}

// glob returns the names matching pattern, in the order fs.Glob gives them,
// which is lexical: cpu10 comes before cpu2. Anything which needs numeric order
// has to sort for itself.
func glob(fsys fs.FS, pattern string) ([]string, error) {
	return fs.Glob(fsys, pattern)
}

// globIDs returns the names matching pattern together with the trailing number
// of each, sorted by that number. It is how the cpuN, nodeN and indexN
// directories are enumerated.
func globIDs(fsys fs.FS, pattern string) ([]string, []ID, error) {
	names, err := glob(fsys, pattern)
	if err != nil {
		return nil, nil, err
	}

	type entry struct {
		name string
		id   ID
	}

	entries := make([]entry, 0, len(names))
	for _, name := range names {
		id, ok := trailingID(name)
		if !ok {
			continue
		}
		entries = append(entries, entry{name: name, id: id})
	}

	slices.SortFunc(entries, func(a, b entry) int { return a.id - b.id })

	sorted := make([]string, len(entries))
	ids := make([]ID, len(entries))
	for i, e := range entries {
		sorted[i], ids[i] = e.name, e.id
	}

	return sorted, ids, nil
}

// trailingID returns the number a name ends in, as "cpu12" ends in 12.
func trailingID(name string) (ID, bool) {
	base := path.Base(name)

	end := len(base)
	for end > 0 && base[end-1] >= '0' && base[end-1] <= '9' {
		end--
	}
	if end == len(base) {
		return 0, false
	}

	id, err := strconv.Atoi(base[end:])
	if err != nil {
		return 0, false
	}

	return id, true
}

// exists reports whether a path is there at all.
func exists(fsys fs.FS, name string) bool {
	_, err := fs.Stat(fsys, name)
	return err == nil
}

//
// Writing
//

// WriterFS is an [fs.FS] which also supports writing. Discovery never needs it;
// it is here for callers which read a topology through an fs.FS and then want to
// write to the same tree. [HostFS] below is one.
type WriterFS interface {
	fs.FS

	// WriteFile writes data to an existing file in a single write. It neither
	// creates nor truncates the file, which is what writing a sysfs attribute
	// requires.
	WriteFile(name string, data []byte) error
}

// writeFile writes to a file through fsys, which has to be a [WriterFS] for
// that to be possible at all.
func writeFile(fsys fs.FS, name string, data []byte) error {
	w, ok := fsys.(WriterFS)
	if !ok {
		return fmt.Errorf("cannot write %s: %T is read-only", name, fsys)
	}
	return w.WriteFile(name, data)
}

//
// The default filesystem
//

// hostFS is the real filesystem below a root, with writing.
type hostFS struct {
	fs.FS
	root string
}

// HostFS returns a [WriterFS] for the real filesystem below root. HostFS("")
// and HostFS("/") both mean the whole filesystem.
func HostFS(root string) WriterFS {
	if root == "" {
		root = "/"
	}
	return &hostFS{FS: os.DirFS(root), root: root}
}

// WriteFile writes data to an existing file in a single write. It opens the file
// write-only and neither creates nor truncates it, which is what writing a sysfs
// attribute requires: os.WriteFile's O_CREATE|O_TRUNC is wrong for a kernel
// attribute.
func (h *hostFS) WriteFile(name string, data []byte) (err error) {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "write", Path: name, Err: fs.ErrInvalid}
	}

	f, err := os.OpenFile(h.osPath(name), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	_, err = f.Write(data)

	return err
}

// osPath turns an fs.FS name into a path in the operating system's own form.
func (h *hostFS) osPath(name string) string {
	return filepath.Join(h.root, filepath.FromSlash(name))
}

// String names the root, so that an error mentioning the filesystem says which.
func (h *hostFS) String() string {
	return "host filesystem at " + h.root
}
