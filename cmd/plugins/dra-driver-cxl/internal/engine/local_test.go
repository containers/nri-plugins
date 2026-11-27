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

package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The parts of a local memdev: the first one is daxN.0 of the region
// (its Whole device resized), the next ones new dax devices; numa parts
// online movable on the node fenced before, dax parts device_dax and
// zeroed; the Unprepare of the last part destroys the region.
func TestLocal(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	at := v.onlineAtFence()
	v.mustPrepare(localDriver, "n1", localReq("n1", 0, serialL, state.ModeNUMA, state.KindLocal, 256*mib))
	g := v.grant("n1", 0)
	if g.Got.Dax != "dax0.0" || state.NUMANodeOf(g.Got.NUMANode) != 2 || v.f.DaxDriver("dax0.0") != kernel.DriverKmem {
		t.Fatalf("numa part %+v", g.Got)
	}
	if !slices.Equal(v.fenced, []int{2}) || !slices.Equal(*at, []string{"2: []"}) {
		t.Fatalf("fenced %v, online at the fences %v", v.fenced, *at)
	}
	v.mustPrepare(localDriver, "d1", localReq("d1", 0, serialL, state.ModeDax, state.KindLocal, 200*mib))
	g = v.grant("d1", 0)
	if g.Got.Dax != "dax0.1" || g.Got.Size != 256*mib || v.f.DaxDriver("dax0.1") != kernel.DriverDeviceDax {
		t.Fatalf("dax part %+v", g.Got)
	}
	if z := v.f.Zeroed(); len(z) != 1 || !strings.HasPrefix(z[0], fmt.Sprintf("%d:%d ", v.records().DaxDevOf(g).Major, v.records().DaxDevOf(g).Minor)) {
		t.Fatalf("zeroed %v", z)
	}
	v.mustUnprepare(localDriver, "n1")
	if d := v.records().Region("region0").DaxDevice("dax0.0"); d == nil || d.State != state.DaxSeed {
		t.Fatalf("dax0.0 after its part went: %+v", d)
	}
	// the seed is sized again for the next part
	v.mustPrepare(localDriver, "n2", localReq("n2", 0, serialL, state.ModeNUMA, state.KindLocal, 128*mib))
	if g := v.grant("n2", 0); g.Got.Dax != "dax0.0" {
		t.Fatalf("part of the seed %+v", g.Got)
	}
	if !slices.Equal(*at, []string{"2: []", "2: []"}) {
		t.Fatalf("online at the fences %v", *at)
	}
	v.mustUnprepare(localDriver, "d1")
	v.mustUnprepare(localDriver, "n2")
	v.clean("unprepared")
	if strings.Contains(v.f.Commands(), "disable-memdev") {
		t.Fatal("a local memdev was disabled")
	}
}

// A local memdev configured shared: the Whole device of its region,
// device_dax and zeroed once, for every claim; it is kept while it is
// configured shared, released at the last claim otherwise.
func TestLocalShared(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	shared := true
	v.e.opts.Shared = func(state.Serial) bool { return shared }
	v.mustPrepare(localDriver, "s1", localReq("s1", 0, serialL, state.ModeDax, state.KindLocalShared, gib))
	v.mustPrepare(localDriver, "s2", localReq("s2", 0, serialL, state.ModeDax, state.KindLocalShared, gib))
	if z := v.f.Zeroed(); len(z) != 1 {
		t.Fatalf("zeroed %v", z)
	}
	if d := v.records().Region("region0").DaxDevice("dax0.0"); !d.Whole || len(d.Users) != 2 || d.State != state.DaxDeviceDax {
		t.Fatalf("whole device %+v", d)
	}
	v.mustUnprepare(localDriver, "s1")
	v.mustUnprepare(localDriver, "s2")
	if m := v.records().Memdev(serialL); m == nil || m.State != state.MemdevIdleKept {
		t.Fatalf("memdev %+v", m)
	}
	v.check("idle shared")
	// configured shared again later: the kept region, not zeroed again
	v.mustPrepare(localDriver, "s3", localReq("s3", 0, serialL, state.ModeDax, state.KindLocalShared, gib))
	if z := v.f.Zeroed(); len(z) != 1 {
		t.Fatalf("zeroed again %v", z)
	}
	shared = false
	v.mustUnprepare(localDriver, "s3")
	v.clean("not configured shared any more")
}

// A local grant on a pool serial is refused before anything is touched.
func TestLocalGuard(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialA})
	if err := v.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Provenance = append(doc.Provenance, state.Provenance{Serial: serialA, State: state.ProvenancePool})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := v.f.Commands()
	err := v.prepare(localDriver, "l1", localReq("l1", 0, serialA, state.ModeDax, state.KindLocal, 256*mib))
	if err == nil || !strings.Contains(err.Error(), "pool device") {
		t.Fatalf("error %v", err)
	}
	if cmds := v.f.CommandsSince(before); cmds != "" {
		t.Fatalf("commands %q", cmds)
	}
	v.clean("refused")
}

// A region creation of a local memdev that fails with add_dev is tried
// once more; a second failure fails the grant and leaves nothing. A region
// that the driver did not create is refused untouched, and a memdev that
// is gone is not found.
func TestLocalRegionErrors(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	ctx := context.Background()
	req := localReq("l1", 0, serialL, state.ModeDax, state.KindLocal, 128*mib)
	refused := func(what, want string, is error) {
		t.Helper()
		before := v.f.Commands()
		err := v.prepare(localDriver, "l1", req)
		if !errors.Is(err, is) || err.Error() != want {
			t.Fatalf("%s: error %v, want %q", what, err, want)
		}
		if cmds := v.f.CommandsSince(before); cmds != "" {
			t.Fatalf("%s: commands %q", what, cmds)
		}
	}

	v.f.FailCommand("cxl create-region", fake.ErrAddDev)
	v.mustPrepare(localDriver, "l1", req)
	if n := strings.Count(v.f.Commands(), "cxl create-region"); n != 2 {
		t.Fatalf("region created %d times:\n%s", n, v.f.Commands())
	}
	v.mustUnprepare(localDriver, "l1")

	v.f.FailCommand("cxl create-region", fake.ErrAddDev, fake.ErrAddDev)
	if err := v.prepare(localDriver, "l1", req); err == nil || err.Error() != "memdev 0x11 (mem0): cxl region: cmd_create_region: region0: add_dev() failed" {
		t.Fatalf("error %v", err)
	}
	wantPreparation(t, v, localDriver, "l1", "")
	v.clean("add_dev twice")

	k := v.f.Kernel()
	d, err := k.RootDecoder("mem0", kernel.RegionRAM)
	if err != nil {
		t.Fatal(err)
	}
	region, err := k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: "mem0", Decoder: d, Type: kernel.RegionRAM})
	if err != nil {
		t.Fatal(err)
	}
	refused("foreign region", "memdev mem0 has region region0 that the driver did not create", errForeignRegion)
	if err := k.DestroyRegion(ctx, region); err != nil {
		t.Fatal(err)
	}

	v.f.Unplug(serialL)
	refused("memdev gone", "no memdev with the serial 0x11", kernel.ErrNotFound)
	v.clean("refused")
}

// A first part whose set-up fails leaves nothing: the region does not come
// up in devdax mode, its zeroing fails, kmem cannot be bound, the memory
// not onlined, the part rounded up to whole memory blocks does not fit or
// the memory block size cannot be read. A later part that fails to resize, to
// come up in devdax mode or to online goes alone; a part that fails to delete
// stays with its claim until a retry.
func TestLocalSliceFailures(t *testing.T) {
	enable := errors.New("device_dax: enable failed")
	numa := func(uid string, size uint64) GrantRequest {
		return localReq(uid, 0, serialL, state.ModeNUMA, state.KindLocal, size)
	}
	dax := func(uid string) GrantRequest {
		return localReq(uid, 0, serialL, state.ModeDax, state.KindLocal, 128*mib)
	}
	noDevdax := func(v *env) {
		v.f.FailWrite("bus/dax/drivers/device_dax/new_id", enable)
		v.f.FailWrite("bus/dax/drivers/device_dax/bind", enable)
	}
	failed := func(t *testing.T, v *env, uid string, req GrantRequest, want string) {
		t.Helper()
		if err := v.prepare(localDriver, uid, req); err == nil || err.Error() != want {
			t.Fatalf("error %v, want %q", err, want)
		}
		for b := 74; b < 82; b++ {
			if v.f.BlockState(b) == kernel.BlockStateOnline {
				t.Fatalf("memory%d online", b)
			}
		}
	}
	for _, tc := range []struct {
		name string
		fail func(v *env)
		req  GrantRequest
		err  string
	}{
		{"no devdax", noDevdax, dax("l1"), "bind dax0.0 to device_dax: device_dax: enable failed"},
		{"zeroing", func(v *env) { v.f.ZeroErr = errors.New("mmap: no such device") }, dax("l1"),
			"memdev 0x11 (mem0): zeroing dax0.0: mmap: no such device"},
		{"kmem bind", func(v *env) { v.f.FailWrite("bus/dax/drivers/kmem/bind", syscall.EIO) }, numa("l1", 128*mib),
			"memdev 0x11 (mem0): bind dax0.0 to kmem: input/output error"},
		{"online", func(v *env) { v.f.FailWrite(blockState(75), syscall.EIO) }, numa("l1", 256*mib),
			"memdev 0x11 (mem0): memory75: online_movable: input/output error"},
		{"no room for whole blocks", func(v *env) { v.f.SetBlockSize(2 * gib) }, numa("l1", 128*mib),
			"memdev 0x11 (mem0): no room for the slice: 1073741824 bytes free in region0, the slice needs 2147483648"},
		{"block size unreadable", func(v *env) { v.f.Write("devices/system/memory/block_size_bytes", "bogus") }, dax("l1"),
			`cannot parse the memory block size "bogus" in <sys>/devices/system/memory/block_size_bytes`},
	} {
		t.Run("first part "+tc.name, func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
			tc.fail(v)
			failed(t, v, "l1", tc.req, strings.ReplaceAll(tc.err, "<sys>", v.f.SysRoot()))
			wantPreparation(t, v, localDriver, "l1", "")
			if strings.Contains(v.f.Commands(), "disable-memdev") {
				t.Fatal("a local memdev was disabled")
			}
			v.clean("failed")
		})
	}
	t.Run("later part", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		v.mustPrepare(localDriver, "d1", dax("d1"))
		for _, tc := range []struct {
			fail func()
			req  GrantRequest
			err  string
		}{
			{func() { v.f.FailWrite("bus/dax/devices/dax0.1/size", syscall.EIO) }, dax("d2"),
				"memdev 0x11 (mem0): dax0.1: set size 134217728: input/output error"},
			{func() { noDevdax(v) }, dax("d2"), "memdev 0x11 (mem0): bind dax0.1 to device_dax: device_dax: enable failed"},
			{func() { v.f.FailWrite(blockState(76), syscall.EIO) }, numa("d2", 256*mib),
				"memdev 0x11 (mem0): memory76: online_movable: input/output error"},
		} {
			tc.fail()
			failed(t, v, "d2", tc.req, tc.err)
			if v.f.DaxExists("dax0.1") {
				t.Fatalf("dax0.1 left after %q", tc.err)
			}
		}
		if v.f.DaxDriver("dax0.0") != kernel.DriverDeviceDax || v.f.DaxSize("dax0.0") != 128*mib || len(v.records().Grants) != 1 {
			t.Fatalf("dax0.0 %s %d, grants %+v", v.f.DaxDriver("dax0.0"), v.f.DaxSize("dax0.0"), v.records().Grants)
		}
		v.mustUnprepare(localDriver, "d1")
		v.clean("unprepared")
	})
	t.Run("part that fails to delete", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		v.mustPrepare(localDriver, "d1", dax("d1"))
		v.mustPrepare(localDriver, "d2", dax("d2"))
		d2 := v.grant("d2", 0).Got.Dax
		v.f.FailWrite("bus/cxl/devices/region0/dax_region0/dax_region/delete", syscall.EBUSY)
		if err := v.unprepare(localDriver, "d2"); err == nil || err.Error() != "memdev 0x11 (mem0): deleting slice dax0.1: region0: delete dax0.1: device or resource busy" {
			t.Fatalf("error %v", err)
		}
		if g := v.grant("d2", 0); !v.f.DaxExists(d2) || g == nil || g.State != state.GrantReleasing {
			t.Fatalf("after a failed release: %s exists %v, grant %+v", d2, v.f.DaxExists(d2), g)
		}
		v.mustUnprepare(localDriver, "d2")
		if v.f.DaxExists(d2) {
			t.Fatalf("%s left", d2)
		}
		v.mustUnprepare(localDriver, "d1")
		v.clean("unprepared")
	})
}

// Failures while a part is placed on the whole blocks of a region that
// does not start at a block boundary, bound or onlined leave nothing of
// it: the misplaced first-fit device cannot be deleted, the placed device
// cannot be mapped or bound to kmem, the region cannot be read, the memory
// blocks of kmem never appear, the free memory is only across block
// boundaries.
func TestLocalPlacementFailures(t *testing.T) {
	const bs = 512 * mib
	v := newEnv(t)
	v.f.SetBlockSize(bs)
	v.f.Plug(fake.MemdevSpec{Serial: serialL, Ram: 2 * gib})
	numa := localReq("n1", 0, serialL, state.ModeNUMA, state.KindLocal, bs)
	dax := localReq("n1", 0, serialL, state.ModeDax, state.KindLocal, bs)
	failed := func(what string, req GrantRequest, want string) {
		t.Helper()
		if err := v.prepare(localDriver, "n1", req); err == nil || err.Error() != want {
			t.Fatalf("%s: error %v, want %q", what, err, want)
		}
		for b := 18; b < 23; b++ {
			if v.f.BlockState(b) == kernel.BlockStateOnline {
				t.Fatalf("%s: memory%d online", what, b)
			}
		}
		if devs := v.f.Kernel().DaxDevices("region0"); len(devs) > 2 {
			t.Fatalf("%s: dax devices %v", what, devs)
		}
	}

	v.f.FailWrite("bus/dax/devices/dax0.0/size", nil, syscall.EIO)
	failed("first-fit device not deleted", numa, "memdev 0x11 (mem0): dax0.0: set size 0: input/output error")
	v.clean("first-fit device not deleted")

	v.mustPrepare(localDriver, "d1", localReq("d1", 0, serialL, state.ModeDax, state.KindLocal, bs))
	d1 := v.grant("d1", 0).Got.Dax
	v.f.FailWrite("bus/dax/devices/dax0.2/mapping", syscall.EIO)
	failed("not mapped", numa, "memdev 0x11 (mem0): dax0.2: map 0x280000000-0x29fffffff: input/output error")
	v.f.FailWrite("bus/dax/drivers/kmem/bind", syscall.EIO)
	failed("not bound", numa, "memdev 0x11 (mem0): bind dax0.2 to kmem: input/output error")

	size := v.f.Read("bus/cxl/devices/region0/size")
	v.f.Before("write bus/dax/devices/dax0.0/size 0", func() { v.f.Write("bus/cxl/devices/region0/size", "bogus") })
	err := v.prepare(localDriver, "n1", dax)
	v.f.Write("bus/cxl/devices/region0/size", size)
	// the available size that the fake wrote while the size was bogus
	free := uint64(0)
	for _, g := range v.f.Gaps("region0") {
		free += g.Size()
	}
	v.f.Write("bus/cxl/devices/region0/dax_region0/dax_region/available_size", fmt.Sprint(free))
	if want := `memdev 0x11 (mem0): cannot parse region region0 resource "0x250000000" size "bogus"`; err == nil || err.Error() != want {
		t.Fatalf("region unreadable: error %v, want %q", err, want)
	}
	v.check("region unreadable")

	v.f.NoKmemBlocks, v.prepareTimeout = true, 50*time.Millisecond
	failed("no memory blocks", numa, "memdev 0x11 (mem0): dax0.2: 0 of 1 memory blocks appeared: context deadline exceeded")
	v.f.NoKmemBlocks, v.prepareTimeout = false, 0

	// the free memory is left only across block boundaries
	v.mustPrepare(localDriver, "d2", localReq("d2", 0, serialL, state.ModeDax, state.KindLocal, bs))
	v.mustPrepare(localDriver, "d3", localReq("d3", 0, serialL, state.ModeDax, state.KindLocal, bs))
	err = v.prepare(localDriver, "n1", numa)
	if want := "memdev 0x11 (mem0): no room for the slice: no 536870912 bytes of whole free memory blocks in region0 " +
		"(free [0x250000000-0x25fffffff 0x2c0000000-0x2cfffffff], memory block size 536870912)"; !errors.Is(err, kernel.ErrNoRoom) || err.Error() != want {
		t.Fatalf("no whole blocks: error %v, want %q", err, want)
	}
	v.check("no whole blocks")
	v.mustUnprepare(localDriver, "d2")
	v.mustUnprepare(localDriver, "d3")

	if v.f.DaxSize("dax0.0") != 0 || v.f.DaxDriver("dax0.0") != "" || len(v.records().Grants) != 1 ||
		v.f.DaxDriver(d1) != kernel.DriverDeviceDax || v.f.DaxSize(d1) != bs {
		t.Fatalf("dax0.0 %d %q, %s %d %q, grants %+v", v.f.DaxSize("dax0.0"), v.f.DaxDriver("dax0.0"),
			d1, v.f.DaxSize(d1), v.f.DaxDriver(d1), v.records().Grants)
	}
	v.mustUnprepare(localDriver, "d1")
	v.clean("unprepared")
}

// Parts of a region that does not start at a memory block boundary are
// placed on whole blocks: a part that first-fit put across a boundary is
// deleted (daxN.0 back to the seed) and made again through the mapping.
func TestLocalSlicesOnWholeBlocks(t *testing.T) {
	v := newEnv(t)
	v.f.SetBlockSize(512 * mib)
	v.f.Plug(fake.MemdevSpec{Serial: serialL, Ram: 4 * gib})
	v.mustPrepare(localDriver, "n1", localReq("n1", 0, serialL, state.ModeNUMA, state.KindLocal, 512*mib))
	v.mustPrepare(localDriver, "n2", localReq("n2", 0, serialL, state.ModeDax, state.KindLocal, 512*mib))
	for _, uid := range []string{"n1", "n2"} {
		g := v.grant(uid, 0)
		for _, r := range v.f.DaxRanges(g.Got.Dax) {
			if !r.Aligned(512 * mib) {
				t.Fatalf("%s: %s at %v", uid, g.Got.Dax, r)
			}
		}
	}
	v.mustUnprepare(localDriver, "n1")
	v.mustUnprepare(localDriver, "n2")
	v.clean("unprepared")
}

// A new part that first-fit put across a block boundary (another dax
// device took the memory before it) is deleted and placed on the first
// whole free blocks.
func TestLocalNewSliceMisplaced(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.mustPrepare(localDriver, "n1", localReq("n1", 0, serialL, state.ModeDax, state.KindLocal, 128*mib))
	k := v.f.Kernel()
	ctx := context.Background()
	stray, err := k.CreateDaxDevice(ctx, "region0", 64*mib)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.prepare(localDriver, "n2", localReq("n2", 0, serialL, state.ModeDax, state.KindLocal, 128*mib)); err != nil {
		t.Fatal(err)
	}
	g := v.grant("n2", 0)
	for _, r := range v.f.DaxRanges(g.Got.Dax) {
		if !r.Aligned(128 * mib) {
			t.Fatalf("%s at %v", g.Got.Dax, r)
		}
	}
	if err := k.DeleteDaxDevice(ctx, "region0", stray); err != nil {
		t.Fatal(err)
	}
	v.check("placed")
	v.mustUnprepare(localDriver, "n2")
	v.mustUnprepare(localDriver, "n1")
	v.clean("unprepared")
}

// daxTrail records the states of the record of a dax device of a region
// after each commit of the engine of the node, one entry per change:
// "<state> whole|part users=<n> size=<kernel size>".
type daxTrail struct {
	region state.RegionRef
	dax    string
	steps  []string
}

// trail starts the trail of the dax device of the region on the engine of
// the node.
func (v *env) trail(region, dax string) *daxTrail {
	tr := &daxTrail{region: state.RegionRef(region), dax: dax}
	prev := v.e.afterCommit
	v.e.afterCommit = func() {
		step := "none"
		if reg := v.records().Region(tr.region); reg != nil {
			if d := reg.DaxDevice(tr.dax); d != nil {
				kind := "part"
				if d.Whole {
					kind = "whole"
				}
				step = fmt.Sprintf("%s %s users=%d size=%d", d.State, kind, len(d.Users), v.f.DaxSize(tr.dax))
			}
		}
		if n := len(tr.steps); n == 0 || tr.steps[n-1] != step {
			tr.steps = append(tr.steps, step)
		}
		prev()
	}
	return tr
}

// want fails the test unless the trail has the steps want in order, and
// starts it again.
func (tr *daxTrail) want(t *testing.T, want ...string) {
	t.Helper()
	i := 0
	for _, s := range tr.steps {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	if i < len(want) {
		t.Fatalf("%s of %s: steps\n  %s\nwant in order\n  %s", tr.dax, tr.region, strings.Join(tr.steps, "\n  "), strings.Join(want, "\n  "))
	}
	tr.steps = nil
}

// The Whole device rows of the local paths, by the records after each
// commit: daxN.0 of a new region KMEM_OFFLINE, set up for device_dax
// (through UNBOUND) as the Whole device; the first part reuses it
// (DEVICE_DAX -> UNBOUND), its release makes it the seed (SEED), the next
// part sizes the seed again (SEED -> UNBOUND); the first device of a KEPT
// region that a cut resize left smaller is spanned again by its set-up.
func TestLocalWholeDeviceRows(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
	tr := v.trail("region0", "dax0.0")
	v.mustPrepare(localDriver, "d1", sliceReq("d1", 0, state.ModeDax, 256*mib))
	tr.want(t, "KMEM_OFFLINE whole users=0 size=1073741824", "DEVICE_DAX whole users=0 size=1073741824",
		"UNBOUND part users=1 size=268435456", "DEVICE_DAX part users=1 size=268435456")
	if reg := v.records().Region("region0"); !reg.SetUp || reg.WholeDevice() != nil {
		t.Fatalf("region %+v", reg)
	}
	v.mustPrepare(localDriver, "d2", sliceReq("d2", 0, state.ModeDax, 256*mib))
	v.mustUnprepare(localDriver, "d1")
	tr.want(t, "SEED part users=0 size=0")
	v.mustPrepare(localDriver, "n3", sliceReq("n3", 0, state.ModeNUMA, 256*mib))
	tr.want(t, "UNBOUND part users=1 size=268435456", "KMEM_OFFLINE part users=1 size=268435456", "KMEM_ONLINE part users=1 size=268435456")
	v.mustUnprepare(localDriver, "n3")
	v.mustUnprepare(localDriver, "d2")
	v.clean("unprepared")

	// a cut after the resize of the first part, below a newer region: KEPT
	// with daxN.0 smaller, unbound
	v.f.CutAfter("write bus/dax/devices/dax0.0/size")
	if !v.cut(func() { _ = v.prepare(localDriver, "k", sliceReq("k", 0, state.ModeDax, 256*mib)) }) {
		t.Fatal("not cut")
	}
	above := rawRegion(t, v, "mem1")
	if out := v.restart(); !slices.Equal(out.Kept, []state.GrantRef{{Claim: "k"}}) {
		t.Fatalf("recovered %+v", out)
	}
	wantRegion(t, v, "region0", state.RegionKept)
	if size, drv := v.f.DaxSize("dax0.0"), v.f.DaxDriver("dax0.0"); size != 256*mib || drv != "" {
		t.Fatalf("dax0.0 of %d bytes, driver %q", size, drv)
	}
	logs := klogtest.Capture(t, 3)
	tr = v.trail("region0", "dax0.0")
	v.mustPrepare(localDriver, "k", sliceReq("k", 0, state.ModeDax, 256*mib))
	tr.want(t, "DEVICE_DAX whole users=0 size=1073741824", "UNBOUND part users=1 size=268435456")
	wantLog(t, logs(), "engine: local memdev 0x11 (mem0): set up kept region0 (devdax, dax dax0.0, NUMA node 2) in ")
	dropRaw(t, v, above)
	v.mustUnprepare(localDriver, "k")
	v.clean("unprepared")
}

// The local grants of a release are released by the serial of their
// memdev, the last result of a claim on a memdev first; the others stay
// where they are.
func TestLocalOrder(t *testing.T) {
	r := &state.Records{}
	add := func(uid string, n int, serial state.Serial, source state.Source) state.GrantRef {
		ref := state.GrantRef{Claim: uid, Result: n}
		r.Grants = append(r.Grants, state.Grant{Ref: ref, Serial: serial, Asked: state.Asked{Source: source}})
		return ref
	}
	p0 := add("p", 0, serialA, state.SourcePool)
	c0 := add("c", 0, 0x22, state.SourceLocal)
	c1 := add("c", 1, serialL, state.SourceLocal)
	p1 := add("p", 1, serialB, state.SourcePool)
	c2 := add("c", 2, 0x22, state.SourceLocal)
	c3 := add("c", 3, serialL, state.SourceLocal)
	gone := state.GrantRef{Claim: "x"}
	got := localOrder(r, []state.GrantRef{p0, c0, c1, p1, gone, c2, c3})
	if want := []state.GrantRef{p0, c3, c1, p1, gone, c2, c0}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// The release of a claim with parts on two local memdevs, by the kernel:
// the memdev of the lower serial first, the last part of a memdev first.
func TestLocalReleaseOrder(t *testing.T) {
	const serialM = 0x22
	v := newEnv(t, fake.MemdevSpec{Serial: serialM, Ram: gib, HostBridge: 24}, fake.MemdevSpec{Serial: serialL, Ram: gib})
	reqs := []GrantRequest{
		localReq("c", 0, serialM, state.ModeDax, state.KindLocal, 256*mib),
		localReq("c", 1, serialL, state.ModeDax, state.KindLocal, 256*mib),
		localReq("c", 2, serialM, state.ModeDax, state.KindLocal, 256*mib),
		localReq("c", 3, serialL, state.ModeDax, state.KindLocal, 256*mib),
	}
	v.mustPrepare(localDriver, "c", reqs...)
	part := func(n int) string {
		g := v.grant("c", n)
		return fmt.Sprintf("%s %s", g.Got.Region, g.Got.Dax)
	}
	parts := []string{part(3), part(1), part(2), part(0)}
	logs := klogtest.Capture(t, 3)
	before := v.f.Commands()
	v.mustUnprepare(localDriver, "c")
	var deleted []string
	for _, l := range linesWithPrefix(logs(), "engine: local memdev ") {
		if _, rest, ok := strings.Cut(l, ": claim c: deleted slice "); ok {
			dax, _, _ := strings.Cut(rest, " ")
			deleted = append(deleted, dax)
		}
	}
	var want []string
	for _, p := range parts {
		want = append(want, strings.Fields(p)[1])
	}
	if !slices.Equal(deleted, want) {
		t.Fatalf("deleted %v, want %v (parts %v)", deleted, want, parts)
	}
	// the region of the memdev of the lower serial first
	cmds := v.f.CommandsSince(before)
	regL, regM := strings.Fields(parts[0])[0], strings.Fields(parts[2])[0]
	if l, m := strings.Index(cmds, "destroy-region "+regL), strings.Index(cmds, "destroy-region "+regM); l < 0 || m < l {
		t.Fatalf("commands %s", cmds)
	}
	v.clean("unprepared")
}

// A local grant on a memdev that grants of other claims use in another way
// is refused before anything is touched, with the words of the refusal.
func TestLocalKindConflicts(t *testing.T) {
	sharedPmem := func(uid string, shared bool) GrantRequest {
		req := localPmemReq(uid, 0, state.ModeFS, true, "")
		req.Facts.Shared = shared
		return req
	}
	for _, c := range []struct {
		name  string
		spec  fake.MemdevSpec
		first GrantRequest
		req   GrantRequest
		want  string
	}{
		{"local on local-shared", fake.MemdevSpec{Serial: serialL, Ram: gib}, sharedReq("u1"), sliceReq("x", 0, state.ModeDax, 256*mib),
			"memdev 0x11 (mem0): used as a local-shared device by claims [u1]; a local request cannot use it"},
		{"local-shared on local", fake.MemdevSpec{Serial: serialL, Ram: gib}, sliceReq("n1", 0, state.ModeNUMA, 256*mib), sharedReq("x"),
			"memdev 0x11 (mem0): used as a local device by claims [n1]; a local-shared request cannot use it"},
		{"local on local-persistent", fake.MemdevSpec{Serial: serialP, Pmem: gib}, sharedPmem("w", false),
			localReq("x", 0, serialP, state.ModeDax, state.KindLocal, 256*mib),
			"memdev 0x91 (mem0): used as a local-persistent device by claims [w]; a local request cannot use it"},
		{"shared on persistent", fake.MemdevSpec{Serial: serialP, Pmem: gib}, sharedPmem("w", false),
			localReq("x", 0, serialP, state.ModeDax, state.KindLocalShared, gib),
			"memdev 0x91 (mem0): used as a local-persistent device by claims [w]; a local-shared request cannot use it"},
		{"local-persistent on local", fake.MemdevSpec{Serial: serialL, Ram: gib}, sliceReq("n1", 0, state.ModeNUMA, 256*mib),
			localReq("x", 0, serialL, state.ModeBlock, state.KindLocalPersistent, 0),
			"memdev 0x11 (mem0): used as a local device by claims [n1]; a local-persistent request cannot use it"},
		{"persistent on shared", fake.MemdevSpec{Serial: serialL, Ram: gib}, sharedReq("u1"),
			localReq("x", 0, serialL, state.ModeBlock, state.KindLocalPersistent, 0),
			"memdev 0x11 (mem0): used as a local-shared device by claims [u1]; a local-persistent request cannot use it"},
		{"unshared on shared persistent", fake.MemdevSpec{Serial: serialP, Pmem: gib}, sharedPmem("w", true), sharedPmem("x", false),
			"memdev 0x91 (mem0): used as a shared local-persistent device by claims [w]; it is not shared now (sharedMemdevs)"},
		{"shared on unshared persistent", fake.MemdevSpec{Serial: serialP, Pmem: gib}, sharedPmem("w", false), sharedPmem("x", true),
			"memdev 0x91 (mem0): used as a not shared local-persistent device by claims [w]; it is shared now (sharedMemdevs)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, c.spec)
			v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
			if c.spec.Pmem > 0 {
				v.formatPmem("mem0", "ext4")
			}
			v.mustPrepare(localDriver, c.first.Ref.Claim, c.first)
			before := v.f.Commands()
			gen := v.st.Snapshot().Generation
			_, err := v.e.PrepareGrants(context.Background(), []GrantRequest{c.req})
			var kind *localKindError
			if !errors.As(err, &kind) || err.Error() != c.want {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			if cmds := v.f.CommandsSince(before); cmds != "" || v.st.Snapshot().Generation != gen {
				t.Fatalf("commands %q, generation %d -> %d", cmds, gen, v.st.Snapshot().Generation)
			}
			v.check("refused")
			v.mustUnprepare(localDriver, c.first.Ref.Claim)
			releaseIdleShared(v)
			v.clean("unprepared")
		})
	}
}

// A local memdev that no grant uses but whose region is of another kind
// (an idle shared one) is released first, its record dropped, for a grant
// of another kind.
func TestLocalKindIdleReleasedFirst(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
	v.mustPrepare(localDriver, "s", sharedReq("s"))
	v.mustUnprepare(localDriver, "s")
	if m := v.records().Memdev(serialL); m == nil || m.State != state.MemdevIdleKept {
		t.Fatalf("memdev %+v", m)
	}
	logs := klogtest.Capture(t, 3)
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
	out := logs()
	wantLog(t, out, "engine: local memdev 0x11 (mem0): destroyed region0 in ", "engine: local memdev 0x11 (mem0): created region0 ")
	if i, j := strings.Index(out, "destroyed region0 in "), strings.Index(out, "created region0 "); j < i {
		t.Fatalf("the region was not released first:\n%s", out)
	}
	if m := v.records().Memdev(serialL); m.Facts.Shared || m.State != state.MemdevActive {
		t.Fatalf("memdev %+v", m)
	}
	if z := v.f.Zeroed(); len(z) != 2 {
		t.Fatalf("zeroed %v", z)
	}
	v.mustUnprepare(localDriver, "n")
	v.clean("unprepared")
}

// stepTime is the time of a step in a log line.
var stepTime = regexp.MustCompile(`in [0-9]+\.[0-9]+ms`)

// wantMessages fails the test unless every wanted line is a message of the
// log; "in <t>" in a wanted line stands for the time of a step.
func wantMessages(t *testing.T, logs string, want ...string) {
	t.Helper()
	var msgs []string
	for _, l := range strings.Split(logs, "\n") {
		if _, msg, ok := strings.Cut(l, "] "); ok {
			msgs = append(msgs, stepTime.ReplaceAllString(msg, "in <t>"))
		}
	}
	for _, w := range want {
		if !slices.Contains(msgs, w) {
			t.Errorf("no %q in\n  %s", w, strings.Join(msgs, "\n  "))
		}
	}
}

// The log lines of the local grants, as the documentation quotes them:
// the region created, the parts created, zeroed, onlined (the rounded
// size), offlined and deleted, the region released; a shared device zeroed
// once, released with the claims left and kept idle; a persistent device
// released with the claims left, kept at a start and kept idle below a
// newer region.
func TestLocalLogLines(t *testing.T) {
	const memdev = "engine: local memdev 0x11 (mem0): "
	t.Run("local", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		logs := klogtest.Capture(t, 3)
		v.mustPrepare(localDriver, "d", sliceReq("d", 0, state.ModeDax, 128*mib))
		v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeNUMA, 100*mib))
		v.mustUnprepare(localDriver, "n")
		v.mustUnprepare(localDriver, "d")
		out := logs()
		wantMessages(t, out,
			memdev+"created region0 (devdax, dax dax0.0, NUMA node 2) in <t>",
			memdev+"destroyed region0 in <t>",
			memdev+"claim d: zeroed slice dax0.0 mode dax size 134217728 in <t>",
			memdev+"claim d: created slice dax0.0 mode dax size 134217728 at [0x250000000-0x257ffffff] in <t>",
			memdev+"claim d: deleted slice dax0.0 mode dax size 134217728 in <t>",
			memdev+"claim n: created slice dax0.1 mode numa size 134217728 at [0x258000000-0x25fffffff] in <t>",
			memdev+"claim n: onlined slice dax0.1 mode numa size 134217728 blocks [75] NUMA node 2 in <t>",
			memdev+"claim n: offlined slice dax0.1 mode numa size 134217728 blocks [75] in <t>",
			memdev+"claim n: deleted slice dax0.1 mode numa size 134217728 in <t>",
			memdev+"claim n: slice of 104857600 bytes rounded up to 134217728 (memory block size 134217728, align 2097152)")
	})
	t.Run("shared", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		v.configure(func(o *Options) { o.Shared = func(state.Serial) bool { return true } })
		logs := klogtest.Capture(t, 3)
		v.mustPrepare(localDriver, "s1", sharedReq("s1"))
		v.mustPrepare(localDriver, "s2", sharedReq("s2"))
		v.mustUnprepare(localDriver, "s1")
		v.mustUnprepare(localDriver, "s2")
		out := logs()
		wantMessages(t, out,
			memdev+"zeroed new shared dax0.0 size 1073741824 in <t>",
			memdev+"keeping region0 and dax0.0 of the idle shared device",
			memdev+"claim s1: released shared dax0.0, claims left [s2]",
			memdev+"claim s2: released shared dax0.0, claims left []")
		if n := strings.Count(out, "zeroed new shared "); n != 1 {
			t.Errorf("%d lines of the zeroing", n)
		}
		releaseIdleShared(v)
		v.clean("released")
	})
	t.Run("persistent", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib}, fake.MemdevSpec{Serial: serialB})
		v.formatPmem("mem0", "ext4")
		pmem := filepath.Join(v.f.DevRoot(), "pmem0")
		const memdev = "engine: local memdev 0x91 (mem0): "
		logs := klogtest.Capture(t, 3)
		v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
		v.mustPrepare(localDriver, "r", localPmemReq("r", 0, state.ModeFS, true, ""))
		v.mustUnprepare(localDriver, "w")
		v.restart()
		above := rawRegion(t, v, "mem1")
		v.mustUnprepare(localDriver, "r")
		out := logs()
		wantMessages(t, out,
			memdev+"claim w: released persistent "+pmem+", claims left [r]",
			memdev+"keeping persistent region0, claims [r]",
			memdev+"keeping region0 of the idle persistent device: region1 lie above it, its release would leave a hole in the decoders of their ports")
		if strings.Contains(out, "claim r: released persistent") {
			t.Errorf("the release of the last claim says the claims left:\n%s", out)
		}
		dropRaw(t, v, above)
		v.restart()
		v.clean("released")
	})
	t.Run("persistent region created", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
		v.formatPmem("mem0", "ext4")
		logs := klogtest.Capture(t, 3)
		v.mustPrepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
		wantMessages(t, logs(), "engine: local memdev 0x91 (mem0): created region0 (pmem, nvdimm region0) in <t>")
		v.mustUnprepare(localDriver, "w")
		v.clean("unprepared")
	})
}

// Timeouts.LocalSettle bounds the wait of a local grant for the kernel to
// set up its region (the nd region of a pmem region, the dax device of a
// ram region bound to its driver); a pool grant waits for Timeouts.Settle.
func TestLocalSettle(t *testing.T) {
	t.Run("pmem", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialP, Pmem: gib})
		v.formatPmem("mem0", "ext4")
		v.configure(func(o *Options) { o.Timeouts.LocalSettle = 50 * time.Millisecond })
		if got := v.e.opts.Timeouts; got.LocalSettle != 50*time.Millisecond || got.Settle != 5*time.Second {
			t.Fatalf("timeouts %+v", got)
		}
		v.f.PmemDelay = 200 * time.Millisecond
		start := time.Now()
		err := v.prepare(localDriver, "w", localPmemReq("w", 0, state.ModeFS, false, ""))
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
			t.Fatalf("error %v after %v", err, time.Since(start))
		}
		v.f.PmemDelay = 0
		v.check("rolled back")
		v.clean("rolled back")
		// the pool grant of the same device waits longer
		v.f.PmemDelay = 100 * time.Millisecond
		v.mustPrepare(poolDriver, "p", fsGrant("p", false, ""))
		v.mustUnprepare(poolDriver, "p")
		v.clean("unprepared")
	})
	t.Run("ram", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialA})
		v.configure(func(o *Options) { o.Timeouts.LocalSettle = 50 * time.Millisecond })
		// the kernel binds daxN.0 of a new ram region to kmem after cxl
		// create-region returns: its driver link is removed then, and made
		// again by bind
		const link = "bus/dax/devices/dax0.0/driver"
		bind := func() { v.f.LinkTo(link, "bus/dax/drivers/kmem") }
		v.f.After("cxl create-region", func() {
			v.f.Remove(link)
			// bound when the rollback releases the region
			v.f.Before("cxl disable-region", bind)
		})
		start := time.Now()
		err := v.prepare(localDriver, "n", sliceReq("n", 0, state.ModeNUMA, 256*mib))
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no dax device bound to a driver appeared in region0") ||
			time.Since(start) > 2*time.Second {
			t.Fatalf("error %v after %v", err, time.Since(start))
		}
		v.check("rolled back")
		v.clean("rolled back")
		// the pool grant waits longer: bound after 100 ms
		v.f.After("cxl create-region", func() {
			v.f.Remove(link)
			time.AfterFunc(100*time.Millisecond, bind)
		})
		start = time.Now()
		v.mustPrepare(poolDriver, "p", poolReq("p", 0, serialA, state.ModeNUMA, state.KindPool))
		if took := time.Since(start); took < 100*time.Millisecond {
			t.Fatalf("the pool grant was prepared in %v, before its dax device was bound", took)
		}
		v.mustUnprepare(poolDriver, "p")
		v.clean("unprepared")
	})
}

// The numa part of a companion claim that a process stop cut is kept at
// the start; the Prepare again reuses it when it went online (the same dax
// device and node, no second part), and deletes it for a new part when it
// did not.
func TestLocalCutCompanionPart(t *testing.T) {
	companion := func() GrantRequest {
		req := sliceReq("comp", 0, state.ModeNUMA, 256*mib)
		req.Lend = &state.GrantLend{Lease: "0123abcd", Container: "c"}
		return req
	}
	ref := state.GrantRef{Claim: "comp"}
	t.Run("online", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
		v.e.afterCommit = func() {
			r := v.records()
			if g := r.Grant(ref); g != nil && g.Got.Dax != "" {
				if d := r.Region(g.Got.Region).DaxDevice(g.Got.Dax); d != nil && d.State == state.DaxKmemOnline {
					panic(fake.ErrCut)
				}
			}
		}
		if !v.cut(func() { _ = v.prepare(localDriver, "comp", companion()) }) {
			t.Fatal("not cut")
		}
		v.restart()
		v.mustKeepCut(ref.Claim)
		dax := v.grant("comp", 0).Got.Dax
		devices := v.f.Kernel().DaxDevices("region0")
		logs := klogtest.Capture(t, 3)
		v.mustPrepare(localDriver, "comp", companion())
		g := v.grant("comp", 0)
		if g.Got.Dax != dax || state.NUMANodeOf(g.Got.NUMANode) != 2 || g.State != state.GrantRealised {
			t.Fatalf("grant %+v, the part was %s", g, dax)
		}
		if got := v.f.Kernel().DaxDevices("region0"); !slices.Equal(got, devices) {
			t.Fatalf("dax devices %v, were %v", got, devices)
		}
		if out := logs(); strings.Contains(out, "claim comp: created slice") || strings.Contains(out, "claim comp: deleted slice") {
			t.Fatalf("a second part:\n%s", out)
		}
		v.mustUnprepare(localDriver, "comp")
		v.mustUnprepare(localDriver, "n")
		v.clean("unprepared")
	})
	t.Run("not online", func(t *testing.T) {
		v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
		v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
		v.f.CutAfter("write bus/dax/drivers/kmem/bind")
		if !v.cut(func() { _ = v.prepare(localDriver, "comp", companion()) }) {
			t.Fatal("not cut")
		}
		v.restart()
		v.mustKeepCut(ref.Claim)
		dax := v.grant("comp", 0).Got.Dax
		if dax == "" || v.f.DaxDriver(dax) != kernel.DriverKmem {
			t.Fatalf("the cut part %q (%s)", dax, v.f.DaxDriver(dax))
		}
		logs := klogtest.Capture(t, 3)
		v.mustPrepare(localDriver, "comp", companion())
		out2 := logs()
		deleted, created := strings.Index(out2, "claim comp: deleted slice "+dax+" "), strings.Index(out2, "claim comp: created slice ")
		if deleted < 0 || created < deleted {
			t.Fatalf("the cut part is not deleted before a new one:\n%s", out2)
		}
		if g := v.grant("comp", 0); v.f.DaxDriver(g.Got.Dax) != kernel.DriverKmem || g.State != state.GrantRealised {
			t.Fatalf("grant %+v", g)
		}
		v.mustUnprepare(localDriver, "comp")
		v.mustUnprepare(localDriver, "n")
		v.clean("unprepared")
	})
}

// A numa companion whose Prepare a process stop cut before its region was
// made (the start kept it) holds no region: the region that another claim
// of the memdev makes goes with that claim, cut or unprepared.
func TestLocalCutCompanionHoldsNoNewRegion(t *testing.T) {
	for _, cut := range []bool{true, false} {
		t.Run(map[bool]string{true: "cut", false: "unprepared"}[cut], func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
			req := sliceReq("comp", 0, state.ModeNUMA, 256*mib)
			req.Lend = &state.GrantLend{Lease: "0123abcd", Container: "c"}
			v.f.CutBefore("cxl create-region")
			if !v.cut(func() { _ = v.prepare(localDriver, "comp", req) }) {
				t.Fatal("not cut")
			}
			v.restart()
			v.mustKeepCut("comp")
			if cut {
				v.f.CutAfter("cxl create-region")
				if !v.cut(func() { _ = v.prepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib)) }) {
					t.Fatal("not cut")
				}
				v.restart()
			} else {
				v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
				v.mustUnprepare(localDriver, "n")
			}
			if regions := v.f.Kernel().Regions(v.f.Memdev(serialL)); len(regions) > 0 {
				t.Fatalf("regions left %v", regions)
			}
			v.mustUnprepare(localDriver, "n")
			v.mustUnprepare(localDriver, "comp")
			v.clean("unprepared")
		})
	}
}

// The Unprepare of a numa companion whose Prepare a process stop cut after
// its region was made, or set up, before it got its part (the start kept
// it) fails: the region's whole dax device counts as used by the releasing
// grant, whether a record names it yet (set up, cxl destroy-region fails)
// or not (made, cxl disable-region fails), and the retry releases
// everything.
func TestLocalCutCompanionRegionReleaseFails(t *testing.T) {
	for _, setUp := range []bool{false, true} {
		t.Run(map[bool]string{false: "made", true: "set up"}[setUp], func(t *testing.T) {
			v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
			req := sliceReq("comp", 0, state.ModeNUMA, 256*mib)
			req.Lend = &state.GrantLend{Lease: "0123abcd", Container: "c"}
			if setUp {
				v.e.afterCommit = func() {
					if reg := v.records().Region("region0"); reg != nil && reg.SetUp {
						panic(fake.ErrCut)
					}
				}
			} else {
				v.f.CutAfter("cxl create-region")
			}
			if !v.cut(func() { _ = v.prepare(localDriver, "comp", req) }) {
				t.Fatal("not cut")
			}
			v.restart()
			v.mustKeepCut("comp")
			v.f.FailCommand(map[bool]string{false: "cxl disable-region", true: "cxl destroy-region"}[setUp], errors.New("failed"))
			if err := v.unprepare(localDriver, "comp"); err == nil {
				t.Fatal("the release did not fail")
			}
			v.check("the release failed")
			v.mustUnprepare(localDriver, "comp")
			v.clean("unprepared")
		})
	}
}

// A numa companion whose Prepare enabled again a region that a failed
// release left disabled and a process stop cut before its set-up (the
// start kept it): the region's whole dax device, recorded as the seed,
// counts as used.
func TestLocalCutCompanionEnabledRegion(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeNUMA, 256*mib))
	v.f.FailCommand("cxl destroy-region", errors.New("failed"))
	if err := v.unprepare(localDriver, "n"); err == nil {
		t.Fatal("the release did not fail")
	}
	req := sliceReq("comp", 0, state.ModeNUMA, 256*mib)
	req.Lend = &state.GrantLend{Lease: "0123abcd", Container: "c"}
	v.e.afterCommit = func() {
		r := v.records()
		if reg := r.Region("region0"); reg != nil && reg.State == state.RegionOwned && !reg.SetUp && r.Grant(state.GrantRef{Claim: "comp"}) != nil {
			panic(fake.ErrCut)
		}
	}
	if !v.cut(func() { _ = v.prepare(localDriver, "comp", req) }) {
		t.Fatal("not cut")
	}
	v.restart()
	v.mustKeepCut("comp")
	v.mustUnprepare(localDriver, "comp")
	v.mustUnprepare(localDriver, "n")
	v.clean("unprepared")
}

// The Unprepare of a numa companion whose Prepare a process stop cut while
// it made its part next to another claim's (the start kept it, its dax
// device CREATING) rolls that part back and releases everything.
func TestLocalCutCompanionCreatingPart(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
	req := sliceReq("comp", 0, state.ModeNUMA, 256*mib)
	req.Lend = &state.GrantLend{Lease: "0123abcd", Container: "c"}
	v.e.afterCommit = func() {
		for _, reg := range v.records().Regions {
			for _, d := range reg.DaxDevices {
				if d.State == state.DaxCreating {
					panic(fake.ErrCut)
				}
			}
		}
	}
	if !v.cut(func() { _ = v.prepare(localDriver, "comp", req) }) {
		t.Fatal("not cut")
	}
	v.restart()
	v.mustKeepCut("comp")
	v.mustUnprepare(localDriver, "comp")
	v.mustUnprepare(localDriver, "n")
	v.clean("unprepared")
}
