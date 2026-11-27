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

package provenance

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// TestPoolSlicesSerials pins Serials: only the slices of the pool driver
// count, unqualified or qualified serial attributes, invalid ones skipped;
// another pool driver name; a failed list keeps the serials of the last
// one, known and failing; a nil watcher knows that there are none.
func TestPoolSlicesSerials(t *testing.T) {
	client := fake.NewClientset(
		slice("pool", device.PoolDriverName, serialAttr("serial", "0xC1EE0001"), serialAttr("serial", "bogus"), nil),
		slice("other", "other.example.com", serialAttr("other.example.com/serial", "3238060770")),
		slice("node1", device.DriverName, serialAttr("serial", "0xc100e2e1")),
	)
	w := New(Options{Client: client, NodeName: testNode})
	ctx := context.Background()
	serials, known := w.ListPoolSerials(ctx)
	if !known || w.Failing() || !maps.Equal(serials, map[uint64]bool{0xc1ee0001: true}) {
		t.Fatalf("serials %v known %v failing %v", serials, known, w.Failing())
	}
	// another pool driver name
	other := New(Options{Client: client, Driver: "other.example.com", NodeName: testNode})
	if serials, _ := other.ListPoolSerials(ctx); !maps.Equal(serials, map[uint64]bool{3238060770: true}) {
		t.Fatalf("serials of other.example.com %v", serials)
	}

	// an API failure keeps the serials of the last list
	logs := klogtest.Capture(t, 4)
	client.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	serials, known = w.ListPoolSerials(ctx)
	if !known || !w.Failing() || !serials[0xc1ee0001] {
		t.Fatalf("after a failed list: serials %v known %v failing %v", serials, known, w.Failing())
	}
	wantLogLine(t, logs(), "listing the ResourceSlices of pool controllers: apiserver unavailable; using the serials of the last list")

	var none *Watcher
	if serials, known := none.ListPoolSerials(ctx); serials != nil || !known || none.Failing() || none.Holding() {
		t.Fatalf("nil watcher: serials %v known %v", serials, known)
	}
	none.NotePrepared(map[uint64]bool{1: true})
	if pool, held := none.Gate(ctx, &kernel.Snapshot{}, map[uint64]bool{1: true}, nil); !pool[1] || held != nil {
		t.Fatalf("nil watcher: pool %v held %v", pool, held)
	}
}

// TestPoolSlicesFirstListFails pins the watcher before the first successful
// list: not known, failing, only the remembered serials of pool devices; a new
// memdev is held without expiry; after a list it is known.
func TestPoolSlicesFirstListFails(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc1ee0001")
	fail := true
	e.client.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	serials, known := e.w.ListPoolSerials(t.Context())
	if known || !e.w.Failing() || len(serials) != 0 {
		t.Fatalf("serials %v known %v failing %v before the first successful list", serials, known, e.w.Failing())
	}
	wantLogLine(t, logs(), "listing the ResourceSlices of pool controllers: apiserver unavailable; no local memdev is published "+
		"until a list succeeds")
	e.plug(localSerial)
	e.wantHeld("never listed", localSerial)
	wantLogLine(t, logs(), holdingBack+" 0xc100e2e1 (mem0): it may be a pool device: the ResourceSlices "+
		"of pool controllers were never listed")
	e.advance(2 * MaxHold)
	e.wantHeld("never listed, later", localSerial)
	fail = false
	serials, known = e.w.ListPoolSerials(t.Context())
	if !known || e.w.Failing() || !maps.Equal(serials, map[uint64]bool{0xc1ee0001: true}) {
		t.Fatalf("serials %v known %v failing %v after the first successful list", serials, known, e.w.Failing())
	}
}

// TestHeldUntilPoolBack pins a new memdev held while a remembered pool has
// no slices: UNKNOWN to HELD, HELD again at the next scan with its hold
// time kept, and LOCAL when the pool is back without it.
func TestHeldUntilPoolBack(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc1ee2001")
	e.wantHeld("pool-c listed")
	e.publish()
	e.plug(otherLocalSerial)
	e.wantHeld("pool-c missing", otherLocalSerial)
	wantLogLine(t, logs(), holdingBack+" 0xa1 (mem0): it may be a pool device: pools without "+
		"ResourceSlices: "+poolC)
	since := e.held(otherLocalSerial)
	e.advance(time.Minute)
	e.wantHeld("a minute later", otherLocalSerial)
	if got := e.held(otherLocalSerial); !got.Equal(since) {
		t.Fatalf("held since %v, expected %v", got, since)
	}
	if n := countLogLines(logs(), holdingBack+" 0xa1 (mem0)"); n != 1 {
		t.Fatalf("%d lines of the hold, expected 1", n)
	}
	e.publish("0xc1ee2001")
	e.wantHeld("pool-c back")
	e.wantState(otherLocalSerial, state.ProvenanceLocal)
	wantLogLine(t, logs(), "provenance: publishing memdev 0xa1 (mem0) as a local memdev: no pool is missing now")
	wantLogLine(t, logs(), "provenance: memdev 0xa1 (mem0) is a local memdev")
	if r := e.record(otherLocalSerial); r == nil || r.State != state.ProvenanceLocal || r.Pool != "" || !r.LastSeen.Equal(e.now()) {
		t.Fatalf("record %+v", r)
	}
}

// held returns when the memdev of the serial was first held, failing the
// test if it is not held.
func (e *watchEnv) held(serial uint64) time.Time {
	e.t.Helper()
	p := e.watched(serial)
	if p == nil || p.State != state.ProvenanceHeld || p.HeldSince.IsZero() {
		e.t.Fatalf("serial %#x is not held: %+v", serial, p)
	}
	return p.HeldSince
}

// TestHeldGone pins a held memdev that is detached before its hold ended:
// its hold is forgotten, nothing is committed for it.
func TestHeldGone(t *testing.T) {
	e := newWatchEnv(t)
	e.publish("0xc1ee2001")
	e.wantHeld("pool-c listed")
	e.publish()
	e.plug(otherLocalSerial)
	e.wantHeld("held", otherLocalSerial)
	e.unplug(otherLocalSerial)
	e.wantHeld("detached")
	e.wantState(otherLocalSerial, state.ProvenanceUnknown)
	if e.w.Holding() || e.record(otherLocalSerial) != nil {
		t.Fatal("the hold of a detached memdev is kept")
	}
}

// TestHeldBecomesPool pins a held memdev that the returning pool lists: it
// is POOL, durably, with the pool, and no longer held.
func TestHeldBecomesPool(t *testing.T) {
	e := newWatchEnv(t)
	e.publish("0xc1ee2001")
	e.wantHeld("pool-c listed")
	e.publish()
	e.plug(sharedPoolSerial)
	e.wantHeld("held", sharedPoolSerial)
	e.publish("0xc1ee2001", "0xc1ae2001")
	_, _ = e.w.ListPoolSerials(t.Context())
	if r := e.st.Snapshot().ProvenanceOf(sharedPoolSerial); r == nil || r.State != state.ProvenancePool || r.Pool != poolC {
		t.Fatalf("record %+v", r)
	}
	e.wantHeld("listed")
	if rec := *e.watched(sharedPoolSerial); rec.State != state.ProvenancePool || !rec.HeldSince.IsZero() || rec.HeldReason != "" {
		t.Fatalf("entry %+v", rec)
	}
}

// TestLocalBecomesPool pins a LOCAL memdev that a pool slice lists later,
// and a serial that the pool driver prepares: both POOL with their line,
// and POOL stays POOL when it is seen again.
func TestLocalBecomesPool(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.plug(localSerial)
	e.plug(otherLocalSerial)
	e.wantHeld("local")
	e.wantState(localSerial, state.ProvenanceLocal)
	e.publish("0xc100e2e1")
	e.wantHeld("listed by pool-c")
	e.wantState(localSerial, state.ProvenancePool)
	wantLogLine(t, logs(), "provenance: memdev serial 0xc100e2e1 is a pool device now (in a ResourceSlice of "+poolC+")")
	if r := e.record(localSerial); r == nil || r.State != state.ProvenancePool || r.Pool != poolC {
		t.Fatalf("record %+v", r)
	}
	e.w.NotePrepared(map[uint64]bool{otherLocalSerial: true, 0xa2: true})
	wantLogLine(t, logs(), "provenance: memdev serial 0xa1 is a pool device now (prepared by the pool driver)")
	e.wantState(otherLocalSerial, state.ProvenancePool)
	e.wantState(0xa2, state.ProvenancePool)
	if r := e.st.Snapshot().ProvenanceOf(0xa2); r == nil || r.State != state.ProvenancePool || r.Pool != "" {
		t.Fatalf("record of a prepared serial %+v", r)
	}
	// seen again: its LastSeen is refreshed a refresh after its change
	changed := e.now()
	e.advance(time.Minute)
	e.wantHeld("seen again")
	e.wantState(localSerial, state.ProvenancePool)
	if seen := e.watched(localSerial).LastSeen; !seen.Equal(changed) {
		t.Fatalf("last seen %v, expected %v", seen, changed)
	}
	e.advance(refresh)
	e.wantHeld("seen a refresh later")
	if seen := e.watched(localSerial).LastSeen; !seen.Equal(e.now()) {
		t.Fatalf("last seen %v, expected %v", seen, e.now())
	}
	e.w.NotePrepared(nil)
	if e.w.prepared != nil {
		t.Fatalf("prepared %v", e.w.prepared)
	}
}

// TestAbsenceExpires pins the observed absence: POOL and LOCAL serials are
// forgotten, and so is a pool, after Retention of observations at most
// observationGap apart; a longer gap starts the absence again.
func TestAbsenceExpires(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc1ee2001")
	e.plug(localSerial)
	e.wantHeld("first scan")
	e.publish()
	e.unplug(localSerial)
	// a day without observations, then one: the absence starts there
	e.advance(Retention + time.Hour)
	e.wantHeld("a day later")
	e.wantState(0xc1ee2001, state.ProvenancePool)
	e.wantState(localSerial, state.ProvenanceLocal)
	// observed, but with a gap
	e.advance(observationGap + time.Second)
	e.wantHeld("after a gap")
	e.idle(Retention - time.Minute)
	e.wantState(0xc1ee2001, state.ProvenancePool)
	e.wantState(localSerial, state.ProvenanceLocal)
	e.idle(2 * time.Minute)
	e.wantState(0xc1ee2001, state.ProvenanceUnknown)
	e.wantState(localSerial, state.ProvenanceUnknown)
	out := logs()
	wantLogLine(t, out, "provenance: forgetting serial 0xc1ee2001 (pool true, observed absent since ")
	wantLogLine(t, out, "provenance: forgetting serial 0xc100e2e1 (pool false, observed absent since ")
	wantLogLine(t, out, "provenance: forgetting pool "+poolC+" (observed without ResourceSlices since ")
	recs, pools := e.records()
	if len(recs) != 0 || len(pools) != 0 {
		t.Fatalf("records %+v, pools %+v", recs, pools)
	}
}

// TestClaimEvidence pins the pool claims of the node: the serial of an
// attached one is POOL with its pool, an attaching one holds new memdevs
// without expiry, failed attaches and claims of other nodes do not count,
// and claims that cannot be listed hold new memdevs for MaxHold.
func TestClaimEvidence(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.claim("attached", testNode, "c-shared0", "0xc1ae2001")
	e.claim("failed", testNode, "c-pooled1", "failed")
	e.claim("elsewhere", "node2", "c-pooled2", "")
	e.plug(localSerial)
	e.plug(sharedPoolSerial)
	e.wantHeld("first start")
	e.wantState(localSerial, state.ProvenanceLocal)
	if r := e.record(sharedPoolSerial); r == nil || r.State != state.ProvenancePool || r.Pool != poolC {
		t.Fatalf("record %+v", r)
	}
	e.claim("attaching", testNode, "c-pooled0", "")
	e.plug(otherLocalSerial)
	e.wantHeld("attach in flight", otherLocalSerial)
	wantLogLine(t, logs(), holdingBack+" 0xa1 (mem2): it may be a pool device: pool claims of the "+
		"node are not attached yet: "+testNS+"/attaching")
	e.advance(2 * MaxHold)
	e.wantHeld("attach in flight, later", otherLocalSerial)
	e.claim("attaching", testNode, "c-pooled0", "0xc1ee2001")
	e.wantHeld("attached")
	e.wantState(otherLocalSerial, state.ProvenanceLocal)

	// the claims cannot be listed
	e.client.PrependReactor("list", "resourceclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	e.plug(0xa2)
	e.wantHeld("claims not listable", 0xa2)
	out := logs()
	wantLogLine(t, out, "provenance: listing the pool claims of the node: apiserver unavailable")
	wantLogLine(t, out, holdingBack+" 0xa2 (mem3): it may be a pool device: the pool claims of the "+
		"node cannot be listed")
	e.advance(MaxHold)
	e.wantHeld("claims not listable, held for long enough")
	e.wantState(0xa2, state.ProvenanceLocal)
	// no new memdev: the claims are not listed
	e.wantHeld("nothing new")
}

// TestPoolDeviceUsedByLocalClaims pins the error, once, about a pool
// device that local claims use: it is a pool serial of the gate.
func TestPoolDeviceUsedByLocalClaims(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc100e2e1")
	e.plug(localSerial)
	serials, _ := e.w.ListPoolSerials(t.Context())
	local := map[uint64][]string{localSerial: {"u1"}}
	for range 2 {
		pool, held := e.w.Gate(t.Context(), e.devices(), serials, local)
		if !pool[localSerial] || len(held) != 0 {
			t.Fatalf("pool %v held %v", pool, held)
		}
	}
	const text = "provenance: memdev 0xc100e2e1 (mem0) is a pool device, but local claims use it: it is no longer " +
		"published, and no local claim is prepared on it; release the local claims ([u1]) and check its memory"
	if n := countLogLines(logs(), text); n != 1 {
		t.Fatalf("%d lines %q:\n%s", n, text, logs())
	}
	// a memdev that local claims use is local, also while a pool is missing
	e.publish()
	e.plug(otherLocalSerial)
	_, held := e.w.Gate(t.Context(), e.devices(), nil, map[uint64][]string{otherLocalSerial: {"u2"}})
	if len(held) != 0 {
		t.Fatalf("held %v", held)
	}
	e.wantState(otherLocalSerial, state.ProvenanceLocal)
}

// TestCheckLocalRefusals pins CheckLocal, the fresh list of the engine's
// guard: a serial that a pool slice lists now is refused, with its pool,
// and its POOL record is in the snapshot when CheckLocal returns. A serial
// that the slices no longer list, a prepared one and a held memdev pass
// CheckLocal: their POOL and HELD records, which the engine reads itself,
// refuse them. A local memdev and a nil watcher are accepted.
func TestCheckLocalRefusals(t *testing.T) {
	e := newWatchEnv(t)
	ctx := t.Context()
	e.publish("0xc1ee2001")
	e.plug(localSerial)
	e.wantHeld("local")
	noErr(t, e.w.CheckLocal(ctx, localSerial))
	// pool-c lists it now, before any scan
	e.publish("0xc1ee2001", "0xc100e2e1")
	wantErr(t, e.w.CheckLocal(ctx, localSerial),
		"memdev 0xc100e2e1 is a pool device of "+poolC+": it is not used as a local memdev")
	snapshotState := func(serial uint64) state.ProvenanceState {
		if r := e.st.Snapshot().ProvenanceOf(state.Serial(serial)); r != nil {
			return r.State
		}
		return state.ProvenanceUnknown
	}
	if st := snapshotState(localSerial); st != state.ProvenancePool {
		t.Fatalf("a listed serial is %s in the snapshot", st)
	}
	e.publish()
	noErr(t, e.w.CheckLocal(ctx, localSerial))
	if st := snapshotState(localSerial); st != state.ProvenancePool {
		t.Fatalf("a remembered pool serial is %s in the snapshot", st)
	}
	// prepared by the pool driver
	e.w.NotePrepared(map[uint64]bool{0xa2: true})
	noErr(t, e.w.CheckLocal(ctx, 0xa2))
	if st := snapshotState(0xa2); st != state.ProvenancePool {
		t.Fatalf("a prepared serial is %s in the snapshot", st)
	}
	e.plug(otherLocalSerial)
	e.wantHeld("held", otherLocalSerial)
	noErr(t, e.w.CheckLocal(ctx, otherLocalSerial))
	if st := snapshotState(otherLocalSerial); st != state.ProvenanceHeld {
		t.Fatalf("a held memdev is %s in the snapshot", st)
	}
	if err := (*Watcher)(nil).CheckLocal(ctx, localSerial); err != nil {
		t.Fatal(err)
	}
	// not listed by a watcher without the records
	fresh := New(Options{Client: e.client, NodeName: testNode})
	noErr(t, fresh.CheckLocal(ctx, localSerial))
}

// TestWatch pins the ticks of Watch: a rescan (not periodic) at every
// ListRetry while the list fails, a memdev is held or the last commit of a
// step failed, none while none of them and the node was scanned, a
// periodic one after Poll without a scan, and the end with ctx.
func TestWatch(t *testing.T) {
	oldRetry, oldPoll := ListRetry, Poll
	t.Cleanup(func() { ListRetry, Poll = oldRetry, oldPoll })
	ListRetry = 5 * time.Millisecond

	var failing atomic.Bool
	listErr := errors.New("apiserver unavailable")
	var holding atomic.Bool
	client := fake.NewClientset()
	client.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, listErr
		}
		return false, nil, nil
	})
	client.PrependReactor("list", "resourceclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		if holding.Load() {
			return true, &resourcev1.ResourceClaimList{Items: []resourcev1.ResourceClaim{*poolClaim("attaching", testNode, "c-pooled0", "")}}, nil
		}
		return false, nil, nil
	})
	w := New(Options{Client: client, NodeName: testNode})
	ctx := t.Context()
	var mu sync.Mutex
	var calls []bool
	last := time.Now()
	scan := func() {
		serials, _ := w.ListPoolSerials(ctx)
		w.Gate(ctx, &kernel.Snapshot{Memdevs: []kernel.MemdevInfo{{Name: "mem0", Serial: otherLocalSerial}}}, serials, nil)
	}
	rescan := func(_ context.Context, periodic bool) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, periodic)
	}
	lastScan := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}
	// watch runs Watch until it rescanned twice (want: periodic), or for
	// 20 ticks if it must not rescan (no want).
	watch := func(what string, want ...bool) {
		t.Helper()
		mu.Lock()
		calls = nil
		mu.Unlock()
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); w.Watch(ctx, rescan, lastScan) }()
		if len(want) == 0 {
			time.Sleep(20 * ListRetry)
		}
		for end := time.Now().Add(5 * time.Second); len(want) > 0 && count() < 2 && time.Now().Before(end); {
			time.Sleep(time.Millisecond)
		}
		cancel()
		<-done
		mu.Lock()
		defer mu.Unlock()
		if len(want) == 0 && len(calls) != 0 {
			t.Fatalf("%s: rescans %v, expected none", what, calls)
		}
		if len(want) > 0 && len(calls) < 2 {
			t.Fatalf("%s: rescans %v, expected one at every tick", what, calls)
		}
		for _, periodic := range calls {
			if periodic != want[0] {
				t.Fatalf("%s: rescans %v, expected periodic %v", what, calls, want[0])
			}
		}
	}

	// the list fails
	Poll = time.Hour
	failing.Store(true)
	scan()
	watch("list failing", false)
	// a memdev is held
	failing.Store(false)
	holding.Store(true)
	scan()
	if !w.Holding() {
		t.Fatal("not holding")
	}
	watch("held", false)
	// neither, scanned
	holding.Store(false)
	scan()
	if w.Holding() || w.Failing() {
		t.Fatal("holding or failing")
	}
	watch("idle")
	// the last commit of an observation failed
	w.mu.Lock()
	w.uncommitted["observation"] = true
	w.mu.Unlock()
	watch("commit failed", false)
	w.mu.Lock()
	delete(w.uncommitted, "observation")
	w.mu.Unlock()
	// not scanned for Poll
	Poll = 20 * time.Millisecond
	mu.Lock()
	last = time.Now().Add(-time.Second)
	mu.Unlock()
	watch("not scanned for Poll", true)
}

// TestCheckLocalListFails pins that a failed list of the pool
// ResourceSlices refuses a new local grant (fail closed) until a list
// succeeds again.
func TestCheckLocalListFails(t *testing.T) {
	e := newWatchEnv(t)
	ctx := t.Context()
	e.publish("0xc1ee2001")
	e.plug(localSerial)
	e.wantHeld("local")
	var fail atomic.Bool
	e.client.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	fail.Store(true)
	wantErr(t, e.w.CheckLocal(ctx, localSerial), "the ResourceSlices of pool controllers cannot be listed (apiserver unavailable): "+
		"memdev 0xc100e2e1 is not used as a local memdev until a list succeeds")
	fail.Store(false)
	noErr(t, e.w.CheckLocal(ctx, localSerial))
}

// TestCheckLocalOwnList pins that CheckLocal decides on its own list, and
// that a list that started before it and ends after it is not recorded
// over it: a scan's slow list that missed the pool slice, or that
// succeeded while CheckLocal's list failed, lets no local use pass.
func TestCheckLocalOwnList(t *testing.T) {
	for _, tc := range []string{"listed", "failing"} {
		t.Run(tc, func(t *testing.T) {
			e := newWatchEnv(t)
			ctx := t.Context()
			e.publish("0xc100e2e1") // localSerial
			var calls atomic.Int32
			started, release, scanned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			list := e.w.listSlices
			e.w.listSlices = func(ctx context.Context) ([]resourcev1.ResourceSlice, error) {
				switch {
				case calls.Add(1) == 1: // the scan's: without the pool slice
					close(started)
					<-release
					return nil, nil
				case tc == "failing":
					return nil, errors.New("apiserver unavailable")
				}
				return list(ctx)
			}
			go func() {
				defer close(scanned)
				e.w.ListPoolSerials(ctx)
			}()
			<-started
			// the scan's list ends after CheckLocal's list is recorded,
			// before CheckLocal decides
			var once sync.Once
			endScan := func() {
				once.Do(func() {
					close(release)
					<-scanned
				})
			}
			commit := e.w.storeCommit
			e.w.storeCommit = func(ctx context.Context, d store.Durability, fn store.Func) error {
				endScan()
				return commit(ctx, d, fn)
			}
			err := e.w.CheckLocal(ctx, localSerial)
			endScan()
			if tc == "listed" {
				wantErr(t, err, "memdev 0xc100e2e1 is a pool device of "+device.PoolDriverName+"/pool-c: "+
					"it is not used as a local memdev")
			} else {
				wantErr(t, err, "the ResourceSlices of pool controllers cannot be listed (apiserver unavailable): "+
					"memdev 0xc100e2e1 is not used as a local memdev until a list succeeds")
			}
			e.w.mu.Lock()
			_, listed := e.w.serials[localSerial]
			failing := e.w.failing
			e.w.mu.Unlock()
			if listed != (tc == "listed") || failing != (tc == "failing") {
				t.Fatalf("recorded: listed %v, failing %v", listed, failing)
			}
		})
	}
}
