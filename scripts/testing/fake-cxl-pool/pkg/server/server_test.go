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

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/client"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu/fake"
)

type testEnv struct {
	t      *testing.T
	dir    string
	cfg    *Config
	srv    *Server
	hs     *httptest.Server
	c      *client.Client
	mu     sync.Mutex
	fakes  map[string]*fake.Qemu // by monitor socket path
	procs  []*qemu.Process
	alive  map[int]bool
	logbuf *lockedBuf
	// hmpSockets: the monitor.sock of every VM exists, too
	hmpSockets bool
	// bySocket: a monitor reaches the fake that serves its socket path at
	// the time of each call (socketMonitor), not the one of its creation
	bySocket bool
	// restartAt: the qemu of vm1 restarts once (restartIf), after the call
	// of that name of a socketMonitor of vm1 ("object_add", "device_add")
	// or after a "liveness check" of it; restarted is the new qemu
	restartAt string
	restarted *fake.Qemu
}

// lockedBuf is a log buffer that tests read while the server writes.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(b)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func vmCmdline(name string, pid int, extra ...string) *qemu.Process {
	args := []string{"/usr/bin/qemu-system-x86_64",
		"-drive", "if=none,id=disk0,format=qcow2,file=/e2e/" + name + "/.vagrant/machines/" + name + "/qemu/vq_x/linked-box.img",
		"-qmp", "unix:qmp.sock,server,nowait",
		"-monitor", "unix:monitor.sock,server,nowait",
	}
	args = append(args, extra...)
	p := qemu.ParseCmdline(args)
	p.PID = pid
	p.StartTime = uint64(1000 + pid)
	p.Cwd = "/"
	for i, s := range p.QMPSockets {
		p.QMPSockets[i] = p.ResolvePath(s, nil)
	}
	for i, s := range p.HMPSockets {
		p.HMPSockets[i] = p.ResolvePath(s, nil)
	}
	return p
}

func newVMFake() *fake.Qemu {
	f := fake.NewQemu(map[string]int{"cxlhb0": 0, "cxlhb1": 1}, map[string]string{
		"ds0_hb0": "cxlhb0", "ds1_hb0": "cxlhb0",
		"ds0_hb1": "cxlhb1", "ds1_hb1": "cxlhb1",
	})
	f.SetFMW([]qemu.FMWWindow{{Targets: []string{"cxlhb0"}, Size: 4 << 30}, {Targets: []string{"cxlhb1"}, Size: 4 << 30}})
	return f
}

const localObj = "beram_cxl_memdev0__bus_ds1_hb1__sn_0xc100e2e0"

func newTestEnv(t *testing.T, extraConfig string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	e := &testEnv{t: t, dir: dir, fakes: map[string]*fake.Qemu{}, alive: map[int]bool{}, logbuf: &lockedBuf{}}
	vm1 := vmCmdline("vm1", 101, "-uuid", "6BA7B810-9DAD-11D1-80B4-00C04FD430C8",
		"-object", "memory-backend-ram,id="+localObj+",share=on,size=256M")
	vm2 := vmCmdline("vm2", 102)
	e.procs = []*qemu.Process{vm1, vm2}
	e.alive[101], e.alive[102] = true, true
	f1, f2 := newVMFake(), newVMFake()
	f1.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendRAM, ID: localObj, Size: 256 << 20, Share: true})
	e.fakes["/e2e/vm1/qmp.sock"] = f1
	e.fakes["/e2e/vm2/qmp.sock"] = f2
	cfgText := `
listen: 127.0.0.1:0
serialBase: 0xc1ee0000
dir: ` + filepath.Join(dir, "pool") + `
devices:
  - name: shared0
    size: 256M
    shared: true
    serial: 0xc1ae0000
  - name: pooled0
    size: 512M
    serial: 0xc1ee0001
` + extraConfig
	cfg, err := ParseConfig([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateFile = filepath.Join(dir, "state.json")
	e.cfg = cfg
	e.start()
	return e
}

func (e *testEnv) start() {
	e.t.Helper()
	if e.logbuf == nil {
		e.logbuf = &lockedBuf{}
	}
	opts := Options{
		Logger:  log.New(io.MultiWriter(e.t.Output(), e.logbuf), "", 0),
		Verbose: true,
		Discover: func() ([]*qemu.Process, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return append([]*qemu.Process(nil), e.procs...), nil
		},
		ProcessAlive: func(pid int, st uint64) bool {
			e.mu.Lock()
			alive := e.alive[pid]
			e.mu.Unlock()
			if pid == 101 {
				// right after a check that it runs
				e.restartIf("liveness check")
			}
			return alive
		},
		NewMonitor: func(proto, path string) qemu.Monitor {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.bySocket {
				return &socketMonitor{e: e, path: path}
			}
			if f, ok := e.fakes[path]; ok {
				return f
			}
			f := fake.NewQemu(nil, nil)
			f.SetUnreachable(true)
			return f
		},
		SocketExists: func(path string) bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			return strings.HasSuffix(path, "qmp.sock") || (e.hmpSockets && strings.HasSuffix(path, "monitor.sock"))
		},
		BackgroundPoll: 20 * time.Millisecond,
	}
	srv, err := New(e.cfg, opts)
	if err != nil {
		e.t.Fatal(err)
	}
	srv.Start()
	e.srv = srv
	e.hs = httptest.NewServer(srv.Handler())
	e.c = client.New(e.hs.URL)
	e.t.Cleanup(e.stop)
}

func (e *testEnv) stop() {
	if e.hs != nil {
		e.hs.Close()
		e.hs = nil
	}
	if e.srv != nil {
		e.srv.Stop()
		e.srv = nil
	}
}

func (e *testEnv) fake(host string) *fake.Qemu { return e.fakes["/e2e/"+host+"/qmp.sock"] }

// attachment returns the attachment of a device to a host.
func (e *testEnv) attachment(device, host string) (*api.Attachment, error) {
	return attachmentOf(e.srv, device, host)
}

// attachmentOf returns the attachment of a device to a host of a server,
// a NotFound error if there is none.
func attachmentOf(s *Server, device, host string) (*api.Attachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := s.atts[api.AttachmentID(device, host)]
	if a == nil {
		return nil, api.NotFound("device %q is not attached to %q", device, host)
	}
	att := a.Attachment
	return &att, nil
}

// waitDetached waits until the device is not attached to the host.
func (e *testEnv) waitDetached(device, host string) {
	e.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := e.attachment(device, host); err != nil {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s is still attached to %s", device, host)
		}
	}
}

// cachedHost returns the view of a host of a server without monitor
// calls.
func cachedHost(s *Server, name string) api.Host {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hostViewLocked(s.hosts[name])
}

// newServerWith starts a server without a state file for the config
// whose hosts vm1 (and vm2) use the given monitors.
func newServerWith(t *testing.T, cfgText string, mons map[string]qemu.Monitor) *Server {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateFile = "-"
	var procs []*qemu.Process
	for i, n := range []string{"vm1", "vm2"} {
		if mons[n] != nil {
			procs = append(procs, vmCmdline(n, 101+i, "-object", "memory-backend-ram,id="+localObj+",share=on,size=256M"))
		}
	}
	srv, err := New(cfg, Options{
		Logger:       log.New(t.Output(), "", 0),
		Verbose:      true,
		Discover:     func() ([]*qemu.Process, error) { return procs, nil },
		ProcessAlive: func(int, uint64) bool { return true },
		NewMonitor: func(proto, path string) qemu.Monitor {
			for n, m := range mons {
				if strings.Contains(path, "/"+n+"/") {
					return m
				}
			}
			f := fake.NewQemu(nil, nil)
			f.SetUnreachable(true)
			return f
		},
		SocketExists:   func(p string) bool { return strings.HasSuffix(p, "qmp.sock") },
		BackgroundPoll: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	t.Cleanup(srv.Stop)
	return srv
}

func poolConfig(dir, extra string) string {
	return "dir: " + filepath.Join(dir, "pool") + "\n" + extra
}

const pooled0 = "devices:\n  - name: pooled0\n    size: 256M\n    serial: 0xc1ee0001\n"

var ctx = context.Background()

func mustCode(t *testing.T, err error, code string) {
	t.Helper()
	if !api.IsCode(err, code) {
		t.Fatalf("expected %s error, got %v", code, err)
	}
}

func TestHostsAndResolve(t *testing.T) {
	e := newTestEnv(t, "")
	hs, err := e.c.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 || hs[0].Name != "vm1" || hs[1].Name != "vm2" {
		t.Fatalf("unexpected hosts %+v", hs)
	}
	h := hs[0]
	if h.State != api.HostRunning || h.Control != "qmp" || h.QMP != "/e2e/vm1/qmp.sock" || h.PID != 101 ||
		h.UUID != "6ba7b810-9dad-11d1-80b4-00c04fd430c8" {
		t.Fatalf("unexpected host %+v", h)
	}
	if len(h.Slots) != 4 || h.Slots[0].Bus != "ds0_hb0" || h.Slots[2].NUMANode != 1 {
		t.Fatalf("unexpected slots %+v", h.Slots)
	}
	if len(h.HostBridges) != 2 || h.HostBridges[1].FMWSize != 4<<30 || h.HostBridges[1].NUMANode != 1 {
		t.Fatalf("unexpected host bridges %+v", h.HostBridges)
	}
	if len(h.LocalDevices) != 1 || h.LocalDevices[0] != "vm1.memdev0" {
		t.Fatalf("unexpected local devices %+v", h.LocalDevices)
	}
	r, err := e.c.Resolve(ctx, "vm2.example.com", "")
	if err != nil || r.Name != "vm2" {
		t.Fatalf("resolve by first label: %+v %v", r, err)
	}
	r, err = e.c.Resolve(ctx, "wrong-hostname", "6BA7B810-9DAD-11D1-80B4-00C04FD430C8")
	if err != nil || r.Name != "vm1" {
		t.Fatalf("resolve by uuid: %+v %v", r, err)
	}
	_, err = e.c.Resolve(ctx, "nosuch", "")
	mustCode(t, err, api.CodeNotFound)
	h2, err := e.c.Host(ctx, "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err != nil || h2.Name != "vm1" {
		t.Fatalf("host by uuid: %+v %v", h2, err)
	}
	st, err := e.c.Status(ctx)
	if err != nil || st.Name != "fake-cxl-pool" || st.Hosts != 2 || st.Devices != 3 || st.Qemu.Versions["vm1"] != "11.1.1 fake" {
		t.Fatalf("unexpected status %+v %v", st, err)
	}
}

func TestCreateDevice(t *testing.T) {
	e := newTestEnv(t, "")
	_, err := e.c.CreateDevice(ctx, api.DeviceCreate{Size: 100 << 20})
	mustCode(t, err, api.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "size 100Mi is not a multiple of 256Mi") {
		t.Fatalf("unexpected error text %q", err)
	}
	d, err := e.c.CreateDevice(ctx, api.DeviceCreate{Size: 256 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "dev0" || d.Serial != "0xc1ee0002" || d.State != api.DeviceFree || d.Scope != api.ScopePool {
		// pooled0 has 0xc1ee0001
		t.Fatalf("unexpected device %+v", d)
	}
	if st, err := os.Stat(d.Path); err != nil || st.Size() != 256<<20 {
		t.Fatalf("backing file: %v %v", st, err)
	}
	_, err = e.c.CreateDevice(ctx, api.DeviceCreate{Name: "dev0", Size: 256 << 20})
	mustCode(t, err, api.CodeConflict)
	_, err = e.c.CreateDevice(ctx, api.DeviceCreate{Name: "x", Size: 256 << 20, Serial: "0xc1ae0000"})
	mustCode(t, err, api.CodeConflict)
	d2, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "s1", Size: 256 << 20, Shared: true, Serial: "0xabc"})
	if err != nil || d2.Serial != "0xabc" || !d2.Shared {
		t.Fatalf("unexpected device %+v %v", d2, err)
	}
	// a shared device without a serial gets one from the shared base
	d3, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "s2", Size: 256 << 20, Shared: true})
	if err != nil || d3.Serial != "0xc1ae0001" || !d3.Shared {
		t.Fatalf("unexpected device %+v %v", d3, err)
	}
	if err := e.c.DeleteDevice(ctx, "s2", false); err != nil {
		t.Fatal(err)
	}
	if err := e.c.DeleteDevice(ctx, "dev0", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.Path); !os.IsNotExist(err) {
		t.Fatalf("backing file not removed: %v", err)
	}
	mustCode(t, e.c.DeleteDevice(ctx, "shared0", false), api.CodeConflict)
	mustCode(t, e.c.DeleteDevice(ctx, "vm1.memdev0", false), api.CodeConflict)
	mustCode(t, e.c.DeleteDevice(ctx, "nosuch", false), api.CodeNotFound)
}

func TestAttachDetach(t *testing.T) {
	e := newTestEnv(t, "")
	a, status, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	if err != nil || status != 201 {
		t.Fatalf("attach: %d %v", status, err)
	}
	// prefers a slot that is not reserved for the local device (ds1_hb1)
	if a.State != api.AttachmentAttached || a.Slot.Bus != "ds0_hb0" || a.QemuDeviceID != "fcp_pooled0.hp1" ||
		a.QemuObjectID != "fcp_pooled0.hp1" || a.Serial != "0xc1ee0001" || a.ID != "pooled0@vm1" {
		t.Fatalf("unexpected attachment %+v", a)
	}
	f1 := e.fake("vm1")
	cmds := strings.Join(f1.Commands(), "\n")
	if !strings.Contains(cmds, "object_add memory-backend-file,id=fcp_pooled0.hp1,size=536870912,share=on,mem-path="+filepath.Join(e.dir, "pool", "pooled0.raw")+",align=2097152") ||
		!strings.Contains(cmds, "device_add cxl-type3,bus=ds0_hb0,volatile-memdev=fcp_pooled0.hp1,id=fcp_pooled0.hp1,sn=0xc1ee0001") {
		t.Fatalf("unexpected qemu commands:\n%s", cmds)
	}
	// idempotent
	a2, status, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	if err != nil || status != 200 || a2.QemuDeviceID != a.QemuDeviceID {
		t.Fatalf("second attach: %d %v %+v", status, err, a2)
	}
	// exclusive
	_, _, err = e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm2"})
	mustCode(t, err, api.CodeConflict)
	d, _ := e.c.Device(ctx, "pooled0")
	if d.State != api.DeviceAttached || len(d.Attachments) != 1 {
		t.Fatalf("unexpected device %+v", d)
	}
	h, _ := e.c.Host(ctx, "vm1")
	if h.Slots[0].Attachment != "pooled0@vm1" || h.Slots[0].Device != "fcp_pooled0.hp1" || h.HostBridges[0].AttachedBytes != 512<<20 {
		t.Fatalf("unexpected host %+v", h)
	}
	mustCode(t, e.c.DeleteDevice(ctx, "pooled0", false), api.CodeConflict)
	// detach: the guest has nothing to release in the fake
	da, status, err := e.c.Detach(ctx, "pooled0", "vm1", client.DetachOptions{})
	if err != nil || status != 200 || da.State != api.AttachmentDetached {
		t.Fatalf("detach: %d %v %+v", status, err, da)
	}
	if f1.HasDevice("fcp_pooled0.hp1") || f1.HasObject("fcp_pooled0.hp1") {
		t.Fatalf("device or object left in qemu: %s", f1)
	}
	_, _, err = e.c.Detach(ctx, "pooled0", "vm1", client.DetachOptions{})
	mustCode(t, err, api.CodeNotFound)
	// attach again: a new qemu id
	a3, status, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm2", NUMANode: new(1)})
	if err != nil || status != 201 || a3.QemuDeviceID != "fcp_pooled0.hp1" || a3.Slot.NUMANode != 1 || a3.Slot.Bus != "ds0_hb1" {
		t.Fatalf("attach to vm2: %d %v %+v", status, err, a3)
	}
	_, _, err = e.c.Detach(ctx, "pooled0", "vm2", client.DetachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a4, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm2"})
	if err != nil || a4.QemuDeviceID != "fcp_pooled0.hp2" {
		t.Fatalf("hotplug counter not used: %+v %v", a4, err)
	}
}

func TestAttachShared(t *testing.T) {
	e := newTestEnv(t, "")
	a1, _, err := e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1"})
	if err != nil {
		t.Fatal(err)
	}
	// host by uuid
	a2, _, err := e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm2"})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Serial != "0xc1ae0000" || a2.Serial != a1.Serial {
		t.Fatalf("serials differ: %s %s", a1.Serial, a2.Serial)
	}
	d, _ := e.c.Device(ctx, "shared0")
	if len(d.Attachments) != 2 || d.State != api.DeviceAttached {
		t.Fatalf("unexpected device %+v", d)
	}
	as, err := e.c.Attachments(ctx, "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "")
	if err != nil || len(as) != 1 || as[0].Host != "vm1" {
		t.Fatalf("attachments by uuid: %+v %v", as, err)
	}
	// detach from vm1 by uuid
	if _, _, err := e.c.Detach(ctx, "shared0", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", client.DetachOptions{}); err != nil {
		t.Fatal(err)
	}
	d, _ = e.c.Device(ctx, "shared0")
	if len(d.Attachments) != 1 || d.Attachments[0].Host != "vm2" {
		t.Fatalf("unexpected device %+v", d)
	}
	// delete with force detaches first; config devices cannot be deleted,
	// so test with a dynamic shared device
	if _, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "s1", Size: 256 << 20, Shared: true}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"vm1", "vm2"} {
		if _, _, err := e.c.Attach(ctx, "s1", api.AttachRequest{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.c.DeleteDevice(ctx, "s1", true); err != nil {
		t.Fatal(err)
	}
	_, err = e.c.Device(ctx, "s1")
	mustCode(t, err, api.CodeNotFound)
}

func TestAttachSingleHost(t *testing.T) {
	e := newTestEnv(t, "")
	// exclusive to vm1: vm2 cannot attach
	a1, _, err := e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", SingleHost: true})
	if err != nil || !a1.SingleHost {
		t.Fatalf("exclusive attach: %+v %v", a1, err)
	}
	_, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm2"})
	mustCode(t, err, api.CodeConflict)
	if !strings.Contains(err.Error(), "has a single-host attachment to") {
		t.Fatalf("conflict %v", err)
	}
	// an attach of vm1 without the flag drops it: vm2 can attach then
	a1, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1"})
	if err != nil || a1.SingleHost {
		t.Fatalf("attach again without exclusive: %+v %v", a1, err)
	}
	if _, _, err := e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm2"}); err != nil {
		t.Fatal(err)
	}
	// attached to two hosts: neither can become exclusive
	for _, h := range []string{"vm1", "vm2"} {
		_, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: h, SingleHost: true})
		mustCode(t, err, api.CodeConflict)
		if !strings.Contains(err.Error(), "needs it attached nowhere else") {
			t.Fatalf("conflict %v", err)
		}
	}
	// after the detach from vm2, vm1 becomes exclusive; the flag persists
	if _, _, err := e.c.Detach(ctx, "shared0", "vm2", client.DetachOptions{}); err != nil {
		t.Fatal(err)
	}
	if a1, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", SingleHost: true}); err != nil || !a1.SingleHost {
		t.Fatalf("exclusive after detach: %+v %v", a1, err)
	}
	as, err := e.c.Attachments(ctx, "", "shared0")
	if err != nil || len(as) != 1 || !as[0].SingleHost {
		t.Fatalf("attachments %+v %v", as, err)
	}
	// a detach ends the exclusive attachment: vm2 can attach
	if _, _, err := e.c.Detach(ctx, "shared0", "vm1", client.DetachOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm2"}); err != nil {
		t.Fatal(err)
	}
}

func TestDetachGuestHolds(t *testing.T) {
	e := newTestEnv(t, "")
	a, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	if err != nil {
		t.Fatal(err)
	}
	f1 := e.fake("vm1")
	f1.Hold(a.QemuDeviceID)
	da, _, err := e.c.Detach(ctx, "pooled0", "vm1", client.DetachOptions{Timeout: 100 * time.Millisecond})
	mustCode(t, err, api.CodeConflict)
	if da.State != api.AttachmentFailed || !strings.Contains(da.Error, "did not release") {
		t.Fatalf("unexpected attachment %+v", da)
	}
	d, _ := e.c.Device(ctx, "pooled0")
	if d.State != api.DeviceError {
		t.Fatalf("unexpected device state %s", d.State)
	}
	// attaching meanwhile is a conflict, here and elsewhere
	_, _, err = e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	mustCode(t, err, api.CodeConflict)
	_, _, err = e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm2"})
	mustCode(t, err, api.CodeConflict)
	// a repeated detach is a conflict and sends no second device_del
	_, _, err = e.c.Detach(ctx, "pooled0", "vm1", client.DetachOptions{Timeout: 50 * time.Millisecond})
	mustCode(t, err, api.CodeConflict)
	n := 0
	for _, c := range f1.Commands() {
		if strings.HasPrefix(c, "device_del") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected one device_del, got %d: %v", n, f1.Commands())
	}
	// the guest releases: the background waiter finalizes
	f1.Release(a.QemuDeviceID)
	e.waitDetached("pooled0", "vm1")
	if f1.HasObject(a.QemuObjectID) {
		t.Fatalf("object not deleted")
	}
}

func TestSlotSelection(t *testing.T) {
	// a slot reserved for a local device is used only when no other slot
	// is free, even if it is on the requested NUMA node
	e := newTestEnv(t, "")
	a, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", NUMANode: new(1)})
	if err != nil || a.Slot.Bus != "ds0_hb1" {
		t.Fatalf("attach on node 1: %+v %v", a, err)
	}
	// only ds1_hb1 (reserved for vm1.memdev0) is left on node 1
	a, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", NUMANode: new(1)})
	if err != nil || a.Slot.NUMANode != 0 {
		t.Fatalf("attach took the reserved slot: %+v %v", a, err)
	}

	e = newTestEnv(t, "")
	_, _, err = e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Slot: "nosuch"})
	mustCode(t, err, api.CodeInvalidArgument)
	a, _, err = e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", Slot: "ds1_hb0"})
	if err != nil || a.Slot.Bus != "ds1_hb0" || a.Slot.HostBridge != "cxlhb0" || a.Slot.Kind != api.SlotDownstream {
		t.Fatalf("explicit slot: %+v %v", a, err)
	}
	_, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", Slot: "ds1_hb0"})
	mustCode(t, err, api.CodeConflict)
	// a slot occupied by a device someone else plugged
	f1 := e.fake("vm1")
	f1.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendRAM, ID: "other", Size: 256 << 20})
	if err := f1.PlugDevice(qemu.CXLType3{ID: "foreign", Bus: "ds0_hb0", VolatileMemdev: "other", Serial: 7}); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", Slot: "ds0_hb0"})
	mustCode(t, err, api.CodeConflict)
	// numa 0 is full now: falls back to numa 1, avoiding the local device slot
	a, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", NUMANode: new(0)})
	if err != nil || a.Slot.Bus != "ds0_hb1" {
		t.Fatalf("fallback slot: %+v %v", a, err)
	}
	h, _ := e.c.Host(ctx, "vm1")
	if h.Slots[0].Device != "foreign" || h.Slots[0].Attachment != "" {
		t.Fatalf("unexpected foreign slot %+v", h.Slots[0])
	}
	// the last free slot is the local device's slot: a pool device takes it
	if _, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "d3", Size: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	a, _, err = e.c.Attach(ctx, "d3", api.AttachRequest{Host: "vm1"})
	if err != nil || a.Slot.Bus != "ds1_hb1" {
		t.Fatalf("last slot: %+v %v", a, err)
	}
	if _, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "d4", Size: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.c.Attach(ctx, "d4", api.AttachRequest{Host: "vm1"})
	mustCode(t, err, api.CodeConflict)
}

func TestLocalDevice(t *testing.T) {
	e := newTestEnv(t, "")
	d, err := e.c.Device(ctx, "vm1.memdev0")
	if err != nil || d.Scope != api.ScopeLocal || d.LocalHost != "vm1" || d.Serial != "0xc100e2e0" || d.Size != 256<<20 || d.Backend != api.BackendRAM {
		t.Fatalf("unexpected local device %+v %v", d, err)
	}
	_, _, err = e.c.Attach(ctx, "vm1.memdev0", api.AttachRequest{Host: "vm2"})
	mustCode(t, err, api.CodeInvalidArgument)
	a, _, err := e.c.Attach(ctx, "vm1.memdev0", api.AttachRequest{Host: "vm1"})
	if err != nil || a.Slot.Bus != "ds1_hb1" || a.QemuObjectID != localObj || a.Serial != "0xc100e2e0" {
		t.Fatalf("unexpected attachment %+v %v", a, err)
	}
	f1 := e.fake("vm1")
	for _, c := range f1.Commands() {
		if strings.HasPrefix(c, "object_add") {
			t.Fatalf("object_add for a local device: %s", c)
		}
	}
	if _, _, err := e.c.Detach(ctx, "vm1.memdev0", "vm1", client.DetachOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f1.Commands() {
		if strings.HasPrefix(c, "object_del") {
			t.Fatalf("object_del for a local device: %s", c)
		}
	}
	if !f1.HasObject(localObj) {
		t.Fatalf("local backend deleted")
	}
}

// A failed device_add deletes the backends that the attach added: the
// device is free, and the attach can be retried.
func TestDeviceAddFailure(t *testing.T) {
	e := newTestEnv(t, llm0Config)
	f1 := e.fake("vm1")
	for _, tc := range []struct {
		device     string
		persistent bool // the backends are the data and the label storage area
	}{
		{"pooled0", false},
		{"llm0", true},
	} {
		n := len(f1.Commands())
		f1.FailNextDeviceAdd(&qemu.Error{Command: "device_add", Desc: "something broke"})
		a, _, err := e.c.Attach(ctx, tc.device, api.AttachRequest{Host: "vm1"})
		mustCode(t, err, api.CodeUnavailable)
		if !strings.Contains(err.Error(), "something broke") || a.State != api.AttachmentFailed {
			t.Fatalf("%s: unexpected error %v %+v", tc.device, err, a)
		}
		ids := []string{a.QemuObjectID}
		if tc.persistent {
			ids = append(ids, a.QemuLSAObjectID)
		}
		// object_add of each backend, the device_add, object_del of each
		var dels []string
		for _, id := range ids {
			if f1.HasObject(id) {
				t.Fatalf("%s: backend %s left after the failed device_add: %s", tc.device, id, f1)
			}
			dels = append(dels, "object_del "+id)
		}
		got := f1.Commands()[n:]
		if len(got) != 2*len(ids)+1 || !strings.HasPrefix(got[len(ids)], "device_add ") || !reflect.DeepEqual(got[len(ids)+1:], dels) {
			t.Fatalf("%s: qemu commands:\n%s", tc.device, strings.Join(got, "\n"))
		}
		d, _ := e.c.Device(ctx, tc.device)
		if d.State != api.DeviceFree || len(d.Attachments) != 0 {
			t.Fatalf("%s: unexpected device %+v", tc.device, d)
		}
		if _, _, err := e.c.Attach(ctx, tc.device, api.AttachRequest{Host: "vm1"}); err != nil {
			t.Fatalf("%s: retry: %v", tc.device, err)
		}
	}
}

func TestAdoptAndReconcile(t *testing.T) {
	e := newTestEnv(t, "")
	f1 := e.fake("vm1")
	// someone (vm-cxl-hotplug) plugs the local device
	if err := f1.PlugDevice(qemu.CXLType3{ID: "cxl_memdev0.hp1", Bus: "ds1_hb1", VolatileMemdev: localObj, Serial: 0xc100e2e0}); err != nil {
		t.Fatal(err)
	}
	e.srv.Refresh(ctx)
	a, err := e.attachment("vm1.memdev0", "vm1")
	if err != nil || !a.Adopted || a.QemuDeviceID != "cxl_memdev0.hp1" || a.State != api.AttachmentAttached {
		t.Fatalf("not adopted: %+v %v", a, err)
	}
	// someone removes it behind the server's back
	if err := f1.DeviceDel(ctx, "cxl_memdev0.hp1"); err != nil {
		t.Fatal(err)
	}
	e.srv.Refresh(ctx)
	_, err = e.attachment("vm1.memdev0", "vm1")
	mustCode(t, err, api.CodeNotFound)
}

func TestPersistenceAndRestart(t *testing.T) {
	e := newTestEnv(t, "")
	if _, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "dyn", Size: 256 << 20, Shared: true}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"vm1", "vm2"} {
		if _, _, err := e.c.Attach(ctx, "dyn", api.AttachRequest{Host: h, Owner: "k8s:" + h}); err != nil {
			t.Fatal(err)
		}
	}
	e.stop()
	// server restart: same qemus
	e.start()
	d, err := e.c.Device(ctx, "dyn")
	if err != nil || len(d.Attachments) != 2 || d.Serial != "0xc1ae0001" {
		t.Fatalf("device not restored: %+v %v", d, err)
	}
	// attachment owners survive
	if d.Attachments[0].Owner != "k8s:"+d.Attachments[0].Host || d.Attachments[1].Owner != "k8s:"+d.Attachments[1].Host {
		t.Fatalf("attachment owners not restored: %+v", d.Attachments)
	}
	// hotplug counters survive
	if _, _, err := e.c.Detach(ctx, "dyn", "vm1", client.DetachOptions{}); err != nil {
		t.Fatal(err)
	}
	a, _, err := e.c.Attach(ctx, "dyn", api.AttachRequest{Host: "vm1"})
	if err != nil || a.QemuDeviceID != "fcp_dyn.hp2" {
		t.Fatalf("counter not restored: %+v %v", a, err)
	}
	// qemu of vm2 restarts: its attachments are dropped
	e.stop()
	e.mu.Lock()
	e.procs[1] = vmCmdline("vm2", 202)
	e.alive[202] = true
	delete(e.alive, 102)
	e.fakes["/e2e/vm2/qmp.sock"] = newVMFake()
	e.mu.Unlock()
	e.start()
	as, err := e.c.Attachments(ctx, "", "dyn")
	if err != nil || len(as) != 1 || as[0].Host != "vm1" {
		t.Fatalf("unexpected attachments after qemu restart: %+v %v", as, err)
	}
	h, _ := e.c.Host(ctx, "vm2")
	if h.PID != 202 || h.State != api.HostRunning {
		t.Fatalf("unexpected host %+v", h)
	}
	// vm1 stops
	e.mu.Lock()
	e.procs = e.procs[1:]
	delete(e.alive, 101)
	e.mu.Unlock()
	e.srv.Refresh(ctx)
	h, _ = e.c.Host(ctx, "vm1")
	if h.State != api.HostStopped || len(h.Attachments) != 0 {
		t.Fatalf("unexpected stopped host %+v", h)
	}
	_, _, err = e.c.Attach(ctx, "dyn", api.AttachRequest{Host: "vm1"})
	mustCode(t, err, api.CodeUnavailable)
}

// subscribers returns the number of subscribers of the broker.
func (b *broker) subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func TestEvents(t *testing.T) {
	e := newTestEnv(t, "")
	ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got := make(chan client.Event, 100)
	go func() {
		_ = e.c.Events(ectx, func(ev client.Event) error {
			got <- ev
			return nil
		})
	}()
	// the stream subscribes before the attach
	for e.srv.events.subscribers() == 0 {
		if ectx.Err() != nil {
			t.Fatal("no event subscription")
		}
		time.Sleep(time.Millisecond)
	}
	if _, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-got:
			if ev.Type == api.EventAttachmentUpdated && strings.Contains(string(ev.Object), `"state":"attached"`) {
				return
			}
		case <-ectx.Done():
			t.Fatal("no attachment.updated event")
		}
	}
}

// An open event stream does not hold up the shutdown of the HTTP server.
func TestShutdownWithEventStream(t *testing.T) {
	e := newTestEnv(t, "")
	hs := httptest.NewUnstartedServer(e.srv.Handler())
	hs.Config.RegisterOnShutdown(e.srv.CloseEvents)
	hs.Start()
	defer hs.Close()
	resp, err := http.Get(hs.URL + api.PathEvents())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != ": fake-cxl-pool events\n" {
		t.Fatalf("event stream: %q %v", line, err)
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := hs.Config.Shutdown(sctx); err != nil {
		t.Fatalf("shutdown with an open event stream: %v", err)
	}
}

// The background waiter of a detach and a reconcile that finds the
// device leaked do not race (run with -race).
func TestLeakedWhileWaiting(t *testing.T) {
	e := newTestEnv(t, "")
	a, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	if err != nil {
		t.Fatal(err)
	}
	e.fake("vm1").Hold(a.QemuDeviceID)
	// the detach times out, the background waiter polls the device
	_, _, err = e.c.Detach(ctx, "pooled0", "vm1", client.DetachOptions{Timeout: 50 * time.Millisecond})
	mustCode(t, err, api.CodeConflict)
	e.srv.mu.RLock()
	att := e.srv.atts[a.ID]
	e.srv.mu.RUnlock()
	time.Sleep(100 * time.Millisecond)
	e.srv.markLeaked(att)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		e.srv.mu.RLock()
		waiting := att.bgWaiting
		e.srv.mu.RUnlock()
		if !waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background waiter of a leaked attachment did not end")
		}
	}
}

func TestConfigErrors(t *testing.T) {
	for _, bad := range []string{
		"devices: [{name: x}]",
		"devices: [{name: x, size: 256Mi}]",
		"devices: [{name: x, size: 256Mi, serial: 1}, {name: x, size: 256Mi, serial: 2}]",
		"unknownKey: 1",
		"discovery: {interval: -1s}",
		"detachTimeout: -5s",
		"discovery: {control: x}",
		`discovery: {slots: ["["]}`,
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	for _, bad := range []string{"Pool-A", "-a", "a-", "a_b", "a.b", strings.Repeat("a", 64)} {
		_, err := ParseConfig([]byte("name: " + bad + "\n"))
		if err == nil || err.Error() != "name must be a DNS label (lower-case letters, digits, '-')" {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	for _, good := range []string{"pool-a", strings.Repeat("a", 63)} {
		if cfg, err := ParseConfig([]byte("name: " + good + "\n")); err != nil || cfg.Name != good {
			t.Errorf("name %q: %+v %v", good, cfg, err)
		}
	}
	cfg := DefaultConfig()
	if cfg.Listen != "127.0.0.1:9909" || cfg.Dir != "/tmp/fake-cxl-pool" || cfg.StateFile != "/tmp/fake-cxl-pool.state.json" ||
		uint64(*cfg.SerialBase) != 0xc1ee0000 || uint64(*cfg.SharedSerialBase) != 0xc1ae0000 ||
		cfg.Name != "fake-cxl-pool" || cfg.Discovery.Control != "" || cfg.Discovery.Slots != nil {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
}

func TestLeakedDetach(t *testing.T) {
	e := newTestEnv(t, "")
	a, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	if err != nil {
		t.Fatal(err)
	}
	f1 := e.fake("vm1")
	f1.SetLeakOnRelease(true)
	f1.Hold(a.QemuDeviceID)
	_, _, err = e.c.Detach(ctx, "pooled0", "vm1", client.DetachOptions{Timeout: 50 * time.Millisecond})
	mustCode(t, err, api.CodeConflict)
	// the guest removes the device with its memory still online
	f1.Release(a.QemuDeviceID)
	deadline := time.Now().Add(5 * time.Second)
	var cur *api.Attachment
	for {
		cur, err = e.attachment("pooled0", "vm1")
		if err == nil && cur.State == api.AttachmentFailed && strings.Contains(cur.Error, "leaked") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not marked leaked: %+v %v", cur, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(cur.Error, "leaked") {
		t.Fatalf("unexpected error %q", cur.Error)
	}
	d, _ := e.c.Device(ctx, "pooled0")
	if d.State != api.DeviceError {
		t.Fatalf("unexpected device state %s", d.State)
	}
	// not handed to another host, the slot is free for others
	_, _, err = e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm2"})
	mustCode(t, err, api.CodeConflict)
	a2, _, err := e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", Slot: a.Slot.Bus})
	if err != nil || a2.Slot.Bus != a.Slot.Bus {
		t.Fatalf("slot of a leaked device not reusable: %+v %v", a2, err)
	}
	// forget with force
	_, _, err = e.c.Detach(ctx, "pooled0", "vm1", client.DetachOptions{})
	mustCode(t, err, api.CodeConflict)
	if _, _, err := e.c.Detach(ctx, "pooled0", "vm1", client.DetachOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.c.Device(ctx, "pooled0"); d.State != api.DeviceFree {
		t.Fatalf("unexpected device state %s", d.State)
	}
	// a device_del that finds no device still checks whether the backend
	// leaked: a surprise removal behind the server, memory kept online in
	// the guest
	a, _, err = e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm2"})
	if err != nil {
		t.Fatal(err)
	}
	f2 := e.fake("vm2")
	f2.SetLeakOnRelease(true)
	f2.Hold(a.QemuDeviceID)
	if err := f2.DeviceDel(ctx, a.QemuDeviceID); err != nil {
		t.Fatal(err)
	}
	f2.Release(a.QemuDeviceID)
	da, _, err := e.c.Detach(ctx, "pooled0", "vm2", client.DetachOptions{Timeout: time.Second})
	mustCode(t, err, api.CodeConflict)
	if da.State != api.AttachmentFailed || !strings.Contains(da.Error, "leaked") {
		t.Fatalf("leak not detected: %+v", da)
	}
}

func TestUUIDWithoutDashesAndFilters(t *testing.T) {
	e := newTestEnv(t, "")
	r, err := e.c.Resolve(ctx, "", "6ba7b8109dad11d180b400c04fd430c8")
	if err != nil || r.Name != "vm1" {
		t.Fatalf("resolve by machine-id: %+v %v", r, err)
	}
	if _, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "6BA7B8109DAD11D180B400C04FD430C8"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.c.Detach(ctx, "pooled0", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", client.DetachOptions{}); err != nil {
		t.Fatal(err)
	}
	e.stop()
	e.cfg.Discovery.Names = []string{"vm2*"}
	e.start()
	hs, err := e.c.Hosts(ctx)
	if err != nil || len(hs) != 1 || hs[0].Name != "vm2" {
		t.Fatalf("discovery filter: %+v %v", hs, err)
	}
}

func TestReservedSocketSkipped(t *testing.T) {
	e := newTestEnv(t, "")
	e.stop()
	e.mu.Lock()
	p := vmCmdline("vm3", 103)
	// qmp-e2e.sock first: it must not be used
	p.QMPSockets = append([]string{"/e2e/vm3/qmp-e2e.sock"}, p.QMPSockets...)
	e.procs = append(e.procs, p)
	e.alive[103] = true
	e.fakes["/e2e/vm3/qmp.sock"] = newVMFake()
	e.fakes["/e2e/vm3/qmp-e2e.sock"] = newVMFake()
	e.mu.Unlock()
	e.start()
	h, err := e.c.Host(ctx, "vm3")
	if err != nil || h.QMP != "/e2e/vm3/qmp.sock" || h.State != api.HostRunning {
		t.Fatalf("unexpected host %+v %v", h, err)
	}
}

func TestFMWCapacityAndHotRemove(t *testing.T) {
	e := newTestEnv(t, "")
	e.stop()
	f1 := e.fake("vm1")
	// 512M window on cxlhb0, 256M on cxlhb1 (as reported by qemu)
	f1.SetFMW([]qemu.FMWWindow{{Targets: []string{"cxlhb0"}, Size: 512 << 20}, {Targets: []string{"cxlhb1"}, Size: 256 << 20}})
	f2 := e.fake("vm2")
	f2.SetStock(true)
	// one 1G window interleaved over both host bridges
	f2.SetFMW([]qemu.FMWWindow{{Targets: []string{"cxlhb0", "cxlhb1"}, Size: 1 << 30}})
	e.start()
	h1, _ := e.c.Host(ctx, "vm1")
	h2, _ := e.c.Host(ctx, "vm2")
	if !h1.HotRemoveCapable || h2.HotRemoveCapable {
		t.Fatalf("unexpected hotRemoveCapable %v %v", h1.HotRemoveCapable, h2.HotRemoveCapable)
	}
	if h1.HostBridges[0].FMWSize != 512<<20 {
		t.Fatalf("fmw from qemu not used: %+v", h1.HostBridges)
	}
	// its targets share an interleaved window
	if h2.HostBridges[0].FMWSize != 512<<20 || h2.HostBridges[1].FMWSize != 512<<20 {
		t.Fatalf("interleaved fmw counted per target: %+v", h2.HostBridges)
	}
	// pooled0 (512M) fills cxlhb0
	a, _, err := e.c.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1", NUMANode: new(0)})
	if err != nil || a.Slot.HostBridge != "cxlhb0" {
		t.Fatalf("attach: %+v %v", a, err)
	}
	// shared0 (256M) does not fit under cxlhb0 any more
	_, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", Slot: "ds1_hb0"})
	mustCode(t, err, api.CodeConflict)
	a, _, err = e.c.Attach(ctx, "shared0", api.AttachRequest{Host: "vm1", NUMANode: new(0)})
	if err != nil || a.Slot.HostBridge != "cxlhb1" {
		t.Fatalf("attach falls back to cxlhb1: %+v %v", a, err)
	}
	if _, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "d1", Size: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.c.Attach(ctx, "d1", api.AttachRequest{Host: "vm1"})
	mustCode(t, err, api.CodeConflict)
	if !strings.Contains(err.Error(), "room") {
		t.Fatalf("unexpected error %v", err)
	}
}

// A device that a stock qemu does not delete keeps its failed attachment
// and its slot. After a forced detach it is not adopted.
func TestZombieNotAdopted(t *testing.T) {
	e := newTestEnv(t, "")
	f1 := e.fake("vm1")
	f1.SetStock(true)
	a, _, err := e.c.Attach(ctx, "vm1.memdev0", api.AttachRequest{Host: "vm1"})
	if err != nil {
		t.Fatal(err)
	}
	da, _, err := e.c.Detach(ctx, "vm1.memdev0", "vm1", client.DetachOptions{Timeout: 50 * time.Millisecond})
	mustCode(t, err, api.CodeConflict)
	if da.State != api.AttachmentFailed {
		t.Fatalf("unexpected attachment %+v", da)
	}
	// reconcile keeps it: the device is still in qemu
	e.srv.Refresh(ctx)
	if cur, err := e.attachment("vm1.memdev0", "vm1"); err != nil || cur.State != api.AttachmentFailed {
		t.Fatalf("unexpected attachment %+v %v", cur, err)
	}
	if _, _, err := e.c.Detach(ctx, "vm1.memdev0", "vm1", client.DetachOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	// the zombie is still in qemu: it must not come back as attached,
	// neither now nor after a server restart
	for i := range 2 {
		e.srv.Refresh(ctx)
		if _, err := e.attachment("vm1.memdev0", "vm1"); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("zombie %s adopted (round %d): %v", a.QemuDeviceID, i, err)
		}
		h, _ := e.c.Host(ctx, "vm1")
		if h.Slots[3].Device != a.QemuDeviceID {
			t.Fatalf("zombie slot looks free: %+v", h.Slots[3])
		}
		e.stop()
		e.start()
	}
}

// A client that gives up does not turn the detach into a timeout.
func TestDetachIgnoresClientCancel(t *testing.T) {
	f := newVMFake()
	srv := newServerWith(t, poolConfig(t.TempDir(), pooled0), map[string]qemu.Monitor{"vm1": f})
	a, _, err := srv.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	if err != nil {
		t.Fatal(err)
	}
	f.Hold(a.QemuDeviceID)
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.Release(a.QemuDeviceID)
	}()
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	da, res, err := srv.Detach(cctx, "pooled0", "vm1", DetachOptions{Timeout: 5 * time.Second})
	if err != nil || res != DetachDone || da.State != api.AttachmentDetached {
		t.Fatalf("detach after client cancel: %v %v %+v", res, err, da)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatalf("detach returned before the guest released the device")
	}
	if f.HasObject(a.QemuObjectID) {
		t.Fatalf("object-del skipped")
	}
}

// slowAddMon is a fake qemu whose device_add takes a while.
type slowAddMon struct{ *fake.Qemu }

func (s slowAddMon) DeviceAdd(ctx context.Context, d qemu.CXLType3) error {
	time.Sleep(30 * time.Millisecond)
	return s.Qemu.DeviceAdd(ctx, d)
}

// Concurrent attaches and detaches of an attaching device do not race
// (run with -race).
func TestConcurrentAttachDetachNoRace(t *testing.T) {
	srv := newServerWith(t, poolConfig(t.TempDir(), pooled0), map[string]qemu.Monitor{"vm1": slowAddMon{newVMFake()}})
	var wg sync.WaitGroup
	attach := func() { _, _, _ = srv.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"}) }
	wg.Go(attach)
	for range 20 {
		wg.Go(attach)
		_, _, _ = srv.Detach(ctx, "pooled0", "vm1", DetachOptions{Timeout: time.Second})
		time.Sleep(3 * time.Millisecond)
	}
	wg.Wait()
}

// The hotplug counter is seeded from the device ids qemu already has.
func TestHotplugCounterSeededFromQemu(t *testing.T) {
	dir := t.TempDir()
	f := newVMFake()
	// leftovers of a previous server run without a state file: a device
	// of an unknown device, an object whose object-del failed and the LSA
	// backend of a persistent device
	obj := "fcp_gone.hp3"
	f.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: obj, Size: 256 << 20, MemPath: filepath.Join(dir, "x.raw"), Share: true})
	if err := f.PlugDevice(qemu.CXLType3{ID: obj, Bus: "ds0_hb0", VolatileMemdev: obj, Serial: 1}); err != nil {
		t.Fatal(err)
	}
	f.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: "fcp_pooled0.hp5", Size: 256 << 20, MemPath: filepath.Join(dir, "y.raw")})
	f.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: "fcp_pooled0.hp7-lsa", Size: lsaSize, MemPath: filepath.Join(dir, "y.lsa")})
	srv := newServerWith(t, poolConfig(dir, pooled0), map[string]qemu.Monitor{"vm1": f})
	a, _, err := srv.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	if err != nil || a.QemuDeviceID != "fcp_pooled0.hp8" {
		t.Fatalf("attach: %+v %v", a, err)
	}
}

// A pool device cannot have the serial of a local device of the host it is
// attached to.
func TestSerialCollisionWithLocalDevice(t *testing.T) {
	cfg := "devices:\n  - name: cfg0\n    size: 256M\n    serial: 0xc100e2e0\n"
	f1, f2 := newVMFake(), newVMFake()
	for _, f := range []*fake.Qemu{f1, f2} {
		f.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendRAM, ID: localObj, Size: 256 << 20, Share: true})
	}
	srv := newServerWith(t, poolConfig(t.TempDir(), cfg), map[string]qemu.Monitor{"vm1": f1, "vm2": f2})
	_, err := srv.CreateDevice(api.DeviceCreate{Name: "d1", Size: api.Size(256 << 20), Serial: "0xc100e2e0"})
	mustCode(t, err, api.CodeConflict)
	// vm1 and vm2 both have the local device with serial 0xc100e2e0
	_, _, err = srv.Attach(ctx, "cfg0", api.AttachRequest{Host: "vm1"})
	mustCode(t, err, api.CodeConflict)
	// local devices of different hosts may share a serial
	for _, h := range []string{"vm1", "vm2"} {
		if _, _, err := srv.Attach(ctx, h+".memdev0", api.AttachRequest{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
}
