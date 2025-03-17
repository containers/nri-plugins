# Test associating loads with balloons that are expected to generate
# them. Test that CPU allocation avoids overloading any part of the
# system.

helm-terminate
helm_config=$TEST_DIR/balloons-loadclasses.cfg helm-launch balloons

cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
    vm-command "rm -f /var/lib/nri-resource-policy/cache" || true
}

# Policy's allocatorTopologyBalancing is false, so CPU allocations are
# tightly packed rather than evenly spread accross the system.

# One of pod0's two containers should find its CPUs on the same node
# with both pre-reserved-avx balloons (cpusets 0,2 and 4,6) and
# reserved[0] (cpuset 1). User-defined "avx" and "membw" loads do not
# interfere in this configuration. The other container does not fit to
# node0, but "pack tightly" is expected to put it on the same package
# nevertheless.
CPUREQ="500m" MEMREQ="100M" CPULIM="500m" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2load" CONTCOUNT=2 create balloons-busybox
# Print balloons and their cpusets from NRT for debugging.
vm-command 'kubectl get -n kube-system noderesourcetopologies.topology.node.k8s.io -o json  | jq ".items[].zones[] | select(.type == \"balloon\") | {balloon:.name, cpuset:(.attributes[] | select(.name == \"cpuset\") | .value)}"'
report allowed
verify 'len(cpus["pod0c0"]) == 1' \
       'len(cpus["pod0c1"]) == 1' \
       'disjoint_sets(nodes["pod0c0"], nodes["pod0c1"])' \
       'packages["pod0c0"] == packages["pod0c1"]'

# pod1 runs in a noload balloon that should be tightly packed on the
# same package as pod0.
CPUREQ="1" MEMREQ="100M" CPULIM="1" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: noload" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'packages["pod1c0"] == packages["pod0c0"]'

# pod2 runs in another noload balloon, should be tigtly packed on new
# package because package0 is full.
CPUREQ="1" MEMREQ="100M" CPULIM="1" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: noload" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'packages["pod2c0"] != packages["pod1c0"]'

# pod3c0 should be tigtly packed on same node as last pod2c0. pod2c0
# generates no load, but pod3c0 generates both membw and avx loads.
CPUREQ="100m" MEMREQ="100M" CPULIM="200m" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2htload" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod3c0"]) == 1' \
       'nodes["pod3c0"] == nodes["pod2c0"]'

# Despite tight packing, pod4c0 should not run the same node as pod3c0
# as they both load L2 cache.
CPUREQ="100m" MEMREQ="100M" CPULIM="200m" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2htload" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cpus["pod4c0"]) == 1' \
       'disjoint_sets(nodes["pod3c0"], nodes["pod4c0"])'

# pod5 gets pre-allocated "avx" CPUs without any changes from pre-reserved-avx[0].
# Both containers fit in the same balloon.
CPUREQ="1" MEMREQ="100M" CPULIM="1" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: pre-reserved-avx" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'len(cores["pod5c0"]) == 2' \
       'len(cpus["pod5c0"]) == 2' \
       'cpus["pod5c0"] == cpus["pod5c1"]'

# pod6c0 gets pre-allocated "avx" CPUS from pre-reserved-avx[1], but fills it completely.
# pod6c1 goes to the same balloon due to not spreading pods. Now pre-reserved-avx[1] needs
# to resize from 2 to 4 CPUs. Verify it is still not using sibling threads.
CPUREQ="2" MEMREQ="100M" CPULIM="2" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: pre-reserved-avx" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'len(cores["pod6c0"]) == 4' \
       'len(cpus["pod6c0"]) == 4' \
       'cpus["pod6c0"] == cpus["pod6c1"]' \
       'disjoint_sets(cores["pod6c0"], cores["pod5c0"])'

# Verify freeing and recreating virtual devices by deleting and
# reallocating l2 and avx pods.
vm-command "kubectl delete pod pod0 pod3 pod4 pod5 pod6 --now"
CPUREQ=4 CPULIM="" MEMREQ="" MEMLIM=""
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: l2htload" CONTCOUNT=1 create balloons-busybox
verify 'len(cpus["pod7c0"]) == 4' \
       'len(cores["pod7c0"]) == 4'

cleanup
