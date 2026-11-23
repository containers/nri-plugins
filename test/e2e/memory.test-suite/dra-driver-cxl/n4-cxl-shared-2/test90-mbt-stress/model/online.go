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

// The on-line mode (contract.txt 5): the line protocol with the bash
// executor, planning the next step while bash executes the current one,
// the oracle, and the files mbt-steps.txt and mbt-fail.txt.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runOpts are the options of a run (online, replay, generate).
type runOpts struct {
	cfg            *Config
	cov            Coverage
	depth          int
	seed           int64
	randomness     int
	maxSteps       int           // 0: no limit
	duration       time.Duration // 0: no limit
	settleTimeout  time.Duration
	unblockTimeout time.Duration // A9: a pod after its decoder hole cleared
	planAhead      bool
	planTime       time.Duration // budget of one plan, 0: none
	maxPaths       int           // budget of one plan, 0: none
	outDir         string
	replay         []string // actions of a replay
	header         string   // the flags, for mbt-steps.txt
	now            func() time.Time
}

// errFailed is returned when the oracle failed a step.
var errFailed = errors.New("the oracle failed a step")

type onlineRun struct {
	runOpts
	in    *bufio.Reader
	out   io.Writer
	log   *log.Logger
	mm    *mbtModel
	pl    *planner
	st    *State
	start time.Time
	steps *os.File

	// planning ahead
	ahead *aheadPlan
	// the executed step, marked covered when the planner is free
	pend *pendingMark
	// notes of the oracle already logged
	noted map[string]bool
	// statistics
	nPlans, nReused, nReplanned, nRandom, nResets int
	planWait, planTotal                           time.Duration
	lastVerdict                                   string
}

type aheadPlan struct {
	pred *State // the predicted end state of the step being executed
	stop chan struct{}
	done chan planned
}

type planned struct {
	act  Act
	info planInfo
	ok   bool
}

type pendingMark struct {
	from *State
	a    Act
	to   *State
}

// markPending marks the executed step covered, with the end state that
// its observation decided. Called only when no plan runs.
func (r *onlineRun) markPending() {
	if r.pend != nil {
		r.pl.mark(r.pend.from, r.pend.a, r.pend.to)
		r.pend = nil
	}
}

func newOnlineRun(o runOpts, in io.Reader, out io.Writer, logw io.Writer) (*onlineRun, error) {
	if o.now == nil {
		o.now = time.Now
	}
	r := &onlineRun{
		runOpts: o,
		in:      bufio.NewReaderSize(in, 1<<20),
		out:     out,
		log:     log.New(logw, "mbt-cxl: ", log.Lmicroseconds),
		mm:      newMbtModel(o.cfg),
		noted:   map[string]bool{},
	}
	r.pl = newPlanner(r.mm, o.cov, o.depth, o.seed, o.randomness)
	r.st = newState(o.cfg)
	return r, nil
}

// send writes one protocol line to bash.
func (r *onlineRun) send(format string, args ...any) error {
	line := fmt.Sprintf(format, args...)
	line = strings.ReplaceAll(line, "\n", " ")
	_, err := io.WriteString(r.out, line+"\n")
	return err
}

// recv reads one protocol line from bash.
func (r *onlineRun) recv() (string, error) {
	line, err := r.in.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", fmt.Errorf("reading from bash: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (r *onlineRun) openFiles() error {
	if r.outDir == "" {
		return nil
	}
	if err := os.MkdirAll(r.outDir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(r.outDir, "mbt-steps.txt"))
	if err != nil {
		return err
	}
	r.steps = f
	_, err = fmt.Fprintf(f, "# mbt-cxl online seed=%d profile=%s coverage=%s depth=%d gofmbt=%s\n# %s\n", r.seed, r.cfg.Profile, r.cov, r.depth, gofmbtVersion, r.header)
	return err
}

// recordAdoption writes the adoption record of a step (every settle has
// one, maybe empty) for generate --from.
func (r *onlineRun) recordAdoption(n int, rec string) {
	if r.steps == nil {
		return
	}
	r.record(strings.TrimSpace(adoptPrefix + strconv.Itoa(n) + " " + rec))
}

func (r *onlineRun) recordStep(name string) {
	if r.steps == nil {
		return
	}
	r.record(name)
}

// record writes a line of the steps file to the disk; it logs an error
// (the run goes on without a complete record).
func (r *onlineRun) record(line string) {
	if _, err := fmt.Fprintln(r.steps, line); err != nil {
		r.log.Printf("steps file: %v", err)
	} else if err := r.steps.Sync(); err != nil {
		r.log.Printf("steps file: %v", err)
	}
}

// run runs the test: an initial settle, planned (or replayed) steps until
// the budget is used, the teardown, end.
func (r *onlineRun) run() (err error) {
	if err := r.openFiles(); err != nil {
		return err
	}
	defer func() {
		if r.steps != nil {
			_ = r.steps.Close() // every line is synced
		}
	}()
	r.start = r.now()
	r.log.Printf("start: profile %s (%d slots, %d actions), seed %d, depth %d, coverage %s, gofmbt %s, plan-ahead %v, unsettled %d, fencing %s, reboot %q",
		r.cfg.Profile, len(r.cfg.Slots), len(r.mm.acts), r.seed, r.depth, r.cov, gofmbtVersion, r.planAhead, r.cfg.Unsettled, r.cfg.Fencing, r.cfg.Reboot)
	settle := Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool}
	replaying := r.replay != nil
	next := 0
	if !replaying {
		if err := r.execute(settle, false, false); err != nil {
			return err
		}
	}
	for {
		if replaying {
			if next >= len(r.replay) {
				break
			}
			a, err := parseAct(r.cfg, r.replay[next])
			if err == nil && !r.st.allowed(a) {
				err = fmt.Errorf("not enabled in the model state")
			}
			if err != nil {
				r.cancelAhead()
				msg := fmt.Sprintf("replay diverged at step %d %q: %v", r.st.Steps+1, r.replay[next], err)
				r.writeFail(int(r.st.Steps)+1, r.replay[next], nil, "", Verdict{Status: VFail, Msg: msg, Diffs: []string{msg}})
				_ = r.send("fail %d %s", r.st.Steps+1, msg)
				return errFailed
			}
			next++
			final := next == len(r.replay) && a.Verb == VSettle && allAbsent(r.st)
			if err := r.execute(a, false, final); err != nil {
				return err
			}
			continue
		}
		if r.budgetUsed() {
			break
		}
		a := r.nextAct()
		if err := r.execute(a, false, false); err != nil {
			return err
		}
	}
	r.cancelAhead()
	var tear []Act
	if !replaying || !allAbsent(r.st) || r.st.Unsettled > 0 {
		// a replayed run that ended with its teardown needs none
		tear = teardownActs(r.st)
	}
	for j, a := range tear {
		if err := r.execute(a, true, j == len(tear)-1); err != nil {
			return err
		}
	}
	msg := fmt.Sprintf("done: %d steps in %s, coverage %d in round %d, plans %d (reused %d, replanned %d, random %d, resets %d), planning %s (waited %s)",
		r.st.Steps, r.now().Sub(r.start).Round(time.Second), r.pl.coverage(), r.pl.rounds(), r.nPlans, r.nReused, r.nReplanned, r.nRandom, r.nResets,
		r.planTotal.Round(time.Millisecond), r.planWait.Round(time.Millisecond))
	r.log.Print(msg)
	return r.send("end %s", msg)
}

// budgetUsed tells if the steps (the initial settle not counted) or the
// time of the run are used.
func (r *onlineRun) budgetUsed() bool {
	if r.maxSteps > 0 && int(r.st.Steps)-1 >= r.maxSteps {
		return true
	}
	return r.duration > 0 && r.now().Sub(r.start) >= r.duration
}

// teardownActs are the steps that end a test: settle what is in flight,
// start stopped controllers, delete every pod (waiters first, so that
// nothing gets promoted), settle: nothing may be left.
func teardownActs(s *State) []Act {
	var acts []Act
	settle := Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool}
	if s.Unsettled > 0 {
		acts = append(acts, settle)
	}
	for p := Pool(0); p < nPools; p++ {
		if !s.Ctrl[p].Up {
			acts = append(acts, Act{Verb: VCtrlStart, Slot: -1, VM: VMAny, Pool: p})
		}
	}
	for _, pending := range []bool{true, false} {
		for i := range s.Slots {
			ph := s.Slots[i].Phase
			if ph == Absent || ph == Deleting || (ph == Pending) != pending {
				continue
			}
			acts = append(acts, Act{Verb: VDelete, Slot: int16(i), VM: VMAny, Pool: noPool})
		}
	}
	return append(acts, settle)
}

// nextAct returns the planned next step: the result of planning ahead if
// it is still valid for the current state, else a new plan.
func (r *onlineRun) nextAct() Act {
	if r.ahead != nil {
		ah := r.ahead
		r.ahead = nil
		same := ah.pred == r.st || ah.pred.String() == r.st.String()
		if !same && r.planTime > 0 {
			// with a deadline the plan is not reproducible anyway:
			// stop it now (without one it completes, so that the
			// random choices of the planner stay the same)
			close(ah.stop)
		}
		t0 := r.now()
		res := <-ah.done
		r.planWait += r.now().Sub(t0)
		r.notePlan(res.info)
		r.markPending()
		if same && res.ok && r.st.enabled(res.act) {
			r.nReused++
			r.log.Printf("plan: reused %q (planned ahead in %s, depth %d, %d paths, +%d), waited %s",
				res.act.name(r.cfg), res.info.Elapsed.Round(time.Millisecond), res.info.Depth, res.info.Paths, res.info.Increase,
				r.now().Sub(t0).Round(time.Millisecond))
			return res.act
		}
		r.nReplanned++
		if same {
			r.log.Printf("plan: nothing planned ahead, planning again")
		} else {
			r.log.Printf("plan: the observation changed the state, planning again")
		}
	}
	r.markPending()
	t0 := r.now()
	a, info := r.pl.plan(r.st, r.budget())
	r.planWait += r.now().Sub(t0)
	r.notePlan(info)
	r.log.Printf("plan: %q in %s (depth %d, %d paths, +%d)", a.name(r.cfg), info.Elapsed.Round(time.Millisecond), info.Depth, info.Paths, info.Increase)
	return a
}

func (r *onlineRun) notePlan(info planInfo) {
	r.nPlans++
	r.planTotal += info.Elapsed
	if info.Random {
		r.nRandom++
	}
	if info.Reset {
		r.nResets++
		r.log.Printf("plan: coverage saturated within depth %d: coverage round %d", r.depth, r.pl.rounds())
	}
}

func (r *onlineRun) budget() planBudget {
	b := planBudget{MaxPaths: r.maxPaths}
	if r.planTime > 0 {
		b.Deadline = r.now().Add(r.planTime)
	}
	return b
}

// startAhead plans the step after the one that bash executes, through
// its predicted end state (the executed step is not marked covered until
// its observation has decided its end state).
func (r *onlineRun) startAhead(from *State, a Act, to *State) {
	ah := &aheadPlan{pred: to, stop: make(chan struct{}), done: make(chan planned, 1)}
	b := r.budget()
	b.Stop = ah.stop
	go func() {
		next, info, ok := r.pl.planAfter(from, a, to, b)
		ah.done <- planned{next, info, ok}
	}()
	r.ahead = ah
}

// cancelAhead stops planning ahead and waits for the planner.
func (r *onlineRun) cancelAhead() {
	if r.ahead == nil {
		return
	}
	close(r.ahead.stop)
	<-r.ahead.done
	r.ahead = nil
}

// execute runs one step: sends it, plans ahead, checks the answers with
// the oracle, adopts what the observation resolved.
func (r *onlineRun) execute(a Act, teardown, final bool) error {
	r.cancelAhead()
	r.markPending()
	from := r.st
	to := from.apply(a)
	n := int(to.Steps)
	name := a.name(r.cfg)
	if err := r.send("step %d %s", n, name); err != nil {
		return err
	}
	sent := r.now()
	r.recordStep(name)
	last := teardown || (r.replay == nil && r.budgetUsedAfter())
	if r.planAhead && !last && r.replay == nil {
		r.startAhead(from, a, to)
	}
	if a.Verb == VSettle {
		return r.settle(n, name, to, sent, final)
	}
	line, err := r.recv()
	if err != nil {
		r.cancelAhead()
		return err
	}
	st, js, err := parseDoneLine(line, n)
	if err != nil {
		r.cancelAhead()
		r.writeFail(n, name, to.expect(n, name), line, Verdict{Status: VFail, Msg: err.Error()})
		_ = r.send("fail %d %s", n, err.Error())
		return errFailed
	}
	d, err := parseDone(st, []byte(js))
	if err != nil {
		r.cancelAhead()
		r.writeFail(n, name, to.expect(n, name), line, Verdict{Status: VFail, Msg: err.Error()})
		_ = r.send("fail %d %s", n, err.Error())
		return errFailed
	}
	exp := expectDone(from, to, a, n)
	v := checkDone(exp, d)
	r.log.Printf("step %d %s: done %d: %s (%s)", n, name, st, v.Status, r.now().Sub(sent).Round(time.Millisecond))
	if v.Status != VOK {
		r.cancelAhead()
		r.writeFail(n, name, exp, line, v)
		_ = r.send("fail %d %s", n, v.Msg)
		return errFailed
	}
	adopted := adoptDone(to, a, d)
	if rec := adoptionRecord(to, 0, adopted); rec != "" {
		r.recordAdoption(n, rec)
	}
	r.done(from, a, adopted)
	return r.send("ok %d", n)
}

// done records the outcome of an accepted step: the new state, and the
// step to mark covered (now, unless a plan runs).
func (r *onlineRun) done(from *State, a Act, to *State) {
	r.st = to
	r.pend = &pendingMark{from, a, to}
	if r.ahead == nil {
		r.markPending()
	}
}

// budgetUsedAfter tells if the steps of the run are used after the
// current step, so that planning ahead is not needed.
func (r *onlineRun) budgetUsedAfter() bool {
	return r.maxSteps > 0 && int(r.st.Steps) >= r.maxSteps
}

// settle polls observations until the oracle says ok, or fails.
func (r *onlineRun) settle(n int, name string, to *State, sent time.Time, final bool) error {
	prefix := ""
	if final {
		prefix = "teardown: "
	}
	var last Verdict
	var lastLine string
	polls := 0
	for {
		line, err := r.recv()
		if err != nil {
			r.cancelAhead()
			return err
		}
		lastLine = line
		js, err := parseObserveLine(line, n)
		var o *Observation
		if err == nil {
			o, err = parseObservation([]byte(js))
		}
		if err != nil {
			r.cancelAhead()
			exp := to.expect(n, name)
			exp.Final = final
			r.writeFail(n, name, exp, line, Verdict{Status: VFail, Msg: err.Error()})
			_ = r.send("fail %d %s", n, err.Error())
			return errFailed
		}
		polls++
		if polls == 1 && to.Truncated {
			how := "issue order only for some stack"
			if to.Wide != nil {
				how = "narrowed by the nodes of its \"any\" pods once they are seen bound, and by the bindings of its race-kill pods"
			}
			r.log.Printf("step %d settle: the outcomes of its races were not all enumerated (the outcome bound, A35): %s", n, how)
		}
		v, adopted, alt := checkAltsSince(to, o, prefix, r.now(), sent, r.unblockTimeout)
		for _, note := range v.Notes {
			if !r.noted[note] {
				r.noted[note] = true
				r.log.Printf("note: %s", note)
			}
		}
		elapsed := r.now().Sub(sent)
		if v.Status == VWait && elapsed >= r.settleTimeout {
			v.Status = VFail
			v.Msg = fmt.Sprintf("%ssettle timeout after %s: %s", prefix, elapsed.Round(time.Second), strings.TrimPrefix(v.Msg, prefix))
		}
		last = v
		switch v.Status {
		case VOK:
			r.log.Printf("step %d settle: ok after %d polls, %s", n, polls, elapsed.Round(time.Millisecond))
			rec := adoptionRecord(to, alt, adopted)
			if rec != "" {
				r.log.Printf("step %d settle: adopted %s", n, rec)
			}
			r.recordAdoption(n, rec)
			r.done(r.st, Act{Verb: VSettle, Slot: -1, VM: VMAny, Pool: noPool}, adopted)
			return r.send("ok %d", n)
		case VWait:
			if v.Msg != r.lastVerdict {
				r.log.Printf("step %d settle: wait: %s", n, v.Msg)
				r.lastVerdict = v.Msg
			}
			if err := r.send("wait %d %s", n, v.Msg); err != nil {
				return err
			}
		default:
			r.cancelAhead()
			exp := to.expect(n, name)
			exp.Final = final
			r.writeFail(n, name, exp, lastLine, last)
			r.log.Printf("step %d settle: FAIL: %s", n, v.Msg)
			_ = r.send("fail %d %s", n, v.Msg)
			return errFailed
		}
	}
}

func parseDoneLine(line string, n int) (int, string, error) {
	f := strings.SplitN(line, " ", 4)
	if len(f) < 3 || f[0] != "done" {
		return 0, "", fmt.Errorf("protocol: want \"done %d STATUS JSON\", got %q", n, abbrev(line))
	}
	if f[1] != strconv.Itoa(n) {
		return 0, "", fmt.Errorf("protocol: done for step %s, want %d", f[1], n)
	}
	st, err := strconv.Atoi(f[2])
	if err != nil {
		return 0, "", fmt.Errorf("protocol: invalid status %q", f[2])
	}
	js := "{}"
	if len(f) == 4 {
		js = f[3]
	}
	return st, js, nil
}

func parseObserveLine(line string, n int) (string, error) {
	f := strings.SplitN(line, " ", 3)
	if len(f) < 3 || f[0] != "observe" {
		return "", fmt.Errorf("protocol: want \"observe %d JSON\", got %q", n, abbrev(line))
	}
	if f[1] != strconv.Itoa(n) {
		return "", fmt.Errorf("protocol: observe for step %s, want %d", f[1], n)
	}
	return f[2], nil
}

func abbrev(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// writeFail writes mbt-fail.txt: the step, the model state, the
// prediction, the last observation and every difference.
func (r *onlineRun) writeFail(n int, name string, exp *Expectation, lastLine string, v Verdict) {
	if r.outDir == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "mbt-cxl: step %d %s failed\n\n%s\n\n", n, name, v.Msg)
	b.WriteString("Differences:\n")
	for _, d := range v.Diffs {
		fmt.Fprintf(&b, "  %s\n", d)
	}
	fmt.Fprintf(&b, "\nModel state before the step:\n%s\n", r.st.dump())
	if exp != nil {
		js, _ := json.MarshalIndent(exp, "", "  ")
		fmt.Fprintf(&b, "Prediction:\n%s\n\n", js)
	}
	fmt.Fprintf(&b, "Last observation:\n%s\n", lastLine)
	fmt.Fprintf(&b, "\nReplay: MBT_REPLAY=%s\n", filepath.Join(r.outDir, "mbt-steps.txt"))
	if err := os.WriteFile(filepath.Join(r.outDir, "mbt-fail.txt"), []byte(b.String()), 0o644); err != nil {
		r.log.Printf("writing mbt-fail.txt: %v", err)
	}
}

// readActions reads the actions of a steps file (mbt-steps.txt): one per
// line, # comments and blank lines skipped.
func readActions(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var acts []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		acts = append(acts, line)
	}
	return acts, nil
}
