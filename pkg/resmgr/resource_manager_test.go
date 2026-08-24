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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/containers/nri-plugins/pkg/agent"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	"github.com/containers/nri-plugins/pkg/resmgr/policy"
)

// fakePolicy is a minimal policy.Policy implementation that records Stop()
// calls (and, optionally, runs a probe from within Stop()) for use by
// resmgr-level shutdown-ordering tests.
type fakePolicy struct {
	stopCalled            int
	stopErr               error
	onStop                func()
	postReconfigureCalled int
	postReconfigureErr    error
	onPostReconfigure     func()
}

func (f *fakePolicy) ActivePolicy() string    { return "fake" }
func (f *fakePolicy) Start(interface{}) error { return nil }
func (f *fakePolicy) Stop() error {
	f.stopCalled++
	if f.onStop != nil {
		f.onStop()
	}
	return f.stopErr
}
func (f *fakePolicy) Reconfigure(interface{}) error { return nil }
func (f *fakePolicy) PostReconfigure() error {
	f.postReconfigureCalled++
	if f.onPostReconfigure != nil {
		f.onPostReconfigure()
	}
	return f.postReconfigureErr
}
func (f *fakePolicy) Sync([]cache.Container, []cache.Container) error     { return nil }
func (f *fakePolicy) AllocateResources(cache.Container) error             { return nil }
func (f *fakePolicy) ReleaseResources(cache.Container) error              { return nil }
func (f *fakePolicy) UpdateResources(cache.Container) error               { return nil }
func (f *fakePolicy) HandleEvent(*events.Policy) (bool, error)            { return false, nil }
func (f *fakePolicy) ExportResourceData(cache.Container)                  {}
func (f *fakePolicy) GetTopologyZones() []*policy.TopologyZone            { return nil }
func (f *fakePolicy) GetExtendedResources() map[string]*resource.Quantity { return nil }

var _ policy.Policy = &fakePolicy{}

// TestResmgrStopCallsPolicyStop verifies that resmgr.Stop() reaches the
// active policy's Stop() method (which, in turn, is how Backend.Stop() --
// e.g. the DRA plugin shutdown -- gets invoked).
func TestResmgrStopCallsPolicyStop(t *testing.T) {
	fp := &fakePolicy{}
	m := &resmgr{policy: fp}

	m.Stop()

	assert.Equal(t, 1, fp.stopCalled)
}

// TestResmgrStopCallsPolicyStopBeforeLock verifies that resmgr.Stop() calls
// policy.Stop() before acquiring the resource manager's write lock: an
// in-flight Prepare/AllocateResources call may be holding that lock via
// WithLock, and policy.Stop() must not have to wait for it.
func TestResmgrStopCallsPolicyStopBeforeLock(t *testing.T) {
	m := &resmgr{}
	fp := &fakePolicy{
		onStop: func() {
			// If resmgr.Stop() had already acquired the write lock before
			// calling policy.Stop(), this TryLock would fail.
			locked := m.TryLock()
			require.True(t, locked, "resmgr write lock must not be held while policy.Stop() runs")
			m.Unlock()
		},
	}
	m.policy = fp

	m.Stop()

	assert.Equal(t, 1, fp.stopCalled)
}

// TestResmgrStopNilPolicyDoesNotPanic verifies that Stop() is safe to call
// when Start() was never called (m.policy is nil).
func TestResmgrStopNilPolicyDoesNotPanic(t *testing.T) {
	m := &resmgr{}
	assert.NotPanics(t, func() {
		m.Stop()
	})
}

// TestWithWriteLockRunsCallbackUnderLock verifies that withWriteLock
// acquires the resource manager's write lock for the duration of the
// callback.
func TestWithWriteLockRunsCallbackUnderLock(t *testing.T) {
	m := &resmgr{}
	ran := false

	m.withWriteLock(func() {
		ran = true
		assert.False(t, m.TryLock(), "lock must be held while the withWriteLock callback runs")
	})

	assert.True(t, ran)
	// The lock must have been released once withWriteLock returned.
	assert.True(t, m.TryLock())
	m.Unlock()
}

// TestKubeClientFnNilWhenAgentHasNoClient verifies the typed-nil-safe
// wrapper: in local-config mode agent.KubeClient() returns a nil
// *client.Client, and m.kubeClientFn() must surface that as a genuinely
// nil kubernetes.Interface rather than a non-nil interface wrapping a
// typed nil pointer.
func TestKubeClientFnNilWhenAgentHasNoClient(t *testing.T) {
	agt, err := agent.New(agent.TopologyAwareConfigInterface(), agent.WithConfigFile("/nonexistent/config.yaml"))
	require.NoError(t, err)

	m := &resmgr{agent: agt}

	assert.Nil(t, m.kubeClientFn())
}

// TestPostReconfigureCalledOnSuccess verifies that m.postReconfigure calls
// the active policy's PostReconfigure when handed a nil reconfErr (i.e. the
// preceding m.reconfigure() call succeeded).
func TestPostReconfigureCalledOnSuccess(t *testing.T) {
	fp := &fakePolicy{}
	m := &resmgr{policy: fp}

	m.postReconfigure(nil)

	assert.Equal(t, 1, fp.postReconfigureCalled)
}

// TestPostReconfigureNotCalledOnReconfigureError verifies that
// m.postReconfigure is a no-op when handed a non-nil reconfErr: a failed
// Reconfigure must not trigger DRA (or any other backend's) PostReconfigure
// follow-up work.
func TestPostReconfigureNotCalledOnReconfigureError(t *testing.T) {
	fp := &fakePolicy{}
	m := &resmgr{policy: fp}

	m.postReconfigure(errors.New("reconfigure failed"))

	assert.Equal(t, 0, fp.postReconfigureCalled)
}

// TestPostReconfigureNotCalledWhileLockHeld verifies that m.postReconfigure
// runs without the resource manager's write lock held -- mirrors
// TestResmgrStopCallsPolicyStopBeforeLock. In the real updateConfig() call
// site, m.reconfigure() has already released the lock (via its own deferred
// Unlock) by the time m.postReconfigure() runs; this test checks that
// invariant directly at the postReconfigure call, not just by inference from
// sequential code ordering.
func TestPostReconfigureNotCalledWhileLockHeld(t *testing.T) {
	m := &resmgr{}
	fp := &fakePolicy{
		onPostReconfigure: func() {
			locked := m.TryLock()
			require.True(t, locked, "resmgr write lock must not be held while policy.PostReconfigure() runs")
			m.Unlock()
		},
	}
	m.policy = fp

	m.postReconfigure(nil)

	assert.Equal(t, 1, fp.postReconfigureCalled)
}
