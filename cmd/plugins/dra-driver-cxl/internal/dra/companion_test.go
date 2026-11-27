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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// companionOf returns the companion claim c1 of lease l1 of the driver of
// the plugin on its device of the env: a slice of 64 MiB of the unshared
// local memdev, or the pool device pooled0, with the configs.
func (e *driverEnv) companionOf(p *Plugin, configs ...string) *resourcev1.ResourceClaim {
	if p == e.local {
		return companion(localClaim("c1", localDev, 64*mib, configs...), "l1")
	}
	return companion(poolClaim("c1", "pooled0", poolExclusiveData, configs...), "l1")
}

// lendEnv returns a node with the unshared local memdev and the pool device
// pooled0, without lending.
func lendEnv(t *testing.T) *driverEnv {
	return newDriverEnv(t, []fake.MemdevSpec{localSpec()}, poolSpec(serialPoolX))
}

// prefix returns the prefix of the errors of a Prepare of the companion
// claim c1 by the plugin.
func (e *driverEnv) prefix(p *Plugin) string {
	if p == e.local {
		return "claim demo/cxl-lease-l1: "
	}
	return "cxl-pool.nri.io: claim demo/cxl-lease-l1: "
}

// The companion gate of both drivers before any device is touched: lending
// disabled, NRI not connected, the broker's refusal; cxl.nri.io lends
// local memdevs only. A claim without the label is no companion.
func TestCompanionGate(t *testing.T) {
	e := lendEnv(t)
	if leaseOf(e.companionOf(e.local)) != "l1" || leaseOf(localClaim("u1", localDev, 0)) != "" {
		t.Fatal("leaseOf")
	}
	for _, p := range []*Plugin{e.local, e.pool} {
		e.t = t
		e.mark()
		wantErrText(t, e.prepare(p, e.companionOf(p)).Err, e.prefix(p)+"lease l1: lending to running containers is disabled (--leases=false)")
		hooks := &fakeHooks{e: e}
		e.lend(nil, hooks)
		res := e.prepare(p, e.companionOf(p))
		if !errors.Is(res.Err, lend.ErrLendingUnavailable) {
			t.Fatalf("%s: without NRI: %v", p.Name(), res.Err)
		}
		wantErrText(t, res.Err, e.prefix(p)+"lease l1: lending unavailable: NRI not connected")
		e.lend(&fakeNodes{e: e}, hooks)
		hooks.setLendingErr(errors.New("claim demo/cxl-lease-l1: not the companion of lease l1"))
		wantErrText(t, e.prepare(p, e.companionOf(p)).Err, e.prefix(p)+"claim demo/cxl-lease-l1: not the companion of lease l1")
		hooks.setLendingErr(nil)
		e.wantNoCommands(p.Name() + ": a refused companion")
		e.wantState(p, "c1", state.PreparationNone)
		e.wantEvents(p.Name(), "lending l1 cxl-lease-l1")
		e.lent, e.nodes, e.hooks = false, nil, nil
		e.local.Lender().Set(nil, nil)
		e.pool.Lender().Set(nil, nil)
	}
	e.lending()
	// cxl.nri.io: memdevs of the node only
	dram := companion(localClaim("c1", device.DRAMDeviceName, 64*mib), "l1")
	wantErrText(t, e.prepare(e.local, dram).Err, "claim demo/cxl-lease-l1: the companion claim of lease l1 needs memdev devices only")
	mixed := addLocalResult(companion(localClaim("c1", localDev, 64*mib), "l1"), "dram", device.DRAMDeviceName, 64*mib)
	wantErrText(t, e.prepare(e.local, mixed).Err, "needs memdev devices only")
	none := companion(poolClaim("c1", "pooled0", poolExclusiveData), "l1")
	none.Status.Allocation.Devices.Results = none.Status.Allocation.Devices.Results[:1]
	wantErrText(t, e.prepare(e.local, none).Err, "needs memdev devices only")
	e.wantNoCommands("refused companions")
	e.wantEvents("memdevs only")
}

// Without NRI a lend of numa devices fails and one of dax devices passes:
// the broker passes them as an fd; a revoke only warns. A nil lender, the
// lender of a plugin that is not there, is disabled and calls no hook.
func TestCompanionLenderWithoutNRI(t *testing.T) {
	e := lendEnv(t)
	hooks := &fakeHooks{e: e}
	e.lend(nil, hooks)
	ctx := context.Background()
	c := e.local.Lender()
	rec := &lend.Companion{Lease: "l1", Pod: state.PodRef{Namespace: envNS, Name: envPod, UID: envPodUID}, Container: envCtr}
	numa := state.DeviceInfo{DeviceType: device.DeviceTypeMemdev, Mode: api.ModeNUMA, NUMANodes: []int{3}}
	if err := c.Lend(ctx, rec, []state.DeviceInfo{numa}); !errors.Is(err, lend.ErrLendingUnavailable) || len(rec.NUMANodes) != 0 {
		t.Fatalf("numa lend: %v, nodes %v", err, rec.NUMANodes)
	}
	dax := state.DeviceInfo{DeviceType: device.DeviceTypeMemdev, Mode: api.ModeDax, DaxDevice: "/dev/dax0.1"}
	if err := c.Lend(ctx, rec, []state.DeviceInfo{dax}); err != nil || len(rec.NUMANodes) != 0 {
		t.Fatalf("dax lend: %v, nodes %v", err, rec.NUMANodes)
	}
	logs := klogtest.Capture(t, 0)
	rec.NUMANodes = []int{3}
	mustOK(t, c.Revoke(ctx, rec, "demo/cxl-lease-l1"))
	line := "cxl.nri.io: claim demo/cxl-lease-l1: lease l1: lending unavailable: NRI not connected, cannot revoke nodes [3] from demo/requester/app"
	if got := logs(); logMatches(got, line) != 1 {
		t.Fatalf("no %q in\n%s", line, got)
	}

	var n *lender
	n.Set(&fakeNodes{e: e}, hooks)
	n.Prepared("l1", state.SourceLocal, nil)
	n.Failed("l1", errors.New("lend failed"))
	n.Releasing("l1")
	n.PreparedAgain(rec, e.companionOf(e.local), state.SourceLocal, nil)
	_, err := n.Check("l1", e.companionOf(e.local))
	wantErrText(t, err, "lease l1: lending to running containers is disabled (--leases=false)")
	e.wantEvents("lender without NRI and nil lender")
}

// specClaim returns the companion claim c1 of the unshared local memdev with
// a spec.
func specClaim() *resourcev1.ResourceClaim {
	c := companion(localClaim("c1", localDev, 64*mib), "l1")
	c.Spec.Devices.Requests = []resourcev1.DeviceRequest{{Name: "mem", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "cxl-local"}}}
	return c
}

// The spec hash of a companion claim is compared only when the hooks tell
// a hash (SpecHasher) and the lease waits for its first Prepare; a
// mismatch refuses the claim before OnLending and any device; a Prepare
// again does not compare.
func TestCompanionSpecHash(t *testing.T) {
	good := lend.SpecHash(&specClaim().Spec)
	bad := strings.Repeat("0", 64)
	for _, tc := range []struct {
		name    string
		hasher  bool
		hash    string
		first   bool
		refused bool
	}{
		{"no hasher", false, "", false, false},
		{"same hash", true, good, true, false},
		{"another hash", true, bad, true, true},
		{"another hash, not the first Prepare", true, bad, false, false},
		{"no hash", true, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := lendEnv(t)
			hooks := &fakeHooks{e: e}
			var lh lend.LeaseHooks = hooks
			if tc.hasher {
				lh = &fakeSpecHooks{fakeHooks: hooks, hash: tc.hash, first: tc.first}
			}
			e.lend(&fakeNodes{e: e}, lh)
			e.mark()
			res := e.prepare(e.local, specClaim())
			var want []string
			if tc.hasher {
				want = append(want, "spec hash l1")
			}
			if tc.refused {
				wantErrText(t, res.Err, "claim demo/cxl-lease-l1: claim demo/cxl-lease-l1: not the companion of lease l1: its spec is not the one made for the lease")
				e.wantNoCommands("a refused companion")
				e.wantEvents("refused", want...)
				return
			}
			mustOK(t, res.Err)
			e.wantEvents("prepared", append(want, "lending l1 cxl-lease-l1", "lend pod-uid-1/app 2 l1", "prepared l1")...)
			if tc.hasher {
				// a Prepare again: the broker says first=false, a hash of
				// another spec
				spec := lh.(*fakeSpecHooks)
				spec.mu.Lock()
				spec.hash, spec.first = bad, false
				spec.mu.Unlock()
				again := e.mustPrepare(e.local, specClaim())
				if !reflect.DeepEqual(again, res) {
					t.Fatalf("again %+v", again)
				}
				e.wantEvents("again", "lending l1 cxl-lease-l1", "prepared l1")
			}
			e.mustUnprepare(e.local, "c1")
			e.wantClean()
		})
	}
}

// A companion claim of a numa slice of either driver: no CDI device, its
// node recorded before it is lent, the broker told the lent devices; a
// Prepare again announces it again, unless the broker no longer has the
// lease; its Unprepare revokes the node with the memory online, then
// releases it.
func TestCompanionLendNUMA(t *testing.T) {
	for _, local := range []bool{true, false} {
		e := lendEnv(t)
		p := e.pool
		pool, source := envPool, state.SourcePool
		want := state.DeviceInfo{RequestName: "mem", DeviceName: "pooled0", DeviceType: device.DeviceTypeMemdev, Mode: api.ModeNUMA,
			Serial: serialPoolX, TotalBytes: 256 << 20, ConsumedBytes: 256 << 20, NUMANodes: []int{2}}
		if local {
			p, pool, source = e.local, envNode, state.SourceLocal
			want.DeviceName, want.Serial, want.TotalBytes, want.ConsumedBytes = localDev, serialLocal, uint64(envBlockSize), envBlockSize
		}
		t.Run(p.Name(), func(t *testing.T) {
			e.t = t
			nodes, hooks := e.lending()
			var atLend []string
			nodes.onLend = func(node int) {
				g := e.records().Grant(state.GrantRef{Claim: "c1"})
				atLend = append(atLend, g.Got.Lend.Lease, string(rune('0'+state.NUMANodeOf(g.Got.Lend.NUMANode))))
			}
			logs := klogCapture(t)
			res := e.mustPrepare(p, e.companionOf(p))
			wantResult(t, res, resultDevice("mem", pool, want.DeviceName))
			if !slices.Equal(atLend, []string{"l1", "2"}) {
				t.Fatalf("the grant's lend at the lend: %v", atLend)
			}
			e.wantEvents("prepared", "lending l1 cxl-lease-l1", "lend pod-uid-1/app 2 l1", "prepared l1")
			if got, src := hooks.lentDevices(); len(got) != 1 || src != source || got[0].Serial != want.Serial || got[0].TotalBytes != want.TotalBytes ||
				got[0].Mode != want.Mode || !slices.Equal(got[0].NUMANodes, want.NUMANodes) || got[0].DeviceName != want.DeviceName ||
				got[0].RequestName != want.RequestName || !reflect.DeepEqual(got[0], p.ClaimInfo("c1").Devices[0]) {
				t.Fatalf("lent devices %+v of %q, expected %+v of %q", got, src, want, source)
			}
			if e.cdiSpec(p.Rules().CDISpecName()) != nil {
				t.Fatal("a CDI device of a companion claim")
			}
			if !local {
				wantLogText(t, logs(), "cxl-pool.nri.io: claim demo/cxl-lease-l1: companion of lease l1 of demo/requester/app prepared (NUMA nodes [2], binds [])")
			}
			if info := p.ClaimInfo("c1"); info == nil || info.Lend == nil || info.Lend.Lease != "l1" || !slices.Equal(info.Lend.NUMANodes, []int{2}) ||
				info.Lend.Pod.UID != envPodUID || info.Lend.Container != envCtr {
				t.Fatalf("claim info %+v", info)
			}
			if got := NUMANodes(e.records(), p.Name()); !slices.Equal(got, []int{2}) {
				t.Fatalf("NUMA nodes %v", got)
			}
			e.mark()
			again := e.mustPrepare(p, e.companionOf(p))
			if !reflect.DeepEqual(again, res) {
				t.Fatalf("again %+v", again)
			}
			e.wantNoCommands("a Prepare again")
			e.wantEvents("a Prepare again", "lending l1 cxl-lease-l1", "prepared l1")
			hooks.mu.Lock()
			hooks.lendingErr = errors.New("unknown lease")
			hooks.mu.Unlock()
			if again := e.mustPrepare(p, e.companionOf(p)); !reflect.DeepEqual(again, res) {
				t.Fatalf("again, the lease unknown %+v", again)
			}
			e.wantEvents("a Prepare again, the lease unknown", "lending l1 cxl-lease-l1")
			hooks.mu.Lock()
			hooks.lendingErr = nil
			hooks.mu.Unlock()
			var atRevoke []string
			nodes.onRevoke = func(int) {
				online := e.onlineBlocks() > 0
				atRevoke = append(atRevoke, string(e.stateOf(p, "c1")), map[bool]string{true: "online", false: "offline"}[online])
			}
			e.mustUnprepare(p, "c1")
			e.wantEvents("unprepared", "releasing l1", "revoke pod-uid-1/app 2 l1")
			if !slices.Equal(atRevoke, []string{"UNPREPARING", "online"}) || len(nodes.lentNodes()) != 0 {
				t.Fatalf("at the revoke: %v, lent %v", atRevoke, nodes.lentNodes())
			}
			e.wantClean()
		})
	}
}

// A Prepare that lends blocks no reader: the readers of the prepared
// claims and the resources of the publisher do not wait for the lend.
func TestCompanionLendBlocksNoReader(t *testing.T) {
	e := lendEnv(t)
	nodes, _ := e.lending()
	e.mustPrepare(e.local, localClaim("u0", localDev, envBlockSize))
	e.wantEvents("prepared u0")
	entered, release := make(chan struct{}), make(chan struct{})
	nodes.onLend = func(int) {
		close(entered)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		claim := e.companionOf(e.local)
		res, err := e.local.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
		if err == nil {
			err = res[claim.UID].Err
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the Prepare did not lend")
	}
	read := make(chan bool, 1)
	go func() {
		_, _, ok := e.local.PreparedResult("u0", 0)
		_ = e.pub.Resources()
		read <- ok && e.local.ClaimInfo("u0") != nil
	}()
	select {
	case ok := <-read:
		if !ok {
			t.Error("no prepared result or claim info of u0")
		}
	case <-time.After(10 * time.Second):
		t.Error("the readers waited for the lend")
	}
	close(release)
	select {
	case err := <-done:
		mustOK(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the Prepare of the companion claim did not end")
	}
	e.check("prepared c1")
	e.wantEvents("prepared c1", "lending l1 cxl-lease-l1", "lend pod-uid-1/app 2 l1", "prepared l1")
	e.mustUnprepare(e.local, "c1")
	e.mustUnprepare(e.local, "u0")
	e.wantClean()
}

// A companion claim of a dax device of either driver, or of a read-only
// share of a shared local memdev: the broker gets the dax device, no node
// is lent or revoked, no CDI device.
func TestCompanionLendDax(t *testing.T) {
	for _, tc := range []struct {
		name          string
		local, shared bool
	}{{"local", true, false}, {"pool", false, false}, {"shared", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var e *driverEnv
			var claim *resourcev1.ResourceClaim
			if tc.shared {
				e = newDriverEnv(t, []fake.MemdevSpec{{Serial: 0xc100e2e0}, sharedOne()})
				e.setShared(serialLocal)
				claim = companion(localClaim("c1", localDev, -1, cfgDaxRO), "l1")
			} else {
				e = lendEnv(t)
				claim = e.companionOf(e.pluginOf(tc.local), cfgDax)
			}
			p := e.pluginOf(tc.local)
			_, hooks := e.lending()
			e.mustPrepare(p, claim)
			e.wantEvents("prepared", "lending l1 cxl-lease-l1", "prepared l1")
			got, _ := hooks.lentDevices()
			if len(got) != 1 || got[0].Mode != api.ModeDax || got[0].DaxDevice == "" || len(got[0].NUMANodes) != 0 ||
				got[0].Shared != tc.shared || got[0].ReadOnly != tc.shared {
				t.Fatalf("lent devices %+v", got)
			}
			if e.cdiSpec(p.Rules().CDISpecName()) != nil {
				t.Fatal("a CDI device of a companion claim")
			}
			e.mustUnprepare(p, "c1")
			e.wantEvents("unprepared", "releasing l1")
			if !tc.shared {
				e.wantClean()
			}
		})
	}
}

// pluginOf returns the plugin of cxl.nri.io (local) or of the pool
// driver.
func (e *driverEnv) pluginOf(local bool) *Plugin {
	if local {
		return e.local
	}
	return e.pool
}

// onlineBlocks returns the number of the memory blocks of the kernel that
// are online.
func (e *driverEnv) onlineBlocks() int {
	snap, err := e.k.Kernel().Observe(context.Background())
	mustOK(e.t, err)
	n := 0
	for _, b := range snap.Blocks {
		if b.State == kernel.BlockOnline {
			n++
		}
	}
	return n
}

// A lend that fails rolls the companion claim back: the node revoked with
// the memory online, then the broker told, then the memory released.
func TestCompanionLendFails(t *testing.T) {
	for _, local := range []bool{true, false} {
		e := lendEnv(t)
		p := e.pool
		if local {
			p = e.local
		}
		t.Run(p.Name(), func(t *testing.T) {
			e.t = t
			nodes, _ := e.lending()
			nodes.setErrors(errFake, nil)
			var online []bool
			nodes.onRevoke = func(int) { online = append(online, e.onlineBlocks() > 0) }
			res := e.prepare(p, e.companionOf(p))
			lendErr := "lease l1: lending NUMA node 2 to demo/requester/app: " + errFake.Error()
			wantErrText(t, res.Err, e.prefix(p)+lendErr)
			e.wantEvents("failed", "lending l1 cxl-lease-l1", "lend pod-uid-1/app 2 l1", "revoke pod-uid-1/app 2 l1", "failed l1: "+lendErr)
			if !slices.Equal(online, []bool{true}) {
				t.Fatalf("memory online at the revokes: %v", online)
			}
			e.wantState(p, "c1", state.PreparationNone)
			e.wantClean()
		})
	}
}

// The lease bind of a lend of a device of mode fs is <FSDir>/<Name>; a lend
// without a name, or with one that is not a valid name, or of a lease
// claim without a filesystem directory is refused.
func TestLeaseBindTarget(t *testing.T) {
	for _, c := range []struct {
		name, fsDir string
		want, err   string
	}{
		{"models", "/run/cxl/fs/l1", "/run/cxl/fs/l1/models", ""},
		{"", "/run/cxl/fs/l1", "", "lease l1: the device is of mode fs, the lease is not"},
		{"..", "/run/cxl/fs/l1", "", `lease l1: invalid name ".."`},
		{"models", "", "", "lease l1: its lease claim has no filesystem directory"},
	} {
		target, err := leaseBindTarget(&lend.Companion{Lease: "l1", Name: c.name, FSDir: c.fsDir})
		got := ""
		if err != nil {
			got = err.Error()
		}
		if target != c.want || got != c.err {
			t.Errorf("name %q in %q: target %q, error %q; want %q, %q", c.name, c.fsDir, target, got, c.want, c.err)
		}
	}
}

// A lease bind that fails fails the Prepare of the companion claim of a
// device of mode fs with the error of the bind, without the device of the
// result, and the broker is told.
func TestCompanionLeaseBindFails(t *testing.T) {
	e := newDriverEnv(t, nil, poolPmemSpec(serialPoolP))
	e.formatPmem(serialPoolP, "erofs")
	_, hooks := e.lending()
	hooks.fsDir, hooks.name = filepath.Join(e.leaseFsRoot(), "l1"), "models"
	target := filepath.Join(hooks.fsDir, hooks.name)
	mustOK(t, os.MkdirAll(target, 0o755))
	mustOK(t, e.k.Kernel().Mount("tmpfs", target, "tmpfs", 0, ""))
	res := e.prepare(e.pool, companion(poolClaim("c1", "llm0", poolPmemData, cfgFsRO), "l1"))
	bindErr := "memdev 0xc1af0001: " + target + " is a tmpfs mount of tmpfs, not of " + filepath.Join(e.poolDir, "mounts", "c1af0001-direct")
	wantErrText(t, res.Err, e.prefix(e.pool)+bindErr)
	e.wantEvents("failed", "lending l1 cxl-lease-l1", "failed l1: "+bindErr)
	e.wantState(e.pool, "c1", state.PreparationNone)
	mustOK(t, e.k.Kernel().Unmount(target))
	e.wantClean()
}

// A companion claim of mode fs whose lease bind cannot be made is refused
// before any device is touched, and the broker is told: its lease claim has
// no filesystem directory, or the claim has two devices of mode fs.
func TestCompanionLeaseBindRefused(t *testing.T) {
	e := newDriverEnv(t, nil, poolPmemSpec(serialPoolP))
	e.formatPmem(serialPoolP, "erofs")
	_, hooks := e.lending()
	hooks.name = "models"
	e.mark()
	noDir := "lease l1: its lease claim has no filesystem directory"
	wantErrText(t, e.prepare(e.pool, companion(poolClaim("c1", "llm0", poolPmemData, cfgFsRO), "l1")).Err, e.prefix(e.pool)+noDir)
	e.wantEvents("no directory", "lending l1 cxl-lease-l1", "failed l1: "+noDir)
	hooks.fsDir = filepath.Join(e.leaseFsRoot(), "l1")
	two := addPoolResult(companion(poolClaim("c1", "llm0", poolPmemData, cfgFsRO), "l1"), "mem2", "llm0", poolPmemData)
	twoFs := "lease l1: more than one filesystem device"
	wantErrText(t, e.prepare(e.pool, two).Err, e.prefix(e.pool)+twoFs)
	e.wantEvents("two fs devices", "lending l1 cxl-lease-l1", "failed l1: "+twoFs)
	e.wantNoCommands("refused lease binds")
	e.wantState(e.pool, "c1", state.PreparationNone)
	if _, err := os.Stat(hooks.fsDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease directory made: %v", err)
	}
	e.wantClean()
}

// A companion claim refused after the gate of the broker: the broker is told
// that the lend failed when the MemoryConfig of the claim does not fit its
// device, not while the pool controller has not attached the device yet.
func TestCompanionRefusedLasting(t *testing.T) {
	e := lendEnv(t)
	e.lending()
	e.mark()
	c := e.companionOf(e.pool)
	c.Status.Devices = nil
	wantErrText(t, e.prepare(e.pool, c).Err, "not attached by the pool controller yet")
	e.wantEvents("not attached", "lending l1 cxl-lease-l1")
	mismatch := "device pooled0 (memdev 0xc1ee0001): MemoryConfig: device.mode fs needs a persistent device"
	wantErrText(t, e.prepare(e.pool, e.companionOf(e.pool, cfgFsRO)).Err, e.prefix(e.pool)+mismatch)
	e.wantEvents("mismatch", "lending l1 cxl-lease-l1", "failed l1: "+mismatch)
	e.wantNoCommands("refused companions")
	e.wantState(e.pool, "c1", state.PreparationNone)
	e.wantClean()
}

// The companions of another boot: a numa one refused at the lease again
// stays PREPARED_STALE and its Unprepare revokes no node, the broker is
// told of the release; the directory of the lease bind of an fs one stays
// over the reboot, its Unprepare removes it.
func TestCompanionOtherBoot(t *testing.T) {
	t.Run("numa", func(t *testing.T) {
		e := lendEnv(t)
		_, hooks := e.lending()
		e.mustPrepare(e.pool, e.companionOf(e.pool))
		e.takeEvents()
		e.reboot()
		e.wantState(e.pool, "c1", state.PreparationStale)
		hooks.setLendingErr(errors.New("lease l1 is gone"))
		wantErrText(t, e.prepare(e.pool, e.companionOf(e.pool)).Err, e.prefix(e.pool)+"lease l1 is gone")
		e.wantState(e.pool, "c1", state.PreparationStale)
		e.mustUnprepare(e.pool, "c1")
		e.wantEvents("stale", "lending l1 cxl-lease-l1", "releasing l1")
		e.wantClean()
	})
	t.Run("fs", func(t *testing.T) {
		e := newDriverEnv(t, nil, poolPmemSpec(serialPoolP))
		e.formatPmem(serialPoolP, "erofs")
		_, hooks := e.lending()
		hooks.fsDir, hooks.name = filepath.Join(e.leaseFsRoot(), "l1"), "models"
		target := filepath.Join(hooks.fsDir, hooks.name)
		e.mustPrepare(e.pool, companion(poolClaim("c1", "llm0", poolPmemData, cfgFsRO), "l1"))
		e.reboot()
		e.wantState(e.pool, "c1", state.PreparationStale)
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("lease bind directory after the reboot: %v", err)
		}
		e.mustUnprepare(e.pool, "c1")
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lease bind directory left: %v", err)
		}
		e.wantClean()
	})
}

// A revoke of a lend goes on after a node it fails to revoke: the errors
// of the nodes are joined.
func TestCompanionRevokeEveryNode(t *testing.T) {
	e := lendEnv(t)
	nodes, _ := e.lending()
	nodes.setErrors(nil, errFake)
	rec := &lend.Companion{Lease: "l1", Pod: state.PodRef{Namespace: envNS, Name: envPod, UID: envPodUID}, Container: envCtr, NUMANodes: []int{2, 3}}
	err := e.pool.Lender().Revoke(context.Background(), rec, "demo/cxl-lease-l1")
	e.wantEvents("revoke", "revoke pod-uid-1/app 2 l1", "revoke pod-uid-1/app 3 l1")
	for _, n := range []int{2, 3} {
		if want := fmt.Sprintf("lease l1: revoking NUMA node %d from demo/requester/app: %v", n, errFake); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v, expected %q in it", err, want)
		}
	}
}

// The Unprepare of a numa companion whose Prepare a stop cut after the
// lend: either driver tells the broker and revokes the node before it
// releases the memory.
func TestCompanionUnfinishedRevoke(t *testing.T) {
	for _, local := range []bool{true, false} {
		e := lendEnv(t)
		p := e.pool
		if local {
			p = e.local
		}
		t.Run(p.Name(), func(t *testing.T) {
			e.t = t
			nodes, _ := e.lending()
			nodes.cut = true
			e.cutPrepare(p, e.companionOf(p))
			e.restart()
			p = e.pluginOf(local)
			e.wantState(p, "c1", state.PreparationPreparing)
			if got := NUMANodes(e.records(), p.Name()); !slices.Equal(got, []int{2}) {
				t.Fatalf("NUMA nodes of the unfinished companion %v", got)
			}
			e.takeEvents()
			e.mustUnprepare(p, "c1")
			e.wantEvents("unprepared", "releasing l1", "revoke pod-uid-1/app 2 l1")
			e.wantClean()
		})
	}
}

// stepHooks are fakeHooks whose OnPrepared the driver commits with the
// companion's PREPARED (PreparedStepper), and whose write the disk stalls
// for stall and then fails with fault (nil: succeeds).
type stepHooks struct {
	*fakeHooks
	stall time.Duration
	fault error
}

// PreparedStep stalls the next write of the store and returns a change of
// nothing.
func (h *stepHooks) PreparedStep(id string, _ state.Source, _ []state.DeviceInfo) (func(*state.Records), func()) {
	h.e.mu.Lock()
	h.e.onWrite = func() error {
		time.Sleep(h.stall)
		return h.fault
	}
	h.e.mu.Unlock()
	return func(*state.Records) {}, func() { h.e.event("after %s", id) }
}

// A joint commit of a companion's PREPARED and its lease's READY whose
// write the disk stalls beyond commitTimeout is waited for in both drivers,
// and its write decides: a write that succeeds prepares the claim and tells
// the broker after it, a write that fails fails the Prepare, rolls it back
// and tells the broker that the lend failed, not that the claim is prepared.
func TestCompanionLateJointCommit(t *testing.T) {
	defer func(d time.Duration) { commitTimeout = d }(commitTimeout)
	commitTimeout = 200 * time.Millisecond
	for _, fault := range []error{nil, errors.New("disk failure")} {
		for _, local := range []bool{true, false} {
			e := lendEnv(t)
			p := e.pool
			if local {
				p = e.local
			}
			t.Run(fmt.Sprintf("%s, write error %v", p.Name(), fault), func(t *testing.T) {
				e.t = t
				e.lend(&fakeNodes{e: e}, &stepHooks{fakeHooks: &fakeHooks{e: e}, stall: 3 * commitTimeout, fault: fault})
				claim := e.companionOf(p)
				res, err := p.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
				mustOK(t, err)
				events := e.takeEvents()
				if fault == nil {
					if res[claim.UID].Err != nil || !slices.Contains(events, "after l1") {
						t.Fatalf("Prepare %v, calls:\n%s", res[claim.UID].Err, strings.Join(events, "\n"))
					}
					e.wantState(p, "c1", state.PreparationPrepared)
					return
				}
				if res[claim.UID].Err == nil || slices.Contains(events, "after l1") {
					t.Fatalf("Prepare %v, calls:\n%s", res[claim.UID].Err, strings.Join(events, "\n"))
				}
				if got := e.stateOf(p, "c1"); got == state.PreparationPrepared {
					t.Fatalf("claim %s after a failed write of PREPARED", got)
				}
				if !slices.ContainsFunc(events, func(ev string) bool { return strings.HasPrefix(ev, "failed l1: ") }) {
					t.Fatalf("the broker was not told that the lend failed, calls:\n%s", strings.Join(events, "\n"))
				}
			})
		}
	}
}
