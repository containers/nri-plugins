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
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiSpecs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// TestClaimsCDISpec checks the CDI spec of the claims of cxl.nri.io built
// from their records: the env of the DRAM and of a region with their
// consumed sizes, the marker only of the lease device and the device of its
// lease claim, nothing of a result of another driver or of a companion
// claim.
func TestClaimsCDISpec(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	e.lending()
	e.setLeases()
	region := e.addRegion("region0", 2, 2<<30)
	// result 1 is of another driver
	u1 := localClaim("u1", device.DRAMDeviceName, 64*mib)
	u1.Status.Allocation.Devices.Results = append(u1.Status.Allocation.Devices.Results,
		poolClaim("x", "pooled0", "").Status.Allocation.Devices.Results[0])
	addLocalResult(u1, "region", region, 64*mib)
	e.mustPrepare(e.local, u1)
	e.mustPrepare(e.local, localClaim("u2", device.LeaseDeviceName, 0))
	// a companion claim
	e.mustPrepare(e.local, companion(localClaim("u3", localDev, 64*mib), "l1"))
	want := []cdiSpecs.Device{
		{Name: "claim-u1-0", ContainerEdits: cdiSpecs.ContainerEdits{Env: []string{
			"CXL_CLAIM_u1_0=1", "CXL_DRAM_MODE=numa", "CXL_DRAM_NUMA_NODES=0-1", "CXL_DRAM_SIZE=67108864"}}},
		{Name: "claim-u1-2", ContainerEdits: cdiSpecs.ContainerEdits{Env: []string{
			"CXL_CLAIM_u1_2=1", "CXL_REGION0_MODE=numa", "CXL_REGION0_NUMA_NODE=2", "CXL_REGION0_SIZE=67108864"}}},
		{Name: "claim-u2-0", ContainerEdits: cdiSpecs.ContainerEdits{Env: []string{"CXL_CLAIM_u2_0=1"}}},
		{Name: "lease-u2", ContainerEdits: cdiSpecs.ContainerEdits{Env: []string{"LEASE_u2=1"}}},
	}
	spec, err := buildCDISpec(e.st.Snapshot(), device.DriverName, device.CDIKind(device.DriverName), e.local.roots, "", e.leases)
	mustOK(t, err)
	if spec.Kind != "cxl.nri.io/device" || !reflect.DeepEqual(spec.Devices, want) {
		t.Fatalf("CDI spec %+v", spec)
	}
	if onDisk := e.cdiSpec(device.ClaimsCDISpecName); onDisk == nil || !reflect.DeepEqual(onDisk.Devices, want) {
		t.Fatalf("CDI spec on disk %+v", onDisk)
	}
}

// The CDI specs of the claims of both drivers, built from the records:
// sorted by claim, a second result on one memdev with the env key _2, of
// the kind and in the file of the driver; companions, claims whose
// devices did not verify at the start and claims whose Prepare is not
// finishing are left out, UNPREPARING ones are kept; the spec of no device
// is removed.
func TestCDISpecOfClaims(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, fake.MemdevSpec{Serial: serialPoolS, HostBridge: 24})
	b := addLocalResult(localClaim("b", localDev, envBlockSize), "second", localDev, envBlockSize)
	e.mustPrepare(e.local, b)
	e.mustPrepare(e.local, localClaim("a", device.DRAMDeviceName, 64*mib))
	e.mustPrepare(e.pool, poolClaim("p2", "shared0", poolSharedData))
	e.mustPrepare(e.pool, poolClaim("p1", "shared0", poolSharedData))
	if got := e.cdiNames(e.local); !slices.Equal(got, []string{"claim-a-0", "claim-b-0", "claim-b-1"}) {
		t.Fatalf("local CDI devices %v", got)
	}
	if got := e.cdiNames(e.pool); !slices.Equal(got, []string{"claim-p1-0", "claim-p2-0"}) {
		t.Fatalf("pool CDI devices %v", got)
	}
	if spec := e.cdiSpec(device.PoolCDISpecName); spec.Kind != device.CDIKind(device.PoolDriverName) {
		t.Fatalf("pool CDI spec kind %q", spec.Kind)
	}
	b1 := e.cdiDevice(e.local, "claim-b-1")
	hasEnv(t, b1, "CXL_CLAIM_b_1=1", "CXL_C100E2E1_2_MODE=numa", "CXL_C100E2E1_2_SERIAL=0xc100e2e1", "CXL_C100E2E1_2_SIZE=134217728")
	hasEnv(t, e.cdiDevice(e.local, "claim-b-0"), "CXL_C100E2E1_MODE=numa")

	// the claims as the spec takes them by their state
	doc := e.st.Snapshot().Clone()
	setState := func(driver, uid string, s state.PreparationState) {
		doc.Preparation(driver, uid).State = s
	}
	names := func(p *Plugin, pending string) []string {
		spec, err := buildCDISpec(doc, p.Name(), p.Rules().CDIKind(), p.roots, pending, nil)
		mustOK(t, err)
		var out []string
		for _, d := range spec.Devices {
			out = append(out, d.Name)
		}
		return out
	}
	setState(device.DriverName, "a", state.PreparationUnpreparing)
	setState(device.DriverName, "b", state.PreparationUnrestored)
	setState(device.PoolDriverName, "p1", state.PreparationUnpreparing)
	setState(device.PoolDriverName, "p2", state.PreparationPreparing)
	if got := names(e.local, ""); !slices.Equal(got, []string{"claim-a-0"}) {
		t.Fatalf("local %v", got)
	}
	if got := names(e.pool, ""); !slices.Equal(got, []string{"claim-p1-0"}) {
		t.Fatalf("pool %v", got)
	}
	if got := names(e.pool, "p2"); !slices.Equal(got, []string{"claim-p1-0", "claim-p2-0"}) {
		t.Fatalf("pool with the pending claim %v", got)
	}
	doc.Preparation(device.DriverName, "a").Lease = "l1"
	if got := names(e.local, "a"); len(got) != 0 {
		t.Fatalf("a pending companion %v", got)
	}

	for _, uid := range []string{"a", "b"} {
		e.mustUnprepare(e.local, uid)
	}
	if e.cdiSpec(device.ClaimsCDISpecName) != nil || e.cdiSpec(device.PoolCDISpecName) == nil {
		t.Fatal("the spec of no device is not removed, or the spec of the other driver is")
	}
	for _, uid := range []string{"p1", "p2"} {
		e.mustUnprepare(e.pool, uid)
	}
	e.wantClean()
}

// The drivers on a LOST store write and remove no CDI spec: the records of
// the prepared claims are unknown, not absent.
func TestCDISpecsOfALostStore(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, fake.MemdevSpec{Serial: serialPoolS, HostBridge: 24})
	e.mustPrepare(e.local, localClaim("a", device.DRAMDeviceName, 64*mib))
	e.mustPrepare(e.pool, poolClaim("p1", "shared0", poolSharedData))
	files := func() map[string]string {
		out := map[string]string{}
		m, err := filepath.Glob(filepath.Join(e.cdiDir, "*"))
		mustOK(t, err)
		for _, f := range m {
			data, err := os.ReadFile(f)
			mustOK(t, err)
			out[filepath.Base(f)] = string(data)
		}
		return out
	}
	before := files()
	if len(before) != 2 {
		t.Fatalf("CDI specs %v", slices.Collect(maps.Keys(before)))
	}
	_ = e.st.Close()
	for _, name := range []string{store.FileName, store.PrevName} {
		mustOK(t, os.WriteFile(filepath.Join(e.dir, name), []byte("{"), 0o600))
	}
	st, err := store.Open(context.Background(), store.Options{Dir: e.dir, BootID: e.k.BootID()})
	mustOK(t, err)
	e.st = st
	if st.Lost() == nil {
		t.Fatal("the store is not lost")
	}
	for _, p := range []*Plugin{e.local, e.pool} {
		lost, err := New(Options{Rules: p.Rules(), Store: st, Engine: e.eng, NodeName: envNode,
			CDI: e.cache, Publisher: e.pub})
		mustOK(t, err)
		mustOK(t, lost.WriteCDI())
	}
	if after := files(); !maps.Equal(after, before) {
		t.Fatalf("CDI specs changed on a lost store:\n%v\nwas\n%v", after, before)
	}
}

// writeCDISpec writes a spec of devices and removes the spec file of no
// devices, its write error wrapped; writeCDI writes nothing without a CDI
// cache.
func TestWriteCDISpec(t *testing.T) {
	dir := t.TempDir()
	cache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(dir), cdiapi.WithAutoRefresh(false))
	mustOK(t, err)
	name, driver := device.PoolCDISpecName, device.PoolDriverName
	spec := func() *cdiSpecs.Spec {
		return &cdiSpecs.Spec{Kind: device.CDIKind(device.PoolDriverName), Devices: []cdiSpecs.Device{{Name: "claim-u1-0",
			ContainerEdits: cdiSpecs.ContainerEdits{Env: []string{"CXL_CLAIM_u1_0=1"}}}}}
	}
	written := spec()
	mustOK(t, writeCDISpec(cache, written, driver, name))
	m, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(m) != 1 || filepath.Base(m[0]) != name || written.Version == "" {
		t.Fatalf("spec files %v, version %q", m, written.Version)
	}
	data, err := os.ReadFile(m[0])
	mustOK(t, err)
	if !json.Valid(data) || !strings.Contains(string(data), "claim-u1-0") {
		t.Fatalf("spec\n%s", data)
	}
	mustOK(t, writeCDISpec(cache, &cdiSpecs.Spec{Kind: device.CDIKind(device.PoolDriverName)}, driver, name))
	if m, _ := filepath.Glob(filepath.Join(dir, "*")); len(m) != 0 {
		t.Fatalf("left %v", m)
	}
	// again: nothing to remove is not an error
	mustOK(t, writeCDISpec(cache, &cdiSpecs.Spec{Kind: device.CDIKind(device.PoolDriverName)}, driver, name))
	// the spec file a directory with an entry: the write fails
	mustOK(t, os.MkdirAll(filepath.Join(dir, name, "entry"), 0o755))
	err = writeCDISpec(cache, spec(), driver, name)
	wantErrText(t, err, "write CDI spec "+name+": ")
	if errors.Unwrap(err) == nil {
		t.Fatalf("the error %v is not wrapped", err)
	}
	st, err := store.Open(t.Context(), store.Options{Dir: t.TempDir(), BootID: "b1"})
	mustOK(t, err)
	defer func() { _ = st.Close() }()
	p := &Plugin{rules: poolRules(device.PoolDriverName, ""), st: st}
	mustOK(t, p.WriteCDI())
}

// The names of the CDI devices and of the result markers: the claim UID
// and the result, the UID's hyphens as underscores in the marker.
func TestCDINames(t *testing.T) {
	if got := ClaimCDIDeviceName("a-b", 3); got != "claim-a-b-3" {
		t.Fatalf("device name %q", got)
	}
	if got := api.ResultMarker("a-b", 3); got != "CXL_CLAIM_a_b_3" {
		t.Fatalf("marker %q", got)
	}
	for _, tc := range []struct {
		env string
		uid string
		n   int
		ok  bool
	}{
		{"CXL_CLAIM_a_b_3=1", "a-b", 3, true},
		{"CXL_CLAIM_u1_0", "u1", 0, true},
		{"CXL_CLAIM_u1_12=1", "u1", 12, true},
		{"CXL_CLAIM_u1_03=1", "", 0, false},
		{"CXL_CLAIM_u1_x=1", "", 0, false},
		{"CXL_CLAIM__1=1", "", 0, false},
		{"CXL_DRAM_SIZE=1", "", 0, false},
	} {
		uid, n, ok := api.ParseResultMarker(tc.env)
		if uid != tc.uid || n != tc.n || ok != tc.ok {
			t.Errorf("%s: %q %d %v", tc.env, uid, n, ok)
		}
	}
}

// A PREPARED or pending claim whose fs device has no node mount makes the
// CDI spec an error: a container would start without its filesystem. The
// kept device of an UNPREPARING claim has none after a failed release, and
// the spec still writes. A dax device that is gone has no device node.
func TestCDISpecOfIncompleteDevices(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec(), pmemSpec()})
	e.formatPmem(serialPmem, "ext4")
	e.mustPrepare(e.local, localClaim("f", pmemDev, -1, cfgFsRO))
	e.mustPrepare(e.local, localClaim("d", localDev, envBlockSize, cfgDax))
	doc := e.st.Snapshot().Clone()
	doc.Grant(state.GrantRef{Claim: "f"}).Got.Mount = ""
	doc.Grant(state.GrantRef{Claim: "d"}).Got.Dax = ""
	build := func(pending string) (*cdiSpecs.Spec, error) {
		return buildCDISpec(doc, device.DriverName, device.CDIKind(device.DriverName), e.local.roots, pending, nil)
	}
	spec, err := build("")
	wantErrText(t, err, "claim demo/claim-f: result 0: no node mount")
	if len(spec.Devices) != 1 || spec.Devices[0].Name != "claim-d-0" || len(spec.Devices[0].ContainerEdits.DeviceNodes) > 0 {
		t.Fatalf("CDI devices %+v", spec.Devices)
	}
	doc.Preparation(device.DriverName, "f").State = state.PreparationPreparing
	_, err = build("f")
	wantErrText(t, err, "claim demo/claim-f: result 0: no node mount")
	doc.Preparation(device.DriverName, "f").State = state.PreparationUnpreparing
	spec, err = build("")
	mustOK(t, err)
	if len(spec.Devices) != 2 || len(spec.Devices[0].ContainerEdits.DeviceNodes)+len(spec.Devices[1].ContainerEdits.Mounts) > 0 {
		t.Fatalf("CDI devices %+v", spec.Devices)
	}
}
