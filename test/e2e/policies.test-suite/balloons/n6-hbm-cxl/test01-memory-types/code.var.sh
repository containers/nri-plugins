helm-terminate
helm_config=${TEST_DIR}/balloons-memory-types.cfg helm-launch balloons

cleanup() {
    vm-command "kubectl delete pods --all --now"
    return 0
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
verify 'mems["pod0c0"] == {hbm0}             if packages["pod0c0"] == {"package0"} else mems["pod0c0"] == {hbm1}' \
       'mems["pod0c1"] == {dram0}            if packages["pod0c1"] == {"package0"} else mems["pod0c1"] == {dram1}' \
       'mems["pod0c2"] == {pmem0}            if packages["pod0c2"] == {"package0"} else mems["pod0c2"] == {pmem1}' \
       'mems["pod0c3"] == {hbm0,dram0}       if packages["pod0c3"] == {"package0"} else mems["pod0c3"] == {hbm1,dram1}' \
       'mems["pod0c4"] == {dram0,pmem0}      if packages["pod0c4"] == {"package0"} else mems["pod0c4"] == {dram1,pmem1}' \
       'mems["pod0c5"] == {hbm0,dram0,pmem0} if packages["pod0c5"] == {"package0"} else mems["pod0c5"] == {hbm1,dram1,pmem1}' \
       'mems["pod0c6"] == {hbm0,pmem0}       if packages["pod0c6"] == {"package0"} else mems["pod0c6"] == {hbm1,pmem1}' \
       'mems["pod0c7"] == {dram0}            if packages["pod0c7"] == {"package0"} else mems["pod0c7"] == {dram1}' \
       'mems["pod0c8"] == {dram0,dram1,hbm0,hbm1,pmem0,pmem1}'

cleanup

helm-terminate
