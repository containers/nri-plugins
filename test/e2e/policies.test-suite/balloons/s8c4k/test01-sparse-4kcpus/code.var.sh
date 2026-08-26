# Prepare virtual machine before installing the policy.
require-kernel-version 6.14 "hot-plugged CPU node topology may not work"

# Hot-plug core 511 of sockets 0, 2 and 7. With 512 cores per socket, those
# cores are cpu511, cpu1535 and cpu4095.
vm-cpus-enabled 511 1535 4095 || {
    vm-cpu-hotplug 0 511 0
    vm-cpu-hotplug 2 511 0
    vm-cpu-hotplug 7 511 0

    vm-wait-cpus 511 1535 4095
    vm-online-all-cpus

    # Restart kubelet to let it detect the new enabled CPUs.
    vm-restart-kubelet
}

# Wait until kubelet has reported all enabled CPUs in node capacity.
wait-node-resource cpu 6 "kubelet did not report all enabled CPUs in node capacity"

# Make sure that k8s root cpuset.cpus contains hot-plugged CPUs.
verify-kubepods-cpus 511 1535 4095

# Install balloons
relaunch-policy balloons "$TEST_DIR/balloons-sparse-4kcpus.cfg"

# Verify NRT
nrt-verify-zone-resource "reserved[0]" "cpu" "capacity" "6"
nrt-verify-zone-resource "reserved[0]" "cpu" "allocatable" "5"
nrt-verify-zone-resource "pkg7[0]" "cpu" "capacity" "6"
nrt-verify-zone-resource "pkg7[0]" "cpu" "allocatable" "1"
nrt-verify-zone-attribute "pkg7[0]" "cpuset" "4095"

CPUREQ="500m" CPULIM="" MEMREQ=50M MEMLIM=""
ANN0="balloon.balloons.resource-policy.nri.io/container.pod0c0: pkg0"
ANN1="balloon.balloons.resource-policy.nri.io/container.pod0c1: pkg2"
ANN2="balloon.balloons.resource-policy.nri.io/container.pod0c2: pkg7"
CONTCOUNT=3 create besteffort
report allowed
verify 'cpus["pod0c0"] == {"cpu0511","cpu0002","cpu0000"}' \
       'cpus["pod0c1"] == {"cpu1535"}' \
       'cpus["pod0c2"] == {"cpu4095"}'
