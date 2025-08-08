# Prototyping CPU DRA device abstraction / DRA-based CPU allocation

## Background

This prototype patch set bolts a DRA allocation frontend on top of the existing
topology aware resource policy plugin. The main intention with of this patch set
is to

- provide something practical to play around with for the [feasibility study]( https://docs.google.com/document/d/1Tb_dC60YVCBr7cNYWuVLddUUTMcNoIt3zjd5-8rgug0/edit?tab=t.0#heading=h.iutbebngx80e) of enabling DRA-based CPU allocation,
- allow (relatively) easy experimentation with how to expose CPU as DRA
devices (IOW test various CPU DRA attributes)
- allow testing how DRA-based CPU allocation (using non-trivial CEL expressions)
would scale with cluster and cluster node size

## Notes

This patched NRI plugin, especially in its current state and form, is
*not a proposal* for a first real DRA-based CPU driver.

## Prerequisites for Testing

To test out this in a cluster, make sure you have

1. DRA enabled in your cluster
One way to ensure it is to bootstrap you cluster using an InitConfig with the
following bits set:

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
...
---
apiServer:
  extraArgs:
  - name: feature-gates
    value: DynamicResourceAllocation=true,DRADeviceTaints=true,DRAAdminAccess=true,DRAPrioritizedList=true,DRAPartitionableDevices=true,DRAResourceClaimDeviceStatus=true
  - name: runtime-config
    value: resource.k8s.io/v1beta2=true,resource.k8s.io/v1beta1=true,resource.k8s.io/v1alpha3=true
apiVersion: kubeadm.k8s.io/v1beta4
...
controllerManager:
  extraArgs:
  - name: feature-gates
    value: DynamicResourceAllocation=true,DRADeviceTaints=true
...
scheduler:
  extraArgs:
  - name: feature-gates
    value: DynamicResourceAllocation=true,DRADeviceTaints=true,DRAAdminAccess=true,DRAPrioritizedList=true,DRAPartitionableDevices=true
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
featureGates:
  DynamicResourceAllocation: true
```

2. CDI enabled in your runtime configuration

## Installation and Testing

Once you have your cluster properly set upset up, you can pull this in to
your cluster with for testing with something like this:

```bash
helm install --devel -n kube-system test oci://ghcr.io/klihub/nri-plugins/helm-charts/nri-resource-policy-topology-aware --version v0.9-dra-driver-unstable --set image.pullPolicy=Always --set extraEnv.OVERRIDE_SYS_ATOM_CPUS='2-5' --set extraEnv.OVERRIDE_SYS_CORE_CPUS='0\,1\,6-15'
```

Once the NRI plugin+DRA driver is up and running, you should see some CPUs
exposed as DRI devices. You can check the resource slices with the following
command

```bash
[kli@n4c16-fedora-40-cloud-base-containerd ~]# kubectl get resourceslices
NAME                                                     NODE                                    DRIVER       POOL    AGE
n4c16-fedora-40-cloud-base-containerd-native.cpu-jxfkj   n4c16-fedora-40-cloud-base-containerd   native.cpu   pool0   4d2h
```

And the exposed devices like this:

```bash
[kli@n4c16-fedora-40-cloud-base-containerd ~]# kubectl get resourceslices -oyaml | less
apiVersion: v1
items:
- apiVersion: resource.k8s.io/v1beta2
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2025-06-10T06:01:54Z"
    generateName: n4c16-fedora-40-cloud-base-containerd-native.cpu-
    generation: 1
    name: n4c16-fedora-40-cloud-base-containerd-native.cpu-jxfkj
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: n4c16-fedora-40-cloud-base-containerd
      uid: 90a99f1f-c1ca-4bea-8dbd-3cc821f744b1
    resourceVersion: "871388"
    uid: 4639d31f-e508-4b0a-8378-867f6c1c7cb1
  spec:
    devices:
    - attributes:
        cache0ID:
          int: 0
        cache1ID:
          int: 8
        cache2ID:
          int: 16
        cache3ID:
          int: 24
        cluster:
          int: 0
        core:
          int: 0
        coreType:
          string: P-core
        die:
          int: 0
        isolated:
          bool: false
        localMemory:
          int: 0
        package:
          int: 0
      name: cpu1
    - attributes:
    - attributes:
        cache0ID:
          int: 1
        cache1ID:
          int: 9
        cache2ID:
          int: 17
        cache3ID:
          int: 24
        cluster:
          int: 2
        core:
          int: 1
        coreType:
          string: E-core
        die:
          int: 0
        isolated:
          bool: false
        localMemory:
          int: 0
        package:
          int: 0
      name: cpu2
    - attributes:
        cache0ID:
          int: 1
        cache1ID:
          int: 9
        cache2ID:
          int: 17
        cache3ID:
          int: 24
        cluster:
          int: 2
        core:
...
```

If everything looks fine and you do have CPUs available as DRA devices, you
can test DRA-based CPU allocation with something like this. This allocates
a single P-core for the container.

```yaml
apiVersion: resource.k8s.io/v1beta1
kind: ResourceClaimTemplate
metadata:
  name: any-cores
spec:
  spec:
    devices:
      requests:
      - name: cpu
        deviceClassName: native.cpu
---
apiVersion: resource.k8s.io/v1beta1
kind: ResourceClaimTemplate
metadata:
  name: p-cores
spec:
  spec:
    devices:
      requests:
      - name: cpu
        deviceClassName: native.cpu
        selectors:
          - cel:
              expression: device.attributes["native.cpu"].coreType == "P-core"
        count: 1
---
apiVersion: resource.k8s.io/v1beta1
kind: ResourceClaimTemplate
metadata:
  name: e-cores
spec:
  spec:
    devices:
      requests:
      - name: cpu
        deviceClassName: native.cpu
        selectors:
          - cel:
              expression: device.attributes["native.cpu"].coreType == "E-core"
        count: 1
---
apiVersion: v1
kind: Pod
metadata:
  name: pcore-test
  labels:
    app: pod
spec:
  containers:
  - name: ctr0
    image: busybox
    imagePullPolicy: IfNotPresent
    args:
      - /bin/sh
      - -c
      - trap 'exit 0' TERM; sleep 3600 & wait
    resources:
      requests:
        cpu: 1
        memory: 100M
      limits:
        cpu: 1
        memory: 100M
      claims:
      - name: claim-pcores
  resourceClaims:
  - name: claim-pcores
    resourceClaimTemplateName: p-cores
  terminationGracePeriodSeconds: 1
```

If you want to try a mixed native CPU + DRA-based allocation, try
increasing the CPU request and limit in the pods spec to 1500m CPUs
or CPUs and see what happens.


## Playing Around with CPU Abstractions

If you want to play around with this (for instance modify the exposed CPU abstraction), the easiest way is to
1. [fork](https://github.com/containers/nri-plugins/fork) the [main NRI Reference Plugins](https://github.com/containers/nri-plugins) repo
2. enable github actions in your personal fork
3. make any changes you want (for instance, to alter the CPU abstraction, take a look at [cpu.DRA()](https://github.com/klihub/nri-plugins/blob/test/build/dra-driver/pkg/sysfs/dra.go)
4. Push your changes to ssh://git@github.com/$YOUR_FORK/nri-plugins/refs/heads/test/build/dra-driver.
5. Wait for the image and Helm chart publishing actions to succeed
6. Once done, you can pull the result in to your cluster with something like `helm install --devel -n kube-system test oci://ghcr.io/$YOUR_GITHUB_USERID/nri-plugins/helm-charts/nri-resource-policy-topology-aware --version v0.9-dra-driver-unstable`
