# Test annotations:
# - prefer-reserved-cpus
# - cpu.preserve
# - memory.preserve

cleanup-test-pods() {
    ( vm-command "kubectl delete pods pod0 --now" ) || true
    ( vm-command "kubectl delete pods pod1 --now" ) || true
    ( vm-command "kubectl delete pods pod2 --now" ) || true
    ( vm-command "kubectl delete pods pod3 --now" ) || true
}
cleanup-test-pods

helm-terminate

AVAILABLE_CPU="cpuset:8-11"
RESERVED_CPU="cpuset:10-11"
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

ANNOTATIONS='prefer-reserved-cpus.resource-policy.nri.io/pod: "true"'
CONTCOUNT=1 create reserved-annotated
report allowed
verify 'cpus["pod0c0"] == {"cpu10", "cpu11"}'

ANNOTATIONS='prefer-reserved-cpus.resource-policy.nri.io/container.special: "false"'
CONTCOUNT=1 create reserved-annotated
report allowed
verify 'cpus["pod1c0"] == {"cpu08"}'

vm-command "kubectl delete pods pod0 --now"
ANNOTATIONS=(
    'prefer-reserved-cpus.resource-policy.nri.io/pod: "true"'
    'hide-hyperthreads.resource-policy.nri.io/pod: "true"'
)
CONTCOUNT=1 create reserved-annotated
report allowed
verify 'cpus["pod2c0"] == {"cpu10"}'

ANNOTATIONS=(
    'cpu.preserve.resource-policy.nri.io: "true"'
    'memory.preserve.resource-policy.nri.io/container.pod3c1: "true"'
    'memory.preserve.resource-policy.nri.io/container.pod3c2: "true"'
    'cpu.preserve.resource-policy.nri.io/container.pod3c2: "false"'
    'cpu.preserve.resource-policy.nri.io/container.pod3c3: "false"'
    'memory.preserve.resource-policy.nri.io/container.pod3c3: "false"'
)
CONTCOUNT=4 CPU=100m MEM=100M create reserved-annotated
report allowed

verify 'len(cpus["pod3c0"]) == 16' \
       'len(mems["pod3c0"]) == 4' \
       'len(cpus["pod3c1"]) == 16' \
       'len(mems["pod3c1"]) == 4' \
       'len(cpus["pod3c2"]) == 1' \
       'len(mems["pod3c2"]) == 4' \
       'len(cpus["pod3c3"]) == 1' \
       'len(mems["pod3c3"]) == 1'

cleanup-test-pods

helm-terminate
