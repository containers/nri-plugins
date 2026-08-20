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

package client

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
)

// fixtureKubeconfig is the path to a minimal valid kubeconfig used by tests
// that need a config source without contacting a real API server.
const fixtureKubeconfig = "testdata/kubeconfig-example.yaml"

// skipIfInCluster fails-fast for tests that assert not-in-cluster behavior
// when they happen to run inside a Pod (e.g. e2e CI).
func skipIfInCluster(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		t.Skip("running inside a Kubernetes Pod; skipping not-in-cluster test")
	}
}

func TestGetConfigForFile_Success(t *testing.T) {
	cfg, err := GetConfigForFile(fixtureKubeconfig)
	if err != nil {
		t.Fatalf("GetConfigForFile(%q) returned error: %v", fixtureKubeconfig, err)
	}
	if cfg == nil {
		t.Fatal("GetConfigForFile returned nil config with no error")
	}
	if cfg.Host == "" {
		t.Errorf("returned config has empty Host; expected value from fixture")
	}
}

func TestGetConfigForFile_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nonexistent-kubeconfig.yaml")
	cfg, err := GetConfigForFile(missing)
	if err == nil {
		t.Fatalf("expected error for missing file, got config: %+v", cfg)
	}
	if cfg != nil {
		t.Errorf("expected nil config on error, got %+v", cfg)
	}
}

func TestGetConfigForFile_MalformedFile(t *testing.T) {
	malformed := filepath.Join(t.TempDir(), "malformed-kubeconfig.yaml")
	if err := os.WriteFile(malformed, []byte("this: is: not: valid: yaml: {[}\n"), 0o600); err != nil {
		t.Fatalf("failed to write malformed fixture: %v", err)
	}
	cfg, err := GetConfigForFile(malformed)
	if err == nil {
		t.Fatalf("expected error for malformed file, got config: %+v", cfg)
	}
	if cfg != nil {
		t.Errorf("expected nil config on error, got %+v", cfg)
	}
}

func TestInClusterConfig_NotInCluster(t *testing.T) {
	skipIfInCluster(t)
	cfg, err := InClusterConfig()
	if err == nil {
		t.Fatalf("expected error outside a cluster, got config: %+v", cfg)
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("expected rest.ErrNotInCluster, got: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil config on error, got %+v", cfg)
	}
}
