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

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// coreExec runs the actions of the core model on a node and observes it:
// kubelet's requests through the front-end of the tests (env.prepare,
// env.unprepare), the faults and cuts at the engine's kernel calls and
// commits, the starts of the process, and the changes of the topology.
type coreExec struct {
	t   *testing.T
	v   *env
	log io.Writer
	// shared tells whether the memdev S is configured shared.
	shared bool
	// ports are the ports of the host bridges.
	ports [2]string
	// mu guards fault and hit: the armed fault or cut at a kernel call, and
	// whether a call reached it.
	mu    sync.Mutex
	fault *coreArm
	hit   bool
	// reads are the reads of the memory of dax devices that started
	// inside writes of the size of a dax device, closed when they end;
	// readProblems what the readers inside those writes saw. mu guards
	// them.
	reads        []<-chan struct{}
	readProblems []string
	// violations is the number of violations of the fake seen.
	violations int
	// front runs kubelet's requests, nil for the tests' own front-end
	// (env.prepare, env.unprepare).
	front CoreFront
}

// coreTB is the testing.TB of the fake of the core model: its log goes to
// the log of the run.
type coreTB struct {
	*testing.T
	log io.Writer
}

// Logf writes a line to the log of the run.
func (tb coreTB) Logf(format string, args ...any) {
	_, _ = fmt.Fprintf(tb.log, format+"\n", args...)
}

// newCoreExec returns the executor of a node with the memdevs of the core
// model plugged, the persistent ones with an ext4 filesystem, its log
// written to log; with newFront the requests go through the front-end that
// it returns, the cuts count every commit of the store.
func newCoreExec(t *testing.T, log io.Writer, newFront func(f *fake.Fake, dir string) CoreFront) *coreExec {
	x := &coreExec{t: t, log: log, shared: true}
	v := &env{t: t, f: fake.New(coreTB{T: t, log: log}), dir: t.TempDir()}
	x.v = v
	if newFront != nil {
		x.front = newFront(v.f, v.dir)
		v.newEngine = x.front.NewEngine
		v.storeCuts = true
	}
	for _, d := range coreMemdevs {
		v.f.Plug(d.spec)
	}
	for _, serial := range []uint64{serialQ, serialP} {
		v.formatPmem(v.f.Memdev(serial), "ext4")
	}
	k := v.f.Kernel()
	x.ports = [2]string{k.Memdev(serialA).Ports[0], k.Memdev(serialC).Ports[0]}
	v.tune = x.tune
	v.open()
	t.Cleanup(func() { _ = v.st.Close() })
	if x.front != nil {
		if err := x.front.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

// tune sets the options of the engines of the node: short waits, the
// address line probe, the memdev S shared, and the faults at the kernel
// calls.
func (x *coreExec) tune(o *Options) {
	o.Timeouts.Memdev = 50 * time.Millisecond
	o.Timeouts.Holder = 30 * time.Millisecond
	o.Timeouts.Settle, o.Timeouts.LocalSettle, o.Timeouts.Restore = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	o.ProbeAddressing = true
	o.ProbesFile = filepath.Join(x.v.dir, "localProbes.json")
	o.Shared = nil
	if x.shared {
		o.Shared = func(serial state.Serial) bool { return serial == serialS }
	}
	o.Kernel.Exec = x.exec(o.Kernel.Exec)
	o.Kernel.WriteFile = x.writeFile(o.Kernel.WriteFile)
}

// take returns the armed fault if the kernel call matches it and no call
// reached it yet, and marks it reached; nil otherwise.
func (x *coreExec) take(match func(a *coreArm) bool) *coreArm {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.fault == nil || x.hit || !match(x.fault) {
		return nil
	}
	x.hit = true
	return x.fault
}

// exec returns the command runner of the kernel with the armed fault: a
// cxl command that fails, or a process stop before or after it.
func (x *coreExec) exec(inner func(ctx context.Context, name string, args ...string) (string, error)) func(
	ctx context.Context, name string, args ...string) (string, error) {
	return func(ctx context.Context, name string, args ...string) (string, error) {
		desc := name + " " + strings.Join(args, " ")
		f := x.take(func(a *coreArm) bool {
			cmd := a.fail
			if cmd == "" {
				cmd = a.cmd
			}
			return cmd != "" && strings.HasPrefix(desc, "cxl "+cmd+" ")
		})
		switch {
		case f == nil:
			return inner(ctx, name, args...)
		case f.fail != "":
			return "", fmt.Errorf("cxl region: cmd_%s: %s: failed", strings.ReplaceAll(f.fail, "-", "_"), args[len(args)-1])
		case f.cut == "before":
			panic(fake.ErrCut)
		}
		_, _ = inner(ctx, name, args...)
		panic(fake.ErrCut)
	}
}

// writeFile returns the sysfs writer of the kernel with the armed fault: a
// memory block that refuses to go online or offline (EBUSY); the writes of
// the size of a dax device have the readers of daxWrite inside them.
func (x *coreExec) writeFile(inner func(path, val string) error) func(path, val string) error {
	root := x.v.f.SysRoot()
	return func(path, val string) error {
		rel, _ := filepath.Rel(root, path)
		if dir := filepath.Dir(rel); filepath.Base(rel) == "size" && filepath.Dir(dir) == "bus/dax/devices" {
			x.daxWrite(filepath.Base(dir))
		}
		block := strings.HasPrefix(rel, "devices/system/memory/memory") && strings.HasSuffix(rel, "/state")
		f := x.take(func(a *coreArm) bool {
			switch {
			case !block:
				return false
			case a.fail == "online":
				return strings.HasPrefix(val, "online")
			case a.fail == "offline":
				return val == "offline"
			}
			return false
		})
		if f != nil {
			return syscall.EBUSY
		}
		return inner(path, val)
	}
}

// daxWrite starts the readers of the process inside a write of the size of
// the dax device dax, which may remove a mapping of it: the topology of the
// node (the scan of the publisher), which reads no mapping and ends inside
// the write, and the memory of the device (the observation of the verify
// step), which waits for the write. A reader of a mapping inside the write
// would stop the kernel's removal of the mapping, and the write, forever
// (drivers/dax/bus.c). The problems and the end of the reads are checked
// at the end of the step (check).
func (x *coreExec) daxWrite(dax string) {
	k := x.v.f.Kernel()
	k.Logf = nil
	read := make(chan struct{})
	top := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = k.DaxDeviceRanges(dax)
	}()
	go func() {
		defer close(top)
		_, _ = k.ObserveTopology(context.Background())
	}()
	problem := ""
	select {
	case <-top:
		select {
		case <-read:
			problem = "a read of the memory of " + dax + " ended inside a write of its size"
		default:
		}
	case <-time.After(coreReadBound):
		problem = "the topology of the node did not end inside a write of the size of " + dax
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.reads = append(x.reads, read)
	if problem != "" {
		x.readProblems = append(x.readProblems, problem)
	}
}

// coreReadBound is the time that a read of the kernel's objects may wait.
const coreReadBound = 10 * time.Second

// checkReads returns the problems of the readers inside the writes of the
// size of dax devices since the last call, and the reads that did not end
// within coreReadBound after the step; and a problem if the engine lock is
// held after the step.
func (x *coreExec) checkReads() []string {
	x.mu.Lock()
	reads, out := x.reads, x.readProblems
	x.reads, x.readProblems = nil, nil
	x.mu.Unlock()
	for _, read := range reads {
		select {
		case <-read:
		case <-time.After(coreReadBound):
			out = append(out, "a read of the memory of a dax device did not end after the step")
		}
	}
	if !x.v.e.mu.TryLock() {
		return append(out, "the engine lock is held after the step")
	}
	x.v.e.mu.Unlock()
	return out
}

// grantRequest returns the request of the grant of the claim uid of the
// slot.
func grantRequest(i int, uid string) GrantRequest {
	sl := &coreSlots[i]
	d := &coreMemdevs[sl.md]
	var req GrantRequest
	switch {
	case d.source == state.SourcePool:
		req = poolReq(uid, 0, d.serial, sl.mode, d.kind)
	case d.kind.Persistent():
		req = localReq(uid, 0, d.serial, sl.mode, d.kind, 0)
	case d.kind.Shared():
		req = localReq(uid, 0, d.serial, sl.mode, d.kind, gib)
	default:
		req = localReq(uid, 0, d.serial, sl.mode, d.kind, 256*mib)
	}
	switch sl.mode {
	case state.ModeFS:
		req = fsReq(req, sl.readOnly, "")
	case state.ModeBlock:
		req.Asked.ReadOnly = sl.readOnly
	}
	return req
}

// errClass returns the class of the error of a request: "" for none,
// "hole" for a decoder hole, "refused" for a foreign region, else
// "failed".
func errClass(err error) string {
	var hole *kernel.ErrDecoderHole
	switch {
	case err == nil:
		return ""
	case errors.As(err, &hole):
		return "hole"
	case errors.Is(err, errForeignRegion):
		return "refused"
	}
	return "failed"
}

// coreOutcome is what the executor saw of an action.
type coreOutcome struct {
	// err is the error class of a request, "cut" when the process
	// stopped in it.
	err string
	// msg is the error of the request or of the action.
	msg string
	// unreached is set when the armed fault was not reached.
	unreached bool
	// start is the error of the start after a cut, "" for none.
	start string
}

// run executes the action from the start state.
func (x *coreExec) run(ca *coreAction, start *coreState) coreOutcome {
	var arm *coreArm
	if start.arm > 0 {
		arm = &coreArms[start.arm-1]
	}
	switch ca.verb {
	case "prepare", "unprepare":
		return x.request(ca, start, arm)
	case "restart", "reboot":
		j := 0
		if arm != nil {
			j = arm.j
		}
		if ca.verb == "reboot" {
			x.v.f.Reboot()
		}
		if err := x.start(j); err != nil {
			return coreOutcome{msg: err.Error()}
		}
	case "foreign":
		if err := x.foreign(ca.md); err != nil {
			return coreOutcome{msg: err.Error()}
		}
	case "unforeign":
		k := x.v.f.Kernel()
		regions := k.Regions(x.v.f.Memdev(coreMemdevs[ca.md].serial))
		if len(regions) != 1 {
			return coreOutcome{msg: fmt.Sprintf("memdev %s has regions %v", coreMemdevs[ca.md].id, regions)}
		}
		if err := k.DestroyRegion(context.Background(), regions[0]); err != nil {
			return coreOutcome{msg: err.Error()}
		}
	case "unplug":
		x.v.f.Unplug(coreMemdevs[ca.md].serial)
	case "plug":
		x.v.f.Plug(coreMemdevs[ca.md].spec)
	case "lose":
		if err := x.lose(ca.md); err != nil {
			return coreOutcome{msg: err.Error()}
		}
	}
	return coreOutcome{err: start.err}
}

// lose unmounts the node mounts of the persistent memdev and starts the
// process again with the mounts of their block devices failing.
func (x *coreExec) lose(md int) error {
	k := x.v.f.Kernel()
	snap, err := k.Observe(context.Background())
	if err != nil {
		return err
	}
	dir := filepath.Join(x.v.dir, driverOf(coreMemdevs[md].source), "mounts") + "/"
	var sources []string
	for _, mi := range snap.Mounts {
		if strings.HasPrefix(mi.Target, dir) {
			if err := k.Unmount(mi.Target); err != nil {
				return err
			}
			sources = append(sources, mi.Source)
		}
	}
	for _, src := range sources {
		x.v.f.SetMountError(src, syscall.EIO, "")
	}
	err = x.start(0)
	for _, src := range sources {
		x.v.f.SetMountError(src, nil, "")
	}
	return err
}

// foreign makes a region on the memdev as another tool does, the memdev
// enabled first (a pool device is disabled after its release).
func (x *coreExec) foreign(md int) error {
	k := x.v.f.Kernel()
	memdev := x.v.f.Memdev(coreMemdevs[md].serial)
	if err := k.EnableMemdev(context.Background(), memdev); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := k.WaitMemdev(ctx, coreMemdevs[md].serial, kernel.MemdevReady); err != nil {
		return err
	}
	d, err := k.RootDecoder(memdev, kernel.RegionRAM)
	if err != nil {
		return err
	}
	_, err = k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: memdev, Decoder: d, Type: kernel.RegionRAM})
	return err
}

// request runs kubelet's request of the action with the armed fault or
// cut, and starts the process again when it stopped in the request.
func (x *coreExec) request(ca *coreAction, start *coreState, arm *coreArm) coreOutcome {
	v := x.v
	sl := &coreSlots[ca.slot]
	gen := start.gens[ca.slot]
	if ca.verb == "prepare" && start.slots[ca.slot].prep == "" {
		gen++
	}
	uid := sl.uid(gen)
	x.mu.Lock()
	x.fault, x.hit = nil, false
	if arm != nil && arm.cut != "commit" {
		x.fault = arm
	}
	x.mu.Unlock()
	if arm != nil && arm.cut == "commit" {
		v.commitMu.Lock()
		v.cutAt = v.commits + arm.k
		v.commitMu.Unlock()
	}
	var err error
	cut := v.cut(func() {
		switch {
		case x.front != nil && ca.verb == "prepare":
			err = x.front.Prepare(coreSlotOf(ca.slot), uid)
		case x.front != nil:
			err = x.front.Unprepare(coreSlotOf(ca.slot), uid)
		case ca.verb == "prepare":
			err = v.prepare(sl.driver(), uid, grantRequest(ca.slot, uid))
		default:
			err = v.unprepare(sl.driver(), uid)
		}
	})
	v.commitMu.Lock()
	v.cutAt = 0
	v.commitMu.Unlock()
	x.mu.Lock()
	hit := x.hit
	x.fault, x.hit = nil, false
	x.mu.Unlock()
	out := coreOutcome{err: errClass(err), unreached: arm != nil && arm.cut != "commit" && !hit}
	if err != nil {
		out.msg = err.Error()
	}
	if cut {
		out.err, out.msg = "cut", ""
		j := 0
		if arm != nil {
			j = arm.j
		}
		if err := x.start(j); err != nil {
			out.start = err.Error()
		}
	}
	return out
}

// start stops the process and starts it again: the store opened, a new
// engine, the recovery. With j > 0 the recovery stops
// after its j-th commit and the process starts once more.
func (x *coreExec) start(j int) error {
	v := x.v
	for {
		_ = v.st.Close()
		v.open()
		v.commitMu.Lock()
		if j > 0 {
			v.cutAt = v.commits + j
		}
		v.commitMu.Unlock()
		var err error
		cut := v.cut(func() { err = x.recoverStart() })
		v.commitMu.Lock()
		v.cutAt = 0
		v.commitMu.Unlock()
		if !cut {
			if err != nil {
				return fmt.Errorf("recover: %w", err)
			}
			return nil
		}
		_, _ = fmt.Fprintf(x.log, "core: the start stopped after commit %d of its recovery\n", j)
		j = 0
	}
}

// recoverStart recovers the records of a start, through the front-end when
// there is one.
func (x *coreExec) recoverStart() error {
	if x.front != nil {
		return x.front.Recover(context.Background())
	}
	_, err := x.v.e.Recover(context.Background())
	return err
}

// observe returns the observable state of the node, with the generations
// and the foreign regions of the model's state from, and the problems
// that no state can express: records of claims of no slot, two regions on
// a memdev, a stack entry of no memdev.
func (x *coreExec) observe(from *coreState, class string) (*coreState, []string) {
	v := x.v
	ctx := context.Background()
	var problems []string
	// a durable commit that changes nothing: the lazy commits written
	if err := v.st.Commit(ctx, store.Durable, func(*store.Document) error { return nil }); err != nil {
		problems = append(problems, "barrier commit: "+err.Error())
	}
	r := v.records()
	snap, err := v.f.Kernel().Observe(ctx)
	if err != nil {
		return nil, []string{"observe: " + err.Error()}
	}
	o := &coreState{
		slots: make([]coreSlotState, len(coreSlots)),
		gens:  slices.Clone(from.gens),
		mds:   make([]coreMemdevState, len(coreMemdevs)),
		err:   class,
	}
	current := map[string]bool{}
	for i := range coreSlots {
		sl := &coreSlots[i]
		uid := sl.uid(o.gens[i])
		current[sl.driver()+"/"+uid] = true
		if p := r.Preparation(sl.driver(), uid); p != nil {
			o.slots[i].prep = p.State
		}
		if g := r.Grant(state.GrantRef{Claim: uid}); g != nil {
			o.slots[i].grant = g.State
			o.slots[i].named = g.Got.Region != "" || g.Intent.Region != ""
		}
	}
	users := map[state.GrantRef]bool{}
	for _, reg := range r.Regions {
		for _, d := range reg.DaxDevices {
			for _, u := range d.Users {
				users[u] = true
			}
		}
		for _, mt := range reg.Mounts {
			for _, u := range mt.Users {
				users[u] = true
			}
		}
	}
	for i := range coreSlots {
		ref := state.GrantRef{Claim: coreSlots[i].uid(o.gens[i])}
		o.slots[i].user = o.slots[i].grant != "" && users[ref]
		if g := r.Grant(ref); g != nil && coreSlots[i].mode == state.ModeBlock {
			// a grant of mode block uses its region as Got names it
			o.slots[i].user = g.Got.Region != ""
		}
	}
	if coreFront {
		x.grantsOnline(o, r, snap)
	}
	for _, p := range r.Preparations {
		if !current[p.Driver+"/"+p.UID] {
			problems = append(problems, fmt.Sprintf("a preparation of no slot: %s %s %s", p.Driver, p.UID, p.State))
		}
	}
	for _, g := range r.Grants {
		if !current[poolDriver+"/"+g.Ref.Claim] && !current[localDriver+"/"+g.Ref.Claim] || g.Ref.Result != 0 {
			problems = append(problems, fmt.Sprintf("a grant of no slot: %s %s", g.Ref, g.State))
		}
	}
	bySerial := map[uint64][]kernel.RegionInfo{}
	idOf := map[string]string{}
	for _, ri := range snap.Regions {
		for _, serial := range ri.Serials {
			bySerial[serial] = append(bySerial[serial], ri)
			for _, d := range coreMemdevs {
				if d.serial == serial {
					idOf[ri.Name] = d.id
				}
			}
		}
	}
	for j, d := range coreMemdevs {
		m := &o.mds[j]
		m.foreign = from.mds[j].foreign
		if rec := r.Memdev(state.Serial(d.serial)); rec != nil {
			m.rec = rec.State
			var regs []string
			for _, ref := range rec.Regions {
				if reg := r.Region(ref); reg != nil && reg.Ours() {
					regs = append(regs, string(reg.State))
				}
			}
			m.reg = state.RegionState(strings.Join(regs, "+"))
		}
		regions := bySerial[d.serial]
		if len(regions) > 1 {
			problems = append(problems, fmt.Sprintf("memdev %s has %d regions", d.id, len(regions)))
		}
		if len(regions) > 0 {
			ri := regions[0]
			switch ri.State {
			case kernel.RegionEnabled:
				m.kreg = kregOn
			case kernel.RegionDisabled:
				m.kreg = kregOff
			default:
				m.kreg = kregHollow
			}
			for _, b := range snap.Blocks {
				addr := uint64(b.Index) * snap.BlockSize
				if b.State == kernel.BlockOnline && addr >= ri.Range.Start && addr <= ri.Range.End {
					m.online = true
				}
			}
		}
		for _, mi := range snap.Memdevs {
			if mi.Serial == d.serial && mi.Name != "" {
				m.plugged = true
			}
		}
		if d.kind.Persistent() {
			m.mount = x.mountOf(snap, d.source)
		}
	}
	for h, port := range x.ports {
		for _, region := range v.f.Stack(port) {
			switch id := idOf[region]; {
			case region == "":
				o.stacks[h] = append(o.stacks[h], "_")
			case id == "":
				problems = append(problems, fmt.Sprintf("region %s of port %s is of no memdev of the model", region, port))
				o.stacks[h] = append(o.stacks[h], "?"+region)
			default:
				o.stacks[h] = append(o.stacks[h], id)
			}
		}
	}
	return o, problems
}

// grantsOnline sets whether the memory of each grant of mode numa of the
// observation o is online: a memory block of the ranges of the dax device
// that it records is online.
func (x *coreExec) grantsOnline(o *coreState, r *state.Records, snap *kernel.Snapshot) {
	for i := range coreSlots {
		g := r.Grant(state.GrantRef{Claim: coreSlots[i].uid(o.gens[i])})
		if g == nil || g.Asked.Mode != state.ModeNUMA || g.Got.Dax == "" {
			continue
		}
		var ranges []kernel.Range
		for _, d := range snap.DaxDevices {
			if d.Name == g.Got.Dax {
				ranges = d.Ranges
			}
		}
		for _, b := range snap.Blocks {
			addr := uint64(b.Index) * snap.BlockSize
			for _, rg := range ranges {
				if b.State == kernel.BlockOnline && addr >= rg.Start && addr <= rg.End {
					o.slots[i].online = true
				}
			}
		}
	}
}

// mountOf returns the node mounts of the driver of the source: "rw" or "ro"
// by the superblock, with "!bdev-ro" or "!bdev-rw" when the read-only flag
// of the block device disagrees, joined by "+".
func (x *coreExec) mountOf(snap *kernel.Snapshot, source state.Source) string {
	dir := filepath.Join(x.v.dir, driverOf(source), "mounts") + "/"
	var out []string
	for _, mi := range snap.Mounts {
		if !strings.HasPrefix(mi.Target, dir) {
			continue
		}
		mode := "rw"
		if mi.SuperReadOnly() {
			mode = "ro"
		}
		for _, bd := range snap.Devices {
			if fmt.Sprintf("%d:%d", bd.Major, bd.Minor) != mi.DevNum {
				continue
			}
			switch {
			case mode == "rw" && bd.ReadOnly:
				mode += "!bdev-ro"
			case mode == "ro" && !bd.ReadOnly:
				mode += "!bdev-rw"
			}
		}
		out = append(out, mode)
	}
	slices.Sort(out)
	return strings.Join(out, "+")
}

// check returns the violations of the invariants of the records and the
// kernel now (Engine.Check), the changes of kernel objects that no row of
// their tables has, the new violations of the rules of persistent devices
// of the fake, and the problems of checkReads.
func (x *coreExec) check() []string {
	v := x.v
	snap, err := v.f.Kernel().Observe(context.Background())
	if err != nil {
		return []string{"observe: " + err.Error()}
	}
	v.fenceMu.Lock()
	fenced := slices.Clone(v.fenced)
	v.fenceMu.Unlock()
	if fenced == nil {
		fenced = []int{}
	}
	var out []string
	for _, viol := range v.e.Check(snap, v.sources, fenced) {
		out = append(out, "invariant: "+viol.String())
	}
	for _, line := range v.e.UnknownSeen() {
		out = append(out, UnknownTransition+line)
	}
	viols := v.f.Violations()
	for _, viol := range viols[min(x.violations, len(viols)):] {
		out = append(out, "fake: "+viol)
	}
	x.violations = len(viols)
	if x.front != nil {
		for _, viol := range x.front.Check() {
			out = append(out, "front-end: "+viol)
		}
	}
	return append(out, x.checkReads()...)
}

// clean returns what is left on the node: memdev, region, grant and
// preparation records, and regions of the kernel.
func (x *coreExec) clean() []string {
	r := x.v.records()
	var out []string
	if n := len(r.Memdevs) + len(r.Regions) + len(r.Grants) + len(r.Preparations); n > 0 {
		out = append(out, fmt.Sprintf("records left: memdevs %+v regions %+v grants %+v preparations %+v", r.Memdevs,
			r.Regions, r.Grants, r.Preparations))
	}
	snap, err := x.v.f.Kernel().Observe(context.Background())
	if err != nil {
		return append(out, "observe: "+err.Error())
	}
	for _, ri := range snap.Regions {
		out = append(out, fmt.Sprintf("region %s (%s) left", ri.Name, ri.State))
	}
	return out
}
