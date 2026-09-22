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
	"bytes"
	"fmt"
	"html/template"
	"maps"
	"os"
	"strings"
)

// indexTitle is what the index of all runs is called.
const indexTitle = "NRI reference plugins e2e test results"

// htmlLink is a piece of text on a page, linked to somewhere if there is
// anywhere to link it to.
type htmlLink struct {
	Text string
	Href string
}

// runPage is the report of a single run, ready to be rendered.
type runPage struct {
	Title        string
	Name         string
	Verdict      string
	Meta         []htmlLink
	Total        int
	Passed       int
	Failed       int
	Errored      int
	Skipped      int
	Note         string
	Failures     []failureRow
	ShowCoverage bool
	Coverage     []coverageRow
	Sections     []vmSection
}

// failureRow is a test case which did not pass, with the reason it did not.
type failureRow struct {
	Verdict  string
	Policy   string
	Where    string
	Name     string
	Duration string
	Links    []htmlLink
	Reason   string
}

// coverageRow is the coverage of the tests of one policy, or of all of them.
type coverageRow struct {
	Policy htmlLink
	All    bool
	Tests  int
	Plugin string
	Total  string
	Links  []htmlLink
}

// vmSection is every test case run on a single VM.
type vmSection struct {
	VM     string
	Open   bool
	Passed int
	Total  int
	Tests  []testRow
}

// testRow is a single test case of a VM.
type testRow struct {
	Verdict  string
	Policy   string
	Name     string
	Duration string
	Links    []htmlLink
}

// indexPage is the list of all runs, ready to be rendered.
type indexPage struct {
	Title   string
	Plugins []string
	Runs    []indexRow
}

// indexRow is a single run in the list of all of them.
type indexRow struct {
	Run      htmlLink
	Verdict  string
	Tests    string
	Percents []string
	Runtimes string
	Version  htmlLink
}

// writeRunPage renders the report of a single run.
func writeRunPage(path string, run *Run) error {
	return writePage(path, "run", newRunPage(run))
}

// writeIndexPage renders the list of runs, latest first.
func writeIndexPage(path string, runs []*Run) error {
	return writePage(path, "index", newIndexPage(runs))
}

func writePage(path, name string, data any) error {
	page, err := renderPage(name, data)
	if err != nil {
		return err
	}

	return os.WriteFile(path, page, 0o644)
}

// renderPage renders a page into memory, for a caller serving it rather than
// writing it out.
func renderPage(name string, data any) ([]byte, error) {
	buf := &bytes.Buffer{}
	if err := pages.ExecuteTemplate(buf, name, data); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func newRunPage(run *Run) *runPage {
	page := &runPage{
		Title:   fmt.Sprintf("e2e results %s: %s", run.Name, run.Verdict),
		Name:    run.Name,
		Verdict: run.Verdict,
		Meta:    runMeta(run),
		Total:   run.Counts["total"],
		Passed:  run.Counts["PASS"],
		Failed:  run.Counts["FAIL"],
		Errored: run.Counts["ERROR"],
		Skipped: run.Counts["SKIP"],
	}

	if len(run.Tests) == 0 {
		if run.Verdict == "RUNNING" {
			page.Note = "This run is still going, so nothing has been" +
				" collected of it yet. The runner log tells where it is."
		} else {
			page.Note = "No test results were collected for this run. See the" +
				" runner log for what happened."
		}
		return page
	}

	for _, test := range run.Tests {
		if test.Verdict == "PASS" || test.Verdict == "SKIP" {
			continue
		}
		where := test.VM
		if test.Topology != nil {
			where = *test.Topology
		}
		page.Failures = append(page.Failures, failureRow{
			Verdict:  test.Verdict,
			Policy:   test.Policy,
			Where:    where,
			Name:     test.Name,
			Duration: durationText(test.Duration),
			Links:    testLinks(run, test),
			Reason:   test.Reason,
		})
	}

	page.ShowCoverage = true
	page.Coverage = coverageRows(run)
	page.Sections = vmSections(run)

	return page
}

// runMeta tells what there is to tell about a run in a single line.
func runMeta(run *Run) []htmlLink {
	meta := []htmlLink{}

	// The revision says what was tested, the branch where it was picked up
	// from, which is what tells a nightly of main from one of a branch.
	if text := versionText(run); text != "" {
		version := htmlLink{Text: text}
		if run.Git.Web != "" && run.Git.SHA1 != "" {
			version.Href = run.Git.Web + "/commit/" + run.Git.SHA1
		}
		meta = append(meta, version)
	}
	if len(run.Runtimes) > 0 {
		meta = append(meta, htmlLink{Text: strings.Join(run.Runtimes, ", ")})
	}
	if run.Started != nil {
		meta = append(meta, htmlLink{
			Text: "started " + strings.ReplaceAll(*run.Started, "T", " "),
		})
	}
	// Only when the runner was not the tree it tested, which is the odd case:
	// the runner re-execs itself out of the worktree it creates, so saying so
	// on every run would be noise.
	if run.Git.Runner != "" && run.Git.Runner != run.Git.SHA1 {
		runner := htmlLink{Text: "runner " + shortSHA(run.Git.Runner)}
		if run.Git.Web != "" {
			runner.Href = run.Git.Web + "/commit/" + run.Git.Runner
		}
		meta = append(meta, runner)
	}
	if run.Log != nil {
		meta = append(meta, htmlLink{Text: "runner log", Href: *run.Log})
	}

	return append(meta, htmlLink{Text: "all runs", Href: ".."})
}

// versionText is what a run says of the revision it tested: what it was called,
// and which branch it came from when there is one recorded.
func versionText(run *Run) string {
	if run.Git.Branch == "" {
		return run.Git.Describe
	}
	if run.Git.Describe == "" {
		return run.Git.Branch
	}

	return run.Git.Describe + " (" + run.Git.Branch + ")"
}

// shortSHA is a revision as written where the whole of it would not help. Eight
// characters, which is what git describe puts in a version.
func shortSHA(sha1 string) string {
	if len(sha1) > 8 {
		return sha1[:8]
	}

	return sha1
}

// coverageRows is the coverage of each plugin, and of everything.
func coverageRows(run *Run) []coverageRow {
	rows := []coverageRow{}

	for _, policy := range sortedKeys(run.Coverage.Policies) {
		data := run.Coverage.Policies[policy]
		plugin := ""
		if data.PluginPercent != nil {
			plugin = data.PluginPercent.String()
		}
		rows = append(rows, coverageRow{
			Policy: htmlLink{Text: policy, Href: pluginHref(run, policy, data)},
			Tests:  data.Tests,
			Plugin: plugin,
			Total:  coverageText(data),
			Links:  orderedLinks(data.Links, ""),
		})
	}

	if run.Coverage.All != nil {
		rows = append(rows, coverageRow{
			All:   true,
			Tests: run.Coverage.All.Tests,
			Total: coverageText(run.Coverage.All),
			Links: orderedLinks(run.Coverage.All.Links, ""),
		})
	}

	return rows
}

func coverageText(coverage *Coverage) string {
	return fmt.Sprintf("%v (%d/%d)", coverage.Percent, coverage.Covered,
		coverage.Statements)
}

// pluginHref links a policy to the source of its plugin, at the tested
// revision.
//
// The name of the plugin comes from the paths in the coverage data, so
// cmd/plugins/<plugin> is a directory which exists in that revision.
func pluginHref(run *Run, policy string, data *Coverage) string {
	if run.Git.Web == "" || run.Git.SHA1 == "" {
		return ""
	}
	plugin := data.Plugin
	if plugin == "" {
		plugin = policy
	}

	return run.Git.Web + "/tree/" + run.Git.SHA1 + "/" + pluginDir + plugin
}

// vmSections lists every test case, in a section per VM which the reader can
// fold away.
func vmSections(run *Run) []vmSection {
	byVM := map[string][]*Test{}
	for _, test := range run.Tests {
		byVM[test.VM] = append(byVM[test.VM], test)
	}

	sections := []vmSection{}
	for _, vm := range sortedKeys(byVM) {
		section := vmSection{VM: vm, Total: len(byVM[vm])}
		for _, test := range byVM[vm] {
			if test.Verdict == "PASS" {
				section.Passed++
			}
			section.Tests = append(section.Tests, testRow{
				Verdict:  test.Verdict,
				Policy:   test.Policy,
				Name:     test.Name,
				Duration: durationText(test.Duration),
				Links:    testLinks(run, test),
			})
		}
		// Fold away what went through, leave open what did not.
		section.Open = section.Passed != section.Total
		sections = append(sections, section)
	}

	return sections
}

// testLinks is everything collected for a test case, and its source at the
// revision which was tested.
func testLinks(run *Run, test *Test) []htmlLink {
	source := ""
	if run.Git.Web != "" && run.Git.SHA1 != "" && test.Topology != nil {
		source = strings.Join([]string{run.Git.Web, "blob", run.Git.SHA1,
			"test/e2e", suiteDir, test.Policy, *test.Topology, test.Name,
			"code.var.sh"}, "/")
	}

	return orderedLinks(test.Links, source)
}

// orderedLinks puts the links of a test case or a coverage report in the order
// they help in, anything we do not know about last.
func orderedLinks(links map[string]string, source string) []htmlLink {
	left := maps.Clone(links)
	if left == nil {
		left = map[string]string{}
	}
	if source != "" {
		left["source"] = source
	}

	ordered := make([]htmlLink, 0, len(left))
	for _, label := range linkOrder() {
		if href, ok := left[label]; ok {
			ordered = append(ordered, htmlLink{Text: label, Href: href})
			delete(left, label)
		}
	}
	for _, label := range sortedKeys(left) {
		ordered = append(ordered, htmlLink{Text: label, Href: left[label]})
	}

	return ordered
}

// linkOrder is the order the links of a test case are worth following in.
func linkOrder() []string {
	labels := make([]string, 0, len(artifacts)+2)
	for _, a := range artifacts {
		labels = append(labels, a.label)
	}

	return append(labels, "runtime log", "source")
}

// durationText tells how long a test case took, in as few characters as it
// takes to tell.
func durationText(seconds *float64) string {
	if seconds == nil {
		return ""
	}
	if *seconds < 60 {
		return fmt.Sprintf("%.0fs", *seconds)
	}

	return fmt.Sprintf("%dm%02ds", int(*seconds)/60, int(*seconds)%60)
}

func newIndexPage(runs []*Run) *indexPage {
	page := &indexPage{Title: indexTitle, Runs: []indexRow{}}

	seen := map[string]bool{}
	for _, run := range runs {
		for policy := range run.Coverage.Policies {
			seen[policy] = true
		}
	}
	page.Plugins = sortedKeys(seen)

	for _, run := range runs {
		version := htmlLink{Text: run.Git.Describe}
		if run.Git.Web != "" && run.Git.SHA1 != "" {
			version.Href = run.Git.Web + "/commit/" + run.Git.SHA1
		}

		// A run still going has no report to open, so its row points at the
		// directory and whoever serves it lists what has piled up so far.
		href := run.Name + "/" + indexHTML
		if run.Unreported {
			href = run.Name + "/"
		}

		row := indexRow{
			Run:      htmlLink{Text: run.Name, Href: href},
			Verdict:  run.Verdict,
			Tests:    fmt.Sprintf("%d/%d", run.Counts["PASS"], run.Counts["total"]),
			Runtimes: strings.Join(run.Runtimes, ", "),
			Version:  version,
		}
		for _, plugin := range page.Plugins {
			percent := ""
			if data := run.Coverage.Policies[plugin]; data != nil {
				if data.PluginPercent != nil {
					percent = data.PluginPercent.String()
				}
			}
			row.Percents = append(row.Percents, percent)
		}
		page.Runs = append(page.Runs, row)
	}

	return page
}

var pages = template.Must(template.New("pages").Parse(`
{{- define "top"}}<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>{{.}}</title>
<style>
:root {
  --fg: #222; --dim: #666; --bg: #fff; --line: #e6e6e6; --head: #f4f4f4;
  --hover: #fafafa; --link: #0645ad; --pass: #157f3d; --fail: #c0392b;
  --skip: #777; --failbg: #fdf3f2;
}
@media (prefers-color-scheme: dark) {
  :root {
    --fg: #ddd; --dim: #9a9a9a; --bg: #1b1b1b; --line: #333; --head: #262626;
    --hover: #242424; --link: #7aa7ff; --pass: #4cc47c; --fail: #ff7b6b;
    --skip: #999; --failbg: #2a1f1e;
  }
}
body { font-family: system-ui, sans-serif; margin: 2em auto; max-width: 76em;
       padding: 0 1em; color: var(--fg); background: var(--bg);
       line-height: 1.45; }
h1 { font-size: 1.4em; margin-bottom: 0.2em; }
h2 { font-size: 1.1em; margin-top: 2em; border-bottom: 1px solid var(--line);
     padding-bottom: 0.2em; }
table { border-collapse: collapse; width: 100%; font-size: 0.9em; }
th { text-align: left; background: var(--head); position: sticky; top: 0; }
th, td { padding: 0.35em 0.6em; border-bottom: 1px solid var(--line);
         vertical-align: top; }
.num { text-align: right; font-variant-numeric: tabular-nums;
       white-space: nowrap; }
tr:hover td { background: var(--hover); }
a { color: var(--link); text-decoration: none; }
a:hover { text-decoration: underline; }
.meta { color: var(--dim); margin-bottom: 1.5em; }
.counts { color: var(--dim); }
.verdict { display: inline-block; padding: 0 0.45em; border-radius: 0.6em;
           font-size: 0.85em; font-weight: bold; letter-spacing: 0.02em;
           border: 1px solid currentColor; }
.PASS { color: var(--pass); }
.FAIL, .ERROR, .ABORTED { color: var(--fail); }
.RUNNING { color: var(--link); }
.SKIP { color: var(--skip); }
h1 .verdict { font-size: 0.7em; vertical-align: 0.15em; }
pre.reason { margin: 0.35em 0 0 0; padding: 0.45em 0.7em;
             background: var(--failbg); border-left: 3px solid var(--fail);
             overflow-x: auto; font-size: 0.95em; white-space: pre-wrap; }
.links a { margin-right: 0.7em; white-space: nowrap; }
details { margin-top: 0.8em; }
summary { cursor: pointer; padding: 0.3em 0; font-weight: bold; }
summary:hover { color: var(--link); }
summary .counts { font-weight: normal; }
</style>
</head>
<body>
{{end}}

{{- define "bottom"}}</body>
</html>
{{end}}

{{- define "verdict"}}<span class="verdict {{.}}">{{.}}</span>{{end}}

{{- define "text"}}{{if .Href}}<a href="{{.Href}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}{{end}}

{{- define "links"}}<span class="links">{{range .}}<a href="{{.Href}}">{{.Text}}</a>{{end}}</span>{{end}}

{{- define "run"}}{{template "top" .Title}}<h1>e2e results {{.Name}} &mdash; {{template "verdict" .Verdict}}</h1>
<div class="meta">{{range $i, $m := .Meta}}{{if $i}} &middot; {{end}}{{template "text" $m}}{{end}}</div>
<p class="counts">{{.Total}} test cases: {{.Passed}} passed, {{.Failed}} failed, {{.Errored}} errored, {{.Skipped}} skipped.</p>
{{if .Note}}<p>{{.Note}}</p>
{{end}}{{if .Failures}}<h2>Failures ({{len .Failures}})</h2>
<table>
{{range .Failures}}<tr><td>{{template "verdict" .Verdict}}</td><td>{{.Policy}} / {{.Where}} / {{.Name}}</td><td class="num">{{.Duration}}</td><td>{{template "links" .Links}}{{if .Reason}}<pre class="reason">{{.Reason}}</pre>{{end}}</td></tr>
{{end}}</table>
{{end}}{{if .ShowCoverage}}<h2>Coverage</h2>
{{if .Coverage}}<table>
<tr><th>policy</th><th class="num">tests</th><th class="num">plugin logic</th><th class="num">all instrumented packages</th><th></th></tr>
{{range .Coverage}}<tr><td>{{if .All}}<em>all tests</em>{{else}}{{template "text" .Policy}}{{end}}</td><td class="num">{{.Tests}}</td><td class="num">{{.Plugin}}</td><td class="num">{{.Total}}</td><td>{{template "links" .Links}}</td></tr>
{{end}}</table>
{{else}}<p>No coverage data was collected. Were the plugins built with instrumentation (make COVER=1)?</p>
{{end}}{{end}}{{if .Sections}}<h2>Test cases ({{.Total}})</h2>
{{range .Sections}}<details{{if .Open}} open{{end}}>
<summary>{{.VM}} <span class="counts">&mdash; {{.Passed}}/{{.Total}} passed</span></summary>
<table>
<tr><th>verdict</th><th>test</th><th class="num">duration</th><th></th></tr>
{{range .Tests}}<tr><td>{{template "verdict" .Verdict}}</td><td>{{.Policy}} / {{.Name}}</td><td class="num">{{.Duration}}</td><td>{{template "links" .Links}}</td></tr>
{{end}}</table>
</details>
{{end}}{{end}}{{template "bottom"}}{{end}}

{{- define "index"}}{{template "top" .Title}}<h1>{{.Title}}</h1>
{{if .Runs}}<table>
<tr><th>run</th><th>verdict</th><th class="num">tests</th>{{range .Plugins}}<th class="num">{{.}}</th>{{end}}<th>runtime</th><th>version</th></tr>
{{range .Runs}}<tr><td>{{template "text" .Run}}</td><td>{{template "verdict" .Verdict}}</td><td class="num">{{.Tests}}</td>{{range .Percents}}<td class="num">{{.}}</td>{{end}}<td>{{.Runtimes}}</td><td>{{template "text" .Version}}</td></tr>
{{end}}</table>
{{else}}<p>No test runs have been published yet.</p>
{{end}}{{template "bottom"}}{{end}}
`))
