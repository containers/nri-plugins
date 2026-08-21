# DRA support for cpuClass

**Status:** design draft. 

**Related context:** [`landscape.md`](landscape.md) — general DRA landscape and KEP map. [`pr-536-analysis.md`](pr-536-analysis.md) — analysis of the earlier topology-aware DRA prototype that this design borrows mechanism from. Read those first for the ecosystem background; this file only covers the cpuClass-DRA design.

## Goal

Expose the full nri-plugins `cpuClass` surface (min/max frequency, EPP, freqGovernor, uncore min/max, disabledCstates, PCT priority, SST CLOS binding) via Kubernetes Dynamic Resource Allocation, so that:

- Users can select cpuClasses via `ResourceClaim` (`deviceClassName` and/or CEL on class-derived attributes) rather than only via nri-plugins policy config.
- The kube-scheduler can enforce cpuClass capacity constraints at admission time where such constraints exist — most notably PCT high-priority CPU headroom per SST-TF punit — instead of relying on nri-plugins hint scoring to steer placement after the pod lands.

**Scoped delivery.** v1 targets the **topology-aware policy only**. The balloons policy is a v2 target and gets its own DRA driver (separate driver name, separate ResourceSlices), sharing implementation code with topology-aware wherever possible.

**Core value proposition.** Today `pct.Allocator.FreeClassCapacity()` returns HP-CPU headroom per punit, but it is consumed only by the local nri-plugin allocator to steer placement inside a balloon/pool. The kube-scheduler bin-packs HP pods onto nodes with no idea which nodes have SST-TF-eligible punits with free HP-CPU room — it sees only `node.status.allocatable.cpu`. Result: HP pods land on nodes that cannot deliver top turbo and fall back silently to lower-bucket frequencies. DRA closes this gap by making per-punit HP capacity a scheduler-visible resource. The same DRA plumbing incidentally lets users select non-PCT cpuClasses (EPP tier, governor, uncore freq) via CEL selectors — no admission-time enforcement, but at least a first-class API instead of "trust the operator to configure balloons correctly."

**Non-goals.**

- Integrating with, replacing, or contributing to [kubernetes-sigs/dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu). Reference-only.
- Managing memory or hugepages as DRA. Separate initiative.
- Removing or deprecating the existing hint-based path in `pct.Allocator.Hints()` or the pool-based CPU allocation logic. Coexistence is required.
- Rewriting cpuClass semantics. The DRA driver publishes existing cpuClass definitions; user-facing cpuClass configuration format is unchanged.

## Assumed baseline

- **[KEP-5075](https://github.com/kubernetes/enhancements/issues/5075) (DRA Consumable Capacity)** — `AllowMultipleAllocations` + `DeviceCapacity.RequestPolicy`. Alpha in Kubernetes v1.34+.
- **[KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) (DRA Node Allocatable Resources)** — `Device.NodeAllocatableResourceMappings`, scheduler-side node-allocatable accounting, kubelet-side pod-level cgroup inflation. Alpha in v1.36, alpha2 in v1.37.
- **[KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) (DRA Shared Consumable Capacity)** — pre-alpha, no upstream code yet. Design has an opt-in path for it (Model C below). Not required for v1.
- Existing PCT allocator in `pkg/resmgr/cpuclass/internal/pct/` — reused as-is for HP-room accounting, SST-CP/SST-TF programming, and CLOS association.
- Existing cpuClass application (EPP, governor, uncore freq, disabledCstates) in `pkg/resmgr/cpuclass/` — reused as-is.
- NRI phase remains the sole enforcement point for cpuClass application (CLOS association, EPP writes, governor writes, etc.). DRA covers scheduling admission and cgroup inflation; NRI covers per-container physical policy application.

## Design

### Device shape: one device per (cpuClass × punit)

The natural DRA unit is **(cpuClass × punit)**: one virtual device for each cpuClass defined in policy config, replicated per SST-TF punit for topology locality.

- Each device represents "some CPUs on this punit configured as this cpuClass."
- `AllowMultipleAllocations: true` ([KEP-5075](https://github.com/kubernetes/enhancements/issues/5075)) — many claims can draw from the same device concurrently, each consuming a portion of its capacity.
- Capacity `cpus` with `RequestPolicy: {default: "1", validRange: {min: "1", max: <see below>, step: "1"}}`.
- The `max` in `RequestPolicy` and the reported capacity value depend on the class's priority tier:
  - **HP class** (`pctPriority: "high"` OR assoc-only class targeting the largest-MaxFreq CLOS): `min(GuaranteedHpCpus, punit.CPUs.Size())`.
  - **Non-HP class**: `punit.CPUs.Size() - GuaranteedHpCpus` (LP/generic room on the punit).
- `NodeAllocatableResourceMappings` ([KEP-5517](https://github.com/kubernetes/enhancements/issues/5517)):
  ```yaml
  cpu:
    capacityKey: "nri/cpus"
    allocationMultiplier: "1"
  ```
  One requested `cpus` == one CPU deducted from `node.status.allocatable.cpu`, regardless of class.
- Attributes on the device fall into two groups:
  - **Topology (from punit/system):**
    - `nri/packageID` (int)
    - `nri/punitID` (int)
    - `nri/punitCpuCount` (int)
    - `nri/maxTurboFreqKHz` (int, from SST-TF bucket 0)
    - `resource.kubernetes.io/numaNode` (int or int list per [KEP-6072](https://github.com/kubernetes/enhancements/issues/6072)) — for cross-driver co-placement with GPU/NIC DRA drivers.
  - **Class-derived (from cpuClass config):**
    - `nri/cpuClass` (string) — the class name.
    - `nri/pctPriority` (string, `"high" | "low" | ""`).
    - `nri/energyPerformancePreference` (int, `EnergyPerformancePreference` field).
    - `nri/freqGovernor` (string, `FreqGovernor` field).
    - `nri/minFreqKHz`, `nri/maxFreqKHz` (int, resolved from symbolic `min/base/turbo` via `pct.Allocator.resolveHWFreq`).
    - `nri/uncoreMinFreqKHz`, `nri/uncoreMaxFreqKHz` (int).
    - `nri/disabledCstates` (string list, [KEP-5491](https://github.com/kubernetes/enhancements/issues/5491) dependent — publish as scalar-joined string when [KEP-5491](https://github.com/kubernetes/enhancements/issues/5491) gate is off).
    - `nri/guaranteedHpFreqKHz` (int, HP classes only — programmed HP CLOS max, or bucket-0 frequency in assoc-only).

User's claim (typical, Model B):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: hp-cpus-on-numa0
spec:
  devices:
    requests:
    - name: cpus
      exactly:
        deviceClassName: nri.native.cpu
        capacity:
          requests:
            nri/cpus: "4"
        selectors:
          - cel: |
              device.attributes["nri"].cpuClass == "hp-perf" &&
              device.attributes["resource.kubernetes.io"].numaNode == 0
```

Or purely by class name (relying on a per-class `DeviceClass` shortcut — see the Helm section):

```yaml
      exactly:
        deviceClassName: nri.native.cpu.hp-perf
        capacity: { requests: { nri/cpus: "4" } }
```

### The capacity-sharing problem (multiple classes per (tier, punit))

Model B publishes one device per (class × punit). If two HP classes exist ("hp-perf" and "hp-turbo") both targeting the same punit, each device reports `cpus = GuaranteedHpCpus`. The scheduler treats them as independent capacities and can allocate `2 × GuaranteedHpCpus` HP CPUs on that punit. **Overcommit — turbo-throttled workloads.**

Same structural issue applies to multiple **non-HP** classes on the same punit: the scheduler is misled about non-HP-tier capacity. Node-level `cpu` is still accounted correctly by [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) (each class's `nri/cpus` maps to `node.allocatable.cpu`), so the *node* is not oversubscribed. But at Prepare time the driver may be asked to pick N non-HP CPUs on a punit that has < N free non-HP CPUs. All fallback choices are bad (fail Prepare / steal HP CPUs / cross the topology boundary). Less on-fire than HP throttling, same class of problem.

The constraint is therefore: **any (priority tier, punit) with >1 DRA-published class overcommits its physical pool**. On systems where SST-TF is not supported and there are no punits, the granularity widens to (tier, package); the validation logic is identical.

#### v1 approach (no [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941)): per-class opt-out, refuse-to-start on residual ambiguity

Add a `dra` sub-block to each `cpuClass`:

```yaml
cpuClasses:
- name: hp-perf
  pctPriority: high
  # implicit dra.publish: true — published by default
- name: hp-turbo
  pctPriority: high
  dra:
    publish: false      # opt-out — this class is not exposed via DRA on the topology-aware driver
```

**Field spec:**
- `cpuClass.dra.publish` (bool, default `true`).
- Placement: colocated with the class so "is this class DRA-visible?" is answered where the class is defined, not in a separate list that can drift.
- Extensibility: the `dra` sub-block gives us a home for future per-class DRA config without polluting the top-level class fields.

**Validation at driver Configure** — implemented in `pkg/resmgr/cpuclass/dra.go:ValidateCPUClassesForDRA`:

v1 validates at **tier granularity only**, not per-(tier, punit), because `CPUClass` carries no punit affinity — the per-class→punit mapping only exists at runtime. Per-punit enforcement is deferred to the device-build step (Step 5) where runtime punit topology is available. Tier classification is static from config: managed PCT → `"pctPriority=<value>"`; assoc-only PCT → `"closID=<N>"`; non-PCT classes are exempt from the tier check (they have no turbo-frequency tier concept) but are still published by Step 5 when `DRAPublish() == true`.

1. Collect the set of DRA-published PCT classes (those with `PctPriority != ""` or `SstClosID != nil` and `DRAPublish() == true`).
2. For each tier label, count published classes.
3. If any tier has `count > 1` and `TopologyAwarePolicy.spec.dra.sharedCounters == false`, **refuse to start** with an error naming the tier, the conflicting classes (sorted), and pointing at the two resolutions:
   - Opt out on all but one via `cpuClass.dra.publish: false`, **or**
   - Enable `TopologyAwarePolicy.spec.dra.sharedCounters: true` (requires [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) in the cluster).

**Rationale for default `publish: true`:**
- Single-HP-class deployments (the common case) require zero configuration.
- Multi-HP-class deployments are forced to make an explicit decision at config time rather than encountering a silent overcommit at runtime.
- Failing loudly at Configure is preferable to silently dropping classes ("first-configured wins") or trusting operators to keep runtime discipline ("only claim one HP class at a time").

**Non-DRA workloads unaffected.** A class with `dra.publish: false` is still fully usable via the existing nri-plugins config path (annotations, pod labels, whatever the policy exposes today). Only its DRA-visibility is suppressed.

#### [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) approach (opt-in via config)

Publish a per-punit `sharedCounters` set:

```yaml
sharedCounters:
- name: "punit-P<pkg>-U<punit>"
  counters:
    hp-cpus:
      value: <GuaranteedHpCpus>
      requestPolicy: { default: "1", validRange: { min: "1", max: <GuaranteedHpCpus>, step: "1" } }
    lp-cpus:
      value: <punit.CPUs.Size() - GuaranteedHpCpus>
      requestPolicy: { default: "1", validRange: { min: "1", max: <...>, step: "1" } }
```

Each (class × punit) device declares `consumesCounters` referencing the appropriate counter via `valueFrom.capacityKey: nri/cpus`. Multiple HP classes on the same punit all draw from the same `hp-cpus` counter. Scheduler prevents aggregate overcommit, and the per-class opt-out described above is no longer required for correctness — it only remains a knob to hide classes from DRA entirely.

This is Model C in earlier drafts. The (class × punit) device shape is the *same* as Model B — Model C only adds the `consumesCounters` / `sharedCounters` structural fields. Both models can coexist on the same driver with a config switch.

### Where the code lives

The DRA driver code sits in `pkg/resmgr/` (used by every policy binary) but is instantiated *per policy* with a per-policy driver name. v1 wires it up only in the topology-aware policy binary; v2 wires the same shared code into balloons with a different driver name.

| Component | Location | Notes |
|-----------|----------|-------|
| Shared DRA driver + kubelet plugin | `pkg/resmgr/dra/` (new package) | Reuse [PR #536](https://github.com/containers/nri-plugins/pull/536)'s `pkg/resmgr/dra.go` as a starting template but split into a package. Policy-agnostic. Driver name is passed in at construction. |
| Kubernetes client / watch | `pkg/kubernetes/client/`, `pkg/kubernetes/watch/` | Lift from [PR #536](https://github.com/containers/nri-plugins/pull/536)'s split-out of `pkg/agent`. |
| Device construction | `pkg/resmgr/cpuclass/` (extend, not `pct/`) | New method `cpuclass.Manager.DRADevices()` that emits per-(class × punit) devices. `pct.Allocator.DRADevices()` becomes an internal helper for HP-tier capacity computation. |
| Prepare/Unprepare handlers | `pkg/resmgr/dra/` | On `Prepare`, call `cpuclass.Manager.PickCpus(className, punitID, N)` (new method) to select concrete CPUs; return CDI env vars. On `Unprepare`, `ReleaseCpus`. Policy-agnostic. |
| Policy wiring (v1) | `cmd/plugins/topology-aware/main.go` + `.../policy/` | Instantiate `dra.Plugin` with driver name `nri.topology-aware.cpu`. Register a claim-allocation callback that lets topology-aware's `allocateResources` see DRA-preselected CPUs. |
| Policy wiring (v2) | `cmd/plugins/balloons/` | Same shared driver, driver name `nri.balloons.cpu`. Deferred to v2. |
| NRI enforcement | existing `cmd/plugins/topology-aware/policy/pools.go` (v1); `cmd/plugins/balloons/policy/balloons-policy.go` (v2) | On `CreateContainer`, new helper parses env vars (see below) and treats those CPUs as claim-pre-allocated. Existing `cpuclass` application path (`UseClass()`, EPP writer, gov writer, PCT `AssociateCPUs`) runs unchanged. |
| Config surface | `pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/config.go` (v1); `.../balloons/config.go` (v2) | New `dra` block under the policy's config. Not shared with other policies — each policy owns its own switch. |
| Helm chart | `deployment/helm/topology-aware/templates/` (v1); `deployment/helm/balloons/templates/` (v2) | Add RBAC, plugin/plugin-registry mounts, per-policy `DeviceClass` objects. Reuse pattern from [PR #536](https://github.com/containers/nri-plugins/pull/536). |

### CDI env-var protocol

Following [PR #536](https://github.com/containers/nri-plugins/pull/536)'s mechanism (which remains valid, see [pr-536-analysis.md](pr-536-analysis.md)). Two env vars per DRA-claim result:

- `NRI_CLASS=<cpuClass name>` — one per claim result.
- `NRI_CPU<N>=1` for each selected CPU ID N in the class allocation.

Renamed from [PR #536](https://github.com/containers/nri-plugins/pull/536)'s `DRA_CPU<N>=1` to `NRI_CPU<N>=1` because `NRI_CLASS` is new and the pair should share a prefix. Existing [PR #536](https://github.com/containers/nri-plugins/pull/536) downstream forks can be updated by search/replace.

The NRI-side handler (`getClaimedCPUs` / equivalent) returns both the CPU set and the class name. The policy allocates those CPUs into the appropriate pool/balloon *and* applies the cpuClass to them (which includes SST CLOS association, EPP write, governor write, uncore freq write).

### Publish flow (at plugin start + on Reconfigure)

1. Policy config parsed; cpuClasses registered with `cpuclass.Manager`.
2. `pct.Allocator.Configure()` runs, populating punits and HP-tier capacity.
3. If `dra.enabled` config flag is set:
   - `cpuclass.Manager.DRADevices()` returns `[]resapi.Device` in the selected model shape (Model B by default, Model C if `dra.sharedCounters: true` **and** [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) feature-gate is detected).
   - Driver calls `kubeletplugin.PublishResources` with one pool named after the node, containing N slices (at most `ResourceSliceMaxDevices` devices per slice).
4. On `Reconfigure`, diff and re-publish. Class definitions changed → devices re-emitted. `GuaranteedHpCpus` changed (operator changed SST-TF buckets) → capacities re-emitted.

**Note: "publish on Reconfigure" is a nri-plugins-specific requirement.** Both reference drivers we examined ([kubernetes-sigs/dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu) and [kubernetes-sigs/dra-example-driver](https://github.com/kubernetes-sigs/dra-example-driver)) publish ResourceSlices exactly once at process start and never re-publish. Config changes there mean DaemonSet restart. Live-reconfigure republication is where our design does something the reference drivers do not, and is the root of open decision 1 (class-derived attribute freshness). See [landscape.md](landscape.md) for the reference-driver patterns.

### Prepare flow (per claim)

1. Scheduler at Filter/Reserve time picks a specific (class × punit) device on this node and computes `consumedCapacity: {nri/cpus: N}`. [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) deducts N from `node.status.allocatable.cpu`. Under Model C, N is also deducted from the appropriate `sharedCounters` counter.
2. Scheduler at PreBind writes `pod.status.nodeAllocatableResourceClaimStatuses`.
3. Kubelet calls `PrepareResourceClaims(claim)` on the driver.
4. Driver locks the resmgr and calls `cpuclass.Manager.PickCpus(className, punitID, N)` — selects N CPUs from that punit that fit the class's requirements, updates `hpUsed[punitID]` if HP.
5. Driver writes a CDI spec at `/var/run/cdi/nri.native.cpu.yaml` with:
   ```yaml
   - name: claim-<uid>
     containerEdits:
       env:
         - NRI_CLASS=<className>
         - NRI_CPU<id0>=1
         - NRI_CPU<id1>=1
         # ...
   ```
6. Returns `PrepareResult` with `CDIDeviceIDs: ["nri.native.cpu/device=claim-<uid>"]`.
7. Driver does **not** call `sst.AssociateCPUs`, `WriteEPP`, or governor writes here. All physical cpuClass application is deferred to the NRI phase.

### NRI enforcement flow (unchanged trust boundary)

1. NRI `CreateContainer` fires. Policy reads container env.
2. Helper parses `NRI_CLASS` and `NRI_CPU<N>=1` env vars; returns `(className, cpuset, residualNativeCpuCount)`.
3. Existing pool/balloon allocator treats the DRA-preselected CPUs as claim-pre-allocated (analogous to [PR #536](https://github.com/containers/nri-plugins/pull/536)'s `getClaimedCPUs`).
4. Existing `cpuclass.Manager.UseClass(className, cpuset)` applies all class properties: SST CLOS via `pct.Allocator.UseClass()`, EPP via `cpuclass.writeEPP()`, governor via cpufreq, uncore freq, disabled Cstates.

### Release flow

1. Kubelet calls `UnprepareResourceClaims`.
2. Driver calls `cpuclass.Manager.ReleaseCpus(className, punitID, cpuIDs)` — releases the HP-tier accounting hold (via `pct.Allocator.ReleaseHpCpus`) if HP.
3. Driver removes the CDI spec entry for this claim.
4. No physical cpuClass "unset" is done. The next container to use these CPUs will re-apply whatever class it belongs to.

### Coexistence with the existing hint path

Same as prior draft: both DRA-claimed and non-DRA workloads share `hpUsed` and `cpuclass.Manager` state. `FreeClassCapacity()` reflects DRA holds. Concurrent Prepare and NRI-phase paths synchronize on the resmgr lock.

### Feature-gate detection

Same as prior draft:

- `AllowMultipleAllocations` off → refuse to start (probe test-slice; fail if API server strips the field). Alternatively require operator to assert `dra.requireConsumableCapacity: true`.
- `NodeAllocatableResources` off → mapping ignored, users must mirror CPU count in `spec.cpu`. Log warning. Not fatal.
- [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) fields (`sharedCounters.requestPolicy`, `consumesCounters.valueFrom`) → same probe; refuse Model C startup if not present.

### Helm chart additions

RBAC:
```yaml
- apiGroups: ["resource.k8s.io"]
  resources: [resourceslices]
  verbs: [list, watch, create, update, delete]
- apiGroups: ["resource.k8s.io"]
  resources: [resourceclaims, deviceclasses]
  verbs: [get]
- apiGroups: ["resource.k8s.io"]
  resources: [resourceclaims/status]
  verbs: [patch, update]
```

Volume mounts (host):
- `/var/lib/kubelet/plugins` → `/var/lib/kubelet/plugins`
- `/var/lib/kubelet/plugins_registry` → `/var/lib/kubelet/plugins_registry`
- `/var/run/cdi` → `/host/var/run/cdi`

`DeviceClass` objects — two levels:

Driver name is per-policy — `nri.topology-aware.cpu` in v1, `nri.balloons.cpu` planned for v2. Two policies deploying to the same cluster do not clash because their driver names differ.

**Base class per policy** (v1 ships one; v2 adds a second):
```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: nri.topology-aware.cpu
spec:
  selectors:
    - cel:
        expression: device.driver == "nri.topology-aware.cpu"
```

**Per-cpuClass shortcut** (optional, one per cpuClass defined in policy config — chart-generated or admin-installed):
```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: nri.topology-aware.cpu.<className>
spec:
  selectors:
    - cel:
        expression: |
          device.driver == "nri.topology-aware.cpu" &&
          device.attributes["nri"].cpuClass == "<className>"
```

This lets users write `deviceClassName: nri.topology-aware.cpu.hp-perf` instead of the CEL boilerplate.

## Resolved decisions

1. **Scope of `cpuClass` exposure — full cpuClass.** (Resolved 2026-08-19.) All cpuClass fields become device attributes; capacity is `cpus` with tier-based enforcement (HP = punit HP capacity; non-HP = punit non-HP capacity).
2. **[KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) target — aspirational; ship without it.** (Resolved 2026-08-19.) Model B ships in v1 without shared counters. Model C is opt-in via config once [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) lands in alpha.
3. **Policy scope — topology-aware first, balloons in v2, shared code.** (Resolved 2026-08-19.) Driver code is factored so both policies use the same implementation; wiring and driver name differ per policy. v1 delivers topology-aware only.
4. **Driver name — per-policy, `nri.<policy>.cpu`.** (Resolved 2026-08-19.) v1: `nri.topology-aware.cpu`. v2: `nri.balloons.cpu`. Two policies never share a driver name — a node running both plugins publishes two independent DRA drivers.
5. **Config surface — per-policy `dra` block.** (Resolved 2026-08-19.) v1: `TopologyAwarePolicy.spec.dra: { enabled: true, sharedCounters: false }`. v2: `BalloonsPolicy.spec.dra: { ... }` with the same shape. Each policy owns its own switch; nothing shared at the resmgr level.
6. **v2 code-sharing verification checklist.** (Resolved 2026-08-19.) The v2 balloons integration is deemed to correctly reuse v1 code when all of the following hold:
   - `pkg/resmgr/dra/` package has no imports from `cmd/plugins/topology-aware/`.
   - `cpuclass.Manager.DRADevices()` / `PickCpus()` / `ReleaseCpus()` signatures are policy-agnostic.
   - v2 wire-up delta is confined to `cmd/plugins/balloons/main.go`, the balloons policy config CR, and the Helm chart.
   - No copy-pasted DRA logic anywhere.

   Applies at v2 review time and to any subsequent policy integration.
7. **Multiple published classes per (tier, punit) without [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941).** (Resolved 2026-08-19.) Add `cpuClass.dra.publish` (bool, default `true`). Driver validates at Configure: if any (priorityTier, punitID) has >1 published class **and** `TopologyAwarePolicy.spec.dra.sharedCounters == false`, refuse to start with an error naming the conflict and its two resolutions (opt-out via `dra.publish: false`, or enable `sharedCounters`). Applies to both HP and non-HP tiers; on non-SST-TF systems the granularity widens to (tier, package). Under [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) (Model C) the constraint is lifted; `dra.publish: false` remains as an "hide from DRA" knob.

## Open decisions

1. **Class-derived attribute freshness.** Attributes like EPP, governor, uncore freq depend on cpuClass config. If a user updates policy config while pods are running, the driver re-publishes ResourceSlices — but claims in flight might see stale attributes. Design assumes existing policy `Reconfigure` semantics apply. Needs confirmation from maintainers on how strict a guarantee to make. Solution space and recommendation are detailed below.

## Discussion: class-derived attribute freshness

Draft notes for maintainer review. Move relevant pieces into resolved decisions once direction is chosen.

### Problem

A ResourceSlice is a snapshot of `[]Device` at some instant. The scheduler treats device attributes as truthful for that snapshot. If `cpuClass` config changes and the driver re-publishes, there is a window between "old slice was observed by the scheduler" and "new slice replaces it" during which three failure modes can occur:

1. **In-flight claims allocated against stale attributes.** User's claim selects `device.attributes["nri"].energyPerformancePreference == 0`. Class `hp-perf` had `EPP: 0`. Operator reconfigures it to `128`. Scheduler's cached ResourceSlice still shows `EPP: 0`; Filter/Reserve matches on the old value. Kubelet then calls Prepare on the driver. The class the CDI env vars point at now has `EPP: 128`. Workload gets CPUs whose EPP does not match its selector. The scheduler contract ("I gave you what your selector asked for") is silently violated.
2. **Already-running claims see attribute drift.** No re-Prepare happens for existing containers on config change. Users comparing ResourceSlice attributes against running claims see mismatch. Cosmetic, but hurts observability.
3. **Class deletion while its CPUs are DRA-claimed.** Class removed from policy config → class-derived attributes disappear from next slice → new claims can't select the old class. Existing containers' CDI env vars still say `NRI_CLASS=hp-perf`. NRI-phase `cpuclass.Manager.UseClass("hp-perf", ...)` calls fail or no-op silently.

Failure mode 1 is the substantive scheduler-contract issue. 2 is cosmetic. 3 is genuinely broken and needs an explicit answer.

### Solution options (sorted least to most intrusive)

**A. Document as a known limitation. Do nothing at runtime.**
Docs say: "cpuClass config changes propagate to new claims via ResourceSlice re-publication. Claims already in flight or already prepared retain the attributes they were allocated against. To force re-selection, delete and recreate the claim." Operators schedule config changes during maintenance windows.
- Pro: zero code.
- Con: silent contract violation possible; debugging surface is "why did my claim get EPP=128 when I asked for EPP=0?"

**B. Refuse Reconfigure that changes class-derived attributes if any claim is currently allocated for the affected class.**
Configure re-runs at reconfigure time. Before it commits, driver checks whether class-derived attributes actually differ from the currently-published slice and whether affected classes have live claims. If yes, refuse the reconfigure and error to the operator.
- Pro: hard guarantee against silent drift.
- Con: breaks operator workflow (can't change class config until DRA-claiming pods are gone). "Live claim" check requires the driver to track claim state across restarts (design already does via CDI-reflection à la `dra-driver-cpu.Synchronize`).

**C. Publish class-derived attributes under a version attribute.**
Every re-publish increments `nri/classGeneration` on affected devices. Users can pin generation in CEL if they care. Makes drift observable but doesn't prevent it.
- Pro: cheap, additive, surfaces the issue.
- Con: users who don't pin generation still see the contract violation. Debugging aid, not a fix.

**D. Split class attributes into "structural" (safe to change) vs "identifying" (require class rename to change).**
Numeric bounds and enum tags describing the class *can* change under Reconfigure and re-publish updates them. To change identifying fields like `pctPriority` (which reclassifies the tier), operator must rename the class — old class retired, new class added.
- Pro: limits the sharp edge to a small set of fields operators explicitly treat as immutable.
- Con: designer/operator burden; the distinction is subtle; users will disagree on which fields are identifying (one user's structural is another's identifying).

**E. Prepare-time validation against the resolved class.**
When Prepare runs, driver rebuilds class attributes from the *current* cpuClass definition and compares against what the claim was allocated with (via `Status.Allocation.Devices.Results[].DeviceRequest`). Mismatch → Prepare fails with a clear error, pod stays Pending, user recreates the claim.
- Pro: scheduler contract honored — if we can't fulfill it, we refuse rather than silently deliver wrong attributes. Failure is explicit and actionable.
- Con: requires recording per-class attribute snapshot at publish time. Still has an unavoidable race between scheduler-observes-slice and driver-Prepares.

**F. Combine: E + A + class-refcount for deletion.**
Enforce sharp guarantees at Prepare time (E), document the residual race (A), and additionally: when a class is deleted from config while claims reference it, driver keeps the class definition alive internally until all live claims for it are released ("retired class" — used by NRI-time application but hidden from new DRA publishes).
- Pro: covers failure modes 1 and 3 explicitly, honest about the residual race.
- Con: most code (per-class snapshotting, refcount tracking).

### Recommendation

**Option F.** Rationale:

- The two sharp edges are (1) silent contract violation on Prepare and (3) class deletion. F addresses both.
- Converts a class of hard-to-debug silent bugs (wrong EPP/governor on a workload that "looked right") into a class of explicit errors the user can act on (Prepare fails with a message pointing at the changed class).
- nri-plugins users historically care about turbo/EPP guarantees (that's what PCT exists for). Being loud beats being fast.

Concrete mechanics for F:
- **At publish:** driver records `classSnapshot[className] = attributes` at each publish.
- **At Prepare:** driver reconstructs the attribute set from the current class definition and compares against the claim's `Status.Allocation.Devices.Results[].DeviceRequest`. Mismatch → refuse Prepare with an error message telling the user to recreate the claim.
- **At Reconfigure with class removal:** any deleted-but-still-referenced class stays alive internally as a "retired class." Retired classes are used by NRI-phase application but are excluded from new DRA publishes. Refcount = live claims. When refcount hits zero, class definition is dropped.
- **Documented residual race:** the "operator reconfigures between scheduler-observes-slice and driver-Prepare" window remains but is now a loud Prepare failure rather than silent attribute drift.

### Reference: how `dra-driver-cpu` avoids this problem

[kubernetes-sigs/dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu) sidesteps class-freshness entirely by **not supporting live reconfigure**. Config is loaded once at process start ([`cmd/dracpu/app.go:112`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/cmd/dracpu/app.go#L112) — `driverconfig.Resolve` from file + flags), and any config change requires a DaemonSet restart. Their own docs make this explicit ([`hack/examples/sysfs-overlay/README.md`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/hack/examples/sysfs-overlay/README.md)).

That approach is not open to us: nri-plugins already supports live `Reconfigure` at the resmgr level, and giving that up on the DRA path would be an operator-ergonomics regression. The freshness problem is therefore genuinely ours to solve, not something we can defer to "restart the pod." This rules out an implicit "option G — do what `dra-driver-cpu` does" and moves options A–F above from "possible answers" to "the only answers we have."

### Fallback

If maintainers consider F too invasive for v1, the acceptable simpler choices are:

- **A alone** — accept the sharp edge, treat cpuClass config changes as maintenance-window operations, document loudly.
- **A + C** — publish a `classGeneration` attribute so operators can audit whether their running claims are on the current class definition; sophisticated users can pin generation in CEL.

**B** (refuse Reconfigure with live claims) is not recommended because it collides with the resmgr's existing Reconfigure semantics — the rest of the plugin already handles live config changes and the DRA driver blocking them creates surprising coupling.

**D** (structural vs identifying) is not recommended because the split is subjective and hard to document.

## Change log

- **2026-08-19 (latest).** Added note to "Publish flow" that "publish on Reconfigure" is a nri-plugins-specific requirement; both reference DRA drivers (`dra-driver-cpu` and `dra-example-driver`) publish exactly once at start and never re-publish. Reinforces the framing that class-freshness (open decision 1) is a problem we're inventing by supporting live Reconfigure.
- **2026-08-19.** Added "Reference: how `dra-driver-cpu` avoids this problem" to the class-freshness discussion. Their approach — no live reconfigure, restart-the-DaemonSet on config change — is not available to us because nri-plugins already supports live `Reconfigure`. Options A–F remain the answer space.
- **2026-08-19.** Multi-class overcommit resolved: introduced `cpuClass.dra.publish` (bool, default true) and Configure-time validation that refuses to start on any (tier, punit) with >1 published class unless `sharedCounters` is on. Old "v1-a first-configured wins" text replaced. Non-HP tier explicitly covered alongside HP.
- **2026-08-19.** v2 code-sharing verification checklist promoted to resolved decision — applied at v2 review time and to any subsequent policy integration.
- **2026-08-19.** Config surface pinned as `<Policy>.spec.dra` per-policy block. Moved from open to resolved decisions.
- **2026-08-19 (later).** Policy scope narrowed to topology-aware for v1, balloons deferred to v2. Driver name changed from single `nri.native.cpu` to per-policy `nri.topology-aware.cpu` / `nri.balloons.cpu`. Shared DRA driver code moved to a new `pkg/resmgr/dra/` package (from a single `pkg/resmgr/dra.go` file) to make policy-agnostic reuse structural, not conventional. Filename renamed to `design.md`.
- **2026-08-19.** Scope expanded from PCT-priority-only to full cpuClass. Driver relocated from `pkg/resmgr/cpuclass/internal/pct/` to `pkg/resmgr/cpuclass/`. Device shape moved from per-punit-with-two-capacities (hp-cpus/lp-cpus) to per-(class × punit)-with-single-capacity (cpus). Multi-class overcommit problem introduced and mitigated with v1-a strategy pending [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941). Driver name pinned to `nri.native.cpu` (superseded).


