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
# containerd:  kubepods/cpuset.cpus
# cri-o: kubepods.slice/cpuset.cpus
vm-command "grep . /sys/fs/cgroup/kubepods*/cpuset.cpus"
if ! ( grep -q 511 <<< $COMMAND_OUTPUT &&
           grep -q 1535 <<< $COMMAND_OUTPUT &&
           grep -q 4095 <<< $COMMAND_OUTPUT ); then
    command-error "kubepods cpuset.cpus does not include expected CPUs"
fi

# Install topology-aware
helm-terminate
helm_config=$(COLOCATE_PODS=true instantiate helm-config.yaml) helm-launch topology-aware

# socket #0 CPUs: 0,1,2,511
# socket #2 CPUs: 1535, cpuallocator takes this for reserved as it is a complete idle package
# socket #7 CPUs: 4095

# 5-CPU allocation must include at least one CPU id>1024.
CPU=5000m create guaranteed || {
    error "failed to create pod, possible cause: topology-aware not consuming all CPUs"
}

report allowed
if pp 'cpus["pod0c0"]' | grep 4095; then
    verify 'len(cpus["pod0c0"]) == 5'
else
    vm-command 'grep . /sys/fs/cgroup/$(grep . /proc/$(pgrep -f "echo pod0c0")/cgroup | awk -F::/ "{print \$2}")/cpuset.cpus.effective'
    if grep 4095 <<< "$COMMAND_OUTPUT"; then
        echo "RUNC BUG: pod0c0 cannot run on CPU 4095 yet the CPU is in cgroup cpuset.cpus.effective. See: https://github.com/opencontainers/runc/issues/5023"
        echo "Test verdict: SKIP (buggy runc)"
        exit 0
    else
        error "expected: pod0c0 is allowed to run on CPU 4095"
    fi
fi

# Release all 5 CPUs.
vm-command 'kubectl delete pod pod0 --now'
vm-run-until '! kubectl get pod pod0'

# Only socket #0 has enough CPUs for pod1.
CONTCOUNT=1 CPUREQ=3100m CPULIM=4000m create burstable
report allowed
verify 'len(packages["pod1c0"]) == 1 and len(cpus["pod1c0"]) == 4'

CONTCOUNT=1 CPUREQ=900m CPULIM=1 create burstable
report allowed
verify 'cpus["pod2c0"] == {"cpu4095"}'

CONTCOUNT=1 create besteffort
report allowed

vm-command 'kubectl delete pods pod1 pod2 pod3 --now'

# Verify no CPU leaks, allocate all again.
CPU=5000m create guaranteed
report allowed
verify 'len(cpus["pod4c0"]) == 5'

vm-command 'kubectl delete pods --all --now'
