# Memtierd Plugin

This chart deploys memtierd Node Resource Interface (NRI) plugin. The memtierd NRI plugin enables
managing workloads with Memtierd in Kubernetes.

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
          helm install my-memtierd nri-plugins/nri-memtierd --set nri.patchRuntimeConfig=true --namespace kube-system
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
          helm install my-memtierd nri-plugins/nri-memtierd --namespace kube-system --set nri.patchRuntimeConfig=true
          ```

## Installing the Chart

Path to the chart: `nri-memtierd`.

```sh
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-memtierd nri-plugins/nri-memtierd --namespace kube-system
```

The command above deploys memtierd NRI plugin on the Kubernetes cluster within the
`kube-system` namespace with default configuration. To customize the available parameters
as described in the [Configuration options]( #configuration-options) below, you have two
options: you can use the `--set` flag or create a custom values.yaml file and provide it
using the `-f` flag. For example: 

```sh
# Install the memtierd plugin with custom values provided using the --set option
helm install my-memtierd nri-plugins/nri-memtierd --namespace kube-system --set nri.patchRuntimeConfig=true
```

```sh
# Install the nri-memtierd plugin with custom values specified in a custom values.yaml file
cat <<EOF > myPath/values.yaml
nri:
  patchRuntimeConfig: true

tolerations:
- key: "node-role.kubernetes.io/control-plane"
  operator: "Exists"
  effect: "NoSchedule"
EOF

helm install my-memtierd nri-plugins/nri-memtierd --namespace kube-system -f myPath/values.yaml
```

## Uninstalling the Chart

To uninstall the memtierd plugin run the following command:

```sh
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
