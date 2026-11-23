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

// The fixed cluster of test19-many-pools with a local memdev on every VM
// (contract.txt 1): three VMs, three pools of two devices each, the
// NUMA nodes of the two CXL host bridges.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// VM is a VM id: 0, 1 or 2 (vm0, vm1, vm2); VMAny is the unknown node of
// an "any" slot.
type VM int8

const (
	VM0   VM = 0
	VM1   VM = 1
	VM2   VM = 2
	VMAny VM = -1
	nVMs     = 3
)

func (v VM) String() string {
	if v < 0 || v >= nVMs {
		return "any"
	}
	return "vm" + strconv.Itoa(int(v))
}

// parseVM parses vm0, vm1, vm2 or any.
func parseVM(s string) (VM, error) {
	switch s {
	case "vm0":
		return VM0, nil
	case "vm1":
		return VM1, nil
	case "vm2":
		return VM2, nil
	case "any":
		return VMAny, nil
	}
	return VMAny, fmt.Errorf("invalid VM %q (vm0, vm1, vm2 or any)", s)
}

// Pool is a pool id: 0, 1, 2 for pools a, b, c.
type Pool int8

const (
	PoolA  Pool = 0
	PoolB  Pool = 1
	PoolC  Pool = 2
	noPool Pool = -1
	nPools      = 3
)

func (p Pool) String() string {
	if p < 0 || p >= nPools {
		return "-"
	}
	return string(rune('a' + p))
}

func parsePool(s string) (Pool, error) {
	if len(s) == 1 && s[0] >= 'a' && s[0] < 'a'+nPools {
		return Pool(s[0] - 'a'), nil
	}
	return noPool, fmt.Errorf("invalid pool %q (a, b or c)", s)
}

// Bridge is a CXL host bridge of every VM: hb0 has the local memdev and
// the slots of pools a and b, hb1 the slots of pool c.
type Bridge int8

const (
	HB0      Bridge = 0
	HB1      Bridge = 1
	nBridges        = 2
)

const (
	mi        = uint64(1) << 20
	blockSize = 128 * mi
	// localSerial is the serial of the local memdev of every VM.
	localSerial = uint64(0xc100e2e1)
)

// DevID is a pool device: pool*2 for its exclusive device P-pooled0,
// pool*2+1 for its shared device P-shared0, then pool-c's persistent
// devices c-pmem0 (shared) and c-pmem1 (exclusive) (A22).
type DevID int8

const (
	nDevs = 2*nPools + 2
	// devsPerPool is the most devices of one pool (CtrlState.Att)
	devsPerPool = 4
	cPmem0      = DevID(2 * nPools)
	cPmem1      = DevID(2*nPools + 1)
	// nsSize is the SIZE of the fsdax namespace of a 256Mi persistent
	// device (pfn info and alignment, A22)
	nsSize = uint64(266338304)
)

// Device is a pool device.
type Device struct {
	Name       string
	Serial     uint64
	Size       uint64
	Shared     bool
	Pool       Pool
	Persistent bool
}

var devices = [nDevs]Device{
	{"a-pooled0", 0xc1ee0001, 512 * mi, false, PoolA, false},
	{"a-shared0", 0xc1ae0001, 256 * mi, true, PoolA, false},
	{"b-pooled0", 0xc1ee1001, 512 * mi, false, PoolB, false},
	{"b-shared0", 0xc1ae1001, 256 * mi, true, PoolB, false},
	{"c-pooled0", 0xc1ee2001, 256 * mi, false, PoolC, false},
	{"c-shared0", 0xc1ae2001, 256 * mi, true, PoolC, false},
	{"c-pmem0", 0xc1ae2002, 256 * mi, true, PoolC, true},
	{"c-pmem1", 0xc1ee2002, 256 * mi, false, PoolC, true},
}

func pooledDev(p Pool) DevID { return DevID(2 * p) }
func sharedDev(p Pool) DevID { return DevID(2*p + 1) }

// poolDevTable lists the devices of each pool in the order of
// CtrlState.Att (-1: none).
var poolDevTable = [nPools][devsPerPool]DevID{
	PoolA: {0, 1, -1, -1},
	PoolB: {2, 3, -1, -1},
	PoolC: {4, 5, cPmem0, cPmem1},
}

// poolDevs are the devices of a pool, in the order of CtrlState.Att.
func poolDevs(p Pool) []DevID {
	var out []DevID
	for _, d := range poolDevTable[p] {
		if d >= 0 {
			out = append(out, d)
		}
	}
	return out
}

// devIndex is the index of a device in its pool's CtrlState.Att.
func devIndex(d DevID) int {
	for x, e := range poolDevTable[devices[d].Pool] {
		if e == d {
			return x
		}
	}
	return -1
}

// pmemIndex is the index of a persistent device in State.PDev.
func pmemIndex(d DevID) int { return int(d - cPmem0) }

const nPDevs = 2

func (d DevID) String() string { return devices[d].Name }

// poolHosts are the VMs that a pool server reaches (its controller's
// nodeSelector).
var poolHosts = [nPools][]VM{
	PoolA: {VM1, VM2},
	PoolB: {VM0},
	PoolC: {VM0, VM1, VM2},
}

// poolBridge is the host bridge of the slots of a pool in every VM.
var poolBridge = [nPools]Bridge{PoolA: HB0, PoolB: HB0, PoolC: HB1}

func poolHasHost(p Pool, v VM) bool {
	for _, h := range poolHosts[p] {
		if h == v {
			return true
		}
	}
	return false
}

// serialHex is the env key of a serial (CXL_<KEY>_...): upper-case hex.
func serialKey(serial uint64) string {
	return strings.ToUpper(strconv.FormatUint(serial, 16))
}

// formatSerial is the 0x<hex> form of a serial.
func formatSerial(serial uint64) string {
	return "0x" + strconv.FormatUint(serial, 16)
}

// parseSerial parses 0x<hex> (any case).
func parseSerial(s string) (uint64, bool) {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	v, err := strconv.ParseUint(s, 16, 64)
	return v, err == nil
}

// Res is a resource that slots hold: a pool device (0..5) or the local
// memdev of a VM (resLocal0+vm); resNone for lease and plain slots.
type Res int8

const (
	resLocal0 Res = nDevs
	nRes          = nDevs + nVMs
	resNone   Res = -1
)

func localRes(v VM) Res { return resLocal0 + Res(v) }

func (r Res) String() string {
	switch {
	case r >= 0 && r < resLocal0:
		return DevID(r).String()
	case r >= resLocal0 && r < nRes:
		return "local@" + VM(r-resLocal0).String()
	}
	return "-"
}

// Config is what the model knows of the setup, and how it generates.
type Config struct {
	Profile     string
	Slots       []Slot
	LocalBlocks [nVMs]int     // blocks of 128Mi of the local memdev of each VM
	NUMANodeHB  [nBridges]int // NUMA node of the regions under each host bridge
	Unsettled   int           // operations in flight before a forced settle
	MaxRestarts int           // crash/driver-kill only below this many restarts
	Online      bool          // "any" slots and nondeterministic outcomes
	Avoid       []string      // MBT_AVOID rules
	AvoidHoles  bool          // never destroy a region below the top of its stack (A9)
	PlanStates  int           // states the planner caches per plan at most (canon.go); 0: no cache
	// StallWindow: a stalled contender (A19) must be promoted this long
	// after the settle that first saw it stalled (--stall-window).
	// StallStrict (the window is not longer than the settle timeout: a
	// kube-scheduler that requeues at once, A28): a stalled contender is not
	// settled, the settle waits for its promotion; else it is settled
	// in both states.
	StallWindow time.Duration
	StallStrict bool
	// Fencing: the fencing mode of the run (--fencing, A36); switch
	// starts in allowed-mems and generates fencing-switch
	Fencing FencingRun
	// Reboot: the node-reboot variant of the run (--reboot, A41): none
	// (the verb is not generated), a guest reboot or a qemu restart
	Reboot RebootRun
	// DRAMNUMANodes are the NUMA nodes of DRAM of every VM (D of A38)
	DRAMNUMANodes []int
	// Verbs that the generator may use; nil: every verb.
	slotByName map[string]int
}

// FencingRun is the fencing mode of a run (A36).
type FencingRun uint8

const (
	FencingAM FencingRun = iota
	FencingNone
	FencingSwitch
)

var fencingRunNames = []string{"allowed-mems", "none", "switch"}

func (f FencingRun) String() string { return fencingRunNames[f] }

// parseFencingRun parses allowed-mems, none or switch.
func parseFencingRun(s string) (FencingRun, error) {
	for f, n := range fencingRunNames {
		if n == s {
			return FencingRun(f), nil
		}
	}
	return 0, fmt.Errorf("invalid fencing mode %q (allowed-mems, none or switch)", s)
}

// RebootRun is the node-reboot variant of a run (A41): none, guest
// (systemctl reboot in the guest, qemu keeps running) or qemu (the qemu
// process restarts: a power cycle).
type RebootRun uint8

const (
	RebootNone RebootRun = iota
	RebootGuest
	RebootQemu
)

var rebootRunNames = []string{"", "guest", "qemu"}

func (r RebootRun) String() string { return rebootRunNames[r] }

// parseRebootRun parses "", guest or qemu.
func parseRebootRun(s string) (RebootRun, error) {
	for r, n := range rebootRunNames {
		if n == s {
			return RebootRun(r), nil
		}
	}
	return 0, fmt.Errorf("invalid reboot variant %q (guest, qemu, or empty: none)", s)
}

// rebootVM: a VM that node-reboot may reboot: vm0 and vm1; vm2 runs the
// control plane (A42).
func rebootVM(v VM) bool { return v == VM0 || v == VM1 }

// defaultConfig returns the configuration of the contract's defaults.
func defaultConfig() *Config {
	return &Config{
		PlanStates:    defaultPlanStates,
		StallWindow:   defaultStallWindow,
		Profile:       "fullp",
		LocalBlocks:   [nVMs]int{VM0: 2, VM1: 2, VM2: 8},
		NUMANodeHB:    [nBridges]int{HB0: 2, HB1: 3},
		Unsettled:     3,
		MaxRestarts:   2,
		Online:        true,
		DRAMNUMANodes: []int{0, 1},
	}
}

// parseLocalBlocks parses vm2=8,vm1=2,vm0=2.
func parseLocalBlocks(s string, into *[nVMs]int) error {
	if s == "" {
		return nil
	}
	for _, f := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
		if !ok {
			return fmt.Errorf("--local-blocks: %q is not VM=BLOCKS", f)
		}
		vm, err := parseVM(k)
		if err != nil || vm == VMAny {
			return fmt.Errorf("--local-blocks: %q: invalid VM", f)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("--local-blocks: %q: invalid number of blocks", f)
		}
		into[vm] = n
	}
	return nil
}

func formatLocalBlocks(b [nVMs]int) string {
	return fmt.Sprintf("vm2=%d,vm1=%d,vm0=%d", b[VM2], b[VM1], b[VM0])
}

// bridgeNUMANode is the NUMA node of the regions under a host bridge.
func (c *Config) bridgeNUMANode(b Bridge) int { return c.NUMANodeHB[b] }

// cxlNUMANodes are the NUMA nodes of CXL memory (fenced nodes are among them).
func (c *Config) cxlNUMANodes() []int { return []int{c.NUMANodeHB[HB0], c.NUMANodeHB[HB1]} }

// NUMANodeBridge returns the host bridge of a NUMA node, -1 for others.
func (c *Config) NUMANodeBridge(node int) Bridge {
	for b := Bridge(0); b < nBridges; b++ {
		if c.NUMANodeHB[b] == node {
			return b
		}
	}
	return -1
}
