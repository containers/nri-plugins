# DRA v1 step 5 — `cpuclass.Handler.DRADevices()`

## Overview

Extends `pkg/resmgr/cpuclass/dra.go` with `Handler.DRADevices(driverName string) ([]resapi.Device, error)`, which builds the `[]resapi.Device` slice in Model B shape that Step 6 passes to `kubeletplugin.PublishResources`.

Model B = one `resapi.Device` per (published cpuClass × SST-TF punit). Each device has:
- `AllowMultipleAllocations: true` ([KEP-5075](https://github.com/kubernetes/enhancements/issues/5075))
- Capacity `nri/cpus` with `RequestPolicy` (default=1, min=1, max=capacity, step=1)
- `NodeAllocatableResourceMappings` ([KEP-5517](https://github.com/kubernetes/enhancements/issues/5517)): `{CapacityKey: "nri/cpus", AllocationMultiplier: "1"}` → 1 `nri/cpus` = 1 `node.allocatable.cpu`
- Topology attributes: `nri/packageID`, `nri/punitID`
- Class-derived attributes: `nri/cpuClass`, `nri/pctPriority`

Deferred to v1 follow-up:
- `resource.kubernetes.io/numaNode` — blocked: needs `PunitInfo.CPUs` or sysfs per-punit lookup
- `nri/minFreqKHz`, `nri/maxFreqKHz`, `nri/guaranteedHpFreqKHz` — blocked: symbolic frequency sentinels overflow without resolver; resolver is unexported in pct/cpufreq
- `nri/energyPerformancePreference`, `nri/freqGovernor`, `nri/uncoreMin/MaxFreqKHz`, `nri/disabledCstates` — unblocked (plain config reads, no sentinel risk); deferred by choice since these are additive and can land as a one-task follow-up

This step also bumps `k8s.io/api` to v0.36.3 (exact version; pre-validated clean against this repo, see Task 1).

**Companion docs:** [design.md §Device shape](../dra/design.md#device-shape-one-device-per-cpuclass--punit), [plan.md Step 5](../dra/plan.md).

## Context (from discovery)

- **Target file:** `pkg/resmgr/cpuclass/dra.go` — already exists (Step 3). Extended here.
- **Pure builder function** (for testability): `buildDRADevices(driverName string, classes []*policyapi.CPUClass, punits []pct.PunitInfo, isHP func(string) bool) []resapi.Device`. `Handler.DRADevices` is a 3-line wrapper. Tests call `buildDRADevices` directly with controlled inputs — no real SST, no `sysfs.System`.
- **Handler fields:**
  - `h.pct.Punits() []pct.PunitInfo` — per-punit `{PkgID, PunitID, HPCapacity, NonHPCapacity}` (Step 4)
  - `h.pct.Active()` — PCT active
  - `h.pct.IsHPClass(name string) bool` — new exported wrapper (added in this step) around unexported `classIsHighPriority`; needed for assoc-only HP classification which is runtime (based on CLOS MaxFreq) and not computable from config alone
  - `h.classes []*policyapi.CPUClass` — updated on every `Configure()` call (new field)
- **`k8s.io/api@v0.36.0+` API (validated against v0.36.0 module cache):**
  - `resapi.Device.AllowMultipleAllocations *bool` — KEP-5075
  - `resapi.Device.NodeAllocatableResourceMappings map[v1.ResourceName]resapi.NodeAllocatableResourceMapping` — KEP-5517
  - `resapi.NodeAllocatableResourceMapping{CapacityKey *QualifiedName; AllocationMultiplier *resource.Quantity}`
  - `resapi.DeviceCapacity.RequestPolicy *CapacityRequestPolicy`
- **Current `k8s.io/api` version:** v0.31.2 (lacks KEP-5075/5517 fields). Must bump to v0.36.3 along with `k8s.io/apimachinery`, `k8s.io/client-go`, `k8s.io/kubelet` for consistency. The reviewer validated v0.36.0 clean (`go build ./...` + `go test ./pkg/...` pass, no source changes) in a scratch copy. v0.36.3 is an unvalidated patch bump; verify in Task 1.
- **Non-PCT class overlap:** a config with two published non-PCT classes (e.g. `default` + `idle`) produces two devices per punit each advertising the full `NonHPCapacity`. This is a v1 known limitation — the validator from Step 3 only checks PCT tiers. Accepted and documented in Post-Completion.

## Development Approach

- **Testing approach:** TDD — tests first.
- Complete each task fully before moving to the next.
- Every task that changes code ends with a passing test run.
- The go.mod bump (Task 1) lands before any code using new API types (Tasks 2-4).
- Task 1 must be committable standalone (build + tests green).

## Testing Strategy

- **Unit tests** in `pkg/resmgr/cpuclass/dra_test.go` (existing file): test `buildDRADevices` directly with controlled `[]pct.PunitInfo` and `isHP func(string) bool`. No fake SST/sysfs needed.
- Cases:
  - one HP class + one punit (`PkgID=0, PunitID=0`) → one device: `AllowMultipleAllocations=true`, `nri/cpus` max=HPCapacity, `NodeAllocatableResourceMappings` set, `nri/packageID=0` and `nri/punitID=0` **present** (not omitted)
  - one non-HP class + one punit → max=NonHPCapacity
  - HP class + HPCapacity==0 → that (class, punit) pair skipped
  - NonHPCapacity==0 → that pair skipped
  - `dra.publish: false` → class excluded entirely
  - two classes × two punits → four devices
  - empty classes → empty result
  - empty punits → empty result
  - DNS-label validity of device names (regex check per device, including suffix length)
  - long class name (>60 chars) → device name ≤ 63 chars
  - two classes that sanitize to same base → names are distinct (collision dedup)
  - non-PCT class (`PctPriority==""`, `SstClosID==nil`) → `nri/pctPriority` attribute absent (not emitted as `""`)
- HP capacity clamp (`punitHPCapacity` not intersecting `allowed`) deferred to Step 4 follow-up; not tested here.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.

## Solution Overview

### `buildDRADevices` pure builder

```go
func buildDRADevices(
    driverName string,   // currently unused in device names/attributes; retained for Step 6 call-site stability and future use as an attribute-domain prefix
    classes    []*policyapi.CPUClass,
    punits     []pct.PunitInfo,
    isHP       func(className string) bool,
) []resapi.Device
```

For each `cc` in `classes` where `cc.DRAPublish()`:
  For each `pu` in `punits`:
    - Compute `capacity = HPCapacity` if `isHP(cc.Name)`, else `NonHPCapacity`.
    - If `capacity == 0`: skip (zero-capacity `RequestPolicy` is invalid).
    - Emit `resapi.Device` with name, attributes, `AllowMultipleAllocations`, capacity, `NodeAllocatableResourceMappings`.

### Device name

Per class (before the punit loop), compute a stable sanitized base for that class — so the same class across punits uses the same base without triggering the dedup counter:

```
// Once per class name:
const disambigLen = 3  // "-N" up to 2-digit suffix + spare
suffix0 = "-pkg0-punit0"  // worst-case-length reference; actual suffix varies
maxBase = 63 - len(suffix0) - disambigLen   // = 63 - 12 - 3 = 48
baseForClass[cc.Name] = sanitizeBase(cc.Name, maxBase)

// Per (class, punit):
suffix = "-pkg" + strconv.Itoa(pu.PkgID) + "-punit" + strconv.Itoa(pu.PunitID)
base   = baseForClass[cc.Name]
name   = base + suffix   // total ≤ 48 + 12 + slack = ≤ 63
```

`sanitizeBase(s string, maxLen int)`: lowercase, replace `[^a-z0-9]+` with `-`, trim leading/trailing `-`, truncate to `maxLen`. If empty after sanitization → fallback `"class"`.

Dedup: only applies when **two different class names** sanitize to the same base. In that case the second class gets a numeric suffix on its base (e.g. `"hp-2"`). Deduplicate within a single `buildDRADevices` call using `seen map[string]int` keyed on `className` (not on base) — so the same class across punits never triggers the counter.

### Device attributes (v1)

```go
Device.Attributes = map[QualifiedName]DeviceAttribute{
    "nri/packageID": intAttr(int64(pu.PkgID)),   // always emitted, even when 0
    "nri/punitID":   intAttr(int64(pu.PunitID)), // always emitted, even when 0
    "nri/cpuClass":  strAttr(cc.Name),            // always emitted
    // "nri/pctPriority" only emitted when non-empty (non-PCT classes omit it)
}
if cc.PctPriority != "" {
    Device.Attributes["nri/pctPriority"] = strAttr(cc.PctPriority)
}
```

Topology attributes (`nri/packageID`, `nri/punitID`) are **always** emitted even when the value is 0 — `PkgID==0`/`PunitID==0` is the normal single-socket case, and `MatchAttribute` semantics exclude devices that lack an attribute entirely. Optional class-derived string attributes are omitted only when empty (avoiding CEL false-positives on `""`-valued attributes).

### `NodeAllocatableResourceMappings` (KEP-5517)

```go
Device.NodeAllocatableResourceMappings = map[corev1.ResourceName]resapi.NodeAllocatableResourceMapping{
    corev1.ResourceCPU: {
        CapacityKey:          ptr.To(resapi.QualifiedName("nri/cpus")),
        AllocationMultiplier: ptr.To(resource.MustParse("1")),
    },
}
```

### `pct.Allocator.IsHPClass(name string) bool`

Added in `pkg/resmgr/cpuclass/internal/pct/pct.go` — a one-line exported wrapper around `classIsHighPriority`. This is the only source of truth for HP classification and avoids duplicating the assoc-only MaxFreq logic in `dra.go`.

### `Handler.classes` cache

```go
// classes is the last-applied cpuClass list.
classes []*policyapi.CPUClass
```

Set in `Configure`:
```go
h.classes = spec.Classes  // caller-owned slice; not deep-copied (consistent with existing pct.Configure behavior)
```

Called from `DRADevices` under the same resmgr serialization (document: must be called on the resmgr goroutine or under the resmgr lock, same as all other Handler methods).

## Technical Details

### go.mod changes (exact versions)

```
k8s.io/api             v0.36.3   # was v0.31.2
k8s.io/apimachinery    v0.36.3   # was v0.33.1
k8s.io/client-go       v0.36.3   # was v0.31.2
k8s.io/kubelet         v0.36.3   # was v0.31.2
```

`k8s.io/dynamic-resource-allocation` is NOT added in this step — it belongs to Step 6 (kubelet plugin wiring).

### `Handler.DRADevices` wrapper

```go
func (h *Handler) DRADevices(driverName string) ([]resapi.Device, error) {
    punits := h.pct.Punits()  // snapshot once (each call allocates)
    if !h.pct.Active() || len(punits) == 0 {
        return []resapi.Device{}, nil
    }
    return buildDRADevices(driverName, h.classes, punits, h.pct.IsHPClass), nil
}
```

Returns `[]resapi.Device{}` (not nil) when empty; always returns nil error in v1 (errors deferred to Step 6 where device schema validation can be added).

## What Goes Where

**Implementation Steps (checkboxes below):** go.mod bump, pct.IsHPClass export, Handler.classes cache, buildDRADevices + tests.

**Post-Completion:** update `docs/dra/plan.md` Step 5 with Landed pointer; move plan to `docs/plans/completed/`.

## Implementation Steps

### Task 1: Bump `k8s.io/api` and related modules to v0.36.3

**Files:**
- Modify: `go.mod`, `go.sum`

- [x] run `go get k8s.io/api@v0.36.3 k8s.io/apimachinery@v0.36.3 k8s.io/client-go@v0.36.3 k8s.io/kubelet@v0.36.3`
- [x] run `go mod tidy`
- [x] run `go build ./...` — must compile; fix any API breakage (pre-validated clean in reviewer's scratch build, but record any actual breakage here with ➕)
- [x] run `go test ./pkg/...` — existing tests must pass
- [x] commit this task standalone before proceeding

### Task 2: Export `pct.Allocator.IsHPClass`

**Files:**
- Modify: `pkg/resmgr/cpuclass/internal/pct/pct.go`
- Modify: `pkg/resmgr/cpuclass/internal/pct/pct_test.go`

- [x] add `func (a *Allocator) IsHPClass(className string) bool { return a.classIsHighPriority(className) }` — one-line exported wrapper
- [x] write `TestIsHPClass`: HP class returns true; non-HP class returns false; unknown class returns false; inactive allocator returns false
- [x] run `go test ./pkg/resmgr/cpuclass/internal/pct/...` — must pass
- [x] run `go build ./...` — must compile

### Task 3: Cache `classes` on `Handler` + write TDD tests for `buildDRADevices`

**Files:**
- Modify: `pkg/resmgr/cpuclass/cpuclass.go` (add `classes` field + populate in `Configure`)
- Modify: `pkg/resmgr/cpuclass/dra_test.go`

- [x] add `classes []*policyapi.CPUClass` field to `Handler`
- [x] set `h.classes = spec.Classes` in `Configure` before existing allocator configure calls
- [x] write `TestBuildDRADevices` table-driven (compile will fail until Task 4 — Tasks 3+4 are committed together):
  - one HP class + one punit (`PkgID=0, PunitID=0`) → one device: `AllowMultipleAllocations=true`, `nri/cpus` max=HPCapacity, `NodeAllocatableResourceMappings` present, `nri/packageID=0` and `nri/punitID=0` **present**, device name DNS-valid
  - one non-HP class + one punit → max=NonHPCapacity
  - HP class + HPCapacity==0 → device skipped for that punit
  - NonHPCapacity==0 → device skipped for that punit
  - class with `dra.publish: false` → excluded
  - two classes × two punits → four devices; assert all four device names explicitly (same class across punits must not trigger dedup counter)
  - empty classes → empty result
  - empty punits → empty result
  - long class name (>60 chars) → device name ≤ 63 chars
  - two classes that sanitize to same base → distinct names
  - non-PCT class → `nri/pctPriority` absent
- [x] confirm tests fail to compile (expected — `buildDRADevices` not yet defined; Tasks 3+4 are committed as one unit)

### Task 4: Implement `buildDRADevices`, `Handler.DRADevices`, and helpers

**Files:**
- Modify: `pkg/resmgr/cpuclass/dra.go`

- [x] add imports: `resapi "k8s.io/api/resource/v1"`, `corev1 "k8s.io/api/core/v1"`, `"k8s.io/apimachinery/pkg/api/resource"`, `kptr "k8s.io/utils/ptr"` (aliased to avoid conflict with test-local `ptr` helper), `"regexp"`, `"strconv"`, `"strings"`
- [x] implement `sanitizeBase(s string, maxLen int) string` — lowercase, replace `[^a-z0-9]+` → `-`, trim, fallback to `"class"` if empty, truncate to `maxLen`; compile `regexp.MustCompile` as a package-level var (not per-call)
- [x] implement `deviceName(classBase string, pkgID, punitID int) string` — assembles `classBase + suffix`; caller is responsible for pre-computing `classBase` once per class name via `sanitizeBase`
- [x] implement per-class base pre-computation loop in `buildDRADevices`: compute `baseForClass[cc.Name]` with dedup — if two different class names sanitize to the same base, append `-N` suffix to the second; key `seen` on `cc.Name` (not on base) so the same class across punits is unaffected
- [x] implement `intAttr(v int64) resapi.DeviceAttribute` and `strAttr(v string) resapi.DeviceAttribute` helpers (use `kptr.To` from `k8s.io/utils/ptr` for pointer fields)
- [x] implement `buildDRADevices(driverName string, classes []*policyapi.CPUClass, punits []pct.PunitInfo, isHP func(string) bool) []resapi.Device` — the pure builder; always emits topology int attributes; omits `nri/pctPriority` when empty
- [x] implement `Handler.DRADevices(driverName string) ([]resapi.Device, error)` as a wrapper calling `buildDRADevices(driverName, h.classes, h.pct.Punits(), h.pct.IsHPClass)`
- [x] run `go test ./pkg/resmgr/cpuclass/...` — all tests including `TestBuildDRADevices` must pass
- [x] run `go build ./...` — must compile

### Task 5: Verify acceptance criteria

- [x] `go test ./pkg/resmgr/cpuclass/... ./pkg/resmgr/cpuclass/internal/pct/...` passes (all new + existing)
- [x] `go build ./...` compiles cleanly
- [x] `go vet ./pkg/resmgr/cpuclass/...` is clean
- [x] `golangci-lint run ./pkg/resmgr/cpuclass/...` is clean
- [x] `Handler.DRADevices` is accessible from `pkg/resmgr/dra/` — confirm with `go build ./pkg/resmgr/dra/...`
- [x] `resapi.Device.NodeAllocatableResourceMappings` compiles at the upgraded k8s.io/api version (confirmed by build passing)
- [x] device names in test assertions pass DNS-label validation (`[a-z0-9-]+`, max 63 chars)

### Task 6: Update documentation

- [x] update `docs/dra/plan.md` Step 5 — add `Landed:` pointer; note deferred attributes (`numaNode`, freq attrs); note go.mod bump here not in Step 6
- [x] update `docs/dra/design.md` §Device shape — correct `NodeAllocatableResources` → `NodeAllocatableResourceMappings` and `CapacityMultiplier` → `AllocationMultiplier` to match actual v0.36.0 API
- [x] move this plan to `docs/plans/completed/` (moved by harness)

## Post-Completion

**Known v1 limitations:**
- No `resource.kubernetes.io/numaNode` attribute: cross-driver co-placement via `matchAttribute: numaNode` not available in v1. Users can use `matchAttribute: nri/packageID` for within-package co-placement — however this only works intra-claim and using a non-standard attribute name, so cross-driver GPU/NIC co-placement (design.md's stated goal for `numaNode`) is not achieved.
- No frequency attributes (`nri/minFreqKHz`, etc.): CEL selection by frequency tier not available in v1. Unblocked attributes (`freqGovernor`, `energyPerformancePreference`, etc.) are easy follow-ups.
- Non-SST-TF nodes (no punits): `DRADevices` returns empty, publishing zero devices. design.md says granularity "widens to (tier, package)", but implementing the pseudo-punit fallback is deferred. Add to design.md Known v1 limitations.
- Two published non-PCT classes on the same punit: both advertise `NonHPCapacity` independently, allowing mild overcommit. Tracked for v2 (Step 3 validator does not cover non-PCT tiers).

**go.mod note:** `k8s.io/code-generator` stays at v0.33.1 (`scripts/hack/update_codegen.sh` pinned version); `pkg/generated/` client code recompiles cleanly without source changes. `google.golang.org/protobuf` will be pinned to a pseudo-version by `go mod tidy` — expected.
