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

// The persistent devices of the simulated SUT (A22-A24). Unlike the pool
// side of volatile devices (refpool.go), the driver's persistent
// behaviour is answered from the model's truth: which Prepare is
// refused, the namespace, the node mounts, binds and loop devices, the
// content of the backing files. Only the open choices are the
// simulator's (a namespace that a writer may have made, its UUID).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	simPoolMounts = "/var/lib/kubelet/plugins/cxl-pool.nri.io/mounts"
	simLeaseFS    = "/var/lib/kubelet/plugins/cxl.nri.io/lease-fs"
)

// blockdev is the block device of a persistent device in every VM.
func simBlockdev(d DevID) string { return fmt.Sprintf("/dev/pmem%d", pmemIndex(d)) }

func simLoop(d DevID) string { return fmt.Sprintf("/dev/loop%d", pmemIndex(d)) }

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// fixPDev decides what the truth leaves open of the persistent devices:
// a namespace that a writer may have made, the UUID of a new one.
func (s *sim) fixPDev(n *State) {
	for i := range n.Slots {
		if ss, sl := &n.Slots[i], n.slot(i); sl.Kind.writer() && (ss.Phase == Running || ss.Phase == Restarting) {
			n.madeNamespace(sl.Dev)
		}
	}
	for x := range n.PDev {
		pd := &n.PDev[x]
		if pd.NS == TriMaybe {
			if s.rng.Intn(2) == 0 {
				pd.NS = TriYes
				pd.Raw = tokZero
			} else {
				pd.NS = TriNo
			}
		}
		if pd.NS == TriYes && pd.UUID == "" {
			s.uuidSeq++
			pd.UUID = fmt.Sprintf("5e2a%04d-0000-4000-8000-%012d", s.uuidSeq, s.uuidSeq)
		}
		if pd.NS != TriYes {
			pd.UUID = ""
		}
	}
}

// simPDev is the pdevs entry of a persistent device in the truth.
func simPDev(pd *PDev) *ObsPDev {
	ob := &ObsPDev{LSA: "zero", PFN: pd.NS == TriYes}
	ob.LSAHash = shaOf("lsa:" + pd.UUID)
	content := fmt.Sprintf("ns=%v fs=%v tok=%v raw=%v", pd.NS, pd.FS, pd.Tok, pd.Raw)
	ob.HeadHash = shaOf(content)
	if pd.NS == TriYes {
		ob.LSA = "labels"
		off := uint64(2 << 20)
		ob.DataOff = &off
		raw := pd.Raw.Text
		ob.Raw = &raw
		if pd.FS == TriYes {
			ob.Erofs = true
			tok := pd.Tok.Text
			ob.Token = &tok
		}
	}
	return ob
}

// refusedMessage is the Warning event of a refused persistent claim.
func refusedMessage(st *State, i int) string {
	sl := st.slot(i)
	pd := &st.PDev[pmemIndex(sl.Dev)]
	s := formatSerial(devices[sl.Dev].Serial)
	switch {
	case sl.Kind == KPBad:
		return "MemoryConfig: device.mode block: a shared device is " + refusedWritable + " (no shares request)"
	case pd.NS != TriYes:
		return "device " + s + ": " + refusedNoNS + " in region0: has a writer written the device?"
	}
	return "device " + s + ": " + simBlockdev(sl.Dev) + " " + refusedNoErofs + " (erofs, squashfs, ext4, xfs, btrfs or gfs2): set device.fsType"
}

// pmemPrepared tells per device and access which persistent claims and
// leases are prepared on a VM of the truth.
type simPmemVM struct {
	devs   map[DevID]bool
	mounts map[mountKey]bool
	binds  []simBind
	claims []int // slots of prepared persistent claims
}

type simBind struct {
	slot int
	mk   mountKey
	name string
	uid  string
}

func (s *sim) pmemVM(st *State, v VM) *simPmemVM {
	p := &simPmemVM{devs: map[DevID]bool{}, mounts: map[mountKey]bool{}}
	for i := range st.Slots {
		ss, sl := &st.Slots[i], st.slot(i)
		switch {
		case sl.Kind.persistent() && ss.Node == v && (ss.Phase == Running || ss.Phase == Restarting):
			p.devs[sl.Dev] = true
			p.claims = append(p.claims, i)
			if sl.Kind.fsKind() {
				p.mounts[mountKey{sl.Dev, sl.Kind == KPFsc}] = true
			}
		case sl.Kind == KPlse && sl.VM == v && (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) && ss.LeaseMode.persistent():
			p.devs[cPmem0] = true
			if ss.LeaseMode.fs() {
				mk := mountKey{cPmem0, ss.LeaseMode == ModePFC}
				p.mounts[mk] = true
				name := "mbt"
				if ss.LeaseMode == ModePFC {
					name = st.LeaseIDs[i]
				}
				p.binds = append(p.binds, simBind{slot: i, mk: mk, name: name, uid: claimUID(sl, ss.Gen)})
			}
		}
	}
	return p
}

func rawJSON(v any) json.RawMessage {
	js, _ := json.Marshal(v)
	return js
}

// observePmemVM fills the persistent parts of a VM's observation.
func (s *sim) observePmemVM(st *State, v VM, ov *ObsVM) {
	if !s.cfg.devUsed(cPmem0) && !s.cfg.devUsed(cPmem1) {
		return
	}
	p := s.pmemVM(st, v)
	var regions []ObsPmemRegion
	for d := cPmem0; d < nDevs; d++ {
		if !p.devs[d] {
			continue
		}
		size := devices[d].Size
		nd := fmt.Sprintf("region%d", 100+pmemIndex(d))
		r := ObsPmemRegion{Region: fmt.Sprintf("region%d", simRegionNo(st, v, int(d))), Serials: []string{formatSerial(devices[d].Serial)}, Size: &size, NdRegion: &nd, Namespaces: []ObsNamespace{}}
		if pd := &st.PDev[pmemIndex(d)]; pd.NS == TriYes {
			mode, mp, uuid, holder, sz := "memory", "ram", pd.UUID, fmt.Sprintf("pfn%d.0", pmemIndex(d)), nsSize
			bd := strings.TrimPrefix(simBlockdev(d), "/dev/")
			r.Namespaces = append(r.Namespaces, ObsNamespace{Name: fmt.Sprintf("namespace%d.0", 100+pmemIndex(d)), Mode: &mode, Size: &sz, UUID: &uuid, Holder: &holder, Map: &mp, Blockdev: &bd})
		}
		regions = append(regions, r)
		ov.Regions = append(ov.Regions, ObsRegion{Name: r.Region, Serials: r.Serials, Mode: "pmem", Size: size, Dax: []ObsDax{}})
	}
	ov.Pmem = rawJSON(nonNil(regions))
	var mounts []ObsMount
	var loops []ObsLoop
	var names []string
	for d := cPmem0; d < nDevs; d++ {
		for _, cached := range []bool{false, true} {
			mk := mountKey{d, cached}
			if !p.mounts[mk] {
				continue
			}
			m := ObsMount{Target: simPoolMounts + "/" + mk.name(), Source: simBlockdev(d), Fstype: "erofs", Root: "/",
				Superopts: "ro,dax=always", Vfsopts: "ro,nosuid,nodev,relatime", Count: 1}
			if cached {
				back, t := simBlockdev(d), true
				m.Source, m.Superopts, m.Backing, m.Dio, m.RO = simLoop(d), "ro", &back, &t, &t
				loops = append(loops, ObsLoop{Name: strings.TrimPrefix(simLoop(d), "/dev/"), Backing: &back, Dio: &t, Ro: &t})
			}
			mounts = append(mounts, m)
			names = append(names, mk.name())
		}
	}
	ov.FSMounts = rawJSON(nonNil(mounts))
	ov.Loops = rawJSON(nonNil(loops))
	var binds []ObsMount
	leaseFs := map[string][]string{}
	for _, b := range p.binds {
		src := simBlockdev(b.mk.dev)
		if b.mk.cached {
			src = simLoop(b.mk.dev)
		}
		binds = append(binds, ObsMount{Target: simLeaseFS + "/" + b.uid + "/" + b.name, Source: src, Fstype: "erofs", Root: "/",
			Superopts: "ro", Vfsopts: "ro,nosuid,nodev,relatime", Count: 1})
		leaseFs[b.uid] = append(leaseFs[b.uid], b.name)
	}
	ov.LeaseBinds = rawJSON(nonNil(binds))
	ov.LeaseDirs = rawJSON(ObsLeaseDirs{LeaseFS: leaseFs, Leases: []string{}, Mounts: nonNil(names)})
	claims := map[string]ObsPoolClaim{}
	cdi := map[string]ObsCDIDevice{}
	for _, i := range p.claims {
		sl, ss := st.slot(i), &st.Slots[i]
		uid := claimUID(sl, ss.Gen)
		serial, bd := formatSerial(devices[sl.Dev].Serial), simBlockdev(sl.Dev)
		claims[uid] = ObsPoolClaim{ClaimName: podName(sl, ss.Gen) + "-c0", Devices: []ObsPoolClaimDev{{Serial: &serial, BlockDevice: &bd}}}
		var dev ObsCDIDevice
		if sl.Kind.fsKind() {
			dev.Mounts = append(dev.Mounts, struct {
				HostPath      string   `json:"hostPath"`
				ContainerPath string   `json:"containerPath"`
				Options       []string `json:"options"`
			}{simPoolMounts + "/" + mountKey{sl.Dev, sl.Kind == KPFsc}.name(), fsPath(sl), []string{"bind", "ro", "nosuid", "nodev"}})
		} else {
			typ, perm := "b", "r"
			if sl.Kind.writer() {
				perm = "rw"
			}
			dev.DeviceNodes = append(dev.DeviceNodes, struct {
				Path        string  `json:"path"`
				Type        *string `json:"type"`
				Permissions *string `json:"permissions"`
			}{bd, &typ, &perm})
		}
		cdi["claim-"+uid+"-0"] = dev
	}
	ov.PoolClaims = rawJSON(claims)
	ov.CDIPool = rawJSON(cdi)
	ov.Kernel = rawJSON(ObsKernel{Lines: []string{}})
	ov.KillAt = json.RawMessage("null")
	ov.CtrMounts = json.RawMessage("[]")
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// pdevsJSON is the pdevs of the truth (the used persistent devices).
func (s *sim) pdevsJSON(st *State) json.RawMessage {
	if !s.cfg.devUsed(cPmem0) && !s.cfg.devUsed(cPmem1) {
		return nil
	}
	m := map[string]*ObsPDev{}
	for d := cPmem0; d < nDevs; d++ {
		if s.cfg.devUsed(d) {
			m[d.String()] = simPDev(&st.PDev[pmemIndex(d)])
		}
	}
	return rawJSON(m)
}

// inspectPmem adds the persistent parts of an inspection: the mount of
// an fs claim's PATH, the /dev/pmem* of the container, a plse
// requester's fs leases under /run/cxl/fs.
func (s *sim) inspectPmem(st *State, i int, in *ObsInspect) {
	sl, ss := st.slot(i), &st.Slots[i]
	in.PmemNodes = []string{}
	in.Mounts = []ObsMount{}
	switch {
	case sl.Kind.fsKind():
		in.Mount = &ObsMount{Target: fsPath(sl), Source: simBlockdev(sl.Dev), Fstype: "erofs", Root: "/", Vfsopts: "ro,nosuid,nodev", Superopts: "ro"}
	case sl.Kind.blockKind():
		in.PmemNodes = []string{simBlockdev(sl.Dev)}
	case sl.Kind == KPlse && ss.leaseFS():
		name := "mbt"
		if ss.LeaseMode == ModePFC {
			name = st.LeaseIDs[i]
		}
		in.Mounts = append(in.Mounts, ObsMount{Target: "/run/cxl/fs/" + name, Source: simBlockdev(cPmem0), Fstype: "erofs", Vfsopts: "ro,nosuid,nodev"})
	}
}

// doPmem is the done observation of a persistent verb (A23).
func (s *sim) doPmem(from, to *State, a Act, d *DoneObs) {
	i := int(a.Slot)
	switch a.Verb {
	case VPWrite:
		if !from.pwriteOK(i) {
			d.Status = 1
			d.Stderr = "write: EROFS: Read-only file system"
			return
		}
		txt := ""
		if a.Arg == 0 {
			txt = fmt.Sprintf("mbt-%d", to.Steps)
		}
		d.Text = &txt
	case VPRead:
		txt := from.preadToken(i).Text
		d.Text = &txt
	case VLeasePut:
		if to.Slots[i].Lease == LeaseBusy {
			d.Status = 3
			d.Stderr = "release: lease still Releasing after --wait"
		}
	}
}
