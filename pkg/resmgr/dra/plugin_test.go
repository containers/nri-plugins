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
	"k8s.io/apimachinery/pkg/api/resource"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
	"tags.cncf.io/container-device-interface/pkg/parser"
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
	// Simulate concurrent Reconfigure by acquiring the same mutex for a brief
	// hold, which is what resmgr.apply() does when it calls policy.Reconfigure()
	// under the write lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			mu.Lock()
			runtime.Gosched() // hold briefly to interleave with PublishResources
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

// ---- Task 7: PrepareResourceClaims test helpers ----

// trackingClaimAllocator tracks PickHpCpus and ReleaseHpCpus calls.
type trackingClaimAllocator struct {
	pickResult cpuset.CPUSet
	pickErr    error
	isHP       bool
	picks      []cpuset.CPUSet  // CPUSets returned per PickHpCpus call
	releases   []cpuset.CPUSet  // CPUSets released per ReleaseHpCpus call
	accounts   []cpuset.CPUSet  // CPUSets accounted per AccountHpCpus call
	accountErr error
}

func (a *trackingClaimAllocator) PickHpCpus(_, _, _ int, _ cpuset.CPUSet) (cpuset.CPUSet, error) {
	if a.pickErr != nil {
		return cpuset.New(), a.pickErr
	}
	a.picks = append(a.picks, a.pickResult)
	return a.pickResult, nil
}

func (a *trackingClaimAllocator) ReleaseHpCpus(_, _ int, cpus cpuset.CPUSet) {
	a.releases = append(a.releases, cpus)
}

func (a *trackingClaimAllocator) AccountHpCpus(_, _ int, cpus cpuset.CPUSet) error {
	if a.accountErr != nil {
		return a.accountErr
	}
	a.accounts = append(a.accounts, cpus)
	return nil
}

func (a *trackingClaimAllocator) IsHPClass(_ string) bool { return a.isHP }

// trackingCDIWriter tracks WriteClaim and RemoveClaim calls.
type trackingCDIWriter struct {
	writeErr    error
	removeErr   error
	existsValue bool
	written     []types.UID
	removed     []types.UID
}

func (w *trackingCDIWriter) WriteClaim(uid types.UID, _ []CDIDevice) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	w.written = append(w.written, uid)
	return nil
}

func (w *trackingCDIWriter) RemoveClaim(uid types.UID) error {
	w.removed = append(w.removed, uid)
	return w.removeErr
}

func (w *trackingCDIWriter) ClaimSpecExists(_ types.UID) bool   { return w.existsValue }
func (w *trackingCDIWriter) ListClaims() ([]types.UID, error)    { return nil, nil }

// trackingClaimStore records Save and Load calls.
type trackingClaimStore struct {
	saveErr error
	saved   int
}

func (s *trackingClaimStore) Save(_ map[types.UID]*ClaimState) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved++
	return nil
}

func (s *trackingClaimStore) Load() (map[types.UID]*ClaimState, error) { return nil, nil }

// strPtr returns a pointer to s, used to build DeviceAttribute.StringValue.
func strPtr(s string) *string { return &s }

// int64Ptr returns a pointer to v, used to build DeviceAttribute.IntValue.
func int64Ptr(v int64) *int64 { return &v }

// hpDevice builds a resourceapi.Device with nri/cpuClass, nri/packageID, and
// nri/punitID attributes, plus an nri/cpus capacity.
func hpDevice(name, className string, pkgID, punitID int) resourceapi.Device {
	return resourceapi.Device{
		Name: name,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"nri/cpuClass":  {StringValue: strPtr(className)},
			"nri/packageID": {IntValue: int64Ptr(int64(pkgID))},
			"nri/punitID":   {IntValue: int64Ptr(int64(punitID))},
		},
	}
}

// hpDeviceLister returns a DeviceLister that always returns the given devices.
func hpDeviceLister(devs ...resourceapi.Device) *fixedDeviceLister {
	return &fixedDeviceLister{devices: devs}
}

// makeClaim builds a ResourceClaim with a single allocation result for
// driverName/poolName/deviceName, with the given ConsumedCapacity cpu count.
func makeClaim(uid types.UID, driverName, poolName, deviceName, request string, cpus int) *resourceapi.ResourceClaim {
	qty := resource.MustParse(fmt.Sprintf("%d", cpus))
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Driver:  driverName,
							Pool:    poolName,
							Device:  deviceName,
							Request: request,
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
								"nri/cpus": qty,
							},
						},
					},
				},
			},
		},
	}
}

// ---- Task 7: PrepareResourceClaims tests ----

// TestPrepare_SingleHPSuccess verifies that a single HP claim results in a
// PrepareResult with one Device and that CPUs are picked and CDI is written.
func TestPrepare_SingleHPSuccess(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	cdiW := &trackingCDIWriter{}
	store := &trackingClaimStore{}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.ClaimStore = store
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-1")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4)
	result, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("PrepareResourceClaims() unexpected error: %v", err)
	}
	r, ok := result[uid]
	if !ok {
		t.Fatal("result map missing uid-1")
	}
	if r.Err != nil {
		t.Fatalf("PrepareResult.Err = %v, want nil", r.Err)
	}
	if len(r.Devices) != 1 {
		t.Fatalf("PrepareResult.Devices len = %d, want 1", len(r.Devices))
	}
	if len(alloc.picks) != 1 {
		t.Errorf("PickHpCpus called %d times, want 1", len(alloc.picks))
	}
	if len(cdiW.written) != 1 || cdiW.written[0] != uid {
		t.Errorf("WriteClaim called for %v, want %v", cdiW.written, []types.UID{uid})
	}
	if store.saved != 1 {
		t.Errorf("ClaimStore.Save called %d times, want 1", store.saved)
	}
	// Verify the claim is stored.
	if _, ok := p.claims[uid]; !ok {
		t.Error("claim not stored in p.claims")
	}
}

// TestPrepare_Idempotent_SpecPresent verifies that a second Prepare call for the
// same claim with the CDI spec already present returns the same PrepareResult
// without re-picking CPUs or re-writing the spec.
func TestPrepare_Idempotent_SpecPresent(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	cdiW := &trackingCDIWriter{}
	store := &trackingClaimStore{}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.ClaimStore = store
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-idem")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4)

	// First Prepare — succeeds.
	result1, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil || result1[uid].Err != nil {
		t.Fatalf("first Prepare failed: %v / %v", err, result1[uid].Err)
	}
	firstPicks := len(alloc.picks)
	firstWrites := len(cdiW.written)

	// Simulate CDI spec existing.
	cdiW.existsValue = true

	// Second Prepare — should be idempotent.
	result2, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil || result2[uid].Err != nil {
		t.Fatalf("second Prepare failed: %v / %v", err, result2[uid].Err)
	}
	if len(alloc.picks) != firstPicks {
		t.Errorf("second Prepare picked CPUs again (picks: %d → %d)", firstPicks, len(alloc.picks))
	}
	if len(cdiW.written) != firstWrites {
		t.Errorf("second Prepare re-wrote CDI spec (writes: %d → %d)", firstWrites, len(cdiW.written))
	}
	// Devices content must be identical between calls.
	devs1 := result1[uid].Devices
	devs2 := result2[uid].Devices
	if len(devs1) != len(devs2) {
		t.Errorf("Devices len differs between calls: first=%d second=%d", len(devs1), len(devs2))
	} else {
		for i := range devs1 {
			if len(devs1[i].CDIDeviceIDs) != len(devs2[i].CDIDeviceIDs) {
				t.Errorf("Device[%d] CDIDeviceIDs len differs: first=%d second=%d", i, len(devs1[i].CDIDeviceIDs), len(devs2[i].CDIDeviceIDs))
				continue
			}
			for j := range devs1[i].CDIDeviceIDs {
				if devs1[i].CDIDeviceIDs[j] != devs2[i].CDIDeviceIDs[j] {
					t.Errorf("Device[%d].CDIDeviceIDs[%d]: first=%q second=%q", i, j, devs1[i].CDIDeviceIDs[j], devs2[i].CDIDeviceIDs[j])
				}
			}
		}
	}
}

// TestPrepare_Idempotent_SpecMissing verifies that a second Prepare call for the
// same claim where the CDI spec is missing re-writes the spec without re-picking CPUs.
func TestPrepare_Idempotent_SpecMissing(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	cdiW := &trackingCDIWriter{}
	store := &trackingClaimStore{}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.ClaimStore = store
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-rewrite")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4)

	// First Prepare.
	result1, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil || result1[uid].Err != nil {
		t.Fatalf("first Prepare failed: %v / %v", err, result1[uid].Err)
	}
	firstPicks := len(alloc.picks)
	firstWrites := len(cdiW.written)

	// CDI spec remains missing (existsValue == false by default).

	// Second Prepare — should re-write but not re-pick.
	result2, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil || result2[uid].Err != nil {
		t.Fatalf("second Prepare failed: %v / %v", err, result2[uid].Err)
	}
	if len(alloc.picks) != firstPicks {
		t.Errorf("second Prepare re-picked CPUs (picks: %d → %d)", firstPicks, len(alloc.picks))
	}
	if len(cdiW.written) != firstWrites+1 {
		t.Errorf("second Prepare should have re-written CDI spec (writes: %d → %d)", firstWrites, len(cdiW.written))
	}
	// Devices content must be identical between calls.
	devs1 := result1[uid].Devices
	devs2 := result2[uid].Devices
	if len(devs1) != len(devs2) {
		t.Errorf("Devices len differs between calls: first=%d second=%d", len(devs1), len(devs2))
	} else {
		for i := range devs1 {
			if len(devs1[i].CDIDeviceIDs) != len(devs2[i].CDIDeviceIDs) {
				t.Errorf("Device[%d] CDIDeviceIDs len differs: first=%d second=%d", i, len(devs1[i].CDIDeviceIDs), len(devs2[i].CDIDeviceIDs))
				continue
			}
			for j := range devs1[i].CDIDeviceIDs {
				if devs1[i].CDIDeviceIDs[j] != devs2[i].CDIDeviceIDs[j] {
					t.Errorf("Device[%d].CDIDeviceIDs[%d]: first=%q second=%q", i, j, devs1[i].CDIDeviceIDs[j], devs2[i].CDIDeviceIDs[j])
				}
			}
		}
	}
}

// TestPrepare_Idempotent_SpecMissing_PartialCorruptCPU verifies that the
// spec-missing idempotency path returns an error when only some stored alloc
// CPUs are parseable (partial corruption), preventing a mismatched CDI spec
// from being written.
func TestPrepare_Idempotent_SpecMissing_PartialCorruptCPU(t *testing.T) {
	alloc := &trackingClaimAllocator{}
	cdiW := &trackingCDIWriter{}
	store := &trackingClaimStore{}

	uid := types.UID("uid-partial-corrupt")
	// Two stored allocs: first is valid, second has an unparseable CPU string.
	claimState := &ClaimState{
		UID: string(uid),
		Allocs: []ResultAlloc{
			{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "0-1", ClassName: "gold"},
			{Device: "dev1", PkgID: 0, PunitID: 1, CPUs: "NOT-A-CPUSET", ClassName: "gold"},
		},
	}

	qty := resource.MustParse("2")
	// Claim carries two allocation results for our driver — one per device.
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: "test-driver", Pool: "p", Device: "dev0", Request: "req0",
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{"nri/cpus": qty}},
						{Driver: "test-driver", Pool: "p", Device: "dev1", Request: "req1",
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{"nri/cpus": qty}},
					},
				},
			},
		},
	}

	// Wire in the pre-loaded claim state so the idempotency path fires.
	p := preparePlugin(t, alloc, cdiW, store, map[types.UID]*ClaimState{uid: claimState})
	// CDI spec is missing (cdiW.existsValue == false by default).

	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r, ok := result[uid]
	if !ok {
		t.Fatal("result map missing UID")
	}
	if r.Err == nil {
		t.Fatal("PrepareResult.Err = nil; want error for partial corrupt stored CPUs")
	}
	// No CDI spec should have been written.
	if len(cdiW.written) != 0 {
		t.Errorf("CDIWriter.WriteClaim called %d time(s), want 0", len(cdiW.written))
	}
	// No CPU picks should have been made.
	if len(alloc.picks) != 0 {
		t.Errorf("PickHpCpus called %d time(s), want 0", len(alloc.picks))
	}
}

// TestPrepare_NilAllocation verifies that a nil claim.Status.Allocation produces
// a per-claim errNilAllocation in the result map, not a global error.
func TestPrepare_NilAllocation(t *testing.T) {
	deps := validDeps()
	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-nil-alloc")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
	}
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r, ok := result[uid]
	if !ok {
		t.Fatal("result map missing UID")
	}
	if !errors.Is(r.Err, errNilAllocation) {
		t.Errorf("PrepareResult.Err = %v, want errNilAllocation", r.Err)
	}
}

// TestPrepare_ForeignDriverOnly verifies that a claim with results only for a
// different driver produces an empty PrepareResult with no error.
func TestPrepare_ForeignDriverOnly(t *testing.T) {
	deps := validDeps()
	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-foreign")
	qty := resource.MustParse("4")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: "other-driver", Pool: "p", Device: "d", Request: "r",
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{"nri/cpus": qty}},
					},
				},
			},
		},
	}
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r, ok := result[uid]
	if !ok {
		t.Fatal("result map missing UID")
	}
	if r.Err != nil {
		t.Errorf("PrepareResult.Err = %v, want nil for foreign-driver-only claim", r.Err)
	}
	if len(r.Devices) != 0 {
		t.Errorf("PrepareResult.Devices = %v, want empty for foreign-driver-only claim", r.Devices)
	}
}

// TestPrepare_UnknownDevice verifies that a result referencing a device not in
// deviceIndex produces a per-claim error.
func TestPrepare_UnknownDevice(t *testing.T) {
	alloc := &trackingClaimAllocator{isHP: true}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	// DeviceLister returns no devices.
	deps.DeviceLister = hpDeviceLister()

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-unknown")
	claim := makeClaim(uid, "test-driver", "pool0", "unknown-dev", "req0", 4)
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	if r.Err == nil {
		t.Error("expected per-claim error for unknown device, got nil")
	}
}

// TestPrepare_NilAttr verifies that a device with a missing (empty) nri/cpuClass
// attribute produces a per-claim error.
func TestPrepare_NilAttr(t *testing.T) {
	alloc := &trackingClaimAllocator{isHP: true}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	// Device has no nri/cpuClass attribute.
	deps.DeviceLister = hpDeviceLister(resourceapi.Device{
		Name: "dev0",
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"nri/packageID": {IntValue: int64Ptr(0)},
			"nri/punitID":   {IntValue: int64Ptr(0)},
			// nri/cpuClass intentionally absent.
		},
	})

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-nil-attr")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4)
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	if r.Err == nil {
		t.Error("expected per-claim error for nil nri/cpuClass attr, got nil")
	}
}

// TestPrepare_NilIntAttr verifies that a device attribute with a nil IntValue
// (for packageID or punitID) is handled gracefully: the field defaults to 0
// and Prepare succeeds when all other required attrs are present.
func TestPrepare_NilIntAttr(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = &trackingCDIWriter{}
	deps.ClaimStore = &trackingClaimStore{}
	// Device has cpuClass set but packageID and punitID with nil IntValue.
	deps.DeviceLister = hpDeviceLister(resourceapi.Device{
		Name: "dev0",
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"nri/cpuClass":  {StringValue: strPtr("gold")},
			"nri/packageID": {IntValue: nil}, // nil IntValue → PkgID defaults to 0
			"nri/punitID":   {IntValue: nil}, // nil IntValue → PunitID defaults to 0
		},
	})

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-nil-int-attr")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4)
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	// Nil IntValue falls back to zero — Prepare should succeed (pick from pkg 0, punit 0).
	if r.Err != nil {
		t.Errorf("PrepareResult.Err = %v, want nil for nil IntValue attrs (defaults to 0)", r.Err)
	}
}

// TestPrepare_AbsentConsumedCapacity verifies that a result with no nri/cpus
// entry in ConsumedCapacity produces errMissingConsumedCapacity.
func TestPrepare_AbsentConsumedCapacity(t *testing.T) {
	alloc := &trackingClaimAllocator{isHP: true}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-no-cap")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: "test-driver", Pool: "p", Device: "dev0", Request: "r"},
						// No ConsumedCapacity.
					},
				},
			},
		},
	}
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	if !errors.Is(r.Err, errMissingConsumedCapacity) {
		t.Errorf("PrepareResult.Err = %v, want errMissingConsumedCapacity", r.Err)
	}
}

// TestPrepare_NonHP verifies that a non-HP class produces errNonHPNotSupported.
func TestPrepare_NonHP(t *testing.T) {
	alloc := &trackingClaimAllocator{isHP: false} // isHP == false → not HP
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "silver", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-non-hp")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4)
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	if !errors.Is(r.Err, errNonHPNotSupported) {
		t.Errorf("PrepareResult.Err = %v, want errNonHPNotSupported", r.Err)
	}
}

// TestPrepare_PickFailure verifies that a PickHpCpus failure rolls back
// any previously picked CPUs and returns a per-claim error.
func TestPrepare_PickFailure(t *testing.T) {
	pickErr := errors.New("pick failed")
	alloc := &trackingClaimAllocator{pickErr: pickErr, isHP: true}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-pick-fail")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4)
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	if r.Err == nil {
		t.Error("expected per-claim error from PickHpCpus failure, got nil")
	}
	if !errors.Is(r.Err, pickErr) {
		t.Errorf("PrepareResult.Err = %v, want to wrap pickErr", r.Err)
	}
	// Claim must not be stored after a PickHpCpus failure.
	if _, ok := p.claims[uid]; ok {
		t.Error("claim stored in p.claims after PickHpCpus failure (should not be present)")
	}
}

// TestPrepare_CDIWriteFailure verifies that a CDI WriteClaim failure rolls back
// the picked CPUs and returns a per-claim error.
func TestPrepare_CDIWriteFailure(t *testing.T) {
	writeErr := errors.New("CDI write failed")
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	cdiW := &trackingCDIWriter{writeErr: writeErr}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-cdi-fail")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4)
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	if r.Err == nil {
		t.Error("expected per-claim error from CDI write failure, got nil")
	}
	if !errors.Is(r.Err, writeErr) {
		t.Errorf("PrepareResult.Err = %v, want to wrap writeErr", r.Err)
	}
	// Rollback: CPUs must be released.
	if len(alloc.releases) == 0 {
		t.Error("expected ReleaseHpCpus to be called on CDI write failure")
	}
	// Claim must not be stored after a CDI write failure.
	if _, ok := p.claims[uid]; ok {
		t.Error("claim stored in p.claims after CDI write failure (should have been rolled back)")
	}
}

// TestPrepare_MultiResultTwoPunits verifies that a claim with two results
// (different punits) picks CPUs for each, builds two CDI devices, and returns
// two Device entries in the PrepareResult.
func TestPrepare_MultiResultTwoPunits(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-1"), isHP: true}
	cdiW := &trackingCDIWriter{}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.DeviceLister = hpDeviceLister(
		hpDevice("dev0", "gold", 0, 0),
		hpDevice("dev1", "gold", 0, 1),
	)

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	qty := resource.MustParse("2")
	uid := types.UID("uid-multi")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: "test-driver", Pool: "p", Device: "dev0", Request: "req0",
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{"nri/cpus": qty}},
						{Driver: "test-driver", Pool: "p", Device: "dev1", Request: "req1",
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{"nri/cpus": qty}},
					},
				},
			},
		},
	}
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	if r.Err != nil {
		t.Fatalf("PrepareResult.Err = %v, want nil", r.Err)
	}
	if len(r.Devices) != 2 {
		t.Errorf("PrepareResult.Devices len = %d, want 2", len(r.Devices))
	}
	if len(alloc.picks) != 2 {
		t.Errorf("PickHpCpus called %d times, want 2", len(alloc.picks))
	}
}

// TestPrepare_ShareIDNil verifies that a result with a nil ShareID produces a
// Device with ShareID == nil.
func TestPrepare_ShareIDNil(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid := types.UID("uid-no-share")
	claim := makeClaim(uid, "test-driver", "pool0", "dev0", "req0", 4) // no ShareID
	result, _ := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	r := result[uid]
	if r.Err != nil {
		t.Fatalf("PrepareResult.Err = %v, want nil", r.Err)
	}
	if r.Devices[0].ShareID != nil {
		t.Errorf("Device.ShareID = %v, want nil", r.Devices[0].ShareID)
	}
}

// TestPrepare_ShareIDSet verifies that a result with a non-nil ShareID produces a
// Device with a matching non-nil ShareID.
func TestPrepare_ShareIDSet(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	shareUID := types.UID("share-abc")
	qty := resource.MustParse("4")
	uid := types.UID("uid-share")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: "test-driver", Pool: "p", Device: "dev0", Request: "req0",
							ShareID: &shareUID,
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{"nri/cpus": qty}},
					},
				},
			},
		},
	}
	result, _ := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	r := result[uid]
	if r.Err != nil {
		t.Fatalf("PrepareResult.Err = %v, want nil", r.Err)
	}
	if r.Devices[0].ShareID == nil {
		t.Error("Device.ShareID = nil, want non-nil")
	} else if *r.Devices[0].ShareID != shareUID {
		t.Errorf("Device.ShareID = %v, want %v", *r.Devices[0].ShareID, shareUID)
	}
}

// TestPrepare_AllUIDsInResultMap verifies that every claim UID appears in the
// result map even when some claims error.
func TestPrepare_AllUIDsInResultMap(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	uid1 := types.UID("uid-ok")
	uid2 := types.UID("uid-nil-alloc")

	good := makeClaim(uid1, "test-driver", "pool0", "dev0", "req0", 4)
	bad := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: uid2}}

	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{good, bad})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	if _, ok := result[uid1]; !ok {
		t.Errorf("result map missing %v", uid1)
	}
	if _, ok := result[uid2]; !ok {
		t.Errorf("result map missing %v", uid2)
	}
	if result[uid1].Err != nil {
		t.Errorf("good claim: PrepareResult.Err = %v, want nil", result[uid1].Err)
	}
	if !errors.Is(result[uid2].Err, errNilAllocation) {
		t.Errorf("bad claim: PrepareResult.Err = %v, want errNilAllocation", result[uid2].Err)
	}
}

// TestPrepare_SubrequestSlashInName verifies that a request name containing '/'
// (FirstAvailable subrequest format) produces a valid CDI device name and that
// the spec can be written successfully.
func TestPrepare_SubrequestSlashInName(t *testing.T) {
	alloc := &trackingClaimAllocator{pickResult: cpuset.MustParse("0-3"), isHP: true}
	cdiW := &trackingCDIWriter{}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.DeviceLister = hpDeviceLister(hpDevice("dev0", "gold", 0, 0))

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	qty := resource.MustParse("4")
	uid := types.UID("uid-slash")
	// Request name uses the FirstAvailable subrequest format: "main/sub".
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: "test-driver", Pool: "p", Device: "dev0",
							Request: "main-req/sub-req",
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{"nri/cpus": qty}},
					},
				},
			},
		},
	}
	result, globalErr := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if globalErr != nil {
		t.Fatalf("PrepareResourceClaims() unexpected global error: %v", globalErr)
	}
	r := result[uid]
	if r.Err != nil {
		t.Fatalf("PrepareResult.Err = %v, want nil", r.Err)
	}
	if len(r.Devices) != 1 {
		t.Fatalf("PrepareResult.Devices len = %d, want 1", len(r.Devices))
	}
	// Verify the device name part of each CDI qualified ID passes parser.ValidateDeviceName.
	// CDI qualified names have the format "vendor/class=name"; split on "=" to get the name.
	for _, cdiID := range r.Devices[0].CDIDeviceIDs {
		parts := strings.SplitN(cdiID, "=", 2)
		if len(parts) != 2 || parts[1] == "" {
			t.Errorf("CDI device ID %q has unexpected format (want vendor/class=name)", cdiID)
			continue
		}
		if err := parser.ValidateDeviceName(parts[1]); err != nil {
			t.Errorf("CDI device name %q from ID %q failed validation: %v", parts[1], cdiID, err)
		}
	}
	if len(cdiW.written) == 0 {
		t.Error("WriteClaim was not called for subrequest claim")
	}
}

// ---- Task 8: UnprepareResourceClaims tests ----

// unprepareObj builds a kubeletplugin.NamespacedObject for a given UID,
// used to drive UnprepareResourceClaims calls in tests.
func unprepareObj(uid types.UID) kubeletplugin.NamespacedObject {
	return kubeletplugin.NamespacedObject{UID: uid}
}

// preparePlugin is a helper that creates a Plugin, pre-populates p.claims with
// the given ClaimState entries, and wires the provided allocator, CDI writer,
// and store into Deps.
func preparePlugin(t *testing.T, alloc ClaimAllocator, cdiW CDIWriter, store ClaimStore,
	claimsIn map[types.UID]*ClaimState) *Plugin {
	t.Helper()
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.ClaimStore = store
	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	for uid, cs := range claimsIn {
		p.claims[uid] = cs
	}
	return p
}

// TestUnprepare_KnownClaim verifies that an existing claim is released,
// CDI is removed, the claim is deleted from p.claims, and the result map
// contains nil for the UID.
func TestUnprepare_KnownClaim(t *testing.T) {
	alloc := &trackingClaimAllocator{}
	cdiW := &trackingCDIWriter{}
	store := &trackingClaimStore{}

	uid := types.UID("uid-known")
	claimState := &ClaimState{
		UID: string(uid),
		Allocs: []ResultAlloc{
			{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "0-3", ClassName: "gold"},
		},
	}
	p := preparePlugin(t, alloc, cdiW, store, map[types.UID]*ClaimState{uid: claimState})

	result, globalErr := p.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{unprepareObj(uid)})
	if globalErr != nil {
		t.Fatalf("UnprepareResourceClaims() unexpected global error: %v", globalErr)
	}
	if perErr, ok := result[uid]; !ok {
		t.Error("result map missing uid")
	} else if perErr != nil {
		t.Errorf("result[uid] = %v, want nil", perErr)
	}
	if len(alloc.releases) != 1 {
		t.Errorf("ReleaseHpCpus called %d times, want 1", len(alloc.releases))
	}
	if len(cdiW.removed) != 1 || cdiW.removed[0] != uid {
		t.Errorf("RemoveClaim called for %v, want [%v]", cdiW.removed, uid)
	}
	if _, exists := p.claims[uid]; exists {
		t.Error("claim still in p.claims after Unprepare")
	}
	if store.saved != 1 {
		t.Errorf("ClaimStore.Save called %d times, want 1", store.saved)
	}
}

// TestUnprepare_UnknownUID verifies that an unknown UID produces a warning and
// a nil entry in the result map (no panic, no error).
func TestUnprepare_UnknownUID(t *testing.T) {
	alloc := &trackingClaimAllocator{}
	cdiW := &trackingCDIWriter{}
	store := &trackingClaimStore{}

	p := preparePlugin(t, alloc, cdiW, store, nil)

	uid := types.UID("uid-unknown")
	result, globalErr := p.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{unprepareObj(uid)})
	if globalErr != nil {
		t.Fatalf("UnprepareResourceClaims() unexpected global error: %v", globalErr)
	}
	if perErr, ok := result[uid]; !ok {
		t.Error("result map missing uid")
	} else if perErr != nil {
		t.Errorf("result[uid] = %v, want nil for unknown UID", perErr)
	}
	// CDI remove must NOT be called for unknown claims.
	if len(cdiW.removed) != 0 {
		t.Errorf("RemoveClaim called for unknown UID (removed = %v)", cdiW.removed)
	}
	// ReleaseHpCpus must NOT be called for unknown claims.
	if len(alloc.releases) != 0 {
		t.Errorf("ReleaseHpCpus called %d times, want 0 for unknown UID", len(alloc.releases))
	}
	// Save must still be called once (batch write even with no-ops).
	if store.saved != 1 {
		t.Errorf("ClaimStore.Save called %d times, want 1", store.saved)
	}
}

// TestUnprepare_CDIRemoveError verifies that a CDI RemoveClaim error produces
// a warning but does not surface as an error in the result map (nil for that UID).
func TestUnprepare_CDIRemoveError(t *testing.T) {
	removeErr := errors.New("CDI remove failed")
	alloc := &trackingClaimAllocator{}
	cdiW := &trackingCDIWriter{removeErr: removeErr}
	store := &trackingClaimStore{}

	uid := types.UID("uid-remove-err")
	claimState := &ClaimState{
		UID:    string(uid),
		Allocs: []ResultAlloc{{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "4-7", ClassName: "gold"}},
	}
	p := preparePlugin(t, alloc, cdiW, store, map[types.UID]*ClaimState{uid: claimState})

	result, globalErr := p.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{unprepareObj(uid)})
	if globalErr != nil {
		t.Fatalf("UnprepareResourceClaims() unexpected global error: %v", globalErr)
	}
	// CDI error is a warning only — result must be nil for the UID.
	if perErr := result[uid]; perErr != nil {
		t.Errorf("result[uid] = %v, want nil on CDI remove error", perErr)
	}
	// Claim must still be deleted from in-memory state.
	if _, exists := p.claims[uid]; exists {
		t.Error("claim still in p.claims after Unprepare despite CDI error")
	}
	// ReleaseHpCpus must have been called once — CPU leak on CDI error goes undetected otherwise.
	if len(alloc.releases) != 1 {
		t.Errorf("ReleaseHpCpus called %d times, want 1 (must release CPUs even on CDI error)", len(alloc.releases))
	}
}

// TestUnprepare_MixedBatch verifies that a batch with one known and one unknown
// UID both appear in the result map (nil for each), and only the known claim's
// CDI is removed.
func TestUnprepare_MixedBatch(t *testing.T) {
	alloc := &trackingClaimAllocator{}
	cdiW := &trackingCDIWriter{}
	store := &trackingClaimStore{}

	known := types.UID("uid-known-batch")
	unknown := types.UID("uid-unknown-batch")
	claimState := &ClaimState{
		UID:    string(known),
		Allocs: []ResultAlloc{{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "0-1", ClassName: "gold"}},
	}
	p := preparePlugin(t, alloc, cdiW, store, map[types.UID]*ClaimState{known: claimState})

	result, globalErr := p.UnprepareResourceClaims(context.Background(),
		[]kubeletplugin.NamespacedObject{unprepareObj(known), unprepareObj(unknown)})
	if globalErr != nil {
		t.Fatalf("UnprepareResourceClaims() unexpected global error: %v", globalErr)
	}
	if perErr, ok := result[known]; !ok {
		t.Error("result map missing known uid")
	} else if perErr != nil {
		t.Errorf("result[known] = %v, want nil", perErr)
	}
	if perErr, ok := result[unknown]; !ok {
		t.Error("result map missing unknown uid")
	} else if perErr != nil {
		t.Errorf("result[unknown] = %v, want nil", perErr)
	}
	// Only the known UID's CDI should be removed.
	if len(cdiW.removed) != 1 || cdiW.removed[0] != known {
		t.Errorf("RemoveClaim called for %v, want [%v]", cdiW.removed, known)
	}
	// One batch save.
	if store.saved != 1 {
		t.Errorf("ClaimStore.Save called %d times, want 1", store.saved)
	}
}

// TestShareIDPtr verifies that shareIDPtr returns nil for "" and a non-nil
// pointer for a non-empty string.
func TestShareIDPtr(t *testing.T) {
	if shareIDPtr("") != nil {
		t.Error("shareIDPtr(\"\") should return nil")
	}
	ptr := shareIDPtr("abc")
	if ptr == nil {
		t.Fatal("shareIDPtr(\"abc\") returned nil, want *types.UID")
	}
	if *ptr != types.UID("abc") {
		t.Errorf("*shareIDPtr(\"abc\") = %v, want %v", *ptr, types.UID("abc"))
	}
}

// ---- Task 9: LiveClaimClasses, RestoreClaimsLocked, Start reconciliation ----

// startTestCDIWriter supports per-UID ClaimSpecExists state and a fixed
// ListClaims result for Start reconciliation tests. WriteClaim is a no-op.
type startTestCDIWriter struct {
	existsByUID map[types.UID]bool
	listResult  []types.UID
	listErr     error
	removed     []types.UID
	removeErr   error
}

func (w *startTestCDIWriter) WriteClaim(_ types.UID, _ []CDIDevice) error { return nil }
func (w *startTestCDIWriter) RemoveClaim(uid types.UID) error {
	w.removed = append(w.removed, uid)
	return w.removeErr
}
func (w *startTestCDIWriter) ClaimSpecExists(uid types.UID) bool { return w.existsByUID[uid] }
func (w *startTestCDIWriter) ListClaims() ([]types.UID, error) {
	return w.listResult, w.listErr
}

// preloadedClaimStore loads a pre-configured map on Load and tracks saves.
type preloadedClaimStore struct {
	initial     map[types.UID]*ClaimState
	loadErr     error
	saved       int
	savedClaims map[types.UID]*ClaimState // last map passed to Save
}

func (s *preloadedClaimStore) Load() (map[types.UID]*ClaimState, error) {
	return s.initial, s.loadErr
}

func (s *preloadedClaimStore) Save(claims map[types.UID]*ClaimState) error {
	s.saved++
	cp := make(map[types.UID]*ClaimState, len(claims))
	for k, v := range claims {
		cp[k] = v
	}
	s.savedClaims = cp
	return nil
}

// TestLiveClaimClasses_Empty verifies that LiveClaimClasses returns an empty
// map when there are no claims.
func TestLiveClaimClasses_Empty(t *testing.T) {
	p, err := New("test-driver", validDeps())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	got := p.LiveClaimClasses()
	if len(got) != 0 {
		t.Errorf("LiveClaimClasses() = %v, want empty map", got)
	}
}

// TestLiveClaimClasses_SameClass verifies that two claims using the same class
// produce a count of 2.
func TestLiveClaimClasses_SameClass(t *testing.T) {
	p, err := New("test-driver", validDeps())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	p.claims[types.UID("a")] = &ClaimState{UID: "a", Allocs: []ResultAlloc{{ClassName: "gold"}}}
	p.claims[types.UID("b")] = &ClaimState{UID: "b", Allocs: []ResultAlloc{{ClassName: "gold"}}}

	got := p.LiveClaimClasses()
	if got["gold"] != 2 {
		t.Errorf("LiveClaimClasses()[gold] = %d, want 2", got["gold"])
	}
	if len(got) != 1 {
		t.Errorf("LiveClaimClasses() len = %d, want 1", len(got))
	}
}

// TestLiveClaimClasses_DifferentClasses verifies that two claims using
// different classes produce two entries in the result map.
func TestLiveClaimClasses_DifferentClasses(t *testing.T) {
	p, err := New("test-driver", validDeps())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	p.claims[types.UID("a")] = &ClaimState{UID: "a", Allocs: []ResultAlloc{{ClassName: "gold"}}}
	p.claims[types.UID("b")] = &ClaimState{UID: "b", Allocs: []ResultAlloc{{ClassName: "silver"}}}

	got := p.LiveClaimClasses()
	if len(got) != 2 {
		t.Errorf("LiveClaimClasses() len = %d, want 2", len(got))
	}
	if got["gold"] != 1 {
		t.Errorf("LiveClaimClasses()[gold] = %d, want 1", got["gold"])
	}
	if got["silver"] != 1 {
		t.Errorf("LiveClaimClasses()[silver] = %d, want 1", got["silver"])
	}
}

// TestLiveClaimsLocked_Empty verifies that LiveClaimsLocked returns an empty
// map when there are no claims.
func TestLiveClaimsLocked_Empty(t *testing.T) {
	p, err := New("test-driver", validDeps())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	got := p.LiveClaimsLocked()
	if len(got) != 0 {
		t.Errorf("LiveClaimsLocked() = %v, want empty map", got)
	}
}

// TestLiveClaimsLocked_Snapshot verifies that LiveClaimsLocked returns a
// snapshot matching p.claims, and that mutating the returned map/slices does
// not corrupt the plugin's internal state (caller holds the resmgr lock, but
// the returned value must still be a defensive copy of the per-claim slice).
func TestLiveClaimsLocked_Snapshot(t *testing.T) {
	p, err := New("test-driver", validDeps())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	p.claims[types.UID("uid-a")] = &ClaimState{
		UID:    "uid-a",
		Allocs: []ResultAlloc{{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "0-3", ClassName: "gold"}},
	}
	p.claims[types.UID("uid-b")] = &ClaimState{
		UID: "uid-b",
		Allocs: []ResultAlloc{
			{Device: "dev1", PkgID: 0, PunitID: 1, CPUs: "4-5", ClassName: "silver"},
			{Device: "dev2", PkgID: 0, PunitID: 1, CPUs: "6-7", ClassName: "silver"},
		},
	}

	got := p.LiveClaimsLocked()
	if len(got) != 2 {
		t.Fatalf("LiveClaimsLocked() len = %d, want 2", len(got))
	}
	if allocs := got[types.UID("uid-a")]; len(allocs) != 1 || allocs[0].ClassName != "gold" {
		t.Errorf("LiveClaimsLocked()[uid-a] = %+v, want one gold alloc", allocs)
	}
	if allocs := got[types.UID("uid-b")]; len(allocs) != 2 {
		t.Errorf("LiveClaimsLocked()[uid-b] len = %d, want 2", len(allocs))
	}

	// Mutating the returned slice must not affect p.claims (defensive copy).
	got[types.UID("uid-a")][0].ClassName = "mutated"
	if p.claims[types.UID("uid-a")].Allocs[0].ClassName != "gold" {
		t.Errorf("LiveClaimsLocked() leaked internal state: p.claims mutated via returned snapshot")
	}
}

// TestRestoreClaimsLocked_RebuildsAccounting verifies that RestoreClaimsLocked
// calls AccountHpCpus for each alloc in p.claims, rebuilding accounting after
// a Reconfigure reset (simulated by a fresh trackingClaimAllocator).
func TestRestoreClaimsLocked_RebuildsAccounting(t *testing.T) {
	alloc := &trackingClaimAllocator{}
	deps := validDeps()
	deps.ClaimAllocator = alloc

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	// Pre-populate two claims with one alloc each.
	p.claims[types.UID("uid-a")] = &ClaimState{
		UID:    "uid-a",
		Allocs: []ResultAlloc{{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "0-3", ClassName: "gold"}},
	}
	p.claims[types.UID("uid-b")] = &ClaimState{
		UID:    "uid-b",
		Allocs: []ResultAlloc{{Device: "dev1", PkgID: 0, PunitID: 1, CPUs: "4-7", ClassName: "gold"}},
	}

	if err := p.RestoreClaimsLocked(); err != nil {
		t.Fatalf("RestoreClaimsLocked() unexpected error: %v", err)
	}
	if len(alloc.accounts) != 2 {
		t.Errorf("AccountHpCpus called %d times, want 2", len(alloc.accounts))
	}
	// Verify the union of all accounted CPU sets matches the expected total.
	// Map iteration order is non-deterministic so we check the union.
	totalAccounted := cpuset.New()
	for _, cs := range alloc.accounts {
		totalAccounted = totalAccounted.Union(cs)
	}
	wantTotal := cpuset.MustParse("0-7")
	if !totalAccounted.Equals(wantTotal) {
		t.Errorf("AccountHpCpus total CPUs = %v, want %v", totalAccounted, wantTotal)
	}
}

// TestRestoreClaims_WithLockWrapper verifies that RestoreClaims acquires the
// lock (via WithLock) and calls AccountHpCpus for each alloc.
func TestRestoreClaims_WithLockWrapper(t *testing.T) {
	var mu sync.Mutex
	alloc := &trackingClaimAllocator{}
	deps := validDeps()
	deps.ClaimAllocator = alloc
	deps.WithLock = func(f func()) {
		mu.Lock()
		defer mu.Unlock()
		f()
	}

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	p.claims[types.UID("uid-c")] = &ClaimState{
		UID:    "uid-c",
		Allocs: []ResultAlloc{{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "0-1", ClassName: "gold"}},
	}

	if err := p.RestoreClaims(); err != nil {
		t.Fatalf("RestoreClaims() unexpected error: %v", err)
	}
	if len(alloc.accounts) != 1 {
		t.Errorf("AccountHpCpus called %d times, want 1", len(alloc.accounts))
	}
}

// TestStart_Reconciliation is an integration test that verifies Start's
// claim reconciliation: live claims (CDI spec present) are kept and
// AccountHpCpus is called; stale claims (CDI spec absent) are dropped and
// saved; orphan CDI specs (not in claims) are removed.
func TestStart_Reconciliation(t *testing.T) {
	liveUID := types.UID("uid-live")
	staleUID := types.UID("uid-stale")
	orphanUID := types.UID("uid-orphan")

	alloc := &trackingClaimAllocator{isHP: true}

	cdiW := &startTestCDIWriter{
		existsByUID: map[types.UID]bool{
			liveUID:  true,
			staleUID: false,
		},
		// ListClaims returns liveUID and orphanUID (orphan has no claim entry).
		listResult: []types.UID{liveUID, orphanUID},
	}

	store := &preloadedClaimStore{
		initial: map[types.UID]*ClaimState{
			liveUID: {
				UID: string(liveUID),
				Allocs: []ResultAlloc{
					{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "0-3", ClassName: "gold"},
				},
			},
			staleUID: {
				UID: string(staleUID),
				Allocs: []ResultAlloc{
					{Device: "dev1", PkgID: 0, PunitID: 1, CPUs: "4-7", ClassName: "gold"},
				},
			},
		},
	}

	fakeClient := fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	})
	deps := validDeps()
	deps.KubeClient = fakeClient
	deps.NodeName = "test-node"
	deps.RegistrarDir = t.TempDir()
	deps.PluginDataDir = t.TempDir()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.ClaimStore = store

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	defer p.Stop()

	// Only liveUID should remain in p.claims.
	if _, ok := p.claims[liveUID]; !ok {
		t.Error("liveUID missing from p.claims after Start")
	}
	if _, ok := p.claims[staleUID]; ok {
		t.Error("staleUID still in p.claims after Start — should have been dropped")
	}

	// AccountHpCpus called once (for the live claim's single alloc).
	if len(alloc.accounts) != 1 {
		t.Errorf("AccountHpCpus called %d times, want 1", len(alloc.accounts))
	}

	// ClaimStore.Save called once (to persist the drop of staleUID).
	if store.saved != 1 {
		t.Errorf("ClaimStore.Save called %d times, want 1", store.saved)
	}
	// staleUID must not be in the saved map.
	if store.savedClaims != nil {
		if _, ok := store.savedClaims[staleUID]; ok {
			t.Error("staleUID still in saved ClaimStore after Start")
		}
	}

	// Orphan sweep: orphanUID must have been removed via CDI writer.
	found := false
	for _, uid := range cdiW.removed {
		if uid == orphanUID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("orphanUID was not removed by orphan sweep (removed = %v)", cdiW.removed)
	}
	// staleUID should NOT be in cdiW.removed (it had no CDI spec to remove).
	for _, uid := range cdiW.removed {
		if uid == staleUID {
			t.Error("staleUID was unexpectedly passed to CDI RemoveClaim")
		}
	}
}

// TestStart_InactiveAllocator verifies that if AccountHpCpus returns an error
// (simulating an inactive allocator), all claims with CDI specs present are
// still kept with a warning (not dropped).
func TestStart_InactiveAllocator(t *testing.T) {
	uid1 := types.UID("uid-1")
	uid2 := types.UID("uid-2")

	alloc := &trackingClaimAllocator{accountErr: errors.New("allocator inactive")}

	cdiW := &startTestCDIWriter{
		existsByUID: map[types.UID]bool{uid1: true, uid2: true},
		listResult:  []types.UID{uid1, uid2},
	}

	store := &preloadedClaimStore{
		initial: map[types.UID]*ClaimState{
			uid1: {UID: string(uid1), Allocs: []ResultAlloc{{Device: "dev0", PkgID: 0, PunitID: 0, CPUs: "0-3", ClassName: "gold"}}},
			uid2: {UID: string(uid2), Allocs: []ResultAlloc{{Device: "dev1", PkgID: 0, PunitID: 1, CPUs: "4-7", ClassName: "gold"}}},
		},
	}

	fakeClient := fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	})
	deps := validDeps()
	deps.KubeClient = fakeClient
	deps.NodeName = "test-node"
	deps.RegistrarDir = t.TempDir()
	deps.PluginDataDir = t.TempDir()
	deps.ClaimAllocator = alloc
	deps.CDIWriter = cdiW
	deps.ClaimStore = store

	p, err := New("test-driver", deps)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	defer p.Stop()

	// Both claims must still be present despite AccountHpCpus errors.
	if _, ok := p.claims[uid1]; !ok {
		t.Error("uid1 missing from p.claims — should be kept despite AccountHpCpus error")
	}
	if _, ok := p.claims[uid2]; !ok {
		t.Error("uid2 missing from p.claims — should be kept despite AccountHpCpus error")
	}
	// No drops occurred, so ClaimStore.Save must not have been called.
	if store.saved != 0 {
		t.Errorf("ClaimStore.Save called %d times, want 0 (no drops)", store.saved)
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
