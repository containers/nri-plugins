# PR #536 — analysis of the topology-aware DRA prototype

Analysis notes for [containers/nri-plugins#536](https://github.com/containers/nri-plugins/pull/536) ("[proto]: bolt a DRA driver frontend on the topology-aware policy," klihub). Fetched locally as branch `pr-536-dra`. Companion: [landscape.md](landscape.md) for the broader DRA context this prototype predates.

**Status.** Prototype from June 2025, rebased through Feb 2026. Author-labeled "prototype for feasibility study, *not a proposal* for a first real DRA-based CPU driver."

## What the prototype adds

Files of interest:

- `pkg/sysfs/dra.go` — maps a CPU to a `resapi.Device` with attributes `package`, `die`, `cluster`, `core`, `coreType` (P/E), `localMemory`, `isolated`, `minFreq/maxFreq/baseFreq`, `cache0ID..cache3ID`.
- `pkg/resmgr/dra.go` (~486 lines) — kubelet-plugin registration (`native.cpu` driver), publishes `ResourceSlice`s (paginated `pool0..poolN`), implements `PrepareResourceClaims`/`UnprepareResourceClaims`, writes CDI spec at `/var/run/cdi/native.cpu.yaml` injecting `DRA_CPU<N>=1` env vars.
- `pkg/resmgr/policy/policy.go` — new `Claim` interface + `PublishCPUFn` + Backend methods `AllocateClaim` / `ReleaseClaim`.
- `cmd/plugins/topology-aware/policy/pools.go` — `allocateClaim` finds tightest pool containing the claimed CPUs, evicts conflicting exclusive grants, marks CPUs claimed in pool supply, reallocates displaced grants. `getClaimedCPUs` parses `DRA_CPU<N>=1` env vars from container spec to reconcile DRA-claimed vs native CPU requests.
- `pkg/kubernetes/{client,watch}` — kube client/watch code lifted out of `pkg/agent` so plugins other than resource managers can use it.
- Helm chart additions: RBAC (`resourceslices` CRUD, `resourceclaims`/`deviceclasses` get, `resourceclaims/status` patch/update), host mounts for `/var/lib/kubelet/plugins` and `.../plugins_registry`, `DeviceClass` named `native.cpu`.

## The DRA-to-NRI bridge

The bridge from DRA to NRI in [PR #536](https://github.com/containers/nri-plugins/pull/536) is **CDI + env vars**:

1. Kubelet resolves the claim and calls the driver's `PrepareResourceClaims`.
2. Driver returns `CDIDeviceIDs: ["native.cpu/device=cpuN"]`.
3. Runtime applies the CDI spec → container gets `DRA_CPU<N>=1` in its env.
4. On NRI `CreateContainer`, the topology-aware policy scans env vars, extracts the DRA-preallocated CPU set, and treats those CPUs as claim-pre-allocated in its pool bookkeeping.

**This mechanism is still valid** for future DRA drivers in this repo. The cpuClass-DRA design ([design.md](design.md)) reuses the same shape with `NRI_CPU<N>=1` env vars.

## Prototype scars

- `getClaimedCPUs` env-var parsing to bridge DRA and native CPU accounting — needed because [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) didn't exist yet. No longer required for scheduler-side accounting once [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) is assumed available; env-var parsing may still be needed as a driver-internal signal to the NRI phase (which CPUs are DRA-preallocated).
- User must currently mirror DRA CPU count in `spec.cpu` — a workaround [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) removes.
- Verbose `***** ...` debug logging left in `allocateClaim`.
- `TODO: sort old grants by QoS class or size and pool distance from root` in reallocation logic.
- Memory not exposed as DRA (CPUs only). No cross-container / pod-scope claims. No tests for the DRA path.

## Where the mechanism holds up vs where [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) replaces it

| PR #536 approach | Post-[KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) replacement |
|---|---|
| User mirrors CPU count in `spec.cpu` | Scheduler adds mapped quantity automatically via `NodeAllocatableResources` |
| `getClaimedCPUs()` parses `DRA_CPU<N>=1` env vars to reconcile accounting | Kubelet reads `pod.status.nodeAllocatableResourceClaimStatuses`; env-var parsing not needed for accounting |
| Prototype scars in `allocateClaim` (evict conflicting grants) still needed | Still needed — that is *inside-driver* logic, orthogonal to [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) |
| CDI + `DRA_CPU<N>=1` env var to signal per-CPU allocation to NRI phase | Same mechanism — this is driver ↔ NRI enforcement, not driver ↔ kubelet accounting |

## Reuse tiers for the implementation work

"Not a proposal" does not mean "not a source." Individual commits and files hold up to different degrees against the design in [design.md](design.md) and are reused accordingly. [plan.md](plan.md) step 1 schedules the clean cherry-picks; steps 6–8 govern the salvage-and-adapt work.

### Tier 1 — direct cherry-pick (whole commits, minimal touch)

Refactors and additive utilities whose scope is orthogonal to the DRA design changes.

- `5dcb66dc` — `pkg/kubernetes: split out client setup code from agent.`
- `42ec1022` — `pkg/kubernetes: allow setting accepted and wire content types.`
- `45a14d1c` — `pkg/kubernetes: split out watch wrapper from agent.`
- `88140644` — `agent: expose node name, kube client and config.`
- `b0efadc3` — `cache: add opaque cache entries, container.GetEnvList().`
- `86e4c7a6` — `log: add AsYaml() for YAML-formatted log (blocks).` (nice-to-have; optional)

### Tier 2 — salvage-and-adapt (copy code into the new package structure, rename symbols)

The mechanism is correct; the surrounding shape needs to change.

- **Kubelet-plugin startup** (`newDRAPlugin`, `connect`, `Start`, `stop`, `unaryInterceptor`, `IsRegistered`, `HandleError`) → new `pkg/resmgr/dra/plugin.go`.
- **`resmgr` DRA plumbing** in `pkg/resmgr/resource-manager.go` (start/stop wiring, cache setup) — adapt to the new package boundary.
- **`saveClaims` / `restoreClaims` persistence** via `cache.SetEntry` / `GetEntry` — same shape; persisted struct changes to per-(class × punit).
- **Eviction / reallocation** in `cmd/plugins/topology-aware/policy/pools.go` `allocateClaim` — the algorithm survives; clean up `***** ...` logging and address `TODO: sort old grants by QoS class` before merge.
- **Helm chart RBAC + host mounts + `DeviceClass`** — copy with driver-name rename (`native.cpu` → `nri.topology-aware.cpu`).

### Tier 3 — learn-from-only (patterns understood, code rewritten)

The design changes moved the ground under these; no meaningful line-level reuse.

- **`pkg/sysfs/dra.go` DRA device schema** — attributes are entirely different in the new design (per-(class × punit) with `nri/cpuClass`, `nri/pctPriority`, `nri/energyPerformancePreference`, etc., not per-CPU with `package`/`die`/`cluster`/`coreType`). Rewrite.
- **Device construction** in `PublishCPUs` — `[]resapi.Device` shape changed from per-CPU to per-(class × punit). Rewrite around `cpuclass.Manager.DRADevices()`.
- **`getClaimedCPUs` env-var-parsing helper** — reduces to a smaller helper because [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) handles accounting; only the "which CPUs" signal remains.
- **`allocateClaim` capacity math** (`ClaimCPUs`, `UnclaimCPUs`, `getLargestSharedUsers`) — per-(class × punit) accounting shifts from pool-supply "shared CPU capacity" to per-punit tier capacity; some helpers may survive, most is redesigned.
- **User-facing accounting workarounds** (mirroring `spec.cpu`) — obsolete under [KEP-5517](https://github.com/kubernetes/enhancements/issues/5517), do not carry forward.
- **CDI writer** (`writeCDISpecFile` in `pkg/resmgr/dra.go`) — hand-rolled `fmt.Fprintf` YAML lacks atomic writes, spec validation, and version tracking. Replace with `tags.cncf.io/container-device-interface` (`cdiapi.Cache.WriteSpec`, `specs-go.Spec` types, `GenerateTransientSpecName` for per-claim filenames), same as `dra-driver-cpu` and `dra-example-driver`. See [plan.md](plan.md) step 7.
