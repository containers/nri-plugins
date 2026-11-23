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
	"reflect"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// stackNode is a VM of the e2e tests: mem0 (1 GiB) and mem1 under the
// switch port3 of host bridge port1, with region0 (mem0), region1 (mem1)
// and region2 (mem0) on decoders 0-2 of both ports.
func stackNode(t *testing.T) (*fake.Fake, *kernel.Kernel) {
	t.Helper()
	f := fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: serial0, Ram: gib, Switch: true})
	f.Plug(fake.MemdevSpec{Serial: serial1, Switch: true})
	k := f.Kernel()
	for _, memdev := range []string{"mem0", "mem1", "mem0"} {
		createRegion(t, k, memdev, kernel.RegionRAM, 256*mib)
	}
	for _, port := range []string{"port1", "port3"} {
		if got := f.Stack(port); !reflect.DeepEqual(got, []string{"region0", "region1", "region2"}) {
			t.Fatalf("%s: stack %v", port, got)
		}
	}
	return f, k
}

// holes returns the decoder holes of a memdev.
func holes(t *testing.T, k *kernel.Kernel, memdev string) []kernel.DecoderHole {
	t.Helper()
	h, err := k.DecoderHoles(memdev)
	noErr(t, err)
	return h
}

// above returns the regions above those of a memdev.
func above(t *testing.T, k *kernel.Kernel, memdev string) []string {
	t.Helper()
	a, err := k.RegionsAbove(memdev)
	noErr(t, err)
	return a
}

// TestDecoderHoles: a region destroyed below newer regions leaves a hole in
// every port that they share; the ports refuse new regions until the
// regions above go, and then the kernel reaps the hole. The endpoint port
// of a memdev with two regions names a hole too.
func TestDecoderHoles(t *testing.T) {
	f, k := stackNode(t)
	ctx := context.Background()
	if h := holes(t, k, "mem1"); h != nil {
		t.Fatalf("in-order decoders: holes %+v", h)
	}
	noErr(t, k.DestroyRegion(ctx, "region1"))
	want := []kernel.DecoderHole{
		{Port: "port1", Committed: 3, Free: []string{"decoder1.1"}, Blocking: []string{"region2"}},
		{Port: "port3", Committed: 3, Free: []string{"decoder3.1"}, Blocking: []string{"region2"}},
	}
	if got := holes(t, k, "mem1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("holes of mem1 %+v, want %+v", got, want)
	}
	if got := holes(t, k, "mem0"); !reflect.DeepEqual(got, want) {
		t.Fatalf("holes of mem0 %+v, want %+v", got, want)
	}
	if got := want[1].String(); got != "port3 has 3 committed decoders, decoder3.1 without a region below region2" {
		t.Errorf("String() = %q", got)
	}
	// a new region of either memdev is refused with the hole explained
	_, err := k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: "mem1", Decoder: "decoder0.0", Type: kernel.RegionRAM})
	var hole *kernel.ErrDecoderHole
	if !errors.As(err, &hole) || hole.Port != "port1" || hole.Decoder != "decoder1.1" || !reflect.DeepEqual(hole.Blocking, []string{"region2"}) {
		t.Fatalf("expected a decoder hole, got %v", err)
	}
	if !strings.Contains(err.Error(), "failed to set target0 to mem1 (CXL decoders released out of order: port1 has 3 committed decoders, decoder1.1 without a region below region2; port3 has") ||
		!strings.HasSuffix(err.Error(), "until region2 is destroyed)") {
		t.Fatalf("error %q", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatal("the error of cxl create-region is not wrapped")
	}
	// the top region goes: the hole is reaped
	noErr(t, k.DestroyRegion(ctx, "region2"))
	if h := holes(t, k, "mem1"); h != nil {
		t.Fatalf("holes after the top went: %+v", h)
	}
	if got := f.Stack("port1"); !reflect.DeepEqual(got, []string{"region0"}) {
		t.Fatalf("stack %v", got)
	}
	createRegion(t, k, "mem1", kernel.RegionRAM, 0)
	// no endpoint: no hole
	if h := holes(t, k, "mem9"); h != nil {
		t.Fatalf("unknown memdev: holes %+v", h)
	}

	// a hole in the endpoint port of a memdev with two regions is a hole
	// of its decoder path too: the endpoint names it
	f = fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: serial0, Ram: gib})
	k = f.Kernel()
	createRegion(t, k, "mem0", kernel.RegionRAM, 256*mib)
	createRegion(t, k, "mem0", kernel.RegionRAM, 256*mib)
	noErr(t, k.DestroyRegion(ctx, "region0"))
	want = []kernel.DecoderHole{
		{Port: "port1", Committed: 2, Free: []string{"decoder1.0"}, Blocking: []string{"region1"}},
		{Port: "endpoint4", Committed: 2, Free: []string{"decoder4.0"}, Blocking: []string{"region1"}},
	}
	if got := holes(t, k, "mem0"); !reflect.DeepEqual(got, want) {
		t.Fatalf("holes in the endpoint %+v, want %+v", got, want)
	}
	ports, err := k.Ports("mem0")
	if err != nil || len(ports) != 2 || ports[1].Name != "endpoint4" || ports[1].Decoders[1].Region != "region1" {
		t.Fatalf("ports %+v %v", ports, err)
	}
	// the endpoint's regions are the memdev's own: nothing of another
	// memdev above
	if a := above(t, k, "mem0"); a != nil {
		t.Fatalf("above %v", a)
	}
}

// TestRegionsAbove: the regions of other memdevs above those of a memdev in
// its ports, none when its regions are on top or it has none.
func TestRegionsAbove(t *testing.T) {
	f, k := stackNode(t)
	for serial, want := range map[uint64][]string{serial0: {"region1"}, serial1: {"region2"}} {
		name := f.Memdev(serial)
		if got := above(t, k, name); !reflect.DeepEqual(got, want) {
			t.Errorf("RegionsAbove(%s) = %v, want %v", name, got, want)
		}
	}
	ctx := context.Background()
	noErr(t, k.DestroyRegion(ctx, "region2"))
	if got := above(t, k, "mem1"); got != nil {
		t.Errorf("top: RegionsAbove(mem1) = %v", got)
	}
	noErr(t, k.DestroyRegion(ctx, "region1"))
	if got := above(t, k, "mem1"); got != nil {
		t.Errorf("no region: RegionsAbove(mem1) = %v", got)
	}
	if got := above(t, k, "mem9"); got != nil {
		t.Errorf("unknown memdev: RegionsAbove = %v", got)
	}
	// a hole above the memdev's region is no region
	f.SetStack("port1", "region0", "", "region5")
	if got := above(t, k, "mem0"); !reflect.DeepEqual(got, []string{"region5"}) {
		t.Errorf("below a hole: RegionsAbove(mem0) = %v", got)
	}
}

// A decoder that cannot be read is an error of the decoder path, never
// "nothing above": RegionsAbove and DecoderHoles say so, and the release in
// order touches nothing.
func TestDecoderUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(f *fake.Fake)
	}{
		{"region of a committed decoder missing", func(f *fake.Fake) { f.Remove("devices/platform/ACPI0017:00/root0/port1/port3/decoder3.1/region") }},
		{"decoders_committed missing", func(f *fake.Fake) { f.Remove("devices/platform/ACPI0017:00/root0/port1/decoders_committed") }},
		{"decoders_committed unreadable", func(f *fake.Fake) { f.Write("devices/platform/ACPI0017:00/root0/port1/decoders_committed", "bogus") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, k := stackNode(t)
			tc.damage(f)
			if a, err := k.RegionsAbove("mem0"); err == nil || a != nil {
				t.Fatalf("RegionsAbove %v %v", a, err)
			}
			if h, err := k.DecoderHoles("mem0"); err == nil || h != nil {
				t.Fatalf("DecoderHoles %v %v", h, err)
			}
			before := f.Commands()
			if _, err := k.DestroyRegionInOrder(context.Background(), "mem0", "region0"); err == nil || f.Commands() != before {
				t.Fatalf("release in order: %v\n%s", err, f.CommandsSince(before))
			}
		})
	}
	// unreadable after the disable: the region stays disabled
	f, k := stackNode(t)
	noErr(t, k.DestroyRegion(context.Background(), "region1"))
	noErr(t, k.DestroyRegion(context.Background(), "region2"))
	f.After("cxl disable-region region0", func() { f.Write("devices/platform/ACPI0017:00/root0/port1/decoders_committed", "bogus") })
	_, err := k.DestroyRegionInOrder(context.Background(), "mem0", "region0")
	wantErr(t, err, nil, "not destroying disabled region0: the decoders of port1")
	if r, _ := k.Region("region0"); r.State != kernel.RegionDisabled {
		t.Fatalf("region0 %v", r.State)
	}
}

// TestDestroyRegionInOrder: a region is released when no region of another
// memdev lies above it in the host bridge and the switch, and not touched
// when one does; a region that comes above it during the release (a
// creation between the first check and cxl destroy-region) keeps it
// disabled.
func TestDestroyRegionInOrder(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *fake.Fake, k *kernel.Kernel)
		want  []string
		state kernel.RegionState
		cmds  string
	}{
		{name: "alone", setup: func(t *testing.T, f *fake.Fake, k *kernel.Kernel) {}, state: kernel.RegionAbsent,
			cmds: "cxl disable-region region0\ncxl destroy-region region0"},
		{name: "below", setup: func(t *testing.T, f *fake.Fake, k *kernel.Kernel) {
			createRegion(t, k, "mem1", kernel.RegionRAM, 0)
		}, want: []string{"region1"}, state: kernel.RegionEnabled},
		{name: "came above", setup: func(t *testing.T, f *fake.Fake, k *kernel.Kernel) {
			f.Before("cxl disable-region region0", func() { createRegion(t, k, "mem1", kernel.RegionRAM, 0) })
		}, want: []string{"region1"}, state: kernel.RegionDisabled,
			cmds: "cxl create-region -t ram -d decoder0.0 -m mem1\ncxl disable-region region0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fake.New(t)
			f.Plug(fake.MemdevSpec{Serial: serial0, Switch: true})
			f.Plug(fake.MemdevSpec{Serial: serial1, Switch: true})
			k := f.Kernel()
			createRegion(t, k, "mem0", kernel.RegionRAM, 0)
			tc.setup(t, f, k)
			before := f.Commands()
			got, err := k.DestroyRegionInOrder(ctx, "mem0", "region0")
			noErr(t, err)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("above %v, want %v", got, tc.want)
			}
			if cmds := f.CommandsSince(before); cmds != tc.cmds {
				t.Errorf("commands:\n%s\nwant:\n%s", cmds, tc.cmds)
			}
			if r, _ := k.Region("region0"); r.State != tc.state {
				t.Errorf("region0 %v, want %v", r.State, tc.state)
			}
		})
	}
	// a missing region
	f, k := newNode(t)
	if a, err := k.DestroyRegionInOrder(ctx, "mem0", "region7"); a != nil || err != nil || f.Commands() != "" {
		t.Errorf("missing region: above %v, error %v, commands %q", a, err, f.Commands())
	}
}
