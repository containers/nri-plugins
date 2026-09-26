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
   reconciler additionally retries removals that failed transiently and clears
   untracked directories left behind by an earlier plugin process.

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
    # and breaks the bundled Grafana dashboards (which query l3_*/perf_*).
    namespace: ""
  otlp:
    enabled: false
    endpoint: ""             # e.g. "otel-collector-resctrl.monitoring.svc:4317"
    protocol: grpc           # grpc | http
    interval: 15s            # must be a positive duration
    insecure: true
  perfCounters:
    enabled: false           # gate rdt=perf counters (c1_res, stalls_*, etc.)
    include: []              # glob patterns for counters to include
    exclude: []              # glob patterns for counters to exclude
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

Labels (OTLP uses the dotted attribute names, e.g. `k8s.pod.uid`):

- `domain_id`: domain instance, e.g. `00`.
- `domain_name`: domain directory, e.g. `mon_L3_00`.
- `k8s_pod_uid`: the pod UID (the `mon_group` name).
- `resctrl_control_group`: the parent control group (RDT class); empty for the
  root group.
- `resctrl_group_source`: always `pod`.
- `k8s_node_name`: the node name, from the `NODE_NAME` environment variable.
- Each key in `telemetry.resourceAttributes`. A key must not map to one of the
  labels above (for example `k8s.pod.uid` or `domain_id`) or to another key's
  label; such a configuration is rejected.

## Coexistence with Allocation Plugins

If an NRI resource allocation plugin (balloons, topology-aware) is running,
it assigns containers to RDT classes via `SetLinuxRDTClass`. The resctrl-mon
plugin reads the effective RDT class from the NRI container spec and creates
`mon_groups` under the corresponding control group:

```
/sys/fs/resctrl/<rdt-class>/mon_groups/<pod-uid>/
```

The container keeps its CLOSID (allocation) and gets a distinct RMID
(monitoring). If no allocation plugin is active, `mon_groups` are created
under the root resctrl directory.

## RMID Management

RMID allocation is delegated entirely to the Linux kernel:

- **Allocation**: `mkdir` on a `mon_group` directory assigns an RMID. If
  none are available, the kernel returns `ENOSPC` and the plugin logs a
  warning and skips the pod.
- **Deallocation**: `rmdir` releases the RMID. The kernel handles the
  hardware recycling window.

## Limitations

- **PID assignment moves one task.** Writing a PID to a `tasks` file moves
  only that thread. Threads and children that already exist stay where they
  are. Containers that started while the plugin was down (adopted in
  `Synchronize`), or whose PID is assigned only in `PostStartContainer`, are
  partly attributed until they restart.
- **One control group per pod.** A pod's `mon_group` lives under one RDT class.
  A container in a different class (for example, a sidecar) is not monitored,
  because assigning it would overwrite its allocation.
- **Uninstall leaves groups behind.** Removing the plugin leaves the pods'
  `mon_groups` (and their RMIDs) in place. To release them, run this as root
  on each node. It removes every UUID-named `mon_group`, including any that
  another tool owns:

  ```bash
  find /sys/fs/resctrl -mindepth 2 -maxdepth 3 -type d -regextype posix-extended \
    -regex '.*/mon_groups/[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}' -exec rmdir {} +
  ```

- **RMID exhaustion.** When no RMID is free, the plugin logs a warning for each
  container and the pod is not monitored. Use `namespaces` or `labelSelector`
  to limit monitoring to the pods that matter.
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
