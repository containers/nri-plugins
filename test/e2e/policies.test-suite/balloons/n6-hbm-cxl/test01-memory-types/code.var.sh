relaunch-policy balloons "${TEST_DIR}/balloons-memory-types.cfg"

cleanup() {
    delete-pods --all
}

cleanup

# pod0: all memory combinations when there is enough memory.
# CPUREQ + CONTCOUNT causes ballooon inflation after 5 containers.
POD_ANNOTATION=()
POD_ANNOTATION[0]="memory-type.resource-policy.nri.io/container.pod0c0: hbm"
POD_ANNOTATION[1]="memory-type.resource-policy.nri.io/container.pod0c1: dram"
POD_ANNOTATION[2]="memory-type.resource-policy.nri.io/container.pod0c2: pmem"
POD_ANNOTATION[3]="memory-type.resource-policy.nri.io/container.pod0c3: hbm,dram"
POD_ANNOTATION[4]="memory-type.resource-policy.nri.io/container.pod0c4: dram,pmem"
POD_ANNOTATION[5]="memory-type.resource-policy.nri.io/container.pod0c5: hbm,dram,pmem"
# pod0c0 and pod0c6 go to the same balloon type and instance that has memoryTypes specified.
# pod0c0's annotation overrides balloon type's memoryTypes. This should be effective
# to pod0c0 only, while pod0c6 should get memory pinning according to the balloon.
POD_ANNOTATION[10]="balloon.balloons.resource-policy.nri.io/container.pod0c0: mem-types"
POD_ANNOTATION[16]="balloon.balloons.resource-policy.nri.io/container.pod0c6: mem-types"
POD_ANNOTATION[17]="balloon.balloons.resource-policy.nri.io/container.pod0c7: no-mem-types"
POD_ANNOTATION[18]="balloon.balloons.resource-policy.nri.io/container.pod0c8: no-pin-mem"
CPUREQ="200m" MEMREQ="300M" CPULIM="" MEMLIM="300M" CONTCOUNT=9 create balloons-busybox
report allowed
verify 'local_mems("pod0c0", "hbm")' \
       'local_mems("pod0c1", "dram")' \
       'local_mems("pod0c2", "pmem")' \
       'local_mems("pod0c3", "hbm", "dram")' \
       'local_mems("pod0c4", "dram", "pmem")' \
       'local_mems("pod0c5", "hbm", "dram", "pmem")' \
       'local_mems("pod0c6", "hbm", "pmem")' \
       'local_mems("pod0c7", "dram")' \
       'mems["pod0c8"] == {dram0,dram1,hbm0,hbm1,pmem0,pmem1}'

cleanup

helm-terminate
