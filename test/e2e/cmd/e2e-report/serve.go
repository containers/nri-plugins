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
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// sniffLen is what http.DetectContentType looks at.
	sniffLen = 512
	// keptListings is how many tarball listings we hold on to.
	keptListings = 32
	// keptBytes is the largest tarball we read into memory to browse it. The
	// artifacts of a test case are a few hundred kilobytes packed, and
	// unpacking the archive of the run for every file of them would be silly.
	keptBytes = 64 << 20
)

// Server serves the published results of e2e test runs: the packed ones as if
// their archive had been extracted, and what a test packed up for itself as if
// it, too, had been extracted where it is.
type Server struct {
	root     string
	dir      *os.Root
	index    indexCache
	mutex    sync.Mutex
	tarballs map[string]*Tarball
	order    []string
}

// indexCache is the index of the runs as it was last built, and what the root
// looked like then. A request which finds nothing changed costs a stat or three
// per run instead of a read of every report, and one which finds a single run
// changed pays for that run only.
//
// Worth having not for what one page costs, which nobody would notice, but for
// how often it is asked for: while any run is going the index asks to be fetched
// again every few seconds, so building it is paid continuously rather than once
// per visit.
type indexCache struct {
	mutex sync.Mutex
	print string
	page  []byte
	// running says whether any run in the page is still going, which decides
	// whether a browser is asked to come back for it. Cached with the page: a
	// request answered from the cache has no runs to ask.
	running bool
	runs    map[string]cachedRun
}

// cachedRun is a run we have read, and what the files a row of it comes from
// looked like when we did.
type cachedRun struct {
	print string
	run   *Run
}

// NewServer serves the results published under root.
func NewServer(root string) (*Server, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if !isDir(resolved) {
		return nil, fmt.Errorf("no such directory: %s", root)
	}

	// Everything a request reaches is read through the root. os.Root refuses to
	// walk out of one, a symlink pointing away included, so what a request asks
	// for needs no checking of its own, and nothing can be swapped for a link
	// out between checking a path and opening it.
	dir, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}

	return &Server{root: resolved, dir: dir, tarballs: map[string]*Tarball{}}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// A report is rewritten whenever a run is reported on again, so let a
	// browser keep what we serve, but never without asking us first.
	w.Header().Set("Cache-Control", "no-cache")

	// Clean away any attempt at leading us out of the results. What is left is
	// a name to read through the root, which refuses the rest.
	clean := path.Clean("/" + r.URL.Path)
	name := within(clean)

	// Cleaning a path drops the trailing slash, and a tarball is downloaded
	// without one and browsed into with one.
	slash := strings.HasSuffix(r.URL.Path, "/")

	if tarball, member, ok := s.tarballFor(clean, slash); ok {
		s.serveTarball(w, r, tarball, member, clean, slash)
		return
	}

	// The index of the runs, under both the names it answers to. Nothing writes
	// one out, so there is never a file here to serve instead.
	if name == "." || name == indexHTML {
		s.serveIndex(w, r)
		return
	}

	// The report of a run, rendered from what the run recorded, which is what
	// gives a run published long ago the report this version writes -- for a run
	// which has been packed up as well, since its results.json is outside the
	// archive.
	if run, ok := s.reportedRun(name); ok {
		s.serveRunReport(w, r, run)
		return
	}

	if info, err := s.dir.Stat(name); err == nil {
		if !info.IsDir() {
			// Reading a log and looking at one are not the same thing, and
			// asking for a view of it is what says which.
			if viewed(r, name) {
				s.serveFileView(w, r, name, info)
				return
			}
			s.serveFile(w, r, name)
			return
		}
		s.serveDir(w, r, clean, name)
		return
	}

	s.servePacked(w, r, clean, slash)
}

// within is the name a request path goes by inside the root: relative, with the
// slashes it came with, and "." for the root itself.
func within(clean string) string {
	if name := strings.TrimPrefix(clean, "/"); name != "" {
		return name
	}

	return "."
}

// tarballFor tells which tarball a request reaches into and which member of it
// it asks for, following as many tarballs as the path names: the archive of a
// run holds the artifacts of each test packed up as well.
//
// The tarball itself, asked for without a trailing slash, is not reached into
// but downloaded, so it is no answer here.
func (s *Server) tarballFor(clean string, slash bool) (*Tarball, string, bool) {
	run, rest := split(clean)
	if run == "" {
		return nil, "", false
	}

	var current *Tarball
	member := ""

	for rest != "" {
		var head string
		head, rest, _ = strings.Cut(rest, "/")
		member = path.Join(member, head)

		if !isTarball(head) {
			continue
		}
		if next := s.reachInto(run, current, member, head); next != nil {
			current, member = next, ""
		}
	}

	if current == nil || (member == "" && !slash) {
		return nil, "", false
	}

	return current, member, true
}

// reachInto opens the tarball a path names, from a file where it is, from the
// archive of the run, or from the tarball we are already in.
func (s *Server) reachInto(run string, current *Tarball, member, name string) *Tarball {
	if current == nil {
		if src, err := newFileSource(s.dir, path.Join(run, member)); err == nil {
			if tarball, err := s.tarball(src, name); err == nil {
				return tarball
			}
			return nil
		}
		archive, err := s.runArchive(run)
		if err != nil {
			return nil
		}
		current = archive
	}

	if _, ok := current.Size(member); !ok {
		return nil
	}
	tarball, err := s.tarball(&memberSource{tarball: current, name: member}, name)
	if err != nil {
		log.Printf("failed to read %s: %v", member, err)
		return nil
	}

	return tarball
}

// runArchive is the archive of a run, if it has one.
func (s *Server) runArchive(run string) (*Tarball, error) {
	src, err := newFileSource(s.dir, path.Join(run, runTar))
	if err != nil {
		return nil, err
	}

	return s.tarball(src, runTar)
}

// tarball reads the listing of a tarball, and keeps it: reading a member walks
// the tarball from the start, but listing it is worth doing once.
func (s *Server) tarball(src source, name string) (*Tarball, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	id := src.id()
	if tarball, ok := s.tarballs[id]; ok && !tarball.Stale() {
		return tarball, nil
	}

	// A tarball inside another one is read into memory once, so that browsing
	// it does not unpack what it is in over and over.
	if inner, ok := src.(*memberSource); ok {
		if size, ok := inner.tarball.Size(inner.name); ok && size <= keptBytes {
			data, err := inner.tarball.Read(inner.name)
			if err != nil {
				return nil, err
			}
			src = &bytesSource{ident: id, data: data}
		}
	}

	tarball, err := openTarball(src, name)
	if err != nil {
		delete(s.tarballs, id)
		return nil, err
	}

	if _, ok := s.tarballs[id]; !ok {
		s.order = append(s.order, id)
	}
	s.tarballs[id] = tarball

	for len(s.order) > keptListings {
		delete(s.tarballs, s.order[0])
		s.order = s.order[1:]
	}

	return tarball, nil
}

// serveTarball serves a member of a tarball, or the listing of one.
func (s *Server) serveTarball(w http.ResponseWriter, r *http.Request, tarball *Tarball,
	member, clean string, slash bool) {
	if member == "" {
		s.listing(w, clean, browsable(tarball.List("")))
		return
	}

	if size, ok := tarball.Size(member); ok {
		file, err := tarball.Open(member)
		if err != nil {
			http.Error(w, "failed to read "+member, http.StatusInternalServerError)
			return
		}
		defer func() { _ = file.Close() }()

		// The results of a run are packed up once it has ended, so nothing in
		// here is still being written to and there is nothing to follow.
		if viewed(r, member) && size <= viewLimit {
			text, err := io.ReadAll(file)
			if err != nil {
				http.Error(w, "failed to read "+member, http.StatusInternalServerError)
				return
			}
			// Named as it was asked for, so that a log has the one title
			// whether it is read out of an archive or off the disk.
			s.serveLogView(w, r, within(clean), text, false)
			return
		}

		s.serveReader(w, r, file, member, size)
		return
	}

	if names := tarball.List(member); len(names) > 0 {
		if !slash {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		s.listing(w, clean, browsable(names))
		return
	}

	http.NotFound(w, r)
}

// servePacked serves what a run packed into its archive, as if the archive had
// been extracted where it is.
func (s *Server) servePacked(w http.ResponseWriter, r *http.Request, clean string, slash bool) {
	run, name := split(clean)
	if run == "" {
		http.NotFound(w, r)
		return
	}

	archive, err := s.runArchive(run)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	s.serveTarball(w, r, archive, name, clean, slash)
}

// serveDir serves the listing of a directory, and of what the archive of the
// run has under it: packing leaves the directories of a run behind, and the
// index of the run itself is served from where it is.
func (s *Server) serveDir(w http.ResponseWriter, r *http.Request, clean, name string) {
	if !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}

	names := []string{}
	if entries, err := fs.ReadDir(s.dir.FS(), name); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
	}

	if run, name := split(clean); run != "" {
		if archive, err := s.runArchive(run); err == nil {
			names = append(names, archive.List(name)...)
		}
	}

	s.listing(w, clean, browsable(names))
}

// browsable is a listing with the duplicates dropped, in order, and with a
// tarball in it listed twice: once to download and once to browse into.
func browsable(names []string) []string {
	listed, seen := []string{}, map[string]bool{}

	for _, name := range names {
		if isTarball(name) {
			name += "/"
			if !seen[name] {
				seen[name] = true
				listed = append(listed, name)
			}
			name = strings.TrimSuffix(name, "/")
		}
		if !seen[name] {
			seen[name] = true
			listed = append(listed, name)
		}
	}
	slices.Sort(listed)

	return listed
}

// serveFile serves a file of a run which is not packed. http.ServeContent
// tells what to serve it as, sniffing the files with no extension of their own,
// answers what a browser asks about the copy it already has, and serves a range
// of a file as well. Only http.ServeFile is not for us: it has an opinion about
// a path which ends in index.html, and the reports do end in index.html.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	file, err := s.dir.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	http.ServeContent(w, r, path.Base(name), info.ModTime(), file)
}

// serveFileView serves a log of a run which is not packed as a page which reads
// it, followed as it grows while the run is still writing to it.
func (s *Server) serveFileView(w http.ResponseWriter, r *http.Request, name string,
	info fs.FileInfo) {
	// A log too big to make a page of is still a log to read as it is.
	if info.Size() > viewLimit {
		s.serveFile(w, r, name)
		return
	}

	text, err := fs.ReadFile(s.dir.FS(), name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	run, _ := split("/" + name)
	s.serveLogView(w, r, name, text, s.stillRunning(run, info))
}

// serveReader serves the contents of a member of a tarball. Content type is
// told from the name, and sniffed for the files of a test which have no
// extension, such as its command transcripts: those are text and worth showing
// in a browser instead of downloading.
func (s *Server) serveReader(w http.ResponseWriter, r *http.Request, body io.Reader,
	name string, size int64) {
	kind := mime.TypeByExtension(path.Ext(name))

	reader := bufio.NewReaderSize(body, sniffLen)
	if kind == "" {
		head, err := reader.Peek(sniffLen)
		if err != nil && len(head) == 0 && size > 0 {
			http.Error(w, "failed to read "+name, http.StatusInternalServerError)
			return
		}
		kind = http.DetectContentType(head)
	}

	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, reader); err != nil {
		log.Printf("failed to serve %s: %v", name, err)
	}
}

// split tells the run a request is for from the rest of the path.
func split(clean string) (string, string) {
	rest := strings.TrimPrefix(clean, "/")
	run, name, _ := strings.Cut(rest, "/")

	return run, name
}

func (s *Server) listing(w http.ResponseWriter, dir string, names []string) {
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := listingPage.Execute(w, map[string]any{"Dir": dir, "Names": names})
	if err != nil {
		log.Printf("failed to serve the listing of %s: %v", dir, err)
	}
}

var listingPage = template.Must(template.New("listing").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>{{.Dir}}</title>
<style>
body { font-family: sans-serif; margin: 2em; }
a { text-decoration: none; }
li { font-family: monospace; }
@media (prefers-color-scheme: dark) {
  body { background: #1b1b1b; color: #ddd; }
  a { color: #7ab8f5; }
}
</style>
</head>
<body>
<h1>{{.Dir}}</h1>
<ul>
<li><a href="../">../</a></li>
{{- range .Names}}
<li><a href="{{.}}">{{.}}</a></li>
{{- end}}
</ul>
</body>
</html>
`))

// runPrint is what the files a row of a run comes from look like now: what the
// run recorded, and the log of the runner, which is all that changes while a run
// is still collecting. Everything a row says comes from these, so a row cannot
// go stale without one of them moving.
func (s *Server) runPrint(name string) string {
	print := &strings.Builder{}

	print.WriteString(name)
	for _, file := range []string{resultsJSON, runnerLog} {
		if info, err := s.dir.Stat(path.Join(name, file)); err == nil {
			fmt.Fprintf(print, "|%s,%d,%d", file, info.Size(), info.ModTime().UnixNano())
		} else {
			fmt.Fprintf(print, "|%s,-", file)
		}
	}
	print.WriteString(";")

	return print.String()
}

// renderIndex is the index of the runs as they are now, rendered, reusing
// whatever has not changed since the last time it was asked for.
func (s *Server) renderIndex() ([]byte, bool, error) {
	names, err := readDir(s.root)
	if err != nil {
		return nil, false, err
	}

	// Every name, not only the runs among them, so that a directory which has
	// become one since is noticed too.
	prints, whole := make(map[string]string, len(names)), &strings.Builder{}
	for _, name := range names {
		prints[name] = s.runPrint(name)
		whole.WriteString(prints[name])
	}

	s.index.mutex.Lock()
	defer s.index.mutex.Unlock()

	if s.index.page != nil && s.index.print == whole.String() {
		return s.index.page, s.index.running, nil
	}

	runs, read := []*Run{}, make(map[string]cachedRun, len(names))
	for _, name := range names {
		dir := filepath.Join(s.root, name)
		if !isRun(dir) {
			continue
		}

		// Reading a report is what costs here, so a run whose files have not
		// moved is taken as it was. Built afresh rather than pruned, so a run
		// which has been pruned drops out of it by itself.
		if was, cached := s.index.runs[name]; cached && was.print == prints[name] {
			runs, read[name] = append(runs, was.run), was
			continue
		}

		run, err := readRunFor(dir, name)
		if err != nil {
			return nil, false, err
		}

		// A row has to name the directory it links to. That is what a run called
		// itself for anything the runner published, but the link has to work even
		// for a run whose results say otherwise.
		run.Name = name
		runs, read[name] = append(runs, run), cachedRun{print: prints[name], run: run}
	}

	sortRuns(runs)

	page, err := renderPage("index", newIndexPage(runs))
	if err != nil {
		return nil, false, err
	}

	s.index.print, s.index.page, s.index.runs = whole.String(), page, read
	s.index.running = anyRunning(runs)

	return page, s.index.running, nil
}

// reportedRun tells which run a request asks for the report of, if that is what
// it asks for: a run's report is the index.html of its directory, asked for by
// that name or through the directory itself. There is no such file -- every
// report is rendered, so what has to be there is the run.
func (s *Server) reportedRun(name string) (string, bool) {
	run, rest := split("/" + name)
	if run == "" || run == "." || (rest != "" && rest != indexHTML) {
		return "", false
	}
	// A run is a single element of a path which has been cleaned, so there is
	// nothing in it to lead anywhere out of the root.
	if !isRun(filepath.Join(s.root, run)) {
		return "", false
	}

	return run, true
}

// serveRunReport renders the report of a run from what the run recorded. A run
// published long ago is shown the way this version shows one, a packed run
// included: its results.json is outside the archive.
//
// Reading only. What a run recorded is what it recorded; only e2e-report run and
// e2e-report refresh write that, and only from the results of the run itself.
func (s *Server) serveRunReport(w http.ResponseWriter, r *http.Request, run string) {
	report, err := readRunFor(filepath.Join(s.root, run), run)
	if err != nil {
		log.Printf("failed to read the results of %s: %v", run, err)
		http.Error(w, "cannot read the results of the run", http.StatusInternalServerError)
		return
	}

	page, err := renderPage("run", newRunPage(report))
	if err != nil {
		log.Printf("failed to render the report of %s: %v", run, err)
		http.Error(w, "cannot render the report of the run", http.StatusInternalServerError)
		return
	}

	// Rendered for this request, so there is no stored copy to revalidate
	// against and no modification time to offer.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, indexHTML, time.Time{}, bytes.NewReader(page))
}

// anyRunning tells whether any of the runs is still going, which is what makes
// an index worth refreshing.
func anyRunning(runs []*Run) bool {
	for _, run := range runs {
		if run.Verdict == "RUNNING" {
			return true
		}
	}

	return false
}

// serveIndex serves an index of the runs under the root as they are right
// now. Reading only: reporting on a run is what e2e-report run and e2e-report
// refresh are for.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	page, running, err := s.renderIndex()
	if err != nil {
		log.Printf("indexing the runs in %s: %v", s.root, err)
		http.Error(w, "cannot index the runs", http.StatusInternalServerError)
		return
	}

	// A run in progress moves from test to test, so the page is worth coming
	// back to often. With nothing going it still comes back, only slowly: a run
	// which starts in the meantime is news no browser learns any other way, and
	// an index which goes still the moment the last run ends is an index nobody
	// sees the next one start on.
	interval := followInterval
	if !running {
		interval = idleInterval
	}
	if every := refreshFor(r, interval); every > 0 {
		w.Header().Set("Refresh", strconv.Itoa(int(every.Seconds())))
	}

	// Built for this request, so there is no stored copy to revalidate against
	// and no modification time to offer.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, indexHTML, time.Time{}, bytes.NewReader(page))
}

// serveCmd serves the published results over HTTP.
func serveCmd(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	address := flags.String("address", ":8080", "address to listen on")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("serve takes a single result root directory")
	}

	server, err := NewServer(flags.Arg(0))
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return err
	}

	httpd := &http.Server{
		Handler: server,
		// The results are ours and the clients are browsers, but a listening
		// socket is a listening socket: never wait forever for a request.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpd.Shutdown(shutdown)
	}()

	fmt.Printf("serving %s on %s\n", server.root, listener.Addr())

	if err := httpd.Serve(listener); err != nil && ctx.Err() == nil {
		return err
	}

	return nil
}
