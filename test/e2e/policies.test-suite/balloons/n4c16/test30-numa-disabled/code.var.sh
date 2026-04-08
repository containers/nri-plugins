vm-command '[ -d /sys/devices/system/node ]' && vm-kernel-pkgs-install

vm-command '[ -d /sys/devices/system/node ]' && error "failed to disable NUMA in kernel"

helm-terminate
helm_config=$TEST_DIR/balloons-numa-disabled.cfg helm-launch balloons

POD_ANNOTATION=(
    "balloon.balloons.resource-policy.nri.io: single-thread-own-core"
    "hide-hyperthreads.resource-policy.nri.io: \"true\""
)
CONTCOUNT=2 namespace="default" create balloons-busybox
report allowed
verify "len(set.union(cpus['pod0c0'], cpus['pod0c1'])) == 1" \
       "len(set.union(cpus['pod0c0'], cpus['pod0c1']).intersection({'cpu06', 'cpu07'})) == 1"

POD_ANNOTATION=()
CONTCOUNT=2 namespace="default" create balloons-busybox
report allowed
verify "cpus['pod1c0'].isdisjoint({'cpu06', 'cpu07'})" \
       "cpus['pod1c1'].isdisjoint({'cpu06', 'cpu07'})" \
       "len(cpus['pod1c0']) == 5" \
       "len(cpus['pod1c1']) == 5" \
       "disjoint_sets(cpus['pod1c0'], cpus['pod1c1'])"

vm-kernel-pkgs-uninstall
