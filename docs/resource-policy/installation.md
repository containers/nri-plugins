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
            release version to use the NRI feature.

        - Enable NRI feature by following [these](https://github.com/containerd/containerd/blob/main/docs/NRI.md#enabling-nri-support-in-containerd)
          detailed instructions. You can optionally enable the NRI in containerd using the Helm chart
          during the chart installation simply by setting the `nri.patchRuntimeConfig` parameter.
          For instance,

          ```sh
          helm install topology-aware nri-plugins/nri-resource-policy-topology-aware --namespace kube-system --set nri.patchRuntimeConfig=true
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
          helm install topology-aware nri-plugins/nri-resource-policy-topology-aware --namespace kube-system --set nri.patchRuntimeConfig=true
          ```

- Kubernetes 1.24+
- Helm 3.0.0+

## Installing the Helm Chart

1. Add the nri-plugins charts repository so that Helm install can find the actual charts.

    ```sh
    helm repo add nri-plugins https://containers.github.io/nri-plugins
    ```

1. List chart repositories to ensure that nri-plugins repo is added.

    ```sh
    helm repo list
    ```

1. Install the plugin. Replace release version with the desired version. If you wish to
   provide custom values to the Helm chart, refer to the [table](#available-parameters) below,
   which describes the available parameters that can be modified before installation.
   Parameters can be specified either using the --set option or through the -f flag along
   with the custom values.yaml file. It's important to note that specifying the namespace
   (using `--namespace` or `-n`) is crucial when installing the Helm chart. If no namespace
   is specified, the manifests will be installed in the default namespace.

    ```sh
    # Install the topology-aware plugin with default values
    helm install topology-aware nri-plugins/nri-resource-policy-topology-aware --namespace kube-system

    # Install the topology-aware plugin with custom values provided using the --set option
    helm install topology-aware nri-plugins/nri-resource-policy-topology-aware --namespace kube-system --set nri.patchRuntimeConfig=true

    # Install the topology-aware plugin with custom values specified in a custom values.yaml file
    cat <<EOF > myPath/values.yaml
    nri:
      patchRuntimeConfig: true

    tolerations:
    - key: "node-role.kubernetes.io/control-plane"
      operator: "Exists"
      effect: "NoSchedule"
    EOF

    helm install topology-aware nri-plugins/nri-resource-policy-topology-aware --namespace kube-system -f myPath/values.yaml
    ```

    The helm repository is named `nri-plugins`, and in step 1, you have the
    flexibility to choose any name when adding it. However, it's important to
    note that `nri-resource-policy-topology-aware`, which serves as the path
    to the chart, must accurately reflect the actual name of the chart. You
    can find the path to each chart in the [helm parameters table](#available-parameters).


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
helm uninstall topology-aware --namespace kube-system
```

Note: this removes DaemonSet, ConfigMap, CustomResourceDefinition, and RBAC-related objects associated with the chart.

## Available parameters

To know what are the available Helm configuration options for currently available Helm charts, you can check:
- [balloons parameters](../../deployment/helm/balloons/README.md)
- [memory-qos parameters](../../deployment/helm/memory-qos/README.md)
- [memtierd parameters](../../deployment/helm/memtierd/README.md)
- [topology-aware parameters](../../deployment/helm/topology-aware/README.md)

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
