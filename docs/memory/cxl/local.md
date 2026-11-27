# Local CXL memory

CAUTION: experimental. Interfaces may change.

A local memdev, attached to the node at boot or by hotplug, holds no
usable memory until a region is created on it. `cxl.nri.io` publishes
such memdevs in the ResourceSlice of the node and creates and destroys
their regions on demand. Every claim gets a slice of a memdev, as RAM of
a NUMA node or as a dax device, and slices of both modes share a memdev.
A memdev in `sharedMemdevs` is not sliced: every claim maps all of it
([Shared local memdevs](#shared-local-memdevs)). A memdev of persistent
memory gives its claims its block device or a filesystem
([Persistent local memdevs](#persistent-local-memdevs)). A running
container leases local memory with
`cxl-request request --source local --size 64Mi` ([leases.md](leases.md)).

Regions that exist already (created by the BIOS or an admin, online as
RAM) are published as `region` devices
([usage.md](usage.md#devices-of-cxlnriio)); this page is about the
memdevs that the driver manages itself.

## What the driver publishes

A memdev is published as a device of type `memdev` when

- it is enabled (bound to `cxl_mem`; the kernel enables a hotplugged
  memdev, and the driver never disables a local one);
- it has RAM capacity, or only persistent capacity
  ([Persistent local memdevs](#persistent-local-memdevs));
- it is not a pool device (below) and not in `ignoreMemdevs`;
- it has no region that the driver did not create.

A memdev with a region of someone else is left alone: its RAM region
with online memory is a `region` device, and any other region of it is
not published. Destroy the region to let the driver manage the memdev.
`manageMemdevs: false` of the [configuration file](usage.md#configuration-file)
turns all of this off: no memdev is published and no region is created.
A memdev that is not published is logged at verbosity 4 with the
reason.

Local memdevs and pool devices are told apart by serial. A memdev is a
pool device when a ResourceSlice of `cxl-pool.nri.io` lists its serial
or the pool driver prepares it. A pool controller publishes a device
before it attaches it to a node, so a pool device is never offered as a
local one. The driver remembers the serials of pool devices in its
state, also across restarts and reboots and while a pool controller is
stopped (it deletes its ResourceSlices then, while its devices stay
attached): such a memdev is never published as a local memdev. A new
memdev whose source is uncertain, for instance while a remembered pool
has no ResourceSlices, is held for up to 5 minutes before it is
published (`provenance: holding new memdev ...`).
[design.md](design.md#provenance) has the rules.

A pool device that stays attached stays a pool device. To use a former
pool device as a local memdev, start the driver once with
`--forget-provenance 0x<serial>`
([usage.md](usage.md#forgetting-a-serial)).

If the remembered serials are lost (a state that cannot be read,
`--new-state`) while a pool controller is stopped, an unprepared pool
device on the node may be taken as local. Shared pool memory must never
be onlined as RAM: keep the state of the driver on the node.

The published device is in [usage.md](usage.md#devices-of-cxlnriio).
Its `memory` capacity is what whole memory blocks of its region are sure
to hold:

- Every slice, `numa` or `dax`, is a multiple of the memory block size
  of the node (`/sys/devices/system/memory/block_size_bytes`: 128 MiB in
  the e2e VMs, up to 2 GiB on servers with much RAM) and lies on whole
  memory blocks. The attribute `memoryBlockSize` says the size.
- The capacity is the usable RAM capacity rounded down to whole blocks.
  With blocks larger than 256 MiB (the alignment of CXL regions in their
  window) a region can start up to block size - 256 MiB after a block
  boundary, so that head is left out first.
- The request policy makes the scheduler account what the driver
  carves: a request is rounded up to the block size, and a request
  without an amount gets the whole capacity. A memdev without room for
  one whole block is not published.

## Slices

The DeviceClass `cxl-local` offers the local memdevs that are neither
shared nor persistent. Every claim gets a slice of its own, in one of
two modes:

- `numa` (default): the slice is onlined as system RAM in movable memory
  blocks of the NUMA node of the memdev. All `numa` slices of a memdev,
  and of the memdevs under one CXL host bridge, are on the same NUMA
  node: containers are isolated by NUMA node (`cpuset.mems`), not per
  slice.
- `dax`: the slice is a dax device `/dev/daxX.Y` of its own, zeroed
  before the claim gets it. A read-only slice has a device node that
  allows reads only. Applications `mmap` it (`MAP_SHARED`, aligned to
  2 MiB) and agree on the layout of the memory themselves.

The NUMA node grows by every block of a `numa` slice that is prepared,
and shrinks when the slice is released: it has no memory when the last
`numa` slice is gone. `dax` slices add nothing to it. Capacity that no
claim has stays offline. The containers whose claims or leases are on
the NUMA node share it; `memory.max` bounds what each uses. Steering
does not: past its last waypoint a steered container allocates beyond
its claims ([usage.md](usage.md#steering)).

| the claim gets | claim on `cxl-local` |
|:---------------|:---------------------|
| 128Mi as RAM | `exactly: {deviceClassName: cxl-local, capacity: {requests: {memory: 128Mi}}}` |
| a 128Mi dax slice | the same and `device: {mode: dax}` |
| a read-only dax slice | `device: {mode: dax, readOnly: true}` |
| RAM, steered | `container: {steering: {policy: auto}}` |
| a specific memdev | `selectors: [{cel: {expression: 'device.attributes["cxl.nri.io"].serial == "0xc100e2e1"'}}]` |
| the whole memdev | no capacity request |
| one slice for several pods | one ResourceClaim (not a template) that the pods reference |

A request is rounded up to the memory block size: 64Mi gets a 128Mi
slice with 128 MiB blocks. Pods that reference one ResourceClaim land on
the node of its memdev, and kubelet prepares the claim once, so all of
them get the same slice. The samples
[local-numa.yaml](blob:/deployment/samples/dra-driver-cxl/local-numa.yaml),
[local-dax-readonly.yaml](blob:/deployment/samples/dra-driver-cxl/local-dax-readonly.yaml)
and
[local-dax-shared-claim.yaml](blob:/deployment/samples/dra-driver-cxl/local-dax-shared-claim.yaml)
(two pods, one claim) are complete.

The first claim of a memdev creates a region on it, and its last claim
destroys the region; the memdev stays enabled, back in its initial
state. Unprepare offlines the memory blocks of a `numa` slice: when
pages cannot move, the Unprepare fails and kubelet retries. After a
restart of the driver the prepared slices are found again; a reboot
leaves no region, and kubelet's Prepare after it gets a new slice
([design.md](design.md#how-a-restart-recovers)).

The driver logs the steps of a slice at verbosity 0:

```
engine: local memdev 0xc100e2e1 (mem1): created region0 (devdax, dax dax0.0, NUMA node 2) in <t>
engine: local memdev 0xc100e2e1 (mem1): claim <uid>: created slice dax0.1 mode numa size 134217728 at [0x258000000-0x25fffffff] in <t>
engine: local memdev 0xc100e2e1 (mem1): claim <uid>: onlined slice dax0.1 mode numa size 134217728 blocks [75] NUMA node 2 in <t>
engine: local memdev 0xc100e2e1 (mem1): claim <uid>: deleted slice dax0.1 mode numa size 134217728 in <t>
engine: local memdev 0xc100e2e1 (mem1): destroyed region0 in <t>
```

## Shared local memdevs

A local memdev in `sharedMemdevs` is not sliced. It is one dax device
of the whole memdev that every claim and lease of the memdev maps: the
same bytes, mode `dax` only. A claim without a `shares` request has the
memdev alone, the writer of single-writer data sharing; a claim with
`shares: "1"` is one of its sharers, usually a reader with
`readOnly: true`. It is the model of shared pool devices on one node.

```yaml
sharedMemdevs: ["0xc100e2e2"]
```

The published device has a `shares` capacity without a request policy,
and no `memory` capacity, no `memoryBlockSize` and no
`nodeAllocatableResources`: nothing of it is onlined
([usage.md](usage.md#devices-of-cxlnriio)). The DeviceClass
`cxl-local-shared` offers it; `cxl-local` never does.

| the claim gets | claim on `cxl-local-shared` |
|:---------------|:----------------------------|
| the memdev alone (the writer) | `exactly: {deviceClassName: cxl-local-shared}` and a `serial` selector, no capacity request |
| one share | the same and `capacity: {requests: {shares: "1"}}` |
| one share, read-only (a reader) | the same and `device: {readOnly: true}` |
| one share for several pods | one ResourceClaim (not a template) that the pods reference |

While a claim has the memdev alone, the scheduler allocates it to no
other claim: the pods of the waiting claims stay Pending.
[local-shared-writer-readers.yaml](blob:/deployment/samples/dra-driver-cxl/local-shared-writer-readers.yaml)
has a writer and three readers. A running container leases the memdev
with `cxl-request request --serial 0xc100e2e2` (alone) or
`--serial 0xc100e2e2 --share --read-only` (one read-only share,
[leases.md](leases.md#cxl-request)). Every claim gets
`CXL_<KEY>_SHARED=1`, the dax device `/dev/daxX.0` of the region and its
size.

The first claim creates the region and zeroes the whole dax device, so
that the first sharer never reads what earlier users of the memdev left
there. Then the region and its bytes stay: when its last claim is gone,
while the driver restarts, until the node reboots or the memdev leaves
`sharedMemdevs`. A claim reads what the last writer left, also after
the memdev was idle. A hot-remove is refused while the region exists:
remove the memdev from `sharedMemdevs` and restart the driver first.

The configuration is read at start; the chart restarts the DaemonSet
when its `config` value changes. A memdev switches between unshared and
shared use only when it is idle: while claims of the other use are
prepared it is not published, and the driver keeps serving those claims.
A memdev that left `sharedMemdevs` loses its region with its last claim
and is published as an unshared memdev again.

## Persistent local memdevs

A local memdev with persistent capacity and no RAM capacity is a device
of persistent memory: its data and namespace labels outlive the claims
and the reboots of the node. The DeviceClass `cxl-local-persistent`
offers it, in modes `block` and `fs`, with the rules of
[persistent.md](persistent.md) that both drivers share. How it differs
from a persistent pool device:

- It is published by `cxl.nri.io` with `persistent: true`, its persistent
  capacity as `size`, and a `shares` capacity: a claim without a
  capacity request has it alone, a claim with `shares: "1"` is a sharer.
  It has no `memory` capacity, no `memoryBlockSize` and no
  `nodeAllocatableResources`. `usableSize` and `verifyAddressing` do not
  apply to it.
- A memdev with both RAM and persistent capacity is published by its
  RAM; its persistent capacity is not used.
- Its facts come from the configuration file, not from a claim status:
  shared when it is in `sharedMemdevs`, single host otherwise, coherent
  with `memdevSettings[].coherent`. A memdev in `sharedMemdevs` is never
  written in mode `block`, and in mode `fs` only by a cluster filesystem
  on coherent memory ([persistent.md](persistent.md#several-nodes)).
- Its node mounts are under `/var/lib/kubelet/plugins/cxl.nri.io/mounts`.
- The last claim of the memdev on the node destroys its region, also of
  a memdev in `sharedMemdevs`. When regions of other memdevs lie above
  the region on the same host bridge, the idle memdev keeps its region,
  and the next claim uses it. The memdev stays enabled.

[local-persistent-fs.yaml](blob:/deployment/samples/dra-driver-cxl/local-persistent-fs.yaml)
has a writer and a reader. A running container leases the memdev with
`cxl-request request --source local --serial 0xc100f5e1 --mode fs`
(`--read-only=false` to write, `--mode block` for its block device).

## Broken and aliased memory

A memdev may decode fewer address lines than its size needs: offsets X
and X+P then reach the same memory, and a slice above P silently
overwrites the slice of another claim below it, in modes `numa` and
`dax` alike. The driver protects against such memdevs in two ways:

- `verifyAddressing` (default `true`): every new region of a memdev of
  RAM gets an address line test before its first slice or shared claim.
  For every power of two P from 4 KiB up to the region size it writes a
  marker at a few offsets X and X+P, bypassing the CPU caches, and reads
  X back. When X holds the marker of X+P, only the first P bytes are
  distinct. Then the driver creates the region again with only those
  bytes and publishes the memdev with that size from the next scan on. It
  keeps the result in `localProbes.json`, also across reboots. A marker
  that reads back as something else fails the claim.

  ```
  engine: local memdev 0x... (mem0): address line probe of region0 (206158430208 bytes): offsets 0x0 and 0x2000000000 reach the same memory (address bit 37 is ignored): only the first 137438953472 bytes are distinct in <t>
  ```

  The claim that triggers the probe was allocated against the capacity
  published before it. If its slice does not fit in the usable size,
  its Prepare fails with "no room for the slice"; create the claim
  again.

- `memdevSettings[].usableSize` limits a memdev by configuration, before
  any claim: regions map only its first `usableSize` bytes, and the
  published capacity is computed from it.

  ```yaml
  memdevSettings:
  - memdev: "0x..."
    usableSize: 128Gi
  ```

## Limitations

- One region per memdev, no interleaving across memdevs.
- `numa` memory is online per claim but not isolated per claim: all
  containers with `numa` claims or leases on a NUMA node share its
  memory, and `memory.max` bounds each. Other containers do not get the
  NUMA node ([usage.md](usage.md#containers-without-claims)).
- Slice sizes are multiples of the memory block size, also of `dax`
  slices: a 64Mi dax claim gets 128Mi with 128 MiB blocks, 2Gi with
  2 GiB blocks. With blocks larger than 256 MiB a memdev publishes one
  block less than its RAM capacity.
- The memory of a NUMA node can be a little less than its `numa` slices
  when the kernel takes the struct pages of hotplugged memory from the
  memory itself (`memory_hotplug.memmap_on_memory`).
- With `DRANodeAllocatableResources`, a dax claim raises the memory
  limit of the container by the slice size, although dax memory is not
  charged to the cgroup.
- A region that an admin creates on a memdev while the driver uses it is
  not published as a `region` device until the driver has released the
  memdev.
- A memdev that is shared with other hosts but not managed by a pool
  controller looks like a local memdev. Onlining it as RAM corrupts the
  data of the other hosts: exclude it with `ignoreMemdevs`, or list it
  in `sharedMemdevs`, so that the driver never onlines it. Exclusive use
  across hosts is not coordinated by the driver of one node.
- Hot-removing a local memdev that is in use is not supported: the
  kernel refuses to remove a memdev with an online region.
- The sharers of a shared local memdev are whoever may create a claim of
  `cxl-local-shared` on the node; `readOnly` guards against writes, not
  reads.
- Leases of local memory have the limitations of every lease
  ([leases.md](leases.md#limitations)).

## Node requirements

The node needs what [usage.md](usage.md#requirements) lists: the kernel
options, the memory onlining and the udev rule. Persistent memdevs need
the options of persistent memory too.
