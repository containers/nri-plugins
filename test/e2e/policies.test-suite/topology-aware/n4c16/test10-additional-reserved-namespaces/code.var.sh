# Test that
# - containers marked in ReservedPoolNamespaces option pinned on Reserved CPUs.

create-namespaces reserved-test

# This script will create pods to the reserved and default namespace.
# Make sure the namespace is clear when starting the test and clean it up
# if exiting with success. Otherwise leave the pod running for
# debugging in case of a failure.
cleanup-test-pods() {
    delete-pods -n kube-system pod0
    delete-pods pod1
}
cleanup-test-pods

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

cleanup-test-pods

# Test that
# - containers that are namespace-assigned to reserved pools are pinned there
# - containers that are annotated to opt-put that are pinned elsewhere, and
# - containers that are namespace-assigned and annotated to reserved pools are pinned there

create-namespaces foobar

cleanup-foobar-namespace() {
    delete-pods -n foobar --all
}
cleanup-foobar-namespace

CONTCOUNT=1 namespace=foobar create besteffort
ANN0='prefer-reserved-cpus.resource-policy.nri.io/pod: "false"'
CONTCOUNT=1 namespace=foobar create besteffort
ANN0='prefer-reserved-cpus.resource-policy.nri.io/pod: "true"'
CONTCOUNT=1 namespace=foobar create besteffort

report allowed
verify 'cpus["pod2c0"] == {"cpu10", "cpu11"}'
verify 'cpus["pod3c0"] == {"cpu08", "cpu09"}'
verify 'cpus["pod4c0"] == {"cpu10", "cpu11"}'

cleanup-foobar-namespace

helm-terminate
