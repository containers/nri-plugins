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

package kernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Memdev: a CXL memory device memN (/sys/bus/cxl/devices/memN).
//
// Identity: the PCIe serial number. The name memN changes with the probe
// order and hot-plug: it is resolved from the serial at every call.
// Facts: RAM and persistent capacity, the CPU NUMA affinity, the ports from
// the CXL root to its endpoint, its device numbers.
// States (MemdevState): ABSENT (no memN has the serial), UNBOUND (no driver:
// its probe is running, cxl disable-memdev ran, or the probe was refused),
// BOUND (cxl_mem bound, no endpoint port yet), READY (bound with its
// endpoint port: regions can be created). Transitions: MemdevTransitions.
// Invariants: a READY memdev has exactly one endpoint whose uport is it.
// Waits: WaitMemdev polls until the memdev reaches a state.

// MemdevState is the state of a memdev.
type MemdevState int

const (
	// MemdevAbsent: no memdev has the serial.
	MemdevAbsent MemdevState = iota
	// MemdevUnbound: the memdev has no driver.
	MemdevUnbound
	// MemdevBound: cxl_mem is bound, the endpoint port is missing.
	MemdevBound
	// MemdevReady: cxl_mem is bound and the endpoint port exists.
	MemdevReady
)

var memdevStateNames = []string{"ABSENT", "UNBOUND", "BOUND", "READY"}

func (s MemdevState) String() string { return stateText(memdevStateNames, int(s)) }

// MarshalText returns the name of the state.
func (s MemdevState) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// MemdevTransitions are the changes of the state of a memdev.
var MemdevTransitions = fsm.Table[MemdevState]{
	Object:   "MemdevInfo",
	Field:    "State",
	Observed: true,
	States:   fsm.List(MemdevAbsent, MemdevUnbound, MemdevBound, MemdevReady),
	Rows: []fsm.Transition[MemdevState]{
		{From: fsm.List(MemdevAbsent), To: MemdevUnbound, When: "hot-add (the pool attaches the device), boot",
			Effects: "the probe of cxl_mem starts"},
		{From: fsm.List(MemdevAbsent), To: MemdevBound, When: "hot-add, observed after the probe bound cxl_mem"},
		{From: fsm.List(MemdevAbsent), To: MemdevReady, When: "hot-add, observed after the probe finished"},
		{From: fsm.List(MemdevUnbound), To: MemdevBound, When: "observed during the probe of cxl_mem (EnableMemdev, hot-add)",
			Effects: "the probe adds the endpoint port (add_dev() fails until then)"},
		{From: fsm.List(MemdevUnbound), To: MemdevReady, When: "EnableMemdev, observed after the probe finished"},
		{From: fsm.List(MemdevBound), To: MemdevReady, When: "the endpoint port added", Effects: "regions can be created"},
		{From: fsm.List(MemdevBound), To: MemdevUnbound, When: "DisableMemdev"},
		{From: fsm.List(MemdevReady), To: MemdevUnbound, When: "DisableMemdev",
			Guard: "no committed decoder of its endpoint", Effects: "the endpoint port goes"},
		{From: fsm.List(MemdevUnbound), To: MemdevAbsent, When: "hot-remove (the pool detaches the device)"},
		{From: fsm.List(MemdevBound), To: MemdevAbsent, When: "hot-remove"},
		{From: fsm.List(MemdevReady), To: MemdevAbsent, When: "hot-remove",
			Effects: "its regions become hollow"},
	},
}

// MemdevInfo is the observation of a memdev.
type MemdevInfo struct {
	// Name is memN, "" when the memdev is absent.
	Name   string      `json:"name,omitempty"`
	Serial uint64      `json:"serial"`
	State  MemdevState `json:"state"`
	// RamSize and PmemSize are the volatile and the persistent capacity
	// in bytes.
	RamSize  uint64 `json:"ramSize,omitempty"`
	PmemSize uint64 `json:"pmemSize,omitempty"`
	// NUMAAffinity is the NUMA node of the CPUs near the memdev, -1 if not
	// known.
	NUMAAffinity int `json:"numaAffinity"`
	// Major and Minor are the numbers of its character device.
	Major int `json:"major,omitempty"`
	Minor int `json:"minor,omitempty"`
	// Endpoint is its endpoint port (endpointN), "" when not READY.
	Endpoint string `json:"endpoint,omitempty"`
	// Ports are the ports of its decoder path from the CXL root: portN
	// (host bridge, switches), and its endpoint port last.
	Ports []string `json:"ports,omitempty"`
	// Regions are the regions with a target decoder of its endpoint.
	Regions []string `json:"regions,omitempty"`
}

// memdevNames returns the names of the memdevs (memN) in natural order.
func (k *Kernel) memdevNames() []string {
	return k.listCXL("mem*")
}

// memdevSerial returns the serial of the memdev.
func (k *Kernel) memdevSerial(name string) (uint64, error) {
	s, err := readTrim(k.cxlDev(name, "serial"))
	if err != nil {
		return 0, err
	}
	return ParseHex(s)
}

// FindMemdev returns the name memN of the memdev with the serial, an error
// of kind ErrNotFound if there is none. No memdev has the serial 0: a
// memdev without a device serial number cannot be told from another one.
func (k *Kernel) FindMemdev(serial uint64) (string, error) {
	if serial == 0 {
		return "", errorf(ErrNotFound, "no memdev with the serial 0x0: a memdev without a device serial number is not used")
	}
	for _, name := range k.memdevNames() {
		if v, err := k.memdevSerial(name); err == nil && v == serial {
			return name, nil
		}
	}
	return "", errorf(ErrNotFound, "no memdev with the serial 0x%x", serial)
}

// Memdev returns the observation of the memdev with the serial, in state
// MemdevAbsent if there is none.
func (k *Kernel) Memdev(serial uint64) MemdevInfo {
	name, err := k.FindMemdev(serial)
	if err != nil {
		return MemdevInfo{Serial: serial, State: MemdevAbsent, NUMAAffinity: -1}
	}
	m, err := k.memdev(name, k.endpoints())
	if err != nil {
		return MemdevInfo{Serial: serial, State: MemdevAbsent, NUMAAffinity: -1}
	}
	return m
}

// memdev returns the observation of the memdev name; endpoints maps memdev
// names to their endpoints.
func (k *Kernel) memdev(name string, endpoints map[string]string) (MemdevInfo, error) {
	serial, err := k.memdevSerial(name)
	if err != nil {
		return MemdevInfo{}, fmt.Errorf("%s: serial: %w", name, err)
	}
	m := MemdevInfo{Name: name, Serial: serial, State: MemdevUnbound, NUMAAffinity: -1}
	numa, err := readTrim(k.cxlDev(name, "numa_node"))
	if err == nil {
		m.NUMAAffinity, err = strconv.Atoi(numa)
	}
	if err != nil {
		m.NUMAAffinity = -1
	}
	k.factErr(err)
	var ramErr, pmemErr error
	m.RamSize, ramErr = readUint(k.cxlDev(name, "ram", "size"))
	m.PmemSize, pmemErr = readUint(k.cxlDev(name, "pmem", "size"))
	dev, err := readTrim(k.cxlDev(name, "dev"))
	if err == nil {
		m.Major, m.Minor, err = parseDevNumbers(name, dev)
	}
	k.factErr(errors.Join(ramErr, pmemErr, err))
	if !hasDriver(k.cxlDev(name)) {
		return m, nil
	}
	m.State = MemdevBound
	if ep := endpoints[name]; ep != "" {
		m.State = MemdevReady
		m.Endpoint = ep
		for _, dir := range k.endpointPorts(ep) {
			m.Ports = append(m.Ports, filepath.Base(dir))
		}
		m.Regions = k.endpointRegions(ep)
	}
	return m, nil
}

// endpoints returns the endpoint port of each memdev that has one, by the
// memdev name: the uport of an endpoint links to its memdev.
func (k *Kernel) endpoints() map[string]string {
	out := map[string]string{}
	for _, ep := range k.listCXL("endpoint*") {
		if target, err := os.Readlink(k.cxlDev(ep, "uport")); err == nil {
			out[filepath.Base(target)] = ep
		}
	}
	return out
}

// endpoint returns the endpoint port (endpointN) of a memdev.
func (k *Kernel) endpoint(memdev string) (string, error) {
	if ep := k.endpoints()[memdev]; ep != "" {
		return ep, nil
	}
	return "", errorf(ErrEndpointNotReady, "no endpoint port for %s (is it enabled?)", memdev)
}

// Regions returns the regions that have a target decoder of the endpoint of
// the memdev, in natural order.
func (k *Kernel) Regions(memdev string) []string {
	ep, err := k.endpoint(memdev)
	if err != nil {
		return nil
	}
	return k.endpointRegions(ep)
}

// endpointRegions returns the regions with a target decoder of the
// endpoint.
func (k *Kernel) endpointRegions(ep string) []string {
	prefix := "decoder" + strings.TrimPrefix(ep, "endpoint") + "."
	var out []string
	for _, r := range k.listCXL("region*") {
		for _, t := range k.regionTargets(r) {
			if strings.HasPrefix(t, prefix) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// RamSize returns the RAM capacity of a memdev in bytes.
func (k *Kernel) RamSize(memdev string) (uint64, error) {
	return readUint(k.cxlDev(memdev, "ram", "size"))
}

// Persistent tells whether a memdev has persistent memory capacity.
func (k *Kernel) Persistent(memdev string) (bool, error) {
	size, err := readUint(k.cxlDev(memdev, "pmem", "size"))
	if err != nil {
		return false, err
	}
	return size > 0, nil
}

var pciRootRe = regexp.MustCompile(`^(.*/pci[0-9a-f]{4}:[0-9a-f]{2})/`)

// HostBridgeUID returns the uid of the host bridge of a memdev, as root
// decoders list it in target_list: N of the dportN of the CXL root that
// links to the PCI root of the memdev, an error of kind ErrNotFound if no
// dport does. The uid is the ACPI _UID of the host bridge, which differs
// from the bus number of its PCI root on real hardware.
func (k *Kernel) HostBridgeUID(memdev string) (int, error) {
	real, err := filepath.EvalSymlinks(k.cxlDev(memdev))
	if err != nil {
		return 0, err
	}
	m := pciRootRe.FindStringSubmatch(real + "/")
	if m == nil {
		return 0, fmt.Errorf("cannot find the PCI root of %s in %s", memdev, real)
	}
	pciRoot := m[1]
	dports, _ := filepath.Glob(k.cxlDev("root*", "dport*"))
	for _, dport := range dports {
		target, err := filepath.EvalSymlinks(dport)
		if err != nil || target != pciRoot {
			continue
		}
		if v, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(dport), "dport")); err == nil {
			return v, nil
		}
	}
	return 0, errorf(ErrNotFound, "no dport of the CXL root links to %s, the PCI root of %s", pciRoot, memdev)
}

// EnableMemdev binds cxl_mem to a memdev without a driver (cxl
// enable-memdev): the endpoint port exists when it returns. A memdev with
// a driver is left as it is.
func (k *Kernel) EnableMemdev(ctx context.Context, memdev string) error {
	if hasDriver(k.cxlDev(memdev)) {
		return nil
	}
	if _, err := k.exec(ctx, "cxl", "enable-memdev", memdev); err != nil {
		return err
	}
	k.logf("%s: enabled", memdev)
	return nil
}

// DisableMemdev unbinds cxl_mem from a memdev (cxl disable-memdev), so that
// the pool can detach it. A memdev without a driver, or one that does not
// exist, is left as it is.
func (k *Kernel) DisableMemdev(ctx context.Context, memdev string) error {
	if !exists(k.cxlDev(memdev)) || !hasDriver(k.cxlDev(memdev)) {
		return nil
	}
	if _, err := k.exec(ctx, "cxl", "disable-memdev", memdev); err != nil {
		return err
	}
	k.logf("%s: disabled", memdev)
	return nil
}

// WaitMemdev waits until the memdev with the serial is in state want or a
// later one (MemdevUnbound < MemdevBound < MemdevReady) and returns its
// name.
func (k *Kernel) WaitMemdev(ctx context.Context, serial uint64, want MemdevState) (string, error) {
	var got MemdevInfo
	err := k.poll(ctx, func() (bool, error) {
		got = k.Memdev(serial)
		return got.State >= want, nil
	})
	if err != nil {
		return "", fmt.Errorf("timeout waiting for the memdev with serial 0x%x: %w", serial, err)
	}
	return got.Name, nil
}
