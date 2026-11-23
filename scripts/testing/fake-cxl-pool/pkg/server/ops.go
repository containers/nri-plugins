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

package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
)

// Status returns the server status.
func (s *Server) Status() api.Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := api.Status{
		Name:        s.cfg.Name,
		Version:     api.ServerVersion,
		Qemu:        api.QemuStatus{Versions: map[string]string{}},
		Hosts:       len(s.hosts),
		Devices:     len(s.devices),
		Attachments: len(s.atts),
		Uptime:      time.Since(s.started).Round(time.Second).String(),
		Started:     s.started.UTC(),
	}
	if s.persistenceEnabled() {
		st.StateFile = s.cfg.StateFile
	}
	for _, h := range s.hosts {
		if h.version != "" {
			st.Qemu.Versions[h.name] = h.version
		}
	}
	return st
}

// Hosts lists hosts. Device trees are queried first.
func (s *Server) Hosts(ctx context.Context) []api.Host {
	ctx = context.WithoutCancel(ctx) // a client disconnect must not fail monitor calls
	s.refreshTrees(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []api.Host{}
	for _, h := range sortedHosts(s.hosts) {
		out = append(out, s.hostViewLocked(h))
	}
	return out
}

// Host returns a host by name or uuid.
func (s *Server) Host(ctx context.Context, ref string) (api.Host, error) {
	s.mu.RLock()
	h := s.lookupHostLocked(ref)
	s.mu.RUnlock()
	if h == nil {
		return api.Host{}, api.NotFound("host %q not found", ref)
	}
	s.refreshTrees(context.WithoutCancel(ctx), h.name)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hostViewLocked(h), nil
}

// Resolve finds the host of a client by uuid and/or hostname.
func (s *Server) Resolve(hostname, uuid string) (api.Host, error) {
	if hostname == "" && uuid == "" {
		return api.Host{}, api.InvalidArgument("hostname or uuid required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.resolveHostLocked(hostname, uuid)
	if h == nil {
		return api.Host{}, api.NotFound("no host matches hostname %q uuid %q", hostname, uuid)
	}
	return s.hostViewLocked(h), nil
}

// Devices lists the devices of a scope ("" for all).
func (s *Server) Devices(scope string) []api.Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []api.Device{}
	for _, d := range sortedDevices(s.devices) {
		if scope == "" || d.Scope == scope {
			out = append(out, s.deviceViewLocked(d))
		}
	}
	return out
}

// Device returns a device.
func (s *Server) Device(name string) (api.Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[name]
	if d == nil {
		return api.Device{}, api.NotFound("device %q not found", name)
	}
	return s.deviceViewLocked(d), nil
}

// CreateDevice creates a pool device and its backing file.
func (s *Server) CreateDevice(req api.DeviceCreate) (api.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size := int64(req.Size)
	if size <= 0 {
		return api.Device{}, api.InvalidArgument("size is required")
	}
	if size%CapacityMultiplier != 0 {
		return api.Device{}, api.InvalidArgument("size %s is not a multiple of %s (CXL capacity unit)", api.FormatSize(size), api.FormatSize(CapacityMultiplier))
	}
	name := req.Name
	if name == "" {
		for i := 0; ; i++ {
			name = "dev" + strconv.Itoa(i)
			if _, ok := s.devices[name]; !ok {
				break
			}
		}
	}
	if err := validName(name); err != nil {
		return api.Device{}, api.InvalidArgument("%v", err)
	}
	if _, ok := s.devices[name]; ok {
		return api.Device{}, api.Conflict("device %q already exists", name)
	}
	var sn uint64
	if req.Serial != "" {
		v, err := cxlapi.ParseSerial(req.Serial)
		if err != nil {
			return api.Device{}, api.InvalidArgument("%v", err)
		}
		if ld := s.localDeviceWithSerialLocked(v, ""); ld != nil {
			return api.Device{}, api.Conflict("serial 0x%x is used by local device %q", v, ld.Name)
		}
		if err := s.serials.Use(v, name); err != nil {
			return api.Device{}, api.Conflict("%v", err)
		}
		sn = v
	} else {
		sn = s.serials.NextExcept(name, req.Shared, s.localSerialLocked)
	}
	path := s.filePath(name)
	d := &device{
		Device: api.Device{
			Name:       name,
			Serial:     cxlapi.FormatSerial(sn),
			Size:       size,
			Shared:     req.Shared,
			Persistent: req.Persistent,
			Backend:    api.BackendFile,
			Path:       path,
			Scope:      api.ScopePool,
			Created:    time.Now().UTC(),
		},
		serial:  sn,
		dynamic: true,
	}
	if req.Persistent {
		d.LSAPath = lsaPath(path)
	}
	if err := ensureFiles(d.Path, d.LSAPath, d.Size); err != nil {
		s.serials.Free(sn)
		removeDeviceFiles(d, s.logf)
		return api.Device{}, api.Internal("cannot create backing file: %v", err)
	}
	s.devices[name] = d
	if d.Persistent {
		s.logf("device %s: created %s shared=%v persistent serial %s file %s lsa %s", name, api.FormatSize(size), req.Shared, d.Serial, path, d.LSAPath)
	} else {
		s.logf("device %s: created %s shared=%v serial %s file %s", name, api.FormatSize(size), req.Shared, d.Serial, path)
	}
	s.saveStateLocked()
	v := s.deviceViewLocked(d)
	s.events.publish(api.EventDeviceUpdated, v)
	return v, nil
}

// DeleteDevice deletes a device created over the API and its backing files.
// With force, attachments are detached first.
func (s *Server) DeleteDevice(ctx context.Context, name string, force bool) error {
	s.mu.RLock()
	d := s.devices[name]
	if d == nil {
		s.mu.RUnlock()
		return api.NotFound("device %q not found", name)
	}
	if !d.dynamic {
		s.mu.RUnlock()
		if d.Static {
			return api.Conflict("device %q is defined in the config file", name)
		}
		return api.Conflict("device %q is a local device of a qemu command line", name)
	}
	atts := s.deviceAttachmentsLocked(name)
	s.mu.RUnlock()
	if len(atts) > 0 {
		if !force {
			return api.Conflict("device %q is attached to %d host(s), detach first or use force", name, len(atts))
		}
		for _, a := range atts {
			if _, _, err := s.Detach(ctx, name, a.Host, DetachOptions{Timeout: time.Duration(s.cfg.DetachTimeout), Force: true}); err != nil {
				return err
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.devices[name] != d {
		return api.NotFound("device %q not found", name)
	}
	if len(s.deviceAttachmentsLocked(name)) > 0 {
		return api.Conflict("device %q was attached again", name)
	}
	delete(s.devices, name)
	if filepath.Dir(d.Path) == filepath.Clean(s.cfg.Dir) {
		removeDeviceFiles(d, s.logf)
	}
	s.serials.Free(d.serial)
	s.logf("device %s: deleted", name)
	s.saveStateLocked()
	v := d.Device
	v.State = "deleted"
	s.events.publish(api.EventDeviceUpdated, v)
	return nil
}

// AttachmentFilter filters attachment lists.
type AttachmentFilter struct {
	Host   string
	Device string
}

// Attachments lists attachments.
func (s *Server) Attachments(f AttachmentFilter) ([]api.Attachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hostName := ""
	if f.Host != "" {
		h := s.lookupHostLocked(f.Host)
		if h == nil {
			return nil, api.NotFound("host %q not found", f.Host)
		}
		hostName = h.name
	}
	out := []api.Attachment{}
	for _, a := range sortedAttachments(s.atts) {
		if hostName != "" && a.Host != hostName {
			continue
		}
		if f.Device != "" && a.Device != f.Device {
			continue
		}
		out = append(out, a.Attachment)
	}
	return out, nil
}

// AttachResult tells how an attach request ended.
type AttachResult int

const (
	// AttachExisting: the device was already attached (200).
	AttachExisting AttachResult = iota
	// AttachCreated: the device was attached now (201).
	AttachCreated
	// AttachAccepted: attaching continues in the background (202).
	AttachAccepted
)

// checkAttachLocked checks the sharing rules.
func (s *Server) checkAttachLocked(d *device, h *host, req api.AttachRequest) error {
	for _, a := range s.deviceAttachmentsLocked(d.Name) {
		switch {
		case a.Host == h.name:
		case !d.Shared:
			return api.Conflict("device %q is not shared and it is attached to %q", d.Name, a.Host)
		case req.SingleHost:
			return api.Conflict("device %q is attached to %q: a single-host attach needs it attached nowhere else", d.Name, a.Host)
		case a.SingleHost:
			return api.Conflict("device %q has a single-host attachment to %q", d.Name, a.Host)
		}
	}
	if d.Scope == api.ScopeLocal {
		if d.localTo(h.name) == nil {
			return api.InvalidArgument("device %q is a local device of host %q", d.Name, d.LocalHost)
		}
		return nil
	}
	// the guest finds devices by serial: it must be unique in the VM
	if ld := s.localDeviceWithSerialLocked(d.serial, h.name); ld != nil {
		return api.Conflict("device %q has the serial %s of local device %q of host %q", d.Name, d.Serial, ld.Name, h.name)
	}
	return nil
}

// fmwSizeLocked returns the fixed memory window size of a host bridge, 0
// if unknown.
func fmwSizeLocked(h *host, hb string) int64 {
	return h.fmw[hb]
}

// attachedBytesLocked returns the size of the devices attached under a
// host bridge (leaked ones included: their guest address range stays
// reserved).
func (s *Server) attachedBytesLocked(h *host, hb string) int64 {
	var total int64
	for _, a := range s.atts {
		if a.Host == h.name && a.Slot.HostBridge == hb {
			if d := s.devices[a.Device]; d != nil {
				total += d.Size
			}
		}
	}
	return total
}

// chooseSlotLocked picks the slot for an attachment of a device of size.
func (s *Server) chooseSlotLocked(h *host, b *binding, req api.AttachRequest, size int64) (string, error) {
	busy := map[string]bool{}
	for _, a := range s.atts {
		if a.Host == h.name && a.State != api.AttachmentFailed {
			busy[a.Slot.Bus] = true
		}
	}
	fits := func(p *qemu.Port) bool {
		fmw := fmwSizeLocked(h, p.HostBridge)
		return fmw == 0 || s.attachedBytesLocked(h, p.HostBridge)+size <= fmw
	}
	free := func(p *qemu.Port) bool { return p.Occupant() == "" && !busy[p.Bus] }
	checkFits := func(p *qemu.Port) error {
		if !fits(p) {
			return api.Conflict("host bridge %s of host %q has no room for %s more: fixed memory window %s, attached %s",
				p.HostBridge, h.name, api.FormatSize(size), api.FormatSize(fmwSizeLocked(h, p.HostBridge)),
				api.FormatSize(s.attachedBytesLocked(h, p.HostBridge)))
		}
		return nil
	}
	if req.Slot != "" {
		p := h.tree.Port(req.Slot)
		if p == nil || !p.IsSlot() || !s.slotKnownLocked(p.Bus) {
			return "", api.InvalidArgument("host %q has no CXL slot %q", h.name, req.Slot)
		}
		if !free(p) {
			return "", api.Conflict("slot %q of host %q is occupied by %q", req.Slot, h.name, s.slotLocked(h, req.Slot).Device)
		}
		return p.Bus, checkFits(p)
	}
	if b != nil && b.Bus != "" {
		p := h.tree.Port(b.Bus)
		if p == nil || !p.IsSlot() || !s.slotKnownLocked(p.Bus) {
			return "", api.InvalidArgument("host %q has no CXL slot %q for its local device", h.name, b.Bus)
		}
		if !free(p) {
			return "", api.Conflict("slot %q of host %q is occupied by %q", b.Bus, h.name, s.slotLocked(h, b.Bus).Device)
		}
		return p.Bus, checkFits(p)
	}
	// Prefer slots that are not the default slot of an unplugged local
	// device, and slots on the requested NUMA node.
	reserved := map[string]bool{}
	for _, d := range s.devices {
		if lb := d.localTo(h.name); lb != nil {
			reserved[lb.Bus] = true
		}
	}
	best, bestScore := "", -1
	full := false
	for _, sl := range s.slotsLocked(h) {
		p := h.tree.Port(sl.Bus)
		if p == nil || !free(p) {
			continue
		}
		if !fits(p) {
			full = true
			continue
		}
		// a slot reserved for a local device only if nothing else is
		// free; then the requested NUMA node
		score := 1
		if !reserved[p.Bus] {
			score += 4
		}
		if req.NUMANode != nil && sl.NUMANode == *req.NUMANode {
			score += 2
		}
		if score > bestScore {
			best, bestScore = p.Bus, score
		}
	}
	if best == "" {
		if full {
			return "", api.Conflict("host %q has no free CXL slot under a host bridge with room for %s", h.name, api.FormatSize(size))
		}
		return "", api.Conflict("host %q has no free CXL slot", h.name)
	}
	return best, nil
}

// Attach hotplugs a device to a host.
func (s *Server) Attach(ctx context.Context, name string, req api.AttachRequest) (api.Attachment, AttachResult, error) {
	if req.Host == "" {
		return api.Attachment{}, 0, api.InvalidArgument("host is required")
	}
	timeout := DefaultAttachTimeout
	if req.Timeout != "" {
		t, err := time.ParseDuration(req.Timeout)
		if err != nil {
			return api.Attachment{}, 0, api.InvalidArgument("invalid timeout %q", req.Timeout)
		}
		timeout = t
	}

	s.mu.Lock()
	d := s.devices[name]
	if d == nil {
		s.mu.Unlock()
		return api.Attachment{}, 0, api.NotFound("device %q not found", name)
	}
	h := s.lookupHostLocked(req.Host)
	if h == nil {
		s.mu.Unlock()
		return api.Attachment{}, 0, api.NotFound("host %q not found", req.Host)
	}
	if existing, ok, err := s.existingAttachmentLocked(d, h, req); ok || err != nil {
		if err == nil {
			err = s.updateExistingLocked(d, h, existing, req)
		}
		att, state := existing.Attachment, existing.State // copy under the lock
		s.mu.Unlock()
		if err != nil || state == api.AttachmentAttached {
			return att, AttachExisting, err
		}
		// attaching
		return s.waitAttaching(ctx, existing, timeout)
	}
	if err := s.checkAttachLocked(d, h, req); err != nil {
		s.mu.Unlock()
		return api.Attachment{}, 0, err
	}
	if h.state == api.HostStopped || h.mon == nil {
		s.mu.Unlock()
		return api.Attachment{}, 0, api.Unavailable("host %q is not running (%s)", h.name, h.state)
	}
	mon := h.mon
	s.mu.Unlock()

	// phase 1: pick a slot from a fresh tree and reserve it
	h.opMu.Lock()
	s.mu.RLock()
	gen := h.gen
	s.mu.RUnlock()
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	tree, err := mon.QueryTree(qctx)
	cancel()
	if err != nil {
		h.opMu.Unlock()
		return api.Attachment{}, 0, api.Unavailable("host %q: cannot query qemu: %v", h.name, err)
	}
	s.mu.Lock()
	if h.mon != mon {
		s.mu.Unlock()
		h.opMu.Unlock()
		return api.Attachment{}, 0, api.Unavailable("host %q: qemu monitor changed (VM restarted?)", h.name)
	}
	if s.qemuGoneLocked(h) {
		s.mu.Unlock()
		h.opMu.Unlock()
		return api.Attachment{}, 0, api.Unavailable("host %q is not running (%s): its qemu process is gone", h.name, h.state)
	}
	// the slot choice needs this tree even if attachments changed while it
	// was read (they are in s.atts); reconcile skips it in that case
	s.storeTreeLocked(h, tree, gen, true)
	if existing, ok, err := s.existingAttachmentLocked(d, h, req); ok || err != nil {
		if err == nil {
			err = s.updateExistingLocked(d, h, existing, req)
		}
		att := existing.Attachment
		s.mu.Unlock()
		h.opMu.Unlock()
		if err != nil {
			return api.Attachment{}, 0, err
		}
		return att, AttachExisting, nil
	}
	if s.devices[name] != d {
		s.mu.Unlock()
		h.opMu.Unlock()
		return api.Attachment{}, 0, api.NotFound("device %q not found", name)
	}
	if err := s.checkAttachLocked(d, h, req); err != nil {
		s.mu.Unlock()
		h.opMu.Unlock()
		return api.Attachment{}, 0, err
	}
	b := d.localTo(h.name)
	bus, err := s.chooseSlotLocked(h, b, req, d.Size)
	if err != nil {
		s.mu.Unlock()
		h.opMu.Unlock()
		return api.Attachment{}, 0, err
	}
	h.counter++
	qemuID := fmt.Sprintf("fcp_%s.hp%d", d.Name, h.counter)
	objID, serial := qemuID, d.serial
	// backends are the objects to add: the data, and of a persistent
	// device its label storage area
	var backends []qemu.MemoryBackend
	if b != nil {
		objID, serial = b.ObjectID, b.Serial
	} else {
		// the host can map the backing file with 2 MiB pages
		backends = append(backends, qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: objID, Size: uint64(d.Size), MemPath: d.Path, Share: true,
			Align: qemu.HugePageAlign})
	}
	t3 := qemu.CXLType3{ID: qemuID, Bus: bus, Serial: serial}
	if d.Persistent {
		lsa := qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: qemu.LSAObjectID(objID), Size: lsaSize, MemPath: d.LSAPath, Share: true}
		backends = append(backends, lsa)
		t3.PersistentMemdev, t3.LSA = objID, lsa.ID
	} else {
		t3.VolatileMemdev = objID
	}
	now := time.Now().UTC()
	a := &attachment{
		Attachment: api.Attachment{
			ID:              api.AttachmentID(d.Name, h.name),
			Device:          d.Name,
			Host:            h.name,
			Serial:          cxlapi.FormatSerial(serial),
			Slot:            s.slotLocked(h, bus),
			QemuDeviceID:    qemuID,
			QemuObjectID:    objID,
			QemuLSAObjectID: t3.LSA,
			State:           api.AttachmentAttaching,
			Owner:           req.Owner,
			SingleHost:      req.SingleHost,
			Created:         now,
			Updated:         now,
		},
		done: make(chan struct{}),
	}
	s.atts[a.ID] = a
	a.Slot.Attachment = a.ID
	a.Slot.Device = qemuID
	if t3.LSA != "" {
		s.logf("attachment %s: attaching to slot %s as %s (persistent backend %s, lsa %s, serial %s)", a.ID, bus, qemuID, objID, t3.LSA, a.Serial)
	} else {
		s.logf("attachment %s: attaching to slot %s as %s (backend %s, serial %s)", a.ID, bus, qemuID, objID, a.Serial)
	}
	s.saveStateLocked()
	s.publishAttachmentLocked(a)
	path, lsa, size := d.Path, d.LSAPath, d.Size
	s.mu.Unlock()
	h.opMu.Unlock()

	// phase 2: object-add + device_add
	run := func(ctx context.Context) error {
		h.opMu.Lock()
		defer h.opMu.Unlock()
		var err error
		if len(backends) > 0 {
			err = ensureFiles(path, lsa, size)
		}
		// Before every command and after the device_add: the qemu of the
		// host still runs. A qemu that restarted after the tree was read
		// answers on the same socket, and the next discovery would drop a
		// device plugged into it as the old qemu's and adopt it without
		// owner. A new qemu listens only after the old one has exited, so
		// what was added before a failed check went to the old qemu and is
		// gone with it: no object_del, and the monitor of a stopped host is
		// closed anyway.
		stopped := false
		check := func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			err := s.attachStoppedLocked(h, a)
			stopped = err != nil
			return err
		}
		var added []string
		for _, be := range backends {
			if err == nil {
				err = check()
			}
			if err != nil {
				break
			}
			if err = mon.ObjectAdd(ctx, be); err == nil {
				added = append(added, be.ID)
			}
		}
		if err == nil {
			err = check()
		}
		if err == nil {
			err = mon.DeviceAdd(ctx, t3)
		}
		if err != nil && !stopped {
			for _, id := range added {
				if derr := mon.ObjectDel(context.WithoutCancel(ctx), id); derr != nil {
					s.logf("attachment %s: object-del %s after failed device_add: %v", a.ID, id, derr)
				}
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		a.Updated = time.Now().UTC()
		if err == nil {
			// plugged; a qemu that exited meanwhile took the device with it
			err = s.attachStoppedLocked(h, a)
		}
		if err != nil {
			a.State = api.AttachmentFailed
			a.Error = err.Error()
			if s.atts[a.ID] == a {
				delete(s.atts, a.ID)
			}
			s.logf("attachment %s: failed: %v", a.ID, err)
		} else {
			a.State = api.AttachmentAttached
			s.logf("attachment %s: attached", a.ID)
		}
		close(a.done)
		s.saveStateLocked()
		s.publishAttachmentLocked(a)
		return err
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer rcancel()
	if err := run(rctx); err != nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return a.Attachment, 0, &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("attach %s to %s failed: %v", d.Name, h.name, err), Object: a.Attachment}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return a.Attachment, AttachCreated, nil
}

// attachStoppedLocked returns an error if the host of an attachment that is
// being attached stopped: the attachment was dropped meanwhile (the qemu of
// the host stopped or restarted), or the qemu process of the host is gone
// now (qemuGoneLocked, which stops the host and drops the attachment).
func (s *Server) attachStoppedLocked(h *host, a *attachment) error {
	switch {
	case s.atts[a.ID] != a:
		return fmt.Errorf("host %s stopped or restarted during the attach", h.name)
	case s.qemuGoneLocked(h):
		return fmt.Errorf("host %s stopped during the attach: its qemu process is gone", h.name)
	}
	return nil
}

// updateExistingLocked updates an existing attachment for an attach
// request. The single-host flag becomes that of the request: a single-host
// attachment needs the device attached to no other host. An attachment
// without owner (adopted, or attached without one) gets the owner of the
// request: the owner takes it over, and fake-cxl-pool-controller detaches
// only the attachments that it owns.
func (s *Server) updateExistingLocked(d *device, h *host, a *attachment, req api.AttachRequest) error {
	changed := false
	if a.SingleHost != req.SingleHost {
		if req.SingleHost {
			for _, o := range s.deviceAttachmentsLocked(d.Name) {
				if o.Host != h.name {
					return api.Conflict("device %q is attached to %q: a single-host attach needs it attached nowhere else", d.Name, o.Host)
				}
			}
		}
		a.SingleHost = req.SingleHost
		s.logf("attachment %s: single host %v", a.ID, a.SingleHost)
		changed = true
	}
	if a.Owner == "" && req.Owner != "" {
		a.Owner = req.Owner
		s.logf("attachment %s: taken over by owner %s (it had none, adopted %v)", a.ID, a.Owner, a.Adopted)
		changed = true
	}
	if !changed {
		return nil
	}
	a.Updated = time.Now().UTC()
	s.saveStateLocked()
	s.publishAttachmentLocked(a)
	return nil
}

// existingAttachmentLocked returns the existing attachment of the device to
// the host, and an error if it is in a state that does not allow attach.
func (s *Server) existingAttachmentLocked(d *device, h *host, req api.AttachRequest) (*attachment, bool, error) {
	a := s.atts[api.AttachmentID(d.Name, h.name)]
	if a == nil {
		return nil, false, nil
	}
	switch a.State {
	case api.AttachmentAttached, api.AttachmentAttaching:
		if req.Slot != "" && req.Slot != a.Slot.Bus {
			return a, true, api.Conflict("device %q is already attached to %q in slot %q", d.Name, h.name, a.Slot.Bus)
		}
		return a, true, nil
	case api.AttachmentDetaching:
		return a, true, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("device %q is being detached from %q", d.Name, h.name), Object: a.Attachment}
	}
	return a, true, api.Conflict("device %q: attachment to %q is %s", d.Name, h.name, a.State)
}

func (s *Server) waitAttaching(ctx context.Context, a *attachment, timeout time.Duration) (api.Attachment, AttachResult, error) {
	s.mu.RLock()
	done := a.done
	s.mu.RUnlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(timeout):
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch a.State {
	case api.AttachmentAttached:
		return a.Attachment, AttachExisting, nil
	case api.AttachmentAttaching:
		return a.Attachment, AttachAccepted, nil
	}
	return a.Attachment, 0, &api.Error{Code: api.CodeUnavailable, Message: "attach failed: " + a.Error, Object: a.Attachment}
}

// DetachOptions are the options of a detach request. Force forgets a
// failed attachment.
type DetachOptions struct {
	Timeout time.Duration
	Force   bool
}

// DetachResult tells how a detach request ended.
type DetachResult int

const (
	// DetachDone: the device is gone from qemu (200).
	DetachDone DetachResult = iota
	// DetachAccepted: detaching continues in the background (202).
	DetachAccepted
)

// Detach hot-removes a device from a host. It waits for qemu to delete the
// device until Timeout, and returns a Conflict error with the attachment
// (state failed) if that did not happen; waiting continues in the
// background.
func (s *Server) Detach(ctx context.Context, name, hostRef string, opts DetachOptions) (api.Attachment, DetachResult, error) {
	// The detach must not depend on the client connection: a client that
	// gives up must not fake a timeout or skip the leak check and object-del.
	// Every wait below is bounded by opts.Timeout or a command timeout.
	ctx = context.WithoutCancel(ctx)
	if opts.Timeout <= 0 {
		opts.Timeout = time.Duration(s.cfg.DetachTimeout)
	}
	s.mu.Lock()
	d := s.devices[name]
	if d == nil {
		s.mu.Unlock()
		return api.Attachment{}, 0, api.NotFound("device %q not found", name)
	}
	h := s.lookupHostLocked(hostRef)
	if h == nil {
		s.mu.Unlock()
		return api.Attachment{}, 0, api.NotFound("host %q not found", hostRef)
	}
	a := s.atts[api.AttachmentID(d.Name, h.name)]
	if a == nil {
		s.mu.Unlock()
		return api.Attachment{}, 0, api.NotFound("device %q is not attached to %q", name, h.name)
	}
	att := a.Attachment // copy under the lock
	if a.State == api.AttachmentAttaching {
		s.mu.Unlock()
		return att, 0, api.Conflict("device %q is being attached to %q", name, h.name)
	}
	if a.State == api.AttachmentFailed {
		defer s.mu.Unlock()
		if !opts.Force {
			return att, 0, &api.Error{Code: api.CodeConflict, Object: att,
				Message: fmt.Sprintf("attachment %s failed: %s; use force to forget it", a.ID, a.Error)}
		}
		delete(s.atts, a.ID)
		a.State = api.AttachmentDetached
		a.Updated = time.Now().UTC()
		s.logf("attachment %s: forgotten (force)", a.ID)
		s.saveStateLocked()
		s.publishAttachmentLocked(a)
		return a.Attachment, DetachDone, nil
	}
	mon := h.mon
	if mon == nil {
		s.mu.Unlock()
		return att, 0, api.Unavailable("host %q has no monitor (%s)", h.name, h.state)
	}
	qemuID := a.QemuDeviceID
	already := a.State == api.AttachmentDetaching
	if !already {
		a.State = api.AttachmentDetaching
		a.Updated = time.Now().UTC()
		a.Error = ""
		s.logf("attachment %s: detaching qemu device %s", a.ID, qemuID)
		s.saveStateLocked()
		s.publishAttachmentLocked(a)
	}
	s.mu.Unlock()
	// Never send device_del twice: qemu refuses it while an unplug is
	// pending, and there is no way to cancel or repeat the guest's part.

	if !already {
		h.opMu.Lock()
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := mon.DeviceDel(dctx, qemuID)
		cancel()
		h.opMu.Unlock()
		switch {
		case err == nil, qemu.IsUnplugInProgress(err):
			s.mu.Lock()
			a.unplugPending = true
			if h.unplugged == nil {
				h.unplugged = map[string]bool{}
			}
			h.unplugged[qemuID] = true
			s.saveStateLocked()
			s.mu.Unlock()
		case qemu.IsNotFound(err):
			// already gone from qemu: still check that the guest does not
			// keep its memory
			return s.detachResult(a, name, h.name, s.completeDetach(ctx, a))
		default:
			s.mu.Lock()
			a.State = api.AttachmentAttached
			a.Error = "device_del: " + err.Error()
			a.Updated = time.Now().UTC()
			s.saveStateLocked()
			s.publishAttachmentLocked(a)
			att = a.Attachment
			s.mu.Unlock()
			return att, 0, api.Unavailable("detach %s from %s: %v", name, h.name, err)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	err := mon.WaitDeviceDeleted(wctx, qemuID)
	cancel()
	if err == nil {
		return s.detachResult(a, name, h.name, s.completeDetach(ctx, a))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.atts[a.ID] != a {
		// finalized meanwhile by reconcile or a background waiter
		return a.Attachment, DetachDone, nil
	}
	if a.State != api.AttachmentFailed {
		s.markTimedOutLocked(a, opts.Timeout)
	}
	s.startBackgroundWaitLocked(a, 0)
	return a.Attachment, 0, &api.Error{
		Code:    api.CodeConflict,
		Message: fmt.Sprintf("detach %s from %s: %s", name, h.name, a.Error),
		Object:  a.Attachment,
	}
}

// detachResult turns the result of completeDetach into the reply of a
// detach request.
func (s *Server) detachResult(a *attachment, name, host string, res completeResult) (api.Attachment, DetachResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch res {
	case completeLeaked:
		return a.Attachment, 0, &api.Error{Code: api.CodeConflict, Object: a.Attachment,
			Message: fmt.Sprintf("detach %s from %s: %s", name, host, a.Error)}
	case completeUnknown:
		if s.atts[a.ID] == a {
			// could not check the backend: finish in the background
			s.startBackgroundWaitLocked(a, 0)
			return a.Attachment, DetachAccepted, nil
		}
	}
	return a.Attachment, DetachDone, nil
}
