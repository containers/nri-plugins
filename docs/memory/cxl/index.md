# CXL DRA driver

CAUTION: experimental, do not use on production clusters. Interfaces may
change.

## About the driver

`nri-dra-driver-cxl` gives pods CXL memory: memory devices of the node
and devices of a CXL memory pool, as system RAM of a NUMA node, as a dax
device, or, of persistent memory, as a block device or a filesystem. One
process runs on every node and has four parts:

- `cxl.nri.io`, a DRA driver: it publishes the memory of the node in the
  node's ResourceSlice (local memdevs, regions that exist already, the
  DRAM and the lease device) and prepares the claims on it.
- `cxl-pool.nri.io`, a DRA driver: it prepares the claims on pool devices.
  A pool controller, one per pool product, publishes the pool devices and
  attaches them to the node of the claim ([pool.md](pool.md)).
- an NRI plugin (`dra-driver-cxl`, index 30): it keeps containers off the
  CXL memory they did not claim, steers the memory of the containers that
  opt in (unsafe for threaded programs, [Steering](usage.md#steering)),
  and lends memory to running containers.
- the lease broker: running containers ask it for memory with
  `cxl-request` ([leases.md](leases.md)).

The driver needs Kubernetes v1.37 or later and a kernel with CXL and DAX
support, see [Requirements](usage.md#requirements).

## Vocabulary

The same words name the same thing in device attributes, DeviceClasses,
configurations, environment variables and the lease protocol.

| word | meaning |
|:-----|:--------|
| memdev | a CXL memory device, `/sys/bus/cxl/devices/memN`, named by its `serial` |
| local memdev | a memdev of the node, whose regions `cxl.nri.io` creates on demand |
| pool device | a device of a memory pool; its memdev appears on the node that the pool controller attaches it to |
| slice | what one claim or lease gets of an unshared local memdev: a dax device of its own on whole memory blocks |
| region | a CXL region; `type: region` is one that existed before the driver, online as a NUMA node |
| `source` | `local` (a local memdev, prepared by `cxl.nri.io`) or `pool` (a pool device, prepared by `cxl-pool.nri.io`) |
| `type` | what a published device is: `memdev`, `region`, `dram` or `lease` |
| `shared` | the same bytes may be mapped by several claims at a time; such memory is never onlined as RAM |
| `shares` | the capacity of a shared device: no request takes the whole device, `shares: "1"` one share |
| `persistent` | persistent memory, whose data and namespace labels outlive the claims; used in modes `block` and `fs` only |
| `coherent` | the hardware keeps the memory coherent between the hosts that use it; only then a cluster filesystem is read-write on several hosts |
| `mode` | `numa` (RAM of a NUMA node, the default), `dax` (a dax device, or its fd for a lease), `block` (the block device of the fsdax namespace), `fs` (a filesystem on it) |
| `readOnly` | the device or the fd allows reads only; `fs` is read-only unless `readOnly: false` |
| `access` | mode `fs`: `direct` (file pages are the memory of the device) or `cached` (the page cache of the node) |
| `fsType` | mode `fs`: the type of the filesystem; the node mounts erofs only unless its `fsTypes` lists more ([persistent.md](persistent.md)) |
| `serial` | `0x` and lower-case hex, the identity of a memdev in both drivers; upper-case hex without `0x` in environment variable names |
| `size` | bytes in attributes and environment variables; requests take a quantity, "at least this much" |
| lease | a request of a running container for memory, served through the lease socket from a companion claim |

## DeviceClasses

The Helm chart installs nine DeviceClasses
([`templates/deviceclasses.yaml`](blob:/deployment/helm/dra-driver-cxl/templates/deviceclasses.yaml)).
A class selects a kind of device; a `MemoryConfig` of the claim, or of
the class as a preset, says how it is prepared
([MemoryConfig](usage.md#memoryconfig)).

| class | devices | a claim gets |
|:------|:--------|:-------------|
| `cxl-local` | local memdevs, not shared, not persistent | a slice: RAM of its NUMA node, or a dax device (`mode: dax`) |
| `cxl-local-shared` | local memdevs in `sharedMemdevs` | the dax device of the whole memdev: alone, or one share (`shares: "1"`) |
| `cxl-local-persistent` | local memdevs of persistent memory | the block device (`mode: block`) or a filesystem on it (`mode: fs`) |
| `cxl-pool` | pool devices, not shared, not persistent | the whole device: a new NUMA node, or a dax device (`mode: dax`) |
| `cxl-pool-shared` | shared pool devices of volatile memory | a dax device: alone, or one share (`shares: "1"`) |
| `cxl-pool-persistent` | pool devices of persistent memory | the block device or a filesystem on it, on one node or several |
| `cxl-region` | regions that existed before the driver | capacity of their NUMA node, for steering |
| `cxl-dram` | the DRAM of the node | capacity of the DRAM, for steering |
| `cxl-lease` | the lease device | the lease socket and `cxl-request` in the container |

## Documentation

- [Deploy and configure](usage.md): requirements, installation,
  devices, MemoryConfig, steering, environment variables, flags, the
  configuration file, operations
- [Local CXL memory](local.md): local memdevs, slices, shared and
  persistent local memdevs
- [CXL memory pools](pool.md): pool devices and the contract of a pool
  controller
- [Persistent memory](persistent.md): modes `block` and `fs`,
  filesystems, one node and several
- [Leases](leases.md): memory for running containers, `cxl-request` and
  its protocol
- [Helm chart of the driver](../../deployment/helm/dra-driver-cxl.md)

For developers:

- [Design of the driver](design.md): its objects, how a claim becomes
  memory, how a restart recovers, its files on the node
- [States of the driver's records](states.md): every state field and
  its transitions, generated from the code
- [Developer's guide](e2e.md): building, unit tests and the e2e suite on
  qemu VMs with a fake CXL memory pool

```{toctree}
:maxdepth: 1
:hidden:

usage.md
local.md
pool.md
persistent.md
leases.md
design.md
states.md
e2e.md
```
