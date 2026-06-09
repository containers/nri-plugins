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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	defaultResctrlPath = "/sys/fs/resctrl"
	monGroupsDir       = "mon_groups"
)

// resctrlOps handles filesystem operations on the resctrl mount.
type resctrlOps struct {
	resctrlPath string
}

func newResctrlOps(resctrlPath string) *resctrlOps {
	return &resctrlOps{
		resctrlPath: resctrlPath,
	}
}

// createMonGroup creates a mon_group directory under the appropriate ctrl_group
// and returns the full path. If rdtClass is empty, the mon_group is created
// under the root resctrl directory.
//
// The kernel assigns an RMID to the new mon_group on mkdir. If no RMIDs are
// available, mkdir returns ENOSPC.
func (r *resctrlOps) createMonGroup(rdtClass, podUID string) (string, error) {
	parentDir := r.resctrlPath
	if rdtClass != "" {
		if !isValidRDTClass(rdtClass) {
			return "", fmt.Errorf("invalid RDT class name %q", rdtClass)
		}
		parentDir = filepath.Join(r.resctrlPath, rdtClass)
	}

	// When an RDT class is specified, the ctrl_group must already exist
	// (created by an allocation plugin). Do not create it implicitly —
	// that would make an unintended ctrl_group in the resctrl filesystem.
	if rdtClass != "" {
		info, err := os.Stat(parentDir)
		if err != nil {
			return "", fmt.Errorf("ctrl_group %s does not exist: %w", parentDir, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("ctrl_group %s is not a directory", parentDir)
		}
	}

	monGroupsPath := filepath.Join(parentDir, monGroupsDir)
	monGroupDir := filepath.Join(monGroupsPath, podUID)

	// Ensure the mon_groups/ directory exists. On a real resctrl mount
	// this is always present. For testing, create it if needed.
	if err := os.MkdirAll(monGroupsPath, 0755); err != nil {
		return "", fmt.Errorf("mon_groups dir not available at %s: %w", monGroupsPath, err)
	}

	// Use Mkdir (not MkdirAll) for the final mon_group directory to
	// avoid accidentally creating a ctrl_group if rdtClass is wrong.
	if err := os.Mkdir(monGroupDir, 0755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return monGroupDir, nil
		}
		if errors.Is(err, syscall.ENOSPC) {
			return "", fmt.Errorf("no RMIDs available for pod %s: %w", podUID, err)
		}
		return "", fmt.Errorf("failed to create mon_group %s: %w", monGroupDir, err)
	}

	return monGroupDir, nil
}

// removeMonGroup removes a mon_group directory. The kernel releases the RMID.
func (r *resctrlOps) removeMonGroup(monGroupDir string) error {
	err := os.Remove(monGroupDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove mon_group %s: %w", monGroupDir, err)
	}
	return nil
}

// writeTaskPID writes a PID to the mon_group's tasks file. The kernel assigns
// this PID (and all future child processes) to the mon_group's RMID.
func (r *resctrlOps) writeTaskPID(monGroupDir string, pid int) error {
	tasksFile := filepath.Join(monGroupDir, "tasks")
	f, err := os.OpenFile(tasksFile, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open %s for pid %d: %w", tasksFile, pid, err)
	}
	defer func() { _ = f.Close() }()
	data := []byte(strconv.Itoa(pid) + "\n")
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write pid %d to %s: %w", pid, tasksFile, err)
	}
	return nil
}

// cleanOrphanedMonGroups removes mon_group directories that are not tracked
// in the given state. This handles cleanup after a plugin crash/restart.
func (r *resctrlOps) cleanOrphanedMonGroups(state *podState) {
	// Scan root-level mon_groups.
	r.cleanOrphanedInDir(filepath.Join(r.resctrlPath, monGroupsDir), state)

	// Scan ctrl_group-level mon_groups.
	entries, err := os.ReadDir(r.resctrlPath)
	if err != nil {
		log.Warnf("cleanOrphanedMonGroups: failed to read %s: %v", r.resctrlPath, err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip non-ctrl_group entries.
		if name == monGroupsDir || name == "info" || strings.HasPrefix(name, "mon_") {
			continue
		}
		ctrlGroupMonDir := filepath.Join(r.resctrlPath, name, monGroupsDir)
		r.cleanOrphanedInDir(ctrlGroupMonDir, state)
	}
}

// cleanOrphanedInDir removes mon_group directories in a specific mon_groups/
// directory that look like pod UIDs but are not tracked in state.
func (r *resctrlOps) cleanOrphanedInDir(monGroupsPath string, state *podState) {
	entries, err := os.ReadDir(monGroupsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warnf("failed to read mon_groups directory %s: %v", monGroupsPath, err)
		}
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Only clean directories that look like pod UIDs (contain dashes like UUIDs).
		if !looksLikePodUID(name) {
			continue
		}
		orphanDir := filepath.Join(monGroupsPath, name)
		trackedDir := state.getMonGroupDir(name)
		if trackedDir == orphanDir {
			// This is the active mon_group for this pod.
			continue
		}
		log.Infof("removing orphaned mon_group %s", orphanDir)
		if err := os.Remove(orphanDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warnf("failed to remove orphaned mon_group %s: %v", orphanDir, err)
		}
	}
}

// looksLikePodUID returns true if the name looks like a Kubernetes pod UID.
// It accepts both the standard UUID format with dashes (e.g.,
// a1b2c3d4-e5f6-7890-abcd-ef1234567890) and the compact 32-char hex format
// without dashes (e.g., a1b2c3d4e5f678901234567890abcdef).
func looksLikePodUID(name string) bool {
	switch len(name) {
	case 36:
		// Check for UUID-like pattern: 8-4-4-4-12 hex chars.
		parts := strings.Split(name, "-")
		if len(parts) != 5 {
			return false
		}
		expectedLens := []int{8, 4, 4, 4, 12}
		for i, part := range parts {
			if len(part) != expectedLens[i] {
				return false
			}
			for _, c := range part {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
					return false
				}
			}
		}
		return true
	case 32:
		// Compact hex format without dashes.
		for _, c := range name {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// isValidRDTClass returns true if the name is a safe resctrl ctrl_group name.
// It rejects path separators, dot-segments, and empty strings to prevent
// path traversal outside the resctrl mount.
func isValidRDTClass(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if c == '/' || c == 0 {
			return false
		}
	}
	return true
}
