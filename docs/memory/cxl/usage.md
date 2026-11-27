# Deploy and configure

CAUTION: experimental. Interfaces may change.

This page tells how to install `nri-dra-driver-cxl`, how to configure it
and its claims, and what a container gets. The words are those of the
[Vocabulary](index.md#vocabulary). [local.md](local.md),
[pool.md](pool.md), [persistent.md](persistent.md) and
[leases.md](leases.md) tell how each kind of memory is used.

- [Requirements](#requirements)
- [Install with Helm](#install-with-helm)
- [RBAC and admission policies](#rbac-and-admission-policies)
- [Devices of cxl.nri.io](#devices-of-cxlnriio)
- [MemoryConfig](#memoryconfig)
- [Steering](#steering)
- [Containers without claims](#containers-without-claims)
- [Environment variables](#environment-variables)
- [What the memory contains](#what-the-memory-contains)
- [Example claims](#example-claims)
- [Flags](#flags)
- [Configuration file](#configuration-file)
- [Files on the node](#files-on-the-node)
- [Operations](#operations)

## Requirements

- Kubernetes v1.37 or later, with the [feature gates](#feature-gates)
  below.
- A container runtime with CDI and NRI enabled. The driver is tested
  with containerd 2; CRI-O is not tested. Through NRI the driver keeps
  containers off the CXL memory that they did not claim, steers them,
  and lends memory to them.
- cgroup v2.
- Linux 6.12 or later, with `CONFIG_CXL_REGION`, `CONFIG_DEV_DAX`,
  `CONFIG_DEV_DAX_CXL`, `CONFIG_DEV_DAX_KMEM`, `CONFIG_MEMORY_HOTPLUG`,
  `CONFIG_MEMORY_HOTREMOVE`, `CONFIG_FS_DAX` (without it every `mmap` of
  a dax device fails), `CONFIG_CPUSETS` and `CONFIG_MEMCG`. Devices of
  persistent memory also need `CONFIG_CXL_PMEM`, `CONFIG_LIBNVDIMM`,
  `CONFIG_BLK_DEV_PMEM`, `CONFIG_ND_PFN`, `CONFIG_BLK_DEV_LOOP` (for
  `access: cached`) and the filesystems that the node mounts
  (`CONFIG_EROFS_FS`, ...). A cluster filesystem needs its cluster stack
  on every host ([persistent.md](persistent.md#several-nodes)).
- The CXL and dax modules loaded on the nodes with CXL memory. On a node
  without `/sys/bus/cxl` the driver publishes the DRAM and the lease
  device only.
- `/sys/devices/system/memory/auto_online_blocks` set to `offline`. The
  driver onlines the memory of `numa` slices and pool devices itself,
  and the kernel must never online a shared device as RAM. A new RAM
  region is bound to kmem for a moment before the driver takes it over:
  with `online` the kernel onlines it in a kernel zone, from which the
  driver may fail to offline it.
- No udev rule that onlines hot-added memory. `40-redhat.rules` of RHEL,
  CentOS Stream and Fedora has one (`ATTR{state}="online_movable"` for
  `SUBSYSTEM=="memory"`, `ACTION=="add"`), whatever `auto_online_blocks`
  says. Override it with a copy without its "Memory hotadd request"
  section:

  ```bash
  sed '/^# Memory hotadd request/,/^LABEL="memory_hotplug_end"/d' \
      /usr/lib/udev/rules.d/40-redhat.rules > /etc/udev/rules.d/40-redhat.rules
  udevadm control --reload
  ```

  With such a rule a Prepare offlines the memory again until it stays
  offline, and fails after 30 s. It logs
  `memoryN: online again after offlining it (auto-onlined by udev?)`.

The image of the driver has the `cxl` and `ndctl` tools that the driver
runs.

### Feature gates

| feature gate | used for |
|:-------------|:---------|
| `DRADeviceBindingConditions` | pool devices: the scheduler waits until the pool controller has attached the device |
| `DRAResourceClaimDeviceStatus` | pool devices: the pool controller writes the facts of the device into the claim status |
| `DRAConsumableCapacity` | many claims on one device: slices of a local memdev, regions, DRAM, `shares` of a shared device |
| `DRAPrioritizedList` | `firstAvailable` requests, for instance local memory first, then pool memory |
| `DRANodeAllocatableResources` | claimed memory is accounted against the allocatable memory of the node |

The driver needs the first four. Without `DRANodeAllocatableResources`
the API server drops `nodeAllocatableResources` from the ResourceSlices
of the driver. The driver then logs a warning once and publishes its
devices without the field. Claimed memory is not accounted against the
allocatable memory of the node. A gate is turned on in the API server,
the scheduler and the kubelet of every node, for instance with kubeadm:

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
  - {name: feature-gates, value: DRANodeAllocatableResources=true}
scheduler:
  extraArgs:
  - {name: feature-gates, value: DRANodeAllocatableResources=true}
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
featureGates:
  DRANodeAllocatableResources: true
```

## Install with Helm

`make images` builds the image `nri-dra-driver-cxl` with the other
images of the repository, from
[`cmd/plugins/dra-driver-cxl/Dockerfile`](blob:/cmd/plugins/dra-driver-cxl/Dockerfile);
`make PLUGINS=nri-dra-driver-cxl OTHER_IMAGE_TARGETS= BINARIES= images`
builds it alone, and `IMAGE_REPO` and `IMAGE_VERSION` set its repository
and tag. The image is based on `gcr.io/distroless/static`. It has the
driver (its entrypoint), the static `cxl-request`, the `cxl` and `ndctl`
tools of ndctl v85, and the licenses of all of them in `/licenses/`.

The chart [`deployment/helm/dra-driver-cxl`](tree:/deployment/helm/dra-driver-cxl)
deploys the driver as a DaemonSet. [Its documentation](../../deployment/helm/dra-driver-cxl.md)
lists the values. For example, a configuration that leaves one memdev
alone and shares another one, with the DaemonSet only on the nodes that
have CXL memory:

```bash
cat > cxl-values.yaml <<'EOF'
config:
  ignoreMemdevs: ["0xc100e2e0"]
  sharedMemdevs: ["0xc100e2e2"]
nodeSelector:
  example.com/cxl: "true"
EOF
helm install -n kube-system cxl deployment/helm/dra-driver-cxl -f cxl-values.yaml
```

The chart creates the DaemonSet `nri-dra-driver-cxl` with its
ServiceAccount, the ClusterRole and ClusterRoleBinding of the driver,
the admission policies of [RBAC and admission policies](#rbac-and-admission-policies),
a ConfigMap with the [configuration file](#configuration-file), and the
nine [DeviceClasses](index.md#deviceclasses). The pod of the DaemonSet:

- is privileged and runs in the host PID namespace: the NRI plugin reads
  the cgroups of container processes, and the lease broker maps the
  processes that connect to it to their containers;
- mounts `/var/lib/kubelet/plugins`, `/var/lib/kubelet/plugins_registry`,
  `/var/run/cdi`, `/var/run/nri`, `/sys`, `/dev` and `/lib/modules` of
  the host at the same paths, `/var/run/nri` and `/lib/modules`
  read-only;
- has `mountPropagation: Bidirectional` on `/var/lib/kubelet/plugins`,
  so that the filesystems of mode `fs` that the driver mounts there reach
  the host, where the runtime binds them into containers;
- copies its static `cxl-request` to the host at start
  (`--lease-cli-source /bin/cxl-request`,
  `--lease-cli /var/lib/kubelet/plugins/cxl.nri.io/cxl-request`), from
  where the containers of lease claims get it mounted.

`helm uninstall -n kube-system cxl` removes the driver and its
DeviceClasses. Delete the pods with claims of the driver first: kubelet
needs the running driver to unprepare their claims, and a pool device is
detached only after that.

## RBAC and admission policies

The ClusterRole `nri-dra-driver-cxl` lets the driver get nodes; list,
watch, create, update and delete ResourceSlices; and get, list, create
and delete ResourceClaims and pods in every namespace, for the
[companions of leases](leases.md#companion-objects). Two
ValidatingAdmissionPolicies of the chart, bound with
`validationActions: [Deny]`, narrow that down for the user
`system:serviceaccount:<release namespace>:nri-dra-driver-cxl`:

- `resourceslices-policy-nri-dra-driver-cxl`: it creates, updates and
  deletes only the ResourceSlices of its own node (`spec.nodeName`).
- `lease-companions-policy-nri-dra-driver-cxl`: it creates and deletes
  only pods and ResourceClaims named `cxl-lease-<lease>` with the labels
  `cxl.nri.io/lease=<lease>`, `cxl.nri.io/requester-uid` and
  `cxl.nri.io/node`, whose node label is its own node. The claims
  request only the classes `cxl-local` and `cxl-pool` and their
  `-shared` and `-persistent` variants, without `adminAccess`. The pods
  are pinned to its node by a required node affinity (`matchFields` on
  `metadata.name`), use only the claim of their own name, and run
  `leases.companionImage` without a command or args. They have no
  volumes, host network, PID or IPC namespace, host ports, privileged
  containers or added capabilities. With `leases.priorityClassNames: []`
  (the default) a pod may have any priority class but
  `system-node-critical` and `system-cluster-critical`; with a list, no
  class or one of the list.

The API server knows the node of the user from the token of the driver's
pod (the user extra `authentication.kubernetes.io/node-name`): the
policies need the `ServiceAccountTokenPodNodeInfo` feature, and without
it they refuse every request of the ServiceAccount.

Things that make the companion policy refuse the companions, so that the
leases fail:

- A mutating admission webhook that adds volumes or containers to the
  pods of the requesters' namespaces, or rewrites their images (sidecar
  injection, registry mirrors). Exempt the pods with the label
  `cxl.nri.io/lease` from it.
- A `leases.priorityClassNames` list without the `globalDefault`
  PriorityClass of the cluster. A companion pod has the priority class
  of the pod that requests the lease, and the `Priority` admission
  plugin gives a pod without a class the `globalDefault` one.

With `leases.namespaceLabels` the chart adds a third policy,
`lease-namespaces-policy-nri-dra-driver-cxl`, which binds every user:
only namespaces with all those labels may have ResourceClaims and
ResourceClaimTemplates that request the class `cxl-lease`
([leases.md](leases.md#opting-in)).

## Devices of cxl.nri.io

The driver publishes the devices of a node in one pool (pool name = node
name), in ResourceSlices of at most 128 devices, under the driver
`cxl.nri.io`:

```yaml
- name: memdev-c100e2e1              # "memdev-" + serial in lower-case hex
  attributes:
    type:            {string: memdev}
    source:          {string: local}
    serial:          {string: "0xc100e2e1"}
    shared:          {bool: false}
    persistent:      {bool: false}
    size:            {int: 1073741824}          # usable RAM capacity of the memdev
    resource.kubernetes.io/numaNode: {int: 0}   # CPU NUMA node; absent if unknown
    memoryBlockSize: {int: 134217728}           # every slice is a multiple of it
  capacity:
    memory: {value: 1Gi, requestPolicy: {default: 1Gi, validRange: {min: 128Mi, step: 128Mi}}}
  allowMultipleAllocations: true
  nodeAllocatableResources: {memory: {mapping: {capacityKey: memory, capacityMultiplier: "1"}}}
- name: memdev-c100e2e2              # a shared local memdev (sharedMemdevs)
  attributes:
    type:            {string: memdev}
    source:          {string: local}
    serial:          {string: "0xc100e2e2"}
    shared:          {bool: true}
    persistent:      {bool: false}
    size:            {int: 1073741824}
    resource.kubernetes.io/numaNode: {int: 0}
  capacity:
    shares: {value: 1M}                         # no requestPolicy
  allowMultipleAllocations: true
- name: region0-numa2                # region0, online as NUMA node 2
  attributes:
    type:            {string: region}
    source:          {string: local}
    memoryNUMANode:  {int: 2}
    size:            {int: 2147483648}
    resource.kubernetes.io/numaNode: {int: 0}   # if all memdevs of the region agree
  capacity: {memory: {value: 2Gi}}
  allowMultipleAllocations: true
  nodeAllocatableResources: {memory: {mapping: {capacityKey: memory, capacityMultiplier: "1"}}}
- name: dram
  attributes:
    type:            {string: dram}
    source:          {string: local}
    memoryNUMANodes: {string: "0-1"}            # cpuset syntax
    size:            {int: 16777216000}
  capacity: {memory: {value: 16000Mi}}
  allowMultipleAllocations: true
  nodeAllocatableResources: {memory: {mapping: {capacityKey: memory, capacityMultiplier: "1"}}}
- name: lease
  attributes:
    type:            {string: lease}
  allowMultipleAllocations: true
```

| attribute | devices | value |
|:----------|:--------|:------|
| `type` | all | `memdev`, `region`, `dram` or `lease` |
| `source` | `memdev`, `region`, `dram` | `local` (pool devices have `pool`) |
| `size` | `memdev`, `region`, `dram` | bytes: the usable RAM capacity of a memdev (the persistent capacity of a persistent memdev), the online memory of a region, the DRAM of the nodes |
| `serial` | `memdev` | `0x` and lower-case hex |
| `shared` | `memdev` | `true`: a memdev in `sharedMemdevs`, whose claims map the same bytes |
| `persistent` | `memdev` | `true`: persistent memory, used in modes `block` and `fs` |
| `memoryNUMANode` | `region` | the NUMA node of the region's memory |
| `memoryNUMANodes` | `dram` | the DRAM NUMA nodes, cpuset syntax |
| `resource.kubernetes.io/numaNode` | `memdev`, `region` | the CPU NUMA node of the device; absent if unknown (for a region: unless all its memdevs agree). The standard attribute: `matchAttribute` aligns it with the devices of other drivers |
| `memoryBlockSize` | `memdev`, not shared, not persistent | the memory block size of the node in bytes |

- `memdev-<serial hex>`: a local memdev without a region. Every claim
  gets a slice of it ([local.md](local.md)). A memdev in `sharedMemdevs`
  has a `shares` capacity instead, and every claim maps the whole memdev
  ([local.md](local.md#shared-local-memdevs)). A persistent memdev has a
  `shares` capacity too ([persistent.md](persistent.md)).
- `<region>-numa<N>`: a region of RAM that exists already (created by
  the BIOS or an admin), is enabled and has memory online as NUMA node
  N. Its size is that of its online memory. Claims consume its capacity,
  and the containers that opt in to steering are steered to N.
- `dram`: one device for all DRAM NUMA nodes, for steering; absent with
  `ignoreDRAM`.
- `lease`: the device of lease claims ([leases.md](leases.md)); absent
  with `--leases=false`.

A selector should test `type` first. Every device has `type`, every
memory device has `source` and `size`; the other attributes exist on
some devices only, and a selector that reads an attribute that a device
does not have fails the allocation of the claim. The classes of the
chart guarantee the attributes of their devices. Pool devices are
published by the pool controller under `cxl-pool.nri.io` with the same
memdev attributes and `coherent`
([pool.md](pool.md#what-the-pool-controller-publishes)); local memdevs
have no `coherent`.

The driver scans the node after udev events, after every Prepare and
Unprepare that changed its records, and at least every 2 minutes, and
writes the ResourceSlice when its devices changed.

## MemoryConfig

Both drivers take one kind of opaque configuration, `MemoryConfig` of
apiVersion `cxl.nri.io/v1alpha1`, in claims and in DeviceClasses. The
`driver` of the entry selects the driver that reads it; the parameters
are the same for local and pool devices. Every section and every field
is optional:

```yaml
config:
- opaque:
    driver: cxl-pool.nri.io            # or cxl.nri.io
    parameters:
      apiVersion: cxl.nri.io/v1alpha1  # for both drivers
      kind: MemoryConfig
      device:                          # how the device of a result is prepared
        mode: numa
      container:                       # how a container that uses the result is managed
        steering: {policy: auto}
      pool:                            # cxl-pool.nri.io only: for the pool controller
        opaque:
          fake-cxl-pool: {numaNode: 1}
```

Each section has one owner, which reads only its section:

| section | owner | when | its errors |
|:--------|:------|:-----|:-----------|
| `device` | the kubelet plugin of the device's driver | NodePrepareResources, per allocation result | fail the Prepare: kubelet retries, the pod does not start |
| `container` | the NRI plugin | CreateContainer of a container that uses the result | invalid values fail the Prepare; results of a container that disagree fail its CreateContainer, and the container is not created |
| `pool` | the pool controller of the device | after the allocation, before the attach | claim condition `cxl-pool.nri.io/AttachFailed`, reason `InvalidConfig`; the scheduler allocates the claim again until the binding timeout |

The driver checks the structure of `pool` (no unknown fields next to
`opaque`), never the object of a controller.

### Fields

*class* is an entry of a DeviceClass, *claim* an entry of a claim,
*node* the `defaults.container` of the
[configuration file](#configuration-file).

| field | values | default | set in | |
|:------|:-------|:--------|:-------|:-|
| `device.mode` | `numa`, `dax`, `block`, `fs` | by the device, below | class, claim | `numa`: system RAM of a NUMA node. `dax`: a dax device `/dev/daxX.Y` of its own to `mmap`. `block`, `fs`: devices of persistent memory only ([persistent.md](persistent.md)) |
| `device.readOnly` | `false`, `true` | `false`; `true` in mode `fs` | class, claim | `dax`, `block`: the device node allows reads only. `fs`: `false` mounts read-write. `null` returns the default of the mode |
| `device.access` | `direct`, `cached` | `direct` when the filesystem has DAX | class, claim | `fs` only: file pages in the device (DAX) or in the page cache of the node |
| `device.fsType` | lower-case letters, digits, `_` and `-` | detected | class, claim | `fs` only: the expected type of the filesystem ([persistent.md](persistent.md#filesystems)) |
| `device.path` | an absolute path | `/run/cxl-fs/<KEY>` | class, claim | `fs` only: the directory of the filesystem in the container; not `/`, not in `/dev`, `/proc`, `/sys`, `/run/cxl` or `/var/run/cxl` |
| `container.steering.policy` | `none`, `auto`, `first-dram`, `first-cxl`, `start-interleaved`, `end-interleaved`, `waypoints` | `none` | class, claim, node | how the allocations of the container are spread over the DRAM and CXL memory of its results ([Steering](#steering)) |
| `container.steering.waypoints` | list of `{targetUsages: [{memoryType: DRAM or CXL, usage: <quantity>}]}` | | class, claim, node | policy `waypoints` only, which needs them: the target usage per memory type at each waypoint. A type that is not listed keeps its usage of the previous waypoint; usages never decrease |
| `container.steering.minStep` | quantity | the smallest change of a usage between two waypoints, at most `maxStep` | class, claim, node | the smallest amount of new allocations after which the plan is evaluated again |
| `container.steering.maxStep` | quantity | the total quota | class, claim, node | the largest amount of new allocations after which the plan is evaluated again |
| `container.fencing` | `allowed-mems`, `none` | `allowed-mems` | node | whether containers are kept off the CXL NUMA nodes that they did not claim ([Containers without claims](#containers-without-claims)) |
| `pool.opaque.<key>` | an object that the controller defines | | class, claim of `cxl-pool.nri.io` | the settings of the pool controllers with controller key `<key>` ([pool.md](pool.md#configuration-of-a-pool-controller)) |

The modes by DeviceClass:

| class | modes | default |
|:------|:------|:--------|
| `cxl-local`, `cxl-pool` | `numa`, `dax` | `numa` |
| `cxl-local-shared`, `cxl-pool-shared` | `dax` | `dax` |
| `cxl-local-persistent`, `cxl-pool-persistent` | `block`, `fs` | none |
| `cxl-region`, `cxl-dram` | `numa` | `numa` |
| `cxl-lease` | none: no `device` settings | |

`readOnly` needs mode `dax`, `block` or `fs`, and `access`, `fsType` and
`path` need mode `fs`. Regions and DRAM are `numa` only: in a claim that
has them next to memdevs in mode `dax`, the entry with `mode: dax` lists
the memdev requests in its `requests`.

Read-only is enforced by the kernel for every process of the container,
whatever its capabilities, except in privileged containers: their device
cgroup allows everything. It is a property of the claim, not of the
device: another claim of the same device may write, and the readers see
it.

### Merge order

A driver resolves the configuration of every allocation result of a
claim:

1. The entries of the result's driver apply that have no `requests`,
   that list the result's request, or, for a subrequest `r/s` of a
   `firstAvailable` request, that list `r` or `r/s`.
2. The entries of the DeviceClass come first, then the entries of the
   claim, each in the order of its list.
3. Each entry is decoded alone, strictly and case-sensitively. Every
   entry of the driver is checked so, also one for another request.
4. The parameters merge in that order as a JSON merge patch (RFC 7386):
   objects merge field by field, scalars and lists (`waypoints`)
   replace, and `null` removes a field, which returns it to its default.
5. The result is validated again and checked against the device (type,
   shared, persistent, single host, coherent).

So a class with `device: {mode: dax}` and a claim with
`device: {readOnly: true}` give a read-only dax slice, and a claim with
`device: {mode: null}` of the same class gives a `numa` slice.

### Container settings

A container uses the results of the requests that it references: all
results of a claim that it lists without a `request`, and only the
result of the request that `resources.claims[].request` names. Only
those results count for the container: their memory for its steering and
its `cpuset.mems`, and their `container` sections. A container that uses
the `dax` request of a claim is neither steered by nor given the NUMA
node of the `numa` request of the same claim, which another container
uses.

1. A `container` field that several results of the container set must
   have the same value in all of them; a field that only some of them
   set is taken from those. A conflict fails CreateContainer, and the
   container is not created.
2. The `defaults.container` of the configuration file, overlaid by these
   fields, is effective. The `waypoints`, `minStep` and `maxStep` of the
   defaults apply only when the results set no policy or the policy of
   the defaults: a claim with `policy: none` on a node whose default is
   `{policy: first-dram, maxStep: 1Gi}` is not steered. A container
   without results gets the defaults.
3. The effective steering takes `waypoints`, `minStep` and `maxStep`
   only with a policy other than `none`, and policy `waypoints` needs
   `waypoints`; else CreateContainer fails.

The `container` sections of different requests of one claim may differ:
they reach different containers.

### Presets in DeviceClasses

A DeviceClass may carry `MemoryConfig` entries of its driver. They are
the first layer of the [merge](#merge-order), so a claim of the class
changes single fields of them. A class for dax slices:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata: {name: cxl-local-dax}
spec:
  selectors:
  - cel: {expression: 'device.driver == "cxl.nri.io" && device.attributes["cxl.nri.io"].type == "memdev" && device.attributes["cxl.nri.io"].shared == false && device.attributes["cxl.nri.io"].persistent == false'}
  config:
  - opaque:
      driver: cxl.nri.io
      parameters: {apiVersion: cxl.nri.io/v1alpha1, kind: MemoryConfig, device: {mode: dax}}
```

The Helm value `deviceClassConfig` gives the classes of the chart such
an entry: class name to a `MemoryConfig` body (`device`, `container`,
and `pool` for the classes of `cxl-pool.nri.io`; `container` only for
`cxl-lease`). Steering on for every container with memory of `cxl-local`
or `cxl-pool`, unless its claim says `container: {steering: {policy: none}}`:

```yaml
deviceClassConfig:
  cxl-local:
    container: {steering: {policy: auto}}
  cxl-pool:
    container: {steering: {policy: auto}}
```

A preset applies to every claim of the class, also to the companion
claims of leases. The lease broker sets the mode and `readOnly` of a
companion itself; the other fields of the preset apply to it as to any
claim. Steering on for every container of a node is
`defaults.container.steering: {policy: auto}` in the
[configuration file](#configuration-file).

### Examples of configurations

Steering on for the containers of a claim (the same with
`driver: cxl-pool.nri.io` for pool memory):

```yaml
config:
- opaque: {driver: cxl.nri.io, parameters: {apiVersion: cxl.nri.io/v1alpha1, kind: MemoryConfig, container: {steering: {policy: auto}}}}
```

Two requests for two containers: `app` uses `numa`, steered DRAM first
in 1Gi steps; `cache` uses `dax`, and gets neither the NUMA node nor
steering:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: {name: app-memory}
spec:
  devices:
    requests:
    - {name: dram, exactly: {deviceClassName: cxl-dram,  capacity: {requests: {memory: 4Gi}}}}
    - {name: numa, exactly: {deviceClassName: cxl-local, capacity: {requests: {memory: 4Gi}}}}
    - {name: dax,  exactly: {deviceClassName: cxl-local, capacity: {requests: {memory: 1Gi}}}}
    config:
    - requests: [dram, numa]
      opaque: {driver: cxl.nri.io, parameters: {apiVersion: cxl.nri.io/v1alpha1, kind: MemoryConfig, container: {steering: {policy: first-dram, maxStep: 1Gi}}}}
    - requests: [dax]
      opaque: {driver: cxl.nri.io, parameters: {apiVersion: cxl.nri.io/v1alpha1, kind: MemoryConfig, device: {mode: dax}}}
---
apiVersion: v1
kind: Pod
metadata: {name: app}
spec:
  resourceClaims:
  - {name: mem, resourceClaimName: app-memory}
  containers:
  - name: app
    image: docker.io/library/python:3-alpine
    command: ["sleep", "infinity"]
    resources: {claims: [{name: mem, request: dram}, {name: mem, request: numa}]}
  - name: cache
    image: docker.io/library/python:3-alpine
    command: ["sleep", "infinity"]
    resources: {claims: [{name: mem, request: dax}]}
```

### Errors

An error of a configuration starts with `MemoryConfig: ` and the path
of the field. An error of an entry names the entry: `FromClass` or
`FromClaim` and its index among the entries of its source, from 0. The
Prepare error of both drivers puts `DRIVER: claim NS/NAME: ` before it,
and `request R: ` or `device D (memdev 0xS): ` before an error of a
result. The CreateContainer error starts with `container NS/POD/CTR: `
and names where the settings came from:

```
cxl.nri.io: claim ns/a: MemoryConfig: FromClaim config 0: json: unknown field "device.online"
cxl.nri.io: claim ns/a: request mem: MemoryConfig: device.readOnly needs mode dax
cxl-pool.nri.io: claim ns/b: device pooled0 (memdev 0xc1ee0001): MemoryConfig: device.mode block needs a persistent device
container ns/pod/ctr: container.steering.policy: claim ns/a request r says first-cxl, claim ns/b request s says none
container ns/pod/ctr: MemoryConfig: container.steering.maxStep needs a policy other than none (policy none (default), maxStep 1Gi from claim ns/a request r)
```

## Steering

Steering is opt-in: a container is steered when its effective
`container.steering.policy` ([Container settings](#container-settings))
is not `none` and its results have memory in mode `numa`. A claim opts
in with one line ([Examples of configurations](#examples-of-configurations)),
a DeviceClass for all its claims ([Presets in DeviceClasses](#presets-in-deviceclasses)),
a node for all its containers (`defaults.container.steering` of the
[configuration file](#configuration-file)).

The NRI plugin finds the results of a container by their markers
(`CXL_CLAIM_<uid>_<n>`). The consumed capacity of every `dram` device is
the DRAM quota; that of every `region` device, `numa` slice and pool
device in mode `numa` is the CXL quota. The policy:

| policy | |
|:-------|:-|
| `none` | not steered (default) |
| `auto` | by the memory types of the results: `first-cxl` with only CXL memory, `first-dram` with only DRAM, `start-interleaved` with both |
| `first-dram` | the DRAM quota first, then CXL |
| `first-cxl` | the CXL quota first, then DRAM |
| `start-interleaved` | DRAM and CXL interleaved until the smaller quota is used, then the rest of the other |
| `end-interleaved` | the excess of the larger quota first, then interleaved |
| `waypoints` | along `waypoints` |

The steering adds the NUMA nodes to the container's `cpuset.mems` as the
plan reaches them (memory is never migrated), sets the memory policy of
the container's threads, and throttles each step with `memory.high`.
Past the last waypoint the container keeps the memory policy of the last
segment of the plan, and `memory.high` is `max`. Its allocations go on
in that direction, beyond its claims, as far as `memory.max` and the
free memory of the NUMA nodes allow. When its usage drops below the last
waypoint again by `minStep` or more, the steering resumes.

A container with only CXL memory onlined movable has no NUMA node with
normal (not movable) memory in its `cpuset.mems`. The steering adds the
nearest NUMA node with normal memory to it. Unmovable user allocations,
such as pipe buffers, obey `cpuset.mems` but cannot use movable memory:
without that node the container would not start. The memory policy keeps
the allocations of the container on the CXL NUMA node.

Steering is not for every workload. It narrows `cpuset.mems` to the
NUMA nodes of the plan and holds the cgroup at `memory.high` until the
plan moves on. It sets the memory policy of the processes of the
container at each step; a process that `kubectl exec` starts gets it at
the next step. Workloads that place their memory themselves, such as HPC
applications and databases, are better left unsteered. A steered
container cannot take a `numa` lease ([leases.md](leases.md#limitations)).

Steering can crash or disturb the programs that it steers. The NRI
plugin sets the memory policy of a running process by injecting a
`set_mempolicy` system call into each of its threads with ptrace. For a
moment the injection writes that system call and a breakpoint
instruction into the code of the process. A task that shares the memory
of the process and runs meanwhile can execute that code and kill the
process with SIGTRAP: a thread that the plugin could not stop, a thread
that appeared during the injection, or a child made with `vfork`. Such
children are common: glibc's `posix_spawn`, Go's `os/exec` and Python's
`subprocess` make them to start other programs.

The injection can also reset a SIGTRAP handler of the process to the
default, lose a signal that arrives meanwhile, kill a 32-bit process,
and leave the nodemask in the memory below the stack pointer of each
thread. A thread in an uninterruptible sleep stalls the injection
without a time limit, and the stop of the container waits for it. The
first injection comes when the container starts, before the runtime
executes the workload. Its process is then the init process of the OCI
runtime, which for runc is a threaded Go program.

Threaded programs may misbehave. Use steering for experiments, or with
programs that are simple enough: few threads, no subprocesses, no
tracer, no use of SIGTRAP. Steering is off unless a claim, a DeviceClass
or the node defaults opt in.

A restart of the driver does not restart the steering of running
containers, nor steers those created while it was down: they are
managed like containers that are not steered, and keep their memory
policies. The restarted driver sets the `memory.high` of the containers
that it steered to `max`, as the end of a steering does.

Steering needs:

- an amd64 node;
- ptrace access to the processes of the container: the NRI plugin stops
  their threads to set their memory policy. This needs `CAP_SYS_PTRACE`,
  the host PID namespace (the chart has both) and a Yama `ptrace_scope`
  below 3;
- a seccomp profile of the container that allows `set_mempolicy`:
  containerd's `RuntimeDefault` allows it only with `CAP_SYS_NICE`;
- `memory.high` `max` at the start of the container: steering never
  replaces a bound that others set, such as kubelet's MemoryQoS.

A container whose steering cannot start, or whose process refuses the
memory policy of a later step, is managed like a container that is not
steered, and the NRI plugin logs one warning with the reason:

```
failed to start memory steering: <reason>; memory fenced
memory steering ended: <reason>; memory fenced
```

A container that is not steered (policy `none`, or no `numa` memory) is
managed like a [container without claims](#containers-without-claims).
With fencing `allowed-mems` its `cpuset.mems` has the NUMA nodes of its
`numa` results and leases next to its own. Its memory policy and
`memory.high` are left alone. The kernel allocates its memory from the
NUMA nodes of its CPUs first. The workload binds memory to the CXL NUMA
node itself with the number in `CXL_<KEY>_NUMA_NODE`, for example
with `numactl --membind` or `mbind`. Its `dram` and `region` results
reserve capacity only.

The NRI plugin logs the management of every container at verbosity 3,
with the effective policy and fencing and where they came from:

```
NRI CreateContainer: ctr=<ctr>: memory steered (steering auto from claim ns/a request r, fencing allowed-mems (default))
NRI CreateContainer: ctr=<ctr>: memory fenced (steering none (default), fencing allowed-mems (default))
```

## Containers without claims

The driver keeps the NUMA nodes on which it onlines CXL memory away from
the containers that have not claimed memory there. These are the NUMA
nodes of `numa` slices and of pool devices in mode `numa`, also those of
the companion claims of leases. The NUMA nodes of `region` devices
(regions that the driver did not create) and DRAM are not among them:
they are system RAM from boot, used by host processes and running
containers.

A container has such a NUMA node only through

- a `numa` result on it: a steered container gets it from the steering,
  any other container in its `cpuset.mems` next to its own NUMA nodes;
- a lease in mode `numa`: the NUMA node is lent ([leases.md](leases.md#flow)).

Every container that is not steered gets `cpuset.mems` = its own NUMA
nodes without these CXL NUMA nodes, plus the NUMA nodes of its `numa`
results and leases. Its own NUMA nodes are the `cpuset.mems` of its spec
(set by the kubelet memory manager or an earlier NRI plugin), or all
NUMA nodes with memory when the spec has none. The running containers
are updated before the memory of a new NUMA node goes online, so no
running container sees it. A NUMA node stays kept away when its last
claim is released, until the driver restarts.

`defaults.container.fencing` of the configuration file selects this for
every container of the node; claims and DeviceClasses cannot set it:

| value | |
|:------|:-|
| `allowed-mems` | (default) a container that has not claimed memory on a CXL NUMA node of the driver does not have it in its `cpuset.mems`. It cannot allocate there even with `mbind` or `set_mempolicy`, and a container out of other memory is OOM-killed rather than spilling to CXL memory. |
| `none` | containers are left as they are: one without its own `cpuset.mems` inherits every NUMA node with memory, CXL NUMA nodes too |

Steering and leases work with both values. At start the driver logs the
node defaults:

```
defaults.container.fencing: allowed-mems: containers get the CXL NUMA nodes of the driver only by claims and leases
defaults.container.steering.policy: none: containers are steered only when their claims opt in
```

Not covered:

- Pod sandboxes (the `pause` containers): NRI cannot update their
  `cpuset.mems`. The pause process allocates nothing after its start.
- Processes outside containers (kubelet, the container runtime, systemd
  services) keep every NUMA node.

An NRI resource policy that sets the `cpuset.mems` of containers
(nri-resource-policy balloons or topology-aware) needs
`defaults.container.fencing` `none` and no steering: NRI refuses a
container whose `cpuset.mems` two plugins set, and the container is not
created.

## Environment variables

Every memory device of a claim, and the memory of a lease, gives a
container the same set of variables:

| variable | value | present for |
|:---------|:------|:------------|
| `CXL_<KEY>_SERIAL` | `0x<hex>` | memdevs (local and pool) |
| `CXL_<KEY>_SHARED` | `0` or `1` | memdevs: the attribute `shared`, whatever the `shares` request |
| `CXL_<KEY>_MODE` | `numa`, `dax`, `block` or `fs` | every memory device |
| `CXL_<KEY>_SIZE` | bytes | every memory device: what the claim or lease got (the slice of a local memdev, the whole pool device, the consumed capacity of a region or DRAM; in modes `block` and `fs` the size of the fsdax namespace) |
| `CXL_<KEY>_NUMA_NODE` | NUMA node | mode `numa`: slices, pool devices, regions |
| `CXL_<KEY>_NUMA_NODES` | cpuset syntax | `dram` |
| `CXL_<KEY>_DAX` | `/dev/daxX.Y` | mode `dax`: the device node of a claim; of a lease, the host path (informational: the container has no such node) |
| `CXL_<KEY>_FD` | file descriptor number | leases of modes `dax` and `block`, in the command of `cxl-request ... --exec`, instead of `DAX` or `PATH` |
| `CXL_<KEY>_PATH` | path | mode `block`: the block device node of a claim; of a lease, the host path (informational); mode `fs`: the directory of the filesystem in the container (`path` of a claim, `/run/cxl/fs/<name>` of a lease) |
| `CXL_<KEY>_ACCESS` | `direct` or `cached` | mode `fs`: the effective access |
| `CXL_<KEY>_FS_TYPE` | filesystem type | mode `fs`: the type that is mounted |
| `CXL_<KEY>_READ_ONLY` | `0` or `1` | modes `dax`, `block` and `fs` |

`<KEY>` is the serial of a memdev in upper-case hex without `0x`
(`C100E2E1`), `REGION<n>` of a region (`REGION0`) and `DRAM` of the
DRAM. The second and later devices with the same key in one claim (two
slices of one memdev, two requests on one region) get `_2`, `_3`, ...
appended: `C100E2E1_2`. The count runs over the allocation results of
the claim, whatever requests a container references. Programs find what
they got by scanning `CXL_*_MODE`.

Other variables:

| variable | |
|:---------|:-|
| `CXL_CLAIM_<uid>_<n>=1` | the marker of allocation result n of a claim (the claim uid with `_` for `-`); the NRI plugin finds the results that a container uses by it |
| `CXL_LEASE_SOCKET=/run/cxl/cxl.sock` | containers of claims on `cxl-lease` |
| `CXL_LEASE=<id>` | the output of `cxl-request env` and the commands of `cxl-request ... --exec` ([leases.md](leases.md)) |

The variables and device nodes come from CDI devices, one per allocation
result of a claim: `cxl.nri.io/device=claim-<uid>-<n>` and
`cxl-pool.nri.io/device=claim-<uid>-<n>`. Each has the marker, the
variables and the device node (or, in mode `fs`, the bind mount) of its
result only. kubelet gives a container the CDI devices of the requests that it
references, all of them without a request. Two claims of the same
memdev in one container set the same names; the container sees one
value of each.

## What the memory contains

- Mode `numa`, any source: the memory is system RAM; the kernel gives a
  process only zeroed pages.
- `dax` slices of local memdevs: the driver writes zeros to the whole
  slice before the claim gets it, so a claim never reads what an earlier
  claim left there.
- Unshared pool devices in mode `dax`: the content is kept. Nothing
  clears the device between claims, so a claim, maybe on another node,
  reads what the previous one wrote.
- Shared local memdevs: the same bytes for every claim and lease of the
  memdev on the node, zeroed only when the driver creates its region: a
  claim reads what the last writer left, also after the memdev was idle,
  until the node reboots ([local.md](local.md#shared-local-memdevs)).
- Shared pool devices: the same bytes for every host that has the device
  attached, as long as the pool keeps the device.
- Devices of persistent memory: the data and the namespace labels stay
  as long as the device keeps them, also while no host has it attached
  and across reboots. A new device is all zeros, without labels. The
  driver never zeroes a persistent device ([persistent.md](persistent.md)).

## Example claims

[`deployment/samples/dra-driver-cxl`](tree:/deployment/samples/dra-driver-cxl)
has a complete claim and pod for each case:

| sample | class | configuration | the container gets |
|:-------|:------|:--------------|:-------------------|
| [local-numa.yaml](blob:/deployment/samples/dra-driver-cxl/local-numa.yaml) | `cxl-local` | | a slice as RAM; not steered, its NUMA node in `cpuset.mems` |
| [local-dax-readonly.yaml](blob:/deployment/samples/dra-driver-cxl/local-dax-readonly.yaml) | `cxl-local` | `device: {mode: dax, readOnly: true}` | a read-only dax slice |
| [local-dax-shared-claim.yaml](blob:/deployment/samples/dra-driver-cxl/local-dax-shared-claim.yaml) | `cxl-local` | `device: {mode: dax}` | one ResourceClaim that several pods reference: the same slice in all of them |
| [local-shared-writer-readers.yaml](blob:/deployment/samples/dra-driver-cxl/local-shared-writer-readers.yaml) | `cxl-local-shared` | readers: `shares: "1"`, `device: {readOnly: true}` | a writer that has the memdev alone, then readers of one share each |
| [local-persistent-fs.yaml](blob:/deployment/samples/dra-driver-cxl/local-persistent-fs.yaml) | `cxl-local-persistent` | `device: {mode: block}`, then `{mode: fs, readOnly: false}` and `{mode: fs}` | a formatter of an ext4, then a read-write and a read-only pod of it ([persistent.md](persistent.md)) |
| [pool-numa.yaml](blob:/deployment/samples/dra-driver-cxl/pool-numa.yaml) | `cxl-pool` | | a whole pool device as a new NUMA node |
| [pool-dax.yaml](blob:/deployment/samples/dra-driver-cxl/pool-dax.yaml) | `cxl-pool` | `device: {mode: dax}` | the pool device as a dax device |
| [pool-shared-writer-readers.yaml](blob:/deployment/samples/dra-driver-cxl/pool-shared-writer-readers.yaml) | `cxl-pool-shared` | readers: `shares: "1"`, `device: {readOnly: true}` | a writer that has the device alone, then readers on any node |
| [pool-persistent-erofs.yaml](blob:/deployment/samples/dra-driver-cxl/pool-persistent-erofs.yaml) | `cxl-pool-persistent` | `device: {mode: block}`, then `{mode: fs}` | a writer of an erofs image, then readers on any node |
| [pool-persistent-fs-rw.yaml](blob:/deployment/samples/dra-driver-cxl/pool-persistent-fs-rw.yaml) | `cxl-pool-persistent` | `device: {mode: block}`, then `{mode: fs, readOnly: false}` and `{mode: fs}` | a formatter of an ext4, then a read-write and a read-only pod of it on one node |
| [local-then-pool-dax.yaml](blob:/deployment/samples/dra-driver-cxl/local-then-pool-dax.yaml) | `cxl-local`, then `cxl-pool` | `device: {mode: dax}` for both drivers | a `firstAvailable` request: local memory if a local memdev has room, else pool memory |
| [region-dram-policy.yaml](blob:/deployment/samples/dra-driver-cxl/region-dram-policy.yaml) | `cxl-dram`, `cxl-region` | `container: {steering: {policy: first-dram}}` | steering, DRAM first, then a region |
| [lease.yaml](blob:/deployment/samples/dra-driver-cxl/lease.yaml) | `cxl-lease` | | the lease socket and `cxl-request` ([leases.md](leases.md)) |

The samples need the devices that their selectors name or that the
e2e VMs have; the persistent samples with ext4 need `fsTypes: [erofs, ext4]`
on the nodes. A request for a specific memdev adds a selector on its
serial:

```yaml
requests:
- name: mem
  exactly:
    deviceClassName: cxl-local
    capacity: {requests: {memory: 128Mi}}
    selectors: [{cel: {expression: 'device.attributes["cxl.nri.io"].serial == "0xc100e2e1"'}}]
```

The container of `local-numa.yaml` sees

```
CXL_C100E2E1_MODE=numa
CXL_C100E2E1_NUMA_NODE=2
CXL_C100E2E1_SERIAL=0xc100e2e1
CXL_C100E2E1_SHARED=0
CXL_C100E2E1_SIZE=134217728
CXL_CLAIM_<uid>_0=1
```

and that of `pool-dax.yaml`

```
CXL_C1EE0001_DAX=/dev/dax0.0
CXL_C1EE0001_MODE=dax
CXL_C1EE0001_READ_ONLY=0
CXL_C1EE0001_SERIAL=0xc1ee0001
CXL_C1EE0001_SHARED=0
CXL_C1EE0001_SIZE=536870912
CXL_CLAIM_<uid>_0=1
```

and that of `region-dram-policy.yaml`

```
CXL_CLAIM_<uid>_0=1
CXL_CLAIM_<uid>_1=1
CXL_DRAM_MODE=numa
CXL_DRAM_NUMA_NODES=0-1
CXL_DRAM_SIZE=67108864
CXL_REGION0_MODE=numa
CXL_REGION0_NUMA_NODE=2
CXL_REGION0_SIZE=67108864
```

[persistent.md](persistent.md#claims) has the variables of modes `block`
and `fs`.

## Flags

`--node-name` and `--kubeconfig` take their defaults from the
environment variables `NODE_NAME` and `KUBECONFIG`. Both `-flag` and
`--flag` work.

| flag | default | |
|:-----|:--------|:-|
| `--node-name` | `$NODE_NAME` | the node of the driver (required) |
| `--kubeconfig` | `$KUBECONFIG` | empty: the in-cluster configuration |
| `--cdi-root` | `/var/run/cdi` | where the CDI spec files are written |
| `--config` | (none) | the [configuration file](#configuration-file), JSON or YAML |
| `--nri-name` | `dra-driver-cxl` | NRI plugin name |
| `--nri-idx` | `30` | NRI plugin index (place in the plugin chain, higher is later) |
| `--pool` | `true` | run the pool driver `cxl-pool.nri.io`; `false` disables it |
| `--pool-prepare-timeout` | `30s` | how long the pool driver waits for the memdev of a pool device |
| `--leases` | `true` | lend memory to running containers: publish the `lease` device, serve lease sockets, create companions (needs NRI) |
| `--lease-cli` | `/usr/local/bin/cxl-request` | host path of the static `cxl-request`, mounted into the containers of lease claims |
| `--lease-cli-source` | (empty) | at start, before any CDI spec is written, copy this file to `--lease-cli` (mode 0755, replaced atomically) |
| `--lease-wait` | `120s` | how long a lease request waits for Ready when it does not say; at most `1h` |
| `--lease-companion-image` | `registry.k8s.io/pause:3.10` | image of the companion pods of leases |
| `--lease-max-connections` | `64` | open connections of the lease socket of one lease claim; one more is closed at once |
| `--lease-idle-timeout` | `10m0s` | how long a connection of a lease socket may stay without a request before the driver closes it |
| `--lease-max-per-pod` | `16` | leases of the containers of one pod; a request beyond it is refused |
| `--lease-max-per-node` | `32` | leases of the node; a request beyond it is refused |
| `--lease-requests-per-minute` | `32` | lease requests of one lease claim per minute, also their burst; a request beyond it is answered `too many requests` |
| `--new-state` | `false` | start with a new `state.json` when neither `state.json` nor `state.json.prev` can be read ([An unreadable state](#an-unreadable-state)) |
| `--forget-provenance` | (none) | serial (`0x<hex>` or decimal) whose remembered provenance is removed at start, so that a memdev of it is new again; repeatable, or a comma-separated list ([Forgetting a serial](#forgetting-a-serial)) |

`-v` and the other logging flags of klog are accepted too;
`nri-dra-driver-cxl -help` lists them all. The driver stops on SIGTERM
and SIGINT, also while it starts; a second one during the stop kills it
at once.

## Configuration file

`--config FILE` (the `config` value of the Helm chart), JSON or YAML;
unknown keys are refused. All keys are optional:

```yaml
ignoreMemdevs: ["mem0", "0xc100e2e0"]
sharedMemdevs: ["0xc100e2e2"]
manageMemdevs: true
memdevSettings:
- memdev: "0xc1ffe2ef"
  usableSize: 128Gi
- memdev: "0xc100f5e2"
  coherent: true
fsTypes: [erofs, ext4]          # default [erofs]
verifyAddressing: true
defaults:
  container:
    steering: {policy: none}    # none (default) | auto | first-dram | ...
    fencing: allowed-mems       # allowed-mems (default) | none
```

| key | type | default | |
|:----|:-----|:--------|:-|
| `ignoreMemdevs` | list of memdevs | | memdevs to leave alone |
| `sharedMemdevs` | list of memdevs | | local memdevs to publish as shared: their claims and leases map the whole memdev ([local.md](local.md#shared-local-memdevs)) |
| `ignoreRegions` | list of strings | | regions to leave alone (`region0`) |
| `ignoreNUMANodes` | list of integers | | NUMA nodes to leave alone |
| `ignoreDRAM` | bool | `false` | publish no `dram` device |
| `manageMemdevs` | bool | `true` | publish the local memdevs that have no region and create their regions for claims ([local.md](local.md#what-the-driver-publishes)) |
| `memdevSettings` | list | | settings of single local memdevs: `memdev`; `usableSize` (quantity): only the first `usableSize` bytes of a memdev of RAM are published and mapped ([local.md](local.md#broken-and-aliased-memory)); `coherent` (bool, default `false`): the memory is coherent between the hosts that share it, so that a cluster filesystem may be read-write on it ([persistent.md](persistent.md#several-nodes)) |
| `fsTypes` | list of strings | `[erofs]` | the filesystem types that mode `fs` mounts on the node, for both drivers; a type added to the list trusts every party that can write the devices of the node raw ([persistent.md](persistent.md#filesystems)). `[]` mounts nothing |
| `verifyAddressing` | bool | `true` | run an address line test on every new region of a local memdev of RAM, and use only the distinct bytes of a memdev whose memory aliases ([local.md](local.md#broken-and-aliased-memory)) |
| `defaults.container.steering` | object | `{policy: none}` | the steering of every container of the node whose results do not set it ([Steering](#steering)) |
| `defaults.container.fencing` | string | `allowed-mems` | `allowed-mems` or `none` ([Containers without claims](#containers-without-claims)) |

A memdev is named by `mem<N>`, `cxl/mem<N>` or its serial (decimal or
`0x` hex, any case), and in `ignoreMemdevs` and `sharedMemdevs` also by
`<major>:<minor>` (`251:0`). The serial is the form that does not change
across reboots. `defaults` has the `container` section of a
[MemoryConfig](#fields) only, decoded strictly and case-sensitively; the
results of a container override it field by field
([Container settings](#container-settings)).

The configuration is read at start: a change takes effect when the
driver restarts. An invalid file stops the driver with the key in the
error, for example
`defaults.container.fencing: invalid value "X" (allowed-mems or none)`
or `sharedMemdevs: invalid memdev "X": not a name (mem0, cxl/mem0), major:minor (251:0) or serial in decimal or 0x hex`.
At start the driver logs the filesystem types of the node:

```
fsTypes: erofs, ext4: the filesystem types that mode fs mounts on this node
```

## Files on the node

`D` is `/var/lib/kubelet/plugins/cxl.nri.io`, the data directory of the
driver, and `P` that of the pool driver,
`/var/lib/kubelet/plugins/cxl-pool.nri.io`. Only the driver writes
these files: do not edit them, and do not copy them to another node.
[design.md](design.md#files-on-the-node) tells how it writes them.

| file | |
|:-----|:-|
| `D/state.json` | the records of both drivers: the prepared claims and the memory they got, the memdevs and regions of the driver, the provenance of serials, the lease claims and leases, the steered containers |
| `D/state.json.prev` | the document before the last write; read when `state.json` cannot be |
| `D/state.json.tmp`, `D/state.json.prev.tmp` | present only while the document is being written |
| `D/state.json.bad` | a `state.json` that could not be read, moved aside when the driver started from `state.json.prev` |
| `D/state.json.lost`, `D/state.json.prev.lost` | the documents that could not be read, moved aside by `--new-state` |
| `D/state.lock` | locked by the running driver, with its holder (`pid <pid> host <host>`) |
| `D/localProbes.json` | the usable size of every local memdev whose memory aliases ([local.md](local.md#broken-and-aliased-memory)); kept across restarts, reboots and `--new-state`. Delete it, with the driver stopped, to probe every memdev again |
| `D/leases/<claim uid>/cxl.sock` | the lease socket of a lease claim; the directory is the container's `/run/cxl` |
| `D/lease-fs/<claim uid>/<name>` | the bind of the node mount of an `fs` lease; the directory of the claim is the container's `/run/cxl/fs` |
| `D/cxl-request` | the lease CLI that the driver of the chart copies from its image |
| `D/mounts/<serial hex>-<access>`, `P/mounts/<serial hex>-<access>` | the node mounts of mode `fs` ([persistent.md](persistent.md#modes)) |
| `<cdi root>/cxl-claims.json`, `<cdi root>/cxl-pool-claims.json` | the CDI devices of the prepared claims of each driver, and of the lease claims |

`state.json` is replaced, never written in place: read it to see the
records, also while the driver runs. [states.md](states.md) lists its
records and their states.

## Operations

### Logs

```bash
kubectl logs -n kube-system -l app.kubernetes.io/name=nri-dra-driver-cxl \
    --field-selector spec.nodeName=NODE
```

The chart's `logLevel` (`-v`) is 2. The diagnostics of the driver are
at 3 to 5: the management of every container at 3, the memdevs that it
does not publish at 4.

### A restart of the runtime

The driver exits with status 1 when the container runtime closes its NRI
connection (when containerd restarts, for example), and connects again
when it is restarted: the DaemonSet restarts its container. It exits the
same way when a gRPC server of its kubelet plugins fails. A driver that
cannot reach NRI at start runs on without it: claims are prepared,
containers are neither steered nor kept off the CXL NUMA nodes of the
driver, and lease requests fail with
`lending unavailable: NRI not connected`. The driver connects to NRI
before it registers its kubelet plugins, and a Prepare waits until NRI
has synchronized (at most 30 s, then it fails and kubelet retries).

### A rolling update

The new pod of the DaemonSet waits up to 2 minutes for the lock of the
old one (`state.lock`), and exits when it does not get it. The new
driver reads the records of the old one: the prepared claims, their
memory and their mounts stay. A `state.json` of another schema is not
read ([An unreadable state](#an-unreadable-state)).

### Forgetting a serial

The driver tells pool devices from local memdevs by their serials. It
remembers the serials of pool devices across restarts and reboots, so a
pool device that stays attached to the node is never published as a
local memdev ([local.md](local.md#what-the-driver-publishes)). To use it
as a local memdev, start the driver once with
`--forget-provenance 0x<serial>`: the start removes the record of the
serial, and a memdev of it is new again. While a ResourceSlice of the
pool driver lists the serial, the driver learns it as a pool device
again. The flags of the files are
no values of the chart: add one to the `args` of the container of the
DaemonSet (`kubectl -n kube-system edit daemonset nri-dra-driver-cxl`),
and remove it again when the driver has started. The DaemonSet passes it
on every node; `--forget-provenance` acts on the nodes that remember the
serial, on them at every start while it is given.

### An unreadable state

A `state.json` that the driver cannot read, because it was written with
another schema or it is damaged, is replaced by `state.json.prev` when
that one can be read. When neither can be read, the driver starts LOST:
it logs

```
store: the driver state is lost: state.json: <error>; state.json.prev: <error>; start the driver with --new-state to create a new one
```

A LOST driver does not crash or restart. It writes nothing, releases
nothing and recovers nothing at start. It fails every Prepare with that
error and keeps the CDI specs. It fences every NUMA node of online CXL
memory. Its lease broker has no leases and deletes no companion. The
remembered provenance is lost, so it holds every new memdev for up to 5
minutes in its first hour. Drain the claims of the node and start the driver once with
`--new-state`: it moves both files aside (`.lost`) and starts with a new
document, and what the lost records named is foreign to it.

### Tools on the node

Do not read the mapping files of dax devices
(`/sys/bus/dax/devices/*/mapping*`, `daxctl list -M`) on a node where
the driver runs. A reader of `mappingN/start` or `end` while the driver
resizes or deletes a dax device can deadlock with it in the kernel, and
then every later CXL operation of the node waits.
