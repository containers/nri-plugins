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
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// resourceSliceTestClient returns a fake clientset with the node, whose
// creates and updates of ResourceSlices do what the API server does and the
// fake does not: a name from GenerateName, a resource version (the mutation
// cache of the controller needs one). stored, if not nil, changes a slice as
// it is stored.
func resourceSliceTestClient(node string, stored func(*resourcev1.ResourceSlice)) *fake.Clientset {
	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, UID: types.UID("uid-" + node)}})
	var mu sync.Mutex
	rv := 0
	store := func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		rs := a.(interface{ GetObject() runtime.Object }).GetObject().(*resourcev1.ResourceSlice)
		rv++
		if rs.Name == "" && rs.GenerateName != "" {
			rs.Name = rs.GenerateName + strconv.Itoa(rv)
		}
		rs.ResourceVersion = strconv.Itoa(rv)
		if stored != nil {
			stored(rs)
		}
		return false, nil, nil // the tracker stores it
	}
	client.PrependReactor("create", "resourceslices", store)
	client.PrependReactor("update", "resourceslices", store)
	return client
}

// nodeResourceSliceDevices returns the device names of the node's cxl.nri.io
// slices, nil if it has none.
func nodeResourceSliceDevices(t *testing.T, client *fake.Clientset, node string) []string {
	t.Helper()
	list, err := client.ResourceV1().ResourceSlices().List(context.Background(), metav1.ListOptions{})
	pubNoErr(t, err)
	var names []string
	for _, rs := range list.Items {
		if rs.Spec.Driver != device.DriverName || rs.Spec.NodeName == nil || *rs.Spec.NodeName != node {
			continue
		}
		if len(rs.OwnerReferences) != 1 || rs.OwnerReferences[0].Kind != "Node" || rs.OwnerReferences[0].UID != types.UID("uid-"+node) {
			t.Errorf("slice %s: owner references %+v, expected node %s", rs.Name, rs.OwnerReferences, node)
		}
		if names == nil {
			names = []string{} // a slice, maybe without devices
		}
		for _, d := range rs.Spec.Devices {
			names = append(names, d.Name)
		}
	}
	return names
}

// waitNodeResourceSlice waits at most timeout until the node's slice has the
// devices, and returns how long it took.
func waitNodeResourceSlice(t *testing.T, client *fake.Clientset, node string, timeout time.Duration, want ...string) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		got := nodeResourceSliceDevices(t, client, node)
		if fmt.Sprint(got) == fmt.Sprint(want) {
			return time.Since(start)
		}
		if time.Since(start) > timeout {
			t.Fatalf("after %v: devices of the slice of %s %v, expected %v", timeout, node, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitLog waits at most 5 s until n lines of the log contain text.
func waitLog(t *testing.T, logs func() string, text string, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); logMatches(logs(), text) != n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d lines with %q, expected %d:\n%s", logMatches(logs(), text), text, n, logs())
		}
	}
}

// TestNodeSliceBackAfterKubeletWipe pins the short SyncDelay of the node's
// slice: kubelet deletes every ResourceSlice of its node when it starts,
// the driver keeps running, and the slice is back within the SyncDelay, not
// after the library's default of 30 s. The driver's own publishes are not
// delayed, and after Stop nothing puts the slice back.
func TestNodeSliceBackAfterKubeletWipe(t *testing.T) {
	client := resourceSliceTestClient(pubNode, nil)
	var dram atomic.Bool
	p := pubPublisher(t, publishOptions{
		Config:   PublishConfig{PublishLeaseDevice: true},
		NodeName: pubNode,
		Client:   client,
		Scan:     dramScan(&dram),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pubNoErr(t, p.Scan(ctx))
	p.Start(ctx)
	defer p.Stop()
	waitNodeResourceSlice(t, client, pubNode, 5*time.Second, device.LeaseDeviceName)

	// kubelet's wipe: DeleteCollection of spec.nodeName (the fake
	// implements no DeleteCollection, and no field selectors).
	list, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	pubNoErr(t, err)
	for _, rs := range list.Items {
		pubNoErr(t, client.ResourceV1().ResourceSlices().Delete(ctx, rs.Name, metav1.DeleteOptions{}))
	}
	if got := nodeResourceSliceDevices(t, client, pubNode); got != nil {
		t.Fatalf("slice not deleted: %v", got)
	}
	took := waitNodeResourceSlice(t, client, pubNode, resourceSliceSyncDelay+5*time.Second, device.LeaseDeviceName)
	t.Logf("slice back %v after the wipe (sync delay %v)", took.Round(time.Millisecond), resourceSliceSyncDelay)

	// A publish of the driver (e.g. a udev rescan) syncs at once, also
	// right after an informer event of the slice (its re-creation above):
	// normally in milliseconds, the bound tolerates a loaded machine and
	// is still far below the default sync delay of 30 s.
	dram.Store(true)
	p.Rescan(ctx, false)
	took = waitNodeResourceSlice(t, client, pubNode, 10*time.Second, device.DRAMDeviceName, device.LeaseDeviceName)
	if took >= 5*time.Second {
		t.Errorf("publish took %v, expected at once", took)
	}
	t.Logf("slice changed %v after the publish", took.Round(time.Millisecond))

	// Stop leaves the slice, and nothing puts it back any more.
	p.Stop()
	p.Stop()
	list, err = client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	pubNoErr(t, err)
	if len(list.Items) != 1 {
		t.Fatalf("%d slices after Stop, expected 1", len(list.Items))
	}
	pubNoErr(t, client.ResourceV1().ResourceSlices().Delete(ctx, list.Items[0].Name, metav1.DeleteOptions{}))
	p.Rescan(ctx, false)
	time.Sleep(resourceSliceSyncDelay + 500*time.Millisecond)
	if got := nodeResourceSliceDevices(t, client, pubNode); got != nil {
		t.Errorf("slice re-created after Stop: %v", got)
	}
}

// TestDroppedNodeAllocatable pins the reaction to an API server without the
// feature gate DRANodeAllocatableResources, which drops
// nodeAllocatableResources from the slice: one warning, and the devices
// are published without it from then on, so that a publish of unchanged
// devices writes nothing. Another dropped field is logged once and changes
// nothing of the devices.
func TestDroppedNodeAllocatable(t *testing.T) {
	logs := klogtest.Capture(t, 0)
	var dropMultiple atomic.Bool
	client := resourceSliceTestClient(pubNode, func(rs *resourcev1.ResourceSlice) {
		for i := range rs.Spec.Devices {
			rs.Spec.Devices[i].NodeAllocatableResources = nil
			if dropMultiple.Load() {
				rs.Spec.Devices[i].AllowMultipleAllocations = nil
			}
		}
	})
	var dram atomic.Bool
	dram.Store(true)
	p := pubPublisher(t, publishOptions{
		Config:   PublishConfig{PublishLeaseDevice: true},
		NodeName: pubNode,
		Client:   client,
		Scan:     dramScan(&dram),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pubNoErr(t, p.Scan(ctx))
	if d := p.Resources().Pools[pubNode].Slices[0].Devices[0]; d.Name != device.DRAMDeviceName || d.NodeAllocatableResources == nil {
		t.Fatalf("device %+v, expected the DRAM with nodeAllocatableResources", d)
	}
	p.Start(ctx)
	defer p.Stop()
	waitNodeResourceSlice(t, client, pubNode, 5*time.Second, device.DRAMDeviceName, device.LeaseDeviceName)
	const warning = "the API server drops nodeAllocatableResources"
	waitLog(t, logs, warning, 1)
	for range 3 {
		p.Rescan(ctx, false)
	}
	// the controller syncs an update of the devices at once: a write would
	// come within milliseconds
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if n := resourceSliceWrites(client); n != 1 {
			t.Fatalf("%d writes of the slice after the rescans of unchanged devices, expected its create", n)
		}
	}
	if d := p.Resources().Pools[pubNode].Slices[0].Devices[0]; d.NodeAllocatableResources != nil {
		t.Fatalf("device %+v published with nodeAllocatableResources", d)
	}
	if n := logMatches(logs(), "some fields were dropped"); n != 0 {
		t.Fatalf("%d errors of the dropped nodeAllocatableResources:\n%s", n, logs())
	}
}

// TestDroppedOtherField pins the reaction to another field dropped by the
// API server: the error is logged once, and the devices are published as
// before, so the controller writes the slice again at every publish.
func TestDroppedOtherField(t *testing.T) {
	logs := klogtest.Capture(t, 0)
	client := resourceSliceTestClient(pubNode, func(rs *resourcev1.ResourceSlice) {
		for i := range rs.Spec.Devices {
			rs.Spec.Devices[i].AllowMultipleAllocations = nil
		}
	})
	var dram atomic.Bool
	dram.Store(true)
	p := pubPublisher(t, publishOptions{
		Config:   PublishConfig{PublishLeaseDevice: true},
		NodeName: pubNode,
		Client:   client,
		Scan:     dramScan(&dram),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pubNoErr(t, p.Scan(ctx))
	p.Start(ctx)
	defer p.Stop()
	waitNodeResourceSlice(t, client, pubNode, 5*time.Second, device.DRAMDeviceName, device.LeaseDeviceName)
	waitLog(t, logs, "some fields were dropped", 1)
	for range 2 {
		writes := resourceSliceWrites(client)
		p.Rescan(ctx, false)
		for deadline := time.Now().Add(5 * time.Second); resourceSliceWrites(client) == writes; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no write of the slice after a publish")
			}
		}
	}
	if n := logMatches(logs(), "some fields were dropped"); n != 1 {
		t.Fatalf("%d errors of the dropped field, expected 1:\n%s", n, logs())
	}
	if n := logMatches(logs(), "nodeAllocatableResources"); n != 0 {
		t.Fatalf("%d lines of nodeAllocatableResources:\n%s", n, logs())
	}
	if d := p.Resources().Pools[pubNode].Slices[0].Devices[0]; d.NodeAllocatableResources == nil || d.AllowMultipleAllocations == nil {
		t.Fatalf("device %+v, expected nodeAllocatableResources and allowMultipleAllocations", d)
	}
}

// TestStartWithoutAPIServer pins that Start does not wait for the API
// server: the slice is published once the controller's informer has
// synced, and Stop returns also while it waits.
func TestStartWithoutAPIServer(t *testing.T) {
	client := resourceSliceTestClient(pubNode, nil)
	var down atomic.Bool
	var lists atomic.Int32
	down.Store(true)
	// the controller's informer lists the node's slices, the test all
	client.PrependReactor("list", "resourceslices", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.ListAction).GetListRestrictions().Fields.Empty() {
			return false, nil, nil
		}
		lists.Add(1)
		if down.Load() {
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	newPublisher := func() *Publisher {
		p := pubPublisher(t, publishOptions{
			Config:   PublishConfig{PublishLeaseDevice: true},
			NodeName: pubNode,
			Client:   client,
			Scan:     pubNoScan,
		})
		pubNoErr(t, p.Scan(context.Background()))
		return p
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newPublisher()
	start := time.Now()
	p.Start(ctx)
	defer p.Stop()
	if took := time.Since(start); took > time.Second {
		t.Errorf("Start took %v", took)
	}
	for deadline := time.Now().Add(5 * time.Second); lists.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no list of the slices")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := nodeResourceSliceDevices(t, client, pubNode); got != nil {
		t.Fatalf("slice %v published while the list fails", got)
	}
	down.Store(false)
	took := waitNodeResourceSlice(t, client, pubNode, 15*time.Second, device.LeaseDeviceName)
	t.Logf("slice published %v after the API server came back", took.Round(time.Millisecond))

	down.Store(true)
	q := newPublisher()
	q.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	q.Stop()
	if took := time.Since(start); took > time.Second {
		t.Errorf("Stop took %v while waiting for the API server", took)
	}
}

// TestSliceChunks pins the slices of the node's pool: at most
// ResourceSliceMaxDevices devices each, sorted by name across them, and one
// empty slice for no devices.
func TestSliceChunks(t *testing.T) {
	// the DRAM of node 0, the lease and 130 regions
	snap := &kernel.Snapshot{NUMANodes: pubNodes(0)}
	for i := 1; i <= 130; i++ {
		snap.NUMANodes = append(snap.NUMANodes, pubNodes(i)...)
		snap.Memdevs = append(snap.Memdevs, pubMem(fmt.Sprintf("mem%d", i), uint64(i), 1<<30, 0))
		pubRegion(snap, fmt.Sprintf("region%d", i), i, 1<<30, uint64(i))
	}
	client := resourceSliceTestClient(pubNode, nil)
	p := pubPublisher(t, publishOptions{
		Config:   PublishConfig{PublishLeaseDevice: true},
		NodeName: pubNode,
		Client:   client,
		Scan:     func(context.Context) (*kernel.Snapshot, error) { return snap, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pubNoErr(t, p.Scan(ctx))
	var names []string
	var sizes []int
	for _, s := range p.Resources().Pools[pubNode].Slices {
		sizes = append(sizes, len(s.Devices))
		for _, d := range s.Devices {
			names = append(names, d.Name)
		}
	}
	if fmt.Sprint(sizes) != "[128 4]" || !slices.IsSorted(names) {
		t.Fatalf("slices of %v devices: %v", sizes, names)
	}
	p.Start(ctx)
	defer p.Stop()
	slices.Sort(names)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		got := nodeResourceSliceDevices(t, client, pubNode)
		slices.Sort(got)
		if slices.Equal(got, names) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("published %d devices, expected %d", len(got), len(names))
		}
	}
	list, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	pubNoErr(t, err)
	if len(list.Items) != 2 || list.Items[0].Spec.Pool.ResourceSliceCount != 2 {
		t.Fatalf("%d slices, expected 2 of a pool of 2", len(list.Items))
	}

	empty := pubPublisher(t, publishOptions{NodeName: pubNode})
	pubNoErr(t, empty.Scan(ctx))
	if s := empty.Resources().Pools[pubNode].Slices; len(s) != 1 || len(s[0].Devices) != 0 {
		t.Fatalf("slices %+v of no devices, expected one empty", s)
	}
}
