cleanup() {
    vm-command "kubectl delete pods --all --now --wait"
    return 0
}

cleanup

# Launch cri-resmgr with wanted metrics update interval and a
# configuration that opens the instrumentation http server.
helm-terminate
helm_config=${TEST_DIR}/balloons-allocator-opts.cfg helm-launch balloons

# pod0 in a 2-CPU balloon
CPUREQ="100m" MEMREQ="100M" CPULIM="100m" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: policydefaults" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cores["pod0c0"]) == 2' \
       'len(cpus["pod0c0"]) == 2' \
       '"node2" not in nodes["pod0c0"]'


# pod1 in a 2-CPU balloon
CPUREQ="100m" MEMREQ="100M" CPULIM="100m" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: topo1cores0" CONTCOUNT=1 create balloons-busybox
report allowed
verify 'len(cores["pod1c0"]) == 1' \
       'len(cpus["pod1c0"]) == 2' \
       '"node2" not in nodes["pod1c0"]'

# pod2: container 0 resizes first from 0 to 1, container 2 from 1 to 2 CPUs,
# use more cores
CPUREQ="1" MEMREQ="100M" CPULIM="1" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: topo1cores1" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'len(cores["pod2c0"]) == 2' \
       'len(cpus["pod2c0"]) == 2' \
       'cpus["pod2c0"] == cpus["pod2c1"]' \
       '"node2" not in nodes["pod2c0"]'

# make room for pod3, because now only node2 should be empty and we
# would not be able to pack tightly elsewhere.
vm-command "kubectl delete pods pod0 pod1 pod2 --now"

# pod3: container 0 resizes first from 0 to 1, container 2 from 1 to 2 CPUs,
# pack tightly
CPUREQ="1" MEMREQ="100M" CPULIM="1" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: topo0cores0" CONTCOUNT=2 create balloons-busybox
report allowed
verify 'len(cores["pod3c0"]) == 1' \
       'len(cpus["pod3c0"]) == 2' \
       'cpus["pod3c0"] == cpus["pod3c1"]' \
       '"node2" not in nodes["pod3c0"]'

# pod4 in new balloon for which node2 should have been kept free
CPUREQ="3" MEMREQ="100M" CPULIM="6" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: device-node2" CONTCOUNT=1 create balloons-busybox
report allowed
verify '{"node2"} == nodes["pod4c0"]' \
       'len(cores["pod4c0"]) == 2' \
       'len(cpus["pod4c0"]) == 3'

vm-command "kubectl delete pods pod0 pod1 pod2 --now"

# pod5 in new balloon that will not fit on node2, ignore device hint and allocate from elsewhere
CPUREQ="2" MEMREQ="100M" CPULIM="6" MEMLIM="100M"
POD_ANNOTATION="balloon.balloons.resource-policy.nri.io: device-node2" CONTCOUNT=1 create balloons-busybox
report allowed
verify '"node2" not in nodes["pod5c0"]' \
       'len(cores["pod5c0"]) == 2' \
       'len(cpus["pod5c0"]) == 2'

cleanup
helm-terminate
