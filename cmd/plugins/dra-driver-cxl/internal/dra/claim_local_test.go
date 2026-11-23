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
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	cdiSpecs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// localDev is the device name of the unshared local memdev.
var localDev = MemdevDeviceName(serialLocal)

// wantResult fails the test unless a PrepareResult has the devices of the
// requests, pools and devices with the CDI ids, in order.
func wantResult(t *testing.T, res kubeletplugin.PrepareResult, want ...kubeletplugin.Device) {
	t.Helper()
	if res.Err != nil || !reflect.DeepEqual(res.Devices, want) {
		t.Fatalf("result %+v (%v), expected %+v", res.Devices, res.Err, want)
	}
}

// resultDevice returns a device of a PrepareResult.
func resultDevice(request, pool, dev string, cdi ...string) kubeletplugin.Device {
	return kubeletplugin.Device{Requests: []string{request}, PoolName: pool, DeviceName: dev, CDIDeviceIDs: cdi}
}

// A slice of mode numa of an unshared local memdev: onlined on the node of
// its region, its CDI device and env, its device info; the Unprepare
// offlines and releases it.
func TestLocalSliceNUMA(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	logs := klogCapture(t)
	res := e.mustPrepare(e.local, localClaim("u1", localDev, 64*mib))
	wantResult(t, res, resultDevice("mem", envNode, localDev, "cxl.nri.io/device=claim-u1-0"))
	e.wantState(e.local, "u1", state.PreparationPrepared)
	g := e.records().Grant(state.GrantRef{Claim: "u1"})
	node := state.NUMANodeOf(g.Got.NUMANode)
	if g.State != state.GrantRealised || node < 0 || g.Got.Size != uint64(envBlockSize) {
		t.Fatalf("grant %+v", g)
	}
	wantLogText(t, logs(), "cxl.nri.io: claim demo/claim-u1: prepared "+localDev+
		" (memdev 0xc100e2e1, share false, mode numa, read-only false) as mem0 region0 dax "+g.Got.Dax+" size 134217728 NUMA node "+strconv.Itoa(node))
	info := e.local.ClaimInfo("u1")
	want := state.DeviceInfo{RequestName: "mem", DeviceName: localDev, DeviceType: device.DeviceTypeMemdev, Mode: api.ModeNUMA,
		SysfsPath: e.k.P("bus/cxl/devices/region0"), NUMANodes: []int{node}, NUMAAffinities: []int{0},
		TotalBytes: uint64(envBlockSize), ConsumedBytes: envBlockSize, Serial: serialLocal}
	if info == nil || info.ClaimName != envNS+"/claim-u1" || len(info.Devices) != 1 ||
		!reflect.DeepEqual(info.Devices[0], want) {
		t.Fatalf("claim info %+v, expected device %+v", info, want)
	}
	cd := e.cdiDevice(e.local, "claim-u1-0")
	wantEnv := []string{"CXL_C100E2E1_MODE=numa", "CXL_C100E2E1_NUMA_NODE=" + strconv.Itoa(node), "CXL_C100E2E1_SERIAL=0xc100e2e1",
		"CXL_C100E2E1_SHARED=0", "CXL_C100E2E1_SIZE=134217728", "CXL_CLAIM_u1_0=1"}
	if cd == nil || !slices.Equal(cd.ContainerEdits.Env, wantEnv) || len(cd.ContainerEdits.DeviceNodes) != 0 {
		t.Fatalf("CDI device %+v, expected env %v", cd, wantEnv)
	}
	if got := NUMANodes(e.records(), device.DriverName); !slices.Equal(got, []int{node}) {
		t.Fatalf("NUMA nodes %v", got)
	}
	e.mustUnprepare(e.local, "u1")
	if !strings.Contains(e.commands(), "cxl destroy-region region0") {
		t.Fatalf("the region is not released:\n%s", e.commands())
	}
	e.wantClean()
}

// A slice of mode dax of an unshared local memdev: a dax device of the
// region, read-only or not, as a CDI device node.
func TestLocalSliceDax(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	e.mustPrepare(e.local, localClaim("u1", localDev, envBlockSize, cfgDaxRO))
	info := e.local.ClaimInfo("u1").Devices[0]
	if info.Mode != api.ModeDax || info.DaxDevice == "" || !info.ReadOnly || info.TotalBytes != uint64(envBlockSize) || len(info.NUMANodes) != 0 {
		t.Fatalf("device info %+v", info)
	}
	cd := e.cdiDevice(e.local, "claim-u1-0")
	wantNodes := []*cdiSpecs.DeviceNode{{Path: info.DaxDevice, Type: "c", Major: int64(info.DaxMajor), Minor: int64(info.DaxMinor), Permissions: "r"}}
	wantEnv := []string{"CXL_C100E2E1_DAX=" + info.DaxDevice, "CXL_C100E2E1_MODE=dax", "CXL_C100E2E1_READ_ONLY=1",
		"CXL_C100E2E1_SERIAL=0xc100e2e1", "CXL_C100E2E1_SHARED=0", "CXL_C100E2E1_SIZE=134217728", "CXL_CLAIM_u1_0=1"}
	if cd == nil || !reflect.DeepEqual(cd.ContainerEdits.DeviceNodes, wantNodes) || !slices.Equal(cd.ContainerEdits.Env, wantEnv) {
		t.Fatalf("CDI device %+v, expected nodes %+v env %v", cd, wantNodes, wantEnv)
	}
	if got := NUMANodes(e.records(), device.DriverName); len(got) != 0 {
		t.Fatalf("NUMA nodes of a dax slice %v", got)
	}
	e.mustUnprepare(e.local, "u1")
	e.wantClean()
}

// A shared local memdev: every claim gets the whole dax device of its
// region; the log lines of the Prepare in the words of the e2e tests and
// of the documentation; the idle device keeps its region.
func TestLocalSharedDax(t *testing.T) {
	// the shared memdev is mem1, as in the documented line
	e := newDriverEnv(t, []fake.MemdevSpec{{Serial: 0xc100e2e0}, sharedOne()})
	e.setShared(serialLocal)
	logs := klogCapture(t)
	e.mustPrepare(e.local, localClaim("w1", localDev, -device.SharesUnlimited, cfgDax))
	e.mustPrepare(e.local, localClaim("r1", localDev, -1, cfgDaxRO))
	log := logs()
	shared := "(memdev 0xc100e2e1, share true, mode dax, read-only %v) as mem1 region0 dax dax0.0 size 1073741824 NUMA node -1 %s"
	wantLogLine(t, log, "cxl.nri.io: claim demo/claim-w1: prepared "+localDev+" "+fmt.Sprintf(shared, false, "exclusive"))
	lines := logLines(log, " 1 share")
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "cxl.nri.io: claim demo/claim-r1: prepared "+localDev+" "+fmt.Sprintf(shared, true, "1 share")) {
		t.Fatalf("shared lines %q", lines)
	}
	w, r := e.local.ClaimInfo("w1").Devices[0], e.local.ClaimInfo("r1").Devices[0]
	if !w.Shared || w.ReadOnly || !r.Shared || !r.ReadOnly || r.DaxDevice != w.DaxDevice || w.DaxDevice == "" || w.TotalBytes != 1<<30 {
		t.Fatalf("device infos %+v %+v", w, r)
	}
	if !e.isShared(serialLocal) {
		t.Fatal("OnShared was not told the serial")
	}
	e.mustUnprepare(e.local, "w1")
	e.mustUnprepare(e.local, "r1")
	if len(e.k.Kernel().Regions(e.k.Memdev(serialLocal))) != 1 {
		t.Fatal("the idle shared device lost its region")
	}
}

// sharedOne returns the spec of the memdev serialLocal of 1 GiB.
func sharedOne() fake.MemdevSpec { return fake.MemdevSpec{Serial: serialLocal, Ram: 1 << 30} }

// pmemDev is the device name of the persistent local memdev.
var pmemDev = MemdevDeviceName(serialPmem)

// hasEnv fails the test unless the env of a CDI device has the variables.
func hasEnv(t *testing.T, cd *cdiSpecs.Device, vars ...string) {
	t.Helper()
	if cd == nil {
		t.Fatal("no CDI device")
	}
	for _, v := range vars {
		if !slices.Contains(cd.ContainerEdits.Env, v) {
			t.Fatalf("CDI device %s: env %v lacks %s", cd.Name, cd.ContainerEdits.Env, v)
		}
	}
}

// A persistent local memdev in mode block: readers before a writer are
// refused without a namespace or a region, the exclusive writer makes the
// fsdax namespace, a reader gets the block device read-only; both as CDI
// device nodes.
func TestLocalPersistentBlock(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{pmemSpec()})
	e.mark()
	for _, cfg := range []string{cfgFsRO, cfgBlockRO} {
		wantErrText(t, e.prepare(e.local, localClaim("r0", pmemDev, -1, cfg)).Err,
			"cxl.nri.io: claim demo/claim-r0: device memdev-c100f5e1 (memdev 0xc100f5e1): memdev 0xc100f5e1: no fsdax namespace in region0: has a writer written the device?")
		e.wantState(e.local, "r0", state.PreparationNone)
	}
	if strings.Contains(e.commands(), "ndctl") || len(e.k.Kernel().Regions("mem0")) != 0 {
		t.Fatalf("a reader made a namespace or left a region:\n%s", e.commands())
	}
	e.mustPrepare(e.local, localClaim("w1", pmemDev, -device.SharesUnlimited, cfgBlock))
	if !strings.Contains(e.commands(), "ndctl create-namespace") {
		t.Fatalf("no namespace made:\n%s", e.commands())
	}
	w := e.local.ClaimInfo("w1").Devices[0]
	if !w.Persistent || w.Mode != api.ModeBlock || w.BlockDevice == "" || w.ReadOnly {
		t.Fatalf("device info %+v", w)
	}
	cd := e.cdiDevice(e.local, "claim-w1-0")
	hasEnv(t, cd, "CXL_C100F5E1_MODE=block", "CXL_C100F5E1_PATH="+w.BlockDevice, "CXL_C100F5E1_READ_ONLY=0", "CXL_CLAIM_w1_0=1")
	if want := []*cdiSpecs.DeviceNode{blockDeviceNode(w.BlockDevice, w.BlockMajor, w.BlockMinor, false)}; !reflect.DeepEqual(cd.ContainerEdits.DeviceNodes, want) {
		t.Fatalf("device nodes %+v", cd.ContainerEdits.DeviceNodes)
	}
	e.mustUnprepare(e.local, "w1")
	e.mustPrepare(e.local, localClaim("r1", pmemDev, -1, cfgBlockRO))
	r := e.local.ClaimInfo("r1").Devices[0]
	cd = e.cdiDevice(e.local, "claim-r1-0")
	hasEnv(t, cd, "CXL_C100F5E1_MODE=block", "CXL_C100F5E1_READ_ONLY=1")
	if !r.ReadOnly || len(cd.ContainerEdits.DeviceNodes) != 1 || cd.ContainerEdits.DeviceNodes[0].Permissions != "r" {
		t.Fatalf("reader %+v, CDI device %+v", r, cd.ContainerEdits)
	}
	e.mustUnprepare(e.local, "r1")
	if spec := e.cdiSpec(device.ClaimsCDISpecName); spec != nil {
		t.Fatalf("CDI spec left %+v", spec)
	}
}

// A persistent local memdev in mode fs: a writer mounts the filesystem
// read-write, a reader shares the node mount; their CDI mounts, the log
// line of the documentation.
func TestLocalPersistentFs(t *testing.T) {
	// the persistent memdev is mem1 with region1, as in the documented line
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec(), pmemSpec()})
	e.formatPmem(serialPmem, "ext4")
	e.mustPrepare(e.local, localClaim("u0", localDev, envBlockSize))
	logs := klogCapture(t)
	e.mustPrepare(e.local, localClaim("w1", pmemDev, -device.SharesUnlimited, cfgFsRW))
	e.mustPrepare(e.local, localClaim("r1", pmemDev, -1, cfgFsRO))
	w, r := e.local.ClaimInfo("w1").Devices[0], e.local.ClaimInfo("r1").Devices[0]
	if !w.Persistent || w.Mode != api.ModeFS || w.NodeMount == "" || w.FSType != "ext4" || w.Access != api.AccessDirect ||
		w.Path != "/run/cxl-fs/C100F5E1" || w.ReadOnly || r.NodeMount != w.NodeMount || !r.ReadOnly {
		t.Fatalf("device infos %+v %+v", w, r)
	}
	wantLogLine(t, logs(), "cxl.nri.io: claim demo/claim-w1: prepared "+pmemDev+" (memdev 0xc100f5e1, share false, mode fs, read-only false)"+
		" as mem1 region1 namespace namespace1.0 block "+w.BlockDevice+` access "direct" fs "ext4" mount "`+w.NodeMount+`" loop ""`)
	for _, c := range []struct {
		uid      string
		readOnly bool
	}{{"w1", false}, {"r1", true}} {
		cd := e.cdiDevice(e.local, "claim-"+c.uid+"-0")
		hasEnv(t, cd, "CXL_C100F5E1_MODE=fs", "CXL_C100F5E1_FS_TYPE=ext4", "CXL_C100F5E1_ACCESS=direct", "CXL_C100F5E1_PATH=/run/cxl-fs/C100F5E1")
		if want := []*cdiSpecs.Mount{fsCDIMount(w.NodeMount, w.Path, c.readOnly)}; !reflect.DeepEqual(cd.ContainerEdits.Mounts, want) {
			t.Fatalf("%s: mounts %+v, expected %+v", c.uid, cd.ContainerEdits.Mounts, want)
		}
	}
	e.mustUnprepare(e.local, "r1")
	e.mustUnprepare(e.local, "w1")
	e.mustUnprepare(e.local, "u0")
	if mi, err := e.k.Kernel().Mounted(w.NodeMount); err != nil || mi != nil {
		t.Fatalf("node mount left: %+v %v", mi, err)
	}
}

// A region that existed before the driver: no kernel command, its NUMA
// node in the device info and the env.
func TestLocalRegion(t *testing.T) {
	e := newDriverEnv(t, nil)
	dev := e.addRegion("region5", 4, 1<<30)
	e.mark()
	res := e.mustPrepare(e.local, localClaim("u1", dev, 64*mib))
	wantResult(t, res, resultDevice("mem", envNode, dev, "cxl.nri.io/device=claim-u1-0"))
	e.wantNoCommands("a region result")
	want := state.DeviceInfo{RequestName: "mem", DeviceName: dev, DeviceType: device.DeviceTypeRegion, Mode: api.ModeNUMA,
		SysfsPath: e.k.P("bus/cxl/devices/region5"), NUMANodes: []int{4}, NUMAAffinities: []int{1}, TotalBytes: 1 << 30,
		ConsumedBytes: 64 * mib, Region: "region5"}
	if info := e.local.ClaimInfo("u1"); !reflect.DeepEqual(info.Devices, []state.DeviceInfo{want}) {
		t.Fatalf("device info %+v, expected %+v", info.Devices, want)
	}
	cd := e.cdiDevice(e.local, "claim-u1-0")
	if want := []string{"CXL_CLAIM_u1_0=1", "CXL_REGION5_MODE=numa", "CXL_REGION5_NUMA_NODE=4", "CXL_REGION5_SIZE=67108864"}; !slices.Equal(cd.ContainerEdits.Env, want) {
		t.Fatalf("env %v", cd.ContainerEdits.Env)
	}
	e.mustUnprepare(e.local, "u1")
	e.wantNoCommands("a region result")
	e.wantClean()
}

// The DRAM of the node: its NUMA nodes and the consumed size in the env.
func TestLocalDRAM(t *testing.T) {
	e := newDriverEnv(t, nil)
	e.mustPrepare(e.local, localClaim("u1", device.DRAMDeviceName, 64*mib, cfgNUMA))
	e.wantNoCommands("a DRAM result")
	d := e.local.ClaimInfo("u1").Devices[0]
	if d.DeviceType != device.DeviceTypeDRAM || d.Mode != api.ModeNUMA || !slices.Equal(d.NUMANodes, []int{0, 1}) ||
		d.TotalBytes != 16<<30 || d.ConsumedBytes != 64*mib {
		t.Fatalf("device info %+v", d)
	}
	cd := e.cdiDevice(e.local, "claim-u1-0")
	if want := []string{"CXL_CLAIM_u1_0=1", "CXL_DRAM_MODE=numa", "CXL_DRAM_NUMA_NODES=0-1", "CXL_DRAM_SIZE=67108864"}; !slices.Equal(cd.ContainerEdits.Env, want) {
		t.Fatalf("env %v", cd.ContainerEdits.Env)
	}
	// a device section that is not numa
	res := e.prepare(e.local, localClaim("u2", device.DRAMDeviceName, 64*mib, cfgDax))
	wantErrText(t, res.Err, "claim demo/claim-u2: request mem: MemoryConfig: device: a dram is used as numa only")
	e.mustUnprepare(e.local, "u1")
	e.wantClean()
}

// The lease device: its lease claim opened at the Prepare (refused
// without the broker), its CDI devices claim-<uid>-<n> and lease-<uid> in
// the spec of the driver, the lease claim closed first at the Unprepare
// and its devices removed.
func TestLocalLeaseDevice(t *testing.T) {
	e := newDriverEnv(t, nil)
	res := e.prepare(e.local, localClaim("u1", device.LeaseDeviceName, 0))
	wantErrText(t, res.Err, "claim demo/claim-u1: lending to running containers is disabled (--leases=false)")
	e.wantState(e.local, "u1", state.PreparationNone)
	e.setLeases()
	res = e.mustPrepare(e.local, localClaim("u1", device.LeaseDeviceName, 0))
	wantResult(t, res, resultDevice("mem", envNode, device.LeaseDeviceName, "cxl.nri.io/device=claim-u1-0", "cxl.nri.io/device=lease-u1"))
	e.wantEvents("prepare", "open u1")
	if d := e.local.ClaimInfo("u1").Devices[0]; d.DeviceType != device.DeviceTypeLease || d.Mode != "" {
		t.Fatalf("device info %+v", d)
	}
	if cd := e.cdiDevice(e.local, "claim-u1-0"); cd == nil || !slices.Equal(cd.ContainerEdits.Env, []string{"CXL_CLAIM_u1_0=1"}) {
		t.Fatalf("CDI device %+v", cd)
	}
	if cd := e.cdiDevice(e.local, "lease-u1"); cd == nil || !slices.Equal(cd.ContainerEdits.Env, []string{"LEASE_u1=1"}) {
		t.Fatalf("CDI device %+v", cd)
	}
	// a socket that does not open: rolled back
	e.leases.setErrors(errFake, nil)
	res = e.prepare(e.local, localClaim("u2", device.LeaseDeviceName, 0))
	wantErrText(t, res.Err, "claim demo/claim-u2: lease socket: "+errFake.Error())
	e.wantState(e.local, "u2", state.PreparationNone)
	e.leases.setErrors(nil, nil)
	e.takeEvents()
	e.mustUnprepare(e.local, "u1")
	e.wantEvents("unprepare", "close u1 (PREPARED)")
	if names := e.cdiNames(e.local); len(names) != 0 {
		t.Fatalf("CDI devices %v", names)
	}
	// a lease claim that does not close: kubelet retries, nothing else
	e.mustPrepare(e.local, localClaim("u3", device.LeaseDeviceName, 0))
	e.leases.setErrors(nil, errFake)
	if err := e.unprepare(e.local, "u3"); !errors.Is(err, errFake) {
		t.Fatalf("unprepare: %v", err)
	}
	e.wantState(e.local, "u3", state.PreparationPrepared)
	e.leases.setErrors(nil, nil)
	e.mustUnprepare(e.local, "u3")
	e.wantClean()
}

// The PrepareResult of a claim of every kind of result: a CDI device per
// result of the driver (the lease device's lease-<uid> too), the share id
// of a result; the results of other drivers are not the driver's.
func TestLocalPrepareResult(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	e.setLeases()
	region := e.addRegion("region5", 4, 1<<30)
	c := localClaim("u1", localDev, envBlockSize)
	addLocalResult(c, "region", region, 64*mib)
	c.Status.Allocation.Devices.Results = append(c.Status.Allocation.Devices.Results,
		poolClaim("x", "pooled0", "").Status.Allocation.Devices.Results[0])
	addLocalResult(c, "dram", device.DRAMDeviceName, 64*mib)
	addLocalResult(c, "lease", device.LeaseDeviceName, 0)
	share := types.UID("share-1")
	c.Status.Allocation.Devices.Results[1].ShareID = &share
	res := e.mustPrepare(e.local, c)
	region5 := resultDevice("region", envNode, region, "cxl.nri.io/device=claim-u1-1")
	region5.ShareID = &share
	wantResult(t, res,
		resultDevice("mem", envNode, localDev, "cxl.nri.io/device=claim-u1-0"),
		region5,
		resultDevice("dram", envNode, device.DRAMDeviceName, "cxl.nri.io/device=claim-u1-3"),
		resultDevice("lease", envNode, device.LeaseDeviceName, "cxl.nri.io/device=claim-u1-4", "cxl.nri.io/device=lease-u1"))
	if got, ok := e.local.Result("u1"); !ok || !reflect.DeepEqual(got, res) {
		t.Fatalf("Result %+v %v", got, ok)
	}
	if names := e.cdiNames(e.local); !slices.Equal(names, []string{"claim-u1-0", "claim-u1-1", "claim-u1-3", "claim-u1-4", "lease-u1"}) {
		t.Fatalf("CDI devices %v", names)
	}
	// a Prepare again returns the same result, without a command or an
	// open of the lease claim
	e.mark()
	e.takeEvents()
	again := e.mustPrepare(e.local, c)
	if !reflect.DeepEqual(again, res) {
		t.Fatalf("again %+v, expected %+v", again, res)
	}
	// also without its allocation
	bare := c.DeepCopy()
	bare.Status.Allocation = nil
	if again := e.mustPrepare(e.local, bare); !reflect.DeepEqual(again, res) {
		t.Fatalf("again without the allocation %+v", again)
	}
	e.wantNoCommands("a Prepare again")
	e.wantEvents("a Prepare again")
	e.mustUnprepare(e.local, "u1")
	if _, ok := e.local.Result("u1"); ok {
		t.Fatal("a result of an unprepared claim")
	}
	e.wantClean()
}
