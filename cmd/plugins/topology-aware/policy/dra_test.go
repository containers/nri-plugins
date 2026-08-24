// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/testutils"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// setupDRATestPolicy builds a real *policy from the "server" sysfs test
// data (same fixture as newDRATestPolicy in resources_test.go) and runs it
// through Setup(). mutateCfg and mutateOpts (both optional) let each test
// case tweak the Config / BackendOptions before Setup() runs; preSetup
// (optional) runs on the freshly constructed, not-yet-Setup *policy — used
// to inject p.cdiDir before buildDRAPlugin would otherwise fall back to the
// real /var/run/cdi default.
func setupDRATestPolicy(
	t *testing.T,
	mutateCfg func(*cfgapi.Config),
	mutateOpts func(*policyapi.BackendOptions),
	preSetup func(*policy),
) (*policy, error) {
	t.Helper()

	dir, err := os.MkdirTemp("", "nri-resource-policy-test-sysfs-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { removeAll(t, dir) })

	if err := testutils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		t.Fatalf("failed to uncompress test sysfs data: %v", err)
	}

	sys, err := system.DiscoverSystemAt(path.Join(dir, "sysfs", "server", "sys"))
	if err != nil {
		t.Fatalf("failed to discover test system: %v", err)
	}

	cfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{
			cfgapi.CPU: "750m",
		},
	}
	if mutateCfg != nil {
		mutateCfg(cfg)
	}

	opts := &policyapi.BackendOptions{
		Cache:  &mockCache{},
		System: sys,
		Config: cfg,
	}
	if mutateOpts != nil {
		mutateOpts(opts)
	}

	p := New().(*policy)
	if preSetup != nil {
		preSetup(p)
	}

	return p, p.Setup(opts)
}

// withOneHPClass sets a single valid HP cpuClass, sufficient to make
// initialize() install a non-nil p.cpuClasses handler.
func withOneHPClass(cfg *cfgapi.Config) {
	cfg.CPUClasses = []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}}
}

// withDRAEnabled sets cfg.DRA.Enabled = true (SharedCounters left false).
func withDRAEnabled(cfg *cfgapi.Config) {
	cfg.DRA = &cfgapi.TopologyAwareDRA{Enabled: true}
}

// TestSetupDRADisabledLeavesPluginNil verifies that when DRA is disabled
// (the default zero-value Config.DRA == nil), Setup() never calls
// buildDRAPlugin, leaving p.draPlugin nil, and that the DRA-adjacent
// lifecycle methods (Start, Stop) remain no-ops/no-panics against that nil
// state — i.e. behavior is unchanged from before Step 8.
func TestSetupDRADisabledLeavesPluginNil(t *testing.T) {
	p, err := setupDRATestPolicy(t, withOneHPClass, nil, nil)
	if err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}
	if p.draPlugin != nil {
		t.Error("draPlugin: got non-nil, want nil (DRA disabled)")
	}

	if err := p.Start(); err != nil {
		t.Fatalf("Start() with DRA disabled: got %v, want nil", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop() with DRA disabled: got %v, want nil", err)
	}
}

// TestSetupDRAEnabledNilKubeClientLeavesPluginNil verifies that when DRA is
// enabled but opts.KubeClientFn() returns a nil kubernetes.Interface (e.g.
// local-config mode, or too early during agent startup), Setup() logs a
// warning and leaves p.draPlugin nil rather than failing.
func TestSetupDRAEnabledNilKubeClientLeavesPluginNil(t *testing.T) {
	p, err := setupDRATestPolicy(t,
		func(cfg *cfgapi.Config) { withOneHPClass(cfg); withDRAEnabled(cfg) },
		func(opts *policyapi.BackendOptions) {
			opts.KubeClientFn = func() kubernetes.Interface { return nil }
			opts.NodeName = "test-node"
			opts.WithLock = func(f func()) { f() }
		},
		nil,
	)
	if err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}
	if p.draPlugin != nil {
		t.Error("draPlugin: got non-nil, want nil (no kube client)")
	}
}

// TestSetupDRAEnabledEmptyNodeNameLeavesPluginNil verifies that when DRA is
// enabled, a kube client is available, but opts.NodeName is empty (also
// possible in local-config mode), Setup() logs a warning and leaves
// p.draPlugin nil rather than failing or calling dra.New with an empty
// NodeName (which would itself error).
func TestSetupDRAEnabledEmptyNodeNameLeavesPluginNil(t *testing.T) {
	p, err := setupDRATestPolicy(t,
		func(cfg *cfgapi.Config) { withOneHPClass(cfg); withDRAEnabled(cfg) },
		func(opts *policyapi.BackendOptions) {
			opts.KubeClientFn = func() kubernetes.Interface { return fake.NewClientset() }
			opts.NodeName = ""
			opts.WithLock = func(f func()) { f() }
		},
		nil,
	)
	if err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}
	if p.draPlugin != nil {
		t.Error("draPlugin: got non-nil, want nil (empty node name)")
	}
}

// TestSetupDRAEnabledNoCPUClassesLeavesPluginNil verifies that when DRA is
// enabled but no cpuClasses are configured (p.cpuClasses stays nil after
// initialize()), Setup() logs a warning and leaves p.draPlugin nil.
func TestSetupDRAEnabledNoCPUClassesLeavesPluginNil(t *testing.T) {
	p, err := setupDRATestPolicy(t,
		withDRAEnabled, // no withOneHPClass: CPUClasses stays empty
		func(opts *policyapi.BackendOptions) {
			opts.KubeClientFn = func() kubernetes.Interface { return fake.NewClientset() }
			opts.NodeName = "test-node"
			opts.WithLock = func(f func()) { f() }
		},
		nil,
	)
	if err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}
	if p.cpuClasses != nil {
		t.Fatal("test setup invariant violated: p.cpuClasses is non-nil, want nil")
	}
	if p.draPlugin != nil {
		t.Error("draPlugin: got non-nil, want nil (no cpuClass configuration)")
	}
}

// TestSetupDRAEnabledValidDepsBuildsPlugin verifies that when DRA is
// enabled and every dependency (kube client, node name, cpuClass
// configuration) is available, Setup() builds a non-nil p.draPlugin.
func TestSetupDRAEnabledValidDepsBuildsPlugin(t *testing.T) {
	p, err := setupDRATestPolicy(t,
		func(cfg *cfgapi.Config) { withOneHPClass(cfg); withDRAEnabled(cfg) },
		func(opts *policyapi.BackendOptions) {
			opts.KubeClientFn = func() kubernetes.Interface { return fake.NewClientset() }
			opts.NodeName = "test-node"
			opts.WithLock = func(f func()) { f() }
		},
		func(p *policy) { p.cdiDir = t.TempDir() },
	)
	if err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}
	if p.draPlugin == nil {
		t.Fatal("draPlugin: got nil, want non-nil")
	}
}

// TestStopCancelsContextAndStopsDRAPlugin verifies that Stop() cancels
// p.draCtx and calls draPlugin.Stop(), and that calling Stop() a second
// time is safe (both context.CancelFunc and dra.Plugin.Stop are documented
// as idempotent).
func TestStopCancelsContextAndStopsDRAPlugin(t *testing.T) {
	p := &policy{}
	p.draPlugin = newTestDRAPlugin(t, cpuset.New(0), "dev0")

	ctx, cancel := context.WithCancel(context.Background())
	p.draCtx = ctx
	p.draCtxCancel = cancel

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	if ctx.Err() == nil {
		t.Error("Stop() did not cancel draCtx")
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop() = %v, want nil", err)
	}
}

// newConflictingTierClassHandler builds a *cpuclass.Handler configured with
// two managed PCT classes at the same tier (both PctPriority: "high"), the
// exact shape ValidateCPUClassesForDRA rejects when sharedCounters is
// false. SST support is left disabled (PCT inactive) since
// ValidateCPUClassesForDRA inspects the class list itself, not device
// activity — mirrors newInactiveClassHandler in dra_adapter_test.go.
func newConflictingTierClassHandler(t *testing.T) *cpuclass.Handler {
	t.Helper()
	t.Setenv("OVERRIDE_SST", "")
	h, err := cpuclass.New(&adapterTestSys{})
	if err != nil {
		t.Fatalf("cpuclass.New() failed: %v", err)
	}
	classes := []*cfgapi.CPUClass{
		{Name: "hp1", PctPriority: "high"},
		{Name: "hp2", PctPriority: "high"},
	}
	if err := h.Configure(cpuclass.ConfigSpec{
		Classes: classes,
		Allowed: cpuset.MustParse("0-7"),
	}); err != nil {
		t.Fatalf("Configure() failed: %v", err)
	}
	return h
}

// TestBuildDRAPluginValidateClassesUsesLiveConfig verifies that the
// ValidateClasses closure buildDRAPlugin hands to the DRA plugin reads
// p.cfg live (via the nil-safe DRASharedCounters() getter) rather than a
// config snapshot taken at buildDRAPlugin call time — required so that a
// later Reconfigure() (which swaps p.cfg for a new *Config) is observed
// without rebuilding the plugin.
//
// PublishResources is used as the probe: it runs ValidateClasses before
// checking whether Start() has been called, so the distinction between "a
// tier-conflict error" (ValidateClasses failed) and "called before Start"
// (ValidateClasses passed) is directly observable without ever calling
// Start() (which would require real kubelet registration directories).
func TestBuildDRAPluginValidateClassesUsesLiveConfig(t *testing.T) {
	classes := []*cfgapi.CPUClass{
		{Name: "hp1", PctPriority: "high"},
		{Name: "hp2", PctPriority: "high"},
	}
	p := &policy{
		cache:      &mockCache{},
		cpuClasses: newConflictingTierClassHandler(t),
		cfg: &cfgapi.Config{
			CPUClasses: classes,
			DRA:        &cfgapi.TopologyAwareDRA{Enabled: true, SharedCounters: false},
		},
		cdiDir: t.TempDir(),
	}

	opts := &policyapi.BackendOptions{
		KubeClientFn: func() kubernetes.Interface { return fake.NewClientset() },
		NodeName:     "test-node",
		WithLock:     func(f func()) { f() },
	}

	if err := p.buildDRAPlugin(opts); err != nil {
		t.Fatalf("buildDRAPlugin() = %v, want nil", err)
	}
	if p.draPlugin == nil {
		t.Fatal("draPlugin: got nil, want non-nil")
	}

	// SharedCounters is false and both classes are at the same PCT tier:
	// ValidateClasses must fail with a tier-conflict error.
	if err := p.draPlugin.PublishResources(context.Background()); err == nil || !strings.Contains(err.Error(), "tier") {
		t.Fatalf("PublishResources() before simulated Reconfigure = %v, want tier-conflict error", err)
	}

	// Simulate a Reconfigure that swaps p.cfg for a new *Config with
	// SharedCounters: true. buildDRAPlugin's ValidateClasses closure
	// captured p, not cfg, so it must observe this change immediately —
	// with no need to rebuild p.draPlugin.
	p.cfg = &cfgapi.Config{
		CPUClasses: classes,
		DRA:        &cfgapi.TopologyAwareDRA{Enabled: true, SharedCounters: true},
	}

	err := p.draPlugin.PublishResources(context.Background())
	if err == nil || !strings.Contains(err.Error(), "called before Start") {
		t.Fatalf("PublishResources() after simulated Reconfigure = %v, want \"called before Start\" error (proves ValidateClasses passed)", err)
	}
}
