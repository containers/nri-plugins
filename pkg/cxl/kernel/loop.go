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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// LoopDevice: a loop device /dev/loopN over a block device, the device of a
// cached node mount (a read-only, direct-I/O, autoclear loop device: one
// page-cache copy per node).
//
// Identity: its name; its backing is identified by the device number of the
// backing (LOOP_GET_STATUS64 lo_rdevice): the path in sysfs
// (loop/backing_file) is the path of the backing file as the reader sees
// it, relative to the root of a mount that the reader may not see ("/pmem1"
// for "/dev/pmem1" after a restart of the driver container).
// States: CONFIGURED (held by the fd of ConfigureLoop), MOUNTED (the fd closed,
// held by its mount), CLEARED (the kernel clears an autoclear loop device at
// its last close; Loop.Clear clears it before a mount). The
// loop driver does not hold its backing exclusively: Holder lists loop
// devices by backing.
// Waits: none; ConfigureLoop retries LOOP_CONFIGURE when another process took
// the free device first.

// LoopDevice is the observation of a loop device with a backing.
type LoopDevice struct {
	// Name is loopN.
	Name string `json:"name"`
	// DevNum is its major:minor.
	DevNum string `json:"devNum"`
	// Backing is its backing file: /dev/<name> of a block device, else
	// the path in sysfs.
	Backing string `json:"backing"`
}

// loopNames returns the names of the loop devices with a backing file.
func (k *Kernel) loopNames() []string {
	m, _ := filepath.Glob(k.blockDev("loop*", "loop", "backing_file"))
	out := make([]string, 0, len(m))
	for _, p := range m {
		out = append(out, filepath.Base(filepath.Dir(filepath.Dir(p))))
	}
	sortNat(out)
	return out
}

// LoopDevices returns the loop devices with a backing file.
func (k *Kernel) LoopDevices() []LoopDevice {
	var out []LoopDevice
	for _, loop := range k.loopNames() {
		l := LoopDevice{Name: loop}
		if n, err := k.blockDevNum(loop); err == nil {
			l.DevNum = n
		}
		l.Backing, _ = k.LoopBackingFile(loop)
		out = append(out, l)
	}
	return out
}

// LoopBackingFile returns the backing file of a loop device (/dev/loopN or
// loopN), "" if it has none: a block device as /dev/<its name>, whatever
// mount namespace set the loop device up, found by its device number; the
// path in sysfs when the device number cannot be found, and for a backing
// file that is not a block device.
func (k *Kernel) LoopBackingFile(loop string) (string, error) {
	b, err := os.ReadFile(k.blockDev(filepath.Base(loop), "loop", "backing_file"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(b))
	dev, err := k.loopBackingDevice(loop)
	switch {
	case err != nil:
		k.debugf("%s: the block device of its backing file %s: %v", loop, path, err)
	case dev != "":
		return dev, nil
	}
	return path, nil
}

// loopBackingDevice returns /dev/<name> of the block device that backs a
// loop device: the device number of the backing file (Sys.LoopRdevice) and
// its name in sysfs (/sys/dev/block/<major>:<minor>/uevent DEVNAME); ""
// when the backing file is not a block device.
func (k *Kernel) loopBackingDevice(loop string) (string, error) {
	rdev, err := k.sys().LoopRdevice(filepath.Join(k.devRoot(), filepath.Base(loop)))
	if err != nil {
		return "", err
	}
	if rdev == 0 {
		return "", nil
	}
	major, minor := unix.Major(rdev), unix.Minor(rdev)
	uevent := filepath.Join(k.sysRoot(), "dev", "block", fmt.Sprintf("%d:%d", major, minor), "uevent")
	data, err := os.ReadFile(uevent)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, ok := strings.CutPrefix(line, "DEVNAME="); ok && name != "" {
			return filepath.Join(k.devRoot(), name), nil
		}
	}
	return "", fmt.Errorf("no DEVNAME in %s", uevent)
}

// DevPath returns the path of the backing file of a loop device as a path
// under /dev when it is relative to the root of a devtmpfs mount that the
// reader does not see (a single component: "/pmem1"), else the path.
func DevPath(backing string) string {
	if filepath.Dir(backing) == "/" && backing != "/" {
		return "/dev" + backing
	}
	return backing
}

// SameDevice tells whether the backing file of a loop device, as the
// process that set it up saw its path, is the device node dev.
func SameDevice(backing, dev string) bool {
	backing = DevPath(backing)
	return backing == dev || (filepath.Dir(backing) == "/dev" && filepath.Base(backing) == filepath.Base(dev))
}

// ConfigureLoop returns a new direct-I/O, autoclear loop device over the file
// or block device backing (see Sys.ConfigureLoop).
func (k *Kernel) ConfigureLoop(backing string, writable bool) (Loop, error) {
	return k.sys().ConfigureLoop(backing, writable)
}
