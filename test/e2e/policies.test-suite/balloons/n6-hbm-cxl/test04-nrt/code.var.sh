cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
}

cleanup
helm_config=${TEST_DIR}/balloons-nrt.cfg helm-launch balloons

export get_nrt="kubectl get noderesourcetopologies.topology.node.k8s.io \$(hostname)"

verify-zone-attribute() {
    local zone_name=$1
    local attribute_name=$2
    local expected_value_re=$3
    echo ""
    echo "### Verifying topology zone $zone_name attribute $attribute_name value matches $expected_value_re"
    vm-command "$get_nrt -o json | jq -r '.zones[] | select (.name == \"$zone_name\").attributes[] | select(.name == \"$attribute_name\").value'"
    [[ "$COMMAND_OUTPUT" =~ $expected_value_re ]] ||
        command-error "expected zone $zone_name attribute $attribute_name value $expected_value, got: $COMMAND_OUTPUT"
}

verify-zone-resource() {
    local zone_name=$1
    local resource_name=$2
    local resource_field=$3
    local expected_value=$4
    echo ""
    echo "### Verifying topology zone $zone_name resource $resouce_name field $resource_field equals $expected_value"
    vm-command "$get_nrt -o json | jq -r '.zones[] | select (.name == \"$zone_name\").resources[] | select(.name == \"$resource_name\").$resource_field'"
    [[ "$COMMAND_OUTPUT" == "$expected_value" ]] ||
        command-error "expected zone $zone_name resource $resource_name.$resource_field $expected_value, got: $COMMAND_OUTPUT"
}

# Print full NRT yaml for debugging
vm-command "$get_nrt -o yaml"

# Verify zones when fullsocket balloons do not include containers.
verify-zone-attribute "fullsocket[0]" "cpuset" "[4567]"
verify-zone-attribute "fullsocket[0]" "shared cpuset" "5-7|4,6-7|4-5,7|4-6"
verify-zone-attribute "fullsocket[0]" "excess cpus" "1"
verify-zone-attribute "fullsocket[1]" "cpuset" "0|1"
verify-zone-attribute "fullsocket[1]" "shared cpuset" "1-2|0,2"
verify-zone-attribute "fullsocket[1]" "excess cpus" "1"
verify-zone-attribute "reserved[0]" "cpuset" "3"
verify-zone-attribute "reserved[0]" "shared cpuset" "^\$"

verify-zone-resource "reserved[0]" "cpu" "capacity" "8"
verify-zone-resource "reserved[0]" "cpu" "allocatable" "6"
verify-zone-resource "fullsocket[0]" "cpu" "allocatable" "4"
verify-zone-resource "fullsocket[0]" "cpu" "available" "4"
verify-zone-resource "fullsocket[1]" "cpu" "allocatable" "4"
verify-zone-resource "fullsocket[1]" "cpu" "available" "4"

# Create burstable containers without CPU limits
CPUREQ="750m" MEMREQ="100M" CPULIM="" MEMLIM="500M"
POD_ANNOTATION='cpu.preserve.resource-policy.nri.io/container.pod0c1: "true"
    memory.preserve.resource-policy.nri.io/container.pod0c2: "true"'
CONTCOUNT=3 create balloons-busybox

# Print full NRT yaml for debugging
vm-command "$get_nrt -o yaml"

# Verify selected zone attributes
verify-zone-resource "default/pod0/pod0c0" "cpu" "capacity" "4" # balloon's + shared CPUs
verify-zone-resource "default/pod0/pod0c0" "cpu" "allocatable" "750m" # requested CPUs
verify-zone-resource "default/pod0/pod0c0" "cpu" "available" "0" # nothing available on the subzone
verify-zone-attribute "default/pod0/pod0c0" "cpuset" "4-7"
verify-zone-attribute "default/pod0/pod0c0" "memory set" "1"

verify-zone-resource "default/pod0/pod0c1" "cpu" "capacity" "" # preserve => container should not exist, not assigned into any balloon

verify-zone-resource "default/pod0/pod0c2" "cpu" "capacity" "4" # expect same balloon as pod0c0
verify-zone-resource "default/pod0/pod0c2" "cpu" "allocatable" "750m"
verify-zone-attribute "default/pod0/pod0c2" "memory set" "^\$" # preserve memory

# Create burstable containers with CPU limits
CPUREQ="200m" MEMREQ="100M" CPULIM="1500m" MEMLIM="500M"
POD_ANNOTATION=''
CONTCOUNT=2 create balloons-busybox

# Print full NRT yaml for debugging
vm-command "$get_nrt -o yaml"

verify-zone-resource "default/pod1/pod1c0" "cpu" "capacity" "1500m" # limit < allowed cpus
verify-zone-attribute "default/pod1/pod1c0" "cpuset" "0-2" # expected fullsocket[1]
