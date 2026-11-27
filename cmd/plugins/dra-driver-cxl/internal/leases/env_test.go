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

package leases

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/lend"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/api"
	"github.com/containers/nri-plugins/pkg/cxl/device"
)

const (
	// envNode and envNS are the node and the namespace of the claims.
	envNode = "node1"
	envNS   = "demo"
	// the requester of the leases
	envPod    = "requester"
	envPodUID = "pod-uid-1"
	envCtr    = "app"
	// envLeaseClaim is the uid of the lease claim of the requester pod.
	envLeaseClaim = "lc1"

	// serials of the local memdevs of this node: unshared, shared.
	serialLocal  = 0xc100e2e1
	serialShared = 0xc100e2e2
	// serials of the pool devices: volatile memory, shared, persistent.
	serialPoolX = 0xc1ee0001
	serialPoolS = 0xc1ae0001
	serialPoolP = 0xc1af0001

	// pmemBytes are the bytes of an fsdax namespace on 512 MiB of
	// persistent memory.
	pmemBytes = 534773760
)

// requester is the container of the requester pod that the env's peers
// are in.
var requester = Requester{Pod: state.PodRef{Namespace: envNS, Name: envPod, UID: envPodUID}, Container: envCtr}

// leaseEnv is a node for the tests of the broker: a store in a temporary
// directory, the broker over it, a fake API client and a fake container
// plugin. The tests call the hooks of the companion's Prepare and
// Unprepare as the drivers do. The files, the client and the container
// plugin outlive a restart.
type leaseEnv struct {
	t *testing.T
	// dir is the plugin data directory of the broker and of the store.
	dir, cli string
	// boots is the number of reboots of the node: its boot id.
	boots int

	client *k8sfake.Clientset
	ctrs   *fakeContainers
	st     *store.Store
	b      *Broker
	// slices are the ResourceSlices of publishedDevice.
	slices []*resourcev1.ResourceSlice

	// opts change the options of the broker of the next start, nil for
	// none; started tells whether the broker was started; newState makes
	// the next start make a new document when the store is lost;
	// afterCommit is the store's AfterCommit hook of the next start; the
	// writes of the store fail while failing is set (failWrites).
	opts        func(o *Options)
	started     bool
	newState    bool
	afterCommit func()
	failing     atomic.Bool
}

// brokerEnv returns a node whose broker is not started yet (start).
func brokerEnv(t *testing.T) *leaseEnv {
	t.Helper()
	dir := t.TempDir()
	e := &leaseEnv{
		t:      t,
		dir:    filepath.Join(dir, device.DriverName),
		cli:    filepath.Join(dir, "cxl-request"),
		client: k8sfake.NewClientset(requesterPod(), leaseClaimObj()),
		ctrs:   newFakeContainers(requester),
	}
	mustOK(t, os.MkdirAll(e.dir, 0o755))
	mustOK(t, os.WriteFile(e.cli, []byte("#!/bin/true\n"), 0o755))
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

// requesterPod is the requester pod.
func requesterPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: envPod, Namespace: envNS, UID: types.UID(envPodUID)},
		Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyAlways, NodeName: envNode},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// leaseClaimObj is the prepared claim on the lease device of the requester
// pod.
func leaseClaimObj() *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lease", Namespace: envNS, UID: types.UID(envLeaseClaim)},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{
				Results: []resourcev1.DeviceRequestAllocationResult{{Request: "lease", Driver: device.DriverName, Pool: envNode, Device: device.LeaseDeviceName}},
			}},
			ReservedFor: []resourcev1.ResourceClaimConsumerReference{{Resource: "pods", Name: envPod, UID: types.UID(envPodUID)}},
		},
	}
}

// boot opens the store and makes the broker over it (not started) as the
// start of the process does.
func (e *leaseEnv) boot() {
	t := e.t
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Dir: e.dir, BootID: fmt.Sprintf("boot-%d", e.boots),
		NewState: e.newState, AfterCommit: e.afterCommit, WriteFault: e.writeFault})
	mustOK(t, err)
	e.st = st
	n := 0
	opts := Options{
		Store:       st,
		Client:      e.client,
		NodeName:    envNode,
		DataDir:     e.dir,
		CLI:         e.cli,
		WaitDefault: 5 * time.Second,
		PoolDriver:  device.PoolDriverName,
		MountPoints: func() ([]string, error) { return nil, nil },
		OpenDevice: func(context.Context, string, bool) (*os.File, error) {
			return nil, errors.New("no device nodes in the lease tests")
		},
		PublishedDevice: e.publishedDevice,
		Timeouts:        Timeouts{Poll: 5 * time.Millisecond, Reconcile: time.Hour, Close: 2 * time.Second},
		NewID:           func() string { n++; return fmt.Sprintf("%08x", 0xa0+n) },
	}
	if e.opts != nil {
		e.opts(&opts)
	}
	e.b, err = New(opts)
	mustOK(t, err)
	e.started = false
}

// start starts the broker with the env's NRI plugin.
func (e *leaseEnv) start() {
	e.startWith(e.ctrs)
}

// startWith starts the broker with the NRI plugin c (nil: NRI is not
// connected).
func (e *leaseEnv) startWith(c Containers) {
	e.b.Start(c)
	e.started = true
}

// restart stops the broker and boots it again on the same files.
func (e *leaseEnv) restart() {
	e.t.Helper()
	e.b.Stop()
	_ = e.st.Close()
	e.boot()
}

// reboot reboots the node and boots the broker again.
func (e *leaseEnv) reboot() {
	e.t.Helper()
	e.b.Stop()
	_ = e.st.Close()
	e.boots++
	e.boot()
}

// openClaim opens the env's lease claim.
func (e *leaseEnv) openClaim() {
	e.t.Helper()
	mustOK(e.t, e.b.OpenClaim(leaseClaimObj()))
}

// sock returns the path of the socket of the env's lease claim.
func (e *leaseEnv) sock() string {
	return filepath.Join(e.dir, leaseDirName, envLeaseClaim, leaseSocketName)
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

// callAt sends one request to the socket of the lease claim uid and returns
// the response.
func (e *leaseEnv) callAt(uid string, req api.Request) api.Response {
	e.t.Helper()
	data, _ := json.Marshal(req)
	resp, _ := e.callRawAt(filepath.Join(e.dir, leaseDirName, uid, leaseSocketName), string(data))
	return resp
}

// callRawAt sends one request line to the socket sock and returns the
// response and the received fd (-1 for none).
func (e *leaseEnv) callRawAt(sock, req string) (api.Response, int) {
	e.t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	mustOK(e.t, err)
	defer func() { _ = c.Close() }()
	_, err = c.Write([]byte(req + "\n"))
	mustOK(e.t, err)
	buf := make([]byte, 64*1024)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	mustOK(e.t, err)
	fd := -1
	if oobn > 0 {
		msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
		mustOK(e.t, err)
		fds, err := syscall.ParseUnixRights(&msgs[0])
		mustOK(e.t, err)
		fd = fds[0]
	}
	line := buf[:n]
	if !strings.Contains(string(line), "\n") {
		rest, err := bufio.NewReader(c).ReadBytes('\n')
		mustOK(e.t, err)
		line = append(line, rest...)
	}
	var resp api.Response
	mustOK(e.t, json.Unmarshal(line, &resp))
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

// waitClaim waits for the companion claim and pod of the name and returns
// the claim.
func (e *leaseEnv) waitClaim(name string) *resourcev1.ResourceClaim {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := e.client.ResourceV1().ResourceClaims(envNS).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			// the pod is created right after the claim
			if _, err := e.client.CoreV1().Pods(envNS).Get(context.Background(), name, metav1.GetOptions{}); err == nil {
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
	_, err := e.client.CoreV1().Pods(envNS).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		e.t.Fatalf("companion pod %s still exists (%v)", name, err)
	}
	_, err = e.client.ResourceV1().ResourceClaims(envNS).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		e.t.Fatalf("companion claim %s still exists (%v)", name, err)
	}
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

// edit commits fn, a change of the records of the store.
func (e *leaseEnv) edit(fn func(r *state.Records)) {
	e.t.Helper()
	mustOK(e.t, e.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		fn(&doc.Records)
		return nil
	}))
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

// companionClaim returns the companion claim of the lease id as the broker
// creates it: name, namespace, label and owner.
func companionClaim(id string) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{
		Name:            CompanionName(id),
		Namespace:       envNS,
		UID:             types.UID("uid-" + CompanionName(id)),
		Labels:          map[string]string{device.LeaseLabel: id},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: envPod, UID: envPodUID, Controller: ptr(true)}},
	}}
}

// sleep waits a poll interval of the tests.
func (e *leaseEnv) sleep() {
	time.Sleep(5 * time.Millisecond)
}

// mustOK fails the test on an error.
func mustOK(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// wantErrText fails the test unless err has the text substr.
func wantErrText(t testing.TB, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected an error with %q, got %v", substr, err)
	}
}

// fakeContainers is the NRI plugin: a table of the running
// containers by pod uid and name, the peer of every connection (byPid),
// the lent nodes with their holders, the calls.
type fakeContainers struct {
	mu      sync.Mutex
	byPid   *Requester // the container of every pid, nil: none
	running map[string]bool
	lent    map[string]map[int][]string // podUID/name -> node -> holders
	calls   []string
	lendErr error
	gone    []func(state.PodRef, string)
	started []func(state.PodRef, string)
	// restored gets the calls so far at LendsRestored, when set.
	restored chan string
	// onRevoke is called at every revoke, nil for nothing.
	onRevoke func(podUID, container string, node int, holder string)
}

var (
	_ Containers       = (*fakeContainers)(nil)
	_ lend.LeaseHooks  = (*Broker)(nil)
	_ lend.LeaseClaims = (*Broker)(nil)
)

// newFakeContainers returns a NRI plugin with the running containers
// of refs, the first the peer of every connection.
func newFakeContainers(refs ...Requester) *fakeContainers {
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
	on := f.onRevoke
	f.mu.Unlock()
	if on != nil {
		on(podUID, container, numaNode, lease)
	}
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

// OnContainerGone registers fn.
func (f *fakeContainers) OnContainerGone(fn func(state.PodRef, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gone = append(f.gone, fn)
}

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
func (f *fakeContainers) setProcess(r *Requester) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPid = r
}

// setRunning adds or removes a running container.
func (f *fakeContainers) setRunning(podUID, container string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if running {
		f.running[podUID+"/"+container] = true
	} else {
		delete(f.running, podUID+"/"+container)
	}
}

// setOnRevoke sets the function called at every revoke.
func (f *fakeContainers) setOnRevoke(fn func(podUID, container string, node int, holder string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRevoke = fn
}
