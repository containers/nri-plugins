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
	"slices"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
)

// The steps of the records without their commit: each change of a step
// whose commit fails did not happen. The records that the watcher reads
// stay as they were, the scan answers from them (a memdev without a POOL
// or LOCAL record is not published), the line of the change is not logged,
// and the next scan takes the step again.

// TestFailedCommitPool pins notePool without its commit: a LOCAL serial
// that a pool slice lists, and a serial that the pool driver prepares,
// while the writes fail. The list and the scan take the listed serial as
// a pool serial, but its record stays LOCAL, no POOL record of either is
// read, and the error is logged instead of the change; once the slice is
// gone the serial is local again. The watch scans at every tick until the
// next list commits the change.
func TestFailedCommitPool(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	ctx := t.Context()
	e.publish("0xc1ee2001")
	e.plug(localSerial)
	e.wantHeld("local")
	e.wantState(localSerial, state.ProvenanceLocal)
	gen := e.generation()
	restore := e.loseWrites()
	e.publish("0xc1ee2001", "0xc100e2e1")
	serials, _ := e.w.ListPoolSerials(ctx)
	pool, held := e.w.Gate(ctx, e.devices(), serials, nil)
	if !serials[localSerial] || !pool[localSerial] || len(held) != 0 {
		t.Fatalf("listed serials %v, pool %v, held %v", serials, pool, held)
	}
	e.w.NotePrepared(map[uint64]bool{0xa2: true})
	e.wantState(localSerial, state.ProvenanceLocal)
	e.wantState(0xa2, state.ProvenanceUnknown)
	if ps := e.w.RecordedPoolSerials(); ps[localSerial] || ps[0xa2] {
		t.Fatalf("serials of pool devices %v without their commit", ps)
	}
	if g := e.generation(); g != gen {
		t.Fatalf("generation %d, expected %d", g, gen)
	}
	out := logs()
	wantLogLine(t, out, "provenance: writing "+e.name()+": "+errDisk.Error())
	if n := countLogLines(out, "is a pool device now"); n != 0 {
		t.Fatalf("%d lines of a change not committed:\n%s", n, out)
	}
	if !e.w.retrying() {
		t.Fatal("the watch does not scan again")
	}
	e.publish("0xc1ee2001")
	serials, _ = e.w.ListPoolSerials(ctx)
	if pool, held := e.w.Gate(ctx, e.devices(), serials, nil); pool[localSerial] || held[localSerial] {
		t.Fatalf("the slice gone: pool %v, held %v", pool, held)
	}
	// the next scan of the publisher: the prepared serials, the list and
	// the observation
	restore()
	e.publish("0xc1ee2001", "0xc100e2e1")
	e.w.NotePrepared(map[uint64]bool{0xa2: true})
	e.wantHeld("written")
	e.wantState(localSerial, state.ProvenancePool)
	e.wantState(0xa2, state.ProvenancePool)
	e.publish("0xc1ee2001")
	serials, _ = e.w.ListPoolSerials(ctx)
	if pool, _ := e.w.Gate(ctx, e.devices(), serials, nil); !pool[localSerial] {
		t.Fatalf("the slice gone after the commit: pool %v", pool)
	}
	wantLogLine(t, logs(), "provenance: memdev serial 0xc100e2e1 is a pool device now (in a ResourceSlice of "+poolC+")")
	if e.w.retrying() {
		t.Fatal("the watch scans again after the commit")
	}
}

// TestFailedCommitHold pins hold without its commit: a new memdev while a
// pool is missing is not published, but it has no HELD record, is not
// held for the watch, and its hold time does not count; the hold starts
// at the scan that commits it.
func TestFailedCommitHold(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc1ee2001")
	e.wantHeld("pool-c listed")
	e.publish()
	e.plug(otherLocalSerial)
	restore := e.loseWrites()
	e.wantHeld("held, not committed", otherLocalSerial)
	e.wantState(otherLocalSerial, state.ProvenanceUnknown)
	if e.w.Holding() {
		t.Fatal("a hold without its commit")
	}
	out := logs()
	if n := countLogLines(out, holdingBack); n != 0 {
		t.Fatalf("%d lines of a hold not committed:\n%s", n, out)
	}
	wantLogLine(t, out, "provenance: writing "+e.name()+": "+errDisk.Error())
	if !e.w.retrying() {
		t.Fatal("the watch does not scan again")
	}
	e.advance(MaxHold)
	e.wantHeld("held, not committed, a MaxHold later", otherLocalSerial)
	restore()
	e.wantHeld("held", otherLocalSerial)
	if since := e.held(otherLocalSerial); !since.Equal(e.now()) {
		t.Fatalf("held since %v, expected %v", since, e.now())
	}
	wantLogLine(t, logs(), holdingBack+" 0xa1 (mem0)")
	if !e.w.Holding() || e.w.retrying() {
		t.Fatalf("holding %v, retrying %v", e.w.Holding(), e.w.retrying())
	}
}

// TestFailedCommitLocal pins noteLocal without its commit: a new memdev
// without doubt is not published while its commit is refused; a lost lazy
// write publishes it without a LOCAL record, so a later doubt holds it as
// a new memdev.
func TestFailedCommitLocal(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc1ee2001")
	e.wantHeld("pool-c listed")
	restore := e.refuseCommits()
	e.plug(localSerial)
	e.wantHeld("not committed", localSerial)
	e.wantState(localSerial, state.ProvenanceUnknown)
	if e.w.Holding() || !e.w.retrying() {
		t.Fatalf("holding %v, retrying %v", e.w.Holding(), e.w.retrying())
	}
	out := logs()
	if n := countLogLines(out, "is a local memdev"); n != 0 {
		t.Fatalf("%d lines of a change not committed:\n%s", n, out)
	}
	wantLogLine(t, out, "provenance: writing "+e.name()+": "+errDisk.Error())
	restore()
	restore = e.loseWrites()
	e.wantHeld("published, write lost")
	e.wantState(localSerial, state.ProvenanceUnknown)
	restore()
	e.publish()
	e.wantHeld("pool-c missing", localSerial)
}

// TestFailedCommitExpire pins the expiries without their commit: a hold
// that expired stays HELD and is not published until a commit makes it
// LOCAL; a pool serial and a pool observed absent for Retention keep their
// records until a commit removes them.
func TestFailedCommitExpire(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	t0 := e.now()
	e.publish("0xc1ee2001")
	e.wantHeld("pool-c listed")
	e.publish()
	e.plug(otherLocalSerial)
	e.wantHeld("held", otherLocalSerial)
	e.advance(MaxHold)
	restore := e.refuseCommits()
	e.wantHeld("hold expired, not committed", otherLocalSerial)
	e.wantState(otherLocalSerial, state.ProvenanceHeld)
	if !e.w.Holding() {
		t.Fatal("an expiry without its commit")
	}
	if n := countLogLines(logs(), "as a local memdev after holding it back"); n != 0 {
		t.Fatalf("%d lines of a change not committed:\n%s", n, logs())
	}
	restore()
	e.wantHeld("hold expired")
	e.wantState(otherLocalSerial, state.ProvenanceLocal)

	// absent since t0 for Retention: forgotten at the scan after it
	e.idle(Retention - MaxHold)
	e.wantState(0xc1ee2001, state.ProvenancePool)
	restore = e.refuseCommits()
	e.advance(Poll)
	e.wantHeld("absent for Retention, not committed")
	e.wantState(0xc1ee2001, state.ProvenancePool)
	if pools := e.w.records().ListedPools; len(pools) != 1 {
		t.Fatalf("listed pools %+v", pools)
	}
	if !e.w.RecordedPoolSerials()[0xc1ee2001] {
		t.Fatal("a pool serial forgotten without its commit")
	}
	if n := countLogLines(logs(), "provenance: forgetting "); n != 0 {
		t.Fatalf("%d lines of a change not committed:\n%s", n, logs())
	}
	restore()
	e.advance(time.Minute)
	e.wantHeld("absent for Retention")
	e.wantState(0xc1ee2001, state.ProvenanceUnknown)
	if _, pools := e.records(); len(pools) != 0 {
		t.Fatalf("listed pools %+v", pools)
	}
	out := logs()
	wantLogLine(t, out, "provenance: forgetting serial 0xc1ee2001 (pool true, observed absent since "+t0.Format(time.RFC3339)+")")
	wantLogLine(t, out, "provenance: forgetting pool "+poolC+" (observed without ResourceSlices since "+t0.Format(time.RFC3339)+")")
}

// TestFailedCommitForget pins forget without its commit: the records that
// the operator orders removed stay when the store refuses the commit, with
// the error and without the line of the removal: a memdev of the serial is
// a pool device; the next start with the order removes them.
func TestFailedCommitForget(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc1ae2001")
	e.plug(localSerial)
	e.wantHeld("first start")
	e.flush()
	noErr(t, e.st.Close())
	w := New(Options{Store: e.st, Client: e.client, NodeName: testNode, Name: e.name(), Forget: []uint64{sharedPoolSerial},
		Now: e.now})
	out := logs()
	wantLogLine(t, out, "provenance: writing "+e.name()+": "+store.ErrClosed.Error())
	if n := countLogLines(out, "provenance: forgetting serial"); n != 0 {
		t.Fatalf("%d lines of a change not committed:\n%s", n, out)
	}
	if p := w.records().ProvenanceOf(sharedPoolSerial); p == nil || p.State != state.ProvenancePool {
		t.Fatalf("record %+v", p)
	}
	e.plug(sharedPoolSerial)
	if pool, held := w.Gate(t.Context(), e.devices(), nil, nil); !pool[sharedPoolSerial] || len(held) != 0 {
		t.Fatalf("pool %v, held %v", pool, held)
	}
	e.forget = []uint64{sharedPoolSerial}
	e.restart()
	wantLogLine(t, logs(), "provenance: forgetting serial 0xc1ae2001 (POOL, --forget-provenance)")
	e.wantState(sharedPoolSerial, state.ProvenanceUnknown)
}

// TestStepDurableAtApply pins a step whose lazy commit finds a change that
// must be durable, another commit having changed the records after its
// dry run: it is applied again in a durable commit.
func TestStepDurableAtApply(t *testing.T) {
	e := newWatchEnv(t)
	e.publish("0xc1ee2001")
	e.wantHeld("pool-c listed")
	e.publish()
	e.plug(localSerial)
	// a LOCAL record whose LastSeen the scan refreshes lazily
	noErr(t, e.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Provenance = append(doc.Provenance, state.Provenance{Serial: localSerial, State: state.ProvenanceLocal,
			LastSeen: e.now().Add(-2 * refresh)})
		return nil
	}))
	var ds []store.Durability
	commit := e.w.storeCommit
	e.w.storeCommit = func(ctx context.Context, d store.Durability, fn store.Func) error {
		ds = append(ds, d)
		if len(ds) == 1 {
			// removed before the step applies: the memdev is new again
			noErr(t, e.st.Commit(ctx, store.Durable, func(doc *store.Document) error {
				doc.Provenance = slices.DeleteFunc(doc.Provenance, func(p state.Provenance) bool { return p.Serial == localSerial })
				return nil
			}))
		}
		return commit(ctx, d, fn)
	}
	e.wantHeld("held after the record went", localSerial)
	if !slices.Equal(ds, []store.Durability{store.Lazy, store.Durable}) {
		t.Fatalf("durabilities %v", ds)
	}
	if p := e.st.Snapshot().ProvenanceOf(localSerial); p == nil || p.State != state.ProvenanceHeld {
		t.Fatalf("record %+v", p)
	}
}
