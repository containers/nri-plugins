reboot-node() {
    timeout=600 vm-reboot
}

restart-kubelet() {
    vm-command "systemctl restart kubelet"
    sleep 5
    vm-wait-process --timeout 120 kube-apiserver
    vm-command "cilium status --wait --wait-duration=120s --interactive=false"
}

cleanup-pods() {
    vm-command "kubectl delete pod --all --now"
    vm-command "kubectl delete namespace $ns --now"
}

cleanup() {
    cleanup-pods
    if [[ "$distro" == *fedora* ]]; then
        fedora-set-kernel-cmdline ""
    else
        ubuntu-set-kernel-cmdline ""
    fi
    reboot-node
    vm-command "grep -v isolcpus /proc/cmdline"
    if [ $? -ne 0 ]; then
        error "failed to unset isolcpus kernel commandline parameter"
    fi
    restart-kubelet
    return 0
}

ns=isolcpus
cleanup-pods

vm-command "grep isolcpus=0,1 /proc/cmdline" || {
    if [[ "$distro" == *fedora* ]]; then
        fedora-set-kernel-cmdline "isolcpus=0,1"
    else
        ubuntu-set-kernel-cmdline "isolcpus=0,1"
    fi
    reboot-node
    vm-command "grep isolcpus=0,1 /proc/cmdline" || {
        error "failed to set isolcpus kernel commandline parameter"
    }
    restart-kubelet
}

helm-terminate
helm_config=${TEST_DIR}/balloons-isolcpus.cfg helm-launch balloons
vm-command "kubectl create namespace $ns"

# pod0: should run on non-isolated CPUs
CONTCOUNT=2 namespace="default" create balloons-busybox
report allowed
verify "set.union(cpus['pod0c0'], cpus['pod0c1']).isdisjoint({'cpu00', 'cpu01'})"

# pod1: runs on system isolated CPUs
CONTCOUNT=2 namespace="$ns" create balloons-busybox
report allowed
verify "cpus['pod1c0'] == {'cpu00', 'cpu01'}" 
verify "cpus['pod1c1'] == {'cpu00', 'cpu01'}"

cleanup
helm-terminate
