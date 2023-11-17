helm-terminate
helm_config=$TEST_DIR/balloons.cfg helm-launch balloons

sleep 1

jsonpath="{.status.nodes['$VM_HOSTNAME'].status}"
vm-command "kubectl wait -n kube-system balloonspolicies/default \
                --for=jsonpath=\"$jsonpath\"=\"Success\" --timeout=5s" || {
    echo "Unexpected config status:"
    vm-command "kubectl get -n kube-system balloonspolicies/default \
                    -o jsonpath=\"{.status}\" | jq ."
    error "expected initial Success status"
}

host-command "$SCP $TEST_DIR/broken-balloons-config.yaml ${VM_HOSTNAME}:"
vm-command "kubectl apply -f broken-balloons-config.yaml"

sleep 1

vm-command "kubectl wait -n kube-system balloonspolicies/default \
                --for=jsonpath=\"$jsonpath\"=\"Failure\" --timeout=5s" || {
    echo "Unexpected config status:"
    vm-command "kubectl get -n kube-system balloonspolicies/default \
                    -o jsonpath=\"{.status}\" | jq ."
    error "expected post-update Failure status"
}

helm-terminate
