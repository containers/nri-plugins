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
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// suiteDir is what a test suite is called within the results of a VM.
	suiteDir = "policies.test-suite"
	// coverageDir is where the coverage report of a run is.
	coverageDir = "coverage-report"
	// resultsJSON is what a run is reported in, machine readable.
	resultsJSON = "results.json"
	// indexHTML is what a run and the collection of all runs are reported in.
	indexHTML = "index.html"
	// runnerLog is the log of the runner, the first thing a run publishes.
	runnerLog = "e2e-runner.log.txt"
	// statusTxt is where we record the verdict of a run in a single line.
	statusTxt = "status.txt"
	// summaryTxt is what a test case and a whole run record their verdict in.
	summaryTxt = "summary.txt"
	// testLog is the log of a single test case.
	testLog = "run.sh.output.txt"
	// summaryJSON is the numbers of a coverage report.
	summaryJSON = "summary.json"
)

var (
	runStamp   = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})[-_]?(\d{2})-?(\d{2})?`)
	verdictRe  = regexp.MustCompile(`Test verdict:\s*(\S+)`)
	durationRe = regexp.MustCompile(`Test duration:\s*([0-9.]+)\s*sec`)
	errorLine  = regexp.MustCompile(`^error:`)
	errorBlock = regexp.MustCompile(`^(command|output|exit status):`)
	sshRemote  = regexp.MustCompile(`^(?:ssh|git)://(?:[^@/]+@)?([^/]+)/(.+)`)
	scpRemote  = regexp.MustCompile(`^(?:[^@/]+@)([^:/]+):(.+)`)
)

// runtimes are the container runtimes we test with, and the last field of the
// name of a VM directory.
var runtimes = []string{"containerd", "crio"}

// artifacts of a test case, in the order they help when one fails.
var artifacts = []struct{ label, name string }{
	{"test log", testLog},
	{"plugin log", "nri-resource-policy.output.txt"},
	{"commands", "commands"},
	{"artifacts", "artifacts.tar.xz"},
	{"pyexec", "pyexec.output.txt"},
	{"pyexec code", "pyexec.py"},
	{"pyexec state", "pyexec_state.py"},
	{"plugin cache", "cache"},
	{"verdict", summaryTxt},
	{"coverage", "coverage"},
}

const (
	// A failure reason is for telling failures apart at a glance, not for
	// reading the whole log in.
	reasonLines = 8
	reasonWidth = 220

	// How long a run which says it is running may go without writing anything
	// to its log before we take it for one which never got to the end.
	staleAfter = 60 * 60 * time.Second
)

// Run is everything worth reporting about a single run of the tests. The json
// tags spell the keys of results.json, in the order they are written in.
type Run struct {
	Counts   map[string]int `json:"counts"`
	Coverage RunCoverage    `json:"coverage"`
	Git      Git            `json:"git"`
	Log      *string        `json:"log"`
	Name     string         `json:"name"`
	Runtimes []string       `json:"runtimes"`
	Started  *string        `json:"started"`
	Tests    []*Test        `json:"tests"`
	Verdict  string         `json:"verdict"`
}

// Git tells which revision was tested, and where to find it.
type Git struct {
	Describe string `json:"describe"`
	Remote   string `json:"remote"`
	SHA1     string `json:"sha1"`
	Web      string `json:"web"`
}

// Test is a single test case of a run. The topology, distro and runtime are
// unset for results which did not come from a VM of ours.
type Test struct {
	Distro   *string           `json:"distro"`
	Duration *float64          `json:"duration"`
	Links    map[string]string `json:"links"`
	Name     string            `json:"name"`
	Path     string            `json:"path"`
	Policy   string            `json:"policy"`
	Reason   string            `json:"reason"`
	Runtime  *string           `json:"runtime"`
	Topology *string           `json:"topology"`
	Verdict  string            `json:"verdict"`
	VM       string            `json:"vm"`
}

// RunCoverage is the coverage a run reached, over all of its tests and over
// the tests of each policy separately.
type RunCoverage struct {
	All      *Coverage            `json:"all"`
	Policies map[string]*Coverage `json:"policies"`
}

// scanRun collects everything worth reporting about a single run.
func scanRun(dir string) (*Run, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	tests, err := scanTests(dir)
	if err != nil {
		return nil, err
	}

	counts := map[string]int{"total": len(tests)}
	for _, t := range tests {
		counts[t.Verdict]++
	}

	var verdict string
	switch {
	case len(tests) == 0:
		verdict = unfinishedVerdict(dir)
	case counts["FAIL"] > 0 || counts["ERROR"] > 0:
		verdict = "FAIL"
	default:
		verdict = "PASS"
	}

	remote := readTrimmed(filepath.Join(dir, "git.remote"))

	seen := map[string]bool{}
	for _, t := range tests {
		if t.Runtime != nil {
			seen[*t.Runtime] = true
		}
	}
	found := make([]string, 0, len(seen))
	for runtime := range seen {
		found = append(found, runtime)
	}
	sort.Strings(found)

	var log *string
	if exists(filepath.Join(dir, runnerLog)) {
		log = ref(runnerLog)
	}

	coverage, err := scanCoverage(dir)
	if err != nil {
		return nil, err
	}

	return &Run{
		Counts:   counts,
		Coverage: coverage,
		Git: Git{
			Describe: readTrimmed(filepath.Join(dir, "git.describe")),
			Remote:   remote,
			SHA1:     readTrimmed(filepath.Join(dir, "git.sha1")),
			Web:      webURL(remote),
		},
		Log:      log,
		Name:     filepath.Base(abs),
		Runtimes: found,
		Started:  startedAt(filepath.Base(abs), dir),
		Tests:    tests,
		Verdict:  verdict,
	}, nil
}

// scanTests collects a record of every test case of a run.
func scanTests(dir string) ([]*Test, error) {
	tests := []*Test{}

	vms, err := readDir(dir)
	if err != nil {
		return nil, err
	}

	for _, vm := range vms {
		suite := filepath.Join(dir, vm, suiteDir)
		if !isDir(suite) {
			continue
		}
		topology, distro, runtime := vmParts(vm)

		policies, err := readDir(suite)
		if err != nil {
			return nil, err
		}
		for _, policy := range policies {
			policyDir := filepath.Join(suite, policy)
			if !isDir(policyDir) {
				continue
			}

			names, err := readDir(policyDir)
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				testDir := filepath.Join(policyDir, name)
				if !isDir(testDir) || !strings.HasPrefix(name, "test") {
					continue
				}

				rel := filepath.Join(vm, suiteDir, policy, name)

				// No verdict at all means the test never got to write one,
				// which is a different thing from failing an assertion.
				verdict := "ERROR"
				summary := readFile(filepath.Join(testDir, summaryTxt))
				if m := verdictRe.FindStringSubmatch(summary); m != nil {
					verdict = strings.ToUpper(m[1])
				}

				var duration *float64
				log := readFile(filepath.Join(testDir, testLog))
				if m := durationRe.FindStringSubmatch(log); m != nil {
					if seconds, err := strconv.ParseFloat(m[1], 64); err == nil {
						duration = &seconds
					}
				}

				reason := ""
				if verdict != "PASS" {
					reason = failureReason(testDir)
				}

				links, err := artifactsOf(testDir, rel)
				if err != nil {
					return nil, err
				}

				tests = append(tests, &Test{
					Distro:   distro,
					Duration: duration,
					Links:    links,
					Name:     name,
					Path:     rel,
					Policy:   policy,
					Reason:   reason,
					Runtime:  runtime,
					Topology: topology,
					Verdict:  verdict,
					VM:       vm,
				})
			}
		}
	}

	return tests, nil
}

// vmParts splits a VM directory name into its topology, distro and runtime.
//
// The name is TOPOLOGY-DISTRO-RUNTIME, with the slash of the distro replaced
// by a dash, so the last three dash separated fields are the distro and the
// runtime. Leave the topology unset if the name is not one of ours, rather
// than guess and link to a test which does not exist.
func vmParts(vm string) (topology, distro, runtime *string) {
	fields := strings.Split(vm, "-")
	last := fields[len(fields)-1]
	if !slices.Contains(runtimes, last) {
		return nil, nil, nil
	}
	if len(fields) < 4 {
		return nil, nil, &last
	}
	return ref(strings.Join(fields[:len(fields)-3], "-")),
		ref(strings.Join(fields[len(fields)-3:len(fields)-1], "-")), &last
}

// artifactsOf maps a label to a link for everything collected for a test case.
func artifactsOf(testDir, rel string) (map[string]string, error) {
	links := map[string]string{}

	for _, a := range artifacts {
		info, err := os.Stat(filepath.Join(testDir, a.name))
		if err != nil {
			continue
		}
		// An empty file is not worth a link: pyexec.output.txt, for one, holds
		// the output of the last pyexec of a test, which wrote nothing at all
		// unless it failed.
		if info.Mode().IsRegular() && info.Size() == 0 {
			continue
		}
		links[a.label] = filepath.Join(rel, a.name)

		// The artifacts of a test are packed up, and worth browsing as well as
		// downloading. Only e2e-report serve serves into a tarball, a plain
		// file server hands it over as it is, so the tarball stays the link
		// which works either way.
		if isTarball(a.name) {
			links[a.label+" (browse)"] = filepath.Join(rel, a.name) + "/"
		}
	}

	names, err := readDir(testDir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if strings.HasPrefix(name, "runtime.") && strings.HasSuffix(name, ".txt") {
			links["runtime log"] = filepath.Join(rel, name)
		}
	}

	return links, nil
}

// failureReason digs out why a test case failed, in as few lines as possible.
func failureReason(testDir string) string {
	// What the framework recorded itself: a failed assertion names itself.
	verified := []string{}
	for _, line := range splitLines(readFile(filepath.Join(testDir, summaryTxt))) {
		if strings.HasPrefix(line, "verify:") {
			verified = append(verified, line)
		}
	}
	if len(verified) > 0 {
		return shorten(verified)
	}

	log := splitLines(readFile(filepath.Join(testDir, testLog)))
	if len(log) == 0 {
		return ""
	}

	// The last error reported, with the command which caused it. command-error
	// prints the command, its output and its exit status before the error.
	for i := len(log) - 1; i >= 0; i-- {
		if !errorLine.MatchString(log[i]) {
			continue
		}
		start := i
		for start > 0 && errorBlock.MatchString(log[start-1]) {
			start--
		}
		return shorten(log[start : i+1])
	}

	// Nothing said why, so the tail of the log is the best we can do.
	tail := []string{}
	for _, line := range log {
		if strings.TrimSpace(line) != "" {
			tail = append(tail, line)
		}
	}
	if len(tail) > reasonLines {
		tail = tail[len(tail)-reasonLines:]
	}

	return shorten(tail)
}

// shorten cuts a reason down to something which fits in a table cell.
func shorten(lines []string) string {
	short := []string{}
	for _, line := range lines[:min(len(lines), reasonLines)] {
		line = strings.TrimRight(line, " \t\v\f\r\n")
		if runes := []rune(line); len(runes) > reasonWidth {
			line = string(runes[:reasonWidth]) + "..."
		}
		short = append(short, line)
	}
	if len(lines) > reasonLines {
		short = append(short, "...")
	}

	return strings.Join(short, "\n")
}

// coverageOf reads the numbers of one coverage report, with links to it.
func coverageOf(dir, resultDir string) (*Coverage, error) {
	data, err := os.ReadFile(filepath.Join(dir, summaryJSON))
	if err != nil {
		return nil, nil
	}

	coverage := &Coverage{}
	if err := json.Unmarshal(data, coverage); err != nil {
		return nil, nil
	}

	links := map[string]string{}
	for _, l := range []struct{ label, name string }{
		{"report", "coverage.html"},
		{"profile", "coverprofile"},
	} {
		if !exists(filepath.Join(dir, l.name)) {
			continue
		}
		rel, err := filepath.Rel(resultDir, filepath.Join(dir, l.name))
		if err != nil {
			return nil, err
		}
		links[l.label] = rel
	}
	if len(links) > 0 {
		coverage.Links = links
	}

	return coverage, nil
}

// scanCoverage reads the coverage reports of a run, the combined and the per
// policy ones.
func scanCoverage(dir string) (RunCoverage, error) {
	coverage := RunCoverage{Policies: map[string]*Coverage{}}

	combined := filepath.Join(dir, coverageDir)
	all, err := coverageOf(combined, dir)
	if err != nil {
		return coverage, err
	}
	coverage.All = all

	policies := filepath.Join(combined, "policies")
	if !isDir(policies) {
		return coverage, nil
	}

	names, err := readDir(policies)
	if err != nil {
		return coverage, err
	}
	for _, policy := range names {
		data, err := coverageOf(filepath.Join(policies, policy), dir)
		if err != nil {
			return coverage, err
		}
		if data == nil {
			continue
		}
		// The plugin of a policy is the interesting part of its coverage, the
		// rest is the infrastructure it happens to link in.
		if _, ok := data.Plugins[policy]; ok {
			data.Plugin = policy
		} else if plugins := sortedKeys(data.Plugins); len(plugins) > 0 {
			data.Plugin = plugins[0]
		}
		if plugin, ok := data.Plugins[data.Plugin]; ok {
			data.PluginPercent = &plugin.Percent
		}
		coverage.Policies[policy] = data
	}

	return coverage, nil
}

// startedAt tells when a run started, from its name or from the results.
//
// A run is usually named after when it started, but not always: --name takes
// anything. Dig a timestamp out of the name if there is one, and fall back to
// when the directory was last written to.
func startedAt(name, dir string) *string {
	if m := runStamp.FindStringSubmatch(name); m != nil {
		minute := m[3]
		if minute == "" {
			minute = "00"
		}
		stamp, err := time.Parse("2006-01-02-15-04", m[1]+"-"+m[2]+"-"+minute)
		if err == nil {
			return ref(stamp.Format(isoTime))
		}
	}

	info, err := os.Stat(dir)
	if err != nil {
		return nil
	}

	return ref(info.ModTime().Format(isoTime))
}

// webURL turns a remote repository into something linkable, if we can.
//
// We are cloned from over https or over ssh, and the ssh remote comes in two
// spellings, so turn all of them into the address of the same thing on the
// web. Anything else we cannot link to.
func webURL(remote string) string {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")

	if m := sshRemote.FindStringSubmatch(remote); m != nil {
		return "https://" + m[1] + "/" + m[2]
	}
	if m := scpRemote.FindStringSubmatch(remote); m != nil {
		return "https://" + m[1] + "/" + m[2]
	}
	if strings.HasPrefix(remote, "https://") {
		return remote
	}

	return ""
}

// unfinishedVerdict tells a run which is still going from one which never got
// to the end.
//
// A run says it is running as soon as it has somewhere to say it, and says how
// it went once it knows. So a run with no results at all is either still at it
// or was cut short, which the log it keeps writing to tells apart.
func unfinishedVerdict(dir string) string {
	// The log a run keeps writing to is what tells: whatever anything says, a
	// log written to a moment ago belongs to a run which is still going.
	if info, err := os.Stat(filepath.Join(dir, runnerLog)); err == nil {
		if time.Since(info.ModTime()) < staleAfter {
			return "RUNNING"
		}
	}

	// It has gone quiet, so it either said it was running and never got to the
	// end, or it is from before we said anything at all.
	status := strings.Fields(readFile(filepath.Join(dir, statusTxt)))
	if len(status) > 0 && (status[0] == "RUNNING" || status[0] == "ABORTED") {
		return "ABORTED"
	}

	return "ERROR"
}

// isRun tells whether a directory holds the results of a run.
//
// Going by what is in it rather than by what it is called: --name takes
// anything. A run which was cut short before it collected a single result
// still has the log of the runner, and belongs in the index just as much.
func isRun(dir string) bool {
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return false
	}
	for _, name := range []string{resultsJSON, summaryTxt, runnerLog} {
		if exists(filepath.Join(dir, name)) {
			return true
		}
	}

	return false
}

// readRun reads back what we reported of a run, if we reported on it at all.
func readRun(dir string) *Run {
	if !exists(filepath.Join(dir, indexHTML)) {
		return nil
	}

	data, err := os.ReadFile(filepath.Join(dir, resultsJSON))
	if err != nil {
		return nil
	}

	run := &Run{}
	if err := json.Unmarshal(data, run); err != nil {
		return nil
	}

	return run
}

// sortRuns orders runs latest first, by when they ran rather than by what they
// are called.
func sortRuns(runs []*Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		return deref(runs[i].Started) > deref(runs[j].Started)
	})
}
