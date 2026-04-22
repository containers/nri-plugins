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

You can use the `debug` setting in the `log` section of the configuration custom
resource to control whether and which internal components produce debug log messages.

The `LOGGER_DEBUG` environment variable can be used to seed this for early logging
before the first configuration is acquired. By default debug logging is globally
turned off. You can turn on full debugging by setting `log.debug` to `[ "*" ]`. You
can turn on early full debugging by setting `LOGGER_DEBUG='*'` in the environment.

Additionally, you can toggle temporarily forced full debugging on using the `SIGUSR1`,
and toggle it again off using the same signal.

When using environment variables for early debug logging, once configuration from a
custom resource or a configuration file is taken into use, it suppresses any setting
from the environment.

### Exporting Logs to OpenTelemetry

Logs can be exported to an OpenTelemetry-compatible log collector. The following
custom resource fragment enables gRPC based log exporting.

```yaml
apiVersion: config.nri/v1alpha1
kind: TopologyAwarePolicy
metadata:
  name: default
spec:
...
  instrumentation:
    httpEndpoint: 8891
    logExportPeriod: 15s
    logExporter: otlp-grpc
  log:
    debug:
      - policy
...
```

Additionally you need to pass the collector endpoint to the resource policy plugin
using the stock Opentelemetry environment variables. With the above configuration,
you need to pass `OTEL\_EXPORTER\_OTLP\_LOGS\_ENDPOINT=http://otel-collector:4317`
if your collector is `otel-collector` and configured to use the standard port.

You can set both the configuration and the necessary extra environment variable
with a Helm config fragment like this:

```yaml
config:
  reservedResources:
    cpu: 750m
  pinCPU: true
  pinMemory: true
  instrumentation:
    httpEndpoint: :8891
    logExporter: otlp-grpc
    logExportPeriod: 15s
  log:
    debug:
      - policy
extraEnv:
  OTEL\_EXPORTER\_OTLP\_LOGS\_ENDPOINT: http://otel-collector:4317
```

For testing, you can set up a corresponding collector with a file backend for log
collection using, for instance, the following deployment, service, and ConfigMap.
Additionally you'll need to create `/tmp/otel/data/otel-export.out` with the right
permissions for your containerized collector to be able to access it.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: otel-config
  namespace: kube-system
data:
  config.yaml: |
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: 0.0.0.0:4317
          http:
            endpoint: 0.0.0.0:4318
    exporters:
      file:
        path: /data/otel-export.out
      debug:
        verbosity: normal
    processors:
      batch:
    service:
      telemetry:
        metrics:
      pipelines:
        traces:
          receivers: [otlp]
          exporters: [file]
          processors: [batch]
        metrics:
          receivers: [otlp]
          exporters: [file]
          processors: [batch]
        logs:
          receivers: [otlp]
          exporters: [file, debug]
          processors: [batch]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: otel-collector
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app: otel-collector
  template:
    metadata:
      labels:
        app: otel-collector
    spec:
      containers:
      - name: collector
        image: ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector
        resources:
          requests:
            cpu: 750m
            memory: 250M
          limits:
            cpu: 750m
            memory: 750M
        ports:
        - containerPort: 4317
        - containerPort: 4318
        volumeMounts:
        - name: otel-config
          mountPath: /etc/otelcol
        - name: otel-data
          mountPath: /data
          readOnly: false
        imagePullPolicy: IfNotPresent
      terminationGracePeriodSeconds: 1
      volumes:
      - name: otel-config
        configMap:
          name: otel-config
      - name: otel-data
        hostPath:
          path: /tmp/otel/data
          type: Directory
---
apiVersion: v1
kind: Service
metadata:
  name: otel-collector
  namespace: kube-system
  labels:
    app: otel-collector
spec:
  selector:
    app: otel-collector
  ports:
  - name: otel-grpc
    port: 4317
    targetPort: 4317
    protocol: TCP
  - name: otel-http
    port: 4318
    targetPort: 4318
    protocol: TCP
```

<!-- Links -->
[configuration]: configuration.md
