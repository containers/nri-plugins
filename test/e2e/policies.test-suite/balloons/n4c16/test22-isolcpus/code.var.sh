cleanup-pods() {
    delete-pods --all
    delete-namespaces "$ns"
}

cleanup() {
    cleanup-pods
    clear-isolcpus
}

ns=isolcpus
cleanup-pods

vm-command "grep isolcpus=0,1 /proc/cmdline" || vm-set-kernel-cmdline-reboot "isolcpus=0,1"

helm-terminate
helm_config=${TEST_DIR}/balloons-isolcpus.cfg helm-launch balloons
create-namespaces "$ns"

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
