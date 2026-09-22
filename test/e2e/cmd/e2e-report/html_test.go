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

import (
	"testing"
)

// covered is a run which reached percent of the logic of one plugin.
func covered(name, plugin string, percent Percent) *Run {
	return &Run{
		Name:    name,
		Verdict: "PASS",
		Counts:  map[string]int{"PASS": 1, "total": 1},
		Coverage: RunCoverage{Policies: map[string]*Coverage{
			plugin: {PluginPercent: &percent},
		}},
	}
}

// TestIndexPageSaysWhenThereIsNoCoverage checks that a run which collected no
// coverage says so, rather than leaving the column of a plugin empty. A run from
// before coverage was collected at all has none, and neither has one whose
// plugins were not built with instrumentation; an empty cell reads as zero
// coverage, or as a report which failed to render.
func TestIndexPageSaysWhenThereIsNoCoverage(t *testing.T) {
	withCoverage := covered("test-2026-09-17-2251", "balloons", 82.5)
	none := &Run{
		Name:    "test-2026-09-15-2113",
		Verdict: "PASS",
		Counts:  map[string]int{"PASS": 1, "total": 1},
	}

	page := newIndexPage([]*Run{withCoverage, none})

	if len(page.Plugins) != 1 || page.Plugins[0] != "balloons" {
		t.Fatalf("the columns are %v, expected balloons alone", page.Plugins)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("%d rows, expected 2", len(page.Runs))
	}

	if got := page.Runs[0].Percents; len(got) != 1 || got[0] != "82.5%" {
		t.Errorf("the run which collected coverage reads %v, expected [82.5%%]", got)
	}
	if got := page.Runs[1].Percents; len(got) != 1 || got[0] != noCoverage {
		t.Errorf("the run which collected none reads %v, expected [%s]", got, noCoverage)
	}
}

// TestIndexPageSaysWhenAPolicyHasNoCoverage checks that a run which collected
// coverage for one policy and not another says so for the one it missed, rather
// than reading as though that policy covered nothing.
func TestIndexPageSaysWhenAPolicyHasNoCoverage(t *testing.T) {
	balloons := covered("test-2026-09-17-2251", "balloons", 82.5)
	topology := covered("test-2026-09-16-0910", "topology-aware", 70.3)

	page := newIndexPage([]*Run{balloons, topology})

	if len(page.Plugins) != 2 {
		t.Fatalf("the columns are %v, expected two", page.Plugins)
	}

	// The columns are sorted, so balloons comes first.
	if got := page.Runs[0].Percents; len(got) != 2 || got[0] != "82.5%" || got[1] != noCoverage {
		t.Errorf("the balloons run reads %v, expected [82.5%% %s]", got, noCoverage)
	}
	if got := page.Runs[1].Percents; len(got) != 2 || got[0] != noCoverage || got[1] != "70.3%" {
		t.Errorf("the topology-aware run reads %v, expected [%s 70.3%%]", got, noCoverage)
	}
}
