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
	"testing"

	"k8s.io/utils/ptr"

	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// ro and rw are the values of device.readOnly true and false.
var (
	ro = ptr.To(true)
	rw = ptr.To(false)
)

// checkErr reports whether err is nil for "" or has the text want.
func checkErr(err error, want string) bool {
	if want == "" {
		return err == nil
	}
	return err != nil && err.Error() == want
}

func TestMemdevMode(t *testing.T) {
	for _, tc := range []struct {
		dev    Device
		shared bool
		want   string
		err    string
	}{
		{shared: true, want: api.ModeDax},
		{want: api.ModeNUMA},
		{dev: Device{Mode: "numa"}, want: api.ModeNUMA},
		{dev: Device{Mode: "numa", ReadOnly: rw}, want: api.ModeNUMA},
		{dev: Device{ReadOnly: rw}, want: api.ModeNUMA},
		{dev: Device{Mode: "dax"}, want: api.ModeDax},
		{dev: Device{Mode: "dax", ReadOnly: ro}, want: api.ModeDax},
		{dev: Device{ReadOnly: ro}, shared: true, want: api.ModeDax},
		{dev: Device{Mode: "dax", ReadOnly: ro}, shared: true, want: api.ModeDax},
		{dev: Device{Mode: "numa"}, shared: true, err: "MemoryConfig: device: a shared device is used as dax only"},
		{dev: Device{ReadOnly: ro}, err: "MemoryConfig: device.readOnly needs mode dax"},
		{dev: Device{Mode: "kmem"}, err: `MemoryConfig: device.mode: invalid mode "kmem" (numa or dax)`},
		// volatile local memdevs have no modes block and fs
		{dev: Device{Mode: "fs", ReadOnly: ro, Access: "direct"}, err: `MemoryConfig: device.mode: invalid mode "fs" (numa or dax)`},
		{dev: Device{Mode: "fs"}, err: `MemoryConfig: device.mode: invalid mode "fs" (numa or dax)`},
		{dev: Device{Mode: "block"}, err: `MemoryConfig: device.mode: invalid mode "block" (numa or dax)`},
		{dev: Device{Access: "direct"}, err: "MemoryConfig: device.access needs mode fs"},
		{dev: Device{Mode: "dax", Path: "/models"}, err: "MemoryConfig: device.path needs mode fs"},
		{dev: Device{Mode: "dax", Path: "/models", FSType: "ext4"}, err: "MemoryConfig: device.path needs mode fs"},
		{dev: Device{FSType: "ext4"}, err: "MemoryConfig: device.fsType needs mode fs"},
		{dev: Device{Mode: "dax", FSType: "Bad"}, shared: true, err: "MemoryConfig: device.fsType needs mode fs"},
	} {
		got, err := tc.dev.MemdevMode(tc.shared)
		if !checkErr(err, tc.err) || (tc.err == "" && got != tc.want) {
			t.Errorf("%s shared %v: got %q %v, expected %q %q", show(tc.dev), tc.shared, got, err, tc.want, tc.err)
		}
	}
}

func TestNUMAOnly(t *testing.T) {
	for _, tc := range []struct {
		dev  Device
		what string
		err  string
	}{
		{Device{}, device.DeviceTypeRegion, ""},
		{Device{Mode: "numa"}, device.DeviceTypeDRAM, ""},
		{Device{ReadOnly: rw}, device.DeviceTypeDRAM, ""},
		{Device{Mode: "dax"}, device.DeviceTypeRegion, "MemoryConfig: device: a region is used as numa only"},
		{Device{Mode: "dax", ReadOnly: ro}, device.DeviceTypeDRAM, "MemoryConfig: device: a dram is used as numa only"},
		{Device{ReadOnly: ro}, device.DeviceTypeRegion, "MemoryConfig: device.readOnly needs mode dax"},
		{Device{Mode: "kmem"}, device.DeviceTypeDRAM, `MemoryConfig: device.mode: invalid mode "kmem" (numa or dax)`},
		{Device{Access: "cached"}, device.DeviceTypeRegion, "MemoryConfig: device.access needs mode fs"},
		{Device{Path: "/x"}, device.DeviceTypeDRAM, "MemoryConfig: device.path needs mode fs"},
		{Device{FSType: "xfs"}, device.DeviceTypeRegion, "MemoryConfig: device.fsType needs mode fs"},
	} {
		if err := tc.dev.NUMAOnly(tc.what); !checkErr(err, tc.err) {
			t.Errorf("%s %s: %v, expected %q", show(tc.dev), tc.what, err, tc.err)
		}
	}
}

func TestCheckLease(t *testing.T) {
	const msg = "MemoryConfig: device: the lease device has no device settings"
	for _, tc := range []struct {
		dev Device
		err string
	}{
		{Device{}, ""},
		{Device{ReadOnly: rw}, ""},
		{Device{Mode: "numa"}, msg},
		{Device{ReadOnly: ro}, msg},
		{Device{Access: "direct"}, msg},
		{Device{Path: "/models"}, msg},
		{Device{FSType: "ext4"}, msg},
		{Device{FSType: "ext4", ReadOnly: rw}, msg},
	} {
		if err := tc.dev.CheckLease(); !checkErr(err, tc.err) {
			t.Errorf("%s: %v, expected %q", show(tc.dev), err, tc.err)
		}
	}
}

func TestIsReadOnly(t *testing.T) {
	for _, tc := range []struct {
		dev  Device
		want bool
	}{
		{Device{}, false},
		{Device{Mode: "numa"}, false},
		{Device{Mode: "dax"}, false},
		{Device{Mode: "block"}, false},
		{Device{Mode: "fs"}, true},
		{Device{Mode: "fs", FSType: "ext4", Access: "cached"}, true},
		{Device{ReadOnly: ro}, true},
		{Device{ReadOnly: rw}, false},
		{Device{Mode: "dax", ReadOnly: ro}, true},
		{Device{Mode: "dax", ReadOnly: rw}, false},
		{Device{Mode: "block", ReadOnly: ro}, true},
		{Device{Mode: "block", ReadOnly: rw}, false},
		{Device{Mode: "fs", ReadOnly: ro}, true},
		{Device{Mode: "fs", ReadOnly: rw}, false},
	} {
		if got := tc.dev.IsReadOnly(); got != tc.want {
			t.Errorf("%s: read-only %v, expected %v", show(tc.dev), got, tc.want)
		}
	}
}

func TestPoolMode(t *testing.T) {
	volatile := DeviceFacts{}
	shared := DeviceFacts{Shared: true}
	// a share of a shared persistent device
	reader := DeviceFacts{Shared: true, Persistent: true}
	// all shares of a shared persistent device, attached exclusively
	writer := DeviceFacts{Shared: true, Persistent: true, Exclusive: true, SingleHost: true}
	// a share of a shared persistent device, attached to this host only
	host := DeviceFacts{Shared: true, Persistent: true, SingleHost: true}
	// a share of a shared persistent coherent device
	coherent := DeviceFacts{Shared: true, Persistent: true, Coherent: true}
	private := DeviceFacts{Persistent: true, Exclusive: true, SingleHost: true}
	const (
		f1     = "MemoryConfig: device.mode block: the device is writable only exclusively (no shares request)"
		f2     = "MemoryConfig: device.mode block: other hosts may use the device: it is not written in mode block"
		f3     = "MemoryConfig: device.readOnly: false: other hosts may use the device: read-write needs it on this node only (claim status singleHost), or a cluster filesystem (device.fsType gfs2) on coherent memory"
		badFs  = `MemoryConfig: device.fsType: invalid filesystem type "Ext4" (lower-case letters, digits, "_" and "-")`
		roMode = "MemoryConfig: device.readOnly needs mode dax, block or fs"
	)
	for _, tc := range []struct {
		dev  Device
		f    DeviceFacts
		want string
		err  string
	}{
		{f: volatile, want: api.ModeNUMA},
		{f: shared, want: api.ModeDax},
		{dev: Device{ReadOnly: rw}, f: volatile, want: api.ModeNUMA},
		{dev: Device{Mode: "dax"}, f: volatile, want: api.ModeDax},
		{dev: Device{Mode: "dax", ReadOnly: ro}, f: volatile, want: api.ModeDax},
		{dev: Device{ReadOnly: ro}, f: shared, want: api.ModeDax},
		{dev: Device{Mode: "block"}, f: writer, want: api.ModeBlock},
		{dev: Device{Mode: "block"}, f: private, want: api.ModeBlock},
		{dev: Device{Mode: "block", ReadOnly: rw}, f: private, want: api.ModeBlock},
		{dev: Device{Mode: "block", ReadOnly: ro}, f: reader, want: api.ModeBlock},
		// mode fs read-only: absent or true
		{dev: Device{Mode: "fs"}, f: reader, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: ro}, f: reader, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: ro, Access: "cached", Path: "/models"}, f: writer, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: ro, Access: "direct"}, f: private, want: api.ModeFS},
		{dev: Device{Mode: "fs", FSType: "xfs"}, f: reader, want: api.ModeFS},
		{dev: Device{Mode: "fs", FSType: "my_fs-1"}, f: reader, want: api.ModeFS},
		// mode fs read-write: on this host only, or a cluster filesystem on coherent memory
		{dev: Device{Mode: "fs", ReadOnly: rw}, f: host, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "ext4"}, f: writer, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: rw, Access: "cached"}, f: private, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "gfs2"}, f: coherent, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "gfs2"}, f: host, want: api.ModeFS},
		// the section alone, in this order
		{dev: Device{Mode: "ext4", Access: "bogus"}, f: reader, err: `MemoryConfig: device.mode: invalid mode "ext4" (numa, dax, block or fs)`},
		{dev: Device{Mode: "block", Access: "bogus", Path: "rel", FSType: "Ext4"}, f: reader, err: "MemoryConfig: device.access needs mode fs"},
		{dev: Device{Access: "direct"}, f: volatile, err: "MemoryConfig: device.access needs mode fs"},
		{dev: Device{Mode: "dax", Path: "rel", FSType: "Ext4"}, f: reader, err: "MemoryConfig: device.path needs mode fs"},
		{dev: Device{Mode: "block", FSType: "Ext4", ReadOnly: ro}, f: reader, err: "MemoryConfig: device.fsType needs mode fs"},
		{dev: Device{FSType: "ext4"}, f: volatile, err: "MemoryConfig: device.fsType needs mode fs"},
		{dev: Device{Mode: "fs", FSType: "Ext4", Access: "dax", Path: "rel"}, f: volatile, err: badFs},
		{dev: Device{Mode: "fs", FSType: "fuse.x"}, f: reader, err: `MemoryConfig: device.fsType: invalid filesystem type "fuse.x" (lower-case letters, digits, "_" and "-")`},
		{dev: Device{Mode: "fs", Access: "dax", Path: "rel"}, f: volatile, err: `MemoryConfig: device.access: invalid access "dax" (direct or cached)`},
		{dev: Device{Mode: "fs", Path: "/run/cxl/fs"}, f: volatile,
			err: `MemoryConfig: device.path: invalid path "/run/cxl/fs" (an absolute path outside /dev, /proc, /sys, /run/cxl and /var/run/cxl)`},
		{dev: Device{Mode: "numa", ReadOnly: ro}, f: reader, err: roMode},
		// with the facts of the device, in this order
		{dev: Device{Mode: "block"}, f: shared, err: "MemoryConfig: device.mode block needs a persistent device"},
		{dev: Device{Mode: "fs", ReadOnly: ro}, f: volatile, err: "MemoryConfig: device.mode fs needs a persistent device"},
		{dev: Device{Mode: "fs"}, f: volatile, err: "MemoryConfig: device.mode fs needs a persistent device"},
		{f: reader, err: "MemoryConfig: device: a persistent device needs mode block or fs"},
		{dev: Device{ReadOnly: ro}, f: private, err: "MemoryConfig: device: a persistent device needs mode block or fs"},
		{dev: Device{Mode: "dax"}, f: writer, err: "MemoryConfig: device: a persistent device needs mode block or fs"},
		{dev: Device{Mode: "numa"}, f: private, err: "MemoryConfig: device: a persistent device needs mode block or fs"},
		// F1
		{dev: Device{Mode: "block"}, f: reader, err: f1},
		{dev: Device{Mode: "block", ReadOnly: rw}, f: host, err: f1},
		{dev: Device{Mode: "block"}, f: coherent, err: f1},
		// F2
		{dev: Device{Mode: "block"}, f: DeviceFacts{Shared: true, Persistent: true, Exclusive: true}, err: f2},
		// F3
		{dev: Device{Mode: "fs", ReadOnly: rw}, f: reader, err: f3},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "ext4"}, f: coherent, err: f3},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "gfs2"}, f: reader, err: f3},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "erofs"}, f: DeviceFacts{Shared: true, Persistent: true, Exclusive: true}, err: f3},
		// volatile devices
		{dev: Device{Mode: "numa"}, f: shared, err: "MemoryConfig: device: a shared device is used as dax only"},
		{dev: Device{ReadOnly: ro}, f: volatile, err: roMode},
	} {
		got, err := tc.dev.PoolMode(tc.f)
		if !checkErr(err, tc.err) || (tc.err == "" && got != tc.want) {
			t.Errorf("%s %+v: got %q %v, expected %q %q", show(tc.dev), tc.f, got, err, tc.want, tc.err)
		}
	}
}

func TestPersistentMode(t *testing.T) {
	// a local persistent memdev that is not in sharedMemdevs: all shares, a share
	all := DeviceFacts{Exclusive: true, SingleHost: true}
	share := DeviceFacts{SingleHost: true}
	// a local persistent memdev in sharedMemdevs
	sharedAll := DeviceFacts{Shared: true, Exclusive: true}
	sharedShare := DeviceFacts{Shared: true}
	sharedCoherent := DeviceFacts{Shared: true, Coherent: true}
	const (
		f1 = "MemoryConfig: device.mode block: the device is writable only exclusively (no shares request)"
		f2 = "MemoryConfig: device.mode block: other hosts may use the device: it is not written in mode block"
		f3 = "MemoryConfig: device.readOnly: false: other hosts may use the device: read-write needs it on this node only (claim status singleHost), or a cluster filesystem (device.fsType gfs2) on coherent memory"
	)
	for _, tc := range []struct {
		dev  Device
		f    DeviceFacts
		want string
		err  string
	}{
		{dev: Device{Mode: "block"}, f: all, want: api.ModeBlock},
		{dev: Device{Mode: "block", ReadOnly: ro}, f: share, want: api.ModeBlock},
		{dev: Device{Mode: "block", ReadOnly: ro}, f: sharedShare, want: api.ModeBlock},
		{dev: Device{Mode: "fs"}, f: sharedShare, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: rw}, f: share, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "btrfs"}, f: all, want: api.ModeFS},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "gfs2"}, f: sharedCoherent, want: api.ModeFS},
		{dev: Device{Mode: "dax", FSType: "ext4"}, f: all, err: "MemoryConfig: device.fsType needs mode fs"},
		{dev: Device{}, f: all, err: "MemoryConfig: device: a persistent device needs mode block or fs"},
		{dev: Device{Mode: "dax"}, f: share, err: "MemoryConfig: device: a persistent device needs mode block or fs"},
		{dev: Device{Mode: "block"}, f: share, err: f1},
		{dev: Device{Mode: "block", ReadOnly: rw}, f: sharedShare, err: f1},
		{dev: Device{Mode: "block"}, f: sharedAll, err: f2},
		{dev: Device{Mode: "block"}, f: DeviceFacts{Shared: true, Exclusive: true, Coherent: true}, err: f2},
		{dev: Device{Mode: "fs", ReadOnly: rw}, f: sharedAll, err: f3},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "gfs2"}, f: sharedShare, err: f3},
		{dev: Device{Mode: "fs", ReadOnly: rw, FSType: "xfs"}, f: sharedCoherent, err: f3},
	} {
		got, err := tc.dev.PersistentMode(tc.f)
		if !checkErr(err, tc.err) || (tc.err == "" && got != tc.want) {
			t.Errorf("%s %+v: got %q %v, expected %q %q", show(tc.dev), tc.f, got, err, tc.want, tc.err)
		}
	}
}

func TestNeedsSingleHost(t *testing.T) {
	for _, tc := range []struct {
		dev      *Device
		coherent bool
		want     bool
	}{
		{nil, false, false},
		{&Device{}, false, false},
		{&Device{Mode: "dax"}, false, false},
		{&Device{Mode: "block"}, false, false},
		{&Device{Mode: "block", ReadOnly: rw}, false, false},
		{&Device{Mode: "fs"}, false, false},
		{&Device{Mode: "fs", ReadOnly: ro}, false, false},
		{&Device{Mode: "fs", ReadOnly: ro, FSType: "ext4"}, false, false},
		{&Device{Mode: "fs", ReadOnly: rw}, false, true},
		{&Device{Mode: "fs", ReadOnly: rw}, true, true},
		{&Device{Mode: "fs", ReadOnly: rw, FSType: "ext4"}, true, true},
		{&Device{Mode: "fs", ReadOnly: rw, FSType: "unknownfs"}, true, true},
		{&Device{Mode: "fs", ReadOnly: rw, FSType: "gfs2"}, false, true},
		{&Device{Mode: "fs", ReadOnly: rw, FSType: "gfs2"}, true, false},
	} {
		cfg := MemoryConfig{APIVersion: APIVersion, Kind: Kind, Device: tc.dev}
		if got := NeedsSingleHost(cfg, tc.coherent); got != tc.want {
			t.Errorf("%s coherent %v: exclusive %v, expected %v", cfg.Compact(), tc.coherent, got, tc.want)
		}
	}
}

// show returns the device section as JSON for messages.
func show(d Device) string {
	return (&MemoryConfig{Device: &d}).Compact()
}

func TestValidFSPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/models": true, "/run/cxl-fs/C1AE0001": true, "/run/cxl-fs": true, "/devices": true, "/run/cxlfs": true, "/a/b": true,
		"": false, "/": false, "models": false, "/models/": false, "/a/../b": false, "/a//b": false,
		"/dev": false, "/dev/pmem0": false, "/proc/1": false, "/sys": false, "/run/cxl": false, "/run/cxl/fs/x": false,
		"/var/run/cxl": false, "/var/run/cxl/fs": false, "/var/run/cxl-fs": true,
	} {
		if got := validFSPath(p); got != want {
			t.Errorf("validFsPath(%q) = %v", p, got)
		}
	}
}

func TestDeviceSection(t *testing.T) {
	var nilConfig *MemoryConfig
	if got := nilConfig.DeviceSection(); got != (Device{}) {
		t.Errorf("nil config: %+v", got)
	}
	if got := (&MemoryConfig{}).DeviceSection(); got != (Device{}) {
		t.Errorf("no device section: %+v", got)
	}
	want := Device{Mode: api.ModeDax, ReadOnly: ro}
	if got := (&MemoryConfig{Device: &want}).DeviceSection(); got != want {
		t.Errorf("device section: %+v", got)
	}
}
