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
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// pluginDir is where the logic of a plugin lives, relative to the repository.
const pluginDir = "cmd/plugins/"

// Coverage is the numbers of one coverage report: what summary.json holds next
// to a coverage profile, and what the report of a run needs on top of them.
// The json tags spell the keys of both, in the order they are written in.
type Coverage struct {
	Covered       int               `json:"covered"`
	Links         map[string]string `json:"links,omitempty"`
	Percent       Percent           `json:"percent"`
	Plugin        string            `json:"plugin,omitempty"`
	PluginPercent *Percent          `json:"plugin_percent,omitempty"`
	Plugins       map[string]Plugin `json:"plugins"`
	Statements    int               `json:"statements"`
	Tests         int               `json:"tests"`
}

// Plugin is the coverage of the logic of a single plugin.
type Plugin struct {
	Covered    int     `json:"covered"`
	Percent    Percent `json:"percent"`
	Statements int     `json:"statements"`
}

// Percent is a percentage of coverage, reported to a single decimal.
type Percent float64

// MarshalJSON reports a percentage the way it is reported everywhere else, so
// that whoever reads it back and prints it gets the same number we printed.
func (p Percent) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(p), 'f', 1, 64)), nil
}

func (p Percent) String() string {
	return fmt.Sprintf("%.1f%%", float64(p))
}

// percent is the share of statements covered, weighted by statements the way
// go tool cover calculates its percentages.
func percent(covered, statements int) Percent {
	if statements <= 0 {
		return 0
	}
	return Percent(100 * float64(covered) / float64(statements))
}

// summarize works out the coverage in a coverage profile: the coverage of the
// logic of each plugin, of the code under cmd/plugins/PLUGIN, and the total
// over everything instrumented. tests is the number of test cases the profile
// covers, which we only pass on.
func summarize(profile string, tests int) (*Coverage, error) {
	data, err := os.ReadFile(profile)
	if err != nil {
		return nil, err
	}

	coverage := &Coverage{Tests: tests, Plugins: map[string]Plugin{}}

	// A profile is a mode line followed by a line per block of statements:
	// FILE:LINE.COL,LINE.COL STATEMENTS COUNT, where COUNT is how many times
	// the block was entered.
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		statements, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}

		covered := 0
		if count > 0 {
			covered = statements
		}

		coverage.Statements += statements
		coverage.Covered += covered

		file, _, _ := strings.Cut(fields[0], ":")
		if name := pluginOf(file); name != "" {
			plugin := coverage.Plugins[name]
			plugin.Statements += statements
			plugin.Covered += covered
			coverage.Plugins[name] = plugin
		}
	}

	coverage.Percent = percent(coverage.Covered, coverage.Statements)
	for name, plugin := range coverage.Plugins {
		// A package with no statements at all, one declaring nothing but
		// types, has no coverage to report.
		if plugin.Statements <= 0 {
			delete(coverage.Plugins, name)
			continue
		}
		plugin.Percent = percent(plugin.Covered, plugin.Statements)
		coverage.Plugins[name] = plugin
	}

	return coverage, nil
}

// pluginOf attributes a file to the plugin whose logic it is part of, if it is
// part of one.
func pluginOf(file string) string {
	i := strings.Index(file, pluginDir)
	if i < 0 || (i > 0 && file[i-1] != '/') {
		return ""
	}
	name, _, _ := strings.Cut(file[i+len(pluginDir):], "/")

	return name
}

// report prints the coverage of each plugin and the total, for whoever runs
// the tests to read.
//
// Note that go tool covdata percent cannot report either of these: it only
// ever reports per package, and it prints a package which has no statements at
// all without a percentage and without a line break, running the line of the
// next package into it.
func (c *Coverage) report(w io.Writer) {
	line := func(label string, covered, statements int) {
		_, _ = fmt.Fprintf(w, "  %-28s %5.1f%% (%d/%d statements)\n", label,
			float64(percent(covered, statements)), covered, statements)
	}

	for _, name := range sortedKeys(c.Plugins) {
		plugin := c.Plugins[name]
		line(pluginDir+name, plugin.Covered, plugin.Statements)
	}

	if c.Statements > 0 {
		line("all instrumented packages", c.Covered, c.Statements)
	} else {
		_, _ = fmt.Fprintln(w, "  nothing instrumented to report on")
	}
}
