# DRA Step 7: Prepare/Unprepare + CDI Writer

## Overview

Implement `PrepareResourceClaims` and `UnprepareResourceClaims` in `pkg/resmgr/dra/`, add a CDI
writer backed by the upstream `tags.cncf.io/container-device-interface` library, persist claim
state through the existing cache via an exported `ClaimStore` interface, and add restart
reconciliation and post-Reconfigure re-accounting. This is the load-bearing step.

**Non-HP CPU class support is deferred.** `pct.PunitInfo` carries capacity counts but not
per-punit CPU lists, making a non-HP pick non-implementable without extending PunitInfo.
Non-HP devices are filtered out of DRA publication in this step to avoid binding unpreparable
claims. Tracked as follow-up.

Companion to [design.md](../dra/design.md) "Prepare/Unprepare" and [plan.md](../dra/plan.md)
Step 7.

## Context (from discovery)

- Files: `pkg/resmgr/cpuclass/internal/pct/pct.go`, `pkg/resmgr/cpuclass/cpuclass.go`,
  `pkg/resmgr/cpuclass/dra.go`, `pkg/resmgr/dra/{plugin,deps,cdi,state}.go`
- Device attrs (`nri/cpuClass`, `nri/packageID`, `nri/punitID`) are `*string`/`*int64` —
  nil-check before dereferencing
- CPU count: `q, ok := r.ConsumedCapacity["nri/cpus"]; n := int(q.Value())` — map index is
  non-addressable so `.Value()` (pointer receiver) cannot be called directly on it; also
  `ConsumedCapacity` may be absent if `DRAConsumableCapacity` feature gate is off
- `claim.Status.Allocation` is a pointer — nil-check required; every input claim UID must
  appear in the result map (`draplugin.go:120-122`)
- `ShareID` in both `DeviceRequestAllocationResult` and `kubeletplugin.Device` is `*types.UID` —
  store as `string` in `ResultAlloc` (`""` = unset), nil-check on read, rebuild `*types.UID`
  when populating `kubeletplugin.Device`
- CDI device names must be unique within a spec AND valid per `parser.ValidateDeviceName`
  (only `[A-Za-z0-9._-:]`; first and last chars must be alphanumeric; no `/`).
  `DeviceRequestAllocationResult.Request` may contain `/` for `firstAvailable` subrequests.
  Multiple results can share the same `Request` **and** the same `Device` (e.g. shared
  `AllowMultipleAllocations` claim with `count: 2` where two results have identical
  Request+Device but different ShareID). Use a shared helper that includes a per-result index:
  `cdiDeviceName(uid types.UID, request, device string, idx int) string`
  → `"claim-<uid>-<sanitize(request)>-<device>-<idx>"` where `sanitize` replaces `/` and any
  invalid CDI char with `-`; validate via `parser.ValidateDeviceName` at test time.
  Call from both `cdi.go` (using `d.Name` which is precomputed) and `plugin.go`.
- Concurrency: design.md:238 states DRA and NRI sync on the resmgr lock. Enforce via
  `WithLock func(func())` in Deps. Whole Prepare/Unprepare body (including `deviceIndex` and
  ClaimAllocator calls) runs inside `WithLock`. `PublishResources` must also guard its
  `ValidateClasses` + `DRADevices` calls under `WithLock`.
- `RestoreClaimsLocked` reentrancy: `resource-manager.go:349` calls `m.Lock()` around `apply()`,
  which calls `policy.Reconfigure()` (which resets `hpDRAUsed`). Re-accounting must happen inside
  that same lock via **exported** `RestoreClaimsLocked() error` (no locking — assumes caller holds
  the resmgr lock; godoc: "caller must already hold the resmgr lock; do not call via WithLock");
  `RestoreClaims() error` is the `WithLock` wrapper for external callers that are not already
  holding the lock. `PublishResources` and `RestoreClaims` must NOT be called while holding the
  resmgr lock (re-entrancy deadlock); Step 8 schedules re-publish and RestoreClaims outside
  `apply()`, and wires `RestoreClaimsLocked()` inside `apply()` after `policy.Reconfigure()`
- Persistence: `cache.GetPolicyEntry` natively handles `map[string]string`; use a named variable
  `m := map[string]string{}; ok := c.GetPolicyEntry(key, &m)` — a composite literal address is
  unaddressable in this context and the unmarshal target would be unreachable. `cache.Save()` is
  a no-op when `saveBlock > 0` (BlockSave window) — data is still in-memory `policyData` and
  will persist on the next unblocked save; note this in the ClaimStore contract
- CDI version: assemble the `specs_go.Spec` first, then set `spec.Version` from
  `specs_go.MinimumRequiredVersion(spec)` which takes the assembled spec as input
- `ListClaims()`: derive UID from `filepath.Base(s.GetPath())` by stripping
  `<vendor>-<class>_` prefix and `.yaml` suffix; skip specs that don't match (foreign files
  must survive the orphan sweep)
- `pct` is internal: inject via `cpuclass.Handler` pass-throughs (Task 2)
- `dra` → `cache` import is cycle-free (verified); `NewCacheClaimStore` lives in `state.go`
- Related: [plan.md Step 7](../dra/plan.md#step-7----prepareunprepare-implementation--cdi-writer)

## Development Approach

- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- **Every task MUST include new/updated tests**
- All tests must pass before starting the next task
- Update this plan file when scope changes

## Testing Strategy

Unit tests: state serialization (cpuset round-trip, ShareID nil handling), CDI writer (device
naming, foreign spec survival, same-request multi-device), Prepare/Unprepare (idempotency,
nil Allocation, multi-result claim, foreign-driver results, subrequest `/` in name).
Integration: Prepare → CDI written → Unprepare → CDI removed.
Restart: ClaimStore populated → Start() → live claims restored, stale dropped, orphans swept.
Reconfigure: claim prepared → hpDRAUsed reset → RestoreClaimsLocked() → accounting rebuilt.

## Progress Tracking

Mark completed `[x]` immediately. ➕ newly discovered. ⚠️ blockers.

## Solution Overview

**Concurrency:** `WithLock func(func())` in Deps (resmgr write-lock closure). All Prepare,
Unprepare, RestoreClaims/RestoreClaimsLocked, and `PublishResources`' device-listing section
run inside `WithLock`. No allocator mutex.

**CDI spec:** one spec file per claim (`cdi.GenerateTransientSpecName(vendor, class, string(uid))`),
one `specs_go.Device` per allocation result within it. The CDI device name is precomputed by
`plugin.go` and stored in `CDIDevice.Name`:
```
name := cdiDeviceName(uid, r.Request, r.Device, i)  // i = per-result index
→ "claim-<uid>-<sanitize(request)>-<device>-<i>"
sanitize: replace '/' and invalid CDI chars with '-'; trim leading/trailing non-alphanumeric
```
`spec.Kind = vendor + "/device"` (must be set before `MinimumRequiredVersion`).
`PrepareResult.Devices`: one `kubeletplugin.Device` per allocation result:
```
{Requests: []string{r.Request}, PoolName: r.Pool, DeviceName: r.Device,
 CDIDeviceIDs: []string{parser.QualifiedName(vendor, "device", name)},
 ShareID: shareIDPtr(alloc.ShareID)}  // *types.UID, nil if ""
```

**State types (exported):**
```go
type ResultAlloc struct {
    Request   string // r.Request
    Pool      string // r.Pool
    Device    string // r.Device (the DRA device name)
    ShareID   string // string(*r.ShareID) or "" if r.ShareID==nil
    ClassName string // nri/cpuClass
    PkgID     int    // nri/packageID
    PunitID   int    // nri/punitID
    CPUs      string // cpuset.CPUSet.String()
}
type ClaimState struct {
    UID    string        // types.UID as string
    Allocs []ResultAlloc
}
type ClaimStore interface {
    Save(claims map[types.UID]*ClaimState) error  // note: Save is no-op in BlockSave window
    Load() (map[types.UID]*ClaimState, error)
}
```
Stored as `map[string]string` (uid → JSON of ClaimState) under key `dra/claims`.

## What Goes Where

**Implementation Steps** (`[ ]`): code, tests, doc updates.
**Post-Completion** (no checkboxes): cluster verification, Step 8 wire-up.

## Implementation Steps

### Task 1: Add pct.AccountHpCpus

**Files:**
- Modify: `pkg/resmgr/cpuclass/internal/pct/pct.go`
- Modify: `pkg/resmgr/cpuclass/internal/pct/pct_test.go`

- [x] add `AccountHpCpus(pkgID, punitID int, cpus cpuset.CPUSet) error`: validate `Active()`,
  find punit, check HP-eligibility (error if not eligible), union into `hpDRAUsed[punitIdx]`;
  log over-commit warning if `hpDRAUsed + hpUsed > HPCapacity` (do NOT reject — container may
  be running)
- [x] write tests: success; HP-ineligible punit → error; allocator inactive → error; over-capacity
  → warning logged, no error, hpDRAUsed updated; double-account same CPUs (union is idempotent)
- [x] run `go test ./pkg/resmgr/cpuclass/... -race` — must pass before Task 2

### Task 2: Add ClaimAllocator pass-throughs on cpuclass.Handler

**Files:**
- Modify: `pkg/resmgr/cpuclass/cpuclass.go`
- Create: `pkg/resmgr/cpuclass/cpuclass_dra_test.go` (does not exist; use `package cpuclass`)

- [x] add `Handler.PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error)`
  delegating to `h.pct.PickHpCpus`
- [x] add `Handler.ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet)` delegating to
  `h.pct.ReleaseHpCpus`
- [x] add `Handler.AccountHpCpus(pkgID, punitID int, cpus cpuset.CPUSet) error` delegating to
  `h.pct.AccountHpCpus`
- [x] add `Handler.IsHPClass(className string) bool` delegating to `h.pct.IsHPClass`
- [x] write tests: each pass-through delegates correctly; inactive allocator returns appropriate
  error/false
- [x] NOTE: the `dra.ClaimAllocator` compile-time assertion is deferred to Task 4 — `ClaimAllocator`
  is not yet defined at Task 2 time
- [x] run `go test ./pkg/resmgr/cpuclass/... -race` — must pass before Task 3

### Task 3: Filter non-HP devices from DRADevices

**Files:**
- Modify: `pkg/resmgr/cpuclass/dra.go`
- Modify: `pkg/resmgr/cpuclass/dra_test.go`

- [x] add `hpOnly bool` parameter to `buildDRADevices`; when true, skip classes where
  `isHP(className) == false`; comment: "non-HP DRA deferred — PunitInfo has no per-punit CPU
  list; see plan.md Step 7"
- [x] update `Handler.DRADevices` to pass `hpOnly: true`
- [x] update tests to cover the filter; mixed HP/non-HP config → only HP devices emitted
- [x] run `go test ./pkg/resmgr/cpuclass/... -race` — must pass before Task 4

### Task 4: Define state types, extend deps.go, fix PublishResources

Task 4 defines `ResultAlloc`/`ClaimState` structs first so that the `ClaimStore` interface
(same task, same package) compiles. Marshal/unmarshal and `NewCacheClaimStore` come in Task 6.

**Files:**
- Create: `pkg/resmgr/dra/state.go` (struct definitions only; persistence added in Task 6)
- Modify: `pkg/resmgr/dra/deps.go`
- Modify: `pkg/resmgr/dra/plugin.go` (PublishResources only)
- Modify: `pkg/resmgr/dra/plugin_test.go`

- [x] create `state.go` with exported `ResultAlloc` and `ClaimState` struct definitions and JSON
  tags exactly as specified in Solution Overview (CPUs as `string`, ShareID as `string`);
  add `Allocs[i]` ordering invariant comment: "`Allocs[i]` corresponds to filtered allocation
  result `i`; CDI device-name index is positional — order must be preserved on rebuild"
- [x] in `deps.go`, define exported `CDIDevice` struct: `{Name, ClassName string; CPUs cpuset.CPUSet}`
  where `Name` is the CDI device name, precomputed by the caller via `cdiDeviceName`; `WriteClaim`
  uses it directly
- [x] define exported `ClaimAllocator` interface:
  `PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error)`,
  `ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet)`,
  `AccountHpCpus(pkgID, punitID int, cpus cpuset.CPUSet) error`,
  `IsHPClass(className string) bool`
- [x] define exported `CDIWriter` interface:
  `WriteClaim(uid types.UID, devices []CDIDevice) error`,
  `RemoveClaim(uid types.UID) error`,
  `ClaimSpecExists(uid types.UID) bool`,
  `ListClaims() ([]types.UID, error)`
- [x] define exported `ClaimStore` interface:
  `Save(claims map[types.UID]*ClaimState) error`,
  `Load() (map[types.UID]*ClaimState, error)`
- [x] add to `Deps`: `ClaimAllocator`, `CDIWriter`, `ClaimStore`, `WithLock func(func())`
  (drop `CDIDir` from `Deps` — default cdiDir inside `NewCDIWriter` when empty, see Task 5)
- [x] update `New()` nil-checks for the four new required deps
- [x] add compile-time assertion in `cpuclass_dra_test.go` (from Task 2):
  `var _ dra.ClaimAllocator = (*Handler)(nil)` (in-package test, so `Handler` not `cpuclass.Handler`;
  import `pkg/resmgr/dra` — no cycle, verified)
- [x] in `PublishResources`: wrap the `deps.ValidateClasses()` + `deps.DeviceLister.DRADevices()`
  calls inside `deps.WithLock(func(){ ... })` to guard unsynchronized Handler access; add a
  race test covering concurrent `Reconfigure` + `PublishResources`; add godoc contract:
  "`PublishResources` must not be called while holding the resmgr lock (see RestoreClaimsLocked
  for the complementary lock-already-held variant)"
- [x] update `plugin_test.go` fakes to satisfy the new interfaces
- [x] run `go test ./pkg/resmgr/dra/... -race` — must pass before Task 5

### Task 5: Add CDI dependency and implement cdi.go

**Files:**
- Create: `pkg/resmgr/dra/cdi.go`
- Create: `pkg/resmgr/dra/cdi_test.go`
- Modify: `go.mod`, `go.sum`

- [x] run `go get tags.cncf.io/container-device-interface/pkg/cdi tags.cncf.io/container-device-interface/specs-go tags.cncf.io/container-device-interface/pkg/parser`
- [x] run `go mod tidy` (only; `make verify-godeps` requires a clean working tree —
  run it in Task 10 after committing)
- [x] implement `cdiDeviceName(uid types.UID, request, device string, idx int) string`:
  replace `/` and any invalid CDI device name char with `-`; trim any leading/trailing
  non-alphanumeric characters; concatenate as
  `"claim-"+string(uid)+"-"+sanitized+"-"+device+"-"+strconv.Itoa(idx)`;
  validate result with `parser.ValidateDeviceName` in unit test (panics if invalid — catching
  it early is the point); add unit test for basic case, subrequest with `/`, and two results
  with identical Request+Device+different idx → distinct valid names
- [x] create `cdiWriter` struct with fields: `cache *cdi.Cache`, `vendor string`, `class string`,
  `cdiDir string` (needed by `ClaimSpecExists` to stat the spec file path — `cdi.Cache` exposes
  no accessor for its spec dirs; open cache with `cdi.WithAutoRefresh(false)`,
  `cdi.WithSpecDirs(cdiDir)`)
- [x] implement `NewCDIWriter(driverName, cdiDir string) (CDIWriter, error)`:
  if `cdiDir == ""` default to `"/var/run/cdi"` (do NOT use `Deps.CDIDir` — that field was
  removed; the default lives here);
  validate vendor/class via `parser.ValidateVendorName(driverName)` and
  `parser.ValidateClassName("device")`; call `os.MkdirAll(cdiDir, 0750)` before opening the
  CDI cache
- [x] implement `WriteClaim(uid types.UID, devices []CDIDevice) error`:
  return error immediately if `len(devices) == 0` (CDI rejects specs with no devices);
  for each `d` in devices, build:
  ```
  specs_go.Device{Name: d.Name, ContainerEdits: specs_go.ContainerEdits{
      Env: []string{"NRI_CLASS=<d.ClassName>", "NRI_CPU<id>=1", ...}}}
  ```
  (env vars must go in `ContainerEdits.Env`; an empty `ContainerEdits` makes `Device.validate()`
  return `"invalid device, empty device edits"` and `WriteSpec` rejects the whole spec);
  assemble `specs_go.Spec{Kind: vendor+"/device", Devices: [...]}`; then set
  `v, err := specs.MinimumRequiredVersion(&spec); if err != nil { return err }; spec.Version = v`
  (set Kind BEFORE `MinimumRequiredVersion` — the function reads it; set Version AFTER assembly);
  call `cache.WriteSpec` using `cdi.GenerateTransientSpecName(vendor, class, string(uid))`
  (`GenerateTransientSpecName` is in `pkg/cdi`, not `pkg/parser`)
- [x] implement `RemoveClaim(uid types.UID) error`: `cache.RemoveSpec` with same transient spec
  name; `ErrNotExist` → success
- [x] implement `ClaimSpecExists(uid types.UID) bool`: stat
  `<cdiDir>/<vendor>-<class>_<string(uid)>.yaml` (the `.yaml` suffix is appended by WriteSpec)
- [x] implement `ListClaims() ([]types.UID, error)`: call `cache.Refresh()` first; log any
  Refresh errors at warn level and continue (`GetVendorSpecs` never errors, Refresh errors
  are from foreign/malformed specs in the dir — they must not abort our sweep); iterate
  `cache.GetVendorSpecs(vendor)` (class = `"device"`); for each spec `s`, derive UID from
  `filepath.Base(s.GetPath())` by stripping prefix `<vendor>-<class>_` and suffix `.yaml`;
  skip entries that do not match this pattern — foreign specs must survive
- [x] unit tests: WriteClaim → env vars on disk (golden check, verify spec.Kind is set);
  remove; idempotent remove; ClaimSpecExists; ListClaims with two claims; same-request +
  same-device + different idx → two distinct valid CDI device names, spec writes without
  duplicate-name error; malformed foreign spec in cdiDir → Refresh logs warning, ListClaims
  still returns our UIDs, orphan sweep does not remove the foreign file
- [x] run `go test ./pkg/resmgr/dra/... -race` — must pass before Task 6

### Task 6: Claim state persistence (state.go — marshal/unmarshal/store)

`ResultAlloc` and `ClaimState` structs were already created in Task 4. This task adds the
persistence layer to the same file.

**Files:**
- Modify: `pkg/resmgr/dra/state.go` (add functions to file created in Task 4)
- Create: `pkg/resmgr/dra/state_test.go`
- [x] define `draClaimsKey = "dra/claims"` constant
- [x] implement `marshalClaims(claims map[types.UID]*ClaimState) (map[string]string, error)` —
  JSON-encode each ClaimState into the value
- [x] implement `unmarshalClaims(raw map[string]string) (map[types.UID]*ClaimState, error)`
- [x] implement `NewCacheClaimStore(c cache.Cache) ClaimStore`:
  - `Save`: `m, err := marshalClaims(claims); if err != nil { return err }; c.SetPolicyEntry(key, m); return c.Save()`  
    (note: `c.Save()` is a no-op when `saveBlock > 0` — data is still in-memory `policyData` and
    persists on the next unblocked Save; ClaimStore.Save callers must tolerate this)
  - `Load`: `m := map[string]string{}; if !c.GetPolicyEntry(key, &m) { return nil, nil }; return unmarshalClaims(m)`
    (named variable is required — a composite-literal address is not addressable in this call)
- [x] export `NewCacheClaimStore` so Step 8 can use it outside package `dra`; note the
  `dra → cache` import is cycle-free
- [x] unit tests (use `cache.NewCache(cache.Options{CacheDir: t.TempDir()})` for a real cache):
  round-trip Save → Load with multi-alloc claim; empty cache → empty map; CPUs survive round-trip
  (round-trip: `got, err := cpuset.Parse(alloc.CPUs); ... if !got.Equals(original)` —
  `cpuset.CPUSet` cannot be compared with `==`, and `Parse` is two-valued); verify Load returns saved data
  (not empty due to composite-literal bug); BlockSave window test (optional: verify in-memory
  data is still accessible after Save no-op)
- [x] run `go test ./pkg/resmgr/dra/... -race` — must pass before Task 7

### Task 7: Implement PrepareResourceClaims

**Files:**
- Modify: `pkg/resmgr/dra/plugin.go`
- Modify: `pkg/resmgr/dra/plugin_test.go`

- [x] declare sentinel errors: `errNilAllocation`, `errMissingConsumedCapacity`,
  `errNonHPNotSupported` (unexported package-level vars or `errors.New(...)`)
- [x] add unexported helper `shareIDPtr(s string) *types.UID` — returns nil if `s == ""`,
  otherwise `ptr.To(types.UID(s))`
- [x] add `claims map[types.UID]*ClaimState` to `Plugin`; initialize empty map in `New()`
- [x] add unexported `deviceIndex() (map[string]deviceInfo, error)` — calls
  `deps.DeviceLister.DRADevices(driverName)`, builds name → `{ClassName, PkgID, PunitID}`;
  nil-check all three attributes before dereferencing
- [x] add unexported `allClaimedCPUs() cpuset.CPUSet` — union of all CPUs in `p.claims`
- [x] remove the `_Stub` tests (`TestPrepareResourceClaims_Stub`,
  `TestUnprepareResourceClaims_Stub`); keep `errNotImplemented` until Task 8 removes it
  (Unprepare still returns it until then, and removing it in Task 7 breaks compilation)
- [x] replace stub `PrepareResourceClaims` with real implementation; entire body inside
  `deps.WithLock(func(){ ... })`:
  1. For each claim: ensure claim UID gets a map entry even on error — use a per-claim inner
     function `func() PrepareResult { ... }()` whose return value is assigned to `result[uid]`;
     do NOT use `defer` at the outer closure level (fires at closure exit, overwrites success
     values set by later iterations)
  2. Nil-check `claim.Status.Allocation`; if nil → `PrepareResult{Err: errNilAllocation}`
  3. Filter results: collect only `r.Driver == p.driverName`; if none → return `PrepareResult{}`
     (empty Devices, no error — valid; skip WriteClaim, skip p.claims/Save)
  4. Idempotency: if `p.claims[uid]` exists → if `CDIWriter.ClaimSpecExists(uid)` true, rebuild
     and return PrepareResult; if spec missing, re-write (WriteClaim) before returning
  5. For each filtered result: look up `deviceIndex()` → attrs; nil attr → per-claim error
  6. Read count: `q, ok := r.ConsumedCapacity["nri/cpus"]`; absent or zero →
     `PrepareResult{Err: errMissingConsumedCapacity}`
  7. HP gate: `deps.ClaimAllocator.IsHPClass(className)` false → `PrepareResult{Err: errNonHPNotSupported}`
  8. `deps.ClaimAllocator.PickHpCpus(pkgID, punitID, n, allClaimedCPUs())`; on failure:
     rollback all previously picked in this claim via `ReleaseHpCpus`
  9. Build `[]CDIDevice` from all picks; for each result at index `i`:
     `name := cdiDeviceName(uid, r.Request, r.Device, i)`;
     `CDIDevice{Name: name, ClassName: attrs.ClassName, CPUs: picked}`
  10. `deps.CDIWriter.WriteClaim(uid, cdiDevices)`; on failure: rollback all picked CPUs by
      iterating the accumulating `[]ResultAlloc` (not `cdiDevices` — only ResultAlloc carries
      PkgID/PunitID needed for `ReleaseHpCpus`)
  11. Build and store `ClaimState`; call `deps.ClaimStore.Save(p.claims)` (log error, no rollback)
  12. Return `PrepareResult{Devices: per result at index i with
      CDIDeviceIDs: []string{parser.QualifiedName(vendor, "device", cdiDeviceName(uid, r.Request, r.Device, i))},
      ShareID: shareIDPtr(alloc.ShareID)}`
     (note: `deviceIndex()` called ONCE per PrepareResourceClaims invocation, not per result)
- [x] write tests: single HP success; repeat Prepare (idempotent, CDI spec present); repeat Prepare
  (spec missing → re-written); nil Allocation → per-claim error; foreign-driver-only results →
  empty PrepareResult no error; unknown device → per-claim error; nil attr → per-claim error;
  absent ConsumedCapacity → per-claim error; non-HP → errNonHPNotSupported; PickHpCpus failure →
  CPUs rolled back; CDI write failure → CPUs rolled back; multi-result claim two punits; ShareID
  nil → nil in Device; ShareID set → non-nil in Device; all UIDs in result map including errors;
  subrequest `/` in Request name → valid CDI device name (no validation error)
- [x] run `go test ./pkg/resmgr/dra/... -race` — must pass before Task 8

### Task 8: Implement UnprepareResourceClaims

**Files:**
- Modify: `pkg/resmgr/dra/plugin.go`
- Modify: `pkg/resmgr/dra/plugin_test.go`

- [x] remove `errNotImplemented` (last reference — Unprepare stub is being replaced)
- [x] replace stub with real implementation inside `deps.WithLock`:
  1. For each `obj`: look up `p.claims[uid]`; absent → log warning, add `nil` to per-UID map
  2. For each `ResultAlloc`: `cs, err := cpuset.Parse(alloc.CPUs); if err != nil { log.Warnf(...)
     continue /* still remove CDI and claim entry */ }; ReleaseHpCpus(pkgID, punitID, cs)`
  3. `CDIWriter.RemoveClaim(uid)` unconditionally; log warning on error, continue
  4. Delete `p.claims[uid]`
  5. After all UIDs: `ClaimStore.Save(p.claims)` (one batch write)
  6. Return per-UID error map (nil for success/absent)
- [x] write tests: known claim → released + CDI removed + nil in map; unknown UID → warning + nil;
  CDI remove error → warning, nil in map; mixed batch
- [x] run `go test ./pkg/resmgr/dra/... -race` — must pass before Task 9

### Task 9: Restart reconciliation + post-Reconfigure re-accounting

**Files:**
- Modify: `pkg/resmgr/dra/plugin.go`
- Modify: `pkg/resmgr/dra/plugin_test.go`

- [x] add exported `LiveClaimClasses() map[string]int` — returns `className → liveClaimCount`
  over all entries in `p.claims`; used by Step 8's Reconfigure refusal check (resolved
  decision 8 / Option B: refuse Reconfigure that changes class-derived attributes while
  claims are live). No locking — caller must already hold the resmgr lock OR call outside
  `WithLock` (Step 8 checks inside the Reconfigure path which already holds the lock).
  Add unit test: zero claims → empty map; two claims same class → count 2; two claims different
  classes → two entries.
- [x] add exported `RestoreClaimsLocked() error` — re-runs `AccountHpCpus` for every entry in
  `p.claims` (no cache reload, no locking); godoc: "caller must already hold the resmgr lock;
  do not call via WithLock"; for each alloc: `cs, err := cpuset.Parse(alloc.CPUs); if err !=
  nil { log.Warnf(...); continue }; AccountHpCpus(pkgID, punitID, cs)`; returns combined errors
- [x] add public `RestoreClaims() error` — wraps `RestoreClaimsLocked()` inside `deps.WithLock`;
  for callers NOT already holding the lock
- [x] **Step 8 note and contract**: three methods — `Start()`, `PublishResources()`, and
  `RestoreClaims()` — **must not be called while holding the resmgr lock** (all acquire it
  via `WithLock`; resmgr `sync.RWMutex` is non-reentrant). Add this contract to the godoc of
  all three. `RestoreClaimsLocked()` is the complementary lock-held variant wired inside
  `resmgr.apply()` after `policy.Reconfigure()`, at `resource-manager.go:349` which already
  holds `m.Lock()`.
- [x] in `Start()`, after `ValidateClasses`, inside `deps.WithLock`:
  - `deps.ClaimStore.Load()` → populate `p.claims`
  - for each claim: `CDIWriter.ClaimSpecExists(uid)` true → call `AccountHpCpus` per alloc
    (on failure: log warning, keep claim — container may be running, over-commit acceptable);
    false → log `"dra plugin: dropping stale claim %s (CDI spec missing)"`, delete from p.claims
  - after any drops: `ClaimStore.Save(p.claims)`
  - orphan sweep: `CDIWriter.ListClaims()` → `RemoveClaim` for UIDs not in `p.claims`
  - document (godoc) that `cpuclass.Handler` must have completed `Configure()` before `Start()`
    so `AccountHpCpus` finds active punits; if allocator is inactive, claims are kept with a
    warning (same over-commit model)
- [x] write integration test: two claims in ClaimStore, one CDI spec present + one absent;
  Start() → only live claim in p.claims; AccountHpCpus called once; stale spec swept;
  stale claim absent from saved ClaimStore; foreign CDI spec in dir survives
- [x] write test: RestoreClaimsLocked() after hpDRAUsed manually cleared (simulates Configure
  reset); verify accounting rebuilt
- [x] write test: Start() with inactive allocator → all claims kept + over-commit warnings
- [x] run `go test ./pkg/resmgr/dra/... -race` — must pass before Task 10

### Task 10: Dependency hygiene + lint

- [x] commit all changes from Tasks 1-9 (or stage them); `verify-godeps` requires a clean
  working tree (`go mod tidy && git diff --quiet`) — run only after staging/committing
- [x] run `make verify-godeps`
- [x] run `make verify-licenses` (CDI library is Apache-2.0)
- [x] run `make golangci-lint` — fix all findings
- [x] run `go test ./pkg/resmgr/... -race` — all green

### Task 11: Verify acceptance criteria

- [x] verify Prepare allocates CPUs, writes CDI spec per request, persists state — covered by TestPrepare_SingleHPSuccess and TestPrepare_MultiResultTwoPunits
- [x] verify idempotent Prepare (spec present → same result; spec missing → re-written) — covered by TestPrepare_Idempotent_SpecPresent and TestPrepare_Idempotent_SpecMissing
- [x] verify Unprepare releases CPUs, removes CDI spec, removes persisted state — covered by TestUnprepare_KnownClaim
- [x] verify restart reconciliation: live claims restored, stale dropped, orphans swept — covered by TestStart_Reconciliation
- [x] verify RestoreClaimsLocked() re-accounts after hpDRAUsed reset — covered by TestRestoreClaimsLocked_RebuildsAccounting
- [x] verify non-HP devices not published — covered by TestPrepare_NonHP (errNonHPNotSupported) and TestBuildDRADevices hpOnly=true case in dra_test.go
- [x] run `go test ./pkg/resmgr/... -race` — all green (cache, cpuclass, pct, dra, lib/memory all pass)

### Task 12: Update documentation

- [x] update [plan.md](../dra/plan.md) Step 7 "Landed" line with commit range and deviations:
  resmgr-lock (WithLock) not allocator mutex; one CDI device per request with sanitized name;
  exported ClaimState/ResultAlloc; non-HP deferred; RestoreClaimsLocked vs. RestoreClaims
- [x] update [design.md](../dra/design.md): CDI device naming
  (`claim-<uid>-<sanitize(request)>-<device>-<idx>`), concurrency model alignment
  (resmgr lock via `WithLock`/`RestoreClaimsLocked`), `Start`/`PublishResources`/`RestoreClaims`
  must-not-hold-lock contract
- [x] update [plan.md](../dra/plan.md) "Not part of v1": add non-HP DRA pick (deferred); confirm
  class-derived attribute freshness is no longer listed (resolved decision 8, already removed)
- [x] move this plan to `docs/plans/completed/` (harness will move)

## Post-Completion

**Manual verification:**
- Deploy topology-aware with `dra.enabled: true` (Step 8 prerequisite) on a DRA-capable cluster
- Verify CDI spec appears with one device per request under `/var/run/cdi/`
- Verify containers see only their request's CPUs (`NRI_CLASS`, `NRI_CPU<n>` env vars)

**External follow-ups:**
- Feature-gate probes deferred from Step 6 — must land before Step 8
- Non-HP DRA support: extend `pct.PunitInfo` with CPU lists; `Handler.PickNonHpCpus`;
  re-enable in `buildDRADevices` (unexported)
- Step 8 wire-up: inject `*cpuclass.Handler` as `ClaimAllocator`; `NewCDIWriter(driverName,
  cdiDir)` as `CDIWriter`; `NewCacheClaimStore(cache)` as `ClaimStore`; resmgr write-lock
  closure as `WithLock`; wire `RestoreClaimsLocked()` after `policy.Reconfigure()` in
  `resmgr.apply()`; use `LiveClaimClasses()` in the Reconfigure refusal check (resolved
  decision 8 — refuse if any class-derived attribute changed and `liveClaimCount > 0`)
