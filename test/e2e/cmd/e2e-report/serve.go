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
	mutex    sync.Mutex
	tarballs map[string]*Tarball
	order    []string
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

	if info, err := s.dir.Stat(name); err == nil {
		if !info.IsDir() {
			s.serveFile(w, r, name)
			return
		}
		if index := path.Join(name, indexHTML); s.exists(index) {
			s.serveFile(w, r, index)
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

// exists tells whether a name is there inside the root.
func (s *Server) exists(name string) bool {
	_, err := s.dir.Stat(name)
	return err == nil
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
