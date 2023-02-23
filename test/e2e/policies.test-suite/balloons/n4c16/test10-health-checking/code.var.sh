# Test that
# Syntatically incorrect configuration file is resulting in the nri-resmgr
# pod readiness probe failure.
vm-put-file $(instantiate nri-resmgr-configmap.yaml) nri-resmgr-configmap.yaml

terminate nri-resmgr
launch nri-resmgr
vm-command "kubectl apply -f nri-resmgr-configmap.yaml"

# Give enough time for the Kubernetes to consider the container unhealthy
sleep 10

# Check if nri-resmgr readiness probe failed as expected
if ! vm-command "kubectl get event -n kube-system --field-selector involvedObject.name=$POD | grep -q 'Readiness probe failed'" 2>&1; then
    error "Expected liveness probe to fail, but got it succeeded"
fi
echo "nri-resmgr health-check failed as expected"
