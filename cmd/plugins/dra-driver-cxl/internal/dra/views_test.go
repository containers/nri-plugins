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
	"maps"
	"reflect"
	"slices"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state/statetest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// NUMANodes has the NUMA nodes of the grants of mode numa of the prepared
// claims of the drivers asked for, sorted, without those of dax slices.
func TestNUMANodes(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, fake.MemdevSpec{Serial: serialPoolX, HostBridge: 24})
	e.mustPrepare(e.pool, poolClaim("p1", "pooled0", poolExclusiveData))
	e.mustPrepare(e.local, localClaim("l1", localDev, envBlockSize))
	e.mustPrepare(e.local, localClaim("l2", localDev, envBlockSize))
	e.mustPrepare(e.local, localClaim("d1", localDev, envBlockSize, cfgDax))
	r := e.records()
	for _, tc := range []struct {
		drivers []string
		want    []int
	}{
		{[]string{device.DriverName, device.PoolDriverName}, []int{2, 3}},
		{[]string{device.DriverName}, []int{2}},
		{[]string{device.PoolDriverName}, []int{3}},
		{nil, nil},
	} {
		if got := NUMANodes(r, tc.drivers...); !slices.Equal(got, tc.want) {
			t.Errorf("%v: %v, expected %v", tc.drivers, got, tc.want)
		}
	}
	for _, uid := range []string{"l1", "l2", "d1"} {
		e.mustUnprepare(e.local, uid)
	}
	e.mustUnprepare(e.pool, "p1")
	if got := NUMANodes(e.records(), device.DriverName, device.PoolDriverName); len(got) != 0 {
		t.Fatalf("NUMA nodes %v", got)
	}
}

// The NUMA node of a companion claim whose Prepare a stop cut after its
// memory came online, before its grant had the node: the node of its
// region, from the start; the claim has no results for the NRI plugin.
func TestNUMANodesCut(t *testing.T) {
	e := lendEnv(t)
	e.lending()
	e.k.CutAfter("write devices/system/memory/memory")
	e.cutPrepare(e.pool, e.companionOf(e.pool))
	e.restart()
	e.wantState(e.pool, "c1", state.PreparationPreparing)
	if g := e.records().Grant(state.GrantRef{Claim: "c1"}); g == nil || g.Got.NUMANode != nil {
		t.Fatalf("grant %+v", g)
	}
	if got := NUMANodes(e.records(), e.pool.Name()); !slices.Equal(got, []int{2}) || e.pool.ClaimInfo("c1") != nil {
		t.Fatalf("NUMA nodes %v, claim info %+v", got, e.pool.ClaimInfo("c1"))
	}
	e.mustUnprepare(e.pool, "c1")
	if got := NUMANodes(e.records(), e.pool.Name()); len(got) != 0 {
		t.Fatalf("NUMA nodes %v", got)
	}
	e.wantClean()
}

// Serials has the serials of the grants of the driver's source and of the
// Prepares in progress, until the Prepare ends.
func TestSerials(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec(), sharedSpec()}, poolSpec(serialPoolX))
	e.setShared(serialShared)
	e.mustPrepare(e.pool, poolClaim("p1", "pooled0", poolExclusiveData))
	e.mustPrepare(e.local, localClaim("l1", localDev, envBlockSize))
	if got := e.pool.Serials(); !reflect.DeepEqual(got, map[uint64]bool{serialPoolX: true}) {
		t.Fatalf("serials of pool devices %v", got)
	}
	if got := e.local.Serials(); !reflect.DeepEqual(got, map[uint64]bool{serialLocal: true}) {
		t.Fatalf("local serials %v", got)
	}
	// a Prepare in progress before its grant is recorded: OnShared is told
	// between the intent and the grants
	var inflight []uint64
	e.local.onShared = func(serial uint64) {
		if e.records().Grant(state.GrantRef{Claim: "s1"}) == nil {
			inflight = slices.Sorted(maps.Keys(e.local.Serials()))
		}
	}
	// the shared memdev has a region of another driver: the Prepare fails
	k := e.k.Kernel()
	memdev := e.k.Memdev(serialShared)
	d, err := k.RootDecoder(memdev, kernel.RegionRAM)
	mustOK(t, err)
	region, err := k.CreateRegion(t.Context(), kernel.CreateRegionRequest{Memdev: memdev, Decoder: d, Type: kernel.RegionRAM})
	mustOK(t, err)
	wantErrText(t, e.prepare(e.local, localClaim("s1", MemdevDeviceName(serialShared), -1, cfgDax)).Err, "that the driver did not create")
	if !slices.Equal(inflight, []uint64{serialLocal, serialShared}) {
		t.Fatalf("serials in the Prepare %v", inflight)
	}
	if got := e.local.Serials(); !reflect.DeepEqual(got, map[uint64]bool{serialLocal: true}) {
		t.Fatalf("serials after the Prepare %v", got)
	}
	mustOK(t, k.DestroyRegion(t.Context(), region))
	e.mustUnprepare(e.local, "l1")
	e.mustUnprepare(e.pool, "p1")
	if len(e.local.Serials())+len(e.pool.Serials()) != 0 {
		t.Fatalf("serials %v %v", e.local.Serials(), e.pool.Serials())
	}
	var none *Plugin
	if _, _, ok := none.PreparedResult("x", 0); none.Serials() != nil || none.Name() != "" || none.Lender() != nil || none.ClaimInfo("x") != nil || ok {
		t.Fatal("a nil plugin")
	}
}

// The readers of the prepared claims, of either driver: PreparedResult
// and ClaimInfo of prepared claims only.
func TestClaimReaders(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, poolSpec(serialPoolX))
	e.mustPrepare(e.pool, poolClaim("p1", "pooled0", poolExclusiveData))
	e.mustPrepare(e.local, localClaim("l1", localDev, envBlockSize))
	name, d, ok := e.pool.PreparedResult("p1", 0)
	if !ok || name != "demo/claim-p1" || d.Result != 0 || d.RequestName != "mem" || d.DeviceName != "pooled0" || d.Serial != serialPoolX ||
		d.DeviceType != device.DeviceTypeMemdev || d.Mode != api.ModeNUMA || !slices.Equal(d.NUMANodes, []int{2}) || d.ConsumedBytes != 256<<20 ||
		d.SysfsPath != e.k.P("bus/cxl/devices/region0") || d.NUMAAffinities != nil {
		t.Fatalf("pool result %q %+v %v", name, d, ok)
	}
	name, d, ok = e.local.PreparedResult("l1", 0)
	if !ok || name != "demo/claim-l1" || d.DeviceName != localDev || d.Result != 0 {
		t.Fatalf("prepared result %q %+v %v", name, d, ok)
	}
	if _, _, ok := e.local.PreparedResult("l1", 1); ok {
		t.Fatal("a prepared result of no allocation result")
	}
	for _, uid := range []string{"x", "p1"} {
		if _, _, ok := e.local.PreparedResult(uid, 0); ok {
			t.Fatalf("a prepared result of %s of cxl.nri.io", uid)
		}
	}
	if e.pool.ClaimInfo("p1") == nil || e.local.ClaimInfo("p1") != nil || e.pool.ClaimInfo("x") != nil {
		t.Fatal("the readers of a claim of another driver or of none")
	}
	e.mustUnprepare(e.pool, "p1")
	e.mustUnprepare(e.local, "l1")
	if _, _, ok := e.pool.PreparedResult("p1", 0); ok || e.local.ClaimInfo("l1") != nil {
		t.Fatal("the readers of an unprepared claim")
	}
}

// The container settings of each result, as the MemoryConfig entries of
// the driver for its request resolve them, with their origins, in the
// device of the result that the readers give, after a restart too: the
// entries of a device class come first, null removes a preset, an entry of
// the other driver is not read.
func TestResultSettings(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, poolSpec(serialPoolX))
	const (
		class = resourcev1.AllocationConfigSourceClass
		claim = resourcev1.AllocationConfigSourceClaim
	)
	entry := func(c *resourcev1.ResourceClaim, driver string, source resourcev1.AllocationConfigSource, requests []string, container string) {
		addConfig(c, driver, requests, `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","container":`+container+`}`)
		c.Status.Allocation.Devices.Config[len(c.Status.Allocation.Devices.Config)-1].Source = source
	}
	requests := func(c *resourcev1.ResourceClaim, classes ...string) {
		for i, r := range c.Status.Allocation.Devices.Results {
			c.Spec.Devices.Requests = append(c.Spec.Devices.Requests,
				resourcev1.DeviceRequest{Name: r.Request, Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: classes[i]}})
		}
	}
	l := addLocalResult(localClaim("l1", localDev, envBlockSize), "region", e.addRegion("region5", 4, 1<<30), 64*mib)
	requests(l, "cxl-local", "cxl-region")
	entry(l, device.DriverName, claim, []string{"region"}, `{"steering":{"policy":"first-dram","maxStep":"1Gi"}}`)
	entry(l, device.DriverName, class, nil, `{"steering":{"policy":"auto"}}`)
	entry(l, device.DriverName, claim, []string{"mem"}, `{"steering":{"policy":null}}`)
	p := poolClaim("p1", "pooled0", poolExclusiveData)
	requests(p, "cxl-pool", "cxl-dram")
	entry(p, device.PoolDriverName, claim, []string{"mem"}, `{"steering":{"maxStep":"64Mi"}}`)
	entry(p, device.PoolDriverName, class, nil, `{"steering":{"policy":"auto"}}`)
	entry(p, device.DriverName, claim, nil, `{"steering":{"policy":"none"}}`)
	e.mustPrepare(e.local, l)
	e.mustPrepare(e.pool, p)
	byClaim := func(request string) map[string]string {
		return map[string]string{config.FieldPolicy: "claim demo/claim-l1 request " + request, config.FieldMaxStep: "claim demo/claim-l1 request " + request}
	}
	want := []config.ResultSettings{
		{},
		{Container: &config.Container{Steering: &config.Steering{Policy: config.PolicyFirstDRAM, MaxStep: "1Gi"}}, Origins: byClaim("region")},
		{Container: &config.Container{Steering: &config.Steering{Policy: config.PolicyAuto, MaxStep: "64Mi"}},
			Origins: map[string]string{config.FieldPolicy: "class cxl-pool", config.FieldMaxStep: "claim demo/claim-p1 request mem"}},
	}
	check := func(when string) {
		t.Helper()
		info := e.local.ClaimInfo("l1")
		_, d, ok := e.pool.PreparedResult("p1", 0)
		if info == nil || len(info.Devices) != 2 || !ok {
			t.Fatalf("%s: claim info %+v, pool result %v", when, info, ok)
		}
		for i, got := range []config.ResultSettings{info.Devices[0].ResultSettings, info.Devices[1].ResultSettings, d.ResultSettings} {
			if !reflect.DeepEqual(got, want[i]) {
				t.Errorf("%s: settings %d %+v %v, expected %+v %v", when, i, got.Container, got.Origins, want[i].Container, want[i].Origins)
			}
		}
	}
	check("prepared")
	e.restart()
	check("after a restart")
	e.mustUnprepare(e.local, "l1")
	e.mustUnprepare(e.pool, "p1")
	e.wantClean()
}

// The devices of the results of every kind, derived from the records of
// both drivers with the roots of the paths: a region with its sysfs path
// and CPU NUMA affinity, a shared dax slice with its device node and the
// NUMA affinity of its local memdev, DRAM, the lease device, a persistent
// fs device, a pool device without an affinity, a result whose grant is
// gone, and the lend of a companion claim.
func TestDeviceInfo(t *testing.T) {
	doc := &store.Document{Schema: store.Schema, BootID: statetest.BootID, Records: statetest.Records()}
	if rs := newRoots("", ""); rs != (roots{sys: "/sys", dev: "/dev"}) {
		t.Errorf("default roots %+v", rs)
	}
	rs := newRoots("/host/sys", "/host/dev")
	info := func(driver, uid string) *ClaimInfo {
		t.Helper()
		p := doc.Preparation(driver, uid)
		if p == nil {
			t.Fatalf("no preparation %s", uid)
		}
		return claimInfo(doc, p, rs)
	}
	mixed := info(statetest.LocalDriver, statetest.ClaimMixed)
	if mixed.ClaimName != "demo/c-mixed" || mixed.Lend != nil || len(mixed.Devices) != 3 {
		t.Fatalf("claim %+v", mixed)
	}
	region, shared, dram := mixed.Devices[0], mixed.Devices[1], mixed.Devices[2]
	if region.DeviceType != device.DeviceTypeRegion || region.Mode != api.ModeNUMA || region.Region != "region5" ||
		region.SysfsPath != "/host/sys/bus/cxl/devices/region5" || !slices.Equal(region.NUMANodes, []int{5}) ||
		!slices.Equal(region.NUMAAffinities, []int{1}) || region.TotalBytes != 2<<30 || region.ConsumedBytes != 1<<30 {
		t.Errorf("region %+v", region)
	}
	if shared.Result != 1 || shared.DeviceType != device.DeviceTypeMemdev || shared.Mode != api.ModeDax || shared.Serial != 0xc100e2e2 ||
		shared.DaxDevice != "/host/dev/dax1.0" || shared.DaxMajor != 252 || shared.DaxMinor != 4 || !shared.ReadOnly || !shared.Shared ||
		!slices.Equal(shared.NUMAAffinities, []int{0}) || shared.SysfsPath != "/host/sys/bus/cxl/devices/region1" {
		t.Errorf("shared slice %+v", shared)
	}
	if dram.DeviceType != device.DeviceTypeDRAM || !slices.Equal(dram.NUMANodes, []int{0, 1}) || dram.TotalBytes != 64<<30 {
		t.Errorf("DRAM %+v", dram)
	}
	if d := info(statetest.LocalDriver, statetest.ClaimLease).Devices[0]; d.DeviceType != device.DeviceTypeLease || d.Mode != "" {
		t.Errorf("lease device %+v", d)
	}
	if d := info(statetest.LocalDriver, statetest.ClaimFs).Devices[0]; !d.Persistent || d.Mode != api.ModeFS || d.BlockDevice != "/dev/pmem7" ||
		d.Access != api.AccessDirect || d.NodeMount == "" || d.FSType != "erofs" || !d.ReadOnly {
		t.Errorf("persistent fs device %+v", d)
	}
	if d := info(statetest.PoolDriver, statetest.ClaimPoolNUMA).Devices[0]; d.DeviceType != device.DeviceTypeMemdev || d.Serial != 0xc1ae2003 ||
		!slices.Equal(d.NUMANodes, []int{4}) || d.NUMAAffinities != nil || d.SysfsPath != "/host/sys/bus/cxl/devices/region4" {
		t.Errorf("pool device %+v", d)
	}
	companion := info(statetest.LocalDriver, statetest.ClaimCompanion)
	if l := companion.Lend; l == nil || l.Lease != statetest.LeaseNUMA || l.Container != "main" || !slices.Equal(l.NUMANodes, []int{2}) {
		t.Errorf("lend %+v", companion.Lend)
	}
	doc.Grants = slices.DeleteFunc(doc.Grants, func(g state.Grant) bool { return g.Ref.Claim == statetest.ClaimDax })
	if d := info(statetest.LocalDriver, statetest.ClaimDax).Devices[0]; d.DeviceType != device.DeviceTypeMemdev || d.RequestName != "mem" ||
		d.Serial != 0 || d.Mode != "" || d.DaxDevice != "" {
		t.Errorf("a result whose grant is gone %+v", d)
	}
}
