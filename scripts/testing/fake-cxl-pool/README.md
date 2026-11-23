# fake-cxl-pool

A fake CXL memory pool appliance for tests on one host:

- `fake-cxl-pool-server` runs on the host. It owns memory devices, whose
  memory is backing files in a directory, and hotplugs and hot-removes them
  as `cxl-type3` devices into running qemu VMs over their QMP or HMP
  monitor sockets.
- `fake-cxl-pool-client` (CLI) and `pkg/client` (Go) talk to the server's
  REST API. In a VM the client also prepares a hotplugged device for use
  and releases it before a detach (`guest` commands).
- `fake-cxl-pool-controller` is the pool controller of a Kubernetes
  cluster: it publishes the devices of the server for DRA (driver
  `cxl-pool.nri.io`) and attaches and detaches them as the scheduler
  allocates and frees claims.
- `cxl-numa-place where FILE...` prints on which NUMA nodes of the host the
  pages of backing files are.
- `cxl-load` streams writes or reads over buffers bound to the memory of
  some NUMA nodes from threads pinned to some CPUs, at a bandwidth that a
  pause per block sets. On the CPUs of the far socket it keeps the far
  memory busy, like a busy CXL link or device
  (`cxl-load -numa-nodes NODES -cpus CPUS -pause 10us`).

One server is one pool appliance. A device is *unshared* (attached to one
VM at a time, used as system RAM or as a dax device there) or *shared*
(the same backing file attached to several VMs, with the same serial in
each, used as a dax device only), and *volatile* or *persistent*
(persistent memory with a label storage area). The e2e suite and the demo
of the CXL DRA driver use it ([e2e.md](../../../docs/memory/cxl/e2e.md)).

## Build

```
make -C scripts/testing/fake-cxl-pool          # bin/: the server, client, controller, cxl-numa-place, cxl-load
make -C scripts/testing/fake-cxl-pool image    # the controller image, bin/fake-cxl-pool-controller-image.tar
```

The client and the controller are static amd64 binaries, so they can be
copied into the e2e VMs as they are.

## The server

```
bin/fake-cxl-pool-server [-config FILE] [-state FILE|-] [-v]
```

Without a configuration the server listens on 127.0.0.1:9909, keeps the
backing files in /tmp/fake-cxl-pool, discovers qemu processes every 10 s
and keeps its state in /tmp/fake-cxl-pool.state.json. `-v` logs every qemu
command and HTTP request. [config.example.yaml](config.example.yaml) has
every key with a comment: the `name` of the server (a DNS label, which the
controller publishes as the DRA pool name), `listen`, `dir`, the devices,
the serial bases, the discovery filters and the detach timeout. VMs with
slirp networking, as the e2e VMs, reach the host loopback at
`192.168.76.2`, which is the client's default server
(`http://192.168.76.2:9909`; `--server URL` or `$FAKE_CXL_POOL_SERVER`).

The guest finds a device by its serial
(`/sys/bus/cxl/devices/memN/serial`), so serials are unique over all
devices of the server. A device created without a serial gets the next
free one above the base of its kind: `0xc1ae0000` for shared devices
(`sharedSerialBase`), `0xc1ee0000` for unshared ones (`serialBase`). The
local devices of the e2e topologies are `0xc100e2e0` and up.

A persistent device has two files, the data file `<dir>/<device>.raw` and
a label storage area of 256 KiB, `<dir>/<device>.lsa`. The attach plugs
`cxl-type3` with `persistent-memdev=` and `lsa=` instead of
`volatile-memdev=`. Every qemu that attaches the device maps the same two
files (`share=on`), so a namespace created in one VM appears in the others
from the labels. The data backend of every device is 2 MiB-aligned, so
that the host can map it with 2 MiB pages where the filesystem has them
(a tmpfs with `huge=`).

### Discovery of qemu VMs

The server scans `/proc/*/cmdline` for `qemu-system-*` processes:

- The host name is the `<name>` of `.vagrant/machines/<name>/` in a
  `-drive` path, else `-name guest=<name>`; the uuid comes from `-uuid`.
- The monitor sockets come from `-qmp`, `-monitor` and `-chardev
  socket` with `-mon`. QMP is preferred: the server keeps one connection
  per VM, so it never misses a `DEVICE_DELETED` event (`qmp-e2e.sock` is
  the test framework's and is never used). HMP is used without a QMP
  socket or with `discovery.control: hmp`, a connection per command, with
  the deletion of devices polled.
- A qemu that restarted has none of the hotplugged devices: the server
  drops the attachments of its host. After every monitor call it checks
  that the qemu process behind the socket still runs, so that an attach
  during a restart plugs nothing into the new qemu.
- The pre-declared `memory-backend-ram` backends
  `beram_<memdev>__bus_<bus>__sn_<serial>` of the e2e topologies are
  *local* devices of their VM (`scope: local`): an attach plugs them into
  their own bus with their own serial. Only `scope: pool` devices are
  published by the controller.
- `hotRemoveCapable: false` means a qemu that cannot complete a
  hot-remove from a `cxl-downstream` port: the device stays in qemu as a
  zombie until the VM restarts. Detach and attach again need a qemu whose
  `cxl-downstream` ports have a power controller
  ([e2e.md](../../../docs/memory/cxl/e2e.md#requirements)).

`discovery.names` (path.Match patterns on VM names) and
`discovery.slots` (patterns on the bus names of the CXL slots) say what
the server is cabled to. Devices that someone else plugged with a known
backend are *adopted* as attachments.

## The client

`fake-cxl-pool-client --help` lists the commands:

```
fake-cxl-pool-client [--server URL] [-o table|json] COMMAND
  status | hosts [NAME] | whoami | devices [NAME] | attachments | events
  create --size 256Mi [--shared] [--persistent] [--name N] [--serial 0x..]
  delete NAME [--force]
  attach DEVICE (--self | --host HOST) [--slot BUS] [--numa-node N] [--owner O] [--timeout 30s] [--single-host]
  detach DEVICE (--self | --host HOST) [--timeout 30s] [--force]
  guest wait|memdev|region create|online|info|disable DEVICE
```

`--host` takes a host name or uuid; `--self` finds the VM that the client
runs in by its hostname and system uuid. `guest` commands run in the VM as
root and take a device name or a serial. `attach --single-host` makes a
single-host attachment: no other host can attach the device until it is
detached, also a shared device. Exit codes: 0 ok, 1 error, 2 usage, 3
conflict or timeout (the device is still held).

A shared device in two VMs, as a dax device:

```
fake-cxl-pool-client create --size 256Mi --shared --name shared0
fake-cxl-pool-client attach shared0 --host vm-a
fake-cxl-pool-client attach shared0 --host vm-b
# in each VM:
sudo fake-cxl-pool-client guest wait shared0
sudo fake-cxl-pool-client guest region create shared0 --mode dax   # /dev/daxN.M: mmap MAP_SHARED
sudo fake-cxl-pool-client guest disable shared0
fake-cxl-pool-client detach shared0 --self
```

Never online a shared device as system RAM in more than one VM: both
kernels would use the same memory. An unshared device as system RAM is
`guest region create pooled0 --mode numa` and `guest online pooled0
--movable`. Release in the guest before the detach: `device_del` makes the
guest remove the device whatever its state, and if its memory was still
online qemu can never finalize it. The server then reports the attachment
`failed`, keeps it listed and keeps an unshared device in state `error`, so
that it is not given to another VM; `detach --force` forgets it.

## The controller

The controller implements the contract of
[pool.md](../../../docs/memory/cxl/pool.md) for the server:

- It publishes the `scope: pool` devices of the server as cluster-scoped
  ResourceSlices of the pool named after the server (`-dra-pool-name`
  overrides it), with the node selector of the nodes whose VMs the server
  reaches. A node maps to a server host by its `systemUUID`, else by its
  name. While no node is reachable, nothing is published. The slices are
  deleted when the controller stops, and it touches the slices of its own
  pool only, so the controllers of several servers publish side by side.
- It attaches a device to the node of an allocated claim, and writes
  `cxl-pool.nri.io/Attached` and the device data into the claim status, or
  `AttachFailed` with the reason `NoHost`, `Conflict`, `Timeout`,
  `AttachError` or `InvalidConfig`.
- It makes a single-host attachment for a claim of all shares of a shared
  device and for a read-write `fs` claim of a single-host filesystem, and
  logs `attach D to H for claim ...: single host for a read-write <fsType>
  claim`.
- It detaches a device when no allocated claim wants it on that node any
  more.

The attachment owner is `k8s:resourceclaim/<claim uid>` (`-owner-prefix`).
The controller is stateless: it touches only the attachments of its owner
prefix on hosts that are nodes of its cluster, so a restart confirms the
existing attachments again. An attachment without an owner of a device
that a claim wants is taken over. Several clusters may share one server,
each with a controller of its own.

```
fake-cxl-pool-controller [-server http://192.168.76.2:9909] [-kubeconfig FILE]
    [-dra-pool-name ""] [-sync-interval 10s] [-attach-timeout 60s] [-detach-timeout 60s]
    [-owner-prefix k8s:] [-controller-key fake-cxl-pool] [-v]
```

The e2e tests run it on the control plane as a systemd unit; the demo runs
it as the Deployment of
[deploy/fake-cxl-pool-controller.yaml](deploy/fake-cxl-pool-controller.yaml),
with its ServiceAccount and RBAC, in the host network of a node, from the
image `localhost/fake-cxl-pool-controller:testing` of `make image`:

```
make -C scripts/testing/fake-cxl-pool image
scp -F test/e2e/<vm>/.ssh-config scripts/testing/fake-cxl-pool/bin/fake-cxl-pool-controller-image.tar node:
ssh -F test/e2e/<vm>/.ssh-config node sudo ctr -n k8s.io images import fake-cxl-pool-controller-image.tar
kubectl apply -f scripts/testing/fake-cxl-pool/deploy/fake-cxl-pool-controller.yaml
```

The settings of the controller in a claim are under its key
(`-controller-key`, default `fake-cxl-pool`) in `pool.opaque` of the
`MemoryConfig` of `cxl-pool.nri.io`:

| field | type | |
|:--|:--|:--|
| `slot` | string | an exact CXL slot (bus name) of the host, as `fake-cxl-pool-client hosts NAME` lists them |
| `numaNode` | integer >= 0 | prefer the free slots of host bridges on that NUMA node of the host |

The settings place a new attachment only: an attachment that exists
already stays in its slot, because the device may be in use. An invalid
object fails the result with `InvalidConfig`, a Warning event on the claim
and the log line `ERROR: claim <ns>/<name>: request <request> device
<device>: invalid configuration: <error>`.

## Several pool servers

A cluster that spans several appliances runs a server and a controller
per appliance; every controller publishes its own pool for the nodes that
its server reaches. Each server has a configuration of its own
([config.example.yaml](config.example.yaml), "Several pool servers"):

- a distinct `name`, `listen` port and `dir`, and the `discovery.names` of
  the VMs it is cabled to;
- serials unique over all servers: `sharedSerialBase: 0xc1aeP000` and
  `serialBase: 0xc1eeP000` for pool index P;
- device names unique over the servers cabled to one VM, because the qemu
  ids are `fcp_<device>.hp<N>`;
- `discovery.control: hmp` for a second server of a VM whose QMP socket
  another server holds, and distinct `discovery.slots` (`["*hb0"]`,
  `["*hb1"]`) for servers that share a VM.

## REST API

Base URL `http://HOST:9909/api/v1`, JSON; errors are
`{"error": "...", "code": "NotFound|Conflict|InvalidArgument|Unavailable|Internal"}`.
The routes are in [pkg/api/routes.go](pkg/api/routes.go):

```
GET    /status
GET    /hosts                     GET /hosts/{name|uuid}
GET    /hosts/resolve?hostname=H&uuid=U
GET    /devices?scope=
POST   /devices                   GET|DELETE /devices/{name}[?force=true]
POST   /devices/{name}/attachments                       {"host": ..., "slot", "numaNode", "owner", "timeout", "singleHost"}
DELETE /devices/{name}/attachments/{host}?timeout=&force=
GET    /attachments?host=&device=
GET    /events                    (text/event-stream)
```

Attach: 201 attached, 200 attached already, 202 still attaching at the
timeout, 409 a conflict of sharing, slot or capacity, 503 a qemu error.
Detach: 200 detached, 202 detaching, 409 the guest did not release the
device in time (the attachment is `failed`).

## Layout

```
cmd/fake-cxl-pool-server      the server
cmd/fake-cxl-pool-client      the CLI; its guest commands use pkg/cxl/kernel of nri-plugins
cmd/fake-cxl-pool-controller  the pool controller
cmd/cxl-numa-place, cxl-load  host tools of the demo
pkg/api                       JSON types, routes, errors, units of sizes and serials
pkg/client                    the Go client
pkg/controller                the ResourceSlice publisher and the attach and detach reconciler
pkg/server                    configuration, state, backing files, serials, HTTP handlers
pkg/qemu                      QMP and HMP monitors, discovery; pkg/qemu/fake for unit tests
deploy/                       the controller's Deployment and RBAC
Dockerfile                    the controller image (make image)
```
