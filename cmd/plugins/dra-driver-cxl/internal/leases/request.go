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
	"fmt"
	"slices"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// The operations of the protocol: request, release (one or all), list, fd
// and env. A lease belongs to the pod of its requester for release, list
// and env, and to the requester container for fd.

// requestLease returns the response to a request of a container on the
// socket of a lease claim, and the device whose fd goes with it: it stores
// the lease, creates its companion and waits up to wait for it.
func (b *Broker) requestLease(claimUID string, who Requester, req *api.Request, wait time.Duration) (*api.Response, leaseFd) {
	if id := requested(b.records(), claimUID, who, req.RequestID); id != "" {
		return b.waitReady(id, wait, true)
	}
	l := &state.Lease{
		Claim:     claimUID,
		RequestID: req.RequestID,
		Pod:       who.Pod,
		Container: who.Container,
		State:     state.LeaseNone,
		Created:   time.Now().UTC(),
	}
	if err := b.requestOptions(&l.Asked, req); err != nil {
		return &api.Response{Error: err.Error()}, leaseFd{}
	}
	l, created, err := b.storeCreating(l, req)
	switch {
	case err != nil:
		return &api.Response{Error: err.Error()}, leaseFd{}
	case !created:
		// an equal request was stored first
		return b.waitReady(l.ID, wait, true)
	}
	a := &l.Asked
	klog.Infof("lease broker: lease %s: %s/%s/%s requests size %q serial %q (source %q, mode %s, share %v, shared device %v, read-only %v, access %q, name %q)",
		l.ID, l.Pod.Namespace, l.Pod.Name, l.Container, a.Size, askedSerial(a), a.Source, a.Mode, a.Share, a.Shared, a.ReadOnly, a.Access, a.Name)

	if err := b.createCompanion(l); err != nil {
		klog.Errorf("lease broker: lease %s: %v", l.ID, err)
		b.fail(l.ID, err.Error(), state.TriggerRequest, false)
		return b.releaseFailed(l.ID, err.Error()), leaseFd{}
	}
	b.change(func(r *state.Records) {
		if cur := r.Lease(l.ID); cur != nil && cur.State == state.LeaseCreating {
			apply(cur, state.LeasePending, state.TriggerRequest)
		}
	})
	return b.waitReady(l.ID, wait, true)
}

// requested returns the id of the lease of the records that the container
// asked for with the request id on the socket of the lease claim claimUID
// before, "" for none.
func requested(r *state.Records, claimUID string, who Requester, requestID string) string {
	if requestID == "" {
		return ""
	}
	for i := range r.Leases {
		if l := &r.Leases[i]; l.RequestID == requestID && l.Claim == claimUID && l.Pod.UID == who.Pod.UID && l.Container == who.Container {
			return l.ID
		}
	}
	return ""
}

// storeCreating commits a new lease CREATING before any API object exists,
// with its id (and the name of an fs lease) chosen, and returns it and
// true; when a lease of the container with the same request id and lease
// claim is stored, that one and false. The lease claim must take new
// leases, the pod must have fewer than Limits.LeasesPerPod.
func (b *Broker) storeCreating(l *state.Lease, req *api.Request) (*state.Lease, bool, error) {
	var stored state.Lease
	created := false
	who := Requester{Pod: l.Pod, Container: l.Container}
	err := b.commit(func(r *state.Records) error {
		if id := requested(r, l.Claim, who, l.RequestID); id != "" {
			stored, created = r.Lease(id).Clone(), false
			return nil
		}
		next := l.Clone()
		if err := creating(r, &next, req, b.opts.Limits, b.opts.NewID); err != nil {
			return err
		}
		stored, created = next, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return &stored, created, nil
}

// creating adds the lease l to the records CREATING, with its id chosen by
// newID (and the name of an fs lease). The lease claim must take new
// leases. The pod and the node must have fewer leases than their limits.
func creating(r *state.Records, l *state.Lease, req *api.Request, limits Limits, newID func() string) error {
	if err := claimOpen(r, l.Claim); err != nil {
		return err
	}
	n := 0
	for i := range r.Leases {
		if r.Leases[i].Pod.UID == l.Pod.UID {
			n++
		}
	}
	if n >= limits.MaxPerPod {
		return fmt.Errorf("pod %s/%s has %d leases, the limit of --lease-max-per-pod", l.Pod.Namespace, l.Pod.Name, n)
	}
	if n := len(r.Leases); n >= limits.LeasesPerNode {
		return fmt.Errorf("the node has %d leases, the limit of --lease-max-per-node", n)
	}
	if err := fsLeaseOptions(r, l, req, newID); err != nil {
		return err
	}
	if !apply(l, state.LeaseCreating, state.TriggerRequest) {
		return fmt.Errorf("lease %s: not created", l.ID)
	}
	r.Leases = append(r.Leases, l.Clone())
	return nil
}

// claimOpen returns an error if the lease claim uid of the records takes no
// new leases: it is closing (its Unprepare runs) or gone.
func claimOpen(r *state.Records, uid string) error {
	switch lc := r.LeaseClaim(uid); {
	case lc == nil:
		return fmt.Errorf("lease claim %s is not prepared: no new leases", uid)
	case lc.State == state.LeaseClaimClosing:
		return fmt.Errorf("lease claim %s/%s is being unprepared: no new leases", lc.Namespace, lc.Name)
	}
	return nil
}

// requestOptions sets the source, mode, sharing, access, name, size and
// device of a validated request on what a lease asks for.
func (b *Broker) requestOptions(a *state.LeaseAsked, req *api.Request) error {
	if b.opts.PoolDriver == "" && req.Source == api.SourcePool {
		return errPoolDisabled
	}
	a.Source, a.Share, a.ReadOnly, a.Mode = state.Source(req.Source), req.Share, req.EffectiveReadOnly(), state.Mode(req.EffectiveMode())
	a.Shared = req.Share
	if a.Mode == state.ModeFS {
		// access "": by the type of the filesystem, decided by the
		// driver of the device
		a.Access, a.Name, a.FSType = state.Access(req.Access), req.Name, req.FSType
	}
	if req.Size != "" {
		q, err := resource.ParseQuantity(req.Size)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("invalid size %q", req.Size)
		}
		a.Size = q.String()
	}
	if req.Serial == "" {
		return nil
	}
	return b.serialOptions(a, req)
}

// serialOptions sets the serial of a validated request on what a lease
// asks for and, when it names a shared device, that the device is shared
// and, for a device of volatile memory, mode dax. Modes block and fs need a
// persistent device, and a persistent device needs mode block or fs. A
// share needs a device with the capacity shares.
func (b *Broker) serialOptions(a *state.LeaseAsked, req *api.Request) error {
	serial, _ := api.ParseSerial(req.Serial)
	a.Serial, a.SerialZero = state.Serial(serial), serial == 0
	text := a.Serial.String()
	dev := b.deviceBySerial(serial)
	if dev == nil {
		return nil
	}
	if err := b.deviceRefusal(dev, a, req, text); err != nil {
		return err
	}
	switch {
	case api.PersistentMode(string(a.Mode)) && !dev.persistent:
		return fmt.Errorf("memdev %s: mode %s needs a persistent device", text, a.Mode)
	case !api.PersistentMode(string(a.Mode)) && dev.persistent:
		return fmt.Errorf("memdev %s: a persistent device needs mode %s or %s", text, api.ModeBlock, api.ModeFS)
	case !dev.shared:
		a.DeviceSource = deviceSourceOf(dev, req)
		return nil
	case req.Mode == api.ModeNUMA:
		return errors.New("shared memory is never onlined as RAM: use mode dax")
	}
	a.Shared = true
	if !dev.persistent {
		a.Mode = state.ModeDax
	}
	a.DeviceSource = deviceSourceOf(dev, req)
	return nil
}

// deviceRefusal returns the error of a request for a share of a device
// without shares and, for a shared or persistent device, of the wrong
// source; text is the serial of the device.
func (b *Broker) deviceRefusal(dev *publishedDevice, a *state.LeaseAsked, req *api.Request, text string) error {
	switch {
	case !dev.shares && a.Share:
		return fmt.Errorf("memdev %s: not shareable", text)
	case !dev.shared && !dev.persistent:
	case req.Source == api.SourceLocal && dev.driver != device.DriverName:
		return fmt.Errorf("memdev %s: not a local device", text)
	case req.Source == api.SourcePool && dev.driver == device.DriverName:
		return fmt.Errorf("memdev %s: not a pool device", text)
	}
	return nil
}

// deviceSourceOf returns the source of a device in a ResourceSlice when the
// request names no source, "" otherwise.
func deviceSourceOf(dev *publishedDevice, req *api.Request) state.Source {
	switch {
	case req.Source != "":
		return ""
	case dev.driver == device.DriverName:
		return state.SourceLocal
	}
	return state.SourcePool
}

// publishedDevice is a device with a serial in a ResourceSlice.
type publishedDevice struct {
	shared     bool
	shares     bool   // the device has the capacity shares
	persistent bool   // a device of persistent memory
	driver     string // the driver of the ResourceSlice
}

// deviceBySerial returns the published device with the serial, nil if
// there is none.
func (b *Broker) deviceBySerial(serial uint64) *publishedDevice {
	driver, d := b.opts.PublishedDevice(serial)
	if d == nil {
		return nil
	}
	dev := &publishedDevice{driver: driver}
	if a, ok := deviceAttribute(d, driver, device.AttrShared); ok && a.BoolValue != nil {
		dev.shared = *a.BoolValue
	}
	if a, ok := deviceAttribute(d, driver, device.AttrPersistent); ok && a.BoolValue != nil {
		dev.persistent = *a.BoolValue
	}
	_, dev.shares = d.Capacity[device.CapacityShares]
	return dev
}

// deviceAttribute returns the attribute of a device of driver, unqualified
// or qualified with the driver name.
func deviceAttribute(d *resourcev1.Device, driver string, name resourcev1.QualifiedName) (resourcev1.DeviceAttribute, bool) {
	for _, k := range []resourcev1.QualifiedName{name, resourcev1.QualifiedName(driver + "/" + string(name))} {
		if a, ok := d.Attributes[k]; ok {
			return a, true
		}
	}
	return resourcev1.DeviceAttribute{}, false
}

// fsLeaseOptions chooses the id of a new lease of the records by newID and,
// for an fs lease, its name: the requested one, or the id. An fs lease
// needs a name that no other lease of the claim has.
func fsLeaseOptions(r *state.Records, l *state.Lease, req *api.Request, newID func() string) error {
	fs := l.Asked.Mode == state.ModeFS
	// a default name never clashes with a name that a request chose
	for l.ID == "" || r.Lease(l.ID) != nil || (fs && req.Name == "" && nameUser(r, l.Claim, l.ID) != nil) {
		l.ID = newID()
	}
	if !fs {
		return nil
	}
	if l.Asked.Name == "" {
		l.Asked.Name = l.ID
	}
	if other := nameUser(r, l.Claim, l.Asked.Name); other != nil {
		return fmt.Errorf("name %q is in use by lease %s", l.Asked.Name, other.ID)
	}
	return nil
}

// nameUser returns the lease of the records of the lease claim whose fs
// mount has the name, nil if there is none.
func nameUser(r *state.Records, claimUID, name string) *state.Lease {
	for i := range r.Leases {
		if l := &r.Leases[i]; l.Claim == claimUID && l.Asked.Name == name {
			return l
		}
	}
	return nil
}

// waitReady waits until the lease is Ready or Failed, at most wait. At
// timeout a lease is released and reported Failed if release is set, else
// reported in its state. Wait 0 returns the state at once.
func (b *Broker) waitReady(id string, wait time.Duration, release bool) (*api.Response, leaseFd) {
	ctx, cancel := context.WithTimeout(b.ctx, wait)
	defer cancel()
	var resp *api.Response
	var fd leaseFd
	done := func() bool {
		l := b.records().Lease(id)
		switch {
		case l == nil:
			resp = &api.Response{Lease: id, State: api.StateReleased, Error: "lease was released"}
			return true
		case l.State == state.LeaseReady || l.State == state.LeaseFailed || releasingState(l.State):
			resp, fd = leaseInfo(l), fdOf(l)
			return true
		}
		resp = leaseInfo(l)
		return wait == 0
	}
	err := b.waitFor(ctx, done)
	if err != nil && b.ctx.Err() != nil {
		// a graceful stop: the lease stays for the restarted plugin
		msg := fmt.Sprintf("the driver is stopping: lease %s stays %s, ask again", id, resp.State)
		klog.Infof("lease broker: lease %s: %s", id, msg)
		resp.Error = msg
		return resp, leaseFd{}
	}
	if err != nil && release {
		msg := fmt.Sprintf("timeout: not Ready after %v", wait)
		klog.Warningf("lease broker: lease %s: %s, releasing", id, msg)
		return b.releaseFailed(id, msg), leaseFd{}
	}
	return resp, fd
}

// releaseFailed starts the release of a lease that failed with msg and
// returns the response: Failed with msg once the release is committed, else
// the lease as it is with why it was not released.
func (b *Broker) releaseFailed(id, msg string) *api.Response {
	resp, err := b.commitRelease(id, msg, state.TriggerRequest)
	if err != nil {
		resp.Error = fmt.Sprintf("%s; not released: %v: release it", msg, err)
		return resp
	}
	resp.State, resp.Error = api.StateFailed, msg
	return resp
}

// ownLease returns the lease of the records if it was asked for on the
// socket of the lease claim claimUID by a container of the requester's pod.
func ownLease(r *state.Records, claimUID string, who Requester, id string) *state.Lease {
	l := r.Lease(id)
	if l == nil || l.Claim != claimUID || l.Pod.UID != who.Pod.UID {
		return nil
	}
	return l
}

// releaseOne releases a lease of the pod of the requester asked for on the
// socket of the lease claim claimUID and returns its state after waiting up
// to wait for it to be gone.
func (b *Broker) releaseOne(claimUID string, who Requester, id string, wait time.Duration) *api.Response {
	if ownLease(b.records(), claimUID, who, id) == nil {
		return &api.Response{Lease: id, Error: "no such lease of this pod"}
	}
	b.startRelease(id, "released by "+who.Container, state.TriggerRelease)
	return b.waitReleased([]string{id}, wait)[0]
}

// releaseAll releases every lease of the pod of the requester asked for on
// the socket of the lease claim claimUID and returns their states after
// waiting up to wait for them to be gone.
func (b *Broker) releaseAll(claimUID string, who Requester, wait time.Duration) *api.Response {
	var ids []string
	for _, l := range leasesOf(b.records(), claimUID) {
		if l.Pod.UID == who.Pod.UID {
			ids = append(ids, l.ID)
		}
	}
	for _, id := range ids {
		b.startRelease(id, "released by "+who.Container, state.TriggerRelease)
	}
	out := []api.Response{}
	for _, r := range b.waitReleased(ids, wait) {
		out = append(out, *r)
	}
	return &api.Response{Leases: &out}
}

// waitReleased waits until the leases are gone, at most wait, and returns
// their states (Released, or the current state).
func (b *Broker) waitReleased(ids []string, wait time.Duration) []*api.Response {
	ctx, cancel := context.WithTimeout(b.ctx, wait)
	defer cancel()
	gone := func() bool {
		r := b.records()
		return !slices.ContainsFunc(ids, func(id string) bool { return r.Lease(id) != nil })
	}
	_ = b.waitFor(ctx, gone)
	r := b.records()
	out := make([]*api.Response, 0, len(ids))
	for _, id := range ids {
		out = append(out, infoOf(r, id))
	}
	return out
}

// list returns the leases of the pod of the requester asked for on the
// socket of the lease claim claimUID.
func (b *Broker) list(claimUID string, who Requester) *api.Response {
	out := []api.Response{}
	for _, l := range leasesOf(b.records(), claimUID) {
		if l.Pod.UID == who.Pod.UID {
			out = append(out, *leaseInfo(l))
		}
	}
	return &api.Response{Leases: &out}
}

// waitLease waits until a lease of the pod of the requester asked for on
// the socket of the lease claim claimUID is Ready, at most wait. It returns
// the lease's response, and with withFd the device of a dax or block lease
// of the requester container whose fd goes with it.
func (b *Broker) waitLease(claimUID string, who Requester, id string, wait time.Duration, withFd bool) (*api.Response, leaseFd) {
	l := ownLease(b.records(), claimUID, who, id)
	switch {
	case withFd && (l == nil || l.Container != who.Container):
		return &api.Response{Lease: id, Error: "no such lease of this container"}, leaseFd{}
	case l == nil:
		return &api.Response{Lease: id, Error: "no such lease of this pod"}, leaseFd{}
	case withFd && l.Asked.Mode != state.ModeDax && l.Asked.Mode != state.ModeBlock:
		resp := leaseInfo(l)
		resp.Error = fmt.Sprintf("lease %s is not a %s or %s lease; use %s", id, api.ModeDax, api.ModeBlock, api.OpEnv)
		return resp, leaseFd{}
	}
	resp, fd := b.waitReady(id, wait, false)
	if !withFd {
		fd = leaseFd{}
	}
	return resp, fd
}
