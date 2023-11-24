# Setup and Usage

When you want to try NRI Resource Policy, here is the list of things
you need to do, assuming you already have a Kubernetes\* cluster up and
running, using either `containerd` or `cri-o` as the runtime.

* [Deploy](../deployment/index.md) NRI Resource Policy Helm Charts.
* Runtime (containerd / cri-o) configuration

Resource Policy plugins are configured using plugin-specific custom
resources. The Helm charts for each policy contain a default configuration.
This configuration can be overridden using extra helm options.

**NOTE**: Currently, the available policies are a work in progress.

## Setting up NRI Resource Policy

### Dynamic Configuration with Custom Resources

The resource policies plugins support[dynamic configuration][configuration]
using custom resources. Plugins watch changes in their configuration and
reconfigure themselves on any update.

Cluster-based dynamic configuration is disabled if a local configuration
file is supplied using the `--config-file <config-file>` command line option.

## Logging and debugging

You can control logging with the klog options in the configuration or by
setting corresponding environment variables. You can get the name of the
environment variable for a klog option by prepending the `LOGGER_` prefix
to the capitalized option name without any leading dashes. For instance,
setting the environment variable `LOGGER_SKIP_HEADERS=true` has the same
effect as setting the log.klog.Skip_headers` config option

Additionally, the `LOGGER_DEBUG` environment variable controls debug logs.
These are globally disabled by default. You can turn on full debugging by
setting `LOGGER_DEBUG='*'`.

When using environment variables, once configuration from a custom resource
or a configuration file is taken into use, it suppresses the settings from
the environment.

<!-- Links -->
[configuration]: configuration.md
