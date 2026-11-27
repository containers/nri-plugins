# CXL memory pools

CAUTION: experimental. Interfaces may change.

A CXL memory pool (a CXL switch with memdevs behind it, managed by a
fabric manager) can hot-add a memdev to a host and hot-remove it again.
With Dynamic Resource Allocation such devices are allocated to pods like
any other device: the scheduler picks a device and a node, a *pool
controller* attaches the device to that node, and the pool driver
`cxl-pool.nri.io` of `nri-dra-driver-cxl` makes the memory usable for
the pod.

This page has two parts: how a user claims pool devices, and the
contract between a pool controller and the driver. The controller works
at the cluster level, one per pool: it publishes the pool devices,
attaches and detaches them, and reports the result in the claim status.
It talks to the management interface of the pool, so it is specific to
the pool product; this repository has none for a product. The driver
works on the node and needs only what the controller writes into the
claim status. The fake pool controller, `fake-cxl-pool-controller`,
implements the contract for a fake pool of qemu VMs; the e2e tests use
it.

```
 pool controller
   publishes a ResourceSlice of pool devices (driver cxl-pool.nri.io),
   attaches a device to the node that the scheduler picked, with the
   settings of the pool section of the claim's MemoryConfig,
   writes claim.status.devices[] (condition Attached=True + data),
   detaches the device after the claim is deallocated.
 kube-scheduler
   allocates a pool device, pins the claim to a node (bindsToNode),
   waits for the binding condition, binds the pod.
 nri-dra-driver-cxl, driver cxl-pool.nri.io
   waits for the memdev with the serial from the claim status, creates a
   region on it as the device section says, returns CDI edits;
   at Unprepare releases the memdev, so that the pool can hot-remove it.
```

## Using pool devices

Three DeviceClasses offer pool devices ([index.md](index.md#deviceclasses)):

- `cxl-pool`: unshared devices, attached to one node at a time. A claim
  gets the whole device: a new NUMA node (mode `numa`, the default) or a
  dax device (`mode: dax`). A capacity request picks a device with at
  least that much memory.
- `cxl-pool-shared`: shared devices of volatile memory, attached to
  several nodes at once, all of them seeing the same bytes. A claim gets
  a dax device: alone without a capacity request, or one share with
  `capacity: {requests: {shares: "1"}}`.
- `cxl-pool-persistent`: devices of persistent memory, in modes `block`
  and `fs` ([persistent.md](persistent.md)).

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: {name: pool-memory}
spec:
  devices:
    requests:
    - name: mem
      exactly:
        deviceClassName: cxl-pool
        capacity: {requests: {memory: 512Mi}}     # a device with at least this much
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: {name: shared-memory}
spec:
  devices:
    requests:
    - name: mem
      exactly:
        deviceClassName: cxl-pool-shared
        selectors:
        - cel: {expression: 'device.attributes["cxl-pool.nri.io"].serial == "0xc1ae0001"'}
        capacity: {requests: {shares: "1"}}       # one share; without it the device alone
```

A `MemoryConfig` of driver `cxl-pool.nri.io` says how the device is
prepared ([usage.md](usage.md#memoryconfig)). Its `device` and
`container` sections are those of local memory; its `pool` section
holds the settings of the pool controller
([Configuration of a pool controller](#configuration-of-a-pool-controller)):

| pool device | claim | the container gets |
|:------------|:------|:-------------------|
| unshared | (nothing) | a NUMA node |
| unshared | `device: {mode: dax}` | a dax device of its own |
| shared | `capacity: {requests: {shares: "1"}}` | a dax device, shared with the other claims of the device |
| shared | (nothing) | a dax device; no other claim has the device meanwhile |
| dax, shared or not | `device: {readOnly: true}` | the dax device, read-only |

In mode `numa` the memory blocks of the device are onlined movable as a
new NUMA node. A container that opts in is steered to it like to local
CXL memory, with the device size as its quota
([usage.md](usage.md#steering)). Any other container that uses the
device gets the NUMA node in its `cpuset.mems`. The containers without
the claim do not get it ([usage.md](usage.md#containers-without-claims)).
In mode `dax` the memory is never onlined: two kernels that use the same
bytes as RAM would corrupt each other. Applications `mmap` the dax
device (`MAP_SHARED`, aligned to 2 MiB) and agree on the layout of the
memory themselves. Nothing clears a pool device between claims: a claim
reads what the previous one wrote, maybe on another node.

The samples
[pool-numa.yaml](blob:/deployment/samples/dra-driver-cxl/pool-numa.yaml),
[pool-dax.yaml](blob:/deployment/samples/dra-driver-cxl/pool-dax.yaml),
[pool-shared-writer-readers.yaml](blob:/deployment/samples/dra-driver-cxl/pool-shared-writer-readers.yaml)
and
[local-then-pool-dax.yaml](blob:/deployment/samples/dra-driver-cxl/local-then-pool-dax.yaml)
(local memory if a local memdev has room, else pool memory) are
complete; [usage.md](usage.md#example-claims) lists them all.

### Single-writer sharing

The writer claims the shared device without a capacity request, so it
has the device alone, and writes. The readers claim one share each with
`device: {readOnly: true}`. A reader that asks while the writer has the
device waits, and its pod or lease stays pending. It waits until the
writer's claim is deallocated: the writer's pod has finished or is
deleted, or its lease is released.
[pool-shared-writer-readers.yaml](blob:/deployment/samples/dra-driver-cxl/pool-shared-writer-readers.yaml)
has a writer and its readers.

Read-only is a property of the claim, not of the attachment: whatever a
host attaches, it can write. `readOnly` keeps readers from writing by
mistake; the consistency of the data is the job of the writer having the
device alone. A filesystem of a persistent device is shared the same
way, read-only on any number of hosts; a read-write one is ReadWriteOnce
([persistent.md](persistent.md#several-nodes)).

### Leases

A running container leases a pool device without a claim of its own:
`cxl-request request --source pool --size 512Mi` in a container with a
claim on `cxl-lease` makes the driver allocate a device for a companion
pod and lend it to the container, as a NUMA node or as an open file
descriptor ([leases.md](leases.md)).

### Force deletion

`kubectl delete pod --force --grace-period=0` removes the pod object at
once. The claim may then be deallocated and the device detached while
the node still uses the memory: the host cannot complete the hot-remove,
and the device stays half removed until the host reboots. It breaks
ReadWriteOnce too: another node may mount the filesystem while the first
one still has it read-write. Do not force-delete pods that use pool
memory. The claims of a node that is lost can be deallocated the same
way. In normal operation kubelet unprepares the claims of a pod after
its containers stopped and before it reports the pod terminal, and only
then is the claim deallocated and the device detached.

## The pool controller contract

### What the pool controller publishes

Cluster-scoped ResourceSlices (no node name) of the pool under driver
`cxl-pool.nri.io`, at most 128 devices in a slice. A cluster that spans
several pools has one controller per pool, each publishing its own pool
name under the same driver. A controller confines itself to its pool
(resourceslice `Options.ReconcilePoolWithName`: without it, a controller
removes the slices of pools it does not know); the fake pool controller
names the pool after its server.

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
spec:
  driver: cxl-pool.nri.io
  pool: {name: fake-cxl-pool, generation: N, resourceSliceCount: 1}
  # the nodes the pool can reach; allNodes: true if every node can
  nodeSelector:
    nodeSelectorTerms:
    - matchExpressions:
      - {key: kubernetes.io/hostname, operator: In, values: [node1, node2]}
  devices:
  - name: pooled0                # device name, a DNS label
    attributes:
      type:   {string: memdev}
      source: {string: pool}
      serial: {string: "0xc1ee0001"}       # /sys/bus/cxl/devices/memN/serial, lower-case hex
      shared: {bool: false}
      persistent: {bool: false}
      coherent: {bool: true}               # absent is false
      size:   {int: 536870912}             # bytes
    capacity:
      memory: {value: 512Mi}               # unshared devices only
    bindsToNode: true
    bindingConditions: ["cxl-pool.nri.io/Attached"]
    bindingFailureConditions: ["cxl-pool.nri.io/AttachFailed"]
  - name: shared0
    attributes:
      type:   {string: memdev}
      source: {string: pool}
      serial: {string: "0xc1ae0001"}
      shared: {bool: true}
      persistent: {bool: false}
      coherent: {bool: true}
      size:   {int: 268435456}
    allowMultipleAllocations: true
    capacity:
      shares: {value: 1M}                  # no requestPolicy
    bindsToNode: true
    bindingConditions: ["cxl-pool.nri.io/Attached"]
    bindingFailureConditions: ["cxl-pool.nri.io/AttachFailed"]
```

| attribute | value | |
|:----------|:------|:-|
| `type` | `memdev` | required |
| `source` | `pool` | required |
| `serial` | `0x` and lower-case hex | required: `/sys/bus/cxl/devices/memN/serial` of the memdev on the hosts |
| `shared` | bool | required: whether several hosts may have the device attached at a time |
| `persistent` | bool | required: whether the device is persistent memory; the DeviceClasses test it, and a selector that reads an attribute that a device lacks fails |
| `size` | bytes | required |
| `coherent` | bool | optional, absent is `false`: whether the hardware keeps the memory coherent between the hosts that have it attached at a time; a cluster filesystem is read-write on several hosts only on such a device |

These are the attributes of the local memdevs of `cxl.nri.io`
([usage.md](usage.md#devices-of-cxlnriio)), plus `coherent`, so that
selectors and environment variables are the same for both sources. The
lease broker finds a device by `serial` and reads its `shared`, and
`cxl.nri.io` never publishes a memdev whose serial a pool device has. A
controller may publish more attributes under a domain of its own (for
example `pool.example.com/model`); the fake pool controller publishes
none, and its devices are all `coherent` (they are memory of the qemu
host that every VM maps).

- The size of an unshared device is its `memory` capacity, so claims can
  request an amount.
- The size of a shared device is never a capacity: every allocation
  would consume it. Its capacity is `shares` without a `requestPolicy`,
  with the value 1M (`1000000`) when the pool has no limit of its own; a
  controller for devices with a head limit publishes the number of
  heads. A claim without a `shares` request consumes the whole value and
  has the device alone: the scheduler allocates the device to no other
  claim until it is deallocated. A claim with `shares: "1"` is the share
  of one host.
- `bindsToNode` makes the scheduler pin the claim to the node it picked.
  One claim covers one node, so N nodes that share a device need N
  claims.
- A persistent device keeps its data and its namespace labels while no
  host has it attached. Every host that attaches it must see the same
  label storage area (LSA) as well as the same data: a host with a label
  storage of its own sees no namespace, or another one. The fake pool
  gives a persistent device a data file and an LSA file, both shared by
  every qemu that attaches it.

### What the scheduler does

Nothing to configure. The scheduler tries pools without binding
conditions first, so local devices of `cxl.nri.io` win when they satisfy
a request. The devices of a pool are considered on the nodes of its
`nodeSelector` only. The scheduler writes `status.allocation` (results
with driver, pool, device and the binding conditions; `shareID` and
`consumedCapacity` for shared devices) with the picked node as its
`nodeSelector`, emits the pod event `BindingConditionsPending`, and
waits for the binding condition until its binding timeout. When a
failure condition becomes True or the timeout expires, the claim is
deallocated and allocated again.

### What the pool controller writes

After the pool has attached the device to the node, the controller adds
an entry to `claim.status.devices` (`UpdateStatus` on the `/status`
subresource):

```yaml
status:
  devices:
  - driver: cxl-pool.nri.io
    pool: fake-cxl-pool
    device: shared0
    shareID: <from the allocation result, shared devices only>
    conditions:
    - type: cxl-pool.nri.io/Attached
      status: "True"
      reason: Attached
      message: "shared0 attached to node1"
      lastTransitionTime: <now>
    data:
      serial: "0xc1ae0001"       # required: string, 0x hex or decimal
      shared: true               # required
      size: 268435456            # required, bytes
      exclusive: true            # only when the allocation consumed all shares
      persistent: true           # only for a device of persistent memory
      singleHost: true           # only for a claim that needs the attachment on its host alone
      coherent: true             # only for a coherent device
      # anything else is informational; the fake pool controller adds:
      host: node1
      hostUUID: <the host's uuid>
      attachment: shared0@node1
      qemuDeviceID: <qemu device id>
```

The driver matches the entry by driver, pool and device, and by
`shareID` for a result that has one. It reads `serial`, `shared`,
`size`, `exclusive`, `persistent`, `singleHost` and `coherent` from
`data`; a missing `exclusive`, `persistent`, `singleHost` or `coherent`
is `false`:

| data | the driver |
|:-----|:-----------|
| `exclusive` | the claim has the device alone: a `block` claim of a shared persistent device may write |
| `persistent` | modes `block` and `fs` |
| `singleHost` | no other host has the device while the claim is allocated: an `fs` claim may mount it read-write |
| `coherent` | a cluster filesystem may be read-write on several hosts |

The driver takes a device that is not shared, or a claim with
`exclusive`, as single-host too. Without `singleHost`, `exclusive` or a
cluster filesystem on a `coherent` device, a read-write `fs` claim of a
shared device fails its Prepare: a controller that does not know the
rule makes writes fail, never corrupts the data.

On failure the controller sets `cxl-pool.nri.io/AttachFailed` to True
instead, and detaches what was half attached for the result. The fake
pool controller uses the reasons `NoHost`, `AttachError`, `Timeout` and
`Conflict`, and every controller uses `InvalidConfig` for an invalid
configuration (below). A result whose status entry is `Attached` already
keeps its attachment, also when its configuration is found invalid (the
device may be in use); otherwise a controller detaches a device only
after its claim has been deallocated.

RBAC of a controller:

```yaml
rules:
- {apiGroups: [resource.k8s.io], resources: [resourceslices], verbs: [list, watch, create, update, delete]}
- {apiGroups: [resource.k8s.io], resources: [resourceclaims], verbs: [get, list, watch]}
- {apiGroups: [resource.k8s.io], resources: [resourceclaims/status], verbs: [update]}
- {apiGroups: [resource.k8s.io], resources: [resourceclaims/driver], verbs: ["arbitrary-node:update"], resourceNames: ["cxl-pool.nri.io"]}
- {apiGroups: [""], resources: [nodes], verbs: [list, watch]}
- {apiGroups: [""], resources: [events], verbs: [create, patch]}
```

### Configuration of a pool controller

A claim configures its pool controller in the `pool` section of the
`MemoryConfig` entries of the pool driver.

**Where.** The configuration of an allocation result is in
`claim.status.allocation.devices.config`: the entries with
`opaque.driver` = `cxl-pool.nri.io`, copied there by the scheduler from
the DeviceClass (`source: FromClass`) and the claim
(`source: FromClaim`). Entries of other drivers are not the
controller's.

**Resolution.** For each allocation result of its pool, the controller
takes the entries of the driver whose `requests` are empty, list the
result's request, or list the main request `r` of a subrequest `r/s`;
class entries first, then claim entries, each in list order. Each entry
is decoded strictly (kind `MemoryConfig`, apiVersion
`cxl.nri.io/v1alpha1`, no unknown fields), and the parameters merge as a
JSON merge patch. This is the rule of the driver,
[usage.md](usage.md#merge-order).

**The controller key.** `pool.opaque` is a map from controller key to an
object that the controller with that key defines:

```yaml
pool:
  opaque:
    fake-cxl-pool: {numaNode: 1}       # the fake pool controller
    example.com/appliance: {port: 3}   # another implementation
```

A controller has one key (a setting of its own, unique per
implementation; all controllers of one implementation share it), reads
only the object under it, and ignores the other keys: a claim may carry
settings for several implementations. The objects merge per key across
the class and claim entries, so a class may give defaults that a claim
changes field by field. A controller versions its object itself if it
needs a version (a field in it); the outer apiVersion stays
`cxl.nri.io/v1alpha1`. The driver refuses unknown fields next to
`opaque` and never looks into the objects.

**Not yours.** The `device` and `container` sections belong to the
driver. A controller does not interpret them, except for the one
question that only the controller can enforce, which it asks through
`config.NeedsSingleHost`.

**ReadWriteOnce.** A claim that mounts a single-host filesystem
read-write (mode `fs`, `readOnly: false`, a type that is not a cluster
filesystem on a coherent device) must have the device on its node only,
because another host that reads or writes the filesystem meanwhile
corrupts or misreads it ([persistent.md](persistent.md#several-nodes)).
The node cannot see other hosts; the controller can:

- For each allocation result of a shared device, the attachment that
  the result wants is single-host when the result consumed all shares,
  or when `config.NeedsSingleHost(res, coherent)` is true for its
  resolved `MemoryConfig` and the `coherent` attribute of its device. A
  device that is not shared is attached to one host anyway. An
  attachment of a device to a host serves every claim of the host. It
  is single-host when any of them wants it so: then no other host
  gets the device, also not from another cluster that shares the pool,
  until no claim of the host wants it any more.
- Write `singleHost: true` into the status data of every result that
  wants the attachment single-host, once it is, and `coherent: true` for
  a coherent device. The readers of the writer's node share the
  attachment without `singleHost`.
- A claim of another host while the attachment is single-host, and a
  single-host one while another host has the device, gets
  `AttachFailed` with reason `Conflict`, and the scheduler allocates it
  again until the device is free.
- Fail only the claims that want the attachment single-host. When an
  existing attachment cannot be made single-host (another host has the
  device), only those get `AttachFailed`; the other claims of the host
  keep, or get, `Attached`. When a new single-host attachment is
  refused, attach the device again without the flag for the results
  that do not need it. A new attachment that fails otherwise fails every
  result that wanted it.
- Release the single host only when the writing claims are deallocated:
  their Unprepare syncs the filesystem and remounts it read-only or
  unmounts it before that, so that no other host sees the device before
  the data is in it. Fail closed: a writer that is attached already and
  whose configuration can no longer be resolved keeps its attachment
  single-host; and while the attach without the flag gets no answer,
  only the results that want the single host get `AttachFailed`, the
  others wait.

**Errors.** An invalid `MemoryConfig` entry of the driver (also one of
another request of the claim) or an invalid object under the
controller's key attaches nothing. The status entry of the result gets
`cxl-pool.nri.io/AttachFailed=True` with reason `InvalidConfig` and the
error as the message; the scheduler deallocates and allocates again
until its binding timeout. Leave no attachment behind for the result,
and give the same answer on every reconcile: the allocation, and so its
configuration, does not change. Because the condition goes away with
the deallocation, an event on the claim with the same message helps
users.

**Go.** Controllers in Go call [`pkg/cxl/config`](tree:/pkg/cxl/config):

```go
import "github.com/containers/nri-plugins/pkg/cxl/config"

type Settings struct { // the controller's object, decoded strictly
	Slot     string `json:"slot,omitempty"`
	NUMANode *int   `json:"numaNode,omitempty"`
}

// for each allocation result r of the pool
if err := config.CheckEntries(claim, driver); err != nil { ... } // every entry of the driver
res, err := config.Resolve(claim, driver, r.Request)               // the merged MemoryConfig of the result
var s Settings
found, err := config.PoolOpaque(res, key, &s)                     // pool.opaque.<key>, other keys ignored
// any err: AttachFailed=True, reason InvalidConfig, message err.Error()
singleHost := allSharesConsumed(r) || (deviceShared && config.NeedsSingleHost(res.Config, deviceCoherent))
```

The error texts start with `MemoryConfig: `, for example
`MemoryConfig: FromClaim config 1: json: unknown field "x"` or
`MemoryConfig: pool.opaque.fake-cxl-pool: json: unknown field "port"`.
`Resolve` also validates the values of the `container` section; it
checks no device fields. `NeedsSingleHost` is true for mode `fs` with
`readOnly: false`, unless `fsType` is a cluster filesystem (gfs2) and
the device is coherent.

**The fake pool controller.** `fake-cxl-pool-controller` has the key
`fake-cxl-pool` (flag `-controller-key`) and the object:

| field | type | |
|:--|:--|:--|
| `slot` | string | an exact CXL slot (bus name) of the host, as the pool server lists them; the server refuses another one (`AttachError`) and an occupied one (`Conflict`) |
| `numaNode` | integer >= 0 | prefer the free slots of host bridges on that NUMA node of the host |

A claim that wants its pool memory near NUMA node 1 of the host:

```yaml
config:
- requests: [mem]
  opaque:
    driver: cxl-pool.nri.io
    parameters:
      apiVersion: cxl.nri.io/v1alpha1
      kind: MemoryConfig
      pool:
        opaque:
          fake-cxl-pool: {numaNode: 1}
```

The settings place a new attachment only. An attachment that exists
already stays in its slot, because the device may be in use: a shared
device attached for another claim of the node, or one found after a
restart. The `Attached` message names the slot.

### What the driver does

The driver registers `cxl-pool.nri.io` with kubelet (socket
`/var/lib/kubelet/plugins/cxl-pool.nri.io/dra.sock`) and never publishes
ResourceSlices. `--pool=false` (Helm value `pool.enabled`) disables it;
`--pool-prepare-timeout` (`pool.prepareTimeout`) is how long a Prepare
waits for the memdev to appear.

At Prepare it reads the claim status entry; a missing entry or data
fails the Prepare, and kubelet retries. Then it resolves the
`MemoryConfig` of the result, waits for a memdev with the serial, and
creates a region on it in the mode of the claim. The CDI device of the result has the variables of
[usage.md](usage.md#environment-variables). A claim that wants a device
in another mode than another claim on the node prepared it fails. When
one device of a claim fails, the devices prepared for it are released
again.

The Unprepare of the last claim of a device on the node releases the
device, so that the pool can hot-remove it. It offlines the memory (mode
`numa`), waits until nothing holds the block device of a persistent
device, destroys the region and disables the memdev. A device that is gone
already is not an error. [design.md](design.md#a-pool-device) has the
steps, and [design.md](design.md#how-a-restart-recovers) what a restart
and a reboot do with them.

### Ordering

- Attach before bind: the controller sets `Attached` only after the pool
  confirmed the attachment. The Prepare then normally finds the memdev
  present already, or waits for it briefly.
- Release before detach: kubelet calls the Unprepare while it terminates
  the pod, before the pod object is gone; the claim is deallocated only
  after that, and the controller detaches only deallocated devices.
- A claim of an unshared device while all of them are taken stays
  pending in the scheduler; it is not a failure.
- A restart of the controller recomputes the attachments from the claims
  and the pool. A graceful stop deletes its ResourceSlices; the driver
  still takes the attached devices as pool devices by the serials that
  it remembers ([local.md](local.md#what-the-driver-publishes)).
