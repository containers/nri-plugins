cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
}

cleanup
helm_config=${TEST_DIR}/topology-aware-nrt.cfg helm-launch topology-aware

get_nrt="kubectl get noderesourcetopologies.topology.node.k8s.io \$(hostname)"

verify-zone-attribute() {
    local zone_name=$1
    local attribute_name=$2
    local expected_value=$3
    vm-command "$get_nrt -o json | jq -r '.zones[] | select (.name == \"$zone_name\").attributes[] | select(.name == \"$attribute_name\").value'"
    [[ "$COMMAND_OUTPUT" == "$expected_value" ]] ||
        command-error "expected zone $zone_name attribute $attribute_name value $expected_value, got: $COMMAND_OUTPUT"
}

# Print full NRT yaml for debugging
vm-command "$get_nrt -o yaml"

# Verify selected zone attributes
verify-zone-attribute "socket #0" "memory set" "0,2,4"
verify-zone-attribute "socket #0" "shared cpuset" "0-2"
verify-zone-attribute "socket #0" "reserved cpuset" "3"

verify-zone-attribute "socket #1" "memory set" "1,3,5"
verify-zone-attribute "socket #1" "shared cpuset" "4-7"

# TODO: Perhaps IDSet.String() or maybe some other method could print
# ranges so that "memory set" below would be just "0-5".
verify-zone-attribute "root" "memory set" "0,1,2,3,4,5"
verify-zone-attribute "root" "shared cpuset" "0-2,4-7"
verify-zone-attribute "root" "reserved cpuset" "3"
