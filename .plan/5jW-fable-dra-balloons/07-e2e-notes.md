# Step 5: e2e test notes

## Environment

- VM `n4c16-dra-fedora-43-containerd`: Kubernetes v1.37.1, containerd 2.4.1,
  feature gate `DRANodeAllocatableResources=true`, 16 CPUs, allocatable cpu 16.
- Image: `build/images/nri-resource-policy-balloons-image-8627054c96e2.tar`
  (no rebuild needed: no policy change).
- Commands (from `test/e2e`, never concurrently):

  ```
  k8s_feature_gates=DRANodeAllocatableResources=true vm_name=n4c16-dra-fedora-43-containerd \
      ./run_tests.sh policies.test-suite/balloons/n4c16/test31-dra-publish-and-claim
  k8s_feature_gates=DRANodeAllocatableResources=true vm_name=n4c16-dra-fedora-43-containerd \
      ./run_tests.sh policies.test-suite/balloons/n4c16/test32-dra-node-allocatable
  # regressions: same variables, test01-basic-placement and test07-maxballoons
  ```

## Tests

- `test/e2e/policies.test-suite/balloons/n4c16/test31-dra-publish-and-claim/`
  `{code.var.sh,balloons-dra.cfg,dra-claim.yaml.in,dra-pod.yaml.in}`
- `test/e2e/policies.test-suite/balloons/n4c16/test32-dra-node-allocatable/`
  (same cfg and templates, copied)

Helpers live in each `code.var.sh` (`dra-devices`, `dra-device FIELD NAME`,
`container-env`, `check-env`, `container-cpus`, `restart-plugin`,
`milli-cpus`). Device attributes are unqualified, so CEL selectors use
`device.attributes["balloons.nri.io"].balloonType`.

## Verdicts

| Run | Test | Verdict | Cause |
|-----|------|---------|-------|
| 1 | test31 | FAIL | `len(cpus["pod1c0"]) == 2` was 3. Test bug: run.sh sets global defaults `CPUREQ=1 CPULIM=2` (`yaml_in_defaults`), so `dra-pod` got native requests 1/limit 2 and the balloon was sized claim + 1 CPU, correctly. Fixed by `CPUREQ="" CPULIM=""` at the top of both tests. |
| 2 | test31 | PASS | |
| 1 | test32 | PASS | |
| - | test01-basic-placement | PASS | |
| - | test07-maxballoons | PASS | (the "pod2/pod5 ... timed out" lines are its expected failures) |
| 3 | test31 | PASS | stability rerun |

## Deviations from 05-e2e-spec.md

- test31 step 8 uses `wait=""` + `verify-container-error POD CTR REGEXP 60`
  instead of `kubectl describe`. Before the "more than one balloon" case
  pod3 is deleted so that fast has capacity for the `fast-two` claim (all
  error cases need their claims to be schedulable, else the pod is just
  Pending), and pod6 (solo) is deleted before the annotation case.
- test31 restart check compares `Cpus_allowed_list` of pod0c0 and pod2c0
  before and after `restart-plugin` (exact equality) and then creates a
  new `fast-two` claim (lands on the same instance as pod2) and, after
  deleting pod0, a new `fast-whole` claim (lands on pod0's former device).
- test32 does not use `A - 10 +- 1` literally: the scheduler also counts
  the native requests of kube-system pods (900m, read from
  `kubectl describe node` before any claim), and the policy has only
  15 - 1 reserved - 10 claimed = 4 CPUs left for regular balloons. With
  `free = (A - native - 10000m) / 1000 = 5`: a 6-CPU pod stays
  `PodScheduled=False` ("0/1 nodes are available: 1 Insufficient cpu"),
  a 4-CPU pod runs in the default balloon, disjoint from the DRA balloons.
  The 6-CPU pod would fit (6 + 0.9 <= 16) without the claim accounting.
- Containerd version check (SKIP if < 2.3.0) is in test31 only.

## Observations from logs

- Messages seen in container state (exact text from the policy):
  - `container default/pod7/pod7c0: pod annotation balloon.balloons.resource-policy.nri.io requests balloon type "two-cpu" but its DRA device "solo-0" is of type "solo"`
  - `container default/pod8/pod8c0 requests DRA devices of more than one balloon: fast-1, solo-0`
  - `balloon allocation for container default/pod9/pod9c0 failed: balloons: balloon type "solo" is published as DRA devices, request it through a resource claim`
- With KEP-5517 the kubelet inflates the DRA container's cgroup: a
  container with native request 1/limit 2 and a 2-CPU claim got
  `shares 3072, quota 400000` and `status.containerStatuses[].resources.limits.cpu: "4"`;
  the policy's request estimate (1000 mCPU) still comes from the spec, so no
  double counting.
- `status.nodeAllocatableResourceClaimStatuses` format:
  `[{"containers":["pod0c0"],"mapping":[{"name":"cpu","quantity":"4"}],"resourceClaimName":"pod0-cpus-..."}]`
- Published `nodeAllocatableResources`: `{"cpu":{"mapping":{"capacityKey":"cpu","capacityMultiplier":"1"}}}` survives with the gate.
- After the restart, inflating fast[1] from 2 to 4 CPUs took fragmented
  free CPUs: `allocated 2000 mCPU in balloon fast[1]{cpus:"1,3-5", mems:"0-1"}`,
  i.e. the balloon spans two NUMA nodes. Correct but worth knowing: DRA
  balloons with minCPUs < maxCPUs can lose locality when they grow late.
- Releases deflate DRA balloons down to their remaining claims
  (`released, changed cpus: balloon from "4-7" to "4-5"`).
- Unrelated pre-existing error on every run: agent cannot patch
  `balloonspolicies/status` (RBAC), and coverage dump curl 500 (image not
  built with -cover).

## Policy fixes

None. No test exposed a policy bug.

## Final confirmation run (coordinator, 2026-10-06)

Both tests rerun sequentially with the same variables after all edits:
`PASS balloons/n4c16/test31-dra-publish-and-claim`,
`PASS balloons/n4c16/test32-dra-node-allocatable`. The VM
`n4c16-dra-fedora-43-containerd` was left running.

## Shared-claim CPU shares (2026-10-07)

test31 extended: pod3 has two containers sharing one 2 CPU claim next to
pod2 with its own 2 CPU claim (weights 2:1:1), and pod11 has three
containers with two claims in one pod (weights 1:1:2). Weights are read
from /sys/fs/cgroup/cpu.weight inside the containers.

| Run | Test | Verdict | Cause |
|-----|------|---------|-------|
| 1 | test31 | FAIL | Test bug: expected weights used the linear shares-to-weight conversion of runc < 1.3; the VM has runc 1.5.2, whose quadratic conversion maps 2048 shares to weight 174 (linear: 79). `expected-weights` now accepts both conversions. |
| 2 | test31 | PASS | pod2c0 174, pod3c0 100, pod3c1 100; pod11c0 100, pod11c1 100, pod11c2 174 |
| 1 | test01-basic-placement | PASS | regression after the pinCpuMem change |

Image: build/images/nri-resource-policy-balloons-image-126caf7ae5b4.tar.

## Zero-CPU DRA balloons (2026-10-07)

balloons-dra.cfg (both tests) gained type `lazy`: minBalloons 1, maxCPUs 2,
no minCPUs. test31 asserts from the plugin log that lazy[0] starts with
no CPUs, claims it whole (pod12, 2 CPUs), and after deleting the pod waits
for the release log line showing lazy[0] with no CPUs again.

| Run | Test | Verdict | Cause |
|-----|------|---------|-------|
| 1 | test31 | FAIL | Test placement: the step ran when only 1 CPU was free (pods 0-5 held 14 of 15), so preparing the 2 CPU claim failed with "not enough free CPUs" and the kubelet retried. Moved the step before pod4/pod5. |
| 1 | test32 | PASS | with the new cfg |
| 2 | test31 | FAIL | Test bug: pod named `lazypod`; the framework's res_allowed collector only recognizes `pod<N>c<M>` container names. Renamed to pod12. |
| 3 | test31 | PASS | |

## dra.nodeAllocatable option (2026-10-07)

`lazy` has `dra.nodeAllocatable: false` in both cfg files. test32 asserts
lazy-0 is published without nodeAllocatableResources, claims fast (2x4) and
solo (2) mapped, then lazy (2, pod5) unmapped: pod5 has no
nodeAllocatableResourceClaimStatuses. With free = (16000 - native - 10000)/1000
= 5, pod3 (6 CPUs) stays unscheduled, pod4 (4 CPUs) is scheduled but its
container fails (policy has 2 free CPUs); after deleting pod5 and waiting for
the lazy[0] release log line, a recreated pod4 runs with >= 4 CPUs.
Image: build/images/nri-resource-policy-balloons-image-150081d351e8.tar.

| Run | Test | Verdict | Cause |
|-----|------|---------|-------|
| 1 | test32 | FAIL | Test bug: expected error "no suitable balloon instance available"; actual is "resizing balloon default[0] failed ... not enough free CPUs (2) to resize current CPU set from 0 to 4 CPUs". Regexp changed to "not enough free CPUs". All other steps passed. |
| 2 | test32 | PASS | |
| 1 | test31 | PASS | unchanged code, new cfg |
| 1 | test01-basic-placement | PASS | regression |

### Coordinator review runs (2026-10-07)

- Widened test32's pod4 error check to accept both refusal messages the
  policy can give ("not enough free CPUs" when FillNewBalloon picks the
  empty default balloon and the resize fails, "no suitable balloon
  instance available" otherwise). First attempt used `|`, which
  verify-container-error's basic grep takes literally: test32 FAIL on
  the check although the container had failed as expected. Fixed with
  `\|`.
- Independent reruns after the agent's work: test31 PASS, test32 FAIL
  (the `|` mistake above), test32 PASS after the fix. golangci-lint 0
  issues, unit tests pass, gofmt clean, ASCII only.
