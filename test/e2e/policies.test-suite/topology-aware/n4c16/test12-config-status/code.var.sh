helm-terminate
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
