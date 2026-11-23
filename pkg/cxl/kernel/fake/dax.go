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

package fake

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// The dax bus of the fake: the dynamic dax region of a ram region, its dax
// devices and the dax drivers with their id lists; and the memory blocks
// that kmem adds.

// daxRegionRel returns the sysfs directory of the dax region of a region,
// relative.
func daxRegionRel(region string) string {
	return "bus/cxl/devices/" + region + "/dax_" + region
}

// makeDaxRegion makes the dax region of a ram region with daxN.0 over all
// of its memory, bound to kmem as the kernel binds the dax device of a ram
// region, its memory blocks added; bound to device_dax instead when
// device_dax has its name as an id (left by a tool that bound a device of
// that name with new_id).
func (f *Fake) makeDaxRegion(region string) {
	w, rr, err := f.regionWindow(region)
	if err != nil {
		f.tb.Fatal(err)
	}
	attrs := daxRegionRel(region) + "/dax_region"
	f.Write(attrs+"/align", strconv.Itoa(DaxAlign))
	f.Write(attrs+"/create", "")
	f.Write(attrs+"/delete", "")
	f.Write(attrs+"/seed", "")
	dax := "dax" + strings.TrimPrefix(region, "region") + ".0"
	f.newDax(region, dax, w.NUMANode)
	f.setRanges(dax, []kernel.Range{rr})
	f.available(region)
	if f.HasDaxID(kernel.DriverDeviceDax, dax) {
		f.LinkTo(f.daxDir(dax)+"/driver", "bus/dax/drivers/device_dax")
		return
	}
	f.LinkTo(f.daxDir(dax)+"/driver", "bus/dax/drivers/kmem")
	f.addBlocks(dax)
}

// newDax makes a dax device of size 0, unbound.
func (f *Fake) newDax(region, dax string, node int) {
	d := daxRegionRel(region) + "/" + dax
	f.Write(d+"/size", "0")
	f.Write(d+"/target_node", strconv.Itoa(node))
	f.Write(d+"/dev", fmt.Sprintf("%d:%d", daxMajor, f.allocDaxMinor()))
	f.Write(d+"/resource", "0x0")
	f.Write(d+"/align", strconv.Itoa(DaxAlign))
	real, _ := filepath.EvalSymlinks(f.P(d))
	rel, _ := filepath.Rel(f.SysRoot(), real)
	f.LinkTo("bus/dax/devices/"+dax, rel)
}

// allocDaxMinor returns the lowest minor of a dax character device that is
// free, and takes it.
func (f *Fake) allocDaxMinor() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for m := 0; ; m++ {
		if !f.daxMinor[m] {
			f.daxMinor[m] = true
			return m
		}
	}
}

// freeDaxMinor frees the minor of a dax device.
func (f *Fake) freeDaxMinor(dax string) {
	_, mi, ok := strings.Cut(f.Read(f.daxDir(dax)+"/dev"), ":")
	if n, err := strconv.Atoi(mi); ok && err == nil {
		f.mu.Lock()
		delete(f.daxMinor, n)
		f.mu.Unlock()
	}
}

// daxDir returns the sysfs directory of a dax device, relative, through the
// link of the dax bus.
func (f *Fake) daxDir(dax string) string {
	return "bus/dax/devices/" + dax
}

// daxNames returns the dax devices of a region.
func (f *Fake) daxNames(region string) []string {
	m, _ := filepath.Glob(f.P(daxRegionRel(region) + "/dax*.*"))
	var out []string
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	slices.SortFunc(out, kernel.NatCompare)
	return out
}

// daxRegionOf returns the region of a dax device.
func (f *Fake) daxRegionOf(dax string) string {
	real, err := filepath.EvalSymlinks(f.P(f.daxDir(dax)))
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Dir(filepath.Dir(real)))
}

// DaxSize returns the size of a dax device.
func (f *Fake) DaxSize(dax string) uint64 {
	v, _ := strconv.ParseUint(f.Read(f.daxDir(dax)+"/size"), 10, 64)
	return v
}

// daxDriverOf returns the driver of a dax device, "" if none.
func (f *Fake) daxDriverOf(dax string) string {
	t, err := os.Readlink(f.P(f.daxDir(dax) + "/driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// DaxDriver returns the driver of a dax device, "" if none.
func (f *Fake) DaxDriver(dax string) string { return f.daxDriverOf(dax) }

// DaxExists tells whether a dax device exists.
func (f *Fake) DaxExists(dax string) bool { return f.Exists(f.daxDir(dax)) }

// HasDaxID tells whether a dax driver has the name of a device as an id.
func (f *Fake) HasDaxID(driver, dax string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[driver][dax]
}

// SetDaxID adds the name of a device to the ids of a dax driver, as a tool
// that bound it with new_id and left it does.
func (f *Fake) SetDaxID(driver, dax string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids[driver] == nil {
		f.ids[driver] = map[string]bool{}
	}
	f.ids[driver][dax] = true
}

// DaxRanges returns the memory of a dax device: its mappingN in address
// order.
func (f *Fake) DaxRanges(dax string) []kernel.Range {
	var out []kernel.Range
	m, _ := filepath.Glob(f.P(f.daxDir(dax) + "/mapping[0-9]*"))
	for _, p := range m {
		start, err1 := kernel.ParseHex(strings.TrimSpace(readFile(p + "/start")))
		end, err2 := kernel.ParseHex(strings.TrimSpace(readFile(p + "/end")))
		if err1 == nil && err2 == nil {
			out = append(out, kernel.Range{Start: start, End: end})
		}
	}
	slices.SortFunc(out, func(a, b kernel.Range) int { return cmp.Compare(a.Start, b.Start) })
	return out
}

// setRanges gives a dax device the memory of the ranges, in that order.
func (f *Fake) setRanges(dax string, ranges []kernel.Range) {
	d := f.daxDir(dax)
	m, _ := filepath.Glob(f.P(d + "/mapping*"))
	for _, dir := range m {
		_ = os.RemoveAll(dir)
	}
	size, start := uint64(0), uint64(0)
	for i, r := range ranges {
		f.Write(fmt.Sprintf("%s/mapping%d/start", d, i), fmt.Sprintf("0x%x", r.Start))
		f.Write(fmt.Sprintf("%s/mapping%d/end", d, i), fmt.Sprintf("0x%x", r.End))
		size += r.Size()
	}
	if len(ranges) > 0 {
		start = ranges[0].Start
	}
	f.Write(d+"/size", strconv.FormatUint(size, 10))
	f.Write(d+"/resource", fmt.Sprintf("0x%x", start))
}

// mapped returns the ranges of a dax device in mapping order.
func (f *Fake) mapped(dax string) []kernel.Range {
	var out []kernel.Range
	for i := 0; ; i++ {
		p := f.P(fmt.Sprintf("%s/mapping%d", f.daxDir(dax), i))
		start, err1 := kernel.ParseHex(strings.TrimSpace(readFile(p + "/start")))
		end, err2 := kernel.ParseHex(strings.TrimSpace(readFile(p + "/end")))
		if err1 != nil || err2 != nil {
			return out
		}
		out = append(out, kernel.Range{Start: start, End: end})
	}
}

// Gaps returns the memory of a region that no dax device has, in address
// order.
func (f *Fake) Gaps(region string) []kernel.Range {
	_, rr, err := f.regionWindow(region)
	if err != nil {
		return nil
	}
	var used []kernel.Range
	for _, dax := range f.daxNames(region) {
		used = append(used, f.DaxRanges(dax)...)
	}
	slices.SortFunc(used, func(a, b kernel.Range) int { return cmp.Compare(a.Start, b.Start) })
	var out []kernel.Range
	next := rr.Start
	for _, u := range used {
		if u.Start > next {
			out = append(out, kernel.Range{Start: next, End: u.Start - 1})
		}
		next = max(next, u.End+1)
	}
	if next <= rr.End {
		out = append(out, kernel.Range{Start: next, End: rr.End})
	}
	return out
}

// available returns the bytes of a region that no dax device holds, and
// writes it to dax_region/available_size.
func (f *Fake) available(region string) uint64 {
	free := uint64(0)
	for _, g := range f.Gaps(region) {
		free += g.Size()
	}
	f.Write(daxRegionRel(region)+"/dax_region/available_size", strconv.FormatUint(free, 10))
	return free
}

// regionOfAttrs returns the region of the attributes of a dax region
// (bus/cxl/devices/regionN/dax_regionN/dax_region).
func regionOfAttrs(dir string) string {
	parts := strings.Split(dir, "/")
	if len(parts) == 6 && parts[0] == "bus" && parts[2] == "devices" {
		return parts[3]
	}
	return ""
}

// daxCreate writes dax_region/create: a new dax device of size 0, the
// youngest of the region.
func (f *Fake) daxCreate(dir, val string) error {
	region := regionOfAttrs(dir)
	if region == "" || val != "1" {
		return syscall.EINVAL
	}
	if f.available(region) == 0 {
		return syscall.ENOSPC
	}
	w, _, err := f.regionWindow(region)
	if err != nil {
		return syscall.ENXIO
	}
	prefix := "dax" + strings.TrimPrefix(region, "region") + "."
	dax := ""
	for id := 0; ; id++ {
		if !f.DaxExists(prefix + strconv.Itoa(id)) {
			dax = prefix + strconv.Itoa(id)
			break
		}
	}
	f.newDax(region, dax, w.NUMANode)
	f.Write(dir+"/create", dax)
	return nil
}

// daxDelete writes dax_region/delete: an unbound dax device of size 0 is
// deleted, but never daxN.0.
func (f *Fake) daxDelete(dir, dax string) error {
	switch {
	case !f.DaxExists(dax):
		return syscall.ENXIO
	case f.daxDriverOf(dax) != "" || f.DaxSize(dax) != 0 || strings.HasSuffix(dax, ".0"):
		return syscall.EBUSY
	}
	region := regionOfAttrs(dir)
	f.freeDaxMinor(dax)
	_ = os.RemoveAll(f.P(daxRegionRel(region) + "/" + dax))
	f.Remove("bus/dax/devices/" + dax)
	f.available(region)
	return nil
}

// daxResize writes the size of an unbound dax device: it shrinks from the
// end, and grows into the free memory of the region first-fit, extending
// its last range or adding ranges.
func (f *Fake) daxResize(dax, val string) error {
	size, err := strconv.ParseUint(val, 0, 64)
	cur := f.DaxSize(dax)
	region := f.daxRegionOf(dax)
	switch {
	case region == "":
		return syscall.ENXIO
	case err != nil || size%DaxAlign != 0:
		return syscall.EINVAL
	case f.daxDriverOf(dax) != "":
		return syscall.EBUSY
	case size > cur && size-cur > f.available(region):
		return syscall.ENOSPC
	}
	ranges := f.mapped(dax)
	for drop := cur - min(cur, size); drop > 0; {
		last := &ranges[len(ranges)-1]
		if last.Size() <= drop {
			drop -= last.Size()
			ranges = ranges[:len(ranges)-1]
		} else {
			last.End -= drop
			drop = 0
		}
	}
	for need := size - min(cur, size); need > 0; {
		f.setRanges(dax, ranges)
		g := f.Gaps(region)[0]
		n := min(need, g.Size())
		if k := len(ranges); k > 0 && ranges[k-1].End+1 == g.Start {
			ranges[k-1].End += n
		} else {
			ranges = append(ranges, kernel.Range{Start: g.Start, End: g.Start + n - 1})
		}
		need -= n
	}
	f.setRanges(dax, ranges)
	f.available(region)
	return nil
}

// daxMap writes the mapping "0x<start>-0x<end>" of a dax device, bound or
// not: memory that is taken or outside the region fails with ENOMEM, a
// length that is not a multiple of the align is ignored.
func (f *Fake) daxMap(dax, val string) error {
	s, e, ok := strings.Cut(val, "-")
	start, err1 := kernel.ParseHex(s)
	end, err2 := kernel.ParseHex(e)
	region := f.daxRegionOf(dax)
	switch {
	case region == "":
		return syscall.ENXIO
	case !ok || err1 != nil || err2 != nil || end < start:
		return syscall.EINVAL
	case (end-start+1)%DaxAlign != 0:
		return nil
	}
	free := false
	for _, g := range f.Gaps(region) {
		free = free || (g.Start <= start && end <= g.End)
	}
	if !free {
		return syscall.ENOMEM
	}
	f.setRanges(dax, append(f.mapped(dax), kernel.Range{Start: start, End: end}))
	f.available(region)
	return nil
}

// daxDriverAttr does what the kernel does for an attribute of a dax driver
// (drivers/dax/bus.c): unbind; bind, which fails for a device that the
// driver does not match (device_dax matches the device of a CXL region only
// by its id, kmem by its type), for a bound device and for one of size 0
// (dax_bus_probe); new_id adds the id and attaches the driver to every
// unbound device that it matches (driver_attach); remove_id removes the id
// (a name that is not one is ignored).
func (f *Fake) daxDriverAttr(driver, attr, dax string) error {
	switch attr {
	case "unbind":
		if f.daxDriverOf(dax) != driver {
			return syscall.ENODEV
		}
		f.setDaxDriver(dax, "")
	case "bind":
		switch {
		case !f.DaxExists(dax), driver != kernel.DriverKmem && !f.HasDaxID(driver, dax):
			return syscall.ENODEV
		case f.daxDriverOf(dax) != "":
			return syscall.EBUSY
		case f.DaxSize(dax) == 0:
			return syscall.ENXIO
		}
		f.setDaxDriver(dax, driver)
	case "new_id":
		f.SetDaxID(driver, dax)
		f.driverAttach(driver)
	case "remove_id":
		f.mu.Lock()
		delete(f.ids[driver], dax)
		f.mu.Unlock()
	default:
		return syscall.EINVAL
	}
	return nil
}

// setDaxDriver binds a dax device to driver ("": unbinds it). The memory
// of kmem becomes memory blocks; unbinding kmem removes the blocks that are
// offline, those online stay (the kernel cannot remove them).
func (f *Fake) setDaxDriver(dax, driver string) {
	if f.daxDriverOf(dax) == kernel.DriverKmem {
		f.removeBlocks(dax)
	}
	f.Remove(f.daxDir(dax) + "/driver")
	if driver == "" {
		return
	}
	f.LinkTo(f.daxDir(dax)+"/driver", "bus/dax/drivers/"+driver)
	if driver == kernel.DriverKmem {
		f.addBlocks(dax)
	}
}

// driverAttach binds a dax driver to every unbound dax device that it matches, as
// driver_attach does: by an id of the driver, and kmem also by type (every
// dax device of a CXL ram region). A device of size 0 fails its probe.
func (f *Fake) driverAttach(driver string) {
	for _, r := range f.regionNames() {
		for _, dax := range f.daxNames(r) {
			if f.daxDriverOf(dax) != "" || f.DaxSize(dax) == 0 {
				continue
			}
			if f.HasDaxID(driver, dax) || driver == kernel.DriverKmem {
				f.setDaxDriver(dax, driver)
			}
		}
	}
}

// blockRel returns the sysfs directory of memory block n, relative.
func blockRel(n int) string {
	return "devices/system/memory/memory" + strconv.Itoa(n)
}

// blockSize returns the memory block size of the fake.
func (f *Fake) blockSize() uint64 {
	bs, _ := kernel.ParseHex(f.Read("devices/system/memory/block_size_bytes"))
	return bs
}

// SetBlockSize sets the memory block size.
func (f *Fake) SetBlockSize(bs uint64) {
	f.Write("devices/system/memory/block_size_bytes", strconv.FormatUint(bs, 16))
}

// blocksOf returns the memory blocks that lie wholly in the memory of a
// dax device.
func (f *Fake) blocksOf(dax string) []int {
	var out []int
	for _, r := range f.DaxRanges(dax) {
		out = append(out, r.Blocks(f.blockSize())...)
	}
	return out
}

// numaNodeOf returns the NUMA node of the memory at addr: the node of its
// window, 0 outside the windows.
func numaNodeOf(addr uint64) int {
	for _, w := range windows {
		if w.Base <= addr && addr < w.Base+w.Size {
			return w.NUMANode
		}
	}
	return 0
}

// addBlocks adds the memory blocks of a dax device, offline (online when a
// udev rule onlines hot-added memory, AutoOnline).
func (f *Fake) addBlocks(dax string) {
	if f.NoKmemBlocks {
		return
	}
	bs := f.blockSize()
	for _, b := range f.blocksOf(dax) {
		if f.Exists(blockRel(b) + "/state") {
			continue
		}
		f.Write(blockRel(b)+"/state", kernel.BlockStateOffline)
		f.Write(blockRel(b)+"/valid_zones", "Normal Movable")
		f.Write(blockRel(b)+"/online", "0")
		f.mkdir(blockRel(b) + "/node" + strconv.Itoa(numaNodeOf(uint64(b)*bs)))
		f.udevOnline(b)
	}
	f.updateNUMANodes()
}

// removeBlocks removes the memory blocks of a dax device that are offline;
// those online stay (kmem cannot remove them).
func (f *Fake) removeBlocks(dax string) {
	for _, b := range f.blocksOf(dax) {
		if f.Read(blockRel(b)+"/state") == kernel.BlockStateOffline {
			f.Remove(blockRel(b))
		}
	}
	f.updateNUMANodes()
}

// AddBlocks makes the memory blocks with the indexes exist, offline.
func (f *Fake) AddBlocks(blocks ...int) {
	bs := f.blockSize()
	for _, b := range blocks {
		f.Write(blockRel(b)+"/state", kernel.BlockStateOffline)
		f.Write(blockRel(b)+"/valid_zones", "Normal Movable")
		f.Write(blockRel(b)+"/online", "0")
		f.mkdir(blockRel(b) + "/node" + strconv.Itoa(numaNodeOf(uint64(b)*bs)))
	}
	f.updateNUMANodes()
}

// BlockState returns the state of memory block n, "" if it does not exist.
func (f *Fake) BlockState(n int) string {
	return f.Read(blockRel(n) + "/state")
}

// SetBlockState sets the state of memory block n as the kernel shows it
// ("online" for every online state), with its zone.
func (f *Fake) SetBlockState(n int, state string) {
	f.setBlock(n, state)
	f.updateNUMANodes()
}

// setBlock writes the state and the zones of memory block n.
func (f *Fake) setBlock(n int, state string) {
	rel := blockRel(n)
	switch state {
	case kernel.BlockStateOffline:
		f.Write(rel+"/state", kernel.BlockStateOffline)
		f.Write(rel+"/valid_zones", "Normal Movable")
		f.Write(rel+"/online", "0")
	case kernel.BlockStateOnlineMovable:
		f.Write(rel+"/state", kernel.BlockStateOnline)
		f.Write(rel+"/valid_zones", "Movable")
		f.Write(rel+"/online", "1")
	default:
		f.Write(rel+"/state", kernel.BlockStateOnline)
		f.Write(rel+"/valid_zones", "Normal")
		f.Write(rel+"/online", "1")
	}
}

// blockWrite writes the state of a memory block: an online of a block that
// is online and an offline of one that is offline are EINVAL; an offline
// that a test made fail with EBUSY (FailWrite) leaves it online; a udev
// rule may online it again after an offline (AutoOnline).
func (f *Fake) blockWrite(dir, val string) error {
	n, err := strconv.Atoi(strings.TrimPrefix(dir, "devices/system/memory/memory"))
	if err != nil || !f.Exists(dir+"/state") {
		return syscall.ENOENT
	}
	cur := f.Read(dir + "/state")
	switch val {
	case kernel.BlockStateOffline:
		if cur == kernel.BlockStateOffline {
			return syscall.EINVAL
		}
		f.setBlock(n, val)
		f.udevOnline(n)
	case kernel.BlockStateOnline, kernel.BlockStateOnlineMovable, kernel.BlockStateOnlineKernel:
		if cur != kernel.BlockStateOffline {
			return syscall.EINVAL
		}
		f.setBlock(n, val)
	default:
		return syscall.EINVAL
	}
	f.updateNUMANodes()
	return nil
}

// udevOnline onlines memory block n again after a moment, as a udev rule
// that onlines hot-added memory does, AutoOnline times per block.
func (f *Fake) udevOnline(n int) {
	f.mu.Lock()
	again := f.autoOnline[n] < f.AutoOnline
	if again {
		f.autoOnline[n]++
	}
	f.mu.Unlock()
	if !again {
		return
	}
	f.probes.Go(func() {
		time.Sleep(2 * time.Millisecond)
		if f.Read(blockRel(n)+"/state") == kernel.BlockStateOffline {
			f.setBlock(n, kernel.BlockStateOnline)
			f.updateNUMANodes()
		}
	})
}

// updateNUMANodes writes has_memory and the MemTotal of the NUMA nodes of the
// windows from the memory blocks that are online.
func (f *Fake) updateNUMANodes() {
	bs := f.blockSize()
	online := map[int]uint64{}
	m, _ := filepath.Glob(f.P("devices/system/memory/memory*"))
	for _, p := range m {
		n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(p), "memory"))
		if err != nil || strings.TrimSpace(readFile(p+"/state")) == kernel.BlockStateOffline || readFile(p+"/state") == "" {
			continue
		}
		online[numaNodeOf(uint64(n)*bs)] += bs
	}
	nodes := []int{0, 1}
	for _, w := range windows {
		kb := online[w.NUMANode] >> 10
		f.Write(fmt.Sprintf("devices/system/node/node%d/meminfo", w.NUMANode), fmt.Sprintf("Node %d MemTotal:       %d kB", w.NUMANode, kb))
		if kb > 0 && !slices.Contains(nodes, w.NUMANode) {
			nodes = append(nodes, w.NUMANode)
		}
	}
	slices.Sort(nodes)
	var s []string
	for _, n := range nodes {
		s = append(s, strconv.Itoa(n))
	}
	f.Write("devices/system/node/has_memory", strings.Join(s, ","))
}
