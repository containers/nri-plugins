# Test that syntatically incorrect configuration file is resulting in
# the nri-resource-policy pod readiness probe failure.

dummyData="foo: bar"
vm-put-file $(instantiate nri-resource-policy-configmap.yaml) nri-resource-policy-configmap.yaml

terminate nri-resource-policy
nri_resource_policy_config=fallback launch nri-resource-policy

# Check that nri-resource-policy readiness probe fails as expected
vm-command "kubectl apply -f nri-resource-policy-configmap.yaml"
namespace=kube-system wait=Ready=false wait_t=60 vm-wait-pod-regexp nri-resource-policy-
if [ $? -ne 0 ]; then
    error "Expected readiness probe to fail, but got it succeeded..."
fi
echo "nri-resource-policy readiness probe failed as expected..."

# Fix the incorrect data in the configuration and check if nri-rm becomes ready
dummyData=""
vm-put-file $(instantiate nri-resource-policy-configmap.yaml) nri-resource-policy-configmap.yaml
vm-command "kubectl apply -f nri-resource-policy-configmap.yaml"
namespace=kube-system wait=Ready=true wait_t=60 vm-wait-pod-regexp nri-resource-policy-
if [ $? -ne 0 ]; then
    error "Expected readiness probe to succeed, but it failed..."
fi
echo "nri-resource-policy readiness probe succeeded as expected..."

terminate nri-resource-policy
