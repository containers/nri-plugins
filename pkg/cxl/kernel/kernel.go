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
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Kernel observes and changes the CXL kernel objects of a node. It holds
// configuration only; the zero value of an optional field is the default
// of New.
type Kernel struct {
	// SysRoot is the sysfs mount point ("/sys").
	SysRoot string
	// ProcRoot is the procfs of the processes of the node ("/proc"): the
	// mount namespaces of every process are read there.
	ProcRoot string
	// DevRoot is the device directory ("/dev").
	DevRoot string
	// Exec runs a command (cxl, ndctl) and returns its combined output.
	Exec func(ctx context.Context, name string, args ...string) (string, error)
	// WriteFile writes a value to a sysfs attribute.
	WriteFile func(path, val string) error
	// Sys makes the system calls that are neither sysfs nor a command.
	Sys Sys
	// Events wakes the waits when the kernel objects change (nil: the
	// waits poll only).
	Events Events
	// Logf logs what is done (nil: nothing).
	Logf func(format string, args ...any)
	// Debugf logs the details of a lookup that falls back (nil: Logf).
	Debugf func(format string, args ...any)
	// PollInterval is the interval of the polls of the waits.
	PollInterval time.Duration

	// fact gets the errors of reading the facts of objects that the
	// observations tolerate (ObserveTopology does not); nil ignores them.
	fact func(error)
}

// Sys makes the system calls of the kernel objects that are not sysfs
// attributes or commands: mounts, the ioctls of block and loop devices,
// and the memory of dax devices.
type Sys interface {
	// Mount mounts the filesystem of source at target (mount(2)).
	Mount(source, target, fstype string, flags uintptr, data string) error
	// FSMount mounts the filesystem of source at target with the new mount
	// API: fsopen(fstype), the source, the flag "ro" unless readWrite, the
	// options ("key=value" as strings, others as flags),
	// FSCONFIG_CMD_CREATE, fsmount with MOUNT_ATTR_NOSUID|MOUNT_ATTR_NODEV,
	// move_mount. Its error wraps the errno and has the messages that the
	// filesystem context logged.
	FSMount(source, target, fstype string, readWrite bool, options []string) error
	// Remount makes the superblock of the mount at target read-write
	// or read-only (fspick, the flag "rw" or "ro",
	// FSCONFIG_CMD_RECONFIGURE).
	Remount(target string, readWrite bool) error
	// Syncfs writes the dirty data of the filesystem of the mount at
	// target to its device (syncfs(2)).
	Syncfs(target string) error
	// BindReadOnly binds the mount at source at target as a read-only,
	// nosuid, nodev mount (open_tree with OPEN_TREE_CLONE, mount_setattr,
	// move_mount).
	BindReadOnly(source, target string) error
	// Unmount unmounts target without detaching it (umount2(target, 0)).
	Unmount(target string) error
	// SetReadOnly sets or clears the read-only flag of a block device
	// (BLKROSET).
	SetReadOnly(dev string, readOnly bool) error
	// ReadOnly returns the read-only flag of a block device (BLKROGET).
	ReadOnly(dev string) (bool, error)
	// OpenExclusive opens a block device exclusively and closes it again:
	// EBUSY when something holds it.
	OpenExclusive(dev string) error
	// ConfigureLoop returns a new direct-I/O, autoclear loop device over the
	// file or block device backing: read-only, or with writable a loop
	// device whose backing is opened read-write.
	ConfigureLoop(backing string, writable bool) (Loop, error)
	// LoopRdevice returns the device number of the backing file of the
	// loop device node loop (lo_rdevice of LOOP_GET_STATUS64), 0 when the
	// backing is not a block device.
	LoopRdevice(loop string) (uint64, error)
	// ZeroDax writes zeros to the first size bytes of the dax character
	// device major:minor, in windows that are multiples of align. The
	// context ends the wait for the character device.
	ZeroDax(ctx context.Context, major, minor int, size, align uint64) error
	// ProbeDax runs the address line probe (see ProbeAliasing) on the
	// first size bytes of the dax character device major:minor. The
	// context ends the wait for the character device.
	ProbeDax(ctx context.Context, major, minor int, size uint64) (*Aliasing, error)
	// OpenDax opens the dax character device major:minor, read-only or
	// read-write, through a private device node that is removed again at
	// once. It does not wait for the character device.
	OpenDax(major, minor int, readOnly bool) (*os.File, error)
}

// Loop is a loop device set up by Sys.ConfigureLoop, held open until
// Close or Clear.
type Loop interface {
	// Path returns the device node of the loop device (/dev/loopN).
	Path() string
	// Close closes the loop device: an autoclear loop device goes away
	// when its last user (a mount) does.
	Close() error
	// Clear clears the backing file of the loop device and closes it.
	Clear() error
}

// Events tells when the kernel objects change.
type Events interface {
	// Changed returns a channel that is closed at the next change.
	Changed() <-chan struct{}
}

// defaultPollInterval is the interval of the polls of a Kernel without
// one.
const defaultPollInterval = 200 * time.Millisecond

// New returns a Kernel for the running system.
func New() *Kernel {
	return &Kernel{
		SysRoot:      "/sys",
		ProcRoot:     "/proc",
		DevRoot:      "/dev",
		Exec:         runCommand,
		WriteFile:    writeSysfs,
		Sys:          &SysCalls{DevRoot: "/dev"},
		PollInterval: defaultPollInterval,
	}
}

// runCommand runs a command and returns its combined output. Its error has
// the command, the error and the output.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// writeSysfs writes a value to a sysfs attribute.
func writeSysfs(path, val string) error {
	return os.WriteFile(path, []byte(val), 0o644)
}

// factErr passes err, an error of reading a fact of an object, to fact.
func (k *Kernel) factErr(err error) {
	if err != nil && k.fact != nil {
		k.fact(err)
	}
}

func (k *Kernel) logf(format string, args ...any) {
	if k.Logf != nil {
		k.Logf(format, args...)
	}
}

func (k *Kernel) debugf(format string, args ...any) {
	if k.Debugf != nil {
		k.Debugf(format, args...)
		return
	}
	k.logf(format, args...)
}

func (k *Kernel) writeFile(path, val string) error {
	if k.WriteFile != nil {
		return k.WriteFile(path, val)
	}
	return writeSysfs(path, val)
}

func (k *Kernel) exec(ctx context.Context, name string, args ...string) (string, error) {
	if k.Exec != nil {
		return k.Exec(ctx, name, args...)
	}
	return runCommand(ctx, name, args...)
}

// sys returns the system calls of the Kernel; SysCalls without a Logf of
// their own log with the Logf of the Kernel.
func (k *Kernel) sys() Sys {
	switch s := k.Sys.(type) {
	case nil:
		return &SysCalls{DevRoot: k.devRoot(), Logf: k.Logf}
	case *SysCalls:
		if s.Logf == nil && k.Logf != nil {
			c := *s
			c.Logf = k.Logf
			return &c
		}
	}
	return k.Sys
}

func (k *Kernel) sysRoot() string {
	if k.SysRoot != "" {
		return k.SysRoot
	}
	return "/sys"
}

func (k *Kernel) procRoot() string {
	if k.ProcRoot != "" {
		return k.ProcRoot
	}
	return "/proc"
}

func (k *Kernel) devRoot() string {
	if k.DevRoot != "" {
		return k.DevRoot
	}
	return "/dev"
}

func (k *Kernel) pollInterval() time.Duration {
	if k.PollInterval > 0 {
		return k.PollInterval
	}
	return defaultPollInterval
}
