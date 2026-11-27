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

package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// lsaSize is the size of the label storage area file of a persistent
// device.
const lsaSize = 256 << 10

// lsaPath returns the label storage area file of a persistent device whose
// data file is dataPath: the extension of dataPath replaced by ".lsa", or
// ".lsa" appended if dataPath has no extension.
func lsaPath(dataPath string) string {
	return strings.TrimSuffix(dataPath, filepath.Ext(dataPath)) + ".lsa"
}

// ensureBackingFile creates the backing file (and its directory) if it is
// missing, and extends it (sparse) if it is smaller than size. An existing
// larger file is left as is: qemu maps only size bytes of it.
func ensureBackingFile(path string, size int64) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("backing file %s is not a regular file", path)
	}
	if st.Size() < size {
		if err := f.Truncate(size); err != nil {
			return err
		}
	}
	return nil
}

// ensureFiles creates the backing files of a device if they are missing:
// the data file of size bytes and, if lsa is not "", the label storage
// area file.
func ensureFiles(path, lsa string, size int64) error {
	if err := ensureBackingFile(path, size); err != nil {
		return err
	}
	if lsa != "" {
		return ensureBackingFile(lsa, lsaSize)
	}
	return nil
}

// removeBackingFile removes a backing file. A missing file is not an error.
func removeBackingFile(path string) error {
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// removeDeviceFiles removes the backing files of a device: the data file
// and the label storage area file.
func removeDeviceFiles(d *device, logf func(string, ...any)) {
	for _, f := range []string{d.Path, d.LSAPath} {
		if f == "" {
			continue
		}
		if err := removeBackingFile(f); err != nil {
			logf("device %s: cannot remove %s: %v", d.Name, f, err)
		}
	}
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

// validName returns an error unless name is a DNS-label-like device name
// (lowercase letters, digits, '-' and '.').
func validName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q: use lowercase letters, digits, '-' and '.', at most 63 characters", name)
	}
	return nil
}
