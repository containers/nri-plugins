# Topology-Aware Policy

## Background

On server-grade hardware the CPU cores, I/O devices and other peripherals
form a rather complex network together with the memory controllers, the
I/O bus hierarchy and the CPU interconnect. When a combination of these
resources are allocated to a single workload, the performance of that
workload can vary greatly, depending on how efficiently data is transferred
between them or, in other words, on how well the resources are aligned.

There are a number of inherent architectural hardware properties that,
unless properly taken into account, can cause resource misalignment and
workload performance degradation. There are a multitude of CPU cores
available to run workloads. There are a multitude of memory controllers
these workloads can use to store and retrieve data from main memory. There
are a multitude of I/O devices attached to a number of I/O buses the same
workloads can access. The CPU cores can be divided into a number of groups,
with each group having different access latency and bandwidth to each
memory controller and I/O device.

If a workload is not assigned to run with a properly aligned set of CPU,
memory and devices, it will not be able to achieve optimal performance.
Given the idiosyncrasies of hardware, allocating a properly aligned set
of resources for optimal workload performance requires identifying and
understanding the multiple dimensions of access latency locality present
in hardware or, in other words, hardware topology awareness.

## Overview

The `topology-aware` policy automatically builds a tree of pools based on the
detected hardware topology. Each pool has a set of CPUs and memory zones
assigned as their resources. Resource allocation for workloads happens by
first picking the pool which is considered to fit the best the resource
requirements of the workload and then assigning CPU and memory from this pool.

The pool nodes at various depths from bottom to top represent the NUMA nodes,
dies, sockets, and finally the whole of the system at the root node. Leaf NUMA
nodes are assigned the memory behind their controllers / zones and CPU cores
with the smallest distance / access penalty to this memory. If the machine
has multiple types of memory separately visible to both the kernel and user
space, for instance both DRAM and
[PMEM](https://www.intel.com/content/www/us/en/products/memory-storage/optane-dc-persistent-memory.html),
each zone of special type of memory is assigned to the closest NUMA node pool.

Each non-leaf pool node in the tree is assigned the union of the resources of
its children. So in practice, dies nodes end up containing all the CPU cores
and the memory zones in the corresponding die, sockets nodes end up containing
the CPU cores and memory zones in the corresponding socket's dies, and the root
ends up containing all CPU cores and memory zones in all sockets.

With this setup, each pool in the tree has a topologically aligned set of CPU
and memory resources. The amount of available resources gradually increases in
the tree from bottom to top, while the strictness of alignment is gradually
relaxed. In other words, as one moves from bottom to top in the tree, it is
getting gradually easier to fit in a workload, but the price paid for this is
a gradually increasing maximum potential cost or penalty for memory access and
data transfer between CPU cores.

Another property of this setup is that the resource sets of sibling pools at
the same depth in the tree are disjoint while the resource sets of descendant
pools along the same path in the tree partially overlap, with the intersection
decreasing as the the distance between pools increases. This makes it easy to
isolate workloads from each other. As long as workloads are assigned to pools
which has no other common ancestor than the root, the resources of these
workloads should be as well isolated from each other as possible on the given
hardware.

With such an arrangement, this policy should handle topology-aware alignment
of resources without any special or extra configuration. When allocating
resources, the policy

- filters out all pools with insufficient free capacity
- runs a scoring algorithm for the remaining ones
- picks the one with the best score
- assigns resources to the workload from there

Although the details of the scoring algorithm are subject to change as the
implementation evolves, its basic principles are roughly

- prefer pools lower in the tree, IOW stricter alignment and lower latency
- prefer idle pools over busy ones, IOW more remaining free capacity and
  fewer workloads
- prefer pools with better overall device alignment

## Features

The `topology-aware` policy has the following features:

- topologically aligned allocation of CPU and memory
  - assign CPU and memory to workloads with tightest available alignment
- aligned allocation of devices
  - pick pool for workload based on locality of devices already assigned
- shared allocation of CPU cores
  - assign workload to shared subset of pool CPUs
- exclusive allocation of CPU cores
  - dynamically slice off CPU cores from shared subset and assign to workload
- mixed allocation of CPU cores
  - assign both exclusive and shared CPU cores to workload
- discovering and using kernel-isolated CPU cores (['isolcpus'](https://www.kernel.org/doc/html/latest/admin-guide/kernel-parameters.html#cpu-lists))
  - use kernel-isolated CPU cores for exclusively assigned CPU cores
- exposing assigned resources to workloads
- notifying workloads about changes in resource assignment
- dynamic relaxation of memory alignment to prevent OOM
  - dynamically widen workload memory set to avoid pool/workload OOM
- multi-tier memory allocation
  - assign workloads to memory zones of their preferred type
  - the policy knows about three kinds of memory:
    - DRAM is regular system main memory
    - PMEM is large-capacity memory, such as
      [Intel® Optane™ memory](https://www.intel.com/content/www/us/en/products/memory-storage/optane-dc-persistent-memory.html)
    - [HBM](https://en.wikipedia.org/wiki/High_Bandwidth_Memory) is high
      speed memory, typically found on some special-purpose computing systems
- cold start
  - pin workload exclusively to PMEM for an initial warm-up period

## Activating the Policy

You can activate the `topology-aware` policy by using the following configuration
fragment in the configuration for `nri-resource-policy-topology-aware`:

```yaml
policy:
  ReservedResources:
    CPU: 750m
```

## Configuring the Policy

The policy has a number of configuration options which affect its default
behavior. These options can be supplied as part of the
[dynamic configuration](../setup.md#dynamic-configuration-with-configmaps)
or in a fallback or forced configuration file. These configuration options
are

- `PinCPU`
  - whether to pin workloads to assigned pool CPU sets
- `PinMemory`
  - whether to pin workloads to assigned pool memory zones
- `PreferIsolatedCPUs`
  - whether isolated CPUs are preferred by default for workloads that are
    eligible for exclusive CPU allocation
- `PreferSharedCPUs`
  - whether shared allocation is preferred by default for workloads that
    would be otherwise eligible for exclusive CPU allocation
- `ReservedPoolNamespaces`
  - list of extra namespaces (or glob patters) that will be allocated to
    reserved CPUs
- `ColocatePods`
  - whether try to allocate containers in a pod to the same or close by
    topology pools
- `ColocateNamespaces`
  - whether try to allocate containers in a namespace to the same or close by
    topology pools

## Policy CPU Allocation Preferences

There are a number of workload properties this policy actively checks to decide
if the workload could potentially benefit from extra resource allocation
optimizations. Unless configured differently, containers fulfilling certain
corresponding criteria are considered eligible for these optimizations. This
will be reflected in the assigned resources whenever that is possible at the
time the container's creation / resource allocation request hits the policy.

The set of these extra optimizations consist of

- assignment of `kube-reserved` CPUs
- assignment of exclusively allocated CPU cores
- usage of kernel-isolated CPU cores (for exclusive allocation)

The policy uses a combination of the QoS class and the resource requirements of
the container to decide if any of these extra allocation preferences should be
applied. Containers are divided into five groups, with each group having a
slightly different set of criteria for eligibility.

- `kube-system` group
  - all containers in the `kube-system` namespace
- `low-priority` group
  - containers in the `BestEffort` or `Burstable` QoS class
- `sub-core` group
  - Guaranteed QoS class containers with `CPU request < 1 CPU`
- `mixed` group
  - Guaranteed QoS class containers with `1 <= CPU request < 2`
- `multi-core` group
  - Guaranteed QoS class containers with `CPU request >= 2`

The eligibility rules for extra optimization are slightly different among these
groups.

- `kube-system`
  - not eligible for extra optimizations
  - eligible to run on `kube-reserved` CPU cores
  - always run on shared CPU cores
- `low-priority`
  - not eligible for extra optimizations
  - always run on shared CPU cores
- `sub-core`
  - not eligible for extra optimizations
  - always run on shared CPU cores
- `mixed`
  - by default eligible for exclusive and isolated allocation
  - not eligible for either if `PreferSharedCPUs` is set to true
  - not eligible for either if annotated to opt out from exclusive allocation
  - not eligible for isolated allocation if annotated to opt out
- `multi-core`
  - CPU request fractional (`(CPU request % 1000 milli-CPU) != 0`):
    - by default not eligible for extra optimizations
    - eligible for exclusive and isolated allocation if annotated to opt in
  - CPU request not fractional:
    - by default eligible for exclusive allocation
    - by default not eligible for isolated allocation
    - not eligible for exclusive allocation if annotated to opt out
    - eligible for isolated allocation if annotated to opt in

Eligibility for kube-reserved CPU core allocation should always be possible to
honor. If this is not the case, it is probably due to an incorrect configuration
which underdeclares `ReservedResources`. In that case, ordinary shared CPU cores
will be used instead of kube-reserved ones.

Eligibility for exclusive CPU allocation should always be possible to honor.
Eligibility for isolated core allocation is only honored if there are enough
isolated cores available to fulfill the exclusive part of the container's CPU
request with isolated cores alone. Otherwise ordinary CPUs will be allocated,
by slicing them off for exclusive usage from the shared subset of CPU cores in
the container's assigned pool.

Containers in the kube-system group are pinned to share all kube-reserved CPU
cores. Containers in the low-priority or sub-core groups, and containers which
are only eligible for shared CPU core allocation in the mixed and multi-core
groups, are all pinned to run on the shared subset of CPU cores in the
container's assigned pool. This shared subset can and usually does change
dynamically as exclusive CPU cores are allocated and released in the pool.

## Container CPU Allocation Preference Annotations

Containers can be annotated to diverge from the default CPU allocation
preferences the policy would otherwise apply to them. These Pod annotations
can be given both with per pod and per container resolution. If for any
container both of these exist, the container-specific one takes precedence.

### Shared, Exclusive, and Isolated CPU Preference

A container can opt in to or opt out from shared CPU allocation using the
following Pod annotation.

```yaml
metadata:
  annotations:
    # opt in container C1 to shared CPU core allocation
    prefer-shared-cpus.resource-policy.nri.io/container.C1: "true"
    # opt in the whole pod to shared CPU core allocation
    prefer-shared-cpus.resource-policy.nri.io/pod: "true"
    # selectively opt out container C2 from shared CPU core allocation
    prefer-shared-cpus.resource-policy.nri.io/container.C2: "false"
```

Opting in to exclusive allocation happens by opting out from shared allocation,
and opting out from exclusive allocation happens by opting in to shared
allocation.

A container can opt in to or opt out from isolated exclusive CPU core
allocation using the following Pod annotation.

```yaml
metadata:
  annotations:
    # opt in container C1 to isolated exclusive CPU core allocation
    prefer-isolated-cpus.resource-policy.nri.io/container.C1: "true"
    # opt in the whole pod to isolated exclusive CPU core allocation
    prefer-isolated-cpus.resource-policy.nri.io/pod: "true"
    # selectively opt out container C2 from isolated exclusive CPU core allocation
    prefer-isolated-cpus.resource-policy.nri.io/container.C2: "false"
```

These Pod annotations have no effect on containers which are not eligible for
exclusive allocation.

### Implicit Hardware Topology Hints

`NRI Resource Policy` automatically generates HW `Topology Hints` for devices
assigned to a container, prior to handing the container off to the active policy
for resource allocation. The `topology-aware` policy is hint-aware and normally
takes topology hints into account when picking the best pool to allocate resources.
Hints indicate optimal `HW locality` for device access and they can alter
significantly which pool gets picked for a container.

Since device topology hints are implicitly generated, there are cases where one
would like the policy to disregard them altogether. For instance, when a local
volume is used by a container but not in any performance critical manner.

Containers can be annotated to opt out from and selectively opt in to hint-aware
pool selection using the following Pod annotations.

```yaml
metadata:
  annotations:
    # only disregard hints for container C1
    topologyhints.resource-policy.nri.io/container.C1: "false"
    # disregard hints for all containers by default
    topologyhints.resource-policy.nri.io/pod: "false"
    # but take hints into account for container C2
    topologyhints.resource-policy.nri.io/container.C2: "true"
```

Topology hint generation is globally enabled by default. Therefore, using the
Pod annotation as opt in only has an effect when the whole pod is annotated to
opt out from hint-aware pool selection.

### Implicit Topological Co-location for Pods and Namespaces

The `ColocatePods` or `ColocateNamespaces` configuration options control whether
the policy will try to co-locate, that is allocate topologically close, containers
within the same Pod or K8s namespace.

Both of these options are false by default. Setting them to true is a shorthand
for adding to each container an affinity of weight 10 for all other containers
in the same pod or namespace.

Containers with user-defined affinities are never extended with either of these
co-location affinities. However, such containers can still have affinity effects
on other containers that do get extended with co-location. Therefore mixing user-
defined affinities with implicit co-location requires both careful consideration
and a thorough understanding of affinity evaluation, or it should be avoided
altogether.

## Cold Start

The `topology-aware` policy supports "cold start" functionality. When cold start
is enabled and the workload is allocated to a topology node with both DRAM and
PMEM memory, the initial memory controller is only the PMEM controller. DRAM
controller is added to the workload only after the cold start timeout is
done. The effect of this is that allocated large unused memory areas of
memory don't need to be migrated to PMEM, because it was allocated there to
begin with. Cold start is configured like this in the pod metadata:

```yaml
metadata:
  annotations:
    memory-type.resource-policy.nri.io/container.container1: dram,pmem
    cold-start.resource-policy.nri.io/container.container1: |
      duration: 60s
```

Again, alternatively you can use the following deprecated Pod annotation syntax
to achieve the same, but support for this syntax is subject to be dropped in a
future release:

```yaml
metadata:
  annotations:
    resource-policy.nri.io/memory-type: |
      container1: dram,pmem
    resource-policy.nri.io/cold-start: |
      container1:
        duration: 60s
```

In the above example, `container1` would be initially granted only PMEM
memory controller, but after 60 seconds the DRAM controller would be
added to the container memset.

## Container memory requests and limits

Due to inaccuracies in how `nri-resource-policy` calculates memory requests for
pods in QoS class `Burstable`, you should either use `Limit` for setting
the amount of memory for containers in `Burstable` pods to provide `cri-resmgr`
with an exact copy of the resource requirements from the Pod Spec as an extra
Pod annotation.

## Reserved pool namespaces

User is able to mark certain namespaces to have a reserved CPU allocation.
Containers belonging to such namespaces will only run on CPUs set aside
according to the global CPU reservation, as configured by the ReservedResources
configuration option in the policy section.
The `ReservedPoolNamespaces` option is a list of namespace globs that will be
allocated to reserved CPU class.

For example:

```yaml
policy:
  topology-aware:
    ReservedPoolNamespaces: ["my-pool","reserved-*"]
```

In this setup, all the workloads in `my-pool` namespace and those namespaces
starting with `reserved-` string are allocated to reserved CPU class.
The workloads in `kube-system` are automatically assigned to reserved CPU
class so no need to mention `kube-system` in this list.

## Reserved CPU annotations

User is able to mark certain pods and containers to have a reserved CPU
allocation by using annotations. Containers having a such annotation will only
run on CPUs set aside according to the global CPU reservation, as configured by
the ReservedResources configuration option in the policy section.

For example:

```yaml
metadata:
  annotations:
    prefer-reserved-cpus.resource-policy.nri.io/pod: "true"
    prefer-reserved-cpus.resource-policy.nri.io/container.special: "false"
```

## Allowing or denying mount/device paths via annotations

User is able mark certain pods and containers to have allowed or denied
paths for mounts or devices. What this means is that when the system
is generating topology hints, it will consult this allowed / denied path
list to determine what hints are created. The deny path list is checked
first and then the allowed path list. This means that you can deny all
the mount/device paths and then allow only the needed ones for example.
User can either set the path with "prefix" (this is the default) or with
"glob" type. The "prefix" type means that the prefix of the mount/device
path is checked for matches. The "glob" type means that user is able to
put wildcards to the matched paths.

For example:

```yaml
metadata:
  annotations:
    allow.topologyhints.resource-policy.nri.io/pod: |+
      type: prefix
      paths:
        - /foo/bar/whitelisted-path1
        - /xy-zy/another-whitelisted-path1
    allow.topologyhints.resource-policy.nri.io/container.pod0c0: |+
      type: glob
      paths:
        - /whitelisted-path*2
        - /xy-zy/another-whitelisted-path2
    deny.topologyhints.resource-policy.nri.io: |+
      type: prefix
      paths:
        - /foo/bar/blacklisted-path3
    deny.topologyhints.resource-policy.nri.io/pod: |+
      type: glob
      paths:
        - /blacklisted-path*4
    deny.topologyhints.resource-policy.nri.io/container.pod0c1: |+
      type: prefix
      paths:
        - /foo/bar/blacklisted-path5
        - /xy-zy/another-blacklisted-path5
```

## Container Affinity and Anti-Affinity

### Introduction

The topology-aware resource policy allow the user to give hints about how
particular containers should be *co-located* within a node. In particular these
hints express whether containers should be located *'close'* to each other or
*'far away'* from each other, in a hardware topology sense.

Since these hints are interpreted always by a particular *policy implementation*,
the exact definitions of 'close' and 'far' are also somewhat *policy-specific*.
However as a general rule of thumb containers running

- on CPUs within the *same NUMA nodes* are considered *'close'* to each other,
- on CPUs within *different NUMA nodes* in the *same socket* are *'farther'*, and
- on CPUs within *different sockets* are *'far'* from each other

These hints are expressed by `container affinity annotations` on the Pod.
There are two types of affinities:

- `affinity` (or `positive affinty`): cause affected containers to *pull* each
  other closer
- `anti-affinity` (or `negative affinity`): cause affected containers to *push*
  each other further away

Policies try to place a container

- close to those the container has affinity towards
- far from those the container has anti-affinity towards.

### Affinity Annotation Syntax

*Affinities* are defined as the `resource-policy.nri.io/affinity` annotation.
*Anti-affinities* are defined as the `resource-policy.nri.io/anti-affinity`
annotation. They are specified in the `metadata` section of the `Pod YAML`, under
`annotations` as a dictionary, with each dictionary key being the name of the
*container* within the Pod to which the annotation belongs to.

```yaml
metadata:
  anotations:
    resource-policy.nri.io/affinity: |
      container1:
        - scope:
            key: key-ref
            operator: op
            values:
            - value1
            ...
            - valueN
          match:
            key: key-ref
            operator: op
            values:
            - value1
            ...
            - valueN
          weight: w
```

An anti-affinity is defined similarly but using `resource-policy.nri.io/anti-affinity`
as the annotation key.

```yaml
metadata:
  anotations:
    resource-policy.nri.io/anti-affinity: |
      container1:
        - scope:
            key: key-ref
            operator: op
            values:
            - value1
            ...
            - valueN
          match:
            key: key-ref
            operator: op
            values:
            - value1
            ...
            - valueN
          weight: w
```

### Affinity Semantics

An affinity consists of three parts:

- `scope expression`: defines which containers this affinity is evaluated against
- `match expression`: defines for which containers (within the scope) the
  affinity applies to
- `weight`: defines how *strong* a pull or a push the affinity causes

*Affinities* are also sometimes referred to as *positive affinities* while
*anti-affinities* are referred to as *negative affinities*. The reason for this is
that the only difference between these are that affinities have a *positive weight*
while anti-affinities have a *negative weight*.

The *scope* of an affinity defines the *bounding set of containers* the
affinity can apply to. The affinity *expression* is evaluated against the
containers *in scope* and it *selects the containers* the affinity really has
an effect on. The *weight* specifies whether the effect is a *pull* or a
*push*. *Positive* weights cause a *pull* while *negative* weights cause a
*push*. Additionally, the *weight* specifies *how strong* the push or the pull
is. This is useful in situations where the policy needs to make some
compromises because an optimal placement is not possible. The weight then also
acts as a way to specify preferences of priorities between the various
compromises: the heavier the weight the stronger the pull or push and the
larger the propbability that it will be honored, if this is possible at all.

The scope can be omitted from an affinity in which case it implies *Pod scope*,
in other words the scope of all containers that belong to the same Pod as the
container for which which the affinity is defined.

The weight can also be omitted in which case it defaults to -1 for
anti-affinities and +1 for affinities. Weights are currently limited to the
range [-1000,1000].

Both the affinity scope and the expression select containers, therefore they
are identical. Both of them are *expressions*. An expression consists of three
parts:

- key: specifies what *metadata* to pick from a container for evaluation
- operation (op): specifies what *logical operation* the expression evaluates
- values: a set of *strings* to evaluate the the value of the key against

The supported keys are:

- for pods:
  - `name`
  - `namespace`
  - `qosclass`
  - `labels/<label-key>`
  - `id`
  - `uid`
- for containers:
  - `pod/<pod-key>`
  - `name`
  - `namespace`
  - `qosclass`
  - `labels/<label-key>`
  - `tags/<tag-key>`
  - `id`

Essentially an expression defines a logical operation of the form (key op values).
Evaluating this logical expression will take the value of the key in  which
either evaluates to true or false.
a boolean true/false result. Currently the following operations are supported:

- `Equals`: equality, true if the *value of key* equals the single item in *values*
- `NotEqual`: inequality, true if the *value of key* is not equal to the single
  item in *values*
- `In`: membership, true if *value of key* equals to any among *values*
- `NotIn`: negated membership, true if the *value of key* is not equal to any
  among *values*
- `Exists`: true if the given *key* exists with any value
- `NotExists`: true if the given *key* does not exist
- `AlwaysTrue`: always evaluates to true, can be used to denote node-global
  scope (all containers)
- `Matches`: true if the *value of key* matches the globbing pattern in values
- `MatchesNot`: true if the *value of key* does not match the globbing pattern
  in values
- `MatchesAny`: true if the *value of key* matches any of the globbing patterns
  in values
- `MatchesNone`: true if the *value of key* does not match any of the globbing
  patterns in values

The effective affinity between containers C_1 and C_2, A(C_1, C_2) is the sum
of the weights of all pairwise in-scope matching affinities W(C_1, C_2). To put
it another way, evaluating an affinity for a container C_1 is done by first
using the scope (expression) to determine which containers are in the scope of
the affinity. Then, for each in-scope container C_2 for which the match
expression evaluates to true, taking the weight of the affinity and adding it
to the effective affinity A(C_1, C_2).

Note that currently (for the topology-aware policy) this evaluation is
asymmetric: A(C_1, C_2) and A(C_2, C_1) can and will be different unless the
affinity annotations are crafted to prevent this (by making them fully
symmetric). Moreover, A(C_1, C_2) is calculated and taken into consideration
during resource allocation for C_1, while A(C_2, C_1) is calculated and taken
into account during resource allocation for C_2. This might be changed in a
future version.

Currently affinity expressions lack support for boolean operators (and, or, not).
Sometimes this limitation can be overcome by using joint keys, especially with
matching operators. The joint key syntax allows joining the value of several keys
with a separator into a single value. A joint key can be specified in a simple or
full format:

- simple: `<colon-separated-subkeys>`, this is equivalent to
  `:::<colon-separated-subkeys>`
- full:   `<ksep><vsep><ksep-separated-keylist>`

A joint key evaluates to the values of all the `<ksep>`-separated subkeys
joined by `<vsep>`. A non-existent subkey evaluates to the empty string. For
instance the joint key

  `:pod/qosclass:pod/name:name`

evaluates to

  `<qosclass>:<pod name>:<container name>`

For existence operators, a joint key is considered to exist if any of its
subkeys exists.

### Examples

Put the container `peter` close to the container `sheep` but far away from the
container `wolf`.

```yaml
    resource-policy.nri.io/affinity: |
      peter:
      - match:
          key: name
          operator: Equals
          values:
          - sheep
        weight: 5
    resource-policy.nri.io/anti-affinity: |
      peter:
      - match:
          key: name
          operator: Equals
          values:
          - wolf
        weight: 5
```

### Shorthand Notation

There is an alternative shorthand syntax for what is considered to be the most common
case: defining affinities between containers within the same pod. With this notation
one needs to give just the names of the containers, like in the example below.

```yaml
  annotations:
    resource-policy.nri.io/affinity: |
      container3: [ container1 ]
    resource-policy.nri.io/anti-affinity: |
      container3: [ container2 ]
      container4: [ container2, container3 ]
```

This shorthand notation defines:

- `container3` having
  - affinity (weight 1) to `container1`
  - `anti-affinity` (weight -1) to `container2`
- `container4` having
  - `anti-affinity` (weight -1) to `container2`, and `container3`

The equivalent annotation in full syntax would be

```yaml
metadata:
  annotations:
    resource-policy.nri.io/affinity: |+
      container3:
      - match:
          key: labels/io.kubernetes.container.name
          operator: In
          values:
          - container1
    resource-policy.nri.io/anti-affinity: |+
      container3:
      - match:
          key: labels/io.kubernetes.container.name
          operator: In
          values:
          - container2
      container4:
      - match:
          key: labels/io.kubernetes.container.name
          operator: In
          values:
          - container2
          - container3
```
