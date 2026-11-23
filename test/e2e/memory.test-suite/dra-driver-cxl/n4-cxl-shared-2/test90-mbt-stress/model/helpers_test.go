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

// The helpers of the model's tests that several files use: states after
// action lines, slots, phases and outcomes, observations of a simulated
// SUT and their verdicts, the options of generated and on-line runs.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T, profile string, online bool) *Config {
	t.Helper()
	cfg := defaultConfig()
	cfg.Online = online
	cfg.Unsettled = 100 // the tests settle when they want
	if err := cfg.loadProfile(profile); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// size returns n, or short under -short.
func size(n, short int) int {
	if testing.Short() {
		return short
	}
	return n
}

// steps applies action lines; "!line" must be disabled.
func steps(t *testing.T, s *State, lines ...string) *State {
	t.Helper()
	for _, line := range lines {
		want := true
		if strings.HasPrefix(line, "!") {
			want, line = false, line[1:]
		}
		a, err := parseAct(s.cfg, line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if got := s.enabled(a); got != want {
			t.Fatalf("%q: enabled %v, want %v in\n%s", line, got, want, s.dump())
		}
		if want {
			s = s.apply(a)
		}
	}
	return s
}

// stateAfter returns the state of a profile after action lines (see steps).
func stateAfter(t *testing.T, profile string, online bool, lines ...string) *State {
	t.Helper()
	return steps(t, newState(testConfig(t, profile, online)), lines...)
}

func slotOf(t *testing.T, s *State, name string) *SlotState {
	t.Helper()
	i := s.cfg.slotIndex(name)
	if i < 0 {
		t.Fatalf("no slot %s", name)
	}
	return &s.Slots[i]
}

func wantPhase(t *testing.T, s *State, name string, ph Phase) {
	t.Helper()
	if got := slotOf(t, s, name).Phase; got != ph {
		t.Fatalf("%s: phase %s, want %s in\n%s", name, got, ph, s.dump())
	}
}

// alts returns the settled state and its alternatives.
func alts(s *State) []*State { return append([]*State{s}, s.Alts...) }

// dumpAlts returns the dumps of the settled state and its alternatives.
func dumpAlts(s *State) string {
	var b strings.Builder
	for j, a := range alts(s) {
		b.WriteString("alt " + strconv.Itoa(j) + ":\n" + a.dump())
	}
	return b.String()
}

// adoptSlices returns the outcome of a settle with the given slices.
func adoptSlices(t *testing.T, s *State, p Pool, st ResourceSlicesState) *State {
	t.Helper()
	for _, a := range alts(s) {
		if a.Ctrl[p].ResourceSlices == st {
			return a
		}
	}
	t.Fatalf("no outcome with the slices of pool %s %s:\n%s", p, st, dumpAlts(s))
	return nil
}

func mustAct(t *testing.T, cfg *Config, line string) Act {
	t.Helper()
	a, err := parseAct(cfg, line)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// observation returns the observation of a simulated SUT in state s (a
// copy that a test may change).
func observation(t *testing.T, s *State) *Observation {
	t.Helper()
	sm := newSim(s.cfg, 1)
	sm.truth = sm.resolve(s)
	o := sm.observe(int(s.Steps), 1)
	js, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseObservation(js)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func podOf(t *testing.T, o *Observation, slot string) *ObsPod {
	t.Helper()
	for j := range o.Pods {
		if o.Pods[j].Slot == slot {
			return &o.Pods[j]
		}
	}
	t.Fatalf("no pod of %s", slot)
	return nil
}

func ctrOf(t *testing.T, o *Observation, vm, podPrefix string) *ObsContainer {
	t.Helper()
	for j := range o.VMs[vm].Containers {
		if strings.HasPrefix(o.VMs[vm].Containers[j].Pod, podPrefix) {
			return &o.VMs[vm].Containers[j]
		}
	}
	t.Fatalf("no container of %s on %s", podPrefix, vm)
	return nil
}

func wantVerdict(t *testing.T, v Verdict, status VStatus, substr string) {
	t.Helper()
	if v.Status != status || !strings.Contains(v.Msg, substr) {
		t.Fatalf("verdict %s %q, want %s with %q; diffs:\n  %s", v.Status, v.Msg, status, substr, strings.Join(v.Diffs, "\n  "))
	}
}

func wantStack(t *testing.T, s *State, si int, want string) {
	t.Helper()
	if got := s.Stacks[si].String(); got != want {
		t.Fatalf("stack %s %s, want %s in\n%s", stackName(si), got, want, s.dump())
	}
}

// syncBuffer is a log buffer that the test may read while the run logs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testOpts(t *testing.T, profile string, steps, depth int, planAhead bool) runOpts {
	t.Helper()
	cfg := defaultConfig()
	cfg.Online = true
	if err := cfg.loadProfile(profile); err != nil {
		t.Fatal(err)
	}
	cov, _ := parseCoverage("C1,C2,C3,C4")
	return runOpts{
		cfg: cfg, cov: cov, depth: depth, seed: 1, randomness: 1, maxSteps: steps,
		settleTimeout: 180 * time.Second, planAhead: planAhead, outDir: t.TempDir(), header: "test",
	}
}

// runSim runs the on-line loop against a simulated SUT over pipes.
func runSim(t *testing.T, o runOpts, sm *sim) (string, error) {
	t.Helper()
	goIn, simOut := io.Pipe()
	simIn, goOut := io.Pipe()
	logs := &syncBuffer{}
	r, err := newOnlineRun(o, goIn, goOut, logs)
	if err != nil {
		t.Fatal(err)
	}
	simDone := make(chan error, 1)
	go func() {
		err := sm.serve(simIn, simOut)
		_ = simOut.Close()
		_, _ = io.Copy(io.Discard, simIn)
		simDone <- err
	}()
	runErr := r.run()
	_ = goOut.Close()
	if err := <-simDone; err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("sim: %v\nlog:\n%s", err, logs.String())
	}
	return logs.String(), runErr
}

func genOpts(t *testing.T, profile string, steps int, seed int64) runOpts {
	t.Helper()
	cfg := defaultConfig()
	cfg.Online = false
	if err := cfg.loadProfile(profile); err != nil {
		t.Fatal(err)
	}
	cov, _ := parseCoverage("C1,C2,C3,C4")
	return runOpts{cfg: cfg, cov: cov, depth: 2, seed: seed, randomness: 1, maxSteps: steps, header: "test"}
}
