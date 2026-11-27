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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// The Unprepare of a claim that the driver does not know returns without
// waiting for the Prepare of another claim, which holds the memdev for long
// when it zeroes a whole shared device.
func TestUnprepareUnknownDoesNotWait(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	entered, release := make(chan struct{}), make(chan struct{})
	e.k.Before("cxl create-region", func() {
		close(entered)
		<-release
	})
	prepared := make(chan error, 1)
	go func() {
		res, err := e.local.PrepareResourceClaims(t.Context(), []*resourcev1.ResourceClaim{localClaim("u1", localDev, envBlockSize)})
		if err == nil {
			err = res["u1"].Err
		}
		prepared <- err
	}()
	<-entered
	done := make(chan error, 1)
	go func() {
		res, err := e.local.UnprepareResourceClaims(t.Context(), []kubeletplugin.NamespacedObject{{UID: "u2"}})
		if err == nil {
			err = res["u2"]
		}
		done <- err
	}()
	select {
	case err := <-done:
		close(release)
		mustOK(t, err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the Unprepare of an unknown claim waits for the Prepare of another claim")
	}
	mustOK(t, <-prepared)
	e.mustUnprepare(e.local, "u1")
	e.wantClean()
}

// The Unprepare of a companion claim of cxl.nri.io: the lease claim closed
// before anything, the broker told, UNPREPARING committed before the
// revoke, the memory released after it, the record removed.
func TestUnprepareOrder(t *testing.T) {
	e := lendEnv(t)
	nodes, _ := e.lending()
	e.setLeases()
	e.mustPrepare(e.local, e.companionOf(e.local))
	var atRevoke []string
	nodes.onRevoke = func(int) {
		atRevoke = append(atRevoke, string(e.stateOf(e.local, "c1")))
		if e.records().Grant(state.GrantRef{Claim: "c1"}) == nil || e.onlineBlocks() == 0 {
			atRevoke = append(atRevoke, "released")
		}
	}
	e.takeEvents()
	e.mustUnprepare(e.local, "c1")
	e.wantEvents("unprepared", "close c1 (PREPARED)", "releasing l1", "revoke pod-uid-1/app 2 l1")
	if len(atRevoke) != 1 || atRevoke[0] != string(state.PreparationUnpreparing) {
		t.Fatalf("at the revoke: %v", atRevoke)
	}
	e.wantClean()
}

// A revoke that fails, or comes before the lender is set up, fails the
// Unprepare of a companion claim of either driver: the claim stays
// UNPREPARING with its memory, kubelet's retry finishes it.
func TestUnprepareRevokeFails(t *testing.T) {
	for _, local := range []bool{true, false} {
		e := lendEnv(t)
		p := e.pluginOf(local)
		t.Run(p.Name(), func(t *testing.T) {
			e.t = t
			nodes, _ := e.lending()
			e.mustPrepare(p, e.companionOf(p))
			nodes.setErrors(nil, errFake)
			err := e.unprepare(p, "c1")
			revokeErr := "lease l1: revoking NUMA node 2 from demo/requester/app: " + errFake.Error()
			wantErrText(t, err, p.Name()+": claim demo/cxl-lease-l1: "+revokeErr)
			e.wantState(p, "c1", state.PreparationUnpreparing)
			if e.onlineBlocks() == 0 {
				t.Fatal("the memory went offline after a failed revoke")
			}
			// a start: the lender is not set up yet
			e.lent = false
			e.restart()
			p = e.pluginOf(local)
			wantErrText(t, e.unprepare(p, "c1"), "lending is not set up yet (the driver is starting)")
			e.wantState(p, "c1", state.PreparationUnpreparing)
			nodes.setErrors(nil, nil)
			e.lend(nodes, e.hooks)
			e.mustUnprepare(p, "c1")
			e.wantClean()
		})
	}
}

// A release that fails leaves the claim UNPREPARING with its CDI devices;
// the retry releases it.
func TestUnprepareReleaseFails(t *testing.T) {
	t.Run("cxl.nri.io", func(t *testing.T) {
		e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
		e.mustPrepare(e.local, localClaim("u1", localDev, envBlockSize, cfgDax))
		dax := e.records().Grant(state.GrantRef{Claim: "u1"}).Got.Dax
		e.k.FailWrite("bus/dax/devices/"+dax+"/size", syscall.EBUSY)
		wantErrText(t, e.unprepare(e.local, "u1"), "cxl.nri.io: claim demo/claim-u1: ")
		e.wantState(e.local, "u1", state.PreparationUnpreparing)
		if e.cdiDevice(e.local, "claim-u1-0") == nil {
			t.Fatal("no CDI device of an UNPREPARING claim")
		}
		if info := e.local.ClaimInfo("u1"); info == nil {
			t.Fatal("no claim info of an UNPREPARING claim")
		}
		// a Prepare again of the UNPREPARING claim returns its result
		res := e.mustPrepare(e.local, localClaim("u1", localDev, envBlockSize, cfgDax))
		wantResult(t, res, resultDevice("mem", envNode, localDev, "cxl.nri.io/device=claim-u1-0"))
		e.wantState(e.local, "u1", state.PreparationUnpreparing)
		e.restart()
		e.wantState(e.local, "u1", state.PreparationUnpreparing)
		if e.cdiDevice(e.local, "claim-u1-0") == nil {
			t.Fatal("no CDI device of an UNPREPARING claim after a start")
		}
		e.mustUnprepare(e.local, "u1")
		e.wantClean()
	})
	t.Run("cxl-pool.nri.io", func(t *testing.T) {
		e := newDriverEnv(t, nil, poolPmemSpec(serialPoolP))
		e.formatPmem(serialPoolP, "erofs")
		e.mustPrepare(e.pool, poolClaim("u1", "llm0", poolPmemData, cfgFsRO))
		mnt := filepath.Join(e.poolDir, "mounts", "c1af0001-direct")
		e.k.SetBusy(mnt, true)
		err := e.unprepare(e.pool, "u1")
		wantErrText(t, err, "cxl-pool.nri.io: claim demo/claim-u1: ")
		wantErrText(t, err, "memdev 0xc1af0001: unmounting "+mnt+": device or resource busy")
		e.wantState(e.pool, "u1", state.PreparationUnpreparing)
		if e.cdiDevice(e.pool, "claim-u1-0") == nil {
			t.Fatal("no CDI device of an UNPREPARING claim")
		}
		// a Prepare again of the UNPREPARING claim returns its result
		res := e.mustPrepare(e.pool, poolClaim("u1", "llm0", poolPmemData, cfgFsRO))
		if len(res.Devices) != 1 || len(res.Devices[0].CDIDeviceIDs) != 1 || res.Devices[0].CDIDeviceIDs[0] != "cxl-pool.nri.io/device=claim-u1-0" {
			t.Fatalf("the result of an UNPREPARING claim %+v", res)
		}
		e.wantState(e.pool, "u1", state.PreparationUnpreparing)
		// the retry of an UNPREPARING claim
		wantErrText(t, e.unprepare(e.pool, "u1"), "busy")
		e.k.SetBusy(mnt, false)
		e.mustUnprepare(e.pool, "u1")
		e.wantClean()
	})
}

// A CDI spec that does not write after the release of a prepared claim
// fails the Unprepare; the claim is gone, and kubelet's retry finds nothing
// to release and writes the spec.
func TestUnprepareCDIFails(t *testing.T) {
	for _, local := range []bool{true, false} {
		e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, poolSpec(serialPoolX), fake.MemdevSpec{Serial: serialPoolS, HostBridge: 24})
		p := e.pluginOf(local)
		t.Run(p.Name(), func(t *testing.T) {
			e.t = t
			claims := []*resourcev1.ResourceClaim{localClaim("u1", localDev, envBlockSize), localClaim("u2", device.DRAMDeviceName, 64*mib)}
			if !local {
				claims = []*resourcev1.ResourceClaim{poolClaim("u1", "pooled0", poolExclusiveData), poolClaim("u2", "shared0", poolSharedData)}
			}
			for _, c := range claims {
				e.mustPrepare(p, c)
			}
			if names := e.cdiNames(p); len(names) != 2 {
				t.Fatalf("CDI devices %v", names)
			}
			// the spec file a directory with an entry: the rename of the
			// written spec fails (a spec of no devices is not written)
			spec := filepath.Join(e.cdiDir, p.Rules().CDISpecName())
			mustOK(t, os.Remove(spec))
			mustOK(t, os.MkdirAll(filepath.Join(spec, "entry"), 0o755))
			logs := klogCapture(t)
			wantErrText(t, e.unprepare(p, "u1"), p.Name()+": claim demo/claim-u1: write CDI spec "+p.Rules().CDISpecName()+": ")
			if strings.Contains(logs(), "unprepared claim") {
				t.Fatal("a failed Unprepare logged as unprepared")
			}
			if e.preparation(p, "u1") != nil {
				t.Fatal("the record of u1 is left")
			}
			mustOK(t, os.RemoveAll(spec))
			e.mustUnprepare(p, "u1")
			if names := e.cdiNames(p); !slices.Equal(names, []string{"claim-u2-0"}) {
				t.Fatalf("CDI devices after kubelet's retry %v", names)
			}
			e.mustUnprepare(p, "u2")
			e.wantClean()
		})
	}
}

// The Unprepare of a claim of mode fs of cxl.nri.io that failed after its
// node mount was unmounted: the claim stays UNPREPARING with its CDI device
// without the mount that is gone, the CDI spec still
// writes, so the next Prepare and a start succeed, and kubelet's retry
// finishes the release.
func TestUnprepareFailedAfterUnmount(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec(), pmemSpec()})
	e.formatPmem(serialPmem, "ext4")
	e.mustPrepare(e.local, localClaim("w1", pmemDev, -device.SharesUnlimited, cfgFsRW))
	mnt := e.local.ClaimInfo("w1").Devices[0].NodeMount
	e.k.FailCommand("cxl disable-region", syscall.EIO)
	wantErrText(t, e.unprepare(e.local, "w1"), "cxl.nri.io: claim demo/claim-w1: ")
	e.wantState(e.local, "w1", state.PreparationUnpreparing)
	if mi, err := e.k.Kernel().Mounted(mnt); err != nil || mi != nil {
		t.Fatalf("node mount %s: %+v %v", mnt, mi, err)
	}
	e.mustPrepare(e.local, localClaim("u2", localDev, envBlockSize))
	cd := e.cdiDevice(e.local, "claim-w1-0")
	hasEnv(t, cd, "CXL_CLAIM_w1_0=1")
	if len(cd.ContainerEdits.Mounts) != 0 {
		t.Fatalf("the CDI device of the claim mounts %+v", cd.ContainerEdits.Mounts)
	}
	e.restart()
	e.wantState(e.local, "w1", state.PreparationUnpreparing)
	if e.cdiDevice(e.local, "claim-w1-0") == nil || e.cdiDevice(e.local, "claim-u2-0") == nil {
		t.Fatalf("CDI devices after a start %v", e.cdiNames(e.local))
	}
	e.mustUnprepare(e.local, "w1")
	e.mustUnprepare(e.local, "u2")
	e.wantClean()
}
