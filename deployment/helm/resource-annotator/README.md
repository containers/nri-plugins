# Resource Annotator Mutating Webhook

This chart deploys the resource annotator mutating admission webhook.
This webhook can be used to provide extra information for NRI resource
policy plugins about compute (CPU and memory) resource requirements of
containers. The hook will put a well known annotation on the pod which
describes the resources for all init container and containers by name.
If found, NRI resource policy plugins will use this extra information
to discover container resource requirements instead of estimating them.

## Prerequisites

- An NRI resource plugin > v0.11.0
- Helm 3.0.0+

## Installing the Chart

Path to the chart: `resource-annotator`

At the moment the webhook does not you cert-manager. Instead you need
to generate a certificate for the webhook before instantiating it and
pass the certificate and its related key to helm. The below example
demonstrates how this can be done.

```shell
$ helm repo add nri-plugins https://containers.github.io/nri-plugins
$ mkdir cert
$ SVC=nri-resource-annotator; NS=kube-system
$ openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
      -keyout ./cert/server-key.pem \
      -out ./cert/server-crt.pem \
      -subj "/CN=$SVC.$NS.svc" \
      -addext "subjectAltName=DNS:$SVC,DNS:$SVC.$NS,DNS:$SVC.$NS.svc"
$ helm -n $NS install nri-webhook nri-plugins/nri-resource-annotator \
      --set service.secret.crt=$(base64 -w0 < ./cert/server-crt.pem) \
      --set service.secret.key=$(base64 -w0 < ./cert/server-key.pem)
```

This will set up everything for the resource annotator webhook.

## Uninstalling the Chart

You can uninstall the resource annotator webhook with the following
helm command.

```shell
$ NS=kube-system
$ helm -n $NS uninstall nri-webhook
```

## Configuration options

The tables below present an overview of the parameters available for users to
customize with their own values, along with the default values.

| Name                        | Default                                               | Description                    |
|-----------------------------|-------------------------------------------------------|--------------------------------|
| `image.name`                | ghcr.io/containers/nri-plugins/nri-resource-annotator | container image name           |
| `image.tag`                 | unstable                                              | container image tag            |
| `image.pullPolicy`          | Always                                                | image pull policy              |
| `service.base64Crt`         | no sane default, see instructions above               | base64 encoded certificate     |
| `service.base64Key`         | no sane default, see instructions above               | base64 encoded certificate key |
| `resources.requests.cpu`    | 250m                                                  | CPU resource request           |
| `resources.requests.memory` | 256Mi                                                 | memory resource request        |
| `resources.limits.cpu`      | 1                                                     | CPU resource limit             |
| `resources.limits.memory`   | 256Mi                                                 | memory resource limit          |
