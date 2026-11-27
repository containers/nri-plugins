# Persistent memory

CAUTION: experimental. Interfaces may change.

A device of persistent memory keeps its data and its namespace labels
when its claims end. A writer stores a filesystem image once, and
readers on many nodes use it in place; or the claims of one node read
and write a filesystem together. Two DeviceClasses offer such devices:

| class | devices | driver |
|:------|:--------|:-------|
| `cxl-pool-persistent` | pool devices with `persistent: true`, shared or not ([pool.md](pool.md)) | `cxl-pool.nri.io` |
| `cxl-local-persistent` | local memdevs of persistent memory, in `sharedMemdevs` or not ([local.md](local.md#persistent-local-memdevs)) | `cxl.nri.io` |

Both drivers prepare these devices with the same code and the same
rules. A persistent device has two modes, `block` and `fs`, and no
default mode.

- [Claims](#claims)
- [Modes](#modes)
- [Filesystems](#filesystems)
- [One node](#one-node)
- [Several nodes](#several-nodes)
- [Filesystem images](#filesystem-images)
- [Errors](#errors)

## Claims

A shared pool device and a persistent local memdev have a `shares`
capacity: a claim without a capacity request has the device alone, a
claim with `capacity: {requests: {shares: "1"}}` is one of its sharers.
An unshared pool device has one claim at a time.

| the claim gets | capacity request | `device` |
|:---------------|:-----------------|:---------|
| the block device, writable: the writer of an image | none | `{mode: block}` |
| the block device, read-only | `shares: "1"` | `{mode: block, readOnly: true}` |
| the filesystem, read-only | `shares: "1"` | `{mode: fs}` |
| the filesystem, read-write | `shares: "1"` | `{mode: fs, readOnly: false}` |

A writer of an erofs image and its readers, on a pool device:

```yaml
# the writer
requests: [{name: mem, exactly: {deviceClassName: cxl-pool-persistent}}]
config:
- opaque: {driver: cxl-pool.nri.io, parameters: {apiVersion: cxl.nri.io/v1alpha1, kind: MemoryConfig, device: {mode: block}}}
---
# a reader, after the writer is done
requests: [{name: mem, exactly: {deviceClassName: cxl-pool-persistent, capacity: {requests: {shares: "1"}}}}]
config:
- opaque: {driver: cxl-pool.nri.io, parameters: {apiVersion: cxl.nri.io/v1alpha1, kind: MemoryConfig, device: {mode: fs, path: /data}}}
```

The reader's container sees

```
CXL_C1AE0002_ACCESS=direct
CXL_C1AE0002_FS_TYPE=erofs
CXL_C1AE0002_MODE=fs
CXL_C1AE0002_PATH=/data
CXL_C1AE0002_READ_ONLY=1
CXL_C1AE0002_SERIAL=0xc1ae0002
CXL_C1AE0002_SHARED=1
CXL_C1AE0002_SIZE=534773760
```

and the writer `CXL_C1AE0002_PATH=/dev/pmem1` and
`CXL_C1AE0002_READ_ONLY=0`. The samples
[pool-persistent-erofs.yaml](blob:/deployment/samples/dra-driver-cxl/pool-persistent-erofs.yaml)
(a writer and two readers of an erofs),
[pool-persistent-fs-rw.yaml](blob:/deployment/samples/dra-driver-cxl/pool-persistent-fs-rw.yaml)
(a read-write and a read-only pod of one ext4 on one node) and
[local-persistent-fs.yaml](blob:/deployment/samples/dra-driver-cxl/local-persistent-fs.yaml)
(a writer and a reader of a persistent local memdev) are complete.
A running container leases a persistent device with
`cxl-request request --serial 0x<hex> --mode fs` (`--read-only=false` to
write, `--mode block` for the block device,
[leases.md](leases.md#cxl-request)).

## Modes

The driver creates a pmem region on the device. The fsdax namespace
(map `mem`) of its nvdimm region is found in the labels of the device;
a writable `block` claim creates it when there is none
(`ndctl create-namespace -f -r regionK -m fsdax -M mem`), which writes
the labels. A read-only claim never creates a namespace or changes
labels, and the driver never creates a filesystem.

- `mode: block`: the container gets the block device `/dev/pmemK` of the
  fsdax namespace, read-only when `readOnly`. `CXL_<KEY>_PATH` is the
  device node, `CXL_<KEY>_SIZE` the size of the namespace. A shared
  device is writable only by a claim that has it alone, without a
  `shares` request.
- `mode: fs`: the container gets the filesystem of the namespace at
  `path` (default `/run/cxl-fs/<KEY>`), and no device node. The driver
  mounts it once per device and access on the node, the *node mount*,
  at `mounts/<serial hex>-<access>` in the data directory of the claim's
  driver, and every claim of the node gets a bind of it, read-only
  unless the claim says `readOnly: false`. `CXL_<KEY>_FS_TYPE` is the
  type of the filesystem and `CXL_<KEY>_ACCESS` the access.

`device.access` of mode `fs` says where the file pages are:

- `direct` mounts `/dev/pmemK` with `dax=always`: mappings of the files
  are the memory of the device, no page cache. It is the default of the
  types with DAX (erofs, ext4, xfs).
- `cached` mounts a direct-I/O loop device over `/dev/pmemK`: the files
  are read into the page cache of the node once. It is the default, and
  the only access, of the other types.

Claims of one device on a node may mix `block` and `fs` and both
accesses, within the rules of [One node](#one-node).

## Filesystems

The driver detects the type of the filesystem from the signatures in the
first 128 KiB of the block device. Each type has a class, which says who
may write it:

| type | class | DAX |
|:-----|:------|:----|
| `erofs` | read-only | yes |
| `squashfs` | read-only | no |
| `ext4` | single-host | yes |
| `xfs` | single-host | yes |
| `btrfs` | single-host | no |
| `gfs2` | cluster with lock protocol `lock_dlm` in its superblock, single-host with `lock_nolock` | no |
| any other type | single-host | no |

- *read-only*: never mounted read-write; any number of hosts read it.
- *single-host*: read-write on one host at a time; other hosts read it
  only while no host writes it ([Several nodes](#several-nodes)).
- *cluster*: read-write on several hosts at a time, on memory that is
  coherent between the hosts.

`device.fsType` is optional; with it the claim says what it expects. A
declared type must be the one detected, or one of them when a filesystem
was written over another one. A declared type that the driver does not
know is mounted as declared when the device has no signature (class
single-host, no DAX); without a signature and without such a type the
claim is refused.

The types that a node mounts are its `fsTypes`
([configuration file](usage.md#configuration-file)), by default `[erofs]`
only. Mounting a filesystem makes the kernel of the node parse metadata
that whoever wrote the device chose. erofs is designed for untrusted
images; squashfs, ext4, xfs, btrfs and gfs2 are not hardened against
crafted images. Whoever can write a device raw chooses what the kernels
of the nodes that mount it parse: a read-write `block` claim or lease of
it, on any node of any cluster that shares the pool, or a read-write
`fs` claim. So a type added to `fsTypes` trusts all of them, for example
`fsTypes: [erofs, ext4]` for the read-write samples. With the default no
read-write mount is possible. A type that `/proc/filesystems` marks
`nodev` (`tmpfs`, `nfs`, `overlay`) is never mounted.

The driver passes only the mount data of the access and the class:
`dax=always` or `dax=never` for the types with DAX, `nouuid` for a
read-only xfs (so that its direct and cached superblocks coexist), and
`spectator` for a read-only gfs2. It never passes `lockproto`,
`locktable`, `noload`, `norecovery` or options of the user: a
filesystem that needs recovery is not mounted read-only, and the lock
protocol of gfs2 is that of its superblock.

## One node

A *writer* of a device is a prepared claim (or lease) of the node that
writes it: mode `fs` with `readOnly: false`, or mode `block` that is not
read-only. Over the prepared claims of a device on the node:

- One superblock per device and access, read-write while the device has
  a writer on the node and read-only otherwise. Writers get read-write
  binds, readers read-only binds: the claims of one node share one
  consistent filesystem, and the readers see what the writers write. A
  write through a read-only bind fails with `Read-only file system`.
- With a writer the node mount is the only mount of the device on the
  node: a claim with the other access waits, and so does a writer while
  the other access is mounted. A writable `block` claim needs the device
  alone on the node.
- The block device (`/dev/pmemK`, and the loop device of `cached`) is
  read-only while the device has no writer on the node. On it nothing
  writes, also not a read-only mount (xfs writes its log at mount, ext4
  and xfs replay a dirty journal), and nobody, root and privileged
  containers included, can make the superblock writable. A filesystem
  that needs recovery is refused until a read-write claim recovers it.
- A writer that comes while read-only claims have the filesystem
  mounted remounts it read-write in place for ext4 and xfs; the binds of
  the readers stay read-only. For the other types the writer waits
  (kubelet retries its Prepare) until the read-only claims of the node
  are gone. When the last writer goes, the filesystem is synced and
  remounted read-only; a file still open for writing fails the
  Unprepare, and kubelet retries.
- A mount of the device that no claim of the node explains, such as one
  that a crashed container or an admin left, keeps writers back until it
  is gone.
- The last claim of the device on the node waits up to 10 s until
  nothing holds `/dev/pmemK` (no mount, no loop device, no container
  with the bind) before the region is released; else the Unprepare fails
  and kubelet retries. Releasing the region touches neither labels nor
  data, and the driver never zeroes a persistent device.

Read-only is enforced by the kernel for every process of the container,
except in privileged containers: they may remount their own bind
read-write, which writes while a writer of the node has the superblock
read-write. After a restart the driver mounts again the node mounts of
the prepared claims ([design.md](design.md#how-a-restart-recovers)).

The driver logs the mounts at verbosity 0, `pool|local` the source of
the device:

```
engine: pool memdev 0xc1ae0002: mounted /dev/pmem1 at <mnt> (erofs, ro, dax=always)
engine: pool memdev 0xc1ae0003: remounted <mnt> read-write
engine: pool memdev 0xc1ae0003: /dev/pmem2 read-only
engine: pool memdev 0xc1ae0002: unmounted <mnt>
```

## Several nodes

A single-host filesystem with a writer on one host and anybody else on
another corrupts or misleads. Two writers have two allocators and
journals of the same blocks. A reader next to a writer elsewhere sees
metadata halfway and keeps stale pages. So a read-write claim of a
single-host filesystem has the device on its node only, as a Kubernetes
volume of access mode ReadWriteOnce:

- The pool controller attaches the device to the writer's node alone and
  writes `singleHost: true` into the claim status
  ([pool.md](pool.md#what-the-pool-controller-writes)). The pool
  attaches it to no other host meanwhile, also not for another cluster
  that uses the pool. The claims of the writer's node share the
  attachment, read-write and read-only alike.
- Claims on other nodes wait: they get the condition
  `cxl-pool.nri.io/AttachFailed` with reason `Conflict`, and the
  scheduler allocates them again until the writing claims are
  deallocated. The scheduler does not prefer the writer's node: put the
  readers and writers of one device on one node with a `podAffinity` to
  the writer's pod (`topologyKey: kubernetes.io/hostname`) or a node
  selector, as
  [pool-persistent-fs-rw.yaml](blob:/deployment/samples/dra-driver-cxl/pool-persistent-fs-rw.yaml)
  does.
- The node mounts read-write only when the claim status says
  `singleHost`, or the device is not shared: a pool controller that does
  not implement this makes the writes fail, never corrupt.
- The attachment becomes shareable again after the writing claims are
  deallocated. Their Unprepare syncs the filesystem before that, so no
  other host sees the device before the data is in it. pmem writes go
  through the CPU caches to the memory. Linux writes back and
  invalidates the CPU caches of a CXL region when it creates and
  destroys the region. So host B reads what host A wrote, synced and
  released, after B creates its region.

A persistent local memdev that is not in `sharedMemdevs` is on one host
by construction: its claims write as the claims of one node do. A
memdev in `sharedMemdevs` (`shared: true`) may be used by other hosts
that no pool controller coordinates: it is never written in mode
`block`, and in mode `fs` only by a cluster filesystem on coherent
memory. Its readers see a consistent filesystem only while no other host
writes it; that is for the hosts to agree on.

A cluster filesystem is read-write on several hosts at a time when
everything agrees: the claim declares `fsType: gfs2`, the driver finds
gfs2 with lock protocol `lock_dlm` on the device, and the device is
coherent: the attribute `coherent` of a pool device
([pool.md](pool.md#what-the-pool-controller-publishes)),
`memdevSettings[].coherent` of a local memdev:

```yaml
sharedMemdevs: ["0xc100f5e2"]
memdevSettings:
- memdev: "0xc100f5e2"
  coherent: true   # the hardware keeps the memory coherent between the hosts
```

The pool controller then makes no single-host attachment. When two hosts
use the memory at the same time and one of them writes, the hardware must
keep the memory coherent between them (CXL shared memory with
back-invalidation). Without that, one host reads stale cache lines of the
other's writes, and a cluster filesystem is corrupted like a single-host
one. The cluster stack (corosync and `dlm_controld`) is the node
admin's; without it the mount fails.

## Filesystem images

A writer in mode `block` makes the filesystem: it writes an image into
the block device, for example with
`dd if=IMAGE of=$CXL_<KEY>_PATH bs=2M oflag=direct conv=fsync`, or runs
`mkfs` on it. The image must fit `CXL_<KEY>_SIZE`.

For `access: direct` the block size of ext4 and xfs must be the page
size: ext4 refuses `dax=always` with another one, and xfs mounts without
DAX, which the driver refuses. An ext4 image with the files of a
directory:

```bash
mkfs.ext4 -F -b 4096 -d DIR IMAGE SIZE
```

An erofs image for `access: direct` keeps each file in one plain,
2 MiB-aligned extent. Without `-Enoinline_data` the tail of a file is
inline in its inode, and the kernel silently uses the page cache for the
whole file although the mount says `dax=always`. Build it with exactly
(erofs-utils 1.8.6 or newer)

```bash
mkfs.erofs -b4096 --dsunit=512 -Enoinline_data IMAGE DIR
```

## Errors

A claim that the rules refuse fails its Prepare; kubelet retries, and the
pod event has the error. The errors of the configuration come first,
before anything is touched ([MemoryConfig](usage.md#errors)); the errors
of the device name its memdev:

```
cxl.nri.io: claim ns/writer: request mem: MemoryConfig: device.mode block: the device is writable only exclusively (no shares request)
cxl.nri.io: claim ns/data: device memdev-c100f5e1 (memdev 0xc100f5e1): memdev 0xc100f5e1: filesystem type ext4 is not allowed on this node (fsTypes: erofs)
```

The ones met most:

| error | |
|:------|:-|
| `no fsdax namespace in regionK: has a writer written the device?` | a reader of a device that no writer has written yet |
| `filesystem type <t> is not allowed on this node (fsTypes: ...)` | add the type to `fsTypes`, knowing what it trusts |
| `<dev> has no filesystem of a type that the driver knows (...): set device.fsType` | no signature found |
| `<t> is a read-only filesystem: set readOnly: true` | a read-write claim of erofs or squashfs |
| `<t> on <dev> needs recovery (it was not unmounted cleanly): ...` | a read-only claim; a read-write claim on one node recovers it |
| `<t> is mounted read-only on this node and cannot be remounted read-write: ...` | a writer of a type that is not remounted in place waits |
| `<dev> has a superblock that no claim of this node explains (...)` | a mount that the driver did not make; it waits until that is gone |
| `read-write on several hosts needs a cluster filesystem on coherent memory: <reason>` | a read-write claim without `singleHost` |
| `/dev/pmemK is still in use` | Unprepare: something still holds the block device |
