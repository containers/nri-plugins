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

package steering

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// memoryCurrent returns the memory.current value of the cgroup in bytes.
func memoryCurrent(cgroupPath string) (int64, error) {
	return readInt64(filepath.Join(cgroupPath, "memory.current"))
}

// numaUsage returns the sum of the given memory.numa_stat categories
// in bytes per NUMA node.
//
// Parameters:
//   - cgroupPath: cgroup directory.
//   - categories: memory.numa_stat row names, for example "anon" and "shmem".
func numaUsage(cgroupPath string, categories ...string) (NodeMem, error) {
	path := filepath.Join(cgroupPath, "memory.numa_stat")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	usage, err := parseNumaStat(f, categories)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return usage, nil
}

// procs returns the process IDs listed in cgroup.procs of the cgroup and
// of the cgroups below it. A cgroup below it that goes away meanwhile is
// left out.
func procs(cgroupPath string) ([]int, error) {
	var pids []int
	err := filepath.WalkDir(cgroupPath, func(dir string, d fs.DirEntry, err error) error {
		switch {
		case err != nil && dir != cgroupPath && errors.Is(err, fs.ErrNotExist):
			return fs.SkipDir
		case err != nil:
			return err
		case !d.IsDir():
			return nil
		}
		path := filepath.Join(dir, "cgroup.procs")
		f, err := os.Open(path)
		if dir != cgroupPath && errors.Is(err, fs.ErrNotExist) {
			return fs.SkipDir
		}
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		ids, err := parseInts(f)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		pids = append(pids, ids...)
		return nil
	})
	return pids, err
}

// readTrim returns the content of the file without the surrounding white
// space.
func readTrim(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// readInt64 returns the integer in the file.
func readInt64(path string) (int64, error) {
	s, err := readTrim(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return value, nil
}

// parseNumaStat returns the sum of the values of the given categories
// per node. Rows have the format "<category> N<node>=<bytes> ...".
func parseNumaStat(r io.Reader, categories []string) (NodeMem, error) {
	wanted := make(map[string]bool, len(categories))
	for _, category := range categories {
		wanted[category] = true
	}
	usage := NodeMem{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !wanted[fields[0]] {
			continue
		}
		for _, field := range fields[1:] {
			nodeStr, valueStr, found := strings.Cut(field, "=")
			if !found || !strings.HasPrefix(nodeStr, "N") {
				return nil, fmt.Errorf("invalid node entry %q", field)
			}
			node, err := strconv.Atoi(nodeStr[1:])
			if err != nil {
				return nil, fmt.Errorf("invalid node entry %q: %w", field, err)
			}
			value, err := strconv.ParseInt(valueStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid node entry %q: %w", field, err)
			}
			usage[node] += value
		}
	}
	return usage, scanner.Err()
}

// parseMemoryEvents returns the memory.events counters by name. Rows
// have the format "<name> <count>".
func parseMemoryEvents(r io.Reader) (map[string]int64, error) {
	events := make(map[string]int64)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		count, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid counter %q: %w", scanner.Text(), err)
		}
		events[fields[0]] = count
	}
	return events, scanner.Err()
}

// parseInts returns the integers listed one per line.
func parseInts(r io.Reader) ([]int, error) {
	var values []int
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		value, err := strconv.Atoi(line)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, scanner.Err()
}
