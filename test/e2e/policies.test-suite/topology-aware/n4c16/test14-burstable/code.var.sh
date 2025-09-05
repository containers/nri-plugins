# Test that burstable container allocation behaves as expected.

helm-terminate
helm_config=$(COLOCATE_PODS=true instantiate helm-config.yaml) helm-launch topology-aware

vm-command "kubectl delete pods --all --now"

# pod0, pod1, pod2, and pod3 have total memory limit which
# exceeds the total node capacity. They would not fit the
# node if memory was allocated by limit, but they should
# fit if memory is allocated by request.
CPUREQ=1 CPULIM=2 MEMREQ=100M MEMLIM=2.5G create burstable
CPUREQ=1 CPULIM=2 MEMREQ=200M MEMLIM=5G create burstable
CPUREQ=1 CPULIM=2 MEMREQ=100M MEMLIM=2.5G create burstable
CPUREQ=1 CPULIM=2 MEMREQ=200M MEMLIM=0 create burstable

# pod0 and pod2 have limits that require 2 NUMA nodes.
# pod1 requires all 4 NUMA nodes. pod3 is unlimited, so
# it should also get all NUMA nodes.
report allowed
verify \
    'len(nodes["pod0c0"]) == 2' \
    'len(nodes["pod1c0"]) == 4' \
    'len(nodes["pod2c0"]) == 2' \
    'len(nodes["pod3c0"]) == 4'

vm-command "kubectl delete pods --all --now"

helm-terminate
helm_config=$(COLOCATE_PODS=false instantiate helm-config.yaml) helm-launch topology-aware

# Limited burstable containers get assigned to the lowest pool where
# there is enough free capacity for their limit. In this case, a socket.
CONTCOUNT=2 CPUREQ=2 CPULIM=5 MEMREQ=100M create burstable
report allowed
verify \
    'nodes["pod4c0"] == { "node2" ,"node3" }' \
    'nodes["pod4c1"] == { "node0", "node1" }'

# Unlimited burstable containers get allocated to the root pool unless
# affinity dictates otherwise.
CONTCOUNT=2 CPUREQ=2 CPULIM=0 MEMREQ=100M create burstable
report allowed
verify \
    'nodes["pod5c0"] == { "node0", "node1", "node2", "node3" }' \
    'nodes["pod5c1"] == { "node0", "node1", "node2", "node3" }'

vm-command "kubectl delete pods --all --now"

helm-terminate
