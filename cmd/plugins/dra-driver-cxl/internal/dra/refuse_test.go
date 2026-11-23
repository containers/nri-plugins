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

package dra

import (
	"context"
	"os"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The Prepares that are refused before any device is touched, of both
// drivers, in the words of each: no kernel command, no record. set changes
// the node for a case and returns what restores it.
func TestPrepareRefusedBeforeDevices(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec(), pmemSpec()}, poolSpec(serialPoolX))
	noAlloc := func(c *resourcev1.ResourceClaim) *resourcev1.ResourceClaim {
		c.Status.Allocation = nil
		return c
	}
	shares0 := func(c *resourcev1.ResourceClaim) *resourcev1.ResourceClaim {
		c.Status.Allocation.Devices.Results[0].ConsumedCapacity[device.CapacityShares] = resource.MustParse("0")
		return c
	}
	badConfig := `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"mode":"memory"}}`
	region := e.addRegion("region5", 4, 1<<30)
	sizeZero := func() func() {
		devs := e.pub.Devices()
		info := *devs[localDev]
		m := *info.Dev.(*Memdev)
		m.Size = 0
		info.Dev = &m
		e.pub.SetDevices(devicesInfo{localDev: &info})
		return func() { e.pub.SetDevices(devs) }
	}
	unmanaged := func() func() {
		e.local.local.Managed = false
		return func() { e.local.local.Managed = true }
	}
	for _, tc := range []struct {
		name  string
		p     *Plugin
		claim *resourcev1.ResourceClaim
		err   string
		set   func() func()
	}{
		{"local no allocation", e.local, noAlloc(localClaim("u1", localDev, 0)), "cxl.nri.io: claim demo/claim-u1: no allocation", nil},
		{"local unknown device", e.local, localClaim("u1", "memdev-77", 0), "could not find allocatable device memdev-77 (pool node1)", nil},
		{"local unknown second device", e.local, addLocalResult(localClaim("u1", localDev, envBlockSize), "other", "memdev-77", 0),
			"could not find allocatable device memdev-77 (pool node1)", nil},
		{"local config", e.local, localClaim("u1", localDev, 0, badConfig),
			`claim demo/claim-u1: request mem: MemoryConfig: device.mode: invalid mode "memory" (numa or dax)`, nil},
		{"local config of another request", e.local,
			addConfig(localClaim("u1", localDev, 0), device.DriverName, []string{"other"}, `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","device":{"online":"all"}}`),
			`cxl.nri.io: claim demo/claim-u1: MemoryConfig: FromClaim config 0: json: unknown field "device.online"`, nil},
		{"local steering", e.local, localClaim("u1", localDev, 0, `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","container":{"steering":{"policy":"off"}}}`),
			`cxl.nri.io: claim demo/claim-u1: request mem: MemoryConfig: container.steering.policy: invalid policy "off"`, nil},
		{"local region of a second result", e.local,
			addConfig(addLocalResult(localClaim("u1", localDev, envBlockSize), "region", region, 64*mib), device.DriverName, []string{"region"}, cfgDax),
			"cxl.nri.io: claim demo/claim-u1: request region: MemoryConfig: device: a region is used as numa only", nil},
		{"local lease device", e.local, localClaim("u1", device.LeaseDeviceName, 0, cfgNUMA),
			"cxl.nri.io: claim demo/claim-u1: request mem: MemoryConfig: device: the lease device has no device settings", nil},
		{"local shares of an unshared memdev", e.local, localClaim("u1", localDev, -1),
			"claim demo/claim-u1: request mem: memdev 0xc100e2e1: allocated as a shared device, which it is not any more", nil},
		{"local persistent writer of a share", e.local, localClaim("u1", pmemDev, -1, cfgBlock),
			"cxl.nri.io: claim demo/claim-u1: request mem: MemoryConfig: device.mode block: the device is writable only exclusively (no shares request)", nil},
		{"local persistent without a mode", e.local, localClaim("u1", pmemDev, -1),
			"cxl.nri.io: claim demo/claim-u1: request mem: MemoryConfig: device: a persistent device needs mode block or fs", nil},
		{"local persistent shares 0", e.local, shares0(localClaim("u1", pmemDev, -1, cfgBlockRO)),
			`cxl.nri.io: claim demo/claim-u1: request mem: memdev 0xc100f5e1: consumed shares "0" is less than 1`, nil},
		{"local persistent allocated as volatile", e.local, localClaim("u1", pmemDev, envBlockSize, cfgBlockRO),
			"cxl.nri.io: claim demo/claim-u1: request mem: memdev 0xc100f5e1: allocated as a device of volatile memory, which it is not any more", nil},
		{"local size 0", e.local, localClaim("u1", localDev, 0), "cxl.nri.io: claim demo/claim-u1: device memdev-c100e2e1: size 0", sizeZero},
		{"local memdevs not managed", e.local, localClaim("u1", localDev, 0),
			"cxl.nri.io: claim demo/claim-u1: device memdev-c100e2e1: local memdevs are not managed (manageMemdevs: false)", unmanaged},
		{"pool no allocation", e.pool, noAlloc(poolClaim("u1", "pooled0", poolExclusiveData)),
			"cxl-pool.nri.io: claim demo/claim-u1: no allocation", nil},
		{"pool no status data", e.pool, poolClaim("u1", "pooled0", ""), "cxl-pool.nri.io: claim demo/claim-u1: ", nil},
		{"pool shares 0", e.pool, shares0(poolClaim("u1", "shared0", poolSharedData)),
			`cxl-pool.nri.io: claim demo/claim-u1: memdev 0xc1ae0001: consumed shares "0" is less than 1: request shares: 1 (a share) or no shares (exclusive)`, nil},
		{"pool config", e.pool, poolClaim("u1", "pooled0", poolExclusiveData, badConfig),
			`cxl-pool.nri.io: claim demo/claim-u1: device pooled0 (memdev 0xc1ee0001): MemoryConfig: device.mode: invalid mode "memory"`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.t = t
			if tc.set != nil {
				defer tc.set()()
			}
			e.mark()
			wantErrText(t, e.prepare(tc.p, tc.claim).Err, tc.err)
			e.wantNoCommands("refused")
			e.wantState(tc.p, "u1", state.PreparationNone)
		})
	}
}

// A claim whose Prepare a stop cut below a newer region of another memdev
// is KEPT at the start, with its record, the region of its grant and its
// memdev; a refused Prepare again keeps them, its Prepare again with the
// status data reuses the region.
func TestPrepareRefusedKeepsKept(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX), poolSpec(0xc1ee0002))
	// a stop cuts the Prepare after its region, another memdev gets a
	// region above it: the start keeps the claim's region
	e.k.CutAfter("cxl create-region")
	e.cutPrepare(e.pool, poolClaim("u1", "pooled0", poolExclusiveData))
	k := e.k.Kernel()
	other := e.k.Memdev(0xc1ee0002)
	d, err := k.RootDecoder(other, kernel.RegionRAM)
	mustOK(t, err)
	above, err := k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: other, Decoder: d, Type: kernel.RegionRAM})
	mustOK(t, err)
	e.restart()
	e.wantState(e.pool, "u1", state.PreparationKept)
	kept := e.records().Grant(state.GrantRef{Claim: "u1"})
	if kept == nil || kept.Serial != serialPoolX || kept.Intent.Region != "region0" || len(e.k.Kernel().Regions("mem0")) != 1 {
		t.Fatalf("kept grant %+v, regions %v", kept, e.k.Kernel().Regions("mem0"))
	}
	// its device as the claim asks it, before the Prepare made it
	if d := e.devices(e.pool, "u1"); len(d) != 1 || d[0].Serial != serialPoolX || d[0].Mode != "numa" || d[0].SysfsPath != "" {
		t.Fatalf("device of the kept claim %+v", d)
	}
	if !e.pool.Serials()[serialPoolX] {
		t.Fatalf("serials %v", e.pool.Serials())
	}
	e.mark()
	wantErrText(t, e.prepare(e.pool, poolClaim("u1", "pooled0", "")).Err, "cxl-pool.nri.io: claim demo/claim-u1: ")
	e.wantNoCommands("a refused Prepare of a KEPT claim")
	e.wantState(e.pool, "u1", state.PreparationKept)
	e.mustPrepare(e.pool, poolClaim("u1", "pooled0", poolExclusiveData))
	if g := e.records().Grant(state.GrantRef{Claim: "u1"}); g == nil || g.Got.Region != kept.Intent.Region {
		t.Fatalf("the kept region %s is not reused: %+v", kept.Intent.Region, g)
	}
	e.mustUnprepare(e.pool, "u1")
	mustOK(t, k.DestroyRegion(context.Background(), above))
	e.wantClean()
}

// A claim whose Prepare a process stop cut is PREPARING after the start:
// a refused Prepare again keeps it, a Prepare again goes on with it, and
// the Unprepare of another cut one removes it.
func TestPrepareRefusedKeepsPreparing(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX), poolSpec(0xc1ee0002))
	for _, uid := range []string{"u1", "u2"} {
		e.k.CutAfter("cxl create-region")
		e.cutPrepare(e.pool, poolClaim(uid, "pooled0", poolExclusiveData))
		e.restart()
		e.wantState(e.pool, uid, state.PreparationPreparing)
	}
	e.mark()
	wantErrText(t, e.prepare(e.pool, poolClaim("u1", "pooled0", "")).Err, "cxl-pool.nri.io: claim demo/claim-u1: ")
	e.wantNoCommands("a refused Prepare of a PREPARING claim")
	e.wantState(e.pool, "u1", state.PreparationPreparing)
	e.mustUnprepare(e.pool, "u2")
	e.wantState(e.pool, "u2", state.PreparationNone)
	e.mustPrepare(e.pool, poolClaim("u1", "pooled0", poolExclusiveData))
	e.mustUnprepare(e.pool, "u1")
	e.wantClean()
}

// After a reboot the claims of both drivers are PREPARED_STALE, without a
// command at the start: no result for kubelet, no CDI device, no NUMA node
// to fence. A refused Prepare again keeps one, a Prepare again prepares it
// anew; the Unprepare of a pool one releases its memdev by its serial, of
// a local one runs no command; both remove the record.
func TestPrepareRefusedKeepsStale(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, poolSpec(serialPoolX), fake.MemdevSpec{Serial: serialPoolS, HostBridge: 24})
	e.mustPrepare(e.pool, poolClaim("u1", "pooled0", poolExclusiveData))
	e.mustPrepare(e.pool, poolClaim("u2", "shared0", poolSharedData))
	e.mustPrepare(e.local, localClaim("l1", localDev, envBlockSize))
	e.mustPrepare(e.local, localClaim("l2", localDev, envBlockSize))
	drivers := []string{device.DriverName, device.PoolDriverName}
	if len(NUMANodes(e.records(), drivers...)) == 0 {
		t.Fatal("no NUMA node before the reboot")
	}
	logs := klogCapture(t)
	e.reboot()
	e.wantNoCommands("the start after a reboot")
	for _, c := range []struct {
		p   *Plugin
		uid string
	}{{e.pool, "u1"}, {e.pool, "u2"}, {e.local, "l1"}, {e.local, "l2"}} {
		e.wantState(c.p, c.uid, state.PreparationStale)
		if _, ok := c.p.Result(c.uid); ok || c.p.ClaimInfo(c.uid) != nil {
			t.Fatalf("%s: stale claim %s is prepared", c.p.Name(), c.uid)
		}
	}
	if names := append(e.cdiNames(e.pool), e.cdiNames(e.local)...); len(names) != 0 {
		t.Fatalf("CDI devices of stale claims %v", names)
	}
	if got := NUMANodes(e.records(), drivers...); len(got) != 0 {
		t.Fatalf("NUMA nodes of stale claims %v", got)
	}
	wantErrText(t, e.prepare(e.pool, poolClaim("u1", "pooled0", "")).Err, "cxl-pool.nri.io: claim demo/claim-u1: ")
	e.wantNoCommands("a refused Prepare of a stale claim")
	e.wantState(e.pool, "u1", state.PreparationStale)
	e.mustPrepare(e.pool, poolClaim("u1", "pooled0", poolExclusiveData))
	// a stale claim prepared anew while a memdev of this boot has a record
	e.mustPrepare(e.local, localClaim("l1", localDev, envBlockSize))
	if e.cdiDevice(e.pool, "claim-u1-0") == nil || e.cdiDevice(e.local, "claim-l1-0") == nil {
		t.Fatal("no CDI device of a claim prepared anew")
	}
	e.mark()
	e.mustUnprepare(e.pool, "u2")
	if got, want := e.commands(), "cxl disable-memdev "+e.k.Memdev(serialPoolS); got != want {
		t.Fatalf("the Unprepare of a stale pool claim ran\n%s\nexpected\n%s", got, want)
	}
	wantLogText(t, logs(), "cxl-pool.nri.io: unpreparing claim demo/claim-u2 (u2), not prepared any more (prepared in another boot)")
	e.mustUnprepare(e.pool, "u1")
	e.wantState(e.local, "l2", state.PreparationStale)
	e.mark()
	e.mustUnprepare(e.local, "l2")
	e.wantNoCommands("the Unprepare of a stale local claim")
	e.wantState(e.local, "l2", state.PreparationNone)
	e.mustUnprepare(e.local, "l1")
	e.wantClean()
}

// A Prepare that fails after its intent: its record removed when its
// rollback released everything, in the words of each driver.
func TestPrepareFailedAfterIntent(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	// a pool device that does not appear
	res := e.prepare(e.pool, poolClaim("u1", "pooled9", poolExclusiveData))
	wantErrText(t, res.Err, "cxl-pool.nri.io: claim demo/claim-u1: device pooled9 (memdev 0xc1ee0001): ")
	e.wantState(e.pool, "u1", state.PreparationNone)
	// a local memdev with a region that the driver did not make
	k := e.k.Kernel()
	memdev := e.k.Memdev(serialLocal)
	d, err := k.RootDecoder(memdev, kernel.RegionRAM)
	mustOK(t, err)
	region, err := k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: memdev, Decoder: d, Type: kernel.RegionRAM})
	mustOK(t, err)
	res = e.prepare(e.local, localClaim("u2", localDev, envBlockSize))
	wantErrText(t, res.Err, "claim demo/claim-u2: device "+localDev+" (memdev 0xc100e2e1): ")
	wantErrText(t, res.Err, "that the driver did not create")
	e.wantState(e.local, "u2", state.PreparationNone)
	if reg := e.records().Region(state.RegionRef(region)); reg == nil || reg.State != state.RegionForeign {
		t.Fatalf("region record %+v", reg)
	}
	// the Prepare again, the foreign region gone, forgets its record
	mustOK(t, k.DestroyRegion(context.Background(), region))
	if res = e.prepare(e.local, localClaim("u2", localDev, envBlockSize)); res.Err != nil {
		t.Fatal(res.Err)
	}
	e.mustUnprepare(e.local, "u2")
	if spec := e.cdiSpec(device.ClaimsCDISpecName); spec != nil {
		t.Fatalf("CDI spec %+v", spec)
	}
	e.wantClean()
}

// A Prepare whose CDI spec cannot be written is rolled back; the spec of
// the other prepared claim cannot be written either, which is logged.
func TestPrepareCDIFails(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	e.mustPrepare(e.local, localClaim("u0", localDev, envBlockSize))
	mustOK(t, os.RemoveAll(e.cdiDir))
	mustOK(t, os.WriteFile(e.cdiDir, nil, 0o644))
	logs := klogCapture(t)
	res := e.prepare(e.local, localClaim("u1", localDev, envBlockSize))
	wantErrText(t, res.Err, "cxl.nri.io: claim demo/claim-u1: write CDI spec "+device.ClaimsCDISpecName+": ")
	wantLogText(t, logs(), "cxl.nri.io: claim demo/claim-u1: rollback: write CDI spec "+device.ClaimsCDISpecName+": ")
	e.wantState(e.local, "u1", state.PreparationNone)
	mustOK(t, os.Remove(e.cdiDir))
	mustOK(t, os.MkdirAll(e.cdiDir, 0o755))
	e.mustUnprepare(e.local, "u0")
	e.wantClean()
}
