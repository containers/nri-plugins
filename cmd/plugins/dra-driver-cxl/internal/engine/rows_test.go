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
	"flag"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// engineTables are the tables of the records that the engine drives; of
// the table of the preparations it drives the rows of the recovery
// (engineRow).
var engineTables = []fsm.TableText{
	state.GrantTable.Text(),
	state.BindTable.Text(),
	state.MemdevTable.Text(),
	state.RegionTable.Text(),
	state.RegionSetUpTable.Text(),
	state.DaxDeviceTable.Text(),
	state.MountTable.Text(),
	state.PreparationTable.Text(),
}

// engineRow tells whether the engine drives a row of one of engineTables:
// a row of the table of the preparations only on recover, the front-end
// drives the others.
func engineRow(t fsm.TableText, r fsm.RowText) bool {
	return t.Object != "Preparation" || slices.Contains(r.Triggers, string(state.TriggerRecover))
}

// notDriven are the rows of engineTables that the engine does not apply,
// by fsm.TableText.RowName, with the reason.
var notDriven = map[string]string{
	"Grant.State JOURNALED,REALISING,REALISED,REALISED_UNRESTORED,RELEASING -> (unchanged) on load": "the store's load " +
		"of a document of another boot",
	"Grant.State JOURNALED,REALISING,REALISED,REALISED_UNRESTORED,RELEASING -> NONE on load": "the store's load of " +
		"a document of another boot",
	"Memdev.State ACTIVE,IDLE_KEPT,GONE -> NONE on load": "the store's load of a document of another boot",
	"Region.State NOTED,OWNED,KEPT,HOLLOW_OWNED,FOREIGN -> NONE on load": "the store's load of a document of " +
		"another boot",
	"Preparation.State UNPREPARING -> (unchanged) on recover": "a start leaves an UNPREPARING preparation " +
		"as it is, with no commit",
}

// TestMain counts the rows that the tests apply and, when every test ran,
// fails if a row of engineTables that the engine drives was applied by no
// test, or if a test applied a row of notDriven; with -v it lists the rows
// that the engine does not drive.
func TestMain(m *testing.M) {
	rows := fsm.CountRows()
	code := m.Run()
	if f := flag.Lookup("test.run"); code == 0 && (f == nil || f.Value.String() == "") {
		code = reportRows(rows)
	}
	os.Exit(code)
}

// reportRows prints the rows of engineTables that the engine drives and no
// test applied and returns 1 if one of them is not in notDriven, or if a
// row of notDriven was applied.
func reportRows(rows *fsm.Rows) int {
	code := 0
	for _, t := range engineTables {
		for i := range t.Rows {
			if name := t.RowName(i); notDriven[name] != "" && rows.Applied(t.Object, t.Field, i) {
				fmt.Printf("FAIL: row %d %s is applied by a test of the engine: remove it from notDriven\n", i, name)
				code = 1
			}
		}
		for _, i := range rows.Unapplied(t) {
			if !engineRow(t, t.Rows[i]) {
				continue
			}
			name := t.RowName(i)
			why, ok := notDriven[name]
			switch {
			case !ok:
				fmt.Printf("FAIL: row %d %s is applied by no test of the engine\n", i, name)
				code = 1
			case testing.Verbose():
				fmt.Printf("rows: %s: not driven by the engine: %s\n", name, why)
			}
		}
	}
	return code
}
