# DRA Step 6: Wire pkg/resmgr/dra/ to PublishResources + Prepare/Unprepare stubs

## Overview

Implement the kubelet-plugin layer in `pkg/resmgr/dra/`: register with the kubelet via
`k8s.io/dynamic-resource-allocation/kubeletplugin`, publish `ResourceSlice` objects, and add
`PrepareResourceClaims`/`UnprepareResourceClaims` stubs that return `errNotImplemented`.

This step turns the empty `Plugin` struct (Step 2 skeleton) into a real DRA kubelet plugin that
can register and publish devices. Step 7 fills in the actual allocation logic.

## Context (from discovery)

- **Files involved:** `pkg/resmgr/dra/plugin.go`, `pkg/resmgr/dra/deps.go`, `pkg/resmgr/dra/logging.go` (new), `pkg/resmgr/dra/plugin_test.go`, `go.mod`/`go.sum`
- **Current state:** `New()` returns `errNotImplemented`; `Deps{}` is empty; `plugin_test.go` has empty-plugin test + import-boundary guard
- **New dep needed:** `k8s.io/dynamic-resource-allocation` (not yet in go.mod) — must-have per plan.md Step 6 imports list
- **Logger bridge:** `kubeletplugin.Start` expects a `logr.Logger` injected into context; nri-plugins uses `pkg/log/`; `log.Logger` exposes `SlogHandler() slog.Handler` and `logr` v1.4.3 provides `logr.FromSlogHandler` — use that, do not hand-roll a `logr.LogSink`
- **context injection:** use `logr.NewContext(ctx, logger)` not `klog.NewContext` — plan.md lists klog under "explicitly not adopted"
- **DRAPlugin interface:** `kubeletplugin.DRAPlugin` has three methods: `PrepareResourceClaims`, `UnprepareResourceClaims`, `HandleError` — not `NodeServer`/`NodePrepareResources`
- **Publishing mechanism:** `Helper.PublishResources` drives a `resourceslice.Controller` that writes ResourceSlice objects to the API server via `KubeClient`; it is NOT a kubelet-socket call
- **Related functions:** `ValidateCPUClassesForDRA` (Step 3, `pkg/resmgr/cpuclass/dra.go:62`) is a package function `(classes, sharedCounters) error`; `Handler.DRADevices` (Step 5) produces the device list

## Development Approach

- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests**
- **CRITICAL: all tests must pass before starting next task**
- Strict dependency discipline: import only the packages listed in plan.md Step 6 "Imports & deps"; `tags.cncf.io/container-device-interface` is Step 7 — do not add it here
- New files must carry the Apache-2.0 license header (match `doc.go` as the template)

## Testing Strategy

- **Unit tests:** every task has unit or compilation tests
- **Integration test (Task 6):** start plugin against a `fake.NewClientset(&corev1.Node{...})`; call `Start` and `PublishResources`; assert the fake clientset received ResourceSlice Create calls (poll or reactor); both socket dirs under `t.TempDir()`

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix

## Solution Overview

1. Add `k8s.io/dynamic-resource-allocation` to `go.mod`, run `go mod tidy`, verify licenses
2. Add a thin `logr.Logger` helper in `logging.go` using `logr.FromSlogHandler(logger.SlogHandler())`
3. Flesh out `Deps` with all fields `kubeletplugin.Start` requires (KubeClient, NodeName, dir overrides, ValidateClasses closure, DeviceLister, Logger)
4. Add `DRAPlugin` interface methods (`PrepareResourceClaims`, `UnprepareResourceClaims`, `HandleError`) — these must exist before `Start` can be called
5. Implement `Plugin.New` (validate deps), `Plugin.Start`, `Plugin.Stop`, `Plugin.PublishResources`
6. Integration test with fake clientset
7. Verify acceptance criteria + lint

## Technical Details

- **`DRAPlugin` interface** (kubeletplugin package, v0.36.3):
  - `PrepareResourceClaims(ctx, claims []*resourceapi.ResourceClaim) (map[types.UID]PrepareResult, error)`
  - `UnprepareResourceClaims(ctx, claims []NamespacedObject) (map[types.UID]error, error)`
  - `HandleError(ctx, err error, msg string)` — called by helper for background/publish errors; distinguish `errors.Is(err, kubeletplugin.ErrRecoverable)` for log-only vs fatal
- **`Plugin.Start(ctx context.Context) error`**:
  - call `deps.ValidateClasses()`; return on error
  - `os.MkdirAll` the plugin data dir (from `Deps.PluginDataDir`, default `kubeletplugin.KubeletPluginsDir+"/"+driverName`)
  - inject `logr.Logger` into ctx via `logr.NewContext(ctx, newLogr(deps.Logger))`
  - call `kubeletplugin.Start(ctx, p, kubeletplugin.KubeClient(deps.KubeClient), kubeletplugin.NodeName(deps.NodeName), kubeletplugin.RegistrarDirectoryPath(deps.RegistrarDir), kubeletplugin.PluginDataDirectoryPath(deps.PluginDataDir), kubeletplugin.GRPCVerbosity(-1))`; store Helper
- **`Plugin.PublishResources(ctx context.Context) error`**:
  - guard: return error if `p.helper == nil`
  - call `deps.ValidateClasses()` (re-validate on every publish, since classes may change on Reconfigure)
  - call `deps.DeviceLister.DRADevices(p.driverName)` to get device list
  - paginate into `resourceslice.Pool.Slices` under a single pool (node name); at most `resapi.ResourceSliceMaxDevices` devices per slice
  - call `p.helper.PublishResources(ctx, resources)`
- **`Plugin.Stop()`**: call `p.helper.Stop()` if non-nil; idempotent
- **Pool layout:** one pool (name = node name), N slices — idiomatic and stable; pool membership does not change as device count changes across Reconfigure
- **Stubs:** `PrepareResourceClaims` and `UnprepareResourceClaims` return `nil, errNotImplemented` (reuses the existing sentinel; no grpc/status import needed at this step)
- **`HandleError`**: log recoverable errors at Warn level, fatal errors at Error level via `pkg/log/`
- **`logging.go`**: `newLogr(l log.Logger) logr.Logger` = `logr.FromSlogHandler(l.SlogHandler())`
- **`Deps`**:
  - `KubeClient kubernetes.Interface` — injected by policy binary; enables fake clientset in tests
  - `NodeName string`
  - `RegistrarDir string` — defaults to `kubeletplugin.KubeletRegistryDir`; overridable for tests
  - `PluginDataDir string` — defaults to `kubeletplugin.KubeletPluginsDir+"/"+driverName`; overridable for tests
  - `ValidateClasses func() error` — closure (bound at construction) over `ValidateCPUClassesForDRA`
  - `DeviceLister` interface: `DRADevices(driverName string) ([]resapi.Device, error)`
  - `Logger log.Logger`
- **`New` validation:** reject empty `driverName`, nil `KubeClient`, empty `NodeName`, nil `ValidateClasses`, nil `DeviceLister`, nil `Logger`

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): all code changes and tests below
- **Post-Completion** (no checkboxes): items that require Step 8 or Step 9 to be meaningful

## Implementation Steps

### Task 1: Add k8s.io/dynamic-resource-allocation to go.mod

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

- [x] run `go get k8s.io/dynamic-resource-allocation@v0.36.3`
- [x] run `go mod tidy`
- [x] review the `go.mod`/`go.sum` diff: confirm no unexpected direct deps added
- [x] run `make verify-godeps` — must pass (passes after commit)
- [x] run `make verify-licenses` — skipped: go-licenses tool not installed in this environment
- [x] run `go build ./pkg/resmgr/dra/...` — must compile cleanly
- [x] run `go test ./pkg/resmgr/dra/...` — existing tests must pass

### Task 2: Add logr.Logger bridge (logging.go)

**Files:**
- Create: `pkg/resmgr/dra/logging.go`
- Modify: `pkg/resmgr/dra/plugin_test.go`

- [x] create `pkg/resmgr/dra/logging.go` with Apache-2.0 header
  - `func newLogr(l log.Logger) logr.Logger { return logr.FromSlogHandler(l.SlogHandler()) }`
  - no custom `LogSink` implementation needed
- [x] write `TestNewLogr` — verify `newLogr(log.Default())` returns a non-zero `logr.Logger` and calling `.Info("test")` does not panic
- [x] run `go test ./pkg/resmgr/dra/...` — must pass

### Task 3: Implement DRAPlugin interface methods (stubs)

**Files:**
- Modify: `pkg/resmgr/dra/plugin.go`

*These must be added before `Start` so `*Plugin` satisfies `kubeletplugin.DRAPlugin`.*

- [x] implement `PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error)`:
  - `return nil, errNotImplemented`
- [x] implement `UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error)`:
  - `return nil, errNotImplemented`
- [x] implement `HandleError(ctx context.Context, err error, msg string)`:
  - if `errors.Is(err, kubeletplugin.ErrRecoverable)` → log at Warn level; otherwise log at Error level
- [x] write unit tests:
  - `TestPrepareResourceClaims_Stub` — verify returns `errNotImplemented`
  - `TestUnprepareResourceClaims_Stub` — verify returns `errNotImplemented`
  - `TestHandleError_RecoverableLogsWarn` — verify recoverable error does not panic (log-only)
  - `TestHandleError_FatalLogsError` — verify fatal error does not panic (log-only)
- [x] run `go test ./pkg/resmgr/dra/...` — must pass

### Task 4: Flesh out Deps and Plugin struct; implement New

**Files:**
- Modify: `pkg/resmgr/dra/deps.go`
- Modify: `pkg/resmgr/dra/plugin.go`

- [x] add to `Deps` (replacing the empty struct from Step 2):
  - `KubeClient kubernetes.Interface`
  - `NodeName string`
  - `RegistrarDir string`
  - `PluginDataDir string`
  - `ValidateClasses func() error`
  - `DeviceLister` interface with `DRADevices(driverName string) ([]resapi.Device, error)`
  - `Logger log.Logger`
- [x] add to `Plugin` struct: `driverName string`, `deps Deps`, `helper kubeletplugin.Helper`
- [x] implement `New(driverName string, deps Deps) (*Plugin, error)`:
  - validate: non-empty `driverName`, non-nil `KubeClient`, non-empty `NodeName`, non-nil `ValidateClasses`, non-nil `DeviceLister`, non-nil `Logger`
  - return `&Plugin{driverName: driverName, deps: deps}, nil`
  - remove `errNotImplemented` as the return value of `New` (but keep `var errNotImplemented` — it is used by stubs)
- [x] update `TestNew_ReturnsNotImplemented` → replace with `TestNew_Succeeds` (valid deps → no error) and `TestNew_Validation` (table test: missing each required dep → error)
- [x] run `go test ./pkg/resmgr/dra/...` — must pass

### Task 5: Implement Plugin.Start, Stop, PublishResources

**Files:**
- Modify: `pkg/resmgr/dra/plugin.go`

- [x] implement `Plugin.Start(ctx context.Context) error`:
  - call `p.deps.ValidateClasses()`; return wrapped error on failure
  - `os.MkdirAll(p.deps.PluginDataDir, 0750)`
  - `ctx = logr.NewContext(ctx, newLogr(p.deps.Logger))`
  - call `kubeletplugin.Start(ctx, p, kubeletplugin.KubeClient(p.deps.KubeClient), kubeletplugin.NodeName(p.deps.NodeName), kubeletplugin.RegistrarDirectoryPath(p.deps.RegistrarDir), kubeletplugin.PluginDataDirectoryPath(p.deps.PluginDataDir), kubeletplugin.GRPCVerbosity(-1))`; store result in `p.helper`
- [x] implement `Plugin.Stop()`:
  - if `p.helper != nil` → `p.helper.Stop()`; set `p.helper = nil` (idempotent)
- [x] implement `Plugin.PublishResources(ctx context.Context) error`:
  - guard: if `p.helper == nil` → return clear error
  - call `p.deps.ValidateClasses()`; return on error
  - call `p.deps.DeviceLister.DRADevices(p.driverName)`
  - paginate into `resourceslice.Pool.Slices` under one pool named `p.deps.NodeName`; at most `resapi.ResourceSliceMaxDevices` devices per slice; publish even one empty slice when device count is zero
  - call `p.helper.PublishResources(ctx, resourceslice.DriverResources{Pools: []resourceslice.Pool{pool}})`
- [x] write unit tests:
  - `TestPublishResources_NilHelper` — returns error before `Start`
  - `TestPublishResources_ValidationError` — ValidateClasses error propagated
  - `TestPublishResources_Pagination_Zero` — zero devices → one slice with zero devices
  - `TestPublishResources_Pagination_ExactMax` — exactly `ResourceSliceMaxDevices` devices → one slice
  - `TestPublishResources_Pagination_OverMax` — `ResourceSliceMaxDevices+1` devices → two slices, same pool
  - `TestStop_Idempotent` — calling `Stop()` twice does not panic
  - `TestStart_ValidateClassesError` — returns error if ValidateClasses fails (before calling kubeletplugin.Start)
- [x] run `go test ./pkg/resmgr/dra/...` — must pass

### Task 6: Integration test — publish against fake clientset

**Files:**
- Modify: `pkg/resmgr/dra/plugin_test.go`

- [x] add `TestPublishResources_Integration`:
  - create two temp dirs: `registrarDir := t.TempDir()`, `pluginDataDir := t.TempDir()`
  - build a `fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}})` as `KubeClient`
  - install a `CreateAction` reactor on the fake clientset to capture ResourceSlice creates
  - construct `Plugin` via `New` with a `DeviceLister` returning 5 test devices and `ValidateClasses` returning nil
  - call `p.Start(ctx)` (short-lived ctx is fine: it is only used for the `MkdirAll` + `kubeletplugin.Start` call; helper manages its own context after that)
  - call `p.PublishResources(ctx)`
  - poll the captured creates (with `time.AfterFunc` deadline) until at least one ResourceSlice is observed in the fake clientset
  - assert the ResourceSlice's `spec.driverName` equals the test driver name
  - call `p.Stop()` to clean up
- [x] run `go test -v -run TestPublishResources_Integration ./pkg/resmgr/dra/...` — must pass
- [x] run `go test ./pkg/resmgr/dra/...` — all tests pass

### Task 7: Feature-gate probes — explicit deferral

**Files:**
- Modify: `docs/dra/plan.md`

- [x] add a ⚠️ note under Step 6 in `docs/dra/plan.md` Cross-cutting: "Feature-gate probes (`AllowMultipleAllocations`, `NodeAllocatableResources`) deferred from Step 6 to a follow-up task; add before Step 8 lands"
- [x] note that without the probe, DRA-published capacity fields that require `NodeAllocatableResources` will be silently stripped by the API server — acceptable for the stub phase
- [x] no code changes in this task

### Task 8: Verify acceptance criteria

- [x] run `go test ./...` — full test suite passes
- [x] run `go build ./cmd/plugins/topology-aware/...` — no compile regressions
- [x] run `make golangci-lint` — clean
- [x] verify `TestNoCmdPluginsImport` still passes (import-boundary guard)
- [x] verify `p.helper` nil-guard: call `PublishResources` before `Start` → returns error, not panic
- [x] verify `Stop` idempotent: double-`Stop` does not panic

### Task 9: [Final] Update documentation

- [x] update `docs/dra/plan.md` Step 6 "Landed:" with commit hash and deviations from spec:
  - `Deps` uses `ValidateClasses func() error` closure (not `ClaimAllocator`/`CDIWriter` — those are Step 7)
  - pool layout: one pool (node name), N slices (not pool0..poolN as original plan.md stated)
  - logger bridge: `logr.FromSlogHandler` (not hand-rolled `LogSink`)
  - feature-gate probes deferred (see Cross-cutting note)
- [x] update `docs/dra/design.md` if any Deps interface shape differs from the design (no change needed — design.md does not specify Deps fields; DeviceLister shape matches implementation)
- [x] correct `docs/dra/plan.md` Step 5 reference from `cpuclass.Manager.DRADevices()` to `Handler.DRADevices()` (landed method name)
- [x] move this plan to `docs/plans/completed/` (harness handles move)

## Post-Completion

**Feature-gate probes (must land before Step 8):**
- Probe `AllowMultipleAllocations` and `NodeAllocatableResources` at `Plugin.Start`; warn or refuse if the cluster lacks them
- This is deliberately deferred from Step 6 to reduce PR surface; it is not optional for Step 8

**Manual smoke test (requires Step 8 + Step 9):**
- Deploy topology-aware plugin with DRA enabled; verify ResourceSlices appear via `kubectl get resourceslices` and the DRA driver socket appears under `/var/lib/kubelet/plugins_registry/`

**`ClaimAllocator` / `CDIWriter` deps (Step 7):**
- Step 7 adds these to `Deps` when implementing real Prepare/Unprepare logic
