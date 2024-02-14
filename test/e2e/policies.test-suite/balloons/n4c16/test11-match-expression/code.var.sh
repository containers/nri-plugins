# Test placing containers with and without annotations to correct balloons
# reserved and shared CPUs.

helm-terminate
helm_config=${TEST_DIR}/../../match-config.yaml helm-launch balloons

cleanup() {
    vm-command "kubectl delete pods --all --now"
    return 0
}

cleanup

# pod0: run precious workload
POD_LABEL="app.kubernetes.io/component: precious" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod0c0"]) == 2'

# pod1: run ordinary workload
CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod1c0"]) == 1'

cleanup
