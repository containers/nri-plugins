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
	libcpu "github.com/containers/nri-plugins/pkg/lib/cpu"
	"github.com/containers/nri-plugins/pkg/lib/hardware"
	"os"
	"path"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	"github.com/containers/nri-plugins/pkg/resmgr/cpuclass"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	"github.com/containers/nri-plugins/pkg/testutils"
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

	machine, err := hardware.Discover(hardware.WithRoot(path.Join(dir, "sysfs", "server")))
	if err != nil {
		t.Fatalf("failed to discover test machine: %v", err)
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
		Cache:   &mockCache{},
		Machine: machine,
		Config:  cfg,
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

// TestSetupDRAEnabledNoCPUClassesBuildsPluginWithNoDevices verifies that
// when DRA is enabled but no cpuClasses are configured (p.cpuClasses stays
// nil after initialize()), Setup() still builds a non-nil p.draPlugin,
// publishing zero devices — rather than leaving it permanently nil, which
// would prevent a later Reconfigure() that adds cpuClasses from ever
// recovering (buildDRAPlugin only ever runs once, from Setup()).
func TestSetupDRAEnabledNoCPUClassesBuildsPluginWithNoDevices(t *testing.T) {
	p, err := setupDRATestPolicy(t,
		withDRAEnabled, // no withOneHPClass: CPUClasses stays empty
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
	if p.cpuClasses != nil {
		t.Fatal("test setup invariant violated: p.cpuClasses is non-nil, want nil")
	}
	if p.draPlugin == nil {
		t.Fatal("draPlugin: got nil, want non-nil (empty cpuClasses must not prevent construction)")
	}

	// *dra.Plugin doesn't expose DeviceLister directly; go through the same
	// adapter path buildDRAPlugin wired in, to confirm the nil-cpuClasses
	// case really does yield an empty (not erroring) device list rather
	// than merely a non-nil plugin.
	adapter := &policyDRAAdapter{p: p}
	devs, err := adapter.DRADevices(DRADriverName)
	if err != nil {
		t.Fatalf("DRADevices() unexpected error: %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("DRADevices() = %v, want empty for nil p.cpuClasses", devs)
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

// TestSetupDRAEnabledCDIWriterFailureReturnsError verifies buildDRAPlugin's
// genuine-hard-failure path: unlike the four "not ready yet" guards above
// (nil kube client, empty node name, no cpuClasses — all warn-and-nil), a
// real construction failure in one of its own dependencies (here,
// dra.NewCDIWriter failing because p.cdiDir cannot be created) must be
// returned as an error from Setup(), not swallowed.
func TestSetupDRAEnabledCDIWriterFailureReturnsError(t *testing.T) {
	// A regular file can't be MkdirAll'd into: NewCDIWriter's os.MkdirAll on
	// p.cdiDir (or a path beneath it) will fail with ENOTDIR.
	tmp := t.TempDir()
	blocker := path.Join(tmp, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatalf("failed to create blocking file: %v", err)
	}
	cdiDir := path.Join(blocker, "cdi")

	p, err := setupDRATestPolicy(t,
		func(cfg *cfgapi.Config) { withOneHPClass(cfg); withDRAEnabled(cfg) },
		func(opts *policyapi.BackendOptions) {
			opts.KubeClientFn = func() kubernetes.Interface { return fake.NewClientset() }
			opts.NodeName = "test-node"
			opts.WithLock = func(f func()) { f() }
		},
		func(p *policy) { p.cdiDir = cdiDir },
	)
	if err == nil {
		t.Fatalf("Setup() with an unusable cdiDir: got nil error, want a descriptive error")
	}
	if p.draPlugin != nil {
		t.Errorf("draPlugin: got non-nil after a failed Setup(), want nil")
	}
}

// TestStopCancelsContextAndStopsDRAPlugin verifies that Stop() cancels the
// context draCtxCancel was set with and calls draPlugin.Stop(), and that
// calling Stop() a second time is safe (both context.CancelFunc and
// dra.Plugin.Stop are documented as idempotent).
func TestStopCancelsContextAndStopsDRAPlugin(t *testing.T) {
	p := &policy{}
	p.draPlugin = newTestDRAPlugin(t, libcpu.NewCpuMask(0), "dev0")

	ctx, cancel := context.WithCancel(context.Background())
	p.draCtxCancel = cancel

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	if ctx.Err() == nil {
		t.Error("Stop() did not cancel the context")
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
	h, err := cpuclass.New(oneCpuMachine(t))
	if err != nil {
		t.Fatalf("cpuclass.New() failed: %v", err)
	}
	classes := []*cfgapi.CPUClass{
		{Name: "hp1", PctPriority: "high"},
		{Name: "hp2", PctPriority: "high"},
	}
	if err := h.Configure(cpuclass.ConfigSpec{
		Classes: classes,
		Allowed: libcpu.MustParseCpuMask("0-7"),
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
// without rebuilding the plugin. The second phase resolves the tier
// conflict by dropping to a single published class, not by setting
// SharedCounters: true — that option is rejected outright regardless of
// conflicts, since Model C (KEP-5941) isn't implemented.
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

	// Simulate a Reconfigure that swaps p.cfg for a new *Config whose
	// CPUClasses no longer conflict (down to a single HP class).
	// buildDRAPlugin's ValidateClasses closure captured p, not cfg, so it
	// must observe this change immediately — with no need to rebuild
	// p.draPlugin. (SharedCounters stays false: it is rejected outright by
	// ValidateCPUClassesForDRA regardless of tier conflicts, since Model C
	// isn't implemented — it can no longer be used to "fix" a conflict.)
	p.cfg = &cfgapi.Config{
		CPUClasses: []*cfgapi.CPUClass{classes[0]},
		DRA:        &cfgapi.TopologyAwareDRA{Enabled: true, SharedCounters: false},
	}

	err := p.draPlugin.PublishResources(context.Background())
	if err == nil || !strings.Contains(err.Error(), "called before Start") {
		t.Fatalf("PublishResources() after simulated Reconfigure = %v, want \"called before Start\" error (proves ValidateClasses passed)", err)
	}
}

// ---- Reconfigure refusal: DRA config changes require a restart. ----

// TestReconfigureRefusesAnyChangeWhileDRAEnabled verifies that Reconfigure()
// refuses a config change unrelated to DRA itself (here: ReservedResources)
// once DRA is enabled -- any config change while DRA is (or was) enabled now
// requires a restart, not just a dra.enabled flip or a live-claim conflict.
func TestReconfigureRefusesAnyChangeWhileDRAEnabled(t *testing.T) {
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
		t.Fatal("test setup error: draPlugin unexpectedly nil")
	}

	oldCfg := p.cfg
	oldDRAPlugin := p.draPlugin

	newCfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{cfgapi.CPU: "1000m"},
		CPUClasses:        []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		DRA:               &cfgapi.TopologyAwareDRA{Enabled: true},
	}

	err = p.Reconfigure(newCfg)
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("Reconfigure() = %v, want an error naming the restart requirement", err)
	}
	if p.cfg != oldCfg {
		t.Error("p.cfg was replaced despite the refused Reconfigure")
	}
	if opt != oldCfg {
		t.Error("opt was replaced despite the refused Reconfigure")
	}
	if p.draPlugin != oldDRAPlugin {
		t.Error("draPlugin was replaced despite the refused Reconfigure")
	}
}

// TestReconfigureRefusesDRAEnabledFlipToTrue verifies that Reconfigure()
// refuses a config change that turns DRA on when it was off at Setup() time.
func TestReconfigureRefusesDRAEnabledFlipToTrue(t *testing.T) {
	p := newDRATestPolicy(t) // DRA disabled by default (cfg.DRA == nil)
	if p.draPlugin != nil {
		t.Fatal("test setup error: draPlugin unexpectedly non-nil")
	}

	oldCfg := p.cfg

	newCfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{cfgapi.CPU: "750m"},
		DRA:               &cfgapi.TopologyAwareDRA{Enabled: true},
	}

	err := p.Reconfigure(newCfg)
	if err == nil {
		t.Fatal("Reconfigure() = nil, want an error (DRAEnabled() flip false -> true)")
	}
	if opt != oldCfg {
		t.Error("opt not restored to the pre-Reconfigure config after the refused DRAEnabled flip")
	}
	if p.draPlugin != nil {
		t.Error("draPlugin unexpectedly built by a refused Reconfigure")
	}
}

// TestReconfigureRefusesDRAEnabledFlipToFalse mirrors
// TestReconfigureRefusesDRAEnabledFlipToTrue for the opposite direction: DRA
// was enabled (and successfully built) at Setup() time, and a later
// Reconfigure() tries to turn it off.
func TestReconfigureRefusesDRAEnabledFlipToFalse(t *testing.T) {
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
	oldCfg := p.cfg
	oldDRAPlugin := p.draPlugin

	newCfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{cfgapi.CPU: "750m"},
		CPUClasses:        []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		// DRA left nil: DRAEnabled() == false.
	}

	err = p.Reconfigure(newCfg)
	if err == nil {
		t.Fatal("Reconfigure() = nil, want an error (DRAEnabled() flip true -> false)")
	}
	if opt != oldCfg {
		t.Error("opt not restored to the pre-Reconfigure config after the refused DRAEnabled flip")
	}
	if p.draPlugin != oldDRAPlugin {
		t.Error("draPlugin was replaced/cleared despite the refused Reconfigure")
	}
}

// TestReconfigureUnaffectedWhenDRANeverEnabled verifies that Reconfigure()
// still succeeds for an ordinary config change when DRA was never enabled
// (nil in both the old and new config) -- guards against the DRA restart
// guard accidentally widening to non-DRA reconfigures.
func TestReconfigureUnaffectedWhenDRANeverEnabled(t *testing.T) {
	p := newDRATestPolicy(t)
	if p.cfg.DRAEnabled() {
		t.Fatal("test setup error: DRA unexpectedly enabled")
	}

	newCfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{cfgapi.CPU: "1000m"},
	}

	if err := p.Reconfigure(newCfg); err != nil {
		t.Fatalf("Reconfigure() = %v, want nil (DRA never enabled)", err)
	}
	if p.cfg != newCfg {
		t.Error("p.cfg was not updated by a successful Reconfigure")
	}
}
