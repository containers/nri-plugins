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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

const (
	poolDriver  = "cxl-pool.nri.io"
	localDriver = "cxl.nri.io"

	// serials of the memdevs of the tests: pool devices under host bridge
	// 12 (a, b), and 24 (c); a local memdev under host bridge 12 (l) and
	// one of persistent memory under 24 (p).
	serialA = 0xa1
	serialB = 0xb1
	serialC = 0xc1
	serialL = 0x11
	serialP = 0x91

	gib = uint64(1) << 30
	mib = uint64(1) << 20
)

// env is a node for the tests of the engine: a fake kernel, a store in a
// temporary directory and the engine over them, with the front-end's
// preparation records done by the test.
type env struct {
	t   *testing.T
	f   *fake.Fake
	dir string
	st  *store.Store
	e   *Engine
	// cutAt stops the process (panics with fake.ErrCut) after the n-th
	// commit from now, 0 for never.
	cutAt   int
	commits int
	// commitMu guards cutAt and commits against concurrent requests.
	commitMu sync.Mutex
	fenceMu  sync.Mutex
	fenced   []int
	// sources are the sources that the engine owns, nil for all.
	sources []state.Source
	// tune changes the options of every engine that the node starts, nil
	// for none.
	tune func(o *Options)
	// newEngine returns every engine that the node starts, nil for New.
	newEngine func(o Options) (*Engine, error)
	// cutZero stops the process before ("before") or after ("after") the
	// next zeroing of a dax device, "" for never; onZero runs before the
	// next zeroing, nil for nothing; cutMu guards them.
	cutMu   sync.Mutex
	cutZero string
	onZero  func()
	// beforePrepared runs once in the next Prepare (prepare) between its
	// grants realised and its preparation PREPARED, nil for nothing.
	beforePrepared func()
	// storeCuts counts the commits of cutAt at the store (every commit)
	// instead of at the engine.
	storeCuts bool
	// prepareTimeout bounds the Prepares of prepare, 0 for no bound.
	prepareTimeout time.Duration
}

// newEnv returns a node with the memdevs of specs plugged.
func newEnv(t *testing.T, specs ...fake.MemdevSpec) *env {
	t.Helper()
	v := &env{t: t, f: fake.New(t), dir: t.TempDir()}
	for _, s := range specs {
		v.f.Plug(s)
	}
	v.open()
	t.Cleanup(func() { _ = v.st.Close() })
	return v
}

// open opens the store and starts a new engine, as a start of the process
// does.
func (v *env) open() {
	v.t.Helper()
	opts := store.Options{Dir: filepath.Join(v.dir, "state"), BootID: v.f.BootID()}
	if v.storeCuts {
		opts.AfterCommit = v.committed
	}
	st, err := store.Open(context.Background(), opts)
	if err != nil {
		v.t.Fatal(err)
	}
	v.st = st
	v.startEngine()
}

// configure sets the options of the engines of the node (tune) and starts
// a new engine with them on the store, without a recovery.
func (v *env) configure(tune func(o *Options)) {
	v.t.Helper()
	v.tune = tune
	v.startEngine()
}

// startEngine starts a new engine on the store of the node.
func (v *env) startEngine() {
	v.t.Helper()
	k := v.f.Kernel()
	k.Sys = &cutSys{Sys: k.Sys, v: v}
	opts := Options{
		Kernel: k,
		Store:  v.st,
		MountsDirs: map[state.Source]string{
			state.SourcePool:  filepath.Join(v.dir, poolDriver, "mounts"),
			state.SourceLocal: filepath.Join(v.dir, localDriver, "mounts"),
		},
		Sources:     v.sources,
		LeaseFsRoot: filepath.Join(v.dir, localDriver, "lease-fs"),
		FSTypes:     []string{api.FSTypeErofs, api.FSTypeExt4, api.FSTypeXfs},
		Fence: func(_ context.Context, node int) {
			v.fenceMu.Lock()
			defer v.fenceMu.Unlock()
			if !slices.Contains(v.fenced, node) {
				v.fenced = append(v.fenced, node)
			}
		},
		Timeouts: Timeouts{Memdev: 2 * time.Second, ProbeGrace: 20 * time.Millisecond, RegionRetry: 10 * time.Millisecond,
			Settle: 5 * time.Second, LocalSettle: 5 * time.Second, Restore: time.Second, Holder: 300 * time.Millisecond,
			OfflineSettle: 20 * time.Millisecond, Offline: time.Second},
	}
	if v.tune != nil {
		v.tune(&opts)
	}
	newEngine := New
	if v.newEngine != nil {
		newEngine = v.newEngine
	}
	e, err := newEngine(opts)
	if err != nil {
		v.t.Fatal(err)
	}
	if !v.storeCuts {
		e.afterCommit = v.committed
	}
	v.e = e
}

// committed counts a commit and stops the process (panics with
// fake.ErrCut) at the commit of cutAt.
func (v *env) committed() {
	v.commitMu.Lock()
	v.commits++
	cut := v.cutAt > 0 && v.commits == v.cutAt
	if cut {
		v.cutAt = 0
	}
	v.commitMu.Unlock()
	if cut {
		panic(fake.ErrCut)
	}
}

// cutSys is the system calls of the fake that stop the process at a
// zeroing (env.cutZero).
type cutSys struct {
	kernel.Sys
	v *env
}

// ZeroDax zeroes a dax device, the process stopped before or after it when
// the node says so.
func (s *cutSys) ZeroDax(ctx context.Context, major, minor int, size, align uint64) error {
	s.v.cutMu.Lock()
	when, on := s.v.cutZero, s.v.onZero
	s.v.cutZero, s.v.onZero = "", nil
	s.v.cutMu.Unlock()
	if on != nil {
		on()
	}
	if when == "before" {
		panic(fake.ErrCut)
	}
	err := s.Sys.ZeroDax(ctx, major, minor, size, align)
	if when == "after" {
		panic(fake.ErrCut)
	}
	return err
}

// cutZeroing makes the process stop before (after false) or after the next
// zeroing of a dax device.
func (v *env) cutZeroing(after bool) {
	v.cutMu.Lock()
	defer v.cutMu.Unlock()
	v.cutZero = map[bool]string{false: "before", true: "after"}[after]
}

// cutAfter makes the process stop (panic with fake.ErrCut) after the n-th
// commit of the engine from now.
func (v *env) cutAfter(n int) {
	v.cutAt = v.commits + n
}

// restart stops the process (the store closed) and starts it again: a new
// store and engine, and the recovery.
func (v *env) restart() *recovered {
	v.t.Helper()
	_ = v.st.Close()
	v.open()
	before := v.records()
	rec, err := v.e.Recover(context.Background())
	if err != nil {
		v.t.Fatalf("recover: %v", err)
	}
	v.check("restarted")
	return recoveredOf(before, v.records(), rec)
}

// recovered is what a recovery did by the records before and after it,
// each list in the order of the refs: the grants that are gone (Released);
// the grants of cut Prepares of KEPT preparations (Kept), and those on GONE
// memdevs, numa companions' but (Absent); the grants that became
// REALISED_UNRESTORED (Unrestored) or REALISED again (Restored). Why is
// what Recover returned.
type recovered struct {
	Released, Kept, Absent, Unrestored, Restored []state.GrantRef
	Why                                          map[state.GrantRef]error
}

// recoveredOf returns what the recovery rec did to the records before,
// which made the records after.
func recoveredOf(before, after *state.Records, rec *Recovered) *recovered {
	out := &recovered{Why: rec.Why}
	for i := range before.Grants {
		if ref := before.Grants[i].Ref; after.Grant(ref) == nil {
			out.Released = append(out.Released, ref)
		}
	}
	for i := range after.Grants {
		g := &after.Grants[i]
		was := before.Grant(g.Ref)
		cut := was != nil && cutGrant(before, was)
		p, m := after.PreparationOf(g.Ref), after.Memdev(g.Serial)
		switch {
		case cut && p != nil && p.State == state.PreparationKept:
			out.Kept = append(out.Kept, g.Ref)
		case cut && m != nil && m.State == state.MemdevGone && !numaCompanion(g):
			out.Absent = append(out.Absent, g.Ref)
		case g.State == state.GrantUnrestored && (was == nil || was.State != state.GrantUnrestored):
			out.Unrestored = append(out.Unrestored, g.Ref)
		case g.State == state.GrantRealised && was != nil && was.State == state.GrantUnrestored:
			out.Restored = append(out.Restored, g.Ref)
		}
	}
	for _, refs := range [][]state.GrantRef{out.Released, out.Kept, out.Absent, out.Unrestored, out.Restored} {
		slices.SortFunc(refs, state.GrantRef.Compare)
	}
	return out
}

// onlineAtFence makes the fence record the memory blocks online at each
// fence, as "node: blocks".
func (v *env) onlineAtFence() *[]string {
	var at []string
	fence := v.e.opts.Fence
	v.e.opts.Fence = func(ctx context.Context, node int) {
		snap, err := v.f.Kernel().Observe(context.Background())
		if err != nil {
			v.t.Fatalf("observe at the fence: %v", err)
		}
		var online []int
		for _, b := range snap.Blocks {
			if b.State == kernel.BlockOnline {
				online = append(online, b.Index)
			}
		}
		at = append(at, fmt.Sprintf("%d: %v", node, online))
		fence(ctx, node)
	}
	return &at
}

// cut runs fn and reports whether the process was stopped in it.
func (v *env) cut(fn func()) (cut bool) {
	defer func() {
		if p := recover(); p != nil {
			if err, ok := p.(error); !ok || !errors.Is(err, fake.ErrCut) {
				panic(p)
			}
			cut = true
		}
	}()
	fn()
	return false
}

// records returns the records of the store.
func (v *env) records() *state.Records {
	return &v.st.Snapshot().Records
}

// check fails the test on a violation of the invariants of the records and
// the kernel now, and on a change of a kernel object that no row of its
// table has since the last check.
func (v *env) check(what string) {
	v.t.Helper()
	snap, err := v.f.Kernel().Observe(context.Background())
	if err != nil {
		v.t.Fatal(err)
	}
	v.fenceMu.Lock()
	fenced := slices.Clone(v.fenced)
	v.fenceMu.Unlock()
	if fenced == nil {
		fenced = []int{}
	}
	for _, viol := range v.e.Check(snap, v.sources, fenced) {
		v.t.Errorf("%s: %s", what, viol)
	}
	for _, line := range v.e.UnknownSeen() {
		v.t.Errorf("%s: %s%s", what, UnknownTransition, line)
	}
}

// grantResult returns a result of kind grant of the claim uid with index
// n.
func grantResult(uid string, n int) state.Result {
	return state.Result{Index: n, Kind: state.ResultGrant, Request: "mem", Grant: &state.GrantRef{Claim: uid, Result: n}}
}

// prepare does what a front-end does for a Prepare of a claim of the driver
// with the grants reqs, in a request of the engine: the preparation
// PREPARING with its results, the grants realised, then (after
// beforePrepared) PREPARED; on an error the preparation is removed, or KEPT
// when the rollback kept a grant. It returns the error of PrepareGrants.
func (v *env) prepare(driver, uid string, reqs ...GrantRequest) error {
	v.t.Helper()
	ctx, end := v.e.Request(context.Background())
	defer end()
	if err := v.st.Commit(ctx, store.Durable, func(doc *store.Document) error {
		p := doc.Preparation(driver, uid)
		if p == nil {
			doc.Preparations = append(doc.Preparations, state.Preparation{UID: uid, Driver: driver, ClaimName: "ns/" + uid,
				State: state.PreparationNone})
			p = &doc.Preparations[len(doc.Preparations)-1]
		}
		if p.State == state.PreparationStale {
			// the grants of another boot are dropped with the Prepare again
			for _, res := range p.Results {
				if res.Grant != nil {
					ref := *res.Grant
					removeRecord(&doc.Grants, func(g *state.Grant) bool { return g.Ref == ref })
				}
			}
		}
		for i := range reqs {
			if p.Result(reqs[i].Ref.Result) == nil {
				p.Results = append(p.Results, grantResult(uid, reqs[i].Ref.Result))
			}
		}
		return fsm.Apply(&state.PreparationTable, &p.State, state.PreparationPreparing, state.TriggerPrepare)
	}); err != nil {
		v.t.Fatal(err)
	}
	pctx := ctx
	if v.prepareTimeout > 0 {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(ctx, v.prepareTimeout)
		defer cancel()
	}
	_, err := v.e.PrepareGrants(pctx, reqs)
	if before := v.beforePrepared; before != nil && err == nil {
		v.beforePrepared = nil
		before()
	}
	if cerr := v.st.Commit(ctx, store.Durable, func(doc *store.Document) error {
		p := doc.Preparation(driver, uid)
		if err == nil {
			return fsm.Apply(&state.PreparationTable, &p.State, state.PreparationPrepared, state.TriggerPrepare)
		}
		return failedPreparation(&doc.Records, p)
	}); cerr != nil {
		v.t.Fatal(cerr)
	}
	return err
}

// failedPreparation makes the preparation of a failed Prepare KEPT when a
// grant of it is left, else removes it.
func failedPreparation(r *state.Records, p *state.Preparation) error {
	left := false
	var results []state.Result
	for _, res := range p.Results {
		if res.Grant != nil && r.Grant(*res.Grant) == nil {
			continue
		}
		left = left || res.Grant != nil
		results = append(results, res)
	}
	p.Results = results
	if left {
		return fsm.Apply(&state.PreparationTable, &p.State, state.PreparationKept, state.TriggerPrepare)
	}
	if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationNone, state.TriggerPrepare); err != nil {
		return err
	}
	uid, driver := p.UID, p.Driver
	removeRecord(&r.Preparations, func(x *state.Preparation) bool { return x.UID == uid && x.Driver == driver })
	return nil
}

// unprepare does what a front-end does for an Unprepare of a claim of the
// driver, in a request of the engine: UNPREPARING, the grants released, the
// preparation removed. It returns the error of ReleaseGrants (the
// preparation stays then).
func (v *env) unprepare(driver, uid string) error {
	v.t.Helper()
	ctx, end := v.e.Request(context.Background())
	defer end()
	p := v.records().Preparation(driver, uid)
	if p == nil {
		return nil
	}
	var refs []state.GrantRef
	for _, res := range p.Results {
		if res.Grant != nil {
			refs = append(refs, *res.Grant)
		}
	}
	if p.Prepared() && p.State != state.PreparationUnpreparing {
		if err := v.st.Commit(ctx, store.Durable, func(doc *store.Document) error {
			p := doc.Preparation(driver, uid)
			return fsm.Apply(&state.PreparationTable, &p.State, state.PreparationUnpreparing, state.TriggerUnprepare)
		}); err != nil {
			v.t.Fatal(err)
		}
	}
	if err := v.e.ReleaseGrants(ctx, refs, ReleaseUnprepare); err != nil {
		return err
	}
	if err := v.st.Commit(ctx, store.Durable, func(doc *store.Document) error {
		p := doc.Preparation(driver, uid)
		if p == nil {
			return nil
		}
		if err := fsm.Apply(&state.PreparationTable, &p.State, state.PreparationNone, state.TriggerUnprepare); err != nil {
			return err
		}
		removeRecord(&doc.Preparations, func(x *state.Preparation) bool { return x.UID == uid && x.Driver == driver })
		return nil
	}); err != nil {
		v.t.Fatal(err)
	}
	return nil
}

// mustPrepare prepares a claim and fails the test on an error.
func (v *env) mustPrepare(driver, uid string, reqs ...GrantRequest) {
	v.t.Helper()
	if err := v.prepare(driver, uid, reqs...); err != nil {
		v.t.Fatalf("prepare %s: %v", uid, err)
	}
	v.check("prepared " + uid)
}

// mustUnprepare unprepares a claim and fails the test on an error.
func (v *env) mustUnprepare(driver, uid string) {
	v.t.Helper()
	if err := v.unprepare(driver, uid); err != nil {
		v.t.Fatalf("unprepare %s: %v", uid, err)
	}
	v.check("unprepared " + uid)
}

// grant returns the record of the grant of result n of the claim uid,
// mount returns the record of the node mount that the grant n of the claim
// uid uses; the test fails if it has none.
func (v *env) mount(uid string, n int) *state.Mount {
	v.t.Helper()
	m := v.records().MountOf(v.grant(uid, n))
	if m == nil {
		v.t.Fatalf("grant %s/%d has no node mount", uid, n)
	}
	return m
}

// grant returns the grant of the result n of the claim uid, failing the
// test without one.
func (v *env) grant(uid string, n int) *state.Grant {
	v.t.Helper()
	g := v.records().Grant(state.GrantRef{Claim: uid, Result: n})
	if g == nil {
		v.t.Fatalf("no grant %s/%d", uid, n)
	}
	return g
}

// mustKeepCut fails the test unless the recovery kept the grant of the cut
// Prepare of the claim uid as it was, with its preparation PREPARING, for
// kubelet's Unprepare or Prepare again.
func (v *env) mustKeepCut(uid string) {
	v.t.Helper()
	r := v.records()
	ref := state.GrantRef{Claim: uid}
	g, p := r.Grant(ref), r.PreparationOf(ref)
	if g == nil || !cutGrant(r, g) || p == nil || p.State != state.PreparationPreparing {
		v.t.Fatalf("the cut grant of %s: %+v of %+v", uid, g, p)
	}
}

// noRecords fails the test when the store has a memdev, region or grant
// record, or the kernel a region.
func (v *env) clean(what string) {
	v.t.Helper()
	r := v.records()
	if len(r.Memdevs)+len(r.Regions)+len(r.Grants) > 0 {
		v.t.Errorf("%s: records left: memdevs %+v regions %+v grants %+v", what, r.Memdevs, r.Regions, r.Grants)
	}
	snap, err := v.f.Kernel().Observe(context.Background())
	if err != nil {
		v.t.Fatal(err)
	}
	for _, ri := range snap.Regions {
		v.t.Errorf("%s: region %s (%s) left", what, ri.Name, ri.State)
	}
}

// poolReq returns the grant request of result n of the claim uid on a pool
// device.
func poolReq(uid string, n int, serial uint64, mode state.Mode, kind state.GrantKind) GrantRequest {
	return GrantRequest{
		Ref:    state.GrantRef{Claim: uid, Result: n},
		Serial: state.Serial(serial),
		Asked:  state.Asked{Source: state.SourcePool, Kind: kind, Mode: mode, Size: 256 * mib, Shared: kind == state.KindPoolShared},
		Facts:  state.MemdevFacts{Shared: kind.Shared(), Persistent: kind.Persistent()},
	}
}

// localReq returns the grant request of result n of the claim uid on a
// local memdev.
func localReq(uid string, n int, serial uint64, mode state.Mode, kind state.GrantKind, size uint64) GrantRequest {
	return GrantRequest{
		Ref:    state.GrantRef{Claim: uid, Result: n},
		Serial: state.Serial(serial),
		Asked:  state.Asked{Source: state.SourceLocal, Kind: kind, Mode: mode, Size: size, Shared: kind == state.KindLocalShared},
		Facts:  state.MemdevFacts{Shared: kind.Shared(), Persistent: kind.Persistent()},
	}
}

// fsReq returns the request of a grant of mode fs on a persistent device.
func fsReq(req GrantRequest, readOnly bool, access state.Access) GrantRequest {
	req.Asked.Mode, req.Asked.ReadOnly, req.Asked.Access = state.ModeFS, readOnly, access
	req.Asked.Path = "/mnt/data"
	req.SingleHost = true
	return req
}

// formatPmem makes the fsdax namespace of the persistent memdev with the
// serial (memdev memdev) and a filesystem of the type on it, as a writer
// on another host would, leaving no region.
func (v *env) formatPmem(memdev string, typ string) {
	v.t.Helper()
	k := v.f.Kernel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := k.RootDecoder(memdev, kernel.RegionPmem)
	if err != nil {
		v.t.Fatal(err)
	}
	region, err := k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: memdev, Decoder: d, Type: kernel.RegionPmem})
	if err != nil {
		v.t.Fatal(err)
	}
	nd, err := k.WaitNdRegion(ctx, region)
	if err != nil {
		v.t.Fatal(err)
	}
	ns, err := k.CreateFsdaxNamespace(ctx, nd, "")
	if err != nil {
		v.t.Fatal(err)
	}
	v.f.SetImage(ns.BlockDevice, fake.FSImage(typ))
	if err := k.DestroyRegion(ctx, region); err != nil {
		v.t.Fatal(err)
	}
}

// blockState returns the sysfs attribute of the state of memory block n.
func blockState(n int) string {
	return fmt.Sprintf("devices/system/memory/memory%d/state", n)
}

// companionTarget returns the lease bind of the companion claims of the
// scenarios.
func companionTarget(v *env) string {
	return filepath.Join(v.dir, localDriver, "lease-fs", "lc1", "data")
}

// finish unprepares every claim that the store has and checks that nothing
// is left.
func finish(t *testing.T, v *env, what string) {
	t.Helper()
	for _, p := range v.records().Preparations {
		if err := v.unprepare(p.Driver, p.UID); err != nil {
			t.Fatalf("%s: unprepare %s: %v", what, p.UID, err)
		}
	}
	v.check(what + ": finished")
	v.clean(what + ": finished")
}

// probeOptions sets the address line probe of the new regions of the local
// memdevs of the node, its results in the node's probes file.
func probeOptions(v *env, o *Options) {
	o.ProbeAddressing = true
	o.ProbesFile = filepath.Join(v.dir, "localProbes.json")
}

// sharedOptions configures every local memdev shared, the new regions
// probed.
func sharedOptions(v *env, o *Options) {
	probeOptions(v, o)
	o.Shared = func(state.Serial) bool { return true }
}

// releaseIdleShared starts the node again with no local memdev configured
// shared: the start releases the regions that idle shared memdevs kept.
func releaseIdleShared(v *env) {
	prev := v.tune
	v.tune = func(o *Options) {
		if prev != nil {
			prev(o)
		}
		o.Shared = nil
	}
	v.restart()
}

// sliceReq returns the request of a slice of the unshared local memdev
// serialL for result n of the claim uid.
func sliceReq(uid string, n int, mode state.Mode, size uint64) GrantRequest {
	return localReq(uid, n, serialL, mode, state.KindLocal, size)
}

// sharedReq returns the request of a grant of the local shared memdev
// serialL for the claim uid.
func sharedReq(uid string) GrantRequest {
	return localReq(uid, 0, serialL, state.ModeDax, state.KindLocalShared, gib)
}

// localPmemReq returns the request of a grant of mode fs (access, readOnly)
// or block of the local persistent memdev serialP for result n of the
// claim uid.
func localPmemReq(uid string, n int, mode state.Mode, readOnly bool, access state.Access) GrantRequest {
	req := localReq(uid, n, serialP, mode, state.KindLocalPersistent, 0)
	if mode == state.ModeFS {
		return fsReq(req, readOnly, access)
	}
	req.Asked.ReadOnly = readOnly
	return req
}

// rawRegion creates a ram region on the memdev through the kernel, as
// another tool would, and returns it.
func rawRegion(t *testing.T, v *env, memdev string) string {
	t.Helper()
	k := v.f.Kernel()
	d, err := k.RootDecoder(memdev, kernel.RegionRAM)
	if err != nil {
		t.Fatal(err)
	}
	name, err := k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: memdev, Decoder: d, Type: kernel.RegionRAM})
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// regionAboveNext makes a ram region on the memdev right after the next cxl
// create-region, above the region that it created.
func regionAboveNext(t *testing.T, v *env, memdev string) {
	v.f.After("cxl create-region", func() {
		v.f.After("cxl create-region", func() {})
		rawRegion(t, v, memdev)
	})
}

// dropRaw destroys a region that rawRegion made.
func dropRaw(t *testing.T, v *env, region string) {
	t.Helper()
	if err := v.f.Kernel().DestroyRegion(context.Background(), region); err != nil {
		t.Fatal(err)
	}
}

// wantLog fails the test unless the log has every line of want.
func wantLog(t *testing.T, logs string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(logs, w) {
			t.Errorf("no %q in the log:\n%s", w, logs)
		}
	}
}

// wantRegion fails the test unless the record of the region is in the
// state, nil for none.
func wantRegion(t *testing.T, v *env, region string, want state.RegionState) *state.Region {
	t.Helper()
	reg := v.records().Region(state.RegionRef(region))
	switch {
	case want == "" && reg != nil:
		t.Fatalf("%s: %+v", region, reg)
	case want != "" && (reg == nil || reg.State != want):
		t.Fatalf("%s: %+v, want %s", region, reg, want)
	}
	return reg
}

// wantPreparation fails the test unless the preparation of the claim uid of
// the driver is in the state, "" for none.
func wantPreparation(t *testing.T, v *env, driver, uid string, want state.PreparationState) {
	t.Helper()
	p := v.records().Preparation(driver, uid)
	switch {
	case want == "" && p != nil:
		t.Fatalf("preparation %s: %+v", uid, p)
	case want != "" && (p == nil || p.State != want):
		t.Fatalf("preparation %s: %+v, want %s", uid, p, want)
	}
}

// cutWhenRealised makes the process stop after the commit that makes the
// grant of result 0 of the claim uid REALISED.
func cutWhenRealised(v *env, uid string) {
	v.e.afterCommit = func() {
		if g := v.records().Grant(state.GrantRef{Claim: uid}); g != nil && g.State == state.GrantRealised {
			panic(fake.ErrCut)
		}
	}
}

// pmemEnv returns a node with the persistent pool memdev serialP whose
// fsdax namespace has a filesystem of the type.
func pmemEnv(t *testing.T, typ string) *env {
	t.Helper()
	v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
	v.formatPmem("mem0", typ)
	return v
}

// fsGrant returns the request of a pool grant of mode fs on serialP.
func fsGrant(uid string, readOnly bool, access state.Access) GrantRequest {
	return fsReq(poolReq(uid, 0, serialP, state.ModeFS, state.KindPoolPersistent), readOnly, access)
}

// sources are the sources of the grants of the engine.
var sources = []state.Source{state.SourcePool, state.SourceLocal}

// driverOf returns the driver of the claims of a source.
func driverOf(source state.Source) string {
	if source == state.SourceLocal {
		return localDriver
	}
	return poolDriver
}

// linesWithPrefix returns the messages of the klog output that begin with
// prefix, in order, without the klog header.
func linesWithPrefix(logs, prefix string) []string {
	var lines []string
	for _, l := range strings.Split(logs, "\n") {
		if _, msg, ok := strings.Cut(l, "] "); ok && strings.HasPrefix(msg, prefix) {
			lines = append(lines, msg)
		}
	}
	return lines
}

// busyCompanion prepares the companion claim c with a lease bind on serialP
// whose Unprepare fails on the busy bind, which is free again afterwards.
func busyCompanion(t *testing.T, v *env) string {
	t.Helper()
	target := filepath.Join(v.dir, localDriver, "lease-fs", "lc1", "data")
	v.mustPrepare(poolDriver, "c", bindReq("c", target))
	v.f.SetBusy(target, true)
	if err := v.unprepare(poolDriver, "c"); err == nil {
		t.Fatal("a busy unbind succeeded")
	}
	v.f.SetBusy(target, false)
	return target
}

// bindReq returns the request of a reader of mode fs on serialP of a
// companion claim whose lease bind is at target.
func bindReq(uid, target string) GrantRequest {
	req := fsGrant(uid, true, "")
	req.BindTarget = target
	req.Lend = &state.GrantLend{Lease: "0123abcd", Name: "data"}
	return req
}

// busyCompanionNextToReader prepares the fs claim r and the fs lease
// companion c on the node mount of serialP, then fails the Unprepare of c
// on its busy lease bind. It returns the node mount and the bind.
func busyCompanionNextToReader(t *testing.T, v *env) (string, string) {
	t.Helper()
	target := companionTarget(v)
	v.mustPrepare(poolDriver, "r", fsGrant("r", true, ""))
	v.mustPrepare(poolDriver, "c", bindReq("c", target))
	mnt := mountPathOf(v.records(), v.grant("c", 0))
	if mnt == "" || mnt != mountPathOf(v.records(), v.grant("r", 0)) {
		t.Fatalf("node mounts %q and %q, want one", mnt, mountPathOf(v.records(), v.grant("r", 0)))
	}
	v.f.SetBusy(target, true)
	if err := v.unprepare(poolDriver, "c"); err == nil {
		t.Fatal("a busy unbind succeeded")
	}
	return mnt, target
}

// mustBeMounted fails the test when nothing is mounted at each path.
func (v *env) mustBeMounted(what string, paths ...string) {
	v.t.Helper()
	for _, p := range paths {
		if mi, err := v.f.Kernel().Mounted(p); err != nil || mi == nil {
			v.t.Fatalf("%s: %s is not mounted (%v)", what, p, err)
		}
	}
}

// mustNotBeMounted fails the test when something is mounted at a path.
func (v *env) mustNotBeMounted(what string, paths ...string) {
	v.t.Helper()
	for _, p := range paths {
		if mi, err := v.f.Kernel().Mounted(p); err != nil || mi != nil {
			v.t.Fatalf("%s: %s is mounted: %+v (%v)", what, p, mi, err)
		}
	}
}

// The decoder stacks of the host bridge 12 in the scenarios of the e2e
// model: the local memdev l and the pool devices b and c under it.
var stackSpecs = []fake.MemdevSpec{{Serial: serialL, Ram: gib}, {Serial: serialB}, {Serial: serialC}}

// wantStack fails the test unless the decoder stack of the host bridge of
// the memdev with the serial is the regions of the memdevs of serials, 0 for
// a hole.
func wantStack(t *testing.T, v *env, serials ...uint64) {
	t.Helper()
	k := v.f.Kernel()
	port := k.Memdev(serialL).Ports[0]
	var want []string
	for _, s := range serials {
		if s == 0 {
			want = append(want, "")
			continue
		}
		rs := k.Regions(k.Memdev(s).Name)
		if len(rs) != 1 {
			t.Fatalf("memdev 0x%x has regions %v", s, rs)
		}
		want = append(want, rs[0])
	}
	if got := v.f.Stack(port); !slices.Equal(got, want) {
		t.Fatalf("stack of %s %q, want %q", port, got, want)
	}
}
