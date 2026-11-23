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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The persistent memory of the fake: the nd region of a pmem region on the
// nd bus of nvdimm-bridge0, probed in the order of the kernel (the region
// bound, its namespace registered from the labels or as the seed, the pfn
// device and its block device last), and ndctl create-namespace. The labels
// and the data of a memdev outlive its regions and reboots.

// dataOffset is the offset of the data of an fsdax namespace with map mem:
// its block device is that much smaller than the namespace.
const dataOffset = 2 << 20

// ndRel returns the sysfs directory of the nd region of a CXL region,
// relative.
func ndRel(region string) string {
	return ndBus + "/" + region
}

// regionSerial returns the serial of the memdev of a region, false if it
// has none.
func (f *Fake) regionSerial(region string) (uint64, bool) {
	for _, t := range f.targets(region) {
		if m := f.memdevByEndpoint(t); m != nil {
			return m.spec.Serial, true
		}
	}
	return 0, false
}

// Labels tells whether the labels of the memdev with the serial have an
// fsdax namespace.
func (f *Fake) Labels(serial uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.labels[serial]
}

// SetLabels sets whether the labels of the memdev with the serial have an
// fsdax namespace.
func (f *Fake) SetLabels(serial uint64, fsdax bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels[serial] = fsdax
}

// makeNdRegion makes the nd region of a pmem region and probes it, before
// returning when PmemDelay is 0, else asynchronously.
func (f *Fake) makeNdRegion(region string) {
	f.Write("bus/cxl/devices/"+region+"/pmem_"+region+"/uevent", "")
	size := f.Read("bus/cxl/devices/" + region + "/size")
	n, _ := strconv.ParseUint(strings.TrimPrefix(size, "0x"), 16, 64)
	f.Write(ndRel(region)+"/size", strconv.FormatUint(n, 10))
	f.LinkTo("bus/nd/devices/"+region, ndRel(region))
	serial, _ := f.regionSerial(region)
	if f.PmemDelay <= 0 {
		f.probeNdRegion(region, serial, n)
		return
	}
	f.probes.Go(func() {
		f.probeNdRegion(region, serial, n)
	})
}

// probeNdRegion makes the nd region and its namespace appear as the kernel
// does: the region bound, then namespaceN.0 (from the labels, else the
// seed), then the pfn device and the block device of an fsdax namespace.
func (f *Fake) probeNdRegion(region string, serial, size uint64) {
	nd := ndRel(region)
	time.Sleep(f.PmemDelay)
	if !f.Exists(nd) {
		return
	}
	f.Write(nd+"/init_namespaces", "1/1")
	f.LinkTo(nd+"/driver", "bus/nd/drivers/nd_region")
	time.Sleep(f.PmemDelay)
	if !f.Exists(nd) {
		return
	}
	ns := nd + "/namespace" + strings.TrimPrefix(region, "region") + ".0"
	f.Write(ns+"/holder", "")
	f.Write(ns+"/mode", "raw")
	f.LinkTo("bus/nd/devices/"+filepath.Base(ns), ns)
	if !f.Labels(serial) {
		f.Write(ns+"/size", "0")
		return
	}
	// nd_pmem probes the namespace before the pfn device claims it
	f.Write(ns+"/size", strconv.FormatUint(size, 10))
	f.LinkTo(ns+"/driver", "bus/nd/drivers/nd_pmem")
	time.Sleep(f.PmemDelay)
	if f.Exists(nd) {
		f.Fsdax(region, "pfn"+strings.TrimPrefix(region, "region")+".1")
	}
}

// Fsdax makes namespaceN.0 of the nd region of a CXL region an fsdax
// namespace with map mem held by the pfn device, with a block device pmemK
// of the next free name and the next device numbers, and returns the block
// device (/dev/pmemK).
func (f *Fake) Fsdax(region, pfn string) string {
	nd := ndRel(region)
	ns := nd + "/namespace" + strings.TrimPrefix(region, "region") + ".0"
	size, _ := strconv.ParseUint(f.Read(nd+"/size"), 10, 64)
	f.Write(ns+"/size", strconv.FormatUint(size, 10))
	f.Write(ns+"/holder", pfn)
	f.Write(ns+"/mode", "memory")
	f.Remove(ns + "/driver")
	f.LinkTo("bus/nd/devices/"+filepath.Base(ns), ns)
	pd := nd + "/" + pfn
	f.Write(pd+"/mode", "ram")
	f.LinkTo("bus/nd/devices/"+pfn, pd)
	f.LinkTo(pd+"/driver", "bus/nd/drivers/nd_pmem")
	blk := ""
	if m, _ := filepath.Glob(f.P(pd + "/block/pmem*")); len(m) > 0 {
		blk = filepath.Base(m[0])
	} else {
		blk = f.freeName("class/block/pmem", 0)
		f.mu.Lock()
		minor := f.pmemMinor
		f.pmemMinor++
		f.mu.Unlock()
		dev := fmt.Sprintf("%d:%d", pmemMajor, minor)
		f.Write(pd+"/block/"+blk+"/dev", dev)
		f.Write(pd+"/block/"+blk+"/size", strconv.FormatUint((size-dataOffset)/512, 10))
		f.Write(pd+"/block/"+blk+"/ro", "0")
		f.LinkTo("class/block/"+blk, pd+"/block/"+blk)
		f.Write("dev/block/"+dev+"/uevent", "MAJOR="+strconv.Itoa(pmemMajor)+"\nMINOR="+strconv.Itoa(minor)+"\nDEVNAME="+blk)
	}
	serial, _ := f.regionSerial(region)
	f.mnt.addBlockDevice(filepath.Join(f.DevRoot(), blk), serial)
	return filepath.Join(f.DevRoot(), blk)
}

// removeNdRegion removes the nd region of a CXL region, its namespaces,
// pfn devices and block devices; the labels stay.
func (f *Fake) removeNdRegion(region string) {
	nd := ndRel(region)
	if !f.Exists(nd) {
		return
	}
	m, _ := filepath.Glob(f.P(nd + "/*/block/*"))
	for _, p := range m {
		blk := filepath.Base(p)
		dev := strings.TrimSpace(readFile(p + "/dev"))
		f.Remove("class/block/" + blk)
		if dev != "" {
			f.Remove("dev/block/" + dev)
		}
		f.mnt.removeBlockDevice(filepath.Join(f.DevRoot(), blk))
	}
	links, _ := filepath.Glob(f.P("bus/nd/devices/*"))
	for _, l := range links {
		if real, err := filepath.EvalSymlinks(l); err != nil || strings.HasPrefix(real, f.P(nd)) {
			_ = os.Remove(l)
		}
	}
	f.Remove(nd)
	f.Remove("bus/cxl/devices/" + region + "/pmem_" + region)
}

// createNamespace runs ndctl create-namespace -f -r <nd region> -m fsdax
// -M mem [-e <namespace>]: the labels get an fsdax namespace.
func (f *Fake) createNamespace(args []string) (string, error) {
	opts := parseArgs(args)
	region := opts["-r"]
	if !f.Exists(ndRel(region)) {
		return "", fmt.Errorf("ndctl: create-namespace: %s: no such region", region)
	}
	if serial, ok := f.regionSerial(region); ok {
		f.SetLabels(serial, true)
	}
	dev := f.Fsdax(region, "pfn"+strings.TrimPrefix(region, "region")+".0")
	ns := "namespace" + strings.TrimPrefix(region, "region") + ".0"
	size, _ := strconv.ParseUint(f.Read(ndRel(region)+"/size"), 10, 64)
	return fmt.Sprintf(`{"dev":"%s","mode":"fsdax","map":"mem","size":%d,"uuid":"ac26f016-7aa3-446c-8719-78f2d546ebed","sector_size":512,"align":2097152,"blockdev":"%s"}`,
		ns, size-dataOffset, filepath.Base(dev)) + "\n", nil
}

// RenamePmem gives the block device of the fsdax namespace of a region the
// name pmemK and the numbers of dev ("259:N"), as the next boot or a new
// probe may, and returns its device node.
func (f *Fake) RenamePmem(region, name, dev string) string {
	f.tb.Helper()
	m, _ := filepath.Glob(f.P(ndRel(region) + "/pfn*/block/pmem*"))
	if len(m) == 0 {
		f.tb.Fatalf("%s has no fsdax block device", region)
	}
	old := m[0]
	pfn, _ := filepath.Rel(f.SysRoot(), filepath.Dir(filepath.Dir(old)))
	size := strings.TrimSpace(readFile(old + "/size"))
	oldName := filepath.Base(old)
	if num := strings.TrimSpace(readFile(old + "/dev")); num != "" {
		f.Remove("dev/block/" + num)
	}
	f.Remove("class/block/" + oldName)
	_ = os.RemoveAll(old)
	f.mnt.mu.Lock()
	serial := f.mnt.devices[filepath.Join(f.DevRoot(), oldName)]
	f.mnt.mu.Unlock()
	f.mnt.removeBlockDevice(filepath.Join(f.DevRoot(), oldName))
	f.Write(pfn+"/block/"+name+"/dev", dev)
	f.Write(pfn+"/block/"+name+"/size", size)
	f.LinkTo("class/block/"+name, pfn+"/block/"+name)
	ma, mi, _ := strings.Cut(dev, ":")
	f.Write("dev/block/"+dev+"/uevent", "MAJOR="+ma+"\nMINOR="+mi+"\nDEVNAME="+name)
	f.mnt.addBlockDevice(filepath.Join(f.DevRoot(), name), serial)
	return filepath.Join(f.DevRoot(), name)
}
