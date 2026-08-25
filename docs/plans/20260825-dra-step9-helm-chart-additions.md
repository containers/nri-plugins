# DRA Step 9 — Helm chart additions

## Overview

Step 9 of the DRA integration plan ([docs/dra/plan.md](../dra/plan.md)) is deployment-shaped
work: give the `topology-aware` Helm chart the RBAC, host mounts, and `DeviceClass` object the
already-landed DRA driver code ([pkg/resmgr/dra/](../../pkg/resmgr/dra/),
[cmd/plugins/topology-aware/policy/dra.go](../../cmd/plugins/topology-aware/policy/dra.go)) needs
to actually register with kubelet and be selectable via `ResourceClaim`s. No Go code changes —
this step only touches `deployment/helm/topology-aware/`.

This is a prerequisite for Step 10 (e2e test): the e2e test needs the `DeviceClass` and RBAC in
place to create a working `ResourceClaim` against a real cluster.

## Context (from discovery)

- **Chart layout:** `deployment/helm/topology-aware/templates/{clusterrole,daemonset,config,...}.yaml`,
  values in `values.yaml`. `values.schema.json` exists (148 lines) but has no `config` property and
  no `additionalProperties: false` anywhere — `allowPCT`, `tolerations`, etc. aren't schema-validated
  either, so adding `config.dra` is schema-safe without touching `values.schema.json`.
- **Existing conditional-mount pattern:** [daemonset.yaml](../../deployment/helm/topology-aware/templates/daemonset.yaml)
  already gates a host mount (`hostdev` → `/host/dev`) behind `.Values.allowPCT`. Follow the same
  `{{- if ... }}` block style for the new DRA mounts.
- **Single source of truth for the toggle.** `templates/config.yaml` renders the `TopologyAwarePolicy`
  CR body via `{{- toYaml .Values.config | nindent 2 }}` — whatever goes under `config:` in
  `values.yaml` becomes `spec:` in the CR verbatim. The CR already has `spec.dra.enabled` /
  `spec.dra.sharedCounters` ([pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/config.go:200-211](../../pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/config.go)),
  but `values.yaml` currently has no `dra:` block under `config:` at all. Decision: add
  `config.dra.enabled` / `config.dra.sharedCounters` to `values.yaml`, and gate the new
  RBAC/mounts/DeviceClass templates on `.Values.config.dra.enabled` — the same flag that turns on
  the policy's DRA integration, not a separate chart-level toggle. This rules out the RBAC/mounts
  being installed while the policy's own DRA integration is off (or vice versa).
- **Mount paths — correcting design.md.** [docs/dra/design.md](../dra/design.md) "Helm chart
  additions" says host `/var/run/cdi` mounts to **`/host/var/run/cdi`**. That's stale relative to
  the landed code: `pkg/resmgr/dra/cdi.go:39` (`defaultCDIDir = "/var/run/cdi"`) and the
  `k8s.io/dynamic-resource-allocation/kubeletplugin` defaults (`KubeletPluginsDir =
  "/var/lib/kubelet/plugins"`, `KubeletRegistryDir = "/var/lib/kubelet/plugins_registry"`,
  confirmed in `k8s.io/dynamic-resource-allocation@v0.36.4/kubeletplugin/draplugin.go:49,51`) are
  all **unprefixed absolute paths** used directly by the driver and the kubeletplugin library —
  there is no `--host-root` offsetting applied to DRA paths (unlike sysfs/procfs/rdt/blkio, which
  go through `opt.HostRoot`, see `pkg/resmgr/resource-manager.go:80-83`). So all three DRA mounts
  must land at the **same absolute path** in the container as on the host — no `/host` prefix.
  This plan corrects design.md's mount table as part of Task 6.
- **Mounts must be read-write, not read-only.** The insertion point in `daemonset.yaml` sits next
  to `pod-resources-socket` and `nrisockets`, both `readOnly: true` — a copy-paste of the
  neighboring block is the most likely implementation slip. `os.MkdirAll(pluginDataDir)`
  (`pkg/resmgr/dra/plugin.go:558-562`), registrar socket creation, and CDI spec writes
  (`pkg/resmgr/dra/cdi.go:64,139`) all require write access; a stray `readOnly: true` fails silently
  as "driver never registers" — exactly the failure class plan.md's Step 9 risk note warns about.
- **Reference-chart comparison owed by plan.md.** plan.md's own Step 9 risk note says to verify
  the mounts "against `dra-driver-cpu`'s Helm chart as a known-good reference" because
  kubelet-plugin socket mounts are the historical source of registration bugs, and
  `docs/dra/pr-536-analysis.md` records the in-repo precedent (notably: only `plugins`/
  `plugins_registry` mounts, no `/var/run/cdi`). Task 3 includes a checkbox to do this comparison
  and record any deviation (mount scope, `mountPropagation`, `hostPath` type) rather than
  discovering gaps only when Step 10's e2e run fails.
- **RBAC verbs — design.md's rule set is broader than what the landed code calls.** design.md's
  "Helm chart additions" section lists `resourceslices` (list/watch/create/update/delete),
  `resourceclaims`+`deviceclasses` (get), and `resourceclaims/status` (patch/update) — inherited
  from PR #536. Cross-checking against the actual call sites in `pkg/resmgr/dra/` and
  `k8s.io/dynamic-resource-allocation@v0.36.4`: the driver only calls `ResourceSlices().List/Watch/
  Create/Update/Delete` (`resourceslice/resourceslicecontroller.go`), `Nodes().Get` (already
  covered by the existing `nodes` rule), and `ResourceClaims(ns).Get` (`kubeletplugin/draplugin.go`).
  There is no `DeviceClasses()` read and no `resourceclaims/status` write anywhere in the driver
  path — granting `resourceclaims/status: patch,update` cluster-wide would be an unused,
  privilege-escalation-relevant surface. Task 2 drops those two rules rather than copying
  design.md's table verbatim; design.md's rule table is corrected to match in Task 6. Existing
  `clusterrole.yaml` already has a `nodes` / `nodes/status` / `noderesourcetopologies` pattern to
  follow for style.
- **DeviceClass scope:** base class only (`nri.topology-aware.cpu`), gated on
  `.Values.config.dra.enabled`. Per-cpuClass shortcut `DeviceClass` generation (design.md's
  optional second tier) is deferred — it would need the chart to duplicate cpuClass names that
  already live in the `TopologyAwarePolicy` CR's `config.cpuClasses`, a second source of truth with
  no concrete consumer yet.
- **CI:** `.github/workflows/helm-lint.yaml` runs `helm lint deployment/helm/*` on any PR touching
  `deployment/helm/**`. No `helm-unittest` or snapshot-test framework is present in this repo — chart
  verification in this repo is `helm lint` + `helm template` (manual/CI), not a Go test suite.
- **User-facing docs are currently unowned by any step.** `docs/resource-policy/policy/topology-aware.md`
  has no DRA content, and plan.md's steps end at Step 10 (e2e) with no step assigned to write an
  end-user "how to enable DRA" section. The repo's precedent for a comparable opt-in flag
  (`allowPCT`) is a values.yaml comment block plus README/doc coverage. Task 6 picks this up so the
  gap doesn't fall through silently once the chart is installable.

## Development Approach

- **Testing approach:** Regular — write each template change, then verify with `helm lint` and
  `helm template` (both with `config.dra.enabled=false` — the default, must be a no-op — and
  `=true`), grepping the rendered output for the expected resources/mounts/rules. No Go code in
  this step, so no `go test`.
- Complete each task fully (template change + rendered-output check) before moving to the next.
- Keep `config.dra.enabled: false` as the chart default so existing deployments are unaffected.
- Update this plan file if scope changes during implementation.

## Testing Strategy

- Every task's verification step is a `helm template deployment/helm/topology-aware` run (default
  values, then `--set config.dra.enabled=true`), asserting via `grep`/manual inspection that the
  new resources/blocks appear only when enabled.
- `helm lint deployment/helm/topology-aware` must pass after every task (matches what CI runs).
- No e2e cluster test here — that's Step 10, out of scope for this plan.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.

## Solution Overview

Four small, independent template/value changes, each independently `helm template`-able:

1. `values.yaml` gets `config.dra.{enabled,sharedCounters}` (default `false`) — the single toggle.
2. `clusterrole.yaml` gets two DRA RBAC rule blocks (re-derived from actual call sites, not
   design.md's broader table), gated on that toggle.
3. `daemonset.yaml` gets the three DRA host mounts, gated on that toggle, at identical host/container
   paths (no `/host` prefix), read-write.
4. A new `deviceclass.yaml` template renders the base `DeviceClass`, gated on that toggle.

`docs/dra/design.md`'s mount table and RBAC rule table get corrected to match (Task 6).

## Technical Details

**values.yaml addition (under `config:`):**
```yaml
  dra:
    enabled: false
    sharedCounters: false
```

**clusterrole.yaml addition** (inside `rules:`, gated — verbs re-derived from actual call sites in
`pkg/resmgr/dra/` and `k8s.io/dynamic-resource-allocation`, dropping design.md's unused
`deviceclasses: get` and `resourceclaims/status: patch,update`):
```yaml
{{- if (.Values.config.dra).enabled }}
- apiGroups:
  - resource.k8s.io
  resources:
  - resourceslices
  verbs:
  - list
  - watch
  - create
  - update
  - delete
- apiGroups:
  - resource.k8s.io
  resources:
  - resourceclaims
  verbs:
  - get
{{- end }}
```
Note the `(.Values.config.dra).enabled` form, not `.Values.config.dra.enabled` — the latter panics
the whole chart render if `config.dra` is ever `null` (e.g. an overlay that sets `dra: null`
instead of omitting it). Same form is used in every gated template below.

**daemonset.yaml additions** (mirrors the existing `allowPCT`/`hostdev` conditional-mount pattern):
- volumeMounts (container): `/var/lib/kubelet/plugins`, `/var/lib/kubelet/plugins_registry`,
  `/var/run/cdi` — each mounted at the identical path, gated on `(.Values.config.dra).enabled`, and
  **not** `readOnly` (the driver writes plugin/registrar sockets and CDI specs there).
- volumes (pod spec): matching `hostPath` entries, `type: DirectoryOrCreate` (mirrors the existing
  `nrisockets`/`resource-policydata` volumes, which also use `DirectoryOrCreate` for paths that may
  not pre-exist).

**deviceclass.yaml (new file — not `native-cpu-device-class.yaml`; every existing template
filename in this chart is a single lowercase word, e.g. `clusterrole.yaml`, `serviceaccount.yaml`):**
```yaml
{{- if (.Values.config.dra).enabled }}
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: nri.topology-aware.cpu
  labels:
    {{- include "nri-plugin.labels" . | nindent 4 }}
spec:
  selectors:
    - cel:
        expression: device.driver == "nri.topology-aware.cpu"
{{- end }}
```
(Driver name matches `DRADriverName` in
[cmd/plugins/topology-aware/policy/dra_adapter.go:27](../../cmd/plugins/topology-aware/policy/dra_adapter.go) —
kept as a literal here since Helm templates can't reference Go constants; a comment ties them
together. `resource.k8s.io/v1` requires Kubernetes 1.34+; note this next to `config.dra.enabled` in
values.yaml since the chart declares no `kubeVersion` constraint.)

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): the four template/value changes above, each verified
  with `helm lint`/`helm template`, plus the design.md corrections and user-facing docs (Task 6).
- **Post-Completion**: deploying to a real KEP-5075/5517-enabled cluster to confirm the
  `ResourceSlice`/`DeviceClass` actually resolve end-to-end — that's Step 10's e2e test, not manual
  verification owed by this plan.

## Implementation Steps

### Task 1: Add `config.dra` toggle to values.yaml

**Files:**
- Modify: `deployment/helm/topology-aware/values.yaml`

- [x] add `dra: { enabled: false, sharedCounters: false }` under the `config:` block in
      `deployment/helm/topology-aware/values.yaml`, with a short comment pointing at
      `TopologyAwareDRA` (`pkg/apis/config/v1alpha1/resmgr/policy/topologyaware/config.go`) as the
      field's meaning, and noting that `resource.k8s.io/v1` (used by the DeviceClass in Task 4)
      requires Kubernetes 1.34+
- [x] verify `helm template deployment/helm/topology-aware` renders `spec.dra.enabled: false` in the
      `TopologyAwarePolicy` CR by default (via `templates/config.yaml`'s `toYaml .Values.config`)
- [x] verify `helm template deployment/helm/topology-aware --set config.dra.enabled=true` renders
      `spec.dra.enabled: true`
- [x] run `helm lint deployment/helm/topology-aware` — must pass before task 2

### Task 2: Add DRA RBAC rules to clusterrole.yaml

**Files:**
- Modify: `deployment/helm/topology-aware/templates/clusterrole.yaml`

- [x] cross-check design.md's proposed RBAC rule set against actual call sites in
      `pkg/resmgr/dra/` and the vendored `k8s.io/dynamic-resource-allocation` — confirm
      `deviceclasses: get` and `resourceclaims/status: patch,update` have no call site and are
      safe to drop (see Context note)
- [x] add the two remaining DRA rule blocks (`resourceslices` CRUD; `resourceclaims: get`) to
      `deployment/helm/topology-aware/templates/clusterrole.yaml`, wrapped in
      `{{- if (.Values.config.dra).enabled }} ... {{- end }}` (nil-safe form — see Technical Details)
- [x] verify `helm template ... | grep -A2 resourceslices` is absent with default values
- [x] verify `helm template ... --set config.dra.enabled=true | grep resourceslices` shows the new
      rules with the correct verbs, and that no `deviceclasses` or `resourceclaims/status` rule is
      present
- [x] verify `helm template ... --set config.dra=null` still renders successfully (does not panic
      the whole chart)
- [x] run `helm lint deployment/helm/topology-aware` — must pass before task 3

### Task 3: Add DRA host mounts to daemonset.yaml

**Files:**
- Modify: `deployment/helm/topology-aware/templates/daemonset.yaml`

- [x] compare against `dra-driver-cpu`'s Helm chart and PR #536's daemonset diff
      (`docs/dra/pr-536-analysis.md`) for mount scope/`mountPropagation`/`hostPath` type; record
      any deviation (e.g. mount scope) in this plan before implementing.
      **Finding** (corrected — an earlier pass of this note claimed the `dra-driver-cpu` chart was
      unavailable and compared only against `docs/dra/pr-536-analysis.md`'s written record; the
      reference chart in fact exists locally at
      `/home/ed/git/dra-driver-cpu/deployment/helm/dra-driver-cpu` and has now been diffed
      directly): its `templates/daemonset.yaml` mounts four `hostPath` volumes — `device-plugin`
      (kubelet plugins dir), `plugin-registry` (kubelet plugins_registry dir), `nri-plugin`
      (`/var/run/nri`, not applicable here — this chart already mounts the NRI socket via its own
      `nrisockets` volume elsewhere in the file), and `cdi-dir` (`/var/run/cdi`). None of the four
      are `readOnly`, matching this chart's three DRA mounts. Only `cdi-dir` sets an explicit
      `hostPath.type: DirectoryOrCreate`; `device-plugin`/`plugin-registry` set no `type` at all
      (defaults to `""`, i.e. no existence check) — this chart uses `DirectoryOrCreate` on all
      three DRA mounts uniformly instead, matching the existing `nrisockets`/`resource-policydata`
      volume style in this same file; no functional difference, since `DirectoryOrCreate` is a
      superset (creates the dir if kubelet hasn't yet). No `mountPropagation` override on any
      mount in either chart. PR #536's Helm chart mounted only `/var/lib/kubelet/plugins` and
      `.../plugins_registry` (line 16 of pr-536-analysis.md), no `/var/run/cdi` mount. This plan's
      third mount (`/var/run/cdi`) is a deliberate addition beyond that PR #536 precedent (and
      matches `dra-driver-cpu`'s `cdi-dir` mount), required because the landed driver code writes
      CDI specs there (`pkg/resmgr/dra/cdi.go`) — PR #536's driver code already wrote CDI specs to
      the identical `/var/run/cdi` path (`pkg/resmgr/dra.go`'s `writeCDISpecFile`, per
      pr-536-analysis.md:12), it just never mounted that host path into the daemonset container,
      so those writes landed in the container's own ephemeral filesystem rather than the host's.
      Separately, `dra-driver-cpu`'s `templates/clusterrole.yaml` grants two rules this chart does
      not: `pods: get/list/watch`, and `resourceclaims/driver` (scoped to `resourceNames:
      [dra.cpu]`) with verbs `associated-node:patch`/`associated-node:update` (a device-binding-
      conditions-related subresource). Neither has a call site anywhere in `pkg/resmgr/dra/` or
      `cmd/plugins/topology-aware/policy/dra*.go` (no `Pods()`/`CoreV1().Pods` call, no
      binding-conditions code), so — consistent with Task 2's drop-unused-rules rationale for
      `deviceclasses`/`resourceclaims/status` — this chart does not add them either. No
      `mountPropagation` or `hostPath.type` deviation with functional impact was found.
- [x] add gated `volumeMounts` entries for `/var/lib/kubelet/plugins`,
      `/var/lib/kubelet/plugins_registry`, `/var/run/cdi` (identical host/container path, no
      `/host` prefix — see Context note on why) inside the container's `volumeMounts:`, following
      the existing `{{- if .Values.allowPCT }}` block style, gated on `(.Values.config.dra).enabled`
      (nil-safe form)
- [x] explicitly do **not** set `readOnly: true` on any of the three (unlike the adjacent
      `pod-resources-socket`/`nrisockets` mounts) — the driver needs write access for sockets and
      CDI specs
- [x] add the matching `volumes:` `hostPath` entries (`type: DirectoryOrCreate`), gated the same way
- [x] verify `helm template ...` (default values) has none of the three new mount paths
- [x] verify `helm template ... --set config.dra.enabled=true` shows all three mounts at identical
      host/container paths, and none has `readOnly: true`
- [x] run `helm lint deployment/helm/topology-aware` — must pass before task 4

### Task 4: Add base DeviceClass template

**Files:**
- Create: `deployment/helm/topology-aware/templates/deviceclass.yaml`

- [x] create `deployment/helm/topology-aware/templates/deviceclass.yaml` rendering the base
      `DeviceClass nri.topology-aware.cpu`, gated on `(.Values.config.dra).enabled` (nil-safe form)
- [x] verify `helm template ...` (default values) has no `DeviceClass` object
- [x] verify `helm template ... --set config.dra.enabled=true` renders the `DeviceClass` with
      `device.driver == "nri.topology-aware.cpu"`
- [x] run `helm lint deployment/helm/topology-aware` — must pass before task 5

### Task 5: Verify acceptance criteria

- [x] verify all four Helm chart additions from the Overview are present and each individually
      gated on `(.Values.config.dra).enabled`
- [x] verify `helm template deployment/helm/topology-aware` with default values renders no DRA
      RBAC rules, no DRA mounts, and no `DeviceClass` — i.e. functionally a no-op for existing
      deployments. Note it is not byte-identical to pre-Task-1 output: `spec.dra: {enabled: false,
      sharedCounters: false}` now appears in the rendered `TopologyAwarePolicy` CR (inert, since
      `DRAEnabled()` is false either way). Diff the full default-values render against a
      pre-change checkout to confirm no other whitespace/indentation regression crept into
      `daemonset.yaml`
- [x] verify `helm template deployment/helm/topology-aware --set config.dra.enabled=true` renders
      all four additions together in one pass
- [x] run `helm lint deployment/helm/*` (matches `.github/workflows/helm-lint.yaml` exactly)

### Task 6: [Final] Update documentation and plan.md

**Files:**
- Modify: `docs/dra/plan.md`
- Modify: `docs/dra/design.md`
- Modify: `docs/resource-policy/policy/topology-aware.md`

- [x] update design.md's "Helm chart additions" mount table: change
      `/var/run/cdi → /host/var/run/cdi` to `/var/run/cdi → /var/run/cdi` (identical path), add a
      one-line note that none of the three DRA mounts go through the `--host-root`/`/host` prefix
      used elsewhere in the daemonset, unlike sysfs/procfs, and drop `deviceclasses`/
      `resourceclaims/status` from the RBAC rule table (or note why they're kept, if Task 2's
      cross-check finds a reason not surfaced during planning)
- [x] add a "Dynamic Resource Allocation" section to `docs/resource-policy/policy/topology-aware.md`:
      how to enable (`--set config.dra.enabled=true`), the KEP-5075/KEP-5517 + Kubernetes 1.34+
      prerequisites, the `nri.topology-aware.cpu` DeviceClass name, and a minimal `ResourceClaim`
      example (reuse the Step 10 snippet from `docs/dra/plan.md`'s Step 10 section)
- [x] no `deployment/helm/topology-aware/README.md` change needed for `config.dra.enabled` itself:
      verified neither this chart's nor the balloons chart's README gives `allowPCT` its own row
      (it's an equally impactful boolean, present in topology-aware's own `values.yaml`, and
      absent from both README value tables — mentioned only inline in balloons' `openshift.grant-scc`
      row) — the established convention is that individual `config.*` flags are covered by the
      generic `config` row, not enumerated. `config.dra` follows the same convention.
- [x] add a "Landed" line to Step 9 in `docs/dra/plan.md` (commit range, and any implementation
      deviations from this plan — e.g. if the gating value ended up different from
      `.Values.config.dra.enabled`, or if the RBAC/mount comparisons in Tasks 2-3 surfaced
      additional rules/mounts)
- [x] move this plan to `docs/plans/completed/` — not performed here per harness instruction: the
      plan file must stay at its current path until all review/finalize phases complete; the
      orchestrator performs the actual move afterward.

## Post-Completion

**External system updates:**
- None required by this step alone. Step 10 (e2e test) will exercise the RBAC/mounts/DeviceClass
  added here against a real KEP-5075/KEP-5517-enabled cluster — that verification is out of scope
  here and belongs to Step 10's own plan.

**Manual verification** (optional, not required to close this plan):
- On a real cluster with the DRA feature gates on, `helm install` with `config.dra.enabled=true`
  and confirm the daemonset pod actually registers the kubelet plugin (no dedicated automation for
  this in-repo; Step 10's e2e test is the automated version of this check).
