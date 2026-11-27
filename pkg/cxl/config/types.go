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

// Package config defines MemoryConfig, the opaque configuration of
// ResourceClaims and DeviceClasses for the CXL DRA drivers (cxl.nri.io,
// cxl-pool.nri.io) and their pool controllers, with its resolution per
// allocation result, its combination per container, the defaults of the
// driver configuration and the rules of the device section per device
// type. A MemoryConfig has three sections by owner: pool (the pool
// controller), device (the kubelet plugin of the device) and container
// (the NRI plugin).
package config

import (
	"encoding/json"

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

const (
	// APIVersion is the apiVersion of MemoryConfig for every driver.
	APIVersion = "cxl.nri.io/v1alpha1"
	// Kind is the kind of MemoryConfig.
	Kind = "MemoryConfig"
)

// The values of container.steering.policy.
const (
	// PolicyNone does not steer the container.
	PolicyNone = "none"
	// PolicyAuto steers by the memory types of the container: CXL only
	// first-cxl, DRAM and CXL start-interleaved, DRAM only first-dram.
	PolicyAuto = "auto"
	// PolicyFirstDRAM consumes the DRAM quota first, then CXL.
	PolicyFirstDRAM = "first-dram"
	// PolicyFirstCXL consumes the CXL quota first, then DRAM.
	PolicyFirstCXL = "first-cxl"
	// PolicyStartInterleaved interleaves DRAM and CXL until the smaller
	// quota is used, then consumes the rest of the other.
	PolicyStartInterleaved = "start-interleaved"
	// PolicyEndInterleaved consumes the excess of the larger quota first,
	// then interleaves DRAM and CXL.
	PolicyEndInterleaved = "end-interleaved"
	// PolicyWaypoints follows container.steering.waypoints.
	PolicyWaypoints = "waypoints"
)

// policies are the values of container.steering.policy in the order of
// the messages.
var policies = []string{PolicyNone, PolicyAuto, PolicyFirstDRAM, PolicyFirstCXL,
	PolicyStartInterleaved, PolicyEndInterleaved, PolicyWaypoints}

// The values of container.fencing.
const (
	// FencingAllowedMems keeps the CXL NUMA nodes of the driver out of
	// cpuset.mems of a container that has not claimed or leased memory
	// on them.
	FencingAllowedMems = "allowed-mems"
	// FencingNone leaves the memory nodes of a container as they are.
	FencingNone = "none"
)

// The memory types of waypoints.
const (
	MemoryTypeDRAM = "DRAM"
	MemoryTypeCXL  = "CXL"
)

// The paths of the fields of the container section, the keys of origins.
const (
	FieldPolicy    = "container.steering.policy"
	FieldWaypoints = "container.steering.waypoints"
	FieldMinStep   = "container.steering.minStep"
	FieldMaxStep   = "container.steering.maxStep"
	FieldFencing   = "container.fencing"
)

// MemoryConfig is how the claimed memory is attached by the pool
// controller (pool), prepared on the node (device) and used by the
// containers of the request (container).
type MemoryConfig struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Device     *Device    `json:"device,omitempty"`
	Container  *Container `json:"container,omitempty"`
	Pool       *Pool      `json:"pool,omitempty"`
}

// Device is how each device of a request is prepared.
type Device struct {
	// Mode is "numa", "dax", "block" or "fs"; "" is the default of the
	// device type.
	Mode string `json:"mode,omitempty"`
	// ReadOnly makes the dax or block device node of the container
	// read-only, or the filesystem of mode fs read-only (true) or
	// read-write (false). Absent is read-only of mode fs and false of the
	// other modes: read it with IsReadOnly.
	ReadOnly *bool `json:"readOnly,omitempty"`
	// Access is "direct" or "cached" of mode fs; "" is direct when the
	// filesystem has DAX, cached otherwise.
	Access string `json:"access,omitempty"`
	// Path is the directory of the filesystem of mode fs in the container.
	Path string `json:"path,omitempty"`
	// FSType is the filesystem type of mode fs; "" is the detected type.
	FSType string `json:"fsType,omitempty"`
}

// IsReadOnly returns whether the device is read-only: readOnly when it is
// set, else true for mode fs and false for the other modes.
func (d Device) IsReadOnly() bool {
	if d.ReadOnly == nil {
		return d.Mode == api.ModeFS
	}
	return *d.ReadOnly
}

// Container is how a container that uses a request is managed.
type Container struct {
	Steering *Steering `json:"steering,omitempty"`
	// Fencing is "allowed-mems" or "none"; set by the driver
	// configuration only.
	Fencing string `json:"fencing,omitempty"`
}

// Steering is how the allocations of a container are spread over the DRAM
// and CXL memory of its results.
type Steering struct {
	// Policy is one of the Policy* values.
	Policy string `json:"policy,omitempty"`
	// Waypoints are the waypoints of policy waypoints.
	Waypoints []Waypoint `json:"waypoints,omitempty"`
	// MinStep is the smallest amount of new allocations after which the
	// route is re-evaluated (a quantity).
	MinStep string `json:"minStep,omitempty"`
	// MaxStep is the largest amount of new allocations after which the
	// route is re-evaluated (a quantity).
	MaxStep string `json:"maxStep,omitempty"`
}

// Waypoint is one waypoint of policy waypoints. A memory type that is not
// listed keeps its usage of the previous waypoint.
type Waypoint struct {
	TargetUsages []TargetUsage `json:"targetUsages"`
}

// TargetUsage is the target usage of one memory type at a waypoint.
type TargetUsage struct {
	// MemoryType is "DRAM" or "CXL".
	MemoryType string `json:"memoryType"`
	// Usage is a quantity such as "4Gi".
	Usage string `json:"usage"`
}

// Pool is how the pool controller attaches a device of a request.
type Pool struct {
	// Opaque has the settings of each pool controller implementation by
	// its controller key; the value is an object that the controller
	// defines.
	Opaque map[string]json.RawMessage `json:"opaque,omitempty"`
}

// DeviceSection returns the device section, the zero Device when there is
// none.
func (c *MemoryConfig) DeviceSection() Device {
	if c == nil || c.Device == nil {
		return Device{}
	}
	return *c.Device
}

// Compact returns the sections of the configuration as compact JSON,
// without apiVersion and kind and without empty sections.
func (c *MemoryConfig) Compact() string {
	if c == nil {
		return "{}"
	}
	sections := struct {
		Device    *Device    `json:"device,omitempty"`
		Container *Container `json:"container,omitempty"`
		Pool      *Pool      `json:"pool,omitempty"`
	}{c.Device, c.Container, c.Pool}
	data, err := json.Marshal(sections)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// normalize removes the sections and objects that set nothing.
func (c *MemoryConfig) normalize() {
	if c.Device != nil && *c.Device == (Device{}) {
		c.Device = nil
	}
	c.Container = c.Container.normalized()
	if c.Pool != nil && len(c.Pool.Opaque) == 0 {
		c.Pool = nil
	}
}

// normalized returns the container section without the objects that set
// nothing, nil if it sets nothing.
func (c *Container) normalized() *Container {
	if c == nil {
		return nil
	}
	out := *c
	if s := out.Steering; s != nil {
		steering := *s
		if len(steering.Waypoints) == 0 {
			steering.Waypoints = nil
		}
		out.Steering = &steering
		if steering.isZero() {
			out.Steering = nil
		}
	}
	if out.Steering == nil && out.Fencing == "" {
		return nil
	}
	return &out
}

// isZero tells whether the steering sets no field.
func (s *Steering) isZero() bool {
	return s.Policy == "" && len(s.Waypoints) == 0 && s.MinStep == "" && s.MaxStep == ""
}
