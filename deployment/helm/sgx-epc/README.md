# SGX EPC Limit Plugin

This chart deploys the sgx-epc Node Resource Interface (NRI) plugin. This plugin
can be used to set limits on the encrypted page cache usage of containers using
annotations and (a yet to be merged pull request to) the cgroup v2 misc controller.

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
          helm install my-sgx-epc nri-plugins/nri-sgx-epc --set nri.patchRuntimeConfig=true --namespace kube-system
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
          helm install my-sgx-epc nri-plugins/nri-sgx-epc --namespace kube-system --set nri.patchRuntimeConfig=true
          ```

## Installing the Chart

Path to the chart: `nri-sgx-epc`.

```sh
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-sgx-epc nri-plugins/nri-sgx-epc --namespace kube-system
```

The command above deploys sgx-epc NRI plugin on the Kubernetes cluster within the
`kube-system` namespace with default configuration. To customize the available parameters
as described in the [Configuration options]( #configuration-options) below, you have two
options: you can use the `--set` flag or create a custom values.yaml file and provide it
using the `-f` flag. For example:

```sh
# Install the sgx-epc plugin with custom values provided using the --set option
helm install my-sgx-epc nri-plugins/nri-sgx-epc --namespace kube-system --set nri.patchRuntimeConfig=true
```

```sh
# Install the sgx-epc plugin with custom values specified in a custom values.yaml file
cat <<EOF > myPath/values.yaml
nri:
  patchRuntimeConfig: true

tolerations:
- key: "node-role.kubernetes.io/control-plane"
  operator: "Exists"
  effect: "NoSchedule"
EOF

helm install my-sgx-epc nri-plugins/nri-sgx-epc --namespace kube-system -f myPath/values.yaml
```

## Uninstalling the Chart

To uninstall the sgx-epc plugin run the following command:

```sh
helm delete my-sgx-epc --namespace kube-system
```

## Configuration options

The tables below present an overview of the parameters available for users to customize with their own values,
along with the default values.

| Name                     | Default                                                                                                                       | Description                                          |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`             | [ghcr.io/containers/nri-plugins/nri-sgx-epc](https://ghcr.io/containers/nri-plugins/nri-sgx-epc)                                | container image name                                 |
| `image.tag`              | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy`       | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`          | 25m                                                                                                                           | cpu resources for the Pod                            |
| `resources.memory`       | 100Mi                                                                                                                         | memory qouta for the                                 |
| `nri.patchRuntimeConfig` | false                                                                                                                         | enable NRI in containerd or CRI-O                    |
| `initImage.name`         | [ghcr.io/containers/nri-plugins/config-manager](https://ghcr.io/containers/nri-plugins/config-manager)                                | init container image name                            |
| `initImage.tag`          | unstable                                                                                                                      | init container image tag                             |
| `initImage.pullPolicy`   | Always                                                                                                                        | init container image pull policy                     |
| `tolerations`            | []                                                                                                                            | specify taint toleration key, operator and effect    |
