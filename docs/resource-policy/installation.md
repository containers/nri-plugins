# Installation

This repository hosts a collection of plugins of various types, one of which is the resource
policy plugins. In this example, we will demonstrate the installation process for the topology-aware
plugin, which falls under the resource policy type. The installation methods outlined
here can be applied to any other plugin hosted in this repository, regardless of its type.

Currently, there are two installation methods available.

1. [Helm](#installing-the-helm-chart)
2. [Manual](#manual-installation)

Regardless of the chosen installation method, the NRI plugin installation includes the
following components: DaemonSet, ConfigMap, CustomResourceDefinition, and RBAC-related objects.

## Prerequisites

- Container runtime:
    - containerD:
        - At least [containerd 1.7.0](https://github.com/containerd/containerd/releases/tag/v1.7.0)
            release version to use the NRI feature
        - Enable NRI feature by following [these](TODO link) detailed instructions.
    - CRI-O
        - At least [v1.26.0](https://github.com/cri-o/cri-o/releases/tag/v1.26.0) release version to
            use the NRI feature
        - Enable NRI feature by following [these](TODO link) detailed instructions.
- Kubernetes 1.24+
- Helm 3.0.0+

## Installing the Helm Chart

1. Clone the project to your local machine
    ```sh
    git clone https://github.com/containers/nri-plugins.git
    ```

1. Navigate to the project directory
    ```sh
    cd nri-plugins
    ```

1. Install the plugin using Helm. Replace release name with the desired name
   for your Helm release. In this example, we named it as topology-aware. The
   default values for topology-aware resource policy plugin are stored in
   values.yaml file. If you wish to provide custom values to the Helm
   chart, refer to the [table](#helm-parameters) below, which describes the
   available parameters that can be modified before installation. It's important
   to note that specifying the namespace (using `--namespace`) is crucial when
   installing the Helm chart. If no namespace is specified, the manifests will
   be installed in the default namespace.

    ```sh
    helm install topology-aware --namespace kube-system deployment/helm/resource-management-policies/topology-aware/
    ```

1. Verify the status of the daemonset to ensure that the plugin is running successfully

    ```bash
    kubectl get daemonset -n kube-system nri-resource-policy-topology-aware
    
    NAME                                 DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR            AGE
    nri-resource-policy-topology-aware   1         1         0       1            0           kubernetes.io/os=linux   4m33s
    ```

That's it! You have now installed the topology-aware NRI resource policy plugin using Helm.

## Uninstalling the Chart

To uninstall plugin chart just deleting it with the release name is enough:

```bash
helm delete topology-aware
```

Note: this removes DaemonSet, ConfigMap, CustomResourceDefinition, and RBAC-related objects associated with the chart.

### Helm parameters

The tables below present an overview of the parameters available for users to customize with their own values,
along with the default values, for the Topology-aware and Balloons plugins Helm charts.

#### Topology-aware

| Name               | Default                                                                                                                       | Description                                          |
| ------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`       | [ghcr.io/containers/nri-plugins/nri-resource-policy-topology-aware](ghcr.io/containers/nri-plugins/nri-resource-policy-topology-aware)    | container image name                                 |
| `image.tag`        | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy` | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`    | 500m                                                                                                                          | cpu resources for the Pod                            |
| `resources.memory` | 512Mi                                                                                                                         | memory qouta for the Pod                             | 
| `hostPort`         | 8891                                                                                                                          | metrics port to expose on the host                   |
| `config`           | <pre><code>ReservedResources:</code><br><code>  cpu: 750m</code></pre>                                                        | plugin configuration data                            |
| `nri.patchContainerdConfig`       | false                                                                                                          | enable/disable NRI in containerd.                    |
| `initImage.name`   | [ghcr.io/containers/nri-plugins/config-manager](ghcr.io/containers/nri-plugins/config-manager)                                | init container image name                            |
| `initImage.tag`    | unstable                                                                                                                      | init container image tag                             |
| `initImage.pullPolicy` | Always                                                                                                                    | init container image pull policy                     |

#### Balloons

| Name               | Default                                                                                                                       | Description                                          |
| ------------------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `image.name`       | [ghcr.io/containers/nri-plugins/nri-resource-policy-balloons](ghcr.io/containers/nri-plugins/nri-resource-policy-balloons)    | container image name                                 |
| `image.tag`        | unstable                                                                                                                      | container image tag                                  |
| `image.pullPolicy` | Always                                                                                                                        | image pull policy                                    |
| `resources.cpu`    | 500m                                                                                                                          | cpu resources for the Pod                            |
| `resources.memory` | 512Mi                                                                                                                         | memory qouta for the Pod                             | 
| `hostPort`         | 8891                                                                                                                          | metrics port to expose on the host                   |
| `config`           | <pre><code>ReservedResources:</code><br><code>  cpu: 750m</code></pre>                                                        | plugin configuration data                            |
| `nri.patchContainerdConfig`       | false                                                                                                          | enable/disable NRI in containerd.                    |
| `initImage.name`   | [ghcr.io/containers/nri-plugins/config-manager](ghcr.io/containers/nri-plugins/config-manager)                                | init container image name                            |
| `initImage.tag`    | unstable                                                                                                                      | init container image tag                             |
| `initImage.pullPolicy` | Always                                                                                                                    | init container image pull policy                     |

## Manual installation

For the manual installation we will be using templating tool to generate Kubernetes YAML manifests.
1. Clone the project to your local machine
    ```sh
    git clone https://github.com/containers/nri-plugins.git
    ```

1. Navigate to the project directory
    ```sh
    cd nri-plugins
    ```

1. If there are any specific configuration values you need to modify, navigate to the plugins
    [directory](https://github.com/containers/nri-plugins/tree/main/deployment/overlays) containing
    the Kustomization file and update the desired configuration
    values according to your environment in the Kustomization file.

1. Use kustomize to generate the Kubernetes manifests for the desired plugin and apply the generated
    manifests to your Kubernetes cluster using kubectl.

    ```sh
    kustomize build deployment/overlays/topology-aware/ | kubectl apply -f -
    ```

1. Verify the status of the DaemonSet to ensure that the plugin is running successfully

    ```bash
    kubectl get daemonset -n kube-system nri-resource-policy
    
    NAME                  DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR            AGE
    nri-resource-policy   1         1         0       1            0           kubernetes.io/os=linux   4m33s
    ```

That's it! You have now installed the topology-aware NRI resource policy plugin using kutomize.

## Manual uninstallation

To uninstall plugin manifests you can run the following command:

```sh
kustomize build deployment/overlays/topology-aware/ | kubectl delete -f -
```

Note: this removes DaemonSet, ConfigMap, CustomResourceDefinition, and RBAC-related objects associated
with the chart.
