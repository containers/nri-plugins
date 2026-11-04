# Block I/O Plugin

This chart deploys the Block I/O NRI plugin as the `nri-blockio` DaemonSet.

## Prerequisites

- Kubernetes 1.35+ and Helm 3.
- Linux nodes with cgroup v2 and the `io` controller enabled. Classes with
  weights require the BFQ scheduler. Per-device weights require Linux 5.4+.
- A runtime with external NRI connections enabled and an OCI runtime that
  supports multiline cgroup v2 Unified settings.
- Admission policy allowing hostPath volumes. The chart mounts the host's
  `/dev` read-only. On SELinux-enforcing nodes, allow the plugin to read device
  metadata and connect to the NRI socket.

Disable other block I/O managers before installing. For containerd, follow the
[runtime configuration steps](../../../docs/blockio.md#configure-containerd).

## Installing the Chart

From the repository root, install the chart with a class file. Adjust the sample
file's device patterns so each pattern matches block devices on every target
node. Choose an unused `nri.plugin.index` (default `50`).

```sh
helm install my-blockio ./deployment/helm/blockio --namespace kube-system \
  --set-file config=sample-configs/nri-blockio.yaml
```

Use `--set-file config=...` for a standalone class file or `-f` for a Helm
values file with the class definitions under `config.Classes`. The default
configuration has no classes.

After installing or upgrading, check that the runtime reports the plugin
connected and synchronized before creating annotated workloads. See
[workload configuration](../../../docs/blockio.md#workload-configuration).

## Updating classes

Run `helm upgrade` with the updated class file.

```sh
helm upgrade my-blockio ./deployment/helm/blockio --namespace kube-system \
  --set-file config=sample-configs/nri-blockio.yaml
```

A change to the class configuration triggers a rolling restart of the plugin.
Recreate existing workload containers to apply the updated classes to them.

## OpenShift

Add `--set openshift.grant-scc=true` to the install command to create a Security
Context Constraint (SCC) and grant its use to the plugin's service account.
The installer needs permission to create the SCC and grant access to it. With
this option, the plugin runs as UID 0 with the `spc_t` SELinux label.

## Uninstalling the Chart

```sh
helm uninstall my-blockio --namespace kube-system
```

## Configuration options

| Name | Default | Description |
| ---- | ------- | ----------- |
| `config` | `{Classes: {}}` | Class definitions as a map or YAML string |
| `nri.plugin.index` | `50` | NRI plugin index (0–99) |
| `nri.plugin.annotations` | `{}` | Extra pod annotations. The chart reserves `checksum/config`. |
| `nri.runtime.socketPath` | `/var/run/nri` | Host directory containing `nri.sock` |
| `image.name` | `ghcr.io/containers/nri-plugins/nri-blockio` | Container image name |
| `image.tag` | `unstable` (Chart.AppVersion) | Container image tag |
| `image.pullPolicy` | `Always` | Image pull policy |
| `resources.cpu` | `10m` | CPU request |
| `resources.memory` | `100Mi` | Memory request |
| `tolerations` | `[]` | Pod tolerations |
| `affinity` | `{}` | Pod affinity rules |
| `nodeSelector` | `{}` | Additional node labels. The chart always selects Linux nodes. |
| `podPriorityClassNodeCritical` | `true` | Use the `system-node-critical` priority class |
| `openshift.grant-scc` | `false` | Create and grant the plugin's SCC |
