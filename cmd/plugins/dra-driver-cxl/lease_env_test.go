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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	cdiSpecs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/dra"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/engine"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/leases"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
	"github.com/containers/nri-plugins/pkg/cxl/kernel/fake"
)

const (
	// the requester of the leases
	envPod    = "requester"
	envPodUID = "pod-uid-1"
	envCtr    = "app"
	// envLeaseClaim is the uid of the lease claim of the requester pod.
	envLeaseClaim = "lc1"
	// envPool is the DRA pool of the pool devices.
	envPool = "fake-cxl-pool"

	// serials of the local memdevs next to localSerial: shared (1 GiB of
	// RAM) and persistent (512 MiB).
	serialShared = 0xc100e2e2
	serialPmem   = 0xc100f5e1
	// serials of the pool devices next to poolSerial: shared, persistent.
	serialPoolS = 0xc1ae0001
	serialPoolP = 0xc1af0001

	// the status data of the pool devices
	poolExclusiveData = `{"serial":"0xc1ee0001","shared":false,"size":268435456,"host":"n1"}`
	poolSharedData    = `{"serial":"0xc1ae0001","shared":true,"size":268435456}`
	poolPmemData      = `{"serial":"0xc1af0001","shared":true,"size":536870912,"persistent":true}`
	poolWriterData    = `{"serial":"0xc1af0001","shared":true,"size":536870912,"persistent":true,"exclusive":true}`
	poolPmemRWData    = `{"serial":"0xc1af0001","shared":true,"size":536870912,"persistent":true,"singleHost":true}`

	// pmemBytes are the bytes of the fsdax namespace of 512 MiB of
	// persistent memory of the fake.
	pmemBytes = 534773760
)

// requester is the container of the requester pod that the env's peers
// are in.
var requester = leases.Requester{Pod: state.PodRef{Namespace: testNS, Name: envPod, UID: envPodUID}, Container: envCtr}

// leaseEnv is a node for the tests of the lease broker with the drivers:
// the fake kernel of pkg/cxl/kernel/fake, a store in a temporary
// directory, both DRA plugins over one device engine (dra.NewNode), the
// broker over the same store, a fake API client and a fake container
// plugin. Like kubelet, the fake client unprepares a companion claim
// through the plugin that prepared it when the claim is deleted, and keeps
// the claim while that fails. The kernel, the files, the client and the
// NRI plugin outlive a restart.
type leaseEnv struct {
	t *testing.T
	k *fake.Fake
	// dir is the plugin data directory of cxl.nri.io, of the broker and
	// of the store; poolDir that of the pool driver.
	dir, poolDir, cdiDir, cli string

	client *k8sfake.Clientset
	ctrs   *fakeContainers
	st     *store.Store
	node   *dra.Node
	local  *dra.Plugin
	pool   *dra.Plugin
	b      *leases.Broker

	// memdevs are the local memdevs that the scans find, by serial.
	memdevs map[uint64]kernel.MemdevInfo
	// shared are the local memdevs configured shared.
	shared map[uint64]bool

	// opts change the options of the broker of the next start, nil for
	// none; afterCommit is the store's AfterCommit hook of the next start.
	opts        func(o *leases.Options)
	afterCommit func()
	// pmem is the block device of the persistent device formatted last.
	pmem string

	mu sync.Mutex
	// prepared are the drivers that prepared the companion claims, by
	// name.
	prepared map[string]string
}

// newLeaseEnv returns a node with the local memdevs of local (found by the
// scans of cxl.nri.io) and the pool devices of pool plugged, whose broker
// is not started yet (start).
func newLeaseEnv(t *testing.T, local []fake.MemdevSpec, pool ...fake.MemdevSpec) *leaseEnv {
	t.Helper()
	dir := t.TempDir()
	e := &leaseEnv{
		t:        t,
		k:        fake.New(t),
		dir:      filepath.Join(dir, device.DriverName),
		poolDir:  filepath.Join(dir, device.PoolDriverName),
		cdiDir:   filepath.Join(dir, "cdi"),
		cli:      filepath.Join(dir, "cxl-request"),
		client:   k8sfake.NewClientset(requesterPod(), leaseClaimObj()),
		memdevs:  map[uint64]kernel.MemdevInfo{},
		shared:   map[uint64]bool{},
		prepared: map[string]string{},
	}
	e.ctrs = newFakeContainers(requester)
	for _, d := range []string{e.dir, e.poolDir, e.cdiDir} {
		noErr(t, os.MkdirAll(d, 0o755))
	}
	noErr(t, os.WriteFile(e.cli, []byte("#!/bin/true\n"), 0o755))
	for _, s := range local {
		e.plugLocal(s)
	}
	for _, s := range pool {
		e.k.Plug(s)
	}
	// added before the broker uses the client: the fake client does not
	// lock its reactor chain
	e.client.PrependReactor("delete", "resourceclaims", e.unprepareDeleted)
	t.Cleanup(func() {
		if e.b != nil {
			e.b.Stop()
		}
		if e.st != nil {
			_ = e.st.Close()
		}
	})
	e.boot()
	return e
}

// brokerEnv returns a node without memdevs.
func brokerEnv(t *testing.T) *leaseEnv {
	return newLeaseEnv(t, nil)
}

// requesterPod is the requester pod.
func requesterPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: envPod, Namespace: testNS, UID: types.UID(envPodUID)},
		Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyAlways, NodeName: testNode},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// leaseClaimObj is the prepared claim on the lease device of the requester
// pod.
func leaseClaimObj() *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lease", Namespace: testNS, UID: types.UID(envLeaseClaim)},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{
				Results: []resourcev1.DeviceRequestAllocationResult{{Request: "lease", Driver: device.DriverName, Pool: testNode, Device: device.LeaseDeviceName}},
			}},
			ReservedFor: []resourcev1.ResourceClaimConsumerReference{{Resource: "pods", Name: envPod, UID: types.UID(envPodUID)}},
		},
	}
}

// plugLocal hot-adds a local memdev that the scans find.
func (e *leaseEnv) plugLocal(spec fake.MemdevSpec) string {
	name := e.k.Plug(spec)
	ram, pmem := spec.Ram, spec.Pmem
	if ram == 0 && pmem == 0 {
		ram = 256 << 20
	}
	e.memdevs[spec.Serial] = kernel.MemdevInfo{Name: name, Serial: spec.Serial, State: kernel.MemdevReady, RamSize: ram, PmemSize: pmem,
		NUMAAffinity: 0}
	return name
}

// boot starts the drivers on the env's kernel and files as the start of the
// process does: the store opened, the drivers of both sources and their
// recovery, the broker (not started), the CDI specs written.
func (e *leaseEnv) boot() {
	t := e.t
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Dir: e.dir, BootID: e.k.BootID(), AfterCommit: e.afterCommit})
	noErr(t, err)
	e.st = st
	e.node, err = dra.NewNode(dra.NodeOptions{
		Kernel:      e.k.Kernel(),
		Store:       st,
		Dir:         e.dir,
		PoolDriver:  device.PoolDriverName,
		LeaseFsRoot: leases.LeaseFsRoot(e.dir),
		CDIDir:      e.cdiDir,
		NodeName:    testNode,
		Client:      e.client,
		Local: dra.LocalConfig{Managed: true, UsableSize: func(string, uint64) uint64 { return 0 },
			Coherent: func(string, uint64) bool { return false }},
		Publish: e.publishConfig(),
		Scan:    func(context.Context) (*kernel.Snapshot, error) { return e.detected(), nil },
		FSTypes: api.KnownFSTypes,
		Timeouts: engine.Timeouts{Memdev: 300 * time.Millisecond, ProbeGrace: 20 * time.Millisecond, RegionRetry: 10 * time.Millisecond,
			Settle: 2 * time.Second, LocalSettle: 2 * time.Second, Restore: time.Second, Holder: 50 * time.Millisecond,
			OfflineSettle: 20 * time.Millisecond, Offline: time.Second},
	})
	noErr(t, err)
	e.local, e.pool = e.node.Local, e.node.Pool
	_, err = e.node.Recover(ctx)
	noErr(t, err)

	n := 0
	opts := leases.Options{
		Store:           st,
		Client:          e.client,
		NodeName:        testNode,
		DataDir:         e.dir,
		CLI:             e.cli,
		WaitDefault:     5 * time.Second,
		PoolDriver:      device.PoolDriverName,
		MountPoints:     func() ([]string, error) { return mountPoints(e.k.Kernel()) },
		OpenDevice:      e.k.Kernel().OpenDevice,
		PublishedDevice: publishedDevice(e.node),
		Timeouts:        leases.Timeouts{Poll: 5 * time.Millisecond, Reconcile: time.Hour, Close: 2 * time.Second},
		NewID:           func() string { n++; return fmt.Sprintf("%08x", 0xa0+n) },
	}
	if e.opts != nil {
		e.opts(&opts)
	}
	e.b, err = leases.New(opts)
	noErr(t, err)
	e.local.SetLeases(e.b)
	noErr(t, e.node.WriteCDI())
}

// start starts the broker with the env's NRI plugin and makes it the
// lender of both drivers.
func (e *leaseEnv) start() {
	e.startWith(e.ctrs)
}

// startWith starts the broker with the NRI plugin c (nil: NRI is not
// connected).
func (e *leaseEnv) startWith(c leases.Containers) {
	var nodes dra.NUMALender
	if c != nil {
		nodes = c
	}
	e.node.SetLenders(nodes, e.b)
	e.b.Start(c)
}

// restart stops the broker and the drivers and boots them again on the
// same kernel and files.
func (e *leaseEnv) restart() {
	e.t.Helper()
	e.b.Stop()
	_ = e.st.Close()
	e.boot()
}

// reconcile restarts the broker and the drivers, starts the broker, and
// waits until the broker reconciled the leases once.
func (e *leaseEnv) reconcile() {
	e.t.Helper()
	e.restart()
	restored := make(chan string, 1)
	e.ctrs.mu.Lock()
	e.ctrs.restored = restored
	e.ctrs.mu.Unlock()
	e.start()
	select {
	case <-restored:
	case <-time.After(5 * time.Second):
		e.t.Fatal("the broker did not reconcile the leases")
	}
}

// rescan scans the node again.
func (e *leaseEnv) rescan() {
	e.node.Publisher.Rescan(context.Background(), false)
}

// setShared configures the local memdev with the serial shared, restarts
// the drivers and starts the broker again.
func (e *leaseEnv) setShared(serial uint64) {
	e.mu.Lock()
	e.shared[serial] = true
	e.mu.Unlock()
	e.restart()
	e.start()
}

// publishConfig returns the configuration of the scans.
func (e *leaseEnv) publishConfig() dra.PublishConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	var shared []string
	for _, s := range slices.Sorted(maps.Keys(e.shared)) {
		shared = append(shared, api.FormatSerial(s))
	}
	specs, err := dra.ParseMemdevSpecs(shared)
	noErr(e.t, err)
	return dra.PublishConfig{ManageMemdevs: true, PublishLeaseDevice: true, SharedDevices: specs}
}

// detected returns the observation of the node that a scan makes.
func (e *leaseEnv) detected() *kernel.Snapshot {
	d := &kernel.Snapshot{NUMANodes: []kernel.NUMANode{{ID: 0, HasMemory: true, MemTotal: 8 << 30}}}
	for _, s := range slices.Sorted(maps.Keys(e.memdevs)) {
		d.Memdevs = append(d.Memdevs, e.memdevs[s])
	}
	return d
}

// check fails the test on a violation of the invariants of the records and
// the kernel now (Engine.Check, with state.Check).
func (e *leaseEnv) check(what string) {
	e.t.Helper()
	snap, err := e.node.Engine.Observe(context.Background())
	noErr(e.t, err)
	for _, v := range e.node.Engine.Check(snap, []state.Source{state.SourcePool, state.SourceLocal}, nil) {
		e.t.Errorf("%s: %s", what, v)
	}
}

// unprepareDeleted is the fake client's reactor of a deleted claim: a
// prepared companion claim is unprepared by its plugin first, and kept
// while that fails.
func (e *leaseEnv) unprepareDeleted(a k8stesting.Action) (bool, runtime.Object, error) {
	name := a.(k8stesting.DeleteAction).GetName()
	e.mu.Lock()
	driver := e.prepared[name]
	e.mu.Unlock()
	p := e.pool
	switch driver {
	case "":
		return false, nil, nil
	case device.DriverName:
		p = e.local
	}
	res, err := p.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{{UID: types.UID("uid-" + name)}})
	if err != nil || res[types.UID("uid-"+name)] != nil {
		return true, nil, nil // kept until the next try
	}
	e.mu.Lock()
	delete(e.prepared, name)
	e.mu.Unlock()
	return false, nil, nil
}

// prepare prepares a companion claim through the plugin as kubelet does and
// returns the result; a prepared claim is unprepared by the fake client
// when it is deleted.
func (e *leaseEnv) prepare(p *dra.Plugin, claim *resourcev1.ResourceClaim) kubeletplugin.PrepareResult {
	e.t.Helper()
	res, err := p.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
	noErr(e.t, err)
	r := res[claim.UID]
	if r.Err == nil {
		e.mu.Lock()
		e.prepared[claim.Name] = p.Name()
		e.mu.Unlock()
	}
	e.check(p.Name() + ": prepared " + claim.Name)
	return r
}

// unprepare unprepares a claim through the plugin as kubelet does.
func (e *leaseEnv) unprepare(p *dra.Plugin, uid string) error {
	e.t.Helper()
	res, err := p.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{{UID: types.UID(uid)}})
	noErr(e.t, err)
	if res[types.UID(uid)] == nil {
		e.mu.Lock()
		for name, driver := range e.prepared {
			if driver == p.Name() && "uid-"+name == uid {
				delete(e.prepared, name)
			}
		}
		e.mu.Unlock()
	}
	e.check(p.Name() + ": unprepared " + uid)
	return res[types.UID(uid)]
}

// openClaim opens the env's lease claim.
func (e *leaseEnv) openClaim() {
	e.t.Helper()
	noErr(e.t, e.b.OpenClaim(leaseClaimObj()))
}

// sock returns the path of the socket of the env's lease claim.
func (e *leaseEnv) sock() string {
	return filepath.Join(e.dir, "leases", envLeaseClaim, "cxl.sock")
}

// call sends one request and returns the response and the received fd
// (-1 for none).
func (e *leaseEnv) call(req api.Request) (api.Response, int) {
	e.t.Helper()
	data, _ := json.Marshal(req)
	return e.callRaw(string(data))
}

// callRaw sends one request line and returns the response and the
// received fd (-1 for none).
func (e *leaseEnv) callRaw(req string) (api.Response, int) {
	e.t.Helper()
	return e.callRawAt(e.sock(), req)
}

// callRawAt sends one request line to the socket sock and returns the
// response and the received fd (-1 for none).
func (e *leaseEnv) callRawAt(sock, req string) (api.Response, int) {
	e.t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	noErr(e.t, err)
	defer func() { _ = c.Close() }()
	_, err = c.Write([]byte(req + "\n"))
	noErr(e.t, err)
	buf := make([]byte, 64*1024)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	noErr(e.t, err)
	fd := -1
	if oobn > 0 {
		msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
		noErr(e.t, err)
		fds, err := syscall.ParseUnixRights(&msgs[0])
		noErr(e.t, err)
		fd = fds[0]
	}
	line := buf[:n]
	if !strings.Contains(string(line), "\n") {
		rest, err := bufio.NewReader(c).ReadBytes('\n')
		noErr(e.t, err)
		line = append(line, rest...)
	}
	var resp api.Response
	noErr(e.t, json.Unmarshal(line, &resp))
	return resp, fd
}

// async runs call in the background; the fd of the response is closed.
func (e *leaseEnv) async(req api.Request) chan api.Response {
	ch := make(chan api.Response, 1)
	go func() {
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: e.sock(), Net: "unix"})
		if err != nil {
			ch <- api.Response{Error: err.Error()}
			return
		}
		defer func() { _ = c.Close() }()
		data, _ := json.Marshal(req)
		_, _ = c.Write(append(data, '\n'))
		line, err := bufio.NewReader(c).ReadBytes('\n')
		var resp api.Response
		if err == nil {
			err = json.Unmarshal(line, &resp)
		}
		if err != nil {
			resp.Error = err.Error()
		}
		ch <- resp
	}()
	return ch
}

// callFd runs call in the background and returns a channel of its
// response with its fd.
func (e *leaseEnv) callFd(req api.Request) chan fdResponse {
	ch := make(chan fdResponse, 1)
	go func() {
		r, fd := e.call(req)
		ch <- fdResponse{r, fd}
	}()
	return ch
}

// fdResponse is a response with its fd (-1 for none).
type fdResponse struct {
	resp api.Response
	fd   int
}

// waitClaim waits for the companion claim and pod of the name and returns
// the claim.
func (e *leaseEnv) waitClaim(name string) *resourcev1.ResourceClaim {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := e.client.ResourceV1().ResourceClaims(testNS).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			// the pod is created right after the claim
			if _, err := e.client.CoreV1().Pods(testNS).Get(context.Background(), name, metav1.GetOptions{}); err == nil {
				return c
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("claim %s not created", name)
	return nil
}

// wantGone fails the test unless the companion pod and claim of the name
// are gone.
func (e *leaseEnv) wantGone(name string) {
	e.t.Helper()
	_, err := e.client.CoreV1().Pods(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		e.t.Fatalf("companion pod %s still exists (%v)", name, err)
	}
	_, err = e.client.ResourceV1().ResourceClaims(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		e.t.Fatalf("companion claim %s still exists (%v)", name, err)
	}
}

// waitGoneDir waits until the directory is removed.
func (e *leaseEnv) waitGoneDir(dir string) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := os.Stat(dir)
	e.t.Fatalf("directory %s left: %v", dir, err)
}

// lease returns a copy of the lease record of the store, nil for none.
func (e *leaseEnv) lease(id string) *state.Lease {
	if l := e.st.Snapshot().Lease(id); l != nil {
		c := l.Clone()
		return &c
	}
	return nil
}

// stored returns the lease record of the store, nil for none.
func (e *leaseEnv) stored(id string) *state.Lease {
	return e.st.Snapshot().Lease(id)
}

// claim returns a copy of the lease claim record of the store, nil for
// none.
func (e *leaseEnv) claim(uid string) *state.LeaseClaim {
	if c := e.st.Snapshot().LeaseClaim(uid); c != nil {
		cp := c.Clone()
		return &cp
	}
	return nil
}

// numLeases returns the number of leases of the store.
func (e *leaseEnv) numLeases() int {
	return len(e.st.Snapshot().Leases)
}

// waitState waits until the lease is in the state (NONE: gone).
func (e *leaseEnv) waitState(id string, want state.LeaseState) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l := e.lease(id)
		if (l == nil && want == state.LeaseNone) || (l != nil && l.State == want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("lease %s not %s: %+v", id, want, e.lease(id))
}

// wantReleaseAll releases all leases of the pod and checks that the n
// leases are gone.
func (e *leaseEnv) wantReleaseAll(n int) {
	e.t.Helper()
	rel, _ := e.call(api.Request{Op: api.OpRelease, All: true})
	if rel.Leases == nil || len(*rel.Leases) != n {
		e.t.Fatalf("release all %+v", rel)
	}
	for _, l := range *rel.Leases {
		if l.State != api.StateReleased {
			e.t.Fatalf("release all: %+v", l)
		}
	}
}

// allocatedPool returns a companion claim as the scheduler and the pool
// controller leave it: allocated the pool device dev, the configs copied
// from the spec, the device data in the status.
func allocatedPool(c *resourcev1.ResourceClaim, dev, data string) *resourcev1.ResourceClaim {
	c = c.DeepCopy()
	c.UID = types.UID("uid-" + c.Name)
	request := c.Spec.Devices.Requests[0].Name
	if len(c.Spec.Devices.Requests) > 0 && len(c.Spec.Devices.Requests[0].FirstAvailable) > 0 {
		request += "/" + api.SourcePool
	}
	c.Status.Allocation = &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{
		Results: []resourcev1.DeviceRequestAllocationResult{{Request: request, Driver: device.PoolDriverName, Pool: envPool, Device: dev,
			ConsumedCapacity: poolConsumed(data)}},
		Config: allocationConfigs(c, request),
	}}
	c.Status.Devices = []resourcev1.AllocatedDeviceStatus{{Driver: device.PoolDriverName, Pool: envPool, Device: dev,
		Data: &runtime.RawExtension{Raw: []byte(data)}}}
	return c
}

// allocatedLocal returns a companion claim as the scheduler leaves it when
// it allocated the local memdev dev to its request: the configs of the
// request copied from the spec, the capacity of the request consumed.
func allocatedLocal(c *resourcev1.ResourceClaim, dev string) *resourcev1.ResourceClaim {
	c = c.DeepCopy()
	c.UID = types.UID("uid-" + c.Name)
	request, capacity := c.Spec.Devices.Requests[0].Name, (*resourcev1.CapacityRequirements)(nil)
	if r := c.Spec.Devices.Requests[0]; r.Exactly != nil {
		capacity = r.Exactly.Capacity
	} else {
		request += "/" + api.SourceLocal
		capacity = r.FirstAvailable[0].Capacity
	}
	result := resourcev1.DeviceRequestAllocationResult{Request: request, Driver: device.DriverName, Pool: testNode, Device: dev}
	if capacity != nil {
		result.ConsumedCapacity = maps.Clone(capacity.Requests)
	}
	c.Status.Allocation = &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{
		Results: []resourcev1.DeviceRequestAllocationResult{result},
		Config:  allocationConfigs(c, request),
	}}
	return c
}

// allocationConfigs returns the configs of the spec of a claim that apply
// to the request, as the allocation copies them.
func allocationConfigs(c *resourcev1.ResourceClaim, request string) []resourcev1.DeviceAllocationConfiguration {
	var cfgs []resourcev1.DeviceAllocationConfiguration
	for _, sc := range c.Spec.Devices.Config {
		if len(sc.Requests) == 0 || slices.Contains(sc.Requests, request) {
			cfgs = append(cfgs, resourcev1.DeviceAllocationConfiguration{Source: resourcev1.AllocationConfigSourceClaim,
				Requests: sc.Requests, DeviceConfiguration: sc.DeviceConfiguration})
		}
	}
	return cfgs
}

// poolConsumed returns the consumed capacity of an allocation result of a
// pool device with the status data: of a shared device all of its shares
// (exclusive) or one, nil otherwise.
func poolConsumed(data string) map[resourcev1.QualifiedName]resource.Quantity {
	var d struct{ Shared, Exclusive bool }
	if err := json.Unmarshal([]byte(data), &d); err != nil || !d.Shared {
		return nil
	}
	if d.Exclusive {
		return map[resourcev1.QualifiedName]resource.Quantity{device.CapacityShares: *resource.NewQuantity(device.SharesUnlimited, resource.DecimalSI)}
	}
	return map[resourcev1.QualifiedName]resource.Quantity{device.CapacityShares: resource.MustParse("1")}
}

// formatPmem makes the fsdax namespace of the persistent memdev with the
// serial and a filesystem of the type on it ("": none), as a writer on
// another host leaves it (no region), and returns its block device.
func (e *leaseEnv) formatPmem(serial uint64, typ string) string {
	e.t.Helper()
	k := e.k.Kernel()
	memdev := e.k.Memdev(serial)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := k.RootDecoder(memdev, kernel.RegionPmem)
	noErr(e.t, err)
	region, err := k.CreateRegion(ctx, kernel.CreateRegionRequest{Memdev: memdev, Decoder: d, Type: kernel.RegionPmem})
	noErr(e.t, err)
	nd, err := k.WaitNdRegion(ctx, region)
	noErr(e.t, err)
	ns, err := k.CreateFsdaxNamespace(ctx, nd, "")
	noErr(e.t, err)
	if typ != "" {
		e.k.SetImage(ns.BlockDevice, fake.FSImage(typ))
	}
	noErr(e.t, k.DestroyRegion(ctx, region))
	e.pmem = ns.BlockDevice
	return ns.BlockDevice
}

// mounted returns the mount at target, nil if there is none.
func (e *leaseEnv) mounted(target string) *kernel.MountInfo {
	mounts, err := e.k.Kernel().Mounts()
	noErr(e.t, err)
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

// nodeMount returns the node mount of the persistent pool device with the
// serial and the access.
func (e *leaseEnv) nodeMount(serial uint64, access string) string {
	return filepath.Join(e.poolDir, "mounts", fmt.Sprintf("%x-%s", serial, access))
}

// cdiSpec returns the CDI spec of the name on disk, nil if there is none.
func (e *leaseEnv) cdiSpec(name string) *cdiSpecs.Spec {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.cdiDir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	noErr(e.t, err)
	spec := &cdiSpecs.Spec{}
	noErr(e.t, json.Unmarshal(data, spec))
	return spec
}

// wantNoCDIDevices fails the test when a prepared companion claim of one
// device has a CDI device, or the CDI spec of a driver has a device of a
// claim.
func (e *leaseEnv) wantNoCDIDevices(r kubeletplugin.PrepareResult) {
	e.t.Helper()
	if len(r.Devices) != 1 || len(r.Devices[0].CDIDeviceIDs) != 0 {
		e.t.Fatalf("CDI devices of a companion claim: %+v", r.Devices)
	}
	for _, name := range []string{device.ClaimsCDISpecName, device.PoolCDISpecName} {
		if spec := e.cdiSpec(name); spec != nil && slices.ContainsFunc(spec.Devices, func(d cdiSpecs.Device) bool {
			return strings.HasPrefix(d.Name, "claim-")
		}) {
			e.t.Fatalf("CDI spec %s of a companion claim: %+v", name, spec.Devices)
		}
	}
}

// wantClean fails the test unless the drivers hold nothing: no preparation,
// grant, memdev or region record, no region in the kernel, no mount under
// the plugin data directories, no violation of the mount rules of the
// fake.
func (e *leaseEnv) wantClean() {
	e.t.Helper()
	r := &e.st.Snapshot().Records
	if len(r.Preparations)+len(r.Grants)+len(r.Memdevs)+len(r.Regions) > 0 {
		e.t.Fatalf("records left: preparations %+v grants %+v memdevs %+v regions %+v", r.Preparations, r.Grants, r.Memdevs, r.Regions)
	}
	e.wantNoRegions()
	mounts, err := e.k.Kernel().Mounts()
	noErr(e.t, err)
	for _, m := range mounts {
		if strings.HasPrefix(m.Target, e.dir+"/") || strings.HasPrefix(m.Target, e.poolDir+"/") {
			e.t.Fatalf("mounted: %+v", m)
		}
	}
	if v := e.k.Violations(); len(v) > 0 {
		e.t.Fatalf("mount rules violated: %v", v)
	}
}

// onlineBlocks returns the number of the memory blocks that are online.
func (e *leaseEnv) onlineBlocks() int {
	snap, err := e.k.Kernel().Observe(context.Background())
	noErr(e.t, err)
	n := 0
	for _, b := range snap.Blocks {
		if b.State == kernel.BlockOnline {
			n++
		}
	}
	return n
}

// regions returns the regions of the kernel.
func (e *leaseEnv) regions() []string {
	snap, err := e.k.Kernel().Observe(context.Background())
	noErr(e.t, err)
	var out []string
	for _, ri := range snap.Regions {
		out = append(out, ri.Name)
	}
	return out
}

// wantNoRegions fails the test when a region is left.
func (e *leaseEnv) wantNoRegions() {
	e.t.Helper()
	if r := e.regions(); len(r) != 0 {
		e.t.Fatalf("regions left: %v", r)
	}
}

// sleep waits a poll interval of the tests.
func (e *leaseEnv) sleep() {
	time.Sleep(5 * time.Millisecond)
}

// waitStored waits until the stored lease satisfies cond.
func (e *leaseEnv) waitStored(id string, cond func(l *state.Lease) bool) {
	e.t.Helper()
	for i := 0; i < 1000; i++ {
		if l := e.stored(id); l != nil && cond(l) {
			return
		}
		e.sleep()
	}
	e.t.Fatalf("lease %s: %+v", id, e.stored(id))
}

// poolSlice is the pool controller's ResourceSlice with an exclusive
// device pooled0 and the shared devices shared0 and shared1.
func poolSlice() *resourcev1.ResourceSlice {
	attrs := func(serial string, shared bool) map[resourcev1.QualifiedName]resourcev1.DeviceAttribute {
		return map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type": {StringValue: ptr("memdev")}, "source": {StringValue: ptr("pool")}, "serial": {StringValue: &serial},
			"shared": {BoolValue: ptr(shared)}, "size": {IntValue: ptr(int64(1 << 28))},
		}
	}
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-cxl-pool"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   device.PoolDriverName,
			Pool:     resourcev1.ResourcePool{Name: "fake-cxl-pool", ResourceSliceCount: 1},
			AllNodes: ptr(true),
			Devices: []resourcev1.Device{
				{Name: "pooled0", Attributes: attrs("0xc1ee0001", false),
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"memory": {Value: resource.MustParse("512Mi")}}},
				{Name: "shared0", Attributes: attrs("0xC1AE0001", true),
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"shares": {Value: resource.MustParse("1M")}}},
				{Name: "shared1", Attributes: attrs("0xc1ae0002", true)},
			},
		},
	}
}

// localResourceSlice is the cxl.nri.io ResourceSlice of node1 with the local
// memdev 0xc100e2e1, the shared local memdev 0xc100e2e2 and the persistent
// local memdev 0xc100f5e1.
func localResourceSlice() *resourcev1.ResourceSlice {
	return nodeResourceSlice(testNode,
		dra.LocalMemdevDevice(dra.MemdevDeviceName(localSerial), &dra.Memdev{Serial: localSerial, Size: 1 << 30, NUMAAffinity: 0, BlockSize: 128 << 20}),
		dra.LocalMemdevDevice("memdev-c100e2e2", &dra.Memdev{Serial: serialShared, Size: 1 << 30, NUMAAffinity: 0, Shared: true}),
		persistentLocalDevice())
}

// persistentLocalDevice is the persistent local memdev 0xc100f5e1 as
// cxl.nri.io publishes it: not shared, persistent, with shares.
func persistentLocalDevice() resourcev1.Device {
	return persistentLocalDeviceOf("0xc100f5e1")
}

// persistentLocalDeviceOf is a persistent local memdev with the serial.
func persistentLocalDeviceOf(serial string) resourcev1.Device {
	return resourcev1.Device{
		Name: "memdev-" + strings.TrimPrefix(serial, "0x"),
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type": {StringValue: ptr("memdev")}, "source": {StringValue: ptr("local")}, "serial": {StringValue: ptr(serial)},
			"shared": {BoolValue: ptr(false)}, "persistent": {BoolValue: ptr(true)}, "size": {IntValue: ptr(int64(1 << 30))},
		},
		Capacity:                 map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"shares": {Value: resource.MustParse("1M")}},
		AllowMultipleAllocations: ptr(true),
	}
}

// nodeResourceSlice returns the cxl.nri.io ResourceSlice of a node with the
// devices.
func nodeResourceSlice(node string, devs ...resourcev1.Device) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-cxl"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   device.DriverName,
			Pool:     resourcev1.ResourcePool{Name: node, ResourceSliceCount: 1},
			NodeName: ptr(node),
			Devices:  devs,
		},
	}
}

// persistentPoolSlice is a ResourceSlice of the pool controller with the
// shared persistent device llm0 (0xc1af0001) and the exclusive persistent
// device llm1 (0xc1af0002).
func persistentPoolSlice() *resourcev1.ResourceSlice {
	attrs := func(serial string, shared bool) map[resourcev1.QualifiedName]resourcev1.DeviceAttribute {
		return map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type": {StringValue: ptr("memdev")}, "source": {StringValue: ptr("pool")}, "serial": {StringValue: &serial},
			"shared": {BoolValue: ptr(shared)}, "size": {IntValue: ptr(int64(1 << 29))}, "persistent": {BoolValue: ptr(true)},
		}
	}
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-cxl-pool-persistent"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   device.PoolDriverName,
			Pool:     resourcev1.ResourcePool{Name: "fake-cxl-pool-persistent", ResourceSliceCount: 1},
			AllNodes: ptr(true),
			Devices: []resourcev1.Device{
				{Name: "llm0", Attributes: attrs("0xc1af0001", true),
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"shares": {Value: resource.MustParse("1M")}}},
				{Name: "llm1", Attributes: attrs("0xc1af0002", false),
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"memory": {Value: resource.MustParse("512Mi")}}},
			},
		},
	}
}

// addSlices adds the ResourceSlices to the API. The watcher lists them, as
// at a scan.
func (e *leaseEnv) addSlices(slices ...*resourcev1.ResourceSlice) {
	e.t.Helper()
	for _, s := range slices {
		_, err := e.client.ResourceV1().ResourceSlices().Create(context.Background(), s, metav1.CreateOptions{})
		noErr(e.t, err)
	}
	e.node.Watcher.ListPoolSerials(context.Background())
}

// fakeContainers is the NRI plugin: a table of the running
// containers by pod uid and name, the peer of every connection (byPid),
// the lent nodes with their holders, the calls.
type fakeContainers struct {
	mu      sync.Mutex
	byPid   *leases.Requester // the container of every pid, nil: none
	running map[string]bool
	lent    map[string]map[int][]string // podUID/name -> node -> holders
	calls   []string
	lendErr error
	started []func(state.PodRef, string)
	// restored gets the calls so far at LendsRestored, when set.
	restored chan string
}

// newFakeContainers returns a NRI plugin with the running containers
// of refs, the first the peer of every connection.
func newFakeContainers(refs ...leases.Requester) *fakeContainers {
	f := &fakeContainers{running: map[string]bool{}, lent: map[string]map[int][]string{}}
	for i := range refs {
		f.running[refs[i].Pod.UID+"/"+refs[i].Container] = true
	}
	if len(refs) > 0 {
		r := refs[0]
		f.byPid = &r
	}
	return f
}

// ContainerByPid returns the peer.
func (f *fakeContainers) ContainerByPid(pid int) (state.PodRef, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byPid == nil {
		return state.PodRef{}, "", false
	}
	return f.byPid.Pod, f.byPid.Container, true
}

// Running tells whether the container runs.
func (f *fakeContainers) Running(podUID, container string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[podUID+"/"+container]
}

// LendNUMANode records the lend.
func (f *fakeContainers) LendNUMANode(_ context.Context, podUID, container string, numaNode int, lease string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("lend %s/%s %d", podUID, container, numaNode))
	if f.lendErr != nil {
		return f.lendErr
	}
	k := podUID + "/" + container
	if f.lent[k] == nil {
		f.lent[k] = map[int][]string{}
	}
	if !slices.Contains(f.lent[k][numaNode], lease) {
		f.lent[k][numaNode] = append(f.lent[k][numaNode], lease)
	}
	return nil
}

// RevokeNUMANode drops the holder of the node.
func (f *fakeContainers) RevokeNUMANode(_ context.Context, podUID, container string, numaNode int, lease string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("revoke %s/%s %d", podUID, container, numaNode))
	k := podUID + "/" + container
	if h := slices.DeleteFunc(f.lent[k][numaNode], func(s string) bool { return s == lease }); len(h) > 0 {
		f.lent[k][numaNode] = h
	} else {
		delete(f.lent[k], numaNode)
	}
	return nil
}

// OnContainerGone does nothing: no container of the tests stops.
func (f *fakeContainers) OnContainerGone(func(state.PodRef, string)) {}

// OnContainerStarted registers fn.
func (f *fakeContainers) OnContainerStarted(fn func(state.PodRef, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, fn)
}

// WaitSynchronized returns at once.
func (f *fakeContainers) WaitSynchronized(context.Context) error {
	return nil
}

// LendsRestored sends the calls so far to restored, when it is set.
func (f *fakeContainers) LendsRestored() {
	f.mu.Lock()
	ch, calls := f.restored, strings.Join(f.calls, "\n")
	f.mu.Unlock()
	if ch != nil {
		ch <- calls
	}
}

// lentNodes returns the nodes lent to the container, sorted.
func (f *fakeContainers) lentNodes(podUID, container string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.lent[podUID+"/"+container]))
}

// holders returns the holders of a node lent to a container.
func (f *fakeContainers) holders(podUID, container string, node int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lent[podUID+"/"+container][node])
}

// callLog returns the calls so far.
func (f *fakeContainers) callLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

// forget forgets the lends and the calls, as a restart of the container
// plugin does.
func (f *fakeContainers) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lent = map[string]map[int][]string{}
	f.calls = nil
}

// setProcess makes r the peer of every connection (nil: an unknown process).
func (f *fakeContainers) setProcess(r *leases.Requester) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPid = r
}

// setLendErr sets the error of the next lends.
func (f *fakeContainers) setLendErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lendErr = err
}
