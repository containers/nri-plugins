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

// Tests of qemu restarts that a monitor sees before discovery does.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu/fake"
)

// socketMonitor is the monitor of a socket path: every call goes to the
// fake qemu that serves the path at the time of the call, as with a real
// socket whose qemu restarted. No fake on the path: the qemu is down.
type socketMonitor struct {
	e    *testEnv
	path string
}

func (m *socketMonitor) qemu() qemu.Monitor {
	m.e.mu.Lock()
	defer m.e.mu.Unlock()
	if f, ok := m.e.fakes[m.path]; ok {
		return f
	}
	f := fake.NewQemu(nil, nil)
	f.SetUnreachable(true)
	return f
}

func (m *socketMonitor) Version(ctx context.Context) (string, error) {
	return m.qemu().Version(ctx)
}
func (m *socketMonitor) ObjectAdd(ctx context.Context, b qemu.MemoryBackend) error {
	defer m.restartAt("object_add")
	return m.qemu().ObjectAdd(ctx, b)
}
func (m *socketMonitor) ObjectDel(ctx context.Context, id string) error {
	return m.qemu().ObjectDel(ctx, id)
}
func (m *socketMonitor) DeviceAdd(ctx context.Context, d qemu.CXLType3) error {
	defer m.restartAt("device_add")
	return m.qemu().DeviceAdd(ctx, d)
}
func (m *socketMonitor) DeviceDel(ctx context.Context, id string) error {
	return m.qemu().DeviceDel(ctx, id)
}
func (m *socketMonitor) WaitDeviceDeleted(ctx context.Context, id string) error {
	return m.qemu().WaitDeviceDeleted(ctx, id)
}
func (m *socketMonitor) DeviceExists(ctx context.Context, id string) (bool, error) {
	return m.qemu().DeviceExists(ctx, id)
}
func (m *socketMonitor) BackendMapped(ctx context.Context, objID string) (bool, error) {
	return m.qemu().BackendMapped(ctx, objID)
}
func (m *socketMonitor) QomGet(ctx context.Context, path, property string) (json.RawMessage, error) {
	return m.qemu().QomGet(ctx, path, property)
}
func (m *socketMonitor) QueryMemdevs(ctx context.Context) ([]qemu.Memdev, error) {
	return m.qemu().QueryMemdevs(ctx)
}
func (m *socketMonitor) QueryTree(ctx context.Context) (*qemu.Tree, error) {
	return m.qemu().QueryTree(ctx)
}
func (m *socketMonitor) Close() {}

// restartAt restarts the qemu of vm1 after the call of a monitor of vm1 if
// the test asks for it there (restartIf).
func (m *socketMonitor) restartAt(call string) {
	if strings.HasPrefix(m.path, "/e2e/vm1/") {
		m.e.restartIf(call)
	}
}

// restartIf restarts the qemu of vm1 (stopQemu, startQemu) if the test
// asks for it at this point (testEnv.restartAt), once.
func (e *testEnv) restartIf(at string) {
	e.mu.Lock()
	match := e.restartAt != "" && e.restartAt == at
	if match {
		e.restartAt = ""
	}
	e.mu.Unlock()
	if !match {
		return
	}
	e.stopQemu()
	f := e.startQemu()
	e.mu.Lock()
	e.restarted = f
	e.mu.Unlock()
}

// newRestartEnv is newTestEnv with socket monitors; with control "hmp"
// vm1 has a monitor.sock and the server uses it.
func newRestartEnv(t *testing.T, control, extraConfig string) *testEnv {
	t.Helper()
	cfg := "discovery: {interval: 1h}\n"
	if control == qemu.ProtoHMP {
		cfg = "discovery: {interval: 1h, control: hmp}\n"
	}
	e := newTestEnv(t, cfg+extraConfig)
	e.stop()
	e.mu.Lock()
	e.bySocket = true
	if control == qemu.ProtoHMP {
		e.hmpSockets = true
		e.fakes["/e2e/vm1/monitor.sock"] = e.fake("vm1")
	}
	e.mu.Unlock()
	e.start()
	if h, err := e.c.Host(ctx, "vm1"); err != nil || h.State != api.HostRunning || h.Control != control || h.PID != 101 {
		t.Fatalf("unexpected host %+v %v", h, err)
	}
	return e
}

// stopQemu stops the qemu of vm1 (pid 101): its sockets are refused.
func (e *testEnv) stopQemu() {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.alive, 101)
	delete(e.fakes, "/e2e/vm1/qmp.sock")
	delete(e.fakes, "/e2e/vm1/monitor.sock")
}

// startQemu starts a new qemu of vm1 (pid 201) on the sockets of the old
// one. Discovery has not seen it yet.
func (e *testEnv) startQemu() *fake.Qemu {
	f := newVMFake()
	f.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendRAM, ID: localObj, Size: 256 << 20, Share: true})
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alive[201] = true
	e.procs[0] = vmCmdline("vm1", 201, "-uuid", "6BA7B810-9DAD-11D1-80B4-00C04FD430C8",
		"-object", "memory-backend-ram,id="+localObj+",share=on,size=256M")
	e.fakes["/e2e/vm1/qmp.sock"] = f
	if e.hmpSockets {
		e.fakes["/e2e/vm1/monitor.sock"] = f
	}
	return f
}

// checkStopped checks that vm1 stopped because its qemu process 101 is
// gone: no attachment left, none finalized into the new qemu f.
func (e *testEnv) checkStopped(f *fake.Qemu) {
	e.t.Helper()
	if h := cachedHost(e.srv, "vm1"); h.State == api.HostRunning || h.PID != 0 || len(h.Attachments) != 0 {
		e.t.Fatalf("vm1 not stopped: %+v", h)
	}
	logs := e.logbuf.String()
	if !strings.Contains(logs, "host vm1: stopped: qemu process 101 not found") ||
		!strings.Contains(logs, "attachment pooled0@vm1: dropped: qemu process 101 not found") ||
		strings.Contains(logs, "qemu device fcp_pooled0.hp1 is gone") {
		e.t.Fatalf("unexpected logs:\n%s", logs)
	}
	if f != nil && len(f.Commands()) != 0 {
		e.t.Fatalf("commands sent to the new qemu: %v", f.Commands())
	}
}

// A restarted qemu answers on the socket of the old one before discovery
// has seen its new pid: its tree is not the old process's, the host stops
// at once. An attachment made into the new qemu keeps its owner, and
// discovery does not drop it as the old qemu's.
func TestQemuRestartSeenByMonitorFirst(t *testing.T) {
	for _, control := range []string{qemu.ProtoQMP, qemu.ProtoHMP} {
		t.Run(control, func(t *testing.T) {
			e := newRestartEnv(t, control, "")
			if _, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Owner: "k8s:old"}); err != nil {
				t.Fatal(err)
			}
			e.stopQemu()
			f := e.startQemu()
			// GET /hosts reads the tree of the new qemu
			if _, err := e.c.Hosts(ctx); err != nil {
				t.Fatal(err)
			}
			e.checkStopped(f)
			_, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Owner: "k8s:new"})
			mustCode(t, err, api.CodeUnavailable)
			e.srv.Refresh(ctx)
			if _, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Owner: "k8s:new"}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				e.srv.Refresh(ctx)
			}
			h, err := e.c.Host(ctx, "vm1")
			if err != nil || h.State != api.HostRunning || h.PID != 201 {
				t.Fatalf("vm1 not running the new qemu: %+v %v", h, err)
			}
			a, err := e.attachment("pooled0", "vm1")
			if err != nil || a.Owner != "k8s:new" || a.Adopted || a.State != api.AttachmentAttached || !f.HasDevice(a.QemuDeviceID) {
				t.Fatalf("attachment into the new qemu: %+v %v", a, err)
			}
			if logs := e.logbuf.String(); strings.Contains(logs, "qemu restarted") || strings.Contains(logs, "adopted") {
				t.Fatalf("unexpected logs:\n%s", logs)
			}
		})
	}
}

// A qemu that restarts during an attach, after the check of the tree that
// it read (the check before the object_add sees it), after the
// object_add, or after the device_add (the old qemu took the device with
// it). The attach fails and
// the host stops at once; nothing reaches the new qemu. (A restart between
// a check and the next command would need a new qemu that listens within
// microseconds of the exit of the old one.) After discovery the attach
// plugs the device into the new qemu with its owner, and nothing is
// adopted.
func TestQemuRestartDuringAttach(t *testing.T) {
	for _, at := range []string{"liveness check", "object_add", "device_add"} {
		for _, control := range []string{qemu.ProtoQMP, qemu.ProtoHMP} {
			t.Run(at+"/"+control, func(t *testing.T) {
				e := newRestartEnv(t, control, "")
				e.mu.Lock()
				e.restartAt = at
				e.mu.Unlock()
				_, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Owner: "k8s:new"})
				mustCode(t, err, api.CodeUnavailable)
				e.mu.Lock()
				f := e.restarted
				e.mu.Unlock()
				if f == nil {
					t.Fatal("qemu not restarted")
				}
				if !strings.Contains(err.Error(), "host vm1 stopped during the attach: its qemu process is gone") {
					t.Fatalf("attach error: %v", err)
				}
				e.checkStopped(f)
				e.srv.Refresh(ctx)
				if _, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Owner: "k8s:new"}); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					e.srv.Refresh(ctx)
				}
				a, err := e.attachment("pooled0", "vm1")
				if err != nil || a.Owner != "k8s:new" || a.Adopted || a.State != api.AttachmentAttached || !f.HasDevice(a.QemuDeviceID) {
					t.Fatalf("attachment into the new qemu: %+v %v", a, err)
				}
				if logs := e.logbuf.String(); strings.Contains(logs, "qemu restarted") || strings.Contains(logs, "adopted") {
					t.Fatalf("unexpected logs:\n%s", logs)
				}
			})
		}
	}
}

// The monitor fails after qemu has exited: the host stops at once, and a
// new qemu on the same socket is used only after discovery has found it.
func TestQemuExitSeenByMonitorFirst(t *testing.T) {
	e := newRestartEnv(t, qemu.ProtoHMP, "")
	if _, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Owner: "k8s:old"}); err != nil {
		t.Fatal(err)
	}
	e.stopQemu()
	if _, err := e.c.Hosts(ctx); err != nil {
		t.Fatal(err)
	}
	e.checkStopped(nil)
	f := e.startQemu()
	if _, err := e.c.Hosts(ctx); err != nil {
		t.Fatal(err)
	}
	e.checkStopped(f)
	e.srv.Refresh(ctx)
	if h, err := e.c.Host(ctx, "vm1"); err != nil || h.State != api.HostRunning || h.PID != 201 {
		t.Fatalf("vm1 not running the new qemu: %+v %v", h, err)
	}
}

// An attachment without owner takes the owner of the first attach request
// that has one, keeps it, and is not plugged again.
func TestAttachTakesOverAttachmentWithoutOwner(t *testing.T) {
	e := newTestEnv(t, "")
	f := e.fake("vm1")
	f.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: "fcp_pooled0.hp7", Size: 512 << 20,
		MemPath: filepath.Join(e.dir, "pool", "pooled0.raw"), Share: true})
	if err := f.PlugDevice(qemu.CXLType3{ID: "fcp_pooled0.hp7", Bus: "ds0_hb0", VolatileMemdev: "fcp_pooled0.hp7", Serial: 0xc1ee0001}); err != nil {
		t.Fatal(err)
	}
	e.srv.Refresh(ctx)
	plugged := len(f.Commands())
	for _, tc := range []struct{ owner, want string }{
		{"", ""},
		{"k8s:a", "k8s:a"},
		{"k8s:b", "k8s:a"},
	} {
		a, code, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Owner: tc.owner})
		if err != nil || code != http.StatusOK || a.Owner != tc.want || !a.Adopted || a.QemuDeviceID != "fcp_pooled0.hp7" {
			t.Fatalf("attach with owner %q: %d %+v %v", tc.owner, code, a, err)
		}
	}
	if n := len(f.Commands()); n != plugged {
		t.Fatalf("commands sent to qemu: %v", f.Commands()[plugged:])
	}
	e.stop()
	e.start()
	if a, err := e.attachment("pooled0", "vm1"); err != nil || a.Owner != "k8s:a" {
		t.Fatalf("owner not restored: %+v %v", a, err)
	}
}
