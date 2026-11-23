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

// The oracle of persistent devices (A22-A26): the Refused phase, the
// adoption of what the model leaves open (a namespace that a writer may
// have made, its UUID, the hashes of the backing files, an armed
// driver-kill-at watcher), and the invariants P1-P11 of
// A26 against the observation fields of A24.
// A field that is absent is not checked (noted once per run), a null one
// is a wait (A11).

import (
	"path"
	"sort"
	"strconv"
	"strings"
)

// killAtDisarm: an armed driver-kill-at watcher gives up after 60 s
// (MBT_KILLAT_TIMEOUT): the model disarms it this long after the settle
// that first saw it armed.
const killAtDisarm = 70

// refusal texts of the driver (pkg/cxl/config/device.go,
// D/pool_persistent.go, the filesystem probe) per cause; refusedNoErofs:
// no signature of a known filesystem type
const (
	refusedWritable = "writable only exclusively"
	refusedNoNS     = "no fsdax namespace"
	refusedNoErofs  = "has no filesystem of a type that the driver knows"
)

// pdevs decodes the observation's pdevs (nil, false: not observed).
func (k *checker) pdevs() (map[string]*ObsPDev, bool) {
	var m map[string]*ObsPDev
	ok, absent, err := rawPart(k.o.PDevs, &m)
	switch {
	case absent:
		k.notef("pdevs not observed: no check of the persistent devices' backing files")
	case err != nil:
		k.waitf("pdevs: %v", err)
	case !ok:
		k.waitf("pdevs not observed (null)")
	}
	return m, ok
}

// pmemUsed: the profile has persistent devices.
func (k *checker) pmemUsed() bool { return k.cfg.devUsed(cPmem0) || k.cfg.devUsed(cPmem1) }

// resolvePersistent adopts what the model leaves open of the persistent
// devices: whether a writer made the namespace (pdevs' LSA), its UUID
// (the namespaces of the VMs), the hashes of the backing files, and the
// end of an armed driver-kill-at watcher.
func (k *checker) resolvePersistent() {
	rs := k.rs
	for v := VM(0); v < nVMs; v++ {
		d := &rs.Drv[v]
		if d.KillAt == 0 {
			continue
		}
		// the watcher's state on the observe line (A30): done without
		// firing is final
		if ov := k.o.VMs[v.String()]; ov != nil {
			var st struct {
				Fired bool `json:"fired"`
				Done  bool `json:"done"`
			}
			if ok, _, _ := rawPart(ov.KillAt, &st); ok && st.Done && !st.Fired {
				d.KillAt, d.KillAtAt = 0, 0
				continue
			}
		}
		if k.now.IsZero() {
			continue
		}
		switch {
		case d.KillAtAt == 0:
			d.KillAtAt = k.now.Unix()
		case k.now.Unix()-d.KillAtAt > killAtDisarm:
			d.KillAt, d.KillAtAt = 0, 0
		}
	}
	if !k.pmemUsed() {
		return
	}
	var pdevs map[string]*ObsPDev
	if ok, _, _ := rawPart(k.o.PDevs, &pdevs); !ok {
		pdevs = nil
	}
	for x := range rs.PDev {
		d := cPmem0 + DevID(x)
		if !k.cfg.devUsed(d) {
			continue
		}
		pd := &rs.PDev[x]
		if ob := pdevs[d.String()]; ob != nil {
			if pd.NS == TriMaybe {
				switch ob.LSA {
				case "labels":
					pd.NS = TriYes
				case "zero":
					pd.NS = TriNo
				}
			}
			if pd.LSAHash == "" {
				pd.LSAHash = ob.LSAHash
			}
			if pd.HeadHash == "" {
				pd.HeadHash = ob.HeadHash
			}
		}
		if pd.NS == TriYes && pd.UUID == "" {
			pd.UUID = k.observedUUID(d)
		}
	}
}

// observedUUID is the UUID of the fsdax namespace of a device in any VM.
func (k *checker) observedUUID(d DevID) string {
	for v := VM(0); v < nVMs; v++ {
		ov := k.o.VMs[v.String()]
		if ov == nil {
			continue
		}
		var regions []ObsPmemRegion
		if ok, _, _ := rawPart(ov.Pmem, &regions); !ok {
			continue
		}
		for _, r := range regions {
			if !hasSerial(r.Serials, devices[d].Serial) {
				continue
			}
			for _, ns := range r.Namespaces {
				if ns.UUID != nil && *ns.UUID != "" {
					return *ns.UUID
				}
			}
		}
	}
	return ""
}

func hasSerial(serials []string, serial uint64) bool {
	for _, s := range serials {
		if x, ok := parseSerial(s); ok && x == serial {
			return true
		}
	}
	return false
}

// refusalText is the text of the driver's refusal of a persistent claim.
func (k *checker) refusalText(i int) string {
	sl := k.rs.slot(i)
	pd := &k.rs.PDev[pmemIndex(sl.Dev)]
	switch {
	case sl.Kind == KPBad:
		return refusedWritable
	case pd.NS != TriYes:
		return refusedNoNS
	}
	return refusedNoErofs
}

// checkRefused: a persistent claim whose Prepare the device or its config
// refuses (A25): Pending on its node, its claim allocated and attached,
// a Warning event with the refusal.
func (k *checker) checkRefused(i int, name string) {
	ss := &k.rs.Slots[i]
	sl := k.rs.slot(i)
	p := k.cur[i]
	why := k.refusalText(i)
	switch {
	case p == nil:
		k.waitf("%s: no pod, want its Prepare refused (%s)", name, why)
	case p.Deleting:
		k.failf("%s: being deleted, want refused", name)
	case p.Phase == "Running":
		k.failf("%s: Running, but its Prepare must be refused (%s)", name, why)
	case p.Phase != "Pending":
		k.waitf("%s: phase %s, want Pending (refused: %s)", name, p.Phase, why)
	case p.Node != nil && ss.Node != VMAny && *p.Node != ss.Node.String():
		k.failf("%s: on %s, want %s", name, *p.Node, ss.Node)
	case p.Node == nil:
		k.waitf("%s: not bound yet, want refused on %s", name, ss.Node)
	case p.Claim == nil || !p.Claim.Allocated:
		k.waitf("%s: claim not allocated, want refused in its Prepare", name)
	case k.rs.Ctrl[sl.Pool].Up && (p.Claim.Attached == nil || !*p.Claim.Attached):
		k.waitf("%s: claim not Attached, want refused in its Prepare", name)
	case !p.hasEvent(why):
		msg := "none"
		if p.Event != nil {
			msg = firstLine(p.Event.Reason + ": " + p.Event.Message)
		}
		k.waitf("%s: no Warning event with %q (newest %q)", name, why, msg)
	}
}

// hasEvent: a Warning event of the pod contains the text.
func (p *ObsPod) hasEvent(text string) bool {
	if p.Event != nil && strings.Contains(p.Event.Message, text) {
		return true
	}
	for _, e := range p.Events {
		if strings.Contains(e.Message, text) {
			return true
		}
	}
	return false
}

// pmemUser is a prepared persistent claim or lease of a VM.
type pmemUser struct {
	slot   int
	dev    DevID
	kind   Kind // the claim's kind; a lease: its companion's
	lease  bool
	either bool // refused or retrying: its region (and mount) may be there or not
}

// pmemUsers returns the persistent claims and leases prepared on a VM.
func (k *checker) pmemUsers(v VM) []pmemUser {
	var out []pmemUser
	for i := range k.rs.Slots {
		ss, sl := &k.rs.Slots[i], k.rs.slot(i)
		switch {
		case sl.Kind.persistent() && ss.Node == v:
			switch ss.Phase {
			case Running, Restarting:
				out = append(out, pmemUser{slot: i, dev: sl.Dev, kind: sl.Kind})
			case Refused, Unblocking:
				if sl.Kind != KPBad {
					out = append(out, pmemUser{slot: i, dev: sl.Dev, kind: sl.Kind, either: true})
				}
			}
		case sl.Kind == KPlse && sl.VM == v && (ss.Lease == LeaseHeld || ss.Lease == LeaseBusy) && ss.LeaseMode.persistent():
			out = append(out, pmemUser{slot: i, dev: cPmem0, kind: ss.LeaseMode.kind(), lease: true})
		}
	}
	return out
}

// mountKey is a node mount: the device and its access.
type mountKey struct {
	dev    DevID
	cached bool
}

func (m mountKey) name() string {
	access := "direct"
	if m.cached {
		access = "cached"
	}
	return strconv.FormatUint(devices[m.dev].Serial, 16) + "-" + access
}

// mountKeyOf parses the basename of a node mount (c1ae2002-direct).
func mountKeyOf(target string) (mountKey, bool) {
	base := path.Base(target)
	hex, access, ok := strings.Cut(base, "-")
	if !ok || (access != "direct" && access != "cached") {
		return mountKey{}, false
	}
	serial, err := strconv.ParseUint(hex, 16, 64)
	if err != nil {
		return mountKey{}, false
	}
	d := devBySerial(serial)
	if d < 0 {
		return mountKey{}, false
	}
	return mountKey{dev: d, cached: access == "cached"}, true
}

func hasOpts(opts string, want ...string) bool {
	have := map[string]bool{}
	for _, o := range strings.Split(opts, ",") {
		have[o] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// checkPersistentVM checks the persistent parts of a VM (A24, A26 P1-P4,
// P7, P8, P10).
func (k *checker) checkPersistentVM(v VM, ov *ObsVM) {
	if !k.pmemUsed() {
		return
	}
	users := k.pmemUsers(v)
	want := map[mountKey]bool{}   // node mounts that must be there
	either := map[mountKey]bool{} // that may be there
	for _, u := range users {
		if !u.kind.fsKind() {
			continue
		}
		mk := mountKey{u.dev, u.kind == KPFsc}
		if u.either {
			either[mk] = true
		} else {
			want[mk] = true
		}
	}
	sources := k.checkFSMounts(v, ov, want, either)
	k.checkLeaseBinds(v, ov, sources)
	k.checkLoops(v, ov, want, either)
	blockdevs := k.checkPmemRegions(v, ov, users)
	k.checkLeaseDirs(v, ov, want, either)
	k.checkPoolClaims(v, ov, blockdevs)
	k.checkCDIPool(v, ov, blockdevs)
	k.checkKernel(v, ov)
	k.checkKillAt(v, ov)
	if dl := ov.DriverLog; dl != nil && dl.NotRemounted != nil && *dl.NotRemounted > 0 {
		k.notef("%s: the driver logged %d times that it did not mount or bind again (A24)", v, *dl.NotRemounted)
	}
}

// part decodes a raw VM part, noting an absent and waiting for a null
// one.
func (k *checker) part(v VM, name string, raw []byte, into any) bool {
	ok, absent, err := rawPart(raw, into)
	switch {
	case absent:
		k.notef("vms.%s.%s not observed: not checked", v, name)
	case err != nil:
		k.waitf("%s: %s: %v", v, name, err)
	case !ok:
		k.waitf("%s: %s not observed (null)", v, name)
	}
	return ok
}

// checkFSMounts (P1): the node mounts of a VM are one per device and
// access of its prepared fs claims and leases, direct an erofs with
// dax=always on the namespace's block device, cached an erofs on a
// read-only direct-I/O loop device over it, ro,nosuid,nodev; nothing
// else under mounts/. It returns the source of each node mount.
func (k *checker) checkFSMounts(v VM, ov *ObsVM, want, either map[mountKey]bool) map[mountKey]string {
	var mounts []ObsMount
	if !k.part(v, "fsMounts", ov.FSMounts, &mounts) {
		return nil
	}
	sources := map[mountKey]string{}
	seen := map[mountKey]bool{}
	for _, m := range mounts {
		mk, ok := mountKeyOf(m.Target)
		switch {
		case !ok:
			k.waitf("%s: node mount %s of no persistent device", v, m.Target)
			continue
		case !want[mk] && !either[mk]:
			k.waitf("%s: node mount %s, no prepared claim or lease uses it (stale node mount)", v, m.Target)
			continue
		}
		seen[mk] = true
		sources[mk] = m.Source
		if m.Fstype != "erofs" {
			k.waitf("%s: node mount %s: %s, want erofs", v, m.Target, m.Fstype)
		}
		if !hasOpts(m.Vfsopts, "ro", "nosuid", "nodev") {
			k.waitf("%s: node mount %s: options %s, want ro,nosuid,nodev", v, m.Target, m.Vfsopts)
		}
		if !mk.cached {
			if !strings.HasPrefix(m.Source, "/dev/pmem") || !strings.Contains(m.Superopts, "dax=always") {
				k.waitf("%s: node mount %s: %s (%s), want /dev/pmem* with dax=always", v, m.Target, m.Source, m.Superopts)
			}
			continue
		}
		switch {
		case !strings.HasPrefix(m.Source, "/dev/loop") || m.Backing == nil || !strings.HasPrefix(*m.Backing, "/dev/pmem"):
			k.waitf("%s: node mount %s: %s, want a loop device over /dev/pmem*", v, m.Target, m.Source)
		case m.Dio == nil || !*m.Dio || m.RO == nil || !*m.RO:
			k.waitf("%s: node mount %s: loop device %s not read-only with direct I/O", v, m.Target, m.Source)
		case strings.Contains(m.Superopts, "dax=always"):
			k.waitf("%s: node mount %s: cached with dax=always", v, m.Target)
		}
	}
	for _, mk := range sortedMountKeys(want) {
		if !seen[mk] {
			k.waitf("%s: no node mount %s, want one for its prepared fs claims", v, mk.name())
		}
	}
	return sources
}

func sortedMountKeys(m map[mountKey]bool) []mountKey {
	var out []mountKey
	for mk, on := range m {
		if on {
			out = append(out, mk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name() < out[j].name() })
	return out
}

// checkLeaseBinds (P2): one bind under lease-fs/<lease claim uid>/<name>
// per Ready or busy fs lease of the VM, of the node mount's source,
// ro,nosuid,nodev; no other mount there.
func (k *checker) checkLeaseBinds(v VM, ov *ObsVM, sources map[mountKey]string) {
	var binds []ObsMount
	if !k.part(v, "leaseBinds", ov.LeaseBinds, &binds) {
		return
	}
	type bw struct {
		slot  int
		uid   string
		name  string // "" any
		mk    mountKey
		found bool
	}
	var wants []*bw
	for i := range k.rs.Slots {
		ss, sl := &k.rs.Slots[i], k.rs.slot(i)
		if sl.Kind != KPlse || sl.VM != v || !ss.leaseFS() {
			continue
		}
		w := &bw{slot: i, mk: mountKey{cPmem0, ss.LeaseMode == ModePFC}}
		if p := k.cur[i]; p != nil && p.Claim != nil {
			w.uid = p.Claim.UID
		}
		w.name = k.rs.LeaseIDs[i]
		if ss.LeaseMode == ModePFD {
			w.name = "mbt"
		}
		wants = append(wants, w)
	}
	for _, b := range binds {
		uid, name := path.Base(path.Dir(b.Target)), path.Base(b.Target)
		var match *bw
		for _, w := range wants {
			if !w.found && (w.uid == "" || w.uid == uid) && (w.name == "" || w.name == name) {
				match = w
				break
			}
		}
		if match == nil {
			k.waitf("%s: lease bind %s, no Ready fs lease has it (bind left)", v, b.Target)
			continue
		}
		match.found = true
		if b.Fstype != "erofs" || !hasOpts(b.Vfsopts, "ro", "nosuid", "nodev") {
			k.waitf("%s: lease bind %s: %s %s, want a read-only nosuid nodev erofs", v, b.Target, b.Fstype, b.Vfsopts)
		}
		if src, ok := sources[match.mk]; ok && src != b.Source {
			k.waitf("%s: lease bind %s of %s, its node mount is of %s", v, b.Target, b.Source, src)
		}
	}
	for _, w := range wants {
		if !w.found {
			k.waitf("%s: no lease bind of the fs lease of %s", v, k.rs.slot(w.slot).Name)
		}
	}
}

// checkLoops (P3): the loop devices over pmem are the cached node mounts.
func (k *checker) checkLoops(v VM, ov *ObsVM, want, either map[mountKey]bool) {
	var loops []ObsLoop
	if !k.part(v, "loops", ov.Loops, &loops) {
		return
	}
	lo, hi := 0, 0
	for mk := range want {
		if mk.cached {
			lo++
			hi++
		}
	}
	for mk := range either {
		if mk.cached && !want[mk] {
			hi++
		}
	}
	if len(loops) < lo || len(loops) > hi {
		names := []string{}
		for _, l := range loops {
			names = append(names, l.Name)
		}
		k.waitf("%s: %d loop devices over pmem %v, want %d (the cached node mounts)", v, len(loops), names, lo)
	}
}

// checkPmemRegions (P4, P5): a pmem region of a persistent device on a VM
// iff a claim or lease of it is prepared there (refused or retrying:
// either); with the namespace, one fsdax namespace mapping its memory,
// of the adopted UUID. It returns the block device of each device.
func (k *checker) checkPmemRegions(v VM, ov *ObsVM, users []pmemUser) map[DevID]string {
	var regions []ObsPmemRegion
	if !k.part(v, "pmem", ov.Pmem, &regions) {
		return nil
	}
	blockdevs := map[DevID]string{}
	need, maybe := map[DevID]bool{}, map[DevID]bool{}
	for _, u := range users {
		if u.either {
			maybe[u.dev] = true
		} else {
			need[u.dev] = true
		}
	}
	for d := cPmem0; d < nDevs; d++ {
		if !k.cfg.devUsed(d) {
			continue
		}
		var found []*ObsPmemRegion
		for j := range regions {
			if hasSerial(regions[j].Serials, devices[d].Serial) {
				found = append(found, &regions[j])
			}
		}
		switch {
		case len(found) > 1:
			k.waitf("%s: %d pmem regions of %s", v, len(found), d)
		case len(found) == 0 && need[d]:
			k.waitf("%s: no pmem region of %s, want one for its prepared claims", v, d)
		case len(found) == 1 && !need[d] && !maybe[d]:
			k.waitf("%s: pmem region %s of %s, no claim of it is prepared here (leaked region?)", v, found[0].Region, d)
		}
		if len(found) != 1 {
			continue
		}
		pd := &k.rs.PDev[pmemIndex(d)]
		var nss []ObsNamespace
		for _, ns := range found[0].Namespaces {
			if ns.Size == nil || *ns.Size > 0 {
				nss = append(nss, ns)
			}
		}
		switch {
		case pd.NS == TriNo && len(nss) > 0 && need[d]:
			k.waitf("%s: pmem region %s of %s has %d namespaces, the device has none", v, found[0].Region, d, len(nss))
		case pd.NS == TriYes && len(nss) != 1 && need[d]:
			k.waitf("%s: pmem region %s of %s: %d namespaces, want its fsdax namespace", v, found[0].Region, d, len(nss))
		case pd.NS == TriYes && len(nss) == 1:
			ns := &nss[0]
			if ns.Mode != nil && *ns.Mode != "memory" && *ns.Mode != "fsdax" {
				k.waitf("%s: namespace %s of %s: mode %s, want fsdax (memory)", v, ns.Name, d, *ns.Mode)
			}
			if ns.Map != nil && *ns.Map != "ram" && *ns.Map != "mem" {
				k.waitf("%s: namespace %s of %s: map %s, want mem (ram)", v, ns.Name, d, *ns.Map)
			}
			if ns.UUID != nil && pd.UUID != "" && *ns.UUID != pd.UUID {
				k.failf("%s: namespace %s of %s has UUID %s, the device's was %s (namespace lost)", v, ns.Name, d, *ns.UUID, pd.UUID)
			}
			if ns.Blockdev != nil {
				blockdevs[d] = "/dev/" + *ns.Blockdev
			} else if need[d] {
				k.waitf("%s: namespace %s of %s has no block device", v, ns.Name, d)
			}
		}
	}
	for _, r := range regions {
		if d := k.regionDev(r.Serials); d < 0 || !devices[d].Persistent {
			k.waitf("%s: pmem region %s of serials %s, not a persistent device", v, r.Region, strings.Join(r.Serials, ","))
		}
	}
	return blockdevs
}

func (k *checker) regionDev(serials []string) DevID {
	for _, s := range serials {
		if x, ok := parseSerial(s); ok {
			return devBySerial(x)
		}
	}
	return -1
}

// checkLeaseDirs (P8): lease-fs/<uid> and leases/<uid> only of prepared
// lease claims, mounts/ only of node mounts in use.
func (k *checker) checkLeaseDirs(v VM, ov *ObsVM, want, either map[mountKey]bool) {
	var dirs ObsLeaseDirs
	if !k.part(v, "leaseDirs", ov.LeaseDirs, &dirs) {
		return
	}
	claims := map[string]bool{} // lease claims of running lease pods
	for i, p := range k.cur {
		sl := k.rs.slot(i)
		if p == nil || !sl.Kind.leaseKind() || sl.VM != v || p.Claim == nil || p.Claim.UID == "" {
			continue
		}
		if ph := k.rs.Slots[i].Phase; ph == Running || ph == Restarting {
			claims[p.Claim.UID] = true
		}
	}
	for uid, names := range dirs.LeaseFS {
		if !claims[uid] && len(names) == 0 {
			k.waitf("%s: lease-fs/%s of no prepared lease claim", v, uid)
		}
	}
	for _, uid := range dirs.Leases {
		if !claims[uid] {
			k.waitf("%s: leases/%s of no prepared lease claim", v, uid)
		}
	}
	for _, name := range dirs.Mounts {
		mk, ok := mountKeyOf(name)
		if !ok || (!want[mk] && !either[mk]) {
			k.waitf("%s: mounts/%s, no node mount in use (orphan directory)", v, name)
		}
	}
}

// checkPoolClaims (P4): the block device of the persistent pool claims in
// state.json is the namespace's.
func (k *checker) checkPoolClaims(v VM, ov *ObsVM, blockdevs map[DevID]string) {
	var claims map[string]ObsPoolClaim
	if !k.part(v, "poolClaims", ov.PoolClaims, &claims) {
		return
	}
	for i := range k.rs.Slots {
		ss, sl := &k.rs.Slots[i], k.rs.slot(i)
		p := k.cur[i]
		if !sl.Kind.persistent() || ss.Node != v || (ss.Phase != Running && ss.Phase != Restarting) || p == nil || p.Claim == nil {
			continue
		}
		pc, ok := claims[p.Claim.UID]
		if !ok {
			k.waitf("%s: state.json: no prepared pool claim of %s", v, sl.Name)
			continue
		}
		bd := blockdevs[sl.Dev]
		for _, d := range pc.Devices {
			if d.BlockDevice != nil && bd != "" && *d.BlockDevice != bd {
				k.waitf("%s: state.json: the pool claim of %s on %s, the namespace's is %s", v, sl.Name, *d.BlockDevice, bd)
			}
		}
	}
}

// fsPath is the PATH of an fs claim (A22).
func fsPath(sl *Slot) string {
	if sl.Kind == KPFsc {
		return "/models"
	}
	return "/run/cxl-fs/" + serialKey(devices[sl.Dev].Serial)
}

// checkCDIPool (P10): a block claim's CDI device has exactly its
// namespace's /dev/pmem*, read-only iff the claim is; an fs claim's has
// no device node and a read-only bind of its node mount at PATH.
func (k *checker) checkCDIPool(v VM, ov *ObsVM, blockdevs map[DevID]string) {
	var cdi map[string]ObsCDIDevice
	if !k.part(v, "cdiPool", ov.CDIPool, &cdi) {
		return
	}
	for i := range k.rs.Slots {
		ss, sl := &k.rs.Slots[i], k.rs.slot(i)
		p := k.cur[i]
		if !sl.Kind.persistent() || ss.Node != v || (ss.Phase != Running && ss.Phase != Restarting) || p == nil || p.Claim == nil {
			continue
		}
		var dev *ObsCDIDevice
		for name := range cdi {
			n := name
			if j := strings.LastIndex(n, "="); j >= 0 {
				n = n[j+1:]
			}
			if strings.HasPrefix(n, "claim-"+p.Claim.UID+"-") {
				d := cdi[name]
				dev = &d
			}
		}
		if dev == nil {
			k.waitf("%s: cdiPool has no device of %s", v, sl.Name)
			continue
		}
		if sl.Kind.blockKind() {
			switch {
			case len(dev.DeviceNodes) != 1 || len(dev.Mounts) != 0:
				k.waitf("%s: CDI device of %s: %d device nodes and %d mounts, want its /dev/pmem*", v, sl.Name, len(dev.DeviceNodes), len(dev.Mounts))
			case blockdevs[sl.Dev] != "" && dev.DeviceNodes[0].Path != blockdevs[sl.Dev]:
				k.waitf("%s: CDI device of %s: %s, the namespace's is %s", v, sl.Name, dev.DeviceNodes[0].Path, blockdevs[sl.Dev])
			case dev.DeviceNodes[0].Permissions != nil && strings.Contains(*dev.DeviceNodes[0].Permissions, "w") == !sl.Kind.writer():
				k.waitf("%s: CDI device of %s: permissions %s", v, sl.Name, *dev.DeviceNodes[0].Permissions)
			}
			continue
		}
		switch {
		case len(dev.DeviceNodes) != 0 || len(dev.Mounts) != 1:
			k.waitf("%s: CDI device of %s: %d device nodes and %d mounts, want one bind", v, sl.Name, len(dev.DeviceNodes), len(dev.Mounts))
		case dev.Mounts[0].ContainerPath != fsPath(sl):
			k.waitf("%s: CDI device of %s: mount at %s, want %s", v, sl.Name, dev.Mounts[0].ContainerPath, fsPath(sl))
		case !hasOpts(strings.Join(dev.Mounts[0].Options, ","), "ro"):
			k.waitf("%s: CDI device of %s: mount options %v, want ro", v, sl.Name, dev.Mounts[0].Options)
		}
	}
}

// checkKernel (P7): no new BUG, Oops, WARNING or general protection line.
func (k *checker) checkKernel(v VM, ov *ObsVM) {
	var kl ObsKernel
	if !k.part(v, "kernel", ov.Kernel, &kl) {
		return
	}
	if kl.Bad > 0 {
		k.failf("%s: kernel: %d new BUG/Oops/WARNING lines: %s", v, kl.Bad, strings.Join(kl.Lines, " | "))
	}
}

// checkKillAt: an armed watcher that fired while the model's outcome is
// the one without the kill (the other outcome fits then).
func (k *checker) checkKillAt(v VM, ov *ObsVM) {
	if k.rs.Drv[v].KillAt == 0 {
		return
	}
	var st struct {
		Fired bool `json:"fired"`
	}
	if ok, _, _ := rawPart(ov.KillAt, &st); ok && st.Fired {
		k.waitf("%s: driver-kill-at fired, want the outcome with the kill", v)
	}
}

// checkPDevs (P5, P6): the backing files of the persistent devices hold
// what the model says: labels iff the namespace, the pfn info with it,
// the erofs, its token and bytes 0-63; their hashes change only at a
// writer's Prepare without namespace, pwrite and pwipe.
func (k *checker) checkPDevs() {
	if !k.pmemUsed() {
		return
	}
	pdevs, ok := k.pdevs()
	if !ok {
		return
	}
	for x := range k.rs.PDev {
		d := cPmem0 + DevID(x)
		if !k.cfg.devUsed(d) {
			continue
		}
		ob := pdevs[d.String()]
		if ob == nil {
			k.waitf("pdevs: %s not observed", d)
			continue
		}
		pd := &k.rs.PDev[x]
		switch pd.NS {
		case TriYes:
			if ob.LSA != "labels" || !ob.PFN {
				k.waitf("pdevs: %s: lsa %s pfn %v, want its namespace (labels, pfn info)", d, ob.LSA, ob.PFN)
			}
		case TriNo:
			if ob.LSA != "zero" || ob.PFN {
				k.waitf("pdevs: %s: lsa %s pfn %v, want a device without namespace", d, ob.LSA, ob.PFN)
			}
		}
		if ob.PFN && pd.FS != TriMaybe && ob.Erofs != (pd.FS == TriYes) {
			k.waitf("pdevs: %s: erofs %v, want %v", d, ob.Erofs, pd.FS == TriYes)
		}
		if pd.FS == TriYes && pd.Tok.Known && ob.Token != nil && *ob.Token != pd.Tok.Text {
			k.failf("pdevs: %s: token %q, want %q (data changed)", d, *ob.Token, pd.Tok.Text)
		}
		if ob.PFN && pd.Raw.Known {
			raw := ""
			if ob.Raw != nil {
				raw = *ob.Raw
			}
			if raw != pd.Raw.Text {
				k.failf("pdevs: %s: bytes 0-63 %q, want %q (data changed)", d, raw, pd.Raw.Text)
			}
		}
		if pd.LSAHash != "" && ob.LSAHash != "" && ob.LSAHash != pd.LSAHash {
			k.failf("pdevs: %s: the LSA changed (sha %s, was %s) without a writer's Prepare on a fresh device or pwipe", d, short(ob.LSAHash), short(pd.LSAHash))
		}
		if pd.HeadHash != "" && ob.HeadHash != "" && ob.HeadHash != pd.HeadHash {
			k.failf("pdevs: %s: the data changed (sha %s, was %s) without a writer's Prepare, pwrite or pwipe", d, short(ob.HeadHash), short(pd.HeadHash))
		}
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// checkInspectPersistent (P10): the container of a persistent claim has
// no /dev/pmem* (fs) or exactly its PATH (block), an fs claim's PATH is
// a read-only erofs mount; a plse requester sees its fs leases under
// /run/cxl/fs.
func (k *checker) checkInspectPersistent(i int, in *ObsInspect) {
	sl := k.rs.slot(i)
	ss := &k.rs.Slots[i]
	if sl.Kind == KPlse {
		if in.Mounts == nil {
			return
		}
		if ss.leaseFS() {
			name := "mbt"
			if ss.LeaseMode == ModePFC {
				name = k.rs.LeaseIDs[i]
			}
			found := false
			for _, m := range in.Mounts {
				if name == "" || path.Base(m.Target) == name {
					found = true
				}
			}
			switch l := unreadyLease(in, ss.LeaseMode, k.rs.LeaseIDs[i]); {
			case found:
			case l != nil:
				// the bind comes before Ready: a lease whose
				// companion's Prepare was cut at the bind (A29 d,
				// A30) has none until kubelet's retry (A33)
				k.waitf("inspect %s: no /run/cxl/fs/%s in its container, its lease %s is %s", sl.Name, name, l.Lease, l.State)
			default:
				k.failf("inspect %s: no /run/cxl/fs/%s in its container (an fs lease it cannot see)", sl.Name, name)
			}
		}
		return
	}
	key := "CXL_" + serialKey(devices[sl.Dev].Serial) + "_PATH"
	switch {
	case in.PmemNodes == nil:
	case sl.Kind.fsKind() && len(in.PmemNodes) > 0:
		k.failf("inspect %s: /dev/pmem* %v in an fs container", sl.Name, in.PmemNodes)
	case sl.Kind.blockKind() && (len(in.PmemNodes) != 1 || in.PmemNodes[0] != in.Env[key]):
		k.failf("inspect %s: /dev/pmem* %v, want exactly %s", sl.Name, in.PmemNodes, in.Env[key])
	}
	if sl.Kind.fsKind() {
		switch m := in.Mount; {
		case m == nil:
			k.failf("inspect %s: %s is no mount in its container", sl.Name, in.Env[key])
		case m.Fstype != "erofs" || !hasOpts(m.Vfsopts, "ro"):
			k.failf("inspect %s: %s is a %s mount (%s), want a read-only erofs", sl.Name, m.Target, m.Fstype, m.Vfsopts)
		}
	}
}

// unreadyLease returns the lease of mode m (and id, if known) that an
// inspection lists on its way to Ready (Creating, Pending, Lending), or
// nil.
func unreadyLease(in *ObsInspect, m LeaseMode, id string) *ObsLease {
	list, _ := in.leases()
	for j := range list {
		l := &list[j]
		switch {
		case id != "" && l.Lease != id, !leaseModeIs(l.Mode, m):
		case l.State == "Creating", l.State == "Pending", l.State == "Lending":
			return l
		}
	}
	return nil
}

// adoptPRead learns a token that a pread saw where the model did not
// know it.
func adoptPRead(n *State, i int, text string) {
	sl, ss := n.slot(i), &n.Slots[i]
	pd := &n.PDev[pmemIndex(n.pdevOf(i))]
	t := Token{Known: true, Text: text}
	if sl.Kind.fsKind() || (sl.Kind == KPlse && ss.LeaseMode.fs()) {
		pd.Tok = t
	} else {
		pd.Raw = t
	}
}
