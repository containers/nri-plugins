# This test verifies that the topology-aware policy publishes one DRA
# device per NUMA node, allocates CPUs to claims, and updates the NRT CR
# after claim changes. It also checks that a container runs on the CPUs
# of its claim, or of the claim requests it names, that the policy
# exports those CPUs to it, and that claims keep their CPUs across a
# restart. A restart with a configuration which reserves a claimed CPU moves
# the claim to other CPUs of its node. If the node has too few CPUs, the
# restart fails until the configuration is reverted.
#

cleanup() {
    cleanup-test --helm --cache --claims
}

claim() {
    # Usage: claim POD NODE CPUS
    #
    # Create POD with a claim for CPUS CPUs of NUMA node NODE, and wait for
    # it to start.
    NAME=$1 NODE=$2 CPUS=$3 wait="" create cpus-claim
    NAME=$1 create dra-pod
}

node-cpus() {
    # Usage: node-cpus NODE
    #
    # Print the CPUs of NUMA node NODE.
    vm-command-q "cat /sys/devices/system/node/node$1/cpulist"
}

claim-uid() {
    # Usage: claim-uid POD
    #
    # Print the UID of the claim of POD.
    vm-command-q "kubectl get resourceclaim -o jsonpath='{.metadata.uid}' \
        \$(kubectl get pod $1 -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}')"
}

nrt-shared-cpus() {
    # Usage: nrt-shared-cpus NODE
    #
    # Print the shared CPUs the NRT CR lists for NUMA node NODE.
    nrt-query-q ".zones[] | select(.name == \"NUMA node #$1\").attributes[] |
                 select(.name == \"shared cpuset\").value"
}

cleanup

# Reserve CPU 15, so that node 3 has one CPU less to give than the others.
helm_config=$(RESERVED_CPU="cpuset:15" DRA_ENABLED=true instantiate helm-config.yaml) \
    helm-launch topology-aware

wait-dra-devices

dra-consumable-capacity ||
    { cleanup; skip-test "DRAConsumableCapacity is not enabled"; }

vm-command "kubectl get resourceslices -o yaml"

devices=$(dra-devices)

# One device per NUMA node, one per socket and one for the machine.
count=$(jq length <<< "$devices")
[ "$count" == "7" ] ||
    error "published $count devices, expected 7"

for node in 0 1 2 3; do
    name=$(printf "cpudevnuma%03d" "$node")
    [ "$(dra-device "$name" .name)" == "$name" ] ||
        error "no device $name for NUMA node $node"

    numa=$(dra-device "$name" '.attributes["resource.kubernetes.io/numaNode"].int')
    [ "$numa" == "$node" ] ||
        error "device $name has numaNode $numa, expected $node"

    # n4c16's topology is two packages of two NUMA nodes each.
    socket=$(dra-device "$name" '.attributes["dra.cpu/socketID"].int')
    expected_socket=$((node / 2))
    [ "$socket" == "$expected_socket" ] ||
        error "device $name has socketID $socket, expected $expected_socket"

    smt=$(dra-device "$name" '.attributes["dra.cpu/smtEnabled"].bool')
    [ "$smt" == "true" ] ||
        error "device $name has smtEnabled \"$smt\", expected true"

    cpus=$(dra-device "$name" '.capacity["dra.cpu/cpu"].value')
    expected_cpus=4
    [ "$node" == "3" ] && expected_cpus=3
    [ "$cpus" == "$expected_cpus" ] ||
        error "device $name has dra.cpu/cpu capacity \"$cpus\", expected $expected_cpus"

    policy=$(dra-device "$name" '.capacity["dra.cpu/cpu"].requestPolicy |
                             "\(.default)/\(.validRange.min)/\(.validRange.step)"')
    [ "$policy" == "1/1/1" ] ||
        error "device $name has request policy default/min/step $policy, expected 1/1/1"

    multi=$(dra-device "$name" .allowMultipleAllocations)
    [ "$multi" == "true" ] ||
        error "device $name has allowMultipleAllocations \"$multi\", expected true"

    # DRANodeAllocatableResources (k8s 1.37+): pre-1.37 API servers drop
    # this field silently, so only check it when the server kept it.
    mapping=$(dra-device "$name" '.nodeAllocatableResources.cpu.mapping |
                              select(. != null) | "\(.capacityKey) x \(.capacityMultiplier)"')
    [ -z "$mapping" ] || [ "$mapping" == "dra.cpu/cpu x 1" ] ||
        error "device $name maps nodeAllocatableResources.cpu to $mapping, expected dra.cpu/cpu x 1"

    echo "$name: numaNode $numa, socketID $socket, smtEnabled $smt, $cpus CPUs"
done

# pod0 claims 3 CPUs on node 3. It gets all node CPUs except the reserved
# CPU 15.
claim pod0 3 3
cpus0=$(dra-claimed-cpus pod0)
verify "cpuset('$cpus0') == cpuset('$(node-cpus 3)') - cpuset('15')" \
       "cpus['pod0c0'] == cpuset('$cpus0')" \
       "node_ids(mems['pod0c0']) == {3}"

# Two CPUs are the two threads of one core, and a second claim on the same
# node gets the other core.
claim pod1 1 2
cpus1=$(dra-claimed-cpus pod1)
siblings=$(vm-command-q "cat /sys/devices/system/cpu/cpu${cpus1%%[-,]*}/topology/thread_siblings_list")
verify "cpuset('$cpus1') == cpuset('$siblings')" \
       "cpuset('$cpus1') <= cpuset('$(node-cpus 1)')" \
       "cpus['pod1c0'] == cpuset('$cpus1')" \
       "node_ids(mems['pod1c0']) == {1}"
claim pod2 1 2
cpus2=$(dra-claimed-cpus pod2)
verify "cpuset('$cpus1') | cpuset('$cpus2') == cpuset('$(node-cpus 1)')" \
       "cpus['pod2c0'] == cpuset('$cpus2')" \
       "node_ids(mems['pod2c0']) == {1}"

# No CPUs are left on node 1, so the scheduler must not place a third claim
# there, until deleting pod1 releases its claim, whose CPUs the third gets.
NAME=pod3 NODE=1 CPUS=2 wait="" create cpus-claim
NAME=pod3 wait="" create dra-pod
wait-pod-condition --timeout 60 pod3 PodScheduled=false \
    "pod3 was scheduled with no CPUs left on node 1"
delete-pods pod1
wait-pod-condition --timeout 120 pod3 Ready \
    "pod3 did not start after pod1 released its claim"
cpus3=$(dra-claimed-cpus pod3)
verify "cpuset('$cpus3') == cpuset('$cpus1')" \
       "cpus['pod3c0'] == cpuset('$cpus3')" \
       "node_ids(mems['pod3c0']) == {1}"

# A shared container does not run on claimed CPUs. It moves off the CPUs a
# claim takes of its node, and back once the claim is released.
NAME=pod4 create besteffort
report allowed
verify "cpus['pod4c0'].isdisjoint(cpuset('$cpus0,$cpus2,$cpus3'))"
[ -z "$(dra-claimed-cpus pod4)" ] ||
    error "pod4 holds no claim but exports CLAIMED_CPUS=$(dra-claimed-cpus pod4)"
node=$(pyexec 'print(min(node_ids(nodes["pod4c0"])))')
shared=$(pyexec 'print(sorted(cpus["pod4c0"]))')
claim pod5 "$node" 2
cpus5=$(dra-claimed-cpus pod5)
verify "cpuset('$cpus5') <= set($shared)" \
       "cpus['pod5c0'] == cpuset('$cpus5')" \
       "node_ids(mems['pod5c0']) == {$node}" \
       "cpus['pod4c0'] == set($shared) - cpuset('$cpus5')"
retry-until --timeout 30 --message "the NRT CR to drop the CPUs of pod5's claim" \
    '[ "$(pyexec "print(cpuset(\"$(nrt-shared-cpus $node)\").isdisjoint(cpuset(\"$cpus5\")))")" == True ]' ||
    error "the NRT CR still lists the CPUs of pod5's claim as shared"
delete-pods pod5
# Deleting the pod does not wait for the kubelet to unprepare its claim.
wait-verify "cpus['pod4c0'] == set($shared)"
# No container event follows the release, so only the claim path can
# refresh the NRT CR here.
retry-until --timeout 30 --message "the NRT CR to list the CPUs of pod5's claim again" \
    '[ "$(pyexec "print(cpuset(\"$cpus5\") <= cpuset(\"$(nrt-shared-cpus $node)\"))")" == True ]' ||
    error "the NRT CR does not list the CPUs released by pod5's claim as shared"

# Two containers of one pod share its claim. Both run on the claim's CPUs.
CONTCOUNT=2 claim pod6 0 2
cpus6=$(dra-claimed-cpus pod6)
verify "cpus['pod6c0'] == cpuset('$cpus6')" \
       "cpus['pod6c1'] == cpuset('$cpus6')" \
       "node_ids(mems['pod6c0']) == {0}" \
       "node_ids(mems['pod6c1']) == {0}" \
       "cpus['pod4c0'].isdisjoint(cpuset('$cpus6'))"

# A container with a CPU request of its own also gets shared CPUs. Node 2 has
# no other claims, so pod7 runs on all its CPUs: the 2 claimed ones and the 2
# shared ones.
OWNCPU=100m claim pod7 2 2
cpus7=$(dra-claimed-cpus pod7)
verify "cpuset('$cpus7') <= cpuset('$(node-cpus 2)')" \
       "cpus['pod7c0'] == cpuset('$(node-cpus 2)')" \
       "node_ids(mems['pod7c0']) == {2}" \
       "cpus['pod4c0'].isdisjoint(cpuset('$cpus7'))"
# Its exported shared CPUs are its own, apart from the claimed ones.
verify "cpuset('$(exported-resource pod7 SHARED_CPUS)') == cpuset('$(node-cpus 2)') - cpuset('$cpus7')"

# A container naming one request of a claim runs only on the CPUs of that
# request. A container naming no request runs on the CPUs of all requests.
# pod8's claim asks for one CPU of node 0 and one of node 2.
NAME=pod8 NODE_A=0 NODE_B=2 CPUS=1 wait="" create two-requests-claim
NAME=pod8 create requests-pod
report allowed
first=$(dra-claimed-cpus pod8 pod8c0)
second=$(dra-claimed-cpus pod8 pod8c1)
verify "len(cpuset('$first')) == 1" \
       "cpuset('$first') <= cpuset('$(node-cpus 0)')" \
       "len(cpuset('$second')) == 1" \
       "cpuset('$second') <= cpuset('$(node-cpus 2)')" \
       "cpus['pod8c0'] == cpuset('$first')" \
       "cpus['pod8c1'] == cpuset('$second')" \
       "cpus['pod8c2'] == cpuset('$first,$second')" \
       "cpuset('$(dra-claimed-cpus pod8 pod8c2)') == cpuset('$first,$second')" \
       "cpus['pod4c0'].isdisjoint(cpuset('$first,$second'))"

# A plugin which forgot its claims would place the containers holding them
# again without them, and could give their CPUs to a new claim. Node 0 has
# one CPU left, the only one pod9 may get. pod4 goes first: the restart may
# place it on node 0, which then keeps that CPU for it.
delete-pods pod4
restart-plugin
report allowed
verify "cpus['pod0c0'] == cpuset('$cpus0')" \
       "cpus['pod2c0'] == cpuset('$cpus2')" \
       "cpus['pod3c0'] == cpuset('$cpus3')" \
       "cpus['pod6c0'] == cpuset('$cpus6')" \
       "cpus['pod6c1'] == cpuset('$cpus6')" \
       "cpuset('$cpus7') <= cpus['pod7c0']" \
       "cpus['pod8c0'] == cpuset('$first')" \
       "cpus['pod8c1'] == cpuset('$second')" \
       "cpus['pod8c2'] == cpuset('$first,$second')"

# Reserving the CPU of pod8's request on node 0 is refused at runtime. A
# restart with it moves the request to the one free CPU of node 0. Its
# containers move with it, and so do the CPUs exported to them.
patch-policy-config "{\"spec\":{\"reservedResources\":{\"cpu\":\"cpuset:$first,15\"}}}"
# Refusing it rebuilds the pools twice, which takes a while.
wait-config-status Failure 90s
restart-plugin
wait-config-status Success 60s
report allowed
moved=$(dra-claimed-cpus pod8 pod8c0)
verify "cpuset('$moved') == cpuset('$(node-cpus 0)') - cpuset('$cpus6,$first')" \
       "cpus['pod8c0'] == cpuset('$moved')" \
       "cpus['pod8c1'] == cpuset('$second')" \
       "cpus['pod8c2'] == cpuset('$moved,$second')" \
       "cpuset('$(dra-claimed-cpus pod8 pod8c2)') == cpuset('$moved,$second')" \
       "cpus['pod0c0'] == cpuset('$cpus0')" \
       "cpus['pod6c0'] == cpuset('$cpus6')"
# Releasing the CPU again is accepted at runtime. It is then the only free
# CPU of node 0.
patch-policy-config '{"spec":{"reservedResources":{"cpu":"cpuset:15"}}}'
wait-config-status Success 90s
first=$moved

claim pod9 0 1
cpus9=$(dra-claimed-cpus pod9)
verify "cpuset('$cpus9') == cpuset('$(node-cpus 0)') - cpuset('$cpus6,$first')" \
       "cpus['pod9c0'] == cpuset('$cpus9')"

# Reserving a CPU of pod0's claim is refused at runtime. It leaves node 3 two
# CPUs, too few for the claim to move, so a restart with it fails, and the
# status of the configuration names the claim. Reverting the configuration
# lets the plugin start again, with the claims kept.
uid0=$(claim-uid pod0)
[ -n "$uid0" ] || error "pod0 has no claim"
patch-policy-config "{\"spec\":{\"reservedResources\":{\"cpu\":\"cpuset:${cpus0%%[-,]*},15\"}}}"
# Refusing it rebuilds the pools twice, which takes a while.
wait-config-status Failure 90s
restart-plugin --no-wait
wait-config-status-error "failed to initialize.*DRA claim $uid0" 60
patch-policy-config '{"spec":{"reservedResources":{"cpu":"cpuset:15"}}}'
# The plugin restarts with the crash-loop backoff of the kubelet.
follow-plugin --timeout 180s
wait-config-status Success 60s
report allowed
verify "cpus['pod0c0'] == cpuset('$cpus0')" \
       "cpus['pod6c0'] == cpuset('$cpus6')" \
       "cpus['pod9c0'] == cpuset('$cpus9')"
# Deleting pod9 releases the only free CPU of node 0, which is all a new
# claim there may get.
delete-pods pod9
claim pod10 0 1
verify "cpuset('$(dra-claimed-cpus pod10)') == cpuset('$cpus9')" \
       "cpus['pod10c0'] == cpuset('$cpus9')"

vm-command "kubectl get deviceclasses,resourceslices,resourceclaims -o yaml" || :

cleanup

echo "OK: the topology-aware policy published one DRA device per NUMA node, gave each claim CPUs of its own, and pinned containers to their claim's CPUs, or to those of the requests they named, and to the memory of its node, exported the claimed CPUs to the containers, the NRT CR followed the claims, the claims kept their CPUs across a restart, a restart which reserved a claimed CPU moved the claim, and one which left a node too few CPUs for a claim failed until the configuration was reverted"
