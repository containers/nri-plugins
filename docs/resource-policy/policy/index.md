# Policies

Currently there are two real resource policies implemented:

The Topology Aware resource policy provides a nearly zero configuration
resource policy that allocates resources evenly in order to avoid the "noisy
neighbor" problem.

The Balloons resource policy allows user to allocate workloads to resources in
a more user controlled way.

Additionally there is a wire-frame Template resource policy implementation
without any real resource assignment logic. It can be used as a template to
implement new policies from scratch.

Also, there is some common functionality offered by the shared generic resource
management code used in these policies. This functionality is available in all
policies.


```{toctree}
---
maxdepth: 1
---
topology-aware.md
balloons.md
template.md
common-functionality.md
```
