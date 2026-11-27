# Block I/O NRI plugin

This NRI plugin applies block I/O weights and per-device limits to containers
using cgroup v2. Workloads select a class defined in the plugin configuration.

## Workload configuration

Set a default class for the pod and override it for individual containers in the
pod's YAML:

```yaml
metadata:
  annotations:
    # Set the default class for all containers in this pod.
    blockio.resources.beta.kubernetes.io/pod: bronze

    # Override the default class for app.
    blockio.resources.beta.kubernetes.io/container.app: gold

    # Remove the default class from sidecar.
    blockio.resources.beta.kubernetes.io/container.sidecar: ""
```

The CRI container annotation `io.kubernetes.cri.blockio-class` takes precedence
over both pod annotations. An empty value clears the class at any level. The
plugin logs unknown classes and skips the adjustment.

## Plugin configuration

Pass a local class file to the plugin with `--config`. List the classes by name
under `Classes` using the
[goresctrl block I/O format](https://github.com/intel/goresctrl/blob/main/doc/blockio.md#configuration).

- `Weight` (10–1000) sets a relative share of I/O under contention. Add `Devices`
  to set a weight for specific devices. The runtime applies weights through
  `io.bfq.weight`.
- `ThrottleReadBps` and `ThrottleWriteBps` cap bytes per second. `ThrottleReadIOPS`
  and `ThrottleWriteIOPS` cap operations per second. These limits require
  `Devices` and use `io.max`. A value of `0` removes that limit.
- `Devices` accepts block device paths, globs, and symlinks. Each pattern must
  match at least one path, and each match must resolve to a block device on the
  node. Later entries override earlier values for the same device and setting.

### Example

```yaml
Classes:
  gold:
    - Weight: 400
  bronze:
    - Weight: 100
    - Devices:
        - /dev/nvme0n1
      ThrottleReadBps: 50M
      ThrottleWriteBps: 10M
      ThrottleReadIOPS: 10k
      ThrottleWriteIOPS: 5k
```

Replace `/dev/nvme0n1` with a backing device on your nodes. In this example,
`bronze` caps that device at 50 MB/s for reads and 10 MB/s for writes, and sets
separate limits on read and write operations.

The plugin validates the class file and discovers devices at startup. Restart it
after changing the file or devices. Recreate existing containers to apply the
updated classes.

## Developer's guide

### Prerequisites

- Linux with cgroup v2 and the `io` controller enabled for workload cgroups.
- For classes with weights, enable the BFQ I/O scheduler on the target devices.
  Per-device weights require Linux 5.4 or later. Container creation fails if the
  runtime cannot write `io.bfq.weight`.
- Enable external NRI plugin connections in the container runtime. Use an OCI
  runtime that supports multiline cgroup v2 Unified settings.
- Give the plugin access to the host's `/dev`, including any configured
  `/dev/disk` symlinks. The plugin reads device metadata and the runtime applies
  the cgroup settings.

### Configure containerd

To let NRI manage block I/O, disable other block I/O managers and clear
`blockio_config_file` in `[plugins."io.containerd.service.v1.tasks-service"]`.
Set the following option so containerd accepts class annotations when its own
class configuration is disabled:

```toml
[plugins."io.containerd.cri.v1.runtime".containerd]
  ignore_blockio_not_enabled_errors = true
```

On containerd 1.7, use `[plugins."io.containerd.grpc.v1.cri".containerd]`.
Restart containerd after changing its configuration.

### Build and run

Run from the repository root. Update the device patterns in the sample file
for the node and choose an unused two-digit plugin index.

```sh
make PLUGINS=nri-blockio build-plugins
./build/bin/nri-blockio --name blockio --idx 50 \
  --config sample-configs/nri-blockio.yaml --v
```

### Deploy

From the repository root, install the chart using the class file you prepared
for the target nodes. See the [Block I/O Helm chart](deployment/helm/blockio.md).

```sh
helm install nri-blockio ./deployment/helm/blockio -n kube-system \
  --set-file config=sample-configs/nri-blockio.yaml
```

### Validation

```sh
go test -race ./cmd/plugins/blockio
```

To check `io.bfq.weight` and `io.max` in a test VM, build the plugin and its
image from the repository root, then run the suite. See the
[end-to-end test guide](blob:/test/e2e/README.md).

```sh
make PLUGINS=nri-blockio build-plugins
make PLUGINS=nri-blockio BINARIES= OTHER_IMAGE_TARGETS= images
cd test/e2e
./run_tests.sh io.test-suite
```

The suite uses containerd by default. Set `k8scri=crio` to test CRI-O.
