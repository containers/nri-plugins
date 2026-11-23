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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
)

// The protocol of a lease socket: SOCK_STREAM, one JSON line each way,
// several requests per connection, a request line of at most
// maxRequestLine bytes with one object of known fields; the response of a
// Ready dax or block lease carries the open fd of its device (SCM_RIGHTS)
// on the write of its line. The process of a connection is the one that
// connected, fixed at connect by its pid, start time and pidfd; the
// identity of every request is that of the process's container then, the
// connection's once the process is gone.

// leaseFd names the device of a dax or block lease whose fd goes with a
// response: none if path is "". The fd goes only while the lease is Ready
// with got.
type leaseFd struct {
	path     string
	readOnly bool
	lease    string
	got      state.LeaseGot
}

// logPeriod is the shortest time between two logged warnings with the same
// lease claim and reason.
const logPeriod = time.Minute

// warningCount is the time of the last logged warning with a lease claim
// and reason, and the number of such warnings not logged since.
type warningCount struct {
	logged  time.Time
	skipped int
}

// accept serves the socket of a lease claim until the socket is closed.
// After an error, for instance EMFILE, it waits and accepts again.
func (b *Broker) accept(claimUID string, l *net.UnixListener) {
	defer b.wg.Done()
	var delay time.Duration
	for {
		c, err := l.AcceptUnix()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			b.warn(claimUID, "accept", err)
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		b.wg.Add(1)
		go b.serveConn(claimUID, c)
	}
}

// serveConn answers the request lines of one connection until it ends,
// its lease claim is closed or the broker stops.
func (b *Broker) serveConn(claimUID string, c *net.UnixConn) {
	defer b.wg.Done()
	if !b.addConn(claimUID, c) {
		_ = c.Close()
		return
	}
	defer b.removeConn(claimUID, c)
	proc, procErr := openProcess(c)
	defer proc.close()
	who, authErr := b.authenticate(claimUID, proc, procErr)
	if authErr != nil {
		b.warn(claimUID, "rejected connection", authErr)
	}
	connected, connectedErr := who, authErr
	r := bufio.NewReaderSize(c, 4096)
	for first := true; ; first = false {
		// a connection without a request for Limits.Idle is closed
		if err := c.SetReadDeadline(time.Now().Add(b.opts.Limits.IdleTimeout)); err != nil {
			return
		}
		line, err := readLine(r)
		if err != nil || b.ctx.Err() != nil {
			return
		}
		if !first {
			who, authErr = b.reauthenticate(claimUID, proc, connected, connectedErr)
		}
		var resp *api.Response
		var fd leaseFd
		req, err := decodeRequest(line)
		switch {
		case err != nil:
			resp = &api.Response{Error: err.Error()}
		case authErr != nil:
			resp = &api.Response{Error: authErr.Error()}
		default:
			resp, fd = b.handle(claimUID, who, req)
		}
		if err := b.respond(c, resp, fd); err != nil {
			klog.Warningf("lease broker: claim %s: response to %s/%s: %v", claimUID, who.Pod.Name, who.Container, err)
			return
		}
	}
}

// addConn records an open connection of the socket of a lease claim, and
// returns false when the claim's socket is closed or has as many open
// connections as Limits.Connections.
func (b *Broker) addConn(claimUID string, c *net.UnixConn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listeners[claimUID] == nil {
		return false
	}
	if n := len(b.conns[claimUID]); n >= b.opts.Limits.MaxConnections {
		b.warnLocked(claimUID, "refusing a connection", fmt.Errorf("%d are open, the limit of --lease-max-connections", n))
		return false
	}
	if b.conns[claimUID] == nil {
		b.conns[claimUID] = map[*net.UnixConn]bool{}
	}
	b.conns[claimUID][c] = true
	return true
}

// removeConn closes a connection of the socket of a lease claim and forgets
// it.
func (b *Broker) removeConn(claimUID string, c *net.UnixConn) {
	_ = c.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns[claimUID], c)
	if len(b.conns[claimUID]) == 0 {
		delete(b.conns, claimUID)
	}
}

// closeConnsLocked closes the open connections of the socket of a lease
// claim. Called with b.mu held.
func (b *Broker) closeConnsLocked(claimUID string) {
	for c := range b.conns[claimUID] {
		_ = c.Close()
	}
}

// warn logs a warning of the socket of a lease claim, like warnLocked.
func (b *Broker) warn(claimUID, reason string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.warnLocked(claimUID, reason, err)
}

// warnLocked logs a warning of the socket of a lease claim, at most one
// for a reason in logPeriod. It counts the other warnings. The next logged
// warning tells their number. A warning of a closed socket is always
// logged. Called with b.mu held.
func (b *Broker) warnLocked(claimUID, reason string, err error) {
	now := time.Now()
	var skipped int
	if b.listeners[claimUID] != nil {
		w := b.warnings[claimUID][reason]
		if w == nil {
			if b.warnings[claimUID] == nil {
				b.warnings[claimUID] = map[string]*warningCount{}
			}
			w = &warningCount{}
			b.warnings[claimUID][reason] = w
		}
		if now.Sub(w.logged) < logPeriod {
			w.skipped++
			return
		}
		skipped = w.skipped
		w.logged, w.skipped = now, 0
	}
	if skipped > 0 {
		klog.Warningf("lease broker: claim %s: %s: %v (%d more not logged)", claimUID, reason, err, skipped)
		return
	}
	klog.Warningf("lease broker: claim %s: %s: %v", claimUID, reason, err)
}

// decodeRequest returns the request of a line. It refuses unknown fields
// and data after the object.
func decodeRequest(line []byte) (*api.Request, error) {
	req := &api.Request{}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		return nil, fmt.Errorf("invalid request: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid request: data after the object")
	}
	return req, nil
}

// readLine reads one request line of at most maxRequestLine bytes.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxRequestLine {
			return nil, errors.New("request line too long")
		}
		if !isPrefix {
			return line, nil
		}
	}
}

// respond writes a response line, with the open device of fd as
// SCM_RIGHTS if it names one. A device that does not open is answered with
// the error. So is a device whose lease is not Ready with it after the
// open.
func (b *Broker) respond(c *net.UnixConn, resp *api.Response, fd leaseFd) error {
	if fd.path != "" {
		f, err := b.opts.OpenDevice(b.ctx, fd.path, fd.readOnly)
		if err == nil && !b.readyWith(fd) {
			// a released device: another one may have its name now
			_ = f.Close()
			resp = infoOf(b.records(), fd.lease)
			err = fmt.Errorf("lease %s is %s now", fd.lease, resp.State)
		}
		if err != nil {
			resp = &api.Response{Lease: resp.Lease, State: resp.State, Error: fmt.Sprintf("opening %s: %v", fd.path, err)}
		} else {
			defer func() { _ = f.Close() }()
			data, err := json.Marshal(resp)
			if err != nil {
				return err
			}
			data = append(data, '\n')
			n, oobn, err := c.WriteMsgUnix(data, syscall.UnixRights(int(f.Fd())), nil)
			if err != nil {
				return err
			}
			if oobn == 0 {
				return errors.New("fd not sent")
			}
			if n < len(data) {
				_, err = c.Write(data[n:])
			}
			return err
		}
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = c.Write(append(data, '\n'))
	return err
}

// readyWith tells whether the lease of fd is Ready with the device of fd.
func (b *Broker) readyWith(fd leaseFd) bool {
	l := b.records().Lease(fd.lease)
	return l != nil && l.State == state.LeaseReady && l.Got != nil && *l.Got == fd.got
}

// waitIdentity waits, at most Timeouts.SyncWait, until the broker is
// started and the NRI plugin has synchronized with the runtime: the
// identity of a process is known only then.
func (b *Broker) waitIdentity() {
	ctx, cancel := context.WithTimeout(b.ctx, b.t.SyncWait)
	defer cancel()
	_ = b.waitFor(ctx, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.started
	})
	b.mu.Lock()
	containers := b.containers
	b.mu.Unlock()
	if containers != nil {
		_ = containers.WaitSynchronized(ctx)
	}
}

// authenticate maps the process that connected to a container of a pod of
// the lease claim, after the identity of processes is known
// (waitIdentity); procErr is the error of reading the credentials of the
// process.
func (b *Broker) authenticate(claimUID string, proc *connProcess, procErr error) (Requester, error) {
	b.waitIdentity()
	b.mu.Lock()
	identity := b.identity
	b.mu.Unlock()
	lc := b.records().LeaseClaim(claimUID)
	if identity == nil {
		return Requester{}, errLendingUnavailable
	}
	if lc == nil {
		return Requester{}, errNotOurPod
	}
	if procErr != nil {
		return Requester{}, fmt.Errorf("credentials of the process: %w", procErr)
	}
	pid := proc.pid
	who, err := identity.Resolve(pid)
	switch {
	case errors.Is(err, errUnknownProcess):
		return Requester{}, &processGoneError{fmt.Errorf("%w (pid %d is in no known container)", errNotOurPod, pid)}
	case err != nil:
		return Requester{}, fmt.Errorf("%w (pid %d: %v)", errNotOurPod, pid, err)
	}
	if !hasPod(lc, who.Pod.UID) {
		if !b.allowRequest(claimUID) {
			return Requester{}, errTooManyRequests
		}
		if !b.refreshClaimPods(lc, who.Pod.UID) {
			return Requester{}, fmt.Errorf("%w (pid %d is in %s/%s/%s)", errNotOurPod, pid, who.Pod.Namespace, who.Pod.Name, who.Container)
		}
	}
	return who, nil
}

// allowRequest takes a token from the bucket of a lease claim and reports
// whether it had one; true for a claim without a listener. The bucket holds
// Limits.RequestsPerMinute tokens and gets as many per minute.
func (b *Broker) allowRequest(claimUID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	bucket := b.requests[claimUID]
	if bucket == nil {
		if b.listeners[claimUID] == nil {
			return true // closed: the request is refused later
		}
		n := b.opts.Limits.RequestsPerMinute
		bucket = flowcontrol.NewTokenBucketPassiveRateLimiter(float32(n)/60, n)
		b.requests[claimUID] = bucket
	}
	return bucket.TryAccept()
}

// reauthenticate returns the identity of a later request of a connection:
// that of its process now while the process that connected runs, else
// the identity of the connection (connected, connectedErr). A process in
// no known container keeps an identity that the connection had.
func (b *Broker) reauthenticate(claimUID string, proc *connProcess, connected Requester, connectedErr error) (Requester, error) {
	if proc == nil || !proc.running() {
		return connected, connectedErr
	}
	who, err := b.authenticate(claimUID, proc, nil)
	// the pid may have gone to another process while it was resolved
	if !proc.running() {
		return connected, connectedErr
	}
	if pe := (*processGoneError)(nil); connectedErr == nil && errors.As(err, &pe) {
		return connected, nil
	}
	return who, err
}

// processGoneError is the refusal of a process that is in no known
// container: it has exited, or it is not in a container of the node.
type processGoneError struct{ err error }

func (e *processGoneError) Error() string { return e.err.Error() }
func (e *processGoneError) Unwrap() error { return e.err }

// refreshClaimPods reads the reservedFor of a lease claim again (a claim
// shared by several pods is prepared only once), commits it, and reports
// whether the pod is in it now.
func (b *Broker) refreshClaimPods(lc *state.LeaseClaim, podUID string) bool {
	ctx, cancel := b.apiContext()
	defer cancel()
	claim, err := b.opts.Client.ResourceV1().ResourceClaims(lc.Namespace).Get(ctx, lc.Name, metav1.GetOptions{})
	if err != nil || string(claim.UID) != lc.UID {
		return false
	}
	pods := reservedPods(claim)
	found := false
	return b.change(func(r *state.Records) {
		if cur := r.LeaseClaim(lc.UID); cur != nil {
			cur.Pods = pods
			if cur.State == state.LeaseClaimOpen {
				applyClaim(cur, state.LeaseClaimOpen, state.TriggerRequest)
			}
			found = hasPod(cur, podUID)
		}
	}) && found
}

// handle runs one authenticated request. It returns the response and the
// device whose fd goes with it.
func (b *Broker) handle(claimUID string, who Requester, req *api.Request) (*api.Response, leaseFd) {
	if req.Op == api.OpRequest && !b.allowRequest(claimUID) {
		return &api.Response{Error: errTooManyRequests.Error()}, leaseFd{}
	}
	if err := req.Validate(); err != nil {
		return &api.Response{Lease: req.Lease, Error: err.Error()}, leaseFd{}
	}
	wait := b.parseWait(req.Wait)
	switch req.Op {
	case api.OpRequest:
		return b.requestLease(claimUID, who, req, wait)
	case api.OpRelease:
		if req.All {
			return b.releaseAll(claimUID, who, wait), leaseFd{}
		}
		return b.releaseOne(claimUID, who, req.Lease, wait), leaseFd{}
	case api.OpList:
		return b.list(claimUID, who), leaseFd{}
	case api.OpFd:
		return b.waitLease(claimUID, who, req.Lease, wait, true)
	default: // api.OpEnv
		return b.waitLease(claimUID, who, req.Lease, wait, false)
	}
}

// parseWait returns the duration of a validated wait, the default for "".
func (b *Broker) parseWait(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return b.opts.WaitDefault
}
