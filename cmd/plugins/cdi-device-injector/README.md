# CDI Device Injector using NRI

The purpose of this NRI plugin is to provide a controlled mechansim for injecting
CDI devices into containers. This can be separated into two aspects:

1. Requesting devices
2. Controlling access to devices


## Requesting devices

For reqesting devices, we use pod annotations to indicate which devices should
be made available to a particular container or containers in a pod. Here a
pre-defined annotation prefix `cdi.nri.io/` is used for these annotations.
Devices are requested for a specific container by including
`container.{{ .ContainerName }}` as a suffix in the annotation key. For a
request targeting ALL containers in a pod `cdi.nri.io/pod` is used as the pod
annotaion key.

In either case, the corresponding annotation value represents a comma-separated
list of fully-qualified CDI device names.

As examples, consider the following pod annotation:
```yaml
apiVersion: v1
kind: Pod
metadata:
  namespace: management
  name: nri-injection-example
  annotations:
    cdi.nri.io/container.first-ctr: "example.com/class=device0,example.com/class=device1"
spec:
  containers:
  - name: first-ctr
    image: ubuntu
  - name: second-ctr
    image: ubuntu
```

This will trigger the injection of the `example.com/class=device0` and
`example.com/class=device1` devices into the `first-ctr` container, but not into
the `second-ctr` container.

When the annotations are updated as follows:
```yaml
apiVersion: v1
kind: Pod
metadata:
  namespace: management
  name: nri-injection-example
  annotations:
    cdi.nri.io/pod: "example.com/class=device0,example.com/class=device1"
spec:
  containers:
  - name: first-ctr
    image: ubuntu
  - name: second-ctr
    image: ubuntu
```

the same `example.com/class=device0` and `example.com/class=device1` devices
will be injected into all (`first-ctr` and `second-ctr`) containers in the pod.

## Controlling Access

In order to control access to specific CDI devices, we make use of namespace
annotations. Here, the same `cdi.nri.io/` prefix is used to identify an
annotation for controlling the injection of CDI devices using NRI. The
pre-defined annotation key `cdi.nri.io/allow` is used to explicitly allow access
to CDI devices.

The value field is interpreted as a filename glob to allow for wildcard matches.

For example:
* `*` will allow any CDI device to be injected
* `example.com/*` will allow any CDI device with the explicit `example.com` to be
  injected.
* `example.com/class=*` will allow any CDI devices from vendor `example.com` and
  class `class` to be injected.
* `example.com/class=device0` will only allow the specified CDI device to be
  injected.

Consider the following example namespace (which was also referenced in the
pod examples above):

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: management
  annotations:
    cdi.nri.io/allow: "*"
```

This allows the injection of any CDI devices into containers belonging to pods
in this namespace.
