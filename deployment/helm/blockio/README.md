# Block I/O Plugin

This chart deploys the blockio Node Resource Interface (NRI) plugin as the
`nri-blockio` DaemonSet. It assigns block I/O classes in goresctrl's format using
workload annotations and sends concrete cgroup v2 settings through NRI.

## Prerequisites

- Kubernetes 1.35+ and Helm 3.
- Linux nodes with cgroup v2, the `io` controller enabled for workload cgroups,
  and BFQ. Weights always go to `io.bfq.weight`, per-device weights included,
  which needs kernel v5.4+ (RHEL 9+, Ubuntu 20.04+). On a node without BFQ the
  runtime cannot apply the settings and container creation fails.
- An NRI-enabled container runtime accepting external plugin connections at
  `/var/run/nri/nri.sock` (or a custom host directory containing `nri.sock`).
- An OCI runtime supporting multiline Unified writes, such as current runc or crun.
- Admission policy allowing hostPath volumes. The chart exposes the real host
  `/dev` (including `/dev/disk` symlinks) and `/sys`, both mounted read-only.
  The plugin only reads metadata; the runtime applies cgroup changes. No
  privileged container is needed.
- On SELinux-enforcing nodes, policy must allow the container to read host device
  metadata and connect to the runtime's NRI socket. On OpenShift, use the
  `openshift.grant-scc` option described below.

Enable external NRI access in the runtime before installing; this chart does not
patch runtime configuration. Use an unused NRI plugin index. Disable competing
block I/O managers and runtime-side class configuration before migrating.
Containerd may also require `ignore_blockio_not_enabled_errors = true` so its
recognized block I/O annotations reach NRI without runtime-side class definitions.
See the [plugin documentation](../../../docs/blockio.md) for runtime configuration,
class syntax, annotations, and weight support requirements.

## Installing the Chart

From the repository root:

```sh
helm install my-blockio ./deployment/helm/blockio --namespace kube-system
```

The default configuration is `Classes: {}`, so no classes are defined. Supply
classes and scheduling settings in a Helm values file, for example `blockio-values.yaml`:

```yaml
config:
  Classes:
    bronze:
      - Devices:
          - /dev/nvme0n1
        ThrottleReadBps: 50M
        ThrottleWriteBps: 10M

tolerations:
  - key: node-role.kubernetes.io/control-plane
    operator: Exists
    effect: NoSchedule
```

Replace the device paths with backing block devices on your nodes, then install:

```sh
helm install my-blockio ./deployment/helm/blockio --namespace kube-system \
  -f blockio-values.yaml
```

The plugin rejects invalid class definitions and device paths at startup. Every
configured device pattern must resolve to block devices on each selected node.
Classes with per-device weights also require BFQ device weight support on those
nodes. A validation failure prevents the plugin from connecting to NRI.

Alternatively, `config` accepts a raw YAML string, so an existing class file
(with top-level `Classes:`, not a Helm values file) can be passed directly:

```sh
helm install my-blockio ./deployment/helm/blockio --namespace kube-system \
  --set-file config=sample-configs/nri-blockio.yaml
```

The chart renders `config` into `nri-blockio-config.default`, mounted at
`/etc/nri/blockio/config.yaml`. A config checksum in the pod template triggers a
rolling restart when the rendered configuration changes on `helm upgrade`:

```sh
helm upgrade my-blockio ./deployment/helm/blockio --namespace kube-system \
  -f blockio-values.yaml
```

On restart the plugin reads the configuration and discovers devices. The updated
classes apply to containers created afterwards. Existing containers retain their
settings; recreate workload containers to apply changed or removed classes to
them. Direct ConfigMap edits do not trigger a restart, and ConfigMap volume
propagation is asynchronous. Prefer Helm upgrades for managed configuration
changes.

DaemonSet readiness does not confirm NRI registration. After installing or
upgrading, confirm that the runtime reports the blockio plugin connected and
synchronized before creating annotated workloads. Containers created before
registration receive no class settings and must be recreated to apply them.

Workloads select a class with annotations, for example:

```yaml
metadata:
  annotations:
    blockio.resources.beta.kubernetes.io/pod: bronze
```

## OpenShift

Enable SCC management when installing on OpenShift:

```sh
helm install my-blockio ./deployment/helm/blockio --namespace kube-system \
  --set openshift.grant-scc=true
```

The installer must have permission to create an SCC and grant access to it. The
chart creates an SCC named `nri-blockio-<release-namespace>`, a ClusterRole that
allows its use, and a RoleBinding granting that access only to the dedicated
`nri-blockio` ServiceAccount. The DaemonSet uses this account for all installs.

The SCC permits hostPath volumes without allowing privileged containers or host
namespaces. The container disables privilege escalation and drops all
capabilities. With `openshift.grant-scc=true`, it runs as UID 0 with the `spc_t`
SELinux label to access the NRI socket and host metadata. This label grants broad
SELinux access; the container's host `/dev` and `/sys` mounts remain read-only.

The option defaults to `false`, which omits the SCC and its RBAC resources.

## Uninstalling the Chart

```sh
helm delete my-blockio --namespace kube-system
```

## Configuration options

| Name | Default | Description |
| ---- | ------- | ----------- |
| `image.name` | `ghcr.io/containers/nri-plugins/nri-blockio` | Container image name |
| `image.tag` | `unstable` (Chart.AppVersion) | Container image tag |
| `image.pullPolicy` | `Always` | Image pull policy |
| `resources.cpu` | `10m` | CPU request |
| `resources.memory` | `100Mi` | Memory request |
| `config` | `{Classes: {}}` | Class configuration map or raw YAML string (`--set-file`) |
| `nri.plugin.index` | `50` | NRI plugin index (0–99, rendered as two digits) |
| `nri.plugin.annotations` | `{}` | Extra annotations for the plugin's pod; `checksum/config` is managed by the chart |
| `nri.runtime.socketPath` | `/var/run/nri` | Host **directory** containing `nri.sock`, always mounted at `/var/run/nri` |
| `tolerations` | `[]` | Pod taint tolerations |
| `affinity` | `{}` | Pod affinity rules |
| `nodeSelector` | `{}` | Additional node labels; `kubernetes.io/os: linux` is always enforced |
| `podPriorityClassNodeCritical` | `true` | Use the `system-node-critical` priority class |
| `openshift.grant-scc` | `false` | Create a dedicated SCC and grant its use to the `nri-blockio` ServiceAccount; run the container as UID 0 with SELinux type `spc_t` |

The container runs, by default:

```sh
nri-blockio --name blockio --idx 50 --config /etc/nri/blockio/config.yaml --v
```

Resource values configure requests, not limits, following the other plugin charts.
