# TODO

## Reference Plugins (this repository)

- admin/repo
  - [ ] figure out resonable name for repo (`nri-plugins` ?)
  - [ ] get repository on some neutral ground (`containerd` organization)
- cache
  - [ ] rework/split up, make the core functionality be usable for other types of plugins
  - [ ] get rid of CRI-specific representation/interfaces, NRI/CRI pod, container conversion
  - [ ] eliminate need for saving cache to disk
- config
  - [ ] (finish readding) config via agent
  - [ ] fallback config via ConfigMap (bind-mounted in deployment file)
  - [ ] switch to using CRDs (from ConfigMaps)
  - [ ] consider usig 1 CRD per policy as opposed to a single 'union' CRD
  - [ ] remove config-like external adjustment via CRD support
- policies
  - [ ] remove support for multiple policies in a single binary
  - [ ] balloons: implement topology hint support
  - [ ] balloons: make sure (just) enough permissions for cpufreq control from within containers
  - [ ] balloons: implement node resource topology export
  - [ ] topology-aware: legacy block I/O, RDT support if needed
  - [ ] topology-aware: cleanup/refactor (rewrite nodes, supply, request, grant)
- misc/infra/other
  - [x] drop AVX512 related bits
  - [ ] rework pkg hierarchy (with co-hosted plugins of other 'classes' in mind)
  - [ ] eliminate/replace `resmgr` in other user-visible 'artifacts' where appropriate
  - [ ] health check (with support for components to hook themselves into it)
  - [ ] check and unify annotation naming for consistency
  - [ ] structural logging (with better configurability), check what Patrik did
  - [ ] agent usage should be optional and controllable
- testing
  - [ ] more unit tests
  - [ ] unit tests in GH actions
  - [ ] e2e-tests on merge by CI/self-hosted runners
- documentation
  - [ ] minimal README with instructions about how to build/deploy/try the plugins


## NRI Core (https://github.com/containerd/nri)

- [ ] change default socket path to `/var/run/nri/nri.sock` (to allow reconnect from container)
- [ ] change socket permissions to `0700`
- [ ] get rid of NRI config file
- [ ] replace config file `disableConnections` with `WithDisableExternalConnections()`
- [ ] Darwin support
- [ ] Windows support
- [ ] add warning to documentation about socket 'access security'
- [ ] openshift: (manual) bootstrapping of enabling NRI support
- [ ] check if runtime-side extra adaptation code could be generalized and moved to NRI
- [ ] less protocol stutter:
    - [ ] add minimal pod/container cache to stub
    - [ ] replace pod/container ID in most protocol messages


## Containerd integration

- [ ] sbserver support


## CRI-O integration
