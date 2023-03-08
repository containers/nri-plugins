# Test that
# - containers marked in Annotations pinned on Reserved CPUs.

cleanup-test-pods() {
    ( vm-command "kubectl delete pods pod0 --now" ) || true
    ( vm-command "kubectl delete pods pod1 --now" ) || true
}
cleanup-test-pods

terminate nri-resmgr

AVAILABLE_CPU="cpuset:8-11"
RESERVED_CPU="cpuset:10-11"
nri_resmgr_cfg=$(instantiate nri-resmgr-reserved-annotations.cfg)
launch nri-resmgr

ANNOTATIONS='prefer-reserved-cpus.nri-resmgr.intel.com/pod: "true"'
CONTCOUNT=1 create reserved-annotated
report allowed

ANNOTATIONS='prefer-reserved-cpus.nri-resmgr.intel.com/container.special: "false"'
CONTCOUNT=1 create reserved-annotated
report allowed

verify 'cpus["pod0c0"] == {"cpu10", "cpu11"}'
verify 'cpus["pod1c0"] == {"cpu08"}'

cleanup-test-pods

terminate nri-resmgr
