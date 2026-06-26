# Memtierd NRI plugin

This plugins enables managing workloads with
[Memtierd](https://github.com/intel/memtierd) in Kubernetes.

Plugin's configuration defines a set of workload classes and their
attributes. If a class is attributed with memtierd configuration,
then this plugin will launch memtierd with that configuration to track
and manage memory of each workload that belongs to the class.

The class of a workload is specified in pod annotations.

## Workload configuration

The class of a pod or a container is defined using pod annotations:

```yaml
  annotations:
    # Set the default class for all containers in this pod.
    class.memtierd.nri.io: swap-idle-data
    # Override the default class for the c0 container.
    class.memtierd.nri.io/c0: track-working-set-size
    # Do not associate any class on the c1 container.
    class.memtierd.nri.io/c1: ""
```

## Plugin configuration

### Classes

Plugin configuration lists workload classes and their attributes.

`classes:` is followed by list of maps with following keys and values:

- `name` (string): name of the class, matches
  `class.memtierd.nri.io` annotations.
- `allowswap` (`true` or `false`): if `true`, allow OS to swap the
  workload. If `false` disallow swapping. If not set, the plugin will
  not affect what will be written to `memory.swap.max` in cgroups v2.
- `memtierdconfig` (string): configuration template with which
  memtierd will be launched to manage workloads in this
  class. Variables that will be replaced with container-specific
  values in this template:
  - `$CGROUP2_ABS_PATH` absolute path to cgroups v2 directory into
    which container's processes will belong to.

### Example

```yaml
classes:
  - name: swap-idle-data
    allowswap: true
    memtierdconfig: |
      policy:
        name: age
        config: |
          intervalms: 10000
          pidwatcher:
            name: cgroups
            config: |
              cgroups:
                - $CGROUP2_ABS_PATH
          swapoutms: 10000
          tracker:
            name: idlepage
            config: |
              pagesinregion: 512
              maxcountperregion: 1
              scanintervalms: 10000
          mover:
            intervalms: 20
            bandwidth: 50
```

The configuration defines the `swap-idle-data` workload class.

`allowswap: true` makes sure that OS will allow swapping when memtierd
decides that data should be swapped out from memory.

`memtierdconfig: ...` means that a memtierd will manage the memory of
a workload in this class. The `age` policy uses the `idlepage` tracker
to find data that has not been accessed in 10 seconds, and swaps out
that data `swapoutms: 10000`. The swapping will be done in 20 ms
interval (`mover.intervalms`), and no more than 50 MB/s
(`mover.bandwidth`). Refer to [memtierd
documentation](https://github.com/intel/memtierd/tree/main/cmd/memtierd)
for more configuration options.

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

- To run the nri-memtierd plugin on a host, install memtierd on the host.

  ```bash
  GOBIN=/usr/local/bin go install github.com/intel/memtierd/cmd/memtierd@latest
  ```

### Build

```bash
cd cmd/plugins/memtierd && go build .
```

### Run

```bash
cmd/plugins/memtierd/memtierd -config sample-configs/nri-memtierd.yaml -idx 40 -vv
```

### Manual test

```bash
kubectl create -f test/e2e/files/nri-memtierd-test-pod.yaml
```

See swap status of dd processes, each allocating the same amount of
memory:

```bash
for pid in $(pidof dd); do
    grep VmSwap /proc/$pid/status
done
```

### Debug

`-v` enables debug output from the plugin. `-vv` makes it even more verbose.

The plugin stores `memtierd` config and output under `/tmp/memtierd/NAMESPACE/POD/CONTAINER/`.

Debugging the plugin with dlv:

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
dlv exec ./memtierd -- -config memtierd.conf -idx 40
(dlv) break plugin.CreateContainer
(dlv) continue
```

### Deploy

Build an image, import it on the node, and deploy the plugin by
running the following in `nri-plugins`:

```bash
rm -rf build
make PLUGINS=nri-memtierd IMAGE_VERSION=devel images
ctr -n k8s.io images import build/images/nri-memtierd-image-*.tar
kubectl create -f build/images/nri-memtierd-deployment-e2e.yaml
```

The e2e deployment variant gives more debug output from both
`nri-memtierd` plugin (see `kubectl logs -n kube-system
nri-memtierd-*`) and `memtierd` to the output (see
`/tmp/memtierd/**/*.output`).

## Security

`memtierd` needs privileged access in order to find pids in other
containers, track memory activity, move pages and swap workload data
out and in. Therefore only privileged users must be allowed to create
and modify memtierd configuration files and ConfigMaps. Commands in
memtierd configurations will be executed by memtierd in privileged
mode.
