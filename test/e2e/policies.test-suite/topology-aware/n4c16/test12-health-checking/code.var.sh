# Test that syntatically incorrect configuration file is resulting in
# the nri-resmgr pod readiness probe failure.

dummyData="foo: bar"
vm-put-file $(instantiate nri-resmgr-configmap.yaml) nri-resmgr-configmap.yaml

terminate nri-resmgr
nri_resmgr_config=fallback launch nri-resmgr

# Check that nri-resmgr readiness probe fails as expected
vm-command "kubectl apply -f nri-resmgr-configmap.yaml"
namespace=kube-system wait=Ready=false wait_t=60 vm-wait-pod-regexp nri-resmgr-
if [ $? -ne 0 ]; then
    error "Expected readiness probe to fail, but got it succeeded..."
fi
echo "nri-resmgr readiness probe failed as expected..."

# Fix the incorrect data in the configuration and check if nri-rm becomes ready
dummyData=""
vm-put-file $(instantiate nri-resmgr-configmap.yaml) nri-resmgr-configmap.yaml
vm-command "kubectl apply -f nri-resmgr-configmap.yaml"
namespace=kube-system wait=Ready=true wait_t=60 vm-wait-pod-regexp nri-resmgr-
if [ $? -ne 0 ]; then
    error "Expected readiness probe to succeed, but it failed..."
fi
echo "nri-resmgr readiness probe succeeded as expected..."

terminate nri-resmgr
