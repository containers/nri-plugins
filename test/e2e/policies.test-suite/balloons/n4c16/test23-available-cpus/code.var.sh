cleanup() {
    vm-command \
        "kubectl -n kube-system delete pod pod0 --now && \
         kubectl -n reserved delete pod pod1 --now || true && \
         kubectl delete ns reserved --now"
}

cleanup
vm-command "kubectl create namespace reserved || true"

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
