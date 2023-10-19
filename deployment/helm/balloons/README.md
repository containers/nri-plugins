# Balloons Policy Plugin

This chart deploys balloons Node Resource Interface (NRI) plugin. The balloons NRI resource
policy plugin implements workload placement into “balloons” that are disjoint CPU pools.

## Installing the Chart

Path to the chart: `nri-resource-policy-balloons`

```
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-balloons nri-plugins/nri-resource-policy-balloons --namespace kube-system
```

The command above deploys balloons NRI plugin on the Kubernetes cluster within the
`kube-system` namespace with default configuration. 

## Uninstalling the Chart

To uninstall the balloons plugin run the following command:

```
helm delete my-balloons --namespace kube-system
```

## Configuration options

The tables below present an overview of the parameters available for users to customize with their own values,
along with the default values.

| Name                     | Default                                                                                                                       | Description                                          |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`             | [ghcr.io/containers/nri-plugins/nri-resource-policy-balloons](https://ghcr.io/containers/nri-plugins/nri-resource-policy-balloons)    | container image name                                 |
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
