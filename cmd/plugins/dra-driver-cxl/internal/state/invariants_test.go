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

package state_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state/statetest"
)

func TestFixtureIsConsistent(t *testing.T) {
	r := statetest.Records()
	for _, v := range state.Check(&r) {
		t.Errorf("%s", v)
	}
}

// Records that may look broken but that the invariants allow.
func TestAllowedRecords(t *testing.T) {
	cut := func(s state.PreparationState) func(r *state.Records) {
		return func(r *state.Records) {
			ref := state.GrantRef{Claim: "cut"}
			r.Preparations = append(r.Preparations, state.Preparation{UID: "cut", Driver: statetest.PoolDriver,
				ClaimName: "demo/c-cut", State: s, Results: []state.Result{{Kind: state.ResultGrant, Request: "mem", Grant: &ref}}})
		}
	}
	for _, tc := range []struct {
		name   string
		change func(r *state.Records)
	}{
		// not journaled yet, or released before the front-end's commit
		{name: "a grant result of a PREPARING preparation without its grant", change: cut(state.PreparationPreparing)},
		{name: "a grant result of an UNPREPARING preparation without its grant", change: cut(state.PreparationUnpreparing)},
		// the recovery released the grants of a cut Prepare, then a reboot
		// made it stale
		{name: "a grant result of a PREPARED_STALE preparation without its grant", change: cut(state.PreparationStale)},
		{
			name: "the whole dax device of a region of an IDLE_KEPT memdev without a user",
			change: func(r *state.Records) {
				r.Grants = slices.DeleteFunc(r.Grants, func(g state.Grant) bool { return g.Serial == statetest.SerialShared })
				r.Preparation(statetest.LocalDriver, statetest.ClaimMixed).State = state.PreparationUnpreparing
				r.Region("region1").DaxDevices[0].Users = nil
				m := r.Memdev(statetest.SerialShared)
				m.Grants = nil
				m.State = state.MemdevIdleKept
			},
		},
		{
			name:   "a FAILED lease that keeps its lend",
			change: func(r *state.Records) { r.Lease(statetest.LeaseNUMA).State = state.LeaseFailed },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := statetest.Records()
			tc.change(&r)
			for _, v := range state.Check(&r) {
				t.Errorf("%s", v)
			}
		})
	}
}

func TestInvariantViolations(t *testing.T) {
	ref := func(claim string, n int) state.GrantRef { return state.GrantRef{Claim: claim, Result: n} }
	for _, tc := range []struct {
		name      string
		change    func(r *state.Records)
		invariant string
		text      string
	}{
		{
			name: "a stored record in a state without a record",
			change: func(r *state.Records) {
				r.Preparation(statetest.LocalDriver, statetest.ClaimDax).State = state.PreparationNone
			},
			invariant: "records", text: `state "NONE"`,
		},
		{
			name:      "a grant in REQUESTED is stored",
			change:    func(r *state.Records) { r.Grant(ref(statetest.ClaimKept, 0)).State = state.GrantRequested },
			invariant: "records", text: `state "REQUESTED"`,
		},
		{
			name:      "two records of one memdev",
			change:    func(r *state.Records) { r.Memdevs = append(r.Memdevs, r.Memdevs[0].Clone()) },
			invariant: "records", text: "more than one record",
		},
		{
			name: "a region of ours that its memdev does not reference",
			change: func(r *state.Records) {
				r.Memdev(statetest.SerialPoolNUMA).Regions = nil
			},
			invariant: "records", text: "does not reference it",
		},
		{
			name: "a memdev references a foreign region",
			change: func(r *state.Records) {
				m := r.Memdev(statetest.SerialLocal)
				m.Regions = append(m.Regions, "region5")
			},
			invariant: "records", text: "is not one of ours on it",
		},
		{
			name:      "a foreign region owned by us",
			change:    func(r *state.Records) { r.Region("region5").Owner = state.OwnerOurs },
			invariant: "records", text: "with owner",
		},
		{
			name: "a grant whose kind is of another source",
			change: func(r *state.Records) {
				r.Grant(state.GrantRef{Claim: statetest.ClaimDax, Result: 0}).Asked.Kind = state.KindPool
			},
			invariant: "records", text: "kind pool of source local",
		},
		{
			name: "a grant that no preparation has",
			change: func(r *state.Records) {
				p := r.Preparation(statetest.LocalDriver, statetest.ClaimDax)
				p.Results = nil
			},
			invariant: "records", text: "belongs to 0 preparation results",
		},
		{
			name: "a grant that its memdev does not list",
			change: func(r *state.Records) {
				r.Memdev(statetest.SerialPoolDax).Grants = nil
			},
			invariant: "records", text: "does not list it",
		},
		{
			name: "a dax device that does not list its grant",
			change: func(r *state.Records) {
				r.Region("region0").DaxDevice("dax0.2").Users = []state.GrantRef{ref(statetest.ClaimNUMA, 0)}
			},
			invariant: "records", text: "does not use it",
		},
		{
			name: "a PREPARED preparation with a grant not realised",
			change: func(r *state.Records) {
				r.Grant(ref(statetest.ClaimDax, 0)).State = state.GrantReleasing
			},
			invariant: "records", text: "PREPARED with grant",
		},
		{
			name:      "a lease claim result without its lease claim",
			change:    func(r *state.Records) { r.LeaseClaims = nil },
			invariant: "lease claims", text: "lease claim " + statetest.ClaimLease + " has no record",
		},
		{
			name: "a grant result of a PREPARED preparation without its grant",
			change: func(r *state.Records) {
				r.Grants = slices.DeleteFunc(r.Grants, func(g state.Grant) bool { return g.Ref == ref(statetest.ClaimDax, 0) })
			},
			invariant: "records", text: "grant " + statetest.ClaimDax + "/0 has no record",
		},
		{
			name:      "a lease that lends while pending",
			change:    func(r *state.Records) { r.Lease(statetest.LeaseNUMA).State = state.LeasePending },
			invariant: "records", text: "lends while PENDING",
		},
		{
			name: "a region of ours still being created between requests",
			change: func(r *state.Records) {
				reg := r.Region("region4")
				reg.State = state.RegionNoted
			},
			invariant: "users", text: "NOTED between requests",
		},
		{
			name: "a result of kind grant without its grant",
			change: func(r *state.Records) {
				r.Preparation(statetest.LocalDriver, statetest.ClaimDax).Results[0].Grant = nil
			},
			invariant: "records", text: "has 0 parts",
		},
		{
			name: "a dax device without a realised user",
			change: func(r *state.Records) {
				reg := r.Region("region0")
				reg.DaxDevices = append(reg.DaxDevices, state.DaxDevice{Name: "dax0.9", State: state.DaxUnbound, Size: 1 << 27})
			},
			invariant: "users", text: "dax device dax0.9 has no realised or releasing user",
		},
		{
			name: "a dax device whose only user is journaled",
			change: func(r *state.Records) {
				r.Grant(ref(statetest.ClaimNUMA, 0)).State = state.GrantJournaled
				r.Preparation(statetest.LocalDriver, statetest.ClaimNUMA).State = state.PreparationPreparing
			},
			invariant: "users", text: "dax device dax0.1 has no realised",
		},
		{
			name: "a region of ours on a memdev without a record",
			change: func(r *state.Records) {
				r.Regions = append(r.Regions, state.Region{
					NotedName: "region9", Mode: state.RegionRAM, Targets: []state.Serial{0x99},
					Owner: state.OwnerOurs, State: state.RegionNoted,
				})
			},
			invariant: "journaled", text: "target memdev 0x99 has no record",
		},
		{
			name: "a grant whose region is not its intent",
			change: func(r *state.Records) {
				r.Grant(ref(statetest.ClaimNUMA, 0)).Intent.Region = ""
			},
			invariant: "journaled", text: `its intent names ""`,
		},
		{
			name: "a lend without its intent",
			change: func(r *state.Records) {
				r.Grant(ref(statetest.ClaimCompanion, 0)).Intent.Lend = nil
			},
			invariant: "journaled", text: "is not its intent",
		},
		{
			name: "a mount that no intent names",
			change: func(r *state.Records) {
				r.Grant(ref(statetest.ClaimFs, 0)).Intent.MountPath = ""
			},
			invariant: "journaled", text: "is named by no intent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := statetest.Records()
			tc.change(&r)
			found := false
			for _, v := range state.Check(&r) {
				if v.Invariant == tc.invariant && strings.Contains(v.Text, tc.text) {
					found = true
				}
			}
			if !found {
				t.Errorf("no %s violation %q in %v", tc.invariant, tc.text, state.Check(&r))
			}
		})
	}
}

func TestCheckStates(t *testing.T) {
	r := statetest.Records()
	if err := state.CheckStates(&r); err != nil {
		t.Errorf("CheckStates of the fixture: %v", err)
	}
	r.Region("region0").DaxDevice("dax0.1").State = "ONLINE"
	err := state.CheckStates(&r)
	if err == nil || !strings.Contains(err.Error(), `region region0 dax device dax0.1: state "ONLINE"`) {
		t.Errorf("CheckStates: %v", err)
	}
}
