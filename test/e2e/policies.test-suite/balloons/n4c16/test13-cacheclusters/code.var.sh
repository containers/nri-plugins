helm-terminate
helm_config=$TEST_DIR/balloons-4cpu-cacheclusters.cfg helm-launch balloons

cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
    vm-command "rm -f /var/lib/nri-resource-policy/cache" || true
}

# pod0c{0,1,2}: one container per free L2 group
CPUREQ="1500m" MEMREQ="100M" CPULIM="1500m" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2burst" CONTCOUNT=3 create balloons-busybox
report allowed
verify 'len(cpus["pod0c0"]) == 4' \
       'len(cpus["pod0c0"]) == 4' \
       'len(cpus["pod0c1"]) == 4' \
       'disjoint_sets(nodes["pod0c0"], nodes["pod0c1"], nodes["pod0c2"])'

# pod1c0: one more container to new L2 group, but this is shared with the reserved balloon of size 1.
CPUREQ="2" MEMREQ="100M" CPULIM="2" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2burst" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod1c0"]) == 3' \
       'disjoint_sets(cpus["pod1c0"], cpus["pod0c0"], cpus["pod0c1"], cpus["pod0c2"])'

# pod2c{0,1,2,3} fit two per l2pack balloon, all l2pack balloons should be in the same L2 cache.
vm-command "kubectl delete pods --all --now"
CPUREQ="500m" MEMREQ="100M" CPULIM="500m" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2pack" CONTCOUNT=4 create balloons-busybox
report allowed
verify 'len(cpus["pod2c0"]) == 2' \
       'len(cpus["pod2c1"]) == 2' \
       'len(cpus["pod2c2"]) == 2' \
       'len(cpus["pod2c3"]) == 2' \
       'len(set.intersection(cpus["pod2c0"], cpus["pod2c1"], cpus["pod2c2"], cpus["pod2c3"])) == 1' \
       'len(set.union(cpus["pod2c0"], cpus["pod2c1"], cpus["pod2c2"], cpus["pod2c3"])) == 3' \
       'nodes["pod2c0"] == nodes["pod2c1"] == nodes["pod2c2"] == nodes["pod2c3"]'

# Due to packing, pod2c* should share the same L2 cache with the only existing balloon (the reserved balloon)
# pod3c0: reserved-balloon container requires the reserved balloon to inflate from 1 to 2 CPUs.
# Therefore there is no more shared idle CPU for pod2* containers.
CPUREQ="500m" MEMREQ="100M" CPULIM="500m" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: reserved" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'nodes["pod3c0"] == nodes["pod2c0"] == nodes["pod2c1"] == nodes["pod2c2"] == nodes["pod2c3"]' \
       'len(cpus["pod3c0"]) == 2' \
       'len(cpus["pod2c0"]) == 1' \
       'len(cpus["pod2c1"]) == 1' \
       'len(cpus["pod2c2"]) == 1' \
       'len(cpus["pod2c3"]) == 1' \
       'disjoint_sets(cpus["pod3c0"], cpus["pod2c0"])' \
       'disjoint_sets(cpus["pod3c0"], cpus["pod2c1"])' \
       'disjoint_sets(cpus["pod3c0"], cpus["pod2c2"])' \
       'disjoint_sets(cpus["pod3c0"], cpus["pod2c3"])' \
       'len(set.union(cpus["pod3c0"], cpus["pod2c0"], cpus["pod2c1"], cpus["pod2c2"], cpus["pod2c3"])) == 4'

cleanup

helm-terminate
helm_config=$TEST_DIR/balloons-2cpu-cacheclusters.cfg helm-launch balloons

# pod4c{0,1,2,3}: one container per free L2 group, this time L2 groups contain only single CPU cores
CPUREQ="500m" MEMREQ="100M" CPULIM="500m" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2burst" CONTCOUNT=4 create balloons-busybox
report allowed
verify 'len(cpus["pod4c0"]) == 2' \
       'len(cpus["pod4c1"]) == 2' \
       'len(cpus["pod4c2"]) == 2' \
       'len(cpus["pod4c3"]) == 2' \
       'disjoint_sets(cpus["pod4c0"], cpus["pod4c1"], cpus["pod4c2"], cpus["pod4c3"])' \
       'disjoint_sets(nodes["pod4c0"], nodes["pod4c1"], nodes["pod4c2"], nodes["pod4c3"])'
# pod5c{0,1,2}: one container per free L2 group, fill three NUMA nodes
CPUREQ="500m" MEMREQ="100M" CPULIM="500m" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2burst" CONTCOUNT=3 create balloons-busybox
report allowed
verify 'len(cpus["pod5c0"]) == 2' \
       'len(cpus["pod5c1"]) == 2' \
       'len(cpus["pod5c2"]) == 2' \
       'disjoint_sets(cpus["pod5c0"], cpus["pod5c1"], cpus["pod5c2"], cpus["pod4c0"], cpus["pod4c1"], cpus["pod4c2"], cpus["pod4c3"])'

cleanup
