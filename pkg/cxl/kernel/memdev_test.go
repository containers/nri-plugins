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

package kernel_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// A memdev is found by its serial, waited for, and its host bridge and root
// decoders found.
func TestFindAndWaitMemdev(t *testing.T) {
	f := fake.New(t)
	k := f.Kernel()
	if _, err := k.FindMemdev(serial0); !errors.Is(err, kernel.ErrNotFound) || !strings.Contains(err.Error(), "no memdev with the serial 0xc1f00001") {
		t.Fatalf("expected not found, got %v", err)
	}
	if m := k.Memdev(serial0); m.State != kernel.MemdevAbsent || m.Name != "" {
		t.Fatalf("absent memdev %+v", m)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.Plug(fake.MemdevSpec{Serial: serial0})
	}()
	name, err := k.WaitMemdev(ctxTimeout(t, 5*time.Second), serial0, kernel.MemdevReady)
	if err != nil || name != "mem0" {
		t.Fatalf("wait: %q %v", name, err)
	}
	if _, err := k.WaitMemdev(ctxTimeout(t, 20*time.Millisecond), 0x1234, kernel.MemdevBound); !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), "timeout waiting for the memdev with serial 0x1234") {
		t.Fatalf("expected timeout, got %v", err)
	}
	m := k.Memdev(serial0)
	if m.State != kernel.MemdevReady || m.Endpoint != "endpoint4" || m.RamSize != 256*mib || m.NUMAAffinity != 0 ||
		strings.Join(m.Ports, ",") != "port1,endpoint4" {
		t.Fatalf("memdev %+v", m)
	}
	if uid, err := k.HostBridgeUID("mem0"); err != nil || uid != 12 {
		t.Fatalf("uid %d %v", uid, err)
	}
	if d, err := k.RootDecoder("mem0", kernel.RegionRAM); err != nil || d != "decoder0.0" {
		t.Fatalf("root decoder %q %v", d, err)
	}
	if d, err := k.RootDecoder("mem0", kernel.RegionPmem); err != nil || d != "decoder0.2" {
		t.Fatalf("root decoder %q %v", d, err)
	}
	if _, err := k.RootDecoder("mem0", "volatile"); err == nil {
		t.Fatal("invalid region type accepted")
	}
}

// The uid of a host bridge in the target_list of root decoders is its ACPI
// _UID, which differs from the bus number of its PCI root on real hardware:
// the dport of the CXL root that links to the PCI root tells it, and
// without such a dport there is none.
func TestHostBridgeUIDNotBusNumber(t *testing.T) {
	f, k := newNode(t)
	if uid, err := k.HostBridgeUID("mem0"); err != nil || uid != 12 {
		t.Fatalf("uid %d %v, expected 12", uid, err)
	}
	f.Remove("devices/platform/ACPI0017:00/root0/dport12")
	_, err := k.HostBridgeUID("mem0")
	wantErr(t, err, kernel.ErrNotFound, "no dport of the CXL root links to")
	// a memdev under host bridge 24 gets its decoder
	f = fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: serial0, HostBridge: 24})
	if d, err := f.Kernel().RootDecoder("mem0", kernel.RegionRAM); err != nil || d != "decoder0.1" {
		t.Fatalf("root decoder %q %v", d, err)
	}
	f.Write("devices/platform/ACPI0017:00/root0/decoder0.1/cap_ram", "0")
	if _, err := f.Kernel().RootDecoder("mem0", kernel.RegionRAM); !errors.Is(err, kernel.ErrNotFound) ||
		!strings.Contains(err.Error(), "no root decoder for ram regions targets host bridge 24 of mem0") {
		t.Fatalf("expected no decoder, got %v", err)
	}
}

// The states of a memdev: the endpoint port comes after the driver link
// (the add_dev race), cxl disable-memdev takes both, and a memdev with
// committed decoders is not disabled.
func TestMemdevStates(t *testing.T) {
	f := fake.New(t)
	f.EndpointDelay = 50 * time.Millisecond
	k := f.Kernel()
	f.Plug(fake.MemdevSpec{Serial: serial0, Disabled: true})
	if m := k.Memdev(serial0); m.State != kernel.MemdevUnbound {
		t.Fatalf("plugged without driver: %v", m.State)
	}
	ctx := context.Background()
	noErr(t, k.EnableMemdev(ctx, "mem0"))
	if m := k.Memdev(serial0); m.State != kernel.MemdevBound || m.Endpoint != "" {
		t.Fatalf("enabled before its endpoint: %+v", m)
	}
	// a region cannot be created yet: ErrEndpointNotReady
	_, err := k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: "mem0", Decoder: "decoder0.0", Type: kernel.RegionRAM})
	wantErr(t, err, kernel.ErrEndpointNotReady, "add_dev() failed")
	if _, err := k.WaitMemdev(ctxTimeout(t, 5*time.Second), serial0, kernel.MemdevReady); err != nil {
		t.Fatal(err)
	}
	r := createRegion(t, k, "mem0", kernel.RegionRAM, 0)
	before := f.Commands()
	noErr(t, k.EnableMemdev(ctx, "mem0"))
	if f.Commands() != before {
		t.Fatal("an enabled memdev was enabled again")
	}
	err = k.DisableMemdev(ctx, "mem0")
	wantErr(t, err, nil, "mem0 is part of an active region")
	noErr(t, k.DestroyRegion(ctx, r))
	noErr(t, k.DisableMemdev(ctx, "mem0"))
	if m := k.Memdev(serial0); m.State != kernel.MemdevUnbound || m.Endpoint != "" {
		t.Fatalf("disabled memdev %+v", m)
	}
	// disabling again, or a memdev that is gone, does nothing
	before = f.Commands()
	noErr(t, k.DisableMemdev(ctx, "mem0"))
	noErr(t, k.DisableMemdev(ctx, "mem7"))
	if f.Commands() != before {
		t.Fatalf("commands:\n%s", f.CommandsSince(before))
	}
	f.Unplug(serial0)
	if m := k.Memdev(serial0); m.State != kernel.MemdevAbsent {
		t.Fatalf("unplugged memdev %+v", m)
	}
}

// Persistent and RAM capacity of a memdev.
func TestMemdevCapacity(t *testing.T) {
	f := fake.New(t)
	k := f.Kernel()
	f.Plug(fake.MemdevSpec{Serial: serial0, Pmem: 512 * mib})
	if p, err := k.Persistent("mem0"); err != nil || !p {
		t.Fatalf("persistent memdev: %v %v", p, err)
	}
	if s, err := k.RamSize("mem0"); err != nil || s != 0 {
		t.Fatalf("ram size %d %v", s, err)
	}
	dir := strings.TrimPrefix(evalLink(t, f, "bus/cxl/devices/mem0"), f.SysRoot()+"/")
	f.Write(dir+"/pmem/size", "0x0")
	if p, err := k.Persistent("mem0"); err != nil || p {
		t.Fatalf("memdev without pmem: %v %v", p, err)
	}
	f.Write(dir+"/pmem/size", "bogus")
	if _, err := k.Persistent("mem0"); err == nil {
		t.Fatal("bogus pmem size accepted")
	}
}

// evalLink returns the path that a sysfs symlink of the fake resolves to.
func evalLink(t *testing.T, f *fake.Fake, rel string) string {
	t.Helper()
	p, err := os.Readlink(f.P(rel))
	noErr(t, err)
	return p
}
