# Memory-QoS Plugin

This chart deploys memory-qos Node Resource Interface (NRI) plugin. The memory-qos NRI plugin
adds two methods for controlling cgroups v2 memory.* parameters: QoS class and direct memory
annotations.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.0.0+
- Container runtime:
    - containerD:
        - At least [containerd 1.7.0](https://github.com/containerd/containerd/releases/tag/v1.7.0)
            release version to use the NRI feature.

        - Enable NRI feature by following [these](https://github.com/containerd/containerd/blob/main/docs/NRI.md#enabling-nri-support-in-containerd)
          detailed instructions. You can optionally enable the NRI in containerd using the Helm chart
          during the chart installation simply by setting the `nri.patchRuntimeConfig` parameter.
          For instance,

          ```sh
          helm install my-memory-qos nri-plugins/nri-memory-qos --set nri.patchRuntimeConfig=true --namespace kube-system
          ```

          Enabling `nri.patchRuntimeConfig` creates an init container to turn on
          NRI feature in containerd and only after that proceed the plugin installation.

    - CRI-O
        - At least [v1.26.0](https://github.com/cri-o/cri-o/releases/tag/v1.26.0) release version to
            use the NRI feature
        - Enable NRI feature by following [these](https://github.com/cri-o/cri-o/blob/main/docs/crio.conf.5.md#crionri-table) detailed instructions.
          You can optionally enable the NRI in CRI-O using the Helm chart
          during the chart installation simply by setting the `nri.patchRuntimeConfig` parameter.
          For instance,

          ```sh
          helm install my-memory-qos nri-plugins/nri-memory-qos --namespace kube-system --set nri.patchRuntimeConfig=true
          ```

## Installing the Chart

Path to the chart: `nri-memory-qos`.

```sh
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-memory-qos nri-plugins/nri-memory-qos --namespace kube-system
```

The command above deploys memory-qos NRI plugin on the Kubernetes cluster within the
`kube-system` namespace with default configuration. To customize the available parameters
as described in the [Configuration options]( #configuration-options) below, you have two
options: you can use the `--set` flag or create a custom values.yaml file and provide it
using the `-f` flag. For example: 

```sh
# Install the memory-qos plugin with custom values provided using the --set option
helm install my-memory-qos nri-plugins/nri-memory-qos --namespace kube-system --set nri.patchRuntimeConfig=true
```

```sh
# Install the memory-qos plugin with custom values specified in a custom values.yaml file
cat <<EOF > myPath/values.yaml
nri:
  patchRuntimeConfig: true

tolerations:
- key: "node-role.kubernetes.io/control-plane"
  operator: "Exists"
  effect: "NoSchedule"
EOF

helm install my-memory-qos nri-plugins/nri-memory-qos --namespace kube-system -f myPath/values.yaml
```

## Uninstalling the Chart

To uninstall the memory-qos plugin run the following command:

```sh
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
