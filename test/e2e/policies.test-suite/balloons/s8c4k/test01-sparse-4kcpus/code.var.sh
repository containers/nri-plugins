# Prepare virtual machine before installing balloons.
min_kernel_version=6.14
vm-command "uname -r"
if [ "$( ( echo $min_kernel_version; echo $COMMAND_OUTPUT ) | sort --version-sort | tail -n 1 )" == "$min_kernel_version" ]; then
    error "quest OS runs too old kernel, hot-plugged CPU node topology may not work. Required: $min_kernel_version"
fi

# Hot-plug CPUs.
vm-command 'grep 511,1535,4095 /sys/devices/system/cpu/enabled' || {
    vm-cpu-hotplug 0 511 0
    vm-cpu-hotplug 2 511 0
    vm-cpu-hotplug 7 511 0

    # Wait for the kernel to expose all hot-plugged CPUs in sysfs.
    vm-run-until '[ -d /sys/devices/system/cpu/cpu511 ] && [ -d /sys/devices/system/cpu/cpu1535 ] && [ -d /sys/devices/system/cpu/cpu4095 ]'

    # Online all CPUs.
    vm-command 'for cpuX in /sys/devices/system/cpu/cpu[1-9]*; do
            echo onlining $cpuX
            ( echo 1 > $cpuX/online && echo Successful: write 1 to $cpuX/online ) || echo Failed: write 1 to $cpuX/online
        done
       grep . /sys/devices/system/cpu/cpu[1-9]*/online'

    # Restart kubelet to let it detect new enabled CPUs.
    vm-command "systemctl restart kubelet"
}

# Wait until kubelet has reported all enabled CPUs in node capacity.
vm-run-until 'kubectl get node -o jsonpath="{.items[0].status.capacity.cpu}" | grep 6' ||
    command-error "Unexpected node CPU capacity"

# Make sure that k8s root cpuset.cpus contains hot-plugged CPUs.
vm-command "grep . /sys/fs/cgroup/kubepods*/cpuset.cpus"
if ! ( grep -q 511 <<< $COMMAND_OUTPUT &&
           grep -q 1535 <<< $COMMAND_OUTPUT &&
           grep -q 4095 <<< $COMMAND_OUTPUT ); then
    command-error "kubepods cpuset.cpus does not include expected CPUs"
fi

# Install balloons
helm-terminate
helm_config=$TEST_DIR/balloons-sparse-4kcpus.cfg helm-launch balloons

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
