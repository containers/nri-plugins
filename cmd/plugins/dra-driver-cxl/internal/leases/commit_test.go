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
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// The records of the store are the only copy of the leases and the lease
// claims: a change is committed before the act that depends on it, and a
// failed commit means that the change did not happen.

// errDisk is the error of a failed write of the store.
var errDisk = errors.New("disk failure")

// failWrites makes the writes of the store's document fail as the disk
// would, or succeed again.
func (e *leaseEnv) failWrites(fail bool) {
	e.failing.Store(fail)
}

// writeFault is the store's WriteFault hook: errDisk while failing is set.
func (e *leaseEnv) writeFault() error {
	if e.failing.Load() {
		return errDisk
	}
	return nil
}

// companionExists tells whether the companion claim and pod of the lease
// id exist.
func (e *leaseEnv) companionExists(id string) bool {
	ctx := context.Background()
	_, cerr := e.client.ResourceV1().ResourceClaims(envNS).Get(ctx, CompanionName(id), metav1.GetOptions{})
	_, perr := e.client.CoreV1().Pods(envNS).Get(ctx, CompanionName(id), metav1.GetOptions{})
	return cerr == nil && perr == nil
}

// pendingLease requests a lease that waits up to wait, returns its id and
// the channel of its response once the lease is PENDING.
func (e *leaseEnv) pendingLease(wait string) (string, *resourcev1.ResourceClaim, chan api.Response) {
	e.t.Helper()
	n := e.numLeases()
	ch := e.async(api.Request{Op: api.OpRequest, Size: "256Mi", Wait: wait})
	for i := 0; i < 1000 && e.numLeases() == n; i++ {
		e.sleep()
	}
	r := e.st.Snapshot()
	id := r.Leases[len(r.Leases)-1].ID
	claim := e.waitClaim(CompanionName(id))
	e.waitStored(id, func(l *state.Lease) bool { return l.State == state.LeasePending })
	return id, claim, ch
}

// noResponse fails the test when the request of ch is answered within a
// short while.
func noResponse(t *testing.T, ch chan api.Response, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s: answered %+v", what, r)
	case <-time.After(200 * time.Millisecond):
	}
}

// A request whose CREATING commit fails is refused with the error of the
// store: no companion is created and no lease is left.
func TestFailedCommitRefusesRequest(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.start()
	e.failWrites(true)
	resp, _ := e.call(api.Request{Op: api.OpRequest, Size: "256Mi", Wait: "0s"})
	if !strings.HasPrefix(resp.Error, "lease broker: ") || resp.Lease != "" {
		t.Fatalf("request with failing writes %+v", resp)
	}
	claims, err := e.client.ResourceV1().ResourceClaims(envNS).List(context.Background(), metav1.ListOptions{})
	mustOK(t, err)
	if len(claims.Items) != 1 || e.numLeases() != 0 {
		t.Fatalf("%d claims and %d leases of a refused request", len(claims.Items), e.numLeases())
	}
	e.failWrites(false)
	if resp, _ := e.call(api.Request{Op: api.OpRequest, Size: "256Mi", Wait: "0s"}); resp.Error != "" || resp.State != api.StatePending {
		t.Fatalf("request after the writes work again %+v", resp)
	}
}

// A release whose RELEASING commit fails does not revoke, delete the
// companion or close the lease claim: the release and the Unprepare of the
// lease claim get the unchanged lease or an error, and work once the
// writes do.
func TestFailedCommitNoRelease(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.start()
	id := e.readyNUMA()
	e.failWrites(true)
	if rel, _ := e.call(api.Request{Op: api.OpRelease, Lease: id, Wait: "100ms"}); rel.State != api.StateReady || rel.Error != "" {
		t.Fatalf("release with failing writes %+v", rel)
	}
	wantErrText(t, e.b.CloseClaim(context.Background(), envLeaseClaim), "lease broker: ")
	if l := e.stored(id); l == nil || l.State != state.LeaseReady || l.ReleaseRequested || l.DeletionRequested {
		t.Fatalf("lease after the failed releases %+v", l)
	}
	if c := e.claim(envLeaseClaim); c == nil || c.State != state.LeaseClaimOpen {
		t.Fatalf("lease claim after a failed close %+v", c)
	}
	if calls := e.ctrs.callLog(); strings.Contains(calls, "revoke") || !e.companionExists(id) {
		t.Fatalf("revoked or deleted without a commit; calls:\n%s", calls)
	}
	if list, _ := e.call(api.Request{Op: api.OpList}); list.Error != "" || list.Leases == nil || len(*list.Leases) != 1 {
		t.Fatalf("list %+v", list)
	}
	e.failWrites(false)
	if rel, _ := e.call(api.Request{Op: api.OpRelease, Lease: id}); rel.State != api.StateReleased {
		t.Fatalf("release %+v", rel)
	}
	if calls := e.ctrs.callLog(); !strings.Contains(calls, "revoke pod-uid-1/app 2") {
		t.Fatalf("not revoked; calls:\n%s", calls)
	}
	e.wantGone(CompanionName(id))

	// a request whose wait ends while its release cannot be committed is
	// not answered Failed: its lease stays as it is
	id, _, ch := e.pendingLease("300ms")
	e.failWrites(true)
	resp := <-ch
	if resp.State != api.StatePending || !strings.HasPrefix(resp.Error, "timeout: not Ready after 300ms; not released: lease broker: ") {
		t.Fatalf("timeout of a request whose release failed %+v", resp)
	}
	if l := e.stored(id); l == nil || l.State != state.LeasePending || l.ReleaseRequested {
		t.Fatalf("lease after a failed release %+v", l)
	}
	e.failWrites(false)
}

// The companion gate refuses a Prepare whose LENDING commit fails, and a
// lease whose READY commit fails is not answered Ready: its request gets
// the response once a commit of the Prepare again succeeds.
func TestFailedCommitNoReady(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.start()
	id, claim, ch := e.pendingLease("5s")
	e.failWrites(true)
	if _, err := e.b.OnLending(id, claim); err == nil || !strings.HasPrefix(err.Error(), "lease broker: ") {
		t.Fatalf("lending with failing writes: %v", err)
	}
	if l := e.stored(id); l.State != state.LeasePending {
		t.Fatalf("lease after a failed lending %+v", l)
	}
	e.failWrites(false)
	_, err := e.b.OnLending(id, claim)
	mustOK(t, err)
	e.failWrites(true)
	e.b.OnPrepared(id, state.SourcePool, []state.DeviceInfo{numaDevice})
	if l := e.stored(id); l.State != state.LeaseLending || l.Got != nil || l.Lend != nil {
		t.Fatalf("lease after a failed READY %+v", l)
	}
	noResponse(t, ch, "a lease whose READY failed")
	// a Prepare again of the companion announces it again
	e.failWrites(false)
	_, err = e.b.OnLending(id, claim)
	mustOK(t, err)
	e.b.OnPrepared(id, state.SourcePool, []state.DeviceInfo{numaDevice})
	if resp := <-ch; resp.State != api.StateReady || resp.NUMANode == nil || *resp.NUMANode != 2 {
		t.Fatalf("response %+v", resp)
	}
}

// A lease whose FAILED commit fails (a failed lend, its companion
// unprepared behind the broker, a device of another mode) is not released.
func TestFailedCommitNoFailure(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.start()
	id, claim, ch := e.pendingLease("5s")
	_, err := e.b.OnLending(id, claim)
	mustOK(t, err)
	e.failWrites(true)
	e.b.OnLendFailed(id, errors.New("boom"))
	e.b.OnReleasing(id)
	e.b.OnPrepared(id, state.SourcePool, []state.DeviceInfo{preparedDev(serialPoolP, pmemBytes, api.ModeFS, -1)})
	if l := e.stored(id); l.State != state.LeaseLending || l.Error != "" || l.ReleaseRequested {
		t.Fatalf("lease after failed commits %+v", l)
	}
	noResponse(t, ch, "a lease whose FAILED commits failed")
	if !e.companionExists(id) {
		t.Fatal("companion deleted without a commit")
	}
	e.failWrites(false)
	e.b.OnLendFailed(id, errors.New("boom"))
	if resp := <-ch; resp.State != api.StateFailed || resp.Error != "boom" {
		t.Fatalf("response %+v", resp)
	}
	e.waitState(id, state.LeaseNone)
	e.wantGone(CompanionName(id))
}

// A lease claim whose OPEN commit fails has no socket and no record; one
// whose removal fails keeps its socket and its record, and is closed by the
// Unprepare again.
func TestFailedCommitOpenAndClose(t *testing.T) {
	e := brokerEnv(t)
	e.failWrites(true)
	wantErrText(t, e.b.OpenClaim(leaseClaimObj()), "lease broker: ")
	if _, err := os.Stat(e.sock()); !errors.Is(err, os.ErrNotExist) || e.claim(envLeaseClaim) != nil {
		t.Fatalf("socket %v or record %+v of a failed open", err, e.claim(envLeaseClaim))
	}
	e.failWrites(false)
	e.openClaim()
	e.start()
	// a claim CLOSING without leases: only its removal is written
	e.edit(func(r *state.Records) { r.LeaseClaim(envLeaseClaim).State = state.LeaseClaimClosing })
	e.failWrites(true)
	wantErrText(t, e.b.CloseClaim(context.Background(), envLeaseClaim), "lease broker: ")
	if c := e.claim(envLeaseClaim); c == nil || c.State != state.LeaseClaimClosing {
		t.Fatalf("lease claim after a failed removal %+v", c)
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: e.sock(), Net: "unix"})
	mustOK(t, err)
	_ = conn.Close()
	e.failWrites(false)
	mustOK(t, e.b.CloseClaim(context.Background(), envLeaseClaim))
	if _, err := os.Stat(e.sock()); !errors.Is(err, os.ErrNotExist) || e.claim(envLeaseClaim) != nil {
		t.Fatalf("socket %v or record %+v after the close", err, e.claim(envLeaseClaim))
	}
}

// A released lease whose removal fails stays, and the next reconcile
// releases it again.
func TestFailedCommitKeepsReleasedLease(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.start()
	id := e.readyNUMA()
	e.edit(func(r *state.Records) {
		l := r.Lease(id)
		l.State, l.ReleaseRequested = state.LeaseReleasing, true
	})
	e.failWrites(true)
	if e.b.removeLease(e.lease(id)) {
		t.Fatal("removed without a commit")
	}
	if l := e.stored(id); l == nil || l.State != state.LeaseReleasing {
		t.Fatalf("lease after a failed removal %+v", l)
	}
	e.failWrites(false)
	e.b.reconcile()
	e.waitState(id, state.LeaseNone)
	e.wantGone(CompanionName(id))
}

// A peer of a pod added to the lease claim later is refused while the
// commit of the claim's pods fails.
func TestFailedCommitRefusesPeer(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.start()
	lc := leaseClaimObj()
	lc.Status.ReservedFor = append(lc.Status.ReservedFor, resourcev1.ResourceClaimConsumerReference{Resource: "pods", Name: "second", UID: "pod-uid-2"})
	_, err := e.client.ResourceV1().ResourceClaims(envNS).Update(context.Background(), lc, metav1.UpdateOptions{})
	mustOK(t, err)
	e.ctrs.setProcess(&Requester{Pod: state.PodRef{Namespace: envNS, Name: "second", UID: "pod-uid-2"}, Container: "x"})
	e.failWrites(true)
	if resp, _ := e.call(api.Request{Op: api.OpList}); !strings.HasPrefix(resp.Error, "not a container of the claim's pod") {
		t.Fatalf("peer of a pod whose commit failed %+v", resp)
	}
	e.failWrites(false)
	if resp, _ := e.call(api.Request{Op: api.OpList}); resp.Error != "" {
		t.Fatalf("peer of the second pod %+v", resp)
	}
}

// The leases whose companion the start did not restore stay as they are
// while their FAILED commit fails; the next reconcile fails and releases
// them.
func TestFailedCommitUnrestored(t *testing.T) {
	e := brokerEnv(t)
	e.openClaim()
	e.start()
	id := e.readyNUMA()
	e.edit(func(r *state.Records) {
		r.Preparations = append(r.Preparations, state.Preparation{UID: "uid-" + CompanionName(id), Driver: device.PoolDriverName,
			ClaimName: envNS + "/" + CompanionName(id), Lease: id, State: state.PreparationUnrestored})
	})
	e.failWrites(true)
	if failed := e.b.failUnrestored(); failed != nil {
		t.Fatalf("failed without a commit: %v", failed)
	}
	if l := e.stored(id); l.State != state.LeaseReady {
		t.Fatalf("lease after a failed commit %+v", l)
	}
	e.failWrites(false)
	e.b.reconcile()
	e.waitState(id, state.LeaseNone)
	if calls := e.ctrs.callLog(); !strings.Contains(calls, "revoke pod-uid-1/app 2") {
		t.Fatalf("not revoked; calls:\n%s", calls)
	}
}

// Requests at once are checked in their commits: the leases of a pod stay
// within its limit, the requests of one request id get one lease, and no
// lease is left unreleased on a lease claim that its Unprepare closed.
func TestConcurrentRequestsCheckedInTheirCommit(t *testing.T) {
	e := brokerEnv(t)
	e.opts = func(o *Options) { o.Limits = Limits{MaxPerPod: 3} }
	e.restart()
	e.openClaim()
	e.start()
	var chans []chan api.Response
	for i := range 8 {
		req := api.Request{Op: api.OpRequest, Size: "256Mi", Wait: "0s"}
		if i%2 == 0 {
			req.RequestID = "same"
		}
		chans = append(chans, e.async(req))
	}
	ids, same := map[string]bool{}, map[string]bool{}
	for i, ch := range chans {
		resp := <-ch
		switch {
		case resp.Error == "" && resp.Lease != "":
			ids[resp.Lease] = true
			if i%2 == 0 {
				same[resp.Lease] = true
			}
		case !strings.HasSuffix(resp.Error, "the limit of --lease-max-per-pod"):
			t.Fatalf("response %+v", resp)
		}
	}
	if len(ids) != 3 || len(same) > 1 || e.numLeases() != 3 {
		t.Fatalf("leases %v (of request id %v), %d stored", ids, same, e.numLeases())
	}

	e.opts = func(o *Options) { o.Limits = Limits{MaxPerPod: 100} }
	e.restart()
	e.start()
	chans = nil
	for range 6 {
		chans = append(chans, e.async(api.Request{Op: api.OpRequest, Size: "256Mi", Wait: "0s"}))
	}
	for err := e.b.CloseClaim(context.Background(), envLeaseClaim); err != nil; err = e.b.CloseClaim(context.Background(), envLeaseClaim) {
		e.sleep()
	}
	for _, ch := range chans {
		<-ch
	}
	for _, l := range e.st.Snapshot().Leases {
		if !l.ReleaseRequested {
			t.Fatalf("lease %+v of a closed lease claim not released", l)
		}
	}
}
