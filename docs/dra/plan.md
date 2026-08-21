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

**Files touched:** `pkg/resmgr/dra/{plugin,cdi,state}.go`, unit tests, integration tests.

**Verification:** integration test — publish, receive fake `PrepareResourceClaims` call, assert CDI spec written correctly, assert `hpUsed` updated. Restart-with-existing-claim test.

**Risk:** high. Correctness of concurrent Prepare + NRI-phase access to `hpUsed` ([design.md](design.md) "Coexistence" section) must be locked down.

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
- Extend `TopologyAwarePolicy` config CR with `spec.dra { enabled: bool, sharedCounters: bool }`.
- Add `spec.dra.enabled: false` to all existing test configs to keep behavior unchanged.

**Files touched:** `cmd/plugins/topology-aware/main.go`, `cmd/plugins/topology-aware/policy/pools.go`, `pkg/resmgr/policy/policy.go`, `pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/config.go`, CRD regeneration.

**Verification:** e2e test — deploy topology-aware with `dra.enabled: true`, create a `ResourceClaim` selecting an HP class, verify pod gets the expected CPUs at CLOS/EPP/etc.

**Risk:** high. Same class of concurrency and eviction hazards as [PR #536](https://github.com/containers/nri-plugins/pull/536) flagged (`TODO: sort old grants by QoS class`). Prototype scars need cleanup before merge, not after.

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

### Step 10 — e2e test

**Rationale.** Prove the full path end-to-end. Follows the pattern of existing `test/e2e/policies.test-suite/topology-aware/n4c16/test*/`.

**Actions.**
- New test directory `test/e2e/policies.test-suite/topology-aware/n4c16/testXX-dra/`:
  - `config.yaml` — topology-aware config with `dra.enabled: true`, one HP cpuClass, one non-HP.
  - `resourceclaim.yaml` — claim requesting `hp-cpus: 2` on numa 0.
  - `pod.yaml` — pod referencing the claim.
  - `code.var.sh` — assertions: pod runs, its CPUs are on numa 0, HP CLOS association is visible via SST tooling, cgroup pod-level `cpu.max` inflated per [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517).
- If cluster lacks required feature gates, skip with a clear message.

**Files touched:** `test/e2e/policies.test-suite/topology-aware/n4c16/testXX-dra/*`.

**Verification:** e2e test passes on the [KEP-5075](https://github.com/kubernetes/enhancements/issues/5075)+5517 test cluster.

**Risk:** medium. Depends on cluster support for the feature gates; the CI environment may need updating.

## Cross-cutting

### Feature gate probes

Design.md "Feature-gate detection" spec'd probe-at-startup for `AllowMultipleAllocations`, `NodeAllocatableResources`. Implement in step 6 as part of `Plugin.Start`. Land the probes even without [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) (Model C stays not-yet-implemented) so the code path exists when Model C is added.

⚠️ Feature-gate probes (`AllowMultipleAllocations`, `NodeAllocatableResources`) deferred from Step 6 to a follow-up task; add before Step 8 lands.

⚠️ Without the probe, DRA-published capacity fields that require `NodeAllocatableResources` will be silently stripped by the API server — acceptable for the stub phase.

### Not part of v1

Explicitly deferred to later work, tracked here so nobody accidentally scope-creeps a step:
- Balloons policy wire-up (v2).
- Model C / [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) shared counters (waits for upstream alpha).
- Class-derived attribute freshness resolution (open decision 1 in [design.md](design.md); implementation depends on which option maintainers pick).
- `cpuClass.dra` sub-block fields beyond `publish` (custom `capacityKey`, per-class DeviceClass suppression).
- PodResources API integration for DRA-claimed CPUs ([KEP-3695](https://github.com/kubernetes/enhancements/issues/3695); complementary but independent).

### Testing philosophy

- Steps 1–5: unit tests only, no live cluster needed.
- Steps 6–7: integration tests against a fake kubelet-plugin harness; still no live cluster.
- Steps 8–10: e2e cluster required, [KEP-5075](https://github.com/kubernetes/enhancements/issues/5075) and [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) feature gates on.

Splitting this way means the first 5 PRs can land and be reviewed in parallel with cluster-setup work, and any KEP-graduation delays only affect the last three steps.

## Change log

- **2026-08-19 (later).** Step 6 gained an **Imports & deps** subsection: enumerates must-have Kubernetes helper packages (`kubeletplugin`, `resourceslice`, `resapi`, `types`, `resource`), lists small additions worth pulling in (`client-go/util/retry`, `utils/ptr`), and explicitly documents what we are *not* adopting (klog switch, `component-base/featuregate`, `component-base/metrics`, `runtime/serializer` for checkpoints, `controller-runtime`, low-level DRA/registration protobufs) with rationale. Also codifies "do not import from `dra-example-driver` or `dra-driver-cpu`."
- **2026-08-19.** Initial plan created based on [design.md](design.md) as of that date.
