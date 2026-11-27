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

// Package fake has an in-memory qemu for the unit tests of the users of
// package qemu.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
)

// Qemu is an in-memory qemu.Monitor for unit tests. It models the parts of
// qemu and of the guest that matter for hotplug: device and object id
// uniqueness, busy backends, slot occupancy and a guest that holds a device
// until it is released.
type Qemu struct {
	mu          sync.Mutex
	version     string
	hostBridges []qemu.TreeHostBridge
	ports       []qemu.Port
	objects     map[string]qemu.MemoryBackend
	devices     map[string]qemu.CXLType3
	usedIDs     map[string]bool // qemu never reuses a device id
	mapped      map[string]bool // backend in use by a device
	pendingDel  map[string]bool
	held        map[string]bool // guest holds the device: device_del does not complete
	stock       bool
	failAdd     error
	unreachable bool
	log         []string
	deletedCh   map[string]chan struct{}
	// leaked backends stay mapped after their device is gone (the guest
	// kept the memory online)
	leaked        map[string]bool
	leakOnRelease bool
	fmw           []qemu.FMWWindow
	// noSerial are the devices plugged without a serial property
	noSerial map[string]bool
}

// NewQemu returns a fake monitor with the given host bridges (id ->
// numa node) and slots (bus -> host bridge, all downstream ports).
func NewQemu(hostBridges map[string]int, slots map[string]string) *Qemu {
	f := &Qemu{
		version:    "11.1.1 fake",
		objects:    map[string]qemu.MemoryBackend{},
		devices:    map[string]qemu.CXLType3{},
		usedIDs:    map[string]bool{},
		mapped:     map[string]bool{},
		pendingDel: map[string]bool{},
		held:       map[string]bool{},
		deletedCh:  map[string]chan struct{}{},
		leaked:     map[string]bool{},
		noSerial:   map[string]bool{},
	}
	for id, numa := range hostBridges {
		f.hostBridges = append(f.hostBridges, qemu.TreeHostBridge{ID: id, NUMANode: numa})
	}
	sort.Slice(f.hostBridges, func(i, j int) bool { return f.hostBridges[i].ID < f.hostBridges[j].ID })
	for bus, hb := range slots {
		f.ports = append(f.ports, qemu.Port{Bus: bus, Kind: qemu.PortDownstream, HostBridge: hb})
	}
	sort.Slice(f.ports, func(i, j int) bool { return f.ports[i].Bus < f.ports[j].Bus })
	return f
}

// SetStock makes the fake emulate unpatched qemu: a deleted cxl-type3
// stays in the tree forever (and its backend stays mapped).
func (f *Qemu) SetStock(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stock = on
}

// FailNextDeviceAdd makes the next DeviceAdd return err.
func (f *Qemu) FailNextDeviceAdd(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAdd = err
}

// SetUnreachable makes every call fail.
func (f *Qemu) SetUnreachable(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreachable = on
}

// SetLeakOnRelease makes Release of a held device remove the device but
// leave its backend mapped (patched qemu, guest memory stuck online).
func (f *Qemu) SetLeakOnRelease(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leakOnRelease = on
}

// SetFMW sets the fixed memory windows that QomGet /machine cxl-fmw
// reports.
func (f *Qemu) SetFMW(ws []qemu.FMWWindow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fmw = ws
}

// AddObject pre-declares a memory backend (like -object on the command line).
func (f *Qemu) AddObject(b qemu.MemoryBackend) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[b.ID] = b
}

// PlugDevice plugs a device as if done by someone else (or at boot).
func (f *Qemu) PlugDevice(d qemu.CXLType3) error {
	return f.DeviceAdd(context.Background(), d)
}

// PlugDeviceWithoutSerial plugs a device like PlugDevice, but the device
// tree shows it without a serial (device_add without serial=).
func (f *Qemu) PlugDeviceWithoutSerial(d qemu.CXLType3) error {
	d.Serial = 0
	if err := f.DeviceAdd(context.Background(), d); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noSerial[d.ID] = true
	return nil
}

// Hold makes the guest hold the device: device_del will not complete until
// Release.
func (f *Qemu) Hold(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[id] = true
}

// Release releases a device in the guest. A pending device_del completes.
func (f *Qemu) Release(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, id)
	if f.pendingDel[id] {
		if f.leakOnRelease {
			f.leaked[f.devices[id].Memdev()] = true
		}
		f.completeDelLocked(id)
	}
}

// BackendMapped implements qemu.Monitor.
func (f *Qemu) BackendMapped(ctx context.Context, objID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return false, err
	}
	return f.leaked[objID], nil
}

// HasDevice returns true if the device is plugged.
func (f *Qemu) HasDevice(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.devices[id]
	return ok
}

// HasObject returns true if the object exists.
func (f *Qemu) HasObject(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[id]
	return ok
}

// Commands returns the log of mutating commands.
func (f *Qemu) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *Qemu) check() error {
	if f.unreachable {
		return fmt.Errorf("fake monitor unreachable")
	}
	return nil
}

// Version implements qemu.Monitor.
func (f *Qemu) Version(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return "", err
	}
	return f.version, nil
}

// ObjectAdd implements qemu.Monitor.
func (f *Qemu) ObjectAdd(ctx context.Context, b qemu.MemoryBackend) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return err
	}
	f.log = append(f.log, qemu.HMPObjectAdd(b))
	if _, ok := f.objects[b.ID]; ok {
		return &qemu.Error{Command: "object_add", Desc: fmt.Sprintf("attempt to add duplicate property '%s' to object (type 'container')", b.ID)}
	}
	f.objects[b.ID] = b
	return nil
}

// ObjectDel implements qemu.Monitor.
func (f *Qemu) ObjectDel(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return err
	}
	f.log = append(f.log, "object_del "+id)
	if _, ok := f.objects[id]; !ok {
		return &qemu.Error{Command: "object_del", Desc: fmt.Sprintf("object '%s' not found", id)}
	}
	if f.mapped[id] {
		return &qemu.Error{Command: "object_del", Desc: fmt.Sprintf("object '%s' is in use, can not be deleted", id)}
	}
	delete(f.objects, id)
	return nil
}

// DeviceAdd implements qemu.Monitor.
func (f *Qemu) DeviceAdd(ctx context.Context, d qemu.CXLType3) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return err
	}
	f.log = append(f.log, qemu.HMPDeviceAdd(d))
	if err := f.failAdd; err != nil {
		f.failAdd = nil
		return err
	}
	if f.usedIDs[d.ID] {
		return &qemu.Error{Command: "device_add", Desc: fmt.Sprintf("Duplicate device ID '%s'", d.ID)}
	}
	var port *qemu.Port
	for i := range f.ports {
		if f.ports[i].Bus == d.Bus {
			port = &f.ports[i]
		}
	}
	if port == nil {
		return &qemu.Error{Command: "device_add", Desc: fmt.Sprintf("Bus '%s' not found", d.Bus)}
	}
	if len(port.Children) > 0 {
		return &qemu.Error{Command: "device_add", Desc: "PCI: slot 0 function 0 already occupied by cxl-type3, new func cxl-type3 cannot be exposed to guest."}
	}
	backends, err := f.checkBackendsLocked(d)
	if err != nil {
		return err
	}
	f.usedIDs[d.ID] = true
	for _, b := range backends {
		f.mapped[b] = true
	}
	f.devices[d.ID] = d
	port.Children = []qemu.TreeDevice{{Driver: "cxl-type3", ID: d.ID}}
	f.deletedCh[d.ID] = make(chan struct{})
	return nil
}

// checkBackendsLocked returns the backend object ids of a cxl-type3, or
// the error qemu gives for missing, unknown or busy backends.
func (f *Qemu) checkBackendsLocked(d qemu.CXLType3) ([]string, error) {
	if d.VolatileMemdev == "" && d.PersistentMemdev == "" {
		return nil, &qemu.Error{Command: "device_add", Desc: "at least one memdev property must be set"}
	}
	if d.PersistentMemdev != "" && d.LSA == "" {
		return nil, &qemu.Error{Command: "device_add", Desc: "lsa property must be set for persistent devices"}
	}
	var backends []string
	for _, p := range []struct{ prop, id string }{
		{"volatile-memdev", d.VolatileMemdev},
		{"persistent-memdev", d.PersistentMemdev},
		{"lsa", d.LSA},
	} {
		if p.id == "" {
			continue
		}
		if _, ok := f.objects[p.id]; !ok {
			return nil, &qemu.Error{Command: "device_add", Desc: fmt.Sprintf("Property 'cxl-type3.%s' can't find value '%s'", p.prop, p.id)}
		}
		if f.mapped[p.id] || slices.Contains(backends, p.id) {
			return nil, &qemu.Error{Command: "device_add", Desc: fmt.Sprintf("memory backend %s can't be used multiple times.", p.id)}
		}
		backends = append(backends, p.id)
	}
	return backends, nil
}

// DeviceDel implements qemu.Monitor.
func (f *Qemu) DeviceDel(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return err
	}
	f.log = append(f.log, "device_del "+id)
	if _, ok := f.devices[id]; !ok {
		return &qemu.Error{Command: "device_del", Class: "DeviceNotFound", Desc: fmt.Sprintf("Device '%s' not found", id)}
	}
	if f.pendingDel[id] {
		return &qemu.Error{Command: "device_del", Desc: fmt.Sprintf("Device %s is already in the process of unplug", id)}
	}
	f.pendingDel[id] = true
	if !f.held[id] {
		f.completeDelLocked(id)
	}
	return nil
}

func (f *Qemu) completeDelLocked(id string) {
	if f.stock {
		// the guest has released it, but qemu keeps the device
		return
	}
	d := f.devices[id]
	delete(f.devices, id)
	delete(f.pendingDel, id)
	for _, b := range []string{d.VolatileMemdev, d.PersistentMemdev, d.LSA} {
		if b != "" && !f.leaked[b] {
			delete(f.mapped, b)
		}
	}
	for i := range f.ports {
		if f.ports[i].Bus == d.Bus {
			f.ports[i].Children = nil
		}
	}
	if ch := f.deletedCh[id]; ch != nil {
		close(ch)
		delete(f.deletedCh, id)
	}
}

// QomGet implements qemu.Monitor for /machine cxl-fmw and
// power_controller_present of ports.
func (f *Qemu) QomGet(ctx context.Context, path, property string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return nil, err
	}
	switch {
	case path == "/machine" && property == "cxl-fmw":
		b, _ := json.Marshal(f.fmw)
		return b, nil
	case property == "power_controller_present":
		if f.stock {
			return nil, &qemu.Error{Command: "qom-get", Desc: "Property 'cxl-downstream.power_controller_present' not found"}
		}
		return json.RawMessage("true"), nil
	}
	return nil, &qemu.Error{Command: "qom-get", Desc: fmt.Sprintf("Property '%s' not found", property)}
}

// DeviceExists implements qemu.Monitor.
func (f *Qemu) DeviceExists(ctx context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return false, err
	}
	_, ok := f.devices[id]
	return ok, nil
}

// WaitDeviceDeleted implements qemu.Monitor.
func (f *Qemu) WaitDeviceDeleted(ctx context.Context, id string) error {
	f.mu.Lock()
	_, ok := f.devices[id]
	ch := f.deletedCh[id]
	f.mu.Unlock()
	if !ok || ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// QueryMemdevs implements qemu.Monitor.
func (f *Qemu) QueryMemdevs(ctx context.Context) ([]qemu.Memdev, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return nil, err
	}
	var out []qemu.Memdev
	for _, o := range f.objects {
		out = append(out, qemu.Memdev{ID: o.ID, Size: o.Size, Share: o.Share})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// QueryTree implements qemu.Monitor.
func (f *Qemu) QueryTree(ctx context.Context) (*qemu.Tree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return nil, err
	}
	t := &qemu.Tree{HostBridges: append([]qemu.TreeHostBridge(nil), f.hostBridges...)}
	for _, p := range f.ports {
		p.Children = append([]qemu.TreeDevice(nil), p.Children...)
		t.Ports = append(t.Ports, p)
	}
	for _, d := range f.devices {
		t.Type3 = append(t.Type3, qemu.Type3Device{ID: d.ID, Bus: d.Bus, VolatileMemdev: d.VolatileMemdev,
			PersistentMemdev: d.PersistentMemdev, LSA: d.LSA, Serial: d.Serial, HasSerial: !f.noSerial[d.ID]})
	}
	sort.Slice(t.Type3, func(i, j int) bool { return t.Type3[i].ID < t.Type3[j].ID })
	return t, nil
}

// Close implements qemu.Monitor.
func (f *Qemu) Close() {}

// String summarizes the fake state.
func (f *Qemu) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return "fake{devices: " + strings.Join(ids, ",") + "}"
}

var _ qemu.Monitor = (*Qemu)(nil)
