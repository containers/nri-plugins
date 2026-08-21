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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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

func TestNew_ReturnsNotImplemented(t *testing.T) {
	p, err := New("test-driver", Deps{})
	if p != nil {
		t.Errorf("New() returned non-nil Plugin, want nil")
	}
	if err == nil {
		t.Fatal("New() returned nil error, want non-nil")
	}
	if !errors.Is(err, errNotImplemented) {
		t.Errorf("New() error = %v, want errors.Is(err, errNotImplemented) == true", err)
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
