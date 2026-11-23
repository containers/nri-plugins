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

// Actions (contract.txt 4): their preconditions and what they change
// in the model. The rules are those of contract.txt.

import (
	"fmt"
	"strings"
)

// Verb is the verb of an action.
type Verb uint8

const (
	VNone Verb = iota
	VSettle
	VCreate
	VDelete
	VCrash
	VWrite
	VRead
	VLeaseGet
	VLeasePut
	VDriverKill
	VDriverRestart
	VCtrlKill
	VCtrlStart
	VCtrlRestart
	VServerRestart
	VKubeletRestart
	VContainerdRestart
	// persistent devices (A23)
	VPWrite       // pwrite SLOT img|junk
	VPRead        // pread SLOT
	VHold         // hold SLOT
	VUnhold       // unhold SLOT
	VPWipe        // pwipe P DEV
	VDriverKillAt // driver-kill-at VM MARKER
	// fencing-switch: the other fencing mode, the drivers of every VM
	// restarted (A37)
	VFencingSwitch
	// node-reboot VM: a reboot of vm0 or vm1 (A41, A42)
	VNodeReboot
	nVerbs
)

var verbNames = [nVerbs]string{"", "settle", "create", "delete", "crash", "write", "read",
	"lease-get", "lease-put", "driver-kill", "driver-restart", "ctrl-kill", "ctrl-start",
	"ctrl-restart", "server-restart", "kubelet-restart", "containerd-restart",
	"pwrite", "pread", "hold", "unhold", "pwipe", "driver-kill-at", "fencing-switch", "node-reboot"}

func (v Verb) String() string { return verbNames[v] }

func parseVerb(s string) (Verb, bool) {
	for v := VSettle; v < nVerbs; v++ {
		if verbNames[v] == s {
			return v, true
		}
	}
	return VNone, false
}

// slotVerb: the verb takes a slot.
func (v Verb) slotVerb() bool {
	return (v >= VCreate && v <= VLeasePut) || (v >= VPWrite && v <= VUnhold)
}

// vmFault: the verb is a fault of a VM.
func (v Verb) vmFault() bool {
	return v == VDriverKill || v == VDriverRestart || v == VKubeletRestart || v == VContainerdRestart || v == VDriverKillAt || v == VNodeReboot
}

// poolFault: the verb is a fault of a pool.
func (v Verb) poolFault() bool {
	return v == VCtrlKill || v == VCtrlStart || v == VCtrlRestart || v == VServerRestart
}

// fault: a VM fault, a pool fault or fencing-switch (a fault of every VM).
func (v Verb) fault() bool { return v.vmFault() || v.poolFault() || v == VFencingSwitch }

// Act is an action.
type Act struct {
	Verb Verb
	Slot int16 // slot verbs: the slot index
	VM   VM    // VM faults
	Pool Pool  // pool faults (and pwipe)
	Mode LeaseMode
	// Arg: pwrite 0 img, 1 junk; driver-kill-at the marker; pwipe the
	// device (DevID)
	Arg uint8
}

// argName is the second argument of pwrite, pwipe and driver-kill-at.
func (a Act) argName() string {
	switch a.Verb {
	case VPWrite:
		return [...]string{"img", "junk"}[a.Arg]
	case VPWipe:
		return DevID(a.Arg).String()
	case VDriverKillAt:
		return markerNames[a.Arg]
	}
	return ""
}

// name is the action line (VERB ARGS...).
func (a Act) name(cfg *Config) string {
	switch {
	case a.Verb == VSettle:
		return "settle"
	case a.Verb == VFencingSwitch:
		return "fencing-switch"
	case a.Verb == VLeaseGet:
		return fmt.Sprintf("lease-get %s %s", cfg.Slots[a.Slot].Name, a.Mode)
	case a.Verb == VPWrite:
		return "pwrite " + cfg.Slots[a.Slot].Name + " " + a.argName()
	case a.Verb.slotVerb():
		return a.Verb.String() + " " + cfg.Slots[a.Slot].Name
	case a.Verb == VDriverKillAt:
		return a.Verb.String() + " " + a.VM.String() + " " + a.argName()
	case a.Verb.vmFault():
		return a.Verb.String() + " " + a.VM.String()
	case a.Verb.poolFault():
		return a.Verb.String() + " " + a.Pool.String()
	case a.Verb == VPWipe:
		return "pwipe " + a.Pool.String() + " " + a.argName()
	}
	return "none"
}

// format is the action format of gofmbt (C3 pairs): the verb and the kind
// family, without the pool, the node and the index.
func (a Act) format(cfg *Config) (string, []any) {
	switch {
	case a.Verb == VSettle:
		return "settle", nil
	case a.Verb == VFencingSwitch:
		return "fencing-switch", nil
	case a.Verb.slotVerb():
		sl := &cfg.Slots[a.Slot]
		f := a.Verb.String() + " " + sl.Kind.String()
		args := []any{}
		if sl.Kind.poolSuffix() {
			f += "-%s"
			args = append(args, sl.Pool.String())
		}
		f += "@%s#%d"
		args = append(args, sl.VM.String(), sl.Index)
		if a.Verb == VLeaseGet {
			f += " " + a.Mode.String()
		}
		if a.Verb == VPWrite {
			f += " " + a.argName()
		}
		return f, args
	case a.Verb == VDriverKillAt:
		return a.Verb.String() + " %s " + a.argName(), []any{a.VM.String()}
	case a.Verb.vmFault():
		return a.Verb.String() + " %s", []any{a.VM.String()}
	case a.Verb.poolFault():
		return a.Verb.String() + " %s", []any{a.Pool.String()}
	case a.Verb == VPWipe:
		return "pwipe %s " + a.argName(), []any{a.Pool.String()}
	}
	return "none", nil
}

// parseAct parses an action line.
func parseAct(cfg *Config, line string) (Act, error) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return Act{}, fmt.Errorf("empty action")
	}
	v, ok := parseVerb(f[0])
	if !ok {
		return Act{}, fmt.Errorf("unknown verb %q", f[0])
	}
	a := Act{Verb: v, Slot: -1, VM: VMAny, Pool: noPool}
	want := 1
	switch {
	case v == VSettle || v == VFencingSwitch:
		want = 0
	case v.slotVerb():
		if len(f) < 2 {
			return Act{}, fmt.Errorf("%s: no slot", f[0])
		}
		i := cfg.slotIndex(f[1])
		if i < 0 {
			return Act{}, fmt.Errorf("%s: slot %q is not in profile %s", f[0], f[1], cfg.Profile)
		}
		a.Slot = int16(i)
		if v == VLeaseGet {
			want = 2
			if len(f) < 3 {
				return Act{}, fmt.Errorf("lease-get: no mode")
			}
			m, ok := parseLeaseMode(f[2])
			if !ok || m.persistent() != (cfg.Slots[i].Kind == KPlse) {
				return Act{}, fmt.Errorf("lease-get: invalid mode %q for %s", f[2], f[1])
			}
			a.Mode = m
		}
		if v == VPWrite {
			want = 2
			if len(f) < 3 {
				return Act{}, fmt.Errorf("pwrite: no img or junk")
			}
			switch f[2] {
			case "img":
			case "junk":
				a.Arg = 1
			default:
				return Act{}, fmt.Errorf("pwrite: %q is not img or junk", f[2])
			}
		}
	case v.vmFault():
		if len(f) < 2 {
			return Act{}, fmt.Errorf("%s: no VM", f[0])
		}
		vm, err := parseVM(f[1])
		if err != nil || vm == VMAny {
			return Act{}, fmt.Errorf("%s: invalid VM %q", f[0], f[1])
		}
		if v == VNodeReboot && !rebootVM(vm) {
			return Act{}, fmt.Errorf("node-reboot: %s runs the control plane (vm0 or vm1, A42)", vm)
		}
		a.VM = vm
		if v == VDriverKillAt {
			want = 2
			if len(f) < 3 {
				return Act{}, fmt.Errorf("driver-kill-at: no marker")
			}
			mk, ok := parseMarker(f[2])
			if !ok {
				return Act{}, fmt.Errorf("driver-kill-at: invalid marker %q", f[2])
			}
			a.Arg = uint8(mk)
		}
	case v == VPWipe:
		want = 2
		if len(f) < 3 {
			return Act{}, fmt.Errorf("pwipe: POOL DEV")
		}
		p, err := parsePool(f[1])
		if err != nil {
			return Act{}, fmt.Errorf("pwipe: %w", err)
		}
		d := DevID(-1)
		for x := DevID(0); x < nDevs; x++ {
			if devices[x].Name == f[2] && devices[x].Persistent && devices[x].Pool == p {
				d = x
			}
		}
		if d < 0 {
			return Act{}, fmt.Errorf("pwipe: %q is not a persistent device of pool %s", f[2], p)
		}
		a.Pool, a.Arg = p, uint8(d)
	case v.poolFault():
		if len(f) < 2 {
			return Act{}, fmt.Errorf("%s: no pool", f[0])
		}
		p, err := parsePool(f[1])
		if err != nil {
			return Act{}, fmt.Errorf("%s: %w", f[0], err)
		}
		a.Pool = p
	}
	if len(f) != want+1 {
		return Act{}, fmt.Errorf("%q: %d arguments, want %d", line, len(f)-1, want)
	}
	return a, nil
}

// allActs is every action of the configuration, MBT_AVOID rules left
// out. Faults of pools are those of the pools of the profile.
func allActs(cfg *Config) []Act {
	var acts []Act
	add := func(a Act) {
		if !cfg.avoided(a) {
			acts = append(acts, a)
		}
	}
	add(Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool})
	for i := range cfg.Slots {
		sl := &cfg.Slots[i]
		for _, v := range []Verb{VCreate, VDelete, VCrash} {
			add(Act{Verb: v, Slot: int16(i), VM: VMAny, Pool: noPool})
		}
		if sl.Kind.dax() {
			add(Act{Verb: VWrite, Slot: int16(i), VM: VMAny, Pool: noPool})
			add(Act{Verb: VRead, Slot: int16(i), VM: VMAny, Pool: noPool})
		}
		if sl.Kind == KLease {
			add(Act{Verb: VLeaseGet, Slot: int16(i), VM: VMAny, Pool: noPool, Mode: ModeNUMA})
			add(Act{Verb: VLeaseGet, Slot: int16(i), VM: VMAny, Pool: noPool, Mode: ModeDax})
			add(Act{Verb: VLeasePut, Slot: int16(i), VM: VMAny, Pool: noPool})
		}
		slotAct := func(v Verb, arg uint8) {
			add(Act{Verb: v, Slot: int16(i), VM: VMAny, Pool: noPool, Arg: arg})
		}
		if sl.Kind.persistent() && sl.Kind != KPBad {
			slotAct(VPWrite, 0)
			if sl.Kind.writer() {
				slotAct(VPWrite, 1)
			}
			slotAct(VPRead, 0)
			if sl.Kind.fsKind() {
				slotAct(VHold, 0)
				slotAct(VUnhold, 0)
			}
		}
		if sl.Kind == KPlse {
			for _, m := range []LeaseMode{ModePBW, ModePBR, ModePFD, ModePFC} {
				add(Act{Verb: VLeaseGet, Slot: int16(i), VM: VMAny, Pool: noPool, Mode: m})
			}
			add(Act{Verb: VLeasePut, Slot: int16(i), VM: VMAny, Pool: noPool})
			slotAct(VPWrite, 0)
			slotAct(VPWrite, 1)
			slotAct(VPRead, 0)
			slotAct(VHold, 0)
			slotAct(VUnhold, 0)
		}
	}
	for _, v := range []Verb{VDriverKill, VDriverRestart, VKubeletRestart, VContainerdRestart} {
		for vm := VM(0); vm < nVMs; vm++ {
			add(Act{Verb: v, Slot: -1, VM: vm, Pool: noPool})
		}
	}
	if cfg.devUsed(cPmem0) || cfg.devUsed(cPmem1) {
		// the markers of a persistent Prepare ("bind": of an fs
		// lease's, A29 d)
		for vm := VM(0); vm < nVMs; vm++ {
			for mk := range markerNames {
				add(Act{Verb: VDriverKillAt, Slot: -1, VM: vm, Pool: noPool, Arg: uint8(mk)})
			}
		}
	}
	for _, d := range []DevID{cPmem0, cPmem1} {
		if cfg.devUsed(d) {
			add(Act{Verb: VPWipe, Slot: -1, VM: VMAny, Pool: PoolC, Arg: uint8(d)})
		}
	}
	for _, v := range []Verb{VCtrlKill, VCtrlStart, VCtrlRestart, VServerRestart} {
		for _, p := range cfg.poolsUsed() {
			add(Act{Verb: v, Slot: -1, VM: VMAny, Pool: p})
		}
	}
	if cfg.Fencing == FencingSwitch {
		add(Act{Verb: VFencingSwitch, Slot: -1, VM: VMAny, Pool: noPool})
	}
	if cfg.Reboot != RebootNone {
		for _, vm := range []VM{VM0, VM1} {
			add(Act{Verb: VNodeReboot, Slot: -1, VM: vm, Pool: noPool})
		}
	}
	return acts
}

// avoided tells if an MBT_AVOID rule leaves an action out. A rule is a
// verb ("containerd-restart") or VERB:ARG, where ARG is the first
// argument (a slot, VM or pool), the kind of the slot with or without
// its pool (xnuma-a, xnuma), the VM of the slot, or the mode of a
// lease-get. The verb "settle" cannot be avoided.
func (c *Config) avoided(a Act) bool {
	if a.Verb == VSettle {
		return false
	}
	for _, rule := range c.Avoid {
		verb, arg, hasArg := strings.Cut(strings.TrimSpace(rule), ":")
		if verb != a.Verb.String() {
			continue
		}
		if !hasArg {
			return true
		}
		for _, x := range a.args(c) {
			if x == arg {
				return true
			}
		}
	}
	return false
}

// args are the strings an MBT_AVOID argument may match.
func (a Act) args(c *Config) []string {
	switch {
	case a.Verb.slotVerb():
		sl := &c.Slots[a.Slot]
		xs := []string{sl.Name, sl.KindName(), sl.Kind.String(), sl.VM.String()}
		if a.Verb == VLeaseGet {
			xs = append(xs, a.Mode.String())
		}
		if a.Verb == VPWrite {
			xs = append(xs, a.argName())
		}
		return xs
	case a.Verb == VDriverKillAt:
		return []string{a.VM.String(), a.argName()}
	case a.Verb.vmFault():
		return []string{a.VM.String()}
	case a.Verb.poolFault():
		return []string{a.Pool.String()}
	case a.Verb == VPWipe:
		return []string{a.Pool.String(), a.argName()}
	}
	return nil
}

// checkAvoid returns an error for rules that match no verb.
func checkAvoid(rules []string) error {
	for _, rule := range rules {
		verb, _, _ := strings.Cut(strings.TrimSpace(rule), ":")
		if _, ok := parseVerb(verb); !ok {
			return fmt.Errorf("--avoid: %q: unknown verb %q", rule, verb)
		}
	}
	return nil
}

// --- preconditions ---

// forced: the number of unsettled operations allows only a settle.
func (s *State) forced() bool { return int(s.Unsettled) >= s.cfg.Unsettled }

// enabled tells if the generator may take an action in the state.
func (s *State) enabled(a Act) bool { return s.next(a) != nil }

// next returns the state after an action that the generator may take,
// nil if it may not: the preconditions (allowed), the limit of unsettled
// operations, and the rules of the decoder stacks (stackRulesOK).
func (s *State) next(a Act) *State {
	if a.Verb == VSettle {
		if s.Unsettled == 0 {
			return nil
		}
		return s.apply(a)
	}
	if s.forced() && !s.synchronous(a) {
		return nil
	}
	if !s.allowed(a) {
		return nil
	}
	if a.Verb == VDelete && s.slot(int(a.Slot)).Kind.dual() && s.Slots[a.Slot].Phase.unbound() {
		// a dnuma pod deleted before it was bound would leave its local
		// claim in flight in the scheduler too (A19), which the model
		// does not know for local claims (A39)
		return nil
	}
	n := s.apply(a)
	if !s.stackRulesOK(a, n) || !n.stallStacksQuiet(s) || !n.persistentRulesOK(s, a) || !n.dualRulesOK() {
		return nil
	}
	if a.Verb == VNodeReboot && !s.cfg.Online && n.Reorder != 0 {
		// (off-line nothing adopts the order in which kubelet prepares
		// the claims again: at most one region per stack of the VM)
		return nil
	}
	// off-line nothing adopts whether a contender stalled (A19), and
	// nothing ends the hold of a device whose claim was deleted before
	// its pod was bound (no clock): no such delete
	if !s.cfg.Online && (n.count(Stalled) > s.count(Stalled) || (a.Verb == VDelete && s.slot(int(a.Slot)).Kind.pooled() && s.Slots[a.Slot].Phase.unbound())) {
		return nil
	}
	return n
}

// stallStacksQuiet: a contender that the action made stalled gets its
// region whenever the scheduler requeues it (A19): the stacks it may get
// it on have no other region events in the window (they would race).
// One that waits for the releases of its device gets it after them
// (handover): those are no race. One stalled only by the scheduler's
// hold (it fits now) may get it at any moment, also before the release
// of another share of its device there (A21); the prepares of
// other shares make the region once in any order.
func (n *State) stallStacksQuiet(s *State) bool {
	for i := range n.Slots {
		if n.Slots[i].Phase != Stalled || s.Slots[i].Phase == Stalled {
			continue
		}
		sl := n.slot(i)
		key := devKey(sl.Dev)
		now, _ := s.fits(i)
		waits := !now
		var stacks []int
		if si, _, ok := n.userRegion(int16(i)); ok {
			stacks = append(stacks, si)
		} else {
			for _, v := range poolHosts[sl.Pool] {
				stacks = append(stacks, stackIndex(v, poolBridge[sl.Pool]))
			}
		}
		lu := n.dualUser(int16(i))
		if lu >= 0 {
			// a dnuma pod gets its local slice with its promotion: no
			// other event on the hb0 stack of its VM either
			stacks = append(stacks, stackIndex(sl.VM, HB0))
		}
		for _, e := range n.Ev {
			if e.Key == key && (waits || e.Kind == evPrepare) || (lu >= 0 && e.User == lu) {
				continue
			}
			for _, si := range stacks {
				if int(e.Stack) == si || (e.Stack < 0 && poolBridge[devices[e.Key-1].Pool] == stackBridge(si)) {
					return false
				}
			}
		}
	}
	return true
}

// synchronous: the action changes nothing that a settle would wait for
// (it is allowed when the limit of unsettled operations is reached).
func (s *State) synchronous(a Act) bool {
	switch a.Verb {
	case VWrite, VRead, VPWrite, VPRead, VHold, VPWipe:
		return true
	case VUnhold:
		return s.Slots[a.Slot].Lease != LeaseBusy
	}
	return false
}

// persistentRulesOK: the generation limits of persistent devices (A27):
// no reader whose Prepare's outcome is not known (a namespace that a
// writer of the window may or may not have made), and off-line no
// Refused claim next to a region change of pool c on the same VM.
func (n *State) persistentRulesOK(s *State, a Act) bool {
	for i := range n.Slots {
		// (an fs reader is refused without an erofs anyway, and there
		// is none while the namespace is not known: only pbro)
		ss, sl := &n.Slots[i], n.slot(i)
		if sl.Kind != KPBro {
			continue
		}
		if (ss.Phase == Starting || ss.Phase == Stalled) && s.Slots[i].Phase != ss.Phase && n.nsOpen(sl.Dev) {
			return false
		}
	}
	if !n.cfg.Online {
		for v := VM(0); v < nVMs; v++ {
			si := stackIndex(v, HB1)
			refused := false
			for i := range n.Slots {
				if ss := &n.Slots[i]; n.slot(i).Kind.persistent() && ss.Node == v && ss.Refuse && ss.Phase.holding() {
					refused = true
				}
			}
			if !refused {
				continue
			}
			for _, e := range changedStacksOn(s.Ev, n.Ev, si) {
				if !e.Probe {
					return false
				}
			}
		}
	}
	return true
}

// nsOpen: whether the persistent device has its namespace is not known
// before the settle: maybe, or a writer of the device that may or may not
// prepare in this window makes it.
func (n *State) nsOpen(d DevID) bool {
	pd := &n.PDev[pmemIndex(d)]
	if pd.NS == TriMaybe {
		return true
	}
	if pd.NS == TriYes {
		return false
	}
	for i := range n.Slots {
		ss, sl := &n.Slots[i], n.slot(i)
		if sl.Kind.writer() && sl.Dev == d && ss.Phase == Deleting && n.hasPrepare(int16(i)) {
			return true
		}
	}
	return false
}

// changedStacksOn returns the events of a stack that an action added.
func changedStacksOn(before, after []stackEvent, si int) []stackEvent {
	var out []stackEvent
	for j, e := range after {
		if j < len(before) && before[j] == e {
			continue
		}
		if int(e.Stack) == si {
			out = append(out, e)
		}
	}
	return out
}

// count counts the slots in a phase.
func (s *State) count(ph Phase) int {
	c := 0
	for i := range s.Slots {
		if s.Slots[i].Phase == ph {
			c++
		}
	}
	return c
}

// stackRulesOK checks what an action does to the decoder stacks (A9):
// nothing on a stack where a pod waits for its retry after its hole
// cleared (but deleting that pod); off-line, every stack has one
// outcome (no race for the order); with --avoid-holes, no outcome
// leaves a new hole.
func (s *State) stackRulesOK(a Act, n *State) bool {
	for _, si := range changedStacks(s.Ev, n.Ev) {
		if si < 0 {
			// an "any" pod: its node is not known yet, every host of
			// its pool may get its region (A9); also when the event
			// comes from another action (ctrl-start)
			if s.unblockingAnyHost(n.anyPools(s.Ev)) {
				return false
			}
			continue
		}
		if s.unblockingOn(si) && !s.deletesLastUnblocking(a, si) {
			return false
		}
		// a window that clears a hole with pods blocked behind it
		// makes no region: their retries (at kubelet's resync, any
		// time) would race with it for the order (A12)
		for _, o := range n.outcomes(si) {
			if o.Cleared && o.Pushed && (len(o.Blocked) > 0 || s.blockedOn(si)) {
				return false
			}
			// a dnuma pod is never blocked (A39)
			if n.dualBlocked(o) {
				return false
			}
		}
		if s.cfg.Online && !s.cfg.AvoidHoles {
			continue
		}
		outs := n.outcomes(si)
		if !s.cfg.Online && len(outs) != 1 {
			return false
		}
		if s.cfg.AvoidHoles {
			for _, o := range outs {
				if o.S.holes() > n.Stacks[si].holes() {
					return false
				}
			}
		}
	}
	return true
}

// anyPools returns the pools of the events of "any" pods (node not
// known) that the state has and before did not.
func (n *State) anyPools(before []stackEvent) []Pool {
	var out []Pool
	for j, e := range n.Ev {
		if e.Stack >= 0 || (j < len(before) && before[j] == e) {
			continue
		}
		out = append(out, devices[e.Key-1].Pool)
	}
	return out
}

// unblockingAnyHost: a stack of a host of one of the pools has a pod
// that waits for its retry.
func (s *State) unblockingAnyHost(pools []Pool) bool {
	for _, p := range pools {
		for _, v := range poolHosts[p] {
			if s.unblockingOn(stackIndex(v, poolBridge[p])) {
				return true
			}
		}
	}
	return false
}

// changedStacks returns the stacks of events that an action added or
// changed (-1 for an unknown node).
func changedStacks(before, after []stackEvent) []int {
	seen := map[int]bool{}
	var out []int
	for j, e := range after {
		if j < len(before) && before[j] == e {
			continue
		}
		if si := int(e.Stack); !seen[si] {
			seen[si] = true
			out = append(out, si)
		}
	}
	return out
}

// deletesLastUnblocking: the action deletes the only unblocking pod of a
// stack, and nothing else is in flight there (its region, if its retry
// made one, is the top: no hole either way).
func (s *State) deletesLastUnblocking(a Act, si int) bool {
	if a.Verb != VDelete || !s.Slots[a.Slot].Phase.waitsRetry() || len(s.eventsOn(si)) > 0 {
		return false
	}
	for i := range s.Slots {
		if i != int(a.Slot) && s.Slots[i].Phase.waitsRetry() {
			if ssi, _, ok := s.userRegion(int16(i)); ok && ssi == si {
				return false
			}
		}
	}
	return true
}

// leaseRegionOK: a lease slice of a VM gets its region now: nothing in
// flight on the VM's hb0 stack, and the local region exists or can be
// made (else the lease-get would fail after its --wait).
func (s *State) leaseRegionOK(v VM) bool {
	si := stackIndex(v, HB0)
	if len(s.eventsOn(si)) > 0 || s.unblockingOn(si) {
		return false
	}
	st := &s.Stacks[si]
	return st.contains(rkLocal) || !st.hasHole()
}

// allowed tells if an action may be taken in the state when the limit of
// unsettled operations is not applied (the teardown, and replays of it).
// A settle is always allowed.
func (s *State) allowed(a Act) bool {
	if a.Verb == VSettle {
		return true
	}
	if a.Verb.slotVerb() {
		return s.slotEnabled(a)
	}
	switch a.Verb {
	case VDriverKill, VContainerdRestart:
		// a containerd restart restarts the driver container in
		// place too (contract A8): the same back-off
		d := &s.Drv[a.VM]
		return int(d.Restarts) < s.cfg.MaxRestarts && !d.Killed && !s.killAtMayHaveFired(a.VM)
	case VDriverRestart:
		return !s.Drv[a.VM].Killed && !s.killAtMayHaveFired(a.VM)
	case VCtrlKill:
		// off-line, a kill right after a graceful restart would leave
		// open whether the slices came back (A18)
		return s.Ctrl[a.Pool].Up && !s.anyUnknownHolder(a.Pool) && (s.cfg.Online || s.Ctrl[a.Pool].ResourceSlices != SlicesUnknown)
	case VCtrlStart:
		return !s.Ctrl[a.Pool].Up
	case VCtrlRestart:
		// not while a claim of the pool is being allocated: it may or
		// may not have been allocated before the slices went (A18)
		return s.Ctrl[a.Pool].Up && !s.poolStarting(a.Pool)
	case VServerRestart, VKubeletRestart:
		return true
	case VFencingSwitch:
		// a driver-restart of every VM (A37)
		for v := VM(0); v < nVMs; v++ {
			if s.Drv[v].Killed || s.killAtMayHaveFired(v) {
				return false
			}
		}
		// (one per window: a settle between two)
		return s.cfg.Fencing == FencingSwitch && !s.Switched
	case VDriverKillAt:
		// on-line only (A27): the observation tells if it fired
		d := &s.Drv[a.VM]
		return s.cfg.Online && int(d.Restarts) < s.cfg.MaxRestarts && !d.Killed && d.KillAt == 0
	case VPWipe:
		return s.pwipeOK(DevID(a.Arg))
	case VNodeReboot:
		return s.rebootOK(a.VM)
	}
	return false
}

// pwipeOK: nothing holds, wants or has attached the device (the server
// would refuse the wipe, and a claim may be allocating).
func (s *State) pwipeOK(d DevID) bool {
	if s.contested(Res(d)) || s.held(Res(d)) || s.Deallocating&(1<<uint(d)) != 0 {
		return false
	}
	for i := range s.Slots {
		ss, sl := &s.Slots[i], s.slot(i)
		if sl.Kind.pooled() && sl.Dev == d && ss.Phase != Absent {
			return false
		}
		if sl.Kind == KPlse && d == cPmem0 && ss.Lease != LeaseNone {
			return false
		}
	}
	c := &s.Ctrl[devices[d].Pool]
	for v := VM(0); v < nVMs; v++ {
		if c.Att[devIndex(d)][v] != TriNo {
			return false
		}
	}
	for _, e := range s.Ev {
		if e.Key == devKey(d) {
			return false
		}
	}
	return c.Up
}

// killAtMayHaveFired: an armed driver-kill-at watcher of the VM may have
// fired already: the window has a Prepare at its marker, or a refused
// claim's retry makes the region at any time, now or before one of the
// window's steps (KillMay). A driver fault would disarm it (A31), and
// whether the kill came first would be open: not generated then.
func (s *State) killAtMayHaveFired(v VM) bool {
	d := &s.Drv[v]
	return d.KillAt != 0 && (d.KillMay || s.killCandidate(v, int(d.KillAt-1)))
}

// markKillMay remembers for the window that an armed watcher may have
// fired before a step (killAnyTime): a step may take the claim whose
// Prepare could show its marker (A32: a refused reader's retry fired
// the watcher after the settle's observation had seen it armed, and its
// delete hid the kill from the next settle and let a containerd-restart
// be generated).
func (n *State) markKillMay() {
	for v := VM(0); v < nVMs; v++ {
		if d := &n.Drv[v]; d.KillAt != 0 && !d.KillMay && n.killAnyTime(v, int(d.KillAt-1)) {
			d.KillMay = true
		}
	}
}

// killAtBind: a lease-get of an fs lease of slot i comes while a
// driver-kill-at bind watcher is armed on its VM: it may return 0 with
// the lease Ready or fail with the lease released (A29 d).
func (s *State) killAtBind(i int, m LeaseMode) bool {
	return m.fs() && s.Drv[s.slot(i).VM].KillAt == markerBind+1
}

// waitingPBro: a pbro claim waits for the device.
func (s *State) waitingPBro(d DevID) bool {
	w := s.waiter(Res(d))
	return w >= 0 && s.slot(w).Kind == KPBro
}

// pleaseOK: a persistent lease of the mode can be had now (cxl-request
// request --wait 60 s would fail else, A27): the controller of pool c up
// with its slices, the device free for it (no waiter, not contested or
// held in the scheduler), its region available on the VM's hb1 stack,
// the content its Prepare needs, the driver up and no watcher armed.
func (s *State) pleaseOK(i int, m LeaseMode) bool {
	sl := s.slot(i)
	c := &s.Ctrl[PoolC]
	// (under an armed watcher only "bind", whose outcome A29 d defines)
	if k := s.Drv[sl.VM].KillAt; !c.Up || c.ResourceSlices != SlicesPresent || s.Drv[sl.VM].Killed || (k != 0 && int(k-1) != markerBind) {
		return false
	}
	r := Res(cPmem0)
	o := s.occ()
	if o.waiter[r] >= 0 || s.contested(r) || s.held(r) || s.Deallocating&(1<<uint(r)) != 0 || s.unschedHolder(r) {
		return false
	}
	if m == ModePBW {
		if o.exclAll[cPmem0]+o.shareAll[cPmem0] != 0 {
			return false
		}
	} else if o.exclAll[cPmem0] != 0 {
		return false
	}
	pd := &s.PDev[pmemIndex(cPmem0)]
	switch m {
	case ModePBR:
		if pd.NS != TriYes {
			return false
		}
	case ModePFD, ModePFC:
		if pd.NS != TriYes || pd.FS != TriYes {
			return false
		}
	}
	si := stackIndex(sl.VM, HB1)
	if len(s.eventsOn(si)) > 0 || s.unblockingOn(si) {
		return false
	}
	st := &s.Stacks[si]
	return st.contains(devKey(cPmem0)) || !st.hasHole()
}

// leaseFile: the slot has a persistent fs lease whose file can be read
// or held (Ready, or Releasing while held).
func (ss *SlotState) leaseFS() bool {
	return (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) && ss.LeaseMode.fs()
}

func (s *State) slotEnabled(a Act) bool {
	i := int(a.Slot)
	ss := &s.Slots[i]
	sl := s.slot(i)
	switch a.Verb {
	case VCreate:
		if ss.Phase != Absent {
			return false
		}
		if sl.Res == resNone {
			return true
		}
		if sl.Kind.hasLocal() {
			// (dnuma: both of its resources)
			r := localRes(sl.VM)
			if s.localReserved(sl.VM) || (sl.Kind.dual() && (s.occ().waiter[r] >= 0 || s.contested(r))) {
				return false
			}
		}
		// a claim that waits for its pool's slices is the only
		// candidate for its device when they are back (A18)
		return s.occ().waiter[sl.Res] < 0 && !s.contested(sl.Res) && !s.unschedHolder(sl.Res)
	case VDelete:
		if sl.Kind.writer() && ss.Phase == Starting && s.PDev[pmemIndex(sl.Dev)].NS == TriNo && s.waitingPBro(sl.Dev) {
			// the waiting block reader would get the device after a
			// Prepare that may or may not have made the namespace
			return false
		}
		return ss.Phase != Absent && ss.Phase != Deleting
	case VCrash:
		return ss.Phase == Running && int(ss.Restarts) < s.cfg.MaxRestarts
	case VWrite, VRead:
		return ss.Phase == Running
	case VLeaseGet:
		if ss.Phase != Running || ss.Lease != LeaseNone || s.Drv[sl.VM].Killed {
			return false
		}
		if sl.Kind == KPlse {
			return s.pleaseOK(i, a.Mode)
		}
		r := localRes(sl.VM)
		o := s.occ()
		return int(o.localAll[sl.VM])+1 <= s.cfg.LocalBlocks[sl.VM] && o.waiter[r] < 0 && !s.contested(r) && s.leaseRegionOK(sl.VM) && !s.localReserved(sl.VM)
	case VLeasePut:
		return ss.Phase == Running && ss.Lease == LeaseHeld && !s.Drv[sl.VM].Killed && !ss.LeaseOpen
	case VPWrite:
		if ss.Phase != Running {
			return false
		}
		if sl.Kind == KPlse {
			// through the lease (block: its fd, the broker's); not a
			// lease still Lending (A30)
			if ss.Lease != LeaseHeld || ss.LeaseOpen || !ss.LeaseMode.persistent() || (!ss.LeaseMode.fs() && s.Drv[sl.VM].Killed) {
				return false
			}
			return a.Arg == 0 || ss.LeaseMode == ModePBW
		}
		return a.Arg == 0 || sl.Kind.writer()
	case VPRead:
		if ss.Phase != Running {
			return false
		}
		if sl.Kind == KPlse {
			return !ss.LeaseOpen && (ss.leaseFS() || (ss.Lease == LeaseHeld && ss.LeaseMode.persistent() && !s.Drv[sl.VM].Killed))
		}
		return true
	case VHold:
		if ss.Phase != Running || ss.Hold {
			return false
		}
		if sl.Kind == KPlse {
			return ss.Lease == LeaseHeld && ss.LeaseMode.fs() && !ss.LeaseOpen
		}
		return sl.Kind.fsKind()
	case VUnhold:
		return ss.Phase == Running && ss.Hold
	}
	return false
}

// --- state changes ---

// apply returns the state after an enabled action.
func (s *State) apply(a Act) *State {
	n := s.clone()
	n.Steps++
	n.Last = a
	if a.Verb != VSettle {
		n.markKillMay()
		if n.Unsettled == 0 {
			// a new window: what a node-reboot left to the settle of
			// its window is over
			n.Reorder = 0
			for v := range n.Drv {
				n.Drv[v].Rebooted = false
			}
		}
	}
	switch a.Verb {
	case VSettle:
		return n.settleAll()
	case VCreate:
		n.create(int(a.Slot))
	case VDelete:
		n.delete(int(a.Slot))
	case VCrash:
		n.crash(int(a.Slot))
	case VWrite:
		n.write(int(a.Slot))
		return n
	case VRead:
		return n
	case VLeaseGet:
		if a.Mode.persistent() {
			n.leaseGetP(int(a.Slot), a.Mode)
		} else {
			n.leaseGet(int(a.Slot), a.Mode)
		}
	case VLeasePut:
		n.leasePut(int(a.Slot))
	case VDriverKill:
		d := &n.Drv[a.VM]
		// it disarms a driver-kill-at watcher of the VM first (A31)
		d.KillAt, d.KillAtAt, d.KillMay, d.KillSlack = 0, 0, false, false
		d.Restarts++
		d.Inc++
		d.Killed = true
		// (kubelet creates its container again at once, or after
		// the back-off; nothing waits for it before the settle)
		d.Creating = true
		n.driverRestarted(a.VM, SteerOff)
	case VContainerdRestart:
		// The driver exits on the loss of its NRI connection and its
		// container restarts in place (contract A8): like
		// driver-kill (A17: also for steering). The containers keep
		// running. A container being created or started then fails
		// ("error reading from server: EOF") and kubelet creates it
		// again: restarts +1 (A8), as for a kubelet restart.
		d := &n.Drv[a.VM]
		d.KillAt, d.KillAtAt, d.KillMay, d.KillSlack = 0, 0, false, false // (A31)
		d.Restarts++
		d.Inc++
		d.Killed = true
		n.driverRestarted(a.VM, SteerOff)
		n.restartSlack(a.VM)
	case VDriverRestart:
		n.restartDriver(a.VM)
	case VFencingSwitch:
		// the drivers come up with the other mode (A37): vm2, vm1, vm0
		// one at a time, like driver-restart
		n.switchFencing()
		n.Switched = true
		for _, v := range []VM{VM2, VM1, VM0} {
			n.restartDriver(v)
		}
	case VCtrlKill:
		n.ctrlKill(a.Pool)
	case VCtrlStart:
		n.ctrlStart(a.Pool)
	case VKubeletRestart:
		// A kubelet restart keeps the containers; one that it was
		// creating is created again by the new kubelet: restarts +1
		// without a crash (A34). The node's cxl.nri.io
		// slice is gone for a while (the sync delay of the
		// resourceslice controller): local claims wait, which the
		// settle waits for.
		n.restartSlack(a.VM)
		n.driverSlack(a.VM)
	case VCtrlRestart:
		// A graceful stop deletes the pool's slices, the new
		// controller publishes them again (A18): unknown until a
		// settle sees the controller up (then present), kept by a kill.
		// The attachments stay.
		n.Ctrl[a.Pool].ResourceSlices = SlicesUnknown
	case VServerRestart:
		// The specification: nothing observable changes: the pool
		// server reloads its state file.
	case VPWrite:
		n.pwrite(int(a.Slot), a.Arg == 1)
		return n
	case VPRead:
		return n
	case VHold:
		n.Slots[a.Slot].Hold = true
		return n
	case VUnhold:
		ss := &n.Slots[a.Slot]
		ss.Hold = false
		if ss.Lease != LeaseBusy {
			return n
		}
		// the busy release of the fs lease completes (A25)
		n.releasePLease(int(a.Slot))
	case VPWipe:
		// zeros over the LSA and the first 4 MiB of the data (A23)
		n.PDev[pmemIndex(DevID(a.Arg))] = freshPDev
		return n
	case VDriverKillAt:
		// armed; the settle has a world in which it fired (stack.go
		// killWorlds)
		d := &n.Drv[a.VM]
		d.KillAt, d.KillAtAt, d.KillMay, d.KillSlack = a.Arg+1, 0, false, false
	case VNodeReboot:
		n.nodeReboot(a.VM)
		// the settle comes next (only synchronous steps before it): the
		// pods come back in kubelet's time (A43)
		if u := n.cfg.Unsettled - 1; int(n.Unsettled) < u {
			n.Unsettled = uint8(u)
		}
	}
	n.Unsettled++
	return n
}

// restartDriver is a driver-restart of a VM: a new driver pod.
func (n *State) restartDriver(v VM) {
	d := &n.Drv[v]
	d.KillAt, d.KillAtAt, d.KillMay, d.KillSlack = 0, 0, false, false // (A31)
	d.PodGen++
	d.Restarts, d.Slack, d.Creating = 0, 0, false
	d.Inc++
	d.Restarted = true
	if d.PodBefore == "" {
		d.PodBefore = d.Pod
	}
	n.driverRestarted(v, SteerOff)
}

// switchFencing flips the fencing mode (A37). The containers that exist
// at a switch to none keep their cpuset.mems until they are created
// again (Loose): those of running pods and of pods whose container may
// have been created before the switch (starting, restarting, a retry or
// a promotion that comes any time); so do the companions of the leases
// held then. The first fence pass of the drivers after a switch to
// allowed-mems makes every container exact.
func (n *State) switchFencing() {
	if n.Fence == FenceNone {
		n.Fence = FenceAM
		for i := range n.Slots {
			n.Slots[i].Loose, n.Slots[i].CompLoose = false, false
		}
		return
	}
	n.Fence = FenceNone
	for i := range n.Slots {
		ss := &n.Slots[i]
		switch ss.Phase {
		case Running, Restarting, Starting, Unblocking, Stalled:
			ss.Loose = true
		}
		ss.CompLoose = ss.Lease != LeaseNone
	}
}

// localReserved: a dnuma pod of the VM waits for its pool's slices or is
// stalled (A19): its local claim is not allocated yet, but the model
// keeps its block for it (it is the only candidate of its pool device):
// no other claim or lease may take a block of the VM then (it could take
// the block that the dnuma pod's allocation needs).
func (s *State) localReserved(v VM) bool {
	for i := range s.Slots {
		if sl := s.slot(i); sl.Kind.dual() && sl.VM == v && (s.Slots[i].Phase == Unsched || s.Slots[i].Phase == Stalled) {
			return true
		}
	}
	return false
}

// dualRulesOK: no dnuma pod stalled over a decoder hole on one of its
// stacks (its promotion would be Blocked: A39, not modelled).
func (n *State) dualRulesOK() bool {
	for i := range n.Slots {
		sl, ss := n.slot(i), &n.Slots[i]
		if !sl.Kind.dual() || ss.Phase != Stalled {
			continue
		}
		if n.Stacks[stackIndex(sl.VM, HB0)].hasHole() || n.Stacks[stackIndex(sl.VM, poolBridge[sl.Pool])].hasHole() {
			return false
		}
	}
	return true
}

// dualBlocked: a dnuma pod's claim is blocked by a hole in an outcome.
func (s *State) dualBlocked(o *stackOutcome) bool {
	for u, on := range o.Blocked {
		if i, _ := s.userSlot(u); on && s.slot(i).Kind.dual() {
			return true
		}
	}
	return false
}

// refusedNow tells if the Prepare of a persistent claim of slot i is
// refused with the device's content now (A25): pbad by its config, fs
// readers without the namespace and the erofs, pbro without the
// namespace; writers never.
func (n *State) refusedNow(i int) bool {
	sl := n.slot(i)
	if !sl.Kind.persistent() {
		return false
	}
	pd := &n.PDev[pmemIndex(sl.Dev)]
	switch {
	case sl.Kind == KPBad:
		return true
	case sl.Kind.fsKind():
		return pd.NS != TriYes || pd.FS != TriYes
	case sl.Kind == KPBro:
		return pd.NS != TriYes
	}
	return false
}

// prepareEvent records the Prepare of slot i (after: the event that
// released what it gets): a probe for a refused one, none for pbad
// (refused by its config before the memdev wait: no region).
func (n *State) prepareEvent(i int, after int16) int16 {
	ss, sl := &n.Slots[i], n.slot(i)
	if sl.Kind == KPBad {
		return -1
	}
	seq := n.addEvent(int16(i), evPrepare, false, after)
	if seq >= 0 && ss.Refuse {
		n.Ev[seq].Probe = true
	}
	if sl.Kind.dual() {
		// and its local slice, after the same event (the pod was bound
		// after it: kubelet prepares both claims)
		n.addEvent(n.secondUser(i), evPrepare, false, after)
	}
	return seq
}

// pwrite writes the template erofs with the token mbt-<step> (img) or
// zeros (junk) at offset 0 of the device of a writer claim or a pbw lease
// (A23); a read-only claim or fd refuses it.
func (n *State) pwrite(i int, junk bool) {
	if !n.pwriteOK(i) {
		return
	}
	pd := &n.PDev[pmemIndex(n.pdevOf(i))]
	pd.HeadHash = ""
	if junk {
		pd.FS, pd.Raw, pd.Tok = TriNo, tokZero, tokUnknown
		return
	}
	tok := Token{Known: true, Text: fmt.Sprintf("mbt-%d", n.Steps)}
	pd.FS, pd.Raw, pd.Tok = TriYes, tok, tok
}

// pwriteOK: the slot writes its persistent device: a writer claim or a
// pbw lease.
func (s *State) pwriteOK(i int) bool {
	sl := s.slot(i)
	if sl.Kind == KPlse {
		return s.Slots[i].LeaseMode == ModePBW
	}
	return sl.Kind.writer()
}

// pdevOf is the persistent device of a slot (plse: c-pmem0).
func (s *State) pdevOf(i int) DevID {
	if s.slot(i).Kind == KPlse {
		return cPmem0
	}
	return s.slot(i).Dev
}

// preadToken is what pread of slot i reads: hello.txt (fs claims and fs
// leases) or bytes 0-63 (block claims and leases).
func (s *State) preadToken(i int) Token {
	sl, ss := s.slot(i), &s.Slots[i]
	pd := &s.PDev[pmemIndex(s.pdevOf(i))]
	if sl.Kind.fsKind() || (sl.Kind == KPlse && ss.LeaseMode.fs()) {
		return pd.Tok
	}
	return pd.Raw
}

// leaseGetP takes a persistent lease of c-pmem0 (A23): its companion
// claim is allocated, attached and prepared on the requester's node
// before cxl-request returns (synchronous); a pbw lease is a writer: its
// Prepare makes the namespace of a fresh device.
func (n *State) leaseGetP(i int, m LeaseMode) {
	ss := &n.Slots[i]
	ss.Lease, ss.LeaseMode, ss.Hold, ss.CompLoose = LeaseHeld, m, false, false
	n.setLeaseID(i, "")
	n.addEvent(n.secondUser(i), evPrepare, true, -1)
	if m == ModePBW {
		n.madeNamespace(cPmem0)
	}
}

// madeNamespace: a writer's Prepare made the fsdax namespace of a device
// without one (labels and pfn info written: the hashes change).
func (n *State) madeNamespace(d DevID) {
	pd := &n.PDev[pmemIndex(d)]
	if pd.NS == TriYes {
		return
	}
	pd.NS, pd.UUID, pd.LSAHash, pd.HeadHash = TriYes, "", "", ""
	pd.Raw = tokZero
}

// releasePLease releases the persistent lease of a plse slot: its
// companion's Unprepare (the bind, the node mount and the region with the
// last user), the device to a waiter.
func (n *State) releasePLease(i int) {
	n.Slots[i].Lease = LeaseReleasing
	seq := n.addEvent(n.secondUser(i), evUnprepare, false, -1)
	n.promote(Res(cPmem0), seq, false)
}

// restartSlack: a kubelet or containerd restart on a VM may restart once
// more every container of it that is being created or started in the
// window (A34): a pod starting, restarting after a crash, or whose
// Prepare is being retried after its decoder hole cleared.
func (n *State) restartSlack(v VM) {
	for i := range n.Slots {
		ss := &n.Slots[i]
		if ss.Node == v && (ss.Phase == Starting || ss.Phase == Restarting || ss.Phase == Unblocking) && ss.Slack < 255 {
			ss.Slack++
		}
	}
}

// driverSlack: a kubelet restart on a VM may restart the driver pod
// once more if its container may be being created again after a kill
// (A34, for the driver pod: the old kubelet created it after
// driver-kill and stopped before starting it, the new one created it
// again), and in the world in which an armed watcher that may have
// fired before this step did (fireKillAt). The verb waits for the
// driver: a later kubelet restart of the window finds it running.
func (n *State) driverSlack(v VM) {
	d := &n.Drv[v]
	if d.Creating && d.Slack < 255 {
		d.Slack++
	}
	d.Creating = false
	if n.killAtMayHaveFired(v) {
		d.KillSlack = true
	}
}

// create creates the pod of a slot.
func (n *State) create(i int) {
	ss := &n.Slots[i]
	sl := n.slot(i)
	*ss = SlotState{Gen: ss.Gen + 1, Node: sl.VM}
	n.setLeaseID(i, "")
	if sl.Res == resNone {
		n.start(i, -1)
		return
	}
	now, later := n.fits(i)
	if sl.Kind.pooled() && (n.Ctrl[sl.Pool].ResourceSlices == SlicesUnknown || n.Deallocating&(1<<uint(sl.Res)) != 0) {
		// it may try before the slices are published again (F4), or
		// before the claim deleted in the last window is deallocated
		// (F8), and wait for that with every other claim that did:
		// the scheduler picks one of them then; let it be the only
		// candidate
		n.Contested |= 1 << uint(sl.Res)
	}
	switch {
	case now && sl.Kind.pooled() && n.held(sl.Res) && n.Ctrl[sl.Pool].ResourceSlices != SlicesAbsent:
		// the scheduler may still hold the device in flight for a
		// claim deleted before the last settle (A19)
		n.Contested |= 1 << uint(sl.Res)
		n.stall(i)
	case now:
		n.start(i, -1)
	case later:
		// the only candidate for what is being released: a pool
		// device is allocated to it after the old claims are gone;
		// it may stall behind a claim deleted before its pod was
		// bound (A19)
		n.Contested |= 1 << uint(sl.Res)
		if sl.Kind.dual() {
			n.Contested |= 1 << uint(localRes(sl.VM))
		}
		if (n.unboundDeleting(sl.Res) || n.held(sl.Res)) && n.Ctrl[sl.Pool].ResourceSlices != SlicesAbsent {
			n.stall(i)
			return
		}
		n.handover(i, n.start(i, -1))
	default:
		ss.Phase = Pending
	}
}

// start makes a slot Starting: it holds its resource. Its Prepare comes
// in this window (a region event) unless it is a pool claim whose
// controller is down; after is the event that released what it gets.
func (n *State) start(i int, after int16) int16 {
	ss := &n.Slots[i]
	sl := n.slot(i)
	ss.RaceKill, ss.Alloc = false, false
	if sl.Kind.pooled() && n.Ctrl[sl.Pool].ResourceSlices == SlicesAbsent {
		// no slices: the scheduler cannot allocate it (A18)
		ss.Phase = Unsched
		return -1
	}
	ss.Phase = Starting
	ss.Refuse = n.refusedNow(i)
	seq := int16(-1)
	if sl.Kind.local() || (sl.Kind.pooled() && n.Ctrl[sl.Pool].Up) {
		seq = n.prepareEvent(i, after)
	}
	switch sl.Kind {
	case KLSteer:
		ss.Steer = n.newSteer(sl.VM)
	case KXNUMA, KDNUMA:
		// onlined as system RAM: the bytes are gone
		n.Data[sl.Dev] = tokUnknown
	case KLDax:
		// a new slice is zeroed before the claim gets it
		n.setSlotData(i, tokZero)
	}
	return seq
}

// handover marks the prepare of a pool claim that got its device after
// the old holders: after every earlier unprepare of its region (A21).
// The local memdev keeps the plain rule: one release is enough.
func (n *State) handover(i int, seq int16) {
	if n.slot(i).Kind.pooled() {
		n.setHandover(seq)
	}
}

// newSteer is the steering of an lsteer container that starts now.
func (n *State) newSteer(v VM) Steer {
	if n.Drv[v].Killed {
		return SteerAny
	}
	return SteerOn
}

// delete deletes the pod of a slot; a waiter of what it held gets it.
func (n *State) delete(i int) {
	ss := &n.Slots[i]
	sl := n.slot(i)
	prev := ss.Phase
	ss.Phase = Deleting
	ss.Prev = prev
	ss.RaceKill = false
	u := int16(i)
	seq := int16(-1)
	lseq := int16(-1)
	switch prev {
	case Running, Restarting:
		seq = n.addEvent(u, evUnprepare, false, -1)
	case Starting:
		// its Prepare may or may not come before the delete
		n.setOptional(u)
		seq = n.addEvent(u, evUnprepare, false, -1)
	case Unblocking, Stalled:
		// its retry (its promotion) may have made its region
		n.prepareEvent(i, -1)
		n.setOptional(u)
		seq = n.addEvent(u, evUnprepare, false, -1)
	}
	if sl.Kind.dual() && seq >= 0 {
		// and its local slice
		lseq = n.addEvent(n.secondUser(i), evUnprepare, false, -1)
	}
	ss.Hold = false
	released := false
	if sl.Kind.leaseKind() && (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) {
		// released when the requester pod is deleted (a held file goes
		// with its container)
		ss.Lease, ss.LeaseOpen = LeaseReleasing, false
		released = true
		lseq = n.addEvent(n.secondUser(i), evUnprepare, false, -1)
	}
	if prev.holding() {
		// a pool claim deleted before its pod was bound (waiting for
		// the binding conditions) holds its device in the scheduler
		// for a while: its contender may stall (A19)
		n.promote(sl.Res, seq, sl.Kind.pooled() && prev.unbound())
		if sl.Kind.dual() {
			n.promote(localRes(sl.VM), lseq, false)
		}
	}
	if released && sl.Kind == KLease {
		n.promote(localRes(sl.VM), lseq, false)
	}
	if released && sl.Kind == KPlse {
		n.promote(Res(cPmem0), lseq, false)
	}
}

// promote gives a resource to its waiter if it fits when the holders
// that are going away are gone; its Prepare comes after the event that
// released the resource.
func (n *State) promote(r Res, after int16, stall bool) {
	if r == resNone {
		return
	}
	w := n.waiter(r)
	if w < 0 {
		return
	}
	if _, later := n.fits(w); later {
		n.Contested |= 1 << uint(r)
		wsl := n.slot(w)
		if wsl.Kind.dual() {
			n.Contested |= 1<<uint(wsl.Res) | 1<<uint(localRes(wsl.VM))
		}
		// (also when another claim of the device deleted in this
		// window, or before the last settle, was not bound; a dnuma
		// pod promoted by a local release: of its pool device)
		pr := wsl.Res
		if wsl.Kind.pooled() && (stall && r == pr || n.unboundDeleting(pr) || n.held(pr)) && n.Ctrl[wsl.Pool].ResourceSlices != SlicesAbsent {
			n.stall(w)
			return
		}
		n.handover(w, n.start(w, after))
	}
}

// stall makes a contender Stalled (A19). Its promotion may come at any
// time: a numa claim onlines its device then (the bytes are gone, as in
// start; A21).
func (n *State) stall(i int) {
	ss := &n.Slots[i]
	ss.Phase, ss.RaceKill = Stalled, false
	ss.Refuse = n.refusedNow(i)
	n.setUnblockAt(i, 0)
	if sl := n.slot(i); sl.Kind == KXNUMA || sl.Kind == KDNUMA {
		n.Data[sl.Dev] = tokUnknown
	}
}

// unboundDeleting: a pool claim of the resource is being deleted before
// its pod was bound (A19).
func (s *State) unboundDeleting(r Res) bool {
	for i := range s.Slots {
		ss := &s.Slots[i]
		if sl := s.slot(i); sl.Res == r && sl.Kind.pooled() && ss.Phase == Deleting && ss.Prev.unbound() {
			return true
		}
	}
	return false
}

// crash kills the workload of a running pod: the container restarts.
func (n *State) crash(i int) {
	ss := &n.Slots[i]
	ss.Phase = Restarting
	ss.Restarts++
	// a new container: not loose (A37)
	ss.Loose = false
	// a holder dies with the container: a busy fs lease is released
	ss.Hold = false
	if ss.Lease == LeaseBusy {
		n.releasePLease(i)
	}
	if n.slot(i).Kind == KLSteer {
		// a new container gets a new manager
		ss.Steer = n.newSteer(n.slot(i).VM)
	}
}

// write writes "mbt-<step>" at offset 0 of the dax device of a slot. A
// read-only claim refuses it.
func (n *State) write(i int) {
	sl := n.slot(i)
	tok := Token{Known: true, Text: fmt.Sprintf("mbt-%d", n.Steps)}
	switch sl.Kind {
	case KSRo:
	case KLDax:
		n.setSlotData(i, tok)
	default:
		n.Data[sl.Dev] = tok
	}
}

// leaseGet takes a lease of a local block (cxl-request request --source
// local --wait: Ready when it returns).
func (n *State) leaseGet(i int, m LeaseMode) {
	ss := &n.Slots[i]
	ss.Lease = LeaseHeld
	ss.LeaseMode = m
	ss.CompLoose = false
	n.setLeaseID(i, "")
	n.addEvent(n.secondUser(i), evPrepare, true, -1)
	if m == ModeNUMA && n.Fence == FenceAM {
		// the companion's slice is fenced before it goes online
		d := &n.Drv[n.slot(i).VM]
		d.F |= n.fenceBit(n.cfg.bridgeNUMANode(HB0))
	}
}

// leasePut releases the lease of a slot. A persistent fs lease whose
// file is held stays (LeaseBusy) until unhold, a crash or the delete
// (A25).
func (n *State) leasePut(i int) {
	if n.slot(i).Kind == KPlse {
		if ss := &n.Slots[i]; ss.Hold && ss.LeaseMode.fs() {
			ss.Lease = LeaseBusy
			return
		}
		n.releasePLease(i)
		return
	}
	n.Slots[i].Lease = LeaseReleasing
	seq := n.addEvent(n.secondUser(i), evUnprepare, false, -1)
	n.promote(localRes(n.slot(i).VM), seq, false)
}

// driverRestarted applies a new driver incarnation on a VM: the fenced
// nodes are seeded from the claims prepared at its start (uncertain for
// claims in flux), and steering managers of running containers are gone:
// the new driver sets their memory.high to max and fences them with
// their claims (A17), running says it (SteerOff). A
// container (re)started in the window is steered or not by timing.
func (n *State) driverRestarted(v VM, running Steer) {
	var def, maybe uint8
	for i := range n.Slots {
		ss := &n.Slots[i]
		sl := n.slot(i)
		if sl.Kind == KLease && sl.VM == v && ss.Lease != LeaseNone && ss.LeaseMode == ModeNUMA {
			bit := n.fenceBit(n.cfg.bridgeNUMANode(HB0))
			if ss.Lease == LeaseHeld {
				def |= bit
			} else {
				maybe |= bit
			}
		}
		if ss.Node != v {
			continue
		}
		if sl.Kind.numa() {
			var bit uint8
			for _, node := range sl.numaNodes(n.cfg) {
				bit |= n.fenceBit(node)
			}
			switch ss.Phase {
			case Running, Restarting:
				def |= bit
			case Starting, Either:
				maybe |= bit
			case Deleting:
				if ss.Prev.holding() && ss.Prev != Attaching {
					maybe |= bit
				}
			}
		}
		if sl.Kind == KLSteer {
			switch ss.Phase {
			case Running:
				ss.Steer = running
			case Starting, Restarting, Blocked, Unblocking, Stalled:
				// its container may be created or started while the
				// driver is away: the new driver's Synchronize sees
				// it CREATED and manages it fenced, not steered
				// (A17), or it comes after
				ss.Steer = SteerAny
			}
		}
	}
	d := &n.Drv[v]
	d.F = def
	d.FMaybe = maybe &^ def
	if n.Fence == FenceNone {
		// fencing none: no fence (D/fence.go newNodeFence)
		d.F, d.FMaybe = 0, 0
	}
}

// poolStarting: a claim of the pool is being allocated (Starting).
func (s *State) poolStarting(p Pool) bool {
	for i := range s.Slots {
		if sl := s.slot(i); sl.Kind.pooled() && sl.Pool == p && s.Slots[i].Phase == Starting {
			return true
		}
	}
	return false
}

// unschedHolder: a claim of the resource waits for its pool's slices or
// is stalled (A19).
func (s *State) unschedHolder(r Res) bool {
	for i := range s.Slots {
		if ph := s.Slots[i].Phase; (ph == Unsched || ph == Stalled) && s.slot(i).Res == r {
			return true
		}
	}
	return false
}

// ctrlKill kills the controller of a pool: attachments that were being
// changed may or may not have changed, pods that were starting may or
// may not have got their Attached condition.
func (n *State) ctrlKill(p Pool) {
	c := &n.Ctrl[p]
	c.Up = false
	want, _ := n.desiredAtt(p)
	// (F5) the attach of a claim that a stalled contender may have got
	// since, or of one deleted before its pod was bound, may have been
	// in flight at the server; and an attachment that only claims being
	// allocated want may have been detached by a reconcile before them
	var flux, settled [devsPerPool][nVMs]bool
	for i := range n.Slots {
		sl, ss := n.slot(i), &n.Slots[i]
		if !sl.Kind.pooled() || sl.Pool != p || ss.Node == VMAny {
			continue
		}
		d := devIndex(sl.Dev)
		if ss.Phase == Stalled || (ss.Phase == Deleting && ss.Prev.unbound()) {
			flux[d][ss.Node] = true
		}
		if ss.Phase.holding() && ss.Phase != Starting && ss.Phase != Unsched && ss.Phase != Stalled {
			settled[d][ss.Node] = true
		}
	}
	// the companion of a persistent lease released in this window: its
	// claim's detach may not have come before the kill, also when the
	// lease was got in this window (attached, synchronously, after the
	// last settle; fuzz, pmem --refpool-len 120 seed 30671)
	for i := range n.Slots {
		if sl, ss := n.slot(i), &n.Slots[i]; p == PoolC && sl.Kind == KPlse && ss.Lease == LeaseReleasing && ss.LeaseMode.persistent() {
			flux[devIndex(cPmem0)][sl.VM] = true
		}
	}
	for d := range poolDevs(p) {
		for v := VM(0); v < nVMs; v++ {
			if c.Att[d][v] == TriYes && want[d][v] && !settled[d][v] {
				flux[d][v] = true
			}
		}
	}
	for d := range poolDevs(p) {
		for v := VM(0); v < nVMs; v++ {
			cur := c.Att[d][v]
			if cur == TriMaybe || (cur == TriYes) != want[d][v] || flux[d][v] {
				c.Att[d][v] = TriMaybe
			}
		}
	}
	for i := range n.Slots {
		sl := n.slot(i)
		if sl.Kind.pooled() && sl.Pool == p && n.Slots[i].Phase == Starting {
			n.Slots[i].RaceKill = true
			n.setOptional(int16(i))
		}
	}
}

// ctrlStart starts the controller of a pool: pods waiting for the attach
// get it, and prepare.
func (n *State) ctrlStart(p Pool) {
	n.Ctrl[p].Up = true
	if n.Ctrl[p].ResourceSlices == SlicesAbsent {
		// the new controller publishes them within seconds (A20): the
		// claims of the window are allocated (one that tried before
		// the publish is requeued by it), unless a kill comes first
		n.Ctrl[p].ResourceSlices = SlicesUnknown
	}
	for i := range n.Slots {
		sl := n.slot(i)
		ss := &n.Slots[i]
		if !sl.Kind.pooled() || sl.Pool != p {
			continue
		}
		switch {
		case ss.Phase == Unsched && (n.unboundDeleting(sl.Res) || n.held(sl.Res)):
			// the publish requeues it, but its device may still be
			// in flight in the scheduler for a claim deleted before
			// its pod was bound: an attempt within that hold fails
			// and nothing requeues it (A19; fuzz, pmem seed 4588: the
			// waiter of an Attaching claim deleted while the slices
			// were gone)
			n.Contested |= 1 << uint(sl.Res)
			n.stall(i)
		case ss.Phase == Attaching || ss.Phase == Either || ss.Phase == Unsched:
			// (Unsched: the new controller publishes the slices; a
			// claim created before they are there would race with
			// it for the device)
			if ss.Phase == Unsched {
				n.Contested |= 1 << uint(sl.Res)
			}
			// (Attaching: no binding timeout any more)
			n.setUnblockAt(i, 0)
			// (Attaching, Either: allocated, kept without the
			// slices; Unsched: not allocated)
			ss.Alloc = ss.Phase != Unsched
			ss.Phase = Starting
			ss.RaceKill = false
			ss.Refuse = n.refusedNow(i)
			n.handoverAfterRelease(i, n.prepareEvent(i, -1))
		case ss.Phase == Starting && !n.hasPrepare(int16(i)):
			// created (or promoted) while the controller was down
			n.handoverAfterRelease(i, n.prepareEvent(i, -1))
		case ss.Phase == Starting && ss.RaceKill:
			// its Prepare comes now for sure
			ss.RaceKill = false
			n.setRequired(int16(i))
		}
	}
}

// handoverAfterRelease marks the prepare (seq) that a controller start
// adds for a pod of its pool as a handover (A21) when a claim that the
// pod's claim could not share the device with was released in this
// window: the pod was allocated after that claim's Unprepare, the
// controller being down only delayed its attach (A21).
func (n *State) handoverAfterRelease(i int, seq int16) {
	if seq < 0 {
		return
	}
	key := n.Ev[seq].Key
	for _, f := range n.Ev[:seq] {
		if f.Kind != evUnprepare || f.Key != key {
			continue
		}
		if j, lease := n.userSlot(f.User); !lease && j != i && (!n.slot(i).Kind.share() || !n.slot(j).Kind.share()) {
			n.setHandover(seq)
			return
		}
	}
}

// settle is the stable state that the SUT reaches after the operations
// in flight.
func (n *State) settle() {
	n.settlePDevs()
	n.Deallocating = 0
	for i := range n.Slots {
		ss, sl := &n.Slots[i], n.slot(i)
		if ss.Phase != Deleting || !sl.Kind.pooled() || !ss.Prev.holding() {
			continue
		}
		n.Deallocating |= 1 << uint(sl.Res)
		// a claim deleted before its pod was bound: its device may stay
		// in flight in the scheduler after the settle (A19)
		if ss.Prev.unbound() {
			n.Held |= 1 << uint(sl.Res)
			n.HeldAt[sl.Dev] = 0
		}
	}
	for i := range n.Slots {
		ss := &n.Slots[i]
		sl := n.slot(i)
		switch ss.Phase {
		case Deleting:
			*ss = SlotState{Gen: ss.Gen, Node: sl.VM}
			n.setLeaseID(i, "")
			n.setUnblockAt(i, 0)
			if sl.Kind == KLDax {
				n.setSlotData(i, tokUnknown)
			}
			continue
		case Starting:
			switch {
			case sl.Kind.pooled() && !n.Ctrl[sl.Pool].Up && ss.RaceKill:
				ss.Phase = Either
			case sl.Kind.pooled() && !n.Ctrl[sl.Pool].Up:
				ss.Phase = Attaching
			case ss.Refuse:
				ss.Phase = Refused
			default:
				ss.Phase = Running
			}
		case Unblocking:
			if ss.Refuse {
				// its retry is refused: the content cannot change
				// while it holds its share (A25)
				ss.Phase = Refused
				n.setUnblockAt(i, 0)
			}
		case Restarting:
			ss.Phase = Running
		case Attaching, Either:
			if n.Ctrl[sl.Pool].Up {
				ss.Phase = Running
			}
		}
		ss.RaceKill, ss.Alloc = false, false
		if ss.Lease == LeaseReleasing {
			ss.Lease = LeaseNone
			ss.CompLoose = false
			n.setLeaseID(i, "")
		}
	}
	for p := Pool(0); p < nPools; p++ {
		if n.Ctrl[p].Up {
			// a controller that runs publishes its slices
			n.Ctrl[p].ResourceSlices = SlicesPresent
			n.deriveAtt(p)
		}
	}
	n.growFence()
	n.Ev = nil
	n.Contested = 0
	n.Switched = false
	for v := range n.Drv {
		n.Drv[v].Killed = false
		n.Drv[v].Restarted = false
		// (the settle's observation decides; a refused claim's retry
		// after it marks the next window again)
		n.Drv[v].KillMay = false
		// (the settle waits for the driver; the oracle adopts its
		// restarts within the Slack)
		n.Drv[v].Creating, n.Drv[v].KillSlack = false, false
	}
	n.Unsettled = 0
}

// settlePDevs applies a window's writer Prepares to the persistent
// devices (A25): a writer Running after a Prepare of the window made the
// namespace of a device without one; one that may have prepared and is
// gone again (deleted while starting), may have been attached (a
// controller kill) or may prepare any time (stalled, unblocking) leaves
// it maybe, adopted from pdevs.
func (n *State) settlePDevs() {
	for i := range n.Slots {
		ss, sl := &n.Slots[i], n.slot(i)
		if !sl.Kind.writer() {
			continue
		}
		pd := &n.PDev[pmemIndex(sl.Dev)]
		switch {
		case pd.NS == TriYes:
		case ss.Phase == Running && n.hasPrepare(int16(i)):
			n.madeNamespace(sl.Dev)
		case ss.Phase == Deleting && n.hasPrepare(int16(i)) && (ss.Prev == Starting || n.Ctrl[sl.Pool].Up),
			ss.Phase == Starting && ss.RaceKill,
			ss.Phase == Stalled && n.Ctrl[sl.Pool].Up,
			ss.Phase == Unblocking:
			// (a stalled writer may be promoted and prepare any time
			// its controller attaches it; an unblocking one's retry
			// comes any time)
			pd.NS, pd.LSAHash, pd.HeadHash = TriMaybe, "", ""
		}
	}
}

// deriveAtt sets the attachments of a pool whose controller is up to
// those that its claims want.
func (n *State) deriveAtt(p Pool) {
	want, _ := n.desiredAtt(p)
	for d := range poolDevs(p) {
		for v := VM(0); v < nVMs; v++ {
			if want[d][v] {
				n.Ctrl[p].Att[d][v] = TriYes
			} else {
				n.Ctrl[p].Att[d][v] = TriNo
			}
		}
	}
}

// growFence adds the nodes of the numa memory prepared on each VM to its
// fenced nodes (they only grow until the driver restarts).
func (n *State) growFence() {
	if n.Fence == FenceNone {
		return
	}
	for i := range n.Slots {
		ss := &n.Slots[i]
		sl := n.slot(i)
		if sl.Kind == KLease && ss.Lease == LeaseHeld && ss.LeaseMode == ModeNUMA {
			n.Drv[sl.VM].F |= n.fenceBit(n.cfg.bridgeNUMANode(HB0))
		}
		if !sl.Kind.numa() || ss.Node == VMAny {
			continue
		}
		var bit uint8
		for _, node := range sl.numaNodes(n.cfg) {
			bit |= n.fenceBit(node)
		}
		switch ss.Phase {
		case Running:
			n.Drv[ss.Node].F |= bit
		case Either:
			n.Drv[ss.Node].FMaybe |= bit
		}
	}
	for v := range n.Drv {
		n.Drv[v].FMaybe &^= n.Drv[v].F
	}
}
