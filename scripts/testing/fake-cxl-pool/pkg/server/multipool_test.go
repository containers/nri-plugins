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

// Tests of the configuration for several pool servers on one host:
// discovery.control, slot patterns and the serial check of adoption.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu/fake"
)

func TestDiscoveryControl(t *testing.T) {
	if _, err := ParseConfig([]byte("discovery: {control: qemu}")); err == nil || err.Error() != "discovery.control must be qmp or hmp" {
		t.Fatalf("bad discovery.control: %v", err)
	}
	// both sockets: qmp is preferred
	e := newTestEnv(t, "")
	e.stop()
	e.mu.Lock()
	e.hmpSockets = true
	e.fakes["/e2e/vm1/monitor.sock"] = e.fake("vm1")
	e.mu.Unlock()
	e.start()
	if h, err := e.c.Host(ctx, "vm1"); err != nil || h.Control != "qmp" || h.State != api.HostRunning {
		t.Fatalf("default control: %+v %v", h, err)
	}

	// hmp for every host
	e = newTestEnv(t, "discovery: {control: hmp}\n")
	h, err := e.c.Host(ctx, "vm1")
	if err != nil || h.Control != "" || h.Error != "no monitor socket" || h.State == api.HostRunning {
		t.Fatalf("hmp without an HMP socket: %+v %v", h, err)
	}
	e.stop()
	e.mu.Lock()
	e.hmpSockets = true
	e.fakes["/e2e/vm1/monitor.sock"] = e.fake("vm1")
	e.mu.Unlock()
	e.start()
	h, err = e.c.Host(ctx, "vm1")
	if err != nil || h.Control != "hmp" || h.HMP != "/e2e/vm1/monitor.sock" || h.State != api.HostRunning || h.Error != "" {
		t.Fatalf("discovery control hmp: %+v %v", h, err)
	}
	if _, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"}); err != nil {
		t.Fatalf("attach over hmp: %v", err)
	}
}

func slotBuses(h *api.Host) string {
	if h == nil {
		return ""
	}
	var bs []string
	for _, sl := range h.Slots {
		bs = append(bs, sl.Bus)
	}
	return strings.Join(bs, " ")
}

func TestSlotPatterns(t *testing.T) {
	if _, err := ParseConfig([]byte(`discovery: {slots: ["["]}`)); err == nil || !strings.Contains(err.Error(), `invalid pattern "`) {
		t.Errorf("bad slot pattern: %v", err)
	}
	e := newTestEnv(t, `discovery: {slots: ["*_hb0"]}`+"\n")
	h1, err := e.c.Host(ctx, "vm1")
	if err != nil || slotBuses(h1) != "ds0_hb0 ds1_hb0" {
		t.Fatalf("vm1 slots %q %v", slotBuses(h1), err)
	}
	// a slot outside the patterns does not exist for this server
	_, _, err = e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Slot: "ds0_hb1"})
	mustCode(t, err, api.CodeInvalidArgument)
	if !strings.Contains(err.Error(), `host "vm1" has no CXL slot "ds0_hb1"`) {
		t.Fatalf("unexpected error %v", err)
	}
	// nor does the slot of the local device (ds1_hb1)
	_, _, err = e.c.Attach(ctx, "vm1.memdev0", api.AttachRequest{Host: "vm1"})
	mustCode(t, err, api.CodeInvalidArgument)
	if !strings.Contains(err.Error(), `host "vm1" has no CXL slot "ds1_hb1" for its local device`) {
		t.Fatalf("unexpected error %v", err)
	}
	// the automatic choice takes matching slots only, NUMA preference
	// notwithstanding
	a, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", NUMANode: new(1)})
	if err != nil || a.Slot.HostBridge != "cxlhb0" {
		t.Fatalf("pooled0: %+v %v", a, err)
	}
	if a, _, err := e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1"}); err != nil || a.Slot.HostBridge != "cxlhb0" {
		t.Fatalf("shared0: %+v %v", a, err)
	}
	if _, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "d3", Size: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.c.Attach(ctx, "d3", api.AttachRequest{Host: "vm1"})
	mustCode(t, err, api.CodeConflict)
}

func TestAdoptionChecksSerial(t *testing.T) {
	e := newTestEnv(t, "")
	plug := func(f *fake.Qemu, id string, serial uint64) {
		t.Helper()
		dev, _, _ := strings.Cut(strings.TrimPrefix(id, "fcp_"), ".")
		d, err := e.c.Device(ctx, dev)
		if err != nil {
			t.Fatal(err)
		}
		f.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: id, Size: uint64(d.Size), MemPath: filepath.Join(e.dir, "pool", dev+".raw"), Share: true})
		t3 := qemu.CXLType3{ID: id, Bus: "ds0_hb0", VolatileMemdev: id, Serial: serial}
		if serial == 0 {
			err = f.PlugDeviceWithoutSerial(t3)
		} else {
			err = f.PlugDevice(t3)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	// another pool server's device of the same name in vm1
	plug(e.fake("vm1"), "fcp_pooled0.hp7", 0xc1ee1001)
	e.srv.Refresh(ctx)
	e.srv.Refresh(ctx)
	_, err := e.attachment("pooled0", "vm1")
	mustCode(t, err, api.CodeNotFound)
	warning := "WARNING: host vm1 has qemu device fcp_pooled0.hp7 with serial 0xc1ee1001, not the serial 0xc1ee0001 " +
		"of device pooled0: not adopted (another pool server's device?)"
	if n := strings.Count(e.logbuf.String(), warning); n != 1 {
		t.Fatalf("warning logged %d times, expected once: %q", n, warning)
	}
	// the warning is forgotten when the qemu device is gone
	if err := e.fake("vm1").DeviceDel(ctx, "fcp_pooled0.hp7"); err != nil {
		t.Fatal(err)
	}
	e.srv.Refresh(ctx)
	e.srv.mu.RLock()
	warned := e.srv.adoptWarned["vm1/fcp_pooled0.hp7"]
	e.srv.mu.RUnlock()
	if warned {
		t.Fatal("warning of a removed qemu device kept")
	}
	// the device's own serial is adopted
	plug(e.fake("vm2"), "fcp_pooled0.hp8", 0xc1ee0001)
	e.srv.Refresh(ctx)
	a, err := e.attachment("pooled0", "vm2")
	if err != nil || !a.Adopted || a.QemuDeviceID != "fcp_pooled0.hp8" || a.Serial != "0xc1ee0001" {
		t.Fatalf("not adopted: %+v %v", a, err)
	}
	// a qemu device without a serial is adopted with the device's serial
	plug(e.fake("vm1"), "fcp_shared0.hp9", 0)
	e.srv.Refresh(ctx)
	a, err = e.attachment("shared0", "vm1")
	if err != nil || !a.Adopted || a.QemuDeviceID != "fcp_shared0.hp9" || a.Serial != "0xc1ae0000" {
		t.Fatalf("without serial not adopted: %+v %v", a, err)
	}
}
