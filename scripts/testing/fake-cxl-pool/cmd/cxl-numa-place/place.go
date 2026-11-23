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
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// movePagesBatch is the number of pages of one move_pages call.
const movePagesBatch = 1 << 16

// errMovePagesDenied is the error of a move_pages status query that the
// host does not permit, for instance a seccomp filter of a container.
var errMovePagesDenied = errors.New("move_pages is not permitted here")

// fileMap is a read-only shared mapping of a whole file.
type fileMap struct {
	path     string
	size     int64
	pageSize int
	data     []byte // nil for an empty file
}

// openMap returns a read-only shared mapping of the regular file path.
func openMap(path string) (*fileMap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("%s is too large to map (%d bytes)", path, st.Size())
	}
	m := &fileMap{path: path, size: st.Size(), pageSize: os.Getpagesize()}
	if m.size == 0 {
		return m, nil
	}
	// The mapping keeps the file referenced after f is closed.
	m.data, err = unix.Mmap(int(f.Fd()), 0, int(m.size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("cannot map %s: %w", path, err)
	}
	return m, nil
}

// close unmaps the file.
func (m *fileMap) close() error {
	if m.data == nil {
		return nil
	}
	err := unix.Munmap(m.data)
	m.data = nil
	return err
}

// pages returns the number of pages of the file.
func (m *fileMap) pages() int {
	return int((m.size + int64(m.pageSize) - 1) / int64(m.pageSize))
}

// addr returns the address of the page with index page in the mapping.
func (m *fileMap) addr(page int) uintptr {
	return uintptr(unsafe.Pointer(&m.data[0])) + uintptr(page)*uintptr(m.pageSize)
}

// resident returns the indexes of the pages of the file that are in
// memory (the page cache of the file), in increasing order.
func (m *fileMap) resident() ([]int, error) {
	n := m.pages()
	if n == 0 {
		return nil, nil
	}
	vec := make([]byte, n)
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, m.addr(0), uintptr(len(m.data)), uintptr(unsafe.Pointer(&vec[0])))
	if errno != 0 {
		return nil, fmt.Errorf("mincore %s: %w", m.path, errno)
	}
	var pages []int
	for i, v := range vec {
		if v&1 != 0 {
			pages = append(pages, i)
		}
	}
	return pages, nil
}

// populate maps the pages of the file with the given indexes into the
// page tables of this process, without reading the other pages. A page
// that cannot be mapped is left out: it has no node in the query.
func (m *fileMap) populate(pages []int) error {
	for _, r := range runs(pages) {
		err := unix.Madvise(m.span(r.first, r.last), unix.MADV_POPULATE_READ)
		if err == nil {
			continue
		}
		if errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("madvise MADV_POPULATE_READ on %s: %w (it needs Linux 5.14 or later)", m.path, err)
		}
		// A page of the run went away after mincore: map the others one
		// by one.
		for p := r.first; p <= r.last; p++ {
			_ = unix.Madvise(m.span(p, p), unix.MADV_POPULATE_READ)
		}
	}
	return nil
}

// span returns the part of the mapping from page first to page last.
func (m *fileMap) span(first, last int) []byte {
	end := min((last+1)*m.pageSize, len(m.data))
	return m.data[first*m.pageSize : end]
}

// query returns the node of each page with the given indexes, or a
// negative errno if the page has none (-ENOENT: not mapped).
func (m *fileMap) query(pages []int) ([]int32, error) {
	status := make([]int32, len(pages))
	err := m.movePages(pages, status)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOSYS) {
		return nil, fmt.Errorf("%w: %v", errMovePagesDenied, err)
	}
	if err != nil {
		return nil, fmt.Errorf("move_pages status query on %s: %w", m.path, err)
	}
	return status, nil
}

// movePages calls move_pages(2) without target nodes on this process for
// the pages with the given indexes, in batches of movePagesBatch pages. It
// sets status to the node of each page or a negative errno.
func (m *fileMap) movePages(pages []int, status []int32) error {
	addrs := make([]uintptr, 0, min(len(pages), movePagesBatch))
	for start := 0; start < len(pages); start += movePagesBatch {
		end := min(start+movePagesBatch, len(pages))
		addrs = addrs[:0]
		for _, p := range pages[start:end] {
			addrs = append(addrs, m.addr(p))
		}
		_, _, errno := unix.Syscall6(unix.SYS_MOVE_PAGES, 0, uintptr(len(addrs)),
			uintptr(unsafe.Pointer(&addrs[0])), 0, uintptr(unsafe.Pointer(&status[start])), 0)
		if errno != 0 {
			return errno
		}
	}
	return nil
}

// pageRun is a range of consecutive page indexes, first to last.
type pageRun struct{ first, last int }

// runs returns the ranges of consecutive indexes of the increasing pages.
func runs(pages []int) []pageRun {
	var rs []pageRun
	for i := 0; i < len(pages); {
		j := i
		for j+1 < len(pages) && pages[j+1] == pages[j]+1 {
			j++
		}
		rs = append(rs, pageRun{pages[i], pages[j]})
		i = j + 1
	}
	return rs
}

// locate returns the resident pages of the file and the node of each,
// or its negative errno.
func (m *fileMap) locate() ([]int, []int32, error) {
	pages, err := m.resident()
	if err != nil {
		return nil, nil, err
	}
	if err := m.populate(pages); err != nil {
		return nil, nil, err
	}
	status, err := m.query(pages)
	if err != nil {
		return nil, nil, err
	}
	return pages, status, nil
}

// checkResidencyVisible returns an error unless mincore(2) shows this
// process the page cache of the file: the kernel shows it only to the
// owner of the file, a process that may write to it, or root.
func checkResidencyVisible(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*unix.Stat_t); ok && int(sys.Uid) == os.Geteuid() {
		return nil
	}
	if os.Geteuid() == 0 || unix.Access(path, unix.W_OK) == nil {
		return nil
	}
	return fmt.Errorf("%s: only its owner, a writer or root sees which of its pages are in memory: run as root", path)
}

// where returns the placement of the resident pages of the file path.
func where(path string) (*Placement, error) {
	if err := checkResidencyVisible(path); err != nil {
		return nil, err
	}
	m, err := openMap(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = m.close() }()
	_, status, err := m.locate()
	if err != nil {
		return nil, err
	}
	return newPlacement(m, status), nil
}
