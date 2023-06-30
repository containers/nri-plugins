# Memtierd demo

To replicate the demo see [replicating the demo](https://github.com/containers/nri-plugins/demo/Replicating-the-demo.md) for instructions.

## What was showcased

The demo showcases the differenece between how low priority and high priority workloads are treated when using Memtierd as the memory manager. Low priority workloads and high priority workloads are defined by giving your deployments the following annotations:

```yaml
class.memtierd.nri: "high-prioconfiguration"
# or
class.memtierd.nri: "low-prio-configuration"
```

This annotation defines whether the workload will be swapped agressively (low-prio) or more moderately (high-prio). More agressive swapping will lead to an increase in the number of page faults the process will have.

## About the metrics

RAM Saved (G)
- How much RAM is being saved by swapping out the idle workloads.

RAM Saved (%)
- How big the total memory saved is in comparison to the overall memory of the system.

Compressed (%)
- How well the data is being compressed.

RAM vs Swap
- How the memory is being distributed between RAM and the swap.

Page faults
- How many new page faults happen in between the requests from Grafana. This is a way to express the possible performance hit workloads experience if being tracked by Memtierd.

![alt text](https://github.com/containers/nri-plugins/demo/memtierd-demo.png)
