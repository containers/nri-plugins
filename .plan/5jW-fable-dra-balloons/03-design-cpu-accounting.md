# Step 2: native CPU, DRA, kubelet and the balloons policy

## The problem

Kubernetes accounts CPU twice if a node both reports CPUs in
`node.status.allocatable` and a DRA driver publishes the same CPUs as
devices: the scheduler's NodeResourcesFit sees only native requests, the
DRA plugin sees only claims. On the node, the kubelet sizes cgroups from
the spec only. The balloons policy additionally has its own accounting
(balloon sizes from native requests).

We need:
1. Traditional containers (native CPU requests only) and DRA containers
   (claims on balloon devices) running on the same node without the
   scheduler overcommitting either group's CPUs.
2. A single source of truth per balloon for how many CPUs it holds.
3. Minimal new machinery; the balloons policy must stay understandable.

## Options studied

### A. Placement-only devices (claim selects the balloon, native requests size it)

Device per instance, no capacity. A claim just names the balloon; the
container's native CPU request sizes the balloon as today. Scheduler
accounting stays native-only and correct for what the container asked.
Problems: the pre-created balloon's `minCPUs` CPUs are invisible to the
scheduler (as today without DRA), a claim gives no isolation guarantee
beyond "same balloon", and the DRA request carries no CPU amount, so DRA
adds nothing that an annotation does not already give. Rejected as too
weak to be worth a DRA driver.

### B. One shareable device per balloon type (template-style)

Capacity = free CPUs of the node or of the type. The policy creates
instances per claim. Problem: capacity is a moving target shared by all
types and by non-DRA balloons; the scheduler's per-device sum check says
nothing about real free CPUs; instance lifecycle (create on prepare,
delete on release, reconfigure) is a lot of new code. Rejected.

### C. One shareable device per balloon instance with consumable `cpu` capacity (chosen)

- Capacity `cpu` = `maxCPUs` of the type. Several claims can share an
  instance; the scheduler guarantees sum(consumed) <= maxCPUs per
  instance (DRAConsumableCapacity, beta on by default).
- A claim with `capacity.requests.cpu: N` reserves N CPUs in that balloon
  exactly like a container requesting N CPUs does today: the policy adds
  the claim's CPUs to the balloon's requested milli-CPUs and inflates the
  balloon (within minCPUs..maxCPUs) when the claim is prepared. A claim
  without an amount consumes the whole capacity, i.e. the whole balloon.
- With KEP-5517 (`DRANodeAllocatableResources`), the device's
  `nodeAllocatableResources.cpu.mapping{capacityKey: cpu, multiplier 1}`
  makes the scheduler subtract the consumed CPUs from node allocatable
  CPU for the pod, so native requests of other pods cannot overcommit the
  CPUs handed to claims. The kubelet adds the same amount to the pod
  cgroup and to container limits (when limits are set). Without the gate
  the field is dropped and only the per-device sum is enforced; node-level
  accounting then has the same gap as any balloon's `minCPUs` has today.
- Native requests of a DRA container are counted too (see rules below),
  which matches KEP-5517 where the pod footprint is native + mapped.
- Instance lifecycle stays static: only `minBalloons` instances exist and
  are published; no on-demand creation or deletion of DRA balloons.

This reuses the policy's existing sizing machinery (requested milli-CPUs
per balloon), the DRA consumable capacity model, and KEP-5517's mapping
without any special per-feature code paths.

### D. Non-shareable whole-balloon devices

One claim per instance, `allowMultipleAllocations: false`, mapping via
`deviceMultiplier: maxCPUs`. Simpler mentally but strictly less flexible
than C (C with no amount gives the same exclusive behavior), and it does
not express how many CPUs the claim means when minCPUs < maxCPUs.
Rejected; C covers it.

## Decision and rules

Terminology: a **DRA balloon** is an instance of a balloon type with the
`dra` option. A **DRA container** is a container whose NRI CDI device list
contains at least one device of the `balloons.nri.io` driver, i.e. the
container references at least one claim prepared by this policy.

1. **Separation (the "all DRA containers in DRA balloons, no others"
   limitation; accepted).** Only DRA containers are placed in DRA
   balloons, and DRA containers are placed only in the balloon their
   claim names. Regular placement (`chooseBalloonDef`) skips DRA balloon
   types. Reason: a container the scheduler did not account for must not
   share CPUs the scheduler handed to a claim.
2. **Claim prepare = CPU reservation.** `AllocateClaim` finds the balloon
   for each result's device name, records (claim UID -> [device, balloon
   type, instance, milli-CPUs]) persistently, adds the claim's milli-CPUs
   to the balloon's requested CPUs and resizes the balloon. Not enough
   free CPUs on the node fails the claim (kubelet retries; pod stays in
   ContainerCreating with the error in events). A re-prepare of a
   recorded claim changes nothing and returns the same edits.
3. **Balloon size = ceil(native requests of its containers + CPUs of its
   claims)**, clamped to [minCPUs, maxCPUs]. `requestedMilliCpus(bln)` is
   extended to include claims; everything derived from it (resize on
   allocate/release, free CPUs, NRT zones, metrics) follows. A DRA balloon
   whose last container leaves is deflated only down to its claims.
4. **Container placement.** A DRA container's claims must all name the
   same balloon instance. Creating the container fails with an error if:
   - its claims name more than one balloon instance (or type);
   - a CDI device of ours refers to a claim the policy has no record of;
   - the pod annotation `balloon.balloons.resource-policy.nri.io` (pod or
     container scope) names a balloon type other than the claimed one.
   An annotation naming the claimed type is accepted (redundant).
   A regular container whose annotation names a DRA balloon type fails:
   "balloon type X is published as DRA devices; request it through a
   resource claim".
5. **Pinning and shares.** DRA containers are pinned to the balloon's
   cpuset (plus shared idle CPUs if configured) like any container in
   that balloon. CPU shares are set from native request + the container's
   claimed CPUs, to counter the KEP-5517 note that a claim-only container
   otherwise gets the minimal weight (the policy already sets shares from
   requests in `pinCpuMem`).
6. **Native requests on DRA containers** are allowed and counted by both
   Kubernetes and the policy. The documentation recommends asking for
   CPUs through the claim only, and notes that a small native request
   lifts the pod out of BestEffort QoS (KEP-5517 keeps QoS spec-based).
7. **Containers of a pod that do not reference the claim** (sidecars,
   init containers) are regular containers and go to regular balloons;
   the same-pod fill preference does not pull them into the DRA balloon.
8. **Claims shared across pods**: not supported by Kubernetes when the
   device has a node-allocatable mapping (scheduler rejects the second
   pod). Without the gate the policy would place both pods' containers in
   the balloon; that works but is unaccounted, and is not recommended.
9. **Restart.** Claim records live in the cache policy entry `dra-claims`
   and survive the policy's `ResetPolicyEntries()` in Setup (read first,
   re-stored after). After a restart `Setup` recreates balloons, re-applies
   claim reservations (resize), then `Sync` re-places running containers,
   DRA containers included. The kubelet does not re-prepare claims after
   a plugin restart, so the records are the only source.
10. **Reconfiguration.** Rejected if any recorded claim loses its balloon
    (type no longer DRA, removed, or instance >= minBalloons). Otherwise
    balloons are rebuilt, claims re-applied, containers re-synced, and
    the device list republished.

## Kubernetes assumptions and gates

- Kubernetes v1.37, kubelet CPU manager policy `none` (as for any NRI
  resource policy).
- DRAConsumableCapacity: beta, on by default. Needed for shareable
  devices; without it the apiserver drops `allowMultipleAllocations` and
  a claim takes a whole device (still works, exclusive balloons only).
- DRANodeAllocatableResources (alpha, off by default): enable on
  apiserver, scheduler and kubelet to get node-level CPU accounting and
  kubelet cgroup inflation. Our devices always publish the mapping.
- Runtime must pass CDI devices to NRI plugins: containerd >= 2.3.0.
  Without it every DRA container looks like a regular container, is
  refused by no rule, and lands in a regular balloon. The policy has no
  reliable signal to detect this, so the requirement is documented.

## Known limitations (to document)

- Claim-only pods are BestEffort (Kubernetes QoS is spec-based).
- A claim can fail at prepare time when the node has fewer free CPUs than
  the scheduler assumed (reserved and other balloons' minCPUs are
  invisible to it). Enabling DRANodeAllocatableResources narrows but does
  not close this gap.
- DRA balloons are static: minBalloons instances, never created or
  deleted on demand.
- `maxCPUs` is required for DRA types because it is the published
  capacity.
