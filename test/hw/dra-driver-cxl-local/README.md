# Hardware tests of local CXL memory with nri-dra-driver-cxl

Scripts that test the local memdevs of `nri-dra-driver-cxl` (driver
`cxl.nri.io`) on a real single-node Kubernetes cluster with a CXL memory
expander. They run on a workstation and drive the node over ssh (`TARGET`,
`root@hostname`), where `kubectl`, `helm`, `cxl` and `daxctl` run. The
node needs a cluster (kubeadm, containerd with NRI and CDI, cgroup v2,
Kubernetes 1.37), `helm`, `cxl` and `daxctl`, `python3`, and a memdev with
RAM capacity.

| file | |
|:--|:--|
| `lib.sh` | helpers: ssh and kubectl, claims and pods (`claim_yaml`, `pod_yaml`), checks (`check_fenced`) |
| `deploy.sh` | import the image, prepare the node, install or reconfigure the chart (`local` or `local-shared`) |
| `run-all.sh` | deploy and run the tests, logs in `$RESULTS_DIR` (default `/tmp/dra-driver-cxl-local`) |
| `test1-local-numa.sh` | slices in mode `numa`: several containers on one NUMA node, `Mems_allowed`, other containers fenced off the node, steered and unsteered containers, release, a driver restart |
| `test2-local-dax.sh` | slices in mode `dax`: one claim of a writer and readers, a read-only slice, zeroing, `numa` and `dax` slices of one memdev |
| `test3-local-leases.sh` | leases of local memory in modes `numa` and `dax`, other containers fenced off the leased node |
| `test4-local-shared-writer-readers.sh` | the memdev in `sharedMemdevs`: one writer, read-only readers, a read-only lease, claims of the whole memdev, the kept bytes |
| `test5-memory-integrity.sh` | every page of the memdev, and the isolation of its first and last slice |
| `alloc-touch.py`, `dax-pattern.py`, `dax-scan.py` | tools that the test pods run, with `numa-touch.py` and `dax-rw.py` of the e2e suite |

## Running

```bash
make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

cd test/hw/dra-driver-cxl-local
./run-all.sh --image ../../../build/images/nri-dra-driver-cxl-image-<id>.tar --prepare-node
./run-all.sh test2-local-dax.sh     # later: the deployed image, one test
```

`--prepare-node` installs `/etc/udev/rules.d/40-redhat.rules` without the
memory hotplug section of the system rule, and offlines and destroys a
region that the BIOS created on the memdev: the driver manages only
memdevs without regions of others. A reboot brings the BIOS region back;
prepare the node again then.

Environment: `TARGET`, `CXL_SERIAL` (the memdev, default the first one),
`NS` (the namespace of the test pods, default `cxl-hw`), `TOOLS_IMAGE`
(default `docker.io/library/python:3-alpine`), and `CONTAINER_FENCING`
(`deploy.sh`, `run-all.sh`): `defaults.container.fencing` of the driver,
`allowed-mems` or `none`, unset for the driver's default `allowed-mems`.
With `none` the fencing checks of test1 to test3 fail: it is for looking
at the driver without fencing.

## Claims, steering and fencing

`claim_yaml` writes claims of one request `mem` on the memdev; its options
`mode=MODE`, `readonly` and `steering=POLICY` give the claim a
`MemoryConfig`:

```yaml
config:
- opaque:
    driver: cxl.nri.io
    parameters:
      apiVersion: cxl.nri.io/v1alpha1
      kind: MemoryConfig
      device: {mode: dax, readOnly: true}       # mode=dax readonly
      container: {steering: {policy: auto}}     # steering=auto
```

A container with a `numa` claim and no steering policy gets the claimed
NUMA node in its `cpuset.mems` next to its own, no memory policy and no
`memory.high`: its default allocations stay on DRAM. In test1 `numa-a` and
`numa-b` opt in with `steering=auto` and `numa-u` does not.

`check_fenced N POD...` checks every running container of the node
(`crictl ps`): the containers `main` of the PODs have NUMA node N in
`cpuset.mems.effective`, all others, those of kube-system included, do
not. It retries for 30 s and prints the table of `node_mems`.

The test containers run their workloads as children of their main process
(`prun`): the driver sets the memory policy of a steered container on its
processes at each step, and their children inherit it; a process of
`kubectl exec` gets it only at the next step.
