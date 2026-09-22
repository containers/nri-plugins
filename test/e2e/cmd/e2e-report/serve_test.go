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
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newRoot publishes a run under a result root, packed or as it is.
func newRoot(t *testing.T, packed bool) (string, string) {
	t.Helper()

	root, name := t.TempDir(), "test-2026-09-16-0910"
	dir := filepath.Join(root, name)
	if err := os.Rename(newRun(t), dir); err != nil {
		t.Fatal(err)
	}
	if packed {
		if err := packRun(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(name, filepath.Join(root, "latest")); err != nil {
		t.Fatal(err)
	}

	return root, name
}

// rendered is in every page the reports are rendered from a template, and in
// none of the files a run leaves behind, so it tells a rendered report from a
// stale index.html served as it is.
const rendered = "<!DOCTYPE html>"

func get(t *testing.T, root, path string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, root, http.MethodGet, path)
}

func request(t *testing.T, root, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, root, method, path)
}

// newServer is one server to ask more than once, for what it remembers between
// requests.
func newServer(t *testing.T, root string) *Server {
	t.Helper()

	server, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}

	return server
}

func ask(t *testing.T, server *Server, path string) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s: %d, expected 200", path, recorder.Code)
	}

	return recorder.Body.String()
}

// rewriteResults puts data in the results.json of a run, leaving its size and
// its modification time as they were: a change nothing can notice by looking,
// which is how a test tells a cached answer from a fresh one.
func rewriteResults(t *testing.T, dir, data string) {
	t.Helper()

	path := filepath.Join(dir, resultsJSON)
	was, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int(was.Size()) != len(data) {
		t.Fatalf("results.json is %d bytes, the replacement %d", was.Size(), len(data))
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, was.ModTime(), was.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func serve(t *testing.T, root string, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	server, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))

	return recorder
}

// TestServePacked checks that a packed run serves as if it had been extracted.
func TestServePacked(t *testing.T) {
	root, name := newRoot(t, true)

	for file, data := range files {
		// The report of a run is rendered when it is asked for, so what a run
		// left under that name is not what answers. TestServeRendersReport.
		if file == indexHTML {
			continue
		}
		got := get(t, root, "/"+name+"/"+file)
		if got.Code != http.StatusOK {
			t.Errorf("GET %s: %d, expected 200", file, got.Code)
			continue
		}
		if got.Body.String() != data {
			t.Errorf("GET %s: %q, expected %q", file, got.Body, data)
		}
	}

	// Through the link at the latest results as well.
	if got := get(t, root, "/latest/"+runnerLog); got.Body.String() != files[runnerLog] {
		t.Errorf("GET /latest/%s: %q", runnerLog, got.Body)
	}
}

// TestServeUnpacked checks that a run which was never packed still serves.
func TestServeUnpacked(t *testing.T) {
	root, name := newRoot(t, false)

	for file, data := range files {
		if file == indexHTML {
			continue
		}
		got := get(t, root, "/"+name+"/"+file)
		if got.Code != http.StatusOK || got.Body.String() != data {
			t.Errorf("GET %s: %d %q, expected 200 %q", file, got.Code, got.Body, data)
		}
	}
}

func TestServeContentType(t *testing.T) {
	root, name := newRoot(t, true)
	test := "/" + name + "/vm/" + suiteDir + "/balloons/test01/"

	for _, tc := range []struct{ path, kind string }{
		// The command transcripts of a test have no extension, and are text
		// worth showing in a browser instead of downloading.
		{test + "commands/0001-vm", "text/plain"},
		{test + testLog, "text/plain"},
		{"/" + name + "/" + indexHTML, "text/html"},
		{"/" + name + "/" + resultsJSON, "application/json"},
	} {
		got := get(t, root, tc.path).Header().Get("Content-Type")
		if !strings.HasPrefix(got, tc.kind) {
			t.Errorf("GET %s: content type %q, expected %q", tc.path, got, tc.kind)
		}
	}
}

func TestServeListing(t *testing.T) {
	root, name := newRoot(t, true)

	// The report of the run is rendered for the run itself.
	if got := get(t, root, "/"+name+"/"); !strings.Contains(got.Body.String(), rendered) {
		t.Errorf("GET /%s/: %q, expected a rendered report", name, got.Body)
	}

	got := get(t, root, "/"+name+"/vm/"+suiteDir+"/balloons/test01/")
	if got.Code != http.StatusOK {
		t.Fatalf("listing a directory of the archive: %d", got.Code)
	}
	for _, expected := range []string{"commands/", testLog, summaryTxt} {
		if !strings.Contains(got.Body.String(), ">"+expected+"<") {
			t.Errorf("the listing does not name %s", expected)
		}
	}

	// A directory of the archive without the trailing slash, which relative
	// links in a listing need.
	got = get(t, root, "/"+name+"/vm")
	if got.Code != http.StatusMovedPermanently {
		t.Errorf("GET a directory without a trailing slash: %d, expected 301", got.Code)
	}
}

func TestServeNotFound(t *testing.T) {
	root, name := newRoot(t, true)

	for _, path := range []string{
		"/" + name + "/no/such/file",
		"/" + name + "/vm/" + suiteDir + "/balloons/test01/nosuchfile",
		"/no-such-run/index.html",
		"/" + name + "/vm/" + suiteDir + "/balloons/test01/commands/0001-vm/deeper",
	} {
		if got := get(t, root, path); got.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, expected 404", path, got.Code)
		}
	}
}

// TestServeStaysUnderRoot checks that nothing outside the published results is
// served, however a request asks for it.
func TestServeStaysUnderRoot(t *testing.T) {
	root, name := newRoot(t, true)

	secret := filepath.Join(filepath.Dir(root), "secret")
	if err := os.WriteFile(secret, []byte("not yours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A link out of the results, which is all it takes to serve anything.
	if err := os.Symlink(secret, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, name, "outside")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/../secret",
		"/../../etc/passwd",
		"/" + name + "/../../secret",
		"/%2e%2e/secret",
		"/outside",
		"/" + name + "/outside",
	} {
		got := get(t, root, path)
		if got.Code == http.StatusOK && strings.Contains(got.Body.String(), "not yours") {
			t.Errorf("GET %s served what is outside the results", path)
		}
	}
}

func TestServeMethods(t *testing.T) {
	root, name := newRoot(t, true)

	got := request(t, root, http.MethodHead, "/"+name+"/"+runnerLog)
	if got.Code != http.StatusOK {
		t.Errorf("HEAD: %d, expected 200", got.Code)
	}
	if got.Body.Len() != 0 {
		t.Errorf("HEAD has a body: %q", got.Body)
	}
	if got.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD tells no content length")
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		got := request(t, root, method, "/"+name+"/"+runnerLog)
		if got.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, expected 405", method, got.Code)
		}
	}
}

// TestServeRevalidates checks that a browser cannot go on showing a file of a run
// which has changed since, the log of a run still going above all.
func TestServeRevalidates(t *testing.T) {
	root, name := newRoot(t, false)
	page := "/" + name + "/" + runnerLog

	got := get(t, root, page)
	if cache := got.Header().Get("Cache-Control"); cache != "no-cache" {
		t.Errorf("Cache-Control of a file: %q, expected no-cache", cache)
	}
	stamp := got.Header().Get("Last-Modified")
	if stamp == "" {
		t.Fatalf("a file is served without Last-Modified")
	}

	// Asking about the copy one has is answered, and answered again once the
	// file has been written anew.
	server, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	ask := func() int {
		request := httptest.NewRequest(http.MethodGet, page, nil)
		request.Header.Set("If-Modified-Since", stamp)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder.Code
	}

	if code := ask(); code != http.StatusNotModified {
		t.Errorf("asking about an unchanged file: %d, expected 304", code)
	}

	local := filepath.Join(root, name, runnerLog)
	if err := os.WriteFile(local, []byte("more of the log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(local, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if code := ask(); code != http.StatusOK {
		t.Errorf("asking about a file written anew: %d, expected 200", code)
	}
}

// TestServeReportIsNeverRevalidated checks that a report is rendered for every
// request and says so: no modification time to offer, so no browser can be told
// the copy it has is still good.
func TestServeReportIsNeverRevalidated(t *testing.T) {
	root, name := newRoot(t, false)

	got := get(t, root, "/"+name+"/"+indexHTML)
	if cache := got.Header().Get("Cache-Control"); cache != "no-cache" {
		t.Errorf("Cache-Control of a report: %q, expected no-cache", cache)
	}
	if stamp := got.Header().Get("Last-Modified"); stamp != "" {
		t.Errorf("a rendered report offers Last-Modified %q", stamp)
	}
}

func TestServeNoSuchRoot(t *testing.T) {
	if _, err := NewServer(filepath.Join(t.TempDir(), "nowhere")); err == nil {
		t.Errorf("serving a directory which is not there did not fail")
	}
}

// TestServeIndexCaches checks that the runs are not read again for a root
// which has not changed. The proof is a change no amount of looking can see:
// results.json rewritten to the same size, with its modification time put back.
func TestServeIndexCaches(t *testing.T) {
	root, name := newRoot(t, false)
	server := newServer(t, root)

	first := ask(t, server, "/")
	if strings.Contains(first, ">FAIL<") {
		t.Fatalf("the run reads FAIL before anything changed it")
	}

	rewriteResults(t, filepath.Join(root, name), `{"verdict":"FAIL" }`)

	if again := ask(t, server, "/"); again != first {
		t.Errorf("the runs were read again for a root which had not changed")
	}
}

// TestServeIndexNoticesAChangedRun checks that a run reported on again is
// read again, which is what the cache must not get in the way of.
func TestServeIndexNoticesAChangedRun(t *testing.T) {
	root, name := newRoot(t, false)
	server := newServer(t, root)

	first := ask(t, server, "/")

	results := filepath.Join(root, name, resultsJSON)
	if err := os.WriteFile(results, []byte(`{"verdict":"FAIL"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(results, later, later); err != nil {
		t.Fatal(err)
	}

	again := ask(t, server, "/")
	if again == first {
		t.Errorf("a run reported on again was not read again")
	}
	if !strings.Contains(again, ">FAIL<") {
		t.Errorf("the verdict of the run did not change")
	}
}

// TestServeIndexNoticesRunsComingAndGoing checks that a run published or
// pruned since the last request is listed, or stops being.
func TestServeIndexNoticesRunsComingAndGoing(t *testing.T) {
	root, name := newRoot(t, false)
	server := newServer(t, root)

	if body := ask(t, server, "/"); strings.Contains(body, "test-2026-09-18-0210") {
		t.Fatalf("a run which is not there yet is listed")
	}

	newOngoingRun(t, root, "test-2026-09-18-0210")
	if body := ask(t, server, "/"); !strings.Contains(body, "test-2026-09-18-0210") {
		t.Errorf("a run published since the last request is not listed")
	}

	if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
	if body := ask(t, server, "/"); strings.Contains(body, name) {
		t.Errorf("a run pruned since the last request is still listed")
	}
}

// TestServeIndexNoticesARunGoingOn checks that a run still collecting is
// read again as it goes, where nothing but its log has changed.
func TestServeIndexNoticesARunGoingOn(t *testing.T) {
	root, _ := newRoot(t, false)
	ongoing := newOngoingRun(t, root, "test-2026-09-18-0210")
	server := newServer(t, root)

	first := ask(t, server, "/")

	// A test which has finished since, and the log the runner keeps appending.
	test := filepath.Join(ongoing, "vm", suiteDir, "balloons", "test01")
	if err := os.MkdirAll(test, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(test, summaryTxt),
		[]byte("Test verdict: PASS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(ongoing, runnerLog)
	if err := os.WriteFile(log, []byte("still going\nand going\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(log, later, later); err != nil {
		t.Fatal(err)
	}

	if again := ask(t, server, "/"); again == first {
		t.Errorf("a run which collected another test since was not read again")
	}
}

// TestServeIndexConcurrently checks that the index one server remembers
// survives being asked for from several requests at once, which is how it is
// asked for. Worth running under -race.
func TestServeIndexConcurrently(t *testing.T) {
	root, name := newRoot(t, false)
	newOngoingRun(t, root, "test-2026-09-18-0210")
	server := newServer(t, root)

	// One writer moving a run about under the readers, so that they race a
	// rebuild and not only each other.
	done := make(chan struct{})
	go func() {
		defer close(done)
		results := filepath.Join(root, name, resultsJSON)
		for i := range 20 {
			stamp := time.Now().Add(time.Duration(i) * time.Minute)
			_ = os.Chtimes(results, stamp, stamp)
		}
	}()

	var waiting sync.WaitGroup
	for range 8 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			for range 20 {
				recorder := httptest.NewRecorder()
				server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
				if recorder.Code != http.StatusOK {
					t.Errorf("GET /: %d, expected 200", recorder.Code)
					return
				}
				if !strings.Contains(recorder.Body.String(), name) {
					t.Errorf("the index does not name %s", name)
					return
				}
			}
		}()
	}

	waiting.Wait()
	<-done
}

// TestServeIndexSkipsLatest checks that the link at the newest run is not
// indexed as a run of its own, which would list the same run twice.
func TestServeIndexSkipsLatest(t *testing.T) {
	root, name := newRoot(t, false)

	body := get(t, root, "/").Body.String()

	if strings.Contains(body, `href="latest/`) {
		t.Errorf("the link at the latest run is indexed as a run of its own")
	}
	if listed := strings.Count(body, `href="`+name+`/`); listed != 1 {
		t.Errorf("the run is listed %d times, expected once", listed)
	}
}

// snapshot records what is under root, so that a caller can tell whether
// anything about it changed.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()

	seen := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		seen[rel] = fmt.Sprintf("%d bytes, mode %s, %d", info.Size(), info.Mode(),
			info.ModTime().UnixNano())

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return seen
}

// TestServeIndexWritesNothing checks that building the index reports on
// nothing. Reporting on a run which recorded nothing is the tempting way to fill
// in a row; a server must not, and the one behind the systemd unit could not, as
// it is given the results read-only.
func TestServeIndexWritesNothing(t *testing.T) {
	root, _ := newRoot(t, false)
	newOngoingRun(t, root, "test-2026-09-17-2251")

	before := snapshot(t, root)
	if got := get(t, root, "/"); got.Code != http.StatusOK {
		t.Fatalf("GET /: %d, expected 200", got.Code)
	}
	after := snapshot(t, root)

	if !maps.Equal(before, after) {
		for name, was := range before {
			if now, there := after[name]; !there {
				t.Errorf("%s went away, was %s", name, was)
			} else if now != was {
				t.Errorf("%s changed, was %s, is %s", name, was, now)
			}
		}
		for name := range after {
			if _, there := before[name]; !there {
				t.Errorf("%s was written", name)
			}
		}
	}
}

// TestServeIndexPackedLikeUnpacked checks that a packed run is indexed
// exactly as an unpacked one, so that packing a run cannot be told from the
// index, let alone break a link in it.
func TestServeIndexPackedLikeUnpacked(t *testing.T) {
	looseRoot, _ := newRoot(t, false)
	packedRoot, _ := newRoot(t, true)

	loose := get(t, looseRoot, "/").Body.String()
	packed := get(t, packedRoot, "/").Body.String()

	if loose != packed {
		t.Errorf("the index of a packed run differs from that of an unpacked one")
		for i := range min(len(loose), len(packed)) {
			if loose[i] != packed[i] {
				t.Errorf("first difference at %d:\n unpacked: %q\n   packed: %q",
					i, cut(loose, i), cut(packed, i))
				break
			}
		}
	}
}

// cut is the neighbourhood of i in s, for telling what differs where.
func cut(s string, i int) string {
	return s[max(0, i-40):min(len(s), i+40)]
}

// TestServeIndexAtIndexHTML checks that the built index answers for the name
// of the file it stands in for, and not just for the root itself.
func TestServeIndexAtIndexHTML(t *testing.T) {
	root, name := newRoot(t, false)
	if err := os.WriteFile(filepath.Join(root, indexHTML),
		[]byte("<html>stale</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/" + indexHTML} {
		got := get(t, root, path)
		if got.Code != http.StatusOK {
			t.Errorf("GET %s: %d, expected 200", path, got.Code)
			continue
		}
		if strings.Contains(got.Body.String(), "stale") {
			t.Errorf("GET %s served the index on disk", path)
		}
		if !strings.Contains(got.Body.String(), name) {
			t.Errorf("GET %s does not name the run %s", path, name)
		}
	}
}

// newOngoingRun adds a run which has no report yet, the way one looks while it
// is still going: the log of the runner and nothing to link to.
func newOngoingRun(t *testing.T, root, name string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runnerLog), []byte("still going\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, statusTxt), []byte("RUNNING\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

// TestServeIndexOngoingRun checks that a run which has recorded nothing of
// itself yet is listed, linked to its report like any other, and that the report
// is rendered from what it has collected so far.
func TestServeIndexOngoingRun(t *testing.T) {
	root, _ := newRoot(t, false)
	ongoing := "test-2026-09-17-2251"
	newOngoingRun(t, root, ongoing)

	body := get(t, root, "/").Body.String()
	if !strings.Contains(body, `href="`+ongoing+`/`+indexHTML+`"`) {
		t.Errorf("an ongoing run is not linked to its report: %q", body)
	}

	// And the link opens something, although the run recorded nothing to open.
	got := get(t, root, "/"+ongoing+"/"+indexHTML)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), rendered) {
		t.Errorf("the report of an ongoing run: %d %q", got.Code, got.Body)
	}
}

// TestServeIndex checks that the index of runs is built from the runs found
// under the root, and not read from the index.html lying next to them.
func TestServeIndex(t *testing.T) {
	root, name := newRoot(t, false)
	stale := filepath.Join(root, indexHTML)
	if err := os.WriteFile(stale, []byte("<html>stale</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := get(t, root, "/")
	if got.Code != http.StatusOK {
		t.Fatalf("GET / with a live index: %d, expected 200", got.Code)
	}
	if strings.Contains(got.Body.String(), "stale") {
		t.Errorf("the index on disk was served instead of a built one")
	}
	if !strings.Contains(got.Body.String(), name) {
		t.Errorf("the built index does not name the run %s", name)
	}
}

// getView asks for a log the way a report links it.
func getView(t *testing.T, root, path string) *httptest.ResponseRecorder {
	t.Helper()
	return get(t, root, path+"?"+viewQuery)
}

// TestServeFollowsARunningLog checks that the log of a run which is still going
// is served as a page which comes back for what is written to it after.
func TestServeFollowsARunningLog(t *testing.T) {
	root, _ := newRoot(t, false)
	ongoing := "test-2026-09-17-2251"
	dir := newOngoingRun(t, root, ongoing)
	text := readFile(filepath.Join(dir, runnerLog))

	got := getView(t, root, "/"+ongoing+"/"+runnerLog)
	body := got.Body.String()

	if got.Code != http.StatusOK {
		t.Fatalf("following a running log: %d, expected 200", got.Code)
	}
	if kind := got.Header().Get("Content-Type"); !strings.HasPrefix(kind, "text/html") {
		t.Errorf("a followed log is served as %q, expected text/html", kind)
	}
	if !strings.Contains(body, text) {
		t.Errorf("the page does not carry the log it follows: %q", body)
	}
	if want := fmt.Sprintf(`data-offset="%d"`, len(text)); !strings.Contains(body, want) {
		t.Errorf("the page does not ask for the rest from %q", want)
	}
	every := fmt.Sprintf(`data-every="%d"`, followInterval.Milliseconds())
	if !strings.Contains(body, every) {
		t.Errorf("the page does not come back every %v", followInterval)
	}
	if !strings.Contains(body, "Range") {
		t.Errorf("the page does not ask for a range of the log: %q", body)
	}
}

// TestServeFollowsNothingWhenFinished checks that the log of a run which has
// stopped is served as a page with nothing to wait for.
func TestServeFollowsNothingWhenFinished(t *testing.T) {
	root, name := newRoot(t, false)

	body := getView(t, root, "/"+name+"/"+runnerLog).Body.String()

	if !strings.Contains(body, `data-every="0"`) {
		t.Errorf("a finished run's log is followed all the same: %q", body)
	}
}

// TestServeFollowsNothingWhenAbandoned checks that a run which says it is
// running but has not written to its log for an hour is not followed either:
// a run which was killed says RUNNING for good.
func TestServeFollowsNothingWhenAbandoned(t *testing.T) {
	root, _ := newRoot(t, false)
	abandoned := "test-2026-09-17-2251"
	dir := newOngoingRun(t, root, abandoned)

	old := time.Now().Add(-2 * staleAfter)
	if err := os.Chtimes(filepath.Join(dir, runnerLog), old, old); err != nil {
		t.Fatal(err)
	}

	body := getView(t, root, "/"+abandoned+"/"+runnerLog).Body.String()

	if !strings.Contains(body, `data-every="0"`) {
		t.Errorf("an abandoned run's log is followed: %q", body)
	}
}

// TestServeFollowLeavesTheLogAlone checks that the log itself is untouched by
// any of this: it is what the page reads, and what everything else reads.
func TestServeFollowLeavesTheLogAlone(t *testing.T) {
	root, _ := newRoot(t, false)
	ongoing := "test-2026-09-17-2251"
	dir := newOngoingRun(t, root, ongoing)
	text := readFile(filepath.Join(dir, runnerLog))

	got := get(t, root, "/"+ongoing+"/"+runnerLog)

	if kind := got.Header().Get("Content-Type"); !strings.HasPrefix(kind, "text/plain") {
		t.Errorf("the log of a running run is served as %q, expected text/plain", kind)
	}
	if got.Body.String() != text {
		t.Errorf("the log served is not the log: %q", got.Body.String())
	}
}

// TestServeViewsOnlyTextFiles checks that asking for a view of something which
// is not a log is ignored: a page of a json file would be a page of whatever it
// happens to look like. index.html is not among them -- it names the report of
// the run, which is rendered whatever is asked of it.
func TestServeViewsOnlyTextFiles(t *testing.T) {
	root, name := newRoot(t, false)

	for _, file := range []string{resultsJSON, "vm/" + suiteDir +
		"/balloons/test01/commands/0001-vm"} {
		got := getView(t, root, "/"+name+"/"+file)
		if strings.Contains(got.Body.String(), "data-every=") {
			t.Errorf("%s is served as a page which reads it", file)
		}
		if got.Body.String() != readFile(filepath.Join(root, name, file)) {
			t.Errorf("%s is not served as it is when a view of it is asked for", file)
		}
	}
}

// TestServeFollowEscapesTheLog checks that a log which reads like markup cannot
// break out of the page carrying it.
func TestServeFollowEscapesTheLog(t *testing.T) {
	root, _ := newRoot(t, false)
	ongoing := "test-2026-09-17-2251"
	dir := newOngoingRun(t, root, ongoing)

	nasty := "</pre><script>alert(1)</script><pre>\n"
	if err := os.WriteFile(filepath.Join(dir, runnerLog), []byte(nasty), 0o644); err != nil {
		t.Fatal(err)
	}

	body := getView(t, root, "/"+ongoing+"/"+runnerLog).Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("a log broke out of the page which carries it: %q", body)
	}
	if !strings.Contains(body, "alert(1)") {
		t.Errorf("the log is not in the page at all: %q", body)
	}
}

// TestServeFollowRange checks what the page relies on: asking for the bytes
// past the ones it has answers those bytes, and nothing until there are any.
func TestServeFollowRange(t *testing.T) {
	root, _ := newRoot(t, false)
	ongoing := "test-2026-09-17-2251"
	dir := newOngoingRun(t, root, ongoing)
	log := filepath.Join(dir, runnerLog)
	served := len(readFile(log))

	ranged := func(from int) *httptest.ResponseRecorder {
		t.Helper()
		server, err := NewServer(root)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/"+ongoing+"/"+runnerLog, nil)
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)

		return recorder
	}

	if got := ranged(served); got.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("a log which has not grown answers %d, expected 416", got.Code)
	}

	more := "and then some more\n"
	file, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(more); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	got := ranged(served)
	if got.Code != http.StatusPartialContent {
		t.Fatalf("a log which has grown answers %d, expected 206", got.Code)
	}
	if got.Body.String() != more {
		t.Errorf("the rest of the log is %q, expected %q", got.Body.String(), more)
	}
}

// storeRun writes a run's results the way a published run keeps them. No page:
// nothing writes one any more, and the runs which kept one from an older reporter
// are what staleReport is for.
func storeRun(t *testing.T, root, name string, run *Run) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]string{
		resultsJSON: string(data),
		statusTxt:   run.Verdict + " 1/1 tests passed\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// staleReport leaves a run the index.html an older reporter rendered for it, to
// check that it is never what answers.
func staleReport(t *testing.T, dir, html string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, indexHTML), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
}

// aRun is a run which recorded one test case with its logs, the links stored as
// plain paths the way a run records them.
func aRun(name string) *Run {
	return &Run{
		Name:    name,
		Verdict: "PASS",
		Counts:  map[string]int{"total": 1, "PASS": 1},
		Tests: []*Test{{
			Name:     "test01-basic-placement",
			Policy:   "balloons",
			VM:       "n4c16-fedora-43-containerd",
			Verdict:  "PASS",
			Topology: ref("n4c16"),
			Links: map[string]string{
				"plugin log": "n4c16-fedora-43-containerd/" + suiteDir +
					"/balloons/test01/nri-resource-policy.output.txt",
				"verdict": "n4c16-fedora-43-containerd/" + suiteDir +
					"/balloons/test01/" + summaryTxt,
			},
		}},
	}
}

// TestServeReportRendersFromTheResults checks that the report of a run is
// rendered from what the run recorded, so that a report which has improved since
// the run was published improves with it.
func TestServeReportRendersFromTheResults(t *testing.T) {
	root, _ := newRoot(t, false)
	name := "test-2026-09-20-0200"
	staleReport(t, storeRun(t, root, name, aRun(name)),
		"<html>the report as it was rendered then</html>")

	for _, path := range []string{"/" + name + "/", "/" + name + "/" + indexHTML} {
		body := get(t, root, path).Body.String()
		if strings.Contains(body, "as it was rendered then") {
			t.Errorf("GET %s served the stored report: %q", path, cut(body, 200))
		}
		if !strings.Contains(body, "test01-basic-placement") {
			t.Errorf("GET %s did not render what the run recorded: %q", path, cut(body, 200))
		}
	}
}

// TestServeReportLinksLogsToRead checks the point of rendering a report
// again: a run which recorded its logs as plain paths, before this tool knew to
// read one as a page, links them to be read all the same.
func TestServeReportLinksLogsToRead(t *testing.T) {
	root, _ := newRoot(t, false)
	name := "test-2026-09-20-0200"
	storeRun(t, root, name, aRun(name))

	body := get(t, root, "/"+name+"/").Body.String()

	if !strings.Contains(body, "nri-resource-policy.output.txt?"+viewQuery) {
		t.Errorf("the rendered report does not link the plugin log to read: %q", cut(body, 400))
	}
	// Only the logs: the verdict of a test is not a log to colour.
	if strings.Contains(body, summaryTxt+"?"+viewQuery) {
		t.Errorf("the rendered report reads the verdict as a log: %q", cut(body, 400))
	}
}

// TestServeScansWhatCannotBeRead checks that a run whose results say nothing we
// understand is scanned instead, the way a run which recorded nothing at all is.
// There is no stored report to fall back on any more.
func TestServeScansWhatCannotBeRead(t *testing.T) {
	root, _ := newRoot(t, false)
	name := "test-2026-09-20-0200"
	dir := storeRun(t, root, name, aRun(name))
	staleReport(t, dir, "<html>all there is</html>")

	// Results which are there but say nothing we understand.
	if err := os.WriteFile(filepath.Join(dir, resultsJSON), []byte("{{{"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := get(t, root, "/"+name+"/")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), rendered) {
		t.Errorf("a run whose results cannot be read has no report: %d %q",
			got.Code, cut(got.Body.String(), 200))
	}
	if strings.Contains(got.Body.String(), "all there is") {
		t.Errorf("the stored report was served after all")
	}
}

// TestServeReportOfAPackedRun checks that the report of a packed run is
// rendered again too. Its results.json is kept outside the archive, so there is
// no unpacking to do and nothing to write back.
func TestServeReportOfAPackedRun(t *testing.T) {
	root, name := newRoot(t, true)

	body := get(t, root, "/"+name+"/").Body.String()

	if body == files[indexHTML] {
		t.Errorf("the report of a packed run was served as stored, not rendered")
	}
	// The name the run recorded of itself, which is what a report is titled by.
	if !strings.Contains(body, "test-run") {
		t.Errorf("the report of a packed run was not rendered from its results: %q",
			cut(body, 200))
	}
}

// TestServeReportWritesNothing checks that rendering a report again leaves
// the run exactly as it was: the results are the run's, not ours.
func TestServeReportWritesNothing(t *testing.T) {
	root, _ := newRoot(t, false)
	name := "test-2026-09-20-0200"
	storeRun(t, root, name, aRun(name))

	before := snapshot(t, root)
	for range 3 {
		get(t, root, "/"+name+"/")
		get(t, root, "/"+name+"/"+indexHTML)
	}

	if after := snapshot(t, root); !maps.Equal(before, after) {
		t.Errorf("serving a rendered report changed the results under %s", root)
	}
}

// TestServeIndexSaysWhereARunIs checks that the index of the runs says which
// test a run which is still going has got to, and tells a browser to come back
// for it.
func TestServeIndexSaysWhereARunIs(t *testing.T) {
	root, _ := newRoot(t, false)
	ongoing := "test-2026-09-17-2251"
	dir := newOngoingRun(t, root, ongoing)
	at := "balloons/test22-isolcpus"
	log := prompt("balloons/test01-basic-placement", "kubectl get pods") +
		prompt(at, "mkdir -p /etc/default")
	if err := os.WriteFile(filepath.Join(dir, runnerLog), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	got := get(t, root, "/")

	if !strings.Contains(got.Body.String(), at) {
		t.Errorf("the index does not say where the run is: %q", cut(got.Body.String(), 600))
	}
	if every := got.Header().Get("Refresh"); every != "3" {
		t.Errorf("Refresh of an index with a run going: %q, expected 3", every)
	}
}

// TestServeIndexRefreshesSlowlyWhenIdle checks that an index of runs which have
// all ended still asks a browser to come back, only seldom. A run starting is
// the one thing no page can be told about, so an index which goes still when the
// last run ends is one nobody sees the next run start on.
func TestServeIndexRefreshesSlowlyWhenIdle(t *testing.T) {
	root, _ := newRoot(t, false)

	if every := get(t, root, "/").Header().Get("Refresh"); every != "30" {
		t.Errorf("Refresh of an idle index: %q, expected 30", every)
	}

	newOngoingRun(t, root, "test-2026-09-17-2251")

	if every := get(t, root, "/").Header().Get("Refresh"); every != "3" {
		t.Errorf("Refresh once a run is going: %q, expected 3", every)
	}
	if every := get(t, root, "/?"+refreshQuery+"=15s").Header().Get("Refresh"); every != "15" {
		t.Errorf("Refresh asked for as 15s: %q", every)
	}
	// Asked for as nothing, whether anything is going or not.
	if every := get(t, root, "/?"+refreshQuery+"=0").Header().Get("Refresh"); every != "" {
		t.Errorf("refresh=0 still asks for a refresh every %q", every)
	}
}

// TestServeIdleIndexTakesTheIntervalAsked checks that the interval can be asked
// for on an index with nothing going, the idle default being the one a reader
// watching for a run to start is most likely to want to shorten.
func TestServeIdleIndexTakesTheIntervalAsked(t *testing.T) {
	root, _ := newRoot(t, false)

	if every := get(t, root, "/?"+refreshQuery+"=5s").Header().Get("Refresh"); every != "5" {
		t.Errorf("Refresh of an idle index asked for as 5s: %q", every)
	}
	if every := get(t, root, "/?"+refreshQuery+"=0").Header().Get("Refresh"); every != "" {
		t.Errorf("an idle index asked for as 0 still refreshes every %q", every)
	}
}

// TestServeReportSaysWhereARunIs checks that the report of a run which is
// still going says which test it is in, where it used to say only that the
// runner log knows.
func TestServeReportSaysWhereARunIs(t *testing.T) {
	root, _ := newRoot(t, false)
	ongoing := "test-2026-09-17-2251"
	dir := newOngoingRun(t, root, ongoing)
	at := "topology-aware/test04-nrt"
	if err := os.WriteFile(filepath.Join(dir, runnerLog),
		[]byte(prompt(at, "kubectl describe node")), 0o644); err != nil {
		t.Fatal(err)
	}
	// A report to fall back to, so that the live one is what answers.
	if err := os.WriteFile(filepath.Join(dir, indexHTML), []byte("<html>x</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, resultsJSON),
		[]byte(`{"name":"`+ongoing+`","verdict":"RUNNING","counts":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	body := get(t, root, "/"+ongoing+"/").Body.String()

	if !strings.Contains(body, "It is at "+at+".") {
		t.Errorf("the report does not say which test the run is in: %q", cut(body, 600))
	}
}
