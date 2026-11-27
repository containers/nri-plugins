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

// Package fake is the kernel's side of the CXL kernel objects of a node
// for tests: a sysfs tree, a procfs and a /dev in temporary directories,
// the cxl and ndctl commands (any other command, daxctl too, is an error),
// the sysfs attributes that the kernel acts on when they are written, and
// the system calls of package kernel (mounts, block and loop devices, the
// memory of dax devices), with the rules of the kernel that the driver
// depends on:
//
//   - memdevs under host bridges and switches, enabled and disabled, with
//     the endpoint port appearing after the driver link (EndpointDelay);
//   - regions allocated first-fit in the window of their root decoder,
//     named by the lowest free id, pushed on the decoder stack of every
//     port of the decoder path; a port with a hole refuses a new region
//     with EBUSY; destroying the top region pops it and reaps the holes
//     below; a memdev removed under a region leaves it hollow;
//   - the dynamic dax region of a ram region: dax devices with first-fit
//     memory, explicit placement through mapping, the id lists of the dax
//     drivers (new_id attaches a driver to every unbound device that it
//     matches, kmem also by type), kmem adding and removing memory blocks;
//   - memory blocks onlined and offlined, valid_zones, a udev rule that
//     onlines hot-added blocks again (AutoOnline);
//   - the nd region of a pmem region probed asynchronously (PmemDelay),
//     namespaces from the labels of the memdev, which outlive the region;
//   - one superblock per block device, mounts in several mount namespaces,
//     autoclear loop devices, the read-only flag of block devices, and the
//     violations of the rules of persistent devices and of the deletion of
//     regions (Violations);
//   - a reboot: a new boot id, no region, dax device, memory block, mount
//     or loop device of the old boot, new device numbers, labels kept.
//
// It does not model the locks of the dax bus (nothing blocks), the
// alignment of kmem to memory blocks beyond whole blocks, the endpoint
// decoder order check of cxl destroy-region, the region ids cached per
// root decoder (every decoder offers the lowest free id), or NUMA node
// directories that come and go with memory (those of the windows always
// exist).
//
// Its hooks are those of the tests of the driver: a command or a sysfs
// write that fails (FailCommand, FailWrite), a function that runs before
// or after one (Before, After), a process stop before or after one
// (CutBefore, CutAfter, CutMount, CutProbe: a panic of ErrCut).
package fake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// ErrCut is the panic of a process stop (CutBefore, CutAfter, CutMount,
// CutProbe): the test recovers it and starts the code again on the same
// fake.
var ErrCut = errors.New("killed")

// Fake is the fake kernel of a node. Its fields configure the kernel's
// behaviour; set them before the code under test runs.
type Fake struct {
	tb   testing.TB
	root string
	tmp  string

	// PmemDelay is how long each step of the probe of a new nd region
	// takes (0: probed before cxl create-region returns).
	PmemDelay time.Duration
	// EndpointDelay is how long after its driver link the endpoint port of
	// a memdev that is plugged or enabled appears (the add_dev race).
	EndpointDelay time.Duration
	// NoKmemBlocks: kmem adds no memory blocks.
	NoKmemBlocks bool
	// AutoOnline is how many times a udev rule onlines a memory block that
	// kmem added or that was offlined, each time after a moment.
	AutoOnline int
	// Aliasing is the result of the next address line probes, nil for no
	// aliasing.
	Aliasing *kernel.Aliasing
	// ZeroErr is the error of zeroing.
	ZeroErr error

	mu       sync.Mutex
	changes  kernel.Changes
	cmds     []string
	writes   []string
	zeroed   []string
	probed   []string
	before   map[string]func()
	after    map[string]func()
	cutAt    map[string]string // prefix -> "before" or "after"
	cutProbe bool
	failCmd  map[string][]error
	failWr   map[string][]error

	bootID     int
	memdevs    map[uint64]*memdev // by serial
	labels     map[uint64]bool    // the LSA of the memdev has an fsdax namespace
	ids        map[string]map[string]bool
	daxMinor   map[int]bool // minors of dax devices in use
	pmemMinor  int          // next minor of a pmem block device
	nextPort   int
	nextEP     int
	hostBridge map[int]string // uid -> port name
	probes     sync.WaitGroup
	autoOnline map[int]int // block -> times it was onlined by "udev"

	mnt *mountTable
}

// New returns a fake kernel with an empty node: memory blocks of 128 MiB,
// NUMA nodes 0 and 1 of DRAM, and the CXL root with two host bridges (uids
// 12 and 24) and their windows (see Window).
func New(tb testing.TB) *Fake {
	f := &Fake{
		tb:         tb,
		root:       tb.TempDir(),
		tmp:        tb.TempDir(),
		before:     map[string]func(){},
		after:      map[string]func(){},
		cutAt:      map[string]string{},
		failCmd:    map[string][]error{},
		failWr:     map[string][]error{},
		memdevs:    map[uint64]*memdev{},
		labels:     map[uint64]bool{},
		ids:        map[string]map[string]bool{},
		daxMinor:   map[int]bool{},
		nextPort:   1,
		nextEP:     4,
		hostBridge: map[int]string{},
		autoOnline: map[int]int{},
	}
	// a probe outliving a failed test must not write into its removed
	// temporary directory
	tb.Cleanup(f.probes.Wait)
	f.mnt = newMountTable(f)
	f.boot()
	f.writeProc("filesystems", "nodev\tproc\nnodev\ttmpfs\n\text4\n\txfs\n\terofs\n\tbtrfs\n\tsquashfs\n\tgfs2\n")
	f.Write("devices/system/memory/block_size_bytes", "8000000")
	for n, kb := range map[int]uint64{0: 8 << 20, 1: 8 << 20} {
		f.Write(fmt.Sprintf("devices/system/node/node%d/meminfo", n), fmt.Sprintf("Node %d MemTotal:       %d kB", n, kb))
	}
	f.Write("devices/system/node/has_memory", "0-1")
	for _, d := range []string{"bus/cxl/drivers/cxl_mem", "bus/cxl/drivers/cxl_region", "bus/dax/drivers/kmem",
		"bus/dax/drivers/device_dax", "bus/dax/devices", "bus/nd/drivers/nd_region", "bus/nd/drivers/nd_pmem", "bus/nd/devices"} {
		f.mkdir(d)
	}
	f.Link("bus/cxl/devices/root0", "../../../"+rootPath)
	f.addHostBridge(12, 0x0c)
	f.addHostBridge(24, 0x18)
	for _, w := range windows {
		f.addWindow(w)
	}
	f.mnt.render()
	return f
}

// Kernel returns a kernel.Kernel on the fake.
func (f *Fake) Kernel() *kernel.Kernel {
	return &kernel.Kernel{
		SysRoot:      f.SysRoot(),
		ProcRoot:     f.ProcRoot(),
		DevRoot:      f.DevRoot(),
		Exec:         f.Exec,
		WriteFile:    f.WriteAttr,
		Sys:          f.mnt,
		Events:       f,
		Logf:         f.tb.Logf,
		PollInterval: 5 * time.Millisecond,
	}
}

// SysRoot returns the sysfs mount point of the fake.
func (f *Fake) SysRoot() string { return filepath.Join(f.root, "sys") }

// ProcRoot returns the procfs of the fake.
func (f *Fake) ProcRoot() string { return filepath.Join(f.root, "proc") }

// DevRoot returns the device directory of the fake.
func (f *Fake) DevRoot() string { return filepath.Join(f.root, "dev") }

// Changed returns a channel that is closed at the next change of the fake
// (kernel.Events).
func (f *Fake) Changed() <-chan struct{} { return f.changes.Changed() }

// changed tells the waits that the fake changed.
func (f *Fake) changed() { f.changes.Notify() }

// P returns the path of rel in sysfs.
func (f *Fake) P(rel string) string { return filepath.Join(f.SysRoot(), rel) }

func (f *Fake) mkdir(rel string) {
	f.tb.Helper()
	if err := os.MkdirAll(f.P(rel), 0o755); err != nil {
		f.tb.Fatal(err)
	}
}

// Write writes val to the sysfs file rel. A sysfs attribute is read whole:
// the file is written aside and renamed in place, for a reader polling from
// another goroutine never to see it truncated or half written.
func (f *Fake) Write(rel, val string) {
	f.tb.Helper()
	if err := writeFile(f.P(rel), val, f.tmp); err != nil {
		f.tb.Fatal(err)
	}
	f.changed()
}

// writeFile writes val and a newline to path through a file in tmp.
func writeFile(path, val, tmp string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	t, err := os.CreateTemp(tmp, "write")
	if err == nil {
		_, err = t.WriteString(val + "\n")
		if cerr := t.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = os.Chmod(t.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(t.Name(), path)
	}
	return err
}

// Read returns the content of the sysfs file rel without white space
// around it, "" if it cannot be read.
func (f *Fake) Read(rel string) string {
	b, _ := os.ReadFile(f.P(rel))
	return strings.TrimSpace(string(b))
}

// Exists tells whether the sysfs path rel exists.
func (f *Fake) Exists(rel string) bool {
	_, err := os.Lstat(f.P(rel))
	return err == nil
}

// Link makes the sysfs path rel a symlink to target.
func (f *Fake) Link(rel, target string) {
	f.tb.Helper()
	f.mkdir(filepath.Dir(rel))
	_ = os.Remove(f.P(rel))
	if err := os.Symlink(target, f.P(rel)); err != nil {
		f.tb.Fatal(err)
	}
	f.changed()
}

// Remove removes the sysfs path rel and everything below it.
func (f *Fake) Remove(rel string) {
	_ = os.RemoveAll(f.P(rel))
	f.changed()
}

// LinkTo makes the sysfs path rel a symlink to the sysfs path target. The
// link is absolute: rel may be reached through a symlink to its directory.
func (f *Fake) LinkTo(rel, target string) {
	f.tb.Helper()
	f.Link(rel, f.P(target))
}

// Commands returns the commands that ran, and the writes of delete_region,
// one per line.
func (f *Fake) Commands() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.cmds, "\n")
}

// CommandsSince returns the commands run after those of before, an earlier
// Commands.
func (f *Fake) CommandsSince(before string) string {
	return strings.TrimPrefix(strings.TrimPrefix(f.Commands(), before), "\n")
}

// Writes returns the sysfs attributes of the dax drivers that were written,
// relative to sysfs, and clears them.
func (f *Fake) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

// Zeroed returns "major:minor size align" of the dax devices zeroed.
func (f *Fake) Zeroed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.zeroed...)
}

// Probed returns "major:minor size" of the address line probes.
func (f *Fake) Probed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.probed...)
}

// Before runs fn once before the first command or sysfs write whose
// description ("cxl create-region ...", "write bus/dax/devices/dax0.0/size
// 0") begins with prefix.
func (f *Fake) Before(prefix string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before[prefix] = fn
}

// After runs fn once after the first command or sysfs write whose
// description begins with prefix, before it returns.
func (f *Fake) After(prefix string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.after[prefix] = fn
}

// CutBefore stops the process (a panic of ErrCut) before the first command
// or sysfs write whose description begins with prefix.
func (f *Fake) CutBefore(prefix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutAt[prefix] = "before"
}

// CutAfter stops the process after the first command or sysfs write whose
// description begins with prefix.
func (f *Fake) CutAfter(prefix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutAt[prefix] = "after"
}

// CutProbe stops the process in the next address line probe.
func (f *Fake) CutProbe() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutProbe = true
}

// FailCommand makes the next commands that begin with prefix fail with
// errs, one each, nil for one that runs.
func (f *Fake) FailCommand(prefix string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCmd[prefix] = append(f.failCmd[prefix], errs...)
}

// FailWrite makes the next writes of the sysfs attribute rel fail with
// errs, one each, nil for one that is done.
func (f *Fake) FailWrite(rel string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWr[rel] = append(f.failWr[rel], errs...)
}

// ErrAddDev is the error of a cxl create-region on a memdev whose endpoint
// port is not there yet.
var ErrAddDev = errors.New("cxl region: cmd_create_region: region0: add_dev() failed")

// hooks runs the hooks of an action described by desc before it: the
// function of Before, the stop of CutBefore; it returns what to do after
// it (the function of After, and whether to stop), and the error that the
// action fails with.
func (f *Fake) hooks(desc string, fails map[string][]error) (func(), bool, error) {
	f.mu.Lock()
	var hook, afterHook func()
	for prefix, fn := range f.before {
		if strings.HasPrefix(desc, prefix) {
			hook = fn
			delete(f.before, prefix)
			break
		}
	}
	for prefix, fn := range f.after {
		if strings.HasPrefix(desc, prefix) {
			afterHook = fn
			delete(f.after, prefix)
			break
		}
	}
	cut, after := false, false
	for prefix, when := range f.cutAt {
		if strings.HasPrefix(desc, prefix) {
			delete(f.cutAt, prefix)
			cut, after = when == "before", when == "after"
			break
		}
	}
	var fail error
	for prefix, errs := range fails {
		if strings.HasPrefix(desc, prefix) && len(errs) > 0 {
			fail = errs[0]
			fails[prefix] = errs[1:]
			break
		}
	}
	f.mu.Unlock()
	if cut {
		panic(ErrCut)
	}
	if hook != nil {
		hook()
	}
	return afterHook, after, fail
}

// record appends a command to the log.
func (f *Fake) record(cmd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
}

// Exec runs a faked command (kernel.Kernel.Exec).
func (f *Fake) Exec(ctx context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	afterHook, cutAfter, fail := f.hooks(cmd, f.failCmd)
	f.record(cmd)
	var out string
	err := fail
	if err == nil {
		out, err = f.run(name, args)
	}
	f.changed()
	if afterHook != nil {
		afterHook()
	}
	if cutAfter {
		panic(ErrCut)
	}
	return out, err
}

// run runs a command.
func (f *Fake) run(name string, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("%s: no arguments", name)
	}
	switch name + " " + args[0] {
	case "cxl create-region":
		return f.createRegion(args[1:])
	case "cxl enable-region":
		return "", f.enableRegion(args[1])
	case "cxl disable-region":
		return "", f.disableRegion(args[1])
	case "cxl destroy-region":
		return "", f.destroyRegion(args[1])
	case "cxl enable-memdev":
		return "", f.enableMemdev(args[1])
	case "cxl disable-memdev":
		return "", f.disableMemdev(args[1])
	case "ndctl create-namespace":
		return f.createNamespace(args[1:])
	}
	return "", errors.New("unexpected command " + name + " " + strings.Join(args, " "))
}

// WriteAttr writes a sysfs attribute, doing what the kernel does for the
// attributes that it acts on (kernel.Kernel.WriteFile).
func (f *Fake) WriteAttr(path, val string) error {
	rel, err := filepath.Rel(f.SysRoot(), path)
	if err != nil {
		return err
	}
	afterHook, cutAfter, _ := f.hooks("write "+rel+" "+val, nil)
	f.mu.Lock()
	err = takeErr(f.failWr, rel)
	f.mu.Unlock()
	if err == nil {
		err = f.writeAttr(rel, val)
	}
	f.changed()
	if afterHook != nil {
		afterHook()
	}
	if cutAfter {
		panic(ErrCut)
	}
	return err
}

// takeErr returns the next error of the attribute rel in fails.
func takeErr(fails map[string][]error, rel string) error {
	if q := fails[rel]; len(q) > 0 {
		fails[rel] = q[1:]
		return q[0]
	}
	return nil
}

// writeAttr writes a sysfs attribute rel.
func (f *Fake) writeAttr(rel, val string) error {
	dir, attr := filepath.Dir(rel), filepath.Base(rel)
	switch {
	case filepath.Base(dir) == "dax_region" && attr == "create":
		return f.daxCreate(dir, val)
	case filepath.Base(dir) == "dax_region" && attr == "delete":
		return f.daxDelete(dir, val)
	case filepath.Dir(dir) == "bus/dax/devices" && attr == "mapping":
		return f.daxMap(filepath.Base(dir), val)
	case filepath.Dir(dir) == "bus/dax/devices" && attr == "size":
		return f.daxResize(filepath.Base(dir), val)
	case filepath.Dir(dir) == "bus/dax/drivers":
		f.mu.Lock()
		f.writes = append(f.writes, rel)
		f.mu.Unlock()
		return f.daxDriverAttr(filepath.Base(dir), attr, val)
	case strings.HasPrefix(dir, "devices/system/memory/memory") && attr == "state":
		return f.blockWrite(dir, val)
	case attr == "delete_region":
		return f.deleteRegion(filepath.Base(dir), val)
	}
	return os.WriteFile(f.P(rel), []byte(val), 0o644)
}

// boot starts a new boot: a new boot id.
func (f *Fake) boot() {
	f.bootID++
	f.writeProc("sys/kernel/random/boot_id", fmt.Sprintf("00000000-0000-4000-8000-%012d", f.bootID))
}

// BootID returns the boot id of the fake.
func (f *Fake) BootID() string {
	b, _ := os.ReadFile(filepath.Join(f.ProcRoot(), "sys", "kernel", "random", "boot_id"))
	return strings.TrimSpace(string(b))
}

// writeProc writes val to the procfs file rel.
func (f *Fake) writeProc(rel, val string) {
	f.tb.Helper()
	if err := writeFile(filepath.Join(f.ProcRoot(), rel), val, f.tmp); err != nil {
		f.tb.Fatal(err)
	}
}

// ReadWithin runs read in a goroutine. It returns true if read returned
// within 200 ms, and a channel that is closed when read has returned.
func ReadWithin(read func()) (bool, <-chan struct{}) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		read()
	}()
	select {
	case <-done:
		return true, done
	case <-time.After(200 * time.Millisecond):
		return false, done
	}
}
