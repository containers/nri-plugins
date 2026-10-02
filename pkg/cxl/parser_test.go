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

package cxl

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates a file with contents under a fresh temporary directory and
// returns its path.
func writeFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
	return path
}

func TestMissingFile(t *testing.T) {
	var value int
	absent := filepath.Join(t.TempDir(), "absent")

	err := parse(parseWithSscanf(absent, "%d", &value))
	if err == nil {
		t.Fatal("expected an error parsing a nonexistent file, got nil")
	}
	if !errors.Is(err, ErrMissingFile) {
		t.Errorf("error does not match ErrMissingFile: %v", err)
	}
	// The chain to the underlying os error must stay intact, so that callers
	// can treat an absent CXL sysfs file like any other absent file.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error does not match fs.ErrNotExist: %v", err)
	}
	if errors.Is(err, ErrMissingLine) {
		t.Errorf("error unexpectedly matches ErrMissingLine: %v", err)
	}
}

func TestMissingLine(t *testing.T) {
	var value int
	path := writeFile(t, "uevent", "DEVTYPE=cxl_memdev\n")

	err := parse(parseWithSscanf(path, "MAJOR=%d", &value))
	if err == nil {
		t.Fatal("expected an error parsing a file without a matching line, got nil")
	}
	if !errors.Is(err, ErrMissingLine) {
		t.Errorf("error does not match ErrMissingLine: %v", err)
	}
	if errors.Is(err, ErrMissingFile) {
		t.Errorf("error unexpectedly matches ErrMissingFile: %v", err)
	}
}

func TestUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, file permissions are not enforced")
	}

	var value int
	path := writeFile(t, "serial", "0xc100e2e0\n")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("failed to chmod %s: %v", path, err)
	}

	err := parse(parseWithSscanf(path, "0x%x", &value))
	if err == nil {
		t.Fatal("expected an error parsing an unreadable file, got nil")
	}
	if !errors.Is(err, ErrUnreadableFile) {
		t.Errorf("error does not match ErrUnreadableFile: %v", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error does not match fs.ErrPermission: %v", err)
	}
	if errors.Is(err, ErrMissingFile) {
		t.Errorf("error unexpectedly matches ErrMissingFile: %v", err)
	}
}

func TestParseIgnoring(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "target0")

	// An ignored error leaves the destination at its previous value and lets
	// the remaining tasks run.
	value := 42
	ran := false
	err := parse(
		parseIgnoring(ErrMissingFile),
		parseWithSscanf(absent, "%d", &value),
		func(p *parser) error { ran = true; return nil },
	)
	if err != nil {
		t.Errorf("expected ErrMissingFile to be ignored, got: %v", err)
	}
	if value != 42 {
		t.Errorf("expected destination to keep its value 42, got %d", value)
	}
	if !ran {
		t.Error("expected the task after the ignored one to run")
	}

	// parseIgnoring() with no targets restores the default of ignoring nothing.
	err = parse(
		parseIgnoring(ErrMissingFile),
		parseIgnoring(),
		parseWithSscanf(absent, "%d", &value),
	)
	if !errors.Is(err, ErrMissingFile) {
		t.Errorf("expected ErrMissingFile after reset, got: %v", err)
	}

	// A non-matching target must not swallow the error.
	err = parse(
		parseIgnoring(ErrMissingLine),
		parseWithSscanf(absent, "%d", &value),
	)
	if !errors.Is(err, ErrMissingFile) {
		t.Errorf("expected ErrMissingFile to survive an unrelated ignore, got: %v", err)
	}
}

// TestParseIgnoringWrappedError guards the fragility that sentinel matching
// removes: matching by reflected type name stopped working as soon as a parse
// task added context to its error, silently turning a tolerated condition into
// a scan failure.
func TestParseIgnoringWrappedError(t *testing.T) {
	wrapping := func(p *parser) error {
		return fmt.Errorf("uevent: %w", fmt.Errorf("%w: %w", ErrMissingFile, fs.ErrNotExist))
	}

	if err := parse(parseIgnoring(ErrMissingFile), wrapping); err != nil {
		t.Errorf("expected a wrapped ErrMissingFile to be ignored, got: %v", err)
	}
	if err := parse(parseIgnoring(fs.ErrNotExist), wrapping); err != nil {
		t.Errorf("expected a wrapped fs.ErrNotExist to be ignored, got: %v", err)
	}
}

// TestDevicesFromSysfsNoBus covers the normal case on a system without CXL: the
// bus directory does not exist at all. That error comes straight from os.ReadDir
// rather than from the parser, so it matches fs.ErrNotExist but not
// ErrMissingFile.
func TestDevicesFromSysfsNoBus(t *testing.T) {
	_, err := DevicesFromSysfs(t.TempDir())
	if err == nil {
		t.Fatal("expected an error scanning a root without a CXL bus, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error does not match fs.ErrNotExist: %v", err)
	}
	if errors.Is(err, ErrMissingFile) {
		t.Errorf("error unexpectedly matches ErrMissingFile: %v", err)
	}
}
