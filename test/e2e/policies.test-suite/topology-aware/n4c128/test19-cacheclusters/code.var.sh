
cleanup() {
    vm-command "kubectl delete pods --all --now"
    vm-command "kubectl delete namespaces highprio lowprio --now --ignore-not-found"
}

cleanup
helm-terminate

helm_config=$TEST_DIR/helm-config.yaml helm-launch topology-aware

# Limit burstability of a container to an L3 cache group and verify that
# it gets confined to an L3 cache group.
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod0c0: l3cache'
ANN1='unlimited-burstable.resource-policy.nri.io/container.pod0c1: system'
ANN2='unlimited-burstable.resource-policy.nri.io/container.pod0c2: socket'
CONTCOUNT=3 CPUREQ=1500m CPULIM=0 MEMREQ=100M create burstable
unset ANN0 ANN1 ANN2
report allowed
verify \
    'len(cpus["pod0c0"]) == 8' \
    'len(cpus["pod0c1"]) == 127' \
    'len(cpus["pod0c2"]) == 64'

cleanup

# Test that when CPU request exceeds L3 cache capacity, the container
# is promoted to the next topology level (NUMA node for this topology).
# Each L3 cache has 8 CPUs, so requesting 12 CPUs should overflow to NUMA.
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod1c0: l3cache'
CONTCOUNT=1 CPUREQ=12 CPULIM=0 MEMREQ=100M create burstable
report allowed
verify \
    'len(cpus["pod1c0"]) == 31'  # Promoted to NUMA level

cleanup

# Test limited burstable: when CPU limit exceeds L3 cache capacity,
# the container is promoted to NUMA level.
# Request fits in L3 (4 CPUs), but limit (20) exceeds L3 capacity (8).
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod2c0: l3cache'
CONTCOUNT=1 CPUREQ=4 CPULIM=20 MEMREQ=100M create burstable
report allowed
verify \
    'len(cpus["pod2c0"]) == 32'  # Limit exceeds L3, promoted to NUMA

cleanup

# Test limited burstable: when CPU limit fits within L3 cache capacity,
# the container stays at L3 level.
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod3c0: l3cache'
CONTCOUNT=1 CPUREQ=4 CPULIM=8 MEMREQ=100M create burstable
report allowed
verify \
    'len(cpus["pod3c0"]) == 8'  # Limit fits in L3, stays at L3 level

cleanup

# Test multi-container pod with L3 cache preferences for each container.
# Both containers should be confined to different L3 cache groups (8 CPUs each, disjoint).
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod4c0: l3cache'
ANN1='unlimited-burstable.resource-policy.nri.io/container.pod4c1: l3cache'
CONTCOUNT=2 CPUREQ=2 CPULIM=0 MEMREQ=100M create burstable
unset ANN0 ANN1
report allowed
verify \
    'len(cpus["pod4c0"]) == 8' \
    'len(cpus["pod4c1"]) == 8' \
    'disjoint_sets(cpus["pod4c0"], cpus["pod4c1"])'

cleanup

# Test that containers with affinity annotation are placed in the same L3 cache.
# Both containers have L3 cache preference and affinity to each other.
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod5c0: l3cache'
ANN1='unlimited-burstable.resource-policy.nri.io/container.pod5c1: l3cache'
ANN2="resource-policy.nri.io/affinity: |+\n      pod5c0: [ pod5c1 ]"
CONTCOUNT=2 CPUREQ=2 CPULIM=0 MEMREQ=100M create burstable
unset ANN0 ANN1 ANN2
report allowed
verify \
    'len(cpus["pod5c0"]) == 8' \
    'len(cpus["pod5c1"]) == 8' \
    'cpus["pod5c0"] == cpus["pod5c1"]'  # Same L3 cache cluster due to affinity

cleanup

# Test L3 cache exhaustion and spillover across multiple L3 caches.
# Create multiple pods with L3 cache preference. Each L3 cache has 8 CPUs.
# Each container should be placed in a unique L3 cache (disjoint CPU sets).
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod6c0: l3cache'
CONTCOUNT=1 CPUREQ=2 CPULIM=0 MEMREQ=50M create burstable
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod7c0: l3cache'
CONTCOUNT=1 CPUREQ=2 CPULIM=0 MEMREQ=50M create burstable
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod8c0: l3cache'
CONTCOUNT=1 CPUREQ=2 CPULIM=0 MEMREQ=50M create burstable
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod9c0: l3cache'
CONTCOUNT=1 CPUREQ=2 CPULIM=0 MEMREQ=50M create burstable
report allowed
verify \
    'len(cpus["pod6c0"]) == 8' \
    'len(cpus["pod7c0"]) == 8' \
    'len(cpus["pod8c0"]) == 8' \
    'len(cpus["pod9c0"]) == 8' \
    'disjoint_sets(cpus["pod6c0"], cpus["pod7c0"], cpus["pod8c0"], cpus["pod9c0"])'

cleanup

# Test L3 cache least-occupied selection.
# Fill all 16 L3 caches: 15 with high CPU usage (4 CPUs), 1 with low usage (1 CPU).
# The 17th container should be placed in the least occupied L3 cache.
# Create 15 pods requesting 4 CPUs each (high occupancy).
for i in $(seq 10 24); do
    ANN0="unlimited-burstable.resource-policy.nri.io/container.pod${i}c0: l3cache"
    CONTCOUNT=1 CPUREQ=4 CPULIM=0 MEMREQ=50M create burstable
done
# Create 1 pod requesting only 1 CPU (low occupancy) - this is the least occupied L3.
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod25c0: l3cache'
CONTCOUNT=1 CPUREQ=1 CPULIM=0 MEMREQ=50M create burstable
report allowed

# Verify all 16 L3 caches are occupied and disjoint.
verify \
    'len(set.union(*[cpus[f"pod{i}c0"] for i in range(10, 26)])) == 127' \
    'disjoint_sets(*[cpus[f"pod{i}c0"] for i in range(10, 26)])'

# Now add the 17th container - it should share the L3 cache with pod25c0
# (the least occupied one with only 1 CPU used).
# Note: pod25c0 will be placed on the L3 cache with 7 CPUs (due to reserve)
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod26c0: l3cache'
CONTCOUNT=1 CPUREQ=2 CPULIM=0 MEMREQ=50M create burstable
report allowed
verify \
    'cpus["pod26c0"] == cpus["pod25c0"]'  # Should share with least occupied L3

cleanup

# Test L3 cache overflow when all caches are nearly full.
# Fill all 16 L3 caches with 7 CPUs each (leaving only 1 CPU free per cache).
# The 17th container requesting 3 CPUs cannot fit in any L3 cache and should
# be promoted to the next topology level (NUMA node).
for i in $(seq 27 42); do
    ANN0="unlimited-burstable.resource-policy.nri.io/container.pod${i}c0: l3cache"
    CONTCOUNT=1 CPUREQ=7 CPULIM=0 MEMREQ=50M create burstable
done
report allowed

# Verify all 16 L3 caches are occupied with disjoint CPU sets.
verify \
    'len(set.union(*[cpus[f"pod{i}c0"] for i in range(27, 43)])) == 127' \
    'disjoint_sets(*[cpus[f"pod{i}c0"] for i in range(27, 43)])'

# Now add the 17th container requesting 3 CPUs - it cannot fit in any L3 cache
# (each has only 1 CPU free), so it should be promoted to NUMA level.
# When multiple NUMA nodes can satisfy the request, lower node ID wins.
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod43c0: l3cache'
CONTCOUNT=1 CPUREQ=3 CPULIM=0 MEMREQ=50M create burstable
report allowed
verify \
    'len(cpus["pod43c0"]) == 31'  # Promoted to NUMA0 (31 CPUs due to reserved CPU 0)

# Add 18th container - NUMA0 now has less capacity, so NUMA1 (next lowest ID) wins.
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod44c0: l3cache'
CONTCOUNT=1 CPUREQ=3 CPULIM=0 MEMREQ=50M create burstable
report allowed
verify \
    'len(cpus["pod44c0"]) == 32'  # Promoted to NUMA1 (32 CPUs, no reserved CPU)

cleanup

# Test guaranteed pod taking exclusive CPUs from L3 cache shared by burstable pod.
# Fill all 16 L3 caches with burstable pods, then create a guaranteed pod.
# The guaranteed pod takes exclusive CPUs, reducing the burstable pod's shared CPUs.
for i in $(seq 45 60); do
    ANN0="unlimited-burstable.resource-policy.nri.io/container.pod${i}c0: l3cache"
    CONTCOUNT=1 CPUREQ=2 CPULIM=0 MEMREQ=50M create burstable
done
report allowed

# Verify all 16 L3 caches occupied with 8 CPUs each (disjoint).
verify \
    'len(set.union(*[cpus[f"pod{i}c0"] for i in range(45, 61)])) == 127' \
    'disjoint_sets(*[cpus[f"pod{i}c0"] for i in range(45, 61)])'

# pod45c0 should have 8 CPUs (full L3 cache).
verify 'len(cpus["pod45c0"]) == 8'

# Create a guaranteed pod requesting 4 exclusive CPUs.
# Lower node ID wins, so it lands on the same L3 cache as pod45c0.
CPU=4 MEM=100M create guaranteed
report allowed

# pod45c0 loses 4 CPUs to the guaranteed pod's exclusive allocation.
verify \
    'len(cpus["pod61c0"]) == 4' \
    'len(cpus["pod45c0"]) == 4' \
    'disjoint_sets(cpus["pod45c0"], cpus["pod61c0"])'

# Create another guaranteed pod requesting 8 exclusive CPUs (full L3 cache).
# The guaranteed pod does not evict burstable pods. Instead, it takes exclusive
# CPUs from the shared pool, spreading across multiple L3 caches if needed.
CPU=8 MEM=100M create guaranteed
report allowed

# pod62c0 gets 8 exclusive CPUs spread across two L3 caches.
# pod50c0 and pod51c0 share a NUMA node with pod62c0. Their shared CPU pools
# are reduced as pod62c0 takes exclusive CPUs: pod50c0 keeps 2, pod51c0 keeps 6.
verify \
    'len(cpus["pod62c0"]) == 8' \
    'len(cpus["pod50c0"]) == 2' \
    'len(cpus["pod51c0"]) == 6'

# Delete the guaranteed pods and verify burstable pods reclaim their CPUs.
vm-command "kubectl delete pods pod61 pod62 --now"
report allowed

# After guaranteed pods are deleted, burstable pods should grow back to full
# L3 cache size as the exclusive CPUs return to the shared pool.
verify \
    'len(cpus["pod45c0"]) == 8' \
    'len(cpus["pod50c0"]) == 8' \
    'len(cpus["pod51c0"]) == 8'

cleanup
helm-terminate
