# NRI-Plugins-Operator 

## Introduction

The nri-plugins-operator is an Ansible-based operator created with operator-sdk to manage the life cycle of the
nri-plugins. The operator deploys community maintained [nri-plugins](https://github.com/containers/nri-plugins) in
Kubernetes cluster. When operator is installed, it doesn't do anything apart from watching for custom resources called
NriPluginDeployment. When NriPluginDeployment object is created, reconciliation loops kicks off and installs the
nri-plugin specified in the NriPluginDeployment. 

## Installation

Build the operator image and push it to some registry
```shell
make -C deployment/operator docker-build docker-push IMAGE="my-registry.com/nri-plugins-operator:unstable"
```

Deploy the operator in your cluster
```shell
make -C deployment/operator deploy
```

Uninstall the operator
```shell
make -C deployment/operator undeploy
```

## Operator CRD

```YAML
apiVersion: config.nri/v1alpha1
kind: NriPluginDeployment
metadata:
  name: nriplugindeployment-sample
  namespace: kube-system
spec:
  pluginName: topology-aware
  pluginVersion: "v0.2.3"
  state: present
  values:
    nri:
      plugin:
        index: 90                       # optional
      runtime:
        patchConfig: true               # optional
        config:                         # optional
          pluginRegistrationTimeout: 5s
          pluginRequestTimeout: 2s
    tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: "Exists"
        effect: "NoSchedule"
    affinity:
      nodeAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          nodeSelectorTerms:
            - matchExpressions:
                - key: topology.kubernetes.io/disk
                  operator: In
                  values:
                    - ssd
```

- `metadata.namespace`: the same namespace is used to install the nri-plugin Helm chart.
- `spec.pluginName`: This field specifies the desired plugin to be installed, with currently accepted values including 
  topology-aware, balloons, memtierd, memory-qos, or sgx-epc.  The list of allowed nri-plugins is expected to grow as
  new plugins are introduced. The field is immutable and to deploy a different plugin you need to re-create the object
  or create a new one with different name and namespace.
- `spec.pluginVersion`: specifies the version of the plugin. If not indicated, it defaults to the latest version. The
  plugin version is mutable, and updating it will uninstall the current version before installing the updated one.
- `spec.state`: Determines whether to install (`present`) or uninstall (`absent`) the plugin.
- `spec.values`: Allows users to provide custom values for manipulating Helm chart values. This field is immutable, 
  requiring users to recreate the object to pass new values.
- `spec.values.nri.plugin.index`: A plugin index to register to NRI. The plugin index is used by NRI to determine in
  which order the plugin is hooked into pod and container lifecycle event processing with respect to any other.
- `spec.values.nri.runtime.patchConfig`: Enable/Disable NRI on the current runtime.
- `spec.values.nri.runtime.config.pluginRegistrationTimeout`: Timeout for a plugin to register itself with NRI. Ensure
  that both timeouts are either left unspecified or specified together, and `patchConfig` must be set to true to update
  the timeouts. Timeouts will have no effect if `patchConfig` is set to false.
- `spec.values.nri.runtime.config.pluginRequestTimeout`: Timeout for a plugin to handle an event/request. Timeout for
  a plugin to register itself with NRI. Ensure that both timeouts are either left unspecified or specified together, and `patchConfig` must be set to true to update
  the timeouts. Timeouts will have no effect if `patchConfig` is set to false.
- `spec.status`: Tracks the basic state of the resource and includes basic messages in case the operator encounters 
  issues while reconciling the object.
