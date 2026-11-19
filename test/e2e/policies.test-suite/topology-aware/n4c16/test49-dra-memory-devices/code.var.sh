# This test verifies that the topology-aware policy publishes one DRA memory
# device per NUMA node, per socket and for the machine. A claim on a memory
# device gets memory of the device's nodes. A container holding the claim runs
# with these nodes in its cpuset.mems. The policy exports the nodes to the
# container. The device capacities follow the claimed memory. A claim with
# CPUs and memory gets both in one socket. A claim keeps its memory across a
# restart.
#
# n4c16 has two sockets. Each socket has two NUMA nodes with 2G of DRAM each.
# Socket 0 has nodes 0 and 1 and CPUs 0-7. Socket 1 has nodes 2 and 3 and
# CPUs 8-15.
#
# The scheduler allocates no claim larger than its device capacity. So this
# test cannot make a prepare fail for too little memory. Unit tests cover
# that case.

cleanup() {
    cleanup-test --helm --cache --claims
}

bytes() {
    # Usage: bytes QUANTITY
    #
    # Print QUANTITY, such as 1993Mi or 2Gi, in bytes.
    local q=$1
    case "$q" in
        *Gi) echo $(( ${q%Gi} << 30 )) ;;
        *Mi) echo $(( ${q%Mi} << 20 )) ;;
        *Ki) echo $(( ${q%Ki} << 10 )) ;;
        *) echo "$q" ;;
    esac
}

mem-capacities() {
    # Usage: mem-capacities
    #
    # Print NAME=BYTES for each memory device of the policy. Print them on
    # one line, sorted by name.
    local name value
    dra-devices | jq -r '.[] | select(.name | startswith("memdev")) | "\(.name) \(.capacity.memory.value)"' |
        sort | while read -r name value; do
            echo -n "$name=$(bytes "$value") "
        done
}

wait-mem-capacities() {
    # Usage: wait-mem-capacities EXPECTED
    #
    # Wait until the memory device capacities are EXPECTED.
    local expected="$1"
    retry-until --timeout 30 --message "memory device capacities \"$expected\"" \
        '[ "$(mem-capacities)" == "$expected " ]' ||
        error "memory device capacities are \"$(mem-capacities)\", expected \"$expected\""
}

capacity() {
    # Usage: capacity CAPACITIES DEVICE
    #
    # Print the DEVICE capacity in CAPACITIES. CAPACITIES is in the
    # mem-capacities format.
    local entry
    for entry in $1; do
        if [ "${entry%=*}" == "$2" ]; then
            echo "${entry#*=}"
        fi
    done
}

replaced() {
    # Usage: replaced CAPACITIES DEVICE=BYTES...
    #
    # Print CAPACITIES with the capacity of each DEVICE set to BYTES.
    local capacities=$1 entry change result=""
    shift
    for entry in $capacities; do
        for change in "$@"; do
            if [ "${change%=*}" == "${entry%=*}" ]; then
                entry=$change
            fi
        done
        result+="${result:+ }$entry"
    done
    echo "$result"
}

sorted() {
    # Usage: sorted ID...
    #
    # Print the IDs sorted, each once, on one line.
    tr ' ' '\n' <<< "$*" | grep . | sort -nu | tr '\n' ' '
}

memory-claim() {
    # Usage: memory-claim POD EXPR MEM
    #
    # Create POD with a claim for MEM of memory. The CEL expression EXPR
    # selects the device. Wait for the pod to start.
    NAME=$1 EXPR=$2 MEM=$3 wait="" create memory-claim
    NAME=$1 create claim-pod
}

claimed-mems() {
    # Usage: claimed-mems POD
    #
    # Print the memory nodes that the POD claim got. The policy exports them
    # to the POD container in CLAIMED_MEMS.
    exported-resource "$1" CLAIMED_MEMS
}

memory-limit() {
    # Usage: memory-limit POD
    #
    # Print the cgroup memory limit of the POD container.
    vm-command-q "kubectl exec $1 -- cat /sys/fs/cgroup/memory.max"
}

cleanup

helm_config=$(DRA_ENABLED=true instantiate helm-config.yaml) helm-launch topology-aware

wait-dra-devices

k8s-feature-enabled DRAConsumableCapacity ||
    { cleanup; skip-test "DRAConsumableCapacity is not enabled"; }

vm-command "kubectl get resourceslices -o yaml"

devices=$(dra-devices)

# The API server drops the memory mapping without DRANodeAllocatableResources.
node_allocatable=false
k8s-feature-enabled DRANodeAllocatableResources && node_allocatable=true

numa='"\(.attributes["level"].string)/\(.attributes["memoryType"].string)/\(.attributes["resource.kubernetes.io/numaNode"].int)/\(.attributes["dra.cpu/socketID"].int)"'
for name in memdevnuma000 memdevnuma001 memdevnuma002 memdevnuma003 memdevsocket000 memdevsocket001 memdevmachine; do
    got=$(dra-device "$name" "$numa")
    case $name in
        memdevnuma*) node=$((10#${name#memdevnuma})) expected="numanode/DRAM/$node/$((node / 2))" ;;
        memdevsocket*) expected="socket/DRAM/null/$((10#${name#memdevsocket}))" ;;
        memdevmachine) expected="machine/DRAM/null/null" ;;
    esac
    [ "$got" == "$expected" ] ||
        error "device $name has level/memoryType/numaNode/socketID $got, expected $expected"

    policy=$(dra-device "$name" '.capacity.memory.requestPolicy | "\(.default)/\(.validRange.min)/\(.validRange.step)"')
    [ "$policy" == "1Mi/1Mi/1Mi" ] ||
        error "device $name has request policy default/min/step $policy, expected 1Mi/1Mi/1Mi"

    if $node_allocatable; then
        mapping=$(dra-device "$name" '.nodeAllocatableResources.memory.mapping |
                                  "\(.capacityKey) x \(.capacityMultiplier)"')
        [ "$mapping" == "memory x 1" ] ||
            error "device $name maps nodeAllocatableResources.memory to $mapping, expected memory x 1"
    fi
done

idle=$(mem-capacities)
idle=${idle% }
echo "idle memory device capacities: $idle"
mi512=$((512 << 20))

# pod0 claims 512Mi of NUMA node 1. Its container runs with node 1 in
# cpuset.mems. The claim gives it no CPUs. The scheduler subtracts the claim
# from memdevnuma001 itself. So memdevnuma001 keeps its capacity. Socket 0 and
# the machine have 512Mi less to give. The container memory limit stays unset.
memory-claim pod0 'device.attributes["topology-aware.nri.io"].level == "numanode" && device.attributes["resource.kubernetes.io"].numaNode == 1' 512Mi
[ "$(claimed-mems pod0)" == "1" ] ||
    error "pod0 got claimed memory nodes \"$(claimed-mems pod0)\", expected 1"
[ "$(container-mems pod0 pod0c0)" == "1" ] ||
    error "pod0 runs on memory nodes $(container-mems pod0 pod0c0), expected 1"
[ -z "$(dra-claimed-cpus pod0)" ] ||
    error "pod0 holds no CPU claim but exports CLAIMED_CPUS=$(dra-claimed-cpus pod0)"
[ "$(memory-limit pod0)" == "max" ] ||
    error "pod0 has memory limit $(memory-limit pod0), expected max"
wait-mem-capacities "$(replaced "$idle" \
    memdevsocket000=$(( $(capacity "$idle" memdevsocket000) - mi512 )) \
    memdevmachine=$(( $(capacity "$idle" memdevmachine) - mi512 )))"

# Deleting pod0 releases its claim. The capacities grow back.
delete-pods pod0
wait-mem-capacities "$idle"

# pod1 claims memory of socket 1. The claim is larger than either node of
# socket 1. So it gets memory on both nodes. Each node then has no more free
# memory than the socket.
node2=$(capacity "$idle" memdevnuma002)
node3=$(capacity "$idle" memdevnuma003)
socket1=$(capacity "$idle" memdevsocket001)
size=$(( (node2 > node3 ? node2 : node3) + (256 << 20) ))
memory-claim pod1 'device.attributes["topology-aware.nri.io"].level == "socket" && device.attributes["dra.cpu"].socketID == 1' "$((size >> 20))Mi"
[ "$(claimed-mems pod1)" == "2-3" ] ||
    error "pod1 got claimed memory nodes \"$(claimed-mems pod1)\", expected 2-3"
[ "$(container-mems pod1 pod1c0)" == "2-3" ] ||
    error "pod1 runs on memory nodes $(container-mems pod1 pod1c0), expected 2-3"
wait-mem-capacities "$(replaced "$idle" \
    memdevnuma002=$((socket1 - size)) memdevnuma003=$((socket1 - size)) \
    memdevmachine=$(( $(capacity "$idle" memdevmachine) - size )))"
delete-pods pod1
wait-mem-capacities "$idle"

# pod2 claims 2 CPUs and 256Mi of memory in one socket. Its container runs on
# the claimed CPUs. Its cpuset.mems has the claimed nodes and the nodes of its
# CPUs.
NAME=pod2 CPUS=2 MEM=256Mi wait="" create cpu-memory-claim
NAME=pod2 create claim-pod
cpus2=$(dra-claimed-cpus pod2)
mems2=$(claimed-mems pod2)
[ -n "$cpus2" ] && [ -n "$mems2" ] ||
    error "pod2 got claimed CPUs \"$cpus2\" and memory nodes \"$mems2\", expected both"
cpu_sockets=$(sorted "$(for cpu in $(expand-cpulist "$cpus2"); do echo $((cpu / 8)); done)")
mem_sockets=$(sorted "$(for node in $(expand-cpulist "$mems2"); do echo $((node / 2)); done)")
[ "$cpu_sockets" == "$mem_sockets" ] && [ "$(wc -w <<< "$cpu_sockets")" == "1" ] ||
    error "pod2 got CPUs $cpus2 of sockets $cpu_sockets and memory nodes $mems2 of sockets $mem_sockets, expected one socket"
[ "$(container-cpus pod2 pod2c0)" == "$cpus2" ] ||
    error "pod2 runs on CPUs $(container-cpus pod2 pod2c0), expected its claimed CPUs $cpus2"
expected=$(sorted "$(for cpu in $(expand-cpulist "$cpus2"); do echo $((cpu / 4)); done) $(expand-cpulist "$mems2")")
got=$(sorted "$(expand-cpulist "$(container-mems pod2 pod2c0)")")
[ "$got" == "$expected" ] ||
    error "pod2 runs on memory nodes \"$got\", expected \"$expected\""

# pod3 claims 512Mi of NUMA node 0 next to pod2. A restart keeps both claims.
# The containers keep their CPUs and memory nodes. The capacities stay.
memory-claim pod3 'device.attributes["topology-aware.nri.io"].level == "numanode" && device.attributes["resource.kubernetes.io"].numaNode == 0' 512Mi
pinned2=$(container-mems pod2 pod2c0)
# Neither claim is on memdevmachine. So the capacity of memdevmachine drops
# by both claims. Wait for this drop. Then save the capacities.
machine=$(( $(capacity "$idle" memdevmachine) - mi512 - (256 << 20) ))
retry-until --timeout 30 --message "memdevmachine to have $machine bytes" \
    '[ "$(capacity "$(mem-capacities)" memdevmachine)" == "$machine" ]' ||
    error "memdevmachine has $(capacity "$(mem-capacities)" memdevmachine) bytes, expected $machine"
before=$(mem-capacities)
before=${before% }
restart-plugin
wait-dra-devices
wait-mem-capacities "$before"
[ "$(container-cpus pod2 pod2c0)" == "$cpus2" ] ||
    error "pod2 runs on CPUs $(container-cpus pod2 pod2c0) after a restart, expected $cpus2"
[ "$(container-mems pod2 pod2c0)" == "$pinned2" ] ||
    error "pod2 runs on memory nodes $(container-mems pod2 pod2c0) after a restart, expected $pinned2"
[ "$(container-mems pod3 pod3c0)" == "0" ] ||
    error "pod3 runs on memory nodes $(container-mems pod3 pod3c0) after a restart, expected 0"
[ "$(claimed-mems pod2)" == "$mems2" ] ||
    error "pod2 got claimed memory nodes \"$(claimed-mems pod2)\" after a restart, expected $mems2"
[ "$(claimed-mems pod3)" == "0" ] ||
    error "pod3 got claimed memory nodes \"$(claimed-mems pod3)\" after a restart, expected 0"
delete-pods pod2 pod3
wait-mem-capacities "$idle"

vm-command "kubectl get resourceslices -o yaml" || :

cleanup

echo "OK: memory devices per NUMA node, socket and machine, a NUMA claim on its node, a socket claim on both nodes of the socket, a claim with CPUs and memory in one socket, capacities that follow the claims, and claims kept across a restart"
