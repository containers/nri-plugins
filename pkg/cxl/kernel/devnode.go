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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// deviceNodeWait bounds the wait of OpenDevice for a device node.
const deviceNodeWait = 5 * time.Second

// deviceNodePoll is the interval of the opens of OpenDevice.
const deviceNodePoll = 100 * time.Millisecond

// OpenDevice opens the device node path of a dax device (daxX.Y) or of a
// block device, read-only or read-write. udev makes the node after the
// driver bound the device, and the node of an earlier dax device of the
// same name may still carry the old numbers: the open waits up to 5 s while
// the node is missing (ENOENT, ENXIO) or, for a dax device, is not the char
// device of its sysfs numbers; then it opens a dax device through a private
// node (Sys.OpenDax).
func (k *Kernel) OpenDevice(ctx context.Context, path string, readOnly bool) (*os.File, error) {
	name := filepath.Base(path)
	var major, minor int
	numErr := errors.New("not a dax device")
	if strings.HasPrefix(name, "dax") {
		major, minor, numErr = k.DaxDevNumbers(name)
	}
	deadline := time.Now().Add(deviceNodeWait)
	for {
		f, err := openDeviceNode(path, readOnly, major, minor, numErr == nil)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, errOtherNode) && !nodeNotYet(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return k.openPrivateDax(path, readOnly, major, minor, numErr, err)
		}
		if serr := sleep(ctx, deviceNodePoll); serr != nil {
			return nil, err
		}
	}
}

// openPrivateDax opens the char device major:minor (numErr: their read
// failed) of the dax device of path through a private node, after the wait
// for its node ended with lastErr; a block device path fails with lastErr.
func (k *Kernel) openPrivateDax(path string, readOnly bool, major, minor int, numErr, lastErr error) (*os.File, error) {
	if !strings.HasPrefix(filepath.Base(path), "dax") {
		return nil, lastErr
	}
	if numErr != nil {
		return nil, fmt.Errorf("%w (and %v)", lastErr, numErr)
	}
	f, err := k.sys().OpenDax(major, minor, readOnly)
	if err != nil {
		return nil, fmt.Errorf("%w; opening private node %d:%d: %v", lastErr, major, minor, err)
	}
	k.logf("%s was not usable within %v (%v), opened %d:%d through a private node", path, deviceNodeWait, lastErr, major, minor)
	return f, nil
}

// errOtherNode is the error of a dax device node that is not the char
// device of the dax device yet.
var errOtherNode = errors.New("not the char device")

// nodeNotYet tells whether an open failed because the device node is not
// there yet: ENOENT or ENXIO.
func nodeNotYet(err error) bool {
	return errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENOENT)
}

// openDeviceNode opens the device node path, which must be the char device
// major:minor when check is set.
func openDeviceNode(path string, readOnly bool, major, minor int, check bool) (*os.File, error) {
	if check {
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err == nil {
			rdev := uint64(st.Rdev)
			if st.Mode&unix.S_IFMT != unix.S_IFCHR || int(unix.Major(rdev)) != major || int(unix.Minor(rdev)) != minor {
				return nil, fmt.Errorf("%s is %w %d:%d yet", path, errOtherNode, major, minor)
			}
		}
	}
	flag := os.O_RDWR
	if readOnly {
		flag = os.O_RDONLY
	}
	return os.OpenFile(path, flag, 0)
}
