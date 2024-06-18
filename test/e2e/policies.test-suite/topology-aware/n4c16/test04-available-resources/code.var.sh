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
vm-command "kubectl delete pods pod1 --now"

# Relaunch NRI-plugins with a set of CPUs from a different socket compared
# to where pod0 is currently running. Restoring pod0 to current CPUs will fail
# so the workload should be moved to new CPUs.
terminate nri-resource-policy
AVAILABLE_CPU="cpuset:0-1,11-14"
nri_resource_policy_cfg=$(instantiate nri-resource-policy-available-resources.cfg)
launch nri-resource-policy
report allowed
verify "cpus['pod0c0'] == {'cpu12', 'cpu13', 'cpu14'}" \
       "mems['pod0c0'] == {'node3'}"

vm-command "kubectl delete pods --all --now"
reset counters

# cleanup, do not leave weirdly configured nri-resource-policy running
helm-terminate
