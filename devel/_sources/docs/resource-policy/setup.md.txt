# Setup and Usage

When you want to try NRI Resource Policy, here is the list of things
you need to do, assuming you already have a Kubernetes\* cluster up and
running, using either `containerd` or `cri-o` as the runtime.

  * [Install](installation.md) NRI Resource Policy DaemonSet deployment file.
  * Runtime (containerd / cri-o) configuration

For NRI Resource Policy, you need to provide a configuration file. The default
configuration ConfigMap file can be found in the DaemonSet deployment yaml file.
You can edit it as needed.

**NOTE**: Currently, the available policies are a work in progress.

## Setting up NRI Resource Policy

### Using NRI Resource Policy Agent and a ConfigMap

The [NRI Resource Policy Node Agent][agent] can monitor and fetch configuration
from the ConfigMap and pass it on to NRI Resource Policy plugin.
By default, it automatically tries to use the agent to acquire configuration,
unless you override this by forcing a static local configuration using
the `--force-config <config-file>` option.
When using the agent, it is also possible to provide an initial fallback for
configuration using the `--fallback-config <config-file>`. This file is
used before the very first configuration is successfully acquired from the
agent.

See the [Node Agent][agent] about how to set up and configure the agent.


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
NRI Resource Policy using a file or ConfigMap. The environment is treated
as default configuration but a file or a ConfigMap has higher precedence.
If something is configured in both, the environment will only be in effect
until the configuration is applied. However, in such a case if you later
push an updated configuration to NRI Resource Policy with the overlapping
settings removed, the original ones from the environment will be in effect
again.

For debug logs, the settings from the configuration are applied in addition
to any settings in the environment. That said, if you turn something on in
the environment but off in the configuration, it will be turned off
eventually.

<!-- Links -->
[agent]: node-agent.md
