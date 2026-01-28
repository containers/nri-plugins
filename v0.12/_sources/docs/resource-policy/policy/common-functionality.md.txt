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

To enable cache control use the `control.rdt.enable` option which defaults to `false`.

Plugins can be configured to assign containers by default to a cache class named after
the Pod QoS class of the container: one of `BestEffort`, `Burstable`, and `Guaranteed`.
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
`poddefaultclass` RDT class. Effectively these containers' processes will
be assigned to the RDT CLOSes corresponding to those classes.

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
Please refer to its [documentation](https://github.com/intel/goresctrl/blob/main/doc/rdt.md) for
a more detailed description of configuration semantics.

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

This is not possible with the NRI Reference Plugins configuration CR. Here you
must use the latter full syntax.

## Cache Occupancy Monitoring Metrics

Plugins can be configured to export cache usage as Prometheus metrics. The following
configuration options must be specified:

  - `control.rdt.enable` set to `true`
  - `instrumentation.prometheusExport` set to `true`,
  - `instrumentation.httpEndpoint` set to a valid non-empty value, eg. `:8891`, and
  - `instrumentation.metrics.enabled` set to contain `policy/rdt`, `rdt`, or `policy`

When deploying with Helm, the default configuration can be modified like this:

```shell
$ helm install test -n kube-system nri-plugins/nri-resource-policy-topology-aware \
    --set config.control.rdt.enable=true \
    --set config.instrumentation.prometheusExport=true \
    --set config.instrumentation.metrics.enabled='{buildinfo,rdt}' \
    --set config.log.debug='{goresctrl}'
```

Once enabled, you'll see RDT metrics similar to the following:

```shell
$ kubectl port-forward -n kube-system ds/nri-resource-policy-topology-aware 9000:8891 &
$ wget -q --no-proxy http://127.0.0.1:9000/metrics -O-
# HELP go_build_info Build information about the main Go module.
# TYPE go_build_info gauge
go_build_info{checksum="",path="github.com/containers/nri-plugins",version="v0.10.0"} 1
# HELP nri_l3_llc_occupancy L3 (LLC) occupancy
# TYPE nri_l3_llc_occupancy counter
nri_l3_llc_occupancy{cache_id="0",rdt_class="BestEffort",rdt_mon_group=""} 655360
nri_l3_llc_occupancy{cache_id="0",rdt_class="Burstable",rdt_mon_group=""} 409600
nri_l3_llc_occupancy{cache_id="0",rdt_class="Guaranteed",rdt_mon_group=""} 0
nri_l3_llc_occupancy{cache_id="0",rdt_class="system/default",rdt_mon_group=""} 2.752512e+07
nri_l3_llc_occupancy{cache_id="1",rdt_class="BestEffort",rdt_mon_group=""} 0
nri_l3_llc_occupancy{cache_id="1",rdt_class="Burstable",rdt_mon_group=""} 0
nri_l3_llc_occupancy{cache_id="1",rdt_class="Guaranteed",rdt_mon_group=""} 491520
nri_l3_llc_occupancy{cache_id="1",rdt_class="system/default",rdt_mon_group=""} 2.818048e+07
```

The RDT-specific set of metrics collected depends on your hardware and your kernel
configuration. If supported by your environment, currently you can expect to get the
following metrics related to cache occupancy:

  - l3_llc_occupancy: L3 (LLC) occupancy

These are collected per cache ID for each RDT class/CLOS.

## Memory Bandwidth Allocation

If the hardware supports it, plugins can limit per RDT class, how much memory
bandwidth processes in containers in a class can use up altogether. You can
enable this using a slightly modified class configuration which specifies MBA
limits for each class and the partition.

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
          mbAllocation:
            all: [ 100%, 1000Mbps ]
          classes:
            BestEffort:
              l2Allocation:
                all:
                  unified: 33%
              l3Allocation:
                all:
                  unified: 33%
              mbAllocation:
                all: [ 33%, 330Mbps ]
            Burstable:
              l2Allocation:
                all:
                  unified: 66%
              l3Allocation:
                all:
                  unified: 66%
              mbAllocation:
                all: [ 66%, 660Mbps ]
            Guaranteed:
              l2Allocation:
                all:
                  unified: 100%
              l3Allocation:
                all:
                  unified: 100%
              mbAllocation:
                all: [ 100%, 1000Mbps ]
```

## Memory Bandwidth Monitoring Metrics

If you have RDT-specific metrics collection enabled and your platform supports
memory bandwidth monitoring, you can expect these related metrics to be exposed:

  - l3_mbm_local_bytes: bytes transferred to/from local memory through LLC
  - l3_mbm_total_bytes: total bytes transferred to/from memory through LLC

An example:

```shell
$ kubectl port-forward -n kube-system ds/nri-resource-policy-topology-aware 9000:8891 &
$ wget -q --no-proxy http://127.0.0.1:9000/metrics -O-
# HELP nri_l3_mbm_local_bytes bytes transferred to/from local memory through LLC
# TYPE nri_l3_mbm_local_bytes counter
nri_l3_mbm_local_bytes{cache_id="0",rdt_class="BestEffort",rdt_mon_group=""} 573440
nri_l3_mbm_local_bytes{cache_id="0",rdt_class="Burstable",rdt_mon_group=""} 1.253376e+07
nri_l3_mbm_local_bytes{cache_id="0",rdt_class="Guaranteed",rdt_mon_group=""} 0
nri_l3_mbm_local_bytes{cache_id="0",rdt_class="system/default",rdt_mon_group=""} 1.98836224e+09
nri_l3_mbm_local_bytes{cache_id="1",rdt_class="BestEffort",rdt_mon_group=""} 1.6384e+07
nri_l3_mbm_local_bytes{cache_id="1",rdt_class="Burstable",rdt_mon_group=""} 0
nri_l3_mbm_local_bytes{cache_id="1",rdt_class="Guaranteed",rdt_mon_group=""} 1.06496e+07
nri_l3_mbm_local_bytes{cache_id="1",rdt_class="system/default",rdt_mon_group=""} 1.63692544e+09
# HELP nri_l3_mbm_total_bytes total bytes transferred to/from memory through LLC
# TYPE nri_l3_mbm_total_bytes counter
nri_l3_mbm_total_bytes{cache_id="0",rdt_class="BestEffort",rdt_mon_group=""} 573440
nri_l3_mbm_total_bytes{cache_id="0",rdt_class="Burstable",rdt_mon_group=""} 1.59744e+07
nri_l3_mbm_total_bytes{cache_id="0",rdt_class="Guaranteed",rdt_mon_group=""} 0
nri_l3_mbm_total_bytes{cache_id="0",rdt_class="system/default",rdt_mon_group=""} 3.172352e+09
nri_l3_mbm_total_bytes{cache_id="1",rdt_class="BestEffort",rdt_mon_group=""} 2.236416e+07
nri_l3_mbm_total_bytes{cache_id="1",rdt_class="Burstable",rdt_mon_group=""} 0
nri_l3_mbm_total_bytes{cache_id="1",rdt_class="Guaranteed",rdt_mon_group=""} 1.318912e+07
nri_l3_mbm_total_bytes{cache_id="1",rdt_class="system/default",rdt_mon_group=""} 2.64511488e+09
```

## Metrics Specific to Monitoring Groups

If there are any monitoring groups present in the system, goresctrl produces
RDT metrics for those as well. You can differentiate between group specific and
other metrics using the `rdt_mon_group` metrics label. Metrics specific to a
monitoring group have this label set to the name of the monitoring group the
metric corresponds to.

## Cache and Memory Bandwidth Allocation and Monitoring Prerequisites

Note that for cache and memory bandwidth allocation and monitoring to work,
you must have
  - a hardware platform which supports these features,
  - resctrlfs pseudofilesystem enabled in your kernel
  - the resctrlfs filesystem mounted (possibly with extra options for your platform)
