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
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	// viewQuery asks for a log to be served as a page which reads it, rather
	// than as the file it is.
	viewQuery = "view"
	// refreshQuery asks for a followed log to be come back for at another
	// interval than the default.
	refreshQuery = "refresh"
	// followInterval is how often a log which is still being written to is
	// asked for what has been written to it since. A run takes hours, so this
	// is about how live a log looks, not about keeping up with it.
	followInterval = 3 * time.Second
	// idleInterval is how often the index is worth coming back for with no run
	// going. The only news it can carry then is a run having started, which
	// nothing else tells a browser about, so the page has to ask -- but seldom,
	// a run being minutes of setup before it says anything.
	idleInterval = 30 * time.Second
	// followMinimum and followMaximum are what an interval asked for is held
	// to. The fetch is only what a log has grown by, so the floor is about
	// somebody asking for milliseconds rather than about the cost of a second.
	followMinimum = time.Second
	followMaximum = time.Hour
	// viewLimit is the largest log we make a page of. A run's own log and the
	// log of a plugin run with debugging on are tens of megabytes, which a
	// browser copes with; there is no sense in finding out where it stops.
	viewLimit = 64 << 20
)

// Severities we tell apart, as the classes which colour them. Info is the plain
// text of the page, and so is a line we make nothing of: neither is worth a
// class of its own, and leaving them unwrapped is what keeps the markup of a
// log which is almost all one severity down to almost nothing.
const (
	sevPlain = ""
	sevDebug = "d"
	sevWarn  = "w"
	sevError = "e"
)

// shortLevel is our own logger with the headers skipped: the severity as a
// letter, followed by the source of the message in brackets. The brackets are
// the source and not the severity, so there is nothing to take off here.
var shortLevel = regexp.MustCompile(`^([DIWE]): `)

// syslogPrefix is what the journal puts in front of a line of a runtime's log:
// the month, the day, the time, the host and the unit with its pid. All of it
// the same for every line of a log collected from one host during one test.
var syslogPrefix = regexp.MustCompile(
	`^[A-Z][a-z]{2}\s+\d{1,2} \d{2}:\d{2}:\d{2} \S+ \S+\[\d+\]: `)

// logfmtHeader is the head of a logfmt line, as our own logger writes it with
// its headers and as logrus writes it for containerd, cri-o and the NRI
// library. Either order: we put the level first, logrus puts the time first.
var logfmtHeader = regexp.MustCompile(
	`^(?:(?:time|level)=(?:"[^"]*"|\S*) )+`)

// levelValue reads the level out of a logfmt header. Quoted or not: RE2 has no
// backreference to pair the quotes with, and a level is a word either way.
var levelValue = regexp.MustCompile(`(?i)\blevel="?([A-Za-z]+)`)

// logLine is what we made of one line of a log.
type logLine struct {
	// text is what the line says, and all of it unless meta took something off.
	text string
	// meta is what we took off the front as being the same on every line, kept
	// for whoever wants to see it after all.
	meta string
	// class is the severity the line was logged at, sevPlain if we cannot tell
	// or if it is plainly informational.
	class string
}

// readLogLine tells what a line of a log says, at which severity, and what of
// it is worth taking out of the way.
//
// Only the header of a line is ever taken off, and only where it repeats on
// every line: the timestamp, the host, the unit and its pid, and the level,
// which the colour says instead. What a line actually says is never touched.
func readLogLine(line string) logLine {
	// Our own short form keeps its prefix: the padded source in it lines the
	// messages up, and reading a log without it is worse, not better.
	if m := shortLevel.FindStringSubmatch(line); m != nil {
		return logLine{text: line, class: severityClass(m[1])}
	}

	meta := ""
	rest := line
	if prefix := syslogPrefix.FindString(rest); prefix != "" {
		meta, rest = prefix, rest[len(prefix):]
	}
	if header := logfmtHeader.FindString(rest); header != "" {
		meta, rest = meta+header, rest[len(header):]
	}

	class := sevPlain
	if m := levelValue.FindStringSubmatch(meta); m != nil {
		class = severityClass(m[1])
	}

	return logLine{text: rest, meta: meta, class: class}
}

// severityClass is the class which colours a severity, however it was written.
func severityClass(level string) string {
	switch strings.ToLower(level) {
	case "d", "debug", "trace":
		return sevDebug
	case "w", "warn", "warning":
		return sevWarn
	case "e", "error", "err", "fatal", "panic", "crit", "critical":
		return sevError
	}

	// Info, notice, and anything we do not know: the text of the page.
	return sevPlain
}

// renderLog marks up a log for reading: each line coloured by the severity it
// was logged at, and what was taken off its front on hand as a tooltip.
//
// Consecutive lines of one severity share a span rather than getting one each.
// Severity comes in long runs -- a plugin log of 158000 lines, almost all of
// them debug, has 1357 such runs -- so this is the difference between markup
// which costs nothing and megabytes of it.
func renderLog(text string) template.HTML {
	page := &strings.Builder{}
	page.Grow(len(text) + len(text)/8)

	open := sevPlain
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		read := readLogLine(line)

		// The newline of a line goes inside the span which colours it, and
		// nothing goes between two spans: a span is a band, which is a block,
		// so a newline left between two of them is a block of its own holding
		// nothing but a line break, and that shows as an empty line. At the end
		// of a block which has something else in it the same newline is
		// suppressed, which is also what keeps a line of plain text in front of
		// a band from making one.
		if read.class != open {
			closeSpan(page, open)
			openSpan(page, read.class)
			open = read.class
		}

		if read.meta != "" {
			// Hung on a mark of its own at the head of the line, so that
			// reading the line does not keep raising a tooltip over it. On an
			// attribute of ours and not on title=, the delay of which belongs
			// to the browser; the page raises it itself instead.
			page.WriteString(`<span class="m" data-h="`)
			page.WriteString(template.HTMLEscapeString(strings.TrimSpace(read.meta)))
			page.WriteString(`">` + metaMark + `</span>`)
		}
		page.WriteString(template.HTMLEscapeString(read.text))
		page.WriteString("\n")
	}
	closeSpan(page, open)

	return template.HTML(page.String())
}

// metaMark is what there is to hover over at the head of a line whose header we
// took off. An ellipsis says what it is, something left out, and the padding
// around it in the page is what makes it big enough to hit with a trackpad.
const metaMark = "&hellip;"

func openSpan(page *strings.Builder, class string) {
	if class != sevPlain {
		page.WriteString(`<span class="` + class + `">`)
	}
}

func closeSpan(page *strings.Builder, class string) {
	if class != sevPlain {
		page.WriteString(`</span>`)
	}
}

// viewed tells whether a request asks for a log to be read as a page.
//
// Only the .txt files, which is what every log a report links is: a page of
// anything else would be a page of whatever it happens to look like.
func viewed(r *http.Request, name string) bool {
	return r.URL.Query().Has(viewQuery) && strings.HasSuffix(name, ".txt")
}

// viewHref links a log to be read as a page.
//
// Decided when a report is rendered rather than when a run is scanned, so that
// the report of a run published before any of this gets the link as well, the
// moment anything renders it again. A link which already says how to read it is
// left alone: a run reported on by a version which stored the query keeps it.
func viewHref(href string) string {
	if strings.Contains(href, "?") || !viewableLog(href) {
		return href
	}

	return href + "?" + viewQuery
}

// viewableLog tells whether a link names a log worth reading as a page. A log
// may be named inside the archive it was packed into, so only the last element
// of the path counts.
func viewableLog(href string) bool {
	name := path.Base(href)
	if strings.HasPrefix(name, "runtime.") && strings.HasSuffix(name, ".log.txt") {
		return true
	}
	for _, a := range artifacts {
		if a.log && a.name == name {
			return true
		}
	}

	return false
}

// refreshFor is how often a page should come back for what has changed since.
// The given default unless the request asks otherwise, held to something sane if
// it does, and nothing at all for an interval of zero: a page which is still
// moving is worth reading without it moving too.
//
// The default is the caller's because what is worth waiting for differs: a log
// grows by the line, an index of runs which have all ended by nothing at all.
//
// Anything we cannot make sense of falls back to the default rather than
// answering an error. A mistyped interval should not take the page away.
func refreshFor(r *http.Request, dflt time.Duration) time.Duration {
	asked := r.URL.Query().Get(refreshQuery)
	if asked == "" {
		return dflt
	}

	every, err := time.ParseDuration(asked)
	switch {
	case err != nil || every < 0:
		return dflt
	case every == 0:
		return 0
	case every < followMinimum:
		return followMinimum
	case every > followMaximum:
		return followMaximum
	}

	return every
}

// stillRunning tells whether the run a log belongs to is still writing to it.
//
// Both halves are needed. A run says RUNNING as soon as it has somewhere to say
// it and only says how it went once it knows, so a run which was killed says
// RUNNING for good; without the log having been written to lately, a page would
// go on following such a log until it was closed.
func (s *Server) stillRunning(run string, log fs.FileInfo) bool {
	if time.Since(log.ModTime()) >= staleAfter {
		return false
	}

	status, err := fs.ReadFile(s.dir.FS(), path.Join(run, statusTxt))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(status))

	return len(fields) > 0 && fields[0] == "RUNNING"
}

// serveLogView serves a log as a page which reads it: coloured by severity, and
// with the headers of its lines out of the way. A log which is still being
// written to is followed as well, the page asking for what has been added to it
// every followInterval.
//
// The log itself stays exactly what it is under the same name without the
// query, plain text to the byte, which is both what this page reads it with and
// what everything else which reads a log goes on getting.
func (s *Server) serveLogView(w http.ResponseWriter, r *http.Request, name string,
	text []byte, follow bool) {
	interval := int64(0)
	if follow {
		interval = refreshFor(r, followInterval).Milliseconds()
	}

	page := &strings.Builder{}
	err := logPage.Execute(page, map[string]any{
		"Name": name,
		"File": path.Base(name),
		"Log":  renderLog(string(text)),
		// What we serve here is where the page asks for the rest from, so that
		// whatever is written while we are serving it is picked up and not
		// skipped.
		"Offset": len(text),
		// Nothing to wait for unless the log is still being written to, and no
		// interval to wait says exactly that to the page.
		"Interval": interval,
		// Verbatim, so that it keeps the comments which say why it does what it
		// does: html/template strips those from a script it is left to write.
		"Script": template.JS(logScript),
	})
	if err != nil {
		http.Error(w, "failed to render "+name, http.StatusInternalServerError)
		return
	}

	// No modification time: the page is made up, and what changes in it is the
	// log it carries, which it fetches for itself anyway.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(page.String()))
}

var logPage = template.Must(template.New("log").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>{{.Name}}</title>
<style>
body { margin: 0; --debug: #f2f2f2; --warn: #fff3cd; --error: #ffe0e0;
       --tip: #333; --tip-fg: #f4f4f4; }
pre { margin: 0; padding: 1em; font-family: monospace; white-space: pre-wrap;
      word-break: break-word; line-height: 1.4; }
/* A band across the page rather than a mark around the text: the severity of a
   line is about the line, and a background which stops where the text does is
   harder to read past than no background at all. */
.d, .w, .e { display: block; }
.d { background: var(--debug); }
.w { background: var(--warn); }
.e { background: var(--error); }
.m { position: relative; padding: .4em .5em; margin-right: .2em; opacity: .45;
     cursor: help; }
.m:hover { opacity: .9; }
/* The header raised by the page rather than by title=, whose delay is the
   browser's own, near a second, and out of reach of any CSS. A tenth of a
   second reads as no wait at all and is still enough that a pointer travelling
   down the column raises nothing on its way past.

   Kept on one line: a header is a fixed shape, 117 characters at the longest of
   the 93029 in 220 real runtime logs and 109 at the median, which comes to some
   870px of monospace and fits any window worth reading a log in. Wrapping one at
   whatever column it reaches is what made it hard to read in the first place, so
   a header too long for the window is clipped rather than wrapped.

   The rule is on :hover so that the box exists only while it is wanted: a
   runtime log runs to some 2000 lines, and every one of them has a header. That
   is also why this is an animation and not a transition -- there is no earlier
   state for a box which did not exist a moment ago to transition from. */
.m:hover::after {
  content: attr(data-h); position: absolute; z-index: 1; left: 0; top: 1.5em;
  padding: .3em .5em; border-radius: 3px; font-size: .95em;
  background: var(--tip); color: var(--tip-fg);
  white-space: pre; max-width: calc(100vw - 2em); overflow: hidden;
  text-overflow: ellipsis;
  opacity: 0; animation: tip .08s ease-out .1s forwards;
}
@keyframes tip { to { opacity: 1; } }
@media (prefers-color-scheme: dark) {
  /* Dark enough to keep the text on them legible, which a light tint would not
     be: the foreground here is light. */
  body { background: #1b1b1b; color: #ddd;
         --debug: #242424; --warn: #423a1e; --error: #4a2326;
         --tip: #e8e8e8; --tip-fg: #1b1b1b; }
}
</style>
</head>
<body>
<pre id="log" data-log="{{.File}}" data-offset="{{.Offset}}"
     data-every="{{.Interval}}">{{.Log}}</pre>
<script>{{.Script}}</script>
</body>
</html>
`))

// logScript is what makes a log which is still being written to keep up with
// itself. What it needs is in the page, on the element it appends to: the log to
// read, how much of it is there already, and how often to come back for more.
//
// What it appends is text as it comes: a line which arrives after the page was
// made is not coloured, since the severities are told apart where the log is
// read, not here. Only a run's own log is ever followed, and nothing colours
// that, so there is nothing to see.
const logScript = `
(function () {
  var log = document.getElementById("log");
  var url = log.getAttribute("data-log");
  var offset = Number(log.getAttribute("data-offset"));
  var every = Number(log.getAttribute("data-every"));
  var decoder = new TextDecoder();

  // Where the end is depends on how the log wrapped, so measure it rather than
  // remember it.
  function atEnd() {
    return window.innerHeight + window.scrollY >= document.body.scrollHeight - 4;
  }
  function toEnd() {
    window.scrollTo(0, document.body.scrollHeight);
  }

  toEnd();

  // No interval to wait means nothing is writing to this log any more, so
  // there is nothing to come back for.
  if (every <= 0) {
    return;
  }

  function poll() {
    // Decided before appending anything: afterwards the end has moved, and
    // every reader is away from it.
    var follow = atEnd();

    fetch(url, {headers: {"Range": "bytes=" + offset + "-"}, cache: "no-store"})
      .then(function (answer) {
        // 206 or nothing to add: 416 is the answer until the log grows, and a
        // run which has been packed up has no log here any more at all.
        return answer.status === 206 ? answer.arrayBuffer() : null;
      })
      .then(function (bytes) {
        if (!bytes || bytes.byteLength === 0) {
          return;
        }
        offset += bytes.byteLength;
        // Decoded as a stream: what we were given may well end in the middle of
        // a multi-byte character, the rest of which comes next time.
        log.appendChild(document.createTextNode(
          decoder.decode(bytes, {stream: true})));
        if (follow) {
          toEnd();
        }
      })
      .catch(function () {})
      .then(function () {
        window.setTimeout(poll, every);
      });
  }
  window.setTimeout(poll, every);
})();
`
