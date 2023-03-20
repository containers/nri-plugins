# This test verifies that configuration updates via nri-resource-policy-agent
# are handled properly in the balloons policy.

terminate nri-resource-policy
launch nri-resource-policy

testns=e2e-balloons-test06

cleanup() {
    vm-command "kubectl delete pods --all --now; \
        kubectl delete pods -n $testns --all --now; \
        kubectl delete pods -n btype1ns0 --all --now; \
        kubectl delete namespace $testns || :; \
        kubectl delete namespace btype1ns0 || :; \
	kubectl -n kube-system delete configmap nri-resource-policy-config.default || :"
    vm-port-forward-disable
    terminate nri-resource-policy

    # Just in case the cache says that the policy is "topology-aware"
    # (from earlier tests) then remove the cache to force "balloons" policy
    vm-command "rm -f /var/lib/nri-resource-policy/cache" || true
}

apply-configmap() {
    vm-put-file $(instantiate balloons-configmap.yaml) balloons-configmap.yaml
    vm-command "cat balloons-configmap.yaml"
    vm-command "kubectl apply -f balloons-configmap.yaml"
}

cleanup
nri_resource_policy_extra_args="-metrics-interval 1s" nri_resource_policy_config=fallback launch nri-resource-policy

vm-command "kubectl create namespace $testns"
vm-command "kubectl create namespace btype1ns0"

AVAILABLE_CPU="cpuset:0,4-15" BTYPE2_NAMESPACE0='"*"' BTYPE1_MAXCPUS='0' apply-configmap
sleep 3
vm-port-forward-enable

# pod0 in btype0, annotation
CPUREQ=1 MEMREQ="100M" CPULIM=1 MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.nri-resmgr.intel.com: btype0" create balloons-busybox
# pod1 in btype1, namespace
CPUREQ=1 MEMREQ="100M" CPULIM=1 MEMLIM="100M"
namespace="btype1ns0" create balloons-busybox
# pod2 in btype2, wildcard namespace
CPUREQ=1 MEMREQ="100M" CPULIM=1 MEMLIM="100M"
namespace="e2e-balloons-test06" create balloons-busybox
vm-command "curl -s $verify_metrics_url"
verify-metrics-has-line 'btype0\[0\].*containers=".*pod0:pod0c0'
verify-metrics-has-line 'btype1\[0\].*containers=".*pod1:pod1c0'
verify-metrics-has-line 'btype2\[0\].*containers=".*pod2:pod2c0'

# Remove first two balloon types, change btype2 to match all
# namespaces.
BTYPE0_SKIP=1 BTYPE1_SKIP=1 BTYPE2_NAMESPACE0='"*"' apply-configmap
# Note:

# pod0 was successfully assigned to and running in balloon of btype0.
# Now btype0 was completely removed from the node.
# Currently this behavior is undefined.
# Possible behaviors: evict pod0, continue assign chain, refuse config...
# For now, skip pod0c0 balloon validation:
# verify-metrics-has-line '"btype2\[0\]".*pod0:pod0c0'
verify-metrics-has-line '"btype2\[0\]".*pod1:pod1c0'
verify-metrics-has-line '"btype2\[0\]".*pod2:pod2c0'

# Bring back btype0 where pod0 belongs to by annotation.
BTYPE1_SKIP=1 BTYPE2_NAMESPACE0='"*"' apply-configmap
verify-metrics-has-line '"btype0\[0\]".*pod0:pod0c0'
verify-metrics-has-line '"btype2\[0\]".*pod1:pod1c0'
verify-metrics-has-line '"btype2\[0\]".*pod2:pod2c0'

# Change only CPU classes, no reassigning.
verify-metrics-has-line 'btype0\[0\].*pod0:pod0c0.*cpu_class="classA"'
verify-metrics-has-line 'btype2\[0\].*pod1:pod1c0.*cpu_class="classC"'
verify-metrics-has-line 'btype2\[0\].*pod2:pod2c0.*cpu_class="classC"'
BTYPE0_CPUCLASS="classC" BTYPE1_SKIP=1 BTYPE2_CPUCLASS="classB" BTYPE2_NAMESPACE0='"*"'  apply-configmap
verify-metrics-has-line 'btype0\[0\].*pod0:pod0c0.*cpu_class="classC"'
verify-metrics-has-line 'btype2\[0\].*pod1:pod1c0.*cpu_class="classB"'
verify-metrics-has-line 'btype2\[0\].*pod2:pod2c0.*cpu_class="classB"'

cleanup
