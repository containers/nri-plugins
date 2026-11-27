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

// The model state: the slots of contract.txt 3. Every slot
// holds at most one pod; what the pods hold (pool devices, local blocks,
// leases) is derived from the slots. The state is immutable once a
// transition has returned it; String() and the coverage abstraction are
// memoized (race-free: the planner and the oracle share states).

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// Phase is the phase of the pod of a slot.
type Phase uint8

const (
	// Absent: no pod.
	Absent Phase = iota
	// Starting: created (or promoted from Pending) in the current
	// window; Running at the next settle, or Attaching if its pool
	// controller is down then. Holds its resource.
	Starting
	// Restarting: crashed in the current window; Running again at the
	// next settle with one more restart. Holds its resource.
	Restarting
	// Running: settled, Running and Ready.
	Running
	// Pending: unschedulable, waits for its resource (at most one per
	// resource). Holds nothing.
	Pending
	// Attaching: allocated, waits for the attach of its pool device
	// while the pool controller is down. Holds its device.
	Attaching
	// Either: Running or Attaching, the outcome of a pool controller
	// killed while the pod was starting. Resolved by the observation.
	Either
	// Deleting: deleted in the current window; absent at the next
	// settle. Holds its resource if Prev did.
	Deleting
	// Blocked: allocated (and attached), but its Prepare needs a new
	// region through a decoder stack with a hole (A9): Pending,
	// ContainerCreating, retried by kubelet. Holds its resource.
	Blocked
	// Unblocking: was blocked, the hole has cleared: Running at
	// kubelet's next retry (within --unblock-timeout); ContainerCreating
	// is accepted meanwhile. Holds its resource.
	Unblocking
	// Unsched: a pool claim that the scheduler cannot allocate because
	// its pool has no ResourceSlices (a controller killed before it
	// published them again, A18): Pending, unschedulable, until the
	// controller is back. Holds its resource in the model (it is the
	// only candidate for it then).
	Unsched
	// Stalled: the contender of a device whose holder was deleted
	// while its claim waited for the binding conditions (A19):
	// the scheduler may keep it Pending (claim not allocated) until a
	// requeueing event or the scheduler's unschedulable flush (pods
	// unschedulable for 5 min, flushed every 30 s) after the delete, or
	// promote it at once. Both are accepted for
	// defaultStallWindow; the observation decides. Holds the
	// device in the model (the only candidate).
	Stalled
	// Refused: an allocated, attached persistent claim whose Prepare the
	// device refuses (fs: no fsdax namespace or no erofs; block ro: no
	// namespace) or the config refuses (pbad): Pending,
	// ContainerCreating, retried ~60-90 s apart; holds its share, which
	// keeps every writer out: it lasts until the pod is deleted (A25).
	Refused
	nPhases
)

var phaseNames = [nPhases]string{"absent", "starting", "restarting", "running", "pending", "attaching", "either", "deleting", "blocked", "unblocking", "unsched", "stalled", "refused"}

func (p Phase) String() string { return phaseNames[p] }

func parsePhase(s string) (Phase, error) {
	for p := Phase(0); p < nPhases; p++ {
		if phaseNames[p] == s {
			return p, nil
		}
	}
	return 0, fmt.Errorf("invalid phase %q", s)
}

// holding: the pod of the phase holds (or will hold) its resource and is
// not being deleted.
func (p Phase) holding() bool {
	return p == Starting || p == Restarting || p == Running || p == Attaching || p == Either || p == Blocked || p == Unblocking || p == Unsched || p == Stalled || p == Refused
}

// unbound: a pool pod of the phase may have its claim allocated while
// it is not bound yet (PreBind waits for the binding conditions).
func (p Phase) unbound() bool {
	return p == Starting || p == Attaching || p == Either || p == Stalled
}

// waitsRetry: the pod gets its region at a time the model does not know
// (kubelet's retry, the scheduler's requeue): nothing else may change its
// decoder stack meanwhile.
func (p Phase) waitsRetry() bool { return p == Unblocking || p == Stalled }

// Steer is what memory.high of an lsteer container is.
type Steer uint8

const (
	SteerNone Steer = iota // not an lsteer slot, or no container
	SteerOn                // steered by the running driver: memory.high != max
	SteerOff               // its manager died with an earlier driver: max
	SteerAny               // started in a driver restart window: either
)

var steerNames = []string{"", "on", "off", "any"}

func (s Steer) String() string { return steerNames[s] }

// LeaseSt is the lease of a lease slot.
type LeaseSt uint8

const (
	LeaseNone      LeaseSt = iota
	LeaseHeld              // Ready, with its companion pod
	LeaseReleasing         // released in the current window: gone at the settle
	// LeaseBusy: lease-put of an fs lease whose file is held: Releasing,
	// its companion Terminating, bind, node mount and region kept,
	// pread works, until unhold, a crash or the delete of the requester
	// (A25); then Releasing
	LeaseBusy
)

func (l LeaseSt) String() string {
	return [...]string{"none", "held", "releasing", "busy"}[l]
}

// LeaseMode is the mode of a lease: numa or dax of the local memdev
// (lease kind), or of c-pmem0 (plse, A23): pbw block writer (the whole
// device), pbr block read-only share, pfd fs direct share (name mbt),
// pfc fs cached share (name = the lease id).
type LeaseMode uint8

const (
	ModeNUMA LeaseMode = iota
	ModeDax
	ModePBW
	ModePBR
	ModePFD
	ModePFC
	nModes
)

var modeNames = [nModes]string{"numa", "dax", "pbw", "pbr", "pfd", "pfc"}

func (m LeaseMode) String() string { return modeNames[m] }

func parseLeaseMode(s string) (LeaseMode, bool) {
	for m := LeaseMode(0); m < nModes; m++ {
		if modeNames[m] == s {
			return m, true
		}
	}
	return 0, false
}

// persistent: a lease of c-pmem0 (plse).
func (m LeaseMode) persistent() bool { return m >= ModePBW }

// fs: an fs lease (a bind of the node mount).
func (m LeaseMode) fs() bool { return m == ModePFD || m == ModePFC }

// kind is the claim kind of the lease's companion (its prepare rules).
func (m LeaseMode) kind() Kind {
	switch m {
	case ModePBW:
		return KPWr
	case ModePBR:
		return KPBro
	case ModePFD:
		return KPFsd
	case ModePFC:
		return KPFsc
	}
	return KLease
}

// Token is what offset 0 of a dax device holds. OrZero: Text or zeros
// (""), not known which: a volatile dax device whose region a node
// reboot made again (A43), until the next write or read decides.
type Token struct {
	Known  bool
	Text   string
	OrZero bool
}

func (t Token) String() string {
	if !t.Known {
		return "?"
	}
	if t.OrZero {
		return fmt.Sprintf("%q|zero", t.Text)
	}
	return fmt.Sprintf("%q", t.Text)
}

// orZero is the token of a device whose content may be gone: the token
// or zeros (an unknown or zero token stays as it is).
func orZero(t Token) Token {
	if !t.Known || t.Text == "" {
		return t
	}
	t.OrZero = true
	return t
}

var (
	tokUnknown = Token{}
	tokZero    = Token{Known: true}
)

// Tri is a three-valued attachment.
type Tri uint8

const (
	TriNo Tri = iota
	TriYes
	TriMaybe
)

// SlotState is the pod of a slot. The data of ldax slices and the lease
// IDs are in State.SlotData and State.LeaseIDs (shared between states
// until changed), so that copying the slots of a state is cheap.
type SlotState struct {
	Phase     Phase
	Prev      Phase // Deleting: the phase when deleted
	Node      VM    // the VM of the pod; VMAny until known (any slots)
	Gen       uint16
	Restarts  uint8
	RaceKill  bool // Starting when its pool controller was killed
	Steer     Steer
	Lease     LeaseSt
	LeaseMode LeaseMode
	// Slack: restarts the pod may have more than its crashes (A34: a
	// kubelet restart while its container is being created)
	Slack uint8
	// Hold: a process of the container maps hello.txt of its fs claim
	// or fs lease read-only (hold, A23); a crash and the delete end it
	Hold bool
	// Refuse: the Prepare of this persistent claim is refused (A25):
	// Refused when it would be Running
	Refuse bool
	// LeaseOpen: a lease-get of this plse slot failed under a
	// driver-kill-at bind (A29 d, A30): the lease may be released, or
	// still Lending and Ready after kubelet's Prepare retry; the settle
	// has both outcomes
	LeaseOpen bool
	// Alloc: a Starting pool claim allocated before the window (an
	// attach-waiter that ctrl-start made Starting; A18, fuzz pmem seed
	// 10091): it keeps its allocation when the slices go, so the world
	// without them leaves it Starting. (ctrl-restart is not generated
	// while a claim is being allocated; a stalled contender promoted
	// before the slices went is the oracle's.) Cleared at the settle.
	Alloc bool
	// Loose: the container existed at a switch of the fencing mode to
	// none (A37): it keeps the cpuset.mems it had until it is created
	// again (a crash after the switch, a new pod). CompLoose: so does
	// the companion of its lease, until the lease is released.
	Loose, CompLoose bool
}

// holds: the slot holds its resource now (also while deleting).
func (ss *SlotState) holds() bool {
	return ss.Phase.holding() || (ss.Phase == Deleting && ss.Prev.holding())
}

// FenceMode is the fencing mode of the drivers (defaults.container.fencing
// of the driver configuration, A36): allowed-mems or none.
type FenceMode uint8

const (
	FenceAM FenceMode = iota
	FenceNone
)

var fenceModeNames = []string{"allowed-mems", "none"}

func (m FenceMode) String() string { return fenceModeNames[m] }

// short is the mode in coverage strings (A40): am or none.
func (m FenceMode) short() string { return [...]string{"am", "none"}[m] }

// DriverState is the driver of a VM.
type DriverState struct {
	PodGen    uint16 // driver pods replaced by driver-restart
	Restarts  uint8  // container restarts of the current pod (driver-kill)
	Inc       uint16 // incarnations: kills and restarts
	Killed    bool   // driver-kill in the current window
	Restarted bool   // driver-restart in the current window
	F, FMaybe uint8  // fenced host bridges (bit b: NUMA node NUMANodeHB[b]); maybe
	Pod       string // the pod name, learnt at a settle
	PodBefore string // driver-restart: the pod that must be gone
	// KillAt: driver-kill-at armed in the current window (marker+1):
	// the driver is killed at the marker of the next persistent Prepare
	// of the VM, or not (60 s): both outcomes, adopted from "killAt"
	KillAt uint8
	// KillAtAt: the Unix time of the first settle that saw it armed (the
	// oracle disarms it 70 s later: the watcher gives up after 60 s)
	KillAtAt int64
	// KillMay: the armed watcher may have fired in the current window: a
	// Prepare that comes at any time (killAnyTime) could show its marker
	// before one of the window's steps, also when its claim is gone by
	// the settle (A32)
	KillMay bool
	// Slack: restarts the pod may have more than its kills (A34 for
	// the driver pod: a kubelet restart while its container was being
	// created again after a kill); adopted at the settle
	Slack uint8
	// Creating: its container may be being created again after a
	// driver-kill of the window that no step has waited out yet
	// (kubelet-restart waits for the driver)
	Creating bool
	// KillSlack: a kubelet restart came while the armed watcher may
	// have fired (killAtMayHaveFired): the world in which it fired
	// has Slack +1
	KillSlack bool
	// Rebooted: node-reboot of the VM in the current window, or in the
	// window that the last settle ended (that settle's observation is
	// the first of the new boot: A43, A44); cleared when the next window
	// starts
	Rebooted bool
	// Reboots counts the node-reboots of the VM (the simulator's boot
	// ids); Boot is the boot id learnt at a settle or from node-reboot's
	// done, BootBefore the one that must be gone after a node-reboot
	// (A44): the oracle's
	Reboots          uint8
	Boot, BootBefore string
}

// Kill markers of driver-kill-at (A23).
var markerNames = []string{"region", "ns", "mount", "bind"}

// the indexes of markerNames
const (
	markerRegion = iota
	markerNS
	markerMount
	markerBind
)

func parseMarker(s string) (int, bool) {
	for i, m := range markerNames {
		if m == s {
			return i, true
		}
	}
	return 0, false
}

// PDev is what a persistent device holds (A25): its fsdax namespace
// (no, yes, maybe), an erofs (FS), the token of its hello.txt (Tok) and
// bytes 0-63 (Raw), and the namespace UUID once observed.
type PDev struct {
	NS   Tri
	FS   Tri // TriYes: the template erofs; TriNo: none
	Tok  Token
	Raw  Token
	UUID string
	// the hashes of the LSA and of the first 4 MiB of the data file as
	// observed (pdevs); "": not known (after an event that changes
	// them: adopted at the next settle)
	LSAHash, HeadHash string
}

// freshPDev is a device without labels and data (zeros).
var freshPDev = PDev{NS: TriNo, FS: TriNo, Raw: tokZero}

// CtrlState is the controller of a pool, and what is known of the
// attachments of its devices (index 0 pooled, 1 shared) per VM when it
// is down; when it is up, the attachments are derived from the claims at
// every settle.
type CtrlState struct {
	Up             bool
	Att            [devsPerPool][nVMs]Tri
	ResourceSlices ResourceSlicesState // the pool's ResourceSlices (A18)
}

// ResourceSlicesState tells if a pool's controller has published its ResourceSlices.
type ResourceSlicesState uint8

const (
	SlicesPresent ResourceSlicesState = iota
	SlicesAbsent                      // deleted by a graceful stop, not published again
	SlicesUnknown                     // a graceful restart: published again soon, or not
)

var resourceSliceNames = []string{"present", "absent", "unknown"}

func (s ResourceSlicesState) String() string { return resourceSliceNames[s] }

// State is the model state.
type State struct {
	cfg       *Config
	Slots     []SlotState
	Ctrl      [nPools]CtrlState
	Drv       [nVMs]DriverState
	Data      [nDevs]Token // pool devices
	PDev      [nPDevs]PDev // persistent devices (A25)
	SlotData  []Token      // ldax slots: the content of the slice (copy on write)
	LeaseIDs  []string     // lease slots: the lease ID if known (copy on write)
	UnblockAt []int64      // Unblocking, Stalled, Attaching slots: Unix time first seen so on-line (copy on write)
	Stacks    [nStacks]Stack
	Ev        []stackEvent // region events of the current window (copy on write)
	Alts      []*State     // a settle: the other possible outcomes (A9 races)
	Truncated bool         // a settle: its outcomes were not all enumerated
	// Wide: a truncated settle with "any" pods whose nodes were not
	// known: the state it was settled from, so that the oracle can
	// settle it again with the nodes it observes (narrowed). Narrow: a
	// settle so narrowed, and the outcome adopted from it: those nodes
	// ("SLOT:VM,...", the adoption record's any=). Neither is cloned.
	Wide      *State
	Narrow    string
	Contested uint16 // resources with an unsettled transfer: no creates
	// Held: pool devices whose claim was deleted before its pod was
	// bound, in a window that a settle ended: kube-scheduler may still
	// hold the device in flight (up to a PreBind poll period, 5 s, after the
	// delete, A19): a contender created now may stall. HeldAt: the Unix
	// time of the settle that ended that window (on-line; the oracle
	// clears the bit heldSeconds after it).
	Held   uint16
	HeldAt [nDevs]int64
	// Deallocating: pool devices whose claim was deleted in the window that
	// the last settle ended: the claim may still be deallocated a
	// moment after its pod is gone; a claim created now may
	// fail its first attempt and wait for that event, and a second one
	// would race with it: one candidate (contested) per such device
	Deallocating uint16
	Unsettled    uint8
	// Fence: the fencing mode of the drivers now (fencing-switch flips
	// it, A37); with none no driver fences (F and FMaybe stay empty)
	Fence FenceMode
	// Switched: a fencing-switch came in this window: no other one
	// until the settle (two in a row would switch back before anything
	// observed the first)
	Switched bool
	// Reorder: the decoder stacks (bit si) that a node-reboot of this
	// window, or of the window that the last settle ended, rebuilt with
	// two or more regions: kubelet prepares the claims again in an order
	// that the model does not know; the stack has the regions in slot
	// order and the oracle adopts the observed order (A43, A9)
	Reorder uint8
	Steps   int32 // steps executed: the next step is Steps+1
	Last    Act   // the action that led to this state
	memo    atomic.Pointer[stateMemo]
	occMemo atomic.Pointer[occupancy]
}

type stateMemo struct {
	abstract []string
	str      string
}

// newState is the state at the start of a test: nothing allocated, the
// controllers up, nothing fenced, the content of pool devices unknown.
func newState(cfg *Config) *State {
	s := &State{
		cfg:       cfg,
		Slots:     make([]SlotState, len(cfg.Slots)),
		SlotData:  make([]Token, len(cfg.Slots)),
		LeaseIDs:  make([]string, len(cfg.Slots)),
		UnblockAt: make([]int64, len(cfg.Slots)),
	}
	for i := range s.Slots {
		s.Slots[i].Node = cfg.Slots[i].VM
	}
	for p := range s.Ctrl {
		s.Ctrl[p].Up = true
	}
	for x := range s.PDev {
		s.PDev[x] = freshPDev
	}
	if cfg.Fencing == FencingNone {
		s.Fence = FenceNone
	}
	s.Last = Act{Verb: VNone, Slot: -1}
	return s
}

func (s *State) clone() *State {
	n := &State{
		cfg:          s.cfg,
		Slots:        append([]SlotState(nil), s.Slots...),
		Ctrl:         s.Ctrl,
		Drv:          s.Drv,
		Data:         s.Data,
		PDev:         s.PDev,
		SlotData:     s.SlotData,
		LeaseIDs:     s.LeaseIDs,
		UnblockAt:    s.UnblockAt,
		Stacks:       s.Stacks,
		Ev:           s.Ev,
		Contested:    s.Contested,
		Held:         s.Held,
		Deallocating: s.Deallocating,
		HeldAt:       s.HeldAt,
		Unsettled:    s.Unsettled,
		Fence:        s.Fence,
		Switched:     s.Switched,
		Reorder:      s.Reorder,
		Steps:        s.Steps,
		Last:         s.Last,
	}
	return n
}

// setSlotData sets the data of an ldax slot (copy on write: call only
// on a state that a transition is building).
func (s *State) setSlotData(i int, t Token) {
	if s.SlotData[i] == t {
		return
	}
	s.SlotData = append([]Token(nil), s.SlotData...)
	s.SlotData[i] = t
}

// setLeaseID sets the lease ID of a lease slot (copy on write).
func (s *State) setLeaseID(i int, id string) {
	if s.LeaseIDs[i] == id {
		return
	}
	s.LeaseIDs = append([]string(nil), s.LeaseIDs...)
	s.LeaseIDs[i] = id
}

// unblockAt is when an Unblocking slot was first seen so (0: unknown).
func (s *State) unblockAt(i int) int64 { return s.UnblockAt[i] }

// setUnblockAt sets it (copy on write).
func (s *State) setUnblockAt(i int, t int64) {
	if s.UnblockAt[i] == t {
		return
	}
	s.UnblockAt = append([]int64(nil), s.UnblockAt...)
	s.UnblockAt[i] = t
}

// slot returns the static slot i.
func (s *State) slot(i int) *Slot { return &s.cfg.Slots[i] }

// --- derived occupancy ---

// occupancy counts what the slots hold. Holders "held" are not going
// away, "all" includes those that are being deleted or released.
type occupancy struct {
	exclHeld, exclAll   [nDevs]int8 // exclusive users of a pool device
	shareHeld, shareAll [nDevs]int8 // shares of a pool device
	localHeld, localAll [nVMs]int8  // blocks of the local memdev
	waiter              [nRes]int16 // the Pending slot of a resource, -1
}

func (s *State) computeOcc() *occupancy {
	o := &occupancy{}
	for r := range o.waiter {
		o.waiter[r] = -1
	}
	for i := range s.Slots {
		ss := &s.Slots[i]
		sl := s.slot(i)
		if sl.Kind == KLease {
			switch ss.Lease {
			case LeaseHeld:
				o.localHeld[sl.VM]++
				o.localAll[sl.VM]++
			case LeaseReleasing:
				o.localAll[sl.VM]++
			}
			continue
		}
		if sl.Kind == KPlse {
			// a persistent lease's companion claim takes c-pmem0: whole
			// (pbw) or a share
			if ss.Lease == LeaseNone {
				continue
			}
			held := ss.Lease == LeaseHeld || ss.Lease == LeaseBusy
			if ss.LeaseMode == ModePBW {
				if held {
					o.exclHeld[cPmem0]++
				}
				o.exclAll[cPmem0]++
			} else {
				if held {
					o.shareHeld[cPmem0]++
				}
				o.shareAll[cPmem0]++
			}
			continue
		}
		if sl.Res == resNone {
			continue
		}
		if ss.Phase == Pending {
			o.waiter[sl.Res] = int16(i)
			if sl.Kind.dual() {
				// (it waits for both: the scheduler allocates the
				// claims of a pod together)
				o.waiter[localRes(sl.VM)] = int16(i)
			}
			continue
		}
		held, all := ss.Phase.holding(), ss.holds()
		if sl.Kind.dual() {
			if held {
				o.localHeld[sl.VM]++
			}
			if all {
				o.localAll[sl.VM]++
			}
		}
		switch {
		case sl.Kind.local():
			if held {
				o.localHeld[sl.VM]++
			}
			if all {
				o.localAll[sl.VM]++
			}
		case sl.Kind.share():
			if held {
				o.shareHeld[sl.Dev]++
			}
			if all {
				o.shareAll[sl.Dev]++
			}
		default:
			if held {
				o.exclHeld[sl.Dev]++
			}
			if all {
				o.exclAll[sl.Dev]++
			}
		}
	}
	return o
}

// occ returns the memoized occupancy of a state that no transition
// changes any more.
func (s *State) occ() *occupancy {
	if o := s.occMemo.Load(); o != nil {
		return o
	}
	o := s.computeOcc()
	s.occMemo.Store(o)
	return o
}

// localUse counts the blocks of the local memdev of a VM.
func (s *State) localUse(v VM) (held, all int) {
	o := s.computeOcc()
	return int(o.localHeld[v]), int(o.localAll[v])
}

// fitsIn tells if slot i would get its resource: now (counting holders
// that are going away) and later (when they are gone).
func (s *State) fitsIn(o *occupancy, i int) (now, later bool) {
	sl := s.slot(i)
	switch {
	case sl.Res == resNone:
		return true, true
	case sl.Kind.local():
		b := s.cfg.LocalBlocks[sl.VM]
		return int(o.localAll[sl.VM])+1 <= b, int(o.localHeld[sl.VM])+1 <= b
	case sl.Kind.share():
		return o.exclAll[sl.Dev] == 0, o.exclHeld[sl.Dev] == 0
	case sl.Kind.dual():
		// c-pooled0 and a block of the local memdev
		d, b := sl.Dev, s.cfg.LocalBlocks[sl.VM]
		return o.exclAll[d]+o.shareAll[d] == 0 && int(o.localAll[sl.VM])+1 <= b,
			o.exclHeld[d]+o.shareHeld[d] == 0 && int(o.localHeld[sl.VM])+1 <= b
	default: // exclusive use of a pool device
		d := sl.Dev
		return o.exclAll[d]+o.shareAll[d] == 0, o.exclHeld[d]+o.shareHeld[d] == 0
	}
}

// fits is fitsIn with fresh counts (for states being built).
func (s *State) fits(i int) (now, later bool) { return s.fitsIn(s.computeOcc(), i) }

// holders counts the slots that hold a resource and are not going away.
func (s *State) holders(r Res) int {
	n := 0
	for i := range s.Slots {
		if s.slot(i).holdsRes(r) && s.Slots[i].Phase.holding() {
			n++
		}
	}
	return n
}

// waiter returns the Pending slot of a resource, -1 if none.
func (s *State) waiter(r Res) int {
	for i := range s.Slots {
		if s.Slots[i].Phase == Pending && s.slot(i).holdsRes(r) {
			return i
		}
	}
	return -1
}

func (s *State) contested(r Res) bool { return r >= 0 && s.Contested&(1<<uint(r)) != 0 }

// held tells if a pool device may still be held in flight by the
// scheduler for a claim deleted before its pod was bound (A19).
func (s *State) held(r Res) bool { return r >= 0 && r < resLocal0 && s.Held&(1<<uint(r)) != 0 }

// desiredAtt are the attachments that the claims of a pool want: the
// device (0 pooled, 1 shared) on the VM of every holder that is not going
// away, and if one of them uses the shared device exclusively.
func (s *State) desiredAtt(p Pool) (att [devsPerPool][nVMs]bool, excl [devsPerPool][nVMs]bool) {
	for i := range s.Slots {
		sl := s.slot(i)
		ss := &s.Slots[i]
		if sl.Kind == KPlse && p == PoolC && (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) && ss.LeaseMode.persistent() {
			// the companion claim of a persistent lease, on the
			// requester's node
			d := devIndex(cPmem0)
			att[d][sl.VM] = true
			if ss.LeaseMode == ModePBW {
				excl[d][sl.VM] = true
			}
			continue
		}
		if !sl.Kind.pooled() || sl.Pool != p {
			continue
		}
		if !ss.Phase.holding() || ss.Phase == Unsched || ss.Phase == Stalled || ss.Node == VMAny {
			continue
		}
		d := devIndex(sl.Dev)
		att[d][ss.Node] = true
		if sl.Kind.wholeShared() {
			excl[d][ss.Node] = true
		}
	}
	return
}

// anyUnknownHolder: an "any" slot of the pool holds its device on a node
// that is not known yet.
func (s *State) anyUnknownHolder(p Pool) bool {
	for i := range s.Slots {
		sl := s.slot(i)
		if sl.Kind.pooled() && sl.Pool == p && s.Slots[i].Node == VMAny && s.Slots[i].holds() {
			return true
		}
	}
	return false
}

// fenceBit is the bit of a NUMA node in DriverState.F.
func (s *State) fenceBit(node int) uint8 {
	b := s.cfg.NUMANodeBridge(node)
	if b < 0 {
		return 0
	}
	return 1 << uint(b)
}

// fenceNUMANodes returns the nodes of fence bits.
func (s *State) fenceNUMANodes(bits uint8) []int {
	var nodes []int
	for b := Bridge(0); b < nBridges; b++ {
		if bits&(1<<uint(b)) != 0 {
			nodes = append(nodes, s.cfg.NUMANodeHB[b])
		}
	}
	return nodes
}

// --- abstraction (C4) and String ---

// abstract returns the coverage abstraction of the state (C4 of
// the flag --coverage): per pool device its users by kind and phase class,
// per local memdev its fill and users, per driver and controller their
// state. Memoized.
func (s *State) abstract() []string {
	if m := s.memo.Load(); m != nil {
		return m.abstract
	}
	m := &stateMemo{abstract: s.computeAbstract()}
	m.str = strings.Join(m.abstract, "; ")
	s.memo.Store(m)
	return m.abstract
}

// String is the canonical abstraction of the state (the C4 strings),
// not the full state: see dump for that.
func (s *State) String() string {
	s.abstract()
	return s.memo.Load().str
}

// phaseClass is the class of a phase in the abstraction.
func phaseClass(p Phase) string {
	switch p {
	case Starting:
		return "new"
	case Restarting, Running:
		return "run"
	case Attaching, Either:
		return "att"
	case Pending:
		return "wait"
	case Deleting:
		return "gone"
	case Blocked:
		return "blk"
	case Unblocking:
		return "unb"
	case Unsched:
		return "uns"
	case Stalled:
		return "stl"
	case Refused:
		return "ref"
	}
	return ""
}

func triName(t Tri) string { return [...]string{"no", "yes", "maybe"}[t] }

// devUsed: a slot of the profile uses the device (persistent leases:
// c-pmem0).
func (c *Config) devUsed(d DevID) bool {
	for i := range c.Slots {
		sl := &c.Slots[i]
		if (sl.Kind.pooled() && sl.Dev == d) || (sl.Kind == KPlse && d == cPmem0) {
			return true
		}
	}
	return false
}

func (s *State) computeAbstract() []string {
	var out []string
	var devUsed [nDevs]bool
	var vmLocal [nVMs]bool
	var poolUsed [nPools]bool
	for i := range s.cfg.Slots {
		sl := s.slot(i)
		switch {
		case sl.Kind.dual():
			devUsed[sl.Dev] = true
			poolUsed[sl.Pool] = true
			vmLocal[sl.VM] = true
		case sl.Kind.pooled():
			devUsed[sl.Dev] = true
			poolUsed[sl.Pool] = true
		case sl.Kind.local() || sl.Kind == KLease:
			vmLocal[sl.VM] = true
		case sl.Kind == KPlse:
			devUsed[cPmem0] = true
			poolUsed[PoolC] = true
		}
	}
	set := func(prefix string, items []string) string {
		sort.Strings(items)
		items = uniq(items)
		if len(items) == 0 {
			return prefix + " free"
		}
		return prefix + " " + strings.Join(items, ",")
	}
	for d := DevID(0); d < nDevs; d++ {
		if !devUsed[d] {
			continue
		}
		var items []string
		for i := range s.Slots {
			sl := s.slot(i)
			ss := &s.Slots[i]
			if sl.Dev == d && sl.Kind.pooled() && ss.Phase != Absent {
				it := sl.Kind.String() + "." + phaseClass(ss.Phase)
				if ss.Hold {
					it += ".hold"
				}
				items = append(items, it)
			}
			if sl.Kind == KPlse && d == cPmem0 && ss.Lease != LeaseNone {
				st := map[LeaseSt]string{LeaseHeld: "held", LeaseReleasing: "gone", LeaseBusy: "busy"}[ss.Lease]
				if ss.Hold {
					st += ".hold"
				}
				items = append(items, "lease-"+ss.LeaseMode.String()+"."+st)
			}
		}
		str := set("dev "+d.String(), items)
		if devices[d].Persistent {
			pd := &s.PDev[pmemIndex(d)]
			str += " ns-" + triName(pd.NS) + " fs-" + triName(pd.FS)
		}
		if s.held(Res(d)) {
			str += " held"
		}
		if !s.Ctrl[devices[d].Pool].Up {
			str += " ctrl-down"
		}
		out = append(out, str)
	}
	for v := VM(0); v < nVMs; v++ {
		if !vmLocal[v] {
			continue
		}
		var items []string
		for i := range s.Slots {
			sl := s.slot(i)
			ss := &s.Slots[i]
			if sl.Kind.hasLocal() && sl.VM == v && ss.Phase != Absent {
				items = append(items, sl.Kind.String()+"."+phaseClass(ss.Phase))
			}
			if sl.Kind == KLease && sl.VM == v && ss.Lease != LeaseNone {
				st := "held"
				if ss.Lease == LeaseReleasing {
					st = "gone"
				}
				items = append(items, "lease-"+ss.LeaseMode.String()+"."+st)
			}
		}
		held, _ := s.localUse(v)
		fill := "room"
		if held >= s.cfg.LocalBlocks[v] {
			fill = "full"
		}
		out = append(out, set("local "+v.String()+" "+fill, items))
	}
	for v := VM(0); v < nVMs; v++ {
		d := &s.Drv[v]
		st := "up"
		switch {
		case d.Killed:
			st = "killed"
		case d.Restarted:
			st = "restarted"
		case d.Rebooted && s.Unsettled > 0:
			st = "rebooted"
		}
		out = append(out, fmt.Sprintf("drv %s %s F=%d", v, st, d.F))
	}
	if s.Fence != FenceAM {
		out = append(out, "fencing "+s.Fence.String())
	}
	for si := 0; si < nStacks; si++ {
		st := &s.Stacks[si]
		if st.N == 0 && !s.stackUsed(si) {
			continue
		}
		kinds := make([]string, 0, st.N)
		for _, k := range st.keys() {
			kinds = append(kinds, k.kind())
		}
		out = append(out, "stack "+stackName(si)+" "+strings.Join(kinds, ","))
	}
	for p := Pool(0); p < nPools; p++ {
		if !poolUsed[p] {
			continue
		}
		st := "up"
		if s.Ctrl[p].ResourceSlices != SlicesPresent {
			st = "up-" + s.Ctrl[p].ResourceSlices.String()
		}
		if !s.Ctrl[p].Up {
			st = "down"
			if s.Ctrl[p].ResourceSlices != SlicesPresent {
				st += "-slices-" + s.Ctrl[p].ResourceSlices.String()
			}
			for _, row := range s.Ctrl[p].Att {
				for _, t := range row {
					if t == TriMaybe {
						st = "down-maybe"
					}
				}
			}
		}
		out = append(out, "ctrl "+p.String()+" "+st)
	}
	return out
}

func uniq(sorted []string) []string {
	out := sorted[:0]
	for i, x := range sorted {
		if i == 0 || x != sorted[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// dump is the full state, for files and messages.
func (s *State) dump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "steps %d, unsettled %d, contested %s, fencing %s", s.Steps, s.Unsettled, s.contestedString(), s.Fence)
	if s.Switched {
		b.WriteString(" (switched in this window)")
	}
	for d := DevID(0); d < nDevs; d++ {
		if s.held(Res(d)) {
			fmt.Fprintf(&b, ", held %s since %d", d, s.HeldAt[d])
		}
	}
	b.WriteString("\n")
	for i := range s.Slots {
		ss := &s.Slots[i]
		if ss.Phase == Absent && ss.Gen == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %-14s %-10s", s.slot(i).Name, ss.Phase)
		if ss.Phase == Deleting {
			fmt.Fprintf(&b, " (was %s)", ss.Prev)
		}
		fmt.Fprintf(&b, " node %s gen %d restarts %d", ss.Node, ss.Gen, ss.Restarts)
		if ss.RaceKill {
			b.WriteString(" race-kill")
		}
		if ss.Steer != SteerNone {
			fmt.Fprintf(&b, " steer %s", ss.Steer)
		}
		if ss.Lease != LeaseNone {
			fmt.Fprintf(&b, " lease %s %s %q", ss.Lease, ss.LeaseMode, s.LeaseIDs[i])
		}
		if s.slot(i).Kind == KLDax && ss.Phase != Absent {
			fmt.Fprintf(&b, " data %s", s.SlotData[i])
		}
		if ss.Hold {
			b.WriteString(" hold")
		}
		if ss.Refuse {
			b.WriteString(" refuse")
		}
		if ss.Alloc {
			b.WriteString(" allocated")
		}
		if ss.Loose {
			b.WriteString(" loose")
		}
		if ss.CompLoose {
			b.WriteString(" companion-loose")
		}
		b.WriteString("\n")
	}
	for x := range s.PDev {
		if pd := &s.PDev[x]; s.cfg.devUsed(cPmem0 + DevID(x)) {
			fmt.Fprintf(&b, "  pdev %s: ns %s fs %s tok %s raw %s uuid %q\n", cPmem0+DevID(x), triName(pd.NS), triName(pd.FS), pd.Tok, pd.Raw, pd.UUID)
		}
	}
	for p := Pool(0); p < nPools; p++ {
		c := &s.Ctrl[p]
		fmt.Fprintf(&b, "  pool %s: controller up %v", p, c.Up)
		if c.ResourceSlices != SlicesPresent {
			fmt.Fprintf(&b, " slices %s", c.ResourceSlices)
		}
		if !c.Up {
			for d, dev := range poolDevs(p) {
				for v := VM(0); v < nVMs; v++ {
					if c.Att[d][v] != TriNo {
						fmt.Fprintf(&b, " %s@%s=%s", dev, v, triName(c.Att[d][v]))
					}
				}
			}
		}
		b.WriteString("\n")
	}
	for v := VM(0); v < nVMs; v++ {
		d := &s.Drv[v]
		fmt.Fprintf(&b, "  driver %s: pod gen %d restarts %d inc %d killed %v F %v maybe %v pod %q",
			v, d.PodGen, d.Restarts, d.Inc, d.Killed, s.fenceNUMANodes(d.F), s.fenceNUMANodes(d.FMaybe), d.Pod)
		if d.KillAt > 0 {
			fmt.Fprintf(&b, " kill-at %s", markerNames[d.KillAt-1])
		}
		if d.KillMay {
			b.WriteString(" may have fired")
		}
		if d.Slack > 0 {
			fmt.Fprintf(&b, " slack %d", d.Slack)
		}
		if d.Creating {
			b.WriteString(" creating")
		}
		if d.KillSlack {
			b.WriteString(" kubelet restarted after a possible kill")
		}
		if d.Rebooted {
			b.WriteString(" rebooted")
		}
		if d.Reboots > 0 || d.Boot != "" || d.BootBefore != "" {
			fmt.Fprintf(&b, " reboots %d boot %q", d.Reboots, d.Boot)
			if d.BootBefore != "" {
				fmt.Fprintf(&b, " not %q", d.BootBefore)
			}
		}
		if d.PodBefore != "" {
			fmt.Fprintf(&b, " not %q", d.PodBefore)
		}
		b.WriteString("\n")
	}
	for si := 0; si < nStacks; si++ {
		if s.Stacks[si].N > 0 {
			fmt.Fprintf(&b, "  stack %s %s", stackName(si), s.Stacks[si])
			if s.Reorder&(1<<uint(si)) != 0 {
				b.WriteString(" (order not known: rebuilt by a node-reboot)")
			}
			b.WriteString("\n")
		}
	}
	for _, e := range s.Ev {
		i, lease := s.userSlot(e.User)
		who := s.slot(i).Name
		switch {
		case lease && s.slot(i).Kind.dual():
			who = "local claim of " + who
		case lease:
			who = "lease of " + who
		}
		fmt.Fprintf(&b, "  event %d %s %s of %s on %d opt %v sync %v after %d\n", e.Seq, map[evKind]string{evPrepare: "prepare", evUnprepare: "unprepare"}[e.Kind], e.Key, who, e.Stack, e.Opt, e.Sync, e.After)
	}
	if len(s.Alts) > 0 {
		fmt.Fprintf(&b, "  %d other possible outcomes\n", len(s.Alts))
	}
	b.WriteString("  data:")
	for d := DevID(0); d < nDevs; d++ {
		fmt.Fprintf(&b, " %s=%s", d, s.Data[d])
	}
	b.WriteString("\n")
	return b.String()
}

// stackUsed tells if a stack has users in the profile.
func (s *State) stackUsed(si int) bool {
	v, b := stackVM(si), stackBridge(si)
	for i := range s.cfg.Slots {
		sl := s.slot(i)
		switch {
		case sl.Kind.dual() && b == HB0 && sl.VM == v:
			return true
		case sl.Kind.pooled():
			if poolBridge[sl.Pool] == b && (sl.VM == v || (sl.Any() && poolHasHost(sl.Pool, v))) {
				return true
			}
		case sl.Kind.local() || sl.Kind == KLease:
			if b == HB0 && sl.VM == v {
				return true
			}
		}
	}
	return false
}

func (s *State) contestedString() string {
	var rs []string
	for r := Res(0); r < nRes; r++ {
		if s.contested(r) {
			rs = append(rs, r.String())
		}
	}
	if len(rs) == 0 {
		return "-"
	}
	return strings.Join(rs, ",")
}

// claimNUMANodes are the CXL NUMA nodes that the claims and leases of the pod of
// slot i give its container (E of A38): the numa nodes of its claims and
// the node that a held numa lease lends it.
func (s *State) claimNUMANodes(i int) numaSet {
	sl, ss := s.slot(i), &s.Slots[i]
	e := numaSetOf(sl.numaNodes(s.cfg))
	if sl.Kind == KLease && ss.Lease == LeaseHeld && ss.LeaseMode == ModeNUMA {
		e |= numaBit(s.cfg.bridgeNUMANode(HB0))
	}
	return e
}

// memNUMANodes are the CXL NUMA nodes of VM v with memory (M of A38) in a settled
// state: the numa memory of its Running pods and of its held numa leases
// (the oracle's vmPred.memTotal).
func (s *State) memNUMANodes(v VM) numaSet {
	var m numaSet
	for i := range s.Slots {
		sl, ss := s.slot(i), &s.Slots[i]
		if sl.Kind == KLease && sl.VM == v && ss.Lease == LeaseHeld && ss.LeaseMode == ModeNUMA {
			m |= numaBit(s.cfg.bridgeNUMANode(HB0))
		}
		if ss.Phase == Running && ss.Node == v {
			m |= numaSetOf(sl.numaNodes(s.cfg))
		}
	}
	return m
}
