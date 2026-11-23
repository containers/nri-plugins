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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
)

// TestRecordsCommitted pins what the store keeps: the POOL and LOCAL
// serials with Serial, State, Pool and LastSeen only, the HELD ones with
// HeldSince and HeldReason, and the listed pools. A new POOL serial is
// committed durably.
func TestRecordsCommitted(t *testing.T) {
	e := newWatchEnv(t)
	t0 := e.now()
	e.publish("0xc1ee2001")
	e.plug(localSerial)
	e.wantHeld("local memdev")
	// pool-c is missing: a new memdev is held
	e.publish()
	e.advance(time.Minute)
	e.plug(otherLocalSerial)
	e.wantHeld("pool-c missing", otherLocalSerial)
	// the second scan held a memdev and changed LastSeen: the hold is
	// committed with the LastSeen of then
	recs, pools := e.records()
	want := []state.Provenance{
		{Serial: otherLocalSerial, State: state.ProvenanceHeld, HeldSince: t0.Add(time.Minute), HeldReason: state.HeldPoolMissing},
		{Serial: localSerial, State: state.ProvenanceLocal, LastSeen: t0.Add(time.Minute)},
		{Serial: 0xc1ee2001, State: state.ProvenancePool, Pool: poolC, LastSeen: t0},
	}
	if !reflect.DeepEqual(recs, want) {
		t.Fatalf("records %+v, expected %+v", recs, want)
	}
	if !reflect.DeepEqual(pools, []state.ListedPool{{Pool: poolC, Listed: t0}}) {
		t.Fatalf("listed pools %+v", pools)
	}
	if e.stateOf(otherLocalSerial) != state.ProvenanceHeld || !e.w.Holding() {
		t.Fatal("the new memdev is not held")
	}
	data, err := os.ReadFile(filepath.Join(e.dir, store.FileName))
	noErr(t, err)
	var doc map[string]any
	noErr(t, json.Unmarshal(data, &doc))
	for _, r := range doc["provenance"].([]any) {
		for k := range r.(map[string]any) {
			if !slices.Contains([]string{"serial", "state", "pool", "lastSeen", "heldSince", "heldReason"}, k) {
				t.Errorf("record field %s committed: %s", k, data)
			}
		}
	}

	// a new pool serial is committed durably: the snapshot has it when the
	// list returns
	e.publish("0xc1ee2001", "0xc1ee2002")
	_, _ = e.w.ListPoolSerials(t.Context())
	if r := e.st.Snapshot().ProvenanceOf(0xc1ee2002); r == nil || r.State != state.ProvenancePool {
		t.Fatalf("the new pool serial is not committed durably: %+v", r)
	}
}

// TestRecordsRefresh pins when the times of last sight of the records are
// committed: not before refresh since they were, then at the next list or
// observation that sees them; a change of the records is committed at
// once, with the times of the records that its step saw.
func TestRecordsRefresh(t *testing.T) {
	e := newWatchEnv(t)
	t0 := e.now()
	e.publish("0xc1ee2001")
	e.plug(localSerial)
	e.wantHeld("first scan")
	e.flush()
	gen := e.st.Snapshot().Generation
	// seen again: LastSeen changes in memory only
	e.advance(refresh - time.Minute)
	e.wantHeld("seen again")
	e.flush()
	if g := e.st.Snapshot().Generation; g != gen {
		t.Fatalf("generation %d after a change of LastSeen only, expected %d", g, gen)
	}
	if r := e.record(0xc1ee2001); r == nil || !r.LastSeen.Equal(t0) {
		t.Fatalf("record %+v, expected LastSeen %v", r, t0)
	}
	if r := e.record(localSerial); r == nil || !r.LastSeen.Equal(t0) {
		t.Fatalf("record %+v, expected LastSeen %v", r, t0)
	}
	// a refresh after the last commit: the list and the observation of
	// the memdevs commit the times of their sight
	e.advance(time.Minute)
	e.wantHeld("a refresh later")
	if r := e.record(0xc1ee2001); r == nil || !r.LastSeen.Equal(t0.Add(refresh)) {
		t.Fatalf("record %+v, expected LastSeen %v", r, t0.Add(refresh))
	}
	if r := e.record(localSerial); r == nil || !r.LastSeen.Equal(t0.Add(refresh)) {
		t.Fatalf("record %+v, expected LastSeen %v", r, t0.Add(refresh))
	}
	if _, pools := e.records(); len(pools) != 1 || !pools[0].Listed.Equal(t0.Add(refresh)) {
		t.Fatalf("listed pools %+v, expected Listed %v", pools, t0.Add(refresh))
	}
	gen = e.st.Snapshot().Generation
	// a change is committed at once
	e.advance(time.Minute)
	e.plug(otherLocalSerial)
	e.wantHeld("a new local memdev")
	if r := e.record(otherLocalSerial); r == nil || r.State != state.ProvenanceLocal || e.st.Snapshot().Generation != gen+1 {
		t.Fatalf("record %+v, generation %d (%d before)", r, e.st.Snapshot().Generation, gen)
	}
}

// TestRecordsLoaded pins the load of the records at New: the line with their
// counts and the name, serials of state POOL that stay pool devices without
// their slices, LOCAL ones that are local at once, and the listed pools.
func TestRecordsLoaded(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	wantLogLine(t, logs(), fmt.Sprintf("provenance: loaded 0 serials (0 of pool devices) and 0 pools from %s", e.name()))
	e.publish("0xc1ee2001", "0xc1ae2001")
	e.plug(localSerial)
	e.wantHeld("first start")
	e.publish()
	e.restart()
	wantLogLine(t, logs(), fmt.Sprintf("provenance: loaded 3 serials (2 of pool devices) and 1 pools from %s", e.name()))
	e.wantState(0xc1ee2001, state.ProvenancePool)
	e.wantState(sharedPoolSerial, state.ProvenancePool)
	e.wantState(localSerial, state.ProvenanceLocal)
	if pools := e.w.records().ListedPools; !reflect.DeepEqual(pools, []state.ListedPool{{Pool: poolC, Listed: e.now()}}) {
		t.Fatalf("pools %v", pools)
	}
	// the pool device is attached while its slices are gone
	e.plug(sharedPoolSerial)
	e.wantHeld("restarted without the slice")
	pool, _ := e.w.Gate(t.Context(), e.devices(), nil, nil)
	if !pool[sharedPoolSerial] || pool[localSerial] {
		t.Fatalf("serials of pool devices %v", pool)
	}
}

// TestLoadHeldRecord pins that a HELD record is loaded: the hold
// goes on from its HeldSince, so a memdev held before a restart is
// published MaxHold after its first hold, not after the restart.
func TestLoadHeldRecord(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc1ee2001")
	e.plug(localSerial)
	e.wantHeld("local memdev")
	e.publish()
	e.advance(time.Minute)
	e.plug(otherLocalSerial)
	e.wantHeld("pool-c missing", otherLocalSerial)
	e.advance(MaxHold - time.Minute)
	e.restart()
	wantLogLine(t, logs(), "provenance: 1 memdevs held before the restart stay held")
	e.wantState(otherLocalSerial, state.ProvenanceHeld)
	e.wantHeld("held before the restart", otherLocalSerial)
	e.advance(2 * time.Minute)
	e.wantHeld("hold expired")
	e.wantState(otherLocalSerial, state.ProvenanceLocal)
}

// TestLostStore pins the records of a LOST store: the error with the lost
// reason at New, a new memdev held for MaxHold within MissingWindow and
// then published as local, a new memdev local at once after MissingWindow,
// and no commit tried on the store.
func TestLostStore(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.lose("{bogus")
	lost := e.st.Lost().Error()
	out := logs()
	wantLogLine(t, out, fmt.Sprintf("provenance: %s: %s: the remembered provenance is lost; new memdevs are held for up "+
		"to 5m0s each in the next 1h0m0s", e.name(), lost))
	if e.w.lost != lost {
		t.Fatalf("lost %q, expected %q", e.w.lost, lost)
	}
	e.plug(localSerial)
	e.wantHeld("lost records", localSerial)
	wantLogLine(t, logs(), fmt.Sprintf(holdingBack+" 0xc100e2e1 (mem0): it may be a pool device: "+
		"the remembered provenance is lost (%s)", lost))
	if reason := e.watched(localSerial).HeldReason; reason != state.HeldLost {
		t.Fatalf("held reason %q", reason)
	}
	e.advance(MaxHold - time.Second)
	e.wantHeld("lost records, before the hold time", localSerial)
	e.advance(time.Second)
	e.wantHeld("lost records, held for long enough")
	e.wantState(localSerial, state.ProvenanceLocal)
	wantLogLine(t, logs(), "provenance: publishing memdev 0xc100e2e1 (mem0) as a local memdev after holding it for 5m0s "+
		"(the remembered provenance is lost (")
	// the store refuses the commits: not tried, nothing logged
	if n := countLogLines(logs(), "provenance: writing "); n != 0 {
		t.Fatalf("%d failed commits logged:\n%s", n, logs())
	}
	// another new memdev within MissingWindow of the first scan
	e.advance(MissingWindow - MaxHold - time.Second)
	e.plug(otherLocalSerial)
	e.wantHeld("lost records, in the window", otherLocalSerial)
	e.advance(time.Second)
	e.wantHeld("lost records, after the window")
	e.plug(0xa2)
	e.wantHeld("a new memdev after the window")
	e.wantState(0xa2, state.ProvenanceLocal)
	e.wantState(otherLocalSerial, state.ProvenanceLocal)
}

// TestForget pins New with Forget: the POOL and LOCAL records of the
// serials are removed with a line each, durably; a serial without a record
// is a warning; a memdev of a removed serial is new again.
func TestForget(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	e := newWatchEnv(t)
	e.publish("0xc1ae2001")
	e.plug(localSerial)
	e.wantHeld("first start")
	e.publish()
	e.forget = []uint64{sharedPoolSerial, localSerial, 0xbeef}
	e.restart()
	out := logs()
	wantLogLine(t, out, "provenance: forgetting serial 0xc1ae2001 (POOL, --forget-provenance)")
	wantLogLine(t, out, "provenance: forgetting serial 0xc100e2e1 (LOCAL, --forget-provenance)")
	wantLogLine(t, out, "provenance: --forget-provenance 0xbeef: no record of the serial")
	if recs := e.st.Snapshot().Provenance; len(recs) != 0 {
		t.Fatalf("records %+v after forgetting them", recs)
	}
	// on disk when New returns
	data, err := os.ReadFile(filepath.Join(e.dir, store.FileName))
	noErr(t, err)
	var doc store.Document
	noErr(t, json.Unmarshal(data, &doc))
	if len(doc.Provenance) != 0 || len(doc.ListedPools) != 1 {
		t.Fatalf("state.json %s", data)
	}
	e.wantState(sharedPoolSerial, state.ProvenanceUnknown)
	e.wantState(localSerial, state.ProvenanceUnknown)
	// pool-c is remembered, and missing: both memdevs are new
	e.plug(sharedPoolSerial)
	e.wantHeld("forgotten serials", localSerial, sharedPoolSerial)
	// a second restart without --forget-provenance keeps that
	e.forget = nil
	e.restart()
	wantLogLine(t, logs(), fmt.Sprintf("provenance: loaded 0 serials (0 of pool devices) and 1 pools from %s", e.name()))
}
