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

// The prediction of a step: the model state after it (the primary facts:
// slots, controllers, drivers) and, for steps that return an observation,
// what it must say. The oracle derives everything else (attachments,
// regions, slices, state files, fencing, env) from these facts after it
// has resolved what the model leaves open (the node of an "any" slot,
// the outcome of a controller kill) with the observation. The JSON form
// is what code.gen.sh embeds for `mbt-cxl check`.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Expectation is the prediction of one step.
type Expectation struct {
	Step        int           `json:"step"`
	Action      string        `json:"action"`
	Final       bool          `json:"final,omitempty"`
	Profile     string        `json:"profile"`
	Online      bool          `json:"online"`
	LocalBlocks [nVMs]int     `json:"localBlocks"`
	NUMANodeHB  [nBridges]int `json:"nodeHB"`
	// StallWindow (seconds; 0: the default) and StallStrict: A19's
	// window of stalled contenders, and whether settles wait for them
	StallWindow int  `json:"stallWindow,omitempty"`
	StallStrict bool `json:"stallStrict,omitempty"`
	// Fencing: the fencing mode of the drivers (A36, A37): "none", or
	// "" for allowed-mems
	Fencing string       `json:"fencing,omitempty"`
	Slots   []SlotJSON   `json:"slots"`
	Pools   []PoolJSON   `json:"pools"`
	Drivers []DriverJSON `json:"drivers"`
	Data    []string     `json:"data,omitempty"`
	PDevs   []PDevJSON   `json:"pdevs,omitempty"`
	// Stacks are the decoder stacks (A9): "vm0/hb0": ["local",
	// "b-pooled0", "hole", ...] bottom first.
	Stacks map[string][]string `json:"stacks,omitempty"`
	// Reorder are the stacks that a node-reboot rebuilt: their regions
	// in any order (A43)
	Reorder []string `json:"reorder,omitempty"`
	// Alts are the other outcomes that races for the decoder stacks
	// can have; the oracle takes the one that fits.
	Alts []*Expectation `json:"alts,omitempty"`
	// Truncated: the outcomes were not all enumerated (the outcome bound, A35)
	Truncated bool      `json:"truncated,omitempty"`
	Done      *DoneJSON `json:"done,omitempty"`
}

// SlotJSON is a slot that has or had a pod.
type SlotJSON struct {
	Slot      string `json:"slot"`
	Phase     string `json:"phase"`
	Prev      string `json:"prev,omitempty"`
	Node      string `json:"node,omitempty"` // "" while unknown
	Gen       int    `json:"gen"`
	Restarts  int    `json:"restarts,omitempty"`
	RaceKill  bool   `json:"raceKill,omitempty"`
	Steer     string `json:"steer,omitempty"`
	Lease     string `json:"lease,omitempty"` // held or releasing
	LeaseMode string `json:"leaseMode,omitempty"`
	LeaseID   string `json:"leaseID,omitempty"`
	SlotData  string `json:"data,omitempty"` // ldax: quoted content, "?" unknown
	UnblockAt int64  `json:"unblockAt,omitempty"`
	Slack     int    `json:"slack,omitempty"`
	Hold      bool   `json:"hold,omitempty"`      // A23: a file of its fs claim or lease is held
	Refuse    bool   `json:"refuse,omitempty"`    // A25: its Prepare is refused
	LeaseOpen bool   `json:"leaseOpen,omitempty"` // A29 d: a failed lease-get, the lease kept or released
	Loose     bool   `json:"loose,omitempty"`     // A37: its container existed at a switch to fencing none
	CompLoose bool   `json:"compLoose,omitempty"` // so did the companion of its lease
}

// PDevJSON is the content of a persistent device (A25).
type PDevJSON struct {
	Dev      string `json:"dev"`
	NS       string `json:"ns"`  // no, yes, maybe
	FS       string `json:"fs"`  // no, yes (the template erofs)
	Tok      string `json:"tok"` // hello.txt: quoted, "?" unknown
	Raw      string `json:"raw"` // bytes 0-63
	UUID     string `json:"uuid,omitempty"`
	LSAHash  string `json:"lsaHash,omitempty"`
	HeadHash string `json:"headHash,omitempty"`
}

// PoolJSON is a pool controller; Att is what is known of the attachments
// while it is down ("DEVICE@VM": "yes" or "maybe").
type PoolJSON struct {
	Pool   string            `json:"pool"`
	Up     bool              `json:"up"`
	Att    map[string]string `json:"att,omitempty"`
	Slices string            `json:"slices,omitempty"` // A18: absent, unknown; "" present
}

// DriverJSON is the driver of a VM.
type DriverJSON struct {
	VM        string `json:"vm"`
	PodGen    int    `json:"podGen"`
	Restarts  int    `json:"restarts"`
	Inc       int    `json:"inc"`
	Pod       string `json:"pod,omitempty"`
	PodBefore string `json:"podBefore,omitempty"`
	F         []int  `json:"fenced,omitempty"`
	FMaybe    []int  `json:"fencedMaybe,omitempty"`
	KillAt    string `json:"killAt,omitempty"` // A23: the marker of an armed watcher
	KillAtAt  int64  `json:"killAtAt,omitempty"`
	KillMay   bool   `json:"killAtMay,omitempty"` // it may have fired in the window (A32)
	Slack     int    `json:"slack,omitempty"`     // restarts it may have more (A34)
	// node-reboot (A42-A44): in its window or the window its settle
	// ended, the reboots, the boot id learnt and the one that must be
	// gone
	Rebooted   bool   `json:"rebooted,omitempty"`
	Reboots    int    `json:"reboots,omitempty"`
	Boot       string `json:"boot,omitempty"`
	BootBefore string `json:"bootBefore,omitempty"`
}

// DoneJSON is what a non-settle step must return.
type DoneJSON struct {
	Verb   string  `json:"verb"`
	Status string  `json:"status"`          // "zero", "nonzero" or "either"
	Text   *string `json:"text,omitempty"`  // read: the string; nil: anything
	Lease  bool    `json:"lease,omitempty"` // lease-get: a lease ID
	// fencing-switch: the mode switched to (A37: done {"fencing": MODE})
	Fencing string `json:"fencing,omitempty"`
	// read: zeros ("") are right too (A43: a dax device after a node
	// reboot, until the next write)
	OrZero bool `json:"orZero,omitempty"`
	// node-reboot: the boot id that the new one must differ from (A44:
	// done {"boot": ID})
	BootNot string `json:"bootNot,omitempty"`
}

// tokenJSON is a token in an expectation: "?" unknown, the quoted text,
// and "|zero" after it when zeros are right too.
func tokenJSON(t Token) string {
	if !t.Known {
		return "?"
	}
	b, _ := json.Marshal(t.Text)
	if t.OrZero {
		return string(b) + orZeroSuffix
	}
	return string(b)
}

const orZeroSuffix = "|zero"

func parseTokenJSON(s string) (Token, error) {
	if s == "?" || s == "" {
		return tokUnknown, nil
	}
	q, orZero := strings.CutSuffix(s, orZeroSuffix)
	var t string
	if err := json.Unmarshal([]byte(q), &t); err != nil {
		return tokUnknown, fmt.Errorf("invalid data %q", s)
	}
	return Token{Known: true, Text: t, OrZero: orZero && t != ""}, nil
}

// expect returns the prediction of the step that led to s.
func (s *State) expect(step int, action string) *Expectation {
	e := &Expectation{
		Step:        step,
		Action:      action,
		Profile:     s.cfg.Profile,
		Online:      s.cfg.Online,
		LocalBlocks: s.cfg.LocalBlocks,
		NUMANodeHB:  s.cfg.NUMANodeHB,
		StallStrict: s.cfg.StallStrict,
	}
	if s.Fence != FenceAM {
		e.Fencing = s.Fence.String()
	}
	if w := s.cfg.StallWindow; w > 0 && w != defaultStallWindow {
		e.StallWindow = int(w / time.Second)
	}
	for i := range s.Slots {
		ss := &s.Slots[i]
		sl := s.slot(i)
		if ss.Phase == Absent && ss.Gen == 0 {
			continue
		}
		j := SlotJSON{
			Slot:      sl.Name,
			Phase:     ss.Phase.String(),
			Gen:       int(ss.Gen),
			Restarts:  int(ss.Restarts),
			RaceKill:  ss.RaceKill,
			Steer:     ss.Steer.String(),
			LeaseID:   s.LeaseIDs[i],
			UnblockAt: s.unblockAt(i),
			Slack:     int(ss.Slack),
			Hold:      ss.Hold,
			Refuse:    ss.Refuse,
			LeaseOpen: ss.LeaseOpen,
			Loose:     ss.Loose,
			CompLoose: ss.CompLoose,
		}
		if ss.Node != VMAny {
			j.Node = ss.Node.String()
		}
		if ss.Phase == Deleting {
			j.Prev = ss.Prev.String()
		}
		switch ss.Lease {
		case LeaseHeld:
			j.Lease, j.LeaseMode = "held", ss.LeaseMode.String()
		case LeaseReleasing:
			j.Lease, j.LeaseMode = "releasing", ss.LeaseMode.String()
		case LeaseBusy:
			j.Lease, j.LeaseMode = "busy", ss.LeaseMode.String()
		}
		if sl.Kind == KLDax && ss.Phase != Absent {
			j.SlotData = tokenJSON(s.SlotData[i])
		}
		e.Slots = append(e.Slots, j)
	}
	for p := Pool(0); p < nPools; p++ {
		c := &s.Ctrl[p]
		pj := PoolJSON{Pool: p.String(), Up: c.Up}
		if c.ResourceSlices != SlicesPresent {
			pj.Slices = c.ResourceSlices.String()
		}
		if !c.Up {
			pj.Att = map[string]string{}
			for d, dev := range poolDevs(p) {
				for v := VM(0); v < nVMs; v++ {
					switch c.Att[d][v] {
					case TriYes:
						pj.Att[devices[dev].Name+"@"+v.String()] = "yes"
					case TriMaybe:
						pj.Att[devices[dev].Name+"@"+v.String()] = "maybe"
					}
				}
			}
		}
		e.Pools = append(e.Pools, pj)
	}
	for v := VM(0); v < nVMs; v++ {
		d := &s.Drv[v]
		e.Drivers = append(e.Drivers, DriverJSON{
			VM:         v.String(),
			PodGen:     int(d.PodGen),
			Restarts:   int(d.Restarts),
			Inc:        int(d.Inc),
			Pod:        d.Pod,
			PodBefore:  d.PodBefore,
			F:          s.fenceNUMANodes(d.F),
			FMaybe:     s.fenceNUMANodes(d.FMaybe),
			KillAt:     killAtName(d.KillAt),
			KillAtAt:   d.KillAtAt,
			KillMay:    d.KillMay,
			Slack:      int(d.Slack),
			Rebooted:   d.Rebooted,
			Reboots:    int(d.Reboots),
			Boot:       d.Boot,
			BootBefore: d.BootBefore,
		})
	}
	for d := DevID(0); d < nDevs; d++ {
		e.Data = append(e.Data, tokenJSON(s.Data[d]))
	}
	for x := range s.PDev {
		d := cPmem0 + DevID(x)
		if !s.cfg.devUsed(d) {
			continue
		}
		pd := &s.PDev[x]
		e.PDevs = append(e.PDevs, PDevJSON{Dev: d.String(), NS: triName(pd.NS), FS: triName(pd.FS), Tok: tokenJSON(pd.Tok),
			Raw: tokenJSON(pd.Raw), UUID: pd.UUID, LSAHash: pd.LSAHash, HeadHash: pd.HeadHash})
	}
	for si := 0; si < nStacks; si++ {
		st := &s.Stacks[si]
		if st.N == 0 {
			continue
		}
		if e.Stacks == nil {
			e.Stacks = map[string][]string{}
		}
		var keys []string
		for _, k := range st.keys() {
			keys = append(keys, k.String())
		}
		e.Stacks[stackName(si)] = keys
		if s.Reorder&(1<<uint(si)) != 0 {
			e.Reorder = append(e.Reorder, stackName(si))
		}
	}
	for _, a := range s.Alts {
		e.Alts = append(e.Alts, a.expect(step, action))
	}
	e.Truncated = s.Truncated
	return e
}

// parseRKey parses a key of an expectation's stack.
func parseRKey(s string) (RKey, bool) {
	switch s {
	case "hole":
		return rkHole, true
	case "local":
		return rkLocal, true
	}
	for d := DevID(0); d < nDevs; d++ {
		if devices[d].Name == s {
			return devKey(d), true
		}
	}
	return 0, false
}

// expectDone returns the prediction of a step that returns a done line:
// the state before the step is from, after it to.
func expectDone(from, to *State, a Act, step int) *Expectation {
	e := to.expect(step, a.name(to.cfg))
	d := &DoneJSON{Verb: a.Verb.String(), Status: "zero"}
	switch a.Verb {
	case VWrite:
		if to.slot(int(a.Slot)).Kind == KSRo {
			d.Status = "nonzero"
		}
	case VRead:
		if t := from.token(int(a.Slot)); t.Known {
			txt := t.Text
			d.Text, d.OrZero = &txt, t.OrZero
		}
	case VLeaseGet:
		d.Lease = true
		if from.killAtBind(int(a.Slot), a.Mode) {
			// 0 with the lease, or nonzero with it Failed (A29 d)
			d.Status = "either"
		}
	case VPWrite:
		// a read-only claim or fd refuses it (A23, P11); img returns
		// its token, junk ""
		if !from.pwriteOK(int(a.Slot)) {
			d.Status = "nonzero"
		} else {
			txt := ""
			if a.Arg == 0 {
				txt = fmt.Sprintf("mbt-%d", to.Steps)
			}
			d.Text = &txt
		}
	case VPRead:
		if t := from.preadToken(int(a.Slot)); t.Known {
			txt := t.Text
			d.Text = &txt
		}
	case VLeasePut:
		// a held fs lease stays Releasing: cxl-request release --wait
		// ends with exit 3 (A25, the executor's MBT_LEASE_PUT_WAIT)
		if to.Slots[a.Slot].Lease == LeaseBusy {
			d.Status = "nonzero"
		}
	case VFencingSwitch:
		d.Fencing = to.Fence.String()
	case VNodeReboot:
		d.BootNot = to.Drv[a.VM].BootBefore
	}
	e.Done = d
	return e
}

// token is the content of the dax device of a slot.
func (s *State) token(i int) Token {
	sl := s.slot(i)
	if sl.Kind == KLDax {
		return s.SlotData[i]
	}
	return s.Data[sl.Dev]
}

// configFor returns the configuration of an expectation.
func (e *Expectation) config() (*Config, error) {
	cfg := defaultConfig()
	cfg.Online = e.Online
	if err := cfg.loadProfile(e.Profile); err != nil {
		return nil, err
	}
	cfg.LocalBlocks = e.LocalBlocks
	cfg.NUMANodeHB = e.NUMANodeHB
	if e.StallWindow > 0 {
		cfg.StallWindow = time.Duration(e.StallWindow) * time.Second
	}
	cfg.StallStrict = e.StallStrict
	return cfg, nil
}

// state reconstructs the model state of an expectation.
func (e *Expectation) state() (*State, error) {
	cfg, err := e.config()
	if err != nil {
		return nil, err
	}
	s := newState(cfg)
	s.Steps = int32(e.Step)
	switch e.Fencing {
	case "", FenceAM.String():
	case FenceNone.String():
		s.Fence = FenceNone
	default:
		return nil, fmt.Errorf("expectation: invalid fencing %q", e.Fencing)
	}
	for _, j := range e.Slots {
		i := cfg.slotIndex(j.Slot)
		if i < 0 {
			return nil, fmt.Errorf("expectation: slot %q is not in profile %s", j.Slot, e.Profile)
		}
		ss := &s.Slots[i]
		ph, err := parsePhase(j.Phase)
		if err != nil {
			return nil, fmt.Errorf("expectation: slot %s: %w", j.Slot, err)
		}
		ss.Phase = ph
		if j.Prev != "" {
			if ss.Prev, err = parsePhase(j.Prev); err != nil {
				return nil, fmt.Errorf("expectation: slot %s: %w", j.Slot, err)
			}
		}
		ss.Node = VMAny
		if j.Node != "" {
			if ss.Node, err = parseVM(j.Node); err != nil {
				return nil, fmt.Errorf("expectation: slot %s: %w", j.Slot, err)
			}
		} else if !cfg.Slots[i].Any() {
			ss.Node = cfg.Slots[i].VM
		}
		ss.Gen = uint16(j.Gen)
		ss.Restarts = uint8(j.Restarts)
		ss.RaceKill = j.RaceKill
		for st := SteerNone; st <= SteerAny; st++ {
			if st.String() == j.Steer {
				ss.Steer = st
			}
		}
		switch j.Lease {
		case "held":
			ss.Lease = LeaseHeld
		case "releasing":
			ss.Lease = LeaseReleasing
		case "busy":
			ss.Lease = LeaseBusy
		}
		if m, ok := parseLeaseMode(j.LeaseMode); ok {
			ss.LeaseMode = m
		}
		ss.Hold, ss.Refuse, ss.LeaseOpen = j.Hold, j.Refuse, j.LeaseOpen
		ss.Loose, ss.CompLoose = j.Loose, j.CompLoose
		s.LeaseIDs[i] = j.LeaseID
		s.setUnblockAt(i, j.UnblockAt)
		ss.Slack = uint8(j.Slack)
		if j.SlotData != "" {
			t, err := parseTokenJSON(j.SlotData)
			if err != nil {
				return nil, fmt.Errorf("expectation: slot %s: %w", j.Slot, err)
			}
			s.SlotData[i] = t
		}
	}
	for _, pj := range e.Pools {
		p, err := parsePool(pj.Pool)
		if err != nil {
			return nil, fmt.Errorf("expectation: %w", err)
		}
		c := &s.Ctrl[p]
		c.Up = pj.Up
		for st := SlicesPresent; st <= SlicesUnknown; st++ {
			if pj.Slices == st.String() {
				c.ResourceSlices = st
			}
		}
		for k, v := range pj.Att {
			d, vm, err := parseDevAt(k)
			if err != nil || devices[d].Pool != p {
				return nil, fmt.Errorf("expectation: pool %s: invalid attachment %q", pj.Pool, k)
			}
			t := TriYes
			if v == "maybe" {
				t = TriMaybe
			}
			c.Att[devIndex(d)][vm] = t
		}
	}
	for _, dj := range e.Drivers {
		v, err := parseVM(dj.VM)
		if err != nil || v == VMAny {
			return nil, fmt.Errorf("expectation: driver %q: invalid VM", dj.VM)
		}
		d := &s.Drv[v]
		d.PodGen, d.Restarts, d.Inc = uint16(dj.PodGen), uint8(dj.Restarts), uint16(dj.Inc)
		d.Pod, d.PodBefore = dj.Pod, dj.PodBefore
		for _, n := range dj.F {
			d.F |= s.fenceBit(n)
		}
		for _, n := range dj.FMaybe {
			d.FMaybe |= s.fenceBit(n)
		}
		if mk, ok := parseMarker(dj.KillAt); ok {
			d.KillAt = uint8(mk) + 1
		}
		d.KillAtAt, d.KillMay = dj.KillAtAt, dj.KillMay
		d.Slack = uint8(dj.Slack)
		d.Rebooted, d.Reboots, d.Boot, d.BootBefore = dj.Rebooted, uint8(dj.Reboots), dj.Boot, dj.BootBefore
	}
	for _, pj := range e.PDevs {
		x := -1
		for d := cPmem0; d < nDevs; d++ {
			if d.String() == pj.Dev {
				x = pmemIndex(d)
			}
		}
		if x < 0 {
			return nil, fmt.Errorf("expectation: invalid persistent device %q", pj.Dev)
		}
		pd := &s.PDev[x]
		var err error
		pd.NS, pd.FS = parseTri(pj.NS), parseTri(pj.FS)
		if pd.Tok, err = parseTokenJSON(pj.Tok); err != nil {
			return nil, fmt.Errorf("expectation: %w", err)
		}
		if pd.Raw, err = parseTokenJSON(pj.Raw); err != nil {
			return nil, fmt.Errorf("expectation: %w", err)
		}
		pd.UUID, pd.LSAHash, pd.HeadHash = pj.UUID, pj.LSAHash, pj.HeadHash
	}
	for d := 0; d < len(e.Data) && d < nDevs; d++ {
		t, err := parseTokenJSON(e.Data[d])
		if err != nil {
			return nil, fmt.Errorf("expectation: %w", err)
		}
		s.Data[d] = t
	}
	for name, keys := range e.Stacks {
		si := -1
		for x := 0; x < nStacks; x++ {
			if stackName(x) == name {
				si = x
			}
		}
		if si < 0 {
			return nil, fmt.Errorf("expectation: invalid stack %q", name)
		}
		for _, kn := range keys {
			k, ok := parseRKey(kn)
			if !ok {
				return nil, fmt.Errorf("expectation: stack %s: invalid region %q", name, kn)
			}
			s.Stacks[si].push(k)
		}
	}
	for _, name := range e.Reorder {
		for si := 0; si < nStacks; si++ {
			if stackName(si) == name {
				s.Reorder |= 1 << uint(si)
			}
		}
	}
	for p := Pool(0); p < nPools; p++ {
		if s.Ctrl[p].Up {
			s.deriveAtt(p)
		}
	}
	s.Truncated = e.Truncated
	for _, ae := range e.Alts {
		a, err := ae.state()
		if err != nil {
			return nil, err
		}
		s.Alts = append(s.Alts, a)
	}
	return s, nil
}

func killAtName(k uint8) string {
	if k == 0 {
		return ""
	}
	return markerNames[k-1]
}

func parseTri(s string) Tri {
	switch s {
	case "yes":
		return TriYes
	case "maybe":
		return TriMaybe
	}
	return TriNo
}

// parseDevAt parses DEVICE@VM.
func parseDevAt(s string) (DevID, VM, error) {
	for d := DevID(0); d < nDevs; d++ {
		prefix := devices[d].Name + "@"
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			vm, err := parseVM(s[len(prefix):])
			if err != nil || vm == VMAny {
				return 0, 0, fmt.Errorf("invalid VM in %q", s)
			}
			return d, vm, nil
		}
	}
	return 0, 0, fmt.Errorf("invalid DEVICE@VM %q", s)
}

// sortedKeys returns the keys of a map in order.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
