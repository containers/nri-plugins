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

// The NRI plugin (internal/containers) as the driver wires it: the fence
// that the device engine gets, the nodes fenced from the start, the order
// of Shutdown.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/containers"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// TestShutdownStopsNRIFirst checks that Shutdown makes a close of the NRI
// connection no loss before it stops anything else: the runtime may close
// it while the broker stops.
func TestShutdownStopsNRIFirst(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Dir: t.TempDir(), BootID: "boot-1"})
	noErr(t, err)
	t.Cleanup(func() { _ = st.Close() })
	nri, err := containers.New(containers.Options{Store: st, SysRoot: t.TempDir()})
	noErr(t, err)
	stopping := false
	d := &driver{nri: nri, stopBroker: func() { stopping = nri.Stopping() }}
	d.Shutdown()
	if !stopping {
		t.Fatal("the broker stopped before the NRI plugin was marked stopping")
	}
}

// TestFenceOf checks the fence that the device engine gets: the NRI
// plugin's with fencing allowed-mems (also before NRI connects: the node is
// recorded), none with fencing none.
func TestFenceOf(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Dir: t.TempDir(), BootID: "boot-1"})
	noErr(t, err)
	t.Cleanup(func() { _ = st.Close() })
	for _, fencing := range []string{"", config.FencingAllowedMems, config.FencingNone} {
		nri, err := containers.New(containers.Options{Defaults: config.Container{Fencing: fencing}, Store: st, SysRoot: t.TempDir()})
		noErr(t, err)
		fence := fenceOf(nri)
		if (fence == nil) != (fencing == config.FencingNone) {
			t.Fatalf("fencing %q: fence %v", fencing, fence != nil)
		}
		if fence != nil {
			fence(context.Background(), 2)
			if got := nri.Fenced().String(); got != "2" {
				t.Fatalf("fencing %q: fenced %s", fencing, got)
			}
		}
	}
	// the driver configuration and the plugin agree on the mode
	for _, cfg := range []string{"", `{"defaults":{"container":{"fencing":"none"}}}`} {
		dc, err := newDriverConfigFromString(cfg)
		noErr(t, err)
		nri, err := containers.New(containers.Options{Defaults: dc.containerDefaults(), Store: st, SysRoot: t.TempDir()})
		noErr(t, err)
		if nri.Fencing() != (dc.fencing() == config.FencingAllowedMems) {
			t.Fatalf("config %q: plugin fencing %v, driver %s", cfg, nri.Fencing(), dc.fencing())
		}
	}
}

// TestFenceSeed checks the NUMA nodes that the NRI plugin fences from the
// start: those of the prepared numa claims, and on a lost store, which does
// not know the claims, every NUMA node of online CXL memory.
func TestFenceSeed(t *testing.T) {
	e := newDriverTestEnv(t)
	e.start()
	e.synchronize()
	e.mustPrepare(e.d.local, localClaim("u1", 64*mib))
	want := dra.NUMANodes(e.records(), device.DriverName)
	if len(want) != 1 {
		t.Fatalf("NUMA nodes of the prepared claim %v", want)
	}
	e.restart()
	if got := e.d.nri.Fenced().List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("fenced %v, want %v", got, want)
	}
	e.stop()
	for _, name := range []string{store.FileName, store.PrevName} {
		noErr(t, os.WriteFile(filepath.Join(e.config.KubeletPluginDir, name), []byte("{"), 0o600))
	}
	e.start()
	if r := e.records(); len(r.Preparations) > 0 {
		t.Fatalf("preparations of a lost store %+v", r.Preparations)
	}
	if got := e.d.nri.Fenced().List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("fenced on a lost store %v, want %v", got, want)
	}
}
