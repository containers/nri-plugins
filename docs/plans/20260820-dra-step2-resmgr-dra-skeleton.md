# DRA v1 step 2 — introduce `pkg/resmgr/dra/` package skeleton

## Overview

Creates the `pkg/resmgr/dra/` package as an empty-but-compilable structural placeholder. Its purpose is to physically separate the shared DRA kubelet-plugin code from any policy binary, enforcing the code-sharing contract from [design.md resolved decision 6](../dra/design.md#resolved-decisions). Once this skeleton exists, subsequent plan steps can only add to this package rather than scattering DRA logic into policy-specific locations.

No functional DRA code is written in this step. The package exposes a `Plugin` struct with a `New()` constructor that returns `ErrNotImplemented`, and a `Deps` struct (empty, fields arrive in Step 6) in `deps.go`.

**Companion docs:** [docs/dra/design.md](../dra/design.md) (overall DRA design, step 6 "Where the code lives"), [docs/dra/plan.md](../dra/plan.md) Step 2.

Note: `docs/dra/plan.md` Step 2 lists only `doc.go` and `plugin.go`. This plan adds `deps.go` so the constructor signature (`New(driverName string, deps Deps)`) is stable from the start and import boundaries are checkable immediately. The deviation is intentional and low-risk.

## Context (from discovery)

- **Target package:** `pkg/resmgr/dra/` — does not yet exist. Lives alongside `pkg/resmgr/cache/`, `pkg/resmgr/cpuclass/`, etc.
- **Policy Backend interface:** `pkg/resmgr/policy/policy.go:94` — `AllocateClaim`/`ReleaseClaim` stubs will come in Step 8; not in scope here.
- **Existing client package:** `pkg/kubernetes/client/` (created in Step 1) — available as a dep but not wired yet.
- **Import constraint (design.md resolved decision 6):** `pkg/resmgr/dra/` must never import from `cmd/plugins/*`.
- **No existing `pkg/resmgr/dra/` files** — clean slate.
- **License headers:** every `.go` file in the repo carries the `Copyright The NRI Plugins Authors.` Apache-2.0 header. All four files must include it.

## Development Approach

- **Testing approach:** TDD — tests first.
- Complete each task fully before moving to the next.
- **CRITICAL: every task MUST include new/updated tests.**
- **CRITICAL: all tests must pass before starting next task.**
- **CRITICAL: update this plan file when scope changes during implementation.**
- Maintain backward compatibility: no existing code is modified in this step.

## Testing Strategy

- **Unit tests:** one test file (`plugin_test.go`) in `package dra` (internal) covering:
  - `New()` returns `(nil, errNotImplemented)` using `errors.Is`.
  - `TestNoCmdPluginsImport`: runs `go list -deps ./...` (from the package dir) via `os/exec`, fails if any line contains the module prefix `github.com/containers/nri-plugins/cmd/`. Uses `exec.LookPath("go")` → `t.Skip` if the toolchain is absent, so `go test -c` doesn't hard-fail.
- **Rationale for import-boundary test:** a direct import cycle (`cmd/plugins -> pkg/resmgr/dra -> cmd/plugins`) would be caught at compile time once Step 8 adds the policy wiring. The test's real value is guarding the pre-Step-8 window and catching any future subpackage added carelessly. Alternative (depguard linter rule) would require introducing a repo-wide `.golangci.yml` which doesn't exist today — too much for a structural PR.
- **Build check:** `go build ./pkg/resmgr/dra/...` and `go build ./...`.
- No e2e tests — structural package, no runtime behaviour.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.

## Solution Overview

Three production files + one test file:

1. **`doc.go`** — package-level comment establishing identity and the import-boundary contract.
2. **`deps.go`** — `Deps` struct, empty for now. No interface types defined in this step — those arrive in Step 6. Defined here solely to stabilise the `New()` signature.
3. **`plugin.go`** — `Plugin` struct + `New(driverName string, deps Deps) (*Plugin, error)` returning `errNotImplemented`. Unexported sentinel `errNotImplemented`; exposed via `errors.Is` match only — no need to export it since callers only need to detect the "not implemented" condition, not name the error.
4. **`plugin_test.go`** — `package dra` (internal) tests.

## Technical Details

### Types

```go
// plugin.go
var errNotImplemented = errors.New("dra plugin: not yet implemented")

type Plugin struct{}

func New(driverName string, deps Deps) (*Plugin, error) {
    return nil, errNotImplemented
}
```

```go
// deps.go
// Deps holds the dependencies a policy binary must supply when constructing
// a Plugin. Fields are intentionally absent in this step — they are added
// in Step 6 when the kubelet-plugin wiring is implemented.
type Deps struct{}
```

### Import boundary rule

`pkg/resmgr/dra` must only import from:
- standard library
- `pkg/resmgr/*` and other `github.com/containers/nri-plugins/pkg/*` as needed in later steps
- `k8s.io/*` as needed in later steps
- **Never** from `github.com/containers/nri-plugins/cmd/`

## What Goes Where

**Implementation Steps (checkboxes below):** all code and tests are in this repo.

**Post-Completion:** update `docs/dra/plan.md` Step 2 with a "Landed:" pointer.

## Implementation Steps

### Task 1: Write tests for the package skeleton (TDD)

**Files:**
- Create: `pkg/resmgr/dra/plugin_test.go`

- [ ] create `pkg/resmgr/dra/plugin_test.go` with Apache-2.0 header, in `package dra`
- [ ] write `TestNew_ReturnsNotImplemented` — calls `New("driver", Deps{})`, asserts `err != nil` and `errors.Is(err, errNotImplemented)`
- [ ] write `TestNoCmdPluginsImport` — uses `exec.LookPath("go")` (skip if absent), runs `go list -deps ./...` from the package directory, fails if any output line has prefix `github.com/containers/nri-plugins/cmd/`
- [ ] confirm the file fails to compile (expected — package doesn't exist yet; the compile error confirms the TDD red step)

### Task 2: Create the package skeleton

**Files:**
- Create: `pkg/resmgr/dra/doc.go`
- Create: `pkg/resmgr/dra/deps.go`
- Create: `pkg/resmgr/dra/plugin.go`

- [ ] create `doc.go` with Apache-2.0 header and package comment: "Package dra provides a policy-agnostic DRA kubelet plugin used by nri-plugins policies. This package must never import from github.com/containers/nri-plugins/cmd/ — see docs/dra/design.md resolved decision 6."
- [ ] create `deps.go` with Apache-2.0 header and `type Deps struct{}` (comment: fields added in Step 6)
- [ ] create `plugin.go` with Apache-2.0 header defining `errNotImplemented`, `Plugin`, and `New`
- [ ] run `go build ./pkg/resmgr/dra/...` — must compile
- [ ] run `go test ./pkg/resmgr/dra/...` — both tests must pass
- [ ] run `go build ./...` — full build must still compile

### Task 3: Verify acceptance criteria

- [ ] `go test ./pkg/resmgr/dra/...` passes — including `TestNoCmdPluginsImport`
- [ ] `go build ./...` compiles cleanly
- [ ] `go vet ./pkg/resmgr/dra/...` is clean
- [ ] `golangci-lint run ./pkg/resmgr/dra/...` is clean (`$(go env GOPATH)/bin/golangci-lint`)
- [ ] `make test` picks up the new package (ginkgo with `TEST_PKGS := .` covers all packages recursively)
- [ ] verify `pkg/resmgr/dra/` contains exactly `doc.go`, `deps.go`, `plugin.go`, `plugin_test.go`

### Task 4: Update documentation

- [ ] update `docs/dra/plan.md` Step 2 — add `Landed:` pointer with commit SHA(s)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:** `go list -deps github.com/containers/nri-plugins/pkg/resmgr/dra | grep 'containers/nri-plugins/cmd'` should return nothing.
