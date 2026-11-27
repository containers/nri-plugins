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
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// The reconcile: at the start once the NRI plugin is synchronized,
// then every Timeouts.Reconcile. It releases the leases whose requester
// or companion is gone and the CREATING ones left over by a crash, starts
// the releases that a stop cut again, lends the nodes of the READY numa
// leases again, deletes the companions of this node that have no lease and
// removes the directories of closed lease claims. It also does again what
// a failed commit left undone: it fails the leases whose companion the
// start did not restore, and listens on the lease claims that are OPEN
// without a socket. The NRI plugin hears after the first one that
// the lends are restored.

// reconcileLoop reconciles at the start (once the NRI plugin is
// synchronized) and periodically.
func (b *Broker) reconcileLoop(containers Containers) {
	defer b.wg.Done()
	if containers != nil {
		ctx, cancel := context.WithTimeout(b.ctx, b.t.SyncWait)
		if err := containers.WaitSynchronized(ctx); err != nil {
			klog.Warningf("lease broker: %v; reconciling anyway", err)
		}
		cancel()
	}
	for first := true; ; first = false {
		b.reconcile()
		if first && containers != nil {
			containers.LendsRestored()
		}
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(b.t.Reconcile):
		}
	}
}

// reconcile runs one reconcile: it releases the leases left releasing, left
// CREATING by a crash, FAILED (also while their companion runs), and those
// whose companion or requester is gone.
func (b *Broker) reconcile() {
	b.mu.Lock()
	containers := b.containers
	b.mu.Unlock()
	b.failUnrestored()
	// the leases before the list: the companion of a lease past CREATING
	// is in it
	leases := b.records().Leases
	pods, err := b.companionPods()
	if err != nil {
		klog.Warningf("lease broker: listing companion pods: %v", err)
	}
	// requesterDone of the pods of stopped containers, by pod UID
	done := map[string]bool{}

	for i := range leases {
		l := &leases[i]
		switch {
		case releasingState(l.State):
			b.startRelease(l.ID, "release interrupted", state.TriggerReconcile)
			continue
		case l.State == state.LeaseFailed:
			// also while its companion still runs
			b.startRelease(l.ID, "failed: "+l.Error, state.TriggerReconcile)
			continue
		case l.State == state.LeaseCreating:
			if time.Since(l.Created) > b.t.CreatingGrace {
				b.startRelease(l.ID, "left over in Creating", state.TriggerReconcile)
			}
			continue
		}
		if gone, reason := companionGone(pods, err, l.Pod.Namespace, CompanionName(l.ID)); gone {
			b.startRelease(l.ID, reason, state.TriggerReconcile)
			continue
		}
		if containers != nil && !containers.Running(l.Pod.UID, l.Container) {
			d, ok := done[l.Pod.UID]
			if !ok {
				d = b.requesterDone(l.Pod)
				done[l.Pod.UID] = d
			}
			if d {
				b.startRelease(l.ID, "requester container gone", state.TriggerReconcile)
			}
			continue
		}
		if l.State == state.LeaseReady && l.Asked.Mode == state.ModeNUMA && numaNodeOf(l) >= 0 {
			b.relendContainer(l.Pod.UID, l.Container)
		}
	}
	if err == nil {
		b.deleteUnowned(pods)
	}
	b.sweepFsDirs(state.TriggerReconcile)
	b.listenAgain()
}

// listenAgain listens again on the sockets of the lease claims that are
// OPEN_NO_LISTENER (a listen at the start failed) or OPEN without one (a
// commit of the start failed); a failure is logged and tried again at the
// next reconcile.
func (b *Broker) listenAgain() {
	r := b.records()
	var again []string
	b.mu.Lock()
	for i := range r.LeaseClaims {
		lc := &r.LeaseClaims[i]
		if lc.State != state.LeaseClaimOpenNoListener && (lc.State != state.LeaseClaimOpen || b.listeners[lc.UID] != nil) {
			continue
		}
		if err := b.listen(lc.UID); err != nil {
			klog.Warningf("lease broker: lease claim %s: listening again: %v", lc.UID, err)
			continue
		}
		klog.Infof("lease broker: lease claim %s/%s (%s): listening again", lc.Namespace, lc.Name, lc.UID)
		if lc.State == state.LeaseClaimOpenNoListener {
			again = append(again, lc.UID)
		}
	}
	b.mu.Unlock()
	if len(again) == 0 {
		return
	}
	b.change(func(r *state.Records) {
		for _, uid := range again {
			if lc := r.LeaseClaim(uid); lc != nil && lc.State == state.LeaseClaimOpenNoListener {
				applyClaim(lc, state.LeaseClaimOpen, state.TriggerReconcile)
			}
		}
	})
}

// companionPods returns the companion pods of the node: the pods with the
// node and lease labels, by namespace and name.
func (b *Broker) companionPods() (map[types.NamespacedName]*corev1.Pod, error) {
	ctx, cancel := b.apiContext()
	defer cancel()
	list, err := b.opts.Client.CoreV1().Pods("").List(ctx, companionSelector(b.opts.NodeName))
	if err != nil {
		return nil, err
	}
	pods := make(map[types.NamespacedName]*corev1.Pod, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		pods[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = p
	}
	return pods, nil
}

// companionSelector returns the options that list the companions of node.
func companionSelector(node string) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: device.NodeLabel + "=" + node + "," + device.LeaseLabel}
}

// companionGone reports whether a companion pod no longer holds its claim:
// not in the companion pods, being deleted or finished (evicted), and why;
// false if the pods were not listed (listErr).
func companionGone(pods map[types.NamespacedName]*corev1.Pod, listErr error, ns, name string) (bool, string) {
	p := pods[types.NamespacedName{Namespace: ns, Name: name}]
	switch {
	case listErr != nil:
		return false, ""
	case p == nil:
		return true, "companion pod gone"
	case p.DeletionTimestamp != nil:
		return true, "companion pod being deleted"
	case p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded:
		return true, "companion pod " + string(p.Status.Phase)
	}
	return false, ""
}

// requesterDone reports whether the leases of a container that is not
// running can be released: the pod is gone, being deleted or finished,
// or does not restart its containers. A container that restarts keeps
// its leases (the nodes of numa leases are lent again when it starts).
func (b *Broker) requesterDone(pod state.PodRef) bool {
	ctx, cancel := b.apiContext()
	defer cancel()
	p, err := b.opts.Client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return true
	case err != nil:
		return false
	}
	return string(p.UID) != pod.UID || p.DeletionTimestamp != nil ||
		p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed ||
		p.Spec.RestartPolicy == corev1.RestartPolicyNever
}

// containerGone is told that a container stopped or was removed: the
// leases of a container that will not come back are released.
func (b *Broker) containerGone(pod state.PodRef, container string) {
	var ids []string
	var owner state.PodRef
	r := b.records()
	for i := range r.Leases {
		if l := &r.Leases[i]; l.Pod.UID == pod.UID && l.Container == container {
			ids = append(ids, l.ID)
			owner = l.Pod
		}
	}
	b.mu.Lock()
	containers := b.containers
	b.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	if containers != nil && containers.Running(pod.UID, container) {
		return // a new instance is already there
	}
	if !b.requesterDone(owner) {
		klog.Infof("lease broker: container %s/%s/%s stopped, keeping its %d leases for its restart", pod.Namespace, pod.Name, container, len(ids))
		return
	}
	for _, id := range ids {
		b.startRelease(id, "requester container gone", state.TriggerRelease)
	}
}

// containerStarted is told that a container started: the nodes of its
// READY numa leases are lent to it again.
func (b *Broker) containerStarted(pod state.PodRef, container string) {
	b.relendContainer(pod.UID, container)
}

// relendContainer lends the nodes of the READY numa leases of a container
// again (idempotent).
func (b *Broker) relendContainer(podUID, container string) {
	b.mu.Lock()
	containers := b.containers
	b.mu.Unlock()
	if containers == nil {
		return
	}
	lends := state.LeaseLends(b.records(), podUID, container)
	for _, node := range slices.Sorted(maps.Keys(lends)) {
		for _, id := range lends[node] {
			ctx, cancel := b.apiContext()
			if err := containers.LendNUMANode(ctx, podUID, container, node, id); err != nil {
				klog.Errorf("lease broker: lease %s: re-applying NUMA node %d: %v", id, node, err)
			}
			cancel()
		}
	}
}

// deleteUnowned deletes the companion pods (of pods) and claims of this
// node that have no lease: the objects with the node and lease labels
// whose name is cxl-lease-<lease label>. A lost store does not know the
// leases of the node, so nothing is deleted then.
func (b *Broker) deleteUnowned(pods map[types.NamespacedName]*corev1.Pod) {
	if b.st.Lost() != nil {
		klog.V(3).Infof("lease broker: the lease table was not decoded, keeping the companions of the node")
		return
	}
	ctx, cancel := b.apiContext()
	defer cancel()
	sel := companionSelector(b.opts.NodeName)
	// unowned tells whether an object is a companion without a lease.
	unowned := func(o metav1.ObjectMeta) bool {
		id := o.Labels[device.LeaseLabel]
		if o.Name != CompanionName(id) || o.DeletionTimestamp != nil {
			return false
		}
		return b.records().Lease(id) == nil
	}
	for _, p := range pods {
		if unowned(p.ObjectMeta) {
			klog.Infof("lease broker: deleting companion pod %s/%s without a lease", p.Namespace, p.Name)
			if err := b.opts.Client.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{Preconditions: metav1.NewUIDPreconditions(string(p.UID))}); err != nil && !apierrors.IsNotFound(err) {
				klog.Warningf("lease broker: %v", err)
			}
		}
	}
	if claims, err := b.opts.Client.ResourceV1().ResourceClaims("").List(ctx, sel); err != nil {
		klog.Warningf("lease broker: listing companion claims: %v", err)
	} else {
		for _, c := range claims.Items {
			if unowned(c.ObjectMeta) {
				klog.Infof("lease broker: deleting companion claim %s/%s without a lease", c.Namespace, c.Name)
				if err := b.opts.Client.ResourceV1().ResourceClaims(c.Namespace).Delete(ctx, c.Name, metav1.DeleteOptions{Preconditions: metav1.NewUIDPreconditions(string(c.UID))}); err != nil && !apierrors.IsNotFound(err) {
					klog.Warningf("lease broker: %v", err)
				}
			}
		}
	}
}
