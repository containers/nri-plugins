# End-to-End tests

## Prerequisites

Install:

- `docker`
- `vagrant`

## Usage

Run policy tests:

```bash
cd test/e2e
[VAR=VALUE...] ./run_tests.sh policies.test-suite
```

Run tests only on certain policy, topology, or only selected test:

```bash
cd test/e2e
[VAR=VALUE...] ./run_tests.sh policies.test-suite[/POLICY[/TOPOLOGY[/testNN-*]]]
```

Get help on available `VAR=VALUE`'s with `./run.sh help`.
`run_tests.sh` calls `run.sh` in order to execute selected tests.
Therefore the same `VAR=VALUE` definitions apply both scripts.

## Test phases

In the *setup phase* `run.sh` creates a virtual machine unless it
already exists. When it is running, tests create a single-node cluster
and deploy `nri-resource-policy` DaemonSet on it.

In the *test phase* `run.sh` runs a test script. *Test scripts* are
`bash` scripts that can use helper functions for running commands and
observing the status of the virtual machine and software running on it.

In the *tear down phase* `run.sh` copies logs from the virtual machine
and finally stops or deletes the virtual machine, if that is wanted.

## Test modes

- `test` mode runs fast and reports `Test verdict: PASS` or
  `FAIL`. The exit status is zero if and only if a test passed.

Currently only the normal test mode is supported.

## Running from scratch and quick rerun in existing virtual machine

The test will use `vagrant`-managed virtual machine named in the
`vm_name` environment variable. The default name is constructed
from used topology, Linux distribution and runtime name.
If a virtual machine already exists, the test will be run on it.
Otherwise the test will create a virtual machine from scratch.
You can delete a virtual machine by going to the VM directory and
giving the command `make destroy`.

## Custom topologies

If you change NUMA node topology of an existing virtual machine, you
must delete the virtual machine first. Otherwise the `topology` variable
is ignored and the test will run in the existing NUMA
configuration.

The `topology` variable is a JSON array of objects. Each object
defines one or more NUMA nodes. Keys in objects:

```text
"mem"                 mem (RAM) size on each NUMA node in this group.
                      The default is "0G".
"nvmem"               nvmem (non-volatile RAM) size on each NUMA node
                      in this group. The default is "0G".
"cores"               number of CPU cores on each NUMA node in this group.
                      The default is 0.
"threads"             number of threads on each CPU core.
                      The default is 2.
"nodes"               number of NUMA nodes on each die.
                      The default is 1.
"dies"                number of dies on each package.
                      The default is 1.
"packages"            number of packages.
                      The default is 1.
```

Example:

Run the test in a VM with two NUMA nodes. There are 4 CPUs (two cores, two
threads per core by default) and 4G RAM in each node

```bash
e2e$ vm_name=my2x4 topology='[{"mem":"4G","cores":2,"nodes":2}]' ./run.sh
```

Run the test in a VM with 32 CPUs in total: there are two packages
(sockets) in the system, each containing two dies. Each die containing
two NUMA nodes, each node containing 2 CPU cores, each core containing
two threads. And with a NUMA node with 16G of non-volatile memory
(NVRAM) but no CPUs.

```bash
e2e$ vm_name=mynvram topology='[{"mem":"4G","cores":2,"nodes":2,"dies":2,"packages":2},{"nvmem":"16G"}]' ./run.sh
```

## Test output

All test output is saved under the directory in the environment
variable `outdir` if the `run.sh` script is executed as is. The default
output directory in this case is `./output`.

For the standard e2e-tests run by `run_tests.sh`, the output directory
is constructed from used Linux distribution, container runtime name and
the used machine topology.
For example `n4c16-generic-fedora37-containerd` output directory would
indicate four node and 16 CPU system, running with Fedora 37 and having
containerd as a container runtime.

Executed commands with their output, exit status and timestamps are
saved under the `output/commands` directory.
