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

package resmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	nriapi "github.com/containerd/nri/pkg/api"
	nrilog "github.com/containerd/nri/pkg/log"
	nristub "github.com/containerd/nri/pkg/stub"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
)

// testPolicy is a policy which only exports resource data. Embedding the
// interface leaves the rest nil, which is fine as long as nothing calls them.
type testPolicy struct {
	policy.Policy
}

func (testPolicy) ExportResourceData(cache.Container) {}

// testStub is an NRI stub which only serves container updates: err fails the
// request, failed is what the runtime reports it could not apply, sent records
// what we asked it to update, and during runs while the runtime has the request.
type testStub struct {
	err    error
	failed []*nriapi.ContainerUpdate
	sent   []*nriapi.ContainerUpdate
	during func()
}

var _ nristub.Stub = (*testStub)(nil)

func (s *testStub) Run(context.Context) error          { return nil }
func (s *testStub) Start(context.Context) error        { return nil }
func (s *testStub) Stop()                              {}
func (s *testStub) Wait()                              {}
func (s *testStub) RegistrationTimeout() time.Duration { return 0 }
func (s *testStub) RequestTimeout() time.Duration      { return 0 }
func (s *testStub) Logger() nrilog.Logger              { return nil }
func (s *testStub) RuntimeNRIVersion() string          { return "" }
func (s *testStub) PluginNRIVersion() string           { return "" }
func (s *testStub) UpdateContainers(updates []*nriapi.ContainerUpdate) ([]*nriapi.ContainerUpdate, error) {
	s.sent = append(s.sent, updates...)
	if s.during != nil {
		s.during()
	}
	return s.failed, s.err
}

// newTestUpdate returns a resource manager which updates containers through the
// given stub, and a running container with a pending update of 42 CPU shares,
// as an out of band allocation change would leave behind.
func newTestUpdate(t *testing.T, stub *testStub) (*resmgr, cache.Container) {
	t.Helper()

	// The cache refuses a directory others may write to, which is what a
	// temporary directory is here.
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(cacheDir, 0700); err != nil {
		t.Fatalf("failed to create cache directory: %v", err)
	}

	cch, err := cache.NewCache(cache.Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	pod := cch.InsertPod(&nriapi.PodSandbox{
		Id:        "test-pod",
		Uid:       "uid-test-pod",
		Name:      "test-pod",
		Namespace: "default",
		Linux: &nriapi.LinuxPodSandbox{
			CgroupParent: "/test/pod",
		},
	}, nil)
	if pod == nil {
		t.Fatal("failed to insert pod into cache")
	}

	ctr, err := cch.InsertContainer(&nriapi.Container{
		Id:           "test-container",
		PodSandboxId: pod.GetID(),
		Name:         "test-container",
		State:        cache.ContainerStateRunning,
	})
	if err != nil {
		t.Fatalf("failed to insert container into cache: %v", err)
	}

	ctr.SetCPUShares(42)

	m := &resmgr{
		agent:  newTestAgent(t, "test-node"),
		cache:  cch,
		cfg:    &cfgapi.TopologyAwarePolicy{},
		policy: testPolicy{},
	}
	m.nri = &nriPlugin{
		resmgr: m,
		stub:   stub,
	}

	return m, ctr
}

// TestPushPendingUpdatesReleasesTheLock covers an NRI request served while the
// runtime has our push: it must get the lock, and it must not carry the update
// in flight in its response.
func TestPushPendingUpdatesReleasesTheLock(t *testing.T) {
	stub := &testStub{}
	m, ctr := newTestUpdate(t, stub)
	stub.during = func() {
		if !m.TryLock() {
			t.Error("pushPendingUpdates() held the lock while the runtime had the push")
			return
		}
		defer m.Unlock()

		if updates := m.nri.peekPendingUpdates(nil); len(updates) != 0 {
			t.Errorf("an NRI response would carry %d update(s) in flight, want 0", len(updates))
		}
	}

	if err := m.nri.pushPendingUpdates(); err != nil {
		t.Fatalf("pushPendingUpdates() failed: %v", err)
	}

	if len(stub.sent) != 1 {
		t.Fatalf("runtime got %d update(s), want 1", len(stub.sent))
	}
	if ctr.PeekPendingUpdate() != nil {
		t.Error("pushPendingUpdates() left the delivered update pending")
	}
	if ctr.HasPending(cache.NRI) {
		t.Error("pushPendingUpdates() left pending container markers behind")
	}
	if len(m.nri.inFlight) != 0 {
		t.Errorf("pushPendingUpdates() left %d update(s) in flight", len(m.nri.inFlight))
	}
}

// TestPushPendingUpdatesPushesUpdatesChangedInFlight covers an update which
// changes while the runtime has the push: the runtime got the older one, so the
// newer one must be pushed too.
func TestPushPendingUpdatesPushesUpdatesChangedInFlight(t *testing.T) {
	stub := &testStub{}
	m, ctr := newTestUpdate(t, stub)
	stub.during = func() {
		stub.during = nil
		if !m.TryLock() {
			t.Error("pushPendingUpdates() held the lock while the runtime had the push")
			return
		}
		defer m.Unlock()
		ctr.SetCPUShares(43)
	}

	if err := m.nri.pushPendingUpdates(); err != nil {
		t.Fatalf("pushPendingUpdates() failed: %v", err)
	}

	if len(stub.sent) != 2 {
		t.Fatalf("runtime got %d update(s), want 2", len(stub.sent))
	}
	for i, want := range []uint64{42, 43} {
		if got := stub.sent[i].GetLinux().GetResources().GetCpu().GetShares().GetValue(); got != want {
			t.Errorf("update %d has CPU shares %d, want %d", i, got, want)
		}
	}
	if ctr.PeekPendingUpdate() != nil {
		t.Error("pushPendingUpdates() left the newer update pending")
	}
	if len(m.nri.inFlight) != 0 {
		t.Errorf("pushPendingUpdates() left %d update(s) in flight", len(m.nri.inFlight))
	}
}

// TestPushPendingUpdatesRetainsUpdatesTheRuntimeRejected covers an update the
// runtime rejects: it stays pending, and it is pushed only once.
func TestPushPendingUpdatesRetainsUpdatesTheRuntimeRejected(t *testing.T) {
	stub := &testStub{}
	m, ctr := newTestUpdate(t, stub)
	stub.failed = []*nriapi.ContainerUpdate{{ContainerId: ctr.GetID()}}

	if err := m.nri.pushPendingUpdates(); err == nil {
		t.Error("pushPendingUpdates() succeeded with an update the runtime rejected")
	}

	if len(stub.sent) != 1 {
		t.Errorf("runtime got %d update(s), want 1", len(stub.sent))
	}
	if ctr.PeekPendingUpdate() == nil {
		t.Error("pushPendingUpdates() cleared the update the runtime rejected")
	}
	if !ctr.HasPending(cache.NRI) {
		t.Error("pushPendingUpdates() cleared the pending container markers of a rejected update")
	}
}

func TestPushPendingUpdatesRetainsPendingUpdatesOnFailure(t *testing.T) {
	want := errors.New("update failed")
	m, ctr := newTestUpdate(t, &testStub{err: want})

	if err := m.nri.pushPendingUpdates(); !errors.Is(err, want) {
		t.Fatalf("pushPendingUpdates() error = %v, want %v", err, want)
	}

	if ctr.PeekPendingUpdate() == nil {
		t.Error("pushPendingUpdates() cleared the pending update after failure")
	}
	if !ctr.HasPending(cache.NRI) {
		t.Error("pushPendingUpdates() cleared pending container markers after failure")
	}
	if len(m.nri.inFlight) != 0 {
		t.Errorf("pushPendingUpdates() left %d update(s) in flight", len(m.nri.inFlight))
	}
}

// TestPushPendingUpdatesWaitsForPushInFlight covers two overlapping pushes: the
// second one must not return before the update the first one has in flight
// reaches the runtime, and it must fail when the runtime rejects that update.
func TestPushPendingUpdatesWaitsForPushInFlight(t *testing.T) {
	stub := &testStub{}
	m, ctr := newTestUpdate(t, stub)
	stub.failed = []*nriapi.ContainerUpdate{{ContainerId: ctr.GetID()}}

	var secondErr error
	done := make(chan struct{})
	stub.during = func() {
		stub.during = nil
		go func() {
			secondErr = m.nri.pushPendingUpdates()
			close(done)
		}()
		select {
		case <-done:
			t.Error("a second push returned while the first one was in flight")
		case <-time.After(100 * time.Millisecond):
		}
	}

	if err := m.nri.pushPendingUpdates(); err == nil {
		t.Error("the first push succeeded with an update the runtime rejected")
	}
	<-done
	if secondErr == nil {
		t.Error("the second push succeeded with an update the runtime rejected")
	}
}
