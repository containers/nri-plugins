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

jsonpath="{.status.nodes['$VM_HOSTNAME'].status}"
vm-command "kubectl wait -n kube-system topologyawarepolicies/default \
                --for=jsonpath=\"$jsonpath\"=\"Success\" --timeout=5s" || {
    echo "Unexpected config status:"
    vm-command "kubectl get -n kube-system topologyawarepolicies/default \
                    -o jsonpath=\"{.status}\" | jq ."
    error "expected initial Success status"
}

# verify propagation of errors back to source CR
vm-put-file $(RESERVED_CPU=750x instantiate custom-config.yaml) broken-config.yaml
vm-command "kubectl apply -f broken-config.yaml"

sleep 1

vm-command "kubectl wait -n kube-system topologyawarepolicies/default \
                --for=jsonpath=\"$jsonpath\"=\"Failure\" --timeout=5s" || {
    echo "Unexpected config status:"
    vm-command "kubectl get -n kube-system topologyawarepolicies/default \
                    -o jsonpath=\"{.status}\" | jq ."
    error "expected post-update Failure status"
}

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
