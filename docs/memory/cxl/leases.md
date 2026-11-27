# Leases: memory for running containers

CAUTION: experimental. Interfaces may change.

The claims of a pod are fixed when it is created, so a running container
cannot get more memory through its own claims. It can *lease* memory
instead: the driver allocates a device for a *companion pod* and lends
it to the container, in one of the modes of claims:

- `numa`: a NUMA node added to the container's `cpuset.mems`;
- `dax`: an open file descriptor of a dax device;
- `block`: an open file descriptor of the block device of a device of
  persistent memory;
- `fs`: the filesystem of a device of persistent memory, mounted into
  the running container, read-only or read-write.

The memory is a slice of a local memdev, a shared or persistent local
memdev ([local.md](local.md)) or a pool device ([pool.md](pool.md)),
local first unless the request names a source.

- [Opting in](#opting-in)
- [Flow](#flow)
- [cxl-request](#cxl-request)
- [Protocol](#protocol)
- [Companion objects](#companion-objects)
- [Life cycle](#life-cycle)
- [Limitations](#limitations)

## Opting in

The driver publishes the device `lease` in the ResourceSlice of the
node (`--leases`, Helm value `leases.enabled`, on by default): attribute
`type: lease`, no capacity, `allowMultipleAllocations: true`, so any
number of pods can claim it. The DeviceClass `cxl-lease` selects it. A
pod that may lease memory claims it through a template, so that the
claim follows the pod
([lease.yaml](blob:/deployment/samples/dra-driver-cxl/lease.yaml)):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata: {name: cxl-lease}
spec: {spec: {devices: {requests: [{name: lease, exactly: {deviceClassName: cxl-lease}}]}}}
---
apiVersion: v1
kind: Pod
metadata: {name: requester}
spec:
  resourceClaims:
  - {name: lease, resourceClaimTemplateName: cxl-lease}
  containers:
  - name: app
    image: docker.io/library/python:3-alpine
    command: ["sleep", "infinity"]
    resources: {claims: [{name: lease}]}
```

The containers that list the claim get

- the socket directory of the claim at `/run/cxl` (read-only; the socket
  is `/run/cxl/cxl.sock`, in `CXL_LEASE_SOCKET`);
- the directory of the filesystems of `fs` leases at `/run/cxl/fs`
  (read-only; a mount that the driver makes there later appears in the
  running containers);
- the static `cxl-request` at `/usr/local/bin/cxl-request`.

Any pod whose namespace may have claims of the class `cxl-lease` may
lease memory. A cluster administrator limits this with a ResourceQuota
(`cxl-lease.deviceclass.resource.k8s.io/devices: 0` refuses such claims
in its namespace) or with the Helm value `leases.namespaceLabels`, which
admits such claims only in namespaces with those labels
([usage.md](usage.md#rbac-and-admission-policies)).

Leases need NRI: the driver lends NUMA nodes through NRI and maps the
processes that connect to its socket to their containers. Leasing local
memory needs no pool driver; with `--pool=false` a pool lease fails with
`pool driver disabled (--pool=false)`.

## Flow

```
container: cxl-request request --size 512Mi
  -> driver: authenticate the process, store lease L                (Creating)
  -> create ResourceClaim cxl-lease-L, then Pod cxl-lease-L          (Pending)
  -> the scheduler allocates a slice of a local memdev or, if none has
     room, a pool device (the pool controller attaches it)
  -> kubelet prepares cxl-lease-L: a slice or a region, online movable,
     NUMA node N                                                     (Lending)
  -> NRI UpdateContainers: cpuset.mems of THAT container = its own NUMA
     nodes without the CXL NUMA nodes of the driver + N
  -> response {"lease":"L","state":"Ready","numaNode":N,...}           (Ready)
container: cxl-request release --lease L
  -> revoke (cpuset.mems shrinks, the kernel migrates the pages back)
  -> delete Pod and ResourceClaim cxl-lease-L                     (Releasing)
  -> kubelet unprepares the companion claim                         (Released)
```

- `dax`: nothing is lent through cgroups. The driver opens `/dev/daxX.Y`
  (read-only for a read-only lease) and passes the open file descriptor
  to the container over the socket (SCM_RIGHTS). The container needs no
  device node and no device cgroup rule. `cxl-request request --exec CMD`
  runs CMD with the fd as fd 3. A read-only fd allows mappings for
  reading only. A writable mapping fails with EACCES, and a store raises
  SIGSEGV. The device cgroup refuses to open the fd again through
  `/proc/self/fd`.
- `block`: the fd model of `dax` applied to the block device `/dev/pmemK`
  of a persistent device. Without `--share` the lease has the whole
  device and may write it (the writer of an image); `--share --read-only`
  is a reader, and a share is never writable. The fd can be read,
  written and fsynced, but not given to a tool that wants a path.
- `fs`: the companion claim mounts the filesystem on the node, as a claim
  of mode `fs` does, and the driver binds it at
  `/var/lib/kubelet/plugins/cxl.nri.io/lease-fs/<claim uid>/<name>`, which
  the running containers of the lease claim see at `/run/cxl/fs/<name>`
  (`--name`, by default the lease id). It is read-only unless the request
  says `--read-only=false`. A lease and the claims of a device on a node
  share one node mount per access, under the rules of
  [persistent.md](persistent.md#one-node).

A shared device of volatile memory is `dax` only, a device of persistent
memory `block` or `fs` only. `--share` takes one share of a device with
a `shares` capacity: a shared device or a persistent local memdev. A
request that names such a device by `--serial` without `--share` takes
the whole device, the writer of single-writer sharing. An unshared local
memdev of volatile memory is never shared through a lease; several pods
share its slice through one ResourceClaim ([local.md](local.md#slices)).

The companion claim of a read-write lease is a claim like any other: a
read-write `fs` lease of a single-host filesystem on a pool device makes
the pool controller attach the device to the node alone
([persistent.md](persistent.md#several-nodes)). While another host has
the device the lease stays Creating until its `--wait` ends, and is then
released and Failed.

## cxl-request

`cxl-request` talks to `$CXL_LEASE_SOCKET`. The first word is the
operation; every other input is `--opt` or `--opt ARG` (`--opt=ARG`
too); `--exec` ends the options, the rest of the line is the command.

```
cxl-request request [--source local|pool] [--mode numa|dax|fs|block] [--share]
                    [--read-only[=false]] [--access direct|cached] [--fs-type TYPE]
                    [--name NAME] [--size SIZE] [--serial SERIAL] [--wait DURATION]
                    [--request-id ID] [--exec CMD [ARG...]]
cxl-request release (--lease ID | --all) [--wait DURATION]
cxl-request list
cxl-request env --lease ID [--wait DURATION] [--exec CMD [ARG...]]
cxl-request help
```

| operation | |
|:----------|:-|
| `request` | create a lease, wait until it is Ready, print the response; with `--exec` run CMD with the lease's environment (and its fd for `dax` and `block`, its mount for `fs`) |
| `release` | release a lease of the pod, or all of them, and wait until Released |
| `list` | print the leases of the pod |
| `env` | print the lease's environment as `KEY=VALUE` lines, or run CMD with it and the fd of a `dax` or `block` lease, for example after CMD restarted |

| option | values, default | |
|:-------|:----------------|:-|
| `--source` | `local`, `pool`; omitted: local first, then pool | which devices |
| `--mode` | `numa`, `dax`, `fs`, `block`; omitted: `numa`, or `dax` for a shared device | how the memory is given; `fs` and `block` are never a default |
| `--share` | off | one share of a device with shares (default: the whole device) |
| `--read-only` | `true` (the flag alone), `false`; default `false`, `true` for `fs` | `dax`, `block`: the fd allows reads only; `block` with `--share` needs it. `fs`: read-write with `--read-only=false` |
| `--access` | `direct`, `cached`; default `direct` when the filesystem has DAX | `fs`: file pages in the device or in the page cache of the node |
| `--fs-type` | lower-case letters, digits, `_` and `-`; default detected | `fs`: the expected filesystem type |
| `--name` | 1 to 255 letters, digits, `.`, `_`, `-`, not `.` or `..`; default the lease id | `fs`: the mount point `/run/cxl/fs/NAME`, unique among the leases of the lease claim |
| `--size` | quantity, for example `512Mi` | at least this much |
| `--serial` | `0x` hex or decimal | this device |
| `--wait` | Go duration, at most `1h`; omitted: the driver's `--lease-wait`; `0s`: do not wait | `request`, `env`: until Ready; `release`: until Released |
| `--request-id` | 1 to 64 letters, digits, `.`, `_`, `-` | the same id from the same container returns the same lease: repeat the whole command after a lost connection |
| `--lease` | 8 lower-case hex digits | the lease of `release` and `env` |
| `--all` | | `release`: every lease of the pod |
| `--exec` | CMD ARG... | run CMD with the lease's environment, and the fd of a `dax` or `block` lease as fd 3 |

A request needs `--size`, `--serial` or `--share`. `cxl-request` refuses
an invalid command line with a message and exit code 2 before anything
is sent, for example `--share` with `--mode numa`
(`shared memory is never onlined as RAM: use mode dax`) or `--access`
without `--mode fs` (`access needs mode fs`); the driver checks the
requests of other clients the same way. The driver refuses what only it
can judge with a JSON line with `error` on stdout (on stderr for `env`
and with `--exec`) and exit code 1, for example
`memdev 0x...: mode fs needs a persistent device`,
`pod <ns>/<name> has N leases, the limit of --lease-max-per-pod` or
`too many requests (--lease-requests-per-minute)`. The checks of a claim
of the device (the filesystem, the rules of one node, ReadWriteOnce) are
those of the companion claim: they fail its Prepare, and the lease stays
Creating until its `--wait` ends.

The driver checks a serial against the devices of the node's
ResourceSlice and its last list of the pool driver's ResourceSlices. A
serial of no such device, for instance of a local memdev of another
node, is not refused, but its companion claim cannot be allocated on
this node.

Output: `request`, `release` and `list` print the JSON response line of
the driver. `env` prints the variables of the lease
([usage.md](usage.md#environment-variables)) and `CXL_LEASE=<id>` as
sorted `KEY=VALUE` lines, so that `eval "$(cxl-request env --lease ID)"`
works. With `--exec`, CMD runs once the lease is Ready, with the
environment of `cxl-request` plus the variables of the lease:

- `dax` and `block`: the fd is fd 3 of CMD (`request --exec`,
  `env --exec`), and `CXL_<KEY>_FD=3` takes the place of `CXL_<KEY>_DAX`
  or `CXL_<KEY>_PATH`. `env --exec` asks the driver for the fd again, in
  the container that requested the lease only. `env` without `--exec`
  prints those two as the host path of the device, informational: the
  container has no such node.
- `numa`: `CXL_<KEY>_NUMA_NODE` and the rest.
- `fs`: `CXL_<KEY>_PATH=/run/cxl/fs/<name>`, `CXL_<KEY>_ACCESS`,
  `CXL_<KEY>_FS_TYPE`, `CXL_<KEY>_READ_ONLY` and the rest; the
  filesystem is mounted when CMD starts.

CMD's exit code is passed through (128+signal when a signal kills it),
and SIGTERM, SIGINT, SIGHUP and SIGQUIT to `cxl-request` are forwarded
to CMD. Exit codes: 0 ok; 1 error (no socket, connection, invalid
response, an error from the driver, CMD could not start); 2 usage; 3 the
lease is Failed, not Ready within `--wait` (`request`, `env`; with
`--exec` CMD is not started), or not Released within `--wait`
(`release`). Without `--exec`,
`request --wait 0s` exits with 0 whatever the state of the lease, unless
it is Failed. Without `$CXL_LEASE_SOCKET` it exits with 1 and
`CXL_LEASE_SOCKET is not set (the pod needs a claim on DeviceClass cxl-lease)`.

Examples, with the devices of the samples: the local memdev
`0xc100e2e1`, the shared local memdev `0xc100e2e2`, the unshared pool
device `0xc1ee0001`, the shared pool device `0xc1ae0001`, the persistent
pool devices `0xc1ae0002` (an erofs with `model.gguf`) and `0xc1ae0003`
(an ext4), and the persistent local memdev `0xc100f5e1`:

```
cxl-request request --size 512Mi
  -> {"lease":"1a2b3c4d","state":"Ready","source":"local","mode":"numa","share":false,"readOnly":false,
      "size":536870912,"serial":"0xc100e2e1","numaNode":2,"env":{"CXL_C100E2E1_MODE":"numa",...}}
cxl-request env --lease 1a2b3c4d --exec numactl --membind=2 prog   # CXL_C100E2E1_NUMA_NODE=2
cxl-request request --source pool --size 512Mi --mode dax --exec python3 app.py
  -> app.py sees CXL_C1EE0001_FD=3 CXL_C1EE0001_MODE=dax CXL_C1EE0001_SIZE=536870912
     CXL_C1EE0001_SERIAL=0xc1ee0001 CXL_C1EE0001_SHARED=0 CXL_C1EE0001_READ_ONLY=0 CXL_LEASE=1a2b3c4e
cxl-request env --lease 1a2b3c4e --wait 60s --exec python3 app.py  # the fd again after a restart of app.py
cxl-request request --serial 0xc1ae0001                         # the shared device alone (the writer)
cxl-request request --serial 0xc1ae0001 --share --read-only --exec prog   # a reader
cxl-request request --serial 0xc100e2e2 --share --read-only     # one read-only share of the shared local memdev
cxl-request request --share --size 256Mi                        # one share of any shared device of 256Mi or more
cxl-request request --size 1Gi --wait 0s --request-id r1         # asynchronous: Pending
cxl-request request --serial 0xc1ae0002 --mode block \
    --exec sh -c 'cat /tmp/model.erofs >&3'                      # the writer: an erofs image into the device
cxl-request request --serial 0xc1ae0002 --share --mode fs \
    --exec sh -c 'exec llama-server -m "$CXL_C1AE0002_PATH/model.gguf"'   # a reader, at /run/cxl/fs/<lease id>
cxl-request request --serial 0xc1ae0002 --share --mode fs --access cached --name models \
    --exec llama-server -m /run/cxl/fs/models/model.gguf         # cached, at a fixed path
cxl-request request --serial 0xc1ae0003 --share --mode fs --read-only=false --fs-type ext4 --name data \
    --exec sh -c 'date > /run/cxl/fs/data/stamp'                # read-write ext4 (fsTypes lists ext4)
cxl-request request --source local --serial 0xc100f5e1 --mode fs   # a persistent local memdev, read-only
cxl-request list
cxl-request release --lease 1a2b3c4d
cxl-request release --all
```

## Protocol

`SOCK_STREAM` unix socket, one JSON object per line in each direction; a
connection may carry several requests. The fields of a request mirror
the options of `cxl-request`; all are optional but `op`. An unknown
field, data after the object and an unknown `op` are refused
(`invalid request: json: unknown field "..."`).

| field | values |
|:------|:-------|
| `op` | `request`, `release`, `list`, `fd`, `env` |
| `source` | `local`, `pool`; omitted: local first, then pool |
| `mode` | `numa`, `dax`, `fs`, `block` |
| `share` | bool: one share of a device with shares |
| `readOnly` | bool; omitted: `true` for mode `fs`, else `false` |
| `fsType` | filesystem type; mode `fs` only |
| `access` | `direct`, `cached`; mode `fs` only |
| `name` | the mount point in `/run/cxl/fs`; mode `fs` only |
| `size` | quantity string |
| `serial` | string, `0x` hex or decimal |
| `wait` | Go duration string, at most `1h` |
| `requestID` | 1 to 64 letters, digits, `.`, `_`, `-` |
| `lease` | lease id: 8 lower-case hex digits |
| `all` | bool |

For example `{"op":"request","size":"512Mi","wait":"120s","requestID":"r1"}`,
`{"op":"request","serial":"0xc1ae0002","share":true,"mode":"fs","access":"cached","name":"models"}`
or `{"op":"release","all":true}`.

The response has `share` and `readOnly` always, the other fields only
when they have a value:

| field | |
|:------|:-|
| `lease` | id, 8 hex digits |
| `state` | `Creating`, `Pending`, `Lending`, `Ready`, `Releasing`, `Failed`, `Released` (in responses only: the lease is gone) |
| `source` | `local` or `pool`: the requested one, the allocated one once Ready |
| `mode` | `numa`, `dax`, `fs` or `block`, effective |
| `share` | bool: whether the lease took one share; `false` for a lease that has a shared device alone |
| `readOnly` | bool |
| `access` | `direct` or `cached`, mode `fs` |
| `fsType` | the type of the filesystem, mode `fs` once Ready |
| `path` | `/run/cxl/fs/<name>`, mode `fs` once Ready |
| `size` | bytes: once Ready the size of the slice or the device (in modes `fs` and `block` the size of the fsdax namespace), the requested size before |
| `serial` | `0x` hex, once known |
| `numaNode` | NUMA node, mode `numa` once Ready |
| `dax` | `/dev/daxX.Y`, mode `dax` once Ready; informational |
| `block` | `/dev/pmemK`, mode `block` once Ready; informational |
| `env` | once Ready: the variables that a claim of this memory would give |
| `error` | message |
| `leases` | list of responses, for `list` and `release` with `all` |

- The response of a Ready `dax` or `block` lease to `request` and `fd`
  carries the fd of its device as SCM_RIGHTS on the same write. A device
  that cannot be opened is answered with `opening <device>: <error>`,
  and the lease is kept.
- `wait` blocks until Ready or Failed (`release`: until Released). At the
  timeout of a `request` the lease is released and reported Failed
  (`timeout: not Ready after <wait>`); `fd` and `env` report the state at
  their timeout and keep the lease. `"wait":"0s"` returns the current
  state at once.
- A repeated request with the same `requestID` from the same container
  on the same socket returns the same lease.
- A release waits up to `wait` for the companion objects to disappear,
  and answers Released, or Releasing while they are still going.
- A lease belongs to the lease claim whose socket got its request, to
  the pod and to the container. `env`, `list` and `release` serve every
  container of the pod on that socket (`no such lease of this pod`);
  `fd` serves only the container that requested the lease
  (`no such lease of this container`). A lend that fails, for example to
  a steered container, makes the lease Failed with that message, and it
  is released.

The socket identifies the lease claim, and the claim's `reservedFor` the
pod. SO_PEERCRED gives the pid of the process that connects; the NRI
plugin maps it through `/proc/<pid>/cgroup` to a container, whose pod
must be a pod of the claim. Every request of a connection is
authenticated again while the process that connected runs; once it has
exited, the connection keeps the identity it had. The driver lends
memory to the requesting container only.

## Companion objects

The companion objects of lease `<id>` live in the requester's
namespace, with the labels `cxl.nri.io/lease=<id>`,
`cxl.nri.io/requester-uid=<pod uid>` and `cxl.nri.io/node=<node>`, and
the annotation `cxl.nri.io/requester=<ns>/<pod>/<container>`:

- ResourceClaim `cxl-lease-<id>`, owned by the requester pod, with one
  request `lease`. A `--serial` of a published device gives an `exactly`
  request of the class of that device; one source gives an `exactly`
  request of its class; else `firstAvailable` of the local class, then
  the pool class. A `--share` asks the shared classes only, modes
  `block` and `fs` the persistent classes only. A serial is a selector
  on `serial`, a size a capacity request (`memory`) on unshared devices
  and a selector on `size` on the classes of shared devices; a share is
  `shares: "1"`, and a shared device taken whole has no capacity
  request. A `MemoryConfig` of the claim says the effective `mode` and
  `readOnly` (and `access` and `fsType` when the request gave them), so
  that the claim shows what was asked. It has no `container` section; a
  preset of its class applies to it as to any claim
  ([usage.md](usage.md#presets-in-deviceclasses)).
- Pod `cxl-lease-<id>`, owned by the requester pod, pinned to the node,
  with the `priorityClassName` of the requester pod, one `pause`
  container (`--lease-companion-image`) with requests = limits of 100m
  CPU and 16Mi memory (Guaranteed), and the claim of its own name. It
  meets the `restricted` Pod Security profile (user and group 65535, a
  read-only root filesystem, no capabilities); an image of
  `--lease-companion-image` must work as that user. Both drivers give a
  companion claim no CDI device, so nothing of its memory reaches the
  pause container.

The driver deletes a companion only if its labels name the lease and
the node and its UID is the one the driver read; a release keeps any
other object of that name and counts it as gone. The admission policy
of the chart admits only such objects of the driver
([usage.md](usage.md#rbac-and-admission-policies)).

A claim with the label `cxl.nri.io/lease` is a companion claim. Before
it touches a device, the driver checks the claim against its lease:

- the lease is being created, or is Lending or Ready for a Prepare again
  after a restart;
- the name, namespace and owner of the claim are those of the lease;
- at the first Prepare, the spec of the claim is the one that the driver
  created.

A claim that carries the label and fails a check fails its Prepare with
`claim <ns>/<name>: not the companion of lease <id>`. A user who may
create claims can make a claim with the label only in their own
namespace, and cannot make it the companion of a lease that they did not
request.

## Life cycle

- The requester container stops: if the pod is being deleted, has
  finished, is gone or has `restartPolicy: Never`, its leases are
  released. Otherwise they are kept, and the restarted container gets
  the NUMA nodes of its `numa` leases again. A `dax` or `block` fd goes
  with the process: get it again with `cxl-request env --lease ID --exec ...`.
- The requester pod is deleted: the companion pods are garbage collected
  (owner references), and the Unprepare of the lease claim releases all
  leases of the pod. It fails with `still releasing N leases, retry`
  (kubelet retries) while their NUMA nodes are not revoked yet.
- A companion pod is deleted or evicted behind the driver's back: the
  driver of the device revokes the lend before it releases the device;
  the lease becomes Failed and is dropped.
- A restart of the driver: a request that waits for its lease is
  answered `the driver is stopping: lease <id> stays <state>, ask again`.
  The restarted driver creates the sockets of the lease claims again in
  the mounted directories, applies the `numa` lends again, and binds the
  binds of `fs` leases again: the leases continue.
- A reboot of the node: a lease does not survive it (the fds went with
  their processes, the binds are unmounted, the memory of `numa` leases
  is gone). The restarted requester containers have no leases and ask
  again; the lease claims stay, and kubelet prepares them again.
- An `fs` lease is not released while the requester has files of the
  filesystem open or mapped (a running `llama-server` maps its model):
  the unmount fails with `device or resource busy`, kubelet retries, the
  companion stays Terminating and the lease Releasing;
  `cxl-request release --wait ...` exits with 3. Close the files (stop
  CMD) before releasing, or let the pod end.
- A `block` lease is released also under a container that still holds
  the fd: I/O through it fails from then on (ENXIO, EIO). Close the fd
  before releasing.

[design.md](design.md#the-life-of-a-lease) tells how the driver keeps
the leases, their directories and binds across these events.

## Limitations

- Lent memory counts against the container's `memory.max` and the pod
  cgroup: without a raise the extra NUMA node adds locality, not room.
  Use requester pods without a memory limit, or raise it with
  `pods/resize`. Node allocatable never includes pool memory.
- `cpuset.mems` has one owner. The lend goes through NRI
  UpdateContainers, as does keeping the CXL NUMA nodes of the driver
  away from every container that is not steered
  ([usage.md](usage.md#containers-without-claims)). A steered container
  cannot take a `numa` lease: the lend is refused with
  `lend NUMA node N to <container>: container is memory-steered (...)`,
  and the lease becomes Failed. A container that is not steered may,
  also with `numa` claims of its own. `dax`, `block` and `fs` leases lend
  no NUMA node and are not affected. Next to an NRI resource policy that
  sets `cpuset.mems`, `defaults.container.fencing` must be `none`.
- A NUMA node is lent per lease: memdevs under one CXL host bridge share
  a NUMA node, and when two leases of a container hold the same NUMA
  node, it stays in the container's `cpuset.mems` until the last of them
  is released.
- Release is cooperative. Releasing a `dax` lease while the device is
  mapped makes the holder get SIGBUS at its next access: munmap first. A
  passed fd cannot be revoked short of the release, and can be
  duplicated or passed on inside the pod. A `numa` release blocks
  (kubelet retries, the companion stays Terminating) while the pages
  cannot move.
- The filesystem of an `fs` lease is visible in every container of the
  pods of the lease claim that lists the claim, not only in the
  requesting one. User-namespaced pods are untested with `fs` leases.
- Block leases pass an fd, not a path: a tool that writes to a path,
  such as `mkfs.erofs`, writes a file that is then copied through the
  fd.
- A read-write `fs` lease on another node than the writers of the device
  waits for them, and readers on other nodes wait for it: companions run
  on the requester's node.
- A pool lease waits for the attach of the device and the binding
  condition, which the scheduler polls; a local lease waits for neither.
  A release takes longer than a request.
- Every lease is a pod on the requester's node. Size the ResourceQuota
  of the namespaces and the kubelet's `maxPods` for these pods.
  `--lease-max-per-pod` and `--lease-max-per-node` bound the leases
  ([usage.md](usage.md#flags)). Companions count against the
  ResourceQuota of the requester's namespace.
- A companion pod has the priority class of its requester. The chart's
  admission policy refuses the companion of a requester of the class
  `system-node-critical` or `system-cluster-critical`, or of a class not
  in `leases.priorityClassNames` when that list is set; the lease fails.
- SELinux labels of the socket are set best effort and unverified.
- Leases follow the pod object: a finished but undeleted pod keeps its
  leases until its lease claim is unprepared.
- Do not force-delete a companion pod while it holds a device
  ([pool.md](pool.md#force-deletion)).
