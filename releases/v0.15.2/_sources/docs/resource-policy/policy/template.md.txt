# Template Policy

The template policy is a wireframe implementation. It does not assign
resources to containers, but it does allocate CPUs to DRA claims, as
described below. It serves as a template and can be used as a starting
point for creating new policies.

## DRA

The template policy also shows how a policy takes part in Dynamic Resource
Allocation (DRA). With DRA enabled (`config.dra.enabled: true` in the Helm
chart), the policy registers the DRA driver `template.nri.io`, and the chart
adds a `DeviceClass` of the same name.

The policy publishes the CPUs it is allowed to use as one device, `cpus`,
with a `dra.cpu/cpu` capacity of their number. Several claims can share the
device, each consuming a number of whole CPUs, which needs the Kubernetes
`DRAConsumableCapacity` feature gate. The allowed CPUs are the online CPUs,
or those in `availableResources.cpu`, which must then be a cpuset, minus
`reservedResources.cpu` if that is a cpuset. Changing either needs a restart.
A claim made before the restart keeps its CPUs until it is released, even
those no longer allowed, and the policy logs a warning about it.

A claim asks for a number of CPUs:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: two-cpus
spec:
  spec:
    devices:
      requests:
      - name: cpus
        exactly:
          deviceClassName: template.nri.io
          capacity:
            requests:
              dra.cpu/cpu: "2"
```

A request without an amount gets one CPU.

The policy picks the CPUs of a claim, lowest IDs first. It tells a container
which ones its claims got in `/.nri-resource-policy/resources.sh`, for
instance `CLAIMED_CPUS="2-3"`. A container which names only some requests of
a claim is told the CPUs of those requests. The policy does not pin the
container to them, nor keep other containers off them: the template policy
does not manage cpusets.
