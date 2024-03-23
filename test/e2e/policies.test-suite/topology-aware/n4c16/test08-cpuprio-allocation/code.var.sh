# Test that guaranteed and burstable pods get the CPUs they require
# when there are enough CPUs available.

# Override CPU type detection, to detect 2 low-power CPUs
helm-terminate
EXTRA_ENV_OVERRIDE_SYS_ATOM_CPUS="4-5" helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# pod0
# 3 containers, one with normal CPU preference, two with low-prio CPU.
ANN0="prefer-cpu-priority.resource-policy.nri.io/container.pod0c0: low"
ANN2="prefer-cpu-priority.resource-policy.nri.io/container.pod0c2: low"
CONTCOUNT=3 CPU=1 create guaranteed
report allowed

verify \
    'cpus["pod0c0"] == {"cpu04"}' \
    'cpus["pod0c1"] not in [ {"cpu04"}, {"cpu05"} ]' \
    'cpus["pod0c2"] == {"cpu05"}'

vm-command "kubectl delete pods --all --now"

# Override CPU type detection, to detect 3 high-perf CPUs (one extra
# for the reserved CPU allocation for which we currently always prefer
# normal prio CPUs).
helm-terminate
EXTRA_ENV_OVERRIDE_SYS_CORE_CPUS="0-1,4-5" helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# pod1
# 3 containers, default with low-prio CPU preference, two with high-prio CPU.
ANN0="prefer-cpu-priority.resource-policy.nri.io/pod: low"
ANN0="prefer-cpu-priority.resource-policy.nri.io/container.pod1c0: high"
ANN2="prefer-cpu-priority.resource-policy.nri.io/container.pod1c2: high"
CONTCOUNT=3 CPU=1 create guaranteed
report allowed

# cpu00 will be allocated for the reserved pool. Our two high-prio
# containers should first take cpu04 (preferred NUMA node with fewer
# containers, lower of the available normal prio cpu04-cpu-5), then
# should take cpu01 (lower ID of NUMA nodes with equal score). The
# low-prio container in between should take some other CPU that
# cpu1,4,5.
#
# However, the only CPU allocation detail of interest here is that
# we want normal prio CPUs (in the lack of high-prio ones) for our
# containers annotated with high-prio CPU preference. So only check
# for that instead of pretending to both understand and remember
# all the various preference details of the allocation pool selection
# and allocation logic.

verify \
    'cpus["pod1c0"] in     [ {"cpu01"}, {"cpu04"}, {"cpu05"} ]' \
    'cpus["pod1c1"] not in [ {"cpu01"}, {"cpu04"}, {"cpu05"} ]' \
    'cpus["pod1c2"] in     [ {"cpu01"}, {"cpu04"}, {"cpu05"} ]' \

vm-command "kubectl delete pods --all --now"
