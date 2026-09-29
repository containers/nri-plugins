# This test verifies that the topology-aware policy publishes one DRA
# device per NUMA node.
#
cleanup() {
    helm-terminate
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

cleanup

echo "OK: the topology-aware policy published one DRA device per NUMA node"
