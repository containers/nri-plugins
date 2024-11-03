# Template Policy Plugin

This chart deploys the template Node Resource Interface (NRI) plugin. The template
NRI resource policy plugin is a wireframe implementation without any actual policy
logic. It serves as a template for creating new policy implementations.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.0.0+
- Container runtime:
  - containerD:
    - At least [containerd 1.7.0](https://github.com/containerd/containerd/releases/tag/v1.7.0)
      release version to use the NRI feature.

    - Enable NRI feature by following
      [these](https://github.com/containerd/containerd/blob/main/docs/NRI.md#enabling-nri-support-in-containerd)
      detailed instructions. You can optionally enable the NRI in containerd
      using the Helm chart during the chart installation simply by setting the
      `nri.runtime.patchConfig` parameter. For instance,

      ```sh
      helm install my-template nri-plugins/nri-resource-policy-template --set nri.runtime.patchConfig=true --namespace kube-system
      ```

      Enabling `nri.runtime.patchConfig` creates an init container to turn on
      NRI feature in containerd and only after that proceed the plugin
      installation.

  - CRI-O
    - At least [v1.26.0](https://github.com/cri-o/cri-o/releases/tag/v1.26.0)
      release version to use the NRI feature
    - Enable NRI feature by following
      [these](https://github.com/cri-o/cri-o/blob/main/docs/crio.conf.5.md#crionri-table)
      detailed instructions.  You can optionally enable the NRI in CRI-O using
      the Helm chart during the chart installation simply by setting the
      `nri.runtime.patchConfig` parameter. For instance,

      ```sh
      helm install my-template nri-plugins/nri-resource-policy-template --namespace kube-system --set nri.runtime.patchConfig=true
      ```

## Installing the Chart

Path to the chart: `nri-resource-policy-template`

```sh
helm repo add nri-plugins https://containers.github.io/nri-plugins
helm install my-template nri-plugins/nri-resource-policy-template --namespace kube-system
```

The command above deploys the template NRI plugin on the Kubernetes cluster within
the `kube-system` namespace with default configuration. To customize the
available parameters as described in the [Configuration options](#configuration-options)
below, you have two options: you can use the `--set` flag or create a custom
values.yaml file and provide it using the `-f` flag. For example:

```sh
# Install the template plugin with custom values provided using the --set option
helm install my-template nri-plugins/nri-resource-policy-template --namespace kube-system --set nri.runtime.patchConfig=true
```

```sh
# Install the template plugin with custom values specified in a custom values.yaml file
cat <<EOF > myPath/values.yaml
nri:
  runtime:
    patchConfig: true
  plugin:
    index: 10

tolerations:
- key: "node-role.kubernetes.io/control-plane"
  operator: "Exists"
  effect: "NoSchedule"
EOF

helm install my-template nri-plugins/nri-resource-policy-template --namespace kube-system -f myPath/values.yaml
```

## Uninstalling the Chart

To uninstall the templates plugin run the following command:

```sh
helm delete my-template --namespace kube-system
```

## Configuration options

The tables below present an overview of the parameters available for users to
customize with their own values, along with the default values.

| Name                     | Default                                                                                                                       | Description                                          |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`             | [ghcr.io/containers/nri-plugins/nri-resource-policy-template](https://ghcr.io/containers/nri-plugins/nri-resource-policy-template)    | container image name                                 |
| `image.tag`              | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy`       | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`          | 500m                                                                                                                          | cpu resources for the Pod                            |
| `resources.memory`       | 512Mi                                                                                                                         | memory qouta for the Pod                             |
| `extraEnv`               | {}                                                                                                                            | extra environment variables to inject (string map)   |
| `hostPort`               | 8891                                                                                                                          | metrics port to expose on the host                   |
| `config`                 | see [helm chart values](tree:/deployment/helm/template/values.yaml) for the default configuration                       | plugin configuration data                            |
| `configGroupLabel`       | config.nri/group                                                                                                        | node label for grouping configuration                |
| `nri.runtime.config.pluginRegistrationTimeout` | ""                                                                                                      | set NRI plugin registration timeout in NRI config of containerd or CRI-O |
| `nri.runtime.config.pluginRequestTimeout`      | ""                                                                                                      | set NRI plugin request timeout in NRI config of containerd or CRI-O |
| `nri.runtime.patchConfig` | false                                                                                                                        | patch NRI configuration in containerd or CRI-O       |
| `nri.plugin.index`        | 90                                                                                                                           | NRI plugin index to register with            
| `nri.plugin.annotations`  | {}                                                                                                                           | extra annotations for the plugin's pod               |
| `initImage.name`         | [ghcr.io/containers/nri-plugins/config-manager](https://ghcr.io/containers/nri-plugins/config-manager)                                | init container image name                            |
| `initImage.tag`          | unstable                                                                                                                      | init container image tag                             |
| `initImage.pullPolicy`   | Always                                                                                                                        | init container image pull policy                     |
| `tolerations`            | []                                                                                                                            | specify taint toleration key, operator and effect    |
| `podPriorityClassNodeCritical` | true                                                                                                                          | enable [marking Pod as node critical](https://kubernetes.io/docs/tasks/administer-cluster/guaranteed-scheduling-critical-addon-pods/#marking-pod-as-critical)                       |
