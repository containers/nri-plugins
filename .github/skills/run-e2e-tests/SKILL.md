---
name: run-e2e-tests
description: "Use when running NRI resource policy end-to-end tests. Covers the test runner, test topology selection, running specific test suites, understanding the test directory structure, debugging, and troubleshooting."
---

# Run NRI Resource Policy E2E Tests

End-to-end tests run policy plugins inside QEMU virtual machines with simulated CPU topologies. Tests verify CPU allocation, C-state control, frequency settings, and container scheduling behaviors.

## Running Tests

```bash
cd test/e2e
./run_tests.sh <test-pattern>
```

Never run tests in parallel. Tests often use the same virtual machine
instance. Starting next test before previous has exited is likely to
cause conflicts and unnecessary failures.

### Examples

Run a specific test:
```bash
./run_tests.sh policies.test-suite/balloons/n4c16/test17-cstates-scheduling
```

Run all balloons tests for a topology:
```bash
./run_tests.sh policies.test-suite/balloons/n4c16/
```

Run all topology-aware tests:
```bash
./run_tests.sh policies.test-suite/topology-aware
```

## Test Directory Structure

```
test/e2e/
├── run_tests.sh                        # Main test runner
├── run.sh                              # Single test runner called by the main runner
├── lib/                                # Bash function libraries used from runners
├── policies.test-suite/                # Policy tests in <policy>/<topology>/<test> structure
│   ├── balloons/                       # Balloons policy tests
│   │   ├── balloons-config.yaml.in     # Balloons config template (shared across tests)
│   │   ├── n4c16/                      # 4-package, 16-CPU topology
│   │   │   ├── test00-basic-placement/
│   │   │   │   └── code.var.sh         # Test script
│   │   │   ├── test17-cstates-scheduling/
│   │   │   │   ├── code.var.sh         # Test script
│   │   │   │   └── balloons-cstates.cfg # Test-specific balloons config
│   │   │   └── ...
│   │   └── s8c4k/                       # 8-socket 4096-CPU topology
│   └── topology-aware/
│       └── n4c16/
└── <topology>-<os>-<runtime>/          # Output directory created by test runner
    ├── .ssh-config                     # Login to the test vm and execute commands in it with: ssh -F n4c16-fedora-43-containerd/.ssh-config vagrant@node "<COMMAND>"
    └── policies.test-suite/
        └── balloons/
            └── test17-cstates-scheduling/ # Output from previously executed test
                ├── nri-resource-policy.output.txt  # NRI policy plugin logs from the test
                └── ...
```

## Test Configuration

Tests use a layered configuration:
1. `balloons-config.yaml.in` — base template with `${VARIABLE}` substitution.
   `*.in` templates are allowed to use bash scripts `$(SCRIPTS)`, they are
   instantiated simply with `eval "echo -e \"(<TEMPLATE_FILE)\""`.
2. `*.source.sh` files contain policy/topology/test specific helper functions.
3. `*.var.sh` files contain policy/topology/test specific environment variables.
   Test code itself is specified in `code.var.sh`.
4. Test-specific `.cfg` files — override specific settings per test
5. Environment variables — control test behavior (e.g., `OVERRIDE_SYS_CSTATES`)

## Test Output

After running, outputs appear in:
```
test/e2e/<topology>-<os>-<runtime>/policies.test-suite/<policy>/<test-name>/
```

For example:
```
test/e2e/n4c16-fedora-43-containerd/policies.test-suite/balloons/test17-cstates-scheduling/
```

Key output files:
- `nri-resource-policy.output.txt` — Full plugin log output (most important for debugging)
- Test verdict printed at end of `run_tests.sh` output

## Test Verdicts

```
Tests summary:
PASS balloons/n4c16/test17-cstates-scheduling
```

Or on failure:
```
Tests summary:
FAIL balloons/n4c16/test17-cstates-scheduling
```

## Environment Variables

| Variable | Purpose |
|----------|---------|
| `OVERRIDE_SYS_CACHES` | Override cache topology for testing |
| `OVERRIDE_SYS_CPUFREQ` | Override sysfs CPU frequencies (min,max,base) for testing |
| `OVERRIDE_SYS_CSTATES` | Override sysfs C-state paths for testing |
| `vm_name` | Name of the test VM |
| `topology` | CPU topology to simulate |
| `qemu_bin` | Absolute path of the qemu binary to run the VM in, instead of `qemu-system-x86_64` in PATH. Used only when the VM is created: it is recorded as `QEMU_BIN` in `<output-dir>/env`, which every later `vagrant up` of the VM reads. To change the qemu of an existing VM, edit `QEMU_BIN` there and restart the VM (`vagrant halt; vagrant up --no-provision` in the output dir) |

## Debugging

If a test fails, NRI resource policy plugin and test pods are left running on the vm for debugging.

Login or execute commands on the vm with .ssh-config file and vagrant@node as user@host in the test output directory. Example:
```
ssh -F test/e2e/n4c16-fedora-43-containerd/.ssh-config vagrant@node "kubectl get pods -A"
```

Qemu of the vm has monitor sockets in the output directory: `monitor.sock`
(human monitor, `vm-monitor` in tests), `qmp-e2e.sock` (QMP, `vm-qmp` in tests)
and `qmp.sock` (QMP, for other tools). Older VMs have only `monitor.sock`. Example:
```
cd test/e2e/n4c16-fedora-43-containerd && echo "info qtree -b" | socat STDIO unix-connect:monitor.sock
```

## Prerequisites

- QEMU, ansible and vagrant installed
- Container images built (see /build-nri-policy-images skill)
- SSH access configured for test VMs (auto-managed by test framework)
- Sufficient disk space for VM images

## Troubleshooting

- New policy configuration options are rejected. Likely reason: "helm
  uninstall" has left old CRDs behind in the test vm, and they are not
  replaced by new CRDs on "helm install" of new policy version. How to
  fix: run on the vm:
  ```
  kubectl delete crd balloonspolicies.config.nri noderesourcetopologies.topology.node.k8s.io
  ```
