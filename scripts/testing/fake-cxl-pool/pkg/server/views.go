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
	"path"
	"sort"
	"strconv"
	"unicode"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
)

// naturalLess compares strings so that embedded numbers compare by value
// (ds2 < ds10).
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		ca, cb := rune(a[0]), rune(b[0])
		if unicode.IsDigit(ca) && unicode.IsDigit(cb) {
			i := 0
			for i < len(a) && unicode.IsDigit(rune(a[i])) {
				i++
			}
			j := 0
			for j < len(b) && unicode.IsDigit(rune(b[j])) {
				j++
			}
			na, _ := strconv.ParseUint(a[:i], 10, 64)
			nb, _ := strconv.ParseUint(b[:j], 10, 64)
			if na != nb {
				return na < nb
			}
			a, b = a[i:], b[j:]
			continue
		}
		if ca != cb {
			return ca < cb
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func sortedDevices(m map[string]*device) []*device {
	out := make([]*device, 0, len(m))
	for _, d := range m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[i].Name, out[j].Name) })
	return out
}

func sortedHosts(m map[string]*host) []*host {
	out := make([]*host, 0, len(m))
	for _, h := range m {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[i].name, out[j].name) })
	return out
}

func sortedAttachments(m map[string]*attachment) []*attachment {
	out := make([]*attachment, 0, len(m))
	for _, a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[i].ID, out[j].ID) })
	return out
}

// deviceAttachmentsLocked returns the attachments of a device.
func (s *Server) deviceAttachmentsLocked(name string) []*attachment {
	var out []*attachment
	for _, a := range sortedAttachments(s.atts) {
		if a.Device == name {
			out = append(out, a)
		}
	}
	return out
}

func (s *Server) hostAttachmentsLocked(name string) []*attachment {
	var out []*attachment
	for _, a := range sortedAttachments(s.atts) {
		if a.Host == name {
			out = append(out, a)
		}
	}
	return out
}

// deviceState computes the state of a device from its attachments. A
// failed attachment makes an unshared device "error" (it cannot be given to
// anyone until that qemu restarts); for a shared device it only affects that
// attachment, unless it is the only one.
func deviceState(shared bool, atts []*attachment) string {
	state := api.DeviceFree
	failed, others := false, 0
	for _, a := range atts {
		if a.State == api.AttachmentFailed {
			failed = true
		} else {
			others++
		}
	}
	if failed && (!shared || others == 0) {
		return api.DeviceError
	}
	for _, a := range atts {
		switch a.State {
		case api.AttachmentDetaching:
			return api.DeviceDetaching
		case api.AttachmentAttaching:
			state = api.DeviceAttaching
		case api.AttachmentAttached:
			if state == api.DeviceFree {
				state = api.DeviceAttached
			}
		}
	}
	return state
}

func (s *Server) deviceViewLocked(d *device) api.Device {
	v := d.Device
	atts := s.deviceAttachmentsLocked(d.Name)
	v.State = deviceState(d.Shared, atts)
	v.Attachments = make([]api.Attachment, 0, len(atts))
	for _, a := range atts {
		v.Attachments = append(v.Attachments, a.Attachment)
	}
	return v
}

// hostNUMALocked returns the NUMA node of a host bridge, -1 if unknown.
func hostNUMALocked(h *host, hb string) int {
	if h.tree != nil {
		if t := h.tree.HostBridge(hb); t != nil {
			return t.NUMANode
		}
	}
	return -1
}

// slotLocked returns the Slot of a bus in a host.
func (s *Server) slotLocked(h *host, bus string) api.Slot {
	sl := api.Slot{Bus: bus, NUMANode: -1}
	if h.tree != nil {
		if p := h.tree.Port(bus); p != nil {
			sl = portSlot(h, p)
		}
	}
	for _, d := range s.devices {
		if b := d.localTo(h.name); b != nil && b.Bus == bus {
			sl.LocalDevice = d.Name
		}
	}
	for _, a := range s.atts {
		if a.Host == h.name && a.Slot.Bus == bus && a.State != api.AttachmentFailed {
			sl.Attachment = a.ID
			if sl.Device == "" {
				sl.Device = a.QemuDeviceID
			}
		}
	}
	return sl
}

func portSlot(h *host, p *qemu.Port) api.Slot {
	kind := api.SlotDownstream
	if p.Kind == qemu.PortRootPort {
		kind = api.SlotRootPort
	}
	return api.Slot{
		Bus:        p.Bus,
		Kind:       kind,
		HostBridge: p.HostBridge,
		NUMANode:   hostNUMALocked(h, p.HostBridge),
		Device:     p.Occupant(),
	}
}

// slotKnownLocked returns true if a slot bus matches the slot patterns of
// discovery, or there are none.
func (s *Server) slotKnownLocked(bus string) bool {
	pats := s.cfg.Discovery.Slots
	if len(pats) == 0 {
		return true
	}
	for _, pat := range pats {
		if ok, _ := path.Match(pat, bus); ok {
			return true
		}
	}
	return false
}

// slotsLocked returns the slots of a host that match the slot patterns, in
// natural order.
func (s *Server) slotsLocked(h *host) []api.Slot {
	out := []api.Slot{}
	if h.tree == nil {
		return out
	}
	var ports []*qemu.Port
	for i := range h.tree.Ports {
		if h.tree.Ports[i].IsSlot() && s.slotKnownLocked(h.tree.Ports[i].Bus) {
			ports = append(ports, &h.tree.Ports[i])
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].HostBridge != ports[j].HostBridge {
			return naturalLess(ports[i].HostBridge, ports[j].HostBridge)
		}
		return naturalLess(ports[i].Bus, ports[j].Bus)
	})
	for _, p := range ports {
		out = append(out, s.slotLocked(h, p.Bus))
	}
	return out
}

func (s *Server) hostViewLocked(h *host) api.Host {
	v := api.Host{
		Name:             h.name,
		UUID:             h.uuid,
		PID:              h.pid,
		State:            h.state,
		Control:          h.control,
		QMP:              h.qmpPath,
		HMP:              h.hmpPath,
		QemuVersion:      h.version,
		HotRemoveCapable: h.hotRemove,
		Error:            h.lastErr,
		HostBridges:      []api.HostBridge{},
		Slots:            s.slotsLocked(h),
		Attachments:      []api.Attachment{},
		LocalDevices:     []string{},
	}
	atts := s.hostAttachmentsLocked(h.name)
	for _, a := range atts {
		v.Attachments = append(v.Attachments, a.Attachment)
	}
	var hbIDs []string
	if h.tree != nil {
		for _, hb := range h.tree.HostBridges {
			hbIDs = append(hbIDs, hb.ID)
		}
	}
	sort.Slice(hbIDs, func(i, j int) bool { return naturalLess(hbIDs[i], hbIDs[j]) })
	for _, id := range hbIDs {
		hb := api.HostBridge{ID: id, NUMANode: hostNUMALocked(h, id), FMWSize: fmwSizeLocked(h, id), AttachedBytes: s.attachedBytesLocked(h, id)}
		v.HostBridges = append(v.HostBridges, hb)
	}
	for _, d := range sortedDevices(s.devices) {
		if d.localTo(h.name) != nil {
			v.LocalDevices = append(v.LocalDevices, d.Name)
		}
	}
	return v
}
