# NRI Resource Policy for Kubernetes

NRI resource policy is a NRI plugin that will apply hardware-aware
resource allocation policies to the containers running in the system.

## NRI Resource Policy Usage

Compile the available resource policies. Currently there exists
topology-aware and balloons policies. The binaries are created to
build/bin directory.

```
   $ make
```

In order to use the policies in a Kubernetes cluster node, a DaemonSet deployment
file and corresponding container image are created to build/images directory.
You need to have Docker installed in order to build the images.

```
   $ make images
   $ ls build/images
   nri-resource-policy-balloons-deployment.yaml
   nri-resource-policy-balloons-image-ed6fffe77071.tar
   nri-resource-policy-topology-aware-deployment.yaml
   nri-resource-policy-topology-aware-image-9797e8de7107.tar
```

Only one policy can be running in the cluster node at one time. In this example we
run topology-aware policy in the cluster node.

You need to copy the deployment file (yaml) and corresponding image file (tar)
to the node:

```
   $ scp nri-resource-policy-topology-aware-deployment.yaml nri-resource-policy-topology-aware-image-9797e8de7107.tar node:
```

NRI needs to be setup in the cluster node:

```
   # mkdir -p /etc/nri
   # echo "disableConnections: false" > /etc/nri/nri.conf
   # mkdir -p /opt/nri/plugins
```

Note that containerd must have NRI support enabled and NRI is currently only
available in 1.7beta or later containerd release. This is why you must do
some extra steps in order to enable NRI plugin support in containerd.

This will create a fresh config file and backup the old one if it existed:

```
   # [ -f /etc/containerd/config.toml ] && cp /etc/containerd/config.toml.backup
   # containerd config default > /etc/containerd/config.toml
```

Edit the `/etc/containerd/config.toml` file and set `plugins."io.containerd.nri.v1.nri"`
option `disable = true` to `disable = false` and restart containerd.


Before deploying NRI resource policy plugin, you need to declare the CRDs it needs.
Copy first the CRD YAMLs to the node:

```
   $ scp deployment/base/crds/noderesourcetopology_crd.yaml node:
```

Then log in to the node and create the CRDs:

```
   $ ssh node
   (on the node) $ kubectl apply -f noderesourcetopology_crd.yaml
```

You can now deploy NRI resource policy plugin:

```
   $ ctr -n k8s.io images import nri-resource-policy-topology-aware-image-9797e8de7107.tar
   $ kubectl apply -f nri-resource-policy-topology-aware-deployment.yaml
```

Verify that the pod is running:

```
   $ kubectl -n kube-system get pods
   NAMESPACE     NAME                        READY   STATUS    RESTARTS   AGE
   kube-system   nri-resource-policy-nblgl   1/1     Running   0          18m
```

To see the resource policy logs:

```
   $ kubectl -n kube-system logs nri-resource-policy-nblgl
```

In order to see how resource policy allocates resources for the topology-aware policy,
you can create a simple pod to see the changes:

```
   $ cat pod0.yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod0
  labels:
    app: pod0
spec:
  containers:
  - name: pod0c0
    image: busybox
    imagePullPolicy: IfNotPresent
    command:
      - sh
      - -c
      - echo pod0c0 $(sleep inf)
    resources:
      requests:
        cpu: 750m
        memory: '100M'
      limits:
        cpu: 750m
        memory: '100M'
  - name: pod0c1
    image: busybox
    imagePullPolicy: IfNotPresent
    command:
      - sh
      - -c
      - echo pod0c0 $(sleep inf)
    resources:
      requests:
        cpu: 750m
        memory: '100M'
      limits:
        cpu: 750m
        memory: '100M'
  terminationGracePeriodSeconds: 1

   $ kubectl apply -f pod0.yaml
```

Then if you *have already* deployed nri-resource-policy, the resources are allocated in isolation like this:

```
   $ kubectl exec pod0 -c pod0c0 -- grep allowed_list: /proc/self/status
   Cpus_allowed_list:	8
   Mems_allowed_list:	2

   $ kubectl exec pod0 -c pod0c1 -- grep allowed_list: /proc/self/status
   Cpus_allowed_list:	12
   Mems_allowed_list:	3
```

If you *have not* deployed yet nri-resource-policy, the containers are allocated to same CPUs and memory:

```
   $ kubectl exec pod0 -c pod0c0 -- grep allowed_list: /proc/self/status
   Cpus_allowed_list:	0-15
   Mems_allowed_list:	0-3

   $ kubectl exec pod0 -c pod0c1 -- grep allowed_list: /proc/self/status
   Cpus_allowed_list:	0-15
   Mems_allowed_list:	0-3
```

You can also check the difference in resource allocation using an alternative sequence
of steps. Remove the simple test pod, remove the nri-resource-policy deployment, re-create the
simple test pod, check the resources, re-create the nri-resource-policy deployment, then check
the resources and compare to the previous. You should see the resources reassigned so
that the containers in the pod are isolated from each other into different NUMA nodes
if your HW setup makes this possible.
