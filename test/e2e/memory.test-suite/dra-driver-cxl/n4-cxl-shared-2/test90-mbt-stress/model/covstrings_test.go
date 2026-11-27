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

// covStrings (planner.go) returns the strings that the coverage functions
// compute for a step.

import (
	"math/rand"
	"slices"
	"testing"

	m "github.com/askervin/gofmbt/gofmbt"
)

// refStepStrings are the C1, C2 and C4 strings of a step as the coverage
// functions compute them, without covStrings.
func refStepStrings(mm *mbtModel, cov Coverage, st *m.Step) []string {
	a := mm.acts[mm.actIndex(st)]
	from, to := st.StartState().(*State), st.EndState().(*State)
	var out []string
	if w := cov.W[0]; w > 0 {
		out = append(out, c1Strings(mm.cfg, a, w)...)
		out = append(out, weigh(w, "", leaseChances(to))...)
	}
	if w := cov.W[1]; w > 0 {
		out = append(out, weigh(w, "C2.", faultContext(from, a))...)
		if a.Verb == VSettle {
			out = append(out, weigh(w, "C2.", memsStrings(to))...)
		}
	}
	if w := cov.W[3]; w > 0 {
		out = append(out, weigh(w, "C4.", to.abstract())...)
	}
	return out
}

// refPairStrings are the C3 strings: gofmbt's
// CoverActionFormatCombinations(2) for weight 1.
func refPairStrings(mm *mbtModel, cov Coverage, prev *m.Step, st *m.Step) []string {
	w := cov.W[2]
	out := weigh(w, "C3.", []string{mm.formats[mm.actIndex(st)]})
	if prev != nil {
		out = append(append([]string(nil), out...), weigh(w, "C3.", []string{mm.formats[mm.actIndex(prev)] + "\x00" + mm.formats[mm.actIndex(st)]})...)
	}
	return out
}

func sorted(xs []string) []string {
	ys := append([]string(nil), xs...)
	slices.Sort(ys)
	return ys
}

func TestCovStrings(t *testing.T) {
	for _, mix := range []string{"C1x3,C2x2,C3", "all", "C1,C3", "C2x2,C3x2,C4x2", "C1x2"} {
		cov, err := parseCoverage(mix)
		if err != nil {
			t.Fatal(err)
		}
		for _, online := range []bool{false, true} {
			cfg := defaultConfig()
			cfg.PlanStates = maxPlanStates
			cfg.Online = online
			if err := cfg.loadProfile("full"); err != nil {
				t.Fatal(err)
			}
			mm := newMbtModel(cfg)
			cs := newCovStrings(mm, cov)
			rng := rand.New(rand.NewSource(int64(len(mix))))
			root := randomState(mm, 10+rng.Intn(30), rng)
			w := newPlanWalk(mm, root)
			for round := 0; round < 3; round++ {
				// round 0 fills the caches, round 1 reads them,
				// round 2 is outside a plan (nothing cached)
				switch round {
				case 0:
					cs.startPlan(w)
				case 2:
					cs.endPlan()
				}
				n := 0
				for _, s1 := range w.StepsFrom(root) {
					for _, s2 := range w.StepsFrom(s1.EndState()) {
						n++
						for _, x := range []struct {
							prev, st *m.Step
						}{{nil, s1}, {s1, s2}} {
							if got, want := sorted(cs.stepStrings(x.st)), sorted(refStepStrings(mm, cov, x.st)); !slices.Equal(got, want) {
								t.Fatalf("%s: %s: step strings\n%q\nwant\n%q", mix, x.st.Action(), got, want)
							}
							if !cov.on(3) {
								continue
							}
							prev := -1
							if x.prev != nil {
								prev = mm.actIndex(x.prev)
							}
							if got, want := sorted(cs.pairStrings(prev, mm.actIndex(x.st))), sorted(refPairStrings(mm, cov, x.prev, x.st)); !slices.Equal(got, want) {
								t.Fatalf("%s: %s: pair strings\n%q\nwant\n%q", mix, x.st.Action(), got, want)
							}
						}
					}
				}
				if n == 0 {
					t.Fatalf("no steps")
				}
			}
		}
	}
}
