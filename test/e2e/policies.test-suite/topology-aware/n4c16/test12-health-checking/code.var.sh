# Test that
# Syntatically incorrect configuration file is resulting in the nri-resmgr
# pod readiness probe failure.
dummyData="foo: bar"
vm-put-file $(instantiate nri-resmgr-configmap.yaml) nri-resmgr-configmap.yaml

terminate nri-resmgr
nri_resmgr_config=fallback launch nri-resmgr
vm-command "kubectl apply -f nri-resmgr-configmap.yaml"

# Give enough time for the Kubernetes to consider the container not ready.
sleep 10

# Check if nri-resmgr readiness probe failed as expected
if ! vm-command "kubectl get event -n kube-system --field-selector involvedObject.name=$POD | grep -q 'Readiness probe failed'" 2>&1; then
    error "Expected readiness probe to fail, but got it succeeded"
fi
echo "nri-resmgr readiness probe failed as expected"

# Fix the incorrect data in the configuration and check if nri-rm becomes ready.
dummyData=""
vm-put-file $(instantiate nri-resmgr-configmap.yaml) nri-resmgr-configmap.yaml
vm-command "kubectl apply -f nri-resmgr-configmap.yaml"

sleep 10

if ! vm-command "kubectl get event -n kube-system --field-selector involvedObject.name=$POD | grep -q 'Readiness probe failed'" 2>&1; then
    echo "nri-resmgr readiness probe succeeded as expected"
else
    error "Expected readiness probe to succeed, but it failed"
fi
