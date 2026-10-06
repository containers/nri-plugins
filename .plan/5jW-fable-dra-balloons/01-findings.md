# Findings

Facts gathered before designing. Sources are named so they can be
re-checked. Kubernetes source: /home/akervine/github.com/kubernetes/kubernetes,
branch origin/release-1.37 unless stated otherwise.

## nri-plugins DRA plumbing (commits by Ed Bartosh, already on the branch)

- `pkg/resmgr/policy/policy.go`
  - `Owner.PublishDRADevices([]resourceapi.Device) error`: a policy pushes
    its device list; the resource manager copies it. Calling it before the
    DRA plugin exists (resmgr.m.dra == nil) silently publishes nothing, so
    policies publish from `Start()`, not `Setup()`. Order in
    `pkg/resmgr/resource-manager.go`: `setupDRA` (creates plugin) ->
    `policy.Start` -> `startDRA`.
  - `Backend.AllocateClaim(claim, results) ([]specs.ContainerEdits, error)`:
    results are already filtered to our driver. One edit per result is
    mandatory (a CDI device without edits is rejected). Must be idempotent
    for a claim already allocated. Records must be persisted by the policy
    (cache policy entries) because claims cannot be rebuilt from running
    containers.
  - `Backend.ReleaseClaim(uid)`: unknown claim is not an error.
- `pkg/resmgr/dra.go`: driver name is `<policy name>.nri.io`
  (`draDomain = "nri.io"`). `ClaimAllocated()` saves the cache and pushes
  pending container updates (so a claim allocation may shrink/move other
  containers via `updateContainers`). `ClaimReleased()` similar but
  failures to update containers are warnings.
- `pkg/resmgr/dra/plugin.go`: kubeletplugin registration, publishes a
  single pool named after the node, chunks devices to
  `ResourceSliceMaxDevices`. Claims refused until `AllowClaims()` after the
  first NRI Synchronize. The resourceslice controller only writes when the
  desired slice differs, so republishing an unchanged device list is cheap.
- `pkg/resmgr/dra/claim.go`: `PrepareResourceClaims` takes the resmgr lock,
  calls `policy.AllocateClaim`, commits via `owner.ClaimAllocated()`, writes
  the CDI spec. `UnprepareResourceClaims` -> `policy.ReleaseClaim`.
- `pkg/resmgr/dra/cdi.go`: CDI kind `<driver>/device`, device names
  `claim-<claim UID>-<result index>`, so the fully qualified CDI device
  name the kubelet hands to the runtime is
  `balloons.nri.io/device=claim-<uid>-<i>`. Spec file per claim in
  /var/run/cdi (DefaultDynamicDir).
- Balloons policy today: `AllocateClaim`/`ReleaseClaim` return
  `policy.ErrNoDRAClaims`; nothing is published.
- Template policy (`cmd/plugins/template/policy/dra.go`): publishes one
  shareable device "cpus" with consumable capacity `dra.cpu/cpu` (count of
  allowed CPUs), RequestPolicy default 1, step 1. Claims get the lowest
  free CPUs, recorded in cache policy entry "claims", restored in Start.
  Containers get env `DRA_CPUSET_<uid>=<cpuset>`; no pinning.
- Helm charts: `deployment/helm/balloons/templates/{daemonset,clusterrole}.yaml`
  already have the DRA mounts (kubelet plugins, plugins_registry, /var/run/cdi)
  and RBAC (resourceslices, resourceclaims get) behind
  `.Values.config.dra.enabled`. `values.yaml` has `dra: {}`. The template
  chart additionally has `templates/deviceclass.yaml` (DeviceClass
  `template.nri.io`, selector `device.driver == "template.nri.io"`); the
  balloons chart has no DeviceClass yet.
- E2E: `test/e2e/policies.test-suite/template/n4c16/test01-dra/` is the
  reference test. `k8s_feature_gates="A=true,B=false"` (env of run.sh) sets
  gates on apiserver, scheduler and kubelet; it is part of the VM box key,
  so a gated cluster is a different VM. A caller-given `vm_name` is kept
  (`run_tests.sh`), so gated and ungated VMs can live side by side. The
  nightly runner does not set gates; the template test SKIPs when
  `allowMultipleAllocations` is dropped by the apiserver.
- Cache: `pkg/resmgr/cache` keeps the NRI `*nri.Container` as `Ctr`; the
  Container interface has `GetEnv`, `GetMounts`, `GetDevices`, but no
  accessor for CDI devices. `ResetPolicyEntries()` wipes all policy
  entries; the balloons `Setup()` calls it ("We keep no policy data across
  restarts"), so claim records must be read before and re-stored after.
- `cache.Container.GetPodResources()` gives kubelet PodResources
  (`DynamicResources` with claim name/namespace and CDI device names;
  `KubeletPodResourcesDynamicResources` is GA in 1.36). Not used: the NRI
  CDI device list is simpler and runtime-authoritative.

## Runtime: CDI devices visible to NRI plugins

- NRI `api.Container` has `CDIDevices []*CDIDevice` (field 20) and
  `GetCDIDevices()` (vendored nri v0.12.2).
- containerd fills it from the CRI ContainerConfig.CDIDevices since commit
  98a2e8876 "cri,nri: pass injected CDI devices to plugins" (2026-01-09),
  first release v2.3.0. v2.2.x and older do not. The e2e framework
  installs the latest containerd (2.4.1 cached), so e2e VMs have it.
- The CRI CDIDevices come from the kubelet (DRA claims and device plugin
  CDI devices). A pod cannot forge them, unlike env variables or
  annotations. This is why container-to-claim association uses CDI
  device names, not env.
- CRI-O: not checked. Documented as "runtime must pass CDI devices to NRI".

## Kubernetes 1.37 DRA features relevant here

Feature gates (pkg/features/kube_features.go, release-1.37):

| Gate | 1.37 state | Use |
|------|-----------|-----|
| DynamicResourceAllocation | GA | base |
| DRAConsumableCapacity | Beta, on by default since 1.36 | shareable devices with consumable capacity (`allowMultipleAllocations`, `capacity.requests`, `consumedCapacity`, `shareID`) |
| DRANodeAllocatableResources | Alpha, off (1.36 alpha, 1.37 alpha2, beta planned 1.38) | KEP-5517: device `nodeAllocatableResources` mapping to node `cpu`; scheduler accounting and kubelet cgroups |
| DRAFractionalCapacityRange | Beta on | fractional request policy ranges, not needed |
| DRAExtendedResource | (exists) | DeviceClass mapped to an extended resource name; not used |

Consumable capacity semantics (staging/.../structured/internal/experimental/consumable_capacity.go):
- A request naming no amount for a capacity consumes `RequestPolicy.Default`
  if set, otherwise the whole capacity value.
- Without a RequestPolicy, the requested quantity is consumed as given
  (fractions allowed).
- The allocator checks sum of consumed <= capacity per device.
- Claim request keys must be the same literal strings as the device
  capacity keys (plain map lookup). Names without a domain are "assumed
  to be part of the driver's domain" (QualifiedName doc); in CEL they are
  `device.capacity["balloons.nri.io"].cpu`.

## KEP-5517: DRA node allocatable resources (gate DRANodeAllocatableResources)

Source: keps/sig-scheduling/5517-dra-node-allocatable-resources/README.md and
release-1.37 code (pkg/scheduler/.../dynamicresources/nodeallocatabledynamicresources.go,
pkg/kubelet/kuberuntime/kuberuntime_container_linux.go,
staging/src/k8s.io/component-helpers/resource/helpers.go).

- Device field `nodeAllocatableResources: map[ResourceName]NodeAllocatableResource`.
  Keys limited to cpu, memory, hugepages-*, ephemeral-storage.
  `mapping.deviceMultiplier` (per allocated device) or
  `mapping.capacityKey` + `mapping.capacityMultiplier` (consumed capacity
  times multiplier; both required together; capacityKey must exist in the
  device capacity map). `overhead.perPod/perContainer` for auxiliary cost.
- Scheduler (Filter): for each pod claim whose devices have a mapping,
  computes mapped quantity = consumedCapacity[capacityKey] * multiplier
  (or device count * deviceMultiplier), sums with the pod's native
  requests, and checks against node allocatable. A claim is counted once
  per pod even if several containers reference it. A claim whose device
  has a mapping cannot be shared by several pods
  (UnschedulableAndUnresolvable). The result is patched into
  `pod.status.nodeAllocatableResourceClaimStatuses` at PreBind (1.37
  name; renamed `additionalNodeAllocatableResources` for 1.38). Node
  allocatable itself (kubelet-reported) is unchanged.
- Kubelet: pod-level cgroup cpu.weight/cpu.max/memory.max include mapped
  quantities (quota only when every container has limits). Container
  level: CPU shares from native request only; CPU quota = native limit +
  mapped only if a native limit is set, otherwise unlimited. QoS class is
  computed from the spec only: a claim-only pod is BestEffort. The kubelet
  does not pin CPUs; "a CPU DRA driver using NRI to set cpuset.cpus" is the
  expected model. CPU manager static policy is not coordinated with DRA.
- When the gate is off the apiserver silently drops
  `nodeAllocatableResources` from ResourceSlices, so publishing it is
  harmless.
- Vendored k8s.io/api v0.37.0 has the types
  (`resourceapi.NodeAllocatableResource`, `NodeAllocatableMapping`).

## Balloons policy internals that matter

- `Balloon{Def, Instance, Cpus, Mems, PodIDs, Groups, ...}`; instances are
  per definition, index from 0; `PrettyName()` is `name[instance]`.
- Container placement: `AllocateResources` -> `allocateBalloon` ->
  `chooseBalloonDef` (annotation `balloon.balloons.resource-policy.nri.io`,
  then matchExpressions/namespaces in list order, then default) ->
  `allocateBalloonOfDef` with fill methods (same group/pod/namespace, new
  balloon, balanced). Then `resizeBalloon(bln, requested)` where requested
  = sum of native CPU requests of containers in the balloon, and
  `assignContainer`.
- `requestedMilliCpus(bln)` is the single place summing container
  requests; `freeMilliCpus`/`maxFreeMilliCpus` derive from it; used by
  fill methods, resize on allocate/release, NRT zones and metrics.
- Instances exist only when created by `applyBalloonDef` (MinBalloons) or
  by `FillNewBalloon` for a container. `freeBalloon` deletes an emptied
  balloon when more than MinBalloons instances exist.
- `setConfig` rebuilds everything from scratch (also on Reconfigure);
  `Reconfigure` then calls `Sync(all, all)` to re-place every container.
  `validateConfig` runs before state is replaced, but `p.bpoptions` is
  assigned early in `setConfig` (pre-existing wart).
- `pinCpuMem` sets cpuset.cpus (and cpu shares from the native request).
