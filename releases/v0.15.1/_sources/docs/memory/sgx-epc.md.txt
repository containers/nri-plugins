# SGX EPC Limit Plugin

The sgx-epc NRI plugin allows control over SGX encrypted page cache usage
using the cgroup v2 misc controller and pod annotations.

## Annotations

You can annotate encrypted page cache limit for every container in the pod,
or just a specific container using the following annotation notations:

```yaml
...
metadata:
  annotations:
    # for all containers in the pod
    epc-limit.nri.io/pod: "32768"
    # alternative notation for all containers in the pod
    epc-limit.nri.io: "8192"
    # for container c0 in the pod
    epc-limit.nri.io/container.c0: "16384"
...
```
