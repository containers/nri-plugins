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
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Lines as the logs we publish really write them. The short forms and the
// logfmt ones come from a plugin log of a real run, which has both: a plugin
// logs with its headers until debugging is turned on and skips them after. The
// syslog ones are a runtime's log as the journal hands it over.
var logLines = []struct {
	line  string
	class string
	text  string
	meta  string
}{{
	line:  `D: [              policy              ] <pool-setup><virtual root>`,
	class: sevDebug,
	text:  `D: [              policy              ] <pool-setup><virtual root>`,
}, {
	line:  `I: [               http               ] stopping HTTP server...`,
	class: sevPlain,
	text:  `I: [               http               ] stopping HTTP server...`,
}, {
	line:  `E: [              policy              ] ignoring invalid preference`,
	class: sevError,
	text:  `E: [              policy              ] ignoring invalid preference`,
}, {
	line:  `W: [              cache               ] no cache to load`,
	class: sevWarn,
	text:  `W: [              cache               ] no cache to load`,
}, {
	// Our own logger with its headers: the level first, then the time.
	line:  `level=WARN time=2026-09-16T06:59:59.1101Z msg="less strict permissions"`,
	class: sevWarn,
	text:  `msg="less strict permissions"`,
	meta:  `level=WARN time=2026-09-16T06:59:59.1101Z`,
}, {
	line:  `level=INFO time=2026-09-16T06:59:59.1061Z msg="registering controller"`,
	class: sevPlain,
	text:  `msg="registering controller"`,
	meta:  `level=INFO time=2026-09-16T06:59:59.1061Z`,
}, {
	line:  `level=ERROR time=2026-09-16T06:59:59.1141Z msg="failed to get cpus"`,
	class: sevError,
	text:  `msg="failed to get cpus"`,
	meta:  `level=ERROR time=2026-09-16T06:59:59.1141Z`,
}, {
	// Logrus, which the NRI library logs with: the time first, quoted.
	line:  `time="2026-09-16T06:59:59Z" level=info msg="Created plugin"`,
	class: sevPlain,
	text:  `msg="Created plugin"`,
	meta:  `time="2026-09-16T06:59:59Z" level=info`,
}, {
	// A runtime's log, as the journal writes it: the level is a hundred
	// characters into the line, behind the host and the unit.
	line: `Sep 16 06:59:53 n4c128-fedora-43-containerd containerd[9576]: ` +
		`time="2026-09-16T06:59:53.456005606Z" level=info msg="No images store"`,
	class: sevPlain,
	text:  `msg="No images store"`,
	meta: `Sep 16 06:59:53 n4c128-fedora-43-containerd containerd[9576]: ` +
		`time="2026-09-16T06:59:53.456005606Z" level=info`,
}, {
	// Logrus says warning where we say warn.
	line: `Sep  6 06:59:53 host crio[9576]: time="2026-09-16T06:59:53Z" ` +
		`level=warning msg="something to look at"`,
	class: sevWarn,
	text:  `msg="something to look at"`,
	meta: `Sep  6 06:59:53 host crio[9576]: time="2026-09-16T06:59:53Z" ` +
		`level=warning`,
}, {
	line:  `-- Boot 6c3a4e6f1b2c4d5e6f7a8b9c0d1e2f30 --`,
	class: sevPlain,
	text:  `-- Boot 6c3a4e6f1b2c4d5e6f7a8b9c0d1e2f30 --`,
}, {
	line:  `no severity anywhere in this line`,
	class: sevPlain,
	text:  `no severity anywhere in this line`,
}}

func TestReadLogLine(t *testing.T) {
	for _, tc := range logLines {
		read := readLogLine(tc.line)
		if read.class != tc.class {
			t.Errorf("%q: severity %q, expected %q", tc.line, read.class, tc.class)
		}
		if read.text != tc.text {
			t.Errorf("%q: text %q, expected %q", tc.line, read.text, tc.text)
		}
		if strings.TrimSpace(read.meta) != tc.meta {
			t.Errorf("%q: header %q, expected %q", tc.line, read.meta, tc.meta)
		}
		// Whatever we take off the front has to be the front: a line is its
		// header and what is left of it, and nothing of it may go missing.
		if read.meta+read.text != tc.line {
			t.Errorf("%q: header and text do not add up to it", tc.line)
		}
	}
}

func TestSeverityClass(t *testing.T) {
	for level, want := range map[string]string{
		"D": sevDebug, "debug": sevDebug, "DEBUG": sevDebug, "trace": sevDebug,
		"W": sevWarn, "warn": sevWarn, "warning": sevWarn, "WARNING": sevWarn,
		"E": sevError, "error": sevError, "fatal": sevError, "panic": sevError,
		"I": sevPlain, "info": sevPlain, "notice": sevPlain,
		"": sevPlain, "whatever": sevPlain,
	} {
		if got := severityClass(level); got != want {
			t.Errorf("severity of %q: %q, expected %q", level, got, want)
		}
	}
}

// TestRenderLogGroupsRuns checks what keeps a log of a hundred thousand lines
// from becoming a hundred thousand elements: consecutive lines of one severity
// share the span which colours them.
func TestRenderLogGroupsRuns(t *testing.T) {
	log := strings.Repeat("D: [ policy ] thinking\n", 500) +
		"E: [ policy ] that went badly\n" +
		strings.Repeat("D: [ policy ] thinking again\n", 500)

	page := string(renderLog(log))

	if got := strings.Count(page, `<span class="d">`); got != 2 {
		t.Errorf("%d spans for two runs of debug lines, expected 2", got)
	}
	if got := strings.Count(page, `<span class="e">`); got != 1 {
		t.Errorf("%d spans for one error line, expected 1", got)
	}
	if got := strings.Count(page, "\n"); got != 1001 {
		t.Errorf("%d lines in the page, expected the 1001 of the log", got)
	}
	if opened, closed := strings.Count(page, "<span"), strings.Count(page, "</span>"); opened != closed {
		t.Errorf("%d spans opened, %d closed", opened, closed)
	}
}

// TestRenderLogLeavesNoGapBetweenBands checks that nothing is left between two
// bands. A band is a block, so a newline between two of them is a block holding
// nothing but a line break, which shows as an empty line between every pair of
// bands -- which is not what a log should read like.
func TestRenderLogLeavesNoGapBetweenBands(t *testing.T) {
	log := "D: [ p ] one\nD: [ p ] two\nE: [ p ] bad\nW: [ p ] hmm\nplain\n"

	page := string(renderLog(log))

	if strings.Contains(page, "</span>\n<span") {
		t.Errorf("a newline is left between two bands: %q", page)
	}
	// One per line of the log, and none added or lost.
	if got := strings.Count(page, "\n"); got != 5 {
		t.Errorf("%d newlines in the page, expected the 5 of the log: %q", got, page)
	}
	// The two debug lines share one band, each line's newline inside it.
	if !strings.Contains(page, `<span class="d">D: [ p ] one`+"\n"+`D: [ p ] two`+"\n"+`</span>`) {
		t.Errorf("consecutive lines of one severity are not one band: %q", page)
	}
}

// TestRenderLogLeavesPlainTextAlone checks that a log we make nothing of costs
// nothing: info is the text of the page, so it needs no markup at all.
func TestRenderLogLeavesPlainTextAlone(t *testing.T) {
	log := "just some output\nand more of it\n"

	if page := string(renderLog(log)); page != log {
		t.Errorf("a plain log is marked up: %q", page)
	}
}

// TestRenderLogHangsTheHeaderOnAMark checks that what was taken off the front of
// a line is there to be looked at, on something to hover over.
func TestRenderLogHangsTheHeaderOnAMark(t *testing.T) {
	log := `Sep 16 06:59:53 host containerd[9576]: time="2026-09-16T06:59:53Z" ` +
		`level=error msg="it broke"` + "\n"

	page := string(renderLog(log))

	if !strings.Contains(page, `<span class="m" data-h="Sep 16 06:59:53 host containerd[9576]:`) {
		t.Errorf("the header of the line is not on a mark: %q", page)
	}
	if !strings.Contains(page, `level=error"`) {
		t.Errorf("the level is not kept with the rest of the header: %q", page)
	}
	if !strings.Contains(page, `msg=&#34;it broke&#34;`) {
		t.Errorf("the line does not say what it says: %q", page)
	}
	if strings.Contains(page, `>Sep 16`) {
		t.Errorf("the header is still in the text of the line: %q", page)
	}
}

// ourMarkup is every tag renderLog is allowed to have written. Everything else
// a log can hold is escaped, so anything which is not one of these and still
// looks like markup got there from the log itself.
var ourMarkup = regexp.MustCompile(
	`<span class="[dwe]">|<span class="m" data-h="[^"<>]*">|</span>`)

// onlyOurMarkup checks that a page holds no markup beyond what we wrote. What is
// left once our own tags are taken out is text and attribute values, and those
// cannot hold a quote or an angle bracket unescaped: one would end a tag or an
// attribute early, which is the whole of how a log could break out.
func onlyOurMarkup(t *testing.T, page string) {
	t.Helper()

	if rest := ourMarkup.ReplaceAllString(page, ""); strings.ContainsAny(rest, `<>"`) {
		t.Errorf("markup which is not ours in the page: %q", rest)
	}
}

// TestRenderLogEscapes checks that a log which reads like markup cannot become
// markup: what we hand the page is HTML, so escaping it is ours to do.
func TestRenderLogEscapes(t *testing.T) {
	log := "D: [ policy ] </pre><script>alert(1)</script>\n" +
		`level=error time=t msg="<img src=x onerror=alert(2)>"` + "\n"

	page := string(renderLog(log))

	onlyOurMarkup(t, page)
	if !strings.Contains(page, "alert(1)") || !strings.Contains(page, "alert(2)") {
		t.Errorf("the log is not in the page at all: %q", page)
	}
}

// TestRenderLogTitleCannotBreakOut checks the same for a header, which ends up
// in an attribute rather than in the text of the line.
func TestRenderLogTitleCannotBreakOut(t *testing.T) {
	for _, log := range []string{
		`time="\" onmouseover=alert(1) x=\"" level=warn msg="hello"` + "\n",
		`Sep 16 06:59:53 host unit[1]: time="x" level="><script>" msg="hi"` + "\n",
	} {
		onlyOurMarkup(t, string(renderLog(log)))
	}
}

// TestServeViewsAPackedLog checks that a log inside the archive of a packed run
// is read as a page too. That is where the log of a plugin is, once a run has
// been published packed, so a view which only worked on files would hardly ever
// work at all.
func TestServeViewsAPackedLog(t *testing.T) {
	root, name := newRoot(t, true)
	log := "vm/" + suiteDir + "/balloons/test01/" + testLog

	got := getView(t, root, "/"+name+"/"+log)

	if got.Code != http.StatusOK {
		t.Fatalf("viewing a log inside the archive of a run: %d", got.Code)
	}
	if kind := got.Header().Get("Content-Type"); !strings.HasPrefix(kind, "text/html") {
		t.Errorf("a packed log is viewed as %q, expected text/html", kind)
	}
	if !strings.Contains(got.Body.String(), files[log]) {
		t.Errorf("the page does not carry the log: %q", got.Body)
	}
	// Nothing in a packed run is still being written to.
	if !strings.Contains(got.Body.String(), `data-every="0"`) {
		t.Errorf("a log inside an archive is followed: %q", got.Body)
	}
	// Named as it was asked for, and not as it is called inside the archive:
	// one log, one title, packed or not.
	if !strings.Contains(got.Body.String(), "<title>"+name+"/"+log+"</title>") {
		t.Errorf("a packed log is not titled by the path it was asked for")
	}
}

// TestViewHref checks which links are turned into a page to read, and that a
// link which already says how to read it is left as it is.
func TestViewHref(t *testing.T) {
	for href, want := range map[string]string{
		"vm/suite/p/test01/nri-resource-policy.output.txt": "?" + viewQuery,
		"vm/suite/p/test01/runtime.containerd.log.txt":     "?" + viewQuery,
		"vm/suite/p/test01/runtime.crio.log.txt":           "?" + viewQuery,
		"vm/suite/p/test01/run.sh.output.txt":              "?" + viewQuery,
		"vm/suite/p/test01/pyexec.output.txt":              "?" + viewQuery,
		// Inside the archive it was packed into, which is where a published
		// test case keeps its logs.
		"vm/suite/p/test01/artifacts.tar.xz/nri-resource-policy.output.txt": "?" + viewQuery,
		// Not logs, and not ours to make pages of.
		"vm/suite/p/test01/summary.txt":      "",
		"vm/suite/p/test01/pyexec.py":        "",
		"vm/suite/p/test01/artifacts.tar.xz": "",
		"vm/suite/p/test01/commands":         "",
		"coverage-report/coverage.html":      "",
		// The log of the runner is linked by the report itself, which says
		// whether the run is still writing to it.
		"e2e-runner.log.txt": "",
	} {
		if got := viewHref(href); got != href+want {
			t.Errorf("viewHref(%q) = %q, expected %q", href, got, href+want)
		}
	}

	// A query already there is not doubled: a run reported on by a version
	// which stored it keeps what it stored.
	stored := "vm/suite/p/test01/nri-resource-policy.output.txt?" + viewQuery
	if got := viewHref(stored); got != stored {
		t.Errorf("viewHref(%q) = %q, expected it unchanged", stored, got)
	}
}

// TestFollowFor checks the interval a page is told to come back at, which a
// request may ask for and which is held to something sane when it does.
func TestFollowFor(t *testing.T) {
	for asked, want := range map[string]time.Duration{
		"":       followInterval,
		"2s":     2 * time.Second,
		"1500ms": 1500 * time.Millisecond,
		// Held to the floor and the ceiling.
		"1ms": followMinimum,
		"2h":  followMaximum,
		// Nothing at all: a log which is still growing, read without it moving.
		"0":  0,
		"0s": 0,
		// Nonsense falls back to the default rather than taking the log away.
		"-5s":    followInterval,
		"banana": followInterval,
		"2":      followInterval,
	} {
		url := "/run/" + runnerLog + "?" + viewQuery
		if asked != "" {
			url += "&" + refreshQuery + "=" + asked
		}
		got := followFor(httptest.NewRequest(http.MethodGet, url, nil))
		if got != want {
			t.Errorf("refresh=%q: %v, expected %v", asked, got, want)
		}
	}
}

// TestServeFollowsAtTheIntervalAsked checks that the interval reaches the page,
// and that a log nothing is writing to stays static however it is asked for.
func TestServeFollowsAtTheIntervalAsked(t *testing.T) {
	root, finished := newRoot(t, false)
	ongoing := "test-2026-09-17-2251"
	newOngoingRun(t, root, ongoing)

	got := get(t, root, "/"+ongoing+"/"+runnerLog+"?"+viewQuery+"&"+refreshQuery+"=2s")
	if !strings.Contains(got.Body.String(), `data-every="2000"`) {
		t.Errorf("a log asked for every 2s does not say so: %q", cut(got.Body.String(), 400))
	}

	// Nothing is writing to the log of a run which has ended, so there is
	// nothing to come back for whatever the request asks.
	got = get(t, root, "/"+finished+"/"+runnerLog+"?"+viewQuery+"&"+refreshQuery+"=2s")
	if !strings.Contains(got.Body.String(), `data-every="0"`) {
		t.Errorf("a finished run's log is followed when asked: %q", cut(got.Body.String(), 400))
	}
}
