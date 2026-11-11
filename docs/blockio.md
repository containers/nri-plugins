# Block I/O NRI plugin

The `nri-blockio` plugin assigns block I/O classes to containers using the
same class configuration and annotations as CRI-O. The plugin owns the class
definitions and sends concrete cgroup v2 settings through NRI's `Unified`
resource fields. CRI-O and containerd do not need their own class definitions.
The plugin does not use NRI's runtime-resolved `BlockioClass` field.

## Prerequisites

- Linux with cgroup v2 and the `io` controller enabled for workload cgroups.
- An NRI-enabled CRI-O or containerd accepting external plugin connections.
- An OCI runtime supporting multiline Unified writes, including current runc
  and crun. Each line is a separate write to the corresponding cgroup file.
- Access to the host's block devices at `/dev`, including any configured
  `/dev/disk` symlinks, and read access to host `/sys`, including the cgroup
  hierarchy at `/sys/fs/cgroup`. Containerized deployments must expose these
  host paths; a private container `/dev` does not contain the backing devices.

The plugin only reads device metadata and cgroup capabilities. The runtime
writes the returned settings to the new container's cgroup. The plugin does
not open block devices for I/O or write to cgroup files.

## Class configuration

Pass a plugin-local YAML file with `--config`. Its format is the same as
CRI-O's `blockio_config_file` and containerd's `blockio_config_file`, implemented
by [goresctrl](https://github.com/intel/goresctrl/blob/main/doc/blockio.md).
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
This intentionally differs from CRI-O's OCI path, which forwards `0` and can
fail container creation when the kernel rejects it. Omitted fields do not
add a setting.

Like CRI-O, configuration loading accepts unknown YAML fields and uses
goresctrl's forced validation. Invalid quantities, unmatched devices, and
throttling without a device list produce warnings; valid parameters can
still be applied. A missing/unreadable configured file or invalid YAML is
an error. No `--config` file disables adjustments.

By default, classes and device discovery are loaded at startup. Restart the
plugin after changing the file or devices. With `--blockio-reload`, the plugin
rereads the file and rescans devices before resolving each nonempty class
selection, including unknown class names. Like CRI-O, read/parse errors log
a warning and skip block I/O adjustments for that container; creation
continues without applying the previous snapshot. Unannotated containers do not trigger reloads.
Neither mode updates existing containers. Weight capability detection is
performed at startup; restart the plugin after loading or unloading BFQ.

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

Weight translation follows runc's structured OCI BlockIO handling:

- When BFQ is available, default and supported per-device weights are sent
  unchanged to `io.bfq.weight`.
- Otherwise, the default weight is converted to `io.weight` with
  `1 + (weight - 10) * 9999 / 990`. For example, 100 becomes 910.
- Per-device weights are omitted with a warning if BFQ device weights are
  unavailable, matching runc. Older BFQ interfaces only support a default
  weight. The plugin probes the mounted hierarchy and its immediate children
  because the kernel does not expose `io.bfq.weight` on the hierarchy root.

Weights express relative shares under contention, not bandwidth ceilings.
BFQ weights require the BFQ scheduler on the relevant device. The fallback
`io.weight` mechanism requires suitable host I/O controller configuration,
such as the I/O cost controller, to provide proportional sharing. `io.max`
does not require BFQ. Kernel/runtime errors applying returned settings are
reported by the runtime.

The class configuration and annotation behavior follows CRI-O, but the
Unified translation is implemented by the plugin. One known runtime
difference is unsupported per-device BFQ weights: runc omits them, while
crun's structured BlockIO path can fail container creation. This plugin
uses runc's omission behavior for both runtimes. Future OCI runtime changes
may require updating the translation.

Another intentional difference is zero throttling: the plugin writes `max`
and permits container creation, while CRI-O's structured OCI path writes `0`,
which the tested kernel rejects. This exception is checked explicitly by the
e2e tests rather than treated as a parity failure.

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
running it externally. There is no Helm chart or operator integration.

Before migrating, clear CRI-O's `blockio_config_file` or containerd's
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
[e2e framework](../test/e2e/README.md), under
`test/e2e/io.test-suite/blockio/n1c2/`:

```sh
make PLUGINS=nri-blockio build-plugins
cd test/e2e
k8scri=crio ./run_tests.sh io.test-suite
```

The deployment playbook copies the binary into the VM; these tests do not
need a plugin image or Helm chart. The VM needs cgroup v2, loop-device support,
and BFQ available for loop devices. The shared `*.source.sh` helpers use
`vm-command` for privileged operations and collect diagnostics through the
framework. Each `code.var.sh` test compares private CRI-O instances using
runtime-side classes and plugin-side classes, with both runc and crun.

- `test01-classes`: two devices, all four throttles, overlapping device rules,
  default/device weights, annotation precedence, unknown/empty classes,
  zero-to-max, and startup-only loading.
- `test02-reload`: successful reload, malformed/missing configuration,
  unannotated containers during a failed reload, and zero-to-max.

The tests compare normalized `io.max`, `io.bfq.weight`, and `io.weight` values
and check that plugin-mode OCI resources do not contain `blockIO`. Their
cleanup restores the loop devices' schedulers, detaches the devices, and stops
the private runtimes, including after failures. They leave the VM's Kubernetes
runtime configuration and physical disk schedulers unchanged. Runtime/plugin
logs, versions, OCI inspection, and cgroup snapshots are copied into the
test's output directory under `blockio/`; verdicts use the normal e2e summary.

The non-BFQ weight fallback is covered by unit tests. Its live behavior needs
a node without the BFQ cgroup interface; runtime compatibility should be
verified on the deployment's runtime versions. This comparison suite requires
CRI-O; containerd with runtime-side classes disabled and soft mode enabled
still needs separate live validation.
