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
  - [ ] figure out resonable name for repo (`nri-plugins` ?) C1
  - [ ] get repository on some neutral ground (`containerd` organization) C1
- cache
  - [ ] rework/split up, make the core functionality be usable for other types of plugins, C4
  - [ ] get rid of CRI-specific representation/interfaces, NRI/CRI pod, container conversion, C4
  - [ ] eliminate need for saving cache to disk, C2
- config
  - [x] (finish readding) config via agent
  - [x] fallback config via ConfigMap (bind-mounted in deployment file)
  - [ ] switch to using CRDs (from ConfigMaps), C2
  - [ ] consider usig 1 CRD per policy as opposed to a single 'union' CRD, C2
  - [x] remove config-like external adjustment via CRD support
- policies
  - [ ] remove support for multiple policies in a single binary, C1
  - [ ] balloons: implement topology hint support, C2
  - [ ] balloons: make sure (just) enough permissions for cpufreq control from within containers, C2
  - [ ] balloons: implement node resource topology export, C4
  - [ ] topology-aware: legacy block I/O, RDT support if needed, C2
  - [ ] topology-aware: cleanup/refactor (rewrite nodes, supply, request, grant), C4
- misc/infra/other
  - [x] drop AVX512 related bits
  - [ ] rework pkg hierarchy (with co-hosted plugins of other 'classes' in mind), C8
  - [ ] eliminate/replace `resmgr` in other user-visible 'artifacts' where appropriate, C1
  - [ ] health check (with support for components to hook themselves into it), C2
  - [ ] check and unify annotation naming for consistency, C1
  - [ ] structural logging (with better configurability), check what Patrik did, C2
  - [ ] agent usage should be optional and controllable, C2
- instrumentation
  - [ ] switch metrics collection to opentelemetry from opencensus
  - [ ] make sure default go runtime metrics gets properly exported
  - [ ] export policy-agnostic metrics data (maybe same as for node resource topology)
  - [ ] (maybe) remove any resulting duplicated data export from policy-specific data
- testing
  - [ ] more unit tests, C4
  - [ ] unit tests in GH actions, C4
  - [ ] e2e-tests on merge by CI/self-hosted runners, C4
  - [ ] migrate shell based e2e-tests into Ansible, C4
- documentation
  - [ ] minimal README with instructions about how to build/deploy/try the plugins, C2


## NRI Core (https://github.com/containerd/nri)

- [ ] change default socket path to `/var/run/nri/nri.sock` (to allow reconnect from container), C1
- [ ] change socket permissions to `0700`, C1
- [ ] get rid of NRI config file, C1
- [ ] replace config file `disableConnections` with `WithDisableExternalConnections()`, C1
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

- [ ] sbserver support, C2


## CRI-O integration
