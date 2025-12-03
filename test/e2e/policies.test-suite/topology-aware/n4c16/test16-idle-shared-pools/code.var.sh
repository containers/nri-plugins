vm-command "kubectl delete pods --all --now"
helm-terminate

helm_config=$(COLOCATE_PODS=false instantiate helm-config.yaml) helm-launch topology-aware

# Test that we allow slicing idle shared pools empty.
CPU=4 MEM=100M CONTCOUNT=3 create guaranteed
verify 'disjoint_sets(cpus["pod0c0"],cpus["pod0c1"],cpus["pod0c2"])'
verify 'len(cpus["pod0c0"]) == 4' \
       'len(cpus["pod0c1"]) == 4' \
       'len(cpus["pod0c2"]) == 4'

verify 'disjoint_sets(nodes["pod0c0"],nodes["pod0c1"],nodes["pod0c2"])'
verify 'len(nodes["pod0c0"]) == 1' \
       'len(nodes["pod0c1"]) == 1' \
       'len(nodes["pod0c2"]) == 1'

# Test that BestEffort and Burstable containers are now placed in the only
# remaining pool with free CPU.
CONTCOUNT=4 create besteffort
verify 'cpus["pod1c0"] == cpus["pod1c1"]' \
       'cpus["pod1c0"] == cpus["pod1c2"]' \
       'cpus["pod1c0"] == cpus["pod1c3"]'

CONTCOUNT=4 CPUREQ=100m CPULIM=150m create burstable
verify 'cpus["pod2c0"] == cpus["pod2c1"]' \
       'cpus["pod2c0"] == cpus["pod2c2"]' \
       'cpus["pod2c0"] == cpus["pod2c3"]' \
       'cpus["pod2c0"] == cpus["pod1c0"]'
verify 'disjoint_sets(nodes["pod0c0"],nodes["pod0c1"],nodes["pod0c2"],nodes["pod1c0"])'

vm-command "kubectl delete pods --all --now"

# Now test the other way around. First spread 2 besteffort containers
# around NUMA nodes. Then create a guaranteed one with 4 CPUs and check
# that it gets allocated to a full NUMA node.
CONTCOUNT=2 create besteffort
verify 'disjoint_sets(cpus["pod3c0"],cpus["pod3c1"])'

CPU=4 MEM=100M CONTCOUNT=1 create guaranteed
verify 'disjoint_sets(cpus["pod4c0"],cpus["pod3c0"],cpus["pod3c1"])'
verify 'len(cpus["pod4c0"]) == 4' \
       'len(nodes["pod4c0"]) == 1'
verify 'disjoint_sets(nodes["pod3c0"],nodes["pod3c1"],nodes["pod4c0"])'

vm-command "kubectl delete pods --all --now"
helm-terminate
