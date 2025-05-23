helm-terminate
MPOLSET=1
helm_config=$(instantiate dram-hbm-cxl.helm-config.yaml) helm-launch memory-policy

verify-policy() {
    local container=$1
    local expected=$2
    vm-command "for pid in \$(pgrep -f $container); do grep heap /proc/\$pid/numa_maps; done | head -n 1"
    local observed="$COMMAND_OUTPUT"
    if [[ "$observed" != *"$expected"*  ]]; then
        command-error "expected memory policy: $expected, got: $observed"
    fi
    echo "verify $container memory policy is $expected: ok"
}

ANN0="class.memory-policy.nri.io: prefer-one-cxl" \
ANN1="class.memory-policy.nri.io/container.pod0c1: prefer-all-cxls" \
ANN2="class.memory-policy.nri.io/container.pod0c2: interleave-max-bandwidth" \
ANN3="class.memory-policy.nri.io/container.pod0c3: bind-hbm" \
ANN4="policy.memory-policy.nri.io/container.pod0c4: |+
      mode: MPOL_BIND
      nodes: 4,5
      flags:
      - MPOL_F_STATIC_NODES" \
ANN5="policy.memory-policy.nri.io/container.pod0c5: \"\"" \
ANN6="class.memory-policy.nri.io/container.pod0c6: \"\"" \
CONTCOUNT=7 \
create besteffort

verify-policy pod0c0 'prefer=relative:4'
verify-policy pod0c1 'prefer (many):4-5'
verify-policy pod0c2 'interleave=static:0-3'
verify-policy pod0c3 'bind:2-3'
verify-policy pod0c4 'bind=static:4-5'
verify-policy pod0c5 'default' # unset pod-default with empty policy
verify-policy pod0c6 'default' # unset pod-default with empty class
