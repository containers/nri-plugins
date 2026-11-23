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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/client"
)

// testPoolName is the name of the fake pool server, the pool name of its
// controller.
const testPoolName = "fake-cxl-pool"

// fakePool is an in-memory fake-cxl-pool server.
type fakePool struct {
	mu      sync.Mutex
	hosts   []api.Host
	devices map[string]api.Device
	atts    map[string]api.Attachment
	// attachErr: device -> error code returned by attach
	attachErr map[string]string
	// detachHold: detach leaves the attachment detaching and returns 409
	detachHold bool
	// attachHook, when set, is called with each attach request and may
	// return an error code to answer with
	attachHook func(name string, req api.AttachRequest) string
	attaches   []string // "device@host owner"
	attachReqs []api.AttachRequest
	detaches   []string // "device@host"
	counter    int
	// name is the name in GET /status, statuses counts the requests
	name     string
	statuses int
}

func newFakePool() *fakePool {
	return &fakePool{
		name: testPoolName,
		hosts: []api.Host{
			{Name: "vm1", UUID: "b6a78455-edb1-558d-8cf1-01a259f76e97", State: api.HostRunning},
			{Name: "node2", UUID: "", State: api.HostRunning},
			{Name: "foreign", UUID: "11111111-2222-3333-4444-555555555555", State: api.HostRunning},
		},
		devices: map[string]api.Device{
			"pooled0":     {Name: "pooled0", Serial: "0xC1EE0001", Size: 512 << 20, Scope: api.ScopePool, State: api.DeviceFree},
			"shared0":     {Name: "shared0", Serial: "0xc1ae0001", Size: 256 << 20, Shared: true, Scope: api.ScopePool, State: api.DeviceFree},
			"bad0":        {Name: "bad0", Serial: "0xc1ee0002", Size: 256 << 20, Scope: api.ScopePool, State: api.DeviceError},
			"weird.name_": {Name: "weird.name_", Serial: "0xc1ee0003", Size: 256 << 20, Scope: api.ScopePool, State: api.DeviceFree},
			"vm1.memdev0": {Name: "vm1.memdev0", Serial: "0xc100e2e0", Size: 256 << 20, Scope: api.ScopeLocal, State: api.DeviceFree},
		},
		atts:      map[string]api.Attachment{},
		attachErr: map[string]string{},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *api.Error) {
	writeJSON(w, e.HTTPStatus(), e)
}

func (p *fakePool) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(api.RouteStatus, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.statuses++
		writeJSON(w, 200, api.Status{Name: p.name})
	})
	mux.HandleFunc(api.RouteHosts, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		writeJSON(w, 200, p.hosts)
	})
	mux.HandleFunc(api.RouteDevices, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		scope := r.URL.Query().Get("scope")
		out := []api.Device{}
		for _, d := range p.devices {
			if scope == "" || d.Scope == scope {
				out = append(out, d)
			}
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc(api.RouteAttachments, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		out := []api.Attachment{}
		for _, a := range p.atts {
			out = append(out, a)
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc(api.RouteDeviceAttach, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var req api.AttachRequest
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			writeErr(w, api.InvalidArgument("%v", err))
			return
		}
		name := r.PathValue("name")
		d, ok := p.devices[name]
		if !ok {
			writeErr(w, api.NotFound("device %q not found", name))
			return
		}
		call := api.AttachmentID(name, req.Host) + " " + req.Owner
		if req.SingleHost {
			call += " exclusive"
		}
		p.attaches = append(p.attaches, call)
		p.attachReqs = append(p.attachReqs, req)
		if code := p.attachErr[name]; code != "" {
			writeErr(w, &api.Error{Code: code, Message: "injected " + code})
			return
		}
		if p.attachHook != nil {
			if code := p.attachHook(name, req); code != "" {
				writeErr(w, &api.Error{Code: code, Message: "injected " + code})
				return
			}
		}
		id := api.AttachmentID(name, req.Host)
		for _, a := range p.atts {
			switch {
			case a.Device != name || a.Host == req.Host:
			case !d.Shared:
				writeErr(w, api.Conflict("device %q is not shared and it is attached to %q", name, a.Host))
				return
			case req.SingleHost || a.SingleHost:
				writeErr(w, api.Conflict("device %q has a single-host attachment to %q", name, a.Host))
				return
			}
		}
		if a, ok := p.atts[id]; ok {
			a.SingleHost = req.SingleHost
			if a.Owner == "" {
				a.Owner = req.Owner // taken over
			}
			p.atts[id] = a
			writeJSON(w, 200, a)
			return
		}
		p.counter++
		bus := fmt.Sprintf("ds%d", p.counter)
		if req.Slot != "" {
			bus = req.Slot
		}
		a := api.Attachment{
			ID: id, Device: name, Host: req.Host, Serial: d.Serial, Owner: req.Owner,
			Slot:         api.Slot{Bus: bus},
			QemuDeviceID: fmt.Sprintf("fcp_%s.hp%d", name, p.counter),
			State:        api.AttachmentAttached,
			SingleHost:   req.SingleHost,
		}
		p.atts[id] = a
		writeJSON(w, 201, a)
	})
	mux.HandleFunc(api.RouteDeviceDetach, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		id := api.AttachmentID(r.PathValue("name"), r.PathValue("host"))
		a, ok := p.atts[id]
		if !ok {
			writeErr(w, api.NotFound("not attached"))
			return
		}
		p.detaches = append(p.detaches, id)
		if p.detachHold {
			a.State = api.AttachmentDetaching
			p.atts[id] = a
			writeJSON(w, 409, &api.Error{Code: api.CodeConflict, Message: "guest did not release", Object: a})
			return
		}
		delete(p.atts, id)
		a.State = api.AttachmentDetached
		writeJSON(w, 200, a)
	})
	return mux
}

func (p *fakePool) addAttachment(a api.Attachment) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.ID == "" {
		a.ID = api.AttachmentID(a.Device, a.Host)
	}
	if a.State == "" {
		a.State = api.AttachmentAttached
	}
	p.atts[a.ID] = a
}

func (p *fakePool) calls() (attaches, detaches []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.attaches...), append([]string(nil), p.detaches...)
}

func (p *fakePool) attachmentIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for id := range p.atts {
		ids = append(ids, id)
	}
	return ids
}

type env struct {
	t    *testing.T
	ctx  context.Context
	pool *fakePool
	kube *fake.Clientset
	rec  *record.FakeRecorder
	c    *Controller
	logs *lockedBuf
}

// lockedBuf is a log buffer that tests read while the controller writes.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(b)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// node returns a Node whose kubernetes.io/hostname label is "h-<name>".
func node(name, uuid string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{corev1.LabelHostname: "h-" + name}},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: uuid}},
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith is newEnv with the fake pool and the controller config
// modified by setup before the controller resolves its pool name.
func newEnvWith(t *testing.T, setup func(*fakePool, *Config)) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := newFakePool()
	hs := httptest.NewServer(p.handler())
	t.Cleanup(hs.Close)
	kube := fake.NewClientset(
		// node1: vm1 by uuid (upper case, no dashes); node2: by name; node3: no host
		node("node1", "B6A78455EDB1558D8CF101A259F76E97"),
		node("node2", "00000000-0000-0000-0000-000000000002"),
		node("node3", "00000000-0000-0000-0000-000000000003"),
	)
	// the API server names a slice after its GenerateName, the fake does not
	var slices atomic.Int32
	kube.PrependReactor("create", "resourceslices", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if rs := a.(k8stesting.CreateAction).GetObject().(*resourceapi.ResourceSlice); rs.Name == "" {
			rs.Name = rs.GenerateName + strconv.Itoa(int(slices.Add(1)))
		}
		return false, nil, nil
	})
	rec := record.NewFakeRecorder(100)
	logs := &lockedBuf{}
	cfg := Config{
		Logger:  log.New(io.MultiWriter(testWriter{t}, logs), "controller: ", 0),
		Verbose: true,
	}
	if setup != nil {
		setup(p, &cfg)
	}
	c := New(cfg, kube, client.New(hs.URL), rec)
	if _, err := c.resolvePoolName(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.resourceSlices != nil {
			c.resourceSlices.Stop()
		}
		cancel()
		c.factory.Shutdown()
	})
	return &env{t: t, ctx: ctx, pool: p, kube: kube, rec: rec, c: c, logs: logs}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

func (e *env) eventually(what string, f func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sync runs one mapNodes + publish + reconcile.
func (e *env) sync() {
	e.t.Helper()
	hm, err := e.c.mapNodes(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.publish(e.ctx, hm); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.reconcile(e.ctx, hm); err != nil {
		e.t.Fatal(err)
	}
}

// createClaim creates a claim allocated with devName on node and waits
// until the lister has it.
func (e *env) createClaim(name, devName, nodeName string, shareID string) *resourceapi.ResourceClaim {
	e.t.Helper()
	return e.createClaimShares(name, devName, nodeName, shareID, 0)
}

// createClaimShares is createClaim with the shares capacity consumed by a
// shared device (0: no consumed capacity in the result).
func (e *env) createClaimShares(name, devName, nodeName string, shareID string, shares int64) *resourceapi.ResourceClaim {
	e.t.Helper()
	return e.createClaimConfig(name, devName, nodeName, shareID, shares, nil)
}

// createClaimConfig is createClaimShares with the config entries of the
// allocation.
func (e *env) createClaimConfig(name, devName, nodeName string, shareID string, shares int64, config []resourceapi.DeviceAllocationConfiguration) *resourceapi.ResourceClaim {
	e.t.Helper()
	r := resourceapi.DeviceRequestAllocationResult{
		Request: "mem", Driver: device.PoolDriverName, Pool: testPoolName, Device: devName,
		BindingConditions:        []string{condAttached},
		BindingFailureConditions: []string{condFailed},
	}
	if shareID != "" {
		r.ShareID = ptr.To(types.UID(shareID))
	}
	if shares > 0 {
		r.ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{device.CapacityShares: *resource.NewQuantity(shares, resource.DecimalSI)}
	}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-" + name)},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{
					// a result of another driver is ignored
					{Request: "local", Driver: "cxl.nri.io", Pool: "node1", Device: "region0-numa2"},
					r,
				}, Config: config},
				NodeSelector: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{nodeName}}},
				}}},
			},
		},
	}
	claim, err := e.kube.ResourceV1().ResourceClaims("default").Create(e.ctx, claim, metav1.CreateOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.waitLister(name, func(c *resourceapi.ResourceClaim) bool { return c.Status.Allocation != nil })
	return claim
}

// waitLister waits until the lister has the claim and f(claim) is true,
// or, with f == nil, until the claim is gone.
func (e *env) waitLister(name string, f func(*resourceapi.ResourceClaim) bool) {
	e.t.Helper()
	e.eventually("lister: claim "+name, func() bool {
		c, err := e.c.claims.ResourceClaims("default").Get(name)
		if f == nil {
			return err != nil
		}
		return err == nil && f(c)
	})
}

// waitStatus waits until the lister sees n status entries of the claim.
func (e *env) waitStatus(name string, n int) {
	e.t.Helper()
	e.waitLister(name, func(c *resourceapi.ResourceClaim) bool { return len(c.Status.Devices) == n })
}

func (e *env) deallocate(name string) {
	e.t.Helper()
	c := e.claim(name)
	c.Status.Allocation = nil
	c.Status.Devices = nil
	if _, err := e.kube.ResourceV1().ResourceClaims("default").UpdateStatus(e.ctx, c, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
	e.waitLister(name, func(c *resourceapi.ResourceClaim) bool { return c.Status.Allocation == nil })
}

func (e *env) deleteClaim(name string) {
	e.t.Helper()
	if err := e.kube.ResourceV1().ResourceClaims("default").Delete(e.ctx, name, metav1.DeleteOptions{}); err != nil {
		e.t.Fatal(err)
	}
	e.waitLister(name, nil)
}

func (e *env) claim(name string) *resourceapi.ResourceClaim {
	e.t.Helper()
	c, err := e.kube.ResourceV1().ResourceClaims("default").Get(e.ctx, name, metav1.GetOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// markAttached writes an Attached condition into the status entry of the
// pool result of a claim, as the controller writes it after an attach.
func (e *env) markAttached(name string) {
	e.t.Helper()
	c := e.claim(name)
	r := c.Status.Allocation.Devices.Results[1]
	st := resourceapi.AllocatedDeviceStatus{Driver: r.Driver, Pool: r.Pool, Device: r.Device, Conditions: []metav1.Condition{{
		Type: condAttached, Status: metav1.ConditionTrue, Reason: ReasonAttached, Message: r.Device + " attached", LastTransitionTime: metav1.Now(),
	}}}
	if r.ShareID != nil {
		id := string(*r.ShareID)
		st.ShareID = &id
	}
	c.Status.Devices = append(c.Status.Devices, st)
	if _, err := e.kube.ResourceV1().ResourceClaims("default").UpdateStatus(e.ctx, c, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
	e.waitLister(name, func(c *resourceapi.ResourceClaim) bool {
		return hasCondition(c, c.Status.Allocation.Devices.Results[1], condAttached) != nil
	})
}

// condition returns a condition of the pool device status entry, and the
// entry.
func (e *env) condition(name, condType string) (*metav1.Condition, *resourceapi.AllocatedDeviceStatus) {
	e.t.Helper()
	c := e.claim(name)
	for i := range c.Status.Devices {
		d := &c.Status.Devices[i]
		if d.Driver == device.PoolDriverName && d.Pool == testPoolName {
			return meta.FindStatusCondition(d.Conditions, condType), d
		}
	}
	return nil, nil
}

func (e *env) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (e *env) slices() []resourceapi.ResourceSlice {
	e.t.Helper()
	l, err := e.kube.ResourceV1().ResourceSlices().List(e.ctx, metav1.ListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

// (1) publish
func TestPublish(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sync()
	e.eventually("ResourceSlice", func() bool { return len(e.slices()) == 1 })
	slice := e.slices()[0]
	s := slice.Spec
	if s.Driver != device.PoolDriverName || s.Pool.Name != testPoolName || s.Pool.ResourceSliceCount != 1 ||
		s.AllNodes != nil || s.NodeName != nil || s.PerDeviceNodeSelection != nil {
		t.Fatalf("unexpected slice spec %+v", s)
	}
	// the nodes that are hosts of the server, by their hostname labels:
	// node1 (vm1 by uuid) and node2 (by name), not node3
	if got := selectorString(s.NodeSelector); got != "kubernetes.io/hostname In [h-node1 h-node2]" {
		t.Fatalf("node selector %s", got)
	}
	byName := map[string]resourceapi.Device{}
	for _, d := range s.Devices {
		byName[d.Name] = d
	}
	if len(byName) != 3 {
		t.Fatalf("expected pooled0, shared0, weird-name: %v", deviceNames(s.Devices))
	}
	if _, ok := byName["bad0"]; ok {
		t.Fatal("device in state error published")
	}
	// exactly the attributes of the contract: type, source, serial,
	// shared, persistent, size, coherent
	for _, name := range []string{"pooled0", "shared0"} {
		var keys []string
		for k := range byName[name].Attributes {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
		if got := strings.Join(keys, ","); got != "coherent,persistent,serial,shared,size,source,type" {
			t.Fatalf("%s: attributes %s", name, got)
		}
		if a := byName[name].Attributes; *a["type"].StringValue != "memdev" || *a["source"].StringValue != "pool" ||
			a["persistent"].BoolValue == nil || *a["persistent"].BoolValue || a["coherent"].BoolValue == nil || !*a["coherent"].BoolValue {
			t.Fatalf("%s: type %q source %q", name, *a["type"].StringValue, *a["source"].StringValue)
		}
	}
	p := byName["pooled0"]
	memory := p.Capacity["memory"]
	if *p.Attributes["serial"].StringValue != "0xc1ee0001" || *p.Attributes["shared"].BoolValue ||
		*p.Attributes["size"].IntValue != 512<<20 || len(p.Capacity) != 1 ||
		!memory.Value.Equal(resource.MustParse("512Mi")) || p.AllowMultipleAllocations != nil ||
		!ptr.Deref(p.BindsToNode, false) ||
		strings.Join(p.BindingConditions, ",") != "cxl-pool.nri.io/Attached" ||
		strings.Join(p.BindingFailureConditions, ",") != "cxl-pool.nri.io/AttachFailed" {
		t.Fatalf("unexpected unshared device %+v", p)
	}
	sh := byName["shared0"]
	// shares 1M without a requestPolicy: no request is exclusive
	shares, ok := sh.Capacity["shares"]
	if *sh.Attributes["serial"].StringValue != "0xc1ae0001" ||
		!*sh.Attributes["shared"].BoolValue || *sh.Attributes["size"].IntValue != 256<<20 ||
		!ptr.Deref(sh.AllowMultipleAllocations, false) || len(sh.Capacity) != 1 || !ok ||
		shares.Value.Value() != 1000000 || shares.Value.String() != "1M" || shares.RequestPolicy != nil ||
		!ptr.Deref(sh.BindsToNode, false) ||
		strings.Join(sh.BindingConditions, ",") != "cxl-pool.nri.io/Attached" ||
		strings.Join(sh.BindingFailureConditions, ",") != "cxl-pool.nri.io/AttachFailed" {
		t.Fatalf("unexpected shared device %+v", sh)
	}
	if _, ok := byName["weird-name"]; !ok {
		t.Fatalf("sanitized name missing: %v", deviceNames(s.Devices))
	}
	// a device change is published
	e.pool.mu.Lock()
	delete(e.pool.devices, "weird.name_")
	e.pool.mu.Unlock()
	e.sync()
	e.eventually("updated ResourceSlice", func() bool {
		l := e.slices()
		return len(l) == 1 && len(l[0].Spec.Devices) == 2
	})
	// shutdown deletes the slice
	e.c.shutdown()
	if l := e.slices(); len(l) != 0 {
		t.Fatalf("slices left after shutdown: %+v", l)
	}
}

// A pool of more devices than a ResourceSlice takes has several slices.
func TestPublishManyDevices(t *testing.T) {
	t.Parallel()
	n := resourceapi.ResourceSliceMaxDevices + 1
	e := newEnvWith(t, func(p *fakePool, _ *Config) {
		p.devices = map[string]api.Device{}
		for i := range n {
			name := fmt.Sprintf("d%03d", i)
			p.devices[name] = api.Device{Name: name, Serial: fmt.Sprintf("0xc1ee%04x", i+1), Size: 256 << 20, Scope: api.ScopePool, State: api.DeviceFree}
		}
	})
	e.sync()
	e.eventually("two ResourceSlices", func() bool { return len(e.slices()) == 2 })
	devs := 0
	for _, s := range e.slices() {
		if s.Spec.Pool.Name != testPoolName || s.Spec.Pool.ResourceSliceCount != 2 || len(s.Spec.Devices) > resourceapi.ResourceSliceMaxDevices {
			t.Fatalf("unexpected slice: pool %+v, %d devices", s.Spec.Pool, len(s.Spec.Devices))
		}
		devs += len(s.Spec.Devices)
	}
	if devs != n {
		t.Fatalf("%d devices published, expected %d", devs, n)
	}
}

func TestDeviceName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"pooled0":                      "pooled0",
		"Shared_0":                     "shared-0",
		"-a.b-":                        "a-b",
		"___":                          "",
		strings.Repeat("a", 62) + "_b": strings.Repeat("a", 62),
		strings.Repeat("x", 70):        strings.Repeat("x", 63),
	} {
		if got := DeviceName(in); got != want {
			t.Errorf("DeviceName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDuplicateNamesSkipped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	devs, byName := e.c.buildDevices([]api.Device{
		{Name: "a.b", Scope: api.ScopePool, Serial: "0xc1ee0001", Size: 256 << 20},
		{Name: "a-b", Scope: api.ScopePool, Serial: "0xc1ee0002", Size: 256 << 20},
	})
	if len(devs) != 1 || byName["a-b"].Name != "a-b" {
		t.Fatalf("unexpected devices %v %v", deviceNames(devs), byName)
	}
}

// A published serial is 0x and lower-case hex. A device without a valid
// serial is not published.
func TestPublishedSerial(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	devs, _ := e.c.buildDevices([]api.Device{
		{Name: "dec", Scope: api.ScopePool, Serial: "3253600257", Size: 256 << 20},
		{Name: "bad", Scope: api.ScopePool, Serial: "zz", Size: 256 << 20},
	})
	if len(devs) != 1 || devs[0].Name != "dec" || *devs[0].Attributes["serial"].StringValue != "0xc1ee0001" {
		t.Fatalf("unexpected devices %+v", devs)
	}
	if !strings.Contains(e.logs.String(), `device bad: invalid serial "zz", not published`) {
		t.Fatalf("no warning of the invalid serial in the log")
	}
}

func TestMapHosts(t *testing.T) {
	t.Parallel()
	hm := mapHosts(newFakePool().hosts, []*corev1.Node{
		node("node1", "B6A78455EDB1558D8CF101A259F76E97"),
		node("node2", "x"),
		node("node3", ""),
		node("vm1", ""), // vm1 is taken by node1's uuid
	})
	if hm.String() != "[node1=vm1 node2=node2]" || hm.hostNode["vm1"] != "node1" {
		t.Fatalf("unexpected map %s", hm)
	}
}

// (2) allocated claim on a known node
func TestAttachAndStatus(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	claim := e.createClaim("pooled-memory", "pooled0", "node1", "")
	e.sync()
	att, _ := e.pool.calls()
	if len(att) != 1 || att[0] != "pooled0@vm1 k8s:resourceclaim/"+string(claim.UID) {
		t.Fatalf("unexpected attach requests %v", att)
	}
	cond, st := e.condition("pooled-memory", "cxl-pool.nri.io/Attached")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Attached" ||
		cond.Message != "pooled0 attached to vm1 (slot ds1)" {
		t.Fatalf("unexpected condition %+v", cond)
	}
	var data map[string]any
	if err := json.Unmarshal(st.Data.Raw, &data); err != nil {
		t.Fatal(err)
	}
	if data["serial"] != "0xc1ee0001" || data["shared"] != false || data["size"] != float64(512<<20) ||
		data["host"] != "vm1" || data["hostUUID"] != "b6a78455-edb1-558d-8cf1-01a259f76e97" ||
		data["attachment"] != "pooled0@vm1" || data["qemuDeviceID"] != "fcp_pooled0.hp1" || st.ShareID != nil {
		t.Fatalf("unexpected data %s", st.Data.Raw)
	}
	if ev := e.events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "Normal Attached pooled0 attached to vm1") {
		t.Fatalf("unexpected events %v", ev)
	}
	// steady state: no new requests, no status writes
	e.waitStatus("pooled-memory", 1)
	rv := e.claim("pooled-memory").ResourceVersion
	e.sync()
	if att, det := e.pool.calls(); len(att) != 1 || len(det) != 0 {
		t.Fatalf("unexpected requests %v %v", att, det)
	}
	if e.claim("pooled-memory").ResourceVersion != rv {
		t.Fatal("status rewritten in steady state")
	}
	// after a re-attach (a qemu restart) the device data is that of the
	// new attachment
	e.pool.mu.Lock()
	a := e.pool.atts["pooled0@vm1"]
	a.QemuDeviceID, a.Slot.Bus = "fcp_pooled0.hp9", "ds9"
	e.pool.atts["pooled0@vm1"] = a
	e.pool.mu.Unlock()
	e.sync()
	e.waitLister("pooled-memory", func(c *resourceapi.ResourceClaim) bool {
		cond := hasCondition(c, c.Status.Allocation.Devices.Results[1], "cxl-pool.nri.io/Attached")
		return cond != nil && cond.Message == "pooled0 attached to vm1 (slot ds9)"
	})
	_, st = e.condition("pooled-memory", "cxl-pool.nri.io/Attached")
	data = nil
	if err := json.Unmarshal(st.Data.Raw, &data); err != nil || data["qemuDeviceID"] != "fcp_pooled0.hp9" {
		t.Fatalf("unexpected data %s %v", st.Data.Raw, err)
	}
	// steady state again: no status write
	rv = e.claim("pooled-memory").ResourceVersion
	e.sync()
	if e.claim("pooled-memory").ResourceVersion != rv {
		t.Fatal("status rewritten in steady state")
	}
}

// (3) node without host
func TestNoHost(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createClaim("c3", "pooled0", "node3", "")
	e.sync()
	if att, _ := e.pool.calls(); len(att) != 0 {
		t.Fatalf("unexpected attach %v", att)
	}
	cond, _ := e.condition("c3", "cxl-pool.nri.io/AttachFailed")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonNoHost {
		t.Fatalf("unexpected condition %+v", cond)
	}
	if ev := e.events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning AttachFailed") {
		t.Fatalf("unexpected events %v", ev)
	}
	// written once
	e.waitStatus("c3", 1)
	e.sync()
	if ev := e.events(); len(ev) != 0 {
		t.Fatalf("unexpected events %v", ev)
	}
}

// (4) attach 409
func TestAttachConflict(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.pool.mu.Lock()
	e.pool.attachErr["pooled0"] = api.CodeConflict
	e.pool.mu.Unlock()
	e.createClaim("c4", "pooled0", "node2", "")
	e.sync()
	cond, _ := e.condition("c4", "cxl-pool.nri.io/AttachFailed")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonConflict || !strings.Contains(cond.Message, "injected Conflict") {
		t.Fatalf("unexpected condition %+v", cond)
	}
	if c, _ := e.condition("c4", "cxl-pool.nri.io/Attached"); c != nil {
		t.Fatalf("unexpected Attached condition %+v", c)
	}
	// the conflict goes away: Attached replaces AttachFailed
	e.pool.mu.Lock()
	delete(e.pool.attachErr, "pooled0")
	e.pool.mu.Unlock()
	e.waitStatus("c4", 1)
	e.sync()
	if c, _ := e.condition("c4", "cxl-pool.nri.io/Attached"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("expected Attached, got %+v", c)
	}
	if c, _ := e.condition("c4", "cxl-pool.nri.io/AttachFailed"); c != nil {
		t.Fatalf("AttachFailed not cleared: %+v", c)
	}
}

// (5) allocation removed
func TestDetachOnDeallocation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createClaim("c5", "pooled0", "node2", "")
	e.sync()
	if ids := e.pool.attachmentIDs(); len(ids) != 1 || ids[0] != "pooled0@node2" {
		t.Fatalf("unexpected attachments %v", ids)
	}
	e.events()
	e.deallocate("c5")
	// the guest has not released the device: 409, the attachment stays
	// detaching, and is not requested again while it is detaching
	e.pool.mu.Lock()
	e.pool.detachHold = true
	e.pool.mu.Unlock()
	e.sync()
	e.sync()
	e.sync()
	if _, det := e.pool.calls(); len(det) != 1 {
		t.Fatalf("unexpected detach requests %v", det)
	}
	if n := strings.Count(e.logs.String(), "detach pooled0@node2: still detaching"); n != 1 {
		t.Fatalf("still detaching logged %d times", n)
	}
	// the server gave up the device_del and reports it attached again: retried
	e.pool.mu.Lock()
	e.pool.detachHold = false
	a := e.pool.atts["pooled0@node2"]
	a.State = api.AttachmentAttached
	e.pool.atts["pooled0@node2"] = a
	e.pool.mu.Unlock()
	e.sync()
	if ids := e.pool.attachmentIDs(); len(ids) != 0 {
		t.Fatalf("attachments left: %v", ids)
	}
	if ev := e.events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "Normal Detached pooled0 detached from node2") {
		t.Fatalf("unexpected events %v", ev)
	}
}

// A failed attachment of a device that a claim wants fails the claim.
func TestFailedAttachmentFailsClaim(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.pool.addAttachment(api.Attachment{Device: "pooled0", Host: "vm1", Owner: e.c.Owner("uid-old"),
		State: api.AttachmentFailed, Error: "guest did not release the device"})
	e.createClaim("c1", "pooled0", "node1", "")
	e.sync()
	cond, _ := e.condition("c1", "cxl-pool.nri.io/AttachFailed")
	if cond == nil || cond.Reason != ReasonAttachError || cond.Message != "attachment pooled0@vm1 failed: guest did not release the device" {
		t.Fatalf("unexpected condition %+v", cond)
	}
	if att, det := e.pool.calls(); len(att) != 0 || len(det) != 0 {
		t.Fatalf("unexpected requests %v %v", att, det)
	}
}

// (7) two claims, same shared device, same node
func TestSharedTwoClaimsOneNode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createClaimShares("a", "shared0", "node1", "share-a", 1)
	e.createClaimShares("b", "shared0", "node1", "share-b", 1)
	e.sync()
	att, _ := e.pool.calls()
	if len(att) != 1 || att[0] != "shared0@vm1 k8s:resourceclaim/uid-a" {
		t.Fatalf("unexpected attach requests %v", att)
	}
	for _, n := range []string{"a", "b"} {
		c, st := e.condition(n, "cxl-pool.nri.io/Attached")
		if c == nil {
			t.Fatalf("claim %s: no Attached condition", n)
		}
		// one share each: not exclusive
		var data DeviceData
		if err := json.Unmarshal(st.Data.Raw, &data); err != nil || data.Exclusive {
			t.Fatalf("claim %s: data %s %v", n, st.Data.Raw, err)
		}
	}
	// the claim that owns the attachment goes first: still attached
	e.deleteClaim("a")
	e.sync()
	if _, det := e.pool.calls(); len(det) != 0 {
		t.Fatalf("detached while claim b uses it: %v", det)
	}
	e.deallocate("b")
	e.sync()
	if _, det := e.pool.calls(); len(det) != 1 || det[0] != "shared0@vm1" {
		t.Fatalf("unexpected detach requests %v", det)
	}
}

// Two shares of one device in one claim use one attachment. Each result
// has a status entry of its own, found by its share id.
func TestSharedTwoSharesOneClaim(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createClaimShares("t", "shared0", "node1", "share-1", 1)
	second := c.Status.Allocation.Devices.Results[1]
	second.Request, second.ShareID = "mem2", ptr.To(types.UID("share-2"))
	c.Status.Allocation.Devices.Results = append(c.Status.Allocation.Devices.Results, second)
	if _, err := e.kube.ResourceV1().ResourceClaims("default").UpdateStatus(e.ctx, c, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.waitLister("t", func(c *resourceapi.ResourceClaim) bool { return len(c.Status.Allocation.Devices.Results) == 3 })
	e.sync()
	if att, _ := e.pool.calls(); len(att) != 1 || att[0] != "shared0@vm1 k8s:resourceclaim/uid-t" {
		t.Fatalf("unexpected attach requests %v", att)
	}
	c = e.claim("t")
	for _, r := range c.Status.Allocation.Devices.Results[1:] {
		st := device.StatusOf(c, &r)
		if st == nil || hasCondition(c, r, condAttached) == nil {
			t.Fatalf("share %s: status %+v", *r.ShareID, st)
		}
		pd, err := device.PoolStatus(c, &r)
		if err != nil || !pd.Shared || pd.Exclusive {
			t.Fatalf("share %s: data %+v %v", *r.ShareID, pd, err)
		}
	}
	if n := len(c.Status.Devices); n != 2 {
		t.Fatalf("%d status entries, expected 2: %+v", n, c.Status.Devices)
	}
	e.deallocate("t")
	e.sync()
	if _, det := e.pool.calls(); len(det) != 1 || det[0] != "shared0@vm1" {
		t.Fatalf("unexpected detach requests %v", det)
	}
}

// (8) other owners and foreign hosts
func TestForeignAttachmentsKept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.pool.addAttachment(api.Attachment{Device: "pooled0", Host: "vm1", Owner: "manual"})
	e.pool.addAttachment(api.Attachment{Device: "shared0", Host: "vm1", Owner: "", Adopted: true})
	e.pool.addAttachment(api.Attachment{Device: "shared0", Host: "foreign", Owner: "k8s:resourceclaim/other-cluster"})
	e.pool.addAttachment(api.Attachment{Device: "weird.name_", Host: "node2", Owner: "k8s:resourceclaim/gone", State: api.AttachmentFailed, Error: "zombie"})
	e.sync()
	e.sync()
	if _, det := e.pool.calls(); len(det) != 0 {
		t.Fatalf("unexpected detach requests %v", det)
	}
	if ids := e.pool.attachmentIDs(); len(ids) != 4 {
		t.Fatalf("attachments changed: %v", ids)
	}
}

// The server adopted the device of a claim as an attachment without
// owner, for instance after a qemu restart: the controller takes it over
// where it is and detaches it when the claim goes.
func TestAdoptedAttachmentTakenOver(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	claim := e.createClaimConfig("c", "pooled0", "node1", "", 0, []resourceapi.DeviceAllocationConfiguration{
		claimConfig(`"pool": {"opaque": {"fake-cxl-pool": {"slot": "ds7"}}}`)})
	e.pool.addAttachment(api.Attachment{Device: "pooled0", Host: "vm1", Serial: "0xc1ee0001", Adopted: true,
		Slot: api.Slot{Bus: "ds6"}, QemuDeviceID: "fcp_pooled0.hp6"})
	e.sync()
	e.sync()
	att, det := e.pool.calls()
	if len(att) != 1 || att[0] != "pooled0@vm1 "+e.c.Owner(claim.UID) || len(det) != 0 {
		t.Fatalf("unexpected requests %v %v", att, det)
	}
	if reqs := e.pool.requests(); reqSettings(reqs[0]) != "" {
		t.Fatalf("settings sent for an existing attachment: %+v", reqs[0])
	}
	e.pool.mu.Lock()
	a := e.pool.atts["pooled0@vm1"]
	e.pool.mu.Unlock()
	if a.Owner != e.c.Owner(claim.UID) {
		t.Fatalf("not taken over: %+v", a)
	}
	_, st := e.condition("c", "cxl-pool.nri.io/Attached")
	var data DeviceData
	if st == nil || json.Unmarshal(st.Data.Raw, &data) != nil || data.QemuDeviceID != "fcp_pooled0.hp6" {
		t.Fatalf("unexpected status %+v", st)
	}
	if !strings.Contains(e.logs.String(), "taking over attachment pooled0@vm1 without owner (attached, adopted true, qemu device fcp_pooled0.hp6)") {
		t.Fatalf("take-over not logged:\n%s", e.logs.String())
	}
	e.deallocate("c")
	e.sync()
	if _, det := e.pool.calls(); len(det) != 1 || det[0] != "pooled0@vm1" {
		t.Fatalf("unexpected detach requests %v", det)
	}
}

func TestExclusiveMovesBetweenNodes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createClaim("m", "pooled0", "node1", "")
	e.sync()
	// reallocated to node2: the attach conflicts until vm1 is detached in
	// the same reconcile, without AttachFailed; the next one attaches
	e.deleteClaim("m")
	e.createClaim("m2", "pooled0", "node2", "")
	e.sync()
	if c, _ := e.condition("m2", "cxl-pool.nri.io/AttachFailed"); c != nil {
		t.Fatalf("unexpected AttachFailed %+v", c)
	}
	e.sync()
	if c, _ := e.condition("m2", "cxl-pool.nri.io/Attached"); c == nil {
		t.Fatal("not attached after the move")
	}
	if ids := e.pool.attachmentIDs(); len(ids) != 1 || ids[0] != "pooled0@node2" {
		t.Fatalf("unexpected attachments %v", ids)
	}
}

// A result is exclusive if it takes all the shares of a shared device. A
// device that is not shared never gets the exclusive flag.
func TestExclusiveResult(t *testing.T) {
	t.Parallel()
	c := &Controller{devices: map[string]api.Device{
		"shared0": {Name: "shared0", Shared: true},
		"pooled0": {Name: "pooled0"},
	}}
	all := map[resourceapi.QualifiedName]resource.Quantity{device.CapacityShares: resource.MustParse("1M")}
	for _, tc := range []struct {
		name     string
		device   string
		consumed map[resourceapi.QualifiedName]resource.Quantity
		want     bool
	}{
		{"all shares", "shared0", all, true},
		{"one share", "shared0", map[resourceapi.QualifiedName]resource.Quantity{device.CapacityShares: resource.MustParse("1")}, false},
		{"shares missing", "shared0", nil, true},
		{"other capacity only", "shared0", map[resourceapi.QualifiedName]resource.Quantity{"memory": resource.MustParse("1Gi")}, true},
		{"not shared", "pooled0", all, false},
	} {
		r := &resourceapi.DeviceRequestAllocationResult{Device: tc.device, ConsumedCapacity: tc.consumed}
		if got := c.allShares(r); got != tc.want {
			t.Errorf("%s: exclusive %v, want %v", tc.name, got, tc.want)
		}
	}
	// after a conflict of a single-host attach, the first want that is not
	// single-host attaches without the flag
	if w := firstShared([]*want{{singleHost: true}, {singleHost: false}}); w == nil || w.singleHost {
		t.Fatalf("first shared want %+v", w)
	}
	if w := firstShared([]*want{{singleHost: true}}); w != nil {
		t.Fatalf("first shared want of exclusive wants %+v", w)
	}
}

func TestPersistentAttributeAndStatus(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.pool.mu.Lock()
	e.pool.devices["llm0"] = api.Device{Name: "llm0", Serial: "0xc1ae0002", Size: 512 << 20, Shared: true, Persistent: true,
		Scope: api.ScopePool, State: api.DeviceFree}
	e.pool.mu.Unlock()
	e.sync()
	e.eventually("ResourceSlice", func() bool { return len(e.slices()) == 1 })
	attrs := map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
	for _, d := range e.slices()[0].Spec.Devices {
		attrs[d.Name] = d.Attributes
	}
	for name, want := range map[string]bool{"llm0": true, "shared0": false, "pooled0": false} {
		if a := attrs[name]["persistent"]; a.BoolValue == nil || *a.BoolValue != want {
			t.Fatalf("%s: attribute persistent %+v, want %v", name, a, want)
		}
	}
	// status data: persistent true for llm0, omitted for volatile devices
	e.createClaimShares("w", "llm0", "node1", "share-w", device.SharesUnlimited)
	e.createClaim("p", "pooled0", "node1", "")
	e.sync()
	for claim, want := range map[string]any{"w": true, "p": nil} {
		c, st := e.condition(claim, "cxl-pool.nri.io/Attached")
		if c == nil {
			t.Fatalf("claim %s: no Attached condition", claim)
		}
		var data map[string]any
		if err := json.Unmarshal(st.Data.Raw, &data); err != nil {
			t.Fatal(err)
		}
		if data["persistent"] != want {
			t.Fatalf("claim %s: data %s, want persistent %v", claim, st.Data.Raw, want)
		}
		if claim == "w" && (data["serial"] != "0xc1ae0002" || data["shared"] != true || data["size"] != float64(512<<20) || data["exclusive"] != true) {
			t.Fatalf("claim w: unexpected data %s", st.Data.Raw)
		}
	}
}
