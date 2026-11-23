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

package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHMPServer emulates a qemu readline monitor socket.
type fakeHMPServer struct {
	path    string
	l       net.Listener
	mu      sync.Mutex
	handler func(cmd string) string
	cmds    []string
}

func newFakeHMPServer(t *testing.T, handler func(string) string) *fakeHMPServer {
	t.Helper()
	s := &fakeHMPServer{path: filepath.Join(t.TempDir(), "monitor.sock"), handler: handler}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	s.l = l
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			s.serve(c) // one client at a time, like qemu
		}
	}()
	return s
}

func (s *fakeHMPServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("QEMU 11.1.1 monitor - type 'help' for more information\r\n(qemu) "))
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		s.mu.Lock()
		s.cmds = append(s.cmds, cmd)
		s.mu.Unlock()
		var echo strings.Builder
		for i := 1; i <= len(cmd); i++ {
			echo.WriteString("\x1b[K" + strings.Repeat("\x1b[D", i-1) + cmd[:i])
		}
		echo.WriteString("\x1b[K\r\n")
		out := s.handler(cmd)
		if out != "" {
			out = strings.ReplaceAll(out, "\n", "\r\n") + "\r\n"
		}
		// write in pieces to exercise the reader
		resp := echo.String() + out + "(qemu) "
		for len(resp) > 0 {
			n := min(len(resp), 100)
			_, _ = c.Write([]byte(resp[:n]))
			resp = resp[n:]
		}
	}
}

func (s *fakeHMPServer) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func TestHMPClient(t *testing.T) {
	qtree := CleanHMPOutput(rawSession(t, "hmp-raw-info-qtree-full-crio.txt"))
	var polls atomic.Int32
	srv := newFakeHMPServer(t, func(cmd string) string {
		switch {
		case cmd == "info version":
			return "11.1.1openSUSE Slowroll"
		case cmd == "info qtree":
			return qtree
		case cmd == "info qtree -b":
			if polls.Add(1) < 3 {
				return `  dev: cxl-type3, id "fcp_d0.hp1"`
			}
			return ""
		case cmd == "device_del nosuch":
			return "Error: Device 'nosuch' not found"
		case strings.HasPrefix(cmd, "device_add"), strings.HasPrefix(cmd, "object_add"),
			strings.HasPrefix(cmd, "device_del"), strings.HasPrefix(cmd, "object_del"):
			return ""
		}
		return "unknown command: '" + cmd + "'"
	})
	h := NewHMP(srv.path, t.Logf)
	h.PollInterval = 10 * time.Millisecond
	ctx := context.Background()
	v, err := h.Version(ctx)
	if err != nil || v != "11.1.1 openSUSE Slowroll" {
		t.Fatalf("version: %q %v", v, err)
	}
	tree, err := h.QueryTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if treeDevice(tree, "cxl_memdev2.hp3") == nil || tree.HostBridge("cxlhb1").NUMANode != 1 {
		t.Fatalf("unexpected tree %+v", tree)
	}
	err = h.DeviceDel(ctx, "nosuch")
	if !IsNotFound(err) {
		t.Fatalf("expected not found error, got %v", err)
	}
	if err := h.ObjectAdd(ctx, MemoryBackend{QomType: MemoryBackendFile, ID: "fcp_d0.hp1", Size: 1 << 28, MemPath: "/tmp/x.raw", Share: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.DeviceAdd(ctx, CXLType3{ID: "fcp_d0.hp1", Bus: "ds0", VolatileMemdev: "fcp_d0.hp1", Serial: 0xc1f00001}); err != nil {
		t.Fatal(err)
	}
	if err := h.DeviceDel(ctx, "fcp_d0.hp1"); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.WaitDeviceDeleted(wctx, "fcp_d0.hp1"); err != nil {
		t.Fatal(err)
	}
	if err := h.ObjectDel(ctx, "fcp_d0.hp1"); err != nil {
		t.Fatal(err)
	}
	// a command without output
	if out, err := h.Command(ctx, "object_del x"); err != nil || out != "" {
		t.Fatalf("object_del output %q %v", out, err)
	}
	cmds := strings.Join(srv.commands(), "\n")
	for _, want := range []string{
		"device_add cxl-type3,bus=ds0,volatile-memdev=fcp_d0.hp1,id=fcp_d0.hp1,sn=0xc1f00001",
		"object_del fcp_d0.hp1",
	} {
		if !strings.Contains(cmds, want) {
			t.Fatalf("%q not seen: %v", want, cmds)
		}
	}
}

func TestHMPBusySocketTimesOut(t *testing.T) {
	// a server that accepts but never greets (another client holds qemu's
	// single monitor connection)
	path := filepath.Join(t.TempDir(), "busy.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	h := NewHMP(path, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := h.Version(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

// fakeQMPServer emulates a qemu QMP socket.
type fakeQMPServer struct {
	path    string
	mu      sync.Mutex
	devices map[string]bool
	devArgs map[string]map[string]any
	qom     bool // serve qom-list /machine/peripheral
	reqs    []map[string]json.RawMessage
	conns   atomic.Int32
	closed  atomic.Int32
	holdDel bool // do not complete device_del
}

func newFakeQMPServer(t *testing.T) *fakeQMPServer {
	t.Helper()
	s := &fakeQMPServer{path: filepath.Join(t.TempDir(), "qmp.sock"), devices: map[string]bool{}, devArgs: map[string]map[string]any{}}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			s.serve(c)
			s.closed.Add(1)
		}
	}()
	return s
}

func (s *fakeQMPServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	var wmu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		_, _ = c.Write(append(b, '\r', '\n'))
		wmu.Unlock()
	}
	send(map[string]any{"QMP": map[string]any{"version": map[string]any{}, "capabilities": []string{"oob"}}})
	dec := json.NewDecoder(c)
	negotiated := false
	for {
		var req map[string]json.RawMessage
		if err := dec.Decode(&req); err != nil {
			return
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, req)
		s.mu.Unlock()
		var cmd string
		_ = json.Unmarshal(req["execute"], &cmd)
		var args map[string]any
		_ = json.Unmarshal(req["arguments"], &args)
		id := req["id"]
		ret := func(v any) { send(map[string]any{"return": v, "id": id}) }
		fail := func(class, desc string) {
			send(map[string]any{"error": map[string]any{"class": class, "desc": desc}, "id": id})
		}
		if !negotiated && cmd != "qmp_capabilities" {
			fail("CommandNotFound", "Expecting capabilities negotiation with 'qmp_capabilities'")
			continue
		}
		switch cmd {
		case "qmp_capabilities":
			negotiated = true
			ret(map[string]any{})
		case "query-version":
			ret(map[string]any{"qemu": map[string]any{"major": 10, "minor": 1, "micro": 50}, "package": "v10.1.0-1-gff076b1e6c"})
		case "object-add", "object-del":
			ret(map[string]any{})
		case "device_add":
			if _, ok := args["sn"].(float64); !ok {
				fail("GenericError", "Invalid parameter type for 'sn', expected: integer")
				continue
			}
			s.mu.Lock()
			s.devices[args["id"].(string)] = true
			s.devArgs[args["id"].(string)] = args
			s.mu.Unlock()
			ret(map[string]any{})
		case "device_del":
			dev := args["id"].(string)
			s.mu.Lock()
			ok := s.devices[dev]
			hold := s.holdDel
			s.mu.Unlock()
			if !ok {
				fail("DeviceNotFound", "Device '"+dev+"' not found")
				continue
			}
			ret(map[string]any{})
			if !hold {
				go func() {
					time.Sleep(20 * time.Millisecond)
					s.mu.Lock()
					delete(s.devices, dev)
					s.mu.Unlock()
					send(map[string]any{"event": "DEVICE_DELETED", "data": map[string]any{"device": dev, "path": "/machine/peripheral/" + dev},
						"timestamp": map[string]any{"seconds": 1, "microseconds": 2}})
				}()
			}
		case "qom-list":
			s.mu.Lock()
			qom := s.qom
			s.mu.Unlock()
			if !qom || args["path"] != "/machine/peripheral" {
				fail("DeviceNotFound", "Device '"+args["path"].(string)+"' not found")
				continue
			}
			items := []map[string]string{{"name": "type", "type": "string"}}
			for n, typ := range fakeQOMPorts {
				items = append(items, map[string]string{"name": n, "type": "child<" + typ[0] + ">"})
			}
			s.mu.Lock()
			for d := range s.devices {
				items = append(items, map[string]string{"name": d, "type": "child<cxl-type3>"})
			}
			s.mu.Unlock()
			ret(items)
		case "qom-get":
			dev := strings.TrimPrefix(args["path"].(string), "/machine/peripheral/")
			prop := args["property"].(string)
			if pt, ok := fakeQOMPorts[dev]; ok {
				switch prop {
				case "parent_bus":
					ret(pt[1])
				case "numa_node":
					ret(0)
				default:
					fail("GenericError", "Property '"+prop+"' not found")
				}
				continue
			}
			s.mu.Lock()
			ok := s.devices[dev]
			da := s.devArgs[dev]
			s.mu.Unlock()
			if !ok {
				fail("DeviceNotFound", "Device '"+args["path"].(string)+"' not found")
				continue
			}
			switch prop {
			case "type":
				ret("cxl-type3")
			case "parent_bus":
				ret("/machine/peripheral/" + da["bus"].(string) + "/" + da["bus"].(string))
			case "sn":
				ret(da["sn"])
			case "volatile-memdev", "persistent-memdev", "lsa":
				// an unset link property reads as ""
				if v, ok := da[prop].(string); ok {
					ret("/objects/" + v)
				} else {
					ret("")
				}
			default:
				fail("GenericError", "Property '"+prop+"' not found")
			}
		case "query-memdev":
			ret([]map[string]any{{"id": "m0", "size": 268435456, "merge": true, "dump": true, "prealloc": false, "share": true, "host-nodes": []int{}, "policy": "default"}})
		default:
			fail("CommandNotFound", "The command "+cmd+" has not been found")
		}
	}
}

// fakeQOMPorts: id -> {driver, parent_bus}
var fakeQOMPorts = map[string][2]string{
	"cxlhb0":    {"pxb-cxl", "/machine/q35/pcie.0"},
	"cxlrp0hb0": {"cxl-rp", "/machine/unattached/device[35]/cxlhb0"},
	"sw0":       {"cxl-upstream", "/machine/peripheral/cxlrp0hb0/cxlrp0hb0"},
	"ds0":       {"cxl-downstream", "/machine/peripheral/sw0/sw0"},
	"ds1":       {"cxl-downstream", "/machine/peripheral/sw0/sw0"},
	"cxlrp1hb0": {"cxl-rp", "/machine/unattached/device[35]/cxlhb0"},
}

func (s *fakeQMPServer) requests(cmd string) []map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]json.RawMessage
	for _, r := range s.reqs {
		var c string
		_ = json.Unmarshal(r["execute"], &c)
		if c == cmd {
			out = append(out, r)
		}
	}
	return out
}

func TestQMPClient(t *testing.T) {
	srv := newFakeQMPServer(t)
	srv.qom = true
	q := NewQMP(srv.path, t.Logf)
	q.PollInterval = time.Second
	ctx := context.Background()
	v, err := q.Version(ctx)
	if err != nil || v != "10.1.50 v10.1.0-1-gff076b1e6c" {
		t.Fatalf("version %q %v", v, err)
	}
	tree, err := q.QueryTree(ctx)
	if err != nil || len(tree.Ports) != 4 {
		t.Fatalf("tree %+v %v", tree, err)
	}
	mds, err := q.QueryMemdevs(ctx)
	if err != nil || len(mds) != 1 || mds[0].Size != 256<<20 {
		t.Fatalf("memdevs %+v %v", mds, err)
	}
	b := MemoryBackend{QomType: MemoryBackendFile, ID: "fcp_d0.hp1", Size: 256 << 20, MemPath: "/tmp/d0.raw", Share: true}
	if err := q.ObjectAdd(ctx, b); err != nil {
		t.Fatal(err)
	}
	d := CXLType3{ID: "fcp_d0.hp1", Bus: "cxlrp0hb0", VolatileMemdev: "fcp_d0.hp1", Serial: 0xc1f00001}
	if err := q.DeviceAdd(ctx, d); err != nil {
		t.Fatal(err)
	}
	reqs := srv.requests("device_add")
	if len(reqs) != 1 || !strings.Contains(string(reqs[0]["arguments"]), `"sn":3253731329`) {
		t.Fatalf("unexpected device_add requests %v", reqs)
	}
	oa := srv.requests("object-add")
	if len(oa) != 1 || !strings.Contains(string(oa[0]["arguments"]), `"mem-path":"/tmp/d0.raw"`) ||
		!strings.Contains(string(oa[0]["arguments"]), `"share":true`) {
		t.Fatalf("unexpected object-add %v", oa)
	}
	if ok, err := q.DeviceExists(ctx, d.ID); err != nil || !ok {
		t.Fatalf("device should exist: %v %v", ok, err)
	}
	if err := q.DeviceDel(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := q.WaitDeviceDeleted(wctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("DEVICE_DELETED event was not used (took %v)", time.Since(start))
	}
	if err := q.DeviceDel(ctx, d.ID); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	if n := srv.conns.Load(); n != 1 {
		t.Fatalf("%d connections, expected one", n)
	}
	q.Close()
}

func TestQMPWaitTimeout(t *testing.T) {
	srv := newFakeQMPServer(t)
	srv.holdDel = true
	q := NewQMP(srv.path, t.Logf)
	q.PollInterval = 20 * time.Millisecond
	ctx := context.Background()
	d := CXLType3{ID: "x.hp1", Bus: "b", VolatileMemdev: "o", Serial: 1}
	if err := q.DeviceAdd(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := q.DeviceDel(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := q.WaitDeviceDeleted(wctx, d.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout, got %v", err)
	}
	// a Close ends a wait at once
	go func() {
		time.Sleep(50 * time.Millisecond)
		q.Close()
	}()
	wctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := q.WaitDeviceDeleted(wctx, d.ID); !errors.Is(err, ErrClosed) || wctx.Err() != nil {
		t.Fatalf("wait during a Close: %v (context %v)", err, wctx.Err())
	}
}

func TestDialLongPath(t *testing.T) {
	base := t.TempDir()
	short := filepath.Join(base, "s.sock")
	l, err := net.Listen("unix", short)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	long := filepath.Join(base, strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	longPath := filepath.Join(long, "qmp.sock")
	if err := os.Rename(short, longPath); err != nil {
		t.Fatal(err)
	}
	if len(longPath) <= maxSunPath {
		t.Fatalf("path not long enough: %d", len(longPath))
	}
	go func() {
		c, err := l.Accept()
		if err == nil {
			_, _ = c.Write([]byte("hi"))
			_ = c.Close()
		}
	}()
	c, err := dialUnix(context.Background(), longPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "hi" {
		t.Fatalf("read %q %v", b, err)
	}
}

func TestBackendMappedIn(t *testing.T) {
	mtree := "    0000000290000000-000000029fffffff (prio 0, ram): alias cxl-direct-mapping-alias-0 @shared2 0000000000000000-000000000fffffff\r\n" +
		"    00000002a0000000-00000002afffffff (prio 0, ram): alias cxl-direct-mapping-alias-1 @shared20 0000000000000000-000000000fffffff\n"
	if !BackendMappedIn(mtree, "shared2") || !BackendMappedIn(mtree, "shared20") || BackendMappedIn(mtree, "shared") || BackendMappedIn(mtree, "fcp_x.hp1") {
		t.Fatal("wrong mapping detection")
	}
}

func TestQMPTreeFromQOM(t *testing.T) {
	srv := newFakeQMPServer(t)
	srv.qom = true
	q := NewQMP(srv.path, t.Logf)
	defer q.Close()
	ctx := context.Background()
	if err := q.DeviceAdd(ctx, CXLType3{ID: "fcp_d0.hp1", Bus: "ds1", VolatileMemdev: "fcp_d0.hp1", Serial: 0xc1f00001}); err != nil {
		t.Fatal(err)
	}
	tree, err := q.QueryTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.HostBridges) != 1 || tree.HostBridges[0].ID != "cxlhb0" || tree.HostBridges[0].NUMANode != 0 {
		t.Fatalf("unexpected host bridges %+v", tree.HostBridges)
	}
	if len(tree.Ports) != 4 {
		t.Fatalf("unexpected ports %+v", tree.Ports)
	}
	if p := tree.Port("cxlrp0hb0"); p == nil || p.IsSlot() || p.Children[0].Driver != "cxl-upstream" {
		t.Fatalf("root port with switch: %+v", p)
	}
	if p := tree.Port("cxlrp1hb0"); p == nil || !p.IsSlot() || p.Occupant() != "" || p.HostBridge != "cxlhb0" {
		t.Fatalf("free root port: %+v", p)
	}
	if p := tree.Port("ds1"); p == nil || p.HostBridge != "cxlhb0" || p.Occupant() != "fcp_d0.hp1" {
		t.Fatalf("occupied downstream port: %+v", p)
	}
	d := treeDevice(tree, "fcp_d0.hp1")
	if d == nil || d.Bus != "ds1" || d.Serial != 0xc1f00001 || d.VolatileMemdev != "fcp_d0.hp1" {
		t.Fatalf("unexpected device %+v", d)
	}
	ok, err := HotRemoveCapable(ctx, q, tree)
	if err != nil || ok {
		// the fake has no power_controller_present: like stock qemu
		t.Fatalf("hot remove capable: %v %v", ok, err)
	}
	// a persistent device: its two backends are deleted after the device
	for _, b := range []MemoryBackend{pmemData, pmemLSA} {
		if err := q.ObjectAdd(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.DeviceAdd(ctx, pmemDev); err != nil {
		t.Fatal(err)
	}
	if tree, err = q.QueryTree(ctx); err != nil {
		t.Fatal(err)
	}
	if d := treeDevice(tree, "fcp_llm0.hp1"); d == nil || d.PersistentMemdev != "fcp_llm0.hp1" || d.LSA != "fcp_llm0.hp1-lsa" || d.VolatileMemdev != "" {
		t.Fatalf("unexpected persistent device %+v", d)
	}
	if err := q.DeviceDel(ctx, pmemDev.ID); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := q.WaitDeviceDeleted(wctx, pmemDev.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pmemData.ID, pmemLSA.ID} {
		if err := q.ObjectDel(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(srv.requests("object-del")); n != 2 {
		t.Fatalf("%d object-del requests", n)
	}
}

// Close is final: a goroutine that still holds a closed monitor does not
// redial and take the socket from the replacement monitor.
func TestQMPCloseIsFinal(t *testing.T) {
	srv := newFakeQMPServer(t)
	q := NewQMP(srv.path, t.Logf)
	if _, err := q.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
	q.Close()
	if _, err := q.Version(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed QMP still works: %v (conns=%d)", err, srv.conns.Load())
	}
	wctx, wcancel := context.WithTimeout(context.Background(), time.Second)
	defer wcancel()
	if err := q.WaitDeviceDeleted(wctx, "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitDeviceDeleted on a closed QMP: %v", err)
	}
	q2 := NewQMP(srv.path, t.Logf)
	defer q2.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := q2.Version(ctx); err != nil {
		t.Fatalf("replacement monitor blocked: %v", err)
	}
	h := NewHMP(filepath.Join(t.TempDir(), "none.sock"), nil)
	h.Close()
	if _, err := h.Version(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed HMP: %v", err)
	}
}

var (
	pmemData = MemoryBackend{QomType: MemoryBackendFile, ID: "fcp_llm0.hp1", Size: 512 << 20, MemPath: "/pool/llm0.raw", Share: true}
	pmemLSA  = MemoryBackend{QomType: MemoryBackendFile, ID: "fcp_llm0.hp1-lsa", Size: 256 << 10, MemPath: "/pool/llm0.lsa", Share: true}
	pmemDev  = CXLType3{ID: "fcp_llm0.hp1", Bus: "ds0", PersistentMemdev: "fcp_llm0.hp1", LSA: "fcp_llm0.hp1-lsa", Serial: 0xc1ae0001}
)

// The HMP command lines and the QMP arguments of object_add and
// device_add.
func TestMonitorCommands(t *testing.T) {
	if LSAObjectID("fcp_llm0.hp1") != "fcp_llm0.hp1-lsa" {
		t.Fatalf("LSA id %q", LSAObjectID("fcp_llm0.hp1"))
	}
	aligned := pmemData
	aligned.Align = HugePageAlign
	for _, tc := range []struct {
		b   MemoryBackend
		hmp string
		qmp map[string]any
	}{
		{aligned, "object_add memory-backend-file,id=fcp_llm0.hp1,size=536870912,share=on,mem-path=/pool/llm0.raw,align=2097152",
			map[string]any{"qom-type": MemoryBackendFile, "id": "fcp_llm0.hp1", "size": uint64(512 << 20), "share": true,
				"mem-path": "/pool/llm0.raw", "align": uint64(2 << 20)}},
		{pmemLSA, "object_add memory-backend-file,id=fcp_llm0.hp1-lsa,size=262144,share=on,mem-path=/pool/llm0.lsa",
			map[string]any{"qom-type": MemoryBackendFile, "id": "fcp_llm0.hp1-lsa", "size": uint64(256 << 10), "share": true,
				"mem-path": "/pool/llm0.lsa"}},
		// a RAM backend has no file mapping to align
		{MemoryBackend{QomType: MemoryBackendRAM, ID: "m", Size: 2 << 20, Share: true, Align: HugePageAlign},
			"object_add memory-backend-ram,id=m,size=2097152,share=on",
			map[string]any{"qom-type": MemoryBackendRAM, "id": "m", "size": uint64(2 << 20), "share": true}},
	} {
		if got := HMPObjectAdd(tc.b); got != tc.hmp {
			t.Errorf("HMP object_add of %s: %s", tc.b.ID, got)
		}
		if got := QMPObjectAddArgs(tc.b); !reflect.DeepEqual(got, tc.qmp) {
			t.Errorf("QMP object-add of %s: %v", tc.b.ID, got)
		}
	}
	vol := CXLType3{ID: "fcp_d0.hp1", Bus: "ds0", VolatileMemdev: "fcp_d0.hp1", Serial: 1}
	for _, tc := range []struct {
		d   CXLType3
		hmp string
		qmp map[string]any
	}{
		{pmemDev, "device_add cxl-type3,bus=ds0,persistent-memdev=fcp_llm0.hp1,lsa=fcp_llm0.hp1-lsa,id=fcp_llm0.hp1,sn=0xc1ae0001",
			map[string]any{"driver": "cxl-type3", "bus": "ds0", "persistent-memdev": "fcp_llm0.hp1", "lsa": "fcp_llm0.hp1-lsa",
				"id": "fcp_llm0.hp1", "sn": uint64(0xc1ae0001)}},
		{vol, "device_add cxl-type3,bus=ds0,volatile-memdev=fcp_d0.hp1,id=fcp_d0.hp1,sn=0x1",
			map[string]any{"driver": "cxl-type3", "bus": "ds0", "volatile-memdev": "fcp_d0.hp1", "id": "fcp_d0.hp1", "sn": uint64(1)}},
	} {
		if got := HMPDeviceAdd(tc.d); got != tc.hmp {
			t.Errorf("HMP device_add of %s: %s", tc.d.ID, got)
		}
		if got := QMPDeviceAddArgs(tc.d); !reflect.DeepEqual(got, tc.qmp) {
			t.Errorf("QMP device_add of %s: %v", tc.d.ID, got)
		}
	}
	if pmemDev.Memdev() != "fcp_llm0.hp1" || vol.Memdev() != "fcp_d0.hp1" {
		t.Fatal("wrong Memdev")
	}
}
