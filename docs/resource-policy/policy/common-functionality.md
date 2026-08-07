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


## Class Based CPU Tuning

Some plugins provide fine grained control over CPU behavior and power
management. They use the common plugin agnostic CPU class functionality
for doing this. Currently the [balloons](balloons.md#cpu-tuning) and
[topology-aware](topology-aware.md) policies have such functionality.
For a more policy specific description of the provided functionality
please refer to their respective documentation.

The sections below only describe the common policy agnostic CPU class
functionality.

### CPU class definitions. Each class is an object with:

- `name` (string): Class name referenced by policy configuration.
- `minFreq` (string): Minimum CPU frequency. Accepts values with
  units: `"3.2GHz"`, `"2900MHz"`, `"2900000kHz"`, or a string
  containing plain number in kHz: `"2900000"`. Also accepts symbolic
  names: `"min"` (platform minimum), `"base"` (CPU base frequency),
  `"turbo"` (maximum turbo frequency), which are resolved at runtime
  from sysfs.
- `maxFreq` (string): Maximum CPU frequency (same format).
- `uncoreMinFreq` / `uncoreMaxFreq` (string): Uncore frequency limits
  (same format).
- `disabledCstates` (list): C-state names to disable (e.g., `["C6", "C8"]`).
  - Disabling deep C-states reduces latency by preventing deep sleep.
  - Disabling intermediate C-states keeps CPU more responsive longer
    after use, but allows it to enter deeper power saving states if
    not needed.
  - List available C-states: `grep
    . /sys/devices/system/cpu/cpu0/cpuidle/state*/name`.
- `energyPerformancePreference` (integer): EPP value for CPUs.
- `freqGovernor` (string): CPUFreq governor (e.g., `"performance"`).
- `turboPriority` (integer): Controls exclusive turbo frequency
  access. Among CPU classes with active allocations, only the class
  with the highest `turboPriority` gets the symbolic frequency
  `"turbo"` resolved to the actual turbo frequency. All other
  classes get `"turbo"` resolved to the base frequency. When the
  highest-priority class no longer has active allocations, the next
  highest-priority class regains turbo. If all classes have
  `turboPriority` 0 (default), every class gets real turbo -- no
  competition occurs. `turboPriority` arbitration is scoped to a
  *turbo domain* (see `turboDomain` below), so on multi-socket
  systems a low-priority class on one socket can keep turbo even
  when a higher-priority class is active on another socket.
- `pctPriority` (string): reset system PCT configuration and use
  `high`  or `low` priority CLOSes for CPUs. See [Priority Core
  Turbo](#priority-core-turbo-pct) for details.
- `sstClosID` (integer): use system PCT configuration and assign
  CPUs to specified CLOS. See [Priority Core
  Turbo](#priority-core-turbo-pct) for details.
- `publishExtendedResource` (bool). If `true` in a class with either
  `pctPriority: high` or `sstClosID: n` policy publishes class's
  available CPU capacity using a policy-specific extended resource.
  Enables Kubernetes to schedule pods on nodes with enough
  CPUs of wanted priority. Notes: container's `cpu` and extended
  resource requests **must be equal** to avoid over and under
  subscription. Extended resources are not cleaned up from node status
  when a plugin is stopped or uninstalled. The agent's reconciliation
  removes extended resources when configured not to publish them.

**`turboDomain`** (string, policy-level configuration):

Selects the scope over which `turboPriority` arbitration happens. The
default is `"package"`: every package independently pick its own
turboPriority winner. Set to `"system"` if highest `turboPriority`
classes anywhere should suppress turbo on every other class
independently of CPU core locations. On single-socket systems the two
modes behave identically.

```yaml
balloonTypes:
- name: latency-critical
  cpuClass: turbo
- name: best-effort
  cpuClass: normal
idleCPUClass: powersave

cpuClasses:
- name: turbo
  minFreq: "turbo"
  maxFreq: "turbo"
  disabledCstates: [C6, C8, C10]
  turboPriority: 10
- name: normal
  minFreq: "min"
  maxFreq: "turbo"
  turboPriority: 1
- name: powersave
  minFreq: "min"
  maxFreq: "1.2GHz"
```

Currently this is only configurable in the [balloons](balloons.md) policy.
The [topology-aware](topology-aware.md) policy always sets `turboDomain` to `package`.

#### Priority Core Turbo (PCT)

On Intel Xeon CPUs that support [Intel Speed Select
Technology](https://docs.kernel.org/admin-guide/pm/intel-speed-select.html)
(SST), policy plugins can additionally drive *Priority Core
Turbo* (PCT) on a per-cpuClass basis. PCT lets a small number of
*High Priority* (HP) cores reach the maximum turbo frequency
while the remaining *Low Priority* (LP) cores are capped. The
mapping between cpuClasses and the underlying SST-CP CLOSes is
managed by the *PCT allocator* using the
[goresctrl SST library](https://github.com/intel/goresctrl).

Two fields on a `cpuClasses` entry enable PCT:

- `pctPriority` (string, optional): `"high"` or `"low"`. When set,
  the CPU classes for PCT are put into **managed mode** for PCT: it
  performs the full SoC-wide SST setup (CP reset, TF enable, CLOS
  configuration, CP enable) and associates CPUs of any active allocations
  using this cpuClass to the HP CLOS (default CLOS 0) or the LP
  CLOS (default CLOS 3). At most one managed `high` and one
  managed `low` cpuClass is allowed.
- `sstClosID` (integer, optional, 0..*ClosCount-1*): pins this
  cpuClass to a specific CLOS slot and selects **assoc-only
  mode**: the policy only associates CPUs to the given CLOS
  without reconfiguring the SoC-wide SST state. Use this when an
  operator or the BIOS has already configured the CLOSes.

`pctPriority` and `sstClosID` are **mutually exclusive** on the
same cpuClass. Managed and assoc-only cpuClasses cannot be mixed
in the same configuration.

By default the CLOS minimum/maximum frequencies programmed in
managed mode come from the cpuClass's own `minFreq`/`maxFreq`.
Two optional overrides exist for cases where the hardware CLOS
bounds should differ from the OS-visible cpufreq limits:

- `pctMinFreq` (string, optional): CLOS minimum frequency,
  defaults to `minFreq`. Accepts the same units and symbolic
  names. Resolves `"turbo"` directly to the hardware maximum
  turbo frequency, regardless of soft `turboPriority`
  arbitration.
- `pctMaxFreq` (string, optional): CLOS maximum frequency,
  defaults to `maxFreq`. Same caveats as `pctMinFreq`.

On hosts without SST support the PCT fields are ignored with a
warning, so a single cpuClass YAML can be portable across PCT and
non-PCT systems.
