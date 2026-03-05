
cleanup() {
    vm-command "kubectl delete pods --all --now"
    vm-command "kubectl delete namespaces highprio lowprio --now --ignore-not-found"
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

vm-command "kubectl get pod pod3 -o jsonpath='{.status.containerStatuses[0].state}' | \
        grep -q \"fail strict hint\"" || {
    error "pod3c0 unexpectedly passed strict topology hint check"
}

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

vm-command "kubectl get pod pod4 -o jsonpath='{.status.containerStatuses[0].state}' | \
        grep -q \"isolated CPUs\"" || {
    error "pod4c0 unexpectedly passed strict isolated CPU requirement check"
}

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

vm-command "kubectl get pod pod6 -o jsonpath='{.status.containerStatuses[0].state}' | \
        grep -q \"isolated CPUs\"" || {
    error "pod6c0 unexpectedly passed strict isolated CPU requirement check"
}

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
