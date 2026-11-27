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

package store

import (
	"errors"
	"os"
	"path/filepath"
)

// fileOps are the file operations of the store's writes.
type fileOps interface {
	// WriteFile replaces the file name with data, readable by the owner
	// only, synced to the disk if sync.
	WriteFile(name string, data []byte, sync bool) error
	// Rename renames oldName to newName, replacing newName.
	Rename(oldName, newName string) error
	// Link makes newName a hard link of oldName.
	Link(oldName, newName string) error
	// Remove removes the file name.
	Remove(name string) error
	// SyncDir syncs the directory dir to the disk.
	SyncDir(dir string) error
}

// osFiles are the file operations of the operating system.
type osFiles struct{}

// WriteFile replaces the file name with data, synced if sync.
func (osFiles) WriteFile(name string, data []byte, sync bool) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if sync {
		if err := f.Sync(); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	return f.Close()
}

// Rename renames oldName to newName.
func (osFiles) Rename(oldName, newName string) error {
	return os.Rename(oldName, newName)
}

// Link makes newName a hard link of oldName.
func (osFiles) Link(oldName, newName string) error {
	return os.Link(oldName, newName)
}

// Remove removes the file name.
func (osFiles) Remove(name string) error {
	return os.Remove(name)
}

// SyncDir syncs the directory dir.
func (osFiles) SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// WriteFileAtomic replaces the file path with data, readable by the owner
// only: data is written to path.tmp and synced, renamed to path, and the
// directory synced, so that a crash leaves the old content or the new one.
func WriteFileAtomic(path string, data []byte) error {
	files, tmp := osFiles{}, path+".tmp"
	if err := files.WriteFile(tmp, data, true); err != nil {
		return err
	}
	if err := files.Rename(tmp, path); err != nil {
		if rerr := files.Remove(tmp); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return errors.Join(err, rerr)
		}
		return err
	}
	return files.SyncDir(filepath.Dir(path))
}
