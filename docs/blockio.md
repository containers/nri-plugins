# Block I/O NRI plugin

The `nri-blockio` plugin assigns block I/O classes to containers using
goresctrl class configuration and workload annotations. The plugin owns the class
definitions and sends concrete cgroup v2 settings through NRI's `Unified`
resource fields. The container runtime does not need its own class definitions.
The plugin does not use NRI's runtime-resolved `BlockioClass` field.

## Prerequisites

- Linux with cgroup v2, the `io` controller enabled for workload cgroups, and
  the BFQ I/O scheduler. Weights are always written to `io.bfq.weight`,
  including per-device weights, which needs kernel v5.4 (commit
  `795fe54c2a8`) or later, for example RHEL 9+ or Ubuntu 20.04+. On a node
  without BFQ the `io.bfq.weight` file does not exist, the runtime cannot apply
  the settings, and container creation fails.
- An NRI-enabled container runtime accepting external plugin connections.
- An OCI runtime supporting multiline Unified writes, including current runc
  and crun. Each line is a separate write to the corresponding cgroup file.
- Access to the host's block devices at `/dev`, including any configured
  `/dev/disk` symlinks, and read access to host `/sys`. Containerized
  deployments must expose these host paths; a private container `/dev` does not
  contain the backing devices.

The plugin only reads device metadata. The runtime writes the returned settings
to the container's cgroup, so the plugin does not open block devices for I/O and
writes no cgroup files. It needs no capabilities and runs unprivileged.

## Class configuration

Pass a plugin-local YAML file with `--config`. Its format is defined by
[goresctrl](https://github.com/intel/goresctrl/blob/v0.14.0/doc/blockio.md)
and is shared with containerd's `blockio_config_file`. The schema is defined by
`Config` and `DevicesParameters` in [goresctrl's `pkg/blockio/config.go`](https://github.com/intel/goresctrl/blob/v0.14.0/pkg/blockio/config.go#L19-L33).
Class definitions in NRI's runtime-supplied plugin configuration are not
supported; use the local file instead.

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

Weights range from 10 to 1000. A `Weight` entry without `Devices` specifies
the default weight; entries with `Devices` can specify per-device weights
and limits. Device paths support globs and symlinks. Entries are processed in
order: later entries override earlier values for the same device and field.
Quantities use Kubernetes quantity syntax: `50M` means 50,000,000 and `10Mi`
means 10,485,760. A zero throttle value removes that limit (`max` in v2).
Omitted fields do not add a setting.

The plugin validates every class at startup. Unknown YAML fields, invalid
quantities, out-of-range weights, and throttling without a device list prevent
startup. Each device pattern must match at least one block device, and every
match must resolve to a block device; empty device lists and unresolved symlinks
also prevent startup. A missing or unreadable configured file, or invalid YAML,
is an error. The plugin rejects the configuration if any class fails validation.
No `--config` file disables adjustments.

Classes and device discovery are loaded at startup. Restart the plugin after
changing the file or devices. The new startup configuration applies only to
containers created after the restart. Existing containers keep their settings,
matching CRI-O's built-in class configuration behavior. Recreate workload
containers to apply changed or removed classes to them. The plugin does not
reload configuration files while running.

## Workload annotations

```yaml
metadata:
  annotations:
    blockio.resources.beta.kubernetes.io/pod: bronze
    blockio.resources.beta.kubernetes.io/container.app: gold
    blockio.resources.beta.kubernetes.io/container.sidecar: ""
```

Here `app` uses `gold`, `sidecar` has no class, and other containers use
`bronze`. Class selection follows this precedence:

1. CRI container annotation `io.kubernetes.cri.blockio-class`.
2. Pod annotation `blockio.resources.beta.kubernetes.io/container.<name>`.
3. Pod annotation `blockio.resources.beta.kubernetes.io/pod`.

An explicitly empty value overrides a lower-priority selection. Unknown
classes are logged and produce no adjustment; they do not fall back to
another annotation. Containers without a selected class are unchanged.

## Weights and throttling

Throttling becomes `io.max` entries keyed by device major/minor. Multiple
limits for one device share a line; multiple devices are separated by
newlines. Unrelated resource fields are not adjusted.

Weights are sent unchanged to `io.bfq.weight`, the default weight as a bare
number and per-device weights as `major:minor weight` lines, the same syntax
BFQ accepts. The plugin does not probe the node: it assumes BFQ with per-device
weight support is available, so there is no `io.weight` fallback and no
conversion from the OCI weight range.

That assumption is a hard requirement. If `io.bfq.weight` does not exist on a
node, the runtime fails to apply the settings and container creation fails,
because Unified values are written verbatim. Check `bfq` in
`/sys/block/<device>/queue/scheduler` and enable it with the `elevator=` kernel
command line argument or sysfs before deploying to a node.

Weights express relative shares under contention, not bandwidth ceilings.
`io.max` does not require BFQ. Kernel/runtime errors applying returned settings
are reported by the runtime.

For zero throttle values, the plugin writes `max`. Forwarding `0` through
structured OCI resources can fail when the kernel rejects it. The e2e tests
check this intentional translation explicitly.

## Build and run

```sh
make PLUGINS=nri-blockio build-plugins
./build/bin/nri-blockio --name blockio --idx 50 \
  --config sample-configs/nri-blockio.yaml --v
```

Choose an unused two-digit index. The default NRI socket is
`/var/run/nri/nri.sock`. Run the command as an external service under your
node's process supervisor so it restarts when the runtime disconnects.

Build the conventional image with:

```sh
make PLUGINS=nri-blockio BINARIES= OTHER_IMAGE_TARGETS= images
```

The image expects its class file at `/etc/nri/blockio/config.yaml` and uses
index 50. Mount a replacement file and the host paths described above when
running it externally.

### Deploy with Helm

The [blockio Helm chart](deployment/helm/blockio.md) mounts the class configuration
from a ConfigMap and exposes the required host paths:

```sh
helm install nri-blockio ./deployment/helm/blockio -n kube-system \
  --set-file config=sample-configs/nri-blockio.yaml
```

A `helm upgrade` that changes the class configuration triggers a rolling plugin
restart. Containers created afterwards use the updated classes; existing
containers keep their settings. The chart also accepts a `config` map in a values
file.
There is no operator integration.

Before migrating, clear containerd's
`[plugins."io.containerd.service.v1.tasks-service"].blockio_config_file` and
disable other plugins that manage the same I/O settings. Containerd may
otherwise reject its recognized block I/O annotations before NRI runs. For
containerd 2.x, enable its existing soft mode:

```toml
[plugins."io.containerd.cri.v1.runtime".containerd]
  ignore_blockio_not_enabled_errors = true
```

For containerd 1.7 the corresponding section is
`[plugins."io.containerd.grpc.v1.cri".containerd]`. Apply runtime configuration
changes using your normal runtime restart procedure.

## Validation

Run the focused tests with:

```sh
go test -race ./cmd/plugins/blockio
go vet ./cmd/plugins/blockio
```

The Go tests live alongside the plugin and are included in `make test`, like
the other plugins' unit tests. Runtime tests use the existing VM-based
[e2e framework](blob:/test/e2e/README.md), under
`test/e2e/io.test-suite/blockio/n4c16/`, reusing the existing topology and VM:

```sh
make PLUGINS=nri-blockio build-plugins
make PLUGINS=nri-blockio BINARIES= OTHER_IMAGE_TARGETS= images
cd test/e2e
k8scri=crio ./run_tests.sh io.test-suite
```

The deployment playbook copies the binary, image, and Helm chart into the VM.
The VM needs cgroup v2, loop-device support, and BFQ available for loop devices. The shared `*.source.sh` helpers use
`vm-command` for privileged operations and collect diagnostics through the
framework. The `test01-classes` test compares private runtime instances
using runtime-side classes and plugin-side classes, with both runc and crun.

- `test01-classes`: two devices, all four throttles, overlapping device rules,
  default/device weights, annotation precedence, unknown/empty classes,
  zero-to-max, and startup-only loading.
- `test03-helm-config`: Helm install/upgrade/uninstall against the Kubernetes
  runtime, checking that newly created containers use updated weights and
  throttles while existing containers retain their settings across rollouts.

The tests compare normalized `io.max`, `io.bfq.weight`, and `io.weight` values
and check that plugin-mode OCI resources do not contain `blockIO`. Their
cleanup restores the loop devices' schedulers, detaches the devices, and stops
the private runtimes, including after failures. They leave the VM's Kubernetes
runtime configuration and physical disk schedulers unchanged. Runtime/plugin
logs, versions, OCI inspection, and cgroup snapshots are copied into the
test's output directory under `blockio/`; verdicts use the normal e2e summary.

Verify compatibility with the deployment's runtime versions. This comparison
suite uses `k8scri=crio`; containerd with runtime-side classes disabled and soft
mode enabled still needs separate live validation.
