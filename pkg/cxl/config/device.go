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

package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// forbiddenPathDirs are the directories that device.path of mode fs must
// not be in.
var forbiddenPathDirs = []string{"/dev", "/proc", "/sys", "/run/cxl", "/var/run/cxl"}

// DeviceFacts are the facts of a pool device or of a local persistent
// memdev that its device section is checked against.
type DeviceFacts struct {
	// Persistent: the device is persistent memory.
	Persistent bool
	// Shared: other hosts may use the device at the same time (a shared
	// pool device, a local memdev listed in sharedMemdevs).
	Shared bool
	// Exclusive: no other claim has the device while this one is
	// allocated (the result consumed all shares; a pool device that is
	// not shared has one claim at most).
	Exclusive bool
	// SingleHost: no other host has the device while this claim is
	// allocated (status data singleHost or exclusive of a pool device, a
	// pool device that is not shared, a local memdev that is not in
	// sharedMemdevs).
	SingleHost bool
	// Coherent: other hosts' writes are visible without cache
	// maintenance (status data coherent of a pool device, memdevSettings
	// coherent of a local memdev).
	Coherent bool
}

// MemdevMode returns the mode of a slice of a local memdev of volatile
// memory with the device section: numa (default) or dax for a memdev that
// is not shared, dax (default and only) for a shared memdev; readOnly
// needs dax; access and path, which are of mode fs, are refused.
func (d Device) MemdevMode(shared bool) (string, error) {
	mode := d.Mode
	switch {
	case mode == "" && shared:
		mode = api.ModeDax
	case mode == "":
		mode = api.ModeNUMA
	case mode != api.ModeNUMA && mode != api.ModeDax:
		return "", fmt.Errorf("%sdevice.mode: invalid mode %q (%s or %s)", errPrefix, d.Mode, api.ModeNUMA, api.ModeDax)
	}
	switch {
	case d.IsReadOnly() && mode != api.ModeDax:
		return "", fmt.Errorf("%sdevice.readOnly needs mode %s", errPrefix, api.ModeDax)
	case shared && mode != api.ModeDax:
		return "", fmt.Errorf("%sdevice: a shared device is used as %s only", errPrefix, api.ModeDax)
	case d.Access != "":
		return "", errNeedsFs("access")
	case d.Path != "":
		return "", errNeedsFs("path")
	case d.FSType != "":
		return "", errNeedsFs("fsType")
	}
	return mode, nil
}

// NUMAOnly returns an error if the device section asks a device that is
// used as numa only for another mode or for another field.
//   - what: the device type in the message, device.DeviceTypeRegion or
//     device.DeviceTypeDRAM
func (d Device) NUMAOnly(what string) error {
	mode, err := d.MemdevMode(false)
	if err == nil && mode != api.ModeNUMA {
		err = fmt.Errorf("%sdevice: a %s is used as %s only", errPrefix, what, api.ModeNUMA)
	}
	return err
}

// CheckLease returns an error if the device section sets a field other
// than readOnly: false: the lease device has no device settings.
func (d Device) CheckLease() error {
	other := d
	other.ReadOnly = nil
	if other != (Device{}) || d.IsReadOnly() {
		return fmt.Errorf("%sdevice: the %s device has no device settings", errPrefix, device.DeviceTypeLease)
	}
	return nil
}

// PoolMode returns the mode of a pool device with the device section. The
// checks of the section alone come first, then those that need the facts
// of the device: block and fs are of persistent devices only and a
// persistent device has no other modes, a writer of a persistent device
// needs the facts that allow it (Exclusive, SingleHost, Coherent), a
// shared device of volatile memory is dax (default and only), numa is the
// default of other volatile devices.
func (d Device) PoolMode(f DeviceFacts) (string, error) {
	mode, err := d.persistentSection()
	if err != nil {
		return "", err
	}
	switch {
	case !f.Persistent && api.PersistentMode(mode):
		return "", fmt.Errorf("%sdevice.mode %s needs a persistent device", errPrefix, mode)
	case f.Persistent && !api.PersistentMode(mode):
		return "", fmt.Errorf("%sdevice: a persistent device needs mode %s or %s", errPrefix, api.ModeBlock, api.ModeFS)
	case f.Persistent:
		if err := d.checkWriter(mode, f); err != nil {
			return "", err
		}
		return mode, nil
	}
	// volatile memory
	switch {
	case mode == "" && f.Shared:
		mode = api.ModeDax
	case mode == "":
		mode = api.ModeNUMA
	}
	switch {
	case f.Shared && mode != api.ModeDax:
		return "", fmt.Errorf("%sdevice: a shared device is used as %s only", errPrefix, api.ModeDax)
	case d.IsReadOnly() && mode != api.ModeDax:
		return "", errReadOnlyMode()
	}
	return mode, nil
}

// PersistentMode returns the mode of a persistent device, a pool device
// of persistent memory or a local persistent memdev, with the device
// section: the rules of PoolMode for persistent devices.
func (d Device) PersistentMode(f DeviceFacts) (string, error) {
	f.Persistent = true
	return d.PoolMode(f)
}

// checkWriter returns an error if the device section writes a persistent
// device in the mode and the facts do not allow it: a writable block
// device needs the device exclusively, and on this host only; a
// read-write filesystem needs the device on this host only, or a cluster
// filesystem type on coherent memory.
func (d Device) checkWriter(mode string, f DeviceFacts) error {
	if d.IsReadOnly() {
		return nil
	}
	switch {
	case mode == api.ModeBlock && !f.Exclusive:
		return fmt.Errorf("%sdevice.mode %s: the device is writable only exclusively (no %s request)", errPrefix, api.ModeBlock, device.CapacityShares)
	case mode == api.ModeBlock && !f.SingleHost:
		return fmt.Errorf("%sdevice.mode %s: other hosts may use the device: it is not written in mode %s", errPrefix, api.ModeBlock, api.ModeBlock)
	case mode == api.ModeFS && !f.SingleHost && (!FSTypeCluster(d.FSType) || !f.Coherent):
		return fmt.Errorf("%sdevice.readOnly: false: other hosts may use the device: read-write needs it on this node only (claim status singleHost), or a cluster filesystem (device.fsType %s) on coherent memory", errPrefix, api.FSTypeGfs2)
	}
	return nil
}

// persistentSection returns the mode of the device section, "" for the
// default, and an error if a field is invalid alone or does not fit the
// mode, of a device that may be persistent.
func (d Device) persistentSection() (string, error) {
	mode := d.Mode
	switch mode {
	case "", api.ModeNUMA, api.ModeDax, api.ModeBlock, api.ModeFS:
	default:
		return "", fmt.Errorf("%sdevice.mode: invalid mode %q (%s, %s, %s or %s)", errPrefix, mode, api.ModeNUMA, api.ModeDax, api.ModeBlock, api.ModeFS)
	}
	switch {
	case d.Access != "" && mode != api.ModeFS:
		return "", errNeedsFs("access")
	case d.Path != "" && mode != api.ModeFS:
		return "", errNeedsFs("path")
	case d.FSType != "" && mode != api.ModeFS:
		return "", errNeedsFs("fsType")
	case d.FSType != "" && !api.ValidFSType(d.FSType):
		return "", fmt.Errorf(`%sdevice.fsType: invalid filesystem type %q (lower-case letters, digits, "_" and "-")`, errPrefix, d.FSType)
	case d.Access != "" && d.Access != api.AccessDirect && d.Access != api.AccessCached:
		return "", fmt.Errorf("%sdevice.access: invalid access %q (%s or %s)", errPrefix, d.Access, api.AccessDirect, api.AccessCached)
	case d.Path != "" && !validFSPath(d.Path):
		return "", fmt.Errorf("%sdevice.path: invalid path %q (an absolute path outside %s)", errPrefix, d.Path, joinList(forbiddenPathDirs, "and"))
	case d.IsReadOnly() && mode == api.ModeNUMA:
		return "", errReadOnlyMode()
	}
	return mode, nil
}

// NeedsSingleHost tells whether the attachment of a device for an
// allocation result with the resolved configuration must be exclusive to
// its host: mode fs read-write, unless the filesystem type is a cluster
// type and the device is coherent between hosts.
func NeedsSingleHost(resolved MemoryConfig, coherent bool) bool {
	d := resolved.DeviceSection()
	return d.Mode == api.ModeFS && !d.IsReadOnly() && (!FSTypeCluster(d.FSType) || !coherent)
}

// errNeedsFs returns the error of a field of mode fs in another mode.
func errNeedsFs(field string) error {
	return fmt.Errorf("%sdevice.%s needs mode %s", errPrefix, field, api.ModeFS)
}

// errReadOnlyMode returns the error of readOnly of a pool device in a mode
// without it.
func errReadOnlyMode() error {
	return fmt.Errorf("%sdevice.readOnly needs mode %s, %s or %s", errPrefix, api.ModeDax, api.ModeBlock, api.ModeFS)
}

// validFSPath tells whether p may be the path of mode fs in a container:
// absolute, clean, not / and not in one of forbiddenPathDirs.
func validFSPath(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		return false
	}
	for _, dir := range forbiddenPathDirs {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return false
		}
	}
	return true
}
