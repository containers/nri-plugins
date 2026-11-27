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
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	cxlapi "github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/qemu/fake"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/server"
)

func newTestServer(t *testing.T) (string, *fake.Qemu) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := server.ParseConfig([]byte("dir: " + filepath.Join(dir, "pool")))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateFile = "-"
	q := fake.NewQemu(map[string]int{"cxlhb0": 0}, map[string]string{"ds0": "cxlhb0", "ds1": "cxlhb0"})
	p := qemu.ParseCmdline([]string{"qemu-system-x86_64", "-uuid", "11111111-2222-3333-4444-555555555555",
		"-drive", "file=/e2e/vm1/.vagrant/machines/vm1/qemu/x/linked-box.img", "-qmp", "unix:/e2e/vm1/qmp.sock,server,nowait"})
	p.PID = 42
	srv, err := server.New(cfg, server.Options{
		Logger:       log.New(io.Discard, "", 0),
		Discover:     func() ([]*qemu.Process, error) { return []*qemu.Process{p}, nil },
		ProcessAlive: func(int, uint64) bool { return true },
		NewMonitor:   func(string, string) qemu.Monitor { return q },
		SocketExists: func(string) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { hs.Close(); srv.Stop() })
	return hs.URL, q
}

func runCLI(t *testing.T, url string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := run(context.Background(), append([]string{"--server", url}, args...), &out, &errb)
	return rc, out.String(), errb.String()
}

func TestCLI(t *testing.T) {
	url, q := newTestServer(t)
	if rc, _, _ := runCLI(t, url); rc != exitUsage {
		t.Fatalf("no command: rc %d", rc)
	}
	if rc, _, _ := runCLI(t, url, "bogus"); rc != exitUsage {
		t.Fatalf("bogus command: rc %d", rc)
	}
	if rc, _, _ := runCLI(t, url, "attach", "x"); rc != exitUsage {
		t.Fatalf("attach without host: rc %d", rc)
	}
	for _, args := range [][]string{{"guest", "--help"}, {"guest", "-h"}, {"guest", "region", "--help"}, {"guest", "wait", "--help"}, {"--help"}} {
		if rc, out, errs := runCLI(t, url, args...); rc != exitOK || !strings.Contains(out, "Usage:") || errs != "" {
			t.Fatalf("%v: rc %d stdout %q stderr %q", args, rc, out, errs)
		}
	}
	if rc, _, errs := runCLI(t, url, "--sever", "x", "hosts"); rc != exitUsage || !strings.Contains(errs, "flag provided but not defined") {
		t.Fatalf("bad global flag: rc %d stderr %q", rc, errs)
	}
	rc, out, _ := runCLI(t, url, "status")
	if first, _, _ := strings.Cut(out, "\n"); rc != 0 || !regexp.MustCompile(`^name: +fake-cxl-pool$`).MatchString(first) {
		t.Fatalf("status: %d\n%s", rc, out)
	}
	rc, out, _ = runCLI(t, url, "-o", "json", "status")
	var st api.Status
	if rc != 0 || json.Unmarshal([]byte(out), &st) != nil || st.Name != "fake-cxl-pool" || !strings.Contains(out, `"name": "fake-cxl-pool"`) {
		t.Fatalf("status JSON: %d\n%s", rc, out)
	}
	rc, out, _ = runCLI(t, url, "hosts")
	if rc != 0 || !strings.Contains(out, "vm1") || !strings.Contains(out, "2/2") {
		t.Fatalf("hosts: %d\n%s", rc, out)
	}
	rc, out, _ = runCLI(t, url, "create", "--size", "256Mi", "--shared", "--name", "s0")
	if rc != 0 || !strings.Contains(out, "s0") || !strings.Contains(out, " 256Mi ") {
		t.Fatalf("create: %d\n%s", rc, out)
	}
	rc, out, _ = runCLI(t, url, "-o", "json", "attach", "s0", "--host", "11111111-2222-3333-4444-555555555555")
	var a api.Attachment
	if rc != 0 || json.Unmarshal([]byte(out), &a) != nil || a.State != api.AttachmentAttached || a.Host != "vm1" {
		t.Fatalf("attach: %d\n%s", rc, out)
	}
	rc, out, _ = runCLI(t, url, "devices", "s0")
	if rc != 0 || !strings.Contains(out, "state:    attached") || !strings.Contains(out, "s0@vm1") {
		t.Fatalf("devices s0: %d\n%s", rc, out)
	}
	// the guest holds the device: detach times out with exit code 3
	q.Hold(a.QemuDeviceID)
	rc, out, errs := runCLI(t, url, "detach", "s0", "--host", "vm1", "--timeout", "50ms")
	if rc != exitConflict || !strings.Contains(errs, "did not release") || !strings.Contains(out, "failed") {
		t.Fatalf("detach held: %d\n%s\n%s", rc, out, errs)
	}
	// delete of an attached device is a conflict too
	if rc, _, _ := runCLI(t, url, "delete", "s0"); rc != exitConflict {
		t.Fatalf("delete attached: rc %d", rc)
	}
	// forget the failed attachment, delete
	if rc, _, errs := runCLI(t, url, "detach", "s0", "--host", "vm1", "--force"); rc != 0 {
		t.Fatalf("detach --force: %d %s", rc, errs)
	}
	if rc, _, errs := runCLI(t, url, "delete", "s0"); rc != 0 {
		t.Fatalf("delete: %d %s", rc, errs)
	}
	if rc, _, _ := runCLI(t, url, "devices", "s0"); rc != exitError {
		t.Fatalf("deleted device: rc %d", rc)
	}
	rc, out, _ = runCLI(t, url, "attachments")
	if rc != 0 || strings.Count(out, "\n") != 1 {
		t.Fatalf("attachments: %d\n%s", rc, out)
	}
	// a serial, hex or decimal, needs no server: the wait times out
	for _, dev := range []string{"0xdeadbeef", "3735928559"} {
		rc, _, errs = runCLI(t, "http://127.0.0.1:1", "guest", "wait", dev, "--timeout", "10ms")
		if rc != exitConflict || !strings.Contains(errs, "serial 0xdeadbeef") {
			t.Fatalf("guest wait %s timeout: %d %s", dev, rc, errs)
		}
	}
}

func TestGuestRegionMode(t *testing.T) {
	_, out, _ := runCLI(t, "http://127.0.0.1:1", "--help")
	if !strings.Contains(out, "guest region create DEVICE --mode numa|dax [--decoder decoderX.Y]") ||
		!strings.Contains(out, "create --size 256Mi ") {
		t.Fatalf("usage:\n%s", out)
	}
	rc, _, errs := runCLI(t, "http://127.0.0.1:1", "guest", "region", "create", "0xdeadbeef")
	if rc != exitUsage || !strings.Contains(errs, "error: guest region create DEVICE --mode numa|dax") {
		t.Fatalf("region create without --mode: rc %d stderr %q", rc, errs)
	}
	// a wrong mode is a usage error before the device lookup: the server
	// is unreachable and the device name unknown
	for _, mode := range []string{"devdax", "ram"} {
		rc, _, errs = runCLI(t, "http://127.0.0.1:1", "guest", "region", "create", "nosuchdevice", "--mode", mode)
		if rc != exitUsage || !strings.HasPrefix(errs, `error: invalid mode "`+mode+`" (numa or dax)`) {
			t.Fatalf("region create --mode %s: rc %d stderr %q", mode, rc, errs)
		}
	}
	for _, mode := range []string{cxlapi.ModeNUMA, cxlapi.ModeDax} {
		var out, errb bytes.Buffer
		c := &cli{output: "json", stdout: &out, stderr: &errb}
		if err := c.printRegionInfo(&regionInfo{Memdev: "mem0", Region: "region0", Mode: mode, Node: 2}); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["mode"] != mode {
			t.Fatalf("guest info JSON %q: %v", out.String(), err)
		}
	}
}

func TestCLIPersistent(t *testing.T) {
	url, _ := newTestServer(t)
	rc, out, errs := runCLI(t, url, "create", "--size", "256Mi", "--shared", "--persistent", "--name", "p0")
	if rc != 0 || !strings.Contains(out, "PERSISTENT") {
		t.Fatalf("create --persistent: %d\n%s\n%s", rc, out, errs)
	}
	if rc, _, errs := runCLI(t, url, "create", "--size", "256Mi", "--name", "v0"); rc != 0 {
		t.Fatalf("create: %d %s", rc, errs)
	}
	rc, out, _ = runCLI(t, url, "devices")
	cols := map[string][]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		cols[f[0]] = f
	}
	if h := cols["NAME"]; rc != 0 || len(h) < 5 || h[3] != "SHARED" || h[4] != "PERSISTENT" ||
		cols["p0"][4] != "true" || cols["v0"][4] != "false" {
		t.Fatalf("devices table: %d\n%s", rc, out)
	}
	rc, out, _ = runCLI(t, url, "-o", "json", "devices")
	var ds []map[string]any
	if rc != 0 || json.Unmarshal([]byte(out), &ds) != nil || len(ds) != 2 {
		t.Fatalf("devices JSON: %d\n%s", rc, out)
	}
	for _, d := range ds {
		lsa, hasLSA := d["lsaPath"].(string)
		switch d["name"] {
		case "p0":
			if d["persistent"] != true || !hasLSA || filepath.Base(lsa) != "p0.lsa" {
				t.Fatalf("p0 JSON %v", d)
			}
		case "v0":
			if d["persistent"] != false || hasLSA {
				t.Fatalf("v0 JSON %v", d)
			}
		}
	}
	rc, out, _ = runCLI(t, url, "devices", "p0")
	if rc != 0 || !strings.Contains(out, "memory:   persistent\n") ||
		!regexp.MustCompile(`\nlsa: +/\S+/p0\.lsa\n`).MatchString(out) {
		t.Fatalf("devices p0: %d\n%s", rc, out)
	}
}
