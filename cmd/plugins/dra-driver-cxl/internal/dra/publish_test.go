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
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// The helpers of the tests of the publisher have the prefix pub (or a name
// of their own) so that they do not clash with those of the other tests of
// the package.
const (
	pubNode = "node1"
	pubNS   = "demo"
	// pubLocalSerial is the local memdev of pubMemdev, published as
	// pubLocalDeviceName.
	pubLocalSerial     = 0xc100e2e1
	pubLocalDeviceName = "memdev-c100e2e1"
	pubMiB             = 1 << 20
	// pubRegionSize is the size of the local memdev and its region.
	pubRegionSize = 1 << 30
	// pubBlockSize is the memory block size of the tests.
	pubBlockSize = 128 * pubMiB
)

// pubMemdev returns the observed local memdev pubLocalSerial.
func pubMemdev() kernel.MemdevInfo {
	m := pubMem("mem0", pubLocalSerial, pubRegionSize, 0)
	m.Major = 251
	return m
}

// memdevSpecs returns the memdevs of the specs.
func memdevSpecs(t *testing.T, specs ...string) []MemdevSpec {
	t.Helper()
	parsed, err := ParseMemdevSpecs(specs)
	mustOK(t, err)
	return parsed
}

// pubMem returns an observed memdev with a driver, of RAM capacity ram and
// with the CPU NUMA node affinity.
func pubMem(name string, serial, ram uint64, affinity int) kernel.MemdevInfo {
	return kernel.MemdevInfo{Name: name, Serial: serial, State: kernel.MemdevReady, RamSize: ram, NUMAAffinity: affinity}
}

// pubRegion adds a region of RAM of size bytes on the memdevs with the
// serials to the snapshot s, after the ranges of its regions: enabled with
// its memory blocks online on node, disabled without memory for node -1.
// The block size of s is pubBlockSize unless it has one.
func pubRegion(s *kernel.Snapshot, name string, node int, size uint64, serials ...uint64) {
	if s.BlockSize == 0 {
		s.BlockSize = pubBlockSize
	}
	start := uint64(0)
	for _, r := range s.Regions {
		start = max(start, (r.Range.End+s.BlockSize)/s.BlockSize*s.BlockSize)
	}
	r := kernel.RegionInfo{Name: name, State: kernel.RegionEnabled, Mode: kernel.RegionRAM, NUMANode: node,
		Range: kernel.Range{Start: start, End: start + size - 1}, Serials: serials}
	if node < 0 {
		r.State = kernel.RegionDisabled
	} else {
		s.DaxDevices = append(s.DaxDevices, kernel.DaxDeviceInfo{Name: "dax-" + name, Region: name, Size: size, TargetNode: node})
		for _, b := range r.Range.Blocks(s.BlockSize) {
			s.Blocks = append(s.Blocks, kernel.MemoryBlock{Index: b, State: kernel.BlockOnline, NUMANode: node})
		}
	}
	s.Regions = append(s.Regions, r)
}

// pubNodes returns NUMA nodes of 1 GiB of memory with the ids.
func pubNodes(ids ...int) []kernel.NUMANode {
	var out []kernel.NUMANode
	for _, id := range ids {
		out = append(out, kernel.NUMANode{ID: id, HasMemory: true, MemTotal: 1 << 30})
	}
	return out
}

func pubNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// pubStore returns a new store in a temporary directory, closed at the end
// of the test.
func pubStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Dir: t.TempDir(), BootID: "boot-1"})
	pubNoErr(t, err)
	t.Cleanup(func() { pubNoErr(t, st.Close()) })
	return st
}

// pubPublisher returns the publisher of opts, with a new store and a scan
// of no devices where opts has none.
func pubPublisher(t *testing.T, opts publishOptions) *Publisher {
	t.Helper()
	if opts.Store == nil {
		opts.Store = pubStore(t)
	}
	if opts.Scan == nil {
		opts.Scan = pubNoScan
	}
	p, err := newPublisher(opts)
	pubNoErr(t, err)
	return p
}

// pubNoScan returns an observation of a node without CXL objects.
func pubNoScan(context.Context) (*kernel.Snapshot, error) { return &kernel.Snapshot{}, nil }

// dramScan returns a scan that finds a node with DRAM while dram is set,
// one without devices otherwise.
func dramScan(dram *atomic.Bool) func(context.Context) (*kernel.Snapshot, error) {
	return func(context.Context) (*kernel.Snapshot, error) {
		if !dram.Load() {
			return &kernel.Snapshot{}, nil
		}
		return &kernel.Snapshot{NUMANodes: []kernel.NUMANode{{ID: 0, HasMemory: true, MemTotal: 8 << 30}}}, nil
	}
}

// pubNames returns the names of the devices, sorted.
func pubNames(devs devicesInfo) []string {
	return slices.Sorted(maps.Keys(devs))
}

// wantLogLineText fails the test unless a line of the log ends with text.
func wantLogLineText(t *testing.T, log, text string) {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if strings.HasSuffix(line, text) {
			return
		}
	}
	t.Fatalf("no log line ends with %q:\n%s", text, log)
}

// logMatches returns the number of lines of the log that contain text.
func logMatches(log, text string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, text) {
			n++
		}
	}
	return n
}

// TestBuildDevInfosLocalMemdevs pins the local memdevs that a scan
// publishes: free and used ones of at least a memory block of a known
// size, not pooled, ignored, disabled or without RAM; the node of the
// plugin's region is not DRAM; without managed memdevs regions only.
func TestBuildDevInfosLocalMemdevs(t *testing.T) {
	devices := &kernel.Snapshot{NUMANodes: pubNodes(0, 2, 3)}
	for i, name := range []string{"mem1", "mem2", "mem3", "mem4", "mem5", "mem6", "mem7"} {
		devices.Memdevs = append(devices.Memdevs, pubMem(name, 0xa1+uint64(i), 1<<30, -1))
	}
	free := &devices.Memdevs[0]
	free.NUMAAffinity = 1
	devices.Memdevs[5].State = kernel.MemdevUnbound
	devices.Memdevs[6].RamSize = 0
	pubRegion(devices, "region0", 2, 1<<30, 0xa2)
	pubRegion(devices, "region1", 3, 1<<30, 0xa5)
	cfg := PublishConfig{IgnoreDevices: memdevSpecs(t, "0xa4"), ManageMemdevs: true}
	local := &localRecords{Memdevs: map[uint64]localMemdev{0xa5: {}}, BlockSize: pubBlockSize}
	pool := map[uint64]bool{0xa3: true}
	devInfos := BuildDevices(cfg, devices, pool, nil, local)
	got := pubNames(devInfos)
	want := []string{"dram", "memdev-a1", "memdev-a5", "region0-numa2"}
	if !slices.Equal(got, want) {
		t.Fatalf("published %v, expected %v", got, want)
	}
	for name, d := range devInfos {
		if d.Name != name {
			t.Fatalf("device %s named %s", name, d.Name)
		}
	}
	if m, ok := devInfos["memdev-a1"].Dev.(*Memdev); !ok || *m != (Memdev{Name: "mem1", Serial: 0xa1, Size: 1 << 30, NUMAAffinity: 1, BlockSize: pubBlockSize}) {
		t.Fatalf("free memdev %+v", devInfos["memdev-a1"].Dev)
	}
	if _, ok := devInfos["memdev-a5"].Dev.(*Memdev); !ok {
		t.Fatalf("used memdev %+v", devInfos["memdev-a5"].Dev)
	}
	// memdevs smaller than a block, or an unknown block size: none
	free.RamSize = pubBlockSize - 2*pubMiB
	devInfos = BuildDevices(cfg, devices, pool, nil, local)
	if _, ok := devInfos["memdev-a1"]; ok {
		t.Fatal("a memdev smaller than a memory block published")
	}
	free.RamSize = pubBlockSize
	// a block of 512 MiB is not sure to fit 512 MiB
	devInfos = BuildDevices(cfg, devices, pool, nil, &localRecords{Memdevs: local.Memdevs, BlockSize: pubBlockSize * 4})
	if _, ok := devInfos["memdev-a1"]; ok {
		t.Fatal("a memdev of one large block published")
	}
	m, ok := devInfos["memdev-a5"].Dev.(*Memdev)
	if !ok {
		t.Fatalf("used memdev %+v", devInfos["memdev-a5"])
	}
	if c := LocalMemdevDevice("x", m).Capacity["memory"]; c.Value.Value() != 512*pubMiB {
		t.Fatalf("capacity %v of a 1 GiB memdev with 512 MiB blocks", c.Value)
	}
	devInfos = BuildDevices(cfg, devices, pool, nil, &localRecords{Memdevs: local.Memdevs})
	if got := pubNames(devInfos); !slices.Equal(got, []string{"dram", "region0-numa2"}) {
		t.Fatalf("published %v without a block size", got)
	}
	// the node of the plugin's region is not DRAM
	if dram, ok := devInfos["dram"].Dev.(*DRAM); !ok || !slices.Equal(dram.NUMANodes, []int{0}) {
		t.Fatalf("DRAM %+v", devInfos["dram"].Dev)
	}

	// manageMemdevs: false publishes regions only
	cfg.ManageMemdevs = false
	devInfos = BuildDevices(cfg, devices, pool, nil, nil)
	if got := pubNames(devInfos); !slices.Equal(got, []string{"dram", "region0-numa2", "region1-numa3"}) {
		t.Fatalf("published %v without managed memdevs", got)
	}
}

// TestGetResourcesLocalMemdev pins the published device of a local memdev:
// its attributes, its capacity with the request policy of memory blocks,
// no numaNode when unknown.
func TestGetResourcesLocalMemdev(t *testing.T) {
	devs := BuildDevices(PublishConfig{IgnoreDRAM: true, ManageMemdevs: true}, &kernel.Snapshot{Memdevs: []kernel.MemdevInfo{pubMemdev()}},
		nil, nil, &localRecords{BlockSize: pubBlockSize})
	p := pubPublisher(t, publishOptions{NodeName: pubNode})
	p.SetDevices(devs)
	got := p.Resources().Pools[pubNode].Slices[0].Devices
	q := func(v int64) *resource.Quantity { return resource.NewQuantity(v, resource.BinarySI) }
	want := []resourcev1.Device{{
		Name: pubLocalDeviceName,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type":                            {StringValue: ptr("memdev")},
			"source":                          {StringValue: ptr("local")},
			"serial":                          {StringValue: ptr("0xc100e2e1")},
			"shared":                          {BoolValue: ptr(false)},
			"persistent":                      {BoolValue: ptr(false)},
			"size":                            {IntValue: ptr(int64(pubRegionSize))},
			"resource.kubernetes.io/numaNode": {IntValue: ptr(int64(0))},
			"memoryBlockSize":                 {IntValue: ptr(int64(134217728))},
		},
		Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			"memory": {
				Value: *q(pubRegionSize),
				RequestPolicy: &resourcev1.CapacityRequestPolicy{
					Default:    q(pubRegionSize),
					ValidRange: &resourcev1.CapacityRequestPolicyRange{Min: q(134217728), Step: q(134217728)},
				},
			},
		},
		NodeAllocatableResources: memoryNodeAllocatableResources(),
		AllowMultipleAllocations: ptr(true),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published\n%+v\nexpected\n%+v", got, want)
	}
	if p := got[0].Capacity["memory"].RequestPolicy; p.Default.String() != "1Gi" || p.ValidRange.Min.String() != "128Mi" || p.ValidRange.Step.String() != "128Mi" {
		t.Fatalf("request policy %v %v %v", p.Default, p.ValidRange.Min, p.ValidRange.Step)
	}
	// no numaNode when unknown
	d := LocalMemdevDevice("x", &Memdev{Size: pubRegionSize, NUMAAffinity: -1, BlockSize: pubBlockSize})
	if _, ok := d.Attributes["resource.kubernetes.io/numaNode"]; ok {
		t.Fatalf("memdev %+v", d)
	}
}

// TestGetResourcesRegionDRAMLease pins the devices of regions, DRAM and the
// lease device: every memory device has type, source and size, a region
// the numaNode of its memdevs when they agree.
func TestGetResourcesRegionDRAMLease(t *testing.T) {
	devices := &kernel.Snapshot{NUMANodes: pubNodes(0, 1, 2, 3), Memdevs: []kernel.MemdevInfo{
		pubMem("mem1", 1, 1<<30, 0), pubMem("mem2", 2, 1<<30, 0), pubMem("mem3", 3, 1<<30, 0), pubMem("mem4", 4, 1<<30, 1),
	}}
	devices.NUMANodes[2].MemTotal = 2 << 30
	pubRegion(devices, "region0", 2, 2<<30, 1, 2)
	pubRegion(devices, "region1", 3, 1<<30, 3, 4)
	devs := BuildDevices(PublishConfig{PublishLeaseDevice: true}, devices, nil, nil, nil)
	p := pubPublisher(t, publishOptions{NodeName: pubNode})
	p.SetDevices(devs)
	got := map[string]resourcev1.Device{}
	for _, d := range p.Resources().Pools[pubNode].Slices[0].Devices {
		got[d.Name] = d
	}
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, []string{"dram", "lease", "region0-numa2", "region1-numa3"}) {
		t.Fatalf("published %v", names)
	}
	wantAttrs := map[string]map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		"region0-numa2": {"type": {StringValue: ptr("region")}, "source": {StringValue: ptr("local")}, "memoryNUMANode": {IntValue: ptr(int64(2))},
			"size": {IntValue: ptr(int64(2 << 30))}, "resource.kubernetes.io/numaNode": {IntValue: ptr(int64(0))}},
		// the memdevs of the region do not agree: no numaNode
		"region1-numa3": {"type": {StringValue: ptr("region")}, "source": {StringValue: ptr("local")}, "memoryNUMANode": {IntValue: ptr(int64(3))},
			"size": {IntValue: ptr(int64(1 << 30))}},
		"dram": {"type": {StringValue: ptr("dram")}, "source": {StringValue: ptr("local")}, "memoryNUMANodes": {StringValue: ptr("0-1")},
			"size": {IntValue: ptr(int64(2 << 30))}},
		"lease": {"type": {StringValue: ptr("lease")}},
	}
	for name, want := range wantAttrs {
		if !reflect.DeepEqual(got[name].Attributes, want) {
			t.Errorf("%s: attributes %+v, expected %+v", name, got[name].Attributes, want)
		}
	}
	for _, name := range []string{"region0-numa2", "region1-numa3", "dram"} {
		d := got[name]
		c := d.Capacity["memory"]
		if c.Value.Value() != *d.Attributes["size"].IntValue || d.NodeAllocatableResources == nil || !*d.AllowMultipleAllocations {
			t.Errorf("%s: capacity %+v", name, d)
		}
	}
	if l := got["lease"]; l.Capacity != nil || !*l.AllowMultipleAllocations {
		t.Errorf("lease device %+v", l)
	}
	if _, ok := regionNUMAAffinity(&Region{NUMAAffinity: []int{-1}}); ok {
		t.Error("an unknown affinity is published")
	}
}

// TestDRAMNodeOrder pins the order of the nodes of the DRAM, which a
// Prepare records: that of their names.
func TestDRAMNodeOrder(t *testing.T) {
	devs := BuildDevices(PublishConfig{}, &kernel.Snapshot{NUMANodes: pubNodes(0, 1, 2, 10)}, nil, nil, nil)
	if dram, ok := devs["dram"].Dev.(*DRAM); !ok || !slices.Equal(dram.NUMANodes, []int{0, 1, 10, 2}) {
		t.Fatalf("DRAM %+v", devs["dram"].Dev)
	}
}

// TestLocalRequestPolicy pins the request policy of a local memdev: steps
// of the memory block size, the whole device (its blocks) by default.
func TestLocalRequestPolicy(t *testing.T) {
	q := func(v int64) *resource.Quantity { return resource.NewQuantity(v, resource.BinarySI) }
	for _, tc := range []struct {
		size, bs uint64
		want     *resourcev1.CapacityRequestPolicy
	}{
		{pubRegionSize, pubBlockSize, &resourcev1.CapacityRequestPolicy{Default: q(pubRegionSize),
			ValidRange: &resourcev1.CapacityRequestPolicyRange{Min: q(pubBlockSize), Step: q(pubBlockSize)}}},
		// rounded down to whole blocks
		{pubRegionSize + 64*pubMiB, pubBlockSize, &resourcev1.CapacityRequestPolicy{Default: q(pubRegionSize),
			ValidRange: &resourcev1.CapacityRequestPolicyRange{Min: q(pubBlockSize), Step: q(pubBlockSize)}}},
		{2 * pubBlockSize, pubBlockSize, &resourcev1.CapacityRequestPolicy{Default: q(2 * pubBlockSize),
			ValidRange: &resourcev1.CapacityRequestPolicyRange{Min: q(pubBlockSize), Step: q(pubBlockSize)}}},
		// one block: a range needs room for min and one step
		{pubBlockSize + 64*pubMiB, pubBlockSize, &resourcev1.CapacityRequestPolicy{Default: q(pubBlockSize),
			ValidValues: []resource.Quantity{*q(pubBlockSize)}}},
		{pubRegionSize, 2 << 30, nil},
		{pubRegionSize, 0, nil},
		// blocks larger than the region alignment: the head that a
		// region can have (block size - 256 MiB) is not counted
		{2 << 30, 512 * pubMiB, &resourcev1.CapacityRequestPolicy{Default: q(1536 * pubMiB),
			ValidRange: &resourcev1.CapacityRequestPolicyRange{Min: q(512 * pubMiB), Step: q(512 * pubMiB)}}},
		{768 * pubMiB, 512 * pubMiB, &resourcev1.CapacityRequestPolicy{Default: q(512 * pubMiB),
			ValidValues: []resource.Quantity{*q(512 * pubMiB)}}},
		{512 * pubMiB, 512 * pubMiB, nil},
		{4 << 30, 2 << 30, &resourcev1.CapacityRequestPolicy{Default: q(2 << 30),
			ValidValues: []resource.Quantity{*q(2 << 30)}}},
	} {
		if got := localRequestPolicy(tc.size, tc.bs); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("size %d, block size %d: policy %+v, expected %+v", tc.size, tc.bs, got, tc.want)
		}
	}
}

// A region of a memdev of the pool driver is not published and its node
// is not DRAM; without the pool driver it is a region device.
func TestBuildDevInfosSkipsPoolRegions(t *testing.T) {
	devices := &kernel.Snapshot{NUMANodes: pubNodes(0, 2, 3), Memdevs: []kernel.MemdevInfo{pubMem("mem0", 0xa1, 0, -1), pubMem("mem1", 0xa2, 0, -1)}}
	pubRegion(devices, "region0", 2, 1<<30, 0xa1)
	pubRegion(devices, "region1", 3, 1<<30, 0xa2)
	devInfos := BuildDevices(PublishConfig{}, devices, map[uint64]bool{0xa2: true}, nil, nil)
	if got := pubNames(devInfos); !slices.Equal(got, []string{"dram", "region0-numa2"}) {
		t.Fatalf("published %v", got)
	}
	if dram, ok := devInfos["dram"].Dev.(*DRAM); !ok || !slices.Equal(dram.NUMANodes, []int{0}) {
		t.Fatalf("DRAM %+v", devInfos["dram"].Dev)
	}
	if got := pubNames(BuildDevices(PublishConfig{}, devices, nil, nil, nil)); !slices.Equal(got, []string{"dram", "region0-numa2", "region1-numa3"}) {
		t.Fatalf("published %v without the pool driver", got)
	}
}

// TestRegionOnlineMemory: a region device has the node and the bytes of
// the online memory of its region; a region without online memory or
// disabled is not published, and its node is not DRAM either.
func TestRegionOnlineMemory(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	s := &kernel.Snapshot{NUMANodes: pubNodes(0, 2, 3, 4)}
	pubRegion(s, "region0", 2, 4*pubBlockSize)
	pubRegion(s, "region1", 3, pubBlockSize)
	pubRegion(s, "region2", 4, pubBlockSize)
	s.Blocks[0].State, s.Blocks[3].State, s.Blocks[4].State = kernel.BlockOffline, kernel.BlockOffline, kernel.BlockOffline
	s.Regions[2].State = kernel.RegionDisabled
	devs := BuildDevices(PublishConfig{}, s, nil, nil, nil)
	if got := pubNames(devs); !slices.Equal(got, []string{"dram", "region0-numa2"}) {
		t.Fatalf("published %v", got)
	}
	if r := devs["region0-numa2"].Dev.(*Region); r.Size != 2*pubBlockSize {
		t.Fatalf("region0 of %d bytes, expected its 2 online blocks", r.Size)
	}
	if dram := devs[device.DRAMDeviceName].Dev.(*DRAM); !slices.Equal(dram.NUMANodes, []int{0}) {
		t.Fatalf("DRAM nodes %v", dram.NUMANodes)
	}
	log := logs()
	wantLogLineText(t, log, `- ignoring region device "region1": region has no online memory`)
	wantLogLineText(t, log, `- ignoring region device "region2": region is disabled`)
}

// TestBuildDevInfosHollowRegion pins a hollow region of a scan: it is not
// published, the other devices are, and it is warned about once while it
// stays, unless it is one of the device engine. The hollow region still
// has a node: memory of its address range that kmem could not remove is
// online, so the scan finds the node; that node is not DRAM either.
func TestBuildDevInfosHollowRegion(t *testing.T) {
	devices := &kernel.Snapshot{NUMANodes: pubNodes(0, 2, 3), Memdevs: []kernel.MemdevInfo{pubMem("mem0", 0xc100e2e0, 0, -1)}, BlockSize: 1 << 28}
	devices.NUMANodes[2].MemTotal = 1 << 28
	pubRegion(devices, "region0", 2, 1<<30, 0xc100e2e0)
	devices.Regions = append(devices.Regions, kernel.RegionInfo{Name: "region1", State: kernel.RegionHollow, Mode: kernel.RegionRAM,
		Range: kernel.Range{Start: 0x850000000, End: 0x850000000 + 1<<28 - 1}, NUMANode: 3, RootDecoder: "decoder0.1"})
	devices.Blocks = append(devices.Blocks, kernel.MemoryBlock{Index: 0x85, State: kernel.BlockOnline, Zones: "Movable", NUMANode: 3})
	devInfos := BuildDevices(PublishConfig{ManageMemdevs: true}, devices, nil, nil, nil)
	if devInfos["region0-numa2"] == nil || devInfos["region1-numa3"] != nil || len(devInfos) != 2 {
		t.Fatalf("devices %v", pubNames(devInfos))
	}
	if dram, ok := devInfos["dram"].Dev.(*DRAM); !ok || !slices.Equal(dram.NUMANodes, []int{0}) {
		t.Fatalf("DRAM %+v", devInfos["dram"])
	}

	logs := klogtest.Capture(t, 0)
	h := &hollowRegionLog{warned: map[string]bool{}}
	notOurs := func(string) bool { return false }
	for i, want := range []int{1, 1, 0} {
		regions := devices.Regions
		if want == 0 {
			regions = regions[:1]
		}
		if h.observe(regions, notOurs); len(h.warned) != want {
			t.Fatalf("scan %d: warned %v", i, h.warned)
		}
	}
	h.observe(devices.Regions, func(name string) bool { return name == "region1" })
	if len(h.warned) != 0 {
		t.Fatalf("a hollow region of the engine warned %v", h.warned)
	}
	out := logs()
	want := "CXL region region1 (mode ram, resource 0x850000000, size 0x10000000) has no target decoder: it is not published."
	if n := strings.Count(out, want); n != 1 || !strings.Contains(out, "echo region1 > /sys/bus/cxl/devices/decoder0.1/delete_region") {
		t.Fatalf("%d warnings of the hollow region:\n%s", n, out)
	}
}

// TestParseMemdevSpec pins the forms of a memdev spec and the memdevs that
// each matches: a serial is decimal or 0x hex of any case, never bare hex.
func TestParseMemdevSpec(t *testing.T) {
	mems := []kernel.MemdevInfo{
		{Name: "mem0", Serial: 0xc100e2e0, Major: 251, Minor: 0},
		{Name: "mem1", Serial: 0x100, Major: 251, Minor: 1},
	}
	for spec, want := range map[string]string{
		"mem0":       `mem0: memdev name "mem0"`,
		"cxl/mem1":   `mem1: memory uevent device name "cxl/mem1"`,
		"251:0":      `mem0: memdev with major:minor "251:0"`,
		"3238060768": `mem0: memdev serial "3238060768"`,
		"0xc100e2e0": `mem0: memdev serial "0xc100e2e0"`,
		"0XC100E2E0": `mem0: memdev serial "0XC100E2E0"`,
		"256":        `mem1: memdev serial "256"`,
		"100":        "",
		"mem2":       "",
	} {
		s, err := parseMemdevSpec(spec)
		mustOK(t, err)
		var got []string
		for i := range mems {
			if m := s.Match(&mems[i]); m != "" {
				got = append(got, mems[i].Name+": "+m)
			}
		}
		if strings.Join(got, ", ") != want {
			t.Errorf("%q matches %q, expected %q", spec, got, want)
		}
	}
	for _, spec := range []string{"", "a5", "c100e2e0", "mem", "memx", "mem-1", "cxl/", "cxl/a5", "251:", "251:x", "-1", "0x", "0xg"} {
		if _, err := parseMemdevSpec(spec); err == nil {
			t.Errorf("%q parsed", spec)
		}
	}
}

// TestSharedLocalPrecedence pins the precedence per memdev: ignoreMemdevs
// over pool devices over sharedMemdevs over unshared local memdevs; any spec form of
// ignoreMemdevs and sharedMemdevs matches; a memdev used one way by claims
// is not published the other way until it has none.
func TestSharedLocalPrecedence(t *testing.T) {
	devices := &kernel.Snapshot{}
	for i := range 10 {
		m := pubMem(fmt.Sprintf("mem%d", i+1), 0xa1+uint64(i), 1<<30, -1)
		m.Major, m.Minor = 251, int(m.Serial&0xf)
		devices.Memdevs = append(devices.Memdevs, m)
	}
	byName, ignored, pooled, unshared := &devices.Memdevs[0], &devices.Memdevs[4], &devices.Memdevs[5], &devices.Memdevs[6]
	pubRegion(devices, "region9", 3, 1<<30, 0xaa)
	cfg := PublishConfig{
		IgnoreDevices: memdevSpecs(t, "0xa5"),
		SharedDevices: memdevSpecs(t, "mem1", "cxl/mem2", "251:3", "164", "0XA5", "0xa6", "0xa8", "0xaa"),
		ManageMemdevs: true,
	}
	pool := map[uint64]bool{0xa6: true}
	local := &localRecords{BlockSize: pubBlockSize, Memdevs: map[uint64]localMemdev{
		0xa8: {Claims: []string{"s1"}},
		0xa9: {Shared: true, Claims: []string{"h1"}},
	}}
	devInfos := BuildDevices(cfg, devices, pool, nil, local)
	shared := map[string]bool{}
	for name, d := range devInfos {
		if m, ok := d.Dev.(*Memdev); ok {
			shared[name] = m.Shared
		}
	}
	want := map[string]bool{"memdev-a1": true, "memdev-a2": true, "memdev-a3": true, "memdev-a4": true, "memdev-a7": false}
	if !maps.Equal(shared, want) {
		t.Fatalf("published memdevs (name: shared) %v, expected %v", shared, want)
	}
	for _, tc := range []struct {
		mem  *kernel.MemdevInfo
		want string
	}{
		{ignored, `ignored by memdev serial "0xa5"`},
		{pooled, "pool device"},
		{byName, ""},
		{unshared, ""},
	} {
		if _, got := sharedListing(cfg, tc.mem, pool, nil); got != tc.want {
			t.Errorf("%s: refusal %q, expected %q", tc.mem.Name, got, tc.want)
		}
	}
	if got := sharedSerials(cfg, devices.Memdevs, pool); !maps.Equal(got, map[uint64]bool{0xa1: true, 0xa2: true, 0xa3: true, 0xa4: true, 0xa8: true, 0xaa: true}) {
		t.Fatalf("shared serials %v", got)
	}

	// the hidden ones come back when the other use has no claims
	local.Memdevs[0xa8] = localMemdev{Claims: []string{}}
	local.Memdevs[0xa9] = localMemdev{Shared: true, Claims: []string{}}
	devInfos = BuildDevices(cfg, devices, pool, nil, local)
	for name, s := range map[string]bool{"memdev-a8": true, "memdev-a9": false} {
		if m, ok := devInfos[name].Dev.(*Memdev); !ok || m.Shared != s {
			t.Errorf("%s: %+v, expected shared %v", name, devInfos[name], s)
		}
	}
}

// TestSharedRefusalLogged pins the line of a memdev of sharedMemdevs that
// is not published as a shared local device, as the documentation quotes
// it, logged once per reason: held, and a pool device.
func TestSharedRefusalLogged(t *testing.T) {
	logs := klogtest.Capture(t, 0)
	// logged once per process: again for this test
	for _, reason := range []string{heldReason, "pool device"} {
		sharedRefusalsLogged.Delete("mem31\x00" + reason)
	}
	cfg := PublishConfig{SharedDevices: memdevSpecs(t, "mem31"), ManageMemdevs: true, IgnoreDRAM: true}
	detected := &kernel.Snapshot{Memdevs: []kernel.MemdevInfo{pubMem("mem31", 0xa1f, 1<<30, -1)}}
	local := &localRecords{BlockSize: pubBlockSize}
	for range 2 {
		if devs := BuildDevices(cfg, detected, nil, map[uint64]bool{0xa1f: true}, local); len(devs) != 0 {
			t.Fatalf("published %v", pubNames(devs))
		}
	}
	if devs := BuildDevices(cfg, detected, map[uint64]bool{0xa1f: true}, nil, local); len(devs) != 0 {
		t.Fatalf("published %v", pubNames(devs))
	}
	out := logs()
	if n := logMatches(out, `- not publishing memdev "mem31" as a shared local device: `+heldReason); n != 1 {
		t.Fatalf("%d lines of the held refusal:\n%s", n, out)
	}
	if n := logMatches(out, `- not publishing memdev "mem31" as a shared local device: pool device`); n != 1 {
		t.Fatalf("%d lines of the pool refusal:\n%s", n, out)
	}
}

// TestBuildDevInfosPersistentMemdevs pins the persistent local memdevs that
// a scan publishes: a memdev of persistent memory only, shared when
// sharedMemdevs lists it; a memdev with RAM is an unshared one, and claims
// that use it as a persistent device keep it from being published.
func TestBuildDevInfosPersistentMemdevs(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	devices := &kernel.Snapshot{}
	for i, ram := range []uint64{0, 0, 1 << 30, 1 << 30} {
		m := pubMem(fmt.Sprintf("mem%d", i+1), 0xb1+uint64(i), ram, -1)
		m.PmemSize = 4 << 30
		devices.Memdevs = append(devices.Memdevs, m)
	}
	devices.Memdevs[0].NUMAAffinity = 1
	devices.Memdevs[3].PmemSize = 0
	cfg := PublishConfig{SharedDevices: memdevSpecs(t, "0xb2"), IgnoreDRAM: true, ManageMemdevs: true}
	local := &localRecords{BlockSize: pubBlockSize, Memdevs: map[uint64]localMemdev{
		0xb1: {Persistent: true},
		0xb4: {Persistent: true, Claims: []string{"u3"}},
	}}
	devs := BuildDevices(cfg, devices, nil, nil, local)
	if got := pubNames(devs); !slices.Equal(got, []string{"memdev-b1", "memdev-b2", "memdev-b3"}) {
		t.Fatalf("published %v", got)
	}
	if m := devs["memdev-b1"].Dev.(*Memdev); *m != (Memdev{Name: "mem1", Serial: 0xb1, Size: 4 << 30, NUMAAffinity: 1, Persistent: true}) {
		t.Fatalf("persistent memdev %+v", m)
	}
	if m := devs["memdev-b2"].Dev.(*Memdev); !m.Persistent || !m.Shared || m.Size != 4<<30 {
		t.Fatalf("shared persistent memdev %+v", m)
	}
	if m := devs["memdev-b3"].Dev.(*Memdev); m.Persistent || m.Shared || m.Size != 1<<30 {
		t.Fatalf("memdev with RAM and persistent memory %+v", m)
	}
	if n := logMatches(logs(), `- not publishing memdev "mem4" as a local memdev: used as a local-persistent device by claims [u3]`); n != 1 {
		t.Fatalf("%d lines of the refusal of mem4:\n%s", n, logs())
	}
}

// TestPersistentMemdevRefusals pins the line of a persistent memdev that is
// not published, as the documentation quotes it, at klog V(4): a pool
// device, a held memdev, a disabled one, an ignored one, one with a region
// of persistent memory that the driver did not create, one that claims use
// as slices or with the other sharing; one that claims use shared while it
// is not shared now is published again when they are gone.
func TestPersistentMemdevRefusals(t *testing.T) {
	logs := klogtest.Capture(t, 4)
	detected := &kernel.Snapshot{Memdevs: []kernel.MemdevInfo{pubMem("mem3", 0xc100f5e1, 0, -1)}}
	m := &detected.Memdevs[0]
	m.PmemSize = 512 * pubMiB
	cfg := PublishConfig{ManageMemdevs: true, IgnoreDRAM: true}
	local := &localRecords{BlockSize: pubBlockSize}
	if devs := BuildDevices(cfg, detected, nil, nil, local); !slices.Equal(pubNames(devs), []string{"memdev-c100f5e1"}) {
		t.Fatalf("published %v", pubNames(devs))
	}
	serial := map[uint64]bool{0xc100f5e1: true}
	sharedUse := map[uint64]localMemdev{0xc100f5e1: {Shared: true, Persistent: true, Claims: []string{"ro1"}}}
	exclusiveUse := map[uint64]localMemdev{0xc100f5e1: {Persistent: true, Claims: []string{"rw1"}}}
	sliceUse := map[uint64]localMemdev{0xc100f5e1: {Claims: []string{"s1"}}}
	for _, tc := range []struct {
		pool, held map[uint64]bool
		enabled    bool
		uses       map[uint64]localMemdev
		reason     string
		set        func(cfg *PublishConfig, s *kernel.Snapshot)
	}{
		{serial, nil, true, nil, "pool device", nil},
		{nil, serial, true, nil, heldReason, nil},
		{nil, nil, false, nil, "memdev is disabled", nil},
		{nil, nil, true, nil, `memdev name "mem3"`, func(cfg *PublishConfig, _ *kernel.Snapshot) {
			cfg.IgnoreDevices = memdevSpecs(t, "mem3")
		}},
		{nil, nil, true, nil, "it has regions [region0] that the driver did not create", func(_ *PublishConfig, s *kernel.Snapshot) {
			pubRegion(s, "region0", 2, 512*pubMiB, 0xc100f5e1)
			s.Regions[0].Mode = kernel.RegionPmem
		}},
		{nil, nil, true, sliceUse, "used as a local device by claims [s1]", nil},
		{nil, nil, true, sharedUse, "used as a shared local-persistent device by claims [ro1]", nil},
		{nil, nil, true, exclusiveUse, "used as a not shared local-persistent device by claims [rw1]", func(cfg *PublishConfig, _ *kernel.Snapshot) {
			cfg.SharedDevices = memdevSpecs(t, "mem3")
		}},
	} {
		m.State = map[bool]kernel.MemdevState{true: kernel.MemdevReady, false: kernel.MemdevUnbound}[tc.enabled]
		c, s := cfg, &kernel.Snapshot{Memdevs: slices.Clone(detected.Memdevs)}
		if tc.set != nil {
			tc.set(&c, s)
		}
		local.Memdevs = tc.uses
		if devs := BuildDevices(c, s, tc.pool, tc.held, local); len(devs) != 0 {
			t.Fatalf("%s: published %v", tc.reason, pubNames(devs))
		}
		if n := logMatches(logs(), `- not publishing memdev "mem3" as a persistent local memdev: `+tc.reason); n != 1 {
			t.Fatalf("%d lines with %q:\n%s", n, tc.reason, logs())
		}
	}
	local.Memdevs = map[uint64]localMemdev{0xc100f5e1: {Shared: true, Persistent: true, Claims: []string{}}}
	if m, ok := BuildDevices(cfg, detected, nil, nil, local)["memdev-c100f5e1"].Dev.(*Memdev); !ok || m.Shared || !m.Persistent {
		t.Fatalf("published %+v", m)
	}
}

// TestMemdevSettings pins the usable size of a local memdev from the
// configured usable size (memdevSettings, UsableSize of the configuration)
// and the probed size: the smaller limit, rounded down to 256 MiB.
func TestMemdevSettings(t *testing.T) {
	cfg := PublishConfig{UsableSize: func(memdev string, serial uint64) uint64 {
		switch {
		case serial == 0x0badc0dedeadbeef:
			return 128 << 30
		case memdev == "mem1":
			return 1000 << 20
		}
		return 0
	}}
	m0 := &kernel.MemdevInfo{Name: "mem0", Serial: 0x0badc0dedeadbeef, RamSize: 192 << 30}
	m1 := &kernel.MemdevInfo{Name: "mem1", Serial: 0x1, RamSize: 2 << 30}
	m2 := &kernel.MemdevInfo{Name: "mem2", Serial: 0x2, RamSize: 2 << 30}
	for _, tc := range []struct {
		m      *kernel.MemdevInfo
		probed uint64
		want   uint64
	}{
		{m0, 0, 128 << 30},
		{m0, 64 << 30, 64 << 30},
		{m1, 0, 768 << 20},
		{m2, 0, 2 << 30},
		{m2, 1 << 30, 1 << 30},
	} {
		snap := &localRecords{Probed: map[uint64]uint64{tc.m.Serial: tc.probed}}
		if got := memdevUsableSize(cfg, tc.m, snap); got != tc.want {
			t.Errorf("%s probed %d: usable size %d, expected %d", tc.m.Name, tc.probed, got, tc.want)
		}
	}
	if got := memdevUsableSize(PublishConfig{}, m2, nil); got != 2<<30 {
		t.Errorf("usable size %d without settings", got)
	}
}

// TestLocalSnapshot pins the snapshot of the local memdevs that the
// publication reads: the usable sizes of the address line probes by
// serial, and the memory block size, read for each snapshot so that one
// unknown at the start is known later; no snapshot without managed
// memdevs.
func TestLocalSnapshot(t *testing.T) {
	probes := func() map[state.Serial]state.Probe {
		return map[state.Serial]state.Probe{0x11: {UsableSize: 512 << 20}}
	}
	bs, err := uint64(0), errors.New("no block_size_bytes")
	blockSize := func() (uint64, error) { return bs, err }
	opts := publishOptions{NodeName: pubNode, Config: PublishConfig{ManageMemdevs: true}, Probes: probes, BlockSize: blockSize}
	p := pubPublisher(t, opts)
	if got := p.LocalRecords(); !maps.Equal(got.Probed, map[uint64]uint64{0x11: 512 << 20}) || got.BlockSize != 0 {
		t.Fatalf("probed %v, block size %d while it is unknown", got.Probed, got.BlockSize)
	}
	bs, err = 2<<30, nil
	if got := p.LocalRecords().BlockSize; got != 2<<30 {
		t.Fatalf("block size %d", got)
	}
	opts.Config.ManageMemdevs = false
	if off := pubPublisher(t, opts); off.LocalRecords() != nil {
		t.Fatal("a snapshot of unmanaged local memdevs")
	}
}

// TestRescanBeforeStart pins a Rescan before Start: it sets the devices and
// publishes nothing (no slice, no line of a publish); after Start a Rescan
// publishes them with its line.
func TestRescanBeforeStart(t *testing.T) {
	logs := klogtest.Capture(t, 0)
	client := resourceSliceTestClient(pubNode, nil)
	p := pubPublisher(t, publishOptions{
		Config:   PublishConfig{IgnoreDRAM: true, PublishLeaseDevice: true},
		NodeName: pubNode,
		Client:   client,
		Scan:     pubNoScan,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Rescan(ctx, false)
	if devs := p.Devices(); devs[device.LeaseDeviceName] == nil {
		t.Fatalf("devices after the rescan: %+v", devs)
	}
	list, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	pubNoErr(t, err)
	if len(list.Items) != 0 {
		t.Fatalf("published before the start: %+v", list.Items)
	}
	published := func() int { return logMatches(logs(), "publisher: published ") }
	if n := published(); n != 0 {
		t.Fatalf("%d lines of a publish before the start:\n%s", n, logs())
	}

	p.Start(ctx)
	defer p.Stop()
	waitNodeResourceSlice(t, client, pubNode, 5*time.Second, device.LeaseDeviceName)
	p.Rescan(ctx, false)
	if n := published(); n != 1 {
		t.Fatalf("%d lines of a publish after the start:\n%s", n, logs())
	}
	wantLogLineText(t, logs(), "publisher: published 1 devices")
}

// TestPublishedTable pins the table that a local Prepare looks its devices
// up in: that of the last publication, the last scan's before the
// first one; a scan that is not published leaves it.
func TestPublishedTable(t *testing.T) {
	client := resourceSliceTestClient(pubNode, nil)
	var dram atomic.Bool
	p := pubPublisher(t, publishOptions{
		Config:   PublishConfig{PublishLeaseDevice: true},
		NodeName: pubNode,
		Client:   client,
		Scan:     dramScan(&dram),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pubNoErr(t, p.Scan(ctx))
	if p.Published()[device.LeaseDeviceName] == nil {
		t.Fatalf("published table before the first publication: %+v", p.Published())
	}
	p.Start(ctx)
	defer p.Stop()
	waitNodeResourceSlice(t, client, pubNode, 5*time.Second, device.LeaseDeviceName)
	// a scan that finds the DRAM device, not published
	dram.Store(true)
	pubNoErr(t, p.Scan(ctx))
	if p.Devices()[device.DRAMDeviceName] == nil || p.Published()[device.DRAMDeviceName] != nil {
		t.Fatalf("scanned %v, published %v", p.Devices(), p.Published())
	}
	p.Rescan(ctx, false)
	if p.Published()[device.DRAMDeviceName] == nil {
		t.Fatalf("published %v after the rescan", p.Published())
	}
}
