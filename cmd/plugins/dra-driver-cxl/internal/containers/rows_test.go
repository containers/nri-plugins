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

package containers

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// TestMain counts the rows of the tables of the containers that the tests
// apply and, when every test ran, fails if a row was applied by no test.
func TestMain(m *testing.M) {
	rows := fsm.CountRows()
	code := m.Run()
	if f := flag.Lookup("test.run"); code == 0 && (f == nil || f.Value.String() == "") {
		code = reportRows(rows, LifecycleTable.Text(), ManagementTable.Text(), ReadinessTable.Text(), ConnTable.Text())
	}
	os.Exit(code)
}

// reportRows prints the rows of the tables that no test applied and
// returns 1 if there is one.
func reportRows(rows *fsm.Rows, tables ...fsm.TableText) int {
	code := 0
	for _, t := range tables {
		for _, i := range rows.Unapplied(t) {
			fmt.Printf("FAIL: row %d %s is applied by no test\n", i, t.RowName(i))
			code = 1
		}
	}
	return code
}

// checkTable checks that every row of a table applies on each of its
// triggers from each of its states, that every state of a row is a state
// of the table, that no row has a transition of a refusal, and that a
// transition of no row is refused with the state unchanged.
func checkTable[S comparable](t *testing.T, table *fsm.Table[S]) {
	t.Helper()
	for i, r := range table.Rows {
		for _, from := range r.From {
			to := r.To
			if r.Stays {
				to = from
			}
			for _, s := range append([]S{to}, r.From...) {
				if !slices.Contains(table.States, s) {
					t.Errorf("%s.%s row %d: state %v is not in the table", table.Object, table.Field, i, s)
				}
			}
			for _, tr := range r.Triggers {
				if !table.Allows(from, to, tr) {
					t.Errorf("%s.%s row %d: %v -> %v on %s not allowed", table.Object, table.Field, i, from, to, tr)
				}
				for _, ref := range table.Refused {
					if slices.Contains(ref.From, from) && (len(ref.To) == 0 || slices.Contains(ref.To, to)) && !slices.Contains(ref.Except, tr) {
						t.Errorf("%s.%s row %d: %v -> %v on %s is refused: %s", table.Object, table.Field, i, from, to, tr, ref.Why)
					}
				}
			}
		}
	}
	for _, from := range table.States {
		for _, to := range table.States {
			for _, tr := range Triggers {
				if table.Allows(from, to, tr) {
					continue
				}
				// a copy: the counter of the rows is not touched
				state := from
				err := fsm.Apply(table, &state, to, tr)
				if !errors.Is(err, fsm.ErrTransition) || state != from {
					t.Errorf("%s.%s: %v -> %v on %s applied: %v", table.Object, table.Field, from, to, tr, err)
				}
			}
		}
	}
}

func TestTables(t *testing.T) {
	checkTable(t, &LifecycleTable)
	checkTable(t, &ManagementTable)
	checkTable(t, &ReadinessTable)
	checkTable(t, &ConnTable)
	for _, tr := range Triggers {
		used := false
		for _, table := range []fsm.TableText{LifecycleTable.Text(), ManagementTable.Text(), ReadinessTable.Text(), ConnTable.Text()} {
			for _, r := range table.Rows {
				used = used || slices.Contains(r.Triggers, string(tr))
			}
		}
		if !used {
			t.Errorf("trigger %s is used by no row", tr)
		}
	}
	// a transition that its table refuses leaves the record as it is
	c := &Container{Lifecycle: LifecycleStarted, Management: ManagementFenced}
	p := &Plugin{}
	p.lifecycle(c, LifecycleCreated, TriggerCreate)
	p.manage(c, ManagementSteered, TriggerCreate)
	if c.Lifecycle != LifecycleStarted || c.Management != ManagementFenced {
		t.Fatalf("record %+v", c)
	}
}
