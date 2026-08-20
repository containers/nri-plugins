# Kubernetes DRA landscape — reference for nri-plugins DRA work

Ecosystem knowledge for anyone designing DRA-related features in nri-plugins. Not repo-specific — treat this as a curated snapshot of the Kubernetes DRA world as of early 2026. Update dates and status flags as KEPs move. Companion: [pr-536-analysis.md](pr-536-analysis.md) for the specific prior-art analysis of the topology-aware DRA prototype.

## [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517): DRA Node Allocatable Resources — the accounting fix

Alpha in Kubernetes v1.36 (Oct 2025), alpha2 in v1.37. Feature gate `DRANodeAllocatableResources`, depends on `DynamicResourceAllocation`. This is *the* KEP that makes DRA-based CPU/memory allocation viable — without it, DRA and `node.status.allocatable` are two independent ledgers and pods get oversubscribed / CFS-throttled.

**Problem it solves.** When a DRA driver hands out CPU or memory, the scheduler's `NodeResourcesFit` plugin doesn't know (sees only `pod.spec.resources`) → node oversubscription. Kubelet writes pod-level cgroup `cpu.max` from spec only → DRA-claimed CPU is CFS-throttled at the pod boundary. QoS class comes from spec too → DRA-only pod is `BestEffort`.

**API.** Driver declares that its devices represent node-allocatable resources:

```go
type Device struct {
    // ...
    NodeAllocatableResources map[v1.ResourceName]NodeAllocatableResource
}
type NodeAllocatableResource struct {
    Mapping  *NodeAllocatableMapping  // this device IS the resource
    Overhead *NodeAllocatableOverhead // this device INCURS cost in the resource (per-pod / per-container)
}
type NodeAllocatableMapping struct {
    CapacityKey        *QualifiedName    // e.g. "dra.cpu/cpu"
    CapacityMultiplier *resource.Quantity
    DeviceMultiplier   *resource.Quantity // one device == deviceMultiplier CPUs
}
```

Allowed keys: `cpu`, `memory`, `hugepages-<size>`.

**Scheduler behavior.** `DynamicResources` plugin's Filter becomes authoritative for node-allocatable resource fit: `standard-requests + DRA-mapped-quantity ≤ node.allocatable`. In PreBind it writes `pod.status.nodeAllocatableResourceClaimStatuses[]` with resolved quantities.

**Kubelet behavior.** Pod-level cgroup `cpu.max` / `cpu.weight` / `memory.max` / `hugepages.limit` are inflated by DRA capacity. Container-level limits also inflated (but not shares — kubelet cannot tell exclusive vs shared). QoS class still spec-only (explicit non-goal). Code: `pkg/kubelet/cm/helpers_linux.go:182-197` gates `UseDRANodeAllocatableResourceClaimStatus: true` into `component-helpers/resource.PodRequests()`.

**Sharing:** mapped devices cannot be shared across pods (scheduler enforces); overhead-mapped can share (accounted per-pod). `PodLevelResources` when explicit takes precedence and DRA additions are NOT added on top.

**Known limitation:** Scheduler *scoring* doesn't yet consider DRA — only Filter does. Fully-unified scoring is future work.

## Related DRA KEPs (map, not exhaustive)

**Core allocation model:**
- [KEP-4381](https://github.com/kubernetes/enhancements/issues/4381) Structured Parameters — base DRA (in-tree since v1.32).
- [KEP-4815](https://github.com/kubernetes/enhancements/issues/4815) Partitionable Devices — static parent counters shared across children.
- [KEP-5075](https://github.com/kubernetes/enhancements/issues/5075) Consumable Capacity — `AllowMultipleAllocations: true` + `DeviceCapacity.RequestPolicy` (default/range/step/valid-values). One device requested many times, each request draws request-time-computed amount. `dra-driver-cpu` grouped mode uses this.
- [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) Shared Consumable Capacity — composition of 4815 + 5075: request-driven consumption from a shared parent counter set. **Story 2 explicitly: "CPU/Memory alignment via PCIe root grouping" — one shared capacity set per NUMA node, PCIe-root child devices consume from it.** Pre-alpha (no code yet).

**Cross-driver co-placement:**
- [KEP-5491](https://github.com/kubernetes/enhancements/issues/5491) List-type attributes — attributes as lists, non-empty-intersection matching.
- [KEP-6072](https://github.com/kubernetes/enhancements/issues/6072) Standard `numaNode` attribute — `resource.kubernetes.io/numaNode` (scalar or int list). Explicit goal: "restore NUMA coordination lost when devices moved from device plugins to DRA." This is *the* attribute for cross-driver NUMA co-placement via `matchAttribute`. Any CPU/memory DRA driver should publish it.
- [KEP-6080](https://github.com/kubernetes/enhancements/issues/6080) Derived attributes — synthesize grouping keys in CEL.
- [KEP-5963](https://github.com/kubernetes/enhancements/issues/5963) Compatibility groups — mutual exclusion (MIG vs vGPU).

**Governance / lifecycle:**
- [KEP-5027](https://github.com/kubernetes/enhancements/issues/5027) Admin-controlled attributes — cluster admin patches attributes into ResourceSlices.
- [KEP-5055](https://github.com/kubernetes/enhancements/issues/5055) Taints/tolerations — drain devices without draining nodes.
- [KEP-5234](https://github.com/kubernetes/enhancements/issues/5234) Mixins — DRY ResourceSlices for large device counts.
- [KEP-5945](https://github.com/kubernetes/enhancements/issues/5945) Optional Node Preparation — driver can declare `SkipNodeOperations`, no kubelet-plugin gRPC needed.
- [KEP-4817](https://github.com/kubernetes/enhancements/issues/4817) Device status — driver reports back to `ResourceClaim.Status.Devices`.
- [KEP-5677](https://github.com/kubernetes/enhancements/issues/5677) Availability visibility — user queries "how much is left in this pool."

**Kubelet/node side:**
- [KEP-3695](https://github.com/kubernetes/enhancements/issues/3695) PodResources for DRA — extends PodResources API to expose DRA allocations. Already consumed by `balloons.preferCloseToDevices` today.
- [KEP-5304](https://github.com/kubernetes/enhancements/issues/5304) Attributes Downward API — driver writes per-request JSON metadata, CDI-mounted into containers.
- [KEP-5526](https://github.com/kubernetes/enhancements/issues/5526) Pod-level resource managers — kubelet's CPU/Memory/Topology Manager gain `pod.spec.resources` awareness. *Competing trajectory* to DRA-based CPU alloc.
- [KEP-5554](https://github.com/kubernetes/enhancements/issues/5554) In-place resize alongside static CPU manager.

**Workloads:**
- [KEP-5729](https://github.com/kubernetes/enhancements/issues/5729) Workload API / PodGroup ResourceClaims — claims at the group level.
- [KEP-5978](https://github.com/kubernetes/enhancements/issues/5978) ClusterResourceClaimTemplate — cluster-scoped templates.

## The reference CPU DRA driver: [kubernetes-sigs/dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu)

Not integrated with nri-plugins — reference architecture only.

Layout:
- [`pkg/driver/`](https://github.com/kubernetes-sigs/dra-driver-cpu/tree/main/pkg/driver) — kubelet plugin + NRI hooks + CDI.
  - [`dra_hooks.go`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/pkg/driver/dra_hooks.go) — `PublishResources`, `PrepareResourceClaims`, `UnprepareResourceClaims`.
  - [`nri_hooks.go`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/pkg/driver/nri_hooks.go) — `Synchronize`, `CreateContainer`, `StopContainer`, `RemoveContainer` — sets `cpuset.cpus` at container start; parses DRA env vars on restart to rebuild state without a checkpoint.
  - [`cdi.go`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/pkg/driver/cdi.go) — CDI spec management (adds/removes device entries).
- [`pkg/device/builder.go`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/pkg/device/builder.go) — builds `resourceapi.Device` list.
  - Two device modes: `individual` (one device per CPU with topology attrs) and `grouped` (per-socket/per-NUMA/per-machine as consumable capacity `dra.cpu/cpu`).
  - `groupedCPUNodeAllocatable` / `individualCPUNodeAllocatable` — wires **[KEP-5517](https://github.com/kubernetes/enhancements/issues/5517)** mappings (`CapacityKey: dra.cpu/cpu, CapacityMultiplier: 1` for grouped; `DeviceMultiplier: 1` for individual). Gated by `PublishNodeAllocatableResourceMapping` config flag.
- [`pkg/cpumanager/cpu_assignment.go`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/pkg/cpumanager/cpu_assignment.go) — copy of kubelet's CPUManager static-policy assignment logic.
- CPU Manager feature parity is the stated goal (see [`docs/user/feature-support.md`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/docs/user/feature-support.md) in that repo).

Notably: **does NOT manage memory.** Memory-as-DRA is unclaimed ecosystem space.

Currently rejects claim sharing across pods until [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) stabilizes.

**Config-change model: restart-the-DaemonSet, no live reconfigure.** [`cmd/dracpu/app.go:112`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/cmd/dracpu/app.go#L112) loads config exactly once at process start via `driverconfig.Resolve(...)`. No fsnotify watcher, no ConfigMap watcher, no SIGHUP handler, no `Reconfigure` method. Their own docs ([`hack/examples/sysfs-overlay/README.md`](https://github.com/kubernetes-sigs/dra-driver-cpu/blob/main/hack/examples/sysfs-overlay/README.md)) tell operators: "When changing the ConfigMap after deployment, restart the DaemonSet." This sidesteps in-flight-claim-vs-changed-attribute races and Prepare-vs-Configure locking — at the cost of disrupting scheduling on every config change. nri-plugins takes a higher operator-ergonomics posture (live `Reconfigure`), so this workaround is not available to us; we own the class-freshness problem `dra-driver-cpu` avoided.

## The SIG-node reference driver: [kubernetes-sigs/dra-example-driver](https://github.com/kubernetes-sigs/dra-example-driver)

Skeleton driver, mock GPU + mock CPU + mock net profiles. Its value is showing the canonical patterns the DRA machinery expects; not a real driver we would run.

**Three binaries.** The example splits into [`cmd/dra-example-kubeletplugin/`](https://github.com/kubernetes-sigs/dra-example-driver/tree/main/cmd/dra-example-kubeletplugin), [`cmd/dra-example-controller/`](https://github.com/kubernetes-sigs/dra-example-driver/tree/main/cmd/dra-example-controller), and [`cmd/dra-example-webhook/`](https://github.com/kubernetes-sigs/dra-example-driver/tree/main/cmd/dra-example-webhook). Deliberate separation: node-local kubelet plugin does allocation and CDI; a cluster-scoped controller (built on `controller-runtime`) watches `ResourceClaim`s allocated to the driver and can update claim status (e.g. via [KEP-4817](https://github.com/kubernetes/enhancements/issues/4817)); an admission webhook validates opaque per-request config carried in claims. We're going kubelet-plugin-only in v1 following [PR #536](https://github.com/containers/nri-plugins/pull/536); the controller and webhook binaries are not something we need but they show what the full picture looks like.

**Profile abstraction.** [`internal/profiles/profiles.go`](https://github.com/kubernetes-sigs/dra-example-driver/blob/main/internal/profiles/profiles.go) defines a `Profile` interface with three implementations: `gpu`, `cpu`, `net`. Each profile provides `EnumerateDevices()` (builds the `resourceslice.DriverResources` at startup), `SchemeBuilder()`/`Validate()` (opaque-config decoding), and `ApplyConfig()` (per-request CDI edits). The driver picks one profile at startup via `--profile`. For us: our "profile" is fixed per binary (topology-aware today, balloons v2), so no runtime selection is needed, but the shape of "device-source interface handed to the kubelet plugin" matches our own `cpuclass.Manager.DRADevices()` design.

**Checkpoint file for state persistence.** [`cmd/dra-example-kubeletplugin/checkpoint.go`](https://github.com/kubernetes-sigs/dra-example-driver/blob/main/cmd/dra-example-kubeletplugin/checkpoint.go) writes a versioned JSON checkpoint at `${PluginDataDir}/checkpoint.json`. The `Checkpoint` type deliberately carries only claim UIDs — the comment states the driver can *deterministically reconstruct* the full CDI config for a claim from the ResourceClaim itself. Uses versioned runtime serializer and atomic tmp+rename writes. This is a **third** approach to restart state, complementing `dra-driver-cpu`'s reconstruct-from-env-vars and [PR #536](https://github.com/containers/nri-plugins/pull/536)'s opaque `pkg/resmgr/cache` entry. We are sticking with PR #536's approach in v1.

**One CDI file per claim.** `CreateClaimSpecFile(claimUID, PreparedDevices)` in [`cdi.go`](https://github.com/kubernetes-sigs/dra-example-driver/blob/main/cmd/dra-example-kubeletplugin/cdi.go) writes a separate `<vendor>-<class>-claim-<uid>.yaml` for each prepared claim. `DeleteClaimSpecFile` on Unprepare. Cleaner concurrency than a single driver-wide spec file. [PR #536](https://github.com/containers/nri-plugins/pull/536) uses a single file; we are following PR #536.

**Startup lifecycle.** `RunPlugin` in [`main.go`](https://github.com/kubernetes-sigs/dra-example-driver/blob/main/cmd/dra-example-kubeletplugin/main.go) creates the plugin dir, creates the CDI root, sets up graceful shutdown on SIGHUP/INT/TERM/QUIT (SIGHUP triggers shutdown, not reload), starts a metrics server, constructs the driver, calls `kubeletplugin.Start` inside `NewDriver`, then calls `helper.PublishResources` **exactly once**. No republish path exists. This matches `dra-driver-cpu`'s one-shot model. Confirms that "publish on Reconfigure" is a nri-plugins-specific enhancement, not a widely-adopted pattern.

**Applies to us: attribution and pattern-vocabulary only.** We are sticking with the [PR #536](https://github.com/containers/nri-plugins/pull/536) implementation approach across all decisions where the reference drivers diverge (checkpoint format, CDI file layout, control-plane binaries). `dra-example-driver` remains the "how the DRA gRPC API is meant to be consumed" reference for questions like "what's the right way to call `kubeletplugin.Start`" or "what does the `Profile` interface split look like."

## Other reference code

- [kubernetes/kubernetes](https://github.com/kubernetes/kubernetes):
  - [`pkg/features/kube_features.go`](https://github.com/kubernetes/kubernetes/blob/master/pkg/features/kube_features.go) — feature gates (`DRANodeAllocatableResources` at line 275).
  - [`staging/src/k8s.io/dynamic-resource-allocation/kubeletplugin`](https://github.com/kubernetes/kubernetes/tree/master/staging/src/k8s.io/dynamic-resource-allocation/kubeletplugin) — the `kubeletplugin.Helper` API used by [PR #536](https://github.com/containers/nri-plugins/pull/536) and `dra-driver-cpu`.
  - [`pkg/scheduler/framework/plugins/dynamicresources/nodeallocatabledynamicresources.go`](https://github.com/kubernetes/kubernetes/blob/master/pkg/scheduler/framework/plugins/dynamicresources/nodeallocatabledynamicresources.go) — [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) scheduler plugin.
  - [`staging/src/k8s.io/component-helpers/resource/helpers.go`](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/component-helpers/resource/helpers.go) — `PodRequests()` with `UseDRANodeAllocatableResourceClaimStatus`.
  - [`pkg/kubelet/cm/helpers_linux.go:182`](https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/cm/helpers_linux.go#L182) — kubelet gates DRA into cgroup calc.
- [kubernetes/enhancements](https://github.com/kubernetes/enhancements) — KEPs live under [`keps/sig-scheduling/`](https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling) and [`keps/sig-node/`](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node).
- [cncf-tags/container-device-interface](https://github.com/cncf-tags/container-device-interface) — the CDI spec and its Go library `tags.cncf.io/container-device-interface`. Every DRA driver that returns `CDIDeviceIDs` writes CDI spec files; both reference drivers use this library (`pkg/cdi.Cache.WriteSpec`, `specs-go` types, `GenerateTransientSpecName`) rather than hand-rolling YAML. Apache-2.0; drivers should import it rather than reinvent atomic writes, spec validation, and version tracking.

## Design options being weighed for nri-plugins DRA integration

**A. Passive DRA-aware topology policy.** Extend topology-aware policy to read PodResources' DRA allocations ([KEP-3695](https://github.com/kubernetes/enhancements/issues/3695)) — mirror `balloons.preferCloseToDevices` — and steer CPU pool / memory / hugepage placement to whichever NUMA the DRA-claimed devices (from [dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu), [DraNet](https://github.com/kubernetes-sigs/dranet), NVIDIA GPU DRA) landed on. Zero upstream coordination burden, no accounting gap, works today. Incremental extension of an existing pattern in this codebase.

**B. nri-plugins publishes memory (and hugepages) via DRA.** Uses [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) `nodeAllocatableResources.mapping` + [KEP-6072](https://github.com/kubernetes/enhancements/issues/6072) `numaNode` attribute. Novel space — no memory DRA driver exists. Plays to `libmem`'s strengths. A single pod claim can combine `dra.cpu/cpu: 8` + `nri-topology.memory: 32Gi` + `nvidia.com/gpu` and `matchAttribute: resource.kubernetes.io/numaNode` co-locates them. Requires [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) alpha (v1.36+).

**C. Full CPU+memory DRA driver on [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941) model.** Per-NUMA shared counter sets for cpu+memory+hugepages; per-CPU and per-memory-zone child devices consume from them. Architecturally right for topology-aware, but blocked on KEP-5941 (pre-alpha) and requires reconciling with `dra-driver-cpu`. Long-horizon research/prototype track.

**Recommendation as of this writing:** A now, B next, C as a prototype tracking [KEP-5941](https://github.com/kubernetes/enhancements/issues/5941). Straight-rebase of [PR #536](https://github.com/containers/nri-plugins/pull/536) as an nri-plugins-shipped CPU DRA driver is discouraged because that space is now occupied by [kubernetes-sigs/dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu).

**Open questions** (not yet answered by the user):
- Compete with / integrate with / ignore [dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu)?
- CPU only, memory only, or joint?
- Is there a customer/upstream deliverable pinning priority?

**Explicit scope carveout:** the cpuClass-DRA initiative ([design.md](design.md)) does *not* involve `dra-driver-cpu`. It uses `dra-driver-cpu` only as an architectural reference.
