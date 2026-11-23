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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/leases"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// A restart keeps a READY lease and its companion: kubelet prepares the
// prepared companion again, the lease stays READY with the same response;
// a lease whose companion's Prepare was cut after the gate (LENDING) is
// READY by kubelet's Prepare after the restart.
func TestRestartAnnounceAgain(t *testing.T) {
	e := newLeaseEnv(t, nil, fake.MemdevSpec{Serial: poolSerial}, fake.MemdevSpec{Serial: 0xc1ee0002, HostBridge: 24})
	e.openClaim()
	e.start()
	claim, r := e.requestPool("000000a1", api.Request{Op: api.OpRequest, Source: "pool", Size: "256Mi", Wait: "5s"}, "pooled0", poolExclusiveData)
	ready := r.resp
	// the second lease: its companion passes the gate, then the driver stops
	e.call(api.Request{Op: api.OpRequest, Source: "pool", Size: "256Mi", Wait: "0s"})
	second := e.waitClaim("cxl-lease-000000a2")
	_, err := e.b.OnLending("000000a2", second)
	noErr(t, err)

	e.restart()
	e.ctrs.forget()
	e.start()
	if l := e.lease("000000a1"); l == nil || l.State != state.LeaseReady {
		t.Fatalf("READY lease after a restart %+v", l)
	}
	if l := e.lease("000000a2"); l == nil || l.State != state.LeaseLending {
		t.Fatalf("LENDING lease after a restart %+v", l)
	}
	noErr(t, e.prepare(e.pool, allocatedPool(claim, "pooled0", poolExclusiveData)).Err)
	if l := e.lease("000000a1"); l == nil || l.State != state.LeaseReady {
		t.Fatalf("READY lease after the Prepare again %+v", l)
	}
	again, _ := e.call(api.Request{Op: api.OpEnv, Lease: "000000a1"})
	if !reflect.DeepEqual(again, ready) {
		t.Fatalf("response after the Prepare again\n%+v\nexpected\n%+v", again, ready)
	}
	data := strings.ReplaceAll(poolExclusiveData, "0xc1ee0001", "0xc1ee0002")
	noErr(t, e.prepare(e.pool, allocatedPool(second, "pooled1", data)).Err)
	if l := e.lease("000000a2"); l == nil || l.Got == nil || l.State != state.LeaseReady || l.Got.Serial != 0xc1ee0002 {
		t.Fatalf("cut lease after kubelet's Prepare %+v", l)
	}
	e.wantReleaseAll(2)
	e.wantNoRegions()
}

// A READY fs lease whose companion's node mount does not mount again at a
// start (its preparation PREPARED_UNRESTORED) fails at the start and is
// released in place: the list shows it Failed with why, its companion is
// deleted and unprepared, the lease removed. A LENDING lease (a stop between
// the companion's PREPARED and OnPrepared) fails the same way.
func TestUnrestoredCompanionFails(t *testing.T) {
	for _, s := range []state.LeaseState{state.LeaseReady, state.LeaseLending} {
		t.Run(string(s), func(t *testing.T) {
			e := newLeaseEnv(t, nil, fake.MemdevSpec{Serial: serialPoolP, Pmem: 512 << 20})
			dev := e.formatPmem(serialPoolP, "erofs")
			e.addSlices(persistentPoolSlice())
			e.openClaim()
			e.start()
			_, r := e.requestPool("000000a1", api.Request{Op: api.OpRequest, Serial: "0xc1af0001", Share: true, Mode: "fs", Wait: "5s"}, "llm0", poolPmemData)
			if r.resp.State != api.StateReady {
				t.Fatalf("response %+v", r.resp)
			}
			var stay atomic.Bool
			stay.Store(true)
			for _, resource := range []string{"pods", "resourceclaims"} {
				e.client.PrependReactor("delete", resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
					return stay.Load(), nil, nil
				})
			}
			e.b.Stop()
			if s != state.LeaseReady {
				noErr(t, e.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
					doc.Lease("000000a1").State = s
					return nil
				}))
			}
			_ = e.st.Close()
			noErr(t, e.k.Kernel().Unmount(filepath.Join(e.poolDir, "mounts", "c1af0001-direct")))
			e.k.SetMountError(dev, syscall.EIO, "")
			e.boot()
			if p := e.st.Snapshot().Preparation(e.pool.Name(), "uid-cxl-lease-000000a1"); p == nil || p.State != state.PreparationUnrestored {
				t.Fatalf("companion %+v", p)
			}
			l := e.stored("000000a1")
			if l == nil || l.State != state.LeaseFailed || l.Error != "the device of companion claim demo/cxl-lease-000000a1 was not restored at the start of the driver" {
				t.Fatalf("lease %+v", l)
			}
			e.start()
			list, _ := e.call(api.Request{Op: api.OpList})
			if list.Leases == nil || len(*list.Leases) != 1 || (*list.Leases)[0].State != api.StateFailed || (*list.Leases)[0].Error != l.Error {
				t.Fatalf("list %+v", list)
			}
			e.waitStored("000000a1", func(l *state.Lease) bool { return l.ReleaseRequested && l.State == state.LeaseFailed })
			stay.Store(false)
			e.k.SetMountError(dev, nil, "")
			e.waitState("000000a1", state.LeaseNone)
			e.wantGone(leases.CompanionName("000000a1"))
			e.wantNoRegions()
		})
	}
}

// A lend that fails at the companion's Prepare through the pool driver: the
// Prepare fails and is rolled back, the lease fails (OnLendFailed) and is
// released; the client gets Failed with the lend error.
func TestPoolLendFailure(t *testing.T) {
	e := newLeaseEnv(t, nil, fake.MemdevSpec{Serial: poolSerial})
	e.openClaim()
	e.start()
	e.ctrs.setLendErr(errors.New("container is memory-steered"))
	ch := e.async(api.Request{Op: api.OpRequest, Source: "pool", Size: "256Mi"})
	claim := e.waitClaim("cxl-lease-000000a1")
	wantErr(t, e.prepare(e.pool, allocatedPool(claim, "pooled0", poolExclusiveData)).Err, "memory-steered")
	e.wantNoRegions()
	resp := <-ch
	if resp.State != api.StateFailed || resp.Error != "lease 000000a1: lending NUMA node 2 to demo/requester/app: container is memory-steered" {
		t.Fatalf("lend failure response %+v", resp)
	}
	e.waitState("000000a1", state.LeaseNone)
	e.wantGone("cxl-lease-000000a1")
}

// The companion unprepared behind the broker (its pod deleted by someone):
// the driver tells the broker, the lease fails, the reconcile releases it.
func TestCompanionUnpreparedBehind(t *testing.T) {
	e := newLeaseEnv(t, nil, fake.MemdevSpec{Serial: poolSerial})
	e.openClaim()
	e.start()
	_, r := e.requestPool("000000a1", api.Request{Op: api.OpRequest, Source: "pool", Size: "256Mi", Wait: "5s"}, "pooled0", poolExclusiveData)
	if r.resp.State != api.StateReady {
		t.Fatalf("response %+v", r.resp)
	}
	noErr(t, e.client.CoreV1().Pods(testNS).Delete(context.Background(), leases.CompanionName("000000a1"), metav1.DeleteOptions{}))
	noErr(t, e.unprepare(e.pool, "uid-"+leases.CompanionName("000000a1")))
	if l := e.lease("000000a1"); l == nil || l.State != state.LeaseFailed || l.Error != "companion claim unprepared (companion pod deleted or evicted)" {
		t.Fatalf("lease %+v", l)
	}
	if got := e.ctrs.lentNodes(envPodUID, envCtr); len(got) != 0 {
		t.Fatalf("lent %v after the companion's Unprepare", got)
	}
	e.reconcile()
	e.waitState("000000a1", state.LeaseNone)
	_, err := e.client.ResourceV1().ResourceClaims(testNS).Get(context.Background(), leases.CompanionName("000000a1"), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("companion claim left: %v", err)
	}
	e.wantNoRegions()
}

// The lease device is published with --leases only; a claim on it is
// prepared by cxl.nri.io through the broker: its CDI devices claim-<uid>-0
// and lease-<uid> (the broker's edits) in the spec of cxl.nri.io, its
// socket; its Unprepare closes the lease claim first and removes them.
func TestLeaseClaimPrepare(t *testing.T) {
	if devs := dra.BuildDevices(dra.PublishConfig{IgnoreDRAM: true, PublishLeaseDevice: true}, &kernel.Snapshot{}, nil, nil, nil); devs["lease"] == nil {
		t.Fatalf("no lease device in %v", devs)
	}
	if devs := dra.BuildDevices(dra.PublishConfig{IgnoreDRAM: true}, &kernel.Snapshot{}, nil, nil, nil); len(devs) != 0 {
		t.Fatalf("lease device published without --leases: %v", devs)
	}
	e := brokerEnv(t)
	e.start()
	r := e.prepare(e.local, leaseClaimObj())
	noErr(t, r.Err)
	if len(r.Devices) != 1 || !slices.Equal(r.Devices[0].CDIDeviceIDs, []string{"cxl.nri.io/device=claim-lc1-0", "cxl.nri.io/device=lease-lc1"}) {
		t.Fatalf("prepared %+v", r.Devices)
	}
	if spec := e.cdiSpec(device.ClaimsCDISpecName); spec == nil || len(spec.Devices) != 2 || spec.Devices[1].Name != "lease-lc1" ||
		!reflect.DeepEqual(spec.Devices[1].ContainerEdits, e.b.CDIEdits(envLeaseClaim)) {
		t.Fatalf("CDI spec %+v", spec)
	}
	if info := e.local.ClaimInfo(envLeaseClaim); info == nil || info.Devices[0].DeviceType != device.DeviceTypeLease || info.Devices[0].Mode != "" {
		t.Fatalf("claim info %+v", info)
	}
	if _, err := os.Stat(e.sock()); err != nil {
		t.Fatalf("no lease socket: %v", err)
	}
	resp, _ := e.call(api.Request{Op: api.OpRequest, Size: "64Mi", Wait: "0s"})
	noErr(t, e.unprepare(e.local, envLeaseClaim))
	if _, err := os.Stat(e.sock()); !os.IsNotExist(err) || e.claim(envLeaseClaim) != nil || e.cdiSpec(device.ClaimsCDISpecName) != nil {
		t.Fatalf("lease claim left: %v", err)
	}
	e.waitState(resp.Lease, state.LeaseNone)
}

// The preparation PREPARED of a companion claim and its lease READY are one
// commit: no document has the companion prepared and its lease LENDING.
func TestCompanionPreparedWithReady(t *testing.T) {
	var mu sync.Mutex
	var apart []string
	e := newLeaseEnv(t, nil, fake.MemdevSpec{Serial: poolSerial})
	e.afterCommit = func() {
		r := e.st.Snapshot()
		for _, p := range r.Preparations {
			if l := r.Lease(p.Lease); p.Lease != "" && l != nil && p.State == state.PreparationPrepared && l.State == state.LeaseLending {
				mu.Lock()
				apart = append(apart, fmt.Sprintf("generation %d: %s %s, lease %s %s", r.Generation, p.ClaimName, p.State, l.ID, l.State))
				mu.Unlock()
			}
		}
	}
	e.restart()
	e.openClaim()
	e.start()
	_, r := e.requestPool("000000a1", api.Request{Op: api.OpRequest, Source: "pool", Size: "256Mi", Wait: "5s"}, "pooled0", poolExclusiveData)
	if r.resp.Error != "" || r.resp.State != api.StateReady {
		t.Fatalf("response %+v", r.resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(apart) > 0 {
		t.Fatalf("PREPARED and READY in separate commits: %v", apart)
	}
}
