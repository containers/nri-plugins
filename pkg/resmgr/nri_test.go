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
// request, failed is what the runtime reports it could not apply, and sent
// records what we asked it to update.
type testStub struct {
	err    error
	failed []*nriapi.ContainerUpdate
	sent   []*nriapi.ContainerUpdate
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

func TestUpdateContainersRetainsPendingUpdatesOnFailure(t *testing.T) {
	want := errors.New("update failed")
	m, ctr := newTestUpdate(t, &testStub{err: want})

	err := m.nri.updateContainers()
	if err == nil {
		t.Fatal("updateContainers() succeeded, expected failure")
	}
	if !errors.Is(err, want) {
		t.Fatalf("updateContainers() error = %v, want %v", err, want)
	}

	pending := ctr.PeekPendingUpdate()
	if pending == nil {
		t.Fatal("updateContainers() cleared the pending update after failure")
	}
	if got := pending.GetLinux().GetResources().GetCpu().GetShares().GetValue(); got != 42 {
		t.Fatalf("pending CPU shares = %d, want 42", got)
	}
	if !ctr.HasPending(cache.NRI) {
		t.Fatal("updateContainers() cleared pending container markers after failure")
	}
}

func TestUpdateContainersClearsDeliveredUpdates(t *testing.T) {
	stub := &testStub{}
	m, ctr := newTestUpdate(t, stub)

	if err := m.nri.updateContainers(); err != nil {
		t.Fatalf("updateContainers() failed: %v", err)
	}

	if len(stub.sent) != 1 {
		t.Fatalf("runtime got %d update(s), want 1", len(stub.sent))
	}
	if got := stub.sent[0].GetLinux().GetResources().GetCpu().GetShares().GetValue(); got != 42 {
		t.Fatalf("updated CPU shares = %d, want 42", got)
	}

	if ctr.PeekPendingUpdate() != nil {
		t.Error("updateContainers() left the delivered update pending")
	}
	if ctr.HasPending(cache.NRI) {
		t.Error("updateContainers() left pending container markers behind")
	}
}

// TestUpdateContainersRetainsUpdatesTheRuntimeRejected covers the updates which
// come back in the runtime's answer instead of failing the request: those
// containers were not updated either, so they fail the push and their updates
// stay pending just the same.
func TestUpdateContainersRetainsUpdatesTheRuntimeRejected(t *testing.T) {
	stub := &testStub{}
	m, ctr := newTestUpdate(t, stub)
	stub.failed = []*nriapi.ContainerUpdate{{ContainerId: ctr.GetID()}}

	if err := m.nri.updateContainers(); err == nil {
		t.Error("updateContainers() succeeded with an update the runtime rejected")
	}

	if ctr.PeekPendingUpdate() == nil {
		t.Error("updateContainers() cleared the update the runtime rejected")
	}
	if !ctr.HasPending(cache.NRI) {
		t.Error("updateContainers() cleared the pending container markers of a rejected update")
	}
}
