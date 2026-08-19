# DRA support for Priority Core Turbo (PCT)

**Status:** design draft — open decisions listed at the bottom. Update in place as decisions are made; add a "Change log" section when it graduates to implementable.

**Related context:** [CLAUDE.md at repo root](../../CLAUDE.md) — DRA landscape, PR #536 analysis, KEP map. Read that first for the general DRA background; this file only covers the PCT-specific design.

## Goal

Expose the nri-plugins PCT capability (Intel Speed Select — Core Priority / Turbo Frequency) via Kubernetes Dynamic Resource Allocation, so that the kube-scheduler can enforce PCT high-priority CPU headroom at admission time instead of relying on nri-plugins hint scoring to steer placement.

**Core value proposition.** Today `pct.Allocator.FreeClassCapacity()` returns HP-CPU headroom per punit, but it is consumed only by the local nri-plugin allocator to steer placement inside a balloon/pool. The kube-scheduler bin-packs HP pods onto nodes with no idea which nodes have SST-TF-eligible punits with free HP-CPU room — it sees only `node.status.allocatable.cpu`. Result: HP pods land on nodes that cannot deliver top turbo and fall back silently to lower-bucket frequencies. DRA + PCT closes this gap by making per-punit HP capacity a scheduler-visible resource.

**Non-goals.**

- Integrating with, replacing, or contributing to `kubernetes-sigs/dra-driver-cpu`. The design uses it as an architectural reference only.
- Managing memory or hugepages as DRA. Separate initiative, tracked outside this doc.
- Exposing every `cpuClass` knob (EPP, freqGovernor, uncore freq, disabledCstates) as a DRA capacity. See open decision (1).
- Removing or deprecating the existing hint-based path in `pct.Allocator.Hints()`. Coexistence is required.

## Assumed baseline

- **KEP-5075 (DRA Consumable Capacity)** — `AllowMultipleAllocations` + `DeviceCapacity.RequestPolicy`. Alpha in Kubernetes v1.34+.
- **KEP-5517 (DRA Node Allocatable Resources)** — `Device.NodeAllocatableResources.Mapping`, scheduler-side node-allocatable accounting, kubelet-side pod-level cgroup inflation. Alpha in v1.36, alpha2 in v1.37.
- **KEP-5941 (DRA Shared Consumable Capacity)** — pre-alpha, no upstream code yet, but *may* be assumed available as a stretch target. Design has an opt-in path for it.
- Existing PCT allocator in `pkg/resmgr/cpuclass/internal/pct/` — reused as-is for HP-room accounting, SST-CP/SST-TF programming, and CLOS association.
- NRI phase remains the sole enforcement point for actual CLOS association. DRA covers scheduling admission and cgroup inflation; NRI covers per-container CLOS `AssociateCPUs`.

## Design

### Model — one device per punit, opt-in per-CPU children (Model B, extendable to Model C)

**Model B (default, KEP-5075 + KEP-5517):** publish one DRA device per SST-TF punit. Each punit device has:

- `AllowMultipleAllocations: true` (KEP-5075) — a single punit device can be allocated to many claims concurrently.
- Two consumable capacities:
  - `hp-cpus` — capacity value equals `GuaranteedHpCpus` for the punit; `RequestPolicy: {default: "1", validRange: {min: "1", max: <GuaranteedHpCpus>, step: "1"}}`.
  - `lp-cpus` — capacity value equals `punit.CPUs.Size() - GuaranteedHpCpus`; same shape.
- Attributes:
  - `nri/punitID` (int) — the SST-TF punit ID.
  - `nri/packageID` (int) — the physical package.
  - `nri/maxTurboFreqKHz` (int) — SST-TF bucket 0 frequency.
  - `nri/guaranteedHpFreqKHz` (int) — programmed HP CLOS max, or bucket-0 frequency in assoc-only.
  - `nri/punitCpuCount` (int).
  - `resource.kubernetes.io/numaNode` (int or int list per KEP-6072) — for cross-driver co-placement with GPUs/NICs.
- `NodeAllocatableResources` (KEP-5517):
  ```
  cpu:
    mapping:
      capacityKey: "nri/hp-cpus"
      capacityMultiplier: "1"
  ```
  One requested `hp-cpus` == one CPU deducted from `node.status.allocatable.cpu`. Same mapping is added for `lp-cpus` in a separate `NodeAllocatableResource` entry — user's total CPU deduction is `hp-cpus + lp-cpus` regardless of which they requested.

User's claim (Model B):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: hp-cpus-numa0
spec:
  devices:
    requests:
    - name: hp
      exactly:
        deviceClassName: nri.native.cpu
        capacity:
          requests:
            nri/hp-cpus: "4"
        selectors:
          - cel: 'device.attributes["resource.kubernetes.io"].numaNode == 0'
```

**Model C (opt-in via config, KEP-5941 required):** the punit device becomes a *parent* structural node (NOT `AllowMultipleAllocations`), plus per-CPU child devices are published. Each child device:

- Represents one specific CPU.
- Attributes: full topology (`nri/coreType: "P-core"|"E-core"`, `nri/cacheNID`, `nri/isolated`, `nri/baseFreqKHz`, `nri/pctPriority: "high"|"low"|""`).
- `consumesCounters` referencing the parent punit's `sharedCounters` set, with `valueFrom.capacityKey: nri/hp-cpus` on the `hp-cpus` counter (KEP-5941) — mapping "request one child CPU with `pctPriority: high`" to "decrement 1 from the parent punit's `hp-cpus` shared counter."

User's claim (Model C):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: hp-e-core-numa0
spec:
  devices:
    requests:
    - name: hp
      exactly:
        deviceClassName: nri.native.cpu
        count: 2
        selectors:
          - cel: |
              device.attributes["nri"].pctPriority == "high" &&
              device.attributes["nri"].coreType == "E-core" &&
              device.attributes["resource.kubernetes.io"].numaNode == 0
```

Scheduler enforces "no more than `GuaranteedHpCpus` HP CPUs per punit" via the shared counter regardless of which specific CPUs are selected.

Model B and Model C use different `[]resapi.Device` shapes and are **not both valid on the same node at the same time** — a punit device that is both `AllowMultipleAllocations: true` and a parent-of-children is not a legal DRA object shape. Config chooses one.

### Where the code lives

| Component | Location | Notes |
|-----------|----------|-------|
| DRA driver + kubelet plugin | `pkg/resmgr/dra.go` | Reuse PR #536's shape as a template. Registers as driver `nri.native.cpu`. |
| Kubernetes client / watch | `pkg/kubernetes/client/`, `pkg/kubernetes/watch/` | Lift from PR #536's split-out of `pkg/agent`. |
| DRA-device construction | new method `pct.Allocator.DRADevices()` in `pkg/resmgr/cpuclass/internal/pct/` | Emits per-punit devices from cached `punits` state. |
| Prepare/Unprepare handlers | `pkg/resmgr/dra.go` | On `Prepare`, call `pct.Allocator.PickHpCpus(punitID, N)` (new method) to select concrete CPUs; return CDI env vars `NRI_HP_CPU<N>=1`. On `Unprepare`, return capacity via a new `pct.Allocator.ReleaseHpCpus(punitID, cpuIDs)`. |
| NRI enforcement | existing `cmd/plugins/topology-aware/policy/pools.go` + `cmd/plugins/balloons/policy/balloons-policy.go` | On `CreateContainer`, existing code parses `NRI_HP_CPU<N>=1` env vars (analogous to PR #536's `DRA_CPU<N>=1`) and treats those CPUs as claim-pre-allocated. Existing `pct.Allocator.UseClass()` associates them to the HP CLOS. |
| Config flag | `pkg/apis/config/v1alpha1/resmgr/policy/` | New optional field, e.g. `pct.publishAsDRA: true` (plus `pct.dra.mode: punit \| individual` for Model B vs C). |
| Helm chart | `deployment/helm/topology-aware/templates/` | Add RBAC (`resourceslices` CRUD, `resourceclaims` get, `resourceclaims/status` patch), plugin/plugin-registry mounts, `DeviceClass` object `nri.native.cpu`. Reuse from PR #536. |

### Publish flow (at plugin start + on Reconfigure)

1. `pct.Allocator.Configure()` runs as today, populating `a.punits` with `GuaranteedHpCpus` per SST-TF punit.
2. If `pct.publishAsDRA` is set AND at least one cpuClass has `pctPriority: "high"` AND `pct.Allocator.Active()`, the resmgr instantiates the DRA plugin.
3. Driver calls `pct.Allocator.DRADevices()` — returns `[]resapi.Device` in the shape selected by `pct.dra.mode`.
4. Driver calls `kubeletplugin.PublishResources` with those devices, paginating into `pool0..poolN` at `ResourceSliceMaxDevices` (like PR #536).
5. On `Reconfigure`, the driver diffs and re-publishes. `pct.Allocator` re-`Configure` may change `GuaranteedHpCpus` per punit (e.g. SST-TF bucket reconfiguration by operator) — the driver must re-emit and let the scheduler observe.

### Prepare flow (per claim)

1. Scheduler at Filter/Reserve time picks a specific punit device on this node and computes `consumedCapacity: {nri/hp-cpus: 4}` against the punit's remaining capacity. KEP-5517 accounting deducts 4 CPUs from `node.status.allocatable.cpu`.
2. Scheduler at PreBind writes `pod.status.nodeAllocatableResourceClaimStatuses`.
3. Kubelet calls `PrepareResourceClaims(claim)` on the driver.
4. Driver locks the resmgr and calls `pct.Allocator.PickHpCpus(punitID, 4)` — selects 4 CPUs from that punit using existing HP-room accounting, marks them as `hpUsed[punitID]`.
5. Driver writes a CDI spec at `/var/run/cdi/nri.native.cpu.yaml` with entries `cpu<N>: env: NRI_HP_CPU<N>=1` for each selected CPU.
6. Driver returns `PrepareResult` with `CDIDeviceIDs: ["nri.native.cpu/device=cpu<N>", ...]`.
7. Driver **does NOT** call `sst.AssociateCPUs` here. CLOS association is deferred to the NRI phase, where the topology-aware/balloons policy already handles it via `pct.Allocator.UseClass()` after pool/balloon assignment.

### NRI enforcement flow (unchanged trust boundary)

1. When NRI `CreateContainer` fires, the topology-aware or balloons policy reads the container's env list.
2. New helper (based on PR #536's `getClaimedCPUs`) parses `NRI_HP_CPU<N>=1` env vars and returns the pre-selected CPU set + the residual native-CPU count.
3. Existing pool/balloon allocator treats those CPUs as claim-pre-allocated and does not re-count them against pool capacity.
4. Existing `pct.Allocator.UseClass(cpuClassName, cpus)` associates them to the HP CLOS, invoking `sst.AssociateCPUs`.

The container ends up with a `cpuset.cpus` that includes the DRA-claimed HP CPUs, and those CPUs are associated to the HP CLOS. Same enforcement pathway as today; only the scheduling admission is new.

### Release flow

1. Kubelet calls `UnprepareResourceClaims`.
2. Driver calls `pct.Allocator.ReleaseHpCpus(punitID, cpuIDs)` — subtracts from `hpUsed[punitID]`.
3. Driver removes the corresponding entries from the CDI spec.
4. No SST CLOS re-association is needed. The next container to use those CPUs will re-associate them via `UseClass()`.

### Coexistence with the existing hint path

Both paths remain active for the same PCT allocator:

- **DRA-claimed HP CPUs** — accounted in `hpUsed[punitID]` via `PickHpCpus` at Prepare time. Contribute to non-DRA workloads' `Hints()` computation exactly as if they had been assigned by the existing hint-driven path.
- **Non-DRA HP CPUs** — assigned by pool/balloon allocator using `pct.Allocator.Hints()` and `FreeClassCapacity()`; tracked in `hpUsed[punitID]` via `trackHpUsage()`.

`FreeClassCapacity(className, held)` and `PickHpCpus(punitID, N)` must share the `hpUsed` map. Concurrent Prepare and NRI-phase paths must synchronize on the resmgr lock, as PR #536 already does.

### Feature-gate detection

The driver publishes `AllowMultipleAllocations`, `capacity.requestPolicy`, and `NodeAllocatableResources` unconditionally when `pct.publishAsDRA` is set. On a cluster where the corresponding feature gate is off:

- `AllowMultipleAllocations` off → scheduler treats the punit device as single-allocation. Bad. **Refuse to start if `AllowMultipleAllocations` is unrecognized by the API server** (detect via a probe: try to publish a test slice with the field set; if the API server strips it, error out). Alternatively, require operators to assert the gate is enabled via a config flag `pct.dra.requireConsumableCapacity: true` and trust it.
- `NodeAllocatableResources` off → mapping field is ignored, users must mirror CPU count in `spec.cpu`. Log a warning. Not fatal.
- `sharedCounters.requestPolicy + valueFrom` (Model C) → same probe approach; refuse Model C startup if not present.

### Helm chart additions

RBAC (mirror PR #536):

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

`DeviceClass` object:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: nri.native.cpu
spec:
  selectors:
    - cel:
        expression: device.driver == "nri.native.cpu"
```

## Open decisions

1. **Scope of `cpuClass` exposure.** PCT priority only, or the full `cpuClass` surface (EPP, freqGovernor, uncore freq, disabledCstates)?
   - **Recommendation:** PCT only for v1. Non-priority cpuClass fields do not benefit from admission-time enforcement (no capacity to run out of). If exposed, they should be **attributes only, not capacities**, so users can CEL-select on them — and only added when a user asks for it. Blocks: needs confirmation.

2. **KEP-5941 target — required or aspirational?**
   - **Recommendation:** design supports both. Ship Model B in v1. Add Model C as opt-in behind a config flag when KEP-5941 lands in alpha with usable code. Blocks: none; already reflected in design.

3. **Config surface naming.** `pct.publishAsDRA` vs `dra.publishPCT` vs new top-level `dra.*` section?
   - Blocks: bikeshed; deferred.

4. **Driver name.** `nri.native.cpu` (mirrors PR #536's `native.cpu`) vs something more specific like `nri.pct` (signals it's PCT-only) vs `nri.topology.cpu` (leaves room for future non-PCT scope)?
   - Blocks: coupled to decision (1).

## Change log

_(none yet — draft state)_
