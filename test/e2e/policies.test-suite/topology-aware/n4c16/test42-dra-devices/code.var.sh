# This test verifies that the topology-aware policy publishes one DRA
# device per NUMA node, with the node's topology as its attributes.
#
# AllocateClaim/ReleaseClaim are still stubs that always fail: no DeviceClass
# ships yet, so no claim can reach these devices. Nothing here creates a claim
# or a pod.

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

cleanup

helm_config=$(DRA_ENABLED=true instantiate helm-config.yaml) helm-launch topology-aware

retry-until --timeout 30 --message "the plugin to publish its devices" \
    '[ -n "$(slice ".spec.devices[0].name")" ]' ||
    error "the plugin published no devices"

# Without DRAConsumableCapacity the API server drops allowMultipleAllocations,
# which lets claims share a device.
if [ "$(slice ".spec.devices[0].allowMultipleAllocations")" != "true" ]; then
    cleanup
    echo "Test verdict: SKIP (DRAConsumableCapacity is not enabled)"
    exit 0
fi

vm-command "kubectl get resourceslices -o yaml"

count=$(slice ".spec.devices | length")
[ "$count" == "4" ] ||
    error "published $count devices, expected 4"

for node in 0 1 2 3; do
    name=$(slice ".spec.devices[$node].name")
    expected_name=$(printf "cpudevnuma%03d" "$node")
    [ "$name" == "$expected_name" ] ||
        error "device $node is named \"$name\", expected \"$expected_name\""

    numa=$(slice ".spec.devices[$node].attributes[\"resource.kubernetes.io/numaNode\"].int")
    [ "$numa" == "$node" ] ||
        error "device $node has numaNode $numa, expected $node"

    # n4c16's topology is two packages of two NUMA nodes each.
    socket=$(slice ".spec.devices[$node].attributes[\"dra.cpu/socketID\"].int")
    expected_socket=$((node / 2))
    [ "$socket" == "$expected_socket" ] ||
        error "device $node has socketID $socket, expected $expected_socket"

    smt=$(slice ".spec.devices[$node].attributes[\"dra.cpu/smtEnabled\"].bool")
    [ "$smt" == "true" ] ||
        error "device $node has smtEnabled \"$smt\", expected true"

    cpus=$(slice ".spec.devices[$node].capacity[\"dra.cpu/cpu\"].value")
    [[ "$cpus" =~ ^[0-9]+$ ]] && [ "$cpus" -gt 0 ] ||
        error "device $node has dra.cpu/cpu capacity \"$cpus\", expected a positive number"

    default=$(slice ".spec.devices[$node].capacity[\"dra.cpu/cpu\"].requestPolicy.default")
    min=$(slice ".spec.devices[$node].capacity[\"dra.cpu/cpu\"].requestPolicy.validRange.min")
    step=$(slice ".spec.devices[$node].capacity[\"dra.cpu/cpu\"].requestPolicy.validRange.step")
    [ "$default" == "1" ] && [ "$min" == "1" ] && [ "$step" == "1" ] ||
        error "device $node has request policy default=$default min=$min step=$step, expected 1/1/1"

    multi=$(slice ".spec.devices[$node].allowMultipleAllocations")
    [ "$multi" == "true" ] ||
        error "device $node has allowMultipleAllocations \"$multi\", expected true"

    # DRANodeAllocatableResources (k8s 1.37+): pre-1.37 API servers drop
    # this field silently, so only check it when the server kept it.
    mapping=$(slice ".spec.devices[$node].nodeAllocatableResources.cpu.mapping.capacityKey")
    if [ "$mapping" != "null" ]; then
        multiplier=$(slice ".spec.devices[$node].nodeAllocatableResources.cpu.mapping.capacityMultiplier")
        [ "$mapping" == "dra.cpu/cpu" ] && [ "$multiplier" == "1" ] ||
            error "device $node maps nodeAllocatableResources.cpu to \"$mapping\" x \"$multiplier\", expected dra.cpu/cpu x 1"
    fi

    echo "$name: numaNode $numa, socketID $socket, smtEnabled $smt, $cpus CPUs"
done

cleanup

echo "OK: the topology-aware policy published one DRA device per NUMA node"
