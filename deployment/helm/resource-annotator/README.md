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

### Manually Generated HTTPS Certificate

Path to the chart: `resource-annotator`

For setting up HTTPS access to the webhook, you can either provide a
certificate and private key yourself, or you can reference a cert-
manager certificate issuer. In the latter case the chart will submit
a certificate request to the issuer and set up annotations for cert-
manager to inject the resulting certificate.

To install the chart with a manually created certificate using the
following commands:

```shell
$ helm repo add nri-plugins https://containers.github.io/nri-plugins
# Create certificate manually.
$ mkdir cert
$ SVC=nri-resource-annotator; NS=kube-system
$ openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
      -keyout ./cert/server-key.pem \
      -out ./cert/server-crt.pem \
      -subj "/CN=$SVC.$NS.svc" \
      -addext "subjectAltName=DNS:$SVC,DNS:$SVC.$NS,DNS:$SVC.$NS.svc"

# Install chart injecting the generated certificate.
$ helm -n $NS install nri-webhook nri-plugins/nri-resource-annotator \
      --set service.secret.crt=$(base64 -w0 < ./cert/server-crt.pem) \
      --set service.secret.key=$(base64 -w0 < ./cert/server-key.pem)
```

This will set up everything for the resource annotator webhook using the
locally generated certificate for HTTPS access.

### Using cert-manager for HTTPS Certificate Injection

Alternatively, you use cert-manager to generate a certificate using
these commands:

```shell
# Install cert-manager, if you don't have it yet.
$ helm install cert-manager oci://quay.io/jetstack/charts/cert-manager \
       --version v1.19.2 --namespace cert-manager --create-namespace \
       --set crds.enabled=true --set crds.keep=false

# Bootstrap a local issuer for cert-manager if you don't have one yet.
$ kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-cluster-issuer
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: internal-root-ca
  namespace: cert-manager
spec:
  isCA: true
  commonName: internal-root-ca
  secretName: internal-root-ca-secret
  issuerRef:
    name: selfsigned-cluster-issuer
    kind: ClusterIssuer
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: console-ca-issuer
  namespace: cert-manager
spec:
  ca:
    secretName: internal-root-ca-secret
EOF
$ kubectl apply -f ca-bootstrap.yaml
$ kubectl wait --for=condition=Ready=True clusterissuer/console-ca-issuer

# Install the chart referring it to the certificate issuer.
$ helm install -n kube-system nri-webhook nri-plugins/nri-resource-annotator \
      --set image.tag=v0.12.0 --set image.pullPolicy=IfNotPresent \
      --set service.certificateIssuer=console-ca-issuer
```

This should set up the resource annotator with a cert-manager issued
certificate.

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
| `extraEnv`                  | {}                                                    | extra environment variables    |
