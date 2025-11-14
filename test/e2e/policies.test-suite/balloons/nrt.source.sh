export nrt_kubectl_get="kubectl get noderesourcetopologies.topology.node.k8s.io \$(hostname)"

nrt-verify-zone-attribute() {
    local zone_name=$1
    local attribute_name=$2
    local expected_value_re=$3
    echo ""
    echo "### Verifying topology zone $zone_name attribute $attribute_name value matches $expected_value_re"
    vm-command "$nrt_kubectl_get -o json | jq -r '.zones[] | select (.name == \"$zone_name\").attributes[] | select(.name == \"$attribute_name\").value'"
    [[ "$COMMAND_OUTPUT" =~ $expected_value_re ]] ||
        command-error "expected zone $zone_name attribute $attribute_name value $expected_value, got: $COMMAND_OUTPUT"
}

nrt-verify-zone-resource() {
    local zone_name=$1
    local resource_name=$2
    local resource_field=$3
    local expected_value=$4
    echo ""
    echo "### Verifying topology zone $zone_name resource $resouce_name field $resource_field equals $expected_value"
    vm-command "$nrt_kubectl_get -o json | jq -r '.zones[] | select (.name == \"$zone_name\").resources[] | select(.name == \"$resource_name\").$resource_field'"
    [[ "$COMMAND_OUTPUT" == "$expected_value" ]] ||
        command-error "expected zone $zone_name resource $resource_name.$resource_field $expected_value, got: $COMMAND_OUTPUT"
}
