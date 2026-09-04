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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/containers/nri-plugins/pkg/resmgr/cache"
	"github.com/containers/nri-plugins/pkg/resmgr/events"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
)

// fakeResourceManager is a minimal ResourceManager implementation that
// records the order in which Start()/Stop() are invoked, so tests can
// assert that Main.Run() reaches Stop() after Start() returns.
type fakeResourceManager struct {
	startErr error
	calls    []string
}

func (f *fakeResourceManager) Start() error {
	f.calls = append(f.calls, "start")
	return f.startErr
}

func (f *fakeResourceManager) Stop() {
	f.calls = append(f.calls, "stop")
}

func (f *fakeResourceManager) SendEvent(interface{}) error { return nil }

// fakeBackend is a minimal policy.Backend implementation, just enough to
// satisfy Main.Run()'s use of m.policy.Name() (for tracing identity).
type fakeBackend struct{}

func (fakeBackend) Name() string                                    { return "fake" }
func (fakeBackend) Description() string                             { return "fake backend for tests" }
func (fakeBackend) Setup(*policyapi.BackendOptions) error           { return nil }
func (fakeBackend) Reconfigure(interface{}) error                   { return nil }
func (fakeBackend) PostReconfigure() error                          { return nil }
func (fakeBackend) Start() error                                    { return nil }
func (fakeBackend) Stop() error                                     { return nil }
func (fakeBackend) Sync([]cache.Container, []cache.Container) error { return nil }
func (fakeBackend) AllocateResources(cache.Container) error         { return nil }
func (fakeBackend) ReleaseResources(cache.Container) error          { return nil }
func (fakeBackend) UpdateResources(cache.Container) error           { return nil }
func (fakeBackend) HandleEvent(*events.Policy) (bool, error)        { return false, nil }
func (fakeBackend) ExportResourceData(cache.Container) map[string]string {
	return nil
}
func (fakeBackend) GetTopologyZones() []*policyapi.TopologyZone { return nil }
func (fakeBackend) GetExtendedResources() map[string]*resource.Quantity {
	return nil
}

var _ policyapi.Backend = fakeBackend{}

// TestMainRunStopsResourceManagerAfterStartReturns verifies that Main.Run()
// calls mgr.Stop() once mgr.Start() returns, so that shutdown reaches the
// active policy's Backend.Stop() (e.g. the DRA plugin) even though nothing
// in the SIGTERM/SIGINT path calls mgr.Stop() directly.
func TestMainRunStopsResourceManagerAfterStartReturns(t *testing.T) {
	mgr := &fakeResourceManager{}
	m := &Main{
		policy: fakeBackend{},
		mgr:    mgr,
	}

	err := m.Run()

	require.NoError(t, err)
	assert.Equal(t, []string{"start", "stop"}, mgr.calls)
}

// TestMainRunStopsResourceManagerEvenOnStartError verifies Stop() is still
// reached (and the original Start() error still surfaces) when Start()
// itself fails.
func TestMainRunStopsResourceManagerEvenOnStartError(t *testing.T) {
	startErr := assert.AnError
	mgr := &fakeResourceManager{startErr: startErr}
	m := &Main{
		policy: fakeBackend{},
		mgr:    mgr,
	}

	err := m.Run()

	assert.ErrorIs(t, err, startErr)
	assert.Equal(t, []string{"start", "stop"}, mgr.calls)
}
