CONFIG_GROUP="group.test"

cleanup() {
    vm-command "kubectl delete -n kube-system topologyawarepolicies.config.nri/default" || :
    vm-command "kubectl delete -n kube-system topologyawarepolicies.config.nri/$CONFIG_GROUP" || :
    vm-command "kubectl label nodes --all config.nri/group-" || :
    helm-terminate || :
}

cleanup
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

sleep 1

wait-config-status Success

#
# Errors in the CPU constraints, which the policy refuses whenever it is
# reconfigured. Each case puts the configuration back to one which is accepted
# before the next, so that every wait below observes a status which changed.
#

# An unparseable available cpuset.
patch-policy-config '{"spec":{"availableResources":{"cpu":"cpuset:nosuchcpu"}}}'
wait-config-status Failure 30s
verify-config-status-error "failed to parse available CPU cpuset"

# An unparseable available exclude-cpuset.
patch-policy-config '{"spec":{"availableResources":{"cpu":"exclude-cpuset:nosuchcpu"}}}'
wait-config-status Failure 30s
verify-config-status-error "failed to parse available CPU cpuset"

# Available CPUs as a plain quantity, which the policy cannot make a cpuset of.
patch-policy-config '{"spec":{"availableResources":{"cpu":"4"}}}'
wait-config-status Failure 30s
verify-config-status-error "can.t handle CPU resources given as resource.Quantity"

patch-policy-config '{"spec":{"availableResources":null}}'
wait-config-status Success 30s

# An unparseable reserved cpuset.
patch-policy-config '{"spec":{"reservedResources":{"cpu":"cpuset:nosuchcpu"}}}'
wait-config-status Failure 30s
verify-config-status-error "failed to parse reserved CPU cpuset"

# A reservation which excludes every CPU there is reserves none of them.
patch-policy-config '{"spec":{"reservedResources":{"cpu":"exclude-cpuset:0-15"}}}'
wait-config-status Failure 30s
verify-config-status-error "cannot start without CPU reservation"

# Note that no reservation at all cannot be tested from here: the CRD makes
# spec.reservedResources a required field, so the policy's own check for a
# missing reservation is only reachable through a configuration which does not
# come from the CRD.

patch-policy-config '{"spec":{"reservedResources":{"cpu":"750m"}}}'
wait-config-status Success 30s

# verify propagation of errors back to source CR
vm-put-file $(RESERVED_CPU=750x instantiate custom-config.yaml) broken-config.yaml
vm-command "kubectl apply -f broken-config.yaml"

sleep 1

wait-config-status Failure

helm-terminate

# verify propagation of initial configuration errors back to source CR
vm-put-file $(CONFIG_NAME="$CONFIG_GROUP" RESERVED_CPU=750x instantiate custom-config.yaml) \
            broken-group-config.yaml
vm-command "kubectl apply -f broken-group-config.yaml" || \
    error "failed to install broken group config"
vm-command "kubectl label nodes --all config.nri/group=test" || \
    error "failed to label nodes for group config"

expect_error=1 launch_timeout=5s helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware
get-config-node-status-error topologyawarepolicies/$CONFIG_GROUP | \
    grep "failed to parse" | grep -q 750x || {
    error "expected initial error not found in status"
}

cleanup
