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
	"flag"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/fsm"
)

// notDriven are the rows of the table of the preparations that the
// front-end does not apply, by fsm.TableText.RowName, with the reason. The tests may apply them through the store or the engine.
var notDriven = map[string]string{
	"Preparation.State PREPARING -> (unchanged) on recover": "the engine's recovery of a cut Prepare",
	"Preparation.State PREPARED -> PREPARED_UNRESTORED on recover": "the engine's recovery of a node mount " +
		"that does not mount again",
	"Preparation.State PREPARED_UNRESTORED -> (unchanged) on recover": "the engine's recovery of a node " +
		"mount that does not mount again",
	"Preparation.State PREPARING,PREPARED,PREPARED_UNRESTORED,PREPARED_STALE,UNPREPARING,KEPT -> " +
		"PREPARED_STALE on load": "the store's load of the records of another boot",
	"Preparation.State UNPREPARING -> (unchanged) on recover": "a start leaves an UNPREPARING preparation " +
		"as it is, with no commit",
	"Preparation.State KEPT -> (unchanged) on recover": "the engine's recovery of a kept region",
	"Preparation.State KEPT -> NONE on recover":        "the engine's recovery of a kept region",
}

// TestMain counts the rows of the table of the preparations that the tests
// apply and, when every test ran, fails if a row that the front-end
// drives was applied by no test; with -v it lists the rows that the
// front-end does not drive.
func TestMain(m *testing.M) {
	rows := fsm.CountRows()
	code := m.Run()
	if f := flag.Lookup("test.run"); code == 0 && (f == nil || f.Value.String() == "") {
		code = reportRows(rows)
	}
	os.Exit(code)
}

// frontEndTriggers are the triggers of the rows that the front-end drives.
var frontEndTriggers = []string{string(state.TriggerPrepare), string(state.TriggerUnprepare)}

// reportRows prints the rows of the table of the preparations that the
// front-end drives and no test applied, and returns 1 if there is one, or
// if notDriven names a row that is not in the table or one that the
// front-end may drive.
func reportRows(rows *fsm.Rows) int {
	t := state.PreparationTable.Text()
	code := 0
	names := map[string]bool{}
	for i, r := range t.Rows {
		name := t.RowName(i)
		names[name] = true
		why, listed := notDriven[name]
		driven := slices.ContainsFunc(r.Triggers, func(tr string) bool { return slices.Contains(frontEndTriggers, tr) })
		applied := rows.Applied(t.Object, t.Field, i)
		switch {
		case listed && driven:
			fmt.Printf("FAIL: row %d %s has a trigger of the front-end: remove it from notDriven\n", i, name)
			code = 1
		case !listed && !applied:
			fmt.Printf("FAIL: row %d %s is applied by no test of the front-end\n", i, name)
			code = 1
		case listed && testing.Verbose():
			fmt.Printf("rows: %s: not driven by the front-end (applied in the tests: %v): %s\n", name, applied, why)
		}
	}
	for name := range notDriven {
		if !names[name] {
			fmt.Printf("FAIL: notDriven names %s, which is not a row of %s.%s\n", name, t.Object, t.Field)
			code = 1
		}
	}
	return code
}
