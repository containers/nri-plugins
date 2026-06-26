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
8. When the last container in a pod stops, the plugin removes the `mon_group`.

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
or pushed by the container runtime via NRI.

```yaml
# Path to the resctrl filesystem. Override for testing.
resctrlPath: /sys/fs/resctrl

# Namespace filter: only create mon_groups for pods in these namespaces.
# Empty list = all namespaces.
namespaces: []

# Pod label selector: only create mon_groups for pods matching these labels.
# Empty = all pods.
labelSelector: {}
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
(monitoring). If no allocation plugin is active, `mon_groups` are created
under the root resctrl directory.

## RMID Management

RMID allocation is delegated entirely to the Linux kernel:

- **Allocation**: `mkdir` on a `mon_group` directory assigns an RMID. If
  none are available, the kernel returns `ENOSPC` and the plugin logs a
  warning and skips the pod.
- **Deallocation**: `rmdir` releases the RMID. The kernel handles the
  hardware recycling window.

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
