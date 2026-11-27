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
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containers/nri-plugins/pkg/cxl/api"
)

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		req  api.Request
		exec []string
		err  string
	}{
		// every synopsis line
		{[]string{"request", "--size", "512Mi"}, api.Request{Op: "request", Size: "512Mi"}, nil, ""},
		{[]string{"request", "--source", "local", "--mode", "dax", "--read-only", "--size=64Mi", "--serial", "0xc100e2e1", "--wait", "10s", "--request-id", "r1", "--exec", "prog", "--wait"},
			api.Request{Op: "request", Source: "local", Mode: "dax", ReadOnly: ptr(true), Size: "64Mi", Serial: "0xc100e2e1", Wait: "10s", RequestID: "r1"}, []string{"prog", "--wait"}, ""},
		{[]string{"request", "--source=pool", "--size", "512Mi", "--mode", "dax", "--exec", "python3", "app.py"},
			api.Request{Op: "request", Source: "pool", Size: "512Mi", Mode: "dax"}, []string{"python3", "app.py"}, ""},
		{[]string{"request", "--serial", "0xc1ae0001"}, api.Request{Op: "request", Serial: "0xc1ae0001"}, nil, ""},
		{[]string{"request", "--serial", "0xc1ae0001", "--share", "--read-only", "--exec", "prog"},
			api.Request{Op: "request", Serial: "0xc1ae0001", Share: true, ReadOnly: ptr(true)}, []string{"prog"}, ""},
		{[]string{"request", "--share", "--size", "256Mi"}, api.Request{Op: "request", Share: true, Size: "256Mi"}, nil, ""},
		{[]string{"request", "--share=true", "--size", "256Mi"}, api.Request{Op: "request", Share: true, Size: "256Mi"}, nil, ""},
		{[]string{"request", "--share", "--source", "local", "--size", "64Mi"}, api.Request{Op: "request", Share: true, Source: "local", Size: "64Mi"}, nil, ""},
		{[]string{"request", "--serial", "0xc100e2e1", "--share", "--read-only"}, api.Request{Op: "request", Serial: "0xc100e2e1", Share: true, ReadOnly: ptr(true)}, nil, ""},
		{[]string{"request", "--size", "1Gi", "--wait", "0s", "--request-id", "r1"}, api.Request{Op: "request", Size: "1Gi", Wait: "0s", RequestID: "r1"}, nil, ""},
		{[]string{"release", "--lease", "1a2b3c4d"}, api.Request{Op: "release", Lease: "1a2b3c4d"}, nil, ""},
		{[]string{"release", "--all", "--wait=1m"}, api.Request{Op: "release", All: true, Wait: "1m"}, nil, ""},
		{[]string{"list"}, api.Request{Op: "list"}, nil, ""},
		{[]string{"env", "--lease", "1a2b3c4d"}, api.Request{Op: "env", Lease: "1a2b3c4d"}, nil, ""},
		{[]string{"env", "--lease=1a2b3c4d", "--exec", "numactl", "--membind=2", "prog"}, api.Request{Op: "env", Lease: "1a2b3c4d"}, []string{"numactl", "--membind=2", "prog"}, ""},
		// mode fs
		{[]string{"request", "--serial", "0xc1ae0001", "--share", "--read-only", "--mode", "fs", "--access", "cached", "--name", "models", "--exec", "sh", "/tmp/r.sh"},
			api.Request{Op: "request", Serial: "0xc1ae0001", Share: true, ReadOnly: ptr(true), Mode: "fs", Access: "cached", Name: "models"}, []string{"sh", "/tmp/r.sh"}, ""},
		{[]string{"request", "--serial=0xc1ae0001", "--read-only", "--mode=fs", "--access=direct", "--name=m.1"},
			api.Request{Op: "request", Serial: "0xc1ae0001", ReadOnly: ptr(true), Mode: "fs", Access: "direct", Name: "m.1"}, nil, ""},
		{[]string{"request", "--source", "pool", "--mode", "fs", "--read-only", "--share", "--size", "64Mi"},
			api.Request{Op: "request", Source: "pool", Mode: "fs", ReadOnly: ptr(true), Share: true, Size: "64Mi"}, nil, ""},
		{[]string{"request", "--serial", "0xc1af0001", "--share", "--mode", "fs"},
			api.Request{Op: "request", Serial: "0xc1af0001", Share: true, Mode: "fs"}, nil, ""},
		{[]string{"request", "--serial", "0xc1af0001", "--share", "--mode", "fs", "--read-only=false", "--fs-type", "ext4", "--exec", "sh", "/tmp/w.sh"},
			api.Request{Op: "request", Serial: "0xc1af0001", Share: true, Mode: "fs", ReadOnly: ptr(false), FSType: "ext4"}, []string{"sh", "/tmp/w.sh"}, ""},
		{[]string{"request", "--source=local", "--serial=0xc100f5e1", "--mode=fs", "--read-only=0", "--fs-type=xfs", "--access=cached", "--name=data"},
			api.Request{Op: "request", Source: "local", Serial: "0xc100f5e1", Mode: "fs", ReadOnly: ptr(false), FSType: "xfs", Access: "cached", Name: "data"}, nil, ""},
		{[]string{"request", "--serial", "0xc1af0001", "--mode", "fs", "--read-only=true", "--fs-type", "btrfs"},
			api.Request{Op: "request", Serial: "0xc1af0001", Mode: "fs", ReadOnly: ptr(true), FSType: "btrfs"}, nil, ""},
		// mode block
		{[]string{"request", "--serial", "0xc1ae0001", "--mode", "block", "--exec", "sh", "/tmp/w.sh"},
			api.Request{Op: "request", Serial: "0xc1ae0001", Mode: "block"}, []string{"sh", "/tmp/w.sh"}, ""},
		{[]string{"request", "--serial=0xc1ae0001", "--share", "--read-only", "--mode=block", "--source", "pool"},
			api.Request{Op: "request", Serial: "0xc1ae0001", Share: true, ReadOnly: ptr(true), Mode: "block", Source: "pool"}, nil, ""},
		{[]string{"request", "--source", "local", "--serial", "0xc100f5e1", "--mode", "block"},
			api.Request{Op: "request", Source: "local", Serial: "0xc100f5e1", Mode: "block"}, nil, ""},
		// validation errors
		{[]string{"request", "--size", "1Gi", "--source", "any"}, api.Request{}, nil, `invalid source "any" (local or pool)`},
		{[]string{"request", "--size", "1Gi", "--mode", "devdax"}, api.Request{}, nil, `invalid mode "devdax" (numa, dax, fs or block)`},
		{[]string{"request", "--share", "--mode", "numa"}, api.Request{}, nil, "shared memory is never onlined as RAM: use mode dax"},
		{[]string{"request", "--size", "1Gi", "--read-only"}, api.Request{}, nil, "readOnly needs mode dax, fs or block"},
		{[]string{"request"}, api.Request{}, nil, "size or serial is required"},
		{[]string{"request", "--serial", "zz"}, api.Request{}, nil, `invalid serial "zz"`},
		{[]string{"release", "--all", "--exec", "cat"}, api.Request{}, nil, "--exec is for request and env"},
		{[]string{"list", "--exec", "cat"}, api.Request{}, nil, "--exec is for request and env"},
		{[]string{"release"}, api.Request{}, nil, "release takes lease or all"},
		{[]string{"release", "--lease", "1a2b3c4d", "--all"}, api.Request{}, nil, "release takes lease or all"},
		{[]string{"request", "--size", "1Gi", "--wait", "soon"}, api.Request{}, nil, `invalid wait "soon"`},
		{[]string{"release", "--all", "--wait", "-1s"}, api.Request{}, nil, `invalid wait "-1s"`},
		// other usage errors
		{nil, api.Request{}, nil, "no operation"},
		{[]string{"memory", "512Mi"}, api.Request{}, nil, `unknown operation "memory"`},
		{[]string{"fd", "--lease", "1a2b3c4d", "--exec", "prog"}, api.Request{}, nil, `unknown operation "fd"`},
		{[]string{"request", "512Mi"}, api.Request{}, nil, `unexpected argument "512Mi"`},
		{[]string{"request", "--size"}, api.Request{}, nil, "--size needs a value"},
		{[]string{"request", "--dax", "--size", "1Gi"}, api.Request{}, nil, "unknown option --dax"},
		{[]string{"request", "--size", "1Gi", "--lease", "1a2b3c4d"}, api.Request{}, nil, "--lease is not an option of request"},
		{[]string{"release", "--all", "--size", "1Gi"}, api.Request{}, nil, "--size is not an option of release"},
		{[]string{"list", "--all"}, api.Request{}, nil, "--all is not an option of list"},
		{[]string{"env", "--lease", "1a2b3c4d", "--read-only"}, api.Request{}, nil, "--read-only is not an option of env"},
		{[]string{"env"}, api.Request{}, nil, "env needs lease"},
		{[]string{"request", "--size", "1Gi", "--exec"}, api.Request{}, nil, "--exec needs a command"},
		{[]string{"request", "--share=maybe"}, api.Request{}, nil, `--share: invalid value "maybe"`},
		{[]string{"request", "--size", "1Gi", "--name"}, api.Request{}, nil, "--name needs a value"},
		{[]string{"env", "--lease", "1a2b3c4d", "--access", "direct"}, api.Request{}, nil, "--access is not an option of env"},
		{[]string{"release", "--all", "--name", "x"}, api.Request{}, nil, "--name is not an option of release"},
		{[]string{"request", "--mode", "fs", "--size", "1Gi", "--fs-type"}, api.Request{}, nil, "--fs-type needs a value"},
		{[]string{"request", "--mode", "fs", "--size", "1Gi", "--read-only=rw"}, api.Request{}, nil, `--read-only: invalid value "rw"`},
		{[]string{"request", "--mode", "fs", "--size", "1Gi", "--read-write"}, api.Request{}, nil, "unknown option --read-write"},
		{[]string{"env", "--lease", "1a2b3c4d", "--read-only"}, api.Request{}, nil, "--read-only is not an option of env"},
	} {
		cmd, err := parseArgs(c.args)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: expected error %q, got %v", c.args, c.err, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if !reflect.DeepEqual(cmd.req, c.req) || !reflect.DeepEqual(cmd.exec, c.exec) {
			t.Errorf("%v: got %+v %v", c.args, cmd.req, cmd.exec)
		}
	}
}

func TestOptions(t *testing.T) {
	want := []string{"--source", "--mode", "--share", "--read-only", "--access", "--fs-type", "--name", "--size", "--serial", "--wait",
		"--request-id", "--exec"}
	if got := options[api.OpRequest]; !reflect.DeepEqual(got, want) {
		t.Fatalf("options of request %v, want %v", got, want)
	}
	cmd := &command{}
	for _, name := range []string{"--access", "--name", "--fs-type"} {
		if cmd.values()[name] == nil || cmd.isFlag(name) {
			t.Errorf("%s is not an option with a value", name)
		}
	}
	for _, name := range []string{"--share", "--read-only", "--all"} {
		if !cmd.isFlag(name) || cmd.values()[name] != nil {
			t.Errorf("%s is not a flag", name)
		}
	}
	for _, line := range []string{"[--mode numa|dax|fs|block]", "[--read-only[=false]] [--access direct|cached] [--fs-type TYPE]",
		"fs: the mount point /run/cxl/fs/NAME; default the lease id",
		"--read-only a dax or block fd that allows reads only; fs is read-only\n              unless --read-only=false",
		"--fs-type   fs: the expected filesystem type (default: detected)",
		"block (an fd of the block device of a persistent device);",
		"--exec      run CMD with the lease's environment and a dax or block lease's fd 3"} {
		if !strings.Contains(usage, line) {
			t.Errorf("usage lacks %q", line)
		}
	}
}

// Requests that the validation refuses exit with 2 before anything is sent.
func TestRunRefusesFs(t *testing.T) {
	t.Setenv(api.SocketEnv, filepath.Join(t.TempDir(), "none.sock"))
	for _, c := range []struct {
		args []string
		err  string
	}{
		{[]string{"request", "--serial", "0xc1ae0001", "--mode", "blk"}, `invalid mode "blk" (numa, dax, fs or block)`},
		{[]string{"request", "--serial", "0xc1ae0001", "--share", "--mode", "block"}, "mode block with share needs readOnly"},
		{[]string{"request", "--mode", "block", "--access", "direct", "--size", "64Mi"}, "access needs mode fs"},
		{[]string{"request", "--mode", "fs", "--read-only", "--access", "slow", "--share", "--size", "64Mi"}, `invalid access "slow" (direct or cached)`},
		{[]string{"request", "--access", "cached", "--size", "64Mi"}, "access needs mode fs"},
		{[]string{"request", "--name", "x", "--size", "64Mi"}, "name needs mode fs"},
		{[]string{"request", "--mode", "fs", "--read-only", "--name", "a/b", "--size", "64Mi"},
			`invalid name "a/b" (letters, digits, ".", "_" and "-", not "." or "..")`},
		{[]string{"request", "--mode", "fs", "--read-only", "--name", "..", "--size", "64Mi"}, `invalid name ".."`},
		{[]string{"request", "--serial", "0xc1ae0001", "--mode", "dax", "--fs-type", "ext4"}, "fsType needs mode fs"},
		{[]string{"request", "--serial", "0xc1ae0001", "--mode", "fs", "--fs-type", "Ext4"}, `invalid fsType "Ext4" (lower-case letters, digits, "_" and "-")`},
		{[]string{"request", "--serial", "0xc1ae0001", "--share", "--mode", "block", "--read-only=false"}, "mode block with share needs readOnly"},
		{[]string{"request", "--size", "64Mi", "--read-only", "--mode", "numa"}, "readOnly needs mode dax, fs or block"},
	} {
		var out, errb bytes.Buffer
		if code := run(c.args, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "cxl-request: "+c.err) {
			t.Errorf("%v: exit %d, stderr %q; want %d and %q", c.args, code, errb.String(), exitUsage, c.err)
		}
	}
}

func TestRunExecFs(t *testing.T) {
	resp := `{"lease":"000000b1","state":"Ready","source":"pool","mode":"fs","share":true,"readOnly":true,"access":"cached","path":"/run/cxl/fs/models",` +
		`"size":534773760,"serial":"0xc1ae0001","env":{"CXL_C1AE0001_ACCESS":"cached","CXL_C1AE0001_MODE":"fs","CXL_C1AE0001_PATH":"/run/cxl/fs/models",` +
		`"CXL_C1AE0001_READ_ONLY":"1","CXL_C1AE0001_SERIAL":"0xc1ae0001","CXL_C1AE0001_SHARED":"1","CXL_C1AE0001_SIZE":"534773760"}}`
	sock, reqs := fakeServer(t, resp, "")
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	script := `echo "$CXL_C1AE0001_PATH $CXL_C1AE0001_ACCESS $CXL_C1AE0001_MODE $CXL_C1AE0001_READ_ONLY $CXL_LEASE ${CXL_C1AE0001_FD-nofd}"; ` +
		`if (exec 9<&3) 2>/dev/null; then echo fd3; else echo nofd3; fi`
	code := run([]string{"request", "--serial", "0xc1ae0001", "--share", "--read-only", "--mode", "fs", "--access", "cached", "--name", "models",
		"--exec", "/bin/sh", "-c", script}, &out, &errb)
	if code != exitOK {
		t.Fatalf("exit %d (stderr %s)", code, errb.String())
	}
	if r := <-reqs; !reflect.DeepEqual(r, api.Request{Op: "request", Serial: "0xc1ae0001", Share: true, ReadOnly: ptr(true), Mode: "fs", Access: "cached", Name: "models"}) {
		t.Fatalf("request %+v", r)
	}
	if want := "/run/cxl/fs/models cached fs 1 000000b1 nofd\nnofd3\n"; out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}

	// env prints the same variables
	out.Reset()
	if code := run([]string{"env", "--lease", "000000b1"}, &out, &errb); code != exitOK {
		t.Fatalf("env: exit %d", code)
	}
	want := "CXL_C1AE0001_ACCESS=cached\nCXL_C1AE0001_MODE=fs\nCXL_C1AE0001_PATH=/run/cxl/fs/models\nCXL_C1AE0001_READ_ONLY=1\n" +
		"CXL_C1AE0001_SERIAL=0xc1ae0001\nCXL_C1AE0001_SHARED=1\nCXL_C1AE0001_SIZE=534773760\nCXL_LEASE=000000b1\n"
	if out.String() != want {
		t.Fatalf("env output\n%s\nwant\n%s", out.String(), want)
	}
	<-reqs

	// env --exec of an fs lease asks env alone and passes no fd
	out.Reset()
	if code := run([]string{"env", "--lease", "000000b1", "--exec", "/bin/sh", "-c", script}, &out, &errb); code != exitOK ||
		out.String() != "/run/cxl/fs/models cached fs 1 000000b1 nofd\nnofd3\n" {
		t.Fatalf("env --exec: exit %d output %q", code, out.String())
	}
	if r := <-reqs; !reflect.DeepEqual(r, api.Request{Op: "env", Lease: "000000b1"}) {
		t.Fatalf("request %+v", r)
	}
	if len(reqs) != 0 {
		t.Fatalf("env --exec of an fs lease sent %d requests more", len(reqs))
	}
}

func TestRunExecBlock(t *testing.T) {
	dir := t.TempDir()
	pmem := filepath.Join(dir, "pmem")
	if err := os.WriteFile(pmem, []byte("BLK-5678"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := `{"lease":"000000c1","state":"Ready","source":"pool","mode":"block","share":false,"readOnly":false,"size":534773760,"serial":"0xc1ae0001","block":"/dev/pmem0",` +
		`"env":{"CXL_C1AE0001_MODE":"block","CXL_C1AE0001_PATH":"/dev/pmem0","CXL_C1AE0001_READ_ONLY":"0","CXL_C1AE0001_SERIAL":"0xc1ae0001","CXL_C1AE0001_SHARED":"1","CXL_C1AE0001_SIZE":"534773760"}}`
	sock, reqs := fakeServer(t, resp, pmem)
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	script := `echo "$CXL_C1AE0001_FD $CXL_LEASE ${CXL_C1AE0001_PATH-nopath} $CXL_C1AE0001_MODE $CXL_C1AE0001_SIZE $CXL_C1AE0001_READ_ONLY"; head -c 8 <&3; exit 5`
	code := run([]string{"request", "--serial", "0xc1ae0001", "--mode", "block", "--exec", "/bin/sh", "-c", script}, &out, &errb)
	if code != 5 {
		t.Fatalf("exit %d (stderr %s)", code, errb.String())
	}
	if r := <-reqs; !reflect.DeepEqual(r, api.Request{Op: "request", Serial: "0xc1ae0001", Mode: "block"}) {
		t.Fatalf("request %+v", r)
	}
	want := "3 000000c1 nopath block 534773760 0\nBLK-5678"
	if out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}

	// env --exec: the same environment and fd, by op env and then op fd
	out.Reset()
	if code := run([]string{"env", "--lease", "000000c1", "--exec", "/bin/sh", "-c", script}, &out, &errb); code != 5 || out.String() != want {
		t.Fatalf("env --exec: exit %d output %q", code, out.String())
	}
	for _, want := range []api.Request{{Op: "env", Lease: "000000c1"}, {Op: "fd", Lease: "000000c1", Wait: "0s"}} {
		if r := <-reqs; !reflect.DeepEqual(r, want) {
			t.Fatalf("request %+v, want %+v", r, want)
		}
	}

	// a block response without its fd is an error
	errb.Reset()
	sock, _ = fakeServer(t, resp, "")
	t.Setenv(api.SocketEnv, sock)
	if code := run([]string{"request", "--serial", "0xc1ae0001", "--mode", "block", "--exec", "/bin/true"}, &out, &errb); code != exitError ||
		!strings.Contains(errb.String(), "no fd with the response of block lease 000000c1") {
		t.Fatalf("block lease without fd: exit %d %s", code, errb.String())
	}
}

func TestExitCode(t *testing.T) {
	released := []api.Response{{State: api.StateReleased}}
	releasing := []api.Response{{State: api.StateReleasing}}
	for _, c := range []struct {
		req  api.Request
		resp api.Response
		code int
	}{
		{api.Request{Op: "request"}, api.Response{State: "Ready"}, exitOK},
		{api.Request{Op: "request"}, api.Response{State: "Pending"}, exitLeaseState},
		{api.Request{Op: "request", Wait: "0s"}, api.Response{State: "Pending"}, exitOK},
		{api.Request{Op: "request", Wait: "0"}, api.Response{State: "Pending"}, exitOK},
		{api.Request{Op: "request"}, api.Response{State: "Failed", Error: "timeout"}, exitLeaseState},
		{api.Request{Op: "request"}, api.Response{Error: "not a container of the claim's pod"}, exitError},
		{api.Request{Op: "release"}, api.Response{State: "Released"}, exitOK},
		{api.Request{Op: "release"}, api.Response{State: "Releasing"}, exitLeaseState},
		{api.Request{Op: "release", All: true}, api.Response{Leases: &released}, exitOK},
		{api.Request{Op: "release", All: true}, api.Response{Leases: &releasing}, exitLeaseState},
		{api.Request{Op: "list"}, api.Response{Leases: &[]api.Response{}}, exitOK},
		{api.Request{Op: "env"}, api.Response{State: "Ready", Mode: "numa"}, exitOK},
		{api.Request{Op: "env"}, api.Response{State: "Ready", Mode: "dax"}, exitOK},
		{api.Request{Op: "env"}, api.Response{State: "Pending", Mode: "block"}, exitLeaseState},
		{api.Request{Op: "env"}, api.Response{Error: "no such lease of this container"}, exitError},
		{api.Request{Op: "env", Wait: "0s"}, api.Response{State: "Pending"}, exitLeaseState},
	} {
		if got := exitCode(&c.req, &c.resp); got != c.code {
			t.Errorf("%+v %+v: exit %d, want %d", c.req, c.resp, got, c.code)
		}
	}
}

// fakeServer answers one request per connection with resp, and with the
// file as SCM_RIGHTS if fdFile is not empty.
func fakeServer(t *testing.T, resp string, fdFile string) (string, chan api.Request) {
	t.Helper()
	return fakeServerOf(t, func(api.Request) string { return resp }, fdFile)
}

// fakeServerOf answers one request per connection with the response that
// respond gives for it, and with the file as SCM_RIGHTS if fdFile is not
// empty.
func fakeServerOf(t *testing.T, respond func(api.Request) string, fdFile string) (string, chan api.Request) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cxlreq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "cxl.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	reqs := make(chan api.Request, 10)
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var r api.Request
			_ = json.Unmarshal(line, &r)
			reqs <- r
			resp := respond(r)
			if fdFile != "" {
				f, _ := os.Open(fdFile)
				_, _, _ = c.WriteMsgUnix([]byte(resp+"\n"), syscall.UnixRights(int(f.Fd())), nil)
				_ = f.Close()
			} else {
				_, _ = c.Write([]byte(resp + "\n"))
			}
			_ = c.Close()
		}
	}()
	return sock, reqs
}

func TestRunPrintsResponse(t *testing.T) {
	resp := `{"lease":"000000a1","state":"Ready","source":"local","mode":"numa","share":false,"readOnly":false,"size":268435456,"serial":"0xc100e2e1","numaNode":2}`
	sock, reqs := fakeServer(t, resp, "")
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	if code := run([]string{"request", "--size", "256Mi", "--wait", "30s"}, &out, &errb); code != exitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != resp {
		t.Fatalf("output %q", out.String())
	}
	if r := <-reqs; !reflect.DeepEqual(r, api.Request{Op: "request", Size: "256Mi", Wait: "30s"}) {
		t.Fatalf("request %+v", r)
	}

	t.Setenv(api.SocketEnv, "")
	errb.Reset()
	if code := run([]string{"list"}, &out, &errb); code != exitError ||
		!strings.Contains(errb.String(), "CXL_LEASE_SOCKET is not set (the pod needs a claim on DeviceClass cxl-lease)") {
		t.Fatalf("no socket: exit %d %s", code, errb.String())
	}
	if code := run([]string{"bogus"}, &out, &errb); code != exitUsage {
		t.Fatalf("usage: exit %d", code)
	}
	out.Reset()
	if code := run([]string{"help"}, &out, &errb); code != exitOK || !strings.Contains(out.String(), "cxl-request env --lease ID") {
		t.Fatalf("help: exit %d %q", code, out.String())
	}
}

func TestRunEnv(t *testing.T) {
	resp := `{"lease":"1a2b3c4d","state":"Ready","source":"pool","mode":"dax","share":false,"readOnly":false,"size":536870912,"serial":"0xc1ee0001","dax":"/dev/dax0.0",` +
		`"env":{"CXL_C1EE0001_DAX":"/dev/dax0.0","CXL_C1EE0001_MODE":"dax","CXL_C1EE0001_READ_ONLY":"0","CXL_C1EE0001_SERIAL":"0xc1ee0001","CXL_C1EE0001_SHARED":"0","CXL_C1EE0001_SIZE":"536870912"}}`
	sock, reqs := fakeServer(t, resp, "")
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	if code := run([]string{"env", "--lease", "1a2b3c4d"}, &out, &errb); code != exitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := "CXL_C1EE0001_DAX=/dev/dax0.0\nCXL_C1EE0001_MODE=dax\nCXL_C1EE0001_READ_ONLY=0\nCXL_C1EE0001_SERIAL=0xc1ee0001\n" +
		"CXL_C1EE0001_SHARED=0\nCXL_C1EE0001_SIZE=536870912\nCXL_LEASE=1a2b3c4d\n"
	if out.String() != want {
		t.Fatalf("env output\n%s\nwant\n%s", out.String(), want)
	}
	if r := <-reqs; !reflect.DeepEqual(r, api.Request{Op: "env", Lease: "1a2b3c4d"}) {
		t.Fatalf("request %+v", r)
	}
}

func TestRunEnvNotReady(t *testing.T) {
	sock, _ := fakeServer(t, `{"lease":"1a2b3c4d","state":"Pending","mode":"numa","share":false,"readOnly":false}`, "")
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	if code := run([]string{"env", "--lease", "1a2b3c4d", "--wait", "0s"}, &out, &errb); code != exitLeaseState || out.Len() != 0 {
		t.Fatalf("exit %d output %q", code, out.String())
	}
}

func TestRunExecDax(t *testing.T) {
	dir := t.TempDir()
	dax := filepath.Join(dir, "dax")
	if err := os.WriteFile(dax, []byte("TAG-1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := `{"lease":"000000a2","state":"Ready","source":"pool","mode":"dax","share":true,"readOnly":true,"size":268435456,"serial":"0xc1ae0001","dax":"/dev/dax0.0",` +
		`"env":{"CXL_C1AE0001_DAX":"/dev/dax0.0","CXL_C1AE0001_MODE":"dax","CXL_C1AE0001_READ_ONLY":"1","CXL_C1AE0001_SERIAL":"0xc1ae0001","CXL_C1AE0001_SHARED":"1","CXL_C1AE0001_SIZE":"268435456"}}`
	sock, reqs := fakeServer(t, resp, dax)
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	script := `echo "$CXL_C1AE0001_FD $CXL_LEASE ${CXL_C1AE0001_DAX-none} $CXL_C1AE0001_SIZE $CXL_C1AE0001_READ_ONLY $CXL_C1AE0001_SHARED"; head -c 8 <&3; exit 7`
	code := run([]string{"request", "--serial", "0xc1ae0001", "--share", "--read-only", "--exec", "/bin/sh", "-c", script}, &out, &errb)
	if code != 7 {
		t.Fatalf("exit %d (stderr %s)", code, errb.String())
	}
	if r := <-reqs; !reflect.DeepEqual(r, api.Request{Op: "request", Serial: "0xc1ae0001", Share: true, ReadOnly: ptr(true)}) {
		t.Fatalf("request %+v", r)
	}
	want := "3 000000a2 none 268435456 1 1\nTAG-1234"
	if out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}

	// env --exec: the same environment and fd, by op env and then op fd
	out.Reset()
	if code := run([]string{"env", "--lease", "000000a2", "--exec", "/bin/sh", "-c", script}, &out, &errb); code != 7 || out.String() != want {
		t.Fatalf("env --exec: exit %d output %q", code, out.String())
	}
	for _, want := range []api.Request{{Op: "env", Lease: "000000a2"}, {Op: "fd", Lease: "000000a2", Wait: "0s"}} {
		if r := <-reqs; !reflect.DeepEqual(r, want) {
			t.Fatalf("request %+v, want %+v", r, want)
		}
	}

	// without --exec the JSON is printed and the fd dropped
	out.Reset()
	if code := run([]string{"request", "--serial", "0xc1ae0001", "--share"}, &out, &errb); code != exitOK || strings.TrimSpace(out.String()) != resp {
		t.Fatalf("exit %d output %q", code, out.String())
	}
}

func TestRunExecNUMA(t *testing.T) {
	resp := `{"lease":"000000a4","state":"Ready","source":"local","mode":"numa","share":false,"readOnly":false,"size":134217728,"serial":"0xc100e2e1","numaNode":2,` +
		`"env":{"CXL_C100E2E1_MODE":"numa","CXL_C100E2E1_NUMA_NODE":"2","CXL_C100E2E1_SERIAL":"0xc100e2e1","CXL_C100E2E1_SHARED":"0","CXL_C100E2E1_SIZE":"134217728"}}`
	sock, _ := fakeServer(t, resp, "")
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	script := `echo "$CXL_C100E2E1_NUMA_NODE $CXL_C100E2E1_SIZE $CXL_LEASE ${CXL_C100E2E1_FD-nofd}"; exec 3<&- 2>/dev/null; exit 0`
	if code := run([]string{"request", "--size", "128Mi", "--exec", "/bin/sh", "-c", script}, &out, &errb); code != exitOK {
		t.Fatalf("exit %d (stderr %s)", code, errb.String())
	}
	if out.String() != "2 134217728 000000a4 nofd\n" {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	if code := run([]string{"env", "--lease", "000000a4", "--exec", "/bin/sh", "-c", script}, &out, &errb); code != exitOK || out.String() != "2 134217728 000000a4 nofd\n" {
		t.Fatalf("env --exec: exit %d output %q", code, out.String())
	}
}

// env --exec of a dax lease in a container of the pod that did not request
// it: the driver answers op env, and refuses op fd; the command does not
// run.
func TestRunExecDaxOtherContainer(t *testing.T) {
	ready := `{"lease":"000000a8","state":"Ready","mode":"dax","share":true,"readOnly":true,"env":{"CXL_C1AE0001_MODE":"dax"}}`
	sock, reqs := fakeServerOf(t, func(r api.Request) string {
		if r.Op == api.OpFd {
			return `{"lease":"000000a8","error":"no such lease of this container"}`
		}
		return ready
	}, "")
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	if code := run([]string{"env", "--lease", "000000a8", "--exec", "/bin/echo", "ran"}, &out, &errb); code != exitError ||
		out.Len() != 0 || !strings.Contains(errb.String(), "no such lease of this container") {
		t.Fatalf("exit %d output %q stderr %q", code, out.String(), errb.String())
	}
	if r1, r2 := <-reqs, <-reqs; r1.Op != api.OpEnv || r2.Op != api.OpFd {
		t.Fatalf("requests %+v %+v", r1, r2)
	}
}

func TestRunExecNotReady(t *testing.T) {
	sock, _ := fakeServer(t, `{"lease":"000000a3","state":"Failed","mode":"dax","share":true,"readOnly":false,"error":"timeout: not Ready after 2m0s"}`, "")
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	if code := run([]string{"request", "--serial", "0x1", "--share", "--exec", "/bin/true"}, &out, &errb); code != exitLeaseState {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "timeout") {
		t.Fatalf("stderr %q", errb.String())
	}
	// asynchronous: Pending is ok without --exec, not with it
	sock, _ = fakeServer(t, `{"lease":"000000a5","state":"Pending","mode":"numa","share":false,"readOnly":false}`, "")
	t.Setenv(api.SocketEnv, sock)
	if code := run([]string{"request", "--size", "1Gi", "--wait", "0s"}, &out, &errb); code != exitOK {
		t.Fatalf("wait 0s: exit %d", code)
	}
	if code := run([]string{"request", "--size", "1Gi", "--wait", "0s", "--exec", "/bin/true"}, &out, &errb); code != exitLeaseState {
		t.Fatalf("wait 0s --exec: exit %d", code)
	}
}

// A command that a signal ends exits 128+signal.
func TestRunExecSignaled(t *testing.T) {
	sock, _ := fakeServer(t, `{"lease":"000000a6","state":"Ready","mode":"numa","share":false,"readOnly":false}`, "")
	t.Setenv(api.SocketEnv, sock)
	var out, errb bytes.Buffer
	if code := run([]string{"request", "--size", "1Gi", "--exec", "/bin/sh", "-c", "kill -TERM $$"}, &out, &errb); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit %d, expected %d (stderr %s)", code, 128+int(syscall.SIGTERM), errb.String())
	}
}

// A signal to cxl-request is forwarded to the command.
func TestRunExecForwardsSignals(t *testing.T) {
	sock, _ := fakeServer(t, `{"lease":"000000a7","state":"Ready","mode":"numa","share":false,"readOnly":false}`, "")
	t.Setenv(api.SocketEnv, sock)
	ready := filepath.Join(t.TempDir(), "ready")
	script := `trap 'exit 42' TERM; touch "$1"; while :; do sleep 0.01; done`
	done := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		done <- run([]string{"request", "--size", "1Gi", "--exec", "/bin/sh", "-c", script, "sh", ready}, &out, &errb)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 42 {
			t.Fatalf("exit %d, expected the trap's 42", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the signal was not forwarded")
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T {
	return &v
}
