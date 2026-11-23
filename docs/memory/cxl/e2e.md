# Developer's guide

This page tells how to build the CXL DRA driver, run its unit tests, and
run its e2e suite: three qemu VMs with emulated CXL memory devices, one
Kubernetes cluster, and a fake CXL memory pool on the host. The user
pages ([index.md](index.md)) tell what the driver does;
[design.md](design.md) and [states.md](states.md) how it does it.

## Build

From the root of the repository:

```bash
make PLUGINS=nri-dra-driver-cxl OTHER_IMAGE_TARGETS= BINARIES= images
make -C scripts/testing/fake-cxl-pool
```

The first builds the image of the driver as
`build/images/nri-dra-driver-cxl-image-<id>.tar` (`SKIP_LICENSES=1`
leaves the licenses of the driver out, for a quicker local build). The
second builds the fake pool's server, client and controller into
`scripts/testing/fake-cxl-pool/bin`. The e2e suite takes the newest
image tar: rebuild it after every change of the driver.

## Unit tests

```bash
go test ./cmd/plugins/dra-driver-cxl/... ./pkg/cxl/... ./cmd/cxl-request/...
go test ./scripts/testing/fake-cxl-pool/...
(cd test/e2e/memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/test90-mbt-stress/model &&
    GOWORK=off go test ./...)
```

`-short` leaves out the long model tests of the engine. The model of
test90 is a module of its own (gofmbt). `go generate ./generate.go` in
`cmd/plugins/dra-driver-cxl` renders [states.md](states.md) from the
state tables; a test fails while the file is stale, and another one
while a link of design.md into states.md names no heading.

## The e2e setup

The suite is
[`test/e2e/memory.test-suite/dra-driver-cxl`](tree:/test/e2e/memory.test-suite/dra-driver-cxl).
Its three VMs, of the topologies `n4-cxl-shared-0`, `-1` and `-2`, are
the three nodes of one cluster: VM2 is the control plane, without the
control-plane taint, and VM1 and VM0 are workers. The VM name is the
host name and the node name, for instance
`n4-cxl-shared-2-fedora-43-containerd`.

```
 host
 +--------------------------------------------------------------------------+
 | fake-cxl-pool-server 127.0.0.1:9909 (pool A), cabled to VM1 and VM2      |
 |   pool devices: backing files /tmp/fake-cxl-pool/<device>.raw            |
 |   hotplugs and hot-removes cxl-type3 devices over QMP                    |
 |                                                                          |
 |  qemu: VM0               qemu: VM1               qemu: VM2               |
 |  +-------------------+   +-------------------+   +-------------------+   |
 |  | worker            |   | worker            |   | control plane     |   |
 |  | nri-dra-driver-cxl|   | nri-dra-driver-cxl|   | nri-dra-driver-cxl|   |
 |  |                   |   |                   |   | fake-cxl-pool-    |   |
 |  |                   |   |                   |   | controller        |   |
 |  +-------------------+   +-------------------+   +-------------------+   |
 |        +------- VM network cxl-pool (10.76.x.y) -------+                 |
 +--------------------------------------------------------------------------+
```

Each VM has two NUMA nodes of DRAM, a memory block size of 128 MiB, and
two CXL host bridges with a CXL switch of four ports each. The ports
hold empty pool slots, where the fake pool server hotplugs the devices
of the pool, and a local device `cxl_memdev0` (serial `0xc100e2e1`),
which the server attaches to its own VM only. The VMs reach the host at
`192.168.76.2`, so the pool controller on VM2 talks to the server on the
host. A pool server is one pool appliance: pool A is cabled to VM1 and
VM2, and its ResourceSlice lists those nodes. `test19-many-pools` adds
pools B and C, `test21-dra-local-persistent` a server of a persistent
local memdev. [The fake pool's README](tree:/scripts/testing/fake-cxl-pool)
tells its server, client and controller.

### Requirements

- What the e2e tests need on the host: qemu, vagrant, ansible, docker,
  go, python3 and curl ([`test/e2e`](tree:/test/e2e)).
- A qemu that can hot-remove CXL memory devices: its `cxl-downstream`
  ports have a power controller (QOM property
  `power_controller_present`), and its `cxl-type3` device unmaps its
  memory backend on unplug. Stock qemu plugs a device into a running VM
  but cannot complete its removal from a switch port: the device stays
  as a zombie until the VM restarts. Give the qemu binary with
  `qemu_bin` when the VMs are created; the framework records it in the
  `env` file of the VM's output directory.
- The CXL guest kernel of
  [`qemu-cxl-kernel.config`](blob:/test/e2e/playbook/files/qemu-cxl-kernel.config)
  (CXL regions, dax, kmem, memory hot-remove, `CONFIG_FS_DAX`, the
  filesystems of mode `fs`, DMI). Provisioning builds it inside the VM
  when the kernel cache (`~/.cache/nri-plugins/e2e`) has no RPMs of
  that configuration ([`custom-kernel.yaml`](blob:/test/e2e/playbook/custom-kernel.yaml)),
  which takes long. `test00-up` fails with the name of the expected
  tarball when the VM runs another kernel.
- `test16-cxl-llm-share`: huggingface.co through `http_proxy` and
  `https_proxy` for the first download of the model (or a copy with
  `llm_model=PATH`); the VMs pull a llama.cpp server image and install
  `erofs-utils`.
- `test20-dra-fs-read-write` and `test21-dra-local-persistent`: the VMs
  install the tools of the filesystems that they build.

### Create the VMs

`test00-up` of each topology creates its VM, installs the kernel and the
`cxl` and `daxctl` tools, and checks the qemu devices.
`n4-cxl-shared-2/test00-up` then makes the cluster. Create VM1 and VM0
first:

```bash
cd test/e2e
for t in n4-cxl-shared-1 n4-cxl-shared-0 n4-cxl-shared-2; do
    qemu_bin=/path/to/qemu-system-x86_64 \
        ./run_tests.sh memory.test-suite/dra-driver-cxl/$t/test00-up
done
```

The cluster stays that way: the other tests check its shape and fail at
once, pointing at `n4-cxl-shared-2/test00-up`, when it is not there. The
VMs run the latest Kubernetes release with its default feature gates;
`k8s_feature_gates=DRANodeAllocatableResources=true` at the creation
turns that gate on. Log in to a VM with the ssh configuration of its
output directory:

```bash
ssh -F test/e2e/n4-cxl-shared-2-fedora-43-containerd/.ssh-config node
```

## Run the tests

One test at a time, from `test/e2e`, or the whole suite in order:

```bash
./run_tests.sh memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/test10-dra-pool
./run_tests.sh memory.test-suite/dra-driver-cxl
```

Each test installs the driver with the chart on every node, runs the
fake pool server on the host and the controller on VM2, and cleans up
after itself. The output, with the logs of the driver of every node, is
in `test/e2e/n4-cxl-shared-2-fedora-43-containerd/memory.test-suite/dra-driver-cxl/<test>/`.
To look around in the cluster with the driver installed, put
`breakpoint` into the `code.var.sh` of a test where you want to stop:
the test reads commands from the terminal until `exit`.

| test | |
|:-----|:-|
| `test00-up` | create and check the VM of the topology; `n4-cxl-shared-2/test00-up` makes the cluster |
| `test01-fake-pool` | CXL memory shared by VM1 and VM2 through the fake pool, without Kubernetes |
| `test10-dra-pool` | unshared pool devices as NUMA nodes and dax devices; the pool's slice for the nodes it reaches |
| `test12-dra-pool-leases` | leases of pool memory; the directories of a lease claim; refused lease requests |
| `test13-dra-shared-one-writer-many-readers` | single-writer sharing of a shared pool device on two nodes |
| `test14-dra-local` | slices of a local memdev, `numa` and `dax` |
| `test15-dra-local-leases` | leases of local memory |
| `test16-cxl-llm-share` | a language model in an erofs on a persistent pool device, read by llama.cpp servers on two nodes, direct and cached, from claims and leases |
| `test17-dra-local-shared` | a shared local memdev: writer and readers, kept bytes, restarts, leases |
| `test19-many-pools` | three pools in one cluster: their slices, scheduling by reach, data shared across racks |
| `test20-dra-fs-read-write` | filesystems of every type on a persistent pool device: ReadWriteOnce, remounts, access cached, recovery, refusals, fs leases |
| `test21-dra-local-persistent` | a persistent local memdev: a block writer, `fsTypes`, read-write and read-only claims, an fs lease |
| `test22-dra-admission-policy` | the admission policies of the chart, rule by rule |
| `test90-mbt-stress` | model-based stress (gofmbt): pods, claims, leases, writes and faults chosen by a model, the cluster checked after each step; a long test, run with `skip_long_tests=no` |

The tests need the ports 9909 to 9912 of the host for their pool
servers. `test90-mbt-stress` takes its parameters (`MBT_DURATION`,
`MBT_PROFILE`, `MBT_FENCING` and others) from the environment; its
`code.var.sh` lists them. Its model and its executor follow the
[contract](blob:/test/e2e/memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/test90-mbt-stress/contract.txt):
the kinds of pods, the actions, the protocol, the observations, the
oracle and the rules A1 to A44 that the code cites.

The tests on real hardware are in
[`test/hw/dra-driver-cxl-local`](tree:/test/hw/dra-driver-cxl-local).

## Troubleshooting

- The log of the driver on a node:
  `kubectl logs -n kube-system $(kubectl get pods -n kube-system -l app.kubernetes.io/name=nri-dra-driver-cxl --field-selector spec.nodeName=NODE -o name)`.
  The tests install the chart at verbosity 4.
- The pool controller: `sudo journalctl -u 'fake-cxl-pool-controller*'`
  in VM2. The server logs into the output directory of the test.
- A pod that stays `Pending` or `ContainerCreating`:
  `kubectl describe pod NAME` (scheduling, `BindingConditionsPending`,
  errors of NodePrepareResources) and `kubectl describe resourceclaim NAME`
  (the allocation, the `Attached` or `AttachFailed` condition). A pod
  with a pool claim on a node that no pool with a free device reaches
  stays Pending with "0/3 nodes are available".
- Pin a pod with a `nodeSelector` on `kubernetes.io/hostname`, never with
  `nodeName`: it skips the scheduler, and with it the allocation of the
  claims.
- The CXL devices of a VM: `sudo cxl list -M -R`, `sudo daxctl list`,
  `cat /sys/devices/system/node/online`. The records of the driver are in
  `/var/lib/kubelet/plugins/cxl.nri.io/state.json`
  ([Files on the node](usage.md#files-on-the-node)).
- A device that stays in a VM after a force-deleted pod, or a zombie of
  a stock qemu: the tests of `n4-cxl-shared-2` from `test10` on restart
  a VM that has CXL devices at their start. Restarting VM2 makes the API
  server of the cluster unavailable for a while.
- `run_tests.sh` fails with `port 9909 is busy`: another pool server
  runs on the host; stop it.
