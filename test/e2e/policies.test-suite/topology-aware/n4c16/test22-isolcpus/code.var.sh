# Test allocation of system isolated CPUs. The node is booted with cpu01
# and cpu02 isolated, both on NUMA node 0, which leaves cpu00 of that node
# for the reserved pool and cpu03 for the shared pool.
#

cleanup() {
    delete-pods --all
    helm-terminate || :
    clear-isolcpus
}

# Note: we hook cleanup to EXIT, so a failed test restores the VM to
# the state subsequent tests expect (no isolated CPUs) to run on.

cleanup
vm-command "grep isolcpus=1,2 /proc/cmdline" || vm-set-kernel-cmdline-reboot "isolcpus=1,2"
trap cleanup EXIT

# Pin the reserved pool to a non-isolated cpu00.
helm_config=$(RESERVED_CPU="cpuset:0" instantiate helm-config.yaml) helm-launch topology-aware

# A container which prefers isolated CPUs gets one.
ANN0='prefer-isolated-cpus.resource-policy.nri.io/container.pod0c0: "true"' \
    CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify 'cpus["pod0c0"].issubset({"cpu01", "cpu02"})'

# The second one gets the other isolated CPU.
ANN0='prefer-isolated-cpus.resource-policy.nri.io/container.pod1c0: "true"' \
    CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify 'cpus["pod1c0"].issubset({"cpu01", "cpu02"})' \
       'disjoint_sets(cpus["pod0c0"], cpus["pod1c0"])'

# There is no isolated CPU left for a third one, so it falls back to
# slicing off ordinary CPUs for exclusive use, since the preference
# is not a required strict one.
ANN0='prefer-isolated-cpus.resource-policy.nri.io/container.pod2c0: "true"' \
    CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify 'cpus["pod2c0"].isdisjoint({"cpu01", "cpu02"})' \
       'len(cpus["pod2c0"]) == 1'

# A container which requires isolated CPUs instead of preferring them is
# refused once there are none left.
ANN0='require-isolated-cpus.resource-policy.nri.io/container.pod3c0: "true"' \
    wait="" CONTCOUNT=1 CPU=1 MEM=100M create guaranteed

PodReadyCond='condition=PodReadyToStartContainers'
vm-command "kubectl wait --timeout=5s pod pod3 --for=$PodReadyCond" || {
    error "failed to wait for pod3 to start containerd"
}
verify-container-error pod3 0 "isolated CPUs"

cleanup
trap - EXIT
