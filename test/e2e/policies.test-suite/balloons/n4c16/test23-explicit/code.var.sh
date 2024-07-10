cleanup() {
    vm-command "kubectl delete pod --all --now"
    vm-command "kubectl delete namespace $ns --now"
}

ns=explicit
cleanup

helm-terminate
helm_config=${TEST_DIR}/balloons-isolcpus.cfg helm-launch balloons
vm-command "kubectl create namespace $ns"

# pod0: should not run on non-explicit CPUs
CONTCOUNT=1 CPU=1 namespace="default" create balloons-busybox
report allowed
verify "set.union(cpus['pod0c0']).isdisjoint({'cpu00', 'cpu01', 'cpu02', 'cpu03'})"

# pod1: runs on user preferred CPUs
CONTCOUNT=2 CPU=1 namespace="$ns" create balloons-busybox
report allowed
verify "cpus['pod1c0'] == {'cpu00', 'cpu01'}" 
verify "cpus['pod1c1'] == {'cpu00', 'cpu01'}"

cleanup
helm-terminate
