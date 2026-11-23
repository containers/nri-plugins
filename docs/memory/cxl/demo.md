# The CXL demo

CAUTION: experimental. The demo runs on emulated hardware only: qemu VMs
and a fake memory pool.

The demo tells several stories. In each one, CXL memory from a virtual
memory pool is attached to virtual machines and detached from them
again. A single Linux host is enough to run it, because the VMs and the
pool are emulated, so you need no CXL hardware.

You will see containers that share the same CXL memory, a scheduler
that decides who may share a device and who has to wait, and readers
that cannot write to the memory they share. The demo is for studying,
trying out and debugging how the CXL DRA driver works, and you can
present stories it to others, too. This page explains how to prepare a
host, how to run each story and look inside every layer, and how to
read the results of the language model story.

- [The stories](#the-stories)
- [What runs where](#what-runs-where)
- [Preparation](#preparation)
- [The presenter's screen](#the-presenters-screen)
- [Setup and teardown](#setup-and-teardown)
- [local and pool](#local-and-pool)
- [pool-shared-fs](#pool-shared-fs)
- [pool-fs](#pool-fs)
- [The wizard](#the-wizard)
- [llm-share](#llm-share)
- [The latency variant](#the-latency-variant)
- [Look inside](#look-inside)
- [Read the results](#read-the-results)
- [Troubleshooting](#troubleshooting)
- [The demo directory](#the-demo-directory)

## The stories

Five stories are scripts, and each one has a writer and readers. A
sixth, the wizard, covers the rest:

| story | what it shows |
|:------|:--------------|
| `local` | four pods on VM2 share VM2's own CXL memory device (`sharedMemdevs`): first a writer that has the device alone, then three read-only readers |
| `pool` | the same story with the shared pool device `shared0`: the writer runs on VM1, and the readers on VM2 and VM1. The driver hot-plugs the device into the nodes when they need it |
| `pool-shared-fs` | `shared0` as a persistent device: the writer puts an erofs with `hello.txt` on it as a block device, and the readers get the filesystem mounted into their running containers |
| `pool-fs` | the persistent `fs0`: the writer makes an ext4 (or xfs, btrfs) and mounts it read-write. The reader on the writer's node shares it at once, and the readers on the other node wait until the writer leaves (ReadWriteOnce) |
| `wizard` | every kind of device that the driver offers, and any claim or lease for it that the driver accepts: you see its YAML, and you create and delete it |
| `llm-share` | a language model on the persistent pool device `llm0`, shared by all three VMs: a writer stores an erofs image of the model, one llama.cpp server per VM serves it, and a client measures the time to the first token and per token, first for one server alone and then for all at once |

`llm-share` answers three questions:

- Is the model in the host memory only once? With direct access (DAX) it
  is, because all three VMs map the same pages of one file on the host.
- What changes when the pool memory is slower, as real CXL memory is? On
  a host with two sockets, the [latency variant](#the-latency-variant)
  puts the VMs on socket 0 and the device on socket 1.
- Does a page cache in each VM make inference faster, and what does it
  cost? The readers with cached access show it.

## What runs where

Everything except the pool "hardware" runs in containers, as in a
cluster with a pool appliance:

| part | where it runs |
|:-----|:--------------|
| the pool server, `fake-cxl-pool-server`, which plays the pool hardware | a process on the host |
| the three VMs | qemu processes on the host |
| Kubernetes | in the VMs: VM2 is the control plane, VM0 and VM1 are workers |
| `nri-dra-driver-cxl` | a DaemonSet pod on every node |
| the pool controller, `fake-cxl-pool-controller` | a Deployment pod, namespace `fake-cxl-pool` |
| writers, readers, the client | pods, namespace `cxl-demo` |
| the model mirror (only if you ask for one) | a Python web server on the host, which stands in for a model registry outside the cluster |
| the latency setup, the views of host memory | the host |

A pool device is a file on the host. Every qemu that attaches the device
maps this file (`share=on`), so all VMs map the same file. The
filesystem, the driver and the pods do not know that the hardware is
fake. In the scripted stories the pool is cabled to VM1 and VM2. In
`llm-share` it is cabled to all three VMs. To log in to the VMs and to
read the objects, the qemu monitor and the pool server directly, see
[Look inside](#look-inside).

## Preparation

### The host

The demo ran on two-socket x86 servers with several NUMA nodes per
socket. The sizes below are estimates from what the VMs use, and smaller
hosts were not tried.

| need | |
|:-----|:-|
| a 64-bit x86 Linux host with KVM | `test -r /dev/kvm -a -w /dev/kvm && echo ok` |
| memory | 32 GiB or more for three VMs of 8 GiB, the model and the host's page cache |
| hardware threads | 24 or more for three VMs of 8 vCPUs. A reader uses 6 threads, and with fewer threads the "all at once" runs get worse |
| disk | 100 GiB free for the sparse VM disks, the images and the kernel cache |
| tools | what the e2e suite needs ([e2e.md](e2e.md#requirements)): vagrant with the plugins `vagrant-qemu`, `vagrant-proxyconf` and `dotenv`, ansible with `ansible.posix`, docker usable without sudo, go, python3, git, make, rsync, curl, ssh, `qemu-img` |
| sudo without a password | to mount the tmpfs for the pool memory (without sudo, setup falls back to `/dev/shm`), and for the latency variant |

The qemu, the guest kernel and the three VMs are the ones that the e2e
suite uses. [e2e.md](e2e.md#the-e2e-setup) explains their requirements
and how to create them. Pin the VM releases in every shell that runs the
e2e framework, that is, for the three `test00-up` runs and for `demo
setup`, `demo llm-share --auto` and `demo teardown`:

```bash
export k8s_release=1.37.2 helm_release=4.3.1 containerd_release=2.4.2
```

The framework refuses a VM that was created with other versions, unless
you set `e2e_vm_versions=ignore`. The model comes from huggingface.co
and the llama.cpp image from ghcr.io. If you use a proxy, set
`http_proxy`, `https_proxy` and `no_proxy` for the e2e framework, and
set docker's proxy in `~/.docker/config.json`. Keep
`~/.cache/nri-plugins/e2e` and `~/.vagrant.d/boxes`, because they save
the downloads and the kernel build.

### Images and the demo command

```bash
make PLUGINS=nri-dra-driver-cxl OTHER_IMAGE_TARGETS= BINARIES= images
make -C scripts/testing/fake-cxl-pool
```

The first command builds the driver image, and `demo setup` imports the
newest one into the VMs. The second builds the pool binaries,
`cxl-numa-place` and the load program `cxl-load`. Setup builds the rest
itself: the pool controller image and the demo image
`localhost/cxl-demo-tools`. The demo image is Python on Alpine with
erofs-utils, e2fsprogs, xfsprogs and btrfs-progs, and it is built from
the `image/Dockerfile` in the demo directory. The VMs pull the llama.cpp
image themselves.

The demo directory is
[`test/e2e/memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/demo01-share-write-read`](tree:/test/e2e/memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/demo01-share-write-read),
and its `bin/demo` is the entry point:

```bash
export N=/path/to/nri-plugins
export D01=$N/test/e2e/memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/demo01-share-write-read
demo() { "$D01"/bin/demo "$@"; }
demo selftest
```

`demo selftest` needs no VM. It renders every view from recorded samples
and checks that each view fits a terminal of 50 x 10. It also runs the
wizard's checks. In addition, it runs the host scripts (`pool-tmpfs.sh`,
`latency.sh`) against stubs of `sudo`, `mount` and `taskset` and
against a fake `/sys`.

### The model

By default, `llm-share` uses Qwen3.5-2B in 4-bit form (`Q4_K_M`) from
the unsloth repository on Hugging Face. It is a small model with native
tool calling, and its licence is Apache-2.0. Setup pins each model to a
commit of its repository and checks its sha256:

| `DEMO_LLM_MODEL` | file | size | `llm0` |
|:-----------------|:-----|:-----|:-------|
| `qwen3.5-2b` (default) | `Qwen3.5-2B-Q4_K_M.gguf` | 1221 MiB | 1536Mi |
| `qwen3.5-0.8b` | `Qwen3.5-0.8B-Q4_K_M.gguf` | 508 MiB | 768Mi |
| `qwen3-0.6b` | `Qwen3-0.6B-Q4_K_M.gguf` | 378 MiB | 512Mi |

If the VMs can reach huggingface.co, the writer downloads the model at
step M3. If they cannot, for instance behind a proxy that intercepts
TLS, download the file on any machine, check it, and give its directory
to setup:

```bash
mkdir -p ~/llm-models && cd ~/llm-models
curl -LO https://huggingface.co/unsloth/Qwen3.5-2B-GGUF/resolve/f6d5376be1edb4d416d56da11e5397a961aca8ae/Qwen3.5-2B-Q4_K_M.gguf
echo "aaf42c8b7c3cab2bf3d69c355048d4a0ee9973d48f16c731c0520ee914699223  Qwen3.5-2B-Q4_K_M.gguf" | sha256sum -c
export DEMO_LLM_MODEL_DIR=$HOME/llm-models
```

Setup then serves this directory to the writer from the host, on port
9920 (`http://192.168.76.2:9920` from the VMs). The URLs, commits and
sums of the other models are in `LLM_MODELS` in
[`test98-demo-setup/code.var.sh`](blob:/test/e2e/memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/test98-demo-setup/code.var.sh).
`DEMO_LLM_MODEL_URL` points the writer at a mirror of your own.

## The presenter's screen

The screen has six terminals of 50 columns x 10 rows, in two columns of
three. It lets you watch every layer at once, whether you study the
demo alone or show it to others:

```
  +---------- T1 ----------+  +---------- T2 ----------+
  | PRESENTER: the steps   |  | PODS & LEASES          |
  +---------- T3 ----------+  +---------- T4 ----------+
  | SCHEDULER: devices and |  | NODES: CXL in the      |
  | claims                 |  | kernel of each node    |
  +---------- T5 ----------+  +---------- T6 ----------+
  | TRACE: one line per    |  | MEMORY (local), POOL   |
  | event                  |  | server (pool)          |
  +------------------------+  +------------------------+
```

T1 runs the story (`demo local`, `demo pool`, ..., `demo llm-share`).
T2 to T6 run `demo view t2` to `demo view t6`, and `demo commands`
prints all of these commands. The views refresh once a second and show
the clock in their title. When a refresh fails, a view says "stale" and
keeps the last good picture. T5 scrolls and shows one line per event,
and one line for a teardown or a new setup. The colors are green for
Ready or allocated, yellow for waiting and red for refused.
`--no-color` (or `NO_COLOR=1`) turns them off. Read the right column
from the application down to the memory (T2, T4, T6), and the left
column from the action down to the reason (T1, T3, T5). On a small
screen, use T1 to T4 and zoom the rest.

Each step in T1 shows its caption and the real command. The command
runs in VM2, in `/root/demo`, with the demo namespace `cxl-demo`. These
keys control the step:

| key | |
|:----|:-|
| Enter | run the step |
| `s` | skip it |
| `z` | zoom into a detail view, for example `claim reader-a` ([Look inside](#look-inside)) |
| `q` | stop |

After the command, the step shows a result of one to four lines and the
time the command took. It never hides the command, and the full output
is in `run/steps.log` (`demo zoom last`). `demo local 6` runs step 6
alone, `demo local 6-` runs from step 6 on, and `demo local 2-5` runs a
range. `--auto` runs all steps with a pause between them
(`DEMO_AUTO_PAUSE`), which is useful for a rehearsal, and it appends
the step timings to `run/timings.txt`.

## Setup and teardown

```bash
demo setup local      # or pool, pool-shared-fs, pool-fs [ext4|xfs|btrfs], wizard, llm-share
demo teardown
demo reset local      # deletes the demo pods and their leases: start from step 1 again
```

Run one demo at a time. To switch, tear down the current demo and set up
the next. Setup is the test `test98-demo-setup` of the suite, which a
normal run of the suite skips. Setup runs through `run_tests.sh`, and it
refuses to start while another `run_tests.sh` is running. Its log is
`run/test98-demo-setup.log`, and `run/state.env` tells which demo is
running. Setup does the following:

- It checks the shape of the cluster.
- It starts `fake-cxl-pool-server` on the host with the devices of the
  story. These are VM2's local device for `local`, `shared0` for `pool`,
  an empty persistent `shared0` for `pool-shared-fs`, an empty
  persistent `fs0` for `pool-fs`, the devices of [the wizard](#the-wizard)
  for `wizard`, and `llm0` on a tmpfs of its own for `llm-share`.
- It installs the driver on every node with helm. For `local` it puts
  VM2's device in `sharedMemdevs`, for `pool-fs` it sets
  `fsTypes: [erofs, <type>]`, and for `wizard` it sets
  `fsTypes: [erofs, ext4, xfs, btrfs]`.
- It installs the pool controller as a Deployment, and builds its image
  with docker and imports it into every node.
- It imports the demo image where the story needs it, and creates the
  ConfigMap `demo-tools`, which puts `/demo/write`, `/demo/read`,
  `/demo/hold`, `/demo/mkfs`, `/demo/fswrite`, ... into the pods.
- It copies the manifests to `/root/demo` in VM2, labels the nodes
  `cxl-demo/node=vm0|vm1|vm2`, and starts the trace.

For `pool-fs` and the wizard, setup prints the trust note from
[persistent.md](persistent.md#filesystems). The note says that a node
that mounts ext4, xfs or btrfs trusts every party that can write the
device raw. A failed setup cleans up after itself. The output ends with
lines that look like errors but are not (`curl: (7) Failed to connect to localhost port
8891`, `no coverage data ...`). Look for `Test verdict: PASS` instead.

Teardown first unconfigures VM2's shared device, then removes the rest,
and finally checks that no VM has CXL memory devices left. Before it
detaches a pool device, it checks that the VMs have no filesystem mount,
lease bind or loop device over pmem left. If it finds one, it stops with
a WARNING and the manual cleanup steps, and a new `demo teardown`
continues from there. Teardown also removes the files of the persistent
devices.

qemu keeps the backing file of a detached device mapped until the VM's
next memory change or restart. On a tmpfs that memory stays allocated,
so teardown unmounts the pool tmpfs lazily
(`tmpfs unmounted lazily, processes ... still map removed files of it`).
To get the memory back, restart the VMs. Run `vagrant halt` in the
output directory of each VM, wait until `vagrant status` says `stopped`,
and then run `vagrant up --no-provision`. Start the workers first and
VM2 last.

## local and pool

`local` has nine steps, L1 to L9. S is the serial of VM2's device
(`0xc100e2e1`):

| step | command | point at |
|:-----|:--------|:---------|
| L1 a writer starts on VM2 | `kubectl apply -f local/writer.yaml` | T2 the writer Running, no lease. T3 the device `1Gi shared`, no claims. T6 the bytes there |
| L2 the writer asks for the whole device | `kubectl exec writer -- cxl-request request --serial S --mode dax` | T2 `dax rw excl`. T3 `exclusive: writer rw`. T4 region0, dax0.0 on `device_dax`. T5 the lease, its allocation, the region, the zeroing once, Ready. T6 zeros |
| L3 the writer writes a line | `kubectl exec writer -- /demo/write "Hello ..."` | T6 the line. T4 `mapped: writer rw` while it writes |
| L4 three readers start | `kubectl apply -f local/readers.yaml` | T2 reader-a to reader-c Running |
| L5 the readers ask for a read-only share | `cxl-request request --serial S --share --read-only --wait 0s` in each | T2 three `waiting`. T3 `waiting: reader-a reader-b reader-c`. T5 `cannot allocate`. Pause here, because the writer has the device alone and nothing moves |
| L6 the writer releases the device | `kubectl exec writer -- cxl-request release --all` | T3 `shared: reader-a ro, ...`. T2 three `dax ro share`. T4 dax0.0 stays. T6 the line stays |
| L7 each reader reads the line and keeps it mapped | `/demo/read`, `/demo/hold` in each | T1 `3 readers read at 0: ...`. T4 `mapped: reader-a ro, ...`, which is the kernel's own record |
| L8 a reader tries to write | `kubectl exec reader-a -- /demo/write "overwrite"` | T1 `refused: read-only (EACCES)`, because the fd of a read-only lease allows no writable mapping, whatever the container's privileges |
| L9 everything goes away | `kubectl delete -f local/` | T3 no claims. T4 region0 and dax0.0 stay. T6 the bytes stay, so the next writer finds them |

At L2, notice that the lease became a claim (its companion), that the
scheduler allocated it like any other claim, and that the node then made
the device. A shared local memdev keeps its region and bytes
as long as it is configured shared. If you run the story again before a
teardown, the writer finds the line of the previous run.

`pool` has the same nine steps. It uses the `pool/` manifests and
`S = 0xc1ae0001` (`shared0`, 256Mi). The writer runs on VM1, reader-a
and reader-b on VM2, and reader-c on VM1. T6 shows the pool server: the
device, where it is attached, and its bytes as the backing file on the
host holds them. The differences are these:

- L2: the pool hot-plugs the device into VM1 (T6 `attached: vm1
  single-host`, T4 mem0 with its region on VM1). The lease is Ready only
  after the attach, because the scheduler waits for it.
- L3: T6 shows the line from the host's side, so the bytes are in the
  pool.
- L6: `shared0` stays on VM1, which now shares it, and the pool attaches
  it to VM2 too. The readers whose node does not have the device yet
  show `attaching`. T5 shows the release of the writer's memdev on VM1
  and its bind again for reader-c.
- L9: the drivers release the device (`disabled, no region` in T4), and
  then the pool detaches it from both nodes. The bytes stay in the pool.

## pool-shared-fs

This story repeats the pool story, but the device holds a filesystem
instead of bytes at offset 0. The writer gets `shared0` as a block
device (an fd of `/dev/pmem0`, with no device node), makes an erofs with
`hello.txt` and writes it into the device. The readers find the erofs
mounted at `/run/cxl/fs/hello` in their running containers. `S =
0xc1ae0001`, the writer runs on VM1, reader-a and reader-b on VM2, and
reader-c on VM1. The steps are F1 to F9:

| step | command | point at |
|:-----|:--------|:---------|
| F1 a writer starts on VM1 | `kubectl apply -f pool-shared-fs/writer.yaml` | T3 `shared0 @pool 256Mi shared persistent`. T6 `no file system (zeros)` |
| F2 the writer asks for a block device | `cxl-request request --serial S --mode block` | T1 `block /dev/pmem0`. T4 the pmem region and `pmem0 ... (block rw)`. T5 the attach, the region and namespace, Ready |
| F3 the writer makes an erofs with hello.txt | `/demo/mkfs "Hello ..."` | T4 `pmem0 ... erofs`. T6 `erofs: hello.txt` and its text, read from the backing file on the host |
| F4 three readers start | `kubectl apply -f pool-shared-fs/readers.yaml` | |
| F5 the readers ask for the filesystem | `cxl-request request --serial S --share --read-only --mode fs --name hello --wait 0s` in each | T2 three `waiting`, because the writer has the device alone. Zoom `claim reader-a`: class `cxl-pool-persistent`, shares `1`, mode `fs`, read-only |
| F6 the writer releases the device | `cxl-request release --all` | T2 three `fs ro share`. T4 on both nodes `erofs (block ro)` and the readers that have it mounted (dax). T5 the node mounts and the binds |
| F7 each reader reads hello.txt | `cat /run/cxl/fs/hello/hello.txt` in each | T1 `3 readers read: ...` |
| F8 a reader tries to write | `sh -c 'echo overwrite > /run/cxl/fs/hello/hello.txt'` | T1 `Read-only file system` |
| F9 everything goes away | `kubectl delete -f pool-shared-fs/` | T4 no CXL memory. T6 the erofs stays in the pool for the next reader |

At F3, notice that `mkfs.erofs` writes a file in the container, and
that the tool copies the file through the fd. The tool
copies it this way because `/proc/self/fd/3` cannot be opened again, and
because `/run/cxl` in the container is read-only. The block device
starts 2 MiB into the device, after the namespace's pfn info block
(`demo zoom fs` shows where).

At F6, notice that each node mounted the erofs once with `dax=always`,
so the pages of `hello.txt` are the device's memory, and
that each node then bound the mount into the running containers. If a
reader still has a file of the mount open, its lease stays in releasing
at F9 (T5 `unmount busy, retry`) until the reader closes the file.

## pool-fs

This story uses any filesystem, read-write. The writer first formats
`fs0` (512Mi) with ext4 through a block lease. (`demo setup pool-fs xfs`
or `btrfs` selects another type.) The writer then mounts `fs0`
read-write through an `fs` lease and writes `hello.txt`. A reader on the
writer's node shares its superblock at once. The readers on the other
node wait while the writer writes, because a single-host filesystem that
one host writes and another host reads gives corrupt or misleading data.
When the writer leaves, its node makes the filesystem read-only, and the
other node mounts it.

Here `S = 0xc1ae0001`, the nodes are the same as in pool-shared-fs, and
the filesystem is at `/run/cxl/fs/data`. T2 labels the writer's lease
`fs rw (vm1 only)`. The lease is one share of `fs0`, and the pool
attaches `fs0` to VM1 alone while the writer writes.

| step | command | point at |
|:-----|:--------|:---------|
| R1 a writer starts on VM1 | `kubectl apply -f pool-fs/writer.yaml` | T6 `no file system (zeros)` |
| R2 the writer asks for a block device | `cxl-request request --serial S --mode block` | as F2 |
| R3 the writer makes an empty ext4 | `/demo/mkfs --type ext4` | T4 `pmem0 ... ext4 (block rw)`. T6 `ext4 "demo" ..., clean`, read from the backing file |
| R4 the writer mounts it read-write instead | `cxl-request release --all && cxl-request request --serial S --share --mode fs --read-only=false --name data` | T1 `attached to vm1 only (ReadWriteOnce)`. T4 `node mount ext4 rw (dax)`. T5 the single-host attach. T6 `needs recovery`, because the journal is in use while a node has the filesystem read-write |
| R5 the writer writes hello.txt | `/demo/fswrite "Hello ..."` | T1 the line |
| R6 three readers start | `kubectl apply -f pool-fs/readers.yaml` | |
| R7 the readers ask to read | `cxl-request request --serial S --share --mode fs --name data --wait 0s` in each | T1 reader-c Ready on VM1 next to the writer, reader-a and reader-b `waiting: Conflict`. T4 one superblock on VM1, reader-c's bind read-only. Pause here, because nothing reaches VM2 while VM1 writes |
| R8 the writer adds a line, and VM1's reader sees it at once | `/demo/fswrite --append "..."`, then `cat` in reader-c | T1 the new line: one node, one superblock |
| R9 the writer is done | `cxl-request release --all` | T1 `vm1: ext4 read-only, pmem0 read-only`, the readers of VM2 Ready. T4 both nodes `ext4 (block ro)`. T6 `clean` |
| R10 each reader reads hello.txt, and a write fails | `cat` in each, then `echo x >> ...` | T1 the same two lines in all three, `Read-only file system` |
| R11 everything goes away | `kubectl delete -f pool-fs/` | T6 the ext4 stays for the next run |

At R3, notice that `mkfs.ext4` makes an image file of the block
device's size in the container, and that the tool writes all of it
through the lease's fd. At R9, notice that the writer's release
synced the ext4 and made it read-only on VM1, the block device too,
before the controller let another host in. So VM2 reads what VM1 wrote.
The story goes the same way with xfs. btrfs has no DAX, so its access is
cached and goes through a loop device.

## The wizard

`demo wizard` asks a few questions that choose a pod and its claim, or a
lease, among the combinations that the driver accepts. Then it shows the
YAML, creates the objects in the demo namespace, and deletes them again
on request. Each question lists its choices with numbers and marks the
default with `*`. It also lists the choices that the driver refuses,
with the reason. After the questions, the wizard shows the request in
words, the warnings (such as that the pod will wait, and why), the same
request as a command line, and the prompt "Enter create, y the YAML, q
quit". `demo setup wizard` makes every kind of device:

- The local memdevs of VM2 (1Gi) and VM1 (256Mi). Both have the serial
  `0xc100e2e1`, so a claim with that serial gets the memdev on its
  pod's node.
- `lp0` (`0xc100f5e1`), a persistent local memdev of VM2, served by a
  second pool server without a controller.
- In pool A: `pooled0` (unshared), `shared0` (shared) and `fs0`
  (persistent, `0xc1ae0002`).

Setup formats `fs0` and `lp0` with an ext4 that holds `hello.txt`, so
that `fs` claims find something to mount. You can also skip the
questions:

```bash
demo wizard new [OPTIONS] [--dry-run]   # --dry-run prints the YAML
demo wizard list
demo wizard yaml NAME
demo wizard delete NAME...|--all
```

The options follow the questions (`demo wizard new --help`): `--kind
claim|lease`, `--source local|pool|any`, `--memory
volatile|persistent`, `--share[=false]`, `--mode numa|dax|block|fs`,
`--size whole|SIZE`, `--serial any|SERIAL`, `--read-only[=false]`,
`--fs-type`, `--access`, `--path`, `--steering`, `--node vm1|vm2|any`,
`--command idle|env|read|write TEXT|mkfs TYPE` and `--name`. The driver
defines the rules. If the devices cannot satisfy a choice right now, the
wizard accepts it with a warning, and the pod waits. This is how the
demo shows waiting. Here is a walk:

```bash
demo wizard new --name w1 --source local --mode numa --size 256Mi --node vm2
demo wizard new --name w12 --source pool --mode dax --serial 0xc1ae0001 --node vm1 --command write "Hello"
demo wizard new --name w13 --kind lease --source pool --share --read-only --serial 0xc1ae0001 --node vm2 --command read
                         # waits: w12 has shared0 alone
demo wizard delete w12   # w13 Ready, its pod's log reads "Hello"
demo wizard new --name w18 --source pool --memory persistent --share --mode fs --read-only=false --serial 0xc1ae0002 --node vm1
demo wizard delete --all
```

Delete an object only after `demo wizard new` has printed that its
command ended, because a `mkfs` that stops half way leaves no
filesystem. `bin/wizard-rules.sh` runs every accepted combination on the
cluster, and it runs every refused combination against the wizard
(`--dry-run` at the desk). `demo wizard check` runs the wizard's checks.

## llm-share

This story puts one copy of a language model in pooled memory for every
VM. The writer on VM1 fetches the model's GGUF file and makes an erofs
of it in its container (`mkfs.erofs -b4096 --dsunit=512 -Enoinline_data`).
This makes one plain, 2 MiB-aligned extent, which DAX needs. The writer
then writes the erofs into `llm0` through its block lease. After that, a
reader starts on each VM. Each reader runs stock llama.cpp
(`llama-server -nr`), with the model at `/models` from a read-only `fs`
claim of `llm0`, and with as many threads as its CPU limit. A client
pod on VM2 asks the readers questions the way an agent asks its model,
with a system prompt, a question and one tool, `get_weather`. The
question is "What is the weather like in Helsinki right now, and should
I take an umbrella?", and the model answers with a call of the tool.

The readers use one of two accesses:

- Direct access: each node mounts the erofs with DAX, so `llama-server`
  maps the gguf file from the device. Every qemu maps the same backing
  file, so the model is in host memory only once, however many VMs there
  are, and it is in no VM's page cache.
- Cached access: each node mounts the erofs through a read-only loop
  device. The model is read once into each VM's page cache, and
  `llama-server` maps that copy.

| step | what happens |
|:-----|:-------------|
| M1 | the writer starts on VM1 |
| M2 | the writer asks for `llm0` as a block device, and the pool attaches it to VM1 |
| M3 | the writer fetches the model, checks its sha256, makes the erofs and writes it into the device |
| M4 | the writer releases the device, and `llm0` detaches from VM1 |
| M5 | a reader starts on each VM with a read-only share of `llm0`, which is the first access. The client starts on VM2 |
| M6 | the client asks each reader once, and the model calls the tool |
| M7 | benchmark, each reader alone |
| M8 | benchmark, all readers at once |
| M9 | (host) the model is in host memory once, because every qemu maps the same pages |
| M10 | the readers start again with the other access |
| M11 to M13 | ask and benchmark again |
| M14 | (host) the memory again, to compare with M9: with cached access, each VM holds a copy of the model |
| M15 | everything goes away |

The options are environment variables of `demo setup llm-share`:

| variable | default | |
|:---------|:--------|:-|
| `DEMO_LLM_MODEL` | `qwen3.5-2b` | `qwen3.5-2b`, `qwen3.5-0.8b` or `qwen3-0.6b` |
| `DEMO_LLM_MODEL_DIR` | | a host directory with the model file, which setup serves to the writer ([The model](#the-model)) |
| `DEMO_LLM_MODEL_URL` | the Hugging Face URL | where the writer fetches the model. It wins over `DEMO_LLM_MODEL_DIR` |
| `DEMO_LLM_ACCESS` | `direct` | the first access, `direct` or `cached`. M10 switches to the other one |
| `DEMO_LLM_LATENCY` | `off` | the [latency setup](#the-latency-variant): `off`, `local` or `far` |
| `DEMO_LLM_LATENCY_LOAD` | `off` | with `far`, the level of the background load: `700` or `1000` |
| `DEMO_LLM_REPEAT` | `3` | the number of requests per reader in a benchmark. You can also set it for one step: `DEMO_LLM_REPEAT=5 demo llm-share 7` |
| `DEMO_POOL_DIR` | `/tmp/fake-cxl-pool-llm` | the pool directory, which must be empty or missing |

The pool server keeps the memory of each device in a file. These files
are in the pool directory. Setup mounts a tmpfs there with
`huge=within_size`, which gives 2 MiB pages where a file is large
enough. Setup also adds `mpol=interleave:<nodes>` over the host's NUMA
nodes that have both memory and CPUs. This leaves out the memory of a
CXL device or HBM. A host with only one such node gets no policy.

Without the policy, a page lands on the node of the CPU that first
writes it, which is wherever the writer's vCPU happened to run, and then
all readers wait on that node. In one run, three direct readers at once
decoded about 1.8 times as slowly with the file on one node as with it
spread over the four nodes of a two-socket host. A node list contains
commas, and mount(8) splits options at commas, so the mount runs with
`LIBMOUNT_FORCE_MOUNT2=always`. With huge pages the spreading is best
effort, because a 2 MiB page can fall to the next node of the list
(`demo llm mem` shows the nodes). Without sudo, setup uses
`/dev/shm/fake-cxl-pool-llm` instead, with 4 KiB pages and no policy.

The framework lets each VM's qemu use transparent huge pages whatever
the host's setting is (`grep THP_enabled /proc/<qemu pid>/status` says
1). This way the guest RAM and the pool memory can be in 2 MiB pages.

One setup runs both accesses and both modes at the latency setup it was
created with. The whole results table therefore takes one setup for each
latency setup:

```bash
DEMO_LLM_LATENCY=off   demo setup llm-share; demo llm-share --auto; demo teardown
DEMO_LLM_LATENCY=local demo setup llm-share; demo llm-share --auto; demo teardown
DEMO_LLM_LATENCY=far   demo setup llm-share; demo llm-share --auto; demo teardown
demo llm results
```

`run/llm-results/` keeps the benchmarks across teardowns, and
`demo llm results` prints them as one table.

## The latency variant

Real CXL memory is slower than the DRAM next to the CPU, but the demo's
VMs read the pool memory at the speed of host memory. On a host with two
sockets, the latency variant puts the device on the other socket, and it
can keep that socket's memory busy. `DEMO_LLM_LATENCY` chooses the
setup:

| setup | the VMs | the memory of `llm0` |
|:------|:--------|:---------------------|
| `off` (default) | not pinned | spread over all memory nodes of the host. On two sockets about half of it is on the other socket |
| `local` | pinned to the CPUs of socket 0, with their memory moved to its nodes | spread over the nodes of socket 0 |
| `far` | as `local` | on the nodes of socket 1 only, with the background load of `DEMO_LLM_LATENCY_LOAD` if set |

These are the runs to compare:

- `local` is the best placement that the host has for the model. Direct
  against cached access within `local` shows how much of the gap between
  them is left when the memory distance is as small as it gets.
- `local` against `far` differ in one thing for the VMs, which is where
  the device memory is. The difference is the cost of the distance to
  the device.
- `off` is the host as the e2e framework leaves it, and it is not a
  local setup. `off` against `local` shows the effect of pinning and
  placement.
- Compare the setups of one host only.

`bin/latency.sh` does the work. `demo setup` runs `latency.sh on` with
the chosen setup after it resets the VMs and before the pool server
makes the device files. Teardown, or a failed setup, runs `latency.sh
off`. You can also run it by hand with `demo latency
on [far|local]|off|status|levels`, but it refuses `on` while the demo is
set up. Before it changes a value, it saves the value in
`run/latency.state`. These are the changes:

| what | to |
|:-----|:---|
| automatic NUMA balancing (`kernel.numa_balancing`) | 0 |
| KSM | `run` 2: unmerge and stop |
| memory demotion | off |
| every thread of each demo VM's qemu | pinned to the CPUs of socket 0 |
| the memory of each demo VM's qemu | moved to the nodes of socket 0 |
| the pool directory | a tmpfs with `huge=within_size` and `mpol=bind:<nodes of socket 1>` (`far`) or `mpol=interleave:<nodes of socket 0>` (`local`) |
| the background load (`far` with `DEMO_LLM_LATENCY_LOAD`) | started after the rest |

`off` stops the load and writes every saved value back. An `on` that
fails or gets SIGINT, SIGTERM or SIGHUP does the same for the values it
had already changed. The VMs' memory stays where `latency.sh` moved it,
and KSM starts again from zero.

`latency.sh` needs two sockets for `far`, and for `local` only socket 0.
Each socket needs NUMA nodes that have both CPUs and memory. It also
needs sudo without a password, `taskset`, `migratepages` (numactl),
`findmnt`, `mountpoint` and `flock`. On a host with one socket,
`latency.sh on far` stops and tells the reason. It never touches VMs
that are not part of the demo. Stop big ones anyway, because they add
noise.

The background load keeps the memory of socket 1 busy. A read from
socket 0 then waits in the queues of its memory controllers, like a read
from a busy CXL link or device. `cxl-load` runs one thread on each CPU
of socket 1, at the lowest priority. Each thread writes block after
block over its own buffer, which is bound to the memory of socket 1. The
threads write instead of read because a CPU that writes a line first
reads it to own it and later writes it back, so the memory channels fill
up. The levels are:

| level | what each thread does |
|:------|:----------------------|
| `700` | writes a block and waits after each one |
| `1000` | writes as fast as it can |

The levels were set on a two-socket server with sub-NUMA clustering. On
another host the same level can give a different load.
`LATENCY_LOAD_ARGS` gives `cxl-load` other arguments (`cxl-load -h`).
The load is a busy memory and not a longer path. It makes reads slow as
a busy CXL device does, but it does not use the CXL protocol. `demo
latency status` shows the topology, the settings, the VMs and their
memory, the placement of the device files and the load.

## Look inside

Zooms answer questions. In T1 press `z`, or run `demo zoom WHAT` in any
terminal (`demo zoom help` lists them):

| zoom | |
|:-----|:-|
| `slice [DEVICE]` | the ResourceSlice entry of a device |
| `claim POD` | the companion claim of POD's lease: request, allocation, configuration |
| `pod POD` | why POD's lease waits (the scheduler's message) |
| `env POD` | the `CXL_*` variables of POD's leases |
| `cdi NODE`, `driver NODE [N]`, `kernel NODE`, `cxl NODE` | the CDI specs, the driver's log, the kernel's CXL lines, `cxl list` of the node |
| `pool` | the pool server's devices and attachments |
| `fs` | the filesystem in the pool device's backing file |
| `mounts NODE` | the mounts of the pmem block devices, their read-only flags, loop devices and lease binds |
| `bytes [OFFSET]` | a hex dump of the device |
| `last` | the full output of the last step |

`demo capture LABEL` writes each view once to `captures/`.
`demo truth LABEL` writes the ground truth of the moment, so that you
can check the views against it. The ground truth has the kubectl
objects, `cxl list`, the dax and pmem devices, the mounts, the mappings
and fds of the pod processes, the pool server and its backing file.

To look at the cluster, run these commands in VM2:

```bash
kubectl -n cxl-demo get pods -o wide
kubectl -n cxl-demo get resourceclaims
kubectl get resourceslices
kubectl -n fake-cxl-pool logs -l app.kubernetes.io/name=fake-cxl-pool-controller --tail=20
kubectl -n cxl-demo logs reader-1 --tail=20       # llama-server: the model load, one line per request
```

You can run one inference by hand, either with the demo's client or
straight to a server:

```bash
kubectl -n cxl-demo exec client -- /demo/llm ask reader-1
ip=$(kubectl -n cxl-demo get pod reader-1 -o jsonpath='{.status.podIP}')
curl -s --noproxy '*' "http://$ip:8080/v1/chat/completions" -H 'Content-Type: application/json' \
    -d '{"messages": [{"role": "user", "content": "Say hello in five words."}], "max_tokens": 32}'
```

On the host, `demo llm mem LABEL` prints where the model is in memory
and saves the result to `run/llm-mem-LABEL.json`. Steps M9 and M14 run
it. It tells how much of `llm0.raw` is in host memory, how much of that
is in 2 MiB pages, and on which NUMA nodes. It also tells what each
qemu maps (`Rss`, and `Pss`, which splits shared pages among the qemus
that map them), and how much page cache each reader is charged for in
its VM. `cxl-numa-place where /tmp/fake-cxl-pool-llm/llm0.raw` shows
the nodes of the file's pages, and
`kubectl -n cxl-demo exec reader-0 -- /demo/llm-mem reader-0` shows a
reader's memory in one line.

### Log in to a node

Each VM has an output directory under `$N/test/e2e/` with an ssh config
in it, and the config names the VM `node`:

| VM | output directory | role |
|:---|:-----------------|:-----|
| VM0 | `n4-cxl-shared-0-fedora-43-containerd` | worker |
| VM1 | `n4-cxl-shared-1-fedora-43-containerd` | worker |
| VM2 | `n4-cxl-shared-2-fedora-43-containerd` | control plane |

```bash
ssh -F $N/test/e2e/n4-cxl-shared-2-fedora-43-containerd/.ssh-config node
ssh -F $N/test/e2e/n4-cxl-shared-2-fedora-43-containerd/.ssh-config node kubectl -n cxl-demo get pods
```

The first command logs you in to VM2 as the user vagrant, and the second
runs one command there. kubectl works as vagrant in VM2. Files under
`/root` and the driver's directories need sudo, and `sudo -i` gives a
root shell.

### Find the YAML

Each story has its manifests in `manifests/<story>/` of the demo
directory. Setup copies them to `/root/demo` in VM2, and the step
commands run them from there. `demo wizard yaml NAME` prints the YAML of
a wizard object. The live objects are in the namespace `cxl-demo`, and
you can read them in VM2:

```bash
kubectl -n cxl-demo get resourceclaims -o yaml   # the claims, with the companion claims of leases
kubectl get resourceslices -o yaml               # the devices that the drivers publish
kubectl get deviceclasses                        # the classes
kubectl -n cxl-demo get pod POD -o yaml          # a pod and its claims
```

A good first question is which claim holds which device. In the pool
story after L7, this command in VM2 answers it:

```bash
kubectl -n cxl-demo get resourceclaims -o custom-columns='CLAIM:.metadata.name,REQUESTER:.metadata.annotations.cxl\.nri\.io/requester,DEVICE:.status.allocation.devices.results[*].device,NODE:.status.allocation.nodeSelector.nodeSelectorTerms[0].matchFields[0].values[0]'
```

```text
CLAIM                REQUESTER                DEVICE    NODE
cxl-lease-65c869c7   cxl-demo/reader-a/main   shared0   n4-cxl-shared-2-fedora-43-containerd
cxl-lease-9401624f   cxl-demo/reader-b/main   shared0   n4-cxl-shared-2-fedora-43-containerd
cxl-lease-d0e0bbce   cxl-demo/reader-c/main   shared0   n4-cxl-shared-1-fedora-43-containerd
reader-a-cxl-gf4nw   <none>                   lease     n4-cxl-shared-2-fedora-43-containerd
...
```

Each pod's own claim holds only the lease device. The memory comes in
the companion claims `cxl-lease-<id>`, which the lease broker creates
when a container runs `cxl-request`. The annotation
`cxl.nri.io/requester` names the container that asked. Read one of them
in full (`kubectl -n cxl-demo get resourceclaim cxl-lease-<id> -o
yaml`). Its spec has the request that `cxl-request` made (the class
`cxl-pool-shared`, one share, the serial, read-only dax), and its status
has the allocation, the share and the binding condition
`cxl-pool.nri.io/Attached`. This condition makes the scheduler wait
with the companion pod until the pool has attached the device.

### Look inside a node

Log in to a node and look at what the kernel and the driver made. The
zooms `cxl`, `kernel`, `cdi` and `driver` run some of these for you:

```bash
sudo cxl list -M                # the memdevs
sudo cxl list -M -R             # with their regions
sudo daxctl list                # the dax devices
numactl -H                      # the NUMA nodes
lsmem                           # the memory blocks
ls /var/run/cdi                 # the CDI specs that the driver writes
sudo less /var/lib/kubelet/plugins/cxl.nri.io/state.json
```

In the pool story after L7, VM1 has `shared0` as `mem0`. `cxl list`
prints the serial in decimal, so `"serial":3249405953` is `0xc1ae0001`
(`printf '%#x\n' 3249405953`). `cxl list -M -R` shows `region0` of type
`ram`, and `daxctl list` shows `dax0.0` in mode `devdax` with
`target_node` 2. `numactl -H` still lists only nodes 0 and 1, because
the memory of a dax device is not online as system RAM. The readers map
`/dev/dax0.0` instead.

The CDI specs are in one file, `/var/run/cdi/cxl-claims.json`. While the
driver runs, `state.json` holds its records, and
[design.md](design.md) and [states.md](states.md) describe them. `jq`
in the VMs reads them, one kind of record at a time:

```bash
sudo jq '.memdevs' /var/lib/kubelet/plugins/cxl.nri.io/state.json
sudo jq '.regions' /var/lib/kubelet/plugins/cxl.nri.io/state.json
sudo jq '.leases' /var/lib/kubelet/plugins/cxl.nri.io/state.json
```

In VM1 after L7 they tell the whole chain. The memdev with the serial
`0xc1ae0001` has the source `pool` and is `ACTIVE`. Its `region0` is
`OWNED` by the driver, with `dax0.0` bound to `device_dax`. The lease
`d0e0bbce` of reader-c is `READY`, and its `got` field names
`/dev/dax0.0`. The lease ID is also the name of its companion claim,
`cxl-lease-d0e0bbce`.

The driver's log is `demo zoom driver NODE`, or in VM2:

```bash
kubectl -n kube-system get pods -o wide --field-selector spec.nodeName=n4-cxl-shared-1-fedora-43-containerd
kubectl -n kube-system logs -c nri-dra-driver-cxl POD
```

where POD is the `nri-dra-driver-cxl-...` pod that the first command
lists for the node. The VMs do not have `ndctl`.

### Watch the qemu monitor

Each VM's qemu has a human monitor (HMP) socket, `monitor.sock`, in the
VM's output directory on the host. It shows what qemu gave the VM, which
is the hardware side of the story:

```bash
cd $N/test/e2e/n4-cxl-shared-1-fedora-43-containerd
socat - unix-connect:monitor.sock
echo "info qtree -b" | socat - unix-connect:monitor.sock
```

The first `socat` opens a prompt, and the second one runs one command.
These commands are useful:

- `info qtree -b` lists the devices. The attached CXL memory devices
  appear as `dev: cxl-type3`. A pool device has the ID
  `fcp_<device>.hp<N>`, so in the pool story VM1 shows
  `dev: cxl-type3, id "fcp_shared0.hp1"` on the bus `cxlsw_ds0_usrp0hb0`.
- `info memdev` lists the memory backends. The local devices that the VM
  has from its start are `beram_...`. A pool device shows up only while
  the pool attaches it to the VM, with the same ID as the device
  (`fcp_shared0.hp1`) and `share: true`.

Mind these rules:

- The socket serves one client at a time, and the e2e framework uses it
  too (its `vm-monitor`). Leave the monitor before you run `demo setup`,
  `demo teardown` or `test00-up`.
- Leave with Ctrl-C. It closes the connection and keeps qemu running.
  Never type `quit`, because it stops the VM.
- Do not connect to `qmp.sock`, because the pool server holds that
  connection to receive the device events. `qmp-e2e.sock` belongs to the
  test framework.

### Watch the pool server

`demo zoom pool` shows the pool server's devices and attachments. The
client `fake-cxl-pool-client` shows the same, and `events` prints a line
for each change until you stop it with Ctrl-C. The VMs have the client
in `/usr/local/bin`, and its default server address works there. On the
host, the make command in [Preparation](#images-and-the-demo-command)
builds it, and you give it the server's local address:

```bash
cd $N/scripts/testing/fake-cxl-pool/bin
./fake-cxl-pool-client --server http://127.0.0.1:9909 devices
./fake-cxl-pool-client --server http://127.0.0.1:9909 attachments
./fake-cxl-pool-client --server http://127.0.0.1:9909 events
curl -s http://127.0.0.1:9909/api/v1/attachments
```

In the pool story after L7, `attachments` lists
`shared0@n4-cxl-shared-1-fedora-43-containerd` and
`shared0@n4-cxl-shared-2-fedora-43-containerd`. Both are in the slot
`cxlsw_ds0_usrp0hb0` with the qemu device `fcp_shared0.hp1`, as the
monitor shows. In a VM, `fake-cxl-pool-client whoami` shows what the
server knows about that VM: its host bridges, its slots and what is in
each, and the qemu socket that the server controls it through.

The demo's first server listens on port 9909. For the wizard, setup
also starts a second server, `local-pmem`, on port 9912. The REST API is
in the
[README of the pool](blob:/scripts/testing/fake-cxl-pool/README.md#rest-api).

Do not use the client's `create`, `delete`, `attach` or `detach` while a
story runs, because the controller and the driver own the attachments.

## Read the results

Each benchmark (M7, M8, M12, M13) prints one line per reader:

```text
reader-0 TTFT <ms> (<min>-<max>) TPOT <ms> <tok/s> tok/s
```

The numbers are the median and the range of the samples. TTFT is the
time from sending the request to the first streamed piece of the
answer. TPOT is the time per predicted token after the first. The
answer is a tool call of a few dozen tokens. For an answer with a tool
call, TPOT is the server's own figure, because the tool-call parser
streams the answer in a few pieces. For other answers, the client
computes it as (last piece minus first piece) / (tokens - 1).
`demo llm results` merges the saved benchmarks into one table. The table
has the model, the latency setup (`off`, `local`, `far` or `far+700`),
the `huge=` option of the pool tmpfs, the share of `llm0` in 2 MiB
pages, the access and the mode.

### Four setups on one host

The numbers come from a two-socket x86 server with sub-NUMA clustering,
Qwen3.5-2B, three readers, and two `--auto` runs of each setup. `llm0`
was in 2 MiB pages in every run, and no request failed. Each cell is
relative to direct access alone in the `local` setup (1.0). It is the
mean over the medians of the three readers in the two runs, with the
lowest and highest median in brackets. Lower is faster.

| latency | access | mode | TTFT | TPOT |
|:--------|:-------|:-----|:-----|:-----|
| off | direct | alone | 1.22 (1.08 - 1.32) | 1.73 (1.68 - 1.79) |
| off | direct | all at once | 1.27 (1.14 - 1.39) | 1.86 (1.84 - 1.88) |
| off | cached | alone | 1.15 (0.98 - 1.27) | 1.51 (1.32 - 1.69) |
| off | cached | all at once | 1.29 (1.04 - 1.44) | 1.38 (1.12 - 1.63) |
| local | direct | alone | 1.00 (0.97 - 1.04) | 1.00 (0.92 - 1.09) |
| local | direct | all at once | 0.95 (0.90 - 1.00) | 0.94 (0.90 - 1.01) |
| local | cached | alone | 0.98 (0.92 - 1.06) | 1.07 (0.99 - 1.17) |
| local | cached | all at once | 0.87 (0.81 - 0.95) | 0.95 (0.90 - 0.99) |
| far | direct | alone | 1.34 (1.28 - 1.38) | 2.03 (1.96 - 2.12) |
| far | direct | all at once | 1.22 (1.21 - 1.23) | 1.99 (1.91 - 2.06) |
| far | cached | alone | 1.00 (0.93 - 1.05) | 1.05 (0.98 - 1.13) |
| far | cached | all at once | 0.93 (0.84 - 0.98) | 0.95 (0.94 - 0.96) |
| far, load 700 | direct | alone | 1.78 (1.74 - 1.80) | 3.61 (3.57 - 3.70) |
| far, load 700 | direct | all at once | 1.50 (1.47 - 1.52) | 3.26 (3.25 - 3.27) |
| far, load 700 | cached | alone | 0.94 (0.87 - 1.04) | 0.98 (0.86 - 1.03) |
| far, load 700 | cached | all at once | 0.89 (0.82 - 0.96) | 0.96 (0.91 - 0.98) |

This table divides the TPOT of direct access by the TPOT of cached
access (1.0 means no gap):

| latency | alone | all at once |
|:--------|:------|:------------|
| off | 1.15 | 1.35 |
| local | 0.93 | 1.00 |
| far | 1.94 | 2.10 |
| far, load 700 | 3.70 | 3.41 |

In `local`, direct access is neither slower nor faster than cached
access beyond the spread between runs. In the other setups the gap comes
from where the memory is, not from how the reader accesses it. In `far`
the direct readers are about twice as slow, and with the load they are
more than three times as slow. The cached readers do not move, because
their copy is in the VM's RAM, on socket 0. In `off` both accesses are
slower than in `local`, because the VMs are not pinned and about half of
the model is on the other socket.

In `local`, both accesses read the DRAM of socket 0 through the host's
huge pages. The page size differs only inside the guest, with 2 MiB
entries for the DAX mapping and 4 KiB entries for the page cache. A
decode step streams the whole model, so it hides most translation
misses. The numbers come from one model and a few runs, so use them for
the direction and not as a benchmark.

### One copy, and what a page cache costs

After M9, with direct access, the device file is in host memory once, in
2 MiB pages. Each of the three qemus maps all of it (`Rss`), and its
`Pss` is a third of that. The page cache charged to the readers is 0,
and the VMs' page cache did not grow, because `llama-server` maps the
file from the device.

After M14, with cached access, each reader is charged the model's size
in page cache, and each VM's page cache grew by as much. The model
therefore costs four times its size, with one copy on the host and one
in each VM, against once with direct access. The host shows the copies
as private memory of the qemus only where the guest touched memory that
the host had not backed yet. A fresh VM grows by the full copy, and a VM
that has run for a while hardly grows at all. The cache is reclaimable,
and in these runs it did not push other cached data out.

## Troubleshooting

- A view says "stale": its ssh or kubectl call failed or took too long.
  The view retries every second. If it stays stale, stop it with Ctrl-C
  and start it again, and check
  `ssh -F $N/test/e2e/n4-cxl-shared-2-fedora-43-containerd/.ssh-config node true`.
- T3 says `drivers: vm2 restarting`: the driver pod is restarting, so
  wait. The trace of that node stops during a restart.
- A step does not finish: every command and every result check has a
  timeout. Ctrl-C in T1 stops the runner. `demo zoom last` shows what
  the command printed, and `demo local N` runs the step again.
- To start a story over, run `demo reset local` and then `demo local`.
- Readers stay waiting after L6, F6 or R9: run `demo zoom pod reader-a`
  (it shows the scheduler's reason) and `demo zoom driver vm2`. In
  pool-fs the writer's release may not have finished. `demo zoom mounts
  vm1` then shows the superblock still read-write while the writer has
  a file open.
- A lease stays releasing and T5 says `unmount busy, retry`: a process
  in the reader has a file of the mount open. The process ends with the
  pod.
- pool-fs: R3 says `mkfs.xfs failed` or R4 does not mount. The VMs'
  kernel lacks a feature that the image's mkfs uses (`demo zoom kernel
  vm1`).
- A pod of the wizard stays Pending: the warning of `demo wizard new`
  said why. Look further with `demo zoom pod wN`, `demo zoom claim wN`
  and `kubectl logs wN`.
- `demo setup` says there is no image of the driver: build it. If it
  says `run_tests.sh runs already`, another e2e run is going. If port
  9909 or 9920 is busy, an earlier pool server or model mirror is still
  running, so run `demo teardown`.
- Setup fails with `cannot hot-remove CXL memory devices`: the VM runs a
  qemu that cannot hot-remove ([e2e.md](e2e.md#requirements)).
- "The VM ... was provisioned with other versions": pin the releases as
  you did when you created the VMs, or set `e2e_vm_versions=ignore`.
- "The VM ... was not provisioned by this recipe": the VMs were created
  from another copy of the e2e provisioning files, even if the versions
  are the same. Set `e2e_vm_versions=ignore` to use the VMs as they are,
  or `provision=all` to provision them again in place.
- M3 cannot download the model (`SSL certificate problem`): use
  `DEMO_LLM_MODEL_DIR`. If you see "DEMO_LLM_MODEL_DIR ... has no ...
  with sha256", the file is not the pinned one.
- A reader of llm-share stays Pending with "0/3 nodes are available":
  check the node labels (`cxl-demo/node`), and check that `demo zoom
  pool` shows `llm0` with no single-host attachment, because the writer
  released it at M4. A reader that is Running but not Ready is still
  loading the model, which takes a while with cached access.
- The `2M` column says 0%: the VM's qemu runs with transparent huge
  pages off (`THP_enabled: 0`), so the numbers are not comparable. Run
  the VM's `test00-up`, which allows huge pages in its Vagrantfile, and
  restart the VM.
- `latency.sh on` refuses: its message says why, for example one socket,
  no sudo, a pool directory that is not empty, or the latency setup
  already on.
- A VM has CXL devices left, or the cluster is broken after a failed
  teardown: run that VM's `test00-up`, which restarts a VM that has CXL
  devices. Restarting VM2 makes the API server unavailable for a while.
- Anything else: run `demo teardown` and `demo setup` again. A teardown
  that failed can run again.

## The demo directory

| path | |
|:-----|:-|
| `bin/demo` | the entry point |
| `bin/views.py`, `bin/probe.py` | the views T2 to T6, and what they run in the VMs |
| `bin/steps.py`, `bin/zoom.py`, `bin/truth.py` | the step runner, the zooms, the ground truth |
| `bin/wizard.py`, `bin/wizard_views.py`, `bin/wizard_check.py`, `bin/wizard-rules.sh` | the wizard, its views, its checks, its scripted rules |
| `bin/llm.py`, `bin/tools/` | llm-share on the host, and the tools in the pods (the ConfigMap `demo-tools`) |
| `bin/latency.sh`, `bin/pool-tmpfs.sh` | the latency variant, the pool tmpfs |
| `bin/selftest.py`, `bin/selftest-shell.sh`, `testdata/` | the selftest and its samples |
| `image/Dockerfile` | the demo image |
| `manifests/` | the pods and claim templates of the stories |
| `run/` | the state, logs, timings and results of the current demo (not in git) |
| `captures/` | `demo capture` and `demo truth` (not in git) |

The setup and teardown are the tests
[`test98-demo-setup`](tree:/test/e2e/memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/test98-demo-setup)
and
[`test99-demo-teardown`](tree:/test/e2e/memory.test-suite/dra-driver-cxl/n4-cxl-shared-2/test99-demo-teardown).
