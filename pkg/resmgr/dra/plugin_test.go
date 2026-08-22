/*
Copyright The NRI Plugins Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// TestNewLogr verifies that newLogr returns a usable logr.Logger backed by the
// default pkg/log Logger and that calling Info on it does not panic.
func TestNewLogr(t *testing.T) {
	l := newLogr(log.Default())
	if l.IsZero() {
		t.Error("newLogr returned a zero logr.Logger")
	}
	// Calling Info must not panic.
	l.Info("test message from TestNewLogr")
}

// validDeps returns a Deps with all required fields populated.
func validDeps() Deps {
	return Deps{
		KubeClient:      fake.NewClientset(),
		NodeName:        "test-node",
		ValidateClasses: func() error { return nil },
		DeviceLister:    &fixedDeviceLister{},
		ClaimAllocator:  &noopClaimAllocator{},
		CDIWriter:       &noopCDIWriter{},
		ClaimStore:      &noopClaimStore{},
		WithLock:        func(f func()) { f() },
		Logger:          log.Default(),
	}
}

// TestNew_Succeeds verifies that New returns a non-nil Plugin when all
// required fields are provided.
func TestNew_Succeeds(t *testing.T) {
	p, err := New("test-driver", validDeps())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("New() returned nil Plugin, want non-nil")
	}
}

// TestNew_Validation verifies that New returns an error when any required
// dependency is absent.
func TestNew_Validation(t *testing.T) {
	tests := []struct {
		name       string
		driverName string
		mutate     func(*Deps)
	}{
		{
			name:       "empty driverName",
			driverName: "",
			mutate:     nil,
		},
		{
			name:       "nil KubeClient",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.KubeClient = nil },
		},
		{
			name:       "empty NodeName",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.NodeName = "" },
		},
		{
			name:       "nil ValidateClasses",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.ValidateClasses = nil },
		},
		{
			name:       "nil DeviceLister",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.DeviceLister = nil },
		},
		{
			name:       "nil Logger",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.Logger = nil },
		},
		{
			name:       "nil ClaimAllocator",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.ClaimAllocator = nil },
		},
		{
			name:       "nil CDIWriter",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.CDIWriter = nil },
		},
		{
			name:       "nil ClaimStore",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.ClaimStore = nil },
		},
		{
			name:       "nil WithLock",
			driverName: "test-driver",
			mutate:     func(d *Deps) { d.WithLock = nil },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := validDeps()
			if tc.mutate != nil {
				tc.mutate(&deps)
			}
			p, err := New(tc.driverName, deps)
			if err == nil {
				t.Errorf("New() expected error, got nil")
			}
			if p != nil {
				t.Errorf("New() expected nil Plugin on error, got %v", p)
			}
		})
	}
}

// TestPrepareResourceClaims_Stub verifies that PrepareResourceClaims returns
// errNotImplemented before real allocation logic is wired in Step 7.
func TestPrepareResourceClaims_Stub(t *testing.T) {
	p := &Plugin{}
	result, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{})
	if result != nil {
		t.Errorf("PrepareResourceClaims() result = %v, want nil", result)
	}
	if !errors.Is(err, errNotImplemented) {
		t.Errorf("PrepareResourceClaims() err = %v, want errNotImplemented", err)
	}
}

// TestUnprepareResourceClaims_Stub verifies that UnprepareResourceClaims returns
// errNotImplemented before real deallocation logic is wired in Step 7.
func TestUnprepareResourceClaims_Stub(t *testing.T) {
	p := &Plugin{}
	result, err := p.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{})
	if result != nil {
		t.Errorf("UnprepareResourceClaims() result = %v, want nil", result)
	}
	if !errors.Is(err, errNotImplemented) {
		t.Errorf("UnprepareResourceClaims() err = %v, want errNotImplemented", err)
	}
}

// TestHandleError_RecoverableLogsWarn verifies that a recoverable error is
// handled without panicking. The method logs at Warn level.
func TestHandleError_RecoverableLogsWarn(t *testing.T) {
	p := &Plugin{deps: Deps{Logger: log.Default()}}
	recoverableErr := fmt.Errorf("transient failure: %w", kubeletplugin.ErrRecoverable)
	// Must not panic.
	p.HandleError(context.Background(), recoverableErr, "publish failed")
}

// TestHandleError_FatalLogsError verifies that a non-recoverable (fatal) error
// is handled without panicking. The method logs at Error level.
func TestHandleError_FatalLogsError(t *testing.T) {
	p := &Plugin{deps: Deps{Logger: log.Default()}}
	fatalErr := errors.New("fatal background error")
	// Must not panic.
	p.HandleError(context.Background(), fatalErr, "fatal error encountered")
}

// TestPublishResources_NilHelper verifies that PublishResources returns an
// error (not a panic) when called before Start, and that the error message
// references "Start" so callers can diagnose the ordering mistake.
func TestPublishResources_NilHelper(t *testing.T) {
	p, err := New("test-driver", validDeps())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	// helper is nil at this point (Start has not been called)
	err = p.PublishResources(context.Background())
	if err == nil {
		t.Fatal("PublishResources() expected error when helper is nil, got nil")
	}
	if !strings.Contains(err.Error(), "Start") {
		t.Errorf("PublishResources() err = %q, want message containing \"Start\"", err.Error())
	}
}

// TestPublishResources_ValidationError verifies that a ValidateClasses failure
// is propagated by PublishResources.
func TestPublishResources_ValidationError(t *testing.T) {
	validateErr := errors.New("invalid class config")
	deps := validDeps()
	deps.ValidateClasses = func() error { return validateErr }
	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	// helper is nil — but ValidateClasses is checked first, so that error
	// is returned before the nil-helper guard.
	err = p.PublishResources(context.Background())
	if err == nil {
		t.Fatal("PublishResources() expected error from ValidateClasses, got nil")
	}
	if !errors.Is(err, validateErr) {
		t.Errorf("PublishResources() err = %v, want to wrap %v", err, validateErr)
	}
}

// TestPublishResources_Pagination_Zero verifies that zero devices produce
// exactly one empty slice.
func TestPublishResources_Pagination_Zero(t *testing.T) {
	res := buildDriverResources("node1", nil)
	pool, ok := res.Pools["node1"]
	if !ok {
		t.Fatal("expected pool named 'node1'")
	}
	if len(pool.Slices) != 1 {
		t.Errorf("zero devices: got %d slice(s), want 1", len(pool.Slices))
	}
	if len(pool.Slices[0].Devices) != 0 {
		t.Errorf("zero devices: slice[0] has %d device(s), want 0", len(pool.Slices[0].Devices))
	}
}

// TestPublishResources_Pagination_ExactMax verifies that exactly
// ResourceSliceMaxDevices devices fit into one slice.
func TestPublishResources_Pagination_ExactMax(t *testing.T) {
	max := resourceapi.ResourceSliceMaxDevices
	devices := makeTestDevices(max)
	res := buildDriverResources("node1", devices)
	pool := res.Pools["node1"]
	if len(pool.Slices) != 1 {
		t.Errorf("exact max devices: got %d slice(s), want 1", len(pool.Slices))
	}
	if len(pool.Slices[0].Devices) != max {
		t.Errorf("exact max devices: slice[0] has %d device(s), want %d", len(pool.Slices[0].Devices), max)
	}
}

// TestPublishResources_Pagination_OverMax verifies that
// ResourceSliceMaxDevices+1 devices are split into exactly two slices
// belonging to the same pool.
func TestPublishResources_Pagination_OverMax(t *testing.T) {
	max := resourceapi.ResourceSliceMaxDevices
	devices := makeTestDevices(max + 1)
	res := buildDriverResources("node1", devices)
	pool := res.Pools["node1"]
	if len(pool.Slices) != 2 {
		t.Errorf("max+1 devices: got %d slice(s), want 2", len(pool.Slices))
	}
	if len(pool.Slices[0].Devices) != max {
		t.Errorf("max+1 devices: slice[0] has %d device(s), want %d", len(pool.Slices[0].Devices), max)
	}
	if len(pool.Slices[1].Devices) != 1 {
		t.Errorf("max+1 devices: slice[1] has %d device(s), want 1", len(pool.Slices[1].Devices))
	}
}

// TestStop_Idempotent verifies that calling Stop twice on a Plugin does not
// panic. Both calls are made on a plugin that was never started (helper == nil).
func TestStop_Idempotent(t *testing.T) {
	p, err := New("test-driver", validDeps())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	// Must not panic.
	p.Stop()
	p.Stop()
}

// TestStart_ValidateClassesError verifies that Start returns the ValidateClasses
// error before attempting to call kubeletplugin.Start.
func TestStart_ValidateClassesError(t *testing.T) {
	validateErr := errors.New("cpu class config invalid")
	deps := validDeps()
	deps.ValidateClasses = func() error { return validateErr }
	deps.PluginDataDir = t.TempDir()
	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	err = p.Start(context.Background())
	if err == nil {
		t.Fatal("Start() expected error from ValidateClasses, got nil")
	}
	if !errors.Is(err, validateErr) {
		t.Errorf("Start() err = %v, want to wrap %v", err, validateErr)
	}
	if p.helper != nil {
		t.Error("Start() set p.helper on ValidateClasses failure, want nil")
	}
}

// makeTestDevices returns a slice of n named resourceapi.Device objects for
// use in pagination tests.
func makeTestDevices(n int) []resourceapi.Device {
	devices := make([]resourceapi.Device, n)
	for i := range devices {
		devices[i] = resourceapi.Device{Name: fmt.Sprintf("dev-%d", i)}
	}
	return devices
}

// fixedDeviceLister is a DeviceLister that always returns a preset list of
// devices, used in integration tests.
type fixedDeviceLister struct {
	devices []resourceapi.Device
}

func (f *fixedDeviceLister) DRADevices(_ string) ([]resourceapi.Device, error) {
	return f.devices, nil
}

// errorDeviceLister is a DeviceLister that always returns the configured error.
type errorDeviceLister struct {
	err error
}

func (e *errorDeviceLister) DRADevices(_ string) ([]resourceapi.Device, error) {
	return nil, e.err
}

// noopClaimAllocator is a ClaimAllocator that succeeds without doing anything.
type noopClaimAllocator struct{}

func (*noopClaimAllocator) PickHpCpus(_, _, _ int, _ cpuset.CPUSet) (cpuset.CPUSet, error) {
	return cpuset.New(), nil
}

func (*noopClaimAllocator) ReleaseHpCpus(_, _ int, _ cpuset.CPUSet) {}

func (*noopClaimAllocator) AccountHpCpus(_, _ int, _ cpuset.CPUSet) error { return nil }

func (*noopClaimAllocator) IsHPClass(_ string) bool { return false }

// noopCDIWriter is a CDIWriter that succeeds without doing anything.
type noopCDIWriter struct{}

func (*noopCDIWriter) WriteClaim(_ types.UID, _ []CDIDevice) error   { return nil }
func (*noopCDIWriter) RemoveClaim(_ types.UID) error                  { return nil }
func (*noopCDIWriter) ClaimSpecExists(_ types.UID) bool               { return false }
func (*noopCDIWriter) ListClaims() ([]types.UID, error)               { return nil, nil }

// noopClaimStore is a ClaimStore that succeeds without persisting anything.
type noopClaimStore struct{}

func (*noopClaimStore) Save(_ map[types.UID]*ClaimState) error             { return nil }
func (*noopClaimStore) Load() (map[types.UID]*ClaimState, error)           { return nil, nil }

// TestPublishResources_DRADevicesError verifies that an error from
// DeviceLister.DRADevices is propagated by PublishResources.
func TestPublishResources_DRADevicesError(t *testing.T) {
	sentinel := errors.New("DRADevices failed")
	deps := validDeps()
	deps.DeviceLister = &errorDeviceLister{err: sentinel}
	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	// Set helper to a non-nil stub so the nil-helper guard is bypassed,
	// allowing the test to reach the DRADevices call.
	p.helper = new(kubeletplugin.Helper)
	err = p.PublishResources(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("PublishResources() err = %v, want to wrap sentinel error", err)
	}
}

// TestPublishResources_ConcurrentNoRace verifies that calling PublishResources
// from multiple goroutines while a simulated Reconfigure goroutine acquires the
// same mutex does not produce a data race. When run with -race, the detector
// will flag any unsynchronized access to shared state.
func TestPublishResources_ConcurrentNoRace(t *testing.T) {
	var mu sync.Mutex

	deps := validDeps()
	// Replace the simple direct-call WithLock from validDeps with a real mutex
	// so the race detector can verify PublishResources holds the lock around
	// its ValidateClasses + DRADevices calls.
	deps.WithLock = func(f func()) {
		mu.Lock()
		defer mu.Unlock()
		f()
	}

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	// Set helper to non-nil so the nil-helper guard is bypassed.
	p.helper = new(kubeletplugin.Helper)

	ctx := context.Background()
	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			// Errors are expected (helper is a zero-value stub); what we are
			// testing is the absence of data races.
			_ = p.PublishResources(ctx)
		}()
	}
	// Simulate concurrent Reconfigure by acquiring the same mutex.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			mu.Lock()
			mu.Unlock()
		}
	}()
	wg.Wait()
}

// TestStart_AlreadyStarted verifies that a second call to Start returns an
// error without spawning a second helper. The guard is tested by setting
// p.helper to a non-nil stub before calling Start.
func TestStart_AlreadyStarted(t *testing.T) {
	deps := validDeps()
	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	// Simulate an already-started plugin.
	p.helper = new(kubeletplugin.Helper)
	err = p.Start(context.Background())
	if err == nil {
		t.Fatal("Start() expected error on double-call, got nil")
	}
	if !strings.Contains(err.Error(), "already started") {
		t.Errorf("Start() err = %q, want message containing \"already started\"", err.Error())
	}
}

// TestPublishResources_Integration starts a Plugin against a fake Kubernetes
// clientset, calls PublishResources, and polls until the fake cluster receives
// at least one ResourceSlice create, then validates the driver name field.
//
// Both the kubelet-plugin registration socket and plugin data socket are
// created under t.TempDir() — no real kubelet is required.
func TestPublishResources_Integration(t *testing.T) {
	registrarDir := t.TempDir()
	pluginDataDir := t.TempDir()

	// Fake clientset pre-loaded with the node object so the
	// resourceslice.Controller can look up the node UID.
	fakeClient := fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	})

	// Reactor captures every ResourceSlice create call and passes it through
	// to the default object tracker so the controller behaves normally.
	var (
		mu             sync.Mutex
		capturedSlices []*resourceapi.ResourceSlice
	)
	fakeClient.PrependReactor("create", "resourceslices",
		func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			createAction := action.(k8stesting.CreateAction)
			if slice, ok := createAction.GetObject().(*resourceapi.ResourceSlice); ok {
				mu.Lock()
				capturedSlices = append(capturedSlices, slice.DeepCopy())
				mu.Unlock()
			}
			return false, nil, nil // pass through to default tracker
		},
	)

	deps := Deps{
		KubeClient:      fakeClient,
		NodeName:        "test-node",
		RegistrarDir:    registrarDir,
		PluginDataDir:   pluginDataDir,
		ValidateClasses: func() error { return nil },
		DeviceLister:    &fixedDeviceLister{devices: makeTestDevices(5)},
		ClaimAllocator:  &noopClaimAllocator{},
		CDIWriter:       &noopCDIWriter{},
		ClaimStore:      &noopClaimStore{},
		WithLock:        func(f func()) { f() },
		Logger:          log.Default(),
	}

	const driverName = "test.driver.io"
	p, err := New(driverName, deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	defer p.Stop()

	if err := p.PublishResources(ctx); err != nil {
		t.Fatalf("PublishResources() unexpected error: %v", err)
	}

	// The resourceslice.Controller drives ResourceSlice creation
	// asynchronously; poll with a 5-second deadline.
	const pollDeadline = 5 * time.Second
	deadline := time.Now().Add(pollDeadline)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(capturedSlices)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(capturedSlices) == 0 {
		t.Fatalf("no ResourceSlice was created within the %s deadline", pollDeadline)
	}
	if got := capturedSlices[0].Spec.Driver; got != driverName {
		t.Errorf("ResourceSlice[0].Spec.Driver = %q, want %q", got, driverName)
	}
}

// TestNoCmdPluginsImport verifies that pkg/resmgr/dra has no transitive
// dependency on any nri-plugins cmd binary package. Such an import would
// violate design.md resolved decision 6 (code-sharing verification
// checklist). The test is most useful guarding the pre-Step-8 window;
// after Step 8 adds policy wiring, a reverse import would also be caught
// at compile time as a cycle.
func TestNoCmdPluginsImport(t *testing.T) {
	goExe, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found; skipping import-boundary check")
	}

	// Run from the package directory so ./... covers future subpackages.
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; this indicates a corrupted runtime")
	}
	pkgDir := filepath.Dir(filename)

	cmd := exec.Command(goExe, "list", "-deps", "./...")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list failed (%v); import-boundary check cannot proceed", err)
	}

	const forbidden = "github.com/containers/nri-plugins/cmd/"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, forbidden) {
			t.Errorf("import-boundary violation: pkg/resmgr/dra depends on %q", line)
		}
	}
}
