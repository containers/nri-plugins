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
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"

	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// DeviceInfo is a device of the table of the publisher: its name in the
// ResourceSlice and the device behind it.
type DeviceInfo struct {
	Name string
	// Dev is a *DRAM, *Region, *Memdev or *Lease.
	Dev any
}

// devicesInfo are the devices of the table of the publisher by name.
type devicesInfo map[string]*DeviceInfo

// DRAM is the Dev of the DRAM device.
type DRAM struct {
	NUMANodes []int
	Size      uint64
}

// Region is the Dev of a region device (DeviceTypeRegion).
type Region struct {
	Name     string // regionN
	NUMANode int    // NUMA node of its online memory
	Size     uint64 // bytes of its online memory
	// NUMAAffinity are the CPU NUMA nodes of its memdevs, -1 for one not
	// known.
	NUMAAffinity []int
}

// Memdev is the Dev of a local memdev device (DeviceTypeMemdev).
type Memdev struct {
	Name         string // memN
	Serial       uint64
	Size         uint64 // RAM capacity, bytes (persistent capacity of a persistent memdev)
	NUMAAffinity int    // CPU NUMA node of the memdev, -1 if unknown
	BlockSize    uint64 // memory block size, bytes: slices are multiples of it
	Shared       bool   // published shared: one dax device for all claims
	// Persistent is true for a memdev of persistent memory: modes block
	// and fs, one pmem region for all claims.
	Persistent bool
}

// Lease is the Dev of the lease device: it has no hardware, no capacity
// and can be allocated any number of times.
type Lease struct{}

// localMemdev is how the device engine uses a local memdev.
type localMemdev struct {
	// Shared is true when the memdev is used as a shared device, false
	// when it is sliced; of a persistent device, whether it is shared.
	Shared bool
	// Persistent is true when the memdev is used as a persistent device.
	Persistent bool
	// Claims are the UIDs of the claims that use the memdev.
	Claims []string
}

// localRecords is what the publisher of local memdevs needs from the
// records of the device engine.
type localRecords struct {
	// Memdevs are the local memdevs that the engine has records of, by
	// serial.
	Memdevs map[uint64]localMemdev
	// BlockSize is the memory block size in bytes, 0 if unknown.
	BlockSize uint64
	// Probed are the usable sizes that the address line probe found, by
	// serial.
	Probed map[uint64]uint64
}

// ProbedSize returns the usable size that the address line probe found for
// the memdev with the serial, 0 if none.
func (l *localRecords) ProbedSize(serial uint64) uint64 {
	if l == nil {
		return 0
	}
	return l.Probed[serial]
}

// known tells whether the engine uses the local memdev with the serial.
func (l *localRecords) known(serial uint64) bool {
	if l == nil {
		return false
	}
	_, ok := l.Memdevs[serial]
	return ok
}

// memdev returns how the engine uses the local memdev with the serial, and
// whether it uses it.
func (l *localRecords) memdev(serial uint64) (localMemdev, bool) {
	if l == nil {
		return localMemdev{}, false
	}
	u, ok := l.Memdevs[serial]
	return u, ok
}

// claims returns the claims of the local memdevs that the engine uses, by
// serial.
func (l *localRecords) claims() map[uint64][]string {
	if l == nil {
		return nil
	}
	out := map[uint64][]string{}
	for s, u := range l.Memdevs {
		out[s] = u.Claims
	}
	return out
}

// localUsableSize returns the bytes of whole memory blocks of bs bytes that
// a region on a memdev of size bytes is sure to hold, wherever in its
// window it starts.
func localUsableSize(size, bs uint64) uint64 {
	if bs == 0 {
		return 0
	}
	if bs > kernel.RegionAlign {
		head := bs - kernel.RegionAlign
		if size <= head {
			return 0
		}
		size -= head
	}
	return size / bs * bs
}

// hollowRegions are the hollow regions of the last scan, warned about.
var hollowRegions = &hollowRegionLog{warned: map[string]bool{}}

// hollowRegionLog warns once about each hollow region of the scans that is
// not one of the device engine (the engine warns about its own).
//
// Identity: one per process (hollowRegions), shared by the scans.
//
// States: none but the set warned, the hollow regions of the last scan.
//
// Transitions: each scan replaces warned with its hollow regions; a region
// is warned about when it enters the set.
//
// Persistence: none: a restart warns again.
//
// Locks and waits: mu guards warned, held for one observe.
type hollowRegionLog struct {
	// mu guards warned: a leaf lock.
	mu     sync.Mutex
	warned map[string]bool // name and resource of a hollow region
}

// observe logs a warning for each hollow region among regions that is not
// ours and that the last scan did not have: no decoder maps its address
// range any more (memory of it that kmem could not remove stays online
// until a reboot, which is why such a region may still have a node), it
// holds that range in its root decoder, and cxl destroy-region cannot
// remove it; with the command that removes it.
func (h *hollowRegionLog) observe(regions []kernel.RegionInfo, ours func(name string) bool) {
	seen := map[string]bool{}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range regions {
		if r.State != kernel.RegionHollow || ours(r.Name) {
			continue
		}
		key := fmt.Sprintf("%s@%#x", r.Name, r.Range.Start)
		seen[key] = true
		if h.warned[key] {
			continue
		}
		decoder := r.RootDecoder
		if decoder == "" {
			decoder = "<its root decoder>"
		}
		klog.Warningf("CXL region %s (mode %s, resource %#x, size %#x) has no target decoder: it is not published. It is left behind when its memory device was removed before the region was destroyed (or is being created right now). No decoder maps its address range, which it holds; cxl destroy-region cannot remove it. If it stays and none of its memory blocks is online: echo %s > /sys/bus/cxl/devices/%s/delete_region",
			r.Name, r.Mode, r.Range.Start, regionSize(r), r.Name, decoder)
	}
	h.warned = seen
}

// regionSize returns the size of a region in bytes, 0 while it has no
// address range.
func regionSize(r kernel.RegionInfo) uint64 {
	if r.Range == (kernel.Range{}) {
		return 0
	}
	return r.Range.Size()
}

// regionMemory returns the NUMA node and the bytes of the online memory of
// a region of the snapshot: the node of its first online memory block, -1
// and 0 if none is online.
func regionMemory(snap *kernel.Snapshot, r *kernel.RegionInfo) (int, uint64) {
	node, size := -1, uint64(0)
	for _, i := range r.Range.Blocks(snap.BlockSize) {
		if b := snap.Block(i); b.State == kernel.BlockOnline && b.NUMANode >= 0 {
			if node < 0 {
				node = b.NUMANode
			}
			size += snap.BlockSize
		}
	}
	return node, size
}

// numaNodesByName returns the NUMA nodes of the snapshot in the order of
// their names (node10 before node2).
func numaNodesByName(snap *kernel.Snapshot) []kernel.NUMANode {
	nodes := slices.Clone(snap.NUMANodes)
	slices.SortStableFunc(nodes, func(a, b kernel.NUMANode) int { return strings.Compare(strconv.Itoa(a.ID), strconv.Itoa(b.ID)) })
	return nodes
}

// regionMemdevs returns the memdevs of the targets of a region, in the
// order of the names of their endpoints.
func regionMemdevs(snap *kernel.Snapshot, r *kernel.RegionInfo) []kernel.MemdevInfo {
	out := make([]kernel.MemdevInfo, 0, len(r.Serials))
	for _, s := range r.Serials {
		out = append(out, snap.Memdev(s))
	}
	slices.SortStableFunc(out, func(a, b kernel.MemdevInfo) int { return strings.Compare(a.Endpoint, b.Endpoint) })
	return out
}

// BuildDevices returns the devices to publish of a snapshot, by device
// name. poolSerials are the serials of pool devices (prepared by the
// pool driver, published by a pool controller, or remembered as such): their
// regions are skipped, and they are never local memdevs; held are the
// serials of new memdevs that are held as possible pool devices: like pool
// devices, neither they nor their regions are published. Regions on memdevs
// that the device engine uses (local) are skipped too: the engine created
// them, and the memdevs are published as memdev devices.
func BuildDevices(cfg PublishConfig, snap *kernel.Snapshot, poolSerials, held map[uint64]bool, local *localRecords) devicesInfo {
	cxlNUMANodes := make(map[int]bool)
	devInfos := make(map[string]*DeviceInfo)
	for i := range snap.Regions {
		r := &snap.Regions[i]
		node, size := regionMemory(snap, r)
		klog.V(3).Infof("discovered CXL region device: %+v", *r)
		// the memory of a region is not DRAM, published or not, hollow
		// with memory that kmem could not remove included
		cxlNUMANodes[r.NUMANode] = true
		if r.State == kernel.RegionHollow {
			klog.V(4).Infof("- ignoring region device %q: it has no target decoder", r.Name)
			continue
		}
		mems := regionMemdevs(snap, r)
		if reason := regionIgnoreReason(cfg, r, node, mems, poolSerials, held, local); reason != "" {
			klog.V(4).Infof("- ignoring region device %q: %s", r.Name, reason)
			continue
		}
		dev := &Region{Name: r.Name, NUMANode: node, Size: size}
		for _, m := range mems {
			dev.NUMAAffinity = append(dev.NUMAAffinity, m.NUMAAffinity)
		}
		name := fmt.Sprintf("%s-numa%d", r.Name, node)
		devInfos[name] = &DeviceInfo{Name: name, Dev: dev}
	}
	if cfg.ManageMemdevs {
		addLocalMemdevs(devInfos, cfg, snap, poolSerials, held, local)
	}
	if !cfg.IgnoreDRAM {
		addDRAM(devInfos, cfg, snap, cxlNUMANodes)
	}
	if cfg.PublishLeaseDevice {
		devInfos[device.LeaseDeviceName] = &DeviceInfo{
			Name: device.LeaseDeviceName,
			Dev:  &Lease{},
		}
	}
	return devInfos
}

// regionIgnoreReason returns why a region with the memdevs mems and online
// memory on node (-1 for none) that is not hollow is not published, "" if
// it is: on a pool or held memdev, on a memdev of the engine, disabled, of
// persistent memory, without online memory, in ignoreRegions, on a node of
// ignoreNUMANodes, with a memdev of ignoreMemdevs; the first reason only.
func regionIgnoreReason(cfg PublishConfig, r *kernel.RegionInfo, node int, mems []kernel.MemdevInfo, poolSerials, held map[uint64]bool, local *localRecords) string {
	reason := poolIgnoreReason(mems, poolSerials, held)
	if reason == "" {
		reason = localIgnoreReason(mems, local)
	}
	switch {
	case reason != "":
		return reason
	case r.State != kernel.RegionEnabled:
		return "region is disabled"
	case r.Mode == kernel.RegionPmem:
		return "region of persistent memory"
	case node < 0:
		return "region has no online memory"
	}
	if slices.Contains(cfg.IgnoreRegions, r.Name) {
		return fmt.Sprintf("region name %q", r.Name)
	}
	if slices.Contains(cfg.IgnoreNUMANodes, node) {
		return fmt.Sprintf("region is on NUMA node %d", node)
	}
	for i := range mems {
		if m := memdevMatches(cfg.IgnoreDevices, &mems[i]); m != "" {
			return "region has a " + m
		}
	}
	return ""
}

// addDRAM adds the DRAM device of the NUMA nodes with memory that are not CXL
// NUMA nodes to devInfos.
func addDRAM(devInfos devicesInfo, cfg PublishConfig, snap *kernel.Snapshot, cxlNUMANodes map[int]bool) {
	dramDev := &DRAM{}
	for _, node := range numaNodesByName(snap) {
		if cxlNUMANodes[node.ID] {
			continue
		}
		ignoreReason := ""
		switch {
		case node.MemTotal == 0:
			ignoreReason = "node has 0 size"
		case slices.Contains(cfg.IgnoreNUMANodes, node.ID):
			ignoreReason = fmt.Sprintf("node ID %d is in ignoreNUMANodes", node.ID)
		}
		if ignoreReason != "" {
			klog.V(4).Infof("- ignoring DRAM on NUMA node %d: %s", node.ID, ignoreReason)
			continue
		}
		dramDev.NUMANodes = append(dramDev.NUMANodes, node.ID)
		dramDev.Size += node.MemTotal
		klog.V(4).Infof("discovered DRAM on NUMA node %d: size %d bytes", node.ID, node.MemTotal)
	}
	if dramDev.Size > 0 {
		devInfos[device.DRAMDeviceName] = &DeviceInfo{
			Name: device.DRAMDeviceName,
			Dev:  any(dramDev),
		}
		klog.V(3).Infof("discovered system DRAM device: %+v", dramDev)
	}
}

// poolIgnoreReason returns why a region on the memdevs mems, a pool device
// or a memdev that is held among them, is not published, "" if it is on
// neither.
func poolIgnoreReason(mems []kernel.MemdevInfo, poolSerials, held map[uint64]bool) string {
	for _, memDev := range mems {
		if poolSerials[memDev.Serial] {
			return fmt.Sprintf("pool device (%s), memdev 0x%x", device.PoolDriverName, memDev.Serial)
		}
		if held[memDev.Serial] {
			return fmt.Sprintf("memdev 0x%x is held: it may be a pool device", memDev.Serial)
		}
	}
	return ""
}

// heldReason is why a memdev that is held is not published.
const heldReason = "held: it may be a pool device"

// localIgnoreReason returns why a region on the memdevs mems, a local
// memdev that the engine uses among them, is not published, "" if it is not
// on one.
func localIgnoreReason(mems []kernel.MemdevInfo, local *localRecords) string {
	for _, memDev := range mems {
		if local.known(memDev.Serial) {
			return fmt.Sprintf("created by the driver on local memdev %s", MemdevDeviceName(memDev.Serial))
		}
	}
	return ""
}

// MemdevSpec is a memdev of the configuration: its name (mem0), its uevent
// device name (cxl/mem0), the <major>:<minor> of its character device
// (251:0) or its serial in decimal or 0x hex.
type MemdevSpec struct {
	spec         string
	form         memdevSpecForm
	name         string
	major, minor int
	serial       uint64
}

type memdevSpecForm int

const (
	specNone memdevSpecForm = iota
	specName
	specDevName
	specDevNum
	specSerial
)

// parseMemdevSpec returns the memdev of a spec of the configuration, an
// error if the spec is none of the forms of a MemdevSpec.
func parseMemdevSpec(spec string) (MemdevSpec, error) {
	s := MemdevSpec{spec: spec}
	if name, ok := strings.CutPrefix(spec, "cxl/"); ok && isMemdevName(name) {
		s.form, s.name = specDevName, name
	} else if isMemdevName(spec) {
		s.form, s.name = specName, spec
	} else if major, minor, ok := strings.Cut(spec, ":"); ok {
		ma, errMa := strconv.ParseUint(major, 10, 32)
		mi, errMi := strconv.ParseUint(minor, 10, 32)
		if errMa == nil && errMi == nil {
			s.form, s.major, s.minor = specDevNum, int(ma), int(mi)
		}
	} else if serial, err := api.ParseSerial(spec); err == nil {
		s.form, s.serial = specSerial, serial
	}
	if s.form == specNone {
		return s, fmt.Errorf("invalid memdev %q: not a name (mem0, cxl/mem0), major:minor (251:0) or serial in decimal or 0x hex", spec)
	}
	return s, nil
}

// ParseMemdevSpecs returns the memdevs of the specs, an error for the first
// invalid one.
func ParseMemdevSpecs(specs []string) ([]MemdevSpec, error) {
	var parsed []MemdevSpec
	for _, spec := range specs {
		s, err := parseMemdevSpec(spec)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, s)
	}
	return parsed, nil
}

// ParseMemdevID returns the memdev of a spec by its name or serial, an
// error for a <major>:<minor>, which MatchID cannot match.
func ParseMemdevID(spec string) (MemdevSpec, error) {
	s, err := parseMemdevSpec(spec)
	if err == nil && s.form == specDevNum {
		return s, fmt.Errorf("invalid memdev %q: not a name (mem0, cxl/mem0) or serial in decimal or 0x hex", spec)
	}
	return s, err
}

// isMemdevName tells whether a name is a memdev name: mem<N>.
func isMemdevName(name string) bool {
	n, ok := strings.CutPrefix(name, "mem")
	if !ok {
		return false
	}
	_, err := strconv.ParseUint(n, 10, 64)
	return err == nil
}

// Match returns how the spec matches a memdev, described, "" if it does
// not.
func (s MemdevSpec) Match(memDev *kernel.MemdevInfo) string {
	switch {
	case s.form == specName && s.name == memDev.Name:
		return fmt.Sprintf("memdev name %q", s.spec)
	case s.form == specDevName && s.name == memDev.Name:
		return fmt.Sprintf("memory uevent device name %q", s.spec)
	case s.form == specDevNum && s.major == memDev.Major && s.minor == memDev.Minor:
		return fmt.Sprintf("memdev with major:minor %q", s.spec)
	case s.form == specSerial && s.serial == memDev.Serial:
		return fmt.Sprintf("memdev serial %q", s.spec)
	}
	return ""
}

// MatchID tells whether the spec matches the memdev of the name and the
// serial; a <major>:<minor> spec does not.
func (s MemdevSpec) MatchID(name string, serial uint64) bool {
	switch s.form {
	case specName, specDevName:
		return s.name == name
	case specSerial:
		return s.serial == serial
	}
	return false
}

// memdevMatches returns how a spec of specs matches a memdev, described,
// "" if none does.
func memdevMatches(specs []MemdevSpec, memDev *kernel.MemdevInfo) string {
	for _, s := range specs {
		if m := s.Match(memDev); m != "" {
			return m
		}
	}
	return ""
}

// sharedListing tells whether SharedDevices of the configuration lists a
// memdev, and why a listed memdev is not a shared local device: it is
// ignored, a pool device or held ("" if it is one).
func sharedListing(cfg PublishConfig, memDev *kernel.MemdevInfo, poolSerials, held map[uint64]bool) (bool, string) {
	if memdevMatches(cfg.SharedDevices, memDev) == "" {
		return false, ""
	}
	if reason := memdevMatches(cfg.IgnoreDevices, memDev); reason != "" {
		return true, "ignored by " + reason
	}
	if poolSerials[memDev.Serial] {
		return true, "pool device"
	}
	if held[memDev.Serial] {
		return true, heldReason
	}
	return true, ""
}

// sharedSerials returns the serials of the memdevs that are shared local
// devices of the configuration: SharedDevices lists them, and they are
// neither ignored nor pool devices.
func sharedSerials(cfg PublishConfig, memDevs []kernel.MemdevInfo, poolSerials map[uint64]bool) map[uint64]bool {
	out := map[uint64]bool{}
	for i := range memDevs {
		memDev := &memDevs[i]
		if listed, refusal := sharedListing(cfg, memDev, poolSerials, nil); listed && refusal == "" {
			out[memDev.Serial] = true
		}
	}
	return out
}

// sharedRefusalsLogged has the refusals of shared local devices that are
// logged already, so that each is logged once.
var sharedRefusalsLogged sync.Map

// logSharedRefusal logs once why a memdev of SharedDevices is not published
// as a shared local device.
func logSharedRefusal(memDev *kernel.MemdevInfo, reason string) {
	if _, logged := sharedRefusalsLogged.LoadOrStore(memDev.Name+"\x00"+reason, true); !logged {
		klog.Infof("- not publishing memdev %q as a shared local device: %s", memDev.Name, reason)
	}
}

// addLocalMemdevs adds the local memdevs that the engine can use to devInfos.
// Per memdev, ignoreMemdevs takes precedence over pool devices, pool devices
// over sharedMemdevs, and sharedMemdevs over unshared local memdevs. A memdev
// of persistent memory only (no RAM capacity) is a persistent memdev, shared
// when sharedMemdevs lists it (addLocalPersistent). A shared memdev is
// published when it is enabled, has RAM capacity, no regions but the engine's
// and no claims of slices. An unshared local memdev is published when it is
// enabled, its RAM capacity is sure to hold a whole memory block, it has no
// regions but the engine's and no claims that use it shared or persistent.
func addLocalMemdevs(devInfos devicesInfo, cfg PublishConfig, snap *kernel.Snapshot, poolSerials, held map[uint64]bool, local *localRecords) {
	bs := uint64(0)
	if local != nil {
		bs = local.BlockSize
	}
	regions := map[uint64][]string{}
	for _, r := range snap.Regions {
		for _, s := range r.Serials {
			regions[s] = append(regions[s], r.Name)
		}
	}
	for i := range snap.Memdevs {
		memDev := &snap.Memdevs[i]
		listed, refusal := sharedListing(cfg, memDev, poolSerials, held)
		if refusal != "" {
			logSharedRefusal(memDev, refusal)
		}
		if memDev.PmemSize > 0 && memDev.RamSize == 0 {
			addLocalPersistent(devInfos, cfg, memDev, listed && refusal == "", poolSerials, held, regions[memDev.Serial], local)
			continue
		}
		if memDev.PmemSize > 0 {
			klog.V(3).Infof("- memdev %q: persistent capacity of a memdev with RAM capacity is not used", memDev.Name)
		}
		if listed && refusal == "" {
			addLocalShared(devInfos, memDev, memdevUsableSize(cfg, memDev, local), regions[memDev.Serial], local, bs)
			continue
		}
		size := memdevUsableSize(cfg, memDev, local)
		if reason := localRefusal(cfg, memDev, size, bs, poolSerials, held, regions[memDev.Serial], local); reason != "" {
			klog.V(4).Infof("- not publishing memdev %q as a local memdev: %s", memDev.Name, reason)
			continue
		}
		name := MemdevDeviceName(memDev.Serial)
		devInfos[name] = &DeviceInfo{
			Name: name,
			Dev: &Memdev{
				Name:         memDev.Name,
				Serial:       memDev.Serial,
				Size:         size,
				NUMAAffinity: memDev.NUMAAffinity,
				BlockSize:    bs,
			},
		}
		klog.V(3).Infof("discovered local memdev %s: %s serial 0x%x size %d (RAM capacity %d) memory block size %d", name, memDev.Name, memDev.Serial, size, memDev.RamSize, bs)
	}
}

// localRefusal returns why a memdev of usable size bytes with memory
// blocks of bs bytes is not published as an unshared local memdev, "" if it
// is: no RAM, an unknown block size, too small, disabled, a pool or held
// memdev, regions that the engine did not create, claims that use it
// persistent or shared, ignoreMemdevs.
func localRefusal(cfg PublishConfig, memDev *kernel.MemdevInfo, size, bs uint64, poolSerials, held map[uint64]bool, regions []string, local *localRecords) string {
	use, used := local.memdev(memDev.Serial)
	switch {
	case memDev.RamSize == 0:
		return "no RAM capacity"
	case bs == 0:
		return "the memory block size is unknown"
	case localUsableSize(size, bs) == 0:
		return fmt.Sprintf("usable RAM capacity %d is not sure to hold a whole memory block of %d bytes", size, bs)
	case memDev.State == kernel.MemdevUnbound:
		return "memdev is disabled"
	case poolSerials[memDev.Serial]:
		return "pool device"
	case held[memDev.Serial]:
		return heldReason
	case len(regions) > 0 && !used:
		return fmt.Sprintf("it has regions %v that the driver did not create", regions)
	case use.Persistent && len(use.Claims) > 0:
		return fmt.Sprintf("used as a local-persistent device by claims %v", use.Claims)
	case use.Shared && len(use.Claims) > 0:
		return fmt.Sprintf("used as a local-shared device by claims %v", use.Claims)
	}
	return memdevMatches(cfg.IgnoreDevices, memDev)
}

// memdevUsableSize returns the bytes of a local memdev that the engine
// uses: its RAM capacity, or less when memdevSettings or the address line
// probe limit it, rounded down to the alignment of CXL regions.
func memdevUsableSize(cfg PublishConfig, memDev *kernel.MemdevInfo, local *localRecords) uint64 {
	size := memDev.RamSize
	for _, limit := range []uint64{cfg.usableSize(memDev.Name, memDev.Serial), local.ProbedSize(memDev.Serial)} {
		if limit > 0 && limit < size {
			size = limit / kernel.RegionAlign * kernel.RegionAlign
		}
	}
	return size
}

// addLocalShared adds a memdev of sharedMemdevs to devInfos as a shared
// local device, unless it is disabled, has no RAM capacity, has regions that
// the engine did not create or claims that use it as slices.
//   - regions: the regions on the memdev
//   - bs: the memory block size, 0 if unknown
func addLocalShared(devInfos devicesInfo, memDev *kernel.MemdevInfo, size uint64, regions []string, local *localRecords, bs uint64) {
	use, used := local.memdev(memDev.Serial)
	reason := ""
	switch {
	case memDev.RamSize == 0:
		reason = "no RAM capacity"
	case memDev.State == kernel.MemdevUnbound:
		reason = "memdev is disabled"
	case len(regions) > 0 && !used:
		reason = fmt.Sprintf("it has regions %v that the driver did not create", regions)
	case use.Persistent && len(use.Claims) > 0:
		reason = fmt.Sprintf("used as a local-persistent device by claims %v", use.Claims)
	case !use.Shared && len(use.Claims) > 0:
		reason = fmt.Sprintf("used as a local device by claims %v", use.Claims)
	}
	if reason != "" {
		klog.V(4).Infof("- not publishing memdev %q as a shared local device: %s", memDev.Name, reason)
		return
	}
	name := MemdevDeviceName(memDev.Serial)
	devInfos[name] = &DeviceInfo{
		Name: name,
		Dev: &Memdev{
			Name:         memDev.Name,
			Serial:       memDev.Serial,
			Size:         size,
			NUMAAffinity: memDev.NUMAAffinity,
			BlockSize:    bs,
			Shared:       true,
		},
	}
	klog.V(3).Infof("discovered shared local memdev %s: %s serial 0x%x size %d", name, memDev.Name, memDev.Serial, memDev.RamSize)
}

// addLocalPersistent adds a memdev of persistent memory to devInfos as a
// persistent local device, shared when sharedMemdevs lists it, unless it is
// ignored, a pool device or held, disabled, has regions that the engine did
// not create, or claims that use it as slices, as a volatile shared device,
// or as a persistent device of the other sharing.
//   - shared: sharedMemdevs lists the memdev, which is neither ignored nor
//     a pool device
//   - regions: the regions on the memdev
func addLocalPersistent(devInfos devicesInfo, cfg PublishConfig, memDev *kernel.MemdevInfo, shared bool, poolSerials, held map[uint64]bool, regions []string, local *localRecords) {
	use, used := local.memdev(memDev.Serial)
	reason := memdevMatches(cfg.IgnoreDevices, memDev)
	switch {
	case reason != "":
	case poolSerials[memDev.Serial]:
		reason = "pool device"
	case held[memDev.Serial]:
		reason = heldReason
	case memDev.State == kernel.MemdevUnbound:
		reason = "memdev is disabled"
	case len(regions) > 0 && !used:
		reason = fmt.Sprintf("it has regions %v that the driver did not create", regions)
	case len(use.Claims) == 0:
	case !use.Persistent && use.Shared:
		reason = fmt.Sprintf("used as a local-shared device by claims %v", use.Claims)
	case !use.Persistent:
		reason = fmt.Sprintf("used as a local device by claims %v", use.Claims)
	case use.Shared != shared:
		reason = fmt.Sprintf("used as a %s local-persistent device by claims %v", sharedWord(use.Shared), use.Claims)
	}
	if reason != "" {
		klog.V(4).Infof("- not publishing memdev %q as a persistent local memdev: %s", memDev.Name, reason)
		return
	}
	name := MemdevDeviceName(memDev.Serial)
	devInfos[name] = &DeviceInfo{
		Name: name,
		Dev: &Memdev{
			Name:         memDev.Name,
			Serial:       memDev.Serial,
			Size:         memDev.PmemSize,
			NUMAAffinity: memDev.NUMAAffinity,
			Shared:       shared,
			Persistent:   true,
		},
	}
	klog.V(3).Infof("discovered persistent local memdev %s: %s serial 0x%x size %d shared %v", name, memDev.Name, memDev.Serial, memDev.PmemSize, shared)
}

// sharedWord returns "shared" or "not shared".
func sharedWord(shared bool) string {
	if shared {
		return "shared"
	}
	return "not shared"
}

// MemdevDeviceName returns the device name of a local memdev: "memdev-" +
// lower-case hex serial.
func MemdevDeviceName(serial uint64) string {
	return device.MemdevDevicePrefix + strconv.FormatUint(serial, 16)
}

// resources returns the devices of the node's ResourceSlices of the table,
// sorted by name.
func resources(devs devicesInfo) []resourcev1.Device {
	out := []resourcev1.Device{}
	for _, name := range slices.Sorted(maps.Keys(devs)) {
		allocatable := devs[name]
		if allocatable.Dev == nil {
			continue
		}
		switch dev := allocatable.Dev.(type) {
		case *Region:
			out = append(out, regionDevice(name, dev))
		case *DRAM:
			out = append(out, dramDevice(name, dev))
		case *Memdev:
			out = append(out, LocalMemdevDevice(name, dev))
		case *Lease:
			out = append(out, leaseDevice(name))
		default:
			klog.Warningf("unexpected device type in state.Allocatable: %T", dev)
		}
	}
	return out
}

// leaseDevice returns the ResourceSlice device of the lease device: no
// capacity, no node allocatable resources, any number of claims can
// allocate it.
func leaseDevice(name string) resourcev1.Device {
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			device.AttrType: {StringValue: ptr(device.DeviceTypeLease)},
		},
		AllowMultipleAllocations: ptr(true),
	}
}

// memoryCapacity returns the memory capacity of a device of size bytes.
func memoryCapacity(size uint64) map[resourcev1.QualifiedName]resourcev1.DeviceCapacity {
	return map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
		device.CapacityMemory: {Value: *resource.NewQuantity(int64(size), resource.BinarySI)},
	}
}

// regionDevice returns the ResourceSlice device of a region.
func regionDevice(name string, dev *Region) resourcev1.Device {
	attrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		device.AttrType:           {StringValue: ptr(device.DeviceTypeRegion)},
		device.AttrSource:         {StringValue: ptr(api.SourceLocal)},
		device.AttrMemoryNUMANode: {IntValue: ptr(int64(dev.NUMANode))},
		device.AttrSize:           {IntValue: ptr(int64(dev.Size))},
	}
	if a, ok := regionNUMAAffinity(dev); ok {
		attrs[device.AttrNUMANode] = resourcev1.DeviceAttribute{IntValue: ptr(int64(a))}
	}
	return resourcev1.Device{
		Name:                     name,
		Attributes:               attrs,
		Capacity:                 memoryCapacity(dev.Size),
		NodeAllocatableResources: memoryNodeAllocatableResources(),
		AllowMultipleAllocations: ptr(true),
	}
}

// regionNUMAAffinity returns the CPU NUMA node of the memdevs of a region,
// and whether they all have the same known one.
func regionNUMAAffinity(dev *Region) (int, bool) {
	affinity := -1
	for _, a := range dev.NUMAAffinity {
		switch {
		case a < 0:
			return -1, false
		case affinity < 0:
			affinity = a
		case a != affinity:
			return -1, false
		}
	}
	return affinity, affinity >= 0
}

// dramDevice returns the ResourceSlice device of the DRAM of the node.
func dramDevice(name string, dev *DRAM) resourcev1.Device {
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			device.AttrType:            {StringValue: ptr(device.DeviceTypeDRAM)},
			device.AttrSource:          {StringValue: ptr(api.SourceLocal)},
			device.AttrMemoryNUMANodes: {StringValue: ptr(cpuset.New(dev.NUMANodes...).String())},
			device.AttrSize:            {IntValue: ptr(int64(dev.Size))},
		},
		Capacity:                 memoryCapacity(dev.Size),
		NodeAllocatableResources: memoryNodeAllocatableResources(),
		AllowMultipleAllocations: ptr(true),
	}
}

// LocalMemdevDevice returns the ResourceSlice device of a local memdev.
func LocalMemdevDevice(name string, dev *Memdev) resourcev1.Device {
	if dev.Persistent || dev.Shared {
		return sharedMemdevDevice(name, dev)
	}
	attrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		device.AttrType:       {StringValue: ptr(device.DeviceTypeMemdev)},
		device.AttrSource:     {StringValue: ptr(api.SourceLocal)},
		device.AttrSerial:     {StringValue: ptr(api.FormatSerial(dev.Serial))},
		device.AttrShared:     {BoolValue: ptr(false)},
		device.AttrPersistent: {BoolValue: ptr(false)},
		device.AttrSize:       {IntValue: ptr(int64(dev.Size))},
		// every slice is a multiple of the memory block size
		device.AttrMemoryBlockSize: {IntValue: ptr(int64(dev.BlockSize))},
	}
	if dev.NUMAAffinity >= 0 {
		attrs[device.AttrNUMANode] = resourcev1.DeviceAttribute{IntValue: ptr(int64(dev.NUMAAffinity))}
	}
	return resourcev1.Device{
		Name:       name,
		Attributes: attrs,
		Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			device.CapacityMemory: {
				// the whole memory blocks that the region is sure to hold
				Value:         *resource.NewQuantity(int64(localUsableSize(dev.Size, dev.BlockSize)), resource.BinarySI),
				RequestPolicy: localRequestPolicy(dev.Size, dev.BlockSize),
			},
		},
		NodeAllocatableResources: memoryNodeAllocatableResources(),
		AllowMultipleAllocations: ptr(true),
	}
}

// sharedMemdevDevice returns the ResourceSlice device of a shared or a
// persistent local memdev: a shares capacity without a request policy, so
// that a claim that requests no shares has the device exclusively; no
// memory capacity and nothing node allocatable, as its memory is never
// onlined.
func sharedMemdevDevice(name string, dev *Memdev) resourcev1.Device {
	attrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		device.AttrType:       {StringValue: ptr(device.DeviceTypeMemdev)},
		device.AttrSource:     {StringValue: ptr(api.SourceLocal)},
		device.AttrSerial:     {StringValue: ptr(api.FormatSerial(dev.Serial))},
		device.AttrShared:     {BoolValue: ptr(dev.Shared)},
		device.AttrPersistent: {BoolValue: ptr(dev.Persistent)},
		device.AttrSize:       {IntValue: ptr(int64(dev.Size))},
	}
	if dev.NUMAAffinity >= 0 {
		attrs[device.AttrNUMANode] = resourcev1.DeviceAttribute{IntValue: ptr(int64(dev.NUMAAffinity))}
	}
	return resourcev1.Device{
		Name:       name,
		Attributes: attrs,
		Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			device.CapacityShares: {Value: *resource.NewQuantity(device.SharesUnlimited, resource.DecimalSI)},
		},
		AllowMultipleAllocations: ptr(true),
	}
}

// localRequestPolicy returns the memory capacity request policy of a local
// memdev of size bytes with memory blocks of bs bytes: a request is rounded
// up to a multiple of bs, and a request without an amount gets the whole
// blocks that the region is sure to hold (localUsableSize). A memdev of one
// such block has it as the only valid amount. Nil for a memdev without one.
func localRequestPolicy(size, bs uint64) *resourcev1.CapacityRequestPolicy {
	usable := localUsableSize(size, bs)
	if usable == 0 {
		return nil
	}
	whole := resource.NewQuantity(int64(usable), resource.BinarySI)
	if usable < 2*bs {
		// a range needs room for its min and one step
		return &resourcev1.CapacityRequestPolicy{Default: whole, ValidValues: []resource.Quantity{*whole}}
	}
	return &resourcev1.CapacityRequestPolicy{
		Default: whole,
		ValidRange: &resourcev1.CapacityRequestPolicyRange{
			Min:  resource.NewQuantity(int64(bs), resource.BinarySI),
			Step: resource.NewQuantity(int64(bs), resource.BinarySI),
		},
	}
}

// memoryNodeAllocatableResources maps the memory capacity that a claim
// consumes from a device to the node allocatable "memory" resource, so that
// kubelet accounts claimed memory in the pod and container cgroups. Memory
// capacity (device and node) is in bytes, hence capacityMultiplier is 1.
//
// Requires DRANodeAllocatableResources feature gate. Without it the
// apiserver drops the field from published ResourceSlices, and the
// publisher logs a warning once and publishes the devices without it.
func memoryNodeAllocatableResources() map[v1.ResourceName]resourcev1.NodeAllocatableResource {
	return map[v1.ResourceName]resourcev1.NodeAllocatableResource{
		v1.ResourceMemory: {
			Mapping: &resourcev1.NodeAllocatableMapping{
				CapacityKey:        ptr(device.CapacityMemory),
				CapacityMultiplier: ptr(resource.MustParse("1")),
			},
		},
	}
}
