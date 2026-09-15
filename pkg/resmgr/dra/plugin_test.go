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
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

const (
	testDriverName = "test.nri.io"
	testNodeName   = "test-node"
)

// testOwner counts how often its lock was taken and records the shutdowns it
// was asked for. The lock is a real one, so a handler taking it twice deadlocks
// instead of passing the test.
type testOwner struct {
	sync.Mutex
	locks   int
	unlocks int

	shutdownMu sync.Mutex // the plugin asks from its own goroutines
	shutdowns  []string
}

func (o *testOwner) Lock() {
	o.Mutex.Lock()
	o.locks++
}

func (o *testOwner) Unlock() {
	o.unlocks++
	o.Mutex.Unlock()
}

func (o *testOwner) RequestShutdown(reason string) {
	o.shutdownMu.Lock()
	defer o.shutdownMu.Unlock()
	o.shutdowns = append(o.shutdowns, reason)
}

func (o *testOwner) shutdownRequests() []string {
	o.shutdownMu.Lock()
	defer o.shutdownMu.Unlock()
	return slices.Clone(o.shutdowns)
}

// newTestPlugin creates a plugin with sockets under the test's own directories,
// so no kubelet is needed. A shutdown request fails the test: no test using this
// is expected to provoke one.
func newTestPlugin(t *testing.T, client kubernetes.Interface) (*Plugin, *testOwner) {
	t.Helper()

	owner := &testOwner{}
	p, err := New(testDriverName, Options{
		NodeName:      testNodeName,
		KubeClient:    client,
		Owner:         owner,
		RegistrarDir:  t.TempDir(),
		PluginDataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	t.Cleanup(func() {
		if asked := owner.shutdownRequests(); len(asked) > 0 {
			t.Errorf("unexpected shutdown request(s): %v", asked)
		}
	})

	return p, owner
}

// newTestClient returns a fake clientset which knows about our node and records
// every published ResourceSlice.
func newTestClient() (kubernetes.Interface, func() []*resourceapi.ResourceSlice) {
	client := fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName},
	})

	var (
		mutex     sync.Mutex
		published []*resourceapi.ResourceSlice
	)

	client.PrependReactor("create", "resourceslices",
		func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			if slice, ok := action.(k8stesting.CreateAction).GetObject().(*resourceapi.ResourceSlice); ok {
				mutex.Lock()
				published = append(published, slice.DeepCopy())
				mutex.Unlock()
			}
			return false, nil, nil // let the default tracker handle it, too
		},
	)

	return client, func() []*resourceapi.ResourceSlice {
		mutex.Lock()
		defer mutex.Unlock()
		return published
	}
}

func TestNew(t *testing.T) {
	valid := Options{
		NodeName:   testNodeName,
		KubeClient: fake.NewClientset(),
		Owner:      &testOwner{},
	}

	for _, tc := range []struct {
		name       string
		driverName string
		options    func(*Options)
		fail       bool
	}{
		{
			name:       "valid options",
			driverName: testDriverName,
		},
		{
			name:       "empty driver name",
			driverName: "",
			fail:       true,
		},
		{
			name:       "empty node name",
			driverName: testDriverName,
			options:    func(o *Options) { o.NodeName = "" },
			fail:       true,
		},
		{
			name:       "nil kube client",
			driverName: testDriverName,
			options:    func(o *Options) { o.KubeClient = nil },
			fail:       true,
		},
		{
			name:       "nil owner",
			driverName: testDriverName,
			options:    func(o *Options) { o.Owner = nil },
			fail:       true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := valid
			if tc.options != nil {
				tc.options(&options)
			}

			p, err := New(tc.driverName, options)

			switch {
			case tc.fail && err == nil:
				t.Errorf("New() succeeded, expected an error")
			case !tc.fail && err != nil:
				t.Errorf("New() failed: %v", err)
			case !tc.fail && p == nil:
				t.Errorf("New() returned nil plugin without an error")
			}
		})
	}
}

// TestNewPluginDataDirDefault verifies the default plugin directory is derived
// from the driver name.
func TestNewPluginDataDirDefault(t *testing.T) {
	p, err := New(testDriverName, Options{
		NodeName:   testNodeName,
		KubeClient: fake.NewClientset(),
		Owner:      &testOwner{},
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	expected := kubeletplugin.KubeletPluginsDir + "/" + testDriverName
	if p.pluginDataDir != expected {
		t.Errorf("plugin directory is %q, expected %q", p.pluginDataDir, expected)
	}
}

// TestStartPublishesEmptySlice verifies that starting the plugin registers the
// driver and publishes a single ResourceSlice with no devices, so that a
// registered driver is visible in the cluster.
func TestStartPublishesEmptySlice(t *testing.T) {
	client, published := newTestClient()
	p, _ := newTestPlugin(t, client)

	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer p.Stop()

	// ResourceSlices are created asynchronously by the resourceslice controller.
	var slices []*resourceapi.ResourceSlice
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if slices = published(); len(slices) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(slices) == 0 {
		t.Fatal("no ResourceSlice was published")
	}
	if driver := slices[0].Spec.Driver; driver != testDriverName {
		t.Errorf("published ResourceSlice for driver %q, expected %q", driver, testDriverName)
	}
	if devices := slices[0].Spec.Devices; len(devices) != 0 {
		t.Errorf("published ResourceSlice with %d devices, expected none", len(devices))
	}
}

// TestStartTwice verifies a started plugin refuses to start again.
func TestStartTwice(t *testing.T) {
	client, _ := newTestClient()
	p, _ := newTestPlugin(t, client)

	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer p.Stop()

	if err := p.Start(t.Context()); err == nil {
		t.Error("second Start() succeeded, expected an error")
	}
}

// TestStopIdempotent verifies Stop can be called repeatedly, and on a plugin
// which was never started, and on no plugin at all. resmgr stops the plugin on
// every shutdown path, whether it got as far as starting it or not.
func TestStopIdempotent(t *testing.T) {
	client, _ := newTestClient()
	p, _ := newTestPlugin(t, client)

	p.Stop() // never started

	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	p.Stop()
	p.Stop()

	var none *Plugin
	none.Stop()
}

// TestStartAfterStop verifies the plugin can be restarted, which is what a
// configuration change that turns DRA off and on again amounts to.
func TestStartAfterStop(t *testing.T) {
	client, _ := newTestClient()
	p, _ := newTestPlugin(t, client)

	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	p.Stop()

	if err := p.Start(t.Context()); err != nil {
		t.Errorf("Start() after Stop() failed: %v", err)
	}
	p.Stop()
}

// TestPrepareResourceClaims verifies claims are answered with empty results,
// under the owner's lock.
func TestPrepareResourceClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims []*resourceapi.ResourceClaim
	}{
		{
			name: "no claims",
		},
		{
			name: "two claims",
			claims: []*resourceapi.ResourceClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "claim-1", UID: types.UID("uid-1")}},
				{ObjectMeta: metav1.ObjectMeta{Name: "claim-2", UID: types.UID("uid-2")}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, owner := newTestPlugin(t, fake.NewClientset())

			result, err := p.PrepareResourceClaims(t.Context(), tc.claims)
			if err != nil {
				t.Fatalf("PrepareResourceClaims() failed: %v", err)
			}

			if len(result) != len(tc.claims) {
				t.Errorf("got %d results, expected %d", len(result), len(tc.claims))
			}
			for _, claim := range tc.claims {
				prepared, ok := result[claim.UID]
				if !ok {
					t.Errorf("claim %s has no result", claim.UID)
					continue
				}
				if prepared.Err != nil {
					t.Errorf("claim %s failed: %v", claim.UID, prepared.Err)
				}
				if len(prepared.Devices) != 0 {
					t.Errorf("claim %s got %d devices, expected none", claim.UID, len(prepared.Devices))
				}
			}

			if owner.locks != 1 || owner.unlocks != 1 {
				t.Errorf("locked %d times and unlocked %d times, expected once each",
					owner.locks, owner.unlocks)
			}
		})
	}
}

// TestUnprepareResourceClaims verifies claims are answered without errors,
// under the owner's lock.
func TestUnprepareResourceClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims []kubeletplugin.NamespacedObject
	}{
		{
			name: "no claims",
		},
		{
			name: "two claims",
			claims: []kubeletplugin.NamespacedObject{
				{UID: types.UID("uid-1")},
				{UID: types.UID("uid-2")},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, owner := newTestPlugin(t, fake.NewClientset())

			result, err := p.UnprepareResourceClaims(t.Context(), tc.claims)
			if err != nil {
				t.Fatalf("UnprepareResourceClaims() failed: %v", err)
			}

			if len(result) != len(tc.claims) {
				t.Errorf("got %d results, expected %d", len(result), len(tc.claims))
			}
			for _, claim := range tc.claims {
				unprepared, ok := result[claim.UID]
				if !ok {
					t.Errorf("claim %s has no result", claim.UID)
					continue
				}
				if unprepared != nil {
					t.Errorf("claim %s failed: %v", claim.UID, unprepared)
				}
			}

			if owner.locks != 1 || owner.unlocks != 1 {
				t.Errorf("locked %d times and unlocked %d times, expected once each",
					owner.locks, owner.unlocks)
			}
		})
	}
}

// TestHandleError verifies that only errors the helper cannot recover from make
// us ask for a shutdown, and that the reason we give still says what failed.
func TestHandleError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		fatal bool
	}{
		{
			name: "recoverable publication error",
			err:  fmt.Errorf("%w: %w", kubeletplugin.ErrRecoverable, errors.New("apiserver is down")),
		},
		{
			name:  "server failure",
			err:   errors.New("listener closed"),
			fatal: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := &testOwner{}
			p, err := New(testDriverName, Options{
				NodeName:      testNodeName,
				KubeClient:    fake.NewClientset(),
				Owner:         owner,
				PluginDataDir: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}

			p.HandleError(t.Context(), tc.err, "DRA gRPC server failed")

			asked := owner.shutdownRequests()

			if !tc.fatal {
				if len(asked) != 0 {
					t.Errorf("asked for shutdown %v, expected none", asked)
				}
				return
			}

			if len(asked) != 1 {
				t.Fatalf("asked for %d shutdowns, expected one", len(asked))
			}
			if !strings.Contains(asked[0], tc.err.Error()) {
				t.Errorf("shutdown reason %q, expected it to mention %v", asked[0], tc.err)
			}
			if !strings.Contains(asked[0], "DRA gRPC server failed") {
				t.Errorf("shutdown reason %q, expected it to mention what failed", asked[0])
			}
		})
	}
}

// TestDriverResources verifies the published resources name a single pool for
// our node, with one empty slice in it.
func TestDriverResources(t *testing.T) {
	p, _ := newTestPlugin(t, fake.NewClientset())

	resources := p.driverResources()

	if len(resources.Pools) != 1 {
		t.Fatalf("got %d pools, expected one", len(resources.Pools))
	}
	pool, ok := resources.Pools[testNodeName]
	if !ok {
		t.Fatalf("no pool for node %q", testNodeName)
	}
	if len(pool.Slices) != 1 {
		t.Fatalf("got %d slices, expected one", len(pool.Slices))
	}
	if devices := pool.Slices[0].Devices; len(devices) != 0 {
		t.Errorf("got %d devices, expected none", len(devices))
	}
}
