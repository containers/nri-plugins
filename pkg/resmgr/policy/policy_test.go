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

package policy

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
)

// newTestCache creates a real, temp-dir-backed cache for tests that need a
// non-nil cache.Cache (e.g. because Start() drives metrics collection which
// reads from it).
func newTestCache(t *testing.T) cache.Cache {
	t.Helper()
	// Use a not-yet-existing subdirectory of t.TempDir(): NewCache
	// validates the permissions of a pre-existing CacheDir and rejects
	// group-writable ones, but t.TempDir() itself is created 0777&^umask,
	// which is group-writable under a permissive umask. Handing NewCache a
	// fresh path lets it create the directory itself with the permissions
	// it expects.
	c, err := cache.NewCache(cache.Options{CacheDir: filepath.Join(t.TempDir(), "cache")})
	require.NoError(t, err)
	return c
}

// mockBackend is a minimal Backend implementation that records calls and
// the options it was set up with, for use by policy-level unit tests.
type mockBackend struct {
	setupOpts           *BackendOptions
	setupErr            error
	startErr            error
	stopErr             error
	reconfigErr         error
	postReconfigureErr  error
	startCalled         bool
	stopCalled          bool
	setupCalled         bool
	postReconfigureCall int
	callOrder           *[]string
	onStart             func()
	onSetup             func(*BackendOptions)
	onPostReconfigure   func()
}

func (m *mockBackend) Name() string        { return "mock" }
func (m *mockBackend) Description() string { return "mock backend for tests" }

func (m *mockBackend) Setup(opts *BackendOptions) error {
	m.setupCalled = true
	m.setupOpts = opts
	if m.callOrder != nil {
		*m.callOrder = append(*m.callOrder, "setup")
	}
	if m.onSetup != nil {
		m.onSetup(opts)
	}
	return m.setupErr
}

func (m *mockBackend) Reconfigure(interface{}) error { return m.reconfigErr }

func (m *mockBackend) PostReconfigure() error {
	m.postReconfigureCall++
	if m.callOrder != nil {
		*m.callOrder = append(*m.callOrder, "post-reconfigure")
	}
	if m.onPostReconfigure != nil {
		m.onPostReconfigure()
	}
	return m.postReconfigureErr
}

func (m *mockBackend) Start() error {
	m.startCalled = true
	if m.callOrder != nil {
		*m.callOrder = append(*m.callOrder, "start")
	}
	if m.onStart != nil {
		m.onStart()
	}
	return m.startErr
}

func (m *mockBackend) Stop() error {
	m.stopCalled = true
	if m.callOrder != nil {
		*m.callOrder = append(*m.callOrder, "stop")
	}
	return m.stopErr
}

func (m *mockBackend) Sync([]cache.Container, []cache.Container) error { return nil }
func (m *mockBackend) AllocateResources(cache.Container) error         { return nil }
func (m *mockBackend) ReleaseResources(cache.Container) error          { return nil }
func (m *mockBackend) UpdateResources(cache.Container) error           { return nil }
func (m *mockBackend) HandleEvent(*events.Policy) (bool, error)        { return false, nil }
func (m *mockBackend) ExportResourceData(cache.Container) map[string]string {
	return nil
}
func (m *mockBackend) GetTopologyZones() []*TopologyZone { return nil }
func (m *mockBackend) GetExtendedResources() map[string]*resource.Quantity {
	return nil
}

var _ Backend = &mockBackend{}

func TestPolicyStopForwardsToBackend(t *testing.T) {
	backend := &mockBackend{}
	p, err := NewPolicy(backend, newTestCache(t), &Options{})
	require.NoError(t, err)

	require.NoError(t, p.Start(nil))
	assert.True(t, backend.startCalled)

	require.NoError(t, p.Stop())
	assert.True(t, backend.stopCalled)
}

func TestPolicyStopNilActiveBackendDoesNotPanic(t *testing.T) {
	p := &policy{}
	assert.NotPanics(t, func() {
		err := p.Stop()
		assert.NoError(t, err)
	})
}

// TestPolicyPostReconfigureForwardsToBackend verifies that Policy.PostReconfigure
// forwards to the active backend's PostReconfigure, and propagates its error.
func TestPolicyPostReconfigureForwardsToBackend(t *testing.T) {
	backend := &mockBackend{postReconfigureErr: assert.AnError}
	p, err := NewPolicy(backend, newTestCache(t), &Options{})
	require.NoError(t, err)

	err = p.PostReconfigure()
	assert.Equal(t, assert.AnError, err)
	assert.Equal(t, 1, backend.postReconfigureCall)
}

// TestPolicyPostReconfigureNilActiveBackendDoesNotPanic verifies that
// PostReconfigure is safe to call on a policy with no active backend set
// (mirrors TestPolicyStopNilActiveBackendDoesNotPanic).
func TestPolicyPostReconfigureNilActiveBackendDoesNotPanic(t *testing.T) {
	p := &policy{}
	assert.NotPanics(t, func() {
		err := p.PostReconfigure()
		assert.NoError(t, err)
	})
}

func TestPolicyStartForwardsKubeClientFnNodeNameAndWithLock(t *testing.T) {
	backend := &mockBackend{}
	wantClient := fakeKubeClient{}
	var lockCalls int
	withLock := func(f func()) {
		lockCalls++
		f()
	}

	p, err := NewPolicy(backend, newTestCache(t), &Options{
		KubeClientFn: func() kubernetes.Interface { return wantClient },
		NodeName:     "node-under-test",
		WithLock:     withLock,
	})
	require.NoError(t, err)
	require.NoError(t, p.Start(nil))

	require.NotNil(t, backend.setupOpts)
	require.NotNil(t, backend.setupOpts.KubeClientFn)
	assert.Equal(t, wantClient, backend.setupOpts.KubeClientFn())
	assert.Equal(t, "node-under-test", backend.setupOpts.NodeName)
	require.NotNil(t, backend.setupOpts.WithLock)

	// Exercise the forwarded WithLock: it should invoke the callback and
	// go through the resmgr-supplied withLock, not some other function.
	called := false
	backend.setupOpts.WithLock(func() { called = true })
	assert.True(t, called)
	assert.Equal(t, 1, lockCalls)
}

func TestPolicyStartKubeClientFnNilWhenNoClient(t *testing.T) {
	backend := &mockBackend{}
	p, err := NewPolicy(backend, newTestCache(t), &Options{
		KubeClientFn: func() kubernetes.Interface { return nil },
	})
	require.NoError(t, err)
	require.NoError(t, p.Start(nil))

	require.NotNil(t, backend.setupOpts.KubeClientFn)
	// Must be observed as a genuinely nil interface by the backend, not a
	// non-nil interface wrapping a typed nil.
	assert.Nil(t, backend.setupOpts.KubeClientFn())
}

// lockContractStub is a WithLock stand-in that panics if invoked while
// already "held", i.e. re-entrantly. It is used to assert that a backend's
// lifecycle calls made through WithLock never nest.
type lockContractStub struct {
	mu   sync.Mutex
	held bool
}

func (s *lockContractStub) run(f func()) {
	s.mu.Lock()
	if s.held {
		s.mu.Unlock()
		panic("WithLock invoked re-entrantly")
	}
	s.held = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.held = false
		s.mu.Unlock()
	}()

	f()
}

// TestLockContractWithLockNotReentrant asserts that a backend driving two
// logically-sequential operations through WithLock (e.g. a future DRA
// plugin's Start() then PublishResources()) never nests those calls: the
// stub above would panic if it did.
func TestLockContractWithLockNotReentrant(t *testing.T) {
	stub := &lockContractStub{}
	var order []string

	backend := &mockBackend{
		onStart: func() {
			// Simulate a backend that runs two operations under the
			// resource manager's write lock, sequentially rather than
			// nested.
			stub.run(func() { order = append(order, "locked-op-1") })
			stub.run(func() { order = append(order, "locked-op-2") })
		},
	}

	p, err := NewPolicy(backend, newTestCache(t), &Options{WithLock: stub.run})
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		require.NoError(t, p.Start(nil))
	})
	assert.Equal(t, []string{"locked-op-1", "locked-op-2"}, order)
	assert.False(t, stub.held)
}

// TestLockContractReentrantCallPanics is a sanity check that the stub
// itself actually detects re-entrancy (guards against a vacuously-true
// contract test above).
func TestLockContractReentrantCallPanics(t *testing.T) {
	stub := &lockContractStub{}
	assert.Panics(t, func() {
		stub.run(func() {
			stub.run(func() {})
		})
	})
}

// fakeKubeClient is a minimal kubernetes.Interface stand-in used only for
// identity comparison in tests; its methods are never called.
type fakeKubeClient struct {
	kubernetes.Interface
}
