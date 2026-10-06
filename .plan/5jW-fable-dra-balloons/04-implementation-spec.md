# Step 3: implementation specification

Follow `.github/skills/coding-style/SKILL.md` (ASCII only, function docs say
what not how, no history in comments). Keep changes minimal and local.
Run `make generate` after the config change, then `make reformat`,
`make golangci-lint`, `go test ./pkg/resmgr/... ./cmd/plugins/balloons/...`.

Note: `go build` without flags needs a consistent `vendor/`; it was
refreshed with `go mod vendor`. If it is stale again, rerun that.

## 1. Config API: pkg/apis/config/v1alpha1/resmgr/policy/balloons/config.go

Add to `BalloonDef` (place after `IrqMode`):

```go
	// DRA publishes balloon instances of this type as DRA devices of
	// the balloons.nri.io driver. Containers get into these balloons
	// only by requesting the devices through resource claims. The
	// type needs minBalloons of at least 1 and maxCPUs above 0, and
	// it cannot have namespaces, matchExpressions or be a component.
	// +optional
	DRA *BalloonDRA `json:"dra,omitempty"`
```

Add type `BalloonDRA` with `DeviceName string json:"deviceName,omitempty"`
as in 02-design-config.md, with `+kubebuilder:object:generate=true`.
Run `make generate` (deepcopy, config/crd/bases, deployment/helm/*/crds).

## 2. Shared DRA helpers: pkg/resmgr/dra

Add to package `dra` (new file `names.go` or in `cdi.go`):

```go
// DriverName returns the name of the DRA driver of the named policy.
func DriverName(policyName string) string  // policyName + ".nri.io"

// ClaimOfCDIDevice returns the driver and the claim UID of a CDI device
// written for a prepared claim. ok is false for any other device.
func ClaimOfCDIDevice(qualifiedName string) (driver string, uid types.UID, ok bool)
```

Implementation: `parser.ParseQualifiedName(qualifiedName)` -> vendor,
class, name; require `class == cdiClass`, `name` has prefix "claim-" and a
suffix "-<decimal index>"; uid is what is between. Must round-trip with
`cdiDeviceName(uid, index)`; add a small unit test for both directions and
for non-matching names. Move the `draDomain` constant from
`pkg/resmgr/dra.go` into the dra package and make `setupDRA` use
`dra.DriverName(m.policy.ActivePolicy())`.

## 3. Cache: expose CDI devices of a container

`pkg/resmgr/cache/cache.go` Container interface, next to `GetDevices`:

```go
	// GetCDIDevices returns the qualified names of the CDI devices of the container.
	GetCDIDevices() []string
```

`pkg/resmgr/cache/container.go`: iterate `c.Ctr.GetCDIDevices()` and
collect `GetName()`. Check whether any mock/fake implementing
cache.Container exists in tests (e.g. cmd/plugins/topology-aware/policy/mocks_test.go)
and add the method there so tests compile.

## 4. Balloons policy: cmd/plugins/balloons/policy

Put the DRA code in a new file `dra.go` (plus `dra_test.go`), and make
small edits in `balloons-policy.go`.

### 4.1 Types and state

```go
const (
	// draCapacityCPU is the consumable capacity of a balloon device: whole CPUs.
	draCapacityCPU resourceapi.QualifiedName = "cpu"
	// draAttrBalloonType and draAttrInstance are the device attributes.
	draAttrBalloonType resourceapi.QualifiedName = "balloonType"
	draAttrInstance    resourceapi.QualifiedName = "instance"
	// draEnvBalloon is the variable telling a container the device of its balloon.
	draEnvBalloon = "DRA_BALLOON"
	// draDefaultDeviceName is the device name template used when the configuration gives none.
	draDefaultDeviceName = "${balloonType}-${instance}"
	// keyDRAClaims is the cache policy entry holding the claim records.
	keyDRAClaims = "dra-claims"
)

// draClaim records what a claim was given, one entry per allocation result.
type draClaim struct {
	Devices []draClaimDevice `json:"devices"`
}

// draClaimDevice records one allocation result of a claim.
type draClaimDevice struct {
	Device      string `json:"device"`      // DRA device name
	BalloonType string `json:"balloonType"` // balloon definition name
	Instance    int    `json:"instance"`    // balloon instance index
	MilliCPUs   int    `json:"milliCPUs"`   // CPUs consumed from the balloon
}
```

`balloons` struct gets `draClaims map[string]*draClaim` (key: claim UID
as string) and `draDriver string` (= `dra.DriverName(PolicyName)`).

### 4.2 Device names

```go
// draDeviceName returns the DRA device name of a balloon instance.
func draDeviceName(blnDef *BalloonDef, instance int) string
```
Expand `blnDef.DRA.DeviceName` (default `draDefaultDeviceName`) with
`strings.NewReplacer("${balloonType}", blnDef.Name, "${instance}", strconv.Itoa(instance))`.
Validation uses `k8s.io/apimachinery/pkg/util/validation.IsDNS1123Label`.

`draBalloonByDevice(name string) *Balloon` finds the published balloon
with that device name (iterate `p.balloons` where `bln.Def.DRA != nil`).
`draBalloonByInstance(defName string, instance int) *Balloon` similar.

### 4.3 Publishing

```go
// publishDRADevices publishes every DRA balloon as a device.
func (p *balloons) publishDRADevices() error
// draDevices returns the devices of the DRA balloons.
func (p *balloons) draDevices() []resourceapi.Device
```
Device content per 02-design-config.md. Use `ptr.To` from k8s.io/utils/ptr.
`NodeAllocatableResources: map[corev1.ResourceName]resourceapi.NodeAllocatableResource{corev1.ResourceCPU: {Mapping: &resourceapi.NodeAllocatableMapping{CapacityKey: ptr.To(draCapacityCPU), CapacityMultiplier: ptr.To(resource.MustParse("1"))}}}`.
Capacity value: `*resource.NewQuantity(int64(blnDef.MaxCpus), resource.DecimalSI)`.
Sort devices by name for stable output. Call from `Start()` (return its
error) and at the end of a successful `Reconfigure()` (log a warning on
error; the configuration is already applied). `p.options.Owner` may be
nil in unit tests: guard with `if p.options.Owner == nil { return nil }`.

### 4.4 Claim records persistence

- `Setup()`: before `policyOptions.Cache.ResetPolicyEntries()`, read the
  entry into `p.draClaims` (`GetPolicyEntry(keyDRAClaims, &p.draClaims)`;
  initialize an empty map when absent). After the reset, store it again
  (`SetPolicyEntry`). Do this before `setConfig` so that validation and
  claim re-application see the claims.
- `saveDRAClaims()` = `p.cch.SetPolicyEntry(keyDRAClaims, p.draClaims)`.
  Call after every change. The resource manager saves the cache in
  `ClaimAllocated/ClaimReleased`.

### 4.5 requestedMilliCpus includes claims

Change `requestedMilliCpus(bln)` to add
`p.draClaimedMilliCpus(bln)`: the sum of `MilliCPUs` over all claim
devices whose (BalloonType, Instance) match `bln.Def.Name, bln.Instance`.
Keep it cheap (claims are few). Nothing else in the sizing paths needs
to change; `AllocateResources` and `ReleaseResources` already resize to
`max(1, requestedMilliCpus)`. In `ReleaseResources`, when
`bln.ContainerCount() == 0`, the current code deflates with
`resizeBalloon(bln, 0)` then `freeBalloon`: change the 0 to
`p.requestedMilliCpus(bln)` so claims keep their CPUs (for non-DRA
balloons that value is 0 as before).

### 4.6 AllocateClaim / ReleaseClaim

```go
func (p *balloons) AllocateClaim(claim *resourceapi.ResourceClaim, results []resourceapi.DeviceRequestAllocationResult) ([]specs.ContainerEdits, error)
```
Wrap with the same irq/meters/cpuclass/irq-affinity block/defer sequence
as `AllocateResources` (resizing may reconfigure CPUs).

1. `uid := string(claim.UID)`. If `p.draClaims[uid]` exists: if
   `len(rec.Devices) != len(results)` return an error, else return
   `draClaimEdits(rec)` (idempotent re-prepare; do not compare device
   names, a rename in configuration must not fail running claims).
2. Otherwise build the record: for each result, `bln :=
   p.draBalloonByDevice(r.Device)`; nil -> error
   `claim %s: device %q is not a balloon published by this node`. CPUs:
   `q, ok := r.ConsumedCapacity[draCapacityCPU]`; if !ok, the device was
   not shareable (DRAConsumableCapacity off): use the whole capacity
   `bln.Def.MaxCpus * 1000`; else `int(q.MilliValue())`, must be > 0.
3. Apply: append the devices to a new record, insert it into
   `p.draClaims`, then for each distinct balloon touched call
   `p.resizeBalloon(bln, max(1, p.requestedMilliCpus(bln)))` only when
   `bln.AvailMilliCpus() < requested`. On any resize error, remove the
   record, resize the already-grown balloons back
   (`resizeBalloon(bln, max(minCPUs*1000, requestedMilliCpus))`, ignore
   errors with a warning), and return the error wrapped as
   `claim %s: not enough CPUs for %d mCPU in balloon %s: %w`.
4. `saveDRAClaims()`, log at Info level which balloon(s) the claim got,
   return `draClaimEdits(rec)`.

```go
// draClaimEdits returns one container edit per recorded device.
func draClaimEdits(rec *draClaim) []specs.ContainerEdits
```
Each edit: `Env: []string{draEnvBalloon + "=" + dev.Device}`.

```go
func (p *balloons) ReleaseClaim(uid types.UID) error
```
Same block/defer wrapping. If no record: return nil. Delete the record,
save, then for each balloon the record touched (that still exists):
`p.resizeBalloon(bln, max(minCpusForBalloon, p.requestedMilliCpus(bln)))`
where an empty balloon with no claims deflates to `minCPUs` (resizeBalloon
already clamps to MinCpus, so passing `requestedMilliCpus` is enough; use
`max(1, requested)` only if the balloon still has containers, mirroring
ReleaseResources). Log warnings on resize errors, return nil.

Reapply after (re)configuration:

```go
// applyDRAClaims resizes DRA balloons to hold the CPUs of the recorded claims.
func (p *balloons) applyDRAClaims() error
```
Called in `setConfig` right after the loop creating balloons
(`applyBalloonDef`) and before `p.ifreeCpus = p.freeCpus.Clone()`? No:
after `ifreeCpus` is set is fine either way; put it right after the
balloon creation loop. For each claim device find the balloon by
(type, instance); missing -> return error (validation should have caught
it). Resize as in AllocateClaim. Errors fail setConfig.

### 4.7 Container to claims

```go
// draContainerClaims returns the UIDs of the claims a container uses, from
// its CDI devices of our driver.
func (p *balloons) draContainerClaims(c cache.Container) []string
```
Iterate `c.GetCDIDevices()`, `dra.ClaimOfCDIDevice(name)`, keep those with
`driver == p.draDriver`, de-duplicate, sort.

```go
// draBalloonForContainer returns the balloon a DRA container must be placed
// in, or an error describing the contradiction in its requests.
func (p *balloons) draBalloonForContainer(c cache.Container, claimUIDs []string) (*Balloon, error)
```
- For each UID: record missing -> error
  `container %s uses claim %s that this policy has not prepared`.
- Collect distinct (BalloonType, Instance) over all devices of all
  records. More than one -> error
  `container %s requests DRA devices of more than one balloon: %s`
  (list device names).
- Find the balloon; missing (should not happen after validation) ->
  error.
- Annotation check: `c.GetEffectiveAnnotation(balloonKey)`; if present
  and != balloon type -> error
  `container %s: pod annotation %s requests balloon type %q but its DRA device %q is of type %q`.

### 4.8 AllocateResources

After the preserve checks and before `allocateBalloon`:

```go
	if claimUIDs := p.draContainerClaims(c); len(claimUIDs) > 0 {
		bln, err := p.draBalloonForContainer(c, claimUIDs)
		if err != nil {
			return balloonsError("%w", err) // keep message readable
		}
		... same resize-if-needed and assignContainer as the regular path
	}
```
Refactor the tail of AllocateResources (resize to fit + assign + debug
dump) into a helper used by both paths, e.g.
`func (p *balloons) placeContainer(c cache.Container, bln *Balloon) error`.

CPU shares for DRA containers: in `pinCpuMem` the shares come from the
native request. Add the container's claimed milli-CPUs: compute
`p.draContainerMilliCpus(c)` = sum of MilliCPUs over the container's
claims (0 for regular containers) and use `native + claimed` for
`SetCPUShares` when the sum is > 0. Keep the existing behavior (no shares
set) when both are 0.

### 4.9 chooseBalloonDef

- Case 1 (annotation): if `blnDef.DRA != nil` -> error
  `balloon type %q is published as DRA devices, request it through a resource claim`.
- Cases 2 and 3: `continue` for defs with `DRA != nil` (they have no
  namespaces/matchExpressions after validation anyway; the guard keeps it
  explicit).

### 4.10 validateConfig additions

In the loop over `bpoptions.BalloonDefs`, for defs with `DRA != nil`,
enforce rules 1-6 of 02-design-config.md with clear messages prefixed
`balloon type %q: dra ...`. Collect device names in a map to detect
duplicates. Rule 7 (claims keep their balloon): iterate `p.draClaims`
devices; the def named `BalloonType` must exist with `DRA != nil` and
`Instance < MinBalloons`, else error
`cannot apply configuration: claim %s holds balloon %s[%d] which the new configuration does not provide`.
Note `validateConfig` is called from `setConfig` which is called from
`Setup` too; `p.draClaims` must already be loaded (4.4).

Rule 5 check: for every def with Components, every component's def must
not have DRA.

### 4.11 Reconfigure

At the end of `Reconfigure`, after `setConfig` succeeded and the Sync is
done, call `publishDRADevices()`; log a warning on error. In the
"configuration changes only on CPU classes" branch nothing changes for
DRA.

### 4.12 Logging

Info level: devices published (names), claim allocated (uid, devices,
milli-CPUs, balloon), claim released, DRA container placed. Debug for the
rest.

## 5. Helm chart

Add `deployment/helm/balloons/templates/deviceclass.yaml`, a copy of the
template chart's file with `balloons.nri.io`.

## 6. Documentation: docs/resource-policy/policy/balloons.md

Add a section `### Publishing Balloons as DRA Devices` under
"Configuration Options" (after "Built-in Balloon Types" or before
"Toggle and Reset Pinning" is fine; keep the document's style). Cover:
enabling (`dra.enabled` in common config + `dra` on the type), the
`deviceName` template, requirements (minBalloons, maxCPUs, no
namespaces/matchExpressions/components), what is published (attributes,
capacity, node-allocatable mapping), claim examples (whole balloon, N
CPUs), the placement rules and error cases, `DRA_BALLOON` env, feature
gates (`DRAConsumableCapacity`, `DRANodeAllocatableResources`), runtime
requirement (containerd >= 2.3), and the limitations list from
03-design-cpu-accounting.md. Also add `dra` to the balloon type option
reference if the document has a table of options.

## 7. Unit tests (minimal)

- `pkg/resmgr/dra`: `ClaimOfCDIDevice` round trip and rejections.
- `cmd/plugins/balloons/policy/dra_test.go`: `draDeviceName` expansion;
  config validation of DRA rules (construct `BalloonsOptions` and call
  the validation on a `balloons` with `p.draClaims` set; if
  `validateConfig` needs more state than that, factor the DRA checks into
  `validateDRAConfig(bpoptions) error` and test that); `draClaimEdits`.
  Do not build a full policy with sysfs; keep tests pure.

## 8. Write 06-implementation-notes.md

List deviations from this spec, anything left undone, and how the
changes were verified (commands and results).
