# Test that AvailableResources are honored.

# Test explicit cpuset in AvailableResources.CPU
terminate nri-resource-policy
AVAILABLE_CPU="cpuset:4-7,8-11"
nri_resource_policy_cfg=$(instantiate nri-resource-policy-available-resources.cfg)
launch nri-resource-policy

# pod0: exclusive CPUs
CPU=3 create guaranteed
verify "cpus['pod0c0'] == {'cpu04', 'cpu05', 'cpu06'}" \
       "mems['pod0c0'] == {'node1'}"

# pod1: shared CPUs
CONTCOUNT=2 CPU=980m create guaranteed
verify "cpus['pod1c0'] == {'cpu08', 'cpu09', 'cpu10'}" \
       "cpus['pod1c1'] == {'cpu08', 'cpu09', 'cpu10'}" \
       "mems['pod1c0'] == {'node2'}" \
       "mems['pod1c1'] == {'node2'}"
vm-command "kubectl delete pods --all --now"
reset counters

# Test cgroup cpuset directory in AvailableResources.CPU

test-and-verify-allowed() {
    # pod0: shared CPUs
    CONTCOUNT=2 CPU=980m create guaranteed
    report allowed
    verify "cpus['pod0c0'] == {'cpu0$1', 'cpu0$2', 'cpu0$3'}" \
           "cpus['pod0c1'] == {'cpu0$4'}"

    # pod1: exclusive CPU
    CPU=1 create guaranteed
    report allowed
    verify "disjoint_sets(cpus['pod1c0'], cpus['pod0c0'])" \
           "disjoint_sets(cpus['pod1c0'], cpus['pod0c1'])"

    vm-command "kubectl delete pods --all --now"
    reset counters
}

if [ -z "$VM_NRI_SYSTEMD" ]; then
    # When nri-rm is run in a pod
    NRIRM_SYS_PATH="/host"
else
    NRIRM_SYS_PATH=""
fi

if vm-command "[ -d /sys/fs/cgroup/cpuset ]"; then
    # cgroup v1
    CGROUP_CPUSET=/sys/fs/cgroup/cpuset
else
    # cgroup v2
    CGROUP_CPUSET=/sys/fs/cgroup
fi
NRIRM_CGROUP=$CGROUP_CPUSET/nri-resource-policy-test-05-1
vm-command "rmdir $NRIRM_CGROUP; mkdir $NRIRM_CGROUP; echo 1-4,11 > $NRIRM_CGROUP/cpuset.cpus"

terminate nri-resource-policy
AVAILABLE_CPU="\"${NRIRM_SYS_PATH}$NRIRM_CGROUP\""
nri_resource_policy_cfg=$(instantiate nri-resource-policy-available-resources.cfg)
launch nri-resource-policy
test-and-verify-allowed 1 2 3 4
vm-command "rmdir $NRIRM_CGROUP || true"

NRIRM_CGROUP=$CGROUP_CPUSET/nri-resource-policy-test-05-2
vm-command "rmdir $NRIRM_CGROUP; mkdir $NRIRM_CGROUP; echo 5-8,11 > $NRIRM_CGROUP/cpuset.cpus"

terminate nri-resource-policy
AVAILABLE_CPU="\"${NRIRM_SYS_PATH}${NRIRM_CGROUP}\""
nri_resource_policy_cfg=$(instantiate nri-resource-policy-available-resources.cfg)
launch nri-resource-policy
test-and-verify-allowed 5 6 7 8
vm-command "rmdir $NRIRM_CGROUP || true"

# cleanup, do not leave weirdly configured nri-resource-policy running
terminate nri-resource-policy
