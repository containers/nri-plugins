helm-terminate
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

source $TEST_DIR/codelib.sh || {
    echo "error importing codelib.sh"
    exit 1
}

# Clean test pods from the kube-system namespace
cleanup-test-pods() {
    ( vm-command "kubectl delete pods -n kube-system \$(kubectl get pods -n kube-system | awk '/t[0-9]r[gb][ue]/{print \$1}')" ) || true
    ( vm-command "kubectl delete pods -n default \$(kubectl get pods -n default | awk '/t[0-9][rgb][ue][0-9]/{print \$1}')" ) || true
}
cleanup-test-pods

run-generated-tests

cleanup-test-pods

# Restore default test configuration, restart nri-resource-policy.
helm-terminate
