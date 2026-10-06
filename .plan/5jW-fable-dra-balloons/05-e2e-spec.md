# Step 4: e2e test specification

Read `.github/skills/run-e2e-tests/SKILL.md`, `.github/skills/coding-style/SKILL.md`
(E2E section: no sleeps, retry-until), and the reference test
`test/e2e/policies.test-suite/template/n4c16/test01-dra/`.

## Environment

- The balloons image must be built from the branch:
  `make PLUGINS=nri-resource-policy-balloons SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images`.
- Kubernetes feature gates: `DRANodeAllocatableResources=true` for the
  accounting test. `DRAConsumableCapacity` is on by default.
  Gates are part of the VM identity; use a dedicated VM so the user's
  ungated `n4c16-fedora-43-containerd` VM is left alone:

  ```
  cd test/e2e
  k8s_feature_gates=DRANodeAllocatableResources=true \
  vm_name=n4c16-dra-fedora-43-containerd \
  ./run_tests.sh policies.test-suite/balloons/n4c16/test31-dra-publish-and-claim
  ```
  The first run creates and provisions the VM (minutes). Output goes to
  `test/e2e/n4c16-dra-fedora-43-containerd/`.
- Tests that need a gate must SKIP cleanly when it is absent, like the
  template test does (print `Test verdict: SKIP (...)` and exit 0), by
  checking whether `nodeAllocatableResources` survived in the published
  ResourceSlice.
- Runtime: containerd >= 2.3.0 (passes CDI devices to NRI). Check
  `containerd --version` on the VM and SKIP with a message if older.

## Shared files in test/e2e/policies.test-suite/balloons/n4c16/

Helm values for the DRA tests, `<test>/balloons-dra.cfg` (plain helm
values, like other tests' `.cfg` files), based on the suite's
`helm-config.yaml`:

```yaml
config:
  dra:
    enabled: true
  availableResources:
    cpu: cpuset:1-15
  reservedResources:
    cpu: "1"
  pinCPU: true
  pinMemory: true
  balloonTypes:
    - name: fast
      minBalloons: 2
      maxBalloons: 2
      minCPUs: 1
      maxCPUs: 4
      allocatorPriority: high
      dra:
        deviceName: "fast-${instance}"
    - name: solo
      minBalloons: 1
      maxBalloons: 1
      minCPUs: 2
      maxCPUs: 2
      dra: {}
    - name: two-cpu
      minCPUs: 2
      maxCPUs: 2
      preferNewBalloons: true
  instrumentation:
    httpEndpoint: ":8891"
  log:
    debug:
      - policy
      - dra
      - resource-manager
    source: true
    klog:
      skip_headers: true
```

Templates (in the test directory so `create` finds them):

- `dra-claim.yaml.in`: ResourceClaimTemplate `${NAME}` with one request
  on deviceClass `balloons.nri.io`, optional CEL selector on
  `balloonType == "${BTYPE}"` (when BTYPE set) or on device name
  `device.attributes["balloons.nri.io"].instance == ${INSTANCE}` when
  given, and optional `capacity.requests.cpu: '${CPUS}'` when CPUS set.
- `dra-pod.yaml.in`: Pod `${NAME}` with `CONTCOUNT` containers
  (`${NAME}c0`, ...), each referencing claim `${CLAIM}` via
  `resources.claims` unless `NOCLAIM_CONTAINERS` lists its index; optional
  `CPUREQ`/`CPULIM` native requests; optional `POD_ANNOTATION` like
  `balloons-busybox.yaml.in`; `terminationGracePeriodSeconds: 1`; image
  `quay.io/prometheus/busybox`, command `sh -c "echo ${NAME}cN $(sleep inf)"`.
  Multiple claims per pod: allow `CLAIM2` to add a second
  `resourceClaims` entry and reference it from the containers listed in
  `CLAIM2_CONTAINERS`.

Helpers in `code.var.sh` (or a shared `dra.source.sh` next to the tests if
the framework sources `*.source.sh` from the test directory; verify in
`test/e2e/run.sh` how `*.source.sh` files are picked up):

- `dra-device FIELD [NAME]`: jq over `kubectl get resourceslices -o json`
  for driver `balloons.nri.io`.
- `dra-devices`: sorted device names.
- `check-env POD CONTAINER VAR VALUE`.
- `restart-plugin` as in the template test but for
  `ds/nri-resource-policy-balloons`.

## test31-dra-publish-and-claim (no extra gates)

1. `cleanup`; `relaunch-policy balloons "$TEST_DIR/balloons-dra.cfg"`.
2. Wait (retry-until, 30 s) until the slice has devices; verify names are
   exactly `fast-0 fast-1 solo-0`, `allowMultipleAllocations == true`
   (else SKIP as in template), `capacity.cpu.value == "4"` for fast,
   `"2"` for solo, attributes `balloonType`/`instance`.
3. Whole balloon: claim template `fast-whole` (BTYPE=fast, no CPUS); pod0
   with 1 container -> Ready. `report allowed`; verify
   `len(cpus["pod0c0"]) == 4`; `check-env pod0 pod0c0 DRA_BALLOON fast-0`
   (or fast-1: capture the device from the env and assert it is one of
   them, then assert the cpuset size).
4. Shared balloon: claim template `fast-two` (BTYPE=fast, CPUS=2); pod1
   and pod2 each 1 container -> both Ready, both land on the other fast
   instance (scheduler fills by order; accept either but require
   `cpus["pod1c0"] == cpus["pod2c0"]`, `len == 2`? No: with 2+2 the
   balloon inflates to 4: assert `len(cpus["pod1c0"]) == 4` after pod2 is
   running and the sets are equal, and disjoint from pod0's).
5. Capacity exhausted: pod3 with `fast-two` -> stays Pending
   (`kubectl wait --for=condition=PodScheduled=false pod/pod3 --timeout=60s`).
   Delete pod1 -> pod3 becomes Ready; balloon shrinks or stays 4; assert
   `cpus["pod3c0"] == cpus["pod2c0"]`.
6. Mixing with traditional containers: pod4 via `balloons-busybox`
   template with annotation `two-cpu` and CPUREQ 100m -> Ready;
   `disjoint_sets(cpus["pod4c0"], cpus["pod0c0"], cpus["pod2c0"])`,
   `len(cpus["pod4c0"]) == 2`. Also a pod5 without annotation (default
   balloon) is disjoint from all DRA balloons.
7. A pod with a DRA container and a plain sidecar (NOCLAIM_CONTAINERS=1):
   c0 in the DRA balloon, c1 not in any DRA balloon (disjoint from the
   `fast` and `solo` cpusets).
8. Contradictions (container creation must fail):
   - pod with claim `solo-whole` and annotation
     `balloon.balloons.resource-policy.nri.io: two-cpu` -> container
     creation error; `kubectl describe pod` contains "annotation" and
     "two-cpu" (grep the policy's message fragment, e.g. `requests balloon type`).
   - pod with two claims (`fast-two` and `solo-whole`) referenced by the
     same container -> error mentioning "more than one balloon".
   - traditional pod with annotation naming `solo` (DRA type) and no
     claim -> error "published as DRA devices".
   Use `wait_t=20s create ...` inside `( ... ) && error` like test07 does,
   or `wait=""` + `wait-container-waiting-reason`/`verify-container-error`
   helpers from test/e2e/lib/test.bash (read them first).
   Delete the failed pods afterwards.
9. Restart persistence: `restart-plugin`; running DRA pods keep their
   cpusets (verify again); a new claim still works and lands correctly;
   releasing pods frees capacity (create a pod after deleting one).
10. `cleanup`, `helm-terminate`.

## test32-dra-node-allocatable (needs DRANodeAllocatableResources)

1. Launch with the same cfg. Check `dra-device nodeAllocatableResources`
   is non-null for fast-0; otherwise SKIP.
2. Node has 16 CPUs; kubelet allocatable CPU is ~15.x. Claim the two
   `fast` balloons whole (2 pods, 4 CPUs each) and `solo` whole (2 CPUs):
   10 CPUs via DRA. Read `kubectl get node -o json` allocatable cpu (A).
3. Traditional pod requesting `A - 10 + 1` CPUs (round down to whole
   CPUs; compute in bash) must stay Pending with
   `PodScheduled=false` within 60 s; a pod requesting `A - 10 - 1` CPUs
   must become Ready (it lands in the default balloon, which is disjoint
   from all DRA balloons).
4. Verify `kubectl get pod <dra pod> -o json | jq .status.nodeAllocatableResourceClaimStatuses`
   lists the claim with cpu "4".
5. Verify the pod cgroup effect only loosely (optional): the container
   has no CPU limit, so `cpu.max` is `max`; skip if awkward.
6. cleanup.

## Expectations on failures

If the plugin rejects a container, the kubelet retries CreateContainer;
the pod shows `CreateContainerError` with the NRI error text. Reuse how
test07-maxballoons inspects `kubectl describe pod`.

## Deliverables

- The two test directories with `code.var.sh`, `balloons-dra.cfg`,
  `*.yaml.in`.
- `.plan/5jW-fable-dra-balloons/07-e2e-notes.md`: VM used, gates, run
  commands, verdicts, log excerpts for any failure, and what was changed
  in the policy code if a test exposed a bug (coordinate: policy fixes go
  through the implementation notes too).
