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

// The oracle of A44: boot ids, the device nodes of the containers, the
// memory of a numa claim's node. The device-node check needs no reboot:
// a container started with the CDI devices of an old boot, or of another
// claim, has a node that is not the current device of its claim (A44:
// an rw container of c-pmem1 must not read c-pmem0).

import (
	"path"
	"strconv"
	"strings"
)

// checkBoot: the boot id of a VM changes with every node-reboot of it and
// never otherwise (A44). On-line only: an off-line expectation does not
// know the boot ids.
func (k *checker) checkBoot(v VM, ov *ObsVM) {
	d := &k.st.Drv[v]
	switch {
	case ov.BootID == "":
		k.notef("vms.%s.bootID not observed: no check of its boots (A44)", v)
	case d.BootBefore != "" && ov.BootID == d.BootBefore:
		k.failf("%s: boot id %s as before its node-reboot (A44)", v, ov.BootID)
	case d.BootBefore == "" && d.Boot != "" && ov.BootID != d.Boot:
		k.failf("%s: boot id %s, want %s: the VM booted without a node-reboot (A44)", v, ov.BootID, d.Boot)
	}
}

// devNodeOf is what the container of a slot's pod must have of the CXL
// device nodes (A44): the serial and kind (dax, pmem) of its claim's
// device and how many (1 for dax and block kinds, 0 for others); want -1:
// not checked beyond their being current devices (lease kinds: a lease's
// device is an fd, its companion's).
func devNodeOf(sl *Slot) (serial uint64, kind string, want int) {
	switch k := sl.Kind; {
	case k.leaseKind():
		return 0, "", -1
	case k == KLDax:
		return localSerial, "dax", 1
	case k.dax():
		return devices[sl.Dev].Serial, "dax", 1
	case k.blockKind():
		return devices[sl.Dev].Serial, "pmem", 1
	}
	return 0, "", 0
}

// cxlDevs decodes the CXL devices of a VM ("cxlDevices") by name; ok
// false (and a wait or a note) when they are not there.
func (k *checker) cxlDevs(v VM, ov *ObsVM) (map[string]*ObsCXLDev, bool) {
	var devs []ObsCXLDev
	ok, absent, err := rawPart(ov.CXLDevs, &devs)
	switch {
	case absent:
		k.notef("vms.%s.cxlDevices not observed: no check of the containers' device nodes (A44)", v)
		return nil, false
	case err != nil:
		k.waitf("%s: cxlDevices: %v", v, err)
		return nil, false
	case !ok:
		k.waitf("%s: cxlDevices not read (null)", v)
		return nil, false
	}
	byName := map[string]*ObsCXLDev{}
	for j := range devs {
		byName[devs[j].Name] = &devs[j]
	}
	return byName, true
}

// serial is the memdev serial of a CXL device's region.
func (d *ObsCXLDev) serial() uint64 {
	s := d.Serial
	if s == "" && len(d.Serials) > 0 {
		s = d.Serials[0]
	}
	x, _ := parseSerial(s)
	return x
}

func (d *ObsCXLDev) dev() string  { return devNumbers(d.Dev, d.Major, d.Minor) }
func (n *ObsDevNode) dev() string { return devNumbers(n.Dev, n.Major, n.Minor) }

// cxlNode: a device node of a container that names a CXL device (dax,
// pmem); the others are not the test's.
func cxlNode(p string) bool {
	b := path.Base(p)
	return strings.HasPrefix(b, "dax") || strings.HasPrefix(b, "pmem")
}

// checkDevNodes (A44): every /dev/dax* and /dev/pmem* in the container of
// a pod that is Running on the VM is a device of the VM now (its name and
// major:minor) and, but for lease kinds, of its claim's memdev; dax and
// block kinds have exactly that one. A stale node (of another boot, of
// another claim) is a wait: the settle fails at its timeout.
func (k *checker) checkDevNodes(v VM, ov *ObsVM) {
	byName, ok := k.cxlDevs(v, ov)
	if !ok {
		return
	}
	podSlot := map[string]int{}
	for i, p := range k.cur {
		if p != nil {
			podSlot[p.Name] = i
		}
	}
	for j := range ov.Containers {
		c := &ov.Containers[j]
		i, isSlot := podSlot[c.Pod]
		if !isSlot {
			continue
		}
		ss, sl := &k.rs.Slots[i], k.rs.slot(i)
		if ss.Phase != Running || ss.Node != v {
			continue
		}
		var nodes []ObsDevNode
		ok, absent, err := rawPart(c.DevNodes, &nodes)
		switch {
		case absent:
			k.notef("vms.%s.containers[].devices not observed: no check of the containers' device nodes (A44)", v)
			continue
		case err != nil:
			k.waitf("%s: container of %s: devices: %v", v, sl.Name, err)
			continue
		case !ok:
			k.waitf("%s: container of %s: devices not read (null)", v, sl.Name)
			continue
		}
		serial, kind, want := devNodeOf(sl)
		mine := 0
		for x := range nodes {
			nd := &nodes[x]
			if !cxlNode(nd.Path) {
				continue
			}
			d := byName[path.Base(nd.Path)]
			switch {
			case d == nil:
				k.waitf("%s: container of %s: device node %s (%s) is no CXL device of the VM (stale, A44)", v, sl.Name, nd.Path, nd.dev())
			case d.dev() != "" && d.dev() != nd.dev():
				k.waitf("%s: container of %s: device node %s is %s, the VM's %s is %s (stale, A44)", v, sl.Name, nd.Path, nd.dev(), d.Name, d.dev())
			case want < 0:
			case d.Kind != kind || d.serial() != serial:
				k.waitf("%s: container of %s: device node %s is the %s device of %s, not its claim's (%s of %s)", v, sl.Name, nd.Path, d.Kind, formatSerial(d.serial()), kindOr(kind), formatSerial(serial))
			default:
				mine++
			}
		}
		if want >= 0 && mine != want {
			k.waitf("%s: container of %s: %d device nodes of its claim (%s of %s), want %d (A44)", v, sl.Name, mine, kindOr(kind), formatSerial(serial), want)
		}
	}
}

func kindOr(kind string) string {
	if kind == "" {
		return "none"
	}
	return kind
}

// checkInspectDevices: in the env of an inspected pod, a numa claim's
// CXL_<KEY>_NUMA_NODE has memory on the pod's VM, and a dax or block claim's
// CXL_<KEY>_DAX or _PATH is the current device of its claim there (A44):
// fatal, as the env checks (an inspection is not repeated).
func (k *checker) checkInspectDevices(i int, in *ObsInspect) {
	ss, sl := &k.rs.Slots[i], k.rs.slot(i)
	ov := k.o.VMs[ss.Node.String()]
	if ov == nil {
		return
	}
	for key, val := range in.Env {
		if !strings.HasPrefix(key, "CXL_") || !strings.HasSuffix(key, "_NUMA_NODE") || ov.Nodes == nil {
			continue
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			continue // (envDiffs says it)
		}
		if ov.Nodes[val].MemTotal == 0 {
			// (a wait: O3's MemTotal of the predicted node says the
			// same at every poll, this one only with the inspection)
			k.waitf("env of %s: %s=%d has no memory on %s (A44)", sl.Name, key, n, ss.Node)
		}
	}
	serial, kind, want := devNodeOf(sl)
	if want <= 0 {
		return
	}
	var devs []ObsCXLDev
	if ok, _, _ := rawPart(ov.CXLDevs, &devs); !ok {
		return
	}
	suffix := "_DAX"
	if kind == "pmem" {
		suffix = "_PATH"
	}
	val, ok := in.Env["CXL_"+serialKey(serial)+suffix]
	if !ok {
		return // (envDiffs says it)
	}
	for j := range devs {
		if d := &devs[j]; d.Name == path.Base(val) && d.Kind == kind && d.serial() == serial {
			return
		}
	}
	k.failf("env of %s: CXL_%s%s=%s is no %s device of %s on %s now (stale, A44)", sl.Name, serialKey(serial), suffix, val, kind, formatSerial(serial), ss.Node)
}
