# DRA v1 step 4 — `pct.Allocator.PickHpCpus` / `ReleaseHpCpus` + capacity helpers

## Overview

Extends `pkg/resmgr/cpuclass/internal/pct.Allocator` with:

- **`Punits() []PunitInfo`** — exported snapshot of per-punit topology with HP and non-HP capacity. Consumed by Step 5 (`cpuclass.Manager.DRADevices()`) to set `RequestPolicy.max` for each (class × punit) device.
- **`PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error)`** — selects `n` HP-eligible CPUs from a specific punit by (PkgID, PunitID), recording them in the new `hpDRAUsed` map.
- **`ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet)`** — releases CPUs previously held via `PickHpCpus`.

**New internal map `hpDRAUsed map[int]cpuset.CPUSet`** (index-keyed like `hpUsed`) — DRA-claimed HP CPUs are tracked here rather than in `hpUsed`, so `clearHpUsage` (called from `trackHpUsage` for non-DRA allocations) can never evict them. `hpInUseCpus()` and `hpReserveCpus()` union both maps.

**Companion docs:** [design.md](../dra/design.md), [plan.md Step 4](../dra/plan.md).

## Context (from discovery)

- **Target file:** `pkg/resmgr/cpuclass/internal/pct/pct.go` (`internal` to `pkg/resmgr/cpuclass/`). Step 5 lives in `pkg/resmgr/cpuclass/dra.go` (`package cpuclass`) and can only access *exported* symbols on `pct.Allocator`.
- **Existing HP accounting state:**
  - `a.punits []pctPunit` (private; index-stable within one `Configure()` call).
  - `a.hpUsed map[int]cpuset.CPUSet` — per-punit-index, non-DRA HP allocations.
  - `a.hpEligiblePunit map[int]bool`.
  - `a.allowed cpuset.CPUSet`.
  - `pctPunitID{PkgID, PunitID int}` — existing struct used by `TFStatus()` and similar SST maps.
- **Why `hpDRAUsed` must be separate from `hpUsed`:** `trackHpUsage` (pct.go:561) calls `clearHpUsage(cpus)` unconditionally before any other action. A `UseClass("<non-HP class>", cpus)` call whose CPU set overlaps a DRA-held CPU would silently delete it from `hpUsed`, causing `hpInUseCpus`/`hpReserveCpus` to under-count HP occupancy and `ReleaseHpCpus` to become a no-op. Separate storage prevents this aliasing.
- **Why (PkgID, PunitID), not index:** punit indices are re-assigned each `Configure()` call (punits with empty `allowed` intersection are dropped and the array is compacted). The DRA driver holds a claim across Reconfigure calls; the `PunitID` in the claim allocation result is stable, the index is not.
- **Why index is still fine internally:** `hpDRAUsed` is keyed by punit index *at the time of PickHpCpus*. `PickHpCpus` and `Configure` both require the resmgr lock, so they cannot interleave. The design implication for Reconfigure (what happens to outstanding DRA holds) is a Step 7 concern, explicitly deferred.
- **Test infrastructure:** `pct_test.go` (1085 lines, `package pct`). Correct builder names: `newManagedPctForTest(t, classes, plans, allowed, sys, sst)` and `pctTestWirePunits(a)`. Tests using `GuaranteedHpCpus` need `fakeSst{punits: []pctPunit{...}}` literals with the field explicitly set (the existing `makeTwoPunitsPerPkg` helper only sets `MaxHpCpus`).

## Development Approach

- **Testing approach:** TDD — tests first.
- Complete each task fully before moving to the next.
- **CRITICAL: every task MUST include new/updated tests.**
- **CRITICAL: all tests must pass before starting next task.**
- **CRITICAL: update this plan file when scope changes during implementation.**
- No existing public API is modified; this is purely additive.

## Testing Strategy

- **Unit tests** in `pct_test.go` (`package pct`):
  - `Punits()`: returns one entry per punit; fields correct (PkgID, PunitID, HPCapacity=GuaranteedHpCpus if eligible else 0, NonHPCapacity); empty when not `Active()`.
  - `PickHpCpus`: success (returns n CPUs, `hpDRAUsed` updated, `hpUsed` *not* touched); exhaustion error; HP-ineligible punit error; (PkgID, PunitID) not found error; `held` exclusion; `Active()==false` error.
  - `ReleaseHpCpus`: CPUs removed from `hpDRAUsed`; releasing CPUs not held is a no-op; full-punit release deletes map entry; out-of-range (not found) is a no-op.
  - `hpDRAUsed` isolation: non-HP `UseClass` call overlapping DRA-held CPUs does NOT remove them from `hpDRAUsed`. DRA holds remain visible to `hpInUseCpus()` after a non-HP `UseClass`.
- All test cases use existing `newManagedPctForTest` / `pctTestWirePunits` + new `fakeSst{punits: []{GuaranteedHpCpus: N}}` fixtures.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.

## Solution Overview

### `PunitInfo` (new exported type)

```go
// PunitInfo is a snapshot of one SST punit's DRA-relevant capacity.
// Returned by Allocator.Punits(); valid within a single Configure/Reconfigure cycle.
type PunitInfo struct {
    PkgID         int
    PunitID       int
    HPCapacity    int // GuaranteedHpCpus, or 0 if punit is HP-ineligible
    NonHPCapacity int // allocatable non-HP CPUs (allowed ∩ punit.CPUs - GuaranteedHpCpus)
}
```

### `Punits() []PunitInfo`

Returns a snapshot of `a.punits` with capacity values filled in from `punitHPCapacity(idx)` and `punitNonHPCapacity(idx)`. Returns nil when `!a.Active()`.

### `PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error)`

1. Return error if `!a.Active()`.
2. Find punit index by (PkgID, PunitID); return error if not found.
3. Return error if `!a.hpEligiblePunit[idx]`.
4. Compute available: `punit.CPUs ∩ a.allowed ∖ held ∖ a.hpDRAUsed[idx] ∖ a.hpUsed[idx]`.
5. Return error if `available.Size() < n`.
6. Sort available and take first `n` (deterministic).
7. Add to `a.hpDRAUsed[idx]` (leave `a.hpUsed` untouched).
8. Return the selected set.

Note on `hpHintsActive()` gate: this gate applies only to the *hints* machinery (`hpInUseCpus`, `hpReserveCpus`). `PickHpCpus` does not require hints to be active; it writes `hpDRAUsed` directly, and the union with `hpUsed` in `hpInUseCpus`/`hpReserveCpus` means DRA holds always contribute to placement hints when the gate is up.

### `ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet)`

1. Find punit index by (PkgID, PunitID); return silently if not found.
2. Subtract `cpus` from `a.hpDRAUsed[idx]`; delete entry if empty.

### `hpDRAUsed` and hint machinery updates

- **`hpInUseCpus()` (pct.go:679):** currently ranges `for idx, used := range a.hpUsed` — a punit with DRA holds but no `hpUsed` entry would be skipped. Change the loop to range over `a.punits` (indices 0..len-1) and union `hpUsed[i]` and `hpDRAUsed[i]` per index. Only include the punit's CPUs in the result if the union is non-empty.
- **`hpReserveCpus()` (pct.go:738):** `hpUsed` is subtracted as a **count** from `MaxHpCpus` to compute `room` (pct.go:765-773), not excluded from `free`. The change is: at pct.go:765, replace `used := a.hpUsed[i]` with `used := a.hpUsed[i].Union(a.hpDRAUsed[i])` so DRA holds reduce reported HP room. The `excludeBln` difference then applies to the union (same treatment as existing).
- **`clearHpUsage()` (pct.go:584):** no change — intentionally does NOT touch `hpDRAUsed`.
- **`Configure()` (pct.go:133):** reset `hpDRAUsed` alongside `hpUsed` and `hpEligiblePunit`. (Outstanding DRA claims are reconciled at the resmgr layer during Reconfigure — Step 7 concern.)

### Private helpers (`pct` package only; called from `Punits()`)

```go
// punitHPCapacity returns GuaranteedHpCpus for punits[idx] if HP-eligible, 0 otherwise.
func (a *Allocator) punitHPCapacity(idx int) int

// punitNonHPCapacity returns allocatable non-HP CPU count for punits[idx].
// Applies the allowed.Size()>0 guard consistent with other allowed-consuming sites.
func (a *Allocator) punitNonHPCapacity(idx int) int
```

`punitNonHPCapacity` formula:
```
cpus = punit.CPUs
if allowed.Size() > 0 { cpus = cpus.Intersection(allowed) }
return max(0, cpus.Size() - punit.GuaranteedHpCpus)
```

## Technical Details

### New exported API

```go
type PunitInfo struct {
    PkgID, PunitID  int
    HPCapacity      int // 0 if HP-ineligible
    NonHPCapacity   int
}

func (a *Allocator) Punits() []PunitInfo
func (a *Allocator) PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error)
func (a *Allocator) ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet)
```

### State changes on `Allocator`

```go
// New field alongside hpUsed:
hpDRAUsed map[int]cpuset.CPUSet // DRA-claimed HP CPUs, keyed by punit index
```

Initialize in `Configure()`:
```go
a.hpDRAUsed = map[int]cpuset.CPUSet{}
```

### Punit lookup helper (private)

```go
// punitIdxByID returns the index in a.punits for (pkgID, punitID), or -1.
func (a *Allocator) punitIdxByID(pkgID, punitID int) int
```

## What Goes Where

**Implementation Steps (checkboxes below):** all in `pkg/resmgr/cpuclass/internal/pct/pct.go` and `pct_test.go`.

**Post-Completion:** update `docs/dra/plan.md` Step 4 with Landed pointer; move plan to `docs/plans/completed/`.

## Implementation Steps

### Task 1: Write TDD tests (all expected to fail until Task 2)

**Files:**
- Modify: `pkg/resmgr/cpuclass/internal/pct/pct_test.go`

- [ ] add `TestPunitHPCapacity` table-driven (use `&Allocator{}` for the `Active()==false` case; use `fakeSst{punits: []pctPunit{{GuaranteedHpCpus: N, ...}}}` for the per-punit cases): eligible punit returns GuaranteedHpCpus; ineligible punit returns 0; out-of-range returns 0; Active()==false returns 0
- [ ] add `TestPunitNonHPCapacity`: normal case with `allowed` intersect; all CPUs HP-guaranteed (returns 0); empty `allowed` (guard: returns full punit size minus GuaranteedHpCpus); out-of-range returns 0
- [ ] add `TestAllocatorPunits`: returns correct PunitInfo per punit (PkgID, PunitID, HPCapacity, NonHPCapacity); nil when !Active()
- [ ] add `TestPickHpCpus` table-driven: success (returns n CPUs, hpDRAUsed updated, hpUsed unchanged); n > available (error); HP-ineligible punit (error); (PkgID, PunitID) not found (error); held exclusion; Active()==false (error)
- [ ] add `TestReleaseHpCpus`: CPUs removed from hpDRAUsed; releasing CPUs not held is a no-op; out-of-range (not found) is a no-op; full-punit release deletes map entry
- [ ] add `TestHpDRAUsedIsolation`: non-HP `UseClass` call overlapping a DRA-held CPU does NOT remove it from hpDRAUsed; DRA holds remain visible in `hpInUseCpus()` after non-HP `UseClass` (use a punit whose `hpUsed` entry is empty so the test is non-vacuous)
- [ ] add `TestHpReserveRoomWithDRAHolds`: after `PickHpCpus` holds k CPUs on a punit, `hpReserveCpus` reports `room` reduced by k — specifically `room == MaxHpCpus - k - len(hpUsed[i])` (copy fixture pattern from pct_test.go:549-700)
- [ ] confirm tests fail to compile (expected — types/methods not yet defined)

### Task 2a: Additive new types, helpers, `PickHpCpus`, `ReleaseHpCpus`

**Files:**
- Modify: `pkg/resmgr/cpuclass/internal/pct/pct.go`

- [ ] add `PunitInfo` struct with `PkgID, PunitID, HPCapacity, NonHPCapacity int` (no `Idx` field)
- [ ] add `hpDRAUsed map[int]cpuset.CPUSet` field to `Allocator`; initialize to empty map in `Configure()` alongside `hpUsed`
- [ ] implement `punitIdxByID(pkgID, punitID int) int` private lookup
- [ ] implement `punitHPCapacity(idx int) int`
- [ ] implement `punitNonHPCapacity(idx int) int` with `allowed.Size() > 0` guard
- [ ] implement `Punits() []PunitInfo`
- [ ] implement `PickHpCpus(pkgID, punitID, n int, held cpuset.CPUSet) (cpuset.CPUSet, error)` — writes `hpDRAUsed`, not `hpUsed`
- [ ] implement `ReleaseHpCpus(pkgID, punitID int, cpus cpuset.CPUSet)`
- [ ] run `go test ./pkg/resmgr/cpuclass/internal/pct/...` — tests for PunitInfo, capacity helpers, Punits, PickHpCpus, ReleaseHpCpus must pass; hint-machinery tests still failing (ok)
- [ ] run `go build ./...` — must compile

### Task 2b: Update hint machinery (`hpInUseCpus`, `hpReserveCpus`) to include `hpDRAUsed`

**Files:**
- Modify: `pkg/resmgr/cpuclass/internal/pct/pct.go`

- [ ] update `hpInUseCpus()` (pct.go:679): change loop to range over `a.punits` indices; per index, union `hpUsed[i]` and `hpDRAUsed[i]`; include punit CPUs in result only if the union is non-empty
- [ ] update `hpReserveCpus()` (pct.go:765): change `used := a.hpUsed[i]` to `used := a.hpUsed[i].Union(a.hpDRAUsed[i])` before the `excludeBln` difference and `MaxHpCpus` subtraction
- [ ] run `go test ./pkg/resmgr/cpuclass/internal/pct/...` — **all** tests must pass including `TestHpDRAUsedIsolation` and `TestHpReserveRoomWithDRAHolds`; pre-existing hint tests must still pass
- [ ] run `go build ./...` — must compile

### Task 3: Verify acceptance criteria

- [ ] `go test ./pkg/resmgr/cpuclass/internal/pct/...` passes (all new + all pre-existing tests)
- [ ] `go build ./...` compiles cleanly
- [ ] `go vet ./pkg/resmgr/cpuclass/internal/pct/...` is clean
- [ ] `golangci-lint run ./pkg/resmgr/cpuclass/internal/pct/...` is clean
- [ ] `Punits()`, `PickHpCpus()`, `ReleaseHpCpus()` are exported and callable from `pkg/resmgr/cpuclass/` (`package cpuclass`) — verify `package cpuclass` compiles with a call site

### Task 4: Update documentation

- [ ] update `docs/dra/plan.md` Step 4 — add `Landed:` pointer with commit SHA(s)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Step 7 concern (not in scope here):** when `Configure()` resets `hpDRAUsed`, any outstanding DRA claims whose CPUs were tracked there silently lose their HP-hold accounting. The resmgr Reconfigure path must reconcile active DRA claims post-`Configure()` — this is a Step 7 design item (design.md §"Publish flow on Reconfigure").
