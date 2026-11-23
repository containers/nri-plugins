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
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// probeEnv returns a node with the memdevs of specs whose engines probe the
// new regions of local memdevs.
func probeEnv(t *testing.T, specs ...fake.MemdevSpec) *env {
	t.Helper()
	v := newEnv(t, specs...)
	v.configure(func(o *Options) { probeOptions(v, o) })
	return v
}

// regionSize returns the size of a region in the kernel.
func regionSize(t *testing.T, v *env, region string) uint64 {
	t.Helper()
	ri, err := v.f.Kernel().Region(region)
	if err != nil {
		t.Fatal(err)
	}
	return ri.Range.Size()
}

// aliasing returns a probe result of memory that holds offset distinct
// bytes.
func aliasing(offset uint64) *kernel.Aliasing {
	return &kernel.Aliasing{Offset: offset, Garbage: -1}
}

// A new local region whose memory aliases is released and made again with
// the distinct bytes only; the probe is saved in the probes file by hex
// serial, and a new engine loads it: the next region of the memdev is made
// with the usable size from the start.
func TestLocalProbeAliasing(t *testing.T) {
	v := probeEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	res := aliasing(512 * mib)
	v.f.Aliasing = res
	logs := klogtest.Capture(t, 3)
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
	if size := regionSize(t, v, "region0"); size != 512*mib {
		t.Fatalf("region0 of %d bytes", size)
	}
	if p := v.f.Probed(); len(p) != 2 || !strings.HasSuffix(p[0], " 1073741824") || !strings.HasSuffix(p[1], " 536870912") {
		t.Fatalf("probed %v", p)
	}
	wantLog(t, logs(),
		"engine: local memdev 0x11 (mem0): address line probe of region0 (1073741824 bytes): "+res.String()+" in ",
		"engine: local memdev 0x11 (mem0): region0: "+res.String()+"; using only the first 536870912 bytes of the memdev from now on",
		"engine: local memdev 0x11 (mem0): destroyed aliased region0, creating a region of 536870912 bytes",
		"engine: local memdev 0x11 (mem0): creating a region of its first 536870912 bytes only (usable size)",
		"engine: local memdev 0x11 (mem0): address line probe of region0 (536870912 bytes): no aliasing in ")
	path := filepath.Join(v.dir, "localProbes.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(file)); !slices.Equal(keys, []string{"11"}) {
		t.Fatalf("probes file %s", data)
	}
	if fields := slices.Sorted(maps.Keys(file["11"])); !slices.Equal(fields, []string{"region", "result", "time", "usableSize"}) {
		t.Fatalf("probe %s", data)
	}
	if p := v.e.Probes()[serialL]; p.UsableSize != 512*mib || p.Region != "region0" || p.Result != res.String() || p.Time.IsZero() {
		t.Fatalf("probe %+v", p)
	}
	v.mustUnprepare(localDriver, "n")
	v.clean("unprepared")

	logs = klogtest.Capture(t, 3)
	v.restart()
	if p := v.e.Probes()[serialL]; p.UsableSize != 512*mib {
		t.Fatalf("loaded probes %+v", v.e.Probes())
	}
	wantLog(t, logs(), "engine: local memdev 0x11: using only its first 536870912 bytes, found by the address line probe of region0 at ",
		": "+res.String()+" (remove "+path+" to probe all of it again)")
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
	if size := regionSize(t, v, "region0"); size != 512*mib {
		t.Fatalf("region0 of %d bytes", size)
	}
	if p := v.f.Probed(); len(p) != 3 {
		t.Fatalf("probed %v", p)
	}
	v.mustUnprepare(localDriver, "n")
	v.clean("unprepared")
}

// The usable size of a probe is its offset rounded down to the alignment of
// a region, 256 MiB.
func TestLocalProbeRoundsDown(t *testing.T) {
	v := probeEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.f.Aliasing = aliasing(640 * mib)
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
	if size := regionSize(t, v, "region0"); size != 512*mib || v.e.Probes()[serialL].UsableSize != 512*mib {
		t.Fatalf("region0 of %d bytes, probes %+v", size, v.e.Probes())
	}
	v.mustUnprepare(localDriver, "n")
	v.clean("unprepared")
}

// A region made again with the usable size that aliases again fails the
// grant ("still aliases"); the rollback releases it.
func TestLocalProbeStillAliases(t *testing.T) {
	v := probeEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.f.Aliasing = aliasing(512 * mib)
	v.f.After("cxl destroy-region", func() { v.f.Aliasing = aliasing(256 * mib) })
	err := v.prepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
	if err == nil || !strings.Contains(err.Error(), "memdev 0x11 (mem0): region0 of 268435456 bytes still aliases") {
		t.Fatalf("error %v", err)
	}
	v.check("rolled back")
	v.clean("rolled back")
	if p := v.e.Probes()[serialL]; p.UsableSize != 256*mib {
		t.Fatalf("probes %+v", v.e.Probes())
	}
}

// A region whose memory aliases below a newer region of another memdev is
// not made again (a hole): the grant fails, the region is KEPT for the
// claim; a Prepare again sets it up and probes it again, and once nothing
// lies above it, makes it again smaller.
func TestLocalProbeAliasedBelow(t *testing.T) {
	v := probeEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib}, fake.MemdevSpec{Serial: serialB})
	regionAboveNext(t, v, "mem1")
	v.f.Aliasing = aliasing(512 * mib)
	req := sliceReq("n", 0, state.ModeDax, 256*mib)
	below := "memdev 0x11 (mem0): region0 aliases: it is created again with its first 536870912 bytes only once no region of another memory device lies above it in the decoders"
	err := v.prepare(localDriver, "n", req)
	var ab *aliasedBelowError
	if !errors.As(err, &ab) || err.Error() != below {
		t.Fatalf("error %v", err)
	}
	v.check("aliased below")
	wantRegion(t, v, "region0", state.RegionKept)
	if g := v.grant("n", 0); g.State != state.GrantJournaled || g.Intent.Region != "region0" {
		t.Fatalf("grant %+v", g)
	}
	wantPreparation(t, v, localDriver, "n", state.PreparationKept)
	if p := v.e.Probes()[serialL]; p.UsableSize != 512*mib {
		t.Fatalf("probes %+v", v.e.Probes())
	}
	// set up again, probed again: still below
	v.f.Aliasing = aliasing(512 * mib)
	if err := v.prepare(localDriver, "n", req); err == nil || err.Error() != below {
		t.Fatalf("error %v", err)
	}
	if p := v.f.Probed(); len(p) != 2 {
		t.Fatalf("probed %v", p)
	}
	wantRegion(t, v, "region0", state.RegionKept)
	dropRaw(t, v, "region1")
	v.f.Aliasing = aliasing(512 * mib)
	v.mustPrepare(localDriver, "n", req)
	if size := regionSize(t, v, "region0"); size != 512*mib {
		t.Fatalf("region0 of %d bytes", size)
	}
	if p := v.f.Probed(); len(p) != 4 {
		t.Fatalf("probed %v", p)
	}
	v.mustUnprepare(localDriver, "n")
	v.clean("unprepared")
}

// A probe that finds memory that does not hold what was written, or less
// than 256 MiB of distinct bytes, fails the grant; the rollback releases
// the region.
func TestLocalProbeErrors(t *testing.T) {
	for _, c := range []struct {
		name string
		res  *kernel.Aliasing
		want string
	}{
		{"garbage", &kernel.Aliasing{Garbage: 0x1000},
			"memdev 0x11 (mem0): region0: no aliasing, but offset 0x1000 did not read back what was written"},
		{"less than 256 MiB", aliasing(128 * mib),
			"memdev 0x11 (mem0): region0: " + aliasing(128*mib).String() + ": less than 256 MiB is usable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := probeEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
			v.f.Aliasing = c.res
			err := v.prepare(localDriver, "n", sliceReq("n", 0, state.ModeDax, 256*mib))
			if err == nil || err.Error() != c.want {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			v.check("rolled back")
			v.clean("rolled back")
			if len(v.e.Probes()) > 0 {
				t.Fatalf("probes %+v", v.e.Probes())
			}
		})
	}
}

// Without Options.ProbeAddressing no region is probed.
func TestLocalProbeOff(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.f.Aliasing = aliasing(512 * mib)
	v.mustPrepare(localDriver, "n", sliceReq("n", 0, state.ModeNUMA, 256*mib))
	if p := v.f.Probed(); len(p) > 0 {
		t.Fatalf("probed %v", p)
	}
	if size := regionSize(t, v, "region0"); size != gib {
		t.Fatalf("region0 of %d bytes", size)
	}
	v.mustUnprepare(localDriver, "n")
	v.clean("unprepared")
}

// A shared local memdev whose region aliases is made again smaller too,
// and zeroed once, in its final region.
func TestLocalProbeShared(t *testing.T) {
	v := newEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
	v.configure(func(o *Options) { sharedOptions(v, o) })
	v.f.Aliasing = aliasing(512 * mib)
	v.mustPrepare(localDriver, "s", sharedReq("s"))
	if size := regionSize(t, v, "region0"); size != 512*mib {
		t.Fatalf("region0 of %d bytes", size)
	}
	if z := v.f.Zeroed(); len(z) != 1 || !strings.Contains(z[0], " 536870912 ") {
		t.Fatalf("zeroed %v", z)
	}
	if g := v.grant("s", 0); g.Got.Size != 512*mib {
		t.Fatalf("grant %+v", g.Got)
	}
	v.mustUnprepare(localDriver, "s")
	releaseIdleShared(v)
	v.clean("released")
}

// The region of a local memdev gets the smaller of its configured usable
// size (Facts.Usable) and the probed one, rounded down to 256 MiB; a usable
// size of all of it makes a region of all of it.
func TestLocalUsableSize(t *testing.T) {
	for _, c := range []struct {
		name           string
		usable, probed uint64
		want           uint64
	}{
		{"configured", 768*mib + 4096, 0, 768 * mib},
		{"probed smaller", 768 * mib, 512 * mib, 512 * mib},
		{"configured smaller", 256 * mib, 512 * mib, 256 * mib},
		{"all of it", gib, 0, gib},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := probeEnv(t, fake.MemdevSpec{Serial: serialL, Ram: gib})
			if c.probed > 0 {
				if err := v.e.saveProbe(serialL, state.Probe{UsableSize: c.probed, Result: "test", Region: "region9"}); err != nil {
					t.Fatal(err)
				}
				v.restart()
			}
			req := sliceReq("n", 0, state.ModeDax, 256*mib)
			req.Facts.UsableSize = c.usable
			v.mustPrepare(localDriver, "n", req)
			if size := regionSize(t, v, "region0"); size != c.want {
				t.Fatalf("region0 of %d bytes", size)
			}
			v.mustUnprepare(localDriver, "n")
			v.clean("unprepared")
		})
	}
}
