# Memory-QoS Plugin

This chart deploys memory-qos Node Resource Interface (NRI) plugin. The memory-qos NRI plugin
adds two methods for controlling cgroups v2 memory.* parameters: QoS class and direct memory
annotations.

## Installing the Chart

Path to the chart: `nri-memory-qos`.

```
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-memory-qos nri-plugins/nri-memory-qos --namespace kube-system
```

The command above deploys memtierd NRI plugin on the Kubernetes cluster within the
`kube-system` namespace with default configuration. 

## Uninstalling the Chart

To uninstall the memory-qos plugin run the following command:

```
helm delete my-memory-qos --namespace kube-system
```

## Configuration options

The tables below present an overview of the parameters available for users to customize with their own values,
along with the default values.

| Name                     | Default                                                                                                                       | Description                                          |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`             | [ghcr.io/containers/nri-plugins/nri-memory-qos](https://ghcr.io/containers/nri-plugins/nri-memory-qos)                                | container image name                                 |
| `image.tag`              | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy`       | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`          | 10m                                                                                                                           | cpu resources for the Pod                            |
| `resources.memory`       | 100Mi                                                                                                                         | memory qouta for the                                 |
| `nri.patchRuntimeConfig` | false                                                                                                                         | enable NRI in containerd or CRI-O                    |
| `initImage.name`         | [ghcr.io/containers/nri-plugins/config-manager](https://ghcr.io/containers/nri-plugins/config-manager)                                | init container image name                            |
| `initImage.tag`          | unstable                                                                                                                      | init container image tag                             |
| `initImage.pullPolicy`   | Always                                                                                                                        | init container image pull policy                     |
| `tolerations`            | []                                                                                                                            | specify taint toleration key, operator and effect    |
