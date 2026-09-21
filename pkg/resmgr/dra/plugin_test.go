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
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
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

// testDevices returns n uniquely named devices, for the cases which care about
// how many devices there are rather than what they say.
func testDevices(n int) []resourceapi.Device {
	devices := make([]resourceapi.Device, n)
	for i := range devices {
		devices[i] = resourceapi.Device{Name: fmt.Sprintf("cpu-%d", i)}
	}
	return devices
}

// newTestPlugin creates a plugin with sockets under the test's own directories.
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

// newTestClient returns a fake clientset which knows about our node.
func newTestClient() kubernetes.Interface {
	return fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName},
	})
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

// TestPublish verifies that the plugin publishes the devices it was given,
// whether before or after it was started. With no devices it still publishes a
// slice, so that a registered driver is visible in the cluster.
func TestPublish(t *testing.T) {
	devices := []resourceapi.Device{{Name: "cpu-0"}, {Name: "cpu-1"}}

	for _, tc := range []struct {
		name         string
		devices      []resourceapi.Device
		publishLater bool
	}{
		{
			name: "no devices",
		},
		{
			name:    "devices published before start",
			devices: devices,
		},
		{
			name:         "devices published after start",
			devices:      devices,
			publishLater: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient()
			p, _ := newTestPlugin(t, client)

			if !tc.publishLater {
				if err := p.Publish(tc.devices); err != nil {
					t.Fatalf("Publish() failed: %v", err)
				}
			}

			if err := p.Start(t.Context()); err != nil {
				t.Fatalf("Start() failed: %v", err)
			}
			defer p.Stop()

			if tc.publishLater {
				if err := p.Publish(tc.devices); err != nil {
					t.Fatalf("Publish() failed: %v", err)
				}
			}

			// ResourceSlices are published asynchronously by the resourceslice
			// controller.
			var slices []resourceapi.ResourceSlice
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				list, err := client.ResourceV1().ResourceSlices().List(t.Context(), metav1.ListOptions{})
				if err != nil {
					t.Fatalf("failed to list ResourceSlices: %v", err)
				}
				slices = list.Items
				if len(slices) == 1 && reflect.DeepEqual(slices[0].Spec.Devices, tc.devices) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}

			if len(slices) != 1 {
				t.Fatalf("got %d ResourceSlices, expected one", len(slices))
			}
			if driver := slices[0].Spec.Driver; driver != testDriverName {
				t.Errorf("published ResourceSlice for driver %q, expected %q", driver, testDriverName)
			}
			if devices := slices[0].Spec.Devices; !reflect.DeepEqual(devices, tc.devices) {
				t.Errorf("published ResourceSlice with devices %v, expected %v", devices, tc.devices)
			}
		})
	}
}

// TestStartTwice verifies a started plugin refuses to start again.
func TestStartTwice(t *testing.T) {
	client := newTestClient()
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
	client := newTestClient()
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
	client := newTestClient()
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

// TestWatchHealthStatus verifies we decline health reporting in the way the
// kubelet expects, which is what makes it stop asking.
func TestWatchHealthStatus(t *testing.T) {
	p, _ := newTestPlugin(t, fake.NewClientset(), &testPolicy{})

	reports := make(chan kubeletplugin.DeviceHealthReport)
	err := p.WatchHealthStatus(t.Context(), reports)

	if !errors.Is(err, kubeletplugin.ErrHealthNotSupported) {
		t.Errorf("WatchHealthStatus() failed with %v, expected %v",
			err, kubeletplugin.ErrHealthNotSupported)
	}
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

// TestDriverResources verifies the resources to publish name a single pool for
// our node, holding the devices of the policy exactly as the policy described
// them, split into slices which each stay within the device limit.
func TestDriverResources(t *testing.T) {
	numaNode := int64(0)

	for _, tc := range []struct {
		name       string
		devices    []resourceapi.Device
		wantSlices int
	}{
		{
			name:       "no devices",
			wantSlices: 1,
		},
		{
			// The driver must not require attributes, let alone understand
			// them: what a device offers is the policy's business.
			name:       "devices without attributes",
			devices:    []resourceapi.Device{{Name: "cpu-0"}, {Name: "cpu-1"}},
			wantSlices: 1,
		},
		{
			name: "device with attributes and capacity",
			devices: []resourceapi.Device{
				{
					Name: "numa-0",
					Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
						"numaNode": {IntValue: &numaNode},
					},
					Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
						"memory": {Value: resource.MustParse("1Gi")},
					},
				},
			},
			wantSlices: 1,
		},
		{
			name:       "devices exactly filling one slice",
			devices:    testDevices(resourceapi.ResourceSliceMaxDevices),
			wantSlices: 1,
		},
		{
			name:       "one device too many for one slice",
			devices:    testDevices(resourceapi.ResourceSliceMaxDevices + 1),
			wantSlices: 2,
		},
		{
			name:       "devices spanning three slices",
			devices:    testDevices(2*resourceapi.ResourceSliceMaxDevices + 7),
			wantSlices: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestPlugin(t, fake.NewClientset())
			if err := p.Publish(tc.devices); err != nil {
				t.Fatalf("Publish() failed: %v", err)
			}

			resources := p.driverResources()

			if len(resources.Pools) != 1 {
				t.Fatalf("got %d pools, expected one", len(resources.Pools))
			}
			pool, ok := resources.Pools[testNodeName]
			if !ok {
				t.Fatalf("no pool for node %q", testNodeName)
			}
			if len(pool.Slices) != tc.wantSlices {
				t.Fatalf("got %d slices, expected %d", len(pool.Slices), tc.wantSlices)
			}

			// Every slice has to be publishable on its own, and the slices
			// together have to be the devices the policy handed us, in order.
			var devices []resourceapi.Device
			for i, slice := range pool.Slices {
				if n := len(slice.Devices); n > resourceapi.ResourceSliceMaxDevices {
					t.Errorf("slice %d holds %d devices, over the %d allowed",
						i, n, resourceapi.ResourceSliceMaxDevices)
				}
				devices = append(devices, slice.Devices...)
			}
			if !reflect.DeepEqual(devices, tc.devices) {
				t.Errorf("got devices %v, expected %v", devices, tc.devices)
			}
		})
	}
}

// TestPublishCopiesDevices verifies the devices are copied, so that a policy
// changing its own devices afterwards does not change what we publish.
func TestPublishCopiesDevices(t *testing.T) {
	p, _ := newTestPlugin(t, fake.NewClientset())

	devices := []resourceapi.Device{{Name: "cpu-0"}}
	if err := p.Publish(devices); err != nil {
		t.Fatalf("Publish() failed: %v", err)
	}
	devices[0].Name = "cpu-1"

	if name := p.driverResources().Pools[testNodeName].Slices[0].Devices[0].Name; name != "cpu-0" {
		t.Errorf("published device %q, expected %q", name, "cpu-0")
	}
}
