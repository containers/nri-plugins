cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
}

cleanup
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# container pod0c0 has no annotations, default: dram, pmem
ANN1="memory-type.resource-policy.nri.io/container.pod0c1: dram" \
    ANN2="memory-type.resource-policy.nri.io/container.pod0c2: hbm" \
    ANN3="memory-type.resource-policy.nri.io/container.pod0c3: pmem" \
    CONTCOUNT=4 \
    CPU=100m \
    MEM=512M \
    create guaranteed

report allowed
verify 'mems["pod0c0"] == {dram0, pmem0} if packages["pod0c0"] == {"package0"} else mems["pod0c0"] == {dram1, pmem1}' \
       'mems["pod0c1"] == {dram0}        if packages["pod0c1"] == {"package0"} else mems["pod0c1"] == {dram1}' \
       'mems["pod0c2"] == {hbm0}         if packages["pod0c2"] == {"package0"} else mems["pod0c2"] == {hbm1}' \
       'mems["pod0c3"] == {pmem0}        if packages["pod0c3"] == {"package0"} else mems["pod0c3"] == {pmem1}'

# Release memory allocated for pod0c*. If something is left behind in
# hbm or dram, the next text fails. If not, it will
vm-command "kubectl delete pods pod0 --now"

ANN0="memory-type.resource-policy.nri.io/container.pod1c0: hbm,dram" \
    ANN1="memory-type.resource-policy.nri.io/container.pod1c1: hbm,dram" \
    ANN2="memory-type.resource-policy.nri.io/container.pod1c2: pmem" \
    ANN3="memory-type.resource-policy.nri.io/container.pod1c3: pmem" \
    CONTCOUNT=4 \
    CPU=100m \
    MEM=2816M \
    create guaranteed

report allowed
verify 'mems["pod1c0"] == {hbm0, dram0} if packages["pod1c0"] == {"package0"} else mems["pod1c0"] == {hbm1, dram1}' \
       'mems["pod1c1"] == {hbm0, dram0} if packages["pod1c1"] == {"package0"} else mems["pod1c1"] == {hbm1, dram1}' \
       'mems["pod1c2"] == {pmem0}       if packages["pod1c2"] == {"package0"} else mems["pod1c2"] == {pmem1}' \
       'mems["pod1c3"] == {pmem0}       if packages["pod1c3"] == {"package0"} else mems["pod1c3"] == {pmem1}'

# 2.6G + 2.6G of PMEM is consumed, 1.4G + 1.4G remains. One more 2.0G
# pmem allocation does not fit into any single PMEM node. libmem will
# first find an initial placement without considering overflow of any
# nodes. Then it will attempt resolving overflows by spreading lower
# priority requests until no zones overflow. Priority is increasing
# by request QoS class, size, and age. Here all containers are of the
# same (guaranteed) QoS class, and pod1 containers have larger size
# than pod2. Therefore pod2c0 should end up spread over {pmem0, pmem1}.

ANN0="memory-type.resource-policy.nri.io/container.pod2c0: pmem" \
    CONTCOUNT=1 \
    CPU=100m \
    MEM=2G \
    create guaranteed

report allowed
verify 'mems["pod1c0"] == {hbm0, dram0} if packages["pod1c0"] == {"package0"} else mems["pod1c0"] == {hbm1, dram1}' \
       'mems["pod1c1"] == {hbm0, dram0} if packages["pod1c1"] == {"package0"} else mems["pod1c1"] == {hbm1, dram1}' \
       'mems["pod1c2"] == {pmem0}       if packages["pod1c2"] == {"package0"} else mems["pod1c2"] == {pmem1}' \
       'mems["pod1c3"] == {pmem0}       if packages["pod1c3"] == {"package0"} else mems["pod1c3"] == {pmem1}' \
       'mems["pod2c0"] == {pmem0, pmem1}'

cleanup
