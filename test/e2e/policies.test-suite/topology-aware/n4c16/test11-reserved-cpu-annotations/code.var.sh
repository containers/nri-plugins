# Test that
# - containers marked in Annotations pinned on Reserved CPUs.

cleanup-test-pods() {
    ( vm-command "kubectl delete pods pod0 --now" ) || true
    ( vm-command "kubectl delete pods pod1 --now" ) || true
}
cleanup-test-pods

helm-terminate

AVAILABLE_CPU="cpuset:8-11"
RESERVED_CPU="cpuset:10-11"
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

ANNOTATIONS='prefer-reserved-cpus.resource-policy.nri.io/pod: "true"'
CONTCOUNT=1 create reserved-annotated
report allowed

ANNOTATIONS='prefer-reserved-cpus.resource-policy.nri.io/container.special: "false"'
CONTCOUNT=1 create reserved-annotated
report allowed

verify 'cpus["pod0c0"] == {"cpu10", "cpu11"}'
verify 'cpus["pod1c0"] == {"cpu08"}'

cleanup-test-pods

helm-terminate
