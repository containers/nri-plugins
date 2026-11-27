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

package leases

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// The lease broker serves the lease sockets of lease claims. A request
// from a container creates, in the container's namespace, a ResourceClaim
// and a companion pause pod "cxl-lease-<id>" pinned to this node. The
// scheduler allocates a local memdev (cxl.nri.io) or a pool device to the
// claim, the pool controller attaches a pool device, and kubelet prepares
// the claim through the driver of the device, which lends the memory to
// the requesting container (the hooks of lend.LeaseHooks). Deleting the
// companion releases it again. See docs/memory/cxl/leases.md.

const (
	// DefaultWait is the wait of a request that names none (--lease-wait).
	DefaultWait = 120 * time.Second
	// DefaultCompanionImage is the image of the companion pods
	// (--lease-companion-image).
	DefaultCompanionImage = "registry.k8s.io/pause:3.10"
	// DefaultCLI is the host path of the cxl-request CLI (--lease-cli).
	DefaultCLI = "/usr/local/bin/cxl-request"

	// apiTimeout bounds one Kubernetes API call.
	apiTimeout = 15 * time.Second
	// maxRequestLine bounds a request line.
	maxRequestLine = 64 * 1024
)

var (
	errLendingUnavailable = lend.ErrLendingUnavailable
	errNotOurPod          = errors.New("not a container of the claim's pod")
	errPoolDisabled       = errors.New("pool driver disabled (--pool=false)")
	errTooManyRequests    = errors.New("too many requests (--lease-requests-per-minute)")
)

// Timeouts are the waits and periods of the broker; zero for the
// defaults.
type Timeouts struct {
	// Close bounds how long the Unprepare of a lease claim waits for its
	// leases to be revoked and the deletion of their companions to be
	// requested (not gone: a companion of a local lease is unprepared by
	// cxl.nri.io, whose calls the kubelet plugin helper serializes); 5 s.
	Close time.Duration
	// Poll is the period of the waits for lease changes; 500 ms.
	Poll time.Duration
	// Reconcile is the period of the reconcile; 60 s.
	Reconcile time.Duration
	// CreatingGrace is how long a lease may stay CREATING before the
	// reconcile takes it as left over by a crash; 2 min.
	CreatingGrace time.Duration
	// SyncWait bounds the wait of the first reconcile for the container
	// plugin to synchronize; 30 s.
	SyncWait time.Duration
	// Persist bounds the wait of a commit of the records; 1 min.
	Persist time.Duration
}

// withDefaults returns the timeouts with the defaults for the zero ones.
func (t Timeouts) withDefaults() Timeouts {
	t.Close = cmp.Or(t.Close, 5*time.Second)
	t.Poll = cmp.Or(t.Poll, 500*time.Millisecond)
	t.Reconcile = cmp.Or(t.Reconcile, 60*time.Second)
	t.CreatingGrace = cmp.Or(t.CreatingGrace, 2*time.Minute)
	t.SyncWait = cmp.Or(t.SyncWait, 30*time.Second)
	t.Persist = cmp.Or(t.Persist, time.Minute)
	return t
}

// Options are the settings of a Broker.
type Options struct {
	// Store keeps the lease and lease claim records.
	Store *store.Store
	// Client is the Kubernetes client.
	Client coreclientset.Interface
	// NodeName is the node of the driver.
	NodeName string
	// DataDir is the plugin data directory of cxl.nri.io.
	DataDir string
	// CLI is the host path of cxl-request, Image the image of the
	// companion pods ("": DefaultCompanionImage), WaitDefault the wait of
	// a request that names none (0: DefaultWait).
	CLI         string
	Image       string
	WaitDefault time.Duration
	// PoolDriver is the name of the pool driver, "" when it is disabled.
	PoolDriver string
	// MountPoints returns the mount points of the driver's mount
	// namespace.
	MountPoints func() ([]string, error)
	// OpenDevice opens the device of a dax or block lease, read-only or
	// read-write, for passing it to a container.
	OpenDevice func(ctx context.Context, path string, readOnly bool) (*os.File, error)
	// PublishedDevice returns the driver and the device of a serial in the
	// ResourceSlices: the local memdev of this node, else a device of the
	// pool driver; nil if there is none.
	PublishedDevice func(serial uint64) (driver string, dev *resourcev1.Device)
	// identity resolves the processes that connect to the sockets when
	// Start gets a NRI plugin (without one every request is refused); nil: the
	// container table of that plugin.
	identity Identity
	// Timeouts are the waits and periods.
	Timeouts Timeouts
	// NewID returns a new lease id; nil: 8 random hex digits.
	NewID func() string
	// Limits are the limits of the sockets and of the leases of a pod.
	Limits Limits
}

// Limits bound what the containers of a pod can make the broker hold.
type Limits struct {
	// MaxConnections is the number of open connections of the socket of a
	// lease claim; 0: DefaultMaxConnections.
	MaxConnections int
	// IdleTimeout is how long a connection may stay without a request; 0:
	// DefaultIdle.
	IdleTimeout time.Duration
	// MaxPerPod is the number of leases of a pod; 0:
	// DefaultMaxLeasesPerPod.
	MaxPerPod int
	// LeasesPerNode is the number of leases of the node; 0:
	// DefaultMaxLeasesPerNode.
	LeasesPerNode int
	// RequestsPerMinute is the rate of the lease requests of a lease
	// claim, and their burst; 0: DefaultRequestsPerMinute.
	RequestsPerMinute int
}

const (
	// DefaultMaxConnections is the default limit of the open connections
	// of the socket of a lease claim.
	DefaultMaxConnections = 64
	// DefaultIdle is the default limit of the time a connection may stay
	// without a request.
	DefaultIdle = 10 * time.Minute
	// DefaultMaxLeasesPerPod is the default limit of the leases of a pod.
	DefaultMaxLeasesPerPod = 16
	// DefaultMaxLeasesPerNode is the default limit of the leases of the
	// node. Each lease has a companion pod on the node.
	DefaultMaxLeasesPerNode = 32
	// DefaultRequestsPerMinute is the default rate of the lease requests
	// of a lease claim.
	DefaultRequestsPerMinute = 32
)

// withDefaults returns the limits with the defaults for the zero ones.
func (l Limits) withDefaults() Limits {
	l.MaxConnections = cmp.Or(l.MaxConnections, DefaultMaxConnections)
	l.IdleTimeout = cmp.Or(l.IdleTimeout, DefaultIdle)
	l.MaxPerPod = cmp.Or(l.MaxPerPod, DefaultMaxLeasesPerPod)
	l.LeasesPerNode = cmp.Or(l.LeasesPerNode, DefaultMaxLeasesPerNode)
	l.RequestsPerMinute = cmp.Or(l.RequestsPerMinute, DefaultRequestsPerMinute)
	return l
}

// Broker is the lease broker of the node: the lease claims of the pods and
// their sockets, the leases asked for on them and their companions.
//
// Identity: one per process, with --leases.
//
// States: the records of state.Lease and state.LeaseClaim (lease.go,
// leaseclaim.go) in the store, their only copy. Process memory, not
// records: the NRI plugin and the identity of the processes that
// connect, the sockets and their connections, the running releases, the
// claims whose directories are being removed (CLOSED and REMOVING) and
// the leases failed at the start.
//
// Transitions: state.LeaseTable and state.LeaseClaimTable, applied by
// apply and applyClaim in the commits of the records with their checks.
//
// Invariants: a lease is in releasing from before the commit of the start
// of its release until its release worker ends, so that it is released
// once; a running release is that of a lease with ReleaseRequested; a
// RELEASING or RELEASING_BUSY lease without one is released again by the
// next reconcile.
//
// Persistence: every change is committed durably before the act that
// depends on it; a failed commit means the change did not happen: the
// request gets an error or the unchanged answer, the release worker and
// the reconcile try again. A read (records) is the store's snapshot,
// taken without mu and possibly stale: a change decided on it is checked
// again inside its commit, on the records it changes; a snapshot read
// before a commit only spares the commit when there is nothing to change.
//
// Locks and waits: mu guards the process memory; a leaf lock among the
// driver's locks, held across the file operations of a socket, never
// across a commit, an API call or a call of the NRI plugin. A commit
// function changes only the records: it takes no lock and waits for
// nothing, as it runs inside the store's commit (the DRA driver runs the
// step of PreparedStep inside its own commit). A commit waits at most
// Timeouts.Persist. Waiters poll changed and every Timeouts.Poll.
type Broker struct {
	opts Options
	st   *store.Store
	t    Timeouts

	// mu guards the fields below: a leaf lock.
	mu sync.Mutex
	// containers is the NRI plugin (nil: NRI not connected) and
	// started: Start ran; identity the identity of the processes that
	// connect (nil: requests are refused).
	started    bool
	containers Containers
	identity   Identity
	// listeners are the sockets of the lease claims by uid, conns their
	// open connections.
	listeners map[string]*net.UnixListener
	conns     map[string]map[*net.UnixConn]bool
	// requests are the token buckets of the lease claims (uid) for the
	// requests that make API calls.
	requests map[string]flowcontrol.PassiveRateLimiter
	// warnings count the warnings of the sockets by lease claim (uid) and
	// reason.
	warnings map[string]map[string]*warningCount
	// releasing are the leases whose release runs, or is being committed.
	releasing map[string]bool
	// removing are the lease claims (uid) whose directories are being
	// removed (removeClaimDirs, without mu), with the channel closed when
	// the removal ends: their socket is not started meanwhile.
	removing map[string]chan struct{}
	// unrestored are the leases that failed at the start because the
	// device of their companion was not restored: released by Start.
	unrestored []string
	// changed is closed and replaced on every change.
	changed chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New loads the lease claims and leases from the store, fails the READY
// leases whose companion's device the start did not restore and starts the
// sockets of the lease claims. Requests are answered with "lending
// unavailable" until Start.
func New(opts Options) (*Broker, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("lease broker: the broker needs the state store")
	case opts.Client == nil:
		return nil, errors.New("lease broker: the broker needs a Kubernetes client")
	case opts.MountPoints == nil || opts.OpenDevice == nil || opts.PublishedDevice == nil:
		return nil, errors.New("lease broker: the broker needs the mount points and the devices of the node")
	}
	if opts.WaitDefault <= 0 {
		opts.WaitDefault = DefaultWait
	}
	opts.Image = cmp.Or(opts.Image, DefaultCompanionImage)
	opts.Limits = opts.Limits.withDefaults()
	if opts.NewID == nil {
		opts.NewID = randomLeaseID
	}
	if err := os.MkdirAll(filepath.Join(opts.DataDir, leaseDirName), 0755); err != nil {
		return nil, fmt.Errorf("lease broker: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Broker{
		opts:      opts,
		st:        opts.Store,
		t:         opts.Timeouts.withDefaults(),
		listeners: map[string]*net.UnixListener{},
		conns:     map[string]map[*net.UnixConn]bool{},
		requests:  map[string]flowcontrol.PassiveRateLimiter{},
		warnings:  map[string]map[string]*warningCount{},
		releasing: map[string]bool{},
		removing:  map[string]chan struct{}{},
		changed:   make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}
	if err := b.st.Lost(); err != nil {
		klog.Errorf("lease broker: %v; starting with an empty lease table", err)
	}
	b.unrestored = b.failUnrestored()
	b.recoverClaims()
	b.sweepFsDirs(state.TriggerRecover)
	r := b.records()
	klog.Infof("lease broker: loaded %d lease claims and %d leases from %s", len(r.LeaseClaims), len(r.Leases),
		filepath.Join(opts.DataDir, store.FileName))
	return b, nil
}

// randomLeaseID returns 4 random bytes as 8 hex digits.
func randomLeaseID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// Start sets the NRI plugin (nil: NRI is not connected, requests
// fail), registers its callbacks, releases the leases that failed at the
// start and starts the reconcile loop.
func (b *Broker) Start(containers Containers) {
	b.mu.Lock()
	b.started = true
	b.containers = containers
	if containers != nil {
		b.identity = b.opts.identity
		if b.identity == nil {
			b.identity = tableIdentity{containers: containers}
		}
	}
	unrestored := b.unrestored
	b.unrestored = nil
	b.notify()
	b.mu.Unlock()
	if containers != nil {
		containers.OnContainerGone(b.containerGone)
		containers.OnContainerStarted(b.containerStarted)
	}
	for _, id := range unrestored {
		b.startRelease(id, "companion not restored", state.TriggerRecover)
	}
	b.wg.Add(1)
	go b.reconcileLoop(containers)
}

// Stop closes the sockets, ends their connections and waits until the
// background work stops. A request in progress gets its answer. The
// leases are kept: a restarted plugin continues with them.
func (b *Broker) Stop() {
	if b == nil {
		return
	}
	b.cancel()
	b.mu.Lock()
	for uid, l := range b.listeners {
		_ = l.Close()
		delete(b.listeners, uid)
	}
	// only the read side: a request in progress can still write its answer
	for _, conns := range b.conns {
		for c := range conns {
			_ = c.CloseRead()
		}
	}
	b.mu.Unlock()
	b.wg.Wait()
}

// notify wakes up the waiters of lease changes. Called with b.mu held.
func (b *Broker) notify() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// waitFor polls cond on every lease change and every poll interval until
// it is true or ctx is done.
func (b *Broker) waitFor(ctx context.Context, cond func() bool) error {
	for {
		b.mu.Lock()
		ch := b.changed
		b.mu.Unlock()
		if cond() {
			return nil
		}
		t := time.NewTimer(b.t.Poll)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			if cond() {
				return nil
			}
			return ctx.Err()
		}
		t.Stop()
	}
}

// records returns the records of the store's snapshot. The caller must not
// change them.
func (b *Broker) records() *state.Records {
	return &b.st.Snapshot().Records
}

// refusal is the error of a commit function: the change is refused.
type refusal struct{ error }

// commit commits fn, a change of the records, durably and wakes up the
// waiters. It returns the error of fn as it is, or the error of the commit:
// then the change did not happen. A result that fn leaves in a variable of
// the caller is valid only without an error.
func (b *Broker) commit(fn func(r *state.Records) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.t.Persist)
	defer cancel()
	err := b.st.Commit(ctx, store.Durable, func(doc *store.Document) error {
		if err := fn(&doc.Records); err != nil {
			return refusal{err}
		}
		return nil
	})
	b.mu.Lock()
	b.notify()
	b.mu.Unlock()
	if r := (refusal{}); errors.As(err, &r) {
		return r.error
	}
	if err != nil {
		return fmt.Errorf("lease broker: %w", err)
	}
	return nil
}

// change commits fn, a change of the records that is never refused, and
// reports whether it is committed; a failed commit is logged.
func (b *Broker) change(fn func(r *state.Records)) bool {
	if err := b.commit(func(r *state.Records) error { fn(r); return nil }); err != nil {
		klog.Errorf("%v", err)
		return false
	}
	return true
}

// apply takes the lease to the state to on trigger, if the lease table
// has the transition, and reports whether it did; a refused transition is
// logged.
func apply(l *state.Lease, to state.LeaseState, trigger state.Trigger) bool {
	if err := fsm.Apply(&state.LeaseTable, &l.State, to, trigger); err != nil {
		klog.Errorf("lease broker: lease %s: %v", l.ID, err)
		return false
	}
	return true
}

// applyClaim takes the lease claim to the state to on trigger, if the
// lease claim table has the transition; a refused transition is logged.
func applyClaim(c *state.LeaseClaim, to state.LeaseClaimState, trigger state.Trigger) bool {
	if err := fsm.Apply(&state.LeaseClaimTable, &c.State, to, trigger); err != nil {
		klog.Errorf("lease broker: lease claim %s: %v", c.UID, err)
		return false
	}
	return true
}

// leasesOf returns the leases of the records received on the socket of the
// lease claim.
func leasesOf(r *state.Records, claimUID string) []*state.Lease {
	var out []*state.Lease
	for i := range r.Leases {
		if l := &r.Leases[i]; l.Claim == claimUID {
			out = append(out, l)
		}
	}
	return out
}

// apiContext returns the context of one API call.
func (b *Broker) apiContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(b.ctx, apiTimeout)
}

var (
	_ lend.LeaseHooks      = (*Broker)(nil)
	_ lend.LeaseClaims     = (*Broker)(nil)
	_ lend.PreparedStepper = (*Broker)(nil)
)
