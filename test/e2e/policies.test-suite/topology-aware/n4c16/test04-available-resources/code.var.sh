# Test that AvailableResources are honored.

# Test explicit cpuset in AvailableResources.CPU
helm-terminate
RESERVED_CPU="cpuset:11"
AVAILABLE_CPU="cpuset:4-7,8-11"
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

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

helm-terminate
RESERVED_CPU="cpuset:11-15"
AVAILABLE_CPU="exclude-cpuset:0-1"
UNLIMITED_BURSTABLE=system
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# pod2: burstable/shared CPUs
CPUREQ=250m CPULIM=0 create burstable
verify "cpus['pod2c0'] == {'cpu02', 'cpu03', 'cpu04', 'cpu05', 'cpu06', 'cpu07', 'cpu08', 'cpu09', 'cpu10'}" \
       "mems['pod2c0'] == {'node0', 'node1', 'node2', 'node3'}"

# cleanup, do not leave weirdly configured nri-resource-policy running
helm-terminate
