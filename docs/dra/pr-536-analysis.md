# PR #536 — analysis of the topology-aware DRA prototype

Analysis notes for <https://github.com/containers/nri-plugins/pull/536> ("[proto]: bolt a DRA driver frontend on the topology-aware policy," klihub). Fetched locally as branch `pr-536-dra`. Companion: `landscape.md` for the broader DRA context this prototype predates.

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

The bridge from DRA to NRI in PR #536 is **CDI + env vars**:

1. Kubelet resolves the claim and calls the driver's `PrepareResourceClaims`.
2. Driver returns `CDIDeviceIDs: ["native.cpu/device=cpuN"]`.
3. Runtime applies the CDI spec → container gets `DRA_CPU<N>=1` in its env.
4. On NRI `CreateContainer`, the topology-aware policy scans env vars, extracts the DRA-preallocated CPU set, and treats those CPUs as claim-pre-allocated in its pool bookkeeping.

**This mechanism is still valid** for future DRA drivers in this repo. The PCT-DRA design (`pct-design.md`) reuses the same shape with `NRI_HP_CPU<N>=1` env vars.

## Prototype scars

- `getClaimedCPUs` env-var parsing to bridge DRA and native CPU accounting — needed because KEP-5517 didn't exist yet. No longer required for scheduler-side accounting once KEP-5517 is assumed available; env-var parsing may still be needed as a driver-internal signal to the NRI phase (which CPUs are DRA-preallocated).
- User must currently mirror DRA CPU count in `spec.cpu` — a workaround KEP-5517 removes.
- Verbose `***** ...` debug logging left in `allocateClaim`.
- `TODO: sort old grants by QoS class or size and pool distance from root` in reallocation logic.
- Memory not exposed as DRA (CPUs only). No cross-container / pod-scope claims. No tests for the DRA path.

## Where the mechanism holds up vs where KEP-5517 replaces it

| PR #536 approach | Post-KEP-5517 replacement |
|---|---|
| User mirrors CPU count in `spec.cpu` | Scheduler adds mapped quantity automatically via `NodeAllocatableResources` |
| `getClaimedCPUs()` parses `DRA_CPU<N>=1` env vars to reconcile accounting | Kubelet reads `pod.status.nodeAllocatableResourceClaimStatuses`; env-var parsing not needed for accounting |
| Prototype scars in `allocateClaim` (evict conflicting grants) still needed | Still needed — that is *inside-driver* logic, orthogonal to KEP-5517 |
| CDI + `DRA_CPU<N>=1` env var to signal per-CPU allocation to NRI phase | Same mechanism — this is driver ↔ NRI enforcement, not driver ↔ kubelet accounting |

## Takeaway for future DRA work in this repo

- The **kubelet-plugin registration, CDI writer, and CDI+env-var bridge** are the reusable pieces.
- The **user-facing accounting workarounds** (mirroring `spec.cpu`, env-var parsing for capacity reconciliation) are obsolete and should not be copied.
- The **prototype scars** (evict-and-reallocate, exclusive-grant conflict handling) are inside-policy logic that any DRA-integrated policy still needs and can be lifted with cleanup.
- The **RBAC + host-mount + `DeviceClass`** Helm additions are directly reusable as a starting template.
