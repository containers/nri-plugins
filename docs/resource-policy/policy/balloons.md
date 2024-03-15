# Balloons Policy

## Overview

The balloons policy implements workload placement into "balloons" that
are disjoint CPU pools. Size of a balloon can be fixed, or the balloon
can be dynamically inflated and deflated, that is CPUs added and
removed, based on the CPU resource requests of containers running in
the balloon. Balloons can be static or dynamically created and
destroyed. CPUs in balloons can be configured, for example, by setting
min and max frequencies on CPU cores and uncore.

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
  using memory only from the closest NUMA nodes. Warning: this may
  cause kernel to kill containers due to out-of-memory error when
  closest NUMA nodes do not have enough memory. In this situation
  consider switching this option `false`.
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
  - `preferCloseToDevices`: prefer creating new balloons close to
    listed devices. List of strings
  - `preferSpreadingPods`: if `true`, containers of the same pod
    should be spread to different balloons of this type. The default
    is `false`: prefer placing containers of the same pod to the same
    balloon(s).
  - `preferPerNamespaceBalloon`: if `true`, containers in the same
    namespace will be placed in the same balloon(s). On the other
    hand, containers in different namespaces are preferrably placed in
    different balloons. The default is `false`: namespace has no
    effect on choosing the balloon of this type.
  - `preferNewBalloons`: if `true`, prefer creating new balloons over
    placing containers to existing balloons. This results in
    preferring exclusive CPUs, as long as there are enough free
    CPUs. The default is `false`: prefer filling and inflating
    existing balloons over creating new ones.
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
    - `core`: ...allowed to use idle CPU threads in the same cores with
      the balloon.
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
  - `reportPeriod`: `/metrics` aggregation interval.

### Example

Example configuration that runs all pods in balloons of 1-4
CPUs. Instrumentation enables reading CPUs and containers in balloons
from `http://localhost:8891/metrics`.

```yaml
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
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

## Disabling CPU or Memory Pinning of a Container

Some containers may need to run on all CPUs or access all memories
without restrictions. Annotate these pods and containers to prevent
the resource policy from touching their CPU or memory pinning.

```yaml
cpu.preserve.resource-policy.nri.io/container.CONTAINER_NAME: "true"
cpu.preserve.resource-policy.nri.io/pod: "true"
cpu.preserve.resource-policy.nri.io: "true"

memory.preserve.resource-policy.nri.io/container.CONTAINER_NAME: "true"
memory.preserve.resource-policy.nri.io/pod: "true"
memory.preserve.resource-policy.nri.io: "true"
```

## Metrics and Debugging

In order to enable more verbose logging and metrics exporting from the
balloons policy, enable instrumentation and policy debugging from the
nri-resource-policy global config:

```yaml
instrumentation:
  # The balloons policy exports containers running in each balloon,
  # and cpusets of balloons. Accessible in command line:
  # curl --silent http://localhost:8891/metrics
  HTTPEndpoint: :8891
  PrometheusExport: true
logger:
  Debug: policy
```
