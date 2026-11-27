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

//go:build linux

package kernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// SysCalls is the Sys of the running system.
type SysCalls struct {
	// DevRoot is the device directory ("/dev"): the private device nodes
	// of dax devices are made in it, a filesystem without nodev.
	DevRoot string
	// Logf logs what is unexpected but not an error (nil: nothing).
	Logf func(format string, args ...any)
}

var _ Sys = &SysCalls{}

func (s *SysCalls) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *SysCalls) devRoot() string {
	if s.DevRoot != "" {
		return s.DevRoot
	}
	return "/dev"
}

func (s *SysCalls) Mount(source, target, fstype string, flags uintptr, data string) error {
	return unix.Mount(source, target, fstype, flags, data)
}

func (s *SysCalls) Unmount(target string) error {
	return unix.Unmount(target, 0)
}

func (s *SysCalls) FSMount(source, target, fstype string, readWrite bool, options []string) error {
	fd, err := unix.Fsopen(fstype, unix.FSOPEN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("fsopen %s: %w", fstype, err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.FsconfigSetString(fd, "source", source); err != nil {
		return fsContextError(fd, err)
	}
	if !readWrite {
		if err := unix.FsconfigSetFlag(fd, "ro"); err != nil {
			return fsContextError(fd, err)
		}
	}
	for _, o := range options {
		k, v, isString := strings.Cut(o, "=")
		if isString {
			err = unix.FsconfigSetString(fd, k, v)
		} else {
			err = unix.FsconfigSetFlag(fd, k)
		}
		if err != nil {
			return fsContextError(fd, fmt.Errorf("%s: %w", o, err))
		}
	}
	if err := unix.FsconfigCreate(fd); err != nil {
		return fsContextError(fd, err)
	}
	mfd, err := unix.Fsmount(fd, unix.FSMOUNT_CLOEXEC, unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV)
	if err != nil {
		return fsContextError(fd, fmt.Errorf("fsmount: %w", err))
	}
	defer func() { _ = unix.Close(mfd) }()
	if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount to %s: %w", target, err)
	}
	return nil
}

func (s *SysCalls) Remount(target string, readWrite bool) error {
	fd, err := unix.Fspick(unix.AT_FDCWD, target, unix.FSPICK_CLOEXEC)
	if err != nil {
		return fmt.Errorf("fspick %s: %w", target, err)
	}
	defer func() { _ = unix.Close(fd) }()
	flag := "ro"
	if readWrite {
		flag = "rw"
	}
	if err := unix.FsconfigSetFlag(fd, flag); err != nil {
		return fsContextError(fd, err)
	}
	if err := unix.FsconfigReconfigure(fd); err != nil {
		return fsContextError(fd, err)
	}
	return nil
}

// fsContextError returns err with the messages that the filesystem context
// of fd logged, if any.
func fsContextError(fd int, err error) error {
	var msgs []string
	buf := make([]byte, 4096)
	for range 64 {
		n, rerr := unix.Read(fd, buf)
		if rerr != nil || n <= 0 {
			break
		}
		msgs = append(msgs, strings.TrimSpace(string(buf[:n])))
	}
	if len(msgs) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.Join(msgs, "; "))
}

func (s *SysCalls) Syncfs(target string) error {
	fd, err := unix.Open(target, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Syncfs(fd); err != nil {
		return fmt.Errorf("syncfs %s: %w", target, err)
	}
	return nil
}

func (s *SysCalls) BindReadOnly(source, target string) error {
	fd, err := unix.OpenTree(unix.AT_FDCWD, source, unix.OPEN_TREE_CLONE|unix.O_CLOEXEC)
	if err != nil {
		return fmt.Errorf("open_tree %s: %w", source, err)
	}
	defer func() { _ = unix.Close(fd) }()
	attr := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV}
	if err := unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH, attr); err != nil {
		return fmt.Errorf("mount_setattr of the clone of %s: %w", source, err)
	}
	if err := unix.MoveMount(fd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount of the clone of %s to %s: %w", source, target, err)
	}
	return nil
}

func (s *SysCalls) SetReadOnly(dev string, readOnly bool) error {
	f, err := os.OpenFile(dev, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	v := 0
	if readOnly {
		v = 1
	}
	if err := unix.IoctlSetPointerInt(int(f.Fd()), unix.BLKROSET, v); err != nil {
		return fmt.Errorf("%s: BLKROSET %d: %w", dev, v, err)
	}
	return nil
}

func (s *SysCalls) ReadOnly(dev string) (bool, error) {
	f, err := os.OpenFile(dev, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	v, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKROGET)
	if err != nil {
		return false, fmt.Errorf("%s: BLKROGET: %w", dev, err)
	}
	return v != 0, nil
}

func (s *SysCalls) OpenExclusive(dev string) error {
	fd, err := unix.Open(dev, unix.O_RDONLY|unix.O_EXCL|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// sysLoop is a loop device of SysCalls.
type sysLoop struct {
	path string
	f    *os.File
}

func (l *sysLoop) Path() string { return l.path }

func (l *sysLoop) Close() error { return l.f.Close() }

func (l *sysLoop) Clear() error {
	err := unix.IoctlSetInt(int(l.f.Fd()), unix.LOOP_CLR_FD, 0)
	return errors.Join(err, l.f.Close())
}

// loopAttachTries is how many free loop devices ConfigureLoop tries when
// another process configures the one it got first.
const loopAttachTries = 5

func (s *SysCalls) ConfigureLoop(backing string, writable bool) (Loop, error) {
	// the kernel makes a loop device read-only when either its backing or
	// the loop device itself is not opened for writing
	mode := os.O_RDONLY
	if writable {
		mode = os.O_RDWR
	}
	bf, err := os.OpenFile(backing, mode, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = bf.Close() }()
	ctl, err := os.OpenFile(filepath.Join(s.devRoot(), "loop-control"), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ctl.Close() }()
	for try := 1; ; try++ {
		n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return nil, fmt.Errorf("%s: LOOP_CTL_GET_FREE: %w", ctl.Name(), err)
		}
		path := filepath.Join(s.devRoot(), "loop"+strconv.Itoa(n))
		lf, err := os.OpenFile(path, mode, 0)
		if err != nil {
			return nil, err
		}
		cfg := &unix.LoopConfig{Fd: uint32(bf.Fd())}
		cfg.Info.Flags = unix.LO_FLAGS_DIRECT_IO | unix.LO_FLAGS_AUTOCLEAR
		if !writable {
			cfg.Info.Flags |= unix.LO_FLAGS_READ_ONLY
		}
		copy(cfg.Info.File_name[:len(cfg.Info.File_name)-1], backing)
		err = unix.IoctlLoopConfigure(int(lf.Fd()), cfg)
		if errors.Is(err, unix.EBUSY) && try < loopAttachTries {
			_ = lf.Close()
			continue
		}
		if err != nil {
			_ = lf.Close()
			return nil, fmt.Errorf("%s: LOOP_CONFIGURE with %s: %w", path, backing, err)
		}
		// the kernel falls back to buffered I/O when the backing file
		// does not allow direct I/O
		if st, err := unix.IoctlLoopGetStatus64(int(lf.Fd())); err == nil && st.Flags&unix.LO_FLAGS_DIRECT_IO == 0 {
			s.logf("%s over %s does not use direct I/O: the page cache may hold the data twice", path, backing)
		}
		return &sysLoop{path: path, f: lf}, nil
	}
}

// LoopRdevice returns the device number of the backing of the loop device
// node path. Opening a loop device does not change it: an autoclear loop
// device is cleared only when its last opener (the mount of a cached node
// mount) closes it.
func (s *SysCalls) LoopRdevice(path string) (uint64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := unix.IoctlLoopGetStatus64(int(f.Fd()))
	if err != nil {
		return 0, fmt.Errorf("%s: LOOP_GET_STATUS64: %w", path, err)
	}
	return info.Rdevice, nil
}

// A dax device that the kernel binds to device_dax has its driver link
// before device_dax has added its character device: until then an open
// fails with ENXIO. ZeroDax and ProbeDax try again every daxOpenInterval
// for daxOpenTimeout.
var (
	daxOpenTimeout  = 5 * time.Second
	daxOpenInterval = 10 * time.Millisecond
	// openFile opens a file (a variable for tests).
	openFile = os.OpenFile
)

// openDaxNode opens the device node of a dax device read-only or for
// reading and writing. It tries again while the character device is not
// there yet, until wait has passed or the context ends.
func openDaxNode(ctx context.Context, node string, readOnly bool, wait time.Duration) (*os.File, error) {
	flag := os.O_RDWR
	if readOnly {
		flag = os.O_RDONLY
	}
	deadline := time.Now().Add(wait)
	for {
		f, err := openFile(node, flag, 0)
		notYet := errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENODEV)
		if !notYet || !time.Now().Before(deadline) {
			return f, err
		}
		if serr := sleep(ctx, daxOpenInterval); serr != nil {
			return nil, err
		}
	}
}

func (s *SysCalls) OpenDax(major, minor int, readOnly bool) (*os.File, error) {
	return s.openDax(context.Background(), major, minor, readOnly, 0)
}

// openDax opens the dax character device major:minor through a private
// device node in a temporary directory under DevRoot. It tries again for
// wait while the character device is not there yet. The container may have
// no /dev/daxX.Y. A device node can be opened only on a filesystem without
// nodev: DevRoot is a devtmpfs, not a tmpfs.
func (s *SysCalls) openDax(ctx context.Context, major, minor int, readOnly bool, wait time.Duration) (*os.File, error) {
	dir, err := os.MkdirTemp(s.devRoot(), daxNodeDirPrefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	node := filepath.Join(dir, "dax")
	if err := unix.Mknod(node, unix.S_IFCHR|0o600, int(unix.Mkdev(uint32(major), uint32(minor)))); err != nil {
		return nil, fmt.Errorf("mknod %d:%d: %w", major, minor, err)
	}
	return openDaxNode(ctx, node, readOnly, wait)
}

// zeroChunk is the size of the windows in which a dax device is zeroed.
var zeroChunk uint64 = 64 << 20

func (s *SysCalls) ZeroDax(ctx context.Context, major, minor int, size, align uint64) error {
	f, err := s.openDax(ctx, major, minor, false, daxOpenTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return ZeroFile(f, size, align)
}

// ZeroFile writes zeros to the first size bytes of a file through shared
// mappings of at most zeroChunk bytes, rounded up to a multiple of align.
func ZeroFile(f *os.File, size, align uint64) error {
	chunk := zeroChunk
	if align > 0 && chunk%align != 0 {
		chunk = (chunk/align + 1) * align
	}
	for off := uint64(0); off < size; off += chunk {
		n := min(chunk, size-off)
		b, err := unix.Mmap(int(f.Fd()), int64(off), int(n), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap %d bytes at %d: %w", n, off, err)
		}
		clear(b)
		if err := unix.Munmap(b); err != nil {
			return fmt.Errorf("munmap: %w", err)
		}
	}
	return nil
}

func (s *SysCalls) ProbeDax(ctx context.Context, major, minor int, size uint64) (*Aliasing, error) {
	if !cacheFlushSupported {
		return nil, ErrProbeUnsupported
	}
	f, err := s.openDax(ctx, major, minor, false, daxOpenTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	mem, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap %d:%d: %w", major, minor, err)
	}
	defer func() { _ = unix.Munmap(mem) }()
	return ProbeMemory(mem), nil
}
