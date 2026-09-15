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

package coverage

import (
	"bufio"
	"encoding/binary"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	xhttp "github.com/containers/nri-plugins/pkg/http"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestUninstrumentedBinaryFailsCleanly checks that we fail requests with an
// error, instead of serving unusable data, when there is no coverage data to
// serve. Note that this is always the case for a test binary: runtime/coverage
// only works in binaries built with go build -cover, so not even go test
// -cover makes the data available here. TestInstrumentedBinary covers the
// other case, using a helper binary.
func TestUninstrumentedBinaryFailsCleanly(t *testing.T) {
	url := serve(t)

	for _, path := range []string{IDPath, MetaPath, CountersPath, ClearPath} {
		status, body := get(t, url+path)
		require.Equal(t, http.StatusInternalServerError, status, "status of %s", path)
		require.NotEmpty(t, body, "error message of %s", path)
	}
}

func TestBinaryIDFromMetaHeader(t *testing.T) {
	hash := []byte{
		0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10,
	}

	header := func(magic []byte, version uint32, hash []byte) []byte {
		data := append([]byte{}, magic...)
		data = binary.LittleEndian.AppendUint32(data, version)
		data = append(data, make([]byte, metaHashOffset-len(data))...)
		return append(data, hash...)
	}

	for _, tc := range []struct {
		name string
		data []byte
		id   string
	}{
		{
			name: "valid header",
			data: header(metaMagic[:], metaVersion, hash),
			id:   "0123456789abcdeffedcba9876543210",
		},
		{
			name: "truncated header",
			data: header(metaMagic[:], metaVersion, hash)[:metaHeaderLen-1],
		},
		{
			name: "wrong magic",
			data: header([]byte{0x00, 0x00, 0x00, 0x00}, metaVersion, hash),
		},
		{
			name: "unknown version",
			data: header(metaMagic[:], metaVersion+1, hash),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &metaHeader{}

			// feed the data in small chunks, the way a real dump arrives
			for chunk := tc.data; len(chunk) > 0; {
				n := min(7, len(chunk))
				cnt, err := m.Write(chunk[:n])
				require.NoError(t, err, "write to header collector")
				require.Equal(t, n, cnt, "bytes accepted by header collector")
				chunk = chunk[n:]
			}

			id, err := m.binaryID()
			if tc.id == "" {
				require.Error(t, err, "binary ID of %s", tc.name)
				return
			}
			require.NoError(t, err, "binary ID of %s", tc.name)
			require.Equal(t, tc.id, id, "binary ID")
		})
	}
}

// TestInstrumentedBinary checks that an instrumented binary serves coverage
// data which the go tools can digest, and that it serves the same binary ID
// which the runtime uses for the data it writes to $GOCOVERDIR. Our callers
// depend on both: the ID is what ties the counter data we serve to the
// meta-data it belongs to, and using the same one keeps the data collected
// over HTTP mergeable with the data dumped at exit.
func TestInstrumentedBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test which builds a helper binary in short mode")
	}

	gocmd, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain available for building the helper binary")
	}

	dir := t.TempDir()
	helper := filepath.Join(dir, "coverage-server")
	covdir := filepath.Join(dir, "gocoverdir")
	require.NoError(t, os.Mkdir(covdir, 0o755), "create GOCOVERDIR")

	build := exec.Command(gocmd, "build", "-cover", "-covermode=atomic",
		"-o", helper, "./internal/coverage-server")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build helper binary: %s", out)

	url, stop := start(t, helper, covdir)

	status, body := get(t, url+IDPath)
	require.Equal(t, http.StatusOK, status, "status of %s", IDPath)
	id := string(body)
	require.Regexp(t, hexID, id, "served binary ID")

	status, meta := get(t, url+MetaPath)
	require.Equal(t, http.StatusOK, status, "status of %s", MetaPath)
	require.Greater(t, len(meta), metaHeaderLen, "length of served meta-data")
	require.Equal(t, metaMagic[:], meta[:metaMagicLen], "magic of served meta-data")

	status, counters := get(t, url+CountersPath)
	require.Equal(t, http.StatusOK, status, "status of %s", CountersPath)
	require.NotEmpty(t, counters, "served counters")

	status, body = get(t, url+ClearPath)
	require.Equal(t, http.StatusOK, status, "status of %s", ClearPath)
	require.Equal(t, "ok", string(body), "response of %s", ClearPath)

	status, cleared := get(t, url+CountersPath)
	require.Equal(t, http.StatusOK, status, "status of %s after clearing", CountersPath)
	require.NotEmpty(t, cleared, "served counters after clearing")

	// Check that the data we served is usable as is, once saved under the
	// names go tool covdata expects.
	saved := filepath.Join(dir, "saved")
	require.NoError(t, os.Mkdir(saved, 0o755), "create directory for served data")
	require.NoError(t, os.WriteFile(filepath.Join(saved, "covmeta."+id), meta, 0o644),
		"save served meta-data")
	require.NoError(t, os.WriteFile(filepath.Join(saved, "covcounters."+id+".1.1"), counters, 0o644),
		"save served counters")

	profile := filepath.Join(dir, "profile.txt")
	out, err = exec.Command(gocmd, "tool", "covdata", "textfmt",
		"-i="+saved, "-o="+profile).CombinedOutput()
	require.NoError(t, err, "convert served data to a text profile: %s", out)

	text, err := os.ReadFile(profile)
	require.NoError(t, err, "read converted profile")
	require.Contains(t, string(text), "instrumentation/coverage/coverage.go",
		"packages in the converted profile")

	// Terminate the helper gracefully to get its data written to GOCOVERDIR,
	// then check that it uses the same ID we served.
	stop(t)

	metaFiles, err := filepath.Glob(filepath.Join(covdir, "covmeta.*"))
	require.NoError(t, err, "look for meta-data in GOCOVERDIR")
	require.Len(t, metaFiles, 1, "meta-data files in GOCOVERDIR")
	require.Equal(t, "covmeta."+id, filepath.Base(metaFiles[0]),
		"name of the meta-data file written to GOCOVERDIR")
}

// serve starts a server with our endpoints registered and returns its URL.
func serve(t *testing.T) string {
	t.Helper()

	srv := xhttp.NewServer()
	Setup(srv.GetMux())
	require.NoError(t, srv.Start("127.0.0.1:0"), "start test server")
	t.Cleanup(srv.Stop)

	return "http://" + srv.GetAddress()
}

// start runs the given helper binary with GOCOVERDIR set to the given
// directory. It returns the URL the helper serves and a function which
// terminates it gracefully.
func start(t *testing.T, helper, covdir string) (string, func(*testing.T)) {
	t.Helper()

	cmd := exec.Command(helper)
	cmd.Env = append(os.Environ(), "GOCOVERDIR="+covdir)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "pipe stdout of helper binary")
	require.NoError(t, cmd.Start(), "start helper binary")

	stopped := false
	stop := func(t *testing.T) {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		require.NoError(t, cmd.Process.Signal(syscall.SIGTERM), "terminate helper binary")
		require.NoError(t, cmd.Wait(), "wait for helper binary to exit")
	}
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	addr := make(chan string, 1)
	go func() {
		if scanner := bufio.NewScanner(stdout); scanner.Scan() {
			addr <- strings.TrimSpace(scanner.Text())
		}
		close(addr)
	}()

	select {
	case a, ok := <-addr:
		require.True(t, ok, "read address from helper binary")
		return "http://" + a, stop
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timeout waiting for the helper binary to serve")
	}

	return "", stop
}

// get performs a GET request and returns the status code and the body.
func get(t *testing.T, url string) (int, []byte) {
	t.Helper()

	rpl, err := http.Get(url) //nolint:noctx
	require.NoError(t, err, "GET %s", url)
	defer func() {
		if err := rpl.Body.Close(); err != nil {
			t.Logf("failed to close HTTP reply body: %v", err)
		}
	}()

	body, err := io.ReadAll(rpl.Body)
	require.NoError(t, err, "read body of %s", url)

	return rpl.StatusCode, body
}
