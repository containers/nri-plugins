# Helm

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
