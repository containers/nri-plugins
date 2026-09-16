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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/klauspost/compress/zstd"
)

const (
	// runTar is what a run collected, all of it, in a single archive.
	runTar = "results.tar.zst"
	// packLevel trades a little packing time for the smallest archive. The
	// results of a run are read once and kept for months.
	packLevel = zstd.SpeedBestCompression
)

// unpacked are the files of a run which are never packed: whatever it takes to
// tell how the run went without a server to serve the archive.
var unpacked = []string{resultsJSON, indexHTML, statusTxt, summaryTxt,
	"git.describe", "git.sha1", "git.remote"}

// packRun packs everything a run collected into a single archive next to the
// files it takes to tell how the run went, and removes what it packed.
func packRun(dir string) error {
	if !isDir(dir) {
		return fmt.Errorf("no such directory: %s", dir)
	}
	if exists(filepath.Join(dir, runTar)) {
		return fmt.Errorf("%s is already packed", dir)
	}

	packed, err := pack(filepath.Join(dir, runTar), dir)
	if err != nil {
		_ = os.Remove(filepath.Join(dir, runTar))
		return err
	}

	// Only ever remove a file which made it into the archive, and only a
	// directory which removing those leaves empty.
	for _, name := range packed {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	pruneEmptyDirs(dir)

	return nil
}

// pack writes the packable files under dir to an archive, and tells which
// files it packed.
func pack(archive, dir string) ([]string, error) {
	names, err := packable(dir)
	if err != nil {
		return nil, err
	}

	file, err := os.Create(archive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	zw, err := zstd.NewWriter(file, zstd.WithEncoderLevel(packLevel))
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)

	for _, name := range names {
		if err := packFile(tw, dir, name); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	return names, file.Sync()
}

// packable lists the regular files under dir which are ours to pack, by their
// path relative to it.
func packable(dir string) ([]string, error) {
	names := []string{}

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}

		name, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if name == runTar || slices.Contains(unpacked, name) {
			return nil
		}
		names = append(names, filepath.ToSlash(name))

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.Sort(names)

	return names, nil
}

func packFile(tw *tar.Writer, dir, name string) error {
	file, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name

	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(tw, file)

	return err
}

// pruneEmptyDirs removes the directories under dir which packing left empty.
func pruneEmptyDirs(dir string) {
	dirs := []string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir {
			dirs = append(dirs, p)
		}
		return nil
	})

	// Deepest first, so that a directory of empty directories goes as well.
	slices.Reverse(dirs)
	for _, d := range dirs {
		_ = os.Remove(d)
	}
}

// isPacked tells whether the results of a run are in an archive.
func isPacked(dir string) bool {
	return exists(filepath.Join(dir, runTar))
}
