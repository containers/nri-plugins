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
	"testing"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// checkTable checks the data of a transition table and returns its name:
// every row has a from state, a trigger of Triggers and When or Effects,
// its states are states of the table and a row that stays has no To; no
// two rows have the same transition and trigger; every state of the table
// is in a row; no row has a transition that the table refuses.
func checkTable[S comparable](t *testing.T, tb *fsm.Table[S]) string {
	t.Helper()
	name := tb.Object + "." + tb.Field
	var zero S
	inRows := map[S]bool{}
	seen := map[string]int{}
	for i, r := range tb.Rows {
		row := fmt.Sprintf("%s row %d", name, i)
		if len(r.From) == 0 || len(r.Triggers) == 0 {
			t.Errorf("%s: no from state or no trigger", row)
		}
		if r.When == "" && r.Effects == "" {
			t.Errorf("%s: neither When nor Effects documented", row)
		}
		if r.Stays && r.To != zero {
			t.Errorf("%s: stays and has To %v", row, r.To)
		}
		if !r.Stays {
			if !slices.Contains(tb.States, r.To) {
				t.Errorf("%s: To %v is not a state of the table", row, r.To)
			}
			inRows[r.To] = true
		}
		for _, tr := range r.Triggers {
			if !slices.Contains(Triggers, tr) {
				t.Errorf("%s: trigger %q is not in Triggers", row, tr)
			}
		}
		for _, from := range r.From {
			if !slices.Contains(tb.States, from) {
				t.Errorf("%s: From %v is not a state of the table", row, from)
			}
			inRows[from] = true
			to := r.To
			if r.Stays {
				to = from
			}
			for _, tr := range r.Triggers {
				key := fmt.Sprintf("%v -> %v on %s", from, to, tr)
				if j, dup := seen[key]; dup {
					t.Errorf("%s: %s is also row %d", row, key, j)
				}
				seen[key] = i
				for _, ref := range tb.Refused {
					if slices.Contains(ref.From, from) && (len(ref.To) == 0 || slices.Contains(ref.To, to)) &&
						!slices.Contains(ref.Except, tr) {
						t.Errorf("%s: %s is refused: %s", row, key, ref.Why)
					}
				}
			}
		}
	}
	for _, s := range tb.States {
		if !inRows[s] {
			t.Errorf("%s: state %v is in no row", name, s)
		}
	}
	return name
}

// TestTables checks every transition table of the records, and that every
// trigger is in a row.
func TestTables(t *testing.T) {
	checkTable(t, &PreparationTable)
	checkTable(t, &GrantTable)
	checkTable(t, &BindTable)
	checkTable(t, &MemdevTable)
	checkTable(t, &RegionTable)
	checkTable(t, &RegionSetUpTable)
	checkTable(t, &DaxDeviceTable)
	checkTable(t, &MountTable)
	checkTable(t, &ProvenanceTable)
	checkTable(t, &LeaseTable)
	checkTable(t, &LeaseClaimTable)
	used := map[string]bool{}
	for _, tt := range []fsm.TableText{
		PreparationTable.Text(), GrantTable.Text(), BindTable.Text(), MemdevTable.Text(), RegionTable.Text(),
		RegionSetUpTable.Text(), DaxDeviceTable.Text(), MountTable.Text(), ProvenanceTable.Text(),
		LeaseTable.Text(), LeaseClaimTable.Text(),
	} {
		for _, r := range tt.Rows {
			for _, tr := range r.Triggers {
				used[tr] = true
			}
		}
	}
	for _, tr := range Triggers {
		if !used[string(tr)] {
			t.Errorf("trigger %s is in no row", tr)
		}
	}
}
