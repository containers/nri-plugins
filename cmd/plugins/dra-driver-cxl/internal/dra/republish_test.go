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
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

// publishing starts the drivers of the env again with a publisher on a
// fake API server and starts publishing; it returns the API server once it
// has the node's slice. The publisher stops at the end of the test.
func (e *driverEnv) publishing() *k8sfake.Clientset {
	e.t.Helper()
	client := resourceSliceTestClient(envNode, nil)
	e.client = client
	e.restart()
	ctx, cancel := context.WithCancel(context.Background())
	pub := e.pub
	pub.Start(ctx)
	e.t.Cleanup(func() {
		pub.Stop()
		cancel()
	})
	waitNodeResourceSlice(e.t, client, envNode, 5*time.Second, pubNames(pub.Devices())...)
	return client
}

// takeTrigger returns whether a rescan was asked for since the last call
// and whether one of them was not periodic, and forgets them.
func takeTrigger(p *Publisher) (asked, urgent bool) {
	p.triggerMu.Lock()
	defer p.triggerMu.Unlock()
	asked, urgent = p.pending, p.pendingUrgent
	p.pending, p.pendingUrgent = false, false
	select {
	case <-p.trigger:
	default:
	}
	return asked, urgent
}

// resourceSliceWrites returns how many creates, updates, patches and deletes
// of ResourceSlices the API server got.
func resourceSliceWrites(client *k8sfake.Clientset) int {
	n := 0
	for _, a := range client.Actions() {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete", "delete-collection":
			if a.GetResource().Resource == "resourceslices" {
				n++
			}
		}
	}
	return n
}

// waitScans waits at most 5 s until scans counts at least want.
func waitScans(t *testing.T, scans *atomic.Int32, want int32) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); scans.Load() < want; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d scans, expected %d", scans.Load(), want)
		}
	}
}

// Prepares and Unprepares of both drivers that commit a change of the
// records ask the publisher for a rescan that is not periodic when they
// end; they neither scan nor publish themselves. A refused Prepare, a
// Prepare of a prepared claim and an Unprepare of a claim without records
// commit nothing and ask for none.
func TestRequestsAskForRescan(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec(), pmemSpec()}, poolSpec(serialPoolX))
	takeTrigger(e.pub)
	var scans atomic.Int32
	e.setOnScan(func() { scans.Add(1) })
	scanned := e.pub.LastScan()
	noAllocation := &resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: "u8", Name: "claim-u8", Namespace: envNS}}
	for _, step := range []struct {
		what string
		do   func()
		want bool
	}{
		{"a local Prepare", func() { e.mustPrepare(e.local, localClaim("u1", localDev, envBlockSize)) }, true},
		{"a Prepare of a prepared claim", func() { e.mustPrepare(e.local, localClaim("u1", localDev, envBlockSize)) }, false},
		{"a refused Prepare", func() { wantErrText(t, e.prepare(e.local, noAllocation).Err, "no allocation") }, false},
		{"a persistent Prepare", func() { e.mustPrepare(e.local, localClaim("u2", pmemDev, -device.SharesUnlimited, cfgBlock)) }, true},
		{"a DRAM Prepare", func() { e.mustPrepare(e.local, localClaim("u3", device.DRAMDeviceName, 64*mib)) }, true},
		{"a pool Prepare", func() { e.mustPrepare(e.pool, poolClaim("p1", "pooled0", poolExclusiveData)) }, true},
		{"a local Unprepare", func() { e.mustUnprepare(e.local, "u1") }, true},
		{"an Unprepare without records", func() { e.mustUnprepare(e.local, "u9") }, false},
		{"a pool Unprepare", func() { e.mustUnprepare(e.pool, "p1") }, true},
		{"a persistent Unprepare", func() { e.mustUnprepare(e.local, "u2") }, true},
		{"a DRAM Unprepare", func() { e.mustUnprepare(e.local, "u3") }, true},
	} {
		step.do()
		if asked, urgent := takeTrigger(e.pub); asked != step.want || urgent != step.want {
			t.Errorf("%s: rescan asked %v (not periodic %v), expected %v", step.what, asked, urgent, step.want)
		}
	}
	if n := scans.Load(); n != 0 || !e.pub.LastScan().Equal(scanned) {
		t.Fatalf("the requests scanned: %d scans, last scan %v (was %v)", n, e.pub.LastScan(), scanned)
	}
}

// TestUnchangedRescanWritesNoSlice pins what makes the rescan after every
// Prepare and Unprepare safe for the scheduling of the node: a rescan whose
// devices did not change writes nothing to the API server. A write of the
// node's slice, an equal one too, would requeue the pods that kube-scheduler
// holds back on a device that a deleted claim still has in flight. The
// Prepares and Unprepares of both drivers rescan, and the slice is written
// once, when it is created; a change of the devices then updates it once.
func TestUnchangedRescanWritesNoSlice(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()}, poolSpec(serialPoolX))
	client := e.publishing()
	if n := resourceSliceWrites(client); n != 1 {
		t.Fatalf("%d writes of the slice at the start, expected its create", n)
	}
	var scans atomic.Int32
	e.setOnScan(func() { scans.Add(1) })
	e.mustPrepare(e.local, localClaim("u1", localDev, envBlockSize))
	waitScans(t, &scans, 1)
	e.mustPrepare(e.pool, poolClaim("p1", "pooled0", poolExclusiveData))
	waitScans(t, &scans, 2)
	e.mustUnprepare(e.local, "u1")
	waitScans(t, &scans, 3)
	e.mustUnprepare(e.pool, "p1")
	waitScans(t, &scans, 4)
	ctx := context.Background()
	e.pub.Rescan(ctx, false)
	e.pub.Rescan(ctx, true)
	// the controller syncs an update of the devices at once: a write would
	// come within milliseconds
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if n := resourceSliceWrites(client); n != 1 {
			t.Fatalf("%d writes of the slice after %d rescans of unchanged devices, expected none but its create", n, scans.Load())
		}
	}
	// a change of the devices is written once
	e.plugLocal(pmemSpec())
	e.pub.Trigger(ctx, false)
	waitNodeResourceSlice(t, client, envNode, 5*time.Second, device.DRAMDeviceName, device.LeaseDeviceName, localDev, pmemDev)
	if n := resourceSliceWrites(client); n != 2 {
		t.Fatalf("%d writes of the slice after a change of the devices, expected its create and one update", n)
	}
}

// The last Unprepare of a memdev used shared that is not configured shared
// any more (the driver restarted with another configuration) has the
// memdev published again as an unshared one within the rescan that it asks
// for: with no kernel event that the publisher sees and no other rescan.
func TestUnprepareRepublishesWithoutRescan(t *testing.T) {
	e := newDriverEnv(t, []fake.MemdevSpec{sharedSpec()})
	e.setShared(serialShared)
	shared := MemdevDeviceName(serialShared)
	e.mustPrepare(e.local, localClaim("u1", shared, -1, cfgDax))
	// configured unshared after the restart of publishing: hidden while it
	// is used shared
	e.mu.Lock()
	delete(e.shared, serialShared)
	e.mu.Unlock()
	client := e.publishing()
	e.pub.Rescan(context.Background(), false)
	waitNodeResourceSlice(t, client, envNode, 5*time.Second, device.DRAMDeviceName, device.LeaseDeviceName)
	e.mustUnprepare(e.local, "u1")
	waitNodeResourceSlice(t, client, envNode, 2*time.Second, device.DRAMDeviceName, device.LeaseDeviceName, shared)
	if d := e.pub.Published()[shared]; d == nil || d.Dev.(*Memdev).Shared {
		t.Fatalf("published %+v, expected the memdev unshared", d)
	}
}

// A Prepare of the pool driver of a memdev that the slice of cxl.nri.io
// publishes as a local memdev (no pool controller listed it) has the slice
// republished without it within the rescan that it asks for.
func TestPoolPrepareRepublishesLocalSlice(t *testing.T) {
	// the scans of cxl.nri.io find the pool device as a local memdev
	e := newDriverEnv(t, []fake.MemdevSpec{poolSpec(serialPoolX)})
	client := e.publishing()
	pooled := MemdevDeviceName(serialPoolX)
	if got := nodeResourceSliceDevices(t, client, envNode); !slices.Contains(got, pooled) {
		t.Fatalf("slice %v without %s", got, pooled)
	}
	e.mustPrepare(e.pool, poolClaim("p1", "pooled0", poolExclusiveData))
	waitNodeResourceSlice(t, client, envNode, 2*time.Second, device.DRAMDeviceName, device.LeaseDeviceName)
	e.mustUnprepare(e.pool, "p1")
}

// A burst of Prepares coalesces into a bounded number of rescans: the one
// that runs when the burst comes and one more that covers every Prepare
// that ended while it ran.
func TestPrepareBurstCoalescesRescans(t *testing.T) {
	const burst = 8
	e := newDriverEnv(t, []fake.MemdevSpec{localSpec()})
	e.publishing()
	var scans atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	e.setOnScan(func() {
		if scans.Add(1) == 1 {
			close(started)
			<-release
		}
	})
	e.mustPrepare(e.local, localClaim("u0", device.DRAMDeviceName, 64*mib))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no rescan after the Prepare")
	}
	for i := 1; i < burst; i++ {
		e.mustPrepare(e.local, localClaim(fmt.Sprintf("u%d", i), device.DRAMDeviceName, 64*mib))
	}
	close(release)
	waitScans(t, &scans, 2)
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if n := scans.Load(); n != 2 {
			t.Fatalf("%d rescans for a burst of %d Prepares, expected 2", n, burst)
		}
	}
	for i := 0; i < burst; i++ {
		e.mustUnprepare(e.local, fmt.Sprintf("u%d", i))
	}
}
