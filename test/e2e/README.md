# NRI-resource-policy E2E tests

## Prerequisites
Before running E2E tests ensure that you have all the required components locally available as described below.

0. Install dependencies:
   - `vagrant`
   - `qemu-system-x86`

1. Build NRI resource policy static binaries. You need to be at the root of the NRI-resource-policy directory.

    ```shell
    make build
    ```

2. Build the container image. You need to be at the root of the NRI-resource-policy directory.

    ```shell
    make images
    ```

3. Build containerd binaries that include NRI support (minimum tag version [`containerd 1.7.0-beta.1`](https://github.com/containerd/containerd/releases/tag/v1.7.0-beta.1) or +)

    ```shell
    git clone https://github.com/containerd/containerd.git
    cd containerd
    make
    ```

4. Then run the tests.

    ```shell
    cd test/e2e
    ./run_tests.sh policies.test-suite
    ```

    The default test output directory name is generated from the used topology
    and runtime name and the directory is created under test/e2e.
    The test output directory can be given as a 2nd parameter to the script.

    ```shell
    ./run_tests.sh policies.test-suite ~/output-directory
    ```

    Note that Vagrant VM is stored into the output directory. If you want to
    remove the output directory, then please remove the Vagrant VM first like
    this:

    ```shell
    cd ~/output-directory && vagrant destroy -f
    ```

    The e2e test runs can be can be configured by setting various options as
    environment variables when starting the run_tests.sh script. These environment
    variables are passed to test VM so that it can establish connection to
    net to download packages etc.

    ```
    HTTP_PROXY
    HTTPS_PROXY
    NO_PROXY
    http_proxy
    https_proxy
    no_proxy
    dns_nameserver
    dns_search_domain
    k8scri
    ```

    For example:

    ```shell
    https_proxy=http://proxy.example.com dns_nameserver=8.8.8.8 dns_search_domain=example.com ./run_tests.sh policies.test-suite
    ```

5. You can login to the e2e test VM:

    ```shell
    cd ~/output-directory
    make ssh
    ```

6. While the e2e tests are running, you can monitor the status of the tests:

    ```shell
    cd ~/output-directory
    <nri-plugins-root-directory>/test/e2e/report-test-status.sh

    policies.test-suite balloons test01-basic-placement : PASS
    policies.test-suite balloons test02-prometheus-metrics : PASS
    policies.test-suite balloons test03-reserved : PASS
    policies.test-suite balloons test05-namespace : PASS
    policies.test-suite balloons test06-update-config : PASS
    policies.test-suite balloons test07-maxballoons : PASS
    policies.test-suite balloons test08-numa : PASS
    policies.test-suite balloons test09-isolated : PASS
    policies.test-suite balloons test10-health-checking : PASS
    ```

## Caching a provisioned VM

Most of the time a test run spends before the first test case goes into
provisioning the VM: installing Kubernetes, the container runtime, the CNI
plugin and Helm, and initializing a single-node cluster with `kubeadm`. The
result only depends on the versions installed, so it can be exported with
`vagrant package` and reused.

Set `e2e_vm_cache` to opt in:

```shell
e2e_vm_cache=yes ./run_tests.sh policies.test-suite
```

| value | meaning |
| --- | --- |
| `no` | do not use or create cached boxes (the default) |
| `yes` | use a cached box if there is one, otherwise provision and keep the result |
| `refresh` | ignore any cached box, provision from scratch, then replace it |
| `cleanup` | remove all cached boxes, printing each of them, and exit without running tests |
| `nuke`, `drop` | synonyms of `cleanup` |

The boxes live in `$CACHE_DIR/boxes`, next to the tarballs the framework
already caches, and are named after everything which shapes the guest: the
topology, the distro, the Kubernetes, runtime, CNI and Helm versions, and a
hash of the files which provisioning uses, listed in `BOX_RECIPE_FILES` in
`lib/vm.bash`. Editing any of them therefore invalidates the boxes instead of a
later run reusing an image which predates the change.

Worth knowing:

- The topology is part of the name. The hostname of the VM is derived from it,
  and `kubeadm` bakes the hostname into the name of the node, into the
  certificates and into etcd, so a box only serves the topology it was made
  for.

- A box is only used when the VM is created for the first time. An output
  directory which already has a `Vagrantfile` keeps the VM and the box it was
  created from, so start from a clean output directory to benefit from the
  cache.

- Expect a couple of gigabytes per box, and note that vagrant unpacks a box
  into `~/.vagrant.d/boxes` on first use, so the disk cost is roughly twice the
  file size. Nothing prunes them automatically, so clean up when done:

  ```shell
  e2e_vm_cache=cleanup ./run_tests.sh policies.test-suite
  ```

  This removes both halves of every cached box, the file and the copy vagrant
  unpacked, and leaves the downloaded distro images alone.

- Boxes expire after 30 days (`BOX_CACHE_DECAY`). They contain a cluster whose
  certificates the `kubeadm` defaults give a year to live, so they are not
  meant to be kept around indefinitely.

- `vagrant package` needs a recent enough `vagrant-qemu`. With an older one the
  framework says so and provisions from scratch.

- A VM which has just booted from a box has a cluster which is still starting
  up, so the framework waits for the node and for the cluster DNS before
  running any test. Raise `CLUSTER_READY_TIMEOUT` (300 seconds by default) if a
  large topology needs longer.

## Collecting coverage data

The tests can collect the same kind of coverage data from the plugins they
exercise that `go test -cover` collects from unit tests. This only works for
the resource-manager-based plugins, in other words balloons, topology-aware
and template.

Collection is always on, so there is nothing to enable:

```shell
make e2e-tests
```

This builds the plugins with coverage instrumentation, runs the tests, and ends
the run with the coverage of all the tests it ran, on top of the usual test
summary: the coverage of the logic of each plugin, in other words of the code
under `cmd/plugins/PLUGIN`, and the total over everything instrumented.

The only thing coverage data needs is instrumented plugins, which `make
e2e-tests` takes care of. When running the tests directly, build the images
with `COVER=1` to get anything to report on:

```shell
make COVER=1 images
cd test/e2e
./run_tests.sh policies.test-suite
```

Without that the tests run just fine, they simply have nothing to collect.

The data of each test case is stored in a `coverage` directory in the output
directory of that test case, so it also tells which test covers what. The
report is merged from all of those at the end of the run, and can be
regenerated, or generated for a subset of the tests, with:

```shell
./report-coverage.sh [DIR]
```

DIR defaults to the current directory, and the report is written to
`DIR/coverage-report`: `coverprofile` in the usual text format for `go tool
cover`, `coverage.html` for browsing, and `summary.json` with the numbers in it
for whoever reports on them further. Run `go tool cover -func` on the profile
for the numbers per package and per function. Note that the report covers all
data found under DIR, so point it at a single policy or topology directory for a
report on those tests alone.

The numbers themselves come from `cmd/e2e-report`, which works them out from the
profile:

```shell
go run ./cmd/e2e-report coverage [--tests N] [--summary FILE] PROFILE
```

The same tool reports on a whole test run, so the coverage of a run and the
coverage of the tests it consists of are always counted the same way:

```shell
go run ./cmd/e2e-report run RESULT_DIR
go run ./cmd/e2e-report index RESULT_ROOT
```

`run` writes `results.json`, `index.html` and `status.txt` for the results
collected into RESULT_DIR, and `index` rebuilds the index of every run under
RESULT_ROOT. This is what `scripts/testing/nightly/e2e-runner` publishes the
results of a nightly run with.

Worth knowing:

- An instrumented plugin dumps its data when it exits, and serves it on request
  over its instrumentation HTTP server. The framework uses both: it asks for a
  dump before terminating a plugin and again at the end of a test, and picks up
  what plugins which already exited wrote to `$GOCOVERDIR`. This is why the data
  survives a test which kills a plugin instead of terminating it, and a test
  which launches a plugin several times.

- Collecting the same data twice costs the percentages nothing: they only ask
  whether a block was entered at all. The hit counts do add up, though, and
  `coverage.html` shades a block by its count relative to the highest in the
  file, so read its colors as covered and not covered, not as a heat map.

- A plugin starts with its counters at zero, so the data of a test covers what
  that test's plugins did, the configuration it launched them with included.
  What keeps one test out of another's data is the collected data being
  discarded on the VM before each test. A plugin which a test leaves running is
  only terminated by the next test, so its final dump is counted for the latter.

- Data collected earlier is added to, not replaced, which is what makes a total
  over several partial runs possible. A run which reruns a test replaces the
  data of that test, but the data of the tests it does not run stays. Start the
  run from scratch with `reset_coverage`, and the report covers this run alone:

  ```shell
  reset_coverage=1 ./run_tests.sh policies.test-suite
  ```

  `1` enables it and `0` disables it, which is also the default; anything else
  is an error, rather than a value which quietly leaves the earlier data in
  place. It discards everything collected earlier once, before the first test,
  so it never throws away the data of the tests of the ongoing run.

- The endpoints, which the framework enables with
  `--set plugin.test.enableAPIs=true`, are also there for poking at by hand:

  | endpoint | serves |
  | --- | --- |
  | `/coverage/id` | the ID of the instrumented binary, which the names of the data files are based on |
  | `/coverage/meta` | the coverage meta-data, to be saved as `covmeta.<id>` |
  | `/coverage/counters` | a snapshot of the counters, to be saved as `covcounters.<id>.N.M` |
  | `/coverage/clear` | resets the counters |

- Only the plugins are instrumented, so the report covers the packages linked
  into a plugin, and nothing else. Do not read a total percentage over it as
  the coverage of the repository, and do not compare it to the unit test
  coverage of `make test`, which measures a different set of packages.

## Publishing and serving results

A run of the whole suite collects some 120 megabytes, most of it the command
transcripts and the plugin logs of each test, and most of that the same lines
over and over. Packed into a single archive it takes a couple of megabytes, so
nothing has to be thrown away to keep the results of months of nightly runs:

```shell
go run ./cmd/e2e-report pack RESULT_DIR
go run ./cmd/e2e-report serve [--address ADDR] RESULT_ROOT
```

`make e2e-report` builds the tool to `build/bin` for serving results without a
checkout to run it from; it is not part of any image.

`pack` writes `results.tar.zst` and removes what it packed, leaving behind the
report, `results.json`, `status.txt`, `summary.txt` and the git information: how
the run went is readable without unpacking anything. `serve` serves a result
root over HTTP, the packed runs as if their archives had been extracted where
they are, so the links of a report work whether the run it belongs to is packed
or not.

`scripts/testing/nightly/e2e-runner --pack-results` publishes a run this way,
and keeps the artifacts of every test and the coverage data of each as well:
with the results packed there is nothing to gain by pruning them. Packing is
off unless asked for, as a packed run needs the server to browse.
`tar --zstd -xf` gets the results of a run out without one.

A tarball is served both ways: without a trailing slash it is downloaded, with
one it is browsed into, and the files in it are served from it as if it had been
extracted. So the artifacts a test packed up can be read one file at a time
without downloading them, and the report links to them both ways:

```text
.../test01-tiny/artifacts.tar.xz              the tarball
.../test01-tiny/artifacts.tar.xz/             what is in it
.../test01-tiny/artifacts.tar.xz/commands/    the commands the test ran
```

The archive of a run browses the same way, and so does a tarball inside another
one, which is what the artifacts of a test are once the run is packed.

Worth knowing:

- Reading a file from an archive decompresses it up to that file, a few hundred
  milliseconds for the largest archive of a full run. The listing of an archive
  is kept, and a tarball inside one is read out whole and kept, so browsing the
  artifacts of a test unpacks the archive of the run once.

- Reading a `.tar.xz`, which is what the tests pack their artifacts with, takes
  the `xz` command. Without it the tarball is still served, it just cannot be
  served into. `.tar.zst`, `.tar.gz` and `.tar` need nothing.

- The server serves what is under the result root and nothing else: it refuses a
  path which leads out of it, and it serves nothing but the regular files of an
  archive.

- A packed run keeps the report it was packed with. Reporting on it again would
  find no results to report on, so `run` refuses, and `index` leaves it alone.

## Writing tests

A test case is a `code.var.sh` file in a
`TEST-SUITE/POLICY/TOPOLOGY/TEST/` directory. In addition to the script API of
`run.sh` (run `./run.sh help` to list it), test cases have shared helpers
available:

- `lib/test.bash` contains helpers that are useful for more than one test case:

  | group | helpers |
  | --- | --- |
  | waiting | `retry-until` |
  | pods and containers | `container-state`, `wait-container-waiting-reason`, `verify-container-error`, `wait-pod-gone` |
  | launching the plugin | `relaunch-policy`, `expect-launch-failure` |
  | cleaning up | `delete-pods`, `create-namespaces`, `delete-namespaces`, `remove-policy-cache`, `kill-test-processes` |
  | log of the plugin | `plugin-daemonset`, `plugin-log`, `plugin-log-tail`, `assert-log-contains`, `assert-log-not-contains`, `wait-assert-log-contains`, `wait-assert-log-grew`, `assert-cpu-clos`, `assert-cpu-freq` |
  | metrics | `verify-metrics-has-line`, `verify-metrics-has-no-line` |
  | node resource topology | `nrt-query`, `nrt-dump`, `nrt-verify-zone-attribute`, `nrt-verify-zone-resource` |
  | node state | `require-kernel-version`, `verify-kubepods-cpus`, `clear-isolcpus`, `disable-numa`, `enable-numa` |
  | CPU lists | `expand-cpulist`, `cpulist-difference`, `container-cpus`, `allowed-cpu-ids` |
  | extended resources | `get-node-resource`, `wait-node-resource` |
  | interrupts | `resolve-irq`, `irq-cpu-ids`, `verify-irq-cpus` |
  | scheduling | `verify-sched`, `SCHED_*` |

  Some of them are configured by variables a test case can set, notably
  `plugin_log_filter`, which restricts the log assertions to the log lines of
  the subsystem under test.

  Add a helper here once a second test case needs it. Run `./run.sh help` for
  the documentation of each of them.

- `TEST-SUITE/POLICY/*.source.sh` files contain policy-specific helpers.

`*.source.sh` files are sourced before the test case code, starting from the
test suite directory and ending in the test case directory, `lib/test.bash`
being the first one. Therefore a test case, or all test cases of a policy or a
topology, can override any shared helper simply by redefining it.
