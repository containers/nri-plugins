# This test verifies that with DRANodeAllocatableResources the scheduler
# subtracts the CPUs of claims on balloon devices from node allocatable CPU,
# so that native CPU requests of regular pods cannot overcommit the CPUs of
# DRA balloons, and that devices of balloon types with
# dra.nodeAllocatable: false are published without the mapping, so that
# the scheduler does not count their claims.

cleanup() {
    vm-command "kubectl delete pods --all --now --wait" || :
    vm-command "kubectl delete resourceclaimtemplates --all --now" || :
    helm-terminate || :
    remove-policy-cache
}

dra-devices() {
    # Usage: dra-devices
    #
    # Print the sorted names of the devices the plugin publishes.
    vm-command-q "kubectl get resourceslices -o json | jq -r '[.items[] |
                      select(.spec.driver == \"balloons.nri.io\") | .spec.devices[].name] |
                      sort | join(\" \")'"
}

dra-device() {
    # Usage: dra-device FIELD NAME
    #
    # Print FIELD of the published device NAME.
    vm-command-q "kubectl get resourceslices -o json | jq -r '.items[] |
                      select(.spec.driver == \"balloons.nri.io\") | .spec.devices[] |
                      select(.name == \"$2\") | .$1'"
}

milli-cpus() {
    # Usage: milli-cpus QUANTITY
    #
    # Print a CPU quantity, like "15" or "950m", in milli-CPUs.
    case "$1" in
        *m) echo "${1%m}";;
        *) echo "$(( $1 * 1000 ))";;
    esac
}

# Native CPU requests are given explicitly where wanted.
CPUREQ=""
CPULIM=""

cleanup
relaunch-policy balloons "$TEST_DIR/balloons-dra.cfg"

retry-until --timeout 30 --message "the plugin to publish its devices" \
    '[ "$(dra-device name fast-0)" == "fast-0" ]' ||
    error "the plugin did not publish fast-0, devices: \"$(dra-devices)\""

# Without DRANodeAllocatableResources the API server drops the field.
if [ "$(dra-device nodeAllocatableResources fast-0)" == "null" ]; then
    cleanup
    echo "Test verdict: SKIP (DRANodeAllocatableResources is not enabled)"
    exit 0
fi
vm-command "kubectl get resourceslices -o json | jq -c '.items[].spec.devices[] | [.name, .nodeAllocatableResources]'"

# The lazy balloon type has dra.nodeAllocatable: false.
[ "$(dra-device nodeAllocatableResources lazy-0)" == "null" ] ||
    error "lazy-0 has nodeAllocatableResources \"$(dra-device nodeAllocatableResources lazy-0)\", expected none"

# Native CPU requests of the pods already on the node, read before any
# claim exists so that mapped claim CPUs cannot be included.
native_m=$(milli-cpus "$(vm-command-q "kubectl describe node" |
                         awk '/^Allocated resources:/{f=1} f && $1 == "cpu" {print $2; exit}')")
alloc_m=$(milli-cpus "$(vm-command-q "kubectl get nodes -o json | jq -r '.items[0].status.allocatable.cpu'")")
echo "node allocatable cpu: ${alloc_m}m, natively requested: ${native_m}m"

# Claim 10 mapped CPUs: two whole fast balloons and the whole solo balloon.
NAME=fast-whole BTYPE=fast wait="" create dra-claim
NAME=solo-whole BTYPE=solo wait="" create dra-claim
NAME=lazy-whole BTYPE=lazy wait="" create dra-claim
NAME=pod0 CLAIM=fast-whole create dra-pod
NAME=pod1 CLAIM=fast-whole create dra-pod
NAME=pod2 CLAIM=solo-whole create dra-pod
report allowed
verify 'len(cpus["pod0c0"]) == 4' \
       'len(cpus["pod1c0"]) == 4' \
       'len(cpus["pod2c0"]) == 2' \
       'disjoint_sets(cpus["pod0c0"], cpus["pod1c0"], cpus["pod2c0"])'

vm-command "kubectl get pod pod0 -o json | jq -c .status.nodeAllocatableResourceClaimStatuses"
jq -e 'any(.[]; .containers == ["pod0c0"] and any(.mapping[]; .name == "cpu" and .quantity == "4"))' \
   <<< "$COMMAND_OUTPUT" >/dev/null ||
    error "pod0 status does not list 4 cpus from its claim in nodeAllocatableResourceClaimStatuses"

# pod5: claim the whole lazy balloon, 2 unmapped CPUs. The policy has
# 15 - 1 - 12 = 2 CPUs left for regular balloons after this.
NAME=pod5 CLAIM=lazy-whole create dra-pod
report allowed
verify 'len(cpus["pod5c0"]) == 2' \
       'disjoint_sets(cpus["pod5c0"], cpus["pod0c0"], cpus["pod1c0"], cpus["pod2c0"])'
vm-command "kubectl get pod pod5 -o json | jq -c .status.nodeAllocatableResourceClaimStatuses"
[[ "$COMMAND_OUTPUT" == "null" || "$COMMAND_OUTPUT" == "[]" ]] ||
    error "pod5 with an unmapped claim has nodeAllocatableResourceClaimStatuses $COMMAND_OUTPUT"

# Whole CPUs the scheduler has left for native requests: only the 10
# mapped CPUs are subtracted.
free=$(( (alloc_m - native_m - 10000) / 1000 ))
echo "whole CPUs left for native requests: $free"

# pod3: one CPU more than is left must stay unschedulable. It would fit if
# the scheduler did not count the claimed CPUs.
CPUREQ="$(( free + 1 ))" CPULIM="" CONTCOUNT=1 NAME=pod3 wait="" create balloons-busybox
vm-command "kubectl wait --for=condition=PodScheduled=false pod/pod3 --timeout=60s" ||
    error "pod3 requesting $(( free + 1 )) CPUs was scheduled although only $free are left"
vm-command "kubectl get pod pod3 -o json | jq -c '.status.conditions'"
vm-command "kubectl delete pod pod3 --now --wait"

# pod4: one CPU less than is left is scheduled, because the scheduler does
# not count the CPUs of the lazy balloon, but the policy has no room for
# it in a regular balloon.
CPUREQ="$(( free - 1 ))" CPULIM="" CONTCOUNT=1 NAME=pod4 wait="" create balloons-busybox
vm-command "kubectl wait --for=condition=PodScheduled pod/pod4 --timeout=60s" ||
    error "pod4 requesting $(( free - 1 )) CPUs was not scheduled although $free are left"
verify-container-error pod4 pod4c0 "not enough free CPUs\|no suitable balloon instance available" 60

# Releasing the unmapped claim gives the policy room for pod4.
vm-command "kubectl delete pod pod5 --now --wait"
wait-assert-log-contains 'released 2000 mCPU in balloon lazy\[0\]' \
    "lazy[0] did not give its CPUs back when its claim was released" 30
vm-command "kubectl delete pod pod4 --now --wait"
CPUREQ="$(( free - 1 ))" CPULIM="" CONTCOUNT=1 NAME=pod4 create balloons-busybox
report allowed
verify "len(cpus['pod4c0']) >= $(( free - 1 ))" \
       'disjoint_sets(cpus["pod4c0"], cpus["pod0c0"], cpus["pod1c0"], cpus["pod2c0"])'

cleanup

echo "OK: the scheduler counted the CPUs of mapped DRA balloon claims, and only them, against node allocatable CPU"
