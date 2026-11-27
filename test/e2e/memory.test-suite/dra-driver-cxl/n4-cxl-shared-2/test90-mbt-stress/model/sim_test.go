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

// A simulated SUT for the protocol tests: it keeps
// its own model state as "the truth", decides what the model leaves
// open (the node of an "any" slot, the outcome of a controller kill,
// steering in a restart window) at random, keeps the bytes of the
// devices, and answers with the observations that the contract
// describes. Settles can first return transient observations (pods not
// Running yet, attachments in flux), and deviations can be injected.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

type sim struct {
	cfg       *Config
	truth     *State
	rng       *rand.Rand
	phys      [nDevs]string
	inspected map[string]bool
	leaseSeq  int
	// transient polls before the stable observation of a settle
	transient int
	// deviate changes a stable settle observation (fault injection)
	deviate func(step int, o *Observation)
	// deviateDone changes a done observation
	deviateDone func(step int, a Act, d *DoneObs)
	// what the run went through
	sawEnd, sawFail string
	failStep        int
	prev            *State
	uuidSeq         int
	// ref, if set, is the independent reference of the pool side
	// (refpool.go): it decides the pool facts of every observation, and
	// which outcome of the model's a settle has (simref.go)
	ref *refPool
	// pend is the prediction of the settle being observed (ref only)
	pend *State
	// lateKill: the truth keeps the armed driver-kill-at watcher of the
	// VM through the window after a settle (keepKillAt)
	lateKill [nVMs]bool
	// the driver pods' restarts of kubelet restarts while their
	// containers were being created again (driverRace): creating from
	// a kill until a step waits for the driver, extra in the window,
	// settleExtra of the window being settled
	creating           [nVMs]bool
	extra, settleExtra [nVMs]uint8
	// next: the truth after the action being done (ref.do: localFree)
	next *State
	// now and since: the oracle's clock at the settle being resolved
	// and at its start (the test's clock; with ref the reference's
	// clock); zero: not known. An unblocking pod's retry and a stalled
	// contender's promotion are a coin at every settle, but never later
	// than kubelet's back-off and the scheduler's window allow (A9,
	// A19): the oracle fails them after that.
	now, since time.Time
}

// kubeletRetryCap is the cap of kubelet's back-off of a failed Prepare
// (A9): the retry of a pod whose decoder hole cleared comes within this
// long, so it runs within MBT_UNBLOCK_TIMEOUT (330 s) of the settle that
// saw the hole clear.
const kubeletRetryCap = 300 * time.Second

// clock is the oracle's clock of the settle being resolved (zero: not
// known).
func (s *sim) clock() time.Time {
	if s.ref != nil {
		return s.refWall()
	}
	return s.now
}

// overdue: slot i of the truth n has waited for a retry or a promotion
// since the settle that first saw it so (at, recorded in its UnblockAt
// as the oracle records it in the model's state) for bound or longer by
// the oracle's clock: it comes now. Never without a clock.
func (s *sim) overdue(n *State, i int, at time.Time, bound time.Duration) bool {
	now := s.clock()
	if now.IsZero() {
		return false
	}
	if n.unblockAt(i) == 0 {
		n.setUnblockAt(i, at.Unix())
	}
	return now.Sub(time.Unix(n.unblockAt(i), 0)) >= bound
}

func newSim(cfg *Config, seed int64) *sim {
	s := &sim{cfg: cfg, truth: newState(cfg), rng: rand.New(rand.NewSource(seed)), inspected: map[string]bool{}}
	for d := range s.phys {
		if devices[d].Persistent {
			// (no draw: the random sequences of the volatile tests
			// stay as they were)
			continue
		}
		s.phys[d] = fmt.Sprintf("junk-%d", s.rng.Intn(1000))
	}
	return s
}

// do applies an action to the truth and returns the done observation
// (nil for settle).
func (s *sim) do(step int, a Act) *DoneObs {
	from := s.truth
	if a.Verb == VSettle {
		s.prev = from
		s.settleExtra, s.extra, s.creating = s.extra, [nVMs]uint8{}, [nVMs]bool{}
		if s.ref != nil {
			// resolved against the reference at every poll
			s.pend = from.apply(a)
			s.truth = s.resolveRef(s.pend)
			return nil
		}
		s.truth = s.resolve(from.apply(a))
		return nil
	}
	from = s.fireLate(from)
	s.driverRace(a)
	to := from.apply(a)
	if s.ref != nil {
		// the reference's scheduler asks the truth after the action for
		// the free blocks of a local memdev (localFree): the requeues
		// that the action causes come after what it releases (the
		// block of a lease-put, the leases of a node-reboot: fuzz, full
		// qemu seed 270849, a dnuma pod never allocated)
		s.next = to
		s.ref.do(a, refHint{freesClaim: freesClaim(from, a) || freesBlock(from, a) || rebootFreesBlock(from, a), releasesLease: releasesPLease(from, to, int(a.Slot))})
		s.next = nil
	}
	d := &DoneObs{}
	switch a.Verb {
	case VWrite:
		if to.slot(int(a.Slot)).Kind == KSRo {
			d.Status = 1
			d.Stderr = "PermissionError: [Errno 13] Permission denied"
		}
	case VRead:
		t := from.token(int(a.Slot))
		txt := t.Text
		if !t.Known {
			sl := from.slot(int(a.Slot))
			if sl.Kind == KLDax {
				txt = "" // a new slice is zeroed
			} else {
				txt = s.phys[sl.Dev]
			}
		}
		d.Text = &txt
		to = adoptDone(to, a, d)
	case VLeaseGet:
		s.leaseSeq++
		id := fmt.Sprintf("L%04d", s.leaseSeq)
		d.Lease = &id
		failed := from.killAtBind(int(a.Slot), a.Mode) && s.rng.Intn(2) == 0
		if failed {
			// the driver killed at its bind: cxl-request fails, the
			// lease is released or stays Lending until kubelet's
			// retry (A29 d, A30); the reference decides which
			d.Status, d.Lease, d.Stderr = 1, nil, "reading response: EOF"
			if s.ref != nil && s.rng.Intn(2) == 0 {
				s.ref.leaseRelease(int(a.Slot))
			}
		}
		to = adoptDone(to, a, d)
		if failed {
			// (the simulated SUT knows the lease's id)
			to = to.clone()
			to.setLeaseID(int(a.Slot), id)
		}
	case VFencingSwitch:
		mode := to.Fence.String()
		d.Fencing = &mode
	case VNodeReboot:
		// the content of a dax device whose region the boot made again:
		// kept or zeros (A43), the simulated SUT's choice
		to = to.clone()
		for dev := range to.Data {
			if t := &to.Data[dev]; t.OrZero {
				if s.rng.Intn(2) == 0 {
					t.OrZero = false
				} else {
					*t = tokZero
				}
			}
		}
		boot := simBootID(a.VM, to.Drv[a.VM].Reboots)
		d.Boot = &boot
		to = adoptDone(to, a, d)
	case VPWrite, VPRead, VLeasePut:
		s.doPmem(from, to, a, d)
		to = adoptDone(to, a, d)
	}
	if to.PDev != from.PDev {
		to = to.clone()
		s.fixPDev(to)
	}
	for dev := DevID(0); dev < nDevs; dev++ {
		switch {
		case to.Data[dev].Known:
			s.phys[dev] = to.Data[dev].Text
		case from.Data[dev].Known:
			s.phys[dev] = fmt.Sprintf("junk-%d", s.rng.Intn(1000))
		}
	}
	s.truth = to
	if s.deviateDone != nil {
		s.deviateDone(step, a, d)
	}
	return d
}

// resolve decides what a settled state leaves open, as an SUT would:
// one of the outcomes of its races, the node of any pods, the outcomes
// of controller kills, steering, the retries of unblocking pods.
func (s *sim) resolve(st *State) *State {
	if len(st.Alts) > 0 {
		if j := s.rng.Intn(len(st.Alts) + 1); j > 0 {
			st = st.Alts[j-1]
		}
	}
	n := st.clone()
	s.reorderStacks(n)
	// The scheduler's hold of a device whose claim was deleted before
	// its pod was bound ends within a PreBind poll period (5 s) of the delete
	// (A19): here it lasts through the window after the settle that ended the
	// delete's window. Never longer: the oracle ends it at a settle at
	// least heldSeconds after that one, and a contender that the truth
	// stalls after the model's hold has ended would be a difference of
	// the simulator's. The step of that settle is kept in HeldAt
	// (the oracle keeps its clock there).
	for d := DevID(0); d < nDevs; d++ {
		switch {
		case !n.held(Res(d)):
		case n.HeldAt[d] == 0:
			n.HeldAt[d] = int64(n.Steps)
		default:
			n.Held &^= 1 << uint(d)
			n.HeldAt[d] = 0
		}
	}
	for i := range n.Slots {
		ss := &n.Slots[i]
		// (the coin first: the random sequences stay those of a run
		// without a clock until something is overdue)
		if ss.Phase == Stalled && (s.rng.Intn(2) == 0 || s.overdue(n, i, s.since, n.cfg.stallWindow())) {
			sl := n.slot(i)
			n.setUnblockAt(i, 0)
			if n.Ctrl[sl.Pool].Up {
				// bound: an "any" pod on a host of its pool, and its
				// Prepare makes its region there, unless a hole is in
				// the way (A9; as simref.go and the oracle's resolve)
				if ss.Node == VMAny {
					hosts := poolHosts[sl.Pool]
					ss.Node = hosts[s.rng.Intn(len(hosts))]
				}
				ss.Phase = Running
				if prepareRegions(n, i, ss.Refuse) {
					ss.Phase = Blocked
				}
				if ss.Refuse && ss.Phase == Running {
					// its Prepare is refused (A25): no region
					ss.Phase = Refused
				}
			} else {
				ss.Phase = Attaching
			}
			continue
		}
		if ss.Phase == Unblocking && (s.rng.Intn(2) == 0 || s.overdue(n, i, s.now, kubeletRetryCap)) {
			ss.Phase = Running
			n.setUnblockAt(i, 0)
			prepareRegions(n, i, false)
		}
	}
	for i := range n.Slots {
		ss := &n.Slots[i]
		sl := n.slot(i)
		if ss.Node == VMAny && (ss.Phase == Running || ss.Phase == Either) {
			hosts := poolHosts[sl.Pool]
			ss.Node = hosts[s.rng.Intn(len(hosts))]
		}
		if ss.Phase == Either {
			c := &n.Ctrl[sl.Pool]
			d := devIndex(sl.Dev)
			if s.rng.Intn(2) == 0 {
				ss.Phase = Running
				if ss.Refuse {
					ss.Phase = Refused
				}
				c.Att[d][ss.Node] = TriYes
			} else {
				ss.Phase = Attaching
			}
		}
		if ss.Steer == SteerAny && ss.Phase == Running {
			if s.rng.Intn(2) == 0 {
				ss.Steer = SteerOn
			} else {
				ss.Steer = SteerOff
			}
		}
	}
	for p := Pool(0); p < nPools; p++ {
		c := &n.Ctrl[p]
		if c.Up {
			n.deriveAtt(p)
			continue
		}
		for d := range poolDevs(p) {
			for v := VM(0); v < nVMs; v++ {
				if c.Att[d][v] == TriMaybe {
					if s.rng.Intn(2) == 0 {
						c.Att[d][v] = TriNo
					} else {
						c.Att[d][v] = TriYes
					}
				}
			}
		}
	}
	for v := VM(0); v < nVMs; v++ {
		d := &n.Drv[v]
		if d.Pod == "" || d.PodBefore != "" {
			d.Pod = fmt.Sprintf("nri-dra-driver-cxl-%s-%d", v, d.PodGen)
			d.PodBefore = ""
		}
	}
	n.growFence()
	s.fixPDev(n)
	s.keepKillAt(n)
	s.driverRestarts(n)
	return n
}

// driverRace: a kubelet restart while the container of the VM's driver
// is being created again after a kill (a driver-kill, or a watcher that
// fired before the step) gives the driver pod one more restart, at
// random: the old kubelet created the container and stopped before it
// started it, the new one creates it again (A34). The verbs that wait
// for the driver end the race. Independent of the model's Slack.
func (s *sim) driverRace(a Act) {
	switch a.Verb {
	case VDriverKill:
		s.creating[a.VM] = true
	case VKubeletRestart:
		if s.creating[a.VM] && s.rng.Intn(2) == 0 {
			s.extra[a.VM]++
		}
		s.creating[a.VM] = false
	case VContainerdRestart, VNodeReboot:
		// (the verbs wait for the driver)
		s.creating[a.VM] = false
	case VDriverRestart:
		// (a new pod)
		s.creating[a.VM], s.extra[a.VM] = false, 0
	}
}

// driverRestarts adds the restarts of driverRace to the settled truth;
// the model's Slack is the oracle's, not the simulated kubelet's.
func (s *sim) driverRestarts(n *State) {
	for v := range n.Drv {
		n.Drv[v].Restarts += s.settleExtra[v]
		n.Drv[v].Slack = 0
	}
}

// keepKillAt ends the truth's armed driver-kill-at watchers at a settle,
// or keeps one through the next window (the model keeps it armed until
// the oracle disarms it, 70 s): the simulated watcher fires in the window
// after it was armed, at the next settle or before a step of the next
// window (fireLate), or never.
func (s *sim) keepKillAt(n *State) {
	for v := range n.Drv {
		d := &n.Drv[v]
		if d.KillAt != 0 && !s.lateKill[v] && s.rng.Intn(2) == 0 {
			s.lateKill[v] = true
			continue
		}
		s.lateKill[v] = false
		d.KillAt, d.KillAtAt = 0, 0
	}
}

// fireLate fires a watcher that the truth kept (keepKillAt) before a
// step, at random, when a Prepare that comes at any time shows its
// marker (killAnyTime): A32's refused reader retried by kubelet right
// after the settle's observation, and deleted later in the window.
func (s *sim) fireLate(st *State) *State {
	var fired [nVMs]bool
	for v := VM(0); v < nVMs; v++ {
		d := &st.Drv[v]
		if s.lateKill[v] && d.KillAt != 0 && st.killAnyTime(v, int(d.KillAt-1)) && s.rng.Intn(2) == 0 {
			fired[v] = true
		}
	}
	if f := st.fireKillAt(fired, false); f != nil {
		for v := VM(0); v < nVMs; v++ {
			if fired[v] {
				s.creating[v] = true
			}
		}
		return f
	}
	return st
}

// reorderStacks puts the regions of the stacks that a node-reboot rebuilt
// in the order in which the simulated kubelet prepared their claims again
// (at random: A43); the truth knows it.
func (s *sim) reorderStacks(n *State) {
	for si := 0; si < nStacks; si++ {
		if n.Reorder&(1<<uint(si)) == 0 {
			continue
		}
		st := &n.Stacks[si]
		s.rng.Shuffle(int(st.N), func(a, b int) { st.E[a], st.E[b] = st.E[b], st.E[a] })
	}
	n.Reorder = 0
}

// rebootFreesBlock: the action is a node-reboot that releases a local
// lease (its companion's claim is deallocated: a requeueing event).
func rebootFreesBlock(st *State, a Act) bool {
	if a.Verb != VNodeReboot {
		return false
	}
	for i := range st.Slots {
		if sl := st.slot(i); sl.Kind == KLease && sl.VM == a.VM && st.Slots[i].Lease != LeaseNone {
			return true
		}
	}
	return false
}

// The names of the simulated devices: the regions of a boot of a VM
// (pool device d, or -1 for the local memdev), their dax devices and
// major:minor, the boot ids. A boot gives every region a new number, as
// the kernel would (A43).
func simBootID(v VM, reboots uint8) string { return fmt.Sprintf("boot-%s-%d", v, reboots) }

func simRegionNo(st *State, v VM, d int) int {
	if v < 0 || v >= nVMs {
		v = 0
	}
	return 10 + d + 20*int(st.Drv[v].Reboots)
}

func simDaxName(r, k int) string { return fmt.Sprintf("dax%d.%d", r, k) }
func simDaxDev(r, k int) string  { return fmt.Sprintf("252:%d", r*256+k) }
func simPmemDev(st *State, v VM, d DevID) string {
	return fmt.Sprintf("259:%d", pmemIndex(d)+2*int(st.Drv[v].Reboots))
}

// simDaxOf is the dax device of the claim of slot i on its node: the
// region's of a pool device, the slot's slice of the local memdev.
func simDaxOf(st *State, i int) (name, dev string) {
	sl, ss := st.slot(i), &st.Slots[i]
	if sl.Kind == KLDax {
		r := simRegionNo(st, ss.Node, -1)
		return simDaxName(r, 1+i), simDaxDev(r, 1+i)
	}
	r := simRegionNo(st, ss.Node, int(sl.Dev))
	return simDaxName(r, 0), simDaxDev(r, 0)
}

// simDevNodes are the /dev/dax* and /dev/pmem* of the container of slot
// i (A44).
func simDevNodes(st *State, i int) []ObsDevNode {
	sl, ss := st.slot(i), &st.Slots[i]
	nodes := []ObsDevNode{}
	switch {
	case sl.Kind.dax():
		name, dev := simDaxOf(st, i)
		nodes = append(nodes, simNode("/dev/"+name, "c", dev))
	case sl.Kind.blockKind():
		nodes = append(nodes, simNode(simBlockdev(sl.Dev), "b", simPmemDev(st, ss.Node, sl.Dev)))
	}
	return nodes
}

// simNode is a device node of a container, simCXLDev a CXL device of a VM
// (A44, the executor's forms).
func simNode(path, typ, dev string) ObsDevNode {
	var major, minor int
	if _, err := fmt.Sscanf(dev, "%d:%d", &major, &minor); err != nil {
		panic(fmt.Sprintf("simNode %s: device %q: %v", path, dev, err))
	}
	return ObsDevNode{Path: path, Type: typ, Major: &major, Minor: &minor, Dev: &dev}
}

func simCXLDev(name, kind, dev string, serial uint64) ObsCXLDev {
	n := simNode("/dev/"+name, "", dev)
	return ObsCXLDev{Name: name, Path: n.Path, Kind: kind, Major: n.Major, Minor: n.Minor, Dev: n.Dev, Serial: formatSerial(serial), Serials: []string{formatSerial(serial)}}
}

// prepareRegions makes the regions of the claims of slot i that are not
// there yet (the Prepare of a promoted or unblocked pod; both claims of a
// dnuma pod): blocked if one needs a new region through a hole (A9); a
// refused Prepare makes none (A25).
func prepareRegions(n *State, i int, refuse bool) (blocked bool) {
	for _, u := range n.claimUsers(i) {
		si, key, ok := n.userRegion(u)
		if !ok || n.Stacks[si].contains(key) {
			continue
		}
		switch {
		case n.Stacks[si].hasHole() && n.slot(i).Kind != KPBad:
			blocked = true
		case refuse:
		default:
			n.Stacks[si].push(key)
		}
	}
	return blocked
}

// localFree: the local memdev of the VM of slot i has a block for the
// local claim of its dnuma pod in the truth: the claims and leases that
// hold one and are not going away, but the slot's own (refPool.LocalFree).
func (s *sim) localFree(i int) bool {
	st := s.truth
	if s.next != nil {
		st = s.next
	}
	v := st.slot(i).VM
	held, _ := st.localUse(v)
	if st.Slots[i].Phase.holding() {
		held--
	}
	return held+1 <= s.cfg.LocalBlocks[v]
}

// simDualClaims are the claims of a dnuma pod (A39): c0 its local slice,
// allocated with its pool claim c1.
func simDualClaims(sl *Slot, gen uint16, pod string, pool ObsClaim) []ObsClaim {
	local := ObsClaim{Name: pod + "-c0", UID: localClaimUID(sl, gen), Allocated: pool.Allocated}
	if local.Allocated {
		local.Devices = []ObsClaimDevice{{Driver: localDriver, Pool: sl.VM.String(), Device: "memdev-c100e2e1"}}
	}
	return []ObsClaim{local, pool}
}

// names of the simulated objects
func podName(sl *Slot, gen uint16) string {
	return fmt.Sprintf("mbt-%s-%s-%d-g%d", sl.KindName(), sl.VM, sl.Index, gen)
}
func podUID(sl *Slot, gen uint16) string   { return fmt.Sprintf("pod-%s-%d", sl.Label(), gen) }
func claimUID(sl *Slot, gen uint16) string { return fmt.Sprintf("c-%s-%d", sl.Label(), gen) }

// localClaimUID is the uid of the local claim of a dnuma pod.
func localClaimUID(sl *Slot, gen uint16) string { return fmt.Sprintf("c-%s-%d-local", sl.Label(), gen) }
func companionName(id string) string            { return "cxl-lease-" + strings.ToLower(id) }
func companionUID(id string) string             { return "comp-" + strings.ToLower(id) }

// observe returns the stable observation of the truth.
func (s *sim) observe(step, poll int) *Observation {
	st := s.truth
	cfg := s.cfg
	o := &Observation{
		Step: step, Poll: poll,
		Others:      []string{},
		Attachments: map[string][]ObsAttachment{},
		Drivers:     map[string]*ObsDriver{},
		Controllers: map[string]string{},
		VMs:         map[string]*ObsVM{},
		Inspect:     map[string]*ObsInspect{},
	}
	t := true
	f := false
	for i := range st.Slots {
		ss := &st.Slots[i]
		sl := st.slot(i)
		if ss.Phase == Absent {
			continue
		}
		p := ObsPod{Slot: sl.Name, Name: podName(sl, ss.Gen), UID: podUID(sl, ss.Gen), Gen: int(ss.Gen), Restarts: int(ss.Restarts)}
		switch ss.Phase {
		case Running:
			node := ss.Node.String()
			p.Node, p.Phase, p.Ready = &node, "Running", true
		case Pending, Unsched, Stalled:
			p.Phase, p.Unschedulable = "Pending", true
		case Attaching:
			p.Phase = "Pending"
		case Refused:
			node := ss.Node.String()
			w := "ContainerCreating"
			p.Node, p.Phase, p.Waiting = &node, "Pending", &w
			p.Event = &ObsEvent{Reason: "FailedPrepareDynamicResources", Message: "Failed to prepare dynamic resources: prepare: " + refusedMessage(st, i)}
			p.Events = []ObsEvent{*p.Event}
		case Blocked, Unblocking:
			node := ss.Node.String()
			w := "ContainerCreating"
			p.Node, p.Phase, p.Waiting = &node, "Pending", &w
			si, _, _ := st.userRegion(int16(i))
			ev := ObsEvent{Reason: "FailedPrepareDynamicResources", Message: "Failed to prepare dynamic resources: prepare: cxl create-region: ... (" +
				blockedText + ": port2 has 3 committed decoders, decoder2.1 without a region below region3 of " + stackName(si) + ")"}
			// the newest event may be another one (a retry while the
			// driver was away): the text is in "events" (A12)
			p.Event = &ObsEvent{Reason: "FailedPrepareDynamicResources", Message: "rpc error: code = Unavailable"}
			p.Events = []ObsEvent{*p.Event, ev}
		}
		if sl.Kind.claimed() {
			held := ss.Phase == Running || ss.Phase == Attaching || ss.Phase == Blocked || ss.Phase == Unblocking || ss.Phase == Refused
			c := &ObsClaim{Name: p.Name + "-c0", UID: claimUID(sl, ss.Gen), Allocated: held}
			if sl.Kind.pooled() && c.Allocated {
				c.Devices = []ObsClaimDevice{{Driver: "cxl-pool.nri.io", Pool: "pool-" + sl.Pool.String(), Device: devices[sl.Dev].Name}}
				if ss.Phase == Attaching {
					c.Attached = &f
				} else {
					c.Attached = &t
				}
				// (the observer's "attachFailed" of an allocated pool
				// claim, A44 (4): the truth's attaches do not fail)
				c.AttachFailed = &f
			}
			p.Claim = c
			if sl.Kind.dual() {
				c.Name = p.Name + "-c1"
				p.Claims = simDualClaims(sl, ss.Gen, p.Name, *c)
				p.Claim = &p.Claims[0]
			}
		}
		o.Pods = append(o.Pods, p)
		if sl.Kind.leaseKind() && (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) {
			id := st.LeaseIDs[i]
			req := sl.Name
			o.Companions = append(o.Companions, ObsCompanion{Name: companionName(id), Node: sl.VM.String(), Phase: "Running", Lease: id, Requester: &req,
				Deleting: ss.Lease == LeaseBusy})
		}
		if ss.Phase == Running {
			key := podUID(sl, ss.Gen) + "/" + strconv.Itoa(int(ss.Restarts))
			if !s.inspected[key] {
				s.inspected[key] = true
				in := &ObsInspect{UID: p.UID, Restarts: int(ss.Restarts), Env: s.env(sl, ss)}
				if sl.Kind.leaseKind() {
					var ls []ObsLease
					switch {
					case ss.Lease == LeaseHeld && ss.LeaseMode.persistent():
						ls = append(ls, ObsLease{Lease: st.LeaseIDs[i], State: "Ready", Mode: ss.LeaseMode.String(), Source: "pool"})
					case ss.Lease == LeaseBusy:
						ls = append(ls, ObsLease{Lease: st.LeaseIDs[i], State: "Releasing", Mode: ss.LeaseMode.String(), Source: "pool"})
					case ss.Lease == LeaseHeld:
						ls = append(ls, ObsLease{Lease: st.LeaseIDs[i], State: "Ready", Mode: ss.LeaseMode.String(), Source: "local"})
					}
					in.Leases, _ = json.Marshal(ls)
				}
				if sl.Kind.persistent() || sl.Kind == KPlse {
					s.inspectPmem(st, i, in)
				}
				o.Inspect[sl.Name] = in
			}
		}
	}
	for p := Pool(0); p < nPools; p++ {
		c := &st.Ctrl[p]
		list := []ObsAttachment{}
		_, excl := st.desiredAtt(p)
		for d, dev := range poolDevs(p) {
			for v := VM(0); v < nVMs; v++ {
				if c.Att[d][v] == TriYes {
					list = append(list, ObsAttachment{Device: devices[dev].Name, Host: v.String(), State: "attached",
						SingleHost: c.Up && excl[d][v], Owner: "k8s:resourceclaim/x"})
				}
			}
		}
		// the setup's local memdevs (pool-a: vm2, vm1; pool-b: vm0)
		switch p {
		case PoolA:
			list = append(list, ObsAttachment{Device: "vm2.memdev1", Host: "vm2", State: "attached", Owner: "e2e:dra-local-device"},
				ObsAttachment{Device: "vm1.memdev1", Host: "vm1", State: "attached", Owner: "e2e:dra-local-device"})
		case PoolB:
			list = append(list, ObsAttachment{Device: "vm0.memdev1", Host: "vm0", State: "attached", Owner: "e2e:dra-local-device"})
		}
		o.Attachments[p.String()] = list
		if c.Up {
			o.Controllers[p.String()] = "active"
		} else {
			o.Controllers[p.String()] = "failed"
		}
	}
	o.PoolSlices = map[string][]ObsPoolSlice{}
	for _, p := range cfg.poolsUsed() {
		list := []ObsPoolSlice{}
		if st.Ctrl[p].ResourceSlices == SlicesPresent {
			var nodes []string
			for _, v := range poolHosts[p] {
				nodes = append(nodes, v.String())
			}
			list = append(list, ObsPoolSlice{Name: "pool-" + p.String() + "-slice", Devices: json.RawMessage(`["pooled0","shared0"]`), Nodes: nodes})
		}
		o.PoolSlices[p.String()] = list
	}
	o.Slices = map[string][]ObsSliceDev{}
	for v := VM(0); v < nVMs; v++ {
		d := &st.Drv[v]
		o.Drivers[v.String()] = &ObsDriver{Pod: d.Pod, Ready: true, Restarts: int(d.Restarts)}
		o.VMs[v.String()] = s.observeVM(v)
		o.Slices[v.String()] = []ObsSliceDev{{Name: "memdev-c100e2e1", Serial: formatSerial(localSerial)}, {Name: "dram"}}
	}
	o.PDevs = s.pdevsJSON(st)
	if s.ref != nil {
		s.refOverride(o)
	}
	return o
}

func (s *sim) env(sl *Slot, ss *SlotState) map[string]string {
	env := map[string]string{"PATH": "/usr/bin", "CXL_CLAIM_" + strings.ReplaceAll(claimUID(sl, ss.Gen), "-", "_") + "_0": "1"}
	if !sl.Kind.claimed() {
		delete(env, "CXL_CLAIM_"+strings.ReplaceAll(claimUID(sl, ss.Gen), "-", "_")+"_0")
	}
	if sl.Kind.dual() {
		env["CXL_CLAIM_"+strings.ReplaceAll(localClaimUID(sl, ss.Gen), "-", "_")+"_0"] = "1"
	}
	for k, v := range envWant(s.cfg, sl) {
		switch {
		case v == "/dev/pmem*":
			v = simBlockdev(sl.Dev)
		case strings.HasSuffix(v, "*"):
			name, _ := simDaxOf(s.truth, s.cfg.slotIndex(sl.Name))
			v = "/dev/" + name
		}
		env[k] = v
	}
	return env
}

func (s *sim) observeVM(v VM) *ObsVM {
	st := s.truth
	if s.ref != nil {
		// the pool memdevs are those that the reference attached
		st = st.clone()
		s.refAtts(st)
	}
	cfg := s.cfg
	ov := &ObsVM{
		BootID:  simBootID(v, st.Drv[v].Reboots),
		Memdevs: []ObsMemdev{{Name: "mem1", Serial: formatSerial(localSerial), Size: uint64(cfg.LocalBlocks[v]) * blockSize}},
		Regions: []ObsRegion{},
		Nodes:   map[string]ObsNUMANode{"0": {MemTotal: 4 << 30}, "1": {MemTotal: 4 << 30}},
		CDI:     map[string][]string{},
	}
	for p := Pool(0); p < nPools; p++ {
		for d, dv := range poolDevs(p) {
			if st.Ctrl[p].Att[d][v] == TriYes {
				dev := devices[dv]
				ov.Memdevs = append(ov.Memdevs, ObsMemdev{Name: fmt.Sprintf("mem%d", 2+int(dv)), Serial: formatSerial(dev.Serial), Size: dev.Size})
			}
		}
	}
	mem := map[int]uint64{}
	var poolMode [nDevs]int
	var prepared, pool, claimsCDI, poolCDI, leasesCDI []string
	localClaims := map[string]int{}
	kmem, ddax := "kmem", "device_dax"
	var localDax []ObsDax
	cxlDevs := []ObsCXLDev{}
	localNo := simRegionNo(st, v, -1)
	// (the slice of slot k-1, or of the companion of the lease of slot
	// k-1-len(Slots))
	slice := func(uid string, numa bool, k int) {
		localClaims[uid] = 1
		if numa {
			localDax = append(localDax, ObsDax{Name: simDaxName(localNo, k), Size: blockSize, Driver: &kmem, Online: blockSize})
			mem[cfg.bridgeNUMANode(HB0)] += blockSize
		} else {
			localDax = append(localDax, ObsDax{Name: simDaxName(localNo, k), Size: blockSize, Driver: &ddax})
		}
		cxlDevs = append(cxlDevs, simCXLDev(simDaxName(localNo, k), "dax", simDaxDev(localNo, k), localSerial))
	}
	containers := []ObsContainer{}
	// the cpuset.mems.effective of the containers as the driver makes
	// it (D/lend.go wantMems, A38): the CXL NUMA nodes with memory first
	var memNUMANodes numaSet
	for i := range st.Slots {
		ss, sl := &st.Slots[i], st.slot(i)
		if sl.Kind == KLease && sl.VM == v && ss.Lease == LeaseHeld && ss.LeaseMode == ModeNUMA {
			memNUMANodes |= numaBit(cfg.bridgeNUMANode(HB0))
		}
		if ss.Phase != Running || ss.Node != v {
			continue
		}
		switch sl.Kind {
		case KXNUMA:
			memNUMANodes |= numaBit(cfg.bridgeNUMANode(poolBridge[sl.Pool]))
		case KLNUMA, KLSteer:
			memNUMANodes |= numaBit(cfg.bridgeNUMANode(HB0))
		case KDNUMA:
			memNUMANodes |= numaBit(cfg.bridgeNUMANode(HB0)) | numaBit(cfg.bridgeNUMANode(HB1))
		}
	}
	fenced := numaSetOf(st.fenceNUMANodes(st.Drv[v].F | st.Drv[v].FMaybe))
	if st.Fence == FenceAM {
		// a node is fenced before its memory goes online (D/fence.go)
		fenced |= memNUMANodes
	}
	dram := numaBit(0) | numaBit(1)
	effective := func(claimed numaSet, loose, steered bool) string {
		var n numaSet
		switch {
		case steered:
			// the manager's steps and the nearest DRAM node
			n = numaBit(0) | claimed
		case loose:
			n = dram | claimed
		case st.Fence == FenceNone:
			n = dram | memNUMANodes
		default:
			n = dram | claimed | (memNUMANodes &^ fenced)
		}
		return n.String()
	}
	for i := range st.Slots {
		ss := &st.Slots[i]
		sl := st.slot(i)
		if sl.Kind == KLease && sl.VM == v && ss.Lease == LeaseHeld {
			id := st.LeaseIDs[i]
			prepared = append(prepared, companionUID(id))
			slice(companionUID(id), ss.LeaseMode == ModeNUMA, 1+len(st.Slots)+i)
			ms := effective(0, ss.CompLoose, false)
			containers = append(containers, ObsContainer{Ns: "mbt", Pod: companionName(id), Name: "pause", Mems: ms, MemsEffective: ms, MemoryHigh: "max"})
		}
		if sl.Kind == KPlse && sl.VM == v && (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) && ss.LeaseMode.persistent() {
			// the companion's pool claim: prepared, no CDI device
			id := st.LeaseIDs[i]
			pool = append(pool, "pool-"+companionUID(id))
			ms := effective(0, ss.CompLoose, false)
			containers = append(containers, ObsContainer{Ns: "mbt", Pod: companionName(id), Name: "pause", Mems: ms, MemsEffective: ms, MemoryHigh: "max"})
		}
		if ss.Phase != Running || ss.Node != v {
			continue
		}
		uid := claimUID(sl, ss.Gen)
		mems := map[int]bool{0: true, 1: true}
		switch {
		case sl.Kind.persistent():
			pool = append(pool, uid)
			poolCDI = append(poolCDI, "claim-"+uid+"-0")
		case sl.Kind.dual():
			// c-pooled0 in numa and a numa slice (A39)
			pool = append(pool, uid)
			poolCDI = append(poolCDI, "claim-"+uid+"-0")
			poolMode[sl.Dev] = 1
			mem[sl.numaNode(cfg)] += devices[sl.Dev].Size
			mems[sl.numaNode(cfg)] = true
			luid := localClaimUID(sl, ss.Gen)
			prepared = append(prepared, luid)
			claimsCDI = append(claimsCDI, "claim-"+luid+"-0")
			slice(luid, true, 1+i)
			mems[cfg.bridgeNUMANode(HB0)] = true
		case sl.Kind.pooled():
			pool = append(pool, uid)
			poolCDI = append(poolCDI, "claim-"+uid+"-0")
			if sl.Kind == KXNUMA {
				poolMode[sl.Dev] = 1
				mem[sl.numaNode(cfg)] += devices[sl.Dev].Size
				mems[sl.numaNode(cfg)] = true
			} else if poolMode[sl.Dev] == 0 {
				poolMode[sl.Dev] = 2
			}
		case sl.Kind.local():
			prepared = append(prepared, uid)
			claimsCDI = append(claimsCDI, "claim-"+uid+"-0")
			slice(uid, sl.Kind != KLDax, 1+i)
			if sl.Kind.numa() {
				mems[sl.numaNode(cfg)] = true
			}
		case sl.Kind.leaseKind():
			prepared = append(prepared, uid)
			claimsCDI = append(claimsCDI, "claim-"+uid+"-0")
			leasesCDI = append(leasesCDI, "lease-"+uid)
			if ss.Lease == LeaseHeld && ss.LeaseMode == ModeNUMA {
				mems[cfg.bridgeNUMANode(HB0)] = true
			}
		}
		high := "max"
		if sl.Kind == KLSteer && ss.Steer == SteerOn {
			high = "134479872"
		}
		delete(mems, 0)
		delete(mems, 1)
		steered := sl.Kind == KLSteer && (ss.Steer == SteerOn || (st.Fence == FenceNone && ss.Steer == SteerOff))
		ms := effective(nodesOfMap(mems), ss.Loose, steered)
		containers = append(containers, ObsContainer{Ns: "mbt", Pod: podName(sl, ss.Gen), Name: "main", ID: fmt.Sprintf("ctr-%s-%d-%d", sl.Label(), ss.Gen, ss.Restarts), Mems: ms, MemsEffective: ms, MemoryHigh: high,
			DevNodes: rawJSON(simDevNodes(st, i)), DevicesFrom: "proc"})
	}
	for d := DevID(0); d < nDevs; d++ {
		if poolMode[d] == 0 {
			continue
		}
		dev := devices[d]
		node := cfg.bridgeNUMANode(poolBridge[dev.Pool])
		no := simRegionNo(st, v, int(d))
		r := ObsRegion{Name: fmt.Sprintf("region%d", no), Serials: []string{formatSerial(dev.Serial)}, Mode: "ram", Size: dev.Size, Node: &node}
		if poolMode[d] == 1 {
			r.Dax = []ObsDax{{Name: simDaxName(no, 0), Size: dev.Size, Driver: &kmem, Online: dev.Size}}
		} else {
			r.Dax = []ObsDax{{Name: simDaxName(no, 0), Size: dev.Size, Driver: &ddax}}
		}
		cxlDevs = append(cxlDevs, simCXLDev(simDaxName(no, 0), "dax", simDaxDev(no, 0), dev.Serial))
		ov.Regions = append(ov.Regions, r)
	}
	memdevs := []ObsLocalMemdev{}
	if len(localDax) > 0 {
		node := cfg.bridgeNUMANode(HB0)
		dax := append([]ObsDax{{Name: simDaxName(localNo, 0), Size: 0, Driver: &ddax}}, localDax...)
		region := fmt.Sprintf("region%d", localNo)
		ov.Regions = append(ov.Regions, ObsRegion{Name: region, Serials: []string{formatSerial(localSerial)}, Mode: "ram",
			Size: uint64(cfg.LocalBlocks[v]) * blockSize, Node: &node, Dax: dax})
		memdevs = append(memdevs, ObsLocalMemdev{Serial: formatSerial(localSerial), Claims: localClaims})
	}
	for _, n := range cfg.cxlNUMANodes() {
		ov.Nodes[strconv.Itoa(n)] = ObsNUMANode{MemTotal: mem[n]}
	}
	claims, leases := s.leaseStore(v)
	ov.State = &ObsDriverState{BootID: ov.BootID, Prepared: map[string][]string{localDriver: prepared, poolDriver: pool},
		LocalMemdevs: memdevs, UnusedDax: []string{}, LeaseClaims: claims, Leases: leases}
	ctr, yes := fmt.Sprintf("drv-%s-%d", v, st.Drv[v].Inc), true
	mode := st.Fence.String()
	ov.DriverLog = &ObsDriverLog{Container: &ctr, LendsRestored: &yes, Sockets: &yes, Fencing: &mode}
	ov.Missing = json.RawMessage("[]")
	for b := Bridge(0); b < nBridges; b++ {
		stk := &st.Stacks[stackIndex(v, b)]
		hb := "hb" + string(rune('0'+b))
		port := ObsPort{Port: fmt.Sprintf("port%d", 2+int(b)), Kind: "hostbridge", HB: &hb, Committed: int(stk.N), Decoders: []ObsDecoder{}}
		for x, k := range stk.keys() {
			d := ObsDecoder{Name: fmt.Sprintf("decoder%d.%d", 2+int(b), x)}
			switch {
			case k == rkLocal:
				d.Region = fmt.Sprintf("region%d", localNo)
			case k != rkHole:
				d.Region = fmt.Sprintf("region%d", simRegionNo(st, v, int(k-1)))
			}
			port.Decoders = append(port.Decoders, d)
		}
		ov.Ports = append(ov.Ports, port)
	}
	ov.CDI[cdiDir+"/"+claimsCDISpec] = append(claimsCDI, leasesCDI...)
	ov.CDI[cdiDir+"/"+poolCDISpec] = poolCDI
	ov.Containers = containers
	s.observePmemVM(st, v, ov)
	for d := cPmem0; d < nDevs; d++ {
		if pd := &st.PDev[pmemIndex(d)]; pd.NS == TriYes && s.pmemVM(st, v).devs[d] {
			cxlDevs = append(cxlDevs, simCXLDev(strings.TrimPrefix(simBlockdev(d), "/dev/"), "pmem", simPmemDev(st, v, d), devices[d].Serial))
		}
	}
	ov.CXLDevs = rawJSON(cxlDevs)
	return ov
}

// leaseStore returns the lease claims and the leases of state.json of a
// VM.
func (s *sim) leaseStore(v VM) ([]string, []ObsStoredLease) {
	st := s.truth
	claims, leases := []string{}, []ObsStoredLease{}
	for i := range st.Slots {
		ss := &st.Slots[i]
		sl := st.slot(i)
		if !sl.Kind.leaseKind() || sl.VM != v || ss.Phase != Running {
			continue
		}
		claims = append(claims, claimUID(sl, ss.Gen))
		if ss.Lease == LeaseHeld || ss.Lease == LeaseBusy {
			state := "Ready"
			if ss.Lease == LeaseBusy {
				state = "Releasing"
			}
			leases = append(leases, ObsStoredLease{ID: st.LeaseIDs[i], Mode: ss.LeaseMode.String(), State: state, Pod: podName(sl, ss.Gen)})
		}
	}
	return claims, leases
}

// transientObservation is a legitimate observation of the SUT on its
// way from the state before the settle to the truth: pods that were
// starting are Pending, crashed ones show the old restart count, deleted
// ones are still there being deleted, new attachments are attaching.
func (s *sim) transientObservation(step, poll int) *Observation {
	o := s.observe(step, poll)
	// keep the inspections for the stable observation
	for k := range o.Inspect {
		delete(s.inspected, o.Inspect[k].UID+"/"+strconv.Itoa(o.Inspect[k].Restarts))
	}
	o.Inspect = map[string]*ObsInspect{}
	prev := s.prev
	if prev == nil {
		return o
	}
	for i := range prev.Slots {
		ps := &prev.Slots[i]
		sl := prev.slot(i)
		switch ps.Phase {
		case Starting:
			for j := range o.Pods {
				if o.Pods[j].Slot == sl.Name && o.Pods[j].Phase == "Running" {
					w := "ContainerCreating"
					o.Pods[j].Phase, o.Pods[j].Ready, o.Pods[j].Waiting = "Pending", false, &w
				}
			}
		case Restarting:
			for j := range o.Pods {
				if o.Pods[j].Slot == sl.Name && o.Pods[j].Restarts > 0 {
					o.Pods[j].Restarts--
				}
			}
		case Deleting:
			p := ObsPod{Slot: sl.Name, Name: podName(sl, ps.Gen), UID: podUID(sl, ps.Gen), Gen: int(ps.Gen), Phase: "Running", Deleting: true, Restarts: int(ps.Restarts)}
			o.Pods = append(o.Pods, p)
		}
	}
	for p := Pool(0); p < nPools; p++ {
		for j := range o.Attachments[p.String()] {
			a := &o.Attachments[p.String()][j]
			if d, v, err := parseDevAt(a.Device + "@" + a.Host); err == nil {
				if prev.Ctrl[p].Up && prev.Ctrl[p].Att[devIndex(d)][v] != TriYes {
					a.State = "attaching"
				}
			}
		}
	}
	return o
}

// serve answers the protocol on in/out until "end" or "fail" (or EOF).
func (s *sim) serve(in io.Reader, out io.Writer) error {
	rd := bufio.NewReaderSize(in, 1<<20)
	readLine := func() (string, error) {
		line, err := rd.ReadString('\n')
		return strings.TrimRight(line, "\n"), err
	}
	for {
		line, err := readLine()
		if err != nil {
			return err
		}
		f := strings.SplitN(line, " ", 3)
		switch f[0] {
		case "end":
			s.sawEnd = line
			return nil
		case "fail":
			s.sawFail = line
			s.failStep, _ = strconv.Atoi(f[1])
			return nil
		case "step":
		default:
			return fmt.Errorf("sim: unexpected line %q", line)
		}
		n, _ := strconv.Atoi(f[1])
		a, err := parseAct(s.cfg, f[2])
		if err != nil {
			if _, err := fmt.Fprintf(out, "done %d 2 {}\n", n); err != nil {
				return err
			}
		} else if a.Verb == VSettle {
			s.do(n, a)
			poll := 0
			for {
				poll++
				var o *Observation
				if s.ref != nil {
					s.refPoll(poll)
					o = s.observe(n, poll)
				} else if poll <= s.transient {
					o = s.transientObservation(n, poll)
				} else {
					o = s.observe(n, poll)
					if s.deviate != nil {
						s.deviate(n, o)
					}
				}
				js, _ := json.Marshal(o)
				if _, err := fmt.Fprintf(out, "observe %d %s\n", n, js); err != nil {
					return err
				}
				ans, err := readLine()
				if err != nil {
					return err
				}
				af := strings.SplitN(ans, " ", 3)
				if af[0] == "wait" {
					continue
				}
				if af[0] == "fail" {
					s.sawFail = ans
					s.failStep, _ = strconv.Atoi(af[1])
					return nil
				}
				if af[0] != "ok" {
					return fmt.Errorf("sim: unexpected answer %q", ans)
				}
				break
			}
			continue
		} else {
			d := s.do(n, a)
			js, _ := json.Marshal(struct {
				Text    *string `json:"text"`
				Stderr  string  `json:"stderr"`
				Lease   *string `json:"lease,omitempty"`
				Fencing *string `json:"fencing,omitempty"`
				Boot    *string `json:"boot,omitempty"`
			}{d.Text, d.Stderr, d.Lease, d.Fencing, d.Boot})
			if _, err := fmt.Fprintf(out, "done %d %d %s\n", n, d.Status, js); err != nil {
				return err
			}
		}
		ans, err := readLine()
		if err != nil {
			return err
		}
		af := strings.SplitN(ans, " ", 3)
		switch af[0] {
		case "ok":
		case "fail":
			s.sawFail = ans
			s.failStep, _ = strconv.Atoi(af[1])
			return nil
		default:
			return fmt.Errorf("sim: unexpected answer %q", ans)
		}
	}
}
