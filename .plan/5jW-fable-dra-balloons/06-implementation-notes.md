# Step 3: implementation notes

Implementation of 04-implementation-spec.md sections 1-7. Nothing is
committed.

## Files changed

| File | Change |
|------|--------|
| pkg/apis/config/v1alpha1/resmgr/policy/balloons/config.go | `BalloonDef.DRA *BalloonDRA` and type `BalloonDRA{DeviceName}` |
| pkg/apis/config/v1alpha1/resmgr/policy/balloons/zz_generated.deepcopy.go | regenerated (make generate) |
| config/crd/bases/config.nri_balloonspolicies.yaml | regenerated (make generate) |
| deployment/helm/balloons/crds/config.nri_balloonspolicies.yaml | regenerated (make generate) |
| pkg/resmgr/dra/names.go (new) | `draDomain` moved here; `DriverName()`, `ClaimOfCDIDevice()`, `cdiClaimPrefix` |
| pkg/resmgr/dra/names_test.go (new) | DriverName, ClaimOfCDIDevice round trip with cdiDeviceName and rejections |
| pkg/resmgr/dra/cdi.go | `cdiDeviceName` uses `cdiClaimPrefix` |
| pkg/resmgr/dra.go | `draDomain` removed; `setupDRA` uses `dra.DriverName()` |
| pkg/resmgr/cache/cache.go | `Container.GetCDIDevices() []string` |
| pkg/resmgr/cache/container.go | `GetCDIDevices()` from NRI `Ctr.GetCDIDevices()` |
| cmd/plugins/topology-aware/policy/mocks_test.go | mock `GetCDIDevices()` (panics "unimplemented" like its neighbors) |
| cmd/plugins/balloons/policy/dra.go (new) | constants, claim record types, device names, publishing, persistence, AllocateClaim/ReleaseClaim, applyDRAClaims, container-to-claim mapping, validateDRAConfig |
| cmd/plugins/balloons/policy/dra_test.go (new) | pure unit tests (see below) |
| cmd/plugins/balloons/policy/balloons-policy.go | struct fields `draDriver`, `draClaims`; Setup keeps claims over ResetPolicyEntries; Start publishes; AllocateResources DRA path + `placeContainer()` helper; ReleaseResources deflates to claims; old AllocateClaim/ReleaseClaim stubs removed; chooseBalloonDef DRA exclusions; requestedMilliCpus includes claims; Reconfigure republishes; validateConfig calls validateDRAConfig; setConfig calls applyDRAClaims; pinCpuMem shares include claimed CPUs |
| deployment/helm/balloons/templates/deviceclass.yaml (new) | DeviceClass `balloons.nri.io`, rendered only with `config.dra.enabled` |
| docs/resource-policy/policy/balloons.md | new section "Publishing Balloons as DRA Devices" (before "Toggle and Reset Pinning..."), TOC entry, a note under "Choosing Balloon Type" |

go.sum was already modified before this work (not touched by me).

## Deviations from the spec and why

1. **Cacheable claim records.** The cache's `GetPolicyEntry` only knows a
   few concrete types (cpusets, maps of strings/cpusets, scalars) or
   types implementing `cache.Cacheable`; anything else hits `log.Fatalf`
   ("can't handle policy data of type"). So `p.draClaims` is a named type
   `draClaimRecords map[string]*draClaim` with `Set/Get` methods, read
   by `loadDRAClaims(cache)`. A unit test covers save, Save()/reload from
   disk and reset.
2. **Capacity checks (last line of defense).**
   - `draGrowBalloon` (used by AllocateClaim and applyDRAClaims) fails if,
     after resizing, the balloon has fewer CPUs than its claims hold.
     `resizeBalloon` silently clamps to `maxCPUs`, so without this a claim
     that does not fit (e.g. force-deleted pods, or no consumable capacity
     tracking) would be accepted and overcommitted.
   - `validateDRAConfig` also rejects a configuration whose `maxCPUs` of a
     claimed balloon is below the CPUs its claims hold ("cannot apply
     configuration: claims hold N mCPU in balloon X[i] but its maxCPUs is
     M"). This keeps setConfig from failing later, after state is wiped.
3. **AllocateClaim with zero results** returns an error instead of
   recording an empty claim (the DRA plugin refuses a CDI spec with no
   devices anyway).
4. **applyDRAClaims placement:** called right after
   `p.ifreeCpus = p.freeCpus.Clone()` (which directly follows the balloon
   creation loop). `ifreeCpus` therefore excludes claim CPUs the same way
   it excludes container CPUs (it means "free before assigning
   containers"). It runs before updatePinning/useCpuClass of setConfig.
5. **CPU shares:** the spec says "use native + claimed when the sum is >
   0, keep no shares when both are 0". The existing code set shares
   whenever a CPU request key existed, even "0" (minimum shares). That is
   preserved: shares are set when a request exists or claimed CPUs > 0.
6. **Extra error case** in draBalloonForContainer: claims that hold no
   devices -> error (cannot happen with records written by AllocateClaim).
7. `publishDRADevices` guards `p.options == nil` as well as a nil Owner.
8. Release paths use one helper `draTargetMilliCpus(bln)`: `max(1,
   requested)` if the balloon has containers, else `requested` (claims
   only); `resizeBalloon` clamps to minCPUs.

## Not done / open issues

- No full-policy (sysfs-backed) unit tests of the resize paths
  (AllocateClaim growing a balloon, ReleaseClaim deflating, restart via
  Setup). Left to e2e (05-e2e-spec.md).
- Restart with a configuration that drops a claimed balloon: Setup's
  validateConfig fails and the plugin does not start until the
  configuration is fixed; the kubelet cannot unprepare the claim
  meanwhile because the DRA plugin is not running. This follows spec
  rule 7 but is worth reviewer attention.
- setConfig failing in applyDRAClaims (not enough free CPUs on the node
  for recorded claims, e.g. after shrinking availableResources) leaves the
  policy in a half-applied state, like the pre-existing applyBalloonDef
  failures. On Reconfigure that is after validation passed.
- Pre-existing wart: setConfig assigns `p.bpoptions` before validation,
  so a rejected Reconfigure leaves `p.bpoptions` pointing to the rejected
  config. Not changed.
- `make generate` (scripts/hack/update_codegen.sh) moves `vendor/` away
  and deletes it at exit. vendor/ was restored with `go mod vendor`.
- `make reformat` runs gofmt on all tracked files and rewrites
  pre-existing generated files (`import ()` in zz_generated.deepcopy.go
  of other packages, pkg/apis/config/v1alpha1/log/klogcontrol/config.go)
  and exits 1 because of `-d`. Those unrelated rewrites were reverted with
  `git checkout`. It skips untracked files, so new files were checked with
  `gofmt -s -l` / formatted with `gofmt -s -w`.

## Verification

| Command | Result |
|---------|--------|
| `make generate` | pass; regenerated balloons deepcopy, config/crd/bases and Helm CRD (git status shows them modified); removed vendor/ (restored by `go mod vendor`) |
| `go build ./...` | pass |
| `go vet ./cmd/plugins/balloons/...` | pass |
| `go test ./pkg/resmgr/... ./cmd/plugins/balloons/... ./cmd/plugins/template/... ./cmd/plugins/topology-aware/...` | pass, 10 packages ok, 0 failures |
| `go test ./cmd/plugins/balloons/policy/ -run 'DRA\|AllocateClaim' -v` | pass: TestDRADeviceName, TestDRADevices, TestDRAClaimEdits, TestAllocateClaimWithoutResize, TestDRABalloonForContainer (8 cases), TestDRAClaimedMilliCpus, TestValidateDRAConfig (14 cases), TestDRAClaimsPersistence |
| `go test ./pkg/resmgr/dra/ -run 'DriverName\|ClaimOfCDI'` | pass |
| `make reformat` | exit 1 due to unrelated pre-existing gofmt diffs (reverted); changed/new files gofmt-clean |
| `make golangci-lint` | pass, "0 issues." |
| `helm template deployment/helm/balloons [--set config.dra.enabled=true]` | DeviceClass rendered only when enabled |
| `make PLUGINS=nri-resource-policy-balloons SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images` | pass |

Built image: `build/images/nri-resource-policy-balloons-image-8627054c96e2.tar`
(tag `nri-resource-policy-balloons:v0.14.0-166-g77146493-dirty`).

## Changes after review (2026-10-07)

- CPU shares of containers sharing a claim: `draContainerMilliCpus(c, bln)`
  now divides the CPUs of a claim equally between the containers of the
  pod in the balloon that use it (counted from `bln.PodIDs` and the
  containers' CDI devices). Two containers sharing a 4 CPU claim and a
  third container with its own 4 CPU claim in the same balloon get shares
  in ratio 1:1:2 (before: 1:1:1). `pinCpuMem` takes the balloon as a
  parameter for this. The first container of a pod gets the whole claim
  when it is created; placing the second re-pins the balloon and updates
  the first (an NRI container update). `ReleaseResources` now re-pins the
  remaining containers of a balloon so that their shares follow when a
  sharing container goes away.
- Documentation: shared-claim item under "Container placement", and a
  limitation note on late inflation of DRA balloons losing CPU locality.
- e2e test31: pod3 has two containers sharing its claim (weights
  2:1:1 against pod2), pod11 has three containers with two claims
  (weights 1:1:2); CPU weights are read from /sys/fs/cgroup/cpu.weight
  inside the containers and compared with the runtime's shares-to-weight
  conversion of the expected milli-CPUs.

## dra.nodeAllocatable option (2026-10-07)

Files changed:
- pkg/apis/config/v1alpha1/resmgr/policy/balloons/config.go: `BalloonDRA.NodeAllocatable *bool`
  (`+kubebuilder:default=true`, optional). Regenerated with `make generate`:
  zz_generated.deepcopy.go, config/crd/bases/config.nri_balloonspolicies.yaml,
  deployment/helm/balloons/crds/config.nri_balloonspolicies.yaml.
- cmd/plugins/balloons/policy/dra.go: helper `draNodeAllocatable(blnDef)`;
  `draDevices` sets `NodeAllocatableResources` only when it returns true.
- cmd/plugins/balloons/policy/dra_test.go: `TestDRADevices` adds a local
  type `unmapped` with `NodeAllocatable: ptr.To(false)` and checks that its
  device has no `NodeAllocatableResources` while fast-1 keeps the mapping.
  `newDRATestPolicy` is unchanged so other tests are not affected.
- docs/resource-policy/policy/balloons.md: `nodeAllocatable` in the `dra`
  option list, device mapping note, requirement note (no effect without the
  gate), new paragraph "Static DRA balloons and node allocatable" with an
  example.
- e2e: balloons-dra.cfg of test31 and test32 (identical) set
  `dra.nodeAllocatable: false` on `lazy`; test32 code.var.sh rewritten as in 08.

Deviations from 08:
- The scheduled-but-failing pod4 fails with "not enough free CPUs"
  (resize of default[0] from 0 to 4 CPUs with 2 free), not "no suitable
  balloon instance available"; the test matches the actual message.
- The old limitation bullet on the scheduler not seeing reserved/minCPUs
  CPUs was replaced by the recipe paragraph plus a narrower bullet: claims
  on growable DRA balloons (minCPUs < maxCPUs) can still fail to prepare
  when CPUs not reserved from the kubelet are held by other balloons.

Verification: make generate, go build ./..., go test
./cmd/plugins/balloons/... ./pkg/resmgr/..., gofmt -s -l (changed files
clean), make golangci-lint (0 issues), image build
build/images/nri-resource-policy-balloons-image-150081d351e8.tar: all pass.
