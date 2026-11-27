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

package dra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// A PREPARED commit whose write the disk stalls beyond commitTimeout is
// waited for and decides the record: the Prepare returns the device of the
// claim, the claim is PREPARED with its grant, and a Prepare again returns
// the same result.
func TestPreparedCommitWaitsForItsWrite(t *testing.T) {
	defer func(d time.Duration) { commitTimeout = d }(commitTimeout)
	commitTimeout = 200 * time.Millisecond
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	claim := localClaim("u1", localDev, envBlockSize, cfgDax)
	// the first write after the CDI spec names the claim's device is the
	// write of PREPARED
	inSpec := func() bool {
		data, _ := os.ReadFile(filepath.Join(e.cdiDir, device.ClaimsCDISpecName))
		return bytes.Contains(data, []byte("claim-u1-0"))
	}
	var stall func() error
	stall = func() error {
		if !inSpec() {
			e.mu.Lock()
			e.onWrite = stall
			e.mu.Unlock()
			return nil
		}
		time.Sleep(3 * commitTimeout)
		return nil
	}
	e.mu.Lock()
	e.onWrite = stall
	e.mu.Unlock()
	res := e.prepare(e.local, claim)
	if res.Err != nil || len(res.Devices) != 1 {
		t.Fatalf("Prepare with a stalled write: %v, devices %+v", res.Err, res.Devices)
	}
	e.wantState(e.local, "u1", state.PreparationPrepared)
	if prep := e.preparation(e.local, "u1"); len(grantRefs(e.records(), prep)) != 1 {
		t.Fatalf("preparation %+v, grants %+v", prep, e.records().Grants)
	}
	if again := e.prepare(e.local, claim); again.Err != nil || !reflect.DeepEqual(again.Devices, res.Devices) {
		t.Fatalf("Prepare again: %v, devices %+v", again.Err, again.Devices)
	}
	e.mustUnprepare(e.local, "u1")
	e.wantClean()
}

// A Prepare waits for the first NRI Synchronize: an error of the wait
// fails every claim of the call, with a warning, before any device or
// record.
func TestWaitNRI(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, poolSpec(serialPoolX))
	errWait := errors.New("NRI has not synchronized")
	waited := 0
	wait := func(ctx context.Context) error {
		waited++
		if ctx == nil {
			return errors.New("no context")
		}
		return errWait
	}
	logs := klogCapture(t)
	for _, p := range []*Plugin{e.local, e.pool} {
		e.mark()
		p.SetWaitNRI(wait)
		claims := []*resourcev1.ResourceClaim{localClaim("u1", localDev, envBlockSize), localClaim("u2", device.DRAMDeviceName, 64*mib)}
		if p == e.pool {
			claims = []*resourcev1.ResourceClaim{poolClaim("u1", "pooled0", poolExclusiveData), poolClaim("u2", "pooled0", poolExclusiveData)}
		}
		res, err := p.PrepareResourceClaims(context.Background(), claims)
		mustOK(t, err)
		for _, c := range claims {
			if !errors.Is(res[c.UID].Err, errWait) || len(res[c.UID].Devices) != 0 {
				t.Fatalf("%s: claim %s: %+v", p.Name(), c.UID, res[c.UID])
			}
			e.wantState(p, string(c.UID), state.PreparationNone)
		}
		wantLogText(t, logs(), fmt.Sprintf("%s: preparing 2 claims: %v", p.Name(), errWait))
		e.wantNoCommands("a Prepare before NRI")
		p.SetWaitNRI(func(context.Context) error { return nil })
		e.mustPrepare(p, claims[0])
		p.SetWaitNRI(nil)
		e.mustUnprepare(p, "u1")
	}
	if waited != 2 {
		t.Fatalf("waited %d times", waited)
	}
	var none *Plugin
	none.SetWaitNRI(wait)
	none.Stop()
	none.Recovered(nil)
	e.wantClean()
}

// HandleError logs an error of the kubelet plugin helper in one line with
// the driver, and tells a fatal one to Shutdown; the drivers report no
// device health.
func TestHandleErrorAndHealth(t *testing.T) {
	e := newDriverEnv(t, nil)
	var fatal []error
	for _, p := range []*Plugin{e.local, e.pool} {
		p.shutdown = func(err error) { fatal = append(fatal, err) }
	}
	logs := klogtest.Capture(t, 0)
	ctx := context.Background()
	e.pool.HandleError(ctx, fmt.Errorf("slice: %w", kubeletplugin.ErrRecoverable), "publishing")
	e.local.HandleError(ctx, errors.New("lost"), "registrar gRPC server failed")
	log := logs()
	wantLogText(t, log, `"publishing" err="slice: recoverable error" driver="cxl-pool.nri.io" fatal=false`)
	wantLogText(t, log, `"registrar gRPC server failed" err="lost" driver="cxl.nri.io" fatal=true`)
	if n := logMatches(log, "slice: recoverable error") + logMatches(log, `err="lost"`); n != 2 {
		t.Fatalf("%d lines of the errors, expected 2:\n%s", n, log)
	}
	if fmt.Sprint(fatal) != "[cxl.nri.io: registrar gRPC server failed: lost]" {
		t.Fatalf("shutdown of %v", fatal)
	}
	for _, p := range []*Plugin{e.local, e.pool} {
		if err := p.WatchHealthStatus(ctx, make(chan kubeletplugin.DeviceHealthReport)); !errors.Is(err, kubeletplugin.ErrHealthNotSupported) {
			t.Fatalf("%s: %v", p.Name(), err)
		}
	}
}

// New refuses a plugin without the store or the engine, one of rules that
// are not those of localRules or poolRules, and one that publishes without
// the publisher; newPublisher refuses a publisher without the store, the
// scan or the node name.
func TestNew(t *testing.T) {
	e := newDriverEnv(t, nil)
	if _, err := New(Options{Rules: poolRules(device.PoolDriverName, ""), Store: e.st}); err == nil {
		t.Fatal("a plugin without the engine")
	}
	_, err := New(Options{Rules: SourceRules{Name: "zero.nri.io"}, Store: e.st, Engine: e.eng})
	wantErrText(t, err, `zero.nri.io: rules of source "": not those of localRules or poolRules`)
	_, err = New(Options{Rules: localRules(""), Store: e.st, Engine: e.eng})
	wantErrText(t, err, "cxl.nri.io: the plugin publishes and needs the publisher")
	for _, opts := range []publishOptions{{Scan: pubNoScan, NodeName: pubNode}, {Store: e.st, NodeName: pubNode}, {Store: e.st, Scan: pubNoScan}} {
		_, err = newPublisher(opts)
		wantErrText(t, err, "the publisher needs the state store, the scan of the devices and the node name")
	}
	p, err := New(Options{Rules: poolRules("other.nri.io", ""), Store: e.st, Engine: e.eng})
	mustOK(t, err)
	if p.Name() != "other.nri.io" || p.Engine() != e.eng || p.roots != (roots{sys: "/sys", dev: "/dev"}) || p.Records() == nil || p.Rules().Source != state.SourcePool {
		t.Fatalf("plugin %+v", p)
	}
}
