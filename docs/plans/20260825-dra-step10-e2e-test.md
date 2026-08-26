# DRA step 10 — e2e test

## Overview

Add an end-to-end test proving the full DRA path for the topology-aware policy: a
`ResourceClaim` selecting an HP (Speed Select high-priority) `cpuClass` by its
published `nri/pctPriority` attribute, a pod consuming that claim, and assertions
that the claimed CPUs actually get associated to the HP CLOS (in the SST mock log)
and that `node.status.allocatable.cpu` is deducted accordingly.

This is Step 10 of the DRA integration plan ([docs/dra/plan.md](../dra/plan.md)),
the last step, following the already-landed Steps 1–9. It requires the
`DRAConsumableCapacity` ([KEP-5075](https://github.com/kubernetes/enhancements/issues/5075))
and `DRANodeAllocatableResources` ([KEP-5517](https://github.com/kubernetes/enhancements/issues/5517))
alpha feature gates to be enabled on the API server, scheduler, and kubelet — these
are the actual `--feature-gates` names; the `AllowMultipleAllocations`/
`NodeAllocatableResourceMappings` names that appear later in this plan are the
*API field names* those gates unlock, not gate names themselves (confirmed against
this repo's own `docs/dra/landscape.md:7` and
`docs/resource-policy/policy/topology-aware.md:749,754`, which already had this
right). **The
e2e harness's own VM cluster does not currently enable either gate** — it runs a
bare `kubeadm init` with no feature-gate plumbing at all (see Task 1) — so a
prerequisite chunk of this plan is teaching the harness to turn them on, guarded by
a variable so every other e2e test keeps running against an ungated cluster.

## Context (from discovery)

- **Files/components involved:**
  - `test/e2e/playbook/provision.yaml:506` — the Ansible task that runs `kubeadm init --pod-network-cidr="{{ network }}"` with no `--config`, no `extraArgs`, no feature gates. This is the actual blocker for running this test at all.
  - `test/e2e/files/Vagrantfile.in:107-110` — `ansible.extra_vars` is where `network` is threaded into `provision.yaml`; a new var for feature gates follows the same path.
  - `test/e2e/policies.test-suite/topology-aware/n4c16/test19-cpuclass/code.var.sh` — the direct pattern reference: SST/cpufreq mock env vars, `DEBUG_LOGGERS="agent cpu cpuclass"`, `assert-cpu-clos`/`wait-assert-log-contains`/`fetch-log` helpers (test-local, not shared — duplicated similarly in `balloons/n4c16/test19-pct/code.var.sh`), `helm-launch`/`helm-terminate` lifecycle.
  - `test/e2e/policies.test-suite/topology-aware/helm-config.yaml.in` — shared Helm values template used by every n4c16 (and n4c128) topology-aware test via `instantiate`. Currently has no `dra:` passthrough.
  - `deployment/helm/topology-aware/values.yaml:74` — `config.dra.{enabled,sharedCounters}` is nested under `config:`, not top-level.
  - `test/e2e/run_tests.sh:284-285` — a run is only classified SKIP if its output contains the literal string `Test verdict: SKIP`; precedent: `test/e2e/policies.test-suite/topology-aware/s8c4k/test01-sparse-4kcpus/code.var.sh:62` (`echo "Test verdict: SKIP (buggy runc)"; exit 0`). Anything else (e.g. a plain `exit 0`) is reported as PASS.
  - `pkg/resmgr/cpuclass/internal/pct/pct_sst_mock.go:192` — `ConfigureClos` logs at daemon-startup time, when CLOS frequency bounds are programmed, **not** when CPUs are associated to a CLOS. The per-CPU association line is `pkg/resmgr/cpuclass/internal/pct/pct.go:786`: `"pct: associated cpus %s to CLOS %d"` — this is what test19-cpuclass's `assert-cpu-clos` actually matches, and what this plan's headline assertion must use too (`docs/dra/plan.md`'s Step 10 bullet currently says `ConfigureClos.*ClosID:<hp-clos-id>`, which is wrong for the same reason and needs the same fix in Task 6).
  - `pkg/resmgr/cpuclass/internal/pct/pct.go:30` / `pkg/resmgr/cpuclass/cpuclass.go:42` — both `pct` and `cpuclass` packages log via the `cpuclass` logger name; `DEBUG_LOGGERS` must include `cpuclass` or these lines never appear.
  - `pkg/resmgr/cpuclass/dra.go:246-285` — confirms the landed device shape: `nri.topology-aware.cpu` DeviceClass, `nri/pctPriority` attribute for PCT classes, `nri/cpus` capacity with `RequestPolicy`, `AllowMultipleAllocations: true`, `NodeAllocatableResourceMappings` set unconditionally — so a claim can read these back as a gate-detection signal (see Task 3).
  - `pkg/resmgr/dra/cdi.go:93-95` — CDI spec still writes `NRI_CLASS`/`NRI_CPU<N>` env vars despite Step 8's device-name-based claim identification, so asserting on them is still valid.
  - `test/e2e/files/guaranteed.yaml.in` — the generic pod template used by `create guaranteed`, shared by ~23 other test dirs; has no `resourceClaims`/`resources.claims` support. **Not modified** — see Solution Overview for why manifests are inlined instead.
- **Related patterns found:**
  - `docs/dra/plan.md`'s Step 10 section already made an explicit decision: a single `code.var.sh` file, no separate static YAML manifests (recorded in its 2026-08-24 change-log entry). This plan follows that — see Solution Overview.
  - Config-driven conditionals in `helm-config.yaml.in` follow a `$([ -n "$VAR" ] && echo "...")` shape, one block per optional field.
- **Dependencies identified:**
  - Steps 1–9 are all landed on branch `DRA` (see completed plans under `docs/plans/completed/dra-step{1..9}*`). This step adds no production code — only e2e test assets, one shared-template line, and one provisioning change.
  - A KEP-5075+5517-gated cluster is required to actually run and observe pass/fail. None exists yet — Task 1 creates one by extending the harness's own VM provisioning, variable-guarded.

## Development Approach

- **Testing approach:** Regular (write the infra/script, then validate by running it) — e2e shell/Ansible assets are validated by execution, not unit tests; TDD doesn't map to this kind of infra code.
- Complete each task fully (including running it against the harness's VM) before moving to the next.
- Keep the provisioning and shared-template changes minimal, additive, and off-by-default — no other e2e test's cluster or config may change when the new variable is unset.
- **CRITICAL: update this plan file when scope changes during implementation.**
- "run tests" in each task's checklist means: rebuild/reprovision the VM if Task 1 changed, then run the new test directory specifically (`test/e2e/run_tests.sh` pointed at it) — not the full suite — except where a task explicitly says otherwise (Task 6 runs the neighboring `test19-cpuclass` too, once, to check for regressions from the shared-template change).

## Testing Strategy

- **e2e test**: this plan's entire deliverable. No unit tests apply — Steps 1–9 already carry their own unit/integration coverage; this step only adds/exercises the e2e layer, plus a one-time provisioning capability the harness lacked.
- Validate the skip path by asserting the harness actually reports `SKIP` (via `run_tests.sh`'s `Test verdict: SKIP` string match), not merely that the script exits 0 — a plain `exit 0` is silently reported as PASS.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep this plan in sync with actual work done; update `docs/dra/plan.md`'s Step 10 "Landed" line and Change log once this lands, per this repo's `docs/dra/*.md` maintenance convention (see project CLAUDE.md)

## Solution Overview

Three-part change:

1. **Provision the feature gates** — extend `test/e2e/playbook/provision.yaml`'s
   `kubeadm init` step to optionally take a `--config` file carrying
   `ClusterConfiguration.apiServer.extraArgs["feature-gates"]` (+ scheduler) and a
   companion `KubeletConfiguration.featureGates` map, gated by a new
   `k8s_feature_gates` Ansible var threaded through `Vagrantfile.in`'s
   `ansible.extra_vars` the same way `network` already is. Default: var unset,
   `kubeadm init` behaves exactly as today.
   (Threading pattern: `Vagrantfile.in:20-21` defines `K8S_RELEASE = "#{ENV['k8s_release']}"` and passes it as `k8s_release: K8S_RELEASE` in `ansible.extra_vars` — `k8s_feature_gates` follows that exact shape, not the hardcoded `network: "10.217.0.0/16"` literal a few lines below it.)
2. **Shared template extension** — add a minimal `dra: enabled: ...` passthrough
   (nested under `config:`, matching `values.yaml`) to
   `test/e2e/policies.test-suite/topology-aware/helm-config.yaml.in`, driven by a
   `DRA_ENABLED` shell var. No `sharedCounters` passthrough — nothing needs it
   (Model C / KEP-5941 is out of v1 scope per `docs/dra/plan.md`'s "Not part of
   v1"); add it later if a test actually requires it.
3. **New test directory** `test/e2e/policies.test-suite/topology-aware/n4c16/test20-dra/`
   containing a **single** `code.var.sh` (per plan.md's recorded decision — no
   separate `resourceclaim.yaml.in`/`pod.yaml.in`). The `ResourceClaim` and pod
   manifests are written inline as heredocs and applied with
   `vm-command "cat <<'EOF' | kubectl apply -f -"` (or copied to the VM and
   `kubectl create -f`'d, whichever proves less fiddly with the harness's
   `vm-command` quoting — decided in Task 4), sidestepping `create()`'s
   `wait=Ready`-by-default behavior, which doesn't apply to a `ResourceClaim`
   anyway.

## Technical Details

- **Feature-gate probe**, ranked by preference (decide in Task 3, confirm against
  the actual VM). Because it must publish real devices to be observable, the probe
  needs the SST/cpufreq mock and a real `cpuClasses` config already active — Task 3
  therefore launches with the full mock+class config up front (moved there from
  Task 5; see Task 3's Files/checklist), not a bare `DRA_ENABLED=true` launch with
  no devices to publish:
  1. **(primary) ResourceSlice round-trip read-back.** After `helm-launch`, read
     the driver's own published `ResourceSlice` (`kubectl get resourceslices
     -o json`) and check whether `spec.devices[].allowMultipleAllocations`
     (KEP-5075) and `spec.devices[].nodeAllocatableResourceMappings` (KEP-5517)
     survived the round trip — `pkg/resmgr/cpuclass/dra.go:279-281` sets both
     unconditionally, so if the API server's alpha gates are off, the server
     strips them silently on write and they'll be absent on read. (Not
     `spec.sharedCounters` — the plugin never sets that field; Model C/KEP-5941 is
     out of v1 scope.) With no `CPU_CLASSES`/SST mock active, `DRADevices()`
     returns an empty device list (`pkg/resmgr/cpuclass/dra.go:304`,
     `h.pct == nil` or no punits) and there is nothing to read back — this is why
     the mock+class launch must precede the probe.
     Because this requires the plugin to already be running, the probe happens
     **after** `helm-launch`, not before — so the skip path must call
     `helm-terminate` before `exit`.
  2. (fallback) `kubectl get --raw /metrics` on the API server, grep for
     `kubernetes_feature_enabled{name="DRAConsumableCapacity"...} 1` (and the
     `DRANodeAllocatableResources` equivalent) — use the gate names here, not the
     API field names from candidate 1.
  3. (fallback) a throwaway `ResourceSlice` with the same fields, same round-trip
     check, if reading the driver's real slice proves awkward.
- **Skip path.** On missing gate(s): `helm-terminate` (clean up the plugin so the
  test doesn't leave state behind), then
  `echo "Test verdict: SKIP (KEP-5075/KEP-5517 feature gate missing)"; exit 0` —
  the exact string, matched by `run_tests.sh:284-285`. Verify by grepping the
  captured test output for `Test verdict: SKIP`, not just checking exit code.
- **Claim → pod → CDI chain being asserted:** `ResourceClaim` (selects
  `nri.topology-aware.cpu` DeviceClass, CEL `device.attributes["nri"].pctPriority
  == "high"`, `nri/cpus: "2"`) → pod's `resourceClaims`/`container.resources.claims`
  → kubelet calls driver `PrepareResourceClaims` → CDI spec with `NRI_CLASS`/
  `NRI_CPU<N>` env vars → topology-aware policy's `allocateClaim` marks those CPUs
  claimed and calls `UseClass` → `pct.go:786`'s `"associated cpus %s to CLOS %d"`
  line for the HP CLOS (CLOS 0, matching test19-cpuclass's convention where the
  HP class is CLOS 0).
- **Assertions**, in order:
  1. Launch with `DEBUG_LOGGERS="agent cpu cpuclass"` (cpuclass is required for
     both the association line and any dra-package logging; drop/extend if Task 5
     needs `dra`-logger lines too) — without this, every log-based assertion below
     times out silently.
  2. Pod reaches `Running`.
  3. Container env has `NRI_CLASS=hp-turbo` and two `NRI_CPU<N>=1` vars — checked
     via `kubectl exec <pod> -c <ctr> -- env` (an `-o json` env check does **not**
     work: CDI env vars are injected into the OCI spec at container-creation time
     by the runtime, never written back to the pod object).
  4. `assert-cpu-clos <ctr> <cpus> "CLOS 0"` (test19-cpuclass's helper, reused/
     copied verbatim) — **not** the `ConfigureClos` startup line, which would pass
     even if the claim were never allocated. `<cpus>` **must** come from the
     claimed CPU ids parsed out of the `NRI_CPU<N>` env vars (step 3) —
     **not** from `ctr-cpu-ids`/`/proc/1/status`'s `Cpus_allowed_list`. The claimed
     CPUs are removed from pool supply by `allocateClaim`
     (`cmd/plugins/topology-aware/policy/pools.go:1624-1663`,
     `Supply.ClaimCPUs` + `updateSharedAllocations(nil)`) — they are never the
     consuming container's own cpuset, so `ctr-cpu-ids` on that container reports a
     disjoint set and would never match. Because the log line formats the cpuset
     with `cpuset.CPUSet.String()` (`pct.go:786`), which collapses contiguous ids
     into ranges (e.g. CPUs 1 and 2 log as `1-2`), the env-derived ids must be
     compressed into the same range form before matching — add a
     `compress-cpulist` helper (inverse of test19's `expand-cpulist`, same
     `python3` idiom) rather than assuming individual/comma-joined ids will match.
  5. `node.status.allocatable.cpu` reduced by 2 from a pre-claim baseline captured
     before pod creation.

## What Goes Where

- **Implementation Steps** (checkboxes below): the provisioning change, the
  template edit, the new test directory's script, and running the test against
  the harness's own VM.
- **Post-Completion**: updating `docs/dra/plan.md`'s Step 10 entry with the actual
  landed commit range and any deviations, and any further CI-side wiring if this
  test should run automatically rather than only ad hoc (the harness change in
  Task 1 makes the gate available to any local run; hooking it into a CI job's
  default invocation is a separate concern).

## Implementation Steps

### Task 1: Provision `DRAConsumableCapacity`/`DRANodeAllocatableResources` gates in the e2e VM

**Files:**
- Modify: `test/e2e/playbook/provision.yaml`
- Modify: `test/e2e/files/Vagrantfile.in`
- Modify: `test/e2e/run.sh`
- Modify: `test/e2e/run_tests.sh`

**VM lifecycle note (read before starting):** the harness's vagrant dir/`Vagrantfile.erb`/`env` are generated once and reused (`test/e2e/lib/vm.bash`) — editing `Vagrantfile.in` has no effect on an already-provisioned VM, and `kubeadm init` is not safely re-runnable on a live cluster. `vm_name` is derived from `k8scri`/topology/distro only, and while `run.sh:155` honors a preset (`${vm_name:=...}`), **`run_tests.sh:225` overrides it unconditionally** (`export vm_name=$(vm-create-name ...)`, no `:=` guard) — every task in this plan runs through `run_tests.sh`, so a preset `vm_name` env var would silently be clobbered. Fix `run_tests.sh:225` to `${vm_name:-$(vm-create-name ...)}` so a caller-supplied `vm_name` sticks, then use a distinct `vm_name` (e.g. append `-gated`) whenever `k8s_feature_gates` is set, and always fully tear down and recreate (not `provision=1` re-provision) when switching a VM between gated and ungated.

- [x] add a `k8s_feature_gates` var: declare/export it in `test/e2e/run.sh` alongside the other e2e env vars (e.g. near `k8s_release`, `test/e2e/run.sh:50`) so it's echoed in the run's config banner like the others, and thread it through `Vagrantfile.in`'s `ansible.extra_vars` alongside the existing `network` var
- [x] fix `test/e2e/run_tests.sh:225` to `export vm_name=${vm_name:-$(vm-create-name "$k8scri" "$(basename "$TOPOLOGY_DIR")" ${distro})}` so a caller-supplied `vm_name` isn't clobbered (required for the gated/ungated VMs in Tasks 3/6 to coexist rather than fight over one vagrant dir) — confirm this doesn't change behavior for any existing test that doesn't preset `vm_name`
- [x] pin/record a minimum `k8s_release` for this test: `DRANodeAllocatableResources` is alpha since Kubernetes 1.36, `DRAConsumableCapacity` since 1.34, so **1.36 is the floor** — do not rely on `test/e2e/run.sh:50`'s `k8s_release=latest` default without pinning it explicitly, since "latest" drifting below 1.36 later would silently break this test in an unrelated way; confirm the installed `kube-apiserver --help`/`kubelet --help` in the VM image recognize `DRAConsumableCapacity` and `DRANodeAllocatableResources` as valid gate names (**not** `AllowMultipleAllocations`/`NodeAllocatableResources` — those are the API field names the gates unlock, not the gate names themselves) for whichever release is pinned; an unrecognized gate name is a **fatal** apiserver/kubelet start error, not a silent no-op
- [x] in `provision.yaml`, when `k8s_feature_gates` is set, write a `kubeadm` `--config` file (multi-doc `ClusterConfiguration` + `KubeletConfiguration`) setting `apiServer.extraArgs`/`scheduler.extraArgs` (list-of-`{name,value}` shape in `kubeadm.k8s.io/v1beta4`, not a map — verify against the installed kubeadm's API version) and `KubeletConfiguration.featureGates` from it; `--config` and `--pod-network-cidr` cannot be combined, so `networking.podSubnet` in the config file is mandatory, not optional, once this path is taken; when `k8s_feature_gates` is unset, keep today's exact `kubeadm init --pod-network-cidr="{{ network }}"` invocation unchanged
- [x] confirm against the actual installed `kubeadm`/`kubelet` version in the VM image which config API version and field names are current (schema has moved across k8s minors, and `ClusterConfiguration.featureGates` is a different, kubeadm-internal set of gates that will hard-error on component gate names like these) — do not assume the field names above are final without checking
- [x] provision a VM with `k8s_feature_gates` unset (default `vm_name`) and confirm cluster comes up identically to before (no regression for every other e2e test)
- [x] provision a separate, distinctly-named VM with `k8s_feature_gates="DRAConsumableCapacity=true,DRANodeAllocatableResources=true"` and confirm via `kubectl get --raw /metrics | grep kubernetes_feature_enabled` (or equivalent) that both gates report enabled on the API server, and that the kubelet accepted its `featureGates` config (check kubelet logs / `kubectl get node -o yaml` doesn't show a crash-loop)
- [x] run tests — must pass before task 2

### Task 2: Add `dra.enabled` passthrough to the shared n4c16/n4c128 helm-config template

**Files:**
- Modify: `test/e2e/policies.test-suite/topology-aware/helm-config.yaml.in`

- [x] add a conditional block directly below the existing `$([ -n "$CPU_CLASSES" ] ...)` block, copying its exact formatting (a `$(... && echo "` opener, the field on its own indented line, a closing `")` — real literal newlines inside the double-quoted string, not an escaped `\n` sequence), nested under `config:` (matching `values.yaml`'s `config.dra.enabled`) — no `sharedCounters` field (YAGNI; nothing sets it)
- [x] render `instantiate helm-config.yaml` with and without `DRA_ENABLED` set (via the same `eval "echo -e ..."` mechanism `create`/`instantiate` uses) and diff the two outputs to confirm the unset case emits nothing new
- [x] run `test19-cpuclass` once against the harness (with `k8s_feature_gates` unset, i.e. the harness's default config) to confirm the template change is a no-op for an existing, unrelated test
- [x] run tests — must pass before task 3

### Task 3: Implement the feature-gate probe and skip path in `code.var.sh`

**Files:**
- Create: `test/e2e/policies.test-suite/topology-aware/n4c16/test20-dra/code.var.sh` (initial skeleton: mock+launch + probe + skip path only; claim/pod and remaining assertions added in Task 4/5)

- [ ] re-verify `test20-dra` is still unused under `n4c16/` (the last-known-free block after `test19-cpuclass` is 20–24, before the suite jumps to `test25-irq`) — re-check for collisions immediately before creating the directory
- [ ] start `code.var.sh` with a `cleanup` call (delete pods, `helm-terminate`) before the first `helm-launch`, mirroring test19-cpuclass — this plan's tasks iterate repeatedly on the same VM, so a leftover deployment/pod from a prior failed run must not be able to interfere
- [ ] reuse test19-cpuclass's `OVERRIDE_SYS_CPUFREQ` / `OVERRIDE_SST` / `OVERRIDE_SST_STATE_DIR` mock block verbatim, and launch with `CPU_CLASSES="[ { name: hp-turbo, pctPriority: high, pctMinFreq: turbo, pctMaxFreq: turbo } ]"`, `DEBUG_LOGGERS="agent cpu cpuclass"`, `DRA_ENABLED=true` (Task 2's var), plus the existing `EXTRA_ENV_OVERRIDE_*` vars — the probe below needs real published devices to read back, which requires this full mock+class config up front, not a bare `DRA_ENABLED=true` launch (an empty `CPU_CLASSES` publishes zero devices, per Technical Details)
- [ ] launch against the distinctly-named VM provisioned with Task 1's gates on (see Task 1's VM lifecycle note)
- [ ] implement the ResourceSlice round-trip probe (Technical Details, primary candidate): read back the published `ResourceSlice`, check `spec.devices[].allowMultipleAllocations` and `spec.devices[].nodeAllocatableResourceMappings` survived
- [ ] on missing gate(s): call `helm-terminate`, then `echo "Test verdict: SKIP (KEP-5075/KEP-5517 feature gate missing)"; exit 0`
- [ ] write a negative-path check: run against the Task 1 ungated VM (default `vm_name`, `k8s_feature_gates` unset) and confirm `test/e2e/run_tests.sh`'s captured `run.sh.output` for this test contains `Test verdict: SKIP` — check the per-test output file directly, not the run summary file, since `run_tests.sh`'s SKIP branch doesn't append to the summary
- [ ] write a positive-path check: run against the Task 1 gated VM and confirm the probe passes and execution continues past it
- [ ] run tests — must pass before task 4

### Task 4: Add inline `ResourceClaim` + pod manifests to `code.var.sh`

**Files:**
- Modify: `test/e2e/policies.test-suite/topology-aware/n4c16/test20-dra/code.var.sh`

- [ ] write the `ResourceClaim` manifest inline (heredoc) per plan.md's Step 10 sketch (`apiVersion: resource.k8s.io/v1`, `spec.devices.requests[].exactly.{deviceClassName,capacity.requests,selectors}` — see `docs/dra/plan.md:299-317` for the exact shape, don't re-derive it): `deviceClassName: nri.topology-aware.cpu`, `capacity.requests.nri/cpus: "2"`, name `hp-turbo-cpus`
- [ ] selector: `device.attributes["nri"].pctPriority == "high" && device.attributes["nri"].packageID == 1` — **not** `pctPriority` alone. With test19-cpuclass's mock SST layout (package 0 = CPUs 0-7, package 1 = CPUs 8-15, `max_hp_cpus: 2` each) and `cpu0=0` being the reserved-pool CPU by convention, an unconstrained claim can resolve to package 0's HP CPUs — `PickHpCpus` (`pkg/resmgr/cpuclass/internal/pct/pct.go:611-652`) takes the first `n` CPUs from a punit without excluding the reserved pool. The KEP-5517 allocatable deduction itself is unaffected by *which* CPUs are picked (it comes from the device's `NodeAllocatableResourceMappings`/`AllocationMultiplier`, not from which physical CPUs were claimed) — the actual hazard of colliding with CPU 0 is `updateSharedAllocations(nil)` re-pinning kube-system/reserved-pool containers off it, and the reserved class's own CLOS/frequency enforcement contending with the claim's `UseClass` call on the same CPU, which would make the CLOS-association assertion (Technical Details #4) flaky. Pinning to `packageID == 1` avoids that; record the resulting claimed CPU ids (expected: two CPUs from `8-15`) so Task 5's assertions have a concrete expectation, not a guess
- [ ] write the consuming pod manifest inline (heredoc), with `spec.resourceClaims` referencing `hp-turbo-cpus` by name and a container `resources.claims` entry — do **not** extend the shared `test/e2e/files/guaranteed.yaml.in` (used by ~23 other tests) or route through `create()` (defaults to `wait=Ready`, which a `ResourceClaim` never satisfies)
- [ ] apply the claim and the pod as two **separate** `vm-command` steps (not one combined apply) — Task 5 needs to capture the node-allocatable baseline in the gap between them, before the pod exists and consumes the claim
- [ ] apply both via `vm-command "cat <<'EOF' | kubectl apply -f -\n...\nEOF"` (or copy-then-`kubectl create -f`, whichever quoting proves cleaner against `vm-command`'s escaping — settle this empirically, not in advance)
- [ ] bypassing `create()` also bypasses its image pre-pull step, so on a freshly created VM the pod's container image (`quay.io/prometheus/busybox`, matching `guaranteed.yaml.in`'s convention) won't be cached yet — either add an explicit pre-pull `vm-command` or confirm the VM has outbound network access to pull it on first use
- [ ] verify both objects create without validation errors and the pod reaches `Running`, by running against the Task 1 gated VM
- [ ] run tests — must pass before task 5

### Task 5: Complete `code.var.sh` — CLOS association, env vars, allocatable deduction, cleanup

**Files:**
- Modify: `test/e2e/policies.test-suite/topology-aware/n4c16/test20-dra/code.var.sh`

- [ ] copy `fetch-log`/`assert-log-contains`/`wait-assert-log-contains`/`assert-cpu-clos` from test19-cpuclass verbatim (test-local convention — already duplicated in `balloons/n4c16/test19-pct`, not worth extracting to a shared lib for a third copy); add a new `compress-cpulist` helper (inverse of test19's `expand-cpulist`, same `python3` idiom) — `ctr-cpu-ids`/`assert-cpu-freq` are not needed here — mock block and launch vars already added in Task 3, not repeated here
- [ ] capture pre-claim `node.status.allocatable.cpu` baseline (as a `resource.Quantity`-comparable value, e.g. via `kubectl get node -o jsonpath` millicore form, so the post-claim comparison isn't a fragile string diff of `16` vs `16000m`) before creating the pod
- [ ] assert `NRI_CLASS`/`NRI_CPU<N>` env vars via `kubectl exec <pod> -c <ctr> -- env` (not `-o json`)
- [ ] assert CLOS association via `assert-cpu-clos <ctr> "$(compress-cpulist <claimed-ids>)" "CLOS 0"` (the `pct.go:786` `"associated cpus ... to CLOS N"` line — not the mock's startup-time `ConfigureClos` line); `<claimed-ids>` come from the `NRI_CPU<N>` env vars parsed in the previous step — **not** `ctr-cpu-ids`, since the claimed CPUs are removed from pool supply, not assigned as the container's own cpuset (Technical Details #4)
- [ ] assert `node.status.allocatable.cpu` is exactly 2 less than the pre-claim baseline
- [ ] add `cleanup` (delete pod + claim, `helm-terminate`) at the end, mirroring test19-cpuclass
- [ ] run the complete test against the Task 1 gated VM; iterate until it passes end-to-end
- [ ] run tests — must pass before task 6

### Task 6: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: claim selects by `nri/pctPriority`, pod consumes it, correct CLOS-association log line asserted, allocatable deduction asserted, feature-gate skip path verified via `Test verdict: SKIP`
- [ ] run `test19-cpuclass` once more against the Task 1 gated (distinctly-named) VM to confirm no regression from Task 2's shared-template change under the gated config too, not just the ungated one already checked in Task 2
- [ ] confirm the skip path fires correctly against the Task 1 ungated VM (already covered in Task 3's negative-path check — re-run once more here as a final end-to-end confirmation, not a new mechanism)
- [ ] verify test coverage matches plan.md's Step 10 spec (no silently dropped assertions)

### Task 7: Update documentation

- [ ] fix `docs/dra/plan.md`'s Step 10 bullet: replace the `ConfigureClos.*ClosID:<hp-clos-id>` assertion example with the correct `associated cpus ... to CLOS N` form (per this plan's Technical Details / Task 5 finding)
- [ ] update `docs/dra/plan.md`'s Step 10 entry with a "Landed" line (commit range, actual test directory name if different from `test20-dra`, and any implementation deviations — e.g. actual `kubeadm` config schema used in Task 1, actual probe mechanism, actual CLOS id observed)
- [ ] add a Change log entry to `docs/dra/plan.md` noting Step 10 landed and that it required a harness provisioning change (Task 1) not originally scoped in the plan
- [ ] note in `docs/dra/plan.md`'s Cross-cutting feature-gate-probes section that the probe implemented here (Task 3) is **test-only**, separate from the still-open production `Plugin.Start` probe gap it already tracks — do not let this step's landing be mistaken for closing that gap
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Confirm the skip message is legible and actionable to an operator running this suite on a fresh checkout without having provisioned `k8s_feature_gates`.

**External system updates:**
- CI enablement: if this test should run automatically, a CI job needs to pass `k8s_feature_gates` (Task 1's new var) when provisioning its VM. This plan only makes the capability available locally/manually; wiring a CI job's defaults is a separate, explicitly out-of-scope follow-up.
