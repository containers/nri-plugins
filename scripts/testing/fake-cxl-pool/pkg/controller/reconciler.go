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

package controller

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/client"
)

// attKey identifies an attachment: server device and host names.
type attKey struct {
	device string
	host   string
}

// want is one allocation result of our driver and pool.
type want struct {
	claim  *resourceapi.ResourceClaim
	result resourceapi.DeviceRequestAllocationResult
	device string // server device name, "" if not in the pool
	node   string
	host   *api.Host // nil: the node is not a pool host
	// allShares: a shared device allocated with all of its shares
	// capacity
	allShares bool
	// writer: a share of a shared device that the result writes with a
	// single-host filesystem (config.NeedsSingleHost)
	writer bool
	// fsType: device.fsType of the MemoryConfig of the result
	fsType string
	// singleHost: allShares or writer, attached with
	// AttachRequest.SingleHost
	singleHost bool
	// keepSingleHost: the want is attached and its MemoryConfig cannot be
	// resolved: it is single-host if its attachment is
	keepSingleHost bool
	// settings: where to attach, from the MemoryConfig of the result
	settings PoolSettings
	// cfgErr: the MemoryConfig of the result is invalid, no attach
	cfgErr error
}

// allShares reports whether an allocation result of a shared device
// consumes all of its shares, or none is recorded (no shares request):
// then no other host may attach the device while it is attached for the
// claim.
func (c *Controller) allShares(r *resourceapi.DeviceRequestAllocationResult) bool {
	d, ok := c.devices[r.Device]
	if !ok || !d.Shared {
		return false
	}
	q, ok := r.ConsumedCapacity[device.CapacityShares]
	return !ok || q.Value() >= device.SharesUnlimited
}

// firstShared returns the first want that is not single-host, nil if
// every want is.
func firstShared(ws []*want) *want {
	for _, w := range ws {
		if !w.singleHost {
			return w
		}
	}
	return nil
}

// devicesCoherent tells that the hardware keeps the memory of every pool
// device coherent between the hosts that share it: the devices of
// fake-cxl-pool are qemu memory backends that every host maps from the
// same host memory.
const devicesCoherent = true

// writerResult reports whether an allocation result of a shared device
// with the resolved MemoryConfig makes its attachment single-host: a read-write filesystem that is not a cluster filesystem on
// coherent memory.
func (c *Controller) writerResult(r *resourceapi.DeviceRequestAllocationResult, res *config.Resolved) bool {
	d, ok := c.devices[r.Device]
	if !ok || !d.Shared || res == nil {
		return false
	}
	return config.NeedsSingleHost(res.Config, devicesCoherent)
}

// anySingleHost reports whether one of the wants of an attachment is
// single-host.
func anySingleHost(ws []*want) bool {
	for _, w := range ws {
		if w.singleHost {
			return true
		}
	}
	return false
}

func (w *want) key() attKey {
	if w.host == nil {
		return attKey{device: w.device}
	}
	return attKey{device: w.device, host: w.host.Name}
}

func (w *want) String() string {
	return fmt.Sprintf("claim %s/%s device %s node %s", w.claim.Namespace, w.claim.Name, w.result.Device, w.node)
}

// failure is a failed attach.
type failure struct {
	reason  string
	message string
	// singleHostOnly: making an existing attachment single-host failed; the
	// wants that are not single-host keep the attachment
	singleHostOnly bool
}

// Owner returns the attachment owner of a claim.
func (c *Controller) Owner(claimUID types.UID) string {
	return c.cfg.OwnerPrefix + "resourceclaim/" + string(claimUID)
}

// reconcile computes desired attachments from the claims and actual ones
// from the server, attaches missing ones to the hosts of the nodes in hm,
// writes the claim status, and detaches attachments that no claim wants
// any more.
func (c *Controller) reconcile(ctx context.Context, hm hostMap) error {
	// 1. desired
	claims, err := c.claims.List(labels.Everything())
	if err != nil {
		return err
	}
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].Namespace != claims[j].Namespace {
			return claims[i].Namespace < claims[j].Namespace
		}
		return claims[i].Name < claims[j].Name
	})
	var wants []*want
	desired := map[attKey][]*want{}
	cfgSeen := map[string]bool{}
	for _, claim := range claims {
		if claim.Status.Allocation == nil {
			continue
		}
		node := device.AllocationNode(claim.Status.Allocation)
		for _, r := range claim.Status.Allocation.Devices.Results {
			if r.Driver != device.PoolDriverName || r.Pool != c.cfg.DRAPoolName {
				continue
			}
			w := &want{claim: claim, result: r, node: node, allShares: c.allShares(&r)}
			if d, ok := c.devices[r.Device]; ok {
				w.device = d.Name
			}
			if h, ok := hm.nodeHost[node]; ok {
				w.host = &h
			}
			var res *config.Resolved
			w.settings, res, w.cfgErr = c.poolSettings(claim, &r)
			if res != nil {
				w.fsType = res.Config.DeviceSection().FSType
			}
			w.writer = c.writerResult(&r, res)
			w.singleHost = w.allShares || w.writer
			if w.cfgErr != nil {
				cfgSeen[c.logConfigError(w)] = true
				if hasCondition(claim, r, c.condAttached()) != nil {
					// attached already: a device in use is never detached,
					// and an unknown writer keeps its single-host flag
					w.cfgErr = nil
					w.keepSingleHost = res == nil
				}
			}
			wants = append(wants, w)
			if w.device != "" && w.host != nil && w.cfgErr == nil {
				desired[w.key()] = append(desired[w.key()], w)
			}
		}
	}

	// 2. actual: the attachments of our owners on the hosts of our nodes.
	// An attachment without owner (the server adopts the devices that it
	// finds in qemu, for instance after a lost state file) is taken over
	// in 3 if a claim wants it, so that 5 detaches it later.
	atts, err := c.pool.Attachments(ctx, "", "")
	if err != nil {
		return fmt.Errorf("list attachments: %w", err)
	}
	actual := map[attKey]api.Attachment{}
	ownerless := map[attKey]api.Attachment{}
	for _, a := range atts {
		if _, ours := hm.hostNode[a.Host]; !ours {
			continue
		}
		k := attKey{a.Device, a.Host}
		switch {
		case strings.HasPrefix(a.Owner, c.cfg.OwnerPrefix):
			actual[k] = a
		case a.Owner == "":
			ownerless[k] = a
		}
	}
	for _, w := range wants {
		if a, ok := actual[w.key()]; ok && w.keepSingleHost && a.SingleHost && !w.singleHost {
			c.debugf("%s: configuration not resolved: attachment %s stays single-host", w, a.ID)
			w.singleHost = true
		}
	}

	// 3. attach, or set the single-host flag of an attachment to that of
	// its claims (an attachment serves all claims of the node), or take
	// over an attachment without owner (the attach request gives it ours)
	// Attachments that exist come first: dropping the single-host flag of
	// the attachment of one node lets the new attachments of the other
	// nodes in, in the same reconcile.
	failures := map[attKey]failure{}
	keys := slices.SortedFunc(maps.Keys(desired), cmpAttKey)
	sort.SliceStable(keys, func(i, j int) bool {
		_, ei := actual[keys[i]]
		_, ej := actual[keys[j]]
		return ei && !ej
	})
	for _, k := range keys {
		singleHost := anySingleHost(desired[k])
		if a, ok := actual[k]; ok && (a.State != api.AttachmentAttached || a.SingleHost == singleHost) {
			continue
		}
		w := desired[k][0]
		if singleHost {
			c.logWriter(k, desired[k])
		}
		_, exists := actual[k]
		if o, ok := ownerless[k]; ok {
			c.logf("attach %s to %s for %s: taking over attachment %s without owner (%s, adopted %v, qemu device %s)",
				k.device, k.host, w, o.ID, o.State, o.Adopted, o.QemuDeviceID)
			exists = true
		}
		a, f := c.attach(ctx, w, k, singleHost, exists, actual, desired)
		if f != nil && singleHost && f.reason == ReasonConflict {
			if _, ok := actual[k]; ok {
				// an existing attachment stays for the other wants
				f.singleHostOnly = true
			} else if r := firstShared(desired[k]); r != nil {
				// a new attachment: the other wants may have one that
				// is not single-host
				c.logf("attach %s to %s for %s: not single-host for the claims that do not need it", k.device, k.host, r)
				// no answer (attaching continues, waiting for a
				// detach): the other wants wait, they are not refused
				if ra, rf := c.attach(ctx, r, k, false, exists, actual, desired); rf == nil {
					a, f.singleHostOnly = ra, true
				}
			}
		}
		if f != nil {
			failures[k] = *f
		}
		if a != nil {
			actual[k] = *a
		}
	}

	// 4. status
	for _, w := range wants {
		c.writeStatus(ctx, w, actual, failures)
	}

	// 5. detach
	seen := map[string]bool{}
	for _, k := range slices.SortedFunc(maps.Keys(actual), cmpAttKey) {
		a := actual[k]
		seen[a.ID] = true
		if _, ok := desired[k]; ok {
			delete(c.attLogged, a.ID)
			continue
		}
		c.detach(ctx, a, hm, claims)
	}
	for id := range c.attLogged {
		if !seen[id] {
			delete(c.attLogged, id)
		}
	}
	for id := range c.cfgLogged {
		if !cfgSeen[id] {
			delete(c.cfgLogged, id)
		}
	}
	return nil
}

// logWriter logs the wants of a single-host attachment that are
// single-host because they write a single-host filesystem, unless one of the wants
// has all shares of the device.
func (c *Controller) logWriter(k attKey, ws []*want) {
	for _, w := range ws {
		if w.allShares {
			return
		}
	}
	for _, w := range ws {
		if !w.writer {
			continue
		}
		fsType := w.fsType
		if fsType == "" {
			fsType = "filesystem"
		}
		c.logf("attach %s to %s for %s: single host for a read-write %s claim", k.device, k.host, w, fsType)
	}
}

// logConfigError logs the configuration error of a want once and returns
// the key of the want in cfgLogged.
func (c *Controller) logConfigError(w *want) string {
	id := string(w.claim.UID) + "/" + w.result.Request + "/" + w.result.Device
	msg := w.cfgErr.Error()
	if c.cfgLogged[id] == msg {
		return id
	}
	c.cfgLogged[id] = msg
	kept := ""
	if hasCondition(w.claim, w.result, c.condAttached()) != nil {
		kept = " (attached already, kept)"
	}
	c.logf("ERROR: claim %s/%s: request %s device %s: invalid configuration: %v%s",
		w.claim.Namespace, w.claim.Name, w.result.Request, w.result.Device, w.cfgErr, kept)
	return id
}

// cmpAttKey orders attachment keys by device, then by host.
func cmpAttKey(a, b attKey) int {
	return cmp.Or(strings.Compare(a.device, b.device), strings.Compare(a.host, b.host))
}

// attach attaches the device of a desired key (exists: it is attached
// already). It returns the attachment, or a failure, or neither
// (attaching continues, or waiting for a detach).
func (c *Controller) attach(ctx context.Context, w *want, k attKey, singleHost, exists bool, actual map[attKey]api.Attachment, desired map[attKey][]*want) (*api.Attachment, *failure) {
	owner := c.Owner(w.claim.UID)
	req := api.AttachRequest{
		Host:       k.host,
		Owner:      owner,
		Timeout:    c.cfg.AttachTimeout.String(),
		SingleHost: singleHost,
	}
	// the settings place a new attachment; an existing one stays where it
	// is
	if !exists {
		req.Slot, req.NUMANode = w.settings.Slot, w.settings.NUMANode
		if s := w.settings.String(); s != "" {
			c.debugf("attach %s to %s for %s: %s", k.device, k.host, w, s)
		}
	}
	actx, cancel := context.WithTimeout(ctx, c.cfg.AttachTimeout+10*time.Second)
	defer cancel()
	a, status, err := c.pool.Attach(actx, k.device, req)
	if err == nil {
		switch status {
		case http.StatusAccepted:
			c.logf("attach %s to %s for %s: still attaching after %s", k.device, k.host, w, c.cfg.AttachTimeout)
		case http.StatusOK:
			c.logf("attach %s to %s for %s: already attached (%s, owner %q, single host %v)", k.device, k.host, w, a.State, a.Owner, a.SingleHost)
		default:
			c.logf("attached %s to %s (node %s, slot %s, qemu device %s, owner %s, single host %v) for %s/%s",
				k.device, k.host, w.node, a.Slot.Bus, a.QemuDeviceID, owner, a.SingleHost, w.claim.Namespace, w.claim.Name)
		}
		return a, nil
	}
	if ctx.Err() != nil {
		return nil, nil
	}
	f := &failure{reason: ReasonAttachError, message: fmt.Sprintf("attach %s to %s: %v", k.device, k.host, err)}
	switch {
	case actx.Err() != nil || strings.Contains(err.Error(), "deadline exceeded"):
		f.reason = ReasonTimeout
	case api.IsCode(err, api.CodeConflict):
		f.reason = ReasonConflict
		// An unshared device that moves between our nodes: the old
		// attachment is detached in this reconcile, attach again in the next
		// one instead of failing the new allocation.
		for ok, oa := range actual {
			if ok.device == k.device && ok.host != k.host {
				if _, still := desired[ok]; !still {
					c.logf("attach %s to %s for %s: waiting for its detach from %s (%s)", k.device, k.host, w, ok.host, oa.State)
					return nil, nil
				}
			}
		}
	}
	c.logf("ERROR: %s for %s: %s", f.reason, w, f.message)
	return nil, f
}

// writeStatus writes Attached or AttachFailed into the claim status of a
// want when it is not there yet, and the device data of Attached when it
// changed (a new attachment after a qemu restart).
func (c *Controller) writeStatus(ctx context.Context, w *want, actual map[attKey]api.Attachment, failures map[attKey]failure) {
	var f *failure
	switch {
	case w.cfgErr != nil:
		f = &failure{reason: ReasonInvalidConfig, message: w.cfgErr.Error()}
	case w.device == "":
		f = &failure{reason: ReasonAttachError, message: fmt.Sprintf("device %s is not in pool %s", w.result.Device, c.cfg.DRAPoolName)}
	case w.host == nil:
		f = &failure{reason: ReasonNoHost, message: fmt.Sprintf("node %q is not a host of the pool", w.node)}
	default:
		if x, ok := failures[w.key()]; ok && (w.singleHost || !x.singleHostOnly) {
			f = &x
		} else if a, ok := actual[w.key()]; ok && a.State == api.AttachmentFailed {
			// The server keeps a failed attachment until its qemu device
			// is gone or a forced detach forgets it.
			f = &failure{reason: ReasonAttachError, message: fmt.Sprintf("attachment %s failed: %s", a.ID, a.Error)}
		}
	}
	if f != nil {
		if old := hasCondition(w.claim, w.result, c.condFailed()); old != nil && old.Message == f.message && old.Reason == f.reason {
			return
		}
		cond := metav1.Condition{Type: c.condFailed(), Status: metav1.ConditionTrue, Reason: f.reason, Message: f.message}
		c.updateStatus(ctx, w, cond, nil, corev1.EventTypeWarning, EventAttachFailed)
		return
	}
	a, ok := actual[w.key()]
	if !ok || a.State != api.AttachmentAttached || (w.singleHost && !a.SingleHost) {
		if ok {
			c.debugf("%s: attachment %s is %s (single host %v, wanted %v)", w, a.ID, a.State, a.SingleHost, w.singleHost)
		}
		return
	}
	d := c.devices[w.result.Device]
	serial := a.Serial
	if serial == "" {
		serial = d.Serial
	}
	data := DeviceData{
		PoolStatusData: device.PoolStatusData{
			Serial:     strings.ToLower(serial),
			Shared:     new(d.Shared),
			Size:       new(uint64(d.Size)),
			Exclusive:  w.allShares,
			Persistent: d.Persistent,
			SingleHost: w.singleHost && a.SingleHost,
			Coherent:   devicesCoherent,
		},
		Host:         a.Host,
		HostUUID:     w.host.UUID,
		Attachment:   a.ID,
		QemuDeviceID: a.QemuDeviceID,
	}
	cond := metav1.Condition{
		Type:    c.condAttached(),
		Status:  metav1.ConditionTrue,
		Reason:  ReasonAttached,
		Message: fmt.Sprintf("%s attached to %s (slot %s)", w.result.Device, a.Host, a.Slot.Bus),
	}
	if attachedUpToDate(w.claim, w.result, cond, data) {
		return
	}
	if s := w.settings.Slot; s != "" && s != a.Slot.Bus {
		c.logf("%s: attachment %s is in slot %s, not in the configured slot %s: kept", w, a.ID, a.Slot.Bus, s)
	}
	c.updateStatus(ctx, w, cond, data, corev1.EventTypeNormal, EventAttached)
}

func (c *Controller) updateStatus(ctx context.Context, w *want, cond metav1.Condition, data any, eventType, reason string) {
	written, err := c.setClaimDeviceStatus(ctx, w.claim, w.result, cond, data)
	if err != nil {
		c.logf("ERROR: status of %s: %v", w, err)
		return
	}
	if !written {
		c.debugf("status of %s: up to date or claim no longer allocated", w)
		return
	}
	c.logf("status of %s: %s=True reason %s: %s", w, cond.Type, cond.Reason, cond.Message)
	c.recorder.Event(w.claim, eventType, reason, cond.Message)
}

// detach detaches an attachment that no claim wants. Only attachments in
// state attached get a detach request: a "detaching" one is waited for by
// the server (the guest has not released the device yet), a "failed" one
// is left alone.
func (c *Controller) detach(ctx context.Context, a api.Attachment, hm hostMap, claims []*resourceapi.ResourceClaim) {
	switch a.State {
	case api.AttachmentAttaching:
		c.debugf("detach %s: still attaching, later", a.ID)
		return
	case api.AttachmentDetaching:
		if c.attLogged[a.ID] != a.State {
			c.attLogged[a.ID] = a.State
			c.logf("detach %s: still detaching (has the guest released the device?)", a.ID)
		}
		return
	case api.AttachmentFailed:
		if st := a.State + ": " + a.Error; c.attLogged[a.ID] != st {
			c.attLogged[a.ID] = st
			c.logf("ERROR: detach %s: attachment failed: %s; not retried (the guest kept the device: see README, release before detach)", a.ID, a.Error)
		}
		return
	}
	dctx, cancel := context.WithTimeout(ctx, c.cfg.DetachTimeout+10*time.Second)
	defer cancel()
	res, _, err := c.pool.Detach(dctx, a.Device, a.Host, client.DetachOptions{Timeout: c.cfg.DetachTimeout})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		state := a.State
		if res != nil && res.State != "" {
			state = res.State
		}
		c.logf("ERROR: detach %s (owner %s): %v; attachment %s, retrying later", a.ID, a.Owner, err, state)
		return
	}
	node := hm.hostNode[a.Host]
	msg := fmt.Sprintf("%s detached from %s (node %s)", a.Device, a.Host, node)
	c.logf("detached %s from %s (node %s, owner %s)", a.Device, a.Host, node, a.Owner)
	delete(c.attLogged, a.ID)
	var obj runtime.Object
	uid := strings.TrimPrefix(a.Owner, c.cfg.OwnerPrefix+"resourceclaim/")
	for _, claim := range claims {
		if string(claim.UID) == uid {
			obj = claim
			break
		}
	}
	if obj == nil && node != "" {
		if n, err := c.nodes.Get(node); err == nil {
			obj = n
		}
	}
	if obj != nil {
		c.recorder.Event(obj, corev1.EventTypeNormal, EventDetached, msg)
	}
}
