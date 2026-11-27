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
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/config"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// TestDriverConfigKeys checks the decoding of the configuration: its keys
// are the lower-camel json tags, in JSON or YAML, from the file of --config
// or none; unknown keys, values of another type and invalid memdevs are
// refused, and manageMemdevs is true unless set false.
func TestDriverConfigKeys(t *testing.T) {
	cfg := &DriverConfig{IgnoreMemdevs: []string{"mem0"}, IgnoreRegions: []string{"region0"}, IgnoreNUMANodes: []int{1},
		IgnoreDRAMNodes: true, ManageMemdevs: ptr(true)}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ignoreMemdevs":["mem0"],"ignoreRegions":["region0"],"ignoreNUMANodes":[1],"ignoreDRAM":true,"manageMemdevs":true}`
	if string(data) != want {
		t.Errorf("configuration %s, expected %s", data, want)
	}
	const parseErr = "parse: error unmarshaling JSON: while decoding JSON: json: "
	const memdevErr = "not a name (mem0, cxl/mem0), major:minor (251:0) or serial in decimal or 0x hex"
	for _, tc := range []struct {
		config string
		want   string // the configuration as JSON, or the start of the error
	}{
		{"", "{}"},
		{"{}", "{}"},
		{`{"ignoreMemdevs": ["mem0"], "manageMemdevs": false}`, `{"ignoreMemdevs":["mem0"],"manageMemdevs":false}`},
		{"ignoreMemdevs: [mem0, mem1]\nignoreRegions: [region0]\nignoreNUMANodes: [1, 3]\nignoreDRAM: true\nmanageMemdevs: true",
			`{"ignoreMemdevs":["mem0","mem1"],"ignoreRegions":["region0"],"ignoreNUMANodes":[1,3],"ignoreDRAM":true,"manageMemdevs":true}`},
		{`sharedMemdevs: ["0xc100e2e1", "mem1"]`, `{"sharedMemdevs":["0xc100e2e1","mem1"]}`},
		{"ignoreDevice: [mem0]", parseErr + `unknown field "ignoreDevice"`},
		{`sharedDevice: ["0xc100e2e1"]`, parseErr + `unknown field "sharedDevice"`},
		{`sharedMemdevs: "0xc100e2e1"`, parseErr + "cannot unmarshal string into "},
		{"sharedMemdevs: [{serial: 1}]", parseErr + "cannot unmarshal object into "},
		{`sharedMemdevs: ["c100e2e1"]`, `sharedMemdevs: invalid memdev "c100e2e1": ` + memdevErr},
		{`ignoreMemdevs: ["mem0", "cxl/c100e2e1"]`, `ignoreMemdevs: invalid memdev "cxl/c100e2e1": ` + memdevErr},
	} {
		cfg, err := newDriverConfigFromString(tc.config)
		got := ""
		if err != nil {
			got = err.Error()
		} else {
			data, err := json.Marshal(cfg)
			noErr(t, err)
			got = string(data)
		}
		// the json package words the rest of its errors
		if got != tc.want && (err == nil || !strings.HasPrefix(got, tc.want)) {
			t.Errorf("%q: %s, expected %s", tc.config, got, tc.want)
		}
	}
	if !(&DriverConfig{}).manageMemdevs() || !(&DriverConfig{ManageMemdevs: ptr(true)}).manageMemdevs() ||
		(&DriverConfig{ManageMemdevs: ptr(false)}).manageMemdevs() {
		t.Error("manageMemdevs is not true unless set false")
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	noErr(t, os.WriteFile(file, []byte("ignoreMemdevs: [mem1]"), 0o600))
	if s, err := readConfig(file); err != nil || s != "ignoreMemdevs: [mem1]" {
		t.Errorf("the configuration file: %q, %v", s, err)
	}
	if s, err := readConfig(""); err != nil || s != "" {
		t.Errorf("no configuration: %q, %v", s, err)
	}
	_, err = readConfig(file + ".gone")
	wantErr(t, err, `configuration: open `+file+`.gone: `)
}

func TestDriverConfigDefaults(t *testing.T) {
	for _, tc := range []struct {
		config string
		want   string // fencing and steering policy, or the error
	}{
		{"{}", "allowed-mems none"},
		{"defaults: {}", "allowed-mems none"},
		{"defaults: {container: {}}", "allowed-mems none"},
		{"defaults: {container: {fencing: allowed-mems}}", "allowed-mems none"},
		{"defaults:\n  container:\n    fencing: none\n    steering: {policy: auto}", "none auto"},
		{`{"defaults": {"container": {"steering": {"policy": "first-dram", "maxStep": "1Gi"}}}}`, "allowed-mems first-dram"},
		{"defaults: {container: {fencing: hard}}", `defaults.container.fencing: invalid value "hard" (allowed-mems or none)`},
		{"defaults: {container: {steering: {policy: bogus}}}", `defaults.container.steering.policy: invalid policy "bogus" (none, auto, first-dram, first-cxl, start-interleaved, end-interleaved or waypoints)`},
		{"defaults: {container: {steering: {maxStep: 1Gi}}}", "defaults.container.steering.maxStep needs a policy other than none"},
		{"defaults: {container: {Fencing: none}}", `defaults: json: unknown field "container.Fencing"`},
		{"defaults: {device: {mode: dax}}", `defaults: json: unknown field "device"`},
		{"containerMemory: {default: none}", "parse: "},
	} {
		cfg, err := newDriverConfigFromString(tc.config)
		got := ""
		if err != nil {
			got = err.Error()
		} else {
			got = cfg.fencing() + " " + cfg.steeringPolicy()
		}
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: %q, expected %q", tc.config, got, tc.want)
		}
	}
	cfg, err := newDriverConfigFromString("defaults: {container: {steering: {policy: auto, minStep: 64Mi}}}")
	noErr(t, err)
	if got := cfg.containerDefaults(); got.Steering == nil || got.Steering.Policy != config.PolicyAuto || got.Steering.MinStep != "64Mi" || got.Fencing != "" {
		t.Errorf("container defaults %+v", got)
	}
	if got := (&DriverConfig{}).containerDefaults(); got.Steering != nil || got.Fencing != "" {
		t.Errorf("container defaults of no defaults %+v", got)
	}
	data, err := json.Marshal(cfg)
	if err != nil || string(data) != `{"defaults":{"container":{"steering":{"minStep":"64Mi","policy":"auto"}}}}` {
		t.Errorf("configuration %s %v", data, err)
	}
}

// fsTypes defaults to erofs, is returned as a copy, and its invalid or
// misspelt settings are refused.
func TestDriverConfigFSTypes(t *testing.T) {
	cfg, err := newDriverConfigFromString("")
	noErr(t, err)
	if got := cfg.fsTypes(); !slices.Equal(got, []string{"erofs"}) {
		t.Fatalf("default fsTypes %v", got)
	}
	cfg.fsTypes()[0] = "x"
	if cfg.fsTypes()[0] != "erofs" || defaultFSTypes[0] != "erofs" {
		t.Fatal("fsTypes() is not a copy")
	}
	cfg, err = newDriverConfigFromString("fsTypes: [erofs, f2fs]")
	noErr(t, err)
	if got := cfg.fsTypes(); !slices.Equal(got, []string{"erofs", "f2fs"}) {
		t.Fatalf("fsTypes %v", got)
	}
	cfg, err = newDriverConfigFromString("fsTypes: []")
	noErr(t, err)
	if got := cfg.fsTypes(); got == nil || len(got) != 0 {
		t.Fatalf("empty fsTypes %#v", got)
	}
	_, err = newDriverConfigFromString("fsTypes: [ext4, Ext4]")
	wantErr(t, err, `fsTypes: invalid filesystem type "Ext4" (lower-case letters, digits, "_" and "-")`)
	_, err = newDriverConfigFromString("fsType: [ext4]")
	wantErr(t, err, `unknown field "fsType"`)
}

// The state store is in the plugin data directory of cxl.nri.io.
func TestOpenDriverStore(t *testing.T) {
	dir := t.TempDir()
	config := &pluginConfig{Flags: &flags{}, KubeletPluginDir: dir}
	_, closeStore, err := openDriverStore(context.Background(), config, config.Flags, "b0")
	noErr(t, err)
	closeStore()
	if _, err := os.Stat(filepath.Join(dir, store.FileName)); err != nil {
		t.Fatal(err)
	}
}

// A Prepare of either driver waits for the first NRI Synchronize (also of a
// prepared claim), failing before any device after nriPrepareWait. Without
// an NRI plugin it does not wait.
func TestPrepareWaitsForNRI(t *testing.T) {
	old := nriPrepareWait
	t.Cleanup(func() { nriPrepareWait = old })
	const why = "NRI not synchronized yet: context deadline exceeded: the containers of the claims would be created before the NRI plugin sees them; kubelet retries the Prepare"
	for _, tc := range []struct {
		driver string
		claim  *resourcev1.ResourceClaim
	}{
		{device.DriverName, localClaim("u1", 16*mib)},
		{device.PoolDriverName, poolClaim("u1")},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			e := newDriverTestEnv(t)
			e.start()
			// plugin returns the driver of the claim
			plugin := func() *dra.Plugin {
				if tc.driver == device.DriverName {
					return e.d.local
				}
				return e.d.pool
			}
			prepare := func(p *dra.Plugin) kubeletplugin.PrepareResult {
				res, err := p.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{tc.claim})
				if err != nil {
					return kubeletplugin.PrepareResult{Err: err}
				}
				return res[tc.claim.UID]
			}
			// waits runs a Prepare in the background and checks that it
			// waits until the NRI plugin synchronizes, then succeeds.
			waits := func() {
				t.Helper()
				nriPrepareWait = time.Minute
				p := plugin()
				done := make(chan kubeletplugin.PrepareResult, 1)
				go func() { done <- prepare(p) }()
				select {
				case res := <-done:
					t.Fatalf("did not wait for NRI: %+v", res)
				case <-time.After(50 * time.Millisecond):
				}
				e.synchronize()
				select {
				case res := <-done:
					noErr(t, res.Err)
				case <-time.After(5 * time.Second):
					t.Fatal("still waiting after Synchronize")
				}
				e.check("prepared u1")
			}

			nriPrepareWait = 20 * time.Millisecond
			cmds := e.commands()
			wantErr(t, prepare(plugin()).Err, why)
			if prep := e.records().Preparation(tc.driver, "u1"); prep != nil || e.commands() != cmds {
				t.Fatalf("preparation %+v, commands:\n%s", prep, strings.TrimPrefix(e.commands(), cmds))
			}
			waits()
			// prepared already: it waits too
			e.restart()
			waits()
			// no NRI plugin
			e.runtime.onStart = func() error { return errors.New("no runtime") }
			e.restart()
			noErr(t, prepare(plugin()).Err)
			e.mustUnprepare(plugin(), "u1")
			e.wantClean()
		})
	}
}

// The lease broker finds the published local memdev of a serial, else the
// device of the last list of the pool ResourceSlices, none without the
// pool driver.
func TestPublishedDevice(t *testing.T) {
	e := newDriverTestEnv(t)
	e.start()
	node := &dra.Node{Publisher: e.d.publisher, Pool: e.d.pool, Watcher: e.d.watcher}
	find := publishedDevice(node)
	for _, tc := range []struct {
		serial       uint64
		driver, name string
	}{
		{localSerial, device.DriverName, dra.MemdevDeviceName(localSerial)},
		{poolSerial, device.PoolDriverName, "pooled0"},
		{0x1234, "", ""},
	} {
		driver, d := find(tc.serial)
		if driver != tc.driver || (d == nil) != (tc.name == "") || (d != nil && d.Name != tc.name) {
			t.Errorf("serial 0x%x: driver %q, device %+v", tc.serial, driver, d)
		}
	}
	node.Pool = nil
	if driver, d := find(poolSerial); d != nil {
		t.Errorf("without the pool driver: driver %q, device %+v", driver, d)
	}
}

// everyPeriod runs its pass at every period until it is stopped.
func TestEveryPeriod(t *testing.T) {
	stop := make(chan struct{})
	passes := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		everyPeriod(time.Millisecond, stop, func() { passes <- struct{}{} })
		close(done)
	}()
	for range 3 {
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatal("no pass")
		}
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("not stopped")
	}
}

// fakeEvents are kernel uevents for tests.
type fakeEvents struct {
	kernel.Changes
	started, stopped bool
	stable           chan struct{}
}

func (f *fakeEvents) Start()                  { f.started = true }
func (f *fakeEvents) Stop()                   { f.stopped = true }
func (f *fakeEvents) Stable() <-chan struct{} { return f.stable }

// One watch of the uevents of the kernel objects wakes the waits of the
// engine's kernel and gives the stable signal of the rescans after
// udevStableDuration; every stable signal rescans once, until the driver
// stops. Without uevents the waits only poll and nothing rescans. The
// removed keys ignoreNewNodes and udevStableDuration are refused.
func TestWakeOnUevents(t *testing.T) {
	old := newKernelEvents
	t.Cleanup(func() { newKernelEvents = old })
	ev := &fakeEvents{}
	var stable time.Duration
	newKernelEvents = func(d time.Duration) (kernelEvents, error) { stable = d; return ev, nil }
	k := &kernel.Kernel{}
	got, stop := wakeOnUevents(k)
	if !ev.started || stable != udevStableDuration || got != kernelEvents(ev) || k.Events != kernel.Events(ev) {
		t.Fatalf("events %+v, stable %v, kernel events %v", ev, stable, k.Events)
	}
	stop()
	if !ev.stopped {
		t.Fatal("not stopped")
	}
	for _, key := range []string{"ignoreNewNodes: true", "udevStableDuration: 1s", "ignoreNewMemoryDevices: true"} {
		if _, err := newDriverConfigFromString(key); err == nil {
			t.Errorf("%s accepted", key)
		}
	}
	newKernelEvents = func(time.Duration) (kernelEvents, error) { return nil, errors.New("no netlink") }
	k = &kernel.Kernel{}
	logs := klogtest.Capture(t, 3)
	got, stop = wakeOnUevents(k)
	stop()
	if got != nil || k.Events != nil {
		t.Fatalf("kernel events %v without uevents", k.Events)
	}
	// an error, not a warning: nothing rescans the ResourceSlice
	if out := logs(); !strings.HasPrefix(out, "E") || !strings.Contains(out, "the ResourceSlice is not rescanned on uevents") {
		t.Fatalf("no error line in\n%s", out)
	}

	stopRescans, signals, rescans := make(chan struct{}), make(chan struct{}), make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rescanLoop(stopRescans, signals, func() { rescans <- struct{}{} })
	}()
	signals <- struct{}{}
	select {
	case <-rescans:
	case <-time.After(5 * time.Second):
		t.Fatal("no rescan after a stable signal")
	}
	close(stopRescans)
	<-done
	if len(rescans) != 0 {
		t.Fatalf("%d rescans more", len(rescans))
	}
	if !strings.Contains(logs(), "kernel uevents: sysfs stabilized, signaling rescan") {
		t.Fatalf("no rescan line in\n%s", logs())
	}
	// without uevents nothing rescans
	d := &driver{stop: make(chan struct{})}
	d.startRescans(context.Background())
}
