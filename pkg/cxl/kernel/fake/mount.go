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

package fake

import (
	"context"
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// mountTable is the mount table, the loop devices and the block devices of
// the node: the Sys of the fake. Like the kernel it gives a second mount of
// a block device the superblock (and the super options) of the first one,
// refuses it with EBUSY when it would change the read-only state of the
// superblock, changes the superblock of every mount of a source at a
// remount, removes an autoclear loop device with its last mount,
// refuses a read-write superblock on a read-only block device, and makes a
// read-only mount of a dirty filesystem on a read-only device fail with
// EROFS. It records the violations of the rules of persistent devices: a
// block device made read-only under a read-write superblock, a read-only
// mount of a writable filesystem type on a writable block device, two
// superblocks of one image of which one is read-write; and those of the
// deletion of regions (Fake.deleteRegion). The mounts are rendered into
// the mountinfo files of the procfs of the fake: the mount namespace of
// the process (self and pid 1), and those of other processes (AddForeign).
type mountTable struct {
	f  *Fake
	mu sync.Mutex
	// cutAt stops the process after a mount at that target, "" for none.
	cutAt      string
	mounts     []kernel.MountInfo
	calls      []string
	loops      map[string]string // loop device -> backing
	nextLoop   int
	mountErr   map[string]error    // source -> error of Mount and FSMount
	ctxMsgs    map[string]string   // source -> messages of the filesystem context of a failed FSMount
	superOpts  map[string][]string // source -> super options of a new superblock
	busy       map[string]bool     // targets whose Unmount fails with EBUSY
	bindRoot   string              // the root of a new bind, "": that of its source
	holders    map[string]string   // block device -> holder, like a container's bind mount
	holderErr  error               // error of OpenExclusive
	detachErr  error               // error of Clear of a loop device
	bindFlags  []string            // the per-mount flags of a new bind, nil: those of its source
	ro         map[string]bool     // read-only flags (BLKROSET)
	roErr      map[string]error    // device -> error of SetReadOnly
	devices    map[string]uint64   // block device -> the serial of its memdev
	data       map[uint64][]byte   // serial -> the head of its block device
	dirty      map[uint64]bool     // serial -> its filesystem needs recovery
	loopRW     map[string]bool     // loop devices whose backing is opened read-write
	writing    map[string]bool     // targets with a file open for writing: a remount read-only is EBUSY
	syncErr    error               // error of Syncfs
	remountErr map[string]error    // target -> error of Remount
	// foreign are mounts in other mount namespaces (a container's), which
	// the process does not see, by the pid of a process in them
	foreign map[int][]kernel.MountInfo
	// anon are the anonymous device numbers of the btrfs superblocks by
	// source
	anon       map[string]string
	violations []string
}

var _ kernel.Sys = &mountTable{}

func newMountTable(f *Fake) *mountTable {
	return &mountTable{
		f:          f,
		loops:      map[string]string{},
		mountErr:   map[string]error{},
		ctxMsgs:    map[string]string{},
		superOpts:  map[string][]string{},
		busy:       map[string]bool{},
		holders:    map[string]string{},
		ro:         map[string]bool{},
		roErr:      map[string]error{},
		devices:    map[string]uint64{},
		data:       map[uint64][]byte{},
		dirty:      map[uint64]bool{},
		loopRW:     map[string]bool{},
		writing:    map[string]bool{},
		remountErr: map[string]error{},
		foreign:    map[int][]kernel.MountInfo{},
		anon:       map[string]string{},
	}
}

// mountCall is the format of a mount call in the call log.
const mountCall = "mount %s %s %s 0x%x %s"

func (m *mountTable) call(format string, args ...any) {
	m.calls = append(m.calls, fmt.Sprintf(format, args...))
}

// violate records a violation of the rules. Called with m.mu held.
func (m *mountTable) violate(format string, args ...any) {
	m.violations = append(m.violations, fmt.Sprintf(format, args...))
}

// image returns the block device under a source: the backing of a loop
// device, else the source. Called with m.mu held.
func (m *mountTable) image(source string) string {
	if b, ok := m.loops[source]; ok {
		return kernel.DevPath(b)
	}
	return source
}

// superblock returns a mount of the superblock of a block device source,
// nil if it has none (a source that is not a path, such as tmpfs, gets a
// new superblock for every mount). Called with m.mu held.
func (m *mountTable) superblock(source string) *kernel.MountInfo {
	if !strings.HasPrefix(source, "/") {
		return nil
	}
	for _, mi := range m.allMounts() {
		if mi.Source == source {
			return &mi
		}
	}
	return nil
}

// rwSuper returns a mount with a read-write superblock on a source that
// match accepts. Called with m.mu held.
func (m *mountTable) rwSuper(match func(source string) bool) *kernel.MountInfo {
	for _, mi := range m.allMounts() {
		if match(mi.Source) && !mi.SuperReadOnly() {
			return &mi
		}
	}
	return nil
}

// allMounts returns the mounts of every mount namespace. Called with m.mu
// held.
func (m *mountTable) allMounts() []kernel.MountInfo {
	all := slices.Clone(m.mounts)
	for _, pid := range slices.Sorted(maps.Keys(m.foreign)) {
		all = append(all, m.foreign[pid]...)
	}
	return all
}

// devNum returns the major:minor of the superblock of a source of the
// type: an anonymous device number (major 0) for btrfs, as the kernel gives
// it, else the number of the block device. Called with m.mu held.
func (m *mountTable) devNum(source, fstype string) string {
	if fstype == api.FSTypeBtrfs {
		if _, ok := m.anon[source]; !ok {
			m.anon[source] = "0:" + strconv.Itoa(40+len(m.anon))
		}
		return m.anon[source]
	}
	if strings.HasPrefix(filepath.Base(source), "loop") {
		if n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(source), "loop")); err == nil {
			return "7:" + strconv.Itoa(n)
		}
	}
	if v := m.f.Read("class/block/" + filepath.Base(source) + "/dev"); v != "" {
		return v
	}
	return "0:99"
}

// render writes the mountinfo of the mount namespaces into the procfs of
// the fake. Called with m.mu held.
func (m *mountTable) render() {
	write := func(pid string, ns int, mounts []kernel.MountInfo) {
		var b strings.Builder
		for i, mi := range mounts {
			opts := strings.Join(mi.SuperOpts, ",")
			if opts == "" {
				opts = "rw"
			}
			fmt.Fprintf(&b, "%d 1 %s %s %s %s - %s %s %s\n", 100+i, mi.DevNum, escape(mi.Root), escape(mi.Target),
				strings.Join(mi.MountFlags, ","), mi.FSType, escape(mi.Source), opts)
		}
		m.f.writeProc(pid+"/mountinfo", b.String())
		if pid != "self" {
			link := filepath.Join(m.f.ProcRoot(), pid, "ns", "mnt")
			_ = os.MkdirAll(filepath.Dir(link), 0o755)
			_ = os.Remove(link)
			_ = os.Symlink(fmt.Sprintf("mnt:[%d]", 4026531840+ns), link)
		}
	}
	write("self", 0, m.mounts)
	write("1", 0, m.mounts)
	for pid, mounts := range m.foreign {
		write(strconv.Itoa(pid), pid, mounts)
	}
	m.f.changed()
}

// escape returns a mountinfo field with spaces, tabs, newlines and
// backslashes escaped.
func escape(s string) string {
	r := strings.NewReplacer(" ", `\040`, "\t", `\011`, "\n", `\012`, `\`, `\134`)
	return r.Replace(s)
}

// cut stops the process after a mount at the target of cutAt. Called with
// m.mu held, which the deferred unlock of the caller releases.
func (m *mountTable) cut(target string) {
	if m.cutAt == "" || filepath.Clean(target) != m.cutAt {
		return
	}
	m.cutAt = ""
	panic(ErrCut)
}

func (m *mountTable) Mount(source, target, fstype string, flags uintptr, data string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.call(mountCall, source, target, fstype, flags, data)
	if err := m.mountErr[source]; err != nil {
		return err
	}
	if _, err := os.Stat(target); err != nil {
		return err
	}
	if flags&syscall.MS_BIND != 0 {
		// a bind has the superblock and the per-mount flags of its source
		src := kernel.FindMount(m.mounts, source)
		if src == nil {
			return syscall.EINVAL
		}
		bind := *src
		bind.Target = filepath.Clean(target)
		bind.MountFlags = slices.Clone(src.MountFlags)
		if m.bindFlags != nil {
			bind.MountFlags = slices.Clone(m.bindFlags)
		}
		if m.bindRoot != "" {
			bind.Root = m.bindRoot
		}
		m.mounts = append(m.mounts, bind)
		m.render()
		m.cut(target)
		return nil
	}
	opts := []string{"ro", data}
	if o, ok := m.superOpts[source]; ok {
		opts = o
	}
	if mi := m.superblock(source); mi != nil {
		if mi.SuperReadOnly() != (flags&syscall.MS_RDONLY != 0) {
			return fmt.Errorf("%w: Can't mount, would change RO state", syscall.EBUSY)
		}
		opts = mi.SuperOpts
	}
	m.mounts = append(m.mounts, kernel.MountInfo{DevNum: m.devNum(source, fstype), Root: "/", Target: filepath.Clean(target), FSType: fstype,
		Source: source, SuperOpts: slices.Clone(opts), MountFlags: []string{"ro", "nosuid", "nodev"}})
	m.render()
	m.cut(target)
	return nil
}

func (m *mountTable) FSMount(source, target, fstype string, readWrite bool, options []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.call("fsmount %s %s %s %s %s", source, target, fstype, map[bool]string{true: "rw", false: "ro"}[readWrite], strings.Join(options, ","))
	if err := m.mountErr[source]; err != nil {
		if msg := m.ctxMsgs[source]; msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	if _, err := os.Stat(target); err != nil {
		return err
	}
	img := m.image(source)
	devRO := m.ro[source]
	_, isLoop := m.loops[source]
	switch {
	case readWrite && devRO:
		return syscall.EACCES
	case readWrite && isLoop && m.ro[img]:
		return syscall.EIO // the loop device writes to a read-only backing
	case !readWrite && devRO && m.dirty[m.devices[img]]:
		return syscall.EROFS
	case !readWrite && !devRO:
		m.violate("fsmount of %s at %s read-only on a writable block device", source, target)
	}
	var opts []string
	if mi := m.superblock(source); mi != nil {
		if mi.SuperReadOnly() == readWrite {
			return fmt.Errorf("%w: Can't mount, would change RO state", syscall.EBUSY)
		}
		opts = mi.SuperOpts
	}
	if opts == nil {
		// a new superblock: another one of the same image is another cache
		for _, mi := range m.mounts {
			if mi.Source != source && m.image(mi.Source) == img {
				if fstype == api.FSTypeXfs && !slices.Contains(options, "nouuid") {
					return fmt.Errorf("%w: Filesystem has duplicate UUID", syscall.EINVAL)
				}
				if readWrite || !mi.SuperReadOnly() {
					m.violate("a second superblock of %s (%s and %s), one read-write", img, mi.Source, source)
				}
			}
		}
		opts = append([]string{map[bool]string{true: "rw", false: "ro"}[readWrite]}, options...)
		if o, ok := m.superOpts[source]; ok {
			opts = o
		}
	}
	m.mounts = append(m.mounts, kernel.MountInfo{DevNum: m.devNum(source, fstype), Root: "/", Target: filepath.Clean(target), FSType: fstype,
		Source: source, SuperOpts: slices.Clone(opts), MountFlags: []string{"rw", "nosuid", "nodev"}})
	m.render()
	m.cut(target)
	return nil
}

func (m *mountTable) Remount(target string, readWrite bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.call("remount %s %s", target, map[bool]string{true: "rw", false: "ro"}[readWrite])
	if err := m.remountErr[target]; err != nil {
		return err
	}
	mi := kernel.FindMount(m.mounts, target)
	if mi == nil {
		return syscall.EINVAL
	}
	source := mi.Source
	img := m.image(source)
	_, isLoop := m.loops[source]
	switch {
	case readWrite && m.ro[source]:
		return syscall.EACCES
	case readWrite && isLoop && m.ro[img]:
		return syscall.EIO
	case !readWrite && m.writing[target]:
		return syscall.EBUSY
	}
	if readWrite {
		for _, o := range m.mounts {
			if o.Source != source && m.image(o.Source) == img {
				m.violate("%s made read-write next to the superblock of %s", source, o.Source)
			}
		}
	}
	for i := range m.mounts {
		if m.mounts[i].Source == source && len(m.mounts[i].SuperOpts) > 0 {
			opts := slices.Clone(m.mounts[i].SuperOpts)
			opts[0] = map[bool]string{true: "rw", false: "ro"}[readWrite]
			m.mounts[i].SuperOpts = opts
		}
	}
	m.render()
	m.cut(target)
	return nil
}

func (m *mountTable) Syncfs(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.call("syncfs %s", target)
	return m.syncErr
}

func (m *mountTable) BindReadOnly(source, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.call("robind %s %s", source, target)
	if _, err := os.Stat(target); err != nil {
		return err
	}
	src := kernel.FindMount(m.mounts, source)
	if src == nil {
		return syscall.EINVAL
	}
	bind := *src
	bind.Target = filepath.Clean(target)
	bind.MountFlags = []string{"ro", "nosuid", "nodev"}
	if m.bindFlags != nil {
		bind.MountFlags = slices.Clone(m.bindFlags)
	}
	if m.bindRoot != "" {
		bind.Root = m.bindRoot
	}
	m.mounts = append(m.mounts, bind)
	m.render()
	m.cut(target)
	return nil
}

func (m *mountTable) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.call("umount %s", target)
	if m.busy[target] {
		return syscall.EBUSY
	}
	mi := kernel.FindMount(m.mounts, target)
	if mi == nil {
		return syscall.EINVAL
	}
	source := mi.Source
	// the topmost mount at the target goes
	for i := len(m.mounts) - 1; i >= 0; i-- {
		if m.mounts[i].Target == filepath.Clean(target) {
			m.mounts = slices.Delete(m.mounts, i, i+1)
			break
		}
	}
	m.autoclear(source)
	m.render()
	return nil
}

// autoclear removes an autoclear loop device without mounts. Called with
// m.mu held.
func (m *mountTable) autoclear(loop string) {
	if _, ok := m.loops[loop]; !ok {
		return
	}
	for _, mi := range m.mounts {
		if mi.Source == loop {
			return
		}
	}
	m.clearLoop(loop)
}

// clearLoop removes a loop device. Called with m.mu held.
func (m *mountTable) clearLoop(loop string) {
	delete(m.loops, loop)
	delete(m.loopRW, loop)
	delete(m.ro, loop)
	name := filepath.Base(loop)
	n := strings.TrimPrefix(name, "loop")
	_ = os.RemoveAll(m.f.P("block/" + name))
	_ = os.Remove(m.f.P("class/block/" + name))
	_ = os.RemoveAll(m.f.P("dev/block/7:" + n))
	_ = os.Remove(loop)
	m.f.changed()
}

func (m *mountTable) SetReadOnly(dev string, readOnly bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.call("blkroset %s %d", dev, map[bool]int{true: 1, false: 0}[readOnly])
	if err := m.roErr[dev]; err != nil {
		return err
	}
	if _, err := os.Stat(dev); err != nil {
		return err
	}
	if readOnly {
		if mi := m.rwSuper(func(s string) bool { return s == dev || m.image(s) == dev }); mi != nil {
			m.violate("%s made read-only under the read-write superblock of %s at %s", dev, mi.Source, mi.Target)
		}
	}
	m.ro[dev] = readOnly
	m.cut(dev)
	return nil
}

func (m *mountTable) ReadOnly(dev string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(dev); err != nil {
		return false, err
	}
	return m.ro[dev], nil
}

func (m *mountTable) OpenExclusive(dev string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holderErr != nil {
		return m.holderErr
	}
	if _, err := os.Stat(dev); err != nil {
		return err
	}
	if m.holders[dev] != "" {
		return syscall.EBUSY
	}
	for _, mi := range m.allMounts() {
		if mi.Source == dev {
			return syscall.EBUSY
		}
	}
	return nil
}

// loop is a loop device of the fake, held open until Release.
type loop struct {
	m    *mountTable
	path string
}

func (l *loop) Path() string { return l.path }

func (l *loop) Close() error {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	l.m.call("loop-close %s", l.path)
	l.m.autoclear(l.path)
	return nil
}

func (l *loop) Clear() error {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	l.m.call("loop-clear %s", l.path)
	if l.m.detachErr != nil {
		return l.m.detachErr
	}
	l.m.clearLoop(l.path)
	return nil
}

func (m *mountTable) ConfigureLoop(backing string, writable bool) (kernel.Loop, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := os.Stat(backing)
	if err != nil {
		return nil, err
	}
	n := m.nextLoop
	m.nextLoop++
	path := filepath.Join(m.f.DevRoot(), "loop"+strconv.Itoa(n))
	if writable {
		m.call("loop %s %s rw", path, backing)
	} else {
		m.call("loop %s %s", path, backing)
	}
	m.loops[path] = backing
	m.loopRW[path] = writable
	// a read-only loop device is read-only; a writable one is not, also
	// over a read-only backing
	m.ro[path] = !writable
	name := filepath.Base(path)
	_ = os.Remove(path)
	if err := os.Symlink(backing, path); err != nil {
		return nil, err
	}
	for rel, val := range map[string]string{
		"block/" + name + "/dev":                     "7:" + strconv.Itoa(n),
		"block/" + name + "/size":                    strconv.FormatInt(st.Size()/512, 10),
		"block/" + name + "/loop/backing_file":       backing,
		"dev/block/7:" + strconv.Itoa(n) + "/uevent": "MAJOR=7\nMINOR=" + strconv.Itoa(n) + "\nDEVNAME=" + name,
	} {
		if err := writeFile(m.f.P(rel), val, m.f.tmp); err != nil {
			return nil, err
		}
	}
	// /sys/class/block/loopN, as for every block device
	m.f.Link("class/block/"+name, "../../block/"+name)
	m.f.changed()
	return &loop{m: m, path: path}, nil
}

func (m *mountTable) LoopRdevice(path string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	backing, ok := m.loops[path]
	if !ok {
		return 0, syscall.ENXIO
	}
	ma, mi, ok := strings.Cut(m.f.Read("class/block/"+filepath.Base(backing)+"/dev"), ":")
	major, err1 := strconv.ParseUint(ma, 10, 32)
	minor, err2 := strconv.ParseUint(mi, 10, 32)
	if !ok || err1 != nil || err2 != nil {
		return 0, nil
	}
	return unix.Mkdev(uint32(major), uint32(minor)), nil
}

func (m *mountTable) ZeroDax(ctx context.Context, major, minor int, size, align uint64) error {
	f := m.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ZeroErr != nil {
		return f.ZeroErr
	}
	f.zeroed = append(f.zeroed, fmt.Sprintf("%d:%d %d %d", major, minor, size, align))
	return nil
}

func (m *mountTable) OpenDax(major, minor int, readOnly bool) (*os.File, error) {
	return nil, fmt.Errorf("no private node of %d:%d in the fake: %w", major, minor, syscall.ENXIO)
}

func (m *mountTable) ProbeDax(ctx context.Context, major, minor int, size uint64) (*kernel.Aliasing, error) {
	f := m.f
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, fmt.Sprintf("%d:%d %d", major, minor, size))
	if f.cutProbe {
		f.cutProbe = false
		panic(ErrCut)
	}
	if a := f.Aliasing; a != nil {
		f.Aliasing = nil
		return a, nil
	}
	return &kernel.Aliasing{Garbage: -1}, nil
}

// addBlockDevice registers the block device dev of the memdev with the
// serial: its device node holds the data of the memdev.
func (m *mountTable) addBlockDevice(dev string, serial uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devices[dev] = serial
	_ = os.MkdirAll(filepath.Dir(dev), 0o755)
	_ = os.WriteFile(dev, m.data[serial], 0o644)
}

// removeBlockDevice removes the block device dev.
func (m *mountTable) removeBlockDevice(dev string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.devices, dev)
	delete(m.ro, dev)
	_ = os.Remove(dev)
}

// reboot clears the mounts, loop devices and block devices.
func (m *mountTable) reboot() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for l := range m.loops {
		m.clearLoop(l)
	}
	m.mounts = nil
	for pid := range m.foreign {
		_ = os.RemoveAll(filepath.Join(m.f.ProcRoot(), strconv.Itoa(pid)))
	}
	m.foreign = map[int][]kernel.MountInfo{}
	m.ro = map[string]bool{}
	m.holders = map[string]string{}
	m.render()
}

// FSImage returns the head of a block device with the signatures of the
// filesystem types (none for ""): erofs, squashfs, ext4 (ext4-dirty: needs
// recovery), xfs, btrfs, gfs2 (lock_dlm), gfs2-nolock (lock_nolock).
func FSImage(types ...string) []byte {
	img := make([]byte, kernel.FSProbeBytes)
	for _, t := range types {
		switch t {
		case "":
		case "erofs":
			binary.LittleEndian.PutUint32(img[1024:], 0xE0F5E1E2)
		case "squashfs":
			copy(img, "hsqs")
		case "ext4", "ext4-dirty":
			binary.LittleEndian.PutUint16(img[1080:], 0xEF53)
			if t == "ext4-dirty" {
				binary.LittleEndian.PutUint32(img[1024+0x60:], 0x4|0x2)
			} else {
				binary.LittleEndian.PutUint32(img[1024+0x60:], 0x2)
			}
		case "xfs":
			copy(img, "XFSB")
		case "btrfs":
			copy(img[65600:], "_BHRfS_M")
		case "gfs2", "gfs2-nolock":
			binary.BigEndian.PutUint32(img[65536:], 0x01161970)
			binary.BigEndian.PutUint32(img[65540:], 1)
			proto := "lock_dlm"
			if t == "gfs2-nolock" {
				proto = "lock_nolock"
			}
			copy(img[65536+96:], proto+"\x00")
		default:
			panic("FSImage: unknown type " + t)
		}
	}
	return img
}

// SetImage sets the head of the block device dev (and of the data of its
// memdev), as mkfs leaves it.
func (f *Fake) SetImage(dev string, head []byte) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	if serial, ok := m.devices[dev]; ok {
		m.data[serial] = slices.Clone(head)
	}
	_ = os.WriteFile(dev, head, 0o644)
}

// SetDirty sets whether the filesystem of the block device dev needs
// recovery.
func (f *Fake) SetDirty(dev string, dirty bool) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirty[m.devices[dev]] = dirty
}

// SetBusy makes the unmount of target fail with EBUSY, or not.
func (f *Fake) SetBusy(target string, busy bool) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy[target] = busy
}

// SetWriting tells whether a file is open for writing under target: a
// remount read-only fails with EBUSY.
func (f *Fake) SetWriting(target string, writing bool) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writing[target] = writing
}

// SetMountError makes the mounts of source fail with err and the messages
// of the filesystem context msg ("" for none).
func (f *Fake) SetMountError(source string, err error, msg string) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mountErr[source] = err
	m.ctxMsgs[source] = msg
}

// SetSuperOpts sets the super options of a new superblock of source.
func (f *Fake) SetSuperOpts(source string, opts ...string) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.superOpts[source] = opts
}

// SetHolder makes something hold the block device dev ("": nothing).
func (f *Fake) SetHolder(dev, holder string) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holders[dev] = holder
}

// SetReadOnlyError makes the BLKROSET of dev fail with err.
func (f *Fake) SetReadOnlyError(dev string, err error) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roErr[dev] = err
}

// SetSyscallErrors sets the errors of the exclusive open of block devices,
// of the detach of loop devices and of syncfs (nil: none).
func (f *Fake) SetSyscallErrors(holder, detach, sync error) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holderErr, m.detachErr, m.syncErr = holder, detach, sync
}

// SetRemountError makes the remount of target fail with err.
func (f *Fake) SetRemountError(target string, err error) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remountErr[target] = err
}

// SetBind sets the root and the per-mount flags of the next binds ("" and
// nil: those of their source).
func (f *Fake) SetBind(root string, flags []string) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindRoot, m.bindFlags = root, flags
}

// AddForeign adds a mount of source in the mount namespace of the process
// pid, with a read-only or read-write superblock.
func (f *Fake) AddForeign(pid int, source, target, fstype string, readOnly bool) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.foreign[pid] = append(m.foreign[pid], kernel.MountInfo{DevNum: m.devNum(source, fstype), Root: "/", Target: target, FSType: fstype, Source: source,
		SuperOpts: []string{map[bool]string{true: "ro", false: "rw"}[readOnly]}, MountFlags: []string{"ro", "nosuid", "nodev"}})
	m.render()
}

// RemoveForeign removes the mounts of the mount namespace of the process
// pid.
func (f *Fake) RemoveForeign(pid int) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.foreign, pid)
	_ = os.RemoveAll(filepath.Join(f.ProcRoot(), strconv.Itoa(pid)))
	m.render()
}

// CutMount stops the process after a mount at target.
func (f *Fake) CutMount(target string) {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cutAt = filepath.Clean(target)
}

// MountCalls returns the system calls of the mount table that begin with
// prefix ("mount", "fsmount", "umount", "blkroset", "loop", ...).
func (f *Fake) MountCalls(prefix string) []string {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// BindCall returns the call that MountCalls lists for a bind mount of
// source at target.
func BindCall(source, target string) string {
	return fmt.Sprintf(mountCall, source, target, "", unix.MS_BIND, "")
}

// MountTable returns the mounts of the mount namespace of the process.
func (f *Fake) MountTable() []kernel.MountInfo {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.mounts)
}

// IsReadOnly returns the read-only flag of a block device.
func (f *Fake) IsReadOnly(dev string) bool {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ro[dev]
}

// Violations returns the violations of the rules of persistent devices and
// of the deletion of regions.
func (f *Fake) Violations() []string {
	m := f.mnt
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.violations)
}

// HideProc makes the procfs of the fake unreadable, or readable again: the
// mount namespaces and the superblocks cannot be read then.
func (f *Fake) HideProc(hidden bool) {
	if hidden {
		_ = os.Rename(f.ProcRoot(), f.ProcRoot()+".hidden")
	} else {
		_ = os.Rename(f.ProcRoot()+".hidden", f.ProcRoot())
	}
	f.changed()
}
