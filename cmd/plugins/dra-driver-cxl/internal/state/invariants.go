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

package state

import (
	"fmt"
	"slices"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Violation is a record that breaks an invariant.
type Violation struct {
	Invariant string
	Record    string
	// Source is the source of the record, set by the document
	// invariants: that of a memdev, of a grant, or of the memdev of a
	// region's first target; "" for the other records.
	Source Source
	Text   string
}

// InvariantLeaseClaims is the name of the document invariant over the
// lease claims, which the engine does not check.
const InvariantLeaseClaims = "lease claims"

// String returns the violation in words.
func (v Violation) String() string {
	return fmt.Sprintf("%s: %s: %s", v.Invariant, v.Record, v.Text)
}

// recordID names a record in violations, with its source.
type recordID struct {
	name   string
	source Source
}

// part returns the identity of a part of the record.
func (id recordID) part(name string) recordID {
	id.name += " " + name
	return id
}

// addFunc adds a violation by a record.
type addFunc func(id recordID, format string, args ...any)

// Invariant is a predicate over the records alone.
type Invariant struct {
	Name  string
	Text  string
	check func(r *Records, add addFunc)
}

// DocumentInvariants are the invariants over the records alone; the engine
// checks the ones that also need the kernel's view.
var DocumentInvariants = []Invariant{
	{
		Name: "records",
		Text: "Every record has a state of its table that a stored record may have, and a unique identity. " +
			"Every region of ours is one Region record, referenced by every memdev in its Targets, and every " +
			"region a memdev references is a record of ours that targets it; a FOREIGN region is owned by no one. " +
			"Every grant belongs to one result of one preparation and is listed by the memdev of its serial " +
			"(except the grants of a PREPARED_STALE preparation); what Got names lists the grant among its users, " +
			"and every user of a dax device or a node mount names it back; a grant result names a grant record, " +
			"but of a PREPARING or UNPREPARING preparation (not journaled yet, or released before the kubelet plugin's " +
			"commit) and of a PREPARED_STALE one (a reboot cut that Prepare or Unprepare). A PREPARED preparation's grants are " +
			"REALISED, a PREPARED_UNRESTORED one's REALISED or REALISED_UNRESTORED. A lease lends only while READY, " +
			"releasing or FAILED.",
		check: checkRecords,
	},
	{
		Name: InvariantLeaseClaims,
		Text: "The lease claim of a lease result of a PREPARED or PREPARED_UNRESTORED preparation has a record, " +
			"unless the claim's Unprepare runs: it closes the lease claim before the preparation turns UNPREPARING. " +
			"The engine does not check this invariant: the lease claims are the lease broker's records, which it " +
			"writes out of order with the preparations.",
		check: checkLeaseClaims,
	},
	{
		Name: "users",
		Text: "Between requests and after the recovery, every region of ours is OWNED, KEPT or HOLLOW_OWNED, and " +
			"every dax device of a region of ours that is OWNED or KEPT and is not the seed has a user grant that is " +
			"REALISED, REALISED_UNRESTORED or RELEASING, or is the Whole device of a KEPT region, of a memdev that " +
			"is IDLE_KEPT, or of a region that a grant of a Prepare that did not finish names, or has as its user a " +
			"grant of a Prepare that did not finish of a numa companion or of a KEPT preparation (kept for its " +
			"Unprepare); the recovery deletes the others, those of a HOLLOW_OWNED region go with it.",
		check: checkUsers,
	},
	{
		Name: "journaled",
		Text: "Every object that the driver created lies inside a journaled container: a region of ours targets " +
			"memdevs with a record; the region, bind target and lend of a grant's Got are named by its Intent; " +
			"a node mount's path is the Intent of a user grant.",
		check: checkJournaled,
	},
}

// Check returns the violations of the document invariants by the records.
func Check(r *Records) []Violation {
	var out []Violation
	for _, inv := range DocumentInvariants {
		inv.check(r, func(id recordID, format string, args ...any) {
			out = append(out, Violation{Invariant: inv.Name, Record: id.name, Source: id.source, Text: fmt.Sprintf(format, args...)})
		})
	}
	return out
}

// CheckStates returns an error that names the first record whose state is
// not one that a stored record may have, nil if there is none.
func CheckStates(r *Records) error {
	var err error
	checkStates(r, func(id recordID, format string, args ...any) {
		if err == nil {
			err = fmt.Errorf("%s: %s", id.name, fmt.Sprintf(format, args...))
		}
	})
	return err
}

// The states that a record in the store may have, by table; the others
// are defined next to their tables.
var (
	memdevStored     = fsm.List(MemdevActive, MemdevIdleKept, MemdevGone)
	daxDeviceStored  = fsm.List(DaxCreating, DaxUnbound, DaxDeviceDax, DaxKmemOffline, DaxKmemOnline, DaxSeed)
	mountStored      = fsm.List(MountedRO, MountedRW, MountForeign, MountUnrestored)
	bindStored       = fsm.List(BindAbsent, BindBound, BindStale)
	leaseClaimStored = fsm.List(LeaseClaimOpen, LeaseClaimOpenNoListener, LeaseClaimClosing)
	provenanceStored = fsm.List(ProvenanceHeld, ProvenanceLocal, ProvenancePool)
	lendingLeases    = fsm.List(LeaseReady, LeaseReleasing, LeaseReleasingBusy, LeaseFailed)
)

// checkRecords adds the violations of record consistency.
func checkRecords(r *Records, add addFunc) {
	checkStates(r, add)
	checkUnique(r, add)
	checkRegionOwners(r, add)
	checkRegionUsers(r, add)
	checkMemdevRefs(r, add)
	for i := range r.Grants {
		checkGrant(r, &r.Grants[i], add)
	}
	for i := range r.Preparations {
		checkPreparation(r, &r.Preparations[i], add)
	}
	checkLends(r, add)
}

// stored adds the records of a section whose state is not one of stored.
func stored[T any, S comparable](add addFunc, records []T, allowed []S, id func(*T) recordID, state func(*T) S) {
	for i := range records {
		if s := state(&records[i]); !slices.Contains(allowed, s) {
			add(id(&records[i]), "state %q is not one of a stored record", fmt.Sprint(s))
		}
	}
}

// checkStates adds the records whose state a stored record may not have.
func checkStates(r *Records, add addFunc) {
	stored(add, r.Preparations, preparationStored, preparationID, func(p *Preparation) PreparationState { return p.State })
	stored(add, r.Grants, grantStored, grantID, func(g *Grant) GrantState { return g.State })
	stored(add, r.Memdevs, memdevStored, memdevID, func(m *Memdev) MemdevState { return m.State })
	stored(add, r.Regions, regionStored, r.regionID, func(reg *Region) RegionState { return reg.State })
	stored(add, r.Leases, leaseStored, leaseID, func(l *Lease) LeaseState { return l.State })
	stored(add, r.LeaseClaims, leaseClaimStored, leaseClaimID, func(c *LeaseClaim) LeaseClaimState { return c.State })
	stored(add, r.Provenance, provenanceStored, provenanceID, func(p *Provenance) ProvenanceState { return p.State })
	for i := range r.Grants {
		if b := r.Grants[i].Got.Bind; b != nil && !slices.Contains(bindStored, b.State) {
			add(grantID(&r.Grants[i]), "bind state %q is not one of a stored record", b.State)
		}
	}
	for i := range r.Regions {
		reg := &r.Regions[i]
		stored(add, reg.DaxDevices, daxDeviceStored, func(d *DaxDevice) recordID { return r.regionID(reg).part("dax device " + d.Name) },
			func(d *DaxDevice) DaxDeviceState { return d.State })
		stored(add, reg.Mounts, mountStored, func(m *Mount) recordID { return r.regionID(reg).part("mount " + m.Path) },
			func(m *Mount) MountState { return m.State })
	}
}

// The identities of records in violations.
func preparationID(p *Preparation) recordID {
	return recordID{name: "preparation " + p.Driver + "/" + p.UID}
}
func grantID(g *Grant) recordID           { return recordID{"grant " + g.Ref.String(), g.Asked.Source} }
func memdevID(m *Memdev) recordID         { return recordID{"memdev " + m.Serial.String(), m.Source} }
func leaseID(l *Lease) recordID           { return recordID{name: "lease " + l.ID} }
func leaseClaimID(c *LeaseClaim) recordID { return recordID{name: "lease claim " + c.UID} }
func provenanceID(p *Provenance) recordID { return recordID{name: "provenance " + p.Serial.String()} }
func steeredID(s *Steered) recordID       { return recordID{name: "steered " + s.Container} }

// regionID returns the identity of a region in violations, whose source is
// that of the memdev of its first target.
func (r *Records) regionID(reg *Region) recordID {
	id := recordID{name: "region " + string(reg.Ref())}
	if len(reg.Targets) > 0 {
		if m := r.Memdev(reg.Targets[0]); m != nil {
			id.source = m.Source
		}
	}
	return id
}

// checkUnique adds the identities that more than one record of a section
// has.
func checkUnique(r *Records, add addFunc) {
	unique(add, r.Preparations, preparationID)
	unique(add, r.Grants, grantID)
	unique(add, r.Memdevs, memdevID)
	unique(add, r.Regions, r.regionID)
	unique(add, r.Leases, leaseID)
	unique(add, r.LeaseClaims, leaseClaimID)
	unique(add, r.Provenance, provenanceID)
	unique(add, r.Steered, steeredID)
}

// unique adds the identities that more than one record of the section has.
func unique[T any](add addFunc, records []T, id func(*T) recordID) {
	seen := map[string]bool{}
	for i := range records {
		k := id(&records[i])
		if seen[k.name] {
			add(k, "more than one record")
		}
		seen[k.name] = true
	}
}

// checkRegionOwners adds the regions whose owner and state disagree, that
// have no name, and the target memdevs that do not reference a region of
// ours.
func checkRegionOwners(r *Records, add addFunc) {
	for i := range r.Regions {
		reg := &r.Regions[i]
		if (reg.State == RegionForeign) != (reg.Owner == OwnerForeign) {
			add(r.regionID(reg), "state %s with owner %q", reg.State, reg.Owner)
		}
		if reg.Ref() == "" {
			add(r.regionID(reg), "no name and no noted name")
		}
		if !reg.Ours() {
			continue
		}
		for _, serial := range reg.Targets {
			if m := r.Memdev(serial); m != nil && !m.HasRegion(reg.Ref()) {
				add(r.regionID(reg), "target memdev %s does not reference it", serial)
			}
		}
	}
}

// checkRegionUsers adds the users of the dax devices and node mounts of
// regions of ours that do not use them.
func checkRegionUsers(r *Records, add addFunc) {
	for i := range r.Regions {
		reg := &r.Regions[i]
		if !reg.Ours() {
			continue
		}
		for _, d := range reg.DaxDevices {
			for _, u := range d.Users {
				if g := r.Grant(u); g == nil || g.Got.Region != reg.Ref() || g.Got.Dax != d.Name {
					add(r.regionID(reg), "dax device %s: user %s does not use it", d.Name, u)
				}
			}
		}
		for _, m := range reg.Mounts {
			for _, u := range m.Users {
				if g := r.Grant(u); g == nil || g.Got.Region != reg.Ref() || g.Got.Mount != m.Access {
					add(r.regionID(reg), "mount %s: user %s does not use it", m.Path, u)
				}
			}
		}
	}
}

// checkMemdevRefs adds the regions and grants that memdevs reference but
// that are not theirs.
func checkMemdevRefs(r *Records, add addFunc) {
	for i := range r.Memdevs {
		m := &r.Memdevs[i]
		for _, ref := range append(slices.Clone(m.Regions), m.Hollow...) {
			reg := r.Region(ref)
			switch {
			case reg == nil:
				add(memdevID(m), "region %s has no record", ref)
			case !reg.Ours() || !reg.TargetsSerial(m.Serial):
				add(memdevID(m), "region %s is not one of ours on it", ref)
			}
		}
		for _, ref := range m.Grants {
			if g := r.Grant(ref); g == nil || g.Serial != m.Serial {
				add(memdevID(m), "grant %s is not one of its grants", ref)
			}
		}
	}
}

// checkGrant adds the violations of the references of a grant: its
// preparation result, its memdev, and what its Got names.
func checkGrant(r *Records, g *Grant, add addFunc) {
	owners := 0
	var prep *Preparation
	for i := range r.Preparations {
		for _, res := range r.Preparations[i].Results {
			if res.Grant != nil && *res.Grant == g.Ref {
				owners++
				prep = &r.Preparations[i]
			}
		}
	}
	if g.Asked.Kind.Source() != g.Asked.Source {
		add(grantID(g), "kind %s of source %s", g.Asked.Kind, g.Asked.Source)
	}
	if owners != 1 {
		add(grantID(g), "belongs to %d preparation results", owners)
	}
	if prep == nil || prep.State != PreparationStale {
		if m := r.Memdev(g.Serial); m == nil || !m.HasGrant(g.Ref) {
			add(grantID(g), "memdev %s does not list it", g.Serial)
		}
	}
	checkGot(r, g, add)
}

// checkGot adds the objects that a grant's Got names and that do not list
// the grant.
func checkGot(r *Records, g *Grant, add addFunc) {
	if g.Got.Region == "" {
		if g.Got.Dax != "" || g.Got.Mount != "" {
			add(grantID(g), "uses a dax device or a mount without a region")
		}
		return
	}
	reg := r.Region(g.Got.Region)
	if reg == nil || !reg.Ours() || !reg.TargetsSerial(g.Serial) {
		add(grantID(g), "region %s is not one of ours on its memdev", g.Got.Region)
		return
	}
	if g.Got.Dax != "" {
		if d := reg.DaxDevice(g.Got.Dax); d == nil || !slices.Contains(d.Users, g.Ref) {
			add(grantID(g), "dax device %s does not list it", g.Got.Dax)
		}
	}
	if g.Got.Mount != "" {
		if m := reg.Mount(g.Got.Mount); m == nil || !slices.Contains(m.Users, g.Ref) {
			add(grantID(g), "mount %s does not list it", g.Got.Mount)
		}
	}
}

// checkPreparation adds the violations of the results of a preparation.
func checkPreparation(r *Records, p *Preparation, add addFunc) {
	seen := map[int]bool{}
	for i := range p.Results {
		res := &p.Results[i]
		if seen[res.Index] {
			add(preparationID(p), "result %d more than once", res.Index)
		}
		seen[res.Index] = true
		if n := resultParts(res); n != 1 || !resultKindSet(res) {
			add(preparationID(p), "result %d of kind %q has %d parts", res.Index, res.Kind, n)
			continue
		}
		if res.Kind == ResultGrant {
			checkGrantResult(r, p, res, add)
		}
	}
}

// checkGrantResult adds the violations of a result of kind grant: it names
// the grant of the claim with its index, and the grant's state fits the
// preparation's.
func checkGrantResult(r *Records, p *Preparation, res *Result, add addFunc) {
	if *res.Grant != (GrantRef{Claim: p.UID, Result: res.Index}) {
		add(preparationID(p), "result %d names grant %s", res.Index, res.Grant)
	}
	g := r.Grant(*res.Grant)
	switch {
	case g == nil && (p.State == PreparationPreparing || p.State == PreparationUnpreparing || p.State == PreparationStale):
		// not journaled yet, or released by a rollback, the recovery or
		// the Unprepare before the kubelet plugin's commit; a stale preparation
		// is such a PREPARING or UNPREPARING one of another boot, whose
		// Prepare again or Unprepare has nothing of the grant to release
	case g == nil:
		add(preparationID(p), "grant %s has no record", res.Grant)
	case p.State == PreparationPrepared && g.State != GrantRealised:
		add(preparationID(p), "PREPARED with grant %s %s", g.Ref, g.State)
	case p.State == PreparationUnrestored && g.State != GrantRealised && g.State != GrantUnrestored:
		add(preparationID(p), "PREPARED_UNRESTORED with grant %s %s", g.Ref, g.State)
	}
}

// resultParts returns the number of the parts of a result that are set.
func resultParts(res *Result) int {
	n := 0
	for _, set := range []bool{res.Grant != nil, res.Region != nil, res.DRAM != nil, res.LeaseClaim != nil} {
		if set {
			n++
		}
	}
	return n
}

// resultKindSet tells whether the part of the result's kind is set.
func resultKindSet(res *Result) bool {
	switch res.Kind {
	case ResultGrant:
		return res.Grant != nil
	case ResultRegion:
		return res.Region != nil
	case ResultDRAM:
		return res.DRAM != nil
	case ResultLeaseClaim:
		return res.LeaseClaim != nil
	}
	return false
}

// checkLends adds the leases that lend in a state that does not lend.
func checkLends(r *Records, add addFunc) {
	for i := range r.Leases {
		if l := &r.Leases[i]; l.Lend != nil && !slices.Contains(lendingLeases, l.State) {
			add(leaseID(l), "lends while %s", l.State)
		}
	}
}

// checkLeaseClaims adds the PREPARED and PREPARED_UNRESTORED preparations
// with a lease result whose lease claim has no record.
func checkLeaseClaims(r *Records, add addFunc) {
	for i := range r.Preparations {
		p := &r.Preparations[i]
		if p.State != PreparationPrepared && p.State != PreparationUnrestored {
			continue
		}
		for _, res := range p.Results {
			if res.Kind == ResultLeaseClaim && res.LeaseClaim != nil && r.LeaseClaim(res.LeaseClaim.UID) == nil {
				add(preparationID(p), "lease claim %s has no record", res.LeaseClaim.UID)
			}
		}
	}
}

// checkUsers adds the regions of ours that are not at rest and their dax
// devices without a realised or releasing user that are not kept.
func checkUsers(r *Records, add addFunc) {
	for i := range r.Regions {
		reg := &r.Regions[i]
		if !reg.Ours() {
			continue
		}
		if reg.State == RegionNoted {
			add(r.regionID(reg), "%s between requests", reg.State)
		}
		if reg.State == RegionHollowOwned {
			// its dax devices go with it when it is deleted
			continue
		}
		for _, d := range reg.DaxDevices {
			if d.State == DaxSeed || (d.Whole && (r.KeptRegion(reg) || r.UnfinishedRegion(reg))) || r.InUse(&d) || r.UnfinishedUser(&d) {
				continue
			}
			add(r.regionID(reg), "dax device %s has no realised or releasing user", d.Name)
		}
	}
}

// InUse tells whether a user of the dax device is REALISED,
// REALISED_UNRESTORED or RELEASING.
func (r *Records) InUse(d *DaxDevice) bool {
	for _, u := range d.Users {
		if g := r.Grant(u); g != nil && (g.State == GrantRealised || g.State == GrantUnrestored || g.State == GrantReleasing) {
			return true
		}
	}
	return false
}

// UnfinishedUser tells whether a user of the dax device is a grant of a
// Prepare that did not finish (JOURNALED, REALISING) that the start kept
// with its slice for its Unprepare: one of a numa companion, or one of a
// KEPT preparation whose release at the start failed.
func (r *Records) UnfinishedUser(d *DaxDevice) bool {
	for _, u := range d.Users {
		g := r.Grant(u)
		if g == nil || (g.State != GrantJournaled && g.State != GrantRealising) {
			continue
		}
		if p := r.PreparationOf(g.Ref); (g.Intent.Lend != nil && g.Asked.Mode == ModeNUMA) || (p != nil && p.State == PreparationKept) {
			return true
		}
	}
	return false
}

// RegionInMaking tells whether the region reg of the local memdev m is
// still being made for a Prepare: NOTED, or a ram region OWNED
// while every grant of the memdev is of a Prepare that did not finish
// (JOURNALED, REALISING) and got no slice of it yet (it is set up, probed and
// zeroed before its first slice).
func (r *Records) RegionInMaking(m *Memdev, reg *Region) bool {
	switch {
	case reg.State == RegionNoted:
		return true
	case reg.State != RegionOwned || reg.Mode != RegionRAM || len(m.Grants) == 0:
		return false
	}
	for _, ref := range m.Grants {
		g := r.Grant(ref)
		if g == nil || (g.State != GrantJournaled && g.State != GrantRealising) || g.Got.Dax != "" {
			return false
		}
	}
	return true
}

// UnfinishedRegion tells whether a grant of a Prepare that did not finish
// (JOURNALED, REALISING, or RELEASING by a failed release before it got a
// dax device) names the region: a numa companion that a start keeps for
// its Unprepare.
func (r *Records) UnfinishedRegion(reg *Region) bool {
	for i := range r.Grants {
		g := &r.Grants[i]
		open := g.State == GrantJournaled || g.State == GrantRealising || g.State == GrantReleasing && g.Got.Dax == "" && r.PrepareUnfinished(g)
		if open && (g.Got.Region == reg.Ref() || g.Intent.Region == reg.Ref()) {
			return true
		}
	}
	return false
}

// PrepareUnfinished tells whether a grant is of a claim whose Prepare did
// not finish: PREPARING (a numa companion that a start kept) or KEPT.
func (r *Records) PrepareUnfinished(g *Grant) bool {
	p := r.PreparationOf(g.Ref)
	return p != nil && (p.State == PreparationPreparing || p.State == PreparationKept)
}

// KeptRegion tells whether the region is KEPT or a memdev of it is
// IDLE_KEPT.
func (r *Records) KeptRegion(reg *Region) bool {
	if reg.State == RegionKept {
		return true
	}
	for _, serial := range reg.Targets {
		if m := r.Memdev(serial); m != nil && m.State == MemdevIdleKept {
			return true
		}
	}
	return false
}

// checkJournaled adds the objects of ours that no intent or record names.
func checkJournaled(r *Records, add addFunc) {
	for i := range r.Regions {
		reg := &r.Regions[i]
		if !reg.Ours() {
			continue
		}
		for _, serial := range reg.Targets {
			if r.Memdev(serial) == nil {
				add(r.regionID(reg), "target memdev %s has no record", serial)
			}
		}
		for _, m := range reg.Mounts {
			if m.State != MountForeign && !journaledMount(r, &m) {
				add(r.regionID(reg), "mount %s is named by no intent", m.Path)
			}
		}
	}
	for i := range r.Grants {
		checkGrantIntent(&r.Grants[i], add)
	}
}

// checkGrantIntent adds the objects of a grant's Got that its Intent does
// not name.
func checkGrantIntent(g *Grant, add addFunc) {
	if g.Got.Region != "" && g.Got.Region != g.Intent.Region {
		add(grantID(g), "region %s, its intent names %q", g.Got.Region, g.Intent.Region)
	}
	if g.Got.Bind != nil && g.Got.Bind.Target != g.Intent.BindTarget {
		add(grantID(g), "bind %s, its intent names %q", g.Got.Bind.Target, g.Intent.BindTarget)
	}
	if l := g.Got.Lend; l != nil {
		il := g.Intent.Lend
		if il == nil || il.Lease != l.Lease || NUMANodeOf(il.NUMANode) != NUMANodeOf(l.NUMANode) {
			add(grantID(g), "lend of lease %s is not its intent", l.Lease)
		}
	}
}

// journaledMount tells whether a user grant's intent names the path of the
// mount.
func journaledMount(r *Records, m *Mount) bool {
	for _, u := range m.Users {
		if g := r.Grant(u); g != nil && g.Intent.MountPath == m.Path {
			return true
		}
	}
	return false
}
