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

package provenance

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// The row counter of state.ProvenanceTable is in this package: the watcher
// is the only writer of Provenance.State, and fsm.CountRows counts the
// Apply calls of one test binary. The scenario tests of internal/dra (the
// publisher over a Watcher) apply the same rows in their own binary, where
// nothing counts them: the tests of this package apply every row that the
// watcher drives by themselves.

// watcherTables are the tables of the records that the watcher drives.
var watcherTables = []fsm.TableText{
	state.ProvenanceTable.Text(),
}

// notDriven are the rows of watcherTables that the watcher does not apply,
// by fsm.TableText.RowName, with the reason. The watcher
// drives every row of the table.
var notDriven = map[string]string{}

// TestMain counts the rows that the tests apply and, when every test ran,
// fails if a row of watcherTables that the watcher drives was applied by no
// test, or if a test applied a row of notDriven; with -v it lists the rows
// that the watcher does not drive.
func TestMain(m *testing.M) {
	rows := fsm.CountRows()
	code := m.Run()
	if f := flag.Lookup("test.run"); code == 0 && (f == nil || f.Value.String() == "") {
		code = reportRows(rows)
	}
	os.Exit(code)
}

// reportRows prints the rows of watcherTables that no test applied and
// returns 1 if one of them is not in notDriven, or if a row of notDriven
// was applied.
func reportRows(rows *fsm.Rows) int {
	code := 0
	for _, t := range watcherTables {
		for i := range t.Rows {
			if name := t.RowName(i); notDriven[name] != "" && rows.Applied(t.Object, t.Field, i) {
				fmt.Printf("FAIL: row %d %s is applied by a test of the watcher: remove it from notDriven\n", i, name)
				code = 1
			}
		}
		for _, i := range rows.Unapplied(t) {
			name := t.RowName(i)
			why, ok := notDriven[name]
			switch {
			case !ok:
				fmt.Printf("FAIL: row %d %s is applied by no test of the watcher\n", i, name)
				code = 1
			case testing.Verbose():
				fmt.Printf("rows: %s: not driven by the watcher: %s\n", name, why)
			}
		}
	}
	return code
}
