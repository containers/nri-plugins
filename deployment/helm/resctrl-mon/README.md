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

### Telemetry options

| Name                                   | Default   | Description                                                        |
| -------------------------------------- | --------- | ------------------------------------------------------------------ |
| `telemetry.prometheus.enabled`         | `true`    | expose a `/metrics` Prometheus endpoint                            |
| `telemetry.prometheus.listenAddress`   | `":9100"` | address:port for the Prometheus HTTP listener                      |
| `telemetry.prometheus.scrapeInterval`  | `"15s"`   | recommended scrape interval (set via pod annotation hint)          |
| `telemetry.prometheus.namespace`       | `""`      | Prometheus metric prefix. Leave empty: a non-empty value renames every series and breaks the bundled dashboards (see note below). |
| `telemetry.otlp.enabled`              | `false`   | push metrics via OTLP                                              |
| `telemetry.otlp.endpoint`             | `""`      | OTLP receiver endpoint (e.g. `otel-collector-resctrl.monitoring.svc:4317`) |
| `telemetry.otlp.protocol`             | `grpc`    | `grpc` or `http`                                                   |
| `telemetry.otlp.interval`             | `15s`     | OTLP export interval                                               |
| `telemetry.otlp.insecure`             | `true`    | disable TLS for OTLP connection                                    |
| `telemetry.perfCounters.enabled`      | `false`   | gate `rdt=perf` counters (c1_res, stalls_*, etc.)                  |
| `telemetry.perfCounters.include`      | `[]`      | glob patterns for counters to include                              |
| `telemetry.perfCounters.exclude`      | `[]`      | glob patterns for counters to exclude                              |
| `telemetry.resourceAttributes`        | `{}`      | static OTel resource attributes added to all metrics               |

> **Note:** `telemetry.prometheus.namespace` prefixes every exported metric
> name with `<namespace>_`. The bundled Grafana dashboards query the unprefixed
> names (`l3_*`/`perf_*`), so setting a non-empty value renames every series and
> breaks those dashboards. Leave it empty unless you are supplying your own
> dashboards that account for the prefix.

## Prometheus Integration

The DaemonSet pods are annotated with `prometheus.io/scrape: "true"` so that
standard Prometheus service-discovery configurations will pick them up
automatically. The `prometheus.io/interval` annotation is set to the
configured `telemetry.prometheus.scrapeInterval` (default 15s) as a hint,
but note that most Prometheus deployments do not honor this annotation
without additional relabel configuration.

If you need a specific scrape interval, configure a dedicated scrape job
in your Prometheus configuration with the desired `scrape_interval`.

## Runtime Requirements

| Component        | Minimum Version | Notes                                                                 |
| ---------------- | --------------- | --------------------------------------------------------------------- |
| Linux kernel     | 5.x+            | CMT/MBM on 5.x+; AET perf/energy counters need `rdt=perf` (pending upstream). |
| containerd       | 1.7.0+          | NRI support required.                                                 |
| CRI-O            | 1.36.0+         | Provides container PIDs via NRI `LinuxContainer.Pid`.                 |
| Kubernetes       | 1.24+           | DaemonSet and NRI socket conventions.                                 |
| kube-state-metrics | —             | Required by the bundled Grafana dashboards for `kube_pod_info`.       |
| CPU              | Intel RDT       | CMT/MBM for bandwidth/LLC counters; AET for energy/perf counters.     |

### Kernel feature matrix

| Counter family                  | Kernel Kconfig                | Available since |
| ------------------------------- | ----------------------------- | --------------- |
| `llc_occupancy`, `mbm_*`       | `CONFIG_X86_CPU_RESCTRL`      | 5.x             |
| `c1_res`, `stalls_*`, `energy_*` | `CONFIG_X86_CPU_RESCTRL` + `rdt=perf` boot param | pending (under review) |

> **Note:** `rdt=perf` kernel support is still under review upstream and is not
> yet part of a released kernel. The "Available since" version for these
> counters is TBD and will be recorded once the change lands.

## Optional: OTel Collector agent

When using OTLP push mode (`telemetry.otlp.enabled=true`), you may deploy an
OTel Collector agent to receive, enrich, and fan out the metrics. Reference
manifests are provided in `optional/`:

```sh
kubectl apply -f optional/otel-collector-rbac.yaml
kubectl apply -f optional/otel-collector-agent.yaml
```

The reference config uses the `k8sattributes` processor to attach pod/namespace
labels and a Prometheus exporter on port 8889. Customize
`otel-collector-agent.yaml` to add additional exporters (e.g. `otlphttp` to a
remote backend).
