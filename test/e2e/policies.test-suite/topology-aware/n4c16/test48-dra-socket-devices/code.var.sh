# This test verifies that the topology-aware policy publishes one DRA device
# per socket next to the NUMA node devices. A claim on a socket device gets
# CPUs of the socket, and CPUs of one NUMA node when it fits in one. The
# capacity of the devices follows the CPUs which containers and claims use.
# A socket claim moves to other CPUs of its socket when a NUMA node claim
# needs its CPUs.
#
# n4c16 has two sockets of two NUMA nodes with four CPUs each: socket 0 has
# CPUs 0-7, socket 1 CPUs 8-15.

cleanup() {
    vm-command "kubectl delete pods --all --now --wait" || :
    vm-command "kubectl delete resourceclaimtemplates --all --now" || :
    helm-terminate || :
    remove-policy-cache
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
    # Print JQ of our device NAME.
    slice ".spec.devices[] | select(.name == \"$1\") | $2"
}

capacities() {
    # Usage: capacities
    #
    # Print NAME=CAPACITY of each of our devices, sorted, on one line.
    slice '.spec.devices[] | "\(.name)=\(.capacity["dra.cpu/cpu"].value)"' | sort | tr '\n' ' '
}

wait-capacities() {
    # Usage: wait-capacities EXPECTED
    #
    # Wait until the capacities of our devices are EXPECTED.
    local expected="$1"
    retry-until --timeout 30 --message "device capacities \"$expected\"" \
        '[ "$(capacities)" == "$expected " ]' ||
        error "device capacities are \"$(capacities)\", expected \"$expected\""
}

exported() {
    # Usage: exported POD VARIABLE
    #
    # Print VARIABLE from the resources the policy exports to the container
    # of POD.
    vm-command-q "kubectl exec $1 -- cat /.nri-resource-policy/resources.sh" |
        sed -n "s/^$2=\"\(.*\)\"$/\1/p"
}

lowered() {
    # Usage: lowered N DEVICE...
    #
    # Print the idle capacities with those of DEVICEs lowered by N.
    local n=$1 entry name cpus expected=""
    shift
    for entry in $idle; do
        name=${entry%=*} cpus=${entry#*=}
        if [[ " $* " == *" $name "* ]]; then
            cpus=$((cpus - n))
        fi
        expected+="${expected:+ }$name=$cpus"
    done
    echo "$expected"
}

cpu-ids() {
    # Usage: cpu-ids CPUSET
    #
    # Print the CPUs of CPUSET, such as 8-10,12, one per line.
    local range
    for range in ${1//,/ }; do
        seq "${range%-*}" "${range#*-}"
    done
}

cpu-nodes() {
    # Usage: cpu-nodes CPUSET
    #
    # Print the NUMA nodes of the CPUs of CPUSET. n4c16 has four CPUs per node.
    local cpu
    for cpu in $(cpu-ids "$1"); do
        echo $(( cpu / 4 ))
    done | sort -u | tr '\n' ' '
}

socket-claim() {
    # Usage: socket-claim POD SOCKET CPUS
    #
    # Create POD with a claim for CPUS CPUs of socket SOCKET, and wait for it to
    # start.
    NAME=$1 SOCKET=$2 CPUS=$3 wait="" create socket-claim
    NAME=$1 create claim-pod
}

numa-claim() {
    # Usage: numa-claim POD NODE CPUS
    #
    # Create POD with a claim for CPUS CPUs of NUMA node NODE, and wait for it
    # to start.
    NAME=$1 NODE=$2 CPUS=$3 wait="" create numa-claim
    NAME=$1 create claim-pod
}

cleanup

# Reserve CPU 15, so that socket 1 has one CPU less to give than socket 0.
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

# Sorted by name, as capacities prints them.
idle="cpudevmachine=15 cpudevnuma000=4 cpudevnuma001=4 cpudevnuma002=4 cpudevnuma003=3 cpudevsocket000=8 cpudevsocket001=7"
wait-capacities "$idle"

for socket in 0 1; do
    name=$(printf "cpudevsocket%03d" "$socket")
    cpus=$((8 - socket))
    got=$(device "$name" '"\(.attributes["dra.cpu/level"].string)/\(.attributes["dra.cpu/socketID"].int)/\(.attributes["dra.cpu/smtEnabled"].bool)/\(.attributes["dra.cpu/numCPUs"].int)/\(.attributes["resource.kubernetes.io/numaNode"])"')
    [ "$got" == "socket/$socket/true/$cpus/null" ] ||
        error "device $name has level/socketID/smtEnabled/numCPUs/numaNode $got, expected socket/$socket/true/$cpus/null"
    policy=$(device "$name" '.capacity["dra.cpu/cpu"].requestPolicy | "\(.default)/\(.validRange.min)/\(.validRange.step)"')
    [ "$policy" == "1/1/1" ] ||
        error "device $name has request policy default/min/step $policy, expected 1/1/1"
done
for node in 0 1 2 3; do
    name=$(printf "cpudevnuma%03d" "$node")
    level=$(device "$name" '.attributes["dra.cpu/level"].string')
    [ "$level" == "numanode" ] ||
        error "device $name has level $level, expected numanode"
done

# pod0 claims 6 CPUs of socket 1, more than one NUMA node has. The scheduler
# subtracts the claim from the socket device. A NUMA device keeps the CPUs of
# the claim in its capacity only as far as the claim can move off them, to the
# one free CPU of the socket, so both NUMA devices of socket 1 have 1 CPU.
# The claim cannot leave the socket, so the machine device leaves its CPUs out.
socket-claim pod0 1 6
cpus0=$(exported pod0 CLAIMED_CPUS)
[ "$(cpu-ids "$cpus0" | wc -l)" == "6" ] && [ "$(cpu-nodes "$cpus0")" == "2 3 " ] ||
    error "pod0 got claimed CPUs \"$cpus0\", expected 6 CPUs of NUMA nodes 2 and 3"
pinned=$(vm-command-q "kubectl exec pod0 -- cat /sys/fs/cgroup/cpuset.cpus.effective")
[ "$pinned" == "$cpus0" ] ||
    error "pod0 runs on CPUs $pinned, expected its claimed CPUs $cpus0"
expected=$(lowered 6 cpudevmachine)
wait-capacities "${expected/cpudevnuma002=4 cpudevnuma003=3/cpudevnuma002=1 cpudevnuma003=1}"

# Deleting pod0 gives the CPUs of its claim back: all 7 CPUs of socket 1 can
# be claimed again.
vm-command "kubectl delete pod pod0 --now --wait"
socket-claim pod1 1 7
cpus1=$(exported pod1 CLAIMED_CPUS)
[ "$cpus1" == "8-14" ] ||
    error "pod1 got claimed CPUs \"$cpus1\", expected 8-14"
vm-command "kubectl delete pod pod1 --now --wait"

# A claim which fits in a NUMA node gets CPUs of one node.
socket-claim pod2 0 3
cpus2=$(exported pod2 CLAIMED_CPUS)
[ "$(cpu-ids "$cpus2" | wc -l)" == "3" ] && [ "$(wc -w <<< "$(cpu-nodes "$cpus2")")" == "1" ] ||
    error "pod2 got claimed CPUs \"$cpus2\", expected 3 CPUs of one NUMA node"
node2=$(cpu-nodes "$cpus2")
node2=${node2% }
vm-command "kubectl delete pod pod2 --now --wait"

# A container with 2 exclusive CPUs lowers the capacity of every device of its
# CPUs by 2.
NAME=pod3 CPU=2 create guaranteed
cpus3=$(exported pod3 EXCLUSIVE_CPUS)
[ -n "$cpus3" ] || cpus3=$(exported pod3 ISOLATED_CPUS)
node3=$(cpu-nodes "$cpus3")
[ "$(cpu-ids "$cpus3" | wc -l)" == "2" ] && [ "$(wc -w <<< "$node3")" == "1" ] ||
    error "pod3 got exclusive CPUs \"$cpus3\", expected 2 CPUs of one NUMA node"
node3=${node3% }
wait-capacities "$(lowered 2 "$(printf "cpudevnuma%03d" "$node3")" "$(printf "cpudevsocket%03d" $((node3 / 2)))" \
    cpudevmachine)"
vm-command "kubectl delete pod pod3 --now --wait"
wait-capacities "$idle"

# pod4 claims 4 CPUs of socket 0, which land on one NUMA node. pod5 then
# claims all CPUs of that node: the claim of pod4 moves to the other node of
# the socket, and pod4 runs on its new CPUs.
socket-claim pod4 0 4
cpus4=$(exported pod4 CLAIMED_CPUS)
node4=$(cpu-nodes "$cpus4")
[ "$(cpu-ids "$cpus4" | wc -l)" == "4" ] && [ "$(wc -w <<< "$node4")" == "1" ] ||
    error "pod4 got claimed CPUs \"$cpus4\", expected 4 CPUs of one NUMA node"
node4=${node4% }
other=$((1 - node4))
numa-claim pod5 "$node4" 4
cpus5=$(exported pod5 CLAIMED_CPUS)
[ "$(cpu-ids "$cpus5" | wc -l)" == "4" ] && [ "$(cpu-nodes "$cpus5")" == "$node4 " ] ||
    error "pod5 got claimed CPUs \"$cpus5\", expected the 4 CPUs of NUMA node $node4"
retry-until --timeout 30 --message "the claim of pod4 to move to NUMA node $other" \
    '[ "$(cpu-nodes "$(exported pod4 CLAIMED_CPUS)")" == "$other " ]' ||
    error "pod4 has claimed CPUs \"$(exported pod4 CLAIMED_CPUS)\", expected the CPUs of NUMA node $other"
cpus4=$(exported pod4 CLAIMED_CPUS)
pinned=$(vm-command-q "kubectl exec pod4 -- cat /sys/fs/cgroup/cpuset.cpus.effective")
[ "$pinned" == "$cpus4" ] ||
    error "pod4 runs on CPUs $pinned, expected its claimed CPUs $cpus4"

cleanup
echo "OK: socket devices next to the NUMA devices, socket claims within their socket, a 3-CPU claim on NUMA node $node2, a socket claim moved to NUMA node $other"
