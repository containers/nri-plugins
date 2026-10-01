# This test verifies that the topology-aware policy publishes one DRA
# device per NUMA node, allocates CPUs to claims and pins containers to
# the CPUs of their claim.
#

cleanup() {
    vm-command "kubectl delete pods --all --now --wait" || :
    vm-command "kubectl delete resourceclaimtemplates --all --now" || :
    vm-command "kubectl delete deviceclass topology-aware.nri.io --ignore-not-found" || :
    helm-terminate || :
}

slice() {
    # Usage: slice JQ
    #
    # Print JQ of our ResourceSlice.
    vm-command-q "kubectl get resourceslices -o json | jq -r '.items[] |
                      select(.spec.driver == \"topology-aware.nri.io\") | $1'"
}

device() {
    # Usage: device NAME JQ
    #
    # Print JQ of our device NAME, from the slices last fetched into $slices.
    jq -r --arg name "$1" '.items[] | select(.spec.driver == "topology-aware.nri.io") |
                           .spec.devices[] | select(.name == $name) | '"$2" <<< "$slices"
}

claim() {
    # Usage: claim POD NODE CPUS
    #
    # Create POD with a claim for CPUS CPUs of NUMA node NODE, and wait for
    # it to start.
    NAME=$1 NODE=$2 CPUS=$3 wait="" create cpus-claim
    NAME=$1 create dra-pod
}

claimed-cpus() {
    # Usage: claimed-cpus POD
    #
    # Print the CPUs the claim of POD got, as its container sees them.
    vm-command-q "kubectl exec $1 -- env" | sed -n 's/^DRA_CPUSET_[^=]*=//p'
}

node-cpus() {
    # Usage: node-cpus NODE
    #
    # Print the CPUs of NUMA node NODE.
    vm-command-q "cat /sys/devices/system/node/node$1/cpulist"
}

cleanup

# Reserve CPU 15, so that node 3 has one CPU less to give than the others.
helm_config=$(RESERVED_CPU="cpuset:15" DRA_ENABLED=true instantiate helm-config.yaml) \
    helm-launch topology-aware

retry-until --timeout 30 --message "the plugin to publish its devices" \
    '[ -n "$(slice ".spec.devices[0].name // empty")" ]' ||
    error "the plugin published no devices"

# Without DRAConsumableCapacity the API server drops allowMultipleAllocations,
# which lets claims share a device.
if [ "$(slice ".spec.devices[0].allowMultipleAllocations")" != "true" ]; then
    cleanup
    echo "Test verdict: SKIP (DRAConsumableCapacity is not enabled)"
    exit 0
fi

vm-command "kubectl get resourceslices -o yaml"

slices=$(vm-command-q "kubectl get resourceslices -o json")

count=$(jq '[.items[] | select(.spec.driver == "topology-aware.nri.io") | .spec.devices[]] | length' <<< "$slices")
[ "$count" == "4" ] ||
    error "published $count devices, expected 4"

for node in 0 1 2 3; do
    name=$(printf "cpudevnuma%03d" "$node")
    [ "$(device "$name" .name)" == "$name" ] ||
        error "no device $name for NUMA node $node"

    numa=$(device "$name" '.attributes["resource.kubernetes.io/numaNode"].int')
    [ "$numa" == "$node" ] ||
        error "device $name has numaNode $numa, expected $node"

    # n4c16's topology is two packages of two NUMA nodes each.
    socket=$(device "$name" '.attributes["dra.cpu/socketID"].int')
    expected_socket=$((node / 2))
    [ "$socket" == "$expected_socket" ] ||
        error "device $name has socketID $socket, expected $expected_socket"

    smt=$(device "$name" '.attributes["dra.cpu/smtEnabled"].bool')
    [ "$smt" == "true" ] ||
        error "device $name has smtEnabled \"$smt\", expected true"

    cpus=$(device "$name" '.capacity["dra.cpu/cpu"].value')
    expected_cpus=4
    [ "$node" == "3" ] && expected_cpus=3
    [ "$cpus" == "$expected_cpus" ] ||
        error "device $name has dra.cpu/cpu capacity \"$cpus\", expected $expected_cpus"

    policy=$(device "$name" '.capacity["dra.cpu/cpu"].requestPolicy |
                             "\(.default)/\(.validRange.min)/\(.validRange.step)"')
    [ "$policy" == "1/1/1" ] ||
        error "device $name has request policy default/min/step $policy, expected 1/1/1"

    multi=$(device "$name" .allowMultipleAllocations)
    [ "$multi" == "true" ] ||
        error "device $name has allowMultipleAllocations \"$multi\", expected true"

    # DRANodeAllocatableResources (k8s 1.37+): pre-1.37 API servers drop
    # this field silently, so only check it when the server kept it.
    mapping=$(device "$name" '.nodeAllocatableResources.cpu.mapping |
                              select(. != null) | "\(.capacityKey) x \(.capacityMultiplier)"')
    [ -z "$mapping" ] || [ "$mapping" == "dra.cpu/cpu x 1" ] ||
        error "device $name maps nodeAllocatableResources.cpu to $mapping, expected dra.cpu/cpu x 1"

    echo "$name: numaNode $numa, socketID $socket, smtEnabled $smt, $cpus CPUs"
done

wait="" create deviceclass

# pod0 claims 3 CPUs on node 3. It gets all node CPUs except the reserved
# CPU 15.
claim pod0 3 3
cpus0=$(claimed-cpus pod0)
verify "cpuset('$cpus0') == cpuset('$(node-cpus 3)') - cpuset('15')" \
       "cpus['pod0c0'] == cpuset('$cpus0')" \
       "node_ids(mems['pod0c0']) == {3}"

# Two CPUs are the two threads of one core, and a second claim on the same
# node gets the other core.
claim pod1 1 2
cpus1=$(claimed-cpus pod1)
siblings=$(vm-command-q "cat /sys/devices/system/cpu/cpu${cpus1%%[-,]*}/topology/thread_siblings_list")
verify "cpuset('$cpus1') == cpuset('$siblings')" \
       "cpuset('$cpus1') <= cpuset('$(node-cpus 1)')" \
       "cpus['pod1c0'] == cpuset('$cpus1')" \
       "node_ids(mems['pod1c0']) == {1}"
claim pod2 1 2
cpus2=$(claimed-cpus pod2)
verify "cpuset('$cpus1') | cpuset('$cpus2') == cpuset('$(node-cpus 1)')" \
       "cpus['pod2c0'] == cpuset('$cpus2')" \
       "node_ids(mems['pod2c0']) == {1}"

# No CPUs are left on node 1, so the scheduler must not place a third claim
# there, until deleting pod1 releases its claim, whose CPUs the third gets.
NAME=pod3 NODE=1 CPUS=2 wait="" create cpus-claim
NAME=pod3 wait="" create dra-pod
vm-command "kubectl wait --for=condition=PodScheduled=false pod/pod3 --timeout=60s" ||
    error "pod3 was scheduled with no CPUs left on node 1"
vm-command "kubectl delete pod pod1 --now --wait"
vm-command "kubectl wait --for=condition=Ready pod/pod3 --timeout=120s" ||
    error "pod3 did not start after pod1 released its claim"
cpus3=$(claimed-cpus pod3)
verify "cpuset('$cpus3') == cpuset('$cpus1')" \
       "cpus['pod3c0'] == cpuset('$cpus3')" \
       "node_ids(mems['pod3c0']) == {1}"

# A shared container does not run on claimed CPUs. It moves off the CPUs a
# claim takes of its node, and back once the claim is released.
NAME=pod4 create besteffort
report allowed
verify "cpus['pod4c0'].isdisjoint(cpuset('$cpus0,$cpus2,$cpus3'))"
node=$(pyexec 'print(min(node_ids(nodes["pod4c0"])))')
shared=$(pyexec 'print(sorted(cpus["pod4c0"]))')
claim pod5 "$node" 2
cpus5=$(claimed-cpus pod5)
verify "cpuset('$cpus5') <= set($shared)" \
       "cpus['pod5c0'] == cpuset('$cpus5')" \
       "node_ids(mems['pod5c0']) == {$node}" \
       "cpus['pod4c0'] == set($shared) - cpuset('$cpus5')"
vm-command "kubectl delete pod pod5 --now --wait"
# Deleting the pod does not wait for the kubelet to unprepare its claim.
retry-until --timeout 30 --message "pod4 to get back the CPUs of pod5's claim" \
    '[ "$(report allowed >/dev/null; pyexec "print(cpus[\"pod4c0\"] == set($shared))")" == True ]'
verify "cpus['pod4c0'] == set($shared)"

# Two containers of one pod share its claim. Both run on the claim's CPUs.
CONTCOUNT=2 claim pod6 0 2
cpus6=$(claimed-cpus pod6)
verify "cpus['pod6c0'] == cpuset('$cpus6')" \
       "cpus['pod6c1'] == cpuset('$cpus6')" \
       "node_ids(mems['pod6c0']) == {0}" \
       "node_ids(mems['pod6c1']) == {0}" \
       "cpus['pod4c0'].isdisjoint(cpuset('$cpus6'))"

# A container with a CPU request of its own also gets shared CPUs. Node 2 has
# no other claims, so pod7 runs on all its CPUs: the 2 claimed ones and the 2
# shared ones.
OWNCPU=100m claim pod7 2 2
cpus7=$(claimed-cpus pod7)
verify "cpuset('$cpus7') <= cpuset('$(node-cpus 2)')" \
       "cpus['pod7c0'] == cpuset('$(node-cpus 2)')" \
       "node_ids(mems['pod7c0']) == {2}" \
       "cpus['pod4c0'].isdisjoint(cpuset('$cpus7'))"

vm-command "kubectl get deviceclasses,resourceslices,resourceclaims -o yaml" || :

cleanup

echo "OK: the topology-aware policy published one DRA device per NUMA node, gave each claim CPUs of its own, and pinned containers to their claim's CPUs and to the memory of its node"
