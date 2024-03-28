# Helm

## Stable Helm Charts

All the available charts can be found in [artifacthub.io](https://artifacthub.io/packages/search?ts_query_web=nri&verified_publisher=true&official=true&sort=relevance&page=1).

**NOTE:** NRI-plugins Helm installation has been successfully verified in both local clusters and major Cloud Providers' managed clusters, including:

   -  AWS EKS
        - kubernetes version: 1.28.x
        - containerd version: 1.7
        - node image: Amazon Linux 2, Ubuntu 20.04, 
   - Google GKE
        - kubernetes version: 1.28.x
        - containerd version: 1.7
        - node image: Container-Optimized OS from Google (COS), Ubuntu 22.04 
   -  Azure AKS
        - kubernetes version: 1.28.x
        - containerd version: 1.7
        - node image: Azure Linux Container Host, Ubuntu 20.04

While Ubuntu 20.04/22.04 was used across all three CSP environments, it's worth noting that node images are not limited to Ubuntu 20.04/22.04 only. The majority of widely recognized Linux distributions should be suitable for use.

## Unstable Helm Charts

Helm charts are also published from the main/development branch after each merge.
These charts reference the latest development images tagged as `unstable` and are
are stored alongside plugin images in the OCI image registry.

### Discovering Unstable Helm Charts

Unstable charts can be discovered using [skopeo](https://github.com/containers/skopeo).
For instance, one can list the available charts for the balloons plugin using this
skopeo command:
`skopeo list-tags docker://ghcr.io/containers/nri-plugins/nri-resource-policy-balloons`

### Using Unstable Helm Charts

Once discovered, unstable Helm charts can be used like any other. For instance, to use
the `$X.$Y-unstable` version of the chart to install the development version of the
balloons plugin one can use this command:
`helm install --devel -n kube-system test oci://ghcr.io/containers/nri-plugins/nri-resource-policy-balloons --version $X.$Y-unstable --set image.tag=unstable --set image.pullPolicy=Always`

```{toctree}
---
maxdepth: 2
caption: Contents
---
balloons.md
topology-aware.md
template.md
memory-qos.md
memtierd.md
sgx-epc.md
```
