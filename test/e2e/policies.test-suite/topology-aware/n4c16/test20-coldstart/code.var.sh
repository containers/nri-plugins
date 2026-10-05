cleanup() {
    delete-pods --all
    helm-terminate
}

OVERRIDE_SYS_MEMORY_TYPE='{"2": "DRAM", "3": "PMEM"}'
DEBUG_LOGGERS="sysfs"

cleanup

helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS" \
              EXTRA_ENV_OVERRIDE_SYS_MEMORY_TYPE="$OVERRIDE_SYS_MEMORY_TYPE" \
    instantiate helm-config.yaml) helm-launch topology-aware

pod=pod0
ANN0_MEMTYPE='memory-type.resource-policy.nri.io/container.pod0c1: "DRAM,PMEM"' \
ANN1_COLDSTART='cold-start.resource-policy.nri.io/container.pod0c1: "{ duration: 3s }"' \
              CONTCOUNT=2 CPU=1 create guaranteed

mem=$(container-mems $pod ${pod}c1)
[ "$mem" = "3" ] || command-error "expected (PMEM) memory 3, got $mem"
echo "initial memory attachment OK ($mem), waiting for coldstart period..."
sleep 5
mem=$(container-mems $pod ${pod}c1)
[ "$mem" = "2-3" ] || command-error "expected (DRAM+PMEM) memory 2-3, got $mem"
echo "final memory attachment OK ($mem)"

cleanup
