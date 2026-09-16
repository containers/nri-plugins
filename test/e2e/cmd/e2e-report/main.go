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
// writes results.json and index.html next to them: the verdict of the run, the
// failures with the reason of each and links to everything collected for them,
// the coverage the tests reached, and every test case with its artifacts.
//
// The index subcommand rebuilds the index.html of a result root from the
// results.json of every run under it, so that the runs, how they went and how
// their coverage develops are all one click away.
//
// The coverage subcommand reports the coverage in a coverage profile: the
// coverage of the logic of each plugin, the total over everything
// instrumented, and the same numbers as json. This is what report-coverage.sh
// reports the coverage of a run with.
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
       e2e-report index RESULT_ROOT
       e2e-report coverage [--tests N] [--summary FILE] PROFILE

run      report on the results collected into RESULT_DIR
index    rebuild the index of every run under RESULT_ROOT
coverage report the coverage in the coverage profile PROFILE`

func main() {
	if len(os.Args) < 2 {
		fail("%s", usage)
	}

	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "index":
		err = indexCmd(os.Args[2:])
	case "coverage":
		err = coverageCmd(os.Args[2:])
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

func indexCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("index takes a single result root directory")
	}
	return reportIndex(args[0])
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

// reportRun writes results.json, index.html and status.txt for one run.
func reportRun(dir string) error {
	if !isDir(dir) {
		return fmt.Errorf("no such directory: %s", dir)
	}

	run, err := scanRun(dir)
	if err != nil {
		return err
	}

	if err := writeJSON(filepath.Join(dir, resultsJSON), run); err != nil {
		return err
	}
	if err := writeRunPage(filepath.Join(dir, indexHTML), run); err != nil {
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

// reportIndex rebuilds the index of every run under root.
func reportIndex(root string) error {
	if !isDir(root) {
		return fmt.Errorf("no such directory: %s", root)
	}

	names, err := readDir(root)
	if err != nil {
		return err
	}

	runs := []*Run{}
	for _, name := range names {
		dir := filepath.Join(root, name)
		if !isRun(dir) {
			continue
		}

		run := readRun(dir)

		// A run from before we reported on runs at all, one which was cut
		// short, and one which had collected nothing when we last looked and
		// may have collected something since: report on it now, which costs
		// nothing for a run with no results, and gives the index somewhere to
		// link to.
		if run == nil || len(run.Tests) == 0 {
			if err := reportRun(dir); err != nil {
				return err
			}
			if run = readRun(dir); run == nil {
				if run, err = scanRun(dir); err != nil {
					return err
				}
			}
		}

		// An older version of us did not tell when a run ran, and we order
		// them by that, so fill it in for those.
		if run.Started == nil {
			named := run.Name
			if named == "" {
				named = name
			}
			run.Started = startedAt(named, dir)
		}

		runs = append(runs, run)
	}

	sortRuns(runs)

	if err := writeIndexPage(filepath.Join(root, indexHTML), runs); err != nil {
		return err
	}

	fmt.Printf("indexed %d test runs in %s\n", len(runs), root)

	return nil
}
