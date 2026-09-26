# This test verifies that the template policy publishes its allowed CPUs as a
# DRA device with consumable capacity, that it gives each claim CPUs of its
# own, and that it remembers which ones across a restart.

cleanup() {
    vm-command "kubectl delete pods --all --now --wait" || :
    vm-command "kubectl delete resourceclaimtemplates --all --now" || :
    helm-terminate || :
    remove-policy-cache
}

device() {
    # Usage: device FIELD
    #
    # Print FIELD of the device the plugin publishes.
    vm-command-q "kubectl get resourceslices -o json | jq -r '.items[] |
                      select(.spec.driver == \"template.nri.io\") | .spec.devices[0].$1'"
}

check-cpus() {
    # Usage: check-cpus POD CPUS
    #
    # Check that the claim of POD got CPUS, as the container sees them.
    local cpus
    cpus=$(vm-command-q "kubectl exec $1 -- env" |
               sed -n 's/^NRI_TEMPLATE_CPU\([0-9]*\)=claimed$/\1/p' | sort -n | xargs)
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

retry-until --timeout 30 --message "the plugin to publish its device" \
    '[ -n "$(device name)" ]' ||
    error "the plugin published no device"

# Without DRAConsumableCapacity the API server drops the field which lets
# claims share the device.
if [ "$(device allowMultipleAllocations)" != "true" ]; then
    cleanup
    echo "Test verdict: SKIP (DRAConsumableCapacity is not enabled)"
    exit 0
fi

capacity=$(device capacity.cpus.value)
[ "$capacity" == "4" ] ||
    error "the device has a capacity of \"$capacity\" CPUs, expected 4"

NAME=two-cpus CPUS=2 wait="" create cpus-claim

# The policy picks the lowest free CPUs.
NAME=pod0 CLAIM=two-cpus create dra-pod
check-cpus pod0 "4 5"
NAME=pod1 CLAIM=two-cpus create dra-pod
check-cpus pod1 "6 7"

# No CPUs are left, so the scheduler must not place a third claim, until
# deleting a pod gives its CPUs back.
NAME=pod2 CLAIM=two-cpus wait="" create dra-pod
vm-command "kubectl wait --for=condition=PodScheduled=false pod/pod2 --timeout=60s" ||
    error "pod2 was scheduled with no CPUs left"
vm-command "kubectl delete pod pod0 --now --wait"
vm-command "kubectl wait --for=condition=Ready pod/pod2 --timeout=120s" ||
    error "pod2 did not start after pod0 gave its CPUs back"
check-cpus pod2 "4 5"

# A plugin which forgot its claims would give pod3 the lowest free CPUs, which
# are pod2's.
restart-plugin
vm-command "kubectl delete pod pod1 --now --wait"
NAME=pod3 CLAIM=two-cpus create dra-pod
check-cpus pod3 "6 7"

cleanup

echo "OK: the template policy gave each claim CPUs of its own, across a restart"
