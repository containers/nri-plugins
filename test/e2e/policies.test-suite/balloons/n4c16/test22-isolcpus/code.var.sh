ns=isolcpus

reboot-node() {
    vm-reboot
    timeout=120 host-wait-vm-ssh-server
}

restart-kubelet() {
    vm-command "systemctl restart kubelet"
    sleep 5
    vm-wait-process --timeout 120 kube-apiserver
    vm-command "cilium status --wait --wait-duration=120s --interactive=false"
}

vm-command "grep isolcpus=0,1 /proc/cmdline" || {
    if [[ "$distro" == *fedora* ]]; then
        fedora-set-kernel-cmdline "isolcpus=0,1"
    else
        ubuntu-set-kernel-cmdline "isolcpus=0,1"
    fi
    vm-reboot
    vm-command "grep isolcpus=0,1 /proc/cmdline" || {
        error "failed to set isolcpus kernel commandline parameter"
    }
    restart-kubelet
}

helm-terminate
helm_config=${TEST_DIR}/balloons-isolcpus.cfg helm-launch balloons
vm-command "kubectl create namespace $ns"

# pod0: runs on system isolated CPUs
CONTCOUNT=2 namespace="$ns" create balloons-busybox
report allowed
verify "cpus['pod0c0'] == {'cpu00', 'cpu01'}"

# pod1: should run on non-isolated CPUs
CONTCOUNT=1 namespace="$ns" create balloons-busybox
report allowed
verify "cpus['pod1c0'] != {'cpu00', 'cpu01'}"

cleanup() {
    vm-command "kubectl delete namespace $ns"
    if [[ "$distro" == *fedora* ]]; then
        fedora-set-kernel-cmdline ""
    else
        ubuntu-set-kernel-cmdline ""
    fi
    reboot-node
    vm-command "grep isolcpus= /proc/cmdline" || {
        error "failed to unset isolcpus kernel commandline parameter"
    }
}
cleanup
