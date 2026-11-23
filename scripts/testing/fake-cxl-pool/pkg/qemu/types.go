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

// Package qemu controls CXL memory hotplug of running qemu processes over
// QMP or HMP monitor sockets, and discovers qemu processes from /proc.
package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Logf is a printf-like logging function. A nil Logf discards.
type Logf func(format string, args ...any)

func (l Logf) printf(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}

const (
	// DefaultCommandTimeout limits a monitor command when the context
	// has no deadline.
	DefaultCommandTimeout = 10 * time.Second
	// DefaultPollInterval is the interval of polling the device tree
	// while waiting for a device to be deleted over HMP.
	DefaultPollInterval = 500 * time.Millisecond
)

func withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, DefaultCommandTimeout)
}

// Monitor protocols.
const (
	ProtoQMP = "qmp"
	ProtoHMP = "hmp"
)

// Monitor controls one qemu process. Implementations are safe for
// concurrent use.
type Monitor interface {
	// Version returns the qemu version string.
	Version(ctx context.Context) (string, error)
	// ObjectAdd creates a memory backend object.
	ObjectAdd(ctx context.Context, b MemoryBackend) error
	// ObjectDel deletes an object.
	ObjectDel(ctx context.Context, id string) error
	// DeviceAdd hotplugs a cxl-type3 device.
	DeviceAdd(ctx context.Context, d CXLType3) error
	// DeviceDel requests hot-removal of a device. Removal completes
	// asynchronously, when the guest has released the device.
	DeviceDel(ctx context.Context, id string) error
	// WaitDeviceDeleted waits until the device is gone from qemu, or the
	// context is done.
	WaitDeviceDeleted(ctx context.Context, id string) error
	// DeviceExists returns true if the device is in qemu.
	DeviceExists(ctx context.Context, id string) (bool, error)
	// BackendMapped returns true if a CXL HDM decoder still maps the
	// memory backend into the guest (see BackendMappedIn).
	BackendMapped(ctx context.Context, objID string) (bool, error)
	// QomGet returns a QOM property value as JSON.
	QomGet(ctx context.Context, path, property string) (json.RawMessage, error)
	// QueryMemdevs lists memory backend objects.
	QueryMemdevs(ctx context.Context) ([]Memdev, error)
	// QueryTree returns the CXL topology of the qemu device tree.
	QueryTree(ctx context.Context) (*Tree, error)
	// Close releases the monitor.
	Close()
}

// Memory backend object types.
const (
	MemoryBackendFile = "memory-backend-file"
	MemoryBackendRAM  = "memory-backend-ram"
)

// MemoryBackend is a memory backend object to be created with ObjectAdd.
type MemoryBackend struct {
	QomType string // MemoryBackendFile or MemoryBackendRAM
	ID      string
	Size    uint64
	MemPath string // memory-backend-file only
	Share   bool
	// Align is the alignment of qemu's mapping of the backend in bytes,
	// 0 for qemu's default (memory-backend-file only).
	Align uint64
}

// HugePageAlign is the alignment of the mapping of a backend that lets
// the host map its file with 2 MiB pages (a tmpfs with huge=).
const HugePageAlign = 2 << 20

// LSAObjectID returns the id of the label storage area backend that
// belongs to the memory backend dataID of a persistent device.
func LSAObjectID(dataID string) string { return dataID + "-lsa" }

// CXLType3 is a cxl-type3 device to be hotplugged with DeviceAdd. A
// volatile device has VolatileMemdev, a persistent device PersistentMemdev
// and LSA.
type CXLType3 struct {
	ID               string
	Bus              string
	VolatileMemdev   string // memory backend object id
	PersistentMemdev string // memory backend object id
	LSA              string // label storage area backend object id
	Serial           uint64
}

// Memdev returns the memory backend object id of the device memory.
func (d CXLType3) Memdev() string {
	if d.PersistentMemdev != "" {
		return d.PersistentMemdev
	}
	return d.VolatileMemdev
}

// Memdev is a memory backend object as reported by query-memdev.
type Memdev struct {
	ID    string `json:"id"`
	Size  uint64 `json:"size"`
	Share bool   `json:"share"`
}

// Tree is the CXL part of the qemu device tree.
type Tree struct {
	HostBridges []TreeHostBridge
	Ports       []Port
	Type3       []Type3Device
}

// TreeHostBridge is a pxb-cxl host bridge.
type TreeHostBridge struct {
	ID       string // pxb-cxl id == name of its bus
	NUMANode int    // -1 if unknown
}

// Port kinds.
const (
	PortDownstream = "downstream"
	PortRootPort   = "rootport"
)

// Port is a cxl-downstream or cxl-rp device and the bus it provides.
type Port struct {
	Bus        string // == port device id
	Kind       string // PortDownstream or PortRootPort
	HostBridge string
	// Children are the devices on the bus of the port.
	Children []TreeDevice
}

// TreeDevice is a device on a bus.
type TreeDevice struct {
	Driver string
	ID     string
}

// IsSlot returns true if the port can host a cxl-type3: it is a downstream
// port, or a root port without a switch below it.
func (p *Port) IsSlot() bool {
	if p.Kind == PortDownstream {
		return true
	}
	for _, c := range p.Children {
		if c.Driver != "cxl-type3" {
			return false
		}
	}
	return true
}

// Occupant returns the id of the first device on the port's bus, "" if
// the bus is empty. Devices without an id are reported as "<driver>".
func (p *Port) Occupant() string {
	if len(p.Children) == 0 {
		return ""
	}
	c := p.Children[0]
	if c.ID == "" {
		return "<" + c.Driver + ">"
	}
	return c.ID
}

// Type3Device is a cxl-type3 device in the tree. Object ids are without
// /objects/, "" if unknown or not set.
type Type3Device struct {
	ID               string
	Bus              string
	VolatileMemdev   string
	PersistentMemdev string
	LSA              string
	Serial           uint64
	HasSerial        bool
}

// Memdev returns the memory backend object id of the device memory: the
// persistent memdev if set, else the volatile memdev.
func (d *Type3Device) Memdev() string {
	if d.PersistentMemdev != "" {
		return d.PersistentMemdev
	}
	return d.VolatileMemdev
}

// Port returns the port by bus name, nil if not found.
func (t *Tree) Port(bus string) *Port {
	for i := range t.Ports {
		if t.Ports[i].Bus == bus {
			return &t.Ports[i]
		}
	}
	return nil
}

// HostBridge returns the host bridge by id, nil if not found.
func (t *Tree) HostBridge(id string) *TreeHostBridge {
	for i := range t.HostBridges {
		if t.HostBridges[i].ID == id {
			return &t.HostBridges[i]
		}
	}
	return nil
}

// ErrClosed is returned by a monitor after Close.
var ErrClosed = errors.New("qemu monitor closed")

// numaUnassigned is qemu's NUMA_NODE_UNASSIGNED (MAX_NODES): the default
// numa_node of a pxb-cxl without numa_node=.
const numaUnassigned = 128

func numaOrUnknown(n int) int {
	if n < 0 || n >= numaUnassigned {
		return -1
	}
	return n
}

// Error is an error reported by qemu.
type Error struct {
	Class   string // QMP error class, "" for HMP
	Desc    string
	Command string
}

func (e *Error) Error() string {
	if e.Class != "" && e.Class != "GenericError" {
		return "qemu: " + e.Command + ": " + e.Class + ": " + e.Desc
	}
	return "qemu: " + e.Command + ": " + e.Desc
}

// IsNotFound returns true if err is a qemu error about a missing device or
// object.
func IsNotFound(err error) bool {
	e, ok := errors.AsType[*Error](err)
	if !ok {
		return false
	}
	return e.Class == "DeviceNotFound" || strings.Contains(e.Desc, "not found")
}

// IsUnplugInProgress returns true if err tells that device_del of the
// device is already pending.
func IsUnplugInProgress(err error) bool {
	e, ok := errors.AsType[*Error](err)
	if !ok {
		return false
	}
	return strings.Contains(e.Desc, "in progress") || strings.Contains(e.Desc, "process of unplug")
}

// FMWWindow is an element of qom-get /machine cxl-fmw.
type FMWWindow struct {
	Targets []string `json:"targets"`
	Size    int64    `json:"size"`
}

// QueryFMW returns the CXL fixed memory windows of a qemu.
func QueryFMW(ctx context.Context, m Monitor) ([]FMWWindow, error) {
	raw, err := m.QomGet(ctx, "/machine", "cxl-fmw")
	if err != nil {
		return nil, err
	}
	var ws []FMWWindow
	if err := json.Unmarshal(raw, &ws); err != nil {
		return nil, err
	}
	return ws, nil
}

// HotRemoveCapable tells if the qemu can complete hot-removal of a
// cxl-type3 from a cxl-downstream port: the patched build gives
// cxl-downstream a power controller (property power_controller_present),
// stock qemu keeps removed devices in the tree forever. It returns
// (false, nil) for stock qemu, and (true, nil) if the tree has no
// downstream ports (root ports always have a power controller).
func HotRemoveCapable(ctx context.Context, m Monitor, t *Tree) (bool, error) {
	for _, p := range t.Ports {
		if p.Kind != PortDownstream {
			continue
		}
		raw, err := m.QomGet(ctx, "/machine/peripheral/"+p.Bus, "power_controller_present")
		if err != nil {
			if IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return false, err
		}
		return v, nil
	}
	return true, nil
}

var (
	_ Monitor = (*HMP)(nil)
	_ Monitor = (*QMP)(nil)
)
