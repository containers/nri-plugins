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

// `mbt-cxl check` (contract.txt 5, off-line): one observation against
// one expectation of a code.gen.sh. The settle timeout needs the time
// since the first poll: bash passes --since (Unix seconds of its first
// poll of the step, `date +%s`) or --elapsed (seconds).

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// exit statuses of check
const (
	exitOK   = 0
	exitFail = 1
	exitWait = 3
)

func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	step := fs.Int("step", 0, "the step number (must match the expectation)")
	expect := fs.String("expect", "", "the expectation file (the mbt-expect here-document)")
	observed := fs.String("observed", "", "the observation file: settle JSON or \"observe N JSON\", or \"done N STATUS JSON\"")
	since := fs.Int64("since", 0, "Unix time (seconds) of the first poll of the step")
	elapsed := fs.Float64("elapsed", -1, "seconds since the first poll of the step (instead of --since)")
	timeout := fs.Duration("settle-timeout", 180*time.Second, "a wait after this long is a fail")
	_ = fs.Duration("unblock-timeout", defaultUnblock, "accepted for symmetry with online (off-line expectations have no unblock times)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *expect == "" || *observed == "" {
		_, _ = fmt.Fprintln(stderr, "check: --expect and --observed are required")
		return 2
	}
	eb, err := os.ReadFile(*expect)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "fail check: %v\n", err)
		return exitFail
	}
	e := &Expectation{}
	if err := json.Unmarshal(eb, e); err != nil {
		_, _ = fmt.Fprintf(stdout, "fail check: expectation %s: %v\n", *expect, err)
		return exitFail
	}
	if *step != 0 && e.Step != *step {
		_, _ = fmt.Fprintf(stdout, "fail check: the expectation is of step %d, not %d\n", e.Step, *step)
		return exitFail
	}
	ob, err := os.ReadFile(*observed)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "fail check: %v\n", err)
		return exitFail
	}
	o, d, err := parseObservedFile(ob)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "fail check: %s: %v\n", *observed, err)
		return exitFail
	}
	var v Verdict
	switch {
	case e.Done != nil && d == nil:
		v = Verdict{Status: VFail, Msg: fmt.Sprintf("step %d %s: want a done observation", e.Step, e.Action)}
	case d != nil:
		v = checkDone(e, d)
	default:
		v = checkSettle(e, o)
	}
	if v.Status == VWait {
		secs := *elapsed
		if secs < 0 && *since > 0 {
			secs = time.Since(time.Unix(*since, 0)).Seconds()
		}
		if secs >= 0 && time.Duration(secs*float64(time.Second)) >= *timeout {
			v.Status = VFail
			v.Msg = fmt.Sprintf("settle timeout after %.0fs: %s", secs, v.Msg)
		}
	}
	for _, diff := range v.Diffs {
		_, _ = fmt.Fprintf(stderr, "  %s\n", diff)
	}
	msg := strings.ReplaceAll(v.Msg, "\n", " ")
	switch v.Status {
	case VOK:
		_, _ = fmt.Fprintln(stdout, "ok")
		return exitOK
	case VWait:
		_, _ = fmt.Fprintf(stdout, "wait %s\n", msg)
		return exitWait
	}
	_, _ = fmt.Fprintf(stdout, "fail %s\n", msg)
	return exitFail
}
