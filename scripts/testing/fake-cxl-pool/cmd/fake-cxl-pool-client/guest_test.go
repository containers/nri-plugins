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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The tests of the guest commands run on the fake of package kernel. The
// steps of the commands have their tests in package kernel.

const serial0 = "0xc1f00001"

// newGuest returns a fake with mem0 of serial0 plugged under host bridge 12,
// and a cli on it. Memory blocks settle offline in 20 ms.
func newGuest(t *testing.T, spec fake.MemdevSpec) (*fake.Fake, *cli) {
	t.Helper()
	old := settleOffline
	settleOffline = 20 * time.Millisecond
	t.Cleanup(func() { settleOffline = old })
	f := fake.New(t)
	spec.Serial = 0xc1f00001
	f.Plug(spec)
	return f, guestCLI(f)
}

// guestCLI returns a cli whose guest commands use the kernel of the fake
// and print JSON.
func guestCLI(f *fake.Fake) *cli {
	return &cli{output: "json", k: f.Kernel()}
}

// guest runs the guest command args and returns its exit code, its output
// and its error output.
func guest(t *testing.T, c *cli, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	c.stdout, c.stderr = &out, &errb
	rc := c.exitCode(c.dispatch(context.Background(), "guest", args))
	return rc, out.String(), errb.String()
}

// guestInfo runs a guest command that prints a region info and returns the
// info.
func guestInfo(t *testing.T, c *cli, args ...string) regionInfo {
	t.Helper()
	rc, out, errs := guest(t, c, args...)
	var ri regionInfo
	if rc != exitOK || json.Unmarshal([]byte(out), &ri) != nil {
		t.Fatalf("guest %v: rc %d\n%s\n%s", args, rc, out, errs)
	}
	return ri
}

func TestGuestWaitAndMemdev(t *testing.T) {
	f := fake.New(t)
	c := guestCLI(f)
	if rc, _, errs := guest(t, c, "memdev", serial0); rc != exitError || !strings.Contains(errs, "no memdev with the serial 0xc1f00001") {
		t.Fatalf("memdev of an absent device: rc %d %q", rc, errs)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.Plug(fake.MemdevSpec{Serial: 0xc1f00001})
	}()
	if rc, out, errs := guest(t, c, "wait", serial0, "--timeout", "5s"); rc != exitOK || out != "mem0\n" {
		t.Fatalf("wait: rc %d %q %q", rc, out, errs)
	}
	if rc, out, errs := guest(t, c, "memdev", serial0); rc != exitOK || out != "mem0\n" {
		t.Fatalf("memdev: rc %d %q %q", rc, out, errs)
	}
	if rc, _, errs := guest(t, c, "wait", "0x1234", "--timeout", "20ms"); rc != exitConflict || !strings.Contains(errs, "serial 0x1234") {
		t.Fatalf("wait timeout: rc %d %q", rc, errs)
	}
}

// A region in mode numa: kmem, its memory onlined movable, released with
// the memdev.
func TestGuestRegionNUMA(t *testing.T) {
	f, c := newGuest(t, fake.MemdevSpec{})
	ri := guestInfo(t, c, "region", "create", serial0, "--mode", "numa")
	want := regionInfo{Memdev: "mem0", Region: "region0", Dax: "dax0.0", Driver: kernel.DriverKmem, Mode: cxlapi.ModeNUMA, Node: 2, Blocks: []int{74, 75}}
	if !reflect.DeepEqual(ri, want) {
		t.Fatalf("region info %+v, expected %+v", ri, want)
	}
	if !strings.Contains(f.Commands(), "cxl create-region -t ram -d decoder0.0 -m mem0") {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	// the region that exists is reused
	if ri := guestInfo(t, c, "region", "create", serial0, "--mode", "numa"); !reflect.DeepEqual(ri, want) {
		t.Fatalf("reused region info %+v", ri)
	}
	if n := strings.Count(f.Commands(), "create-region"); n != 1 {
		t.Fatalf("region created %d times", n)
	}
	guestInfo(t, c, "online", serial0, "--movable")
	f.Write("devices/system/memory/memory75/valid_zones", "Normal")
	if rc, _, errs := guest(t, c, "online", serial0, "--movable"); rc != exitError || !strings.Contains(errs, "zone Normal") {
		t.Fatalf("online movable of a Normal block: rc %d %q", rc, errs)
	}
	f.Write("devices/system/memory/memory75/valid_zones", "Movable")
	guestInfo(t, c, "online", serial0, "--movable")
	if rc, _, errs := guest(t, c, "disable", serial0); rc != exitOK {
		t.Fatalf("release: rc %d %q", rc, errs)
	}
	if want := "cxl disable-region region0\ncxl destroy-region region0\ncxl disable-memdev mem0"; !strings.HasSuffix(f.Commands(), want) {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	// a released memdev and a missing one are released again
	for _, dev := range []string{serial0, "0xc1f00007"} {
		if rc, _, errs := guest(t, c, "disable", dev); rc != exitOK {
			t.Fatalf("release %s again: rc %d %q", dev, rc, errs)
		}
	}
}

// A region in mode dax: the disabled memdev is enabled, device_dax bound
// through sysfs, no id left.
func TestGuestRegionDax(t *testing.T) {
	f, c := newGuest(t, fake.MemdevSpec{Disabled: true})
	ri := guestInfo(t, c, "region", "create", serial0, "--mode", "dax", "--decoder", "decoder0.0")
	if ri.Driver != kernel.DriverDeviceDax || ri.Mode != cxlapi.ModeDax || ri.Device != f.DevRoot()+"/dax0.0" {
		t.Fatalf("region info %+v", ri)
	}
	if cmds := f.Commands(); !strings.HasPrefix(cmds, "cxl enable-memdev mem0\n") || strings.Contains(cmds, "daxctl") {
		t.Fatalf("commands:\n%s", cmds)
	}
	if f.HasDaxID(kernel.DriverDeviceDax, "dax0.0") {
		t.Fatal("an id of dax0.0 left in device_dax")
	}
	if got := guestInfo(t, c, "info", serial0); !reflect.DeepEqual(got, ri) {
		t.Fatalf("info %+v, expected %+v", got, ri)
	}
	if rc, _, errs := guest(t, c, "online", serial0); rc != exitError || !strings.Contains(errs, "not kmem") {
		t.Fatalf("online in mode dax: rc %d %q", rc, errs)
	}
}

// The kernel binds the dax device of a volatile region to kmem by itself,
// and its driver link exists before kmem added the memory blocks: region
// create waits for the blocks, in mode numa and before the switch to
// device_dax.
func TestGuestRegionWaitsForKmemProbe(t *testing.T) {
	for _, mode := range []string{cxlapi.ModeNUMA, cxlapi.ModeDax} {
		f, c := newGuest(t, fake.MemdevSpec{})
		f.NoKmemBlocks = true
		go func() {
			time.Sleep(30 * time.Millisecond)
			f.AddBlocks(74, 75)
		}()
		f.Before("write bus/dax/drivers/kmem/unbind", func() {
			if f.BlockState(74) == "" || f.BlockState(75) == "" {
				t.Errorf("%s: kmem unbound before its memory blocks appeared", mode)
			}
		})
		// device_dax has no memory blocks
		if ri := guestInfo(t, c, "region", "create", serial0, "--mode", mode); mode == cxlapi.ModeNUMA && len(ri.Blocks) != 2 {
			t.Fatalf("%s: region create returned before the memory blocks appeared: %+v", mode, ri)
		}
	}
}

// A udev rule onlines the blocks that kmem adds for a new region, also
// after they were offlined: they are offlined again and stay offline
// before the switch to device_dax.
func TestGuestRegionDaxReofflinesAutoOnlined(t *testing.T) {
	f, c := newGuest(t, fake.MemdevSpec{})
	f.AutoOnline = 2
	const offline = "write devices/system/memory/memory74/state offline"
	offlines := 0
	var count func()
	count = func() {
		offlines++
		f.Before(offline, count)
	}
	f.Before(offline, count)
	f.Before("write bus/dax/drivers/kmem/unbind", func() {
		if offlines != 2 {
			t.Errorf("memory74 offlined %d times, expected 2: once more after the udev rule onlined it again", offlines)
		}
		for _, b := range []int{74, 75} {
			if st := f.BlockState(b); st != kernel.BlockStateOffline {
				t.Errorf("kmem unbound with memory%d %s", b, st)
			}
		}
	})
	if ri := guestInfo(t, c, "region", "create", serial0, "--mode", "dax"); ri.Driver != kernel.DriverDeviceDax {
		t.Fatalf("region info %+v", ri)
	}
}
