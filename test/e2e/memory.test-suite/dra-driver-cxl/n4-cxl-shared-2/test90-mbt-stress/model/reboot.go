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

// node-reboot VM (contract A41-A44): a guest reboot or a qemu restart
// of vm0 or vm1 (A42, A43: a claim prepared in another boot is
// prepared anew at kubelet's Prepare after the boot; the leases of
// another boot are released and their companions deleted).
//
// The step is synchronous: the executor returns when the VM runs its new
// boot and the driver is ready (A15); the pods come back in kubelet's
// time (90-120 s), which the settle that must come next waits for. Only
// synchronous steps (write, read, ... of pods elsewhere) may come
// between: the window is the reboot's alone.
//
// What the boot does to the model:
//   - every pod on the VM keeps its UID and its claims' allocations; the
//     container of a Running one restarts once (+1); a pod without a
//     claim may restart once more (Slack 1, adopted): its container does
//     not wait for the driver, and the boot's first kubelet may have
//     created it before it was stopped (kubelet started twice in the
//     boot, A43). The driver pod: +1, Slack 1 too.
//   - the decoder stacks of the VM are empty at the boot; kubelet
//     prepares every claim again in its own order (back-off of its
//     first Prepares that fail before the driver registers): the
//     regions are pushed in an order that the model does not know. The
//     window plays the prepares in slot order (the stacks have no hole,
//     every prepare succeeds), and a stack with two or more regions is
//     marked Reorder: the oracle adopts the observed order (A9's
//     adoption). Off-line nothing adopts: there a node-reboot is
//     generated only when no stack of the VM gets two regions.
//   - a Blocked pod is prepared then (no hole): Running, no restart
//     (it had no container); a refused one stays Refused (the content
//     of a persistent device survives: A25's refusal stands).
//   - the leases of the VM are released (their fds, binds and lent
//     memory went with the boot): the broker of the new boot drops them
//     at its start and its first reconcile deletes their companions; a
//     waiter of what they held gets it. A Prepare of a companion's claim
//     after the boot fails before it touches a device (OnLending: no
//     such lease, A42), so it makes no region.
//   - volatile dax content: the token or zeros until a write or a read
//     (Token.OrZero); ldax slices are new (zeros); numa memory is
//     onlined again (unknown bytes); persistent content is kept.
//   - F of the VM: the numa nodes of the claims prepared in the new boot
//     (the driver starts with none prepared); lsteer containers are
//     steered (created after the driver is up); no container is loose.
//   - attachments: guest: unchanged; qemu: dropped by the pool servers
//     and attached again by the controllers (A44), the same set at
//     the settle; the local memdev again by the executor (A42).
//   - the driver's armed driver-kill-at watcher is disarmed (A42); the
//     boot id must change (A44).
//
// When it is generated (rebootOK, next): the window is empty (a settle
// came last), no armed watcher of the VM may have fired (A31, A32), every
// pool controller runs with its slices, and no pod that is or may be on
// the VM waits for a retry or a promotion that comes at a time the model
// does not know (Unblocking, Stalled), or for an attach (Attaching,
// Either, Unsched): whether that came before the boot decides whether its
// container restarts. Pods being deleted or starting exist only in a
// window. Pending pods are not bound: the boot does not touch them.

// rebootOK: node-reboot of VM v may be generated (see above).
func (s *State) rebootOK(v VM) bool {
	if s.cfg.Reboot == RebootNone || !rebootVM(v) || s.Unsettled != 0 || s.killAtMayHaveFired(v) || s.Drv[v].Killed {
		return false
	}
	for p := range s.Ctrl {
		if !s.Ctrl[p].Up || s.Ctrl[p].ResourceSlices != SlicesPresent {
			return false
		}
	}
	for i := range s.Slots {
		if !s.mayLandOn(i, v) {
			continue
		}
		switch s.Slots[i].Phase {
		case Absent, Running, Pending, Blocked, Refused:
		default:
			return false
		}
	}
	return true
}

// nodeReboot applies a node-reboot of VM v (see above).
func (n *State) nodeReboot(v VM) {
	d := &n.Drv[v]
	d.KillAt, d.KillAtAt, d.KillMay, d.KillSlack = 0, 0, false, false // (A42)
	d.Restarts = inc8(d.Restarts)
	d.Inc++
	d.Slack = inc8(d.Slack)
	d.Creating = false
	d.Rebooted = true
	d.Reboots = inc8(d.Reboots)
	if d.BootBefore == "" {
		d.BootBefore = d.Boot
	}
	d.Boot = ""
	for b := Bridge(0); b < nBridges; b++ {
		n.Stacks[stackIndex(v, b)] = Stack{}
	}
	var released []int
	for i := range n.Slots {
		ss, sl := &n.Slots[i], n.slot(i)
		if sl.Kind.leaseKind() && sl.VM == v && ss.Lease != LeaseNone {
			ss.Lease, ss.LeaseOpen = LeaseReleasing, false
			released = append(released, i)
		}
		if ss.Node != v {
			continue
		}
		// (new containers: nothing held, nothing loose)
		ss.Hold, ss.Loose, ss.CompLoose = false, false, false
		switch ss.Phase {
		case Running:
			ss.Phase = Restarting
			ss.Restarts = inc8(ss.Restarts)
			if !sl.Kind.claimed() {
				ss.Slack = inc8(ss.Slack)
			}
		case Blocked:
			if ss.Refuse {
				// its retry is refused now that no hole is in the way
				ss.Phase = Refused
				continue
			}
		default:
			continue
		}
		n.prepareEvent(i, -1)
		if sl.Kind == KLSteer {
			// Strict on purpose: the container is created after its
			// Prepare, which comes after the new driver registered. A
			// driver that serves a Prepare before its NRI plugin is up
			// leaves the container unsteered (the driver's race after a
			// boot, A43): a run with such a
			// driver fails here, which is intended. Do not relax to
			// SteerAny.
			ss.Steer = SteerOn
		}
		switch {
		case sl.Kind == KLDax:
			n.setSlotData(i, tokZero)
		case sl.Kind == KXNUMA || sl.Kind == KDNUMA:
			n.Data[sl.Dev] = tokUnknown
		case sl.Kind.dax() && ss.Phase == Restarting:
			n.Data[sl.Dev] = orZero(n.Data[sl.Dev])
		}
	}
	for _, i := range released {
		r := localRes(v)
		if n.slot(i).Kind == KPlse {
			r = Res(cPmem0)
		}
		n.promote(r, -1, false)
	}
	var def uint8
	if n.Fence == FenceAM {
		for i := range n.Slots {
			ss, sl := &n.Slots[i], n.slot(i)
			if ss.Node != v || !sl.Kind.numa() || (ss.Phase != Restarting && ss.Phase != Blocked) {
				continue
			}
			for _, node := range sl.numaNodes(n.cfg) {
				def |= n.fenceBit(node)
			}
		}
	}
	d.F, d.FMaybe = def, 0
	for b := Bridge(0); b < nBridges; b++ {
		if si := stackIndex(v, b); len(n.windowKeys(si)) > 1 {
			n.Reorder |= 1 << uint(si)
		}
	}
}

// windowKeys returns the regions that the prepares of the window make on
// a stack (a refused claim's probe makes none), in issue order.
func (s *State) windowKeys(si int) []RKey {
	var keys []RKey
	for _, e := range s.Ev {
		if int(e.Stack) == si && e.Kind == evPrepare && !e.Probe && !containsKey(keys, e.Key) {
			keys = append(keys, e.Key)
		}
	}
	return keys
}

// rebooted: a node-reboot of the VM of stack si came in this window (its
// stack was empty at the boot, nothing was prepared on it).
func (s *State) rebooted(si int) bool { return s.Drv[stackVM(si)].Rebooted && s.Unsettled > 0 }

// inc8 increments a counter up to 255.
func inc8(x uint8) uint8 {
	if x < 255 {
		return x + 1
	}
	return x
}
