# CXL DRA Driver

This chart deploys `nri-dra-driver-cxl`, a Dynamic Resource Allocation (DRA)
driver for CXL memory and a Node Resource Interface (NRI) plugin. It
publishes the CXL memory of every node (driver `cxl.nri.io`), prepares the
claims of CXL memory pool devices (driver `cxl-pool.nri.io`), keeps
containers off the CXL memory that they did not claim, steers the memory of
the containers that opt in, and lends memory to running containers.

CAUTION: experimental. Interfaces may change.

The [CXL memory documentation](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/index.md)
describes the driver;
[Deploy and configure](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/usage.md)
has the details of this chart.

## Prerequisites

- Kubernetes 1.37+, with the
  [feature gates](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/usage.md#feature-gates)
  of the driver.
- Helm 3.0.0+
- A container runtime with CDI and NRI enabled: the driver is tested with
  containerd 2
  ([enabling NRI](https://github.com/containerd/containerd/blob/main/docs/NRI.md#enabling-nri-support-in-containerd),
  or set `nri.runtime.patchConfig`); CRI-O is not tested.
- The kernel, memory onlining and udev settings of the
  [requirements](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/usage.md#requirements).

The pod of the DaemonSet runs privileged in the host PID namespace and
mounts the kubelet plugin directories with `mountPropagation:
Bidirectional`, so that the filesystems that the driver mounts reach the
containers
([files on the node](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/usage.md#files-on-the-node)).

## Installing the Chart

Path to the chart: `nri-dra-driver-cxl`.

```sh
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-dra-driver-cxl nri-plugins/nri-dra-driver-cxl --namespace kube-system
```

The command above deploys the driver in the `kube-system` namespace with
the default configuration. Besides the DaemonSet the chart installs the
nine [DeviceClasses](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/index.md#deviceclasses),
a ClusterRole, and the
[admission policies](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/usage.md#rbac-and-admission-policies)
that let the driver change only the ResourceSlices and the lease companions
of its own node. Use `--set` or a values file with `-f` for the
[configuration options](#configuration-options). For example:

```sh
cat > myPath/values.yaml <<'VALUES'
config:
  ignoreMemdevs: ["mem0"]
  sharedMemdevs: ["0xc100e2e2"]   # shared by the claims of class cxl-local-shared
  fsTypes: [erofs, ext4]          # ext4 trusts every party that can write the devices raw
leases:
  enabled: false
VALUES
helm install my-dra-driver-cxl nri-plugins/nri-dra-driver-cxl --namespace kube-system -f myPath/values.yaml
```

## Uninstalling the Chart

Delete the pods that use CXL claims first, so that the driver can
unprepare their claims, then:

```sh
helm delete my-dra-driver-cxl --namespace kube-system
```

## Configuration options

| Name                     | Default                                                                                                                       | Description                                          |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`             | [ghcr.io/containers/nri-plugins/nri-dra-driver-cxl](https://ghcr.io/containers/nri-plugins/nri-dra-driver-cxl)                | container image name                                 |
| `image.tag`              | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy`       | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`          | 10m                                                                                                                           | cpu resources for the Pod                            |
| `resources.memory`       | 128Mi                                                                                                                         | memory quota for the Pod                             |
| `nri.runtime.config.pluginRegistrationTimeout` | ""                                                                                                      | set NRI plugin registration timeout in NRI config of containerd or CRI-O |
| `nri.runtime.config.pluginRequestTimeout`      | ""                                                                                                      | set NRI plugin request timeout in NRI config of containerd or CRI-O |
| `nri.runtime.patchConfig` | false                                                                                                                        | patch NRI configuration in containerd or CRI-O       |
| `nri.plugin.index`       | 30                                                                                                                            | NRI plugin index (`--nri-idx`)                       |
| `pool.enabled`           | true                                                                                                                          | run the pool driver `cxl-pool.nri.io` and install its DeviceClasses (`--pool`) |
| `pool.prepareTimeout`    | 30s                                                                                                                           | how long the pool driver waits for the memdev of a pool device to appear (`--pool-prepare-timeout`) |
| `leases.enabled`         | true                                                                                                                          | lend memory to running containers (`--leases`)       |
| `leases.wait`            | 120s                                                                                                                          | how long a lease request waits for Ready when it does not say, at most 1h (`--lease-wait`) |
| `leases.companionImage`  | registry.k8s.io/pause:3.10                                                                                                    | image of the companion pods of leases (`--lease-companion-image`), the only image that the admission policy of companions allows; a digest pins it |
| `leases.priorityClassNames` | []                                                                                                                         | priority classes that companion pods may have; [] allows every class but `system-node-critical` and `system-cluster-critical` |
| `leases.namespaceLabels` | {}                                                                                                                            | labels that a namespace needs so that its claims may request `cxl-lease`; {} allows every namespace |
| `leases.maxConnections`  | 64                                                                                                                            | open connections of the lease socket of a lease claim (`--lease-max-connections`) |
| `leases.idleTimeout`     | 10m                                                                                                                           | how long a connection of a lease socket may stay without a request (`--lease-idle-timeout`) |
| `leases.requestsPerMinute`| 32                                                                                                                            | lease requests of a lease claim per minute, also their burst (`--lease-requests-per-minute`) |
| `leases.maxPerPod`       | 16                                                                                                                            | leases of the containers of a pod (`--lease-max-per-pod`) |
| `leases.maxPerNode`      | 32                                                                                                                            | leases of the node (`--lease-max-per-node`) |
| `config`                 | {}                                                                                                                            | driver configuration file (`--config`)                |
| `deviceClassConfig`      | {}                                                                                                                            | class name: the body of a `MemoryConfig` (`device`, `container`, `pool`; `container` only for `cxl-lease`), rendered into the `spec.config` of the class |
| `logLevel`               | 2                                                                                                                             | log verbosity (`-v`)                                 |
| `extraEnv`               | {}                                                                                                                            | extra environment variables of the driver, name: value |
| `initContainerImage.name` | [ghcr.io/containers/nri-plugins/nri-config-manager](https://ghcr.io/containers/nri-plugins/nri-config-manager)               | init container image name                            |
| `initContainerImage.tag` | unstable                                                                                                                      | init container image tag                             |
| `initContainerImage.pullPolicy` | Always                                                                                                                 | init container image pull policy                     |
| `tolerations`            | []                                                                                                                            | specify taint toleration key, operator and effect    |
| `affinity`               | {}                                                                                                                            | specify node affinity                                |
| `nodeSelector`           | {}                                                                                                                            | specify node selector labels                         |
| `podPriorityClassNodeCritical` | true                                                                                                                    | enable [marking Pod as node critical](https://kubernetes.io/docs/tasks/administer-cluster/guaranteed-scheduling-critical-addon-pods/#marking-pod-as-critical) |

`config` is the
[configuration file](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/usage.md#configuration-file)
of the driver. `deviceClassConfig` gives the DeviceClasses of the chart a
`MemoryConfig` as a
[preset](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/usage.md#presets-in-deviceclasses),
which the claims of a class change field by field. For example, steering on
for every container with local or pool CXL memory, unless its claim says
`container: {steering: {policy: none}}`:

```yaml
deviceClassConfig:
  cxl-local:
    container: {steering: {policy: auto}}
  cxl-pool:
    container: {steering: {policy: auto}}
```

Steering is unsafe for threaded programs: read
[steering](https://github.com/containers/nri-plugins/blob/main/docs/memory/cxl/usage.md#steering)
before you turn it on for a class.
