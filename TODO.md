# TODO

The estimated complexity and priority of a feature/task is defined like this:

- Priority scale: High (1), Medium (2) and Low (3). This is marked as Px in the
  task description.

- Complexity scale: C1, C2, C4 and C8.
   The complexity scale is exponential, with complexity 1 being the
   lowest complexity. The estimated complexity is a function of both
   task 'complexity' and task 'scope'. If you think the complexity is >C8,
   then that is a good hint that the task needs to be split into smaller
   pieces.

## Reference Plugins (this repository)

- admin/repo
- cache
  - [ ] rework/split up, make the core functionality be usable for other types of plugins, C4
  - [ ] get rid of CRI-specific representation/interfaces, NRI/CRI pod, container conversion, C4
  - [ ] eliminate need for saving cache to disk, C2
- config
  - [ ] switch to using CRDs (from ConfigMaps), C2
  - [ ] consider using 1 CRD per policy as opposed to a single 'union' CRD, C2
  - [ ] create helm chart support for the resource policies, C1
- policies
  - [ ] remove support for multiple policies in a single binary, C1
  - [ ] balloons: implement topology hint support, C2
  - [ ] balloons: make sure (just) enough permissions for cpufreq control from within containers, C2
  - [ ] balloons: implement node resource topology export, C4
  - [ ] topology-aware: legacy block I/O, RDT support if needed, C2
  - [ ] topology-aware: cleanup/refactor (rewrite nodes, supply, request, grant), C4
- misc/infra/other
  - [ ] rework pkg hierarchy (with co-hosted plugins of other 'classes' in mind), C8
  - [ ] eliminate/replace `resmgr` in other user-visible 'artifacts' where appropriate, C1
  - [ ] check and unify annotation naming for consistency, C1
  - [ ] structural logging (with better configurability), check what Patrik did, C2
  - [ ] agent usage should be optional and controllable, C2
  - [ ] fix crun+cgroupv2 support (ineffective/broken for CPU and memory)
      - [ ] set cgroup parameters using v2/unified notation if possible
      - [ ] check if this fixes the problems with crun+cri-o
- instrumentation
  - [ ] switch metrics collection to opentelemetry from opencensus
  - [ ] make sure default go runtime metrics gets properly exported
  - [ ] export policy-agnostic metrics data (maybe same as for node resource topology)
  - [ ] (maybe) remove any resulting duplicated data export from policy-specific data
- testing
  - [ ] more unit tests, C4
  - [ ] unit tests in GH actions, C4
  - [ ] migrate shell based e2e-tests into Ansible, C4
  - [ ] make topology-aware e2e test07-mixed-allocations more reliable, C1
  - [ ] allow e2e tests to use the upstream containerd or cri-o instead of using
        binaries compiled from local sources, C1
- documentation
  - [ ] minimal README with instructions about how to build/deploy/try the plugins, C2


## NRI Core (https://github.com/containerd/nri)

- [ ] Darwin support, C4
- [ ] Windows support, C8
- [ ] add warning to documentation about socket 'access security', C1
- [ ] openshift: (manual) bootstrapping of enabling NRI support, C4
- [ ] check if runtime-side extra adaptation code could be generalized and moved to NRI, C4
- [ ] less protocol stutter:
    - [ ] add minimal pod/container cache to stub, C4
    - [ ] replace pod/container ID in most protocol messages, C1
- [ ] add trace spans in adaptation for request/event handling
- [ ] add trace spans to stub for request/event handling


## Containerd integration



## CRI-O integration
