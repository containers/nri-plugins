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

package dra

import (
	"context"
	"slices"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// Preparation: the commits of the record of a claim as one driver prepares
// it (state.Preparation), the record that kubelet's prepared flag of the
// claim refers to.
//
// Identity: the claim UID and the driver (cxl.nri.io or the pool driver),
// one record per pair, with a result per allocation result of the driver.
//
// States: state.PreparationState. A Prepare commits the record PREPARING
// with every result (the intent) before the engine touches a device,
// PREPARED once the grants are realised and the CDI spec is written; KEPT
// with the results of the grants that a failed Prepare's rollback kept, or
// removed. An Unprepare commits UNPREPARING before it revokes and releases,
// and removes the record once every grant is released.
//
// Transitions: state.PreparationTable (docs/memory/cxl/states.md), applied
// by fsm.Apply in the commits below; a transition not in it is refused.
//
// Invariants: those of state.Preparation; a PREPARED preparation's CDI
// devices are in the CDI spec of its driver.
//
// Persistence: every commit is durable, bounded by commitTimeout and not by
// the request's context, and done before kubelet gets the response.
//
// Locks and waits: the kubelet helper serializes the Prepares and
// Unprepares of a driver; the store serializes the commits.

// commitTimeout bounds the wait of a commit of the records of the claims
// in the queue of the store.
var commitTimeout = time.Minute

// commit applies fn to the document of the store in one durable commit.
// commitTimeout, not ctx, bounds its wait in the queue.
func commit(ctx context.Context, st *store.Store, fn func(doc *store.Document) error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	return st.Commit(cctx, store.Durable, fn)
}

// preparing commits the preparation of the claim uid of the driver
// PREPARING with its results and the lease of a companion claim ("" for
// another): a new one, a Prepare again of a claim whose Prepare did not
// finish or that its rollback kept, or of a claim of another boot, whose
// grants are dropped in the same commit.
func preparing(doc *store.Document, driver, uid, claimName, lease string, results []state.Result) error {
	prep := doc.Preparation(driver, uid)
	if prep == nil {
		doc.Preparations = append(doc.Preparations, state.Preparation{UID: uid, Driver: driver, State: state.PreparationNone})
		prep = &doc.Preparations[len(doc.Preparations)-1]
	}
	if prep.State == state.PreparationStale {
		// nothing of another boot is left of its grants
		for _, res := range prep.Results {
			if res.Grant != nil {
				dropGrant(&doc.Records, *res.Grant)
			}
		}
	}
	prep.ClaimName, prep.Results, prep.Lease = claimName, slices.Clone(results), lease
	return fsm.Apply(&state.PreparationTable, &prep.State, state.PreparationPreparing, state.TriggerPrepare)
}

// prepared commits the preparation of the claim uid of the driver, whose
// devices are realised and whose CDI devices are written, PREPARED.
func prepared(doc *store.Document, driver, uid string) error {
	prep := doc.Preparation(driver, uid)
	if prep == nil {
		return errNoRecord(uid)
	}
	return fsm.Apply(&state.PreparationTable, &prep.State, state.PreparationPrepared, state.TriggerPrepare)
}

// restored commits a PREPARED_UNRESTORED preparation of the claim uid of
// the driver whose grants are REALISED again PREPARED; nothing for another.
func restored(doc *store.Document, driver, uid string) error {
	cur := doc.Preparation(driver, uid)
	if cur == nil || cur.State != state.PreparationUnrestored || !doc.AllRealised(cur) {
		return nil
	}
	return fsm.Apply(&state.PreparationTable, &cur.State, state.PreparationPrepared, state.TriggerPrepare)
}

// failedCommit commits the preparation of the claim uid of the driver whose
// Prepare failed after its rollback: KEPT with the results of its grants
// that are left (the regions that the rollback kept for the claim; its
// Unprepare releases them, its Prepare again reuses them), else removed.
// It returns whether the preparation is KEPT.
func failedCommit(doc *store.Document, driver, uid string) (bool, error) {
	prep := doc.Preparation(driver, uid)
	if prep == nil {
		return false, nil
	}
	prep.Results = slices.DeleteFunc(prep.Results, func(res state.Result) bool {
		return res.Grant == nil || doc.Grant(*res.Grant) == nil
	})
	if len(prep.Results) > 0 {
		return true, fsm.Apply(&state.PreparationTable, &prep.State, state.PreparationKept, state.TriggerPrepare)
	}
	return false, remove(doc, driver, uid, state.TriggerPrepare)
}

// unpreparing commits a prepared preparation of the claim uid of the
// driver UNPREPARING; nothing for one that is UNPREPARING already.
func unpreparing(doc *store.Document, driver, uid string) error {
	prep := doc.Preparation(driver, uid)
	if prep == nil {
		return errNoRecord(uid)
	}
	if prep.State == state.PreparationUnpreparing {
		return nil
	}
	return fsm.Apply(&state.PreparationTable, &prep.State, state.PreparationUnpreparing, state.TriggerUnprepare)
}

// remove removes the preparation of the claim uid of the driver (->
// NONE) on the trigger; nothing for none.
func remove(doc *store.Document, driver, uid string, trigger state.Trigger) error {
	prep := doc.Preparation(driver, uid)
	if prep == nil {
		return nil
	}
	if err := fsm.Apply(&state.PreparationTable, &prep.State, state.PreparationNone, trigger); err != nil {
		return err
	}
	doc.Preparations = slices.DeleteFunc(doc.Preparations, func(x state.Preparation) bool {
		return x.Driver == driver && x.UID == uid
	})
	return nil
}

// stays checks that the table has the row of a step that keeps the state of
// a preparation on the trigger, without a commit.
func stays(prep *state.Preparation, trigger state.Trigger) error {
	s := prep.State
	return fsm.Apply(&state.PreparationTable, &s, s, trigger)
}

// dropGrant removes the record of a grant of another boot and its memdev's
// reference to it.
func dropGrant(r *state.Records, ref state.GrantRef) {
	r.Grants = slices.DeleteFunc(r.Grants, func(g state.Grant) bool { return g.Ref == ref })
	for i := range r.Memdevs {
		r.Memdevs[i].Grants = slices.DeleteFunc(r.Memdevs[i].Grants, func(o state.GrantRef) bool { return o == ref })
	}
}

// grantOf returns the grant of a result, nil if it has none.
func grantOf(r *state.Records, res *state.Result) *state.Grant {
	if res.Grant == nil {
		return nil
	}
	return r.Grant(*res.Grant)
}

// grantRefs returns the refs of the grants of a preparation that the store
// has.
func grantRefs(r *state.Records, prep *state.Preparation) []state.GrantRef {
	var refs []state.GrantRef
	for _, res := range prep.Results {
		if g := grantOf(r, &res); g != nil {
			refs = append(refs, g.Ref)
		}
	}
	return refs
}

// unrestoredMount returns the path of the node mount of a grant that the
// start did not mount again.
func unrestoredMount(r *state.Records, g *state.Grant) string {
	if reg := r.Region(g.Got.Region); reg != nil && g.Got.Mount != "" {
		if m := reg.Mount(g.Got.Mount); m != nil {
			return m.Path
		}
	}
	return g.Intent.MountPath
}

// cutNUMANodes returns the NUMA nodes of the grants of mode numa of a
// preparation whose Prepare did not finish: the node of the grant, else of
// its region.
func cutNUMANodes(r *state.Records, prep *state.Preparation) []int {
	var nodes []int
	for _, res := range prep.Results {
		g := grantOf(r, &res)
		if g == nil || g.Asked.Mode != state.ModeNUMA {
			continue
		}
		node := state.NUMANodeOf(g.Got.NUMANode)
		if node < 0 {
			region := g.Got.Region
			if region == "" {
				region = g.Intent.Region
			}
			if reg := r.Region(region); reg != nil {
				node = state.NUMANodeOf(reg.NUMANode)
			}
		}
		if node >= 0 {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// NUMANodes returns the NUMA nodes on which the claims of the drivers have
// memory that the engine onlined: the grants in mode numa of prepared
// claims, and of claims whose Prepare did not finish (a numa companion kept
// at the start), sorted.
func NUMANodes(r *state.Records, drivers ...string) []int {
	var nodes []int
	for i := range r.Preparations {
		prep := &r.Preparations[i]
		if !slices.Contains(drivers, prep.Driver) {
			continue
		}
		switch {
		case prep.Prepared():
			for _, res := range prep.Results {
				if g := grantOf(r, &res); g != nil && g.Asked.Mode == state.ModeNUMA && state.NUMANodeOf(g.Got.NUMANode) >= 0 {
					nodes = append(nodes, *g.Got.NUMANode)
				}
			}
		case prep.State == state.PreparationPreparing || prep.State == state.PreparationKept:
			nodes = append(nodes, cutNUMANodes(r, prep)...)
		}
	}
	slices.Sort(nodes)
	return slices.Compact(nodes)
}
