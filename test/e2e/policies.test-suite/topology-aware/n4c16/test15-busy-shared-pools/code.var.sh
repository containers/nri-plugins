vm-command "kubectl delete pods --all --now"
helm-terminate

helm_config=$(COLOCATE_PODS=false instantiate helm-config.yaml) helm-launch topology-aware

# Create 4 BestEffort containers, which should be spread across NUMA nodes,
# putting one container in each. Then create a Guaranteed container with a
# CPU request equal to the number of CPUs in a NUMA node. This container
# cannot be put into any NUMA node, since then it would exhaus the shared
# pool, which we can't do in any NUMA node as they are all busy with a
# BestEffort container. So the Guaranteed container should be placed in a
# socket, slicing off CPUs from both of its NUMA nodes.

CONTCOUNT=4 create besteffort
CPU=4 MEM=100M CONTCOUNT=1 create guaranteed
verify 'disjoint_sets(cpus["pod0c0"],cpus["pod0c1"],cpus["pod0c2"],cpus["pod0c3"],cpus["pod1c0"])'

vm-command "kubectl delete pods --all --now"

# Repeat the same test using Burstable containers that look like BestEffort
# based on their CPU request alone.
CONTCOUNT=4 CPUREQ=1m CPULIM=750m create burstable
CPU=4 MEM=100M CONTCOUNT=1 create guaranteed
verify 'disjoint_sets(cpus["pod2c0"],cpus["pod2c1"],cpus["pod2c2"],cpus["pod2c3"],cpus["pod3c0"])'

vm-command "kubectl delete pods --all --now"

# Repeat the same test using non-zero CPU request Burstable containers.
CONTCOUNT=4 CPUREQ=250m CPULIM=750m create burstable
CPU=4 MEM=100M CONTCOUNT=1 create guaranteed
verify 'disjoint_sets(cpus["pod4c0"],cpus["pod4c1"],cpus["pod4c2"],cpus["pod4c3"],cpus["pod5c0"])'

vm-command "kubectl delete pods --all --now"
helm-terminate
