cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
}

# ctr-cpu-ids <pod> <container>
# Inspect the container of the given pod and report the cpuset it is pinned to.
ctr-cpu-ids() {
    local pod=$1 ctr=$2
    local cpus=""

    $SSH -oConnectTimeout=1 node \
         "kubectl exec $pod -c $ctr -- grep Cpus_allowed_list /proc/1/status" | \
        tr -d '\t ' | cut -d ':' -f 2
    if [ $? -ne 0 ]; then
        error "Failed to get cpuset for container $ctr in pod $pod" >&2
        return 1
    fi
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

# expand-cpulist "0-2,5" prints "0 1 2 5"
expand-cpulist() {
    local cpus="$1"

    if [ "${cpus//-/}" == "$cpus" ] && [ "${cpus//,/}" == "$cpus" ]; then
        echo $cpus
        return 0
    fi

    python3 -c '
import sys
r = set()
for part in sys.argv[1].split(","):
    if not part:
        continue
    if "-" in part:
        a, b = part.split("-")
        r.update(range(int(a), int(b) + 1))
    else:
        r.add(int(part))
print(" ".join(str(x) for x in sorted(r)))
' "$cpus"
}

# ids-difference "1 2 3 4" "2 3" prints "1 4"
ids-difference() {
    local ids1="$1" ids2="$2"

    if [ "${ids1//-/}" != "$ids1" ] || [ "${ids1//,/}" != "$ids1" ]; then
        ids1=$(expand-cpulist $ids1)
    fi
    if [ "${ids2//-/}" != "$ids2" ] || [ "${ids2//,/}" != "$ids2" ]; then
        ids2=$(expand-cpulist $ids2)
    fi

    python3 -c 'import sys
allc = set(int(x) for x in sys.argv[1].split())
iso = set(int(x) for x in sys.argv[2].split())
print(" ".join(str(x) for x in sorted(allc - iso)))
' "$ids1" "$ids2"
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

# wait-pod-waiting-reason <pod> <reason> [<timeout-in-secs>]
# Wait until the pods waiting reason becomes the given one.
wait-pod-waiting-reason() {
    local pod=$1 reason=$2 timeout=${3:-5}
    local cnt=0

    while true; do
        vm-command "kubectl get pod $pod -o json | \
            jq '.status.containerStatuses[].state.waiting.reason'"
        grep -q $reason <<<$COMMAND_OUTPUT && break

        if [ $cnt -lt $timeout ]; then
            let cnt=$cnt+1
            sleep 1
            continue
        fi

        error "Failed to wait for CreateContainerError of $pod"
    done
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

verify-irq-cpus ".*ttyS0.*" "$(ctr-cpu-ids $pod ${pod}c0)"
verify-irq-cpus ".*rtc0.*" "$(ctr-cpu-ids $pod ${pod}c1)"

# Delete pod and check that the IRQ affinities are restored to all CPUs.
vm-command "kubectl delete pod pod0"

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

verify-irq-cpus ".*ttyS0.*" "$(ids-difference $ALLCPUS $(ctr-cpu-ids $pod ${pod}c0))"
verify-irq-cpus ".*rtc0.*" "$(ids-difference $ALLCPUS $(ctr-cpu-ids $pod ${pod}c1))"

# Delete pod and check that the IRQ affinities are restored to the default (all CPUs).
vm-command "kubectl delete pod pod1"

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

verify-irq-cpus ".*ttyS0.*" $(ctr-cpu-ids $pod ${pod}c0)
verify-irq-cpus ".*rtc0.*" "$(ids-difference "$(ids-difference $ALLCPUS $(ctr-cpu-ids $pod ${pod}c0))" $(ctr-cpu-ids $pod ${pod}c1))"

# Delete pod and check that the IRQ affinities are restored to the default (all CPUs).
vm-command "kubectl delete pod pod2"

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

wait-pod-waiting-reason $pod CreateContainerError

ctr=${pod}c0
vm-command "kubectl get pods $pod -ojson | \
    jq '.status.containerStatuses | map(select(.name == \"$ctr\")) | .[0].state'"

grep -q "invalid IRQ affinity, QoS class .* is not Guaranteed" <<< $COMMAND_OUTPUT ||
    error "Missing QoS-based IRQ affinity denial error for $ctr"

# Create Burstable pod, try to annotate container for IRQ affinity. Should fail.

pod=pod4
ANN0=$(cat <<EOF
irq-affinity.resource-policy.nri.io/container.${pod}c0: |
      claim: [ "*ttyS0*" ]
EOF
)
ANN0=$ANN0 \
    wait="" CPU=1 CONTCOUNT=1 create burstable

wait-pod-waiting-reason $pod CreateContainerError

ctr=${pod}c0
vm-command "kubectl get pods $pod -ojson | \
    jq '.status.containerStatuses | map(select(.name == \"$ctr\")) | .[0].state'"

grep -q "invalid IRQ affinity, QoS class .* is not Guaranteed" <<< $COMMAND_OUTPUT ||
    error "Missing QoS-based IRQ affinity denial error for $ctr"

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

wait-pod-waiting-reason $pod CreateContainerError

ctr=${pod}c0
vm-command "kubectl get pods $pod -ojson | \
    jq '.status.containerStatuses | map(select(.name == \"$ctr\")) | .[0].state'"

grep -q "IRQ affinity .* invalid without exclusive CPUs" <<< $COMMAND_OUTPUT ||
    error "Missing shared CPU-based IRQ affinity denial error for $ctr"

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

wait-pod-waiting-reason $pod CreateContainerError

ctr=${pod}c0
vm-command "kubectl get pods $pod -ojson | \
    jq '.status.containerStatuses | map(select(.name == \"$ctr\")) | .[0].state'"

grep -q "invalid IRQ affinity .*: .*" <<< $COMMAND_OUTPUT ||
    error "Missing unparsable IRQ affinity denial error for $ctr"

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

wait-pod-waiting-reason $pod CreateContainerError

ctr=${pod}c0
vm-command "kubectl get pods $pod -ojson | \
    jq '.status.containerStatuses | map(select(.name == \"$ctr\")) | .[0].state'"

grep -q "invalid IRQ affinity mode .*xyzzy.* (valid modes: .*)" <<< $COMMAND_OUTPUT ||
    error "Missing invalid mode based IRQ affinity denial error for $ctr"

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

wait-pod-waiting-reason $pod CreateContainerError

ctr=${pod}c0
vm-command "kubectl get pods $pod -ojson | \
    jq '.status.containerStatuses | map(select(.name == \"$ctr\")) | .[0].state'"

grep -q "denied interrupt: .* denied but matched by user pattern .*" <<< $COMMAND_OUTPUT ||
    error "Missing IRQ affinity denial error for $ctr"

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

verify-irq-cpus ".*ttyS0.*" "$(ctr-cpu-ids $pod ${pod}c0)"

vm-command "kubectl delete pod $pod"
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

vm-command "kubectl delete pod $pod"
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

wait-pod-waiting-reason $pod CreateContainerError

ctr=${pod}c0
vm-command "kubectl get pods $pod -ojson | \
    jq '.status.containerStatuses | map(select(.name == \"$ctr\")) | .[0].state'"

grep -q "invalid IRQ affinity devices pattern" <<< $COMMAND_OUTPUT ||
    error "Missing invalid affinity devices pattern error for $ctr"


verify-irq-cpus ".*rtc0.*" $ALLCPUS


cleanup
unset ANN0 ANN1
