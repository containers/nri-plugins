# Task 5jW: basic DRA support in the balloons policy

Task file: `5jW-fable-dra-balloons.md` in the repository root.
Branch: `5jW-balloons-dra-proto`.

This directory holds the plan, findings and notes. Read the files in
numeric order. Each file is self-contained enough that the work can be
continued from it.

| File | Content |
|------|---------|
| 00-status.md | this file: what is done, what is next, how to continue |
| 01-findings.md | facts learned about Kubernetes 1.37 DRA, KEP-5517, nri-plugins DRA plumbing, runtimes, repository quirks |
| 02-design-config.md | step 1: the balloon type configuration option |
| 03-design-cpu-accounting.md | step 2: how native CPU requests, DRA, kubelet and the policy co-operate; options studied; decision |
| 04-implementation-spec.md | step 3: precise implementation specification for the policy code |
| 05-e2e-spec.md | step 4: end-to-end test specification |
| 06-implementation-notes.md | written during/after implementation: deviations, open issues |
| 07-e2e-notes.md | written during/after e2e testing: environment, results |

## Status

- [x] Study references (resmgr DRA code, template policy, Kubernetes 1.37 source, KEP-5517, containerd NRI).
- [x] Step 1 design: configuration option (02-design-config.md).
- [x] Step 2 design: CPU accounting co-operation (03-design-cpu-accounting.md).
- [x] Step 3 implementation (Opus agent, spec in 04-implementation-spec.md; notes in 06). Reviewed: lint 0 issues, unit tests pass, image built.
- [x] Step 4 e2e tests (Opus agent, spec in 05-e2e-spec.md; notes in 07). test31 and test32 PASS, regressions test01 and test07 PASS.
- [x] Review, lint (0 issues), unit tests (pass), e2e runs, final notes (see Wrap-up below).

## Wrap-up (2026-10-06)

- Nothing is committed; all changes are in the working tree of branch
  5jW-balloons-dra-proto. `git status` lists the touched files; `.plan/`
  and the e2e VM directory are untracked and should not be committed.
- The incidental go.sum additions made by `go mod vendor` were reverted;
  builds and tests pass without them.
- Known open points (not bugs, design consequences; see 03 and 06):
  claim-only pods are BestEffort; a restart with a configuration that
  drops a claimed balloon keeps the plugin from starting until the
  configuration is fixed; DRA balloons with minCPUs < maxCPUs may grow
  from fragmented free CPUs (documented in balloons.md limitations).
- Unrelated pre-existing e2e noise: agent lacks RBAC to patch
  balloonspolicies/status; coverage dump fails when the image is built
  without coverage.

## Follow-up (2026-10-07)

- CPU shares of containers sharing a claim are now divided equally
  between them (1:1:2 example in docs); re-pinning on release keeps them
  current. Verified by the extended test31 and regression test01 (see 06
  and 07). Image: nri-resource-policy-balloons-image-126caf7ae5b4.tar.

## How to continue

1. Read 01-05.
2. If implementation is incomplete, follow 04-implementation-spec.md; it
   lists every file to touch and the exact behavior.
3. Build: `make PLUGINS=nri-resource-policy-balloons SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images`
   (after `make generate` if config types changed).
4. Lint: `make reformat && make golangci-lint`.
5. E2E: see 05-e2e-spec.md for the VM and feature gates needed.

## E2E environment prepared (2026-10-06)

- VM `test/e2e/n4c16-dra-fedora-43-containerd`: Kubernetes v1.37.1,
  containerd 2.4.1, gate `DRANodeAllocatableResources=true` on apiserver,
  scheduler and kubelet, node allocatable cpu 16, CPU manager policy none.
  Created with
  `k8s_feature_gates=DRANodeAllocatableResources=true vm_name=n4c16-dra-fedora-43-containerd ./run_tests.sh policies.test-suite/balloons/n4c16/test01-basic-placement`
  (PASS with the pre-change balloons image). Always pass the same
  `k8s_feature_gates` and `vm_name` when running tests on it.
- The user's own VM `test/e2e/n4c16-fedora-43-containerd` (no gates) is
  untouched.

## Repository quirks found

- An untracked, stale `vendor/` directory (k8s.io/api v0.34.11, go.mod
  says v0.37.0) made plain `go build` fail with "inconsistent vendoring".
  It was refreshed with `go mod vendor` so that go.mod and vendor agree.
- `.plan/` is not in .gitignore; do not commit it unless asked.
