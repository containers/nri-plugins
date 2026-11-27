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
	"errors"
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

// The CXL topology of the fake: the CXL root root0 with a dport per host
// bridge, the host bridge ports (port1 for uid 12, port2 for uid 24), a
// switch port under a host bridge for the memdevs plugged with Switch, the
// endpoint ports endpoint4 and up, and the root decoders with their windows.

const (
	rootPath = "devices/platform/ACPI0017:00/root0"
	ndBus    = rootPath + "/nvdimm-bridge0/ndbus0"
	// portDecoders is the number of HDM decoders of every port (qemu).
	portDecoders = 4
	// daxMajor is the major number of dax character devices, pmemMajor of
	// pmem block devices.
	daxMajor  = 252
	pmemMajor = 259
	// DaxAlign is the alignment of the memory of dax devices.
	DaxAlign = 2 << 20
)

// Window is a root decoder and the window of host physical addresses that
// its regions get memory from.
type Window struct {
	// Decoder is the root decoder, decoder0.N.
	Decoder string
	// HostBridge is the uid of its host bridge.
	HostBridge int
	// Type is kernel.RegionRAM or kernel.RegionPmem.
	Type string
	// Base and Size are its addresses.
	Base, Size uint64
	// NUMANode is the NUMA node of its memory.
	NUMANode int
}

// windows are the root decoders of the fake: one ram and one pmem window
// per host bridge, each with a NUMA node of its own.
var windows = []Window{
	{Decoder: "decoder0.0", HostBridge: 12, Type: kernel.RegionRAM, Base: 0x250000000, Size: 0x400000000, NUMANode: 2},
	{Decoder: "decoder0.1", HostBridge: 24, Type: kernel.RegionRAM, Base: 0x850000000, Size: 0x400000000, NUMANode: 3},
	{Decoder: "decoder0.2", HostBridge: 12, Type: kernel.RegionPmem, Base: 0xc50000000, Size: 0x400000000, NUMANode: 4},
	{Decoder: "decoder0.3", HostBridge: 24, Type: kernel.RegionPmem, Base: 0x1050000000, Size: 0x400000000, NUMANode: 5},
}

// window returns the window of a root decoder.
func window(decoder string) (Window, bool) {
	for _, w := range windows {
		if w.Decoder == decoder {
			return w, true
		}
	}
	return Window{}, false
}

// memdev is a memdev that was plugged.
type memdev struct {
	spec MemdevSpec
	name string // memN, "" when not plugged
	path string // its sysfs directory, relative
	ep   string // its endpoint port, "" when it has none
	epID int
	slot int // its slot under the PCI root
}

// MemdevSpec is a memdev to plug:
//   - Serial: its serial
//   - Ram, Pmem: its volatile and persistent capacity in bytes (both 0:
//     256 MiB of RAM)
//   - HostBridge: the uid of its host bridge (0: 12)
//   - Switch: it is under the switch port of its host bridge
//   - Disabled: it is plugged without a driver
type MemdevSpec struct {
	Serial     uint64
	Ram, Pmem  uint64
	HostBridge int
	Switch     bool
	Disabled   bool
}

// addHostBridge adds the host bridge with the uid on the PCI root of bus.
func (f *Fake) addHostBridge(uid, bus int) {
	port := "port" + strconv.Itoa(f.nextPort)
	f.nextPort++
	f.hostBridge[uid] = port
	pci := fmt.Sprintf("devices/pci0000:%02x", bus)
	f.mkdir(pci)
	f.LinkTo(rootPath+"/dport"+strconv.Itoa(uid), pci)
	dir := rootPath + "/" + port
	f.Write(dir+"/decoders_committed", "0")
	for i := 0; i < portDecoders; i++ {
		f.Write(fmt.Sprintf("%s/decoder%s.%d/region", dir, strings.TrimPrefix(port, "port"), i), "")
	}
	f.LinkTo("bus/cxl/devices/"+port, dir)
}

// switchPort returns the directory of the switch port under the host
// bridge with the uid, made at its first use.
func (f *Fake) switchPort(uid int) string {
	hb := rootPath + "/" + f.hostBridge[uid]
	if m, _ := filepath.Glob(f.P(hb + "/port*")); len(m) > 0 {
		return hb + "/" + filepath.Base(m[0])
	}
	port := "port" + strconv.Itoa(f.nextPort)
	f.nextPort++
	dir := hb + "/" + port
	f.Write(dir+"/decoders_committed", "0")
	for i := 0; i < portDecoders; i++ {
		f.Write(fmt.Sprintf("%s/decoder%s.%d/region", dir, strings.TrimPrefix(port, "port"), i), "")
	}
	f.LinkTo("bus/cxl/devices/"+port, dir)
	return dir
}

// addWindow adds a root decoder.
func (f *Fake) addWindow(w Window) {
	dir := rootPath + "/" + w.Decoder
	f.Write(dir+"/devtype", "cxl_decoder_root")
	f.Write(dir+"/target_list", strconv.Itoa(w.HostBridge))
	f.Write(dir+"/interleave_ways", "1")
	f.Write(dir+"/start", fmt.Sprintf("0x%x", w.Base))
	f.Write(dir+"/size", fmt.Sprintf("0x%x", w.Size))
	f.Write(dir+"/cap_ram", map[bool]string{true: "1", false: "0"}[w.Type == kernel.RegionRAM])
	f.Write(dir+"/cap_pmem", map[bool]string{true: "1", false: "0"}[w.Type == kernel.RegionPmem])
	f.Write(dir+"/delete_region", "")
	f.LinkTo("bus/cxl/devices/"+w.Decoder, dir)
	f.Write(fmt.Sprintf("devices/system/node/node%d/meminfo", w.NUMANode), fmt.Sprintf("Node %d MemTotal:       0 kB", w.NUMANode))
	f.nextRegionNames()
}

// nextRegionNames writes the name of the next region to the
// create_ram_region and create_pmem_region of every root decoder.
func (f *Fake) nextRegionNames() {
	next := f.nextRegionName()
	for _, w := range windows {
		dir := rootPath + "/" + w.Decoder
		if f.Exists(dir) {
			f.Write(dir+"/create_ram_region", next)
			f.Write(dir+"/create_pmem_region", next)
		}
	}
}

// nextRegionName returns the lowest region name that is free.
func (f *Fake) nextRegionName() string {
	for id := 0; ; id++ {
		if !f.Exists("bus/cxl/devices/region"+strconv.Itoa(id)) && !f.Exists("bus/nd/devices/region"+strconv.Itoa(id)) {
			return "region" + strconv.Itoa(id)
		}
	}
}

// Plug makes a memdev appear (hot-add by the pool) with its driver and,
// after EndpointDelay, its endpoint port; it returns its name memN.
func (f *Fake) Plug(spec MemdevSpec) string {
	f.tb.Helper()
	if spec.Ram == 0 && spec.Pmem == 0 {
		spec.Ram = 256 << 20
	}
	if spec.HostBridge == 0 {
		spec.HostBridge = 12
	}
	if _, ok := f.hostBridge[spec.HostBridge]; !ok {
		f.tb.Fatalf("no host bridge with uid %d", spec.HostBridge)
	}
	f.mu.Lock()
	m := f.memdevs[spec.Serial]
	if m == nil {
		m = &memdev{slot: len(f.memdevs)}
		f.memdevs[spec.Serial] = m
	}
	m.spec = spec
	f.mu.Unlock()
	name := f.freeName("bus/cxl/devices/mem", 0)
	bus := map[int]int{12: 0x0c, 24: 0x18}[spec.HostBridge]
	path := fmt.Sprintf("devices/pci0000:%02x/0000:%02x:00.0/0000:%02x:00.0/0000:%02x:%02x.0/0000:%02x:00.0/%s",
		bus, bus, bus+1, bus+2, m.slot, bus+3+m.slot, name)
	f.mu.Lock()
	m.name, m.path = name, path
	f.mu.Unlock()
	f.Write(m.path+"/serial", fmt.Sprintf("0x%x", spec.Serial))
	f.Write(m.path+"/ram/size", fmt.Sprintf("0x%x", spec.Ram))
	f.Write(m.path+"/pmem/size", fmt.Sprintf("0x%x", spec.Pmem))
	f.Write(m.path+"/numa_node", map[int]string{12: "0", 24: "1"}[spec.HostBridge])
	f.Write(path+"/dev", "251:"+strings.TrimPrefix(name, "mem"))
	f.LinkTo("bus/cxl/devices/"+name, path)
	if !spec.Disabled {
		f.bind(m)
	}
	return name
}

// freeName returns prefix with the lowest number from first whose sysfs
// path does not exist, without the directory.
func (f *Fake) freeName(prefix string, first int) string {
	for id := first; ; id++ {
		if !f.Exists(prefix + strconv.Itoa(id)) {
			return filepath.Base(prefix + strconv.Itoa(id))
		}
	}
}

// bind binds cxl_mem to a memdev and adds its endpoint port, after
// EndpointDelay.
func (f *Fake) bind(m *memdev) {
	f.LinkTo(m.path+"/driver", "bus/cxl/drivers/cxl_mem")
	if f.EndpointDelay <= 0 {
		f.addEndpoint(m)
		return
	}
	f.probes.Go(func() {
		time.Sleep(f.EndpointDelay)
		f.mu.Lock()
		plugged := f.memdevs[m.spec.Serial] == m && m.name != ""
		f.mu.Unlock()
		if plugged && f.Exists(m.path+"/driver") {
			f.addEndpoint(m)
		}
	})
}

// addEndpoint adds the endpoint port of a memdev.
func (f *Fake) addEndpoint(m *memdev) {
	parent := rootPath + "/" + f.hostBridge[m.spec.HostBridge]
	if m.spec.Switch {
		parent = f.switchPort(m.spec.HostBridge)
	}
	id := f.nextEP
	for f.Exists("bus/cxl/devices/endpoint" + strconv.Itoa(id)) {
		id++
	}
	ep := "endpoint" + strconv.Itoa(id)
	dir := parent + "/" + ep
	f.Write(dir+"/decoders_committed", "0")
	for i := 0; i < portDecoders; i++ {
		f.Write(fmt.Sprintf("%s/decoder%d.%d/region", dir, id, i), "")
		f.Write(fmt.Sprintf("%s/decoder%d.%d/mode", dir, id, i), "none")
	}
	f.LinkTo(dir+"/uport", m.path)
	f.LinkTo("bus/cxl/devices/"+ep, dir)
	f.mu.Lock()
	m.ep, m.epID = ep, id
	f.mu.Unlock()
}

// ep returns the endpoint port of a memdev, "" if it has none.
func (f *Fake) ep(m *memdev) string {
	ep, _ := f.endpointOf(m)
	return ep
}

// endpointOf returns the endpoint port of a memdev and its id, "" if it has
// none.
func (f *Fake) endpointOf(m *memdev) (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m.ep, m.epID
}

// removeEndpoint removes the endpoint port of a memdev.
func (f *Fake) removeEndpoint(m *memdev) {
	ep, _ := f.endpointOf(m)
	if ep == "" {
		return
	}
	if real, err := filepath.EvalSymlinks(f.P("bus/cxl/devices/" + ep)); err == nil {
		_ = os.RemoveAll(real)
	}
	f.Remove("bus/cxl/devices/" + ep)
	f.mu.Lock()
	m.ep = ""
	f.mu.Unlock()
}

// memdevByName returns the plugged memdev memN.
func (f *Fake) memdevByName(name string) *memdev {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.memdevs {
		if m.name == name {
			return m
		}
	}
	return nil
}

// memdevByEndpoint returns the memdev of the endpoint decoder decoderE.K.
func (f *Fake) memdevByEndpoint(decoder string) *memdev {
	id := strings.TrimSuffix(strings.TrimPrefix(decoder, "decoder"), filepath.Ext(decoder))
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.memdevs {
		if m.ep != "" && strconv.Itoa(m.epID) == id {
			return m
		}
	}
	return nil
}

// Memdev returns the name memN of the memdev with the serial, "" if it is
// not plugged.
func (f *Fake) Memdev(serial uint64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.memdevs[serial]; m != nil {
		return m.name
	}
	return ""
}

// Unplug removes a memdev (hot-remove by the pool): its regions become
// hollow, their dax devices and nd regions go, memory that is online stays
// online.
func (f *Fake) Unplug(serial uint64) {
	f.mu.Lock()
	m := f.memdevs[serial]
	name := ""
	if m != nil {
		name = m.name
	}
	f.mu.Unlock()
	if name == "" {
		return
	}
	if ep, id := f.endpointOf(m); ep != "" {
		prefix := "decoder" + strconv.Itoa(id) + "."
		for _, r := range f.regionNames() {
			for _, t := range f.targets(r) {
				if strings.HasPrefix(t, prefix) {
					f.hollow(r)
				}
			}
		}
	}
	f.removeEndpoint(m)
	f.Remove("bus/cxl/devices/" + name)
	f.Remove(m.path)
	f.mu.Lock()
	m.name = ""
	f.mu.Unlock()
}

// hollow makes a region hollow: no target, commit 0, unbound, its decoders
// released.
func (f *Fake) hollow(region string) {
	rel := "bus/cxl/devices/" + region
	f.takeDown(region)
	for _, t := range f.targetFiles(region) {
		f.Write(rel+"/"+t, "")
	}
	f.Write(rel+"/commit", "0")
	f.resetDecoders(region)
}

// enableMemdev runs cxl enable-memdev.
func (f *Fake) enableMemdev(name string) error {
	m := f.memdevByName(name)
	if m == nil {
		return fmt.Errorf("cxl memdev: cmd_enable_memdev: %s: no such memdev", name)
	}
	if !f.Exists(m.path + "/driver") {
		f.bind(m)
	}
	return nil
}

// disableMemdev runs cxl disable-memdev: it refuses a memdev with
// committed endpoint decoders.
func (f *Fake) disableMemdev(name string) error {
	m := f.memdevByName(name)
	if m == nil {
		return fmt.Errorf("cxl memdev: cmd_disable_memdev: %s: no such memdev", name)
	}
	if ep, _ := f.endpointOf(m); ep != "" {
		if n, _ := strconv.Atoi(f.Read("bus/cxl/devices/" + ep + "/decoders_committed")); n > 0 {
			return fmt.Errorf("cxl memdev: cmd_disable_memdev: %s is part of an active region", name)
		}
	}
	f.removeEndpoint(m)
	f.Remove(m.path + "/driver")
	return nil
}

// regionNames returns the names of the regions.
func (f *Fake) regionNames() []string {
	m, _ := filepath.Glob(f.P("bus/cxl/devices/region*"))
	var out []string
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	return out
}

// targetFiles returns the names of the target files of a region.
func (f *Fake) targetFiles(region string) []string {
	m, _ := filepath.Glob(f.P("bus/cxl/devices/" + region + "/target[0-9]*"))
	var out []string
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	return out
}

// targets returns the target decoders of a region that are set.
func (f *Fake) targets(region string) []string {
	var out []string
	for _, t := range f.targetFiles(region) {
		if v := f.Read("bus/cxl/devices/" + region + "/" + t); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// pathPorts returns the sysfs directories of the ports of the decoder path
// of a memdev, its endpoint last.
func (f *Fake) pathPorts(m *memdev) []string {
	ep, _ := f.endpointOf(m)
	if ep == "" {
		return nil
	}
	real, err := filepath.EvalSymlinks(f.P("bus/cxl/devices/" + ep))
	if err != nil {
		return nil
	}
	rel, _ := filepath.Rel(f.SysRoot(), real)
	var dirs []string
	for dir := rel; dir != "." && dir != rootPath; dir = filepath.Dir(dir) {
		if b := filepath.Base(dir); strings.HasPrefix(b, "port") || strings.HasPrefix(b, "endpoint") {
			dirs = append(dirs, dir)
		}
	}
	slices.Reverse(dirs)
	return dirs
}

// portStack returns the committed decoders of the port in dir and their
// regions.
func (f *Fake) portStack(dir string) (string, []string) {
	id := strings.TrimPrefix(strings.TrimPrefix(filepath.Base(dir), "port"), "endpoint")
	n, _ := strconv.Atoi(f.Read(dir + "/decoders_committed"))
	var regions []string
	for i := 0; i < n; i++ {
		regions = append(regions, f.Read(fmt.Sprintf("%s/decoder%s.%d/region", dir, id, i)))
	}
	return id, regions
}

// Stack returns the regions of the committed decoders of a port (portN or
// endpointN), "" for a hole.
func (f *Fake) Stack(port string) []string {
	real, err := filepath.EvalSymlinks(f.P("bus/cxl/devices/" + port))
	if err != nil {
		return nil
	}
	rel, _ := filepath.Rel(f.SysRoot(), real)
	_, regions := f.portStack(rel)
	return regions
}

// SetStack sets the committed decoders of a port (portN or endpointN) to
// the regions, "" for a hole.
func (f *Fake) SetStack(port string, regions ...string) {
	real, err := filepath.EvalSymlinks(f.P("bus/cxl/devices/" + port))
	if err != nil {
		f.tb.Fatalf("no port %s", port)
	}
	dir, _ := filepath.Rel(f.SysRoot(), real)
	id := strings.TrimPrefix(strings.TrimPrefix(filepath.Base(dir), "port"), "endpoint")
	for i := 0; i < portDecoders; i++ {
		r := ""
		if i < len(regions) {
			r = regions[i]
		}
		f.Write(fmt.Sprintf("%s/decoder%s.%d/region", dir, id, i), r)
	}
	f.Write(dir+"/decoders_committed", strconv.Itoa(len(regions)))
}

// resetDecoders releases the decoders of a region in every port: a region
// on the top decoder is popped with the holes below it, another one leaves
// a hole.
func (f *Fake) resetDecoders(region string) {
	for _, rel := range f.portDirs() {
		id, regions := f.portStack(rel)
		i := slices.Index(regions, region)
		if i < 0 {
			continue
		}
		f.Write(fmt.Sprintf("%s/decoder%s.%d/region", rel, id, i), "")
		if strings.HasPrefix(filepath.Base(rel), "endpoint") {
			f.Write(fmt.Sprintf("%s/decoder%s.%d/mode", rel, id, i), "none")
		}
		regions[i] = ""
		n := len(regions)
		for n > 0 && regions[n-1] == "" {
			n--
		}
		f.Write(rel+"/decoders_committed", strconv.Itoa(n))
	}
}

// portDirs returns the sysfs directories of the ports and endpoint ports
// under the CXL root, relative.
func (f *Fake) portDirs() []string {
	var out []string
	_ = filepath.WalkDir(f.P(rootPath), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "decoders_committed" {
			rel, _ := filepath.Rel(f.SysRoot(), filepath.Dir(path))
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// parseArgs returns the options of a command line ("-t ram -d decoder0.0
// -f"); a flag without a value has "".
func parseArgs(args []string) map[string]string {
	opts := map[string]string{}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			opts[args[i]] = args[i+1]
			i++
		} else {
			opts[args[i]] = ""
		}
	}
	return opts
}

// createRegion runs cxl create-region -t <type> -d <decoder> -m <memdev>
// [-s <size>].
func (f *Fake) createRegion(args []string) (string, error) {
	opts := parseArgs(args)
	typ, decoder, name := opts["-t"], opts["-d"], opts["-m"]
	next := f.nextRegionName()
	m := f.memdevByName(name)
	switch {
	case m == nil:
		return "", fmt.Errorf("cxl region: cmd_create_region: %s: no such memdev", name)
	case !f.Exists(m.path+"/driver") || f.ep(m) == "":
		return "", fmt.Errorf("cxl region: cmd_create_region: %s: add_dev() failed", next)
	}
	w, ok := window(decoder)
	if !ok || !f.Exists(rootPath+"/"+decoder) {
		return "", fmt.Errorf("cxl region: cmd_create_region: %s: no such root decoder", decoder)
	}
	if w.Type != typ || w.HostBridge != m.spec.HostBridge {
		return "", fmt.Errorf("cxl region: create_region: %s: %s cannot map %s regions of %s", next, decoder, typ, name)
	}
	capacity := map[string]uint64{kernel.RegionRAM: m.spec.Ram, kernel.RegionPmem: m.spec.Pmem}[typ]
	size := capacity
	if s, ok := opts["-s"]; ok {
		v, err := strconv.ParseUint(s, 0, 64)
		if err != nil || v > capacity {
			return "", fmt.Errorf("cxl region: create_region: %s: failed to set size: %w", next, syscall.EINVAL)
		}
		size = v
	}
	if size == 0 || size%kernel.RegionAlign != 0 {
		return "", fmt.Errorf("cxl region: create_region: %s: failed to set size: %w", next, syscall.EINVAL)
	}
	ports := f.pathPorts(m)
	for _, dir := range ports {
		_, regions := f.portStack(dir)
		if slices.Contains(regions, "") {
			return "", fmt.Errorf("cxl region: create_region: %s: failed to set target0 to %s", next, name)
		}
		if len(regions) >= portDecoders {
			return "", fmt.Errorf("cxl region: create_region: %s: failed to set target0 to %s: no free decoder", next, name)
		}
	}
	start, ok := f.allocate(w, size)
	if !ok {
		return "", fmt.Errorf("cxl region: create_region: %s: failed to set size: %w", next, syscall.ENOSPC)
	}
	// the decoders, endpoint last
	target := ""
	for _, dir := range ports {
		id, regions := f.portStack(dir)
		k := len(regions)
		f.Write(fmt.Sprintf("%s/decoder%s.%d/region", dir, id, k), next)
		if strings.HasPrefix(filepath.Base(dir), "endpoint") {
			f.Write(fmt.Sprintf("%s/decoder%s.%d/mode", dir, id, k), typ)
			target = fmt.Sprintf("decoder%s.%d", id, k)
		}
		f.Write(dir+"/decoders_committed", strconv.Itoa(k+1))
	}
	dir := rootPath + "/" + decoder + "/" + next
	for file, value := range map[string]string{
		"commit": "1", "devtype": "cxl_region", "interleave_ways": "1", "mode": typ, "locked": "0",
		"resource": fmt.Sprintf("0x%x", start), "size": fmt.Sprintf("0x%x", size), "target0": target,
		"uuid": fmt.Sprintf("675e71d7-179f-4beb-ab43-%012x", start),
	} {
		f.Write(dir+"/"+file, value)
	}
	f.LinkTo("bus/cxl/devices/"+next, dir)
	f.nextRegionNames()
	f.bringUp(next)
	return fmt.Sprintf(`{"region":"%s","resource":"0x%x","size":"%d.00 MiB","type":"%s"}`, next, start, size>>20, typ) +
		"\ncxl region: cmd_create_region: created 1 region\n", nil
}

// allocate returns the lowest address of the window with size bytes that
// no region has, first-fit.
func (f *Fake) allocate(w Window, size uint64) (uint64, bool) {
	var used []kernel.Range
	m, _ := filepath.Glob(f.P(rootPath + "/" + w.Decoder + "/region*"))
	for _, p := range m {
		start, err1 := kernel.ParseHex(strings.TrimSpace(readFile(p + "/resource")))
		sz, err2 := kernel.ParseHex(strings.TrimSpace(readFile(p + "/size")))
		if err1 == nil && err2 == nil && sz > 0 {
			used = append(used, kernel.Range{Start: start, End: start + sz - 1})
		}
	}
	slices.SortFunc(used, func(a, b kernel.Range) int { return cmp.Compare(a.Start, b.Start) })
	next := w.Base
	for _, u := range used {
		if u.Start >= next+size {
			break
		}
		next = max(next, u.End+1)
	}
	if next+size > w.Base+w.Size {
		return 0, false
	}
	return next, true
}

// readFile returns the content of a file, "" if it cannot be read.
func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// regionWindow returns the window and the range of a region.
func (f *Fake) regionWindow(region string) (Window, kernel.Range, error) {
	real, err := filepath.EvalSymlinks(f.P("bus/cxl/devices/" + region))
	if err != nil {
		return Window{}, kernel.Range{}, err
	}
	w, ok := window(filepath.Base(filepath.Dir(real)))
	if !ok {
		return Window{}, kernel.Range{}, fmt.Errorf("%s is not under a root decoder", region)
	}
	start, err1 := kernel.ParseHex(f.Read("bus/cxl/devices/" + region + "/resource"))
	size, err2 := kernel.ParseHex(f.Read("bus/cxl/devices/" + region + "/size"))
	if err := errors.Join(err1, err2); err != nil || size == 0 {
		return w, kernel.Range{}, fmt.Errorf("%s has no memory", region)
	}
	return w, kernel.Range{Start: start, End: start + size - 1}, nil
}

// bringUp binds cxl_region to a region: a ram region gets its dax region
// with daxN.0 over all of its memory bound to kmem, a pmem region its nd
// region.
func (f *Fake) bringUp(region string) {
	rel := "bus/cxl/devices/" + region
	f.LinkTo(rel+"/driver", "bus/cxl/drivers/cxl_region")
	switch f.Read(rel + "/mode") {
	case kernel.RegionRAM:
		f.makeDaxRegion(region)
	case kernel.RegionPmem:
		f.makeNdRegion(region)
	}
}

// takeDown unbinds cxl_region from a region: its dax region and dax
// devices, or its nd region, go; the memory blocks of kmem that are offline
// go with them, those online stay.
func (f *Fake) takeDown(region string) {
	rel := "bus/cxl/devices/" + region
	for _, dax := range f.daxNames(region) {
		if f.daxDriverOf(dax) == kernel.DriverKmem {
			f.removeBlocks(dax)
		}
		f.Remove("bus/dax/devices/" + dax)
		f.freeDaxMinor(dax)
	}
	m, _ := filepath.Glob(f.P(rel + "/dax_region*"))
	for _, p := range m {
		_ = os.RemoveAll(p)
	}
	f.removeNdRegion(region)
	f.Remove(rel + "/driver")
}

// enableRegion runs cxl enable-region.
func (f *Fake) enableRegion(region string) error {
	rel := "bus/cxl/devices/" + region
	if !f.Exists(rel) {
		return fmt.Errorf("cxl region: cmd_enable_region: %s: no such region", region)
	}
	if !f.Exists(rel + "/driver") {
		f.bringUp(region)
	}
	return nil
}

// disableRegion runs cxl disable-region: it offlines the memory of the
// kmem devices of the region first and fails if it cannot (an offline that
// a test made fail, FailWrite); the decoders stay committed.
func (f *Fake) disableRegion(region string) error {
	if !f.Exists("bus/cxl/devices/" + region) {
		return fmt.Errorf("cxl region: cmd_disable_region: %s: no such region", region)
	}
	for _, dax := range f.daxNames(region) {
		if f.daxDriverOf(dax) != kernel.DriverKmem {
			continue
		}
		for _, b := range f.blocksOf(dax) {
			rel := blockRel(b)
			if !f.Exists(rel+"/state") || f.Read(rel+"/state") == kernel.BlockStateOffline {
				continue
			}
			f.mu.Lock()
			err := takeErr(f.failWr, rel+"/state")
			f.mu.Unlock()
			if err != nil {
				return fmt.Errorf("cxl region: disable_region: %s: unable to offline %s: %w", region, dax, err)
			}
			f.setBlock(b, kernel.BlockStateOffline)
		}
	}
	f.updateNUMANodes()
	f.takeDown(region)
	return nil
}

// destroyRegion runs cxl destroy-region: it refuses an enabled, a hollow
// or a locked region.
func (f *Fake) destroyRegion(region string) error {
	rel := "bus/cxl/devices/" + region
	switch {
	case !f.Exists(rel):
		return fmt.Errorf("cxl region: cmd_destroy_region: %s: no such region", region)
	case len(f.targets(region)) == 0:
		return fmt.Errorf("cxl region: destroy_region: cxl_region_get_target_decoder: %s: error locating decoder for target0", region)
	case f.Read(rel+"/locked") == "1":
		return fmt.Errorf("cxl region: destroy_region: %s: region is locked", region)
	case f.Exists(rel + "/driver"):
		return fmt.Errorf("cxl region: destroy_region: %s active, abort", region)
	}
	f.takeDown(region)
	f.resetDecoders(region)
	f.removeRegion(region)
	return nil
}

// removeRegion removes the sysfs directory and link of a region.
func (f *Fake) removeRegion(region string) {
	if real, err := filepath.EvalSymlinks(f.P("bus/cxl/devices/" + region)); err == nil {
		_ = os.RemoveAll(real)
	}
	f.Remove("bus/cxl/devices/" + region)
	f.nextRegionNames()
}

// deleteRegion does what writing a region to the delete_region of a root
// decoder does: it deletes a region under the decoder, bound or not, and
// releases its decoders. The package deletes only hollow regions: the
// deletion of another one is a violation (Violations).
func (f *Fake) deleteRegion(decoder, region string) error {
	f.record("delete_region " + decoder + " " + region)
	real, err := filepath.EvalSymlinks(f.P("bus/cxl/devices/" + region))
	if err != nil || filepath.Base(filepath.Dir(real)) != decoder {
		return syscall.ENODEV
	}
	if len(f.targets(region)) > 0 {
		f.mnt.mu.Lock()
		f.mnt.violate("delete_region of %s, which is not hollow", region)
		f.mnt.mu.Unlock()
	}
	if f.Exists("bus/cxl/devices/" + region + "/driver") {
		f.takeDown(region)
	}
	f.resetDecoders(region)
	f.removeRegion(region)
	return nil
}

// HollowRegion makes a hollow region of the type under the root decoder of
// the window, as the kernel leaves a region whose memdev was removed before
// it was destroyed: commit 0, target0 empty, its range kept. It returns its
// name.
func (f *Fake) HollowRegion(decoder string, size uint64) string {
	f.tb.Helper()
	w, ok := window(decoder)
	if !ok {
		f.tb.Fatalf("no root decoder %s", decoder)
	}
	start, ok := f.allocate(w, size)
	if !ok {
		f.tb.Fatalf("no room for %d bytes in %s", size, decoder)
	}
	name := f.nextRegionName()
	dir := rootPath + "/" + decoder + "/" + name
	for file, value := range map[string]string{
		"commit": "0", "devtype": "cxl_region", "interleave_ways": "1", "mode": w.Type, "locked": "0",
		"resource": fmt.Sprintf("0x%x", start), "size": fmt.Sprintf("0x%x", size), "target0": "",
		"uuid": fmt.Sprintf("675e71d7-179f-4beb-ab43-%012x", start),
	} {
		f.Write(dir+"/"+file, value)
	}
	f.LinkTo("bus/cxl/devices/"+name, dir)
	f.nextRegionNames()
	return name
}

// Reboot reboots the node: a new boot id; no region, dax device, memory
// block of CXL memory, nd region, block device, mount or loop device of
// the old boot; the memdevs plugged come up enabled with new endpoint
// ports; the labels and the data of persistent memdevs stay; device
// numbers are given out anew.
func (f *Fake) Reboot() {
	for _, r := range f.regionNames() {
		f.takeDown(r)
		f.removeRegion(r)
	}
	for _, port := range f.allPorts() {
		f.SetStack(port)
	}
	m, _ := filepath.Glob(f.P("devices/system/memory/memory*"))
	for _, p := range m {
		_ = os.RemoveAll(p)
	}
	f.mnt.reboot()
	f.mu.Lock()
	var plugged []*memdev
	for _, md := range f.memdevs {
		if md.name != "" {
			plugged = append(plugged, md)
		}
	}
	f.ids = map[string]map[string]bool{}
	f.mu.Unlock()
	for _, md := range plugged {
		f.removeEndpoint(md)
		f.LinkTo(md.path+"/driver", "bus/cxl/drivers/cxl_mem")
		f.addEndpoint(md)
	}
	f.boot()
	f.updateNUMANodes()
	f.changed()
}

// allPorts returns the names of the ports and endpoint ports.
func (f *Fake) allPorts() []string {
	var out []string
	for _, pattern := range []string{"port*", "endpoint*"} {
		m, _ := filepath.Glob(f.P("bus/cxl/devices/" + pattern))
		for _, p := range m {
			out = append(out, filepath.Base(p))
		}
	}
	return out
}
