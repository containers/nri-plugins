cleanup() {
    delete-pods --all
    helm-terminate
}

# irq-cpu-ids <irq-number>
# Read the current affinity for the given interrupt from /proc/irq/$irq/smp_affinity_list.
irq-cpu-ids() {
    local pattern=$1
    local irq="" cpus=""

    irq=$(resolve-irq "$pattern")
    if [ -z "$irq" ]; then
        error "Failed to resolve IRQ for pattern: $pattern" >&2
        return 1
    fi
    $SSH -oConnectTimeout=1 node "cat /proc/irq/$irq/smp_affinity_list"
    if [ $? -ne 0 ]; then
        error "Failed to read smp_affinity_list for IRQ $irq" >&2
        return 1
    fi
}

# resolve-irq <IRQ number or proc-interrupts-pattern>
# Returns the IRQ number matching the given interrupt or pattern in /proc/interrupts.
resolve-irq() {
    local irq_or_pattern="$1"
    local irq=""

    irq=$($SSH -oConnectTimeout=1 node "cat /proc/interrupts" | \
              tr -s ' \t' ' ' | grep "^ *$irq_or_pattern:" | cut -d ':' -f1 | tr -d ' ')
    if [ -z "$irq" ]; then
        irq=$($SSH -oConnectTimeout=1 node "cat /proc/interrupts" | \
              tr -s ' \t' ' ' | grep -E "$irq_or_pattern" | cut -d ':' -f1 | tr -d ' ')
    fi

    if [ -n "$irq" ]; then
        if [ "$irq" != "$irq_or_pattern" ]; then
            echo "IRQ $irq_or_pattern resolved to $irq..." >&2
        fi
        echo $irq
        return 0
    else
        echo "IRQ not found for pattern: $irq_or_pattern" >&2
        return 1
    fi
}

# verify-irq-cpus IRQNUM EXPECTED waits until the affinity of the IRQ
# equals EXPECTED (sorted space-separated CPU ids), or fails after a
# timeout.
verify-irq-cpus() {
    local irq=$1 expected=$2 got tries=20
    irqnum=$(resolve-irq "$irq")
    while [ "$tries" -gt 0 ]; do
        got=$(irq-cpu-ids "$irqnum")
        if [ "$(expand-cpulist "$got")" == "$(expand-cpulist "$expected")" ]; then
            echo "IRQ $irqnum affinity is '$got' as expected"
            return 0
        fi
        tries=$((tries - 1))
        sleep 1
    done
    error "IRQ $irqnum affinity: expected CPUs '$expected', got '$got'"
}

cleanup

DEBUG_LOGGERS="irq"

helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS" \
    instantiate helm-config.yaml) helm-launch topology-aware

ALLCPUS=0-15

# Create pod with 4 containers, 2 eligible for exclusive CPU allocation
# and 2 others running in shared pools. Annotate the exclusive ones for
# IRQ claims. Check that the affinity of the requested IRQs get set to
# the allocated CPUs.

pod=pod0
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: [ "*ttyS0*" ]
EOF
)
ANN1=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c1: |
      claim: [ "*rtc0*" ]
EOF
)

ANN0=$ANN0 ANN1=$ANN1 \
    CPU=2 CONTCOUNT=4 create guaranteed

verify-irq-cpus ".*ttyS0.*" "$(container-cpus $pod ${pod}c0)"
verify-irq-cpus ".*rtc0.*" "$(container-cpus $pod ${pod}c1)"

# Delete pod and check that the IRQ affinities are restored to all CPUs.
delete-pods $pod

verify-irq-cpus ".*ttyS0.*" $ALLCPUS
verify-irq-cpus ".*rtc0.*" $ALLCPUS

unset ANN0 ANN1

# Create pod with 4 containers, 2 eligible for exclusive CPU allocation
# and 2 others running in shared pools. Annotate the exclusive ones for
# masked IRQs. Check that allocated CPUs get masked from the IRQs.

pod=pod1
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      mask: [ "*ttyS0*" ]
EOF
)
ANN1=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c1: |
      mask: [ "*rtc0*" ]
EOF
)
ANN0=$ANN0 ANN1=$ANN1 \
    CPU=2 CONTCOUNT=4 create guaranteed

verify-irq-cpus ".*ttyS0.*" "$(cpulist-difference $ALLCPUS $(container-cpus $pod ${pod}c0))"
verify-irq-cpus ".*rtc0.*" "$(cpulist-difference $ALLCPUS $(container-cpus $pod ${pod}c1))"

# Delete pod and check that the IRQ affinities are restored to the default (all CPUs).
delete-pods $pod

verify-irq-cpus ".*ttyS0.*" $ALLCPUS
verify-irq-cpus ".*rtc0.*" $ALLCPUS

unset ANN0 ANN1

# Create pod with 4 containers, 2 eligible for exclusive CPU allocation
# and 2 others running in shared pools. Annotate one exclusive ones for
# and IRQ claim and the other for a masked IRQ. Check that IRQ affinity
# gets updated accordingly.

pod=pod2
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: [ "*ttyS0*" ]
      mask: [ "*" ]
EOF
)
ANN1=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c1: |
      mask: [ "*rtc0*" ]
EOF
)
ANN0=$ANN0 ANN1=$ANN1 \
    CPU=2 CONTCOUNT=4 create guaranteed

verify-irq-cpus ".*ttyS0.*" $(container-cpus $pod ${pod}c0)
verify-irq-cpus ".*rtc0.*" "$(cpulist-difference "$(cpulist-difference $ALLCPUS $(container-cpus $pod ${pod}c0))" $(container-cpus $pod ${pod}c1))"

# Delete pod and check that the IRQ affinities are restored to the default (all CPUs).
delete-pods $pod

verify-irq-cpus ".*ttyS0.*" $ALLCPUS
verify-irq-cpus ".*rtc0.*" $ALLCPUS

unset ANN0 ANN1

# Create BestEffort pod, try to annotate container for IRQ affinity. Should fail.

pod=pod3
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: [ "*ttyS0*" ]
EOF
)
ANN0=$ANN0 \
    wait="" CPU=1 CONTCOUNT=1 create besteffort

verify-container-error $pod ${pod}c0 "invalid IRQ affinity, QoS class .* is not Guaranteed"

# Create Burstable pod, try to annotate container for IRQ affinity. Should fail.

pod=pod4
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: [ "*ttyS0*" ]
EOF
)
ANN0=$ANN0 \
    wait="" CPU=1 CONTCOUNT=1 create burstable

verify-container-error $pod ${pod}c0 "invalid IRQ affinity, QoS class .* is not Guaranteed"

unset ANN0

# Create Guaranteed pod without exclusive CPUs, try to annotate container
# for IRQ affinity. Should fail.

pod=pod5
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: [ "*ttyS0*" ]
EOF
)
ANN0=$ANN0 \
    wait="" CPU=250m CONTCOUNT=1 create guaranteed

verify-container-error $pod ${pod}c0 "IRQ affinity .* invalid without exclusive CPUs"

unset ANN0

# Create Guaranteed pod with exclusive CPUs but with an unparsable IRQ affinity
# annotation. Should fail.

pod=pod6
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: xyzzy, foobar
EOF
)
ANN0=$ANN0 \
    wait="" CONTCOUNT=1 create guaranteed

verify-container-error $pod ${pod}c0 "invalid IRQ affinity .*: .*"

unset ANN0

# Create Guaranteed pod with exclusive CPUs but invalid IRQ affinity mode
# in annotation. Should fail.

pod=pod7
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: [ "*ttyS0*" ]
      mask: [ "*rtc0*" ]
      mode: xyzzy
EOF
)
ANN0=$ANN0 \
    wait="" CONTCOUNT=1 create guaranteed

verify-container-error $pod ${pod}c0 "invalid IRQ affinity mode .*xyzzy.* (valid modes: .*)"

cleanup
unset ANN0

#
# Restrict interrupts control and verify that it takes effect.
#

CONTROLLABLE_INTERRUPTS="[\"*rtc0*\"]"

helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS" \
              CONTROLLABLE_INTERRUPTS="$CONTROLLABLE_INTERRUPTS" \
    instantiate helm-config.yaml) helm-launch topology-aware

# Try to exercise control over disallowed ttyS0, too.
pod=pod8
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: [ "*ttyS0*" ]
EOF
)
ANN0=$ANN0 \
    wait="" CONTCOUNT=1 create guaranteed

verify-container-error $pod ${pod}c0 "denied interrupt: .* denied but matched by user pattern .*"

cleanup
unset ANN0

#
# Test IRQ affinity from topology hints.
#

CONTROLLABLE_INTERRUPTS="[\"*ttyS*\"]"

helm_config=$(COLOCATE_PODS=false \
              DEBUG_LOGGERS="$DEBUG_LOGGERS,policy" \
    instantiate helm-config.yaml) helm-launch topology-aware

# Create Guaranteed pod annotated to take IRQ affinity for hinted devices,
# and with a test topology-hint with IRQ for an allowed ttyS0.
pod=pod9
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      devices: [ "*ttyS0*" ]
EOF
)
ANN1=$(cat <<EOF
test.topologyhints.resource-policy.nri.io/container.${pod}c0: |
      test:
        NUMAs: "0"
        IRQs: [ 4 ]
EOF
)

ANN0=$ANN0 ANN1=$ANN1 \
    CONTCOUNT=1 create guaranteed

verify-irq-cpus ".*ttyS0.*" "$(container-cpus $pod ${pod}c0)"

delete-pods $pod
unset ANN0 ANN1


# Create Guaranteed pod annotated to take IRQ affinity for hinted devices,
# and with a test topology-hint with an IRQ for a denied rtc0. Shouldn't
# claim IRQ.

pod=pod10
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      devices: [ "*rtc0*" ]
EOF
)
ANN1=$(cat <<EOF
test.topologyhints.resource-policy.nri.io/container.${pod}c0: |
      test:
        NUMAs: "0"
        IRQs: [ 8 ]
EOF
)

ANN0=$ANN0 ANN1=$ANN1 \
    CONTCOUNT=1 create guaranteed

verify-irq-cpus ".*rtc0.*" $ALLCPUS

delete-pods $pod
unset ANN0 ANN1

# Create Guaranteed pod annotated to take IRQ affinity with an invalid
# match glob. Should fail creating the container.

pod=pod11
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      devices:
        - "*ttyS0*"
        - "[abc"
EOF
)
ANN1=$(cat <<EOF
test.topologyhints.resource-policy.nri.io/container.${pod}c0: |
      test:
        NUMAs: "0"
        IRQs: [ 4 ]
EOF
)

ANN0=$ANN0 ANN1=$ANN1 \
    wait="" CONTCOUNT=1 create guaranteed

verify-container-error $pod ${pod}c0 "invalid IRQ affinity devices pattern"

cleanup
unset ANN0 ANN1
