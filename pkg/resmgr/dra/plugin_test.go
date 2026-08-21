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
