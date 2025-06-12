# Balloons Policy

## Overview

The balloons policy implements workload placement into "balloons" that
are disjoint CPU pools. Size of a balloon can be fixed, or the balloon
can be dynamically inflated and deflated, that is CPUs added and
removed, based on the CPU resource requests of containers running in
the balloon. Balloons can be static or dynamically created and
destroyed. CPUs in balloons can be configured, for example, by setting
min and max frequencies on CPU cores and uncore. Balloons in
Kubernetes cluster, including CPU and memory affinities of their
containers, can be exposed and observed through noderesourcetopologies
custom resources.

## How It Works

1. User configures balloon types from which the policy creates
   balloons.

2. A balloon has a set of CPUs and a set of containers that run on the
   CPUs.

3. Every container is assigned to exactly one balloon. A container is
   allowed to use all CPUs of its balloon and no other CPUs.

4. Every logical CPU belongs to at most one balloon. There can be CPUs
   that do not belong to any balloon.

5. The number of CPUs in a balloon can change during the lifetime of
   the balloon. If a balloon inflates, that is CPUs are added to it,
   all containers in the balloon are allowed to use more CPUs. If a
   balloon deflates, the opposite is true.

6. When a new container is created on a Kubernetes node, the policy
   first decides the type of the balloon that will run the
   container. The decision is based on annotations of the pod, or the
   namespace if annotations are not given.

7. Next the policy decides which balloon of the decided type will run
   the container. Options are:
  - an existing balloon that already has enough CPUs to run its
    current and new containers
  - an existing balloon that can be inflated to fit its current and
    new containers
  - new balloon.

9. When a CPU is added to a balloon or removed from it, the CPU is
   reconfigured based on balloon's CPU class attributes, or idle CPU
   class attributes.

## Deployment

Deploy nri-resource-policy-balloons on each node as you would for any
other policy. See [deployment](../../deployment/index.md) for more details.

## Configuration

The balloons policy is configured using BalloonsPolicy Custom Resources.
See [setup and usage](../setup.md#setting-up-nri-resource-policy) for
more details on managing the configuration.

### Parameters

Balloons policy parameters:

- `availableResources`:
  - `cpu` specifies cpuset that is managed by the balloons policy. All
    balloons created by the policy can utilize only CPUs in this set.
    Example: `cpu: cpuset:48-95,144-191` allows the policy to manage
    only 48+48 vCPUs on socket 1 in a two-socket 192-CPU system.
- `reservedResources`:
  - `cpu` specifies cpuset or number of CPUs in the special `reserved`
    balloon. By default all containers in the `kube-system` namespace
    are assigned to the reserved balloon. Examples: `cpu: cpuset:0,48`
    uses two logical CPUs: cpu0 and cpu48. `cpu: 2000m` uses any two
    CPUs. If minCPUs are explicitly defined for the `reserved`
    balloon, that number of CPUs will be allocated from the `cpuset`
    and more later (up to `maxCpus`) as needed.
- `pinCPU` controls pinning a container to CPUs of its balloon. The
  default is `true`: the container cannot use other CPUs.
- `pinMemory` controls pinning a container to the memories that are
  closest to the CPUs of its balloon. The default is `true`: allow
  using memory only from the closest NUMA nodes. Can be overridden by
  pinMemory in balloon types. Warning: pinning memory may cause kernel
  to kill containers due to out-of-memory error when allowed NUMA
  nodes do not have enough memory. In this situation consider
  switching this option `false`.
- `preserve` specifies containers whose resource pinning must not be
  modified by the policy.
  - `matchExpressions` if a container matches an expression in this
    list, the policy will preserve container's resource pinning. If
    there is no resource pinning, the policy will not change that
    either. Example: preserve containers named "a" and "b". As a
    result, the policy will not modify CPU or memory pinning of
    matching containers.
    ```
    preserve:
      matchExpressions:
        - key: name
          operator: In
          values:
            - a
            - b
    ```
- `idleCPUClass` specifies the CPU class of those CPUs that do not
  belong to any balloon.
- `reservedPoolNamespaces` is a list of namespaces (wildcards allowed)
  that are assigned to the special reserved balloon, that is, will run
  on reserved CPUs. This always includes the `kube-system` namespace.
- `allocatorTopologyBalancing` affects selecting CPUs for new
  balloons. If `true`, new balloons are created using CPUs on
  NUMA/die/package with most free CPUs, that is, balloons are spread
  across the hardware topology. This helps inflating balloons within
  the same NUMA/die/package and reduces interference between containers
  in balloons when system is not fully loaded. The default is `false`:
  pack new balloons tightly into the same NUMAs/dies/packages. This
  helps keeping large portions of hardware idle and entering into deep
  power saving states.
- `preferSpreadOnPhysicalCores` prefers allocating logical CPUs
  (possibly hyperthreads) for a balloon from separate physical CPU
  cores. This prevents containers in the balloon from interfering with
  themselves as they do not compete on the resources of the same CPU
  cores. On the other hand, it allows more interference between
  containers in different balloons. The default is `false`: balloons
  are packed tightly to a minimum number of physical CPU cores. The
  value set here is the default for all balloon types, but it can be
  overridden with the balloon type specific setting with the same
  name.
- `showContainersInNrt` controls whether containers in balloons are
  exposed as part of NodeResourceTopology. If `true`,
  noderesourcetopologies.topology.node.k8s.io custom resources provide
  visibility to CPU and memory affinity of all containers assigned
  into balloons. The default is `false`. Use balloon-type option with
  the same name to override the policy-level default. Exposing
  affinities of all containers on all nodes may generate a lot of
  traffic and large CR object updates to Kubernetes API server. This
  option has no effect unless `agent:NodeResourceTopology` enables
  node resource topology exposure in general.
- `balloonTypes` is a list of balloon type definitions. The order of
  the types is significant in two cases.

  In the first case the policy pre-creates balloons and allocates
  their CPUs when it starts or is reconfigured, see `minBalloons` and
  `minCPUs` below. Balloon types with the highest `allocatorPriority`
  will get their CPUs in the listed order. Balloon types with a lower
  `allocatorPriority` will get theirs in the same order after them.

  In the second case the policy looks for a balloon type for a new
  container. If annotations do not specify it, the container will be
  be assignd to the first balloon type in the list with matching
  criteria, for instance based on `namespaces` below.

  Each balloon type can be configured with following parameters:
  - `name` of the balloon type. This is used in pod annotations to
    assign containers to balloons of this type.
  - `namespaces` is a list of namespaces (wildcards allowed) whose
    pods should be assigned to this balloon type, unless overridden by
    pod annotations.
  - `groupBy` groups containers into same balloon instances if
    their GroupBy expressions evaluate to the same group.
    Expressions are strings where key references like
    `${pod/labels/mylabel}` will be substituted with corresponding
    values.
  - `matchExpressions` is a list of container match expressions. These
    expressions are evaluated for all containers which have not been
    assigned otherwise to other balloons. If an expression matches,
    IOW it evaluates to true, the container gets assigned to this
    balloon type. Container mach expressions have the same syntax and
    semantics as the scope and match expressions in container affinity
    annotations for the topology-aware policy.
    See the [affinity documentation](./topology-aware.md#affinity-semantics)
    for a detailed description of expressions.
  - `minBalloons` is the minimum number of balloons of this type that
    is always present, even if the balloons would not have any
    containers. The default is 0: if a balloon has no containers, it
    can be destroyed.
  - `maxBalloons` is the maximum number of balloons of this type that
    is allowed to co-exist. The default is 0: creating new balloons is
    not limited by the number of existing balloons.
  - `maxCPUs` specifies the maximum number of CPUs in any balloon of
    this type. Balloons will not be inflated larger than this. 0 means
    unlimited.
  - `minCPUs` specifies the minimum number of CPUs in any balloon of
    this type. When a balloon is created or deflated, it will always
    have at least this many CPUs, even if containers in the balloon
    request less.
  - `cpuClass` specifies the name of the CPU class according to which
    CPUs of balloons are configured. Class properties are defined in
    separate `cpu.classes` objects, see below.
  - `pinMemory` overrides policy-level `pinMemory` in balloons of this
    type.
  - `memoryTypes` is a list of allowed memory types for containers in
    a balloon. Supported types are "HBM", "DRAM" and "PMEM". This
    setting can be overridden by a pod/container specific
    `memory-type` annotation. Memory types have no when not pinning
    memory (see `pinMemory`).
  - `preferCloseToDevices`: prefer creating new balloons close to
    listed devices. List of strings
  - `preferCoreType`:  specifies preferences of the core type which
    could be either power efficient (`efficient`) or high performance
    (`performance`).
  - `preferSpreadingPods`: if `true`, containers of the same pod
    should be spread to different balloons of this type. The default
    is `false`: prefer placing containers of the same pod to the same
    balloon(s).
  - `preferPerNamespaceBalloon`: if `true`, containers in the same
    namespace will be placed in the same balloon(s). On the other
    hand, containers in different namespaces are preferably placed in
    different balloons. The default is `false`: namespace has no
    effect on choosing the balloon of this type.
  - `preferNewBalloons`: if `true`, prefer creating new balloons over
    placing containers to existing balloons. This results in
    preferring exclusive CPUs, as long as there are enough free
    CPUs. The default is `false`: prefer filling and inflating
    existing balloons over creating new ones.
  - `preferIsolCpus`: if `true`, prefer system isolated CPUs (refer to
    kernel command line parameter "isolcpus") for this balloon. Warning:
    if there are not enough isolated CPUs in the system for balloons that
    prefer them, balloons may include normal CPUs, too. This kind of
    mixed-CPU balloons require special attention when implementing
    programs that run on them. Therefore it is recommended to limit the
    number of balloon CPUs (see maxCPUs) and allocate CPUs upfront (see
    minBalloons, minCPUs) when using preferIsolCpus. The default is `false`.
  - `shareIdleCPUsInSame`: Whenever the number of or sizes of balloons
    change, idle CPUs (that do not belong to any balloon) are reshared
    as extra CPUs to containers in balloons with this option. The value
    sets locality of allowed extra CPUs that will be common to these
    containers.
    - `system`: containers are allowed to use idle CPUs available
      anywhere in the system.
    - `package`: ...allowed to use idle CPUs in the same package(s)
    (sockets) as the balloon.
    - `die`: ...in the same die(s) as the balloon.
    - `numa`: ...in the same numa node(s) as the balloon.
    - `l2cache`: ...allowed to use idle CPUs that share the same level
      2 cache as the balloon.
    - `core`: ...allowed to use idle CPU threads in the same cores with
      the balloon.
  - `hideHyperthreads`: "soft" disable hyperthreads. If `true`, only
    one hyperthread from every physical CPU core in the balloon is
    allowed to be used by containers in the balloon. Hidden
    hyperthreads are not available to any container in the system
    either. If containers in the balloon are allowed to share idle
    CPUs (see `shareIdleCPUsInSame`), hyperthreads of idle CPUs, too,
    are hidden from the containers. If containers in another balloon
    share the same idle CPUs, those containers are allowed to use both
    hyperthreads of the idle CPUs if `hideHyperthreads` is `false` for
    the other balloon. The default is `false`: containers are allowed
    to use all hyperthreads of balloon's CPUs and shared idle CPUs.
  - `preferSpreadOnPhysicalCores` overrides the policy level option
    with the same name in the scope of this balloon type.
  - `preferCloseToDevices` prefers creating new balloons close to
    listed devices. If all preferences cannot be fulfilled, preference
    to first devices in the list override preferences to devices after
    them. Adding this preference to any balloon type automatically
    adds corresponding anti-affinity to other balloon types that do
    not prefer to be close to the same device: they prefer being
    created away from the device. Example:
    ```
    preferCloseToDevices:
      - /sys/class/net/eth0
      - /sys/class/block/sda
    ```
  - `allocatorPriority` (0: High, 1: Normal, 2: Low, 3: None). CPU
    allocator parameter, used when creating new or resizing existing
    balloons. If there are balloon types with pre-created balloons
    (`minBalloons` > 0), balloons of the type with the highest
    `allocatorPriority` are created first.
  - `loads`, a list of load class names, describes system load
    generated by containers in a balloon that may affect other
    containers in other balloons. The policy prefers selecting CPUs
    for balloons so that no parts of system are overloaded. For
    instance, two balloons that generate heavy load on level 2 cache
    should get their CPUs from separate cache blocks for best
    performance. Every listed class must be specified in
    `loadClasses`.
  - `components` is a list of components of a balloon. If a balloon
    consists of components, its CPUs are allocated by allocating CPUs
    for each component balloon separately, and then adding them up.
    See [combining balloons](#combining-balloons) for more details and
    an example. Properties of components in the list are:
    - `balloonType` specifies the name of the balloon type according
      to which CPUs are allocated to this component.
- `loadClasses`: lists properties of loads that containers in balloons
  generate to some parts of the system. When the policy allocates CPUs
  for load generating balloon instances, it selects CPUs so that it
  avoids overloading any of these parts. Properties of load classes
  are:
  - `name` is the name of the load class. Balloon types that cause
    this type of load include the class name in their `loads` list.
    See [load example](#selecting-one-hyperthread-per-core-for-heavy-compute).
  - `level` specifies the CPU topology level affected by this
    load. Supported level values and their consequences are:
    - `core`: if one CPU hyperthread from a physical CPU core belongs
      to a balloon that loads `core`, then the policy avoids selecting
      the other CPU hyperthread to any other balloon that loads
      `core`.
    - `l2cache`: if one CPU from CPUs that share the same L2 cache is
      selected to a balloon that loads `l2cache`, then the policy
      avoids selecting other CPUs from the same L2 cache block to any
      other balloon that loads `l2cache`.
  - `overloadsLevelInBalloon`: if `true`, avoiding the load on the
    same `level` is taken into account, not only when selecting CPUs
    to other balloons, but also when selecting CPUs in the balloon
    that causes the load at hand. This enables, for instance,
    allocating CPUs so that every CPU is chosen from different
    physical `core` or different `l2cache` block. The default is
    `false`, that is, locality of balloon's CPUs is seen more
    important than avoiding balloon's own load.
- `control.cpu.classes`: defines CPU classes and their
    properties. Class names are keys followed by properties:
    - `minFreq` minimum frequency for CPUs in this class (kHz).
    - `maxFreq` maximum frequency for CPUs in this class (kHz).
    - `uncoreMinFreq` minimum uncore frequency for CPUs in this
      class (kHz).  If there are differences in `uncoreMinFreq`s in
      CPUs within the same uncore frequency zone, the maximum value
      of all `uncoreMinFreq`s is used.
    - `uncoreMaxFreq` maximum uncore frequency for CPUs in this
      class (kHz).
- `instrumentation`: configures interface for runtime instrumentation.
  - `httpEndpoint`: the address the HTTP server listens on. Example:
    `:8891`.
  - `prometheusExport`: if set to True, balloons with their CPUs
     and assigned containers are readable through `/metrics` from the
     httpEndpoint.
  - `reportPeriod`: `/metrics` aggregation interval for polled metrics.
  - `metrics`: defines which metrics to collect.
    - `enabled`: a list of glob patterns that match metrics to collect.
      Example: `["policy"]`
  - `samplingRatePerMillion`: the number of samples to collect per million spans.
    Example: `100000`
  - `tracingCollector`: defines the external endpoint for tracing data collection.
    Example: `otlp-http://localhost:4318`.
- `agent`: controls communicating with the Kubernetes node agent and
  the API server.
  - `nodeResourceTopology`: if `true`, expose balloons as node
    resource topology zones in noderesourcetopologies custom
    resources. Moreover, showing containers assigned to balloons and
    their CPU/memory affinities can be enabled with
    `showContainersInNrt`. The default is `false`.
- `log`: contains the logging configuration for the policy.
  - `debug`: an array of components to enable debug logging for.
    Example: `["policy"]`.
  - `source`: set to `true` to prefix messages with the name of the logger
    source.

### Example

Example configuration that runs all pods in balloons of 1-4
CPUs. Instrumentation enables reading CPUs and containers in balloons
from `http://$localhost_or_pod_IP:8891/metrics`.

```yaml
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
  # Expose balloons as node resource topology zones in
  # noderesourcestopologies custom resources.
  agent:
    nodeResourceTopology: true

  reservedResources:
    cpu: 1000m
  pinCPU: true
  pinMemory: true
  allocatorTopologyBalancing: true
  idleCPUClass: lowpower
  balloonTypes:
    - name: "quad"
      maxCPUs: 4
      cpuClass: dynamic
      namespaces:
        - "*"
      showContainersInNrt: true
  control:
    cpu:
      classes:
        lowpower:
          minFreq: 800000
          maxFreq: 800000
        dynamic:
          minFreq: 800000
          maxFreq: 3600000
        turbo:
          minFreq: 3000000
          maxFreq: 3600000
          uncoreMinFreq: 2000000
          uncoreMaxFreq: 2400000
  instrumentation:
    httpEndpoint: :8891
    prometheusExport: true
```

## Assigning a Container to a Balloon

The balloon type of a container can be defined in pod annotations. In
the example below, the first annotation sets the balloon type (`BT`)
of a single container (`CONTAINER_NAME`). The last two annotations set
the balloon type for all containers in the pod. This will be used
unless overridden with the container-specific balloon type.

```yaml
balloon.balloons.resource-policy.nri.io/container.CONTAINER_NAME: BT
balloon.balloons.resource-policy.nri.io/pod: BT
balloon.balloons.resource-policy.nri.io: BT
```

If the pod does not have these annotations, the container is matched
to `matchExpressions` and `namespaces` of each type in the
`balloonType`s list. The first matching balloon type is used.

If the container does not match any of the balloon types, it is
assigned to the `default` balloon type. Parameters for this balloon
type can be defined explicitly among other balloon types. If they are
not defined, a built-in `default` balloon type is used.

## Pod and Container Overrides to CPU and Memory Pinning

### Disabling CPU or Memory Pinning of a Container

Some containers may need to run on all CPUs or access all memories
without restrictions. There are two alternatives to achieve this:
policy configuration and pod annotations.

The resource policy will not touch allowed resources of containers
that match `preserve` criteria. See policy configuration options
above.

Alternatively, pod annotations can opt-out all or selected containers
in the pod from CPU or memory pinning by preserving whatever existing
or non-existing pinning configuration:

```yaml
cpu.preserve.resource-policy.nri.io/container.CONTAINER_NAME: "true"
cpu.preserve.resource-policy.nri.io/pod: "true"
cpu.preserve.resource-policy.nri.io: "true"

memory.preserve.resource-policy.nri.io/container.CONTAINER_NAME: "true"
memory.preserve.resource-policy.nri.io/pod: "true"
memory.preserve.resource-policy.nri.io: "true"
```

### Selectively Disabling Hyperthreading

If a container opts to hide hyperthreads, it is allowed to use only
one hyperthread from every physical CPU core allocated to it. Note
that as a result the container may be allowed to run on only half of
the CPUs it has requested. In case of workloads that do not benefit
from hyperthreading this nevertheless results in better performance
compared to running on all hyperthreads of the same CPU cores. If
container's CPU allocation is exclusive, no other container can run on
hidden hyperthreads either.

```yaml
metadata:
  annotations:
    # allow the "LLM" container to use only single thread per physical CPU core
    hide-hyperthreads.resource-policy.nri.io/container.LLM: "true"
```

The `hide-hyperthreads` pod annotation overrides the
`hideHyperthreads` balloon type parameter value for selected
containers in the pod.

### Selecting One Hyperthread per Core for Heavy Compute

An alternative to completely hiding one hyperthread on each heavily
loaded physical CPU core (see previous Section), is marking the CPU
cores loaded, and prefer selecting CPUs from unloaded cores for
compute-intensive workloads. Unlike in hiding, unused hyperhtreads on
loaded cores are still available for containers that do not load them
that heavily.

Example:

```yaml
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
  allocatorTopologyBalancing: false
  reservedResources:
    cpu: 1000m
  pinCPU: true
  pinMemory: false
  balloonTypes:
  - name: ai-inference
    minCPUs: 16
    minBalloons: 2
    preferNewBalloons: true
    loads:
    - avx
  - name: video-encoding
    maxCPUs: 8
    loads:
    - avx
  - name: default
    maxCPUs: 16
    namespaces:
    - "*"
  loadClasses:
  - name: avx
    level: core
    overloadsLevelInBalloon: true
```

Containers in both "ai-inference" and "video-encoding" balloons are
expected to cause heavy load on physical CPU core resources due to
high throughput of AVX optimized code. Therefore all CPUs to these
balloons are picked from different physical cores, as long as they are
available. Because `AllocatorTopologyBalancing` is set to `false`, the
policy will select CPUs in "pack tightly" rather than "spread evenly"
manner. This leads into preferring the left-over threads over threads
from unused CPU cores when allocating CPUs for default balloons.

### Memory Type

If a container must be pinned to specific memory types that may differ
from its balloon's `memoryTypes`, container-specific types can be
given in the `memory-type` pod annotations:

```yaml
memory-type.resource-policy.nri.io/container.CONTAINER_NAME: <COMMA-SEPARATED-TYPES>
memory-type.resource-policy.nri.io/pod: <COMMA-SEPARATED-TYPES>
memory-type.resource-policy.nri.io: <COMMA-SEPARATED-TYPES>
```

The first sets the memory type for a single container in the pod, the
latter two for other containers in the pod. Supported types are "HBM",
"DRAM" and "PMEM". Example:

```yaml
metadata:
  annotations:
    memory-type.resource-policy.nri.io/container.LLM: HBM,DRAM
```


## Combining Balloons

Sometimes a container needs a set of CPUs where some CPUs have
different properties or they are selected based on different criteria
than other CPUs.

This kind of a container needs to be assigned into a composite
balloon. A composite balloon consists of component balloons.
Specifying `components` in a balloon type makes it a composite balloon
type. Composite balloons get their CPUs by combining CPUs of their
components.

Each component specifies its balloon type, that must be defined in the
`balloonTypes` list. CPUs are allocated to the component based on its
own balloon type configuration only. As CPUs are not allocated
directly to composite balloons, CPU allocation parameters are not
allowed in composite balloon types.

When the policy creates new composite balloon, it creates hidden
instances of balloons's components, too. Resizing the composite
balloon due to changes in its containers causes resizing these hidden
instances.

Example: allocate CPUs for distributed AI inference containers so
that, depending on a balloon type, a container will get:
- equal number of CPUs from all 4 NUMA nodes in the system
- equal number of CPUs from both NUMA nodes on CPU package 0
- equal number of CPUs from both NUMA nodes on CPU package 1.

Following balloon type configuration implements this. Containers can
be assigned into balloons `balance-all-nodes`, `balance-pkg0-nodes`,
and `balance-pkg1-nodes`, respectively.

```yaml
  balloonTypes:
  - name: balance-all-nodes
    components:
    - balloonType: balance-pkg0-nodes
    - balloonType: balance-pkg1-nodes

  - name: balance-pkg0-nodes
    components:
    - balloonType: node0
    - balloonType: node1

  - name: balance-pkg1-nodes
    components:
    - balloonType: node2
    - balloonType: node3

  - name: node0
    preferCloseToDevices:
    - /sys/devices/system/node/node0

  - name: node1
    preferCloseToDevices:
    - /sys/devices/system/node/node1

  - name: node2
    preferCloseToDevices:
    - /sys/devices/system/node/node2

  - name: node3
    preferCloseToDevices:
    - /sys/devices/system/node/node3
```

## Metrics and Debugging

In order to enable more verbose logging and metrics exporting from the
balloons policy, enable instrumentation and policy debugging from the
nri-resource-policy global config:

```yaml
instrumentation:
  # The balloons policy exports containers running in each balloon,
  # and cpusets of balloons. Accessible in command line:
  # curl --silent http://$localhost_or_pod_IP:8891/metrics
  HTTPEndpoint: :8891
  PrometheusExport: true
  metrics:
    enabled: # use '*' instead for all available metrics
    - policy
logger:
  Debug: policy
```
