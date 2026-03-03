
cleanup() {
    vm-command "kubectl delete pods --all --now"
    vm-command "kubectl delete namespaces highprio lowprio --now --ignore-not-found"
}

cleanup
helm-terminate

helm_config=$TEST_DIR/helm-config.yaml helm-launch topology-aware

# Limit burstability of a container to an L3 cache group and verify that
# it gets confined to an L3 cache group.
ANN0='unlimited-burstable.resource-policy.nri.io/container.pod0c0: l3cache'
ANN1='unlimited-burstable.resource-policy.nri.io/container.pod0c1: system'
ANN2='unlimited-burstable.resource-policy.nri.io/container.pod0c2: socket'
CONTCOUNT=3 CPUREQ=1500m CPULIM=0 MEMREQ=100M create burstable
report allowed
verify \
    'len(cpus["pod0c0"]) == 8' \
    'len(cpus["pod0c1"]) == 127' \
    'len(cpus["pod0c2"]) == 64'

