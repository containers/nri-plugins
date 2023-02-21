# Setup and Usage

If you want to give CRI Resource Manager a try, here is the list of things
you need to do, assuming you already have a Kubernetes\* cluster up and
running, using either `containerd` or `cri-o` as the runtime.

  0. [Install](installation.md) CRI Resource Manager.
  1. Set up kubelet to use CRI Resource Manager as the runtime.
  2. Set up CRI Resource Manager to use the runtime with a policy.

For kubelet you do this by altering its command line options like this:

```
   kubelet <other-kubelet-options> --container-runtime=remote \
     --container-runtime-endpoint=unix:///var/run/cri-resmgr/cri-resmgr.sock
```

For CRI Resource Manager, you need to provide a configuration file, and also
a socket path if you don't use `containerd` or you run it with a different
socket path.

```
   # for containerd with default socket path
   cri-resmgr --force-config <config-file> --runtime-socket unix:///var/run/containerd/containerd.sock
   # for cri-o
   cri-resmgr --force-config <config-file> --runtime-socket unix:///var/run/crio/crio.sock
```

The choice of policy to use along with any potential parameters specific to
that policy are taken from the configuration file. You can take a look at the
[sample configurations](/sample-configs) for some minimal/trivial examples.
For instance, you can use
[sample-configs/topology-aware-policy.cfg](/sample-configs/topology-aware-policy.cfg)
as `<config-file>` to activate the topology aware policy with memory
tiering support.

**NOTE**: Currently, the available policies are a work in progress.

## Setting up kubelet to use CRI Resource Manager as the runtime

To let CRI Resource Manager act as a proxy between kubelet and the CRI
runtime, you need to configure kubelet to connect to CRI Resource Manager
instead of the runtime. You do this by passing extra command line options to
kubelet as shown below:

```
   kubelet <other-kubelet-options> --container-runtime=remote \
     --container-runtime-endpoint=unix:///var/run/cri-resmgr/cri-resmgr.sock
```

## Setting up CRI Resource Manager

Setting up CRI Resource Manager involves pointing it to your runtime and
providing it with a configuration. Pointing to the runtime is done using
the `--runtime-socket <path>` and, optionally, the `--image-socket <path>`.

For providing a configuration there are two options:

  1. use a local configuration YAML file
  2. use the [CRI Resource Manager Node Agent][agent] and a `ConfigMap`

The former is easier to set up and it is also the preferred way to run CRI
Resource Manager for development, and in some cases testing. Setting up the
latter is a bit more involved but it allows you to

  - manage policy configuration for your cluster as a single source, and
  - dynamically update that configuration

### Using a local configuration from a file

This is the easiest way to run CRI Resource Manager for development or
testing. You can do it with the following command:

```
   cri-resmgr --force-config <config-file> --runtime-socket <path>
```

When started this way, CRI Resource Manager reads its configuration from the
given file. It does not fetch external configuration from the node agent and
also disables the config interface for receiving configuration updates.

### Using CRI Resource Manager Agent and a ConfigMap

This setup requires an extra component, the
[CRI Resource Manager Node Agent][agent],
to monitor and fetch configuration from the ConfigMap and pass it on to CRI
Resource Manager. By default, CRI Resource Manager automatically tries to
use the agent to acquire configuration, unless you override this by forcing
a static local configuration using the `--force-config <config-file>` option.
When using the agent, it is also possible to provide an initial fallback for
configuration using the `--fallback-config <config-file>`. This file is
used before the very first configuration is successfully acquired from the
agent.

Whenever a new configuration is acquired from the agent and successfully
taken into use, this configuration is stored in the cache and becomes
the default configuration to take into use the next time CRI Resource
Manager is restarted (unless that time the --force-config option is used).
While CRI Resource Manager is shut down, any cached configuration can be
cleared from the cache using the --reset-config command line option.

See the [Node Agent][agent] about how to set up and configure the agent.


### Changing the active policy

Currently, CRI Resource Manager disables changing the active policy using
the [agent][agent]. That is, once the active policy is recorded in the cache,
any configuration received through the agent that requests a different policy
is rejected. This limitation will be removed in a future version of
CRI Resource Manager.

However, by default CRI Resource Manager allows you to change policies during
its startup phase. If you want to disable this, you can pass the command line
option `--disable-policy-switch` to CRI Resource Manager.

If you run CRI Resource Manager with disabled policy switching, you can still
switch policies by clearing any policy-specific data stored in the cache while
CRI Resource Manager is shut down. You can do this by using the command line
option `--reset-policy`. The whole sequence of switching policies this way is

  - stop cri-resmgr (`systemctl stop cri-resource-manager`)
  - reset policy data (`cri-resmgr --reset-policy`)
  - change policy (`$EDITOR /etc/cri-resource-manager/fallback.cfg`)
  - start cri-resmgr (`systemctl start cri-resource-manager`)


## Kata Containers

[Kata Containers](https://katacontainers.io/) is an open source container
runtime, building lightweight virtual machines that seamlessly plug into the
containers ecosystem.

In order to enable Kata Containers in a Kubernetes-CRI-RM stack, both
Kubernetes and the Container Runtime need to be aware of the new runtime
environment:

  * The Container Runtime can only be CRI-O or containerd, and needs to
   have the runtimes enabled in their configuration files.
  * Kubernetes must be made aware of the CRI-O/containerd runtimes via a
   "RuntimeClass"
   [resource](https://kubernetes.io/docs/concepts/containers/runtime-class/)

After these prerequisites are satisfied, the configuration file for the
target  Kata Container, must have the flag "SandboxCgroupOnly" set to true.
As of Kata 2.0 this is the only way Kata Containers can work with the
Kubernetes cgroup naming conventions.

   ```toml
   ...
   # If enabled, the runtime will add all the kata processes inside one dedicated cgroup.
   # The container cgroups in the host are not created, just one single cgroup per sandbox.
   # The runtime caller is free to restrict or collect cgroup stats of the overall Kata sandbox.
   # The sandbox cgroup path is the parent cgroup of a container with the PodSandbox annotation.
   # The sandbox cgroup is constrained if there is no container type annotation.
   # See: https://godoc.org/github.com/kata-containers/runtime/virtcontainers#ContainerType
   sandbox_cgroup_only=true
   ...
   ```

### Reference

If you have a pre-existing Kubernetes cluster, for an easy deployement
follow this [document](https://github.com/kata-containers/packaging/blob/master/kata-deploy/README.md#kubernetes-quick-start).


Starting from scratch:

   * [Kata installation guide](https://github.com/kata-containers/kata-containers/tree/2.0-dev/docs/install#manual-installation)
   * [Kata Containers + CRI-O](https://github.com/kata-containers/documentation/blob/master/how-to/run-kata-with-k8s.md)
   * [Kata Containers + containerd](https://github.com/kata-containers/documentation/blob/master/how-to/containerd-kata.md)
   * [Kubernetes Runtime Class](https://kubernetes.io/docs/concepts/containers/runtime-class/)
   * [Cgroup and Kata containers](https://github.com/kata-containers/kata-containers/blob/stable-2.0.0/docs/design/host-cgroups.md)


## Running with Untested Runtimes

CRI Resource Manager is tested with `containerd` and `CRI-O`. If any other runtime is
detected during startup, `cri-resmgr` will refuse to start. This default behavior can
be changed using the `--allow-untested-runtimes` command line option.

## Logging and debugging

You can control logging with the klog command line options or by setting the
corresponding environment variables. You can get the name of the environment
variable for a command line option by prepending the `LOGGER_` prefix to the
capitalized option name without any leading dashes. For instance, setting the
environment variable `LOGGER_SKIP_HEADERS=true` has the same effect as using
the `-skip_headers` command line option.

Additionally, the `LOGGER_DEBUG` environment variable controls debug logs.
These are globally disabled by default. You can turn on full debugging by
setting `LOGGER_DEBUG='*'`.

When using environment variables, be careful which configuration you pass to
CRI Resource Manager using a file or ConfigMap. The environment is treated
as default configuration but a file or a ConfigMap has higher precedence.
If something is configured in both, the environment will only be in effect
until the configuration is applied. However, in such a case if you later
push an updated configuration to CRI Resource Manager with the overlapping
settings removed, the original ones from the environment will be in effect
again.

For debug logs, the settings from the configuration are applied in addition
to any settings in the environment. That said, if you turn something on in
the environment but off in the configuration, it will be turned off
eventually.

<!-- Links -->
[agent]: node-agent.md
