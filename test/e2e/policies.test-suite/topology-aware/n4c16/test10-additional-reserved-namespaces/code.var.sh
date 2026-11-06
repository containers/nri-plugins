# Test that
# - containers marked in ReservedPoolNamespaces option pinned on Reserved CPUs.

# Called between the phases of the test and at its end, so that a failing
# phase leaves its pods running for debugging.
cleanup() {
    # The test places one of its pods in kube-system, which cleanup-test
    # leaves alone, and needs foobar until its last phase is over.
    delete-pods -n kube-system pod0
    cleanup-test --preserve-namespaces foobar
}
cleanup

# Make the namespace of the second phase now, so that create-namespaces
# empties what a failed run left in it before the first phase measures the
# CPUs of the node.
create-namespaces foobar

helm-terminate
RESERVED_POOL_NAMESPACES="reserved-pool reserved-* foobar"
AVAILABLE_CPU="cpuset:8-11"
RESERVED_CPU="cpuset:10-11"
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

CONTCOUNT=1 namespace=kube-system create besteffort
CONTCOUNT=1 create besteffort
report allowed
verify 'cpus["pod0c0"] == {"cpu10", "cpu11"}'
verify 'cpus["pod1c0"] == {"cpu08", "cpu09"}'

cleanup

# Test that
# - containers that are namespace-assigned to reserved pools are pinned there
# - containers that are annotated to opt-put that are pinned elsewhere, and
# - containers that are namespace-assigned and annotated to reserved pools are pinned there

CONTCOUNT=1 namespace=foobar create besteffort
ANN0='prefer-reserved-cpus.resource-policy.nri.io/pod: "false"'
CONTCOUNT=1 namespace=foobar create besteffort
ANN0='prefer-reserved-cpus.resource-policy.nri.io/pod: "true"'
CONTCOUNT=1 namespace=foobar create besteffort

report allowed
verify 'cpus["pod2c0"] == {"cpu10", "cpu11"}'
verify 'cpus["pod3c0"] == {"cpu08", "cpu09"}'
verify 'cpus["pod4c0"] == {"cpu10", "cpu11"}'

cleanup-test --helm
