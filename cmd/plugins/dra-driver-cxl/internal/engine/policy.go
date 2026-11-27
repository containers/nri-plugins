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

package engine

import (
	"slices"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
)

// Release is what releases the region of a memdev whose last grant goes,
// the column of the release policy.
type Release string

const (
	// ReleaseUnprepare is the Unprepare of the last grant of the memdev.
	ReleaseUnprepare Release = "unprepare"
	// ReleaseRollback is the rollback of a Prepare that failed.
	ReleaseRollback Release = "rollback"
	// ReleaseRecovery is the release of a Prepare that a process stop cut,
	// at the start.
	ReleaseRecovery Release = "recovery"
	// ReleaseIdle is the release of a region of the driver that no grant
	// and no cut Prepare holds, at the start, at the next Prepare of the
	// memdev or when the stale lease binds that held it are gone.
	ReleaseIdle Release = "idle"
)

// Kind is how the grants of a memdev use it, the row of the release
// policy.
type Kind string

const (
	// KindLocal is a local memdev whose grants have slices of it, or
	// one used shared that is not configured shared any more whose region
	// is new.
	KindLocal Kind = "local"
	// KindLocalShared is a local memdev configured shared whose region is
	// established: set up and zeroed, or kept by an earlier release.
	KindLocalShared Kind = "local shared"
	// KindLocalSharedNew is a local memdev configured shared whose region a
	// Prepare made and did not set up or zero (it failed or was cut).
	KindLocalSharedNew Kind = "local shared, new region"
	// KindLocalSharedUnlisted is a local memdev used shared that is not
	// configured shared any more (not in sharedMemdevs), its region
	// established.
	KindLocalSharedUnlisted Kind = "local shared, not in sharedMemdevs"
	// KindLocalPersistent is a local memdev of persistent memory whose
	// region is established: set up, or kept by an earlier release.
	KindLocalPersistent Kind = "local persistent"
	// KindLocalPersistentNew is a local memdev of persistent memory whose
	// region a Prepare made and did not set up (it failed or was cut).
	KindLocalPersistentNew Kind = "local persistent, new region"
	// KindPool is a pool device of one grant.
	KindPool Kind = "pool"
	// KindPoolShared is a pool device that several grants share.
	KindPoolShared Kind = "pool shared"
	// KindPoolPersistent is a pool device of persistent memory.
	KindPoolPersistent Kind = "pool persistent"
)

// Decision is what the release does with the region of a memdev, and
// whom a region that stays is for.
type Decision string

const (
	// Destroy releases the region, also below a newer region of another
	// memdev (a hole left in the decoders of a port).
	Destroy Decision = "destroy"
	// InOrder releases the region unless a newer region of another memdev
	// lies above it in the decoders of a port, before or after its
	// disable: then the region stays (disabled when the newer one came
	// during the release) for the claim of a rollback or a recovery
	// (KEPT), for the idle memdev of an Unprepare or an idle release.
	InOrder Decision = "in order"
	// InOrderIdle releases the region in order, and a region that stays
	// is for the idle memdev also at a rollback or a recovery.
	InOrderIdle Decision = "in order, kept idle"
	// Keep leaves the region as it is, for the claim of a rollback or a
	// recovery (KEPT), for the idle memdev of an Unprepare or an idle
	// release.
	Keep Decision = "keep"
	// KeepIdle leaves the region as it is for the idle memdev, also at a
	// rollback or a recovery.
	KeepIdle Decision = "keep idle"
)

// forClaim tells whether a region that the decision keeps at the release
// is kept for the claim of the released grant (KEPT), not for the idle
// memdev.
func (d Decision) forClaim(release Release) bool {
	return (d == InOrder || d == Keep) && (release == ReleaseRollback || release == ReleaseRecovery)
}

// inOrder tells whether the decision releases the region in order.
func (d Decision) inOrder() bool {
	return d == InOrder || d == InOrderIdle
}

// keeps tells whether the decision leaves the region as it is.
func (d Decision) keeps() bool {
	return d == Keep || d == KeepIdle
}

// PolicyRow is a row of the release policy: the decisions of the releases
// of the last grant of a memdev of a kind.
type PolicyRow struct {
	Kind                Kind
	Unprepare, Rollback Decision
	Recovery            Decision
	RecoveryAbove       Decision // with regions of other memdevs above the region
	Idle                Decision
}

// PolicyRows are the release policy, in the order of the documentation: a
// unshared local memdev is released in the order that the user chose (a hole
// is allowed), a shared one is kept while it is configured shared (zeroed
// once), a persistent one is kept idle while regions of other memdevs lie
// above it; a pool region is destroyed unless a recovery finds regions
// above it.
var PolicyRows = []PolicyRow{
	// kind, unprepare, rollback, recovery, recovery above, idle
	{KindLocal, Destroy, InOrder, InOrder, InOrder, Destroy},
	{KindLocalShared, KeepIdle, KeepIdle, KeepIdle, KeepIdle, KeepIdle},
	{KindLocalSharedNew, KeepIdle, InOrder, InOrder, InOrder, KeepIdle},
	{KindLocalSharedUnlisted, Destroy, InOrder, Destroy, Destroy, Destroy},
	{KindLocalPersistent, InOrderIdle, InOrder, InOrderIdle, InOrderIdle, InOrderIdle},
	{KindLocalPersistentNew, InOrderIdle, InOrder, InOrder, InOrder, InOrderIdle},
	// a kept pool region would block the hot-remove of the device; a
	// recovery keeps one below regions of other memdevs for its claim
	{KindPool, Destroy, Destroy, Destroy, Keep, Destroy},
	{KindPoolShared, Destroy, Destroy, Destroy, Keep, Destroy},
	{KindPoolPersistent, Destroy, Destroy, Destroy, Keep, Destroy},
}

// PolicyReleases are the columns of the release policy, in the order of
// the documentation.
var PolicyReleases = []Release{ReleaseUnprepare, ReleaseRollback, ReleaseRecovery, ReleaseIdle}

// Decision returns the decision of the row for the release, with regions
// of other memdevs above the region or not.
func (p PolicyRow) Decision(release Release, above bool) Decision {
	switch release {
	case ReleaseUnprepare:
		return p.Unprepare
	case ReleaseRollback:
		return p.Rollback
	case ReleaseRecovery:
		if above {
			return p.RecoveryAbove
		}
		return p.Recovery
	case ReleaseIdle:
		return p.Idle
	}
	return Keep
}

// Policy returns what happens to the region of a memdev of a kind when its
// last grant goes by release, with the regions of other memdevs above its
// regions now (regionsAbove); Keep of a kind that it does not know.
func Policy(release Release, kind Kind, regionsAbove []string) Decision {
	i := slices.IndexFunc(PolicyRows, func(p PolicyRow) bool { return p.Kind == kind })
	if i < 0 {
		return Keep
	}
	return PolicyRows[i].Decision(release, len(regionsAbove) > 0)
}

// kindOf returns the row of the release policy of a memdev of the source
// whose grants ask as asked; shared tells whether a local memdev is
// configured shared now, established whether its region is set up (and
// zeroed, of a shared memdev) or kept by an earlier release.
func kindOf(asked state.Asked, shared, established bool) Kind {
	switch k := asked.Kind; {
	case k == state.KindPoolPersistent:
		return KindPoolPersistent
	case k == state.KindPoolShared:
		return KindPoolShared
	case k == state.KindPool:
		return KindPool
	case k == state.KindLocalPersistent && established:
		return KindLocalPersistent
	case k == state.KindLocalPersistent:
		return KindLocalPersistentNew
	case k == state.KindLocalShared && shared && established:
		return KindLocalShared
	case k == state.KindLocalShared && shared:
		return KindLocalSharedNew
	case k == state.KindLocalShared && established:
		return KindLocalSharedUnlisted
	}
	return KindLocal
}

// kindOf returns the row of the release policy of the memdev of the grant g
// now: shared as configured (Options.Shared), its region established or
// new.
func (e *Engine) kindOf(g *state.Grant) Kind {
	return kindOf(g.Asked, e.configuredShared(g.Serial), established(e.records(), g.Serial, g.Asked.Kind.Persistent()))
}

// idleKind returns the row of the release policy of the memdev m without
// grants: the kind of its facts, configured shared now or not (shared),
// its region established or new.
func (e *Engine) idleKind(m *state.Memdev, shared bool) Kind {
	asked := state.Asked{Source: m.Source, Kind: kindOfFacts(m.Facts)}
	return kindOf(asked, shared, established(e.records(), m.Serial, m.Facts.Persistent))
}

// configuredShared tells whether the local memdev with the serial is
// configured shared (Options.Shared).
func (e *Engine) configuredShared(serial state.Serial) bool {
	return e.opts.Shared != nil && e.opts.Shared(serial)
}

// established tells whether the region of ours of the memdev with the
// serial (persistent: its pmem region) is set up (a ram region also
// zeroed) or kept by an earlier release, not one that a Prepare made and
// did not finish setting up.
func established(r *state.Records, serial state.Serial, persistent bool) bool {
	if persistent {
		reg := ownRegion(r, serial, state.RegionPmem)
		return reg != nil && (reg.State == state.RegionKept || reg.SetUp)
	}
	reg := ownRegion(r, serial, state.RegionRAM)
	return reg != nil && (reg.State == state.RegionKept || (reg.SetUp && reg.Zeroed))
}

// decision returns what the release does with the region of the memdev of
// the last grant g (Policy), and the regions of other memdevs above the
// regions of ours of a recovery; decoders that cannot be read count as a
// region above.
func (e *Engine) decision(g *state.Grant, memdev string, release Release) (Decision, []string) {
	var above []string
	if r := e.records(); release == ReleaseRecovery && hasOwnRegion(r, r.Memdev(g.Serial)) {
		var err error
		if above, err = e.k.RegionsAbove(memdev); err != nil {
			above = []string{"(" + err.Error() + ")"}
		}
	}
	return Policy(release, e.kindOf(g), above), above
}
