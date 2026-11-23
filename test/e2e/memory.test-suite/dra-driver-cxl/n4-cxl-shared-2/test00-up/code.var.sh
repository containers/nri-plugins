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
#     ./run_tests.sh memory.test-suite/dra-driver-cxl/n4-cxl-shared-1/test00-up
#
# Then make the Kubernetes cluster of the tests of n4-cxl-shared-2, three
# nodes: this VM (VM2) the control plane, the VMs of n4-cxl-shared-1 (VM1)
# and n4-cxl-shared-0 (VM0) workers, pods on every node (no control-plane
# taint), over their VM network "cxl-pool" (vm_network.var.sh). Create VM1
# and VM0 first with test00-up of n4-cxl-shared-1 and n4-cxl-shared-0. A
# cluster in this shape is left alone, otherwise it is rebuilt, which takes
# minutes. This is the only test that changes the shape of a cluster: the
# other tests of n4-cxl-shared-2 only check it (pool-cluster-require) and
# fail at once if it is not there.

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

dra-vm-check

echo "### the cluster: VM2 (this VM) control plane, VM1 and VM0 workers"
VM0="$POOL_VM0_DIR"
VM1="$POOL_VM1_DIR"
VM2="$OUTPUT_DIR"
vmdir-up "$VM1" "create VM1 first: ./run_tests.sh memory.test-suite/dra-driver-cxl/n4-cxl-shared-1/test00-up"
vmdir-up "$VM0" "create VM0 first: ./run_tests.sh memory.test-suite/dra-driver-cxl/n4-cxl-shared-0/test00-up"
# The pool controllers of earlier tests are systemd units of VM2 that
# use the cluster: they go before the cluster may be rebuilt.
vm-command "systemctl stop '$DRA_CONTROLLER_UNITS_GLOB' 2>/dev/null; systemctl reset-failed '$DRA_CONTROLLER_UNITS_GLOB' 2>/dev/null; true" ||
    command-error "cannot stop the pool controllers"
vm-k8s-reinit "$VM2 $VM1 $VM0"
vm-command "kubectl get nodes -o wide" || command-error "cannot list the nodes of the cluster"
