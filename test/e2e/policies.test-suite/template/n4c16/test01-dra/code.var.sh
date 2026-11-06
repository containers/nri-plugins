# This test verifies that the template policy publishes its allowed CPUs as a
# DRA device with consumable capacity, that it gives each claim CPUs of its
# own, and that it remembers which ones across a restart, even one which no
# longer allows them.

cleanup() {
    cleanup-test --helm --cache --claims
}

check-cpus() {
    # Usage: check-cpus POD CPUS
    #
    # Check that the claim of POD got the cpuset CPUS, as the policy exports it
    # to the container.
    local cpus
    cpus=$(dra-claimed-cpus $1)
    [ "$cpus" == "$2" ] ||
        error "$1 got CPUs \"$cpus\", expected \"$2\""
    echo "$1 got CPUs $cpus"
}

restart-plugin() {
    # Usage: restart-plugin
    #
    # Restart the plugin, keeping its cache, and keep collecting its logs.
    local ds=ds/nri-resource-policy-template
    vm-command "kubectl rollout restart -n kube-system $ds &&
                kubectl rollout status -n kube-system $ds --timeout=60s" ||
        error "failed to restart the plugin"
    vm-stop-log-collection
    vm-command "kubectl logs -f -n kube-system $ds >>nri-resource-policy.output.txt 2>&1 &"
    vm-port-forward-enable
}

cleanup

# Allow four CPUs, so that two claims of two CPUs take all of them.
helm_config=$(AVAILABLE_CPU="cpuset:4-7" DRA_ENABLED=true instantiate helm-config.yaml) \
    helm-launch template

wait-dra-devices

dra-consumable-capacity ||
    { cleanup; skip-test "DRAConsumableCapacity is not enabled"; }

capacity=$(dra-devices | jq -r '.[0].capacity["dra.cpu/cpu"].value')
[ "$capacity" == "4" ] ||
    error "the device has a capacity of \"$capacity\" CPUs, expected 4"

NAME=two-cpus CPUS=2 wait="" create cpus-claim

# The policy picks the lowest free CPUs.
NAME=pod0 CLAIM=two-cpus create dra-pod
check-cpus pod0 "4-5"
NAME=pod1 CLAIM=two-cpus create dra-pod
check-cpus pod1 "6-7"

# No CPUs are left, so the scheduler must not place a third claim, until
# deleting a pod gives its CPUs back.
NAME=pod2 CLAIM=two-cpus wait="" create dra-pod
wait-pod-condition --timeout 60 pod2 PodScheduled=false \
    "pod2 was scheduled with no CPUs left"
delete-pods pod0
wait-pod-condition --timeout 120 pod2 Ready \
    "pod2 did not start after pod0 gave its CPUs back"
check-cpus pod2 "4-5"

# A plugin which forgot its claims would give pod3 the lowest free CPUs, which
# are pod2's.
restart-plugin
delete-pods pod1
NAME=pod3 CLAIM=two-cpus create dra-pod
check-cpus pod3 "6-7"

# Changing the allowed CPUs is refused at runtime. A restart with CPUs 5-9
# keeps pod2's claim on CPUs 4-5, with a warning about CPU 4, so a new claim
# gets CPU 8, the lowest one no claim holds.
patch-policy-config '{"spec":{"availableResources":{"cpu":"cpuset:5-9"}}}'
wait-config-status Failure 30s
verify-config-status-error "needs a restart"
restart-plugin
wait-config-status Success 30s
vm-command "kubectl logs -n kube-system ds/nri-resource-policy-template |
            grep 'keeps CPUs 4, which are not allowed any more'" ||
    error "the plugin did not warn about the CPUs of pod2's claim"
NAME=one-cpu CPUS=1 wait="" create cpus-claim
NAME=pod4 CLAIM=one-cpu create dra-pod
check-cpus pod4 "8"

vm-command "kubectl get deviceclasses,resourceslices,resourceclaims -o yaml" || :

cleanup

echo "OK: the template policy gave each claim CPUs of its own, across a restart, even one which no longer allowed them"
