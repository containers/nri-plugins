# Common Functionality

## Overview

There is some common functionality implemented by the generic resource management
infrastructure shared by all resource policy plugin implementations. This functionality
is available in all policies, unless stated otherwise in the policy-specific documentation.

## Cache Allocation

Plugins can be configured to exercise class-based control over the L2 and L3 cache
allocated to containers' processes. In practice, containers are assigned to classes.
Classes have a corresponding cache allocation configuration. This configuration is
applied to all containers and subsequently to all processes started in a container.

To enable cache control use the `control.rdt.enable` option which default to `false`.

Plugins can be configured to assign containers by default to a cache class named after
the Pod QoS class of a container: one of `BestEffort`, `Burstable`, and `Guaranteed`.
The configuration setting controlling this behavior is `control.rdt.usagePodQoSAsDefaultClass`
and it defaults to `false`.

Additionally, containers can be explicitly annotated to be assigned to a class.
Use the `rdtclass.resource-policy.nri.io` annotation key for this. For instance

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  annotations:
    rdtclass.resource-policy.nri.io/pod: poddefaultclass
    rdtclass.resource-policy.nri.io/container.special-container: specialclass
...
```

This will assign the container named `special-container` within the pod to
the `specialclass` RDT class and any other container within the pod to the
`poddefaultclass` RDT class.

### Cache Class/Partitioning Configuration

RDT configuration is supplied as part of the`control.rdt` configuration block.
Here is a sample snippet as a Helm chart value which assigns 33%, 66% and 100%
of cache lines to `BestEffort`, `Burstable` and `Guaranteed` Pod QoS class
containers correspondingly:

```yaml
config:
  control:
    rdt:
      enable: false
      usePodQoSAsDefaultClass: true
      options:
        l2:
          optional: true
        l3:
          optional: true
        mb:
          optional: true
      partitions:
        fullCache:
          l2Allocation:
            all:
              unified: 100%
          l3Allocation:
            all:
              unified: 100%
          classes:
            BestEffort:
              l2Allocation:
                all:
                  unified: 33%
              l3Allocation:
                all:
                  unified: 33%
            Burstable:
              l2Allocation:
                all:
                  unified: 66%
              l3Allocation:
                all:
                  unified: 66%
            Guaranteed:
              l2Allocation:
                all:
                  unified: 100%
              l3Allocation:
                all:
                  unified: 100%
```

The actual library used to implement cache control is [goresctrl](https://github.com/intel/goresctrl).
Please refer to the its [documentation](https://github.com/intel/goresctrl/blob/main/doc/rdt.md) for
a more detailed description of configuration details and semantics.

#### A Warning About Configuration Syntax Differences

Note that the configuration syntax used for cache partitioning and classes is slightly
different for [goresctrl](https://github.com/intel/goresctrl/blob/main/doc/rdt.md) and
NRI Reference Plugins. When directly using goresctrl you can use a shorthand notation
like this

```yaml
...
      classes:
        fullCache:
          l2Allocation:
            all: 100%
          l3Allocation:
            all: 100%
...
```

to actually mean

```yaml
...
      classes:
        fullCache:
          l2Allocation:
            all:
              unified: 100%
          l3Allocation:
            all:
              unified: 100%
...
```

This is not possible with the NRI Reference plugins configuration CR. Here you
must use the latter full syntax.

### Cache Allocation Prerequisites

Note that for cache allocation control to work, you must have
- a hardware platform which supports cache allocation
- resctrlfs pseudofilesystem enabled in your kernel, and loaded if it is a module
- the resctrlfs filesystem mounted (possibly with extra options for your platform)

## Cache Usage Monitoring

TBD

## Memory Bandwidth Allocation

TBD

## Memory Bandwidth Monitoring

TBD
