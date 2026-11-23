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

package kernel

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

// Superblock and Mount: the filesystem of a block device (/dev/pmemK, or a
// loop device over it) and its mounts: node mounts, binds of them, and the
// copies of those in the mount namespaces of containers.
//
// Identity: one superblock per block device per kernel: a second mount with
// other options silently gets the first superblock; a mount is its target
// in a mount namespace.
// States: a superblock is read-only or read-write (the super options of
// its mounts, "ro"); the DAX option may be silently off (xfs, erofs drop
// it). A block device is read-only or read-write (BLKROSET): a read-only
// one refuses a dirty filesystem (EROFS) and read-write mounts (EACCES).
// Transitions: Mount and FSMount make a mount (a new superblock or the
// existing one); Remount changes the superblock of every mount of it;
// Unmount removes one mount (never detached: a lazy unmount under a live
// DAX mapping crashes the guest); the last unmount of a cached node mount
// clears its autoclear loop device.
// Invariants: a block device is never made read-only under a read-write
// superblock anywhere on the node, and not when the superblocks cannot be
// read (SetBlockDeviceReadOnly). Every mount namespace is read: a
// superblock that a container holds outlives the node mount.
// Waits: none.

// MountInfo is a mount of a /proc/<pid>/mountinfo.
type MountInfo struct {
	// DevNum is major:minor of the device of the superblock.
	DevNum string `json:"devNum"`
	// Root is the directory of the filesystem that is mounted, / for its
	// root.
	Root string `json:"root"`
	// Target is the mount point.
	Target string `json:"target"`
	FSType string `json:"fsType"`
	Source string `json:"source"`
	// SuperOpts are the super options, like dax=always.
	SuperOpts []string `json:"superOpts,omitempty"`
	// MountFlags are the per-mount options, like ro.
	MountFlags []string `json:"mountFlags,omitempty"`
}

// SuperReadOnly tells whether the superblock of a mount is read-only.
func (m *MountInfo) SuperReadOnly() bool {
	return m.HasSuperOpt("ro")
}

// HasSuperOpt tells whether the super options of a mount have opt.
func (m *MountInfo) HasSuperOpt(opt string) bool {
	for _, o := range m.SuperOpts {
		if o == opt {
			return true
		}
	}
	return false
}

// Superblock is a superblock on a block device, as one of its mounts shows
// it.
type Superblock struct {
	// Device is the block device of the superblock, /dev/pmemK or
	// /dev/loopN.
	Device string `json:"device"`
	// DevNum is the major:minor of Device.
	DevNum string `json:"devNum"`
	// Source is the source of the mount.
	Source string `json:"source"`
	FSType string `json:"fsType"`
	// ReadOnly: the superblock is read-only.
	ReadOnly bool `json:"readOnly"`
	// Where is the mount: "pid P at <mount point>".
	Where string `json:"where"`
}

// ParseMountInfo returns the mounts of a /proc/<pid>/mountinfo.
func ParseMountInfo(r io.Reader) ([]MountInfo, error) {
	var out []MountInfo
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		// id parent major:minor root target options [optional...] - fstype source superopts
		f := strings.Fields(s.Text())
		if len(f) == 0 {
			continue
		}
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if len(f) < 6 || sep < 0 || len(f) < sep+3 {
			return nil, fmt.Errorf("invalid mountinfo line %q", s.Text())
		}
		mi := MountInfo{
			DevNum:     f[2],
			Root:       unescapeMountInfo(f[3]),
			Target:     unescapeMountInfo(f[4]),
			MountFlags: strings.Split(f[5], ","),
			FSType:     f[sep+1],
			Source:     unescapeMountInfo(f[sep+2]),
		}
		if len(f) > sep+3 {
			mi.SuperOpts = strings.Split(f[sep+3], ",")
		}
		out = append(out, mi)
	}
	return out, s.Err()
}

// unescapeMountInfo returns a mountinfo field with its octal escapes
// (\040 for a space) replaced.
func unescapeMountInfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// FindMount returns the topmost mount at target, nil if there is none.
func FindMount(mounts []MountInfo, target string) *MountInfo {
	target = filepath.Clean(target)
	for i := len(mounts) - 1; i >= 0; i-- {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

// Mounts returns the mounts of the mount namespace of the process.
func (k *Kernel) Mounts() ([]MountInfo, error) {
	data, err := os.ReadFile(filepath.Join(k.procRoot(), "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	return ParseMountInfo(bytes.NewReader(data))
}

// Mounted returns the topmost mount at target in the mount namespace of the
// process, nil if there is none.
func (k *Kernel) Mounted(target string) (*MountInfo, error) {
	mounts, err := k.Mounts()
	if err != nil {
		return nil, err
	}
	return FindMount(mounts, target), nil
}

// blockDevNum returns the major:minor of a block device (/dev/<name>) from
// sysfs.
func (k *Kernel) blockDevNum(dev string) (string, error) {
	num, err := readTrim(k.blockDev(filepath.Base(dev), "dev"))
	if err != nil {
		return "", fmt.Errorf("the device number of %s: %w", dev, err)
	}
	return num, nil
}

// Superblocks returns the superblocks of the node on the block device dev
// and on the loop devices over it, in every mount namespace, one per device
// with a mount: it scans the mountinfo of a process of each mount namespace
// of the node for the mounts whose device number is that of dev or of a
// loop device over it (by sysfs: the dev file of the block device, the
// backing of the loop devices). A btrfs superblock has an anonymous device
// number: its mounts are matched by their source, and a btrfs that lists
// one of the devices in sysfs without such a mount is a read-write
// superblock of the device.
func (k *Kernel) Superblocks(dev string) ([]Superblock, error) {
	devs := map[string]string{} // major:minor -> device node
	num, err := k.blockDevNum(dev)
	if err != nil {
		return nil, err
	}
	devs[num] = dev
	for _, loop := range k.loopNames() {
		backing, err := k.LoopBackingFile(loop)
		if err != nil || !SameDevice(backing, dev) {
			continue
		}
		n, err := k.blockDevNum(loop)
		if err != nil {
			return nil, err
		}
		devs[n] = filepath.Join(k.devRoot(), loop)
	}
	return k.superblocksOf(devs)
}

// superblocksOf returns the superblocks on the devices of devs (major:minor
// -> device node) in every mount namespace of the node, one per device.
func (k *Kernel) superblocksOf(devs map[string]string) ([]Superblock, error) {
	entries, err := os.ReadDir(k.procRoot())
	if err != nil {
		return nil, err
	}
	seenNS := map[string]bool{}
	seen := map[string]bool{}
	var out []Superblock
	for _, e := range entries {
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		ns, err := os.Readlink(filepath.Join(k.procRoot(), pid, "ns", "mnt"))
		if err != nil || seenNS[ns] {
			continue // gone, or a namespace that is read
		}
		data, err := os.ReadFile(filepath.Join(k.procRoot(), pid, "mountinfo"))
		if err != nil {
			continue
		}
		mounts, err := ParseMountInfo(bytes.NewReader(data))
		if err != nil {
			continue
		}
		seenNS[ns] = true
		for _, mi := range mounts {
			d, ok := devs[mi.DevNum]
			if !ok && strings.HasPrefix(mi.DevNum, "0:") {
				d, ok = sourceDevice(mi.Source, devs)
			}
			if !ok || seen[mi.DevNum] {
				continue
			}
			seen[mi.DevNum] = true
			out = append(out, Superblock{Device: d, DevNum: mi.DevNum, Source: mi.Source, FSType: mi.FSType,
				ReadOnly: mi.SuperReadOnly(), Where: "pid " + pid + " at " + mi.Target})
		}
	}
	return append(out, k.unmatchedBtrfs(devs, out)...), nil
}

// sourceDevice returns the device node of devs that the source of a mount
// names, and whether there is one.
func sourceDevice(source string, devs map[string]string) (string, bool) {
	for _, d := range devs {
		if SameDevice(source, d) {
			return d, true
		}
	}
	return "", false
}

// unmatchedBtrfs returns a read-write superblock for each device node of
// devs that a mounted btrfs lists (/sys/fs/btrfs/<fsid>/devices/<name>) and
// that has no superblock in found: the RO state of that btrfs is not known.
func (k *Kernel) unmatchedBtrfs(devs map[string]string, found []Superblock) []Superblock {
	var out []Superblock
	for _, d := range slices.Sorted(maps.Values(devs)) {
		if slices.ContainsFunc(found, func(sb Superblock) bool { return sb.Device == d }) {
			continue
		}
		m, _ := filepath.Glob(filepath.Join(k.sysRoot(), "fs", "btrfs", "*", "devices", filepath.Base(d)))
		if len(m) == 0 {
			continue
		}
		fsid := filepath.Base(filepath.Dir(filepath.Dir(m[0])))
		out = append(out, Superblock{Device: d, Source: d, FSType: api.FSTypeBtrfs, Where: "btrfs " + fsid + ", no mount of it found"})
	}
	return out
}

// Holder returns what holds a block device, "" if nothing does: a loop
// device over it, or "in use" when it cannot be opened exclusively. A
// plain open of the device (a container's block device) is not a holder.
func (k *Kernel) Holder(dev string) (string, error) {
	for _, loop := range k.loopNames() {
		if v, err := k.LoopBackingFile(loop); err == nil && SameDevice(v, dev) {
			return loop, nil
		}
	}
	err := k.sys().OpenExclusive(dev)
	switch {
	case errors.Is(err, unix.EBUSY):
		return "in use", nil
	case isGone(err):
		return "", nil // the device is gone
	case err != nil:
		return "", fmt.Errorf("open %s: %w", dev, err)
	}
	return "", nil
}

// ReadHead returns the first n bytes of a device, fewer when it is
// shorter.
func (k *Kernel) ReadHead(dev string, n int) ([]byte, error) {
	f, err := os.Open(dev)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading %s: %w", dev, err)
	}
	return buf[:got], nil
}

// SetBlockDeviceReadOnly sets the read-only flag of the block device dev
// (BLKROSET) when it differs, and tells whether it changed it; the caller
// logs the change. A block device is never made read-only while a
// read-write superblock is on it, or on a loop device over it, in any mount
// namespace (EBUSY), nor when its superblocks cannot be read. A block
// device that is gone is not an error.
func (k *Kernel) SetBlockDeviceReadOnly(dev string, readOnly bool) (bool, error) {
	if dev == "" {
		return false, nil
	}
	sys := k.sys()
	cur, err := sys.ReadOnly(dev)
	switch {
	case isGone(err):
		return false, nil
	case err == nil && cur == readOnly:
		return false, nil
	}
	if readOnly {
		// never under a read-write superblock anywhere on the node; and
		// not when that cannot be known
		sbs, err := k.Superblocks(dev)
		if err != nil {
			return false, fmt.Errorf("not making %s read-only: its superblocks: %w", dev, err)
		}
		for _, sb := range sbs {
			if !sb.ReadOnly {
				return false, errorf(unix.EBUSY, "not making %s read-only: %s is mounted read-write in %s", dev, sb.Source, sb.Where)
			}
		}
	}
	if err := sys.SetReadOnly(dev, readOnly); err != nil {
		if isGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("making %s %s: %w", dev, RoWord(readOnly), err)
	}
	return true, nil
}

// RoWord returns "read-only" or "read-write".
func RoWord(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "read-write"
}

// Mount mounts the filesystem of source at target (mount(2)): binds, and
// filesystems of read-only types.
func (k *Kernel) Mount(source, target, fstype string, flags uintptr, data string) error {
	return k.sys().Mount(source, target, fstype, flags, data)
}

// FSMount mounts the filesystem of source at target with the new mount API,
// nosuid and nodev, its superblock read-only unless readWrite (see
// Sys.FSMount).
func (k *Kernel) FSMount(source, target, fstype string, readWrite bool, options []string) error {
	return k.sys().FSMount(source, target, fstype, readWrite, options)
}

// Remount makes the superblock of the mount at target read-write or
// read-only: every mount of the superblock changes. A reconfiguration to
// read-only while a file is open for writing fails with EBUSY.
func (k *Kernel) Remount(target string, readWrite bool) error {
	return k.sys().Remount(target, readWrite)
}

// Syncfs writes the dirty data of the filesystem of the mount at target to
// its device.
func (k *Kernel) Syncfs(target string) error {
	return k.sys().Syncfs(target)
}

// BindReadOnly binds the mount at source at target as a read-only, nosuid,
// nodev mount whose attributes reach the copies that propagate from it
// (open_tree with OPEN_TREE_CLONE, mount_setattr, move_mount).
func (k *Kernel) BindReadOnly(source, target string) error {
	return k.sys().BindReadOnly(source, target)
}

// Unmount unmounts target, never detached: a mount in use fails with
// EBUSY.
func (k *Kernel) Unmount(target string) error {
	return k.sys().Unmount(target)
}
