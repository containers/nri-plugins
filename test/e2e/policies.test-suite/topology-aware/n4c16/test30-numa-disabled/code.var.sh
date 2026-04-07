vm-command '[ -d /sys/devices/system/node ]' && vm-kernel-pkgs-install

vm-command '[ -d /sys/devices/system/node ]' && error "failed to disable NUMA in kernel"

helm-terminate
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# pod0: Test that 2 containers are spread on two sockets
# even if they would seem to fit in the same fake NUMA node 0.
CONTCOUNT=2 CPU=5 create guaranteed
report allowed
verify \
    'len(cpus["pod0c0"]) == 5' \
    'len(cpus["pod0c1"]) == 5' \
    'disjoint_sets(cpus["pod0c0"], cpus["pod0c1"])' \
    'disjoint_sets(packages["pod0c0"], packages["pod0c1"])'

vm-command "kubectl delete pods --all --now"

helm-terminate
vm-kernel-pkgs-uninstall
