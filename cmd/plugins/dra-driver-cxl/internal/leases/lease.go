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
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// Lease: one request of a running container for memory, served through
// the socket of its lease claim; its memory comes from a companion claim
// cxl-lease-<id> that the broker creates with a companion pod, and that
// the driver of the device prepares and lends to the requester.
//
// Identity: the record state.Lease: ID, 8 hex digits, the suffix of the
// companion's name and the holder of its lent NUMA node; the requester
// (Pod, Container); Claim, the lease claim whose socket received it;
// RequestID, the client's idempotency key.
//
// States: state.LeaseState, in the protocol's words (stateName);
// RELEASING_BUSY is Releasing there.
//
// Transitions: state.LeaseTable, applied by apply. The request makes it
// CREATING, the companion's creation PENDING, the companion gate of its
// Prepare LENDING (OnLending), the lent devices READY (OnPrepared, in the
// DRA driver's commit of the companion's preparation PREPARED through
// PreparedStep); a release (commitRelease, or startRelease when the caller
// does not need to know that it was committed) makes it RELEASING, or
// keeps a FAILED lease FAILED so that a list shows why, and the release
// worker removes it once its companion pod and claim are gone; while the
// companion's bind is busy the lease is RELEASING_BUSY.
//
// Invariants: the lease owns its Lend, set at READY: the lent NUMA node of
// a numa lease, the bind target of an fs lease; the companion's grants
// keep a copy (state.GrantLend, recorded by the engine) so that the
// companion's Unprepare revokes without the lease. A RELEASING or
// RELEASING_BUSY lease is released by a running release or by the next
// reconcile; a FAILED one with ReleaseRequested by a running release, or by
// a reconcile once its companion pod is gone.
//
// Persistence: the record in the store, its only copy, CREATING before any
// API object exists; a document of another boot has no leases (the store
// drops them, the reconcile deletes their companions).
//
// Locks and waits: the commits of the store (the broker's releasing holds
// a release from before its commit, see Broker); the release worker waits
// for the companion with a backoff of 0.5 s to 8 s and calls the API and the
// NRI plugin without the broker's mutex.

// stateName returns the protocol's word for a lease state.
func stateName(s state.LeaseState) string {
	switch s {
	case state.LeaseCreating:
		return api.StateCreating
	case state.LeasePending:
		return api.StatePending
	case state.LeaseLending:
		return api.StateLending
	case state.LeaseReady:
		return api.StateReady
	case state.LeaseReleasing, state.LeaseReleasingBusy:
		return api.StateReleasing
	case state.LeaseFailed:
		return api.StateFailed
	}
	return string(s)
}

// releasingState tells whether a lease in the state is being released and
// not failed.
func releasingState(s state.LeaseState) bool {
	return s == state.LeaseReleasing || s == state.LeaseReleasingBusy
}

// askedSerial returns the serial that a request named in the protocol's
// words, "" for none.
func askedSerial(a *state.LeaseAsked) string {
	if !a.AsksSerial() {
		return ""
	}
	return a.Serial.String()
}

// got returns what the companion's Prepare gave a lease, nothing before.
func got(l *state.Lease) state.LeaseGot {
	if l.Got == nil {
		return state.LeaseGot{}
	}
	return *l.Got
}

// sourceOf returns the source of the device of a lease, the requested one
// before it has one.
func sourceOf(l *state.Lease) state.Source {
	if g := got(l); g.Source != "" {
		return g.Source
	}
	return l.Asked.Source
}

// serialOf returns the serial of the device of a lease in the protocol's
// words, the requested one before it has one, "" for none.
func serialOf(l *state.Lease) string {
	if l.Got != nil {
		return l.Got.Serial.String()
	}
	return askedSerial(&l.Asked)
}

// sharedDevice tells whether the device of a lease is a shared device.
func sharedDevice(l *state.Lease) bool {
	if l.Got != nil {
		return l.Got.Shared
	}
	return l.Asked.Shared
}

// accessOf and fsTypeOf return the access and the filesystem type of an fs
// lease: those mounted once it is Ready, the requested ones before.
func accessOf(l *state.Lease) state.Access {
	if g := got(l); g.Access != "" {
		return g.Access
	}
	return l.Asked.Access
}

func fsTypeOf(l *state.Lease) string {
	if g := got(l); g.FSType != "" {
		return g.FSType
	}
	return l.Asked.FSType
}

// numaNodeOf returns the lent NUMA node of a lease, -1 for none.
func numaNodeOf(l *state.Lease) int {
	if l.Lend == nil {
		return -1
	}
	return state.NUMANodeOf(l.Lend.NUMANode)
}

// memory returns what a Ready lease gives its container, false before.
// The path of a block lease is the host path of its block device.
func memory(l *state.Lease) (api.Memory, bool) {
	if l.State != state.LeaseReady || l.Got == nil {
		return api.Memory{}, false
	}
	g := *l.Got
	path := g.Path
	if l.Asked.Mode == state.ModeBlock {
		path = g.Block
	}
	// a persistent device: its attribute shared, whatever the share
	shared := l.Asked.Share || sharedDevice(l)
	if api.PersistentMode(string(l.Asked.Mode)) {
		shared = sharedDevice(l)
	}
	m := api.Memory{
		Key:      api.Key(uint64(g.Serial), 1),
		Serial:   g.Serial.String(),
		Shared:   shared,
		Mode:     string(l.Asked.Mode),
		Size:     g.Size,
		NUMANode: numaNodeOf(l),
		Dax:      g.Dax,
		Path:     path,
		Access:   string(accessOf(l)),
		ReadOnly: l.Asked.ReadOnly,
	}
	if l.Asked.Mode == state.ModeFS {
		m.FSType = fsTypeOf(l)
	}
	return m, true
}

// leaseInfo is the response of a lease.
func leaseInfo(l *state.Lease) *api.Response {
	g := got(l)
	r := &api.Response{Lease: l.ID, State: stateName(l.State), Source: string(sourceOf(l)), Mode: string(l.Asked.Mode),
		Share: l.Asked.Share, ReadOnly: l.Asked.ReadOnly, Access: string(accessOf(l)), FSType: fsTypeOf(l), Path: g.Path,
		Serial: serialOf(l), Dax: g.Dax, Block: g.Block, Error: l.Error, Size: g.Size}
	if n := numaNodeOf(l); n >= 0 {
		r.NUMANode = &n
	}
	if r.Size == 0 && l.Asked.Size != "" {
		if q, err := resource.ParseQuantity(l.Asked.Size); err == nil {
			r.Size = uint64(q.Value())
		}
	}
	if m, ok := memory(l); ok {
		r.Env = api.Env(m)
	}
	return r
}

// fdOf returns the device whose fd goes with the response of a lease: that
// of a Ready dax or block lease.
func fdOf(l *state.Lease) leaseFd {
	g := got(l)
	switch {
	case l.State != state.LeaseReady:
	case g.Dax != "":
		return leaseFd{path: g.Dax, readOnly: l.Asked.ReadOnly, lease: l.ID, got: g}
	case l.Asked.Mode == state.ModeBlock && g.Block != "":
		return leaseFd{path: g.Block, readOnly: l.Asked.ReadOnly, lease: l.ID, got: g}
	}
	return leaseFd{}
}

// startRelease starts the release of a lease (once) on trigger and returns
// its response (commitRelease).
func (b *Broker) startRelease(id, reason string, trigger state.Trigger) *api.Response {
	resp, _ := b.commitRelease(id, reason, trigger)
	return resp
}

// commitRelease starts the release of a lease (once) on trigger and returns
// its response, and an error unless its release is committed: a FAILED
// lease stays FAILED while it is released, so that it shows why. The
// release runs once its start is committed.
func (b *Broker) commitRelease(id, reason string, trigger state.Trigger) (*api.Response, error) {
	b.mu.Lock()
	running := b.releasing[id]
	b.releasing[id] = true
	b.mu.Unlock()
	if running {
		if l := b.records().Lease(id); l != nil && !l.ReleaseRequested {
			return leaseInfo(l), errors.New("its release is being committed")
		}
		return infoOf(b.records(), id), nil
	}
	var was string
	var started *state.Lease
	err := b.commit(func(r *state.Records) error {
		l := r.Lease(id)
		if l == nil {
			return nil
		}
		to, from := state.LeaseReleasing, stateName(l.State)
		if l.State == state.LeaseFailed || l.State == state.LeaseReleasingBusy {
			to = l.State
		}
		was = from
		if apply(l, to, trigger) {
			l.ReleaseRequested = true
			cp := l.Clone()
			started = &cp
		}
		return nil
	})
	if err != nil || started == nil {
		if err != nil {
			klog.Errorf("lease broker: lease %s: not releasing (%s): %v", id, reason, err)
		} else if was != "" {
			err = fmt.Errorf("lease %s is %s", id, was)
		}
		b.mu.Lock()
		delete(b.releasing, id)
		b.mu.Unlock()
		return infoOf(b.records(), id), err
	}
	klog.Infof("lease broker: lease %s: releasing (%s, was %s)", id, reason, was)
	b.wg.Add(1)
	go b.release(started)
	return leaseInfo(started), nil
}

// infoOf returns the response of the lease id of the records, Released
// without one.
func infoOf(r *state.Records, id string) *api.Response {
	if l := r.Lease(id); l != nil {
		return leaseInfo(l)
	}
	return &api.Response{Lease: id, State: api.StateReleased}
}

// release revokes the lent node of a numa lease, deletes the companion pod
// and claim, waits until both are gone and removes the lease; kubelet
// unprepares the companion claim in between (its driver releases the
// device), and while its bind is busy the lease is RELEASING_BUSY. The
// lease's DeletionRequested is set once the deletion of both is requested.
func (b *Broker) release(l *state.Lease) {
	defer b.wg.Done()
	b.mu.Lock()
	containers := b.containers
	b.mu.Unlock()
	if n := numaNodeOf(l); l.Asked.Mode == state.ModeNUMA && n >= 0 && containers != nil {
		ctx, cancel := b.apiContext()
		if err := containers.RevokeNUMANode(ctx, l.Pod.UID, l.Container, n, l.ID); err != nil {
			// The driver of the device revokes again before it
			// releases it, and fails (kubelet retries) while it cannot.
			klog.Errorf("lease broker: lease %s: revoking NUMA node %d: %v", l.ID, n, err)
		}
		cancel()
	}
	ns := l.Pod.Namespace
	foreign := map[string]bool{}
	delay := b.t.Poll
	for {
		gone, err := b.deleteCompanion(ns, l.ID, foreign)
		if err != nil {
			klog.Warningf("lease broker: lease %s: %v", l.ID, err)
		} else {
			b.deletionRequested(l.ID)
		}
		b.noteBusy(l.ID, gone)
		if gone {
			break
		}
		select {
		case <-b.ctx.Done():
			return // continued by the next plugin instance
		case <-time.After(delay):
		}
		// a bind can stay busy as long as a file of it is mapped
		delay = min(2*delay, b.t.Reconcile)
	}
	if b.removeLease(l) {
		klog.Infof("lease broker: lease %s: released", l.ID)
	}
}

// deletionRequested commits that the deletion of the companion of a lease
// was requested; a failed commit is logged (the release tries again).
func (b *Broker) deletionRequested(id string) {
	if l := b.records().Lease(id); l != nil && !l.DeletionRequested {
		b.change(func(r *state.Records) {
			if l := r.Lease(id); l != nil {
				l.DeletionRequested = true
			}
		})
	}
}

// removeLease removes a released lease and the directories of its lease
// claim when the claim is closed and this was its last lease, and reports
// whether the lease is gone: a failed commit leaves it for the next
// reconcile to release again.
func (b *Broker) removeLease(l *state.Lease) bool {
	removed := b.change(func(r *state.Records) {
		if cur := r.Lease(l.ID); cur != nil && apply(cur, state.LeaseNone, state.TriggerRelease) {
			r.Leases = slices.DeleteFunc(r.Leases, func(other state.Lease) bool { return other.ID == l.ID })
		}
	})
	b.mu.Lock()
	delete(b.releasing, l.ID)
	b.mu.Unlock()
	if !removed {
		return false
	}
	// the lease-fs directory of a closed lease claim may have waited for
	// the unmount of the companion's bind
	if r := b.records(); r.LeaseClaim(l.Claim) == nil && len(leasesOf(r, l.Claim)) == 0 {
		b.removeClaimDirs(l.Claim, state.TriggerRelease)
	}
	return true
}

// noteBusy makes a RELEASING lease RELEASING_BUSY while the bind of its
// companion is busy (its unbind failed with EBUSY: a file of it is mapped)
// and RELEASING again once it is not, or once the companion is gone.
func (b *Broker) noteBusy(id string, gone bool) {
	if _, ok := busyTo(b.records(), id, gone); ok {
		b.change(func(r *state.Records) {
			if to, ok := busyTo(r, id, gone); ok {
				apply(r.Lease(id), to, state.TriggerRelease)
			}
		})
	}
}

// busyTo returns the state that the lease id of the records goes to as the
// bind of its companion is busy or not (gone: the companion is gone), false
// for none.
func busyTo(r *state.Records, id string, gone bool) (state.LeaseState, bool) {
	l := r.Lease(id)
	if l == nil {
		return "", false
	}
	busy := !gone && companionBusy(r, id)
	switch {
	case busy && l.State == state.LeaseReleasing:
		return state.LeaseReleasingBusy, true
	case !busy && l.State == state.LeaseReleasingBusy:
		return state.LeaseReleasing, true
	}
	return "", false
}

// companionBusy tells whether the bind of a grant of the companion claim
// of the lease is STALE: its unmount failed busy and is retried.
func companionBusy(r *state.Records, id string) bool {
	for i := range r.Grants {
		g := &r.Grants[i]
		if g.Got.Bind == nil || g.Got.Bind.State != state.BindStale {
			continue
		}
		if (g.Got.Lend != nil && g.Got.Lend.Lease == id) || (g.Intent.Lend != nil && g.Intent.Lend.Lease == id) {
			return true
		}
	}
	return false
}

// deleteCompanion deletes the companion pod and claim of lease id and
// reports whether both are gone. It keeps an object of the companion name
// that does not have the lease and node labels of the lease. Such an
// object counts as gone. It logs the first such object of each kind and
// adds the kind to foreign.
func (b *Broker) deleteCompanion(ns, id string, foreign map[string]bool) (bool, error) {
	ctx, cancel := b.apiContext()
	defer cancel()
	name := CompanionName(id)
	// deleteOpts returns the options to delete the object of uid only.
	deleteOpts := func(uid types.UID) metav1.DeleteOptions {
		fg := metav1.DeletePropagationForeground
		return metav1.DeleteOptions{PropagationPolicy: &fg, Preconditions: metav1.NewUIDPreconditions(string(uid))}
	}
	// isForeign tells whether an object of the name is not the companion.
	isForeign := func(kind string, o metav1.ObjectMeta) bool {
		if o.Labels[device.LeaseLabel] == id && o.Labels[device.NodeLabel] == b.opts.NodeName {
			return false
		}
		if !foreign[kind] {
			foreign[kind] = true
			klog.Warningf("lease broker: lease %s: keeping %s %s/%s: it is not the companion of the lease on this node", id, kind, ns, name)
		}
		return true
	}
	var errs []error
	gone := true
	pod, err := b.opts.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		gone = false
		errs = append(errs, err)
	case isForeign("pod", pod.ObjectMeta):
	default:
		gone = false
		if pod.DeletionTimestamp == nil {
			if err := b.opts.Client.CoreV1().Pods(ns).Delete(ctx, name, deleteOpts(pod.UID)); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("deleting companion pod %s/%s: %w", ns, name, err))
			}
		}
	}
	claim, err := b.opts.Client.ResourceV1().ResourceClaims(ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		gone = false
		errs = append(errs, err)
	case isForeign("claim", claim.ObjectMeta):
	default:
		gone = false
		if claim.DeletionTimestamp == nil {
			if err := b.opts.Client.ResourceV1().ResourceClaims(ns).Delete(ctx, name, deleteOpts(claim.UID)); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("deleting companion claim %s/%s: %w", ns, name, err))
			}
		}
	}
	return gone, errors.Join(errs...)
}

// failUnrestored fails the LENDING and READY leases whose companion claim
// the start did not restore (its preparation PREPARED_UNRESTORED) and
// returns them: the requester's next list shows them Failed, and they are
// released. A failed commit is logged: the next reconcile tries again.
func (b *Broker) failUnrestored() []string {
	if len(unrestoredOf(b.records())) == 0 {
		return nil
	}
	failed := map[string]string{}
	if !b.change(func(r *state.Records) {
		for id, claim := range unrestoredOf(r) {
			if l := r.Lease(id); apply(l, state.LeaseFailed, state.TriggerRecover) {
				l.Error = fmt.Sprintf("the device of companion claim %s was not restored at the start of the driver", claim)
				failed[id] = l.Error
			}
		}
	}) {
		return nil
	}
	ids := slices.Sorted(maps.Keys(failed))
	for _, id := range ids {
		klog.Warningf("lease broker: lease %s: %s", id, failed[id])
	}
	return ids
}

// unrestoredOf returns the LENDING and READY leases of the records whose
// companion's preparation is PREPARED_UNRESTORED, with the name of the
// companion claim.
func unrestoredOf(r *state.Records) map[string]string {
	out := map[string]string{}
	for i := range r.Preparations {
		p := &r.Preparations[i]
		if p.Lease == "" || p.State != state.PreparationUnrestored {
			continue
		}
		if l := r.Lease(p.Lease); l != nil && (l.State == state.LeaseReady || l.State == state.LeaseLending) {
			out[l.ID] = p.ClaimName
		}
	}
	return out
}

// CompanionName is the name of the companion claim and pod of a lease.
func CompanionName(id string) string {
	return "cxl-lease-" + id
}

// fsBindTarget returns the bind target of an fs lease in the filesystem
// directory of its lease claim, "" for none.
func fsBindTarget(fsDir, name string) string {
	if fsDir == "" || name == "" {
		return ""
	}
	return filepath.Join(fsDir, name)
}
