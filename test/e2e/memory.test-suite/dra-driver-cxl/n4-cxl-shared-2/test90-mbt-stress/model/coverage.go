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

// Coverage criteria (the flag --coverage):
//   C1 every action (gofmbt action names)
//   C2 fault in context: per fault step, fault|kind|phase of every pod
//      that the fault affects (custom, incremental)
//   C3 pairs of action formats: verb+kind without pool, node and index
//   C4 abstract states: State.abstract() of every state (custom)

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Coverage is a mix of criteria with weights: a criterion of weight w
// counts each of its strings w times (w copies with different prefixes),
// 0 is off.
type Coverage struct {
	W [4]int
}

// on tells if criterion Cn (1..4) is in the mix.
func (c Coverage) on(n int) bool { return c.W[n-1] > 0 }

// parseCoverage parses a comma list of C1..C4, each with an optional
// weight xN (C1x3), or "all".
func parseCoverage(s string) (Coverage, error) {
	var c Coverage
	for _, f := range strings.Split(s, ",") {
		f = strings.ToUpper(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if f == "ALL" {
			c.W = [4]int{1, 1, 1, 1}
			continue
		}
		name, wstr, hasW := strings.Cut(f, "X")
		w := 1
		if hasW {
			n, err := strconv.Atoi(wstr)
			if err != nil || n < 1 || n > 100 {
				return c, fmt.Errorf("--coverage: invalid weight in %q", f)
			}
			w = n
		}
		switch name {
		case "C1", "C2", "C3", "C4":
			c.W[name[1]-'1'] = w
		default:
			return c, fmt.Errorf("--coverage: unknown criterion %q (C1, C2, C3, C4 with an optional weight xN, or all)", f)
		}
	}
	if c == (Coverage{}) {
		return c, fmt.Errorf("--coverage: no criterion")
	}
	return c, nil
}

func (c Coverage) String() string {
	var cs []string
	for i, w := range c.W {
		switch {
		case w == 1:
			cs = append(cs, fmt.Sprintf("C%d", i+1))
		case w > 1:
			cs = append(cs, fmt.Sprintf("C%dx%d", i+1, w))
		}
	}
	return strings.Join(cs, ",")
}

// weigh returns w copies of strings with different prefixes (the
// strings themselves for w == 1).
func weigh(w int, tag string, xs []string) []string {
	if w <= 1 || len(xs) == 0 {
		return xs
	}
	out := make([]string, 0, w*len(xs))
	for k := 0; k < w; k++ {
		p := tag + strconv.Itoa(k) + "|"
		for _, x := range xs {
			out = append(out, p+x)
		}
	}
	return out
}

// faultContext returns the C2 strings of a fault taken in a state: one
// "fault VERB KIND PHASE" for every pod that the fault affects (the pods
// on the VM of a VM fault, the pods of the pool of a pool fault), and
// "fault VERB -" when it affects none. Lease pods with a lease are of
// kind lease-numa or lease-dax.
func faultContext(s *State, a Act) []string {
	switch {
	case a.Verb == VCrash:
		return crashContext(s, a)
	case a.Verb == VFencingSwitch:
		return switchContext(s)
	case !a.Verb.fault():
		return nil
	}
	fv := faultVerb(a)
	var out []string
	for i := range s.Slots {
		ss := &s.Slots[i]
		sl := s.slot(i)
		if ss.Phase == Absent {
			continue
		}
		switch {
		case a.Verb.vmFault():
			if ss.Node != a.VM {
				continue
			}
		case a.Verb.poolFault():
			if !sl.Kind.pooled() || sl.Pool != a.Pool {
				continue
			}
		}
		out = append(out, "fault "+fv+" "+contextKind(sl, ss)+" "+ss.Phase.String())
	}
	// a decoder hole is a context of its own (A9): pods blocked behind it
	// show up as "blocked" above
	switch {
	case a.Verb.vmFault():
		for b := Bridge(0); b < nBridges; b++ {
			if s.Stacks[stackIndex(a.VM, b)].hasHole() {
				out = append(out, "fault "+faultVerb(a)+" hole hb"+string(rune('0'+b)))
			}
		}

	case a.Verb.poolFault():
		for _, v := range poolHosts[a.Pool] {
			if s.Stacks[stackIndex(v, poolBridge[a.Pool])].hasHole() {
				out = append(out, "fault "+a.Verb.String()+" hole")
			}
		}
	}
	// the content of a persistent device in a fault of its pool or of a
	// VM with a claim of it (A25: faults must not change it)
	if a.Verb.poolFault() && a.Pool == PoolC || a.Verb.vmFault() {
		for x := range s.PDev {
			d := cPmem0 + DevID(x)
			if s.cfg.devUsed(d) {
				pd := &s.PDev[x]
				out = append(out, "fault "+fv+" "+d.String()+" ns-"+triName(pd.NS)+" fs-"+triName(pd.FS))
			}
		}
	}
	// a fault that affects nothing is no context (a fault of an empty
	// cluster); C1 covers the verb once
	sort.Strings(out)
	return uniq(out)
}

// memsStrings are the strings of the cpuset.mems of the containers of a
// settled state (A40): one "mems|MODE|e=E|f=F|m=M" per running container
// of a slot or a companion, MODE am or none, E its CXL NUMA nodes (claimed or
// lent), F the fenced nodes of its VM, M the CXL NUMA nodes with memory there
// ("-" for none). From the predicted state, as the planner sees it.
func memsStrings(s *State) []string {
	var out []string
	for v := VM(0); v < nVMs; v++ {
		f, m := numaSetOf(s.fenceNUMANodes(s.Drv[v].F)), s.memNUMANodes(v)
		str := func(e numaSet) string {
			return "mems|" + s.Fence.short() + "|e=" + e.String() + "|f=" + f.String() + "|m=" + m.String()
		}
		for i := range s.Slots {
			sl, ss := s.slot(i), &s.Slots[i]
			if sl.Kind.leaseKind() && sl.VM == v && (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) && (sl.Kind == KLease || ss.LeaseMode.persistent()) {
				// its companion
				out = append(out, str(0))
			}
			if ss.Phase == Running && ss.Node == v {
				out = append(out, str(s.claimNUMANodes(i)))
			}
		}
	}
	sort.Strings(out)
	return uniq(out)
}

// switchContext is C2 of fencing-switch (A40): "fault fencing-switch>MODE
// KIND" for every pod with a container (running, starting or restarting)
// and the mode it switches to: what matters is the cpuset.mems that the
// containers keep or get. (Not by phase or VM: a switch is three driver
// restarts, 45-90 s.)
func switchContext(s *State) []string {
	to := FenceNone
	if s.Fence == FenceNone {
		to = FenceAM
	}
	fv := "fault " + VFencingSwitch.String() + ">" + to.short()
	var out []string
	for i := range s.Slots {
		ss := &s.Slots[i]
		if ss.Phase == Running || ss.Phase == Starting || ss.Phase == Restarting {
			out = append(out, fv+" "+contextKind(s.slot(i), ss))
		}
	}
	if len(out) == 0 {
		out = append(out, fv+" -")
	}
	sort.Strings(out)
	return uniq(out)
}

// faultVerb is the verb of a fault in C2: driver-kill-at with its marker.
func faultVerb(a Act) string {
	if a.Verb == VDriverKillAt {
		return a.Verb.String() + ":" + a.argName()
	}
	return a.Verb.String()
}

// contextKind is the kind of a pod in C2: lease pods with their lease
// mode (and a busy fs lease), a held file.
func contextKind(sl *Slot, ss *SlotState) string {
	kind := sl.Kind.String()
	if sl.Kind.leaseKind() && ss.Lease != LeaseNone {
		kind += "-" + ss.LeaseMode.String()
		if ss.Lease == LeaseBusy {
			kind += "-busy"
		}
	}
	if ss.Hold {
		kind += "-held"
	}
	return kind
}

// crashContext is C2 of a crash (a fault of one container): the kind of the pod and what its container has that a restart
// must give back: a held lease and its mode, steering, the hole of its
// stack.
func crashContext(s *State, a Act) []string {
	i := int(a.Slot)
	ss := &s.Slots[i]
	sl := s.slot(i)
	str := "fault crash " + sl.Kind.String()
	if sl.Kind.leaseKind() && (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) {
		str += " lease-" + ss.LeaseMode.String()
		if ss.Lease == LeaseBusy {
			str += " busy"
		}
	}
	if ss.Hold {
		str += " held"
	}
	if ss.Steer == SteerOn || ss.Steer == SteerOff {
		str += " steer-" + ss.Steer.String()
	}
	if si, _, ok := s.userRegion(int16(i)); ok && s.Stacks[si].hasHole() {
		str += " hole"
	}
	if ss.Restarts > 0 {
		str += " again"
	}
	return []string{str}
}

// leaseChances are C1 strings of a state: the lease actions it allows
// (lease-get needs a free local block and a quiet hb0
// stack, which the search reaches only with a reward on the way).
func leaseChances(s *State) []string {
	var out []string
	for i := range s.Slots {
		sl := s.slot(i)
		if !sl.Kind.leaseKind() || s.Slots[i].Phase != Running {
			continue
		}
		modes := []LeaseMode{ModeNUMA}
		if sl.Kind == KPlse {
			modes = []LeaseMode{ModePBW, ModePBR, ModePFD, ModePFC}
		}
		for _, m := range modes {
			if s.allowed(Act{Verb: VLeaseGet, Slot: int16(i), VM: VMAny, Pool: noPool, Mode: m}) {
				str := "C1.can lease-get " + sl.Name
				if sl.Kind == KPlse {
					str += " " + m.String()
				}
				out = append(out, str)
			}
		}
		if s.allowed(Act{Verb: VLeasePut, Slot: int16(i), VM: VMAny, Pool: noPool}) {
			out = append(out, "C1.can lease-put "+sl.Name)
		}
	}
	return out
}

// c1Strings are the C1 strings of an action: its name, weighed w times;
// a fault counts by its verb only, once, whatever its VM or pool: what
// matters of faults is their context (C2), and the planner must not
// spend the first steps on faults of an empty cluster.
func c1Strings(cfg *Config, a Act, w int) []string {
	if a.Verb.fault() {
		return []string{"C1.fault " + faultVerb(a)}
	}
	return weigh(w, "C1.", []string{a.name(cfg)})
}
