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
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/askervin/gofmbt/gofmbt"
	"k8s.io/klog/v2"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The flags of TestCoreModel.
var (
	coreSeed     = flag.Int64("core.seed", 1, "core model: the seed of the planner")
	coreSteps    = flag.Int("core.steps", 200, "core model: the number of steps")
	coreDuration = flag.Duration("core.duration", 0, "core model: stop after this time, 0 for no limit")
	coreDepth    = flag.Int("core.depth", 2, "core model: the search depth of the planner")
	coreTeardown = flag.Int("core.teardown", 100, "core model: the steps between teardowns")
	coreReplay   = flag.String("core.replay", "", "core model: run the steps of this steps file instead of planning")
	coreOut      = flag.String("core.out", "", "core model: the directory of the steps file and the log, \"\" for a temporary one")
	coreVerbose  = flag.Int("core.v", 0, "core model: the klog verbosity of the log")
	coreKnown    = flag.String("core.known", "all", "core model: the findings not fixed yet whose requests are not generated, "+
		"comma-separated names, \"all\" or \"\"")
)

// The engine and the store over the fake kernel, driven by the core model
// (core_model_test.go): every step's observation equals one of the states
// that the model predicts from the design, the invariants hold, and every
// teardown leaves nothing. -core.seed, -core.steps, -core.duration,
// -core.depth plan a run; -core.replay runs a steps file again.
func TestCoreModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}
	r := newCoreRun(t, nil)
	if *coreReplay != "" {
		r.replay(*coreReplay)
	} else {
		r.generate()
	}
	r.summary()
}

// coreRun is a run of the core model.
type coreRun struct {
	t       *testing.T
	x       *coreExec
	actions []*coreAction
	byAct   map[*gofmbt.Action]*coreAction
	byName  map[string]*coreAction
	model   *gofmbt.Model
	out     string
	steps   *os.File
	log     *os.File
	state   *coreState
	n       int
	begin   time.Time
	// adopted counts the steps by the alternative the observation picked.
	adopted map[string]int
	// planned is the time spent planning.
	planned time.Duration
}

// newCoreRun returns a run on a new node, its steps file and log in the
// output directory; with newFront the requests go through the front-end
// that it returns.
func newCoreRun(t *testing.T, newFront func(f *fake.Fake, dir string) CoreFront) *coreRun {
	clear(coreAvoid)
	for name := range coreFindings {
		if *coreKnown == "all" || slices.Contains(strings.Split(*coreKnown, ","), name) {
			coreAvoid[name] = true
		}
	}
	r := &coreRun{t: t, actions: coreActions(), byAct: map[*gofmbt.Action]*coreAction{}, byName: map[string]*coreAction{},
		out: *coreOut, state: newCoreState(), begin: time.Now(), adopted: map[string]int{}}
	for _, ca := range r.actions {
		r.byAct[ca.a] = ca
		r.byName[ca.a.String()] = ca
	}
	r.model = newCoreModel(r.actions)
	if r.out == "" {
		r.out = t.TempDir()
	}
	if err := os.MkdirAll(r.out, 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	if r.log, err = os.Create(filepath.Join(r.out, "run.log")); err != nil {
		t.Fatal(err)
	}
	if r.steps, err = os.Create(filepath.Join(r.out, "steps")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.steps.Close()
		_ = r.log.Close()
	})
	r.captureKlog(r.log)
	r.x = newCoreExec(t, r.log, newFront)
	_, _ = fmt.Fprintf(r.steps, "# core model: seed=%d steps=%d duration=%v depth=%d teardown=%d known=%q replay=%q front=%v\n", *coreSeed,
		*coreSteps, *coreDuration, *coreDepth, *coreTeardown, *coreKnown, *coreReplay, coreFront)
	return r
}

// captureKlog writes the log of klog to w at the verbosity of -core.v
// until the end of the test.
func (r *coreRun) captureKlog(w io.Writer) {
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	_ = fs.Set("logtostderr", "false")
	_ = fs.Set("alsologtostderr", "false")
	_ = fs.Set("stderrthreshold", "FATAL")
	_ = fs.Set("v", strconv.Itoa(*coreVerbose))
	klog.SetOutput(w)
	r.t.Cleanup(func() {
		klog.Flush()
		_ = fs.Set("logtostderr", "true")
		_ = fs.Set("stderrthreshold", "ERROR")
		_ = fs.Set("v", "0")
	})
}

// coverer returns the coverer of a run: every verb (C1), every fault and
// cut in the context of the request that consumes it (C2), and pairs of
// requests and the stack shapes on one host bridge (C3).
func (r *coreRun) coverer() *gofmbt.Coverer {
	c := gofmbt.NewCoverer()
	c.CoverSteps(1, func(p gofmbt.Path) []string {
		ca := r.byAct[p[len(p)-1].Action()]
		if ca.verb == "arm" {
			return []string{"do " + coreArms[ca.arm-1].verb}
		}
		return []string{"do " + ca.a.String()}
	})
	c.CoverSteps(1, func(p gofmbt.Path) []string {
		st := p[len(p)-1]
		s, ca := st.StartState().(*coreState), r.byAct[st.Action()]
		if s.arm == 0 {
			return nil
		}
		arm := &coreArms[s.arm-1]
		if !ca.isRequest() {
			return []string{fmt.Sprintf("fault %s in %s", arm.verb, ca.verb)}
		}
		prep := s.slots[ca.slot].prep
		if prep == "" {
			prep = "NONE"
		}
		return []string{fmt.Sprintf("fault %s in %s of %s from %s", arm.verb, ca.verb, coreSlots[ca.slot].kind, prep)}
	})
	c.CoverSteps(2, func(p gofmbt.Path) []string {
		st := p[len(p)-1]
		s, ca := st.StartState().(*coreState), r.byAct[st.Action()]
		if !ca.isRequest() {
			return nil
		}
		md := coreSlots[ca.slot].md
		var out []string
		if s.regionsAbove(md) {
			out = append(out, fmt.Sprintf("below: %s %s", ca.verb, coreSlots[ca.slot].kind))
		}
		if len(p) < 2 {
			return out
		}
		prev := r.byAct[p[len(p)-2].Action()]
		if !prev.isRequest() {
			return out
		}
		pmd := coreSlots[prev.slot].md
		if pmd != md && coreMemdevs[pmd].hb == coreMemdevs[md].hb {
			out = append(out, fmt.Sprintf("pair: %s %s, %s %s", prev.verb, coreSlots[prev.slot].kind, ca.verb, coreSlots[ca.slot].kind))
		}
		return out
	})
	return c
}

// generate plans and runs the steps of the run.
func (r *coreRun) generate() {
	c := r.coverer()
	c.SetBestPathRandom(*coreSeed, gofmbt.BestPathRandomAmongEquallyGood)
	rnd := rand.New(rand.NewSource(*coreSeed))
	budget := gofmbt.SearchBudget{MaxPaths: 50000}
	end := time.Time{}
	if *coreDuration > 0 {
		end = r.begin.Add(*coreDuration)
	}
	last := 0
	for r.n < *coreSteps && (end.IsZero() || time.Now().Before(end)) {
		if r.n-last >= *coreTeardown && r.state.arm == 0 {
			r.teardown()
			last = r.n
		}
		t0 := time.Now()
		path, _, _ := c.BestPathWithBudget(r.model, r.state, *coreDepth, budget)
		if path == nil {
			c.ResetCoverage()
			path, _, _ = c.BestPathWithBudget(r.model, r.state, *coreDepth, budget)
		}
		r.planned += time.Since(t0)
		var act *gofmbt.Action
		if path != nil {
			act = path[0].Action()
		} else {
			steps := r.model.StepsFrom(r.state)
			act = steps[rnd.Intn(len(steps))].Action()
		}
		start := r.state
		r.step(r.byAct[act], "")
		c.MarkCovered(gofmbt.NewStep(start, act, r.state))
	}
	r.disarm()
	r.teardown()
	_, _ = fmt.Fprintf(r.log, "core: coverage %d\n", c.Coverage())
	for _, s := range c.CoveredStrings() {
		_, _ = fmt.Fprintf(r.log, "core: covered %s\n", s)
	}
}

// replay runs the steps of a steps file.
func (r *coreRun) replay(path string) {
	f, err := os.Open(path)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "  ") || strings.TrimSpace(line) == "" {
			continue
		}
		switch line {
		case "teardown":
			r.teardown()
			continue
		case "disarm":
			r.disarm()
			continue
		}
		_, rest, _ := strings.Cut(line, " ")
		name, adoption, _ := strings.Cut(rest, " | ")
		if strings.HasPrefix(name, "teardown ") {
			// the failed step of a teardown
			r.teardown()
			continue
		}
		ca := r.byName[name]
		if ca == nil {
			r.t.Fatalf("steps file %s: no action %q", path, name)
		}
		if !r.state.enabled(ca) {
			r.t.Fatalf("steps file %s: step %d %s is not enabled in %s", path, r.n+1, name, r.state)
		}
		r.step(ca, adoption)
	}
	if err := sc.Err(); err != nil {
		r.t.Fatal(err)
	}
	r.disarm()
	r.teardown()
}

// disarm drops a fault or cut that the last step armed: nothing reached
// it.
func (r *coreRun) disarm() {
	if r.state.arm > 0 {
		w := r.state.clone()
		w.arm = 0
		r.state = w
		_, _ = fmt.Fprintln(r.steps, "disarm")
	}
}

// step runs the action from the state of the run, checks the observation
// against the prediction and adopts it; in a replay, want is the adoption
// of the steps file.
func (r *coreRun) step(ca *coreAction, want string) {
	r.n++
	start := r.state
	alts := start.predict(ca)
	got := r.runAndObserve(ca, start, alts, fmt.Sprintf("%d %s", r.n, ca.a))
	adoption := fmt.Sprintf("alt %d/%d err=%s", got+1, len(alts), r.state.err)
	_, _ = fmt.Fprintf(r.steps, "%d %s | %s\n", r.n, ca.a, adoption)
	if want != "" && want != adoption {
		_, _ = fmt.Fprintf(r.log, "core: step %d %s: adopted %q, the steps file %q\n", r.n, ca.a, adoption, want)
	}
}

// runAndObserve executes the action, observes the node and fails the test
// unless the observation equals one of the alternatives and the checks
// pass; it adopts the observation as the state of the run and returns the
// index of the alternative.
func (r *coreRun) runAndObserve(ca *coreAction, start *coreState, alts []*coreState, what string) int {
	_, _ = fmt.Fprintf(r.log, "core: step %s\n", what)
	defer r.watch(what)()
	out := r.x.run(ca, start)
	if ca.verb == "arm" {
		r.state = alts[0]
		r.adopted[ca.verb+" 1"]++
		return 0
	}
	obs, problems := r.x.observe(alts[0], out.err)
	problems = append(problems, r.x.check()...)
	if out.unreached {
		problems = append(problems, "the armed "+coreArms[start.arm-1].name+" was not reached")
	}
	if ca.verb != "prepare" && ca.verb != "unprepare" && out.msg != "" {
		problems = append(problems, "the action failed: "+out.msg)
	}
	if out.start != "" {
		problems = append(problems, "the start after the cut failed: "+out.start)
	}
	got := -1
	if obs != nil {
		for i, a := range alts {
			if a.observable() == obs.observable() {
				got = i
				break
			}
		}
	}
	if got < 0 || len(problems) > 0 {
		r.fail(what, start, alts, obs, out, problems)
	}
	// the alternative has the observed parts, those that the model knows
	// (generations, foreign regions) and those it learns (named)
	w := alts[got].clone()
	for i := range w.slots {
		w.slots[i].named = obs.slots[i].named
		if coreFront {
			w.slots[i].online = obs.slots[i].online
		}
	}
	r.state = w
	r.adopted[fmt.Sprintf("%s %d", ca.verb, got+1)]++
	return got
}

// coreStepBound is the time that a step may take.
const coreStepBound = 2 * time.Minute

// watch writes the stacks of every goroutine to the log and to stderr and
// ends the process with a panic if the step what does not end within
// coreStepBound; the returned function ends the watch.
func (r *coreRun) watch(what string) func() {
	timer := time.AfterFunc(coreStepBound, func() {
		buf := make([]byte, 16<<20)
		buf = buf[:runtime.Stack(buf, true)]
		_, _ = fmt.Fprintf(r.log, "core: step %s did not end within %v\n%s\n", what, coreStepBound, buf)
		_ = r.log.Sync()
		_, _ = os.Stderr.Write(buf)
		panic(fmt.Sprintf("core model: step %s did not end within %v (log %s)", what, coreStepBound, filepath.Join(r.out, "run.log")))
	})
	return func() { timer.Stop() }
}

// fail fails the test with the report of a step: the start state, the
// outcome, the problems and the differences of the observation from the
// closest alternatives; the steps file is kept for its replay.
func (r *coreRun) fail(what string, start *coreState, alts []*coreState, obs *coreState, out coreOutcome, problems []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "core model: step %s failed\n", what)
	fmt.Fprintf(&b, "start:\n  %s\n", strings.Join(start.components(), "\n  "))
	fmt.Fprintf(&b, "outcome: err=%q msg=%q unreached=%v\n", out.err, out.msg, out.unreached)
	for _, p := range problems {
		fmt.Fprintf(&b, "problem: %s\n", p)
	}
	if obs != nil {
		type diff struct {
			i     int
			lines []string
		}
		var diffs []diff
		oc := obs.components()
		for i, a := range alts {
			var lines []string
			for k, c := range a.components() {
				if c != oc[k] {
					lines = append(lines, fmt.Sprintf("predicted %s, observed %s", c, oc[k]))
				}
			}
			diffs = append(diffs, diff{i, lines})
		}
		slices.SortStableFunc(diffs, func(x, y diff) int { return len(x.lines) - len(y.lines) })
		for _, d := range diffs[:min(3, len(diffs))] {
			fmt.Fprintf(&b, "alternative %d of %d:\n  %s\n", d.i+1, len(alts), strings.Join(d.lines, "\n  "))
		}
	}
	_ = r.steps.Sync()
	kept := filepath.Join(os.TempDir(), fmt.Sprintf("core-model-seed%d-%s.steps", *coreSeed, time.Now().Format("150405")))
	if data, err := os.ReadFile(filepath.Join(r.out, "steps")); err == nil {
		data = append(data, []byte(fmt.Sprintf("%s | FAILED\n", what))...)
		_ = os.WriteFile(kept, data, 0o644)
	}
	fmt.Fprintf(&b, "steps file %s (log %s)\nreplay: go test ./cmd/plugins/dra-driver-cxl/internal/engine -run %s -core.replay=%s",
		kept, filepath.Join(r.out, "run.log"), r.t.Name(), kept)
	_, _ = fmt.Fprintln(r.log, b.String())
	_ = os.WriteFile(filepath.Join(r.out, "failure.txt"), []byte(b.String()), 0o644)
	if data, err := json.MarshalIndent(r.x.v.records(), "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(r.out, "records.json"), data, 0o644)
	}
	r.t.Fatal(b.String())
}

// teardown unprepares every claim, removes the regions of other tools and
// starts the node without the shared memdev configured: nothing may be
// left. The node starts again configured as before.
func (r *coreRun) teardown() {
	_, _ = fmt.Fprintln(r.steps, "teardown")
	// the unrestored claims go first; the claims whose Unprepare runs into
	// a known finding go last, when the others are gone
	for pass := 0; pass < 3; pass++ {
		for i := range coreSlots {
			ca := r.byName["unprepare "+coreSlots[i].id]
			prep := r.state.slots[i].prep
			if prep != "" && (pass == 0) == (prep == state.PreparationUnrestored) && (pass == 2 || !r.state.avoided(ca)) {
				r.substep(ca)
			}
		}
	}
	for _, md := range []int{mdA, mdB, mdC} {
		if r.state.mds[md].foreign {
			r.substep(r.byName["unforeign "+coreMemdevs[md].id])
		}
	}
	// the shared memdev unconfigured: its idle region is destroyed at the
	// start (Memdev IDLE_KEPT -> NONE, unconfigured shared)
	w := r.state.clone()
	if m := &w.mds[mdS]; m.rec == state.MemdevIdleKept {
		w.destroy(mdS)
		m.rec = ""
	}
	r.x.shared = false
	r.substepStart("restart unshared", w.restarted())
	if left := r.x.clean(); len(left) > 0 {
		r.fail(fmt.Sprintf("%d teardown", r.n), r.state, nil, nil, coreOutcome{}, left)
	}
	r.x.shared = true
	r.substepStart("restart shared", r.state.clone().restarted())
}

// substep runs an action of a teardown.
func (r *coreRun) substep(ca *coreAction) {
	alts := r.state.predict(ca)
	got := r.runAndObserve(ca, r.state, alts, fmt.Sprintf("%d teardown %s", r.n, ca.a))
	_, _ = fmt.Fprintf(r.steps, "  %s | alt %d/%d\n", ca.a, got+1, len(alts))
}

// substepStart starts the node again in a teardown, with the alternatives
// predicted.
func (r *coreRun) substepStart(what string, alts []*coreState) {
	restart := r.byName["restart"]
	got := r.runAndObserve(restart, r.state, dedup(alts), fmt.Sprintf("%d teardown %s", r.n, what))
	_, _ = fmt.Fprintf(r.steps, "  %s | alt %d/%d\n", what, got+1, len(alts))
}

// summary logs the counts of the run.
func (r *coreRun) summary() {
	elapsed := time.Since(r.begin)
	keys := make([]string, 0, len(r.adopted))
	for k := range r.adopted {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s:%d", k, r.adopted[k])
	}
	r.t.Logf("core model: %d steps in %v (%.1f ms/step, planning %v); adopted%s", r.n, elapsed.Round(time.Millisecond),
		float64(elapsed.Milliseconds())/float64(max(r.n, 1)), r.planned.Round(time.Millisecond), b.String())
	_, _ = fmt.Fprintf(r.log, "core: %d steps in %v; adopted%s\n", r.n, elapsed, b.String())
}
