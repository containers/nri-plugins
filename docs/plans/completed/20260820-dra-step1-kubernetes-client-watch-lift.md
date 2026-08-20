# DRA v1 step 1 — lift `pkg/kubernetes/{client,watch}` out of `pkg/agent`

## Overview

Refactor: extract the Kubernetes client bootstrap currently inlined in `pkg/agent/agent.go` into a new `pkg/kubernetes/client/` package, and relocate the existing `pkg/agent/watch/` package to `pkg/kubernetes/watch/` unchanged. Expose node name, kube client, kubeconfig path, and rest config from the agent so future consumers (DRA driver in later plan steps) can reach them.

Prerequisite for the DRA integration described in [docs/dra/design.md](../dra/design.md); this is step 1 of the [docs/dra/plan.md](../dra/plan.md) landable-PR sequence. Pure refactor — no behavioral change intended. Ships as a standalone PR that has value on its own (cleaner agent package boundary).

Adapted from three commits on the `pr-536-dra` branch (klihub's DRA prototype):

- `5dcb66dc` — `pkg/kubernetes: split out client setup code from agent.`
- `42ec1022` — `pkg/kubernetes: allow setting accepted and wire content types.`
- `88140644` — `agent: expose node name, kube client and config.`

Note: PR #536 also introduced a *parallel* `pkg/kubernetes/watch` with a new `ObjectClient` interface (its commit `45a14d1c`) while leaving `pkg/agent/watch` intact — two watch packages coexisting. This plan deliberately deviates: the existing `pkg/agent/watch` package is *moved* verbatim to `pkg/kubernetes/watch`, keeping its `CreateFn`-based API. Rationale: minimum-risk refactor, single canonical watch package, no API change for existing callers. Migrating to PR #536's `ObjectClient` API can be a separate future improvement.

The source commits are proven; the plan below re-derives them cleanly (rather than direct cherry-pick) so we can add tests that PR #536 does not have.

## Context (from discovery)

- **Existing packages relevant to this refactor:**
  - `pkg/agent/agent.go` — 749 lines. Contains inline client bootstrap: `setupClients` (line 343), `getRESTConfig` (line 391), `cleanupClients` (line 382), plus `httpCli` field on `Agent`. Watch call sites at lines 420, 444, 452, 488 use `pkg/agent/watch`.
  - `pkg/agent/watch/` already exists with `watch.go` (~39 lines, `k8s.io/apimachinery/pkg/watch` re-exports), `object.go` (~203 lines, `Object(ctx, ns, name, CreateFn)` API), `file.go` (~188 lines). This whole package is what gets moved verbatim to `pkg/kubernetes/watch/`.
  - `pkg/kubernetes/` already exists with `cpuset.go`, `kubernetes.go`, `resources.go` and their tests. The new `client/` and `watch/` subdirectories coexist alongside these.
- **Consumers of the current inline client:** `pkg/agent/agent.go` internally, plus anything downstream of the `ConfigInterface.SetKubeClient(cli *http.Client, cfg *rest.Config)` callback. Callers should be unaffected by the client extraction (the agent's external surface only gains getters). Watch consumers inside `agent.go` will have their import path updated but call-site signatures stay identical.
- **PR #536 file layout to derive (partial — watch package differs):**
  - `pkg/kubernetes/client/client.go` — ~194 lines. `Client` struct wraps `*kubernetes.Clientset`, `*rest.Config`, `*http.Client`. Functional `Option` pattern. `New(opts ...Option)` constructor. Options: `WithKubeConfig`, `WithInClusterConfig`, `WithKubeOrInClusterConfig`, `WithRestConfig`, `WithHttpClient`, `WithAcceptContentTypes`, `WithContentType`. Methods: `RestConfig()` (returns shallow copy), `HttpClient()`, `K8sClient()`, `Close()`.
  - `pkg/kubernetes/watch/` — moved verbatim from `pkg/agent/watch/`. PR #536's `pkg/kubernetes/watch` files are NOT used.
- **Dependencies already in go.mod:** `k8s.io/client-go`, `k8s.io/apimachinery` — all transitively present, no new go.mod entries needed.

## Development Approach

- **testing approach:** TDD (tests first).
- complete each task fully before moving to the next.
- make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task.
  - write unit tests for new functions/methods before implementation.
  - write unit tests for modified functions/methods.
  - tests cover both success and error scenarios.
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**
- run tests after each change.
- **maintain backward compatibility of `pkg/agent`'s external surface.** Callers using `SetKubeClient(cli, cfg)` continue to work. Adding getters (node name, kube client, kube config) is additive.

## Testing Strategy

- **unit tests:** required per task. `pkg/kubernetes/client/client_test.go` covers option composition (with order-independence), error paths (bad kubeconfig, missing file, retry sentinel), and Client-method behavior (`RestConfig()` shallow copy, `Close()` idempotency). `pkg/kubernetes/watch/*_test.go` inherits whatever tests existed in `pkg/agent/watch/*_test.go` (verbatim move) plus minimum additions for the type-alias surface and Create-vs-Write file event distinction.
- **e2e tests:** not applicable — the refactor doesn't change observable plugin behavior. Existing e2e test suites for topology-aware / balloons must continue to pass unchanged; that is our regression guard.
- **manual smoke test:** `make build && ./build/bin/nri-resource-policy-topology-aware --help` verifies the binary compiles. Deploy on a dev cluster and confirm the plugin registers with kubelet as before.

## Progress Tracking

- mark completed items with `[x]` immediately when done.
- add newly discovered tasks with ➕ prefix.
- document issues/blockers with ⚠️ prefix.
- update plan if implementation deviates from original scope.
- keep plan in sync with actual work done.

## Solution Overview

One new package and one relocation:

- **`pkg/kubernetes/client`** (new) — encapsulates rest-config resolution (in-cluster vs kubeconfig file), content-type negotiation (JSON / protobuf), and clientset construction into a single `Client` type built via functional options. Replaces `agent.getRESTConfig` and the inline bootstrap in `agent.setupClients`.
- **`pkg/kubernetes/watch`** (moved) — the current `pkg/agent/watch` package, relocated without API change. Existing wrappers (`Object(ctx, ns, name, CreateFn)`, file watcher, type-alias core) preserved intact. Only the import path changes for callers.

**Design decisions:**

- **Functional options over a config struct.** Matches Kubernetes ecosystem convention (`kubeletplugin.Start` uses the same pattern). Makes it easy to add options later without breaking callers.
- **Two-pass option application.** Options that need `c.cfg` present (content-type options) return a sentinel `errRetryWhenConfigSet`; `New()` collects them, applies the config source, then re-runs them. Result: option ordering doesn't matter to callers.
- **`Client` embeds `*kubernetes.Clientset`.** Callers who need the raw clientset just use it directly; `Client` adds nothing to their surface. This means we don't have to proxy every method on `Clientset`.
- **`Client.RestConfig()` returns a `rest.CopyConfig` copy** of the internal `*rest.Config`. Symmetric with `WithRestConfig` on input. `rest.CopyConfig` copies top-level fields and value-typed nested structs (`TLSClientConfig`, `Impersonate`, `ContentConfig`) but *shares* underlying map/slice storage (`Impersonate.Extra`, `TLSClientConfig.CAData`, etc.). Callers may overwrite top-level and value-struct fields on the returned config freely; callers must NOT mutate the contents of nested maps or slices. This matches the Kubernetes-ecosystem convention used by `dra-driver-cpu` and PR #536's `WithRestConfig` side. Discovered during Task 2 implementation — the earlier plan promise of "safe to mutate at any depth" was aspirational; matching upstream is correct.
- **Move `pkg/agent/watch` verbatim** rather than adopting PR #536's `ObjectClient` redesign. Keeps this step a pure refactor with zero API change. The `ObjectClient` redesign is a defensible future improvement, tracked separately.
- **Agent gains four getters.** `NodeName() string`, `KubeClient() *client.Client`, `KubeConfig() string` (the kubeconfig file path), `RestConfig() *rest.Config`. Simple accessors, no side effects; the DRA driver in later plan steps consumes them.

## Technical Details

### Data structures

- `client.Client` struct:
  ```go
  type Client struct {
      cfg  *rest.Config
      http *http.Client
      *kubernetes.Clientset
  }
  ```
- `client.Option func(*Client) error` — functional-option type.
- `watch` re-export aliases as listed under Context above.

### Options catalog

- `WithKubeConfig(file string)` — build config from a kubeconfig file path.
- `WithInClusterConfig()` — build config from in-cluster service-account.
- `WithKubeOrInClusterConfig(file string)` — try file first (if non-empty), fall back to in-cluster.
- `WithRestConfig(cfg *rest.Config)` — use a pre-built rest config. Invoked internally by `WithKubeConfig`/`WithInClusterConfig`; also useful for callers that already have a config. Deep-copies its input via `rest.CopyConfig` so the caller retains full ownership of the original and future mutations don't leak in.
- `WithHttpClient(hc *http.Client)` — inject a pre-built HTTP client, e.g. one shared with another component.
- `WithAcceptContentTypes(types ...string)` — set `AcceptContentTypes` on the rest config. Requires config to be present (uses retry-when-config-not-set).
- `WithContentType(type string)` — set `ContentType` on the rest config. Requires config to be present (uses retry-when-config-not-set).
- Constants: `ContentTypeJSON` (`"application/json"`), `ContentTypeProtobuf` (`"application/vnd.kubernetes.protobuf"`).

**`New()` fallback:** if all options are applied and `c.cfg` is still nil, `New()` runs `WithInClusterConfig()` as a final fallback. So `client.New()` with no options returns whatever `rest.InClusterConfig()` returns (working config in-cluster; `rest.ErrNotInCluster` outside).

### Client methods

- `RestConfig() *rest.Config` — returns a `rest.CopyConfig` copy of the internal rest config. Top-level and value-struct fields are safe to overwrite; nested map/slice contents (`Impersonate.Extra`, `TLSClientConfig.CAData`, etc.) are shared with the internal copy and must not be mutated.
- `HttpClient() *http.Client` — the underlying HTTP client (used by `agent.setupClients` to construct `nrtCli` and to pass into `ConfigInterface.SetKubeClient`).
- `K8sClient() *kubernetes.Clientset` — the wrapped clientset. Callers can also use the embedded field directly.
- `Close()` — release resources (idempotent). Used by `agent.cleanupClients`.

### Agent surface additions

New methods on `pkg/agent/agent.Agent`:

- `NodeName() string` — node name (already known internally; exposed).
- `KubeClient() *client.Client` — the new client type.
- `KubeConfig() string` — the kubeconfig **file path** (i.e., the value of `a.kubeConfig`, matches PR #536 commit `88140644`).
- `RestConfig() *rest.Config` — shortcut for `KubeClient().RestConfig()`; returns the `rest.CopyConfig` copy.

### Processing flow

Before:

```
Agent.setupClients (~30 lines inline)
  → getRESTConfig: rest.InClusterConfig / clientcmd.BuildConfigFromFlags
  → construct http.Client from restCfg (stored on Agent.httpCli)
  → kubernetes.NewForConfigAndClient
  → nrtapi.NewForConfigAndClient(restCfg, httpCli)
  → invoke ConfigInterface.SetKubeClient(httpCli, restCfg)
Agent.cleanupClients:
  → a.httpCli.CloseIdleConnections() (best-effort)
```

After:

```
Agent.setupClients (~5 lines)
  → a.k8sCli, err = client.New(client.WithKubeOrInClusterConfig(a.kubeConfig))
  → nrtCli, err = nrtapi.NewForConfigAndClient(a.k8sCli.RestConfig(), a.k8sCli.HttpClient())
  → invoke ConfigInterface.SetKubeClient(a.k8sCli.HttpClient(), a.k8sCli.RestConfig())
Agent.cleanupClients:
  → a.k8sCli.Close()
```

The `httpCli *http.Client` field on `Agent` is removed; every caller reaches it through `a.k8sCli.HttpClient()`. The `getRESTConfig` method is removed entirely.

## What Goes Where

- **Implementation Steps (below):** all code changes and tests are in this repo — this plan can be completed autonomously.
- **Post-Completion:** manual smoke test on a real cluster, and running the existing e2e suite as a regression check.

## Implementation Steps

### Task 1: Add `pkg/kubernetes/client` package skeleton with rest-config helpers

**Files:**
- Create: `pkg/kubernetes/client/client.go`
- Create: `pkg/kubernetes/client/client_test.go`

- [ ] write tests for `GetConfigForFile` (success with valid kubeconfig, error with missing file, error with malformed file)
- [ ] write tests for `InClusterConfig` (error when not in cluster — check `rest.ErrNotInCluster`)
- [ ] implement `GetConfigForFile(kubeConfig string) (*rest.Config, error)` wrapping `clientcmd.BuildConfigFromFlags`
- [ ] implement `InClusterConfig() (*rest.Config, error)` wrapping `rest.InClusterConfig`
- [ ] define `Client` struct with `cfg`, `http`, and embedded `*kubernetes.Clientset`; define `Option` type
- [ ] run tests — must pass before task 2

### Task 2: Add `client.New` constructor, config options, and Client methods

**Files:**
- Modify: `pkg/kubernetes/client/client.go`
- Modify: `pkg/kubernetes/client/client_test.go`

- [ ] write tests for `New()` with no options — falls back to `WithInClusterConfig()`; expect `rest.ErrNotInCluster` outside a cluster, guarded by `os.Getenv("KUBERNETES_SERVICE_HOST") == ""` to skip inside CI
- [ ] write tests for `New(WithKubeConfig(file))` — success case with fixture kubeconfig, error case with missing file
- [ ] write tests for `New(WithInClusterConfig())` — same in-cluster guard as above
- [ ] write tests for `New(WithKubeOrInClusterConfig(""))` and `New(WithKubeOrInClusterConfig(file))` — verifies file-first behavior
- [ ] write tests for `New(WithRestConfig(cfg))` — accepts a pre-built config and does not call any file / in-cluster resolver
- [ ] write tests for `New(WithHttpClient(hc))` — accepts a pre-built HTTP client; verifies `Client.HttpClient()` returns the same pointer
- [ ] write tests for `Client.RestConfig()` — `rest.CopyConfig` semantics: mutating a top-level field (`Host`) on the returned config does not affect subsequent `RestConfig()` calls. Do NOT assert nested-map/slice-content independence — that is not `rest.CopyConfig`'s guarantee.
- [ ] write tests for `WithRestConfig(cfg)` — `rest.CopyConfig` semantics on input: overwriting `cfg.Host` after `New()` returns does not affect the client's `RestConfig()`
- [ ] write tests for `Client.HttpClient()`, `Client.K8sClient()` — return the expected inner values
- [ ] write tests for `Client.Close()` — safe to call multiple times (idempotent)
- [ ] implement `New(opts ...Option) (*Client, error)` — applies options; if `c.cfg` still nil, run `WithInClusterConfig()`; construct HTTP client and clientset
- [ ] implement `WithKubeConfig`, `WithInClusterConfig`, `WithKubeOrInClusterConfig`, `WithRestConfig`, `WithHttpClient` options
- [ ] implement `RestConfig() *rest.Config` (returning `rest.CopyConfig(c.cfg)`), `HttpClient()`, `K8sClient()`, `Close()`
- [ ] run tests — must pass before task 3

### Task 3: Add content-type options with retry-when-config-not-set

**Files:**
- Modify: `pkg/kubernetes/client/client.go`
- Modify: `pkg/kubernetes/client/client_test.go`

- [ ] write tests for `WithAcceptContentTypes(types...)` — verifies `AcceptContentTypes` set on the config as a comma-joined string
- [ ] write tests for `WithContentType(t)` — verifies `ContentType` set on the config
- [ ] write tests for **order-independence**: `New(WithContentType(...), WithKubeConfig(file))` and `New(WithKubeConfig(file), WithContentType(...))` produce equivalent clients — verifies the retry-when-config-not-set mechanism
- [ ] write test for content-type-only with no explicit config source: `New(WithContentType(ContentTypeProtobuf))` — the `WithInClusterConfig` fallback must run first, and the retry list must be re-applied afterward. Use the in-cluster env guard like Task 2.
- [ ] write test for multiple content-type options in the retry list: `New(WithAcceptContentTypes(...), WithContentType(...), WithKubeConfig(file))` — verifies both retried options land in the final config, in the order they were passed
- [ ] add `ContentTypeJSON` (`"application/json"`) and `ContentTypeProtobuf` (`"application/vnd.kubernetes.protobuf"`) constants
- [ ] implement `errRetryWhenConfigSet` sentinel error and the two-pass logic in `New()`: on first pass, collect options returning the sentinel; after the config fallback runs, re-apply those options
- [ ] implement `WithAcceptContentTypes(...string)` and `WithContentType(string)` options — return `errRetryWhenConfigSet` if `c.cfg == nil`, otherwise set the field on the config
- [ ] run tests — must pass before task 4

### Task 4: Move `pkg/agent/watch` → `pkg/kubernetes/watch` verbatim

**Files:**
- Delete: `pkg/agent/watch/watch.go`, `pkg/agent/watch/object.go`, `pkg/agent/watch/file.go` (and any `_test.go` files present)
- Create: `pkg/kubernetes/watch/watch.go`, `pkg/kubernetes/watch/object.go`, `pkg/kubernetes/watch/file.go` (and moved test files)

- [ ] `git mv pkg/agent/watch/*.go pkg/kubernetes/watch/` (preserves history). The moved package has no existing test files (verified) — new tests are added in the checkboxes below.
- [ ] update `package` declaration only if needed (it's already `package watch` — no change) and update any relative imports referenced *from within* the moved files
- [ ] change the logger tag in `pkg/kubernetes/watch/watch.go` from `logger.Get("agent")` to `logger.Get("watch")` so operator log-grep matches the new location
- [ ] run `go build ./pkg/kubernetes/watch/...` — must compile
- [ ] create `pkg/kubernetes/watch/watch_test.go` with type-alias assertions: `var _ k8swatch.Interface = Interface(nil)` and `_ = Added == k8swatch.Added` (no cast — aliases make it unnecessary). Simple assignment tests only, no fake Interface implementation.
- [ ] add tests covering: object-watch happy path (Added/Modified via fake `CreateFn`), object-watch `w.Stop()` idempotency, file-watch Create-vs-Write distinguishing (verifies existing `file.go` emits `Added` for Create and `Modified` for Write — defensive regression guard, since PR #536's variant emits `Added` for both)
- [ ] run tests — must pass before task 5

### Task 5: (reserved — merged into task 4)

*Previously task 5 was "port `pkg/kubernetes/watch/object.go` from pr-536-dra". Since we're doing a verbatim move rather than adopting PR #536's `ObjectClient` API, this task no longer exists. Numbering preserved for continuity with the review that used the original numbering; skip and proceed to Task 6.*

### Task 6: (reserved — merged into task 4)

*Previously task 6 was "port `pkg/kubernetes/watch/file.go` from pr-536-dra". Same reason as Task 5. Skip and proceed to Task 7.*

### Task 7: Rewire `pkg/agent/agent.go` to use `pkg/kubernetes/client` + updated watch import

**Files:**
- Modify: `pkg/agent/agent.go`
- Modify or Create: `pkg/agent/agent_test.go` (for the new getters and regression guard)

Getters and regression tests:

- [ ] write tests for `Agent.NodeName()` — returns the node name once known
- [ ] write tests for `Agent.KubeClient()` — returns non-nil `*client.Client` after `setupClients` succeeds; returns nil before. Success path uses a minimal fixture kubeconfig pointing at `example.com` (`clientcmd.BuildConfigFromFlags` only parses; `kubernetes.NewForConfigAndClient` does not contact the server). Reuse the fixture from Task 2.
- [ ] write tests for `Agent.KubeConfig()` — returns the kubeconfig file path (a `string`); returns empty string before `setupClients`
- [ ] write tests for `Agent.RestConfig()` — returns the shallow copy from `KubeClient().RestConfig()`
- [ ] write test verifying `setupClients` still invokes `ConfigInterface.SetKubeClient(hc, cfg)` on success (regression guard for existing callers)

Refactor:

- [ ] rewrite `setupClients` to construct `a.k8sCli, err = client.New(client.WithKubeOrInClusterConfig(a.kubeConfig))`
- [ ] rewire NRT-client init: `nrtCli, err = nrtapi.NewForConfigAndClient(a.k8sCli.RestConfig(), a.k8sCli.HttpClient())` — replaces the old `restCfg` / `a.httpCli` locals
- [ ] update `ConfigInterface.SetKubeClient` invocation to pass `a.k8sCli.HttpClient()` and `a.k8sCli.RestConfig()`
- [ ] remove the `httpCli *http.Client` field from `Agent`
- [ ] update `configure()` (line 308 area) — any place currently using `a.httpCli` switches to `a.k8sCli.HttpClient()`
- [ ] simplify `cleanupClients` to call `a.k8sCli.Close()` (drop the manual `a.httpCli.CloseIdleConnections()`)
- [ ] remove `getRESTConfig()` method entirely
- [ ] add `NodeName()`, `KubeClient()`, `KubeConfig()`, `RestConfig()` getters (see "Agent surface additions")
- [ ] update the four watch call sites (lines 420, 444, 452, 488 area — file.Watch and object.Watch) to import `github.com/containers/nri-plugins/pkg/kubernetes/watch` instead of `.../pkg/agent/watch`. Signatures do not change.
- [ ] verify no import of `k8s.io/client-go/tools/clientcmd` or direct `rest.*` construction remains in `agent.go` (only through `pkg/kubernetes/client`)
- [ ] verify `go build ./cmd/plugins/topology-aware/... ./cmd/plugins/balloons/...` compiles cleanly
- [ ] run all agent tests — must pass before task 8

### Task 8: Verify acceptance criteria

- [ ] verify the three PR #536 commits' functionality is re-derived: client setup + options (`5dcb66dc`), content-type options with retry (`42ec1022`), agent getters (`88140644`)
- [ ] verify `pkg/kubernetes/watch` reached its target state via `git mv` — history preserved; `git log --follow pkg/kubernetes/watch/object.go` shows the old `pkg/agent/watch/object.go` commits
- [ ] verify `pkg/kubernetes/client/`, `pkg/kubernetes/watch/` packages have no cyclic imports and no imports from `cmd/plugins/`
- [ ] verify `pkg/agent/watch/` directory is gone
- [ ] run full test suite: `make test`
- [ ] run linter: `make lint`
- [ ] manually build all plugin binaries: `make build` — confirm no compilation errors
- [ ] confirm existing e2e tests still pass in dev environment (or note in Post-Completion if unavailable)

### Task 9: Update documentation

- [ ] update `docs/dra/plan.md` step 1: mark checkbox-style done, and append a "Landed in #NNN" line under the step (example format: `**Landed:** [containers/nri-plugins#NNN](https://github.com/containers/nri-plugins/pull/NNN)`)
- [ ] update `CLAUDE.md` if new patterns emerged worth flagging (probably none — this is a straight refactor)
- [ ] move this plan to `docs/plans/completed/` (create the directory if needed)

## Post-Completion

**Manual verification:**

- Smoke test on a real cluster: deploy `topology-aware` and `balloons` plugins, confirm they register with kubelet and run steady-state workloads without regressions. The refactor should be invisible at runtime.
- Confirm the e2e suite `test/e2e/policies.test-suite/topology-aware/n4c16/` and `test/e2e/policies.test-suite/balloons/n4c16/` still pass.

**External system updates:** none. This is a pure internal refactor; no consuming projects or deployment configs change.
