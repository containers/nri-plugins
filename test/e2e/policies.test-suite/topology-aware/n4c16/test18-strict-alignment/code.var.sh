
cleanup() {
    delete-pods --all
    delete-namespaces highprio lowprio
}

cleanup
helm-terminate
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# Create a container with a strict test hint for NUMA node 2.
ANN0="strict.topologyhints.resource-policy.nri.io/container.pod0c0: 'true'"
ANN1="test.topologyhints.resource-policy.nri.io/container.pod0c0: '{ test: { NUMAs: \"2\" } } }'"

CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify \
    'cpus["pod0c0"].issubset({"cpu08", "cpu09", "cpu10", "cpu11"})' \
    'node_ids(nodes["pod0c0"]) == {2}'

# Create a container with a strict test hint for NUMA node 3.
ANN0="strict.topologyhints.resource-policy.nri.io/container.pod1c0: 'true'"
ANN1="test.topologyhints.resource-policy.nri.io/container.pod1c0: '{ test: { NUMAs: \"3\" } } }'"

CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify \
    'cpus["pod1c0"].issubset({"cpu12", "cpu13", "cpu14", "cpu15"})' \
    'node_ids(nodes["pod1c0"]) == {3}'

# Create a container with a strict test hint for NUMA node 2.
ANN0='strict.topologyhints.resource-policy.nri.io/container.pod2c0: "true"'
ANN1='test.topologyhints.resource-policy.nri.io/container.pod2c0: "{ test: { NUMAs: \"2\" } } }"'
CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify \
    'cpus["pod2c0"].issubset({"cpu08", "cpu09", "cpu10", "cpu11"})' \
    'node_ids(nodes["pod2c0"]) == {2}'

# Try to create a container with a strict test hint NUMA node 2.
# This one should not fit (not enough CPU left) and should fail with a
# strict hint check error.
ANN0='strict.topologyhints.resource-policy.nri.io/container.pod3c0: "true"'
ANN1='test.topologyhints.resource-policy.nri.io/container.pod3c0: "{ test: { NUMAs: \"2\" } } }"'
wait="" CONTCOUNT=1 CPU=3 MEM=100M create guaranteed

PodReadyCond='condition=PodReadyToStartContainers'
vm-command "kubectl wait --timeout=5s pod pod3 --for=$PodReadyCond" || {
    error "failed to wait for pod3 to start containerd"
}

verify-container-error pod3 0 "fail strict hint"

# Try to create a container with a strict test hints for NUMA node 2 and
# required isolated CPUs. This one would fit but there are not isolated
# CPUs so it should fail with a strict isolation preference error.
ANN0='strict.topologyhints.resource-policy.nri.io/container.pod4c0: "true"'
ANN1='test.topologyhints.resource-policy.nri.io/container.pod4c0: "{ test: { NUMAs: \"2\" } } }"'
ANN2='require-isolated-cpus.resource-policy.nri.io/container.pod4c0: "true"'
wait="" CONTCOUNT=1 CPU=1 MEM=100M create guaranteed

PodReadyCond='condition=PodReadyToStartContainers'
vm-command "kubectl wait --timeout=5s pod pod4 --for=$PodReadyCond" || {
    error "failed to wait for pod4 to start containerd"
}

verify-container-error pod4 0 "isolated CPUs"

# Create a container with a strict test hint for NUMA node 2 and preference
# for isolated CPUs. This one should fit and succeed because the unfulfilled
# preference is not a strict requirement.
ANN0='strict.topologyhints.resource-policy.nri.io/container.pod5c0: "true"'
ANN1='test.topologyhints.resource-policy.nri.io/container.pod5c0: "{ test: { NUMAs: \"2\" } } }"'
ANN2='prefer-isolated-cpus.resource-policy.nri.io/container.pod5c0: "true"'
CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify \
    'cpus["pod5c0"].issubset({"cpu08", "cpu09", "cpu10", "cpu11"})' \
    'node_ids(nodes["pod5c0"]) == {2}'

vm-command "kubectl delete pod pod5 --now"

# Now recreate pod4 test but with more complex effective annotation.
# Try to create a container with a strict test hints for NUMA node 2 an
# required isolated CPUs. This one would fit but there are not isolated
# CPUs so it should fail with a strict isolation preference error.
ANN0='strict.topologyhints.resource-policy.nri.io/container.pod6c0: "true"'
ANN1='test.topologyhints.resource-policy.nri.io/container.pod6c0: "{ test: { NUMAs: \"2\" } } }"'
ANN2='prefer-isolated-cpus.resource-policy.nri.io/pod: "false"'
ANN3='require-isolated-cpus.resource-policy.nri.io/container.pod6c0: "true"'
wait="" CONTCOUNT=1 CPU=1 MEM=100M create guaranteed

PodReadyCond='condition=PodReadyToStartContainers'
vm-command "kubectl wait --timeout=5s pod pod6 --for=$PodReadyCond" || {
    error "failed to wait for pod6 to start containerd"
}

verify-container-error pod6 0 "isolated CPUs"

# Now recreate pod5 test but with more complex effective annotation.
# Create a container with a strict test hint for NUMA node 2 and preference
# for isolated CPUs. This one should fit and succeed because the unfulfilled
# preference is not a strict requirement.
ANN0='strict.topologyhints.resource-policy.nri.io/container.pod7c0: "true"'
ANN1='test.topologyhints.resource-policy.nri.io/container.pod7c0: "{ test: { NUMAs: \"2\" } } }"'
ANN2='prefer-isolated-cpus.resource-policy.nri.io/container.pod7c0: "true"'
ANN3='require-isolated-cpus.resource-policy.nri.io/pod: "true"'
CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify \
    'cpus["pod7c0"].issubset({"cpu08", "cpu09", "cpu10", "cpu11"})' \
    'node_ids(nodes["pod7c0"]) == {2}'

unset ANN0 ANN1 ANN2 ANN3

# A hint which names a socket rather than a NUMA node. A NUMA hint is
# resolved to the CPUs of that node before the pools are scored, so it
# is scored as a set of CPUs; a socket hint is not resolved and is scored
# as a socket, which leaves the container free to land on either node of
# socket 1.
ANN0='test.topologyhints.resource-policy.nri.io/container.pod8c0: "{ test: { Sockets: \"1\" } }"'
CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify \
    'cpus["pod8c0"].issubset({"cpu08", "cpu09", "cpu10", "cpu11",
                              "cpu12", "cpu13", "cpu14", "cpu15"})'

unset ANN0

# A strict hint on a container which gets a shared portion of a pool
# instead of exclusive CPUs: the strict check has to hold for the shared
# CPUs of the grant, which a fractional CPU request is served from.
ANN0='strict.topologyhints.resource-policy.nri.io/container.pod9c0: "true"'
ANN1='test.topologyhints.resource-policy.nri.io/container.pod9c0: "{ test: { NUMAs: \"3\" } }"'
CONTCOUNT=1 CPU=500m MEM=100M create guaranteed
report allowed
verify 'node_ids(nodes["pod9c0"]) == {3}'

unset ANN0 ANN1

# A strict hint on a container in the reserved pool. The reservation is
# cpu00, on NUMA node 0, so a hint for node 0 holds.
ANN0='strict.topologyhints.resource-policy.nri.io/container.pod10c0: "true"'
ANN1='test.topologyhints.resource-policy.nri.io/container.pod10c0: "{ test: { NUMAs: \"0\" } }"'
ANN2='prefer-reserved-cpus.resource-policy.nri.io/container.pod10c0: "true"'
CONTCOUNT=1 CPU=50m MEM=100M create guaranteed
report allowed
verify 'cpus["pod10c0"] == {"cpu00"}'

unset ANN0 ANN1 ANN2

# A hint which names a NUMA node that does not exist. Such a hint cannot be
# resolved to CPUs, so unlike every hint above it is scored as a set of node
# ids, and it matches no pool: the container is placed as if it had no hint at
# all rather than refused.
ANN0='test.topologyhints.resource-policy.nri.io/container.pod11c0: "{ test: { NUMAs: \"99\" } }"'
CONTCOUNT=1 CPU=1 MEM=100M create guaranteed
report allowed
verify 'len(cpus["pod11c0"]) == 1'

plugin-log 'evaluating topology hint <hints NUMAs:99' > /dev/null ||
    command-error "the policy resolved a hint for a node which does not exist"

unset ANN0
