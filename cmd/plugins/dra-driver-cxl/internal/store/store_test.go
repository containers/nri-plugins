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

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// hookFiles are file operations that call hook before each operation and
// fail it with the hook's error.
type hookFiles struct {
	base fileOps
	mu   sync.Mutex
	hook func(op, name string) error
	ops  []string
}

func (h *hookFiles) call(op, name string) error {
	h.mu.Lock()
	hook := h.hook
	h.ops = append(h.ops, op+" "+filepath.Base(name))
	h.mu.Unlock()
	if hook != nil {
		return hook(op, name)
	}
	return nil
}

func (h *hookFiles) setHook(hook func(op, name string) error) {
	h.mu.Lock()
	h.hook = hook
	h.mu.Unlock()
}

func (h *hookFiles) WriteFile(name string, data []byte, sync bool) error {
	if err := h.call("write", name); err != nil {
		return err
	}
	return h.base.WriteFile(name, data, sync)
}

func (h *hookFiles) Rename(o, n string) error {
	if err := h.call("rename", o); err != nil {
		return err
	}
	return h.base.Rename(o, n)
}

func (h *hookFiles) Link(o, n string) error {
	if err := h.call("link", o); err != nil {
		return err
	}
	return h.base.Link(o, n)
}

func (h *hookFiles) Remove(name string) error {
	if err := h.call("remove", name); err != nil {
		return err
	}
	return h.base.Remove(name)
}

func (h *hookFiles) SyncDir(dir string) error {
	if err := h.call("syncdir", dir); err != nil {
		return err
	}
	return h.base.SyncDir(dir)
}

// open opens a store in dir with the file operations files ("": the
// operating system's).
func open(t *testing.T, dir string, files fileOps, change func(o *Options)) *Store {
	t.Helper()
	opts := Options{Dir: dir, BootID: "boot-1", files: files}
	if change != nil {
		change(&opts)
	}
	s, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// closeStore closes the store s and fails the test on an error.
func closeStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// addSteered returns a commit that adds a steering record of the container
// id.
func addSteered(id string) Func {
	return func(doc *Document) error {
		doc.Steered = append(doc.Steered, state.Steered{Container: id, CgroupDir: "/cg/" + id})
		return nil
	}
}

// steered returns the containers of the steering records of a document.
func steered(doc *Document) []string {
	var out []string
	for _, s := range doc.Steered {
		out = append(out, s.Container)
	}
	return out
}

// reload returns the document that a new store loads from dir.
func reload(t *testing.T, dir string) *Document {
	t.Helper()
	s := open(t, dir, nil, nil)
	defer closeStore(t, s)
	return s.Snapshot()
}

func TestOpenCreatesTheDocument(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, nil, nil)
	defer closeStore(t, s)
	doc := s.Snapshot()
	if doc.Schema != Schema || doc.BootID != "boot-1" || doc.Generation != 1 {
		t.Errorf("new document %+v", doc)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("no %s: %v", FileName, err)
	}
	if s.Lost() != nil {
		t.Errorf("a new store is lost: %v", s.Lost())
	}
}

func TestDurableCommit(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, nil, nil)
	before := s.Snapshot()
	for _, id := range []string{"a", "b"} {
		if err := s.Commit(context.Background(), Durable, addSteered(id)); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	doc := s.Snapshot()
	if got := steered(doc); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("snapshot has %v", got)
	}
	if doc.Generation != before.Generation+2 {
		t.Errorf("generation %d after two commits of generation %d", doc.Generation, before.Generation)
	}
	if len(before.Steered) != 0 {
		t.Errorf("an earlier snapshot changed: %v", steered(before))
	}
	closeStore(t, s)
	if got := steered(reload(t, dir)); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("reloaded document has %v", got)
	}
	prev, err := readDocument(filepath.Join(dir, PrevName))
	if err != nil {
		t.Fatalf("previous document: %v", err)
	}
	if got := steered(prev); !reflect.DeepEqual(got, []string{"a"}) || prev.Generation != doc.Generation-1 {
		t.Errorf("previous document generation %d with %v", prev.Generation, got)
	}
}

func TestFailedFunctionChangesNothing(t *testing.T) {
	s := open(t, t.TempDir(), nil, nil)
	defer closeStore(t, s)
	boom := errors.New("refused")
	err := s.Commit(context.Background(), Durable, func(doc *Document) error {
		doc.Steered = append(doc.Steered, state.Steered{Container: "x"})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("Commit: %v, want %v", err, boom)
	}
	if len(s.Snapshot().Steered) != 0 {
		t.Errorf("the change of a failed function is in the snapshot")
	}
	if err := s.Commit(context.Background(), Durable, addSteered("y")); err != nil {
		t.Fatal(err)
	}
	if got := steered(s.Snapshot()); !reflect.DeepEqual(got, []string{"y"}) {
		t.Errorf("snapshot has %v", got)
	}
}

func TestRefusedTransitionFailsTheCommit(t *testing.T) {
	s := open(t, t.TempDir(), nil, nil)
	defer closeStore(t, s)
	err := s.Commit(context.Background(), Durable, func(doc *Document) error {
		doc.Preparations = append(doc.Preparations, state.Preparation{UID: "u", Driver: "d", State: state.PreparationNone})
		p := &doc.Preparations[0]
		return fsm.Apply(&state.PreparationTable, &p.State, state.PreparationPrepared, state.TriggerPrepare)
	})
	if !errors.Is(err, fsm.ErrTransition) {
		t.Errorf("Commit: %v, want a refused transition", err)
	}
	if len(s.Snapshot().Preparations) != 0 {
		t.Errorf("the preparation of a refused transition is in the snapshot")
	}
}

func TestLazyCommit(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, nil, nil)
	if err := s.Commit(context.Background(), Lazy, addSteered("lazy")); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	closeStore(t, s)
	if got := steered(reload(t, dir)); !reflect.DeepEqual(got, []string{"lazy"}) {
		t.Errorf("reloaded document has %v", got)
	}
}

func TestUnchangedDocumentIsNotWritten(t *testing.T) {
	s := open(t, t.TempDir(), nil, nil)
	defer closeStore(t, s)
	n := s.writes.Load()
	if err := s.Commit(context.Background(), Durable, func(*Document) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if s.writes.Load() != n {
		t.Errorf("a commit without a change wrote the document")
	}
}

func TestGroupCommit(t *testing.T) {
	files := &hookFiles{base: osFiles{}}
	s := open(t, t.TempDir(), files, nil)
	defer closeStore(t, s)
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	files.setHook(func(op, name string) error {
		if op == "write" && filepath.Base(name) == FileName+".tmp" {
			once.Do(func() {
				close(blocked)
				<-release
			})
		}
		return nil
	})
	writes := s.writes.Load()
	errs := make(chan error, 11)
	go func() { errs <- s.Commit(context.Background(), Durable, addSteered("first")) }()
	<-blocked
	for i := range 10 {
		go func() { errs <- s.Commit(context.Background(), Durable, addSteered(fmt.Sprintf("c%02d", i))) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.pending)
		s.mu.Unlock()
		if n == 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d commits pending, want 10", n)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range 11 {
		if err := <-errs; err != nil {
			t.Errorf("Commit: %v", err)
		}
	}
	if got := s.writes.Load() - writes; got != 2 {
		t.Errorf("%d writes for a commit and a group of 10, want 2", got)
	}
	if got := len(s.Snapshot().Steered); got != 11 {
		t.Errorf("%d records after 11 commits", got)
	}
}

// stallWrite makes the next write of the document wait until release is
// closed and then fail with fault (nil: succeed); blocked is closed when
// the write waits.
func stallWrite(files *hookFiles, fault error) (release, blocked chan struct{}) {
	release, blocked = make(chan struct{}), make(chan struct{})
	var stalled atomic.Bool
	files.setHook(func(op, name string) error {
		if op != "write" || filepath.Base(name) != FileName+".tmp" || stalled.Swap(true) {
			return nil
		}
		close(blocked)
		<-release
		return fault
	})
	return release, blocked
}

// A commit whose context ends while it waits in the queue is withdrawn: it
// fails with the error of the context, and neither the snapshot nor the
// file ever shows it. A commit whose context ends while the writer writes
// it returns the outcome of that write.
func TestCommitWhoseContextEnds(t *testing.T) {
	t.Run("withdrawn", func(t *testing.T) {
		dir := t.TempDir()
		files := &hookFiles{base: osFiles{}}
		s := open(t, dir, files, nil)
		release, blocked := stallWrite(files, nil)
		first := make(chan error, 1)
		go func() { first <- s.Commit(context.Background(), Durable, addSteered("first")) }()
		<-blocked
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := s.Commit(ctx, Durable, addSteered("withdrawn")); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Commit of a request in the queue: %v, want the deadline", err)
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if got := steered(s.Snapshot()); !slices.Equal(got, []string{"first"}) {
			t.Errorf("snapshot %v, want the withdrawn commit left out", got)
		}
		closeStore(t, s)
		if got := steered(reload(t, dir)); !slices.Equal(got, []string{"first"}) {
			t.Errorf("reloaded %v, want the withdrawn commit left out", got)
		}
	})
	for _, fault := range []error{nil, errors.New("disk failure")} {
		t.Run(fmt.Sprintf("taken, write error %v", fault), func(t *testing.T) {
			files := &hookFiles{base: osFiles{}}
			s := open(t, t.TempDir(), files, nil)
			defer closeStore(t, s)
			release, blocked := stallWrite(files, fault)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.Commit(ctx, Durable, addSteered("taken")) }()
			<-blocked
			cancel()
			select {
			case err := <-done:
				close(release)
				t.Fatalf("Commit returned %v while the writer wrote it", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			err := <-done
			want := []string{"taken"}
			if fault != nil {
				want = nil
			}
			if !errors.Is(err, fault) || !slices.Equal(steered(s.Snapshot()), want) {
				t.Errorf("Commit %v, snapshot %v; want error %v and %v", err, steered(s.Snapshot()), fault, want)
			}
		})
	}
}

// writeSteps are the operations of a write of the document after the
// first one, in order.
var writeSteps = []string{
	"write " + FileName + ".tmp",
	"remove " + PrevName + ".tmp",
	"link " + FileName,
	"rename " + PrevName + ".tmp",
	"rename " + FileName + ".tmp",
	"syncdir",
}

// TestWriteFailuresAndCutPoints fails each step of a write in turn: the
// commit fails, the snapshot stays the last written document, a reload
// gives the last good document (or the new one when only the sync of the
// directory failed), the previous document decodes, and the next commit
// succeeds without the failed change. After a failed sync of the
// directory the discarded group is visible in state.json; the next write
// links it to state.json.prev and reuses its generation.
func TestWriteFailuresAndCutPoints(t *testing.T) {
	for i, step := range writeSteps {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			files := &hookFiles{base: osFiles{}}
			s := open(t, dir, files, nil)
			if err := s.Commit(context.Background(), Durable, addSteered("1-good")); err != nil {
				t.Fatal(err)
			}
			good := s.Snapshot()
			files.mu.Lock()
			files.ops = nil
			files.mu.Unlock()
			cut := errors.New("cut")
			files.setHook(func(op, name string) error {
				if strings.HasPrefix(op+" "+filepath.Base(name), step) || (step == "syncdir" && op == "syncdir") {
					return cut
				}
				return nil
			})
			err := s.Commit(context.Background(), Durable, addSteered("2-failed"))
			if !errors.Is(err, cut) {
				t.Fatalf("Commit: %v, want the cut", err)
			}
			files.mu.Lock()
			ops := files.ops
			files.mu.Unlock()
			if len(ops) != i+1 {
				t.Errorf("operations %v, want the first %d of %v", ops, i+1, writeSteps)
			}
			if s.Snapshot() != good {
				t.Errorf("the snapshot changed after a failed write")
			}
			files.setHook(nil)
			closeStore(t, s)

			loaded := reload(t, dir)
			want := []string{"1-good"}
			if step == "syncdir" {
				want = []string{"1-good", "2-failed"}
			}
			if got := steered(loaded); !reflect.DeepEqual(got, want) {
				t.Errorf("reloaded %v, want %v", got, want)
			}
			if _, err := readDocument(filepath.Join(dir, PrevName)); err != nil {
				t.Errorf("previous document: %v", err)
			}

			s = open(t, dir, nil, nil)
			defer closeStore(t, s)
			if err := s.Commit(context.Background(), Durable, addSteered("3-next")); err != nil {
				t.Fatal(err)
			}
			if got := steered(s.Snapshot()); !reflect.DeepEqual(got, append(want, "3-next")) {
				t.Errorf("after the next commit %v", got)
			}
		})
	}
}

func TestWriteFailureFailsTheWholeGroup(t *testing.T) {
	files := &hookFiles{base: osFiles{}}
	s := open(t, t.TempDir(), files, nil)
	defer closeStore(t, s)
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	full := errors.New("no space left on device")
	failing := false
	files.setHook(func(op, name string) error {
		if op != "write" || filepath.Base(name) != FileName+".tmp" {
			return nil
		}
		once.Do(func() {
			close(blocked)
			<-release
		})
		files.mu.Lock()
		defer files.mu.Unlock()
		if failing {
			return full
		}
		failing = true
		return nil
	})
	errs := make(chan error, 4)
	go func() { errs <- s.Commit(context.Background(), Durable, addSteered("first")) }()
	<-blocked
	for _, id := range []string{"x", "y", "z"} {
		go func() { errs <- s.Commit(context.Background(), Durable, addSteered(id)) }()
	}
	for {
		s.mu.Lock()
		n := len(s.pending)
		s.mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	failed := 0
	for range 4 {
		if err := <-errs; errors.Is(err, full) {
			failed++
		}
	}
	if failed != 3 {
		t.Errorf("%d commits of the failed group got the error, want 3", failed)
	}
	if got := steered(s.Snapshot()); !reflect.DeepEqual(got, []string{"first"}) {
		t.Errorf("snapshot %v after a failed group", got)
	}
}

func TestPrevFallback(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, nil, nil)
	for _, id := range []string{"a", "b"} {
		if err := s.Commit(context.Background(), Durable, addSteered(id)); err != nil {
			t.Fatal(err)
		}
	}
	closeStore(t, s)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"schema": 1, "generation": `), 0o600); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, nil, nil)
	if got := steered(s.Snapshot()); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("loaded %v from the previous document, want [a]", got)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName+".bad")); err != nil {
		t.Errorf("the bad document was not moved aside: %v", err)
	}
	if err := s.Commit(context.Background(), Durable, addSteered("c")); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)
	prev, err := readDocument(filepath.Join(dir, PrevName))
	if err != nil {
		t.Fatalf("previous document after the fallback: %v", err)
	}
	if got := steered(prev); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("previous document %v", got)
	}
	if got := steered(reload(t, dir)); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("reloaded %v", got)
	}
}

func TestMissingDocumentUsesPrev(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, nil, nil)
	for _, id := range []string{"a", "b"} {
		if err := s.Commit(context.Background(), Durable, addSteered(id)); err != nil {
			t.Fatal(err)
		}
	}
	closeStore(t, s)
	if err := os.Remove(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if got := steered(reload(t, dir)); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("loaded %v, want the previous document [a]", got)
	}
}

func TestLostMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state, prev string
	}{
		{"garbage", "{", "nonsense"},
		{"unknown schema", `{"schema": 3, "generation": 1}`, `{"schema": 3, "generation": 0}`},
		{"unknown field", `{"schema": 1, "generation": 1, "fence": []}`, ""},
		{
			"unknown state",
			`{"schema": 1, "generation": 1, "preparations": [{"uid": "u", "driver": "cxl.nri.io", "claimName": "n/c", ` +
				`"state": "PREPARD", "results": []}]}`,
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(tc.state), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.prev != "" {
				if err := os.WriteFile(filepath.Join(dir, PrevName), []byte(tc.prev), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s := open(t, dir, nil, nil)
			lost := s.Lost()
			var le *LostError
			if !errors.As(lost, &le) || !errors.Is(lost, ErrLost) {
				t.Fatalf("Lost: %v, want a *LostError", lost)
			}
			if !strings.Contains(lost.Error(), "--new-state") {
				t.Errorf("lost error %q does not tell the way out", lost)
			}
			if tc.name == "unknown state" && !strings.Contains(lost.Error(), `preparation cxl.nri.io/u: state "PREPARD"`) {
				t.Errorf("lost error %q does not name the state", lost)
			}
			if tc.name == "unknown schema" && !strings.Contains(lost.Error(), "unsupported schema 3 (this driver reads schema 1)") {
				t.Errorf("lost error %q does not name the schema", lost)
			}
			if tc.name == "unknown field" && !strings.Contains(lost.Error(), `unknown field "fence"`) {
				t.Errorf("lost error %q does not name the field", lost)
			}
			if s.Origin() != Lost {
				t.Errorf("origin %v of a lost store", s.Origin())
			}
			err := s.Commit(context.Background(), Durable, addSteered("x"))
			if !errors.Is(err, ErrLost) {
				t.Errorf("Commit in LOST mode: %v", err)
			}
			doc := s.Snapshot()
			if len(doc.Steered) != 0 || doc.BootID != "boot-1" {
				t.Errorf("snapshot in LOST mode %+v", doc)
			}
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			data, _ := os.ReadFile(filepath.Join(dir, FileName))
			if string(data) != tc.state {
				t.Errorf("LOST mode changed %s", FileName)
			}
			if entries, err := os.ReadDir(dir); err == nil {
				for _, e := range entries {
					if n := e.Name(); n != FileName && n != PrevName && n != LockName {
						t.Errorf("LOST mode wrote %s", n)
					}
				}
			}

			s = open(t, dir, nil, func(o *Options) { o.NewState = true })
			defer closeStore(t, s)
			if s.Lost() != nil {
				t.Errorf("lost with --new-state")
			}
			if _, err := os.Stat(filepath.Join(dir, FileName+".lost")); err != nil {
				t.Errorf("the lost document was not kept: %v", err)
			}
			if err := s.Commit(context.Background(), Durable, addSteered("new")); err != nil {
				t.Errorf("Commit with a new state: %v", err)
			}
		})
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	host, _ := os.Hostname()
	holder := fmt.Sprintf("pid %d host %s", os.Getpid(), host)
	if _, err := Open(ctx, Options{Dir: dir, BootID: "boot-1"}); !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "held by "+holder) {
		t.Errorf("second Open: %v, want ErrLocked held by %s", err, holder)
	}
	if got := lockHolder(filepath.Join(t.TempDir(), LockName)); got != "another process" {
		t.Errorf("holder of no lock file %q", got)
	}
	done := make(chan error, 1)
	go func() {
		s2, err := Open(context.Background(), Options{Dir: dir, BootID: "boot-1"})
		if err == nil {
			err = s2.Close()
		}
		done <- err
	}()
	time.Sleep(2 * lockPoll)
	closeStore(t, s)
	if err := <-done; err != nil {
		t.Errorf("Open after Close: %v", err)
	}
}

func TestCommitAfterClose(t *testing.T) {
	s := open(t, t.TempDir(), nil, nil)
	closeStore(t, s)
	if err := s.Commit(context.Background(), Durable, addSteered("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Commit after Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestStoreOwnsTheHeader(t *testing.T) {
	s := open(t, t.TempDir(), nil, nil)
	defer closeStore(t, s)
	before := s.Snapshot()
	err := s.Commit(context.Background(), Durable, func(doc *Document) error {
		doc.BootID, doc.Generation, doc.Schema = "other", 99, 7
		doc.Steered = append(doc.Steered, state.Steered{Container: "x"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := s.Snapshot()
	if doc.BootID != before.BootID || doc.Generation != before.Generation+1 || doc.Schema != Schema {
		t.Errorf("a commit changed the header: %+v", doc)
	}
}

func TestOrigin(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, nil, nil)
	if s.Origin() != New {
		t.Errorf("first start: origin %v", s.Origin())
	}
	if err := s.Commit(context.Background(), Durable, addSteered("a")); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)
	s = open(t, dir, nil, nil)
	if s.Origin() != Loaded {
		t.Errorf("restart: origin %v", s.Origin())
	}
	closeStore(t, s)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, nil, nil)
	if s.Origin() != FromPrev {
		t.Errorf("bad document: origin %v", s.Origin())
	}
	closeStore(t, s)
	for _, name := range []string{FileName, PrevName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s = open(t, dir, nil, func(o *Options) { o.NewState = true })
	if s.Origin() != NewState {
		t.Errorf("--new-state: origin %v", s.Origin())
	}
	closeStore(t, s)
	for _, o := range []Origin{Loaded, FromPrev, New, NewState, Lost} {
		if strings.HasPrefix(o.String(), "origin ") {
			t.Errorf("origin %d has no name", o)
		}
	}
}

// Without the running boot id the store does not open: it cannot tell the
// records of another boot, and it touches no file.
func TestOpenNeedsTheBootID(t *testing.T) {
	dir := t.TempDir()
	if s, err := Open(context.Background(), Options{Dir: dir}); err == nil || !strings.Contains(err.Error(), "boot id") {
		if s != nil {
			closeStore(t, s)
		}
		t.Fatalf("Open without a boot id: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("files %v (%v) after a refused Open", entries, err)
	}
}

// The hook after a commit sees every commit that succeeded, a Durable one
// written, and no failed or refused one.
func TestAfterCommit(t *testing.T) {
	var s *Store
	var seen []string
	s = open(t, t.TempDir(), nil, func(o *Options) {
		o.AfterCommit = func() { seen = append(seen, strings.Join(steered(s.Snapshot()), ",")) }
	})
	if err := s.Commit(context.Background(), Durable, addSteered("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(context.Background(), Durable, func(*Document) error { return errors.New("refused") }); err == nil {
		t.Fatal("a refused commit succeeded")
	}
	if err := s.Commit(context.Background(), Lazy, addSteered("b")); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)
	if err := s.Commit(context.Background(), Durable, addSteered("c")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Commit after Close: %v", err)
	}
	if len(seen) != 2 || seen[0] != "a" {
		t.Fatalf("the hook saw %q, expected the durable commit written and the lazy one", seen)
	}
}

// A fault of a write fails the commits of its group as a failed write of
// the disk does: a Durable commit gets the error, the snapshot and the
// file keep the document before them; the next write works again.
func TestWriteFault(t *testing.T) {
	var fault atomic.Bool
	dir := t.TempDir()
	s := open(t, dir, nil, func(o *Options) {
		o.WriteFault = func() error {
			if fault.Load() {
				return errors.New("disk failure")
			}
			return nil
		}
	})
	defer closeStore(t, s)
	if err := s.Commit(context.Background(), Durable, addSteered("a")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	fault.Store(true)
	if err := s.Commit(context.Background(), Durable, addSteered("b")); err == nil || !strings.Contains(err.Error(), "disk failure") {
		t.Fatalf("a commit with a write fault: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || !bytes.Equal(before, after) || !slices.Equal(steered(s.Snapshot()), []string{"a"}) {
		t.Fatalf("a failed write changed the document: %v, snapshot %v", err, steered(s.Snapshot()))
	}
	fault.Store(false)
	if err := s.Commit(context.Background(), Durable, addSteered("c")); err != nil || !slices.Equal(steered(s.Snapshot()), []string{"a", "c"}) {
		t.Fatalf("a commit after the fault: %v, snapshot %v", err, steered(s.Snapshot()))
	}
}

// TestDropEmptyCoversEverySection sets every section of the records to an
// empty slice: dropEmpty must make each nil, as a decoded document has
// it, or a commit that empties a section writes an unchanged document.
func TestDropEmptyCoversEverySection(t *testing.T) {
	var d Document
	v := reflect.ValueOf(&d.Records).Elem()
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.Slice {
			f.Set(reflect.MakeSlice(f.Type(), 0, 0))
		}
	}
	d.dropEmpty()
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.Slice && !f.IsNil() {
			t.Errorf("section %s empty but not nil after dropEmpty", v.Type().Field(i).Name)
		}
	}
}
