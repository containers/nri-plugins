# Memtierd NRI plugin

An easy way to start using [Memtierd](https://github.com/intel/memtierd/tree/main) as a memory manager in your Kubernetes cluster.

## Prerequisities
- NRI enabled on your container runtime

## Making your own image

```console
# Build the image
docker build cmd/memtierd/ -t memtierd-nri

# Then tag and push the image to your registry
docker tag memtierd-nri <your registry>
docker push <your registry>
```

See [running Memtierd NRI plugin in a pod](#running-memtierd-nri-plugin-in-a-pod) on how to deploy it.

## Running a self compiled version locally

To compile your own version run:
```console
go build .
```

Then move the output to the plugin path specified in your /etc/containerd/config.toml file:
```toml
plugin_path = "/opt/nri/plugins"
```

You also need to specify an index for the plugin in the plugin name. The index is like an priority for the plugin to be executed in case you have multiple plugins.

For example:
```console
mv memtier-nri /opt/nri/plugins/10-memtier-nri
```

Then just run:
```console
/opt/nri/plugin/10-memtier-nri
```

After that you should see something like the following:
```console
INFO   [0000] Created plugin 10-memtier-nri (10-memtier-nri, handles RunPodSandbox,StopPodSandbox,RemovePodSandbox,CreateContainer,PostCreateContainer,StartContainer,PostStartContainer,UpdateContainer,PostUpdateContainer,StopContainer,RemoveContainer)
INFO   [0000] Registering plugin 10-memtier-nri...
...
```

Now the plugin is ready to recognize events happening in the cluster.

## <a name="running-memtierd-nri-plugin-in-a-pod"></a> Running Memtierd NRI plugin in a pod

To run Memtierd NRI plugin in a pod change the image in cmd/memtierd/templates/pod-memtierd.yaml to point at your image and then deploy the pod to your cluster:

```console
kubectl apply -f cmd/memtierd/templates/pod-memtierd.yaml
```

## Using Memtierd with your deployments

Workload configurations are defined with the "class.memtierd.nri" annotation. Now for example the following annotation:

```yaml
class.memtierd.nri: "high-prio-configuration"
```

Would start Memtierd for the workload with the configuration found in "cmd/memtierd/templates/high-prio-configuration.yaml"

Workloads need to run in privileged state.
