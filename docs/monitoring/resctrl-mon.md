# Resctrl-Mon NRI Plugin

The resctrl-mon NRI plugin creates per-pod resctrl monitoring groups
(`mon_groups`) to support [Kepler](https://sustainable-computing.io/)'s
passive mode for Application Energy Telemetry (AET).

When a container is created, the plugin assigns its init process to a
`mon_group` before the process starts executing. The Linux kernel then
propagates the RMID (Resource Monitoring ID) to all child processes
automatically, eliminating the fork race that affects userspace-based
approaches.

## How It Works

1. The container runtime creates a container process (paused).
2. The NRI `PostCreateContainer` hook fires.
3. The plugin creates a `mon_group` named with the pod's UUID under
   the appropriate resctrl control group.
4. The NRI `StartContainer` hook fires with the container's init PID.
5. The plugin writes the init PID to the `mon_group`'s `tasks` file.
   (If the PID is not yet available, `PostStartContainer` retries.)
6. The runtime starts the container. All child processes inherit the RMID.
7. Kepler scans the resctrl filesystem and reads monitoring data.
8. The `mon_group` lives for the lifetime of the pod *sandbox*, not the
   individual container. It is **not** removed when a container stops or
   restarts, so the pod keeps a stable RMID across container restarts.
   (Releasing and re-allocating an RMID would hand the replacement container a
   recycled RMID whose counters still carry the previous tenant's residual,
   producing a false energy/occupancy spike.)
9. When the pod's last sandbox is removed, the plugin removes the `mon_group`
   in the NRI `RemovePodSandbox` hook. Removing an older sandbox of a live pod
   (kubelet garbage collection after a sandbox is recreated) keeps it. If the
   teardown is missed (for example while the plugin is disconnected), the next
   NRI `Synchronize` reaps the stale
   `mon_group` using the runtime's authoritative pod list. A background
   reconciler (every 30 s) additionally retries removals that failed
   transiently, clears untracked directories left behind by an earlier plugin
   process, and retries pods that found no free RMID.

The plugin DaemonSet runs with `hostPID: true` so that it can write
host-namespace PIDs to the resctrl `tasks` file. Without `hostPID`,
the kernel rejects the write with `ESRCH` because the PID does not
exist in the plugin's PID namespace.

## Mon_Group Naming

Mon_groups are named with the Kubernetes pod UID:

```
/sys/fs/resctrl/[<rdt-class>/]mon_groups/<pod-uid>/
```

This enables Kepler to correlate monitoring data with Kubernetes metadata
by querying the K8s API using the pod UID extracted from the directory name.

## Plugin Configuration

Configuration is loaded from a YAML file specified with the `-config` flag
or pushed by the container runtime via NRI. It is read once at startup; to
apply a change, restart the plugin (for example
`kubectl rollout restart daemonset/nri-resctrl-mon -n <namespace>`).

```yaml
# Path to the resctrl filesystem. Override for testing.
resctrlPath: /sys/fs/resctrl

# Namespace filter: only create mon_groups for pods in these namespaces.
# Empty list = all namespaces.
namespaces: []

# Pod label selector: only create mon_groups for pods matching these labels.
# Empty = all pods.
labelSelector: {}

# Embedded OpenTelemetry exporter. Exposes a Prometheus /metrics endpoint
# and/or pushes metrics via OTLP.
telemetry:
  prometheus:
    enabled: true
    listenAddress: ":9100"
    # Metric-name prefix. Leave empty: a non-empty value renames every series
    # and breaks the sample Grafana dashboards (which query l3_*/perf_*).
    namespace: ""
  otlp:
    enabled: false
    endpoint: ""             # e.g. "otel-collector-resctrl.monitoring.svc:4317"
    protocol: grpc           # grpc | http
    interval: 15s            # must be a positive duration
    insecure: false          # true sends OTLP in plaintext (no TLS)
  perfCounters:
    enabled: false           # gate rdt=perf counters (c1_res, stalls_*, etc.)
    include: []              # path.Match patterns for counters to include
    exclude: []              # path.Match patterns for counters to exclude
  resourceAttributes: {}     # static OTel resource attributes on all metrics
```

If telemetry cannot start (for example, its port is in use), the plugin logs
an error and keeps managing `mon_groups` without it. When the plugin runs in
the host network namespace (for example, launched by the runtime), the default
`:9100` collides with node_exporter; set `telemetry.prometheus.listenAddress`
or disable Prometheus.

## Metrics

The Prometheus endpoint exports these metrics per pod `mon_group` and domain:

| Prometheus name                 | Type    | Unit   | Source file                   |
| ------------------------------- | ------- | ------ | ----------------------------- |
| `l3_llc_occupancy_bytes`        | gauge   | bytes  | `mon_L3_*/llc_occupancy`      |
| `l3_mbm_local_bytes_total`      | counter | bytes  | `mon_L3_*/mbm_local_bytes`    |
| `l3_mbm_bytes_total`            | counter | bytes  | `mon_L3_*/mbm_total_bytes`    |
| `perf_core_energy_joules_total` | counter | joules | `mon_PERF_PKG_*/core_energy`  |
| `perf_activity_farads_total`    | counter | farads | `mon_PERF_PKG_*/activity`     |

Other `mon_PERF_PKG_*` counters (`c1_res`, `stalls_*`, ...) are exported only
when `telemetry.perfCounters.enabled` is true. Names follow goresctrl's
[naming rules](https://github.com/intel/goresctrl/blob/main/doc/resctrl-mon.md)
under the `UnderscoreEscapingWithSuffixes` translation strategy, which the
plugin pins.

The plugin also exports `resctrl_mon_errors_total`, a counter of failed
`mon_group` creations and PID assignments, with the errno in the label
`error_type` (OTLP `error.type`), for example `EACCES` or `ENOSPC`. A pod that
hits one of these errors is not monitored, or only partly (see
[Limitations](#limitations)).

Labels (OTLP uses the dotted attribute names, e.g. `k8s.pod.uid`):

- `domain_id`: domain instance, e.g. `00`.
- `domain_name`: domain directory, e.g. `mon_L3_00`.
- `k8s_pod_uid`: the pod UID (the `mon_group` name).
- `resctrl_control_group`: the parent control group (RDT class); empty for the
  root group.
- `resctrl_group_source`: always `pod`.
- `k8s_node_name`: the node name, from the `NODE_NAME` environment variable.
- Each key in `telemetry.resourceAttributes`. A key must not map to one of the
  labels above (for example `k8s.pod.uid` or `domain_id`), to `error_type`, to
  another key's label, or to a label that OTLP-to-Prometheus conversion derives:
  `job` or `service_name` (set `service.name` itself to override it), and
  `instance` when `service.instance.id` is set. Names starting with `__` are
  reserved by Prometheus. Such a configuration is rejected.

## Samples

[`deployment/samples/resctrl-mon`](https://github.com/containers/nri-plugins/tree/main/deployment/samples/resctrl-mon)
has files that the Helm chart does not deploy:

- `grafana-resctrl-pod-energy.json`, `grafana-resctrl-perf-counters.json`:
  Grafana dashboards for the metrics above. They need kube-state-metrics (for
  `kube_pod_info`) and an empty `telemetry.prometheus.namespace`.
- `otel-collector-rbac.yaml`, `otel-collector-agent.yaml`: an OTel Collector
  agent DaemonSet for the OTLP push path (`telemetry.otlp.enabled=true`). It
  adds pod and namespace labels with the `k8sattributes` processor and exposes
  the metrics to Prometheus on port 8889. Add exporters (for example
  `otlphttp`) to send them elsewhere.

Before applying the collector manifests:

- Set the two namespace placeholders in the NetworkPolicy (the plugin's and
  Prometheus's).
- Copy any `tolerations` or `nodeSelector` set for the plugin to the collector
  DaemonSet.
- Configure TLS on the collector's OTLP receivers, or set
  `telemetry.otlp.insecure=true`: the receivers do not use TLS.
- Set `telemetry.prometheus.enabled=false` if Prometheus scrapes the
  collector; otherwise it ingests every series twice.

```sh
kubectl apply -f deployment/samples/resctrl-mon/otel-collector-rbac.yaml
kubectl apply -f deployment/samples/resctrl-mon/otel-collector-agent.yaml
```

## Coexistence with Allocation Plugins

If an NRI resource allocation plugin (balloons, topology-aware) is running,
it assigns containers to RDT classes via `SetLinuxRDTClass`. The resctrl-mon
plugin reads the effective RDT class from the NRI container spec and creates
`mon_groups` under the corresponding control group:

```
/sys/fs/resctrl/<rdt-class>/mon_groups/<pod-uid>/
```

The container keeps its CLOSID (allocation) and gets a distinct RMID
(monitoring). Pods without an RDT class, from an allocation plugin or from the
runtime's RDT annotations, get `mon_groups` under the root resctrl directory.

## RMID Management

RMID allocation is delegated entirely to the Linux kernel:

- **Allocation**: `mkdir` on a `mon_group` directory assigns an RMID. If
  none are available, the kernel returns `ENOSPC` (or `EBUSY` while freed
  RMIDs wait to be reused). The plugin then logs a warning, and the
  reconciler retries the pod every 30 s until an RMID is free or the pod is
  removed.
- **Deallocation**: `rmdir` releases the RMID. The kernel handles the
  hardware recycling window.

The first `EACCES`, `EPERM` or `EINVAL` from resctrl is logged at error level,
and later ones as warnings. These errors are not retried. `EACCES` usually
means that a security profile (AppArmor or SELinux) confines the plugin.
Every failure is counted in `resctrl_mon_errors_total`.

## Limitations

- **PID assignment moves one task.** Writing a PID to a `tasks` file moves
  only that thread. Threads and children that already exist stay where they
  are. Containers that started while the plugin was down (adopted in
  `Synchronize`), whose PID is assigned only in `PostStartContainer`, or that
  waited for a free RMID, are partly attributed until they restart.
- **One control group per pod.** A pod's `mon_group` lives under one RDT class.
  A container in a different class (for example, a sidecar) is not monitored,
  because assigning it would overwrite its allocation. The plugin reads a
  container's class from its OCI `linux.intelRdt.closID` (NRI
  `Linux.Rdt.ClosId`), which containerd and CRI-O set from the pod's RDT
  annotations. The group's class is chosen as follows:
  - If the pod has the annotation `rdt.resources.beta.kubernetes.io/pod`, the
    group is under that class. A container that its own annotation
    (`rdt.resources.beta.kubernetes.io/container.<name>`) puts in another class
    is not monitored, whichever container is created first.
  - Otherwise, or when the pod-level class was not applied (for example, an NRI
    plugin set another class), the first container to be created fixes the
    class.
- **The runtime's RDT setup removes groups.** Each time containerd or CRI-O
  starts with an RDT configuration (`rdt_config_file`), it removes every
  control group that the configuration does not define and every `mon_group`
  that has no tasks. Pods with no running task (Completed, in back-off, or
  killed) lose their `mon_group`, with its RMID and counters, and the plugin
  does not re-create it. Do not keep hand-made control groups on such a host.
- **Uninstall leaves groups behind.** Removing the plugin leaves the pods'
  `mon_groups` (and their RMIDs) in place. To release them, run this as root
  on each node. It removes every UUID-named `mon_group`, including any that
  another tool owns:

  ```bash
  find /sys/fs/resctrl -mindepth 2 -maxdepth 3 -type d -regextype posix-extended \
    -regex '.*/mon_groups/[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}' -exec rmdir {} +
  ```

- **RMID exhaustion.** When no RMID is free, the plugin logs a warning for each
  container and the pod is not monitored until the reconciler gets an RMID for
  it. Only containers that have started are retried, and only while their init
  process runs. Use `namespaces` or `labelSelector` to limit monitoring to the
  pods that matter.
- **Ownership.** The plugin reconciles every UUID-named `mon_group`: it keeps
  the groups of all live pods (including filtered ones) and removes the rest.
  Other tools must not create UUID-named groups for anything else.

## Developer's Guide

### Prerequisites

- Containerd v1.7+ or CRI-O v1.36+
- Enable NRI in the container runtime:

  **containerd** — in `/etc/containerd/config.toml`:

  ```toml
  [plugins."io.containerd.nri.v1.nri"]
    disable = false
    disable_connections = false
    plugin_config_path = "/etc/nri/conf.d"
    plugin_path = "/opt/nri/plugins"
    plugin_registration_timeout = "5s"
    plugin_request_timeout = "2s"
    socket_path = "/var/run/nri/nri.sock"
  ```

  **CRI-O** — in `/etc/crio/crio.conf.d/10-nri.conf` (or equivalent):

  ```toml
  [crio.nri]
  enable_nri = true
  ```

  See the [CRI-O NRI documentation](https://github.com/cri-o/cri-o/blob/main/docs/crio.conf.5.md#crionri-table)
  for additional options.

- Intel CPU with RDT monitoring support
- resctrl filesystem mounted at `/sys/fs/resctrl`

### Build

```bash
make PLUGINS=nri-resctrl-mon build-plugins
```

### Run

```bash
./build/bin/nri-resctrl-mon -config sample-configs/nri-resctrl-mon.yaml -idx 90 -vv
```

### Manual Test

Verify that `mon_groups` are created when pods start:

```bash
# Start a test pod
kubectl run test-pod --image=busybox -- sleep 3600

# Check that a mon_group was created with the pod UID
POD_UID=$(kubectl get pod test-pod -o jsonpath='{.metadata.uid}')

# Without an RDT allocation plugin, mon_groups are under the root class:
MON_GROUP_BASE=/sys/fs/resctrl/mon_groups
# With an allocation plugin that assigns an RDT class (e.g. BestEffort):
# MON_GROUP_BASE=/sys/fs/resctrl/BestEffort/mon_groups

ls "$MON_GROUP_BASE/$POD_UID/"

# Verify monitoring data is available
cat "$MON_GROUP_BASE/$POD_UID/mon_data/mon_L3_00/llc_occupancy"
```

### Debug

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
dlv exec build/bin/nri-resctrl-mon -- -config sample-configs/nri-resctrl-mon.yaml -idx 90
(dlv) break plugin.PostCreateContainer
(dlv) continue
```

### Deploy

Build an image, import it on the node, and deploy the plugin by
running the following in `nri-plugins`:

```bash
rm -rf build
make clean
make PLUGINS=nri-resctrl-mon IMAGE_VERSION=devel images
ctr -n k8s.io images import build/images/nri-resctrl-mon-image-*.tar
kubectl create -f build/images/nri-resctrl-mon-deployment.yaml
```
