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
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/client"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
)

const llm0Config = `  - name: llm0
    size: 512Mi
    shared: true
    persistent: true
    serial: 0xc1ae0001
`

// rawDevice returns the JSON object of a device as the server sends it.
func (e *testEnv) rawDevice(name string) map[string]any {
	e.t.Helper()
	resp, err := http.Get(e.hs.URL + "/api/v1/devices/" + name)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		e.t.Fatalf("device %s: %v: %s", name, err, b)
	}
	return m
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestPersistentConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte("dir: /p\ndevices:\n" + llm0Config +
		"  - name: vol0\n    size: 256Mi\n    serial: 0xc1ee0001\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Devices[0].Persistent || cfg.Devices[1].Persistent {
		t.Fatalf("unexpected persistent flags %+v", cfg.Devices)
	}
	bad := "devices: [{name: a, size: 256Mi, persistent: yes-please, serial: 1}]"
	if _, err := ParseConfig([]byte(bad)); err == nil || !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("%s: expected an invalid config, got %v", bad, err)
	}
}

func TestPersistentAttachDetach(t *testing.T) {
	e := newTestEnv(t, llm0Config)
	data := filepath.Join(e.dir, "pool", "llm0.raw")
	lsa := filepath.Join(e.dir, "pool", "llm0.lsa")
	if fileSize(t, data) != 512<<20 || fileSize(t, lsa) != 262144 {
		t.Fatalf("backing files: data %d lsa %d", fileSize(t, data), fileSize(t, lsa))
	}
	d, err := e.c.Device(ctx, "llm0")
	if err != nil || !d.Persistent || d.LSAPath != lsa || d.Path != data {
		t.Fatalf("unexpected device %+v %v", d, err)
	}
	raw := e.rawDevice("llm0")
	if raw["persistent"] != true || raw["lsaPath"] != lsa {
		t.Fatalf("unexpected JSON %v", raw)
	}
	raw = e.rawDevice("pooled0")
	if v, ok := raw["persistent"]; !ok || v != false {
		t.Fatalf("volatile device JSON without persistent false: %v", raw)
	}
	if _, ok := raw["lsaPath"]; ok {
		t.Fatalf("volatile device JSON with lsaPath: %v", raw)
	}
	for _, h := range []string{"vm1", "vm2"} {
		a, _, err := e.c.Attach(ctx, "llm0", api.AttachRequest{Host: h})
		if err != nil || a.QemuDeviceID != "fcp_llm0.hp1" || a.QemuObjectID != "fcp_llm0.hp1" ||
			a.QemuLSAObjectID != "fcp_llm0.hp1-lsa" || a.Serial != "0xc1ae0001" {
			t.Fatalf("attach to %s: %+v %v", h, a, err)
		}
		// every qemu maps the same two files
		want := []string{
			"object_add memory-backend-file,id=fcp_llm0.hp1,size=536870912,share=on,mem-path=" + data + ",align=2097152",
			"object_add memory-backend-file,id=fcp_llm0.hp1-lsa,size=262144,share=on,mem-path=" + lsa,
			"device_add cxl-type3,bus=" + a.Slot.Bus + ",persistent-memdev=fcp_llm0.hp1,lsa=fcp_llm0.hp1-lsa,id=fcp_llm0.hp1,sn=0xc1ae0001",
		}
		if got := e.fake(h).Commands(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: qemu commands:\n%s\nwant:\n%s", h, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	if _, _, err := e.c.Detach(ctx, "llm0", "vm1", client.DetachOptions{}); err != nil {
		t.Fatal(err)
	}
	f1 := e.fake("vm1")
	if got := f1.Commands()[3:]; !reflect.DeepEqual(got, []string{"device_del fcp_llm0.hp1", "object_del fcp_llm0.hp1", "object_del fcp_llm0.hp1-lsa"}) {
		t.Fatalf("detach commands %v", got)
	}
	if f1.HasDevice("fcp_llm0.hp1") || f1.HasObject("fcp_llm0.hp1") || f1.HasObject("fcp_llm0.hp1-lsa") {
		t.Fatalf("left in qemu: %s", f1)
	}
	// a static device keeps its files
	if fileSize(t, data) != 512<<20 || fileSize(t, lsa) != 262144 {
		t.Fatal("backing files changed")
	}
	// a re-attach uses new ids for both backends
	a, _, err := e.c.Attach(ctx, "llm0", api.AttachRequest{Host: "vm1"})
	if err != nil || a.QemuDeviceID != "fcp_llm0.hp2" || !f1.HasObject("fcp_llm0.hp2-lsa") {
		t.Fatalf("re-attach: %+v %v", a, err)
	}
}

func TestPersistentCreateDelete(t *testing.T) {
	e := newTestEnv(t, "")
	poolDir := filepath.Join(e.dir, "pool")
	d, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "p0", Size: 256 << 20, Shared: true, Persistent: true})
	if err != nil || !d.Persistent || d.LSAPath != filepath.Join(poolDir, "p0.lsa") || d.Path != filepath.Join(poolDir, "p0.raw") {
		t.Fatalf("create: %+v %v", d, err)
	}
	if fileSize(t, d.Path) != 256<<20 || fileSize(t, d.LSAPath) != 262144 {
		t.Fatal("backing files not created")
	}
	v, err := e.c.CreateDevice(ctx, api.DeviceCreate{Name: "v0", Size: 256 << 20})
	if err != nil || v.Persistent || v.LSAPath != "" {
		t.Fatalf("volatile create: %+v %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(poolDir, "v0.lsa")); !os.IsNotExist(err) {
		t.Fatalf("volatile device has an LSA file: %v", err)
	}
	// the server restarts: the device stays persistent, a missing LSA file
	// is created again
	e.stop()
	if err := os.Remove(d.LSAPath); err != nil {
		t.Fatal(err)
	}
	e.start()
	r, err := e.c.Device(ctx, "p0")
	if err != nil || !r.Persistent || r.LSAPath != d.LSAPath || fileSize(t, d.LSAPath) != 262144 {
		t.Fatalf("after restart: %+v %v", r, err)
	}
	if err := e.c.DeleteDevice(ctx, "p0", false); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{d.Path, d.LSAPath} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s not removed: %v", f, err)
		}
	}
}

func TestPersistentAdopt(t *testing.T) {
	e := newTestEnv(t, llm0Config)
	f1 := e.fake("vm1")
	data := filepath.Join(e.dir, "pool", "llm0.raw")
	lsa := filepath.Join(e.dir, "pool", "llm0.lsa")
	// a previous server run (state lost) attached llm0, or a hand hotplug
	// whose LSA backend does not follow the -lsa naming
	f1.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: "fcp_llm0.hp7", Size: 512 << 20, MemPath: data, Share: true})
	f1.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: "fcp_llm0.labels", Size: lsaSize, MemPath: lsa, Share: true})
	if err := f1.PlugDevice(qemu.CXLType3{ID: "fcp_llm0.hp7", Bus: "ds0_hb0", PersistentMemdev: "fcp_llm0.hp7", LSA: "fcp_llm0.labels", Serial: 0xc1ae0001}); err != nil {
		t.Fatal(err)
	}
	e.srv.Refresh(ctx)
	a, err := e.attachment("llm0", "vm1")
	if err != nil || !a.Adopted || a.QemuDeviceID != "fcp_llm0.hp7" || a.QemuObjectID != "fcp_llm0.hp7" || a.Serial != "0xc1ae0001" {
		t.Fatalf("not adopted: %+v %v", a, err)
	}
	if a.QemuLSAObjectID != "fcp_llm0.labels" {
		t.Fatalf("LSA backend not recorded: %+v", a)
	}
	if _, _, err := e.c.Detach(ctx, "llm0", "vm1", client.DetachOptions{}); err != nil {
		t.Fatal(err)
	}
	cmds := f1.Commands()
	if got := cmds[len(cmds)-3:]; !reflect.DeepEqual(got, []string{"device_del fcp_llm0.hp7", "object_del fcp_llm0.hp7", "object_del fcp_llm0.labels"}) {
		t.Fatalf("detach commands %v", got)
	}
	if f1.HasObject("fcp_llm0.labels") {
		t.Fatalf("LSA backend left in qemu: %s", f1)
	}
	// the counter is above the adopted ids
	a, _, err = e.c.Attach(ctx, "llm0", api.AttachRequest{Host: "vm1"})
	if err != nil || a.QemuDeviceID != "fcp_llm0.hp8" {
		t.Fatalf("attach: %+v %v", a, err)
	}
}
