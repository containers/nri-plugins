# Resctrl-Mon Plugin

This chart deploys the resctrl-mon Node Resource Interface (NRI) plugin. The
resctrl-mon NRI plugin creates per-pod resctrl monitoring groups (mon_groups)
to support Application Energy Telemetry (AET) via Kepler passive mode.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.0.0+
- Intel CPU with RDT monitoring support (CMT/MBM and/or AET)
- resctrl filesystem mounted at `/sys/fs/resctrl`
- Container runtime:
  - containerd:
    - At least [containerd 1.7.0](https://github.com/containerd/containerd/releases/tag/v1.7.0)
      release version to use the NRI feature.

    - Enable NRI feature by following
      [these](https://github.com/containerd/containerd/blob/main/docs/NRI.md#enabling-nri-support-in-containerd)
      detailed instructions. You can optionally enable the NRI in containerd
      using the Helm chart during the chart installation simply by setting the
      `nri.runtime.patchConfig` parameter. For instance,

      ```sh
      helm install my-resctrl-mon nri-plugins/nri-resctrl-mon --set nri.runtime.patchConfig=true --namespace kube-system
      ```

      Enabling `nri.runtime.patchConfig` creates an init container to turn on
      NRI feature in containerd and only after that proceed the plugin
      installation.

  - CRI-O
    - At least [v1.36.0](https://github.com/cri-o/cri-o/releases/tag/v1.36.0)
      release version. CRI-O >= 1.36 is required because earlier versions do
      not provide container PIDs via NRI, which prevents the plugin from
      assigning tasks to monitoring groups.
    - Enable NRI feature by following
      [these](https://github.com/cri-o/cri-o/blob/main/docs/crio.conf.5.md#crionri-table)
      detailed instructions.  You can optionally enable the NRI in CRI-O using
      the Helm chart during the chart installation simply by setting the
      `nri.runtime.patchConfig` parameter. For instance,

      ```sh
      helm install my-resctrl-mon nri-plugins/nri-resctrl-mon --namespace kube-system --set nri.runtime.patchConfig=true
      ```

## Installing the Chart

Path to the chart: `nri-resctrl-mon`.

```sh
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-resctrl-mon nri-plugins/nri-resctrl-mon --namespace kube-system
```

The command above deploys resctrl-mon NRI plugin on the Kubernetes cluster
within the `kube-system` namespace with default configuration. To customize the
available parameters as described in the [Configuration options](#configuration-options)
below, you have two options: you can use the `--set` flag or create a custom
values.yaml file and provide it using the `-f` flag. For example:

```sh
# Install the resctrl-mon plugin with custom values provided using the --set option
helm install my-resctrl-mon nri-plugins/nri-resctrl-mon --namespace kube-system --set nri.runtime.patchConfig=true
```

```sh
# Install the resctrl-mon plugin with custom values specified in a custom values.yaml file
cat <<EOF > myPath/values.yaml
nri:
  runtime:
    patchConfig: true
  plugin:
    index: 90

tolerations:
- key: "node-role.kubernetes.io/control-plane"
  operator: "Exists"
  effect: "NoSchedule"
EOF

helm install my-resctrl-mon nri-plugins/nri-resctrl-mon --namespace kube-system -f myPath/values.yaml
```

## Uninstalling the Chart

To uninstall the resctrl-mon plugin run the following command:

```sh
helm delete my-resctrl-mon --namespace kube-system
```

## Security

The DaemonSet runs with `hostPID: true` because the plugin must write
host-namespace PIDs into resctrl `tasks` files. Without host PID
visibility the kernel rejects the write (`ESRCH`). The container also
requires `SYS_ADMIN` and `DAC_OVERRIDE` capabilities to manage resctrl
`mon_group` directories.

By default the container runs without an AppArmor profile (`appArmorProfile.type:
Unconfined`). The runtimes' default profiles, containerd's
`cri-containerd.apparmor.d` and CRI-O's `crio-default`, deny writes under
`/sys/fs/[^c]*`, which includes `/sys/fs/resctrl`. Under either profile the
plugin still becomes Ready, but it logs one error and then only `permission
denied` warnings, counts the failures in
`resctrl_mon_errors_total{error_type="EACCES"}`, and tracks no `mon_groups`. On
Kubernetes older than 1.30, which lacks the `appArmorProfile` field, the chart
sets the equivalent pod annotation
`container.apparmor.security.beta.kubernetes.io/nri-resctrl-mon` instead.

The Prometheus `/metrics` endpoint is unauthenticated. Collection is
rate-bounded (resctrl is read at most once per second, however many scrapes
arrive), but you should still restrict access to the endpoint with a
NetworkPolicy.

## RDT classes

A pod's `mon_group` is created under the pod's RDT class: the class in the pod
annotation `rdt.resources.beta.kubernetes.io/pod` if it is set, otherwise the
class of the pod's first container. Containers in another class, such as a
sidecar annotated with `rdt.resources.beta.kubernetes.io/container.<name>`,
are not monitored.

When containerd or CRI-O starts with an RDT configuration (`rdt_config_file`),
it removes every control group that the configuration does not define and
every `mon_group` without tasks, so pods with no running task lose their
`mon_group`. See the
[plugin documentation](https://github.com/containers/nri-plugins/blob/main/docs/monitoring/resctrl-mon.md#limitations)
for details.

## Configuration options

The tables below present an overview of the parameters available for users to
customize with their own values, along with the default values.

| Name                     | Default                                                                                                                       | Description                                          |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`             | [ghcr.io/containers/nri-plugins/nri-resctrl-mon](https://ghcr.io/containers/nri-plugins/nri-resctrl-mon)                      | container image name                                 |
| `image.tag`              | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy`       | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`          | 10m                                                                                                                           | cpu resources for the Pod                            |
| `resources.memory`       | 50Mi                                                                                                                          | memory quota for the Pod                             |
| `appArmorProfile`        | `{type: Unconfined}`                                                                                                          | AppArmor profile of the plugin container (see [Security](#security)) |
| `nri.runtime.config.pluginRegistrationTimeout` | ""                                                                                                      | set NRI plugin registration timeout in NRI config of containerd or CRI-O |
| `nri.runtime.config.pluginRequestTimeout`      | ""                                                                                                      | set NRI plugin request timeout in NRI config of containerd or CRI-O |
| `nri.runtime.patchConfig` | false                                                                                                                        | patch NRI configuration in containerd or CRI-O       |
| `nri.plugin.index`        | 90                                                                                                                           | NRI plugin index to register with                    |
| `initContainerImage.name`         | [ghcr.io/containers/nri-plugins/nri-config-manager](https://ghcr.io/containers/nri-plugins/nri-config-manager)                | init container image name                            |
| `initContainerImage.tag`          | unstable                                                                                                                      | init container image tag                             |
| `initContainerImage.pullPolicy`   | Always                                                                                                                        | init container image pull policy                     |
| `tolerations`            | []                                                                                                                            | specify taint toleration key, operator and effect    |
| `affinity`               | []                                                                                                                            | specify node affinity                                |
| `nodeSelector`           | []                                                                                                                            | specify node selector labels                         |
| `podPriorityClassNodeCritical` | true                                                                                                                    | enable [marking Pod as node critical](https://kubernetes.io/docs/tasks/administer-cluster/guaranteed-scheduling-critical-addon-pods/#marking-pod-as-critical) |
| `podMonitor.enabled`     | false                                                                                                                         | create a Prometheus Operator PodMonitor for the metrics port (see [Prometheus Integration](#prometheus-integration)) |
| `podMonitor.interval`    | ""                                                                                                                            | PodMonitor scrape interval; empty uses Prometheus's default |
| `podMonitor.labels`      | {}                                                                                                                            | extra PodMonitor labels, e.g. to match the Prometheus `podMonitorSelector` |

### Telemetry options

| Name                                   | Default   | Description                                                        |
| -------------------------------------- | --------- | ------------------------------------------------------------------ |
| `telemetry.prometheus.enabled`         | `true`    | expose a `/metrics` Prometheus endpoint                            |
| `telemetry.prometheus.listenAddress`   | `":9100"` | address:port for the Prometheus HTTP listener                      |
| `telemetry.prometheus.namespace`       | `""`      | Prometheus metric prefix. Leave empty: a non-empty value renames every series and breaks the sample dashboards (see note below). |
| `telemetry.otlp.enabled`              | `false`   | push metrics via OTLP                                              |
| `telemetry.otlp.endpoint`             | `""`      | OTLP receiver endpoint (e.g. `otel-collector-resctrl.monitoring.svc:4317`) |
| `telemetry.otlp.protocol`             | `grpc`    | `grpc` or `http`                                                   |
| `telemetry.otlp.interval`             | `15s`     | OTLP export interval                                               |
| `telemetry.otlp.insecure`             | `false`   | disable TLS for OTLP connection                                    |
| `telemetry.perfCounters.enabled`      | `false`   | gate `rdt=perf` counters (c1_res, stalls_*, etc.)                  |
| `telemetry.perfCounters.include`      | `[]`      | [`path.Match`](https://pkg.go.dev/path#Match) patterns for counters to include |
| `telemetry.perfCounters.exclude`      | `[]`      | [`path.Match`](https://pkg.go.dev/path#Match) patterns for counters to exclude |
| `telemetry.resourceAttributes`        | `{}`      | static OTel resource attributes added to all metrics               |

> **Note:** `telemetry.prometheus.namespace` prefixes every exported metric
> name with `<namespace>_`. The [sample](#samples) Grafana dashboards query the unprefixed
> names (`l3_*`/`perf_*`), so setting a non-empty value renames every series and
> breaks those dashboards. Leave it empty unless you are supplying your own
> dashboards that account for the prefix.

## Prometheus Integration

The plugin serves metrics at `/metrics` on the container port named `metrics`.
The chart does not annotate the pods for Prometheus discovery; configure
Prometheus to scrape them in one of these ways.

With the Prometheus Operator (for example kube-prometheus-stack), set
`podMonitor.enabled=true` to create a PodMonitor, and set `podMonitor.labels`
to match the Prometheus `podMonitorSelector` (for kube-prometheus-stack,
`release: <its release name>`):

```sh
helm install my-resctrl-mon nri-plugins/nri-resctrl-mon --namespace kube-system \
  --set podMonitor.enabled=true --set podMonitor.labels.release=kube-prometheus-stack
```

Without the operator, add a scrape job that keeps the plugin pods' `metrics`
port. Set the namespace to the one the chart is installed in:

```yaml
scrape_configs:
  - job_name: nri-resctrl-mon
    scrape_interval: 15s
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: [kube-system]
    relabel_configs:
      - source_labels:
          - __meta_kubernetes_pod_label_app_kubernetes_io_name
          - __meta_kubernetes_pod_container_port_name
        regex: nri-resctrl-mon;metrics
        action: keep
```

## Runtime Requirements

| Component        | Minimum Version | Notes                                                                 |
| ---------------- | --------------- | --------------------------------------------------------------------- |
| Linux kernel     | 5.x+            | CMT/MBM on 5.x+; AET perf/energy counters need `rdt=perf` (pending upstream). |
| containerd       | 1.7.0+          | NRI support required.                                                 |
| CRI-O            | 1.36.0+         | Provides container PIDs via NRI `LinuxContainer.Pid`.                 |
| Kubernetes       | 1.24+           | DaemonSet and NRI socket conventions.                                 |
| kube-state-metrics | —             | Required by the sample Grafana dashboards for `kube_pod_info`.        |
| CPU              | Intel RDT       | CMT/MBM for bandwidth/LLC counters; AET for energy/perf counters.     |

### Kernel feature matrix

| Counter family                  | Kernel Kconfig                | Available since |
| ------------------------------- | ----------------------------- | --------------- |
| `llc_occupancy`, `mbm_*`       | `CONFIG_X86_CPU_RESCTRL`      | 5.x             |
| `c1_res`, `stalls_*`, `energy_*` | `CONFIG_X86_CPU_RESCTRL` + `rdt=perf` boot param | pending (under review) |

> **Note:** `rdt=perf` kernel support is still under review upstream and is not
> yet part of a released kernel. The "Available since" version for these
> counters is TBD and will be recorded once the change lands.

## Samples

[`deployment/samples/resctrl-mon`](https://github.com/containers/nri-plugins/tree/main/deployment/samples/resctrl-mon)
has Grafana dashboards and reference manifests for an OTel Collector agent
(for `telemetry.otlp.enabled=true`). The chart does not deploy them. See the
[plugin documentation](https://github.com/containers/nri-plugins/blob/main/docs/monitoring/resctrl-mon.md#samples)
for how to use them.
