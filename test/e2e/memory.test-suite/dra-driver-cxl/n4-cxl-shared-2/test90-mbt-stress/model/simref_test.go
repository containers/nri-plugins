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

// The simulated SUT with the independent reference of the pool side
// (refpool.go): the pool facts of every observation (pool pods' claims,
// allocation, Attached, binding and node; attachments; pool slices;
// controllers) are the reference's, never the model's. The node side
// (regions, stacks, state files, fencing, env) still comes from the
// model's truth, which is the outcome of the model's settle closest to
// the reference, resolved by it. Where no outcome of the model fits the
// reference, the observation shows the reference's facts anyway and the
// oracle has to say so: that is a disagreement.

import (
	"fmt"
	"time"
)

// refEpoch is the wall time of the reference's clock 0.
var refEpoch = time.Unix(1800000000, 0)

// withRef gives the simulated SUT the reference of the pool side.
func (s *sim) withRef(seed int64) *sim {
	s.ref = newRefPool(s.cfg, seed)
	s.ref.LocalFree = s.localFree
	return s
}

// refWall is the reference's virtual clock as wall time.
func (s *sim) refWall() time.Time { return refEpoch.Add(time.Duration(s.ref.Now()) * time.Millisecond) }

// freesClaim: the action deletes a pod whose claim that is not a pool
// claim is allocated (its deallocation requeues pending pods).
func freesClaim(st *State, a Act) bool {
	if a.Verb != VDelete {
		return false
	}
	sl := st.slot(int(a.Slot))
	ss := &st.Slots[a.Slot]
	return sl.Kind.claimed() && !sl.Kind.pooled() && ss.Phase != Pending && ss.Phase != Absent
}

// freesBlock: the action releases a local lease (lease-put; its
// companion's claim is deallocated, a requeueing event: a dnuma pod may
// wait for the block, A39).
func freesBlock(st *State, a Act) bool {
	return a.Verb == VLeasePut && st.slot(int(a.Slot)).Kind == KLease && st.Slots[a.Slot].Lease == LeaseHeld
}

// releasesPLease: the action released the persistent lease of a plse
// slot (its companion goes).
func releasesPLease(from, to *State, i int) bool {
	if i < 0 || from.slot(i).Kind != KPlse {
		return false
	}
	f, t := from.Slots[i].Lease, to.Slots[i].Lease
	return (f == LeaseHeld || f == LeaseBusy) && t == LeaseReleasing && from.Slots[i].LeaseMode.persistent()
}

// refPoll advances the reference to the poll of a settle (bash observes
// soon after the step, then every MBT_POLL) and resolves the prediction
// against it.
func (s *sim) refPoll(poll int) {
	gap := int64(3000)
	if poll == 1 {
		gap = s.ref.rnd(400, 900)
	}
	s.ref.advance(s.ref.now + gap)
	if s.pend != nil {
		s.truth = s.resolveRef(s.pend)
	}
}

// refFactWeight is what a fact of the reference counts in refMismatch
// against an attachment, which may be in flux (a detach that the
// controller has not done yet): the node of an allocated pod, and the
// companion of a persistent lease (a lease-get cut at its bind keeps or
// releases the lease: the reference decides, A29 d). Counted as one, a
// pending detach tied with them: the truth's node side took another host
// than the reference's binding (review fuzz, full guest seed 652722:
// "pod sdax-c@any#0: on vm2, want vm0", its inspected env of a dax
// device of vm0), or kept a lease that the reference released (fullp
// qemu switch 30s, seed 1380903: "attachment c-pmem0@vm0: missing" a
// settle later, when the detach came).
const refFactWeight = 100

// refMismatch counts the pool facts in which a settled outcome of the
// model differs from the reference (the facts of refFactWeight weigh
// more).
func (s *sim) refMismatch(st *State) int {
	r := s.ref
	n := 0
	for i := range st.Slots {
		if st.slot(i).Kind != KPlse {
			continue
		}
		l := st.Slots[i].Lease
		if kept := l == LeaseHeld || l == LeaseBusy; kept != (r.comps[i] != nil) {
			n += refFactWeight
		}
	}
	for i := range st.Slots {
		sl := st.slot(i)
		if !sl.Kind.pooled() {
			continue
		}
		ss := &st.Slots[i]
		rp := r.pod(i)
		ph := rp.phase()
		if rp != nil && rp.gen != ss.Gen {
			n++
			continue
		}
		nodeOK := rp == nil || !rp.alloc || ss.Node == VMAny || ss.Node == rp.node
		if !nodeOK {
			n += refFactWeight - 1 // (and one below)
		}
		switch ss.Phase {
		case Absent:
			if ph != rpNone || r.deletingPod(i) != nil {
				n++
			}
		case Running, Blocked, Unblocking, Refused:
			// (Refused: bound, its Prepare refused; the node of an
			// "any" one decides the worlds of the watchers its
			// probe may fire: fuzz, fullp seeds 50748, 72622)
			if ph != rpBound || !nodeOK {
				n++
			}
		case Attaching:
			if ph != rpAlloc || !nodeOK {
				n++
			}
		case Either:
			if ph != rpAlloc && ph != rpBound || !nodeOK {
				n++
			}
			// its prepare was optional: the outcome with its region
			// iff it is bound
			if si, key, ok := st.userRegion(int16(i)); ok && st.Stacks[si].contains(key) != (ph == rpBound) && !s.otherUser(st, i, si, key) {
				n++
			}
		case Pending, Unsched:
			if ph != rpUnalloc {
				n++
			}
		case Stalled:
			if ph == rpNone || !nodeOK {
				n++
			}
		default:
			n++
		}
	}
	for p := Pool(0); p < nPools; p++ {
		c := &st.Ctrl[p]
		rc := &r.ctrl[p]
		if c.Up != rc.running {
			n++
		}
		if (c.ResourceSlices == SlicesPresent) != rc.slices || c.ResourceSlices == SlicesUnknown {
			n++
		}
		for d, dev := range poolDevs(p) {
			for v := VM(0); v < nVMs; v++ {
				got := r.attState(dev, v)
				switch c.Att[d][v] {
				case TriYes:
					if got != "attached" {
						n++
					}
				case TriNo:
					if got != "" {
						n++
					}
				case TriMaybe:
					if got != "attached" && got != "" {
						n++
					}
				}
			}
		}
	}
	return n
}

// otherUser: another settled user of a region on the stack holds it.
func (s *sim) otherUser(st *State, i, si int, key RKey) bool {
	for j := range st.Slots {
		if j == i {
			continue
		}
		ph := st.Slots[j].Phase
		if ph != Running && ph != Restarting && ph != Unblocking {
			continue
		}
		if sj, kj, ok := st.userRegion(int16(j)); ok && sj == si && kj == key {
			return true
		}
	}
	return false
}

// resolveRef returns the outcome of a settle's prediction that fits the
// reference best, with what the model leaves open decided by the
// reference (the pool facts) or at random (the node side).
func (s *sim) resolveRef(pred *State) *State {
	r := s.ref
	cands := append([]*State{pred}, pred.Alts...)
	if pred.Wide != nil {
		// a truncated settle: its outcomes with the nodes of the
		// "any" pods that the reference allocated (fuzz, fullp seed
		// 1484)
		nodes := map[int]VM{}
		for _, i := range pred.Wide.unplacedAny() {
			if rp := r.pod(i); rp != nil && rp.alloc && rp.gen == pred.Slots[i].Gen {
				nodes[i] = rp.node
			}
		}
		// and the bindings of its race-kill pods (review fuzz, fullp
		// seed 710078)
		bound := map[int]bool{}
		for _, i := range pred.Wide.optPrepared() {
			if rp := r.pod(i); rp != nil && rp.gen == pred.Slots[i].Gen {
				bound[i] = rp.bound || (rp.alloc && rp.attached)
			}
		}
		if n := pred.narrowed(nodes, bound); n != nil {
			cands = append([]*State{n}, n.Alts...)
		}
	}
	var best []*State
	bestN := -1
	for _, c := range cands {
		n := s.refMismatch(c)
		switch {
		case bestN < 0 || n < bestN:
			best, bestN = []*State{c}, n
		case n == bestN:
			best = append(best, c)
		}
	}
	n := best[s.rng.Intn(len(best))].clone()
	s.reorderStacks(n)
	for i := range n.Slots {
		ss := &n.Slots[i]
		sl := n.slot(i)
		if !sl.Kind.pooled() {
			continue
		}
		rp := r.pod(i)
		ph := rp.phase()
		if ph == rpAlloc && rp.attached {
			// PreBind binds it at its next poll: as bound
			ph = rpBound
		}
		if rp != nil && rp.alloc && ss.Node == VMAny && (ph == rpBound || ss.Phase == Either) {
			ss.Node = rp.node
		}
		switch ss.Phase {
		case Either:
			switch {
			case ph != rpBound:
				ss.Phase = Attaching
			case ss.Refuse:
				ss.Phase = Refused
			default:
				ss.Phase = Running
			}
		case Stalled:
			if ph != rpAlloc && ph != rpBound {
				break
			}
			// promoted (A19)
			n.setUnblockAt(i, 0)
			switch {
			case !n.Ctrl[sl.Pool].Up && ph == rpAlloc:
				ss.Phase = Attaching
			case ph == rpBound:
				if ss.Node == VMAny {
					ss.Node = rp.node
				}
				ss.Phase = Running
				// its Prepare: the region, unless a hole is in
				// the way (A9)
				if prepareRegions(n, i, ss.Refuse) {
					ss.Phase = Blocked
				}
				if ss.Refuse && ss.Phase == Running {
					ss.Phase = Refused
				}
			}
		}
	}
	// a binding timeout handed the device of a claim waiting for the
	// attach to its waiter
	for i := range n.Slots {
		sl := n.slot(i)
		if n.Slots[i].Phase != Attaching || !sl.Kind.pooled() || r.pod(i).phase() != rpUnalloc {
			continue
		}
		if w := n.waiter(sl.Res); w >= 0 && r.pod(w).phase() != rpUnalloc && r.pod(w).phase() != rpNone {
			n.Slots[i].Phase, n.Slots[w].Phase = Pending, Attaching
		}
	}
	for i := range n.Slots {
		ss := &n.Slots[i]
		if ss.Phase == Unblocking && (s.rng.Intn(2) == 0 || s.overdue(n, i, s.refWall(), kubeletRetryCap)) {
			ss.Phase = Running
			n.setUnblockAt(i, 0)
			prepareRegions(n, i, false)
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
		if n.Ctrl[p].Up {
			n.deriveAtt(p)
		}
	}
	s.refAtts(n)
	for v := VM(0); v < nVMs; v++ {
		d := &n.Drv[v]
		if d.Pod == "" || d.PodBefore != "" {
			d.Pod = fmt.Sprintf("nri-dra-driver-cxl-%s-%d", v, d.PodGen)
			d.PodBefore = ""
		}
	}
	// the truth's hold of a device ends as the oracle ends the
	// model's (heldSeconds after the settle that ended the window of
	// the unbound delete): one kept for ever stalled every later
	// create of the device in the truth, where the model had it
	// Starting, and such a stalled "any" pod fired a watcher late on a
	// host that the reference did not give it (fuzz, fullp seed 72622;
	// the stalled writers of seeds 1452, 2264, 3780)
	now := s.refWall().Unix()
	for d := DevID(0); d < nDevs; d++ {
		switch {
		case !n.held(Res(d)):
		case n.HeldAt[d] == 0:
			n.HeldAt[d] = now
		case now-n.HeldAt[d] >= heldSeconds:
			n.Held &^= 1 << uint(d)
			n.HeldAt[d] = 0
		}
	}
	n.growFence()
	// a namespace that the truth leaves maybe was not made if the
	// reference let no writer of the device prepare (fuzz, pmem seeds
	// 9442, 9762: the truth's own window had the writer's Prepare that
	// the reference never allowed)
	for x := range n.PDev {
		if pd := &n.PDev[x]; pd.NS == TriMaybe && s.prev != nil && !s.refWriterPrepared(n, cPmem0+DevID(x)) {
			p := &s.prev.PDev[x]
			pd.NS, pd.LSAHash, pd.HeadHash = p.NS, p.LSAHash, p.HeadHash
		}
	}
	s.fixPDev(n)
	s.keepKillAt(n)
	s.driverRestarts(n)
	return n
}

// refWriterPrepared: the reference may have let a writer of device d
// prepare (its claim attached or its pod bound, also a deleted one), or
// a pbw lease is in the state. A writer that the truth has Blocked did
// not: its Prepare fails at the region through the hole, before any
// namespace (pool_persistent.go preparePersistent; fuzz, fullp seeds
// 1452, 2264, 3780: a stalled writer promoted through a hole).
func (s *sim) refWriterPrepared(n *State, d DevID) bool {
	r := s.ref
	for i := range n.Slots {
		sl := n.slot(i)
		if sl.Kind == KPlse && n.Slots[i].LeaseMode == ModePBW {
			return true
		}
		if !sl.Kind.writer() || sl.Dev != d {
			continue
		}
		if p := r.pod(i); p != nil && (p.attached || p.bound) && n.Slots[i].Phase != Blocked {
			return true
		}
		for _, p := range r.old {
			if p.slot == i && (p.attached || p.bound) {
				return true
			}
		}
	}
	return false
}

// refAtts sets the attachments of a state to the reference's (attached
// and detaching ones have their memdev in the VM).
func (s *sim) refAtts(st *State) {
	for p := Pool(0); p < nPools; p++ {
		for d, dev := range poolDevs(p) {
			for v := VM(0); v < nVMs; v++ {
				switch s.ref.attState(dev, v) {
				case "attached", "detaching":
					st.Ctrl[p].Att[d][v] = TriYes
				default:
					st.Ctrl[p].Att[d][v] = TriNo
				}
			}
		}
	}
}

// refOverride puts the reference's pool facts into an observation.
func (s *sim) refOverride(o *Observation) {
	r := s.ref
	t, f := true, false
	var pods []ObsPod
	seen := map[int]bool{}
	for _, p := range o.Pods {
		i := s.cfg.slotIndex(p.Slot)
		if i < 0 || !s.cfg.Slots[i].Kind.pooled() {
			pods = append(pods, p)
			continue
		}
		sl := &s.cfg.Slots[i]
		rp := r.pod(i)
		if rp == nil || int(rp.gen) != p.Gen {
			// the reference has no such pod (any more)
			continue
		}
		seen[i] = true
		claim := &ObsClaim{Name: p.Name + "-c0", UID: claimUID(sl, rp.gen)}
		if sl.Kind.dual() {
			claim.Name = p.Name + "-c1"
		}
		switch rp.phase() {
		case rpUnalloc, rpAlloc:
			p.Phase, p.Ready, p.Node, p.Waiting, p.Event, p.Events = "Pending", false, nil, nil, nil, nil
			p.Unschedulable = rp.unsched && !rp.alloc
			delete(o.Inspect, sl.Name)
			if rp.alloc {
				claim.Allocated = true
				claim.Devices = []ObsClaimDevice{{Driver: "cxl-pool.nri.io", Pool: "pool-" + sl.Pool.String(), Device: devices[sl.Dev].Name}}
				if rp.attached {
					claim.Attached = &t
				} else {
					claim.Attached = &f
				}
				// the reference's AttachFailed condition (a conflict:
				// a binding failure at PreBind's next poll)
				failed := rp.failed
				claim.AttachFailed = &failed
			}
		case rpBound:
			node := rp.node.String()
			if p.Node == nil {
				// the model has it not bound: as the reference says
				p.Phase, p.Ready, p.Unschedulable = "Running", true, false
			}
			p.Node = &node
			claim.Allocated, claim.Attached, claim.AttachFailed = true, &t, &f
			claim.Devices = []ObsClaimDevice{{Driver: "cxl-pool.nri.io", Pool: "pool-" + sl.Pool.String(), Device: devices[sl.Dev].Name}}
		}
		p.Claim = claim
		if sl.Kind.dual() {
			// its local claim is allocated with the pool claim
			p.Claim, p.Claims = nil, simDualClaims(sl, rp.gen, p.Name, *claim)
			p.Claim = &p.Claims[0]
		}
		pods = append(pods, p)
	}
	for i := range s.cfg.Slots {
		sl := &s.cfg.Slots[i]
		if !sl.Kind.pooled() {
			continue
		}
		if rp := r.pod(i); rp != nil && !seen[i] {
			// a pod the model's truth does not have
			p := ObsPod{Slot: sl.Name, Name: podName(sl, rp.gen), UID: podUID(sl, rp.gen), Gen: int(rp.gen), Phase: "Pending", Unschedulable: rp.unsched && !rp.alloc,
				Claim: &ObsClaim{Name: podName(sl, rp.gen) + "-c0", UID: claimUID(sl, rp.gen), Allocated: rp.alloc}}
			if sl.Kind.dual() {
				c := *p.Claim
				c.Name = p.Name + "-c1"
				p.Claims = simDualClaims(sl, rp.gen, p.Name, c)
				p.Claim = &p.Claims[0]
			}
			if rp.bound {
				node := rp.node.String()
				p.Phase, p.Ready, p.Node = "Running", true, &node
			}
			pods = append(pods, p)
		}
		if dp := r.deletingPod(i); dp != nil {
			p := ObsPod{Slot: sl.Name, Name: podName(sl, dp.gen), UID: podUID(sl, dp.gen), Gen: int(dp.gen), Phase: "Pending", Deleting: true}
			if dp.bound {
				node := dp.node.String()
				p.Phase, p.Node = "Running", &node
			}
			pods = append(pods, p)
		}
	}
	o.Pods = pods
	for p := Pool(0); p < nPools; p++ {
		var list []ObsAttachment
		for _, a := range o.Attachments[p.String()] {
			if !k8sOwned(&a) {
				list = append(list, a)
			}
		}
		for _, a := range r.atts {
			if devices[a.dev].Pool == p {
				list = append(list, ObsAttachment{Device: devices[a.dev].Name, Host: a.host.String(), State: a.state, SingleHost: a.excl, Owner: "k8s:resourceclaim/x"})
			}
		}
		o.Attachments[p.String()] = list
		if r.ctrl[p].running {
			o.Controllers[p.String()] = "active"
		} else {
			o.Controllers[p.String()] = "failed"
		}
		if _, ok := o.PoolSlices[p.String()]; ok {
			list := []ObsPoolSlice{}
			if r.ctrl[p].slices {
				var nodes []string
				for _, v := range poolHosts[p] {
					nodes = append(nodes, v.String())
				}
				list = append(list, ObsPoolSlice{Name: "pool-" + p.String() + "-slice", Nodes: nodes})
			}
			o.PoolSlices[p.String()] = list
		}
	}
}
