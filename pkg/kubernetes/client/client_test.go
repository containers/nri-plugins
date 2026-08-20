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
	"net/http"
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

// TestNew_NoOptions verifies that New() with no options falls back to
// WithInClusterConfig — which fails when the test runs outside a Pod.
// Only exercises the fallback path; the success path requires an in-
// cluster environment which is not usable from a unit test.
func TestNew_NoOptions(t *testing.T) {
	skipIfInCluster(t)
	c, err := New()
	if err == nil {
		t.Fatalf("expected error from New() outside a cluster, got: %+v", c)
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("expected rest.ErrNotInCluster, got: %v", err)
	}
	if c != nil {
		t.Errorf("expected nil client on error, got %+v", c)
	}
}

func TestNew_WithKubeConfig_Success(t *testing.T) {
	c, err := New(WithKubeConfig(fixtureKubeconfig))
	if err != nil {
		t.Fatalf("New(WithKubeConfig) returned error: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil client with no error")
	}
	if c.K8sClient() == nil {
		t.Error("Client.K8sClient() is nil")
	}
	if c.RestConfig() == nil {
		t.Error("Client.RestConfig() is nil")
	}
	if c.HttpClient() == nil {
		t.Error("Client.HttpClient() is nil")
	}
}

func TestNew_WithKubeConfig_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	c, err := New(WithKubeConfig(missing))
	if err == nil {
		t.Fatalf("expected error for missing kubeconfig, got: %+v", c)
	}
	if c != nil {
		t.Errorf("expected nil client on error, got %+v", c)
	}
}

func TestNew_WithInClusterConfig(t *testing.T) {
	skipIfInCluster(t)
	c, err := New(WithInClusterConfig())
	if err == nil {
		t.Fatalf("expected error outside a cluster, got: %+v", c)
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("expected rest.ErrNotInCluster, got: %v", err)
	}
	if c != nil {
		t.Errorf("expected nil client on error, got %+v", c)
	}
}

func TestNew_WithKubeOrInClusterConfig_EmptyFallsBack(t *testing.T) {
	skipIfInCluster(t)
	// Empty file path should fall back to in-cluster, which fails outside
	// a cluster; that's how we know the fallback path was taken.
	_, err := New(WithKubeOrInClusterConfig(""))
	if !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("expected rest.ErrNotInCluster from empty-file fallback, got: %v", err)
	}
}

func TestNew_WithKubeOrInClusterConfig_FileWins(t *testing.T) {
	c, err := New(WithKubeOrInClusterConfig(fixtureKubeconfig))
	if err != nil {
		t.Fatalf("New(WithKubeOrInClusterConfig(file)) returned error: %v", err)
	}
	if c == nil || c.K8sClient() == nil {
		t.Fatalf("expected non-nil client, got %+v", c)
	}
}

func TestNew_WithRestConfig(t *testing.T) {
	// Build a config via the file helper first; use it as input to WithRestConfig
	// to skip the file/in-cluster resolvers entirely.
	cfg, err := GetConfigForFile(fixtureKubeconfig)
	if err != nil {
		t.Fatalf("GetConfigForFile fixture failed: %v", err)
	}
	c, err := New(WithRestConfig(cfg))
	if err != nil {
		t.Fatalf("New(WithRestConfig) returned error: %v", err)
	}
	if c == nil || c.K8sClient() == nil {
		t.Fatalf("expected non-nil client, got %+v", c)
	}
}

func TestNew_WithHttpClient(t *testing.T) {
	// Provide a pre-built HTTP client, verify HttpClient() returns the same pointer.
	hc := &http.Client{}
	c, err := New(WithHttpClient(hc), WithKubeConfig(fixtureKubeconfig))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if c.HttpClient() != hc {
		t.Errorf("HttpClient() returned different pointer than provided: got %p, want %p", c.HttpClient(), hc)
	}
}

// newTestConfig returns a fresh rest.Config. Built inline (not via the
// fixture) so tests fully control every field. Uses Insecure=true and no
// CAData so rest.HTTPClientFor inside New() does not try to parse CAData
// as a PEM block.
func newTestConfig() *rest.Config {
	return &rest.Config{
		Host: "https://example.com:6443",
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
		UserAgent: "test-user-agent",
	}
}

// TestClient_RestConfig_CopySemantics verifies RestConfig() returns a copy
// with rest.CopyConfig semantics: top-level and value-struct fields are
// safely overwritable on the returned value without affecting subsequent
// RestConfig() calls. Nested map/slice contents are NOT tested here —
// they share storage per rest.CopyConfig's contract.
func TestClient_RestConfig_CopySemantics(t *testing.T) {
	c, err := New(WithRestConfig(newTestConfig()))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	first := c.RestConfig()
	first.Host = "https://mutated.example.com"
	first.UserAgent = "mutated-user-agent"

	second := c.RestConfig()
	if second.Host == first.Host {
		t.Errorf("RestConfig Host mutation leaked: got %q, want unchanged", second.Host)
	}
	if second.UserAgent == first.UserAgent {
		t.Errorf("RestConfig UserAgent mutation leaked: got %q, want unchanged", second.UserAgent)
	}
}

// TestClient_WithRestConfig_CopySemanticsOnInput verifies that WithRestConfig
// takes a rest.CopyConfig copy of its input — post-New mutations of the
// original config's top-level fields do not leak into the client.
func TestClient_WithRestConfig_CopySemanticsOnInput(t *testing.T) {
	cfg := newTestConfig()

	c, err := New(WithRestConfig(cfg))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	cfg.Host = "https://mutated-input.example.com"
	cfg.UserAgent = "mutated-input-user-agent"

	rc := c.RestConfig()
	if rc.Host == cfg.Host {
		t.Errorf("input-side Host mutation leaked to client: got %q", rc.Host)
	}
	if rc.UserAgent == cfg.UserAgent {
		t.Errorf("input-side UserAgent mutation leaked to client: got %q", rc.UserAgent)
	}
}

func TestWithAcceptContentTypes(t *testing.T) {
	c, err := New(
		WithKubeConfig(fixtureKubeconfig),
		WithAcceptContentTypes(ContentTypeProtobuf, ContentTypeJSON),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	got := c.RestConfig().AcceptContentTypes
	want := ContentTypeProtobuf + "," + ContentTypeJSON
	if got != want {
		t.Errorf("AcceptContentTypes: got %q, want %q", got, want)
	}
}

func TestWithContentType(t *testing.T) {
	c, err := New(
		WithKubeConfig(fixtureKubeconfig),
		WithContentType(ContentTypeProtobuf),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if got := c.RestConfig().ContentType; got != ContentTypeProtobuf {
		t.Errorf("ContentType: got %q, want %q", got, ContentTypeProtobuf)
	}
}

// TestContentType_OrderIndependence exercises the retry-when-config-not-set
// mechanism: content-type options passed BEFORE the config-source option
// must produce the same result as content-type options passed AFTER.
func TestContentType_OrderIndependence(t *testing.T) {
	// Case A: config-source first, content-type second — no retry needed.
	a, err := New(
		WithKubeConfig(fixtureKubeconfig),
		WithContentType(ContentTypeProtobuf),
		WithAcceptContentTypes(ContentTypeProtobuf, ContentTypeJSON),
	)
	if err != nil {
		t.Fatalf("case A New returned error: %v", err)
	}

	// Case B: content-type first, config-source second — retry required.
	b, err := New(
		WithContentType(ContentTypeProtobuf),
		WithAcceptContentTypes(ContentTypeProtobuf, ContentTypeJSON),
		WithKubeConfig(fixtureKubeconfig),
	)
	if err != nil {
		t.Fatalf("case B New returned error: %v", err)
	}

	if a.RestConfig().ContentType != b.RestConfig().ContentType {
		t.Errorf("ContentType differs: a=%q, b=%q", a.RestConfig().ContentType, b.RestConfig().ContentType)
	}
	if a.RestConfig().AcceptContentTypes != b.RestConfig().AcceptContentTypes {
		t.Errorf("AcceptContentTypes differs: a=%q, b=%q", a.RestConfig().AcceptContentTypes, b.RestConfig().AcceptContentTypes)
	}
}

// TestContentType_OnlyNoConfigSource verifies that content-type options
// alone (no config-source option) still work: the WithInClusterConfig
// fallback runs first, then the retry list is re-applied. Outside a Pod
// the fallback errors — we assert that error path.
func TestContentType_OnlyNoConfigSource(t *testing.T) {
	skipIfInCluster(t)
	_, err := New(WithContentType(ContentTypeProtobuf))
	if !errors.Is(err, rest.ErrNotInCluster) {
		t.Errorf("expected rest.ErrNotInCluster from in-cluster fallback, got: %v", err)
	}
}

// TestContentType_MultipleRetries verifies that when multiple content-type
// options are deferred to the retry list, all of them apply in the order
// they were passed (last write within the retry list wins). Note: this
// mechanism means options in the retry list execute AFTER all non-retry
// options, so a retry-list content-type overrides any content-type
// applied earlier during the first pass. Callers should therefore not
// mix retry-required and non-retry setters for the same setting.
func TestContentType_MultipleRetries(t *testing.T) {
	c, err := New(
		WithAcceptContentTypes(ContentTypeProtobuf), // retry #1
		WithAcceptContentTypes(ContentTypeJSON),     // retry #2 — applies last within retry
		WithContentType(ContentTypeProtobuf),        // retry #3
		WithKubeConfig(fixtureKubeconfig),           // provides the config
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	// Retry-list order preserved: last WithAcceptContentTypes wins.
	if got := c.RestConfig().AcceptContentTypes; got != ContentTypeJSON {
		t.Errorf("last-applied retry AcceptContentTypes should win: got %q, want %q", got, ContentTypeJSON)
	}
	if got := c.RestConfig().ContentType; got != ContentTypeProtobuf {
		t.Errorf("ContentType: got %q, want %q", got, ContentTypeProtobuf)
	}
}

func TestClient_Close_Idempotent(t *testing.T) {
	c, err := New(WithKubeConfig(fixtureKubeconfig))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	// First call — must not panic.
	c.Close()
	// Second call — must also not panic.
	c.Close()
}
