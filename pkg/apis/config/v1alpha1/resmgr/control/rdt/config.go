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

package rdt

import (
	"encoding/json"
	"fmt"
	"log/slog"

	grcpath "github.com/intel/goresctrl/pkg/path"
	"github.com/intel/goresctrl/pkg/rdt"
)

var (
	// ErrConfigConversion is an error returned if we can't convert our configuration
	// to a goresctrl native representation (goresctrl/pkg/rdt.Config).
	ErrConfigConversion = fmt.Errorf("failed to convert to native goresctrl configuration")
)

var (
	// Expose goresctrl/rdt functions for configuration via this package.
	SetPrefix  func(string)                  = grcpath.SetPrefix
	Initialize func(string) error            = rdt.Initialize
	SetLogger  func(*slog.Logger)            = rdt.SetLogger
	SetConfig  func(*rdt.Config, bool) error = rdt.SetConfig

	// And some that we need for other plumbing.
	NewCollector                     = rdt.NewCollector
	RegisterOpenTelemetryInstruments = rdt.RegisterOpenTelemetryInstruments
)

// Config provides runtime configuration for class based cache allocation
// and memory bandwidth control.
// +kubebuilder:object:generate=true
type Config struct {
	// Enable class based cache allocation and memory bandwidth control.
	// When enabled, policy implementations can adjust cache allocation
	// and memory bandwidth by assigning containers to RDT classes.
	// +optional
	Enable bool `json:"enable,omitempty"`
	// usePodQoSAsDefaultClass controls whether a container's Pod QoS
	// class is used as its RDT class, if this is otherwise unset.
	// +optional
	UsePodQoSAsDefaultClass bool `json:"usePodQoSAsDefaultClass,omitempty"`
	// Options container common goresctrl/rdt settings.
	Options *Options `json:"options,omitempty"`
	// Partitions configure cache partitions.
	Partitions map[string]PartitionConfig `json:"partitions,omitempty"`
	// Force indicates if the configuration should be forced to goresctrl.
	Force bool `json:"force,omitempty"`
}

// +kubebuilder:object:generate=true
// PartitionConfig provides configuration for a single cache partition.
type PartitionConfig struct {
	L2Allocation CatConfig              `json:"l2Allocation,omitempty"`
	L3Allocation CatConfig              `json:"l3Allocation,omitempty"`
	MBAllocation MbaConfig              `json:"mbAllocation,omitempty"`
	Classes      map[string]ClassConfig `json:"classes,omitempty"`
}

// +kubebuilder:object:generate=true
// ClassConfig provides configuration for a single named cache CLOS/class.
type ClassConfig struct {
	L2Allocation CatConfig `json:"l2Allocation,omitempty"`
	L3Allocation CatConfig `json:"l3Allocation,omitempty"`
	MBAllocation MbaConfig `json:"mbAllocation,omitempty"`
}

// CatConfig contains the L2 or L3 cache allocation configuration for one partition or class.
type CatConfig map[string]CacheIdCatConfig

// MbaConfig contains the memory bandwidth configuration for one partition or class.
type MbaConfig map[string]CacheIdMbaConfig

// +kubebuilder:object:generate=true
// CacheIdCatConfig is the cache allocation configuration for one cache id.
// Code and Data represent an optional configuration for separate code and data
// paths and only have effect when RDT CDP (Code and Data Prioritization) is
// enabled in the system. Code and Data go in tandem so that both or neither
// must be specified - only specifying the other is considered a configuration
// error.
//
//	TODO(klihub): Ideally we'd have a validation rule ensuring that either
//	unified or code+data are set here. I tried that using a CEL-expression
//	but couldn't avoid hitting the complexity estimation limit (even with
//	extra MaxProperties limits thrown in). Maybe we'll be able to do that
//	eventually with https://github.com/kubernetes-sigs/controller-tools/pull/1212
type CacheIdCatConfig struct {
	Unified CacheProportion `json:"unified,omitempty"`
	Code    CacheProportion `json:"code,omitempty"`
	Data    CacheProportion `json:"data,omitempty"`
}

// CacheIdMbaConfig is the memory bandwidth configuration for one cache id.
// It's an array of at most two values, specifying separate values to be used
// for percentage based and MBps based memory bandwidth allocation. For
// example, `{"80%", "1000MBps"}` would allocate 80% if percentage based
// allocation is used by the Linux kernel, or 1000 MBps in case MBps based
// allocation is in use.
type CacheIdMbaConfig []MbProportion

// MbProportion specifies a share of available memory bandwidth. It's an
// integer value followed by a unit. Two units are supported:
//
// - percentage, e.g. `80%`
// - MBps, e.g. `1000MBps`
type MbProportion string

// CacheProportion specifies a share of the available cache lines.
// Supported formats:
//
// - percentage, e.g. `50%`
// - percentage range, e.g. `50-60%`
// - bit numbers, e.g. `0-5`, `2,3`, must contain one contiguous block of bits set
// - hex bitmask, e.g. `0xff0`, must contain one contiguous block of bits set
type CacheProportion string

// +kubebuilder:object:generate=true
// Options contains common settings.
type Options struct {
	L2 CatOptions `json:"l2,omitempty"`
	L3 CatOptions `json:"l3,omitempty"`
	MB MbOptions  `json:"mb,omitempty"`
}

// +kubebuilder:object:generate=true
// CatOptions contains the common settings for cache allocation.
type CatOptions struct {
	Optional bool `json:"optional"`
}

// +kubebuilder:object:generate=true
// MbOptions contains the common settings for memory bandwidth allocation.
type MbOptions struct {
	Optional bool `json:"optional"`
}

type GoresctrlConfig struct {
	// Options contain common settings.
	Options *Options `json:"options,omitempty"`
	// Partitions configure cache partitions.
	Partitions map[string]PartitionConfig `json:"partitions,omitempty"`
}

// ToGoresctrl returns the configuration in native goresctrl format.
func (c *Config) ToGoresctrl() (*rdt.Config, bool, error) {
	if c == nil || !c.Enable {
		return nil, false, nil
	}

	if c.Options == nil && c.Partitions == nil {
		return nil, false, nil
	}

	in := GoresctrlConfig{
		Options:    c.Options,
		Partitions: c.Partitions,
	}

	data, err := json.Marshal(in)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrConfigConversion, err)
	}

	out := &rdt.Config{}
	err = json.Unmarshal(data, &out)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrConfigConversion, err)
	}

	return out, c.Force, nil
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	_, _, err := c.ToGoresctrl()
	return err
}
