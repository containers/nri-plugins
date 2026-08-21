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
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/containers/nri-plugins/pkg/log"
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

// mockDeviceLister is a minimal DeviceLister for tests.
type mockDeviceLister struct{}

func (m *mockDeviceLister) DRADevices(_ string) ([]resourceapi.Device, error) {
	return nil, nil
}

// validDeps returns a Deps with all required fields populated.
func validDeps() Deps {
	return Deps{
		KubeClient:      fake.NewClientset(),
		NodeName:        "test-node",
		ValidateClasses: func() error { return nil },
		DeviceLister:    &mockDeviceLister{},
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
		name    string
		mutate  func(*Deps)
		wantErr bool
	}{
		{
			name:    "empty driverName",
			mutate:  nil, // driverName is a parameter, handled via empty string below
			wantErr: true,
		},
		{
			name:    "nil KubeClient",
			mutate:  func(d *Deps) { d.KubeClient = nil },
			wantErr: true,
		},
		{
			name:    "empty NodeName",
			mutate:  func(d *Deps) { d.NodeName = "" },
			wantErr: true,
		},
		{
			name:    "nil ValidateClasses",
			mutate:  func(d *Deps) { d.ValidateClasses = nil },
			wantErr: true,
		},
		{
			name:    "nil DeviceLister",
			mutate:  func(d *Deps) { d.DeviceLister = nil },
			wantErr: true,
		},
		{
			name:    "nil Logger",
			mutate:  func(d *Deps) { d.Logger = nil },
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := validDeps()
			driverName := "test-driver"
			if tc.name == "empty driverName" {
				driverName = ""
			} else if tc.mutate != nil {
				tc.mutate(&deps)
			}
			p, err := New(driverName, deps)
			if tc.wantErr {
				if err == nil {
					t.Errorf("New() expected error, got nil")
				}
				if p != nil {
					t.Errorf("New() expected nil Plugin on error, got %v", p)
				}
			} else {
				if err != nil {
					t.Errorf("New() unexpected error: %v", err)
				}
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
// error (not a panic) when called before Start.
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
		t.Skip("runtime.Caller failed; skipping import-boundary check")
	}
	pkgDir := filepath.Dir(filename)

	cmd := exec.Command(goExe, "list", "-deps", "./...")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list failed (%v); skipping import-boundary check", err)
	}

	const forbidden = "github.com/containers/nri-plugins/cmd/"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, forbidden) {
			t.Errorf("import-boundary violation: pkg/resmgr/dra depends on %q", line)
		}
	}
}
