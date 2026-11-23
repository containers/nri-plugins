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
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// localRegionNode returns the NUMA node of the region of the local memdev
// in the kernel.
func localRegionNode(e *driverTestEnv) int {
	e.t.Helper()
	k := e.k.Kernel()
	rs := k.Regions(e.k.Memdev(localSerial))
	if len(rs) != 1 {
		e.t.Fatalf("regions %v", rs)
	}
	ri, err := k.Region(rs[0])
	noErr(e.t, err)
	if ri.NUMANode < 0 {
		e.t.Fatalf("region %+v without a node", ri)
	}
	return ri.NUMANode
}

// injectDevices adds devices of regions, DRAM or the lease device to the
// devices of the last scan of the driver.
func injectDevices(e *driverTestEnv, infos ...*dra.DeviceInfo) {
	devs := maps.Clone(e.d.publisher.Devices())
	for _, d := range infos {
		devs[d.Name] = d
	}
	e.d.publisher.SetDevices(devs)
}

// memoryConfigClaim returns a claim with the results numa (a slice of the
// local memdev), region, dram and lease, in this order, of the device
// classes cxl-local, cxl-region, cxl-dram and cxl-lease, with the configs.
func memoryConfigClaim(uid string, configs ...resourcev1.DeviceAllocationConfiguration) *resourcev1.ResourceClaim {
	c := localClaim(uid, localBlockSize)
	c.Status.Allocation.Devices.Results[0].Request = "numa"
	for _, r := range []struct{ request, dev string }{{"region", "region5-numa4"}, {"dram", "dram"}, {"lease", device.LeaseDeviceName}} {
		c.Status.Allocation.Devices.Results = append(c.Status.Allocation.Devices.Results,
			resourcev1.DeviceRequestAllocationResult{Request: r.request, Driver: device.DriverName, Pool: testNode, Device: r.dev})
	}
	for _, i := range []int{1, 2} {
		c.Status.Allocation.Devices.Results[i].ConsumedCapacity = map[resourcev1.QualifiedName]resource.Quantity{
			device.CapacityMemory: *resource.NewQuantity(64*mib, resource.BinarySI)}
	}
	for _, r := range []struct{ request, class string }{{"numa", "cxl-local"}, {"region", "cxl-region"}, {"dram", "cxl-dram"}, {"lease", "cxl-lease"}} {
		c.Spec.Devices.Requests = append(c.Spec.Devices.Requests,
			resourcev1.DeviceRequest{Name: r.request, Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: r.class}})
	}
	c.Status.Allocation.Devices.Config = configs
	return c
}

// memoryConfigEntry returns an opaque config entry of cxl.nri.io.
func memoryConfigEntry(source resourcev1.AllocationConfigSource, requests []string, params string) resourcev1.DeviceAllocationConfiguration {
	e := makeOpaqueConfig(device.DriverName, source, json.RawMessage(params))
	e.Requests = requests
	return e
}

// A container gets the management of the prepared results that it uses,
// as the MemoryConfig entries of their requests say: the NRI plugin finds
// each result in the driver by the marker of the result in its env.
func TestContainerOfResultSettings(t *testing.T) {
	e := newDriverTestEnv(t)
	e.config.Flags.Leases = true
	e.start()
	e.synchronize()
	injectDevices(e,
		&dra.DeviceInfo{Name: "region5-numa4", Dev: &dra.Region{Name: "region5", NUMANode: 4, Size: 1 << 30}},
		&dra.DeviceInfo{Name: "dram", Dev: &dra.DRAM{NUMANodes: []int{0}, Size: 1 << 30}},
		&dra.DeviceInfo{Name: device.LeaseDeviceName, Dev: &dra.Lease{}})
	const (
		class = resourcev1.AllocationConfigSourceClass
		claim = resourcev1.AllocationConfigSourceClaim
	)

	c := memoryConfigClaim("u1",
		// a claim entry before a class entry: classes come first
		memoryConfigEntry(claim, []string{"dram"}, `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","container":{"steering":{"policy":"first-dram","maxStep":"1Gi"}}}`),
		memoryConfigEntry(class, nil, `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","container":{"steering":{"policy":"auto"}}}`),
		// null removes the preset of the class
		memoryConfigEntry(claim, []string{"numa"}, `{"apiVersion":"cxl.nri.io/v1alpha1","kind":"MemoryConfig","container":{"steering":{"policy":null}}}`),
	)
	e.mustPrepare(e.d.local, c)
	node := localRegionNode(e)
	for _, tc := range []struct {
		results []int
		want    string
	}{
		{[]int{0}, "fenced " + strconv.Itoa(node) + " none (default)"},
		{[]int{1}, "steered 4 auto from class cxl-region"},
		{[]int{2}, "steered  first-dram from claim demo/claim-u1 request dram"},
		{[]int{0, 1}, "steered " + strconv.Itoa(node) + ",4 auto from class cxl-region"},
		{[]int{3}, "fenced  auto from class cxl-lease"},
	} {
		c, err := e.createContainer("u1", tc.results...)
		noErr(t, err)
		if got := strings.ToLower(string(c.Management)) + " " + c.Claimed.String() + " " + c.Steering; got != tc.want {
			t.Errorf("results %v: %q, expected %q", tc.results, got, tc.want)
		}
	}
	_, err := e.createContainer("u1", 1, 2)
	wantErr(t, err, "container demo/pod/ctr: container.steering.policy: class cxl-region says auto, claim demo/claim-u1 request dram says first-dram")
	e.mustUnprepare(e.d.local, "u1")
	e.wantClean()
}
