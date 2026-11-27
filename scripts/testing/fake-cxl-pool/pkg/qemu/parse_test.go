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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// treeDevice returns the cxl-type3 device of the tree by id, nil if not
// found.
func treeDevice(t *Tree, id string) *Type3Device {
	for i := range t.Type3 {
		if t.Type3[i].ID == id {
			return &t.Type3[i]
		}
	}
	return nil
}

// rawSession returns the bytes that a client sees after sending the
// command line in a raw socat capture: everything after the first prompt.
func rawSession(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, hmpPrompt)
	if i < 0 {
		t.Fatalf("%s: no prompt", name)
	}
	return s[i+len(hmpPrompt):]
}

func TestParseQtreeFull(t *testing.T) {
	text := CleanHMPOutput(rawSession(t, "hmp-raw-info-qtree-full-crio.txt"))
	tree := ParseQtree(text)
	if hb := tree.HostBridge("cxlhb1"); hb == nil || hb.NUMANode != 1 {
		t.Fatalf("expected cxlhb1 on NUMA node 1: %+v", tree.HostBridges)
	}
	if hb := tree.HostBridge("cxlhb0"); hb == nil || hb.NUMANode != 0 {
		t.Fatalf("expected cxlhb0 on NUMA node 0: %+v", tree.HostBridges)
	}
	d := treeDevice(tree, "cxl_memdev2.hp3")
	if d == nil {
		t.Fatalf("cxl_memdev2.hp3 not found: %+v", tree.Type3)
	}
	if d.VolatileMemdev != "beram_cxl_memdev2__bus_cxlsw_ds0_usrp0hb1__sn_0xc100e2e2" ||
		!d.HasSerial || d.Serial != 0xc100e2e2 || d.Bus != "cxlsw_ds0_usrp0hb1" {
		t.Fatalf("unexpected device %+v", d)
	}
	slots := 0
	for _, p := range tree.Ports {
		if p.IsSlot() {
			slots++
		}
	}
	if slots != 4 {
		t.Fatalf("expected 4 slots in crio VM, got %d", slots)
	}
	// a persistent device in the same tree
	old := `                    memdev = ""
                    persistent-memdev = ""
                    volatile-memdev = "/objects/beram_cxl_memdev2__bus_cxlsw_ds0_usrp0hb1__sn_0xc100e2e2"
                    lsa = ""`
	if !strings.Contains(text, old) {
		t.Fatal("test data changed: cxl_memdev2.hp3 properties not found")
	}
	text = strings.Replace(text, old, `                    memdev = ""
                    persistent-memdev = "/objects/fcp_llm0.hp1"
                    volatile-memdev = ""
                    lsa = "/objects/fcp_llm0.hp1-lsa"`, 1)
	tree = ParseQtree(text)
	d = treeDevice(tree, "cxl_memdev2.hp3")
	if d == nil || d.PersistentMemdev != "fcp_llm0.hp1" || d.LSA != "fcp_llm0.hp1-lsa" || d.VolatileMemdev != "" ||
		d.Memdev() != "fcp_llm0.hp1" {
		t.Fatalf("unexpected persistent device %+v", d)
	}
	v := treeDevice(tree, "cxl_memdev1.hp2")
	if v == nil || v.PersistentMemdev != "" || v.LSA != "" || v.Memdev() != "beram_cxl_memdev1__bus_cxlsw_ds1_usrp0hb0__sn_0xc100e2e1" {
		t.Fatalf("unexpected volatile device %+v", v)
	}
}

// Root ports without a switch are slots. An unassigned NUMA node (qemu
// MAX_NODES = 128) is -1.
func TestParseQtreeRootPortSlot(t *testing.T) {
	text := `bus: main-system-bus
  type System
  dev: pxb-cxl-host, id ""
    bus: cxlhb0
      type pxb-cxl-bus
      dev: cxl-rp, id "cxlrp0hb0"
        bus: cxlrp0hb0
          type CXL
      dev: cxl-rp, id "cxlrp1hb0"
        bus: cxlrp1hb0
          type CXL
          dev: cxl-type3, id "x.hp1"
            volatile-memdev = "/objects/fcp_x.hp1"
            sn = 3253731329 (0xc1f00001)
  dev: pxb-cxl-host, id ""
    bus: cxlhb1
      type pxb-cxl-bus
  dev: q35-pcihost, id ""
    bus: pcie.0
      type PCIE
      dev: pxb-cxl, id "cxlhb0"
        numa_node = 0 (0x0)
      dev: pxb-cxl, id "cxlhb1"
        numa_node = 128 (0x80)
`
	tree := ParseQtree(text)
	if len(tree.Ports) != 2 || !tree.Ports[0].IsSlot() || !tree.Ports[1].IsSlot() {
		t.Fatalf("expected 2 root port slots: %+v", tree.Ports)
	}
	if tree.Port("cxlrp1hb0").Occupant() != "x.hp1" {
		t.Fatalf("unexpected occupant")
	}
	d := treeDevice(tree, "x.hp1")
	if d == nil || d.VolatileMemdev != "fcp_x.hp1" || d.Serial != 0xc1f00001 || d.Bus != "cxlrp1hb0" {
		t.Fatalf("unexpected device %+v", d)
	}
	if tree.HostBridge("cxlhb0").NUMANode != 0 || tree.HostBridge("cxlhb1").NUMANode != -1 {
		t.Fatalf("unexpected numa %+v", tree.HostBridges)
	}
}

func TestParseInfoMemdev(t *testing.T) {
	mds := ParseInfoMemdev(CleanHMPOutput(rawSession(t, "hmp-raw-info-memdev.txt")))
	if len(mds) != 12 {
		t.Fatalf("expected 12 memdevs, got %d", len(mds))
	}
	found := false
	for _, m := range mds {
		if m.ID == "beram_cxl_memdev6__bus_cxlsw_ds4_usrp0hb1__sn_0xc100e2e6" {
			found = true
			if m.Size != 256<<20 || !m.Share {
				t.Fatalf("unexpected memdev %+v", m)
			}
		}
		if m.ID == "membuiltin_0_node_0" && (m.Size != 4<<30 || m.Share) {
			t.Fatalf("unexpected memdev %+v", m)
		}
	}
	if !found {
		t.Fatalf("memdev6 not found")
	}
}

func readCmdline(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestParseCmdline(t *testing.T) {
	p := ParseCmdline(readCmdline(t, "cmdline-n4-cxl-containerd.txt"))
	if p.Name != "n4-cxl-fedora-43-containerd" {
		t.Fatalf("unexpected name %q", p.Name)
	}
	if p.ProjectDir != "/home/user/github.com/containers/nri-plugins/test/e2e/n4-cxl-fedora-43-containerd" {
		t.Fatalf("unexpected project dir %q", p.ProjectDir)
	}
	if len(p.QMPSockets) != 0 {
		t.Fatalf("unexpected qmp sockets %v", p.QMPSockets)
	}
	if len(p.HMPSockets) != 2 || p.HMPSockets[0] != "monitor.sock" ||
		!strings.HasSuffix(p.HMPSockets[1], "/qemu_socket") {
		t.Fatalf("unexpected hmp sockets %v", p.HMPSockets)
	}
	lbs := p.LocalBackends()
	if len(lbs) != 10 {
		t.Fatalf("expected 10 local backends, got %d", len(lbs))
	}
	lb := lbs[3]
	if lb.Memdev != "cxl_memdev3" || lb.Bus != "cxlsw_ds1_usrp0hb1" || lb.Serial != 0xc100e2e3 ||
		lb.Size != 256<<20 || lb.ObjectID != "beram_cxl_memdev3__bus_cxlsw_ds1_usrp0hb1__sn_0xc100e2e3" {
		t.Fatalf("unexpected local backend %+v", lb)
	}
	// daemonized qemu: cwd is "/", sockets are in the project dir
	p.Cwd = "/"
	exists := func(path string) bool { return path == p.ProjectDir+"/monitor.sock" }
	if got := p.ResolvePath("monitor.sock", exists); got != p.ProjectDir+"/monitor.sock" {
		t.Fatalf("unexpected resolved path %q", got)
	}
	p.Cwd = "/some/dir"
	if got := p.ResolvePath("monitor.sock", func(string) bool { return false }); got != "/some/dir/monitor.sock" {
		t.Fatalf("unexpected resolved path %q", got)
	}
}

func TestParseCmdlineQMPAndUUID(t *testing.T) {
	args := []string{"/opt/qemu/bin/qemu-system-x86_64",
		"-name", "guest=vm1,debug-threads=on",
		"-uuid", "6BA7B810-9DAD-11D1-80B4-00C04FD430C8",
		"-qmp", "unix:qmp.sock,server=on,wait=off",
		"-chardev", "socket,id=qmp1,path=/run/vm1.qmp,server=on,wait=off",
		"-mon", "chardev=qmp1,mode=control",
		"-chardev", "socket,id=c2,path=/run/client.sock",
		"-mon", "chardev=c2",
		"-object", `{"qom-type":"memory-backend-ram","id":"beram_cxl_memdev0__bus_ds0__sn_0xc1f0ee00","size":268435456,"share":true}`,
		"-object", "memory-backend-file,id=befile_cxl_memdev1__bus_ds1__sn_0xc1f0ee01,mem-path=/tmp/f.raw,size=256M",
		"-device", "cxl-type3,bus=ds0,volatile-memdev=beram_cxl_memdev0__bus_ds0__sn_0xc1f0ee00,id=cxl_memdev0,sn=0xc1f0ee00",
		"-daemonize",
	}
	p := ParseCmdline(args)
	if p.Name != "vm1" || p.UUID != "6ba7b810-9dad-11d1-80b4-00c04fd430c8" {
		t.Fatalf("unexpected name/uuid %q %q", p.Name, p.UUID)
	}
	if len(p.QMPSockets) != 2 || p.QMPSockets[0] != "qmp.sock" || p.QMPSockets[1] != "/run/vm1.qmp" {
		t.Fatalf("unexpected qmp sockets %v", p.QMPSockets)
	}
	if len(p.HMPSockets) != 0 {
		t.Fatalf("client chardev must not be a monitor: %v", p.HMPSockets)
	}
	// a memory-backend-file is no local backend
	lbs := p.LocalBackends()
	if len(lbs) != 1 || lbs[0].Size != 256<<20 || lbs[0].Serial != 0xc1f0ee00 || lbs[0].Bus != "ds0" {
		t.Fatalf("unexpected local backends %+v", lbs)
	}
}

func TestParseOpts(t *testing.T) {
	o := ParseOpts("memory-backend-file,id=a,mem-path=/tmp/a,,b.raw,share=on,size=1G", "qom-type")
	if o.First != "memory-backend-file" || o.Get("mem-path") != "/tmp/a,b.raw" || o.Get("size") != "1G" {
		t.Fatalf("unexpected opts %+v", o)
	}
	if EscapeOptValue("a,b") != "a,,b" {
		t.Fatalf("bad escape")
	}
	o = ParseOpts("driver=cxl-type3,id=x", "driver")
	if o.First != "cxl-type3" || o.Get("id") != "x" {
		t.Fatalf("unexpected opts %+v", o)
	}
}

func TestDiscoverSelf(t *testing.T) {
	// Discover must not fail on a real /proc, whether or not qemus run.
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc")
	}
	if _, err := Discover("/proc"); err != nil {
		t.Fatal(err)
	}
	if !ProcessAlive("/proc", os.Getpid(), 0) {
		t.Fatal("own process not alive")
	}
}

// An exited qemu that is not reaped yet (a zombie) is not alive: its
// monitor sockets are closed, another qemu may serve their paths.
func TestProcessAliveZombie(t *testing.T) {
	root := t.TempDir()
	for pid, state := range map[string]string{"10": "S", "11": "Z", "12": "X"} {
		// starttime is the 22nd field, the 20th after the comm
		stat := pid + " (qemu-system-x86) " + state + strings.Repeat(" 0", 18) + " 500 0 0\n"
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pid, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		pid   int
		start uint64
		alive bool
	}{
		{10, 500, true}, {10, 0, true}, {10, 501, false},
		{11, 500, false}, {12, 0, false}, {13, 0, false},
	} {
		if got := ProcessAlive(root, tc.pid, tc.start); got != tc.alive {
			t.Errorf("ProcessAlive(%d, %d) = %v, want %v", tc.pid, tc.start, got, tc.alive)
		}
	}
}
