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
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/leases"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	k8stesting "k8s.io/client-go/testing"
)

// testParseFlags returns the flags of args with the environment env.
func testParseFlags(args []string, env map[string]string) (*flags, error) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return parseFlags(fs, args, func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	})
}

// The defaults of the flags; --new-state changes NewState only.
func TestParseFlagsDefaults(t *testing.T) {
	f, err := testParseFlags(nil, map[string]string{"NODE_NAME": "n1"})
	if err != nil {
		t.Fatal(err)
	}
	want := &flags{
		NodeName:            "n1",
		CDIRoot:             "/var/run/cdi",
		NRIName:             "dra-driver-cxl",
		NRIIdx:              "30",
		Pool:                true,
		PoolPrepareTimeout:  30 * time.Second,
		Leases:              true,
		LeaseCLI:            "/usr/local/bin/cxl-request",
		LeaseWait:           120 * time.Second,
		LeaseCompanionImage: "registry.k8s.io/pause:3.10",
		LeaseLimits:         leases.Limits{MaxConnections: 64, IdleTimeout: 10 * time.Minute, MaxPerPod: 16, LeasesPerNode: 32, RequestsPerMinute: 32},
	}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("flags\n%+v, expected\n%+v", f, want)
	}
	if f.poolDriver() != "cxl-pool.nri.io" || device.DriverName != "cxl.nri.io" {
		t.Fatalf("driver names %s %s", device.DriverName, f.poolDriver())
	}
	f, err = testParseFlags([]string{"--new-state", "--pool=false"}, map[string]string{"NODE_NAME": "n1"})
	want.NewState, want.Pool = true, false
	if err != nil || !reflect.DeepEqual(f, want) || f.poolDriver() != "" {
		t.Fatalf("flags\n%+v, expected\n%+v, error %v", f, want, err)
	}
}

// NODE_NAME and KUBECONFIG are the defaults of --node-name and
// --kubeconfig; the command line overrides them. No other flag has an
// environment variable.
func TestParseFlagsEnvironment(t *testing.T) {
	env := map[string]string{"NODE_NAME": "n1", "KUBECONFIG": "/kubeconfig", "LEASES": "false", "CONFIG_STR": "{}"}
	f, err := testParseFlags([]string{"--config", "/config.yaml"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if f.NodeName != "n1" || f.KubeConfig != "/kubeconfig" || !f.Leases || f.Config != "/config.yaml" {
		t.Fatalf("flags %+v", f)
	}
	f, err = testParseFlags([]string{"--node-name", "n2", "--kubeconfig="}, env)
	if err != nil || f.NodeName != "n2" || f.KubeConfig != "" {
		t.Fatalf("flags %+v, error %v", f, err)
	}
}

// TestParseFlagsForget pins --forget-provenance: repeatable, a
// comma-separated list of serials in hex or decimal, and an invalid serial
// refused.
func TestParseFlagsForget(t *testing.T) {
	f, err := testParseFlags([]string{"--forget-provenance", "0xC1AE2001", "--forget-provenance=161, 0xa2,,"}, map[string]string{"NODE_NAME": "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.ForgetProvenance, []uint64{0xc1ae2001, 0xa1, 0xa2}) {
		t.Fatalf("serials %#x", f.ForgetProvenance)
	}
	if got := (*serialsFlag)(&f.ForgetProvenance).String(); got != "0xc1ae2001,0xa1,0xa2" {
		t.Fatalf("value %q", got)
	}
	if (*serialsFlag)(nil).String() != "" {
		t.Fatal("the value of no flag")
	}
}

func TestParseFlagsErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "--node-name"},
		{[]string{"--node-name", "n1", "extra"}, "arguments not supported"},
		{[]string{"--node-name", "n1", "--lease-wait", "61m"}, "--lease-wait 1h1m0s: not between 0s and 1h0m0s"},
		{[]string{"--node-name", "n1", "--lease-wait", "-1s"}, "--lease-wait -1s: not between 0s and 1h0m0s"},
		{[]string{"--node-name", "n1", "--lease-cli=", "--lease-cli-source", "/bin/cxl-request"}, "--lease-cli"},
		{[]string{"--no-such-flag"}, "no-such-flag"},
		{[]string{"--node-name", "n1", "-f", "/config.yaml"}, "provided but not defined: -f"},
		{[]string{"--node-name", "n1", "--forget-provenance", "0xa1,0xzz"}, `invalid value "0xa1,0xzz" for flag -forget-provenance: invalid serial "0xzz"`},
	} {
		if _, err := testParseFlags(tc.args, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %v: error %v, expected one with %q", tc.args, err, tc.want)
		}
	}
}

func TestInstallLeaseCLI(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "cxl-request")
	dst := filepath.Join(dir, "plugins", "cxl.nri.io", "cxl-request")
	if err := installLeaseCLI(src, dst); err == nil {
		t.Fatal("missing source: expected an error")
	}
	if err := os.MkdirAll(filepath.Dir(src), 0755); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"v1", "v2"} {
		if err := os.WriteFile(src, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if err := installLeaseCLI(src, dst); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(dst)
		if err != nil || string(data) != content {
			t.Fatalf("installed %q %v, expected %q", data, err, content)
		}
		info, err := os.Stat(dst)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("installed mode %v %v", info.Mode(), err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil || len(entries) != 1 {
		t.Fatalf("files next to the CLI: %v %v", entries, err)
	}
}

// newDriver starts the NRI plugin after the first list of the pool
// devices and after the CDI specs of the prepared claims are written
// again, and before the kubelet plugins are registered and the node's
// ResourceSlices are published.
func TestStartOrder(t *testing.T) {
	e := newDriverTestEnv(t)
	e.start()
	e.synchronize()
	e.mustPrepare(e.d.local, localClaim("u1", localBlockSize))
	e.mustPrepare(e.d.pool, poolClaim("u2"))
	e.stop()
	specs := []string{filepath.Join(e.config.Flags.CDIRoot, device.ClaimsCDISpecName), filepath.Join(e.config.Flags.CDIRoot, device.PoolCDISpecName)}
	for _, spec := range specs {
		noErr(t, os.Remove(spec))
	}
	sockets := func() []string {
		socks, err := filepath.Glob(filepath.Join(e.config.KubeletPluginsRegistryDir, "*.sock"))
		noErr(t, err)
		return socks
	}
	if socks := sockets(); len(socks) > 0 {
		t.Fatalf("registration sockets left after the stop: %v", socks)
	}
	hasAction := func(verb string) bool {
		return slices.ContainsFunc(e.client.Actions(), func(a k8stesting.Action) bool { return a.Matches(verb, "resourceslices") })
	}
	e.client.ClearActions()
	started := false
	e.runtime.onStart = func() error {
		started = true
		if !hasAction("list") || hasAction("create") {
			t.Errorf("NRI started with the actions %v: want a list of ResourceSlices and no create", e.client.Actions())
		}
		if socks := sockets(); len(socks) > 0 {
			t.Errorf("NRI started after the registration of %v", socks)
		}
		for _, spec := range specs {
			if _, err := os.Stat(spec); err != nil {
				t.Errorf("NRI started before the CDI spec is written: %v", err)
			}
		}
		return nil
	}
	e.start()
	if !started {
		t.Fatal("NRI did not start")
	}
	want := []string{filepath.Join(e.config.KubeletPluginsRegistryDir, device.DriverName+"-reg.sock"),
		filepath.Join(e.config.KubeletPluginsRegistryDir, device.PoolDriverName+"-reg.sock")}
	slices.Sort(want)
	if socks := sockets(); !slices.Equal(socks, want) {
		t.Errorf("registration sockets %v, want %v", socks, want)
	}
	for deadline := time.Now().Add(5 * time.Second); !hasAction("create"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no ResourceSlice published in 5 s")
		}
	}
	e.synchronize()
	e.mustUnprepare(e.d.local, "u1")
	e.mustUnprepare(e.d.pool, "u2")
	e.wantClean()
}
