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

// Package server implements the fake-cxl-pool REST API server: memory
// devices (backing files), qemu hosts (VMs) and hotplug attachments.
package server

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

// Defaults.
const (
	// Serial scheme: 0xc1 = CXL, 0x00 in the next byte = present at boot
	// (e2e topology local devices are 0xc100e2e0+i), 0xae = shared pool
	// devices, 0xee = unshared pool devices.
	DefaultSharedSerialBase = 0xc1ae0000
	DefaultSerialBase       = 0xc1ee0000
	DefaultDir              = "/tmp/fake-cxl-pool"
	DefaultDiscoveryPeriod  = 10 * time.Second
	DefaultDetachTimeout    = 15 * time.Second
	DefaultAttachTimeout    = 30 * time.Second
	// CapacityMultiplier: CXL device capacity is reported in 256 MiB units.
	CapacityMultiplier = 256 << 20
)

// DefaultName is the default name of the server (the appliance), the DRA
// pool name of fake-cxl-pool-controller.
const DefaultName = "fake-cxl-pool"

// Config is the server configuration (YAML).
type Config struct {
	// Name is the name of the server: a DNS label, the DRA pool name of
	// fake-cxl-pool-controller.
	Name   string `json:"name,omitempty"`
	Listen string `json:"listen,omitempty"`
	// SharedSerialBase and SerialBase: devices without a serial
	// get the next free serial above the base of shared devices or of the
	// others.
	SharedSerialBase *api.Serial `json:"sharedSerialBase,omitempty"`
	SerialBase       *api.Serial `json:"serialBase,omitempty"`
	// Dir is the directory of the device backing files.
	Dir           string          `json:"dir,omitempty"`
	Devices       []DeviceConfig  `json:"devices,omitempty"`
	Discovery     DiscoveryConfig `json:"discovery"`
	DetachTimeout api.Duration    `json:"detachTimeout,omitempty"`
	// StateFile is the file of the persistent state: "-" for none,
	// default <Dir>.state.json. It is not a key of the configuration file.
	StateFile string `json:"-"`
}

// DeviceConfig configures a static pool device.
type DeviceConfig struct {
	Name   string   `json:"name"`
	Size   api.Size `json:"size"`
	Shared bool     `json:"shared,omitempty"`
	// Persistent makes the device persistent memory with a label storage
	// area file next to the data file (see lsaPath).
	Persistent bool       `json:"persistent,omitempty"`
	Serial     api.Serial `json:"serial"`
}

// DiscoveryConfig configures qemu process discovery.
type DiscoveryConfig struct {
	// Names, if set, limits discovery to VMs whose name matches one of
	// these path.Match patterns.
	Names        []string     `json:"names,omitempty"`
	Interval     api.Duration `json:"interval,omitempty"`
	LocalDevices *bool        `json:"localDevices,omitempty"`
	// Control forces the monitor protocol of every host: "qmp" or "hmp"
	// (default: qmp if a QMP socket is known, else hmp).
	Control string `json:"control,omitempty"`
	// Slots, if set, limits the CXL slots of every host to the buses that
	// match one of these path.Match patterns.
	Slots []string `json:"slots,omitempty"`
}

// dnsLabelRe matches a DNS label (RFC 1123) of at most 63 characters.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// checkPatterns returns an error if one of the path.Match patterns is
// malformed.
func checkPatterns(pats []string) error {
	for _, pat := range pats {
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("invalid pattern %q", pat)
		}
	}
	return nil
}

// LocalDevicesEnabled returns true if pre-declared backends become local
// devices (default true).
func (d *DiscoveryConfig) LocalDevicesEnabled() bool { return d.LocalDevices == nil || *d.LocalDevices }

// LoadConfig reads a YAML config file. Unknown keys are errors.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// ParseConfig parses YAML config data and applies defaults.
func ParseConfig(data []byte) (*Config, error) {
	cfg := &Config{}
	if err := yaml.UnmarshalStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if err := cfg.Complete(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DefaultConfig returns the configuration used without a config file.
func DefaultConfig() *Config {
	cfg := &Config{}
	_ = cfg.Complete()
	return cfg
}

// Complete applies defaults and validates the configuration.
func (c *Config) Complete() error {
	if c.Name == "" {
		c.Name = DefaultName
	}
	if !dnsLabelRe.MatchString(c.Name) {
		return fmt.Errorf("name must be a DNS label (lower-case letters, digits, '-')")
	}
	if c.Listen == "" {
		c.Listen = api.DefaultListen
	}
	if c.SharedSerialBase == nil {
		sb := api.Serial(DefaultSharedSerialBase)
		c.SharedSerialBase = &sb
	}
	if c.SerialBase == nil {
		sb := api.Serial(DefaultSerialBase)
		c.SerialBase = &sb
	}
	if c.Dir == "" {
		c.Dir = DefaultDir
	}
	seen := map[string]bool{}
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.Name == "" {
			return fmt.Errorf("device %d: missing name", i)
		}
		if seen[d.Name] {
			return fmt.Errorf("duplicate device %q", d.Name)
		}
		seen[d.Name] = true
		if d.Size <= 0 {
			return fmt.Errorf("device %q: missing size", d.Name)
		}
		if d.Serial == 0 {
			return fmt.Errorf("device %q: missing serial", d.Name)
		}
	}
	if c.Discovery.Control != "" && c.Discovery.Control != "qmp" && c.Discovery.Control != "hmp" {
		return fmt.Errorf("discovery.control must be qmp or hmp")
	}
	if err := checkPatterns(c.Discovery.Slots); err != nil {
		return fmt.Errorf("discovery.slots: %w", err)
	}
	if c.Discovery.Interval == 0 {
		c.Discovery.Interval = api.Duration(DefaultDiscoveryPeriod)
	}
	if c.Discovery.Interval < 0 {
		return fmt.Errorf("discovery.interval must be positive")
	}
	if c.DetachTimeout == 0 {
		c.DetachTimeout = api.Duration(DefaultDetachTimeout)
	}
	if c.DetachTimeout < 0 {
		return fmt.Errorf("detachTimeout must be positive")
	}
	if c.StateFile == "" {
		c.StateFile = filepath.Clean(c.Dir) + ".state.json"
	}
	return nil
}
