# Topology-Aware Policy Plugin

This chart deploys topology-aware Node Resource Interface (NRI) plugin. Topology-aware NRI
resource policy plugin is a NRI plugin that will apply hardware-aware resource allocation
policies to the containers running in the system.

## Installing the Chart

Path to the chart: `nri-resource-policy-topology-aware`.

```
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-topology-aware nri-plugins/nri-resource-policy-topology-aware --namespace kube-system
```

The command above deploys topology-aware NRI plugin on the Kubernetes cluster within the
`kube-system` namespace with default configuration. 

## Uninstalling the Chart

To uninstall the topology-aware plugin run the following command:

```
helm delete my-topology-aware --namespace kube-system
```

## Configuration options

The tables below present an overview of the parameters available for users to customize with their own values,
along with the default values.

| Name                     | Default                                                                                                                       | Description                                          |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`             | [ghcr.io/containers/nri-plugins/nri-resource-policy-topology-aware](https://ghcr.io/containers/nri-plugins/nri-resource-policy-topology-aware)    | container image name                     |
| `image.tag`              | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy`       | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`          | 500m                                                                                                                          | cpu resources for the Pod                            |
| `resources.memory`       | 512Mi                                                                                                                         | memory qouta for the Pod                             | 
| `hostPort`               | 8891                                                                                                                          | metrics port to expose on the host                   |
| `config`                 | <pre><code>ReservedResources:</code><br><code>  cpu: 750m</code></pre>                                                        | plugin configuration data                            |
| `nri.patchRuntimeConfig` | false                                                                                                                         | enable NRI in containerd or CRI-O                    |
| `initImage.name`         | [ghcr.io/containers/nri-plugins/config-manager](https://ghcr.io/containers/nri-plugins/config-manager)                                | init container image name                            |
| `initImage.tag`          | unstable                                                                                                                      | init container image tag                             |
| `initImage.pullPolicy`   | Always                                                                                                                        | init container image pull policy                     |
| `tolerations`            | []                                                                                                                            | specify taint toleration key, operator and effect    |
