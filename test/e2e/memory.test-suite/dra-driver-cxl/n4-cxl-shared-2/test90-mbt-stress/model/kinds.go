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

// Allocation kinds (contract.txt 2) and slots (3).

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind is an allocation kind: the claim of a pod.
type Kind int8

const (
	KXNUMA  Kind = iota // exclusive pool device, numa
	KXDax               // exclusive pool device, dax
	KSDax               // a share of the shared pool device, dax
	KSRo                // a read-only share
	KSExcl              // the shared pool device without shares: exclusive
	KLNUMA              // a 128Mi numa slice of the local memdev
	KLDax               // a 128Mi dax slice
	KLSteer             // a numa slice with steering policy auto
	KLease              // a lease claim
	KPlain              // no claim
	// persistent pool devices (A22): c-pmem0 shared, c-pmem1 exclusive
	KPWr  // the writer: c-pmem0 whole (no shares), block rw
	KPFsd // a share, fs direct (read-only)
	KPFsc // a share, fs cached at /models (read-only)
	KPBro // a share, block read-only
	KPBad // a share, block rw: refused by the config at every Prepare
	KXPb  // c-pmem1, block rw (writer)
	KXPf  // c-pmem1, fs direct (read-only)
	KPlse // a lease claim whose leases are of c-pmem0 (pbw, pbr, pfd, pfc)
	// two claims in one container (A39): a local numa slice (node 2) and
	// pool-c's exclusive device c-pooled0 in numa (node 3)
	KDNUMA
	nKinds
)

// kindNames are the families (the kind without its pool).
var kindNames = [nKinds]string{"xnuma", "xdax", "sdax", "sro", "sexcl", "lnuma", "ldax", "lsteer", "lease", "plain",
	"pwr", "pfsd", "pfsc", "pbro", "pbad", "xpb", "xpf", "plse", "dnuma"}

func (k Kind) String() string { return kindNames[k] }

// pooled: the kind has a pool device.
func (k Kind) pooled() bool { return k <= KSExcl || k.persistent() || k == KDNUMA }

// poolSuffix: the kind's name has its pool (xnuma-a); dnuma has pool c
// only.
func (k Kind) poolSuffix() bool { return k.pooled() && k != KDNUMA }

// dual: the pod has two claims, its pool device and a local slice (A39).
// Its pool claim is the slot's claim (Res, Dev); the local one is a
// second user of the decoder stacks (State.secondUser) and holds a block
// of the local memdev of its VM.
func (k Kind) dual() bool { return k == KDNUMA }

// hasLocal: the kind holds a block of the local memdev of its VM.
func (k Kind) hasLocal() bool { return k.local() || k == KDNUMA }

// persistent: the kind has a persistent pool device (A22).
func (k Kind) persistent() bool { return k >= KPWr && k <= KXPf }

// fsKind: the claim mounts an erofs of a persistent device (read-only).
func (k Kind) fsKind() bool { return k == KPFsd || k == KPFsc || k == KXPf }

// blockKind: the claim is a block device of a persistent device.
func (k Kind) blockKind() bool { return k == KPWr || k == KPBro || k == KPBad || k == KXPb }

// writer: the claim may write a persistent device (block rw, the whole
// device); its Prepare creates the fsdax namespace of a fresh device.
func (k Kind) writer() bool { return k == KPWr || k == KXPb }

// wholeShared: the claim takes a shared device without shares: its
// attachment is exclusive.
func (k Kind) wholeShared() bool { return k == KSExcl || k == KPWr }

// leaseKind: the slot's claim is a lease claim.
func (k Kind) leaseKind() bool { return k == KLease || k == KPlse }

// local: the kind has a slice of the local memdev.
func (k Kind) local() bool { return k == KLNUMA || k == KLDax || k == KLSteer }

// numa: the kind's memory is online on a node of the container.
func (k Kind) numa() bool { return k == KXNUMA || k == KLNUMA || k == KLSteer || k == KDNUMA }

// dax: the container has a dax device to write and read.
func (k Kind) dax() bool {
	return k == KXDax || k == KSDax || k == KSRo || k == KSExcl || k == KLDax
}

// share: the kind takes a share of a shared device.
func (k Kind) share() bool {
	return k == KSDax || k == KSRo || k == KPFsd || k == KPFsc || k == KPBro || k == KPBad
}

// onShared: the kind uses the shared device of its pool.
func (k Kind) onShared() bool { return k == KSDax || k == KSRo || k == KSExcl }

// claimed: the pod has a claim.
func (k Kind) claimed() bool { return k != KPlain }

func parseKind(s string) (Kind, Pool, error) {
	fam, p, hasPool := strings.Cut(s, "-")
	for k := Kind(0); k < nKinds; k++ {
		if kindNames[k] != fam {
			continue
		}
		if k.poolSuffix() != hasPool {
			break
		}
		if k == KDNUMA {
			return k, PoolC, nil
		}
		if !hasPool {
			return k, noPool, nil
		}
		pool, err := parsePool(p)
		if err != nil {
			return 0, noPool, fmt.Errorf("kind %q: %w", s, err)
		}
		return k, pool, nil
	}
	return 0, noPool, fmt.Errorf("invalid kind %q", s)
}

// Slot is a pod slot KIND@VM#I.
type Slot struct {
	Kind  Kind
	Pool  Pool // pool kinds only
	VM    VM   // VMAny for "any" slots
	Index int
	Name  string // KIND@VM#I
	Res   Res    // the resource the slot's claim holds
	Dev   DevID  // pool kinds: the device
}

// KindName is the kind with its pool (xnuma-a).
func (s *Slot) KindName() string {
	if s.Kind.poolSuffix() {
		return s.Kind.String() + "-" + s.Pool.String()
	}
	return s.Kind.String()
}

// Label is the value of the label mbt.slot.
func (s *Slot) Label() string {
	return s.KindName() + "_" + s.VM.String() + "_" + strconv.Itoa(s.Index)
}

// Any: the scheduler chooses the node.
func (s *Slot) Any() bool { return s.VM == VMAny }

// hostOK tells if a pod of the slot may run on a VM.
func (s *Slot) hostOK(v VM) bool {
	if s.Kind.pooled() {
		if !poolHasHost(s.Pool, v) {
			return false
		}
	}
	return s.Any() || s.VM == v
}

// numaNode is the node of the numa memory of the slot's claim, -1 if none
// (dnuma: of its pool claim).
func (s *Slot) numaNode(c *Config) int {
	switch s.Kind {
	case KXNUMA, KDNUMA:
		return c.bridgeNUMANode(poolBridge[s.Pool])
	case KLNUMA, KLSteer:
		return c.bridgeNUMANode(HB0)
	}
	return -1
}

// numaNodes are the nodes of the numa memory of the slot's claims (E of
// A38 without lent nodes).
func (s *Slot) numaNodes(c *Config) []int {
	switch {
	case s.Kind == KDNUMA:
		return []int{c.bridgeNUMANode(HB0), c.bridgeNUMANode(HB1)}
	case s.Kind.numa():
		return []int{s.numaNode(c)}
	}
	return nil
}

// holdsRes: the slot's claims take the resource (dnuma: its pool device
// and the local memdev of its VM).
func (s *Slot) holdsRes(r Res) bool {
	return r != resNone && (s.Res == r || (s.Kind == KDNUMA && r == localRes(s.VM)))
}

// parseSlot parses KIND@VM#I.
func parseSlot(name string) (Slot, error) {
	kind, rest, ok := strings.Cut(name, "@")
	if !ok {
		return Slot{}, fmt.Errorf("slot %q: not KIND@VM#I", name)
	}
	vms, idx, ok := strings.Cut(rest, "#")
	if !ok {
		return Slot{}, fmt.Errorf("slot %q: not KIND@VM#I", name)
	}
	k, p, err := parseKind(kind)
	if err != nil {
		return Slot{}, fmt.Errorf("slot %q: %w", name, err)
	}
	vm, err := parseVM(vms)
	if err != nil {
		return Slot{}, fmt.Errorf("slot %q: %w", name, err)
	}
	i, err := strconv.Atoi(idx)
	if err != nil || i < 0 || strconv.Itoa(i) != idx {
		return Slot{}, fmt.Errorf("slot %q: invalid index", name)
	}
	s := Slot{Kind: k, Pool: p, VM: vm, Index: i, Name: name, Res: resNone, Dev: -1}
	switch {
	case k.pooled():
		if vm != VMAny && !poolHasHost(p, vm) {
			return Slot{}, fmt.Errorf("slot %q: pool %s has no host %s", name, p, vm)
		}
		switch {
		case k.persistent() && p != PoolC:
			return Slot{}, fmt.Errorf("slot %q: persistent devices are pool-c's (A22)", name)
		case k == KXPb || k == KXPf:
			s.Dev = cPmem1
		case k.persistent():
			s.Dev = cPmem0
		case k == KDNUMA && vm == VMAny:
			return Slot{}, fmt.Errorf("slot %q: a local kind needs a VM", name)
		case k.onShared():
			s.Dev = sharedDev(p)
		default:
			s.Dev = pooledDev(p)
		}
		s.Res = Res(s.Dev)
	case k.local():
		if vm == VMAny {
			return Slot{}, fmt.Errorf("slot %q: a local kind needs a VM", name)
		}
		s.Res = localRes(vm)
	case vm == VMAny:
		return Slot{}, fmt.Errorf("slot %q: only pool kinds may be on any VM", name)
	}
	return s, nil
}

// profiles are the slot lists. "fullp", the default, is "full" with the
// persistent slots; "small" is the contract's; "tiny" is for unit tests
// and quick runs.
var profiles = map[string][]string{
	"full": {
		// pool a
		"xnuma-a@vm1#0", "xnuma-a@vm2#0", "xdax-a@vm1#0", "xdax-a@vm2#0",
		"sdax-a@vm1#0", "sdax-a@vm2#0", "sdax-a@vm2#1", "sro-a@vm1#0",
		"sro-a@vm2#0", "sexcl-a@vm2#0",
		// pool b
		"xnuma-b@vm0#0", "xdax-b@vm0#0", "sdax-b@vm0#0", "sdax-b@vm0#1",
		"sro-b@vm0#0", "sexcl-b@vm0#0",
		// pool c
		"xnuma-c@vm1#0", "xdax-c@vm2#0", "xdax-c@any#0", "sdax-c@vm0#0",
		"sdax-c@vm1#0", "sdax-c@vm2#0", "sdax-c@any#0", "sro-c@vm0#0",
		"sro-c@vm2#0", "sexcl-c@vm1#0",
		// local
		"lnuma@vm0#0", "lnuma@vm0#1", "ldax@vm0#0", "lnuma@vm1#0", "ldax@vm1#0",
		"ldax@vm1#1", "lnuma@vm2#0", "lnuma@vm2#1", "ldax@vm2#0", "lsteer@vm2#0",
		"lsteer@vm1#0",
		// lease
		"lease@vm0#0", "lease@vm1#0", "lease@vm2#0",
		// plain
		"plain@vm0#0", "plain@vm1#0", "plain@vm2#0",
		// both CXL NUMA nodes of a VM (A39): pool-c numa slots on every VM
		// and the two-claim kind
		"xnuma-c@vm0#0", "xnuma-c@vm2#0", "dnuma@vm0#0", "dnuma@vm1#0",
	},
	"small": {
		"xnuma-a@vm1#0", "xdax-a@vm2#0", "sdax-c@vm0#0", "sdax-c@vm2#0",
		"sro-c@vm1#0", "sexcl-c@vm1#0", "lnuma@vm0#0", "lnuma@vm0#1",
		"ldax@vm0#0", "lease@vm2#0", "plain@vm0#0", "plain@vm2#0",
	},
	"tiny": {
		"xnuma-a@vm1#0", "xdax-a@vm2#0", "sdax-c@vm0#0", "sexcl-c@vm1#0",
		"lnuma@vm0#0", "ldax@vm0#0", "lsteer@vm2#0", "lease@vm0#0", "plain@vm0#0",
	},
	// persistent devices (A22):
	// a writer on two VMs, both accesses on two VMs, a block reader,
	// fs leases, volatile hb1 neighbours for A9
	"pmem": {
		"pwr-c@vm0#0", "pwr-c@vm2#0", "pfsd-c@vm1#0", "pfsd-c@vm2#0",
		"pfsc-c@vm1#0", "pfsc-c@vm2#0", "pbro-c@vm0#0", "plse@vm1#0",
		"plse@vm2#0", "sdax-c@vm0#0", "xnuma-c@vm1#0",
	},
}

// persistentSlots are fullp's slots beyond full (3.3, with c-pmem1).
var persistentSlots = []string{
	"pwr-c@vm0#0", "pwr-c@vm2#0", "pwr-c@any#0", "pfsd-c@vm1#0",
	"pfsd-c@vm2#0", "pfsd-c@vm2#1", "pfsd-c@any#0", "pfsc-c@vm0#0",
	"pfsc-c@vm1#0", "pfsc-c@vm2#0", "pbro-c@vm1#0", "pbad-c@vm0#0",
	"plse@vm0#0", "plse@vm1#0", "plse@vm2#0", "xpb-c@vm1#0", "xpf-c@vm0#0",
}

func init() {
	profiles["fullp"] = append(append([]string(nil), profiles["full"]...), persistentSlots...)
	// "union": the slots of full and small (replays of runs that used
	// both, like the executor's stub lists)
	seen := map[string]bool{}
	var u []string
	for _, n := range append(append([]string(nil), profiles["full"]...), profiles["small"]...) {
		if !seen[n] {
			seen[n] = true
			u = append(u, n)
		}
	}
	profiles["union"] = u
}

func profileNames() []string {
	names := make([]string, 0, len(profiles))
	for n := range profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// loadProfile sets the slots of the configuration from a profile. Off-line
// (cfg.Online false) "any" slots are left out.
func (c *Config) loadProfile(name string) error {
	list, ok := profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q (%s)", name, strings.Join(profileNames(), ", "))
	}
	c.Profile = name
	c.Slots = nil
	c.slotByName = map[string]int{}
	for _, n := range list {
		s, err := parseSlot(n)
		if err != nil {
			return err
		}
		if s.Any() && !c.Online {
			continue
		}
		if _, dup := c.slotByName[n]; dup {
			return fmt.Errorf("profile %s: slot %s twice", name, n)
		}
		c.slotByName[n] = len(c.Slots)
		c.Slots = append(c.Slots, s)
	}
	return nil
}

// slotIndex returns the index of a slot by name, -1 if there is none.
func (c *Config) slotIndex(name string) int {
	if i, ok := c.slotByName[name]; ok {
		return i
	}
	return -1
}

// slotByLabel returns the index of a slot by its label, -1 if none.
func (c *Config) slotByLabel(label string) int {
	for i := range c.Slots {
		if c.Slots[i].Label() == label {
			return i
		}
	}
	return -1
}

// poolsUsed are the pools of the profile's slots.
func (c *Config) poolsUsed() []Pool {
	var used [nPools]bool
	for i := range c.Slots {
		if c.Slots[i].Kind.pooled() {
			used[c.Slots[i].Pool] = true
		}
	}
	var ps []Pool
	for p := Pool(0); p < nPools; p++ {
		if used[p] {
			ps = append(ps, p)
		}
	}
	return ps
}
