# Balloons Policy

## Overview

### What Problems Does the Balloons Policy Solve?

The balloons policy addresses CPU, device, and container affinity
requirements by organizing workloads into **isolated CPU pools called
"balloons."** This approach solves several key challenges:

- **Flexible node resource partitioning**: Isolates and regroups
  containers from same or different pods, multi-pod applications and
  namespaces to share balloons.

- **Realtime requirements and latency-sensitivity**: Minimizes
  latencies by isolating critical workloads, tuning cache and physical
  CPU core sharing, memory and device locality, CPU frequencies and
  powersaving states, and process scheduling parameters, including
  realtime scheduling policies and I/O priorities.

- **Maximal server throughput**: Optimizes resource utilization by
  spreading memory and memory bandwidth hungry workloads across
  sockets, NUMA nodes and cache domains. Tunes the balance of memory
  accesses from lowest latency accesses to the closest memories only
  towards maximal bandwidth by using more memory channels through
  balanced cross-NUMA balloons within the same socket, or even more by
  crossing socket boundaries. Controls allowed memory types (DRAM,
  HBM, PMEM).

- **Different workloads, different servers, different rules**:
  Allocates CPUs for different containers based on different CPU
  allocation preferences. Preferences per worker node or worker node
  group. Supports both pre-allocation of CPUs and on-demand
  allocations as containers are created, and both always static and
  dynamically growing/shrinking balloons.

### Organization of this document

This document is organized so you can either read it top-to-bottom or
jump directly to the sections below:

- **[Integration with Kubernetes](#integration-with-kubernetes)**
- **[Installation and Configuration](#installation-and-configuration)**
- **[Configuration Options](#configuration-options)**
  - **[Container-to-Balloon Assignment](#container-to-balloon-assignment)**
  - **[CPUs-to-Balloon Selection](#cpus-to-balloon-selection)**
  - **[Memories-to-Balloon Selection](#memories-to-balloon-selection)**
  - **[Container Tuning](#container-tuning)**
  - **[CPU Tuning](#cpu-tuning)**
  - **[Built-in Balloon Types](#built-in-balloon-types)**
  - **[Toggle and Reset Pinning Memory, CPUs, and Containers](#toggle-and-reset-pinning-memory-cpus-and-containers)**
  - **[Visibility, Scheduling, Metrics, Logging, Debugging](#visibility-scheduling-metrics-logging-debugging)**
- **[Cookbook](#cookbook)**
  - **[Latency-Critical Containers](#latency-critical-containers)**
  - **[Maximum Memory Bandwidth Containers](#maximum-memory-bandwidth-containers)**
  - **[Workload-Aware Hyperthread Sharing](#workload-aware-hyperthread-sharing)**
- **[Troubleshooting](#troubleshooting)**

### Integration with Kubernetes

The balloons policy integrates into the Kubernetes stack as an NRI
(Node Resource Interface) plugin that works with container runtimes
(containerd or CRI-O):

1. **Runtime Integration**: Runs as a DaemonSet on each node,
   intercepting container lifecycle events through NRI.

2. **Dynamic Configuration**: Uses Kubernetes Custom Resources (CRs)
   for configuration, supporting cluster-wide, node-group, and
   node-specific settings.

3. **Pod Annotations**: Allows fine-grained control through pod and
   container annotations.

4. **Topology Awareness**: Can expose balloon topology through
   NodeResourceTopology CRs for scheduler integration and for
   cluster-wide inspection of containers CPU affinity.

The policy evaluates each container when it starts, assigns it to an
appropriate balloon based on configuration rules, and sets its CPU and
memory affinity accordingly.

## Installation and Configuration

### Prerequisites

- Kubernetes 1.24+
- Helm 3.0.0+
- Container runtime with NRI support:
  - containerd 1.7.0+ or CRI-O 1.26.0+
  - NRI feature enabled in the runtime (this is the default in
    up-to-date runtimes).

### Installing with Helm

Add the NRI plugins Helm repository:

```sh
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm repo update
```

Install the balloons policy with default configuration:

```sh
helm install nri-resource-policy-balloons nri-plugins/nri-resource-policy-balloons --namespace kube-system
```

Install with a custom configuration from a values file:

```sh
cat > balloons.values.helm.yaml <<EOF
nri:
  runtime:
    patchConfig: true
  plugin:
    index: 10

config:
  agent:
    nodeResourceTopology: true
    podResourceAPI: false
  allocatorTopologyBalancing: true
  pinCPU: true
  pinMemory: false
  reservedResources:
    cpu: cpuset:0
  balloonTypes:
  - name: high-priority
    minCPUs: 4
    maxCPUs: 8
    namespaces:
    - production
    preferNewBalloons: true
  control:
    rdt:
      enable: false
      partitions:
      options:
  log:
    debug:
    - policy
    klog:
      skip_headers: true
    source: true
EOF

helm install nri-resource-policy-balloons nri-plugins/nri-resource-policy-balloons \
  --namespace kube-system \
  -f balloons.values.helm.yaml
```

### Uninstalling with Helm

Uninstalling will not restore CPU and memory pinning of running
containers. See [Reset CPU and MemoryPinning](#reset-cpu-and-memory-pinning)
to allow containers to use any CPU and memory again.

Uninstalling leaves behind the BalloonsPolicy custom resource
definition (CRD). Because installation skips installing new
BalloonsPolicy CRD if it already exists, it is recommended to manually
remove the CRD after uninstalling.

```sh
helm uninstall nri-resource-policy-balloons -n kube-system

kubectl delete crd balloonspolicies.config.nri
```

### Managing Configuration with kubectl

View the current balloons policy configuration:

```sh
# List all balloons policy configurations
kubectl get balloonspolicies.config.nri -n kube-system

# View the default configuration
kubectl get balloonspolicies.config.nri/default -n kube-system -o yaml
```

Edit the configuration:

```sh
kubectl edit balloonspolicies.config.nri/default -n kube-system
```

The policy watches for configuration changes and automatically
reconfigures itself when the configuration is updated.

### Configuration Scopes

The balloons policy supports three levels of configuration precedence:

1. **Default configuration** (lowest precedence): Applies to all nodes
   without more specific configuration
   - Resource name: `default`

2. **Group-specific configuration**: Applies to nodes labeled with a
   configuration group
   - Resource name: `group.$GROUP_NAME`
   - Node label: `config.nri/group=$GROUP_NAME`

3. **Node-specific configuration** (highest precedence): Applies to a
   single named node
   - Resource name: `node.$NODE_NAME`

**Example**: Create a group-specific configuration for high-memory nodes:

```sh
# Create the configuration
kubectl apply -f - <<EOF
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: group.high-memory
  namespace: kube-system
spec:
  reservedResources:
    cpu: cpuset:0-1
  balloonTypes:
  - name: memory-intensive
    shareIdleCPUsInSame: numa
    maxBalloons: 4
    preferNewBalloons: true
    allocatorTopologyBalancing: true
    namespaces:
    - analytics
EOF

# Label nodes to use this configuration
kubectl label node node-1 config.nri/group=high-memory
kubectl label node node-2 config.nri/group=high-memory
```

**Example**: Create a node-specific configuration:

```sh
kubectl apply -f - <<EOF
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: node.special-node
  namespace: kube-system
spec:
  reservedResources:
    cpu: 4000m
  balloonTypes:
  - name: gpu-workloads
    preferCloseToDevices:
      - /sys/class/drm/card0
EOF
```

## Configuration Options

### Core Concepts

A **balloon** is a set of CPUs shared by a set of containers. Key
principles:

- Every container is assigned to exactly one balloon.

- Every CPU belongs to at most one balloon.

- A container is allowed to use all CPUs in its balloon.

- Optionally, containers can share "idle CPUs" (CPUs not in any
  balloon).

- Containers never access CPUs from other balloons.

### Container-to-Balloon Assignment

These options control which containers are assigned to which
balloons. The policy first chooses a balloon type for a container, and
then an existing or a new balloon instance of that type.

#### Choosing Balloon Type

**Balloon type selection for a new container:**
1. Balloon type effective for the container is specified by its pod
   [annotation](#pod-annotations-for-container-overrides). This skips
   matching in user-defined and built-in balloon types. (Highest
   precedence.)
2. Implicit [built-in reserved balloon](#reserved-balloon) type
   matches kube-system containers, unless configured otherwise.
3. The first balloon type in the `balloonTypes` list where a
   `matchExpression` or a `namespace` matches the container.
4. Implicit [built-in default balloon](#default-balloon) type matches
   any container as a fallback, unless configured otherwise.
5. If no match, creating the container fails.

**`namespaces`** (list of strings)
- Assigns containers from matching namespaces to this balloon type.
- Supports wildcards (e.g., `"prod-*"`, `"*"`).

```yaml
balloonTypes:
- name: production
  namespaces:
  - prod-*
  - critical
```

**`matchExpressions`** (list of expressions)
- Evaluates expressions against container attributes.
- First balloon type with a matching expression is selected.
- Supports keys:
  - `name` (container name)
  - `namespace`
  - `qosclass` (Guaranteed, Burstable or BestEffort)
  - `labels/KEY` (container's label)
  - `pod/name` (pod's name)
  - `pod/qosclass`
  - `pod/labels/KEY`
  - `pod/id`
  - `pod/uid`
- Supports operators:
  - `Equals`, `NotEqual`: key value is (not) equal to the only value
  - `In`, `NotIn`: key value is (not) in listed values
  - `Exists`, `NotExists`: key is [not] present, values ignored
  - `AlwaysTrue`
  - `Matches`, `MatchesNot`: key value matches (not) a single globbing pattern
  - `MatchesAny`, `MatchesNone`: key value matches any/none of a set of globbing patterns

Example: first pick logger and monitor containers into low-priority
balloons from any pod, including high and critical priority pods. Then
pick all (remaining) containers from high priority pods into
latency-critical balloons.

```yaml
balloonTypes:
- name: low-priority
  matchExpressions:
  - key: name
    operator: In
    values:
    - logger
    - monitor
- name: latency-critical
  matchExpressions:
  - key: pod/labels/priority
    operator: In
    values:
    - high
    - critical
```

**Composite balloons with `componentCreation: balance-balloons`:**
- When a balloon type has `components` list, `componentCreation`
  enables controlling which component balloon types are used for
  allocating its CPUs. The default is all of them.
- `componentCreation: balance-balloons` creates only one component -
  the one whose balloon type has the fewest instances.
- Use this to alternate container assignments across multiple balloon
  types.

```yaml
balloonTypes:
- name: balanced-a-b
  components:
  - balloonType: type-a
  - balloonType: type-b
  componentCreation: balance-balloons
- name: type-a
  ...
- name: type-b
  ...
```

#### Choosing Balloon Instance

Once the policy has chosen a balloon type for a container, following
options determine which instance of that type receives the container.

If no instance, new or existing, can get enough CPUs to fit the
container, the container creation fails.

**`groupBy`** (string)
- Groups containers into the same balloon instance based on expression
  evaluation.
- Expression uses substitution: `${pod/labels/mylabel}` replaced with
  label value. Substitution supports the keys as listed in
  `matchExpressions` above.
- Containers with the same expression value go to the same balloon.

```yaml
balloonTypes:
- name: app-instances
  groupBy: "${pod/labels/app}-${pod/labels/instance}"
```

**Pod spreading options:**
- `preferSpreadingPods: false` (default): Containers from the same pod
  prefer the same balloon.
- `preferSpreadingPods: true`: Containers from the same pod prefer
  different balloons.

**Namespace grouping:**
- `preferPerNamespaceBalloon: false` (default): Namespace has no
  effect on balloon selection.
- `preferPerNamespaceBalloon: true`: Containers in the same namespace
  prefer the same balloon and different namespaces prefer different
  balloons.

Example: assign all containers into per-namespace balloons. Use the
same balloon type for every container by placing it before built-in
types in the list.

```yaml
balloonTypes:
- name: per-namespace-balloon
  namespaces:
  - "*"
  preferPerNamespaceBalloon: true
  allocatorTopologyBalancing: true
- name: reserved
- name: default
```

**`preferNewBalloons`** (boolean, default: `false`)
- `false`: Prefer filling existing balloons, inflate if needed up to
  maximum size, create new balloon as last resort.
- `true`: Prefer creating new balloons for exclusive CPU access (if
  CPUs available).

```yaml
balloonTypes:
- name: exlusive-cpus
  allocatorTopologyBalancing: true
  preferNewBalloons: true
```

### CPUs-to-Balloon Selection

These options control which CPUs are selected when creating or
resizing balloons.

#### Static CPU Preferences

**`preferIsolCpus`** (boolean, default: `false`)
- `true`: Prefer kernel-isolated CPUs (from `isolcpus` kernel parameter)
- Tip: use with `minCPUs`, `maxCPUs`, and `minBalloons` to
  pre-allocate CPUs when using this option.
- Warning: If insufficient isolated CPUs exist, balloons may include
  non-isolated CPUs, too. If the application is unaware of this, it is
  likely to cause unwanted Linux scheduling behavior.

Example: pre-allocate two fixed-size one-CPU-balloons from
kernel-isolated CPUs.

```yaml
balloonTypes:
- name: kernel-isolated-cpu
  preferIsolCpus: true
  minCPUs: 1
  maxCPUs: 1
  minBalloons: 2
```

**`preferCoreType`** (string: `"efficient"` or `"performance"`)
- On hybrid architectures (P/E cores), prefers the specified core type.
- `"performance"`: Select high-performance cores.
- `"efficient"`: Select power-efficient cores.

```yaml
balloonTypes:
- name: background-tasks
  preferCoreType: efficient
```

**`preferCloseToDevices`** (list of strings)
- Prefers CPUs topologically close to specified devices.
- Device paths like
  - `/sys/class/net/eth0`
  - `/sys/class/drm/card0`
  - `/sys/devices/system/cpu/cpu14/cache/index2`
  - `/sys/devices/system/node/node0`
- First device in list has highest priority.
- Automatically adds anti-affinity between listed devices and other balloon types.

```yaml
balloonTypes:
- name: gpu-workloads
  preferCloseToDevices:
  - /sys/class/drm/card0
- name: network-io
  preferCloseToDevices:
  - /sys/class/net/eth0
```

#### Dynamic CPU Preferences

**`allocatorTopologyBalancing`** (boolean, inherits from policy-level
setting if not specified)
- `true`: Spread balloons across hardware topology (NUMA/die/package
  with most free CPUs).
  - Reduces interference when system is partially loaded.
  - Helps with future balloon inflation within same NUMA/die/package.
- `false` (default): Pack balloons tightly into same hardware topology
  elements.
  - Keeps large portions of hardware idle for power saving.
  - More interference between balloons.

**`preferSpreadOnPhysicalCores`** (boolean, inherits from policy-level
setting if not specified)
- `true`: Allocate logical CPUs from separate physical cores
  - Prevents containers from competing on same physical core resources.
  - Allows more interference between different balloons.
- `false` (default): Pack logical CPUs tightly to minimum number of
  physical cores
  - Reduces inter-balloon interference.
  - Containers in the same balloon share physical core resources.
- Recommendation: use `loads` and `loadClasses` for better control in
  sharing physical cores and low-level caches, and/or
  `hideHyperthreads` to prevent any process in any balloon from using
  the other hyperthread from the same physical core.

**`loads`** (list of strings)
- Marks logical CPUs and their surroundings (physical core, cache) as
  loaded by certain balloons.
- Avoids selecting CPUs from surroundings for other
  same-load-generating balloons.
- Every load is defined in `loadClasses` (see below).

```yaml
balloonTypes:
- name: compute-heavy
  loads:
  - avx-compute
```

**`loadClasses`** (list, policy-level configuration):

Defines system load characteristics that balloons can generate. The
CPU allocator uses this information to avoid overloading hardware
resources.

Each load class defines:
- `name` (string): Load class identifier (referenced in balloon types'
  `loads` lists).
- `level` (string): Hardware topology level affected:
  - `"core"`: Load affects physical CPU core resources.
  - `"l2cache"`: Load affects L2 cache block.
- `overloadsLevelInBalloon` (boolean, default: `false`)
  - `false`: CPUs within balloon can be from same core/cache (locality
    prioritized).
  - `true`: CPUs within balloon should avoid same core/cache (avoid
    self-interference).

How load classes affect CPU allocation:
1. Balloon type declares it generates certain loads.
2. When allocating CPUs for this balloon, allocator marks affected
   topology levels as loaded.
3. When allocating CPUs for another balloon with the same load class
   (same or different balloon type), allocator avoids loaded topology
   levels.
4. Result: Balloons generating similar loads get CPUs from separate
   cores/caches. Balloons generating different loads on same
   cores/caches are allowed to share the same surroundings.

Example: containers in `compute-heavy-1` and `compute-heavy-2`
balloons should get only one hyperthread from every physical core, and
they should not share physical cores. Containers in `light-compute`
are marked to load cores, too, but only to avoid taking both
hyperthreads from the same cores. This leaves the other hyperthread
free for compute heavy workloads. A `heavy-avx` and a `light-avx` load
can share the same physical core, but two `heavy-avx` or two
`light-avx` loads should not.

```yaml
balloonTypes:
- name: compute-heavy-1
  loads:
  - heavy-avx
- name: compute-heavy-2
  loads:
  - heavy-avx
- name: light-compute
  loads:
  - light-avx

loadClasses:
- name: heavy-avx
  level: core
  overloadsLevelInBalloon: true # Each CPU from different physical core
- name: light-avx
  level: core
  overloadsLevelInBalloon: true # Spread on different physical cores
                                # to leave the other thread free for
                                # compute-heavy balloons
```

**`components`** (list of objects):

For balloons with diverse CPU requirements, use composite balloons
where each component specifies different requirements:

```yaml
balloonTypes:
- name: hybrid-workload
  components:
  - balloonType: near-gpu
  - balloonType: near-network
- name: near-gpu
  preferCloseToDevices:
  - /sys/class/drm/card0
- name: near-network
  preferCloseToDevices:
  - /sys/class/net/eth0
```

#### Balloon Size Control

**`minCPUs`** (integer, default: 0)
- Minimum number of CPUs in any balloon of this type.
- When balloon is created or deflated, it always has at least this many CPUs.
- Useful for
  - ensuring minimum performance guarantees
  - allocating exactly wanted number of special CPUs (from isolcpus,
  reserved CPU set, efficient-cores, CPUs local to a device, ...)
  - partitioning hardware block-by-block (take all CPUs from a
  physical core, L2 cache domain, NUMA node, compute die or socket at
  once.

**`maxCPUs`** (integer, default: 0 = unlimited)
- Maximum number of CPUs in any balloon of this type.
- Balloon will not inflate beyond this limit. The policy has to create
  a new balloon instead.
- Set to -1 to prevent containers matching this balloon from running
  on the node (see cookbook).

**Fully dynamic sizing:**
- Set `minCPUs: 0` and `maxCPUs: 0` or leave them undefined.
- Balloon size determined entirely by container CPU requests.

**Fixed size:**
- Set `minCPUs` and `maxCPUs` to the same value.

```yaml
balloonTypes:
- name: fixed-quad
  minCPUs: 4
  maxCPUs: 4
- name: dynamic-small
  minCPUs: 1
  maxCPUs: 8
- name: unlimited
  minCPUs: 2
  maxCPUs: 0
```

#### CPU Allocation Priority

**`minBalloons`** (integer, default: 0)
- Number of balloon instances pre-created when policy starts or
  reconfigures. `allocatorPriority` and the order in the
  `balloonTypes` list affect the order of instantiating `minBalloons`
  of different balloon types (see below).
- Ensures critical balloons always exist and have CPUs before other
  balloons.
- Yet new balloon instances of this type may be dynamically created
  and destroyed, the number of instances never goes below
  `minBalloons`.

**`maxBalloons`** (integer, default: 0 = unlimited)
- Maximum number of balloon instances allowed to co-exist.
- Prevents creating new balloons beyond this limit.

**`allocatorPriority`** (string: `"high"`, `"normal"` (default), `"low"`, `"none"`)
- At policy initialization, balloons are pre-created in this order:
  1. Balloon types with `priority: high` (in list order)
  2. Balloon types with `priority: normal` (in list order)
  3. Balloon types with `priority: low` (in list order)

```yaml
balloonTypes:
- name: critical-service
  minBalloons: 2
  maxBalloons: 2
  minCPUs: 4
  maxCPUs: 4
  allocatorPriority: high
- name: best-effort
  allocatorPriority: low
```

### Memories-to-Balloon Selection

If `pinMemory: true`, the policy allows containers to use memory only
from NUMA nodes closest to their balloon's CPUs. This node set may be
larger if there is not enough memory in the closest nodes
alone. Pinning can be fine-tuned to include only certain memory types
for certain balloons and containers.

**`memoryTypes`** (list of strings):
- Restricts memory types available to containers: `"DRAM"`, `"HBM"`, `"PMEM"`.
- Default: All memory types in system are allowed.
- Can be overridden per container with `memory-type.resource-policy.nri.io` annotation.
- Effective only when `pinMemory: true`.

```yaml
pinMemory: true

balloonTypes:
- name: hbm-only
  memoryTypes:
  - HBM
- name: flexible
  memoryTypes:
  - DRAM
  - PMEM
- name: default
  pinMemory: false
```

### Container Tuning

These options configure how containers behave within their balloons.

#### Scheduling and Priority

**`schedulingClass`** (string)
- References a scheduling class defined in the `schedulingClasses`
  list.
- Sets Linux scheduling policy, priority, nice value, and I/O class
  for containers.
- Can be overridden with `scheduling-class.resource-policy.nri.io` pod
  annotation.

**`schedulingClasses`** (list, policy-level configuration):

Each scheduling class defines:
- `name` (string): Class name referenced by `schedulingClass` in
  balloon types.
- `policy` (string): Linux scheduling policy: `"none"`, `"other"`,
  `"fifo"`, `"rr"`, `"batch"`, `"idle"`, `"deadline"`.
- `priority` (integer): Scheduling priority, depends on `policy`, see
  `sched_setscheduler(2)`.
- `flags` (list): Scheduling flags: `"reset-on-fork"`, `"reclaim"`,
  `"dl-overrun"`, `"keep-policy"`, `"keep-params"`,
  `"util-clamp-min"`, `"util-clamp-max"`.
- `nice` (integer): Nice value for container process (-20 to 19).
- `runtime` (integer): Runtime for deadline policy (microseconds).
- `deadline` (integer): Deadline for deadline policy (microseconds).
- `period` (integer): Period for deadline policy (microseconds).
- `ioClass` (string): I/O class: `"none"`, `"rt"` (realtime), `"be"`
  (best-effort), `"idle"`.
- `ioPriority` (integer): I/O priority, see `ionice(1)`.

```yaml
balloonTypes:
- name: high-priority
  schedulingClass: critical
schedulingClasses:
- name: critical
  policy: rr
  priority: 50
  ioClass: rt
  ioPriority: 0
- name: background
  policy: idle
  ioClass: idle
```

#### Sharing idle CPUs

**`shareIdleCPUsInSame`** (string: `"system"`, `"package"`, `"die"`,
`"numa"`, `"l2cache"`, `"core"`)
- Allows containers to use "idle CPUs" (not in any balloon) in
  addition to balloon's own CPUs.
- Value sets locality constraint for which idle CPUs can be used, with
  respect to balloon's own CPUs.
  - `"system"`: All idle CPUs in the system.
  - `"package"`: Idle CPUs in same socket(s).
  - `"die"`: Idle CPUs in same die(s).
  - `"numa"`: Idle CPUs in same NUMA node(s).
  - `"l2cache"`: Idle CPUs sharing same L2 caches.
  - `"core"`: Idle hyperthreads in same physical cores.
- Containers in all balloon instances of multiple balloon types can be
  allowed to run on the same idle CPUs.
- Balloon type's CPU tuning does not affect these extra CPUs.

```yaml
balloonTypes:
- name: burstable-in-numa
  shareIdleCPUsInSame: numa
- name: one-cpu-with-bonus-thread
  maxCPUs: 1
  shareIdleCPUsInSame: core
```

#### Hyperthread Visibility

**`hideHyperthreads`** (boolean, default: `false`)
- `true`: Containers can use only one hyperthread from each physical
  core in the balloon.
  - Hidden hyperthreads remain completely idle (not available to any
    container).
  - Useful for workloads that don't benefit from hyperthreading.
  - Balloon with 16 logical CPUs from 8 cores allows using only 8 CPUs.
- `false`: Containers can use all hyperthreads.
- Can be overridden with `hide-hyperthreads.resource-policy.nri.io`
  pod annotation that hides hyperthreads only in affected containers
  rather than of all containers in certain balloon types.

```yaml
balloonTypes:
- name: compute-intensive
  hideHyperthreads: true
```

#### Pod Annotations for Container Overrides

All pod annotations below are effective to all containers in a pod
(annotation key ending `.../pod` or missing `/`), or to named
containers in the pod (key ending `.../container.CONTAINER_NAME`). The
latter override the former.

**Balloon type selection:**
```yaml
balloon.balloons.resource-policy.nri.io: BALLOON_TYPE
balloon.balloons.resource-policy.nri.io/pod: BALLOON_TYPE
balloon.balloons.resource-policy.nri.io/container.CONTAINER_NAME: BALLOON_TYPE
```

**Scheduling class:**
```yaml
scheduling-class.resource-policy.nri.io/container.CONTAINER_NAME: CLASS_NAME
```

**Hyperthread hiding:**
```yaml
hide-hyperthreads.resource-policy.nri.io/container.CONTAINER_NAME: "true"
```

**Preserve existing pinning (opt-out of policy management):**
```yaml
cpu.preserve.resource-policy.nri.io/container.CONTAINER_NAME: "true"
memory.preserve.resource-policy.nri.io/container.CONTAINER_NAME: "true"
```

**Memory type:**
```yaml
memory-type.resource-policy.nri.io/container.CONTAINER_NAME: HBM,DRAM
```

### CPU Tuning

These options configure CPU behavior and power management.

**`cpuClass`** (string)
- References a CPU class defined in `control.cpu.classes`
  (policy-level configuration).
- Applied when balloon is created, inflated, or deflated.
- Configures frequency scaling and C-states for CPUs in the balloon.

**`idleCPUClass`** (string, policy-level configuration)
- CPU class for idle CPUs (not in any balloon).
- Applied when CPUs are removed from balloons.

**`control.cpu.classes`** (object, policy-level configuration):

Each CPU class (keyed by name) can define:

- `minFreq` (integer): Minimum CPU frequency in kHz.
- `maxFreq` (integer): Maximum CPU frequency in kHz.
- `uncoreMinFreq` (integer): Minimum uncore frequency in kHz.
- `uncoreMaxFreq` (integer): Maximum uncore frequency in kHz.
- `disabledCstates` (list): C-state names to disable (e.g., `["C6", "C8"]`).
  - Disabling deep C-states reduces latency by preventing deep sleep.
  - Disabling intermediate C-states keeps CPU more responsive longer
    after use, but allows it to enter deeper power saving states if
    not needed.
  - List available C-states: `grep
    . /sys/devices/system/cpu/cpu0/cpuidle/state*/name`.

```yaml
balloonTypes:
- name: latency-critical
  cpuClass: turbo
- name: best-effort
  cpuClass: normal
idleCPUClass: powersave

control:
  cpu:
    classes:
      turbo:
        minFreq: 3000000
        maxFreq: 3600000
        uncoreMinFreq: 2000000
        uncoreMaxFreq: 2400000
        disabledCstates: [C6, C8, C10]
      normal:
        minFreq: 1200000
        maxFreq: 3000000
      powersave:
        minFreq: 800000
        maxFreq: 1200000
```

### Built-in Balloon Types

The policy includes two built-in balloon types that can be customized
and whose position in the balloon type list can be changed.

#### Reserved Balloon

**Purpose**: Runs system containers (typically from `kube-system` namespace)

**Default behavior** (when not explicitly defined):
- Automatically placed first in balloon types list.
- Captures containers from `kube-system` namespace and namespaces
  matching `reservedPoolNamespaces`.
- Uses CPUs specified in `reservedResources.cpu`. If a `cpuset` is
  defined, those CPUs will be preferred when inflating the reserved
  balloon, and correspondingly avoided by other balloons. Reserved
  balloon can inflate beyond this cpuset if required by its
  containers.

**`reservedResources`** (policy-level configuration):
- `cpu` (string): CPUs for reserved balloon.
  - Preferred CPUs: `"cpuset:0,48"` prefers using CPU 0 and 48. Uses
    many enough to satisfy container CPU requests, but no more than
    that.
  - Quantity: `"2000m"` or `"2"` uses at least 2 CPUs.
  - If `minCPUs` is explicitly set for `reserved` balloon type, that
    overrides the quantity.

**`reservedPoolNamespaces`** (list of strings, policy-level configuration):
- Additional namespaces (beyond `kube-system`) assigned to reserved balloon.
- Supports wildcards.

**Customizing reserved balloon:**

```yaml
# Policy-level settings
reservedResources:
  cpu: cpuset:0,48  # preferred specific CPUs
reservedPoolNamespaces:
  - kube-system
  - monitoring

# Explicitly define 'reserved' balloon type for more control.
# Because "reserved" is moved below "own-cpus", kube-system and
# monitoring pods with priority=high label will get their own CPUs
# instead of sharing CPUs with other reserved pods.
balloonTypes:
- name: own-cpus
  preferNewBalloons: true
  matchExpressions:
  - key: pod/labels/priority
    operator: In
    values:
    - high
- name: reserved  # Must be named 'reserved'
  shareIdleCPUsInSame: numa
```

#### Default Balloon

**Purpose**: Catches all containers not matched by user-defined balloon types.

**Default behavior** (when not explicitly defined):
- Automatically placed last in balloon types list.
- Captures all remaining containers.
- Uses any remaining CPUs not allocated to other balloons.

**Customizing default balloon:**

```yaml
balloonTypes:
- name: production
  namespaces:
    - prod-*
- name: default  # Must be named 'default'
  minCPUs: 1
  shareIdleCPUsInSame: package # Allow bursting without crossing socket boundary.
  minBalloons: 2 # Create one balloon in both sockets in a 2-socket system.
  maxBallons: 2
  namespaces:
  - "*" # Match all containers not matched in above types
```

### Toggle and Reset Pinning Memory, CPUs, and Containers

#### Memory Pinning

**`pinMemory`** (boolean, policy-level, default: `true`)
- `true`: Pin containers to NUMA nodes closest to their balloon's CPUs.
- `false`: Allow containers to use memory from any NUMA node.
- Can be overridden per balloon type.
- Warning: Pinning may cause OOM kills if pinned memory nodes have
  insufficient memory.

#### CPU Pinning

**`pinCPU`** (boolean, policy-level, default: `true`)
- `true`: Restrict containers to their balloon's CPUs (and optionally
  shared idle CPUs).
- `false`: Containers can use any CPUs in the system.
- Usually left as `true` for proper balloon isolation.

#### Managed CPUs

**`availableResources`** (object, policy-level configuration):
- `cpu` (string): CPUset managed by the policy.
- All balloons use only CPUs from this set.
- Useful for reserving CPUs for non-policy-managed workloads.

```yaml
availableResources:
  cpu: cpuset:48-95,144-191  # Use only socket 1 in 2-socket system
```

#### Managed Containers

The balloons policy can completely ignore selected containers.

**`preserve`** (object, policy-level configuration):
- `matchExpressions` (list): Container match expressions.
- Containers matching these expressions are not managed by the policy.
- Their existing CPU/memory pinning (or lack thereof) is preserved.

Useful for
- analyzers and other containers that need access to every CPU
  ```yaml
  preserve:
    matchExpressions:
    - key: name
      operator: In
      values:
      - analyzer
      - debugger
  ```

- special containers managed by external policies, for instance on
  CPUs not in `availableResources`.
  ```yaml
  preserve:
    matchExpressions:
    - key: pod/labels/non-balloon-cpus
      operator: Equals
      values:
      - "true"
  ```

- balloons configures only few special containers while others can run
  unrestricted on any CPU in the system.
  ```yaml
  preserve:
    matchExpressions:
    - key: pod/labels/workload-type
      operator: NotIn
      values:
      - "idle"
  balloonTypes:
  - name: idle-workloads
    matchExpressions:
    - key: pod/labels/workload-type
      operator: In
      values:
      - "idle"
    schedulingClass: idle
  schedulingClasses:
  - name: idle
    policy: idle
    ioClass: idle
  pinCPUs: false
  pinMemory: false
  ```

#### Reset CPU and Memory Pinning

Running containers can be "reset" to allow accessing all CPUs and
memories in the system by applying a configuration that pins them
accordingly.

```sh
kubectl apply -f - <<EOF
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
  balloonTypes:
  - name: reserved
    namespaces:
    - "*"
    shareIdleCPUsInSame: system
    # preferIsolCpus: true # uncomment to use kernel isolcpus, too
  reservedResources:
    cpu: 1000m
  pinCPU: true
  pinMemory: true
EOF
```

### Visibility, Scheduling, Metrics, Logging, Debugging

These options control observability of balloons and their containers,
including making them visible to Kubernetes scheduler.

#### NodeResourceTopology Integration

**`agent.nodeResourceTopology`** (boolean, policy-level, default:
`false`)
- `true`: Expose balloons as topology zones in
  `noderesourcetopologies.topology.node.k8s.io` CRs.
- Enables topology-aware scheduling, exposes created balloon instances
  as zones.

**`showContainersInNrt`** (boolean, can be set at policy-level and
overridden in balloon types, default: `false`)
- `true`: Include container names and resource affinities in
  NodeResourceTopology CRs.
- Policy-level setting applies to all balloons by default.
- Balloon-type setting overrides policy-level default.
- Warning: May generate significant API server traffic with many
  containers.

```yaml
# Enable topology exposure at policy level
agent:
  nodeResourceTopology: true

# Do not show containers in NodeResourceTopology by default.
showContainersInNrt: false

# Override for specific balloon type
balloonTypes:
- name: devel
  namespaces:
  - "devel-*"
  showContainersInNrt: true # Show development container CPU affinities for debugging
```

Example: print all balloons in all nodes in a single table
```sh
kubectl get noderesourcetopologies.topology.node.k8s.io -o json | jq -r '
  ["NODE","BALLOON","CPUSET","SHARED_CPUSET"],
  (
    .items.[] as $node
    | $node.zones[]
    | select(.type == "balloon")
    | [
        $node.metadata.name,
	.name,
	(.attributes[] | select(.name=="cpuset") | .value),
	(.attributes[] | select(.name=="shared cpuset") | .value)
      ]
  )
  | @tsv'
```

Example: print all containers whose balloon type has effective
`showContainersInNrt: true` in all nodes
```sh
kubectl get noderesourcetopologies.topology.node.k8s.io -o json | jq -r '
  ["NODE","BALLOON","CONTAINER","CPUS","MEMS"],
  (
    .items.[] as $node
    | $node.zones[]
    | select(.type == "allocation for container")
    | [
        $node.metadata.name,
        .parent,
        .name,
        (.attributes[] | select(.name=="cpuset") | .value),
        (.attributes[] | select(.name=="memory set") | .value)
      ]
  )
  | @tsv'
```

NodeResourceTopology information can be used in Kubernetes scheduling
through [Topology-aware scheduler
plugin](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/pkg/noderesourcetopology/README.md).

#### Instrumentation and Metrics

**`instrumentation`** (object, policy-level configuration):
- `httpEndpoint` (string): HTTP server address (e.g., `":8891"`).
- `prometheusExport` (boolean): Enable Prometheus metrics at
  `/metrics` endpoint.
- `reportPeriod` (string): Aggregation interval for polled metrics
  (e.g., `"10s"`).
- `metrics.enabled` (list): Glob patterns for metrics to collect
  (e.g., `["policy"]`).
- `samplingRatePerMillion` (integer): Tracing sample rate (e.g.,
  `100000` for 10%).
- `tracingCollector` (string): External tracing endpoint (e.g.,
  `"otlp-http"`).

**Available metrics:**
- Balloon instances and their CPUs.
- Containers assigned to each balloon.
- CPU allocation and utilization.

```yaml
instrumentation:
  httpEndpoint: :8891
  prometheusExport: true
  reportPeriod: 30s
  metrics:
    enabled:
      - policy
      - '*'  # All metrics
  samplingRatePerMillion: 100000
```

**Accessing metrics:**

```sh
# Direct access (if pod IPs are reachable)
for podip in $(kubectl get pod -n kube-system -l "app.kubernetes.io/instance=nri-resource-policy-balloons" -o=jsonpath='{.items[*].status.podIP}'); do
  curl -s http://$podip:8891/metrics
done

# Port forwarding
kubectl port-forward -n kube-system daemonset/nri-resource-policy-balloons 8891:8891 &
curl http://localhost:8891/metrics
```

#### Logging and Debugging

**`log`** (object, policy-level configuration):
- `debug` (list): List of components to enable debug logging for.
  - `nri-plugin` - NRI communication with the container runtime
  - `policy` for balloons policy
  - `expression` for matchExpression and groupBy expressions
  - `cache` for container and pod cache
  - `cpu` for low-level CPU tuning controls (frequencies, cstates)
  - `cpuallocator` for lowest-level CPU allocator below the policy
  - `sysfs` for hardware discovery
  - `"*"` for all
- `source` (boolean): Prefix messages with logger source name.
- `klog` (object): klog-specific options (see klog documentation).

```yaml
log:
  debug:
    - policy
    - nri-plugin
  source: true
  klog:
    skip_headers: true
```

**View logs:**

Show policy decision in assigning container CTRNAME in PODNAME in NAMESPACE to a balloon:

```sh
kubectl logs -n kube-system daemonset/nri-resource-policy-balloons | grep 'assigning container NAMESPACE/PODNAME/CTRNAME'
```

**Inspect container information and cgroups:**

Inspect container CTRNAME in PODNAME in NAMESPACE:

```sh
CTR_ID=$(kubectl get pod -n NAMESPACE PODNAME -o jsonpath='{.status.containerStatuses[?(@.name=="CTRNAME")].containerID}' | sed 's|^[^:]*://||')
crictl inspect $CTR_ID | jq .info.runtimeSpec
```

Read effective CPU and memory sets from cgroups on the local node with the `kube-cgroups` script.

```sh
curl -OL https://raw.githubusercontent.com/containers/nri-plugins/refs/heads/main/scripts/testing/kube-cgroups
chmod a+rx kube-cgroups
./kube-cgroups -n NAMESPACE -p PODNAME -c CTRNAME -f 'cpuset.cpus.effective|cpuset.mems.effective'
```

## Cookbook

### Latency-Critical Containers

**Goal**: Minimize latency by preventing cache sharing and optimizing
CPU configuration.

**Requirements:**
- Containers should not share L2 cache to avoid cache contention.
- CPUs should run at maximum frequency.
- Disable deep C-states to avoid wakeup latency.
- Each container gets dedicated CPUs (no sharing between containers).

**Configuration:**

```yaml
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
  reservedResources:
    cpu: "2"
  pinCPU: true
  pinMemory: true
  allocatorTopologyBalancing: false  # Pack tightly for power efficiency
  idleCPUClass: powersave # Force to minimal frequencies for external processes

  balloonTypes:
  - name: ultra-low-latency
    # Match latency-critical containers
    matchExpressions:
    - key: pod/labels/latency
      operator: In
      values:
      - critical

    # Fixed size, pre-created balloons
    minBalloons: 4
    minCPUs: 4
    allocatorPriority: high

    # Each container gets its own balloon
    preferNewBalloons: true

    # CPU configuration for minimal latency
    cpuClass: ultra-low-latency

    schedulingClass: realtime

    # Mark CPUs as generating cache load
    loads:
    - l2-intensive

  - name: default
    namespaces:
    - "*"
    loads:
    - l2-intensive # avoid sharing L2 domains with critical containers
    cpuClass: normal # low to mid GHz to save power budget for critical containers

  # Define L2 cache load to prevent containers in other balloons from sharing cache
  loadClasses:
  - name: l2-intensive
    level: l2cache
    overloadsLevelInBalloon: false  # Share L2 between CPUs within balloon

  # CPU classes for frequency and C-state control
  control:
    cpu:
      classes:
        ultra-low-latency:
          minFreq: 3500000
          maxFreq: 3900000
          uncoreMinFreq: 2400000
          uncoreMaxFreq: 2400000
          disabledCstates: [C6, C7, C8, C10]
        normal:
          minFreq: 800000
          maxFreq: 2500000
        powersave:
          minFreq: 800000
          maxFreq: 800000

  # Scheduling for high priority
  schedulingClasses:
  - name: realtime
    policy: fifo
    priority: 80
    ioClass: rt
    ioPriority: 0
```

**Pod annotation example:**

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: latency-critical-app
  labels:
    latency: critical
spec:
  containers:
  - name: app
    image: my-app:latest
    resources:
      requests:
        cpu: "4"
```

### Maximum Memory Bandwidth Containers

**Goal**: Maximize memory bandwidth by two alternative means:
1. balancing memory-bandwidth balloons across multiple NUMA nodes,
   each balloon using only the local (lowest-latency) memory nodes.
2. balancing each memory-bandwidth balloon to have equal number of
   CPUs from both NUMA nodes of single socket. These balloons are
   then balanced between sockets.

**Requirements:**
- CPUs from multiple NUMA nodes to access multiple memory channels.
- Balanced distribution across topology for maximum bandwidth.
- Containers can be large, using many CPUs.

**Configuration 1: Local NUMA accesses only**

For 2-socket 4-NUMA-node system where each where pods labelled
"memory-intensive" should be restricted to use local NUMA only in both
CPU and memory. Other pods should share all CPUs of either socket
except for CPUs reserved for workloads memory-intensive workloads.

```yaml
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
  reservedResources:
    cpu: cpuset:0
  pinCPU: true
  pinMemory: true
  allocatorTopologyBalancing: true  # Spread across NUMA nodes

  balloonTypes:
  - name: memory-bandwidth
    matchExpressions:
    - key: pod/labels/workload-type
      operator: In
      values:
      - memory-intensive
    # Each workload gets its own balloon
    preferNewBalloons: true
  - name: default
    # create one default balloon per socket for other workloads
    minBalloons: 2
    shareIdleCPUsInSame: package
```

**Configuration 2: balanced CPUs from both NUMAs of either socket**

For a 2-socket 4-NUMA-node system where each container needs an equal
number of CPUs from all NUMAs of either package, but needs to avoid
cross-socket allocation.

```yaml
  balloonTypes:
  - name: max-bandwidth-either-package
    # Composite balloon combining CPUs from all NUMA nodes from either package
    components:
    - balloonType: max-bandwidth-package0
    - balloonType: max-bandwidth-package1
    componentCreation: balance-balloons
    preferNewBalloons: true
    matchExpressions:
    - key: pod/labels/workload-type
      operator: In
      values:
      - max-bandwidth

  # Component balloon types - one per package
  - name: max-bandwidth-package0
    components:
    - balloonType: numa0
    - balloonType: numa1

  - name: max-bandwidth-package1
    components:
    - balloonType: numa2
    - balloonType: numa3

  # Component balloon types - one per NUMA
  - name: numa0
    preferCloseToDevices:
    - /sys/devices/system/node/node0
  - name: numa1
    preferCloseToDevices:
    - /sys/devices/system/node/node1
  - name: numa2
    preferCloseToDevices:
    - /sys/devices/system/node/node2
  - name: numa3
    preferCloseToDevices:
    - /sys/devices/system/node/node3
```

### Workload-Aware Hyperthread Sharing

**Goal**: Optimize physical core utilization by controlling which
workload types share cores.

**Scenario:**
- **Type A workloads**: High CPU utilization, always running (e.g.,
  batch processing), no bursts.
- **Type B workloads**: Bursty, short-duration spikes (e.g., request
  handling). Benefit from temporarily using more CPUs than requested.
- **Type C workloads**: Low priority background tasks. Benefit from
  from extra CPUs but must not slow down type B workloads.

**Strategy:**
- Prevent two Type A workloads from sharing physical cores (would
  compete heavily).
- Allow Type A + Type B on same physical cores.
- Allow Type A + Type C on same physical cores.
- Allow Type B and C use extra available CPUs.
- Prioritize running Type B over Type C on shared CPUs.

**Configuration:**

```yaml
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
  reservedResources:
    cpu: "2"
  pinCPU: true
  pinMemory: false
  allocatorTopologyBalancing: true

  balloonTypes:
  - name: always-running-batch
    matchExpressions:
    - key: pod/labels/workload-pattern
      operator: In
      values:
      - always-running
    # Mark as generating sustained core load
    loads:
    - sustained-compute

  - name: bursty-requests
    matchExpressions:
    - key: pod/labels/workload-pattern
      operator: In
      values:
      - bursty
    schedulingClass: high-priority
    # Allow bursting within NUMA node scope.
    shareIdleCPUsInSame: numa

  - name: background-tasks
    matchExpressions:
    - key: pod/labels/workload-pattern
      operator: In
      values:
      - background
    # No loads specified - can share cores with anything.
    schedulingClass: low-priority
    # Allow bursting within NUMA node scope.
    shareIdleCPUsInSame: numa

  # Define sustained compute load
  loadClasses:
  - name: sustained-compute
    level: core

  schedulingClasses:
  - name: high-priority
    nice: -10
  - name: low-priority
    policy: batch
    nice: 10
    ioClass: idle
```

**How it works:**

1. **always-running-batch** balloons declare `sustained-compute` load
   with `overloadsLevelInBalloon: true`.
   - CPUs allocated from different physical cores within each balloon.
   - When multiple such balloons exist, they avoid each other's cores.

2. **bursty-requests** and **background-tasks** balloons don't declare
   any loads.
   - Can be allocated to hyperthreads on cores already used by
     always-running-batch.

3. Containers in **bursty-requests** and **background-tasks** balloons
   share idle CPUs in the same NUMA nodes.
   - They get CPU scheduling priority (bursty) or depriority
     (background) to manage competition.

3. Result: Physical cores are shared between complementary workloads,
   maximizing utilization without excessive competition.

**Pod annotation examples:**

```yaml
# Always-running batch job
apiVersion: batch/v1
kind: Job
metadata:
  name: data-processing
spec:
  template:
    metadata:
      labels:
        workload-pattern: always-running
    spec:
      containers:
      - name: processor
        image: batch-processor:latest
        resources:
          requests:
            cpu: "4"

---
# Bursty request handler
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-server
spec:
  template:
    metadata:
      labels:
        workload-pattern: bursty
    spec:
      containers:
      - name: server
        image: api-server:latest
        resources:
          requests:
            cpu: "2"

---
# Background task
apiVersion: batch/v1
kind: CronJob
metadata:
  name: log-aggregator
spec:
  schedule: "*/5 * * * *"
  jobTemplate:
    spec:
      template:
        metadata:
          labels:
            workload-pattern: background
        spec:
          containers:
          - name: aggregator
            image: log-aggregator:latest
            resources:
              requests:
                cpu: "1"
```

**Advanced: Fine-grained control with L2 cache awareness**

For workloads where L2 cache contention is also a concern:

```yaml
  balloonTypes:
  - name: cache-sensitive-batch
    matchExpressions:
    - key: pod/labels/workload-pattern
      operator: In
      values:
      - cache-sensitive
    loads:
    - sustained-compute
    - l2-cache-intensive

  loadClasses:
  - name: sustained-compute
    level: core
    overloadsLevelInBalloon: true
  - name: l2-cache-intensive
    level: l2cache
    overloadsLevelInBalloon: false  # Allow cache sharing within balloon
```

This prevents cache-sensitive workloads from sharing L2 caches across
balloons while still allowing it within a balloon.

---

## Troubleshooting

**Issue**: Containers not being assigned to expected balloons or
Balloons not getting desired CPUs

**Solution**: Check the balloon type matching order and CPU allocation from logs with
```yaml
log:
  debug:
  - policy
```

**Issue**: Out of memory errors with `pinMemory: true`

**Solution**: Either disable memory pinning or ensure sufficient memory on each NUMA node:
```yaml
pinMemory: false
```

**Issue**: Policy not applying configuration changes

**Solution**:
- Verify that `nri-resource-policy-balloons-...` pod is running.
  ```sh
  kubectl get -n kube-system daemonset/nri-resource-policy-balloons
  kubectl get pod -n kube-system nri-resource-policy-balloons-...
  ```
- Verify the configuration object (BalloonsPolicy) exists, is valid, is named
  correctly (`default`, `group.GROUPNAME` or `node.NODENAME`) and is in the
  same namespace as the pod.
  ```sh
  kubectl get balloonspolicies.config.nri -n kube-system
  ```
- Verify node labels. If a node has a `config.nri/group=GROUPNAME`
  label, it will not use BalloonsPolicy named `default`, only
  `group.GROUPNAME` or `node.NODENAME`.
  ```sh
  kubectl get node NODENAME --show-labels
  ```
- If necessary, delete the policy pod, let the DaemonSet re-create it
  and look for "config" issues in the log.
```sh
kubectl delete pod -n kube-system nri-resource-policy-balloons-...
kubectl logs -n kube-system nri-resource-policy-balloons-...
```

**Issue**: System unresponsive or performance drop after
configuration change

**Solution**:
- Delete all workloads, especially those with large memory footprint,
  before changing a configuration. Pinning their memory to different
  nodes may cause large node-to-node memory migrations, causing
  unresponsiveness even up to minutes.
- Redeploy performance critical workloads only after configuration
  updateds. That ensures their processes get started and data aligned
  in memory as configured.
- Make sure important containers request CPUs. Check if BestEffort and
  Burstable containers without CPU requests are still allowed to use
  enough CPUs (see `shareIdleCPUsInSame`) instead of getting all
  packed on a single CPU.
