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
	"fmt"
	"os"
	"path"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// ---- Task 10 tests: Reconfigure refusal (DRA-enabled flip, cpuClass
// attribute change with live claims) and its rollback/opt-restoration. ----

// sstOverrideJSON builds an OVERRIDE_SST mock document with a single
// package/punit spanning CPUs 0-7, with the given HP CPU capacity. Mirrors
// newActiveClassHandler's fixture (dra_adapter_test.go) but with a
// parameterized capacity, so a test can force a genuine cpuClass DRA-device
// attribute change (capacity) between an initial Setup() and a later
// Reconfigure() without touching the cpuClass config itself.
func sstOverrideJSON(hpCpus int) string {
	return fmt.Sprintf(
		`{"supported":true,"clos_count":4,"packages":[{"id":0,"cpus":"0-7","tf_supported":true,"tf_enabled":true,"cp_supported":true,"cp_enabled":false,"punits":[{"id":0,"cpus":"0-7","max_hp_cpus":%d,"guaranteed_hp_cpus":%d}]}]}`,
		hpCpus, hpCpus)
}

// setupDRATestPolicyWithActivePCT builds a real *policy (via
// setupDRATestPolicy) with DRA enabled, one managed-PCT HP cpuClass ("hp",
// PctPriority "high"), and OVERRIDE_SST/OVERRIDE_SST_STATE_DIR seeded so
// p.cpuClasses ends up backed by a real (mocked) active PCT allocator with
// non-empty DRADevices() output. Required by the Reconfigure refusal tests,
// which need a genuine, comparable device-attribute change between two
// configurations — a nil or inactive Handler always reports zero devices,
// which can never "change".
func setupDRATestPolicyWithActivePCT(t *testing.T, hpCpus int) *policy {
	t.Helper()
	t.Setenv("OVERRIDE_SST", sstOverrideJSON(hpCpus))
	t.Setenv("OVERRIDE_SST_STATE_DIR", t.TempDir())

	p, err := setupDRATestPolicy(t,
		func(cfg *cfgapi.Config) {
			cfg.CPUClasses = []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}}
			withDRAEnabled(cfg)
		},
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
	if p.cpuClasses == nil {
		t.Fatal("test setup error: p.cpuClasses is nil (PCT mock not active)")
	}
	if p.draPlugin == nil {
		t.Fatal("test setup error: p.draPlugin is nil")
	}
	return p
}

// seedLiveHPClaim runs PrepareResourceClaims on p.draPlugin for a single
// claim requesting numCPUs from the first device p.cpuClasses.DRADevices
// reports, so that a subsequent p.draPlugin.LiveClaimClasses() call reports
// that device's cpuClass as live. Fails the test on any error.
func seedLiveHPClaim(t *testing.T, p *policy, uid types.UID, numCPUs int) {
	t.Helper()

	devices, err := p.cpuClasses.DRADevices(DRADriverName)
	if err != nil || len(devices) == 0 {
		t.Fatalf("test setup error: DRADevices() = (%v, %v), want at least one device", devices, err)
	}

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: uid},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Driver:  DRADriverName,
							Pool:    "pool0",
							Device:  devices[0].Name,
							Request: "req0",
							ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
								"nri/cpus": resource.MustParse(fmt.Sprintf("%d", numCPUs)),
							},
						},
					},
				},
			},
		},
	}

	result, err := p.draPlugin.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("PrepareResourceClaims() unexpected error: %v", err)
	}
	r, ok := result[uid]
	if !ok {
		t.Fatalf("PrepareResourceClaims() result missing uid %s", uid)
	}
	if r.Err != nil {
		t.Fatalf("PrepareResourceClaims() PrepareResult.Err = %v, want nil", r.Err)
	}
}

// TestReconfigureRefusesCpuClassAttrChangeWithLiveClaim verifies that a
// Reconfigure() call which changes a cpuClass's DRA-visible device
// attributes (here: HP CPU capacity, via a changed OVERRIDE_SST punit
// definition) is refused when a live DRA claim references that class, and
// that the refusal performs a *full* rollback -- *p = savedPolicy, not just
// opt = p.cfg (see the Reconfigure doc comment in topology-aware-policy.go
// and the plan's "opt global + full rollback" note). Verified by checking
// that p.cpuClasses/p.root are the exact pre-Reconfigure objects (pointer
// identity), not merely equivalent ones, and that opt/p.cfg are restored to
// the pre-Reconfigure *cfgapi.Config.
func TestReconfigureRefusesCpuClassAttrChangeWithLiveClaim(t *testing.T) {
	p := setupDRATestPolicyWithActivePCT(t, 4)

	seedLiveHPClaim(t, p, types.UID("live-claim-1"), 2)

	liveClasses := p.draPlugin.LiveClaimClasses()
	if liveClasses["hp"] == 0 {
		t.Fatalf("test setup error: LiveClaimClasses() = %v, want class \"hp\" > 0", liveClasses)
	}

	oldCfg := p.cfg
	oldCpuClasses := p.cpuClasses
	oldRoot := p.root

	// Change the mocked hardware's HP CPU capacity: once initialize()
	// creates a fresh cpuclass.Handler from this env var, this changes the
	// "nri/cpus" capacity attribute buildDRADevices emits for class "hp",
	// without touching the cpuClass config itself (still {"hp", "high"}).
	t.Setenv("OVERRIDE_SST", sstOverrideJSON(2))

	newCfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{cfgapi.CPU: "750m"},
		CPUClasses:        []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		DRA:               &cfgapi.TopologyAwareDRA{Enabled: true},
	}

	err := p.Reconfigure(newCfg)
	if err == nil {
		t.Fatal("Reconfigure() = nil, want an error (live claim on a changed cpuClass)")
	}

	if p.cpuClasses != oldCpuClasses {
		t.Error("p.cpuClasses was replaced despite the refused Reconfigure: *p = savedPolicy was not applied")
	}
	if p.root != oldRoot {
		t.Error("p.root was replaced despite the refused Reconfigure: *p = savedPolicy was not applied")
	}
	if opt != oldCfg {
		t.Error("opt not restored to the pre-Reconfigure config after the refused Reconfigure")
	}
	if p.cfg != oldCfg {
		t.Error("p.cfg was not restored to the pre-Reconfigure config after the refused Reconfigure")
	}
}

// TestReconfigureSucceedsWithZeroLiveClaimsForChangedClass verifies that the
// same cpuClass attribute change exercised in
// TestReconfigureRefusesCpuClassAttrChangeWithLiveClaim is *not* refused
// when no live DRA claim references the changed class.
func TestReconfigureSucceedsWithZeroLiveClaimsForChangedClass(t *testing.T) {
	p := setupDRATestPolicyWithActivePCT(t, 4)

	if liveClasses := p.draPlugin.LiveClaimClasses(); len(liveClasses) != 0 {
		t.Fatalf("test setup error: LiveClaimClasses() = %v, want empty", liveClasses)
	}

	t.Setenv("OVERRIDE_SST", sstOverrideJSON(2))

	newCfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{cfgapi.CPU: "750m"},
		CPUClasses:        []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		DRA:               &cfgapi.TopologyAwareDRA{Enabled: true},
	}

	if err := p.Reconfigure(newCfg); err != nil {
		t.Fatalf("Reconfigure() = %v, want nil (no live claims on the changed class)", err)
	}
}

// TestReconfigureRefusesDRAEnabledFlipToTrue verifies that Reconfigure()
// refuses a config change that turns DRA on when it was off at Setup() time.
// buildDRAPlugin only ever runs once, from Setup() (see its doc comment) --
// Reconfigure() never (re)builds p.draPlugin, so flipping cfg.DRAEnabled()
// in a later Reconfigure() would desync p.draPlugin from the new config's
// intent; it is refused outright.
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
// Reconfigure() tries to turn it off. Refused regardless of live claims --
// this test seeds none, showing the flip check fires independently of any
// live-claim check.
func TestReconfigureRefusesDRAEnabledFlipToFalse(t *testing.T) {
	p := setupDRATestPolicyWithActivePCT(t, 4)
	oldCfg := p.cfg
	oldDRAPlugin := p.draPlugin

	newCfg := &cfgapi.Config{
		ReservedResources: cfgapi.Constraints{cfgapi.CPU: "750m"},
		CPUClasses:        []*cfgapi.CPUClass{{Name: "hp", PctPriority: "high"}},
		// DRA left nil: DRAEnabled() == false.
	}

	err := p.Reconfigure(newCfg)
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

// TestPostReconfigurePublishesResourcesWhenDRAEnabled verifies that
// (*policy).PostReconfigure calls draPlugin.PublishResources(p.draCtx) when
// DRA is enabled. PublishResources itself requires Start() to have run (it
// errors "called before Start" otherwise -- see
// TestBuildDRAPluginValidateClassesUsesLiveConfig); that specific error is
// used here as the probe that PostReconfigure actually reached
// PublishResources, without needing a real kubelet registration directory.
func TestPostReconfigurePublishesResourcesWhenDRAEnabled(t *testing.T) {
	p := setupDRATestPolicyWithActivePCT(t, 4)
	p.draCtx = context.Background()

	err := p.PostReconfigure()
	if err == nil || !strings.Contains(err.Error(), "called before Start") {
		t.Fatalf("PostReconfigure() = %v, want a \"called before Start\" error from PublishResources", err)
	}
}

// TestPostReconfigureNilDRAPluginNoop verifies that PostReconfigure is a
// no-op (no panic, nil error) when DRA is disabled (draPlugin == nil, the
// default).
func TestPostReconfigureNilDRAPluginNoop(t *testing.T) {
	p := newDRATestPolicy(t)
	if p.draPlugin != nil {
		t.Fatal("test setup error: draPlugin unexpectedly non-nil")
	}

	if err := p.PostReconfigure(); err != nil {
		t.Fatalf("PostReconfigure() with nil draPlugin = %v, want nil", err)
	}
}

// TestChangedDRAClassesNoDifference verifies that changedDRAClasses reports
// no changed classes when the old and new device snapshots are identical
// (down to attribute values), even when the device slices are ordered
// differently -- groupDRADevicesByClass sorts by device name before
// comparing.
func TestChangedDRAClassesNoDifference(t *testing.T) {
	className := "hp"
	dev := func(name string) resourceapi.Device {
		return resourceapi.Device{
			Name: name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"nri/cpuClass": {StringValue: &className},
			},
		}
	}

	oldDevices := []resourceapi.Device{dev("hp-pkg0-punit0"), dev("hp-pkg0-punit1")}
	newDevices := []resourceapi.Device{dev("hp-pkg0-punit1"), dev("hp-pkg0-punit0")} // reordered

	if changed := changedDRAClasses(oldDevices, newDevices); len(changed) != 0 {
		t.Errorf("changedDRAClasses() = %v, want empty (identical device sets, different order)", changed)
	}
}

// TestChangedDRAClassesDetectsAttributeChange verifies that changedDRAClasses
// reports a class whose device attributes differ between the two snapshots,
// and that an unrelated, unchanged class is not reported.
func TestChangedDRAClassesDetectsAttributeChange(t *testing.T) {
	hpClass, lpClass := "hp", "lp"
	devWithPunit := func(name, class string, punitID int64) resourceapi.Device {
		return resourceapi.Device{
			Name: name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"nri/cpuClass": {StringValue: &class},
				"nri/punitID":  {IntValue: &punitID},
			},
		}
	}

	var punit0, punit1 int64 = 0, 1
	oldDevices := []resourceapi.Device{
		devWithPunit("hp-dev", hpClass, punit0),
		devWithPunit("lp-dev", lpClass, punit0),
	}
	newDevices := []resourceapi.Device{
		devWithPunit("hp-dev", hpClass, punit1), // changed
		devWithPunit("lp-dev", lpClass, punit0), // unchanged
	}

	changed := changedDRAClasses(oldDevices, newDevices)
	if len(changed) != 1 || changed[0] != hpClass {
		t.Errorf("changedDRAClasses() = %v, want [%q]", changed, hpClass)
	}
}

// TestChangedDRAClassesDeviceAddedOrRemoved verifies that a class gaining or
// losing a device between snapshots counts as "changed" for that class.
func TestChangedDRAClassesDeviceAddedOrRemoved(t *testing.T) {
	className := "hp"
	dev := func(name string) resourceapi.Device {
		return resourceapi.Device{
			Name: name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"nri/cpuClass": {StringValue: &className},
			},
		}
	}

	oldDevices := []resourceapi.Device{dev("hp-pkg0-punit0")}
	newDevices := []resourceapi.Device{} // class removed entirely

	changed := changedDRAClasses(oldDevices, newDevices)
	if len(changed) != 1 || changed[0] != className {
		t.Errorf("changedDRAClasses() with a removed class = %v, want [%q]", changed, className)
	}

	// And the reverse: a brand new class appearing.
	changed = changedDRAClasses(newDevices, oldDevices)
	if len(changed) != 1 || changed[0] != className {
		t.Errorf("changedDRAClasses() with an added class = %v, want [%q]", changed, className)
	}
}
