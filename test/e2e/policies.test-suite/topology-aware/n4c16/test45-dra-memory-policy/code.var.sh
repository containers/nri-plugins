# This test verifies that a claim config sets the memory policy of the
# containers holding the claim. The nodes of the policy are resolved for
# the requests each container holds. It also verifies that a claim with a
# bad memory policy fails to prepare.
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

memory-policies() {
    # Usage: memory-policies POD CONTAINER
    #
    # Print the memory policies of the memory mappings of the first process
    # in CONTAINER of POD. Each policy is printed once.
    vm-command-q "kubectl exec $1 -c $2 -- cat /proc/1/numa_maps" | awk '{print $2}' | sort -u
}

cleanup

helm_config=$(DRA_ENABLED=true instantiate helm-config.yaml) helm-launch topology-aware

retry-until --timeout 30 --message "the plugin to publish its devices" \
    '[ -n "$(slice ".spec.devices[0].name // empty")" ]' ||
    error "the plugin published no devices"

# allowMultipleAllocations lets claims share a device. Without
# DRAConsumableCapacity, the API server drops this field.
if [ "$(slice ".spec.devices[0].allowMultipleAllocations")" != "true" ]; then
    cleanup
    echo "Test verdict: SKIP (DRAConsumableCapacity is not enabled)"
    exit 0
fi

wait="" create deviceclass

# pod0's claim asks for one CPU of node 0 and one of node 1, both in package
# 0. Its config interleaves memory on the nodes the containers may use.
# pod0c0 holds both requests, so it interleaves on nodes 0 and 1. pod0c1
# holds only the first one, so it interleaves on node 0.
NAME=pod0 MODE=interleave wait="" create mempolicy-claim
NAME=pod0 create mempolicy-pod
vm-command "kubectl exec pod0 -c pod0c0 -- cat /proc/1/numa_maps"
policies=$(memory-policies pod0 pod0c0)
[ "$policies" == "interleave:0-1" ] ||
    error "pod0c0 has memory policies \"$policies\", expected interleave:0-1"
policies=$(memory-policies pod0 pod0c1)
[ "$policies" == "interleave:0" ] ||
    error "pod0c1 has memory policies \"$policies\", expected interleave:0"

# A claim with an unknown mode fails to prepare, and the pod's events say why.
NAME=pod1 MODE=spread wait="" create mempolicy-claim
NAME=pod1 wait="" create mempolicy-pod
retry-until --timeout 60 --message "the events of pod1 to report its bad memory policy" \
    'vm-command-q "kubectl get events --field-selector involvedObject.name=pod1 -o jsonpath={.items[*].message}" |
         grep -q "invalid memory policy mode"' ||
    error "the events of pod1 do not report its bad memory policy"
vm-command "kubectl get events --field-selector involvedObject.name=pod1"

cleanup

echo "OK: claim configs set the memory policy of the containers holding the claim, with the nodes of the requests each holds, and a bad memory policy failed to prepare"
