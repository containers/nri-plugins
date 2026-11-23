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

// Package api defines the JSON types and the routes of the fake-cxl-pool
// REST API v1. The server and the client share these definitions.
package api

import (
	"strings"
	"time"
)

// ServerVersion is the version string reported by the server.
const ServerVersion = "fake-cxl-pool/v1.0"

// Status is the server status (GET /api/v1/status).
type Status struct {
	// Name is the name of the server, the DRA pool name of the fake pool
	// controller.
	Name        string     `json:"name"`
	Version     string     `json:"version"`
	Qemu        QemuStatus `json:"qemu"`
	Hosts       int        `json:"hosts"`
	Devices     int        `json:"devices"`
	Attachments int        `json:"attachments"`
	Uptime      string     `json:"uptime"`
	Started     time.Time  `json:"started"`
	StateFile   string     `json:"stateFile,omitempty"`
}

// QemuStatus summarizes the qemu processes the server controls.
type QemuStatus struct {
	Versions map[string]string `json:"versions,omitempty"` // host name -> qemu version
}

// Host states.
const (
	HostRunning     = "running"
	HostStopped     = "stopped"
	HostUnreachable = "unreachable"
)

// Host is a memory consumer: a qemu VM.
type Host struct {
	Name        string `json:"name"`
	UUID        string `json:"uuid"` // SMBIOS system UUID (qemu -uuid), "" if not set
	PID         int    `json:"pid"`
	State       string `json:"state"`
	Control     string `json:"control"`
	QMP         string `json:"qmp"`
	HMP         string `json:"hmp"`
	QemuVersion string `json:"qemuVersion"`
	// HotRemoveCapable is false for qemu builds that cannot complete
	// hot-removal from cxl-downstream ports (stock qemu: the device stays
	// as a zombie and its slot and backend can never be reused).
	HotRemoveCapable bool         `json:"hotRemoveCapable"`
	Error            string       `json:"error,omitempty"`
	HostBridges      []HostBridge `json:"hostBridges"`
	Slots            []Slot       `json:"slots"`
	Attachments      []Attachment `json:"attachments"`
	LocalDevices     []string     `json:"localDevices"`
}

// NormUUID returns a uuid in lower case without dashes, so that a SMBIOS
// uuid matches /etc/machine-id and the systemUUID of a Node.
func NormUUID(u string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(u), "-", ""))
}

// HostBridge is a CXL host bridge (pxb-cxl) of a host.
type HostBridge struct {
	ID            string `json:"id"`
	NUMANode      int    `json:"numaNode"`
	FMWSize       int64  `json:"fmwSize"`
	AttachedBytes int64  `json:"attachedBytes"`
}

// Slot kinds.
const (
	SlotDownstream = "downstream"
	SlotRootPort   = "rootport"
)

// Slot is a place where a cxl-type3 device can be plugged in.
type Slot struct {
	Bus        string `json:"bus"`
	Kind       string `json:"kind"`
	HostBridge string `json:"hostBridge"`
	NUMANode   int    `json:"numaNode"`
	Device     string `json:"device"`
	Attachment string `json:"attachment"`
	// LocalDevice is the local device whose pre-declared backend names this
	// slot (beram_/befile_..._bus_<slot>_...). The server gives such slots
	// to other devices only when no other slot is free.
	LocalDevice string `json:"localDevice,omitempty"`
}

// Device backends.
const (
	BackendFile = "file"
	BackendRAM  = "ram"
)

// Device scopes.
const (
	ScopePool  = "pool"
	ScopeLocal = "local"
)

// Device states.
const (
	DeviceFree      = "free"
	DeviceAttached  = "attached"
	DeviceAttaching = "attaching"
	DeviceDetaching = "detaching"
	DeviceError     = "error"
)

// Device is a pool memory device: a backing file or a ram object.
type Device struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	Size   int64  `json:"size"`
	Shared bool   `json:"shared"`
	// Persistent devices are persistent memory with a label storage
	// area (LSA) file at LSAPath; others are volatile memory.
	Persistent  bool         `json:"persistent"`
	Backend     string       `json:"backend"`
	Path        string       `json:"path,omitempty"`
	LSAPath     string       `json:"lsaPath,omitempty"`
	Scope       string       `json:"scope"`
	LocalHost   string       `json:"localHost,omitempty"`
	Static      bool         `json:"static,omitempty"`
	State       string       `json:"state"`
	Attachments []Attachment `json:"attachments"`
	Created     time.Time    `json:"created"`
}

// DeviceCreate is the body of POST /api/v1/devices.
type DeviceCreate struct {
	Name       string `json:"name,omitempty"`
	Size       Size   `json:"size"`
	Shared     bool   `json:"shared,omitempty"`
	Persistent bool   `json:"persistent,omitempty"`
	Serial     string `json:"serial,omitempty"`
}

// AttachRequest is the body of POST /api/v1/devices/{name}/attachments.
type AttachRequest struct {
	Host     string `json:"host"`
	Slot     string `json:"slot,omitempty"`
	NUMANode *int   `json:"numaNode,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	// SingleHost attaches a shared device to this host only: the attach
	// fails while the device is attached to another host, and attaching it
	// to another host fails while this attachment is single host. An attach
	// of an attached device sets the flag of the attachment.
	SingleHost bool `json:"singleHost,omitempty"`
}

// Attachment states.
const (
	AttachmentAttaching = "attaching"
	AttachmentAttached  = "attached"
	AttachmentDetaching = "detaching"
	AttachmentDetached  = "detached"
	AttachmentFailed    = "failed"
)

// Attachment is the hotplug state of a device in a host.
type Attachment struct {
	ID           string `json:"id"`
	Device       string `json:"device"`
	Host         string `json:"host"`
	Serial       string `json:"serial"`
	Slot         Slot   `json:"slot"`
	QemuDeviceID string `json:"qemuDeviceID"`
	QemuObjectID string `json:"qemuObjectID"`
	// QemuLSAObjectID is the label storage area backend of a persistent
	// device, "" for volatile devices.
	QemuLSAObjectID string `json:"qemuLSAObjectID,omitempty"`
	State           string `json:"state"`
	Error           string `json:"error,omitempty"`
	// Owner is the owner given in the attach request that created the
	// attachment ("" if none, or adopted). An attachment without owner
	// takes the owner of the first attach request that has one.
	Owner string `json:"owner,omitempty"`
	// SingleHost is the flag of the last attach request: no other host may
	// attach the device meanwhile.
	SingleHost bool      `json:"singleHost,omitempty"`
	Adopted    bool      `json:"adopted,omitempty"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
}

// AttachmentID returns the id of the attachment of a device to a host.
func AttachmentID(device, host string) string {
	return device + "@" + host
}

// Event types.
const (
	EventAttachmentUpdated = "attachment.updated"
	EventDeviceUpdated     = "device.updated"
	EventHostUpdated       = "host.updated"
)

// Event is one item of GET /api/v1/events (text/event-stream).
type Event struct {
	Type   string    `json:"type"`
	Time   time.Time `json:"time"`
	Object any       `json:"object"`
}
