# This test verifies that the groupby expression in a balloon type
# affects grouping containers into balloon instances of that type.

helm-terminate
helm_config=$TEST_DIR/balloons-groupby.cfg helm-launch balloons

testns=e2e-balloons-test04

cleanup() {
    vm-command "kubectl delete pods --all --now; \
        kubectl delete pods -n $testns --all --now; \
        kubectl delete namespace $testns; \
        true"
}

cleanup

POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: grouped-by-label"
# pod0c0
POD_LABEL='balloon-instance: g1'
create balloons-busybox

# pod1c0
POD_LABEL='balloon-instance: g2'
create balloons-busybox

# pod2c0
POD_LABEL='balloon-instance: g1'
create balloons-busybox

# pod3c0
POD_LABEL='balloon-instance: g1'
vm-command "kubectl create namespace $testns"
namespace=$testns create balloons-busybox
report allowed

verify 'cpus["pod0c0"] == cpus["pod2c0"]' \
       'disjoint_sets(cpus["pod0c0"], cpus["pod1c0"], cpus["pod3c0"])'

# Test that pods are grouped by namespaces in separate default
# balloon instances.
POD_ANNOTATION=""
# pod4c0
namespace=$testns create balloons-busybox
# pod5c0
create balloons-busybox
# pod6c0
create balloons-busybox
# pod7c0
namespace=$testns create balloons-busybox
report allowed
verify 'cpus["pod4c0"] == cpus["pod7c0"]' \
       'cpus["pod5c0"] == cpus["pod6c0"]' \
       'disjoint_sets(cpus["pod4c0"], cpus["pod5c0"])'

verify-metrics-has-line 'groups="e2e-balloons-test04-g1"'
verify-metrics-has-line 'groups="default-g2"'
verify-metrics-has-line 'groups="default-g1"'

cleanup

helm-terminate
