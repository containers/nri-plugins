# Design of the CXL DRA driver

CAUTION: experimental. Interfaces may change.

This page tells how `nri-dra-driver-cxl` works inside: the objects it
keeps, how a claim becomes memory that a container uses, how a restart
of the driver finds its way back, and which files it keeps on the node.
It is written for the developers who change the driver, and for users
who want to know what it does with their devices. [usage.md](usage.md)
tells how to deploy and use the driver. [states.md](states.md) is
generated from the code: it lists every state of every record and every
transition that the driver allows, with its trigger, guard and effects.
This page links to it for every state that it names.

- [The parts of the driver](#the-parts-of-the-driver)
- [The objects](#the-objects)
- [How a claim becomes memory](#how-a-claim-becomes-memory)
- [How memory is released](#how-memory-is-released)
- [How a restart recovers](#how-a-restart-recovers)
- [The life of a lease](#the-life-of-a-lease)
- [Files on the node](#files-on-the-node)

## The parts of the driver

One process on every node has these parts:

- Two kubelet plugins of one kind: `cxl.nri.io` for the memory of the
  node (local memdevs, regions, DRAM, the lease device) and the pool
  driver (`cxl-pool.nri.io` by default) for the devices of a CXL memory
  pool. They differ only in a few source rules: where the serial and the
  facts of a device come from, where their node mounts live, and whether
  they publish. The facts of a local memdev come from the node's own
  ResourceSlice, those of a pool device from the claim status that the
  pool controller writes.
- The publisher, which writes the ResourceSlice of `cxl.nri.io` for the
  node. It scans the node after udev events, after the Prepares and
  Unprepares of both drivers that changed the records, and on the ticks
  of the provenance watch, and writes the slice only when its devices
  changed. The pool driver publishes nothing: the pool controller does.
- The device engine, the one owner of every CXL kernel object that the
  driver makes on the node, for both drivers: regions, dax devices and
  their drivers, memory blocks, namespaces, block device flags, node
  mounts, loop devices and lease binds. Nothing else in the driver
  changes them. It reads and changes the kernel through one package that
  observes sysfs and runs the `cxl` and `ndctl` tools; dax devices are
  bound to their drivers through sysfs.
- The NRI plugin, the one writer of the `cpuset.mems` of containers. It
  writes them through NRI, and in the cgroup of a steered container. It
  keeps containers off the CXL memory that they did not claim (fencing),
  starts the steering of the containers that opt in, and lends NUMA
  nodes to running containers.
- The lease broker: the sockets of lease claims, the leases that running
  containers ask for on them, and the companion claims and pods that
  hold the memory of the leases.
- The provenance watcher, which tells pool devices from local memdevs by
  their serials.
- The store: one document, `state.json`, that holds every record of the
  driver that must outlive the process.

```
  kubelet --> cxl.nri.io plugin --+                 +--> kernel: sysfs, cxl, ndctl,
  kubelet --> pool plugin --------+--> engine ------+    mounts
                  |                     |  ^
                  |                     v  |
  runtime --> NRI plugin ------------> store (state.json)
                  ^                     ^
  container --> lease broker -----------+--> Kubernetes API: companions,
                                        |    ResourceSlices, ResourceClaims
  uevents, lists --> provenance --------+
```

## The objects

The driver's words for its objects, and how they relate. The kernel's
objects keep the kernel's words: memdev, region, dax device, memory
block, NUMA node, namespace, block device, mount.

### Memdev

A memdev is a CXL memory device of the node, `memN`, known by its serial
(the name `memN` changes with the probe order). The driver keeps a
[Memdev](states.md#memdev) record while it uses one: with its facts (RAM
and persistent capacity, shared, persistent, block size, host bridge),
the regions of the driver on it and its grants. A memdev with grants is
[ACTIVE](states.md#memdevstate). One whose region is kept without grants
is IDLE_KEPT: a configured shared device, or a persistent device below
newer regions. One that went away while grants exist is GONE, such as a
pool device detached under its claims. A memdev of no grant and no region of the
driver has no record. How the kernel shows it is
[Memdev (kernel)](states.md#memdev-kernel).

### Region

A region maps the memory of one or more memdevs into the address space
of the host. The driver keeps a [Region](states.md#region) record of
every region on the memdevs it uses. A region of its own goes through
the [states](states.md#regionstate) NOTED, OWNED, KEPT and HOLLOW_OWNED.
NOTED: the name that `cxl create-region` will give is written down
first. KEPT: disabled and kept, because its release would leave a hole
in the decoders of a port, or because it waits for the claim whose
Prepare did not finish. HOLLOW_OWNED: its memdev went; it is deleted
when its memory is offline. A region that the driver did not create is never touched: it is published
as a `region` device when it is online RAM, and its memdevs are not
published as memdevs. On a memdev that the driver uses it gets a FOREIGN
record (a Prepare that it refuses, or a start, records it), and the
record goes with the region.

Linux commits the decoders of a CXL port in order. Destroying a region
below a newer region of another memdev on the same host bridge or switch
leaves a hole, and no new region goes through that port until the
regions above are gone. So the release of a region follows a release
policy per kind of memdev, described in
[How memory is released](#how-memory-is-released).

### Dax device

A dax device (`daxN.M`) is a part of a region. The driver keeps it in
the region's record as a [DaxDevice](states.md#daxdevice) with the
grants that use it: the slice of one claim of a local memdev, or the
whole device of a shared memdev that every claim maps. Bound to `kmem`
its memory is system RAM of a NUMA node
([KMEM_ONLINE](states.md#daxdevicestate)); bound to `device_dax` it is a
device node that a container maps (DEVICE_DAX). `daxN.0` of the region
of an unshared memdev stays as the SEED at size 0, which the kernel never deletes.

### Mount and bind

A mount is a node mount of the filesystem on the block device of a
persistent region, one per access (`direct` or `cached`), with the grants
that use it ([Mount](states.md#mount)). Containers of claims get a bind
of it at their `path`. It is read-write while a writer grant uses it and
read-only otherwise ([MOUNTED_RO, MOUNTED_RW](states.md#mountstate)). A
bind is the bind of a node mount into the `lease-fs` directory of a
lease claim, the lend of an `fs` lease ([Bind](states.md#bind)). A bind
whose unmount is busy (a file of it is mapped) is STALE and is unmounted
again later. A node mount whose unmount is busy fails the release, and
kubelet retries it.

### Grant

A grant is the memory that one allocation result of a claim gets on one
memdev ([Grant](states.md#grant)). It has three parts:

- What the result asks for: mode, read-only, access, filesystem type,
  path, size, shares or exclusive.
- Its intent, what the driver may create for it: the noted region name,
  the namespace, the mount path, the bind target, the lend.
- What it got: the region, the dax device, the NUMA node, the namespace
  and block device, the mount, the bind, the lend.

A grant references the region, dax device and mount that it uses; those list
the grant among their users, so that several grants can share one
object. Its [states](states.md#grantstate): JOURNALED (the intent is
written), REALISING (kernel work in progress), REALISED (usable),
REALISED_UNRESTORED (its mount, dax or block device did not verify at a
start), RELEASING; a released grant is gone from the store.

### Preparation

A preparation is the record of one claim as one driver prepares it
([Preparation](states.md#preparation)), the record that kubelet's
prepared flag of the claim refers to. Its results say what each
allocation result of the driver got: a grant on a memdev, the capacity
of a region, the DRAM nodes, or a lease claim. The CDI devices, the
environment variables of the containers and the NUMA nodes that a
container has claimed are derived from the results. Its
[states](states.md#preparationstate): PREPARING (written before any
kernel object is touched), PREPARED, PREPARED_UNRESTORED, PREPARED_STALE
(prepared in another boot), UNPREPARING, and KEPT (not prepared, but a
region is kept for the claim).

### Leases and lease claims

A lease claim is a claim on the `lease` device of `cxl.nri.io`
([LeaseClaim](states.md#leaseclaim)): its socket, which its containers
have at `/run/cxl/cxl.sock`, its `lease-fs` directory, which they have at
`/run/cxl/fs`, and the pods it is reserved for. A lease is one request of
a running container for memory on that socket ([Lease](states.md#lease)),
known by 8 hex digits. Its memory comes from a companion claim
`cxl-lease-<id>` of a companion pod, which a driver prepares like any
claim. The lease owns its lend: the NUMA node lent to the requester's
`cpuset.mems`, the file descriptor passed to it, or the bind in its
`lease-fs` directory. The companion's grant records the lend too, so
that an Unprepare can revoke it also after the lease record is gone.

### Container and fence

The NRI plugin keeps a record of every container of the node in memory
([Container (in memory)](states.md#container-in-memory)). It holds how
the NRI plugin manages the memory of the container: STEERED, FENCED or
UNFENCED, decided once when the container is created. It also holds the
container's own NUMA nodes, the nodes of its results and the nodes lent
to it. The record is built again from the runtime after a
restart. The fence is the set of NUMA nodes on which the driver onlined
CXL memory since it started: a FENCED container has its own nodes
without the fenced ones, plus the nodes of its results and leases. A
node joins the fence before its memory goes online, and the running
containers are updated before that (an update that takes longer than
the fence deadline finishes in the background); it leaves the fence
only when the driver restarts. A container whose memory a steering
manager of the driver writes has a [Steered](states.md#steered) record
in the store, so that the next process releases exactly the containers
that its managers throttled. A steering manager sets the memory policy
of the processes of its container by injecting a `set_mempolicy` system
call into their threads with ptrace (package `mpolinject`). The
injection can crash threaded programs; [usage.md](usage.md#steering)
lists its risks.

### Provenance

A memdev on the node is either a device of the node or a device of a
memory pool that a pool controller attached. The Kubernetes API does not
always tell: a pool controller deletes its ResourceSlices when it stops,
while its devices stay attached. The provenance of a serial
([Provenance](states.md#provenance)) is the driver's sticky knowledge of
it. It is POOL once a ResourceSlice or a claim of the pool driver names
it, and stays POOL until the serial was observed absent for 24 hours. It
is LOCAL for a device of the node, and HELD for a new memdev whose
source is uncertain, which is not published for a while. The pools whose ResourceSlices the
driver has listed are kept as [ListedPool](states.md#listedpool)
records. An operator removes the provenance of a serial with
`--forget-provenance` ([local.md](local.md#what-the-driver-publishes)).

The rules, at every scan of the publisher (after udev events, after the
Prepares and Unprepares that changed the records, and at least every 2
minutes plus at most 30 seconds):

- The scan lists the ResourceSlices of `cxl-pool.nri.io` (those of
  other drivers do not count). A serial that a slice lists, that the pool
  driver prepared, or that the status of a pool claim allocated to the
  node names is POOL. A failed list keeps the serials of the last good
  one; until a list has succeeded no memdev is published (regions are),
  and the list is tried again every 30 seconds.
- POOL is forgotten after the serial was observed absent (in no slice,
  not prepared, not on the node) for 24 hours. Absence counts over scans
  at most 5 minutes apart, and starts again after a longer gap (a stopped
  driver, failing lists, a step of the clock) and after a restart.
- A memdev that was published as local stays LOCAL unless the pool
  driver publishes or prepares its serial; then it is POOL, and the next
  scan withdraws it. Local claims that use it at that moment keep their
  memory (the log has an error), and no new local claim is prepared on
  it.
- Any other memdev is new. It is HELD while a pool claim allocated to
  the node is not attached yet (its status has neither a serial nor a
  failed attach): it may be the device of that claim. It is HELD for up
  to 5 minutes while a remembered pool has been without ResourceSlices
  for less than an hour, while the list fails, or in the first hour
  after a start without a readable state (LOST). Then it is POOL if the
  slices list it, else LOCAL. Memdevs on the node at the first start,
  and a memdev hot-added while all pools have their slices, are LOCAL at
  once. The hour of a pool without slices starts again at a restart.
- Before it creates a region on a local memdev or onlines its memory,
  the engine reads the provenance again and lists the slices of the pool
  driver. The Prepare of a local claim on a HELD or POOL memdev, or on a
  serial that a pool lists, fails with `memdev 0x... is a pool device
  (HELD|POOL): not used as a local memdev`.

### Probe

The address line probe runs on every new region of a local memdev of RAM
and finds memory that aliases. Its result, the usable size of the
memdev, is a fact about the hardware ([Probe](states.md#probe)), kept in
a file of its own, `localProbes.json`, outside the document.

### How they relate

```
  Preparation (claim uid, driver)
    results: grant | region | dram | lease claim
                |                         |
                v                         v
  Grant (claim uid, result index)     LeaseClaim (claim uid)
    serial --> Memdev                    <-- Lease (id): requester container,
    got:   --> Region                            companion claim, lend
                 dax devices, mounts
                 (users: grants)
  Provenance (serial): may a local grant use the memdev?
  Container: the NUMA nodes of the grants of its results and of its
             leases; the fence: the nodes kept away from the others
```

## How a claim becomes memory

Every Prepare of either driver goes the same way:

1. Validate. The `MemoryConfig` of every result is resolved and checked;
   the device must be in the last ResourceSlice that `cxl.nri.io`
   published (local devices) or have the status data that the pool
   controller wrote (pool devices). The companion claim of a lease must
   pass the companion gate: its name, namespace and owner must be those
   of the lease, and its device that of the lease. A refusal here touches
   nothing.
2. Write the intent. The preparation is committed as PREPARING with its
   results, durably, before any kernel object is touched.
3. Realise the grants. For each grant the engine first commits its
   intent (JOURNALED), then does the kernel work (REALISING), and commits
   every object it made before the next step that depends on it. A
   grant that is REALISED already and still verifies is reused, so that
   a Prepare again after a cut continues where the first one stopped.
4. Open the lease claim of a `lease` result: its socket and directories.
5. Write the CDI spec of the claim, and commit the preparation as
   PREPARED. Only then kubelet gets the result.

Then kubelet creates the containers. The CDI device of every result has
a marker variable; at CreateContainer the NRI plugin finds the results
of the container by their markers and sets its `cpuset.mems` or starts
its steering. The NRI plugin ignores, with a warning, a marker of a
claim of another namespace, and one of a lease claim whose recorded
`reservedFor` does not have the pod. A container cannot take the memory
of another pod's claim by setting the variable itself.

A Prepare that fails rolls back: it releases what its grants made, with
the release policy of a rollback. A region that it cannot release
without leaving a hole in the decoders stays for the claim, and the
preparation is KEPT: a Prepare again of the claim reuses the region,
its Unprepare releases it.

### A local memdev

On the first claim of an unshared local memdev the engine notes the
name of the region to come and creates a RAM region over the memdev.
With `verifyAddressing` it runs the address line probe on the region; a
region whose memory aliases is made again with the usable size. Every
claim gets a
slice of its own: a dax device of the region on whole memory blocks.
Mode `numa`: the NUMA node of the region joins the fence (the running
containers are updated first), the slice is bound to `kmem` and its
memory blocks are onlined movable. Mode `dax`: the slice is bound to
`device_dax` and zeroed before the claim gets it. A shared local device
has one dax device over the whole region, bound to `device_dax` and
zeroed once when the region is made; every claim maps it. Before any
kernel work on a local memdev, the engine checks that the memdev is not
a pool device. Its provenance must not be POOL or HELD, a fresh list of
the pool driver's ResourceSlices must not name it, and no pool grant may
use it.

The size of a slice is the consumed capacity of the allocation (the
whole memdev without one), rounded up to the memory block size, which is
read again for every slice. The first slice uses `daxX.0` when nothing
else does, every other slice is a new dax device that the kernel places
first-fit. Because every slice is a multiple of the block size, the
placement keeps every slice and every free gap of a block-aligned region
block-aligned, also after releases. The engine checks every range of
every slice before it binds a driver. First-fit places a slice across a
block boundary when the region does not start at one. Such a slice is
deleted and made again on the first whole free blocks, combining free
ranges when one is not enough, so that the head and tail of such a
region are never used. A region with less free memory than the slice
fails the Prepare with "no room for the slice": the scheduler accounts
the capacity, so only a smaller usable size found by the probe does
that.

A `numa` slice is bound to `kmem` through sysfs and its memory blocks
are onlined movable. A udev rule that onlines hot-added memory onlines
the blocks of a new region after the engine offlined them: the engine
offlines them again until they stay offline for 500 ms, and fails the
Prepare after 30 s. `kmem` is never unbound from memory that is online,
and a region whose memory is online is never destroyed: the kernel could
not remove the memory, and online memory without its region would read
garbage. A `dax` slice is bound to `device_dax` and zeroed. The seed
`daxX.0` is never deleted: an Unprepare sets it to size 0.

A shared local memdev has one dax device over the whole region. The
first claim creates the region and zeroes the device; when the zeroing
fails, the region is destroyed again and the claim gets nothing. A
region that went away while the driver runs is forgotten with its
claims, and the next claim makes a new one.

### A pool device

The pool controller attaches the device to the node before the claim is
prepared. The grant's intent and the POOL provenance of its serial are
committed together. The engine waits for the memdev to appear
(`--pool-prepare-timeout`), enables it when it has no driver after a
short grace, and creates a region over it. Mode `numa`: the node joins
the fence, and the memory blocks of the region are onlined movable as a
new NUMA node. Mode `dax`: the region's dax device is bound to
`device_dax`; the memory is never onlined and never zeroed (a shared
device has the same bytes on every host that has it). Modes `block` and
`fs` are those of [a persistent device](#a-persistent-device). The
Unprepare of the last claim of the device on the node offlines its
memory, destroys its region and disables the memdev, so that the pool
can hot-remove it; [pool.md](pool.md#ordering) has the order of attach,
Prepare, Unprepare and detach.

### A persistent device

A persistent pool device and a persistent local memdev are prepared by
the same code. The engine creates a region of persistent memory, waits
for its nvdimm region, finds the fsdax namespace (a writable `block`
claim creates one when there is none) and its block device, which stays
read-only unless a writer grant uses it. Mode `block`: the container
gets the block device. Mode `fs`: the filesystem is mounted on the node,
once per access, at `mounts/<serial hex>-<access>` in the data directory
of the claim's driver, only when its type is in `fsTypes`; a `direct`
mount is verified to map the device's memory. The container gets a bind
of the node mount. A writer is allowed only when no other host may
write the device, or with a cluster filesystem on coherent memory. A
persistent device is never zeroed: its data and namespace labels outlive
the claims. [usage.md](persistent.md) has the rules.

### A lease

A container with a claim on `cxl-lease` asks on its socket with
`cxl-request`. For every request the broker finds the requester's
container from the id of the process that connected, through the
container table of the NRI plugin. It checks the limits of the claim and
the pod, and commits the lease as CREATING before it creates any API
object. Then it creates the companion claim and pod (PENDING); the
scheduler allocates the claim, and kubelet asks the driver of the device
to prepare it. The companion gate passes the claim to the lease
(LENDING), and the grant is realised like that of any claim. At the end
of the Prepare the lend is made and the lease is READY:

- The NUMA node of a `numa` lease is added to the requester's
  `cpuset.mems` through NRI.
- The file descriptor of a `dax` or `block` lease is passed to the
  client.
- The node mount of an `fs` lease is bound into the claim's `lease-fs`
  directory, where the container sees it at `/run/cxl/fs/<name>`.

[leases.md](leases.md#flow) has the protocol and the flow.

### Regions and DRAM

A result on a `region` or the `dram` device needs no device work: the
claim consumes capacity, and its containers get the NUMA nodes for their
`cpuset.mems` or their steering.

## How memory is released

kubelet's Unprepare of a claim first closes its lease claims (their
leases are released) and, for a companion claim, revokes the lend and
unbinds the bind. Then the preparation is committed as UNPREPARING and
the engine releases each grant. It unmounts the node mounts that no
other grant needs, or makes a mount read-only when its last writer goes.
It offlines the memory blocks of the grant and deletes its dax device.
When the grant was the last of its memdev, it releases the region by the
release policy. A pool memdev is disabled after its region is
destroyed, so that the pool can hot-remove it. A step that fails (memory
that cannot be offlined, a busy mount) fails the Unprepare with every
record as it was; kubelet retries, and the retry finds out from the
kernel what still exists. A record is removed only by a successful write
of the document without it.

A slice whose release fails where kubelet does not retry it (the
release at a start of a claim whose Prepare did not finish, the rollback
of a failed Prepare) stays recorded with its claim as
[RELEASING](states.md#grantstate). It is never handed out again; the
next Prepare or Unprepare of the claim, and every start, release it
first and fail while they cannot. The region stays while such a slice
exists.

The release policy, by kind of memdev:

- An unshared local memdev: its region is destroyed with its last slice. A
  rollback or a recovery releases it in order only: one below a newer
  region of another memdev stays for its claim.
- A shared local device: its region stays while the device is in
  `sharedMemdevs`, zeroed only when it was made; once the device is no
  longer in `sharedMemdevs`, its region is destroyed with its last claim.
- A persistent local memdev: its region stays idle while regions of
  other memdevs lie above it, and is destroyed otherwise.
- A pool device: its region is destroyed with its last grant, also below
  newer regions, because a kept region would block the hot-removal of
  the device; the hole closes when the regions above go.

After `cxl disable-region`, the release in the running driver checks
again, under the lock of the region creations, that no region came above
it meanwhile, such as one of a pool Prepare on the same host bridge. If
one did, the region stays, disabled, and is KEPT. A kept region has
no slices. The next Prepare of its claim sets it up like a new one:
enabled again, its dax devices deleted, `daxX.0` made over the whole
region, bound to `device_dax`, probed, and zeroed for a shared memdev.
When nothing lies above it any more, that Prepare destroys it and
creates a new one instead. A kept region whose memory aliases is made
again smaller only when nothing lies above it; the Prepare fails
meanwhile.

## How a restart recovers

A stop of the process cuts what is running: a Prepare between two of its
steps, a release, a lease request. Because every record is written
before the kernel objects it names are made, the next start knows what
may exist and finishes or undoes it. The start goes in this order:

1. Read the boot id and open the store: lock `state.lock`, read
   `state.json` (or `state.json.prev` when `state.json` cannot be read).
2. Recovery, before NRI and before kubelet can call:
   - Observe the kernel.
   - Mount again the node mounts of the prepared claims, found by the
     identity of their block device. Verify their dax and block
     devices; a mismatch makes the claim PREPARED_UNRESTORED.
   - Bind the binds of prepared `fs` companions again, but not of those
     being unprepared.
   - Release what the grants of the Prepares that did not finish made,
     the top-most region first, or keep it by the release policy. The
     preparation stays PREPARING or becomes KEPT.
   - Keep what the `numa` companions whose Prepare did not finish have,
     for kubelet's Unprepare or Prepare again.
   - Unmount the mounts and binds under the driver's directories that
     no prepared claim lists.
   - Delete the dax devices of the driver's regions that no grant uses,
     and its hollow regions.
   - Write the CDI specs again from the records.
3. The first list of the pool driver's ResourceSlices, which gates the
   publication of local memdevs, and the fence: the NUMA nodes of the
   claims prepared in this boot.
4. The lease broker: the lease claims and leases of this boot, their
   sockets listened again.
5. The NRI plugin connects and synchronizes: the container records are
   built again from the runtime; the steering of running containers is
   not restarted, and the containers that the previous process steered
   get their `memory.high` back. The lends are wired.
6. The kubelet plugins register, the ResourceSlice is published, the
   watchers and the periodic passes start (stale binds are unmounted
   again, the `cpuset.mems` of fenced containers repaired).

A Prepare cut while it created the region of the first claim of a
memdev leaves the region (enabled, or disabled by a cut rollback) and
the noted name. The start destroys that region, and the Prepare again of
the claim creates a new one; a region that a cut `cxl create-region` left
without a target is deleted. A region on the memdev with another name
is not the driver's: it is not touched, and the memdev is not
published. On a memdev that is a pool device now nothing is touched.

The document records the boot id of the kernel
(`/proc/sys/kernel/random/boot_id`). At the first start of a new boot
the records of regions and slices of the other boot are dropped without
touching any region. A region that firmware or an admin created under
the same name is then never taken for the driver's.

What a restart keeps, and what a reboot changes:

| record | restart of the driver | reboot of the node |
|:-------|:----------------------|:-------------------|
| preparation | kept; one whose Prepare did not finish stays PREPARING (or KEPT) for kubelet's retry | becomes PREPARED_STALE |
| grant | kept, verified | keeps what it asked for, forgets what it got |
| memdev, region | kept, matched with the kernel | dropped: nothing of them survives a reboot, and region names are given anew |
| lease | kept | dropped; the first reconcile deletes the companions |
| lease claim | kept, socket listened again | kept: kubelet prepares the claim again |
| provenance, listed pools | kept | kept |
| steered container | its container gets its `memory.high` back at the first Synchronize, which keeps the records of the new process's managers only | dropped at the first Synchronize: the containers are gone |
| probe results | kept (own file) | kept (own file) |

A stale preparation is the record of a claim prepared in another boot:
nothing of what its Prepare made exists any more, and the new boot gives
the names and numbers of those devices to other devices. It has no CDI
device and no NUMA node in the fence, and nothing is released for it at
start. kubelet prepares the claim again before it starts the pod's
containers, which makes a new preparation. Or it unprepares the claim,
when the pod went during the reboot; that disables a pool memdev that
no claim of this boot uses.

When neither `state.json` nor `state.json.prev` can be read, the driver
starts LOST instead of crash-looping:

- It writes nothing, releases nothing and recovers nothing.
- It fails every Prepare with the error of the loss and keeps the CDI
  specs.
- It fences every NUMA node of online CXL memory.
- It starts the lease broker without leases, and deletes no companion.
- It holds back new memdevs, since it does not know which are pool
  devices.

An operator starts it once with `--new-state` to begin with a new
document ([usage.md](usage.md#an-unreadable-state)).

### Node mounts after a restart

The node mounts of mode `fs` are found again by the device number of
their block device. A path would not do: a loop device of a cached node
mount that the previous driver container set up shows its backing file
in sysfs relative to the root of that container's `/dev` (`/pmem1`). A
missing node mount is mounted again when the fsdax namespace of the
region is the one prepared: its name, its block device and the numbers
of that. A node mount may stay unmounted after that: the region is
gone, the namespace or block device is another one, or the mount
failed. Its claims stay prepared without a CDI device
([PREPARED_UNRESTORED](states.md#preparationstate)). Their containers
then fail at creation on an unresolvable CDI device rather than bind a
missing directory. The next start, kubelet's Prepare again
of such a claim and the Prepare of another claim of the node mount
mount it again if they can; the Prepare again fails with the reason
otherwise (`its node mount ... is not mounted and cannot be mounted
again: ...; unprepare the claim (delete its pods) to prepare it anew`).
Before the release of such a claim the Unprepare waits for the block
device of the region now, not the recorded one, which may be another
device's. Companion claims have no CDI device; their lease bind is bound
again only with the node mount.

### A Prepare cut on a pool device

The region of a cut Prepare is the newest one in the decoders of its
host bridge and switch at that moment. Kept until kubelet's retry, it
would lie below the regions that other claims get meanwhile, and its
release by a retry that fails, or by the claim's Unprepare, would leave
a hole. So the start releases the device of a cut Prepare before kubelet
can call, as for a prepared claim, and forgets the claim; kubelet's
Prepare again makes the region anew. The start keeps the claim, KEPT, in
three cases: it is a companion claim of a lease, a region of another
memdev lies above its region already, or something could not be
released, such as a busy node mount. The NUMA node of such a `numa`
device is fenced. kubelet then prepares the claim again, which reuses
the region. Or it unprepares the claim, which revokes the NUMA nodes
that a companion claim may have lent, unmounts its lease bind and
releases the device. An Unprepare
that would revoke lent NUMA nodes before the driver has connected to NRI
fails, and kubelet retries it.

At a reboot a stale preparation of a pool device releases the device at
its Unprepare (`cxl disable-memdev`) before the controller detaches it,
also when the device was away at a start. The driver does not start
when it cannot read the boot id.

### Hollow regions

A hollow region has no target and is not committed, and its address
range is kept in the root decoder. The kernel leaves one of a region
whose memdev was removed before the region was destroyed (a forced
deletion), and a `cxl create-region` that was killed before it set the
target leaves one. No
decoder maps its address range any more; memory of it that `kmem` could
not remove stays online until a reboot. `cxl destroy-region` refuses it
("error locating decoder for target0"); writing its name to
`/sys/bus/cxl/devices/<root decoder>/delete_region` removes it. The
driver deletes only the hollow regions that it knows as its own
([HOLLOW_OWNED](states.md#regionstate)), and only while none of their
memory blocks is online. These are the region of a prepared claim whose
memdev is gone, at its Unprepare, and the regions recorded for a Prepare
that did not finish. The scan of `cxl.nri.io` skips every hollow region; it warns
once, with that command, about each one that is not the driver's.

`cxl create-region` right after a hotplug can fail once with
"add_dev() failed"; the pool driver tries a region creation once more
after 3 s.

## The life of a lease

The lease is committed before any companion object is created. The
driver that prepares the companion claim records the lend with the
claim's grant, so that an Unprepare revokes it also after a restart or
after the lease record is gone. A start without a readable state (LOST)
has an empty lease table; it deletes no companion and keeps the CDI
specs.

The Prepare of a lease claim creates
`/var/lib/kubelet/plugins/cxl.nri.io/leases/<claim uid>/cxl.sock` (mode
0666; the directory 0755, labelled `container_file_t` when SELinux is
enabled), the empty mount point `fs` in that directory, and
`lease-fs/<claim uid>` (0755, labelled the same way). Its CDI device
`cxl.nri.io/device=lease-<claim uid>` binds the socket directory at
`/run/cxl` (rbind, ro), the `lease-fs` directory at `/run/cxl/fs`
(rbind, ro, rslave) and the CLI at `/usr/local/bin/cxl-request` (ro; left
out with a warning when `--lease-cli` does not exist). The directory,
not the socket, is mounted, so that a restarted driver can create a new
socket that the running containers see. `/run/cxl/fs` is a slave of the
host's mount: a mount that the driver makes in `lease-fs/<claim uid>`
later appears in every running container of the claim, and its unmount
takes it away there. The directory is separate from the socket directory
and read-only in the containers, so a container cannot replace a mount
point by a symlink and make the privileged driver mount over a host
path.

The bind of an `fs` lease is the lend, made when the companion claim is
prepared. A read-only lease of a writable type gets a read-only clone of
the node mount (`open_tree` and `mount_setattr` with `ro`, `nosuid`,
`nodev`), which stays read-only when a writer of the node makes the
superblock read-write. A read-only lease of erofs or squashfs, and a
read-write lease, get a plain bind with the flags of the node mount.
After the bind its mountinfo must show the type and source of the node
mount and the flags of the lease, else the driver unmounts it and the
Prepare fails (`the bind of <node mount> at <target> is not a read-only
nosuid nodev <t> mount`). Its unmount when the companion claim is
unprepared is the revoke, and is never lazy: it fails while files of it
are open or mapped. The node mount is unmounted, and the region
released, only after the bind is gone and when no claim of the node uses
them. A `block` lease binds and lends nothing: the driver opens the block
device for each response that carries the fd and closes its copy. A
plain open fd is no holder that the driver sees (it checks mounts, loop
devices and exclusive opens), so the release does not wait for it; no
DAX mapping is involved, so this cannot crash the node.

The Unprepare of a lease claim starts the release of all leases of the
pod. It waits up to 5 s until their NUMA nodes are revoked and the
deletion of their companions is requested. It does not wait for the
companions to be gone: kubelet unprepares the companion claim of a local
lease through the same driver, one call at a time. A lease stays
Releasing until its companion pod and claim are gone, also across a
restart; the broker checks them at growing intervals, at most 60 s
apart. A restarted requester container is created with the NUMA nodes
of its `numa` leases in its `cpuset.mems`, and they are checked against
its cgroup once it has started. A fenced container gets them in the
CreateContainer adjustment, because containerd applies an update before
the start to the spec only.

The reconcile runs at start, after NRI has synchronized, and every 60 s:

- It restarts interrupted releases.
- It releases the leases left in Creating, those whose companion pod is
  gone or finished and those whose requester will not come back.
- It applies the `numa` lends of Ready leases again, so that a restart
  restores them, and a lend that a cgroup lacks is updated again.
- It deletes the pods and claims labelled with the node and a lease,
  named `cxl-lease-<lease>`, that have no lease in the table. A
  companion pod without the labels counts as gone.

At the first start of a new boot the leases of the other boot are
dropped; the first reconcile deletes their companions, whose claims the
drivers then unprepare.

When a lease claim is unprepared, its socket is removed, then
`lease-fs/<claim uid>`, then `leases/<claim uid>` (its entries removed by
name). While a bind is still in `lease-fs/<claim uid>` both directories
stay, because removing the mount point of `/run/cxl/fs` would detach the
bind lazily in a container that is still there. They are removed when
the last lease of the claim is dropped, and by the reconcile and the
start, which remove the directories of lease claims that are gone. Under
`lease-fs` only empty directories without a mount are removed, never
recursively, and nothing is removed while the lease table cannot be
decoded.

At start the driver binds again a missing bind of a prepared companion
claim, when its node mount is there or could be mounted again and the
claim is not being unprepared. It unmounts the mounts under `lease-fs`
that belong to no prepared companion claim, so that they do not hold
the device; a crash between the bind and the write of its record leaves
such a mount. A bind of no prepared companion that cannot be unmounted
(busy) is [STALE](states.md#bindstate) and is unmounted at the next
Unprepare or failed Prepare of any pool claim on the node. Once it is
gone, the region of its device is released if no claim of the node has the device
and nothing holds `/dev/pmemK` within 10 s. The driver never detaches a
mount and releases a region only when nothing holds its block device: a
bind left mounted keeps the device and never crashes the node.

## Files on the node

Only the driver writes its files; do not edit them, and do not copy
them to another node. The flags `--forget-provenance` and
`--new-state` are the changes that an operator makes.
[usage.md](usage.md#files-on-the-node) lists every file with its path.

- `state.json` in the plugin data directory of `cxl.nri.io` is the one
  document of both drivers. A change is a function of the document; the
  writer applies the pending changes in groups and writes each group
  once: a synced temporary file, the current file linked to
  `state.json.prev`, a rename, a synced directory. What must be on the
  disk before something happens outside (a response to kubelet or to a
  lease client, an NRI update, a kernel command that creates something)
  waits for that write; the rest does not. A failed write discards the
  group, and the driver never acts on what is not on the disk. The
  document has a schema number and is read strictly: a document of
  another schema, or with a field the driver does not know, cannot be
  read.
- `state.json.prev` is the document before the last write, read when
  `state.json` cannot be. `state.json.bad`, `state.json.lost` and
  `state.json.prev.lost` are documents that could not be read, kept for
  diagnosis.
- `state.lock` is locked by the running driver, so that a second
  process (the new pod of a rolling update) waits for the first one to
  exit.
- `localProbes.json` holds the results of the address line probe, facts
  about the hardware that outlive every state of the driver.
- `leases/<claim uid>/cxl.sock` are the lease sockets and
  `lease-fs/<claim uid>` the directories of the binds of `fs` leases,
  owned by the lease broker; `mounts/` in the data directory of each
  driver holds its node mounts of mode `fs`. The start removes the
  directories that no record names, and unmounts what no prepared claim
  lists.
- The CDI specs `cxl-claims.json` (with the devices of the lease claims)
  and `cxl-pool-claims.json` in the CDI root are written from the
  records, and again at every start.
