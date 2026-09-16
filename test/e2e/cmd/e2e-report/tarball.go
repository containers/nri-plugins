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
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// packed is what we know how to read, by the suffix of the name. The tests
// pack the artifacts of a test case with xz, which takes the xz command: no
// packing is ever done with it, and the download of a tarball needs none of
// this, so a host without xz can still serve everything.
var packed = []struct {
	suffix string
	open   func(io.Reader) (io.ReadCloser, error)
}{
	{".tar.zst", openZstd},
	{".tar.gz", openGzip},
	{".tgz", openGzip},
	{".tar.xz", openXz},
	{".tar", openPlain},
}

// unpacker tells how to read a tarball, by its name.
func unpacker(name string) (func(io.Reader) (io.ReadCloser, error), bool) {
	for _, p := range packed {
		if strings.HasSuffix(name, p.suffix) {
			return p.open, true
		}
	}

	return nil, false
}

// isTarball tells whether a name is one of a tarball we can read into.
func isTarball(name string) bool {
	_, ok := unpacker(name)
	return ok
}

func openZstd(r io.Reader) (io.ReadCloser, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}

	return zr.IOReadCloser(), nil
}

func openGzip(r io.Reader) (io.ReadCloser, error) {
	return gzip.NewReader(r)
}

func openPlain(r io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(r), nil
}

// source is where a tarball is read from: a file, a member of another tarball,
// or one read into memory to be read again.
type source interface {
	open() (io.ReadCloser, error)
	// id tells one source from another, and one state of it from a later one.
	id() string
}

// fileSource reads a tarball from a file. Given a root, the path is a name
// within it, read through the root: what serves a request never walks out of
// what is published, whatever the request asks for.
type fileSource struct {
	dir   *os.Root
	path  string
	stamp int64
}

func newFileSource(dir *os.Root, p string) (*fileSource, error) {
	var (
		info os.FileInfo
		err  error
	)

	if dir != nil {
		info, err = dir.Stat(p)
	} else {
		info, err = os.Stat(p)
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}

	return &fileSource{dir: dir, path: p, stamp: info.ModTime().UnixNano()}, nil
}

func (s *fileSource) open() (io.ReadCloser, error) {
	if s.dir != nil {
		return s.dir.Open(s.path)
	}

	return os.Open(s.path)
}

func (s *fileSource) id() string {
	return s.path + "@" + strconv.FormatInt(s.stamp, 10)
}

type memberSource struct {
	tarball *Tarball
	name    string
}

func (s *memberSource) open() (io.ReadCloser, error) {
	return s.tarball.Open(s.name)
}

func (s *memberSource) id() string {
	return s.tarball.src.id() + "!" + s.name
}

type bytesSource struct {
	ident string
	data  []byte
}

func (s *bytesSource) open() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func (s *bytesSource) id() string {
	return s.ident
}

// Tarball is the contents of a tarball, listed. Reading a member unpacks the
// tarball up to it, which is why the listing is kept but the members are not.
type Tarball struct {
	src     source
	unpack  func(io.Reader) (io.ReadCloser, error)
	names   []string
	members map[string]int64
}

// openTarball reads the listing of a tarball.
func openTarball(src source, name string) (*Tarball, error) {
	unpack, ok := unpacker(name)
	if !ok {
		return nil, fmt.Errorf("%s is not a tarball we can read", name)
	}

	t := &Tarball{src: src, unpack: unpack, members: map[string]int64{}}
	err := t.walk(func(h *tar.Header, r io.Reader) (bool, error) {
		name, ok := member(h)
		if !ok {
			return false, nil
		}
		t.names = append(t.names, name)
		t.members[name] = h.Size
		return false, nil
	})
	if err != nil {
		return nil, err
	}

	slices.Sort(t.names)

	return t, nil
}

// openArchive reads the listing of the archive of a run, if it has one.
func openArchive(dir string) (*Tarball, error) {
	src, err := newFileSource(nil, filepath.Join(dir, runTar))
	if err != nil {
		return nil, err
	}

	return openTarball(src, runTar)
}

// member is the name a header goes by, and whether it is a member we serve.
// Anything but a regular file, and any name which would lead out of the
// tarball, we pretend is not there: we pack neither, and we are not the one to
// find out what a doctored tarball can talk us into.
func member(h *tar.Header) (string, bool) {
	if h.Typeflag != tar.TypeReg {
		return "", false
	}

	name := path.Clean("/" + filepath.ToSlash(h.Name))

	return strings.TrimPrefix(name, "/"), name != "/"
}

// walk calls fn for every header in the tarball, until it says it is done.
func (t *Tarball) walk(fn func(*tar.Header, io.Reader) (bool, error)) error {
	packed, err := t.src.open()
	if err != nil {
		return err
	}
	defer func() { _ = packed.Close() }()

	unpacked, err := t.unpack(packed)
	if err != nil {
		return err
	}
	defer func() { _ = unpacked.Close() }()

	tr := tar.NewReader(unpacked)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if done, err := fn(header, tr); err != nil || done {
			return err
		}
	}
}

// Size is the size of a member, and whether the tarball has it.
func (t *Tarball) Size(name string) (int64, bool) {
	size, ok := t.members[name]
	return size, ok
}

// Open gives the contents of a member. The caller closes it.
func (t *Tarball) Open(name string) (io.ReadCloser, error) {
	if _, ok := t.members[name]; !ok {
		return nil, fs.ErrNotExist
	}

	packed, err := t.src.open()
	if err != nil {
		return nil, err
	}

	unpacked, err := t.unpack(packed)
	if err != nil {
		_ = packed.Close()
		return nil, err
	}

	reader := &memberReader{unpacked: unpacked, packed: packed}

	tr := tar.NewReader(unpacked)
	for {
		header, err := tr.Next()
		if err != nil {
			_ = reader.Close()
			if err == io.EOF {
				return nil, fs.ErrNotExist
			}
			return nil, err
		}
		if found, ok := member(header); ok && found == name {
			reader.Reader = tr
			return reader, nil
		}
	}
}

// Read reads a whole member.
func (t *Tarball) Read(name string) ([]byte, error) {
	file, err := t.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	return io.ReadAll(file)
}

// List gives the names in a directory of the tarball, the directories among
// them with a trailing slash.
func (t *Tarball) List(dir string) []string {
	if dir != "" {
		dir += "/"
	}

	names, seen := []string{}, map[string]bool{}
	for _, name := range t.names {
		if !strings.HasPrefix(name, dir) {
			continue
		}
		rest := name[len(dir):]
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i+1]
		}
		if rest == "" || seen[rest] {
			continue
		}
		seen[rest] = true
		names = append(names, rest)
	}
	slices.Sort(names)

	return names
}

// Stale tells whether the tarball has changed since we read its listing.
func (t *Tarball) Stale() bool {
	src, ok := t.src.(*fileSource)
	if !ok {
		return false
	}
	fresh, err := newFileSource(src.dir, src.path)

	return err != nil || fresh.id() != src.id()
}

// memberReader reads a member and unwinds the tarball with it.
type memberReader struct {
	io.Reader
	unpacked io.ReadCloser
	packed   io.ReadCloser
}

func (r *memberReader) Close() error {
	_ = r.unpacked.Close()
	return r.packed.Close()
}

// openXz unpacks with the xz command: there is no xz in the standard library,
// and reading the tarballs of tests published before we packed with zstd is
// not worth a dependency of its own.
func openXz(r io.Reader) (io.ReadCloser, error) {
	if _, err := exec.LookPath("xz"); err != nil {
		return nil, fmt.Errorf("xz is needed to read this tarball: %w", err)
	}

	cmd := exec.Command("xz", "--decompress", "--stdout")
	cmd.Stdin = r
	cmd.Stderr = io.Discard

	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &commandReader{Reader: out, out: out, cmd: cmd}, nil
}

// commandReader reads what a command writes, and reaps it when it is done with
// or dropped half way through.
type commandReader struct {
	io.Reader
	out io.ReadCloser
	cmd *exec.Cmd
}

func (r *commandReader) Close() error {
	_ = r.out.Close()
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	// The command is done, or was killed for being of no more use: either way
	// how it went is not news.
	_ = r.cmd.Wait()

	return nil
}
