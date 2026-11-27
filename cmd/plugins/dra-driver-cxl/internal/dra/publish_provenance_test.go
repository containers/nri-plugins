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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/klogtest"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/provenance"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/store"
	"github.com/containers/nri-plugins/pkg/cxl/device"
	"github.com/containers/nri-plugins/pkg/cxl/kernel"
)

// The serials of a shared pool device, c-shared0 of pool-c (c-pooled0 is
// 0xc1ee2001), and of a local memdev other than pubLocalSerial.
const (
	pubSharedPoolSerial = 0xc1ae2001
	pubOtherLocalSerial = 0xa1
)

// pubSerialsEnv is a node with memdevs, the ResourceSlices and
// ResourceClaims of a fake API server, a state store, the provenance
// Watcher and the Publisher over them, scanned as the driver does without
// a pool driver.
type pubSerialsEnv struct {
	t      *testing.T
	client *fake.Clientset
	dir    string
	st     *store.Store
	w      *provenance.Watcher
	p      *Publisher

	mu      sync.Mutex
	clock   time.Time
	devs    map[uint64]kernel.MemdevInfo // on the node
	regions map[uint64]bool              // memdevs with a region
	plugged int
}

func newPubSerialsEnv(t *testing.T) *pubSerialsEnv {
	e := &pubSerialsEnv{
		t:       t,
		client:  fake.NewClientset(),
		dir:     t.TempDir(),
		clock:   time.Date(2026, 10, 26, 9, 35, 0, 0, time.UTC),
		devs:    map[uint64]kernel.MemdevInfo{},
		regions: map[uint64]bool{},
	}
	t.Cleanup(e.closeStore)
	e.restart()
	return e
}

// closeStore closes the store, writing its pending commits.
func (e *pubSerialsEnv) closeStore() {
	if e.st != nil {
		pubNoErr(e.t, e.st.Close())
		e.st = nil
	}
}

// restart starts the driver again: the store reopened, a new Watcher and
// Publisher.
func (e *pubSerialsEnv) restart() {
	e.t.Helper()
	e.closeStore()
	st, err := store.Open(context.Background(), store.Options{Dir: e.dir, BootID: "boot-1"})
	pubNoErr(e.t, err)
	e.st = st
	e.w = provenance.New(provenance.Options{Store: e.st, Client: e.client, NodeName: pubNode,
		Name: filepath.Join(e.dir, store.FileName), Now: e.now})
	e.p = e.publisher(func(context.Context) (*kernel.Snapshot, error) { return e.devices(), nil })
}

// publisher returns a publisher of the env (not started: it does not
// publish) whose scans detect the devices of scan.
func (e *pubSerialsEnv) publisher(scan func(context.Context) (*kernel.Snapshot, error)) *Publisher {
	return pubPublisher(e.t, publishOptions{
		Config:    PublishConfig{IgnoreDRAM: true, ManageMemdevs: true},
		NodeName:  pubNode,
		Scan:      scan,
		Watcher:   e.w,
		Store:     e.st,
		BlockSize: func() (uint64, error) { return pubBlockSize, nil },
	})
}

// lose makes state.json content and removes state.json.prev, so that the
// store opens LOST at the restart.
func (e *pubSerialsEnv) lose(content string) {
	e.t.Helper()
	e.closeStore()
	pubNoErr(e.t, os.WriteFile(filepath.Join(e.dir, store.FileName), []byte(content), 0o600))
	if err := os.Remove(filepath.Join(e.dir, store.PrevName)); err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	e.restart()
	if e.st.Lost() == nil {
		e.t.Fatal("the store is not LOST")
	}
}

func (e *pubSerialsEnv) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clock
}

// advance moves the clock forward without a scan.
func (e *pubSerialsEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = e.clock.Add(d)
}

// idle lets d pass with a scan every provenance.Poll, as the watch does on
// a node without udev events.
func (e *pubSerialsEnv) idle(d time.Duration) {
	e.t.Helper()
	for end := e.now().Add(d); e.now().Before(end); {
		e.advance(min(provenance.Poll, end.Sub(e.now())))
		e.scan()
	}
}

// publish makes pool-c publish devices with the serials; without serials
// it deletes its ResourceSlice as its graceful stop does.
func (e *pubSerialsEnv) publish(serials ...string) {
	e.t.Helper()
	ctx := context.Background()
	_ = e.client.ResourceV1().ResourceSlices().Delete(ctx, "pool-c", metav1.DeleteOptions{})
	if len(serials) == 0 {
		return
	}
	var attrs []map[resourcev1.QualifiedName]resourcev1.DeviceAttribute
	for _, s := range serials {
		attrs = append(attrs, pubSerialAttr("serial", s))
	}
	_, err := e.client.ResourceV1().ResourceSlices().Create(ctx, pubResourceSlice("pool-c", device.PoolDriverName, attrs...), metav1.CreateOptions{})
	pubNoErr(e.t, err)
}

// plug attaches a memdev with the serial to the node, unplug detaches it.
func (e *pubSerialsEnv) plug(serial uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := pubMemdev()
	m.Serial = serial
	m.Name = fmt.Sprintf("mem%d", e.plugged)
	e.plugged++
	e.devs[serial] = m
}

func (e *pubSerialsEnv) unplug(serial uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.devs, serial)
}

// withRegion gives the memdev of the serial a region.
func (e *pubSerialsEnv) withRegion(serial uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.regions[serial] = true
}

// devices returns the observation of the node: the memdevs, and
// region<N> of node 2+N on the memdevs with a region.
func (e *pubSerialsEnv) devices() *kernel.Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	devs := &kernel.Snapshot{}
	for _, s := range slices.Sorted(maps.Keys(e.devs)) {
		devs.Memdevs = append(devs.Memdevs, e.devs[s])
		if e.regions[s] {
			n := len(devs.Regions)
			pubRegion(devs, fmt.Sprintf("region%d", n), 2+n, 1<<30, s)
		}
	}
	return devs
}

// scan returns the devices that a scan of the publisher sets.
func (e *pubSerialsEnv) scan() []string {
	e.t.Helper()
	pubNoErr(e.t, e.p.Scan(context.Background()))
	return pubNames(e.p.Devices())
}

func (e *pubSerialsEnv) wantScan(what string, want ...string) {
	e.t.Helper()
	if got := e.scan(); !slices.Equal(got, want) {
		e.t.Fatalf("%s: published %v, expected %v", what, got, want)
	}
}

// record returns the record of the serial in the store once the commits so
// far are written, nil for none.
func (e *pubSerialsEnv) record(serial uint64) *state.Provenance {
	e.t.Helper()
	e.flush()
	return e.st.Snapshot().ProvenanceOf(state.Serial(serial))
}

// flush waits until the lazy commits so far are written.
func (e *pubSerialsEnv) flush() {
	e.t.Helper()
	pubNoErr(e.t, e.st.Commit(context.Background(), store.Durable, func(*store.Document) error { return nil }))
}

// claim makes a pool claim of pool-c's device dev allocated to node, with
// the serial in its status ("" for none, "failed" for a failed attach).
func (e *pubSerialsEnv) claim(name, node, dev, serial string) {
	e.t.Helper()
	r := resourcev1.DeviceRequestAllocationResult{Request: "mem", Driver: device.PoolDriverName, Pool: "pool-c", Device: dev}
	c := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pubNS},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{Results: []resourcev1.DeviceRequestAllocationResult{r}},
				NodeSelector: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
				}}},
			},
		},
	}
	st := resourcev1.AllocatedDeviceStatus{Driver: r.Driver, Pool: r.Pool, Device: r.Device}
	switch serial {
	case "":
	case "failed":
		st.Conditions = []metav1.Condition{{Type: device.PoolDriverName + "/AttachFailed", Status: metav1.ConditionTrue, Reason: "NoHost"}}
		c.Status.Devices = append(c.Status.Devices, st)
	default:
		st.Conditions = []metav1.Condition{{Type: device.PoolDriverName + "/Attached", Status: metav1.ConditionTrue, Reason: "Attached"}}
		st.Data = &runtime.RawExtension{Raw: []byte(`{"serial":"` + serial + `","shared":true,"size":268435456}`)}
		c.Status.Devices = append(c.Status.Devices, st)
	}
	ctx := context.Background()
	claims := e.client.ResourceV1().ResourceClaims(pubNS)
	_ = claims.Delete(ctx, name, metav1.DeleteOptions{})
	_, err := claims.Create(ctx, c, metav1.CreateOptions{})
	pubNoErr(e.t, err)
}

// pubResourceSlice returns a ResourceSlice of the driver whose devices have
// the attributes.
func pubResourceSlice(name, driver string, attrs ...map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) *resourcev1.ResourceSlice {
	rs := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       resourcev1.ResourceSliceSpec{Driver: driver, Pool: resourcev1.ResourcePool{Name: name, ResourceSliceCount: 1}},
	}
	for i, a := range attrs {
		rs.Spec.Devices = append(rs.Spec.Devices, resourcev1.Device{Name: name + "-" + string(rune('a'+i)), Attributes: a})
	}
	return rs
}

// pubSerialAttr returns the attributes of a device with the serial under
// key.
func pubSerialAttr(key, serial string) map[resourcev1.QualifiedName]resourcev1.DeviceAttribute {
	return map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{resourcev1.QualifiedName(key): {StringValue: ptr(serial)}}
}

// TestProvenanceSliceDeleted pins a shared pool device in pool-c's slice
// that is attached to the node for a claim that is not prepared: pool-c's
// controller stops gracefully and deletes its slice; a rescan, and a
// driver restart while the slice is absent, do not publish it; the store
// has the serials of pool devices, the local memdev and pool-c.
func TestProvenanceSliceDeleted(t *testing.T) {
	logs := klogtest.Capture(t, 3)
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	e.publish("0xc1ee2001", "0xc1ae2001")
	e.wantScan("local device", pubLocalDeviceName)
	e.plug(pubSharedPoolSerial)
	e.wantScan("pool device attached", pubLocalDeviceName)
	e.publish()
	e.wantScan("slice deleted", pubLocalDeviceName)
	e.restart()
	e.wantScan("driver restarted without the slice", pubLocalDeviceName)
	if e.w.Holding() {
		t.Fatal("a remembered pool device is held as a new memdev")
	}
	if n := logMatches(logs(), "discovered local memdev memdev-c1ae2001"); n != 0 {
		t.Fatalf("the pool device was published as a local one:\n%s", logs())
	}
	if n := logMatches(logs(), "discovered local memdev memdev-c100e2e1"); n == 0 {
		t.Fatalf("no line of the local memdev:\n%s", logs())
	}
	// the store has the serials of pool devices and the local memdev
	e.flush()
	doc := e.st.Snapshot()
	got := map[string]bool{}
	for _, r := range doc.Provenance {
		got[r.Serial.String()] = r.State == state.ProvenanceLocal
	}
	if !maps.Equal(got, map[string]bool{"0xc100e2e1": true, "0xc1ae2001": false, "0xc1ee2001": false}) || len(doc.ListedPools) != 1 ||
		doc.ListedPools[0].Pool != device.PoolDriverName+"/pool-c" || doc.ListedPools[0].Listed.IsZero() {
		t.Fatalf("records %+v, listed pools %+v", doc.Provenance, doc.ListedPools)
	}
	// the controller is back
	e.publish("0xc1ee2001", "0xc1ae2001")
	e.wantScan("slice back", pubLocalDeviceName)
}

// TestProvenanceHotAddWhileSliceDeleted pins a pool device detached while
// the slice lists it, the slice deleted and the device attached again
// before the controller publishes again: never published.
func TestProvenanceHotAddWhileSliceDeleted(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	e.plug(pubSharedPoolSerial)
	e.publish("0xc1ee2001", "0xc1ae2001")
	e.wantScan("pool device attached", pubLocalDeviceName)
	e.unplug(pubSharedPoolSerial)
	e.wantScan("pool device detached", pubLocalDeviceName)
	e.publish()
	e.advance(13 * time.Second)
	e.plug(pubSharedPoolSerial)
	e.wantScan("pool device attached without the slice", pubLocalDeviceName)
	e.advance(3 * time.Minute)
	e.wantScan("3 minutes later", pubLocalDeviceName)
	e.restart()
	e.wantScan("driver restarted", pubLocalDeviceName)
}

// TestProvenanceNewMemdevHeld pins a pool device whose serial the
// driver never saw in a slice, attached while pool-c has no slice: it is
// held, and is a pool device once pool-c lists it.
func TestProvenanceNewMemdevHeld(t *testing.T) {
	logs := klogtest.Capture(t, 0)
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	e.publish("0xc1ee2001")
	e.wantScan("local device", pubLocalDeviceName)
	e.publish()
	e.plug(pubSharedPoolSerial)
	e.wantScan("new memdev while pool-c is missing", pubLocalDeviceName)
	if !e.w.Holding() {
		t.Fatal("the new memdev is not held")
	}
	out := logs()
	if logMatches(out, "provenance: holding new memdev") != 1 {
		t.Fatalf("no line of the hold:\n%s", out)
	}
	e.advance(provenance.MaxHold / 2)
	e.wantScan("half the hold time later", pubLocalDeviceName)
	e.publish("0xc1ee2001", "0xc1ae2001")
	e.wantScan("pool-c lists it", pubLocalDeviceName)
	if e.w.Holding() {
		t.Fatal("a pool device is held")
	}
	e.publish()
	e.advance(2 * provenance.MaxHold)
	e.wantScan("pool-c missing again", pubLocalDeviceName)
}

// TestProvenanceHoldEnds pins a new memdev held while a pool is
// missing: published as a local one after MaxHold, and at once when the
// pool is back without it; a pool observed missing for longer than
// MissingWindow holds back nothing, but only when its absence was observed.
func TestProvenanceHoldEnds(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.publish("0xc1ee2001")
	e.wantScan("no memdevs")
	e.publish()
	e.plug(pubLocalSerial)
	e.wantScan("held")
	e.advance(provenance.MaxHold)
	e.wantScan("after the hold time", pubLocalDeviceName)
	if e.w.Holding() {
		t.Fatal("still holding")
	}
	// another new memdev, and pool-c comes back without it
	e.plug(pubOtherLocalSerial)
	e.wantScan("second new memdev held", pubLocalDeviceName)
	e.publish("0xc1ee2001")
	e.wantScan("pool-c back", "memdev-a1", pubLocalDeviceName)
	// a pool observed missing for longer than MissingWindow holds back
	// nothing
	e.publish()
	e.unplug(pubOtherLocalSerial)
	e.wantScan("detached", pubLocalDeviceName)
	e.idle(provenance.MissingWindow + time.Minute)
	e.plug(0xa2)
	e.wantScan("pool-c gone for long", "memdev-a2", pubLocalDeviceName)
	// but only when its absence was observed: after a driver restart it
	// starts again
	e.restart()
	e.plug(0xa3)
	e.wantScan("pool-c gone for long, restarted", "memdev-a2", pubLocalDeviceName)
	if !e.w.Holding() {
		t.Fatal("not holding after the restart")
	}
}

// TestProvenanceLocalDevice pins a local memdev hot-added while every pool
// has its slices and never in a pool slice: published at once, also after
// a restart while the pool slices are absent.
func TestProvenanceLocalDevice(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.wantScan("no memdevs, no pools")
	e.plug(pubLocalSerial)
	e.wantScan("first run, no pools", pubLocalDeviceName)
	e.publish("0xc1ee2001")
	e.unplug(pubLocalSerial)
	e.wantScan("detached")
	e.plug(pubLocalSerial)
	e.wantScan("attached again", pubLocalDeviceName)
	e.publish()
	e.restart()
	e.wantScan("restart without the pool slices", pubLocalDeviceName)
	e.unplug(pubLocalSerial)
	e.wantScan("detached without the pool slices")
	e.plug(pubLocalSerial)
	e.wantScan("attached again without the pool slices", pubLocalDeviceName)
}

// TestProvenancePoolDeviceRemoved pins a pool device detached and a local
// memdev with another serial attached later; the pool serial is forgotten
// after Retention of observed absence, not of a gap without scans.
func TestProvenancePoolDeviceRemoved(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.publish("0xc1ee2001", "0xc1ae2001")
	e.plug(pubSharedPoolSerial)
	e.wantScan("pool device attached")
	e.unplug(pubSharedPoolSerial)
	e.wantScan("pool device detached")
	e.plug(pubOtherLocalSerial)
	e.wantScan("local device attached", "memdev-a1")
	// the controller stops: the pool device stays a pool device
	e.publish()
	e.plug(pubSharedPoolSerial)
	e.wantScan("pool device attached without the slice", "memdev-a1")
	e.unplug(pubSharedPoolSerial)
	// a day without a scan, then one scan: its absence was not observed
	e.advance(provenance.Retention + time.Hour)
	e.wantScan("a day later", "memdev-a1")
	if e.record(pubSharedPoolSerial) == nil {
		t.Fatal("pool serial forgotten without observing its absence")
	}
	// a day of scans
	e.idle(provenance.Retention + time.Hour)
	e.wantScan("a day of observed absence later", "memdev-a1")
	if e.record(pubSharedPoolSerial) != nil {
		t.Fatal("pool serial not forgotten")
	}
	if e.record(pubOtherLocalSerial) == nil {
		t.Fatal("present local memdev forgotten")
	}
}

// TestProvenanceLocalAllocatorUses pins a memdev that the device engine
// has a record of: local, also when it is new while a pool is missing.
func TestProvenanceLocalAllocatorUses(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.publish("0xc1ee2001")
	e.wantScan("no memdevs")
	e.publish()
	e.plug(pubLocalSerial)
	pubNoErr(t, e.st.Commit(context.Background(), store.Durable, func(doc *store.Document) error {
		doc.Memdevs = append(doc.Memdevs, state.Memdev{Serial: pubLocalSerial, Name: "mem0", Source: state.SourceLocal, State: state.MemdevIdleKept})
		return nil
	}))
	e.wantScan("used by the local allocator", pubLocalDeviceName)
}

// TestProvenancePrepared pins the memdevs that the pool driver prepares:
// remembered as serials of pool devices, also after a restart.
func TestProvenancePrepared(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.publish("0xc1ee2001")
	e.plug(pubLocalSerial)
	e.wantScan("local", pubLocalDeviceName)
	// once prepared, the local memdev's serial is a pool device's
	e.w.NotePrepared(map[uint64]bool{pubLocalSerial: true})
	e.wantScan("prepared")
	e.restart()
	e.wantScan("restarted")
	// the pool driver's serials of a scan
	e.plug(pubOtherLocalSerial)
	e.p = pubPublisher(t, publishOptions{
		Config:      PublishConfig{IgnoreDRAM: true, ManageMemdevs: true},
		NodeName:    pubNode,
		Scan:        func(context.Context) (*kernel.Snapshot, error) { return e.devices(), nil },
		Watcher:     e.w,
		PoolSerials: func() map[uint64]bool { return map[uint64]bool{pubOtherLocalSerial: true} },
		Store:       e.st,
		BlockSize:   func() (uint64, error) { return pubBlockSize, nil },
	})
	e.wantScan("prepared by the pool driver")
	if r := e.record(pubOtherLocalSerial); r == nil || r.State != state.ProvenancePool {
		t.Fatalf("record %+v", r)
	}
}

// TestProvenanceBadFile pins a store whose state.json does not decode
// (LOST): the remembered serials are lost, a new memdev is held as
// while a pool is missing, for MaxHold within MissingWindow. The LOST
// store is not written.
func TestProvenanceBadFile(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.lose("{bogus")
	e.plug(pubLocalSerial)
	e.wantScan("bad file")
	e.advance(provenance.MaxHold)
	e.wantScan("bad file, held for long enough", pubLocalDeviceName)
	e.idle(provenance.MissingWindow)
	e.plug(pubOtherLocalSerial)
	e.wantScan("an hour later", "memdev-a1", pubLocalDeviceName)
	data, err := os.ReadFile(filepath.Join(e.dir, store.FileName))
	pubNoErr(t, err)
	if string(data) != "{bogus" {
		t.Fatalf("the LOST state.json was written: %s", data)
	}
}

// TestProvenanceTruncatedFile pins a truncated state.json without a
// previous one (LOST) while pool-c's slices are gone: the pool device is
// not published at once; neither is the local memdev, which is new again.
func TestProvenanceTruncatedFile(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	e.publish("0xc1ee2001", "0xc1ae2001")
	e.wantScan("local", pubLocalDeviceName)
	e.publish()
	e.lose("")
	e.plug(pubSharedPoolSerial)
	e.wantScan("truncated file, controller down")
	if !e.w.Holding() {
		t.Fatal("not holding")
	}
}

// TestProvenanceQuietNodeWindow pins pool-c listed at the last scan 2 h
// ago, no udev event since, and a pool device that the node never saw in a
// slice attached while pool-c's slices are gone: its absence is observed
// from that scan on, the new memdev is held; after MissingWindow of
// observed absence nothing is.
func TestProvenanceQuietNodeWindow(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	e.publish("0xc1ee2001")
	e.wantScan("local", pubLocalDeviceName)
	e.advance(2 * time.Hour)
	e.publish()
	e.plug(pubSharedPoolSerial)
	e.wantScan("never-listed pool device on a quiet node", pubLocalDeviceName)
	if !e.w.Holding() {
		t.Fatal("not holding")
	}
	// with the pool watch, an hour of observed absence holds back nothing
	e.unplug(pubSharedPoolSerial)
	e.idle(provenance.MissingWindow + time.Minute)
	e.plug(pubOtherLocalSerial)
	e.wantScan("after an hour of observed absence", "memdev-a1", pubLocalDeviceName)
}

// TestProvenanceQuietNodeForget pins a pool serial listed at a scan 25 h
// ago, no scan since; the controller stops gracefully, an unrelated rescan
// runs, and the device is attached: it is still a pool serial.
func TestProvenanceQuietNodeForget(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	e.publish("0xc1ee2001", "0xc1ae2001")
	e.wantScan("local", pubLocalDeviceName)
	e.advance(25 * time.Hour)
	e.publish()
	e.wantScan("unrelated udev scan", pubLocalDeviceName)
	e.advance(time.Second)
	e.plug(pubSharedPoolSerial)
	e.wantScan("pool device attached", pubLocalDeviceName)
	if e.w.Holding() {
		t.Fatal("a remembered pool serial is held")
	}
}

// TestProvenanceLocalBecomesPool pins an established local memdev that a
// pool slice lists later: a pool device from then on, also after the slice
// is gone and a restart.
func TestProvenanceLocalBecomesPool(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	e.wantScan("local", pubLocalDeviceName)
	e.publish("0xc100e2e1")
	e.wantScan("listed by pool-c")
	e.publish()
	e.wantScan("slice gone")
	e.restart()
	e.wantScan("restarted")
}

// TestProvenanceClaimEvidence pins a first start without records while
// pool-c's controller is down: the status of the pool claims of the node
// tells pool devices, and a pool claim of the node that is not attached
// yet holds back new memdevs for as long as it is not.
func TestProvenanceClaimEvidence(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.claim("attached", pubNode, "c-shared0", "0xc1ae2001")
	e.claim("failed", pubNode, "c-pooled1", "failed")
	e.claim("elsewhere", "node2", "c-pooled2", "")
	e.plug(pubLocalSerial)
	e.plug(pubSharedPoolSerial)
	e.wantScan("first start", pubLocalDeviceName)
	if r := e.record(pubSharedPoolSerial); r == nil || r.State != state.ProvenancePool || r.Pool != device.PoolDriverName+"/pool-c" {
		t.Fatalf("record %+v", r)
	}
	// a claim of the node is being attached: a new memdev waits for it
	e.claim("attaching", pubNode, "c-pooled0", "")
	e.plug(pubOtherLocalSerial)
	e.wantScan("attach in flight", pubLocalDeviceName)
	e.advance(2 * provenance.MaxHold)
	e.wantScan("attach in flight, later", pubLocalDeviceName)
	// its status tells another serial
	e.claim("attaching", pubNode, "c-pooled0", "0xc1ee2001")
	e.wantScan("attached", "memdev-a1", pubLocalDeviceName)
	// or it tells this one
	e.claim("attaching2", pubNode, "c-pooled3", "")
	e.plug(0xc1ee2003)
	e.wantScan("second attach in flight", "memdev-a1", pubLocalDeviceName)
	e.claim("attaching2", pubNode, "c-pooled3", "0xc1ee2003")
	e.wantScan("second attached", "memdev-a1", pubLocalDeviceName)
	if r := e.record(0xc1ee2003); e.w.Holding() || r == nil || r.State != state.ProvenancePool {
		t.Fatalf("holding %v record %+v", e.w.Holding(), r)
	}
}

// TestProvenanceHoldWhileNotListed pins a new memdev while no list of the
// pool slices has succeeded: held without the hold time running,
// which counts from the first successful list on.
func TestProvenanceHoldWhileNotListed(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.publish("0xc1ee2001")
	e.wantScan("pool-c listed")
	e.publish()
	var fail atomic.Bool
	fail.Store(true)
	e.client.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	e.restart()
	e.plug(pubLocalSerial)
	e.wantScan("not listed")
	e.advance(2 * provenance.MaxHold)
	e.wantScan("not listed, later")
	if !e.w.Holding() {
		t.Fatal("not holding")
	}
	fail.Store(false)
	e.wantScan("listed, pool-c missing")
	e.advance(provenance.MaxHold)
	e.wantScan("held for long enough", pubLocalDeviceName)
}

// TestProvenanceHeldRegion pins the regions of a memdev that is held
// back: not published either, and the reason is not "pool device".
func TestProvenanceHeldRegion(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.publish("0xc1ee2001")
	e.wantScan("no memdevs")
	e.publish()
	e.plug(pubOtherLocalSerial)
	e.withRegion(pubOtherLocalSerial)
	e.wantScan("held")
	devs := e.devices()
	if r := poolIgnoreReason(regionMemdevs(devs, &devs.Regions[0]), nil, map[uint64]bool{pubOtherLocalSerial: true}); !strings.Contains(r, "is held: ") {
		t.Fatalf("reason %q", r)
	}
	e.advance(provenance.MaxHold)
	e.wantScan("published", "region0-numa2")
}

// TestProvenanceCheckLocal pins CheckLocal, the fresh list of the
// engine's guard: it refuses a serial of a current pool slice; a
// remembered pool serial and a held memdev pass it: their POOL and HELD
// records, which the engine reads itself, refuse them.
func TestProvenanceCheckLocal(t *testing.T) {
	e := newPubSerialsEnv(t)
	ctx := context.Background()
	e.plug(pubLocalSerial)
	e.publish("0xc1ee2001")
	e.wantScan("local", pubLocalDeviceName)
	pubNoErr(t, e.w.CheckLocal(ctx, pubLocalSerial))
	// pool-c lists it now, before any rescan
	e.publish("0xc1ee2001", "0xc100e2e1")
	if err := e.w.CheckLocal(ctx, pubLocalSerial); err == nil ||
		!strings.Contains(err.Error(), "memdev 0xc100e2e1 is a pool device of "+device.PoolDriverName+"/pool-c") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	e.publish()
	pubNoErr(t, e.w.CheckLocal(ctx, pubLocalSerial))
	if r := e.st.Snapshot().ProvenanceOf(pubLocalSerial); r == nil || r.State != state.ProvenancePool {
		t.Fatalf("a remembered pool serial: record %+v", r)
	}
	e.plug(pubOtherLocalSerial)
	e.wantScan("held")
	pubNoErr(t, e.w.CheckLocal(ctx, pubOtherLocalSerial))
	if r := e.st.Snapshot().ProvenanceOf(pubOtherLocalSerial); r == nil || r.State != state.ProvenanceHeld {
		t.Fatalf("a held memdev: record %+v", r)
	}
	if err := (*provenance.Watcher)(nil).CheckLocal(ctx, pubLocalSerial); err != nil {
		t.Fatal(err)
	}
}

// TestRescanSerialized pins that two rescans (a udev event and a tick of
// the watch) never run at the same time: the later one sets the devices
// last.
func TestRescanSerialized(t *testing.T) {
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	devs := e.devices()
	entered := make(chan int32, 2)
	release := make(chan struct{})
	var n atomic.Int32
	p := e.publisher(func(context.Context) (*kernel.Snapshot, error) {
		i := n.Add(1)
		entered <- i
		if i == 1 {
			<-release
		}
		return devs, nil
	})
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.Rescan(ctx, false) }()
	<-entered
	go func() { defer wg.Done(); p.Rescan(ctx, true) }()
	select {
	case i := <-entered:
		t.Errorf("rescan %d ran while rescan 1 was running", i)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if got := pubNames(p.Devices()); n.Load() != 2 || !slices.Equal(got, []string{pubLocalDeviceName}) {
		t.Fatalf("%d rescans, devices %v", n.Load(), got)
	}
}

// TestPoolWatch pins the watch of the publisher: it rescans while a memdev
// is held, and also without udev events every Poll, so that a local
// memdev that a returning pool slice lists is withdrawn.
func TestPoolWatch(t *testing.T) {
	oldRetry, oldPoll := provenance.ListRetry, provenance.Poll
	t.Cleanup(func() { provenance.ListRetry, provenance.Poll = oldRetry, oldPoll })
	e := newPubSerialsEnv(t)
	e.plug(pubLocalSerial)
	e.publish("0xc1ee2001")
	e.wantScan("local", pubLocalDeviceName)
	e.publish()
	e.plug(pubOtherLocalSerial)
	devs := e.devices()
	p := e.publisher(func(context.Context) (*kernel.Snapshot, error) { return devs, nil })
	ctx := context.Background()
	p.Rescan(ctx, false)
	if got := pubNames(p.Devices()); !slices.Equal(got, []string{pubLocalDeviceName}) {
		t.Fatalf("devices %v", got)
	}
	watch := func(want ...string) {
		t.Helper()
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); e.w.Watch(ctx, p.Rescan, p.LastScan) }()
		defer func() { cancel(); <-done }()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if slices.Equal(pubNames(p.Devices()), want) {
				return
			}
		}
		t.Fatalf("devices %v, expected %v", pubNames(p.Devices()), want)
	}
	// held: the ticks rescan, it is published after the hold-back
	provenance.ListRetry, provenance.Poll = 5*time.Millisecond, time.Hour
	e.advance(provenance.MaxHold)
	watch("memdev-a1", pubLocalDeviceName)
	// pool-c is back and lists the local memdev: a periodic rescan
	// withdraws it
	provenance.Poll = 20 * time.Millisecond
	e.publish("0xc1ee2001", "0xc100e2e1")
	watch("memdev-a1")
}

// poolTestDevices are local memdevs with the serial of a pool device
// (0xc1ee0001) and of a free local device.
func poolTestDevices() *kernel.Snapshot {
	pooled := pubMemdev()
	pooled.Name, pooled.Serial = "mem1", 0xc1ee0001
	return &kernel.Snapshot{Memdevs: []kernel.MemdevInfo{pubMemdev(), pooled}}
}

// scanNames returns the names of the devices that a scan of
// poolTestDevices publishes with the watcher w, and the configuration of
// the publisher after it.
func scanNames(t *testing.T, ctx context.Context, st *store.Store, w *provenance.Watcher) ([]string, PublishConfig) {
	t.Helper()
	p := pubPublisher(t, publishOptions{
		Config:    PublishConfig{IgnoreDRAM: true, ManageMemdevs: true},
		NodeName:  pubNode,
		Scan:      func(context.Context) (*kernel.Snapshot, error) { return poolTestDevices(), nil },
		Watcher:   w,
		Store:     st,
		BlockSize: func() (uint64, error) { return pubBlockSize, nil },
	})
	pubNoErr(t, p.Scan(ctx))
	return pubNames(p.Devices()), p.opts.Config
}

// TestPoolSlicesSerials pins the scan input of the pool slices: a pool
// device is not a local memdev, the others are, also after a failed list
// (the serials of the last one).
func TestPoolSlicesSerials(t *testing.T) {
	client := fake.NewClientset(
		pubResourceSlice("pool", device.PoolDriverName, pubSerialAttr("serial", "0xC1EE0001"), pubSerialAttr("serial", "bogus"), nil),
		pubResourceSlice("other", "other.example.com", pubSerialAttr("other.example.com/serial", "3238060770")),
		pubResourceSlice("node1", device.DriverName, pubSerialAttr("serial", "0xc100e2e1")),
	)
	w := provenance.New(provenance.Options{Client: client, NodeName: pubNode})
	st := pubStore(t)
	ctx := context.Background()
	if got, _ := scanNames(t, ctx, st, w); !slices.Equal(got, []string{pubLocalDeviceName}) {
		t.Fatalf("published %v", got)
	}
	// an API failure keeps the serials of the last list
	client.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	if got, _ := scanNames(t, ctx, st, w); !slices.Equal(got, []string{pubLocalDeviceName}) || !w.Failing() {
		t.Fatalf("published %v after a failed list (failing %v)", got, w.Failing())
	}
}

// TestPoolSlicesFirstListFails pins the scan before the first successful
// list of the pool slices: no local memdev at all, with its warning, and
// the configuration unchanged; after it the local memdevs; without a
// watcher (no pool controllers) every memdev.
func TestPoolSlicesFirstListFails(t *testing.T) {
	logs := klogtest.Capture(t, 0)
	client := fake.NewClientset(pubResourceSlice("pool", device.PoolDriverName, pubSerialAttr("serial", "0xc1ee0001")))
	var fail atomic.Bool
	fail.Store(true)
	client.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	w := provenance.New(provenance.Options{Client: client, NodeName: pubNode})
	st := pubStore(t)
	ctx := context.Background()
	// nothing is known: no local memdev at all, regions stay as they are
	got, cfg := scanNames(t, ctx, st, w)
	if len(got) != 0 || !w.Failing() {
		t.Fatalf("published %v before the first successful list (failing %v)", got, w.Failing())
	}
	if !cfg.ManageMemdevs {
		t.Fatal("the configuration was changed")
	}
	wantLogLineText(t, logs(), "not publishing local memdevs: the devices of the pool controllers are not known")
	fail.Store(false)
	if got, _ := scanNames(t, ctx, st, w); !slices.Equal(got, []string{pubLocalDeviceName}) || w.Failing() {
		t.Fatalf("published %v after the first successful list (failing %v)", got, w.Failing())
	}
	// without pool controllers (no watcher) everything is known
	if got, _ := scanNames(t, ctx, st, nil); !slices.Equal(got, []string{pubLocalDeviceName, "memdev-c1ee0001"}) {
		t.Fatalf("published %v without a watcher", got)
	}
}
