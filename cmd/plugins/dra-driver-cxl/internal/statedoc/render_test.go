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
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/containers"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
)

const (
	stateDir      = "../state"
	containersDir = "../containers"
	engineDir     = "../engine"
	kernelDir     = "../../../../../pkg/cxl/kernel"
	statesFile    = "../../../../../docs/memory/cxl/states.md"
	designFile    = "../../../../../docs/memory/cxl/design.md"
)

// TestStatesFileIsCurrent checks that docs/memory/cxl/states.md is what
// go generate renders from the state package now.
func TestStatesFileIsCurrent(t *testing.T) {
	want, err := render(stateDir, containersDir, engineDir, kernelDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(statesFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, want) {
		return
	}
	gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		g, w := "", ""
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("docs/memory/cxl/states.md is stale, run go generate ./cmd/plugins/dra-driver-cxl; line %d:\n  have: %s\n  want: %s", i+1, g, w)
		}
	}
}

// TestDesignLinksIntoStates checks that every link of design.md into
// states.md names a heading of states.md. The anchors are the slugs that
// the documentation gives headings of levels 1 to 4 (myst_heading_anchors).
func TestDesignLinksIntoStates(t *testing.T) {
	states, err := os.ReadFile(statesFile)
	if err != nil {
		t.Fatal(err)
	}
	design, err := os.ReadFile(designFile)
	if err != nil {
		t.Fatal(err)
	}
	anchors := headingAnchors(string(states))
	links := regexp.MustCompile(`\]\(states\.md#([^)]+)\)`).FindAllStringSubmatch(string(design), -1)
	if len(links) == 0 {
		t.Fatal("design.md has no links into states.md")
	}
	for _, m := range links {
		if !anchors[m[1]] {
			t.Errorf("design.md links states.md#%s, which no heading of states.md has", m[1])
		}
	}
}

// headingAnchors returns the anchors of the headings of levels 1 to 4 of
// a markdown document: the heading in lower case without its markup and
// punctuation, spaces as "-", and "-N" after the Nth repeat.
func headingAnchors(doc string) map[string]bool {
	heading := regexp.MustCompile(`^#{1,4}\s+(.*?)\s*$`)
	drop := regexp.MustCompile(`[^\p{L}\p{N}_\s-]`)
	anchors, seen, fence := map[string]bool{}, map[string]int{}, false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
			continue
		}
		m := heading.FindStringSubmatch(line)
		if fence || m == nil {
			continue
		}
		slug := drop.ReplaceAllString(strings.ToLower(strings.ReplaceAll(m[1], "`", "")), "")
		slug = strings.Join(strings.Fields(slug), "-")
		if n := seen[slug]; n > 0 {
			anchors[fmt.Sprintf("%s-%d", slug, n)] = true
		} else {
			anchors[slug] = true
		}
		seen[slug]++
	}
	return anchors
}

// TestEveryObjectHasItsHeader checks that the doc comment of every record
// type has every section of the header.
func TestEveryObjectHasItsHeader(t *testing.T) {
	docs, triggers, err := parseState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range stateObjects() {
		h, ok := docs[o.Name]
		if !ok {
			t.Errorf("%s: no header", o.Name)
			continue
		}
		if len(h.purpose) == 0 {
			t.Errorf("%s: no purpose", o.Name)
		}
		for _, l := range labels {
			if h.sections[l] == "" {
				t.Errorf("%s: no %q section", o.Name, l)
			}
		}
	}
	for _, tr := range state.Triggers {
		if triggers[string(tr)] == "" {
			t.Errorf("trigger %s: no doc comment", tr)
		}
	}
	docs, triggers, err = parseState(containersDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range containerObjects() {
		h, ok := docs[o.Name]
		if !ok || len(h.purpose) == 0 {
			t.Errorf("%s: no header", o.Name)
			continue
		}
		for _, l := range labels {
			if h.sections[l] == "" {
				t.Errorf("%s: no %q section", o.Name, l)
			}
		}
	}
	for _, tr := range containers.Triggers {
		if triggers[string(tr)] == "" {
			t.Errorf("trigger %s: no doc comment", tr)
		}
	}
}

// TestEveryKernelObjectHasItsHeader checks that every kind of kernel object
// of the catalogue has a header comment with a purpose and its states.
func TestEveryKernelObjectHasItsHeader(t *testing.T) {
	docs, err := parseKernel(kernelDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range kernelObjects() {
		h := docs[o.Name]
		if len(h) == 0 || h[0].label != "" || !strings.HasPrefix(h[0].text, o.Name+": ") {
			t.Errorf("%s: no header comment", o.Name)
			continue
		}
		states := false
		for _, p := range h {
			states = states || strings.HasPrefix(p.label, "State") || strings.Contains(p.text, "States:")
		}
		if !states {
			t.Errorf("%s: the header names no state", o.Name)
		}
	}
}

func TestEscape(t *testing.T) {
	if got := escape("a | b * <uid>"); got != `a \| b \* &lt;uid>` {
		t.Errorf("escape: %q", got)
	}
}

// The release policy has a row per kind of memdev with the decisions of
// engine.Policy, and every kind, release and decision has its text.
func TestPolicyMatrix(t *testing.T) {
	var b bytes.Buffer
	if err := renderPolicy(&b, engineDir); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| local | destroy | in order | in order | destroy |",
		"| local persistent | in order, kept idle | in order | in order, kept idle | in order, kept idle |",
		"| pool | destroy | destroy | destroy; keep | destroy |",
		"- `keep idle`: Leaves the region as it is for the idle memdev",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}
	if err := renderPolicy(&b, t.TempDir()); err == nil {
		t.Error("a policy without the doc comments")
	}
}

// TestEngineInvariants reads the engine's invariants from its source:
// each has a name I<n> of its own and a text.
func TestEngineInvariants(t *testing.T) {
	invs, err := invariantDocs(engineDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) == 0 {
		t.Fatal("no invariants")
	}
	name := regexp.MustCompile(`^I[0-9]+$`)
	seen := map[string]bool{}
	for _, inv := range invs {
		if !name.MatchString(inv[0]) || seen[inv[0]] || inv[1] == "" {
			t.Errorf("invariant %q: %q", inv[0], inv[1])
		}
		seen[inv[0]] = true
	}
}
