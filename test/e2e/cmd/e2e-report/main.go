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

// Command e2e-report reports on the results of e2e test runs.
//
// The run subcommand reads the results collected into a result directory and
// writes results.json next to them: the verdict of the run, the failures with
// the reason of each and links to everything collected for them, the coverage
// the tests reached, and every test case with its artifacts. What that is shown
// as is decided when it is served, so nothing here renders a page.
//
// The refresh subcommand reports on every unpacked run under a result root
// again. What a run recorded is what a report can show, so a run published by
// an older runner records what that runner knew to record, and nothing but
// reporting on it again brings it up to what we record today.
//
// The coverage subcommand reports the coverage in a coverage profile: the
// coverage of the logic of each plugin, the total over everything
// instrumented, and the same numbers as json. This is what report-coverage.sh
// reports the coverage of a run with.
//
// The pack subcommand packs everything a run collected into a single archive,
// leaving behind what it takes to tell how the run went without unpacking
// anything. A run costs a tenth of what it costs browsable, and nothing has to
// be thrown away to get there.
//
// The serve subcommand serves published results over HTTP, the packed ones as
// if their archive had been extracted where it is. The index of the runs and
// the report of each run are rendered for every request from what the runs
// recorded, so every run is shown the way this version shows one, whenever it
// was published and whether or not it has been packed up since. It writes
// nothing.
//
// All of them read what is there and are safe to rerun on results already
// reported on.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const usage = `Usage: e2e-report run RESULT_DIR
       e2e-report refresh RESULT_ROOT
       e2e-report coverage [--tests N] [--summary FILE] PROFILE
       e2e-report pack RESULT_DIR
       e2e-report serve [--address ADDR] RESULT_ROOT

run      report on the results collected into RESULT_DIR
refresh  report on every unpacked run under RESULT_ROOT again
coverage report the coverage in the coverage profile PROFILE
pack     pack up what the run in RESULT_DIR collected
serve    serve the results published under RESULT_ROOT over HTTP`

func main() {
	if len(os.Args) < 2 {
		fail("%s", usage)
	}

	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "refresh":
		err = refreshCmd(os.Args[2:])
	case "coverage":
		err = coverageCmd(os.Args[2:])
	case "pack":
		err = packCmd(os.Args[2:])
	case "serve":
		err = serveCmd(os.Args[2:])
	default:
		fail("%s", usage)
	}

	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e-report: "+format+"\n", args...)
	os.Exit(1)
}

func runCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("run takes a single result directory")
	}
	return reportRun(args[0])
}

func refreshCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("refresh takes a single result root directory")
	}

	return refreshRuns(args[0])
}

func packCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("pack takes a single result directory")
	}

	dir := args[0]
	before, err := dirSize(dir)
	if err != nil {
		return err
	}

	if err := packRun(dir); err != nil {
		return err
	}

	after, err := dirSize(dir)
	if err != nil {
		return err
	}
	fmt.Printf("packed %s, %dM of %dM saved\n", filepath.Base(dir),
		(before-after)/(1024*1024), before/(1024*1024))

	return nil
}

func coverageCmd(args []string) error {
	flags := flag.NewFlagSet("coverage", flag.ContinueOnError)
	tests := flags.Int("tests", 0, "number of test cases the profile covers")
	summary := flags.String("summary", "", "write the numbers as json here")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("coverage takes a single coverage profile")
	}

	coverage, err := summarize(flags.Arg(0), *tests)
	if err != nil {
		return err
	}

	if *summary != "" {
		if err := writeJSON(*summary, coverage); err != nil {
			return err
		}
	}

	coverage.report(os.Stdout)

	return nil
}

// reportRun writes results.json and status.txt for one run. What a report shows
// is decided when one is rendered, which only happens while serving a run, so
// there is no page to write here.
func reportRun(dir string) error {
	if !isDir(dir) {
		return fmt.Errorf("no such directory: %s", dir)
	}
	// The results are in the archive now, and scanning would find none of
	// them: the report of a packed run is the one made before it was packed.
	if isPacked(dir) {
		return fmt.Errorf("%s is packed, its report is the one it was packed with", dir)
	}

	run, err := scanRun(dir)
	if err != nil {
		return err
	}

	if err := writeJSON(filepath.Join(dir, resultsJSON), run); err != nil {
		return err
	}

	status := fmt.Sprintf("%s %d/%d tests passed", run.Verdict,
		run.Counts["PASS"], run.Counts["total"])

	// One line for whoever runs the tests to act on without reading json.
	err = os.WriteFile(filepath.Join(dir, statusTxt), []byte(status+"\n"), 0o644)
	if err != nil {
		return err
	}

	fmt.Printf("%s: %s\n", run.Name, status)

	return nil
}

// refreshRuns reports on every unpacked run under root again. A packed run is
// left alone: its results are inside the archive, so there is nothing left to
// scan. Nothing else needs doing for a root -- the index of the runs is built
// when it is served, and a run which was never reported on at all is scanned
// then and there.
func refreshRuns(root string) error {
	if !isDir(root) {
		return fmt.Errorf("no such directory: %s", root)
	}

	names, err := readDir(root)
	if err != nil {
		return err
	}

	runs, reported := 0, 0
	for _, name := range names {
		dir := filepath.Join(root, name)
		if !isRun(dir) {
			continue
		}
		runs++

		if isPacked(dir) {
			continue
		}
		if err := reportRun(dir); err != nil {
			return err
		}
		reported++
	}

	fmt.Printf("reported on %d of the %d test runs in %s again\n", reported, runs, root)

	return nil
}
