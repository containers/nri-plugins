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
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	cdiSpecs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// cutPrepare runs a Prepare of the claim on the plugin that the process
// does not finish: it stops at a cut point of the fake (fake.ErrCut).
func (e *driverEnv) cutPrepare(p *Plugin, claim *resourcev1.ResourceClaim) {
	e.t.Helper()
	defer func() {
		r := recover()
		if err, ok := r.(error); ok && errors.Is(err, fake.ErrCut) {
			return
		}
		if r == nil {
			e.t.Fatal("the Prepare was not cut")
		}
		panic(r)
	}()
	_, _ = p.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
}

// A pool device of volatile memory in mode numa: its region onlined, the
// line of test10, its CDI device; the result of the claim has the pool
// driver's devices only; the Unprepare releases the device.
func TestPoolNUMA(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX))
	logs := klogCapture(t)
	res := e.mustPrepare(e.pool, poolClaim("u1", "pooled0", poolExclusiveData))
	wantResult(t, res, resultDevice("mem", envPool, "pooled0", "cxl-pool.nri.io/device=claim-u1-0"))
	if prepared := regexp.MustCompile(`cxl-pool\.nri\.io: claim .*: prepared pooled0`); !prepared.MatchString(logs()) {
		t.Fatalf("no line %q in\n%s", prepared, logs())
	}
	if !strings.Contains(e.commands(), "cxl create-region -t ram -d decoder0.0 -m mem0") {
		t.Fatalf("commands:\n%s", e.commands())
	}
	if spec := e.cdiSpec(e.pool.Rules().CDISpecName()); spec == nil || spec.Kind != "cxl-pool.nri.io/device" {
		t.Fatalf("CDI spec %+v", spec)
	}
	cd := e.cdiDevice(e.pool, "claim-u1-0")
	want := []string{"CXL_C1EE0001_MODE=numa", "CXL_C1EE0001_NUMA_NODE=2", "CXL_C1EE0001_SERIAL=0xc1ee0001", "CXL_C1EE0001_SHARED=0",
		"CXL_C1EE0001_SIZE=268435456", "CXL_CLAIM_u1_0=1"}
	if cd == nil || !slices.Equal(cd.ContainerEdits.Env, want) || len(cd.ContainerEdits.DeviceNodes) != 0 {
		t.Fatalf("CDI device %+v, expected env %v", cd, want)
	}
	if got := NUMANodes(e.records(), e.pool.Name()); !slices.Equal(got, []int{2}) {
		t.Fatalf("NUMA nodes %v", got)
	}
	e.mustUnprepare(e.pool, "u1")
	if !strings.Contains(e.commands(), "cxl destroy-region region0\ncxl disable-memdev mem0") {
		t.Fatalf("not released:\n%s", e.commands())
	}
	wantLogText(t, logs(), "cxl-pool.nri.io: unprepared claim demo/claim-u1 (u1)")
	e.wantClean()
	// an unknown claim is not an error
	e.mustUnprepare(e.pool, "u1")
}

// Two claims of one unshared pool device in mode numa, an allocation that
// the scheduler does not make: the second gets the region of the first,
// its memory onlined again without a command, on the same node; the last
// Unprepare releases the region.
func TestPoolNUMATwoClaims(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX))
	e.mustPrepare(e.pool, poolClaim("u1", "pooled0", poolExclusiveData))
	e.mark()
	e.mustPrepare(e.pool, poolClaim("u2", "pooled0", poolExclusiveData))
	e.wantNoCommands("the second claim")
	for _, uid := range []string{"u1", "u2"} {
		if d := e.pool.ClaimInfo(uid).Devices[0]; !slices.Equal(d.NUMANodes, []int{2}) || !strings.HasSuffix(d.SysfsPath, "/region0") {
			t.Fatalf("claim %s: device %+v", uid, d)
		}
	}
	e.mustUnprepare(e.pool, "u1")
	e.wantNoCommands("the Unprepare of the first claim")
	e.mustUnprepare(e.pool, "u2")
	if !strings.Contains(e.commands(), "cxl destroy-region region0\ncxl disable-memdev mem0") {
		t.Fatalf("not released:\n%s", e.commands())
	}
	e.wantClean()
}

// A prepared pool claim over restarts: the start writes its CDI device,
// a Prepare again returns its result without a command; a memdev gone
// while the drivers are down keeps its claim until the Unprepare, and its
// hollow region while its memory is online.
func TestPoolRestart(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX))
	claim := poolClaim("u1", "pooled0", poolExclusiveData)
	res := e.mustPrepare(e.pool, claim)
	mustOK(t, os.RemoveAll(e.cdiDir))
	e.restart()
	if e.cdiDevice(e.pool, "claim-u1-0") == nil {
		t.Fatal("no CDI device after the restart")
	}
	e.mark()
	if again := e.mustPrepare(e.pool, claim); !reflect.DeepEqual(again, res) {
		t.Fatalf("result %+v, expected %+v", again, res)
	}
	e.wantNoCommands("a Prepare again")
	e.k.Unplug(serialPoolX)
	e.restart()
	e.wantState(e.pool, "u1", state.PreparationPrepared)
	if !e.pool.Serials()[serialPoolX] {
		t.Fatalf("serials %v", e.pool.Serials())
	}
	e.mustUnprepare(e.pool, "u1")
	r := e.records()
	if len(r.Preparations)+len(r.Grants) > 0 || len(r.Regions) != 1 || r.Regions[0].State != state.RegionHollowOwned {
		t.Fatalf("records left: preparations %+v grants %+v regions %+v", r.Preparations, r.Grants, r.Regions)
	}
}

// A pool device of volatile memory in mode dax, and a shared one used by
// two claims: dax devices as CDI device nodes, read-only by the
// configuration. A claim of either device that does not fit how it is
// prepared is refused.
func TestPoolDaxAndShared(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX), fake.MemdevSpec{Serial: serialPoolS, HostBridge: 24})
	e.mustPrepare(e.pool, poolClaim("x1", "pooled0", poolExclusiveData, cfgDax))
	x := e.devices(e.pool, "x1")[0]
	cd := e.cdiDevice(e.pool, "claim-x1-0")
	if x.Mode != api.ModeDax || x.DaxDevice == "" || len(x.NUMANodes) != 0 || cd == nil ||
		!reflect.DeepEqual(cd.ContainerEdits.DeviceNodes, []*cdiSpecs.DeviceNode{daxDeviceNode(x.DaxDevice, x.DaxMajor, x.DaxMinor, false)}) {
		t.Fatalf("device %+v, CDI device %+v", x, cd)
	}
	e.mustPrepare(e.pool, poolClaim("s1", "shared0", poolSharedData))
	e.mustPrepare(e.pool, poolClaim("s2", "shared0", poolSharedData, cfgDaxRO))
	s1, s2 := e.devices(e.pool, "s1")[0], e.devices(e.pool, "s2")[0]
	if !s1.Shared || s1.Mode != api.ModeDax || s1.DaxDevice != s2.DaxDevice || s1.ReadOnly || !s2.ReadOnly {
		t.Fatalf("shared devices %+v %+v", s1, s2)
	}
	if cd := e.cdiDevice(e.pool, "claim-s2-0"); cd == nil || cd.ContainerEdits.DeviceNodes[0].Permissions != "r" {
		t.Fatalf("read-only CDI device %+v", cd)
	}
	// claims that do not fit how the devices are prepared are refused
	// before the devices are touched
	e.mark()
	wantErrText(t, e.prepare(e.pool, poolClaim("s3", "shared0", poolSharedData, cfgNUMA)).Err,
		"cxl-pool.nri.io: claim demo/claim-s3: device shared0 (memdev 0xc1ae0001): MemoryConfig: device: a shared device is used as dax only")
	notShared := `{"serial":"0xc1ae0001","shared":false,"size":268435456}`
	wantErrText(t, e.prepare(e.pool, poolClaim("s4", "shared0", notShared)).Err,
		"cxl-pool.nri.io: claim demo/claim-s4: device shared0 (memdev 0xc1ae0001): shared=false, but another claim prepared it with shared=true")
	wantErrText(t, e.prepare(e.pool, poolClaim("x2", "pooled0", poolExclusiveData)).Err,
		"cxl-pool.nri.io: claim demo/claim-x2: device pooled0 (memdev 0xc1ee0001): mode numa, but another claim prepared it in mode dax")
	e.wantNoCommands("refused claims")
	for _, uid := range []string{"x1", "s1", "s2"} {
		e.mustUnprepare(e.pool, uid)
	}
	e.wantClean()
}

// Pool devices of persistent memory: an exclusive writer in mode block
// makes the namespace; readers in mode fs share the node mount of the
// access.
func TestPoolPersistent(t *testing.T) {
	e := newDriverEnv(t, nil, poolPmemSpec(serialPoolP))
	e.mustPrepare(e.pool, poolClaim("w1", "llm0", poolWriterData, cfgBlock))
	if !strings.Contains(e.commands(), "ndctl create-namespace") {
		t.Fatalf("no namespace made:\n%s", e.commands())
	}
	w := e.devices(e.pool, "w1")[0]
	cd := e.cdiDevice(e.pool, "claim-w1-0")
	if !w.Persistent || w.Mode != api.ModeBlock || w.BlockDevice == "" || cd == nil ||
		!reflect.DeepEqual(cd.ContainerEdits.DeviceNodes, []*cdiSpecs.DeviceNode{blockDeviceNode(w.BlockDevice, w.BlockMajor, w.BlockMinor, false)}) {
		t.Fatalf("device %+v, CDI device %+v", w, cd)
	}
	hasEnv(t, cd, "CXL_C1AF0001_MODE=block", "CXL_C1AF0001_PATH="+w.BlockDevice, "CXL_C1AF0001_SHARED=1")
	e.mustUnprepare(e.pool, "w1")
	// the writer of another host leaves a filesystem
	e.formatPmemAgain(serialPoolP, "erofs")
	e.mustPrepare(e.pool, poolClaim("r1", "llm0", poolPmemData, cfgFsRO))
	e.mustPrepare(e.pool, poolClaim("r2", "llm0", poolPmemData, cfgFsRO))
	r1, r2 := e.devices(e.pool, "r1")[0], e.devices(e.pool, "r2")[0]
	mnt := filepath.Join(e.poolDir, "mounts", "c1af0001-direct")
	if r1.Mode != api.ModeFS || r1.NodeMount != mnt || r2.NodeMount != mnt || r1.FSType != "erofs" || r1.Access != api.AccessDirect ||
		r1.Path != "/run/cxl-fs/C1AF0001" || !r1.ReadOnly {
		t.Fatalf("devices %+v %+v", r1, r2)
	}
	cd = e.cdiDevice(e.pool, "claim-r1-0")
	if want := []*cdiSpecs.Mount{fsCDIMount(mnt, "/run/cxl-fs/C1AF0001", true)}; cd == nil || !reflect.DeepEqual(cd.ContainerEdits.Mounts, want) {
		t.Fatalf("CDI device %+v", cd)
	}
	e.mustUnprepare(e.pool, "r1")
	if mi, _ := e.k.Kernel().Mounted(mnt); mi == nil {
		t.Fatal("the node mount went with a claim of it left")
	}
	e.mustUnprepare(e.pool, "r2")
	if mi, _ := e.k.Kernel().Mounted(mnt); mi != nil {
		t.Fatalf("node mount left %+v", mi)
	}
	e.wantClean()
}

// formatPmemAgain writes a filesystem of the type on the fsdax namespace of
// the persistent memdev, which has no region of the drivers, as a writer
// on another host does.
func (e *driverEnv) formatPmemAgain(serial uint64, typ string) {
	e.t.Helper()
	k := e.k.Kernel()
	ctx := context.Background()
	memdev := e.k.Memdev(serial)
	// the release of the driver disabled it
	mustOK(e.t, k.EnableMemdev(ctx, memdev))
	d, err := k.RootDecoder(memdev, kernel.RegionPmem)
	mustOK(e.t, err)
	region, err := k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: memdev, Decoder: d, Type: kernel.RegionPmem})
	mustOK(e.t, err)
	nd, err := k.WaitNdRegion(ctx, region)
	mustOK(e.t, err)
	nss, err := k.Namespaces(ctx, nd)
	mustOK(e.t, err)
	ns := kernel.FsdaxNamespace(nss)
	if ns == nil {
		e.t.Fatalf("%s has no fsdax namespace", memdev)
	}
	e.k.SetImage(ns.BlockDevice, fake.FSImage(typ))
	mustOK(e.t, k.DestroyRegion(ctx, region))
	e.mark()
}

// A rollback that cannot release everything keeps the claim's record
// (KEPT) with the grants left and says so in the pool driver's words; its
// Unprepare releases them.
func TestPoolRollbackKept(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX), poolSpec(0xc1ee0002))
	k := e.k.Kernel()
	foreign := e.k.Memdev(0xc1ee0002)
	d, err := k.RootDecoder(foreign, kernel.RegionRAM)
	mustOK(t, err)
	region, err := k.CreateRegion(context.Background(), kernel.CreateRegionRequest{Memdev: foreign, Decoder: d, Type: kernel.RegionRAM})
	mustOK(t, err)
	e.mark()
	// the second device has a region that the driver did not make; the
	// rollback of the first one cannot disable its memdev
	e.k.FailCommand("cxl disable-memdev", syscall.EBUSY)
	claim := addPoolResult(poolClaim("u1", "pooled0", poolExclusiveData), "mem2", "pooled1",
		`{"serial":"0xc1ee0002","shared":false,"size":268435456}`)
	logs := klogCapture(t)
	wantErrText(t, e.prepare(e.pool, claim).Err, "that the driver did not create")
	wantLogText(t, logs(), "cxl-pool.nri.io: claim demo/claim-u1 (u1): rollback: not everything was released: "+
		"keeping its record for its Unprepare or its next Prepare")
	prep := e.preparation(e.pool, "u1")
	if prep == nil || prep.State != state.PreparationKept || len(prep.Results) != 1 || prep.Results[0].Index != 0 {
		t.Fatalf("preparation %+v", prep)
	}
	if devs := e.devices(e.pool, "u1"); len(devs) != 1 || devs[0].Serial != serialPoolX {
		t.Fatalf("devices %+v", devs)
	}
	if _, _, ok := e.pool.PreparedResult("u1", 0); ok || e.pool.ClaimInfo("u1") != nil {
		t.Fatal("results of a KEPT claim")
	}
	// the foreign region goes; the Unprepare forgets its record
	mustOK(t, k.DestroyRegion(context.Background(), region))
	e.mustUnprepare(e.pool, "u1")
	e.wantClean()
}

// The scheduler holds a pool device for a claim that waits for its attach
// (the pool controller is down, no status data): its Prepare is refused
// without touching anything, its pod is deleted (the Unprepare of a claim
// without a record is nil), and another claim on the same device is then
// prepared once it is attached.
func TestPoolSchedulerHold(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX))
	logs := klogCapture(t)
	xnuma := poolClaim("xnuma", "pooled0", "")
	res := e.prepare(e.pool, xnuma)
	wantErrText(t, res.Err, "cxl-pool.nri.io: claim demo/claim-xnuma: ")
	e.wantNoCommands("a Prepare without status data")
	e.wantState(e.pool, "xnuma", state.PreparationNone)
	if s := e.pool.Serials(); len(s) != 0 {
		t.Fatalf("serials %v", s)
	}
	mustOK(t, e.unprepare(e.pool, "xnuma"))
	wantLogText(t, logs(), "cxl-pool.nri.io: claim xnuma is not prepared, nothing to release")
	if strings.Contains(logs(), "cxl-pool.nri.io: unprepared claim") {
		t.Fatalf("an Unprepare without a record logged as unprepared:\n%s", logs())
	}
	e.wantNoCommands("an Unprepare without a record")
	xdax := poolClaim("xdax", "pooled0", "", cfgDax)
	wantErrText(t, e.prepare(e.pool, xdax).Err, "cxl-pool.nri.io: claim demo/claim-xdax: ")
	e.wantNoCommands("a Prepare without status data")
	e.mustPrepare(e.pool, setPoolData(xdax, "pooled0", poolExclusiveData))
	if d := e.devices(e.pool, "xdax"); len(d) != 1 || d[0].Mode != api.ModeDax {
		t.Fatalf("devices %+v", d)
	}
	e.mustUnprepare(e.pool, "xdax")
	e.wantClean()
}

// A Prepare whose context ends after cxl create-region and whose CDI spec
// cannot be written fails, and its rollback still releases everything: the
// cleanup is not bound to the context of the request.
func TestPoolRollbackOutlivesContext(t *testing.T) {
	e := newDriverEnv(t, nil, poolSpec(serialPoolX))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.k.After("cxl create-region", func() {
		cancel()
		mustOK(t, os.RemoveAll(e.cdiDir))
		mustOK(t, os.WriteFile(e.cdiDir, nil, 0o644))
	})
	claim := poolClaim("u1", "pooled0", poolExclusiveData)
	res, err := e.pool.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{claim})
	mustOK(t, err)
	wantErrText(t, res[claim.UID].Err, "write CDI spec")
	mustOK(t, os.Remove(e.cdiDir))
	mustOK(t, os.MkdirAll(e.cdiDir, 0o755))
	e.check("rolled back")
	e.wantState(e.pool, "u1", state.PreparationNone)
	e.wantClean()
}
