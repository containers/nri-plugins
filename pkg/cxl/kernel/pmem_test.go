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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

const (
	// ndRegion0 is the nd region of region0 in the fake.
	ndRegion0   = "devices/platform/ACPI0017:00/root0/nvdimm-bridge0/ndbus0/region0"
	pmemNsBytes = 512 * mib
	// pmemBlockBytes is the size of the block device of an fsdax namespace
	// with map mem (ndctl list: 510 MiB).
	pmemBlockBytes = 510 * mib
)

// pmemNode returns a fake with a persistent memdev mem0 (512 MiB, no RAM).
func pmemNode(t *testing.T) (*fake.Fake, *kernel.Kernel) {
	t.Helper()
	f := fake.New(t)
	f.Plug(fake.MemdevSpec{Serial: serial0, Pmem: pmemNsBytes})
	return f, f.Kernel()
}

// A writer creates the fsdax namespace in a new region; the labels outlive
// the region, and a reader finds the namespace from them after the probe
// of the region, the namespace and its pfn device.
func TestPmemWriterThenReader(t *testing.T) {
	f, k := pmemNode(t)
	f.PmemDelay = 10 * time.Millisecond
	ctx := ctxTimeout(t, 5*time.Second)
	r := createRegion(t, k, "mem0", kernel.RegionPmem, 0)
	if info, _ := k.Region(r); !strings.Contains(f.Commands(), "cxl create-region -t pmem -d decoder0.2 -m mem0") || info.Mode != kernel.RegionPmem {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	nd, err := k.WaitNdRegion(ctx, r)
	if err != nil || nd != "region0" {
		t.Fatalf("nd region %q %v", nd, err)
	}
	nss, err := k.Namespaces(ctx, nd)
	if err != nil || len(nss) != 0 || kernel.FsdaxNamespace(nss) != nil {
		t.Fatalf("namespaces of a new region %+v %v", nss, err)
	}
	ns, err := k.CreateFsdaxNamespace(ctx, nd, "")
	noErr(t, err)
	want := kernel.Namespace{Name: "namespace0.0", State: kernel.NsFsdax, Mode: kernel.NamespaceFsdax, Map: kernel.MapMem, Size: pmemBlockBytes,
		Holder: "pfn0.0", BlockDevice: f.DevRoot() + "/pmem0", Major: 259, Minor: 0}
	if !reflect.DeepEqual(*ns, want) {
		t.Fatalf("namespace %+v\nwant %+v", *ns, want)
	}
	if !strings.Contains(f.Commands(), "ndctl create-namespace -f -r region0 -m fsdax -M mem") {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	noErr(t, k.DestroyRegion(ctx, r))
	noErr(t, k.DisableMemdev(ctx, "mem0"))
	if !strings.HasSuffix(f.Commands(), "cxl disable-region region0\ncxl destroy-region region0\ncxl disable-memdev mem0") {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	// the reader: the namespace from the labels, a block device with new
	// numbers
	noErr(t, k.EnableMemdev(ctx, "mem0"))
	r = createRegion(t, k, "mem0", kernel.RegionPmem, 0)
	nd, err = k.WaitNdRegion(ctx, r)
	noErr(t, err)
	nss, err = k.Namespaces(ctx, nd)
	noErr(t, err)
	got := kernel.FsdaxNamespace(nss)
	want.Holder, want.Minor = "pfn0.1", 1
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("reader namespaces %+v", nss)
	}
	if strings.Count(f.Commands(), "ndctl") != 1 {
		t.Fatalf("commands:\n%s", f.Commands())
	}
}

// The probe of an nd region and its namespaces in the order of the kernel:
// every partial state is PROBING; the pfn mode is read last, and an
// attribute that cannot be read is a probe in progress.
func TestNamespacesWaitForProbe(t *testing.T) {
	f, k := pmemNode(t)
	f.Write(ndRegion0+"/size", itoa(pmemNsBytes))
	f.LinkTo("bus/nd/devices/region0", ndRegion0)
	pfn := ndRegion0 + "/pfn0.1"
	ns := ndRegion0 + "/namespace0.0"
	poll := func(want kernel.NdRegionState) []kernel.Namespace {
		t.Helper()
		nd, err := k.NdRegion("region0")
		if err != nil || nd.State != want {
			t.Fatalf("nd region %v (%s) %v, want %v", nd.State, nd.Why, err, want)
		}
		nss, why, err := k.ProbeNamespaces("region0")
		if err != nil || (why == "") != (want == kernel.NdSettled) {
			t.Fatalf("probe: why %q %v", why, err)
		}
		return nss
	}
	if nd, _ := k.NdRegion("region9"); nd.State != kernel.NdAbsent {
		t.Fatalf("absent nd region %v", nd.State)
	}
	// region not bound
	poll(kernel.NdProbing)
	// bound, init_namespaces not there yet
	f.LinkTo(ndRegion0+"/driver", "bus/nd/drivers/nd_region")
	poll(kernel.NdProbing)
	// its namespace not registered yet
	f.Write(ndRegion0+"/init_namespaces", "1/1")
	poll(kernel.NdProbing)
	// registered without a holder attribute yet
	f.LinkTo("bus/nd/devices/namespace0.0", ns)
	f.Write(ns+"/size", itoa(pmemNsBytes))
	if nd, _ := k.NdRegion("region0"); nd.State != kernel.NdProbing || nd.Namespaces[0].State != kernel.NsProbing || !strings.Contains(nd.Why, "holder") {
		t.Fatalf("namespace without a holder: %+v", nd)
	}
	// nd_pmem probing the namespace: raw, without its block device yet
	f.Write(ns+"/holder", "")
	f.LinkTo(ns+"/driver", "bus/nd/drivers/nd_pmem")
	poll(kernel.NdProbing)
	// claimed by a pfn device without its driver yet: its disk complete,
	// only the driver missing
	f.Write(ns+"/holder", "pfn0.1")
	f.Write(pfn+"/mode", "ram")
	f.LinkTo("bus/nd/devices/pfn0.1", pfn)
	f.Write(pfn+"/block/pmem0/dev", "259:0")
	f.Write(pfn+"/block/pmem0/size", itoa(pmemBlockBytes/512))
	f.LinkTo("class/block/pmem0", pfn+"/block/pmem0")
	poll(kernel.NdProbing)
	if nd, _ := k.NdRegion("region0"); !strings.HasSuffix(nd.Why, "pfn0.1/driver") {
		t.Fatalf("pfn device without its driver: %q", nd.Why)
	}
	// the driver bound, its probe adding the disk: the disk directory is
	// there, its class link and dev attribute not yet
	f.LinkTo(pfn+"/driver", "bus/nd/drivers/nd_pmem")
	f.Remove("class/block/pmem0")
	poll(kernel.NdProbing)
	f.LinkTo("class/block/pmem0", pfn+"/block/pmem0")
	f.Remove(pfn + "/block/pmem0/dev")
	poll(kernel.NdProbing)
	f.Write(pfn+"/block/pmem0/dev", "259:0")
	// the disk complete, the mode of the pfn device not readable (or
	// unknown): not settled, whatever the order of the reads
	f.Remove(pfn + "/mode")
	poll(kernel.NdProbing)
	f.Write(pfn+"/mode", "bogus")
	poll(kernel.NdProbing)
	f.Write(pfn+"/mode", "ram")
	nss := poll(kernel.NdSettled)
	if len(nss) != 1 || nss[0].State != kernel.NsFsdax || nss[0].Map != kernel.MapMem || nss[0].BlockDevice != f.DevRoot()+"/pmem0" {
		t.Fatalf("namespaces %+v", nss)
	}
	// map dev
	f.Write(pfn+"/mode", "pmem")
	if nss := poll(kernel.NdSettled); nss[0].Map != kernel.MapDev || kernel.FsdaxNamespace(nss) != nil {
		t.Fatalf("map dev namespace %+v", nss)
	}
	// a raw namespace with its own block device
	f.Remove(pfn)
	f.Write(ns+"/holder", "")
	f.Write(ns+"/block/pmem0/dev", "259:0")
	f.Write(ns+"/block/pmem0/size", itoa(pmemNsBytes/512))
	f.LinkTo("class/block/pmem0", ns+"/block/pmem0")
	if nss := poll(kernel.NdSettled); nss[0].State != kernel.NsRaw || nss[0].Size != pmemNsBytes || nss[0].BlockDevice != f.DevRoot()+"/pmem0" {
		t.Fatalf("raw namespace %+v", nss)
	}
	// devdax
	f.Write(ns+"/holder", "dax0.1")
	f.Write(ndRegion0+"/dax0.1/uevent", "")
	f.LinkTo("bus/nd/devices/dax0.1", ndRegion0+"/dax0.1")
	f.LinkTo(ndRegion0+"/dax0.1/driver", "bus/nd/drivers/nd_pmem")
	if nss := poll(kernel.NdSettled); nss[0].State != kernel.NsDevdax || nss[0].BlockDevice != "" || kernel.FsdaxNamespace(nss) != nil {
		t.Fatalf("devdax namespace %+v", nss)
	}
	// the seed
	f.Write(ns+"/size", "0")
	if nd, _ := k.NdRegion("region0"); nd.State != kernel.NdSettled || nd.Namespaces[0].State != kernel.NsSeed {
		t.Fatalf("seed: %+v", nd)
	}
	if nss := poll(kernel.NdSettled); len(nss) != 0 {
		t.Fatalf("a seed has memory: %+v", nss)
	}
	// Namespaces gives up with the context, naming the missing file
	f.Write(ns+"/size", itoa(pmemNsBytes))
	f.Remove(ndRegion0 + "/dax0.1/driver")
	_, err := k.Namespaces(ctxTimeout(t, 20*time.Millisecond), "region0")
	wantErr(t, err, nil, "region0: namespaces still probing (namespace0.0: no ")
	if !strings.Contains(err.Error(), "bus/nd/devices/dax0.1/driver): context deadline exceeded") {
		t.Fatalf("error %q", err)
	}
}

// ndctl create-namespace -f is never run over a region that is probing or
// has an fsdax namespace: it would destroy what a writer formatted.
func TestCreateFsdaxNamespaceRefuses(t *testing.T) {
	f, k := pmemNode(t)
	ctx := ctxTimeout(t, 5*time.Second)
	r := createRegion(t, k, "mem0", kernel.RegionPmem, 0)
	// a writer formatted the device meanwhile, with map dev
	f.Fsdax(r, "pfn0.1")
	f.Write(ndRegion0+"/pfn0.1/mode", "pmem")
	_, err := k.CreateFsdaxNamespace(ctx, "region0", "namespace0.0")
	wantErr(t, err, nil, `region0: not creating an fsdax namespace: namespace0.0 is one already (map "dev")`)
	// its mode cannot be read
	f.Remove(ndRegion0 + "/pfn0.1/mode")
	_, err = k.CreateFsdaxNamespace(ctx, "region0", "namespace0.0")
	wantErr(t, err, nil, "region0: not creating an fsdax namespace: the namespaces are still probing (namespace0.0: cannot read ")
	// the region is not bound
	f.Remove(ndRegion0 + "/driver")
	_, err = k.CreateFsdaxNamespace(ctx, "region0", "")
	wantErr(t, err, nil, "the namespaces are still probing")
	if strings.Contains(f.Commands(), "ndctl") {
		t.Fatalf("ndctl ran:\n%s", f.Commands())
	}
	// ndctl output without a namespace
	f.LinkTo(ndRegion0+"/driver", "bus/nd/drivers/nd_region")
	f.Write(ndRegion0+"/pfn0.1/mode", "ram")
	f.Remove(ndRegion0 + "/pfn0.1")
	f.Write(ndRegion0+"/namespace0.0/holder", "")
	f.Write(ndRegion0+"/namespace0.0/size", "0")
	k.Exec = func(ctx context.Context, name string, args ...string) (string, error) { return "{}", nil }
	_, err = k.CreateFsdaxNamespace(ctx, "region0", "")
	wantErr(t, err, nil, "ndctl create-namespace did not report a namespace")
	// no nd region of a region that is not a pmem one
	_, err = k.NdRegionOf("pmem_region0")
	wantErr(t, err, nil, "invalid region name")
	_, err = k.WaitNdRegion(ctxTimeout(t, 20*time.Millisecond), "region3")
	wantErr(t, err, nil, "region3: no nvdimm region appeared")
}

// An fsdax namespace in two steps: ndctl returns the name, the wait gives
// the namespace with its block device. The namespace that a writer
// reconfigures is named (-e).
func TestFsdaxNamespaceInTwoSteps(t *testing.T) {
	f, k := pmemNode(t)
	f.PmemDelay = 20 * time.Millisecond
	createRegion(t, k, "mem0", kernel.RegionPmem, 0)
	ctx := ctxTimeout(t, 5*time.Second)
	nd, err := k.WaitNdRegion(ctx, "region0")
	noErr(t, err)
	_, err = k.Namespaces(ctx, nd)
	noErr(t, err)
	name, err := k.StartFsdaxNamespace(ctx, nd, "namespace0.0")
	noErr(t, err)
	if !strings.Contains(f.Commands(), "ndctl create-namespace -f -r region0 -m fsdax -M mem -e namespace0.0") {
		t.Fatalf("commands:\n%s", f.Commands())
	}
	ns, err := k.WaitFsdaxNamespace(ctx, nd, name)
	noErr(t, err)
	if ns.Name != name || ns.Mode != kernel.NamespaceFsdax || ns.BlockDevice == "" || !f.Labels(serial0) {
		t.Fatalf("namespace %+v", ns)
	}
	if major, minor, size, err := k.BlockDeviceInfo("pmem0"); err != nil || major != 259 || minor != 0 || size != pmemBlockBytes {
		t.Fatalf("block device %d:%d %d %v", major, minor, size, err)
	}
	_, err = k.StartFsdaxNamespace(ctx, nd, "")
	wantErr(t, err, nil, "is one already")
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := k.WaitFsdaxNamespace(short, nd, "namespace9.9"); err == nil || !strings.Contains(err.Error(), "has no fsdax block device") {
		t.Fatalf("waiting for a missing namespace: %v", err)
	}
	f.Write("class/block/pmem0/dev", "bogus")
	if _, _, _, err := k.BlockDeviceInfo("pmem0"); err == nil {
		t.Fatal("bogus device numbers accepted")
	}
}
