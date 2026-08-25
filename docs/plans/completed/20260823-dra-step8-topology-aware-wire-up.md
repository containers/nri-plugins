# DRA Step 8: Topology-Aware Policy Wire-Up

## Overview

Wire the DRA kubelet plugin into the topology-aware policy binary. When `spec.dra.enabled: true`
the plugin starts, publishes CPU devices derived from the current `cpuClass` configuration, and
integrates with the policy's pool accounting so DRA-claimed CPUs are excluded from regular
exclusive grants. Reconfigure attempts that would change the DRA-visible attributes of a CPU class
with live claims are refused (Option B from resolved decision 8).

Companion to [design.md](../dra/design.md) and [plan.md](../dra/plan.md) Step 8.

## Context (from discovery)

- **DRA plugin**: `pkg/resmgr/dra/`; `dra.New(driverName, deps)`. `Deps` in `deps.go` needs
  `ClaimAllocator`, `CDIWriter`, `ClaimStore`, `WithLock`, `DeviceLister`, `ValidateClasses`,
  `KubeClient`, `NodeName`, `Logger`. `LiveClaimClasses()` and `RestoreClaimsLocked()` exist.
- **Handler staleness**: `initialize()` sets `p.cpuClasses = nil` and creates a new
  `*cpuclass.Handler` on every `Reconfigure`. A direct pointer in `Deps` would silently use stale
  state. A `*policy`-backed adapter forwarding to `p.cpuClasses` at call time is required.
- **CDI writer**: `dra.NewCDIWriter(driverName, cdiDir string)` (`cdi.go:57`) — no `cdi.New()`.
- **ValidateCPUClassesForDRA**: `cpuclass.ValidateCPUClassesForDRA(classes, sharedCounters bool)`
  (`dra.go:62`) — package-level function, not a Handler method.
- **KubeClient typed-nil trap**: `agent.KubeClient()` returns `*client.Client`
  (`agent/agent.go:379`). Wrapping it in `func() kubernetes.Interface { return m.agent.KubeClient() }`
  yields a **non-nil interface wrapping a nil pointer** in local-config mode — the `deps.KubeClient == nil`
  guard in `plugin.go:66` never fires. Fix: wrap as `func() kubernetes.Interface { c := m.agent.KubeClient(); if c == nil { return nil }; return c }`.
  Same issue for NodeName: can be empty in local-config mode; `dra.New` checks `NodeName == ""`
  (`plugin.go:69`). The Setup()-time nil check must use the accessor return value, not interface nilness.
- **KubeClient timing**: `agent.KubeClient()` returns nil until `setupClients()` runs inside the
  first `notifyFn` callback. The notify callback is invoked during `agent.Start()` before it blocks;
  by the time `m.start()` → `policy.Start()` → `backend.Setup()` runs, `setupClients()` has
  completed. Storing the value at `Setup()` time is therefore safe. An accessor closure is still
  preferred to make the nil-check explicit.
- **WithLock**: resmgr write-lock closure needed by DRA `Deps`. Add `WithLock func(func())` to
  `policy.Options`/`BackendOptions`; wire from `resmgr.setupPolicy()` as `m.withWriteLock`.
  `withWriteLock` must not be called while the resmgr lock is held (non-reentrant).
- **`Backend` has no `Stop()`** and `resmgr.Stop()` has no caller — shutdown relies on
  `m.agt.Stop()` which makes `mgr.Start()` return, then `Run()` exits. Fix: (a) add
  `Stop() error` to `Backend` interface; (b) call `p.active.Stop()` in `resmgr.Stop()` **before**
  acquiring `m.Lock()` (in-flight `Prepare` holds `deps.WithLock` = `m.Lock()`; calling Stop under
  the lock deadlocks); (c) add `m.mgr.Stop()` in `Main.Run()` after `mgr.Start()` returns.
- **Claim UID at NRI call site**: CDI device names embed `claim-<uid>-...`
  (`cdi.go` `cdiDeviceName`). NRI `api.Container` carries `CDIDevices []*CDIDevice` (field 20).
  Add `GetCDIDeviceNames() []string` to `cache.Container`; parse UID from names matching
  `"nri.topology-aware.cpu/device=claim-<uid>-..."`. This is independent of CDI container-edit
  ordering and eliminates the env-var-visibility risk.
- **Plugin claims accessor**: `Plugin.ClaimState` is a struct type, not a method. `reapplyDRAClaims()`
  needs a read-only `LiveClaimsLocked() map[types.UID][]ResultAlloc` on `*Plugin` — lock-held
  convention (like `RestoreClaimsLocked`), no `deps.WithLock` call inside. Must be called while
  holding the resmgr write lock.
- **Reconfigure refusal attribute diff**: `buildDRADevices` is unexported. Use the exported
  `p.cpuClasses.DRADevices(DRADriverName)` to snapshot per-class device attributes **before**
  `initialize()` (using `p.cfg`) and **after** (using `newCfg` via the newly created handler).
  Compare per-class attribute maps; if any differ and `LiveClaimClasses()[className] > 0`, refuse.
- **resmgr restructuring**: unnecessary. `updateConfig` (`resource-manager.go:163-166`) calls
  `updateTopologyZones()`/`updateNodeExtendedResources()` *after* `m.reconfigure()` returns and
  the lock is released. Use this existing post-unlock seam to call `policy.PostReconfigure(ctx)`.
- **Pool supply claim marking**: must be tree-wide (tightest pool + all ancestors), mirroring how
  `AccountAllocateCPU`/`AccountReleaseCPU` work. A `claimed` field that nothing reads has zero
  effect; subtract claimed CPUs from `isolated`/`sharable` at each affected pool level. `Clone()`
  and `Cumulate()` must carry the claimed set. Use a per-UID refcount (`claimRefs map[types.UID]cpuset.CPUSet`) so a multi-container ResourceClaim (`AllowMultipleAllocations`) is safe.
- **`opt` global + full rollback**: after a refused Reconfigure, restore using the existing idiom:
  `*p = savedPolicy; opt = p.cfg; defaultPrio = p.cfg.DefaultCPUPriority.Value()` — not just
  `opt = p.cfg`. The `*p = savedPolicy` is essential because the deferred `commitCpuClasses` runs
  against `p`; without it, new pool/class objects remain in `p` and get committed to hardware.
  See `topology-aware-policy.go:546-549`.
- **DRA enable/disable flip across Reconfigure**: refuse in `Reconfigure()` only. `Setup()` builds
  the plugin once when `cfg.DRAEnabled()` is true. A subsequent `Reconfigure` that changes
  `DRAEnabled()` is refused with a clear error (same Option B philosophy). Do NOT add this check to
  `Setup()` — `p.cfg` is nil at initial `Setup()` time (policy is zero-valued at construction),
  making the "detect Reconfigure by checking p.cfg" heuristic unreachable dead code.
  Add `DRASharedCounters() bool` nil-safe getter alongside `DRAEnabled()`.
- **`ValidateClasses` nil guard**: the closure calls `p.cfg.DRA.SharedCounters`; if a Reconfigure
  removes the `dra:` section, `p.cfg.DRA` becomes nil → panic inside `WithLock`. Use the
  `DRASharedCounters()` getter.
- **Plan.md deviations**: (a) `AllocateClaim`/`ReleaseClaim` NOT added to `Backend` interface —
  they are unexported TA policy methods called only from `AllocateResources`/`ReleaseResources`
  (YAGNI; no-op stubs in every backend avoided); (b) `Deps` closure path dropped — pool eviction
  requires the NRI handler to call `updateContainers`; a `Prepare`-path closure cannot do that.
- Related: [plan.md Step 8](../dra/plan.md#step-8----topology-aware-policy-wire-up)

## Development Approach

- **Testing approach**: TDD — write failing tests first, then implement.
- Complete each task fully before moving to the next.
- **Every task MUST include new/updated tests.**
- All tests must pass before starting the next task.
- Update this plan when scope changes.
- `make generate` regenerates CRDs; `make verify-generate` confirms.
- `const DRADriverName = "nri.topology-aware.cpu"` — defined once (Task 4), referenced everywhere.

## Testing Strategy

- **Unit tests**: each task has its own (listed inline).
- **Lock-contract test**: a `WithLock` stub that panics on re-entrancy — asserts `Start`,
  `PublishResources`, `RestoreClaims`, `PostReconfigure` are never invoked while the stub is held.
- **Integration**: Task 9 reuses the fake-kubelet harness from `plugin_test.go`; inject test-safe
  `PluginDataDir`, `RegistrarDir`, `cdiDir`.
- **No e2e** in this step — that is step 10.

## Progress Tracking

Mark completed `[x]` immediately. ➕ newly discovered. ⚠️ blockers.

## Solution Overview

**Data flow:**
1. `Setup()` — if `cfg.DRAEnabled()` and kube client available: build `*policyDRAAdapter`,
   construct `dra.Plugin` with adapter as both `ClaimAllocator` and `DeviceLister`; refuse any
   DRAEnabled change in subsequent `Reconfigure`.
2. `Start()` — call `draPlugin.Start(ctx)` then `draPlugin.PublishResources(ctx)` (outside lock).
3. NRI `AllocateResources(c)` — if container has TA CDI devices (detected via `GetCDIDeviceNames`),
   parse claim UID + CPUs, call `allocateClaim(uid, cpus, className)`.
4. NRI `ReleaseResources(c)` — if container has TA CDI devices, call `releaseClaim(uid, cpus)`.
5. `Reconfigure()` — pre: refuse DRA-attr change with live claims; commit; post-locked:
   `draPlugin.RestoreClaimsLocked()` + `reapplyDRAClaims()`; post-unlock (via
   `policy.PostReconfigure(ctx)`): `draPlugin.PublishResources(ctx)`.
6. `Stop()` — cancel DRA context; call `draPlugin.Stop()`.

**Pool accounting:** `Supply` gains a per-UID refcount map. `allocateClaim` subtracts CPUs from
`isolated`/`sharable` in the tightest pool and all ancestors (tree-wide); `releaseClaim` adds them
back when the last container using the claim is released. `Clone()` carries the refcount map.
`reapplyDRAClaims()` re-marks pool supplies after any `initialize()` call by calling a
**marking-only** path (`remarkClaimInSupply`) that does **not** touch `claimContainerRefs` — using
`allocateClaim` for re-apply would inflate refcounts since containers were already counted at
`AllocateResources` time.

## Technical Details

### Config CR addition

```go
type TopologyAwareDRA struct {
    // +kubebuilder:default=false
    Enabled bool `json:"enabled,omitempty"`
    // +kubebuilder:default=false
    SharedCounters bool `json:"sharedCounters,omitempty"`
}
```
`Config.DRA *TopologyAwareDRA` (pointer; nil = disabled).
Getters: `DRAEnabled() bool`, `DRASharedCounters() bool` — both nil-safe.

### KubeClient accessor

```go
// in policy.Options / BackendOptions
KubeClientFn func() kubernetes.Interface
```

Wrapper (in resmgr.setupPolicy):
```go
KubeClientFn: func() kubernetes.Interface {
    c := m.agent.KubeClient()
    if c == nil { return nil }
    return c
},
```
In `Setup()`, call `KubeClientFn()` and check `if c == nil` — the wrapper already returns an
untyped nil when the agent has no kube client, so a plain nil check is sufficient.

### Supply claim refcount

```go
claimRefs map[types.UID]cpuset.CPUSet // per-claim CPUs subtracted from this supply
```
`ClaimCPUs(uid, cpus)` — add to `claimRefs`, subtract from `isolated`+`sharable` tree-wide.
`UnclaimCPUs(uid)` — remove from `claimRefs`, add back to `isolated`+`sharable` tree-wide.
Idempotent: a second `ClaimCPUs` for the same UID replaces the entry (only possible in re-apply).
Carried through `Clone()`/`Cumulate()`.

### Claim identification

Parse CDI device names of the form `"nri.topology-aware.cpu/device=claim-<uid>-..."`:
`GetCDIDeviceNames()` returns the raw strings; a helper extracts `uid` from the prefix.
Look up CPUs + className from `draPlugin.LiveClaimsLocked()[uid].Allocs`.

### Reconfigure refusal

Snapshot `oldDevices, _ := p.cpuClasses.DRADevices(DRADriverName)` before calling `initialize()`
on `newCfg`. After the new cpuClasses handler is configured, snapshot `newDevices`. Compare device
attribute maps per class name. For each class where attrs differ: `if p.draPlugin.LiveClaimClasses()[class] > 0` → refuse.

## What Goes Where

**Implementation Steps** — all code changes and tests.
**Post-Completion** — items requiring external infrastructure (cluster, Helm step 9).

## Implementation Steps

### Task 1: Config CR extension + CRD regeneration

**Files:**
- Modify: `pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/config.go`
- Modify: `pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/zz_generated.deepcopy.go`
- Modify: `config/crd/bases/config.nri_topologyawarepolicies.yaml`
- Modify: `deployment/helm/topology-aware/crds/config.nri_topologyawarepolicies.yaml`

- [x] write test: zero `Config` (nil `DRA`) → `DRAEnabled()` returns false; `DRASharedCounters()` returns false
- [x] write test: `Config{DRA: &TopologyAwareDRA{Enabled: true}}` → `DRAEnabled()` true; `Validate()` passes
- [x] write test: `Config{DRA: &TopologyAwareDRA{SharedCounters: true}}` → `DRASharedCounters()` true; `Validate()` passes
- [x] add `TopologyAwareDRA` struct with `Enabled bool`, `SharedCounters bool` (+kubebuilder default:false tags)
- [x] add `DRA *TopologyAwareDRA` field to `Config`
- [x] add nil-safe getters `DRAEnabled() bool` and `DRASharedCounters() bool`
- [x] run `make generate`; verify `make verify-generate` passes (verified idempotent; git diff is nonzero only because source changes are not yet committed — see decision log)
- [x] run tests — must pass before task 2

### Task 2: `BackendOptions` / `policy.Options` / `Backend` interface + shutdown wiring

**Files:**
- Modify: `pkg/resmgr/policy/policy.go`
- Modify: `pkg/resmgr/resource-manager.go`
- Modify: `pkg/resmgr/main/main.go`
- Modify: balloons + template policy backends (no-op `Stop() error`)

- [x] write test: `Backend.Stop()` is called from `resmgr.Stop()` and is reachable from
      `Main.Run()` shutdown path (use mock backend that records calls)
- [x] write test: `resmgr.Stop()` calls `p.active.Stop()` **before** acquiring the lock;
      verify with a backend stub that asserts it is not called inside a `withWriteLock` callback
- [x] write test: `resmgr.Stop()` when `Start()` was never called (nil `m.policy`) → no panic
- [x] write test: `BackendOptions.KubeClientFn()` evaluated at `Setup()` time returns nil when
      the agent has no kube client; `Setup()` must see the nil **as a nil interface** (not
      typed-nil-wrapped-in-interface)
- [x] write test (lock-contract): a `WithLock` stub that panics on re-entrancy; `Start()` and
      `PublishResources()` must not be called while the stub is held
- [x] add `KubeClientFn func() kubernetes.Interface`, `NodeName string`, `WithLock func(func())`
      to `policy.Options`; forward them to `BackendOptions` in `policy.Start()`
- [x] add same fields to `BackendOptions`
- [x] add `(m *resmgr) withWriteLock(f func())` method; wire into `setupPolicy()` as
      `WithLock: m.withWriteLock`; wire `KubeClientFn` with the typed-nil-safe wrapper;
      wire `NodeName: m.agent.NodeName()`
- [x] add `Stop() error` to `Backend` interface and `Policy` interface; implement in
      `policy.go` wrapper via `p.active.Stop()`; add no-op impls to balloons + template
- [x] in `resmgr.Stop()`, call `m.policy.Stop()` **before** `m.Lock()` (if `m.policy != nil`)
- [x] in `Main.Run()`, call `m.mgr.Stop()` after `m.mgr.Start()` returns (deferred or explicit)
- [x] run tests — must pass before task 3

### Task 3: `cache.Container` accessors + mock update

**Files:**
- Modify: `pkg/resmgr/cache/cache.go` (Container interface)
- Modify: `pkg/resmgr/cache/container.go` (implementation)
- Modify: `pkg/resmgr/cache/cache_test.go` (or new test file)
- Modify: `cmd/plugins/topology-aware/policy/mocks_test.go` (add stubs for new methods)

- [x] write test: container with `CDIDeviceIDs = ["nri.topology-aware.cpu/device=claim-abc-req-dev-0"]`
      → `GetCDIDeviceNames()` returns that list
- [x] write test: container with no CDI devices → `GetCDIDeviceNames()` returns nil/empty
- [x] add `GetCDIDeviceNames() []string` to `cache.Container` interface + impl
- [x] add no-op stub for the new method to `mocks_test.go` mock
- [x] run tests — must pass before task 4

### Task 4: Driver name constant + `*policy`-backed adapter + `Plugin.LiveClaimsLocked()`

**Files:**
- Create: `cmd/plugins/topology-aware/policy/dra_adapter.go`
- Create: `cmd/plugins/topology-aware/policy/dra_adapter_test.go`
- Modify: `pkg/resmgr/dra/plugin.go`

- [x] write test: adapter's `PickHpCpus`/`DRADevices`/`IsHPClass` route to the **current**
      `p.cpuClasses` after a handler replacement (simulate by swapping the field)
- [x] write test: adapter methods with nil `p.cpuClasses` return safe zero-values / errors (no panic)
- [x] write test: `Plugin.LiveClaimsLocked()` returns the correct
      `map[types.UID][]ResultAlloc` snapshot while the caller holds the resmgr lock (use the
      fake-lock stub from Task 2)
- [x] define `const DRADriverName = "nri.topology-aware.cpu"` (in `dra_adapter.go` or a constants file)
- [x] implement unexported `policyDRAAdapter` struct holding `*policy`; implement `dra.ClaimAllocator`
      and `dra.DeviceLister` by forwarding to `p.cpuClasses` at call time
- [x] add compile checks: `var _ dra.ClaimAllocator = &policyDRAAdapter{}`
      and `var _ dra.DeviceLister = &policyDRAAdapter{}`
- [x] add `LiveClaimsLocked() map[types.UID][]ResultAlloc` to `*Plugin` in `plugin.go`; it
      reads `p.claims` directly (no `deps.WithLock`); doc: "caller must hold the resmgr lock"
- [x] run tests — must pass before task 5

### Task 5: Supply claim refcount + tree-wide marking

**Files:**
- Modify: `cmd/plugins/topology-aware/policy/resources.go`
- Modify: `cmd/plugins/topology-aware/policy/pools_test.go` (or new supply test file)

- [x] write test: `ClaimCPUs(uid, cpus)` on pool X subtracts `cpus` from `AllocatableCPUs()`
      in pool X **and in all ancestor pools**; `AllocateCPUs()` for another container cannot pick them
- [x] write test: two `ClaimCPUs` for the same UID replace (not stack) the entry (idempotent for re-apply)
- [x] write test: `UnclaimCPUs(uid)` restores CPUs in pool X and ancestors
- [x] write test: `UnclaimCPUs` for unknown UID is a no-op (no panic)
- [x] write test: `Clone()` carries the `claimRefs` map into the clone
- [x] write test: ancestor claim marks are not double-subtracted when two child pools share an ancestor
- [x] add `claimRefs map[types.UID]cpuset.CPUSet` field to the concrete supply struct
- [x] implement `ClaimCPUs(uid types.UID, cpus cpuset.CPUSet)` — subtract from `isolated`/`sharable`
      tree-wide (walk up `node.Parent()` chain); store in `claimRefs`
- [x] implement `UnclaimCPUs(uid types.UID)` — reverse; walk same path
- [x] update `Clone()` to copy `claimRefs` (Cumulate is dead-code path — no change needed)
- [x] verify all existing allocation read paths (`GetCPUOffer`, `GetScore`, `Reserve`, `Allocate`,
      `AllocatableSharedCPU`, `SliceableCPUs`) use the subtracted `isolated`/`sharable` fields
      (no change needed if they already read those fields)
- [x] run tests — must pass before task 6

### Task 6: Claim identification + `allocateClaim`/`releaseClaim` + eviction

**Files:**
- Modify: `cmd/plugins/topology-aware/policy/pools.go`
- Modify: `cmd/plugins/topology-aware/policy/pools_test.go`

- [x] write test: `parseCDIClaimUID("nri.topology-aware.cpu/device=claim-abc123-req-dev-0")` → `("abc123", true)`
- [x] write test: CDI device name not matching the driver prefix → `("", false)`
- [x] write test: container with a TA CDI device → `claimCPUsFromContainer(c, plugin)` returns
      the correct `(uid, cpus, className)` from `LiveClaimsLocked()`
- [x] write test: container without TA CDI devices → returns `("", empty, "", false)`
- [x] write test: `allocateClaim` marks CPUs in the tightest pool containing them; a subsequent
      `AllocateCPUs` call for another container cannot pick those CPUs
- [x] write test: `allocateClaim` evicts an exclusive grant overlapping the claimed CPUs; the
      displaced container is queued for reallocation
- [x] write test: `allocateClaim` for CPUs entirely outside `p.allowed` or spanning no pool →
      returns descriptive error
- [x] write test: `releaseClaim` restores CPUs; refcount — only releases when last referencing
      container is gone (two AllocateResources for same claim UID → two ReleaseResources needed)
- [x] write test: `releaseClaim` for unknown UID → nil (idempotent)
- [x] implement `parseCDIClaimUID(deviceName string) (uid string, ok bool)`
- [x] implement `claimCPUsFromContainer(c cache.Container, plugin *dra.Plugin) (uid types.UID, cpus cpuset.CPUSet, className string, ok bool)` — parse UID from `GetCDIDeviceNames()`, look up CPUs+class from `plugin.LiveClaimsLocked()[uid]`
      (implemented against a local `claimLister` interface that `*dra.Plugin` satisfies — see decision log)
- [x] implement `(p *policy) allocateClaim(uid types.UID, cpus cpuset.CPUSet, className string) error`
      — find tightest pool containing `cpus`, call `pool.GetSupply().ClaimCPUs(uid, cpus)`,
      evict + reallocate conflicting grants; maintain per-claim container refcount `p.claimContainerRefs map[types.UID]int`
- [x] implement `(p *policy) releaseClaim(uid types.UID, cpus cpuset.CPUSet)` — decrement
      refcount; call `pool.GetSupply().UnclaimCPUs(uid)` and restore grants only when refcount == 0
- [x] run tests — must pass before task 7

### Task 7: NRI call sites — AllocateResources / ReleaseResources

**Files:**
- Modify: `cmd/plugins/topology-aware/policy/topology-aware-policy.go`
- Modify: `cmd/plugins/topology-aware/policy/mocks_test.go` (if mock needs updating)

- [x] write test: `AllocateResources` on a container with a TA CDI device → `allocateClaim` called;
      pool supply updated
- [x] write test: `ReleaseResources` on that container → `releaseClaim` called; supply restored
- [x] write test: `AllocateResources` on a container with no TA CDI devices → no claim methods
      called; behavior identical to pre-step-8
- [x] write test: `AllocateResources` with a nil `draPlugin` (DRA disabled) → no crash
- [x] in `AllocateResources(c)`, call `claimCPUsFromContainer(c, p.draPlugin)` and if ok,
      call `p.allocateClaim(uid, cpus, className)` before regular pool allocation
- [x] in `ReleaseResources(c)`, call `claimCPUsFromContainer(c, p.draPlugin)` and if ok,
      call `p.releaseClaim(uid, cpus)`
- [x] run tests — must pass before task 8

### Task 8: Pool claim mark restoration after rebuild

**Files:**
- Modify: `cmd/plugins/topology-aware/policy/topology-aware-policy.go`

- [x] write test: `Start()` with live claims in ClaimStore → after `restoreCache()`,
      pool supplies have claim CPUs marked; a subsequent `AllocateCPUs` cannot use them
- [x] write test: `Reconfigure()` with a live claim → after `restoreAllocations()`,
      pool supplies re-marked; exclusive grants remain correct
- [x] write test: `reapplyDRAClaims()` with nil `draPlugin` → no-op (no panic)
- [x] write test: ordering — `draPlugin.Start(ctx)` must run before `reapplyDRAClaims()` in
      `Start()` (plugin loads ClaimStore; adapter reads live claims); assert with call-order mock
      (adapted — see decision log: `draPlugin.Start(ctx)` is not yet wired into `Start()`
      until Task 9, so this checks that `reapplyDRAClaims()` runs strictly after
      `restoreCache()` within `Start()` instead of a call-order mock on `draPlugin.Start`)
- [x] implement `(p *policy) remarkClaimInSupply(uid types.UID, cpus cpuset.CPUSet)` — find
      pool containing `cpus`, call `pool.GetSupply().ClaimCPUs(uid, cpus)` tree-wide; does NOT
      touch `p.claimContainerRefs` (marking only, no refcount side-effects)
- [x] implement `(p *policy) reapplyDRAClaims()` — if `draPlugin` is nil, return; call
      `draPlugin.LiveClaimsLocked()` (must be under resmgr lock — caller's responsibility);
      for each `(uid, allocs)`: parse cpus from allocs, call `p.remarkClaimInSupply(uid, cpus)`
      (not `allocateClaim` — re-apply must not inflate `claimContainerRefs`)
- [x] in `Start()`, call `reapplyDRAClaims()` after `restoreCache()` (after `draPlugin.Start(ctx)`)
- [x] in `Reconfigure()`, call `reapplyDRAClaims()` after `restoreAllocations()` succeeds, before
      returning (still inside the resmgr lock held by the caller)
- [x] run tests — must pass before task 9

### Task 9: DRA plugin lifecycle in topology-aware policy

**Files:**
- Modify: `cmd/plugins/topology-aware/policy/topology-aware-policy.go`
- Create: `cmd/plugins/topology-aware/policy/dra.go`
- Create: `cmd/plugins/topology-aware/policy/dra_test.go`

- [x] write test: DRA disabled → `Setup()` leaves `draPlugin` nil; all lifecycle methods
      no-op cleanly
- [x] write test: DRA enabled + `KubeClientFn()` returns nil → `Setup()` logs warning, leaves
      `draPlugin` nil; no error returned
- [x] write test: DRA enabled + empty NodeName → `Setup()` logs warning, leaves `draPlugin` nil
- [x] write test: DRA enabled + nil `cpuClasses` (no cpuClass config) → `Setup()` logs warning,
      leaves `draPlugin` nil
- [x] write test: DRA enabled + valid deps → `Setup()` builds plugin; `Start()` calls
      `plugin.Start()` then `plugin.PublishResources()`; verify ordering and that neither
      is called while the re-entrancy `WithLock` stub is held
      (adapted — see decision log: ordering/lock-contract verified via the existing Task 7/8
      real-`*dra.Plugin` test harness (`newTestDRAPlugin`) rather than a call-order mock,
      since `*dra.Plugin` is a concrete struct, not an interface, so its methods cannot be
      swapped for recording fakes)
- [x] write test: `Stop()` cancels context and calls `plugin.Stop()`
- [x] write test: `ValidateClasses` closure uses `p.cfg.DRASharedCounters()` (nil-safe) and
      picks up the config from a post-Reconfigure `p.cfg`
- [x] implement `buildDRAPlugin(opts *policyapi.BackendOptions) error` in `dra.go`:
      - guard: kube client, node name, cpuClasses all must be available; log + return nil on absence
      - construct `policyDRAAdapter` (Task 4)
      - `CDIWriter`: `dra.NewCDIWriter(DRADriverName, p.cdiDir)` (`p.cdiDir` injectable for tests)
      - `ClaimStore`: `dra.NewCacheClaimStore(p.cache)`
      - `ValidateClasses`: closure `func() error { return cpuclass.ValidateCPUClassesForDRA(p.cfg.CPUClasses, p.cfg.DRASharedCounters()) }`
      - `WithLock`, `NodeName`, `Logger` from opts; `KubeClient` from `opts.KubeClientFn()`
        with typed-nil check (see Context)
      - call `dra.New(DRADriverName, deps)` and store result in `p.draPlugin`
- [x] add `draPlugin *dra.Plugin`, `draCtx context.Context`, `draCtxCancel context.CancelFunc`,
      `cdiDir string` to `policy` struct
- [x] in `Setup()`, call `buildDRAPlugin(opts)` if `cfg.DRAEnabled()` (initial setup only;
      no DRAEnabled-flip check here — `p.cfg` is nil at construction time, making the check
      unreachable; flip detection belongs in `Reconfigure()`, Task 10)
- [x] in `Start()`, if `draPlugin != nil`: create `ctx, cancel = context.WithCancel(context.Background())`;
      store `p.draCtx = ctx; p.draCtxCancel = cancel`; call `draPlugin.Start(ctx)` then `draPlugin.PublishResources(ctx)`;
      then call `reapplyDRAClaims()` (Task 8)
- [x] implement `Stop() error` on `*policy`: cancel `draCtxCancel`, call `draPlugin.Stop()` if set; return nil
- [x] run tests — must pass before task 10

### Task 10: resmgr post-unlock seam + `PostReconfigure` hook + Reconfigure refusal

**Files:**
- Modify: `pkg/resmgr/resource-manager.go`
- Modify: `pkg/resmgr/policy/policy.go`
- Modify: `cmd/plugins/topology-aware/policy/topology-aware-policy.go`
- Modify: `cmd/plugins/topology-aware/policy/dra.go`
- Modify: `cmd/plugins/topology-aware/policy/dra_test.go`
- Modify: balloons + template backends (no-op `PostReconfigure`)

- [x] write test: Reconfigure changes an HP-class attr + live claim → returns error; `p.cpuClasses`
      and `p.root` are the old objects (verify `*p = savedPolicy` was applied); `opt` is old config
- [x] write test: Reconfigure with zero live claims for changed class → succeeds
- [x] write test: Reconfigure that changes `DRAEnabled()` (false→true or true→false) → returns
      error regardless of live claims
- [x] write test: `opt` (package global) is the old config after a refused Reconfigure
- [x] write test: `PostReconfigure()` not called while resmgr lock is held (re-entrancy stub);
      also not called when `m.reconfigure()` returns an error (guarded on `reconfErr == nil`)
- [x] add `PostReconfigure() error` to `Backend` interface and `Policy` wrapper;
      TA implementation calls `p.draPlugin.PublishResources(p.draCtx)` if set (Task 9 stores
      `draCtx context.Context` alongside `draCtxCancel`); balloons/template return nil
- [x] in `resmgr/resource-manager.go` `updateConfig()`, after the existing post-unlock calls and
      **only when `reconfErr == nil`**, call `m.policy.PostReconfigure()`
- [x] in `topology-aware Reconfigure()`:
      - **before** `opt = newCfg` / `p.cfg = newCfg`: snapshot old attrs via
        `p.cpuClasses.DRADevices(DRADriverName)` if `draPlugin != nil` and `p.cpuClasses != nil`
      - add DRA-enabled flip check: if `newCfg.DRAEnabled() != p.cfg.DRAEnabled()` → refuse
      - after `initialize()` on new config: snapshot new attrs (new `p.cpuClasses` may be nil if
        new config has no cpuClasses — handle nil); diff per class; for each class where attrs
        differ, check `p.draPlugin.LiveClaimClasses()[className] > 0` → refuse
      - **on any refusal**: full rollback via `*p = savedPolicy; opt = p.cfg; defaultPrio = ...`
        (same as existing rollback paths at `:546-549`), then `return policyError(...)`
      - on commit: call `p.draPlugin.RestoreClaimsLocked()` (inside the lock), then
        `p.reapplyDRAClaims()` (Task 8; inside the lock)
      - (PublishResources flows through `PostReconfigure` after the lock releases — no extra call here)
- [x] run tests — must pass before task 11

### Task 11: Verify acceptance criteria

- [x] `make build` succeeds; `go vet ./...` clean; `golangci-lint run` clean (no new warnings)
- [x] `go test ./...` passes across all packages
- [x] `make verify-generate` passes
- [x] DRA disabled: full existing test suite unchanged; no behavioral change
- [x] DRA enabled: plugin constructs + starts + publishes; Reconfigure refusal fires for class-attr
      changes with live claims; pool accounting consistent after Reconfigure + restart (unit-level)
- [x] Lock-contract tests pass: `Start`, `PublishResources`, `RestoreClaims`, `PostReconfigure`
      never called while re-entrancy stub is held

### Task 12: Update documentation

- [x] update `docs/dra/plan.md` step 8 entry: add "Landed:" line with commit range + deviations:
      (a) `AllocateClaim`/`ReleaseClaim` not on `Backend` interface — unexported TA policy methods;
      (b) Deps closures dropped — NRI path only; (c) CDI device names as claim UID source;
      (d) `LiveClaimsLocked()` added to `Plugin`; (e) `Backend.Stop()` + `PostReconfigure(ctx)` added
- [x] update `docs/dra/design.md` if any resolved decisions require wording corrections
      (corrected "CDI env-var protocol"/"NRI enforcement flow"/"Where the code lives" table:
      claim identification uses CDI device names, not NRI_CLASS/NRI_CPU<N> env-var parsing;
      no resolved decision itself changed)
- [x] move this plan to `docs/plans/completed/` (harness moves this after all review phases finish)

## Post-Completion

**Cluster verification (blocked on step 9 Helm additions):**
- After step 9: deploy TA with `dra.enabled: true`; verify `kubectl get resourceslices` shows
  slices from `nri.topology-aware.cpu`

**e2e test (step 10):**
- Full end-to-end: ResourceClaim selecting an HP cpuClass; verify pod CPUs on expected NUMA
  node; HP CLOS visible via SST tooling.

**Feature-gate probes (cross-cutting ⚠️):**
- `AllowMultipleAllocations` and `NodeAllocatableResources` probes deferred from step 6.
  Must land before step 8 merges to main (see plan.md cross-cutting note). No owner yet.
