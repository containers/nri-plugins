# Memtierd Plugin

This chart deploys memtierd Node Resource Interface (NRI) plugin. The memtierd NRI plugin enables
managing workloads with Memtierd in Kubernetes.

## Installing the Chart

Path to the chart: `nri-memtierd`.

```
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-memtierd nri-plugins/nri-memtierd --namespace kube-system
```

The command above deploys memtierd NRI plugin on the Kubernetes cluster within the
`kube-system` namespace with default configuration. 

## Uninstalling the Chart

To uninstall the memtierd plugin run the following command:

```
helm delete my-memtierd --namespace kube-system
```

## Configuration options

The tables below present an overview of the parameters available for users to customize with their own values,
along with the default values.

| Name                     | Default                                                                                                                       | Description                                          |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`             | [ghcr.io/containers/nri-plugins/nri-memtierd](https://ghcr.io/containers/nri-plugins/nri-memtierd)                                    | container image name                                 |
| `image.tag`              | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy`       | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`          | 250m                                                                                                                          | cpu resources for the Pod                            |
| `resources.memory`       | 100Mi                                                                                                                         | memory qouta for the                                 |
| `outputDir`              | empty string                                                                                                                  | host directory for memtierd.output files             |
| `nri.patchRuntimeConfig` | false                                                                                                                         | enable NRI in containerd or CRI-O                    |
| `initImage.name`         | [ghcr.io/containers/nri-plugins/config-manager](https://ghcr.io/containers/nri-plugins/config-manager)                                | init container image name                            |
| `initImage.tag`          | unstable                                                                                                                      | init container image tag                             |
| `initImage.pullPolicy`   | Always                                                                                                                        | init container image pull policy                     |
| `tolerations`            | []                                                                                                                            | specify taint toleration key, operator and effect    |
