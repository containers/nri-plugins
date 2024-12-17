# Memory QoS NRI plugin

This NRI plugin adds two methods for controlling cgroups v2 `memory.*`
parameters: memory QoS classes and direct memory annotations.

## Workload configuration

There are two configuration methods:

1. Memory QoS classes: memory parameters are calculated in the same
   way for all workloads that belong to the same class.
2. Direct workload-specific memory parameters.

Memory QoS class of a pod or a container is defined using annotations
in pod yaml:

```yaml
  annotations:
    # Set the default memory QoS class for all containers in this pod.
    class.memory-qos.nri.io: silver

    # Override the default class for the c0 container.
    class.memory-qos.nri.io/c0: bronze

    # Remove the default class from the c1 container.
    class.memory-qos.nri.io/c1: ""
```

Cgroups v2 memory parameters are given pod annotations. Following
example affects `memory.swap.max`, `memory.high` and
`memory.oom.group`:

```yaml
  annotations:
    # Never swap memory of the noswap container in this pod.
    memory.swap.max.memory-qos.nri.io/noswap: "0"
    memory.high.memory-qos.nri.io/noswap: max

    # For all containers: if a process gets OOM killed,
    # do not group-kill the whole cgroup.
    memory.oom.group.memory-qos.nri.io: "0"
```

## Plugin configuration

### Classes

Plugin configuration lists memory QoS classes and their parameters
that affect calculating actual memory parameters.

`classes:` is followed by list of maps with following keys and values:

- `name` (string): name of the memory QoS class, matches
  `class.memory-qos.nri.io` annotation values.
- `swaplimitratio` (from 0.0 to 1.0): minimum ratio of container's
  memory on swap and resources.limits.memory when container's memory
  consumption reaches the limit. Adjusts `memory.high` watermark to
  `resources.limits.memory * (1.0 - swaplimitratio)`.

### Unified annotations

`unifiedannotations:` (list of strings): OCI Linux unified fields
(cgroups v2 file names) whose values are allowed to be set using
direct annotations. If annotations define these values, they override
values implied by container's memory QoS class.

### Example

```yaml
classes:
- name: bronze
  swaplimitratio: 0.5
- name: silver
  swaplimitratio: 0.2
unifiedannotations:
- memory.swap.max
- memory.high
```

This configuration defines the following.

- If a container belongs to the memory QoS class `bronze` has allocated
  half of the memory of its `resources.limits.memory`, next
  allocations will cause kernel to swap out corresponding amount of
  container's memory. In other words, when container's memory usage is
  close to the limit, at most half of its data is stored in RAM.
- Containers in `silver` class are allowed to keep up to 80 % of their
  data in RAM when reaching memory limit.
- Memory annotations are allowed to modify `memory.swap.max` and
  `memory.high` values directly but, for instance, modifying
  `memory.oom.group` is not enabled by this configuration.

## Developer's guide

### Prerequisites

- Containerd v1.7+
- Enable NRI in /etc/containerd/config.toml:

  ```toml
  [plugins."io.containerd.nri.v1.nri"]
    disable = false
    disable_connections = false
    plugin_config_path = "/etc/nri/conf.d"
    plugin_path = "/opt/nri/plugins"
    plugin_registration_timeout = "5s"
    plugin_request_timeout = "2s"
    socket_path = "/var/run/nri/nri.sock"
  ```

### Build

```bash
cd cmd/plugins/memory-qos && go build .
```

### Run

```bash
cmd/plugins/memory-qos/memory-qos -config sample-configs/nri-memory-qos.yaml -idx 40 -vv
```

### Manual test

```bash
kubectl create -f test/e2e/files/nri-memory-qos-test-pod.yaml
```

See swap status of dd processes, each allocating the same amount of
memory:

```bash
for pid in $(pidof dd); do
    grep VmSwap /proc/$pid/status
done
```

### Debug

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
dlv exec cmd/plugins/memory-qos/memory-qos -- -config sample-configs/nri-memory-qos.yaml -idx 40
(dlv) break plugin.CreateContainer
(dlv) continue
```

### Deploy

Build an image, import it on the node, and deploy the plugin by
running the following in `nri-plugins`:

```bash
rm -rf build
make clean
make PLUGINS=nri-memory-qos IMAGE_VERSION=devel images
ctr -n k8s.io images import build/images/nri-memory-qos-image-*.tar
kubectl create -f build/images/nri-memory-qos-deployment.yaml
```
