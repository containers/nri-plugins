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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
)

const (
	// FileName is the document in the plugin data directory of cxl.nri.io.
	FileName = "state.json"
	// PrevName is the document before the last commit.
	PrevName = "state.json.prev"
	// LockName is the file that the store locks against a second process.
	LockName = "state.lock"
	// Schema is the schema of the document; a document of another schema
	// does not decode.
	Schema = 1

	// lockPoll is how often Open tries to lock the document again.
	lockPoll = 100 * time.Millisecond
)

var (
	// ErrLost is the error of every commit of a store in LOST mode.
	ErrLost = errors.New("the driver state is lost")
	// ErrLocked is the error of an Open whose context ended while another
	// process held the document.
	ErrLocked = errors.New("the driver state is locked by another process")
	// ErrClosed is the error of a commit to a closed store.
	ErrClosed = errors.New("the driver state store is closed")
)

// Document is the one persistent document of the driver: every record it
// remembers. The store owns Schema, Generation and BootID; commits change
// the records.
type Document struct {
	Schema int `json:"schema"`
	// Generation counts the writes of the document.
	Generation uint64 `json:"generation"`
	// BootID is the boot of the records.
	BootID string `json:"bootID,omitempty"`
	state.Records
}

// dropEmpty sets the empty sections of the document to nil, as a decoded
// document has them.
func (d *Document) dropEmpty() {
	r := &d.Records
	if len(r.Memdevs) == 0 {
		r.Memdevs = nil
	}
	if len(r.Regions) == 0 {
		r.Regions = nil
	}
	if len(r.Grants) == 0 {
		r.Grants = nil
	}
	if len(r.Preparations) == 0 {
		r.Preparations = nil
	}
	if len(r.LeaseClaims) == 0 {
		r.LeaseClaims = nil
	}
	if len(r.Leases) == 0 {
		r.Leases = nil
	}
	if len(r.Provenance) == 0 {
		r.Provenance = nil
	}
	if len(r.Steered) == 0 {
		r.Steered = nil
	}
	if len(r.ListedPools) == 0 {
		r.ListedPools = nil
	}
}

// Clone returns a deep copy of the document.
func (d *Document) Clone() *Document {
	return &Document{Schema: d.Schema, Generation: d.Generation, BootID: d.BootID, Records: d.Records.Clone()}
}

// Durability is how long a commit waits for the disk.
type Durability int

const (
	// Durable commits return once the document with the change is on
	// disk: before any externally visible act that derives from it.
	Durable Durability = iota
	// Lazy commits return once the change is applied; it is written with
	// its group and lost if that write fails. Snapshot shows the change
	// once its group is written. Only a change that a later pass makes
	// again may be lazy.
	Lazy
)

// Origin is where the document of a store comes from.
type Origin int

const (
	// Loaded is a document read from state.json.
	Loaded Origin = iota
	// FromPrev is a document read from state.json.prev: state.json was
	// missing or did not decode.
	FromPrev
	// New is a new document of a first start: there was neither file.
	New
	// NewState is a new document made by --new-state when neither file
	// decoded.
	NewState
	// Lost is a store in LOST mode.
	Lost
)

// String returns the origin in words.
func (o Origin) String() string {
	switch o {
	case Loaded:
		return "loaded"
	case FromPrev:
		return "from " + PrevName
	case New:
		return "new"
	case NewState:
		return "new state"
	case Lost:
		return "lost"
	}
	return "origin " + strconv.Itoa(int(o))
}

// Func is a commit: a pure function that changes the records of the
// document, or returns an error and leaves nothing of its changes in the
// store.
type Func func(doc *Document) error

// LostError is the state of a store whose document and previous document
// do not decode: the records of the node are unknown.
type LostError struct {
	State error
	Prev  error
}

// Error returns why the state is lost and what makes a new one.
func (e *LostError) Error() string {
	return fmt.Sprintf("%v: %s: %v; %s: %v; start the driver with --new-state to create a new one",
		ErrLost, FileName, e.State, PrevName, e.Prev)
}

// Is tells whether target is ErrLost.
func (e *LostError) Is(target error) bool {
	return target == ErrLost
}

// Options are the settings of a store.
type Options struct {
	// Dir is the plugin data directory of cxl.nri.io: the document and its
	// lock.
	Dir string
	// BootID is the running boot id. It is required: without it a
	// document of another boot would be taken as of this boot.
	BootID string
	// NewState makes a new document when neither the document nor the
	// previous one decodes.
	NewState bool
	// AfterCommit is called after every commit that succeeded, a Durable
	// one once it is written, in the goroutine of Commit; nil for none.
	// Only tests set it. It must not commit or call the engine: the caller
	// may hold the engine's locks.
	AfterCommit func()
	// WriteFault is called before every write of the document, in the
	// store's writer; an error fails the write as the disk would. nil for
	// none. Only tests set it.
	WriteFault func() error

	files fileOps
}

// Store keeps the document of the records of the node, state.json in the
// plugin data directory: readers get an immutable snapshot, commits are
// applied by one writer goroutine in groups, each group written with one
// fsync'd rename.
//
// Identity: one per process, over its directory, locked with flock so that
// a second process cannot open it.
//
// States: OPEN (Origin Loaded, FromPrev, New or NewState): commits are
// applied and written. LOST (Origin Lost, the LostError in lost): neither
// state.json nor state.json.prev decodes; the snapshot is an empty
// document, every commit fails with the LostError and nothing is written.
// CLOSED (closed): every commit fails with ErrClosed.
//
// Transitions: Open makes an OPEN or a LOST store; Close makes either
// CLOSED after the pending commits are written; nothing else changes the
// state.
//
// Invariants: the snapshot is never ahead of the disk for a Durable commit:
// a failed write leaves it as it was; Generation grows by one per write.
//
// Persistence: state.json and the previous document in state.json.prev.
//
// Locks and waits: mu guards the document pointer, the pending commits
// and closed, and is never held across a write; the writer goroutine
// applies the pure commit functions. A commit that the writer has not
// taken is withdrawn when its context ends. A taken commit waits for its
// outcome.
type Store struct {
	opts   Options
	files  fileOps
	lock   *os.File
	lost   *LostError
	origin Origin

	mu      sync.Mutex
	doc     *Document
	pending []*request
	closed  bool
	wake    chan struct{}
	done    chan struct{}
	// writes counts the writes of the document.
	writes atomic.Int64
}

// request is one pending commit.
type request struct {
	fn      Func
	applied chan error
	written chan error
	ok      bool
}

// Open locks and loads the document in opts.Dir and starts its writer.
// A document of another boot is changed as a reboot requires. A store whose document and previous document do not decode is
// returned in LOST mode (Lost) unless opts.NewState. Open waits for a lock
// held by another process until ctx ends and then returns ErrLocked: the
// caller bounds ctx and exits on the error. Open fails without the running
// boot id.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.BootID == "" {
		return nil, errors.New("store: the boot id of the running kernel is unknown")
	}
	if opts.files == nil {
		opts.files = osFiles{}
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	lock, err := lockFile(ctx, filepath.Join(opts.Dir, LockName))
	if err != nil {
		return nil, err
	}
	s := &Store{
		opts:  opts,
		files: opts.files,
		lock:  lock,
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	doc, write, err := s.load()
	if err != nil {
		return nil, errors.Join(err, s.unlock())
	}
	if s.lost != nil {
		klog.Errorf("store: %v", s.lost)
		s.doc = &Document{Schema: Schema, BootID: opts.BootID}
		close(s.done)
		return s, nil
	}
	changed, err := applyBoot(doc, opts.BootID)
	if err != nil {
		return nil, errors.Join(err, s.unlock())
	}
	if changed || write {
		doc.Sort()
		doc.Generation++
		if err := s.write(doc); err != nil {
			return nil, errors.Join(err, s.unlock())
		}
	}
	s.doc = doc
	go s.writer()
	return s, nil
}

// load returns the document of the directory and whether it must be
// written before it is used: a new document, one read from the previous
// file, or one of a new state. It sets s.lost when neither file decodes.
func (s *Store) load() (*Document, bool, error) {
	path := filepath.Join(s.opts.Dir, FileName)
	prev := filepath.Join(s.opts.Dir, PrevName)
	doc, stateErr := readDocument(path)
	if stateErr == nil {
		s.origin = Loaded
		return doc, false, nil
	}
	stateMissing := errors.Is(stateErr, os.ErrNotExist)
	pdoc, prevErr := readDocument(prev)
	prevMissing := errors.Is(prevErr, os.ErrNotExist)
	switch {
	case stateMissing && prevMissing:
		klog.Infof("store: no %s: starting with a new document", path)
		s.origin = New
		return &Document{Schema: Schema, BootID: s.opts.BootID}, true, nil
	case prevErr == nil:
		klog.Errorf("store: %s: %v; using %s", path, stateErr, prev)
		if !stateMissing {
			// the next commit must not keep the bad file as the previous one
			if err := s.files.Rename(path, path+".bad"); err != nil {
				return nil, false, err
			}
		}
		s.origin = FromPrev
		return pdoc, true, nil
	case s.opts.NewState:
		klog.Warningf("store: %s: %v; %s: %v; --new-state: starting with a new document", path, stateErr, prev, prevErr)
		for _, p := range []string{path, prev} {
			if err := s.files.Rename(p, p+".lost"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, false, err
			}
		}
		s.origin = NewState
		return &Document{Schema: Schema, BootID: s.opts.BootID}, true, nil
	}
	s.lost = &LostError{State: stateErr, Prev: prevErr}
	s.origin = Lost
	return nil, false, nil
}

// readDocument returns the document in the file path, decoded strictly.
func readDocument(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeDocument(data)
}

// decodeDocument returns the document in data, decoded strictly: another
// schema (checked first), unknown fields, data after the document and
// states that a stored record may not have are errors.
func decodeDocument(data []byte) (*Document, error) {
	var head struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, err
	}
	if head.Schema != Schema {
		return nil, fmt.Errorf("unsupported schema %d (this driver reads schema %d)", head.Schema, Schema)
	}
	doc := &Document{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(doc); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the document")
	}
	if err := state.CheckStates(&doc.Records); err != nil {
		return nil, err
	}
	return doc, nil
}

// Snapshot returns the last written document. The caller must not change
// it. The snapshot of a LOST store is an empty document: its records are
// unknown, not absent, so a missing record means nothing there.
func (s *Store) Snapshot() *Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc
}

// Origin returns where the document of the store comes from.
func (s *Store) Origin() Origin {
	return s.origin
}

// Lost returns the *LostError of a store in LOST mode, nil for a store
// that is not lost.
func (s *Store) Lost() error {
	if s.lost == nil {
		return nil
	}
	return s.lost
}

// Commit applies fn to the document. A Durable commit returns nil once
// the document with the change is on disk and is the snapshot. A Lazy
// commit returns nil once fn is applied to the group that the writer
// writes next; the snapshot shows the change after that write, and a
// failed write loses it. An error means that the store has not applied the
// change and never will: the error of fn as it is, a *LostError,
// ErrClosed, the error of the write of the group, or ctx.Err() when ctx
// ended before the writer took the commit. ctx bounds only that wait in the
// queue: Commit never returns while fn may still run. One limit: after a
// failed sync of the directory, state.json may hold the failed change
// until the next write.
func (s *Store) Commit(ctx context.Context, d Durability, fn Func) error {
	err := s.commit(ctx, d, fn)
	if err == nil && s.opts.AfterCommit != nil {
		s.opts.AfterCommit()
	}
	return err
}

// commit applies fn to the document, as Commit.
func (s *Store) commit(ctx context.Context, d Durability, fn Func) error {
	if s.lost != nil {
		return s.lost
	}
	req := &request{fn: fn, applied: make(chan error, 1), written: make(chan error, 1)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.pending = append(s.pending, req)
	s.mu.Unlock()
	s.signal()
	var err error
	select {
	case err = <-req.applied:
	case <-ctx.Done():
		if s.withdraw(req) {
			return ctx.Err()
		}
		err = <-req.applied
	}
	if err != nil || d == Lazy {
		return err
	}
	return <-req.written
}

// withdraw removes req from the pending commits and reports whether it was
// there: false once the writer has taken it.
func (s *Store) withdraw(req *request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.Index(s.pending, req)
	if i < 0 {
		return false
	}
	s.pending = slices.Delete(s.pending, i, i+1)
	return true
}

// Close writes the pending commits, stops the writer and unlocks the
// document.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.signal()
	<-s.done
	return s.unlock()
}

// signal wakes the writer.
func (s *Store) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// writer applies the pending commits in groups until the store is closed.
func (s *Store) writer() {
	defer close(s.done)
	for range s.wake {
		for {
			s.mu.Lock()
			group, cur, closed := s.pending, s.doc, s.closed
			s.pending = nil
			s.mu.Unlock()
			if len(group) == 0 {
				if closed {
					return
				}
				break
			}
			s.commitGroup(cur, group)
		}
	}
}

// commitGroup applies the functions of a group in order to a copy of cur,
// writes the result once and makes it the snapshot.
func (s *Store) commitGroup(cur *Document, group []*request) {
	work := cur
	ok := 0
	for _, req := range group {
		next := work.Clone()
		if err := req.fn(next); err != nil {
			req.applied <- err
			continue
		}
		next.Schema, next.Generation, next.BootID = cur.Schema, cur.Generation, cur.BootID
		work = next
		req.ok = true
		ok++
		req.applied <- nil
	}
	if ok == 0 {
		return
	}
	var err error
	work.Sort()
	work.dropEmpty()
	if !reflect.DeepEqual(work.Records, cur.Records) {
		work.Generation = cur.Generation + 1
		if err = s.write(work); err == nil {
			s.mu.Lock()
			s.doc = work
			s.mu.Unlock()
		} else {
			klog.Errorf("store: %v; the commits of the group are discarded", err)
		}
	}
	for _, req := range group {
		if req.ok {
			req.written <- err
		}
	}
}

// write writes the document: a synced temporary file, the current file
// linked as the previous one, the rename and the sync of the directory.
func (s *Store) write(doc *Document) error {
	if s.opts.WriteFault != nil {
		if err := s.opts.WriteFault(); err != nil {
			return fmt.Errorf("writing %s: %w", FileName, err)
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", FileName, err)
	}
	path := filepath.Join(s.opts.Dir, FileName)
	tmp := path + ".tmp"
	if err := s.files.WriteFile(tmp, data, true); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if _, err := os.Lstat(path); err == nil {
		prev := filepath.Join(s.opts.Dir, PrevName)
		prevTmp := prev + ".tmp"
		if err := s.files.Remove(prevTmp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", prevTmp, err)
		}
		if err := s.files.Link(path, prevTmp); err != nil {
			return fmt.Errorf("linking %s to %s: %w", path, prevTmp, err)
		}
		if err := s.files.Rename(prevTmp, prev); err != nil {
			return fmt.Errorf("renaming %s: %w", prevTmp, err)
		}
	}
	if err := s.files.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming %s: %w", tmp, err)
	}
	if err := s.files.SyncDir(s.opts.Dir); err != nil {
		return fmt.Errorf("syncing the directory of %s: %w", path, err)
	}
	s.writes.Add(1)
	return nil
}

// lockFile returns the lock file path, locked exclusively; it waits for a
// lock of another process until ctx ends.
func lockFile(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for waited := false; ; waited = true {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, writeHolder(f)
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("locking %s: %w", path, err), f.Close())
		}
		if !waited {
			klog.Warningf("store: waiting for the lock of %s held by %s", path, lockHolder(path))
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("%w: %s held by %s", ErrLocked, path, lockHolder(path)), f.Close())
		case <-time.After(lockPoll):
		}
	}
}

// writeHolder writes the holder of the lock, this process, into the lock
// file f: "pid <pid> host <host name>".
func writeHolder(f *os.File) error {
	host, _ := os.Hostname()
	holder := fmt.Sprintf("pid %d host %s\n", os.Getpid(), host)
	if err := f.Truncate(0); err != nil {
		return errors.Join(fmt.Errorf("writing the holder of %s: %w", f.Name(), err), f.Close())
	}
	if _, err := f.WriteAt([]byte(holder), 0); err != nil {
		return errors.Join(fmt.Errorf("writing the holder of %s: %w", f.Name(), err), f.Close())
	}
	return nil
}

// lockHolder returns the holder that the lock file path names, "another
// process" if it names none.
func lockHolder(path string) string {
	data, err := os.ReadFile(path)
	if holder := strings.TrimSpace(string(data)); err == nil && strings.HasPrefix(holder, "pid ") {
		return holder
	}
	return "another process"
}

// unlock releases the lock of the document.
func (s *Store) unlock() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}
