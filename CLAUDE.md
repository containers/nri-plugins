# CLAUDE.md — nri-plugins project context

This file is auto-loaded by Claude Code at the start of every session in this repo. Keep it short and repo-specific — background research and design docs go under `docs/`.

## About this repo

`containers/nri-plugins` — NRI (Node Resource Interface) plugins for Kubernetes container runtimes. Ships several policy plugins that steer container CPU/memory placement via NRI: `topology-aware`, `balloons`, `memory-policy`, `memory-qos`, `memtierd`, `resctrl-mon`, `sgx-epc`, `template`.

Primary areas of interest right now:
- Topology-aware policy ([cmd/plugins/topology-aware/](cmd/plugins/topology-aware/), including its policy code at [cmd/plugins/topology-aware/policy/](cmd/plugins/topology-aware/policy/)).
- Balloons policy ([cmd/plugins/balloons/](cmd/plugins/balloons/), particularly its `preferCloseToDevices` + PodResources API integration — recently added).
- Resource manager plumbing ([pkg/resmgr/](pkg/resmgr/)), cache ([pkg/resmgr/cache/](pkg/resmgr/cache/)), libmem allocator ([pkg/resmgr/lib/memory/](pkg/resmgr/lib/memory/)), sysfs ([pkg/sysfs/](pkg/sysfs/)).
- PCT allocator ([pkg/resmgr/cpuclass/internal/pct/](pkg/resmgr/cpuclass/internal/pct/)) — Intel Speed Select (SST-CP + SST-TF) integration driven by `cpuClass` config.

## Ongoing initiatives

- **DRA integration for the topology-aware policy.** Background, KEP map, and reference-implementation notes: [docs/dra/landscape.md](docs/dra/landscape.md). Analysis of the earlier prototype: [docs/dra/pr-536-analysis.md](docs/dra/pr-536-analysis.md). Design draft (full cpuClass surface, topology-aware in v1, balloons in v2): [docs/dra/design.md](docs/dra/design.md). Implementation plan: [docs/dra/plan.md](docs/dra/plan.md).

Read the linked docs when the current task touches these areas. They are not auto-loaded.

## Session hygiene

- Loaded automatically at session start. No manual command needed.
- Keep this file short — nri-plugins-specific context only. External / ecosystem knowledge (Kubernetes KEPs, other projects' code) belongs under `docs/`.
- When a design decision lands, update the relevant `docs/dra/*.md` in place. Add pointers here only for new initiatives or new docs.
- Do NOT commit half-baked design notes as "decisions" — mark speculation as such in the linked docs.
