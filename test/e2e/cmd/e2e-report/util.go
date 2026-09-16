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
	"bytes"
	"cmp"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// isoTime is how we spell a point in time, in results.json and on a page.
const isoTime = "2006-01-02T15:04:05"

// readFile returns the contents of a file, or nothing if there is none.
func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// readTrimmed returns the contents of a file without the whitespace around it.
func readTrimmed(path string) string {
	return strings.TrimSpace(readFile(path))
}

// splitLines splits text into lines, without a final empty one.
func splitLines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}

	return lines
}

// readDir lists the names in a directory, in order.
func readDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names, nil
}

// exists tells whether there is anything at a path.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isDir tells whether a path leads to a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// dirSize is how much the files under a directory take.
func dirSize(dir string) (int64, error) {
	var total int64

	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})

	return total, err
}

// writeJSON writes a value as the indented json we publish.
func writeJSON(path string, value any) error {
	buf := &bytes.Buffer{}
	encoder := json.NewEncoder(buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}

	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// sortedKeys returns the keys of a map in order.
func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}

// ref is the address of a value, for a field which is unset when there is
// nothing to put in it.
func ref[T any](value T) *T {
	return &value
}

// deref is the value behind a pointer, or the zero value if there is none.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
