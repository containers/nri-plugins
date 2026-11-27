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

// Tests of reconcile with device trees that may be stale, and of adoption.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu/fake"
)

// gatedMon blocks the first QueryTree after arm() until release(), after it
// has taken its snapshot: a refresh with a stale tree.
type gatedMon struct {
	*fake.Qemu
	mu    sync.Mutex
	armed bool
	snap  chan struct{}
	gate  chan struct{}
}

func newGatedMon(f *fake.Qemu) *gatedMon {
	return &gatedMon{Qemu: f, snap: make(chan struct{}), gate: make(chan struct{})}
}

func (g *gatedMon) arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

func (g *gatedMon) QueryTree(ctx context.Context) (*qemu.Tree, error) {
	t, err := g.Qemu.QueryTree(ctx)
	g.mu.Lock()
	armed := g.armed
	g.armed = false
	g.mu.Unlock()
	if armed {
		close(g.snap)
		<-g.gate
	}
	return t, err
}

// A refresh whose tree was read before an attach completed does not drop
// the new attachment, and opens no window for attaching the exclusive
// device to a second host.
func TestStaleTreeKeepsNewAttachment(t *testing.T) {
	f := newVMFake()
	g := newGatedMon(f)
	srv := newServerWith(t, poolConfig(t.TempDir(), pooled0), map[string]qemu.Monitor{"vm1": g, "vm2": newVMFake()})
	g.arm()
	done := make(chan struct{})
	go func() { srv.Hosts(ctx); close(done) }() // GET /hosts
	<-g.snap
	a, _, err := srv.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm1"})
	if err != nil {
		t.Fatal(err)
	}
	close(g.gate)
	<-done
	cur, err := attachmentOf(srv, "pooled0", "vm1")
	if err != nil || cur.State != api.AttachmentAttached {
		t.Fatalf("attachment dropped by a reconcile with a stale tree: %+v %v (qemu has %s: %v)",
			cur, err, a.QemuDeviceID, f.HasDevice(a.QemuDeviceID))
	}
	if d, _ := srv.Device("pooled0"); d.State != api.DeviceAttached {
		t.Fatalf("device state %s", d.State)
	}
	_, _, err = srv.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm2"})
	mustCode(t, err, api.CodeConflict)
	srv.refreshTrees(ctx)
	as, _ := srv.Attachments(AttachmentFilter{Device: "pooled0"})
	if len(as) != 1 || as[0].Host != "vm1" || as[0].Adopted {
		t.Fatalf("unshared device pooled0 has attachments %+v", as)
	}
}

// Adoption respects the exclusive rule.
func TestAdoptionRespectsUnshared(t *testing.T) {
	dir := t.TempDir()
	f1, f2 := newVMFake(), newVMFake()
	srv := newServerWith(t, poolConfig(dir, pooled0), map[string]qemu.Monitor{"vm1": f1, "vm2": f2})
	if _, _, err := srv.Attach(ctx, "pooled0", api.AttachRequest{Host: "vm2"}); err != nil {
		t.Fatal(err)
	}
	// someone plugs the same unshared device into vm1 behind the server
	obj := "fcp_pooled0.hp7"
	f1.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: obj, Size: 256 << 20, MemPath: filepath.Join(dir, "pool", "pooled0.raw"), Share: true})
	if err := f1.PlugDevice(qemu.CXLType3{ID: obj, Bus: "ds0_hb0", VolatileMemdev: obj, Serial: 0xc1ee0001}); err != nil {
		t.Fatal(err)
	}
	srv.refreshTrees(ctx)
	as, _ := srv.Attachments(AttachmentFilter{Device: "pooled0"})
	if len(as) != 1 || as[0].Host != "vm2" {
		t.Fatalf("unshared device adopted twice: %+v", as)
	}
	// a shared device may be adopted on another host
	sh := "devices:\n  - name: s0\n    size: 256M\n    shared: true\n    serial: 0xc1ae0001\n"
	f3, f4 := newVMFake(), newVMFake()
	dir2 := t.TempDir()
	srv2 := newServerWith(t, poolConfig(dir2, sh), map[string]qemu.Monitor{"vm1": f3, "vm2": f4})
	if _, _, err := srv2.Attach(ctx, "s0", api.AttachRequest{Host: "vm2"}); err != nil {
		t.Fatal(err)
	}
	obj = "fcp_s0.hp9"
	f3.AddObject(qemu.MemoryBackend{QomType: qemu.MemoryBackendFile, ID: obj, Size: 256 << 20, MemPath: filepath.Join(dir2, "pool", "s0.raw"), Share: true})
	if err := f3.PlugDevice(qemu.CXLType3{ID: obj, Bus: "ds0_hb0", VolatileMemdev: obj, Serial: 0xc1ae0001}); err != nil {
		t.Fatal(err)
	}
	srv2.refreshTrees(ctx)
	if as, _ := srv2.Attachments(AttachmentFilter{Device: "s0"}); len(as) != 2 {
		t.Fatalf("shared device not adopted: %+v", as)
	}
}
