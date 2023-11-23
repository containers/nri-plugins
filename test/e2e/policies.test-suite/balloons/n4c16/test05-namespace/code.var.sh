helm-terminate
helm_config=${TEST_DIR}/balloons-namespace.cfg helm-launch balloons

cleanup() {
    vm-command \
        "kubectl delete pods -n e2e-a --all --now
         kubectl delete pods -n e2e-b --all --now
         kubectl delete pods -n e2e-c --all --now
         kubectl delete pods -n e2e-d --all --now
         kubectl delete pods --all --now
         kubectl delete namespace e2e-a
         kubectl delete namespace e2e-b
         kubectl delete namespace e2e-c
         kubectl delete namespace e2e-d"
    return 0
}
cleanup

vm-command "kubectl create namespace e2e-a"
vm-command "kubectl create namespace e2e-b"
vm-command "kubectl create namespace e2e-c"
vm-command "kubectl create namespace e2e-d"

# pod0: create in the default namespace, both containers go to nsballoon[0]
CPUREQ=""
CONTCOUNT=2 create balloons-busybox
report allowed
verify 'cpus["pod0c0"] == cpus["pod0c1"]' \
       'len(cpus["pod0c0"]) == 2'

# pod1: create in the e2e-a namespace, both containers go nsballoon[1] because
# nsballoon[0] does not contain any containers in this namespace.
CPUREQ=""
namespace="e2e-a" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'cpus["pod1c0"] == cpus["pod1c1"]' \
       'len(cpus["pod0c0"]) == 2' \
       'disjoint_sets(cpus["pod0c0"], cpus["pod1c0"])'

# pod2: create in the default namespace, should go to nsballoon[0] as
# pod0, and the balloon should inflate to 4 CPUs. cpusets with pod1
# should not overlap after inflation.
CPUREQ="2" MEMREQ="100M" CPULIM="2" MEMLIM="100M"
CONTCOUNT=2 create balloons-busybox
report allowed
verify 'cpus["pod2c0"] == cpus["pod2c1"]' \
       'len(cpus["pod2c0"]) == 4' \
       'cpus["pod2c0"] == cpus["pod0c0"]' \
       'cpus["pod2c0"] == cpus["pod0c1"]' \
       'disjoint_sets(cpus["pod2c0"], cpus["pod1c0"])'

# pod3: create again in the default namespace. nsballoon[0] has
# reached the maximum capacity, nsballoon[2] should be created for
# this pod.
CPUREQ="100m" MEMREQ="100M" CPULIM="100m" MEMLIM="100M"
CONTCOUNT=2 create balloons-busybox
report allowed
verify 'cpus["pod3c0"] == cpus["pod3c1"]' \
       'len(cpus["pod3c0"]) == 2' \
       'disjoint_sets(cpus["pod3c0"], cpus["pod2c0"], cpus["pod1c0"])'

# pod4: new namespace => nsballoon[3]
CPUREQ="2" MEMREQ="100M" CPULIM="2" MEMLIM="100M"
namespace="e2e-b" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'cpus["pod4c0"] == cpus["pod4c1"]' \
       'len(cpus["pod4c0"]) == 4' \
       'disjoint_sets(cpus["pod4c0"], cpus["pod3c0"], cpus["pod2c0"], cpus["pod1c0"])'

# pod5: new namespace => nsballoon[5]
CPUREQ="100m" MEMREQ="100M" CPULIM="100m" MEMLIM="100M"
namespace="e2e-c" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'cpus["pod5c0"] == cpus["pod5c1"]' \
       'len(cpus["pod5c0"]) == 2' \
       'disjoint_sets(cpus["pod5c0"], cpus["pod4c0"], cpus["pod3c0"], cpus["pod2c0"], cpus["pod1c0"])'

# pod6: new namespace, but nsballoon[6] cannot be created because all
# CPUs are already allocated to balloons. Cannot honor the preference
# of spreading different namespaces to different balloon instances
# anymore, should fallback to balanced assignment.
CPUREQ="100m" MEMREQ="100M" CPULIM="100m" MEMLIM="100M"
namespace="e2e-d" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'cpus["pod6c0"] == cpus["pod6c1"]'

cleanup
helm-terminate
