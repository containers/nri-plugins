# DRA support for cpuClass — implementation plan

**Status:** draft. Companion to [design.md](design.md). Update alongside the design as decisions land.

**Related:** [design.md](design.md) (what to build), [landscape.md](landscape.md) (why / DRA ecosystem context), [pr-536-analysis.md](pr-536-analysis.md) (prior-art reference for the CDI+NRI bridge).

## Scope of this plan

v1 delivery: DRA support for the **topology-aware policy** exposing the full `cpuClass` surface. Balloons policy (v2) is out of scope here but the code split must not paint it into a corner — see step 2.

## Sequencing principle

Each step lands as its own PR, is independently reviewable, and does not require a subsequent step to be merged before it can be verified. Where a step introduces code that is not yet exercised, it must either be behind a config gate (default off) or covered by unit tests.

Approximate PR count: 8–10. Reviewer-friendly sizing; the largest logical unit is the driver package itself and even that splits cleanly.

## Steps

### Step 1 — Lift `pkg/kubernetes/{client,watch}` out of `pkg/agent`

**Rationale.** Every subsequent DRA step needs a Kubernetes client and object-watch helpers. [PR #536](https://github.com/containers/nri-plugins/pull/536) already did this lift; the code is proven. Doing it as its own PR isolates a pure refactor from any behavioral change.

**Actions.**
- Cherry-pick / re-derive the `pkg/kubernetes/client/` and `pkg/kubernetes/watch/` packages from [PR #536](https://github.com/containers/nri-plugins/pull/536) branch `pr-536-dra` (commits `5dcb66dc`, `42ec1022`, `45a14d1c`).
- Update `pkg/agent/agent.go` to use the new packages instead of its inline client bootstrap.
- Expose node name, kube client, and kube config from the agent ([PR #536](https://github.com/containers/nri-plugins/pull/536) commit `88140644`).

**Files touched:** `pkg/kubernetes/client/*`, `pkg/kubernetes/watch/*`, `pkg/agent/agent.go`.

**Verification:** existing tests pass, no behavioral change. Manual smoke: topology-aware plugin still starts and configures.

**Risk:** low. Refactor of already-vetted code.

**Landed:** commits `022b876f`…`f595440b` on branch `DRA` (see [`docs/plans/20260820-dra-step1-kubernetes-client-watch-lift.md`](../plans/completed/20260820-dra-step1-kubernetes-client-watch-lift.md) for the detailed per-task implementation log).

### Step 2 — Introduce `pkg/resmgr/dra/` package skeleton (no functionality)

**Rationale.** Physically separate the shared DRA driver code from any policy binary. Enforces resolved decision 6 (code-sharing verification checklist) structurally — subsequent steps can only add to this package, not to a policy-specific location.

**Actions.**
- Create `pkg/resmgr/dra/` with:
  - `doc.go` — package comment stating "policy-agnostic DRA kubelet plugin used by nri-plugins policies."
  - `plugin.go` — empty `Plugin` struct + constructor stub `New(driverName string, deps Deps) (*Plugin, error)` that returns an error "not yet implemented."
- No wire-up in any policy binary yet.

**Files touched:** `pkg/resmgr/dra/*` (new).

**Verification:** package compiles, empty-plugin test passes, no imports from `cmd/`.

**Risk:** low. Structural only.

**Landed:** commits `a1244e33`…`404ff242` on branch `DRA` (see [`docs/plans/20260820-dra-step2-resmgr-dra-skeleton.md`](../plans/completed/20260820-dra-step2-resmgr-dra-skeleton.md) for the detailed implementation log). Also added `*.test` to `.gitignore` (cleanup).

### Step 3 — Add `cpuClass.dra.publish` config field + validation

**Rationale.** Resolved decision 7 requires this before device publication makes sense. It's a small, self-contained config-API change that can land ahead of any driver code.

**Actions.**
- Extend `pkg/apis/config/v1alpha1/resmgr/policy/cpuclass.go`:
  - Add `DRA *CPUClassDRA` struct field to `CPUClass`.
  - Define `type CPUClassDRA struct { Publish *bool }` — pointer so "unset" is distinguishable from "explicit false."
  - Add `DRAPublish() bool` getter (nil → true).
- Add `ValidateCPUClassesForDRA(classes []*policyapi.CPUClass, sharedCounters bool) error` in **`pkg/resmgr/cpuclass/dra.go`** (new file; extended by Step 5):
  - Tier classification is static from config: managed PCT → `"pctPriority=<value>"`; assoc-only PCT → `"closID=<N>"`; non-PCT classes exempt.
  - Groups DRA-published PCT classes by tier label; returns a sorted, deterministic error if any tier has >1 published class and `sharedCounters == false`.
  - No `[]Punit` parameter — per-punit enforcement is deferred to Step 5 where runtime punit topology is available (CPUClass carries no punit affinity).
- Regenerate CRDs via `make generate` — updates all four YAML files (bases + helm copies).

**Files touched:** `pkg/apis/config/.../cpuclass.go`, `pkg/resmgr/cpuclass/dra.go` (new), `zz_generated.deepcopy.go`, four CRD YAML files, tests.

**Verification:** unit tests pass. Existing tests untouched. `make verify-generate` passes.

**Risk:** low. Additive config change.

**Not yet wired into anything — validation is called from step 6.**

**Landed:** commit `a0fd7f10` on branch `DRA` (see [`docs/plans/20260820-dra-step3-cpuclass-dra-publish.md`](../plans/completed/20260820-dra-step3-cpuclass-dra-publish.md)).

### Step 4 — Add `pct.Allocator.PickCpus` / `ReleaseCpus`, extract capacity helpers

**Rationale.** The driver's Prepare/Unprepare handlers need to select specific CPUs from a punit's HP tier and account them. Existing `pct.Allocator.FreeClassCapacity` / `trackHpUsage` cover the accounting; we need the picker.

**Actions.**
- Add to `pkg/resmgr/cpuclass/internal/pct/pct.go`:
  - `PickHpCpus(punitID int, n int, held cpuset.CPUSet) (cpuset.CPUSet, error)` — selects `n` CPUs from the punit that fit HP-tier constraints, using existing `hpEligiblePunit` and `hpUsed` state. Updates `hpUsed`.
  - `ReleaseHpCpus(punitID int, cpus cpuset.CPUSet)` — subtracts from `hpUsed[punitID]`.
  - Extract capacity computation into `punitHPCapacity(punitID int) int` and `punitNonHPCapacity(punitID int) int` helpers for reuse by device-shape construction.
- Unit tests for picker: single-punit exhaustion, cross-punit failure, held-CPU exclusion.

**Files touched:** `pkg/resmgr/cpuclass/internal/pct/pct.go`, `pkg/resmgr/cpuclass/internal/pct/pct_test.go`.

**Verification:** unit tests pass.

**Risk:** low-medium. Existing HP-room accounting is nontrivial; the new methods must share `hpUsed` semantics with `trackHpUsage`/`clearHpUsage`.

**Landed:** commit `5c4c04fa` on branch `DRA` (see [`docs/plans/20260820-dra-step4-pct-pick-release-hp-cpus.md`](../plans/completed/20260820-dra-step4-pct-pick-release-hp-cpus.md)). Implementation deviations from the original plan.md spec: signature uses `(pkgID, punitID int, ...)` not `(punitID int, ...)`; DRA holds tracked in a separate `hpDRAUsed` map (not `hpUsed`) to prevent `clearHpUsage` aliasing; exported `PunitInfo` + `Punits()` added for Step 5.

### Step 5 — `cpuclass.Handler.DRADevices()` — build the device list

**Rationale.** Given a set of cpuClass definitions and the PCT allocator's punit list, emit `[]resapi.Device` in Model B shape. Pure function; unit-testable without any DRA plumbing.

**Actions.**
- Add to `pkg/resmgr/cpuclass/`:
  - **Extend existing** `dra.go` (created in Step 3) with method `Handler.DRADevices(driverName string) ([]resapi.Device, error)`. Do not recreate the file.
  - Emits one device per (class × punit) where `class.DRAPublish() == true` (use the getter, not `class.DRA.Publish` directly — the latter panics on nil `DRA`) and the class is applicable to that punit's tier.
  - Attributes as spec'd in [design.md](design.md) (topology + class-derived).
  - Capacity `nri/cpus` with `RequestPolicy` tied to `punitHPCapacity` / `punitNonHPCapacity`.
  - `NodeAllocatableResourceMappings: {cpu: {capacityKey: nri/cpus, allocationMultiplier: "1"}}`.
  - `resource.kubernetes.io/numaNode` attribute (scalar int; list form deferred until [KEP-5491](https://github.com/kubernetes/enhancements/issues/5491) is baseline).
- Unit tests covering: single-HP class one punit, two HP classes one punit (one opted out), non-HP-only class, non-SST-TF (single "package" pseudo-punit).

**Files touched:** `pkg/resmgr/cpuclass/dra.go`, unit tests.

**Verification:** unit tests pass; hand-inspect a generated device list against a fixture.

**Risk:** medium. Attribute schema needs to be right on first design pass — changing it after users write claims is disruptive. Freeze attribute names via the tests.

**Landed:** commits `63a0df1c`…`db0b8770` on branch `DRA` (see [`docs/plans/20260820-dra-step5-cpuclass-dra-devices.md`](../plans/completed/20260820-dra-step5-cpuclass-dra-devices.md) for the detailed per-task implementation log). **go.mod bump** (`k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go`, `k8s.io/kubelet` → v0.36.3) is part of this step, not Step 6. **Deferred attributes:** `resource.kubernetes.io/numaNode` blocked (needs per-punit CPU list; `pct.PunitInfo` carries `PkgID`/`PunitID`/`HPCapacity`/`NonHPCapacity` but not CPU IDs); frequency attrs (`nri/minFreqKHz`, `nri/maxFreqKHz`, `nri/guaranteedHpFreqKHz`) blocked (symbolic frequency sentinels overflow without resolver — `resolveHWFreq` is unexported in `pct/cpufreq`); `nri/freqGovernor`, `nri/energyPerformancePreference`, `nri/uncoreMinFreqKHz`, `nri/uncoreMaxFreqKHz`, `nri/disabledCstates` deferred by choice (unblocked, plain config reads — additive follow-up).

**Follow-up (2026-08-28): two independent KEP-5517 field tombstones between Kubernetes 1.36 and 1.37, floor moved to 1.37.** Running the Step 10 e2e test against a real Kubernetes 1.37.0 cluster surfaced not one but **two** separate forward-compatibility breaks in the same KEP:

1. **Device-side:** `k8s.io/api` v0.37 (Kubernetes 1.37) tombstones `Device.NodeAllocatableResourceMappings` (`CapacityKey`/`AllocationMultiplier`) entirely — it's replaced by `Device.NodeAllocatableResources` (`map[ResourceName]NodeAllocatableResource{Mapping: *NodeAllocatableMapping{CapacityKey, CapacityMultiplier}, Overhead: ...}`), not merely renamed. A binary built against `k8s.io/api` v0.36 sending the old field name gets it silently pruned by a 1.37 API server (unrecognized field), so KEP-5517 support silently no-oped there even with the gate on.
2. **Pod-status-side:** `NodeAllocatableResourceClaimStatus.Resources map[ResourceName]Quantity` is *also* tombstoned in the same v0.37, independently of the Device-side change — replaced by `Mapping []NodeAllocatableMappedResources{Name, Quantity}` (a list, not a map). This one only surfaced after fixing (1): the feature-gate probe started passing, the claim allocated correctly (CDI env vars and CLOS association both verified correct on 1.37), but the `pod.status.nodeAllocatableResourceClaimStatuses[]` assertion still failed — `kubectl get pod ... -o jsonpath` showed `{"containers":["dra-pod0c0"],"mapping":[{"name":"cpu","quantity":"2"}],"resourceClaimName":"hp-turbo-cpus"}`, no `resources` key at all.

Fixed by bumping `k8s.io/api`/`k8s.io/apimachinery`/`k8s.io/client-go`/`k8s.io/kubelet`/`k8s.io/dynamic-resource-allocation` to v0.37.0, updating `dra.go`'s device construction to the new `NodeAllocatableResources`/`Mapping`/`CapacityMultiplier` shape (`pkg/resmgr/dra/plugin.go` also needed a new `WatchHealthStatus` method to satisfy `kubeletplugin.DRAPlugin`'s v0.37 interface — implemented as `return kubeletplugin.ErrHealthNotSupported`, this driver doesn't do per-device health reporting), and updating the e2e test's probe and its `wait-node-allocatable-claim-status` assertion (both in `test20-dra/code.var.sh`) to the new `nodeAllocatableResources`/`mapping[].{name,quantity}` shapes. **This moves the effective floor from Kubernetes 1.36 to 1.37** — a single compiled binary can only speak one wire-shape for these fields, so 1.36 clusters now (correctly) hit the test's skip path instead of passing. Verified live end-to-end against a fresh Kubernetes 1.37.0 gated VM after both fixes: `Test verdict: PASS`, full scheduler `DynamicResources` flow observable at `--v=5` (`nodeallocatabledynamicresources.go`'s `"Patched pod status with NodeAllocatableResourceClaimStatuses"`).

A rebuilt plugin image is required for any of this to take effect — `test/e2e/run_tests.sh` does **not** rebuild the plugin image itself; it picks up whatever `build/images/nri-resource-policy-<plugin>-image-*.tar` is newest, so a stale tarball from before a code change will silently keep testing the old binary. Run `make image.nri-resource-policy-topology-aware` (or the matching target for whichever plugin changed) before re-running an e2e test after a Go code change.

### Step 6 — Wire `pkg/resmgr/dra/` to `PublishResources` + Prepare/Unprepare stubs

**Rationale.** With devices constructible (step 5) and a picker (step 4), stand up the actual kubelet plugin. Steps 5–6 could be one PR, but splitting keeps the Kubernetes-facing surface isolated from the domain logic.

**Actions.**
- In `pkg/resmgr/dra/plugin.go`:
  - `Plugin.Start(ctx)` — register with kubelet via `k8s.io/dynamic-resource-allocation/kubeletplugin.Start`, driver name from constructor arg.
  - `Plugin.PublishResources(devices []resapi.Device)` — paginate into `pool0..poolN` at `ResourceSliceMaxDevices`, call `PublishResources` on the kubelet-plugin helper.
  - `Plugin.PrepareResourceClaims(claims)` — stub that returns `NotImplemented`. Step 7 fills this in.
  - `Plugin.UnprepareResourceClaims(claims)` — stub that returns `NotImplemented`. Step 7 fills this in.
- New file `deps.go` — interfaces the driver depends on (`ClaimAllocator`, `CDIWriter`) so the topology-aware wire-up can inject implementations without a circular import.
- Call `ValidateCPUClassesForDRA` (step 3) at plugin start; refuse to start if validation fails.

**Files touched:** `pkg/resmgr/dra/plugin.go`, `pkg/resmgr/dra/deps.go`.

**Verification:** integration test that starts the plugin against a fake kubelet-plugin harness, publishes a device list, verifies the harness saw the expected ResourceSlices.

**Risk:** medium. First real interaction with `k8s.io/dynamic-resource-allocation/kubeletplugin`. Consider a paired prototype experiment ([design.md](design.md) "Feature-gate detection" section — probe test) before landing.

**Imports & deps.** The Kubernetes-side helper packages this step (and steps 5/7) will pull in. Chosen after inspecting what `dra-example-driver` imports from Kubernetes and filtering to what we actually need for a kubelet-plugin driver. Tiered by necessity:

*Must-have — cannot build a DRA driver without these:*
- `k8s.io/dynamic-resource-allocation/kubeletplugin` — `kubeletplugin.Start`, `Helper`, `PrepareResult`, `Device`, `NamespacedObject`, `ErrRecoverable`, path constants `KubeletRegistryDir`/`KubeletPluginsDir`. The DRA kubelet-plugin library. Used by [PR #536](https://github.com/containers/nri-plugins/pull/536) and every DRA driver.
- `k8s.io/dynamic-resource-allocation/resourceslice` — `DriverResources`, `Pool`, `Slice`. The type `PublishResources` expects.
- `k8s.io/api/resource/v1` (`resapi`) — DRA API types (`Device`, `ResourceClaim`, `DeviceAttribute`).
- `k8s.io/apimachinery/pkg/types` — `types.UID`, `types.NamespacedName`. Already used across nri-plugins.
- `k8s.io/apimachinery/pkg/api/resource` — `resource.Quantity`, needed for `RequestPolicy.default/min/max/step`.
- `tags.cncf.io/container-device-interface/pkg/cdi` — `Cache` with `WriteSpec`/`RemoveSpec`/`NewCache(WithSpecDirs(...), WithAutoRefresh(false))`, name helpers `GenerateSpecName` / `GenerateTransientSpecName`. Atomic tmp+rename writes and spec validation, so we don't reinvent them.
- `tags.cncf.io/container-device-interface/specs-go` — the wire types `Spec`, `Device`, `ContainerEdits` used to construct CDI spec content in a type-safe way.
- `tags.cncf.io/container-device-interface/pkg/parser` — `QualifiedName(vendor, class, name)` helper for CDI device references returned in `PrepareResult.CDIDeviceIDs`.

  Not currently a nri-plugins dep — will be added at step 7. Apache-2.0, tracked upstream at [cncf-tags/container-device-interface](https://github.com/cncf-tags/container-device-interface). Both reference DRA drivers use it.

*Should probably use — idiomatic, low-risk, small additions:*
- `k8s.io/client-go/util/retry` — retry-with-backoff for API-server writes. Pull in when step 7 first writes to `resourceclaims/status` or similar.
- `k8s.io/utils/ptr` — `ptr.To(x)` one-liner. Likely already transitively present.

*Explicitly not adopted (with rationale):*
- `k8s.io/klog/v2` — the DRA library expects `logr.Logger` and reads it from `context.Context` via `klog.FromContext`. nri-plugins uses its own logger (`pkg/log/`). Bridge with a small adapter under `pkg/resmgr/dra/logging.go` that wraps nri-plugins' logger as `logr.Logger` and injects it into the context passed to `kubeletplugin.Start`. Do not switch DRA-side code to raw klog.
- `k8s.io/component-base/featuregate` — reads `--feature-gates` CLI flags. nri-plugins doesn't expose those; we probe the API server at startup ([design.md](design.md) "Feature-gate detection") instead.
- `k8s.io/component-base/metrics` + `legacyregistry` — Prometheus via a second registry. nri-plugins already has its own metrics setup; reuse that.
- `k8s.io/apimachinery/pkg/runtime` + `runtime/serializer/json` — needed by `dra-example-driver` for its versioned checkpoint file. We are using [PR #536](https://github.com/containers/nri-plugins/pull/536)'s opaque `pkg/resmgr/cache` entry instead of a dedicated checkpoint file (see [design.md](design.md) step 7 discussion).
- `sigs.k8s.io/controller-runtime` — used by `dra-example-driver`'s controller binary. We are not shipping a controller in v1.
- `k8s.io/kubelet/pkg/apis/dra/v1` (`drapb`) — older DRA protobuf types. `kubeletplugin` exposes its own `Device` type; driver authors don't touch the protobufs directly.
- `k8s.io/kubelet/pkg/apis/pluginregistration/v1` — plugin-registration protobuf, under-the-hood detail of `kubeletplugin`. Don't import directly.

*Deferred to future work (v2 or later):*
- `k8s.io/dynamic-resource-allocation/client` (`draclient`) — thin wrapper for DRA-specific status endpoints (`resourceclaims/status` etc.). Needed if we ever adopt [KEP-4817](https://github.com/kubernetes/enhancements/issues/4817) device-status reporting. Not in v1.

**Do not import from `sigs.k8s.io/dra-example-driver` or `github.com/kubernetes-sigs/dra-driver-cpu`.** Both are positioned as reference implementations, not libraries. Their reusable-looking helpers live in `internal/` and are not exported. Copy patterns, credit the source in code comments.

**Landed:** commits `5c39733a`…`10d0403c` on branch `DRA` (see [`docs/plans/20260821-dra-step6-plugin-wire.md`](../plans/20260821-dra-step6-plugin-wire.md) for the detailed per-task implementation log). Deviations from spec: `Deps` uses `ValidateClasses func() error` closure (not `ClaimAllocator`/`CDIWriter` — those are Step 7); pool layout is one pool (node name) with N slices (not `pool0..poolN` as stated above); logger bridge uses `logr.FromSlogHandler` (not hand-rolled `LogSink`); feature-gate probes deferred to follow-up (see Cross-cutting note); `k8s.io/dynamic-resource-allocation` resolved to v0.36.4 by Go MVS (not v0.36.3 as requested).

### Step 7 — Prepare/Unprepare implementation + CDI writer

**Rationale.** Turn stubs into real allocation. This is the load-bearing PR.

**Actions.**
- Implement `PrepareResourceClaims`:
  - For each claim, parse `Status.Allocation.Devices.Results[]` to extract `(className, punitID, count)`.
  - Call `cpuclass.Manager.PickCpus(className, punitID, count)` (delegates to `pct.Allocator.PickHpCpus` for HP classes; simple "free-CPU-in-punit" pick for non-HP).
  - Emit CDI entry `claim-<uid>` with env vars `NRI_CLASS=<className>`, `NRI_CPU<id>=1` for each picked CPU.
  - Return `PrepareResult` with `CDIDeviceIDs: ["nri.topology-aware.cpu/device=claim-<uid>"]`.
  - Persist claim state (`{uid, className, punitID, cpus}`) via the existing opaque cache (`pkg/resmgr/cache/cache.go` `SetEntry`/`GetEntry` — [PR #536](https://github.com/containers/nri-plugins/pull/536) already added this).
- Implement `UnprepareResourceClaims`: release CPUs, remove CDI entry, drop cache entry.
- CDI writer in `pkg/resmgr/dra/cdi.go` — use `tags.cncf.io/container-device-interface` upstream library (`cdiapi.Cache` with `WithAutoRefresh(false)`, `WriteSpec`, `RemoveSpec`; `specs-go.Spec`/`Device`/`ContainerEdits` types; `GenerateTransientSpecName(vendor, class, claimUID)` for per-claim filenames). Do NOT carry forward [PR #536](https://github.com/containers/nri-plugins/pull/536)'s hand-rolled `fmt.Fprintf` YAML — it lacks atomic writes, spec validation, and version tracking. See `dra-driver-cpu`'s [`pkg/driver/cdi.go`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/pkg/driver/cdi.go) as the pattern.
- Restart reconciliation: on `Plugin.Start`, load persisted claims and rebuild `hpUsed`. If the persisted claim's CPUs are still in the CDI spec, keep it; otherwise drop and warn.
- Expose `Plugin.LiveClaimClasses() map[string]int` — returns a map of `className → liveClaimCount` over all persisted claim state. Used by Step 8's Reconfigure check (resolved decision 8 / Option B).

**Files touched:** `pkg/resmgr/dra/{plugin,cdi,state}.go`, unit tests, integration tests.

**Verification:** integration test — publish, receive fake `PrepareResourceClaims` call, assert CDI spec written correctly, assert `hpUsed` updated. Restart-with-existing-claim test.

**Risk:** high. Correctness of concurrent Prepare + NRI-phase access to `hpUsed` ([design.md](design.md) "Coexistence" section) must be locked down.

**Landed:** commits `148d09b2`…`52b6f182` on branch `DRA` (see [`docs/plans/20260821-dra-step7-prepare-unprepare-cdi.md`](../plans/20260821-dra-step7-prepare-unprepare-cdi.md) for the detailed per-task implementation log). Deviations from spec: concurrency is via resmgr write-lock closure (`WithLock func(func())` in Deps), not a per-allocator mutex; CDI device name is per-result `claim-<uid>-<sanitize(request)>-<device>-<idx>` (not per-claim `claim-<uid>`), where `sanitize` replaces `/` and invalid CDI name chars with `-`; `ClaimState`/`ResultAlloc` are exported types; non-HP DRA pick is deferred (see "Not part of v1" below — `pct.PunitInfo` carries capacity counts but not per-punit CPU lists); `RestoreClaimsLocked() error` is the lock-held variant for use inside `resmgr.apply()`, with `RestoreClaims() error` as the `WithLock` wrapper for external callers not already holding the lock; `Start`, `PublishResources`, and `RestoreClaims` must not be called while holding the resmgr lock (non-reentrant `sync.RWMutex`).

### Step 8 — Topology-aware policy wire-up

**Rationale.** Finally connect the driver to the topology-aware plugin binary and its allocator.

**Actions.**
- In `cmd/plugins/topology-aware/main.go`:
  - Read `TopologyAwarePolicy.spec.dra.enabled` config.
  - If enabled, instantiate `dra.Plugin` with driver name `nri.topology-aware.cpu` and start it.
  - Publish devices on plugin start and after every `Reconfigure`.
- Extend `pkg/resmgr/policy/policy.go` `Backend` interface with `AllocateClaim(Claim)` / `ReleaseClaim(Claim)` ([PR #536](https://github.com/containers/nri-plugins/pull/536) pattern, cleaned up).
- Implement in `cmd/plugins/topology-aware/policy/pools.go`:
  - `allocateClaim` — find tightest pool containing claim CPUs, evict conflicting exclusive grants, mark CPUs claimed in pool supply, reallocate displaced grants. Adapted from [PR #536](https://github.com/containers/nri-plugins/pull/536) with the `getClaimedCPUs` env-var parser generalized to read both `NRI_CLASS` and `NRI_CPU<N>` (see [design.md](design.md) CDI env-var protocol).
  - `releaseClaim` — reverse.
- **Option B — Reconfigure refusal (resolved decision 8):** before committing a new config, compare incoming `cpuClass` definitions against the currently-published slice's per-class attribute snapshot stored in `pkg/resmgr/dra/`. For each class whose attributes differ, call `Plugin.LiveClaimClasses()` (Step 7); if `liveClaimCount > 0`, return an error from `policy.Reconfigure` naming the affected class(es) and changed fields. The resmgr rollback-on-failure path (`resource-manager.go`) then re-applies the old config without any further action needed. Class deletion with live claims is treated as an attribute change (all attributes go to absent) — same code path.
- Extend `TopologyAwarePolicy` config CR with `spec.dra { enabled: bool, sharedCounters: bool }`.
- Add `spec.dra.enabled: false` to all existing test configs to keep behavior unchanged.

**Files touched:** `cmd/plugins/topology-aware/main.go`, `cmd/plugins/topology-aware/policy/pools.go`, `pkg/resmgr/policy/policy.go`, `pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/config.go`, CRD regeneration.

**Verification:** e2e test — deploy topology-aware with `dra.enabled: true`, create a `ResourceClaim` selecting an HP class, verify pod gets the expected CPUs at CLOS/EPP/etc.

**Risk:** high. Same class of concurrency and eviction hazards as [PR #536](https://github.com/containers/nri-plugins/pull/536) flagged (`TODO: sort old grants by QoS class`). Prototype scars need cleanup before merge, not after.

**Landed:** commits `f0834c77`…`6ce104a6` on branch `DRA` (see [`docs/plans/20260823-dra-step8-topology-aware-wire-up.md`](../plans/20260823-dra-step8-topology-aware-wire-up.md) for the detailed per-task implementation log). Deviations from spec:

- (a) `AllocateClaim`/`ReleaseClaim` were **not** added to the `Backend` interface — `allocateClaim`/`releaseClaim` are unexported `*policy` methods called only from `AllocateResources`/`ReleaseResources` (avoids no-op stubs in every other backend; YAGNI).
- (b) The `Deps`-closure Prepare-path idea was dropped in favor of an NRI-only call path — pool eviction of conflicting exclusive grants requires calling `updateContainers`, which only the NRI handler can invoke; a `Prepare`-path closure cannot reach it.
- (c) Claim identification uses **CDI device names**, not the `NRI_CLASS`/`NRI_CPU<N>` env-var protocol described in [design.md](design.md)'s "CDI env-var protocol"/"NRI enforcement flow" sections. `cache.Container.GetCDIDeviceNames()` exposes the container's CDI device names; `parseCDIClaimUID` recovers the claim UID from the `claim-<uid>-<sanitize(request)>-<device>-<idx>` shape (Step 7's naming). This is a best-effort parse (strips the trailing three `-`-separated tokens: idx/device/request) that can mis-parse multi-token sanitized device/request names, so `claimCPUsFromContainer` falls back to `matchLiveClaimUID`, which re-derives the split against the caller's known live-claim UID set when the fast-path guess isn't itself a live claim.
- (d) `Plugin.LiveClaimsLocked() map[types.UID][]ResultAlloc` was added to `*dra.Plugin` (lock-held convention, no internal `WithLock` call, doc'd "caller must hold the resmgr lock") to let the policy re-mark pool supplies after `Start`/`Reconfigure` (`reapplyDRAClaims`/`remarkClaimInSupply`) without inflating `claimContainerRefs`.
- (e) `Backend.Stop() error` and `Backend.PostReconfigure() error` were added to the `Backend`/`Policy` interfaces (not spec'd in the original plan text). TA's `Stop()` cancels `draCtx`/`draCtxCancel` and calls `draPlugin.Stop()`; TA's `PostReconfigure()` calls `draPlugin.PublishResources(p.draCtx)` from the resmgr's post-unlock seam. No-op implementations added to balloons and template backends.

Additional deviations not in the original checklist: the claim-lookup helper (`claimCPUsFromContainer`) is written against a local `claimLister` interface (`LiveClaimsLocked() map[types.UID][]dra.ResultAlloc`) rather than a concrete `*dra.Plugin`, so unit tests can use a lightweight fake instead of standing up a full plugin; the `draPlugin *dra.Plugin` field was added to the `policy` struct in Task 7, ahead of its Task 9 lifecycle wiring, because the plan's own `AllocateResources`/`ReleaseResources` wiring text already required the field to exist; `allocateClaim` errors fail the NRI `AllocateResources` call while `releaseClaim` errors are only logged (mirrors `ReleaseResources`'s existing tolerance of a missing grant); `buildDRAPlugin` warns-and-returns-nil only for the four documented "not ready yet" preconditions (no `KubeClientFn`, nil kube client, empty node name, nil `cpuClasses`) and returns a hard error for genuine construction failures (`dra.NewCDIWriter`/`dra.New` errors); the Reconfigure attribute-change check diffs live `p.cpuClasses.DRADevices(DRADriverName)` snapshots taken immediately before/after `initialize()` rather than a separately persisted "currently-published slice" store.

### Step 9 — Helm chart additions

**Rationale.** Deployment-shaped: RBAC, mounts, DeviceClass objects. Independent of code but needed before e2e can run.

**Actions.**
- `deployment/helm/topology-aware/templates/`:
  - `clusterrole.yaml` — add RBAC rules from [design.md](design.md) "Helm chart additions."
  - `daemonset.yaml` — add host mounts for `/var/lib/kubelet/plugins`, `.../plugins_registry`, `/var/run/cdi`.
  - `native-cpu-device-class.yaml` — the base `DeviceClass nri.topology-aware.cpu`. Per-class shortcut DeviceClasses generated from Helm values if listed.
- Values additions: `dra.enabled` toggles the DeviceClass emission and RBAC installation.

**Files touched:** `deployment/helm/topology-aware/templates/*`, `deployment/helm/topology-aware/values.yaml`.

**Verification:** `helm template` + `helm lint`. Deploy on a [KEP-5075](https://github.com/kubernetes/enhancements/issues/5075) + [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517)-enabled cluster; verify ResourceSlices appear.

**Risk:** low-medium. Kubelet-plugin socket path mounts have historically been the source of "why doesn't my driver register" bugs; verify against `dra-driver-cpu`'s Helm chart as a known-good reference.

**Landed:** commits `2030c1f9`…`2b046670` on branch `DRA` (see
[`docs/plans/20260825-dra-step9-helm-chart-additions.md`](../plans/20260825-dra-step9-helm-chart-additions.md)
for the detailed per-task implementation log). Implementation deviations from this
plan's original Actions text: gating value is `.Values.config.dra.enabled` (read
nil-safely as `(.Values.config.dra).enabled` in every gated template, not a
separate chart-level toggle) rather than a bare `dra.enabled`; the new template file
is named `deviceclass.yaml`, not `native-cpu-device-class.yaml` (matches this
chart's existing single-lowercase-word filename convention); per-cpuClass shortcut
`DeviceClass` generation was not implemented (deferred — see design.md's "Helm chart
additions" second tier, no concrete consumer yet); RBAC dropped `deviceclasses: get`
and `resourceclaims/status: patch,update` from design.md's original table — neither
has a call site in `pkg/resmgr/dra/` or the vendored
`k8s.io/dynamic-resource-allocation@v0.36.4`; the `/var/run/cdi` mount was
additionally confirmed to be a deliberate addition beyond PR #536's precedent (which
only mounted the two `kubelet/plugins{,_registry}` paths), required because the
landed driver writes CDI specs there (`pkg/resmgr/dra/cdi.go`); all three mounts land
at identical host/container paths with no `/host` prefix (corrected from design.md's
stale `/host/var/run/cdi`) and are read-write, not `readOnly`. The mounts/RBAC were
also diffed directly against the `dra-driver-cpu` reference chart (available locally
at `/home/ed/git/dra-driver-cpu/deployment/helm/dra-driver-cpu`): its `cdi-dir` mount
matches this chart's `/var/run/cdi` addition, and no `mountPropagation`/`hostPath.type`
deviation with functional impact was found; its clusterrole additionally grants
`pods: get/list/watch` and a `resourceclaims/driver` (`associated-node:patch/update`)
rule that this chart does not — neither has a call site in `pkg/resmgr/dra/` or
`cmd/plugins/topology-aware/policy/dra*.go`, so they were left out per the same
drop-unused-rules rationale as `deviceclasses`/`resourceclaims/status`.

### Step 10 — e2e test

**Rationale.** Prove the full path end-to-end. Follows the pattern of existing `test/e2e/policies.test-suite/topology-aware/n4c16/test*/`.

**Actions.**
- New test directory `test/e2e/policies.test-suite/topology-aware/n4c16/testXX-dra/` containing
  a single `code.var.sh` — matches the convention of `test19-cpuclass` (cpuClass config and
  Kubernetes manifests generated inline via shell variables + `instantiate helm-config.yaml`,
  not separate static `config.yaml`/`resourceclaim.yaml`/`pod.yaml` files).
- **Hardware-independent setup.** Reuse `test19-cpuclass`'s exact `OVERRIDE_SYS_CPUFREQ` /
  `OVERRIDE_SST` / `OVERRIDE_SST_STATE_DIR` mock block so the test runs identically on any
  machine — no physical Intel PCT/SST-TF silicon or real cpufreq driver required:
  ```bash
  OVERRIDE_SYS_CPUFREQ='[{"cpus": "0-15", "base": 2900000, "min": 800000, "max": 3800000}]'
  OVERRIDE_SST='{"supported": true, "clos_count": 4, "packages": [
    {"id": 0, "cpus": "0-7",  "tf_supported": true, "cp_supported": true, "max_hp_cpus": 2},
    {"id": 1, "cpus": "8-15", "tf_supported": true, "cp_supported": true, "max_hp_cpus": 2}]}'
  OVERRIDE_SST_STATE_DIR="/tmp/nri-pct-mock"
  CPU_CLASSES="[ { name: hp-turbo, pctPriority: high, pctMinFreq: turbo, pctMaxFreq: turbo } ]"
  ```
  wired via `EXTRA_ENV_OVERRIDE_SYS_CPUFREQ` / `EXTRA_ENV_OVERRIDE_SST` /
  `EXTRA_ENV_OVERRIDE_SST_STATE_DIR` into `helm-config.yaml`, plus `dra.enabled: true`.
- A `ResourceClaim` selecting on the already-landed `nri/pctPriority` attribute only — no
  class-name selector, no frequency-bound attributes (those remain deferred; see Cross-cutting
  "Not part of v1"):
  ```yaml
  apiVersion: resource.k8s.io/v1
  kind: ResourceClaim
  metadata:
    name: hp-turbo-cpus
  spec:
    devices:
      requests:
      - name: cpus
        exactly:
          deviceClassName: nri.topology-aware.cpu
          capacity:
            requests:
              nri/cpus: "2"
          selectors:
          - cel:
              expression: |
                device.attributes["nri"].pctPriority == "high"
  ```
- A pod referencing the claim via `resourceClaims` / `resources.claims`.
- `code.var.sh` assertions: pod scheduled and running; `NRI_CLASS` / `NRI_CPU<N>` env vars
  present in the container; the claimed CPUs are associated to the HP CLOS in the policy log
  (`wait-assert-log-contains 'associated cpus <cpus> to CLOS <hp-clos-id>'`, matching
  `pct.go`'s `"pct: associated cpus %s to CLOS %d"` line — **not** the mock's startup-time
  `ConfigureClos` line, which would pass even without a live claim; same style as
  `test19-cpuclass`'s `assert-cpu-clos`); `pod.status.nodeAllocatableResourceClaimStatuses[]`
  contains an entry for the claim with `resources.cpu` equal to 2 ([KEP-5517](https://github.com/kubernetes/enhancements/issues/5517)) — **not**
  `node.status.allocatable.cpu`, which KEP-5517 never mutates (see Step 10's "Landed" line).
- **Feature-gate precondition — all-or-nothing.** Probe both [KEP-5075](https://github.com/kubernetes/enhancements/issues/5075) and [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) at test setup; skip the entire test with a clear message if either is missing. KEP-5075 is a hard requirement (the device/claim shape itself depends on `AllowMultipleAllocations`+`RequestPolicy`; without it there's no fallback claim shape). KEP-5517 is only a soft requirement at the plugin level (design.md's "Feature-gate detection": mapping ignored, not fatal) — but the test treats it the same as KEP-5075 for simplicity, rather than conditionally skipping just the `node.status.allocatable.cpu` assertion on clusters with partial gate support.

**Files touched:** `test/e2e/policies.test-suite/topology-aware/n4c16/test20-dra/code.var.sh`
(new); harness changes required to run it: `test/e2e/playbook/provision.yaml`,
`test/e2e/files/Vagrantfile.in`, `test/e2e/run.sh`, `test/e2e/run_tests.sh` (feature-gate
plumbing, Task 1), `test/e2e/policies.test-suite/topology-aware/helm-config.yaml.in`
(`dra.enabled` passthrough, Task 2).

**Verification:** e2e test passes on the [KEP-5075](https://github.com/kubernetes/enhancements/issues/5075)+5517 test cluster. Hardware-independence is limited to the SST/cpufreq mocks — the test still depends on the K8s API server itself exposing the alpha DRA feature gates.

**Risk:** medium. Depends on cluster support for the DRA feature gates specifically (not on physical PCT/SST-TF hardware, which is mocked); the CI environment may need updating for [KEP-5075](https://github.com/kubernetes/enhancements/issues/5075)/[KEP-5517](https://github.com/kubernetes/enhancements/issues/5517).

**Landed:** commits `327afdc8`…`b5883bf6` on branch `DRA` (see
[`docs/plans/20260825-dra-step10-e2e-test.md`](../plans/20260825-dra-step10-e2e-test.md)
for the detailed per-task implementation log), test directory
`test/e2e/policies.test-suite/topology-aware/n4c16/test20-dra/`, exactly as originally
planned (no rename). Implementation deviations from this plan's original Actions text:
- The e2e harness had no way to turn on API-server/scheduler/kubelet feature gates at
  all before this step — `test/e2e/playbook/provision.yaml`'s `kubeadm init` took no
  `--config`. Extending it (a `k8s_feature_gates` var threaded through
  `Vagrantfile.in`'s `ansible.extra_vars`, a generated multi-doc `ClusterConfiguration`
  + `KubeletConfiguration` on `kubeadm.k8s.io/v1beta4`, list-of-`{name,value}`
  `extraArgs`, confirmed against the live VM's installed `kubeadm`/`kubelet` v1.36.4)
  was a harness-provisioning capability not originally scoped by this Step 10
  description — see the linked plan's Task 1.
- `Config.Validate()` requires a shared (non-PCT) `shared` CPU class whenever
  `cpuClasses` is non-empty, so the test's `CPU_CLASSES` needed a second plain
  `shared` class alongside `hp-turbo`, not just the PCT class alone as the sketch
  above shows.
- The feature-gate probe (Technical Details/Task 3 of the linked plan) reads back the
  driver's own published `ResourceSlice` and checks both `allowMultipleAllocations`
  (KEP-5075) and `nodeAllocatableResourceMappings` (KEP-5517) survived the round trip.
  On this test cluster's Kubernetes 1.36, KEP-5075/`DRAConsumableCapacity` is beta and
  default-`true`, so in practice only KEP-5517/`nodeAllocatableResourceMappings`
  (alpha, default-`false`) actually gates the probe/skip-path on an ungated cluster —
  the AND of both fields is still correct, just not both load-bearing here.
- The `ResourceClaim` selector needed `device.attributes["nri"].packageID == 1`
  pinned in addition to `pctPriority == "high"`, to avoid colliding with the
  reserved-pool CPU 0 on package 0 (see the linked plan's Task 4). Actual claimed
  CPUs observed on the gated VM: 8 and 9 (from package 1's 8-15 range).
- **The CLOS-association and allocatable-deduction assertions above are corrected
  from this Step 10 section's original text**, per a finding in the linked plan's
  Task 5: the `pct_sst_mock.go` `ConfigureClos` line logs at daemon-startup time (CLOS
  frequency-bound programming), not per-CPU association, and would pass even if the
  claim were never allocated. The correct, per-allocation line is `pct.go:786`'s
  `"pct: associated cpus %s to CLOS %d"` (observed: `pct: associated cpus 8-9 to CLOS
  0`). Separately, `node.status.allocatable.cpu` is **never** mutated by KEP-5517 on
  any real cluster — confirmed by reading
  `pkg/scheduler/framework/plugins/dynamicresources/nodeallocatabledynamicresources.go`
  in Kubernetes source (`NodeAllocatableResourceMapping`/`AllocationMultiplier`
  accounting is consumed only by the scheduler's in-memory `NodeInfo` cache; zero
  references under `pkg/kubelet/`) and by direct observation on the gated VM
  (`node.status.allocatable.cpu` stayed at `16` through a full post-Ready poll). The
  actual persisted, observable signal is
  `pod.status.nodeAllocatableResourceClaimStatuses[]`, written by the scheduler at
  PreBind (observed: `[{"resourceClaimName":"hp-turbo-cpus","containers":["dra-pod0c0"],"resources":{"cpu":"2"}}]`)
  — matching `docs/dra/landscape.md:31`, which already had this right. This plan
  section and design.md's equivalent passages previously carried the inaccurate
  "deducted from `node.status.allocatable.cpu`" phrasing; this section has now been
  corrected in place.
- **Coverage gap (accepted, not fixed):** the test only checks KEP-5075/
  `DRAConsumableCapacity`'s presence via the static `ResourceSlice` field round-trip in
  the feature-gate probe; it never functionally exercises consumable/shared capacity
  (the single claim in this test consumes a device's entire capacity, 2 of 2). A
  regression in multi-claim capacity-sharing on one device wouldn't be caught by this
  test. Adding a second, concurrent claim against the same device to exercise that path
  is a real scope expansion (a new test scenario, not a fixup) — left as a follow-up if
  KEP-5075's sharing behavior needs its own coverage.
- **Follow-up commits `38f69599` + a later `code.var.sh` fixup (2026-08-28):** running
  this test against a real Kubernetes 1.37.0 cluster (not just 1.36) surfaced two
  independent KEP-5517 tombstones, both described in Step 5's "Landed" line — the
  feature-gate probe's `jq` query was updated from `nodeAllocatableResourceMappings` to
  `nodeAllocatableResources` to match the Device-side rename, and once that unblocked
  the probe, `wait-node-allocatable-claim-status`'s assertion needed a second,
  independent fix from `.resources.cpu == "$cpus"` to `.mapping[] | {name, quantity}`
  matching to match the separately-tombstoned pod-status-side field. This is also where
  the floor-version bump to 1.37 is actually exercised end-to-end (`Test verdict: PASS`
  on a fresh 1.37.0 gated VM, after both fixes and a plugin-image rebuild — a stale
  image from before the Go code change silently masked the fix on the first re-run).

## Cross-cutting

### Feature gate probes

Design.md "Feature-gate detection" spec'd probe-at-startup for `AllowMultipleAllocations`, `NodeAllocatableResources`. Implement in step 6 as part of `Plugin.Start`. Land the probes even without [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) (Model C stays not-yet-implemented) so the code path exists when Model C is added.

⚠️ Feature-gate probes (`AllowMultipleAllocations`, `NodeAllocatableResources`) deferred from Step 6 to a follow-up task. They did not land alongside Step 8 either (Step 8's "Landed" entry above has no probe-related deviation, and no probe code exists anywhere in `pkg/resmgr/dra/` or `cmd/plugins/topology-aware/policy/` — only comments reference the concept). This is a tracked follow-up gap, not a precondition that blocked Step 8: Step 8 does not depend on the probes (its acceptance criteria and e2e scenario mock the SST/cpufreq surface, not `node.status.allocatable.cpu`). Land the probes as their own follow-up task before relying on `NodeAllocatableResources`-dependent capacity fields in a real cluster.

⚠️ Without the probe, DRA-published capacity fields that require `NodeAllocatableResources` will be silently stripped by the API server — acceptable for the stub phase.

⚠️ Step 10's e2e test (landed — see its "Landed" line above) implements its own feature-gate
probe (a `ResourceSlice` round-trip read-back checking `allowMultipleAllocations`/
`nodeAllocatableResourceMappings` survived, with a skip path if either is absent), but that
probe is **test-only** — it lives in
`test/e2e/policies.test-suite/topology-aware/n4c16/test20-dra/code.var.sh`, runs after
`helm-launch`, and exists purely to make the e2e test skip cleanly on an ungated cluster. It
does **not** close the production gap tracked immediately above: `Plugin.Start` still has no
probe of its own, and the driver still publishes `AllowMultipleAllocations`/
`NodeAllocatableResources`-dependent fields unconditionally regardless of what the live API
server supports. Do not mistake Step 10 landing for this cross-cutting item being resolved.

### Not part of v1

Explicitly deferred to later work, tracked here so nobody accidentally scope-creeps a step:
- Balloons policy wire-up (v2).
- Model C / [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) shared counters (waits for upstream alpha).
- `cpuClass.dra` sub-block fields beyond `publish` (custom `capacityKey`, per-class DeviceClass suppression).
- PodResources API integration for DRA-claimed CPUs ([KEP-3695](https://github.com/kubernetes/enhancements/issues/3695); complementary but independent).
- Non-HP DRA CPU pick (deferred from Step 7): `pct.PunitInfo` carries HP/non-HP capacity counts but not per-punit CPU lists; non-HP devices are filtered out of DRA publication until `PunitInfo` is extended with per-punit CPU IDs. Tracked in Step 7 plan; see `buildDRADevices` `hpOnly: true` filter in `pkg/resmgr/cpuclass/dra.go`.

### Testing philosophy

- Steps 1–5: unit tests only, no live cluster needed.
- Steps 6–7: integration tests against a fake kubelet-plugin harness; still no live cluster.
- Steps 8–10: e2e cluster required, [KEP-5075](https://github.com/kubernetes/enhancements/issues/5075) and [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) feature gates on.

Splitting this way means the first 5 PRs can land and be reviewed in parallel with cluster-setup work, and any KEP-graduation delays only affect the last three steps.

## Change log

- **2026-08-28.** Fixed two independent Kubernetes 1.37 forward-compatibility breaks in
  KEP-5517, both surfaced by running Step 10's e2e test against a real 1.37.0 cluster:
  (1) `k8s.io/api` v0.37 tombstones `Device.NodeAllocatableResourceMappings`, replacing
  it with the differently-shaped `Device.NodeAllocatableResources` (`Mapping`/
  `Overhead`); (2) the same release *independently* tombstones
  `NodeAllocatableResourceClaimStatus.Resources map[ResourceName]Quantity` on the Pod
  status side too, replacing it with `Mapping []NodeAllocatableMappedResources{Name,
  Quantity}` — a list, not a map. Bumped `k8s.io/api` and siblings to v0.37.0, reworked
  `dra.go`'s device construction to the new shape, added `Plugin.WatchHealthStatus`
  (new `kubeletplugin.DRAPlugin` interface method in v0.37, returns
  `ErrHealthNotSupported`), and updated both the e2e test's feature-gate probe and its
  `wait-node-allocatable-claim-status` assertion to the new field names/shapes.
  **This moves the effective floor from Kubernetes 1.36 to 1.37** — see Step 5's and
  Step 10's "Landed" lines for the full writeup, including a note on the stale-plugin-
  image gotcha this chase ran into (`run_tests.sh` doesn't rebuild the plugin image;
  `make image.nri-resource-policy-topology-aware` must be run manually after a Go code
  change). Also added a `k8s_log_verbosity` e2e harness var (independent of
  `k8s_feature_gates`, same threading pattern) to make `--v=N`/
  `KubeletConfiguration.logging.verbosity` debugging available for future DRA (or any)
  e2e work.
- **2026-08-26.** Step 10 landed: added "Landed" line with commit range `327afdc8`…`b5883bf6`,
  test directory `test/e2e/policies.test-suite/topology-aware/n4c16/test20-dra/` (unchanged
  from the plan), and implementation deviations. Landing required an e2e-harness
  provisioning change (feature-gate-aware `kubeadm --config` generation in
  `test/e2e/playbook/provision.yaml`/`Vagrantfile.in`) that was not originally scoped by
  this Step 10 section — the harness had no way to turn on API-server/scheduler/kubelet
  feature gates before this. Also corrected this section's CLOS-association assertion
  example (`ConfigureClos.*ClosID:<hp-clos-id>` → `pct.go`'s per-allocation `"associated
  cpus %s to CLOS %d"` line) and its allocatable-deduction assertion (`node.status.allocatable.cpu`
  deducted by 2 → `pod.status.nodeAllocatableResourceClaimStatuses[]`, the field KEP-5517
  actually mutates) — both were stale/inaccurate, per a finding surfaced while implementing
  the e2e test. Added a note to the Cross-cutting feature-gate-probes section that Step 10's
  test-only probe does not close the still-open production `Plugin.Start` probe gap tracked
  there.
- **2026-08-24 (code-review fixups).** Reworded the feature-gate-probes cross-cutting note (it previously said the probes "must land before Step 8 lands," which contradicted Step 8's own "Landed" line below — the probes never landed, in Step 8 or anywhere else); the note now says they remain a tracked follow-up gap rather than a blocking precondition Step 8 secretly failed to satisfy. Also fixed several Step 8 implementation bugs found in review: `allocateClaim`/`remarkClaimInSupply` now call `cpuClasses.UseClass` so DRA-claimed CPUs actually get their physical cpuClass (SST-CP/EPP/governor) applied, not just pool-accounting exclusion; `allocateClaim` now propagates eviction-reallocation failures as errors (previously only logged) and force-repins any evicted container that couldn't be reallocated off the newly claimed CPUs; claiming CPUs now also triggers `updateSharedAllocations` so already-running shared/burstable containers get re-pinned off CPUs the claim subtracted from the sharable pool; fixed a missing `defaultPrio` reset on one of `Reconfigure`'s four rollback paths.
- **2026-08-24 (later).** Step 8 landed: added "Landed" line with commit range `f0834c77`…`6ce104a6` and implementation deviations — no `AllocateClaim`/`ReleaseClaim` on `Backend`; NRI-only call path (no `Deps` Prepare-path closure); claim UID recovered from CDI device names via `parseCDIClaimUID`/`matchLiveClaimUID` (not the `NRI_CLASS`/`NRI_CPU<N>` env-var protocol); `Plugin.LiveClaimsLocked()` added; `Backend.Stop()`/`PostReconfigure()` added to the `Backend`/`Policy` interfaces. See design.md's CDI env-var protocol / NRI enforcement flow sections, corrected accordingly.
- **2026-08-24 (latest).** Step 10 fleshed out with a concrete scenario: single-file `code.var.sh` test (matching `test19-cpuclass`'s convention, not the earlier sketch's separate static YAML files); reuses `test19-cpuclass`'s `OVERRIDE_SYS_CPUFREQ`/`OVERRIDE_SST`/`OVERRIDE_SST_STATE_DIR` mocks for hardware-independence (no physical PCT/SST-TF silicon needed); `ResourceClaim` selects on the already-landed `nri/pctPriority` attribute only. Feature-gate precondition is all-or-nothing (skip whole test if either KEP-5075 or KEP-5517 is missing), chosen for simplicity over conditionally skipping individual assertions.
- **2026-08-23.** Step 7 landed: added "Landed" line with commit range `148d09b2`…`52b6f182` and implementation deviations (per-result CDI device naming, `WithLock` concurrency model, exported state types, non-HP deferred, `RestoreClaimsLocked`/`RestoreClaims` split, must-not-hold-lock contract). Added non-HP DRA pick to "Not part of v1."
- **2026-08-22.** Class-derived attribute freshness resolved as Option B (design.md resolved decision 8). Removed from "Not part of v1." Added `Plugin.LiveClaimClasses()` to Step 7 (claim tracking infrastructure) and the Reconfigure refusal check to Step 8 (policy wire-up where `Reconfigure` fires).
- **2026-08-19 (later).** Step 6 gained an **Imports & deps** subsection: enumerates must-have Kubernetes helper packages (`kubeletplugin`, `resourceslice`, `resapi`, `types`, `resource`), lists small additions worth pulling in (`client-go/util/retry`, `utils/ptr`), and explicitly documents what we are *not* adopting (klog switch, `component-base/featuregate`, `component-base/metrics`, `runtime/serializer` for checkpoints, `controller-runtime`, low-level DRA/registration protobufs) with rationale. Also codifies "do not import from `dra-example-driver` or `dra-driver-cpu`."
- **2026-08-19.** Initial plan created based on [design.md](design.md) as of that date.
