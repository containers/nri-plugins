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

// Package engine owns the CXL kernel objects that the driver makes on the
// node, for local and pool memdevs alike: regions, dax devices and their
// drivers, memory block states, namespaces, block device flags, node
// mounts, loop devices and lease binds. It is the only code of the driver
// that changes them, through pkg/cxl/kernel, and it keeps what it made in
// the records of package state (Memdev, Region with its DaxDevices and
// Mounts, Grant with its Bind) in the store.
//
// A grant is realised in the order journal, kernel work, verify, record:
// its intent is committed durably before the first kernel command, and
// every object it made is committed before the next command that depends on
// it, so that a process stop at any point leaves records that the recovery
// can finish (PrepareGrants, ReleaseGrants, Recover). The release of a
// region follows Policy. The engine serializes its kernel work: one request
// per memdev at a time (a queue per memdev), and one engine lock around each
// short kernel call; waits and zeroing run outside the lock.
//
// The transitions of the records are the tables of package state,
// rendered into docs/memory/cxl/states.md; the invariants of the records and
// the kernel together are in invariants.go.
package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// Engine: the one owner of the CXL kernel objects that the driver makes on
// the node.
//
// Identity: one per process.
//
// States: the records of the store (Memdev, Region, Grant) and, in memory,
// the last kernel observation.
//
// Transitions: the tables of package state, applied in store commits.
//
// Invariants: invariants.go, over the records and a kernel snapshot.
//
// Persistence: the records only; a commit is durable before the next
// kernel command that depends on it.
//
// Locks and waits: a queue per memdev (one request of a memdev at a time),
// and the engine lock around each short kernel call and the durable commit
// of the note that must precede it, never across a wait. Waits (a pool
// memdev to appear, an nd region to settle, a holder to go, memory blocks
// to stay offline) and zeroing run outside the lock. The lock order is queue -> engine lock -> store commit.
type Engine struct {
	k     *kernel.Kernel
	store *store.Store
	opts  Options

	// mu is the engine lock.
	mu sync.Mutex

	// qmu guards queues, last, unknown, unownedBinds, running,
	// started, observed, hollowLogged and probes: a leaf lock.
	qmu    sync.Mutex
	queues map[state.Serial]chan struct{}
	last   *kernel.Snapshot
	// unknown are the latest changes of kernel objects that no row of
	// their tables has, as Observe logged them, until UnknownSeen.
	unknown []string
	// unownedBinds are the lease binds of no memdev (their node mount is
	// gone) that a start could not unmount, unmounted again at every
	// release.
	unownedBinds []string
	// running is the number of requests in progress (Request), started
	// the number of those that started, observed the value of started at
	// the latest observation (last).
	running, started, observed int
	hollowLogged               map[string]bool
	// probes are the results of the address line probes of the local
	// memdevs (Options.ProbesFile).
	probes map[state.Serial]state.Probe

	// afterCommit is called after every commit of the engine that the store
	// applied, nil outside the tests that stop the process at a commit of
	// the engine. The store's AfterCommit sees the commits of the
	// kubelet plugins too.
	afterCommit func()
}

// Options configure an Engine.
type Options struct {
	// Kernel observes and changes the kernel objects.
	Kernel *kernel.Kernel
	// Store keeps the records.
	Store *store.Store
	// MountsDirs are the directories of the node mounts of mode fs of the
	// sources, <plugin data dir of the driver>/mounts.
	MountsDirs map[state.Source]string
	// LeaseFsRoot is the directory of the lease-fs directories of the
	// lease claims, "" for no sweep of lease binds.
	LeaseFsRoot string
	// FSTypes are the filesystem types that mode fs may mount, none when
	// empty.
	FSTypes []string
	// Fence fences a NUMA node before memory of it goes online, nil for
	// no fencing; it returns once the node is fenced or the fence is not
	// ready, or when its context (Timeouts.Fence) ends.
	Fence func(ctx context.Context, numaNode int)
	// LocalGuard returns an error if a local grant on the serial must not
	// be made: the serial is listed by a pool ResourceSlice now (a fresh
	// list; a failed list is an error). Nil: no such check.
	LocalGuard func(ctx context.Context, serial state.Serial) error
	// Shared tells whether a local memdev is shared: configured shared,
	// or used shared by a claim since the configuration was last read.
	// Nil: none is.
	Shared func(serial state.Serial) bool
	// ProbeAddressing runs the address line probe on the new regions of
	// local memdevs; ProbesFile keeps the probes that found aliasing, ""
	// for none.
	ProbeAddressing bool
	ProbesFile      string
	// Revoke revokes the NUMA node that the grant ref of a companion claim
	// lends (lend.NUMANodes), nil for no lends.
	Revoke func(ctx context.Context, ref state.GrantRef, lend state.GrantLend) error
	// Sources are the sources whose records the engine owns, nil for all.
	Sources []state.Source
	// Timeouts are the bounds of the waits; zero values are the defaults.
	Timeouts Timeouts
}

// Timeouts are the bounds of the waits of the engine.
type Timeouts struct {
	// Memdev is how long a pool grant waits for its memdev to appear.
	Memdev time.Duration
	// ProbeGrace is how long a pool grant waits for the driver of a memdev
	// that is present.
	ProbeGrace time.Duration
	// RegionRetry is the pause before the one retry of a region creation
	// whose memdev had no endpoint port yet.
	RegionRetry time.Duration
	// Settle is how long a pool grant waits for the kernel to set up its
	// region: the dax region and the memory blocks of a ram region, the nd
	// region and the namespaces of a pmem region; LocalSettle how long a
	// local grant does.
	Settle, LocalSettle time.Duration
	// Restore is how long the verification of a block device waits for the
	// namespaces of its region.
	Restore time.Duration
	// Holder is how long one release waits for the block devices of its
	// persistent grants to have no holder.
	Holder time.Duration
	// Rollback bounds a rollback and the release of leftovers, which do
	// not end with the context of the request.
	Rollback time.Duration
	// OfflineSettle is how long memory blocks must stay offline before
	// their dax device changes driver, Offline how long the engine
	// offlines them again when they come online.
	OfflineSettle, Offline time.Duration
	// Fence bounds the fence of a NUMA node: the memory goes online
	// anyway (with a warning) when it ends first.
	Fence time.Duration
}

// The defaults of Timeouts.
const (
	defaultMemdevTimeout = 30 * time.Second
	defaultProbeGrace    = 2 * time.Second
	defaultRegionRetry   = 3 * time.Second
	defaultSettle        = 30 * time.Second
	defaultRestore       = 5 * time.Second
	defaultHolderWait    = 10 * time.Second
	defaultRollback      = time.Minute
	defaultOfflineSettle = 500 * time.Millisecond
	defaultOffline       = 30 * time.Second
	defaultFence         = 10 * time.Second
	// holderPoll is the interval of the checks of the holder wait.
	holderPoll = 100 * time.Millisecond
)

// withDefaults returns the timeouts with the defaults for zero values.
func (t Timeouts) withDefaults() Timeouts {
	set := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&t.Memdev, defaultMemdevTimeout)
	set(&t.ProbeGrace, defaultProbeGrace)
	set(&t.RegionRetry, defaultRegionRetry)
	set(&t.Settle, defaultSettle)
	set(&t.LocalSettle, defaultSettle)
	set(&t.Restore, defaultRestore)
	set(&t.Holder, defaultHolderWait)
	set(&t.Rollback, defaultRollback)
	set(&t.OfflineSettle, defaultOfflineSettle)
	set(&t.Offline, defaultOffline)
	set(&t.Fence, defaultFence)
	return t
}

// New returns an engine over the kernel and the store of opts.
func New(opts Options) (*Engine, error) {
	if opts.Kernel == nil || opts.Store == nil {
		return nil, errors.New("engine: a kernel and a store are required")
	}
	opts.Timeouts = opts.Timeouts.withDefaults()
	probes, err := loadProbes(opts.ProbesFile)
	if err != nil {
		return nil, err
	}
	return &Engine{
		k:            opts.Kernel,
		store:        opts.Store,
		opts:         opts,
		queues:       map[state.Serial]chan struct{}{},
		hollowLogged: map[string]bool{},
		probes:       probes,
	}, nil
}

// settle returns how long a grant of the source waits for the kernel to
// set up its region.
func (e *Engine) settle(source state.Source) time.Duration {
	if source == state.SourceLocal {
		return e.opts.Timeouts.LocalSettle
	}
	return e.opts.Timeouts.Settle
}

// GrantRequest is a grant that a kubelet plugin asks the engine to realise: the
// result of a claim on a memdev, with what it asks and the facts of the
// memdev.
type GrantRequest struct {
	Ref    state.GrantRef
	Serial state.Serial
	Asked  state.Asked
	// Facts are the facts of the memdev that the source knows (the pool
	// controller's status data, the local configuration).
	Facts state.MemdevFacts
	// SingleHost: no other host has the device while the claim is
	// allocated. For a pool device: the status data's singleHost or
	// exclusive, or a device that is not shared.
	SingleHost bool
	// BindTarget is the lease bind of the grant of mode fs of a companion
	// claim, <lease claim FSDir>/<name>; "" for none.
	BindTarget string
	// Lend is the lend of a grant of a companion claim, nil for others:
	// the engine binds the lease of mode fs; the NUMA node of mode numa is
	// lent by the kubelet plugin after PrepareGrants returned, recorded before
	// (RecordLend), and revoked by the kubelet plugin before ReleaseGrants, by
	// the engine (Options.Revoke) before the rollback of a later Prepare
	// of the claim releases the grant.
	Lend *state.GrantLend
}

// queue returns the queue of the memdev with the serial.
func (e *Engine) queue(serial state.Serial) chan struct{} {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	q := e.queues[serial]
	if q == nil {
		q = make(chan struct{}, 1)
		e.queues[serial] = q
	}
	return q
}

// acquire waits for the queue of the memdev with the serial and returns
// the function that leaves it; a request whose ctx ended does not start.
func (e *Engine) acquire(ctx context.Context, serial state.Serial) (func(), error) {
	q := e.queue(serial)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("waiting for the requests of memdev %s: %w", serial, err)
	}
	select {
	case q <- struct{}{}:
		return func() { <-q }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the requests of memdev %s: %w", serial, ctx.Err())
	}
}

// tryAcquire takes the queue of the memdev with the serial when it is
// free and returns the function that leaves it, nil when another request
// of the memdev is in progress.
func (e *Engine) tryAcquire(serial state.Serial) func() {
	q := e.queue(serial)
	select {
	case q <- struct{}{}:
		return func() { <-q }
	default:
		return nil
	}
}

// locked runs fn, a short kernel call, holding the engine lock.
func (e *Engine) locked(fn func() error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return fn()
}

// records returns the records of the last written document; the caller
// must not change them.
func (e *Engine) records() *state.Records {
	return &e.store.Snapshot().Records
}

// commit applies fn to the records in one durable commit. The rollback
// timeout, not ctx, bounds its wait in the queue of the store.
func (e *Engine) commit(ctx context.Context, fn func(r *state.Records) error) error {
	return e.commitAs(ctx, store.Durable, fn)
}

// commitAs is commit with the durability d.
func (e *Engine) commitAs(ctx context.Context, d store.Durability, fn func(r *state.Records) error) error {
	cctx, cancel := e.rollbackContext(ctx)
	defer cancel()
	err := e.store.Commit(cctx, d, func(doc *store.Document) error { return fn(&doc.Records) })
	if err == nil && e.afterCommit != nil {
		e.afterCommit()
	}
	return err
}

// rollbackContext returns the context of a rollback or a release of
// leftovers: not ended by ctx, bounded by the rollback timeout.
func (e *Engine) rollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), e.opts.Timeouts.Rollback)
}

// owns tells whether the engine owns the records of the source
// (Options.Sources).
func (e *Engine) owns(source state.Source) bool {
	return e.opts.Sources == nil || slices.Contains(e.opts.Sources, source)
}

// ownsAll tells whether the engine owns the records of every source.
func (e *Engine) ownsAll() bool {
	return e.owns(state.SourcePool) && e.owns(state.SourceLocal)
}

// memdevText names the memdev of a source with the serial in the log
// lines: "pool memdev 0xc1ae0001".
func memdevText(source state.Source, serial state.Serial) string {
	return string(source) + " memdev " + serial.String()
}

// roWord returns the mount option of a read-only or a read-write mount.
func roWord(readOnly bool) string {
	if readOnly {
		return "ro"
	}
	return "rw"
}

// removeRecord removes the records of a section for which drop is true.
func removeRecord[T any](records *[]T, drop func(*T) bool) {
	*records = slices.DeleteFunc(*records, func(r T) bool { return drop(&r) })
}

// removeRef returns a copy of refs without ref.
func removeRef[T comparable](refs []T, ref T) []T {
	return slices.DeleteFunc(slices.Clone(refs), func(r T) bool { return r == ref })
}

// addRef returns refs with ref, once.
func addRef[T comparable](refs []T, ref T) []T {
	if slices.Contains(refs, ref) {
		return refs
	}
	return append(refs, ref)
}

// memdevName returns the name of the memdev with the serial: its record's,
// else the kernel's, else the serial.
func (e *Engine) memdevName(serial state.Serial) string {
	if m := e.records().Memdev(serial); m != nil && m.Name != "" {
		return m.Name
	}
	if name, err := e.k.FindMemdev(uint64(serial)); err == nil {
		return name
	}
	return serial.String()
}

// since returns the time since start in milliseconds for logs ("12.345ms").
func since(start time.Time) string {
	return fmt.Sprintf("%.3fms", float64(time.Since(start))/float64(time.Millisecond))
}

// claimNameOf returns the name of the claim of the grant ref (its
// preparation's), else its uid.
func (e *Engine) claimNameOf(ref state.GrantRef) string {
	if p := e.records().PreparationOf(ref); p != nil && p.ClaimName != "" {
		return p.ClaimName
	}
	return ref.Claim
}

// deviceNameOf returns the DRA device of the grant ref (its preparation's
// result), else the ref.
func (e *Engine) deviceNameOf(ref state.GrantRef) string {
	if p := e.records().PreparationOf(ref); p != nil {
		if res := p.Result(ref.Result); res != nil && res.Device != "" {
			return res.Device
		}
	}
	return ref.String()
}
