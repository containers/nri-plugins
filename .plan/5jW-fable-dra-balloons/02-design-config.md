# Step 1: configuration option design

## Goal

Let a cluster admin mark balloon types whose instances are published as
DRA devices by this node's driver `balloons.nri.io`, and let the admin
choose (or template) the device names.

## Option

A new per-balloon-type block `dra` in `balloonTypes[]`:

```yaml
balloonTypes:
- name: fast
  minBalloons: 2        # required >= 1: instances must exist to be published
  maxCPUs: 4            # required > 0: the cpu capacity of every instance
  minCPUs: 4            # optional; 4 == maxCPUs gives fixed-size balloons
  dra:
    deviceName: "fast-${instance}"   # optional; default "${balloonType}-${instance}"
```

- The presence of `dra` (even `dra: {}`) turns publishing on for the type.
  Absence (or `dra: null`) leaves the type as it is today.
- `deviceName` is a template. `${balloonType}` expands to the balloon type
  name, `${instance}` to the instance index (0, 1, ...). The result must
  be a DNS label (lowercase alphanumerics and '-', 1-63 chars, no leading
  or trailing '-') and unique over all instances of all DRA balloon types.
  Invalid or duplicate names are a configuration error.
- Why a nested block instead of flat `publishDRA`/`draDeviceName` fields:
  it mirrors the policy-level `dra:` block of the common configuration,
  keeps the DRA knobs together and leaves room for later additions
  (attributes, exclusive mode) without adding more top-level fields to an
  already long BalloonDef.

Go types (pkg/apis/config/v1alpha1/resmgr/policy/balloons/config.go):

```go
// DRA publishes balloon instances of this type as DRA devices of the
// balloons.nri.io driver. Containers get into these balloons only by
// requesting the devices through resource claims.
// +optional
DRA *BalloonDRA `json:"dra,omitempty"`

// BalloonDRA contains the DRA parameters of a balloon type.
// +kubebuilder:object:generate=true
type BalloonDRA struct {
	// DeviceName is a template for the names of the DRA devices that
	// publish the balloon instances. ${balloonType} expands to the
	// balloon type name and ${instance} to the instance index. The
	// expanded name must be a DNS label and unique among all DRA
	// devices of the policy. The default is "${balloonType}-${instance}".
	// +optional
	DeviceName string `json:"deviceName,omitempty"`
}
```

Run `make generate` after adding the types (deepcopy, CRDs, Helm CRDs).

## Rules a DRA balloon type must satisfy (validateConfig)

1. Not the built-in `reserved` or `default` type.
2. `maxCPUs > 0`. It is the published `cpu` capacity of each instance.
3. `minBalloons >= 1`. Only the pre-created instances are published; the
   policy never creates DRA balloons on demand (there is no container
   placement path that could create one, see 03). `maxBalloons`, when set,
   should equal `minBalloons`; a larger value is accepted but has no effect.
4. `namespaces` and `matchExpressions` must be empty: regular containers
   never land in a DRA balloon, so these would be dead configuration.
5. Not referenced as a component of a composite balloon type.
6. The device name template must expand to valid, unique DNS labels for
   instances 0..minBalloons-1.
7. On reconfiguration: every recorded claim must still find its balloon
   (type with `dra` and instance < minBalloons). Otherwise the new
   configuration is rejected with an error naming the claim and balloon.

Options that have no effect on DRA types and are silently ignored:
`groupBy`, `preferNewBalloons`, `preferSpreadingPods`,
`preferPerNamespaceBalloon`. Everything else (cpuClass, loads, memory
types, allocator options, IRQ options, scheduling class, hide
hyperthreads, shareIdleCPUsInSame, preferCloseToDevices, ...) applies as
usual to the balloon instances and their containers.

## What is published for each instance

One `resourceapi.Device` per existing instance of a DRA balloon type:

| Field | Value |
|-------|-------|
| name | expanded `deviceName` template |
| allowMultipleAllocations | true |
| attributes.balloonType | string, the balloon type name |
| attributes.instance | int, the instance index |
| capacity.cpu | `maxCPUs` as a whole number; no RequestPolicy |
| nodeAllocatableResources.cpu.mapping | `capacityKey: cpu`, `capacityMultiplier: 1` |

Attributes are static on purpose: the balloon's current cpuset changes on
every inflate/deflate, and publishing it would cause a ResourceSlice write
per change. Attribute and capacity names without a domain are in the
driver's domain, so CEL selectors read
`device.attributes["balloons.nri.io"].balloonType == "fast"` and
`device.capacity["balloons.nri.io"].cpu`.

Published when the policy starts and after every successful
reconfiguration (the set of DRA instances changes only with
configuration).

## Helm chart and DeviceClass

The balloons chart gets `templates/deviceclass.yaml` like the template
chart: DeviceClass `balloons.nri.io` selecting
`device.driver == "balloons.nri.io"`, rendered when
`.Values.config.dra.enabled`. Admins can add narrower DeviceClasses (per
balloon type) with CEL on `balloonType`.

## Claim examples

Whole balloon (no amount: consumes the whole `cpu` capacity, so the
instance is exclusive to this claim):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: fast-balloon
spec:
  spec:
    devices:
      requests:
      - name: balloon
        exactly:
          deviceClassName: balloons.nri.io
          selectors:
          - cel:
              expression: device.attributes["balloons.nri.io"].balloonType == "fast"
```

Two CPUs of a shared balloon instance:

```yaml
      requests:
      - name: balloon
        exactly:
          deviceClassName: balloons.nri.io
          selectors:
          - cel:
              expression: device.attributes["balloons.nri.io"].balloonType == "fast"
          capacity:
            requests:
              cpu: "2"
```

Pod:

```yaml
spec:
  resourceClaims:
  - name: balloon
    resourceClaimTemplateName: fast-balloon
  containers:
  - name: c0
    resources:
      claims:
      - name: balloon
```

The container is pinned to the CPUs of the claimed balloon and gets the
environment variable `DRA_BALLOON=<device name>`.
