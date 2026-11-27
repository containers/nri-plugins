# Bring up a VM for testing CXL memory that is shared by VMs or pooled with
# fake-cxl-pool (scripts/testing/fake-cxl-pool), and check that it is ready:
# - The CXL kernel and the cxl, daxctl and cxl-dump tools are installed.
#   The kernel has CONFIG_FS_DAX (devdax mmap, the way to share CXL memory),
#   CONFIG_EROFS_FS (erofs on shared CXL persistent memory) and DMI (the VM
#   sees its UUID in /sys/class/dmi/id/product_uuid).
# - Qemu answers on QMP, its host bridges have HDM decoders
#   (hdm_for_passthrough=on) for more than one region per host bridge, and
#   its cxl-downstream ports can hot-remove devices.
# - Qemu has the local CXL memory device of the topology (cxl_memdev0,
#   serial 0xc100e2e1), unplugged, and the empty pool slots for hotplugging
#   pool devices. Serials: 0xc100.... boot-time devices of one VM,
#   0xc1ae.... shared and 0xc1ee.... unshared pool devices.
# - The VM sees no CXL memory devices.
#
# dra-vm-check of dra.source.sh does these checks. The topologies
# n4-cxl-shared-0, n4-cxl-shared-1 and n4-cxl-shared-2 have the same pool
# slots, so their VMs can share the same CXL memory devices. They differ
# only in the size of the local device: 256M in n4-cxl-shared-0 and
# n4-cxl-shared-1, 1G in n4-cxl-shared-2. Hot-removing and hotplugging a device again needs a
# qemu that can hot-remove CXL memory devices (docs/memory/cxl/e2e.md,
# Prerequisites). Give it when creating the VM, for instance:
#   qemu_bin=/path/to/qemu-system-x86_64 \
#     ./run_tests.sh memory.test-suite/dra-driver-cxl/n4-cxl-shared-0/test00-up
#
# This VM (VM0) is the second worker of the Kubernetes cluster of the tests
# of n4-cxl-shared-2 once test00-up of n4-cxl-shared-2 has made it: this
# test checks only the VM and leaves its cluster as it is. Pool A, the pool
# server of most tests, is cabled to VM1 and VM2 only; test19-many-pools
# cables VM0 to pool B (and to pool C, which reaches every VM).

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

dra-vm-check

echo "### Kubernetes role of the VM, left as it is"
echo "$(basename "$OUTPUT_DIR"): $(vm-k8s-shape "$OUTPUT_DIR")"
