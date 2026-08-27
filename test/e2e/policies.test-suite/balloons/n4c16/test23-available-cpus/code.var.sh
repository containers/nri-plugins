cleanup() {
    delete-pods -n kube-system pod0
    delete-namespaces reserved
}

cleanup
create-namespaces reserved

helm-terminate
helm_config=${TEST_DIR}/balloons-excluded-cpusets.cfg helm-launch balloons

# pod0: run on reserved CPUs
CPUREQ="50m" CPULIM="" namespace=kube-system CONTCOUNT=1 create balloons-busybox
report allowed
verify 'cpus["pod0c0"].issubset({"cpu02", "cpu03"})'

# pod1: run in namespace with reserved CPUs
CPUREQ="50m" CPULIM="" namespace=reserved CONTCOUNT=1 create balloons-busybox
report allowed
verify 'cpus["pod1c0"].issubset({"cpu02", "cpu03"})'

cleanup
